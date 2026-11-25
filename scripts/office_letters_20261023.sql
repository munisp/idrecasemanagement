-- office_letters_20261023.sql — the Partial Acceptance letter (the office's own template, lettergen
-- assets/office/partial_acceptance.docx) joins the letters a case can generate. Safe to run twice.
--   psql -d <db> -v ON_ERROR_STOP=1 -f office_letters_20261023.sql
\set ON_ERROR_STOP on
BEGIN;
UPDATE public.program_rules
   SET config = jsonb_set(config, '{letter_templates}', coalesce(config->'letter_templates','[]'::jsonb) ||
       '[{"key":"partial_acceptance_letter","subject":"Results of Preliminary Review {case_number}","filename":"Partial Acceptance Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"}]'::jsonb)
 WHERE tenant = 'fl'
   AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(config->'letter_templates','[]'::jsonb)) t
                    WHERE t->>'key' = 'partial_acceptance_letter');
-- The email that sends a review invoice to the party billed, drafted when the invoice passes QA.
UPDATE public.program_rules
   SET config = jsonb_set(config, '{correspondence,templates}', coalesce(config->'correspondence'->'templates','[]'::jsonb) ||
       jsonb_build_array(jsonb_build_object(
         'key','invoice', 'subject','{case_number} Invoice {invoice_number}', 'to', jsonb_build_array('billed_party'),
         'cc', '[]'::jsonb, 'qa_role','PM',
         'body', E'Hello,\n\nPlease find invoice {invoice_number} for case {case_number}, for {invoice_amount}, at the secure link below.\n{download_link}\n\nPayment is due within 35 days. A $50 late charge will be added if payment is not received within 35 days, and an additional $50 late charge will accrue every 30 days until the invoice is paid. {org_short} only accepts ACH payment for FL AHCA dispute invoices; the remit-to details are on the invoice. Please email a copy of the remittance advice to {contact_email} with case number {case_number}.\n\nIf you have any questions regarding this correspondence, please email {contact_email} or call {contact_phone}.')))
 WHERE tenant = 'fl'
   AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(config->'correspondence'->'templates','[]'::jsonb)) t
                    WHERE t->>'key' = 'invoice');
SELECT t->>'key' AS letter FROM public.program_rules, jsonb_array_elements(config->'letter_templates') t WHERE tenant = 'fl';
COMMIT;
