package main

// casescope.go — which cases a Reviewer may see.
//
// A Reviewer (CASE_MANAGER) works the cases assigned to them, and only those: cases are given out by the rotation
// (autoassign.go) or by the PM, so there is no unassigned pool for a Reviewer to browse or take from. Any other case
// — someone else's, or nobody's yet — does not exist for them: the case list, search, the calendar, the per-case
// routes, everything answers as if it were not there (404, never 403: a reviewer must not learn which case ids
// exist).
//
// The PM, Tenant Admin and the platform's admins see every case of the tenant, and so do the specialist roles
// (coder, clinician, attorney, arbitrator, finance, auditor), whose own queues are built across every case. The
// rule is on the role a person holds, not on the menu: a Reviewer who also holds one of those roles sees every case.
//
// One place decides (reviewerScoped / caseVisible / caseScopeSQL); accessGuard applies it to every route that names a
// case in its path (accesspolicy.go), and each list that a Reviewer can reach adds caseScopeSQL to its query.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// seeAllCaseRoles: holding any of these, a person sees every case of the tenant.
var seeAllCaseRoles = []string{
	"PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole,
	"CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "ARBITRATOR", "FINANCE",
}

// reviewerScoped: the caller is a Reviewer and nothing that sees every case. Exact role names on purpose —
// hasAnyRole counts a Tenant Admin as every staff role, which would make every Tenant Admin a scoped Reviewer.
func reviewerScoped(p principal) bool {
	if !contains(p.Roles, "CASE_MANAGER") {
		return false
	}
	for _, r := range seeAllCaseRoles {
		if contains(p.Roles, r) {
			return false
		}
	}
	return true
}

// caseVisibleTo is the rule itself, for a case whose assignee is known: assigned to the caller. Pure, so it is
// tested.
func caseVisibleTo(p principal, assignedTo string) bool {
	if !reviewerScoped(p) {
		return true
	}
	return assignedTo != "" && assignedTo == p.Subject
}

// caseVisible looks the case up. A case that does not exist is "visible": the handler answers its own 404.
func (s *server) caseVisible(ctx context.Context, p principal, tenant, caseID string) bool {
	if !reviewerScoped(p) {
		return true
	}
	var to *string
	if err := s.db.QueryRow(ctx, fmt.Sprintf(`SELECT assigned_to FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)),
		caseID).Scan(&to); err != nil {
		return true
	}
	return caseVisibleTo(p, deref(to))
}

// caseScopeSQL is the same rule as an SQL condition on a cases row (alias, e.g. "c"), for lists: "" when the caller
// sees every case. argN is the placeholder number the caller's subject takes.
func caseScopeSQL(p principal, alias string, argN int) (string, bool) {
	if !reviewerScoped(p) {
		return "", false
	}
	return fmt.Sprintf(" AND %[1]s.assigned_to = $%[2]d", alias, argN), true
}

// caseIDScopeSQL: the rule for rows of another table that point at a case (col holds the case id, as text or uuid):
// rows that point at no case stay; rows that point at a case stay when it is the caller's.
func caseIDScopeSQL(p principal, tenant, col string, argN int) (string, bool) {
	if !reviewerScoped(p) {
		return "", false
	}
	return fmt.Sprintf(` AND (%[1]s IS NULL OR EXISTS (SELECT 1 FROM tenant_%[2]s.cases sc WHERE sc.id::text = %[1]s::text
		AND sc.assigned_to = $%[3]d))`,
		col, sanitizeTenant(tenant), argN), true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// hiddenCaseIDs: of the given case ids, the ones the caller may not see (nil when they see every case). For lists
// that come back with a case id per row and are filtered after the query (search, the calendar, the journals).
func (s *server) hiddenCaseIDs(ctx context.Context, p principal, tenant string, ids []string) map[string]bool {
	if !reviewerScoped(p) || len(ids) == 0 {
		return nil
	}
	out := map[string]bool{}
	rows, err := s.db.Query(ctx, fmt.Sprintf(`SELECT id::text FROM tenant_%s.cases
		WHERE id::text = ANY($1) AND assigned_to IS DISTINCT FROM $2`,
		sanitizeTenant(tenant)), ids, p.Subject)
	if err != nil {
		// Fail closed: if the check cannot run, every named case is hidden from a Reviewer.
		for _, id := range ids {
			out[id] = true
		}
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// scopedResponse buffers a read's JSON answer for a Reviewer and removes from it every object that names a case they
// may not see (by its "case_id"), wherever it sits. Lists across cases (the calendar, deadline clocks, journals,
// tasks, intake, the QA queue, account and entity pages) are then right for a Reviewer without each having its own
// filter, and a list added later is covered too. Non-JSON answers (downloads) pass through untouched.
type scopedResponse struct {
	http.ResponseWriter
	status      int
	buf         bytes.Buffer
	decided     bool // whether this answer is JSON (buffered and filtered) was settled on the first header or write
	passthrough bool // not JSON: written straight through, as it comes (zips and other downloads stream)
}

func (w *scopedResponse) decide() {
	if w.decided {
		return
	}
	w.decided = true
	if !strings.Contains(w.Header().Get("Content-Type"), "json") {
		w.passthrough = true
		w.ResponseWriter.WriteHeader(w.status)
	}
}

func (w *scopedResponse) WriteHeader(code int) {
	if w.decided {
		return
	}
	w.status = code
	w.decide()
}

func (w *scopedResponse) Write(b []byte) (int, error) {
	w.decide()
	if w.passthrough {
		return w.ResponseWriter.Write(b)
	}
	return w.buf.Write(b)
}

func (w *scopedResponse) Flush() {
	if w.passthrough {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (s *server) serveScoped(next http.Handler, w http.ResponseWriter, r *http.Request, p principal, tenant string) {
	rec := &scopedResponse{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(rec, r)
	if rec.passthrough {
		return
	}
	body := rec.buf.Bytes()
	if !strings.Contains(w.Header().Get("Content-Type"), "json") || !bytes.Contains(body, []byte(`"case_id"`)) {
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil {
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
		return
	}
	var ids []string
	collectCaseIDs(doc, &ids)
	hidden := s.hiddenCaseIDs(r.Context(), p, tenant, ids)
	if len(hidden) == 0 {
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
		return
	}
	out, err := json.Marshal(pruneHiddenCases(doc, hidden))
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(rec.status)
	_, _ = w.Write(out)
}

func collectCaseIDs(v any, ids *[]string) {
	switch t := v.(type) {
	case map[string]any:
		if id, ok := t["case_id"].(string); ok && id != "" {
			*ids = append(*ids, id)
		}
		for _, x := range t {
			collectCaseIDs(x, ids)
		}
	case []any:
		for _, x := range t {
			collectCaseIDs(x, ids)
		}
	}
}

// pruneHiddenCases drops every object whose case_id is hidden from the arrays that hold it. Pure, so it is tested.
func pruneHiddenCases(v any, hidden map[string]bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = pruneHiddenCases(x, hidden)
		}
		return t
	case []any:
		kept := make([]any, 0, len(t))
		for _, x := range t {
			if m, ok := x.(map[string]any); ok {
				if id, ok := m["case_id"].(string); ok && hidden[id] {
					continue
				}
			}
			kept = append(kept, pruneHiddenCases(x, hidden))
		}
		return kept
	}
	return v
}
