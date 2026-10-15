"""EPR-KGQA — Entity linking, Path Retrieval, KG-grounded Question Answering.

Pipeline for a natural-language question over a tenant's dispute graph:

    1. Entity linking   — case numbers / party names mentioned in the question
                          are resolved to graph nodes (graphdb.find_entities).
    2. Path retrieval   — shortest paths between mentioned entities, or the
                          2-hop neighborhood for a single entity. Paths are
                          the evidence and become the citations.
    3. GNN ranking      — when the question names a case, link-prediction
                          scores rank the related cases surfaced alongside.
    4. Answer           — ollama (local LLM, no PHI leaves the box) composes
                          the answer from the retrieved paths only. When no
                          ollama server is reachable the deterministic
                          extractive composer answers from the same paths and
                          the response is labeled generator=extractive-fallback.
    5. Logging          — every interaction is appended to the gold-zone
                          kgqa_logs parquet in ART schema (prompt, retrieved
                          context, completion, rating) — no training loop here,
                          the logs are the ART handoff.

Positive feedback reinforces the retrieved edges in FalkorDB, which the next
GNN training round consumes — closing the kgqa -> gnn loop.
"""

from __future__ import annotations

import json
import math
import os
import re
import time

import httpx

import gnn
import graphdb
import lakehouse_io

OLLAMA_URL = os.environ.get("OLLAMA_URL", "http://localhost:11434")
OLLAMA_MODEL = os.environ.get("OLLAMA_MODEL", "qwen2.5:3b")
OLLAMA_TIMEOUT = float(os.environ.get("OLLAMA_TIMEOUT", "60"))

_WORD = re.compile(r"[A-Za-z0-9][A-Za-z0-9._\-]{2,}")
_STOP = {
    "the", "and", "for", "with", "from", "that", "this", "what", "which", "how",
    "many", "much", "are", "was", "were", "did", "does", "show", "list", "tell",
    "about", "between", "related", "cases", "case", "dispute", "disputes",
    # Ordinary words of a question about the program. Left in, they are
    # looked up as names: "plan" matched every "... Health Plan", and a
    # question about all plans was answered about one of them.
    "health", "plan", "plans", "provider", "providers", "payer", "payers", "service", "services", "line", "lines",
    "open", "opened", "closed", "eligible", "ineligible", "eligibility", "amount", "amounts", "average", "total",
    "status", "organizations", "organization", "filed", "file", "files", "filer", "filing", "most", "often", "right",
    "now", "last", "days", "day", "have", "has", "been", "each", "stage", "who", "why", "more", "than", "older",
    "past", "limit", "type", "deadline", "deadlines", "missed", "contract", "deliverables", "due", "late", "long",
    "take", "opening", "closing", "found", "versus", "common", "reasons", "reason", "hold", "invalid",
    "authorization", "representative", "decisions", "decision", "borderline", "sent", "second", "look", "paid",
    "yet", "largest", "five", "million", "decided", "awarded", "share", "involve", "recorded", "emergency",
    "highest", "waiting", "internal", "currently", "summary", "overview", "our", "all", "any", "them", "their",
    "there", "you", "can", "give", "name", "names", "number", "numbers", "top", "against", "together", "appear",
    "pairs", "connects", "connect", "new", "old", "one", "two", "disputed", "money", "worth", "value", "they",
    "where", "when", "not", "but", "yes", "per", "its", "out", "into", "over", "under", "still", "also",
    # Kinds of service. "air" in "air ambulance services" matched a provider
    # called "... Air Rescue", and the question was answered about that provider.
    "air", "ambulance", "anesthesia", "radiology", "imaging", "lab", "laboratory", "surgery", "surgical",
}


def _terms(question: str) -> list[str]:
    out, seen = [], set()
    for tok in _WORD.findall(question):
        t = tok.strip(".-").lower()
        if len(t) >= 3 and t not in _STOP and t not in seen:
            seen.add(t)
            out.append(tok.strip(".-"))
    return out[:6]


def link_entities(tenant: str, question: str) -> list[dict]:
    ents: list[dict] = []
    seen: set[str] = set()
    for term in _terms(question):
        for e in graphdb.find_entities(tenant, term):
            key = f"{e['kind']}:{e['id']}"
            if key not in seen:
                seen.add(key)
                ents.append(e)
        if len(ents) >= 8:
            break
    # The entities the question is most plainly about come first: the first
    # two decide which paths are retrieved. A name written out in full beats
    # one that merely shares a word with the question.
    ql, terms = question.lower(), [t.lower() for t in _terms(question)]

    def weight(e: dict) -> int:
        label = str(e.get("label") or "").lower()
        return (10 if label and label in ql else 0) + sum(1 for t in terms if t in label)
    return sorted(ents, key=weight, reverse=True)


def _ollama_generate(prompt: str) -> tuple[str, str]:
    """Returns (text, generator_label). Raises on connectivity failure."""
    resp = httpx.post(
        f"{OLLAMA_URL}/api/generate",
        json={"model": OLLAMA_MODEL, "prompt": prompt, "stream": False,
              "options": {"temperature": 0.1}},
        timeout=OLLAMA_TIMEOUT,
    )
    resp.raise_for_status()
    return resp.json()["response"].strip(), f"ollama:{OLLAMA_MODEL}"


def _general_summary(tenant: str) -> list[str]:
    """Aggregate overview, used when the question names no specific case or
    party (link_entities' stopword list filters out generic words like
    "summary"/"disputes"/"currently" on purpose, so a broad "what's the
    summary of our disputes" question links zero entities -- that's not a
    failed lookup, it's a different kind of question this QA system didn't
    previously have an answer for at all, surfacing as a flat "no matching
    cases" even when the tenant has plenty of cases). Feeds both the ollama
    prompt and the extractive fallback so this works whichever one answers."""
    cases = graphdb.case_index(tenant)
    if not cases:
        return []
    by_status: dict[str, int] = {}
    for c in cases:
        status = c["status"] or "UNKNOWN"
        by_status[status] = by_status.get(status, 0) + 1
    breakdown = ", ".join(f"{n} {s}" for s, n in sorted(by_status.items(), key=lambda kv: -kv[1]))
    return [f"{len(cases)} total case(s) in this tenant's graph — status breakdown: {breakdown}."]


def _extractive_answer(question: str, entities: list[dict], paths: list[str],
                       ranked: list[dict], general: list[str] | None = None) -> str:
    lines = ["Based on the dispute graph (deterministic composer — ollama unreachable):"]
    if entities:
        ent_s = "; ".join(f"{e['label']} ({e['kind']}{', ' + e['detail'] if e['detail'] else ''})"
                          for e in entities[:5])
        lines.append(f"Entities matched: {ent_s}.")
    if paths:
        lines.append(f"{len(paths)} evidence path(s) connect these entities; the strongest are cited below.")
    if ranked:
        top = ", ".join(f"{r['case_id']} (score {r['score']})" for r in ranked[:3])
        lines.append(f"GNN link prediction ranks these as most related: {top}.")
    if general:
        lines.extend(general)
    elif not (entities or paths):
        lines.append("No matching cases or parties were found in this tenant's graph.")
    return "\n".join(lines)


def _prompt(question: str, paths: list[str], general: list[str] | None = None) -> str:
    # The records first (a case's own lines lead them), then the graph paths;
    # capped so a small local model reads all of it.
    evidence_lines = list(general or [])[:14] + paths[:12]
    evidence = "\n".join(f"- {p}" for p in evidence_lines) or "(no evidence retrieved)"
    return (
        "You are the analyst for a healthcare claims dispute resolution program. "
        "The evidence below is of two kinds: graph paths linking cases and parties, "
        "and figures computed from the program's own records. Answer ONLY from it. "
        "Give the exact numbers and names the evidence gives; do not round, add or "
        "invent any. If the evidence does not answer the question, say what is "
        "missing in one sentence.\n\n"
        f"Evidence:\n{evidence}\n\nQuestion: {question}\nAnswer (short, plain English):"
    )


def _facts_for(tenant: str, question: str, allow: list[str] | None = None) -> list[str]:
    """The program figures that bear on the question; [] when they cannot be
    computed (no database in reach) -- the graph answer still stands.
    `allow` is the categories the caller's role may read (None = all): the
    others are not looked at, so they can reach neither the answer nor the model."""
    try:
        import facts
        got = facts.gather(tenant)
        if allow is not None:
            got = {name: lines for name, lines in got.items() if name in allow}
        return facts.select(question, got)
    except Exception:
        return []


def _withheld(question: str, allow: list[str] | None) -> list[str]:
    """Labels of the figures this question asks for that the caller's role
    may not read."""
    if allow is None:
        return []
    try:
        import facts
        return [facts.LABELS.get(c, c) for c in facts.matching(question) if c not in allow]
    except Exception:
        return []


def _matched(question: str) -> list[str]:
    try:
        import facts
        return facts.matching(question)
    except Exception:
        return []


def _case_lines(tenant: str, case_id: str) -> list[str]:
    try:
        import facts
        return facts.case_lines(tenant, case_id)
    except Exception:
        return []


def _records_answer(question: str, lines: list[str]) -> str:
    """The answer to a question about the program as a whole: the figures
    themselves, most relevant first. No model writes this one. Tried live, the
    model restated a ranked list as "each with 10 disputes" when the figures
    said 12, 12, 10, 10 -- and a count that is nearly right is worse than a
    plain line that is exactly right."""
    words = {w for w in re.findall(r"[a-z0-9$]+", question.lower()) if len(w) > 2 or w.isdigit()} - {
        "the", "and", "are", "was", "were", "how", "many", "what", "which", "with", "have", "has", "been", "for",
        "from", "that", "this", "does", "did", "our", "all", "any", "per", "each", "there"}
    stems = {w[:6] for w in words}

    # A word found in few of the lines says more about which line is meant
    # than one found in most: "provider" is in half of them, "waiting" in one.
    lows = [ln.lower() for ln in lines]
    rarity = {st: math.log(1 + len(lows) / df) for st in stems if (df := sum(1 for lo in lows if st in lo))}

    def score(line: str) -> float:
        low = line.lower()
        return sum(min(low.count(st), 2) * w for st, w in rarity.items())
    ranked = sorted(lines, key=score, reverse=True)       # stable: ties keep the category order
    # The line that answers, and a second only when it matches nearly as
    # well. Everything else considered is listed under the answer anyway.
    hits = [ln for ln in ranked if score(ln) > 0]
    if not hits:
        # A general question ("the disputes in the system"): the overview.
        return "\n".join(lines[:4])
    best = hits[:1] + [ln for ln in hits[1:2] if score(ln) >= 0.6 * score(hits[0])]
    return "\n".join(best)


def ask(tenant: str, question: str, k: int = 5, allow: list[str] | None = None) -> dict:
    t0 = time.time()
    entities = link_entities(tenant, question)
    entity_ids = [e["id"] for e in entities]
    paths = graphdb.retrieve_paths(tenant, entity_ids) if entity_ids else []
    # Figures from the program's records go with every question: alone when
    # it names no case or party, beside the graph paths when it does.
    general = _facts_for(tenant, question, allow)
    withheld = _withheld(question, allow)
    # A question about figures the caller's role may not read is told so --
    # not answered from whatever else happened to match ("how many" matches
    # the caseload for every count question).
    narrow_ok = allow is None or any(c != "caseload" and c in allow for c in _matched(question))
    if withheld and not entity_ids and not narrow_ok:
        answer = ("Your role does not have access to " + " or ".join(withheld) + ", so I can't answer that here. "
                  "You can ask about the caseload, services, or any single case.")
        return {"log_id": lakehouse_io.append_kgqa_log({
                    "tenant": tenant, "ts": int(time.time()), "question": question, "entities_json": "[]",
                    "retrieved_paths_json": "[]", "answer": answer, "generator": "role-boundary",
                    "gnn_ranked_json": "[]"}),
                "tenant": tenant, "question": question, "answer": answer, "entities": [], "citations": [],
                "facts": [], "gnn_ranked": [], "generator": "role-boundary", "withheld": withheld,
                "latency_ms": int((time.time() - t0) * 1000)}
    from_records = bool(general) and not entity_ids
    # A question about one case also gets that case's own record, and the
    # other disputes that share its filer or plan.
    case_ents = [e for e in entities if e["kind"] == "Case"]
    for e in case_ents[:2]:
        general = _case_lines(tenant, e["id"]) + general
    if not general and not entity_ids:
        # No database in reach: the graph's own status count, put to the model as before.
        general = _general_summary(tenant)

    # GNN ranking for the first linked case (graph <-> gnn <-> kgqa)
    ranked: list[dict] = []
    if case_ents:
        pred = gnn.predict(tenant, case_ents[0]["id"], k=k, write_back=False)
        ranked = pred.get("predictions", [])

    generator = ""
    if from_records:
        answer, generator = _records_answer(question, general), "program-records"
        if withheld:
            answer += "\n(Not included: " + ", ".join(withheld) + " -- your role does not have access to those figures.)"
    else:
        try:
            answer, generator = _ollama_generate(_prompt(question, paths, general))
        except Exception:
            answer = _extractive_answer(question, entities, paths, ranked, general)
            generator = "extractive-fallback"

    log_id = lakehouse_io.append_kgqa_log({
        "tenant": tenant,
        "ts": int(time.time()),
        "question": question,
        "entities_json": json.dumps(entities),
        "retrieved_paths_json": json.dumps(paths),
        "answer": answer,
        "generator": generator,
        "gnn_ranked_json": json.dumps(ranked),
    })

    return {
        "log_id": log_id,
        "tenant": tenant,
        "question": question,
        "answer": answer,
        "entities": entities,
        "citations": [{"path": p} for p in paths],
        "facts": general,
        "withheld": withheld,
        "gnn_ranked": ranked,
        "generator": generator,
        "latency_ms": int((time.time() - t0) * 1000),
    }


def feedback(tenant: str, log_id: str, rating: int) -> dict:
    """Thumbs up/down. Positive ratings reinforce the retrieved edges so the
    next GNN training round weights them higher (kgqa -> gnn)."""
    rating = 1 if rating > 0 else 0
    lakehouse_io.rate_kgqa_log(log_id, rating)
    reinforced = 0
    if rating == 1:
        for row in lakehouse_io.read_kgqa_logs(limit=2000):
            if row["log_id"] != log_id:
                continue
            for p in json.loads(row["retrieved_paths_json"] or "[]"):
                # paths render case nodes by case_number (or UUID) — accept both
                ids = re.findall(
                    r"\b(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
                    r"|[A-Z]{2,5}-[A-Z]{2}-\d{4}-\d+)\b", p)
                for a, b in zip(ids, ids[1:]):
                    reinforced += graphdb.reinforce_edge(tenant, a, b, 0.05, "kgqa")
            break
    return {"log_id": log_id, "rating": rating, "edges_reinforced": reinforced}
