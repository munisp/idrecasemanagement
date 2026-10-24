-- outcomes_20261022.sql — the program's case outcomes as the office defines them (2026-10-22), each with its
-- definition, and the cases' recorded outcomes renamed to match. Safe to run twice.
--   psql -d <db> -v ON_ERROR_STOP=1 -f outcomes_20261022.sql
\set ON_ERROR_STOP on
BEGIN;

-- The list (field_schema.case_outcome: what the outcome may be) and what each one means (outcome_definitions).
UPDATE public.program_rules SET config = jsonb_set(jsonb_set(config,
  '{field_schema,case_outcome}', '[
    "TBD - In Process", "Ineligible - Closed", "Withdrawn", "Dismissed", "Default Award", "Full Award", "Partial Award",
    "No Award", "Plan Opt-Out", "Other", "Costs Resolved Prior to Final Determination (Pre-Settlement)", "Refunded"]'::jsonb),
  '{outcome_definitions}', '{
    "TBD - In Process": "Case remains active and has not reached a final disposition.",
    "Ineligible - Closed": "Case failed eligibility requirements and was closed prior to acceptance.",
    "Withdrawn": "Provider or Health Plan withdrew the dispute after acceptance and prior to final determination.",
    "Dismissed": "Case was dismissed due to procedural requirements not being met (e.g., authorization not received within required timeframe).",
    "Default Award": "Final determination issued due to lack of Health Plan response within required timeframe.",
    "Full Award": "Reviewer determined the provider''s disputed amount should be awarded in full.",
    "Partial Award": "Reviewer determined only a portion of the disputed amount should be awarded.",
    "No Award": "Reviewer determined no additional payment is warranted.",
    "Plan Opt-Out": "Health Plan exercised a valid statutory opt-out and provided supporting documentation.",
    "Other": "Outcome does not fit an existing disposition category and requires explanation.",
    "Costs Resolved Prior to Final Determination (Pre-Settlement)": "Parties resolved the payment dispute before a determination was issued.",
    "Refunded": "Case fee or payment refunded in accordance with program requirements."}'::jsonb)
WHERE tenant = 'fl';

-- Outcomes already recorded on cases, under their new names.
UPDATE tenant_fl.cases SET details = jsonb_set(details, '{case_outcome}', to_jsonb(CASE details->>'case_outcome'
    WHEN 'TBD – case in process' THEN 'TBD - In Process'
    WHEN 'Provider Default Award' THEN 'Default Award'
    WHEN 'Provider Full Award' THEN 'Full Award'
    WHEN 'Provider Partial Award' THEN 'Partial Award'
    WHEN 'Provider No Award' THEN 'No Award' END))
WHERE details->>'case_outcome' IN ('TBD – case in process', 'Provider Default Award', 'Provider Full Award',
                                    'Provider Partial Award', 'Provider No Award');

SELECT details->>'case_outcome' AS case_outcome, count(*) FROM tenant_fl.cases
 WHERE details ? 'case_outcome' GROUP BY 1 ORDER BY 2 DESC;
COMMIT;
