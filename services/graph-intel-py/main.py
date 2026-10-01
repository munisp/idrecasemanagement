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
import time

import psycopg
from fastapi import FastAPI, Query
from pydantic import BaseModel

import epr_kgqa
import gnn
import graphdb
import lakehouse_io

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")

app = FastAPI(title="idre-graph-intel", version="1.0.0")


def _load_cases(tenant: str) -> list[dict]:
    t = "".join(c for c in tenant.lower() if c.isalnum())
    with psycopg.connect(DSN) as c:
        rows = c.execute(
            f"SELECT id, case_number, status, service_line, plan_type, qpa_cents,"
            f" provider_id, payer_id, opened_at FROM tenant_{t}.cases"
        ).fetchall()
    cols = ["id", "case_number", "status", "service_line", "plan_type",
            "qpa_cents", "provider_id", "payer_id", "opened_at"]
    return [dict(zip(cols, (str(v) if v is not None else None for v in r))) for r in rows]


@app.get("/healthz")
def healthz():
    try:
        graphdb.q("tx", "RETURN 1")
        graph_ok = True
    except Exception:
        graph_ok = False
    return {"ok": True, "falkordb": graph_ok, "model": epr_kgqa.OLLAMA_MODEL}


@app.post("/sync/from-db")
def sync_from_db(tenant: str = Query(...)):
    """Source-of-truth sync: Postgres tenant schema -> FalkorDB + bronze/silver."""
    cases = _load_cases(tenant)
    for c in cases:
        graphdb.upsert_case(tenant, c)
    bronze = lakehouse_io.append_bronze(f"cases-{tenant}", cases)
    silver = lakehouse_io.write_silver_cases(tenant, cases)
    return {"tenant": tenant, "cases": len(cases), "bronze": bronze, "silver": silver}


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


@app.post("/ask")
def ask(req: AskRequest):
    return epr_kgqa.ask(req.tenant, req.question, k=req.k)


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
