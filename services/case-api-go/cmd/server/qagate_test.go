package main

import (
	"sort"
	"strings"
	"testing"
	"time"
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

func TestReviewNoteOwners(t *testing.T) {
	as := func(roles ...string) principal { return principal{Subject: "u", Roles: roles} }
	for _, c := range []struct {
		field string
		p     principal
		want  bool
	}{
		{"internal_case_review", as("CASE_MANAGER"), true}, {"internal_case_review", as("PM"), true},
		{"internal_case_review", as("CODER"), false},
		{"coder_review", as("CODER"), true}, {"coder_review", as("CASE_MANAGER"), false}, {"coder_review", as("PM"), false},
		{"clinical_review", as("NURSE_PHYSICIAN"), true}, {"clinical_review", as("ATTORNEY"), false},
		{"legal_review", as("ATTORNEY"), true}, {"legal_review", as("CODER"), false},
		{"external", as("CASE_MANAGER"), true}, {"external", as("ATTORNEY"), false},
		{"legal_review", as("TENANT_ADMIN"), true}, {"coder_review", as("FEDERAL_ADMIN"), true},
		{"no_such_field", as("PLATFORM_ADMIN"), false}, {"legal_review", as("TPA"), false},
	} {
		if got := mayWriteReviewNote(c.p, c.field); got != c.want {
			t.Errorf("mayWriteReviewNote(%v, %q) = %v, want %v", c.p.Roles, c.field, got, c.want)
		}
	}
}

func TestEligibilityChecklistHTML(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	out := eligibilityChecklistHTML("FL26-000187", "INELIGIBLE", "below_threshold", "R. Reviewer <x>", at,
		map[string]any{"provider_type": "physician_dentist", "threshold_min_cents": 50000, "flags": nil})
	for _, want := range []string{"Eligibility Checklist — FL26-000187", "INELIGIBLE", "below threshold",
		"October 11, 2026", "provider type", "physician_dentist", "R. Reviewer &lt;x&gt;"} {
		if !strings.Contains(out, want) {
			t.Errorf("checklist is missing %q", want)
		}
	}
	if strings.Contains(out, "<x>") {
		t.Error("checklist must escape what people typed")
	}
}
