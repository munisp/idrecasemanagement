package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimitResponse(t *testing.T) {
	reset := time.Now().Add(30 * time.Second)
	w := httptest.NewRecorder()
	if !limitResponse(w, 5, 5, reset, "an API key") {
		t.Fatal("the 5th request of 5 must pass")
	}
	if w.Header().Get("X-RateLimit-Remaining") != "0" || w.Header().Get("Retry-After") != "" {
		t.Fatalf("headers at the limit: %v", w.Header())
	}
	w = httptest.NewRecorder()
	if limitResponse(w, 5, 6, reset, "an API key") {
		t.Fatal("the 6th request of 5 must be refused")
	}
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || w.Header().Get("X-RateLimit-Limit") != "5" {
		t.Fatalf("refusal: %d %v", w.Code, w.Header())
	}
}

func TestLoadAPILimits(t *testing.T) {
	t.Setenv("API_RATE_LIMIT_KEY_PER_MIN", "0")
	t.Setenv("API_RATE_LIMIT_USER_PER_MIN", "bad")
	t.Setenv("API_RATE_LIMIT_EXEMPT", " API-Key-LoadTest , ")
	l := loadAPILimits()
	if l.key != 0 || l.user != 600 || l.public != 120 || !l.exempt["api-key-loadtest"] || len(l.exempt) != 1 {
		t.Fatalf("%+v", l)
	}
}
