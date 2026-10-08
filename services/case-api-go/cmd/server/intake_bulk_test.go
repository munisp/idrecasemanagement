package main

import "testing"

func TestBulkValidateRequiresEmail(t *testing.T) {
	if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "not-an-email"}); msg == "" {
		t.Fatal("bad email must be rejected")
	}
	if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "filer@rcm.example"}); msg != "" {
		t.Fatalf("valid row rejected: %s", msg)
	}
}

func TestBulkValidateFilingPartyCodes(t *testing.T) {
	if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "a@b.c", FilingPartyType: "EMPLOYER"}); msg == "" {
		t.Fatal("unknown filing party code must be rejected")
	}
	for _, ok := range []string{"", "PROVIDER", "HEALTH_PLAN", "provider"} {
		if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "a@b.c", FilingPartyType: ok}); msg != "" {
			t.Fatalf("%q should pass: %s", ok, msg)
		}
	}
}

func TestBulkValidateAmounts(t *testing.T) {
	if msg := validateBulkIntakeItem(bulkIntakeItem{Email: "a@b.c", DisputedAmountCents: -5}); msg == "" {
		t.Fatal("negative amounts must be rejected")
	}
}

func TestBulkMaxItemsGuard(t *testing.T) {
	// The guard lives in the handler; the constant is the contract — pin it
	// so a careless edit can't silently uncap batch size.
	if bulkIntakeMaxItems <= 0 || bulkIntakeMaxItems > 1000 {
		t.Fatalf("bulkIntakeMaxItems out of sane range: %d", bulkIntakeMaxItems)
	}
}
