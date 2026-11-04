package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

var routeLine = regexp.MustCompile(`\br\.(Get|Post|Put|Patch|Delete)\("([^"]+)"`)

// registeredRoutes reads "METHOD /path" for every route registered in this package's source.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	out := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeLine.FindAllStringSubmatch(string(b), -1) {
			out[strings.ToUpper(m[1])+" "+m[2]] = true
		}
	}
	return out
}

func TestCuratedAPIDocsNameRealRoutes(t *testing.T) {
	routes := registeredRoutes(t)
	for key := range apiCurated {
		if !routes[key] {
			t.Errorf("apidocs_curated.go documents %q, but no such route is registered", key)
		}
	}
}

func testRouter() chi.Router {
	ok := func(w http.ResponseWriter, _ *http.Request) {}
	r := chi.NewRouter()
	r.Get("/healthz", ok)
	r.Get("/api/public/docs/{audience}", ok)
	r.Route("/v1/tenants/{tenant}", func(r chi.Router) {
		r.Get("/tpa/me", ok)
		r.Post("/tpa/intake", ok)
		r.Post("/tpa/claim", ok)
		r.Get("/hp/me", ok)
		r.Get("/cases/{caseId}", ok)
		r.Get("/cases/{caseId}/timeline", ok)
		r.Get("/qa/{id}", ok)
		r.Get("/api-keys", ok)
	})
	return r
}

func specPaths(t *testing.T, s *server, aud apiAudience) map[string]map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/public/docs/x", nil)
	req.Host = "idre.example"
	spec := s.buildAPISpec(req, aud)
	return spec["paths"].(map[string]map[string]any)
}

func TestPartnerSpecsHoldOnlyTheirArea(t *testing.T) {
	s := &server{apiRoutes: collectAPIRoutes(testRouter())}
	tpa := specPaths(t, s, audTPA)
	for p := range tpa {
		if !strings.Contains(p, "/tpa/") && p != "/v1/tenants/{tenant}/cases/{caseId}" {
			t.Errorf("TPA reference lists %s", p)
		}
	}
	if tpa["/v1/tenants/{tenant}/tpa/me"] == nil || tpa["/v1/tenants/{tenant}/tpa/intake"]["post"] == nil {
		t.Errorf("TPA reference is missing its own endpoints: %v", tpa)
	}
	if tpa["/v1/tenants/{tenant}/tpa/claim"] != nil {
		t.Error("claim (a portal step) should not be in the partner reference")
	}
	for _, p := range []string{"/v1/tenants/{tenant}/cases/{caseId}/timeline", "/v1/tenants/{tenant}/qa/{id}", "/v1/tenants/{tenant}/api-keys", "/v1/tenants/{tenant}/hp/me"} {
		if tpa[p] != nil {
			t.Errorf("TPA reference leaks %s", p)
		}
	}
	plan := specPaths(t, s, audPlan)
	if plan["/v1/tenants/{tenant}/hp/me"] == nil || plan["/v1/tenants/{tenant}/tpa/me"] != nil {
		t.Errorf("health plan reference has the wrong area: %v", plan)
	}
	internal := specPaths(t, s, audInternal)
	if internal["/v1/tenants/{tenant}/qa/{id}"] == nil || internal["/healthz"] != nil {
		t.Errorf("internal reference should list every route but /healthz: %v", internal)
	}
}

func TestHandlerNamesResolve(t *testing.T) {
	s := &server{}
	r := chi.NewRouter()
	r.Get("/a", s.tpaMe)
	r.Get("/b", s.orgAPIKeysList("tpa"))
	got := map[string]string{}
	for _, rt := range collectAPIRoutes(r) {
		got[rt.Pattern] = rt.Handler
	}
	if got["/a"] != "tpaMe" || got["/b"] != "orgAPIKeysList" {
		t.Errorf("handler names: %v", got)
	}
}
