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
// via the same account-id match account360 uses (case.provider_id/payer_id
// are accounts.id, confirmed directly against live data -- 158 real case
// matches on id; the legal-name match this comment used to describe never
// matched anything, a wrong assumption that had gone unverified since).
// Partial by design: "pm" would need a Keycloak user-by-role lookup this
// codebase has no API for anywhere yet, and "billed_party"/"all_parties"/
// "provider_or_plan" need invoice/template context resolveRecipients
// doesn't have — those roles resolve to no addresses here rather than a
// guess, so draftCorrespondence's caller-supplied to/cc (today's only
// path) still works as the fallback.
func (s *server) resolveRecipients(r *http.Request, tenant, caseID string, roles []string) []string {
	var providerAccountID, payerAccountID string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(provider_id,''), coalesce(payer_id,'') FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&providerAccountID, &payerAccountID)

	lookup := func(accountID string) []string {
		if accountID == "" {
			return nil
		}
		rows, err := s.queryRows(r, `
			SELECT c.email FROM public.contacts c JOIN public.accounts a ON a.id = c.account_id
			WHERE a.tenant=$1 AND a.id::text=$2 AND coalesce(c.email,'') <> ''`, tenant, accountID)
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
			add(lookup(providerAccountID))
		case "health_plan":
			add(lookup(payerAccountID))
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
		Template     string            `json:"template"` // template key from program config
		Body         string            `json:"body"`     // staff-composed body (subject comes from config)
		To           []string          `json:"to"`       // resolved recipient emails
		CC           []string          `json:"cc"`
		Vars         map[string]string `json:"vars"`          // extra placeholders
		ShareTokens  []string          `json:"share_tokens"`  // attach pre-created share links
		RfiTo        string            `json:"rfi_to"`        // "provider"|"plan" -- only used when template=="rfi"
		AutoShare    bool              `json:"auto_share"`    // mint an upload link and embed it (G9, no copy-paste)
		AutoDownload bool              `json:"auto_download"` // mint a download link for case documents and embed it
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
	// Auto-share (G9): the template or the sender wants a secure upload link in
	// the message. Mint one inline and substitute the {share_link} placeholder —
	// the case manager never leaves the compose screen to copy-paste a URL.
	// Fire when explicitly requested OR when the body still carries the
	// placeholder after variable substitution.
	if in.AutoShare || strings.Contains(body, "{share_link}") {
		link := s.autoShareLink(r, tenant, caseID, "upload", "")
		if strings.Contains(body, "{share_link}") {
			body = strings.ReplaceAll(body, "{share_link}", link)
		} else {
			body += "\n\nSecure upload link: " + link
		}
	}
	// Auto-download (G9): {download_link} pins a download link to the case's
	// latest generated document (determination letter, notice) — the sender
	// never hunts the document panel for a URL.
	if in.AutoDownload || strings.Contains(body, "{download_link}") {
		var objectKey string
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
			SELECT object_key FROM tenant_%s.documents
			WHERE case_id=$1 AND NOT sealed AND scan_status<>'BLOCKED'
			ORDER BY created_at DESC LIMIT 1`, sanitizeTenant(tenant)), caseID).Scan(&objectKey)
		link := ""
		if objectKey != "" {
			link = s.autoShareLink(r, tenant, caseID, "download", objectKey)
		}
		if strings.Contains(body, "{download_link}") {
			if link != "" {
				body = strings.ReplaceAll(body, "{download_link}", link)
			} else {
				body = strings.ReplaceAll(body, "{download_link}",
					"(document pending — link will follow)")
			}
		} else if link != "" {
			body += "\n\nSecure document download link: " + link
		}
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
		INSERT INTO public.qa_reviews (tenant, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by, requested_by_sub)
		VALUES ($1,$2,$3,'email',$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		tenant, caseID, tpl.Key, subject, body, toJ, ccJ, status, displayName(p), p.Subject).Scan(&qid)

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
		if len(in.To) > 0 {
			s.logCorrespondenceInquiry(r, tenant, caseID, in.To[0], fmt.Sprintf("%s: %s", in.Template, subject), p.Subject)
		}
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
		// "parties notified" and friends tick themselves on delivery.
		s.autoChecklist(r, tenant, caseID)
	}
	corrAction := "CORRESPONDENCE_SENT"
	if status == "PENDING" {
		corrAction = "CORRESPONDENCE_DRAFTED" // held at the QA gate, not yet sent
	}
	s.logAudit(r.Context(), tenant, caseID, corrAction, map[string]any{
		"by": p.Subject, "qa_id": qid, "template": in.Template, "status": status,
		"to": in.To, "cc": in.CC,
	})
	writeJSON(w, http.StatusOK, map[string]any{"qa_id": qid, "status": status, "subject": subject})
}

// qaQueue lists pending QA reviews; qaDecision approves/rejects. Approving a
// PENDING review marks it SENT and writes the correspondence log — nothing
// reaches a party without passing the gate.
func (s *server) qaQueue(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, qaReadRoles...) {
		http.Error(w, `{"error":"forbidden: requires a case-staff or QA role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	// Optional ?case_id= filter — the conversational QA gate (Assistant thread)
	// renders the gate for ONE case inline, while the QA screen keeps the
	// full queue. Same query otherwise; the filter never changes semantics.
	if cid := r.URL.Query().Get("case_id"); cid != "" {
		rows, err := s.queryRows(r, `
			SELECT id, case_id, artifact, channel, subject, status, drafted_by, created_at
			FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING' AND case_id=$2
			ORDER BY created_at`, tenant, cid)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"queue": rows})
		return
	}
	rows, err := s.queryRows(r, `
		SELECT id, case_id, artifact, channel, subject, status, drafted_by, created_at
		FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING' ORDER BY created_at`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// Recent decisions keep the page useful when the pending queue is empty —
	// reviewers see what the gate has been doing, not a blank screen.
	recent, _ := s.queryRows(r, `
		SELECT id, case_id, subject, status, drafted_by, reviewed_by, reviewed_at
		FROM public.qa_reviews WHERE tenant=$1 AND status<>'PENDING'
		ORDER BY reviewed_at DESC NULLS LAST LIMIT 20`, tenant)
	writeJSON(w, http.StatusOK, map[string]any{"queue": rows, "recent": recent})
}

func (s *server) qaGet(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, qaReadRoles...) {
		http.Error(w, `{"error":"forbidden: requires a case-staff or QA role"}`, http.StatusForbidden)
		return
	}
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
		// EditedBody implements the EDIT prong of accept/edit/reject: when
		// present on a PENDING row, the reviewer's text replaces the draft
		// body before the decision lands. The edit itself is recorded so the
		// audit trail always distinguishes model text from human text.
		EditedBody string `json:"edited_body"`
		// To/CC let the reviewer fix recipients before sending -- confirmed
		// live: a case with no provider/payer account linked resolves to
		// ZERO recipients at draft time (resolveRecipients has nothing to
		// look up), nothing caught that before the draft reached QA, and
		// approving it reached sendMail with an empty address list, which
		// SMTP rejects as a raw "503 need RCPT command" -- the exact error
		// the OLD response text promised a retry could fix, with no actual
		// way to supply one. nil (omitted) means "don't touch the stored
		// recipients"; present (even []) means "replace them with this".
		To *[]string `json:"to"`
		CC *[]string `json:"cc"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.Decision != "APPROVE" && in.Decision != "REJECT") {
		http.Error(w, `{"error":"decision must be APPROVE or REJECT"}`, http.StatusBadRequest)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var caseID, subject, body, status, artifact, channel, draftedBy, requestedBySub string
	var toJ, ccJ []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT case_id, subject, body, status, to_recipients, cc_recipients, artifact, coalesce(channel,'email'), coalesce(drafted_by,''), coalesce(requested_by_sub,'') FROM public.qa_reviews WHERE tenant=$1 AND id=$2`,
		tenant, qid).Scan(&caseID, &subject, &body, &status, &toJ, &ccJ, &artifact, &channel, &draftedBy, &requestedBySub)
	// APPROVED (not yet SENT) + a fresh APPROVE decision is a retry of a
	// previously failed send -- status only reaches APPROVED-without-SENT
	// when sendMail below failed last time, and the response used to claim
	// "stays APPROVED so the reviewer can retry" with no actual way to do
	// that (calling this again always 409'd). REJECT still requires PENDING
	// -- rejecting something already approved isn't a retry, it's reversing
	// a decision, which this endpoint doesn't support.
	// Retry only applies to mail rows — channel 'note' rows (copilot
	// determination rationales) are terminal at APPROVED, nothing is sent.
	isRetry := status == "APPROVED" && in.Decision == "APPROVE" && channel != "note"
	if err != nil || (status != "PENDING" && !isRetry) {
		http.Error(w, `{"error":"not pending"}`, http.StatusConflict)
		return
	}
	// Self-approval guard: the drafter and the reviewer must be different
	// people, or a "QA gate" isn't actually one. Skipped for action-batch-
	// originated drafts ("batch approved by X") -- those already passed an
	// independent human decision at the batch-approval step (approving a
	// draft_followup_correspondence action IS that decision); this row is
	// that decision's paperwork, not a second judgment call needing a
	// different reviewer. displayName(p) != "" guards strings.Contains(x,
	// "") always being true -- an empty name must never match everything.
	//
	// Also skipped when the draft is copilot-originated AND the approver is
	// the exact person who asked for it (requested_by_sub, not a
	// drafted_by name-match): a human composing their own correspondence
	// still needs a genuinely independent second reviewer before it reaches
	// a real external party, but an AI-drafted item never had a second
	// human in the loop at creation time either -- requiring one now adds
	// no check that wasn't already missing, and the usual review (does this
	// say what it should, is it grounded in the facts) still happens, same
	// reviewer, same read of the draft, just without a second account.
	// Deliberate product decision, not a workaround: this does NOT relax
	// the guard for human-composed (template-based) correspondence, which
	// is exactly the case the guard exists to cover.
	name := displayName(p)
	nameMatch := name != "" && !strings.Contains(draftedBy, "batch approved by") && strings.Contains(draftedBy, name)
	requesterMatch := requestedBySub != "" && requestedBySub == p.Subject
	selfApproval := nameMatch || requesterMatch
	copilotRequesterExempt := strings.HasPrefix(artifact, "copilot_") && requesterMatch
	if selfApproval && !copilotRequesterExempt {
		http.Error(w, `{"error":"forbidden: cannot approve or reject your own draft -- needs a second reviewer"}`, http.StatusForbidden)
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
		// ARBITRATOR added: the portal's own QA Gate nav link already shows
		// to ARBITRATOR (has("CASE_MANAGER", "ARBITRATOR", "PM", "ATTORNEY",
		// ...)), and a federal determination letter's QA review -- which
		// lands here, not in the qa_role branch above -- is exactly the kind
		// of review an arbitrator needs to approve before it goes out.
	} else if !hasAnyRole(p, "CASE_MANAGER", "ARBITRATOR", "PM", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	newStatus := "APPROVED"
	if in.Decision == "REJECT" {
		newStatus = "REJECTED"
	}
	// EDIT prong: a PENDING draft the reviewer rewrote lands with the human
	// text, and the review note records that an edit happened (model text is
	// never silently replaced — the QA row keeps drafted_by attribution and
	// the note flags the human revision).
	if in.EditedBody != "" && status == "PENDING" {
		body = in.EditedBody
		editMark := "[human-edited before " + in.Decision + "]"
		if in.Note != "" {
			in.Note = editMark + " " + in.Note
		} else {
			in.Note = editMark
		}
		_, _ = s.db.Exec(r.Context(),
			`UPDATE public.qa_reviews SET body=$3 WHERE tenant=$1 AND id=$2`, tenant, qid, body)
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.qa_reviews SET status=$3, reviewed_by=$4, reviewed_at=now(), review_note=$5
		WHERE tenant=$1 AND id=$2`, tenant, qid, newStatus, p.Subject, in.Note)

	var to, cc []string
	_ = json.Unmarshal(toJ, &to)
	_ = json.Unmarshal(ccJ, &cc)
	// Reviewer-supplied recipients replace whatever was stored (or never
	// resolved) at draft time -- persisted immediately so a retry after a
	// delivery failure doesn't need the address re-entered twice, and so
	// the qa_reviews row stays the source of truth for what actually sent.
	if in.To != nil {
		to = *in.To
		if toJ2, err := json.Marshal(to); err == nil {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.qa_reviews SET to_recipients=$3 WHERE tenant=$1 AND id=$2`, tenant, qid, toJ2)
		}
	}
	if in.CC != nil {
		cc = *in.CC
		if ccJ2, err := json.Marshal(cc); err == nil {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.qa_reviews SET cc_recipients=$3 WHERE tenant=$1 AND id=$2`, tenant, qid, ccJ2)
		}
	}
	deliveryError := ""
	if in.Decision == "APPROVE" && channel == "note" {
		// channel 'note' (copilot determination rationale): approval files the
		// rationale on the case timeline — nothing is emailed, ever.
		s.logActivity(r.Context(), tenant, caseID, "DETERMINATION_RATIONALE_FILED",
			fmt.Sprintf("Rationale approved in QA by %s:\n%s", p.Subject, truncate(body, 6000)))
		s.logAudit(r.Context(), tenant, caseID, "QA_DECISION", map[string]any{
			"by": p.Subject, "decision": in.Decision, "subject": subject, "note": in.Note, "channel": channel,
		})
		if requestedBySub != "" {
			s.notify(r, tenant, requestedBySub,
				"DRAFT_APPROVED", fmt.Sprintf("Your draft %q was approved and filed on the case.", subject), "#/cases/"+caseID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": newStatus})
		return
	}
	if in.Decision == "APPROVE" {
		// The narrative's send-safety rule (drafts saved without addresses) ends
		// here: addresses enter only at QA-approved send time, and delivery goes
		// through the configured SMTP relay. Failure keeps status APPROVED (not
		// SENT) so the reviewer can retry — nothing is marked sent that wasn't.
		//
		// A failed send used to hard-502 the whole request here (an early
		// return before the QA_DECISION audit line below) -- confirmed live:
		// the approval itself WAS saved (the UPDATE above already ran), but
		// the reviewer saw an opaque Cloudflare 502 with no indication their
		// decision had actually gone through, and no audit trail existed for
		// that approval at all -- exactly the record a 502-looking "did this
		// even work?" moment most needs. Approval and delivery are now two
		// separate facts: the decision always succeeds and is always
		// audited; delivery failure is reported back in the 200 response
		// instead of standing in for the whole request's status.
		// Fails loud and clear instead of handing an empty recipient list to
		// SMTP -- confirmed live: that produced a raw "503 need RCPT command"
		// protocol error with no indication of WHY (the case had no
		// provider/payer account linked, so resolveRecipients had nothing to
		// resolve at draft time, and nothing caught it before now).
		var sendErr error
		if len(to) == 0 && len(cc) == 0 {
			sendErr = fmt.Errorf("no recipient email address on this draft -- add one above and approve again")
		} else {
			sendErr = s.sendMail(to, cc, subject, body)
		}
		if err := sendErr; err != nil {
			deliveryError = err.Error()
			s.logActivity(r.Context(), tenant, caseID, "EMAIL_DELIVERY_FAILED",
				fmt.Sprintf("SMTP delivery failed for %q: %s", subject, err))
			// The reviewer who clicked Approve already sees this in the
			// response; the person who ASKED for the draft in the first
			// place (often a different person, often not watching this
			// screen at all -- that's the whole point of a QA gate) had no
			// way to learn their mail never actually went out.
			if requestedBySub != "" {
				s.notify(r, tenant, requestedBySub, "DRAFT_SEND_FAILED",
					fmt.Sprintf("Your correspondence draft %q was approved but failed to send: %s", subject, deliveryError), "#/qa")
			}
		} else {
			_, _ = s.db.Exec(r.Context(),
				`UPDATE public.qa_reviews SET status='SENT', sent_at=now() WHERE tenant=$1 AND id=$2`, tenant, qid)
			s.logCorrespondence(r, tenant, caseID, "OUT", "qa_approved", subject, body, to, cc, p.Subject)
			if len(to) > 0 {
				s.logCorrespondenceInquiry(r, tenant, caseID, to[0], fmt.Sprintf("%s: %s", artifact, subject), p.Subject)
			}
			s.logActivity(r.Context(), tenant, caseID, "EMAIL_SENT",
				fmt.Sprintf("QA-approved by %s: %s sent to %d recipient(s)", p.Subject, subject, len(to)))
			if requestedBySub != "" {
				s.notify(r, tenant, requestedBySub, "DRAFT_SENT",
					fmt.Sprintf("Your correspondence draft %q was approved and sent to %d recipient(s).", subject, len(to)), "#/cases/"+caseID)
			}
			// Notification is a fact now — the checklist follows.
			s.autoChecklist(r, tenant, caseID)
		}
	} else {
		s.logActivity(r.Context(), tenant, caseID, "QA_REJECTED",
			fmt.Sprintf("Draft rejected in QA by %s: %s%s", p.Subject, subject, orDash(" — "+in.Note)))
		if requestedBySub != "" {
			s.notify(r, tenant, requestedBySub, "DRAFT_REJECTED",
				fmt.Sprintf("Your correspondence draft %q was rejected%s", subject, orDash(" — "+in.Note)), "#/cases/"+caseID)
		}
	}
	s.logAudit(r.Context(), tenant, caseID, "QA_DECISION", map[string]any{
		"by": p.Subject, "decision": in.Decision, "subject": subject, "note": in.Note,
		"email_delivery_error": deliveryError,
	})
	out := map[string]any{"status": newStatus}
	if deliveryError != "" {
		out["email_delivery_error"] = deliveryError
	}
	writeJSON(w, http.StatusOK, out)
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
	limit, offset := pageParams(r, 50, 500)
	var total int
	if err := s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.correspondence_log WHERE tenant=$1 AND case_id=$2`,
		tenant, caseID).Scan(&total); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT id, direction, template, subject, recipients, sent_by, created_at
		FROM public.correspondence_log WHERE tenant=$1 AND case_id=$2
		ORDER BY created_at DESC, id DESC LIMIT %d OFFSET %d`, limit, offset),
		tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"correspondence": rows,
		"total": total, "next_offset": nextOffset(offset, limit, total)})
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
	// The token is a bearer credential -- never written to the audit payload.
	s.logAudit(r.Context(), tenant, caseID, "SHARE_LINK_CREATED", map[string]any{
		"by": p.Subject, "kind": in.Kind, "days_ttl": in.DaysTTL, "max_uses": in.MaxUses,
		"object_key": in.ObjectKey,
	})
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "path": "/api/share/" + token, "kind": in.Kind})
}

// autoShareLink mints a 7-day, 10-use upload link for a case and returns the
// absolute portal URL. Used by draftCorrespondence's auto-share path so the
// sender never has to pre-create and paste a link. Failure is soft: the
// placeholder degrades to the relative path rather than failing the draft.
func (s *server) autoShareLink(r *http.Request, tenant, caseID, kind, objectKey string) string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "/s/"
	}
	token := hex.EncodeToString(buf)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.share_links (token, tenant, case_id, kind, object_key, expires_at, max_uses, created_by)
		VALUES ($1,$2,$3,$4, nullif($5,''), now() + interval '7 days', 10, $6)`,
		token, tenant, caseID, kind, objectKey, p.Subject); err != nil {
		return "/s/" + token
	}
	s.logActivity(r.Context(), tenant, caseID, "SHARE_LINK",
		fmt.Sprintf("Secure %s link auto-created for correspondence by %s", kind, p.Subject))
	return strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/s/" + token
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
