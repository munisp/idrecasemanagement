# Program Rules — Per-State Customization

The platform runs many dispute programs on one codebase. A tenant with a row in
`public.program_rules` runs its own program; a tenant without one runs the
built-in federal NSA program (45 CFR Part 149). **States customize by editing
config, not code.** FL AHCA CDR is the first seeded profile
(`scripts/program-rules.sql`, tenant `fl`).

## Config shape (`config jsonb`)

| Key | Drives | Gap closed |
|---|---|---|
| `case_number.pattern`, `seq_pad` | Auto-generated case numbers (`FL{yy}-{seq}` → `FL26-042`) when intake omits a number | G11 |
| `statuses.internal[]`, `statuses.agency[]` | Dual status model — internal workflow status + agency-facing status, validated against these lists | G5 |
| `clocks[]` | Generic date-driven clock engine: `{name,label,basis,days,day_type,cite,breach,follow_ups[]}`. Basis is a key in `cases.program_dates` (record via `POST /cases/{id}/program-date`); `received_at` falls back to `opened_at`. Calendar + business days supported | G1 |
| `eligibility` | Threshold matrix (`provider_type` × `contracted` → `min_cents`), `filing_window_months`, `ineligibility_reasons[]`, `proof_of_timeliness[]`. Computed by `POST /cases/{id}/eligibility`, stored in `eligibility_reviews` with evidence | G2 |
| `fees` | `initial_fee_cents`, `refund_window_days`, `invoice_due_days` | G8, G12 |
| `escalation` | `amount_trigger_cents` — auto-escalates large disputes to `route_role` at initiation and after claims import | G6 |
| `correspondence.templates[]` | Template key, subject (with `{case_number}` merge), to/cc party policy, optional `qa_role` gate | G3, G15 |
| `deliverables[]` | Contract report schedule: `weekly:MONDAY`, `monthly:10`, `case:EVENT` | G7 |
| `notes_streams[]` | Named note streams for the timeline | G5 |

## API surface

- `GET /program` — tenant's active config (portal renders program-specific labels from this)
- `POST /cases/{id}/program-date` `{key, value}` — record a clock-basis event
- `POST /cases/{id}/status` — dual internal/agency status update (validated against config)
- `POST /cases/{id}/eligibility` — threshold + filing-window + flags computation; `HOLD_AOR` routes to attorney
- `POST /cases/{id}/correspondence` — draft/send a template; templates with `qa_role` enter the QA queue (PENDING) and are sent only on approval (G4)
- `GET /qa`, `POST /qa/{id}/decision` — QA gate queue; approval logs to `correspondence_log`
- `POST /cases/{id}/share-links` — tokenized upload/download links with expiry + use limits (G9 ShareFile replacement); party-facing landing at `GET /api/share/{token}` (no login)
- `POST /cases/{id}/invoices`, `POST /invoices/{id}/settle` — dual-party receivables, invoice number = case number, PAY/REFUND/VOID with remittance ref (G8)
- `POST /cases/{id}/claims` — bulk claim-line import, max 5000/batch, idempotent per claim number (G10, 21k-claim cases)
- `POST /intake`, `POST /intake/{id}/advance` — pre-case intake with refund-window enforcement (G12)
- `GET /deliverables`, `POST /deliverables` — schedule with computed next-due + delivery history (G7)
- `POST /cases/{id}/opt-out` — plan opt-out adjudication with rationale (G14)

## Case columns added (per tenant schema)

`internal_status`, `agency_status`, `disputed_amount_cents`, `num_claims`,
`program_dates jsonb` — all program-scoped, so federal tenants are unaffected.

## Deferred (Phase 0 dependencies)

- **G13 migration**: needs the live PLUM DB schema export to map legacy rows
  into `cases.program_dates` / `case_claims` / `correspondence_log`.
- Remaining open questions from the gap analysis (AHCA clarifications) are
  tracked in `FL-AHCA-business-case-and-gap-analysis.md`.

## Adding a new state

1. `INSERT INTO public.program_rules (tenant, program, config) VALUES ('<st>', '<program>', '<json>')`
   — copy the FL seed and edit.
2. No deploy required; `loadProgram` reads config per request.
