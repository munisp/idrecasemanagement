-- repair_financials_20261022.sql — corrects money records written before the fixes of 2026-10-22, so the
-- Financials figures reconcile: Collected = fees paid (card + checks + recorded by hand), Refunded = refunds that
-- happened, and money that pays no fee is held as "not applied".
--
-- Three cases, each found by a rule (not by hand-picked ids), each change labelled actor='data-repair 2026-10-22':
--   1. A PAYMENT_PAID larger than its fee (a check for more than the fee was counted whole): the event is cut to
--      the fee and the rest recorded as PAYMENT_UNAPPLIED.
--   2. A fee marked paid (INVOICE_PAID on its case timeline) with no PAYMENT_PAID event: the missing event is added,
--      dated when it was marked paid.
--   3. A fee "refunded" that was never paid (no PAYMENT_PAID, no paid payment, never marked paid): the refund never
--      happened. A REFUND_REVERSED offsets its REFUND_ISSUED and the invoice becomes VOID (nothing paid, nothing owed).
--
-- Safe to run twice: each rule only matches what is still wrong. Run with  -v dry=1  to see what would change
-- (rolled back), without it to apply. Usage:
--   { echo "SET ROLE <table owner>;"; cat repair_financials_20261022.sql; } | psql -d <db> -v ON_ERROR_STOP=1 [-v dry=1]
\set ON_ERROR_STOP on
BEGIN;

-- 1. payments larger than their fee
CREATE TEMP TABLE fix_over ON COMMIT DROP AS
SELECT e.id, e.tenant, e.case_id, e.invoice_id, e.amount_cents AS paid, i.amount_cents AS fee, e.ref, e.created_at, i.invoice_no
FROM public.financial_events e JOIN public.invoices i ON i.id::text = e.invoice_id::text AND i.tenant = e.tenant
WHERE e.kind = 'PAYMENT_PAID' AND e.amount_cents > i.amount_cents;

-- 2. fees marked paid with no payment event
CREATE TEMP TABLE fix_missing ON COMMIT DROP AS
SELECT i.id AS invoice_id, i.tenant, i.case_id, i.amount_cents, i.party, i.invoice_no,
       (SELECT max(a.created_at) FROM public.case_activities a
         WHERE a.tenant = i.tenant AND a.case_id = i.case_id AND a.type = 'INVOICE_PAID' AND a.body LIKE '%' || i.id::text || '%') AS paid_at,
       coalesce(nullif(i.remittance_ref, ''), 'recorded by hand') AS ref
FROM public.invoices i
WHERE i.status IN ('PAID', 'REFUNDED', 'REFUND_PENDING')
  AND NOT EXISTS (SELECT 1 FROM public.financial_events e WHERE e.tenant = i.tenant AND e.invoice_id::text = i.id::text AND e.kind = 'PAYMENT_PAID')
  AND EXISTS (SELECT 1 FROM public.case_activities a
              WHERE a.tenant = i.tenant AND a.case_id = i.case_id AND a.type = 'INVOICE_PAID' AND a.body LIKE '%' || i.id::text || '%');

-- 3. refunds of fees that were never paid
CREATE TEMP TABLE fix_phantom ON COMMIT DROP AS
SELECT i.id AS invoice_id, i.tenant, i.case_id, i.party, i.invoice_no,
       coalesce((SELECT sum(e.amount_cents) FROM public.financial_events e WHERE e.tenant = i.tenant AND e.invoice_id::text = i.id::text AND e.kind = 'REFUND_ISSUED'), 0)
     - coalesce((SELECT sum(e.amount_cents) FROM public.financial_events e WHERE e.tenant = i.tenant AND e.invoice_id::text = i.id::text AND e.kind = 'REFUND_REVERSED'), 0) AS refunded
FROM public.invoices i
WHERE i.status = 'REFUNDED'
  AND NOT EXISTS (SELECT 1 FROM public.financial_events e WHERE e.tenant = i.tenant AND e.invoice_id::text = i.id::text AND e.kind = 'PAYMENT_PAID')
  AND NOT EXISTS (SELECT 1 FROM public.payments p WHERE p.tenant = i.tenant AND p.invoice_id = i.id AND p.status IN ('PAID', 'REFUNDED'))
  AND NOT EXISTS (SELECT 1 FROM public.case_activities a
                  WHERE a.tenant = i.tenant AND a.case_id = i.case_id AND a.type = 'INVOICE_PAID' AND a.body LIKE '%' || i.id::text || '%');

\echo '== 1. payments larger than their fee (cut to the fee; the rest -> not applied)'
SELECT tenant, invoice_no, paid AS paid_cents, fee AS fee_cents, paid - fee AS unapplied_cents FROM fix_over ORDER BY 1, 2;
\echo '== 2. fees marked paid with no payment event (event added)'
SELECT tenant, invoice_no, amount_cents, paid_at, ref FROM fix_missing ORDER BY 1, 2;
\echo '== 3. refunds of fees never paid (refund offset, invoice -> VOID)'
SELECT tenant, invoice_no, refunded AS refund_offset_cents FROM fix_phantom ORDER BY 1, 2;

INSERT INTO public.financial_events (tenant, case_id, invoice_id, kind, direction, amount_cents, party, ref, actor, created_at)
SELECT tenant, case_id, invoice_id, 'PAYMENT_UNAPPLIED', 'IN', paid - fee, '', coalesce(ref, ''), 'data-repair 2026-10-22', created_at FROM fix_over;
UPDATE public.financial_events e SET amount_cents = f.fee FROM fix_over f WHERE e.id = f.id;

INSERT INTO public.financial_events (tenant, case_id, invoice_id, kind, direction, amount_cents, party, ref, actor, created_at)
SELECT tenant, case_id, invoice_id, 'PAYMENT_PAID', 'IN', amount_cents, coalesce(party, ''), ref, 'data-repair 2026-10-22', coalesce(paid_at, now()) FROM fix_missing;

INSERT INTO public.financial_events (tenant, case_id, invoice_id, kind, direction, amount_cents, party, ref, actor)
SELECT tenant, case_id, invoice_id, 'REFUND_REVERSED', 'IN', refunded, coalesce(party, ''), 'correction: this fee was never paid, so nothing was refunded', 'data-repair 2026-10-22'
FROM fix_phantom WHERE refunded > 0;
UPDATE public.invoices i SET status = 'VOID', remittance_ref = 'void: refunded in error, never paid (data-repair 2026-10-22)'
FROM fix_phantom f WHERE i.id = f.invoice_id;
INSERT INTO public.case_activities (tenant, case_id, type, body)
SELECT tenant, case_id, 'FINANCE_CORRECTION', 'Invoice ' || invoice_no || ': marked refunded although it was never paid — corrected to void, the refund offset (data-repair 2026-10-22)' FROM fix_phantom;

\if :{?dry}
  \echo '== dry run: rolled back, nothing changed'
  ROLLBACK;
\else
  COMMIT;
  \echo '== applied'
\endif
