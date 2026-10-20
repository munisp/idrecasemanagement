-- The specialists on a case, alongside its Reviewer (case-api helpers.go).
--
-- A case belongs to its Reviewer (cases.assigned_to). The Coder, the Clinical reviewer (NURSE_PHYSICIAN), the
-- Attorney and the Arbitrator are named here, one person per role, without taking the case off the Reviewer: by the
-- PM, or in turn when the case reaches their step.
--
-- Cases that were handed to a specialist as their holder (before this) move here: the specialist keeps their place,
-- and the Reviewer's place is empty again, for the PM or the Team page's "Assign them now".
--
-- Safe to run more than once.
CREATE TABLE IF NOT EXISTS public.case_helpers (
  tenant      text NOT NULL,
  case_id     text NOT NULL,
  role        text NOT NULL,
  user_id     text NOT NULL,
  assigned_by text,
  assigned_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, case_id, role)
);
CREATE INDEX IF NOT EXISTS case_helpers_user_ix ON public.case_helpers (tenant, user_id);

DO $$
DECLARE t text;
BEGIN
  FOR t IN SELECT substr(table_schema, 8) FROM information_schema.tables
           WHERE table_schema LIKE 'tenant\_%' AND table_name = 'cases' LOOP
    EXECUTE format($q$
      INSERT INTO public.case_helpers (tenant, case_id, role, user_id, assigned_by, assigned_at)
      SELECT %L, id::text, assigned_role, assigned_to, 'moved from the case holder', coalesce(updated_at, now())
      FROM tenant_%s.cases
      WHERE assigned_role IN ('CODER','NURSE_PHYSICIAN','ATTORNEY','ARBITRATOR') AND coalesce(assigned_to,'') NOT IN ('','unassigned-pool')
      ON CONFLICT DO NOTHING$q$, t, t);
    EXECUTE format($q$
      UPDATE tenant_%s.cases SET assigned_to = NULL, assigned_role = NULL
      WHERE assigned_role IN ('CODER','NURSE_PHYSICIAN','ATTORNEY','ARBITRATOR')$q$, t);
  END LOOP;
END $$;
