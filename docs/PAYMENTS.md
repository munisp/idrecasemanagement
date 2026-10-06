# Payments & Financial Dashboard

## Collection rail: Stripe Checkout

Card collection for program fees (FL: $123.59 initial fee, full-review fees;
federal NSA admin fees) runs through **Stripe Checkout** — Stripe-hosted payment
pages, so card data never touches platform infrastructure (PCI scope stays with
Stripe).

- `POST /v1/tenants/{st}/invoices/{invId}/checkout` → creates a Checkout Session
  for an OPEN invoice, persists a PENDING row in `public.payments`, returns the
  hosted URL. Portal redirects the payer there.
- `POST /api/webhooks/stripe` → Stripe-signed (HMAC-SHA256, 5-min tolerance),
  handles `checkout.session.completed` (payment PAID → invoice PAID with the
  payment intent as remittance ref), `checkout.session.expired`, and
  `charge.refunded`.
- Staff-side refund (`settleInvoice` action=REFUND) calls `POST /v1/refunds`
  when a card payment is on file — an invoice is never marked REFUNDED unless
  the money actually moved.
- Config: `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `PORTAL_BASE_URL`
  (compose reads them from the environment; unset = card payments disabled,
  manual settlement still works).
- No Stripe SDK: the integration is dependency-free REST + HMAC, keeping the
  Go module graph offline-reproducible.

## Ledger of record: TigerBeetle

Stripe is the *rail*; **TigerBeetle is the ledger of record**. Every card
payment posts a double-entry leg (deterministic id = sha256(payment_intent +
direction), so webhook retries are idempotent):

| Event | Debit | Credit |
|---|---|---|
| Payment settled | `STRIPE_CLEARING` (6000) | `ESCROW_TRUST_HELD` (1000, party) |
| Refund issued | `ESCROW_TRUST_HELD` (party) | `STRIPE_CLEARING` (6000) |

Ledger posting is fail-open with an error log (the money is already real);
drift is caught by reconciling `public.payments` totals against TB account
balances. Manual fee flows (`/fees/transfer`) post directly to TB as before.

**Mojaloop settlement option (implemented, disabled by default).** Mojaloop is
not deployed — it is an interbank/scheme switch, not a merchant collection
rail. The platform-side boundary now exists for the day a state mandates it:

- `POST /v1/tenants/{st}/invoices/{invId}/checkout?provider=mojaloop` →
  prepares a transfer via the SDK scheme-adapter (`MOJALOOP_ADAPTER_URL`) and
  reserves funds as a **TB pending transfer** (Mojaloop prepare phase).
- `POST /api/webhooks/mojaloop` → HMAC-verified fulfil/reject from the
  adapter: COMMITTED posts the pending transfer and settles the invoice;
  ABORTED voids it. Idempotent per transferId — same webhook → invoice → TB
  pattern as Stripe, with Mojaloop's 2-phase lifecycle mapping onto
  TigerBeetle pending → post | void.
- Mojaloop's central-ledger is **MySQL-only** (no upstream Postgres profile);
  `deploy/helm-values/mojaloop.yaml` carries a tuned InnoDB config for that
  optional deployment (`enabled: false` by default).

## Financial dashboard (`#/finance`)

Backed by `GET /reports/financial` + `GET /payments`, over two tables:

- `public.payments` — every checkout session/payment intent with status,
  payer email, Stripe refs, and the last raw webhook payload (audit).
- `public.financial_events` — unified event stream: INVOICE_ISSUED,
  PAYMENT_INITIATED, PAYMENT_PAID (IN), REFUND_ISSUED (OUT), INVOICE_VOIDED —
  regardless of rail (Stripe, check/ACH manual settlement).

The dashboard shows: KPI cards (collected all-time / 30-day, refunded, payment
count), receivables by status × party, A/R aging buckets (current/1-30/31-60/60+),
payment-rail totals, the card payment list, and the full transaction stream.
Roles: FINANCE, CASE_MANAGER, FEDERAL_ADMIN, PLATFORM_ADMIN, STATE_AUDITOR
(read-only as always).
