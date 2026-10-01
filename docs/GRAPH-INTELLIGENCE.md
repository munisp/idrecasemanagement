# Graph Intelligence — FalkorDB + GraphSAGE + EPR-KGQA + Lakehouse

Adds a dispute graph and grounded natural-language querying to the platform.
Everything is self-hosted; no PHI or prompt ever leaves the deployment.

## Components

| Piece | Where | Role |
|---|---|---|
| FalkorDB | `falkordb` compose service (port 6397) | Single graph store, one graph per tenant (`idre_<st>`), Cypher over RESP |
| graph-intel | `services/graph-intel-py` (port 8082) | Sync, GNN, KGQA, lakehouse bridge (FastAPI) |
| ollama | `ollama` compose service (port 11434) | Local LLM that composes KGQA answers from retrieved paths only |
| case-api graph.go | `services/case-api-go/cmd/server/graph.go` | Authenticated, tenant-scoped proxy — the portal never touches graph-intel directly |
| Portal | `#/ask` route, case-workspace GNN panel, ⌘K actions | UI surface (PWA + Capacitor native, same assets) |

## Bidirectional flows (all real, no mocks)

1. **Postgres → graph → lakehouse.** Case mutations in `initiateCase`/`signalCase`
   fire a best-effort `graphSync` nudge; `POST /sync/from-db` upserts
   `:Case`/`:Party` nodes and `FILED_BY`/`AGAINST` edges into FalkorDB, then
   writes a bronze JSONL segment and the normalized `silver/cases_<t>.parquet`.
2. **Lakehouse → graph.** `POST /sync/from-lakehouse` rebuilds FalkorDB from the
   silver zone (disaster recovery / replay path).
3. **Graph → lakehouse.** `POST /sync/to-lakehouse` exports nodes + edges
   (including GNN/KGQA-written `RELATED_TO` edges) to the gold zone.
4. **Graph ↔ GNN.** `gnn.py` reads nodes/edges from FalkorDB, trains a numpy
   GraphSAGE link predictor (mean aggregator, dot-product decoder, BCE with
   negative sampling, manual backprop — no torch dependency), and writes top-k
   predictions back as `RELATED_TO {source:'gnn', score}` edges plus a
   gold-zone `gnn_predictions` parquet record.
5. **Graph ↔ KGQA.** `epr_kgqa.py` links entities (case numbers, party names),
   retrieves shortest paths as evidence, GNN-ranks related cases, and asks
   ollama to answer *from the paths only*. If ollama is unreachable a
   deterministic extractive composer answers from the same paths and the
   response is labeled `generator: extractive-fallback` — it is never silently
   presented as LLM output.
6. **KGQA → GNN (feedback).** Every ask is logged to gold-zone `kgqa_logs`
   parquet in ART-ready schema (prompt/entities/retrieved paths/answer/rating).
   A thumbs-up reinforces the retrieved edges (`score += 0.05`), which the next
   GNN training round consumes.

> **[ASSUMPTION]** "ART" in the original request was interpreted as OpenPipe
> ART (Agent Reinforcement Trainer). This deliverable produces the ART-ready
> interaction logs (prompt, retrieved context, completion, thumbs rating) but
> intentionally does **not** run an RL training loop against a local model —
> that is a separate, heavier project.

## API surface (via case-api, tenant from JWT — cross-state access impossible)

```
POST /v1/tenants/{t}/graph/ask            {question, k?}     -> answer + citations + gnn_ranked + log_id
POST /v1/tenants/{t}/graph/feedback       {log_id, rating}   -> edges_reinforced
POST /v1/tenants/{t}/graph/sync                              -> full Postgres->graph->lakehouse sync
POST /v1/tenants/{t}/graph/to-lakehouse                      -> graph->gold export
POST /v1/tenants/{t}/graph/train          {epochs?}          -> loss, edges, seconds
GET  /v1/tenants/{t}/cases/{id}/related                      -> top-k link predictions
GET  /v1/tenants/{t}/cases/{id}/graph-neighbors              -> evidence neighborhood
```

## Portal / mobile

- **`#/ask` "Ask the dispute graph"** — question bar, hint chips, cited answer
  card (entities, evidence paths, GNN-ranked cases, generator + latency),
  thumbs up/down wired to the feedback loop.
- **Case workspace** — "Suggested related disputes" panel with link scores and
  one-click confirm-link.
- **⌘K palette** — "Ask the dispute graph", "Sync dispute graph",
  "Train GNN link predictor".
- Native mobile ships the same PWA assets via Capacitor (`portal/capacitor.config.json`), so both surfaces get these views automatically.

## Run

```bash
docker compose up -d falkordb ollama graph-intel
docker exec $(docker ps -qf name=ollama) ollama pull qwen2.5:3b
curl -X POST localhost:8080/v1/tenants/tx/graph/sync   -H "Authorization: Bearer ..."
curl -X POST localhost:8080/v1/tenants/tx/graph/train  -H ...
curl -X POST localhost:8080/v1/tenants/tx/graph/ask -H ... \
  -d '{"question":"Which cases are most related to CMS-TX-2026-01479?"}'
```
