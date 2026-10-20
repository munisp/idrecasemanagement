-- Automatic case assignment: cases go to staff in turn, one rotation per tenant and role (case-api autoassign.go).
--
-- case_auto_assign: one row per tenant and role (CASE_MANAGER = Reviewer, CODER, ATTORNEY, NURSE_PHYSICIAN,
-- ARBITRATOR). enabled — the rotation is on (a missing row is treated as on): for Reviewers, new cases are assigned
-- automatically; for every role, the PM can hand a case to "Next in turn"; last_assignee — who got the last case, so
-- the next goes to the one after them; excluded — people the PM has taken out of the rotation (on leave, part-time),
-- by Keycloak user id. The PM and the Tenant Admin change any assignment by hand as before.
--
-- Safe to run more than once, and upgrades the first version (one row per tenant, Reviewers only): its rows become
-- the Reviewer rotation.
CREATE TABLE IF NOT EXISTS public.case_auto_assign (
  tenant        text NOT NULL,
  role          text NOT NULL DEFAULT 'CASE_MANAGER',
  enabled       boolean NOT NULL DEFAULT true,
  last_assignee text,
  excluded      text[] NOT NULL DEFAULT '{}',
  updated_by    text,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, role)
);

ALTER TABLE public.case_auto_assign ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'CASE_MANAGER';

DO $$
BEGIN
  IF (SELECT i.indnatts FROM pg_index i
      WHERE i.indrelid = 'public.case_auto_assign'::regclass AND i.indisprimary) = 1 THEN
    ALTER TABLE public.case_auto_assign DROP CONSTRAINT case_auto_assign_pkey;
    ALTER TABLE public.case_auto_assign ADD PRIMARY KEY (tenant, role);
  END IF;
END $$;
