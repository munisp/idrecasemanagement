"""doc-intel service: consumes doc.uploaded events from Kafka, fetches the
ciphertext from MinIO, decrypts via the vault, runs the docked PaddleOCR+VLM
pipeline, and persists results (Postgres + OpenSearch + Kafka + Temporal signal).

Run: python main.py   (GPU optional — PaddleOCR auto-detects; VLM is remote)
"""

from __future__ import annotations

import json
import os
import re
import signal
import sys
import time
from collections import deque
from concurrent.futures import ThreadPoolExecutor

import httpx
import otel_setup
import psycopg
from confluent_kafka import Consumer
from minio import Minio
from opensearchpy import OpenSearch

from eobshape import as_lines, as_money, as_text, line_text, lines_text
from pipeline import (ModelUnavailable, bytes_to_pages, image_bytes, ooxml_safe, run_pipeline, sniff_doc_kind,
                      straighten_pages, upright_pages)
from rules_engine import expand_template, fire_rules, load_rules
from check_processor import extract_check
import learning


def is_blocked(evt: dict, kind: str, subj_id: str) -> bool:
    """case-api quarantines rule-blocked uploads (analysis_status='BLOCKED')
    AFTER publishing doc.uploaded — doc-intel must honor the quarantine and
    never run OCR/VLM over blocked bytes."""
    table = (f"tenant_{evt['tenant']}.documents" if kind == "case"
             else "public.application_documents")
    try:
        with psycopg.connect(DSN) as c:
            row = c.execute(
                f"SELECT analysis_status FROM {table} WHERE id=%s",
                (evt["doc_id"],),
            ).fetchone()
        return bool(row and row[0] == "BLOCKED")
    except psycopg.Error:
        return False  # fail open: quarantine is enforced again at read time


def fire_doc_rules(evt: dict, kind: str, subj_id: str, ctx: dict) -> None:
    """doc.analyzed program rules — same engine semantics as the Go side
    (rules_engine mirrors rules.go). Facts describe the analysis outcome;
    actions drive notifications, activity, review flags, and case detail
    fields. Fresh rule read per document: admin edits apply immediately."""
    low_conf = [f for f, cf in ctx.get("field_confidence", {}).items() if cf == "low"]
    facts = {
        "tenant": evt["tenant"], "doc_id": evt["doc_id"],
        "case_id": subj_id if kind == "case" else "",
        "application_id": subj_id if kind == "application" else "",
        "doc_type": ctx.get("doc_type") or "",
        "analysis_status": ctx.get("status", ""),
        "ungrounded_count": len(low_conf),
        "scan_quality_poor": bool(ctx.get("scan_quality_poor")),
        "ocr_confidence": ctx.get("ocr_confidence"),
        "ocr_low_confidence": bool(ctx.get("ocr_low_confidence")),
        "ambiguous": bool(ctx.get("classify_ambiguous")),
        "truncated": bool(ctx.get("result_truncated")),
        "findings_count": len(ctx.get("findings", [])),
    }
    with psycopg.connect(DSN, autocommit=True) as c:
        rules = load_rules(c, evt["tenant"], "doc.analyzed")
        if not rules:
            return
        actions, fired, suspect = fire_rules(rules, facts)
        for name in suspect:
            print(f"doc-intel: suspect rule {name!r} skipped on doc.analyzed",
                  file=sys.stderr, flush=True)
        for a in actions:
            atype, params = a.get("type", ""), a.get("params", {}) or {}
            if atype == "notify":
                c.execute(
                    """INSERT INTO public.notifications (tenant, user_sub, type, body)
                       VALUES (%s,'*',%s,%s)""",
                    (evt["tenant"], params.get("kind") or "MILESTONE",
                     expand_template(str(params.get("body", "")), facts)[:2000]),
                )
            elif atype == "log_activity" and kind == "case":
                c.execute(
                    """INSERT INTO public.case_activities (tenant, case_id, type, body)
                       VALUES (%s,%s,'RULE_DOC_ANALYZED',%s)""",
                    (evt["tenant"], subj_id,
                     expand_template(str(params.get("body", "")), facts)[:2000]),
                )
            elif atype == "flag_review" and kind == "case":
                c.execute(
                    f"UPDATE tenant_{evt['tenant']}.documents SET analysis_status='NEEDS_REVIEW' WHERE id=%s",
                    (evt["doc_id"],),
                )
            elif atype == "set_detail" and kind == "case":
                key = str(params.get("key", ""))
                if re.fullmatch(r"[a-z][a-z0-9_]{0,40}", key):
                    c.execute(
                        f"""UPDATE tenant_{evt['tenant']}.cases
                            SET details = jsonb_set(details, %s, to_jsonb(%s::text), true), updated_at=now()
                            WHERE id=%s""",
                        ("{" + key + "}", expand_template(str(params.get("value", "")), facts), subj_id),
                    )
        if fired:
            print(f"doc-intel: rules fired on doc.analyzed: {', '.join(fired)}",
                  file=sys.stderr, flush=True)

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
VAULT = os.environ.get("VAULT_URL", "http://localhost:8081")
TEMPORAL_SIGNAL_URL = os.environ.get("CASE_API_URL", "http://localhost:8080")
# The signal call below sent no Authorization header at all, wrapped in a
# silent except -- it has always 401'd (case-api's /signal routes require
# auth like every other /v1/tenants/... route), invisibly. WORKER_TOKEN is
# the same service credential case-api validates for idre-workflows' own
# service-to-service calls (authn.middleware, SERVICE_WORKER role).
WORKER_TOKEN = os.environ.get("WORKER_TOKEN", "")
AUTH_HEADERS = {"Authorization": f"Bearer {WORKER_TOKEN}"} if WORKER_TOKEN else {}

minio = Minio(
    os.environ.get("MINIO_ENDPOINT", "localhost:9000"),
    access_key=os.environ.get("MINIO_USER", "idre"),
    secret_key=os.environ.get("MINIO_PASSWORD", "idre-secret"),
    secure=False,
)
# The shared OpenSearch cluster's security plugin is on -- constructed with
# no credentials, every index() call below 401'd (opensearchpy raises
# AuthenticationException), uncaught, which escaped persist() into main()'s
# poll-loop catch-all and overwrote the already-successful analysis with a
# generic ERROR status. Confirmed live.
OPENSEARCH_PASSWORD = os.environ.get("OPENSEARCH_PASSWORD", "")
search = OpenSearch(
    hosts=[os.environ.get("OPENSEARCH_URL", "http://localhost:9200")],
    http_auth=("admin", OPENSEARCH_PASSWORD) if OPENSEARCH_PASSWORD else None,
    use_ssl=False,
)


def fetch_and_decrypt(tenant: str, object_key: str) -> bytes:
    import base64
    ct = minio.get_object("idre-docs", object_key).read()
    resp = httpx.post(f"{VAULT}/docs/open", timeout=60, json={
        "tenant": tenant, "key": object_key, "data_b64": base64.b64encode(ct).decode(),
    })
    resp.raise_for_status()
    return base64.b64decode(resp.json()["data_b64"])


def load_case(tenant: str, case_id: str) -> dict | None:
    with psycopg.connect(DSN) as c:
        row = c.execute(
            f"SELECT case_number, qpa_cents FROM tenant_{tenant}.cases WHERE id=%s",
            (case_id,),
        ).fetchone()
    return {"case_number": row[0], "qpa_cents": row[1]} if row else None


def unwrap_cloudevent(raw: dict) -> dict:
    """This consumer reads Kafka directly (confluent-kafka), not through a
    Dapr app-level subscription, but case-api publishes through its Dapr
    sidecar -- which wraps every message in a CloudEvents envelope
    (real payload nested under "data", top-level "type" forced to
    "com.dapr.event.sent") regardless of the pubsub component's config.
    Without this, every `evt.get("type") == "doc.uploaded"` check below
    silently never matched: the message was still consumed and committed
    (just skipped, no exception raised), so document analysis -- and the
    DOCS_VERIFIED/DOC_ANALYZED signals that unblock onboarding and offer
    workflows -- was dead with zero trace anywhere."""
    if raw.get("specversion") and isinstance(raw.get("data"), dict):
        return raw["data"]
    return raw


def subject(evt: dict) -> tuple[str, str]:
    """("case", id) or ("application", id) -- whichever key the event carries.
    Checked first, before anything touches evt["case_id"] directly: an
    application event has no case_id key at all (not an empty one), so a
    blind evt["case_id"] raises KeyError -- which used to propagate out of
    process(), into the bare `except Exception` in main()'s poll loop, which
    itself called mark() with the same blind evt["case_id"] access and raised
    AGAIN, uncaught, killing the whole consumer (every tenant's doc processing
    stops, not just the one event) until the pod restarted."""
    if evt.get("case_id"):
        return "case", evt["case_id"]
    return "application", evt["application_id"]


_SERVICE_LINE_KEYWORDS = (
    ("AIR AMBULANCE", "AIR_AMBULANCE"), ("ANESTHESIA", "ANESTHESIA"),
    ("RADIOLOGY", "RADIOLOGY"), ("LAB", "LAB"), ("EMERGENCY", "ER"), ("ER", "ER"),
)

# CPT/HCPCS code ranges are the AMA/CMS's own standard category boundaries
# (Anesthesia 00100-01999, Radiology 70010-79999, Pathology & Lab 80047-
# 89398, Emergency Dept E/M 99281-99285) -- not a guess, same standing as
# the POS-code -> provider_type map above. This is idr_claim's own field
# (cpt_hcpcs_codes); EOB's service_lines free-text field never has it, which
# is why idr_claim documents -- the common case -- never got a service_line
# backfill before even though EOB's keyword match existed.
_CPT_AIR_AMBULANCE = {"A0430", "A0431", "A0435", "A0436"}


def _service_line_from_cpt(codes_raw: str | None) -> str | None:
    m = re.search(r"\b([A-Z]\d{4}|\d{5})\b", as_text(codes_raw).upper())
    if not m:
        return None
    code = m.group(1)
    if code in _CPT_AIR_AMBULANCE:
        return "AIR_AMBULANCE"
    if not code.isdigit():
        return None
    n = int(code)
    if 99281 <= n <= 99285:
        return "ER"
    if 100 <= n <= 1999:
        return "ANESTHESIA"
    if 70010 <= n <= 79999:
        return "RADIOLOGY"
    if 80047 <= n <= 89398:
        return "LAB"
    return None

def _service_line_from_text(line) -> str | None:
    """Category of one EOB service line, or None. The line arrives either as
    free text or as a row ({"service": ..., "billed_amount_usd": ...}); only
    the description is read, never the column names. Keywords match whole
    words: a bare substring test found "ER" inside "SERVICE" and "LAB" inside
    "AVAILABLE", and filed an office visit under the emergency room. A
    procedure code written into the description is the better evidence, so
    it is tried first -- and when it names a category the keywords do not get
    a vote (99215, an office visit, is not an ER line whatever the text says)."""
    text = line_text(line).upper()
    if re.search(r"\b([A-Z]\d{4}|\d{5})\b", text):
        return _service_line_from_cpt(text)
    for kw, canon in _SERVICE_LINE_KEYWORDS:
        if re.search(rf"\b{re.escape(kw)}\b", text):
            return canon
    return None


# CMS place-of-service codes unambiguous enough to map to an eligibility
# provider_type without guessing: 21 (Inpatient Hospital) and 22/19 (on/off
# -campus Outpatient Hospital) are explicit facility-type statements; 11
# (Office) is the standard code for a physician/dentist practice. Anything
# else -- including "rural hospital", which isn't a POS code at all -- is
# left for a human, same miss-don't-guess rule as the dollar fields below.
_POS_PROVIDER_TYPE = {
    "21": "hospital_inpatient",
    "22": "hospital_outpatient",
    "19": "hospital_outpatient",
    "11": "physician_dentist",
}


# Type-of-bill (UB-04 form locator 4): the first digit after the leading zero
# is the facility type (1 = hospital), the second the care setting (1 or 2 =
# inpatient, 3 = outpatient). Only the hospital combinations are mapped.
_TOB_PROVIDER_TYPE = {"11": "hospital_inpatient", "12": "hospital_inpatient", "13": "hospital_outpatient"}


def provider_type_from_text(text: str | None) -> str | None:
    """Read the provider type off the form's own printed codes when the
    model did not extract a place of service: "Place of Service: 11" on a
    claim form, or a UB-04's type of bill. Only the codes that state a
    provider type outright are used -- a form with neither stays unknown
    rather than being guessed from its layout. Pure, so it is tested."""
    t = str(text or "").upper()
    m = re.search(r"(?:PLACE\s+OF\s+SERVICE|\bPOS\b)[\s:|.#(A-Z]{0,24}?\b(\d{2})\b", t)
    if m and m.group(1) in _POS_PROVIDER_TYPE:
        return _POS_PROVIDER_TYPE[m.group(1)]
    # Forms are often read with every label first and the values after, so
    # the code is not next to its label. A place-of-service code printed with
    # its standard description ("11 (Office)") is just as explicit -- accepted
    # when the form has a place-of-service field at all.
    if re.search(r"PLACE\s+OF\s+SERVICE|\bPOS\b", t):
        m = re.search(r"\b(11|19|21|22)\s*[(\-–:]\s*(OFFICE|INPATIENT\s+HOSPITAL|(?:ON|OFF)[\s-]*CAMPUS|OUTPATIENT\s+HOSPITAL)", t)
        if m:
            return _POS_PROVIDER_TYPE[m.group(1)]
    m = re.search(r"TYPE\s+OF\s+BILL[\s:|.#]{0,12}0?(\d{2})\d\b", t)
    if m and m.group(1) in _TOB_PROVIDER_TYPE:
        return _TOB_PROVIDER_TYPE[m.group(1)]
    return None


_HOSPITAL_NAME = re.compile(r"\b(HOSPITAL|MEDICAL CENTER|HEALTH SYSTEM|REGIONAL MEDICAL)\b")
_PRACTICE_NAME = re.compile(
    r"\b(P\.?C\.?|P\.?A\.?|M\.?D\.?|D\.?O\.?|D\.?D\.?S\.?|D\.?M\.?D\.?|PHYSICIANS?|DENTAL|DENTISTRY|"
    r"MEDICAL GROUP|ASSOCIATES|FAMILY (?:MEDICINE|PRACTICE)|CLINIC)(?=$|[\s,.])")
_INPATIENT = re.compile(r"\b(INPATIENT|ROOM (?:AND|&) BOARD|ADMISSION|ADMITTED|DISCHARGE|DRG)\b")
_OUTPATIENT = re.compile(r"\b(OUTPATIENT|EMERGENCY (?:ROOM|DEPARTMENT)|OBSERVATION|AMBULATORY SURGERY)\b")


def infer_provider_type(provider_name: str | None, text: str | None, cpt_codes=None) -> tuple[str | None, str]:
    """Provider type for a document that prints no place-of-service code --
    an EOB. Returns (type, basis). It is an INFERENCE and is stored as one:
    the eligibility review shows the basis and sends the result for a second
    look, because the provider type picks the dollar threshold.

      a hospital by name + inpatient wording   -> hospital_inpatient
      a hospital by name + outpatient wording  -> hospital_outpatient
      a practice by name (P.C., M.D., D.D.S., Physicians, Dental...) or
      office-visit / dental procedure codes    -> physician_dentist

    A hospital with neither wording, a rural hospital, and anything else stay
    unknown: None, for a person to decide.
    """
    name = as_text(provider_name).upper()
    t = as_text(text).upper()
    # Codes and service lines arrive as strings, lists, or rows
    # ({"service": "Office visit (99215)", ...}): lines_text reads the
    # describing cells of whatever it is given (joining rows blind crashed).
    codes = lines_text(cpt_codes)
    if _HOSPITAL_NAME.search(name):
        if _INPATIENT.search(t):
            return "hospital_inpatient", f"provider is a hospital ({provider_name}) and the document describes inpatient services"
        if _OUTPATIENT.search(t):
            return "hospital_outpatient", f"provider is a hospital ({provider_name}) and the document describes outpatient services"
        return None, ""
    if name and _PRACTICE_NAME.search(name):
        return "physician_dentist", f"provider is a professional practice ({provider_name})"
    hay = codes.upper() + " " + t
    if re.search(r"\b992(0[2-9]|1[0-5])\b", hay):
        return "physician_dentist", "office-visit procedure codes (99202-99215) are billed"
    if re.search(r"\bD\d{4}\b", hay) and "DENTAL" in t:
        return "physician_dentist", "dental procedure codes are billed"
    return None, ""


def line_of_business_from(value: str | None) -> str | None:
    """Map what a document calls the coverage ("Medicaid MMA", "Medicare
    Advantage HMO", "Employer group PPO") onto the program's Line of Business
    list. Order matters: Medicare Advantage before Medicare. None when the
    document's wording matches nothing -- never a default."""
    t = as_text(value).lower()
    if not t.strip():
        return None
    if "medicaid" in t:
        return "Medicaid"
    if "medicare advantage" in t or "medicare part c" in t or re.search(r"\bma\b", t):
        return "Medicare Advantage"
    if "medicare" in t:
        return "Medicare"
    if "marketplace" in t or "exchange" in t or "aca" in t.split():
        return "Marketplace"
    if any(k in t for k in ("commercial", "employer", "group", "ppo", "hmo", "epo", "pos plan")):
        return "Commercial"
    return None


def disputed_issue_from(billed: float | None, allowed: float | None, denial_codes) -> str | None:
    """What kind of dispute an EOB describes, from its own numbers: nothing
    allowed on a billed claim is a Denial; less allowed than billed is an
    Underpayment; more is an Overpayment. None when the EOB does not carry
    both figures -- the Reviewer decides then."""
    if billed is None or allowed is None or billed <= 0 or allowed < 0:
        return None
    if allowed == 0:
        return "Denial"
    if allowed < billed:
        return "Underpayment"
    if allowed > billed:
        return "Overpayment"
    return None


def provider_address_from(extracted: dict) -> dict:
    """The provider's address parts in the report's formats: the state as a
    2-letter code and a US ZIP. A part that does not fit its format is left
    out rather than stored wrong."""
    out = {}
    street = as_text(extracted.get("provider_street"))
    city = as_text(extracted.get("provider_city"))
    state = as_text(extracted.get("provider_state")).upper()
    zipc = as_text(extracted.get("provider_zip"))
    if street and len(street) <= 200:
        out["provider_street"] = street
    if city and len(city) <= 100:
        out["provider_city"] = city
    if re.fullmatch(r"[A-Z]{2}", state):
        out["provider_state"] = state
    if re.fullmatch(r"\d{5}(-\d{4})?", zipc):
        out["provider_postal"] = zipc
    return out


def _contracted_from_network_status(text: str | None) -> bool | None:
    """True/False only on an explicit in-/out-of-network statement in the
    document; None (don't guess) for anything else, including silence."""
    if isinstance(text, bool):
        return None  # a bare true/false does not say which way the question was asked
    t = re.sub(r"[\s_\-]+", " ", as_text(text).lower()).strip()
    if not t:
        return None
    # The "out" wordings first: "non participating" contains "participating".
    if re.search(r"\b(out of network|oon|non ?participating|not participating|non ?par|non ?contracted|"
                 r"not contracted|non ?network|not in network|out of plan)\b", t):
        return False
    if re.search(r"\b(in network|inn|participating|par|contracted|in plan)\b", t):
        return True
    return None


def eligibility_conflicts(current: dict, found: dict, source: str, known: list | None = None) -> list[dict]:
    """Where a document disagrees with a value the case already carries.

    `current` and `found` map field -> value for the eligibility inputs
    (disputed_amount_cents, provider_type, contracted). The case keeps the
    value it has -- the first one read, or what staff entered -- so a later
    document that says otherwise would vanish without this. Each disagreement
    is returned once: one already in `known` for the same field and document
    value is not repeated. Pure, so it is tested without a database.
    """
    seen = {(k.get("field"), json.dumps(k.get("document_value"), sort_keys=True)) for k in (known or [])}
    out = []
    for field, doc_val in found.items():
        cur = current.get(field)
        if doc_val is None or cur is None or cur == "" or cur == doc_val:
            continue
        key = (field, json.dumps(doc_val, sort_keys=True))
        if key in seen:
            continue
        seen.add(key)
        out.append({"field": field, "case_value": cur, "document_value": doc_val, "document": source})
    return out


def backfill_case_fields(tenant: str, case_id: str, extracted: dict, source: str = "", text: str = "") -> list[str]:
    """Fill a couple of case fields from analyzed-document facts so staff
    don't have to re-type what the document already states -- but only
    fields with an unambiguous semantic match, and only where the case
    doesn't already carry a value (never overwrites staff-entered data).

    qpa_usd (idr_claim schema) IS the case's qpa_cents -- same concept,
    direct match. service_line is inferred from the EOB schema's
    service_lines[0] leading keyword -- a coarse category guess, safe to
    get wrong (falls through to manual, never silently wrong in a way that
    matters). disputed_amount_cents (the figure programmed tenants' own
    eligibility thresholds actually check -- qpa_cents is a federal-NSA-only
    concept) comes from idr_claim's billed_amount_usd: the provider's own
    stated claim amount, literally printed on the same document, same kind
    of direct match as qpa_usd. Deliberately still NOT sourcing it from
    EOB's allowed_amount_usd/patient_responsibility_usd -- those are the
    INSURER's determination and the patient's share, neither of which IS
    the amount a provider is disputing, and unlike billed_amount_usd they'd
    require guessing which side of the payer's math the provider is even
    contesting.

    provider_type/contracted (eligibility-review inputs) land in details
    jsonb, the same bag deriveEligibilityInput (case-api triage.go) already
    reads -- so a case that's had a document analyzed can go straight to
    autoEligibility with no manual form entry, which today NEVER happens any
    other way: nothing else in the platform persists these two fields.
    """
    changed: list[str] = []
    if not isinstance(extracted, dict):
        extracted = {}

    def reading(what: str, fn, default=None):
        """One field's reading. A value in a shape nobody expected costs that
        field, logged -- never the other fields, never the document."""
        try:
            return fn()
        except Exception as exc:  # noqa: BLE001
            print(f"doc-intel: could not read {what} from {source or 'a document'}: "
                  f"{type(exc).__name__}: {exc}", file=sys.stderr, flush=True)
            return default

    # Every value is coerced first: an amount may be 1850, "$1,850.00" or
    # {"amount": 1850}; service lines may be rows, strings or one string.
    qpa_usd = reading("the QPA", lambda: as_money(extracted.get("qpa_usd")))
    billed_amount_usd = reading("the billed amount", lambda: as_money(extracted.get("billed_amount_usd")))
    allowed_amount_usd = reading("the allowed amount", lambda: as_money(extracted.get("allowed_amount_usd")))
    service_lines = reading("the service lines", lambda: as_lines(extracted.get("service_lines")), [])
    codes = extracted.get("cpt_hcpcs_codes")

    def read_provider_type():
        pos_match = re.match(r"\s*(\d{1,2})", as_text(extracted.get("place_of_service")))
        ptype = _POS_PROVIDER_TYPE.get(pos_match.group(1).zfill(2)) if pos_match else None
        if not ptype:
            ptype = provider_type_from_text(text)
        if ptype:
            return ptype, ""
        return infer_provider_type(extracted.get("provider_name"), text, [codes, service_lines])
    provider_type, provider_type_basis = reading("the provider type", read_provider_type, (None, ""))
    contracted = reading("the network status",
                         lambda: _contracted_from_network_status(extracted.get("network_status")))

    with psycopg.connect(DSN, autocommit=True) as c:
        row = c.execute(
            f"SELECT qpa_cents, service_line, details, disputed_amount_cents "
            f"FROM tenant_{tenant}.cases WHERE id=%s",
            (case_id,),
        ).fetchone()
        if not row:
            return changed
        cur_qpa, cur_service_line, details, cur_disputed = row
        if not isinstance(details, dict):
            details = {}

        def set_details(values: dict) -> None:
            c.execute(f"UPDATE tenant_{tenant}.cases SET details = details || %s::jsonb WHERE id=%s",
                      (json.dumps(values), case_id))

        def fill_qpa():
            cents = round(qpa_usd * 100) if qpa_usd and not cur_qpa else 0
            if cents > 0:
                c.execute(f"UPDATE tenant_{tenant}.cases SET qpa_cents=%s WHERE id=%s", (cents, case_id))
                changed.append(f"QPA ${cents / 100:,.2f}")

        # billed_amount_usd is the provider's own charge. A claim form prints
        # it; so does an EOB ("billed" / "charges"), which is now read too --
        # so a dispute filed with only an EOB still gets its amount. The
        # plan's allowed amount and the patient's share are never used.
        def fill_disputed():
            cents = round(billed_amount_usd * 100) if billed_amount_usd and not cur_disputed else 0
            if cents > 0:
                c.execute(f"UPDATE tenant_{tenant}.cases SET disputed_amount_cents=%s WHERE id=%s", (cents, case_id))
                changed.append(f"disputed amount ${cents / 100:,.2f}")

        def fill_service_line():
            if cur_service_line:
                return
            service_line = _service_line_from_cpt(codes)
            if not service_line and service_lines:
                service_line = _service_line_from_text(service_lines[0])
            if service_line:
                c.execute(f"UPDATE tenant_{tenant}.cases SET service_line=%s WHERE id=%s", (service_line, case_id))
                changed.append(f"service line {service_line}")

        def fill_provider_type():
            if provider_type and "provider_type" not in details:
                set_details({"provider_type": provider_type,
                             **({"provider_type_basis": "inferred: " + provider_type_basis} if provider_type_basis else {})})
                changed.append(f"provider type {provider_type}" + (f" (INFERRED — {provider_type_basis}; confirm)" if provider_type_basis else ""))

        def fill_contracted():
            if contracted is not None and "contracted" not in details:
                set_details({"contracted": contracted})
                changed.append(f"contracted={contracted}")

        # Report fields the documents already state, so nobody re-types them:
        # out-of-network (the same network-status reading as "contracted"),
        # and the provider's and the plan's names as printed on the claim.
        # Like everything here: only where the case has no value yet.
        def fill_report_fields():
            extra = {}
            if contracted is not None and "out_of_network" not in details:
                extra["out_of_network"] = "No" if contracted else "Yes"
            for key, field in (("provider_org", "provider_name"), ("health_plan", "payer_name")):
                val = reading(field, lambda field=field: as_text(extracted.get(field)), "")
                if val and len(val) <= 200 and key not in details:
                    extra[key] = val
            for key, val in reading("the provider address", lambda: provider_address_from(extracted), {}).items():
                if key not in details:
                    extra[key] = val
            lob = reading("the line of business", lambda: line_of_business_from(extracted.get("line_of_business")))
            if lob and "line_of_business" not in details:
                extra["line_of_business"] = lob
            issue = disputed_issue_from(billed_amount_usd, allowed_amount_usd, extracted.get("denial_codes"))
            if issue and "disputed_issue" not in details:
                extra["disputed_issue"] = issue
            if extra:
                set_details(extra)
                labels = {"out_of_network": "out-of-network", "provider_org": "provider", "health_plan": "health plan",
                          "provider_street": "street", "provider_city": "city", "provider_state": "state",
                          "provider_postal": "postal code", "line_of_business": "line of business",
                          "disputed_issue": "disputed issue"}
                changed.extend(f"{labels[k]} {v}" for k, v in extra.items())

        # How many distinct claims the analyzed documents carry. Kept under
        # its own key and refreshed on every document: it is what the reader
        # FOUND, offered to the Reviewer, who records the filing packet's count.
        def count_claims():
            n_claims = c.execute(
                "SELECT count(DISTINCT nullif(trim(result->'extracted'->>'claim_number'),'')) FROM public.doc_analysis "
                "WHERE tenant=%s AND case_id=%s AND status LIKE 'ANALYZED%%'", (tenant, case_id)).fetchone()[0]
            if n_claims and details.get("claims_detected") != n_claims:
                set_details({"claims_detected": n_claims})

        # A document that contradicts what the case already has is recorded,
        # never applied: the eligibility review reads this list and sends the
        # result for a second look.
        def record_conflicts():
            billed_cents = round(billed_amount_usd * 100) if billed_amount_usd else None
            known = details.get("eligibility_conflicts")
            known = known if isinstance(known, list) else []
            new_conflicts = eligibility_conflicts(
                {"disputed_amount_cents": cur_disputed, "provider_type": details.get("provider_type"),
                 "contracted": details.get("contracted")},
                {"disputed_amount_cents": billed_cents or None, "provider_type": provider_type, "contracted": contracted},
                source or "a document", known,
            )
            if new_conflicts:
                set_details({"eligibility_conflicts": known + new_conflicts})
                for cf in new_conflicts:
                    changed.append(
                        f"CONFLICT on {cf['field'].replace('_', ' ')}: case has {cf['case_value']}, "
                        f"{cf['document']} says {cf['document_value']} (case value kept)")

        # Each field is its own step: one that fails is logged and skipped,
        # and the rest are still filled.
        for what, step in (("QPA", fill_qpa), ("disputed amount", fill_disputed), ("service line", fill_service_line),
                           ("provider type", fill_provider_type), ("network status", fill_contracted),
                           ("report fields", fill_report_fields), ("claim count", count_claims),
                           ("conflicts", record_conflicts)):
            reading(what, step)
    return changed


def persist(evt: dict, ctx: dict) -> None:
    kind, subj_id = subject(evt)
    case_id = subj_id if kind == "case" else None
    application_id = subj_id if kind == "application" else None
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            """INSERT INTO public.doc_analysis (doc_id, tenant, case_id, application_id, status, doc_type, result)
               VALUES (%s,%s,%s,%s,%s,%s,%s)
               ON CONFLICT (doc_id) DO UPDATE SET status=EXCLUDED.status,
                 doc_type=EXCLUDED.doc_type, result=EXCLUDED.result, analyzed_at=now()""",
            (evt["doc_id"], evt["tenant"], case_id, application_id,
             ctx["status"], ctx.get("doc_type", ""),
             json.dumps({
                 "extracted": ctx.get("extracted", {}),
                 "normalized": ctx.get("normalized", {}),
                 "field_confidence": ctx.get("field_confidence", {}),
                 "findings": ctx.get("findings", []),
                 "seal_detected": ctx.get("seal_detected", False),
                 "table_count": len(ctx.get("tables", [])),
                 "schema_used": ctx.get("schema_used"),
                 "scan_quality_poor": ctx.get("scan_quality_poor", False),
                 "ocr_enhanced": ctx.get("ocr_enhanced", False),
                 "ocr_confidence": ctx.get("ocr_confidence"),
                 "ocr_low_confidence": ctx.get("ocr_low_confidence", False),
                 "ocr_pages": ctx.get("ocr_pages", []),
                 "extracted_fields": ctx.get("extracted_fields", {}),
                 "vlm_field_probability": ctx.get("vlm_field_probability", {}),
             })),
        )
    # Best-effort, like the Temporal signal call below it: the Postgres
    # insert just above is the real system of record for analysis status.
    # A search-index outage should never cost the already-persisted result
    # (confirmed live: it did, before this was guarded).
    try:
        search.index(
            index=f"idre-docs-{evt['tenant']}",
            id=evt["doc_id"],
            # stage_index builds the searchable body; fall back to raw fields if
            # the pipeline stopped before indexing (e.g. early ERROR paths).
            body={**ctx.get("index_body", {}), "case_id": case_id, "application_id": application_id}
            if ctx.get("index_body") else {
                "case_id": case_id, "application_id": application_id, "doc_type": ctx.get("doc_type"),
                "text": ctx.get("text", "")[:100000],
                "extracted": ctx.get("extracted", {}),
            },
        )
    except Exception as exc:  # noqa: BLE001 — opensearchpy's exceptions don't share one base worth catching narrowly
        print(f"doc-intel: OpenSearch index failed (non-fatal): {exc}", file=sys.stderr, flush=True)
    if kind != "case":
        return  # case_activities is a case-timeline table; applications have no equivalent here
    # Unified timeline: analysis result appears on the case activity stream
    # (same table case-api writes DOCUMENT_UPLOADED / signals / voice / notes to).
    findings = ctx.get("findings", [])
    summary = f"Document analysis {ctx['status']}: type={ctx.get('doc_type', '?')}"
    if ctx.get("schema_used"):
        summary += f", schema={ctx['schema_used']}"
    if ctx.get("seal_detected"):
        summary += ", seal/stamp detected"
    low_conf = [f for f, c in ctx.get("field_confidence", {}).items() if c == "low"]
    if low_conf:
        summary += f", unverified field(s): {', '.join(low_conf[:5])}"
    if findings:
        summary += f", {len(findings)} finding(s): " + "; ".join(map(str, findings[:3]))
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            """INSERT INTO public.case_activities (tenant, case_id, type, body)
               VALUES (%s,%s,'ANALYSIS_COMPLETE',%s)""",
            (evt["tenant"], case_id, summary[:2000]),
        )
    source = ""
    try:
        with psycopg.connect(DSN, autocommit=True) as c:
            row = c.execute(f"SELECT filename FROM tenant_{evt['tenant']}.documents WHERE id=%s", (evt["doc_id"],)).fetchone()
            source = (row[0] if row and row[0] else "") or ""
    except Exception:
        source = ""  # the name only labels a conflict; never worth failing the analysis for
    # The analysis is already stored. Filling the case from it is a second
    # step: if it fails, the document stays analyzed and the failure is put
    # on the case timeline for a person, instead of marking the document ERROR.
    try:
        changed = backfill_case_fields(evt["tenant"], case_id, ctx.get("extracted", {}), source,
                                      str(ctx.get("text") or ctx.get("markdown") or ""))
    except Exception as exc:  # noqa: BLE001
        print(f"doc-intel: case fields not filled from {evt.get('doc_id')}: {type(exc).__name__}: {exc}",
              file=sys.stderr, flush=True)
        changed = []
        try:
            with psycopg.connect(DSN, autocommit=True) as c:
                c.execute(
                    """INSERT INTO public.case_activities (tenant, case_id, type, body)
                       VALUES (%s,%s,'AUTO_FILL_FAILED',%s)""",
                    (evt["tenant"], case_id,
                     f"The document{' ' + source if source else ''} was read, but its values could not be copied "
                     f"onto the case ({type(exc).__name__}). Enter them from the document's analysis."))
        except Exception:  # noqa: BLE001
            pass
    if changed:
        with psycopg.connect(DSN, autocommit=True) as c:
            c.execute(
                """INSERT INTO public.case_activities (tenant, case_id, type, body)
                   VALUES (%s,%s,'AUTO_FILLED',%s)""",
                (evt["tenant"], case_id,
                 f"Auto-filled from document analysis: {', '.join(changed)} — review before relying on it"),
            )


def process(evt: dict) -> None:
    kind, subj_id = subject(evt)
    if evt.get("sealed"):
        # Sealed offer documents: skip analysis until lawful reveal.
        mark(evt, "SEALED_PENDING_REVEAL")
        return
    if is_blocked(evt, kind, subj_id):
        # Quarantined by a doc.upload program rule — never analyze blocked bytes.
        mark(evt, "BLOCKED")
        return
    raw = fetch_and_decrypt(evt["tenant"], evt["object_key"])
    # Magic-byte gate before any parsing: content the API allowlist can't
    # process (video, archives, unknown binaries — e.g. uploaded before the
    # edge policy existed) is refused cleanly instead of burning OCR/VLM work
    # and landing as an exception-driven ERROR.
    kind_of = sniff_doc_kind(raw, evt.get("content_type", ""))
    if kind_of == "unknown":
        mark(evt, "UNSUPPORTED_TYPE")
        return
    if kind_of == "office":
        # Zip-bomb pre-flight: a hostile .docx that expands past the cap must
        # never reach Docling's parser (worker memory = every tenant's queue).
        safe, reason = ooxml_safe(raw)
        if not safe:
            mark(evt, "UNSUPPORTED_TYPE")
            print(f"doc-intel: refused office container {evt.get('doc_id')}: {reason}",
                  file=sys.stderr, flush=True)
            return
    # Raster pages only for image uploads. PDFs render LAZILY inside the
    # pipeline (ensure_pages) — a born-digital PDF never pays for bitmaps.
    pages, truncated = (bytes_to_pages(raw, evt.get("content_type", ""), kind=kind_of)
                        if kind_of == "image" else ([], False))
    # A page faxed upside down or sideways is turned the right way up before
    # anything reads it: read as it came, its values landed in the wrong
    # fields (a claim number as the provider).
    # Crooked pages are straightened the same way.
    turned, straightened = [], []
    if pages:
        try:
            pages, turned = upright_pages(pages)
            pages, straightened = straighten_pages(pages)
            if turned or straightened:
                raw = image_bytes(pages, raw)
        except Exception as exc:  # noqa: BLE001 -- turning is an aid; reading goes on without it
            print(f"doc-intel: could not check page orientation: {exc}", file=sys.stderr, flush=True)
            turned, straightened = [], []
    ctx = {
        "pages_turned": turned,
        "pages_straightened": straightened,
        "filename": evt.get("object_key", ""),
        "raw_bytes": raw,                                   # Docling parses bytes directly
        "content_type": evt.get("content_type", ""),
        "pages": pages,                                     # for OCR fallback + VLM image
    }
    try:
        ctx = run_pipeline(ctx, case=load_case(evt["tenant"], subj_id) if kind == "case" else None)
    except ModelUnavailable as exc:
        # Not saved half-read: the document stays unread, and the unread-
        # document sweep in case-api sends it again (and tells people if it
        # still cannot be read after several tries).
        print(f"doc-intel: model unavailable for {evt.get('doc_id')}, left for re-reading: {exc}", flush=True)
        return
    truncated = truncated or ctx.get("pages_truncated", False)
    if truncated:
        # Page-capped render: analysis covers the first DOC_INTEL_MAX_PAGES
        # pages; a human must look at the rest.
        ctx.setdefault("findings", []).append(
            {"field": None, "issue": f"Document exceeds page cap — analysis covers first {len(ctx.get('pages', []))} pages only; remainder needs manual review"})
        ctx["status"] = "ANALYZED_WITH_FINDINGS"
        ctx["result_truncated"] = True
    persist(evt, ctx)
    # doc.analyzed program rules (notify/flag/detail side effects, audited).
    try:
        fire_doc_rules(evt, kind, subj_id, ctx)
    except Exception as exc:  # rules must never crash analysis delivery
        print(f"doc-intel: doc.analyzed rule evaluation failed: {exc}",
              file=sys.stderr, flush=True)
    # Signal the workflow waiting on this analysis (case: DOC_ANALYZED: offer-
    # window-adjacent gates; application: DOCS_VERIFIED: the PENDING_DOCS gate).
    if kind == "case":
        url = f"{TEMPORAL_SIGNAL_URL}/v1/tenants/{evt['tenant']}/cases/{subj_id}/signal"
        payload = {"signal": "DOC_ANALYZED",
                   "data": {"doc_id": evt["doc_id"], "doc_type": ctx.get("doc_type"), "status": ctx["status"]}}
    else:
        url = f"{TEMPORAL_SIGNAL_URL}/v1/tenants/{evt['tenant']}/onboarding/applications/{subj_id}/signal"
        clean = ctx["status"] not in ("ERROR",) and not ctx.get("findings")
        payload = {"signal": "DOCS_VERIFIED",
                   "data": {"clean": clean, "findings": ctx.get("findings", [])}}
    try:
        httpx.post(url, json=payload, headers=AUTH_HEADERS, timeout=10)
    except httpx.HTTPError:
        pass  # signal is best-effort; analysis is already persisted


def mark(evt: dict, status: str) -> None:
    kind, subj_id = subject(evt)
    case_id = subj_id if kind == "case" else None
    application_id = subj_id if kind == "application" else None
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            """INSERT INTO public.doc_analysis (doc_id, tenant, case_id, application_id, status, doc_type, result)
               VALUES (%s,%s,%s,%s,%s,'','{}')
               ON CONFLICT (doc_id) DO UPDATE SET status=EXCLUDED.status""",
            (evt["doc_id"], evt["tenant"], case_id, application_id, status),
        )


def process_check(evt: dict) -> None:
    """Physical check intake: vault-sealed image -> MICR/OCR/ICR extraction ->
    results posted back to case-api, which does invoice matching. The image
    never lingers unsealed; extraction detail persists via case-api (audit)."""
    check_id = evt["check_id"]
    try:
        raw = fetch_and_decrypt(evt["tenant"], evt["object_key"])
        result = extract_check(raw)
        resp = httpx.post(
            f"{TEMPORAL_SIGNAL_URL}/v1/tenants/{evt['tenant']}/internal/checks/{check_id}/result",
            json=result.as_dict(),
            headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
            timeout=30,
        )
        resp.raise_for_status()
        print(f"check {check_id}: extracted conf={result.confidence} "
              f"amount={result.amount_cents} match={resp.json().get('match')}", flush=True)
    except Exception as exc:  # noqa: BLE001 — tell case-api extraction failed
        print(f"check {check_id} extraction failed: {exc}", file=sys.stderr, flush=True)
        try:
            httpx.post(
                f"{TEMPORAL_SIGNAL_URL}/v1/tenants/{evt['tenant']}/internal/checks/{check_id}/result",
                json={"confidence": "low", "detail": {"error": str(exc)}},
                headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
                timeout=30,
            )
        except Exception:
            pass


# Two deployments sharing one Kafka broker must not share topics or a consumer
# group, or each consumes the other's events. TOPIC_PREFIX namespaces both; the
# default keeps the original names.
TOPIC_PREFIX = os.environ.get("TOPIC_PREFIX", "idre")
CONSUMER_GROUP = "doc-intel" if TOPIC_PREFIX == "idre" else f"doc-intel-{TOPIC_PREFIX}"


def main() -> None:
    otel_setup.init("idre-doc-intel")
    consumer = Consumer({
        "bootstrap.servers": os.environ.get("KAFKA_BROKERS", "localhost:9092"),
        "group.id": CONSUMER_GROUP,
        # Regex subscriptions only see a newly created topic on a metadata
        # refresh; the 5-minute default delays a tenant's first event that long.
        "topic.metadata.refresh.interval.ms": 30000,
        "auto.offset.reset": "earliest",
        "enable.auto.commit": False,
    })
    consumer.subscribe([f"^{re.escape(TOPIC_PREFIX)}\\..*\\.documents"], on_assign=lambda c, ps: None)

    running = True
    def stop(*_):
        nonlocal running
        running = False
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    # Bounded worker pool: analysis is I/O-heavy (vault fetch, VLM round-
    # trips) plus CPU bursts (OCR). Serial poll->process->commit left the
    # pool idle during every network wait. Documents are processed
    # concurrently but commits stay IN ORDER (head-of-line drain), so an
    # offset is only committed once every preceding message has finished —
    # at-least-once semantics unchanged. DOC_INTEL_WORKERS default 3: the
    # heavy models are process singletons now, so each extra worker costs
    # threads, not model copies.
    workers = int(os.environ.get("DOC_INTEL_WORKERS", "3"))
    pool = ThreadPoolExecutor(max_workers=workers, thread_name_prefix="doc")
    pending = deque()  # (msg, future), FIFO for in-order commit

    # Continuous learning: load ACTIVE extraction synonyms mined from past
    # human corrections (stage_fields applies them immediately), then mine
    # new corrections on an interval. Fail-open: a learning outage must never
    # stop document processing — extraction just runs with the base label set.
    try:
        with psycopg.connect(DSN) as c:
            n = learning.load_active(c)
            if n:
                print(f"doc-intel learning: {n} active label synonyms loaded", flush=True)
        learning.start_learning_loop(lambda: psycopg.connect(DSN), search)
    except Exception as exc:  # noqa: BLE001
        print(f"doc-intel learning: startup skipped: {exc}", flush=True)

    def run(evt: dict) -> None:
        kind = evt.get("type")
        if kind not in ("doc.uploaded", "check.uploaded"):
            return
        with otel_setup.span(f"{kind} process", traceparent=evt.get("traceparent"), consumer=True,
                             **{"idre.tenant": evt.get("tenant"), "idre.doc_id": evt.get("doc_id"),
                                "idre.case_id": evt.get("case_id"), "messaging.system": "kafka"}):
            if kind == "doc.uploaded":
                process(evt)
            else:
                process_check(evt)

    def drain(block: bool = False) -> None:
        while pending and (block or pending[0][1].done()):
            msg, fut = pending[0]
            try:
                fut.result()  # raises only on a bug that escaped process()
            except Exception as exc:  # noqa: BLE001
                # Poison message handling: park on doc_processing_errors,
                # keep consuming. mark() itself must never raise here.
                print(f"doc-intel error: {exc}", file=sys.stderr, flush=True)
                try:
                    mark(unwrap_cloudevent(json.loads(msg.value())), "ERROR")
                except Exception as mark_exc:  # noqa: BLE001
                    print(f"doc-intel error (while marking a previous error): {mark_exc}",
                          file=sys.stderr, flush=True)
            try:
                consumer.commit(msg)
            except Exception as exc:  # noqa: BLE001
                # The partition moved to another consumer while this document
                # was being read. It will be delivered again; the work is
                # idempotent, so losing the commit costs time, not correctness.
                print(f"doc-intel: commit skipped ({exc})", file=sys.stderr, flush=True)
            pending.popleft()
            block = False

    print(f"doc-intel consuming {TOPIC_PREFIX}.*.documents as group {CONSUMER_GROUP} (workers={workers}) ...", flush=True)
    paused = False
    while running:
        drain()
        # Backpressure: don't outrun the pool. The consumer must keep polling
        # while it waits -- a scanned fax takes minutes, and a consumer silent
        # for max.poll.interval.ms is thrown out of the group and its
        # documents handed out again. So pause the partitions and poll on.
        # Re-pause every pass: a rebalance hands back unpaused partitions.
        full = len(pending) >= workers * 2
        if full:
            consumer.pause(consumer.assignment())
            paused = True
        elif paused:
            consumer.resume(consumer.assignment())
            paused = False
        msg = consumer.poll(0.2 if full else 0.5)
        if msg is None or msg.error():
            continue
        try:
            evt = unwrap_cloudevent(json.loads(msg.value()))
            pending.append((msg, pool.submit(run, evt)))
        except Exception as exc:  # noqa: BLE001 — unparseable envelope
            print(f"doc-intel error: {exc}", file=sys.stderr, flush=True)
            consumer.commit(msg)
    # graceful shutdown: finish in-flight work before closing
    while pending:
        drain(block=True)
    pool.shutdown(wait=True)
    consumer.close()


if __name__ == "__main__":
    main()
