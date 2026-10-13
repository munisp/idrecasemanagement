package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// features.go — per-tenant feature toggles for the original (federal NSA +
// FL AHCA) platform, mirroring NG's manifest Features semantics: flags live
// in program_rules.config->'features', an absent key means DEFAULT ON (so
// existing deployments change nothing), and server-side gates — not just the
// nav — enforce every toggle. A UI-only toggle is decoration, not a control.

// registeredFeatures is the closed vocabulary of toggleable capabilities.
// Unknown keys are rejected at write time so a typo can never silently
// create a flag that gates nothing.
var registeredFeatures = map[string]bool{
	"sealed_offers":         true,
	"review_queues":         true,
	"party_workspace":       true,
	"benchmarks":            true,
	"batch_suggestions":     true,
	"bundling_filter":       true,
	"necessity_screen":      true,
	"deadline_verification": true,
	"duplicate_detection":   true,
}

// featureEnabled reads the tenant's flags fresh (no cache — an admin toggle
// applies to the very next request). Absent flag or absent row = true.
func (s *server) featureEnabled(r *http.Request, tenant, name string) bool {
	var raw []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT coalesce(config->'features','{}'::jsonb) FROM public.program_rules WHERE tenant=$1`,
		tenant).Scan(&raw)
	if err != nil {
		return true
	}
	var flags map[string]bool
	if json.Unmarshal(raw, &flags) != nil {
		return true
	}
	if v, ok := flags[name]; ok {
		return v
	}
	return true
}

// featureGate rejects the request with 404 (not 403 — a disabled feature
// should be indistinguishable from one that doesn't exist) when the flag is
// off. Returns true when the caller may proceed.
func (s *server) featureGate(w http.ResponseWriter, r *http.Request, tenant, name string) bool {
	if registeredFeatures[name] && s.featureEnabled(r, tenant, name) {
		return true
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	return false
}

// tenantFeatures returns the effective flag set (defaults merged) for the
// portal's config payload.
func (s *server) tenantFeatures(r *http.Request, tenant string) map[string]bool {
	out := map[string]bool{}
	for k := range registeredFeatures {
		out[k] = true
	}
	var raw []byte
	if err := s.db.QueryRow(r.Context(),
		`SELECT coalesce(config->'features','{}'::jsonb) FROM public.program_rules WHERE tenant=$1`,
		tenant).Scan(&raw); err == nil {
		var flags map[string]bool
		if json.Unmarshal(raw, &flags) == nil {
			for k, v := range flags {
				if registeredFeatures[k] {
					out[k] = v
				}
			}
		}
	}
	return out
}

// setTenantFeatures: PLATFORM_ADMIN / FEDERAL_ADMIN toggle surface. Validates
// keys against the registered vocabulary and upserts config->'features'.
func (s *server) setTenantFeatures(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "PLATFORM_ADMIN", "FEDERAL_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := chi.URLParam(r, "tenant")
	var in struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	for k := range in.Features {
		if !registeredFeatures[k] {
			http.Error(w, fmt.Sprintf(`{"error":"unknown feature %q (registered vocabulary is closed)"}`, k), http.StatusBadRequest)
			return
		}
	}
	raw, _ := json.Marshal(in.Features)
	// Upsert: a federal-NSA tenant may have no program_rules row yet; the
	// program column is NOT NULL so seed it explicitly on insert.
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.program_rules (tenant, program, config)
		VALUES ($1, 'FEDERAL_NSA', jsonb_build_object('features', $2::jsonb))
		ON CONFLICT (tenant) DO UPDATE SET
		  config = jsonb_set(coalesce(public.program_rules.config,'{}'::jsonb), '{features}', $2::jsonb, true),
		  updated_at = now()`, tenant, string(raw)); err != nil {
		http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, "", "FEATURES_UPDATED", fmt.Sprintf("feature flags updated by %s", p.Subject))
	s.logAudit(r.Context(), tenant, "", "FEATURES_UPDATED", map[string]any{"features": in.Features, "actor": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "features": in.Features})
}
