package main

import (
	"context"
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
	"win_probability":       true,
	"payer_analytics":       true,
	"fraud_signals":         true,
	"network_adequacy":      true,
	"letter_citation":       true,
	"tpa_onboarding":        true,
	"healthplan_onboarding": true,
	// On: every filing by a TPA or health plan must reference a READY package
	// (its documents accepted) and a verified payment method. Off (the default,
	// featureDefaultOff): onboarding is optional and any active client can be
	// filed for; a package named on a filing must still be READY.
	"onboarding_required": true,
	// Cross-rail organization aggregate (TPA 360 / provider 360 / health
	// plan 360): entities registry + resolver + six-panel endpoint.
	"entity360": true,
	// Case workflow rules (workflowsteps.go, caselock.go). A tenant admin turns these off when their office works a
	// case out of the SOP's order, or lets anyone on staff take any step.
	"step_order":      true, // only the case's next step can be acted on
	"step_owner":      true, // only the role that owns a step (or an admin) can act on it
	"payment_lock":    true, // a case awaiting its initial fee allows only correspondence
	"ineligible_lock": true, // a case found ineligible allows only correspondence
	"fwa_review":      true, // fraud, waste or abuse review on the case page, and its automatic flag on large disputes
}

// tenantAdminRoles may change their own tenant's toggles; the platform and federal admins any tenant's.
var tenantAdminRoles = []string{"TENANT_ADMIN", "PLATFORM_ADMIN", "FEDERAL_ADMIN"}

// featureDefaultOff are the flags a tenant has to turn on; every other flag is on until turned off.
var featureDefaultOff = map[string]bool{"onboarding_required": true}

// featureEnabled reads the tenant's flags fresh (no cache — an admin toggle
// applies to the very next request). Absent flag or absent row = on, except featureDefaultOff.
// featureFlags is a tenant's stored flags: any left in its program settings
// (config->'features', where they were first kept) overlaid by
// public.tenant_features, where they are written now. Absent = not stored.
func (s *server) featureFlags(r *http.Request, tenant string) map[string]bool {
	return s.featureFlagsCtx(r.Context(), tenant)
}

func (s *server) featureFlagsCtx(ctx context.Context, tenant string) map[string]bool {
	var legacy, own []byte
	_ = s.db.QueryRow(ctx,
		`SELECT coalesce(config->'features','{}'::jsonb) FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&legacy)
	_ = s.db.QueryRow(ctx,
		`SELECT features FROM public.tenant_features WHERE tenant=$1`, tenant).Scan(&own)
	return mergeFlags(legacy, own)
}

// mergeFlags: the second set wins; unparseable input is ignored.
func mergeFlags(sets ...[]byte) map[string]bool {
	out := map[string]bool{}
	for _, raw := range sets {
		var m map[string]bool
		if len(raw) > 0 && json.Unmarshal(raw, &m) == nil {
			for k, v := range m {
				out[k] = v
			}
		}
	}
	return out
}

func (s *server) featureEnabled(r *http.Request, tenant, name string) bool {
	return s.featureEnabledCtx(r.Context(), tenant, name)
}

// featureEnabledCtx is featureEnabled for code that has no request (the case lock, background passes).
func (s *server) featureEnabledCtx(ctx context.Context, tenant, name string) bool {
	if v, ok := s.featureFlagsCtx(ctx, tenant)[name]; ok {
		return v
	}
	return !featureDefaultOff[name]
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
		out[k] = !featureDefaultOff[k]
	}
	for k, v := range s.featureFlags(r, tenant) {
		if registeredFeatures[k] {
			out[k] = v
		}
	}
	return out
}

// setTenantFeatures: PUT /v1/admin/tenants/{tenant}/features -- the platform and federal admins, any tenant.
func (s *server) setTenantFeatures(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "PLATFORM_ADMIN", "FEDERAL_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	s.writeTenantFeatures(w, r, chi.URLParam(r, "tenant"), p)
}

// setOwnTenantFeatures: PUT /v1/tenants/{t}/features -- a tenant admin, for the tenant they are signed in to
// (the tenancy middleware has already checked they belong to it).
func (s *server) setOwnTenantFeatures(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, tenantAdminRoles...) {
		http.Error(w, `{"error":"forbidden: requires TENANT_ADMIN, PLATFORM_ADMIN or FEDERAL_ADMIN"}`, http.StatusForbidden)
		return
	}
	s.writeTenantFeatures(w, r, r.Context().Value(ctxTenant{}).(string), p)
}

// writeTenantFeatures validates keys against the registered vocabulary and stores the flags named.
func (s *server) writeTenantFeatures(w http.ResponseWriter, r *http.Request, tenant string, p principal) {
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
	// Stored in public.tenant_features, never program_rules: creating a
	// program_rules row would switch a federal tenant into custom-program
	// mode (see scripts/features.sql). Flags not named keep their value.
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.tenant_features (tenant, features, updated_by)
		VALUES ($1, $2::jsonb, $3)
		ON CONFLICT (tenant) DO UPDATE SET
		  features = public.tenant_features.features || EXCLUDED.features,
		  updated_by = EXCLUDED.updated_by, updated_at = now()`, tenant, string(raw), displayName(p)); err != nil {
		http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, "", "FEATURES_UPDATED", fmt.Sprintf("feature flags updated by %s", p.Subject))
	s.logAudit(r.Context(), tenant, "", "FEATURES_UPDATED", map[string]any{"features": in.Features, "actor": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "features": in.Features})
}
