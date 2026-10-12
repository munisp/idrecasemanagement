package main

import (
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
		{"ineligible", "over_filing_window", "Dismissed-Timeliness eligibility failed", "Dismissed"},
		{"ineligible", "below_threshold", "Other", "Dismissed"},
		{"dismissal", "", "Provider No Response", "Dismissed"},
		{"withdrawal", "", "Other", "Withdrawn"},
		{"acceptance", "", "", ""},
	} {
		if r, o := closureReasonFor(c.tpl, c.elig); r != c.reason || o != c.outcome {
			t.Errorf("%s/%s: got %q/%q, want %q/%q", c.tpl, c.elig, r, o, c.reason, c.outcome)
		}
	}
}
