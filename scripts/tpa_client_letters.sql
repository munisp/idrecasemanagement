-- ============================================================
-- A TPA client's letter of authorization: the client's permission for the TPA to act for it (tpa_client_letter.go).
-- A client has one current letter (ended_at IS NULL); a replaced or removed one stays as history.
-- Additive and safe to repeat.
-- ============================================================
CREATE TABLE IF NOT EXISTS public.tpa_client_letters (
  id           uuid PRIMARY KEY,
  tenant       text NOT NULL,
  client_id    uuid NOT NULL REFERENCES public.tpa_clients(id),
  object_key   text NOT NULL,
  filename     text NOT NULL,
  content_type text NOT NULL,
  size_bytes   bigint NOT NULL,
  uploaded_by  text NOT NULL,
  uploaded_at  timestamptz NOT NULL DEFAULT now(),
  ended_at     timestamptz,
  ended_by     text
);
CREATE UNIQUE INDEX IF NOT EXISTS tpa_client_letters_current ON public.tpa_client_letters (tenant, client_id) WHERE ended_at IS NULL;
DO $$ DECLARE o text; BEGIN
  SELECT tableowner INTO o FROM pg_tables WHERE schemaname='public' AND tablename='invoices';
  EXECUTE format('ALTER TABLE public.tpa_client_letters OWNER TO %I', o);
END $$;
