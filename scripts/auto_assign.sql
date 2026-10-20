-- Automatic case assignment: new cases go to the tenant's Reviewers in turn (case-api autoassign.go).
--
-- case_auto_assign: one row per tenant. enabled — new cases are assigned automatically (a tenant with no row is
-- treated as on); last_assignee — the Reviewer who got the last case, so the next goes to the one after them;
-- excluded — Reviewers the PM has taken out of the rotation (on leave, part-time), by Keycloak user id. The PM and
-- the Tenant Admin change any assignment by hand as before.
--
-- Safe to run more than once.
CREATE TABLE IF NOT EXISTS public.case_auto_assign (
  tenant        text PRIMARY KEY,
  enabled       boolean NOT NULL DEFAULT true,
  last_assignee text,
  excluded      text[] NOT NULL DEFAULT '{}',
  updated_by    text,
  updated_at    timestamptz NOT NULL DEFAULT now()
);
