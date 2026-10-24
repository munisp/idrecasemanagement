package main

// step_tasks.go — the system's tasks tick themselves off.
//
// The case workflow opens a task for each step someone has to take ("Dispute is eligible — send the Acceptance
// letter through QA…", created_by system:deadlines), and an SLA breach opens an escalation task ("Escalation:
// AGENCY_RECOMMENDATION breach on FL26-…", created_by system). Each is settled by a fact the case already records --
// the letter went out, the provider's permission came in, the plan responded, the recommendation was sent -- so
// when that fact is there the task is marked done, with why, and the calendar shows it done. A task someone wrote
// by hand is never touched: only its author knows when it is done.

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

func wfSent(f wfFacts, tpl string) bool { return f.Letter[tpl] == "SENT" }

func wfHas(f wfFacts, signal string) bool { _, ok := f.Act[signal]; return ok }

func wfPermissionIn(f wfFacts) bool {
	return wfHas(f, "ESTIMATE_PERMISSION") || f.Dates["date_provider_responded"] != "" || wfSent(f, "dismissal")
}

func wfPlanResponded(f wfFacts) bool {
	return wfHas(f, "PLAN_RESPONSE_RECEIVED") || f.Dates["date_plan_responded"] != "" || f.Internal == "Plan - No Response" ||
		f.Internal == "Plan Opt-Out" || wfSent(f, "final_order")
}

// stepTaskRules: a workflow task by the words of its step, the fact that settles it, and that fact in words.
var stepTaskRules = []struct {
	phrase string
	done   func(f wfFacts) bool
	why    string
}{
	// An eligibility letter settles the other one's task too: a dispute re-found eligible needs no Ineligible letter.
	{"send the Acceptance letter", func(f wfFacts) bool { return wfSent(f, "acceptance") || wfSent(f, "ineligible") }, "the eligibility letter was sent"},
	{"send the Ineligible Dispute letter", func(f wfFacts) bool { return wfSent(f, "ineligible") || wfSent(f, "acceptance") }, "the eligibility letter was sent"},
	{"provider permission not yet received", wfPermissionIn, "the provider's permission was recorded"},
	{"no permission to proceed received", wfPermissionIn, "the provider's permission was recorded"},
	{"send the Dismissal letter", func(f wfFacts) bool { return wfSent(f, "dismissal") }, "the Dismissal letter was sent"},
	{"Send the Health Plan Notification", func(f wfFacts) bool { return wfSent(f, "plan_notification") }, "the Health Plan Notification was sent"},
	{"Final Order package", func(f wfFacts) bool { return wfSent(f, "final_order") }, "the Final Order package was sent to AHCA"},
	{"verify it with the party and confirm it", func(f wfFacts) bool { return wfHas(f, "WITHDRAWAL_CONFIRMED") || wfSent(f, "withdrawal") }, "the withdrawal was confirmed"},
	{"send the Withdrawal letter", func(f wfFacts) bool { return wfSent(f, "withdrawal") }, "the Withdrawal letter was sent"},
	{"remind the Health Plan its response is due", wfPlanResponded, "the health plan responded"},
	{"no response from the Health Plan", func(f wfFacts) bool { return wfSent(f, "final_order") || f.HasOutcome }, "the determination was made"},
	{"notify AHCA for follow-up", wfPlanResponded, "the health plan responded"},
	{"response due tomorrow", wfPlanResponded, "the health plan responded"},
	{"final attempt to reach the plan", wfPlanResponded, "the health plan responded"},
}

// clockSettled: an SLA breach is over once the step its clock runs for is done.
var clockSettled = map[string]struct {
	done func(f wfFacts) bool
	why  string
}{
	"INITIAL_REVIEW":      {func(f wfFacts) bool { return wfSent(f, "acceptance") || wfSent(f, "ineligible") }, "the eligibility letter was sent"},
	"PROVIDER_PERMISSION": {wfPermissionIn, "the provider's permission was recorded"},
	"PLAN_NOTIFICATION":   {func(f wfFacts) bool { return wfSent(f, "plan_notification") }, "the Health Plan Notification was sent"},
	"PLAN_RESPONSE":       {wfPlanResponded, "the health plan responded"},
	"AGENCY_RECOMMENDATION": {func(f wfFacts) bool {
		return f.Dates["date_recommendation_sent_to_agency"] != "" || wfSent(f, "final_order")
	}, "the recommendation was sent to AHCA"},
	"FINAL_ORDER":            {func(f wfFacts) bool { return f.Dates["date_final_order_sent"] != "" || wfHas(f, "FINAL_ORDER_ISSUED") }, "AHCA's Final Order was recorded"},
	"OPT_OUT_LETTER":         {func(f wfFacts) bool { return wfSent(f, "plan_opt_out") }, "the opt-out letter was sent"},
	"ACCEPTANCE_PLAN_NOTICE": {func(f wfFacts) bool { return wfSent(f, "plan_notification") }, "the Health Plan Notification was sent"},
}

var escalationRe = regexp.MustCompile(`^Escalation: ([A-Z_]+) breach`)

// stepTaskSettled: whether a system task is settled by the case's facts, and why. closed: nothing further is due.
func stepTaskSettled(subject string, f wfFacts, closed bool) (bool, string) {
	if m := escalationRe.FindStringSubmatch(subject); m != nil {
		if c, ok := clockSettled[m[1]]; ok && c.done(f) {
			return true, c.why
		}
	} else {
		for _, rule := range stepTaskRules {
			if strings.Contains(subject, rule.phrase) {
				if rule.done(f) {
					return true, rule.why
				}
				break
			}
		}
	}
	if closed {
		return true, "the case is closed"
	}
	return false, ""
}

// settleStepTasks marks a case's system tasks done whose fact is in. Returns how many.
func (s *server) settleStepTasks(ctx context.Context, tenant, caseID string) int {
	rows, err := s.db.Query(ctx, `SELECT id::text, subject FROM public.tasks
		WHERE tenant=$1 AND case_id=$2 AND status='OPEN' AND created_by IN ('system','system:deadlines')`, tenant, caseID)
	if err != nil {
		return 0
	}
	type task struct{ id, subject string }
	var open []task
	for rows.Next() {
		var t task
		if rows.Scan(&t.id, &t.subject) == nil {
			open = append(open, t)
		}
	}
	rows.Close()
	if len(open) == 0 {
		return 0
	}
	f, err := s.workflowFacts(ctx, tenant, caseID)
	if err != nil {
		return 0
	}
	closed := wfClosedStatuses[f.Internal] || f.Status == "CLOSED" || f.Status == "WITHDRAWN" || f.Status == "INELIGIBLE"
	n := 0
	for _, t := range open {
		ok, why := stepTaskSettled(t.subject, f, closed)
		if !ok {
			continue
		}
		tag, err := s.db.Exec(ctx, `UPDATE public.tasks SET status='DONE', completed_at=now() WHERE tenant=$1 AND id::text=$2 AND status='OPEN'`, tenant, t.id)
		if err != nil || tag.RowsAffected() == 0 {
			continue
		}
		n++
		s.logActivity(ctx, tenant, caseID, "TASK_DONE", fmt.Sprintf("Task done automatically — %s: %s", why, truncate(t.subject, 300)))
	}
	return n
}

// sweepStepTasks settles the open system tasks of every case (the hooks catch most at once; this catches the rest,
// and the tasks opened before this existed).
func (s *server) sweepStepTasks() int {
	ctx := context.Background()
	rows, err := s.db.Query(ctx, `SELECT DISTINCT tenant, case_id FROM public.tasks
		WHERE status='OPEN' AND created_by IN ('system','system:deadlines') AND coalesce(case_id,'') <> '' LIMIT 2000`)
	if err != nil {
		return 0
	}
	type key struct{ tenant, caseID string }
	var ks []key
	for rows.Next() {
		var k key
		if rows.Scan(&k.tenant, &k.caseID) == nil {
			ks = append(ks, k)
		}
	}
	rows.Close()
	n := 0
	for _, k := range ks {
		n += s.settleStepTasks(ctx, k.tenant, k.caseID)
	}
	if n > 0 {
		slog.Info("step tasks settled", "tasks", n)
	}
	return n
}
