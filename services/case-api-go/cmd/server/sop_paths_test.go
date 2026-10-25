package main

import (
	"strings"
	"testing"
)

// The SOP's closures each carry their outcome.
func TestClosureOutcome(t *testing.T) {
	for _, c := range []struct {
		internal                  string
		dismissalSent, ineligible bool
		want                      string
	}{
		{"Provider Closure Letter Issued", false, true, "Ineligible - Closed"},
		{"Provider Closure Letter Issued", true, false, "Dismissed"},
		{"Ineligible", false, false, "Ineligible - Closed"},
		{"Dismissed", true, false, "Dismissed"},
		{"Provider - Withdrawal", false, false, "Withdrawn"},
		{"Withdrawn", false, false, "Withdrawn"},
		{"Plan Opt-Out", false, false, "Plan Opt-Out"},
		{"Closed-Refunded", false, false, "Refunded"},
		{"Review In Progress", false, false, ""},
		{"Final Order Issued", false, false, ""},
	} {
		if got := closureOutcome(c.internal, c.dismissalSent, c.ineligible); got != c.want {
			t.Errorf("closureOutcome(%q) = %q, want %q", c.internal, got, c.want)
		}
	}
	if !unsetOutcome("") || !unsetOutcome("TBD - In Process") || unsetOutcome("Full Award") {
		t.Error("unsetOutcome: only empty and TBD are unset")
	}
}

// A full review can't end in a Default Award, nor a default in a Full Award; Other fits both.
func TestOutcomesFollowThePath(t *testing.T) {
	for _, c := range []struct {
		outcome   string
		isDefault bool
		ok        bool
	}{
		{"Full Award", false, true}, {"Partial Award", false, true}, {"No Award", false, true},
		{"Default Award", false, false}, {"Default Award", true, true},
		{"Full Award", true, false}, {"Partial Award", true, false},
		{"Other", true, true}, {"Other", false, true},
		{"Costs Resolved Prior to Final Determination (Pre-Settlement)", false, true},
		{"Withdrawn", false, false}, {"Ineligible - Closed", true, false}, {"Plan Opt-Out", false, false},
	} {
		if got := outcomeAllowed(c.outcome, c.isDefault); got != c.ok {
			t.Errorf("outcomeAllowed(%q, default=%v) = %v, want %v", c.outcome, c.isDefault, got, c.ok)
		}
	}
}

// Withdrawn: after acceptance and prior to final determination.
func TestWithdrawalWindow(t *testing.T) {
	if msg := withdrawalRefusal("", ""); !strings.Contains(msg, "Close and refund") {
		t.Errorf("before acceptance: %q", msg)
	}
	if msg := withdrawalRefusal("PENDING", ""); msg == "" {
		t.Error("an Acceptance letter still in QA hasn't gone: refused")
	}
	if msg := withdrawalRefusal("SENT", ""); msg != "" {
		t.Errorf("accepted, no determination: allowed, got %q", msg)
	}
	if msg := withdrawalRefusal("SENT", "PENDING"); msg != "" {
		t.Errorf("the package still in QA hasn't gone to AHCA: allowed, got %q", msg)
	}
	if msg := withdrawalRefusal("SENT", "SENT"); !strings.Contains(msg, "AHCA") {
		t.Errorf("after the determination went: %q", msg)
	}
}

// SOP 7: no response from the plan by day 15 is a default -- no Coder, Nurse or Attorney review; the Default
// Determination goes to AHCA as the Final Order package.
func TestDefaultPathSkipsTheReviews(t *testing.T) {
	f := wfFacts{Internal: "QA Final Determination", Dates: map[string]string{"packet_complete_at": "2026-10-01", "plan_no_response_at": "2026-10-20"},
		HasElig: true, EligResult: "ELIGIBLE", HasSummary: true,
		Act:    map[string]wfAct{"PACKET_COMPLETE": {}, "ESTIMATE_CONFIRMED": {Body: `{"requested":false}`}},
		Letter: map[string]string{"acceptance": "SENT", "plan_notification": "SENT"}, Generated: map[string]bool{}}
	keys := map[string]bool{}
	for _, st := range buildWorkflowSteps(f) {
		keys[st.Key] = true
	}
	for _, k := range []string{"coding_review", "clinical_review", "attorney_review", "report_save", "report_generate"} {
		if keys[k] {
			t.Errorf("a default case has no %s step", k)
		}
	}
	for _, k := range []string{"plan_response", "invoice", "report_default", "letter:final_order", "final_order_issued", "outcome", "payment"} {
		if !keys[k] {
			t.Errorf("a default case has a %s step", k)
		}
	}
	for _, st := range buildWorkflowSteps(f) {
		if st.Key == "plan_response" && st.State != "done" {
			t.Errorf("the plan's response step is done on a default (it lapsed): %s", st.State)
		}
	}
	// A late response logged after the 15 days doesn't undo the default.
	f.Act["PLAN_RESPONSE_RECEIVED"] = wfAct{}
	if !wfDefaulted(f) {
		t.Error("a response after the lapse doesn't undo a default")
	}
	// The plan responded in time: a full review.
	g := f
	g.Dates = map[string]string{"packet_complete_at": "2026-10-01", "date_plan_responded": "2026-10-10"}
	g.Internal = "Review In Progress"
	if wfDefaulted(g) {
		t.Error("a plan that responded in time is not a default")
	}
	full := map[string]bool{}
	for _, st := range buildWorkflowSteps(g) {
		full[st.Key] = true
	}
	if !full["coding_review"] || !full["attorney_review"] || full["report_default"] {
		t.Error("a full review keeps the Coder and Attorney reviews and has no Default Determination")
	}
}

func TestReviewInvoiceFollowsThePath(t *testing.T) {
	if reviewInvoiceKindProblem("DEFAULT", true) != "" || reviewInvoiceKindProblem("FULL_REVIEW", false) != "" {
		t.Error("the right kind for each path passes")
	}
	if reviewInvoiceKindProblem("FULL_REVIEW", true) == "" || reviewInvoiceKindProblem("DEFAULT", false) == "" {
		t.Error("the wrong kind for the path is refused")
	}
	if reviewInvoiceKindProblem("INITIAL_FEE", true) != "" || reviewInvoiceKindProblem("ADMIN_FEE", false) != "" {
		t.Error("non-review invoices pass")
	}
}

// SOP 4: the Attorney allowing the opt-out puts the Opt-Out Determination Letter next; the case closes when it's sent.
func TestOptOutLetterFollowsTheDecision(t *testing.T) {
	f := wfFacts{Internal: "Plan Notification Packet Issued", Dates: map[string]string{"packet_complete_at": "2026-10-01"},
		HasElig: true, EligResult: "ELIGIBLE", HasSummary: true,
		Act: map[string]wfAct{"PACKET_COMPLETE": {}, "ESTIMATE_CONFIRMED": {Body: `{"requested":false}`},
			"OPT_OUT_DECISION": {Body: "Opt-out eligibility: true — self-funded ERISA plan (by attorney)"}},
		Letter: map[string]string{"acceptance": "SENT", "plan_notification": "SENT"}, Generated: map[string]bool{}}
	next := ""
	for _, st := range buildWorkflowSteps(f) {
		if st.State == "next" {
			next = st.Key
		}
	}
	if next != "letter:plan_opt_out" {
		t.Errorf("after the Attorney allows the opt-out the Opt-Out letter is next, got %q", next)
	}
	f.Act["OPT_OUT_DECISION"] = wfAct{Body: "Opt-out eligibility: false — not self-funded (by attorney)"}
	if wfOptedOut(f) {
		t.Error("a refused opt-out isn't an opt-out")
	}
}

func TestAwardsCantOutliveAClosure(t *testing.T) {
	for o, want := range map[string]bool{"Full Award": true, "Partial Award": true, "No Award": true, "Default Award": true,
		"Other": false, "Costs Resolved Prior to Final Determination (Pre-Settlement)": false, "Dismissed": false} {
		if awardOutcome(o) != want {
			t.Errorf("awardOutcome(%q) = %v", o, !want)
		}
	}
}
