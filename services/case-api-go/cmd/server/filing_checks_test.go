package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDisputedAmountProblem(t *testing.T) {
	for _, c := range []struct {
		cents    int64
		required bool
		bad      bool
	}{
		{350000, true, false}, {1, true, false}, {maxDisputedCents, true, false},
		{0, true, true}, {0, false, false}, {-100, true, true}, {-100, false, true},
		{maxDisputedCents + 1, true, true}, {1_000_000_000_000_000, false, true},
	} {
		if got := disputedAmountProblem(c.cents, c.required) != ""; got != c.bad {
			t.Errorf("disputedAmountProblem(%d, %v) refused=%v, want %v", c.cents, c.required, got, c.bad)
		}
	}
	if msg := disputedAmountProblem(1_000_000_000_000_000, true); !strings.Contains(msg, "extra digit") {
		t.Errorf("an absurd amount says why: %q", msg)
	}
}

func TestDecodeProblem(t *testing.T) {
	var in struct {
		Amount int64 `json:"disputed_amount_cents"`
	}
	err := json.Unmarshal([]byte(`{"disputed_amount_cents":"lots"}`), &in)
	if msg := decodeProblem(err); msg != "disputed_amount_cents must be a number" {
		t.Errorf("a text amount names the field: %q", msg)
	}
	if msg := decodeProblem(json.Unmarshal([]byte(`{nope`), &in)); msg != "the request isn't valid JSON" {
		t.Errorf("broken JSON: %q", msg)
	}
}

func TestMalformedRequest(t *testing.T) {
	for raw, bad := range map[string]bool{
		"/v1/tenants/fl/cases?limit=5&status=OPEN":                  false,
		"/v1/tenants/fl/cases?status=%00":                           true,
		"/v1/tenants/fl/cases/..%2F..%2Fetc%2Fpasswd/documents":     true,
		"/v1/tenants/fl/cases/0f44ac4d-d00e-4dd2-b56f-5da464abda69": false,
		"/v1/tenants/fl/search?q=O%27Brien%20..%20report":           false,
		"/api/share/abc/download":                                   false,
	} {
		r := httptest.NewRequest(http.MethodGet, raw, nil)
		if got := malformedRequest(r); got != bad {
			t.Errorf("malformedRequest(%s) = %v, want %v", raw, got, bad)
		}
	}
}
