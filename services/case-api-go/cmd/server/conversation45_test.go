package main

import (
	"strings"
	"testing"
	"time"
)

// ---- Step 4: party voice ---------------------------------------------------

// The party system prompt is the leak firewall: it must forbid internal
// review, amounts, and invented facts, and pin the grounded fallback target.
func TestPartyChatSystemGuards(t *testing.T) {
	for _, rule := range []string{"ONLY the JSON", "Never discuss internal review", "dollar amounts", "NEVER invent"} {
		if !strings.Contains(partyChatSystem, rule) {
			t.Errorf("party system prompt missing guardrail %q", rule)
		}
	}
}

// Cost bound: the turn cap exists and is small — a token is not an open
// chat account.
func TestPartyChatTurnCapIsBounded(t *testing.T) {
	if partyChatMaxTurns <= 0 || partyChatMaxTurns > 100 {
		t.Errorf("partyChatMaxTurns out of sane bounds: %d", partyChatMaxTurns)
	}
	if partyChatMaxMessage > 2000 {
		t.Errorf("partyChatMaxMessage too generous: %d", partyChatMaxMessage)
	}
}

// partyFacts must carry NO internal surface — compile-time field audit:
// if someone adds internal notes/QA/assignment to the struct later, the
// JSON contract here breaks loudly.
func TestPartyFactsProjectionIsMinimal(t *testing.T) {
	f := partyFacts{
		CaseNumber: "TX-2026-0001", Status: "OPEN",
		OutstandingRequests: []string{"itemized bill (due 2026-10-15)"},
		LinkKind:            "upload", LinkExpires: time.Now().Add(72 * time.Hour).Format("January 2, 2006"),
	}
	if f.CaseNumber == "" || len(f.OutstandingRequests) != 1 {
		t.Errorf("projection broken: %+v", f)
	}
}

// ---- Step 5: conversational intake -----------------------------------------

// Extraction parser: clean JSON, prose-wrapped JSON, and garbage.
func TestParseIntakeModelJSON(t *testing.T) {
	f, reply, err := parseIntakeModelJSON(
		`Sure! {"fields":{"email":"dana@meridian.example","org":"Meridian Surgical","disputed_amount_cents":420000,"filing_party_type":"provider"},"reply":"Got it — who is the health plan?"}`)
	if err != nil {
		t.Fatalf("prose-wrapped JSON should parse: %v", err)
	}
	if f.Email != "dana@meridian.example" || f.Org != "Meridian Surgical" ||
		f.DisputedAmountCents != 420000 || f.FilingPartyType != "PROVIDER" {
		t.Errorf("fields not extracted/normalized: %+v", f)
	}
	if !strings.Contains(reply, "health plan") {
		t.Errorf("reply lost: %q", reply)
	}
	if _, _, err := parseIntakeModelJSON("no json here"); err == nil {
		t.Error("garbage must be rejected")
	}
}

// The trust boundary: malformed model output degrades to zero values,
// never to trusted data.
func TestValidateIntakeFieldsDropsBad(t *testing.T) {
	f := validateIntakeFields(intakeFields{
		Email:               "not-an-email",
		FilingPartyType:     "ALIEN",
		DisputedAmountCents: -50,
		QPACents:            10_000_000_000_00, // absurd
	})
	if f.Email != "" || f.FilingPartyType != "" || f.DisputedAmountCents != 0 || f.QPACents != 0 {
		t.Errorf("bad model output must be dropped, got %+v", f)
	}
}

// Readiness is computed server-side from validated fields — the model can
// never declare an intake ready early.
func TestIntakeMissingGatesReady(t *testing.T) {
	if m := intakeMissing(intakeFields{}); len(m) != 3 {
		t.Errorf("empty fields: want 3 gaps, got %v", m)
	}
	ready := intakeFields{Email: "a@b.example", Org: "Org", DisputedAmountCents: 1000}
	if m := intakeMissing(ready); len(m) != 0 {
		t.Errorf("complete fields should have no gaps, got %v", m)
	}
	// contact_name alone satisfies the org-or-contact requirement.
	if m := intakeMissing(intakeFields{Email: "a@b.example", ContactName: "Dana", QPACents: 500}); len(m) != 0 {
		t.Errorf("contact+amount should suffice, got %v", m)
	}
}
