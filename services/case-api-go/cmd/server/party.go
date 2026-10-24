// Author: Patrick Munis at newwave
package main

// party.go — authenticated party workspace.
//
// External parties previously interacted only through one-shot share links.
// This adds persistent Keycloak accounts (PARTY realm role) provisioned by
// case staff per case, plus the workspace endpoints those accounts use.
// The party_accounts mapping is the ONLY source of party access — a PARTY
// token without a mapping sees nothing.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// inviteParty: POST /cases/{caseId}/party-invites — case staff provision a
// Keycloak account for an external party representative and bind it to this
// case + side. Re-inviting the same email for the same case is idempotent.
func (s *server) inviteParty(w http.ResponseWriter, r *http.Request) {
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "party_workspace") {
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires case staff"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Email   string `json:"email"`
		Name    string `json:"name"`
		PartyID string `json:"party_id"` // INITIATING | RESPONDING
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil ||
		!strings.Contains(in.Email, "@") || !validOfferParty(in.PartyID) {
		http.Error(w, `{"error":"email and party_id (INITIATING|RESPONDING) required"}`, http.StatusBadRequest)
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if len(in.Email) > 254 || len(in.Name) > 200 {
		http.Error(w, `{"error":"field too long"}`, http.StatusBadRequest)
		return
	}
	var caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		http.Error(w, `{"error":"identity provider unavailable"}`, http.StatusBadGateway)
		return
	}
	username := "party-" + in.Email
	userID, _ := s.kcFindUserByUsername(r.Context(), tok, username)
	tempPassword := ""
	if userID == "" {
		res := s.kcCreateUser(r.Context(), tok, username, in.Email, "PARTY", []string{"tenant-" + tenant})
		if e, ok := res["error"]; ok {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, "identity provisioning: "+fmt.Sprint(e)), http.StatusBadGateway)
			return
		}
		tempPassword, _ = res["temporary_password"].(string)
		userID, _ = s.kcFindUserByUsername(r.Context(), tok, username)
		if userID == "" {
			http.Error(w, `{"error":"identity created but could not be resolved"}`, http.StatusBadGateway)
			return
		}
	} else {
		// Existing account: ensure the PARTY role is present (idempotent).
		_ = s.kcAssignRealmRole(r.Context(), tok, userID, "PARTY")
	}
	_, err = s.db.Exec(r.Context(), `
		INSERT INTO public.party_accounts (tenant, case_id, party_id, kc_subject, email, invited_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant, case_id, kc_subject) DO UPDATE SET party_id=EXCLUDED.party_id`,
		tenant, caseID, in.PartyID, userID, in.Email, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"persist failed"}`, http.StatusInternalServerError)
		return
	}
	// Welcome mail with the portal link; the temporary password is returned in
	// the API response for out-of-band delivery (never emailed, never logged).
	mailBody := fmt.Sprintf(
		"%s,\n\nYou have been given access to IDR case %s as the %s party.\nSign in at %s with username %s.\n\n"+
			"Federal IDR process: 45 CFR Part 149. Your offer, once submitted, is cryptographically sealed until both parties have submitted or the window closes.\n",
		in.Name, caseNumber, in.PartyID, s.cfg.PortalBaseURL, username)
	_ = s.sendMail([]string{in.Email}, nil, "IDR case access: "+caseNumber, mailBody)
	s.logActivity(r.Context(), tenant, caseID, "PARTY_INVITED",
		fmt.Sprintf("Party account %s invited as %s by %s", in.Email, in.PartyID, p.Subject))
	s.logAudit(r.Context(), tenant, caseID, "PARTY_INVITED", map[string]any{
		"by": p.Subject, "email": in.Email, "party_id": in.PartyID})
	out := map[string]any{"username": username, "party_id": in.PartyID, "case_id": caseID}
	if tempPassword != "" {
		out["temporary_password"] = tempPassword // one-time, share out-of-band
	}
	writeJSON(w, http.StatusCreated, out)
}

// partyCases: GET /party/cases — every case the authenticated party is mapped to.
func (s *server) partyCases(w http.ResponseWriter, r *http.Request) {
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "party_workspace") {
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "PARTY") {
		http.Error(w, `{"error":"forbidden: party accounts only"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT c.id, c.case_number, c.status, coalesce(c.status_reason,''),
		       coalesce(c.service_line,''), c.offer_window_ends_at, a.party_id, a.created_at AS granted_at
		FROM public.party_accounts a
		JOIN tenant_%s.cases c ON c.id::text = a.case_id
		WHERE a.tenant=$1 AND a.kc_subject=$2
		ORDER BY c.opened_at DESC LIMIT 200`, sanitizeTenant(tenant)), tenant, p.Subject)
	if err != nil {
		slog.Error("query failed", "handler", "party.go", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": rows})
}

// partyCaseDetail: GET /party/cases/{caseId} — status, statutory clocks,
// determination outcome (post-issue), and this party's invoice obligations.
// Offer amounts are never exposed here — listOffers handles that lawfully.
func (s *server) partyCaseDetail(w http.ResponseWriter, r *http.Request) {
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "party_workspace") {
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "PARTY") {
		http.Error(w, `{"error":"forbidden: party accounts only"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !s.partyOwnsCase(r, tenant, caseID, p.Subject, "") {
		http.Error(w, `{"error":"no party assignment for this case"}`, http.StatusForbidden)
		return
	}
	var partyID string
	_ = s.db.QueryRow(r.Context(), `
		SELECT party_id FROM public.party_accounts
		WHERE tenant=$1 AND case_id=$2 AND kc_subject=$3`, tenant, caseID, p.Subject).Scan(&partyID)
	detailRows, _ := s.queryRows(r, fmt.Sprintf(`
		SELECT c.case_number, c.status, coalesce(c.status_reason,''), coalesce(c.service_line,''),
		       c.offer_window_ends_at, c.opened_at
		FROM tenant_%s.cases c WHERE c.id=$1`, sanitizeTenant(tenant)), caseID)
	var detail any
	if len(detailRows) > 0 {
		detail = detailRows[0]
	}
	detRows, _ := s.queryRows(r, `
		SELECT amount_cents, rationale, issued_at FROM public.determinations
		WHERE tenant=$1 AND case_id=$2`, tenant, caseID)
	var det any
	if len(detRows) > 0 {
		det = detRows[0]
	}
	invoices, _ := s.queryRows(r, `
		SELECT id, invoice_no, party, kind, amount_cents, status, due_date, paid_at
		FROM public.invoices WHERE tenant=$1 AND case_id=$2 ORDER BY created_at`, tenant, caseID)
	writeJSON(w, http.StatusOK, map[string]any{
		"case": detail, "your_party": partyID,
		"determination": det, "invoices": invoices,
	})
}

// listPartyAccounts: GET /cases/{caseId}/party-accounts — staff visibility
// over who holds party access to a case.
func (s *server) listPartyAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "party_workspace") {
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `
		SELECT id, party_id, email, invited_by, created_at
		FROM public.party_accounts WHERE tenant=$1 AND case_id=$2 ORDER BY created_at`,
		tenant, chi.URLParam(r, "caseId"))
	if err != nil {
		slog.Error("query failed", "handler", "party.go", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"party_accounts": rows})
}
