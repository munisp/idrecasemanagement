-- ============================================================
-- The responding party of a dispute (respondents.go)
--
-- A provider's dispute (filed directly or by its TPA) responds to the health
-- plan; a health plan's dispute responds to the provider. Each case names
-- its other side here: linked to its account (so letters to the plan reach
-- it -- cases.payer_id / provider_id) and, when the other side is on the
-- platform, to the registered health plan or to the TPA whose client the
-- provider is. That organization then sees the dispute on its dashboard and
-- answers it there.
-- ============================================================

CREATE TABLE IF NOT EXISTS public.case_respondents (
    tenant          text NOT NULL,
    case_id         text NOT NULL,
    side            text NOT NULL,                 -- HEALTH_PLAN | PROVIDER
    name            text NOT NULL,
    email           text,                          -- where notices go
    naic            text,                          -- health plan
    npi             text,                          -- provider
    account_id      uuid REFERENCES public.accounts(id),
    plan_id         uuid REFERENCES public.health_plans(id),
    tpa_id          uuid REFERENCES public.tpas(id),
    client_id       uuid REFERENCES public.tpa_clients(id),
    response_status text NOT NULL DEFAULT 'AWAITING',  -- AWAITING|SUBMITTED|OPT_OUT_REQUESTED|CLOSED (found ineligible after it was told)
    response_note   text,
    responded_at    timestamptz,
    responded_by    text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, case_id)
);
CREATE INDEX IF NOT EXISTS case_respondents_plan ON public.case_respondents (tenant, plan_id) WHERE plan_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS case_respondents_tpa  ON public.case_respondents (tenant, tpa_id)  WHERE tpa_id IS NOT NULL;

-- Bulk rows carry their dispute's other party until it is filed.
ALTER TABLE public.tpa_dispute_batch_items
    ADD COLUMN IF NOT EXISTS other_party_name  text,
    ADD COLUMN IF NOT EXISTS other_party_email text,
    ADD COLUMN IF NOT EXISTS other_party_naic  text;

-- When the other side was told, in the portal, that the dispute is against
-- it (org_notify.go: once, when the dispute opens).
ALTER TABLE public.case_respondents ADD COLUMN IF NOT EXISTS notified_at timestamptz;

-- When the other side's notice address was emailed that the dispute is open
-- (org_notify.go). Separate from notified_at (the portal notice), so a plan
-- that registers later still gets its portal notice.
ALTER TABLE public.case_respondents ADD COLUMN IF NOT EXISTS emailed_at timestamptz;
