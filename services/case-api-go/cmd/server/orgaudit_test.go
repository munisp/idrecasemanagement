// Author: Patrick Munis at newwave
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

// An entry belongs to the person who did it; machine actors belong to no one.
func TestAuditActor(t *testing.T) {
	for pl, want := range map[string]string{"u-1": "u-1", "system": "", "system:bulk-filer": "", "stripe": "", "": ""} {
		if got := auditActor(map[string]any{"by": pl}); got != want {
			t.Errorf("auditActor(by=%q) = %q, want %q", pl, got, want)
		}
	}
}

// Every action reads as words in the trails, known or not.
func TestActionLabel(t *testing.T) {
	if l := actionLabel("POST /cases/{caseId}/eligibility"); l != "Recorded an eligibility review" {
		t.Errorf("known staff action: %q", l)
	}
	if l := actionLabel("POST /tpa/disputes/bulk"); l != "Uploaded a bulk filing" {
		t.Errorf("known org action: %q", l)
	}
	if l := actionLabel("DELETE /cases/{caseId}/notes/{noteId}"); l != "Deleted: cases › notes" {
		t.Errorf("fallback: %q", l)
	}
	for _, p := range []string{"/cases/x/documents/y/download", "/hp/responding/x/documents/y/file", "/cases/x/documents.zip"} {
		if !isDocumentRead(p) {
			t.Errorf("%s hands over a document", p)
		}
	}
	if isDocumentRead("/cases/x") {
		t.Error("opening a case is not a document read")
	}
}
