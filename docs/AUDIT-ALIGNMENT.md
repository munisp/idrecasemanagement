# Comprehensive Alignment Audit — Frontend ↔ Backend ↔ Data ↔ Middleware

Date: 2026-10-01. Scope: every HTTP route, every portal screen, every database
table, every middleware component named in the platform charter.

**Result:** 46 authenticated routes — 100% have a UI trigger or are deliberate
machine-to-machine endpoints. 4 UI orphans found and fixed. 5 middleware gaps
found and fixed (outbox relay, Redis, Permify, Dapr cron, Fluvio bridge).
GeoLibre integrated as the lakehouse map layer.

---

## 1. Route ↔ UI matrix (authenticated tenant API)

| Route | UI trigger | Status |
|---|---|---|
| POST /cases/initiate | New dispute form (#/new) | ✅ |
| GET /cases | Disputes list + saved views | ✅ |
| GET /cases/{id} | Case detail | ✅ |
| POST /fees/transfer | **Finance "Post fee transfer" button (FIXED — was orphan)** | ✅ fixed |
| POST /cases/{id}/signal | Action buttons: respond, offer, fees paid, selection, determination, **record payment (FIXED)** | ✅ fixed |
| POST /cases/{id}/documents | Upload form on case detail | ✅ |
| GET /cases/{id}/documents | Documents table | ✅ |
| GET …/documents/{docId}/download | download link | ✅ |
| GET …/documents/{docId}/analysis | analysis link | ✅ |
| POST /onboarding/applications | Onboarding application wizard | ✅ |
| GET /onboarding/applications | Onboarding console | ✅ |
| POST …/applications/{appId}/decision | Approve/reject buttons | ✅ |
| GET /voice/intake, /voice/logs | Voice console | ✅ |
| POST /voice/outbound | Voice console dial form | ✅ |
| GET /cases/{id}/activities | Timeline panel | ✅ |
| GET/POST /accounts, /accounts/{id}/360 | Accounts list, New account, Account 360 | ✅ |
| POST /contacts | Add-contact form on Account 360 | ✅ |
| GET /leads, POST /leads/{id}/convert | Leads list + convert button | ✅ |
| GET/POST /tasks, POST /tasks/{id}/complete | Tasks page + done button | ✅ |
| POST /notes | Notes form on Account 360 | ✅ |
| GET /search | Global search box | ✅ |
| POST /cases/{id}/assign | Assign button | ✅ |
| POST /cases/{id}/escalate | **Escalate button (FIXED — api.js had it, no button)** | ✅ fixed |
| POST /cases/relate | **"Link related case" button (FIXED — was read-only)** | ✅ fixed |
| GET …/relationships | Related-cases table | ✅ |
| GET …/checklist, POST /checklists/{id}/check | Stage checklist panel | ✅ |
| GET /calendar | Calendar page | ✅ |
| GET /notifications, POST …/read | Notification bell + panel | ✅ |
| GET/POST /views | Saved-views dropdown + save button | ✅ |
| POST …/letters/{template} | Letter buttons | ✅ |
| GET /reports/sla, /reports/summary | Reports page | ✅ |
| GET /healthz | infra probe | n/a |

**Machine-to-machine by design (no UI needed):** POST /voice/tools/case-status,
/voice/tools/deadlines, /voice/tools/intake, /voice/events (HMAC webhooks from
the voice platform), POST /api/email/inbound (SMTP gateway).

## 2. Database (Postgres CRUD) alignment

Every handler performs real CRUD — no stubs. Transactional write paths use the
**outbox pattern** (state change + event row in one tx). All tenant data is
schema-pinned (`SET LOCAL search_path TO tenant_<st>`) — fail-closed against
cross-tenant leakage. Reference/shared data lives in `public` (accounts,
leads, activities, notifications, checklists, saved views, sla_breaches,
doc_analysis, stakeholder_applications, idre_directory, npi_cache).

## 3. Middleware integration matrix

| Component | Integration point in code | Status after audit |
|---|---|---|
| **Postgres** | All services (Go pgx, Python psycopg, Rust via HTTP only) | ✅ deep |
| **Keycloak** | JWT/JWKS validation (Go), PKCE S256 (portal), realm export, onboarding provisioning activity | ✅ deep |
| **Temporal** | `IdrCaseWorkflow`, `CmsMonthlyReportWorkflow`, onboarding workflows; signals from case-api; durable statutory timers incl. negotiation auto-open | ✅ deep |
| **TigerBeetle** | `postFeeTransfer` (Go client), `post_ledger_transfer` activity, per-tenant ledgers, pending/post/void | ✅ deep |
| **Kafka** | **Outbox relay (NEW `outbox-relay-py`)** → topics; doc-intel consumer; Flink/Spark source | ✅ fixed (was: writes stranded in outbox table) |
| **OpenSearch** | doc-intel indexes full text; ISM 6-year policy; Wazuh backend | ✅ deep |
| **Redis** | **NEW (Go stdlib RESP client):** JWKS shared cache (300s TTL) + initiate idempotency keys (24h, `Idempotency-Key` header) + Dapr statestore component | ✅ fixed (was: deploy-only) |
| **Permify** | **NEW:** ReBAC schema (`deploy/permify/schema.perm`) + fail-closed check middleware on ledger transact, sealed-doc reveal, escalation; compose service | ✅ fixed (was: absent) |
| **Dapr** | **NEW runtime use:** cron binding → worker `POST /cms-monthly` starts 50 monthly-report workflows; existing pubsub/state/secrets components documented | ✅ fixed (was: configs only) |
| **Fluvio** | **NEW `edge-bridge-py`:** Fluvio edge topics → Kafka core topics with tenant routing; `edge` compose profile | ✅ fixed (was: absent) |
| **APISIX** | Gateway routes + open-appsec WAF plugin config | ✅ config-complete |
| **OpenAppSec** | APISIX plugin (`deploy/apisix/`) | ✅ config-complete |
| **Mojaloop** | Settlement-pattern reference: prepare/fulfil/abort ≡ TigerBeetle pending/post/void; hub fee scheme ≡ per-tenant ledgers (docs §7) | ✅ documented pattern (no runtime dependency by design) |
| **Lakehouse (Delta/Parquet)** | MinIO bronze/silver/gold; Flink stream ETL; Spark batch; DataFusion ad-hoc service; Ray ML | ✅ implemented |
| **Apache Sedona** | `sedona_geo.py`: jurisdiction check + coverage gaps (Spark/SedonaSQL) | ✅ implemented |
| **GeoLibre** | **NEW:** gold-zone GeoParquet export for the GeoLibre map workspace; portal Reports → "Geospatial audit map" link (`geoMapUrl` config); deploy/geolibre/README.md | ✅ fixed (new component) |
| **Wazuh / OpenCTI / Kubecost** | helm-values deployments (security ops + cost), no app-code coupling required | ✅ deploy-complete |

## 4. Fixes applied in this audit

1. `outbox-relay-py` — the missing Postgres→Kafka bridge (critical).
2. UI: fee-transfer form, escalate button, relate-case form, record-payment button.
3. Redis live in case-api: JWKS cache + idempotent initiation.
4. Permify: schema + fail-closed ReBAC checks on ledger/reveal/escalate.
5. Dapr cron → CMS monthly workflows for all 50 tenants.
6. Fluvio edge bridge service.
7. GeoLibre: Sedona GeoParquet export + portal map link + deployment guide.
8. docker-compose: outbox-relay, permify, fluvio, edge-bridge services.

## 5. Language split (as chartered)

- **Go** — case-api (control plane, auth, CRUD, ledger client, Redis, Permify)
- **Rust** — vault (seal/reveal cryptography)
- **Python** — Temporal workflows, doc-intel, outbox relay, edge bridge, analytics (Spark/Flink/DataFusion/Ray/Sedona)

---

## 6. Round 2 — engine enhancements (2026-10-01)

**Backend (case-api, `engines.go`):**

| Route | Capability | UI trigger |
|---|---|---|
| GET /cases/clocks | Batch statutory-clock projection (all open cases): remaining business/calendar days, state (ok/watch/risk/breach), CFR cite, basis provenance | Disputes grid "Statutory clock" column, dashboard attention sort |
| GET /cases/{id}/clocks | Per-case projection | Case workspace SLA cluster |
| POST /cases/bulk | Bulk assign/status, ≤200 items, per-item results, activity + outbox events | Grid selection bulk bar |
| POST /queues/grab-next | Atomic queue claim (FOR UPDATE SKIP LOCKED) | "Grab next" button + ⌘K action |
| saved_views.pinned | Pinned views sort first (DDL + save/list) | ★ in saved-views dropdown |
| account360.health | Relationship health score (100 − 8×open − 20×breaches) with band + formula | Account 360 health card |

**Frontend (portal):**
- `js/ui.js` — modal dialogs (focus-trapped, Esc, required-field inline validation) +
  toasts with undo and aria-live. **All `prompt()`/`alert()` removed** (anti-pattern #13):
  escalate, relate, fee transfer, assign, letters, save view, lead convert, onboarding
  decide, sealed offer, determination.
- `js/palette.js` — ⌘K command palette: navigation, actions (incl. grab-next), fuzzy
  record search, recent records.
- Disputes grid: SLA clock column, row selection + bulk action bar, density toggle
  (comfortable/compact, persisted), L4 peek panel (preview without losing list position).
- Case workspace: server-projected SLA cluster with CFR citations in the header.
- Dashboard: "Needs your attention" sorted by nearest statutory clock.
- Account 360: relationship-health card with formula disclosure.

`demo.js` fixtures extended for every new endpoint; `go build`/`go vet` clean; all JS syntax-checked.

## 7. Round 3 — graph intelligence (FalkorDB + GraphSAGE + EPR-KGQA)

- **graph-intel-py** (new service, port 8082): FalkorDB dispute graph (one graph per
  tenant), bidirectional lakehouse bridge (bronze JSONL / silver / gold parquet via
  pyarrow), numpy GraphSAGE link predictor (residual mean-aggregator, cosine decoder,
  manual backprop, negative sampling with positive exclusion — no torch dependency),
  EPR-KGQA (entity linking -> path retrieval -> GNN ranking -> ollama answer, with a
  clearly-labeled deterministic extractive composer when ollama is unreachable),
  ART-ready kgqa_logs parquet + thumbs feedback that reinforces retrieved edges
  (kgqa -> gnn loop).
- **case-api graph.go** (new): authenticated tenant-scoped proxy endpoints
  (/graph/ask, /graph/feedback, /graph/sync, /graph/to-lakehouse, /graph/train,
  /cases/{id}/related, /cases/{id}/graph-neighbors) + best-effort graph resync nudge
  on case initiate/signal. `GRAPH_INTEL_URL` config (compose-wired).
- **Portal**: `#/ask` view (cited answers, entity chips, evidence paths, feedback
  buttons), case-workspace "Suggested related disputes" GNN panel, palette actions
  (Ask / Sync / Train), demo fixtures mirroring the service response shapes.
  Capacitor native app ships the same assets (webDir="."); splash color aligned to
  Meridian ink.
- compose: falkordb, ollama, graph-intel services + lakehouse volume.
- [ASSUMPTION] "ART" = OpenPipe ART: logs are produced in its schema; no RL training
  loop is run (documented in docs/GRAPH-INTELLIGENCE.md).

## 8. Round 4 — cross-tenant access model

- **Tenancy middleware** now implements the full matrix: PLATFORM_ADMIN /
  FEDERAL_ADMIN read+write in all 50 state tenants; STATE_AUDITOR read-only
  (GET/HEAD/OPTIONS) in all 50 — writes rejected even in the auditor's home
  tenant (`isAuditorOnly`); everyone else limited to `/tenant/<st>` group claims.
- **Portal tenant switcher**: cross-tenant roles get a 50-state (+DC) dropdown in
  the topbar chip; pure auditors additionally see a "read-only audit" badge.
  Tenant choice persists in localStorage; write actions stay hidden for auditors
  because every write button is gated on operational roles.
