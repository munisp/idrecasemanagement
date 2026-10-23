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
	"errors"
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
	"ineligible":        {"date_acknowledgment_letter_sent_to_provider", "date_ineligibility_letter_sent"},
	"dismissal":         {"date_acknowledgment_letter_sent_to_provider", "date_dismissal_letter_sent"},
	"plan_notification": {"date_acknowledgment_letter_sent_to_health_plan"},
	// The closing letters: each stops the clock that timed it.
	"plan_opt_out": {"date_opt_out_letter_sent"},
	"withdrawal":   {"date_withdrawal_letter_sent"},
	// Clock bases: each clock starts when its email actually goes out.
	"estimate_cost": {"estimate_sent_at"},
	"rfi":           {"rfi_sent_at"},
}

type CorrTemplate struct {
	Key     string   `json:"key"`
	Subject string   `json:"subject"`
	To      []string `json:"to"`      // party roles: provider|health_plan|filing_party|requester|agency…
	CC      []string `json:"cc"`      // copied parties per program CC matrix
	QARole  string   `json:"qa_role"` // "" = send immediately; else gated on that role
	// QAPreRole: a role that must check the draft BEFORE the QA role's
	// approval sends it (FL dismissal: the PM, then the Attorney).
	QAPreRole string `json:"qa_pre_role"`
	Thread    bool   `json:"thread"` // reply-all on the existing thread
	// Body is the program's standard message text (FL: verbatim from the
	// "Email Templates FL AHCA" source document) with {case_number},
	// {share_link} and {download_link} placeholders. The compose screen
	// pre-fills it; staff edit before sending, exactly as they used to fill
	// the Word templates. The caller's body still wins on send.
	Body string `json:"body"`

	// Set for what the Templates screen manages (templates_admin.go); a
	// program's own config carries none of these.
	Name        string `json:"name,omitempty"`     // the name people see; the key when empty
	Inactive    bool   `json:"inactive,omitempty"` // retired: kept, no longer offered
	Custom      bool   `json:"custom,omitempty"`   // added from the dashboard, not a standard template
	Edited      bool   `json:"edited,omitempty"`   // a standard template whose wording was changed
	UpdatedBy   string `json:"updated_by,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	ContentFrom string `json:"content_from,omitempty"` // the template whose content a usage rule put here
}

// corrTemplates is every template of the tenant: the program's standard
// ones with any dashboard edits applied, then the ones people added.
func (s *server) corrTemplates(r *http.Request, tenant string) []CorrTemplate {
	base := s.baseCorrTemplates(r, tenant)
	if base == nil {
		return nil
	}
	return overlayTemplates(base, s.templateRows(r, tenant))
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
	// The address that asked to file stands in for a provider with no
	// contacts on its account (and for a dispute with no account linked).
	var requester, partyBilled string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(details->>'requester_email',''), coalesce(details->>'party_billed','') FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&requester, &partyBilled)
	provider := func() []string {
		if got := lookup(providerAccountID); len(got) > 0 {
			return got
		}
		if requester != "" {
			return []string{requester}
		}
		return nil
	}
	var pmRecipients []string
	var praw []byte
	if err := s.db.QueryRow(r.Context(),
		`SELECT config->'correspondence'->'pm_recipients' FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&praw); err == nil {
		_ = json.Unmarshal(praw, &pmRecipients)
	}
	for _, role := range roles {
		switch role {
		case "provider", "requester", "filing_party":
			add(provider())
		case "health_plan":
			add(lookup(payerAccountID))
		case "agency":
			add(agencyRecipients)
		case "pm":
			add(pmRecipients)
		case "all_parties":
			add(provider())
			add(lookup(payerAccountID))
		case "billed_party":
			// who the Reviewer recorded as billed
			if partyBilled == "Provider" || partyBilled == "Both" {
				add(provider())
			}
			if partyBilled == "Health Plan" || partyBilled == "Both" {
				add(lookup(payerAccountID))
			}
		}
	}
	return out
}

// renderTemplate substitutes the case's placeholders ({case_number}, {provider_name}, {payer_name}, ...), the
// office's own names ({org_name}, {contact_email}, {agency_salutation}, ... -- letterVars) and the bracketed
// fill-ins the case can answer ([Plan name], [Provider Contact Name], ...).
func (s *server) renderTemplate(r *http.Request, tenant, caseID, text string) string {
	var caseNumber, providerID, payerID string
	var qpa int64
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT case_number, coalesce(provider_id,''), coalesce(payer_id,''), coalesce(qpa_cents,0)
		FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&caseNumber, &providerID, &payerID, &qpa)
	// Names as people know them: what was read from the documents or given
	// at intake. The internal ids these placeholders used to print mean
	// nothing in a letter.
	var provider, plan, filer, filerType, contact, providerContact, planContact, due string
	var disputed int64
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT coalesce(details->>'provider_org',''), coalesce(details->>'health_plan',''),
		       coalesce(details->>'requester_org',''), coalesce(details->>'filing_party_type',''),
		       coalesce(disputed_amount_cents,0), coalesce(details->>'requester_contact_name',''),
		       coalesce(nullif(details->>'provider_contact',''), ''), coalesce(nullif(details->>'health_plan_contact',''), '')
		FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&provider, &plan, &filer, &filerType, &disputed, &contact,
		&providerContact, &planContact)
	// The open invoice's due date, for "[due date]" in a payment reminder.
	_ = s.db.QueryRow(r.Context(), `
		SELECT coalesce(to_char(min(due_date), 'FMMonth FMDD, YYYY'), '') FROM public.invoices
		WHERE tenant=$1 AND case_id::text=$2 AND status='OPEN' AND due_date IS NOT NULL`, tenant, caseID).Scan(&due)
	if provider == "" {
		provider = s.accountName(r, tenant, providerID)
	}
	if plan == "" {
		plan = s.accountName(r, tenant, payerID)
	}
	provider, plan = letterPartyNames(provider, plan, filer, filerType)
	amount := "(amount not yet recorded)"
	if disputed > 0 {
		amount = fmt.Sprintf("$%s.%02d", groupThousands(disputed/100), disputed%100)
	}
	repl := map[string]string{
		"{case_number}":     caseNumber,
		"{provider_name}":   orWord(provider, "the provider"),
		"{payer_name}":      orWord(plan, "the health plan"),
		"{filing_party}":    orWord(filer, "the filing party"),
		"{disputed_amount}": amount,
		"{today}":           templateToday(),
		"{qpa}":             fmt.Sprintf("$%d.%02d", qpa/100, qpa%100),
		"{case_url_suffix}": "#/cases/" + caseID,
	}
	for k, v := range s.letterVars(r, tenant) {
		repl[k] = v
	}
	// Twice: an office value may itself use a variable ("Dear {agency_contacts},").
	for pass := 0; pass < 2; pass++ {
		for k, v := range repl {
			text = strings.ReplaceAll(text, k, v)
		}
	}
	// The filer's contact is its own side's contact: the provider's when a provider filed, the plan's when a plan did.
	if strings.EqualFold(filerType, "HEALTH_PLAN") {
		if planContact == "" {
			planContact = contact
		}
	} else if contact != "" {
		providerContact = contact
	}
	// The filer, as the letter greets it; and the Full Review estimate the Reviewer recorded in the Dispute Summary.
	filerName := orWord(contact, map[bool]string{true: plan, false: provider}[strings.EqualFold(filerType, "HEALTH_PLAN")])
	var estimateCents int64
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce((details->'dispute_summary'->>'full_review_estimate_cents')::bigint, 0)
		FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&estimateCents)
	estimate := ""
	if estimateCents > 0 {
		estimate = usd(estimateCents)
	}
	return fillInNames(text, letterNames{Provider: provider, Plan: plan, ProviderContact: providerContact, PlanContact: planContact, DueDate: due,
		Filer: filerName, Estimate: estimate})
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
	// The template for this point on THIS case: the usage rules may put
	// another template's wording and recipients in its place.
	tpl := s.effectiveTemplate(r, tenant, caseID, in.Template)
	if tpl == nil {
		http.Error(w, `{"error":"unknown or retired template for this program"}`, http.StatusBadRequest)
		return
	}
	// The stage letters (acceptance, plan notification, Final Order...) are
	// steps: each can be drafted only when it is the case's next step.
	if !s.stepGuard(w, r, tenant, caseID, "letter:"+tpl.Key) {
		return
	}
	// Resolve the template's role-keyed to/cc policy to real addresses when
	// the caller didn't already supply them -- previously the template's
	// own to/cc config was never actually used for anything, only its
	// subject/qa_role; address resolution was silently left to the caller
	// every time, with no fallback for roles that have no account contact.
	if len(in.To) == 0 {
		toRoles := tpl.To
		// An RFI goes to whichever side it was addressed to.
		if tpl.Key == "rfi" {
			toRoles = []string{"provider"}
			if in.RfiTo == "plan" {
				toRoles = []string{"health_plan"}
			}
		}
		in.To = s.resolveRecipients(r, tenant, caseID, toRoles)
	}
	if len(in.CC) == 0 {
		in.CC = s.resolveRecipients(r, tenant, caseID, tpl.CC)
	}
	subject := s.renderTemplate(r, tenant, caseID, tpl.Subject)
	bodyText := in.Body
	if strings.TrimSpace(bodyText) == "" {
		bodyText = tpl.Body // program standard text (FL: verbatim source templates)
	}
	// No cost estimate requested: the acceptance email carries no estimate language (the paragraphs about one go).
	if tpl.Key == "acceptance" && s.estimateDeclined(r, tenant, caseID) {
		bodyText = dropEstimateParagraphs(bodyText)
	}
	// "[contact name]" is the contact of the side the letter goes to.
	side := "[Provider Contact Name]"
	if (tpl.Key == "rfi" && in.RfiTo == "plan") || (tpl.Key != "rfi" && len(tpl.To) > 0 && (tpl.To[0] == "health_plan" || tpl.To[0] == "plan")) {
		side = "[Health Plan Contact Name]"
	}
	bodyText = strings.ReplaceAll(bodyText, "[contact name]", side)
	body := s.renderTemplate(r, tenant, caseID, bodyText)
	for k, v := range in.Vars {
		subject = strings.ReplaceAll(subject, "{"+k+"}", v)
		body = strings.ReplaceAll(body, "{"+k+"}", v)
	}
	// Auto-share (G9): the template or the sender wants a secure upload link in
	// the message. Mint one inline and substitute the {share_link} placeholder —
	// the case manager never leaves the compose screen to copy-paste a URL.
	// Fire when explicitly requested OR when the body still carries the
	// placeholder after variable substitution.
	if tpl.Key == "rfi" && s.portalRFI(r.Context(), tenant, caseID, in.RfiTo) {
		// That side answers in its portal (document_requests.go): the letter
		// tells it where, instead of carrying an upload link.
		portal := strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/app.html#/document-requests"
		if strings.Contains(body, "{share_link}") {
			body = strings.ReplaceAll(body, "{share_link}", portal)
		} else {
			body += "\n\nPlease sign in to the portal and upload the documents under Document requests: " + portal
		}
		in.AutoShare = false
	}
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
			SELECT d.object_key FROM tenant_%s.documents d
			WHERE d.case_id=$1 AND NOT d.sealed AND d.scan_status<>'BLOCKED' AND NOT `+internalDocSQL+`
			ORDER BY d.created_at DESC LIMIT 1`, sanitizeTenant(tenant)), caseID).Scan(&objectKey)
		// The plan notification carries ALL the filing party's documents,
		// zipped ("Filing party dispute documentation FLxx-..."), not one file.
		if tpl.Key == "plan_notification" && objectKey != "" {
			objectKey = caseZipKey
		}
		// The Final Order package and the rationale copy carry their own set of documents (letterBundles),
		// resolved when the link is used.
		if _, ok := letterBundles[tpl.Key]; ok {
			objectKey = caseSetPrefix + tpl.Key
		}
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
	// QA gate (G4): the template's QA role must approve before send. The
	// worker is exempt -- what the workflow sends at a lapsed deadline is a
	// fixed notice with nobody at a compose screen to wait for.
	if tpl.QARole != "" && !contains(p.Roles, serviceRole) {
		status = "PENDING"
	}
	var qid string
	// artifact stores the template key (schema comment: "template key, e.g.
	// acceptance_provider") -- previously hardcoded to the literal 'EMAIL',
	// which made it impossible for qaDecision to ever look up which role's
	// approval a pending draft actually needed.
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.qa_reviews (tenant, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by, requested_by_sub, rfi_to)
		VALUES ($1,$2,$3,'email',$4,$5,$6,$7,$8,$9,$10,nullif($11,'')) RETURNING id`,
		tenant, caseID, tpl.Key, subject, body, toJ, ccJ, status, displayName(p), p.Subject, in.RfiTo).Scan(&qid)

	if status == "PENDING" {
		// Held for QA: nothing is sent yet, but the mail log shows it as
		// PENDING so "where is that email?" has an answer before approval.
		s.logCorrespondenceHeld(r, tenant, caseID, in.Template, subject, body, in.To, in.CC, p.Subject, qid)
		// The case shows it is waiting on QA, and the approver is told.
		if prev, changed := s.markCaseInQA(r, tenant, caseID, tpl.Key); changed {
			_, _ = s.db.Exec(r.Context(),
				`UPDATE public.qa_reviews SET prev_internal_status=$3 WHERE tenant=$1 AND id=$2`, tenant, qid, prev)
		}
		s.notify(r, tenant, "role:"+tpl.QARole, "QA_REVIEW",
			fmt.Sprintf("%s draft on case %s awaiting %s QA approval", tpl.Key, caseID, tpl.QARole), "#/qa")
	} else {
		// No QA gate on this template — deliver immediately via SMTP. Mail the provider queued for later
		// (mailqueue.go) is sent: it goes out automatically, so the letter counts as sent.
		if err := s.sendMail(in.To, in.CC, subject, body); err != nil && !errors.Is(err, errMailQueued) {
			s.logCorrespondence(r, tenant, caseID, "OUT", in.Template, subject, body, in.To, in.CC, p.Subject, err.Error())
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
		s.afterLetterSent(r, tenant, caseID, in.Template, in.RfiTo)
		s.recordPartyDocs(r, tenant, caseID, in.Template, in.RfiTo, subject, body, qid, in.To, in.CC, time.Time{})
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
// withCaseNumbers adds each row's case_number (from its case_id), so the QA gate shows the case people know.
func (s *server) withCaseNumbers(r *http.Request, tenant string, lists ...[]map[string]any) {
	var ids []string
	for _, l := range lists {
		for _, row := range l {
			if id := fmt.Sprint(row["case_id"]); id != "" && id != "<nil>" && !contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	nums := map[string]string{}
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`SELECT id::text, case_number FROM tenant_%s.cases WHERE id::text = ANY($1)`, sanitizeTenant(tenant)), ids)
	if err != nil {
		return
	}
	for rows.Next() {
		var id, n string
		if rows.Scan(&id, &n) == nil {
			nums[id] = n
		}
	}
	rows.Close()
	// Who approved or reviewed it: stored as the account id; shown by name.
	var subs []string
	for _, l := range lists {
		for _, row := range l {
			row["case_number"] = nums[fmt.Sprint(row["case_id"])]
			if v, ok := row["reviewed_by"].(string); ok && len(v) == 36 && strings.Count(v, "-") == 4 && !contains(subs, v) {
				subs = append(subs, v)
			}
		}
	}
	if len(subs) > 0 {
		names := s.resolveSubjectNames(r.Context(), subs)
		for _, l := range lists {
			for _, row := range l {
				if v, ok := row["reviewed_by"].(string); ok && names[v] != "" {
					row["reviewed_by"] = names[v]
				}
			}
		}
	}
}

func (s *server) qaQueue(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, qaReadRoles...) {
		if hasAnyRole(p, qaDraftOnlyRoles...) {
			s.qaQueueOwn(w, r, p)
			return
		}
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
			ORDER BY created_at DESC`, tenant, cid)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"queue": rows})
		return
	}
	rows, err := s.queryRows(r, `
		SELECT id, case_id, artifact, channel, subject, status, drafted_by, created_at
		FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING' ORDER BY created_at DESC`, tenant)
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
	// Approved and handed back: waiting for the drafter to send.
	toSend, _ := s.queryRows(r, `
		SELECT id, case_id, artifact, subject, drafted_by, reviewed_by, reviewed_at, coalesce(requested_by_sub,'') AS requested_by_sub,
		       to_recipients, cc_recipients
		FROM public.qa_reviews WHERE tenant=$1 AND status='APPROVED' AND sent_at IS NULL AND coalesce(channel,'email')='email'
		ORDER BY reviewed_at DESC`, tenant)
	s.withCaseNumbers(r, tenant, rows, recent, toSend)
	writeJSON(w, http.StatusOK, map[string]any{"queue": rows, "recent": recent, "to_send": toSend})
}

// qaDraftOnlyRoles draft a gated letter without reviewing any: the Coder
// drafts the Full Review Estimated Cost email, the PM approves it, and the
// Coder sends it. They see only their own drafts in the QA gate.
var qaDraftOnlyRoles = []string{"CODER"}

// qaQueueOwn: the QA gate for a drafter -- their own drafts awaiting review, the approved ones waiting for them to
// send, and their recent decisions; and the same for every case they are now the specialist on, so a draft made
// before they were assigned is theirs to follow and send.
func (s *server) qaQueueOwn(w http.ResponseWriter, r *http.Request, p principal) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `
		SELECT id, case_id, artifact, channel, subject, status, drafted_by, created_at
		FROM public.qa_reviews WHERE tenant=$1 AND status='PENDING' AND (requested_by_sub=$2 OR case_id IN (SELECT h.case_id FROM public.case_helpers h WHERE h.tenant=$1 AND h.user_id=$2)) ORDER BY created_at DESC`, tenant, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	recent, _ := s.queryRows(r, `
		SELECT id, case_id, subject, status, drafted_by, reviewed_by, reviewed_at
		FROM public.qa_reviews WHERE tenant=$1 AND status<>'PENDING' AND (requested_by_sub=$2 OR case_id IN (SELECT h.case_id FROM public.case_helpers h WHERE h.tenant=$1 AND h.user_id=$2))
		ORDER BY reviewed_at DESC NULLS LAST LIMIT 20`, tenant, p.Subject)
	toSend, _ := s.queryRows(r, `
		SELECT id, case_id, artifact, subject, drafted_by, reviewed_by, reviewed_at, coalesce(requested_by_sub,'') AS requested_by_sub,
		       to_recipients, cc_recipients
		FROM public.qa_reviews WHERE tenant=$1 AND status='APPROVED' AND sent_at IS NULL AND coalesce(channel,'email')='email'
		  AND (requested_by_sub=$2 OR case_id IN (SELECT h.case_id FROM public.case_helpers h WHERE h.tenant=$1 AND h.user_id=$2))
		ORDER BY reviewed_at DESC`, tenant, p.Subject)
	s.withCaseNumbers(r, tenant, rows, recent, toSend)
	writeJSON(w, http.StatusOK, map[string]any{"queue": rows, "recent": recent, "to_send": toSend, "own_only": true})
}

func (s *server) qaGet(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, qaReadRoles...) {
		http.Error(w, `{"error":"forbidden: requires a case-staff or QA role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	qid := chi.URLParam(r, "qaId")
	rows, err := s.queryRows(r, `
		SELECT id, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by, created_at,
		       coalesce(reviewed_by,'') AS reviewed_by, reviewed_at, sent_at, coalesce(review_note,'') AS review_note,
		       coalesce(requested_by_sub,'') AS requested_by_sub
		FROM public.qa_reviews WHERE tenant=$1 AND id=$2`, tenant, qid)
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// What the reviewer previews besides the text: a letter's own PDF, and the documents an email's links carry.
	q := rows[0]
	caseID, _ := q["case_id"].(string)
	artifact, _ := q["artifact"].(string)
	docs := []map[string]any{}
	if caseID != "" {
		t := sanitizeTenant(tenant)
		if q["channel"] == "letter" {
			var id, name string
			if s.db.QueryRow(r.Context(), fmt.Sprintf(`
				SELECT d.id::text, coalesce(d.filename,'') FROM tenant_%s.documents d, public.qa_reviews q
				WHERE q.tenant=$1 AND q.id::text=$2 AND d.case_id::text=$3
				  AND (d.object_key LIKE '%%/' || $4 || '-%%' OR ($4 = 'final_order_rationale' AND d.filename ILIKE 'Review Rationale and Letter%%'))
				  AND d.created_at <= q.created_at + interval '2 minutes'
				ORDER BY d.created_at DESC LIMIT 1`, t), tenant, qid, caseID, artifact).Scan(&id, &name) == nil {
				docs = append(docs, map[string]any{"doc_id": id, "filename": name})
			}
		} else if body, _ := q["body"].(string); body != "" {
			for _, id := range s.linkedDocs(r.Context(), tenant, caseID, body) {
				var name string
				_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(filename,'') FROM tenant_%s.documents WHERE id::text=$1`, t), id).Scan(&name)
				docs = append(docs, map[string]any{"doc_id": id, "filename": name})
			}
		}
	}
	q["documents"] = docs
	writeJSON(w, http.StatusOK, q)
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
	var caseID, subject, body, status, artifact, channel, draftedBy, requestedBySub, rfiTo string
	var toJ, ccJ []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT case_id, subject, body, status, to_recipients, cc_recipients, artifact, coalesce(channel,'email'), coalesce(drafted_by,''), coalesce(requested_by_sub,''), coalesce(rfi_to,'') FROM public.qa_reviews WHERE tenant=$1 AND id=$2`,
		tenant, qid).Scan(&caseID, &subject, &body, &status, &toJ, &ccJ, &artifact, &channel, &draftedBy, &requestedBySub, &rfiTo)
	originalBody := body // the EDIT prong below may replace body; the draft-quality capture needs the pre-edit text
	// APPROVED (not yet SENT) + a fresh APPROVE decision is a retry of a
	// previously failed send -- status only reaches APPROVED-without-SENT
	// when sendMail below failed last time, and the response used to claim
	// "stays APPROVED so the reviewer can retry" with no actual way to do
	// that (calling this again always 409'd). REJECT still requires PENDING
	// -- rejecting something already approved isn't a retry, it's reversing
	// a decision, which this endpoint doesn't support.
	// Retry only applies to mail rows — channel 'note' rows (copilot
	// determination rationales) are terminal at APPROVED, nothing is sent.
	isRetry := status == "APPROVED" && in.Decision == "APPROVE" && channel != "note" && channel != "letter"
	if err != nil || (status != "PENDING" && !isRetry) {
		http.Error(w, `{"error":"not pending"}`, http.StatusConflict)
		return
	}
	if isRetry && s.reviewerSends(r, tenant) {
		// Approving again what has already passed (a second click, or a stale page) changes nothing: say where the
		// draft is instead of refusing -- the decision the person wanted is already on record.
		writeJSON(w, http.StatusOK, map[string]any{"status": "APPROVED", "unchanged": true, "next": "with the Reviewer to send"})
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
	// A program with a QA gate (every template names the role that approves
	// it -- the PM in FL) applies that gate to an assistant-written draft
	// too. Without this, a Reviewer could have the assistant write a letter
	// and approve it personally: no template matched, so no approving role
	// was required, and the exemption below waived the second person.
	programQARole := ""
	if strings.HasPrefix(artifact, "copilot_") {
		programQARole = defaultQARole(s.corrTemplates(r, tenant))
	}
	copilotRequesterExempt := strings.HasPrefix(artifact, "copilot_") && requesterMatch && programQARole == ""
	// A tenant admin (or a platform/federal admin) may decide their own drafts: the office's admin is the last word
	// on its own queue. It is recorded on the case so the missing second person is visible, not silent.
	adminSelf := selfApproval && wfIsAdmin(p)
	if selfApproval && !copilotRequesterExempt && !adminSelf {
		http.Error(w, `{"error":"forbidden: cannot approve or reject your own draft -- needs a second reviewer"}`, http.StatusForbidden)
		return
	}
	if adminSelf {
		s.logActivity(r.Context(), tenant, caseID, "QA_SELF_DECIDED",
			fmt.Sprintf("%s: %s decided their own draft as an admin (%s)", subject, name, strings.ToLower(in.Decision)))
	}
	// Generic QA-role gate: the approver must hold the role the template
	// configured as qa_role (e.g. "ATTORNEY" on FL AHCA's dismissal
	// template), read straight from program_rules config -- no per-program
	// branching in Go. Falls back to a broad staff check when the artifact
	// doesn't match a known correspondence template (e.g. a lettergen-py
	// DOCX letter review, which opens a qa_reviews row with no qa_role
	// concept at all).
	qaRole, qaPreRole := "", ""
	for _, t := range s.corrTemplates(r, tenant) {
		if t.Key == artifact {
			qaRole, qaPreRole = t.QARole, t.QAPreRole
			break
		}
	}
	if qaRole == "" {
		qaRole = programQARole
	}
	// Two-stage QA: until the first role has checked the draft, an APPROVE
	// from that role records the check and leaves the draft pending for the
	// second role; nothing is sent yet.
	if qaPreRole != "" && in.Decision == "APPROVE" && status == "PENDING" {
		var pre string
		_ = s.db.QueryRow(r.Context(),
			`SELECT coalesce(pre_approved_by,'') FROM public.qa_reviews WHERE tenant=$1 AND id=$2`, tenant, qid).Scan(&pre)
		if pre == "" {
			if !hasAnyRole(p, qaPreRole, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": fmt.Sprintf("forbidden: the %s checks this draft first, then the %s approves and sends it", wfOwnerLabels[qaPreRole], wfOwnerLabels[qaRole])})
				return
			}
			_, _ = s.db.Exec(r.Context(),
				`UPDATE public.qa_reviews SET pre_approved_by=$3, pre_approved_at=now() WHERE tenant=$1 AND id=$2`, tenant, qid, displayName(p))
			s.logActivity(r.Context(), tenant, caseID, "QA_FIRST_CHECK",
				fmt.Sprintf("%s checked by %s — now with the %s for approval", subject, displayName(p), wfOwnerLabels[qaRole]))
			s.notify(r, tenant, "role:"+qaRole, "QA_REVIEW",
				fmt.Sprintf("%s draft on case %s has passed the %s's check and awaits your approval", artifact, caseID, wfOwnerLabels[qaPreRole]), "#/qa")
			writeJSON(w, http.StatusOK, map[string]any{"status": "PENDING", "first_check": "recorded",
				"next": "awaiting " + wfOwnerLabels[qaRole] + " approval"})
			return
		}
		if pre == displayName(p) && !wfIsAdmin(p) {
			http.Error(w, `{"error":"forbidden: the second approval must come from a different person than the first check"}`, http.StatusForbidden)
			return
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
	// displayName(p), not p.Subject -- drafted_by (same table) already uses
	// it; reviewed_by was the one field here still showing a bare UUID.
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.qa_reviews SET status=$3, reviewed_by=$4, reviewed_at=now(), review_note=$5
		WHERE tenant=$1 AND id=$2`, tenant, qid, newStatus, displayName(p), in.Note)
	// Phase 3-lite: every decision is a draft-quality observation (decision +
	// token edit distance when the reviewer rewrote the body). Synchronous on
	// purpose: losing observations to a queue outage would silently bias the
	// dataset that later decides which templates actually work.
	if status == "PENDING" { // not on send-retry re-approvals — one observation per draft
		s.captureQAEdit(r, tenant, qid, caseID, artifact, channel, draftedBy, in.Decision, originalBody, body, p.Subject)
	}

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
	// channel 'letter' (a generated PDF letter: decision report, cover letter, invoice, stage letters): approval
	// clears the document itself. Nothing is emailed here -- the letter goes out attached to the email that
	// carries it (the Final Order package, the acceptance email...), so there is no recipient to ask for.
	if in.Decision == "APPROVE" && channel == "letter" {
		s.logActivity(r.Context(), tenant, caseID, "LETTER_APPROVED",
			fmt.Sprintf("%s passed QA (%s) — ready to go out with its email", subject, displayName(p)))
		// An invoice that passed QA is mailed to the party billed: its email is drafted for the PM.
		if party := map[string]string{"invoice_health_plan": "HEALTH_PLAN", "invoice_provider": "PROVIDER"}[artifact]; party != "" {
			s.draftInvoiceEmail(r, tenant, caseID, party, qid)
		}
		s.logAudit(r.Context(), tenant, caseID, "QA_DECISION", map[string]any{
			"by": p.Subject, "decision": in.Decision, "subject": subject, "note": in.Note, "channel": channel,
		})
		if requestedBySub != "" {
			s.notify(r, tenant, requestedBySub, "DRAFT_APPROVED",
				fmt.Sprintf("The letter %q passed QA.", subject), "#/cases/"+caseID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": newStatus, "letter": true})
		return
	}
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
		// Hand-back (SOP stage 0): where the program has the drafter send,
		// approval does not deliver. The draft is APPROVED and goes back to
		// the Reviewer, who enters the addresses and sends it (qaSend).
		if s.reviewerSends(r, tenant) && !isRetry {
			who := requestedBySub
			if who == "" {
				who = "role:CASE_MANAGER"
			}
			s.notify(r, tenant, who, "DRAFT_APPROVED",
				fmt.Sprintf("%q passed QA — check the addresses and send it", subject), "#/qa")
			s.logActivity(r.Context(), tenant, caseID, "QA_APPROVED",
				fmt.Sprintf("QA-approved by %s: %s — back with the Reviewer to send", displayName(p), subject))
			s.logAudit(r.Context(), tenant, caseID, "QA_DECISION", map[string]any{
				"by": p.Subject, "decision": in.Decision, "subject": subject, "note": in.Note, "sent": false,
			})
			writeJSON(w, http.StatusOK, map[string]any{"status": "APPROVED", "next": "with the Reviewer to send"})
			return
		}
		deliveryError = s.deliverApproved(r, p, tenant, qid, caseID, artifact, subject, body, rfiTo, requestedBySub, to, cc)
	} else {
		s.releaseCaseFromQA(r, tenant, caseID, qid)
		s.settleHeldMail(r, tenant, qid, "REJECTED", "rejected in QA"+orDash(": "+in.Note), body, to, cc)
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

// logCorrespondence is the platform's mail journal: EVERY inbound and
// outbound mail is recorded here — including failed deliveries, so "did that
// letter actually go out?" is answerable from the record, not from memory.
// case_id may be "" for pre-case mail (payment demands, intake links).
func (s *server) logCorrespondence(r *http.Request, tenant, caseID, direction, template, subject, body string, to, cc []string, actor string, deliveryErr ...string) {
	rcpts, _ := json.Marshal(map[string]any{"to": to, "cc": cc})
	status, derr := "SENT", ""
	if len(deliveryErr) > 0 && deliveryErr[0] != "" {
		status, derr = "FAILED", truncate(deliveryErr[0], 500)
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.correspondence_log (tenant, case_id, direction, template, subject, body, recipients, sent_by, delivery_status, delivery_error)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		tenant, caseID, direction, template, subject, body, rcpts, actor, status, derr)
	// Mail that reached a party on the platform is kept under the dispute in its portal (party_documents.go).
	if direction == "OUT" && status == "SENT" && caseID != "" && template != "sharebox_download" {
		s.recordPartyDocs(r, tenant, caseID, template, "", subject, body, "", to, cc, time.Time{})
	}
}

// listMailJournal: GET /correspondence?direction=IN|OUT&q=... — the
// tenant-wide mail log. Every inbound and outbound mail lands here,
// including pre-case mail (intake links, payment demands, time reports) and
// inbound mail that matched no case — the "can we refer to it?" view.
// Staff-only: bodies carry party PII.
func (s *server) listMailJournal(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FINANCE", "ARBITRATOR", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	q := `SELECT c.id, c.case_id, COALESCE(k.case_number,'') AS case_number, c.direction, c.template,
	             c.subject, c.body, c.recipients, c.sent_by, c.created_at,
	             c.delivery_status, coalesce(c.delivery_error,'') AS delivery_error,
	             c.status_updated_at, count(*) OVER() AS total_count
	      FROM public.correspondence_log c
	      LEFT JOIN tenant_` + sanitizeTenant(tenant) + `.cases k ON k.id::text = c.case_id
	      WHERE c.tenant=$1`
	args := []any{tenant}
	if d := r.URL.Query().Get("direction"); d == "IN" || d == "OUT" {
		q += ` AND c.direction=$2`
		args = append(args, d)
	}
	if term := strings.TrimSpace(r.URL.Query().Get("q")); term != "" {
		q += fmt.Sprintf(` AND (c.subject ILIKE $%d OR c.body ILIKE $%d OR k.case_number ILIKE $%d)`, len(args)+1, len(args)+1, len(args)+1)
		args = append(args, "%"+term+"%")
	}
	if st := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status"))); st != "" {
		args = append(args, st)
		q += fmt.Sprintf(` AND c.delivery_status = $%d`, len(args))
	}
	if from := r.URL.Query().Get("from"); from != "" {
		args = append(args, from)
		q += fmt.Sprintf(` AND c.created_at >= $%d::date`, len(args))
	}
	if to := r.URL.Query().Get("to"); to != "" {
		args = append(args, to)
		q += fmt.Sprintf(` AND c.created_at < ($%d::date + interval '1 day')`, len(args))
	}
	limit, offset := pageParams(r, 100, 500)
	q += fmt.Sprintf(` ORDER BY c.created_at DESC, c.id DESC LIMIT %d OFFSET %d`, limit, offset)
	rows, err := s.queryRows(r, q, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// The window count rides on every row; a page past the end has no rows
	// and so reports only what it skipped.
	total := offset
	if len(rows) > 0 {
		total = int(toInt64(rows[0]["total_count"]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"mail": rows,
		"total": total, "next_offset": nextOffset(offset, limit, total)})
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
		SELECT id, direction, template, subject, recipients, sent_by, created_at,
		       delivery_status, coalesce(delivery_error,'') AS delivery_error
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
	in.MaxUses = shareLinkUses(in.Kind, in.MaxUses)
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.share_links (token, tenant, case_id, kind, object_key, expires_at, max_uses, max_files, created_by)
		VALUES ($1,$2,$3,$4, nullif($5,''), now() + make_interval(days => $6), $7, $7, $8)`,
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

// autoShareLink mints a 7-day link (uploads: uploadLinkFiles files; downloads: 10 uses) for a case and returns the
// absolute portal URL. Used by draftCorrespondence's auto-share path so the
// sender never has to pre-create and paste a link. Failure is soft: the
// placeholder degrades to the relative path rather than failing the draft.
func (s *server) autoShareLink(r *http.Request, tenant, caseID, kind, objectKey string) string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "/s/"
	}
	token := hex.EncodeToString(buf)
	p, _ := r.Context().Value(ctxPrincipal{}).(principal) // none in background work (a payment settled by the reconcile pass)
	if p.Subject == "" {
		p.Subject = "system"
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.share_links (token, tenant, case_id, kind, object_key, expires_at, max_uses, max_files, created_by)
		VALUES ($1,$2,$3,$4, nullif($5,''), now() + interval '7 days', $7, $7, $6)`,
		token, tenant, caseID, kind, objectKey, p.Subject, shareLinkUses(kind, 10)); err != nil {
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

// reviewerSends reports whether this program hands an approved draft back to
// its drafter to send (config correspondence.reviewer_sends), as the FL SOP
// does, instead of sending on approval.
func (s *server) reviewerSends(r *http.Request, tenant string) bool {
	var on bool
	_ = s.db.QueryRow(r.Context(),
		`SELECT coalesce((config->'correspondence'->>'reviewer_sends')::boolean, false) FROM public.program_rules WHERE tenant=$1`,
		tenant).Scan(&on)
	return on
}

// deliverApproved sends an approved draft and applies everything that
// follows a letter going out. It returns the delivery error text, "" on
// success; a failure leaves the draft APPROVED so the send can be retried.
func (s *server) deliverApproved(r *http.Request, p principal, tenant, qid, caseID, artifact, subject, body, rfiTo, requestedBySub string, to, cc []string) (deliveryError string) {
	// De-identification gate: nothing with direct identifiers leaves the
	// building by mail. Fail-closed when deid is down (DEID_FAIL_OPEN=1
	// downgrades to a logged release). The decision is always persisted to
	// public.deid_reports.
	if block := s.deidGateOutbound(r, tenant, caseID, artifact, qid, subject+"\n"+body); block != "" {
		s.logActivity(r.Context(), tenant, caseID, "EMAIL_HELD_DEID",
			fmt.Sprintf("Outbound %q held at send time: %s", subject, block))
		return block
	}
	var sendErr error
	if len(to) == 0 && len(cc) == 0 {
		sendErr = fmt.Errorf("no recipient email address on this draft — enter one and send again")
	} else {
		sendErr = s.sendMail(to, cc, subject, body)
	}
	queued := errors.Is(sendErr, errMailQueued)
	if err := sendErr; err != nil && !queued {
		deliveryError = err.Error()
		if !s.settleHeldMail(r, tenant, qid, "FAILED", err.Error(), body, to, cc) {
			s.logCorrespondence(r, tenant, caseID, "OUT", "qa_approved", subject, body, to, cc, p.Subject, err.Error())
		}
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
		if !s.settleHeldMail(r, tenant, qid, "SENT", "", body, to, cc) {
			s.logCorrespondence(r, tenant, caseID, "OUT", "qa_approved", subject, body, to, cc, p.Subject)
		}
		if len(to) > 0 {
			s.logCorrespondenceInquiry(r, tenant, caseID, to[0], fmt.Sprintf("%s: %s", artifact, subject), p.Subject)
		}
		how := "sent"
		if queued {
			how = "accepted for sending — the mail provider's limit was reached, so it goes out automatically shortly"
		}
		s.logActivity(r.Context(), tenant, caseID, "EMAIL_SENT",
			fmt.Sprintf("QA-approved by %s: %s %s to %d recipient(s)", p.Subject, subject, how, len(to)))
		if requestedBySub != "" {
			s.notify(r, tenant, requestedBySub, "DRAFT_SENT",
				fmt.Sprintf("Your correspondence draft %q was approved and sent to %d recipient(s).", subject, len(to)), "#/cases/"+caseID)
		}
		// The letter is out: the case leaves its QA status, the dates the
		// SOP ties to this letter are set, and the workflow hears of it.
		s.releaseCaseFromQA(r, tenant, caseID, qid)
		s.afterLetterSent(r, tenant, caseID, artifact, rfiTo)
		s.recordPartyDocs(r, tenant, caseID, artifact, rfiTo, subject, body, qid, to, cc, time.Time{})
		// Notification is a fact now — the checklist follows.
		s.autoChecklist(r, tenant, caseID)
	}
	return deliveryError
}

// qaSend: POST /qa/{qaId}/send {to, cc} — the drafter sends a draft that has
// passed QA. The addresses can be corrected here; the text cannot (it is
// what was approved).
func (s *server) qaSend(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	qid := chi.URLParam(r, "qaId")
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		To *[]string `json:"to"`
		CC *[]string `json:"cc"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var caseID, subject, body, status, artifact, channel, requestedBySub, rfiTo string
	var toJ, ccJ []byte
	if err := s.db.QueryRow(r.Context(), `
		SELECT case_id, subject, body, status, to_recipients, cc_recipients, artifact, coalesce(channel,'email'),
		       coalesce(requested_by_sub,''), coalesce(rfi_to,'')
		FROM public.qa_reviews WHERE tenant=$1 AND id=$2`, tenant, qid).
		Scan(&caseID, &subject, &body, &status, &toJ, &ccJ, &artifact, &channel, &requestedBySub, &rfiTo); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if status != "APPROVED" || channel != "email" {
		http.Error(w, `{"error":"only a draft that has passed QA and not yet been sent can be sent"}`, http.StatusConflict)
		return
	}
	// The drafter sends; Reviewers and admins may send any approved draft
	// (reminders the system drafted have no person behind them).
	// The specialist named on the case now sends it too: a draft outlives a reassignment.
	if requestedBySub != p.Subject && !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") && !s.namedOnCase(r, tenant, caseID, p.Subject) {
		http.Error(w, `{"error":"forbidden: an approved draft is sent by the person who drafted it, the specialist now on the case, or a Reviewer"}`, http.StatusForbidden)
		return
	}
	var to, cc []string
	_ = json.Unmarshal(toJ, &to)
	_ = json.Unmarshal(ccJ, &cc)
	if in.To != nil {
		to = *in.To
		if j, err := json.Marshal(to); err == nil {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.qa_reviews SET to_recipients=$3 WHERE tenant=$1 AND id=$2`, tenant, qid, j)
		}
	}
	if in.CC != nil {
		cc = *in.CC
		if j, err := json.Marshal(cc); err == nil {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.qa_reviews SET cc_recipients=$3 WHERE tenant=$1 AND id=$2`, tenant, qid, j)
		}
	}
	deliveryError := s.deliverApproved(r, p, tenant, qid, caseID, artifact, subject, body, rfiTo, "", to, cc)
	s.logAudit(r.Context(), tenant, caseID, "QA_SENT", map[string]any{
		"by": p.Subject, "qa_id": qid, "subject": subject, "to": to, "cc": cc, "email_delivery_error": deliveryError,
	})
	out := map[string]any{"status": "SENT"}
	if deliveryError != "" {
		out = map[string]any{"status": "APPROVED", "email_delivery_error": deliveryError}
	}
	writeJSON(w, http.StatusOK, out)
}

// defaultQARole is the role a program's QA gate belongs to: the one most of
// its templates name. "" for a tenant whose templates name none (no gate).
func defaultQARole(templates []CorrTemplate) string {
	n := map[string]int{}
	for _, t := range templates {
		if t.QARole != "" {
			n[t.QARole]++
		}
	}
	best, most := "", 0
	for role, c := range n {
		if c > most || (c == most && role < best) {
			best, most = role, c
		}
	}
	return best
}

func orWord(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// groupThousands writes 1234567 as 1,234,567.
func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// accountName is the legal name of a linked CRM account, "" when there is
// none (the id is not a name and is never printed in its place).
func (s *server) accountName(r *http.Request, tenant, accountID string) string {
	if accountID == "" {
		return ""
	}
	var name string
	_ = s.db.QueryRow(r.Context(),
		`SELECT coalesce(legal_name,'') FROM public.accounts WHERE tenant=$1 AND id::text=$2`, tenant, accountID).Scan(&name)
	return name
}

// letterPartyNames fills a missing provider or plan name from the filer: the
// organization that filed IS the provider (or, when a health plan filed, the
// plan), and a letter should name it rather than say "the provider". A name
// already known is never replaced. Pure, so it is tested.
func letterPartyNames(provider, plan, filer, filingPartyType string) (string, string) {
	if strings.EqualFold(filingPartyType, "HEALTH_PLAN") {
		if plan == "" {
			plan = filer
		}
	} else if provider == "" {
		provider = filer
	}
	return provider, plan
}

// namedOnCase: sub is one of the specialists named on the case (case_helpers).
func (s *server) namedOnCase(r *http.Request, tenant, caseID, sub string) bool {
	var n int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.case_helpers WHERE tenant=$1 AND case_id=$2 AND user_id=$3`,
		tenant, caseID, sub).Scan(&n)
	return n > 0
}

// estimateDeclined: the filing said no cost estimate is wanted (unknown counts as not declined).
func (s *server) estimateDeclined(r *http.Request, tenant, caseID string) bool {
	var v string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT lower(coalesce(details->>'fee_estimate_requested','')) FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&v)
	return v == "false" || v == "no"
}

// dropEstimateParagraphs removes the paragraphs of a message that speak of a cost estimate.
func dropEstimateParagraphs(body string) string {
	paras := strings.Split(body, "\n\n")
	kept := paras[:0]
	for _, p := range paras {
		if l := strings.ToLower(p); strings.Contains(l, "estimated cost") || strings.Contains(l, "cost estimate") {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "\n\n")
}
