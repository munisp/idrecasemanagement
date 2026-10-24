// Author: Patrick Munis at newwave
// tpa_edit.go — a TPA correcting a dispute it filed, before the fee is paid.
//
// A case that is awaiting payment is frozen (caselock.go): staff may only
// write to the party. This is the one deliberate exception. Until the fee is
// paid the dispute has not opened, nobody has reviewed anything, and the TPA
// is the only one who knows it picked the wrong client or mistyped the
// amount. Once the fee settles the case leaves AWAITING_PAYMENT and this
// endpoint refuses: from then on a correction is a staff matter.
//
// The route is /tpa/disputes/{caseId}, not /cases/{caseId}/..., so the case
// lock guard does not see it; the status check below is what bounds it.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// maxDisputedAmountCents bounds a typed amount: $1 billion is far above any
// real claim and far below where int64 cents arithmetic gets interesting.
const maxDisputedAmountCents = 100_000_000_000

type tpaDisputeEdit struct {
	ClientID            *string `json:"client_id"`
	ContactName         *string `json:"contact_name"`
	DisputedAmountCents *int64  `json:"disputed_amount_cents"`
	Notes               *string `json:"notes"`
}

// validate trims the text fields and reports the first problem, or "".
func (e *tpaDisputeEdit) validate() string {
	if e.ClientID == nil && e.ContactName == nil && e.DisputedAmountCents == nil && e.Notes == nil {
		return "nothing to change"
	}
	if e.ClientID != nil {
		v := strings.TrimSpace(*e.ClientID)
		if v == "" {
			return "client_id cannot be empty"
		}
		e.ClientID = &v
	}
	if e.ContactName != nil {
		v := strings.TrimSpace(*e.ContactName)
		if len(v) > 200 {
			return "contact_name is too long (200 characters at most)"
		}
		e.ContactName = &v
	}
	if e.Notes != nil {
		v := strings.TrimSpace(*e.Notes)
		if len(v) > 4000 {
			return "notes are too long (4000 characters at most)"
		}
		e.Notes = &v
	}
	if a := e.DisputedAmountCents; a != nil && (*a < 0 || *a > maxDisputedAmountCents) {
		return "disputed_amount_cents is out of range"
	}
	return ""
}

// tpaDisputeEditRefusal says why a TPA may not edit a dispute, or returns 0.
// A dispute filed by another organization answers 404, not 403: a TPA must
// not be able to probe which case ids exist.
func tpaDisputeEditRefusal(callerTPA, originTPA, status string) (int, string) {
	if originTPA == "" || originTPA != callerTPA {
		return http.StatusNotFound, "dispute not found"
	}
	if status != "AWAITING_PAYMENT" {
		return http.StatusConflict, "this dispute is " + status + ": it can only be edited while it is awaiting payment"
	}
	return 0, ""
}

// tpaEditDispute: PATCH /tpa/disputes/{caseId}. Only the fields sent change.
func (s *server) tpaEditDispute(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	var in tpaDisputeEdit
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if msg := in.validate(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	caseID := r.PathValue("caseId")
	schema := sanitizeTenant(tenant)

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	// Lock the case row: the payment webhook moves it out of AWAITING_PAYMENT,
	// and an edit must not land after that.
	var status, caseNumber string
	var oldAmount int64
	var oldContact, oldNotes string
	err = tx.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT status, case_number, coalesce(disputed_amount_cents,0),
		       coalesce(details->>'initiating_party_contact',''), coalesce(details->>'filing_notes','')
		FROM tenant_%s.cases WHERE id::text=$1 FOR UPDATE`, schema), caseID).
		Scan(&status, &caseNumber, &oldAmount, &oldContact, &oldNotes)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, `{"error":"dispute not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var originTPA, oldClientID, oldParty, oldPartyType string
	_ = tx.QueryRow(r.Context(), `
		SELECT tpa_id::text, client_id::text, initiating_party_name, initiating_party_type
		FROM public.case_origin WHERE tenant=$1 AND case_id=$2`, tenant, caseID).
		Scan(&originTPA, &oldClientID, &oldParty, &oldPartyType)
	if code, msg := tpaDisputeEditRefusal(org.ID, originTPA, status); code != 0 {
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}

	changed := map[string]any{}
	party, partyType := oldParty, oldPartyType
	if in.ClientID != nil && *in.ClientID != oldClientID {
		var clientStatus string
		err := tx.QueryRow(r.Context(), `
			SELECT party_name, party_type, status FROM public.tpa_clients
			WHERE tenant=$1 AND tpa_id=$2 AND id::text=$3`, tenant, org.ID, *in.ClientID).
			Scan(&party, &partyType, &clientStatus)
		if err != nil {
			http.Error(w, `{"error":"client not found"}`, http.StatusNotFound)
			return
		}
		if clientStatus != "ACTIVE" {
			http.Error(w, `{"error":"client relationship is SUSPENDED"}`, http.StatusConflict)
			return
		}
		if _, err := tx.Exec(r.Context(), `
			UPDATE public.case_origin SET client_id=$3, initiating_party_name=$4, initiating_party_type=$5
			WHERE tenant=$1 AND case_id=$2`, tenant, caseID, *in.ClientID, party, partyType); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		if _, err := tx.Exec(r.Context(), `
			UPDATE public.payments SET payer_org=$3 WHERE tenant=$1 AND case_id=$2`,
			tenant, caseID, org.Name+" (on behalf of "+party+")"); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		changed["initiating_party"] = map[string]string{"from": oldParty, "to": party}
	}
	// Everything the case itself shows about the filer, kept in step with
	// what tpaIntake writes.
	details := map[string]string{
		"initiating_party": party, "filing_party_type": partyType,
		"requester_org": org.Name + " on behalf of " + party,
	}
	if in.ContactName != nil && *in.ContactName != oldContact {
		details["initiating_party_contact"] = *in.ContactName
		changed["contact_name"] = map[string]string{"from": oldContact, "to": *in.ContactName}
	}
	if in.Notes != nil && *in.Notes != oldNotes {
		details["filing_notes"] = *in.Notes
		changed["notes"] = true
	}
	newAmount := oldAmount
	if in.DisputedAmountCents != nil && *in.DisputedAmountCents != oldAmount {
		newAmount = *in.DisputedAmountCents
		changed["disputed_amount_cents"] = map[string]int64{"from": oldAmount, "to": newAmount}
	}
	if len(changed) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"case_id": caseID, "case_number": caseNumber, "changed": []string{}})
		return
	}
	detailsJSON, _ := json.Marshal(details)
	if _, err := tx.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET details = details || $2::jsonb,
		       disputed_amount_cents = nullif($3::bigint,0), updated_at = now()
		WHERE id::text=$1`, schema), caseID, detailsJSON, newAmount); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}

	fields := make([]string, 0, len(changed))
	for k := range changed {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	s.logAudit(r.Context(), tenant, caseID, "TPA_DISPUTE_EDITED", map[string]any{
		"tpa_id": org.ID, "by": p.Subject, "case_number": caseNumber, "changes": changed})
	s.logActivity(r.Context(), tenant, caseID, "TPA_EDIT",
		fmt.Sprintf("%s corrected the filing before payment (%s)", org.Name, strings.Join(fields, ", ")))
	// A corrected amount can cross the program's escalation trigger. Intake
	// only checked the original figure, so check the new one -- once.
	if newAmount > oldAmount {
		var already bool
		_ = s.db.QueryRow(r.Context(), `
			SELECT EXISTS(SELECT 1 FROM public.escalations
			WHERE tenant=$1 AND case_id=$2 AND clock='PROGRAM_TRIGGER')`, tenant, caseID).Scan(&already)
		if !already {
			s.escalationTrigger(r, tenant, caseID, newAmount)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"case_id": caseID, "case_number": caseNumber, "status": status, "changed": fields,
		"initiating_party": party, "initiating_party_type": partyType, "disputed_amount_cents": newAmount,
	})
}
