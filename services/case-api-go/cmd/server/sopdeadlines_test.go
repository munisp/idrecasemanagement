// Author: Patrick Munis at newwave
package main

import (
	"strings"
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestClockDueAndTimeliness(t *testing.T) {
	rules := []ClockRule{
		{Name: "AGENCY_RECOMMENDATION", Basis: "received_at", Days: 60, MaxDays: 90, DayType: "calendar"},
		{Name: "INITIAL_REVIEW", Basis: "packet_complete_at", Days: 10, MaxDays: 13, DayType: "calendar"},
		{Name: "PLAN_RESPONSE", Basis: "plan_notified_at", Days: 15, DayType: "calendar"},
	}
	dates := map[string]string{"received_at": "2026-01-01", "packet_complete_at": "2026-01-05"}
	target, hard, ok := clockDue(rules, dates, "AGENCY_RECOMMENDATION")
	if !ok || target != d("2026-03-02") || hard != d("2026-04-01") {
		t.Fatalf("60/90-day clock from Jan 1: got target %s hard %s ok=%v", target.Format("2006-01-02"), hard.Format("2006-01-02"), ok)
	}
	target, hard, _ = clockDue(rules, dates, "INITIAL_REVIEW")
	if target != d("2026-01-15") || hard != d("2026-01-18") {
		t.Fatalf("10(+3)-day clock from Jan 5: got %s / %s", target.Format("2006-01-02"), hard.Format("2006-01-02"))
	}
	if _, _, ok := clockDue(rules, dates, "PLAN_RESPONSE"); ok {
		t.Error("a clock whose basis date is not recorded has not started")
	}
	for on, want := range map[string]string{
		"2026-01-15": "ON_TIME", "2026-01-16": "LATE_WITHIN_LIMIT", "2026-01-18": "LATE_WITHIN_LIMIT", "2026-01-19": "BREACH",
	} {
		if got := timeliness(d(on).Add(15*time.Hour), target, hard); got != want {
			t.Errorf("acceptance letter sent %s: got %s, want %s", on, got, want)
		}
	}
	// A clock with no hard limit goes straight from on time to breach.
	target, hard, _ = clockDue(rules, map[string]string{"plan_notified_at": "2026-02-01"}, "PLAN_RESPONSE")
	if timeliness(d("2026-02-17"), target, hard) != "BREACH" {
		t.Error("a 15-day clock with no extra limit is in breach on day 16")
	}
}

func TestClockLateState(t *testing.T) {
	rules := []ClockRule{{Name: "AGENCY_RECOMMENDATION", Basis: "received_at", Days: 60, MaxDays: 90, DayType: "calendar"}}
	state := func(today string) string {
		return projectProgramClocks(rules, map[string]string{"received_at": "2026-01-01"}, d(today))[0].State
	}
	if s := state("2026-03-10"); s != "late" {
		t.Errorf("day 68 of a 60/90 clock should read late, got %s", s)
	}
	if s := state("2026-04-05"); s != "breach" {
		t.Errorf("day 94 of a 60/90 clock should read breach, got %s", s)
	}
	if s := state("2026-01-10"); s == "late" || s == "breach" {
		t.Errorf("day 9 should not be late or in breach, got %s", s)
	}
}

func TestContractDue(t *testing.T) {
	dates := map[string]string{"execution_date": "2026-01-01", "effective_date": "2026-03-01", "end_date": "2028-12-29"}
	for rule, want := range map[string]string{
		"contract:execution+30d": "2026-01-31",
		"contract:effective-30d": "2026-01-30",
		"contract:end":           "2028-12-29",
		"contract:end+60d":       "2029-02-27",
		"contract:end+30bd":      "2029-02-09", // 30 business days after Fri Dec 29
		"contract:standing":      "",
		"weekly:MONDAY":          "",
	} {
		if got := contractDue(rule, dates); got != want {
			t.Errorf("%s: got %q, want %q", rule, got, want)
		}
	}
	if contractDue("contract:end+60d", map[string]string{}) != "" {
		t.Error("no contract end date entered: no due date")
	}
}

func TestReportPeriod(t *testing.T) {
	// Wed 14 Oct 2026: the weekly report was due Mon 12 Oct, the monthly on the 10th.
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	p, due, ok := reportPeriod("weekly:MONDAY", now)
	if !ok || p != "2026-W42" || due != d("2026-10-12") {
		t.Errorf("weekly: got %s due %s ok=%v", p, due.Format("2006-01-02"), ok)
	}
	p, due, ok = reportPeriod("monthly:10", now)
	if !ok || p != "2026-10" || due != d("2026-10-10") {
		t.Errorf("monthly: got %s due %s ok=%v", p, due.Format("2006-01-02"), ok)
	}
	// A Monday is its own week's due date.
	if _, due, _ := reportPeriod("weekly:MONDAY", d("2026-10-12")); due != d("2026-10-12") {
		t.Errorf("a Monday should be due that day, got %s", due.Format("2006-01-02"))
	}
	if _, _, ok := reportPeriod("case:INITIAL_REVIEW", now); ok {
		t.Error("a per-case rule is not a calendar report")
	}
}

func TestClosureReason(t *testing.T) {
	for _, c := range []struct{ tpl, elig, reason, outcome string }{
		{"ineligible", "over_filing_window", "Dismissed-Timeliness eligibility failed", "Ineligible - Closed"},
		{"ineligible", "below_threshold", "Other", "Ineligible - Closed"},
		{"plan_opt_out", "", "Plan Opt-Out", "Plan Opt-Out"},
		{"refund", "", "Refunded", "Refunded"},
		{"dismissal", "", "Provider No Response", "Dismissed"},
		{"withdrawal", "", "Other", "Withdrawn"},
		{"acceptance", "", "", ""},
	} {
		if r, o := closureReasonFor(c.tpl, c.elig); r != c.reason || o != c.outcome {
			t.Errorf("%s/%s: got %q/%q, want %q/%q", c.tpl, c.elig, r, o, c.reason, c.outcome)
		}
	}
}

func TestReminderDue(t *testing.T) {
	due := d("2026-11-10")
	for today, want := range map[string]string{
		"2026-11-01": "",                  // more than a week out
		"2026-11-03": "payment_reminder",  // a week before
		"2026-11-10": "payment_reminder",  // the due date itself is not past due
		"2026-11-11": "past_due_reminder", // the day after
		"2026-12-25": "past_due_reminder",
	} {
		if got := reminderDue(due, d(today), 7); got != want {
			t.Errorf("invoice due Nov 10, today %s: got %q, want %q", today, got, want)
		}
	}
}

func TestFlReportReadsRecordedFields(t *testing.T) {
	c := &flCaseRow{
		CaseNumber: "FL26-000001", Status: "NEGOTIATION_TRACKED", Internal: "Review In Progress", Agency: "Under Review",
		OpenedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), ProviderOrg: "Sunshine Clinic", HealthPlan: "Gulf Health Plan",
		ProviderAddr: map[string]any{"city": "Miami", "state": "FL"}, ClaimsSubmitted: 12, ClaimsReviewed: 11,
		Details: map[string]any{"withdrawal_dismissed_reason": "Provider No Response", "filing_party_type": "PROVIDER",
			"review_notes": map[string]any{"external": map[string]any{"text": "plan asked for an extension"},
				"legal_review": map[string]any{"text": "INTERNAL ONLY"}}},
		Dates: map[string]any{
			"received_at": "2026-01-06", "date_acknowledgment_letter_sent_to_provider": "2026-01-10",
			"date_provider_response_due": "2026-01-10", "date_provider_responded": "2026-01-12",
			"date_acknowledgment_letter_sent_to_health_plan": "2026-01-13", "date_health_plan_response_due": "2026-01-28",
			"date_plan_responded": "2026-01-20", "date_recommendation_sent_to_agency": "2026-02-20", "date_final_order_sent": "2026-03-01",
		},
		InitialFee: &flFee{AmountCents: 12359, PaidAt: "2026-01-06"},
		Invoices:   []flInv{{Party: "HEALTH_PLAN", Status: "PAID", AmountCents: 250000, PaidAt: "2026-03-20"}},
	}
	w := c.weeklyRow()
	want := []string{"Under Review", "FL26-000001", "01/06/2026", "01/10/2026", "12", "01/10/2026", "01/12/2026", "01/13/2026",
		"01/28/2026", "01/20/2026", "11", "03/07/2026", "02/20/2026", "TBD - In Process", "No", "plan asked for an extension"}
	for i := range want {
		if w[i] != want[i] {
			t.Errorf("weekly column %c (%s): got %q, want %q", 'A'+i, flWeeklyHeaders[i], w[i], want[i])
		}
	}
	m := c.monthlyRow()
	get := func(h string) string {
		for i, x := range flMonthlyHeaders {
			if x == h {
				return m[i]
			}
		}
		t.Fatalf("no monthly column %q", h)
		return ""
	}
	for h, v := range map[string]string{
		"FL AHCA Status": "Under Review", "Case Status": "Review In Progress", "Provider Organization": "Sunshine Clinic",
		"Provider City": "Miami", "Provider State": "FL", "Health Plan": "Gulf Health Plan", "Date Final Order Sent": "03/01/2026",
		"Withdrawal/Dismissed Reason": "Provider No Response", "Initial Review Fee Who Paid": "Provider",
		"Initial Review Fee Amount Paid": "$123.59", "Initial Review Fee Date of Payment": "01/06/2026",
		"Health Plan Review Cost": "$2500.00", "Health Plan Paid": "Yes", "Health Plan Date Paid": "03/20/2026",
		"Provider Review Cost": "N/A", "Invoice Number": "FL26-000001",
	} {
		if got := get(h); got != v {
			t.Errorf("monthly %q: got %q, want %q", h, got, v)
		}
	}
	for _, cell := range append(w, m...) {
		if strings.Contains(cell, "INTERNAL ONLY") || cell == "NEGOTIATION_TRACKED" {
			t.Errorf("an internal-only value reached a report: %q", cell)
		}
	}
	// Nothing recorded: N/A, never a zero or an engine code.
	empty := &flCaseRow{CaseNumber: "FL26-000002", Status: "PENDING_INTAKE", Details: map[string]any{}, Dates: map[string]any{}}
	e := empty.weeklyRow()
	if e[0] != "Pending Initial Review" || e[4] != "N/A" || e[10] != "N/A" {
		t.Errorf("a paid case with no status reads Pending Initial Review, unrecorded counts N/A; got %q %q %q", e[0], e[4], e[10])
	}
}

func TestFlDefaultPeriod(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC) // a Wednesday
	if f, to := flDefaultPeriod("fl-weekly-report", now); f != "2026-10-05" || to != "2026-10-11" {
		t.Errorf("weekly covers the previous Mon-Sun: got %s..%s", f, to)
	}
	if f, to := flDefaultPeriod("fl-monthly-report", now); f != "2026-09-01" || to != "2026-09-30" {
		t.Errorf("monthly covers the previous month: got %s..%s", f, to)
	}
}

func TestReportAgencyStatusIsOneOfFive(t *testing.T) {
	for stored, want := range map[string]string{
		"Pending Initial Review": "Pending Initial Review", "Awaiting Provider Response": "Awaiting Provider Response",
		"Awaiting Plan Response": "Awaiting Plan Response", "Under Review": "Under Review", "Decided": "Decided",
		"Closed": "Decided", "Other": "Pending Initial Review", "": "Pending Initial Review",
		"NEGOTIATION_TRACKED": "Pending Initial Review",
	} {
		got := reportAgencyStatus(stored)
		if got != want {
			t.Errorf("stored %q reports as %q, want %q", stored, got, want)
		}
		if !contains(flReportedStatuses, got) {
			t.Errorf("stored %q reports as %q, which is not one of the five allowed values", stored, got)
		}
	}
	if reminderDue(d("2026-11-10"), d("2026-11-06"), 3) != "" || reminderDue(d("2026-11-10"), d("2026-11-07"), 3) != "payment_reminder" {
		t.Error("the reminder lead time should follow the setting")
	}
}

func TestValidPostal(t *testing.T) {
	for _, ok := range []string{"32301", "33101-1234"} {
		if !validPostal(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "3230", "323011", "32301-12", "3230A", "32301 1234", "33101_1234"} {
		if validPostal(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestERA835(t *testing.T) {
	x12 := "ISA*00*          *00*          *ZZ*PAYER          *ZZ*PROVIDER       *260301*1200*^*00501*000000001*0*P*:~" +
		"GS*HP*PAYER*PROVIDER*20260301*1200*1*X*005010X221A1~ST*835*0001~" +
		"BPR*I*1500.00*C*ACH*CCP*01*999999999*DA*123456*1512345678**01*888888888*DA*654321*20260305~TRN*1*EFT778899*1512345678~" +
		"N1*PR*GULF HEALTH PLAN~N1*PE*SUNSHINE CLINIC*XX*1234567890~" +
		"CLP*CLM-001*1*1850.00*412.00*82.40*12*PAYERCLM1*11~NM1*QC*1*DOE*JANE~DTM*232*20260312~CAS*CO*45*1355.60~CAS*PR*2*82.40~" +
		"CLP*CLM-002*4*500.00*0.00**12*PAYERCLM2*11~NM1*QC*1*ROE*SAM~DTM*472*20260315~CAS*CO*29*500.00~SE*20*0001~GE*1*1~IEA*1*000000001~"
	got := parseERA835(x12)
	if len(got) != 2 {
		t.Fatalf("want 2 claims, got %d", len(got))
	}
	a := got[0]
	if a.ClaimNumber != "CLM-001" || a.Billed != "1850.00" || a.Paid != "412.00" || a.PatientResp != "82.40" ||
		a.Status != "Processed as primary" || a.Patient != "JANE DOE" || a.ServiceDate != "03/12/2026" ||
		a.Payer != "GULF HEALTH PLAN" || a.Payee != "SUNSHINE CLINIC" || a.CheckNumber != "EFT778899" || a.CheckDate != "03/05/2026" {
		t.Errorf("first claim read wrong: %+v", a)
	}
	if strings.Join(a.Adjustments, "; ") != "CO-45 1355.60; PR-2 82.40" {
		t.Errorf("adjustments read wrong: %v", a.Adjustments)
	}
	if got[1].Status != "Denied" || got[1].ServiceDate != "03/15/2026" {
		t.Errorf("second claim read wrong: %+v", got[1])
	}
	// one segment per line, no ISA header (how a text export often arrives)
	lines := "ST*835*0001\nBPR*I*10.00*C\nTRN*1*CHK1\nN1*PR*PLAN\nCLP*A1*1*20.00*10.00*0\nSE*5*0001"
	if c := parseERA835(strings.ReplaceAll(lines, "\\n", "\n")); len(c) != 1 || c[0].ClaimNumber != "A1" {
		t.Errorf("newline-delimited 835 not read: %+v", c)
	}
	if parseERA835("Dear provider, your claim CLP was reviewed.") != nil || parseERA835("") != nil {
		t.Error("ordinary text must not be taken for an 835")
	}
}

func TestAdhocColumns(t *testing.T) {
	cols := flAllColumns()
	if len(cols) != 16+25 {
		t.Fatalf("an ad hoc report can draw on the 16 weekly columns plus the 25 monthly-only ones, got %d", len(cols))
	}
	c := &flCaseRow{CaseNumber: "FL26-000001", Agency: "Under Review", Internal: "Review In Progress", Details: map[string]any{}, Dates: map[string]any{}}
	if v, ok := flCell(c, "Case Number"); !ok || v != "FL26-000001" {
		t.Errorf("Case Number: %q %v", v, ok)
	}
	if v, ok := flCell(c, "Case Status"); !ok || v != "Review In Progress" {
		t.Errorf("Case Status (monthly-only): %q %v", v, ok)
	}
	for _, internal := range []string{"Legal Review notes", "Rationale", "Plan Type", "RFI Details"} {
		if _, ok := flCell(c, internal); ok {
			t.Errorf("internal-only field %q must not be reportable", internal)
		}
	}
}
