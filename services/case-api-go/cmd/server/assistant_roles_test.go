package main

import (
	"slices"
	"testing"
)

// The assistants are one more way to read and act on the same data, so each
// follows the caller's role exactly as the screens do.

func TestGraphAskIsForStaffOnly(t *testing.T) {
	for _, role := range []string{"CASE_MANAGER", "PM", "CODER", "ATTORNEY", "FINANCE", "TENANT_ADMIN", "FEDERAL_ADMIN"} {
		if !mayAskGraph(principal{Roles: []string{role}}) {
			t.Errorf("%s should be able to ask the graph", role)
		}
	}
	for _, roles := range [][]string{{"TPA"}, {"PARTY"}, {}, {"TPA", "CASE_MANAGER"}, {"PARTY", "PM"}} {
		if mayAskGraph(principal{Roles: roles}) {
			t.Errorf("%v must not be able to ask the graph", roles)
		}
	}
}

func TestGraphFiguresFollowTheScreens(t *testing.T) {
	cases := []struct {
		role       string
		has, hasNo []string
	}{
		// The Reviewer has no Reports, Ops or money screens.
		{"CASE_MANAGER", []string{"caseload", "services"}, []string{"parties", "eligibility", "outcomes", "deadlines", "money"}},
		{"CODER", []string{"caseload", "services"}, []string{"parties", "eligibility", "deadlines", "money"}},
		{"ATTORNEY", []string{"caseload", "services"}, []string{"parties", "deadlines", "money"}},
		// Finance reads money and Ops, not the program reports.
		{"FINANCE", []string{"caseload", "money", "deadlines"}, []string{"parties", "eligibility", "outcomes"}},
		{"PM", []string{"caseload", "services", "parties", "eligibility", "outcomes", "deadlines", "money"}, nil},
		{"TENANT_ADMIN", []string{"parties", "eligibility", "outcomes", "deadlines", "money"}, nil},
		{"FEDERAL_ADMIN", []string{"parties", "eligibility", "outcomes", "deadlines", "money"}, nil},
	}
	for _, c := range cases {
		got := graphFactCategories(principal{Roles: []string{c.role}})
		for _, want := range c.has {
			if !slices.Contains(got, want) {
				t.Errorf("%s should get %s figures, got %v", c.role, want, got)
			}
		}
		for _, no := range c.hasNo {
			if slices.Contains(got, no) {
				t.Errorf("%s must not get %s figures, got %v", c.role, no, got)
			}
		}
	}
}

func TestAssistantDraftsTakeTheProgramsQAGate(t *testing.T) {
	fl := []CorrTemplate{{Key: "acceptance", QARole: "PM"}, {Key: "plan_notification", QARole: "PM"},
		{Key: "dismissal", QARole: "ATTORNEY"}, {Key: "submission_instructions"}}
	if got := defaultQARole(fl); got != "PM" {
		t.Errorf("FL's QA gate is the PM's, got %q", got)
	}
	// A tenant whose templates name no approving role has no gate to apply.
	if got := defaultQARole([]CorrTemplate{{Key: "a"}, {Key: "b"}}); got != "" {
		t.Errorf("no gate expected, got %q", got)
	}
	if got := defaultQARole(nil); got != "" {
		t.Errorf("no templates, no gate; got %q", got)
	}
}
