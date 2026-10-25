// Author: Patrick Munis at newwave
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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
)

type tpaOrg struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContactName  string `json:"contact_name"`
	ContactEmail string `json:"contact_email"`
	Status       string `json:"status"`
	// InviteCode: this org's current claim code, if one is pending -- null
	// once it's been used. Shown to an already-linked user on their own
	// #/tpa dashboard so they can add a second staff member at their firm
	// themselves (generate a fresh one via tpaInviteCode if none is live).
	InviteCode *string `json:"invite_code"`
}

// tpaForPrincipal resolves the caller's TPA org (linked via tpa_users).
func (s *server) tpaForPrincipal(r *http.Request, tenant string) (*tpaOrg, error) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var o tpaOrg
	err := s.db.QueryRow(r.Context(), `
		SELECT t.id, t.name, t.contact_name, t.contact_email, t.status, t.claim_code
		FROM public.tpa_users u JOIN public.tpas t ON t.id = u.tpa_id
		WHERE u.tenant=$1 AND u.user_sub=$2`, tenant, p.Subject).
		Scan(&o.ID, &o.Name, &o.ContactName, &o.ContactEmail, &o.Status, &o.InviteCode)
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
	// The login is emailed, so the address has to be one mail can reach.
	// Checked here so the visitor gets a plain message instead of an opaque
	// upstream error from account creation.
	in.ContactEmail = strings.TrimSpace(in.ContactEmail)
	if !validMailbox(in.ContactEmail) {
		http.Error(w, `{"error":"enter a valid contact email address — your sign-in details will be sent there"}`, http.StatusBadRequest)
		return
	}
	if msg := emailReachMsg(r.Context(), "the contact email", in.ContactEmail); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg+" — your sign-in details will be sent there")+`"}`, http.StatusBadRequest)
		return
	}

	// Public and it sends email: same per-address throttle as the public
	// dispute form, so it cannot be used to flood a mailbox or mint accounts.
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	if !s.rateLimit("tpa-register:"+ip, 10, 3600) {
		http.Error(w, `{"error":"too many registrations from this address — try again later"}`, http.StatusTooManyRequests)
		return
	}

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
		INSERT INTO public.tpa_users (tenant, user_sub, tpa_id, team_role, full_name, email, username, added_by)
		VALUES ($1,$2,$3,'ADMIN',nullif($4,''),$5,$6,'registration')`,
		tenant, userID, id, in.ContactName, strings.ToLower(in.ContactEmail), username); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "organization and login were created, but linking them failed — sign in and use claim code " + code + " (Profile → Claim TPA) to finish linking",
		})
		return
	}
	// The login goes to the registered mailbox only -- never back to the
	// browser. Showing it on the page handed a working account to whoever
	// typed an email address, whether or not they owned it; emailing it also
	// proves the address is real.
	tempPassword, _ := result["temporary_password"].(string)
	subject, body := tpaWelcomeMail(in.Name, in.ContactName, username, tempPassword, s.cfg.PortalBaseURL)
	// The mail log records that the email went out, not what it said: the
	// sign-in details belong to the recipient only.
	logBody := tpaWelcomeLogNote
	// A queued email (the provider's limit was reached) still arrives: the registration stands.
	if merr := s.sendMail([]string{in.ContactEmail}, nil, subject, body); merr != nil && !errors.Is(merr, errMailQueued) {
		// Without the email nobody holds the password, so the account would
		// be unusable and the address could not be registered again. Undo
		// the registration and let the person retry.
		s.logCorrespondence(r, tenant, "", "OUT", "tpa_welcome", subject, logBody, []string{in.ContactEmail}, nil, "system:tpa-register", merr.Error())
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.tpa_users WHERE tenant=$1 AND tpa_id=$2`, tenant, id)
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.tpas WHERE tenant=$1 AND id=$2`, tenant, id)
		if derr := s.kcDeleteUser(r.Context(), tok, userID); derr != nil {
			slog.Error("TPA registration rollback: could not delete the Keycloak user", "user_id", userID, "err", derr)
		}
		// 422, not 502: the CDN in front of the portal replaces the body of
		// a 502 with its own error page, which hid this message.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "we could not send the sign-in email to that address, so nothing was registered. Check the email address and try again.",
		})
		return
	}
	// The mail log keeps the message without the password.
	s.logCorrespondence(r, tenant, "", "OUT", "tpa_welcome", subject, logBody, []string{in.ContactEmail}, nil, "system:tpa-register")
	s.logAudit(r.Context(), tenant, "", "TPA_REGISTERED", map[string]any{
		"tpa_id": id, "name": in.Name, "username": username, "user_id": userID, "credentials_emailed_to": in.ContactEmail,
	})
	if s.featureEnabled(r, tenant, "entity360") {
		s.ensureEntityQuiet(r.Context(), tenant, "tpas", id, "TPA", in.Name, "", "", "", "")
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tpa_id": id, "status": "ACTIVE",
		"next": "Check your email for how to sign in. Your organization is already linked to your portal account.",
	})
}

// validMailbox: a single plain address (no display name, no list) with a
// dotted domain and no doubled or edge dots in the local part.
func validMailbox(addr string) bool {
	a, err := mail.ParseAddress(addr)
	if err != nil || a.Address != addr || strings.ContainsAny(addr, " \t") {
		return false
	}
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" || !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	return !strings.Contains(local, "..") && !strings.HasPrefix(local, ".") && !strings.HasSuffix(local, ".")
}

// tpaWelcomeLogNote is what the mail log keeps for a TPA sign-in email.
const tpaWelcomeLogNote = "Sign-in details for the new TPA portal account were emailed to the recipient. They are not kept in the mail log."

// tpaWelcomeMail builds the sign-in email for a newly registered TPA.
func tpaWelcomeMail(orgName, contactName, username, tempPassword, portalBase string) (subject, body string) {
	greeting := "Hello,"
	if strings.TrimSpace(contactName) != "" {
		greeting = "Hello " + strings.TrimSpace(contactName) + ","
	}
	pw := tempPassword
	subject = "Your TPA portal account — " + orgName
	body = greeting + "\n\n" +
		orgName + " is now registered as a third-party administrator. Your portal account is ready and already linked to your organization.\n\n" +
		"Sign in:   " + strings.TrimRight(portalBase, "/") + "/app.html\n" +
		"Username:  " + username + "\n" +
		"Password:  " + pw + "\n\n" +
		"This password is temporary: you will be asked to choose a new one the first time you sign in.\n\n" +
		"Once signed in, open TPA in the menu to add the organizations you act for, then file, pay and track disputes on their behalf.\n\n" +
		"If you did not register, you can ignore this email — the account cannot be used without this message."
	return subject, body
}

// claimTPA links the authenticated TPA user to their org via the claim code.
// Deliberately open to any authenticated tenant member otherwise (a brand
// new TPA user has no other role yet) -- CASE_MANAGER is the one exclusion,
// since the only reason a CASE_MANAGER would ever call this is "Act as this
// TPA" from the oversight list, an administrative action reviewer no
// longer gets per the "reviewer only reviews" follow-up. TENANT_ADMIN is
// exempted from that exclusion -- a different role from reviewer, keeps
// full access (hasRole("CASE_MANAGER") is also true for TENANT_ADMIN via
// its own hasRole quirk, onboarding.go, so this must check explicitly).
func (s *server) claimTPA(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if hasRole(p, "CASE_MANAGER") && !hasRole(p, "TENANT_ADMIN") {
		http.Error(w, `{"error":"forbidden: reviewer cannot act as a TPA"}`, http.StatusForbidden)
		return
	}
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

// unlinkTPA: POST /tpa/unlink -- the way back out of claimTPA/"Act as this
// TPA", for staff only. A genuine single-role TPA account IS that
// organization and has no legitimate reason to unlink itself (it would
// just lock itself out, needing a fresh code from an admin to get back
// in) -- this exists for a staffer who claimed a TPA from the oversight
// list for support/escalation and now wants #/tpa to go back to showing
// the oversight list instead of staying linked to that org forever.
func (s *server) unlinkTPA(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	res, err := s.db.Exec(r.Context(), `
		DELETE FROM public.tpa_users WHERE tenant=$1 AND user_sub=$2`, tenant, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not linked to any TPA organization"}`, http.StatusNotFound)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_UNLINKED", map[string]any{"by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"status": "unlinked"})
}

// tpaInviteCode: POST /tpa/invite-code -- mints a fresh claim code for the
// caller's OWN already-linked org, so they can add a second staff member
// at their firm themselves (that colleague signs up for any portal
// account, then claims with this code) without going through an admin.
// Overwrites any code already pending; the old one stops working.
func (s *server) tpaInviteCode(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	code := "TPA-" + strings.ToUpper(hex.EncodeToString(func() []byte { b := make([]byte, 3); _, _ = rand.Read(b); return b }()))
	if _, err := s.db.Exec(r.Context(), `
		UPDATE public.tpas SET claim_code=$1, updated_at=now() WHERE id=$2`, code, org.ID); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	s.logAudit(r.Context(), tenant, "", "TPA_INVITE_CODE_GENERATED", map[string]any{"tpa_id": org.ID, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"claim_code": code})
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
		GroupID      string `json:"group_id"` // the provider group it belongs to ("" = none)
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.PartyName == "" {
		http.Error(w, `{"error":"party_name required"}`, http.StatusBadRequest)
		return
	}
	if msg := s.tpaGroupUsable(r, tenant, org.ID, in.GroupID); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
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
		INSERT INTO public.tpa_clients (tenant, tpa_id, party_name, party_type, contact_email, group_id)
		VALUES ($1,$2,$3,$4,$5,nullif($6,'')::uuid) RETURNING id`, tenant, org.ID, in.PartyName, pt, in.ContactEmail, in.GroupID).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"client already exists for this TPA"}`, http.StatusConflict)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_CLIENT_ADDED", map[string]any{"tpa_id": org.ID, "client_id": id, "party": in.PartyName, "by": p.Subject})
	if s.featureEnabled(r, tenant, "entity360") {
		s.ensureEntityQuiet(r.Context(), tenant, "tpa_clients", id, pt, in.PartyName, "", "", "", "")
	}
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
		       (SELECT count(*) FROM public.case_origin o WHERE o.client_id=c.id) AS case_count,
		       coalesce(c.group_id::text,'') AS group_id, coalesce(g.name,'') AS group_name
		FROM public.tpa_clients c LEFT JOIN public.tpa_provider_groups g ON g.id=c.group_id
		WHERE c.tenant=$1 AND c.tpa_id=$2 ORDER BY lower(coalesce(g.name,'~')), c.party_name`, tenant, org.ID)
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
		PackageID           string `json:"package_id"`   // optional until onboarding completes for the client
		ContactName         string `json:"contact_name"` // client's contact for the dispute
		DisputedAmountCents int64  `json:"disputed_amount_cents"`
		Notes               string `json:"notes"`
		OtherPartyName      string `json:"other_party_name"`  // the health plan the dispute is against
		OtherPartyEmail     string `json:"other_party_email"` // where its notices go
		OtherPartyNAIC      string `json:"other_party_naic"`
		// The filer's own answers to two eligibility questions the
		// documents often don't state: "" = not sure, left to the documents.
		ProviderType string        `json:"provider_type"`
		InNetwork    string        `json:"in_network"` // yes | no | ""
		Packet       *filingPacket `json:"packet"`     // the filing packet form (filing_packet.go)
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": decodeProblem(err)})
		return
	}
	if in.ClientID == "" {
		http.Error(w, `{"error":"client_id required"}`, http.StatusBadRequest)
		return
	}
	if msg := disputedAmountProblem(in.DisputedAmountCents, true); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
		return
	}
	if packetRejected(w, in.Packet) {
		return
	}
	answers, msg := s.filerEligibilityAnswers(r, tenant, in.ProviderType, in.InNetwork)
	if msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg)+`"}`, http.StatusBadRequest)
		return
	}
	// The packet form's plan name and notice email fill what the short form left blank.
	rsp := in.Packet.otherParty(respondent{Side: "HEALTH_PLAN", Name: in.OtherPartyName, Email: in.OtherPartyEmail, NAIC: in.OtherPartyNAIC})
	if msg := rsp.check(); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg)+`"}`, http.StatusBadRequest)
		return
	}
	if msg := emailReachMsg(r.Context(), "the health plan's email", rsp.Email); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg)+`"}`, http.StatusBadRequest)
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
	if why := s.clientNPIBlock(r.Context(), tenant, in.ClientID); why != "" {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "this dispute can't be filed yet: "+why), http.StatusConflict)
		return
	}
	// Onboarding gate (tpa_onboarding): optional unless the tenant turns on
	// onboarding_required, which makes every filing reference a READY package
	// and a verified payment rail. A package named on a filing must be READY.
	if s.featureEnabled(r, tenant, "tpa_onboarding") {
		required := s.featureEnabled(r, tenant, "onboarding_required")
		if required || in.PackageID != "" {
			if in.PackageID == "" {
				http.Error(w, `{"error":"filings must reference this client's READY package (package_id) — complete its onboarding first"}`, http.StatusConflict)
				return
			}
			var pkgStatus string
			err = s.db.QueryRow(r.Context(), `
				SELECT status FROM public.tpa_packages
				WHERE tenant=$1 AND tpa_id=$2 AND client_id=$3 AND id=$4`,
				tenant, org.ID, in.ClientID, in.PackageID).Scan(&pkgStatus)
			if ok, _ := s.packageFileable(r.Context(), tenant, "tpa", in.PackageID); err != nil || !ok {
				http.Error(w, `{"error":"these documents are still being read — file once they are (a minute or two), or file without them; none of them is required"}`, http.StatusConflict)
				return
			}
			var verified int
			_ = s.db.QueryRow(r.Context(), `
				SELECT count(*) FROM public.tpa_payment_methods
				WHERE tenant=$1 AND client_id=$2 AND verified AND status='ACTIVE'`,
				tenant, in.ClientID).Scan(&verified)
			if verified == 0 && required {
				http.Error(w, `{"error":"no verified payment method for this client"}`, http.StatusConflict)
				return
			}
		}
	}
	caseID, caseNumber, st, err := s.fileTPADispute(r, tenant, org, in.ClientID, partyName, partyType, in.PackageID, in.ContactName, in.Notes, in.DisputedAmountCents)
	if err == nil {
		s.recordRespondent(r, tenant, caseID, rsp)
		s.applyFilerEligibilityAnswers(r.Context(), tenant, caseID, answers)
		s.fileWithPacket(r, tenant, caseID, in.Packet, "TPA "+org.Name)
	}
	if err != nil {
		switch {
		case errors.Is(err, errNotProgrammed):
			http.Error(w, `{"error":"TPA filing is available on programmed tenants"}`, http.StatusBadRequest)
		case isUniqueViolation(err):
			http.Error(w, `{"error":"case_number already exists — retry"}`, http.StatusConflict)
		default:
			slog.Error("TPA filing: case creation failed", "tenant", tenant, "client", in.ClientID, "err", err)
			http.Error(w, `{"error":"case creation failed"}`, http.StatusBadGateway)
		}
		return
	}
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
	// Staff may name any TPA with ?tpa_id=. Everyone else -- and staff who
	// did not name one -- gets the org their own account is linked to. The
	// second case used to be a 400 ("tpa_id required for staff view"): a
	// staffer who had clicked "Act as this TPA" is linked to that org, the
	// page then loads this dashboard without a tpa_id, and it failed.
	isStaff := hasAnyRole(p, "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
	orgID := ""
	if q := r.URL.Query().Get("tpa_id"); q != "" && isStaff {
		orgID = q
	} else if org, err := s.tpaForPrincipal(r, tenant); err == nil {
		orgID = org.ID
	} else if !isStaff {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	if orgID == "" {
		http.Error(w, `{"error":"not linked to a TPA organization -- pass tpa_id to view one"}`, http.StatusBadRequest)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT o.case_id, o.initiating_party_name, o.initiating_party_type, o.filed_by_email,
		       o.created_at::date::text AS filed,
		       c.case_number, c.status, coalesce(c.agency_status,'') AS agency_status, c.disputed_amount_cents,
		       cl.party_name AS client_name, cl.id AS client_id,
		       coalesce(c.details->>'initiating_party_contact','') AS contact_name,
		       coalesce(c.details->>'filing_notes','') AS notes,
		       (SELECT coalesce(sum(i.amount_cents),0) FROM public.invoices i
		         WHERE i.tenant=o.tenant AND i.case_id=o.case_id)::bigint AS invoiced_cents,
		       (SELECT coalesce(sum(py.amount_cents),0) FROM public.payments py
		         WHERE py.tenant=o.tenant AND py.case_id=o.case_id AND py.status='PAID')::bigint AS paid_cents
		FROM public.case_origin o
		JOIN public.tpa_clients cl ON cl.id = o.client_id
		JOIN tenant_%s.cases c ON c.id::text = o.case_id
		WHERE o.tenant=$1 AND o.tpa_id=$2
		ORDER BY o.created_at DESC LIMIT 500`, sanitizeTenant(tenant)), tenant, orgID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.attachFeeStates(r.Context(), tenant, rows)
	// Roll up per initiating party.
	type rollup struct {
		ClientID  string `json:"client_id"`
		Party     string `json:"initiating_party"`
		PartyType string `json:"party_type"`
		Cases     int    `json:"cases"`
		Open      int    `json:"open"`
		Invoiced  int64  `json:"invoiced_cents"`
		Paid      int64  `json:"paid_cents"`
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
	// The claim code is NOT in this list, and staff cannot mint one. A code
	// is the TPA's to generate (tpaInviteCode) and to hand over: staff who
	// need to act for a TPA ask it for a code and enter it like anyone else
	// (claimTPA). Listing or minting codes here let an admin link their own
	// account to any TPA without that TPA knowing.
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
	// CASE_MANAGER removed, TENANT_ADMIN added back, same reasoning as
	// the removed staff code-minting endpoint.
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") {
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

// errNotProgrammed: TPA filing needs a programmed tenant (program rules).
var errNotProgrammed = errors.New("not a programmed tenant")

// fileTPADispute opens the case for one dispute a TPA files for a client --
// the part single filing (tpaIntake) and bulk filing (tpa_bulk_disputes.go)
// share once their own checks have passed. With a package, the package is
// marked FILED and its documents are attached to the case.
func (s *server) fileTPADispute(r *http.Request, tenant string, org *tpaOrg, clientID, partyName, partyType, packageID, contactName, notes string, amountCents int64) (caseID, caseNumber, status string, err error) {
	return s.fileTPADisputeOpt(r, tenant, org, clientID, partyName, partyType, packageID, contactName, notes, amountCents, false)
}

// fileTPADisputeOpt: bulk=true for a bulk-filed row. Its fee is paid with the
// batch's one checkout instead of a link per dispute, and a TPA on the
// program's invoiced_filer_orgs list is invoiced instead of paying up front.
func (s *server) fileTPADisputeOpt(r *http.Request, tenant string, org *tpaOrg, clientID, partyName, partyType, packageID, contactName, notes string, amountCents int64, bulk bool) (caseID, caseNumber, status string, err error) {
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		return "", "", "", errNotProgrammed
	}
	// The TPA is the operational contact: payment link, case number, and
	// upload link all flow to them — they pay and manage documents on behalf.
	deferFee := false
	if bulk {
		for _, approved := range cfg.Fees.InvoicedFilerOrgs {
			if strings.EqualFold(strings.TrimSpace(approved), strings.TrimSpace(org.Name)) {
				deferFee = true
			}
		}
	}
	caseID, caseNumber, err = s.startAhcaCaseFull(r, tenant, cfg,
		org.ContactEmail, org.ContactName, org.Name+" on behalf of "+partyName, partyType, amountCents, "", deferFee, bulk && !deferFee)
	if err != nil {
		return "", "", "", err
	}
	// Origin record: the dispute belongs to the initiating party; the TPA filed it.
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.case_origin
		  (tenant, case_id, tpa_id, client_id, initiating_party_name, initiating_party_type, filed_by_email)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		tenant, caseID, org.ID, clientID, partyName, partyType, org.ContactEmail)
	// Mark the validated package as consumed by this filing.
	if packageID != "" {
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.tpa_packages SET status='FILED', case_id=$3, updated_at=now()
			WHERE tenant=$1 AND id=$2`, tenant, packageID, caseID)
		// The checked documents belong to the dispute: put them on the case.
		s.attachPackageDocsToCase(r, tenant, onboardingKinds["tpa"], packageID, caseID, "tpa:"+org.Name)
	}
	// Attribute the intake-fee payment row: paid by the TPA on behalf of the party.
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.payments SET payer_org=$3 WHERE tenant=$1 AND case_id=$2`,
		tenant, caseID, org.Name+" (on behalf of "+partyName+")")
	// Initiating party visible on the case itself.
	// The client's own contact and the filing notes are kept too, so the TPA
	// can see and correct them while the fee is unpaid (tpa_edit.go).
	originDetails, _ := json.Marshal(map[string]string{
		"initiating_party": partyName, "filed_by_tpa": org.Name,
		"initiating_party_contact": strings.TrimSpace(contactName), "filing_notes": strings.TrimSpace(notes),
	})
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET details = details || $2::jsonb WHERE id=$1`, sanitizeTenant(tenant)),
		caseID, originDetails)
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&status)
	s.logAudit(r.Context(), tenant, caseID, "TPA_INTAKE", map[string]any{
		"tpa_id": org.ID, "client_id": clientID, "initiating_party": partyName, "case_number": caseNumber})
	return caseID, caseNumber, status, nil
}
