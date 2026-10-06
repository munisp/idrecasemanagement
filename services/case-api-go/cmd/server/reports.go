// reports.go — tenant-scoped console feeds: voice console + compliance reports.
package main

import (
	"fmt"
	"net/http"
	"time"
)

func (s *server) listVoiceIntake(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT id, caller_phone, caller_name, organization, summary, status, created_at
		FROM public.voice_intake_requests WHERE tenant=$1
		ORDER BY created_at DESC LIMIT 100`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, phone, name, org, summary, status string
		var at time.Time
		if rows.Scan(&id, &phone, &name, &org, &summary, &status, &at) == nil {
			out = append(out, map[string]any{
				"id": id, "caller_phone": phone, "caller_name": name,
				"organization": org, "summary": summary, "status": status, "created_at": at,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) listVoiceLogs(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT id, direction, COALESCE(tool,''), COALESCE(caller_phone,''),
		       COALESCE(case_number,''), status, created_at
		FROM public.voice_call_logs WHERE tenant=$1
		ORDER BY created_at DESC LIMIT 100`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, dir, tool, phone, cn, status string
		var at time.Time
		if rows.Scan(&id, &dir, &tool, &phone, &cn, &status, &at) == nil {
			out = append(out, map[string]any{
				"id": id, "direction": dir, "tool": tool, "caller_phone": phone,
				"case_number": cn, "status": status, "created_at": at,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// slaReport: statutory breach feed for auditors/federal admins.
func (s *server) slaReport(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT case_id, clock, COALESCE(detail,''), created_at
		FROM public.sla_breaches WHERE tenant=$1 ORDER BY created_at DESC LIMIT 200`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var cid, clock, detail string
		var at time.Time
		if rows.Scan(&cid, &clock, &detail, &at) == nil {
			out = append(out, map[string]any{
				"case_id": cid, "clock": clock, "detail": detail, "at": at,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// summaryReport: case-status aggregates (CMS-report style rollups).
func (s *server) summaryReport(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// Programmed tenants (FL AHCA) track progress on internal_status, not
	// the federal status column -- grouping by status there would aggregate
	// every case under one empty/unused bucket. Cases that haven't reached
	// their first internal_status yet (a just-created PENDING_INTAKE shell
	// case) fall back to status so they aren't dropped from the rollup.
	// Programmed tenants have no QPA concept at all (that's a federal NSA
	// term) -- qpa_cents is always 0/null there, so the average silently
	// showed "avg QPA $0" on every status. disputed_amount_cents is FL's
	// actual equivalent (accumulates from imported claims). Confirmed live.
	programmed := s.loadProgram(r, tenant) != nil
	statusExpr := "status"
	amountExpr := "qpa_cents"
	if programmed {
		statusExpr = "coalesce(internal_status, status)"
		amountExpr = "disputed_amount_cents"
	}
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT %s, COUNT(*), COALESCE(AVG(%s),0)
		FROM tenant_%s.cases GROUP BY 1 ORDER BY 2 DESC`, statusExpr, amountExpr, sanitizeTenant(tenant)))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var status string
		var n int
		var avg float64
		if rows.Scan(&status, &n, &avg) == nil {
			out = append(out, map[string]any{
				"status": status, "count": n, "avg_amount_usd": avg / 100,
				"amount_label": map[bool]string{true: "Disputed", false: "QPA"}[programmed],
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
