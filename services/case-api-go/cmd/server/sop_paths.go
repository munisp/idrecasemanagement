package main

// sop_paths.go — the SOP's paths and their outcomes (Streamline Workflows, Timelines, and Outcomes).
//
// A case closed without review takes its outcome from how it closed; a default (the plan never responded) is
// decided without the Coder, Nurse or Attorney reviews; and an outcome recorded on a case must be one its path can
// end in -- a full review can't be recorded as a Default Award, nor a default as a Full Award.

import (
	"context"
	"fmt"
	"strings"
)

// closureOutcome: the outcome a closing internal status means ("" for a status that closes nothing by itself).
// "Provider Closure Letter Issued" is the ineligible path's closure, unless it was the Dismissal letter that went.
func closureOutcome(internal string, dismissalSent, ineligibleSent bool) string {
	switch internal {
	case "Ineligible":
		return "Ineligible - Closed"
	case "Provider Closure Letter Issued":
		if dismissalSent && !ineligibleSent {
			return "Dismissed"
		}
		return "Ineligible - Closed"
	case "Dismissed":
		return "Dismissed"
	case "Provider - Withdrawal", "Withdrawn":
		return "Withdrawn"
	case "Plan Opt-Out":
		return "Plan Opt-Out"
	case "Closed-Refunded":
		return "Refunded"
	}
	return ""
}

// unsetOutcome: no outcome recorded yet ("" or the in-process placeholder).
func unsetOutcome(o string) bool {
	o = strings.TrimSpace(o)
	return o == "" || strings.HasPrefix(strings.ToUpper(o), "TBD")
}

// pathOutcomes: the outcomes a case on this path may be recorded with by hand. Default: the plan never responded.
// "Other" and a pre-settlement resolution fit any path that reached a decision stage.
func pathOutcomes(isDefault bool) []string {
	admin := []string{"Other", "Costs Resolved Prior to Final Determination (Pre-Settlement)"}
	if isDefault {
		return append([]string{"Default Award"}, admin...)
	}
	return append([]string{"Full Award", "Partial Award", "No Award"}, admin...)
}

func outcomeAllowed(outcome string, isDefault bool) bool {
	for _, o := range pathOutcomes(isDefault) {
		if strings.EqualFold(o, strings.TrimSpace(outcome)) {
			return true
		}
	}
	return false
}

// recordClosureOutcome sets the outcome of a case that has just closed without review: the outcome is how it closed.
// An award recorded before it closed can't stand (a case closed without review has no determination); an
// administrative closure someone recorded (Other, a pre-settlement) is kept. Returns the outcome set ("" for none).
func (s *server) recordClosureOutcome(ctx context.Context, tenant, caseID, internal string) string {
	t := sanitizeTenant(tenant)
	var current string
	var dismissal, ineligible bool
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(details->>'case_outcome','') FROM tenant_%s.cases WHERE id::text=$1`, t), caseID).Scan(&current)
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.qa_reviews WHERE tenant=$1 AND case_id::text=$2 AND artifact='dismissal' AND status='SENT'),
		EXISTS(SELECT 1 FROM public.qa_reviews WHERE tenant=$1 AND case_id::text=$2 AND artifact='ineligible' AND status='SENT')`,
		tenant, caseID).Scan(&dismissal, &ineligible)
	outcome := closureOutcome(internal, dismissal, ineligible)
	if outcome == "" || outcome == current || !(unsetOutcome(current) || awardOutcome(current)) {
		return ""
	}
	if _, err := s.db.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET details = coalesce(details,'{}'::jsonb) || jsonb_build_object('case_outcome', $2::text), updated_at=now() WHERE id::text=$1`, t),
		caseID, outcome); err != nil {
		return ""
	}
	note := ""
	if !unsetOutcome(current) {
		note = " — replaces " + current + ", recorded before it closed"
	}
	s.logActivity(ctx, tenant, caseID, "OUTCOME_RECORDED", fmt.Sprintf("Outcome: %s — the case closed without review (%s)%s", outcome, internal, note))
	return outcome
}

// awardOutcome: an outcome only a determination can have.
func awardOutcome(o string) bool {
	return strings.HasSuffix(o, " Award")
}

// withdrawalRefusal: why a withdrawal can't be recorded now, or "". The SOP's Withdrawn is "after acceptance and
// prior to final determination": the Acceptance letter has gone, and the Final Order package to AHCA hasn't.
func withdrawalRefusal(acceptance, finalOrder string) string {
	if acceptance != "SENT" {
		return "a withdrawal comes after the dispute is accepted, and the Acceptance letter hasn't gone yet — " +
			"to stop a dispute before acceptance, close it (Close and refund)"
	}
	if finalOrder == "SENT" {
		return "the determination has gone to AHCA — a dispute can't be withdrawn after its final determination"
	}
	return ""
}

// reviewInvoiceKindProblem: the review invoice a path is billed with -- DEFAULT for a default, FULL_REVIEW for a full
// review. Other kinds (the initial fee, administrative fees) aren't a review invoice and pass.
func reviewInvoiceKindProblem(kind string, isDefault bool) string {
	switch {
	case isDefault && kind == "FULL_REVIEW":
		return "the health plan didn't respond: this is a default — bill it with the DEFAULT invoice"
	case !isDefault && kind == "DEFAULT":
		return "the default invoice is only for a plan that didn't respond — a full review is billed with the FULL_REVIEW invoice"
	}
	return ""
}

// closingWithoutReview: why the case is closed, or closing, without review ("" when it isn't): a closed-without-review
// status, or one of the paths whose letter closes the case and hasn't gone yet -- the Attorney allowed the plan's
// opt-out (SOP 4), Capitol Bridge confirmed a withdrawal (SOP 5), or the provider didn't give its permission to
// proceed (SOP 6: the Dismissal). Neither path generates an invoice, and the
// outcome follows from how the case closes.
func (s *server) closingWithoutReview(ctx context.Context, tenant, caseID, internal string) string {
	if closedWithoutReviewStatuses[internal] {
		return internal
	}
	var optedOut, withdrawn bool
	var permission string
	_ = s.db.QueryRow(ctx, `SELECT coalesce((SELECT eligible FROM public.opt_out_decisions WHERE tenant=$1 AND case_id=$2
		ORDER BY created_at DESC LIMIT 1), false)`, tenant, caseID).Scan(&optedOut)
	if optedOut {
		return "Plan Opt-Out"
	}
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.case_activities WHERE tenant=$1 AND case_id::text=$2 AND type='WITHDRAWAL_CONFIRMED')`,
		tenant, caseID).Scan(&withdrawn)
	if withdrawn {
		return "Withdrawn"
	}
	_ = s.db.QueryRow(ctx, `SELECT coalesce((SELECT body FROM public.case_activities WHERE tenant=$1 AND case_id::text=$2 AND type='ESTIMATE_PERMISSION'
		ORDER BY created_at DESC LIMIT 1), '')`, tenant, caseID).Scan(&permission)
	if strings.Contains(strings.ReplaceAll(permission, " ", ""), `"granted":false`) {
		return "Dismissed"
	}
	return ""
}

// determinationSent: the determination has gone to AHCA -- the Final Order package was sent, or (a case from before
// letters were tracked) its status or date says so.
func determinationSent(f wfFacts) bool {
	return f.Letter["final_order"] == "SENT" || f.Dates["date_recommendation_sent_to_agency"] != "" ||
		f.Internal == "Determination Sent to FL" || f.Internal == "Final Order Issued"
}
