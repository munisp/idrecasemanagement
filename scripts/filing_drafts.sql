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
