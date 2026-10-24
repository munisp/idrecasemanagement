-- tpa_team_groups.sql — a TPA's own staff logins, and provider groups between the TPA and its providers
-- (TPA -> provider groups -> providers). Idempotent.

-- Each login linked to a TPA is one of its employees. ADMIN manages the team; STAFF files and pays. Logins linked
-- before this (the registrant, claim-code teammates) are admins, as they could already add teammates.
ALTER TABLE public.tpa_users
  ADD COLUMN IF NOT EXISTS team_role text NOT NULL DEFAULT 'ADMIN' CHECK (team_role IN ('ADMIN', 'STAFF')),
  ADD COLUMN IF NOT EXISTS full_name text,
  ADD COLUMN IF NOT EXISTS email     text,
  ADD COLUMN IF NOT EXISTS username  text,
  ADD COLUMN IF NOT EXISTS added_by  text;

-- A provider group: a practice or health system the TPA acts for, holding several providers.
CREATE TABLE IF NOT EXISTS public.tpa_provider_groups (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant        text NOT NULL,
  tpa_id        uuid NOT NULL REFERENCES public.tpas(id) ON DELETE CASCADE,
  name          text NOT NULL,
  tin           text,
  npi           text,
  contact_email text,
  status        text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'SUSPENDED')),
  created_by    text,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS tpa_provider_groups_name ON public.tpa_provider_groups (tenant, tpa_id, lower(name));

-- A provider belongs to at most one group; none = filed for directly under the TPA.
ALTER TABLE public.tpa_clients ADD COLUMN IF NOT EXISTS group_id uuid REFERENCES public.tpa_provider_groups(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS tpa_clients_group ON public.tpa_clients (group_id);
