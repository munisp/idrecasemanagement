-- Program rules + FL AHCA extension tables.
-- One row per tenant in public.program_rules; the JSONB config drives clocks,
-- statuses, eligibility, numbering, fees, escalation, correspondence, and
-- deliverables. A state customizes its program by editing config — no code.

CREATE TABLE IF NOT EXISTS public.program_rules (
    tenant     text PRIMARY KEY,
    program    text NOT NULL,
    config     jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Program-specific dates/statuses ride on the case row.
ALTER TABLE tenant_tx.cases ADD COLUMN IF NOT EXISTS internal_status text;
-- All 50 tenant schemas get the same columns -- case-api-go's listCases/
-- getCase select internal_status/agency_status unconditionally (not just
-- for programmed tenants), so any state missing these columns 500s on
-- every case list/detail call. Previously hardcoded to ['tx','ca','ny','fl']
-- despite the comment above claiming "all tenant schemas" -- confirmed live
-- that the other 46 states (e.g. 'ga') never got these columns and 500'd on
-- GET /v1/tenants/ga/cases. Must match init-schemas.sql's provisioning list.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['al','ak','az','ar','ca','co','ct','de','fl','ga','hi','id','il','in','ia',
                           'ks','ky','la','me','md','ma','mi','mn','ms','mo','mt','ne','nv','nh','nj',
                           'nm','ny','nc','nd','oh','ok','or','pa','ri','sc','sd','tn','tx','ut','vt',
                           'va','wa','wv','wi','wy'] LOOP
    -- %I must quote the whole schema name, not just the state code: with the
    -- quoting split as tenant_%I, a reserved word like 'in' or 'or' becomes
    -- tenant_"in" -- an unquoted prefix glued to a quoted suffix, which is a
    -- syntax error that aborts this entire DO block (rolling back every
    -- state processed earlier in the same loop). Confirmed live.
    EXECUTE format('ALTER TABLE %I.cases
        ADD COLUMN IF NOT EXISTS internal_status text,
        ADD COLUMN IF NOT EXISTS agency_status text,
        ADD COLUMN IF NOT EXISTS disputed_amount_cents bigint,
        ADD COLUMN IF NOT EXISTS num_claims int,
        ADD COLUMN IF NOT EXISTS program_dates jsonb NOT NULL DEFAULT ''{}''::jsonb', 'tenant_' || t);
  END LOOP;
END $$;

-- QA gate: every outbound letter/email is a record with approver + decision.
CREATE TABLE IF NOT EXISTS public.qa_reviews (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant       text NOT NULL,
    case_id      text NOT NULL,
    artifact     text NOT NULL,        -- template key, e.g. acceptance_provider
    channel      text NOT NULL DEFAULT 'email',
    subject      text,
    body         text NOT NULL,        -- rendered merge output
    to_recipients   jsonb NOT NULL DEFAULT '[]',
    cc_recipients   jsonb NOT NULL DEFAULT '[]',
    status       text NOT NULL DEFAULT 'PENDING',  -- PENDING|APPROVED|REJECTED|SENT
    drafted_by   text NOT NULL,
    reviewed_by  text,
    review_note  text,
    reviewed_at  timestamptz,
    sent_at      timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS qa_reviews_case ON public.qa_reviews (tenant, case_id);
CREATE INDEX IF NOT EXISTS qa_reviews_queue ON public.qa_reviews (tenant, status);
-- drafted_by is a free-text display-name string ("copilot:... (requested by
-- Jane Doe)"), not a Keycloak sub -- there was no machine-readable way to
-- notify the person who ASKED for a draft once a second reviewer decides
-- its fate later (approve/reject/send-failure), often in a different
-- session entirely. requested_by_sub closes that: qaDecision notifies this
-- user directly on outcome instead of the requester only ever finding out
-- by checking back on the QA queue themselves.
ALTER TABLE public.qa_reviews ADD COLUMN IF NOT EXISTS requested_by_sub text;

-- Correspondence log: what actually went out (replaces the Excel Inquiries Log).
CREATE TABLE IF NOT EXISTS public.correspondence_log (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    direction  text NOT NULL DEFAULT 'OUT',  -- OUT|IN
    channel    text NOT NULL DEFAULT 'email',
    template   text,
    recipients jsonb NOT NULL DEFAULT '{}',  -- {to:[],cc:[]}
    subject    text,
    body       text,
    sent_by    text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS correspondence_case ON public.correspondence_log (tenant, case_id);

-- Invoices: one invoice number (= case number) can carry two receivables.
CREATE TABLE IF NOT EXISTS public.invoices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant       text NOT NULL,
    case_id      text NOT NULL,
    invoice_no   text NOT NULL,               -- equals case_number per program convention
    party        text NOT NULL,               -- HEALTH_PLAN|PROVIDER
    kind         text NOT NULL DEFAULT 'FULL_REVIEW', -- INITIAL_FEE|FULL_REVIEW|DEFAULT
    amount_cents bigint NOT NULL,
    status       text NOT NULL DEFAULT 'OPEN',-- OPEN|PAID|REFUNDED|VOID
    due_date     date,
    paid_at      date,
    remittance_ref text,                      -- RA/ERA reference
    created_by   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, case_id, party, kind)
);

-- Claim-level sub-records for large-volume disputes (G10).
CREATE TABLE IF NOT EXISTS public.case_claims (
    id           bigserial PRIMARY KEY,
    tenant       text NOT NULL,
    case_id      text NOT NULL,
    claim_number text NOT NULL,
    cpt          text,
    billed_cents bigint,
    paid_cents   bigint,
    status       text NOT NULL DEFAULT 'SUBMITTED',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, case_id, claim_number)
);
-- Capitol Bridge Large Volume Claims Dispute Submission Policy v01.01.2026,
-- item 4: the required spreadsheet columns beyond what G10 originally
-- tracked. Self-healing ADD COLUMN (see the init-schemas.sql comment on this
-- exact pattern) so an already-provisioned table picks these up too.
ALTER TABLE public.case_claims
    ADD COLUMN IF NOT EXISTS patient_first_name text,
    ADD COLUMN IF NOT EXISTS patient_last_name text,
    ADD COLUMN IF NOT EXISTS type_of_service text,
    ADD COLUMN IF NOT EXISTS denial_reason text,
    ADD COLUMN IF NOT EXISTS date_of_service date,
    ADD COLUMN IF NOT EXISTS disputed_cents bigint,
    ADD COLUMN IF NOT EXISTS date_claim_submitted date,
    ADD COLUMN IF NOT EXISTS date_of_denial date,
    ADD COLUMN IF NOT EXISTS date_of_final_determination date,
    ADD COLUMN IF NOT EXISTS provider_name text,
    ADD COLUMN IF NOT EXISTS facility_name text,
    ADD COLUMN IF NOT EXISTS evidence_location text;
CREATE INDEX IF NOT EXISTS case_claims_case ON public.case_claims (tenant, case_id);

-- Pre-case intake: instructions requested before any case exists (G9/G12).
CREATE TABLE IF NOT EXISTS public.intake_requests (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    email       text NOT NULL,
    contact_name text,
    org         text,
    status      text NOT NULL DEFAULT 'INSTRUCTED', -- INSTRUCTED|DOCS_RECEIVED|PAID|CONVERTED|CLOSED_REFUNDED
    case_id     text,                               -- set at conversion
    outreach_at timestamptz,                        -- refund clock basis
    notes       text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
-- AHCA answers 2026: intake carries the filing party type (a health plan CAN
-- file, though rare) and the packet-complete timestamp that anchors the
-- 10-day initial-review clock (the clock runs from COMPLETE packet receipt,
-- not payment or submission). Backfilled for pre-existing databases.
ALTER TABLE public.intake_requests
    ADD COLUMN IF NOT EXISTS filing_party_type text NOT NULL DEFAULT 'PROVIDER', -- PROVIDER|HEALTH_PLAN
    ADD COLUMN IF NOT EXISTS packet_complete_at timestamptz;                     -- null = awaiting documents
-- Federal NSA intakes price on QPA from the very first contact (it drives
-- eligibility/threshold logic downstream); the form sent it but nothing
-- stored it -- every legacy intake silently dropped the amount. Programmed
-- tenants never use this column (their amount lands on the real case
-- directly via startAhcaCase, not an intake_requests row at all).
ALTER TABLE public.intake_requests
    ADD COLUMN IF NOT EXISTS qpa_cents bigint;

-- Rule-engine audit trail (append-only by design — no UPDATE/DELETE path).
-- Every rules write stores the full before/after config plus actor + note.
CREATE TABLE IF NOT EXISTS public.rule_changes (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    changed_by text NOT NULL,              -- keycloak sub of the admin
    note       text,                       -- change rationale from the UI
    before     jsonb NOT NULL,
    after      jsonb NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS rule_changes_tenant ON public.rule_changes (tenant, id DESC);

-- Rule quarantine for onboarding uploads: doc.upload rules fire on the
-- pre-case path too, and block_request needs somewhere to land. doc-intel
-- skips documents in this state (is_blocked pre-flight in main.py).
ALTER TABLE public.application_documents
    ADD COLUMN IF NOT EXISTS analysis_status text NOT NULL DEFAULT 'QUEUED'; -- QUEUED|BLOCKED|...

-- Tokenized party document links (ShareFile replacement).
CREATE TABLE IF NOT EXISTS public.share_links (
    token      text PRIMARY KEY,              -- URL-safe random
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    kind       text NOT NULL,                 -- upload|download
    object_key text,                          -- download: specific doc
    expires_at timestamptz NOT NULL,
    uses       int NOT NULL DEFAULT 0,
    max_uses   int NOT NULL DEFAULT 1,
    files_used int NOT NULL DEFAULT 0,        -- abuse budgets (ShareBox)
    max_files  int NOT NULL DEFAULT 5,
    bytes_used bigint NOT NULL DEFAULT 0,
    max_bytes  bigint NOT NULL DEFAULT 524288000, -- 500MB per link
    created_by text,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Eligibility results: computed per case from program rules + evidence.
CREATE TABLE IF NOT EXISTS public.eligibility_reviews (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    result     text NOT NULL,                 -- ELIGIBLE|INELIGIBLE|HOLD_AOR
    reason     text,                          -- reason code from program config
    evidence   jsonb NOT NULL DEFAULT '{}',   -- per-criterion findings
    decided_by text,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Plan opt-out adjudication (G14).
CREATE TABLE IF NOT EXISTS public.opt_out_decisions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    eligible   boolean NOT NULL,
    rationale  text NOT NULL,
    decided_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Phone call tracker + inquiries log: the workflow narrative calls for a
-- running record of every call a Reviewer makes/receives and every inbound
-- inquiry, independent of formal templated correspondence
-- (public.correspondence_log, which only covers the 16 sent templates).
-- case_id is nullable: the source docs' own PLUM CRM/ShareFile process names
-- it as a known gap that calls/inquiries made before a case exists never get
-- linked to the case once one is opened. requester_email is carried so
-- publicAhcaIntake can backfill case_id onto any pre-case rows when the real
-- case is created (see ahca_intake.go).
CREATE TABLE IF NOT EXISTS public.call_log (
    id              bigserial PRIMARY KEY,
    tenant          text NOT NULL,
    case_id         text,
    requester_email text,
    direction       text NOT NULL,          -- INBOUND|OUTBOUND
    reviewer        text NOT NULL,          -- staff principal on the call
    phone           text NOT NULL,
    summary         text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS call_log_case ON public.call_log (tenant, case_id);
CREATE INDEX IF NOT EXISTS call_log_email ON public.call_log (tenant, requester_email);

CREATE TABLE IF NOT EXISTS public.inquiry_log (
    id              bigserial PRIMARY KEY,
    tenant          text NOT NULL,
    case_id         text,
    requester_email text,
    method          text NOT NULL,          -- EMAIL|PHONE_CALL
    inquiry_type    text NOT NULL,          -- GENERAL_QUESTION|GENERAL_INQUIRY|SUBMISSION_DOCUMENTS
    detail          text,
    logged_by       text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS inquiry_log_case ON public.inquiry_log (tenant, case_id);
CREATE INDEX IF NOT EXISTS inquiry_log_email ON public.inquiry_log (tenant, requester_email);

-- Contract deliverables calendar (G7 second half).
CREATE TABLE IF NOT EXISTS public.deliverables (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    name       text NOT NULL,
    contract_ref text,
    due_rule   text NOT NULL,                 -- e.g. weekly:MONDAY | monthly:10 | case:AGENCY_RECOMMENDATION
    case_id    text,                          -- set for per-case deliverables
    due_date   date,
    status     text NOT NULL DEFAULT 'OPEN',  -- OPEN|DELIVERED|LATE
    delivered_at timestamptz,
    UNIQUE (tenant, name, case_id)
);

-- ---------------------------------------------------------------------------
-- Seed: Florida AHCA CDR program profile.
-- "Closed-Refunded" added to statuses.internal: the Refund Email Notification
-- template's own subject line is "FL CDR Case {case_number} Closed-Refunded",
-- but that exact status string was missing from the vocabulary below --
-- setDualStatus would have 400'd on it the first time the refund path (fee
-- paid, packet never completed within the 7-day window) actually ran.
--
-- Status lifecycle notes (moved out of config->field_schema below -- three
-- entries there held plain strings/notes instead of the []string allowlists
-- every other field_schema key holds, which broke the Go ProgramConfig
-- struct's json.Unmarshal with no error surfaced to the caller: loadProgram
-- silently returned nil, and getProgram's nil-fallback made every FL
-- request look like a plain federal NSA tenant. Confirmed live.):
-- a case is only ever CLOSED via Plan Opt-Out, Ineligible, Dismissed, or
-- Withdrawn. A completed case is NOT closed -- it rests at 'Decided -
-- Invoice Paid'. The 60-day Agency clock never pauses (holds/RFIs/estimate
-- window included). The 10-day initial review runs from complete-packet
-- receipt. RFI may ride with the acceptance letter, but documentation not
-- received by day 13 => Ineligible. After plan notification, internal
-- status reads 'Plan Notification Packet Issued'.
-- ---------------------------------------------------------------------------
INSERT INTO public.program_rules (tenant, program, config) VALUES ('fl', 'FL AHCA CDR', $$
{
  "case_number": {"pattern": "FL{yy}-{seq}", "seq_pad": 3},
  "statuses": {
    "internal": ["Initial Review Pending","QA Initial Review","Provider Acceptance Letter Issued","Provider Closure Letter Issued","RFI Pending Provider Response","QA Letter","Plan Notification Packet Issued","Review In Progress","RFI Pending Plan Response","Plan - No Response","Plan Opt-Out","Provider - Withdrawal","Hold","QA Final Determination","Determination sent to FL","Final Order Issued","Decided - Invoice Paid","Dismissed","Withdrawn","Ineligible"],
    "agency": ["Pending Initial Review","Awaiting Provider Response","Awaiting Plan Response","Under Review","Decided","Closed","Other"]
  },
  "clocks": [
    {"name":"AGENCY_RECOMMENDATION","label":"Recommendation to the Agency","basis":"received_at","days":60,"day_type":"calendar","cite":"AHCA CDR contract §2.3.3","breach":"Contract SLA breach"},
    {"name":"INITIAL_REVIEW","label":"Initial eligibility review","basis":"packet_complete_at","days":10,"day_type":"calendar","cite":"Contract §2.3.4","breach":"Internal SLA breach"},
    {"name":"PROVIDER_PERMISSION","label":"Provider permission after estimate","basis":"estimate_sent_at","days":15,"day_type":"calendar","follow_ups":[{"day":13,"action":"call_and_reply_all"}],"breach":"Dismissal"},
    {"name":"RFI_RESPONSE","label":"RFI / additional documentation","basis":"rfi_sent_at","days":15,"day_type":"calendar","follow_ups":[{"day":15,"action":"timeframe_lapsed_email_and_call"}],"breach":"Case closed"},
    {"name":"PLAN_RESPONSE","label":"Health Plan response","basis":"plan_notified_at","days":15,"day_type":"calendar","follow_ups":[{"day":13,"action":"medicaid_ahca_notify"},{"day":14,"action":"call_and_email"},{"day":15,"action":"call_again"}],"breach":"Default determination for provider"},
    {"name":"REFUND_WINDOW","label":"Complete packet after payment","basis":"outreach_at","days":7,"day_type":"calendar","breach":"Close and refund"},
    {"name":"WITHDRAWAL_LETTER","label":"Withdrawal letter","basis":"withdrawal_requested_at","days":1,"day_type":"business","breach":"Target missed"},
    {"name":"FILING_ELIGIBILITY","label":"Filing window from final determination","basis":"final_determination_at","days":365,"day_type":"calendar","breach":"Ineligible — no extensions"}
  ],
  "eligibility": {
    "thresholds": [
      {"provider_type":"hospital_inpatient","contracted":true,"min_cents":2500000},
      {"provider_type":"hospital_inpatient","contracted":false,"min_cents":1000000},
      {"provider_type":"hospital_outpatient","contracted":true,"min_cents":1000000},
      {"provider_type":"hospital_outpatient","contracted":false,"min_cents":300000},
      {"provider_type":"physician_dentist","min_cents":50000},
      {"provider_type":"rural_hospital","min_cents":0},
      {"provider_type":"other","min_cents":0}
    ],
    "filing_window_months": 12,
    "proof_of_timeliness": ["last_eob_date","service_date","appeal_documents"],
    "ineligibility_reasons": ["late_payment_only","interest_only","medicare_grievance","plan_not_fl_regulated","provider_not_fl_licensed","medicaid_fair_hearing","pending_court_action","over_12_months","pre_2000_binding_process","below_threshold","internal_process_not_exhausted"]
  },
  "fees": {"initial_fee_cents": 12359, "refund_window_days": 7, "invoice_due_days": null, "invoice_number_equals_case_number": true},
  "escalation": {"amount_trigger_cents": 100000000, "route_role": "PM", "reasons": ["fraud_waste_abuse","over_1m"]},
  "notes_streams": ["internal","coder","clinical","legal","external_agency"],
  "correspondence": {
    "agency_recipients": [],
    "templates": [
      {"key":"submission_instructions","subject":"Florida Claims Dispute Submission Process: FL AHCA","to":["requester"],"cc":[]},
      {"key":"estimate_cost","subject":"Full Review Estimate {case_number}: FL AHCA","to":["filing_party"],"cc":["agency"]},
      {"key":"estimate_followup","subject":"URGENT: Response Timeframe Lapsed — {case_number}","to":["filing_party"],"cc":["agency"],"thread":true},
      {"key":"acceptance","subject":"Results of Preliminary Review {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"]},
      {"key":"ineligible","subject":"Results of Preliminary Review {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"]},
      {"key":"dismissal","subject":"Dismissal {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"],"qa_role":"ATTORNEY"},
      {"key":"withdrawal","subject":"Withdrawal Request {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"]},
      {"key":"plan_notification","subject":"Notification of Claims Dispute {case_number}: FL AHCA","to":["health_plan"],"cc":["agency","provider"]},
      {"key":"plan_opt_out","subject":"Plan Opt-out {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"]},
      {"key":"final_order","subject":"Full Review Complete {case_number}: FL AHCA","to":["agency"],"cc":["pm"]},
      {"key":"final_order_rationale_copy","subject":"Copy of Final Order Rationale {case_number}","to":["all_parties"],"cc":["agency"]},
      {"key":"rfi","subject":"Request for Additional Documentation: FL AHCA {case_number}","to":["provider_or_plan"],"cc":[]},
      {"key":"documentation_not_received","subject":"Response Timeframe Lapsed — Additional Documentation Not Received {case_number}","to":["filing_party"],"cc":["agency"],"thread":true},
      {"key":"refund","subject":"FL CDR Case {case_number} Closed-Refunded","to":["filing_party"],"cc":[]},
      {"key":"payment_reminder","subject":"{case_number} Invoice - Payment Due Reminder","to":["billed_party"],"cc":[]},
      {"key":"past_due_reminder","subject":"{case_number} Past Due Payment Follow up","to":["billed_party"],"cc":[]},
      {"key":"plan_docs_notice","subject":"Additional Documentation from Health Plan – {case_number}","to":["provider"],"cc":[]}
    ]
  },
  "letter_templates": [
    {"key":"acceptance_letter","subject":"Results of Preliminary Review {case_number}","filename":"Acceptance Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"ineligible_letter","subject":"Results of Preliminary Review {case_number}","filename":"Ineligible Dispute Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"dismissal_letter","subject":"Dismissal {case_number}","filename":"Dismissal Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"withdrawal_letter","subject":"Withdrawal Request {case_number}","filename":"Withdrawal Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"plan_notification_letter","subject":"Notification of Claims Dispute {case_number}","filename":"Health Plan Notification {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"plan_optout_letter","subject":"Plan Opt-out {case_number}","filename":"Plan Opt-out Letter {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"rfi_letter","subject":"Request for Additional Documentation {case_number}","filename":"Request for Additional Information {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"cover_letter_final_notice","subject":"Full Review Complete {case_number}","filename":"Cover Letter {case_number}.pdf","folder":"DETERMINATION"},
    {"key":"final_order_rationale","subject":"Full Review Complete {case_number}","filename":"Review Rationale and Letter {case_number}.pdf","folder":"DETERMINATION"},
    {"key":"default_determination","subject":"Default Determination {case_number}","filename":"Default Determination {case_number}.pdf","folder":"DETERMINATION"},
    {"key":"fl_plan_packet","subject":"Health Plan Response Form {case_number}","filename":"FL Plan Packet {case_number}.pdf","folder":"CORRESPONDENCE"}
  ],
  "deliverables": [
    {"name":"Weekly report","contract_ref":"2.4.2","due_rule":"weekly:MONDAY"},
    {"name":"Monthly report","contract_ref":"2.4.3","due_rule":"monthly:10"},
    {"name":"Ad hoc report","contract_ref":"2.4.4","due_rule":"on_request:10bd"},
    {"name":"Fee schedule / cost estimate","contract_ref":"2.3.2","due_rule":"case:INITIAL_REVIEW"},
    {"name":"Rationale per determination","contract_ref":"2.3.3","due_rule":"case:AGENCY_RECOMMENDATION"},
    {"name":"Invoice per determination","contract_ref":"2.3.3","due_rule":"case:AGENCY_RECOMMENDATION"},
    {"name":"Cover letter per determination","contract_ref":"2.3.3","due_rule":"case:AGENCY_RECOMMENDATION"},
    {"name":"Case acceptance letter","contract_ref":"2.3.4","due_rule":"case:INITIAL_REVIEW"},
    {"name":"Closure letter (withdrawal/opt-out/dismissal)","contract_ref":"2.3.5","due_rule":"case:AGENCY_RECOMMENDATION"},
    {"name":"Policies & procedures: criminal background screening","contract_ref":"K.7","due_rule":"contract:execution+30d"},
    {"name":"Disaster recovery plan","contract_ref":"11","due_rule":"contract:effective-30d"},
    {"name":"Emergency operations plan","contract_ref":"2.2.7","due_rule":"contract:standing"},
    {"name":"Transition: contract documentation","contract_ref":"8.2","due_rule":"contract:end+60d"},
    {"name":"Transition cooperation agreement","contract_ref":"8.3","due_rule":"contract:end"},
    {"name":"Final report","contract_ref":"2.4.5","due_rule":"contract:end+30bd"}
  ],
  "checklist": {
    "INTAKE": ["requester email + organization on file","initial review fee paid ($123.59)","case number assigned (FL{yy}-{seq})"],
    "ELIGIBILITY": ["disputed amount meets threshold for provider type/contracted status","proof of timely filing attached (last EOB date, service date, or appeal documents)","filing window not expired (12 months from final determination)","AOR valid","no ineligibility reason present (late payment/interest only, Medicare grievance, non-FL-regulated plan, etc.)"],
    "PROVIDER_PERMISSION": ["cost estimate sent to filing party","provider permission received or 15-day window elapsed without response"],
    "PLAN_NOTIFICATION": ["health plan notified of the dispute","plan response received, opted out, or 15-day window elapsed (default determination)"],
    "DETERMINATION": ["Coder plan-type verification complete","clinical review complete if requested from Nurse/Physician","Attorney rationale QA-approved","case outcome recorded","invoice issued to non-prevailing party"],
    "PAYMENT": ["invoice paid or past-due reminder sent","recommendation sent to the Agency within 60 days of receipt"]
  },
  "field_schema": {
    "line_of_business": ["Medicaid","Commercial","Medicare","Medicare Advantage","Marketplace","Other"],
    "disputed_issue": ["Medicaid Medical Necessity","Underpayment","Overpayment","Denial","Other"],
    "out_of_network": ["Yes","No"],
    "case_outcome": ["TBD - case in process","Withdrawn","Dismissed","Provider Default Award","Provider Full Award","Provider Partial Award","Provider No Award","Other"],
    "party_billed": ["Health Plan","Provider","Both Parties","N/A"],
    "withdrawal_dismissed_reason": ["Dismissed-Timeliness eligibility failed","Member plan is not regulated by Florida","Self-Funded Plan","Provider No Response","Withdrawal-Claim Resolved","Other","N/A"],
    "internal_status_terminal": ["Plan Opt-Out","Ineligible","Dismissed","Withdrawn"]
  },
  "volume_rules": {
    "comment": "Capitol Bridge Large Volume Claims Dispute Submission Policy v01.01.2026 — ADOPTED by AHCA. Governs claims-per-dispute volume (NOT repeat filer volume — AHCA confirmed 2026). Applies to disputes with >=100 claims. Non-compliant disputes are found INELIGIBLE; resubmission permitted once the ineligibility reason is cured. Written exemptions by Capitol Bridge only.",
    "source_document": "Capitol Bridge Large Volume Claims Disputes Policy 1.1.2026",
    "enabled": true,
    "large_volume_threshold_claims": 100,
    "single_cpt_per_dispute": true,
    "caps": {
      "no_medical_review": {"max_claims_per_dispute": 500, "max_claims_per_rolling_14_days": 500},
      "medical_review":    {"max_claims_per_dispute": 100, "max_claims_per_rolling_14_days": 100}
    },
    "claim_listing": {
      "format": "xlsx",
      "required_columns": ["claim_number","patient_first_name","patient_last_name","type_of_service","denial_reason","date_of_service","amount_billed","amount_paid","amount_in_dispute","date_claim_submitted","date_of_denial","date_of_final_determination","provider_name","facility_name","evidence_location"],
      "claim_numbers_must_match_eobs": true
    },
    "documentation": {
      "searchable_required": true,
      "filenames_must_match_contents": true,
      "combined_pdf_categories": ["EOBs","Appeal documents","Medical documentation"],
      "unlocked_unrestricted_required": true,
      "disallowed_file_types": ["EDIDATA","BAK"]
    },
    "exemptions": "effective only with prior written approval from Capitol Bridge (flcdr@capitolbridge.com)",
    "noncompliance": {"disposition": "INELIGIBLE", "resubmission_allowed": true}
  },
  "rules": [
    {
      "name": "intake-day13-incomplete",
      "event": "sweep.intake",
      "enabled": true,
      "_basis": "AHCA 2026: RFI may ride with the acceptance letter, but if documentation is not received by the 13th day the case is found incomplete and an ineligibility letter issues",
      "conditions": [
        {"field": "days_since_outreach", "op": "gte", "value": 13},
        {"field": "status", "op": "in", "value": ["INSTRUCTED", "DOCS_RECEIVED"]}
      ],
      "actions": [
        {"type": "set_status", "params": {"status": "INELIGIBLE"}},
        {"type": "notify", "params": {"kind": "SLA_BREACH", "body": "Intake {{id}} ({{email}}) found incomplete at day {{days}} — issue ineligibility letter"}}
      ]
    },
    {
      "name": "doc-unverified-fields-review",
      "event": "doc.analyzed",
      "enabled": true,
      "_basis": "Doc-intel grounding check: extracted fields that cannot be traced to source text must be human-verified before they feed a determination",
      "conditions": [
        {"field": "ungrounded_count", "op": "gt", "value": 0}
      ],
      "actions": [
        {"type": "flag_review", "params": {"reason": "{{ungrounded_count}} unverified field(s) extracted from {{doc_type}} — requires human confirmation"}},
        {"type": "notify", "params": {"kind": "MILESTONE", "body": "Document {{doc_id}} ({{doc_type}}) analyzed with {{ungrounded_count}} unverified field(s) — flagged for review"}}
      ]
    },
    {
      "name": "doc-poor-scan-review",
      "event": "doc.analyzed",
      "enabled": true,
      "_basis": "Barely-legible scans are valid evidence but extraction confidence is degraded; staff should sight the original before relying on extracted values",
      "conditions": [
        {"field": "scan_quality_poor", "op": "eq", "value": true}
      ],
      "actions": [
        {"type": "flag_review", "params": {"reason": "Poor scan quality on {{doc_id}} — verify extracted values against the original"}}
      ]
    }
  ]
}
$$::jsonb)
ON CONFLICT (tenant) DO UPDATE SET config=EXCLUDED.config, program=EXCLUDED.program, updated_at=now();
