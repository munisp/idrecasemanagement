-- 2026-11-26: the amount owed to the provider is entered by the reviewer on the decision report, not worked out
-- from the award less what was already paid (case-api decision_report.go, lettergen builtin_letters.py).
ALTER TABLE public.decision_reports ADD COLUMN IF NOT EXISTS owed_cents bigint;
