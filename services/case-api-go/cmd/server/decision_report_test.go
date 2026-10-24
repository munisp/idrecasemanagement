package main

import (
	"strings"
	"testing"
)

func TestABARoutingChecksum(t *testing.T) {
	for _, ok := range []string{"011000015", "021000021"} {
		if !abaOK(ok) {
			t.Errorf("%s is a valid routing number", ok)
		}
	}
	for _, bad := range []string{"123456789", "02100002", "0210000211", "02100002a", ""} {
		if abaOK(bad) {
			t.Errorf("%q is not a valid routing number", bad)
		}
	}
}

func TestMaskTail(t *testing.T) {
	if got := maskTail("164474499"); got != "•••••4499" {
		t.Errorf("maskTail = %q", got)
	}
	if got := maskTail("123"); strings.ContainsAny(got, "123") {
		t.Errorf("a short number is masked whole, got %q", got)
	}
}

func TestDecisionReportTables(t *testing.T) {
	neg, big, ok := int64(-1), int64(maxDisputedAmountCents+1), int64(106200)
	if msg := validDrClaims([]drClaim{{ClaimNumber: "C1", BilledCents: &ok}}); msg != "" {
		t.Errorf("a plain claim row is fine, got %q", msg)
	}
	if validDrClaims([]drClaim{{ClaimNumber: "C1", PaidCents: &neg}}) == "" {
		t.Error("a negative amount is refused")
	}
	if validDrClaims([]drClaim{{ClaimNumber: "C1", UCRCents: &big}}) == "" {
		t.Error("an amount over the cap is refused")
	}
	if validDrClaims(make([]drClaim, drClaimsMax+1)) == "" {
		t.Error("too many claim rows are refused")
	}
	if validDrDisallowed([]drDisallowed{{Reason: strings.Repeat("x", 601)}}) == "" {
		t.Error("an over-long reason is refused")
	}
}

func TestOutcomeFromCase(t *testing.T) {
	for in, want := range map[string]string{
		"Provider Full Award": "OVERTURNED", "Provider Default Award": "OVERTURNED",
		"Provider Partial Award": "PARTIALLY_OVERTURNED", "Provider No Award": "UPHELD", "Withdrawn": "",
	} {
		if got := drOutcomeFromCase(in); got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
}

func TestDecisionSectionsHaveOwners(t *testing.T) {
	for k, roles := range drSections {
		if len(roles) == 0 {
			t.Errorf("section %s has nobody who may write it", k)
		}
	}
	if contains(drSections["coding_review"], "NURSE_PHYSICIAN") || contains(drSections["physician_review"], "CODER") {
		t.Error("the coding and physician reviews belong to their own reviewers")
	}
}

func TestFillInNames(t *testing.T) {
	got := fillInNames("Dear [Provider Contact Name], [Plan name] declined. AGAINST [PLAN NAME]. [Health Plan Contact Name]. Due [due date].",
		letterNames{Provider: "Gulf Clinic", Plan: "Bay Plan", ProviderContact: "Pat Doe", DueDate: "November 21, 2026"})
	want := "Dear Pat Doe, Bay Plan declined. AGAINST BAY PLAN. Health Plan Representative. Due November 21, 2026."
	if got != want {
		t.Errorf("got %q", got)
	}
	if fillInNames("[Plan name]", letterNames{}) != "[Plan name]" {
		t.Error("an unknown party name is left for the drafter")
	}
	if got := fillInNames("Dear [Health Plan Contact Name],", letterNames{PlanContact: "Jo Lee"}); got != "Dear Jo Lee," {
		t.Errorf("plan contact: %q", got)
	}
	if got := fillInNames("Dear [Filing Party full name], the fee is $[insert amount].", letterNames{Filer: "Pat Doe", Estimate: "$1,500.00"}); got != "Dear Pat Doe, the fee is $1,500.00." {
		t.Errorf("filer and estimate: %q", got)
	}
	if got := fillInNames("$[insert amount]", letterNames{}); got != "$[insert amount]" {
		t.Errorf("no estimate yet: the fill-in stays for QA to see, got %q", got)
	}
}

func TestTemplateVariableNames(t *testing.T) {
	for _, ok := range []string{"agency_contacts", "director_name", "ab"} {
		if !tvNameRE.MatchString(ok) || tvReserved(ok) {
			t.Errorf("%s should be a usable variable name", ok)
		}
	}
	for _, bad := range []string{"Agency", "1st", "a", "with space", "x-y"} {
		if tvNameRE.MatchString(bad) {
			t.Errorf("%q should not be a variable name", bad)
		}
	}
	for _, taken := range []string{"case_number", "provider_name", "share_link", "org_name", "agency_salutation"} {
		if !tvReserved(taken) {
			t.Errorf("{%s} is filled by the system and must not be a variable", taken)
		}
	}
}
