package main

import (
	"fmt"
	"net/http"
)

// pipelineCounts: GET /cases/pipeline/counts?by=internal_status|status
// The Pipeline board's columns: every stage with its true number of disputes and their totals. The cards themselves
// are paged per column through /cases (internal_status= or status=, unset= for "Not yet reviewed"), so the board
// no longer stops at the first 200 cases.
func (s *server) pipelineCounts(w http.ResponseWriter, r *http.Request) {
	if hasRole(r.Context().Value(ctxPrincipal{}).(principal), "TPA") {
		http.Error(w, `{"error":"forbidden: TPA accounts see their own disputes on the TPA page"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	by := "internal_status"
	if r.URL.Query().Get("by") == "status" {
		by = "status"
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT coalesce(%[1]s, '') AS stage, count(*) AS count,
		       coalesce(sum(disputed_amount_cents), 0) AS disputed_cents, coalesce(sum(qpa_cents), 0) AS qpa_cents
		FROM tenant_%[2]s.cases GROUP BY 1 ORDER BY 1`, by, sanitizeTenant(tenant)))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	total := int64(0)
	for _, row := range rows {
		if n, ok := row["count"].(int64); ok {
			total += n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"by": by, "stages": rows, "total": total})
}
