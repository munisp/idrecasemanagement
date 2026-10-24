-- Author: Patrick Munis at newwave
-- npi_registry.sql — a TPA client's and a health plan party's NPI are checked
-- against the national registry (NPPES), and the answer kept. See case-api npi_registry.go.
-- Safe to re-run.

-- What NPPES holds for each NPI checked (the cache existed for onboarding).
ALTER TABLE public.npi_cache ADD COLUMN IF NOT EXISTS registry_name text;
ALTER TABLE public.npi_cache ADD COLUMN IF NOT EXISTS other_names jsonb;
ALTER TABLE public.npi_cache ADD COLUMN IF NOT EXISTS enumeration_type text;
ALTER TABLE public.npi_cache ADD COLUMN IF NOT EXISTS taxonomy text;
ALTER TABLE public.npi_cache ADD COLUMN IF NOT EXISTS state text;

-- The decision on each client's NPI:
--   VERIFIED      the registry holds it under this client's name (or staff confirmed it)
--   NEEDS_REVIEW  the registry holds it under another name, or it was deactivated later
--   REJECTED      staff rejected it; the TPA must enter the right one
--   UNVERIFIED    the registry could not be reached; checked again in the background
ALTER TABLE public.tpa_clients ADD COLUMN IF NOT EXISTS npi_status text;
ALTER TABLE public.tpa_clients ADD COLUMN IF NOT EXISTS npi_registry_name text;
ALTER TABLE public.tpa_clients ADD COLUMN IF NOT EXISTS npi_checked_at timestamptz;
ALTER TABLE public.tpa_clients ADD COLUMN IF NOT EXISTS npi_reviewed_by text;
ALTER TABLE public.tpa_clients ADD COLUMN IF NOT EXISTS npi_review_note text;

ALTER TABLE public.hp_parties ADD COLUMN IF NOT EXISTS npi_status text;
ALTER TABLE public.hp_parties ADD COLUMN IF NOT EXISTS npi_registry_name text;
ALTER TABLE public.hp_parties ADD COLUMN IF NOT EXISTS npi_checked_at timestamptz;
ALTER TABLE public.hp_parties ADD COLUMN IF NOT EXISTS npi_reviewed_by text;
ALTER TABLE public.hp_parties ADD COLUMN IF NOT EXISTS npi_review_note text;

-- Clients and parties saved before this check: verified in the background.
UPDATE public.hp_parties SET npi_status='UNVERIFIED'
WHERE coalesce(npi,'') <> '' AND npi_status IS NULL;
UPDATE public.tpa_clients SET npi_status='UNVERIFIED'
WHERE coalesce(npi,'') <> '' AND npi_status IS NULL;

CREATE INDEX IF NOT EXISTS tpa_clients_npi_status ON public.tpa_clients (npi_status)
  WHERE npi_status IN ('UNVERIFIED','NEEDS_REVIEW');
CREATE INDEX IF NOT EXISTS hp_parties_npi_status ON public.hp_parties (npi_status)
  WHERE npi_status IN ('UNVERIFIED','NEEDS_REVIEW');
