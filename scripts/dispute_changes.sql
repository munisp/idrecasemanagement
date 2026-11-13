-- ============================================================
-- A dispute's filed details, changed after filing (tpa_filing.go)
--
-- dispute_changes: every change to a dispute's filing (its packet details, amount and claim rows), whoever made it:
-- the filer's own edit while the fee is unpaid, a change request and its decision once paid, a staff correction.
-- Each row says who, when, how (kind) and exactly what changed (field: from -> to). It is what the dispute's change
-- history shows the filer's team and the office (never the other party); the same entry also goes on the audit
-- chain (public.audit_log), which stays the tamper-evident record.
--
-- dispute_change_requests: a filer's change asked for after the fee is paid, waiting for a case manager to approve
-- (applied then) or reject (with a note). Additive and safe to repeat.
-- ============================================================
CREATE TABLE IF NOT EXISTS public.dispute_changes (
  id          bigserial PRIMARY KEY,
  tenant      text NOT NULL,
  case_id     text NOT NULL,
  kind        text NOT NULL,          -- EDITED (filer, before payment) | REQUESTED | APPROVED | REJECTED | WITHDRAWN | CORRECTED (staff)
  by_sub      text NOT NULL,
  by_name     text NOT NULL DEFAULT '',
  by_side     text NOT NULL,          -- FILER | OFFICE
  changes     jsonb NOT NULL DEFAULT '[]',  -- [{"field","from","to"}] ; claim rows as "claim <number>: <column>"
  reason      text NOT NULL DEFAULT '',
  request_id  uuid,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS dispute_changes_case ON public.dispute_changes (tenant, case_id, created_at DESC);

CREATE TABLE IF NOT EXISTS public.dispute_change_requests (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant         text NOT NULL,
  case_id        text NOT NULL,
  tpa_id         uuid,
  requested_by   text NOT NULL,
  requested_name text NOT NULL DEFAULT '',
  packet         jsonb NOT NULL,      -- the whole form as the filer wants it
  changes        jsonb NOT NULL,      -- what it would change, against the dispute when it was asked
  reason         text NOT NULL,
  status         text NOT NULL DEFAULT 'PENDING',  -- PENDING | APPROVED | REJECTED | WITHDRAWN
  decided_by     text,
  decided_name   text,
  decided_at     timestamptz,
  decision_note  text NOT NULL DEFAULT '',
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS dispute_change_requests_case ON public.dispute_change_requests (tenant, case_id, created_at DESC);
CREATE INDEX IF NOT EXISTS dispute_change_requests_open ON public.dispute_change_requests (tenant, created_at) WHERE status = 'PENDING';
-- One open request per dispute at a time: a second one waits for the first to be decided or withdrawn.
CREATE UNIQUE INDEX IF NOT EXISTS dispute_change_requests_one_open ON public.dispute_change_requests (tenant, case_id) WHERE status = 'PENDING';

-- The application's own role must own them (idre on dev, idre_prod on prod): the owner of public.invoices.
DO $$ DECLARE o text; BEGIN
  SELECT tableowner INTO o FROM pg_tables WHERE schemaname='public' AND tablename='invoices';
  EXECUTE format('ALTER TABLE public.dispute_changes OWNER TO %I', o);
  EXECUTE format('ALTER TABLE public.dispute_change_requests OWNER TO %I', o);
  EXECUTE format('ALTER SEQUENCE public.dispute_changes_id_seq OWNER TO %I', o);
END $$;
