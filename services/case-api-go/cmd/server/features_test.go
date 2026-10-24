// Author: Patrick Munis at newwave
package main

import "testing"

// The flag vocabulary is deliberately closed: a typo'd flag must be rejected
// at write time, never silently gate nothing. Pins the registered set and
// the admin-only write gate.
func TestFeatureFlagVocabulary(t *testing.T) {
	want := []string{"sealed_offers", "review_queues", "party_workspace", "benchmarks",
		"batch_suggestions", "bundling_filter", "necessity_screen",
		"deadline_verification", "duplicate_detection", "win_probability",
		"payer_analytics", "fraud_signals", "network_adequacy", "letter_citation"}
	for _, k := range want {
		if !registeredFeatures[k] {
			t.Errorf("feature %q not registered", k)
		}
	}
	if registeredFeatures["blockchain_reconciliation"] {
		t.Error("unregistered feature accepted")
	}
}

func TestFeatureAdminGate(t *testing.T) {
	gate := []string{"PLATFORM_ADMIN", "FEDERAL_ADMIN"}
	cases := []struct {
		roles []string
		want  bool
	}{
		{[]string{"PLATFORM_ADMIN"}, true},
		{[]string{"FEDERAL_ADMIN"}, true},
		{[]string{"CASE_MANAGER"}, false}, // tenant staff never toggle their own tenant's features
		{[]string{"STATE_AUDITOR"}, false},
		{[]string{"PARTY"}, false},
	}
	for _, c := range cases {
		if got := hasAnyRole(principal{Roles: c.roles}, gate...); got != c.want {
			t.Errorf("roles=%v: got %v, want %v", c.roles, got, c.want)
		}
	}
}

func TestStoredFlagsOverlayTheProgramSettings(t *testing.T) {
	got := mergeFlags([]byte(`{"benchmarks":false,"fraud_signals":false}`), []byte(`{"benchmarks":true}`), []byte(`not json`))
	if got["benchmarks"] != true || got["fraud_signals"] != false || len(got) != 2 {
		t.Errorf("tenant_features wins over program settings; got %v", got)
	}
	if len(mergeFlags(nil, nil)) != 0 {
		t.Errorf("nothing stored: no flags")
	}
}

// The tenant's step rules: with step order off any step not done can be taken; with step owners off anyone can.
func TestStepRules(t *testing.T) {
	next := wfStep{State: "next", OwnerRoles: []string{"FINANCE"}}
	later := wfStep{State: "upcoming", OwnerRoles: []string{"FINANCE"}}
	done := wfStep{State: "done", OwnerRoles: []string{"FINANCE"}}
	reviewer := principal{Roles: []string{"CASE_MANAGER"}}
	strict, loose := wfRules{order: true, owner: true}, wfRules{order: false, owner: false}
	if !strict.open(next) || strict.open(later) || strict.open(done) {
		t.Error("step order on: only the next step is open")
	}
	if !loose.open(next) || !loose.open(later) || loose.open(done) {
		t.Error("step order off: every step not done is open")
	}
	if strict.mayAct(reviewer, next) {
		t.Error("step owners on: a Reviewer may not take Finance's step")
	}
	if !loose.mayAct(reviewer, next) {
		t.Error("step owners off: anyone may take it")
	}
	for _, k := range []string{"step_order", "step_owner", "payment_lock", "ineligible_lock"} {
		if !registeredFeatures[k] {
			t.Errorf("%s is not a registered feature", k)
		}
	}
}
