-- ============================================================
-- A TPA's staff authorization letter (2026-11-25): on its letterhead, signed by an officer, naming the staff who
-- may act for the TPA on Florida claim disputes -- e.g. "I, <name>, COO at <TPA>, certify that the following
-- individuals can act on behalf of <TPA> regarding Florida Claim Disputes: ...". One current letter per TPA
-- (ended_at IS NULL); a replaced one stays as history. It goes onto every dispute the TPA files (tpa_staff_letter.go).
-- Additive and safe to repeat.
-- ============================================================
CREATE TABLE IF NOT EXISTS public.tpa_staff_letters (
  id           uuid PRIMARY KEY,
  tenant       text NOT NULL,
  tpa_id       uuid NOT NULL REFERENCES public.tpas(id),
  object_key   text NOT NULL,
  filename     text NOT NULL,
  content_type text NOT NULL,
  size_bytes   bigint NOT NULL,
  uploaded_by  text NOT NULL,
  uploaded_at  timestamptz NOT NULL DEFAULT now(),
  ended_at     timestamptz,
  ended_by     text
);
CREATE UNIQUE INDEX IF NOT EXISTS tpa_staff_letters_current ON public.tpa_staff_letters (tenant, tpa_id) WHERE ended_at IS NULL;
DO $$ DECLARE o text; BEGIN
  SELECT tableowner INTO o FROM pg_tables WHERE schemaname='public' AND tablename='invoices';
  EXECUTE format('ALTER TABLE public.tpa_staff_letters OWNER TO %I', o);
END $$;
