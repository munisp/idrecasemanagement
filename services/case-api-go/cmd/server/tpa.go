package main

// tpa.go — third-party administrators (TPAs).
//
// A TPA is an organization that files, pays, and tracks disputes ON BEHALF OF
// one or more initiating parties (providers or health plans — a client list
// the TPA manages itself). Onboarding is self-serve: the TPA registers its
// organization publicly and receives a one-time claim code; its first portal
// user claims the org with that code (no staff approval gate — states retain
// SUSPEND as the after-the-fact control).
//
// Every TPA-filed dispute gets a case_origin row: who the case belongs to
// (initiating party = the client) vs who filed/paid (the TPA). The intake
// itself reuses the standard gated flow (startAhcaCase) with the TPA as the
// contact — the payment link and upload link go to the TPA, which is exactly
// the "pay on behalf" requirement — and the payment row is attributed
// "TPA on behalf of <initiating party>" (payer_org), so collections roll up
// correctly per dispute AND per TPA.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type tpaOrg struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContactName  string `json:"contact_name"`
	ContactEmail string `json:"contact_email"`
	Status       string `json:"status"`
}

// tpaForPrincipal resolves the caller's TPA org (linked via tpa_users).
func (s *server) tpaForPrincipal(r *http.Request, tenant string) (*tpaOrg, error) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var o tpaOrg
	err := s.db.QueryRow(r.Context(), `
		SELECT t.id, t.name, t.contact_name, t.contact_email, t.status
		FROM public.tpa_users u JOIN public.tpas t ON t.id = u.tpa_id
		WHERE u.tenant=$1 AND u.user_sub=$2`, tenant, p.Subject).
		Scan(&o.ID, &o.Name, &o.ContactName, &o.ContactEmail, &o.Status)
	if err != nil {
		return nil, fmt.Errorf("no TPA organization linked to your account — claim one with your claim code")
	}
	if o.Status != "ACTIVE" {
		return nil, fmt.Errorf("TPA organization is %s", o.Status)
	}
	return &o, nil
}

// registerTPA: PUBLIC self-serve onboarding. Returns the org and a one-time
// claim code (shown once — the first portal user links the org with it).
func (s *server) registerTPA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tenant       string `json:"tenant"`
		Name         string `json:"name"`
		ContactName  string `json:"contact_name"`
		ContactEmail string `json:"contact_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		in.Tenant == "" || in.Name == "" || in.ContactEmail == "" || !strings.Contains(in.ContactEmail, "@") {
		http.Error(w, `{"error":"tenant, name, and a valid contact_email are required"}`, http.StatusBadRequest)
		return
	}
	code := "TPA-" + strings.ToUpper(hex.EncodeToString(func() []byte { b := make([]byte, 3); _, _ = rand.Read(b); return b }()))
	var id string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpas (tenant, name, contact_name, contact_email, claim_code, status)
		VALUES (lower($1), $2, $3, $4, $5, 'ACTIVE') RETURNING id`,
		in.Tenant, in.Name, in.ContactName, in.ContactEmail, code).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"registration failed — an organization with this email may already exist for this program"}`, http.StatusConflict)
		return
	}
	s.logAudit(r.Context(), strings.ToLower(in.Tenant), "", "TPA_REGISTERED", map[string]any{"tpa_id": id, "name": in.Name})
	writeJSON(w, http.StatusCreated, map[string]any{
		"tpa_id": id, "status": "ACTIVE", "claim_code": code,
		"next": "Sign in to the portal with a TPA account and enter this claim code (Profile → Claim TPA) to link your organization. Then add your initiating parties and file on their behalf.",
	})
}

// claimTPA links the authenticated TPA user to their org via the claim code.
func (s *server) claimTPA(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Code == "" {
		http.Error(w, `{"error":"claim code required"}`, http.StatusBadRequest)
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(), `
		SELECT id FROM public.tpas WHERE tenant=$1 AND claim_code=$2 AND status='ACTIVE'`,
		tenant, strings.ToUpper(strings.TrimSpace(in.Code))).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"invalid claim code"}`, http.StatusNotFound)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.tpa_users (tenant, user_sub, tpa_id) VALUES ($1,$2,$3)
		ON CONFLICT (tenant, user_sub) DO UPDATE SET tpa_id=$3`, tenant, p.Subject, id)
	// Burn the code: it has done its one job.
	_, _ = s.db.Exec(r.Context(), `UPDATE public.tpas SET claim_code=NULL, updated_at=now() WHERE id=$1`, id)
	s.logAudit(r.Context(), tenant, "", "TPA_CLAIMED", map[string]any{"tpa_id": id, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"tpa_id": id, "status": "linked"})
}

// tpaMe: the caller's org + clients + filing stats.
func (s *server) tpaMe(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tpa": org})
}

// tpaAddClient registers an initiating party the TPA acts for.
func (s *server) tpaAddClient(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		PartyName    string `json:"party_name"`
		PartyType    string `json:"party_type"` // PROVIDER | HEALTH_PLAN
		ContactEmail string `json:"contact_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.PartyName == "" {
		http.Error(w, `{"error":"party_name required"}`, http.StatusBadRequest)
		return
	}
	pt := strings.ToUpper(strings.TrimSpace(in.PartyType))
	if pt == "" {
		pt = "PROVIDER"
	}
	if pt != "PROVIDER" && pt != "HEALTH_PLAN" {
		http.Error(w, `{"error":"party_type must be PROVIDER or HEALTH_PLAN"}`, http.StatusBadRequest)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpa_clients (tenant, tpa_id, party_name, party_type, contact_email)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`, tenant, org.ID, in.PartyName, pt, in.ContactEmail).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"client already exists for this TPA"}`, http.StatusConflict)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_CLIENT_ADDED", map[string]any{"tpa_id": org.ID, "client_id": id, "party": in.PartyName, "by": p.Subject})
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": id, "party_type": pt, "status": "ACTIVE"})
}

func (s *server) tpaListClients(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, _ := s.queryRows(r, `
		SELECT c.id, c.party_name, c.party_type, c.contact_email, c.status, c.created_at,
		       (SELECT count(*) FROM public.case_origin o WHERE o.client_id=c.id) AS case_count
		FROM public.tpa_clients c WHERE c.tenant=$1 AND c.tpa_id=$2 ORDER BY c.party_name`, tenant, org.ID)
	writeJSON(w, http.StatusOK, map[string]any{"clients": rows})
}

// tpaSetClientStatus suspends/reactivates a client relationship.
func (s *server) tpaSetClientStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		Status string `json:"status"` // ACTIVE | SUSPENDED
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Status != "ACTIVE" && in.Status != "SUSPENDED" {
		http.Error(w, `{"error":"status must be ACTIVE|SUSPENDED"}`, http.StatusBadRequest)
		return
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.tpa_clients SET status=$3 WHERE tenant=$1 AND tpa_id=$2 AND id=$4`,
		tenant, org.ID, in.Status, r.PathValue("clientId"))
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": in.Status})
}

// tpaIntake files a dispute on behalf of an initiating party. Uses the
// standard gated intake (payment link + upload link go to the TPA contact);
// the case_origin row ties the dispute to both the TPA and the client.
func (s *server) tpaIntake(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		ClientID            string `json:"client_id"`
		ContactName         string `json:"contact_name"` // client's contact for the dispute
		DisputedAmountCents int64  `json:"disputed_amount_cents"`
		Notes               string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ClientID == "" {
		http.Error(w, `{"error":"client_id required"}`, http.StatusBadRequest)
		return
	}
	var partyName, partyType, clientEmail, clientStatus string
	err = s.db.QueryRow(r.Context(), `
		SELECT party_name, party_type, coalesce(contact_email,''), status FROM public.tpa_clients
		WHERE tenant=$1 AND tpa_id=$2 AND id=$3`, tenant, org.ID, in.ClientID).
		Scan(&partyName, &partyType, &clientEmail, &clientStatus)
	if err != nil {
		http.Error(w, `{"error":"client not found"}`, http.StatusNotFound)
		return
	}
	if clientStatus != "ACTIVE" {
		http.Error(w, `{"error":"client relationship is SUSPENDED"}`, http.StatusConflict)
		return
	}
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"TPA filing is available on programmed tenants"}`, http.StatusBadRequest)
		return
	}
	// The TPA is the operational contact: payment link, case number, and
	// upload link all flow to them — they pay and manage documents on behalf.
	caseID, caseNumber, err := s.startAhcaCase(r, tenant, cfg,
		org.ContactEmail, org.ContactName, org.Name+" on behalf of "+partyName, partyType, in.DisputedAmountCents)
	if err != nil {
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"case_number already exists — retry"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"case creation failed"}`, http.StatusBadGateway)
		return
	}
	// Origin record: the dispute belongs to the initiating party; the TPA filed it.
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.case_origin
		  (tenant, case_id, tpa_id, client_id, initiating_party_name, initiating_party_type, filed_by_email)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		tenant, caseID, org.ID, in.ClientID, partyName, partyType, org.ContactEmail)
	// Attribute the intake-fee payment row: paid by the TPA on behalf of the party.
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.payments SET payer_org=$3 WHERE tenant=$1 AND case_id=$2`,
		tenant, caseID, org.Name+" (on behalf of "+partyName+")")
	// Initiating party visible on the case itself.
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET details = details || $2::jsonb WHERE id=$1`, sanitizeTenant(tenant)),
		caseID, fmt.Sprintf(`{"initiating_party":%q,"filed_by_tpa":%q}`, partyName, org.Name))
	var st string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&st)
	s.logAudit(r.Context(), tenant, caseID, "TPA_INTAKE", map[string]any{
		"tpa_id": org.ID, "client_id": in.ClientID, "initiating_party": partyName, "case_number": caseNumber})
	writeJSON(w, http.StatusCreated, map[string]any{
		"case_id": caseID, "case_number": caseNumber, "status": st,
		"payment_required": st == "AWAITING_PAYMENT",
		"initiating_party": partyName, "filed_by": org.Name,
		"note": "payment link and case instructions go to the TPA contact — you pay and upload on behalf of " + partyName,
	})
}

// tpaDashboard: every initiating party with its cases, statuses, and money.
func (s *server) tpaDashboard(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	orgID := ""
	if hasAnyRole(p, "TPA") && !hasAnyRole(p, "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		org, err := s.tpaForPrincipal(r, tenant)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
			return
		}
		orgID = org.ID
	} else if q := r.URL.Query().Get("tpa_id"); q != "" {
		orgID = q // staff inspecting a specific TPA
	}
	if orgID == "" {
		http.Error(w, `{"error":"tpa_id required for staff view"}`, http.StatusBadRequest)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT o.case_id, o.initiating_party_name, o.initiating_party_type, o.filed_by_email,
		       o.created_at::date::text AS filed,
		       c.case_number, c.status, c.disputed_amount_cents,
		       cl.party_name AS client_name, cl.id AS client_id,
		       (SELECT coalesce(sum(i.amount_cents),0) FROM public.invoices i
		         WHERE i.tenant=o.tenant AND i.case_id=o.case_id) AS invoiced_cents,
		       (SELECT coalesce(sum(py.amount_cents),0) FROM public.payments py
		         WHERE py.tenant=o.tenant AND py.case_id=o.case_id AND py.status='PAID') AS paid_cents
		FROM public.case_origin o
		JOIN public.tpa_clients cl ON cl.id = o.client_id
		JOIN tenant_%s.cases c ON c.id = o.case_id
		WHERE o.tenant=$1 AND o.tpa_id=$2
		ORDER BY o.created_at DESC LIMIT 500`, sanitizeTenant(tenant)), tenant, orgID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// Roll up per initiating party.
	type rollup struct {
		ClientID   string `json:"client_id"`
		Party      string `json:"initiating_party"`
		PartyType  string `json:"party_type"`
		Cases      int    `json:"cases"`
		Open       int    `json:"open"`
		Invoiced   int64  `json:"invoiced_cents"`
		Paid       int64  `json:"paid_cents"`
	}
	byClient := map[string]*rollup{}
	order := []string{}
	for _, row := range rows {
		cid := fmt.Sprint(row["client_id"])
		rl, ok := byClient[cid]
		if !ok {
			rl = &rollup{ClientID: cid, Party: fmt.Sprint(row["initiating_party_name"]),
				PartyType: fmt.Sprint(row["initiating_party_type"])}
			byClient[cid] = rl
			order = append(order, cid)
		}
		rl.Cases++
		st := fmt.Sprint(row["status"])
		if st != "CLOSED" && st != "INELIGIBLE" && st != "WITHDRAWN" {
			rl.Open++
		}
		rl.Invoiced += toInt64(row["invoiced_cents"])
		rl.Paid += toInt64(row["paid_cents"])
	}
	rollups := []rollup{}
	for _, cid := range order {
		rollups = append(rollups, *byClient[cid])
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": rows, "by_client": rollups})
}

// adminListTPAs: staff view of all TPA orgs in the tenant.
func (s *server) adminListTPAs(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, _ := s.queryRows(r, `
		SELECT t.id, t.name, t.contact_name, t.contact_email, t.status, t.created_at,
		       (SELECT count(*) FROM public.tpa_clients c WHERE c.tpa_id=t.id) AS clients,
		       (SELECT count(*) FROM public.case_origin o WHERE o.tpa_id=t.id) AS cases_filed
		FROM public.tpas t WHERE t.tenant=$1 ORDER BY t.created_at DESC`, tenant)
	writeJSON(w, http.StatusOK, map[string]any{"tpas": rows})
}

// adminSetTPAStatus: the state's after-the-fact control (suspend bad actors).
func (s *server) adminSetTPAStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Status string `json:"status"` // ACTIVE | SUSPENDED
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Status != "ACTIVE" && in.Status != "SUSPENDED" {
		http.Error(w, `{"error":"status must be ACTIVE|SUSPENDED"}`, http.StatusBadRequest)
		return
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.tpas SET status=$3, updated_at=now() WHERE tenant=$1 AND id=$2`,
		tenant, r.PathValue("tpaId"), in.Status)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_STATUS", map[string]any{"tpa_id": r.PathValue("tpaId"), "status": in.Status, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"status": in.Status})
}
