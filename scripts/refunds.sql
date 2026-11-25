-- Refunds (case-api refunds.go): one row per refund of a paid fee, the single record of what was refunded, how and
-- whether it went through. An invoice is REFUND_PENDING while a card refund waits on Stripe, REFUNDED once it has
-- gone through, and back to PAID if Stripe fails it. A duplicate payment's automatic refund is kind DUPLICATE: it
-- never changes the invoice, which the other payment still pays.
CREATE TABLE IF NOT EXISTS public.refunds (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant           text NOT NULL,
    case_id          text NOT NULL,
    invoice_id       uuid NOT NULL,
    payment_id       uuid,                         -- public.payments row refunded (null: marked paid by hand)
    kind             text NOT NULL DEFAULT 'FEE',  -- FEE | DUPLICATE
    method           text NOT NULL,                -- stripe | check | offline | stripe-dashboard
    amount_cents     bigint NOT NULL CHECK (amount_cents > 0),
    status           text NOT NULL,                -- PENDING | SUCCEEDED | FAILED
    stripe_refund_id text,
    payment_intent   text,
    ledger_key       text,                         -- the payment's ledger key, reversed once the refund succeeds
    party            text,
    reference        text,                         -- check / ACH number for a refund made outside Stripe
    failure          text,
    requested_by     text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS refunds_stripe_id ON public.refunds (stripe_refund_id) WHERE stripe_refund_id IS NOT NULL;
-- At most one live (pending or done) fee refund per invoice: a second click cannot refund it twice.
CREATE UNIQUE INDEX IF NOT EXISTS refunds_one_live_fee ON public.refunds (tenant, invoice_id)
    WHERE kind = 'FEE' AND status IN ('PENDING', 'SUCCEEDED');
CREATE INDEX IF NOT EXISTS refunds_pending ON public.refunds (status, created_at) WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS refunds_invoice ON public.refunds (tenant, invoice_id);
