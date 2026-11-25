package main

import "testing"

func TestQAAcceptRoles(t *testing.T) {
	for _, c := range []struct {
		role string
		want []string
	}{{"PM", []string{"QA", "PM"}}, {"QA", []string{"QA", "PM"}}, {"ATTORNEY", []string{"ATTORNEY"}}, {"", nil}} {
		got := qaAcceptRoles(c.role)
		if len(got) != len(c.want) {
			t.Fatalf("%q: %v", c.role, got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q: %v", c.role, got)
			}
		}
	}
	qa := principal{Subject: "q", Roles: []string{"QA"}}
	if !hasAnyRole(qa, qaAcceptRoles("PM")...) {
		t.Fatal("a QA may decide a step the templates give the PM")
	}
	if reviewerScoped(qa) {
		t.Fatal("QA sees every case")
	}
	if !hasAnyRole(qa, qaReadRoles...) || !hasAnyRole(qa, caseStaffRoles...) {
		t.Fatal("QA reads the QA gate and the cases")
	}
}

func TestCaseAlertTargets(t *testing.T) {
	team := map[string]string{"CODER": "ada", "QA": "quinn"}
	got := caseAlertTargets(team, "CODER", "NURSE_PHYSICIAN", "QA", "PM", "FINANCE", "CODER")
	want := []string{"role:TENANT_ADMIN", "role:CASE_MANAGER", "role:PM", "ada", "quinn", "role:FINANCE"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// Nobody on the case: only the Tenant Admin, the Admin and the PM -- never every Coder in the program.
	for _, x := range caseAlertTargets(nil, "CODER", "ATTORNEY", "DOCTOR") {
		if x == "role:CODER" || x == "role:ATTORNEY" || x == "role:NURSE_PHYSICIAN" {
			t.Fatalf("a specialist role was alerted as a whole: %v", x)
		}
	}
}
