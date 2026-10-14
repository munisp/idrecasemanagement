package main

import "testing"

// An entry belongs to the organization it names, so that organization's own
// audit log shows it.
func TestAuditOrg(t *testing.T) {
	cases := []struct {
		payload map[string]any
		want    string
	}{
		{map[string]any{"org": "plan:p-1", "tpa_id": "t-9"}, "plan:p-1"},
		{map[string]any{"tpa_id": "t-1", "client_id": "c-1"}, "tpa:t-1"},
		{map[string]any{"plan_id": "p-2"}, "plan:p-2"},
		{map[string]any{"by": "u-1"}, ""},
	}
	for _, c := range cases {
		if got := auditOrg(c.payload); got != c.want {
			t.Errorf("auditOrg(%v) = %q, want %q", c.payload, got, c.want)
		}
	}
}

func TestEveryOrgRouteHasWords(t *testing.T) {
	for _, a := range []string{"POST /tpa/disputes/bulk", "POST /hp/responding/{caseId}/submit", "POST /tpa/fees/checkout",
		"GET /hp/responding/{caseId}/documents/{docId}/file", "POST /hp/claim",
		"POST /tpa/disputes/{caseId}/pay", "POST /tpa/clients/{clientId}/fees/checkout", "POST /tpa/fees/checkout/{groupId}/cancel"} {
		if orgActionLabels[a] == "" {
			t.Errorf("%s has no label for the audit log", a)
		}
	}
}
