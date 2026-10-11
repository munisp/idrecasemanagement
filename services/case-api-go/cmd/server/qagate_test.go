package main

import (
	"sort"
	"testing"
)

func TestQAInternalStatus(t *testing.T) {
	for tpl, want := range map[string]string{
		"acceptance": qaStatusInitialReview, "ineligible": qaStatusInitialReview, "dismissal": qaStatusInitialReview,
		"plan_notification": qaStatusLetter, "final_order": qaStatusLetter, "rfi": qaStatusLetter,
		"withdrawal": qaStatusLetter, "payment_reminder": qaStatusLetter,
	} {
		if got := qaInternalStatus(tpl); got != want {
			t.Errorf("qaInternalStatus(%q) = %q, want %q", tpl, got, want)
		}
	}
}

func TestNotifTargets(t *testing.T) {
	has := func(xs []string, v string) bool { return contains(xs, v) }
	pm := notifTargets(principal{Subject: "u1", Roles: []string{"PM"}})
	for _, want := range []string{"u1", "*", "role:PM"} {
		if !has(pm, want) {
			t.Errorf("PM should read %q; got %v", want, pm)
		}
	}
	if has(pm, "role:ATTORNEY") {
		t.Errorf("PM must not read the Attorney's notices: %v", pm)
	}
	reviewer := notifTargets(principal{Subject: "u2", Roles: []string{"CASE_MANAGER"}})
	if has(reviewer, "role:PM") {
		t.Errorf("a reviewer must not read the PM's QA notices: %v", reviewer)
	}
	admin := notifTargets(principal{Subject: "u3", Roles: []string{"TENANT_ADMIN"}})
	sort.Strings(admin)
	for _, want := range []string{"role:PM", "role:ATTORNEY", "role:CODER"} {
		if !has(admin, want) {
			t.Errorf("tenant admin stands in for every staff role, missing %q", want)
		}
	}
}

func TestCanonStatus(t *testing.T) {
	list := []string{"QA Letter", "Determination Sent to FL", "Final Order Issued"}
	if got, ok := canonStatus(list, "Determination sent to FL"); !ok || got != "Determination Sent to FL" {
		t.Errorf("case-insensitive match should return the program's spelling, got %q %v", got, ok)
	}
	if got, ok := canonStatus(list, " QA Letter "); !ok || got != "QA Letter" {
		t.Errorf("surrounding space should not matter, got %q %v", got, ok)
	}
	if _, ok := canonStatus(list, "Closed-Refunded"); ok {
		t.Error("a status the program does not list must still be refused")
	}
}
