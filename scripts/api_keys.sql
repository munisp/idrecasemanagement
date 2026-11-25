-- ============================================================
-- Partner API keys (api_keys.go)
--
-- A key is a Keycloak confidential client with a service account. Its
-- service-account user is linked to the TPA (tpa_users) or health plan
-- (health_plan_users) exactly like a person, so every existing scope and
-- audit rule applies to it. This table records which logins are keys, who
-- made them and when they were revoked. The secret is never stored here.
-- Additive and safe to repeat. Apply BEFORE the case-api that reads it.
-- ============================================================

CREATE TABLE IF NOT EXISTS public.api_keys (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant        text NOT NULL,
  org_kind      text NOT NULL CHECK (org_kind IN ('tpa', 'plan')),
  org_id        uuid NOT NULL,
  label         text NOT NULL,
  kc_client_id  text NOT NULL UNIQUE,   -- what the partner sends as client_id
  kc_client_uuid text NOT NULL,         -- Keycloak's internal id, for revoking
  sa_user_sub   text NOT NULL UNIQUE,   -- the token's sub
  team_role     text,                   -- TPA keys: STAFF | FINANCE
  client_id     uuid,                   -- TPA keys limited to one client
  created_by    text NOT NULL,
  created_by_name text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz,
  revoked_at    timestamptz,
  revoked_by    text
);
CREATE INDEX IF NOT EXISTS api_keys_org ON public.api_keys (tenant, org_kind, org_id);
