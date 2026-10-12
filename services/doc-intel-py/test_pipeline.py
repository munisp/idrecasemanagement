"""Pipeline stage tests — heavy models (docling/paddleocr) and the OpenAI
client are stubbed; the endpoint under test is the LOCAL Ollama, never a
network service."""

import sys
import time
import types

import pytest

import pipeline as pl


# --- classify --------------------------------------------------------------

def test_classify_strong_signal_wins():
    dtype, score, margin = pl.classify_text(
        "eob.pdf", "Explanation of Benefits. Allowed amount: $120.00. Patient responsibility applies.")
    assert dtype == "eob"
    assert score >= pl.CLASSIFY_MIN_SCORE


def test_classify_no_signal_is_unrelated():
    dtype, score, margin = pl.classify_text("menu.pdf", "today's specials: soup, salad, sandwich")
    assert dtype == "unrelated"
    assert score == 0


def test_stage_classify_flags_thin_margin():
    ctx = pl.stage_classify({"filename": "x.pdf",
                             "text": "determination and claim number and cpt"}, {})
    assert "classify_ambiguous" in ctx


# --- model singletons --------------------------------------------------------

def test_model_singleton_builds_once_even_under_threads():
    pl._MODEL_CACHE.clear()
    builds = []

    def factory():
        builds.append(1)
        time.sleep(0.02)  # widen the race window
        return object()

    from concurrent.futures import ThreadPoolExecutor
    with ThreadPoolExecutor(max_workers=8) as pool:
        objs = list(pool.map(lambda _: pl._model(("test", 1), factory), range(16)))
    assert len(builds) == 1
    assert all(o is objs[0] for o in objs)


def test_model_cache_keys_by_config():
    pl._MODEL_CACHE.clear()
    a = pl._model(("k", True), lambda: "A")
    b = pl._model(("k", False), lambda: "B")
    assert a == "A" and b == "B"


# --- pipeline spec cache -----------------------------------------------------

def test_load_pipeline_caches_and_invalidates_on_mtime(tmp_path):
    p = tmp_path / "spec.yaml"
    p.write_text("stages: []\n")
    s1 = pl.load_pipeline(str(p))
    s2 = pl.load_pipeline(str(p))
    assert s1 is s2  # cached object, not a re-parse
    time.sleep(0.02)
    p.write_text("stages: [{name: classify}]\n")
    import os
    os.utime(str(p), (time.time() + 5, time.time() + 5))
    s3 = pl.load_pipeline(str(p))
    assert s3["stages"] == [{"name": "classify"}]


# --- verify / grounding ------------------------------------------------------

def test_grounded_tolerates_punctuation_variance():
    hay = "claim number: abc-12345, billed $1,234.56"
    squashed = pl._grounded.__defaults__  # not used; explicit below
    import re
    hs = re.sub(r"[^a-z0-9]", "", hay)
    assert pl._grounded("ABC 12345", hay, hs)
    assert pl._grounded("$1234.56", hay, hs)
    assert not pl._grounded("ZZZ-999", hay, hs)


def test_stage_verify_drops_ungrounded_from_trusted_normalized():
    ctx = {
        "markdown": "billed amount $500.00 for CPT 99213",
        "extracted": {"billed_amount_usd": 500.0, "qpa_usd": 9999.0},
        "normalized": {"billed_amount_usd": 500.0, "qpa_usd": 9999.0},
    }
    ctx = pl.stage_verify(ctx, {})
    assert ctx["field_confidence"]["billed_amount_usd"] == "high"
    assert ctx["field_confidence"]["qpa_usd"] == "low"
    assert "qpa_usd" in ctx["ungrounded_fields"]
    assert "qpa_usd" not in ctx["normalized"]
    assert ctx["normalized"]["billed_amount_usd"] == 500.0


# --- normalization -----------------------------------------------------------

def test_norm_amount_and_date():
    assert pl._norm_amount("$1,234.56") == 1234.56
    assert pl._norm_amount("garbage") is None
    assert pl._norm_date("January 5, 2026") == "2026-01-05"
    assert pl._norm_date("01/05/26") == "2026-01-05"
    assert pl._norm_date("2026-13-45") is None


def test_merge_extractions_first_non_null_and_list_union():
    merged = pl._merge_extractions(
        [{"a": 1, "b": ["x", "y"], "c": None},
         {"a": 2, "b": ["y", "z"], "c": "seen"}],
        ["a", "b", "c"])
    assert merged == {"a": 1, "b": ["x", "y", "z"], "c": "seen"}


# --- VLM extraction: local Ollama stubbed ------------------------------------

def _stub_openai(monkeypatch):
    """stage_vlm_extract builds its client via `from openai import OpenAI`;
    inject a fake module so no real client (or network) exists."""
    fake = types.ModuleType("openai")

    class OpenAI:  # noqa: D401 - test stub
        def __init__(self, **kwargs):
            self.kwargs = kwargs

    fake.OpenAI = OpenAI
    monkeypatch.setitem(sys.modules, "openai", fake)


def _long_ctx(monkeypatch, n_chunks=4):
    monkeypatch.setattr(pl, "_chunk_markdown",
                        lambda md, n: [f"chunk-{i} billed amount ${100+i}.00" for i in range(n_chunks)])
    return {"markdown": "x" * 50, "doc_type": "idr_claim", "pages": []}


_SCHEMAS = {"idr_claim": {"fields": ["billed_amount_usd", "claim_number"]}}


def test_vlm_chunks_run_concurrently_and_merge_in_order(monkeypatch):
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    calls = []

    def fake_call(client, model, content, max_tokens):
        import re as _re
        calls.append(time.monotonic())
        time.sleep(0.15)  # simulate Ollama round-trip
        m = _re.search(r"section (\d+) of", content[0]["text"])
        i = int(m.group(1)) - 1
        return {"_in_domain": True, "billed_amount_usd": 100 + i,
                "claim_number": f"C{i}"}

    monkeypatch.setattr(pl, "_vlm_call", fake_call)
    ctx = _long_ctx(monkeypatch)
    t0 = time.monotonic()
    ctx = pl.stage_vlm_extract(ctx, {"endpoint": "http://ollama:11434/v1",
                                     "model": "qwen2.5:7b-instruct",
                                     "parallel_chunks": 4,
                                     "chunk_chars": 10}, _SCHEMAS)
    elapsed = time.monotonic() - t0
    assert elapsed < 0.45  # 4 x 0.15s serial would be 0.60s+
    # merge is order-deterministic: first non-null wins -> chunk 0's amount
    assert ctx["extracted"]["billed_amount_usd"] == 100
    assert ctx["in_domain"] is True


def test_vlm_parallel_default_is_conservative_for_local_ollama(monkeypatch):
    """Without explicit config the concurrency default must be gentle —
    the endpoint is the shared local Ollama, not OpenAI."""
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    inflight = {"cur": 0, "max": 0}

    def fake_call(client, model, content, max_tokens):
        inflight["cur"] += 1
        inflight["max"] = max(inflight["max"], inflight["cur"])
        time.sleep(0.1)
        inflight["cur"] -= 1
        return {"_in_domain": True, "billed_amount_usd": 1}

    monkeypatch.setattr(pl, "_vlm_call", fake_call)
    monkeypatch.delenv("VLM_PARALLEL_CHUNKS", raising=False)
    ctx = _long_ctx(monkeypatch, n_chunks=6)
    pl.stage_vlm_extract(ctx, {"endpoint": "http://ollama:11434/v1",
                               "model": "m", "chunk_chars": 10}, _SCHEMAS)
    assert inflight["max"] <= 2


def test_vlm_out_of_domain_short_circuits(monkeypatch):
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    monkeypatch.setattr(pl, "_vlm_call",
                        lambda *a, **k: {"_in_domain": False, "billed_amount_usd": None})
    ctx = _long_ctx(monkeypatch, n_chunks=1)
    ctx = pl.stage_vlm_extract(ctx, {"endpoint": "http://x/v1", "model": "m",
                                     "chunk_chars": 10}, _SCHEMAS)
    assert ctx["in_domain"] is False


def test_vlm_client_cached_per_endpoint(monkeypatch):
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    monkeypatch.setattr(pl, "_vlm_call",
                        lambda *a, **k: {"_in_domain": True})
    cfg = {"endpoint": "http://ollama:11434/v1", "model": "m", "chunk_chars": 10}
    pl.stage_vlm_extract(_long_ctx(monkeypatch, 1), cfg, _SCHEMAS)
    pl.stage_vlm_extract(_long_ctx(monkeypatch, 1), cfg, _SCHEMAS)
    clients = [k for k in pl._MODEL_CACHE if k[0] == "vlm_client"]
    assert len(clients) == 1


# --- scan quality (cv2 available, no paddle needed) --------------------------

def test_assess_page_quality_flags_blur_not_crisp():
    from PIL import Image, ImageDraw, ImageFilter, ImageFont
    # NB: int fill on an RGB image is a packed color (255 -> red), not white
    crisp = Image.new("RGB", (1000, 1400), (255, 255, 255))
    d = ImageDraw.Draw(crisp)
    try:
        f = ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 28)
    except OSError:
        f = ImageFont.load_default()
    for y in range(60, 1300, 44):
        d.text((50, y), "Billed amount $1,234.56  CPT 99213  NPI 1234567890",
               font=f, fill=(0, 0, 0))
    blurry = crisp.filter(ImageFilter.GaussianBlur(6))
    q_crisp = pl.assess_page_quality(crisp)
    q_blur = pl.assess_page_quality(blurry)
    assert q_crisp["blur"] > q_blur["blur"]
    assert q_blur["poor"] is True
    assert q_crisp["poor"] is False


# --- tenant schema overrides --------------------------------------------------

def test_run_pipeline_merges_schema_overrides(monkeypatch):
    """NG Program Manifest schemas must SHADOW same-named built-ins and extend
    with new ones, without touching the pipeline spec on disk."""
    captured = {}
    fake_spec = {"stages": [{"name": "vlm_extract", "enabled": True}],
                 "schemas": {"idr_claim": {"fields": {"builtin": {}}}}}
    monkeypatch.setattr(pl, "load_pipeline", lambda: fake_spec)
    def fake_extract(ctx, cfg, schemas):
        captured["schemas"] = schemas
        return ctx
    monkeypatch.setattr(pl, "stage_vlm_extract", fake_extract)
    pl.run_pipeline({}, schema_overrides={"idr_claim": {"fields": {"tenant": {}}},
                                                "custom_doc": {"fields": {"x": {}}}})
    assert captured["schemas"]["idr_claim"]["fields"] == {"tenant": {}}  # shadowed
    assert "custom_doc" in captured["schemas"]                            # extended
    # no overrides -> built-ins pass through untouched
    captured.clear()
    pl.run_pipeline({})
    assert captured["schemas"] == fake_spec["schemas"]


# --- lazy page rasterization -------------------------------------------------

_TINY_PDF = b"""%PDF-1.4
1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj
2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj
3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj
4 0 obj<</Length 58>>stream
BT /F1 24 Tf 100 700 Td (Billed $500.00 CPT 99213) Tj ET
endstream
endobj
5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj
trailer<</Root 1 0 R>>
%%EOF"""


def test_ensure_pages_renders_pdf_lazily_and_caches():
    pypdfium2 = pytest.importorskip("pypdfium2")
    ctx = {"raw_bytes": _TINY_PDF, "content_type": "application/pdf", "pages": []}
    pages = pl.ensure_pages(ctx)
    assert len(pages) == 1
    assert ctx["pages"] is pages
    again = pl.ensure_pages(ctx)
    assert again is pages  # no second render


def test_ensure_pages_noop_for_non_pdf():
    ctx = {"raw_bytes": b"not a pdf", "content_type": "text/plain", "pages": []}
    assert pl.ensure_pages(ctx) == []


# --- OCR stage batching (paddleocr stubbed) ---------------------------------

def _stub_paddle(monkeypatch, predict_fn):
    fake = types.ModuleType("paddleocr")

    class PaddleOCR:
        def __init__(self, **kwargs):
            pass
        def predict(self, inp):
            return predict_fn(inp)

    fake.PaddleOCR = PaddleOCR
    monkeypatch.setitem(sys.modules, "paddleocr", fake)
    pl._MODEL_CACHE.clear()


def _ocr_ctx(n_pages=6):
    from PIL import Image
    page = Image.new("RGB", (400, 300), (255, 255, 255))
    return {"pages": [page.copy() for _ in range(n_pages)],
            "raw_bytes": b"\x89PNG\r\n\x1a\n...", "content_type": "image/png"}


def test_ocr_batches_pages(monkeypatch):
    calls = []

    def predict(inp):
        calls.append(inp)
        n = len(inp) if isinstance(inp, list) else 1
        return [{"rec_texts": [f"line-from-{n}-page-batch"]} for _ in range(n)]

    _stub_paddle(monkeypatch, predict)
    ctx = pl.stage_ocr(_ocr_ctx(6), {"batch_pages": 4})
    # 6 pages at batch 4 -> 2 predict calls, both list input
    assert len(calls) == 2
    assert all(isinstance(c, list) for c in calls)
    assert [len(c) for c in calls] == [4, 2]
    assert "line-from-4-page-batch" in ctx["text"]
    assert "line-from-2-page-batch" in ctx["text"]


def test_ocr_falls_back_to_serial_when_batch_rejected(monkeypatch):
    def predict(inp):
        if isinstance(inp, list) and len(inp) > 1:
            raise RuntimeError("this paddle build rejects list input")
        return [{"rec_texts": ["serial-line"]}]

    _stub_paddle(monkeypatch, predict)
    ctx = pl.stage_ocr(_ocr_ctx(3), {"batch_pages": 4})
    assert ctx["text"].count("serial-line") == 3


# --- validate (all-null extraction) ------------------------------------------

def test_validate_flags_all_null_extraction_even_when_classified_confidently():
    # classify_score > 0 (type classification succeeded) but every extracted
    # field came back null -- previously only reachable as a sub-condition
    # of classify_score == 0, so this exact combination never flagged at all
    # and field_confidence ended up {} (nothing to grade), identical to a
    # clean document with nothing to flag.
    ctx = {
        "extracted": {"qpa_usd": None, "claim_number": "", "service_date": None},
        "normalized": {},
        "classify_score": 0.9,
        "doc_type": "eob",
    }
    out = pl.stage_validate(ctx, {}, None)
    assert any(f["issue"].startswith("Extraction returned no usable field values")
               for f in out["findings"])
    assert out["status"] == "ANALYZED_WITH_FINDINGS"


def test_validate_does_not_flag_when_some_fields_extracted():
    ctx = {
        "extracted": {"qpa_usd": 123.45, "claim_number": None},
        "normalized": {"qpa_usd": 123.45},
        "classify_score": 0.9,
        "doc_type": "eob",
    }
    out = pl.stage_validate(ctx, {}, None)
    assert not any(f["issue"].startswith("Extraction returned no usable field values")
                   for f in out["findings"])


# --- OCR accuracy hardening ---------------------------------------------------

def test_ocr_records_recognizer_confidence(monkeypatch):
    def predict(inp):
        n = len(inp) if isinstance(inp, list) else 1
        return [{"rec_texts": ["some readable text here"], "rec_scores": [0.9, 0.8]}
                for _ in range(n)]

    _stub_paddle(monkeypatch, predict)
    ctx = pl.stage_ocr(_ocr_ctx(2), {"batch_pages": 4, "consensus_min_conf": 0.5})
    assert ctx["ocr_confidence"] == pytest.approx(0.85)
    assert len(ctx["ocr_pages"]) == 2
    assert all(p["engine"] == "paddle" for p in ctx["ocr_pages"])
    assert "ocr_low_confidence" not in ctx


def test_ocr_consensus_rescues_empty_paddle_page(monkeypatch):
    def predict(inp):
        n = len(inp) if isinstance(inp, list) else 1
        return [{"rec_texts": [], "rec_scores": []} for _ in range(n)]

    _stub_paddle(monkeypatch, predict)
    monkeypatch.setattr(pl, "_tesseract_text", lambda img: "rescued claim text")
    ctx = pl.stage_ocr(_ocr_ctx(2), {})
    assert "rescued claim text" in ctx["text"]
    assert all(p["engine"] == "tesseract" for p in ctx["ocr_pages"])
    assert ctx["ocr_low_confidence"] is True


def test_ocr_consensus_records_agreement(monkeypatch):
    def predict(inp):
        return [{"rec_texts": ["allowed amount claim number"], "rec_scores": [0.6]}]

    _stub_paddle(monkeypatch, predict)
    monkeypatch.setattr(pl, "_tesseract_text", lambda img: "allowed amount claim number")
    ctx = pl.stage_ocr(_ocr_ctx(1), {"consensus_min_conf": 0.75})
    assert ctx["ocr_pages"][0]["agreement"] == 1.0
    assert ctx["ocr_pages"][0]["engine"] == "paddle"  # agree -> keep primary


def test_ocr_consensus_absent_when_confident(monkeypatch):
    def predict(inp):
        n = len(inp) if isinstance(inp, list) else 1
        return [{"rec_texts": ["crisp clean text"], "rec_scores": [0.99]} for _ in range(n)]

    _stub_paddle(monkeypatch, predict)
    called = []
    monkeypatch.setattr(pl, "_tesseract_text", lambda img: called.append(1) or "")
    ctx = pl.stage_ocr(_ocr_ctx(2), {"consensus_min_conf": 0.75})
    assert called == []  # second opinion only when actually needed
    assert "ocr_low_confidence" not in ctx


def test_ocr_low_confidence_flag_on_weak_pages(monkeypatch):
    def predict(inp):
        return [{"rec_texts": ["faint but present text here"], "rec_scores": [0.4]}]

    _stub_paddle(monkeypatch, predict)
    monkeypatch.setattr(pl, "_tesseract_text", lambda img: "faint but present text here")
    ctx = pl.stage_ocr(_ocr_ctx(1), {"consensus_min_conf": 0.75})
    assert ctx["ocr_low_confidence"] is True
    assert ctx["ocr_low_confidence_pages"] == [0]


def test_validate_finds_low_ocr_confidence():
    ctx = {"extracted": {"claim_number": "X1"}, "normalized": {},
           "markdown": "claim number X1", "ocr_low_confidence": True,
           "ocr_confidence": 0.42}
    out = pl.stage_validate(pl.stage_verify(ctx, {}), {}, None)
    assert any("OCR confidence" in (f["issue"] or "") for f in out["findings"])


def test_deskew_corrects_small_rotation():
    import cv2
    import numpy as np
    from PIL import Image

    # Horizontal ink bars rotated 5 degrees on white paper.
    canvas = np.full((400, 600, 3), 255, np.uint8)
    for y in range(80, 340, 40):
        cv2.line(canvas, (60, y), (540, y), (0, 0, 0), 6)
    m = cv2.getRotationMatrix2D((300, 200), 5.0, 1.0)
    rotated = cv2.warpAffine(canvas, m, (600, 400), borderValue=(255, 255, 255))
    fixed = pl.deskew_image(Image.fromarray(rotated))
    gray = cv2.cvtColor(np.array(fixed), cv2.COLOR_RGB2GRAY)
    coords = np.column_stack(np.where(gray < 128))
    angle = pl._skew_angle(cv2.minAreaRect(coords)[-1])
    assert abs(angle) < 1.5  # 5 degrees in, sub-1.5 out


def test_deskew_leaves_straight_pages_alone():
    from PIL import Image
    img = Image.new("RGB", (300, 200), (255, 255, 255))
    assert pl.deskew_image(img) is img  # no ink -> untouched


def test_word_overlap():
    assert pl._word_overlap("allowed amount $120", "allowed amount $120") == 1.0
    assert pl._word_overlap("cat dog", "sun moon") == 0.0
    assert 0 < pl._word_overlap("allowed amount claim", "allowed amount number") < 1


def test_vlm_out_of_domain_read_without_underscore(monkeypatch):
    # The deployed model answers "in_domain", not the prompted "_in_domain".
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    monkeypatch.setattr(pl, "_vlm_call",
                        lambda *a, **k: {"in_domain": False, "billed_amount_usd": None})
    ctx = pl.stage_vlm_extract(_long_ctx(monkeypatch, n_chunks=1),
                               {"endpoint": "http://x/v1", "model": "m", "chunk_chars": 10}, _SCHEMAS)
    assert ctx["in_domain"] is False
    assert "in_domain" not in ctx["extracted"]


def test_vlm_rejection_with_image_is_rechecked_text_only(monkeypatch):
    from PIL import Image
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    calls = []

    def fake_call(client, model, content, max_tokens):
        calls.append(len(content))
        if len(content) > 1:  # image attached: the blurry-thumbnail rejection
            return {"in_domain": False, "billed_amount_usd": None}
        return {"in_domain": True, "billed_amount_usd": 1850.0}

    monkeypatch.setattr(pl, "_vlm_call", fake_call)
    ctx = _long_ctx(monkeypatch, n_chunks=1)
    ctx["pages"] = [Image.new("RGB", (800, 1000), "white")]
    ctx = pl.stage_vlm_extract(ctx, {"endpoint": "http://x/v1", "model": "m", "chunk_chars": 10,
                                     "vision": True}, _SCHEMAS)
    assert calls == [2, 1]
    assert ctx["in_domain"] is True
    assert ctx["extracted"]["billed_amount_usd"] == 1850.0


def test_index_body_dates_are_iso_or_omitted():
    body = pl._index_safe_extracted(
        {"determination_date": "06/15/2026", "service_date": "sometime", "claim_number": "C-1"},
        {"determination_date": "2026-06-15"})
    assert body == {"determination_date": "2026-06-15", "claim_number": "C-1"}


def test_skew_angle_folds_both_opencv_conventions():
    assert abs(pl._skew_angle(-85.0) - 5.0) < 1e-9   # pre-4.5.1 range [-90, 0)
    assert abs(pl._skew_angle(5.0) - 5.0) < 1e-9     # post-4.5.1 range (0, 90]
    assert abs(pl._skew_angle(90.0)) < 1e-9
    assert abs(pl._skew_angle(-90.0)) < 1e-9


def test_eligibility_conflicts_reports_disagreement_once():
    from main import eligibility_conflicts
    current = {"disputed_amount_cents": 185000, "provider_type": "physician_dentist", "contracted": False}
    # same values, or nothing read: no conflict
    assert eligibility_conflicts(current, {"disputed_amount_cents": 185000, "provider_type": None, "contracted": False}, "a.pdf") == []
    found = {"disputed_amount_cents": 3140000, "provider_type": "hospital_inpatient", "contracted": True}
    got = eligibility_conflicts(current, found, "VALID-05.pdf")
    assert [c["field"] for c in got] == ["disputed_amount_cents", "provider_type", "contracted"]
    assert got[0] == {"field": "disputed_amount_cents", "case_value": 185000, "document_value": 3140000, "document": "VALID-05.pdf"}
    # the same disagreement from a re-analysis is not recorded twice
    assert eligibility_conflicts(current, found, "VALID-05.pdf", known=got) == []
    # a case with no value yet has nothing to disagree with (the backfill fills it)
    assert eligibility_conflicts({"disputed_amount_cents": None, "provider_type": "", "contracted": None}, found, "x") == []


def test_provider_type_from_text_reads_only_explicit_codes():
    from main import provider_type_from_text as f
    assert f("Service Date: 03/12/2026 Place of Service: 11 (Office) Network Status: Out-of-network") == "physician_dentist"
    assert f("| Place of Service | 21 (Inpatient Hospital) |") == "hospital_inpatient"
    assert f("24B PLACE OF SERVICE 22") == "hospital_outpatient"
    assert f("POS: 19") == "hospital_outpatient"
    assert f("4 TYPE OF BILL 0111") == "hospital_inpatient"
    assert f("Type of Bill: 131") == "hospital_outpatient"
    # codes that do not state a provider type, and forms with none, stay unknown
    assert f("Place of Service: 41 (Ambulance - Land)") is None
    assert f("Type of Bill 0721") is None   # a dialysis clinic, not a hospital
    assert f("HEALTH INSURANCE CLAIM FORM CMS-1500") is None
    assert f("Explanation of Benefits — physician services") is None
    # labels first, values after (how a form is often read)
    assert f("Service Date:\n\n03/12/2026\n\nPlace of Service:\n\nNetwork Status:\n\nCPT / HCPCS Codes:\n\n11 (Office)\n\nOut-of-network") == "physician_dentist"
    assert f("Place of Service:\n\nDischarge Date:\n\n21 (Inpatient Hospital)") == "hospital_inpatient"
    assert f("Units: 11 (Office visits)") is None   # no place-of-service field on the page
    assert f("") is None and f(None) is None


def test_report_fields_from_documents():
    from main import line_of_business_from as lob, disputed_issue_from as issue, provider_address_from as addr
    assert lob("Florida Medicaid MMA") == "Medicaid"
    assert lob("Medicare Advantage HMO") == "Medicare Advantage"
    assert lob("Medicare Part B") == "Medicare"
    assert lob("Health Insurance Marketplace") == "Marketplace"
    assert lob("Employer group PPO") == "Commercial"
    assert lob("") is None and lob(None) is None and lob("Sunshine Plan") is None
    assert issue(1850.0, 412.0, []) == "Underpayment"
    assert issue(500.0, 0.0, ["CO-29"]) == "Denial"
    assert issue(100.0, 150.0, []) == "Overpayment"
    assert issue(100.0, 100.0, []) is None      # paid in full: nothing to name
    assert issue(None, 412.0, []) is None and issue(1850.0, None, []) is None
    a = addr({"provider_street": "1200 Bayshore Blvd", "provider_city": "Tampa", "provider_state": "fl", "provider_zip": "33606-1234"})
    assert a == {"provider_street": "1200 Bayshore Blvd", "provider_city": "Tampa", "provider_state": "FL", "provider_postal": "33606-1234"}
    # parts that do not fit the report's formats are left out, not stored wrong
    assert addr({"provider_state": "Florida", "provider_zip": "3360", "provider_city": "Tampa"}) == {"provider_city": "Tampa"}
    assert addr({}) == {}


def test_infer_provider_type_from_an_eob():
    from main import infer_provider_type as f
    assert f("Northgate Health, P.C.", "Explanation of Benefits ... Office visit, established patient (99215)")[0] == "physician_dentist"
    assert f("Bayside Dental Associates", "")[0] == "physician_dentist"
    assert f("Jane Roe, M.D.", "")[0] == "physician_dentist"
    assert f("St. Luke's Regional Hospital", "Inpatient admission 02/03/2026, room and board, DRG 470")[0] == "hospital_inpatient"
    assert f("Gulf Coast Medical Center", "Emergency department visit, outpatient")[0] == "hospital_outpatient"
    assert f("St. Luke's Regional Hospital", "claim processed")[0] is None       # a hospital, setting not stated
    assert f("Acme Ambulance Transport", "ground transport A0429")[0] is None
    assert f("", "Office visit 99213")[0] == "physician_dentist"                 # by procedure code alone
    assert f(None, None)[0] is None
    typ, basis = f("Northgate Health, P.C.", "")
    assert "Northgate Health, P.C." in basis
    # an EOB's service lines are rows, not strings (this crashed the read)
    rows = [{"service": "Office visit, established patient (99215)", "billed_amount_usd": 1450.0},
            {"service": "Electrocardiogram (93000)", "billed_amount_usd": 400.0}]
    assert f("Northgate Health, P.C.", "", rows)[0] == "physician_dentist"
    assert f("", "", rows) == ("physician_dentist", "office-visit procedure codes (99202-99215) are billed")
