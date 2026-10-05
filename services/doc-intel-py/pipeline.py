"""Composable document-analysis stages (Docling-first, PaddleOCR fallback).

Each stage is a function stage_<name>(ctx) -> ctx that reads/writes keys on a
shared context dict. pipeline.yaml decides which stages run, in what order, and
under what condition (`when: <ctx-flag>`): Docling is the primary parser,
PaddleOCR is conditionally docked for scanned pages, PP-StructureV3 handles
seal/stamp detection, and the VLM stage does semantic extraction.
"""

from __future__ import annotations

import io
import json
import os
import re
from typing import Any

import httpx
import yaml
from PIL import Image

# ---------------------------------------------------------------------------
# Stage implementations
# ---------------------------------------------------------------------------

# Keyword signals per doc type, scored against extracted text. Classification
# runs twice: stage 1 (filename only, before parsing) and stage 2 (re-classify
# with full text after Docling/OCR — a misleading filename can't win).
TYPE_SIGNALS = {
    "eob": ["explanation of benefits", "allowed amount", "patient responsibility",
            "claim number", "remittance", "denial code", "coinsurance"],
    "determination_letter": ["independent dispute resolution", "determination",
            "prevailing party", "certified idre", "45 cfr", "dispute resolution entity",
            "offer selected", "out-of-network rate"],
    "idr_claim": ["cpt", "hcpcs", "billed amount", "qualifying payment amount",
            "qpa", "service date", "place of service", "npi", "taxonomy",
            "itemized", "ub-04", "cms-1500", "diagnosis code", "procedure code",
            "claim", "provider", "payer", "member id"],
}


def classify_text(name: str, text: str) -> tuple[str, int]:
    """Best-scoring doc type and its score; ('unrelated', 0) when nothing matches."""
    hay = (name + " " + (text or "").lower()[:20000])
    best, best_score = "unrelated", 0
    for dtype, signals in TYPE_SIGNALS.items():
        score = sum(1 for s in signals if s in hay)
        # Filename hits count double — names are deliberate, body text noisy.
        score += sum(1 for s in signals if s in name)
        if score > best_score:
            best, best_score = dtype, score
    return best, best_score


def stage_classify(ctx: dict, cfg: dict) -> dict:
    """Doc-type guess: filename + (when available) extracted text. Documents
    with no IDR signal at all are typed 'unrelated' rather than force-fit
    into idr_claim — validate() routes those for manual review."""
    name = (ctx.get("filename") or "").lower()
    dtype, score = classify_text(name, ctx.get("text", ""))
    ctx["doc_type"] = dtype
    ctx["classify_score"] = score
    return ctx


def stage_docling(ctx: dict, cfg: dict) -> dict:
    """PRIMARY parser — IBM Docling: PDF/DOCX/PPTX/HTML/images into a structured
    document (reading order, layout regions, tables, figures, formulas)."""
    from docling.datamodel.base_models import DocumentStream
    from docling.datamodel.pipeline_options import PdfPipelineOptions
    from docling.document_converter import DocumentConverter, PdfFormatOption
    from docling.datamodel.base_models import InputFormat

    opts = PdfPipelineOptions(
        do_ocr=cfg.get("do_ocr", True),
        do_table_structure=cfg.get("do_table_structure", True),
    )
    converter = DocumentConverter(
        format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=opts)}
    )
    stream = DocumentStream(name=ctx.get("filename", "doc.pdf"),
                            stream=io.BytesIO(ctx["raw_bytes"]))
    doc = converter.convert(stream).document

    ctx["text"] = doc.export_to_markdown()            # structure-aware reading order
    ctx["markdown"] = ctx["text"]
    ctx["tables"] = [
        t.export_to_dataframe().to_dict(orient="records")
        for t in getattr(doc, "tables", [])
    ]
    ctx["layout_regions"] = [
        {"label": getattr(item, "label", "text"), "content": getattr(item, "text", "")[:2000]}
        for item in getattr(doc, "texts", [])
    ]
    # Low text coverage => scanned document => conditionally dock PaddleOCR.
    n_pages = max(1, len(ctx.get("pages", [])) or 1)
    ctx["low_text_coverage"] = len(ctx["text"].strip()) < 40 * n_pages
    return ctx


def stage_ocr(ctx: dict, cfg: dict) -> dict:
    """PaddleOCR full-text extraction."""
    from paddleocr import PaddleOCR

    ocr = PaddleOCR(
        lang=cfg.get("lang", "en"),
        use_doc_orientation_classify=cfg.get("use_doc_orientation_classify", True),
        use_doc_unwarping=cfg.get("use_doc_unwarping", True),
        use_textline_orientation=cfg.get("use_textline_orientation", True),
    )
    texts: list[str] = []
    for page in ctx["pages"]:  # list[PIL.Image]
        result = ocr.predict(np_from_pil(page))
        for res in result:
            texts.extend(res.get("rec_texts", []))
    ctx["text"] = "\n".join(texts)
    return ctx


def stage_layout(ctx: dict, cfg: dict) -> dict:
    """PP-StructureV3 layout analysis: regions, reading order, seals/stamps."""
    from paddleocr import PPStructureV3

    engine = PPStructureV3(
        use_table_recognition=cfg.get("use_table_recognition", True),
        use_seal_recognition=cfg.get("use_seal_recognition", True),
    )
    regions: list[dict] = []
    for i, page in enumerate(ctx["pages"]):
        for res in engine.predict(np_from_pil(page)):
            for blk in res.get("parsing_res_list", []):
                regions.append({
                    "page": i,
                    "label": blk.get("block_label"),
                    "content": blk.get("block_content", "")[:2000],
                })
    ctx["layout_regions"] = regions
    ctx["seal_detected"] = any(r["label"] == "seal" for r in regions)
    return ctx


def stage_vlm_extract(ctx: dict, cfg: dict, schema_fields: list[str]) -> dict:
    """VLM semantic extraction via an OpenAI-compatible endpoint
    (PaddleOCR-VL or Qwen2.5-VL served by vLLM; swap with one env var)."""
    from openai import OpenAI

    client = OpenAI(base_url=cfg["endpoint"], api_key=os.environ.get("VLM_API_KEY", "none"))
    tables_hint = json.dumps(ctx.get("tables", [])[:3])[:3000]
    prompt = (
        "You are an IDR (No Surprises Act dispute) document analyst. "
        "FIRST decide whether this document is related to medical billing, health "
        "insurance claims, explanation-of-benefits, or IDR arbitration at all, and "
        'set "_in_domain" true or false. A resume, menu, tax form, legal contract, '
        "photograph with no document content, or any other unrelated material is "
        "false. When false, set every other field null and stop. "
        f"Extract these fields as strict JSON (null when absent): {', '.join(schema_fields)}.\n"
        "Only extract values that literally appear in the document — never invent "
        "or infer identifiers or amounts.\n"
        "Document content (markdown with layout) follows, then extracted tables.\n\n"
        + ctx.get("markdown", ctx.get("text", ""))[:10000]
        + "\n\nTABLES:\n" + tables_hint
    )
    # Multimodal: send first page image alongside the OCR text for layout cues.
    img_b64 = pil_to_b64(ctx["pages"][0]) if ctx.get("pages") else None
    content: list[dict] = [{"type": "text", "text": prompt}]
    if img_b64:
        content.append({"type": "image_url",
                        "image_url": {"url": f"data:image/png;base64,{img_b64}"}})
    resp = client.chat.completions.create(
        model=cfg["model"],
        messages=[{"role": "user", "content": content}],
        max_tokens=cfg.get("max_tokens", 2048),
        temperature=0,
    )
    raw = resp.choices[0].message.content or "{}"
    m = re.search(r"\{.*\}", raw, re.S)
    parsed = json.loads(m.group(0)) if m else {}
    ctx["in_domain"] = bool(parsed.pop("_in_domain", True))
    # Strict schema: only declared fields survive, all present (null-filled) —
    # the VLM can't smuggle invented keys into the record.
    ctx["extracted"] = {f: parsed.get(f) for f in schema_fields}
    return ctx


def stage_validate(ctx: dict, cfg: dict, case: dict | None) -> dict:
    """Cross-check extracted fields against the case record; mismatches flag."""
    findings = []
    ex = ctx.get("extracted", {})
    if case:
        if ex.get("qpa_usd") and case.get("qpa_cents"):
            if abs(float(ex["qpa_usd"]) * 100 - case["qpa_cents"]) > 100:
                findings.append({"field": "qpa_usd", "issue": "QPA mismatch vs case record"})
        if ex.get("claim_number") and case.get("case_number"):
            # claim vs CMS case number are different identifiers; record both
            ctx["extracted"]["cms_case_number"] = case["case_number"]
    # Out-of-domain guard: VLM verdict, zero-signal classification, or an
    # all-null extraction each independently mean a human should look before
    # this document is treated as case evidence.
    ex_fields = {k: v for k, v in ex.items() if v not in (None, "", [], {})}
    if ctx.get("in_domain") is False:
        findings.append({"field": None, "issue": "Document is not related to medical billing or IDR — possible mis-upload; routed for manual review"})
        ctx["doc_type"] = "unrelated"
    elif ctx.get("classify_score", 1) == 0 and not ex_fields:
        findings.append({"field": None, "issue": "No IDR-relevant content detected — possible mis-upload; routed for manual review"})
    ctx["findings"] = findings
    ctx["status"] = "ANALYZED" if not findings else "ANALYZED_WITH_FINDINGS"
    return ctx


# ---------------------------------------------------------------------------
# Docking engine
# ---------------------------------------------------------------------------

def load_pipeline(path: str = "pipeline.yaml") -> dict:
    with open(path) as f:
        return yaml.safe_load(f)


def run_pipeline(ctx: dict, case: dict | None = None) -> dict:
    spec = load_pipeline()
    schema_name = None
    for st in spec["stages"]:
        if not st.get("enabled", True):
            continue
        # Conditional docking: a stage runs only when its `when` flag is truthy
        # in the pipeline context (e.g. PaddleOCR only when Docling found a scan).
        cond = st.get("when")
        if cond and not ctx.get(cond):
            continue
        name, cfg = st["name"], st.get("config", {})
        if name == "vlm_extract":
            schema_name = cfg.get("schema", "idr_claim")
            fields = spec["schemas"][schema_name]["fields"]
            ctx = stage_vlm_extract(ctx, cfg, fields)
        elif name == "validate":
            ctx = stage_validate(ctx, cfg, case)
        else:
            fn = globals().get(f"stage_{name}")
            if fn is None:
                raise RuntimeError(f"pipeline stage not implemented: {name}")
            ctx = fn(ctx, cfg)
    ctx.setdefault("status", "ANALYZED")
    return ctx


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def np_from_pil(img: Image.Image):
    import numpy as np
    return np.array(img.convert("RGB"))


def pil_to_b64(img: Image.Image) -> str:
    import base64
    buf = io.BytesIO()
    img.convert("RGB").save(buf, format="PNG")
    return base64.b64encode(buf.getvalue()).decode()


def pdf_to_pages(data: bytes, dpi: int = 200) -> list[Image.Image]:
    """Rasterize PDF pages (pypdfium2 bundled with PaddleOCR toolchain)."""
    import pypdfium2 as pdfium

    pdf = pdfium.PdfDocument(data)
    pages = []
    for page in pdf:
        pages.append(page.render(scale=dpi / 72).to_pil())
    return pages


def bytes_to_pages(data: bytes, content_type: str) -> list[Image.Image]:
    if "pdf" in content_type:
        return pdf_to_pages(data)
    return [Image.open(io.BytesIO(data))]
