package main

import "testing"

func TestFeeEstimate(t *testing.T) {
	off := &feeEstimateConfig{Method: "flat", FlatCents: 50000}
	if _, ok := off.estimate(1, 100); ok {
		t.Fatal("switched off gives no estimate")
	}
	flat := &feeEstimateConfig{Enabled: true, Method: "flat", FlatCents: 50000}
	if f, _ := flat.estimate(9, 1); f != 50000 {
		t.Fatalf("flat %d", f)
	}
	per := &feeEstimateConfig{Enabled: true, Method: "per_claim", PerClaimCents: 2500, MinCents: 10000, MaxCents: 60000}
	for _, c := range []struct {
		claims int
		want   int64
	}{{0, 10000}, {2, 10000}, {6, 15000}, {100, 60000}} {
		if f, _ := per.estimate(c.claims, 0); f != c.want {
			t.Fatalf("per claim %d: %d, want %d", c.claims, f, c.want)
		}
	}
	tiers := &feeEstimateConfig{Enabled: true, Method: "tiered", Tiers: []feeEstimateTier{{100000, 30000}, {1000000, 60000}}}
	if f, _ := tiers.estimate(1, 100000); f != 30000 {
		t.Fatalf("tier 1: %d", f)
	}
	if f, _ := tiers.estimate(1, 100001); f != 60000 {
		t.Fatalf("tier 2: %d", f)
	}
	if _, ok := tiers.estimate(1, 2000000); ok {
		t.Fatal("above the last band gives no estimate")
	}
	tiers.Tiers = append(tiers.Tiers, feeEstimateTier{0, 90000})
	if f, _ := tiers.estimate(1, 2000000); f != 90000 {
		t.Fatalf("open last band: %d", f)
	}
}

func TestFeeEstimateProblems(t *testing.T) {
	if p := (&feeEstimateConfig{}).problems(); len(p) != 0 {
		t.Fatalf("empty and off is fine: %v", p)
	}
	if p := (&feeEstimateConfig{Enabled: true}).problems(); len(p) != 1 {
		t.Fatalf("on without a method: %v", p)
	}
	if p := (&feeEstimateConfig{Enabled: true, Method: "per_claim"}).problems(); len(p) != 1 {
		t.Fatalf("per claim without a fee: %v", p)
	}
	bad := &feeEstimateConfig{Enabled: true, Method: "tiered", MinCents: 5, MaxCents: 1,
		Tiers: []feeEstimateTier{{0, 1}, {500, 2}, {400, 3}}}
	if p := bad.problems(); len(p) != 3 {
		t.Fatalf("min>max, open band not last, bands not rising: %v", p)
	}
	if p := (&feeEstimateConfig{Method: "magic"}).problems(); len(p) != 1 {
		t.Fatalf("unknown method: %v", p)
	}
}
