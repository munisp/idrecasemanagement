# Capability Matrix — NSA Federal IDRE Platform vs Premier CRM & Case Management

Feature-for-feature comparison of this platform against the premier CRMs
(Salesforce Sales Cloud, HubSpot, Microsoft Dynamics 365) and the premier case
management platforms (Pega, Appian, ServiceNow CSM, Filevine).

Legend: **✅** shipped · **⚙️** shipped with platform-specific constraint · **🔜** roadmap

## 1. CRM core (vs Salesforce / HubSpot / Dynamics 365)

| Capability | Salesforce | HubSpot | Dynamics | This platform |
|---|---|---|---|---|
| Accounts & contacts | ✅ | ✅ | ✅ | ✅ `accounts`, `contacts`, Account 360 view |
| Leads & lead conversion | ✅ | ✅ | ✅ | ✅ `leads` → convert creates account + contact |
| Lead auto-capture | ✅ web-to-lead | ✅ | ✅ | ✅ voice-to-lead (AI intake) + email-to-lead (`/api/email/inbound`) |
| Activities / timeline | ✅ | ✅ | ✅ | ✅ `case_activities` — calls, emails, documents, offers, system events on one timeline |
| Tasks & reminders | ✅ | ✅ | ✅ | ✅ `tasks`, due dates, one-click complete |
| Notes | ✅ | ✅ | ✅ | ✅ `notes` attached to accounts/cases |
| Global search | ✅ Einstein | ✅ | ✅ Relevance | ✅ cases + accounts + contacts + leads (ILIKE), OpenSearch for deep document search |
| List views / saved filters | ✅ | ✅ | ✅ | ✅ `saved_views` per user, save-current-filter, dropdown on Disputes |
| Custom fields | ✅ | ✅ | ✅ | ✅ JSONB `custom_fields` on accounts/contacts/leads |
| Pipeline / kanban | ✅ drag-drop | ✅ drag-drop | ✅ | ⚙️ kanban board, no drag-to-change — stage transitions are workflow-gated by statute (Temporal), deliberate design |
| Email integration | ✅ Outlook/Gmail sync | ✅ | ✅ Exchange | ⚙️ inbound email-to-case/lead shipped; full mailbox sync 🔜 |
| Calendar | ✅ | ✅ | ✅ | ✅ deadline calendar (statutory offer windows + tasks agenda) |
| Voice / telephony | ✅ Dialer | ✅ calling | ✅ Teams | ✅ inbound AI intake line + outbound click-to-call, transcripts auto-logged to timeline |
| Notifications | ✅ | ✅ | ✅ | ✅ in-app bell with unread badge; SLA-breach broadcast to FEDERAL_ADMIN |
| Reports & dashboards | ✅ report builder | ✅ | ✅ Power BI | ⚙️ statutory reports (SLA, summary, CMS breach) shipped; ad-hoc report builder 🔜 (lakehouse + DataFusion is the foundation) |
| Documents | ✅ Files | ✅ | ✅ SharePoint | ✅ double-blind encrypted vault (AES-256-GCM, per-tenant keys) + Docling/PaddleOCR/VLM analysis — **beyond all three** |
| Mobile | ✅ app | ✅ app | ✅ | ✅ PWA (offline SW) + native iOS/Android via Capacitor |
| Workflow automation | ✅ Flow | ✅ Workflows | ✅ Power Automate | ✅ Temporal durable workflows — stronger: statutory timers survive restarts, exactly-once signals |
| Audit trail | ✅ field history | ✅ | ✅ | ✅ immutable audit topic per tenant (`idre.<st>.audit`), OpenSearch 6-yr retention — 45 CFR 149 aligned |
| Multi-tenant | ❌ single org | ❌ | ❌ | ✅ 50 isolated state tenants, schema-per-tenant + RLS — **differentiator** |
| Payments / ledger | ❌ (CPQ billing) | ❌ | ❌ | ✅ TigerBeetle double-entry escrow per tenant — **differentiator** |

## 2. Case management (vs Pega / Appian / ServiceNow CSM / Filevine)

| Capability | Pega | Appian | ServiceNow | Filevine | This platform |
|---|---|---|---|---|---|
| Case lifecycle orchestration | ✅ | ✅ | ✅ | ✅ | ✅ `IdrCaseWorkflow` — full NSA IDR lifecycle with statutory clocks |
| SLA timers & breach escalation | ✅ | ✅ | ✅ | ✅ | ✅ durable timers; breach → CMS report + FEDERAL_ADMIN escalation + supervisor task (2-day due) |
| Assignment & workload balancing | ✅ | ✅ | ✅ | ✅ | ✅ workload-balanced auto-assignment + manual override by role |
| Case relationships (batch/parent/duplicate) | ✅ | ✅ | ✅ | ✅ | ✅ `case_relationships` (BATCH, PARENT_CHILD, DUPLICATE) + automatic duplicate detection on intake |
| Stage checklists | ✅ | ✅ | ✅ playbooks | ✅ | ✅ per-stage checklists incl. 12-element determination checklist (45 CFR 149.520) |
| Deadline calendar | ✅ | ✅ | ✅ | ✅ calendaring | ✅ statutory offer-window close + task agenda |
| Document generation | ✅ | ✅ | ✅ | ✅ | ✅ letter templates (offer-window notice, determination letter) rendered to the sealed vault |
| e-Signature | ✅ DocuSign | ✅ | ✅ | ✅ | 🔜 roadmap |
| Email-to-case | ✅ | ✅ | ✅ | ✅ | ✅ `/api/email/inbound` — case_number → case timeline, else → lead |
| Omnichannel intake | ✅ | ✅ | ✅ | ✅ | ✅ portal + voice AI + email + Fluvio edge ingestion |
| Business rules engine | ✅ decision tables | ✅ | ✅ | ✅ | ✅ state_config matrix (eligibility, fee bands) + pipeline.yaml doc rules |
| Reporting / analytics | ✅ | ✅ | ✅ | ✅ | ✅ lakehouse (Delta/Parquet, Flink, Spark, DataFusion, Sedona geospatial) + Ray ML |
| Low-code customization | ✅ | ✅ | ✅ | ✅ | 🔜 config-first today (state_config, pipeline.yaml); visual designer roadmap |

## 3. Capabilities no competitor offers

- **Double-blind offer vault** — cryptographically sealed offers, lawful-reveal only after Temporal verifies both parties submitted (NSA double-blind requirement).
- **Statutory clock engine** — 30bd negotiation, 4bd initiation, 10bd offer window, 30bd determination, 30cd payment as durable workflow timers, not calendar reminders.
- **CMS breach reporting** — automatic monthly `CmsMonthlyReportWorkflow` + real-time breach flags.
- **50-tenant federal isolation** — schema-per-tenant, per-tenant crypto keys, per-tenant Kafka topics, Keycloak tenant groups.
- **Integrated doc intelligence** — Docling → PaddleOCR → seal/stamp detection → VLM extraction → case-record validation.
- **Escrow settlement** — TigerBeetle double-entry ledger, pending/post/void admin-fee transfers, Mojaloop settlement patterns.

## 4. Honest roadmap (not yet shipped)

1. Full mailbox/calendar sync (inbound email done; two-way sync pending)
2. Visual report builder over the lakehouse
3. Kanban drag-to-change (with workflow-gated transition validation)
4. e-Signature integration
5. Visual workflow designer (Temporal workflows are code today)
