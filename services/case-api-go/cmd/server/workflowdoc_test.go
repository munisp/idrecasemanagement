package main

import (
	"strings"
	"testing"
	"time"
)

func docTitles(d wfDoc) []string {
	var out []string
	for _, s := range d.Steps {
		out = append(out, s.Title)
	}
	return out
}

func docStep(d wfDoc, title string) *wfDocStep {
	for i := range d.Steps {
		if d.Steps[i].Title == title {
			return &d.Steps[i]
		}
	}
	return nil
}

func TestDocStepsFollowTheDocument(t *testing.T) {
	now := time.Now()
	f := wfBase()
	f.Act["PACKET_COMPLETE"] = wfAct{At: now}
	f.HasElig, f.EligResult, f.HasSummary = true, "ELIGIBLE", true
	f.Act["ESTIMATE_CONFIRMED"] = wfAct{Body: `{"requested":true}`, At: now}
	d := buildDocSteps(f, buildWorkflowSteps(f))
	if d.Workflow != "1. Acceptance Workflow (Cost Estimate Requested)" {
		t.Fatalf("workflow %q", d.Workflow)
	}
	want := []string{"Eligibility Review", "Case deemed Eligible", "Confirm if Cost Estimate requested",
		"Acceptance and Cost Estimate issued", "Provider submits authorization/permission", "Health Plan Notification sent",
		"Health Plan response period", "Health Plan submits required documentation"}
	got := strings.Join(docTitles(d), " | ")
	if !strings.Contains(got, strings.Join(want, " | ")) {
		t.Fatalf("steps:\n%s\nwant in order:\n%s", got, strings.Join(want, " | "))
	}
	if s := docStep(d, "Eligibility Review"); s.N != 1 || s.Clock != "INITIAL_REVIEW" || s.Phase != phase1 {
		t.Errorf("eligibility: %+v", s)
	}
	if s := docStep(d, "Confirm if Cost Estimate requested"); s.N != 3 || s.Note != "Cost estimate requested: Yes" {
		t.Errorf("confirm: %+v", s)
	}
	if s := docStep(d, "Acceptance and Cost Estimate issued"); s.State != "next" || !strings.Contains(strings.Join(s.Keys, ","), "estimate") {
		t.Errorf("acceptance: %+v", s)
	}
	if s := docStep(d, "Provider submits authorization/permission"); s.Clock != "PROVIDER_PERMISSION" || s.Detail != "Within 15 calendar days" {
		t.Errorf("permission: %+v", s)
	}
	if s := docStep(d, "Attorney review"); s == nil || s.Phase != phase3 {
		t.Errorf("full review phases missing: %+v", s)
	}

	// No cost estimate: the acceptance and the plan's notice together, by day 13.
	f.Act["ESTIMATE_CONFIRMED"] = wfAct{Body: `{"requested":false}`, At: now}
	d = buildDocSteps(f, buildWorkflowSteps(f))
	if d.Workflow != "2. Acceptance Workflow (No Cost Estimate Requested)" {
		t.Fatalf("workflow %q", d.Workflow)
	}
	if s := docStep(d, "Acceptance and Health Plan Notification issued"); s == nil || s.N != 4 || s.Clock != "ACCEPTANCE_PLAN_NOTICE" {
		t.Errorf("no-estimate acceptance: %+v", s)
	}
	if docStep(d, "Provider submits authorization/permission") != nil {
		t.Error("no permission step without a cost estimate")
	}
}

func TestDocStepsClosingWorkflows(t *testing.T) {
	now := time.Now()
	base := func() wfFacts {
		f := wfBase()
		f.Act["PACKET_COMPLETE"] = wfAct{At: now}
		f.HasElig, f.EligResult, f.HasSummary = true, "ELIGIBLE", true
		f.Act["ESTIMATE_CONFIRMED"] = wfAct{Body: `{"requested":true}`, At: now}
		f.Letter["acceptance"], f.Letter["estimate_cost"], f.HasEstimate = "SENT", "SENT", true
		return f
	}
	f := wfBase()
	f.Act["PACKET_COMPLETE"] = wfAct{At: now}
	f.HasElig, f.EligResult = true, "INELIGIBLE"
	d := buildDocSteps(f, buildWorkflowSteps(f))
	if d.Workflow != "3. Ineligible Workflow" || strings.Join(docTitles(d), "|") != "Initial fee paid and filing packet complete|Eligibility Review|Case deemed Ineligible|Ineligibility Letter issued|Case closed" {
		t.Errorf("ineligible: %s %v", d.Workflow, docTitles(d))
	}
	f = base()
	f.Act["ESTIMATE_PERMISSION"] = wfAct{Body: `{"granted":false}`, At: now}
	d = buildDocSteps(f, buildWorkflowSteps(f))
	if d.Workflow != "6. Dismissal Workflow" || docStep(d, "Dismissal Letter issued") == nil || docStep(d, "No authorization received") == nil {
		t.Errorf("dismissal: %s %v", d.Workflow, docTitles(d))
	}
	f = base()
	f.Act["PROVIDER_WITHDRAW"] = wfAct{At: now}
	d = buildDocSteps(f, buildWorkflowSteps(f))
	if d.Workflow != "5. Withdrawal Workflow" || docStep(d, "Withdrawal request verified and confirmed by Capitol Bridge").State != "next" {
		t.Errorf("withdrawal: %s %v", d.Workflow, docTitles(d))
	}
	f = base()
	f.Act["ESTIMATE_PERMISSION"] = wfAct{Body: `{"granted":true}`, At: now}
	f.Letter["plan_notification"] = "SENT"
	f.Act["PLAN_OPT_OUT"] = wfAct{Body: `{"eligible":true}`, At: now}
	d = buildDocSteps(f, buildWorkflowSteps(f))
	if d.Workflow != "4. Plan Opt-Out Workflow" || docStep(d, "Opt-Out Determination Letter issued").State != "next" {
		t.Errorf("opt-out: %s %v", d.Workflow, docTitles(d))
	}
	f = base()
	f.Act["ESTIMATE_PERMISSION"] = wfAct{Body: `{"granted":true}`, At: now}
	f.Letter["plan_notification"], f.Internal = "SENT", "Plan - No Response"
	d = buildDocSteps(f, buildWorkflowSteps(f))
	if d.Workflow != "7. Default Workflow" || docStep(d, "Default Determination issued") == nil {
		t.Errorf("default: %s %v", d.Workflow, docTitles(d))
	}
}
