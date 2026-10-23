package main

import "testing"

func TestTPAGroupCheck(t *testing.T) {
	str := func(v string) *string { return &v }
	g := tpaGroupIn{Name: str("  Coastal Physicians  "), TIN: str("59 1234567"), NPI: str("1234567893")}
	if msg := g.check(); msg != "" || *g.Name != "Coastal Physicians" || *g.TIN != "59-1234567" {
		t.Fatalf("valid group refused or not tidied: %q %q %q", msg, *g.Name, *g.TIN)
	}
	for name, in := range map[string]tpaGroupIn{
		"blank name": {Name: str("  ")},
		"short TIN":  {Name: str("G"), TIN: str("12-34")},
		"bad NPI":    {Name: str("G"), NPI: str("1234567890")},
		"bad email":  {Name: str("G"), ContactEmail: str("nope")},
		"bad status": {Status: str("CLOSED")},
	} {
		if msg := in.check(); msg == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A login that holds any staff role besides TPA is never switched off when it leaves a TPA.
func TestOnlyTPARole(t *testing.T) {
	if !onlyTPARole([]string{"TPA", "default-roles-idre", "offline_access"}) {
		t.Error("a TPA-only login should be disabled on removal")
	}
	if onlyTPARole([]string{"TPA", "CASE_MANAGER"}) {
		t.Error("a staff login must not be disabled")
	}
}
