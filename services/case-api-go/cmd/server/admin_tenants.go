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
	"NURSE_PHYSICIAN": true, "ATTORNEY": true, "FINANCE": true,
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
			http.Error(w, `{"error":"first_user.role must be one of CASE_MANAGER, ARBITRATOR, PM, CODER, NURSE_PHYSICIAN, ATTORNEY, FINANCE, STATE_AUDITOR, PARTY"}`, http.StatusBadRequest)
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
	groupBody, _ := json.Marshal(map[string]string{"name": "tenant/" + in.Tenant})
	groupResp, err := s.kcDo(r.Context(), http.MethodPost, "/admin/realms/idre/groups", tok, groupBody)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "keycloak group create failed: " + err.Error()})
		return
	}
	defer groupResp.Body.Close()
	if groupResp.StatusCode != http.StatusCreated && groupResp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(io.LimitReader(groupResp.Body, 2048))
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("keycloak group create: status %d: %s", groupResp.StatusCode, strings.TrimSpace(string(body)))})
		return
	}

	out := map[string]any{
		"tenant": in.Tenant, "group": groupPath,
		"group_already_existed": groupResp.StatusCode == http.StatusConflict,
	}

	if in.FirstUser != nil {
		tempPassword, err := generateTempPassword()
		if err != nil {
			out["first_user_error"] = "password generation failed: " + err.Error()
			writeJSON(w, http.StatusCreated, out)
			return
		}
		userBody, _ := json.Marshal(map[string]any{
			"username": in.FirstUser.Username, "email": in.FirstUser.Email,
			"enabled": true, "emailVerified": false,
			"groups":     []string{groupPath},
			"realmRoles": []string{in.FirstUser.Role},
			"credentials": []map[string]any{
				{"type": "password", "value": tempPassword, "temporary": true},
			},
		})
		userResp, err := s.kcDo(r.Context(), http.MethodPost, "/admin/realms/idre/users", tok, userBody)
		if err != nil {
			out["first_user_error"] = "keycloak user create failed: " + err.Error()
		} else {
			defer userResp.Body.Close()
			switch userResp.StatusCode {
			case http.StatusCreated:
				out["first_user"] = map[string]any{
					"username": in.FirstUser.Username, "role": in.FirstUser.Role,
					"temporary_password": tempPassword, // one-time: share out-of-band, never logged
				}
			case http.StatusConflict:
				out["first_user_error"] = "a user with this username or email already exists -- add them to " + groupPath + " manually"
			default:
				body, _ := io.ReadAll(io.LimitReader(userResp.Body, 2048))
				out["first_user_error"] = fmt.Sprintf("keycloak user create: status %d: %s", userResp.StatusCode, strings.TrimSpace(string(body)))
			}
		}
	}

	writeJSON(w, http.StatusCreated, out)
}
