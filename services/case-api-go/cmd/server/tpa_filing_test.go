package main

import (
	"testing"
)

func i64(v int64) *int64 { return &v }

func TestDiffFilingNamesEveryChange(t *testing.T) {
	before := filingState{
		Details: map[string]any{"provider_npi": "1821186313", "health_plan": "Plan A", "contracted": false},
		Amount:  i64(240000),
		Claims: []packetClaim{
			{ClaimNumber: "C1", PatientFirstName: "Jane", DisputedCents: i64(100000)},
			{ClaimNumber: "C2", DateOfService: "2026-01-02"},
		},
	}
	after := filingState{
		Details: map[string]any{"provider_npi": "1821186313", "health_plan": "Plan B", "contracted": true, "provider_city": "Miami"},
		Amount:  i64(250000),
		Claims: []packetClaim{
			{ClaimNumber: "C1", PatientFirstName: "Jane", DisputedCents: i64(120000)},
			{ClaimNumber: "C3"},
		},
	}
	got := map[string]fieldChange{}
	for _, c := range diffFiling(before, after) {
		got[c.Field] = c
	}
	want := map[string]fieldChange{
		"contracted":                  {"contracted", "No", "Yes"},
		"health_plan":                 {"health_plan", "Plan A", "Plan B"},
		"provider_city":               {"provider_city", "", "Miami"},
		"disputed_amount":             {"disputed_amount", "$2400.00", "$2500.00"},
		"claim C1: amount in dispute": {"claim C1: amount in dispute", "$1000.00", "$1200.00"},
		"claim C3":                    {"claim C3", "", "added"},
		"claim C2":                    {"claim C2", "on the dispute", "removed"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes %v, want %d", len(got), got, len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %+v, want %+v", k, got[k], w)
		}
	}
	if _, ok := got["provider_npi"]; ok {
		t.Error("an unchanged value must not be listed")
	}
}

func TestDiffFilingNothingChanged(t *testing.T) {
	st := filingState{Details: map[string]any{"health_plan": "P"}, Amount: i64(5), Claims: []packetClaim{{ClaimNumber: "C1"}}}
	if d := diffFiling(st, st); len(d) != 0 {
		t.Fatalf("no change expected, got %v", d)
	}
}

func TestFilingModeFollowsTheFee(t *testing.T) {
	for status, want := range map[string]string{
		"AWAITING_PAYMENT": "direct", "PENDING_INTAKE": "request", "IN_REVIEW": "request",
		"CLOSED": "closed", "CLOSED_DETERMINED": "closed", "WITHDRAWN": "closed", "SETTLED_IN_NEGOTIATION": "closed",
	} {
		if got := filingMode(status); got != want {
			t.Errorf("%s: got %s, want %s", status, got, want)
		}
	}
}

func TestClientScopedLoginsReachOnlyTheirOwnDisputesFiling(t *testing.T) {
	client := tpaLogin{TPAID: "t", ClientID: "c"}
	finance := tpaLogin{TPAID: "t", Role: "FINANCE"}
	id := "11111111-1111-1111-1111-111111111111"
	for _, c := range []struct {
		l      tpaLogin
		method string
		path   string
		ok     bool
	}{
		{client, "GET", "/tpa/disputes/" + id + "/filing", true},
		{client, "PUT", "/tpa/disputes/" + id + "/filing", true},
		{client, "POST", "/tpa/disputes/" + id + "/change-requests", true},
		{client, "DELETE", "/tpa/disputes/" + id + "/change-requests/" + id, true},
		{finance, "GET", "/tpa/disputes/" + id + "/filing", true},
		{finance, "PUT", "/tpa/disputes/" + id + "/filing", false},
		{finance, "POST", "/tpa/disputes/" + id + "/change-requests", false},
		{client, "POST", "/cases/" + id + "/change-requests/" + id + "/decide", false},
	} {
		ok, check, gotID := tpaScopedDecision(c.l, c.method, c.path)
		if ok != c.ok {
			t.Errorf("%s %s for %+v: allowed=%v, want %v", c.method, c.path, c.l, ok, c.ok)
		}
		if ok && c.l.ClientID != "" && (check != "case" || gotID != id) {
			t.Errorf("%s %s: must be checked against the login's client by case, got %q %q", c.method, c.path, check, gotID)
		}
	}
}
