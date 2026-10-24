-- verifications.sql — the checks beyond the NPI registry on a TPA's client
-- or a health plan's party (case-api verifications.go). Safe to re-run.

-- The HHS-OIG exclusion list (LEIE), downloaded weekly by case-api.
CREATE TABLE IF NOT EXISTS public.oig_exclusions (
    last_name     text NOT NULL DEFAULT '',
    first_name    text NOT NULL DEFAULT '',
    mid_name      text NOT NULL DEFAULT '',
    business_name text NOT NULL DEFAULT '',
    general       text NOT NULL DEFAULT '',
    specialty     text NOT NULL DEFAULT '',
    npi           text NOT NULL DEFAULT '',
    state         text NOT NULL DEFAULT '',
    excl_type     text NOT NULL DEFAULT '',
    excl_date     text NOT NULL DEFAULT '',
    name_key      text NOT NULL DEFAULT ''   -- the business (or person) name, normalized
);
CREATE INDEX IF NOT EXISTS oig_exclusions_npi ON public.oig_exclusions (npi) WHERE npi <> '';
CREATE INDEX IF NOT EXISTS oig_exclusions_name ON public.oig_exclusions (name_key);

CREATE TABLE IF NOT EXISTS public.oig_meta (
    id              int PRIMARY KEY,
    loaded_at       timestamptz NOT NULL,
    row_count       int NOT NULL,
    source_modified text
);

-- One row per (client or party, check):
--   EXCLUSION  CLEAR | NEEDS_REVIEW | CLEARED (staff: not the excluded party) | EXCLUDED | UNAVAILABLE (list not loaded)
--   TIN        NOT_CONFIGURED (form checked; no IRS matching service set up)
CREATE TABLE IF NOT EXISTS public.verifications (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    kind        text NOT NULL,          -- tpa (tpa_clients) | hp (hp_parties)
    subject_id  text NOT NULL,
    check_name  text NOT NULL,
    status      text NOT NULL,
    subject_key text NOT NULL DEFAULT '', -- what was checked (NPI|name, or the TIN)
    detail      jsonb NOT NULL DEFAULT '{}'::jsonb,
    checked_at  timestamptz NOT NULL DEFAULT now(),
    reviewed_by text,
    review_note text,
    UNIQUE (tenant, kind, subject_id, check_name)
);
CREATE INDEX IF NOT EXISTS verifications_open ON public.verifications (check_name, status)
  WHERE status IN ('NEEDS_REVIEW','UNAVAILABLE');

-- Clients and parties saved before these checks: checked in the background.
INSERT INTO public.verifications (tenant, kind, subject_id, check_name, status, subject_key, checked_at)
SELECT tenant, 'tpa', id::text, 'EXCLUSION', 'UNAVAILABLE', '', '-infinity' FROM public.tpa_clients
WHERE coalesce(npi,'') <> '' OR coalesce(tin,'') <> ''
ON CONFLICT DO NOTHING;
INSERT INTO public.verifications (tenant, kind, subject_id, check_name, status, subject_key, checked_at)
SELECT tenant, 'hp', id::text, 'EXCLUSION', 'UNAVAILABLE', '', '-infinity' FROM public.hp_parties
WHERE coalesce(npi,'') <> '' OR coalesce(tin,'') <> ''
ON CONFLICT DO NOTHING;
