-- 2026-11-26, SOP step 10 "Clinical/Coding review completed": more than one person can review a case. The case's
-- Coder and Nurse Reviewer (case_helpers) are joined by any further reviewers the PM or the Admin names here; each
-- records their own review. The main Coding / Clinical review completes only when every named reviewer has.
CREATE TABLE IF NOT EXISTS public.case_review_team (
  tenant    text NOT NULL,
  case_id   text NOT NULL,
  user_id   text NOT NULL,
  role      text NOT NULL CHECK (role IN ('CODER','NURSE_PHYSICIAN')),
  added_by  text NOT NULL DEFAULT '',
  added_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, case_id, user_id)
);
CREATE TABLE IF NOT EXISTS public.case_review_records (
  id                  bigserial PRIMARY KEY,
  tenant              text NOT NULL,
  case_id             text NOT NULL,
  user_id             text NOT NULL,
  user_name           text NOT NULL DEFAULT '',
  role                text NOT NULL,
  recommended_outcome text,
  findings            text NOT NULL DEFAULT '',
  created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS case_review_records_case ON public.case_review_records (tenant, case_id, created_at);
DO $$ DECLARE o text; BEGIN
  SELECT tableowner INTO o FROM pg_tables WHERE schemaname='public' AND tablename='invoices';
  EXECUTE format('ALTER TABLE public.case_review_team OWNER TO %I', o);
  EXECUTE format('ALTER TABLE public.case_review_records OWNER TO %I', o);
  EXECUTE format('ALTER SEQUENCE public.case_review_records_id_seq OWNER TO %I', o);
END $$;
