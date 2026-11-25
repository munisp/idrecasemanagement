-- configuration.sql — dated pay rates for the timesheet: each role's hourly rate with the day it takes effect, so a
-- rate change (yearly, or any time) prices time worked from that day on and never re-prices time already worked.
-- The program's dated fee schedule lives in program_rules.config->'fees'->'initial_fee_schedule'. Idempotent.
CREATE TABLE IF NOT EXISTS public.time_rate_schedule (
  tenant              text   NOT NULL,
  role                text   NOT NULL,
  effective_from      date   NOT NULL,
  rate_cents_per_hour bigint NOT NULL CHECK (rate_cents_per_hour >= 0),
  set_by              text,
  set_at              timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, role, effective_from)
);

-- The rates already set count from the start: time already recorded keeps the price it had.
INSERT INTO public.time_rate_schedule (tenant, role, effective_from, rate_cents_per_hour, set_by, set_at)
SELECT tenant, role, DATE '2000-01-01', rate_cents_per_hour, updated_by, updated_at FROM public.time_rates
ON CONFLICT DO NOTHING;
