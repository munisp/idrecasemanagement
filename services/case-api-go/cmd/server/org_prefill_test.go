package main

import "testing"

func TestOrgPrefillFrom(t *testing.T) {
	docs := []orgPrefillDoc{
		{Slot: "EOB", Filename: "eob.pdf", Fields: map[string]any{"payer_name": "Sunshine Health Plan", "payer_email": "claims@sun.example",
			"payer_naic": "12345", "paid_cents": float64(60000), "contracted": false}},
		{Slot: "ITEMIZED_BILL", Filename: "bill.pdf", Fields: map[string]any{"billed_cents": float64(245000), "provider_type": "physician_dentist",
			"provider_type_basis": "a professional practice", "payer_name": "Sunshine Health Plan"}},
	}
	got := orgPrefillFrom(docs, "tpa")
	if got["other_party_name"].Value != "Sunshine Health Plan" || got["other_party_name"].Confidence != "high" {
		t.Errorf("plan named on both documents: %+v", got["other_party_name"])
	}
	if got["other_party_email"].Value != "claims@sun.example" || got["other_party_naic"].Value != "12345" {
		t.Errorf("plan email and NAIC: %+v %+v", got["other_party_email"], got["other_party_naic"])
	}
	if got["amount"].Value != "1850.00" {
		t.Errorf("billed less paid: %+v", got["amount"])
	}
	if got["in_network"].Value != "no" || got["provider_type"].Value != "physician_dentist" {
		t.Errorf("network and provider type: %+v %+v", got["in_network"], got["provider_type"])
	}
	stated := orgPrefillFrom([]orgPrefillDoc{{Slot: "MORE_OTHER_1A2B3C4D", Filename: "form.pdf", Fields: map[string]any{"disputed_cents": float64(185000)}}}, "tpa")
	if stated["amount"].Value != "1850.00" {
		t.Errorf("a stated amount in dispute wins: %+v", stated["amount"])
	}
	if len(orgPrefillFrom([]orgPrefillDoc{{Slot: "EOB", Fields: map[string]any{}}}, "tpa")) != 0 {
		t.Error("nothing stated, nothing filled")
	}
	hp := orgPrefillFrom([]orgPrefillDoc{{Slot: "ITEMIZED_BILL", Filename: "b.pdf", Fields: map[string]any{"provider_name": "Gulf Coast ER", "provider_npi": "1234567893"}}}, "hp")
	if hp["other_party_name"].Value != "Gulf Coast ER" || hp["other_party_npi"].Value != "1234567893" {
		t.Errorf("a plan's filing names the provider: %+v", hp)
	}
}
