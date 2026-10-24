// Author: Patrick Munis at newwave
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestEveryActionIsRecordedWithWhoWhatAndOutcome(t *testing.T) {
	var buf bytes.Buffer
	old := actionOut
	actionOut = slog.New(slog.NewJSONHandler(&buf, nil))
	defer func() { actionOut = old }()

	r := chi.NewRouter()
	r.Use(actionLog)
	r.Route("/v1/tenants/{tenant}", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler { // stands in for the auth middleware
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), ctxPrincipal{}, principal{Subject: "u-1", Username: "tpa.user", Roles: []string{"TPA"}})
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		}, noteActor)
		r.Post("/cases/{caseId}/documents", func(w http.ResponseWriter, req *http.Request) {
			logEvent(req.Context(), "audit", "fl", chi.URLParam(req, "caseId"), "DOCUMENT_UPLOADED",
				eventDetail(map[string]any{"filename": "eob.pdf", "requester_email": "a@b.c", "body": "secret text"}))
			w.WriteHeader(http.StatusCreated)
		})
		r.Get("/cases/{caseId}", func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(http.StatusForbidden) })
	})
	for _, c := range []struct{ m, p string }{{"POST", "/v1/tenants/fl/cases/c-9/documents?q=private"}, {"GET", "/v1/tenants/fl/cases/c-8"}} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.m, c.p, nil))
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want an event and two action lines, got %d: %s", len(lines), buf.String())
	}
	var ev, up, denied map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &ev)
	_ = json.Unmarshal([]byte(lines[1]), &up)
	_ = json.Unmarshal([]byte(lines[2]), &denied)
	if ev["kind"] != "event" || ev["event"] != "DOCUMENT_UPLOADED" || ev["user"] != "tpa.user" {
		t.Errorf("event line: %v", ev)
	}
	if d, _ := ev["detail"].(map[string]any); d["filename"] != "eob.pdf" || d["requester_email"] != nil || d["body"] != nil {
		t.Errorf("event detail must keep plain fields and drop contact details and text: %v", d)
	}
	if up["action"] != "POST /cases/{caseId}/documents" || up["user"] != "tpa.user" || up["roles"] != "TPA" ||
		up["case_id"] != "c-9" || up["outcome"] != "ok" || up["tenant"] != "fl" {
		t.Errorf("upload action line: %v", up)
	}
	if strings.Contains(buf.String(), "private") {
		t.Errorf("query strings are never logged")
	}
	if denied["outcome"] != "denied" || denied["status"] != float64(403) {
		t.Errorf("a refusal is recorded as denied: %v", denied)
	}
}

func TestOutcomeAndActionNames(t *testing.T) {
	for code, want := range map[int]string{200: "ok", 201: "ok", 302: "ok", 400: "rejected", 401: "unauthenticated",
		403: "denied", 404: "not_found", 409: "rejected", 429: "throttled", 500: "error", 502: "error"} {
		if got := outcomeOf(code); got != want {
			t.Errorf("outcomeOf(%d) = %q, want %q", code, got, want)
		}
	}
	if got := roleList([]string{"TPA", "default-roles-idre", "offline_access", "uma_authorization"}); got != "TPA" {
		t.Errorf("roleList keeps only idre roles; got %q", got)
	}
	if got := withoutIDs("/v1/tenants/fl/cases/2d756871-3a59-4d8c-a346-f28ab547038c/notes/42"); got != "/v1/tenants/fl/cases/{id}/notes/{id}" {
		t.Errorf("ids are taken out of paths; got %q", got)
	}
	if actionName("GET", "") != "unmatched" || actionName("GET", "/v1/tenants/{tenant}/cases/") != "GET /cases" {
		t.Errorf("action names: %q %q", actionName("GET", ""), actionName("GET", "/v1/tenants/{tenant}/cases/"))
	}
}
