// Author: Patrick Munis at newwave
// billingreminders.go — collection reminders, the case outcome, and time
// prompts.
//
// Reminders: every open review invoice gets a Payment Reminder drafted a few
// days before its due date and a Past Due Payment Reminder once it is past,
// one of each per invoice. They are DRAFTS in the PM's QA queue, like every
// other email -- nothing is sent without approval.
//
// Outcome: after the Final Order, the Reviewer records the Case Outcome,
// Number of Claims Reviewed, Final Amount Awarded and Party Billed (SOP
// stage 6). It is a step of its own, owned by the Reviewer.
//
// Time prompts: what a person did on a dispute is already on the audit
// trail; a day with work on a dispute and no time entry for it is offered
// back to them as a timesheet prompt.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// reminderLeadDays: how long before the due date the courtesy reminder is
// drafted, unless the program sets fees.reminder_lead_days.
const reminderLeadDays = 7

// reminderDue says which reminder an open invoice is owed today, if any.
func reminderDue(due, today time.Time, leadDays int) string {
	switch {
	case today.After(due):
		return "past_due_reminder"
	case !today.Before(due.AddDate(0, 0, -leadDays)):
		return "payment_reminder"
	}
	return ""
}

// sweepPaymentReminders drafts the reminders that are owed. Hourly; each
// reminder is drafted once per invoice (the draft's label carries the
// invoice id, so a second pass finds it).
func (s *server) sweepPaymentReminders() int {
	ctx := context.Background()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	rows, err := s.db.Query(ctx, `
		SELECT i.id::text, i.tenant, i.case_id, i.party, i.due_date
		FROM public.invoices i JOIN public.program_rules p ON p.tenant = i.tenant
		WHERE i.status='OPEN' AND i.kind NOT IN ('INITIAL_FEE','LATE_FEE') AND i.due_date IS NOT NULL`)
	if err != nil {
		return 0
	}
	type inv struct {
		id, tenant, caseID, party string
		due                       time.Time
	}
	var open []inv
	for rows.Next() {
		var x inv
		if rows.Scan(&x.id, &x.tenant, &x.caseID, &x.party, &x.due) == nil {
			open = append(open, x)
		}
	}
	rows.Close()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	drafted := 0
	for _, x := range open {
		lead := reminderLeadDays
		if cfg := s.loadProgram(r, x.tenant); cfg != nil && cfg.Fees.ReminderLeadDays != nil && *cfg.Fees.ReminderLeadDays >= 0 {
			lead = *cfg.Fees.ReminderLeadDays
		}
		tplKey := reminderDue(x.due, today, lead)
		if tplKey == "" {
			continue
		}
		label := fmt.Sprintf("system: %s for invoice %s", tplKey, x.id)
		var exists bool
		_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.qa_reviews WHERE tenant=$1 AND case_id=$2 AND drafted_by=$3)`,
			x.tenant, x.caseID, label).Scan(&exists)
		if exists {
			continue
		}
		tpl := s.effectiveTemplate(r, x.tenant, x.caseID, tplKey)
		if tpl == nil || tpl.QARole == "" {
			continue // no template, or one that would send unreviewed: leave it to a person
		}
		// Who is billed: the plan's or the provider's contacts on the linked
		// account; for a provider with none, the address that filed.
		role, who := "provider", "the Provider"
		if x.party == "HEALTH_PLAN" {
			role, who = "health_plan", "the Health Plan"
		}
		to := s.resolveRecipients(r, x.tenant, x.caseID, []string{role})
		var org, requester string
		_ = s.db.QueryRow(ctx, fmt.Sprintf(`
			SELECT coalesce(a.legal_name,''), coalesce(c.details->>'requester_email','')
			FROM tenant_%s.cases c LEFT JOIN public.accounts a
			  ON a.id::text = CASE WHEN $2='HEALTH_PLAN' THEN c.payer_id ELSE c.provider_id END AND a.tenant=$3
			WHERE c.id::text=$1`, sanitizeTenant(x.tenant)), x.caseID, x.party, x.tenant).Scan(&org, &requester)
		if len(to) == 0 && role == "provider" && requester != "" {
			to = []string{requester}
		}
		if org != "" {
			who = org
		}
		// The billed party first: rendering would otherwise fill "[Plan name] or [Provider Name]" with both names.
		body := strings.ReplaceAll(tpl.Body, "[Plan name] or [Provider Name]", who)
		body = s.renderTemplate(r, x.tenant, x.caseID, body)
		body = strings.ReplaceAll(body, "[due date]", x.due.Format("January 2, 2006"))
		subject := s.renderTemplate(r, x.tenant, x.caseID, tpl.Subject)
		if to == nil {
			to = []string{}
		}
		toJ, _ := json.Marshal(to)
		var qid string
		if err := s.db.QueryRow(ctx, `
			INSERT INTO public.qa_reviews (tenant, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by)
			VALUES ($1,$2,$3,'email',$4,$5,$6,'[]','PENDING',$7) RETURNING id`,
			x.tenant, x.caseID, tplKey, subject, body, toJ, label).Scan(&qid); err != nil {
			continue
		}
		s.logCorrespondenceHeld(r, x.tenant, x.caseID, tplKey, subject, body, to, nil, "system: collection reminders", qid)
		s.logActivity(ctx, x.tenant, x.caseID, "REMINDER_DRAFTED",
			fmt.Sprintf("%s drafted for %s (invoice due %s) — waiting in the QA gate", subject, who, x.due.Format("Jan 2, 2006")))
		s.notify(r, x.tenant, "role:"+tpl.QARole, "QA_REVIEW",
			fmt.Sprintf("%s drafted automatically for case %s — review and send", tplKey, x.caseID), "#/qa")
		drafted++
	}
	return drafted
}

// recordOutcome: POST /cases/{caseId}/outcome — the Reviewer's record after
// the Final Order: outcome, claims reviewed, amount awarded, party billed.
func (s *server) recordOutcome(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: the case outcome is recorded by the Reviewer"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !s.stepGuard(w, r, tenant, caseID, "outcome") {
		return
	}
	var in struct {
		CaseOutcome string `json:"case_outcome"`
		PartyBilled string `json:"party_billed"`
		AwardCents  *int64 `json:"final_amount_awarded_cents"`
		Claims      *int   `json:"num_claims_reviewed"`
		Explanation string `json:"outcome_explanation"` // required for "Other" (the SOP: "requires explanation")
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if in.CaseOutcome == "" || in.PartyBilled == "" {
		http.Error(w, `{"error":"case_outcome and party_billed are required"}`, http.StatusBadRequest)
		return
	}
	if prog := s.loadProgram(r, tenant); prog != nil {
		for key, v := range map[string]string{"case_outcome": in.CaseOutcome, "party_billed": in.PartyBilled} {
			if allowed := prog.FieldSchema[key]; len(allowed) > 0 && !contains(allowed, v) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": key + " must be one of " + strings.Join(allowed, ", ")})
				return
			}
		}
	}
	// The SOP's outcomes follow the path: a case that closed without review has its outcome from how it closed; a
	// default (the plan never responded) is a Default Award; a full review is a Full, Partial or No Award -- or, on
	// either decided path, Other (with its explanation) or a pre-settlement resolution.
	var internal, noResponse string
	var defaultDoc bool
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(internal_status,''), coalesce(program_dates->>'plan_no_response_at','') FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&internal, &noResponse)
	_ = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM public.qa_reviews WHERE tenant=$1 AND case_id::text=$2 AND artifact='default_determination' AND status <> 'REJECTED')`,
		tenant, caseID).Scan(&defaultDoc)
	if why := s.closingWithoutReview(r.Context(), tenant, caseID, internal); why != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this case closed without review (" + why + "): its outcome is set by how it closed"})
		return
	}
	isDefault := noResponse != "" || internal == "Plan - No Response" || defaultDoc
	if !outcomeAllowed(in.CaseOutcome, isDefault) {
		path := "a full review"
		if isDefault {
			path = "a default (the health plan did not respond)"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%q isn't an outcome of %s — choose one of: %s",
			in.CaseOutcome, path, strings.Join(pathOutcomes(isDefault), ", "))})
		return
	}
	if strings.EqualFold(in.CaseOutcome, "Other") && strings.TrimSpace(in.Explanation) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "explain the outcome: \"Other\" requires an explanation"})
		return
	}
	if (in.AwardCents != nil && (*in.AwardCents < 0 || *in.AwardCents > maxDisputedAmountCents)) || (in.Claims != nil && *in.Claims < 0) {
		http.Error(w, `{"error":"amount or claim count out of range"}`, http.StatusBadRequest)
		return
	}
	fields := map[string]any{"case_outcome": in.CaseOutcome, "party_billed": in.PartyBilled,
		"outcome_recorded_by": displayName(p), "outcome_recorded_at": time.Now().UTC().Format(time.RFC3339)}
	if in.AwardCents != nil {
		fields["final_amount_awarded_cents"] = *in.AwardCents
	}
	if in.Claims != nil {
		fields["num_claims_reviewed"] = *in.Claims
	}
	if strings.TrimSpace(in.Explanation) != "" {
		fields["outcome_explanation"] = strings.TrimSpace(in.Explanation)
	}
	merge, _ := json.Marshal(fields)
	res, err := s.db.Exec(r.Context(), fmt.Sprintf(
		`UPDATE tenant_%s.cases SET details = details || $2::jsonb, updated_at=now() WHERE id::text=$1`, sanitizeTenant(tenant)), caseID, string(merge))
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	if in.AwardCents != nil {
		s.syncAwardPayable(r, tenant, caseID, fields, p.Subject)
	}
	s.logActivity(r.Context(), tenant, caseID, "OUTCOME_RECORDED",
		fmt.Sprintf("Outcome recorded by %s: %s — party billed: %s", displayName(p), in.CaseOutcome, in.PartyBilled))
	s.logAudit(r.Context(), tenant, caseID, "CASE_OUTCOME_RECORDED", map[string]any{"by": p.Subject, "fields": fields})
	writeJSON(w, http.StatusOK, map[string]any{"status": "recorded", "outcome": fields})
}

// timePrompts: GET /time/prompts — disputes the caller worked on in the last
// 14 days, by day, for which they have recorded no time that day.
func (s *server) timePrompts(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT a.case_id, c.case_number, a.day::text AS day, a.actions, a.what
		FROM (SELECT case_id, created_at::date AS day, count(*) AS actions,
		             string_agg(DISTINCT lower(replace(action,'_',' ')), ', ') AS what
		      FROM public.audit_log
		      WHERE tenant=$1 AND created_at >= current_date - 14 AND coalesce(case_id,'') <> ''
		        AND action NOT IN ('TIME_ENTRY','TIME_ENTRY_UPDATED','TIME_ENTRY_DELETED')
		        AND payload LIKE '%%"by":"' || $2 || '"%%'
		      GROUP BY case_id, created_at::date) a
		JOIN tenant_%s.cases c ON c.id::text = a.case_id
		WHERE NOT EXISTS (SELECT 1 FROM public.time_entries t
		                  WHERE t.tenant=$1 AND t.subject=$2 AND t.case_id::text = a.case_id AND t.entry_date = a.day)
		ORDER BY a.day DESC, a.actions DESC LIMIT 50`, sanitizeTenant(tenant)), tenant, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prompts": rows})
}

// sweepLateFees charges the program's late fee (fees.late_fee) on review invoices still unpaid after the case's
// Final Order: on day after_days+1 and every every_days after, until the invoice is paid. Each charge is a
// LATE_FEE_CHARGED event on the late invoice, and the charges add up on the case's LATE_FEE invoice for that
// party. A pass charges only what has newly fallen due, so running it again the same day does nothing.
func (s *server) sweepLateFees() int {
	ctx := context.Background()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	// One replica at a time: two passes at once could both charge the same newly due fee.
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return 0
	}
	defer conn.Release()
	var mine bool
	if conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('idre:late-fee-sweep'))`).Scan(&mine) != nil || !mine {
		return 0
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('idre:late-fee-sweep'))`) //nolint:errcheck
	rows, err := s.db.Query(ctx, `
		SELECT i.id::text, i.tenant, i.case_id, i.party, i.kind FROM public.invoices i
		WHERE i.status='OPEN' AND i.kind NOT IN ('INITIAL_FEE','LATE_FEE')`)
	if err != nil {
		return 0
	}
	type inv struct{ id, tenant, caseID, party, kind string }
	var open []inv
	for rows.Next() {
		var x inv
		if rows.Scan(&x.id, &x.tenant, &x.caseID, &x.party, &x.kind) == nil {
			open = append(open, x)
		}
	}
	rows.Close()
	charged := 0
	for _, x := range open {
		cfg := s.loadProgram(r, x.tenant)
		if cfg == nil || cfg.Fees.LateFee == nil || cfg.Fees.LateFee.Cents <= 0 {
			continue
		}
		var finalOrder, caseNumber, internal string
		if s.db.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(program_dates->>'date_final_order_sent',''), case_number, coalesce(internal_status,'')
			FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(x.tenant)), x.caseID).Scan(&finalOrder, &caseNumber, &internal) != nil ||
			finalOrder == "" || closedWithoutReviewStatuses[internal] { // a case closed without review owes no review fee
			continue
		}
		fo, err := time.ParseInLocation("2006-01-02", finalOrder, cfg.location())
		if err != nil {
			continue
		}
		today, _ := time.ParseInLocation("2006-01-02", time.Now().In(cfg.location()).Format("2006-01-02"), cfg.location())
		due := cfg.Fees.LateFee.lateChargesDue(int(today.Sub(fo).Hours() / 24))
		var have int
		_ = s.db.QueryRow(ctx, `SELECT count(*) FROM public.financial_events WHERE tenant=$1 AND invoice_id::text=$2 AND kind='LATE_FEE_CHARGED'`,
			x.tenant, x.id).Scan(&have)
		for n := have; n < due; n++ {
			cents := cfg.Fees.LateFee.Cents
			var lateID string
			if s.db.QueryRow(ctx, `
				INSERT INTO public.invoices (tenant, case_id, invoice_no, party, kind, amount_cents, due_date, created_by)
				VALUES ($1,$2,$3,$4,'LATE_FEE',$5, now()::date, 'system:late-fee')
				ON CONFLICT (tenant, case_id, party, kind) DO UPDATE
				  SET amount_cents = CASE WHEN invoices.status='OPEN' THEN invoices.amount_cents ELSE 0 END + EXCLUDED.amount_cents,
				      status='OPEN', paid_at=NULL, due_date=EXCLUDED.due_date
				RETURNING id::text`, x.tenant, x.caseID, caseNumber+"-LATE", x.party, cents).Scan(&lateID) != nil {
				break
			}
			s.finEvent(r, x.tenant, x.caseID, x.id, "LATE_FEE_CHARGED", "NONE", cents, x.party, lateID, "system:late-fee")
			what := fmt.Sprintf("Late fee %s charged to %s: invoice %s (%s) not paid %d days after the Final Order of %s (charge %d)",
				usd(cents), x.party, caseNumber, strings.ReplaceAll(x.kind, "_", " "), cfg.Fees.LateFee.AfterDays, finalOrder, n+1)
			s.logActivity(ctx, x.tenant, x.caseID, "LATE_FEE_CHARGED", what)
			s.logAudit(ctx, x.tenant, x.caseID, "LATE_FEE_CHARGED", map[string]any{"by": "system:late-fee", "invoice_id": x.id,
				"late_fee_invoice_id": lateID, "amount_cents": cents, "party": x.party, "charge": n + 1})
			for _, role := range []string{"FINANCE", "PM"} {
				s.notify(r, x.tenant, "role:"+role, "LATE_FEE_CHARGED", "Case "+caseNumber+": "+what, "#/cases/"+x.caseID)
			}
			charged++
		}
	}
	return charged
}

// draftInvoiceEmail drafts the email that sends a review invoice to the party billed (the "invoice" template), with
// a secure link to that invoice's PDF, into the QA gate for the PM. Once per invoice letter (qid: its QA row).
func (s *server) draftInvoiceEmail(r *http.Request, tenant, caseID, party, qid string) {
	ctx := r.Context()
	label := "system: invoice email for letter " + qid
	var exists bool
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.qa_reviews WHERE tenant=$1 AND case_id=$2 AND drafted_by=$3)`,
		tenant, caseID, label).Scan(&exists)
	if exists {
		return
	}
	tpl := s.effectiveTemplate(r, tenant, caseID, "invoice")
	if tpl == nil || tpl.QARole == "" {
		s.logActivity(ctx, tenant, caseID, "INVOICE_EMAIL_SKIPPED", "The invoice passed QA, but the program has no invoice email template to send it with")
		return
	}
	// The invoice's own PDF: the newest generated invoice to this party.
	suffix := map[string]string{"HEALTH_PLAN": "%Health Plan%", "PROVIDER": "%Provider%"}[party]
	var objectKey, invoiceNo string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT object_key FROM tenant_%s.documents
		WHERE case_id=$1 AND uploaded_by LIKE 'lettergen:%%' AND filename ILIKE 'Invoice %%' AND filename ILIKE $2
		ORDER BY created_at DESC LIMIT 1`, sanitizeTenant(tenant)), caseID, suffix).Scan(&objectKey)
	var amount int64
	var due *time.Time
	_ = s.db.QueryRow(ctx, `SELECT invoice_no, amount_cents, due_date FROM public.invoices
		WHERE tenant=$1 AND case_id=$2 AND party=$3 AND kind NOT IN ('INITIAL_FEE','LATE_FEE') AND status<>'VOID'
		ORDER BY created_at DESC LIMIT 1`, tenant, caseID, party).Scan(&invoiceNo, &amount, &due)
	role, who := "provider", "the Provider"
	if party == "HEALTH_PLAN" {
		role, who = "health_plan", "the Health Plan"
	}
	to := s.resolveRecipients(r, tenant, caseID, []string{role})
	var org, requester string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`
		SELECT coalesce(a.legal_name,''), coalesce(c.details->>'requester_email','')
		FROM tenant_%s.cases c LEFT JOIN public.accounts a
		  ON a.id::text = CASE WHEN $2='HEALTH_PLAN' THEN c.payer_id ELSE c.provider_id END AND a.tenant=$3
		WHERE c.id::text=$1`, sanitizeTenant(tenant)), caseID, party, tenant).Scan(&org, &requester)
	if len(to) == 0 && role == "provider" && requester != "" {
		to = []string{requester}
	}
	if org != "" {
		who = org
	}
	link := "(the invoice is attached in the case's documents)"
	if objectKey != "" {
		link = s.autoShareLink(r, tenant, caseID, "download", objectKey)
	}
	dueText := ""
	if due != nil {
		dueText = due.Format("January 2, 2006")
	}
	rep := strings.NewReplacer("[Plan name] or [Provider Name]", who, "{invoice_number}", invoiceNo, "{invoice_amount}", usd(amount),
		"[due date]", dueText, "{download_link}", link)
	body := s.renderTemplate(r, tenant, caseID, rep.Replace(tpl.Body))
	subject := s.renderTemplate(r, tenant, caseID, rep.Replace(tpl.Subject))
	if to == nil {
		to = []string{}
	}
	toJ, _ := json.Marshal(to)
	var did string
	if err := s.db.QueryRow(ctx, `
		INSERT INTO public.qa_reviews (tenant, case_id, artifact, channel, subject, body, to_recipients, cc_recipients, status, drafted_by)
		VALUES ($1,$2,'invoice','email',$3,$4,$5,'[]','PENDING',$6) RETURNING id`,
		tenant, caseID, subject, body, toJ, label).Scan(&did); err != nil {
		return
	}
	s.logCorrespondenceHeld(r, tenant, caseID, "invoice", subject, body, to, nil, "system: invoice email", did)
	s.logActivity(ctx, tenant, caseID, "INVOICE_EMAIL_DRAFTED",
		fmt.Sprintf("Invoice %s email to %s drafted — waiting in the QA gate", invoiceNo, who))
	s.notify(r, tenant, "role:"+tpl.QARole, "QA_REVIEW", fmt.Sprintf("Invoice %s email drafted for %s — review and send", invoiceNo, who), "#/qa")
}
