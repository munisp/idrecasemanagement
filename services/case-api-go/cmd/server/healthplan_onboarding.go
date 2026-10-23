package main

// healthplan_onboarding.go — payer-side onboarding rail (feature flag:
// healthplan_onboarding), the mirror of tpa_onboarding.go.
//
// A health plan registers as a first-class organization (NAIC / state
// license / line-of-business credentialing), claims itself via one-time
// code, rosters the parties it submits for (self-funded employer groups,
// delegated networks/IPAs, facilities), stands up a verified CARD/ACH rail
// PER PARTY, and assembles a validated six-document package per party —
// discrete uploads or one merged PDF split by doc-intel. Filing is gated on
// a READY package + verified payment.
//
// Reuses the TPA rail's validators and slot vocabulary; payer-specific
// differences live in credentialing (NAIC/license/LOB) and doc-intel's
// QPA-substantiation expectation for plan-uploaded OFFER_SUBSTANTIATION.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// hpPartyTypes: who a health plan may submit for.
var hpPartyTypes = map[string]bool{
	"EMPLOYER_GROUP":    true, // self-funded / ASO group the plan administers
	"DELEGATED_NETWORK": true, // IPA / capitated network the plan pays for
	"FACILITY":          true, // ancillary facility under the master contract
}

var hpLOBs = map[string]bool{
	"FULLY_INSURED": true, "ASO": true, "MEDICARE_ADVANTAGE": true, "MEDICAID_MANAGED": true,
}

type healthPlanOrg struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	NAIC         string `json:"naic"`
	LOB          string `json:"line_of_business"`
	ContactName  string `json:"contact_name"`
	ContactEmail string `json:"contact_email"`
	Status       string `json:"status"`
}

// healthPlanForPrincipal resolves the caller's plan org (linked via
// health_plan_users).
func (s *server) healthPlanForPrincipal(r *http.Request, tenant string) (*healthPlanOrg, error) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var o healthPlanOrg
	err := s.db.QueryRow(r.Context(), `
		SELECT h.id, h.name, coalesce(h.naic,''), coalesce(h.line_of_business,''),
		       h.contact_name, h.contact_email, h.status
		FROM public.health_plan_users u JOIN public.health_plans h ON h.id = u.plan_id
		WHERE u.tenant=$1 AND u.user_sub=$2`, tenant, p.Subject).
		Scan(&o.ID, &o.Name, &o.NAIC, &o.LOB, &o.ContactName, &o.ContactEmail, &o.Status)
	if err != nil {
		return nil, fmt.Errorf("no health plan organization linked to your account — claim one with your claim code")
	}
	if o.Status != "ACTIVE" {
		return nil, fmt.Errorf("health plan organization is %s", o.Status)
	}
	return &o, nil
}

// registerHealthPlan: PUBLIC self-serve onboarding, the same shape as a TPA's
// (registerTPA): it creates the plan AND its first HEALTH_PLAN portal login,
// links them, and emails the sign-in details to the contact. It used to hand
// the claim code back in the response, with no throttle, to anyone who
// posted a name and an address -- and the code could be claimed from any
// account. The code now never leaves the database; the login goes to the
// registered mailbox only. FEIN is required, and NAIC for every line of
// business but ASO (a self-funded group has none).
func (s *server) registerHealthPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tenant         string `json:"tenant"`
		Name           string `json:"name"`
		NAIC           string `json:"naic"`
		FEIN           string `json:"fein"`
		StateLicense   string `json:"state_license"`
		LineOfBusiness string `json:"line_of_business"`
		QPAContact     string `json:"qpa_contact_email"`
		ContactName    string `json:"contact_name"`
		ContactEmail   string `json:"contact_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		in.Tenant == "" || in.Name == "" || in.ContactEmail == "" || !strings.Contains(in.ContactEmail, "@") {
		http.Error(w, `{"error":"tenant, name, and a valid contact_email are required"}`, http.StatusBadRequest)
		return
	}
	tenant := strings.ToLower(strings.TrimSpace(in.Tenant))
	in.ContactEmail = strings.TrimSpace(in.ContactEmail)
	if !validMailbox(in.ContactEmail) {
		http.Error(w, `{"error":"enter a valid contact email address — your sign-in details will be sent there"}`, http.StatusBadRequest)
		return
	}
	if msg := emailReachMsg(r.Context(), "the contact email", in.ContactEmail); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg+" — your sign-in details will be sent there")+`"}`, http.StatusBadRequest)
		return
	}
	lob := strings.ToUpper(strings.TrimSpace(in.LineOfBusiness))
	if !hpLOBs[lob] {
		http.Error(w, `{"error":"line_of_business is required: FULLY_INSURED|ASO|MEDICARE_ADVANTAGE|MEDICAID_MANAGED"}`, http.StatusBadRequest)
		return
	}
	in.FEIN = normTIN(in.FEIN)
	if msg := feinProblem(in.FEIN); msg != "" {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), http.StatusBadRequest)
		return
	}
	in.NAIC = strings.TrimSpace(in.NAIC)
	if lob == "ASO" && in.NAIC != "" && !isDigits(in.NAIC, 5) || lob != "ASO" && !isDigits(in.NAIC, 5) {
		http.Error(w, `{"error":"NAIC company code is required (5 digits) for an insured line of business"}`, http.StatusBadRequest)
		return
	}
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	if !s.rateLimit("hp-register:"+ip, 10, 3600) {
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
		result = s.kcCreateUser(r.Context(), tok, username, in.ContactEmail, "HEALTH_PLAN", []string{groupPath})
		if _, failed := result["error"]; !failed {
			break
		}
		if msg, _ := result["error"].(string); !strings.Contains(msg, "already exists") {
			writeJSON(w, http.StatusBadGateway, result)
			return
		}
		username = base + hex6()[:4]
	}
	if _, failed := result["error"]; failed {
		writeJSON(w, http.StatusConflict, result)
		return
	}
	userID, _ := result["user_id"].(string)
	if userID == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "account created but its Keycloak id was unavailable -- cannot link it to the plan"})
		return
	}
	undo := func() {
		if derr := s.kcDeleteUser(r.Context(), tok, userID); derr != nil {
			slog.Error("health plan registration rollback: could not delete the Keycloak user", "user_id", userID, "err", derr)
		}
	}
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.health_plans
		  (tenant, name, naic, fein, state_license, line_of_business, qpa_contact_email, contact_name, contact_email, claim_code, status)
		VALUES ($1, $2, NULLIF($3,''), $4, NULLIF($5,''), $6, NULLIF($7,''), $8, $9, $10, 'ACTIVE')
		RETURNING id`,
		tenant, in.Name, in.NAIC, in.FEIN, in.StateLicense, lob,
		in.QPAContact, in.ContactName, in.ContactEmail, "HP-"+strings.ToUpper(hex6())).Scan(&id); err != nil {
		undo()
		http.Error(w, `{"error":"registration failed — an organization with this email may already exist for this program"}`, http.StatusConflict)
		return
	}
	dropPlan := func() {
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.health_plan_users WHERE tenant=$1 AND plan_id=$2`, tenant, id)
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.health_plans WHERE tenant=$1 AND id=$2`, tenant, id)
		undo()
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.health_plan_users (tenant, user_sub, plan_id) VALUES ($1,$2,$3)`,
		tenant, userID, id); err != nil {
		dropPlan()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "registration failed while linking the account — nothing was registered, try again"})
		return
	}
	if s.featureEnabled(r, tenant, "entity360") {
		s.ensureEntityQuiet(r.Context(), tenant, "health_plans", id, "HEALTH_PLAN", in.Name, "", in.FEIN, in.NAIC, in.FEIN)
	}
	tempPassword, _ := result["temporary_password"].(string)
	subject, body := hpWelcomeMail(in.Name, in.ContactName, username, tempPassword, s.cfg.PortalBaseURL)
	// A queued email (the provider's limit was reached) still arrives: the registration stands.
	if merr := s.sendMail([]string{in.ContactEmail}, nil, subject, body); merr != nil && !errors.Is(merr, errMailQueued) {
		s.logCorrespondence(r, tenant, "", "OUT", "hp_welcome", subject, tpaWelcomeLogNote, []string{in.ContactEmail}, nil, "system:hp-register", merr.Error())
		dropPlan()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "we could not send the sign-in email to that address, so nothing was registered. Check the email address and try again.",
		})
		return
	}
	s.logCorrespondence(r, tenant, "", "OUT", "hp_welcome", subject, tpaWelcomeLogNote, []string{in.ContactEmail}, nil, "system:hp-register")
	if n := s.linkRespondentsToPlan(r.Context(), tenant, id, in.NAIC, in.ContactEmail, in.Name); n > 0 {
		slog.Info("new health plan linked to disputes already naming it", "plan", id, "disputes", n)
	}
	s.logAudit(r.Context(), tenant, "", "HEALTH_PLAN_REGISTERED", map[string]any{
		"plan_id": id, "name": in.Name, "username": username, "user_id": userID, "credentials_emailed_to": in.ContactEmail})
	writeJSON(w, http.StatusCreated, map[string]any{
		"plan_id": id, "status": "ACTIVE",
		"next": "Check your email for how to sign in. Your plan is already linked to your portal account.",
	})
}

// hpWelcomeMail builds the sign-in email for a newly registered health plan.
func hpWelcomeMail(planName, contactName, username, tempPassword, portalBase string) (subject, body string) {
	greeting := "Hello,"
	if strings.TrimSpace(contactName) != "" {
		greeting = "Hello " + strings.TrimSpace(contactName) + ","
	}
	subject = "Your health plan portal account — " + planName
	body = greeting + "\n\n" +
		planName + " is now registered. Your portal account is ready and already linked to your plan.\n\n" +
		"Sign in:   " + strings.TrimRight(portalBase, "/") + "/app.html\n" +
		"Username:  " + username + "\n" +
		"Password:  " + tempPassword + "\n\n" +
		"This password is temporary: you will be asked to choose a new one the first time you sign in.\n\n" +
		"Once signed in, open Health plan in the menu to roster the groups, networks and facilities you act for, set up payment, and assemble their document packages.\n\n" +
		"If you did not register, you can ignore this email — the account cannot be used without this message."
	return subject, body
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// hex6: 3 random bytes as uppercase hex — one-time claim codes.
func hex6() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// claimHealthPlan links the authenticated user to their plan org via the code.
func (s *server) claimHealthPlan(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// A plan's own login, or staff acting for it -- not any signed-in account
	// (a TPA or a provider must not become a health plan by knowing a code).
	if !hasAnyRole(p, "HEALTH_PLAN", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: only a health plan account or program staff can link a health plan"}`, http.StatusForbidden)
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
		SELECT id FROM public.health_plans WHERE tenant=$1 AND claim_code=$2 AND status='ACTIVE'`,
		tenant, strings.ToUpper(strings.TrimSpace(in.Code))).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"invalid claim code"}`, http.StatusNotFound)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.health_plan_users (tenant, user_sub, plan_id) VALUES ($1,$2,$3)
		ON CONFLICT (tenant, user_sub) DO UPDATE SET plan_id=$3`, tenant, p.Subject, id)
	_, _ = s.db.Exec(r.Context(), `UPDATE public.health_plans SET claim_code=NULL, updated_at=now() WHERE id=$1`, id)
	s.logAudit(r.Context(), tenant, "", "HEALTH_PLAN_CLAIMED", map[string]any{"plan_id": id, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"plan_id": id, "status": "linked"})
}

// hpMe: the caller's plan org.
// hpUnlink: POST /hp/unlink -- staff stop acting for a health plan (the way
// back out of claimHealthPlan; mirrors unlinkTPA). A plan's own login does
// not unlink itself.
func (s *server) hpUnlink(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FINANCE", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: only staff acting for a plan can unlink"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	res, err := s.db.Exec(r.Context(), `
		DELETE FROM public.health_plan_users WHERE tenant=$1 AND user_sub=$2`, tenant, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not linked to any health plan"}`, http.StatusNotFound)
		return
	}
	s.logAudit(r.Context(), tenant, "", "HEALTH_PLAN_UNLINKED", map[string]any{"by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"status": "unlinked"})
}

func (s *server) hpMe(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health_plan": org})
}

// --- parties ---------------------------------------------------------------

// hpAddParty rosters one party the plan submits for.
func (s *server) hpAddParty(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		PartyName    string `json:"party_name"`
		PartyType    string `json:"party_type"`
		ContactEmail string `json:"contact_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.PartyName) == "" {
		http.Error(w, `{"error":"party_name required"}`, http.StatusBadRequest)
		return
	}
	ptype := strings.ToUpper(strings.TrimSpace(in.PartyType))
	if !hpPartyTypes[ptype] {
		http.Error(w, `{"error":"party_type must be EMPLOYER_GROUP|DELEGATED_NETWORK|FACILITY"}`, http.StatusBadRequest)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.hp_parties (tenant, plan_id, party_name, party_type, contact_email)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')) RETURNING id`,
		tenant, org.ID, strings.TrimSpace(in.PartyName), ptype, strings.TrimSpace(in.ContactEmail)).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"party already on your roster"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "HP_PARTY_ADDED", map[string]any{"plan_id": org.ID, "party_id": id, "party_name": in.PartyName})
	if s.featureEnabled(r, tenant, "entity360") {
		s.ensureEntityQuiet(r.Context(), tenant, "hp_parties", id, ptype, in.PartyName, "", "", "", "")
	}
	writeJSON(w, http.StatusCreated, map[string]any{"party_id": id, "onboarding_status": "PROFILE_INCOMPLETE"})
}

// hpUpdatePartyProfile: NPI (required for networks/facilities), TIN, address.
func (s *server) hpUpdatePartyProfile(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	partyID := chi.URLParam(r, "partyId")
	var in struct {
		NPI     string `json:"npi"`
		TIN     string `json:"tin"`
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"npi, tin, address required"}`, http.StatusBadRequest)
		return
	}
	in.NPI, in.TIN, in.Address = strings.TrimSpace(in.NPI), strings.TrimSpace(in.TIN), strings.TrimSpace(in.Address)
	var tinMsg string
	in.TIN, tinMsg = tinCheck(in.TIN)
	var ptype, partyName, oldNPI, oldStatus string
	if err := s.db.QueryRow(r.Context(), `
		SELECT party_type, party_name, coalesce(npi,''), coalesce(npi_status,'') FROM public.hp_parties WHERE tenant=$1 AND plan_id=$2 AND id=$3`,
		tenant, org.ID, partyID).Scan(&ptype, &partyName, &oldNPI, &oldStatus); err != nil {
		http.Error(w, `{"error":"party not found"}`, http.StatusNotFound)
		return
	}
	// Employer groups have no NPI; networks and facilities must have one.
	if ptype != "EMPLOYER_GROUP" && !npiValid(in.NPI) {
		http.Error(w, `{"error":"NPI failed the 10-digit Luhn check (required for DELEGATED_NETWORK/FACILITY)"}`, http.StatusBadRequest)
		return
	}
	if in.NPI != "" && !npiValid(in.NPI) {
		http.Error(w, `{"error":"NPI failed the 10-digit Luhn check"}`, http.StatusBadRequest)
		return
	}
	if msg := tinMsg; msg != "" {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), http.StatusBadRequest)
		return
	}
	if in.Address == "" {
		http.Error(w, `{"error":"address required"}`, http.StatusBadRequest)
		return
	}
	// The registry decides a new NPI; an unchanged one keeps its decision.
	var chk npiCheck
	if in.NPI != "" && (in.NPI != oldNPI || oldStatus == "" || oldStatus == "UNVERIFIED") {
		chk = s.checkClientNPI(r.Context(), in.NPI, partyName, npiCacheFresh)
		if chk.Refuse != "" {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, chk.Refuse), http.StatusUnprocessableEntity)
			return
		}
	}
	if _, err := s.db.Exec(r.Context(), `
		UPDATE public.hp_parties SET npi=NULLIF($4,''), tin=$5, address=$6
		WHERE tenant=$1 AND plan_id=$2 AND id=$3`,
		tenant, org.ID, partyID, in.NPI, in.TIN, in.Address); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	switch {
	case chk.Status != "":
		s.saveNPICheck(r.Context(), "hp", tenant, partyID, chk)
	case in.NPI == "": // an employer group without one: nothing to decide
		_, _ = s.db.Exec(r.Context(), `UPDATE public.hp_parties SET npi_status=NULL, npi_registry_name=NULL WHERE tenant=$1 AND id::text=$2`, tenant, partyID)
	}
	s.runVerifications(r.Context(), "hp", tenant, partyID, in.NPI, partyName, in.TIN)
	st := s.recomputeHpPartyStatus(r, tenant, partyID)
	var npiStatus, registry string
	_ = s.db.QueryRow(r.Context(), `SELECT coalesce(npi_status,''), coalesce(npi_registry_name,'') FROM public.hp_parties WHERE tenant=$1 AND id::text=$2`,
		tenant, partyID).Scan(&npiStatus, &registry)
	s.logAudit(r.Context(), tenant, "", "HP_PARTY_PROFILE", map[string]any{"party_id": partyID, "onboarding_status": st, "npi_status": npiStatus})
	writeJSON(w, http.StatusOK, map[string]any{"party_id": partyID, "onboarding_status": st, "npi_status": npiStatus, "npi_registry_name": registry})
}

// recomputeHpPartyStatus derives PROFILE_INCOMPLETE -> PAYMENT_PENDING ->
// DOCS_PENDING -> READY. Employer groups skip the NPI requirement.
func (s *server) recomputeHpPartyStatus(r *http.Request, tenant, partyID string) string {
	return s.recomputeHpPartyStatusCtx(r.Context(), tenant, partyID)
}

func (s *server) recomputeHpPartyStatusCtx(ctx context.Context, tenant, partyID string) string {
	status := "PROFILE_INCOMPLETE"
	var npi, tin, addr *string
	var ptype, npiStatus string
	if err := s.db.QueryRow(ctx, `
		SELECT npi, tin, address, party_type, coalesce(npi_status,'') FROM public.hp_parties WHERE tenant=$1 AND id::text=$2`,
		tenant, partyID).Scan(&npi, &tin, &addr, &ptype, &npiStatus); err != nil {
		return status
	}
	profileOK := tin != nil && *tin != "" && addr != nil && *addr != "" &&
		(ptype == "EMPLOYER_GROUP" || (npi != nil && *npi != ""))
	if !profileOK {
		_, _ = s.db.Exec(ctx, `
			UPDATE public.hp_parties SET onboarding_status=$3 WHERE tenant=$1 AND id::text=$2`,
			tenant, partyID, status)
		return status
	}
	// An NPI the registry holds under another name waits for staff.
	if npiStatus == "NEEDS_REVIEW" || npiStatus == "REJECTED" {
		status = map[string]string{"NEEDS_REVIEW": "NPI_REVIEW", "REJECTED": "NPI_REJECTED"}[npiStatus]
		_, _ = s.db.Exec(ctx, `
			UPDATE public.hp_parties SET onboarding_status=$3 WHERE tenant=$1 AND id::text=$2`,
			tenant, partyID, status)
		return status
	}
	if hold := s.exclusionOnboardingStatus(ctx, "hp", tenant, partyID); hold != "" {
		_, _ = s.db.Exec(ctx, `
			UPDATE public.hp_parties SET onboarding_status=$3 WHERE tenant=$1 AND id::text=$2`,
			tenant, partyID, hold)
		return hold
	}
	status = "PAYMENT_PENDING"
	var payments int
	_ = s.db.QueryRow(ctx, `
		SELECT count(*) FROM public.hp_payment_methods
		WHERE tenant=$1 AND party_id::text=$2 AND verified AND status='ACTIVE'`,
		tenant, partyID).Scan(&payments)
	if payments == 0 {
		_, _ = s.db.Exec(ctx, `
			UPDATE public.hp_parties SET onboarding_status=$3 WHERE tenant=$1 AND id::text=$2`,
			tenant, partyID, status)
		return status
	}
	status = "DOCS_PENDING"
	var ready int
	_ = s.db.QueryRow(ctx, `
		SELECT count(*) FROM public.hp_packages
		WHERE tenant=$1 AND party_id::text=$2 AND status='READY'`,
		tenant, partyID).Scan(&ready)
	if ready > 0 {
		status = "READY"
	}
	_, _ = s.db.Exec(ctx, `
		UPDATE public.hp_parties SET onboarding_status=$3 WHERE tenant=$1 AND id::text=$2`,
		tenant, partyID, status)
	return status
}

// hpImportPartiesCSV: bulk roster. Header:
// party_name,party_type,contact_email,npi,tin,address
func (s *server) hpImportPartiesCSV(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, `{"error":"csv too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	rows, err := csv.NewReader(io.LimitReader(f, 8<<20)).ReadAll()
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"unparseable CSV"}`, http.StatusBadRequest)
		return
	}
	start := 0
	if len(rows[0]) > 0 && strings.EqualFold(strings.TrimSpace(rows[0][0]), "party_name") {
		start = 1
	}
	type rowResult struct {
		Row       int    `json:"row"`
		PartyName string `json:"party_name"`
		OK        bool   `json:"ok"`
		PartyID   string `json:"party_id,omitempty"`
		NPIStatus string `json:"npi_status,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	report := make([]rowResult, 0, len(rows)-start)
	imported := 0
	for i := start; i < len(rows); i++ {
		rec := rows[i]
		res := rowResult{Row: i + 1}
		if len(rec) < 1 || strings.TrimSpace(rec[0]) == "" {
			res.Error = "empty row"
			report = append(report, res)
			continue
		}
		cell := func(j int) string {
			if j < len(rec) {
				return strings.TrimSpace(rec[j])
			}
			return ""
		}
		name, ptype, email, npi, tin, addr := cell(0), strings.ToUpper(cell(1)), cell(2), cell(3), cell(4), cell(5)
		res.PartyName = name
		tinMsg := ""
		if tin != "" {
			tin, tinMsg = tinCheck(tin)
		}
		switch {
		case !hpPartyTypes[ptype]:
			res.Error = "party_type must be EMPLOYER_GROUP|DELEGATED_NETWORK|FACILITY"
		case npi != "" && !npiValid(npi):
			res.Error = "NPI failed Luhn check"
		case ptype != "EMPLOYER_GROUP" && npi == "":
			res.Error = "NPI required for DELEGATED_NETWORK/FACILITY"
		case tinMsg != "":
			res.Error = tinMsg
		default:
			var chk npiCheck
			if npi != "" {
				if chk = s.checkClientNPI(r.Context(), npi, name, npiCacheFresh); chk.Refuse != "" {
					res.Error = chk.Refuse
					break
				}
			}
			var id string
			err := s.db.QueryRow(r.Context(), `
				INSERT INTO public.hp_parties (tenant, plan_id, party_name, party_type, contact_email, npi, tin, address)
				VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''))
				ON CONFLICT (tenant, plan_id, party_name) DO UPDATE SET
				  contact_email=excluded.contact_email, npi=coalesce(excluded.npi, public.hp_parties.npi),
				  tin=coalesce(excluded.tin, public.hp_parties.tin), address=coalesce(excluded.address, public.hp_parties.address)
				RETURNING id`, tenant, org.ID, name, ptype, email, npi, tin, addr).Scan(&id)
			if err != nil {
				res.Error = "insert failed"
			} else {
				res.OK, res.PartyID, res.NPIStatus = true, id, chk.Status
				imported++
				if chk.Status != "" {
					s.saveNPICheck(r.Context(), "hp", tenant, id, chk)
				}
				s.runVerifications(r.Context(), "hp", tenant, id, npi, name, tin)
				s.recomputeHpPartyStatus(r, tenant, id)
				if s.featureEnabled(r, tenant, "entity360") {
					s.ensureEntityQuiet(r.Context(), tenant, "hp_parties", id, ptype, name, npi, tin, "", "")
				}
			}
		}
		report = append(report, res)
	}
	s.logAudit(r.Context(), tenant, "", "HP_PARTIES_IMPORTED", map[string]any{"plan_id": org.ID, "rows": len(report), "imported": imported})
	writeJSON(w, http.StatusOK, map[string]any{"imported": imported, "total": len(report), "rows": report})
}

// --- payment rails -----------------------------------------------------------

// hpAddPayment: CARD (processor token) or ACH (ABA checksum + auth capture)
// per party.
func (s *server) hpAddPayment(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	partyID := chi.URLParam(r, "partyId")
	var in struct {
		Method       string `json:"method"`
		Label        string `json:"label"`
		Brand        string `json:"brand"`
		Last4        string `json:"last4"`
		Routing      string `json:"routing"`
		ProcessorRef string `json:"processor_ref"`
		AuthReceived bool   `json:"auth_received"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"method required"}`, http.StatusBadRequest)
		return
	}
	in.Method = strings.ToUpper(strings.TrimSpace(in.Method))
	if len(in.Last4) != 4 {
		http.Error(w, `{"error":"last4 (4 digits) required"}`, http.StatusBadRequest)
		return
	}
	verified := false
	switch in.Method {
	case "CARD":
		if in.ProcessorRef == "" {
			http.Error(w, `{"error":"processor_ref (card token) required — raw card numbers are never accepted"}`, http.StatusBadRequest)
			return
		}
		verified = true
	case "ACH":
		if !routingValid(in.Routing) {
			http.Error(w, `{"error":"routing number failed the ABA checksum"}`, http.StatusBadRequest)
			return
		}
		if !in.AuthReceived {
			http.Error(w, `{"error":"ACH debit authorization must be captured (auth_received=true)"}`, http.StatusBadRequest)
			return
		}
		verified = true
	default:
		http.Error(w, `{"error":"method must be CARD or ACH"}`, http.StatusBadRequest)
		return
	}
	var owner int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.hp_parties WHERE tenant=$1 AND plan_id=$2 AND id=$3`,
		tenant, org.ID, partyID).Scan(&owner)
	if owner == 0 {
		http.Error(w, `{"error":"party not found"}`, http.StatusNotFound)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.hp_payment_methods
		  (tenant, plan_id, party_id, method, label, brand, last4, routing, processor_ref, auth_received, verified)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),$10,$11)
		RETURNING id`,
		tenant, org.ID, partyID, in.Method, in.Label, strings.ToUpper(in.Brand),
		in.Last4, in.Routing, in.ProcessorRef, in.AuthReceived, verified).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		return
	}
	st := s.recomputeHpPartyStatus(r, tenant, partyID)
	s.logAudit(r.Context(), tenant, "", "HP_PAYMENT_ADDED", map[string]any{
		"party_id": partyID, "method": in.Method, "last4": in.Last4, "verified": verified})
	writeJSON(w, http.StatusCreated, map[string]any{
		"payment_method_id": id, "verified": verified, "onboarding_status": st})
}

// hpListPayments: masked rails for one party.
func (s *server) hpListPayments(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT id, method, coalesce(label,''), coalesce(brand,''), last4,
		       coalesce(routing,''), auth_received, verified, status
		FROM public.hp_payment_methods
		WHERE tenant=$1 AND plan_id=$2 AND party_id=$3 ORDER BY created_at DESC`,
		tenant, org.ID, chi.URLParam(r, "partyId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, m, label, brand, last4, routing, status string
		var auth, verified bool
		if rows.Scan(&id, &m, &label, &brand, &last4, &routing, &auth, &verified, &status) == nil {
			out = append(out, map[string]any{
				"id": id, "method": m, "label": label, "brand": brand, "last4": last4,
				"routing": routing, "auth_received": auth, "verified": verified, "status": status})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payment_methods": out})
}

// --- document packages -------------------------------------------------------

func (s *server) hpPackageOwned(r *http.Request, tenant, planID, pkgID string) (partyID, mode, status string, err error) {
	err = s.db.QueryRow(r.Context(), `
		SELECT party_id, mode, status FROM public.hp_packages
		WHERE tenant=$1 AND plan_id=$2 AND id=$3`, tenant, planID, pkgID).
		Scan(&partyID, &mode, &status)
	if err != nil {
		err = fmt.Errorf("package not found")
	}
	return
}

// hpCreatePackage opens a DISCRETE or MERGED package for a party.
func (s *server) hpCreatePackage(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	partyID := chi.URLParam(r, "partyId")
	var in struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	mode := strings.ToUpper(in.Mode)
	if mode != "MERGED" {
		mode = "DISCRETE"
	}
	var owner int
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.hp_parties WHERE tenant=$1 AND plan_id=$2 AND id=$3`,
		tenant, org.ID, partyID).Scan(&owner)
	if owner == 0 {
		http.Error(w, `{"error":"party not found"}`, http.StatusNotFound)
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.hp_packages (tenant, plan_id, party_id, mode) VALUES ($1,$2,$3,$4) RETURNING id`,
		tenant, org.ID, partyID, mode).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "HP_PACKAGE_CREATED", map[string]any{"package_id": id, "party_id": partyID, "mode": mode})
	writeJSON(w, http.StatusCreated, map[string]any{
		"package_id": id, "mode": mode, "status": "ASSEMBLING", "slots": tpaSlots})
}

// hpStorePackageDoc seals + stores one document and publishes the validation
// event with actor=healthplan (doc-intel's QPA floor applies to payer-side
// OFFER_SUBSTANTIATION).
func (s *server) hpStorePackageDoc(r *http.Request, tenant, planID, pkgID, slot, filename string, raw []byte) (string, error) {
	if n, err := s.rds.incrExpire(s.cfg.RedisPrefix+":rl:hpdoc:"+tenant+":"+planID, 3600); err == nil && n > 400 {
		return "", fmt.Errorf("upload rate limit exceeded (400/hour)")
	}
	if err := docPolicy(filename, raw); err != nil {
		return "", err
	}
	if sig, ok := s.scanOrRefuse(discardWriter{}, bytes.NewReader(raw), "file"); !ok {
		return "", fmt.Errorf("malware scan rejected: %s", sig)
	}
	docID := newUUID()
	objectKey := fmt.Sprintf("%s/hp-packages/%s/%s.enc", tenant, pkgID, docID)
	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		return "", fmt.Errorf("seal failed")
	}
	if _, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
		bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
			UserMetadata: map[string]string{"tenant": tenant, "package": pkgID},
		}); err != nil {
		return "", fmt.Errorf("store failed")
	}
	// One of the six waits for doc-intel's verdict; an additional document is taken as filed (tpa_onboarding.go).
	more := moreDocKind(slot) != ""
	initial := map[bool]string{true: "ACCEPTED", false: "PENDING"}[more]
	var rowID string
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO public.hp_package_docs (tenant, package_id, slot, object_key, filename, status, analyzed_at)
		VALUES ($1,$2,$3,$4,$5,$6, CASE WHEN $6='ACCEPTED' THEN now() END)
		ON CONFLICT (package_id, slot) DO UPDATE SET
		  object_key=excluded.object_key, filename=excluded.filename,
		  status=excluded.status, reasons='[]'::jsonb, analyzed_at=excluded.analyzed_at
		RETURNING id`, tenant, pkgID, slot, objectKey, filename, initial).Scan(&rowID)
	if err != nil {
		return "", fmt.Errorf("db")
	}
	if more {
		return rowID, nil
	}
	s.packageDocReplaced(r, tenant, onboardingKinds["hp"], pkgID)
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "tpa_doc.uploaded", "tenant": tenant, "actor": "healthplan",
		"package_id": pkgID, "package_doc_id": rowID, "slot": slot,
		"object_key": objectKey, "filename": filename, "merged": false,
		"at": time.Now().UTC(),
	})
	return rowID, nil
}

// hpAttachDoc: one DISCRETE slot upload.
func (s *server) hpAttachDoc(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	_, mode, status, err := s.hpPackageOwned(r, tenant, org.ID, pkgID)
	if err != nil {
		http.Error(w, `{"error":"package not found"}`, http.StatusNotFound)
		return
	}
	// Until it is filed, a slot can be added or replaced: slots are checked
	// one at a time, so the package is VALIDATING after the first upload.
	if status == "FILED" {
		http.Error(w, `{"error":"this package has already been filed"}`, http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	if err := r.ParseMultipartForm(maxDocBytes); err != nil {
		http.Error(w, `{"error":"file too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	slot := slotForUpload(strings.ToUpper(strings.TrimSpace(r.FormValue("slot"))))
	if slot == "" {
		http.Error(w, `{"error":"slot must be one of: `+strings.Join(tpaSlots, ", ")+`, or MORE_ and one of: `+strings.Join(moreDocKinds, ", ")+`"}`, http.StatusBadRequest)
		return
	}
	// A merged set's six come from its one PDF; every additional document is added on its own.
	if mode != "DISCRETE" && moreDocKind(slot) == "" {
		http.Error(w, `{"error":"this package takes one merged PDF for its six documents"}`, http.StatusConflict)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	rowID, err := s.hpStorePackageDoc(r, tenant, org.ID, pkgID, slot, hdr.Filename, raw)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"package_doc_id": rowID, "slot": slot, "status": "PENDING",
		"note": "doc-intel validates the document; poll the package for the verdict"})
}

// hpUploadMerged: one PDF containing all six documents; doc-intel splits it.
func (s *server) hpUploadMerged(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	_, mode, status, err := s.hpPackageOwned(r, tenant, org.ID, pkgID)
	if err != nil {
		http.Error(w, `{"error":"package not found"}`, http.StatusNotFound)
		return
	}
	if mode != "MERGED" || status == "FILED" {
		http.Error(w, `{"error":"this package takes documents slot by slot, or it has already been filed"}`, http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	if err := r.ParseMultipartForm(maxDocBytes); err != nil {
		http.Error(w, `{"error":"file too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	if !bytes.HasPrefix(raw, []byte("%PDF")) {
		http.Error(w, `{"error":"merged package must be a single PDF"}`, http.StatusUnsupportedMediaType)
		return
	}
	if err := docPolicy(hdr.Filename, raw); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
		return
	}
	if sig, ok := s.scanOrRefuse(discardWriter{}, bytes.NewReader(raw), "file"); !ok {
		http.Error(w, fmt.Sprintf(`{"error":"malware scan rejected: %s"}`, sig), http.StatusUnsupportedMediaType)
		return
	}
	objectKey := fmt.Sprintf("%s/hp-packages/%s/merged-%s.enc", tenant, pkgID, newUUID())
	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	if _, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
		bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
			UserMetadata: map[string]string{"tenant": tenant, "package": pkgID},
		}); err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.hp_packages SET source_object_key=$3, status='VALIDATING', updated_at=now()
		WHERE tenant=$1 AND id=$2`, tenant, pkgID, objectKey)
	_, _ = s.db.Exec(r.Context(), `DELETE FROM public.hp_package_docs WHERE tenant=$1 AND package_id=$2`, tenant, pkgID)
	s.packageDocReplaced(r, tenant, onboardingKinds["hp"], pkgID)
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "tpa_doc.uploaded", "tenant": tenant, "actor": "healthplan",
		"package_id": pkgID, "object_key": objectKey, "filename": hdr.Filename,
		"merged": true, "at": time.Now().UTC(),
	})
	s.logAudit(r.Context(), tenant, "", "HP_PACKAGE_MERGED_UPLOADED", map[string]any{"package_id": pkgID, "filename": hdr.Filename})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"package_id": pkgID, "status": "VALIDATING",
		"note": "doc-intel is splitting the PDF into the six required slots; poll the package for per-slot verdicts"})
}

// hpPackageStatus: per-slot verdict grid + package readiness.
func (s *server) hpPackageStatus(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	partyID, mode, status, err := s.hpPackageOwned(r, tenant, org.ID, pkgID)
	if err != nil {
		http.Error(w, `{"error":"package not found"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT id, slot, coalesce(filename,''), status, reasons::text,
		       coalesce(page_start::text,''), coalesce(page_end::text,''), coalesce(analyzed_at::text,'')
		FROM public.hp_package_docs WHERE tenant=$1 AND package_id=$2 ORDER BY slot`, tenant, pkgID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	bySlot := map[string]map[string]any{}
	for rows.Next() {
		var id, slot, fn, st, reasons, ps, pe, at string
		if rows.Scan(&id, &slot, &fn, &st, &reasons, &ps, &pe, &at) == nil {
			bySlot[slot] = map[string]any{
				"id": id, "slot": slot, "filename": fn, "status": st,
				"reasons": json.RawMessage(reasons), "page_start": ps, "page_end": pe, "analyzed_at": at}
		}
	}
	grid := make([]map[string]any, 0, len(tpaSlots))
	for _, slot := range tpaSlots {
		if d, ok := bySlot[slot]; ok {
			grid = append(grid, d)
		} else {
			grid = append(grid, map[string]any{"slot": slot, "status": "MISSING"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"package_id": pkgID, "party_id": partyID, "mode": mode,
		"status": status, "slots": grid, "more": moreDocsOf(bySlot), "more_kinds": moreDocKinds})
}

// hpOnboardingBoard: the plan's whole party roster with the three gates.
func (s *server) hpOnboardingBoard(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT c.id, c.party_name, c.party_type, c.onboarding_status, c.status,
		       coalesce(c.npi,''), coalesce(c.tin,''), coalesce(c.npi_status,''), coalesce(c.npi_registry_name,''), coalesce(c.npi_review_note,''),
		       (SELECT count(*) FROM public.hp_payment_methods m
		         WHERE m.tenant=c.tenant AND m.party_id=c.id AND m.verified AND m.status='ACTIVE'),
		       (SELECT count(*) FROM public.hp_packages pk
		         WHERE pk.tenant=c.tenant AND pk.party_id=c.id AND pk.status='READY'),
		       (SELECT json_agg(json_build_object('id',pk.id,'mode',pk.mode,'status',pk.status))
		        FROM public.hp_packages pk WHERE pk.tenant=c.tenant AND pk.party_id=c.id AND pk.status<>'FILED')
		FROM public.hp_parties c WHERE c.tenant=$1 AND c.plan_id=$2 ORDER BY c.party_name`,
		tenant, org.ID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	checks := s.verificationsSummary(r.Context(), "hp", tenant)
	parties := []map[string]any{}
	for rows.Next() {
		var id, name, ptype, ost, cst, npi, tin, npiSt, npiReg, npiNote string
		var npay, nready int
		var pkgs *string
		if rows.Scan(&id, &name, &ptype, &ost, &cst, &npi, &tin, &npiSt, &npiReg, &npiNote, &npay, &nready, &pkgs) != nil {
			continue
		}
		var packages json.RawMessage
		if pkgs != nil {
			packages = json.RawMessage(*pkgs)
		} else {
			packages = json.RawMessage("[]")
		}
		parties = append(parties, map[string]any{
			"party_id": id, "party_name": name, "party_type": ptype,
			"onboarding_status": ost, "status": cst,
			"profile_complete": tin != "" && (ptype == "EMPLOYER_GROUP" || npi != ""),
			"npi":              npi, "npi_status": npiSt, "npi_registry_name": npiReg, "npi_review_note": npiNote, "checks": checks[id],
			"verified_payments": npay, "ready_packages": nready,
			"open_packages": packages,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan_id": org.ID, "parties": parties, "required_slots": tpaSlots})
}

// --- bulk ZIP packages ---------------------------------------------------------

// hpUploadBulkZip: one ZIP, many parties. Top-level folder = party_name;
// inside, six slot-named files or one merged.pdf. Per-party report.
func (s *server) hpUploadBulkZip(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, zipMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, `{"error":"zip too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, zipMaxBytes+1))
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	zr, ok := s.openUploadedZip(w, raw) // size, readable, entries, virus scan
	if !ok {
		return
	}
	type entry struct {
		name string
		zf   *zip.File
	}
	byParty := map[string][]entry{}
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		parts := strings.SplitN(strings.Trim(zf.Name, "/"), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		byParty[parts[0]] = append(byParty[parts[0]], entry{parts[1], zf})
	}
	type partyReport struct {
		Party     string   `json:"party"`
		PackageID string   `json:"package_id,omitempty"`
		Mode      string   `json:"mode,omitempty"`
		Attached  []string `json:"attached,omitempty"`
		Errors    []string `json:"errors,omitempty"`
	}
	reports := []partyReport{}
	for partyName, entries := range byParty {
		rep := partyReport{Party: partyName}
		var partyID string
		err := s.db.QueryRow(r.Context(), `
			SELECT id FROM public.hp_parties WHERE tenant=$1 AND plan_id=$2 AND lower(party_name)=lower($3)`,
			tenant, org.ID, partyName).Scan(&partyID)
		if err != nil {
			rep.Errors = append(rep.Errors, "no party with this name on your roster — import the roster first")
			reports = append(reports, rep)
			continue
		}
		var merged *entry
		discrete := []entry{}
		for _, e := range entries {
			if strings.EqualFold(e.name, "merged.pdf") {
				m := e
				merged = &m
			} else {
				discrete = append(discrete, e)
			}
		}
		mode := "DISCRETE"
		if merged != nil {
			mode = "MERGED"
		}
		var pkgID string
		if err := s.db.QueryRow(r.Context(), `
			INSERT INTO public.hp_packages (tenant, plan_id, party_id, mode) VALUES ($1,$2,$3,$4) RETURNING id`,
			tenant, org.ID, partyID, mode).Scan(&pkgID); err != nil {
			rep.Errors = append(rep.Errors, "package create failed")
			reports = append(reports, rep)
			continue
		}
		rep.PackageID, rep.Mode = pkgID, mode
		if merged != nil {
			rc, err := merged.zf.Open()
			if err != nil {
				rep.Errors = append(rep.Errors, "merged.pdf unreadable")
			} else {
				data, _ := io.ReadAll(io.LimitReader(rc, maxDocBytes+1)) // one byte past the limit: an oversized entry is refused, not cut short
				rc.Close()
				if !bytes.HasPrefix(data, []byte("%PDF")) {
					rep.Errors = append(rep.Errors, "merged.pdf is not a PDF")
				} else if err := docPolicy("merged.pdf", data); err != nil {
					rep.Errors = append(rep.Errors, err.Error())
				} else {
					objectKey := fmt.Sprintf("%s/hp-packages/%s/merged-%s.enc", tenant, pkgID, newUUID())
					ct, serr := s.vaultSealDoc(r, tenant, objectKey, data)
					if serr == nil {
						_, serr = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
							bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
								UserMetadata: map[string]string{"tenant": tenant, "package": pkgID}})
					}
					if serr != nil {
						rep.Errors = append(rep.Errors, "merged.pdf store failed")
					} else {
						_, _ = s.db.Exec(r.Context(), `
							UPDATE public.hp_packages SET source_object_key=$3, status='VALIDATING', updated_at=now()
							WHERE tenant=$1 AND id=$2`, tenant, pkgID, objectKey)
						s.publish(r.Context(), tenant, "documents", map[string]any{
							"type": "tpa_doc.uploaded", "tenant": tenant, "actor": "healthplan",
							"package_id": pkgID, "object_key": objectKey, "filename": "merged.pdf",
							"merged": true, "at": time.Now().UTC(),
						})
						rep.Attached = append(rep.Attached, "merged.pdf")
					}
				}
			}
		}
		for _, e := range discrete {
			slot := tpaSlotFromFilename(e.name)
			if slot == "" {
				rep.Errors = append(rep.Errors, e.name+": filename does not map to a required slot")
				continue
			}
			rc, err := e.zf.Open()
			if err != nil {
				rep.Errors = append(rep.Errors, e.name+": unreadable")
				continue
			}
			data, _ := io.ReadAll(io.LimitReader(rc, maxDocBytes+1)) // one byte past the limit: an oversized entry is refused, not cut short
			rc.Close()
			if _, err := s.hpStorePackageDoc(r, tenant, org.ID, pkgID, slot, e.name, data); err != nil {
				rep.Errors = append(rep.Errors, e.name+": "+err.Error())
				continue
			}
			rep.Attached = append(rep.Attached, slot)
		}
		reports = append(reports, rep)
	}
	s.logAudit(r.Context(), tenant, "", "HP_BULK_PACKAGES", map[string]any{"plan_id": org.ID, "parties": len(reports)})
	writeJSON(w, http.StatusAccepted, map[string]any{"reports": reports})
}

// --- filing -------------------------------------------------------------------

// hpIntake: file a dispute on behalf of a rostered party. Gated on a READY
// package + verified payment once the party has started onboarding.
func (s *server) hpIntake(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "HEALTH_PLAN", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "healthplan_onboarding") {
		return
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		PartyID             string        `json:"party_id"`
		PackageID           string        `json:"package_id"`
		DisputedAmountCents int64         `json:"disputed_amount_cents"`
		Notes               string        `json:"notes"`
		OtherPartyName      string        `json:"other_party_name"`  // the provider the dispute is against
		OtherPartyEmail     string        `json:"other_party_email"` // where its notices go
		OtherPartyNPI       string        `json:"other_party_npi"`
		Packet              *filingPacket `json:"packet"` // the filing packet form (filing_packet.go)
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.PartyID == "" {
		http.Error(w, `{"error":"party_id required"}`, http.StatusBadRequest)
		return
	}
	if packetRejected(w, in.Packet) {
		return
	}
	// The packet form's provider name, email and NPI fill what the short form left blank.
	rsp := in.Packet.otherParty(respondent{Side: "PROVIDER", Name: in.OtherPartyName, Email: in.OtherPartyEmail, NPI: in.OtherPartyNPI})
	if msg := rsp.check(); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg)+`"}`, http.StatusBadRequest)
		return
	}
	if msg := s.respondentNPIProblem(r.Context(), rsp.NPI); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg)+`"}`, http.StatusBadRequest)
		return
	}
	if msg := emailReachMsg(r.Context(), "the provider's email", rsp.Email); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg)+`"}`, http.StatusBadRequest)
		return
	}
	var partyName, partyType, partyStatus string
	err = s.db.QueryRow(r.Context(), `
		SELECT party_name, party_type, status FROM public.hp_parties
		WHERE tenant=$1 AND plan_id=$2 AND id=$3`, tenant, org.ID, in.PartyID).
		Scan(&partyName, &partyType, &partyStatus)
	if err != nil {
		http.Error(w, `{"error":"party not found"}`, http.StatusNotFound)
		return
	}
	if partyStatus != "ACTIVE" {
		http.Error(w, `{"error":"party relationship is SUSPENDED"}`, http.StatusConflict)
		return
	}
	if why := s.npiBlock(r.Context(), "hp", tenant, in.PartyID); why != "" {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "this dispute can't be filed yet: "+why), http.StatusConflict)
		return
	}
	// Onboarding gate: optional unless the tenant turns on onboarding_required
	// (READY package + verified payment rail). A package named must be READY.
	required := s.featureEnabled(r, tenant, "onboarding_required")
	if required || in.PackageID != "" {
		if in.PackageID == "" {
			http.Error(w, `{"error":"filings must reference this party's READY package (package_id) — complete its onboarding first"}`, http.StatusConflict)
			return
		}
		var pkgStatus string
		err = s.db.QueryRow(r.Context(), `
			SELECT status FROM public.hp_packages
			WHERE tenant=$1 AND plan_id=$2 AND party_id=$3 AND id=$4`,
			tenant, org.ID, in.PartyID, in.PackageID).Scan(&pkgStatus)
		if ok, _ := s.packageFileable(r.Context(), tenant, "hp", in.PackageID); err != nil || !ok {
			http.Error(w, `{"error":"these documents can't be filed yet: every one of the six that was started must be accepted first (or file without them)"}`, http.StatusConflict)
			return
		}
		var verified int
		_ = s.db.QueryRow(r.Context(), `
			SELECT count(*) FROM public.hp_payment_methods
			WHERE tenant=$1 AND party_id=$2 AND verified AND status='ACTIVE'`,
			tenant, in.PartyID).Scan(&verified)
		if verified == 0 && required {
			http.Error(w, `{"error":"no verified payment method for this party"}`, http.StatusConflict)
			return
		}
	}
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"health plan filing is available on programmed tenants"}`, http.StatusBadRequest)
		return
	}
	// The plan is the operational contact; the case is filed as HEALTH_PLAN
	// on behalf of the rostered party.
	caseID, caseNumber, err := s.startAhcaCase(r, tenant, cfg,
		org.ContactEmail, org.ContactName, org.Name+" on behalf of "+partyName, "HEALTH_PLAN", in.DisputedAmountCents, "")
	if err != nil {
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"case_number already exists — retry"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"case creation failed"}`, http.StatusBadGateway)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.case_origin
		  (tenant, case_id, health_plan_id, hp_party_id, initiating_party_name, initiating_party_type, filed_by_email)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		tenant, caseID, org.ID, in.PartyID, partyName, partyType, org.ContactEmail)
	if in.PackageID != "" {
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.hp_packages SET status='FILED', case_id=$3, updated_at=now()
			WHERE tenant=$1 AND id=$2`, tenant, in.PackageID, caseID)
		// The checked documents belong to the dispute: put them on the case.
		s.attachPackageDocsToCase(r, tenant, onboardingKinds["hp"], in.PackageID, caseID, "health-plan:"+org.Name)
	}
	s.recordRespondent(r, tenant, caseID, rsp)
	s.fileWithPacket(r, tenant, caseID, in.Packet, "health plan "+org.Name)
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.cases SET details = details || jsonb_build_object('filed_by_plan', $2::text) WHERE id=$1`, sanitizeTenant(tenant)), caseID, org.Name)
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.payments SET payer_org=$3 WHERE tenant=$1 AND case_id=$2`,
		tenant, caseID, org.Name+" (on behalf of "+partyName+")")
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET details = details || $2::jsonb WHERE id=$1`, sanitizeTenant(tenant)),
		caseID, fmt.Sprintf(`{"submitting_party":%q,"filed_by_health_plan":%q}`, partyName, org.Name))
	var st string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&st)
	s.logAudit(r.Context(), tenant, caseID, "HP_INTAKE", map[string]any{
		"plan_id": org.ID, "party_id": in.PartyID, "party": partyName, "case_number": caseNumber})
	writeJSON(w, http.StatusCreated, map[string]any{
		"case_id": caseID, "case_number": caseNumber, "status": st,
		"payment_required": st == "AWAITING_PAYMENT",
		"party":            partyName, "filed_by": org.Name,
		"note": "payment link and case instructions go to the health plan contact — you pay and upload on behalf of " + partyName,
	})
}

// --- oversight ------------------------------------------------------------------

func (s *server) adminListHealthPlans(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// Same readers as the TPA oversight list (adminListTPAs).
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FINANCE", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT h.id, h.name, coalesce(h.naic,''), coalesce(h.line_of_business,''),
		       h.contact_email, h.status,
		       (SELECT count(*) FROM public.hp_parties c WHERE c.tenant=h.tenant AND c.plan_id=h.id)
		FROM public.health_plans h WHERE h.tenant=$1 ORDER BY h.name`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, naic, lob, email, status string
		var parties int
		if rows.Scan(&id, &name, &naic, &lob, &email, &status, &parties) == nil {
			out = append(out, map[string]any{
				"id": id, "name": name, "naic": naic, "line_of_business": lob,
				"contact_email": email, "status": status, "parties": parties})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"health_plans": out})
}

func (s *server) adminSetHealthPlanStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(strings.ToUpper(in.Status) != "ACTIVE" && strings.ToUpper(in.Status) != "SUSPENDED") {
		http.Error(w, `{"error":"status must be ACTIVE or SUSPENDED"}`, http.StatusBadRequest)
		return
	}
	tag, err := s.db.Exec(r.Context(), `
		UPDATE public.health_plans SET status=$3, updated_at=now() WHERE tenant=$1 AND id=$2`,
		tenant, chi.URLParam(r, "planId"), strings.ToUpper(in.Status))
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"health plan not found"}`, http.StatusNotFound)
		return
	}
	s.logAudit(r.Context(), tenant, "", "HEALTH_PLAN_STATUS", map[string]any{
		"plan_id": chi.URLParam(r, "planId"), "status": strings.ToUpper(in.Status), "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
