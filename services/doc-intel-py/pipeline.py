"""Composable document-analysis stages (Docling-first, PaddleOCR fallback).

Each stage is a function stage_<name>(ctx) -> ctx that reads/writes keys on a
shared context dict. pipeline.yaml decides which stages run, in what order, and
under what condition (`when: <ctx-flag>`): Docling is the primary parser,
PaddleOCR is conditionally docked for scanned pages, PP-StructureV3 handles
seal/stamp detection, and the VLM stage does semantic extraction.

Accuracy architecture (v2):
  classify (weighted, margin-aware) -> docling -> ocr? -> layout
    -> classify (2nd pass) -> vlm_extract (per-doc-type schema, chunked,
       multi-page, self-repairing JSON, transport retries)
    -> normalize (deterministic canonicalization of amounts/dates/codes)
    -> verify (grounding check: every extracted value must literally appear
       in the document text; per-field confidence)
    -> validate (cross-checks vs case record + review routing)
    -> index

Performance architecture (v3):
  * Heavy models (Docling converter, PaddleOCR, PP-StructureV3) are process-
    level SINGLETONS keyed by their config — construction is model-loading
    from disk, so per-document construction was multiplying every document's
    latency by seconds and churning worker memory (a real OOM vector on a
    multi-tenant queue).
  * VLM chunk extraction runs CONCURRENTLY (config: parallel_chunks) — the
    calls are network-bound, and a 40-page itemized bill was paying N serial
    round-trips. Merge order is preserved; results are deterministic.
  * Scan-quality assessment measures on downscaled pages with a Gaussian
    pre-filter — fastNlMeansDenoising is a beautiful denoiser and a terrible
    thing to run on every page of every document on CPU.
  * Degraded-scan recovery re-renders and enhances ONLY the poor pages.
  * verify() squashes the document haystack ONCE, not per extracted value;
    VLM page images ship as JPEG, not PNG (5-10x smaller, faster encode).
"""

from __future__ import annotations

import io
import json
import os
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import httpx
import otel_setup
import yaml
from eobshape import as_money, as_text, fill_from_text, reconcile_totals, shape_extraction, split_codes
from PIL import Image

# ---------------------------------------------------------------------------
# Model singletons — construction IS model loading; never per-document
# ---------------------------------------------------------------------------

_MODEL_CACHE: dict[tuple, Any] = {}
_MODEL_LOCK = threading.Lock()
# PDFium (pypdfium2, and docling's PDF backend on top of it) is not safe to
# use from several threads at once. With three consumer workers, a bulk
# upload's documents were opened concurrently and the process died with
# "double free or corruption" (exit 139), over and over, stalling every
# tenant's queue. Every PDFium use in this process holds this lock.
PDFIUM_LOCK = threading.RLock()


def _model(key: tuple, factory) -> Any:
    """Process-level singleton for heavy model objects. Double-checked
    locking: workers are typically single-threaded per replica, but the VLM
    stage already parallelizes and stage code must stay safe under threads."""
    if key not in _MODEL_CACHE:
        with _MODEL_LOCK:
            if key not in _MODEL_CACHE:
                _MODEL_CACHE[key] = factory()
    return _MODEL_CACHE[key]

# ---------------------------------------------------------------------------
# Stage implementations
# ---------------------------------------------------------------------------

# Weighted keyword signals per doc type. Strong signals (weight 3) are phrases
# that essentially only appear in that document type; weak signals (weight 1)
# are common billing vocabulary that needs corroboration. Classification runs
# twice: pass 1 (filename only, before parsing) and pass 2 (full text after
# Docling/OCR — a misleading filename can't win).
TYPE_SIGNALS: dict[str, dict[str, list[str]]] = {
    "eob": {
        "strong": ["explanation of benefits", "remittance advice", "patient responsibility",
                   "allowed amount", "adjustment amount", "claim adjustment reason",
                   # an 835 remittance printout (Epic's "Claim Details" view): remit_docs.py
                   "claim charge amount", "claim paid amount", "payer control number",
                   "claim filing indicator", "service line summary"],
        "weak": ["claim number", "denial code", "coinsurance", "copay", "deductible",
                 "eob", "benefits", "covered", "not covered"],
    },
    "determination_letter": {
        "strong": ["independent dispute resolution", "certified idre", "prevailing party",
                   "offer selected", "45 cfr", "dispute resolution entity",
                   "notice of idr determination", "determination of payment"],
        "weak": ["determination", "out-of-network rate", "dispute", "arbitration",
                 "idre", "batched", "attest"],
    },
    "idr_claim": {
        "strong": ["qualifying payment amount", "ub-04", "ub04", "cms-1500", "hcpcs",
                   "place of service", "taxonomy", "itemized bill", "type of bill",
                   "treatment authorization codes", "statement covers period"],
        "weak": ["cpt", "billed amount", "qpa", "service date", "npi", "diagnosis code",
                 "procedure code", "claim", "provider", "payer", "member id", "units",
                 "revenue code", "drg"],
    },
    # The patient's clinical record (ER note, discharge summary, operative report...): evidence in nearly every
    # dispute -- the program's packet lists it ("Medical records") -- though it carries no billing vocabulary.
    "medical_records": {
        "strong": ["discharge summary", "history of present illness", "chief complaint", "emergency department record",
                   "emergency department note", "progress note", "operative report", "assessment and plan",
                   "review of systems", "medication administration record", "nursing note", "admission note",
                   "consultation note", "radiology report", "pathology report", "physical examination"],
        "weak": ["diagnosis:", "treated with", "history of", "ct scan", "x-ray", "vital signs", "allergies",
                 "admitted", "discharged", "medications", "medical record"],
    },
}
# The office's own letters (decision report, cover letter, review invoice, IRO decision): letter_docs.py.
import letter_docs as _letter_docs  # noqa: E402
TYPE_SIGNALS.update(_letter_docs.TYPE_SIGNALS)
# The program's filing packet (Capitol Bridge): packet_docs.py.
import packet_docs as _packet_docs  # noqa: E402
TYPE_SIGNALS.update(_packet_docs.TYPE_SIGNALS)
# An 835 remittance printout, a payer's EOB and a UB-04, read for the claims table: remit_docs.py.
import remit_docs as _remit_docs  # noqa: E402
WEIGHT = {"strong": 3, "weak": 1}

# Minimum score to accept a classification at all, and minimum lead over the
# runner-up to accept it without an ambiguity flag.
CLASSIFY_MIN_SCORE = 2
CLASSIFY_MIN_MARGIN = 2


def classify_text(name: str, text: str) -> tuple[str, int, int]:
    """(best_type, best_score, margin). ('unrelated', 0, 0) when no signal."""
    body = (text or "").lower()[:20000]
    name_l = (name or "").lower()
    scores: dict[str, int] = {}
    for dtype, tiers in TYPE_SIGNALS.items():
        s = 0
        for tier, signals in tiers.items():
            w = WEIGHT[tier]
            for sig in signals:
                if sig in body:
                    s += w
                # Filename hits count double — names are deliberate, body noisy.
                if sig in name_l:
                    s += 2 * w
        scores[dtype] = s
    ranked = sorted(scores.items(), key=lambda kv: kv[1], reverse=True)
    best, best_score = ranked[0]
    margin = best_score - ranked[1][1]
    if best_score < CLASSIFY_MIN_SCORE:
        return "unrelated", 0, 0
    return best, best_score, margin


def stage_classify(ctx: dict, cfg: dict) -> dict:
    """Doc-type guess: filename + (when available) extracted text. Documents
    with no IDR signal are typed 'unrelated'; a winning type with a thin
    margin over the runner-up is kept but flagged ambiguous — validate()
    routes ambiguous docs for a human glance instead of trusting the coin
    flip."""
    name = ctx.get("filename") or ""
    dtype, score, margin = classify_text(name, ctx.get("text", ""))
    ctx["doc_type"] = dtype
    ctx["classify_score"] = score
    ctx["classify_ambiguous"] = bool(dtype != "unrelated" and margin < CLASSIFY_MIN_MARGIN)
    return ctx


def stage_docling(ctx: dict, cfg: dict) -> dict:
    """PRIMARY parser — IBM Docling: PDF/DOCX/PPTX/HTML/images into a structured
    document (reading order, layout regions, tables, figures, formulas)."""
    from docling.datamodel.base_models import DocumentStream

    def build():
        from docling.datamodel.pipeline_options import PdfPipelineOptions
        from docling.document_converter import DocumentConverter, PdfFormatOption
        from docling.datamodel.base_models import InputFormat
        opts = PdfPipelineOptions(
            do_ocr=cfg.get("do_ocr", True),
            do_table_structure=cfg.get("do_table_structure", True),
        )
        return DocumentConverter(
            format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=opts)}
        )

    converter = _model(("docling", cfg.get("do_ocr", True),
                        cfg.get("do_table_structure", True)), build)
    stream = DocumentStream(name=ctx.get("filename", "doc.pdf"),
                            stream=io.BytesIO(ctx["raw_bytes"]))
    with PDFIUM_LOCK:
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
    # An uploaded IMAGE (photo, scan, fax) is all pixels: Docling's built-in
    # OCR may return a little text from a faint page and so look "covered",
    # which skipped the full OCR pass (quality assessment, enhancement,
    # second-engine check). Sparse text on an image always gets that pass.
    if sniff_doc_kind(ctx.get("raw_bytes", b""), ctx.get("content_type", "")) == "image" \
            and len(ctx["text"].strip()) < 400 * n_pages:
        ctx["low_text_coverage"] = True
    return ctx


# Scan-quality thresholds (empirical; tune against your document mix).
# NOTE: blur is now measured on 1400px-downscaled, Gaussian-smoothed pages
# (see assess_page_quality) — a different variance scale than the original
# full-res NLM measurement. 80.0 remains a sane focus floor at this scale,
# but recalibrate against a labeled sample of your real scan mix.
_BLUR_MIN = 80.0        # Laplacian variance (denoised) below this => out of focus
_SEPARATION_MIN = 40.0  # bg median - fg p5 below this => faint/washed-out print
_BRIGHTNESS_LO = 60.0   # mean below => underexposed (overexposure is caught
                        # by separation: washed-out pages have no dark ink)


def assess_page_quality(img: Image.Image) -> dict:
    """Objective scan-quality metrics via OpenCV (bundled with PaddleOCR):
    - blur: Laplacian variance, measured after light denoising (raw variance
      is inflated by sensor noise, which masquerades as sharpness)
    - separation: background median minus foreground 5th percentile — the
      ink-to-paper gap. Whole-page stddev is useless here: sparse crisp text
      scores low on it, while a faint scan's defining trait is exactly that
      its darkest pixels never get dark.
    - exposure: mean brightness out of range.
    Returns metrics + a poor flag. A 'valid but barely legible' document is
    detected HERE — before OCR — so the pipeline can enhance and reviewers
    get an accurate diagnosis instead of a generic 'no content' finding."""
    import cv2

    gray = cv2.cvtColor(np_from_pil(img), cv2.COLOR_RGB2GRAY)
    # Downscale first: blur/separation/exposure are stable at assessment
    # scale, and a 300dpi page is ~8MP of wasted arithmetic otherwise.
    h, w = gray.shape
    if max(h, w) > 1400:
        s = 1400 / max(h, w)
        gray = cv2.resize(gray, (int(w * s), int(h * s)), interpolation=cv2.INTER_AREA)
    # Gaussian pre-filter in place of fastNlMeansDenoising: NLM is the right
    # denoiser for RESTORATION, but for a variance measurement it only needs
    # sensor noise suppressed — a 5x5 Gaussian does that in microseconds
    # where NLM costs seconds per page on CPU.
    smooth = cv2.GaussianBlur(gray, (5, 5), 0)
    blur = float(cv2.Laplacian(smooth, cv2.CV_64F).var())
    # Separation = how much darker the ink is than the paper, measured over
    # actual ink pixels (anything below near-white). Percentile-of-page
    # approaches fail on sparse pages; whole-page stddev fails everywhere.
    ink = gray[gray < 240]
    separation = float(240 - ink.mean()) if ink.size >= 100 else 0.0
    brightness = float(gray.mean())
    poor = (blur < _BLUR_MIN or separation < _SEPARATION_MIN
            or brightness < _BRIGHTNESS_LO)
    return {"blur": round(blur, 1), "separation": round(separation, 1),
            "brightness": round(brightness, 1), "poor": poor}


def _skew_angle(raw_angle: float) -> float:
    """Fold a minAreaRect angle into (-45, 45]. OpenCV builds disagree on the
    raw range ([-90, 0) before 4.5.1, (0, 90] after); handling only one of
    them read a 5-degree skew as 85 degrees on the other, which is outside
    the correctable range -- so deskew silently did nothing."""
    return ((raw_angle + 45) % 90) - 45


def upright(img: Image.Image) -> tuple[Image.Image, int]:
    """Turn a page that was scanned or faxed sideways or upside down the right
    way up. Returns (page, degrees turned clockwise). Uses Tesseract's
    orientation detector; a page it is unsure about is left as it is."""
    try:
        import pytesseract
        probe = img.convert("L")
        if max(probe.size) > 2000:
            probe.thumbnail((2000, 2000))
        osd = pytesseract.image_to_osd(probe, output_type=pytesseract.Output.DICT, config="--psm 0")
        turn, conf = int(osd.get("rotate", 0)) % 360, float(osd.get("orientation_conf", 0))
    except Exception:  # noqa: BLE001 -- too little text to tell, or no tesseract: leave it
        return img, 0
    if turn not in (90, 180, 270) or conf < 3.0:
        return img, 0
    return img.rotate(-turn, expand=True), turn


def image_bytes(pages: list[Image.Image], raw: bytes) -> bytes:
    """The pages written back in the upload's own image format (so the text
    reader sees the turned pages, not the original bytes)."""
    try:
        fmt = Image.open(io.BytesIO(raw)).format or "PNG"
    except Exception:  # noqa: BLE001
        fmt = "PNG"
    if fmt not in ("PNG", "JPEG", "TIFF", "BMP", "WEBP"):
        fmt = "PNG"
    buf = io.BytesIO()
    first = pages[0].convert("RGB") if fmt == "JPEG" else pages[0]
    if len(pages) > 1 and fmt == "TIFF":
        first.save(buf, format="TIFF", save_all=True, append_images=pages[1:])
    else:
        first.save(buf, format=fmt)
    return buf.getvalue()


def skew_degrees(img: Image.Image, span: float = 12.0) -> float:
    """How far a page's text lines are tilted, in degrees (0 when straight or
    when it cannot tell). Tries small rotations and keeps the one where the
    rows of text line up sharpest (the variance of the row ink totals peaks).
    A page is only called crooked when that beats leaving it as it is by a
    clear margin, so a dark or speckled page is not turned on noise."""
    import cv2
    import numpy as np

    g = np.asarray(img.convert("L"))
    scale = 1000 / max(g.shape)
    if scale < 1:
        g = cv2.resize(g, None, fx=scale, fy=scale, interpolation=cv2.INTER_AREA)
    ink = cv2.adaptiveThreshold(g, 1, cv2.ADAPTIVE_THRESH_MEAN_C, cv2.THRESH_BINARY_INV, 31, 15).astype(np.float32)
    if ink.sum() < 500 or ink.mean() > 0.4:
        return 0.0
    h, w = ink.shape

    def score(a: float) -> float:
        m = cv2.getRotationMatrix2D((w / 2, h / 2), float(a), 1.0)
        return float(np.var(cv2.warpAffine(ink, m, (w, h), flags=cv2.INTER_NEAREST, borderValue=0).sum(axis=1)))

    coarse = max(np.arange(-span, span + 0.01, 0.5), key=score)
    fine = max(np.arange(coarse - 0.5, coarse + 0.51, 0.1), key=score)
    if score(fine) < 1.15 * score(0.0):
        return 0.0
    return round(float(fine), 1)


def straighten_pages(pages: list[Image.Image]) -> tuple[list[Image.Image], list[tuple[int, float]]]:
    """Straighten pages fed in crooked (1 degree or more): (pages, [(page number, degrees)]).
    Read crooked, a form's labels and values drift onto other lines and a
    model may not recognise it as a bill at all."""
    out, done = [], []
    for i, p in enumerate(pages):
        a = skew_degrees(p)
        if abs(a) >= 1.0:
            fill = 255 if p.mode in ("L", "1") else (255, 255, 255)
            p = (p if p.mode in ("L", "RGB") else p.convert("RGB")).rotate(a, resample=Image.BICUBIC, fillcolor=fill)
            done.append((i + 1, a))
        out.append(p)
    return out, done


def upright_pages(pages: list[Image.Image]) -> tuple[list[Image.Image], list[tuple[int, int]]]:
    """upright() over every page: (pages, [(page number, degrees turned)])."""
    out, turned = [], []
    for i, p in enumerate(pages):
        q, deg = upright(p)
        out.append(q)
        if deg:
            turned.append((i + 1, deg))
    return out, turned


def deskew_image(img: Image.Image) -> Image.Image:
    """Estimate and correct page skew via the ink pixels' minimum-area
    rectangle. Scanners and phone photos routinely land 1-5 degrees off, and
    text-line detectors degrade well before the skew is visible to a human.
    Only angles in (0.3, 15) degrees are applied — anything larger is almost
    certainly the rectangle fitting page content (a table, a signature)
    rather than the page, and rotating on that would CREATE skew."""
    import cv2
    import numpy as np

    gray = cv2.cvtColor(np_from_pil(img), cv2.COLOR_RGB2GRAY)
    ink = cv2.threshold(gray, 0, 255, cv2.THRESH_BINARY_INV + cv2.THRESH_OTSU)[1]
    coords = np.column_stack(np.where(ink > 0))
    if len(coords) < 200:
        return img
    angle = _skew_angle(cv2.minAreaRect(coords)[-1])
    if abs(angle) < 0.3 or abs(angle) > 15:
        return img
    # minAreaRect's angle sign convention shifts with the rectangle's aspect
    # and OpenCV version — rotating the wrong way DOUBLES the skew. Rotate,
    # re-measure, and flip the sign if we made it worse.
    def skew_of(arr):
        g = cv2.cvtColor(arr, cv2.COLOR_RGB2GRAY) if arr.ndim == 3 else arr
        c = np.column_stack(np.where(g < 128))
        if len(c) < 200:
            return 0.0
        return _skew_angle(cv2.minAreaRect(c)[-1])

    h, w = gray.shape
    raw = np_from_pil(img)

    def rot(a):
        m = cv2.getRotationMatrix2D((w // 2, h // 2), a, 1.0)
        return cv2.warpAffine(raw, m, (w, h), flags=cv2.INTER_CUBIC,
                              borderMode=cv2.BORDER_REPLICATE)

    cand = rot(angle)
    if abs(skew_of(cand)) > abs(angle):
        cand = rot(-angle)
    if abs(skew_of(cand)) >= abs(angle) - 0.2:
        return img  # neither direction actually helps — leave the page alone
    return Image.fromarray(cand)


def preprocess_for_ocr(img: Image.Image) -> Image.Image:
    """Enhancement pass for degraded scans before OCR: deskew (rotation is the
    cheapest accuracy win on scanned pages), periodic-watermark removal (the
    single biggest accuracy factor on stamped/faxed EOBs and claim forms —
    CLAHE without it amplifies the watermark lattice exactly as much as the
    text), grayscale, CLAHE local contrast (recovers faint print), fast
    denoise, and 2x upscale when the page is small (sub-150dpi effective
    resolution starves the detector)."""
    import cv2

    import dewatermark

    img = deskew_image(img)
    gray = cv2.cvtColor(np_from_pil(img), cv2.COLOR_RGB2GRAY)
    # enhance_page: lattice detection + subtraction (no-op on clean pages),
    # median speckle cleanup, CLAHE — in the order that removes structure
    # before amplifying contrast.
    enhanced, _removed = dewatermark.enhance_page(gray)
    h, w = enhanced.shape
    if max(h, w) < 2000:
        enhanced = cv2.resize(enhanced, (w * 2, h * 2), interpolation=cv2.INTER_CUBIC)
    return Image.fromarray(enhanced)


def _page_ink_coverage(img: Image.Image) -> float:
    """Fraction of dark pixels — distinguishes 'blank page' from 'text the
    OCR failed to read', which is the decision input for a hi-res retry."""
    import cv2
    import numpy as np

    gray = cv2.cvtColor(np_from_pil(img), cv2.COLOR_RGB2GRAY)
    return float(np.mean(gray < 200))


def _tesseract_text(img: Image.Image) -> str:
    """Second-opinion OCR via tesseract. Optional by design: any failure
    (binary missing, import error, crash) returns empty rather than costing
    the document its primary OCR result.

    PSM fallback chain: automatic page segmentation (3) handles clean
    multi-column layouts, but its layout analysis collapses on degraded
    fax scans and returns NOTHING (measured: 0 chars on watermarked EOBs
    where PSM 6 reads the page) — a single uniform block (6) is the rescue,
    and sparse text (11) is the last resort for scattered form fields."""
    try:
        import pytesseract
        import fields as _fields
        best, best_score = "", -1.0
        for psm in (3, 6, 11):
            text = (pytesseract.image_to_string(img, config=f"--psm {psm}") or "").strip()
            # Scored by extraction yield, not length: confetti noise on
            # degraded scans produces LONG strings with zero fields, so a
            # first/longest-read-wins policy actively prefers garbage.
            s = _fields.extraction_score(text)
            if s > best_score:
                best, best_score = text, s
        return best
    except Exception:
        return ""


def _word_overlap(a: str, b: str) -> float:
    """Jaccard similarity over word sets — the two engines agreeing on most
    words is the cheapest possible corroboration signal."""
    wa = set(re.findall(r"[a-z0-9]+", a.lower()))
    wb = set(re.findall(r"[a-z0-9]+", b.lower()))
    if not wa or not wb:
        return 0.0
    return len(wa & wb) / len(wa | wb)


def stage_ocr(ctx: dict, cfg: dict) -> dict:
    """PaddleOCR full-text extraction, engineered for accuracy on real scans:

    1. Assess every page (blur/contrast/exposure).
    2. EVERY poor page is enhanced (deskew + CLAHE + denoise + upscale);
       poor PDF pages are first re-rendered at 300dpi from the original
       bytes, which hold more detail than the 200dpi rasterization.
    3. PaddleOCR runs batched, but results are kept PER PAGE with their
       recognizer confidence (rec_scores) — the confidence signal Paddle
       always returned and this pipeline previously discarded.
    4. Low-yield pages (almost no text despite real ink coverage) get one
       400dpi retry; the better read wins.
    5. Tesseract second opinion on empty or low-confidence pages: it
       rescues text Paddle missed outright, and the word-overlap agreement
       between the two engines is recorded as a corroboration signal.
    ctx['scan_quality_poor'] / ctx['ocr_low_confidence'] tell validate() to
    demand human verification; ctx['ocr_pages'] carries the per-page audit.
    """
    from paddleocr import PaddleOCR

    pages = ensure_pages(ctx, dpi=cfg.get("render_dpi", 200))
    qualities = [assess_page_quality(p) for p in pages[:10]]  # sample bound
    poor_pages = [i for i, q in enumerate(qualities) if q["poor"]]
    ctx["scan_quality"] = qualities
    # Periodic-watermark detection drives both the enhancement path (inside
    # preprocess_for_ocr) and the review flag: a watermarked page's text is
    # trustworthy only after lattice removal, and reviewers should know it
    # happened.
    import dewatermark
    import cv2
    wm_pages = [i for i, p in enumerate(pages[:10])
                if dewatermark.detect_periodic_background(
                    cv2.cvtColor(np_from_pil(p), cv2.COLOR_RGB2GRAY))]
    if wm_pages:
        ctx["watermark_detected"] = True
        ctx["watermark_pages"] = wm_pages
        # A watermarked page is effectively degraded for OCR even when its
        # blur/contrast metrics look fine — route it through enhancement.
        poor_pages = sorted(set(poor_pages) | set(wm_pages))
    is_pdf = sniff_doc_kind(ctx.get("raw_bytes", b""), ctx.get("content_type", "")) == "pdf"
    if poor_pages:
        if len(poor_pages) >= max(1, len(qualities) // 2):
            ctx["scan_quality_poor"] = True
        # Re-render poor PDF pages at higher DPI, then enhance EVERY poor
        # page individually — a document with 3 bad pages out of 40 still
        # has 3 bad pages, and eligibility reads values off single pages.
        if is_pdf:
            hires, _ = pdf_to_pages(ctx["raw_bytes"], dpi=cfg.get("enhance_dpi", 300))
            if hires:
                for i in poor_pages:
                    if i < len(hires):
                        pages[i] = hires[i]
                ctx["pages"] = pages
        for i in poor_pages:
            pages[i] = preprocess_for_ocr(pages[i])
        ctx["ocr_enhanced"] = True
        if wm_pages:
            ctx["watermark_removed"] = True

    # PaddleOCR construction loads the detector+recognizer from disk — a
    # process singleton, not a per-document cost.
    ocr = _model(
        ("paddleocr", cfg.get("lang", "en"),
         cfg.get("use_doc_orientation_classify", True),
         cfg.get("use_doc_unwarping", True),
         cfg.get("use_textline_orientation", True)),
        lambda: PaddleOCR(
            lang=cfg.get("lang", "en"),
            use_doc_orientation_classify=cfg.get("use_doc_orientation_classify", True),
            use_doc_unwarping=cfg.get("use_doc_unwarping", True),
            use_textline_orientation=cfg.get("use_textline_orientation", True),
            # Same oneDNN regression stage_layout already works around below
            # (PaddlePaddle/Paddle#79749): without this, predict() raises
            # NotImplementedError on CPU for EVERY image, so no scanned page,
            # photo or fax was ever read -- only PDFs with a text layer were.
            enable_mkldnn=False,
            # Detection resize MUST cap the LONG side ("max"). Paddle's
            # default for named det configs scales the SHORT side to the
            # limit, ballooning enhanced pages (e.g. 524x1600 -> 4885x1600),
            # which both OOMs the worker and destroys aspect fidelity.
            text_det_limit_side_len=int(cfg.get("det_limit_side_len", 1600)),
            text_det_limit_type="max",
        ),
    )
    # Batched inference: predict() accepts a LIST of images and pipelines
    # detection/recognition across them internally. Results come back as one
    # dict per input image, so per-page alignment survives batching. Falls
    # back to per-page if a Paddle version rejects list input — OCR must
    # never fail the document.
    arrays = [np_from_pil(p) for p in pages]
    batch_n = max(1, int(cfg.get("batch_pages", 4)))

    def _unpack(res: dict) -> tuple[str, float | None]:
        text = "\n".join(res.get("rec_texts", []))
        scores = [x for x in res.get("rec_scores", []) if isinstance(x, (int, float))]
        return text, (sum(scores) / len(scores) if scores else None)

    page_reads: list[tuple[str, float | None]] = [("", None)] * len(arrays)
    try:
        for i in range(0, len(arrays), batch_n):
            for j, res in enumerate(ocr.predict(arrays[i:i + batch_n])):
                page_reads[i + j] = _unpack(res)
    except Exception:
        page_reads = [("", None)] * len(arrays)
        for i, arr in enumerate(arrays):
            # A page Paddle cannot read at all stays empty here and is handed
            # to the Tesseract pass below; one engine failing must never
            # leave the whole document unread.
            try:
                for res in ocr.predict(arr):
                    page_reads[i] = _unpack(res)
                    break
            except Exception as exc:  # noqa: BLE001
                ctx.setdefault("ocr_engine_errors", []).append(f"page {i}: {type(exc).__name__}: {str(exc)[:160]}")

    # Low-yield retry: ink on the page but almost no text means the read —
    # not the page — failed. One 400dpi enhanced retry; keep the better read.
    low_yield_chars = int(cfg.get("low_yield_chars", 40))
    retry_dpi = int(cfg.get("retry_dpi", 400))
    retried = []
    if is_pdf:
        hires = None
        for i, (text, _) in enumerate(page_reads):
            if len(text) >= low_yield_chars or _page_ink_coverage(pages[i]) < 0.005:
                continue
            if hires is None:
                hires, _ = pdf_to_pages(ctx["raw_bytes"], dpi=retry_dpi)
            if not hires or i >= len(hires):
                continue
            cand = preprocess_for_ocr(hires[i])
            try:
                res = ocr.predict(np_from_pil(cand))
                if res:
                    t2, c2 = _unpack(res[0])
                    if len(t2) > len(text):
                        page_reads[i] = (t2, c2)
                        pages[i] = cand
                        retried.append(i)
            except Exception:
                pass
    if retried:
        ctx["ocr_retried_pages"] = retried

    # Tesseract consensus on empty or unconfident pages.
    consensus_min_conf = float(cfg.get("consensus_min_conf", 0.75))
    use_consensus = bool(cfg.get("consensus_fallback", True))
    page_audit: list[dict] = []
    low_conf_pages = []
    for i, (text, conf) in enumerate(page_reads):
        engine, agreement = "paddle", None
        if use_consensus and (not text.strip() or conf is None or conf < consensus_min_conf):
            tess = _tesseract_text(preprocess_for_ocr(pages[i]))
            if tess:
                if not text.strip():
                    # Paddle read nothing; tesseract read something — take it.
                    text, engine = tess, "tesseract"
                    page_reads[i] = (text, conf)
                else:
                    agreement = round(_word_overlap(text, tess), 3)
                    # Paddle's own confidence is very low and the second
                    # engine produced substantially MORE text: prefer the
                    # fuller read, flagged for review via the audit trail.
                    if (conf or 0) < 0.5 and len(tess) > 2 * len(text):
                        text, engine = tess, "tesseract"
                        page_reads[i] = (text, conf)
        if conf is not None and conf < consensus_min_conf:
            low_conf_pages.append(i)
        page_audit.append({"page": i, "chars": len(text), "confidence": conf,
                           "engine": engine, "agreement": agreement})

    scored = [(len(t), c) for t, c in page_reads if c is not None]
    if scored:
        total_chars = sum(n for n, _ in scored) or 1
        ctx["ocr_confidence"] = round(sum(n * c for n, c in scored) / total_chars, 4)
    if low_conf_pages or any(a["engine"] == "tesseract" for a in page_audit):
        ctx["ocr_low_confidence"] = True
        ctx["ocr_low_confidence_pages"] = low_conf_pages
    ctx["ocr_pages"] = page_audit

    ocr_text = "\n\n".join(t for t, _ in page_reads if t)
    # Keep whichever read is fuller: this pass now also runs on images Docling
    # already got some text from, and a weaker OCR read must not replace it.
    if len(ocr_text.strip()) < len(str(ctx.get("text") or "").strip()):
        return ctx
    ctx["text"] = ocr_text
    # OCR recovered text for a scanned doc: markdown has no structure, but
    # downstream stages read markdown first, so mirror it.
    ctx["markdown"] = ctx["text"]
    return ctx


def stage_layout(ctx: dict, cfg: dict) -> dict:
    """PP-StructureV3 layout analysis: regions, reading order, seals/stamps."""
    from paddleocr import PPStructureV3

    engine = _model(
        ("ppstructure", cfg.get("use_table_recognition", True),
         cfg.get("use_seal_recognition", True)),
        lambda: PPStructureV3(
            use_table_recognition=cfg.get("use_table_recognition", True),
            use_seal_recognition=cfg.get("use_seal_recognition", True),
        # Known PaddlePaddle 3.3.x regression: the oneDNN CPU backend's PIR
        # attribute converter has no case for ArrayAttribute<DoubleAttribute>,
        # which several of this pipeline's models hit (upstream issue
        # PaddlePaddle/Paddle#79749/#77340). Confirmed live. No fix upstream
        # yet -- enable_mkldnn=False is the documented workaround.
            enable_mkldnn=False,
        ),
    )
    regions: list[dict] = []
    for i, page in enumerate(ensure_pages(ctx)):
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


# ---------------------------------------------------------------------------
# VLM extraction
# ---------------------------------------------------------------------------

class ModelUnavailable(Exception):
    """The extraction model could not be reached or kept timing out. The
    document is left unread so the unread-document sweep reads it again
    later, rather than saved with only what could be read without the model."""


# One model request at a time per reader process (VLM_MAX_CONCURRENT). The
# local Ollama serves requests one by one; three workers each sending two
# sections queued requests for minutes, past the client timeout, and the
# documents were saved half-read. Waiting here does not count against it.
_VLM_SLOTS = threading.BoundedSemaphore(max(1, int(os.environ.get("VLM_MAX_CONCURRENT", "1"))))


def _transient(exc: BaseException) -> bool:
    """A timeout, a dropped connection or a server error from the model --
    worth reading the document again later, unlike a bad answer."""
    names = {type(e).__name__ for e in (exc, exc.__cause__, exc.__context__) if e is not None}
    return bool(names & {"APITimeoutError", "APIConnectionError", "InternalServerError", "ReadTimeout",
                         "ConnectTimeout", "ConnectError", "RemoteProtocolError", "TimeoutException"}) \
        or "timed out" in str(exc).lower() or "connection error" in str(exc).lower()


def _vlm_call(client: Any, model: str, content: list[dict], max_tokens: int) -> dict:
    with _VLM_SLOTS:
        return _vlm_call_unlocked(client, model, content, max_tokens)


def _vlm_call_unlocked(client: Any, model: str, content: list[dict], max_tokens: int) -> dict:
    """One VLM round-trip with transport retries and JSON self-repair.

    Retries up to 3 times on transport/5xx errors with exponential backoff.
    If the model returns unparseable JSON, sends one repair follow-up asking
    for the same answer as bare JSON before giving up. Raises only when all
    attempts fail."""
    last_exc: Exception | None = None
    messages = [{"role": "user", "content": content}]
    for attempt in range(3):
        try:
            resp = client.chat.completions.create(
                model=model, messages=messages,
                max_tokens=max_tokens, temperature=0,
            )
            raw = resp.choices[0].message.content or ""
            try:
                return _parse_json_object(raw)
            except ValueError:
                # Self-repair: one follow-up demanding strict JSON, then fail.
                messages = messages + [
                    {"role": "assistant", "content": raw},
                    {"role": "user", "content": (
                        "Your previous answer was not valid JSON. Reply with the "
                        "SAME answer as a single bare JSON object only — no prose, "
                        "no markdown fences, no explanation.")},
                ]
                resp2 = client.chat.completions.create(
                    model=model, messages=messages,
                    max_tokens=max_tokens, temperature=0,
                )
                return _parse_json_object(resp2.choices[0].message.content or "")
        except (httpx.HTTPError, ConnectionError, TimeoutError) as exc:
            last_exc = exc
            time.sleep(0.5 * (2 ** attempt))
    raise RuntimeError(f"VLM call failed after retries: {last_exc}")


def _parse_json_object(raw: str) -> dict:
    """Strict parse first; fall back to the largest {...} span; else ValueError."""
    raw = raw.strip()
    if raw.startswith("```"):
        raw = re.sub(r"^```(json)?\s*|\s*```$", "", raw, flags=re.S).strip()
    try:
        obj = json.loads(raw)
        if isinstance(obj, dict):
            return obj
    except json.JSONDecodeError:
        pass
    m = re.search(r"\{.*\}", raw, re.S)
    if m:
        try:
            obj = json.loads(m.group(0))
            if isinstance(obj, dict):
                return obj
        except json.JSONDecodeError:
            pass
    raise ValueError("no parseable JSON object in VLM response")


def _chunk_markdown(md: str, chunk_chars: int) -> list[str]:
    """Split long markdown on paragraph boundaries so no field-bearing line is
    cut mid-sentence; keeps every chunk under chunk_chars."""
    if len(md) <= chunk_chars:
        return [md]
    chunks, buf = [], ""
    for para in re.split(r"\n\s*\n", md):
        if buf and len(buf) + len(para) + 2 > chunk_chars:
            chunks.append(buf)
            buf = para
        else:
            buf = (buf + "\n\n" + para) if buf else para
    if buf:
        chunks.append(buf)
    return chunks


def _merge_extractions(parts: list[dict], fields: list[str]) -> dict:
    """Merge per-chunk extractions: first non-null value wins; lists are
    unioned (order-preserving, de-duplicated)."""
    merged: dict[str, Any] = {f: None for f in fields}
    for f in fields:
        for part in parts:
            v = part.get(f)
            if v in (None, "", [], {}):
                continue
            if isinstance(v, list):
                cur = merged[f] if isinstance(merged[f], list) else []
                merged[f] = cur + [x for x in v if x not in cur]
            elif merged[f] in (None, "", [], {}):
                merged[f] = v
    return merged


def _in_domain_verdict(parsed: dict, pop: bool = False) -> bool:
    """The model's related-to-billing verdict; True when it gave none.

    The prompt asks for "_in_domain" but the deployed model answers with
    "in_domain" (no underscore) -- reading only the prompted spelling meant a
    rejection was never seen at all. Accept both, and string booleans."""
    verdict = True
    for key in ("_in_domain", "in_domain"):
        if key in parsed:
            v = parsed.pop(key) if pop else parsed[key]
            if isinstance(v, str):
                v = v.strip().lower() not in ("false", "no", "0")
            if v is not None:
                verdict = bool(v)
    return verdict


def _field_question(field: str) -> str:
    """One atomic typed question for a schema field (System One style:
    decompose the extraction into small, independently-judged questions
    instead of one monolithic generate-everything prompt)."""
    if field.endswith("_usd") or "amount" in field:
        return (f"What is the {field.replace('_usd', '').replace('_', ' ')} in this document? "
                'Answer {"value": <number in USD, no symbols>, "probability": <0-1>} '
                "or {\"value\": null} if absent.")
    if "date" in field:
        return (f"What is the {field.replace('_', ' ')} in this document? "
                'Answer {"value": "<date as it appears>", "probability": <0-1>} '
                "or {\"value\": null} if absent.")
    if field.endswith("_codes") or field == "service_lines":
        return (f"List the {field.replace('_', ' ')} in this document. "
                'Answer {"value": [<strings>], "probability": <0-1>} '
                "or {\"value\": null} if absent.")
    return (f"What is the {field.replace('_', ' ')} in this document? "
            'Answer {"value": "<exact value as it appears>", "probability": <0-1>} '
            "or {\"value\": null} if absent.")


PROMPT_BUDGET_CHARS = 2000


def _atomic_questions(fields: list[str]) -> str:
    return "\n".join(f'- "{f}": {_field_question(f)}' for f in fields)


def _merge_atomic(parts: list[dict], fields: list[str]) -> tuple[dict, dict]:
    """Merge per-chunk atomic answers: each part is {field: {"value": v,
    "probability": p}}. Highest-probability non-null value wins per field —
    a confident read on chunk 3 beats a hesitant null on chunk 1, and the
    probability map rides along for verify()."""
    merged: dict[str, Any] = {f: None for f in fields}
    probs: dict[str, float] = {}
    for f in fields:
        best_p = -1.0
        for part in parts:
            cell = part.get(f)
            if not isinstance(cell, dict):
                continue
            v, p = cell.get("value"), cell.get("probability")
            try:
                p = float(p)
            except (TypeError, ValueError):
                p = 0.5 if v not in (None, "", [], {}) else 0.0
            if v in (None, "", [], {}):
                continue
            if p > best_p:
                best_p, merged[f], probs[f] = p, v, p
    return merged, probs


def stage_vlm_extract(ctx: dict, cfg: dict, schemas: dict) -> dict:
    """VLM semantic extraction via an OpenAI-compatible endpoint
    (PaddleOCR-VL or Qwen2.5-VL served by vLLM; swap with one env var).

    - Schema follows the classified doc_type (a determination letter is asked
      for prevailing_party, not CPT codes); unknown/unrelated types fall back
      to idr_claim so mis-uploads still get probed for any billing content.
    - Documents longer than chunk_chars are extracted per chunk and merged —
      a 40-page itemized bill no longer loses everything past character 10k.
    - Up to max_pages page images accompany the text for layout cues.
    """
    # Client is a process singleton per endpoint: TCP keep-alive to the
    # local Ollama survives across documents instead of re-handshaking per
    # doc, and the OpenAI client is thread-safe for the parallel chunks.
    def build_client():
        from openai import OpenAI
        return OpenAI(base_url=cfg["endpoint"],
                      api_key=os.environ.get("VLM_API_KEY", "none"),
                      timeout=httpx.Timeout(180.0, connect=10.0))

    client = _model(("vlm_client", cfg["endpoint"]), build_client)
    doc_type = ctx.get("doc_type") or "idr_claim"
    schema_name = doc_type if doc_type in schemas else cfg.get("schema", "idr_claim")
    ctx["schema_used"] = schema_name
    fields = schemas[schema_name]["fields"]

    md = ctx.get("markdown", ctx.get("text", ""))
    # Docling's own markdown export already renders detected tables as GFM
    # markdown tables inline in `md` -- this JSON blob is largely the SAME
    # table data a second time, structured differently. Sized small since
    # MAX_PROMPT_CHARS below hard-caps the whole prompt at 2000 chars
    # anyway -- this just avoids that cap being spent entirely on a
    # redundant copy of content `md` already has.
    tables_hint = json.dumps(ctx.get("tables", [])[:1])[:300]
    # Page images only when the configured model actually consumes them —
    # with the text-only local Ollama default, rasterizing pages for the
    # VLM is pure waste (ensure_pages renders lazily on first real need).
    pages = ensure_pages(ctx) if cfg.get("vision") else ctx.get("pages", [])
    max_pages = cfg.get("max_pages", 3)

    # Confirmed live against this exact endpoint/model: a page thumbnailed to
    # vlm_image_max_px (650px, sized for token cost -- see config comment
    # above) shrinks a normal page of body text to a few px tall, illegible
    # to the vision model. Given a prompt with BOTH clear transcribed text
    # AND that illegible thumbnail, the model let the blurry image override
    # its own correct reading of the text -- _in_domain:false and every
    # field nulled, on a document whose text-only extraction (same prompt,
    # no image) was perfect. The image-caveat line below is what fixes it;
    # only added when vision is actually on (no image attached => nothing
    # to caveat, and prompt budget is the one resource genuinely scarce
    # here per MAX_PROMPT_CHARS below).
    image_caveat = (
        "A small page thumbnail is also attached for layout/seal/stamp cues "
        "only -- it may be too low-resolution to read in full; never let a "
        "blurry or hard-to-read image override what the transcribed text "
        "below clearly states. "
    ) if cfg.get("vision") else ""
    atomic = bool(cfg.get("atomic", True))
    # The whole prompt is capped at 2000 chars (MAX_PROMPT_CHARS in
    # run_chunk). One typed question per field costs ~150 chars, so for a
    # schema of a dozen fields the questions alone fill the cap and the model
    # is sent no document at all (seen live: an EOB read back with
    # "Provider NPI" as the provider). Ask per field only when at least half
    # the budget is left for the document.
    if atomic and len(_atomic_questions(fields)) + 900 > PROMPT_BUDGET_CHARS // 2:
        atomic = False
    if atomic:
        # System One decomposition: each field is an independently-judged
        # typed question carrying its own calibrated probability, instead of
        # one monolithic blob where a weak field silently rides on strong
        # ones. Same single call per chunk (the local Ollama serializes
        # anyway), but the ANSWER STRUCTURE forces per-field judgment and
        # yields the probability map verify() routes on.
        questions = _atomic_questions(fields)
        instructions = (
            "You are an IDR (No Surprises Act dispute) document analyst. "
            "The transcribed text below is the authoritative content of this document. "
            f"{image_caveat}"
            "FIRST decide whether this document is related to medical billing, health "
            "insurance claims, explanation-of-benefits, IDR arbitration, or the patient's clinical "
            "record of the care in dispute (ER notes, discharge summaries, operative reports, "
            "progress notes -- these ARE related) at all, and "
            'set "_in_domain" true or false based on the TEXT. A resume, menu, tax form, legal contract, '
            "photograph with no document content, or any other unrelated material is "
            "false. When false, set every other field null and stop. "
            f"This document is believed to be of type '{doc_type}'.\n"
            "Answer EACH of these extraction questions independently. Reply as one "
            'JSON object: {"_in_domain": true/false, "<field>": {"value": ..., '
            '"probability": 0-1}, ...}. probability is your calibrated confidence '
            "that the value is exactly right — honest, not optimistic. null value "
            "when the field is absent. Only extract values that literally appear in "
            "the document — never invent or infer identifiers or amounts. Preserve "
            "exact formatting of codes and identifiers. Amounts as numbers in USD, "
            "dates as they appear.\n"
            f"QUESTIONS:\n{questions}\n"
        )
    else:
        instructions = (
            "You are an IDR (No Surprises Act dispute) document analyst. "
            "The transcribed text below is the authoritative content of this document. "
            f"{image_caveat}"
            "FIRST decide whether this document is related to medical billing, health "
            "insurance claims, explanation-of-benefits, IDR arbitration, or the patient's clinical "
            "record of the care in dispute (ER notes, discharge summaries, operative reports, "
            "progress notes -- these ARE related) at all, and "
            'set "_in_domain" true or false based on the TEXT. A resume, menu, tax form, legal contract, '
            "photograph with no document content, or any other unrelated material is "
            "false. When false, set every other field null and stop. "
            f"This document is believed to be of type '{doc_type}'. "
            f"Extract these fields as strict JSON (null when absent): {', '.join(fields)}.\n"
            "Only extract values that literally appear in the document — never invent "
            "or infer identifiers or amounts. Preserve exact formatting of codes and "
            "identifiers (CPT/HCPCS/NDC, claim numbers, NPIs). Amounts as numbers in "
            "USD, dates as they appear.\n"
        )

    chunk_chars = cfg.get("chunk_chars", 12000)
    chunks = _chunk_markdown(md, chunk_chars)

    def run_chunk(i: int, chunk: str) -> dict:
        prompt = instructions
        if len(chunks) > 1:
            prompt += f"This is section {i + 1} of {len(chunks)} of one document.\n"
        prompt += "Document content (markdown with layout) follows, then extracted tables.\n\n" + chunk
        if i == 0:
            prompt += "\n\nTABLES:\n" + tables_hint
        # Hard backstop. Every chars-per-token ratio assumed here so far has
        # been wrong, twice: billing content (CPT/HCPCS codes, NPIs, dollar
        # amounts, dates, MICR strings) is mostly digits and symbols, and
        # that tokenizes far denser than English prose, by an amount that
        # keeps moving the goalposts (9000 chars -> 4581 tokens, then 6000
        # chars -> 4532 tokens -- the real ratio got WORSE, not better, when
        # the cap shrank, meaning density isn't even constant across
        # documents). Stop estimating a ratio and just cap low enough that
        # no plausible density can overflow: 2000 chars would need under
        # 0.49 chars/token to still exceed 4096 -- not a real tokenizer
        # outcome for any text, JSON, or digit-heavy content.
        MAX_PROMPT_CHARS = PROMPT_BUDGET_CHARS
        if len(prompt) > MAX_PROMPT_CHARS:
            prompt = prompt[:MAX_PROMPT_CHARS]
        content: list[dict] = [{"type": "text", "text": prompt}]
        # Multimodal: page images give layout cues text alone loses -- but
        # only when the configured model actually supports it. The deployed
        # model/vision flag found live (VLM_MODEL=qwen2.5vl:7b, VLM_VISION=
        # true) don't match this file's own long-standing comments assuming
        # a text-only model with vision off -- someone upgraded the live
        # config without updating here. With vision genuinely on, every
        # call attached a FULL page bitmap straight from pdf_to_pages'
        # 200-300dpi render (1700x2200+ px for a letter page) -- the real
        # source of every "exceeds context size" failure this prompt-char
        # cap could never fix, since image tokens aren't text and this cap
        # never touched them. Confirmed live against this exact endpoint/
        # model: a 2000-char text prompt alone costs ~1030 prompt tokens,
        # but the SAME request with a page image attached costs ~2095 --
        # ~1065 tokens of pure image cost -- and that number held constant
        # (not proportional) across every size tried from 350px up to
        # 612px on the long edge, i.e. a resize within that range is free:
        # it doesn't trade away any of the layout-cue fidelity the image
        # was for. Thumbnail to a small fixed box before encoding instead
        # of sending the raw full-DPI render.
        if cfg.get("vision"):
            max_px = cfg.get("vlm_image_max_px", 650)
            for page in pages[i * max_pages:(i + 1) * max_pages] or pages[:max_pages]:
                # The thumbnail is a layout cue, not the source of the values
                # (those come from the transcribed text). If the page image
                # cannot be decoded, extract from the text alone -- a bad
                # thumbnail used to abandon the whole extraction, which left
                # every scanned image with its text read and nothing taken
                # from it.
                try:
                    thumb = page.copy()
                    thumb.thumbnail((max_px, max_px))
                    content.append({"type": "image_url",
                                    # upstream context fix (thumbnail) + JPEG encoding
                                    "image_url": {"url": f"data:image/jpeg;base64,{pil_to_b64(thumb)}"}})
                except Exception as exc:  # noqa: BLE001
                    ctx.setdefault("findings", []).append(
                        {"field": None, "issue": f"Page image could not be attached ({type(exc).__name__}); values were read from the transcribed text only"})
                break  # one page image per chunk keeps token cost bounded
        parsed = _vlm_call(client, cfg["model"], content, cfg.get("max_tokens", 2048))
        # A "not in domain" verdict reached WITH a page image attached is not
        # trusted on its own: confirmed live, the low-resolution thumbnail made
        # the model reject a plainly worded claim form that the same prompt
        # without the image extracted perfectly. Re-ask text-only and take
        # that answer -- one extra call, only on rejections.
        if len(content) > 1 and not _in_domain_verdict(parsed):
            parsed = _vlm_call(client, cfg["model"], content[:1], cfg.get("max_tokens", 2048))
        return parsed

    # Chunk concurrency: the endpoint is the LOCAL in-cluster Ollama, NOT
    # OpenAI -- a shared platform instance with a known OOM history (see
    # pipeline.yaml). Ollama serializes past its own OLLAMA_NUM_PARALLEL
    # (default 1), so more workers than that just queue server-side while
    # multiplying KV-cache pressure. Default 2 overlaps request/JSON overhead
    # with inference; raise VLM_PARALLEL_CHUNKS only in lockstep with
    # OLLAMA_NUM_PARALLEL on the Ollama deployment.
    parallel = int(cfg.get("parallel_chunks",
                           os.environ.get("VLM_PARALLEL_CHUNKS", "2")))
    if len(chunks) > 1 and parallel > 1:
        with ThreadPoolExecutor(max_workers=min(parallel, len(chunks))) as pool:
            raw_parts = list(pool.map(lambda ic: run_chunk(*ic), enumerate(chunks)))
    else:
        raw_parts = [run_chunk(i, c) for i, c in enumerate(chunks)]

    parts: list[dict] = []
    in_domain_votes: list[bool] = []
    for parsed in raw_parts:
        in_domain_votes.append(_in_domain_verdict(parsed, pop=True))
        # The model does not always answer in the flat shape it was asked
        # for (a wrapper object, other names for the same field, a provider
        # as an object). Read its answer onto the schema whatever the shape.
        # A per-field answer ({"value": .., "probability": ..}) is unwrapped
        # first, so the shape reader sees the values and the probabilities
        # survive for _merge_atomic.
        typed = {f: parsed[f] for f in fields
                 if isinstance(parsed, dict) and isinstance(parsed.get(f), dict) and "value" in parsed[f]}
        if typed:
            values = {k: (v["value"] if k in typed else v) for k, v in parsed.items()}
            shaped = shape_extraction(values, fields)
            parts.append({f: ({"value": shaped.get(f), "probability": typed[f].get("probability")}
                              if f in typed else shaped.get(f)) for f in fields})
        else:
            parts.append(shape_extraction(parsed, fields))

    ctx["in_domain"] = any(in_domain_votes) if in_domain_votes else True
    # Strict schema: only declared fields survive, all present (null-filled) —
    # the VLM can't smuggle invented keys into the record.
    # Whichever prompt was sent, a per-field answer keeps its probabilities.
    if any(isinstance(p.get(f), dict) and "value" in p[f] for p in parts for f in fields):
        parts = [{f: (p.get(f) if isinstance(p.get(f), dict) and "value" in p[f] else {"value": p.get(f)})
                  for f in fields} for p in parts]
        merged, probs = _merge_atomic(parts, fields)
        ctx["extracted"] = merged
        ctx["vlm_field_probability"] = probs
    else:
        # Model didn't follow the typed structure (or atomic disabled):
        # degrade gracefully to plain value merging rather than losing the
        # document's extraction entirely.
        cleaned = []
        for p in parts:
            cleaned.append({
                f: (p[f].get("value") if isinstance(p.get(f), dict) else p.get(f))
                for f in fields
            })
        ctx["extracted"] = _merge_extractions(cleaned, fields)
    return ctx


# ---------------------------------------------------------------------------
# Normalization + verification
# ---------------------------------------------------------------------------

_RE_AMOUNT = re.compile(r"^\$?\s*(-?[\d,]+(?:\.\d{1,2})?)$")
_RE_DATE = re.compile(
    r"^(\d{4})-(\d{2})-(\d{2})$|"                     # ISO
    r"^(\d{1,2})[/-](\d{1,2})[/-](\d{2,4})$|"          # US numeric
    r"^([A-Za-z]+)\s+(\d{1,2}),?\s+(\d{4})$"           # January 5, 2026
)
_MONTHS = {m.lower(): i + 1 for i, m in enumerate(
    ["January", "February", "March", "April", "May", "June", "July",
     "August", "September", "October", "November", "December"])}
_RE_DATE_ANYWHERE = re.compile(
    r"\d{4}-\d{2}-\d{2}|\d{1,2}[/-]\d{1,2}[/-]\d{2,4}|[A-Za-z]{3,9}\.?\s+\d{1,2},?\s+\d{4}")
_MONTH_ABBR = {name[:3]: name for name in _MONTHS}   # "mar" -> "march"
# Claim adjustment reason / remark codes as EOBs print them: CO-45, PR-1,
# OA-23, N130, MA01, or the bare number.
_RE_DENIAL_CODE = re.compile(r"^(?:(?:CO|PR|OA|PI|CR)-?\d{1,3}|(?:MA|M|N)\d{1,4}|\d{1,3})$")
_RE_CODE = re.compile(r"^[A-Z]?\d{4}[A-Z0-9]?$")       # CPT/HCPCS/DRG-ish
_RE_NPI = re.compile(r"^\d{10}$")


def _norm_amount(v: Any) -> float | None:
    if isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        return round(float(v), 2)
    if isinstance(v, str):
        m = _RE_AMOUNT.match(v.strip())
        if m:
            return round(float(m.group(1).replace(",", "")), 2)
    # "1,850.00 USD", "(82.40)", {"amount": 1850}, [1850] -- one amount in
    # another wrapping. Still None for anything holding several amounts.
    return as_money(v)


def _norm_date(v: Any) -> str | None:
    """Canonicalize a date-ish value to ISO YYYY-MM-DD; None when unparseable.
    A value that wraps one date -- a range ("03/12/2026 - 03/14/2026": its
    first day), a timestamp, "Mar. 12, 2026", a one-item list or an object --
    is read for that date."""
    if isinstance(v, (list, tuple, dict)):
        v = as_text(v)
    if not isinstance(v, str):
        return None
    s = v.strip()
    m = _RE_DATE.match(s)
    if not m:
        inner = _RE_DATE_ANYWHERE.search(s)
        if not inner:
            return None
        s = re.sub(r"\.", "", inner.group(0)).strip()
        mon = re.match(r"([A-Za-z]+)(.*)", s)
        if mon and mon.group(1).lower()[:3] in _MONTH_ABBR:
            s = _MONTH_ABBR[mon.group(1).lower()[:3]] + mon.group(2)
        m = _RE_DATE.match(s)
        if not m:
            return None
    if m.group(1):
        mm, dd = int(m.group(2)), int(m.group(3))
        if 1 <= mm <= 12 and 1 <= dd <= 31:
            return f"{m.group(1)}-{m.group(2)}-{m.group(3)}"
        return None
    if m.group(4):
        mm, dd, yy = int(m.group(4)), int(m.group(5)), m.group(6)
        yyyy = int(yy) if len(yy) == 4 else (2000 + int(yy) if int(yy) < 50 else 1900 + int(yy))
        if 1 <= mm <= 12 and 1 <= dd <= 31:
            return f"{yyyy:04d}-{mm:02d}-{dd:02d}"
        return None
    mon = _MONTHS.get(m.group(7).lower())
    if mon:
        return f"{int(m.group(9)):04d}-{mon:02d}-{int(m.group(8)):02d}"
    return None


def stage_fields(ctx: dict, cfg: dict) -> dict:
    """Deterministic typed-field extraction over the document text (amounts,
    dates, NPI/TIN/claim identifiers, CPT/HCPCS codes). Pure regex + checksum
    — no LLM, no vendor SDK — so these fields are trustworthy enough to feed
    eligibility evidence directly. Cross-checks (Luhn failures, conflicting
    totals) become validate() findings via ctx['field_check_issues']."""
    import fields as _fields
    import learning as _learning
    text = ctx.get("text", "") or ""
    ex = _fields.extract_fields(text, extra_labels=_learning.get_active_labels())
    issues = _fields.cross_check(ex)
    letter, letter_issues = _letter_docs.extract(ctx.get("doc_type") or "", text)
    if letter:
        ex["letter"] = letter
        issues = issues + letter_issues
    remit_key, remit, remit_issues = _remit_docs.extract(ctx.get("doc_type") or "", text, ctx.get("raw_bytes"))
    if remit_key:
        ex[remit_key] = remit
        issues = issues + remit_issues
        # The eligibility review's filing window and program-start checks read normalized service/determination
        # dates (case-api deriveEligibilityInput); a remittance states both, so the model is not needed for them.
        norm = ctx.setdefault("normalized", {})
        claim = (remit.get("claims") or [{}])[0]
        if not norm.get("service_date") and claim.get("date_of_service"):
            norm["service_date"] = claim["date_of_service"]
        if not norm.get("determination_date") and claim.get("date_of_denial"):
            norm["determination_date"] = claim["date_of_denial"]
    packet, packet_issues = _packet_docs.extract(ctx.get("doc_type") or "", text, ctx.get("raw_bytes"))
    if packet:
        ex["packet"] = packet
        issues = issues + packet_issues
    # A document a layout reader understood states its own claim number; the generic regex's guess (the first
    # "claim/account/bill ..." word on the page) is replaced by it, or dropped when the reader found none. Its
    # code lists are dropped too: on an 835 printout, a payer EOB, a UB-04 or the packet the regex reads zip
    # codes, amounts and ICD-10 codes as CPT/HCPCS, and those would be lifted onto the case.
    read_by_layout = [v for v in (ex.get("remit"), ex.get("ub04"), ex.get("packet")) if v]
    if read_by_layout:
        ids = ex.setdefault("identifiers", {})
        number = next((c.get("claim_number") for v in read_by_layout for c in (v.get("claims") or []) if c.get("claim_number")), None)
        if number:
            ids["claim_number"] = number
        else:
            ids.pop("claim_number", None)
        for key in ("cpt_codes", "hcpcs_codes", "diagnosis_codes"):
            if ex.get(key):
                ex.setdefault("codes_not_lifted", {})[key] = ex[key]
                ex[key] = []
    ctx["extracted_fields"] = ex
    ctx["field_check_issues"] = issues
    return ctx


def stage_normalize(ctx: dict, cfg: dict) -> dict:
    """Deterministic canonicalization of extracted values: amounts to floats,
    dates to ISO, identifiers de-formatted and shape-checked. Normalized
    copies live under ctx['normalized']; originals are never overwritten.
    Values that fail their shape check are recorded as normalize_warnings so
    verify()/validate() can weigh them."""
    # Before anything is canonicalized: a field the model left empty, but
    # which the page prints next to its own label, is read from the page.
    # This also covers a document whose model extraction failed outright.
    try:
        ex, filled, summed = fill_from_text(ctx.get("extracted") or {},
                                            ctx.get("markdown") or ctx.get("text") or "", ctx.get("doc_type"))
    except Exception:  # noqa: BLE001 -- a fallback must never cost the extraction it backs up
        ex, filled, summed = ctx.get("extracted") or {}, [], []
    if not isinstance(ex, dict):
        ex = {}
    # A claim total is checked against the service lines it sums: a line's
    # own figure taken for the total is corrected (only to a sum the page
    # prints), any other disagreement is reported.
    ex, total_notes = reconcile_totals(ex, ctx.get("markdown") or ctx.get("text") or "")
    if total_notes:
        ctx["total_notes"] = total_notes
    ctx["extracted"] = ex
    if filled:
        ctx["text_filled_fields"] = filled
    if summed:
        ctx["summed_fields"] = summed
    norm: dict[str, Any] = {}
    warnings: list[dict] = []
    for field, value in ex.items():
        if _is_blank(value):
            continue
        if field.endswith("_usd") or "amount" in field or field.startswith(("qpa", "billed", "allowed", "awarded", "patient_responsibility")):
            n = _norm_amount(value)
            if n is None:
                warnings.append({"field": field, "issue": f"amount not parseable: {value!r}"})
            else:
                norm[field] = n
        elif "date" in field:
            n = _norm_date(value)
            if n is None:
                warnings.append({"field": field, "issue": f"date not parseable: {value!r}"})
            else:
                norm[field] = n
        elif field == "provider_npi":
            digits = re.sub(r"\D", "", str(value))
            if _RE_NPI.match(digits):
                norm[field] = digits
            else:
                warnings.append({"field": field, "issue": "NPI is not 10 digits"})
        elif field == "denial_codes":
            cleaned = [c.upper() for c in split_codes(value)]
            bad = [c for c in cleaned if not _RE_DENIAL_CODE.match(c) and not _RE_CODE.match(c)]
            norm[field] = cleaned
            if bad:
                warnings.append({"field": field, "issue": f"implausible code(s): {', '.join(bad[:5])}"})
        elif field == "cpt_hcpcs_codes" and isinstance(value, (list, str)):
            cleaned = [as_text(c).upper() for c in value] if isinstance(value, list) \
                else [c for c in re.split(r"[\s,;/]+", value.strip().upper()) if c]
            bad = [c for c in cleaned if not _RE_CODE.match(c)]
            norm[field] = cleaned
            if bad:
                warnings.append({"field": field, "issue": f"implausible code(s): {', '.join(bad[:5])}"})
    ctx["normalized"] = norm
    ctx["normalize_warnings"] = warnings
    return ctx


def _is_blank(v: Any) -> bool:
    """None, "", [], {} -- written out because `v in (None, "", [], {})`
    compares with ==, and 0 == False == "" surprises live there."""
    return v is None or (isinstance(v, (str, list, dict, tuple)) and len(v) == 0)


def _flatten_values(v: Any) -> list[str]:
    """Scalar string forms of an extracted value. Lists are flattened, and a
    row ({"service": ..., "billed_amount_usd": ...}) contributes its VALUES:
    its printed form, braces and column names and all, is on no document."""
    if isinstance(v, (list, tuple)):
        out: list[str] = []
        for x in v:
            out.extend(_flatten_values(x))
        return out
    if isinstance(v, dict):
        out = []
        for x in v.values():
            if not _is_blank(x):
                out.extend(_flatten_values(x))
        return out
    if isinstance(v, bool):
        return []
    if isinstance(v, (int, float)):
        return [str(v), f"{float(v):,.2f}", f"${float(v):,.2f}"]
    return [str(v)]


def _grounded(value_str: str, hay: str, hay_squashed: str) -> bool:
    """A value is grounded when it (or a whitespace/punctuation-tolerant form)
    literally appears in the document text. hay_squashed is precomputed once
    per document by the caller — squashing a 100k-char haystack per extracted
    value was O(fields x doc size) of pure waste."""
    needle = value_str.strip().lower()
    if not needle:
        return True
    if needle in hay:
        return True
    # Tolerate OCR/typography variance: collapse non-alphanumerics on both sides.
    squash_n = re.sub(r"[^a-z0-9]", "", needle)
    if len(squash_n) < 3:
        return needle in hay
    return squash_n in hay_squashed


def stage_verify(ctx: dict, cfg: dict) -> dict:
    """Grounding check: every extracted scalar must literally appear in the
    document text. Per-field confidence is recorded (high = grounded,
    low = not found in text). Ungrounded values are the hallucination
    signature — they become findings in validate() and are excluded from the
    trusted normalized set."""
    hay = (ctx.get("markdown") or ctx.get("text") or "").lower()
    hay_squashed = re.sub(r"[^a-z0-9]", "", hay)
    probs = ctx.get("vlm_field_probability", {}) or {}
    prob_floor = float(cfg.get("probability_floor", 0.6))
    confidence: dict[str, str] = {}
    ungrounded: list[str] = []
    for field, value in ctx.get("extracted", {}).items():
        if value in (None, "", [], {}):
            continue
        atoms = [a for a in _flatten_values(value) if a.strip()]
        ok = all(_grounded(a, hay, hay_squashed) for a in atoms) if atoms else False
        # Three tiers: ungrounded is still the hallucination signature (low);
        # grounded but the model itself was unsure of the value (calibrated
        # probability under the floor) lands in "medium" — review-visible
        # without crying hallucination.
        p = probs.get(field)
        if not ok:
            confidence[field] = "low"
            ungrounded.append(field)
        elif isinstance(p, (int, float)) and p < prob_floor:
            confidence[field] = "medium"
        else:
            confidence[field] = "high"
    ctx["field_confidence"] = confidence
    ctx["ungrounded_fields"] = ungrounded
    # Trusted normalized view: drop normalized copies of ungrounded fields.
    if ungrounded and ctx.get("normalized"):
        ctx["normalized"] = {k: v for k, v in ctx["normalized"].items() if k not in ungrounded}
    return ctx


# ---------------------------------------------------------------------------
# Validation + indexing
# ---------------------------------------------------------------------------

def stage_validate(ctx: dict, cfg: dict, case: dict | None) -> dict:
    """Cross-check extracted fields against the case record; mismatches flag."""
    # Accumulate onto whatever's already here -- a stage that failed earlier
    # and was skipped (run_pipeline's per-stage try/except) records itself as
    # a finding too; overwriting ctx["findings"] fresh here would silently
    # erase that, undoing the whole point of recording it.
    findings: list[dict] = list(ctx.get("findings", []))
    ex = ctx.get("extracted", {})
    norm = ctx.get("normalized", {})
    if case:
        qpa = norm.get("qpa_usd") or (_norm_amount(ex.get("qpa_usd")) if ex.get("qpa_usd") else None)
        if qpa and case.get("qpa_cents"):
            if abs(qpa * 100 - case["qpa_cents"]) > 100:
                findings.append({"field": "qpa_usd", "issue": "QPA mismatch vs case record"})
        if ex.get("claim_number") and case.get("case_number"):
            # claim vs CMS case number are different identifiers; record both
            ctx["extracted"]["cms_case_number"] = case["case_number"]
        # One of the office's letters names its case: uploaded to another case, someone should look.
        lcase = ((ctx.get("extracted_fields") or {}).get("letter") or {}).get("case_number")
        if lcase and case.get("case_number"):
            a, b = _letter_docs.case_seq(lcase), _letter_docs.case_seq(case["case_number"])
            if a and b and a != b:
                findings.append({"field": "case_number", "issue": f"This letter is for case {lcase}, but it was "
                                 f"uploaded to case {case['case_number']} — check it belongs here"})
    # Out-of-domain guard: VLM verdict, zero-signal classification, or an
    # all-null extraction each independently mean a human should look before
    # this document is treated as case evidence.
    ex_fields = {k: v for k, v in ex.items() if v not in (None, "", [], {})}
    unreadable = bool(ctx.get("scan_quality_poor") or ctx.get("ocr_low_confidence"))
    # A layout reader that recognised the page (an 835 printout, a payer EOB, a UB-04, the filing packet) is
    # firmer evidence than the model's relevance vote, which sometimes calls a plain EOB "not medical billing":
    # the document keeps its type instead of becoming "unrelated, possible mis-upload".
    layout_read = any((ctx.get("extracted_fields") or {}).get(k) for k in ("remit", "ub04", "packet"))
    # A clinical record named by its own headings is evidence too, though it holds no billing field to extract.
    clinical = ctx.get("doc_type") == "medical_records" and ctx.get("classify_score", 0) >= WEIGHT["strong"]
    if layout_read or clinical:
        pass
    elif unreadable and (ctx.get("in_domain") is False or (ctx.get("classify_score", 1) == 0 and not ex_fields)):
        # The model saw too little legible text to recognise the page. That is
        # a page nobody could read, not a page about something else -- and
        # "unrelated, possible mis-upload" sends a person looking for the
        # wrong problem.
        findings.append({"field": None, "issue": "This page could not be read well enough to tell what it is — it is too faint, blurred or skewed. Nothing was taken from it: read it by hand, or ask the sender for a clearer copy"})
        ctx["doc_type"] = "unreadable"
    elif ctx.get("in_domain") is False:
        findings.append({"field": None, "issue": "Document is not related to medical billing or IDR — possible mis-upload; routed for manual review"})
        ctx["doc_type"] = "unrelated"
    elif ctx.get("classify_score", 1) == 0 and not ex_fields and not ctx.get("scan_quality_poor"):
        findings.append({"field": None, "issue": "No IDR-relevant content detected — possible mis-upload; routed for manual review"})
    elif not ex_fields and ex and not ctx.get("scan_quality_poor"):
        # The comment above promises this as an INDEPENDENT trigger, but it
        # was only ever reachable as a sub-condition of the classify_score==0
        # branch above -- a document whose type classified fine (score > 0)
        # but whose field extraction still came back null for every field
        # never hit either branch, so field_confidence ended up {} (nothing
        # to grade) and no finding ever fired. Confirmed live: an all-null
        # extraction silently looked identical to a clean, fully-verified
        # document with zero fields to flag.
        findings.append({"field": None, "issue": "Extraction returned no usable field values for this document type — verify manually before relying on it as case evidence"})
    # Degraded scan: extraction ran on enhanced images but confidence in the
    # underlying text is inherently limited — say so plainly. A barely legible
    # VALID document must not be misdiagnosed as a mis-upload.
    if ctx.get("scan_quality_poor"):
        findings.append({"field": None, "issue": "Poor scan quality (blur/contrast/exposure) — extraction uncertain despite image enhancement; manual verification of all values required"})
    # Low OCR confidence (recognizer score under threshold, or a fallback
    # engine had to rescue a page): the text itself is suspect, so every
    # value derived from it is too.
    if ctx.get("ocr_low_confidence"):
        conf = ctx.get("ocr_confidence")
        pct = f" (mean recognizer confidence {conf:.0%})" if isinstance(conf, float) else ""
        findings.append({"field": None, "issue": f"Low OCR confidence{pct} — one or more pages needed fallback OCR or scored below threshold; manual verification of all values required"})
    # Periodic watermark removed before OCR: extraction ran on the recovered
    # text layer — informational for the audit trail, values still verified
    # by grounding like any other.
    for page, deg in ctx.get("pages_turned", []):
        how = "upside down" if deg == 180 else "sideways"
        findings.append({"field": None, "issue": f"Page {page} was {how} and was turned the right way up before reading"})
    for page, deg in ctx.get("pages_straightened", []):
        findings.append({"field": None, "issue": f"Page {page} was fed in crooked ({abs(deg):.1f}°) and was straightened before reading"})
    if ctx.get("watermark_removed"):
        findings.append({"field": None, "issue": "Periodic watermark/stamp background detected and removed before OCR — extraction used the recovered text layer"})
    # Ambiguous classification: the winning type barely beat the runner-up.
    if ctx.get("classify_ambiguous") and ctx.get("doc_type") not in ("unrelated", "unreadable"):
        findings.append({"field": None, "issue": f"Document type '{ctx['doc_type']}' is a low-margin classification — confirm type on review"})
    # Hallucination guard from verify().
    money = lambda v: f"${v:,.2f}"  # noqa: E731
    labels = {"billed_amount_usd": "billed total", "allowed_amount_usd": "allowed total",
              "patient_responsibility_usd": "patient responsibility total"}
    for n in ctx.get("total_notes", []):
        what = labels.get(n["field"], n["field"])
        if n["field"] == "service_lines":
            findings.append({"field": "service_lines", "issue": f"Service line {n['line']} ({n['what'][:60]}) does not add up: allowed {money(n['allowed'])} is not plan paid {money(n['paid'])} plus patient {money(n['patient'])} — one of the three was misread; verify against the document"})
            continue
        if n.get("now") is not None:
            findings.append({"field": n["field"], "issue": f"The {what} was first read as {money(n['was'])}, which is one service line's figure. The lines add up to {money(n['lines_sum'])}, which the page also prints, so that is used — verify"})
        else:
            findings.append({"field": n["field"], "issue": f"The {what} reads {money(n['was'])} but the service lines add up to {money(n['lines_sum'])} — one of them was misread; verify against the document"})
    summed = ctx.get("summed_fields", [])
    for field in ctx.get("ungrounded_fields", []):
        if field in summed:
            continue  # said below, in its own words
        findings.append({"field": field, "issue": "Extracted value not found in document text — possible extraction error; verify before use"})
    for field in summed:
        findings.append({"field": field, "issue": "The document prints no total: this is the sum of its service lines — verify before use"})
    if ctx.get("text_filled_fields"):
        findings.append({"field": None, "issue": "Read directly from the page next to its label (the extraction model did not return it): "
                         + ", ".join(ctx["text_filled_fields"]) + " — verify before use"})
    # Calibrated-uncertainty routing: grounded but the model itself reported
    # low probability — human glance warranted, hallucination language not.
    for field, tier in ctx.get("field_confidence", {}).items():
        if tier == "medium":
            findings.append({"field": field, "issue": "Model reported low confidence in this value — verify before use"})
    # Shape warnings from normalize().
    for w in ctx.get("normalize_warnings", []):
        findings.append({"field": w["field"], "issue": w["issue"]})
    # Deterministic field cross-checks from stage_fields (Luhn failures,
    # conflicting totals): deterministic evidence beats LLM output, so these
    # always surface.
    for issue in ctx.get("field_check_issues", []):
        findings.append({"field": "extracted_fields", "issue": issue})
    ctx["findings"] = findings
    ctx["status"] = "ANALYZED" if not findings else "ANALYZED_WITH_FINDINGS"
    return ctx


def _index_safe_extracted(extracted: dict, normalized: dict) -> dict:
    """Copy of `extracted` safe for the search index. OpenSearch maps the
    date fields as `date`, and a raw value as printed on the document
    ("06/15/2026") is rejected there -- which failed the WHOLE document's
    indexing, so it never became searchable. Dates go in as their normalized
    ISO form, or are left out when they could not be parsed; the raw string
    is still stored with the analysis itself."""
    out = {}
    for k, v in (extracted or {}).items():
        if "date" in k:
            if normalized.get(k):
                out[k] = normalized[k]
            continue
        # The index maps these fields as text. A row or an object is refused
        # there (and took the whole document's indexing with it), so anything
        # that is not a plain value goes in as its text.
        if isinstance(v, dict):
            v = as_text(v)
        elif isinstance(v, (list, tuple)):
            v = [x if isinstance(x, (str, int, float)) and not isinstance(x, bool) else as_text(x) for x in v]
        out[k] = v
    return out


def stage_index(ctx: dict, cfg: dict) -> dict:
    """Build the OpenSearch document body. main.persist() performs the actual
    index call (it owns the client); this stage decides WHAT is searchable:
    full text, trusted normalized fields, doc type, and confidence map."""
    ctx["index_body"] = {
        "doc_type": ctx.get("doc_type"),
        "text": ctx.get("text", "")[:100000],
        "extracted": _index_safe_extracted(ctx.get("extracted", {}), ctx.get("normalized", {})),
        "extracted_fields": ctx.get("extracted_fields", {}),
        "normalized": ctx.get("normalized", {}),
        "field_confidence": ctx.get("field_confidence", {}),
        "vlm_field_probability": ctx.get("vlm_field_probability", {}),
        "status": ctx.get("status"),
    }
    return ctx


# ---------------------------------------------------------------------------
# Docking engine
# ---------------------------------------------------------------------------

_ENV_VAR_RE = re.compile(r"\$\{(\w+)(:-([^}]*))?\}")


def _expand_env_vars(text: str) -> str:
    """${VAR:-default} / ${VAR} substitution -- PyYAML does not do shell-style
    env-var expansion on its own, so pipeline.yaml's endpoint/model defaults
    (e.g. ${VLM_ENDPOINT:-http://vllm:8000/v1}) were being passed through
    verbatim as literal strings. Confirmed live: the VLM client was trying to
    connect to the literal unexpanded string as a URL, surfacing as a generic
    "Connection error" with no hint the config was never resolved."""
    def repl(m: re.Match) -> str:
        name, _, default = m.groups()
        return os.environ.get(name, default or "")
    return _ENV_VAR_RE.sub(repl, text)


_PIPELINE_CACHE: dict[str, tuple[float, dict]] = {}


def load_pipeline(path: str = "pipeline.yaml") -> dict:
    """YAML spec, cached by mtime — run_pipeline() is called per document,
    and re-reading + re-parsing + env-expanding the spec each time is pure
    overhead. An edit to pipeline.yaml invalidates via mtime."""
    mtime = os.path.getmtime(path)
    cached = _PIPELINE_CACHE.get(path)
    if cached and cached[0] == mtime:
        return cached[1]
    with open(path) as f:
        spec = yaml.safe_load(_expand_env_vars(f.read()))
    _PIPELINE_CACHE[path] = (mtime, spec)
    return spec


def run_pipeline(ctx: dict, case: dict | None = None, schema_overrides: dict | None = None) -> dict:
    spec = load_pipeline()
    # Tenants may declare their own extraction schemas (NG: Program Manifest
    # documents.schemas). Overrides MERGE onto the built-ins — a tenant
    # schema shadows the same-named built-in, unknown names just extend.
    schemas = {**spec["schemas"], **(schema_overrides or {})}
    for st in spec["stages"]:
        if not st.get("enabled", True):
            continue
        # Conditional docking: a stage runs only when its `when` flag is truthy
        # in the pipeline context (e.g. PaddleOCR only when Docling found a scan).
        cond = st.get("when")
        if cond and not ctx.get(cond):
            continue
        name, cfg = st["name"], st.get("config", {})
        try:
            with otel_setup.span(f"stage {name}") as _sp:
                if name == "vlm_extract":
                    ctx = stage_vlm_extract(ctx, cfg, schemas)
                elif name == "validate":
                    ctx = stage_validate(ctx, cfg, case)
                else:
                    fn = globals().get(f"stage_{name}")
                    if fn is None:
                        raise RuntimeError(f"pipeline stage not implemented: {name}")
                    ctx = fn(ctx, cfg)
                _sp.set_attribute("idre.doc_type", str(ctx.get("doc_type") or ""))
        except Exception as exc:
            # docling is the primary parser -- if it fails there is no usable
            # text/markdown/tables at all, so that failure stays fatal (same
            # as before). Every other stage (ocr, layout/seal-detection,
            # vlm_extract) is an enhancement on top of what docling already
            # extracted: a missing vLLM endpoint or a PaddleX dependency
            # issue shouldn't take the whole document down with it. Confirmed
            # live: layout's PP-StructureV3 construction failing aborted
            # run_pipeline() before persist() or the DOCS_VERIFIED/
            # DOC_ANALYZED signal ever ran -- for every document, forever,
            # not just the one that happened to trigger it first.
            if name == "docling":
                raise
            if name == "vlm_extract" and _transient(exc):
                raise ModelUnavailable(str(exc)) from exc
            ctx.setdefault("findings", []).append(
                {"stage": name, "issue": f"stage failed, skipped: {exc}"}
            )
    ctx.setdefault("status", "ANALYZED")
    return ctx


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def np_from_pil(img: Image.Image):
    import numpy as np
    return np.array(img.convert("RGB"))


def pil_to_b64(img: Image.Image) -> str:
    """JPEG, not PNG: a 300dpi page PNG is 5-10x the bytes of a q85 JPEG at
    no loss the VLM can detect, and the payload rides base64 into the prompt
    of the LOCAL Ollama — smaller payload = faster encode, faster transfer,
    less memory on a shared instance with an OOM history."""
    import base64
    buf = io.BytesIO()
    img.convert("RGB").save(buf, format="JPEG", quality=85)
    return base64.b64encode(buf.getvalue()).decode()


def pdf_to_pages(data: bytes, dpi: int = 200,
                 max_pages: int | None = None) -> tuple[list[Image.Image], bool]:
    """Rasterize PDF pages (pypdfium2 bundled with PaddleOCR toolchain).

    Page-capped: each page becomes a full-resolution bitmap in worker memory,
    so an unbounded render of a huge/hostile PDF is a memory bomb that kills
    the consumer for every tenant. Returns (pages, truncated)."""
    import pypdfium2 as pdfium

    cap = max_pages or int(os.environ.get("DOC_INTEL_MAX_PAGES", "150"))
    pages = []
    truncated = False
    with PDFIUM_LOCK:
        pdf = pdfium.PdfDocument(data)
        for i, page in enumerate(pdf):
            if i >= cap:
                truncated = True
                break
            pages.append(page.render(scale=dpi / 72).to_pil())
    return pages, truncated


# OOXML safety: a tiny .docx can expand to gigabytes when parsed. Before
# Docling opens an office container, sum the UNCOMPRESSED sizes from the zip
# central directory (no decompression needed) and reject beyond the cap.
OOXML_MAX_EXPANDED_BYTES = 512 * 1024 * 1024   # 512 MiB expanded ceiling
OOXML_MAX_RATIO = 200                           # compressed:expanded sanity bound


def ooxml_safe(data: bytes) -> tuple[bool, str]:
    """(safe, reason). Reads central directory metadata only — never inflates."""
    import zipfile
    try:
        zf = zipfile.ZipFile(io.BytesIO(data))
    except zipfile.BadZipFile:
        return False, "office container is corrupt or not a valid zip"
    total = sum(i.file_size for i in zf.infolist())
    if total > OOXML_MAX_EXPANDED_BYTES:
        return False, f"expands to {total >> 20} MiB — exceeds the {OOXML_MAX_EXPANDED_BYTES >> 20} MiB safety cap"
    if len(data) > 0 and total / len(data) > OOXML_MAX_RATIO:
        return False, f"expansion ratio {total // max(len(data),1)}:1 — probable zip bomb"
    return True, ""


def sniff_doc_kind(data: bytes, content_type: str) -> str:
    """Cheap magic-byte gate run BEFORE any parsing: pdf / image / office /
    text / unknown. The API edge enforces an allowlist, but events can also
    arrive from paths that predate it — defense in depth, and a clean
    UNSUPPORTED_TYPE instead of an exception-marked ERROR."""
    if data[:5] == b"%PDF-":
        return "pdf"
    if data[:4] == b"PK\x03\x04":
        return "office"
    if (data[:8] == b"\x89PNG\r\n\x1a\n" or data[:3] == b"\xff\xd8\xff"
            or data[:4] in (b"II*\x00", b"MM\x00*", b"GIF8") or data[:2] == b"BM"
            or (data[:4] == b"RIFF" and data[8:12] == b"WEBP")):
        return "image"
    sample = data[:4096]
    if sample and b"\x00" not in sample:
        printable = sum(1 for b in sample if b in b"\t\n\r" or 0x20 <= b < 0x7F or b >= 0x80)
        if printable / len(sample) > 0.95:
            return "text"
    return "unknown"


def ensure_pages(ctx: dict, dpi: int = 200) -> list[Image.Image]:
    """Lazily rasterize PDF pages. Only stages that truly need bitmaps
    (OCR fallback, vision VLM, layout analysis) pay the render cost —
    previously EVERY PDF was rasterized at 200dpi before Docling even
    ran, and for born-digital documents (the common case) those bitmaps
    were never used: seconds of CPU and hundreds of MB per doc, wasted."""
    if ctx.get("pages"):
        return ctx["pages"]
    if sniff_doc_kind(ctx.get("raw_bytes", b""), ctx.get("content_type", "")) != "pdf":
        return ctx.get("pages") or []
    pages, truncated = pdf_to_pages(ctx["raw_bytes"], dpi=dpi)
    ctx["pages"] = pages
    if truncated:
        ctx["pages_truncated"] = True
    return pages


def bytes_to_pages(data: bytes, content_type: str,
                   kind: str | None = None) -> tuple[list[Image.Image], bool]:
    """(pages, truncated). kind comes from sniff_doc_kind when available."""
    k = kind or ("pdf" if "pdf" in content_type else "image")
    if k == "pdf":
        return pdf_to_pages(data)
    if k == "image":
        # Decode NOW and keep a plain RGB copy. Image.open() is lazy: it only
        # reads the header, and the pixels were decoded later from the same
        # stream by whichever stage touched the page first -- which failed
        # mid-pipeline ("unrecognized data stream contents"). A file that is
        # slightly truncated still loads what it has.
        try:
            img = Image.open(io.BytesIO(data))
            img.load()
        except Exception:
            from PIL import ImageFile
            ImageFile.LOAD_TRUNCATED_IMAGES = True
            img = Image.open(io.BytesIO(data))
            img.load()
        return [img.convert("RGB")], False
    return [], False  # office/text: Docling reads raw bytes; no raster pages
