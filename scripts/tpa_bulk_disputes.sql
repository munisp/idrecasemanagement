-- ============================================================
-- TPA bulk dispute filing with documents per dispute
-- (tpa_bulk_disputes.go, feature flag: tpa_onboarding)
--
-- A TPA files many disputes in one ZIP: disputes.csv lists the disputes,
-- and a folder per dispute (named after its dispute_ref) holds that
-- dispute's six required documents, or one merged.pdf with all six. Every
-- row gets its own package, checked by doc-intel like any other; a row is
-- filed by the background filer once its package is READY.
-- ============================================================

CREATE TABLE IF NOT EXISTS public.tpa_dispute_batches (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    tpa_id      uuid NOT NULL REFERENCES public.tpas(id),
    submitter   text NOT NULL,
    batch_ref   text NOT NULL,           -- the TPA's own idempotency key
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, tpa_id, batch_ref)
);

CREATE TABLE IF NOT EXISTS public.tpa_dispute_batch_items (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant                text NOT NULL,
    batch_id              uuid NOT NULL REFERENCES public.tpa_dispute_batches(id) ON DELETE CASCADE,
    row_no                int  NOT NULL,
    dispute_ref           text NOT NULL,
    client_id             uuid REFERENCES public.tpa_clients(id),
    package_id            uuid REFERENCES public.tpa_packages(id),
    disputed_amount_cents bigint NOT NULL DEFAULT 0,
    contact_name          text,
    notes                 text,
    status                text NOT NULL DEFAULT 'WAITING_DOCS',  -- WAITING_DOCS|FILING|FILED|ERROR
    error                 text,
    case_id               text,
    case_number           text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    filed_at              timestamptz,
    UNIQUE (batch_id, dispute_ref)
);
CREATE INDEX IF NOT EXISTS tpa_dispute_batch_items_waiting ON public.tpa_dispute_batch_items (status) WHERE status='WAITING_DOCS';
CREATE INDEX IF NOT EXISTS tpa_dispute_batch_items_batch ON public.tpa_dispute_batch_items (tenant, batch_id);

-- One Stripe checkout paying the initial fees of a bulk batch's filed
-- disputes together (tpa_bulk_payments.go). A new checkout for the batch
-- expires the previous unpaid one, so no invoice can be paid twice.
CREATE TABLE IF NOT EXISTS public.payment_groups (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant         text NOT NULL,
    batch_id       uuid REFERENCES public.tpa_dispute_batches(id),
    invoice_ids    uuid[] NOT NULL,
    amount_cents   bigint NOT NULL,
    session_id     text UNIQUE,
    checkout_url   text,
    status         text NOT NULL DEFAULT 'OPEN',   -- OPEN|PAID|EXPIRED
    payment_intent text,
    created_by     text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    paid_at        timestamptz
);
CREATE INDEX IF NOT EXISTS payment_groups_batch ON public.payment_groups (tenant, batch_id, status);
-- A checkout can also cover all of a TPA's unpaid fees, not one batch.
ALTER TABLE public.payment_groups ADD COLUMN IF NOT EXISTS tpa_id uuid REFERENCES public.tpas(id);
