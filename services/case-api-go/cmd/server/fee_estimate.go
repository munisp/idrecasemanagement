// Author: Patrick Munis at newwave
package main

// The full review fee estimate at filing (change log 2026-11-25 #4: "If 'Yes' is selected, populate a cost
// estimate"). Whether an estimate may be shown at filing is the attorney's call, so it ships switched off: an
// administrator turns it on and sets the pricing in the portal (Settings → Fee estimate), with no deploy.
//
// When it is on and a filer answers "Yes" to "Do you want an estimate of the full review fee?", the filing form
// shows the estimate (with the program's note: it is preliminary), and the case keeps it as
// details.fee_estimate_preview_cents. The Admin's Cost Estimate letter stays the official figure: the case
// page offers the preview as the starting value of the estimate, which the Admin can change.
//
// Kept apart from program_rules.config->'fees' (migration-only, guarded by a trigger) because the office edits
// it; every change is kept in fee_estimate_settings_history (scripts/fee_estimate.sql).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

var feeEstimateEditRoles = []string{"TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

type feeEstimateTier struct {
	UpToCents int64 `json:"up_to_cents"` // the tier covers disputed amounts up to this (0 = no upper limit: the last tier)
	FeeCents  int64 `json:"fee_cents"`
}

type feeEstimateConfig struct {
	Enabled       bool              `json:"enabled"`
	Method        string            `json:"method"` // flat | per_claim | tiered
	FlatCents     int64             `json:"flat_cents,omitempty"`
	PerClaimCents int64             `json:"per_claim_cents,omitempty"`
	Tiers         []feeEstimateTier `json:"tiers,omitempty"` // by the dispute's aggregated disputed amount
	MinCents      int64             `json:"min_cents,omitempty"`
	MaxCents      int64             `json:"max_cents,omitempty"`
	Note          string            `json:"note,omitempty"` // shown with the figure, e.g. "preliminary, non-binding"
}

const feeEstimateDefaultNote = "This is a preliminary estimate. If the dispute is eligible, the Cost Estimate letter gives the official figure."

const feeEstimateCapCents = 100_000_000 // $1,000,000: anything above is a typing mistake

// problems: what keeps the settings from being saved (empty = fine). Settings that are off may be incomplete.
func (c *feeEstimateConfig) problems() []string {
	var out []string
	amt := func(name string, v int64) {
		if v < 0 || v > feeEstimateCapCents {
			out = append(out, name+" must be between $0 and $1,000,000")
		}
	}
	amt("The flat fee", c.FlatCents)
	amt("The fee per claim", c.PerClaimCents)
	amt("The minimum", c.MinCents)
	amt("The maximum", c.MaxCents)
	if c.MaxCents > 0 && c.MinCents > c.MaxCents {
		out = append(out, "The minimum is more than the maximum")
	}
	if len(c.Note) > 600 {
		out = append(out, "The note is too long (600 characters at most)")
	}
	if len(c.Tiers) > 20 {
		out = append(out, "At most 20 amount bands")
	}
	for i, t := range c.Tiers {
		amt(fmt.Sprintf("Band %d's fee", i+1), t.FeeCents)
		if t.UpToCents < 0 {
			out = append(out, fmt.Sprintf("Band %d: the upper amount can't be negative", i+1))
		}
		if t.UpToCents == 0 && i != len(c.Tiers)-1 {
			out = append(out, "Only the last band may have no upper amount")
		}
		if i > 0 && t.UpToCents != 0 && t.UpToCents <= c.Tiers[i-1].UpToCents {
			out = append(out, fmt.Sprintf("Band %d must go higher than band %d", i+1, i))
		}
	}
	switch c.Method {
	case "flat", "per_claim", "tiered":
	case "":
		if c.Enabled {
			out = append(out, "Choose how the estimate is worked out")
		}
	default:
		out = append(out, "Unknown method "+c.Method)
	}
	if c.Enabled {
		switch {
		case c.Method == "flat" && c.FlatCents == 0:
			out = append(out, "Enter the flat fee")
		case c.Method == "per_claim" && c.PerClaimCents == 0:
			out = append(out, "Enter the fee per claim")
		case c.Method == "tiered" && len(c.Tiers) == 0:
			out = append(out, "Add at least one amount band")
		}
	}
	return out
}

// estimate: the fee for a dispute with this many claims and this aggregated disputed amount, and whether the
// settings give one (off, or a disputed amount above the last band).
func (c *feeEstimateConfig) estimate(claims int, disputedCents int64) (int64, bool) {
	if c == nil || !c.Enabled {
		return 0, false
	}
	var fee int64
	switch c.Method {
	case "flat":
		fee = c.FlatCents
	case "per_claim":
		if claims < 1 {
			claims = 1
		}
		fee = c.PerClaimCents * int64(claims)
	case "tiered":
		found := false
		for _, t := range c.Tiers {
			if t.UpToCents == 0 || disputedCents <= t.UpToCents {
				fee, found = t.FeeCents, true
				break
			}
		}
		if !found {
			return 0, false
		}
	default:
		return 0, false
	}
	if fee < c.MinCents {
		fee = c.MinCents
	}
	if c.MaxCents > 0 && fee > c.MaxCents {
		fee = c.MaxCents
	}
	return fee, true
}

func (c *feeEstimateConfig) note() string {
	if n := strings.TrimSpace(c.Note); n != "" {
		return n
	}
	return feeEstimateDefaultNote
}

func (s *server) feeEstimateSettings(ctx context.Context, tenant string) (cfg feeEstimateConfig, by string, at any) {
	var raw []byte
	_ = s.db.QueryRow(ctx, `SELECT config, updated_by, updated_at FROM public.fee_estimate_settings WHERE tenant=$1`, tenant).Scan(&raw, &by, &at)
	_ = json.Unmarshal(raw, &cfg)
	return
}

// GET /fee-estimate-settings: the settings, for staff.
func (s *server) getFeeEstimateSettings(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, drReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	cfg, by, at := s.feeEstimateSettings(r.Context(), r.Context().Value(ctxTenant{}).(string))
	writeJSON(w, http.StatusOK, map[string]any{"settings": cfg, "default_note": feeEstimateDefaultNote,
		"updated_by": by, "updated_at": at, "can_edit": hasAnyRole(p, feeEstimateEditRoles...)})
}

// PUT /fee-estimate-settings: replace the settings (administrators only). Every change is kept in the history.
func (s *server) putFeeEstimateSettings(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, feeEstimateEditRoles...) {
		http.Error(w, `{"error":"forbidden: the fee estimate is set by an administrator"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in feeEstimateConfig
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 32<<10))
	if json.Unmarshal(raw, &in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if in.Method != "tiered" {
		in.Tiers = nil
	} else {
		sort.SliceStable(in.Tiers, func(i, j int) bool { // "no upper amount" goes last
			a, b := in.Tiers[i].UpToCents, in.Tiers[j].UpToCents
			return a != 0 && (b == 0 || a < b)
		})
	}
	if pr := in.problems(); len(pr) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "The fee estimate wasn't saved: " + strings.Join(pr, "; "), "problems": pr})
		return
	}
	js, _ := json.Marshal(in)
	who := displayName(p)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"could not save"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	var old []byte
	_ = tx.QueryRow(r.Context(), `SELECT config FROM public.fee_estimate_settings WHERE tenant=$1 FOR UPDATE`, tenant).Scan(&old)
	if _, err := tx.Exec(r.Context(), `INSERT INTO public.fee_estimate_settings (tenant, config, updated_by, updated_at) VALUES ($1,$2,$3,now())
		ON CONFLICT (tenant) DO UPDATE SET config=EXCLUDED.config, updated_by=EXCLUDED.updated_by, updated_at=now()`, tenant, string(js), who); err != nil {
		http.Error(w, `{"error":"could not save"}`, http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO public.fee_estimate_settings_history (tenant, changed_by, changed_sub, old_config, new_config)
		VALUES ($1,$2,$3,$4,$5)`, tenant, who, p.Subject, nullJSON(old), string(js)); err != nil {
		http.Error(w, `{"error":"could not save"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"could not save"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": in})
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// GET /api/public/fee-estimate?tenant=fl: what the filing forms need to show the estimate (nothing when it is off).
func (s *server) publicFeeEstimate(w http.ResponseWriter, r *http.Request) {
	tenant := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tenant")))
	if !tenantCodeRe.MatchString(tenant) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	cfg, _, _ := s.feeEstimateSettings(r.Context(), tenant)
	if !cfg.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	cfg.Note = cfg.note()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, cfg)
}

// feeEstimateOnFiling: when the filer asked for an estimate and the setting is on, the estimate goes into the
// details saved with the case.
func (s *server) feeEstimateOnFiling(ctx context.Context, tenant string, details map[string]any, claims int, disputed *int64) {
	if want, _ := details["fee_estimate_requested"].(bool); !want {
		return
	}
	cfg, _, _ := s.feeEstimateSettings(ctx, tenant)
	var amt int64
	if disputed != nil {
		amt = *disputed
	}
	if fee, ok := cfg.estimate(claims, amt); ok {
		details["fee_estimate_preview_cents"] = fee
		details["fee_estimate_preview_note"] = cfg.note()
	}
}
