package main

// casescope.go — which cases a person may see (least privilege, change log #14).
//
// An Admin (CASE_MANAGER) works the cases assigned to them, and only those: cases are given out by the rotation
// (autoassign.go) or by the PM, so there is no unassigned pool for an Admin to browse or take from. Any other case
// — someone else's, or nobody's yet — does not exist for them: the case list, search, the calendar, the per-case
// routes, everything answers as if it were not there (404, never 403: a reviewer must not learn which case ids
// exist).
//
// The PM, Tenant Admin and the platform's admins see every case of the tenant, and so do the specialist roles
// (coder, clinician, attorney, arbitrator, finance, auditor), whose own queues are built across every case. The
// rule is on the role a person holds, not on the menu: an Admin who also holds one of those roles sees every case.
//
// One place decides (reviewerScoped / caseVisible / caseScopeSQL); accessGuard applies it to every route that names a
// case in its path (accesspolicy.go), and each list that an Admin can reach adds caseScopeSQL to its query.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Least privilege (change log #14, 2026-11-26): the PM and the Admin see every case (the Admin does the eligibility
// review of every incoming dispute), as do the Tenant Admin, Finance (invoices across cases), the state auditor and
// the platform's admins. The specialists -- Coder, Nurse Reviewer, Attorney, QA (and an arbitrator) -- see only the
// cases they work: assigned to them, named on the case team (case_helpers), or named as a further reviewer
// (case_review_team). A QA also sees a case with no QA named yet, since every QA hears of its drafts until one is.

// seeAllCaseRoles: holding any of these, a person sees every case of the tenant.
var seeAllCaseRoles = []string{
	"CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole, "FINANCE",
}

// specialistRoles: roles that see only the cases they are named on.
var specialistRoles = []string{"CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "QA", "ARBITRATOR"}

// reviewerScoped: the caller sees only the cases they work -- they hold a specialist role and nothing that sees every
// case. Exact role names on purpose: hasAnyRole counts a Tenant Admin as every staff role.
func reviewerScoped(p principal) bool {
	scoped := false
	for _, r := range specialistRoles {
		if contains(p.Roles, r) {
			scoped = true
		}
	}
	if !scoped {
		return false
	}
	for _, r := range seeAllCaseRoles {
		if contains(p.Roles, r) {
			return false
		}
	}
	return true
}

// caseVisibleTo is the rule for a case whose people are known: assigned to the caller, or the caller is one of
// named (helpers and further reviewers), or the caller is a QA and the case has no QA named. Pure, so it is tested.
func caseVisibleTo(p principal, assignedTo string, named []string, qaNamed bool) bool {
	if !reviewerScoped(p) {
		return true
	}
	if assignedTo != "" && assignedTo == p.Subject {
		return true
	}
	if contains(named, p.Subject) {
		return true
	}
	return contains(p.Roles, "QA") && !qaNamed
}

// scopeSQL: the same rule as an SQL condition over a case id expression (idExpr, as text or uuid) and its assignee
// (assignedExpr, "" when the rows have none at hand); argN is the placeholder number the caller's subject takes.
func scopeSQL(p principal, tenant, idExpr, assignedExpr string, argN int) string {
	t := sanitizeTenant(tenant)
	cond := fmt.Sprintf(`EXISTS (SELECT 1 FROM public.case_helpers h WHERE h.tenant='%[1]s' AND h.case_id::text=(%[2]s)::text AND h.user_id=$%[3]d)
		OR EXISTS (SELECT 1 FROM public.case_review_team rt WHERE rt.tenant='%[1]s' AND rt.case_id=(%[2]s)::text AND rt.user_id=$%[3]d)`,
		t, idExpr, argN)
	if assignedExpr != "" {
		cond = fmt.Sprintf(`%s = $%d OR `, assignedExpr, argN) + cond
	} else {
		cond = fmt.Sprintf(`EXISTS (SELECT 1 FROM tenant_%s.cases ac WHERE ac.id::text=(%s)::text AND ac.assigned_to=$%d) OR `, t, idExpr, argN) + cond
	}
	if contains(p.Roles, "QA") {
		cond += fmt.Sprintf(` OR NOT EXISTS (SELECT 1 FROM public.case_helpers hq WHERE hq.tenant='%s' AND hq.case_id::text=(%s)::text AND hq.role='QA')`,
			t, idExpr)
	}
	return "(" + cond + ")"
}

// caseVisible looks the case up. A case that does not exist is "visible": the handler answers its own 404.
func (s *server) caseVisible(ctx context.Context, p principal, tenant, caseID string) bool {
	if !reviewerScoped(p) {
		return true
	}
	var ok bool
	if err := s.db.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM tenant_%s.cases c WHERE c.id::text=$2 AND %s)`,
		sanitizeTenant(tenant), scopeSQL(p, tenant, "c.id", "c.assigned_to", 1)), p.Subject, caseID).Scan(&ok); err != nil {
		// Fail closed, like hiddenCaseIDs: if the rule cannot be checked, a scoped specialist does not see the case.
		return false
	}
	return ok
}

// caseScopeSQL is the same rule as an SQL condition on a cases row (alias, e.g. "c"), for lists: "" when the caller
// sees every case. argN is the placeholder number the caller's subject takes.
func caseScopeSQL(p principal, tenant, alias string, argN int) (string, bool) {
	if !reviewerScoped(p) {
		return "", false
	}
	return " AND " + scopeSQL(p, tenant, alias+".id", alias+".assigned_to", argN), true
}

// caseIDScopeSQL: the rule for rows of another table that point at a case (col holds the case id, as text or uuid):
// rows that point at no case stay; rows that point at a case stay when the caller works it.
func caseIDScopeSQL(p principal, tenant, col string, argN int) (string, bool) {
	if !reviewerScoped(p) {
		return "", false
	}
	return fmt.Sprintf(` AND (%s IS NULL OR %s)`, col, scopeSQL(p, tenant, col, "", argN)), true
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
	rows, err := s.db.Query(ctx, fmt.Sprintf(`SELECT c.id::text FROM tenant_%s.cases c
		WHERE c.id::text = ANY($1) AND NOT %s`,
		sanitizeTenant(tenant), scopeSQL(p, tenant, "c.id", "c.assigned_to", 2)), ids, p.Subject)
	if err != nil {
		// Fail closed: if the check cannot run, every named case is hidden from an Admin.
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

// scopedResponse buffers a read's JSON answer for an Admin and removes from it every object that names a case they
// may not see (by its "case_id"), wherever it sits. Lists across cases (the calendar, deadline clocks, journals,
// tasks, intake, the QA queue, account and entity pages) are then right for an Admin without each having its own
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
