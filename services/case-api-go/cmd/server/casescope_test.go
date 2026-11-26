package main

import (
	"net/http"
	"strings"
	"testing"
)

const (
	scopeCase = "11111111-1111-1111-1111-111111111111"
	scopeQA   = "22222222-2222-2222-2222-222222222222"
)

// Least privilege (change log #14): the specialists see only the cases they work; the PM, the Admin and the rest
// of seeAllCaseRoles see every case.
func TestReviewerScoped(t *testing.T) {
	for _, c := range []struct {
		roles []string
		want  bool
	}{
		{[]string{"CODER"}, true},
		{[]string{"NURSE_PHYSICIAN"}, true},
		{[]string{"ATTORNEY"}, true},
		{[]string{"QA"}, true},
		{[]string{"CODER", "QA", "offline_access"}, true},
		{[]string{"CASE_MANAGER"}, false},
		{[]string{"PM"}, false},
		{[]string{"TENANT_ADMIN"}, false},          // a Tenant Admin counts as every staff role in hasAnyRole: must not be scoped
		{[]string{"CODER", "CASE_MANAGER"}, false}, // an Admin who also codes sees every case
		{[]string{"ATTORNEY", "PM"}, false},
		{[]string{"FINANCE"}, false},
		{[]string{"TPA"}, false}, // outside accounts have their own rule (accessDecision's own-case)
	} {
		if got := reviewerScoped(principal{Subject: "me", Roles: c.roles}); got != c.want {
			t.Errorf("reviewerScoped(%v) = %v, want %v", c.roles, got, c.want)
		}
	}
}

func TestCaseVisibleTo(t *testing.T) {
	coder := principal{Subject: "me", Roles: []string{"CODER"}}
	qa := principal{Subject: "me", Roles: []string{"QA"}}
	pm := principal{Subject: "pm", Roles: []string{"PM"}}
	for _, c := range []struct {
		name    string
		p       principal
		to      string
		named   []string
		qaNamed bool
		want    bool
	}{
		{"coder named on the case", coder, "admin", []string{"me"}, true, true},
		{"coder assigned the case", coder, "me", nil, true, true},
		{"coder not on the case", coder, "admin", []string{"someone"}, false, false},
		{"coder, no QA named, still not theirs", coder, "", nil, false, false},
		{"QA named", qa, "admin", []string{"me"}, true, true},
		{"QA, another QA named", qa, "admin", []string{"other-qa"}, true, false},
		{"QA, no QA named yet", qa, "admin", nil, false, true},
		{"PM sees every case", pm, "someone-else", nil, true, true},
	} {
		if got := caseVisibleTo(c.p, c.to, c.named, c.qaNamed); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAccessDecisionScopesReviewers(t *testing.T) {
	rev := principal{Subject: "me", Roles: []string{"CODER"}}
	pm := principal{Subject: "pm", Roles: []string{"PM"}}
	admin := principal{Subject: "ad", Roles: []string{"CASE_MANAGER"}}
	for _, c := range []struct {
		p               principal
		method, rel     string
		decision, case_ string
	}{
		{rev, http.MethodGet, "/cases/" + scopeCase, "assigned-case", scopeCase},
		{rev, http.MethodPost, "/cases/" + scopeCase + "/signal", "assigned-case", scopeCase},
		{rev, http.MethodGet, "/cases/" + scopeCase + "/documents/" + scopeQA + "/download", "assigned-case", scopeCase},
		{rev, http.MethodPost, "/qa/" + scopeQA + "/send", "qa-case", scopeQA},
		{rev, http.MethodPost, "/invoices/" + scopeQA + "/checkout", "invoice-case", scopeQA},
		{rev, http.MethodPost, "/graph/ask", "deny", ""},
		{rev, http.MethodGet, "/cases", "allow", ""}, // the list scopes itself in SQL
		{pm, http.MethodGet, "/cases/" + scopeCase, "allow", ""},
		{pm, http.MethodPost, "/graph/ask", "allow", ""},
		{admin, http.MethodGet, "/cases/" + scopeCase, "allow", ""},
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
	if cond, ok := caseScopeSQL(principal{Roles: []string{"PM"}}, "fl", "c", 3); ok || cond != "" {
		t.Fatal("a PM's lists are not scoped")
	}
	cond, ok := caseScopeSQL(principal{Roles: []string{"CODER"}}, "fl", "c", 3)
	if !ok || !strings.Contains(cond, "c.assigned_to = $3") || !strings.Contains(cond, "public.case_helpers") ||
		!strings.Contains(cond, "public.case_review_team") || strings.Contains(cond, "hq.role='QA'") {
		t.Fatalf("coder scope: %q", cond)
	}
	cond, _ = caseScopeSQL(principal{Roles: []string{"QA"}}, "fl", "c", 3)
	if !strings.Contains(cond, "hq.role='QA'") {
		t.Fatalf("a QA also sees cases with no QA named: %q", cond)
	}
}

func TestAdminsSeeEveryCase(t *testing.T) {
	if reviewerScoped(principal{Roles: []string{"CASE_MANAGER"}}) {
		t.Fatal("since 2026-11-25 the Admin sees every case")
	}
}
