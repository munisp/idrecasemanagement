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

import json
import os
import socket
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

# Lakehouse de-identification (deid-py): when DEID_URL is set, text fields
# are scrubbed BEFORE bronze/silver parquet writes and the audit report is
# posted back to case-api — the analytics tier stays PHI-free by
# construction. Unset = legacy behavior (pre-deid deployments). A deid
# outage fails the sync: unscrubbed PHI in the lakehouse is worse than a
# delayed sync.
DEID_URL = os.environ.get("DEID_URL", "")
CASE_API = os.environ.get("CASE_API_URL", "http://localhost:8080")
WORKER_TOKEN = os.environ.get("WORKER_TOKEN", "")


# deid-py takes at most 500,000 characters a request; batches stay well under.
DEID_BATCH_CHARS = int(os.environ.get("DEID_BATCH_CHARS", "200000"))


def _deid_batches(values: list[str], limit: int) -> list[list[str]]:
    """Split values (one per line) into batches whose joined text stays under limit."""
    batches, cur, size = [], [], 0
    for v in values:
        if cur and size + len(v) + 1 > limit:
            batches.append(cur)
            cur, size = [], 0
        cur.append(v)
        size += len(v) + 1
    if cur:
        batches.append(cur)
    return batches


def _deid_rows(tenant: str, rows: list[dict]) -> list[dict]:
    """Scrub free-text fields through deid-py; report the decision.

    Each distinct value is scrubbed once (a tenant's 20,000 cases name a few
    hundred organizations), in batches under deid-py's request limit. A value
    is sent on a line of its own, so its newlines are flattened first; a batch
    that comes back with a different number of lines fails the sync rather
    than pair a scrubbed value with the wrong field."""
    if not DEID_URL:
        return rows
    import hashlib
    import httpx
    fields = ["provider_name", "payer_name", "provider_org", "health_plan",
              "requester_org", "service_line"]
    flat = lambda v: " ".join(str(v).splitlines())  # noqa: E731
    out = [dict(d) for d in rows]
    unique = sorted({flat(d[f]) for d in out for f in fields if d.get(f) and flat(d[f]).strip()})
    if not unique:
        return out
    scrubbed: dict[str, str] = {}
    counts: dict[str, int] = {}
    detectors: list[str] = []
    policy = "safe-harbor-18/v1"
    audit: dict = {}
    for batch in _deid_batches(unique, DEID_BATCH_CHARS):
        resp = httpx.post(f"{DEID_URL}/v1/deidentify", timeout=120, json={
            "tenant": tenant, "artifact": "lakehouse", "text": "\n".join(batch)})
        resp.raise_for_status()
        body = resp.json()
        redacted = body["text"].split("\n")
        if len(redacted) != len(batch):
            raise RuntimeError(f"deid returned {len(redacted)} lines for {len(batch)} values")
        scrubbed.update(zip(batch, redacted))
        audit = body["audit"]
        for k, n in audit.get("counts", {}).items():
            counts[k] = counts.get(k, 0) + n
        detectors = sorted(set(detectors) | set(audit.get("detectors", [])))
        policy = audit.get("policy", policy)
    for d in out:
        for f in fields:
            if d.get(f):
                d[f] = scrubbed.get(flat(d[f]), d[f])
    findings_total = sum(counts.values())
    # One report per sync run: the input hash covers every value scrubbed.
    report = dict(audit, counts=counts, detectors=detectors, policy=policy)
    try:
        httpx.post(
            f"{CASE_API}/v1/tenants/{tenant}/internal/deid/reports",
            headers={"Authorization": f"Bearer {WORKER_TOKEN}"}, timeout=10,
            json={"artifact": "lakehouse",
                  "ref_id": hashlib.sha256("\n".join(unique).encode()).hexdigest()[:16],
                  "action": "DEIDENTIFY", "audit": report})
    except httpx.HTTPError:
        pass  # report persistence is best-effort; the scrub itself is not
    if findings_total:
        print(f"lakehouse deid: {findings_total} identifier(s) scrubbed "
              f"for {tenant} ({', '.join(detectors)}, {policy})", flush=True)
    return out

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
# on a tenant of 20,000 cases). graph-intel runs more than one replica, so the
# lock, the "once more" flag and the last result are kept in FalkorDB's Redis
# (shared by them all), not in one process's memory: an ask that lands on the
# other replica sees the sync that is running, and its status.
SYNC_LOCK_TTL = int(os.environ.get("SYNC_LOCK_TTL", "3600"))  # a replica killed mid-sync frees the lock


def _sync_key(tenant: str, what: str) -> str:
    return f"idre:graph-sync:{tenant}:{what}"


def _redis():
    return graphdb._client().connection


def _full_sync(tenant: str) -> dict:
    cases = _load_cases(tenant)
    for c in cases:
        graphdb.upsert_case(tenant, c)
    graphdb.drop_orphan_parties(tenant)
    # FalkorDB gets the operational graph unscrubbed (it is a transactional
    # system, tenant-isolated); the lakehouse tiers get the de-identified
    # projection — analytics must never hold raw PHI.
    scrubbed = _deid_rows(tenant, cases)
    bronze = lakehouse_io.append_bronze(f"cases-{tenant}", scrubbed)
    silver = lakehouse_io.write_silver_cases(tenant, scrubbed)
    return {"tenant": tenant, "cases": len(cases), "bronze": bronze, "silver": silver}


def _sync_last(tenant: str):
    raw = _redis().get(_sync_key(tenant, "last"))
    return json.loads(raw) if raw else None


def _run_full_sync(tenant: str) -> None:
    r = _redis()
    try:
        while True:
            r.delete(_sync_key(tenant, "again"))
            started = time.time()
            try:
                out = _full_sync(tenant)
                out.update(ok=True, seconds=round(time.time() - started, 1))
            except Exception as e:  # recorded for /sync/status; the next ask tries again
                out = {"tenant": tenant, "ok": False, "error": str(e)[:500]}
                print(f"full sync of {tenant} failed: {e}", flush=True)
            out["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            r.set(_sync_key(tenant, "last"), json.dumps(out))
            if not r.get(_sync_key(tenant, "again")):
                return
            r.expire(_sync_key(tenant, "running"), SYNC_LOCK_TTL)
    finally:
        r.delete(_sync_key(tenant, "running"))


@app.post("/sync/from-db")
def sync_from_db(tenant: str = Query(...), wait: bool = False):
    """Source-of-truth sync: Postgres tenant schema -> FalkorDB + bronze/silver.

    A tenant of 20,000 cases takes minutes, longer than a caller waits, so it
    runs in the background: the answer says it started (or was queued behind
    the one running), and /sync/status tells how it ended. wait=true runs it
    in the request (scripts, tests)."""
    r = _redis()
    if not r.set(_sync_key(tenant, "running"), socket.gethostname(), nx=True, ex=SYNC_LOCK_TTL):
        r.set(_sync_key(tenant, "again"), "1", ex=SYNC_LOCK_TTL)
        return {"tenant": tenant, "queued": True,
                "note": "a full sync of this tenant is running; one more runs when it ends"}
    if wait:
        _run_full_sync(tenant)
        return _sync_last(tenant) or {"tenant": tenant}
    threading.Thread(target=_run_full_sync, args=(tenant,), daemon=True).start()
    return {"tenant": tenant, "started": True, "last": _sync_last(tenant)}


@app.get("/sync/status")
def sync_status(tenant: str = Query(...)):
    """Whether a full sync of the tenant is running (on any replica), and how the last one ended."""
    return {"tenant": tenant, "running": bool(_redis().exists(_sync_key(tenant, "running"))), "last": _sync_last(tenant)}


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
