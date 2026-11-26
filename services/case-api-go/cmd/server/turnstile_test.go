package main

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestTurnstileOnlyForPeople(t *testing.T) {
	t.Setenv("TURNSTILE_SECRET", "1x0000000000000000000000000000000AA")
	person := httptest.NewRequest("POST", "/tpa/intake", nil)
	person = person.WithContext(context.WithValue(person.Context(), ctxPrincipal{}, principal{Subject: "u", Username: "dana.tpa", Roles: []string{"TPA"}}))
	w := httptest.NewRecorder()
	if portalTurnstilePassed(w, person) || w.Code != 400 {
		t.Fatalf("a person filing without the check must be asked for it: %d", w.Code)
	}
	key := httptest.NewRequest("POST", "/tpa/intake", nil)
	key = key.WithContext(context.WithValue(key.Context(), ctxPrincipal{}, principal{Subject: "sa", Username: "service-account-" + apiKeyClientPrefix + "abc", Roles: []string{"TPA"}}))
	if !portalTurnstilePassed(httptest.NewRecorder(), key) {
		t.Fatal("a partner's API key is never asked for the check")
	}
	t.Setenv("TURNSTILE_SECRET", "")
	if !turnstilePassed(httptest.NewRecorder(), httptest.NewRequest("POST", "/x", nil)) {
		t.Fatal("with no secret the check is off")
	}
}
