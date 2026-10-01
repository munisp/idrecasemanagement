# In-Memory Store Audit — full-repo sweep (Go / Rust / Python / TS-JS)

Method: swept every service for process-local mutable state — Go package-level
maps / `sync.Map`, Rust `HashMap`/`Mutex`/`RwLock`/`static`, Python
module-level dicts/lists/globals, portal JS `Map`/`Set`/storage APIs. Every
occurrence classified below. Verified by `go build`/`go vet`, `node --check`,
and `py_compile` after the fix wave.

## Verdict

**No critical occurrences.** No business data lives in process memory anywhere
in the platform — cases, offers, ledger, documents, graph, and lakehouse all
persist to Postgres / MinIO / TigerBeetle / FalkorDB / Parquet respectively,
and Temporal workflow state rides event history. The two `map[string]` package
vars in Go are code constants (checklist defaults, letter templates), not data
stores.

## Classification table

| # | Location | Store | Class | Persistence target / disposition |
|---|----------|-------|-------|----------------------------------|
| 1 | `portal/js/auth.js` | tokens + PKCE verifier in `sessionStorage` | **medium** (session/auth) | Accepted SPA practice (per-tab, cleared on close; refresh rotation already implemented). Hardening option documented: memory-only tokens + hidden-iframe silent refresh. Not changed — see note below. |
| 2 | `portal/js/{app,views,palette,api}.js` | theme / density / last-tenant / recents in `localStorage` only | **medium** (user state, device-local) | **FIXED** — server-side `public.user_prefs` + `GET/PUT /prefs`; localStorage demoted to offline cache, server wins on login. |
| 3 | `casemgmt.go` `stageChecklistDefaults` | package-level map (code constant) | benign | If per-tenant customization is ever requested: `tenant_settings` table. No action. |
| 4 | `casemgmt.go` `letterTemplates` | package-level map (code constant) | benign | Same as #3. |
| 5 | `graph.go` `graphHTTP` | shared `http.Client` | benign | Infra, not data. |
| 6 | `vault-rs` `AppState` | Arc master key + URLs + reqwest client | benign by design | Keys are never stored per-tenant in memory: DEKs are wrapped and persisted in Postgres (`sealed_offers`); master is env-injected config. |
| 7 | `idre-workflows-py` `EASTER_CACHE` | computation memo | benign | Deterministic (Computus); eviction-free is correct. |
| 8 | `idre-workflows-py` `worker.py::_MAIN_LOOP` | asyncio loop handle | benign | Infra (signal handlers), not data. |
| 9 | `edge-bridge-py` `TOPIC_MAP`, `epr_kgqa._STOP` | module constants | benign | Config/code. |
| 10 | `gnn.py` tensors (`X`, `A`, `pos`, …) | per-request numpy arrays | benign | Derived from FalkorDB per call; trained weights persist to `gold/gnn_weights.npz` + `gnn_meta.json`. |
| 11 | `graphdb._db`, `doc-intel` clients | module-level connection clients | benign | Connections, not data. |
| 12 | `views.js` `selection` Set, `__notifPanel` | transient UI state | benign | Re-render semantics intentional. |
| 13 | `sw.js` Cache API | offline shell/API cache | benign | PWA requirement; API cache is network-first. |
| 14 | `portal/js/demo.js` fixtures | fixture maps | **test-only** | Active solely with `demoMode:true` (staging preview only; repo default `false`). |
| 15 | `/tmp/graph_demo.py` | demo seed data | **test-only** | Sandbox demo script, not shipped. |

## Fix wave (delivered)

1. **Server-side user preferences** (`public.user_prefs`, PK `(tenant, user_sub, key)`):
   - `prefs.go`: `GET /v1/tenants/{t}/prefs`, `PUT /prefs {key, value}` with an
     allow-list (`theme`, `density`, `last_tenant`, `recents`) and an 8 KiB value cap.
   - Portal: `Prefs.push()` writes through (localStorage immediate + server
     fire-and-forget); on login the server copy re-applies theme/density/recents.
   - Result: prefs follow a case manager from the desktop PWA to the Capacitor
     native app; offline still works from the local cache.
2. **Auth storage (decision record)**: tokens remain in `sessionStorage`
   (industry-standard for SPAs without a BFF; PKCE verifier is single-use and
   cleared by flow completion; refresh tokens rotate at Keycloak). The
   memory-only + silent-iframe variant is recorded here as the accepted-risk
   alternative if a future threat assessment requires it — it trades an
   XSS-exfiltration window for an extra auth-server round trip per tab.

## Adversarial verification performed

- `go build ./... && go vet ./...` — clean.
- `node --check` on all five modified portal bundles — clean.
- `py_compile` on all Python services — clean (previous round).
- DDL typo check: comment syntax corrected (`--` not `//`).
- Read-through of every `grep` hit, not just pattern counts — response-literal
  `map[string]any{...}` values were excluded deliberately (they are per-request
  JSON, not stores).
