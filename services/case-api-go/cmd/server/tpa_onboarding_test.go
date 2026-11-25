// Author: Patrick Munis at newwave
package main

import "testing"

func TestNPIValid(t *testing.T) {
	// 1234567893 is the canonical valid NPI example from CMS docs.
	if !npiValid("1234567893") {
		t.Fatal("canonical valid NPI rejected")
	}
	for _, bad := range []string{"", "123456789", "12345678901", "1234567894", "12345678a3"} {
		if npiValid(bad) {
			t.Fatalf("invalid NPI %q accepted", bad)
		}
	}
}

func TestNormTINThenTINProblem(t *testing.T) {
	for _, ok := range []string{"591234567", "12-3456789", "123 45 6789", "123456789"} {
		if _, msg := tinCheck(ok); msg != "" {
			t.Errorf("%q refused: %s", ok, msg)
		}
	}
	for _, bad := range []string{"", "12345678", "1234567890", "abcdefghi", "111111111", "00-1234567", "07-1234567", "000-12-3456", "666-12-3456"} {
		if _, msg := tinCheck(bad); msg == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRoutingValid(t *testing.T) {
	// 021000021 (JPMorgan Chase) — canonical valid ABA number.
	if !routingValid("021000021") {
		t.Fatal("canonical valid routing number rejected")
	}
	for _, bad := range []string{"", "02100002", "0210000211", "021000022", "02100002a"} {
		if routingValid(bad) {
			t.Fatalf("invalid routing %q accepted", bad)
		}
	}
}

func TestTPASlotFromFilename(t *testing.T) {
	cases := map[string]string{
		"EOB_claim42.pdf":             "EOB",
		"itemized_bill.pdf":           "ITEMIZED_BILL",
		"proof_of_coverage.pdf":       "PROOF_OF_COVERAGE",
		"open_negotiation_notice.pdf": "OPEN_NEGOTIATION_NOTICE",
		"offer_substantiation.pdf":    "OFFER_SUBSTANTIATION",
		"idre_attestation.pdf":        "IDRE_ATTESTATION",
		"random_notes.txt":            "",
	}
	for name, want := range cases {
		if got := tpaSlotFromFilename(name); got != want {
			t.Fatalf("%s: got %q want %q", name, got, want)
		}
	}
}

func TestTPASlotsComplete(t *testing.T) {
	if len(tpaSlots) != 6 {
		t.Fatalf("expected 6 required slots, got %d", len(tpaSlots))
	}
	seen := map[string]bool{}
	for _, s := range tpaSlots {
		if seen[s] || !tpaSlotValid(s) {
			t.Fatalf("slot %q duplicate or invalid", s)
		}
		seen[s] = true
	}
}

func TestMoreDocSlots(t *testing.T) {
	s := newMoreSlot("MEDICAL_RECORDS")
	if moreDocKind(s) != "MEDICAL_RECORDS" || !tpaSlotValid(s) {
		t.Fatalf("new slot %q not recognized", s)
	}
	if moreDocKind("EOB") != "" || moreDocKind("MORE_NOPE_ABCDEF12") != "" || moreDocKind("MORE_OTHER_xyz") != "" {
		t.Error("only MORE_<kind>_<8 hex> slots are additional documents")
	}
	if got := slotForUpload("EOB"); got != "EOB" {
		t.Errorf("the six upload as themselves, got %q", got)
	}
	if got := slotForUpload("MORE_CONTRACT"); moreDocKind(got) != "CONTRACT" {
		t.Errorf("a kind gets a fresh slot, got %q", got)
	}
	if a, b := slotForUpload("MORE_OTHER"), slotForUpload("MORE_OTHER"); a == b {
		t.Error("each additional document gets its own slot")
	}
	if slotForUpload("MORE_NOPE") != "" || slotForUpload("RANDOM") != "" {
		t.Error("unknown kinds are refused")
	}
	for name, want := range map[string]string{"medical_records.pdf": "MEDICAL_RECORDS", "provider_contract.pdf": "CONTRACT",
		"fl_license.pdf": "FLORIDA_LICENSE", "overpayment_notice.pdf": "NOTICE_OF_OVERPAYMENT", "scan001.pdf": "OTHER"} {
		if got := moreKindFromFilename(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	if packageDocType(newMoreSlot("CODING_RULES")) != "coding_rules" {
		t.Error("an additional document goes onto the case as its kind")
	}
}
