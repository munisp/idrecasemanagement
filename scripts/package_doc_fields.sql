-- The eligibility facts each checked document states (dates of service, the plan's decision date, provider type,
-- network status), read when a TPA's or plan's document set is checked and carried onto the case when the set is
-- filed, so the eligibility review can decide from them. Safe to run more than once.
ALTER TABLE public.tpa_package_docs ADD COLUMN IF NOT EXISTS fields jsonb NOT NULL DEFAULT '{}';
ALTER TABLE public.hp_package_docs ADD COLUMN IF NOT EXISTS fields jsonb NOT NULL DEFAULT '{}';
-- A bulk-filed row's own answers to the two eligibility questions (provider_type, in_network columns of
-- disputes.csv), put on the case when the row is filed.
ALTER TABLE public.tpa_dispute_batch_items ADD COLUMN IF NOT EXISTS eligibility_answers jsonb NOT NULL DEFAULT '{}';
