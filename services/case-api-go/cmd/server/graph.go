// Author: Patrick Munis at newwave
package main

// Graph-intelligence middleware: tenant-scoped proxy from the authenticated
// case-api surface to the graph-intel service (FalkorDB dispute graph, numpy
// GraphSAGE link prediction, EPR-KGQA over the local ollama model, lakehouse
// sync). Auth, tenant extraction, and Permify checks happen here — the Python
// service never faces the portal directly.
//
// Also nudges a graph resync after case lifecycle mutations (initiate /
// signal) so FalkorDB converges on Postgres within seconds, with the
// lakehouse silver zone written by the same sync (db -> graph -> lakehouse).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var graphHTTP = &http.Client{Timeout: 90 * time.Second}

// graphSync is a best-effort, fire-and-forget nudge; failures never block the
// transactional path (Postgres remains the source of truth; the next manual
// /graph/sync reconciles). It brings the one case that changed up to date in
// the graph -- a full rebuild on every change read the whole tenant into
// graph-intel's memory each time, and a run of quick changes ran it out.
func (s *server) graphSync(tenant, caseID string) {
	if s.cfg.GraphIntelURL == "" || caseID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			s.cfg.GraphIntelURL+"/sync/case?tenant="+url.QueryEscape(tenant)+"&case_id="+url.QueryEscape(caseID), nil)
		resp, err := graphHTTP.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
}

func (s *server) proxyGraph(w http.ResponseWriter, r *http.Request, method, path string, body []byte) {
	if s.cfg.GraphIntelURL == "" {
		http.Error(w, `{"error":"graph intelligence not configured (GRAPH_INTEL_URL unset)"}`, http.StatusServiceUnavailable)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	sep := "?"
	if bytes.Contains([]byte(path), []byte("?")) {
		sep = "&"
	}
	u := s.cfg.GraphIntelURL + path + sep + "tenant=" + url.QueryEscape(tenant)
	req, err := http.NewRequestWithContext(r.Context(), method, u, bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":"proxy"}`, http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := graphHTTP.Do(req)
	if err != nil {
		http.Error(w, `{"error":"graph-intel unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// graphAsk handles POST /graph/ask {question, k?} — EPR-KGQA: entity linking,
// path retrieval, GNN ranking, ollama answer with citations. The tenant is
// injected server-side so a caller can never query across the state line.
// graphAskRoles may question the dispute graph: staff who can already open
// the whole case list. A filing party and a TPA cannot -- the graph answers
// from every dispute in the tenant, and they have standing only in their own.
var graphAskRoles = []string{"CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY",
	"ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"}

func mayAskGraph(p principal) bool {
	return !hasRole(p, "TPA") && !hasRole(p, "PARTY") && hasAnyRole(p, graphAskRoles...)
}

// graphFactCategories is which of the program's figures a caller may be
// answered from. The assistant is one more way to read the same data, so it
// follows the screens: a role without the Reports, Ops or money screens does
// not get their contents by asking for them in a sentence.
//
//	caseload, services  anyone who may ask -- the case list shows as much
//	parties, eligibility, outcomes   the Reports roles (program-wide rankings and totals)
//	deadlines           the Reports and Ops roles
//	money               the roles that read invoices and payments
func graphFactCategories(p principal) []string {
	cats := []string{"caseload", "services"}
	reports := hasAnyRole(p, reportsReadRoles...)
	if reports {
		cats = append(cats, "parties", "eligibility", "outcomes")
	}
	if reports || hasAnyRole(p, opsRoles...) {
		cats = append(cats, "deadlines")
	}
	if hasAnyRole(p, financialReadRoles...) {
		cats = append(cats, "money")
	}
	return cats
}

func (s *server) graphAsk(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !mayAskGraph(p) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	var in struct {
		Question string `json:"question"`
		K        int    `json:"k"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Question == "" {
		http.Error(w, `{"error":"question required"}`, http.StatusBadRequest)
		return
	}
	if in.K <= 0 || in.K > 20 {
		in.K = 5
	}
	payload, _ := json.Marshal(map[string]any{"tenant": tenant, "question": in.Question, "k": in.K,
		"allow": graphFactCategories(p)})
	s.logActivity(r.Context(), tenant, "", "KGQA_ASK", truncate("KGQA: "+in.Question, 400))
	s.proxyGraph(w, r, http.MethodPost, "/ask", payload)
}

// graphHints handles GET /graph/hints -- real case numbers and party names
// from this tenant, so the portal's sample questions are about things that
// exist. Same audience as graphAsk.
func (s *server) graphHints(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !mayAskGraph(p) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	s.proxyGraph(w, r, http.MethodGet, "/hints?allow="+url.QueryEscape(strings.Join(graphFactCategories(p), ",")), nil)
}

// graphFeedback handles POST /graph/feedback {log_id, rating}.
func (s *server) graphFeedback(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !mayAskGraph(r.Context().Value(ctxPrincipal{}).(principal)) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	var in struct {
		LogID  string `json:"log_id"`
		Rating int    `json:"rating"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.LogID == "" {
		http.Error(w, `{"error":"log_id required"}`, http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(map[string]any{"tenant": tenant, "log_id": in.LogID, "rating": in.Rating})
	s.proxyGraph(w, r, http.MethodPost, "/feedback", payload)
}

// caseRelated proxies GET /gnn/predict for the case workspace "Related
// disputes" panel. Scores are numpy-GraphSAGE link predictions, written back
// to FalkorDB as RELATED_TO edges by the graph service.
func (s *server) caseRelated(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "caseId")
	s.proxyGraph(w, r, http.MethodGet, "/gnn/predict?case_id="+url.QueryEscape(id)+"&k=5", nil)
}

// caseGraphNeighbors proxies GET /graph/neighbors (evidence neighborhood for
// the case graph panel).
func (s *server) caseGraphNeighbors(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "caseId")
	s.proxyGraph(w, r, http.MethodGet, "/graph/neighbors?case_id="+url.QueryEscape(id)+"&hops=2", nil)
}

// graphSyncNow handles POST /graph/sync — full Postgres -> FalkorDB ->
// lakehouse resync for this tenant (admin action, also runs on a schedule).
// graphAdminRoles gates the three expensive backend jobs below (full graph
// resync, lakehouse export, retraining) -- unlike graphAsk/graphFeedback
// (any case staff querying/correcting the assistant), these cost real
// compute/time and have no per-tenant rate limit, so any authenticated
// member being able to trigger them repeatedly is a real abuse/cost vector.
var graphAdminRoles = []string{"FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN"}

func (s *server) graphSyncNow(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, graphAdminRoles...) {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	s.proxyGraph(w, r, http.MethodPost, "/sync/from-db", nil)
}

// graphSyncStatus handles GET /graph/sync — a full sync runs in the background (minutes on a large tenant): whether
// one is running, and how the last one ended.
func (s *server) graphSyncStatus(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, graphAdminRoles...) {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	s.proxyGraph(w, r, http.MethodGet, "/sync/status", nil)
}

// graphToLakehouse handles POST /graph/to-lakehouse — graph -> gold export.
func (s *server) graphToLakehouse(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, graphAdminRoles...) {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	s.proxyGraph(w, r, http.MethodPost, "/sync/to-lakehouse", nil)
}

// graphTrain handles POST /graph/train {epochs?}.
func (s *server) graphTrain(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, graphAdminRoles...) {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	var in struct {
		Epochs int `json:"epochs"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Epochs <= 0 || in.Epochs > 2000 {
		in.Epochs = 120
	}
	s.proxyGraph(w, r, http.MethodPost, "/gnn/train?epochs="+url.QueryEscape(itoa(in.Epochs)), nil)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
