# Author: Patrick Munis at newwave
"""TPA onboarding package validation (feature flag: tpa_onboarding).

Consumes `tpa_doc.uploaded` events (published by case-api tpa_onboarding.go
and healthplan_onboarding.go — evt["actor"] is "tpa" (default) or
"healthplan"; table prefix and the offer-substantiation expectation follow
the actor):

  DISCRETE  — one event per slot upload; validate the document against the
              slot's expectation and write the verdict onto
              public.tpa_package_docs.
  MERGED    — one event for the giant PDF; split it by page-level
              classification into the six required slots, seal + store each
              segment through the vault, then validate each segment.

Every verdict is machine-readable (reasons jsonb), fail-open on analyzer
outages (NEEDS_REVIEW, never silent acceptance), and the package flips to
READY only when all six slots are ACCEPTED. The filing gate in case-api
re-checks READY + verified payment, so a worker bug can delay a filing but
never let an unvalidated package through.
"""

from __future__ import annotations

import hashlib
import base64
import io
import json
import uuid

import httpx
import psycopg

from pipeline import PDFIUM_LOCK

SLOTS = [
    "EOB", "ITEMIZED_BILL", "PROOF_OF_COVERAGE",
    "OPEN_NEGOTIATION_NOTICE", "OFFER_SUBSTANTIATION", "IDRE_ATTESTATION",
]

# Slot -> classifier type that must win (None = keyword floor instead).
SLOT_TYPE = {
    "EOB": "eob",
    "ITEMIZED_BILL": "idr_claim",
    "OFFER_SUBSTANTIATION": "idr_claim",
    "PROOF_OF_COVERAGE": None,
    "OPEN_NEGOTIATION_NOTICE": None,
    "IDRE_ATTESTATION": None,
}

# Keyword floor for slots without a dedicated classifier type — the document
# must say what it is, in its own words.
SLOT_KEYWORDS = {
    "PROOF_OF_COVERAGE": ["coverage", "eligible", "eligibility", "member", "subscriber"],
    "OPEN_NEGOTIATION_NOTICE": ["open negotiation", "30 business days", "notice of initiation"],
    "IDRE_ATTESTATION": ["attest", "attestation", "certify", "under penalty"],
}

MIN_TEXT_CHARS = 40  # below this the document is illegible to the pipeline


def _pdf_pages(raw: bytes) -> tuple[list, object]:
    import pypdfium2 as pdfium
    pdf = pdfium.PdfDocument(raw)
    texts = []
    for page in pdf:
        tp = page.get_textpage()
        texts.append(tp.get_text_range() or "")
        tp.close()
        page.close()
    return texts, pdf


def _seal(vault: str, headers: dict, tenant: str, key: str, raw: bytes) -> bytes:
    resp = httpx.post(f"{vault}/docs/seal", timeout=60, headers=headers, json={
        "tenant": tenant, "key": key, "data_b64": base64.b64encode(raw).decode(),
    })
    resp.raise_for_status()
    return base64.b64decode(resp.json()["sealed_b64"])


# Payer-side expectation: a health plan's offer substantiation must document
# the QPA it derived — the median contracted rate methodology is the plan's
# whole case in baseball-style arbitration.
QPA_KEYWORDS = ["qualifying payment amount", "qpa", "median contracted rate"]


def _verdict(slot: str, filename: str, text: str, classify_text, extract_fields,
             actor: str = "tpa") -> tuple[str, list[dict]]:
    """(status, reasons). Conservative: only a confident wrong-type is a
    REJECTED; ambiguity and illegibility go to NEEDS_REVIEW for a human."""
    reasons: list[dict] = []
    body = (text or "").strip()
    if len(body) < MIN_TEXT_CHARS:
        reasons.append({"code": "LEGIBILITY_LOW",
                        "detail": f"only {len(body)} characters of extractable text (scanned or corrupt?)"})
        return "NEEDS_REVIEW", reasons
    dtype, score, margin = classify_text(filename, body)
    expected = SLOT_TYPE[slot]
    if expected:
        if dtype == "unrelated":
            reasons.append({"code": "TYPE_UNCLEAR",
                            "detail": "no recognizable document-type signal"})
            return "NEEDS_REVIEW", reasons
        if dtype != expected:
            reasons.append({"code": "TYPE_MISMATCH",
                            "detail": f"classified as {dtype} (score {score}), expected {expected}"})
            return "REJECTED", reasons
        if margin < 2:
            reasons.append({"code": "TYPE_AMBIGUOUS",
                            "detail": f"{dtype} won by a thin margin ({margin})"})
            return "NEEDS_REVIEW", reasons
        # Field floor: billing documents must carry something to fight about.
        fields = extract_fields(body) or {}
        idents = fields.get("identifiers") or {}
        if expected == "eob" and not (idents.get("claim_number") or fields.get("cpt_codes")):
            reasons.append({"code": "FIELDS_MISSING",
                            "detail": "no claim number or CPT/HCPCS codes extracted"})
            return "NEEDS_REVIEW", reasons
        if expected == "idr_claim" and not fields.get("cpt_codes"):
            reasons.append({"code": "FIELDS_MISSING",
                            "detail": "no CPT/HCPCS codes extracted"})
            return "NEEDS_REVIEW", reasons
        if (actor == "healthplan" and slot == "OFFER_SUBSTANTIATION"
                and not any(k in body.lower() for k in QPA_KEYWORDS)):
            reasons.append({"code": "QPA_DOC_MISSING",
                            "detail": "health plan offer substantiation does not document the "
                                      "qualifying payment amount / median contracted rate methodology"})
            return "NEEDS_REVIEW", reasons
        return "ACCEPTED", reasons
    # Keyword-floor slots.
    low = body.lower()
    if any(k in low for k in SLOT_KEYWORDS[slot]):
        return "ACCEPTED", reasons
    reasons.append({"code": "KEYWORDS_MISSING",
                    "detail": f"none of {SLOT_KEYWORDS[slot]} found in document text"})
    return "NEEDS_REVIEW", reasons


def _fields(ctx: dict, text: str) -> dict:
    """The eligibility facts the document states (main.package_fields), kept
    with the slot and carried onto the case when the set is filed. Reading
    them never costs the verdict."""
    try:
        return (ctx.get("package_fields") or (lambda _t: {}))(text) or {}
    except Exception as exc:  # noqa: BLE001
        print(f"tpa-doc: could not read eligibility facts: {type(exc).__name__}: {exc}", flush=True)
        return {}


def _notify_rejected(conn, tenant: str, package_id: str, prefix: str) -> None:
    """The TPA or plan hears in the portal's bell that a document of its
    package was rejected (addressed to the organization, as case-api's
    notifyOrg does), instead of finding out only by opening the package."""
    if prefix == "tpa":
        q = """INSERT INTO public.notifications (tenant, user_sub, type, body, link)
               SELECT p.tenant, 'tpa:' || p.tpa_id::text, 'PACKAGE_REJECTED',
                      'A document for ' || c.party_name || ' was rejected — open its package to see why and replace it', '#/tpa'
               FROM public.tpa_packages p JOIN public.tpa_clients c ON c.id = p.client_id
               WHERE p.tenant=%s AND p.id=%s"""
    else:
        q = """INSERT INTO public.notifications (tenant, user_sub, type, body, link)
               SELECT p.tenant, 'plan:' || p.plan_id::text, 'PACKAGE_REJECTED',
                      'A document for ' || c.party_name || ' was rejected — open its package to see why and replace it', '#/healthplan'
               FROM public.hp_packages p JOIN public.hp_parties c ON c.id = p.party_id
               WHERE p.tenant=%s AND p.id=%s"""
    conn.execute(q, (tenant, package_id))


def _audit_package(conn, tenant: str, package_id: str, prefix: str, status: str) -> None:
    """A package's checked status goes into the organization's audit trail
    (public.audit_log, hash-chained exactly as case-api's logAudit does:
    hash = sha256(prev_hash + payload text), one writer per tenant)."""
    if prefix == "tpa":
        row = conn.execute("""SELECT p.tpa_id::text, c.party_name FROM public.tpa_packages p
                              JOIN public.tpa_clients c ON c.id = p.client_id WHERE p.tenant=%s AND p.id=%s""",
                           (tenant, package_id)).fetchone()
        org = f"tpa:{row[0]}" if row else ""
    else:
        row = conn.execute("""SELECT p.plan_id::text, c.party_name FROM public.hp_packages p
                              JOIN public.hp_parties c ON c.id = p.party_id WHERE p.tenant=%s AND p.id=%s""",
                           (tenant, package_id)).fetchone()
        org = f"plan:{row[0]}" if row else ""
    if not row:
        return
    payload = json.dumps({"org": org, "package_id": str(package_id), "party": row[1], "status": status, "by": "system:doc-intel"},
                         sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    conn.execute("SELECT pg_advisory_xact_lock(hashtext('audit_log:' || %s))", (tenant,))
    prev = conn.execute("SELECT hash FROM public.audit_log WHERE tenant=%s ORDER BY id DESC LIMIT 1", (tenant,)).fetchone()
    prev_hash = prev[0] if prev else "GENESIS"
    digest = hashlib.sha256((prev_hash + payload).encode()).hexdigest()
    conn.execute("""INSERT INTO public.audit_log (tenant, case_id, action, payload, prev_hash, hash, org)
                    VALUES (%s, '', 'PACKAGE_STATUS', %s, %s, %s, %s)""", (tenant, payload, prev_hash, digest, org))


def _refresh_package(conn, tenant: str, package_id: str, prefix: str = "tpa") -> str:
    """Package status from its documents: READY once every one has been read. A document that doesn't match its
    slot is flagged for the Reviewer (case-api attaches it with the rest and says so); it never holds the set."""
    # Lock the package first: two workers finishing two of its documents at
    # once each counted the other's verdict as not there yet (uncommitted),
    # and the package stayed VALIDATING with all six accepted.
    locked = conn.execute(f"SELECT status FROM public.{prefix}_packages WHERE tenant=%s AND id=%s FOR UPDATE",
                          (tenant, package_id)).fetchone()
    was = locked[0] if locked else ""
    rows = conn.execute(
        f"SELECT slot, status FROM public.{prefix}_package_docs WHERE tenant=%s AND package_id=%s",
        (tenant, package_id)).fetchall()
    by_slot = {r[0]: r[1] for r in rows}
    if by_slot and not any(v in ("PENDING", "ANALYZING") for v in by_slot.values()):
        status = "READY"
    else:
        status = "VALIDATING"
    conn.execute(
        f"UPDATE public.{prefix}_packages SET status=%s, updated_at=now() WHERE tenant=%s AND id=%s",
        (status, tenant, package_id))
    if status == "REJECTED" and was != "REJECTED":
        _notify_rejected(conn, tenant, package_id, prefix)
    if status != was:
        _audit_package(conn, tenant, package_id, prefix, status)
    # A READY package can complete the party's docs gate (employer groups on
    # the health-plan rail have no NPI requirement).
    if prefix == "tpa":
        conn.execute(
            """UPDATE public.tpa_clients c SET onboarding_status='READY'
               WHERE c.tenant=%s AND c.id=(SELECT client_id FROM public.tpa_packages WHERE id=%s)
                 AND %s='READY'
                 AND c.npi IS NOT NULL AND c.tin IS NOT NULL AND c.address IS NOT NULL
                 AND EXISTS (SELECT 1 FROM public.tpa_payment_methods m
                              WHERE m.tenant=c.tenant AND m.client_id=c.id AND m.verified AND m.status='ACTIVE')""",
            (tenant, package_id, status))
    else:
        conn.execute(
            """UPDATE public.hp_parties c SET onboarding_status='READY'
               WHERE c.tenant=%s AND c.id=(SELECT party_id FROM public.hp_packages WHERE id=%s)
                 AND %s='READY'
                 AND c.tin IS NOT NULL AND c.address IS NOT NULL
                 AND (c.party_type='EMPLOYER_GROUP' OR c.npi IS NOT NULL)
                 AND EXISTS (SELECT 1 FROM public.hp_payment_methods m
                              WHERE m.tenant=c.tenant AND m.party_id=c.id AND m.verified AND m.status='ACTIVE')""",
            (tenant, package_id, status))
    return status


# Phrases that name a keyword-floor document in its own heading. Checked on
# the page's first lines only: an offer substantiation may well mention "open
# negotiation" further down without being the notice.
SLOT_TITLES = {
    "OPEN_NEGOTIATION_NOTICE": ["open negotiation notice", "notice of initiation", "open negotiation period"],
    "IDRE_ATTESTATION": ["attestation", "i attest", "under penalty of perjury"],
    "PROOF_OF_COVERAGE": ["proof of coverage", "certificate of coverage", "eligibility verification",
                          "verification of coverage", "coverage verification"],
}


def _headings(text: str) -> list[str]:
    """The page's heading lines, lower-cased: among its first four lines, the
    ones set as a title (all capitals) -- a banner or letterhead may sit above
    the title, and body sentences can mention other documents."""
    lines = [ln.strip() for ln in (text or "").splitlines() if ln.strip()][:4]
    return [ln.lower() for ln in lines if ln.isupper()]


def _starts(text: str, phrases: list[str]) -> bool:
    """A heading names it, or a line among the first four begins with it."""
    lines = [ln.strip().lower() for ln in (text or "").splitlines() if ln.strip()][:4]
    return (any(p in h for h in _headings(text) for p in phrases)
            or any(ln.startswith(p) for ln in lines for p in phrases))


def _page_slot(text: str, classify_text) -> str | None:
    """The slot one page starts or belongs to; None = no signal of its own
    (a continuation page, which stays with the document before it)."""
    for slot, titles in SLOT_TITLES.items():
        if _starts(text, titles):
            return slot
    dtype, score, _ = classify_text("", text)
    if dtype == "eob" and score >= 2:
        return "EOB"
    if dtype == "idr_claim" and score >= 2:
        # An itemized bill and an offer substantiation are both billing
        # documents; the offer is the one whose heading argues an amount.
        return "OFFER_SUBSTANTIATION" if _starts(text, ["offer"]) else "ITEMIZED_BILL"
    return None


def split_pages(texts: list[str], classify_text) -> dict[str, tuple[int, int]]:
    """Slot -> (first page, last page), 0-based inclusive.

    Each page is judged on its own (_page_slot): a page that starts a new
    document starts a new segment, and only pages with no signal of their own
    carry on the document before them. The splitter used to classify by
    document type alone and let every page without a strong type inherit its
    predecessor, so the coverage letter, the open-negotiation notice and the
    offer (all after the itemized bill) were swallowed into the bill's
    segment and reported "not found". The first segment of a slot wins."""
    segments: list[list] = []  # [slot, start, end]
    for i, t in enumerate(texts):
        slot = _page_slot(t, classify_text)
        if slot is None and segments:
            segments[-1][2] = i
        elif segments and segments[-1][0] == slot:
            segments[-1][2] = i
        else:
            segments.append([slot, i, i])
    assigned: dict[str, tuple[int, int]] = {}
    for slot, a, b in segments:
        if slot and slot not in assigned:
            assigned[slot] = (a, b)
    return assigned


def handle_tpa_doc(evt: dict, ctx: dict) -> None:
    """Entry point called from main.run() on tpa_doc.uploaded events.
    ctx carries the shared helpers so this module stays testable:
    {dsn, vault, vault_headers, minio, classify_text, extract_fields,
     fetch_and_decrypt, features_for}."""
    tenant = evt.get("tenant")
    if not tenant:
        return
    actor = evt.get("actor") or "tpa"
    flag = "healthplan_onboarding" if actor == "healthplan" else "tpa_onboarding"
    if not ctx["features_for"](tenant).get(flag, True):
        return
    if evt.get("merged"):
        _handle_merged(evt, ctx, actor)
    else:
        _handle_discrete(evt, ctx, actor)


def _handle_discrete(evt: dict, ctx: dict, actor: str = "tpa") -> None:
    tenant, doc_id = evt["tenant"], evt["package_doc_id"]
    prefix = "hp" if actor == "healthplan" else "tpa"
    with psycopg.connect(ctx["dsn"]) as conn:
        conn.execute(
            f"UPDATE public.{prefix}_package_docs SET status='ANALYZING' WHERE tenant=%s AND id=%s",
            (tenant, doc_id))
        conn.commit()
    try:
        raw = ctx["fetch_and_decrypt"](tenant, evt["object_key"])
        if raw[:4] == b"%PDF":
            with PDFIUM_LOCK:
                texts, pdf = _pdf_pages(raw)
                pdf.close()
            text = "\n".join(texts)
        else:
            text = raw.decode("utf-8", "ignore")
        status, reasons = _verdict(evt["slot"], evt.get("filename") or "", text,
                                   ctx["classify_text"], ctx["extract_fields"], actor)
        fields = _fields(ctx, text)
    except Exception as exc:  # noqa: BLE001 — analyzer outage must fail open
        status, reasons = "NEEDS_REVIEW", [{"code": "ANALYZER_ERROR", "detail": str(exc)[:300]}]
        fields = {}
    with psycopg.connect(ctx["dsn"]) as conn:
        conn.execute(
            f"""UPDATE public.{prefix}_package_docs
               SET status=%s, reasons=%s::jsonb, fields=%s::jsonb, analyzed_at=now()
               WHERE tenant=%s AND id=%s""",
            (status, json.dumps(reasons), json.dumps(fields), tenant, doc_id))
        pkg_status = _refresh_package(conn, tenant, evt["package_id"], prefix)
        conn.commit()
    print(f"tpa-doc {doc_id} slot={evt['slot']} -> {status} ({len(reasons)} reasons); "
          f"package {evt['package_id']} -> {pkg_status}", flush=True)


def _handle_merged(evt: dict, ctx: dict, actor: str = "tpa") -> None:
    """Split the giant PDF by page classification into the six slots."""
    tenant, package_id = evt["tenant"], evt["package_id"]
    prefix = "hp" if actor == "healthplan" else "tpa"
    try:
        raw = ctx["fetch_and_decrypt"](tenant, evt["object_key"])
        # All PDFium work (read, split, cut out each slot's pages) under the
        # lock; sealing and storing happen after it is released.
        with PDFIUM_LOCK:
            texts, pdf = _pdf_pages(raw)
            try:
                assigned = split_pages(texts, ctx["classify_text"])
                segments = {slot: _export_segment(pdf, a, b) for slot, (a, b) in assigned.items()}
            finally:
                pdf.close()
        with psycopg.connect(ctx["dsn"]) as conn:
            for slot in SLOTS:
                if slot in assigned:
                    start, end = assigned[slot]
                    seg = segments[slot]
                    key = f"{tenant}/{prefix}-packages/{package_id}/{slot.lower()}-{uuid.uuid4()}.enc"
                    ct = _seal(ctx["vault"], ctx["vault_headers"], tenant, key, seg)
                    ctx["minio"].put_object(ctx.get("doc_bucket", "idre-docs"), key, io.BytesIO(ct), len(ct))
                    text = "\n".join(texts[start:end + 1])
                    status, reasons = _verdict(slot, f"{slot.lower()}.pdf", text,
                                               ctx["classify_text"], ctx["extract_fields"], actor)
                    conn.execute(
                        f"""INSERT INTO public.{prefix}_package_docs
                             (tenant, package_id, slot, object_key, filename, status, reasons, fields, page_start, page_end, analyzed_at)
                           VALUES (%s,%s,%s,%s,%s,%s,%s::jsonb,%s::jsonb,%s,%s,now())
                           ON CONFLICT (package_id, slot) DO UPDATE SET
                             object_key=excluded.object_key, filename=excluded.filename,
                             status=excluded.status, reasons=excluded.reasons, fields=excluded.fields,
                             page_start=excluded.page_start, page_end=excluded.page_end,
                             analyzed_at=now()""",
                        (tenant, package_id, slot, key, f"{slot.lower()} (pages {start + 1}-{end + 1})",
                         status, json.dumps(reasons + [{"code": "SPLIT_FROM_MERGED",
                                                        "detail": f"pages {start + 1}-{end + 1}"}]),
                         json.dumps(_fields(ctx, text)), start + 1, end + 1))
                else:
                    # Slot never found in the merged PDF: NEEDS_REVIEW with the
                    # source as evidence — a human decides whether it's missing
                    # or the splitter missed it.
                    conn.execute(
                        f"""INSERT INTO public.{prefix}_package_docs
                             (tenant, package_id, slot, object_key, filename, status, reasons, analyzed_at)
                           VALUES (%s,%s,%s,%s,%s,'NEEDS_REVIEW',
                                   %s::jsonb, now())
                           ON CONFLICT (package_id, slot) DO UPDATE SET
                             status='NEEDS_REVIEW', reasons=excluded.reasons, analyzed_at=now()""",
                        (tenant, package_id, slot, evt["object_key"], evt.get("filename") or "merged.pdf",
                         json.dumps([{"code": "SLOT_NOT_FOUND_IN_MERGED",
                                      "detail": "page classification found no segment for this slot"}])))
            pkg_status = _refresh_package(conn, tenant, package_id, prefix)
            conn.commit()
        print(f"tpa-doc merged {package_id}: slots {sorted(assigned)} -> package {pkg_status}", flush=True)
    except Exception as exc:  # noqa: BLE001
        with psycopg.connect(ctx["dsn"]) as conn:
            conn.execute(
                f"UPDATE public.{prefix}_packages SET status='REJECTED', updated_at=now() WHERE tenant=%s AND id=%s",
                (tenant, package_id))
            conn.commit()
        print(f"tpa-doc merged {package_id} FAILED: {exc}", flush=True)


def _export_segment(pdf, start: int, end: int) -> bytes:
    """Pages [start, end] of the merged PDF as a standalone PDF."""
    import pypdfium2 as pdfium
    out = pdfium.PdfDocument.new()
    out.import_pages(pdf, [i for i in range(start, end + 1)])
    buf = io.BytesIO()
    out.save(buf)
    out.close()
    return buf.getvalue()
