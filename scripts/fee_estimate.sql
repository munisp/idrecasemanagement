-- Change log 2026-11-25 #4: the full review fee estimate shown at filing, set by an administrator in the portal
-- (case-api fee_estimate.go). Off until it is switched on. Every change is kept in the history.
CREATE TABLE IF NOT EXISTS public.fee_estimate_settings (
  tenant     text PRIMARY KEY,
  config     jsonb NOT NULL DEFAULT '{}'::jsonb,
  updated_by text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS public.fee_estimate_settings_history (
  id          bigserial PRIMARY KEY,
  tenant      text NOT NULL,
  changed_by  text NOT NULL,
  changed_sub text NOT NULL DEFAULT '',
  changed_at  timestamptz NOT NULL DEFAULT now(),
  old_config  jsonb,
  new_config  jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS fee_estimate_settings_history_tenant ON public.fee_estimate_settings_history (tenant, changed_at DESC);
DO $$ DECLARE o text; BEGIN
  SELECT tableowner INTO o FROM pg_tables WHERE schemaname='public' AND tablename='invoices';
  EXECUTE format('ALTER TABLE public.fee_estimate_settings OWNER TO %I', o);
  EXECUTE format('ALTER TABLE public.fee_estimate_settings_history OWNER TO %I', o);
  EXECUTE format('ALTER SEQUENCE public.fee_estimate_settings_history_id_seq OWNER TO %I', o);
END $$;
