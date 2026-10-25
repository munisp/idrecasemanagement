// Author: Patrick Munis at newwave
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
	ReportSaved      bool            // the decision report has been saved
	Generated        map[string]bool // generated letter (lettergen template) -> a draft not rejected in QA
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
	// Steps whose evidence may come early: the decision report and its letters
	// can be saved and generated at any point (the Attorney and the Coder
	// write their parts during their own steps), so their being done says
	// nothing about how far the case has got.
	early := map[string]bool{}
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

	estimate := wfEstimateRequested(f)
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
		// Whether the filer asked for a cost estimate decides the path (with an estimate: estimate, permission,
		// then the plan's notice; without: the acceptance and the plan's notice together, by day 13). A case whose
		// acceptance letter was drafted before this step existed has been past it.
		add("estimate_confirm", "Confirm if Cost Estimate requested", "CASE_MANAGER", "signal:ESTIMATE_CONFIRMED",
			has("ESTIMATE_CONFIRMED") || f.Letter["acceptance"] != "", "The filing packet's “estimate requested” box.")
		steps[len(steps)-1].OwnerRoles = []string{"CASE_MANAGER", "PM"}
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
	optedOut := wfOptedOut(f)
	if !withdrawn && !closing {
		letter("plan_notification", "Health Plan Notification", "CASE_MANAGER", "PM")
		add("plan_response", "Record the health plan's response", "CASE_MANAGER", "signal:PLAN_RESPONSE_RECEIVED",
			has("PLAN_RESPONSE_RECEIVED") || f.Dates["date_plan_responded"] != "" || f.Internal == "Plan - No Response" ||
				f.Dates["plan_no_response_at"] != "" || optedOut,
			"15 calendar days from the notification. No response by day 15 becomes a default determination.")
		if optedOut {
			// The plan opted out with its supporting documentation: the Opt-Out Determination Letter closes the case.
			letter("plan_opt_out", "Opt-Out Determination Letter", "CASE_MANAGER", "PM")
			closing = true
		}
	}
	// Default (SOP 7): the plan's 15 days ran out with no response. The determination is a default for the provider --
	// no Coder, Nurse or Attorney review -- and goes to AHCA like any determination: the Default Determination,
	// its cover letter and the default invoice, as the Final Order package.
	if !withdrawn && !closing && wfDefaulted(f) {
		add("invoice", "Create the default invoice", "PM", "invoice", f.ReviewInvoice, "The health plan is billed: a default.")
		add("report_default", "Generate the Default Determination", "CASE_MANAGER", "report:default_determination",
			f.Generated["default_determination"], "The plan did not respond within its 15 days: a default determination for the provider.")
		add("report_cover_letter", "Generate the cover letter", "CASE_MANAGER", "report:cover_letter_final_notice",
			f.Generated["cover_letter_final_notice"], "")
		add("report_plan_invoice", "Generate the health plan invoice", "CASE_MANAGER", "report:invoice_health_plan",
			f.Generated["invoice_health_plan"], "")
		for i := len(steps) - 3; i < len(steps); i++ {
			early[steps[i].Key] = true
			steps[i].OwnerRoles = []string{"CASE_MANAGER", "PM"}
		}
		letter("final_order", "Final Order package to AHCA", "CASE_MANAGER", "PM")
		add("final_order_issued", "Record AHCA's Final Order", "CASE_MANAGER", "signal:FINAL_ORDER_ISSUED",
			has("FINAL_ORDER_ISSUED") || f.Dates["date_final_order_sent"] != "", "")
		letter("final_order_rationale_copy", "Final Order copy to the parties", "CASE_MANAGER", "PM")
		add("outcome", "Record the outcome and party billed", "CASE_MANAGER", "outcome", f.HasOutcome,
			"Default Award, the health plan billed.")
		add("payment", "Review costs paid", "FINANCE", "settle",
			f.Internal == "Invoice Paid" || f.Internal == "Decided - Invoice Paid" || (f.ReviewInvoice && f.OpenInvoices == 0),
			"The case closes when the invoices are paid.")
		closing = true
	}
	if !withdrawn && !closing {
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
		// The decision report, then the documents made from it, before the
		// Final Order package that carries them.
		add("report_save", "Save the decision report", "CASE_MANAGER", "report:save", f.ReportSaved,
			"Enter the outcome and amounts, paste or upload the report, and save.")
		add("report_generate", "Generate the decision report", "CASE_MANAGER", "report:final_order_rationale",
			f.Generated["final_order_rationale"], "Generate the pasted report, or upload the finished one; it goes under Documents and to the QA gate.")
		add("report_cover_letter", "Generate the cover letter", "CASE_MANAGER", "report:cover_letter_final_notice",
			f.Generated["cover_letter_final_notice"], "")
		add("report_plan_invoice", "Generate the health plan invoice", "CASE_MANAGER", "report:invoice_health_plan",
			f.Generated["invoice_health_plan"], "")
		// The Reviewer and the PM may both edit the whole report.
		for i := len(steps) - 4; i < len(steps); i++ {
			early[steps[i].Key] = true
			steps[i].OwnerRoles = []string{"CASE_MANAGER", "PM"}
		}
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
		add("withdrawal_confirm", "Confirm the withdrawal request", "CASE_MANAGER", "signal:WITHDRAWAL_CONFIRMED",
			has("WITHDRAWAL_CONFIRMED") || f.Letter["withdrawal"] != "", "Verify the request with the party who asked.")
		steps[len(steps)-1].OwnerRoles = []string{"CASE_MANAGER", "PM"}
		letter("withdrawal", "Withdrawal letter", "CASE_MANAGER", "PM")
	}

	// How far the case has got, for steps with no evidence of their own: the
	// step its Internal Status implies. Anything before that was done before
	// steps were tracked (a case migrated mid-way), so it reads skipped.
	// Only the status says so -- not a later step done out of order. With
	// "follow the steps in order" off, a specialist can finish a later step
	// first; that must not mark the Acceptance letter, the Health Plan
	// Notification or the plan's response as passed. (It did: a coding review
	// done early skipped 8 steps, so a case could reach a decision without
	// the plan ever being notified.)
	reached := -1
	mapped := wfStatusReached[f.Internal]
	for i, st := range steps {
		if st.Key == mapped || (strings.HasPrefix(mapped, "qa:") && st.Key == "send:"+strings.TrimPrefix(mapped, "qa:")) {
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

// wfFlag: `"field":true` in the data a signal recorded.
func wfFlag(f wfFacts, typ, field string) bool {
	return strings.Contains(strings.ReplaceAll(f.Act[typ].Body, " ", ""), `"`+field+`":true`)
}

// wfEstimateRequested: what the Reviewer confirmed (Confirm if Cost Estimate requested), or for a case from
// before that step, the answer recorded with the filing packet.
func wfEstimateRequested(f wfFacts) bool {
	if _, ok := f.Act["ESTIMATE_CONFIRMED"]; ok {
		return wfFlag(f, "ESTIMATE_CONFIRMED", "requested")
	}
	return wfFlag(f, "PACKET_COMPLETE", "estimate_requested")
}

// wfDefaulted: the plan never responded within its 15 days (the workflow set "Plan - No Response", which case-api
// keeps as plan_no_response_at). A response logged after the 15 days doesn't undo it (SOP 7: no response by then is
// a default); an opt-out within them means it was never a default.
func wfDefaulted(f wfFacts) bool {
	if wfOptedOut(f) {
		return false
	}
	return f.Dates["plan_no_response_at"] != "" || f.Internal == "Plan - No Response" || f.Generated["default_determination"]
}

// wfOptedOut: the plan exercised a valid opt-out (the Attorney found it eligible, or its letter is under way).
func wfOptedOut(f wfFacts) bool {
	return f.Internal == "Plan Opt-Out" || wfFlag(f, "PLAN_OPT_OUT", "eligible") || f.Letter["plan_opt_out"] != "" ||
		strings.HasPrefix(f.Act["OPT_OUT_DECISION"].Body, "Opt-out eligibility: true")
}

// wfAnytime: actions that are never tied to a position in the list.
func wfAnytime(action string) bool {
	switch action {
	case "signal:PROVIDER_WITHDRAW", "signal:OUTREACH_DONE", "signal:RFI_SENT", "signal:RFI_RESPONSE_RECEIVED",
		"signal:PLAN_OPT_OUT", "signal:LETTER_SENT", "signal:ELIGIBILITY_RESULT", "signal:REFUND_APPROVED", "signal:WITHDRAWAL_CONFIRMED":
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
			"CLINICAL_REVIEW_COMPLETE", "ATTORNEY_REVIEW_COMPLETE", "FINAL_ORDER_ISSUED", "PROVIDER_WITHDRAW",
			"ESTIMATE_CONFIRMED", "WITHDRAWAL_CONFIRMED", "PLAN_OPT_OUT", "OPT_OUT_DECISION", "RFI_SENT"}); err == nil {
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
	_ = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.decision_reports WHERE tenant=$1 AND case_id=$2)`,
		tenant, caseID).Scan(&f.ReportSaved)
	f.Generated = map[string]bool{}
	if rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (artifact) artifact, status FROM public.qa_reviews
		WHERE tenant=$1 AND case_id=$2 AND channel='letter' ORDER BY artifact, created_at DESC`, tenant, caseID); err == nil {
		for rows.Next() {
			var tpl, st string
			if rows.Scan(&tpl, &st) == nil {
				f.Generated[tpl] = st != "REJECTED"
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

// wfRules is the tenant's step rules (features.go): step_order -- only the next step can be acted on;
// step_owner -- only its owner role (or an admin) can act on it. Both on unless a tenant admin turns them off.
type wfRules struct{ order, owner bool }

func (s *server) stepRules(r *http.Request, tenant string) wfRules {
	return wfRules{order: s.featureEnabled(r, tenant, "step_order"), owner: s.featureEnabled(r, tenant, "step_owner")}
}

// open: the step can be acted on at all -- the next one, or with step order off any step not done yet.
func (x wfRules) open(st wfStep) bool {
	if x.order {
		return st.State == "next"
	}
	return st.State != "done"
}

// mayAct: its owner or an admin; with step owners off, anyone who may work the case.
func (x wfRules) mayAct(p principal, st wfStep) bool {
	return !x.owner || wfMayAct(p, st)
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
	s.ensureStepHelper(r, tenant, caseID) // cases already at a specialist's step get one too (helpers.go)
	steps := buildWorkflowSteps(f)
	rules := s.stepRules(r, tenant)
	for i := range steps {
		steps[i].CanAct = rules.open(steps[i]) && rules.mayAct(p, steps[i])
	}
	out := map[string]any{"steps": steps, "programmed": true, "closed": wfClosedStatuses[f.Internal], "is_admin": wfIsAdmin(p),
		"step_order": rules.order, "step_owner": rules.owner}
	// The document's numbered steps for this case's workflow, each with the countdown of the clock that times it.
	doc := buildDocSteps(f, steps)
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		views := map[string]ClockView{}
		for _, v := range s.programClockViews(r, tenant, caseID, cfg) {
			views[v.Clock] = v
		}
		var invDue *time.Time
		_ = s.db.QueryRow(r.Context(), `SELECT min(due_date) FROM public.invoices WHERE tenant=$1 AND case_id=$2
			AND kind NOT IN ('INITIAL_FEE','LATE_FEE') AND status='OPEN'`, tenant, caseID).Scan(&invDue)
		for i := range doc.Steps {
			if v, ok := views[doc.Steps[i].Clock]; ok {
				doc.Steps[i].View = &v
			}
			if doc.Steps[i].Title == "Invoice cycle begins" && invDue != nil {
				doc.Steps[i].Due = invDue.Format("2006-01-02")
			}
		}
	}
	out["doc"] = doc
	// Whether the case's workflow can still take its steps (workflow_alive.go); left out when Temporal can't be asked.
	var caseNumber string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
	if caseNumber != "" {
		// An open case's steps stay open even while its workflow isn't running: taking one starts the workflow again
		// (signalCaseWorkflow, workflow_alive.go). Only a closed case's steps are shut.
		if running, known := s.workflowRunning(r.Context(), s.caseWorkflowID(r, tenant, caseNumber)); known && !running && wfClosedStatuses[f.Internal] {
			out["workflow_running"] = false
			for i := range steps {
				steps[i].CanAct = false
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
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
	rules := s.stepRules(r, tenant)
	firstLetterSent := f.Letter["acceptance"] == "SENT" || f.Letter["ineligible"] == "SENT" || f.Letter["dismissal"] == "SENT"
	if rules.order {
		if why := wfDecide(steps, action, firstLetterSent); why != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": why})
			return false
		}
	}
	for _, st := range steps {
		if st.Action == action && rules.open(st) && !rules.mayAct(p, st) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": fmt.Sprintf("forbidden: %q is done by the %s", st.Label, st.Owner)})
			return false
		}
	}
	return true
}
