// workflowsteps.go — the SOP as an ordered list of steps on a case.
//
// A programmed case moves through the stages of the program's SOP one step
// at a time. This file answers two questions from the case's own record:
//
//   - What are the steps, which are done, and which ONE is next?
//   - May this person take this action now? Only the next step can be acted
//     on, and only by the role that owns it (or an admin).
//
// The steps are derived, not stored: every fact used here is something the
// case already records (status, dates, eligibility reviews, the signals on
// its timeline, letters sent, invoices). So the list is right for cases that
// were already in flight, and nothing can drift out of step with the case.
//
// Not everything is a step. Things that can happen at any point -- an RFI, a
// withdrawal request, a plan opt-out, a reminder, a dismissal, a correction
// to an eligibility finding before its letter goes out -- stay available
// throughout, under their own role gates.
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type wfStep struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`
	Owner      string   `json:"owner"`       // who does it, in words
	OwnerRoles []string `json:"owner_roles"` // the roles behind that
	Action     string   `json:"action"`      // signal:X | letter:X | qa:X | eligibility | summary | aor_confirm | invoice | settle | ""
	State      string   `json:"state"`       // done | skipped | next | upcoming
	Hint       string   `json:"hint,omitempty"`
	CanAct     bool     `json:"can_act"` // the caller owns this step (or is an admin) AND it is next
}

type wfAct struct {
	Body string
	At   time.Time
}

// wfFacts is everything buildWorkflowSteps needs, read once from the case.
type wfFacts struct {
	Status, Internal string
	Dates            map[string]string
	HasSummary       bool
	HasEstimate      bool
	HasElig          bool
	EligResult       string
	EligAt           time.Time
	AORConfirmedAt   time.Time
	Act              map[string]wfAct  // latest timeline entry per type (signals)
	Letter           map[string]string // template -> status of its latest draft
	HasOutcome       bool              // the Reviewer recorded the outcome and party billed
	ReviewerSends    bool              // an approved draft goes back to its drafter to send
	ReviewInvoice    bool              // an invoice other than the initial fee exists
	OpenInvoices     int
}

var wfOwnerLabels = map[string]string{
	"CASE_MANAGER": "Reviewer", "PM": "PM", "CODER": "Coder", "NURSE_PHYSICIAN": "Nurse / Physician",
	"ATTORNEY": "Attorney", "FINANCE": "Finance",
}

// wfClosedStatuses: Internal Statuses at which nothing further is due.
var wfClosedStatuses = map[string]bool{
	"Provider Closure Letter Issued": true, "Ineligible": true, "Dismissed": true, "Withdrawn": true,
	"Provider - Withdrawal": true, "Plan Opt-Out": true, "Invoice Paid": true, "Decided - Invoice Paid": true,
	"Closed-Refunded": true,
}

// wfStatusReached: for cases that got where they are before the steps were
// tracked, the Internal Status says how far along they are.
var wfStatusReached = map[string]string{
	"Initial Review Pending":            "packet",
	"Provider Acceptance Letter Issued": "qa:acceptance",
	"Plan Notification Packet Issued":   "qa:plan_notification",
	"Review In Progress":                "plan_response",
	"Plan - No Response":                "plan_response",
	"QA Final Determination":            "attorney_review",
	"Determination Sent to FL":          "qa:final_order",
	"Final Order Issued":                "final_order_issued",
}

// buildWorkflowSteps is the pure core: facts in, ordered steps out, with at
// most one step in state "next".
func buildWorkflowSteps(f wfFacts) []wfStep {
	var steps []wfStep
	done := map[string]bool{}
	add := func(key, label, role, action string, isDone bool, hint string) {
		steps = append(steps, wfStep{Key: key, Label: label, Owner: wfOwnerLabels[role], OwnerRoles: []string{role},
			Action: action, Hint: hint})
		done[key] = isDone
	}
	has := func(typ string) bool { _, ok := f.Act[typ]; return ok }
	flag := func(typ, field string) bool { // `"field":true` in the signal's recorded data
		return strings.Contains(strings.ReplaceAll(f.Act[typ].Body, " ", ""), `"`+field+`":true`)
	}
	// A letter is two steps: its owner drafts it, then the QA role approves
	// it, which is what sends it.
	letter := func(tpl, label, drafter, qaRole string) bool {
		st := f.Letter[tpl]
		sent := st == "SENT"
		add("letter:"+tpl, "Draft the "+label, drafter, "letter:"+tpl, sent || st == "PENDING" || st == "APPROVED",
			"Compose it in Correspondence; it is held for QA, not sent.")
		if f.ReviewerSends {
			add("qa:"+tpl, "QA the "+label, qaRole, "qa:"+tpl, sent || st == "APPROVED",
				"Check the parties, the analysis, the letter and the links; correct anything wrong.")
			add("send:"+tpl, "Send the "+label, drafter, "send:"+tpl, sent,
				"Enter the addresses and send. The send is logged and the dates are set.")
			return sent
		}
		add("qa:"+tpl, "QA and send the "+label, qaRole, "qa:"+tpl, sent, "Approve it in the QA gate; approval sends it.")
		return sent
	}

	add("fee", "Initial fee paid", "FINANCE", "settle", f.Status != "AWAITING_PAYMENT",
		"The filer pays by card, or Finance records a check/ACH against the invoice.")
	add("packet", "Confirm the filing packet is complete", "CASE_MANAGER", "signal:PACKET_COMPLETE",
		f.Dates["packet_complete_at"] != "" || has("PACKET_COMPLETE"), "Payment and documents have both arrived.")

	withdrawn := has("PROVIDER_WITHDRAW")
	closing := false // the remaining steps end the case
	switch {
	case withdrawn:
		// handled below, after whatever was already done
	case f.HasElig && f.EligResult == "HOLD_AOR":
		add("eligibility", "Eligibility review", "CASE_MANAGER", "eligibility", true, "On hold: the AOR was recorded as invalid.")
		add("aor_confirm", "Confirm the AOR is invalid", "ATTORNEY", "aor_confirm", f.AORConfirmedAt.After(f.EligAt), "")
		add("aor_revised", "Record the revised AOR", "CASE_MANAGER", "signal:AOR_REVISED", f.Act["AOR_REVISED"].At.After(f.EligAt),
			"Ask the filer for a revised AOR; record it when it arrives.")
		add("eligibility_again", "Run the eligibility review again", "CASE_MANAGER", "eligibility", false, "")
		closing = true // nothing after this is known until the new review
	default:
		add("eligibility", "Eligibility review", "CASE_MANAGER", "eligibility", f.HasElig, "Complete the Eligibility Checklist.")
	}

	estimate := flag("PACKET_COMPLETE", "estimate_requested")
	switch {
	case withdrawn || closing:
	case f.HasElig && f.EligResult == "INELIGIBLE":
		if f.Letter["dismissal"] == "SENT" || f.Letter["dismissal"] == "PENDING" {
			letter("dismissal", "Dismissal letter", "CASE_MANAGER", "ATTORNEY")
		} else {
			letter("ineligible", "Ineligible Dispute letter", "CASE_MANAGER", "PM")
		}
		closing = true
	default:
		// The Coder's Dispute Summary follows the eligibility review, and is
		// only owed on a dispute that is going forward.
		add("dispute_summary", "Complete the Dispute Summary", "CODER", "summary", f.HasSummary,
			"Verify the plan type from the plan's letterhead.")
		letter("acceptance", "Acceptance letter", "CASE_MANAGER", "PM")
		if estimate {
			add("estimate", "Record the full review cost estimate", "CODER", "summary", f.HasEstimate,
				"Pick the figure from the Estimated Cost Matrix (Dispute Summary).")
			letter("estimate_cost", "Full Review Estimated Cost email", "CODER", "PM")
			add("permission", "Record the provider's permission to proceed", "CASE_MANAGER", "signal:ESTIMATE_PERMISSION",
				has("ESTIMATE_PERMISSION"), "15 calendar days; call and reply-all on day 13.")
			if has("ESTIMATE_PERMISSION") && !flag("ESTIMATE_PERMISSION", "granted") {
				letter("dismissal", "Dismissal letter", "CASE_MANAGER", "ATTORNEY")
				closing = true
			}
		}
	}
	if !withdrawn && !closing {
		letter("plan_notification", "Health Plan Notification", "CASE_MANAGER", "PM")
		add("plan_response", "Record the health plan's response", "CASE_MANAGER", "signal:PLAN_RESPONSE_RECEIVED",
			has("PLAN_RESPONSE_RECEIVED") || f.Dates["date_plan_responded"] != "" || f.Internal == "Plan - No Response",
			"15 calendar days from the notification. No response by day 15 becomes a default determination.")
		add("coding_review", "Coding review", "CODER", "signal:CODING_REVIEW_COMPLETE", has("CODING_REVIEW_COMPLETE"),
			"Decide whether clinical review is needed.")
		if flag("CODING_REVIEW_COMPLETE", "clinical_requested") {
			add("clinical_review", "Clinical review", "NURSE_PHYSICIAN", "signal:CLINICAL_REVIEW_COMPLETE",
				has("CLINICAL_REVIEW_COMPLETE"), "Add Clinical Review notes.")
			// Accounts made before NURSE_PHYSICIAN existed hold DOCTOR or NURSE.
			steps[len(steps)-1].OwnerRoles = []string{"NURSE_PHYSICIAN", "DOCTOR", "NURSE"}
		}
		add("attorney_review", "Final rationale and Final Order package", "ATTORNEY", "signal:ATTORNEY_REVIEW_COMPLETE",
			has("ATTORNEY_REVIEW_COMPLETE"), "Enter the rationale; record the outcome.")
		add("invoice", "Create the review invoice", "PM", "invoice", f.ReviewInvoice, "")
		letter("final_order", "Final Order package to AHCA", "CASE_MANAGER", "PM")
		add("final_order_issued", "Record AHCA's Final Order", "CASE_MANAGER", "signal:FINAL_ORDER_ISSUED",
			has("FINAL_ORDER_ISSUED") || f.Dates["date_final_order_sent"] != "", "")
		letter("final_order_rationale_copy", "Final Order Rationale Copy", "CASE_MANAGER", "PM")
		add("outcome", "Record the outcome and party billed", "CASE_MANAGER", "outcome", f.HasOutcome,
			"Case Outcome, Number of Claims Reviewed, Final Amount Awarded, Party Billed.")
		add("payment", "Review costs paid", "FINANCE", "settle",
			f.Internal == "Invoice Paid" || f.Internal == "Decided - Invoice Paid" || (f.ReviewInvoice && f.OpenInvoices == 0),
			"The case closes when the invoices are paid.")
	}
	if withdrawn {
		letter("withdrawal", "Withdrawal letter", "CASE_MANAGER", "PM")
	}

	// How far the case has got: the last step with evidence, or the step its
	// Internal Status implies. Anything before that without evidence was
	// done before steps were tracked.
	reached := -1
	for i, st := range steps {
		mapped := wfStatusReached[f.Internal]
		if done[st.Key] || st.Key == mapped || (strings.HasPrefix(mapped, "qa:") && st.Key == "send:"+strings.TrimPrefix(mapped, "qa:")) {
			reached = i
		}
	}
	closed := wfClosedStatuses[f.Internal]
	nextSet := false
	for i := range steps {
		switch {
		case done[steps[i].Key]:
			steps[i].State = "done"
		case i <= reached || closed:
			steps[i].State = "skipped"
		case !nextSet:
			steps[i].State, nextSet = "next", true
		default:
			steps[i].State = "upcoming"
		}
	}
	return steps
}

// wfAnytime: actions that are never tied to a position in the list.
func wfAnytime(action string) bool {
	switch action {
	case "signal:PROVIDER_WITHDRAW", "signal:OUTREACH_DONE", "signal:RFI_SENT", "signal:RFI_RESPONSE_RECEIVED",
		"signal:PLAN_OPT_OUT", "signal:LETTER_SENT", "signal:ELIGIBILITY_RESULT":
		return true
	}
	return false
}

// wfStagedLetters: templates that ARE steps. Every other template (RFI,
// withdrawal, dismissal, reminders, notices) can be drafted at any time.
var wfStagedLetters = map[string]bool{
	"acceptance": true, "ineligible": true, "estimate_cost": true, "plan_notification": true,
	"final_order": true, "final_order_rationale_copy": true,
}

// wfDecide: may `action` be taken now, given the steps? Returns "" for yes,
// otherwise the reason. Pure.
func wfDecide(steps []wfStep, action string, firstLetterSent bool) string {
	if wfAnytime(action) {
		return ""
	}
	if strings.HasPrefix(action, "letter:") && !wfStagedLetters[strings.TrimPrefix(action, "letter:")] {
		return ""
	}
	var next *wfStep
	staged := false
	for i := range steps {
		if steps[i].State == "next" {
			next = &steps[i]
		}
		if steps[i].Action == action {
			staged = true
		}
	}
	if next != nil && next.Action == action {
		return ""
	}
	// A finding can be corrected until the letter that announces it is out.
	if action == "eligibility" && !firstLetterSent {
		for _, st := range steps {
			if st.Key == "eligibility" && st.State != "upcoming" {
				return ""
			}
		}
	}
	if !staged && !strings.HasPrefix(action, "signal:") && !strings.HasPrefix(action, "letter:") {
		return "" // not something the step list governs
	}
	if next == nil {
		return "this case has no open step: nothing further is due on it"
	}
	return fmt.Sprintf("not the next step on this case — next is %q (%s)", next.Label, next.Owner)
}

func (s *server) workflowFacts(ctx context.Context, tenant, caseID string) (wfFacts, error) {
	f := wfFacts{Dates: map[string]string{}, Act: map[string]wfAct{}, Letter: map[string]string{}}
	tbl := sanitizeTenant(tenant)
	var details, dates map[string]any
	var internal *string
	if err := s.db.QueryRow(ctx, fmt.Sprintf(`
		SELECT status, internal_status, coalesce(details,'{}'::jsonb), coalesce(program_dates,'{}'::jsonb)
		FROM tenant_%s.cases WHERE id::text=$1`, tbl), caseID).Scan(&f.Status, &internal, &details, &dates); err != nil {
		return f, err
	}
	if internal != nil {
		f.Internal = *internal
	}
	for k, v := range dates {
		f.Dates[k] = fmt.Sprint(v)
	}
	if ds, ok := details["dispute_summary"].(map[string]any); ok {
		f.HasSummary = true
		_, f.HasEstimate = ds["full_review_estimate_cents"]
	}
	if by, _ := details["outcome_recorded_by"].(string); by != "" {
		f.HasOutcome = true
	}
	if ac, ok := details["aor_confirmation"].(map[string]any); ok {
		f.AORConfirmedAt, _ = time.Parse(time.RFC3339, fmt.Sprint(ac["at"]))
	}
	if err := s.db.QueryRow(ctx, `
		SELECT result, created_at FROM public.eligibility_reviews
		WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC LIMIT 1`, tenant, caseID).Scan(&f.EligResult, &f.EligAt); err == nil {
		f.HasElig = true
	}
	if rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (type) type, coalesce(body,''), created_at FROM public.case_activities
		WHERE tenant=$1 AND case_id=$2 AND type = ANY($3) ORDER BY type, created_at DESC`, tenant, caseID,
		[]string{"PACKET_COMPLETE", "AOR_REVISED", "ESTIMATE_PERMISSION", "PLAN_RESPONSE_RECEIVED", "CODING_REVIEW_COMPLETE",
			"CLINICAL_REVIEW_COMPLETE", "ATTORNEY_REVIEW_COMPLETE", "FINAL_ORDER_ISSUED", "PROVIDER_WITHDRAW"}); err == nil {
		for rows.Next() {
			var typ string
			var a wfAct
			if rows.Scan(&typ, &a.Body, &a.At) == nil {
				f.Act[typ] = a
			}
		}
		rows.Close()
	}
	// The latest draft of each template decides its state; a rejected draft
	// puts the letter back to "not drafted".
	if rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (artifact) artifact, status FROM public.qa_reviews
		WHERE tenant=$1 AND case_id=$2 AND coalesce(channel,'email')='email' ORDER BY artifact, created_at DESC`, tenant, caseID); err == nil {
		for rows.Next() {
			var tpl, st string
			if rows.Scan(&tpl, &st) == nil {
				f.Letter[tpl] = st
			}
		}
		rows.Close()
	}
	_ = s.db.QueryRow(ctx,
		`SELECT coalesce((config->'correspondence'->>'reviewer_sends')::boolean, false) FROM public.program_rules WHERE tenant=$1`,
		tenant).Scan(&f.ReviewerSends)
	_ = s.db.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE kind <> 'INITIAL_FEE') > 0, count(*) FILTER (WHERE status='OPEN')
		FROM public.invoices WHERE tenant=$1 AND case_id=$2`, tenant, caseID).Scan(&f.ReviewInvoice, &f.OpenInvoices)
	return f, nil
}

func wfIsAdmin(p principal) bool {
	return hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")
}

// wfMayAct: the owner role, or an admin. hasAnyRole would let a tenant admin
// through on its own; it is spelled out here because this is the rule.
func wfMayAct(p principal, st wfStep) bool {
	return wfIsAdmin(p) || hasAnyRole(p, st.OwnerRoles...)
}

// caseWorkflow: GET /cases/{caseId}/workflow — the steps, for the case page.
func (s *server) caseWorkflow(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, append(append([]string{}, caseStaffRoles...), "FINANCE")...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if s.loadProgram(r, tenant) == nil {
		writeJSON(w, http.StatusOK, map[string]any{"steps": []wfStep{}, "programmed": false})
		return
	}
	f, err := s.workflowFacts(r.Context(), tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	steps := buildWorkflowSteps(f)
	for i := range steps {
		steps[i].CanAct = steps[i].State == "next" && wfMayAct(p, steps[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": steps, "programmed": true,
		"closed": wfClosedStatuses[f.Internal], "is_admin": wfIsAdmin(p)})
}

// stepGuard refuses an action that is not the case's next step, or that the
// caller does not own. It writes the response and returns false when it
// refuses. Tenants without a program, and the worker, are not governed.
func (s *server) stepGuard(w http.ResponseWriter, r *http.Request, tenant, caseID, action string) bool {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if contains(p.Roles, serviceRole) || s.loadProgram(r, tenant) == nil {
		return true
	}
	f, err := s.workflowFacts(r.Context(), tenant, caseID)
	if err != nil {
		return true // let the handler answer 404
	}
	steps := buildWorkflowSteps(f)
	firstLetterSent := f.Letter["acceptance"] == "SENT" || f.Letter["ineligible"] == "SENT" || f.Letter["dismissal"] == "SENT"
	if why := wfDecide(steps, action, firstLetterSent); why != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": why})
		return false
	}
	for _, st := range steps {
		if st.State == "next" && st.Action == action && !wfMayAct(p, st) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": fmt.Sprintf("forbidden: %q is done by the %s", st.Label, st.Owner)})
			return false
		}
	}
	return true
}
