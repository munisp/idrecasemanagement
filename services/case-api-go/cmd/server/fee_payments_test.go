package main

import "testing"

// A payer pays only for its own disputes, through its own column, and comes
// back to its own page.
func TestPayerScope(t *testing.T) {
	tpa, plan := payer{kind: "tpa", id: "t1"}, payer{kind: "plan", id: "p1"}
	if tpa.originCol() != "tpa_id" || plan.originCol() != "health_plan_id" {
		t.Errorf("origin columns: %s, %s", tpa.originCol(), plan.originCol())
	}
	if tpa.home() != "#/tpa" || plan.home() != "#/healthplan" {
		t.Errorf("home pages: %s, %s", tpa.home(), plan.home())
	}
	if tpa.org() != "tpa:t1" || plan.org() != "plan:p1" {
		t.Errorf("audit orgs: %s, %s", tpa.org(), plan.org())
	}
}

func TestUSD(t *testing.T) {
	for c, want := range map[int64]string{12359: "$123.59", 5: "$0.05", 30000: "$300.00"} {
		if got := usd(c); got != want {
			t.Errorf("usd(%d) = %s, want %s", c, got, want)
		}
	}
}
