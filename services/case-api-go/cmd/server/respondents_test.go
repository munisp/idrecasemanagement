package main

import "testing"

func TestRespondentCheck(t *testing.T) {
	ok := respondent{Side: "HEALTH_PLAN", Name: " Sunshine Health ", Email: "notices@sunshine.test", NAIC: "12345"}
	if msg := ok.check(); msg != "" || ok.Name != "Sunshine Health" {
		t.Fatalf("valid plan refused: %q (%q)", msg, ok.Name)
	}
	for name, r := range map[string]respondent{
		"no name":   {Side: "HEALTH_PLAN", Email: "a@b.test"},
		"no email":  {Side: "HEALTH_PLAN", Name: "Plan"},
		"bad email": {Side: "HEALTH_PLAN", Name: "Plan", Email: "not-an-email"},
		"bad NAIC":  {Side: "HEALTH_PLAN", Name: "Plan", Email: "a@b.test", NAIC: "12"},
		"bad NPI":   {Side: "PROVIDER", Name: "Clinic", Email: "a@b.test", NPI: "1234567890"},
	} {
		if msg := r.check(); msg == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	prov := respondent{Side: "PROVIDER", Name: "Clinic", Email: "a@b.test", NPI: "1234567893"}
	if msg := prov.check(); msg != "" {
		t.Fatalf("valid provider refused: %q", msg)
	}
}
