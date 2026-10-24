"""lettergen service: generates party-facing letters from DOCX templates.

Consumes letter.requested events (Kafka topic idre.<tenant>.letters, produced
via the transactional outbox), then:
  1. fetch the tenant's DOCX template from MinIO (idre-templates/<tenant>/<key>.docx)
  2. merge case fields ({case_number}, {provider_name}, {payer_name}, {qpa},
     {date}, {details.*} — any key in cases.details)
  3. convert to PDF with LibreOffice headless
  4. seal through the vault, store in idre-docs, insert the documents row
     (folder from the template config, scan_status CLEAN — generated content)
  5. open a PENDING qa_reviews row — every outbound letter passes QA (G4)
     before it is attached to correspondence and mailed.

Templates are per-tenant and replaceable at runtime: upload a new DOCX to
idre-templates/<tenant>/<key>.docx and the next letter uses it. No deploys.

Run: python main.py
"""

from __future__ import annotations

import base64
import io
import json
import os
import re
import signal
import subprocess
import sys
import tempfile
from datetime import date

import builtin_letters
import office_letters
import httpx
import otel_setup
import psycopg
from confluent_kafka import Consumer
from docx import Document
from minio import Minio

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
VAULT = os.environ.get("VAULT_URL", "http://localhost:8081")
# Vault enforces X-Internal-Auth on every route when VAULT_INTERNAL_TOKEN is
# set on its side — both sides must agree (compose wires one shared value).
VAULT_TOKEN = os.environ.get("VAULT_INTERNAL_TOKEN", "")
VAULT_HEADERS = {"X-Internal-Auth": VAULT_TOKEN} if VAULT_TOKEN else {}

minio = Minio(
    os.environ.get("MINIO_ENDPOINT", "localhost:9000"),
    access_key=os.environ.get("MINIO_USER", "idre"),
    secret_key=os.environ.get("MINIO_PASSWORD", "idre-secret"),
    secure=False,
)

# Per deployment: idre and idre-prod share one MinIO on the dev cluster.
TEMPLATE_BUCKET = os.environ.get("TEMPLATE_BUCKET", "idre-templates")
DOC_BUCKET = os.environ.get("DOC_BUCKET", "idre-docs")


def features_for(tenant: str) -> dict:
    """Per-tenant feature toggles: any kept in the program settings
    (config->'features', or the NG manifest) overlaid by public.tenant_features,
    where case-api writes them. Absent = ON, matching case-api's gate."""
    flags: dict = {}
    try:
        with psycopg.connect(DSN) as c:
            row = c.execute("SELECT config FROM public.program_rules WHERE tenant=%s", (tenant,)).fetchone()
            cfg = (row[0] if row else None) or {}
            flags.update((cfg.get("manifest") or {}).get("features") or {})
            flags.update(cfg.get("features") or {})
            own = c.execute("SELECT features FROM public.tenant_features WHERE tenant=%s", (tenant,)).fetchone()
            if own and own[0]:
                flags.update(own[0])
    except Exception:
        pass
    return flags


def load_citations(tenant: str, case_id: str) -> dict:
    """Determination-letter auto-citation: the extracted codes, denial date,
    determination amount and rationale the letter relies on, flattened to
    {citation_*} placeholders. Templates that lack them render unchanged —
    templates that include them make the certified letter self-documenting."""
    out: dict = {}
    with psycopg.connect(DSN) as c:
        det = c.execute(
            """SELECT d.amount_cents, d.rationale, d.issued_at, o.party_id
               FROM public.determinations d JOIN public.offers o ON o.id = d.selected_offer_id
               WHERE d.tenant=%s AND d.case_id=%s""",
            (tenant, case_id),
        ).fetchone()
        row = c.execute(
            f"SELECT details FROM tenant_{tenant}.cases WHERE id=%s", (case_id,),
        ).fetchone()
    details = (row[0] if row else {}) or {}
    codes = sorted(set((details.get("cpt_codes") or []) + (details.get("hcpcs_codes") or [])))
    if codes:
        out["citation_codes"] = ", ".join(codes[:20])
    if details.get("diagnosis_codes"):
        out["citation_diagnoses"] = ", ".join(details["diagnosis_codes"][:20])
    if details.get("eob_denial_date"):
        out["citation_denial_date"] = str(details["eob_denial_date"])
    if det:
        out["citation_determination_amount"] = f"${(det[0] or 0) / 100:,.2f}"
        out["citation_rationale"] = det[1] or ""
        out["citation_prevailing_party"] = det[3] or ""
        out["citation_issued_at"] = det[2].date().isoformat() if det[2] else ""
    return out


def load_case(tenant: str, case_id: str) -> dict:
    with psycopg.connect(DSN) as c:
        row = c.execute(
            f"""SELECT case_number, provider_id, payer_id, qpa_cents, details
                FROM tenant_{tenant}.cases WHERE id=%s""",
            (case_id,),
        ).fetchone()
    if not row:
        raise KeyError(f"case {case_id} not found")
    details = row[4] or {}
    fields = {
        "case_number": row[0],
        "provider_name": row[1] or "",
        "payer_name": row[2] or "",
        "qpa": f"${(row[3] or 0) / 100:,.2f}",
        "date": date.today().isoformat(),
    }
    for k, v in details.items():
        fields[f"details.{k}"] = str(v)
    fields.update(office_variables(tenant))
    return fields


def office_variables(tenant: str) -> dict:
    """The tenant's template variables -- {agency_contacts}, {org_name}, {contact_email}... -- so an uploaded DOCX
    template can use the same names the email templates do (program config "letters", overlaid with what the
    office set on the Templates page)."""
    with psycopg.connect(DSN) as c:
        prog = c.execute("SELECT config->'letters' FROM public.program_rules WHERE tenant=%s", (tenant,)).fetchone()
        ls = c.execute("SELECT settings FROM public.letter_settings WHERE tenant=%s", (tenant,)).fetchone()
    d = (prog[0] if prog and prog[0] else {}) or {}
    st = (ls[0] if ls and ls[0] else {}) or {}
    head = st.get("letterhead") or {}
    out = {k: str(v) for k, v in (d.get("variables") or {}).items()}
    out.update({k: str(v) for k, v in (st.get("variables") or {}).items()})
    for k in ("org_name", "org_short", "contact_email", "contact_phone", "program_name"):
        if head.get(k) or d.get(k):
            out[k] = head.get(k) or d.get(k)
    sal = (st.get("agency") or {}).get("email_salutation") or d.get("agency_email_salutation")
    if not sal and out.get("agency_contacts"):
        sal = f"Dear {out['agency_contacts']},"
    if sal:
        out["agency_salutation"] = sal
    return out


def merge_docx(template: bytes, fields: dict) -> bytes:
    """Replace {placeholder} tokens across paragraphs and tables. Runs are
    joined per paragraph before substitution so tokens split across runs by
    Word's spell-checker still merge."""
    with tempfile.NamedTemporaryFile(suffix=".docx", delete=False) as tf:
        tf.write(template)
        path = tf.name
    doc = Document(path)

    def sub(text: str) -> str:
        return re.sub(r"\{([a-z0-9_.]+)\}", lambda m: fields.get(m.group(1), m.group(0)), text, flags=re.I)

    for para in doc.paragraphs:
        merged = sub("".join(r.text for r in para.runs))
        for r in para.runs:
            r.text = ""
        if para.runs:
            para.runs[0].text = merged
        elif merged:
            para.add_run(merged)
    for table in doc.tables:
        for row in table.rows:
            for cell in row.cells:
                for para in cell.paragraphs:
                    merged = sub("".join(r.text for r in para.runs))
                    for r in para.runs:
                        r.text = ""
                    if para.runs:
                        para.runs[0].text = merged
    out = path + ".merged.docx"
    doc.save(out)
    with open(out, "rb") as f:
        return f.read()


def to_pdf(docx_bytes: bytes) -> bytes:
    with tempfile.TemporaryDirectory() as td:
        src = os.path.join(td, "letter.docx")
        with open(src, "wb") as f:
            f.write(docx_bytes)
        subprocess.run(
            ["soffice", "--headless", "--convert-to", "pdf", "--outdir", td, src],
            check=True, capture_output=True, timeout=120,
        )
        with open(os.path.join(td, "letter.pdf"), "rb") as f:
            return f.read()


def vault_seal(tenant: str, key: str, pt: bytes) -> bytes:
    # /docs/seal responds with sealed_b64, not data_b64 -- confirmed against
    # vault-rs's own seal_doc handler and against documents.go's working
    # caller, which already reads sealed_b64. This mismatch meant every
    # letter generation attempt failed here with KeyError('data_b64'),
    # confirmed live: never caught before because nothing ever reached this
    # far until the Kafka connectivity fix above let a request through.
    resp = httpx.post(f"{VAULT}/docs/seal", timeout=60, headers=VAULT_HEADERS, json={
        "tenant": tenant, "key": key, "data_b64": base64.b64encode(pt).decode(),
    })
    resp.raise_for_status()
    return base64.b64decode(resp.json()["sealed_b64"])


def template_config(tenant: str, key: str) -> dict:
    with psycopg.connect(DSN) as c:
        row = c.execute(
            "SELECT config->'letter_templates' FROM public.program_rules WHERE tenant=%s",
            (tenant,),
        ).fetchone()
    for t in (row[0] if row and row[0] else []):
        if t.get("key") == key:
            return t
    raise KeyError(f"no letter template {key} for tenant {tenant}")


PLACEHOLDER_MARK = "PLACEHOLDER TEMPLATE"


def uploaded_template(tenant: str, key: str) -> bytes | None:
    """The tenant's own DOCX for this letter, or None when there is none (or only the placeholder)."""
    try:
        data = minio.get_object(TEMPLATE_BUCKET, f"{tenant}/{key}.docx").read()
    except Exception:  # noqa: BLE001 -- NoSuchKey: not uploaded
        return None
    try:
        first = " ".join(p.text for p in Document(io.BytesIO(data)).paragraphs[:3])
    except Exception:  # noqa: BLE001 -- unreadable upload: let merge_docx report it
        return data
    return None if PLACEHOLDER_MARK in first else data


def docx_preview(docx_bytes: bytes) -> str:
    """The letter's words for the QA queue, laid out as the letter is: paragraphs in order, a blank line between
    paragraphs of running text (an address block or a heading stays on consecutive lines), line breaks kept, and
    each table row on its own line where the table stands (an invoice's amounts live in tables)."""
    from docx.table import Table
    from docx.text.paragraph import Paragraph
    doc = Document(io.BytesIO(docx_bytes))
    blocks: list[tuple[str, str]] = []  # (kind, text): "p" paragraph, "t" table
    for el in doc.element.body.iterchildren():
        tag = el.tag.rsplit("}", 1)[-1]
        if tag == "p":
            text = "\n".join(re.sub(r"[ \t]+", " ", ln).strip() for ln in Paragraph(el, doc).text.splitlines())
            if text.strip():
                blocks.append(("p", text.strip()))
        elif tag == "tbl":
            rows = []
            for row in Table(el, doc).rows:
                seen: list[str] = []
                for cell in row.cells:
                    t = re.sub(r"\s+", " ", cell.text).strip()
                    if t and t not in seen:
                        seen.append(t)
                if seen:
                    rows.append(" | ".join(seen))
            if rows:
                blocks.append(("t", "\n".join(rows)))
    out = ""
    for i, (kind, b) in enumerate(blocks):
        if i:
            pkind, prev = blocks[i - 1]
            # Short lines in a run (date, addressee, "RE:" lines) stay together; running text, a table and the
            # greeting get a blank line around them.
            tight = (kind == pkind == "p" and len(b) <= 60 and len(prev) <= 60 and "\n" not in b
                     and not b.startswith("Dear "))
            out += "\n" if tight else "\n\n"
        out += b
    return out[:20000]


def handle(evt: dict) -> None:
    tenant, case_id, key = evt["tenant"], evt["case_id"], evt["template"]
    requested_by = evt.get("requested_by", "system")
    tpl = template_config(tenant, key)
    fields = load_case(tenant, case_id)
    if key == "determination_letter" and features_for(tenant).get("letter_citation", True):
        fields.update(load_citations(tenant, case_id))

    # An uploaded template wins (replaceable without a deploy); then the office's own Word letter, filled from the
    # case (office_letters); then the letter built in code (builtin_letters).
    template = uploaded_template(tenant, key)
    office = office_letters.build(DSN, tenant, case_id, key) if template is None and office_letters.has_template(key) else None
    if template is not None:
        merged = merge_docx(template, fields)
    elif office is not None:
        merged, kind, extra = office
        fields.update(extra)
    elif builtin_letters.has_builder(key):
        merged, extra = builtin_letters.build(DSN, minio, TEMPLATE_BUCKET, tenant, case_id, key)
        fields.update(extra)
    else:
        raise KeyError(f"no template {tenant}/{key}.docx uploaded and no built-in letter for {key}")
    if office is not None and office[1] == "pdf":  # the office's PDF form goes out as it is
        pdf = merged
        merged = None
    else:
        pdf = to_pdf(merged)

    filename = tpl.get("filename", "{case_number}.pdf").format(**fields)
    object_key = f"{tenant}/{case_id}/{key}-{date.today().isoformat()}.pdf"
    ct = vault_seal(tenant, object_key, pdf)
    minio.put_object(DOC_BUCKET, object_key, data=io.BytesIO(ct), length=len(ct))

    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            f"""INSERT INTO tenant_{tenant}.documents
                  (case_id, object_key, size_bytes, content_type, sealed,
                   uploaded_by, version, scan_status, filename, folder)
                VALUES (%s,%s,%s,'application/pdf',false,%s,1,'CLEAN',%s,%s)""",
            (case_id, object_key, len(pdf), f"lettergen:{requested_by}", filename,
             tpl.get("folder", "CORRESPONDENCE")),
        )
        # Every letter goes through QA before it is mailed (G4).
        body_preview = docx_preview(merged) if merged is not None else "The office's FL Plan Packet (PDF form) for the health plan to complete."
        c.execute(
            """INSERT INTO public.qa_reviews
                 (tenant, case_id, artifact, channel, subject, body, status, drafted_by)
               VALUES (%s,%s,%s,'letter',%s,%s,'PENDING',%s)""",
            (tenant, case_id, key, tpl.get("subject", key).format(**fields),
             body_preview, requested_by),
        )
        c.execute(
            """INSERT INTO public.notifications (tenant, user_sub, type, body, link)
               VALUES (%s,'*','QA_REVIEW',%s,%s)""",
            (tenant, f"Letter {key} generated for case {fields['case_number']} — awaiting QA",
             f"#/qa"),
        )
        c.execute(
            f"""INSERT INTO public.case_activities (tenant, case_id, type, body)
                VALUES (%s,%s,'LETTER_GENERATED',%s)""",
            (tenant, case_id, f"{filename} generated from template {key}, sealed, queued for QA"),
        )
    print(f"[lettergen] {tenant}/{case_id}: {key} -> {filename}", flush=True)


# Two deployments sharing one Kafka broker must not share topics or a consumer
# group, or each consumes the other's events. TOPIC_PREFIX namespaces both; the
# default keeps the original names.
TOPIC_PREFIX = os.environ.get("TOPIC_PREFIX", "idre")
CONSUMER_GROUP = "lettergen" if TOPIC_PREFIX == "idre" else f"lettergen-{TOPIC_PREFIX}"


def main() -> None:
    otel_setup.init("idre-lettergen")
    consumer = Consumer({
        "bootstrap.servers": os.environ.get("KAFKA_BROKERS", "localhost:9092"),
        "group.id": CONSUMER_GROUP,
        # Regex subscriptions only see a newly created topic on a metadata
        # refresh; the 5-minute default delays a tenant's first event that long.
        "topic.metadata.refresh.interval.ms": 30000,
        "auto.offset.reset": "earliest",
        "enable.auto.commit": False,
    })
    consumer.subscribe([f"^{re.escape(TOPIC_PREFIX)}\\..*\\.letters$"])

    running = True
    def stop(*_):
        nonlocal running
        running = False
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)

    print("[lettergen] listening for letter requests", flush=True)
    while running:
        msg = consumer.poll(1.0)
        if msg is None or msg.error():
            continue
        try:
            evt = json.loads(msg.value())
            with otel_setup.span("letter.requested process", traceparent=evt.get("traceparent"), consumer=True,
                                 **{"idre.tenant": evt.get("tenant"), "idre.case_id": evt.get("case_id"),
                                    "idre.template": evt.get("template"), "messaging.system": "kafka"}):
                handle(evt)
            consumer.commit(msg)  # commit only after the letter exists
        except Exception as e:  # noqa: BLE001 — poison event: log, skip once committed offsets move past
            print(f"[lettergen] ERROR {e}", file=sys.stderr, flush=True)
            # Previously silent: a reviewer who clicked "Generate letter" saw
            # it go QUEUED and then nothing, ever, with no error, no retry,
            # no explanation (confirmed live -- a missing template .docx hit
            # exactly this path). Surface the failure the same way a real
            # letter's success is surfaced: a case activity note and a
            # notification, so staff know to re-upload the template or
            # escalate instead of waiting on a letter that will never arrive.
            try:
                tenant, case_id, key = evt.get("tenant"), evt.get("case_id"), evt.get("template")
                if tenant and case_id:
                    with psycopg.connect(DSN, autocommit=True) as c:
                        c.execute(
                            """INSERT INTO public.case_activities (tenant, case_id, type, body)
                               VALUES (%s,%s,'LETTER_GENERATION_FAILED',%s)""",
                            (tenant, case_id, f"Letter {key!r} could not be generated: {e}"
                             if isinstance(e, builtin_letters.LetterDataMissing)
                             else f"Letter template {key!r} failed to generate: {e}"),
                        )
                        c.execute(
                            """INSERT INTO public.notifications (tenant, user_sub, type, body, link)
                               VALUES (%s,'*','LETTER_GENERATION_FAILED',%s,%s)""",
                            (tenant, f"Letter {key} failed to generate on case — {e}", f"#/cases/{case_id}"),
                        )
            except Exception as notify_exc:  # noqa: BLE001 — failure visibility must never mask the original error
                print(f"[lettergen] ERROR could not record failure: {notify_exc}", file=sys.stderr, flush=True)
            consumer.commit(msg)
    consumer.close()


if __name__ == "__main__":
    main()
