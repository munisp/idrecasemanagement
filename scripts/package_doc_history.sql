-- Every file a document-set slot held before the one it holds now: kept when the TPA replaces or removes it, with
-- its verdict and reasons, who uploaded it and who replaced it. A slot's current file stays in tpa_package_docs;
-- this is the history behind it ("Earlier versions" on the TPA's document set). Safe to run more than once.
CREATE TABLE IF NOT EXISTS public.package_doc_history (
  id          bigserial PRIMARY KEY,
  tenant      text NOT NULL,
  kind        text NOT NULL DEFAULT 'tpa' CHECK (kind IN ('tpa', 'hp')),
  package_id  uuid NOT NULL,
  slot        text NOT NULL,
  filename    text NOT NULL DEFAULT '',
  object_key  text NOT NULL DEFAULT '',
  status      text NOT NULL DEFAULT '',
  reasons     jsonb NOT NULL DEFAULT '[]',
  uploaded_by text NOT NULL DEFAULT '',
  uploaded_at timestamptz,
  ended_at    timestamptz NOT NULL DEFAULT now(),
  ended_by    text NOT NULL DEFAULT '',
  ended_how   text NOT NULL CHECK (ended_how IN ('REPLACED', 'REMOVED'))
);
CREATE INDEX IF NOT EXISTS package_doc_history_slot_idx ON public.package_doc_history (package_id, slot, ended_at DESC);

-- Who uploaded the slot's current file, and when (created_at stays the slot's first upload).
ALTER TABLE public.tpa_package_docs ADD COLUMN IF NOT EXISTS uploaded_by text NOT NULL DEFAULT '';
ALTER TABLE public.tpa_package_docs ADD COLUMN IF NOT EXISTS uploaded_at timestamptz;

DO $own$
DECLARE app text;
BEGIN
  SELECT tableowner INTO app FROM pg_tables WHERE schemaname = 'public' AND tablename = 'invoices';
  IF app IS NOT NULL THEN
    EXECUTE format('ALTER TABLE public.package_doc_history OWNER TO %I', app);
    EXECUTE format('ALTER SEQUENCE public.package_doc_history_id_seq OWNER TO %I', app);
  END IF;
END
$own$;
