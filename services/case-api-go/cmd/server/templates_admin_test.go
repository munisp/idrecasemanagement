// Author: Patrick Munis at newwave
package main

import (
	"strings"
	"testing"
)

func flBase() []CorrTemplate {
	return []CorrTemplate{
		{Key: "submission_instructions", Subject: "Submission", Body: "std instr", To: []string{"requester"}},
		{Key: "acceptance", Subject: "Accepted {case_number}", Body: "std acceptance", To: []string{"provider"}, QARole: "PM"},
		{Key: "dismissal", Subject: "Dismissed", Body: "std dismissal", To: []string{"provider"}, QARole: "ATTORNEY", QAPreRole: "PM"},
		{Key: "plan_notification", Subject: "Notice", Body: "std notice", To: []string{"health_plan"}, QARole: "PM"},
	}
}

func find(ts []CorrTemplate, key string) *CorrTemplate {
	for i := range ts {
		if ts[i].Key == key {
			return &ts[i]
		}
	}
	return nil
}

// An edit changes what a standard letter says, never what approves it.
func TestOverlayKeepsTheQAGateOnAnEditedStandardTemplate(t *testing.T) {
	got := overlayTemplates(flBase(), []CorrTemplate{
		// A row that tries to carry its own (weaker) QA roles: they are not stored, and must not count.
		{Key: "dismissal", Subject: "New subject", Body: "new body", To: []string{"filing_party"}, QARole: "", QAPreRole: "", Inactive: true},
	})
	d := find(got, "dismissal")
	if d == nil || d.Subject != "New subject" || d.Body != "new body" || d.To[0] != "filing_party" || !d.Edited {
		t.Fatalf("edit not applied: %+v", d)
	}
	if d.QARole != "ATTORNEY" || d.QAPreRole != "PM" {
		t.Errorf("the dismissal's two-stage QA was lost: %q / %q", d.QARole, d.QAPreRole)
	}
	if d.Inactive {
		t.Error("a standard template can never be retired: steps depend on it")
	}
	if a := find(got, "acceptance"); a.Body != "std acceptance" || a.Edited {
		t.Errorf("an untouched template changed: %+v", a)
	}
}

// A template somebody adds always goes through the program's QA gate.
func TestOverlayPutsAnAddedTemplateBehindTheProgramsQAGate(t *testing.T) {
	got := overlayTemplates(flBase(), []CorrTemplate{
		{Key: "holiday_notice", Name: "Holiday notice", Subject: "Closed", Body: "we are closed", To: []string{"filing_party"}},
		{Key: "old_notice", Subject: "x", Body: "y", To: []string{"provider"}, Inactive: true},
	})
	h := find(got, "holiday_notice")
	if h == nil || !h.Custom || h.QARole != "PM" {
		t.Fatalf("an added template must take the PM gate: %+v", h)
	}
	if o := find(got, "old_notice"); o == nil || !o.Inactive {
		t.Errorf("a retired added template stays listed as retired: %+v", o)
	}
	if len(got) != len(flBase())+2 {
		t.Errorf("got %d templates", len(got))
	}
	// A tenant whose program has no gate at all gets none invented.
	nogate := overlayTemplates([]CorrTemplate{{Key: "a", Subject: "s", Body: "b"}}, []CorrTemplate{{Key: "mine", Subject: "s", Body: "b"}})
	if find(nogate, "mine").QARole != "" {
		t.Error("no gate should be invented for a program without one")
	}
}

func TestPickUsageTakesTheMostSpecificRuleThatFits(t *testing.T) {
	rules := []templateUsage{
		{ID: 1, Event: "acceptance", Template: "acceptance_general"},
		{ID: 2, Event: "acceptance", Template: "acceptance_plan", FilingPartyType: "HEALTH_PLAN"},
		{ID: 3, Event: "acceptance", Template: "acceptance_plan_medicaid", FilingPartyType: "HEALTH_PLAN", LineOfBusiness: "Medicaid"},
		{ID: 4, Event: "acceptance", Template: "acceptance_medicaid", LineOfBusiness: "Medicaid"},
		{ID: 5, Event: "ineligible", Template: "ineligible_short"},
	}
	cases := []struct {
		event, party, lob, want string
	}{
		{"acceptance", "PROVIDER", "Commercial", "acceptance_general"},
		{"acceptance", "HEALTH_PLAN", "Commercial", "acceptance_plan"},
		{"acceptance", "health_plan", "medicaid", "acceptance_plan_medicaid"}, // matched without regard to case
		{"acceptance", "PROVIDER", "Medicaid", "acceptance_medicaid"},
		{"acceptance", "", "", "acceptance_general"}, // a case with neither fact: only the unconditional rule fits
		{"ineligible", "HEALTH_PLAN", "Medicaid", "ineligible_short"},
		{"dismissal", "PROVIDER", "Medicaid", ""}, // no rule: the standard template
	}
	for _, c := range cases {
		got := ""
		if u := pickUsage(rules, c.event, c.party, c.lob); u != nil {
			got = u.Template
		}
		if got != c.want {
			t.Errorf("%s for %q/%q: got %q, want %q", c.event, c.party, c.lob, got, c.want)
		}
	}
	// A rule that names a fact the case does not have never applies.
	only := []templateUsage{{Event: "acceptance", Template: "x", LineOfBusiness: "Medicaid"}}
	if pickUsage(only, "acceptance", "PROVIDER", "") != nil {
		t.Error("a Medicaid-only rule applied to a case with no line of business")
	}
}

func TestTemplateValidation(t *testing.T) {
	ok := templateIn{Name: " Holiday ", Subject: " Closed {case_number} ", Body: "Dear {filing_party},\r\nwe are closed.", To: []string{"filing_party"}}
	if msg := ok.validate(); msg != "" {
		t.Fatalf("a good template was refused: %s", msg)
	}
	if ok.Name != "Holiday" || ok.Subject != "Closed {case_number}" || strings.Contains(ok.Body, "\r") {
		t.Errorf("not tidied: %+v", ok)
	}
	bad := []struct {
		name string
		in   templateIn
		want string
	}{
		{"no subject", templateIn{Body: "b", To: []string{"provider"}}, "subject"},
		{"no body", templateIn{Subject: "s", To: []string{"provider"}}, "body"},
		{"no recipient", templateIn{Subject: "s", Body: "b"}, "who the template goes to"},
		{"unknown recipient", templateIn{Subject: "s", Body: "b", To: []string{"everyone"}}, "not a recipient"},
		{"a placeholder the system cannot fill", templateIn{Subject: "s", Body: "Dear {patient_name}", To: []string{"provider"}}, "{patient_name}"},
		{"a raw email as a recipient", templateIn{Subject: "s", Body: "b", To: []string{"a@b.com"}}, "not a recipient"},
	}
	for _, c := range bad {
		if msg := c.in.validate(); !strings.Contains(msg, c.want) {
			t.Errorf("%s: got %q, want it to mention %q", c.name, msg, c.want)
		}
	}
	for _, key := range []string{"holiday_notice", "abc", "a1_b2"} {
		if !templateKeyRE.MatchString(key) {
			t.Errorf("%q should be a valid short name", key)
		}
	}
	for _, key := range []string{"", "ab", "Holiday", "1st", "has space", "has-dash", "../etc"} {
		if templateKeyRE.MatchString(key) {
			t.Errorf("%q should not be a valid short name", key)
		}
	}
}

// Whatever the template says, the filer gets the case number and both links.
func TestIntakeLinksBodyAlwaysCarriesTheNumberAndBothLinks(t *testing.T) {
	const dl, up = "https://x/api/share/DOWN", "https://x/api/share/UP"
	sop := "Hello,\n\nDownload:\n{download_link}\n\nUpload:\n{share_link}\n\nThanks."
	got := intakeLinksBody(sop, "FL26-001", dl, up)
	if !strings.Contains(got, dl) || !strings.Contains(got, up) || !strings.Contains(got, "Case number: FL26-001") {
		t.Errorf("SOP wording: %q", got)
	}
	if strings.Count(got, dl) != 1 || strings.Count(got, up) != 1 || strings.Contains(got, "{") {
		t.Errorf("links repeated or a placeholder left in: %q", got)
	}
	// Someone edits the template and drops the upload link: it is still sent.
	edited := intakeLinksBody("Please read {download_link}. Your case is FL26-001.", "FL26-001", dl, up)
	if !strings.Contains(edited, up) || strings.Contains(edited, "Case number:") {
		t.Errorf("edited wording: %q", edited)
	}
	// No wording at all.
	bare := intakeLinksBody("  ", "FL26-001", dl, up)
	if !strings.Contains(bare, dl) || !strings.Contains(bare, up) || !strings.Contains(bare, "FL26-001") {
		t.Errorf("empty wording: %q", bare)
	}
}

func TestGroupThousands(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 12850595: "12,850,595"} {
		if got := groupThousands(in); got != want {
			t.Errorf("%d: got %q", in, got)
		}
	}
}

func TestLetterPartyNamesComeFromTheFilerWhenNothingBetterIsKnown(t *testing.T) {
	cases := []struct {
		provider, plan, filer, kind, wantProvider, wantPlan string
	}{
		{"", "", "Northgate Health, P.C.", "PROVIDER", "Northgate Health, P.C.", ""},
		{"", "", "Sunshine Plan", "HEALTH_PLAN", "", "Sunshine Plan"},
		{"", "", "Northgate Health, P.C.", "", "Northgate Health, P.C.", ""},                                              // not recorded: filers are providers unless said otherwise
		{"Read From EOB, M.D.", "Plan From EOB", "A Billing Company", "PROVIDER", "Read From EOB, M.D.", "Plan From EOB"}, // known names win
		{"", "", "", "PROVIDER", "", ""},
	}
	for _, c := range cases {
		gp, gl := letterPartyNames(c.provider, c.plan, c.filer, c.kind)
		if gp != c.wantProvider || gl != c.wantPlan {
			t.Errorf("%+v: got %q / %q", c, gp, gl)
		}
	}
}
