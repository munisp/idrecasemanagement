-- Author: Patrick Munis at newwave
-- ============================================================
-- Fee payments for outside filers (fee_payments.go)
--
-- One Stripe checkout pays the initial fees of a scope the filer picks: one
-- dispute, one provider (a TPA's client / a plan's party), one bulk upload,
-- or everything unpaid. payment_groups records each checkout and what it
-- covers; the server works out the amount, never the browser.
--
-- payment_groups.status: OPEN (checkout opened) | PROCESSING (Stripe took
-- the payment, not confirmed yet) | PAID | FAILED | CANCELLED (the payer
-- left the checkout) | EXPIRED (Stripe closed it, or a newer checkout for
-- the same fees replaced it).
-- ============================================================

ALTER TABLE public.payment_groups
    ADD COLUMN IF NOT EXISTS plan_id    uuid REFERENCES public.health_plans(id),
    ADD COLUMN IF NOT EXISTS client_id  uuid,
    ADD COLUMN IF NOT EXISTS case_id    text,
    ADD COLUMN IF NOT EXISTS scope      text,
    ADD COLUMN IF NOT EXISTS last_error text,
    ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS payment_groups_invoices ON public.payment_groups USING gin (invoice_ids);

-- Why the latest attempt failed (card declined, ...), shown to the payer.
ALTER TABLE public.payments ADD COLUMN IF NOT EXISTS last_error text;
