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

// reminderLeadDays: how long before the due date the courtesy reminder is drafted.
const reminderLeadDays = 7

// reminderDue says which reminder an open invoice is owed today, if any.
func reminderDue(due, today time.Time) string {
	switch {
	case today.After(due):
		return "past_due_reminder"
	case !today.Before(due.AddDate(0, 0, -reminderLeadDays)):
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
		WHERE i.status='OPEN' AND i.kind <> 'INITIAL_FEE' AND i.due_date IS NOT NULL`)
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
		tplKey := reminderDue(x.due, today)
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
		var tpl *CorrTemplate
		for _, t := range s.corrTemplates(r, x.tenant) {
			if t.Key == tplKey {
				cp := t
				tpl = &cp
			}
		}
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
		body := s.renderTemplate(r, x.tenant, x.caseID, tpl.Body)
		body = strings.ReplaceAll(body, "[due date]", x.due.Format("January 2, 2006"))
		body = strings.ReplaceAll(body, "[Plan name] or [Provider Name]", who)
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
