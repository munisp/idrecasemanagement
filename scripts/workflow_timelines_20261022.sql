-- workflow_timelines_20261022.sql — the FL program's timelines and outcome structure exactly as the office's
-- "Streamline Workflows, Timelines, and Outcomes" document gives them (2026-10-22). Each clock names the date it runs
-- from (basis) and the dates that mean its step was done (stop_on): a done step's clock stops, on time or late.
-- A review step's target clock also stops once the determination has gone to AHCA (a review done before its own
-- start/finish dates were recorded). A "target" is a working target (how long a step usually takes): shown, never a breach. Safe to run twice.
--   psql -d <db> -v ON_ERROR_STOP=1 -f workflow_timelines_20261022.sql
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL app.config_migration = 'on';  -- fees and billing (the 35-day payment timeframe) are guarded

UPDATE public.program_rules SET config = jsonb_set(jsonb_set(jsonb_set(jsonb_set(jsonb_set(config,
  '{clocks}', $j$[
    {"name": "INITIAL_REVIEW", "label": "Eligibility review", "basis": "packet_complete_at", "days": 13, "day_type": "calendar",
     "cite": "Complete within 13 calendar days; the acceptance or ineligibility letter on or before day 13",
     "breach": "Eligibility review late", "stop_on": ["date_case_accepted", "date_ineligibility_letter_sent"]},
    {"name": "ACCEPTANCE_PLAN_NOTICE", "label": "Acceptance and Health Plan Notification (no cost estimate)", "basis": "no_estimate_window_from",
     "days": 13, "day_type": "calendar", "cite": "No cost estimate requested: acceptance and Health Plan Notification on or before day 13",
     "breach": "Health Plan Notification late", "stop_on": ["plan_notified_at"]},
    {"name": "PROVIDER_PERMISSION", "label": "Provider authorization / permission", "basis": "estimate_sent_at", "days": 15, "day_type": "calendar",
     "cite": "Within 15 calendar days of the acceptance and cost estimate; none received is a dismissal",
     "breach": "Dismissal", "stop_on": ["date_provider_responded", "date_dismissal_letter_sent"]},
    {"name": "PLAN_NOTIFICATION", "label": "Health Plan Notification", "basis": "date_provider_responded", "days": 1, "day_type": "business",
     "cite": "Same day or next business day after the provider's permission", "breach": "Health Plan Notification late",
     "stop_on": ["plan_notified_at"]},
    {"name": "PLAN_RESPONSE", "label": "Health Plan response", "basis": "plan_notified_at", "days": 15, "day_type": "calendar",
     "cite": "Up to 15 calendar days; a reminder of the due date on day 13; no response is a default determination",
     "breach": "Default determination for provider", "follow_ups": [{"day": 13, "action": "plan_due_date_reminder"}],
     "stop_on": ["date_plan_responded", "opt_out_decided_at"]},
    {"name": "OPT_OUT_LETTER", "label": "Opt-Out Determination Letter", "basis": "opt_out_decided_at", "days": 1, "day_type": "business",
     "cite": "Same day or next business day after the plan's opt-out", "breach": "Opt-Out letter late",
     "stop_on": ["date_opt_out_letter_sent"]},
    {"name": "REVIEW_START", "label": "Nurse/Coder review begins", "basis": "date_plan_responded", "days": 15, "day_type": "calendar",
     "target": true, "cite": "Approximately 15 days after receipt of the complete Health Plan packet",
     "stop_on": ["coding_review_started_at", "clinical_review_started_at", "date_recommendation_sent_to_agency"]},
    {"name": "CLINICAL_CODING_REVIEW", "label": "Clinical/Coding review", "basis": "coding_review_started_at", "days": 5, "max_days": 6,
     "day_type": "calendar", "target": true, "cite": "Target review period: 5-6 days", "stop_on": ["coding_review_completed_at", "date_recommendation_sent_to_agency"]},
    {"name": "ATTORNEY_START", "label": "Attorney review begins", "basis": "date_plan_responded", "days": 30, "day_type": "calendar",
     "target": true, "cite": "Approximately 30 days after receipt of the Health Plan packet", "stop_on": ["attorney_review_started_at", "date_recommendation_sent_to_agency"]},
    {"name": "ATTORNEY_REVIEW", "label": "Attorney review", "basis": "attorney_review_started_at", "days": 5, "max_days": 6,
     "day_type": "calendar", "target": true, "cite": "Target review period: 5-6 days", "stop_on": ["attorney_review_completed_at", "date_recommendation_sent_to_agency"]},
    {"name": "PM_REVIEW", "label": "Program Manager review", "basis": "attorney_review_completed_at", "days": 5, "day_type": "calendar",
     "target": true, "cite": "Target review period: 5 days", "stop_on": ["date_recommendation_sent_to_agency"]},
    {"name": "AGENCY_RECOMMENDATION", "label": "Determination submitted to AHCA", "basis": "received_at", "days": 60, "day_type": "calendar",
     "cite": "On or before the 60-day turnaround time requirement", "breach": "60-day turnaround missed",
     "stop_on": ["date_recommendation_sent_to_agency"]},
    {"name": "FINAL_ORDER", "label": "Final Order from AHCA", "basis": "date_recommendation_sent_to_agency", "days": 30, "day_type": "calendar",
     "target": true, "cite": "Typically issued within 30 days", "stop_on": ["date_final_order_sent"]},
    {"name": "RFI_RESPONSE", "label": "RFI / additional documentation", "basis": "rfi_sent_at", "days": 15, "day_type": "calendar",
     "follow_ups": [{"day": 15, "action": "timeframe_lapsed_email_and_call"}], "stop_on": ["rfi_response_received_at"]},
    {"name": "REFUND_WINDOW", "label": "Complete packet after payment", "basis": "outreach_at", "days": 7, "day_type": "calendar",
     "stop_on": ["packet_complete_at"]},
    {"name": "FILING_ELIGIBILITY", "label": "Filing window from final determination", "basis": "final_determination_at", "days": 365,
     "day_type": "calendar", "stop_on": ["received_at"]}
  ]$j$::jsonb),
  '{fees,invoice_due_days}', '35'::jsonb),
  '{billing}', coalesce(config->'billing', '{}'::jsonb) || '{"due_days": 35}'::jsonb),  -- billing may not exist yet
  '{outcome_groups}', $j$[
    {"group": "Open Cases", "outcomes": ["Initial Review Pending", "TBD - In Process"]},
    {"group": "Closed Without Review", "outcomes": ["Ineligible - Closed", "Withdrawn", "Dismissed", "Plan Opt-Out", "Refunded"]},
    {"group": "Decided With Determination", "outcomes": ["Default Award", "Full Award", "Partial Award", "No Award"]},
    {"group": "Administrative Closure", "outcomes": ["Other", "Costs Resolved Prior to Final Determination (Pre-Settlement)"]}
  ]$j$::jsonb),
  '{fees,payment_timeframe_days}', '35'::jsonb)
WHERE tenant = 'fl';

SELECT c->>'name' AS clock, c->>'days' AS days, coalesce(c->>'max_days','') AS max, c->>'day_type' AS counted,
       c->>'basis' AS from_date, coalesce(c->>'target','') AS target
  FROM public.program_rules, jsonb_array_elements(config->'clocks') c WHERE tenant = 'fl';
SELECT config->'fees'->>'invoice_due_days' AS invoice_due_days, config->'billing'->>'due_days' AS billing_due_days
  FROM public.program_rules WHERE tenant = 'fl';
COMMIT;
