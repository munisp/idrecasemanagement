package main

import (
	"fmt"
	"net/http"
	"strings"
)

// pipelineCounts: GET /cases/pipeline/counts?by=internal_status|status[&assigned=mine|unassigned][&q=…]
// The Pipeline board's columns: every stage with its true number of disputes and their totals. The cards themselves
// are paged per column through /cases (internal_status= or status=, unset= for "Not yet reviewed"), so the board
// no longer stops at the first 200 cases.
func (s *server) pipelineCounts(w http.ResponseWriter, r *http.Request) {
	if hasRole(r.Context().Value(ctxPrincipal{}).(principal), "TPA") {
		http.Error(w, `{"error":"forbidden: TPA accounts see their own disputes on the TPA page"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	q := r.URL.Query()
	by := "internal_status"
	if q.Get("by") == "status" {
		by = "status"
	}
	// The same filters as the cards' /cases list, so a column's count is the number of cards it pages through:
	// the Reviewer's scope (only unassigned cases and their own), assigned=mine|unassigned, and the ?q= search.
	caller := r.Context().Value(ctxPrincipal{}).(principal)
	args := []any{tenant}
	where := " AND $1::text IS NOT NULL"
	if cond, ok := caseScopeSQL(caller, "c", len(args)+1); ok {
		args = append(args, caller.Subject)
		where += cond
	}
	switch q.Get("assigned") {
	case "mine":
		args = append(args, caller.Subject)
		where += fmt.Sprintf(" AND c.assigned_to = $%d", len(args))
	case "unassigned":
		where += " AND (c.assigned_to IS NULL OR c.assigned_to = 'unassigned-pool')"
	}
	if qs := strings.TrimSpace(q.Get("q")); qs != "" {
		if len(qs) > 100 {
			qs = qs[:100]
		}
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(qs)+"%")
		where += caseSearchSQL(len(args))
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT coalesce(c.%[1]s, '') AS stage, count(*) AS count,
		       coalesce(sum(c.disputed_amount_cents), 0) AS disputed_cents, coalesce(sum(c.qpa_cents), 0) AS qpa_cents,
		       count(*) FILTER (WHERE c.assigned_to IS NULL OR c.assigned_to IN ('', 'unassigned-pool')) AS unassigned,
		       -- unassigned AND paid: the ones actually waiting for a Reviewer (an unpaid filing is not given out yet)
		       count(*) FILTER (WHERE (c.assigned_to IS NULL OR c.assigned_to IN ('', 'unassigned-pool')) AND c.status <> 'AWAITING_PAYMENT') AS unassigned_paid,
		       count(*) FILTER (WHERE c.status = 'AWAITING_PAYMENT') AS awaiting_payment
		FROM tenant_%[2]s.cases c WHERE true%[3]s GROUP BY 1 ORDER BY 1`, by, sanitizeTenant(tenant), where), args...)
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
