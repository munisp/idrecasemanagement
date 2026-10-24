-- The written decision on a case, and the letterhead / invoice details the generated letters print.
--
-- decision_reports: one per case. What the reviewers write for the Review Rationale (the decision report), the
-- cover letter and the invoices: the outcome, the amounts, the sections (claim summary, each party's position,
-- applicable law, the physician's and the coding consultant's reviews...), the claims table (billed, UCR, lesser
-- of, paid, reimbursement) and the disallowed-charges table. lettergen reads it when it builds those letters.
--
-- letter_settings: per tenant. The organization's name and program lines on the letterhead, the Agency's
-- addressee for the cover letter, and the invoice's late-charge terms and remit-to bank details. Kept in the
-- database, not in code, so a bank change is an edit on the Billing page, not a deploy.
--
-- Safe to run more than once.
CREATE TABLE IF NOT EXISTS public.decision_reports (
  tenant             text NOT NULL,
  case_id            text NOT NULL,
  report_type        text NOT NULL DEFAULT 'CLAIM_DISPUTE' CHECK (report_type IN ('CLAIM_DISPUTE', 'IRO')),
  outcome            text CHECK (outcome IN ('OVERTURNED', 'PARTIALLY_OVERTURNED', 'UPHELD')),
  awarded_cents      bigint CHECK (awarded_cents >= 0),
  already_paid_cents bigint CHECK (already_paid_cents >= 0),
  percent_awarded    numeric(5,2) CHECK (percent_awarded BETWEEN 0 AND 100),
  sections           jsonb NOT NULL DEFAULT '{}',
  claims             jsonb NOT NULL DEFAULT '[]',
  disallowed         jsonb NOT NULL DEFAULT '[]',
  reviewers          jsonb NOT NULL DEFAULT '[]',
  section_authors    jsonb NOT NULL DEFAULT '{}',
  updated_by         text NOT NULL DEFAULT '',
  updated_at         timestamptz NOT NULL DEFAULT now(),
  created_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, case_id)
);

CREATE TABLE IF NOT EXISTS public.letter_settings (
  tenant     text PRIMARY KEY,
  settings   jsonb NOT NULL DEFAULT '{}',
  updated_by text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- Owned by the database's app role -- whoever owns public.invoices (idre on dev, idre_prod on idre-prod) -- so
-- the case-api of that deployment can read and write them.
DO $own$
DECLARE app text;
BEGIN
  SELECT tableowner INTO app FROM pg_tables WHERE schemaname = 'public' AND tablename = 'invoices';
  IF app IS NOT NULL THEN
    EXECUTE format('ALTER TABLE public.decision_reports OWNER TO %I', app);
    EXECUTE format('ALTER TABLE public.letter_settings OWNER TO %I', app);
  END IF;
END
$own$;
