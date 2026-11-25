-- Outbound mail the provider turned away for now (rate limit, daily quota, unreachable): kept and sent again by
-- case-api's mail sweeper (mailqueue.go) until it is accepted or three days have passed. Safe to run more than once.
CREATE TABLE IF NOT EXISTS public.mail_queue (
  id              bigserial PRIMARY KEY,
  from_addr       text NOT NULL,
  to_addrs        jsonb NOT NULL,
  cc_addrs        jsonb NOT NULL DEFAULT '[]',
  subject         text NOT NULL DEFAULT '',
  message         bytea NOT NULL,
  attempts        int NOT NULL DEFAULT 1,
  last_error      text NOT NULL DEFAULT '',
  next_attempt_at timestamptz NOT NULL,
  sent_at         timestamptz,
  given_up_at     timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS mail_queue_due_idx ON public.mail_queue (next_attempt_at) WHERE sent_at IS NULL AND given_up_at IS NULL;
DO $own$
DECLARE app text;
BEGIN
  SELECT tableowner INTO app FROM pg_tables WHERE schemaname = 'public' AND tablename = 'invoices';
  IF app IS NOT NULL THEN
    EXECUTE format('ALTER TABLE public.mail_queue OWNER TO %I', app);
    EXECUTE format('ALTER SEQUENCE public.mail_queue_id_seq OWNER TO %I', app);
  END IF;
END
$own$;
