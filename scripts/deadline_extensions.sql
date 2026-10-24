-- Deadline extensions granted by the PM: one row per extension of one of a case's program clocks (INITIAL_REVIEW,
-- PLAN_RESPONSE, ...). The clock's due date (and its hard limit, if any) moves by the sum of its extensions; the
-- case's workflow is told too, so its follow-ups and breach wait for the new date. Append-only. Safe to re-run.
CREATE TABLE IF NOT EXISTS public.case_deadline_extensions (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant      text NOT NULL,
  case_id     text NOT NULL,
  clock       text NOT NULL,
  days        int  NOT NULL CHECK (days BETWEEN 1 AND 90),
  reason      text NOT NULL CHECK (length(btrim(reason)) >= 10),
  granted_by  text NOT NULL,
  workflow_signalled boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS case_deadline_extensions_case_idx ON public.case_deadline_extensions (tenant, case_id, clock);
