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


# ---- program figures (facts.py) ----------------------------------------------

FACTS = {
    "caseload": ["There are 213 disputes in total: 158 open and 55 closed or decided.", "12 opened in the last 30 days.",
                 "Disputes by status: PENDING_INTAKE: 100."],
    "parties": ["Organizations that filed the most disputes: Tri-County Air Rescue: 15."],
    "services": ["Disputes by service line: AIR_AMBULANCE: 32."],
    "deadlines": ["Of 158 open disputes, 40 have been open more than 60 days and 22 more than 90 days."],
    "eligibility": ["Latest eligibility decision per dispute: ELIGIBLE: 46, INELIGIBLE: 18."],
    "money": ["Total amount in dispute: $12,850,595.98 across 195 disputes."],
    "outcomes": ["Finished disputes: Closed: 46, Decided: 9."],
}


def test_each_sample_question_reaches_its_own_figures():
    import facts
    for question, must in [
        ("How many disputes are open right now versus closed?", "213 disputes"),
        ("Which organizations file the most disputes?", "Tri-County"),
        ("Which service lines have the most disputes?", "AIR_AMBULANCE"),
        ("Which open cases are older than 60 days?", "more than 60 days"),
        ("What are the most common reasons a dispute is ineligible?", "INELIGIBLE"),
        ("What is the total amount in dispute?", "$12,850,595.98"),
        ("How many cases have been decided, and what was awarded?", "Decided: 9"),
    ]:
        assert any(must in line for line in facts.select(question, FACTS)), question
    # the narrower category leads; the caseload lines follow as context, none dropped
    lines = facts.select("How many disputes are $1 million or more?", FACTS)
    assert lines[0].startswith("Total amount") and lines[1:] == FACTS["caseload"]
    # a question that matches no category gets a little of everything
    assert len(facts.select("anything interesting?", FACTS)) >= 7
    assert facts.select("anything", {}) == []


def test_ordinary_question_words_are_not_looked_up_as_names():
    for q in ("Which health plans are named in the most disputes?",
              "How many disputes were filed by providers versus health plans?",
              "How many open cases past payment have no eligibility decision yet?"):
        ts = {t.lower() for t in k._terms(q)}
        assert not ts & {"plan", "plans", "health", "providers", "open", "eligibility", "filed"}, (q, ts)
    assert "sunshine" in {t.lower() for t in k._terms("Which cases name Sunshine Coast Health Plan?")}


def test_a_whole_program_question_is_answered_with_the_figures_themselves():
    _wire()
    lines = FACTS["money"] + ["Five largest disputes by amount: FL26-000193 ($1,000,000.00)."]
    with mock.patch.object(k, "_facts_for", return_value=lines), \
         mock.patch.object(k, "_ollama_generate", side_effect=AssertionError("no model for a count")):
        r = k.ask("t1", "What are the five largest disputes by amount?")
    assert r["generator"] == "program-records" and r["answer"].startswith("Five largest disputes")
    assert "$12,850,595.98" not in r["answer"] and r["facts"] == lines   # the answer is the line that answers


def test_a_question_naming_a_case_gets_the_figures_beside_the_graph_paths():
    _wire()
    seen = {}
    with mock.patch.object(k, "_facts_for", return_value=FACTS["money"]), \
         mock.patch.object(k, "_ollama_generate", side_effect=lambda p: (seen.setdefault("p", p) and "ok", "ollama:x")):
        r = k.ask("t1", "How does IDR-2026-0004 compare with the total amount in dispute?")
    assert "$12,850,595.98" in seen["p"] and "AGAINST" in seen["p"] and r["generator"] == "ollama:x"


def test_a_waiting_question_is_answered_from_the_status_line_not_the_filer_line():
    lines = ["Disputes by who filed: providers: 201, health plans: 3.",
             "Disputes by internal status (where the work is): Provider Closure Letter Issued: 17, Provider Acceptance "
             "Letter Issued: 11, RFI Pending Plan Response: 10, Plan Notification Packet Issued: 8.",
             "Provider and plan pairs that appear together most: Northgate Health vs Sunshine Coast Health Plan: 4.",
             "Disputes by agency status: Awaiting Provider Response: 22, Awaiting Plan Response: 8."]
    a = k._records_answer("How many cases are waiting on the provider and how many on the plan?", lines)
    assert a.startswith("Disputes by agency status")


def test_service_words_are_not_party_names_and_full_names_rank_first():
    assert "air" not in {t.lower() for t in k._terms("Which disputes involve air ambulance services?")}
    graphdb.find_entities = lambda tenant, term: [
        {"id": "Sunshine Coast Emergency Physicians, PA", "kind": "Party", "label": "Sunshine Coast Emergency Physicians, PA", "detail": ""},
        {"id": "Sunshine Coast Health Plan", "kind": "Party", "label": "Sunshine Coast Health Plan", "detail": ""},
    ] if term.lower() in ("sunshine", "coast") else []
    ents = k.link_entities("t1", "What connects Northgate and Sunshine Coast Health Plan?")
    assert ents[0]["label"] == "Sunshine Coast Health Plan"
