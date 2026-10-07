"""KGQA correctness tests — the pipeline must return grounded answers:
entities actually mentioned in the question, paths only from the graph,
extractive fallback when ollama is down, and a log entry every time."""
import sys, types
from unittest import mock

# Stub infra modules before importing epr_kgqa (no FalkorDB/ollama in CI).
graphdb = types.ModuleType("graphdb")
gnn = types.ModuleType("gnn")
lake = types.ModuleType("lakehouse_io")
sys.modules["graphdb"] = graphdb
sys.modules["gnn"] = gnn
sys.modules["lakehouse_io"] = lake

CASE = {"id": "c-1", "kind": "Case", "label": "IDR-2026-0004", "detail": "OPEN"}
PAYER = {"id": "p-9", "kind": "Payer", "label": "Acme Health", "detail": ""}
PATHS = ["(IDR-2026-0004)-[:AGAINST]->(Acme Health)<-[:AGAINST]-(IDR-2026-0007)"]
CASE_INDEX = [
    {"id": "c-1", "case_number": "IDR-2026-0004", "status": "OPEN"},
    {"id": "c-2", "case_number": "IDR-2026-0007", "status": "OPEN"},
    {"id": "c-3", "case_number": "IDR-2026-0002", "status": "CLOSED_PAID"},
]

import epr_kgqa as k  # noqa: E402


def _wire(case_index=CASE_INDEX):
    graphdb.find_entities = lambda tenant, term: (
        [CASE] if "idr-2026-0004" in term.lower() else
        [PAYER] if "acme" in term.lower() else [])
    graphdb.retrieve_paths = lambda tenant, ids: PATHS if ids else []
    graphdb.case_index = lambda tenant: case_index
    gnn.predict = lambda tenant, cid, k=5, write_back=False: {
        "predictions": [{"case_id": "c-7", "score": 0.91}]}
    lake.append_kgqa_log = lambda rec: "log-123"


def test_terms_keep_case_numbers_and_drop_stopwords():
    ts = [t.lower() for t in k._terms("Which disputes share a payer with case IDR-2026-0004?")]
    assert "idr-2026-0004" in ts and "which" not in ts and "case" not in ts


def test_ask_extractive_fallback_cites_paths_and_entities():
    _wire()
    with mock.patch.object(k, "_ollama_generate", side_effect=Exception("down")):
        r = k.ask("t1", "Which disputes share a payer with case IDR-2026-0004?")
    assert r["generator"] == "extractive-fallback"
    assert r["citations"] == PATHS or any(p in str(r.get("citations")) for p in PATHS)
    labels = [e["label"] for e in r["entities"]]
    assert "IDR-2026-0004" in labels
    assert r["gnn_ranked"] and r["gnn_ranked"][0]["case_id"] == "c-7"
    assert r["log_id"] == "log-123"


def test_ask_uses_ollama_when_reachable():
    _wire()
    with mock.patch.object(k, "_ollama_generate", return_value=("grounded answer", "ollama:test")):
        r = k.ask("t1", "Tell me about IDR-2026-0004")
    assert r["generator"] == "ollama:test" and r["answer"] == "grounded answer"


def test_ask_no_entities_is_honest_when_graph_is_truly_empty():
    _wire(case_index=[])
    with mock.patch.object(k, "_ollama_generate", side_effect=Exception("down")):
        r = k.ask("t1", "anything about zzz-unknown")
    assert r["entities"] == [] and r["citations"] == []
    assert "No matching" in r["answer"]


def test_ask_no_entities_falls_back_to_general_summary():
    # A broad question ("what's the summary of our disputes") links zero
    # entities on purpose -- _terms' stopword list drops "summary"/
    # "disputes"/generic words, so there's nothing to look up by name. That
    # used to surface as a flat "No matching cases" even when the tenant has
    # real cases; it should instead report real aggregate numbers.
    _wire()
    with mock.patch.object(k, "_ollama_generate", side_effect=Exception("down")):
        r = k.ask("t1", "what's the summary of the disputes we have currently")
    assert r["entities"] == [] and r["citations"] == []
    assert "No matching" not in r["answer"]
    assert "3 total case(s)" in r["answer"]
    assert "2 OPEN" in r["answer"] and "1 CLOSED_PAID" in r["answer"]


def test_ask_general_summary_also_feeds_the_ollama_prompt():
    # Same broad-question case, but ollama IS reachable -- the prompt it
    # receives must carry the aggregate summary as evidence, not the literal
    # "(no graph paths retrieved)" placeholder, or a reachable LLM would
    # still have nothing to answer from.
    _wire()
    captured = {}

    def fake_generate(prompt):
        captured["prompt"] = prompt
        return "answer", "ollama:test"

    with mock.patch.object(k, "_ollama_generate", side_effect=fake_generate):
        k.ask("t1", "what's the summary of the disputes we have currently")
    assert "no graph paths retrieved" not in captured["prompt"]
    assert "3 total case(s)" in captured["prompt"]
