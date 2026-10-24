package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The filing packet form as the portal posts it: the Exhibit 2 sample (Shands Jacksonville / Molina).
const samplePacket = `{
  "details": {
    "filing_party_type": "provider", "provider_org": "Shands Jacksonville Medical Center Inc",
    "provider_legal_entity": "Shands Jacksonville Medical Center, Inc.", "provider_tin": "59-1234567",
    "provider_npi": "1821186313", "provider_street": "655 W 8th St", "provider_city": "Jacksonville",
    "provider_state": "Florida", "provider_postal": "32209-6511", "provider_email": "appeals@example.test",
    "packet_provider_type": "hospital", "health_plan": "Molina Healthcare of Florida", "plan_type": "Medicaid HMO",
    "contracted": false, "internal_dispute_exhausted": "yes", "type_of_service": "Hospital Inpatient",
    "claims_submitted": 2, "fee_estimate_requested": true,
    "packet_documents_submitted": ["claim_forms", "denials_eobs"],
    "packet_attestation": {"interest_only": true, "rural_hospital": false},
    "attested_on": "2025-10-15", "provider_position_basis": "Medically necessary inpatient stay."
  },
  "disputed_amount_cents": 75256477,
  "claims": [
    {"claim_number": "25118100868", "patient_last_name": "Doe", "date_of_service": "2024-05-24",
     "billed_cents": 72446961, "paid_cents": 0},
    {"claim_number": "", "patient_last_name": ""}
  ]
}`

func TestFilingPacketNormalize(t *testing.T) {
	var pk filingPacket
	if err := json.Unmarshal([]byte(samplePacket), &pk); err != nil {
		t.Fatal(err)
	}
	d, disputed, claims, problems := pk.normalize()
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	want := map[string]any{
		"filing_party_type": "PROVIDER", "provider_state": "FL", "packet_provider_type": "Hospital",
		"plan_type": "Medicaid HMO", "line_of_business": "Medicaid", "contracted": false, "out_of_network": "Yes",
		"internal_dispute_exhausted": "Yes", "type_of_service": "Hospital Inpatient", "provider_type": "hospital_inpatient",
		"claims_submitted": int64(2), "fee_estimate_requested": true, "attested_on": "2025-10-15",
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("%s = %#v, want %#v", k, d[k], v)
		}
	}
	if att := d["packet_attestation"].(map[string]bool); !att["interest_only"] || att["rural_hospital"] {
		t.Errorf("attestation = %v", att)
	}
	if *disputed != 75256477 {
		t.Errorf("disputed = %d", *disputed)
	}
	if len(claims) != 1 || claims[0].ClaimNumber != "25118100868" || *claims[0].DisputedCents != 72446961 {
		t.Errorf("claims = %+v (the blank row is dropped, the disputed amount is billed less paid)", claims)
	}
}

func TestFilingPacketProblemsAreAllReported(t *testing.T) {
	var pk filingPacket
	_ = json.Unmarshal([]byte(`{"details": {"provider_state": "Flo", "provider_npi": "123", "plan_type": "Gold",
		"claims_submitted": 1.5, "attested_on": "10/15/2025", "packet_attestation": {"made_up": true}},
		"claims": [{"claim_number": "A1", "date_of_service": "2024-13-01"}, {"claim_number": "A1"},
		{"patient_last_name": "Doe"}]}`), &pk)
	_, _, _, problems := pk.normalize()
	all := strings.Join(problems, "\n")
	for _, want := range []string{"provider_state", "provider_npi", "plan_type", "claims_submitted", "attested_on",
		"unknown criterion made_up", "claims row 1: date of service", "claim A1 is listed twice", "claims row 3: claim # is required"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing problem %q in:\n%s", want, all)
		}
	}
}

func TestPacketProgramProviderType(t *testing.T) {
	for _, c := range []struct{ ptype, service, want string }{
		{"Rural Hospital", "", "rural_hospital"}, {"Hospital", "Hospital Outpatient", "hospital_outpatient"},
		{"Hospital", "", ""}, {"Dentist", "Professional", "physician_dentist"}, {"Pharmacy", "Other", "other"}, {"", "", ""},
	} {
		if got := packetProgramProviderType(c.ptype, c.service); got != c.want {
			t.Errorf("%q/%q = %q, want %q", c.ptype, c.service, got, c.want)
		}
	}
}

func TestPacketOtherParty(t *testing.T) {
	var pk filingPacket
	_ = json.Unmarshal([]byte(`{"details": {"health_plan": "Molina Healthcare of Florida", "health_plan_email": "disputes@molina.example",
		"provider_org": "Shands Jacksonville", "provider_email": "appeals@shands.example", "provider_npi": "1821186313"}}`), &pk)
	// A provider files: the health plan is the other side, its notice email from the packet's plan contact.
	if got := pk.otherParty(respondent{Side: otherSide("PROVIDER")}); got.Side != "HEALTH_PLAN" || got.Name != "Molina Healthcare of Florida" || got.Email != "disputes@molina.example" {
		t.Errorf("plan side = %+v", got)
	}
	// What the filing screen's own fields say wins over the packet.
	if got := pk.otherParty(respondent{Side: "HEALTH_PLAN", Email: "notices@molina.example"}); got.Email != "notices@molina.example" {
		t.Errorf("explicit email lost: %+v", got)
	}
	// A health plan files: the provider is the other side, with its NPI.
	if got := pk.otherParty(respondent{Side: otherSide("HEALTH_PLAN")}); got.Side != "PROVIDER" || got.Email != "appeals@shands.example" || got.NPI != "1821186313" {
		t.Errorf("provider side = %+v", got)
	}
	var none *filingPacket
	if got := none.otherParty(respondent{Side: "HEALTH_PLAN", Name: "X"}); got.Name != "X" {
		t.Errorf("no packet = %+v", got)
	}
	var bad filingPacket
	_ = json.Unmarshal([]byte(`{"details": {"health_plan_email": "not-an-email"}}`), &bad)
	if _, _, _, problems := bad.normalize(); len(problems) != 1 || !strings.Contains(problems[0], "health_plan_email") {
		t.Errorf("problems = %v", problems)
	}
}

func TestIntakeChatNeedsTheOtherParty(t *testing.T) {
	f := validateIntakeFields(intakeFields{Email: "filer@clinic.example", Org: "Clinic", DisputedAmountCents: 100,
		OtherPartyName: "Bay Health Plan", OtherPartyEmail: "filer@clinic.example"})
	if f.OtherPartyEmail != "" {
		t.Errorf("the filer's own email was taken as the other party's: %+v", f)
	}
	if m := intakeMissingFor(f, true); len(m) != 1 || !strings.Contains(m[0], "email for dispute notices") {
		t.Errorf("missing = %v", m)
	}
	if m := intakeMissingFor(f, false); len(m) != 0 {
		t.Errorf("a federal tenant does not need it: %v", m)
	}
	f.OtherPartyEmail = "disputes@bayhealth.example"
	if m := intakeMissingFor(validateIntakeFields(f), true); len(m) != 0 {
		t.Errorf("complete intake still missing %v", m)
	}
}
