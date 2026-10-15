package main

import "testing"

func TestCaseActionAllowedWhileFrozen(t *testing.T) {
	cases := []struct {
		lock, action string
		want         bool
	}{
		// correspondence is the one thing always open
		{lockAwaitingPayment, "correspondence", true},
		{lockIneligible, "correspondence", true},
		{lockAwaitingPayment, "lettergen/ineligible_letter", true},
		{lockIneligible, "letters/dismissal", true},
		{lockAwaitingPayment, "calls", true},
		{lockIneligible, "inquiries", true},
		// the way out of INELIGIBLE is a new review; not available while unpaid
		{lockIneligible, "eligibility", true},
		{lockIneligible, "eligibility/auto", true},
		{lockAwaitingPayment, "eligibility", false},
		// everything else is frozen in both states
		{lockAwaitingPayment, "documents", false},
		{lockIneligible, "documents/abc/analysis/retry", false},
		{lockAwaitingPayment, "status", false},
		{lockIneligible, "assign", false},
		{lockIneligible, "signal", false},
		{lockAwaitingPayment, "details", false},
		{lockIneligible, "time", false},
		{lockAwaitingPayment, "share-links", false},
		{lockIneligible, "invoices", false},
		{lockIneligible, "copilot/actions", false},
		{lockIneligible, "opt-out", false},
		{lockIneligible, "correspondence-extra", false}, // prefix match must not leak
	}
	for _, c := range cases {
		if got := caseActionAllowed(c.lock, c.action); got != c.want {
			t.Errorf("%s %q: got %v, want %v", c.lock, c.action, got, c.want)
		}
	}
	if caseLockMessage(lockAwaitingPayment) == "" || caseLockMessage(lockIneligible) == "" || caseLockMessage("") != "" {
		t.Error("lock messages")
	}
}
