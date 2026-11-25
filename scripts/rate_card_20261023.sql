-- rate_card_20261023.sql — the FL program's prices as AHCA Contract HQA028, Amendment No. 2, Exhibit II-A (Price Sheet)
-- gives them, for the contract term January 1, 2024 – December 31, 2027. Safe to run twice.
--   * the initial eligibility determination filing fee for each year, with the credit card transaction fee a card
--     payment adds (line 6; a bank account or check pays the fee alone);
--   * the late fee on a review fee not paid within 35 days of the Final Order: $50 from day 36, and $50 more every
--     30 days until paid (line 7);
--   * the hourly rate of each role's billable time (lines 1-5). A Nurse / Physician logs each entry as a Nurse
--     (Utilization Nursing Review) or a Physician (Physician Expert), so that role has no rate of its own.
--   psql -d <db> -v ON_ERROR_STOP=1 -f rate_card_20261023.sql
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL app.config_migration = 'on';  -- fees are guarded

-- The card fee each payment carried (card_fee.go).
ALTER TABLE public.payments ADD COLUMN IF NOT EXISTS card_fee_cents bigint NOT NULL DEFAULT 0;
ALTER TABLE public.payment_groups ADD COLUMN IF NOT EXISTS card_fee_cents bigint NOT NULL DEFAULT 0;
ALTER TABLE public.payment_groups ADD COLUMN IF NOT EXISTS pay_by text;

UPDATE public.program_rules SET config = jsonb_set(jsonb_set(jsonb_set(config,
  '{fees,initial_fee_schedule}', $j$[
    {"effective_from": "2024-01-01", "cents": 8500,  "card_fee_cents": 285},
    {"effective_from": "2025-01-01", "cents": 12000, "card_fee_cents": 388},
    {"effective_from": "2026-01-01", "cents": 12360, "card_fee_cents": 399},
    {"effective_from": "2027-01-01", "cents": 12730, "card_fee_cents": 410}
  ]$j$::jsonb),
  '{fees,initial_fee_cents}', to_jsonb(CASE
    WHEN (now() AT TIME ZONE 'America/New_York')::date >= '2027-01-01' THEN 12730
    WHEN (now() AT TIME ZONE 'America/New_York')::date >= '2026-01-01' THEN 12360
    WHEN (now() AT TIME ZONE 'America/New_York')::date >= '2025-01-01' THEN 12000 ELSE 8500 END)),
  '{fees,late_fee}', '{"cents": 5000, "after_days": 35, "every_days": 30}'::jsonb)
WHERE tenant = 'fl';

INSERT INTO public.time_rate_schedule (tenant, role, effective_from, rate_cents_per_hour, set_by)
SELECT 'fl', role, effective_from::date, cents, 'AHCA HQA028 Amendment 2, Exhibit II-A' FROM (VALUES
  ('DOCTOR',       '2024-01-01', 23500), ('DOCTOR',       '2025-01-01', 24200), ('DOCTOR',       '2026-01-01', 24900), ('DOCTOR',       '2027-01-01', 25700),
  ('NURSE',        '2024-01-01', 10000), ('NURSE',        '2025-01-01', 10300), ('NURSE',        '2026-01-01', 10600), ('NURSE',        '2027-01-01', 10900),
  ('CODER',        '2024-01-01', 13000), ('CODER',        '2025-01-01', 13400), ('CODER',        '2026-01-01', 13800), ('CODER',        '2027-01-01', 14200),
  ('ATTORNEY',     '2024-01-01', 17500), ('ATTORNEY',     '2025-01-01', 18000), ('ATTORNEY',     '2026-01-01', 18600), ('ATTORNEY',     '2027-01-01', 19100),
  ('CASE_MANAGER', '2024-01-01',     0), ('CASE_MANAGER', '2025-01-01',  7000), ('CASE_MANAGER', '2026-01-01',  7200), ('CASE_MANAGER', '2027-01-01',  7400)
) AS r(role, effective_from, cents)
ON CONFLICT (tenant, role, effective_from) DO UPDATE SET rate_cents_per_hour = EXCLUDED.rate_cents_per_hour, set_by = EXCLUDED.set_by, set_at = now();

SELECT jsonb_pretty(config->'fees'->'initial_fee_schedule') AS fee_schedule, config->'fees'->>'initial_fee_cents' AS fee_now,
       config->'fees'->'late_fee' AS late_fee FROM public.program_rules WHERE tenant = 'fl';
SELECT role, string_agg(effective_from || ' $' || to_char(rate_cents_per_hour / 100.0, 'FM990.00'), ' · ' ORDER BY effective_from) AS rates
  FROM public.time_rate_schedule WHERE tenant = 'fl' GROUP BY role ORDER BY role;
COMMIT;
