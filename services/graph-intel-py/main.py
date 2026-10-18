"""graph-intel service — FalkorDB dispute graph, numpy GraphSAGE link
prediction, EPR-KGQA over ollama, and bidirectional lakehouse sync.

Endpoints (all tenant-scoped; the Go case-api proxies these with auth):
    GET  /healthz
    POST /sync/from-db?tenant=tx          Postgres -> FalkorDB -> silver parquet
    POST /sync/from-lakehouse?tenant=tx   silver parquet -> FalkorDB
    POST /sync/to-lakehouse?tenant=tx     FalkorDB -> gold parquet export
    POST /gnn/train?tenant=tx
    GET  /gnn/predict?tenant=tx&case_id=..&k=5
    GET  /graph/neighbors?tenant=tx&case_id=..&hops=2
    POST /ask        {tenant, question}   EPR-KGQA with citations
    POST /feedback   {tenant, log_id, rating}

Run: uvicorn main:app --host 0.0.0.0 --port 8082
"""

from __future__ import annotations

import os
import threading
import time

import psycopg
from fastapi import FastAPI, Query
from pydantic import BaseModel

import epr_kgqa
import gnn
import graphdb
import lakehouse_io
import otel_setup

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")

app = FastAPI(title="idre-graph-intel", version="1.0.0")
otel_setup.init("idre-graph-intel")
otel_setup.instrument_fastapi(app)


def _load_cases(tenant: str, case_id: str | None = None) -> list[dict]:
    """Every case of the tenant, or just one (case_id)."""
    t = "".join(c for c in tenant.lower() if c.isalnum())
    where, args = (" WHERE id::text=%s", (case_id,)) if case_id else ("", ())
    cols = ["id", "case_number", "status", "service_line", "plan_type",
            "qpa_cents", "provider_id", "payer_id", "opened_at"]
    # The parties as people know them. A program tenant records the filer's
    # organization and, once read from the documents, the provider's and the
    # plan's names; provider_id/payer_id there are internal ids nobody can
    # ask about by name. Names are used where they exist, ids otherwise.
    named = cols + ["provider_org", "health_plan", "requester_org", "filing_party_type"]
    with psycopg.connect(DSN, autocommit=True) as c:
        try:
            rows = c.execute(
                f"SELECT id, case_number, status, service_line, plan_type, qpa_cents,"
                f" provider_id, payer_id, opened_at, details->>'provider_org', details->>'health_plan',"
                f" details->>'requester_org', details->>'filing_party_type' FROM tenant_{t}.cases" + where, args
            ).fetchall()
            use = named
        except Exception:  # a tenant whose cases table has no details column
            rows = c.execute(
                f"SELECT id, case_number, status, service_line, plan_type, qpa_cents,"
                f" provider_id, payer_id, opened_at FROM tenant_{t}.cases" + where, args
            ).fetchall()
            use = cols
    out = []
    for r in rows:
        d = dict(zip(use, (str(v) if v is not None else None for v in r)))
        filer, by_plan = (d.get("requester_org") or "").strip(), d.get("filing_party_type") == "HEALTH_PLAN"
        d["provider_name"] = (d.get("provider_org") or "").strip() or ("" if by_plan else filer) or d.get("provider_id")
        d["payer_name"] = (d.get("health_plan") or "").strip() or (filer if by_plan else "") or d.get("payer_id")
        out.append(d)
    return out


@app.get("/hints")
def hints(tenant: str = Query(...), allow: str | None = Query(None)):
    """Real case numbers and party names for the portal's sample questions,
    and the categories this caller's role may ask about."""
    import facts
    out = facts.hints(tenant)
    out["categories"] = [c for c in allow.split(",") if c] if allow is not None else [n for n, _ in facts.CATEGORIES]
    return out


@app.get("/healthz")
def healthz():
    try:
        graphdb.q("tx", "RETURN 1")
        graph_ok = True
    except Exception:
        graph_ok = False
    return {"ok": True, "falkordb": graph_ok, "model": epr_kgqa.OLLAMA_MODEL}


@app.post("/sync/case")
def sync_case(tenant: str = Query(...), case_id: str = Query(...)):
    """One case changed (filed, or moved a step): just that case and its
    parties are brought up to date in the graph. This is what case-api calls on
    every change; the full rebuild below is for an admin or a restore."""
    cases = _load_cases(tenant, case_id)
    for c in cases:
        graphdb.upsert_case(tenant, c)
    if cases:
        graphdb.drop_orphan_parties(tenant)
    return {"tenant": tenant, "case_id": case_id, "synced": len(cases)}


# A full rebuild reads every case of the tenant into memory. One runs per
# tenant at a time: asking again while one runs queues exactly one more (the
# changes made meanwhile are then picked up), instead of starting copies side
# by side -- several at once ran the service out of memory (OOMKilled at 1 GiB
# on a tenant of 20,000 cases).
_sync_lock = threading.Lock()
_sync_state: dict[str, str] = {}  # tenant -> "running" | "again"


def _full_sync(tenant: str) -> dict:
    cases = _load_cases(tenant)
    for c in cases:
        graphdb.upsert_case(tenant, c)
    graphdb.drop_orphan_parties(tenant)
    bronze = lakehouse_io.append_bronze(f"cases-{tenant}", cases)
    silver = lakehouse_io.write_silver_cases(tenant, cases)
    return {"tenant": tenant, "cases": len(cases), "bronze": bronze, "silver": silver}


@app.post("/sync/from-db")
def sync_from_db(tenant: str = Query(...)):
    """Source-of-truth sync: Postgres tenant schema -> FalkorDB + bronze/silver."""
    with _sync_lock:
        if tenant in _sync_state:
            _sync_state[tenant] = "again"
            return {"tenant": tenant, "queued": True,
                    "note": "a full sync of this tenant is running; one more runs when it ends"}
        _sync_state[tenant] = "running"
    try:
        while True:
            out = _full_sync(tenant)
            with _sync_lock:
                if _sync_state.get(tenant) != "again":
                    return out
                _sync_state[tenant] = "running"
    finally:
        with _sync_lock:
            _sync_state.pop(tenant, None)


@app.post("/sync/from-lakehouse")
def sync_from_lakehouse(tenant: str = Query(...)):
    """Lakehouse -> graph: rebuild FalkorDB from the silver zone."""
    cases = lakehouse_io.read_silver_cases(tenant)
    for c in cases:
        graphdb.upsert_case(tenant, c)
    return {"tenant": tenant, "restored": len(cases)}


@app.post("/sync/to-lakehouse")
def sync_to_lakehouse(tenant: str = Query(...)):
    """Graph -> lakehouse: export nodes/edges (incl. GNN/KGQA-written edges)
    to the gold zone."""
    dump = graphdb.dump_graph(tenant)
    path = lakehouse_io.write_gold_graph_export(tenant, dump)
    return {"tenant": tenant, "export": path,
            "cases": len(dump["cases"]), "case_edges": len(dump["case_edges"])}


@app.post("/gnn/train")
def gnn_train(tenant: str = Query(...), epochs: int = gnn.EPOCHS):
    return gnn.train(tenant, epochs=epochs)


@app.get("/gnn/predict")
def gnn_predict(tenant: str = Query(...), case_id: str = Query(...), k: int = 5):
    return gnn.predict(tenant, case_id, k=k)


@app.get("/graph/neighbors")
def neighbors(tenant: str = Query(...), case_id: str = Query(...), hops: int = 2):
    return graphdb.neighbors(tenant, case_id, hops=hops)


class AskRequest(BaseModel):
    tenant: str
    question: str
    k: int = 5
    # The figure categories the caller's role may read, set by case-api from
    # the signed-in user's roles. None (a direct, in-cluster call) = all.
    allow: list[str] | None = None


@app.post("/ask")
def ask(req: AskRequest):
    return epr_kgqa.ask(req.tenant, req.question, k=req.k, allow=req.allow)


class FeedbackRequest(BaseModel):
    tenant: str
    log_id: str
    rating: int  # 1 up, 0 down


@app.post("/feedback")
def feedback(req: FeedbackRequest):
    return epr_kgqa.feedback(req.tenant, req.log_id, req.rating)


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "8082")))
