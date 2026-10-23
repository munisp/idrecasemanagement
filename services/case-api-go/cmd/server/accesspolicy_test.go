package main

import (
	"net/http"
	"testing"
)

const caseA = "2d756871-3a59-4d8c-a346-f28ab547038c"
const docA = "c37ea445-470b-4120-accb-a8209f89a6e1"

func who(roles ...string) principal { return principal{Subject: "u-1", Roles: roles} }

func TestExternalOnly(t *testing.T) {
	for _, roles := range [][]string{{"TPA"}, {"TPA", "BULK_SUBMITTER"}, {"TPA", "PARTY"}, {"HEALTH_PLAN"}, {"PARTY"}, {}, {"default-roles-idre", "offline_access"}} {
		if !isExternalOnly(who(roles...)) {
			t.Errorf("%v is an outside account", roles)
		}
	}
	// A staff role alongside it makes the account staff (an admin who claimed a TPA code to help it).
	for _, roles := range [][]string{{"CASE_MANAGER"}, {"HEALTH_PLAN", "PM"}, {"TPA", "FEDERAL_ADMIN"}, {"TPA", "TENANT_ADMIN"}, {"PM"}, {"STATE_AUDITOR"}, {serviceRole}, {"PARTY", "CASE_MANAGER"}} {
		if isExternalOnly(who(roles...)) {
			t.Errorf("%v is not an outside-only account", roles)
		}
	}
}

// A TPA has standing only in what it filed. Every one of these answered 200
// for a TPA before the guard existed.
func TestATPACannotReachTheRestOfTheTenant(t *testing.T) {
	tpa := who("TPA")
	denied := []struct{ method, path string }{
		{"GET", "/cases"}, {"GET", "/search"}, {"GET", "/accounts"}, {"GET", "/accounts/" + caseA + "/360"},
		{"GET", "/intake"}, {"GET", "/deliverables"}, {"GET", "/onboarding/applications"}, {"GET", "/voice/logs"},
		{"GET", "/voice/intake"}, {"GET", "/cases/clocks"}, {"GET", "/program/recipients"}, {"GET", "/program/templates"},
		{"GET", "/time/mine"}, {"GET", "/views"}, {"GET", "/qa"}, {"GET", "/correspondence"}, {"GET", "/reports/summary"},
		{"POST", "/voice/outbound"}, {"POST", "/tasks/" + caseA + "/complete"}, {"POST", "/graph/ask"},
		// On a case, even its own: nothing internal.
		{"GET", "/cases/" + caseA + "/activities"}, {"GET", "/cases/" + caseA + "/eligibility"},
		{"GET", "/cases/" + caseA + "/correspondence"}, {"GET", "/cases/" + caseA + "/checklist"},
		{"GET", "/cases/" + caseA + "/documents/" + docA + "/analysis"}, {"GET", "/cases/" + caseA + "/documents.zip"},
		{"GET", "/cases/" + caseA + "/workflow"}, {"GET", "/cases/" + caseA + "/related"},
		// And it reads; it does not write through the staff routes.
		{"POST", "/cases/" + caseA + "/signal"}, {"PUT", "/cases/" + caseA + "/review-notes"},
		{"POST", "/cases/" + caseA + "/documents"}, {"DELETE", "/cases/" + caseA},
	}
	for _, c := range denied {
		if d, _, _ := accessDecision(tpa, c.method, c.path); d != "deny" {
			t.Errorf("TPA %s %s: got %q, want deny", c.method, c.path, d)
		}
	}
	ownCase := []string{"/cases/" + caseA, "/cases/" + caseA + "/clocks", "/cases/" + caseA + "/documents",
		"/cases/" + caseA + "/documents/" + docA + "/download"}
	for _, path := range ownCase {
		d, id, _ := accessDecision(tpa, http.MethodGet, path)
		if d != "own-case" || id != caseA {
			t.Errorf("TPA GET %s: got %q for case %q, want own-case for %s", path, d, id, caseA)
		}
	}
	for _, path := range []string{"/tpa/me", "/tpa/dashboard", "/tpa/intake", "/tpa/disputes/" + caseA, "/program", "/prefs",
		"/notifications", "/notifications/read-all", "/calendar", "/intake/bulk", "/intake/bulk/" + caseA} {
		if d, _, _ := accessDecision(tpa, http.MethodGet, path); d != "allow" {
			t.Errorf("TPA %s should be open to a TPA, got %q", path, d)
		}
	}
	// An invited party reads the cases it was invited to (own-case: party_accounts), nothing else.
	if d, id, _ := accessDecision(who("PARTY"), http.MethodGet, "/cases/"+caseA); d != "own-case" || id != caseA {
		t.Errorf("PARTY GET a case: got %q, want own-case", d)
	}
	// Path tricks do not turn a staff route into the TPA's own area.
	for _, path := range []string{"/tpaX/me", "/cases/" + caseA + "/documents/../activities", "/cases/not-a-uuid", "/tpa/../cases"} {
		if d, _, _ := accessDecision(tpa, http.MethodGet, path); path != "/tpa/../cases" && d == "allow" {
			t.Errorf("TPA %s: allowed", path)
		}
	}
}

// A page's data follows the page: a role without it in the menu does not
// get it by address.
func TestAnOutsiderHearsOnlyItsOwnNotices(t *testing.T) {
	for _, role := range []string{"TPA", "HEALTH_PLAN"} {
		got := notifTargets(principal{Subject: "u1", Roles: []string{role}})
		if len(got) != 1 || got[0] != "u1" {
			t.Errorf("a %s hears only what is addressed to it (and its organization); targets %v", role, got)
		}
	}
	if staff := notifTargets(principal{Subject: "u2", Roles: []string{"PM"}}); !contains(staff, "*") {
		t.Errorf("staff still receive tenant broadcasts; targets %v", staff)
	}
}

func TestAnOutsiderSeesOnlyThePublicProgram(t *testing.T) {
	cfg := &ProgramConfig{}
	cfg.Fees.InitialFeeCents = 30000
	cfg.Correspondence.Templates = []CorrTemplate{{Key: "acceptance"}}
	pub := publicProgram(cfg)
	if pub.Fees.InitialFeeCents != 30000 {
		t.Errorf("the fee is public; got %d", pub.Fees.InitialFeeCents)
	}
	if len(pub.Correspondence.Templates) != 0 {
		t.Errorf("letter wording is not for outside filers")
	}
}

func TestAFilerDoesNotReceiveTheOfficesNotes(t *testing.T) {
	got := filingDetails(map[string]any{
		"initiating_party": "Clinic", "filing_notes": "n", "fwa": map[string]any{"flag": true},
		"dispute_summary": "internal", "eligibility_conflicts": []any{"x"}, "provider_type_basis": "inferred",
	})
	if got["initiating_party"] != "Clinic" || got["filing_notes"] != "n" {
		t.Errorf("the filer's own details are kept; got %v", got)
	}
	for _, k := range []string{"fwa", "dispute_summary", "eligibility_conflicts", "provider_type_basis"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s is the office's working note and must not reach the filer", k)
		}
	}
}

func TestPageDataFollowsThePage(t *testing.T) {
	cases := []struct {
		role, path string
		allowed    bool
	}{
		{"CASE_MANAGER", "/intake", true}, {"PM", "/intake", true}, {"TENANT_ADMIN", "/intake", true}, {"FEDERAL_ADMIN", "/intake", true},
		{"CODER", "/intake", false}, {"NURSE_PHYSICIAN", "/intake", false}, {"ATTORNEY", "/intake", false}, {"FINANCE", "/intake", false}, {"DOCTOR", "/intake", false},
		{"CASE_MANAGER", "/deliverables", true}, {"PM", "/deliverables", true},
		{"CODER", "/deliverables", false}, {"ATTORNEY", "/deliverables", false}, {"FINANCE", "/deliverables", false},
		{"CASE_MANAGER", "/voice/logs", true}, {"TENANT_ADMIN", "/voice/intake", true},
		{"PM", "/voice/logs", false}, {"CODER", "/voice/logs", false}, {"ATTORNEY", "/voice/intake", false}, {"FINANCE", "/voice/logs", false},
		// Staff routes with no page policy are untouched by this guard (their handlers decide).
		{"CODER", "/cases", true}, {"NURSE_PHYSICIAN", "/cases/" + caseA, true}, {"ATTORNEY", "/tasks", true},
	}
	for _, c := range cases {
		d, _, _ := accessDecision(who(c.role), http.MethodGet, c.path)
		if (d == "allow") != c.allowed {
			t.Errorf("%s GET %s: got %q, want allowed=%v", c.role, c.path, d, c.allowed)
		}
	}
	// Writing to those paths is not this guard's business; the handlers have their own role checks.
	if d, _, _ := accessDecision(who("CODER"), http.MethodPost, "/intake"); d != "allow" {
		t.Errorf("the page policy is for reads; got %q", d)
	}
}

// A health plan's login comes from the public registration page: it has its
// own area and its own filings, and nothing of the tenant beyond them.
func TestAHealthPlanCannotReachTheRestOfTheTenant(t *testing.T) {
	hp := who("HEALTH_PLAN")
	for _, c := range []struct{ method, path string }{
		{"GET", "/cases"}, {"GET", "/search"}, {"GET", "/accounts"}, {"GET", "/intake"}, {"GET", "/qa"},
		{"GET", "/reports/summary"}, {"GET", "/calendar"}, {"GET", "/tpa/me"}, {"POST", "/tpa/intake"},
		{"GET", "/admin/healthplans"}, {"GET", "/onboarding/reviews"}, {"POST", "/graph/ask"},
		{"GET", "/cases/" + caseA + "/activities"}, {"POST", "/cases/" + caseA + "/signal"},
	} {
		if d, _, _ := accessDecision(hp, c.method, c.path); d != "deny" {
			t.Errorf("HEALTH_PLAN %s %s: got %q, want deny", c.method, c.path, d)
		}
	}
	for _, path := range []string{"/hp/me", "/hp/responding", "/hp/responding/" + caseA, "/hp/onboarding", "/program", "/notifications"} {
		if d, _, _ := accessDecision(hp, http.MethodGet, path); d != "allow" {
			t.Errorf("HEALTH_PLAN %s: got %q, want allow", path, d)
		}
	}
	if d, id, _ := accessDecision(hp, http.MethodGet, "/cases/"+caseA); d != "own-case" || id != caseA {
		t.Errorf("a plan reads the disputes it filed (own-case), got %q", d)
	}
	// A TPA does not get the plan's area either.
	if d, _, _ := accessDecision(who("TPA"), http.MethodGet, "/hp/me"); d != "deny" {
		t.Errorf("TPA /hp/me: got %q, want deny", d)
	}
	// Staff acting for a plan keep their staff access.
	if d, _, _ := accessDecision(who("HEALTH_PLAN", "PM"), http.MethodGet, "/cases"); d != "allow" {
		t.Errorf("PM with a plan linked: got %q, want allow", d)
	}
}

// A login with no role (a registration whose role was never assigned) gets
// the shell and nothing else -- it used to pass as staff.
func TestALoginWithNoRoleGetsNothing(t *testing.T) {
	for _, path := range []string{"/cases", "/search", "/accounts", "/calendar", "/tpa/me", "/hp/me"} {
		if d, _, _ := accessDecision(who("default-roles-idre"), http.MethodGet, path); d != "deny" {
			t.Errorf("role-less GET %s: got %q, want deny", path, d)
		}
	}
	if d, _, _ := accessDecision(who(), http.MethodGet, "/notifications"); d != "allow" {
		t.Errorf("role-less still gets the shell; got %q", d)
	}
}

// An invited party has its workspace and its case's sealed offers, and can
// start a federal dispute -- and nothing of the office's.
func TestAPartyCannotReachTheRestOfTheTenant(t *testing.T) {
	party := who("PARTY")
	for _, c := range []struct{ method, path string }{
		{"GET", "/cases"}, {"GET", "/search"}, {"GET", "/accounts"}, {"GET", "/intake"}, {"GET", "/qa"}, {"GET", "/tpa/me"}, {"GET", "/hp/me"},
		{"POST", "/cases/" + caseA + "/signal"}, {"GET", "/cases/" + caseA + "/activities"}, {"POST", "/cases/" + caseA + "/offers/reveal"},
	} {
		if d, _, _ := accessDecision(party, c.method, c.path); d != "deny" {
			t.Errorf("PARTY %s %s: got %q, want deny", c.method, c.path, d)
		}
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/party/cases"}, {"GET", "/party/cases/" + caseA}, {"POST", "/party/cases/" + caseA + "/respond/submit"},
		{"POST", "/cases/" + caseA + "/offers"}, {"GET", "/cases/" + caseA + "/offers"}, {"POST", "/cases/initiate"}, {"GET", "/notifications"},
	} {
		if d, _, _ := accessDecision(party, c.method, c.path); d != "allow" {
			t.Errorf("PARTY %s %s: got %q, want allow", c.method, c.path, d)
		}
	}
}

func TestOutsideTelemetry(t *testing.T) {
	tpa := principal{Subject: "t1", Roles: []string{"TPA"}}
	if d, _, _ := accessDecision(tpa, http.MethodPost, "/telemetry/events"); d != "allow" {
		t.Errorf("a TPA sends its own portal-use counts: %s", d)
	}
	if d, _, _ := accessDecision(tpa, http.MethodGet, "/telemetry/metrics"); d != "deny" {
		t.Errorf("a TPA never reads the tenant's metrics: %s", d)
	}
}
