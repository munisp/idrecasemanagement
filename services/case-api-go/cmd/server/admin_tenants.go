package main

// admin_tenants.go — platform-level tenant provisioning (Keycloak group +
// optional first user). Postgres already provisions tenant_<xx> schemas for
// all 50 US states ahead of time (scripts/init-schemas.sql); what actually
// gates a tenant being usable is the Keycloak side -- tenancy() in main.go
// only allows a non-admin principal into a tenant whose /tenant/<xx> group
// is in their token. Until this endpoint, no code anywhere created that
// group; it was always done by hand against the live realm (confirmed:
// deploy/keycloak/realm-idre.json isn't synced by any config-cli sidecar in
// this cluster, so the checked-in file is documentation, not live state).
//
// Group shape confirmed live against the real idre realm: groups are flat
// top-level groups literally named "tenant/<xx>" (not nested subgroups under
// a parent "tenant" group), which the Admin REST API accepts as-is via
// POST /admin/realms/idre/groups {"name": "tenant/<xx>"} and resolves to
// path "/tenant/<xx>" -- exactly what tenancy()'s "/tenant/" prefix check
// expects from the groups claim.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var kcHTTP = &http.Client{Timeout: 15 * time.Second}

var tenantCodeRe = regexp.MustCompile(`^[a-z]{2}$`)

// generateTempPassword satisfies the idre realm's password policy
// (length(12) and digits(1) and upperCase(1) and specialChars(1)) -- a
// plain hex string never has uppercase or special chars and Keycloak
// 400s the user-create call on it. Confirmed live against
// GET /admin/realms/idre?fields=passwordPolicy.
func generateTempPassword() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var extra [2]byte
	if _, err := rand.Read(extra[:]); err != nil {
		return "", err
	}
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const special = "!@#$%^&*-_="
	return hex.EncodeToString(buf) +
		string(upper[int(extra[0])%len(upper)]) +
		string(special[int(extra[1])%len(special)]), nil
}

var validTenantRoles = map[string]bool{
	"CASE_MANAGER": true, "ARBITRATOR": true, "PM": true, "CODER": true,
	"NURSE_PHYSICIAN": true, "DOCTOR": true, "NURSE": true, "ATTORNEY": true, "FINANCE": true,
	"STATE_AUDITOR": true, "PARTY": true,
}

type createTenantRequest struct {
	Tenant    string            `json:"tenant"`
	Label     string            `json:"label"`
	FirstUser *createTenantUser `json:"first_user"`
}

type createTenantUser struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

// kcAdminToken exchanges the service admin credentials for a master-realm
// access token -- the same grant idre-workflows already uses in
// onboarding_activities.py's provision_keycloak_account.
func (s *server) kcAdminToken(ctx context.Context) (string, error) {
	if s.cfg.KCAdmin == "" || s.cfg.KCAdminPassword == "" {
		return "", fmt.Errorf("KC_ADMIN/KC_ADMIN_PASSWORD not configured")
	}
	form := url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {s.cfg.KCAdmin}, "password": {s.cfg.KCAdminPassword},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.KeycloakURL+"/realms/master/protocol/openid-connect/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := kcHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak admin login: %s", resp.Status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

func (s *server) kcDo(ctx context.Context, method, path string, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.KeycloakURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return kcHTTP.Do(req)
}

// kcEnsureGroup creates the Keycloak group for tenant if it doesn't already
// exist. 409 (already exists) is treated as success -- same idempotent
// shape createTenant's own inline group-create step already had, extracted
// so createTenantStaff can ensure a target tenant's group exists too
// without duplicating this.
func (s *server) kcEnsureGroup(ctx context.Context, tok, tenant string) (alreadyExisted bool, err error) {
	groupBody, _ := json.Marshal(map[string]string{"name": "tenant/" + tenant})
	resp, err := s.kcDo(ctx, http.MethodPost, "/admin/realms/idre/groups", tok, groupBody)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		return false, nil
	case http.StatusConflict:
		return true, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// kcAssignRealmRole assigns one realm role to a user by name. Keycloak's
// POST .../users/{id}/role-mappings/realm needs the role's id (not just its
// name), so this looks the role up first via GET .../roles/{roleName}.
func (s *server) kcAssignRealmRole(ctx context.Context, tok, userID, roleName string) error {
	roleResp, err := s.kcDo(ctx, http.MethodGet, "/admin/realms/idre/roles/"+roleName, tok, nil)
	if err != nil {
		return err
	}
	defer roleResp.Body.Close()
	if roleResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(roleResp.Body, 2048))
		return fmt.Errorf("role lookup: status %d: %s", roleResp.StatusCode, strings.TrimSpace(string(body)))
	}
	var role struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(roleResp.Body).Decode(&role); err != nil {
		return err
	}
	assignBody, _ := json.Marshal([]map[string]string{{"id": role.ID, "name": role.Name}})
	assignResp, err := s.kcDo(ctx, http.MethodPost, "/admin/realms/idre/users/"+userID+"/role-mappings/realm", tok, assignBody)
	if err != nil {
		return err
	}
	defer assignResp.Body.Close()
	if assignResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(assignResp.Body, 2048))
		return fmt.Errorf("role assign: status %d: %s", assignResp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// kcCreateUser creates a Keycloak user with a generated temporary password
// in the given groups, then assigns the given realm role as a SEPARATE
// follow-up call. Never returns a Go error -- a failure (conflict, bad
// gateway, etc.) comes back as an "error" key in the result map, matching
// createTenant's existing tolerant shape where a first-user failure
// doesn't fail the whole request (the group/tenant side may have already
// succeeded).
//
// The role used to be passed as "realmRoles" in the create-user request
// body -- confirmed live, against this exact Keycloak deployment, that
// field is silently ignored by POST .../admin/realms/{realm}/users: the
// account was created and reported success, but the role was never
// actually attached (verified by re-listing the created user's role
// mappings -- empty, every time, for every account this endpoint had
// created so far). Role assignment is now its own real API call, and a
// failure there is surfaced distinctly from user-creation failure, since
// "the account exists but has no role" is a materially different, worse
// outcome than "nothing was created at all" and callers need to be able
// to tell them apart.
func (s *server) kcCreateUser(ctx context.Context, tok, username, email, role string, groups []string) map[string]any {
	tempPassword, err := generateTempPassword()
	if err != nil {
		return map[string]any{"error": "password generation failed: " + err.Error()}
	}
	userBody, _ := json.Marshal(map[string]any{
		"username": username, "email": email,
		"enabled": true, "emailVerified": false,
		"groups": groups,
		"credentials": []map[string]any{
			{"type": "password", "value": tempPassword, "temporary": true},
		},
	})
	userResp, err := s.kcDo(ctx, http.MethodPost, "/admin/realms/idre/users", tok, userBody)
	if err != nil {
		return map[string]any{"error": "keycloak user create failed: " + err.Error()}
	}
	defer userResp.Body.Close()
	switch userResp.StatusCode {
	case http.StatusCreated:
		out := map[string]any{
			"username": username, "role": role,
			"temporary_password": tempPassword, // one-time: share out-of-band, never logged
		}
		loc := userResp.Header.Get("Location")
		parts := strings.Split(loc, "/")
		userID := parts[len(parts)-1]
		if loc == "" || userID == "" {
			out["role_assignment_error"] = "user created but could not determine its id from the Location header -- role was not assigned"
			return out
		}
		if err := s.kcAssignRealmRole(ctx, tok, userID, role); err != nil {
			out["role_assignment_error"] = "user created but role assignment failed: " + err.Error()
		}
		return out
	case http.StatusConflict:
		return map[string]any{"error": "a user with this username or email already exists"}
	default:
		body, _ := io.ReadAll(io.LimitReader(userResp.Body, 2048))
		return map[string]any{"error": fmt.Sprintf("keycloak user create: status %d: %s", userResp.StatusCode, strings.TrimSpace(string(body)))}
	}
}

// createTenant: POST /v1/admin/tenants -- activates a pre-provisioned state
// schema for real login access by creating its Keycloak group, and
// optionally its first user in one call (a tenant nobody belongs to is
// invisible to everyone but FEDERAL_ADMIN/PLATFORM_ADMIN).
func (s *server) createTenant(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"tenant creation is admin-managed (FEDERAL_ADMIN or PLATFORM_ADMIN required)"}`, http.StatusForbidden)
		return
	}
	var in createTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	in.Tenant = strings.ToLower(strings.TrimSpace(in.Tenant))
	if !tenantCodeRe.MatchString(in.Tenant) {
		http.Error(w, `{"error":"tenant must be a 2-letter lowercase code (e.g. 'ga')"}`, http.StatusBadRequest)
		return
	}
	if in.FirstUser != nil {
		if in.FirstUser.Username == "" || in.FirstUser.Email == "" {
			http.Error(w, `{"error":"first_user requires username and email"}`, http.StatusBadRequest)
			return
		}
		if !validTenantRoles[in.FirstUser.Role] {
			http.Error(w, `{"error":"first_user.role must be one of CASE_MANAGER, ARBITRATOR, PM, CODER, DOCTOR, NURSE, NURSE_PHYSICIAN, ATTORNEY, FINANCE, STATE_AUDITOR, PARTY"}`, http.StatusBadRequest)
			return
		}
	}

	// Confirm the DB side actually exists rather than silently activating a
	// Keycloak group for a tenant with no backing schema -- the 50 US states
	// are pre-provisioned by scripts/init-schemas.sql; anything else needs
	// that file (and program-rules.sql's column-backfill loop) extended and
	// the idre-schema-apply job re-run before it can hold real cases.
	var exists bool
	if err := s.db.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name=$1)`,
		"tenant_"+in.Tenant).Scan(&exists); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, fmt.Sprintf(
			`{"error":"tenant_%s has no provisioned database schema -- add it to scripts/init-schemas.sql's state array and re-run the schema-apply job first"}`,
			in.Tenant), http.StatusConflict)
		return
	}

	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak admin auth failed: " + err.Error()})
		return
	}

	groupPath := "/tenant/" + in.Tenant
	groupAlreadyExisted, err := s.kcEnsureGroup(r.Context(), tok, in.Tenant)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak group create failed: " + err.Error()})
		return
	}

	out := map[string]any{
		"tenant": in.Tenant, "group": groupPath,
		"group_already_existed": groupAlreadyExisted,
	}
	// Chained under the newly activated tenant. Never includes the
	// temporary password -- only whether the first user was created.
	auditTenantCreated := func() {
		payload := map[string]any{
			"by": p.Subject, "group": groupPath,
			"group_already_existed": groupAlreadyExisted,
		}
		if in.FirstUser != nil {
			_, created := out["first_user"]
			payload["first_user"] = in.FirstUser.Username
			payload["first_user_role"] = in.FirstUser.Role
			payload["first_user_created"] = created
		}
		s.logAudit(r.Context(), in.Tenant, "", "TENANT_CREATED", payload)
	}

	if in.FirstUser != nil {
		result := s.kcCreateUser(r.Context(), tok, in.FirstUser.Username, in.FirstUser.Email, in.FirstUser.Role, []string{groupPath})
		if errMsg, isErr := result["error"]; isErr {
			out["first_user_error"] = errMsg
		} else {
			out["first_user"] = result
		}
	}

	auditTenantCreated()
	writeJSON(w, http.StatusCreated, out)
}

// createFederalAdminRequest / createFederalAdmin: POST
// /v1/tenants/{tenant}/admin/federal-admins -- creates a cross-tenant
// FEDERAL_ADMIN or PLATFORM_ADMIN account. There was previously no way to
// create one of these through the app at all -- federal.admin itself was
// seeded by hand-editing the Keycloak realm. Gated to the federal tier
// itself: either PLATFORM_ADMIN or FEDERAL_ADMIN may create more of
// either (scoped with the user: a FEDERAL_ADMIN may mint another
// FEDERAL_ADMIN or a PLATFORM_ADMIN, not restricted to PLATFORM_ADMIN-only).
type createFederalAdminRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"` // FEDERAL_ADMIN | PLATFORM_ADMIN
}

func (s *server) createFederalAdmin(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	var in createFederalAdminRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Username == "" || in.Email == "" {
		http.Error(w, `{"error":"username and email required"}`, http.StatusBadRequest)
		return
	}
	if in.Role != "FEDERAL_ADMIN" && in.Role != "PLATFORM_ADMIN" {
		http.Error(w, `{"error":"role must be FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusBadRequest)
		return
	}
	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak admin auth failed: " + err.Error()})
		return
	}
	// No tenant group -- cross-tenant roles match federal.admin's own seed
	// (realmRoles: [FEDERAL_ADMIN], groups: []).
	result := s.kcCreateUser(r.Context(), tok, in.Username, in.Email, in.Role, []string{})
	_, failed := result["error"]
	// This route (/v1/admin/*) has no {tenant} URL segment and never runs
	// tenancy(), so there is no ctxTenant value here (unlike every
	// /v1/tenants/{tenant}/* handler) -- "" buckets this as its own global
	// audit chain rather than attaching a cross-tenant action to whichever
	// tenant the caller happened to be looking at.
	s.logAudit(r.Context(), "", "", "FEDERAL_ADMIN_CREATED", map[string]any{
		"by": p.Subject, "username": in.Username, "role": in.Role, "created": !failed,
	})
	if failed {
		writeJSON(w, http.StatusConflict, result)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// createTenantStaffRequest / createTenantStaff: POST
// /v1/tenants/{tenant}/admin/tenant-staff -- adds a new staff account to
// an EXISTING, already-activated tenant without going through
// createTenant's first_user field again (which means resubmitting a
// whole tenant-creation request just to piggyback a second user). A
// CASE_MANAGER may only add staff to their own tenant (the one their
// token already grants); FEDERAL_ADMIN/PLATFORM_ADMIN may target any
// tenant, scoped with the user.
type createTenantStaffRequest struct {
	Tenant   string `json:"tenant"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

func (s *server) createTenantStaff(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in createTenantStaffRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Username == "" || in.Email == "" || in.Tenant == "" {
		http.Error(w, `{"error":"tenant, username and email required"}`, http.StatusBadRequest)
		return
	}
	in.Tenant = strings.ToLower(strings.TrimSpace(in.Tenant))
	// This route has no {tenant} URL segment (unlike every
	// /v1/tenants/{tenant}/* handler) -- "the caller's own tenant" comes
	// from the JWT's group claim (principal.Tenants) directly, same source
	// tenancy() itself reads from, not a URL-derived ctxTenant that
	// doesn't exist on this route.
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		if !hasRole(p, "CASE_MANAGER") || !contains(p.Tenants, in.Tenant) {
			http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN/PLATFORM_ADMIN, or CASE_MANAGER adding staff to their own tenant"}`, http.StatusForbidden)
			return
		}
	}
	if !validTenantRoles[in.Role] {
		http.Error(w, `{"error":"role must be one of CASE_MANAGER, ARBITRATOR, PM, CODER, DOCTOR, NURSE, NURSE_PHYSICIAN, ATTORNEY, FINANCE, STATE_AUDITOR, PARTY"}`, http.StatusBadRequest)
		return
	}
	var exists bool
	if err := s.db.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name=$1)`,
		"tenant_"+in.Tenant).Scan(&exists); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, fmt.Sprintf(`{"error":"tenant_%s has no provisioned database schema"}`, in.Tenant), http.StatusConflict)
		return
	}
	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak admin auth failed: " + err.Error()})
		return
	}
	groupPath := "/tenant/" + in.Tenant
	if _, err := s.kcEnsureGroup(r.Context(), tok, in.Tenant); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak group ensure failed: " + err.Error()})
		return
	}
	result := s.kcCreateUser(r.Context(), tok, in.Username, in.Email, in.Role, []string{groupPath})
	_, failed := result["error"]
	s.logAudit(r.Context(), in.Tenant, "", "TENANT_STAFF_CREATED", map[string]any{
		"by": p.Subject, "username": in.Username, "role": in.Role, "created": !failed,
	})
	if failed {
		writeJSON(w, http.StatusConflict, result)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

type staffMember struct {
	Username string   `json:"username"`
	Email    string   `json:"email"`
	Enabled  bool     `json:"enabled"`
	Roles    []string `json:"roles"`
}

// kcUserRoles fetches one user's realm role names. Keycloak's group-members
// listing only returns basic user fields (username/email/enabled), not
// roles, so the Team page -- which exists specifically to show WHO has
// WHAT role -- needs one follow-up call per member. Team sizes here are a
// handful of staff per tenant, not thousands, so the N+1 is fine; silently
// returns an empty slice on any error rather than failing the whole list
// over one bad lookup.
func (s *server) kcUserRoles(ctx context.Context, tok, userID string) []string {
	resp, err := s.kcDo(ctx, http.MethodGet, "/admin/realms/idre/users/"+userID+"/role-mappings/realm", tok, nil)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var roles []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&roles); err != nil {
		return nil
	}
	out := make([]string, 0, len(roles))
	for _, ro := range roles {
		out = append(out, ro.Name)
	}
	return out
}

// kcFindUserByUsername resolves a username to its Keycloak user id via an
// exact-match search -- suspend/delete act on a username (what the Team
// page displays and what an admin types), not the opaque Keycloak id.
func (s *server) kcFindUserByUsername(ctx context.Context, tok, username string) (string, error) {
	resp, err := s.kcDo(ctx, http.MethodGet,
		"/admin/realms/idre/users?username="+url.QueryEscape(username)+"&exact=true", tok, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("user lookup: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var users []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", nil // not found -- caller treats as 404, not a transport error
	}
	return users[0].ID, nil
}

// kcUserInTenantGroup checks the target user's OWN group membership against
// the claimed tenant, rather than trusting a caller-supplied tenant param --
// a CASE_MANAGER's suspend/delete request is scoped to their own tenant, so
// this is what stops them reaching a user who merely has a similar username
// in a different tenant.
func (s *server) kcUserInTenantGroup(ctx context.Context, tok, userID, tenant string) (bool, error) {
	resp, err := s.kcDo(ctx, http.MethodGet, "/admin/realms/idre/users/"+userID+"/groups", tok, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("user groups lookup: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var groups []struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return false, err
	}
	want := "/tenant/" + tenant
	for _, g := range groups {
		if g.Path == want {
			return true, nil
		}
	}
	return false, nil
}

// kcSetUserEnabled suspends (enabled=false) or reactivates (enabled=true) a
// user. Keycloak's PUT .../users/{id} tolerates a partial representation for
// this field -- confirmed live, it does not null out the rest of the account.
func (s *server) kcSetUserEnabled(ctx context.Context, tok, userID string, enabled bool) error {
	body, _ := json.Marshal(map[string]bool{"enabled": enabled})
	resp, err := s.kcDo(ctx, http.MethodPut, "/admin/realms/idre/users/"+userID, tok, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// kcDeleteUser permanently removes a Keycloak account. There is no undo --
// callers must have already resolved and tenant-scoped the target user id.
func (s *server) kcDeleteUser(ctx context.Context, tok, userID string) error {
	resp, err := s.kcDo(ctx, http.MethodDelete, "/admin/realms/idre/users/"+userID, tok, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// resolveStaffTarget runs the common suspend/delete preamble shared by the
// tenant-staff and federal-admin variants: Keycloak admin auth, username ->
// id resolution (404 if no such user), and the caller-is-the-target guard
// (self-suspend/self-delete would let an admin lock themselves out with no
// one left able to undo it). Returns ("", "") and has already written the
// HTTP response when ok is false.
func (s *server) resolveStaffTarget(w http.ResponseWriter, r *http.Request, p principal, username string) (tok, userID string, ok bool) {
	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak admin auth failed: " + err.Error()})
		return "", "", false
	}
	userID, err = s.kcFindUserByUsername(r.Context(), tok, username)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak user lookup failed: " + err.Error()})
		return "", "", false
	}
	if userID == "" {
		http.Error(w, `{"error":"no such user"}`, http.StatusNotFound)
		return "", "", false
	}
	if userID == p.Subject {
		http.Error(w, `{"error":"cannot suspend or delete your own account"}`, http.StatusBadRequest)
		return "", "", false
	}
	return tok, userID, true
}

// updateTenantStaffStatus: PATCH /v1/admin/tenant-staff/{username}
// {"tenant":"xx","enabled":bool} -- suspend (enabled=false) or reactivate
// (enabled=true). Same access scoping as createTenantStaff/listTenantStaff,
// but a CASE_MANAGER's own-tenant claim is additionally checked against the
// TARGET's real group membership (kcUserInTenantGroup), not just trusted
// from the request body -- otherwise any CASE_MANAGER could suspend any
// user platform-wide by simply naming their own tenant in the body.
func (s *server) updateTenantStaffStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	username := chi.URLParam(r, "username")
	var in struct {
		Tenant  string `json:"tenant"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Tenant == "" {
		http.Error(w, `{"error":"tenant required"}`, http.StatusBadRequest)
		return
	}
	in.Tenant = strings.ToLower(strings.TrimSpace(in.Tenant))
	isAdmin := hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN")
	if !isAdmin && (!hasRole(p, "CASE_MANAGER") || !contains(p.Tenants, in.Tenant)) {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN/PLATFORM_ADMIN, or CASE_MANAGER acting on their own tenant"}`, http.StatusForbidden)
		return
	}
	tok, userID, ok := s.resolveStaffTarget(w, r, p, username)
	if !ok {
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
	if err := s.kcSetUserEnabled(r.Context(), tok, userID, in.Enabled); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak status update failed: " + err.Error()})
		return
	}
	action := "TENANT_STAFF_SUSPENDED"
	if in.Enabled {
		action = "TENANT_STAFF_REACTIVATED"
	}
	s.logAudit(r.Context(), in.Tenant, "", action, map[string]any{
		"by": p.Subject, "username": username, "enabled": in.Enabled,
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": username, "enabled": in.Enabled})
}

// deleteTenantStaff: DELETE /v1/admin/tenant-staff/{username}?tenant=xx --
// permanent. Same scoping as updateTenantStaffStatus.
func (s *server) deleteTenantStaff(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	username := chi.URLParam(r, "username")
	tenant := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tenant")))
	if tenant == "" {
		http.Error(w, `{"error":"tenant query param required"}`, http.StatusBadRequest)
		return
	}
	isAdmin := hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN")
	if !isAdmin && (!hasRole(p, "CASE_MANAGER") || !contains(p.Tenants, tenant)) {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN/PLATFORM_ADMIN, or CASE_MANAGER acting on their own tenant"}`, http.StatusForbidden)
		return
	}
	tok, userID, ok := s.resolveStaffTarget(w, r, p, username)
	if !ok {
		return
	}
	if !isAdmin {
		inTenant, err := s.kcUserInTenantGroup(r.Context(), tok, userID, tenant)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak membership check failed: " + err.Error()})
			return
		}
		if !inTenant {
			http.Error(w, `{"error":"no such user"}`, http.StatusNotFound)
			return
		}
	}
	if err := s.kcDeleteUser(r.Context(), tok, userID); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak delete failed: " + err.Error()})
		return
	}
	s.logAudit(r.Context(), tenant, "", "TENANT_STAFF_DELETED", map[string]any{
		"by": p.Subject, "username": username,
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": username, "deleted": true})
}

// updateFederalAdminStatus: PATCH /v1/admin/federal-admins/{username}
// {"enabled":bool} -- FEDERAL_ADMIN/PLATFORM_ADMIN only, no tenant scoping
// (these accounts aren't tenant members).
func (s *server) updateFederalAdminStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	username := chi.URLParam(r, "username")
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	tok, userID, ok := s.resolveStaffTarget(w, r, p, username)
	if !ok {
		return
	}
	if err := s.kcSetUserEnabled(r.Context(), tok, userID, in.Enabled); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak status update failed: " + err.Error()})
		return
	}
	action := "FEDERAL_ADMIN_SUSPENDED"
	if in.Enabled {
		action = "FEDERAL_ADMIN_REACTIVATED"
	}
	s.logAudit(r.Context(), "", "", action, map[string]any{
		"by": p.Subject, "username": username, "enabled": in.Enabled,
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": username, "enabled": in.Enabled})
}

// deleteFederalAdmin: DELETE /v1/admin/federal-admins/{username} --
// permanent, FEDERAL_ADMIN/PLATFORM_ADMIN only.
func (s *server) deleteFederalAdmin(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	username := chi.URLParam(r, "username")
	tok, userID, ok := s.resolveStaffTarget(w, r, p, username)
	if !ok {
		return
	}
	if err := s.kcDeleteUser(r.Context(), tok, userID); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak delete failed: " + err.Error()})
		return
	}
	s.logAudit(r.Context(), "", "", "FEDERAL_ADMIN_DELETED", map[string]any{
		"by": p.Subject, "username": username,
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": username, "deleted": true})
}

// listTenantStaff: GET /v1/admin/tenant-staff?tenant=<xx> -- members of
// that tenant's Keycloak group, with their roles. Powers the Team page,
// which previously let you ADD staff but had no way to see who's already
// there (confirmed live: "i can't see the list of team members"). Same
// access scoping as createTenantStaff: any tenant for FEDERAL_ADMIN/
// PLATFORM_ADMIN, own tenant only for CASE_MANAGER.
func (s *server) listTenantStaff(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tenant")))
	if tenant == "" {
		http.Error(w, `{"error":"tenant query param required"}`, http.StatusBadRequest)
		return
	}
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		if !hasRole(p, "CASE_MANAGER") || !contains(p.Tenants, tenant) {
			http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN/PLATFORM_ADMIN, or CASE_MANAGER viewing their own tenant"}`, http.StatusForbidden)
			return
		}
	}
	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak admin auth failed: " + err.Error()})
		return
	}
	groupResp, err := s.kcDo(r.Context(), http.MethodGet, "/admin/realms/idre/group-by-path/tenant/"+tenant, tok, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak group lookup failed: " + err.Error()})
		return
	}
	defer groupResp.Body.Close()
	if groupResp.StatusCode == http.StatusNotFound {
		writeJSON(w, http.StatusOK, []staffMember{}) // tenant has no group yet -- no staff, not an error
		return
	}
	if groupResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(groupResp.Body, 2048))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("keycloak group lookup: status %d: %s", groupResp.StatusCode, strings.TrimSpace(string(body)))})
		return
	}
	var group struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(groupResp.Body).Decode(&group); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak group decode failed: " + err.Error()})
		return
	}
	membersResp, err := s.kcDo(r.Context(), http.MethodGet, "/admin/realms/idre/groups/"+group.ID+"/members?max=200", tok, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak members fetch failed: " + err.Error()})
		return
	}
	defer membersResp.Body.Close()
	var kcUsers []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Email    string `json:"email"`
		Enabled  bool   `json:"enabled"`
	}
	_ = json.NewDecoder(membersResp.Body).Decode(&kcUsers)
	out := make([]staffMember, 0, len(kcUsers))
	for _, u := range kcUsers {
		out = append(out, staffMember{
			Username: u.Username, Email: u.Email, Enabled: u.Enabled,
			Roles: s.kcUserRoles(r.Context(), tok, u.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// listFederalAdmins: GET /v1/admin/federal-admins -- cross-tenant admin
// accounts (FEDERAL_ADMIN/PLATFORM_ADMIN), for the same reason and the
// same previously-missing listing as listTenantStaff above.
func (s *server) listFederalAdmins(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FEDERAL_ADMIN or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak admin auth failed: " + err.Error()})
		return
	}
	out := []staffMember{}
	for _, role := range []string{"FEDERAL_ADMIN", "PLATFORM_ADMIN"} {
		resp, err := s.kcDo(r.Context(), http.MethodGet, "/admin/realms/idre/roles/"+role+"/users?max=200", tok, nil)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var users []struct {
			Username string `json:"username"`
			Email    string `json:"email"`
			Enabled  bool   `json:"enabled"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&users)
		for _, u := range users {
			out = append(out, staffMember{Username: u.Username, Email: u.Email, Enabled: u.Enabled, Roles: []string{role}})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
