// Author: Patrick Munis at newwave
package main

// Eligibility decision-core tests. The vectors come straight from the FL
// AHCA SOP threshold matrix and filing-window rule seeded in
// scripts/program-rules.sql — if the SOP changes, the seed changes, and
// these vectors must change with it. That coupling is intentional: the test
// file is the executable copy of the SOP's eligibility page.

import (
	"strings"
	"testing"
	"time"
)

func flTestConfig() *ProgramConfig {
	var cfg ProgramConfig
	cfg.Eligibility.Thresholds = []ThresholdRule{
		{ProviderType: "hospital_inpatient", Contracted: boolP(true), MinCents: 2500000},
		{ProviderType: "hospital_inpatient", Contracted: boolP(false), MinCents: 1000000},
		{ProviderType: "hospital_outpatient", Contracted: boolP(true), MinCents: 1000000},
		{ProviderType: "hospital_outpatient", Contracted: boolP(false), MinCents: 300000},
		{ProviderType: "physician_dentist", MinCents: 50000},
		{ProviderType: "rural_hospital", MinCents: 0},
		{ProviderType: "other", MinCents: 0},
	}
	cfg.Eligibility.FilingWindowMonths = 12
	cfg.Eligibility.Reasons = []string{"late_payment_only", "interest_only", "medicare_grievance",
		"medicare_reconsideration", "plan_not_fl_regulated", "provider_not_fl_licensed",
		"medicaid_fair_hearing", "pending_court_action", "over_12_months",
		"pre_2000_binding_process", "below_threshold", "improper_aggregation",
		"internal_process_not_exhausted"}
	cfg.Eligibility.ServicesAfter = "2000-10-02"
	cfg.Eligibility.DisputeTypes = []string{"denied_in_full", "denied_in_part", "underpaid", "overpaid"}
	cfg.Statuses.Internal = []string{"Initial Review Pending", "QA Initial Review", "Review In Progress", "Final Order Issued"}
	return &cfg
}

func boolP(b bool) *bool { return &b }

func TestEligibilitySOPVectors(t *testing.T) {
	cfg := flTestConfig()
	vectors := []struct {
		name       string
		in         eligibilityInput
		wantResult string
		wantReason string
	}{
		{"inpatient contracted at threshold", eligibilityInput{ProviderType: "hospital_inpatient", Contracted: boolP(true), DisputedAmountCents: 2500000}, "ELIGIBLE", ""},
		{"inpatient contracted below threshold", eligibilityInput{ProviderType: "hospital_inpatient", Contracted: boolP(true), DisputedAmountCents: 2499999}, "INELIGIBLE", "below_threshold"},
		{"inpatient non-contracted threshold", eligibilityInput{ProviderType: "hospital_inpatient", Contracted: boolP(false), DisputedAmountCents: 1000000}, "ELIGIBLE", ""},
		{"outpatient non-contracted below", eligibilityInput{ProviderType: "hospital_outpatient", Contracted: boolP(false), DisputedAmountCents: 299999}, "INELIGIBLE", "below_threshold"},
		{"physician at $500", eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 50000}, "ELIGIBLE", ""},
		{"physician at $499.99", eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 49999}, "INELIGIBLE", "below_threshold"},
		{"rural hospital no minimum", eligibilityInput{ProviderType: "rural_hospital", DisputedAmountCents: 1}, "ELIGIBLE", ""},
		{"explicit flag wins over amount", eligibilityInput{ProviderType: "rural_hospital", DisputedAmountCents: 99999999, Flags: []string{"plan_not_fl_regulated"}}, "INELIGIBLE", "plan_not_fl_regulated"},
		{"medicare grievance excluded", eligibilityInput{ProviderType: "other", DisputedAmountCents: 100, Flags: []string{"medicare_grievance"}}, "INELIGIBLE", "medicare_grievance"},
		{"AOR hold regardless of merits", eligibilityInput{ProviderType: "other", DisputedAmountCents: 100, AORValid: boolP(false)}, "HOLD_AOR", "aor_invalid_pending_attorney"},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			res, reason, _ := evalEligibility(cfg, v.in)
			if res != v.wantResult || reason != v.wantReason {
				t.Fatalf("got %s/%s, want %s/%s", res, reason, v.wantResult, v.wantReason)
			}
		})
	}
}

func TestEligibilityFilingWindow(t *testing.T) {
	cfg := flTestConfig()
	old := time.Now().AddDate(-1, -1, 0).Format("2006-01-02") // 13 months ago
	res, reason, ev := evalEligibility(cfg, eligibilityInput{
		ProviderType: "other", DisputedAmountCents: 100, FinalDeterminationAt: old})
	if res != "INELIGIBLE" || reason != "over_filing_window" {
		t.Fatalf("stale filing: got %s/%s", res, reason)
	}
	if ev["filing_deadline"] == nil {
		t.Fatal("deadline not recorded in evidence")
	}
	recent := time.Now().AddDate(0, -2, 0).Format("2006-01-02")
	res, _, _ = evalEligibility(cfg, eligibilityInput{
		ProviderType: "other", DisputedAmountCents: 100, FinalDeterminationAt: recent})
	if res != "ELIGIBLE" {
		t.Fatalf("timely filing: got %s", res)
	}
}

func TestEligibilityBorderline(t *testing.T) {
	cfg := flTestConfig()
	// $540 vs the $500 physician threshold: +8% — borderline.
	in := eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 54000}
	_, _, ev := evalEligibility(cfg, in)
	if got := eligibilityBorderline(cfg, in, ev); len(got) != 1 {
		t.Fatalf("expected borderline, got %v", got)
	}
	// $460: -8% below — also borderline (and INELIGIBLE, which is exactly the
	// call dual control exists to catch).
	in = eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 46000}
	res, _, ev := evalEligibility(cfg, in)
	if res != "INELIGIBLE" || len(eligibilityBorderline(cfg, in, ev)) != 1 {
		t.Fatalf("below-margin case: res=%s borderline=%v", res, eligibilityBorderline(cfg, in, ev))
	}
	// $1,000: far clear of the threshold — not borderline.
	in = eligibilityInput{ProviderType: "physician_dentist", DisputedAmountCents: 100000}
	_, _, ev = evalEligibility(cfg, in)
	if got := eligibilityBorderline(cfg, in, ev); len(got) != 0 {
		t.Fatalf("unexpected borderline: %v", got)
	}
	// Deadline within 30 days — borderline.
	in = eligibilityInput{ProviderType: "other", DisputedAmountCents: 100,
		FinalDeterminationAt: time.Now().AddDate(0, -11, -15).Format("2006-01-02")}
	_, _, ev = evalEligibility(cfg, in)
	if got := eligibilityBorderline(cfg, in, ev); len(got) != 1 {
		t.Fatalf("deadline-margin case: %v", got)
	}
}

func TestEligibilityRuleVersionStable(t *testing.T) {
	cfg := flTestConfig()
	v1 := eligibilityRuleVersion(cfg)
	v2 := eligibilityRuleVersion(cfg)
	if v1 == "" || v1 != v2 {
		t.Fatalf("rule version unstable: %q vs %q", v1, v2)
	}
	cfg.Eligibility.Thresholds[4].MinCents = 60000
	if eligibilityRuleVersion(cfg) == v1 {
		t.Fatal("rule version did not change when thresholds changed")
	}
}

func TestPastEligibilityGate(t *testing.T) {
	cfg := flTestConfig()
	if pastEligibilityGate(cfg, "Initial Review Pending") || pastEligibilityGate(cfg, "QA Initial Review") {
		t.Fatal("intake-phase statuses must not trip the gate")
	}
	if !pastEligibilityGate(cfg, "Review In Progress") || !pastEligibilityGate(cfg, "Final Order Issued") {
		t.Fatal("post-review statuses must trip the gate")
	}
	if pastEligibilityGate(nil, "Review In Progress") {
		t.Fatal("federal tenants (no program) have no gate")
	}
}

func TestValidOverrideReason(t *testing.T) {
	if !validOverrideReason("ten chars ok") {
		t.Fatal("10 chars is the floor")
	}
	if validOverrideReason("too short") {
		t.Fatal("9 chars must be rejected")
	}
	if validOverrideReason("  no  ") {
		t.Fatal("whitespace is not a reason")
	}
}

// The remaining SOP criteria: every listed exclusion is a reason code, the
// date-of-service floor, accepted claim types, and input validation.
func TestEligibilitySOPExclusionsAndScope(t *testing.T) {
	cfg := flTestConfig()
	ok := eligibilityInput{ProviderType: "other", DisputedAmountCents: 100}
	for _, flag := range []string{"late_payment_only", "interest_only", "medicare_grievance",
		"medicare_reconsideration", "plan_not_fl_regulated", "provider_not_fl_licensed",
		"medicaid_fair_hearing", "pending_court_action", "over_12_months",
		"pre_2000_binding_process", "below_threshold", "improper_aggregation",
		"internal_process_not_exhausted"} {
		in := ok
		in.Flags = []string{flag}
		if res, reason, _ := evalEligibility(cfg, in); res != "INELIGIBLE" || reason != flag {
			t.Errorf("flag %s: got %s/%s, want INELIGIBLE/%s", flag, res, reason, flag)
		}
	}
	cases := []struct {
		name       string
		mut        func(*eligibilityInput)
		wantResult string
		wantReason string
	}{
		{"service on the floor date is out", func(i *eligibilityInput) { i.ServiceDate = "2000-10-02" }, "INELIGIBLE", "service_before_program_start"},
		{"service before the floor is out", func(i *eligibilityInput) { i.ServiceDate = "1999-06-01" }, "INELIGIBLE", "service_before_program_start"},
		{"service the day after is in", func(i *eligibilityInput) { i.ServiceDate = "2000-10-03" }, "ELIGIBLE", ""},
		{"underpaid is an accepted claim type", func(i *eligibilityInput) { i.DisputeType = "underpaid" }, "ELIGIBLE", ""},
		{"overpaid is an accepted claim type", func(i *eligibilityInput) { i.DisputeType = "overpaid" }, "ELIGIBLE", ""},
		{"an unlisted claim type is out", func(i *eligibilityInput) { i.DisputeType = "contract_interpretation" }, "INELIGIBLE", "not_an_eligible_dispute_type"},
	}
	for _, c := range cases {
		in := ok
		c.mut(&in)
		if res, reason, _ := evalEligibility(cfg, in); res != c.wantResult || reason != c.wantReason {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, res, reason, c.wantResult, c.wantReason)
		}
	}
	if invalidEligibilityInput(eligibilityInput{FinalDeterminationAt: "06/15/2026"}) == "" {
		t.Error("a non-ISO determination date must be rejected")
	}
	if invalidEligibilityInput(eligibilityInput{FinalDeterminationAt: "2026-06-15", ServiceDate: "2026-03-12"}) != "" {
		t.Error("valid ISO dates must be accepted")
	}
	if !noOverrideReasons["over_filing_window"] || noOverrideReasons["below_threshold"] {
		t.Error("only the filing-window results are non-overridable")
	}
}

func TestEligibilityProviderTypesAndConflicts(t *testing.T) {
	cfg := &ProgramConfig{}
	yes, no := true, false
	cfg.Eligibility.Thresholds = []ThresholdRule{
		{ProviderType: "hospital_inpatient", Contracted: &yes, MinCents: 2500000},
		{ProviderType: "hospital_inpatient", Contracted: &no, MinCents: 1000000},
		{ProviderType: "physician_dentist", MinCents: 50000},
	}
	types := eligibilityProviderTypes(cfg)
	if len(types) != 2 || types[0] != "hospital_inpatient" || types[1] != "physician_dentist" {
		t.Fatalf("provider types should be distinct and in configured order: %v", types)
	}
	if contains(types, "physician") {
		t.Error("a near-miss spelling must not count as a known provider type")
	}
	got := conflictSentences([]byte(`[{"field":"disputed_amount_cents","case_value":185000,"document_value":3140000,"document":"VALID-05.pdf"},{"field":"contracted","case_value":false,"document_value":true}]`))
	if len(got) != 2 || !strings.Contains(got[0], "disputed amount cents") || !strings.Contains(got[0], "VALID-05.pdf says") {
		t.Errorf("conflict sentences wrong: %v", got)
	}
	if !strings.Contains(got[1], "a document says true") {
		t.Errorf("a conflict with no document name should still read: %q", got[1])
	}
	if conflictSentences([]byte(`not json`)) != nil || len(conflictSentences([]byte(`[]`))) != 0 {
		t.Error("no conflicts should yield nothing")
	}
}

// Network status is asked for only where it picks the threshold.
func TestMissingInputsAsksNetworkStatusOnlyWhereItMatters(t *testing.T) {
	cfg := flTestConfig()
	has := func(m []string, k string) bool { return strings.Contains(","+strings.Join(m, ",")+",", ","+k+",") }
	physician := missingEligibilityInputs(cfg, eligibilityInput{ProviderType: "physician_dentist", FinalDeterminationAt: "2026-09-18"})
	if len(physician) != 0 {
		t.Fatalf("a physician's dispute needs nothing more, got %v", physician)
	}
	hospital := missingEligibilityInputs(cfg, eligibilityInput{ProviderType: "hospital_outpatient", FinalDeterminationAt: "2026-09-18"})
	if !has(hospital, "contracted") {
		t.Fatalf("a hospital's threshold depends on its network status, got %v", hospital)
	}
	unknown := missingEligibilityInputs(cfg, eligibilityInput{})
	for _, k := range []string{"provider_type", "contracted", "final_determination_at"} {
		if !has(unknown, k) {
			t.Fatalf("with nothing known %s is missing, got %v", k, unknown)
		}
	}
}

// A filed document set's facts: a stated provider type beats an inferred
// one, the EOB is read first, and the network status sets out-of-network.
func TestPackageCaseFacts(t *testing.T) {
	got := packageCaseFacts([]packageSlotFacts{
		{Slot: "ITEMIZED_BILL", Fields: []byte(`{"provider_type":"physician_dentist","provider_type_basis":"provider is a professional practice (X Physicians)"}`)},
		{Slot: "OFFER_SUBSTANTIATION", Fields: []byte(`{"provider_type":"hospital_outpatient"}`)},
		{Slot: "EOB", Fields: []byte(`{"contracted":false,"service_date":"2026-08-14"}`)},
		{Slot: "PROOF_OF_COVERAGE", Fields: []byte(`{"contracted":true}`)},
	})
	if got["provider_type"] != "hospital_outpatient" || got["provider_type_basis"] != nil {
		t.Fatalf("a stated type beats an inferred one: %v", got)
	}
	if got["contracted"] != false || got["out_of_network"] != "Yes" {
		t.Fatalf("the EOB's network status is read first: %v", got)
	}
	if _, ok := got["service_date"]; ok {
		t.Fatalf("dates stay with each document's analysis: %v", got)
	}
	inferred := packageCaseFacts([]packageSlotFacts{
		{Slot: "ITEMIZED_BILL", Fields: []byte(`{"provider_type":"physician_dentist","provider_type_basis":"provider is a professional practice (X Physicians)"}`)},
	})
	if inferred["provider_type"] != "physician_dentist" || !strings.HasPrefix(inferred["provider_type_basis"].(string), "inferred: ") {
		t.Fatalf("an inferred type is kept with its basis: %v", inferred)
	}
	if len(packageCaseFacts([]packageSlotFacts{{Slot: "EOB", Fields: []byte(`{}`)}})) != 0 {
		t.Fatal("nothing read, nothing set")
	}
}

// The filing window is about the day the dispute was filed, not the day it is reviewed: filed in time and
// reviewed after the deadline is still in time; filed after the deadline is late however soon it is reviewed.
func TestFilingWindowUsesFilingDate(t *testing.T) {
	cfg := &ProgramConfig{}
	cfg.Eligibility.FilingWindowMonths = 12
	in := eligibilityInput{FinalDeterminationAt: "2025-05-09", FiledOn: "2026-04-30"} // filed 9 days before the deadline
	if res, reason, ev := evalEligibility(cfg, in); res != "ELIGIBLE" {
		t.Errorf("filed in time, reviewed later: got %s %s %v", res, reason, ev)
	}
	in.FiledOn = "2026-10-21" // filed five months after the deadline
	res, reason, ev := evalEligibility(cfg, in)
	if res != "INELIGIBLE" || reason != "over_filing_window" {
		t.Errorf("filed late: got %s %s", res, reason)
	}
	if ev["filing_deadline"] != "2026-05-09" || ev["filed_on"] != "2026-10-21" || ev["final_determination_at"] != "2025-05-09" {
		t.Errorf("evidence should carry the determination date, deadline and filing date: %v", ev)
	}
}
