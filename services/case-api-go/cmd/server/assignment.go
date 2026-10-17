package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
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
	writeJSON(w, http.StatusOK, map[string]any{"role": role, "role_label": assignableRoles[role], "assignees": list})
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

// assignCase: POST /cases/{caseId}/assign {role, assignee?, reason?, action?: "unassign"}
// The PM assigns and reassigns (SOP). A Reviewer still takes work with "Grab next"; it cannot route cases to others.
func (s *server) assignCase(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: cases are assigned by the PM"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Role     string `json:"role"`
		Assignee string `json:"assignee"` // Keycloak user id; "" = least-loaded eligible staff member
		Reason   string `json:"reason"`
		Action   string `json:"action"` // "unassign"
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
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
	names := s.resolveSubjectNames(r.Context(), []string{prev, p.Subject})
	byName := displayName(p)

	if in.Action == "unassign" {
		if prev == "" {
			writeJSON(w, http.StatusOK, map[string]any{"assigned_to": nil, "unchanged": true})
			return
		}
		if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.cases SET assigned_to=NULL, assigned_role=NULL, updated_at=now() WHERE id=$1`, t), caseID); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
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
	if assignee == "" {
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
	if assignee == prev && prevRole != nil && *prevRole == in.Role {
		writeJSON(w, http.StatusOK, map[string]any{"assigned_to": assignee, "name": assigneeName, "unchanged": true})
		return
	}
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role=$2, updated_at=now() WHERE id=$3`, t),
		assignee, in.Role, caseID); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	label := assignableRoles[in.Role]
	var msg string
	if prev != "" && prev != assignee {
		msg = fmt.Sprintf("Reassigned from %s to %s (%s) by %s%s", names[prev], assigneeName, label, byName, withReason(in.Reason))
		s.notify(r, tenant, prev, "ASSIGNMENT", fmt.Sprintf("%s was reassigned to %s by %s", cn, assigneeName, byName), "#/cases/"+caseID)
	} else {
		msg = fmt.Sprintf("Assigned to %s (%s) by %s%s", assigneeName, label, byName, withReason(in.Reason))
	}
	s.notify(r, tenant, assignee, "ASSIGNMENT", fmt.Sprintf("You were assigned %s as %s by %s", cn, label, byName), "#/cases/"+caseID)
	s.logActivity(r.Context(), tenant, caseID, "MILESTONE", msg)
	s.logAudit(r.Context(), tenant, caseID, "CASE_ASSIGNED", map[string]any{
		"by": p.Subject, "assigned_to": assignee, "previous": prev, "role": in.Role, "manual": in.Assignee != "", "reason": in.Reason,
	})
	writeJSON(w, http.StatusOK, map[string]any{"assigned_to": assignee, "name": assigneeName, "role": in.Role, "previous": prev})
}

// withReason: " — reason" for an activity line, or nothing when no reason was given.
func withReason(reason string) string {
	if reason = strings.TrimSpace(reason); reason == "" {
		return ""
	}
	return " — " + reason
}
