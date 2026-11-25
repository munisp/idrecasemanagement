-- audit_deadlines_20261113.sql — two deadline decisions from the 2026-11-13 audit. Safe to run twice.
--  F-05: the recommendation to AHCA is due in 60 days and may not pass the contract's 90 days without AHCA's
--        approval: AGENCY_RECOMMENDATION gets max_days 90, and an extension past it records AHCA's approval
--        (case_deadline_extensions.agency_approval; deadline_extensions.go refuses one without it).
--  F-04: the health plan's 15 days keep the Workflow's steps: day 13 remind the plan and, for a Medicaid plan,
--        notify AHCA; day 14 call and email; day 15 call again; no response is a default determination
--        (ahca_workflow.py, patch plan-followups-sop-20261113).
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL app.config_migration = 'on';

ALTER TABLE public.case_deadline_extensions ADD COLUMN IF NOT EXISTS agency_approval text;

UPDATE public.program_rules p SET config = jsonb_set(p.config, '{clocks}', (
  SELECT jsonb_agg(CASE c->>'name'
    WHEN 'AGENCY_RECOMMENDATION' THEN c || jsonb_build_object(
      'max_days', 90,
      'cite', 'On or before the 60-day turnaround time requirement; never past 90 days without AHCA''s approval')
    WHEN 'PLAN_RESPONSE' THEN c || jsonb_build_object(
      'cite', 'Up to 15 calendar days; day 13 remind the plan and, for a Medicaid plan, notify AHCA; day 14 call and email; day 15 call again; no response is a default determination',
      'follow_ups', '[{"day":13,"action":"plan_due_date_reminder"},{"day":13,"action":"medicaid_ahca_notify"},{"day":14,"action":"call_and_email"},{"day":15,"action":"call_again"}]'::jsonb)
    ELSE c END ORDER BY ord)
  FROM jsonb_array_elements(p.config->'clocks') WITH ORDINALITY AS t(c, ord)))
WHERE p.tenant = 'fl' AND p.config ? 'clocks';

SELECT c->>'name', c->>'max_days', jsonb_array_length(coalesce(c->'follow_ups','[]'::jsonb))
  FROM public.program_rules, jsonb_array_elements(config->'clocks') c
 WHERE tenant = 'fl' AND c->>'name' IN ('AGENCY_RECOMMENDATION', 'PLAN_RESPONSE');
COMMIT;
