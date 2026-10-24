// Author: Patrick Munis at newwave
// auth.js — Keycloak OIDC with PKCE (no library, no secrets in the SPA).
const Auth = (() => {
  const cfg = window.IDRE_CONFIG;
  const base = `${cfg.keycloakUrl}/realms/${cfg.realm}/protocol/openid-connect`;
  const K = { token: "idre.token", refresh: "idre.refresh", verifier: "idre.verifier", returnHash: "idre.returnHash", idToken: "idre.id_token" };

  const b64url = (buf) =>
    btoa(String.fromCharCode(...new Uint8Array(buf)))
      .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

  async function login() {
    // Fragments never reach the server, so redirect_uri can't carry the
    // deep-linked route (#/cases/abc, #/qa, ...) through the Keycloak round
    // trip -- save it ourselves and restore it in handleCallback, otherwise
    // every login always lands back on the bare path with no hash.
    if (location.hash) sessionStorage.setItem(K.returnHash, location.hash);
    const verifier = b64url(crypto.getRandomValues(new Uint8Array(32)));
    sessionStorage.setItem(K.verifier, verifier);
    const challenge = b64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)));
    const params = new URLSearchParams({
      client_id: cfg.clientId, response_type: "code",
      redirect_uri: location.origin + "/app.html", scope: "openid profile",
      code_challenge: challenge, code_challenge_method: "S256",
    });
    location.href = `${base}/auth?${params}`;
  }

  async function handleCallback() {
    const code = new URLSearchParams(location.search).get("code");
    if (!code) return false;
    const resp = await fetch(`${base}/token`, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "authorization_code", client_id: cfg.clientId, code,
        redirect_uri: location.origin + "/app.html",
        code_verifier: sessionStorage.getItem(K.verifier),
      }),
    });
    if (!resp.ok) return false;
    const tok = await resp.json();
    sessionStorage.setItem(K.token, tok.access_token);
    sessionStorage.setItem(K.refresh, tok.refresh_token);
    if (tok.id_token) sessionStorage.setItem(K.idToken, tok.id_token); // Keycloak's sign-out needs it to end the session without asking
    const returnHash = sessionStorage.getItem(K.returnHash) || "";
    sessionStorage.removeItem(K.returnHash);
    history.replaceState(null, "", location.pathname + returnHash);
    return true;
  }

  async function token() {
    const tok = sessionStorage.getItem(K.token);
    if (!tok) return null;
    const { exp } = JSON.parse(atob(tok.split(".")[1]));
    if (exp * 1000 > Date.now() + 30000) return tok;
    // refresh
    const resp = await fetch(`${base}/token`, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "refresh_token", client_id: cfg.clientId,
        refresh_token: sessionStorage.getItem(K.refresh),
      }),
    });
    if (!resp.ok) { logout(); return null; }
    const fresh = await resp.json();
    sessionStorage.setItem(K.token, fresh.access_token);
    sessionStorage.setItem(K.refresh, fresh.refresh_token);
    if (fresh.id_token) sessionStorage.setItem(K.idToken, fresh.id_token);
    return fresh.access_token;
  }

  function claims() {
    const tok = sessionStorage.getItem(K.token);
    if (!tok) return null;
    const p = JSON.parse(atob(tok.split(".")[1]));
    const roles = (p.realm_access && p.realm_access.roles) || [];
    // Per-tenant super admin: mirrors the backend's hasRole quirk
    // (cmd/server/onboarding.go) where every check for CASE_MANAGER also
    // admits TENANT_ADMIN. Every frontend role check reads from this one
    // function (app.js's sidebar `has()`, views.js's `can()`), so
    // synthesizing CASE_MANAGER here once is equivalent to adding
    // TENANT_ADMIN to every one of those call sites individually.
    if (roles.includes("TENANT_ADMIN") && !roles.includes("CASE_MANAGER")) roles.push("CASE_MANAGER");
    return {
      sub: p.sub,
      name: p.preferred_username || p.sub,
      roles,
      tenants: (p.groups || []).filter((g) => g.startsWith("/tenant/")).map((g) => g.slice(8)),
    };
  }

  // The one role check every screen uses. TENANT_ADMIN is the tenant's own
  // super-admin: it passes any check that admits a tenant staff role. It
  // does not pass platform-only checks (FEDERAL_ADMIN/PLATFORM_ADMIN alone)
  // or identity checks on external roles like can("TPA"). Mirrors the
  // backend's hasAnyRole (cmd/server/documents.go) exactly.
  const STAFF_ROLES = ["CASE_MANAGER", "PM", "FINANCE", "ARBITRATOR", "ATTORNEY", "CODER",
    "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "TENANT_ADMIN"];
  function can(...rs) {
    // Auth.claims, not the local claims(): demo mode swaps Auth.claims out.
    const roles = Auth.claims()?.roles || [];
    return rs.some((r) => roles.includes(r)) ||
      (roles.includes("TENANT_ADMIN") && rs.some((r) => STAFF_ROLES.includes(r)));
  }

  // A Reviewer with nothing that sees every case: they see only unassigned cases and their own. Mirrors the
  // backend's reviewerScoped (cmd/server/casescope.go) — exact roles, so a Tenant Admin is never scoped.
  const SEE_ALL_ROLES = ["PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR",
    "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "ARBITRATOR", "FINANCE"];
  function reviewerScoped() {
    const roles = Auth.claims()?.roles || [];
    return roles.includes("CASE_MANAGER") && !SEE_ALL_ROLES.some((r) => roles.includes(r));
  }

  // Sign out of Keycloak too. With the ID token as id_token_hint the session ends at once; without it Keycloak asks
  // "Do you want to log out?" and, if that page is left, the session stays and the portal signs straight back in.
  function logout() {
    const hint = sessionStorage.getItem(K.idToken);
    sessionStorage.clear();
    location.href = `${base}/logout?client_id=${cfg.clientId}&post_logout_redirect_uri=${encodeURIComponent(location.origin + "/app.html")}`
      + (hint ? `&id_token_hint=${encodeURIComponent(hint)}` : "");
  }

  return { login, logout, handleCallback, token, claims, can, reviewerScoped };
})();
