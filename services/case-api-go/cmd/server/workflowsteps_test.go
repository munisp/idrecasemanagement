// Author: Patrick Munis at newwave
package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func wfBase() wfFacts {
	return wfFacts{Status: "PENDING_INTAKE", Dates: map[string]string{}, Act: map[string]wfAct{}, Letter: map[string]string{}}
}

func wfNext(steps []wfStep) *wfStep {
	var next *wfStep
	n := 0
	for i := range steps {
		if steps[i].State == "next" {
			next = &steps[i]
			n++
		}
	}
	if n > 1 {
		panic("more than one next step")
	}
	return next
}

func TestWorkflowStepsWalk(t *testing.T) {
	f := wfBase()
	f.Status = "AWAITING_PAYMENT"
	expect := func(key, owner string) {
		t.Helper()
		n := wfNext(buildWorkflowSteps(f))
		if n == nil || n.Key != key || n.Owner != owner {
			t.Fatalf("next step: got %+v, want %s (%s)", n, key, owner)
		}
	}
	now := time.Now()
	expect("fee", "Finance")
	f.Status = "PENDING_INTAKE"
	expect("packet", "Admin")
	f.Act["PACKET_COMPLETE"] = wfAct{Body: `Workflow signal PACKET_COMPLETE delivered — {"estimate_requested":false}`, At: now}
	expect("eligibility", "Admin")
	f.HasElig, f.EligResult, f.EligAt = true, "ELIGIBLE", now
	expect("dispute_summary", "Coder")
	f.HasSummary = true
	expect("estimate_confirm", "Admin")
	f.Act["ESTIMATE_CONFIRMED"] = wfAct{Body: `Workflow signal ESTIMATE_CONFIRMED delivered — {"requested":false}`, At: now}
	expect("letter:acceptance", "Admin")
	f.Letter["acceptance"] = "PENDING"
	expect("qa:acceptance", "PM")
	f.Letter["acceptance"] = "REJECTED" // back to the Admin
	expect("letter:acceptance", "Admin")
	f.Letter["acceptance"] = "SENT"
	expect("letter:plan_notification", "Admin")
	f.Letter["plan_notification"] = "SENT"
	expect("plan_response", "Admin")
	f.Act["PLAN_RESPONSE_RECEIVED"] = wfAct{At: now}
	expect("full_review", "Coder") // SOP step 7, recorded by the Coder (2026-11-26)
	f.Act["FULL_REVIEW_INITIATED"] = wfAct{At: now}
	expect("coding_review", "Coder")
	f.Act["CODING_REVIEW_COMPLETE"] = wfAct{Body: `{"clinical_requested": true}`, At: now}
	expect("clinical_review", "Nurse Reviewer")
	f.Act["CLINICAL_REVIEW_COMPLETE"] = wfAct{At: now}
	expect("attorney_review", "Attorney")
	f.Act["ATTORNEY_REVIEW_COMPLETE"] = wfAct{At: now}
	expect("invoice", "PM")
	f.ReviewInvoice, f.OpenInvoices = true, 1
	expect("report_save", "Admin")
	f.ReportSaved = true
	f.Generated = map[string]bool{}
	expect("report_generate", "Admin")
	f.Generated["final_order_rationale"] = true
	expect("report_cover_letter", "Admin")
	f.Generated["cover_letter_final_notice"] = true
	expect("report_plan_invoice", "Admin")
	f.Generated["invoice_health_plan"] = false // rejected in QA: generate it again
	expect("report_plan_invoice", "Admin")
	f.Generated["invoice_health_plan"] = true
	expect("letter:final_order", "Admin")
	f.Letter["final_order"] = "SENT"
	// Change log 2026-11-25 #20: the outcome and party billed close the Final Order package step.
	expect("outcome", "Admin")
	f.HasOutcome = true
	expect("final_order_issued", "Admin")
	f.Act["FINAL_ORDER_ISSUED"] = wfAct{At: now}
	expect("payment", "Finance")
	// #19: the Final Order Rationale Copy is not a step unless a party asks for it.
	if strings.Contains(fmt.Sprint(buildWorkflowSteps(f)), "final_order_rationale_copy") {
		t.Fatal("the rationale copy is a step though nobody asked for it")
	}
	f.CopyRequested = true
	f.OpenInvoices, f.Internal = 0, "Invoice Paid"
	steps := buildWorkflowSteps(f)
	if !strings.Contains(fmt.Sprint(steps), "letter:final_order_rationale_copy") {
		t.Fatal("a requested copy is not listed")
	}
	f.Letter["final_order_rationale_copy"] = "SENT"
	if n := wfNext(buildWorkflowSteps(f)); n != nil {
		t.Fatalf("a closed case has no next step, got %+v", n)
	}
}

func TestWorkflowStepsBranches(t *testing.T) {
	now := time.Now()
	keys := func(steps []wfStep) string {
		var ks []string
		for _, s := range steps {
			ks = append(ks, s.Key)
		}
		return strings.Join(ks, " ")
	}
	// Ineligible: the Ineligible letter, then nothing.
	f := wfBase()
	f.Act["PACKET_COMPLETE"], f.HasSummary = wfAct{At: now}, true
	f.HasElig, f.EligResult, f.EligAt = true, "INELIGIBLE", now
	steps := buildWorkflowSteps(f)
	if n := wfNext(steps); n == nil || n.Key != "letter:ineligible" {
		t.Fatalf("ineligible: next should be the Ineligible letter, got %+v", n)
	}
	if strings.Contains(keys(steps), "plan_notification") {
		t.Error("an ineligible case has no plan notification step")
	}
	// AOR hold: Attorney confirms, Admin records the revision, then re-review.
	f.EligResult = "HOLD_AOR"
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "aor_confirm" || n.Owner != "Attorney" {
		t.Fatalf("AOR hold: next should be the Attorney's confirmation, got %+v", n)
	}
	f.AORConfirmedAt = now.Add(time.Minute)
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "aor_revised" {
		t.Fatalf("AOR hold: next should be recording the revised AOR, got %+v", n)
	}
	f.Act["AOR_REVISED"] = wfAct{At: now.Add(2 * time.Minute)}
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "eligibility_again" {
		t.Fatalf("AOR hold: next should be the new eligibility review, got %+v", n)
	}
	// Estimate requested: Coder's estimate and email, then the provider's permission.
	f = wfBase()
	f.Act["PACKET_COMPLETE"] = wfAct{Body: `{"estimate_requested":true}`, At: now}
	f.HasSummary, f.HasElig, f.EligResult = true, true, "ELIGIBLE"
	f.Letter["acceptance"] = "SENT"
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "estimate" || n.Owner != "Coder" {
		t.Fatalf("estimate: next should be the Coder's estimate, got %+v", n)
	}
	f.HasEstimate = true
	f.Letter["estimate_cost"] = "SENT"
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "permission" {
		t.Fatalf("estimate: next should be the provider's permission, got %+v", n)
	}
	f.Act["ESTIMATE_PERMISSION"] = wfAct{Body: `{"granted":false}`, At: now}
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "letter:dismissal" {
		t.Fatalf("permission declined: next should be the Dismissal letter, got %+v", n)
	}
	// Withdrawal replaces what is left with its letter.
	f = wfBase()
	f.Act["PACKET_COMPLETE"], f.HasSummary = wfAct{At: now}, true
	f.Act["PROVIDER_WITHDRAW"] = wfAct{At: now}
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "withdrawal_confirm" {
		t.Fatalf("withdrawal: next should be confirming the request, got %+v", n)
	}
	f.Act["WITHDRAWAL_CONFIRMED"] = wfAct{At: now}
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "letter:withdrawal" {
		t.Fatalf("withdrawal: next should be the Withdrawal letter, got %+v", n)
	}
	// A case already in review before steps were tracked: earlier steps are
	// skipped, not demanded again.
	f = wfBase()
	f.Internal = "Review In Progress"
	steps = buildWorkflowSteps(f)
	if n := wfNext(steps); n == nil || n.Key != "coding_review" {
		t.Fatalf("legacy case in review: next should be the coding review, got %+v", n)
	}
	for _, s := range steps {
		if s.Key == "dispute_summary" && s.State != "skipped" {
			t.Errorf("legacy case: the Dispute Summary should read skipped, got %s", s.State)
		}
	}
}

func TestWorkflowDecide(t *testing.T) {
	now := time.Now()
	f := wfBase()
	f.Act["PACKET_COMPLETE"] = wfAct{At: now}
	steps := buildWorkflowSteps(f) // next: the Admin's eligibility review
	for action, wantOK := range map[string]bool{
		"eligibility":                   true,  // the next step
		"summary":                       false, // a later step
		"signal:CODING_REVIEW_COMPLETE": false,
		"letter:acceptance":             false,
		"letter:plan_notification":      false,
		"invoice":                       false,
		"signal:PROVIDER_WITHDRAW":      true, // any time
		"signal:RFI_RESPONSE_RECEIVED":  true,
		"letter:rfi":                    true, // not a stage letter
		"letter:withdrawal":             true,
		"letter:payment_reminder":       true,
		"review_notes":                  true, // not governed by the steps
	} {
		why := wfDecide(steps, action, false)
		if (why == "") != wantOK {
			t.Errorf("%s: allowed=%v, want %v (%s)", action, why == "", wantOK, why)
		}
		if why != "" && !strings.Contains(why, "Eligibility review") {
			t.Errorf("%s: the refusal should name the next step: %q", action, why)
		}
	}
	// A finding can be corrected until its letter is out, not after.
	f.HasSummary, f.HasElig, f.EligResult = true, true, "INELIGIBLE"
	steps = buildWorkflowSteps(f)
	if why := wfDecide(steps, "eligibility", false); why != "" {
		t.Errorf("a review should be correctable before its letter is sent: %s", why)
	}
	if why := wfDecide(steps, "eligibility", true); why == "" {
		t.Error("a review must not be redone after its letter has been sent")
	}
}

func TestWorkflowOwnerOrAdmin(t *testing.T) {
	st := wfStep{OwnerRoles: []string{"CODER"}}
	for roles, want := range map[string]bool{"CODER": true, "TENANT_ADMIN": true, "FEDERAL_ADMIN": true, "PLATFORM_ADMIN": true,
		"CASE_MANAGER": false, "PM": false, "ATTORNEY": false, "FINANCE": false} {
		if got := wfMayAct(principal{Roles: []string{roles}}, st); got != want {
			t.Errorf("%s acting on the Coder's step: got %v, want %v", roles, got, want)
		}
	}
}

func TestChecklistItemOwner(t *testing.T) {
	as := func(r string) principal { return principal{Roles: []string{r}} }
	for _, c := range []struct {
		role, owner string
		want        bool
	}{
		{"CODER", "CODER", true}, {"CASE_MANAGER", "CODER", false}, {"PM", "CODER", false},
		{"TENANT_ADMIN", "CODER", true}, {"FEDERAL_ADMIN", "ATTORNEY", true},
		{"FINANCE", "FINANCE", true}, {"CASE_MANAGER", "FINANCE", false},
		{"CASE_MANAGER", "", true}, // no owner configured: the old rule applies
	} {
		if got := mayCheckItem(as(c.role), c.owner); got != c.want {
			t.Errorf("%s ticking a %q item: got %v, want %v", c.role, c.owner, got, c.want)
		}
	}
}

// With the hand-back, a letter is three steps: draft, QA, send -- and the
// send belongs to the drafter, not the approver.
func TestWorkflowLetterHandBack(t *testing.T) {
	f := wfBase()
	f.ReviewerSends = true
	f.Act["PACKET_COMPLETE"], f.HasSummary = wfAct{At: time.Now()}, true
	f.Act["ESTIMATE_CONFIRMED"] = wfAct{Body: `{"requested":false}`, At: time.Now()}
	f.HasElig, f.EligResult = true, "ELIGIBLE"
	for st, want := range map[string][2]string{
		"": {"letter:acceptance", "Admin"}, "PENDING": {"qa:acceptance", "PM"},
		"APPROVED": {"send:acceptance", "Admin"}, "REJECTED": {"letter:acceptance", "Admin"},
		"SENT": {"letter:plan_notification", "Admin"},
	} {
		f.Letter["acceptance"] = st
		n := wfNext(buildWorkflowSteps(f))
		if n == nil || n.Key != want[0] || n.Owner != want[1] {
			t.Errorf("acceptance letter %q: next is %+v, want %s (%s)", st, n, want[0], want[1])
		}
	}
	// A case already past the letter by status does not ask for it again.
	f = wfBase()
	f.ReviewerSends, f.Internal = true, "Provider Acceptance Letter Issued"
	if n := wfNext(buildWorkflowSteps(f)); n == nil || n.Key != "letter:plan_notification" {
		t.Errorf("legacy case past acceptance: next is %+v, want the plan notification draft", n)
	}
}

// The clinical step belongs to the Nurse / Physician, and to accounts made
// with the older DOCTOR or NURSE roles, which the Team page used to offer.
func TestClinicalStepOwners(t *testing.T) {
	f := wfBase()
	f.Act["CODING_REVIEW_COMPLETE"] = wfAct{Body: `{"clinical_requested":true}`, At: time.Now()}
	var st *wfStep
	for i, s := range buildWorkflowSteps(f) {
		if s.Key == "clinical_review" {
			ss := buildWorkflowSteps(f)[i]
			st = &ss
		}
	}
	if st == nil {
		t.Fatal("no clinical review step when the Coder asked for one")
	}
	for _, role := range []string{"NURSE_PHYSICIAN", "DOCTOR", "NURSE"} {
		if !wfMayAct(principal{Roles: []string{role}}, *st) {
			t.Errorf("%s should be able to act on the clinical review step", role)
		}
	}
	if wfMayAct(principal{Roles: []string{"CODER"}}, *st) {
		t.Error("a Coder must not act on the clinical review step")
	}
}

// The decision report can be saved and its letters generated early (the
// Attorney and the Coder write their parts during their own steps): that
// marks those steps done but does not skip the steps before them.
func TestWorkflowDecisionReportEarly(t *testing.T) {
	f := wfBase()
	now := time.Now()
	f.Act["PACKET_COMPLETE"] = wfAct{At: now}
	f.HasElig, f.EligResult, f.EligAt, f.HasSummary = true, "ELIGIBLE", now, true
	f.Letter["acceptance"], f.Letter["plan_notification"] = "SENT", "SENT"
	f.Act["PLAN_RESPONSE_RECEIVED"] = wfAct{At: now}
	f.Act["FULL_REVIEW_INITIATED"] = wfAct{At: now}
	f.ReportSaved = true
	f.Generated = map[string]bool{"final_order_rationale": true}
	steps := buildWorkflowSteps(f)
	if n := wfNext(steps); n == nil || n.Key != "coding_review" {
		t.Fatalf("next step: got %+v, want coding_review", n)
	}
	for _, st := range steps {
		if st.Key == "report_save" && st.State != "done" {
			t.Fatalf("the saved report shows as done, got %s", st.State)
		}
		if (st.Key == "attorney_review" || st.Key == "invoice") && st.State != "upcoming" {
			t.Fatalf("%s must not be skipped by an early report, got %s", st.Key, st.State)
		}
	}
	for _, st := range steps {
		if strings.HasPrefix(st.Key, "report_") && strings.Join(st.OwnerRoles, ",") != "CASE_MANAGER,PM" {
			t.Fatalf("%s is the Admin's or the PM's, got %v", st.Key, st.OwnerRoles)
		}
	}
}

// A later step done out of order (step order off: the Coder completed the coding review from the queue) leaves the
// earlier steps open: the Acceptance letter and the Health Plan Notification are still owed, not "skipped".
func TestOutOfOrderStepSkipsNothing(t *testing.T) {
	now := time.Now()
	f := wfBase()
	f.Internal = "Initial Review Pending"
	f.Act["PACKET_COMPLETE"] = wfAct{At: now}
	f.HasElig, f.EligResult, f.EligAt = true, "ELIGIBLE", now
	f.Act["CODING_REVIEW_COMPLETE"] = wfAct{Body: `{"clinical_requested":true}`, At: now}
	f.Act["CLINICAL_REVIEW_COMPLETE"] = wfAct{At: now}
	steps := buildWorkflowSteps(f)
	if n := wfNext(steps); n == nil || n.Key != "dispute_summary" {
		t.Fatalf("next should still be the Dispute Summary, got %+v", n)
	}
	for _, st := range steps {
		switch st.Key {
		case "letter:acceptance", "qa:acceptance", "letter:plan_notification", "qa:plan_notification", "plan_response":
			if st.State == "skipped" || st.State == "done" {
				t.Errorf("%s must still be owed, got %s", st.Key, st.State)
			}
		case "coding_review", "clinical_review":
			if st.State != "done" {
				t.Errorf("%s was done, got %s", st.Key, st.State)
			}
		}
	}
}

// Change log 2026-11-25 #13: with a cost estimate requested, the Cost Estimate then the acceptance, both through QA,
// the second approval sending both.
func TestWorkflowEstimatePair(t *testing.T) {
	now := time.Now()
	f := wfBase()
	f.Status = "PENDING_INTAKE"
	f.Act["PACKET_COMPLETE"] = wfAct{Body: `{"estimate_requested":true}`, At: now}
	f.HasElig, f.EligResult, f.EligAt, f.HasSummary = true, "ELIGIBLE", now, true
	f.Act["ESTIMATE_CONFIRMED"] = wfAct{Body: `Workflow signal ESTIMATE_CONFIRMED delivered — {"requested":true}`, At: now}
	next := func() string {
		if n := wfNext(buildWorkflowSteps(f)); n != nil {
			return n.Key
		}
		return ""
	}
	want := func(k string) {
		t.Helper()
		if got := next(); got != k {
			t.Fatalf("next: got %s, want %s", got, k)
		}
	}
	want("estimate")
	f.HasEstimate = true
	want("letter:estimate_cost")
	f.Letter["estimate_cost"] = "PENDING"
	want("letter:acceptance")
	f.Letter["acceptance"] = "PENDING"
	want("qa:estimate_cost")
	f.Letter["estimate_cost"] = "APPROVED" // held for the acceptance
	want("qa:acceptance")
	f.Letter["estimate_cost"], f.Letter["acceptance"] = "SENT", "SENT"
	if strings.HasPrefix(next(), "qa:") || strings.HasPrefix(next(), "letter:acceptance") {
		t.Fatalf("both sent, still on %s", next())
	}
}
