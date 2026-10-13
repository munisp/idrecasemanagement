package main

import "testing"

// The flag vocabulary is deliberately closed: a typo'd flag must be rejected
// at write time, never silently gate nothing. Pins the registered set and
// the admin-only write gate.
func TestFeatureFlagVocabulary(t *testing.T) {
	want := []string{"sealed_offers", "review_queues", "party_workspace", "benchmarks",
		"batch_suggestions", "bundling_filter", "necessity_screen",
		"deadline_verification", "duplicate_detection"}
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
