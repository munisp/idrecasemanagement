package main

// respondents.go — the other side of a dispute.
//
// A dispute is between two parties. The initiating party files it (a
// provider, directly or through its TPA, or a health plan); the responding
// party answers it. A TPA files for its clients; it does not answer disputes
// filed against them. Every filing names its other side, which is recorded
// in public.case_respondents and:
//
//   - linked to an account (created if new) whose contact gets the notices:
//     cases.payer_id / provider_id, which the program's letters to "the
//     health plan" / "the provider" already address;
//   - for a health plan, linked to the registered plan when there is one (by
//     NAIC, notice email or name), so the dispute shows on its dashboard
//     ("Disputes against us");
//   - for a provider, answered through the secure link its notice email
//     carries, or a staff invitation (party workspace, RESPONDING side).
//
// The responding side opens the filer's documents, adds its own, and submits
// its response. Submitting counts as the response at once (the user's call):
// the workflow gets PLAN_RESPONSE_RECEIVED and the Reviewer is told. A health
// plan can also ask for an opt-out; the Attorney decides it as before.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata" // programZone: the image may have no zoneinfo

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// respondent is the other side as the filer names it.
type respondent struct {
	Side  string `json:"side"` // HEALTH_PLAN | PROVIDER
	Name  string `json:"name"`
	Email string `json:"email"`
	NAIC  string `json:"naic,omitempty"`
	NPI   string `json:"npi,omitempty"`
}

// normalizeNAIC is the optional NAIC as a 5-digit code: digits only ("NAIC 12-345" -> "12345"), a shorter code
// padded with its leading zeros ("6319" -> "06319"). Anything else is dropped -- the code only helps match the
// plan, so a mistyped one never stops a filing.
func normalizeNAIC(v string) string {
	d := strings.Map(func(c rune) rune {
		if c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, v)
	if d == "" || len(d) > 5 || strings.Trim(d, "0") == "" {
		return ""
	}
	return strings.Repeat("0", 5-len(d)) + d
}

// check: who the other side is, and where its notices go, are required.
func (rsp *respondent) check() string {
	rsp.Name, rsp.Email = strings.TrimSpace(rsp.Name), strings.TrimSpace(rsp.Email)
	rsp.NAIC, rsp.NPI = normalizeNAIC(rsp.NAIC), strings.TrimSpace(rsp.NPI)
	other := "health plan"
	if rsp.Side == "PROVIDER" {
		other = "provider"
	}
	switch {
	case rsp.Name == "":
		return "name the " + other + " the dispute is against (other_party_name)"
	case len(rsp.Name) > 200:
		return "the other party's name is too long"
	case !validMailbox(rsp.Email):
		return "give the " + other + "'s email for dispute notices (other_party_email)"
	case rsp.NPI != "" && !npiValid(rsp.NPI):
		return "the provider's NPI failed the Luhn check"
	}
	return ""
}

// recordRespondent names the case's other side, links it to its account and
// (when on the platform) its organization, and tells that organization.
func (s *server) recordRespondent(r *http.Request, tenant, caseID string, rsp respondent) {
	ctx := r.Context()
	accountType, caseCol, detailKey := "PAYER_ORG", "payer_id", "health_plan"
	if rsp.Side == "PROVIDER" {
		accountType, caseCol, detailKey = "PROVIDER_ORG", "provider_id", "provider_org"
	}
	var accountID string
	err := s.db.QueryRow(ctx, `
		SELECT id::text FROM public.accounts
		WHERE tenant=$1 AND type=$2 AND (
		      ($3 <> '' AND npi=$3) OR ($4 <> '' AND custom->>'naic'=$4) OR lower(legal_name)=lower($5))
		ORDER BY (lower(legal_name)=lower($5)) DESC LIMIT 1`,
		tenant, accountType, rsp.NPI, rsp.NAIC, rsp.Name).Scan(&accountID)
	if err != nil {
		custom, _ := json.Marshal(map[string]string{"naic": rsp.NAIC, "source": "named by a filer"})
		if err := s.db.QueryRow(ctx, `
			INSERT INTO public.accounts (tenant, type, legal_name, npi, custom) VALUES ($1,$2,$3,NULLIF($4,''),$5)
			RETURNING id::text`, tenant, accountType, rsp.Name, rsp.NPI, custom).Scan(&accountID); err != nil {
			slog.Error("respondent account", "case", caseID, "err", err)
		}
	}
	if accountID != "" {
		var has int
		_ = s.db.QueryRow(ctx, `SELECT count(*) FROM public.contacts WHERE tenant=$1 AND account_id::text=$2 AND lower(email)=lower($3)`,
			tenant, accountID, rsp.Email).Scan(&has)
		if has == 0 {
			_, _ = s.db.Exec(ctx, `INSERT INTO public.contacts (tenant, account_id, name, role_title, email)
				VALUES ($1,$2::uuid,$3,'Dispute notices',$4)`, tenant, accountID, rsp.Name, rsp.Email)
		}
		_, _ = s.db.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET %s=$2 WHERE id::text=$1 AND coalesce(%s,'')=''`,
			sanitizeTenant(tenant), caseCol, caseCol), caseID, accountID)
	}
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`
		UPDATE tenant_%s.cases SET details = coalesce(details,'{}'::jsonb) || jsonb_build_object($2::text, $3::text)
		WHERE id::text=$1 AND NOT (coalesce(details,'{}'::jsonb) ? $2)`, sanitizeTenant(tenant)), caseID, detailKey, rsp.Name)

	// Only an organization on the same email domain as the notice address the
	// filer gave is linked: registration is public, so a NAIC or a name alone
	// would let anyone who registers as "that plan" read the dispute (and its
	// documents). The plan's login is emailed to its own address, so the
	// domain is one it controls. Within the domain the best match wins:
	// exact address, identifier and name, identifier, name. Otherwise the
	// other side is reached through the notice email and a staff invitation.
	var planID, tpaID, clientID *string
	if rsp.Side == "HEALTH_PLAN" {
		var id string
		if s.db.QueryRow(ctx, `
			SELECT id::text FROM public.health_plans WHERE tenant=$1 AND status='ACTIVE'
			  AND ((split_part(lower(contact_email),'@',2) = split_part(lower($3),'@',2) AND NOT $5) OR lower(contact_email)=lower($3))
			  AND (($2 <> '' AND naic=$2) OR lower(contact_email)=lower($3) OR lower(name)=lower($4))
			ORDER BY (lower(contact_email)=lower($3)) DESC, ($2 <> '' AND naic=$2 AND lower(name)=lower($4)) DESC,
			         ($2 <> '' AND naic=$2) DESC, created_at DESC LIMIT 1`, tenant, rsp.NAIC, rsp.Email, rsp.Name, isFreemail(rsp.Email)).Scan(&id) == nil {
			planID = &id
		}
	}
	// The provider side is not linked to a TPA: a TPA files for its clients,
	// it does not answer for them. The provider answers through its notice
	// email (a secure link) or a staff invitation.
	var acct any
	if accountID != "" {
		acct = accountID
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO public.case_respondents (tenant, case_id, side, name, email, naic, npi, account_id, plan_id, tpa_id, client_id)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8::uuid,$9::uuid,$10::uuid,$11::uuid)
		ON CONFLICT (tenant, case_id) DO UPDATE SET side=EXCLUDED.side, name=EXCLUDED.name, email=EXCLUDED.email,
		  naic=EXCLUDED.naic, npi=EXCLUDED.npi, account_id=EXCLUDED.account_id, plan_id=EXCLUDED.plan_id,
		  tpa_id=EXCLUDED.tpa_id, client_id=EXCLUDED.client_id`,
		tenant, caseID, rsp.Side, rsp.Name, rsp.Email, rsp.NAIC, rsp.NPI, acct, planID, tpaID, clientID); err != nil {
		slog.Error("record respondent", "case", caseID, "err", err)
		return
	}
	s.logActivity(ctx, tenant, caseID, "RESPONDENT_NAMED", fmt.Sprintf("Responding party: %s (%s, notices to %s)%s",
		rsp.Name, map[string]string{"HEALTH_PLAN": "health plan", "PROVIDER": "provider"}[rsp.Side], rsp.Email,
		map[bool]string{true: " — on the platform; the dispute is on its dashboard", false: ""}[planID != nil || tpaID != nil]))
	s.tellRespondentOpened(ctx, tenant, caseID) // now, if no fee holds the dispute back; else when it is paid
}

// eligibleForOtherSide is the SQL test (on a case_respondents row aliased rs)
// for "the other side is in this dispute now". It joins when the reviewer
// finds the dispute ELIGIBLE (a borderline call too: that only asks the PM
// for a second look, the decision stands), and once it has joined (it was
// told: notified_at / emailed_at) it keeps the dispute through a later hold
// (HOLD_AOR) and loses it only when the dispute is found INELIGIBLE. Before
// the first ELIGIBLE it is neither shown the dispute nor told of it -- except
// a side told of it already (before this rule) with no review on file yet.
const eligibleForOtherSide = `coalesce((SELECT CASE WHEN e.result = 'ELIGIBLE' THEN true
	                       WHEN e.result = 'INELIGIBLE' THEN false
	                       ELSE (rs.notified_at IS NOT NULL OR rs.emailed_at IS NOT NULL) END
	FROM public.eligibility_reviews e WHERE e.tenant = rs.tenant AND e.case_id = rs.case_id
	ORDER BY e.created_at DESC LIMIT 1), rs.notified_at IS NOT NULL OR rs.emailed_at IS NOT NULL)`

// freemailDomains: public email providers. A shared domain there says nothing
// about who owns an address, so a dispute is linked to an organization on one
// of them only by the exact address.
var freemailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "yahoo.com": true, "yahoo.co.uk": true, "ymail.com": true, "rocketmail.com": true,
	"outlook.com": true, "hotmail.com": true, "live.com": true, "msn.com": true, "icloud.com": true, "me.com": true, "mac.com": true,
	"aol.com": true, "proton.me": true, "protonmail.com": true, "gmx.com": true, "gmx.net": true, "mail.com": true, "zoho.com": true,
	"yandex.com": true, "yandex.ru": true, "comcast.net": true, "att.net": true, "verizon.net": true, "sbcglobal.net": true,
}

func isFreemail(email string) bool {
	_, domain, _ := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	return freemailDomains[domain]
}

// linkRespondentsToPlan: a newly registered health plan sees every dispute
// already naming it (by NAIC, notice email or name).
func (s *server) linkRespondentsToPlan(ctx context.Context, tenant, planID, naic, email, name string) int64 {
	tag, err := s.db.Exec(ctx, `
		UPDATE public.case_respondents SET plan_id=$2::uuid
		WHERE tenant=$1 AND side='HEALTH_PLAN' AND plan_id IS NULL
		  AND ((split_part(lower(email),'@',2) = split_part(lower($4),'@',2) AND NOT $6) OR lower(email)=lower($4))
		  AND (($3 <> '' AND naic=$3) OR lower(email)=lower($4) OR lower(name)=lower($5))`,
		tenant, planID, naic, email, name, isFreemail(email))
	if err != nil {
		return 0
	}
	// One notice for the ones found eligible, not one per dispute; the rest are told
	// when the reviewer finds them eligible (tellRespondentOpened).
	var open int
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`
		WITH t AS (
		  UPDATE public.case_respondents rs SET notified_at=now()
		  FROM tenant_%s.cases c
		  WHERE rs.tenant=$1 AND rs.plan_id=$2::uuid AND rs.notified_at IS NULL
		    AND c.id::text=rs.case_id AND `+eligibleForOtherSide+`
		  RETURNING 1)
		SELECT count(*) FROM t`, sanitizeTenant(tenant)), tenant, planID).Scan(&open)
	if open > 0 {
		s.notifyOrg(ctx, tenant, "plan", planID, "", "DISPUTE_AGAINST_YOU",
			fmt.Sprintf("%d dispute%s filed before you registered name%s you — respond under Disputes against us",
				open, map[bool]string{true: "", false: "s"}[open == 1], map[bool]string{true: "s", false: ""}[open == 1]), "#/healthplan")
	}
	return tag.RowsAffected()
}

// respondScope is who is answering: a health plan or an invited party, and
// how its cases are found.
type respondScope struct {
	actor   string // plan | party
	orgID   string
	subject string
	label   string // shown on the case ("Sunshine Health Plan")
}

func (s *server) respondScopeFor(r *http.Request, tenant, actor string) (respondScope, error) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	switch actor {
	case "plan":
		org, err := s.healthPlanForPrincipal(r, tenant)
		if err != nil {
			return respondScope{}, err
		}
		return respondScope{actor: "plan", orgID: org.ID, subject: p.Subject, label: org.Name}, nil
	}
	if !hasAnyRole(p, "PARTY") {
		return respondScope{}, fmt.Errorf("party accounts only")
	}
	return respondScope{actor: "party", subject: p.Subject, label: displayName(p)}, nil
}

// owns: is this case one the scope answers for?
func (s *server) respondentOwns(r *http.Request, tenant, caseID string, sc respondScope) bool {
	var ok bool
	switch sc.actor {
	case "plan":
		_ = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM public.case_respondents rs WHERE rs.tenant=$1 AND rs.case_id=$2 AND rs.plan_id::text=$3 AND `+eligibleForOtherSide+`)`,
			tenant, caseID, sc.orgID).Scan(&ok)
	default:
		ok = s.partyOwnsCase(r, tenant, caseID, sc.subject, "RESPONDING")
	}
	return ok
}

// respondingList: GET /hp/responding -- the disputes this
// organization is the other side of. A dispute waiting for the filer's fee
// is not open yet and is not shown.
func (s *server) respondingList(actor string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		sc, err := s.respondScopeFor(r, tenant, actor)
		if err != nil {
			http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
			return
		}
		col := "plan_id"
		rows, err := s.queryRows(r, fmt.Sprintf(`
			SELECT c.id::text AS case_id, c.case_number, c.status, coalesce(c.internal_status,'') AS internal_status,
			       coalesce(c.disputed_amount_cents,0) AS disputed_amount_cents, c.opened_at::text AS opened_at,
			       coalesce(c.details->>'initiating_party', c.details->>'requester_org', '') AS initiating_party,
			       coalesce(c.details->>'filed_by_tpa', c.details->>'filed_by_plan', '') AS filed_by,
			       rs.name AS respondent, rs.response_status, coalesce(rs.responded_at::text,'') AS responded_at,
			       coalesce(cl.party_name,'') AS your_client,
			       coalesce(c.program_dates->>'date_health_plan_response_due','') AS response_due,
			       coalesce(c.program_dates->>'plan_notified_at','') AS notified_at,
			       rs.side, coalesce(least(rs.notified_at, rs.emailed_at)::text,'') AS told_at
			FROM public.case_respondents rs
			JOIN tenant_%s.cases c ON c.id::text=rs.case_id
			LEFT JOIN public.tpa_clients cl ON cl.id=rs.client_id
			WHERE rs.tenant=$1 AND rs.%s::text=$2 AND `+eligibleForOtherSide+`
			ORDER BY (rs.response_status='AWAITING') DESC, c.opened_at DESC LIMIT 500`, sanitizeTenant(tenant), col), tenant, sc.orgID)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		s.fillResponseDue(r, tenant, rows)
		writeJSON(w, http.StatusOK, map[string]any{"cases": rows})
	}
}

// respondingCase: GET .../responding/{caseId} -- the dispute, the filer's
// documents and the response so far.
func (s *server) respondingCase(actor string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		sc, err := s.respondScopeFor(r, tenant, actor)
		caseID := chi.URLParam(r, "caseId")
		if err != nil || !s.respondentOwns(r, tenant, caseID, sc) {
			http.Error(w, `{"error":"no such dispute against you"}`, http.StatusNotFound)
			return
		}
		cs, _ := s.queryRows(r, fmt.Sprintf(`
			SELECT c.case_number, c.status, coalesce(c.internal_status,'') AS internal_status,
			       coalesce(c.disputed_amount_cents,0) AS disputed_amount_cents, c.opened_at::text AS opened_at,
			       coalesce(c.details->>'initiating_party', c.details->>'requester_org', '') AS initiating_party,
			       coalesce(c.details->>'filed_by_tpa', c.details->>'filed_by_plan', '') AS filed_by,
			       coalesce(c.details->>'disputed_issue','') AS disputed_issue,
			       coalesce(c.program_dates->>'date_health_plan_response_due','') AS response_due,
			       coalesce(c.program_dates->>'plan_notified_at','') AS notified_at,
			       coalesce(least(rs.notified_at, rs.emailed_at)::text,'') AS told_at,
			       rs.side, rs.name AS respondent, rs.response_status, coalesce(rs.response_note,'') AS response_note,
			       coalesce(rs.responded_at::text,'') AS responded_at
			FROM tenant_%s.cases c JOIN public.case_respondents rs ON rs.case_id=c.id::text AND rs.tenant=$1
			WHERE c.id::text=$2`, sanitizeTenant(tenant)), tenant, caseID)
		docs, _ := s.queryRows(r, fmt.Sprintf(`
			SELECT id::text AS id, coalesce(filename,'') AS filename, folder, created_at::text AS created_at,
			       (folder='PARTY_UPLOADS' AND uploaded_by LIKE 'respondent:%%') AS yours
			FROM tenant_%s.documents d WHERE d.case_id::text=$1 AND NOT d.sealed AND d.folder IN ('INTAKE','PARTY_UPLOADS') AND NOT `+internalDocSQL+`
			ORDER BY created_at DESC`, sanitizeTenant(tenant)), caseID)
		var c any
		if len(cs) > 0 {
			s.fillResponseDue(r, tenant, cs)
			c = cs[0]
		}
		writeJSON(w, http.StatusOK, map[string]any{"case": c, "documents": docs, "you": sc.label})
	}
}

// respondingDocFile: GET .../responding/{caseId}/documents/{docId}/file
func (s *server) respondingDocFile(actor string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		sc, err := s.respondScopeFor(r, tenant, actor)
		caseID, docID := chi.URLParam(r, "caseId"), chi.URLParam(r, "docId")
		if err != nil || !s.respondentOwns(r, tenant, caseID, sc) {
			http.Error(w, `{"error":"no such dispute against you"}`, http.StatusNotFound)
			return
		}
		var key, filename string
		if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
			SELECT object_key, coalesce(filename,'') FROM tenant_%s.documents
			WHERE id::text=$1 AND case_id::text=$2 AND NOT sealed AND folder IN ('INTAKE','PARTY_UPLOADS')
			  AND NOT (content_type = 'text/html' AND left(coalesce(filename,''), 22) = 'Eligibility Checklist ')`, sanitizeTenant(tenant)),
			docID, caseID).Scan(&key, &filename); err != nil {
			http.Error(w, `{"error":"document not found"}`, http.StatusNotFound)
			return
		}
		obj, err := s.docs.mc.GetObject(r.Context(), docBucket, key, minio.GetObjectOptions{})
		if err != nil {
			http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
			return
		}
		defer obj.Close()
		ct, err := io.ReadAll(obj)
		if err != nil {
			http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
			return
		}
		pt, err := s.vaultOpenDoc(r, tenant, key, ct)
		if err != nil {
			http.Error(w, `{"error":"decrypt failed"}`, http.StatusBadGateway)
			return
		}
		s.logAudit(r.Context(), tenant, caseID, "RESPONDENT_DOC_VIEWED", map[string]any{"doc_id": docID, "by": sc.subject, "as": sc.label})
		ctype := "application/octet-stream"
		if bytes.HasPrefix(pt, []byte("%PDF")) {
			ctype = "application/pdf"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(pt)
	}
}

// respondingUpload: POST .../responding/{caseId}/documents (multipart file)
// -- the responding side's own documents (QPA methodology, EOB, contract...).
func (s *server) respondingUpload(actor string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		sc, err := s.respondScopeFor(r, tenant, actor)
		caseID := chi.URLParam(r, "caseId")
		if err != nil || !s.respondentOwns(r, tenant, caseID, sc) {
			http.Error(w, `{"error":"no such dispute against you"}`, http.StatusNotFound)
			return
		}
		docID, filename, code, msg := s.storeCaseUpload(w, r, tenant, caseID, "respondent:"+sc.label, "Response — ")
		if code != 0 {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), code)
			return
		}
		hdrName := strings.TrimPrefix(filename, "Response — ")
		s.logActivity(r.Context(), tenant, caseID, "RESPONDENT_DOCUMENT", fmt.Sprintf("%s added a response document: %s", sc.label, hdrName))
		writeJSON(w, http.StatusCreated, map[string]any{"doc_id": docID, "filename": filename})
	}
}

// respondingSubmit: POST .../responding/{caseId}/submit {"note": "..."} --
// the response, counted at once.
func (s *server) respondingSubmit(actor string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		sc, err := s.respondScopeFor(r, tenant, actor)
		caseID := chi.URLParam(r, "caseId")
		if err != nil || !s.respondentOwns(r, tenant, caseID, sc) {
			http.Error(w, `{"error":"no such dispute against you"}`, http.StatusNotFound)
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
		var docs int
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT count(*) FROM tenant_%s.documents WHERE case_id::text=$1 AND folder='PARTY_UPLOADS' AND uploaded_by LIKE 'respondent:%%'`,
			sanitizeTenant(tenant)), caseID).Scan(&docs)
		if docs == 0 && strings.TrimSpace(in.Note) == "" {
			http.Error(w, `{"error":"add your response documents (or at least a written response) before submitting"}`, http.StatusBadRequest)
			return
		}
		tag, err := s.db.Exec(r.Context(), `
			UPDATE public.case_respondents SET response_status='SUBMITTED', response_note=NULLIF($3,''), responded_at=now(), responded_by=$4
			WHERE tenant=$1 AND case_id=$2 AND response_status<>'SUBMITTED'`, tenant, caseID, strings.TrimSpace(in.Note), sc.label)
		if err != nil || tag.RowsAffected() == 0 {
			http.Error(w, `{"error":"your response was already submitted"}`, http.StatusConflict)
			return
		}
		var caseNumber string
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
		wfID := fmt.Sprintf("IDR-%s-%s", strings.ToUpper(tenant), caseNumber)
		if s.loadProgram(r, tenant) != nil {
			wfID = fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
		}
		signalled := s.sigCase(r.Context(), tenant, caseID, wfID, "PLAN_RESPONSE_RECEIVED",
			map[string]any{"source": "respondent dashboard", "by": sc.label, "documents": docs}) == nil
		detail, _ := json.Marshal(map[string]any{"by": sc.label, "documents": docs, "note": strings.TrimSpace(in.Note), "source": "respondent dashboard"})
		s.logActivity(r.Context(), tenant, caseID, "PLAN_RESPONSE_RECEIVED",
			fmt.Sprintf("Response submitted by %s on their dashboard (%d document(s)) — %s", sc.label, docs, string(detail)))
		s.logAudit(r.Context(), tenant, caseID, "RESPONDENT_RESPONDED", map[string]any{"by": sc.subject, "as": sc.label, "documents": docs, "workflow_signalled": signalled})
		for _, role := range []string{"CASE_MANAGER", "ATTORNEY"} {
			s.notify(r, tenant, "role:"+role, "RESPONDENT_RESPONDED",
				fmt.Sprintf("%s responded to %s (%d document(s))", sc.label, caseNumber, docs), "#/cases/"+caseID)
		}
		s.tellFiler(r.Context(), tenant, caseID, "RESPONDENT_RESPONDED",
			fmt.Sprintf("%s responded to %s (%d document(s))", sc.label, caseNumber, docs))
		writeJSON(w, http.StatusOK, map[string]any{"status": "SUBMITTED", "workflow_signalled": signalled})
	}
}

// respondingOptOut: POST /hp/responding/{caseId}/opt-out {"reason": "..."} --
// a plan asks to opt out; the Attorney decides (recordOptOut).
func (s *server) respondingOptOut(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	sc, err := s.respondScopeFor(r, tenant, "plan")
	caseID := chi.URLParam(r, "caseId")
	if err != nil || !s.respondentOwns(r, tenant, caseID, sc) {
		http.Error(w, `{"error":"no such dispute against you"}`, http.StatusNotFound)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
	if strings.TrimSpace(in.Reason) == "" {
		http.Error(w, `{"error":"say why the plan asks to opt out"}`, http.StatusBadRequest)
		return
	}
	tag, _ := s.db.Exec(r.Context(), `
		UPDATE public.case_respondents SET response_status='OPT_OUT_REQUESTED', response_note=$3, responded_at=now(), responded_by=$4
		WHERE tenant=$1 AND case_id=$2 AND response_status='AWAITING'`, tenant, caseID, strings.TrimSpace(in.Reason), sc.label)
	if tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"this dispute already has your response"}`, http.StatusConflict)
		return
	}
	var caseNumber string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
	s.logActivity(r.Context(), tenant, caseID, "PLAN_OPT_OUT_REQUESTED", fmt.Sprintf("%s asked to opt out: %s", sc.label, strings.TrimSpace(in.Reason)))
	s.notify(r, tenant, "role:ATTORNEY", "PLAN_OPT_OUT_REQUESTED",
		fmt.Sprintf("%s asks to opt out of %s — decide it on the case", sc.label, caseNumber), "#/cases/"+caseID)
	s.tellFiler(r.Context(), tenant, caseID, "PLAN_OPT_OUT_REQUESTED",
		fmt.Sprintf("%s asked to opt out of %s; the office decides", sc.label, caseNumber))
	writeJSON(w, http.StatusOK, map[string]any{"status": "OPT_OUT_REQUESTED"})
}

// fillResponseDue sets each row's response deadline. The date the program
// office recorded (date_health_plan_response_due) wins; until one is, the
// tenant's PLAN_RESPONSE clock (its days and day type, from the program
// config) runs from when the other side was told — the recorded
// plan_notified_at, else when the platform notified or emailed it. The
// deadline is the end of the due day in the program's time zone, so the
// portal can count down to it.
//
//	response_due      YYYY-MM-DD
//	response_due_at   RFC 3339, end of that day
//	response_clock    {label, days, day_type, started, source: recorded|clock}
func (s *server) fillResponseDue(r *http.Request, tenant string, rows []map[string]any) {
	rule := ClockRule{Name: "PLAN_RESPONSE", Label: "Health Plan response", Days: 15, DayType: "calendar"}
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		for _, c := range cfg.Clocks {
			if c.Name == "PLAN_RESPONSE" && c.Days > 0 {
				rule = c
			}
		}
	}
	loc := programZone(tenant)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
	for _, row := range rows {
		due, source, started := str(row, "response_due"), "recorded", ""
		if len(due) < 10 {
			due, source = "", "clock"
			for _, k := range []string{"notified_at", "told_at"} {
				if v := str(row, k); len(v) >= 10 {
					started = v[:10]
					break
				}
			}
			if started != "" {
				if basis, err := time.Parse("2006-01-02", started); err == nil {
					if rule.DayType == "business" {
						due = addBusinessDays(basis, rule.Days).Format("2006-01-02")
					} else {
						due = basis.AddDate(0, 0, rule.Days).Format("2006-01-02")
					}
				}
			}
		}
		row["response_due"] = due
		row["response_due_at"] = ""
		if d, err := time.ParseInLocation("2006-01-02", due[:min(len(due), 10)], loc); err == nil {
			row["response_due_at"] = d.Add(24*time.Hour - time.Second).Format(time.RFC3339)
		}
		row["response_clock"] = map[string]any{"label": rule.Label, "days": rule.Days, "day_type": rule.DayType, "started": started, "source": source}
		delete(row, "told_at")
	}
}

// programZone: the time zone a program's day ends in (Florida's AHCA program
// runs on Eastern time).
func programZone(tenant string) *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.FixedZone("ET", -5*3600)
}
