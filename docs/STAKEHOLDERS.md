# Stakeholders & Journeys — NSA Federal IDRE Platform

Every human and system actor, their onboarding path, day-to-day journey, and the
exact backend + frontend surfaces that serve them. Roles are Keycloak realm
roles; tenancy is the Keycloak `/tenant/<state>` group claim.

---

## 1. Stakeholder registry

| # | Stakeholder | Role(s) | Tenant scope | Onboarding type |
|---|---|---|---|---|
| 1 | **Platform Operator** (us / host org) | `PLATFORM_ADMIN` | all 50 | internal sponsorship |
| 2 | **CMS / Federal Admin** | `FEDERAL_ADMIN` | all 50 | `ADMIN_STAFF` application, approved by existing FEDERAL_ADMIN |
| 3 | **State Auditor** (state DOI staff) | `STATE_AUDITOR` | read-only, all 50 states (writes rejected even in home tenant; see tenancy middleware) | `STATE_AUDITOR_ORG` + state credential letter |
| 4 | **Case Manager** (operations staff) | `CASE_MANAGER` | assigned state(s) | `ADMIN_STAFF` + sponsoring manager |
| 5 | **IDRE Entity** (certified dispute entity) + its **Arbitrators** | `ARBITRATOR` | states it serves | `IDRE_ENTITY` — CMS cert #, fee schedule, COI, banking; FEDERAL_ADMIN approval |
| 6 | **Provider / Facility / Air-ambulance org** + billing staff | `PARTY` (provider) | state(s) of service | `PROVIDER_ORG` — NPI (NPPES-verified), TIN, W-9, state license |
| 7 | **Payer / Health plan / TPA org** + claims staff | `PARTY` (payer) | state(s) of operation | `PAYER_ORG` — NAIC code, state DOI license, W-9 |
| 8 | **Voice AI platform** (getline.ai-style) | service account `voice-integration` | per-tenant API key | API key + webhook secret issued by CASE_MANAGER |
| 9 | **State SSL registries** (external systems) | none (data source) | per-tenant | Fluvio connector config |
| 10 | **Members/patients** (indirect) | none | — | never log in; represented via party orgs and call-center intake |

---

## 2. Individual stakeholder journeys

### 2.1 Provider org (party) — *"I want my out-of-network claim paid fairly"*
1. **Onboard** (PWA → Onboarding → "Provider organization"): submits legal name, EIN, NPI, W-9 + state license documents → `StakeholderOnboardingWorkflow`: EIN format + **NPPES live lookup (with offline-cache fallback)**, docs analyzed by Docling/VLM pipeline → tenant CASE_MANAGER approves → Keycloak account + invite email.
2. **Initiate dispute**: Dashboard → "New dispute" → enters claim, QPA, service line, uploads EOB/claim docs (encrypted at rest; analysis auto-extracts CPT/QPA and cross-checks).
3. **Open negotiation → IDR**: Temporal workflow runs statutory clocks; portal shows a live timeline with business-day deadlines.
4. **Offer window**: submits sealed offer + justification (AES-256-GCM sealed; even admins can't read it); pays admin + IDRE fee (TigerBeetle pending transfer).
5. **Reveal → determination**: both offers in (or window expiry) → automatic lawful reveal; arbitrator decides; party sees outcome and award.
6. **Payment**: non-prevailing party pays within 30cd; escrow settles; case closes `CLOSED_PAID`.
7. **Voice channel**: calls the support line; the voice agent reads case status/deadlines via `POST /api/voice/tools/*` (no portal login needed).

### 2.2 Payer org — mirror of 2.1, plus batch initiation (batched determinations when criteria met) and QPA defense documents.

### 2.3 IDRE entity / arbitrator — *"I decide disputes I'm certified for, without conflicts"*
1. **Onboard**: `IDRE_ENTITY` application → CMS certification verified against the directory, fee schedule validated against 2026 bands ($425–800 single / $1,125 batched), COI attestation, banking (vault-sealed) → **FEDERAL_ADMIN approval only** → Keycloak `ARBITRATOR` + TigerBeetle settlement accounts.
2. **Assignment**: case managers assign disputes; conflict-of-interest attestation recorded per case before access.
3. **Offer review**: after lawful reveal, `arbitratorView` decrypts offers + justifications.
4. **Determination**: baseball-style — must pick one submitted offer; checklist (12 on-notice elements) enforced in the form; written rationale; 30bd clock tracked.
5. **Compensation**: IDRE fee posts from escrow to the entity's settlement account on determination.

### 2.4 Case manager — *"I keep the pipeline moving and compliant"*
Dashboard workqueues: initiation reviews (eligibility routing incl. SSL), selection facilitation, SLA breach list, onboarding approvals (parties), voice-intake triage, document findings review. Approvals and assignments signal the workflows; every action lands in the hash-chained audit log.

### 2.5 CMS / Federal admin — *"I oversee the national program"*
Cross-tenant read access; approves IDRE entities; manages state SSL configuration matrix; reviews CMS monthly reports (gold zone, generated ≤30bd after month-end); monitors suspension registry and breach analytics.

### 2.6 State auditor — *"I watch my state"*
Read-only over own tenant: cases, SLA compliance, fee flows, SSL routing correctness; OpenSearch dashboards + DataFusion ad-hoc SQL over gold-zone marts.

### 2.7 Platform operator — *"I run the fabric"*
Tenant onboarding (`TenantOnboardingWorkflow`), Kubecost per-tenant chargeback, Wazuh/OpenCTI security posture, key rotation (vault master via KMS), capacity on Temporal/Kafka/TigerBeetle.

### 2.8 Voice AI platform — *"I answer calls and log everything"*
Per-tenant API key; mid-call "tool" invocations (`case-status`, `deadlines`, `intake`); HMAC-signed event webhooks (`call.completed`, transcripts) → `voice_call_logs`/`voice_intake_requests` → Kafka → case-manager triage queue. Outbound milestone calls triggered by workflow activities.

---

## 3. Frontend coverage matrix (PWA + native mobile)

One codebase — `portal/` (framework-free PWA, installable, offline shell) wrapped
by Capacitor for native iOS/Android builds. Every backend surface has a screen:

| Backend surface | PWA screen | Roles |
|---|---|---|
| Keycloak OIDC (PKCE) | Login / silent refresh | all |
| `GET/POST /v1/tenants/{t}/cases*` | Dashboard, Case list, Case detail (timeline, deadlines), New dispute | all (scoped) |
| Temporal signals (`/cases/{id}/signal`) | Action buttons on Case detail (respond, submit offer, mark fees paid…) | PARTY, ARBITRATOR, CASE_MANAGER |
| Offers (sealed submit / arbitrator view) | Offer form (sealed), Determination form (baseball pick + checklist) | PARTY, ARBITRATOR |
| Documents upload/download/analysis | Documents tab on Case detail (upload, analysis status, extracted fields, findings) | all (scoped) |
| Onboarding applications + decisions | Onboarding (self-service submit; review queue with approve/reject) | applicants, CASE_MANAGER, FEDERAL_ADMIN |
| Voice tools/events | Voice console (intake queue, call logs) | CASE_MANAGER |
| CMS monthly report / SLA breaches | Reports | FEDERAL_ADMIN, STATE_AUDITOR |
| DataFusion ad-hoc query | Reports → SQL (auditor) | STATE_AUDITOR |
| Kubecost / tenant admin | Admin → Tenants | PLATFORM_ADMIN |

Native shells: `npx cap add ios && npx cap add android` — see `portal/README.md`.
Push notifications (native) map to the same notifier service the voice triggers use.
