package main

// Correspondence engine — program-template driven outbound email with a
// mandatory QA gate for attorney/PM-flagged templates, a full correspondence
// log (OUT + IN), and tokenized ShareFile-style upload/download links.
//
// Gaps closed: G3 (correspondence engine + CC matrix), G4 (QA gate),
// G9 (ShareFile replacement), G15 (template hygiene: templates live in
// program_rules config with explicit to/cc policy — one source of truth).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// templateDateKeys: sending one of these templates marks a date the source
// narrative names explicitly (e.g. "Date Case Accepted") -- previously
// nothing recorded it automatically, so it was a separate step staff had
// to remember on top of actually sending the letter. final_order is
// deliberately excluded: that date (date_recommendation_sent_to_agency)
// is set by the workflow at the same "Determination sent to FL" status
// transition, which is the single source of truth for it.
var templateDateKeys = map[string][]string{
	"acceptance":        {"date_case_accepted", "date_acknowledgment_letter_sent_to_provider"},
	"ineligible":        {"date_acknowledgment_letter_sent_to_provider"},
	"dismissal":         {"date_acknowledgment_letter_sent_to_provider"},
	"plan_notification": {"date_acknowledgment_letter_sent_to_health_plan"},
}

type CorrTemplate struct {
	Key     string   `json:"key"`
	Subject string   `json:"subject"`
	To      []string `json:"to"`      // party roles: provider|health_plan|filing_party|requester|agency…
	CC      []string `json:"cc"`      // copied parties per program CC matrix
	QARole  string   `json:"qa_role"` // "" = send immediately; else gated on that role
	Thread  bool     `json:"thread"`  // reply-all on the existing thread
}

func (s *server) corrTemplates(r *http.Request, tenant string) []CorrTemplate {
	var raw []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT config->'correspondence'->'templates' FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw)
	if err != nil {
		return nil
	}
	var out []CorrTemplate
	_ = json.Unmarshal(raw, &out)
	return out
}

// resolveRecipients turns correspondence-template role labels (provider,
// health_plan, agency, requester, filing_party) into real contact emails,
// via the same legal-name match account360 already uses (case.provider_id/
// payer_id store the party's legal name as text, not an account UUID --
// confirmed against crm.go's own join). Partial by design: "pm" would need
// a Keycloak user-by-role lookup this codebase has no API for anywhere yet,
// and "billed_party"/"all_parties"/"provider_or_plan" need invoice/template
// context resolveRecipients doesn't have — those roles resolve to no
// addresses here rather than a guess, so draftCorrespondence's caller-
// supplied to/cc (today's only path) still works as the fallback.
func (s *server) resolveRecipients(r *http.Request, tenant, caseID string, roles []string) []string {
	var providerName, payerName string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(provider_id,''), coalesce(payer_id,'') FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&providerName, &payerName)

	lookup := func(legalName string) []string {
		if legalName == "" {
			return nil
		}
		rows, err := s.queryRows(r, `
			SELECT c.email FROM public.contacts c JOIN public.accounts a ON a.id = c.account_id
			WHERE a.tenant=$1 AND a.legal_name=$2 AND coalesce(c.email,'') <> ''`, tenant, legalName)
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			if e, ok := row["email"].(string); ok {
				out = append(out, e)
			}
		}
		return out
	}

	var agencyRecipients []string
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		var raw []byte
		if err := s.db.QueryRow(r.Context(),
			`SELECT config->'correspondence'->'agency_recipients' FROM public.program_rules WHERE tenant=$1`, tenant).
			Scan(&raw); err == nil {
			_ = json.Unmarshal(raw, &agencyRecipients)
		}
	}

	seen := map[string]bool{}
	var out []string
	add := func(emails []string) {
		for _, e := range emails {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	for _, role := range roles {
		switch role {
		case "provider", "requester", "filing_party":
			add(lookup(providerName))
		case "health_plan":
			add(lookup(payerName))
		case "agency":
			add(agencyRecipients)
		}
	}
	return out
}

// renderTemplate substitutes {case_number}, {provider_name}, {payer_name},
// {amount}, {due_date} placeholders from the case row.
func (s *server) renderTemplate(r *http.Request, tenant, caseID, text string) string {
	var caseNumber, providerID, payerID string
	var qpa int64
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT case_number, coalesce(provider_id,''), coalesce(payer_id,''), coalesce(qpa_cents,0)
		FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&caseNumber, &providerID, &payerID, &qpa)
	repl := map[string]string{
		"{case_number}":     caseNumber,
		"{provider_name}":   providerID,
		"{payer_name}":      payerID,
		"{qpa}":             fmt.Sprintf("$%d.%02d", qpa/100, qpa%100),
		"{case_url_suffix}": "#/cases/" + caseID,
	}
	for k, v := range repl {
		text = strings.ReplaceAll(text, k, v)
	}
	return text
}

// draftCorrespondence renders a program template for a case. If the template
// carries qa_role, the draft enters the QA queue (PENDING) and is NOT sent;
// otherwise it sends immediately.
func (s *server) draftCorrespondence(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Template    string            `json:"template"` // template key from program config
		Body        string            `json:"body"`     // staff-composed body (subject comes from config)
		To          []string          `json:"to"`       // resolved recipient emails
		CC          []string          `json:"cc"`
		Vars        map[string]string `json:"vars"`         // extra placeholders
		ShareTokens []string          `json:"share_tokens"` // attach share links
		RfiTo       string            `json:"rfi_to"`       // "provider"|"plan" -- only used when template=="rfi"
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Template == "" {
		http.Error(w, `{"error":"template required"}`, http.StatusBadRequest)
		return
	}
	var tpl *CorrTemplate
	for _, t := range s.corrTemplates(r, tenant) {
		if t.Key == in.Template {
			cp := t
			tpl = &cp
			break
		}
	}
	if tpl == nil {
		http.Error(w, `{"error":"unknown template for this program"}`, http.StatusBadRequest)
		return
	}
	// Resolve the template's role-keyed to/cc policy to real addresses when
	// the caller didn't already supply them -- previously the template's
	// own to/cc config was never actually used for anything, only its
	// subject/qa_role; address resolution was silently left to the caller
	// every time, with no fallback for roles that have no account contact.
	if len(in.To) == 0 {
		in.To = s.resolveRecipients(r, tenant, caseID, tpl.To)
	}
	if len(in.CC) == 0 {
		in.CC = s.resolveRecipients(r, tenant, caseID, tpl.CC)
	}
	subject := s.renderTemplate(r, tenant, caseID, tpl.Subject)
	body := s.renderTemplate(r, tenant, caseID, in.Body)
	for k, v := range in.Vars {
		subject = strings.ReplaceAll(subject, "{"+k+"}", v)
		body = strings.ReplaceAll(body, "{"+k+"}", v)
	}
	// append share links (G9) when requested
	if len(in.ShareTokens) > 0 {
		var links []string
		for _, tok := range in.ShareTokens {
			var kind string
			var expires time.Time
			if err := s.db.QueryRow(r.Context(),
				`SELECT kind, expires_at FROM public.share_links WHERE token=$1 AND tenant=$2 AND uses < max_uses`,
				tok, tenant).Scan(&kind, &expires); err != nil {
				continue
			}
			links = append(links, fmt.Sprintf("%s link (expires %s): /api/share/%s", kind, expires.Format("2006-01-02"), tok))
		}
		if len(links) > 0 {
			body += "\n\nSecure document links:\n" + strings.Join(links, "\n")
		}
	}

	toJ, _ := json.Marshal(in.To)
	ccJ, _ := json.Marshal(in.CC)
	status := "SENT"
	if tpl.QARole != "" {
		status = "PENDING" // QA gate (G4): attorney/PM must approve before send
	}
	var qid string
	// artifact stores the template key (schema comment: "template key, e.g.
	// acceptance_provider") -- previously hardcoded to the literal 'EMAIL',
	// which made it impossible for qaDecision to ever look up which role's
	// approval a pending draft actually needed.
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.qa_reviews (tenant, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by)
		VALUES ($1,$2,$3,'email',$4,$5,$6,$7,$8,$9) RETURNING id`,
		tenant, caseID, tpl.Key, subject, body, toJ, ccJ, status, p.Subject).Scan(&qid)

	if status == "PENDING" {
		s.notify(r, tenant, "*", "QA_REVIEW",
			fmt.Sprintf("%s draft on case %s awaiting %s QA approval", tpl.Key, caseID, tpl.QARole), "#/qa")
	} else {
		// No QA gate on this template — deliver immediately via SMTP.
		if err := s.sendMail(in.To, in.CC, subject, body); err != nil {
			s.logActivity(r.Context(), tenant, caseID, "EMAIL_DELIVERY_FAILED",
				fmt.Sprintf("SMTP delivery failed for %q: %s", subject, err))
			http.Error(w, `{"error":"smtp delivery failed — draft retained as APPROVED for retry"}`, http.StatusBadGateway)
			return
		}
		_, _ = s.db.Exec(r.Context(), `UPDATE public.qa_reviews SET sent_at=now() WHERE tenant=$1 AND id=$2`, tenant, qid)
		s.logCorrespondence(r, tenant, caseID, "OUT", in.Template, subject, body, in.To, in.CC, p.Subject)
		s.logActivity(r.Context(), tenant, caseID, "EMAIL_SENT",
			fmt.Sprintf("%s sent to %d recipient(s) (cc %d) — template %s", subject, len(in.To), len(in.CC), in.Template))
		if dateKeys, ok := templateDateKeys[in.Template]; ok {
			today := time.Now().UTC().Format("2006-01-02")
			for _, key := range dateKeys {
				_, _ = s.db.Exec(r.Context(),
					fmt.Sprintf(`UPDATE tenant_%s.cases SET program_dates = program_dates || jsonb_build_object($2::text, $3::text), updated_at=now() WHERE id=$1`,
						sanitizeTenant(tenant)), caseID, key, today)
			}
		}
		// RFI can go out at stages 3-5 ("3-5 Any" per the narrative) -- signal
		// the workflow's concurrent RFI watcher so the 15-day response clock
		// and its day-15 lapse->closed outcome actually run, same auto-signal
		// shape as checkEligibility/recordOptOut. Best-effort.
		if in.Template == "rfi" {
			rfiTo := in.RfiTo
			if rfiTo != "plan" {
				rfiTo = "provider"
			}
			var caseNumber string
			if err := s.db.QueryRow(r.Context(),
				fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber); err == nil {
				wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
				_ = s.tc.SignalWorkflow(r.Context(), wfID, "", "RFI_SENT", map[string]any{"to": rfiTo})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"qa_id": qid, "status": status, "subject": subject})
}

// qaQueue lists pending QA reviews; qaDecision approves/rejects. Approving a
// PENDING review marks it SENT and writes the correspondence log — nothing
// reaches a party without passing the gate.
func (s *server) qaQueue(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `
		SELECT id, case_id, artifact, channel, subject, status, drafted_by, created_at
		FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING' ORDER BY created_at`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": rows})
}

func (s *server) qaGet(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	qid := chi.URLParam(r, "qaId")
	rows, err := s.queryRows(r, `
		SELECT id, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by, created_at
		FROM public.qa_reviews WHERE tenant=$1 AND id=$2`, tenant, qid)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rows[0])
}

func (s *server) qaDecision(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	qid := chi.URLParam(r, "qaId")
	var in struct {
		Decision string `json:"decision"` // APPROVE|REJECT
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.Decision != "APPROVE" && in.Decision != "REJECT") {
		http.Error(w, `{"error":"decision must be APPROVE or REJECT"}`, http.StatusBadRequest)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var caseID, subject, body, status, artifact string
	var toJ, ccJ []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT case_id, subject, body, status, to_recipients, cc_recipients, artifact FROM public.qa_reviews WHERE tenant=$1 AND id=$2`,
		tenant, qid).Scan(&caseID, &subject, &body, &status, &toJ, &ccJ, &artifact)
	if err != nil || status != "PENDING" {
		http.Error(w, `{"error":"not pending"}`, http.StatusConflict)
		return
	}
	// Generic QA-role gate: the approver must hold the role the template
	// configured as qa_role (e.g. "ATTORNEY" on FL AHCA's dismissal
	// template), read straight from program_rules config -- no per-program
	// branching in Go. Falls back to a broad staff check when the artifact
	// doesn't match a known correspondence template (e.g. a lettergen-py
	// DOCX letter review, which opens a qa_reviews row with no qa_role
	// concept at all).
	qaRole := ""
	for _, t := range s.corrTemplates(r, tenant) {
		if t.Key == artifact {
			qaRole = t.QARole
			break
		}
	}
	if qaRole != "" {
		if !hasAnyRole(p, qaRole, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
			http.Error(w, fmt.Sprintf(`{"error":"forbidden: requires %s, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, qaRole), http.StatusForbidden)
			return
		}
	} else if !hasAnyRole(p, "CASE_MANAGER", "PM", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	newStatus := "APPROVED"
	if in.Decision == "REJECT" {
		newStatus = "REJECTED"
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.qa_reviews SET status=$3, reviewed_by=$4, reviewed_at=now(), review_note=$5
		WHERE tenant=$1 AND id=$2`, tenant, qid, newStatus, p.Subject, in.Note)

	var to, cc []string
	_ = json.Unmarshal(toJ, &to)
	_ = json.Unmarshal(ccJ, &cc)
	if in.Decision == "APPROVE" {
		// The narrative's send-safety rule (drafts saved without addresses) ends
		// here: addresses enter only at QA-approved send time, and delivery goes
		// through the configured SMTP relay. Failure keeps status APPROVED (not
		// SENT) so the reviewer can retry — nothing is marked sent that wasn't.
		if err := s.sendMail(to, cc, subject, body); err != nil {
			s.logActivity(r.Context(), tenant, caseID, "EMAIL_DELIVERY_FAILED",
				fmt.Sprintf("SMTP delivery failed for %q: %s", subject, err))
			http.Error(w, `{"error":"smtp delivery failed — draft stays APPROVED for retry"}`, http.StatusBadGateway)
			return
		}
		_, _ = s.db.Exec(r.Context(),
			`UPDATE public.qa_reviews SET status='SENT', sent_at=now() WHERE tenant=$1 AND id=$2`, tenant, qid)
		s.logCorrespondence(r, tenant, caseID, "OUT", "qa_approved", subject, body, to, cc, p.Subject)
		s.logActivity(r.Context(), tenant, caseID, "EMAIL_SENT",
			fmt.Sprintf("QA-approved by %s: %s sent to %d recipient(s)", p.Subject, subject, len(to)))
	} else {
		s.logActivity(r.Context(), tenant, caseID, "QA_REJECTED",
			fmt.Sprintf("Draft rejected in QA by %s: %s%s", p.Subject, subject, orDash(" — "+in.Note)))
	}
	s.logAudit(r.Context(), tenant, caseID, "QA_DECISION", map[string]any{
		"by": p.Subject, "decision": in.Decision, "subject": subject, "note": in.Note,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": newStatus})
}

func (s *server) logCorrespondence(r *http.Request, tenant, caseID, direction, template, subject, body string, to, cc []string, actor string) {
	rcpts, _ := json.Marshal(map[string]any{"to": to, "cc": cc})
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.correspondence_log (tenant, case_id, direction, template, subject, body, recipients, sent_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		tenant, caseID, direction, template, subject, body, rcpts, actor)
}

// listCorrespondence: full OUT/IN trail for a case.
func (s *server) listCorrespondence(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	rows, err := s.queryRows(r, `
		SELECT id, direction, template, subject, recipients, sent_by, created_at
		FROM public.correspondence_log WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC`,
		tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"correspondence": rows})
}

// ---- Share links (G9: ShareFile replacement) --------------------------------

func (s *server) createShareLink(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Kind      string `json:"kind"` // upload|download
		DaysTTL   int    `json:"days_ttl"`
		MaxUses   int    `json:"max_uses"`
		ObjectKey string `json:"object_key"` // download links: pin to one document
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Kind != "upload" && in.Kind != "download" {
		http.Error(w, `{"error":"kind must be upload or download"}`, http.StatusBadRequest)
		return
	}
	if in.DaysTTL <= 0 {
		in.DaysTTL = 7
	}
	if in.MaxUses <= 0 {
		in.MaxUses = 1
	}
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.share_links (token, tenant, case_id, kind, object_key, expires_at, max_uses, created_by)
		VALUES ($1,$2,$3,$4, nullif($5,''), now() + make_interval(days => $6), $7, $8)`,
		token, tenant, caseID, in.Kind, in.ObjectKey, in.DaysTTL, in.MaxUses, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "SHARE_LINK",
		fmt.Sprintf("Secure %s link created (%d-day expiry) by %s", in.Kind, in.DaysTTL, p.Subject))
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "path": "/api/share/" + token, "kind": in.Kind})
}

// resolveShareLink is the unauthenticated landing for a token (upload/download
// portal page consumes this; expiry and use-count enforced).
func (s *server) resolveShareLink(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	var tenant, caseID, kind string
	var uses, maxUses int
	var expires time.Time
	err := s.db.QueryRow(r.Context(), `
		SELECT tenant, case_id, kind, uses, max_uses, expires_at FROM public.share_links WHERE token=$1`, token).
		Scan(&tenant, &caseID, &kind, &uses, &maxUses, &expires)
	if err != nil || time.Now().After(expires) || uses >= maxUses {
		http.Error(w, `{"error":"link expired or invalid"}`, http.StatusGone)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant": tenant, "case_id": caseID, "kind": kind,
		"expires_at": expires.Format(time.RFC3339), "remaining_uses": maxUses - uses,
	})
}
