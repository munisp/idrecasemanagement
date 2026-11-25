"""Sample TPA app: file and follow IDR disputes through the Python SDK (idre_tpa.py), in a browser.

A starting point for your own integration, not a replacement for the portal. It runs on your computer only.

    (put idre_tpa.py next to this file; download it from the developers page)
    python3 -m venv .venv && source .venv/bin/activate && pip install flask requests
    export IDRE_CLIENT_ID=api-key-…  IDRE_CLIENT_SECRET=…  IDRE_ENVIRONMENT=test
    python app.py
    then open http://127.0.0.1:5000 (IDRE_ENVIRONMENT=production with a production key)

The API key is read from the environment and never shown in or sent to the browser.
"""
from __future__ import annotations

import os
import secrets
import tempfile
from pathlib import Path

from flask import Flask, abort, flash, redirect, render_template, request, url_for
from jinja2 import DictLoader

from idre_tpa import DOCUMENT_SLOTS, Claim, IdreClient, IdreError

app = Flask(__name__)
app.secret_key = os.environ.get("SAMPLE_APP_SECRET") or secrets.token_hex(32)  # signs flash messages only
app.config["MAX_CONTENT_LENGTH"] = 100 * 1024 * 1024
idre = IdreClient.from_env()

SLOT_LABELS = {
    "EOB": "Explanation of benefits (EOB)", "ITEMIZED_BILL": "Itemized bill", "PROOF_OF_COVERAGE": "Proof of coverage",
    "OPEN_NEGOTIATION_NOTICE": "Open negotiation notice", "OFFER_SUBSTANTIATION": "Offer substantiation",
    "IDRE_ATTESTATION": "IDRE attestation",
}


def cents(value: str) -> int:
    """'1,250.00' → 125000. Raises ValueError for anything that isn't an amount."""
    text = (value or "").replace(",", "").replace("$", "").strip()
    whole, _, frac = text.partition(".")
    if not whole.isdigit() or (frac and (not frac.isdigit() or len(frac) > 2)):
        raise ValueError(f"{value!r} isn't an amount")
    return int(whole) * 100 + int((frac + "00")[:2])


@app.template_filter("money")
def money(value) -> str:
    try:
        return f"${int(value) / 100:,.2f}"
    except (TypeError, ValueError):
        return "—"


@app.errorhandler(IdreError)
def refused(error: IdreError):
    message = f"The API refused that ({error.status or 'not reached'}): {error.message}"
    if request.method == "POST":
        # After a form, go back to the page it came from and say why.
        flash(message, "error")
        return redirect(request.referrer or url_for("home"))
    # Loading a page failed (e.g. a wrong or revoked key): explain it here; redirecting would just fail again.
    return render_template("problem.html", message=message), 502 if not error.status else (error.status if error.status >= 400 else 502)


@app.context_processor
def who():
    try:
        return {"tpa": idre.me()["tpa"], "environment": os.environ.get("IDRE_ENVIRONMENT", "test")}
    except IdreError as error:
        return {"tpa": None, "connection_error": error.message, "environment": os.environ.get("IDRE_ENVIRONMENT", "test")}


@app.get("/")
def home():
    return render_template("home.html", board=idre.dashboard(), fees=idre.fees(), requests=idre.document_requests())


@app.get("/providers")
def providers():
    return render_template("providers.html", clients=idre.clients())


@app.post("/providers")
def add_provider():
    f = request.form
    client_id = idre.add_client(f["name"].strip(), contact_email=f["email"].strip())["client_id"]
    profile = idre.set_client_profile(client_id, npi=f["npi"].strip(), tin=f["tin"].strip(), address=f["address"].strip())
    if f.get("processor_ref", "").strip():
        idre.add_payment_method(client_id, f["method"], label=f.get("label", ""), last4=f.get("last4", ""),
                                brand=f.get("brand", ""), processor_ref=f["processor_ref"].strip())
    flash(f"{f['name']} added (NPI {profile.get('npi_status', 'recorded').lower()}).", "ok")
    return redirect(url_for("providers"))


@app.get("/file")
def file_form():
    return render_template("file.html", clients=idre.clients(), slots=SLOT_LABELS)


@app.post("/file")
def file_dispute():
    f = request.form
    try:
        claim = Claim(claim_number=f["claim_number"].strip(), date_of_service=f["date_of_service"],
                      billed_cents=cents(f["billed"]), paid_cents=cents(f["paid"]), disputed_cents=cents(f["disputed"]),
                      type_of_service=f.get("type_of_service") or None)
    except ValueError as error:
        flash(f"Check the amounts: {error}", "error")
        return redirect(url_for("file_form"))
    client_id = f["client_id"]
    merged = request.files.get("merged")
    uploads = {slot: request.files.get(slot) for slot in DOCUMENT_SLOTS}
    package_id = ""
    with tempfile.TemporaryDirectory() as tmp:
        if merged and merged.filename:
            package_id = idre.start_package(client_id, merged=True)["package_id"]
            path = Path(tmp, "merged.pdf"); merged.save(path)
            idre.upload_merged_pdf(package_id, path)
        elif any(u and u.filename for u in uploads.values()):
            package_id = idre.start_package(client_id)["package_id"]
            for slot, upload in uploads.items():
                if upload and upload.filename:
                    path = Path(tmp, f"{slot}{Path(upload.filename).suffix or '.pdf'}"); upload.save(path)
                    idre.upload_document(package_id, slot, path)
    notes = []
    if package_id:
        state = idre.wait_for_package(package_id, timeout=600, interval=5)   # usually a minute or two
        notes = IdreClient.notes_for_reviewer(state)
    filed = idre.file_dispute(client_id, f["plan_name"].strip(), [claim], package_id=package_id,
                              other_party_email=f["plan_email"].strip(), in_network=f.get("in_network", ""),
                              notes=f.get("notes", "").strip())
    flash(f"Filed {filed['case_number']}.", "ok")
    said = {"NEEDS_REVIEW": "will be checked by the reviewer", "REJECTED": "didn't pass the automatic check",
            "MISSING": "wasn't attached"}
    for note in notes:
        flash(f"{SLOT_LABELS.get(note['slot'], note['slot'])} {said.get(note['status'], 'is ' + note['status'].lower())}.", "info")
    return redirect(url_for("dispute", case_id=filed["case_id"]))


@app.get("/disputes/<case_id>")
def dispute(case_id: str):
    return render_template("dispute.html", case_id=case_id, summary=idre.dispute(case_id), fee=idre.dispute_fee(case_id))


@app.post("/disputes/<case_id>/pay")
def pay(case_id: str):
    return redirect(idre.pay_dispute(case_id, method=request.form.get("method", "card"))["checkout_url"])


@app.get("/bulk")
def bulk_form():
    return render_template("bulk.html", batches=idre.bulk_batches())


@app.post("/bulk")
def bulk_file():
    upload = request.files.get("file")
    if not upload or not upload.filename.lower().endswith((".csv", ".zip")):
        flash("Choose a disputes.csv or a ZIP.", "error")
        return redirect(url_for("bulk_form"))
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp, Path(upload.filename).name); upload.save(path)
        batch = idre.file_bulk(path, batch_ref=request.form.get("batch_ref", "").strip() or f"web-{secrets.token_hex(4)}")
    return redirect(url_for("bulk_batch", batch_id=batch["batch_id"]))


@app.get("/bulk/<batch_id>")
def bulk_batch(batch_id: str):
    return render_template("batch.html", batch=idre.bulk_batch(batch_id))


@app.get("/bulk-template.zip")
def bulk_template():
    path = Path(tempfile.gettempdir(), f"idre-bulk-template-{secrets.token_hex(4)}.zip")
    idre.bulk_template(path)
    data = path.read_bytes(); path.unlink(missing_ok=True)
    return app.response_class(data, mimetype="application/zip", headers={"Content-Disposition": "attachment; filename=template.zip"})


@app.get("/requests")
def doc_requests():
    return render_template("requests.html", requests=idre.document_requests())


@app.post("/requests/<request_id>")
def answer_request(request_id: str):
    upload = request.files.get("file")
    if not upload or not upload.filename:
        abort(400)
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp, Path(upload.filename).name); upload.save(path)
        idre.add_request_file(request_id, path)
    idre.submit_request(request_id, note=request.form.get("note", "").strip())
    flash("Sent to the office.", "ok")
    return redirect(url_for("doc_requests"))


# ── Pages ─────────────────────────────────────────────────────────────────────────────────────────────────────

app.jinja_loader = DictLoader({
    "base.html": """<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{% block title %}{% endblock %} · TPA sample app</title>
<style>
  :root { --ink:#14213d; --muted:#5b6577; --line:#d9dee7; --bg:#f6f7f9; --accent:#1d4ed8; --ok:#166534; --bad:#b91c1c; }
  * { box-sizing:border-box } body { margin:0; font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif; color:var(--ink); background:var(--bg) }
  header { background:var(--ink); color:#fff; padding:12px 20px; display:flex; flex-wrap:wrap; gap:8px 20px; align-items:center }
  header strong { font-size:16px } header nav { display:flex; flex-wrap:wrap; gap:14px } header a { color:#fff; text-decoration:none; opacity:.85 } header a:hover { opacity:1 }
  header .who { margin-left:auto; font-size:13px; opacity:.85 }
  main { max-width:1000px; margin:0 auto; padding:20px 16px 48px }
  h1 { font-size:22px; margin:4px 0 16px } h2 { font-size:17px; margin:24px 0 8px }
  .card { background:#fff; border:1px solid var(--line); border-radius:8px; padding:16px; margin-bottom:16px }
  .figures { display:grid; grid-template-columns:repeat(auto-fit,minmax(170px,1fr)); gap:12px }
  .figures div { background:#fff; border:1px solid var(--line); border-radius:8px; padding:12px } .figures b { display:block; font-size:22px }
  table { width:100%; border-collapse:collapse; background:#fff } th,td { text-align:left; padding:8px 10px; border-bottom:1px solid var(--line); vertical-align:top } th { font-size:12px; color:var(--muted); text-transform:uppercase }
  .wrap { overflow-x:auto }
  form .grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(220px,1fr)); gap:12px }
  label { display:flex; flex-direction:column; gap:4px; font-size:13px; font-weight:600 } label small { font-weight:400; color:var(--muted) }
  input,select,textarea { font:inherit; padding:8px 10px; border:1px solid var(--line); border-radius:6px; background:#fff }
  button,.button { font:inherit; font-weight:600; padding:9px 16px; border-radius:6px; border:1px solid var(--ink); background:var(--ink); color:#fff; cursor:pointer; text-decoration:none; display:inline-block }
  .flash { padding:10px 12px; border-radius:6px; margin-bottom:10px; border:1px solid var(--line); background:#fff } .flash.ok { border-color:var(--ok); color:var(--ok) } .flash.error { border-color:var(--bad); color:var(--bad) }
  .muted { color:var(--muted) } code { font-size:13px } .busy { display:none } form.sending .busy { display:inline }
</style></head><body>
<header><strong>TPA sample app</strong>
  <nav><a href="{{ url_for('home') }}">Disputes</a><a href="{{ url_for('file_form') }}">File a dispute</a><a href="{{ url_for('providers') }}">Providers</a><a href="{{ url_for('bulk_form') }}">Bulk filing</a><a href="{{ url_for('doc_requests') }}">Document requests</a></nav>
  <span class="who">{% if tpa %}{{ tpa.name }} · {{ environment }}{% else %}not connected{% endif %}</span></header>
<main>
{% if connection_error %}<div class="flash error">Can't connect with the API key: {{ connection_error }}. Set IDRE_CLIENT_ID, IDRE_CLIENT_SECRET and IDRE_ENVIRONMENT, then restart.</div>{% endif %}
{% for kind, message in get_flashed_messages(with_categories=true) %}<div class="flash {{ kind }}" role="status">{{ message }}</div>{% endfor %}
{% block body %}{% endblock %}
</main>
<script>document.querySelectorAll('form[data-slow]').forEach(f => f.addEventListener('submit', () => { f.classList.add('sending'); f.querySelector('button').disabled = true }))</script>
</body></html>""",

    "problem.html": """{% extends "base.html" %}{% block title %}Problem{% endblock %}{% block body %}
<h1>That page couldn't be loaded</h1><div class="flash error" role="alert">{{ message }}</div>
<p class="muted">If the API key was refused, check IDRE_CLIENT_ID, IDRE_CLIENT_SECRET and IDRE_ENVIRONMENT (a test key only works with
IDRE_ENVIRONMENT=test), then restart the app.</p>{% endblock %}""",

    "home.html": """{% extends "base.html" %}{% block title %}Disputes{% endblock %}{% block body %}
<h1>Your disputes</h1>
<div class="figures">
  <div><span class="muted">Disputes filed</span><b>{{ board.cases|length }}</b></div>
  <div><span class="muted">Fees owed</span><b>{{ fees.amount_cents|money }}</b><span class="muted">{{ fees.unpaid }} unpaid</span></div>
  <div><span class="muted">Document requests open</span><b>{{ requests.open or 0 }}</b></div>
</div>
<h2>All disputes</h2>
{% if board.cases %}<div class="wrap"><table><tr><th>Case</th><th>Provider</th><th>Stage</th><th>Disputed</th><th>Filed</th></tr>
{% for c in board.cases %}<tr><td><a href="{{ url_for('dispute', case_id=c.case_id) }}">{{ c.case_number }}</a></td><td>{{ c.client_name }}</td><td>{{ c.status|replace('_',' ')|lower }}</td><td>{{ c.disputed_amount_cents|money }}</td><td>{{ c.filed }}</td></tr>{% endfor %}
</table></div>{% else %}<div class="card">No disputes yet. <a href="{{ url_for('file_form') }}">File the first one</a>.</div>{% endif %}
{% endblock %}""",

    "providers.html": """{% extends "base.html" %}{% block title %}Providers{% endblock %}{% block body %}
<h1>Providers you file for</h1>
{% if clients %}<div class="wrap"><table><tr><th>Provider</th><th>Contact</th><th>Status</th><th>Disputes</th></tr>
{% for c in clients %}<tr><td>{{ c.party_name }}</td><td>{{ c.contact_email }}</td><td>{{ (c.onboarding_status or c.status)|replace('_',' ')|lower }}</td><td>{{ c.case_count }}</td></tr>{% endfor %}</table></div>
{% else %}<div class="card">No providers yet.</div>{% endif %}
<h2>Add a provider</h2>
<form class="card" method="post"><div class="grid">
  <label>Name<input name="name" required></label><label>Contact email<input name="email" type="email" required></label>
  <label>NPI<input name="npi" required pattern="[0-9]{10}" inputmode="numeric"><small>Checked against the national registry</small></label>
  <label>TIN<input name="tin" required placeholder="59-1234567"></label><label>Address<input name="address" required></label>
</div><h2>Payment method on file <span class="muted">(optional)</span></h2><div class="grid">
  <label>Method<select name="method"><option value="card">Card</option><option value="ach">Bank (ACH)</option></select></label>
  <label>Label<input name="label" placeholder="Visa on file"></label><label>Last 4<input name="last4" pattern="[0-9]{4}" inputmode="numeric"></label>
  <label>Brand<input name="brand" placeholder="visa"></label><label>Processor reference<input name="processor_ref" placeholder="pm_…"><small>Your payment processor's token; card numbers are never sent</small></label>
</div><p><button>Add provider</button></p></form>
{% endblock %}""",

    "file.html": """{% extends "base.html" %}{% block title %}File a dispute{% endblock %}{% block body %}
<h1>File a dispute</h1>
{% if not clients %}<div class="card">Add a provider first: <a href="{{ url_for('providers') }}">Providers</a>.</div>{% else %}
<form method="post" enctype="multipart/form-data" data-slow>
<div class="card"><div class="grid">
  <label>Provider filing<select name="client_id" required>{% for c in clients %}<option value="{{ c.id }}">{{ c.party_name }}</option>{% endfor %}</select></label>
  <label>Health plan<input name="plan_name" required placeholder="Gulf Health Plan"></label>
  <label>Health plan's email<input name="plan_email" type="email" required><small>Where the plan's notices go</small></label>
  <label>In network?<select name="in_network"><option value="">Let the documents say</option><option value="no">No</option><option value="yes">Yes</option></select></label>
</div></div>
<div class="card"><h2 style="margin-top:0">Claim</h2><div class="grid">
  <label>Claim number<input name="claim_number" required></label><label>Date of service<input name="date_of_service" type="date" required></label>
  <label>Type of service<input name="type_of_service" placeholder="Imaging"></label>
  <label>Billed ($)<input name="billed" required inputmode="decimal" placeholder="3000.00"></label>
  <label>Paid ($)<input name="paid" required inputmode="decimal" placeholder="1750.00"></label>
  <label>Disputed ($)<input name="disputed" required inputmode="decimal" placeholder="1250.00"></label>
</div></div>
<div class="card"><h2 style="margin-top:0">Documents <span class="muted">(each is read automatically; a minute or two)</span></h2><div class="grid">
  {% for slot, label in slots.items() %}<label>{{ label }}<input type="file" name="{{ slot }}" accept=".pdf,image/*"></label>{% endfor %}
</div><p class="muted">Or all six in one PDF, split for you:</p><label>Merged PDF<input type="file" name="merged" accept=".pdf"></label></div>
<label class="card">Notes for the reviewer <span class="muted">(optional)</span><textarea name="notes" rows="3"></textarea></label>
<p><button>File dispute</button> <span class="busy muted">Uploading and reading the documents…</span></p>
</form>{% endif %}
{% endblock %}""",

    "dispute.html": """{% extends "base.html" %}{% block title %}{{ summary.case.case_number }}{% endblock %}{% block body %}
<h1>{{ summary.case.case_number }}</h1>
<div class="figures">
  <div><span class="muted">Stage</span><b>{{ summary.case.status|replace('_',' ')|lower }}</b></div>
  <div><span class="muted">Your side</span><b>{{ (summary.side or '')|lower }}</b></div>
  <div><span class="muted">Fee</span><b>{{ (fee.amount_cents or fee.fee_cents or (summary.fees or {}).get('amount_cents'))|money }}</b><span class="muted">{{ (fee.status or '')|lower }}</span></div>
</div>
{% if summary.case.status == 'AWAITING_PAYMENT' %}<form class="card" method="post" action="{{ url_for('pay', case_id=case_id) }}">
  <div style="display:flex;flex-wrap:wrap;gap:10px;align-items:center">The fee is due. Pay by
  <select name="method" aria-label="Pay by"><option value="card">card</option><option value="bank">bank transfer</option></select>
  <button>Open the payment page</button></div></form>{% endif %}
<h2>Claims</h2>
{% if summary.claims %}<div class="wrap"><table><tr><th>Claim</th><th>Service date</th><th>Billed</th><th>Paid</th><th>Disputed</th></tr>
{% for c in summary.claims %}<tr><td>{{ c.claim_number }}</td><td>{{ c.date_of_service }}</td><td>{{ c.billed_cents|money }}</td><td>{{ c.paid_cents|money }}</td><td>{{ c.disputed_cents|money }}</td></tr>{% endfor %}</table></div>
{% else %}<p class="muted">No claim lines recorded.</p>{% endif %}
<h2>Key dates</h2>
{% if summary.dates %}<table>{% for k, v in summary.dates.items() if v %}<tr><td>{{ k|replace('_',' ') }}</td><td>{{ v }}</td></tr>{% endfor %}</table>{% else %}<p class="muted">None yet.</p>{% endif %}
{% endblock %}""",

    "bulk.html": """{% extends "base.html" %}{% block title %}Bulk filing{% endblock %}{% block body %}
<h1>File many disputes at once</h1>
<div class="card">Start from the <a href="{{ url_for('bulk_template') }}">template</a> (disputes.csv plus a folder of documents per dispute). Replace its sample documents and <code>.example</code> addresses with real ones.
<ul><li><b>disputes.csv on its own</b>: every row is filed at once.</li><li><b>A ZIP</b> (disputes.csv + one folder per <code>dispute_ref</code>): each row is filed once its six documents are accepted.</li></ul></div>
<form class="card" method="post" enctype="multipart/form-data" data-slow><div class="grid">
  <label>disputes.csv or ZIP<input type="file" name="file" accept=".csv,.zip" required></label>
  <label>Your reference for this batch<input name="batch_ref" placeholder="2026-11-batch-1"><small>Sending the same reference again never files twice</small></label>
</div><p><button>Upload</button> <span class="busy muted">Uploading…</span></p></form>
{% set rows = batches.batches if batches is mapping and batches.batches is defined else [] %}
{% if rows %}<h2>Earlier batches</h2><div class="wrap"><table><tr><th>Reference</th><th>Rows</th><th>Filed</th><th>Refused</th><th>Waiting on documents</th><th>Unpaid fees</th><th>Uploaded</th></tr>
{% for b in rows %}<tr><td><a href="{{ url_for('bulk_batch', batch_id=b.id) }}">{{ b.batch_ref or b.id[:8] }}</a></td><td>{{ b.rows }}</td><td>{{ b.filed }}</td><td>{{ b.refused }}</td><td>{{ b.waiting }}</td><td>{{ b.unpaid_cents|money }}</td><td>{{ (b.created_at or '')[:16] }}</td></tr>{% endfor %}</table></div>{% endif %}
{% endblock %}""",

    "batch.html": """{% extends "base.html" %}{% block title %}Bulk filing{% endblock %}{% block body %}
<h1>Bulk filing <span class="muted">{{ batch.batch_id[:8] }}</span></h1>
<p><a class="button" href="">Refresh</a></p>
<div class="wrap"><table><tr><th>Row</th><th>Outcome</th><th>Case</th><th>Detail</th></tr>
{% for i in batch['items'] %}<tr><td>{{ i.dispute_ref }}</td><td>{{ i.status|replace('_',' ')|lower }}</td>
<td>{% if i.case_id %}<a href="{{ url_for('dispute', case_id=i.case_id) }}">{{ i.case_number }}</a>{% endif %}</td>
<td>{{ i.error or '' }}{% if i.status == 'WAITING_DOCS' %}{% for d in i.docs or [] if d.status != 'ACCEPTED' %}{{ d.slot }}: {{ d.status|lower }}{% if not loop.last %}, {% endif %}{% endfor %}{% endif %}</td></tr>{% endfor %}
</table></div>
{% endblock %}""",

    "requests.html": """{% extends "base.html" %}{% block title %}Document requests{% endblock %}{% block body %}
<h1>Documents the office asked for</h1>
{% for r in requests.requests %}<div class="card"><b>{{ r.case_number }}</b> · {{ r.reason_label }} · due {{ r.due_date }}{% if r.overdue %} <b style="color:var(--bad)">overdue</b>{% endif %} · {{ r.status|lower }}
<p>{{ r.message }}</p>
{% if r.status == 'OPEN' %}<form method="post" action="{{ url_for('answer_request', request_id=r.id) }}" enctype="multipart/form-data" data-slow><div class="grid">
<label>File<input type="file" name="file" required></label><label>Note <span class="muted">(optional)</span><input name="note"></label></div><p><button>Send to the office</button></p></form>{% endif %}
</div>{% else %}<div class="card">Nothing has been asked for.</div>{% endfor %}
{% endblock %}""",
})

if __name__ == "__main__":
    # 127.0.0.1 only: this sample has no sign-in of its own, so it must not be reachable from other machines.
    app.run(host="127.0.0.1", port=int(os.environ.get("PORT", "5000")), debug=False)
