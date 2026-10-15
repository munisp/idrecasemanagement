package main

import (
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
	expect("packet", "Reviewer")
	f.Act["PACKET_COMPLETE"] = wfAct{Body: `Workflow signal PACKET_COMPLETE delivered — {"estimate_requested":false}`, At: now}
	expect("eligibility", "Reviewer")
	f.HasElig, f.EligResult, f.EligAt = true, "ELIGIBLE", now
	expect("dispute_summary", "Coder")
	f.HasSummary = true
	expect("letter:acceptance", "Reviewer")
	f.Letter["acceptance"] = "PENDING"
	expect("qa:acceptance", "PM")
	f.Letter["acceptance"] = "REJECTED" // back to the Reviewer
	expect("letter:acceptance", "Reviewer")
	f.Letter["acceptance"] = "SENT"
	expect("letter:plan_notification", "Reviewer")
	f.Letter["plan_notification"] = "SENT"
	expect("plan_response", "Reviewer")
	f.Act["PLAN_RESPONSE_RECEIVED"] = wfAct{At: now}
	expect("coding_review", "Coder")
	f.Act["CODING_REVIEW_COMPLETE"] = wfAct{Body: `{"clinical_requested": true}`, At: now}
	expect("clinical_review", "Nurse / Physician")
	f.Act["CLINICAL_REVIEW_COMPLETE"] = wfAct{At: now}
	expect("attorney_review", "Attorney")
	f.Act["ATTORNEY_REVIEW_COMPLETE"] = wfAct{At: now}
	expect("invoice", "PM")
	f.ReviewInvoice, f.OpenInvoices = true, 1
	expect("letter:final_order", "Reviewer")
	f.Letter["final_order"] = "SENT"
	expect("final_order_issued", "Reviewer")
	f.Act["FINAL_ORDER_ISSUED"] = wfAct{At: now}
	expect("letter:final_order_rationale_copy", "Reviewer")
	f.Letter["final_order_rationale_copy"] = "SENT"
	expect("outcome", "Reviewer")
	f.HasOutcome = true
	expect("payment", "Finance")
	f.OpenInvoices, f.Internal = 0, "Invoice Paid"
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
	// AOR hold: Attorney confirms, Reviewer records the revision, then re-review.
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
	steps := buildWorkflowSteps(f) // next: the Reviewer's eligibility review
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
	f.HasElig, f.EligResult = true, "ELIGIBLE"
	for st, want := range map[string][2]string{
		"": {"letter:acceptance", "Reviewer"}, "PENDING": {"qa:acceptance", "PM"},
		"APPROVED": {"send:acceptance", "Reviewer"}, "REJECTED": {"letter:acceptance", "Reviewer"},
		"SENT": {"letter:plan_notification", "Reviewer"},
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
