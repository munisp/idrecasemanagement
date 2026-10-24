-- Author: Patrick Munis at newwave
-- tpa_saved_payments.sql — cards and US bank accounts (ACH) saved with Stripe,
-- for a TPA's clients and for the TPA itself, and fee payments charged to them
-- by the FeePaymentWorkflow (Temporal). Idempotent.

-- One Stripe customer per payer: the TPA (owner_kind 'tpa') or one of its
-- clients ('tpa_client').
CREATE TABLE IF NOT EXISTS public.stripe_customers (
  tenant      text NOT NULL,
  owner_kind  text NOT NULL CHECK (owner_kind IN ('tpa','tpa_client')),
  owner_id    uuid NOT NULL,
  customer_id text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, owner_kind, owner_id)
);

-- A payment method with no client is the TPA's own.
ALTER TABLE public.tpa_payment_methods ALTER COLUMN client_id DROP NOT NULL;
ALTER TABLE public.tpa_payment_methods
  ADD COLUMN IF NOT EXISTS stripe_customer text,
  ADD COLUMN IF NOT EXISTS setup_intent    text,
  ADD COLUMN IF NOT EXISTS mandate         text,   -- ACH: the debit authorization Stripe recorded
  ADD COLUMN IF NOT EXISTS bank_name       text,
  ADD COLUMN IF NOT EXISTS verify_url      text,   -- ACH by micro-deposits: where to confirm them
  ADD COLUMN IF NOT EXISTS added_by        text;
CREATE UNIQUE INDEX IF NOT EXISTS tpa_payment_methods_pm ON public.tpa_payment_methods (processor_ref) WHERE processor_ref LIKE 'pm\_%';
CREATE INDEX IF NOT EXISTS tpa_payment_methods_owner ON public.tpa_payment_methods (tenant, tpa_id, client_id);

-- A fee payment charged to a saved method: who pays (the client's own method,
-- or the TPA's), which method, and the run it belongs to (one Pay click).
ALTER TABLE public.payment_groups
  ADD COLUMN IF NOT EXISTS source            text NOT NULL DEFAULT 'CHECKOUT', -- CHECKOUT | CLIENT_METHOD | TPA_METHOD
  ADD COLUMN IF NOT EXISTS payment_method_id uuid,
  ADD COLUMN IF NOT EXISTS run_id            uuid,
  ADD COLUMN IF NOT EXISTS workflow_id       text;
CREATE INDEX IF NOT EXISTS payment_groups_run ON public.payment_groups (tenant, run_id) WHERE run_id IS NOT NULL;
