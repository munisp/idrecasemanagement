package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// Case assignment. The PM assigns and reassigns cases to real staff of the tenant (members of its Keycloak group
// holding the role, enabled), picked from a list that shows each person's open caseload. Auto-assignment balances
// over everyone eligible, including people with no cases yet. A reassignment tells the person who had the case.

// assignableRoles: who a case can be assigned to, as which role.
var assignableRoles = map[string]string{
	"CASE_MANAGER": "Reviewer", "ARBITRATOR": "Arbitrator", "CODER": "Coder",
	"NURSE_PHYSICIAN": "Clinical reviewer", "ATTORNEY": "Attorney",
}

type assigneeView struct {
	ID        string `json:"id"` // Keycloak user id: what cases.assigned_to holds (= the JWT sub)
	Username  string `json:"username"`
	OpenCases int    `json:"open_cases"`
}

// staffWithRole: enabled members of the tenant's Keycloak group holding `role`.
func (s *server) staffWithRole(ctx context.Context, tenant, role string) ([]staffMember, error) {
	tok, err := s.kcAdminToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.kcDo(ctx, http.MethodGet, "/admin/realms/idre/group-by-path/tenant/"+tenant, tok, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return []staffMember{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak group lookup: status %d", resp.StatusCode)
	}
	var group struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&group); err != nil {
		return nil, err
	}
	mr, err := s.kcDo(ctx, http.MethodGet, "/admin/realms/idre/groups/"+group.ID+"/members?max=500", tok, nil)
	if err != nil {
		return nil, err
	}
	defer mr.Body.Close()
	var users []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Enabled  bool   `json:"enabled"`
	}
	_ = json.NewDecoder(mr.Body).Decode(&users)
	out := []staffMember{}
	for _, u := range users {
		if !u.Enabled {
			continue
		}
		roles := s.kcUserRoles(ctx, tok, u.ID)
		if contains(roles, role) {
			out = append(out, staffMember{ID: u.ID, Username: u.Username, Enabled: true, Roles: roles})
		}
	}
	return out, nil
}

// openCaseCounts: assigned_to → number of the tenant's open cases they hold.
func (s *server) openCaseCounts(ctx context.Context, tenant string) map[string]int {
	out := map[string]int{}
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT assigned_to, count(*) FROM tenant_%s.cases
		WHERE assigned_to IS NOT NULL AND coalesce(status,'') NOT LIKE 'CLOSED%%'
		  AND coalesce(internal_status,'') NOT IN ('Closed','Closed-Refunded','Dismissed','Withdrawn','Ineligible','Plan Opt-Out','Decided - Invoice Paid','Provider Closure Letter Issued')
		GROUP BY 1`, sanitizeTenant(tenant)))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var who string
		var n int
		if rows.Scan(&who, &n) == nil {
			out[who] = n
		}
	}
	return out
}

func (s *server) assigneesFor(ctx context.Context, tenant, role string) ([]assigneeView, error) {
	staff, err := s.staffWithRole(ctx, tenant, role)
	if err != nil {
		return nil, err
	}
	counts := s.openCaseCounts(ctx, tenant)
	out := make([]assigneeView, 0, len(staff))
	for _, m := range staff {
		out = append(out, assigneeView{ID: m.ID, Username: m.Username, OpenCases: counts[m.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OpenCases != out[j].OpenCases {
			return out[i].OpenCases < out[j].OpenCases
		}
		return out[i].Username < out[j].Username
	})
	return out, nil
}

// listAssignees: GET /cases/assignees?role=CASE_MANAGER — the picker: eligible staff, least loaded first.
func (s *server) listAssignees(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: cases are assigned by the PM"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	role := strings.ToUpper(r.URL.Query().Get("role"))
	if role == "" {
		role = "CASE_MANAGER"
	}
	if _, ok := assignableRoles[role]; !ok {
		http.Error(w, `{"error":"unknown role"}`, http.StatusBadRequest)
		return
	}
	list, err := s.assigneesFor(r.Context(), tenant, role)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "staff directory unavailable: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": role, "role_label": assignableRoles[role], "assignees": list,
		"next_in_turn": s.nextInTurnPreview(r.Context(), tenant, role)}) // nil when the role's rotation is off or empty
}

// caseAssignment: GET /cases/{caseId}/assignment — who holds the case, by name.
func (s *server) caseAssignment(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var to, role *string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT assigned_to, assigned_role FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&to, &role); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	out := map[string]any{"assigned_to": nil, "assigned_role": nil, "name": nil, "role_label": nil}
	if to != nil && *to != "" && *to != "unassigned-pool" {
		out["assigned_to"] = *to
		out["name"] = s.resolveSubjectNames(r.Context(), []string{*to})[*to]
	}
	if role != nil && *role != "" {
		out["assigned_role"], out["role_label"] = *role, assignableRoles[*role]
	}
	writeJSON(w, http.StatusOK, out)
}

// validAssignee: the user is enabled staff of the tenant holding `role`. Returns their username.
func (s *server) validAssignee(ctx context.Context, tenant, role, userID string) (string, error) {
	staff, err := s.staffWithRole(ctx, tenant, role)
	if err != nil {
		return "", err
	}
	for _, m := range staff {
		if m.ID == userID {
			return m.Username, nil
		}
	}
	return "", nil
}

// assignCase: POST /cases/{caseId}/assign {role, assignee?, reason?, expected?, action?: "unassign" | "claim"}
// The PM and the Tenant Admin assign and reassign (SOP), to eligible staff or (Tenant Admin) to themselves. A
// Reviewer claims an unassigned case for themselves ("claim") and can do nothing else here: they cannot route cases
// to others or take a case someone holds. Every write is conditional on the assignment the caller saw, so two people
// acting at once cannot both win: the second gets 409 with who holds the case now.
func (s *server) assignCase(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Role     string  `json:"role"`
		Assignee string  `json:"assignee"` // Keycloak user id; "" = least-loaded eligible staff member
		Reason   string  `json:"reason"`
		Action   string  `json:"action"`   // "unassign" | "claim"
		Expected *string `json:"expected"` // the assignee the caller saw ("" = unassigned); a change since then is a 409
		Next     bool    `json:"next"`     // the next person in the role's rotation (autoassign.go), instead of Assignee
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	assigner := hasAnyRole(p, "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole)
	if in.Action == "claim" {
		if !assigner && !hasAnyRole(p, "CASE_MANAGER") {
			http.Error(w, `{"error":"forbidden: only a Reviewer (or the PM / Tenant Admin) can take a case"}`, http.StatusForbidden)
			return
		}
		s.claimCase(w, r, p, tenant, caseID, in.Reason)
		return
	}
	if !assigner {
		http.Error(w, `{"error":"forbidden: cases are assigned by the PM or the Tenant Admin; a Reviewer can take an unassigned case for themselves"}`, http.StatusForbidden)
		return
	}
	in.Role, in.Reason = strings.ToUpper(strings.TrimSpace(in.Role)), strings.TrimSpace(in.Reason)
	if in.Role == "" {
		in.Role = "CASE_MANAGER"
	}
	if _, ok := assignableRoles[in.Role]; !ok && in.Action != "unassign" {
		http.Error(w, `{"error":"a case is assigned as Reviewer, Arbitrator, Coder, Clinical reviewer or Attorney"}`, http.StatusBadRequest)
		return
	}
	t := sanitizeTenant(tenant)
	var cn string
	var prevTo, prevRole *string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number, assigned_to, assigned_role FROM tenant_%s.cases WHERE id=$1`, t),
		caseID).Scan(&cn, &prevTo, &prevRole); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	prev := ""
	if prevTo != nil && *prevTo != "unassigned-pool" {
		prev = *prevTo
	}
	if in.Expected != nil && *in.Expected != prev {
		s.assignmentChanged(w, r, prev)
		return
	}
	names := s.resolveSubjectNames(r.Context(), []string{prev, p.Subject})
	byName := displayName(p)

	if in.Action == "unassign" {
		if prev == "" {
			writeJSON(w, http.StatusOK, map[string]any{"assigned_to": nil, "unchanged": true})
			return
		}
		tag, err := s.db.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.cases SET assigned_to=NULL, assigned_role=NULL, updated_at=now()
			WHERE id=$1 AND assigned_to IS NOT DISTINCT FROM $2`, t), caseID, prevTo)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() == 0 {
			s.assignmentChanged(w, r, s.currentAssignee(r.Context(), tenant, caseID))
			return
		}
		msg := fmt.Sprintf("Unassigned from %s by %s%s", names[prev], byName, withReason(in.Reason))
		s.notify(r, tenant, prev, "ASSIGNMENT", fmt.Sprintf("%s was taken off your list by %s", cn, byName), "#/cases/"+caseID)
		s.logActivity(r.Context(), tenant, caseID, "MILESTONE", msg)
		s.logAudit(r.Context(), tenant, caseID, "CASE_UNASSIGNED", map[string]any{"by": p.Subject, "previous": prev, "reason": in.Reason})
		writeJSON(w, http.StatusOK, map[string]any{"assigned_to": nil})
		return
	}

	assignee, assigneeName := in.Assignee, ""
	viaTurn := false
	if in.Next {
		// The role's next person in turn; choosing them, the case update and recording the turn are one transaction.
		changed := false
		who, name, err := s.takeTurn(r.Context(), tenant, in.Role, prev, func(tx pgx.Tx, who string) (bool, error) {
			if who == prev && prevRole != nil && *prevRole == in.Role {
				return false, nil // the only one in the rotation, and they have it already
			}
			tag, err := tx.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role=$2, updated_at=now()
				WHERE id=$3 AND assigned_to IS NOT DISTINCT FROM $4`, t), who, in.Role, caseID, prevTo)
			if err != nil {
				return false, err
			}
			changed = tag.RowsAffected() == 0
			return !changed, nil
		})
		label := assignableRoles[in.Role]
		switch {
		case errors.Is(err, errRotationOff):
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("the %s rotation is off — pick a person, or turn it on on the Team page", label)})
			return
		case errors.Is(err, errRotationEmpty):
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("nobody is in the %s rotation — pick a person, or add one on the Team page", label)})
			return
		case err != nil:
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not take the next in turn: " + err.Error()})
			return
		case changed:
			s.assignmentChanged(w, r, s.currentAssignee(r.Context(), tenant, caseID))
			return
		case who == "":
			writeJSON(w, http.StatusOK, map[string]any{"assigned_to": prev, "name": names[prev], "unchanged": true})
			return
		}
		assignee, assigneeName, viaTurn = who, name, true
	} else if assignee == "" {
		list, err := s.assigneesFor(r.Context(), tenant, in.Role)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "staff directory unavailable: " + err.Error()})
			return
		}
		for _, a := range list { // least loaded first; skip the current holder so "auto" always moves the case
			if a.ID != prev || len(list) == 1 {
				assignee, assigneeName = a.ID, a.Username
				break
			}
		}
		if assignee == "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("no active %s on this tenant to assign to — add one on the Team page", assignableRoles[in.Role])})
			return
		}
	} else if assignee == p.Subject && contains(p.Roles, "TENANT_ADMIN") || assignee == p.Subject && hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		// Taking ownership: a Tenant Admin (or a platform admin) may hold a case themselves, as its Reviewer.
		assigneeName = displayName(p)
	} else if !hasAnyRole(p, serviceRole) {
		name, err := s.validAssignee(r.Context(), tenant, in.Role, assignee)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "staff directory unavailable: " + err.Error()})
			return
		}
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("that person is not an active %s on this tenant", assignableRoles[in.Role])})
			return
		}
		assigneeName = name
	}
	if !viaTurn { // (a turn has already written the case)
		if assignee == prev && prevRole != nil && *prevRole == in.Role {
			writeJSON(w, http.StatusOK, map[string]any{"assigned_to": assignee, "name": assigneeName, "unchanged": true})
			return
		}
		tag, err := s.db.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role=$2, updated_at=now()
			WHERE id=$3 AND assigned_to IS NOT DISTINCT FROM $4`, t), assignee, in.Role, caseID, prevTo)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() == 0 { // someone else changed it between our read and this write
			s.assignmentChanged(w, r, s.currentAssignee(r.Context(), tenant, caseID))
			return
		}
	}
	label := assignableRoles[in.Role]
	if viaTurn {
		label += ", next in turn"
	}
	var msg string
	if prev != "" && prev != assignee {
		msg = fmt.Sprintf("Reassigned from %s to %s (%s) by %s%s", names[prev], assigneeName, label, byName, withReason(in.Reason))
		s.notify(r, tenant, prev, "ASSIGNMENT", fmt.Sprintf("%s was reassigned to %s by %s", cn, assigneeName, byName), "#/cases/"+caseID)
	} else {
		msg = fmt.Sprintf("Assigned to %s (%s) by %s%s", assigneeName, label, byName, withReason(in.Reason))
	}
	s.notify(r, tenant, assignee, "ASSIGNMENT", fmt.Sprintf("You were assigned %s as %s by %s", cn, assignableRoles[in.Role], byName), "#/cases/"+caseID)
	s.logActivity(r.Context(), tenant, caseID, "MILESTONE", msg)
	s.logAudit(r.Context(), tenant, caseID, "CASE_ASSIGNED", map[string]any{
		"by": p.Subject, "assigned_to": assignee, "previous": prev, "role": in.Role, "manual": in.Assignee != "" && !viaTurn, "reason": in.Reason,
		"rule": map[bool]string{true: "round_robin", false: ""}[viaTurn],
	})
	writeJSON(w, http.StatusOK, map[string]any{"assigned_to": assignee, "name": assigneeName, "role": in.Role, "previous": prev})
}

// claimCase: a person takes an unassigned case for themselves, as its Reviewer. The update only applies while the
// case is still unassigned, so of two people claiming at once exactly one gets it.
func (s *server) claimCase(w http.ResponseWriter, r *http.Request, p principal, tenant, caseID, reason string) {
	t := sanitizeTenant(tenant)
	var cn string
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role='CASE_MANAGER', updated_at=now()
		WHERE id=$2 AND (assigned_to IS NULL OR assigned_to = 'unassigned-pool')
		RETURNING case_number`, t), p.Subject, caseID).Scan(&cn)
	if err != nil {
		holder := s.currentAssignee(r.Context(), tenant, caseID)
		if holder == p.Subject {
			writeJSON(w, http.StatusOK, map[string]any{"assigned_to": p.Subject, "name": displayName(p), "unchanged": true})
			return
		}
		if holder == "" { // no such case
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		// A Reviewer is not told who: the case is someone else's now, and out of their view.
		msg := "Someone else took this case a moment ago."
		out := map[string]any{"error": msg, "taken": true}
		if !reviewerScoped(p) {
			name := s.resolveSubjectNames(r.Context(), []string{holder})[holder]
			out["error"], out["assigned_to"], out["name"] = "It is already assigned to "+name+".", holder, name
		}
		writeJSON(w, http.StatusConflict, out)
		return
	}
	who := displayName(p)
	s.logActivity(r.Context(), tenant, caseID, "MILESTONE", fmt.Sprintf("Taken by %s (Reviewer)%s", who, withReason(reason)))
	s.logAudit(r.Context(), tenant, caseID, "CASE_ASSIGNED", map[string]any{
		"by": p.Subject, "assigned_to": p.Subject, "previous": "", "role": "CASE_MANAGER", "claim": true, "reason": reason,
	})
	writeJSON(w, http.StatusOK, map[string]any{"assigned_to": p.Subject, "name": who, "role": "CASE_MANAGER", "previous": ""})
}

// currentAssignee: who holds the case now ("" when unassigned or not found).
func (s *server) currentAssignee(ctx context.Context, tenant, caseID string) string {
	var to *string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT assigned_to FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&to)
	if v := deref(to); v != "unassigned-pool" {
		return v
	}
	return ""
}

// assignmentChanged answers a write that lost a race (or was based on a stale page): 409, with who holds it now.
func (s *server) assignmentChanged(w http.ResponseWriter, r *http.Request, holder string) {
	name := "nobody (unassigned)"
	if holder != "" {
		name = s.resolveSubjectNames(r.Context(), []string{holder})[holder]
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":       "The assignment changed while you were choosing — it is now with " + name + ". Look again before changing it.",
		"changed":     true,
		"assigned_to": holder, "name": name,
	})
}

// withReason: " — reason" for an activity line, or nothing when no reason was given.
func withReason(reason string) string {
	if reason = strings.TrimSpace(reason); reason == "" {
		return ""
	}
	return " — " + reason
}
