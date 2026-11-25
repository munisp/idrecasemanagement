-- ============================================================
-- Save as you go: a signed-in user's unfinished dispute form (drafts.go)
--
-- One row per login and form. Holds what was typed, which can include patient details, so it is deleted when the
-- dispute is filed or the draft discarded, and purged after 90 days unused. Uploaded documents are not here: they are
-- already stored in their package, and the draft names it. Additive and safe to repeat.
-- ============================================================
CREATE TABLE IF NOT EXISTS public.filing_drafts (
  tenant     text NOT NULL,
  user_sub   text NOT NULL,
  form       text NOT NULL,
  data       jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, user_sub, form)
);
CREATE INDEX IF NOT EXISTS filing_drafts_updated ON public.filing_drafts (updated_at);
-- The application's own role must own it (idre on dev, idre_prod on prod): the owner of public.invoices.
DO $$ DECLARE o text; BEGIN
  SELECT tableowner INTO o FROM pg_tables WHERE schemaname='public' AND tablename='invoices';
  EXECUTE format('ALTER TABLE public.filing_drafts OWNER TO %I', o);
END $$;

-- Several drafts per login and form (2026-11-12). Each gets its own id, a short reference to quote (D-4F2A9C) and a
-- summary for the drafts list (client, provider, claim, amount; never patient details). The one-per-form key becomes
-- an ordinary index. Safe to repeat. Apply BEFORE the case-api that writes `ref`.
ALTER TABLE public.filing_drafts ADD COLUMN IF NOT EXISTS id uuid NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE public.filing_drafts ADD COLUMN IF NOT EXISTS ref text;
ALTER TABLE public.filing_drafts ADD COLUMN IF NOT EXISTS summary jsonb NOT NULL DEFAULT '{}';
ALTER TABLE public.filing_drafts ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
UPDATE public.filing_drafts SET ref = 'D-' || upper(substr(replace(id::text, '-', ''), 1, 6)) WHERE ref IS NULL;
ALTER TABLE public.filing_drafts ALTER COLUMN ref SET NOT NULL;
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'public.filing_drafts'::regclass AND contype = 'p'
             AND pg_get_constraintdef(oid) = 'PRIMARY KEY (tenant, user_sub, form)') THEN
    ALTER TABLE public.filing_drafts DROP CONSTRAINT filing_drafts_pkey;
    ALTER TABLE public.filing_drafts ADD PRIMARY KEY (id);
  END IF;
END $$;
CREATE INDEX IF NOT EXISTS filing_drafts_owner ON public.filing_drafts (tenant, user_sub, form, updated_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS filing_drafts_ref ON public.filing_drafts (tenant, ref);
