package main

import (
	"net/http"
	"testing"
)

const (
	scopeCase = "11111111-1111-1111-1111-111111111111"
	scopeQA   = "22222222-2222-2222-2222-222222222222"
)

func TestReviewerScoped(t *testing.T) {
	for _, c := range []struct {
		roles []string
		want  bool
	}{
		{[]string{"CASE_MANAGER"}, true},
		{[]string{"CASE_MANAGER", "offline_access"}, true},
		{[]string{"PM"}, false},
		{[]string{"TENANT_ADMIN"}, false}, // a Tenant Admin counts as every staff role in hasAnyRole: must not be scoped
		{[]string{"CASE_MANAGER", "PM"}, false},
		{[]string{"CASE_MANAGER", "CODER"}, false}, // their coding queue spans every case
		{[]string{"CODER"}, false},
		{[]string{"FINANCE"}, false},
		{[]string{"TPA"}, false}, // outside accounts have their own rule (accessDecision's own-case)
	} {
		if got := reviewerScoped(principal{Subject: "me", Roles: c.roles}); got != c.want {
			t.Errorf("reviewerScoped(%v) = %v, want %v", c.roles, got, c.want)
		}
	}
}

func TestCaseVisibleTo(t *testing.T) {
	rev := principal{Subject: "me", Roles: []string{"CASE_MANAGER"}}
	pm := principal{Subject: "pm", Roles: []string{"PM"}}
	for _, c := range []struct {
		p    principal
		to   string
		want bool
	}{
		{rev, "", false}, {rev, "unassigned-pool", false}, {rev, "me", true}, {rev, "someone-else", false},
		{pm, "someone-else", true}, {pm, "", true},
	} {
		if got := caseVisibleTo(c.p, c.to); got != c.want {
			t.Errorf("caseVisibleTo(%v, %q) = %v, want %v", c.p.Roles, c.to, got, c.want)
		}
	}
}

func TestAccessDecisionScopesReviewers(t *testing.T) {
	rev := principal{Subject: "me", Roles: []string{"CASE_MANAGER"}}
	pm := principal{Subject: "pm", Roles: []string{"PM"}}
	for _, c := range []struct {
		p              principal
		method, rel    string
		decision, case_ string
	}{
		{rev, http.MethodGet, "/cases/" + scopeCase, "assigned-case", scopeCase},
		{rev, http.MethodPost, "/cases/" + scopeCase + "/eligibility", "assigned-case", scopeCase},
		{rev, http.MethodGet, "/cases/" + scopeCase + "/documents/" + scopeQA + "/download", "assigned-case", scopeCase},
		{rev, http.MethodPost, "/qa/" + scopeQA + "/send", "qa-case", scopeQA},
		{rev, http.MethodPost, "/invoices/" + scopeQA + "/checkout", "invoice-case", scopeQA},
		{rev, http.MethodPost, "/graph/ask", "deny", ""},
		{rev, http.MethodGet, "/cases", "allow", ""}, // the list scopes itself in SQL
		{pm, http.MethodGet, "/cases/" + scopeCase, "allow", ""},
		{pm, http.MethodPost, "/graph/ask", "allow", ""},
	} {
		d, id, _ := accessDecision(c.p, c.method, c.rel)
		if d != c.decision || id != c.case_ {
			t.Errorf("%v %s %s: got (%s, %s), want (%s, %s)", c.p.Roles, c.method, c.rel, d, id, c.decision, c.case_)
		}
	}
}

func TestPruneHiddenCases(t *testing.T) {
	doc := map[string]any{
		"tasks": []any{
			map[string]any{"id": "t1", "case_id": "keep"},
			map[string]any{"id": "t2", "case_id": "hide"},
			map[string]any{"id": "t3"}, // a task on no case stays
		},
		"nested": map[string]any{"rows": []any{map[string]any{"case_id": "hide"}, "plain"}},
		"total":  3,
	}
	var ids []string
	collectCaseIDs(doc, &ids)
	if len(ids) != 3 {
		t.Fatalf("case ids collected: %v", ids)
	}
	out := pruneHiddenCases(doc, map[string]bool{"hide": true}).(map[string]any)
	if n := len(out["tasks"].([]any)); n != 2 {
		t.Fatalf("tasks kept: %d, want 2", n)
	}
	rows := out["nested"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0] != "plain" {
		t.Fatalf("nested rows: %v", rows)
	}
}

func TestCaseScopeSQL(t *testing.T) {
	if cond, ok := caseScopeSQL(principal{Roles: []string{"PM"}}, "c", 3); ok || cond != "" {
		t.Fatal("a PM's lists are not scoped")
	}
	cond, ok := caseScopeSQL(principal{Roles: []string{"CASE_MANAGER"}}, "c", 3)
	if !ok || cond != " AND c.assigned_to = $3" {
		t.Fatalf("reviewer scope: %q", cond)
	}
}
