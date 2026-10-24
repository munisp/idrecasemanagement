// Author: Patrick Munis at newwave
package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestTPADisputeEditRefusal(t *testing.T) {
	cases := []struct {
		name                   string
		caller, origin, status string
		wantCode               int
	}{
		{"own dispute awaiting payment", "tpa-1", "tpa-1", "AWAITING_PAYMENT", 0},
		{"another TPA's dispute", "tpa-1", "tpa-2", "AWAITING_PAYMENT", http.StatusNotFound},
		{"not filed by any TPA", "tpa-1", "", "AWAITING_PAYMENT", http.StatusNotFound},
		{"paid and open", "tpa-1", "tpa-1", "OPEN", http.StatusConflict},
		{"pending intake", "tpa-1", "tpa-1", "PENDING_INTAKE", http.StatusConflict},
		{"closed", "tpa-1", "tpa-1", "CLOSED", http.StatusConflict},
		// Someone else's paid dispute must look the same as a missing one.
		{"another TPA's paid dispute", "tpa-1", "tpa-2", "OPEN", http.StatusNotFound},
	}
	for _, c := range cases {
		code, msg := tpaDisputeEditRefusal(c.caller, c.origin, c.status)
		if code != c.wantCode {
			t.Errorf("%s: got %d (%s), want %d", c.name, code, msg, c.wantCode)
		}
		if code == http.StatusConflict && !strings.Contains(msg, c.status) {
			t.Errorf("%s: refusal should name the status: %q", c.name, msg)
		}
	}
}

func TestTPADisputeEditValidate(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	if msg := (&tpaDisputeEdit{}).validate(); msg == "" {
		t.Error("an empty edit should be refused")
	}
	for name, e := range map[string]*tpaDisputeEdit{
		"negative amount":  {DisputedAmountCents: num(-1)},
		"absurd amount":    {DisputedAmountCents: num(maxDisputedAmountCents + 1)},
		"blank client":     {ClientID: str("  ")},
		"oversize contact": {ContactName: str(strings.Repeat("x", 201))},
		"oversize notes":   {Notes: str(strings.Repeat("x", 4001))},
	} {
		if e.validate() == "" {
			t.Errorf("%s should be refused", name)
		}
	}
	e := &tpaDisputeEdit{ContactName: str("  Dana Lee "), Notes: str(" corrected "), DisputedAmountCents: num(0)}
	if msg := e.validate(); msg != "" {
		t.Fatalf("valid edit refused: %s", msg)
	}
	if *e.ContactName != "Dana Lee" || *e.Notes != "corrected" {
		t.Errorf("text fields should be trimmed: %q %q", *e.ContactName, *e.Notes)
	}
}
