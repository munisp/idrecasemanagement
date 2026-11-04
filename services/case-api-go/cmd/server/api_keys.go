package main

// Partner API keys (scripts/api_keys.sql).
//
// A TPA or health plan connects its own software to idre with an API key: a
// Keycloak confidential client whose service account is a member of the
// organization, exactly like one of its people. The partner exchanges the
// key (client id + secret) for a short-lived access token with the OAuth2
// client-credentials grant, then calls the same /v1/tenants/{tenant}/tpa/*
// or /hp/* endpoints the portal uses. Nothing about authorization is new:
// the token carries the tenant group and the TPA / HEALTH_PLAN role, and the
// service account is in tpa_users / health_plan_users, so the access guard,
// client scope, fee rules and org audit all apply to it unchanged.
//
// Rules:
//   - TPA keys are made by the TPA's admins, health plan keys by the plan's
//     logins. A key can never make or revoke keys.
//   - A TPA key is STAFF or FINANCE (never ADMIN) and may be limited to one
//     client, like an employee login.
//   - The secret is returned once, at creation, and never stored by idre.
//   - Revoking deletes the Keycloak client (no new tokens) and the
//     membership; apiKeyGuard also refuses any token still in flight.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// apiKeyClientPrefix starts every key's Keycloak client id, so a token's
// username ("service-account-api-key-…") marks it as a key at a glance.
const apiKeyClientPrefix = "api-key-"

var apiKeySlugRe = regexp.MustCompile(`[^a-z0-9]+`)

type apiKeyOrg struct {
	kind     string // "tpa" | "plan"
	id, name string
	role     string // Keycloak realm role the key's service account gets
}

// isAPIKeyPrincipal: is this request made with a partner API key?
func isAPIKeyPrincipal(p principal) bool {
	return strings.HasPrefix(p.Username, "service-account-"+apiKeyClientPrefix)
}

// apiKeyGuard refuses revoked keys (a token issued before the revoke is still
// signed for up to its 5-minute life) and records when a key was last used.
// Only key tokens touch the database here.
func (s *server) apiKeyGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := r.Context().Value(ctxPrincipal{}).(principal)
		if !isAPIKeyPrincipal(p) {
			next.ServeHTTP(w, r)
			return
		}
		var revoked bool
		err := s.db.QueryRow(r.Context(), `
			UPDATE public.api_keys SET last_used_at = CASE
			    WHEN last_used_at IS NULL OR last_used_at < now() - interval '1 minute' THEN now() ELSE last_used_at END
			WHERE sa_user_sub=$1 RETURNING revoked_at IS NOT NULL`, p.Subject).Scan(&revoked)
		if err != nil || revoked {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this API key has been revoked"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiKeyCallerOrg is the organization whose keys the caller may manage, or an
// error message for them. kind is "tpa" or "plan" (from the route).
func (s *server) apiKeyCallerOrg(r *http.Request, tenant, kind string) (*apiKeyOrg, int, string) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if isAPIKeyPrincipal(p) {
		return nil, http.StatusForbidden, "an API key can't manage API keys; sign in to the portal"
	}
	if kind == "tpa" {
		if !hasAnyRole(p, tpaRoles...) {
			return nil, http.StatusForbidden, "forbidden"
		}
		org, err := s.tpaForPrincipal(r, tenant)
		if err != nil {
			return nil, http.StatusNotFound, err.Error()
		}
		if !s.tpaTeamAdmin(r, tenant, org.ID) {
			return nil, http.StatusForbidden, "only your organization's admins can manage API keys"
		}
		return &apiKeyOrg{kind: "tpa", id: org.ID, name: org.Name, role: "TPA"}, 0, ""
	}
	org, err := s.healthPlanForPrincipal(r, tenant)
	if err != nil {
		return nil, http.StatusNotFound, err.Error()
	}
	return &apiKeyOrg{kind: "plan", id: org.ID, name: org.Name, role: "HEALTH_PLAN"}, 0, ""
}

// GET /tpa/api-keys, GET /hp/api-keys
func (s *server) orgAPIKeysList(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		org, code, msg := s.apiKeyCallerOrg(r, tenant, kind)
		if org == nil {
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		rows, _ := s.queryRows(r, apiKeyListSQL+` WHERE k.tenant=$1 AND k.org_kind=$2 AND k.org_id=$3::uuid ORDER BY k.revoked_at IS NOT NULL, k.created_at DESC`,
			tenant, org.kind, org.id)
		writeJSON(w, http.StatusOK, map[string]any{"api_keys": rows, "token_url": s.apiTokenURL()})
	}
}

const apiKeyListSQL = `
	SELECT k.id, k.org_kind, k.org_id, k.label, k.kc_client_id AS client_id, k.team_role,
	       coalesce(k.client_id::text,'') AS limited_to_client, coalesce(cl.party_name,'') AS limited_to_client_name,
	       coalesce(k.created_by_name, k.created_by) AS created_by, k.created_at, k.last_used_at, k.revoked_at,
	       coalesce(t.name, h.name, '') AS org_name
	FROM public.api_keys k
	LEFT JOIN public.tpa_clients cl ON cl.id = k.client_id
	LEFT JOIN public.tpas t ON k.org_kind='tpa' AND t.id = k.org_id
	LEFT JOIN public.health_plans h ON k.org_kind='plan' AND h.id = k.org_id`

// apiTokenURL is where partners exchange a key for a token (this deployment's realm).
func (s *server) apiTokenURL() string {
	return strings.TrimRight(s.cfg.KeycloakIssuer, "/") + "/protocol/openid-connect/token"
}

// POST /tpa/api-keys {label, team_role?, client_id?}, POST /hp/api-keys {label}
func (s *server) orgAPIKeyCreate(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		org, code, msg := s.apiKeyCallerOrg(r, tenant, kind)
		if org == nil {
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		var in struct {
			Label    string `json:"label"`
			TeamRole string `json:"team_role"`
			ClientID string `json:"client_id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send JSON: {\"label\": \"…\"}"})
			return
		}
		in.Label = strings.TrimSpace(in.Label)
		if len(in.Label) < 2 || len(in.Label) > 80 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "give the key a name of 2 to 80 characters, e.g. \"Claims system\""})
			return
		}
		teamRole, limitClient := "", ""
		if kind == "tpa" {
			teamRole = strings.ToUpper(strings.TrimSpace(in.TeamRole))
			if teamRole == "" {
				teamRole = "STAFF"
			}
			if teamRole != "STAFF" && teamRole != "FINANCE" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "team_role must be STAFF or FINANCE (keys can't be admins)"})
				return
			}
			limitClient = strings.TrimSpace(in.ClientID)
			if limitClient != "" {
				var ok bool
				_ = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM public.tpa_clients WHERE id::text=$1 AND tenant=$2 AND tpa_id=$3::uuid)`,
					limitClient, tenant, org.id).Scan(&ok)
				if !ok {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client_id is not one of your clients"})
					return
				}
			}
		}
		var count int
		_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.api_keys WHERE tenant=$1 AND org_kind=$2 AND org_id=$3::uuid AND revoked_at IS NULL`,
			tenant, org.kind, org.id).Scan(&count)
		if count >= 20 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "your organization has 20 active keys; revoke one you no longer use first"})
			return
		}

		p := r.Context().Value(ctxPrincipal{}).(principal)
		key, err := s.provisionAPIKey(r.Context(), tenant, org, in.Label, teamRole, limitClient, p)
		if err != nil {
			slog.Error("api key create failed", "tenant", tenant, "org", org.kind+":"+org.id, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the key could not be created: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, key)
	}
}

// provisionAPIKey makes the Keycloak client + service account and links it to
// the organization. Any failure after the client exists removes it again.
func (s *server) provisionAPIKey(ctx context.Context, tenant string, org *apiKeyOrg, label, teamRole, limitClient string, by principal) (map[string]any, error) {
	tok, err := s.kcAdminToken(ctx)
	if err != nil {
		return nil, err
	}
	slug := strings.Trim(apiKeySlugRe.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(slug) > 24 {
		slug = strings.Trim(slug[:24], "-")
	}
	if slug == "" {
		slug = "key"
	}
	rnd := make([]byte, 4)
	_, _ = rand.Read(rnd)
	clientID := apiKeyClientPrefix + slug + "-" + hex.EncodeToString(rnd)

	body, _ := json.Marshal(map[string]any{
		"clientId":                  clientID,
		"name":                      label + " — " + org.name,
		"description":               fmt.Sprintf("idre API key for %s %s (%s), created by %s", org.kind, org.name, tenant, displayName(by)),
		"enabled":                   true,
		"protocol":                  "openid-connect",
		"publicClient":              false,
		"serviceAccountsEnabled":    true,
		"standardFlowEnabled":       false,
		"implicitFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		"protocolMappers": []map[string]any{{
			"name": "groups", "protocol": "openid-connect", "protocolMapper": "oidc-group-membership-mapper",
			"config": map[string]string{"full.path": "true", "claim.name": "groups", "access.token.claim": "true", "id.token.claim": "false", "userinfo.token.claim": "false"},
		}},
	})
	resp, err := s.kcDo(ctx, http.MethodPost, "/admin/realms/idre/clients", tok, body)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("sign-in service refused the key (status %d)", resp.StatusCode)
	}
	parts := strings.Split(resp.Header.Get("Location"), "/")
	clientUUID := parts[len(parts)-1]
	if clientUUID == "" {
		return nil, fmt.Errorf("sign-in service did not return the key's id")
	}
	undo := func(cause error) (map[string]any, error) {
		if dr, derr := s.kcDo(context.WithoutCancel(ctx), http.MethodDelete, "/admin/realms/idre/clients/"+url.PathEscape(clientUUID), tok, nil); derr == nil {
			dr.Body.Close()
		} else {
			slog.Error("api key: half-made client left in keycloak", "client", clientID, "err", derr)
		}
		return nil, cause
	}

	var secret struct {
		Value string `json:"value"`
	}
	if err := s.kcGetJSON(ctx, tok, "/admin/realms/idre/clients/"+url.PathEscape(clientUUID)+"/client-secret", &secret); err != nil || secret.Value == "" {
		return undo(fmt.Errorf("could not read the key's secret"))
	}
	var sa struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := s.kcGetJSON(ctx, tok, "/admin/realms/idre/clients/"+url.PathEscape(clientUUID)+"/service-account-user", &sa); err != nil || sa.ID == "" {
		return undo(fmt.Errorf("could not read the key's service account"))
	}
	if err := s.kcAddUserToTenantGroup(ctx, tok, sa.ID, tenant); err != nil {
		return undo(err)
	}
	if err := s.kcAssignRealmRole(ctx, tok, sa.ID, org.role); err != nil {
		return undo(err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return undo(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if org.kind == "tpa" {
		_, err = tx.Exec(ctx, `
			INSERT INTO public.tpa_users (tenant, user_sub, tpa_id, team_role, full_name, email, username, added_by, client_id)
			VALUES ($1,$2,$3::uuid,$4,$5,'',$6,$7,nullif($8,'')::uuid)`,
			tenant, sa.ID, org.id, teamRole, "API key: "+label, sa.Username, by.Subject, limitClient)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO public.health_plan_users (tenant, user_sub, plan_id) VALUES ($1,$2,$3::uuid)`, tenant, sa.ID, org.id)
	}
	if err != nil {
		return undo(fmt.Errorf("could not link the key to your organization"))
	}
	var id string
	var created time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO public.api_keys (tenant, org_kind, org_id, label, kc_client_id, kc_client_uuid, sa_user_sub, team_role, client_id, created_by, created_by_name)
		VALUES ($1,$2,$3::uuid,$4,$5,$6,$7,nullif($8,''),nullif($9,'')::uuid,$10,$11) RETURNING id, created_at`,
		tenant, org.kind, org.id, label, clientID, clientUUID, sa.ID, teamRole, limitClient, by.Subject, displayName(by)).Scan(&id, &created)
	if err != nil {
		return undo(fmt.Errorf("could not record the key"))
	}
	if err := tx.Commit(ctx); err != nil {
		return undo(err)
	}
	return map[string]any{
		"id": id, "label": label, "client_id": clientID,
		"client_secret": secret.Value, // shown once; idre does not keep it
		"token_url":     s.apiTokenURL(),
		"api_base":      "/v1/tenants/" + tenant + map[string]string{"tpa": "/tpa", "plan": "/hp"}[org.kind],
		"team_role":     teamRole, "limited_to_client": limitClient, "created_at": created,
		"note": "Store the secret now: it is shown only once. Exchange it for a token with grant_type=client_credentials.",
	}, nil
}

// DELETE /tpa/api-keys/{keyId}, DELETE /hp/api-keys/{keyId}
func (s *server) orgAPIKeyRevoke(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		org, code, msg := s.apiKeyCallerOrg(r, tenant, kind)
		if org == nil {
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		s.revokeAPIKey(w, r, tenant, chi.URLParam(r, "keyId"), org)
	}
}

// GET /api-keys -- every key in the tenant, for staff (tenant and platform admins).
func (s *server) staffAPIKeysList(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TENANT_ADMIN", "PLATFORM_ADMIN", "FEDERAL_ADMIN") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "tenant admins only"})
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, _ := s.queryRows(r, apiKeyListSQL+` WHERE k.tenant=$1 ORDER BY k.revoked_at IS NOT NULL, k.created_at DESC`, tenant)
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": rows, "token_url": s.apiTokenURL()})
}

// DELETE /api-keys/{keyId} -- staff revoke (tenant and platform admins).
func (s *server) staffAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TENANT_ADMIN", "PLATFORM_ADMIN", "FEDERAL_ADMIN") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "tenant admins only"})
		return
	}
	s.revokeAPIKey(w, r, r.Context().Value(ctxTenant{}).(string), chi.URLParam(r, "keyId"), nil)
}

// revokeAPIKey: org nil = staff (any key in the tenant); otherwise only that org's keys.
func (s *server) revokeAPIKey(w http.ResponseWriter, r *http.Request, tenant, keyID string, org *apiKeyOrg) {
	ctx := r.Context()
	var kind, orgID, clientUUID, sub, label, clientID string
	var revoked bool
	err := s.db.QueryRow(ctx, `
		SELECT org_kind, org_id::text, kc_client_uuid, sa_user_sub, label, kc_client_id, revoked_at IS NOT NULL
		FROM public.api_keys WHERE id::text=$1 AND tenant=$2`, keyID, tenant).
		Scan(&kind, &orgID, &clientUUID, &sub, &label, &clientID, &revoked)
	if err != nil || (org != nil && (org.kind != kind || org.id != orgID)) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such API key"})
		return
	}
	if revoked {
		writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "already": true})
		return
	}
	tok, err := s.kcAdminToken(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the sign-in service is unavailable; try again"})
		return
	}
	resp, err := s.kcDo(ctx, http.MethodDelete, "/admin/realms/idre/clients/"+url.PathEscape(clientUUID), tok, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the sign-in service is unavailable; try again"})
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("the sign-in service refused (status %d)", resp.StatusCode)})
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if kind == "tpa" {
		_, _ = s.db.Exec(ctx, `DELETE FROM public.tpa_users WHERE tenant=$1 AND user_sub=$2`, tenant, sub)
	} else {
		_, _ = s.db.Exec(ctx, `DELETE FROM public.health_plan_users WHERE tenant=$1 AND user_sub=$2`, tenant, sub)
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.api_keys SET revoked_at=now(), revoked_by=$3 WHERE id::text=$1 AND tenant=$2`, keyID, tenant, displayName(p))
	slog.Info("api key revoked", "tenant", tenant, "org", kind+":"+orgID, "key", label, "client_id", clientID, "by", displayName(p))
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// kcGetJSON GETs an admin path and decodes a 200 body into out.
func (s *server) kcGetJSON(ctx context.Context, tok, path string, out any) error {
	resp, err := s.kcDo(ctx, http.MethodGet, path, tok, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// kcAddUserToTenantGroup puts a user in the "tenant/<tenant>" group (its path
// becomes the token's groups claim "/tenant/<tenant>").
func (s *server) kcAddUserToTenantGroup(ctx context.Context, tok, userID, tenant string) error {
	name := "tenant/" + tenant
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := s.kcGetJSON(ctx, tok, "/admin/realms/idre/groups?briefRepresentation=true&exact=true&search="+url.QueryEscape(name), &groups); err != nil {
		return fmt.Errorf("could not find the %s group: %v", tenant, err)
	}
	gid := ""
	for _, g := range groups {
		if g.Name == name {
			gid = g.ID
		}
	}
	if gid == "" {
		return fmt.Errorf("the %s group does not exist", tenant)
	}
	resp, err := s.kcDo(ctx, http.MethodPut, "/admin/realms/idre/users/"+url.PathEscape(userID)+"/groups/"+url.PathEscape(gid), tok, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("group join: status %d", resp.StatusCode)
	}
	return nil
}
