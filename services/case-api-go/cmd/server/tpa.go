package main

// tpa.go — third-party administrators (TPAs).
//
// A TPA is an organization that files, pays, and tracks disputes ON BEHALF OF
// one or more initiating parties (providers or health plans — a client list
// the TPA manages itself). Onboarding is self-serve: registering the
// organization also creates its first Keycloak account (TPA role, same
// temp-password mechanism staff accounts get) and links it to the org in
// the same call — no staff approval gate, no separate manual step (states
// retain SUSPEND as the after-the-fact control). A claim code is still
// generated and kept live: if that first account is ever lost, a FEDERAL_
// ADMIN/PLATFORM_ADMIN can create a replacement account the normal staff
// way and have it claim the org with the code instead of starting over.
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

// tpaUsernameFromEmail derives a Keycloak-safe username from the local part
// of an email (lowercase, [a-z0-9.-_] only) -- registrants give us an email,
// not a chosen username, and the account still needs one.
func tpaUsernameFromEmail(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i >= 0 {
		local = email[:i]
	}
	var b strings.Builder
	for _, c := range strings.ToLower(local) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			b.WriteByte(byte(c))
		}
	}
	if b.Len() == 0 {
		return "tpa"
	}
	return b.String()
}

// registerTPA: PUBLIC self-serve onboarding. Creates the org AND its first
// Keycloak login (TPA role, temp password) in one call, already linked --
// returns the credentials directly, the same way staff account creation
// does, since a brand-new registrant has no other way to get one.
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
	tenant := strings.ToLower(in.Tenant)

	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak admin auth failed: " + err.Error()})
		return
	}
	groupPath := "/tenant/" + tenant
	if _, err := s.kcEnsureGroup(r.Context(), tok, tenant); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak group ensure failed: " + err.Error()})
		return
	}
	base := tpaUsernameFromEmail(in.ContactEmail)
	username := base
	var result map[string]any
	for attempt := 0; attempt < 5; attempt++ {
		result = s.kcCreateUser(r.Context(), tok, username, in.ContactEmail, "TPA", []string{groupPath})
		if _, failed := result["error"]; !failed {
			break
		}
		errMsg, _ := result["error"].(string)
		if !strings.Contains(errMsg, "already exists") {
			writeJSON(w, http.StatusBadGateway, result)
			return
		}
		// Username collision (contact_email reused, or base is very common,
		// e.g. "info"/"admin") -- try a short random suffix instead of
		// failing registration over something the registrant never typed.
		suffix := make([]byte, 2)
		_, _ = rand.Read(suffix)
		username = base + hex.EncodeToString(suffix)
	}
	if _, failed := result["error"]; failed {
		writeJSON(w, http.StatusConflict, result)
		return
	}
	userID, _ := result["user_id"].(string)
	if userID == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "TPA account created but its Keycloak id was unavailable -- cannot link it to an organization"})
		return
	}

	code := "TPA-" + strings.ToUpper(hex.EncodeToString(func() []byte { b := make([]byte, 3); _, _ = rand.Read(b); return b }()))
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpas (tenant, name, contact_name, contact_email, claim_code, status)
		VALUES ($1, $2, $3, $4, $5, 'ACTIVE') RETURNING id`,
		tenant, in.Name, in.ContactName, in.ContactEmail, code).Scan(&id); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "registration failed — an organization with this email may already exist for this program"})
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.tpa_users (tenant, user_sub, tpa_id) VALUES ($1,$2,$3)`,
		tenant, userID, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "organization and login were created, but linking them failed — sign in and use claim code " + code + " (Profile → Claim TPA) to finish linking",
		})
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_REGISTERED", map[string]any{
		"tpa_id": id, "name": in.Name, "username": username, "user_id": userID,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"tpa_id": id, "status": "ACTIVE",
		"username": username, "temporary_password": result["temporary_password"],
		"next": "Sign in to the portal with the username and password above (you'll be asked to set a new password) — your organization is already linked. Then add your initiating parties and file on their behalf.",
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
		org.ContactEmail, org.ContactName, org.Name+" on behalf of "+partyName, partyType, in.DisputedAmountCents, "")
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
