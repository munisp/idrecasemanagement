package main

import (
	"strings"
	"testing"
	"time"
)

func ptr(v int64) *int64 { return &v }

func claimCfg() *ProgramConfig {
	cfg := &ProgramConfig{}
	cfg.Eligibility.FilingWindowMonths = 12
	cfg.Eligibility.ServicesAfter = "2025-01-01"
	return cfg
}

// Partial eligibility (change log 2026-11-28): one claim out of time, one before the program start, one fine.
func TestClaimsPartiallyEligible(t *testing.T) {
	in := eligibilityInput{FiledOn: "2026-11-01", FinalDeterminationAt: "2026-06-01"}
	rows := []caseClaimRow{
		{number: "A", fd: "2025-06-01", service: "2025-05-01", cents: ptr(500000)}, // filed 17 months after: out
		{number: "B", fd: "2026-03-01", service: "2024-12-01", cents: ptr(500000)}, // service before the start: out
		{number: "C", fd: "", service: "2026-01-10", cents: ptr(900000)},           // no date: the case's (in time)
	}
	ev := map[string]any{"threshold_min_cents": int64(100000)}
	res, why := decidePartial("ELIGIBLE", "", ev, checkClaims(claimCfg(), in, rows))
	if res != "ELIGIBLE" || why != "partially_eligible" {
		t.Fatalf("got %s %s", res, why)
	}
	cs := ev["claims"].([]claimCheck)
	if cs[0].Eligible || cs[0].Rule != "over_filing_window" || cs[0].Wording != "were filed more than 12 months after the final determination." {
		t.Fatalf("A: %+v", cs[0])
	}
	if cs[1].Eligible || cs[1].Rule != "service_before_program_start" {
		t.Fatalf("B: %+v", cs[1])
	}
	if !cs[2].Eligible || !cs[2].DateFromCase || cs[2].FinalDetermination != "2026-06-01" {
		t.Fatalf("C: %+v", cs[2])
	}
	if ev["eligible_claims_amount_cents"] != int64(900000) || ev["partially_eligible"] != true {
		t.Fatalf("evidence: %v", ev)
	}
}

// The minimum amount is tested on the eligible claims only: dropping claims can take a dispute under it.
func TestClaimsUnderMinimumOnceClaimsDrop(t *testing.T) {
	in := eligibilityInput{FiledOn: "2026-11-01"}
	rows := []caseClaimRow{{number: "A", fd: "2025-01-01", cents: ptr(900000)}, {number: "B", fd: "2026-05-01", cents: ptr(50000)}}
	ev := map[string]any{"threshold_min_cents": int64(100000)}
	if res, why := decidePartial("ELIGIBLE", "", ev, checkClaims(claimCfg(), in, rows)); res != "INELIGIBLE" || why != "below_threshold" {
		t.Fatalf("got %s %s", res, why)
	}
}

// No claim left: the dispute is ineligible; a person's mark counts and is kept.
func TestClaimsNoneLeftAndStaffMarks(t *testing.T) {
	in := eligibilityInput{FiledOn: "2026-11-01"}
	rows := []caseClaimRow{{number: "A", fd: "2025-01-01"}, {number: "B", status: "INELIGIBLE", by: "manual.pm", reason: "were billed under another NPI."}}
	ev := map[string]any{}
	res, why := decidePartial("ELIGIBLE", "", ev, checkClaims(claimCfg(), in, rows))
	if res != "INELIGIBLE" || why != "over_filing_window" {
		t.Fatalf("got %s %s", res, why)
	}
	if cs := ev["claims"].([]claimCheck); cs[1].MarkedBy != "manual.pm" || cs[1].Wording != "were billed under another NPI." {
		t.Fatalf("staff mark: %+v", cs[1])
	}
}

// Every claim in time: nothing changes.
func TestClaimsAllEligible(t *testing.T) {
	in := eligibilityInput{FiledOn: "2026-11-01"}
	rows := []caseClaimRow{{number: "A", fd: "2026-05-01"}, {number: "B", fd: "2026-06-01"}}
	ev := map[string]any{}
	if res, why := decidePartial("ELIGIBLE", "", ev, checkClaims(claimCfg(), in, rows)); res != "ELIGIBLE" || why != "" || ev["partially_eligible"] != nil {
		t.Fatalf("got %s %q %v", res, why, ev)
	}
}

// The checklist prints the claims and a partial result.
func TestChecklistShowsPartial(t *testing.T) {
	ev := map[string]any{"eligible_claims": 1, "claims": []claimCheck{{Claim: "A", Eligible: false, Wording: "were filed late."}, {Claim: "C", Eligible: true, FinalDetermination: "2026-06-01", DateFromCase: true}}}
	h := eligibilityChecklistHTML("FL26-1", "ELIGIBLE", "partially_eligible", "pm", time.Now(), ev)
	for _, want := range []string{"PARTIALLY ELIGIBLE", "1 of 2 claims", "Not eligible — were filed late.", "date taken from the case"} {
		if !strings.Contains(h, want) {
			t.Fatalf("checklist missing %q", want)
		}
	}
}
