package main

// staffroles.go — the PM and the Tenant Admin change what a staff member does (Reviewer, Coder, Attorney…) from the
// Team page, instead of only at account creation.
//
// Only the tenant staff roles are touched (staffEditableRoles); anything else the account holds in Keycloak
// (default roles, a platform role) is left as it is. The rules match account creation (createTenantStaff): a PM or
// Tenant Admin acts only on members of their own tenant, nobody changes their own roles, and giving or taking away
// TENANT_ADMIN stays with the federal / platform admins.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

// staffEditableRoles: what the Team page can give or take away. The outside roles (PARTY, TPA) belong to filer
// accounts, not staff.
var staffEditableRoles = map[string]bool{
	"CASE_MANAGER": true, "ARBITRATOR": true, "PM": true, "CODER": true, "NURSE_PHYSICIAN": true, "DOCTOR": true,
	"NURSE": true, "ATTORNEY": true, "FINANCE": true, "STATE_AUDITOR": true, "TENANT_ADMIN": true,
}

// roleChanges: what to add and remove to go from `current` to `want`, limited to the editable roles. Sorted, so it
// is stable to test and to audit.
func roleChanges(current, want []string) (add, remove []string) {
	for _, r := range want {
		if staffEditableRoles[r] && !contains(current, r) && !contains(add, r) {
			add = append(add, r)
		}
	}
	for _, r := range current {
		if staffEditableRoles[r] && !contains(want, r) {
			remove = append(remove, r)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

// kcRemoveRealmRole takes one realm role off a user (Keycloak wants the role's id, so it is looked up first).
func (s *server) kcRemoveRealmRole(ctx context.Context, tok, userID, roleName string) error {
	rr, err := s.kcDo(ctx, http.MethodGet, "/admin/realms/idre/roles/"+roleName, tok, nil)
	if err != nil {
		return err
	}
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusOK {
		return fmt.Errorf("role lookup: status %d", rr.StatusCode)
	}
	var role struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&role); err != nil {
		return err
	}
	body, _ := json.Marshal([]map[string]string{{"id": role.ID, "name": role.Name}})
	resp, err := s.kcDo(ctx, http.MethodDelete, "/admin/realms/idre/users/"+userID+"/role-mappings/realm", tok, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("role remove: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// setTenantStaffRoles: PUT /v1/admin/tenant-staff/{username}/roles {"tenant":"fl","roles":["CASE_MANAGER","CODER"]}
// — the person's full set of staff roles. Takes effect at their next sign-in (or token refresh).
func (s *server) setTenantStaffRoles(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	username := chi.URLParam(r, "username")
	var in struct {
		Tenant string   `json:"tenant"`
		Roles  []string `json:"roles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Tenant == "" {
		http.Error(w, `{"error":"tenant and roles required"}`, http.StatusBadRequest)
		return
	}
	in.Tenant = strings.ToLower(strings.TrimSpace(in.Tenant))
	want := []string{}
	for _, ro := range in.Roles {
		ro = strings.ToUpper(strings.TrimSpace(ro))
		if !staffEditableRoles[ro] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%q is not a staff role", ro)})
			return
		}
		if !contains(want, ro) {
			want = append(want, ro)
		}
	}
	if len(want) == 0 {
		http.Error(w, `{"error":"a staff member needs at least one role — suspend the account instead"}`, http.StatusBadRequest)
		return
	}
	isAdmin := hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN")
	if !isAdmin && (!hasAnyRole(p, "PM", "TENANT_ADMIN") || !contains(p.Tenants, in.Tenant)) {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN/PLATFORM_ADMIN, or PM/TENANT_ADMIN acting on their own tenant"}`, http.StatusForbidden)
		return
	}
	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak admin auth failed: " + err.Error()})
		return
	}
	userID, err := s.kcFindUserByUsername(r.Context(), tok, username)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak user lookup failed: " + err.Error()})
		return
	}
	if userID == "" {
		http.Error(w, `{"error":"no such user"}`, http.StatusNotFound)
		return
	}
	if userID == p.Subject {
		http.Error(w, `{"error":"you cannot change your own roles — ask another PM or an admin"}`, http.StatusBadRequest)
		return
	}
	if !isAdmin {
		inTenant, err := s.kcUserInTenantGroup(r.Context(), tok, userID, in.Tenant)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak membership check failed: " + err.Error()})
			return
		}
		if !inTenant {
			http.Error(w, `{"error":"no such user"}`, http.StatusNotFound)
			return
		}
	}
	current := s.kcUserRoles(r.Context(), tok, userID)
	add, remove := roleChanges(current, want)
	if !isAdmin && (contains(add, "TENANT_ADMIN") || contains(remove, "TENANT_ADMIN")) {
		http.Error(w, `{"error":"forbidden: only FEDERAL_ADMIN/PLATFORM_ADMIN may give or take away TENANT_ADMIN"}`, http.StatusForbidden)
		return
	}
	for _, ro := range add {
		if err := s.kcAssignRealmRole(r.Context(), tok, userID, ro); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak role update failed: " + err.Error()})
			return
		}
	}
	for _, ro := range remove {
		if err := s.kcRemoveRealmRole(r.Context(), tok, userID, ro); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak role update failed: " + err.Error()})
			return
		}
	}
	forgetRotationStaff(in.Tenant) // the rotations follow who holds which role
	s.logAudit(r.Context(), in.Tenant, "", "TENANT_STAFF_ROLES_CHANGED", map[string]any{
		"by": p.Subject, "username": username, "added": add, "removed": remove,
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": username, "roles": s.kcUserRoles(r.Context(), tok, userID), "added": add, "removed": remove})
}
