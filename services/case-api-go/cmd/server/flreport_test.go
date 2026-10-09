package main

import (
	"testing"
	"time"
)

func TestFlReportFormats(t *testing.T) {
	if mmdd("2026-10-09") != "10/09/2026" {
		t.Fatal("ISO date must render MM/DD/YYYY")
	}
	if mmdd("") != "N/A" || mmdd("garbage") != "N/A" {
		t.Fatal("unrecorded/unparseable dates must render N/A")
	}
	if addDays("2026-01-01", 60) != "03/02/2026" {
		t.Fatalf("60-day recommendation clock wrong: %s", addDays("2026-01-01", 60))
	}
	if addDays("", 15) != "N/A" {
		t.Fatal("no basis date => N/A, never an invented date")
	}
	if money(12359) != "$123.59" || money(0) != "N/A" {
		t.Fatal("money format")
	}
}

func TestFlWeeklyRowShape(t *testing.T) {
	c := &flCaseRow{
		CaseNumber: "FL26-000001", Status: "Under Review",
		OpenedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		Details:  map[string]any{},
		Dates: map[string]any{
			"ack_provider_letter_at": "2026-01-10",
			"estimate_sent_at":       "2026-01-10",
			"plan_notified_at":       "2026-02-01",
		},
		ClaimsSubmitted: 12,
	}
	row := c.weeklyRow()
	if len(row) != 16 {
		t.Fatalf("weekly report must be exactly 16 columns A-P, got %d", len(row))
	}
	// Provider response due = ack + 15 because an estimate was sent.
	if row[5] != "01/25/2026" {
		t.Fatalf("provider due should be ack+15 with estimate, got %s", row[5])
	}
	// Plan response due = plan notified + 15.
	if row[8] != "02/16/2026" {
		t.Fatalf("plan due wrong: %s", row[8])
	}
	// Recommendation due = received + 60.
	if row[11] != "03/06/2026" {
		t.Fatalf("recommendation due wrong: %s", row[11])
	}
	// No outcome recorded => contract placeholder.
	if row[13] != "TBD – case in process" {
		t.Fatalf("outcome default wrong: %s", row[13])
	}
	// No estimate/RFI => provider due equals the acknowledgment date itself.
	c.Dates = map[string]any{"ack_provider_letter_at": "2026-01-10"}
	if r2 := c.weeklyRow(); r2[5] != "01/10/2026" {
		t.Fatalf("provider due without estimate/RFI must equal ack date, got %s", r2[5])
	}
}

func TestFlMonthlyRowShape(t *testing.T) {
	c := &flCaseRow{
		CaseNumber: "FL26-000001", Status: "Under Review",
		OpenedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		Details:  map[string]any{"health_plan": "Sunshine", "party_billed": "Health Plan"},
		Dates:    map[string]any{},
		Invoices: []flInv{{Party: "HEALTH_PLAN", Status: "PAID", AmountCents: 450000, PaidAt: "2026-03-01"}},
	}
	row := c.monthlyRow()
	if len(row) != 33 {
		t.Fatalf("monthly report must be exactly 33 fields, got %d", len(row))
	}
	if row[26] != "FL26-000001" {
		t.Fatal("invoice number must equal case number")
	}
	if row[27] != "$4,500.00" && row[27] != "$4500.00" {
		t.Fatalf("plan review cost wrong: %s", row[27])
	}
	if row[28] != "Yes" || row[29] != "03/01/2026" {
		t.Fatalf("plan paid columns wrong: %s %s", row[28], row[29])
	}
	// Provider never invoiced => all three N/A.
	if row[30] != "N/A" || row[31] != "N/A" || row[32] != "N/A" {
		t.Fatal("uninvoiced party must be N/A across cost/paid/date")
	}
}
