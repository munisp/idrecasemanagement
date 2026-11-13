-- fee_in_templates_20261113.sql — the initial fee in filer emails comes from the admin's fee schedule, not typed text.
-- The submission instructions said "$123.59"; the schedule (Configuration > Fees, per AHCA Contract HQA028 Exhibit
-- II-A) charges $123.60 in 2026 plus a card fee. Templates now say {initial_fee}, {initial_fee_by_card} and
-- {card_fee}, filled from the schedule in force (letter_vars.go). Safe to run twice; touches only that sentence.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL app.config_migration = 'on';
UPDATE public.program_rules
   SET config = replace(config::text,
         'requires the $123.59 Initial Eligibility Determination Review Fee to be paid in full.',
         'requires the {initial_fee} Initial Eligibility Determination Review Fee to be paid in full ({initial_fee_by_card} when paid by card, which adds a {card_fee} card processing fee).')::jsonb
 WHERE config::text LIKE '%requires the $123.59 Initial Eligibility Determination Review Fee to be paid in full.%';
-- The intake checklist line named the old amount too; it now points at the schedule.
UPDATE public.program_rules
   SET config = replace(config::text, 'initial review fee paid ($123.59)', 'initial review fee paid (the amount in Configuration > Fees)')::jsonb
 WHERE config::text LIKE '%initial review fee paid ($123.59)%';
-- Each case keeps its own copy of the checklist (public.case_checklists, matched by text), so rename those items
-- too, keeping who ticked them; drop a fresh duplicate a case may have been given since the rename.
DELETE FROM public.case_checklists n
 WHERE n.item = 'initial review fee paid (the amount in Configuration > Fees)' AND NOT n.done
   AND EXISTS (SELECT 1 FROM public.case_checklists o WHERE o.tenant=n.tenant AND o.case_id=n.case_id
               AND o.stage=n.stage AND o.item='initial review fee paid ($123.59)');
UPDATE public.case_checklists SET item = 'initial review fee paid (the amount in Configuration > Fees)'
 WHERE item = 'initial review fee paid ($123.59)';
SELECT tenant, (config::text LIKE '%{initial_fee}%') AS uses_schedule, (config::text LIKE '%$123.59%') AS old_amount_left FROM public.program_rules WHERE config::text LIKE '%Initial Eligibility Determination Review Fee%';
SELECT count(*) AS checklist_items_old_text FROM public.case_checklists WHERE item LIKE '%$123.59%';
COMMIT;
