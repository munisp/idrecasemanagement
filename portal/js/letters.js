// letters.js — the decision report (what the reviewers write for the Review Rationale, the cover letter and the
// invoices), the case's letters, and the office's letterhead & invoice details.
//
// Each section of the report is written by the people who own it: the physician's review by the clinical
// reviewers, the coding review and the claims tables by the coder, the applicable law by the attorney; the
// Reviewer and the PM may edit all of it. The server says which parts the caller may edit (can_edit) and refuses
// the rest regardless. Letters are built by lettergen from what is saved here and go to the QA queue.
const Letters = (() => {
  const afterRender = (fn) => setTimeout(fn, 0);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const can = (...rs) => Auth.can(...rs);
  const usd = (c) => (c === null || c === undefined || c === "" ? "" : (Number(c) / 100).toLocaleString("en-US", { style: "currency", currency: "USD" }));
  const dollars = (c) => (c === null || c === undefined || c === "" ? "" : (Number(c) / 100).toFixed(2));
  const cents = (v) => { const s = String(v ?? "").replace(/[$,\s]/g, ""); return s === "" || isNaN(+s) ? null : Math.round(parseFloat(s) * 100); };
  const STAFF = ["CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN", "FINANCE"];

  const OUTCOMES = [["", "choose…"], ["OVERTURNED", "Overturned — the award goes to the provider"],
    ["PARTIALLY_OVERTURNED", "Partially overturned"], ["UPHELD", "Upheld — the denial stands"]];

  // [key, label, who writes it, hint, kind]
  const CLAIM_SECTIONS = [
    ["claim_summary", "Claim summary", "Reviewer", "Left blank, the report says: “This case concerns N claims brought by the provider for services rendered to N patients enrolled in the plan.”"],
    ["dispute_issue", "What the dispute is about", "Reviewer", "For example: “This dispute is regarding the appropriate reimbursement amount for the emergent surgical procedures provided to five (5) patients.”"],
    ["provider_position", "Provider’s position", "Reviewer"],
    ["plan_position", "Health Plan’s position", "Reviewer"],
    ["applicable_law", "Applicable law", "Attorney"],
    ["analysis", "Analysis and conclusion", "Reviewer", "Printed at the end of the Rationale, before the reviews."],
    ["physician_review", "Independent Physician Review", "Nurse / Physician"],
    ["coding_review", "Coding Consultant Review", "Coder"],
    ["pricing_method", "How the claims were priced", "Coder", "For example: “Capitol Bridge used FAIR Health (80th percentile) to price all claims per Florida Statute § 641.513(5).”"],
    ["closing_note", "Closing note", "Reviewer", "Optional — printed after “Based on the foregoing…”."],
    ["case_file_summary", "Case File Summary (cover letter)", "Reviewer", "Left blank, the cover letter uses the claim summary."],
    ["standard_of_review", "Standard of review", "Attorney", "Only to change the program’s standard wording."],
  ];
  const IRO_SECTIONS = [
    ["iro_program_title", "Program line", "Reviewer", "Printed under the organization’s name, e.g. the state’s external review program."],
    ["agency_case_no", "Agency case number", "Reviewer"],
    ["iro_number", "IRO number", "Reviewer"],
    ["plan_reference_no", "Plan reference number", "Reviewer"],
    ["review_received_from", "Assignment received from", "Reviewer", "For example: “the Department of Insurance”."],
    ["date_received", "Date received", "Reviewer", "", "date"],
    ["records_received", "Case documentation received", "Reviewer", "", "date"],
    ["decision_date", "Decision date", "Reviewer", "", "date"],
    ["dates_of_service", "Date(s) of service", "Reviewer", "", "line"],
    ["denial_reason", "Denial reason", "Reviewer", "", "line"],
    ["claim_summary", "Claim summary", "Reviewer"],
    ["reviewer_credentials", "Who reviewed it", "Nurse / Physician", "For example: “A Capitol Bridge Medical Oncologist has reviewed this appeal… has more than five years of relevant practice experience.”"],
    ["determination_reason", "Because…", "Nurse / Physician", "Completes “…be overturned because …”.", "line"],
    ["evidence", "Evidence supporting the determination", "Nurse / Physician", "One reference per line."],
    ["professional_review", "Rationale (professional review)", "Nurse / Physician"],
    ["binding_statute", "Binding under", "Attorney", "Completes “This decision is final and binding … pursuant to …”.", "line"],
    ["standard_of_review", "Standard of review", "Attorney", "Only to change the program’s standard wording."],
  ];
  const CLAIM_COLS = [["claim_number", "Claim number"], ["cpt", "CPT"], ["dos", "Date of service"], ["zip", "Zip"],
    ["billed_cents", "Billed", 1], ["ucr_cents", "UCR", 1], ["lesser_cents", "Lesser of UCR & billed", 1],
    ["paid_cents", "Paid", 1], ["reimbursement_cents", "Reimbursement", 1]];
  const DIS_COLS = [["reason", "Denial reason"], ["description", "Description"], ["revenue_code", "Revenue code"],
    ["quantity", "Quantity"], ["amount_cents", "Disallowed", 1]];
  const REVIEWER_LABEL = { attorney: "Attorney", physician: "Physician", "coding consultant": "Coding consultant", nurse: "Nurse", administrative: "Administrative" };

  let state = null; // { id, data, report, dirty }

  // ---- case page sections --------------------------------------------------------------------------------------
  function caseSecHtml(id, cfg) {
    if (!can(...STAFF)) return "";
    afterRender(() => {
      const d = document.getElementById("p-dr-details");
      d?.addEventListener("toggle", () => { if (d.open && !d.dataset.loaded) { d.dataset.loaded = "1"; load(id); } });
    });
    const tpls = (cfg && cfg.letter_templates) || [];
    const label = (t) => (t.filename || t.key).replace(/\s*\{case_number\}|\s*\{invoice_number\}/g, "").replace(/\.pdf$/i, "").replace(/\s+-\s*$/, "");
    return `<details class="prog-sec" id="p-dr-details" data-tab="review"><summary>Decision report</summary>
      <p class="muted">The decision (outcome, amounts, who reviewed it), which the cover letter and the invoices print, and
        the Review Rationale and Letter itself: pasted here, or uploaded as the finished document. Both go to the QA queue.</p>
      <div id="dr-body"><p class="muted">Loading…</p></div></details>
      ${tpls.length ? `<details class="prog-sec" id="p-letters-details" data-tab="mail"><summary>Generate a letter <span class="muted">— most are drafted by their step; use this for anything else</span></summary>
      <p class="muted">Each letter is built from the case in the program’s layout (unless your office uploaded its own
        template for it), filed under Documents and sent to the QA queue before it goes anywhere.</p>
      <table class="tbl" data-nosearch><thead><tr><th>Letter</th><th>Filed under</th><th></th></tr></thead><tbody>
      ${tpls.map((t) => `<tr><td>${esc(label(t))}</td><td>${esc((t.folder || "").toLowerCase())}</td>
        <td><button type="button" class="mini" onclick="Letters.gen('${esc(id)}','${esc(t.key)}')">Generate</button></td></tr>`).join("")}
      </tbody></table></details>` : ""}`;
  }

  async function load(id) {
    const box = document.getElementById("dr-body");
    if (!box) return;
    let data;
    try { data = await Api.letters.report(id); }
    catch (e) { box.innerHTML = `<p class="error">The decision report could not be loaded: ${esc(e.message)}</p>`; return; }
    const rep = data.report || { report_type: "CLAIM_DISPUTE", sections: {}, claims: [], disallowed: [], reviewers: [], section_authors: {} };
    state = { id, data, report: JSON.parse(JSON.stringify(rep)), dirty: false };
    render();
  }

  function render() {
    const box = document.getElementById("dr-body");
    if (!box || !state) return;
    const { data, report: r } = state;
    const ce = data.can_edit || {};
    const c = data.case || {};
    const iro = r.report_type === "IRO";
    const authors = r.section_authors || {};
    const dis = (k) => (ce[k] ? "" : " disabled");
    const by = (k) => (authors[k] ? `<span class="muted"> — last edited by ${esc(authors[k])}</span>` : "");
    const outcome = r.outcome || "";
    const startOutcome = !r.outcome && c.outcome ? `<p class="muted">The recorded case outcome (“${esc(c.case_outcome)}”) suggests <b>${esc(OUTCOMES.find((o) => o[0] === c.outcome)?.[1] || c.outcome)}</b>.</p>` : "";
    const aw = r.awarded_cents ?? null, paid = r.already_paid_cents ?? null;
    box.innerHTML = `
      <div class="dr-grid">
        <label>Report<select data-f="report_type"${dis("decision")}>
          <option value="CLAIM_DISPUTE"${!iro ? " selected" : ""}>Claim dispute (overturned / partially overturned / upheld)</option>
          <option value="IRO"${iro ? " selected" : ""}>External review (IRO) — medical necessity</option></select></label>
        <label>Outcome<select data-f="outcome"${dis("decision")}>${OUTCOMES.map(([v, l]) => `<option value="${v}"${v === outcome ? " selected" : ""}>${esc(l)}</option>`).join("")}</select></label>
        ${iro ? "" : `<label>Amount awarded ($)<input data-f="awarded_cents" inputmode="decimal" value="${esc(dollars(aw))}" placeholder="${esc(dollars(c.awarded_cents))}"${dis("decision")} /></label>
        <label>Already paid to the provider ($)<input data-f="already_paid_cents" inputmode="decimal" value="${esc(dollars(paid))}" placeholder="${esc(dollars(c.already_paid_cents))}"${dis("decision")} /></label>
        <label>Amount owed to the provider ($)<input id="dr-owed" value="${esc(owedText(aw, paid))}" readonly tabindex="-1" title="The amount awarded less what was already paid" /></label>`}
      </div>
      ${startOutcome}
      ${iro ? "" : `<p class="muted" id="dr-balance">${balanceText(aw, paid, c.disputed_cents)}</p>`}
      <fieldset class="dr-reviewers"><legend>Reviewed by (named in the cover letter)</legend>
        ${(data.reviewer_kinds || []).map((k) => `<label><input type="checkbox" data-rev="${esc(k)}"${(r.reviewers || []).includes(k) ? " checked" : ""}${dis("decision")} /> ${esc(REVIEWER_LABEL[k] || k)}</label>`).join("")}
      </fieldset>
      ${reportHtml(r, data, ce)}
      <div class="actions">
        <button type="button" onclick="Letters.save()">Save the decision report</button>
        <span class="muted" id="dr-saved">${r.updated_at ? `Saved ${new Date(r.updated_at).toLocaleString()} by ${esc(r.updated_by || "")}` : "Not saved yet"}</span>
      </div>
      <div class="actions">
        ${mode(r, data) === "paste" ? `<button type="button" class="mini" onclick="Letters.gen('${esc(state.id)}','final_order_rationale', true)">Generate the decision report</button>` : ""}
        ${iro ? "" : `<button type="button" class="mini" onclick="Letters.gen('${esc(state.id)}','cover_letter_final_notice', true)">Generate the cover letter</button>
        <button type="button" class="mini" onclick="Letters.gen('${esc(state.id)}','invoice_health_plan', true)">Generate the health plan invoice</button>
        <button type="button" class="mini" onclick="Letters.gen('${esc(state.id)}','invoice_provider', true)">Generate the provider invoice</button>`}
      </div>`;
    box.oninput = onInput;
    box.onchange = onInput;
  }

  // The report itself: pasted whole, or the finished document uploaded. (The sectioned form it replaced is still
  // read by lettergen for reports written that way.)
  const mode = (r, data) => (r.sections || {}).report_mode || (data.report_file ? "upload" : "paste");
  function reportHtml(r, data, ce) {
    const m = mode(r, data), dis = ce.full_report ? "" : " disabled", f = data.report_file;
    const text = (r.sections || {}).full_report || "";
    return `<fieldset class="dr-mode"><legend>The report</legend>
        <label><input type="radio" name="dr-mode" value="paste"${m === "paste" ? " checked" : ""}${dis} onchange="Letters.setMode('paste')" /> Paste the report</label>
        <label><input type="radio" name="dr-mode" value="upload"${m === "upload" ? " checked" : ""}${dis} onchange="Letters.setMode('upload')" /> Upload the finished report</label>
      </fieldset>
      ${m === "paste" ? `<label class="dr-sec dr-full"><span><b>Review Rationale and Letter</b> <span class="chip">Reviewer</span>${(r.section_authors || {}).full_report ? `<span class="muted"> — last edited by ${esc(r.section_authors.full_report)}</span>` : ""}</span>
          <textarea data-sec="full_report" rows="${text.length > 2000 ? 24 : 14}" maxlength="200000"${dis} placeholder="Paste the whole report as it should read: the summary, the rationale, the reviews and the claims.">${esc(text)}</textarea>
          <span class="muted dr-hint">Blank lines start new paragraphs. “Generate the decision report” prints it on the letterhead, files it under Documents and sends it to the QA gate.</span></label>`
        : `<div class="dr-upload">
          ${f ? `<p>Uploaded: <b>${esc(f.filename)}</b> <span class="muted">· ${new Date(f.uploaded_at).toLocaleString()}</span></p>` : `<p class="muted">No report uploaded yet.</p>`}
          ${ce.full_report ? `<label class="dr-sec"><span><b>${f ? "Replace it" : "The report"}</b> <span class="muted">— PDF or Word</span></span>
            <input type="file" id="dr-file" accept=".pdf,.doc,.docx,application/pdf,application/vnd.openxmlformats-officedocument.wordprocessingml.document,application/msword" /></label>
            <button type="button" class="mini" onclick="Letters.upload(this)">Upload the report</button>
            <span class="muted dr-hint">It is filed under Documents as the Review Rationale and Letter and goes to the QA gate.</span>` : ""}
        </div>`}`;
  }
  function setMode(m) {
    if (!state) return;
    state.report.sections = { ...(state.report.sections || {}), report_mode: m };
    markDirty("sections");
    render();
  }
  async function upload(btn) {
    const file = document.getElementById("dr-file")?.files?.[0];
    if (!file) { UI.toast("Choose the report file first", { kind: "warn" }); return; }
    if (state.dirty && !(await save(true))) return;
    await UI.run(btn, async () => {
      try {
        const up = await Api.cases.upload(state.id, file, false, "DETERMINATION");
        await Api.letters.attachReport(state.id, up.doc_id || up.id);
        UI.toast("Decision report uploaded — it is under Documents and in the QA queue");
        await load(state.id);
      } catch (e) { UI.toast(`The report could not be uploaded: ${e.message}`, { kind: "warn", duration: 9000 }); }
    }, "Uploading…");
  }

  // The amount owed: the award less what the plan already paid (never below zero).
  const owedCents = (aw, paid) => (aw == null ? null : Math.max(aw - (paid || 0), 0));
  const owedText = (aw, paid) => (aw == null ? "" : dollars(owedCents(aw, paid)));
  function balanceText(aw, paid, disputed) {
    if (aw == null) return "Enter the amount awarded; the amount owed is the award less what was already paid.";
    return `Amount owed to the provider: <b>${usd(owedCents(aw, paid))}</b>${disputed ? ` · of the ${usd(disputed)} in dispute` : ""}.`;
  }

  function gridHtml(name, cols, rows, editable) {
    const cell = (row, [k, , money], i) => {
      const v = money ? dollars(row[k]) : (row[k] ?? "");
      return `<td><input data-grid="${name}" data-row="${i}" data-col="${k}" value="${esc(v)}"${money ? ' inputmode="decimal" class="num"' : ""}${editable ? "" : " disabled"} /></td>`;
    };
    return `<div class="tbl-scroll"><table class="tbl dr-tbl" data-nosearch><thead><tr>${cols.map(([, l]) => `<th>${esc(l)}</th>`).join("")}${editable ? "<th></th>" : ""}</tr></thead>
      <tbody>${rows.map((row, i) => `<tr>${cols.map((c) => cell(row, c, i)).join("")}${editable ? `<td><button type="button" class="mini" title="Remove this row" onclick="Letters.delRow('${name}',${i})">×</button></td>` : ""}</tr>`).join("")
        || `<tr><td colspan="${cols.length + 1}" class="muted">No rows.</td></tr>`}</tbody></table></div>
      ${editable ? `<button type="button" class="mini" onclick="Letters.addRow('${name}')">Add a row</button>` : ""}`;
  }

  function onInput(ev) {
    if (!state) return;
    const el = ev.target, r = state.report;
    if (el.dataset.sec) { r.sections = r.sections || {}; r.sections[el.dataset.sec] = el.value; markDirty("sections"); return; }
    if (el.dataset.rev !== undefined) {
      const set = new Set(r.reviewers || []);
      el.checked ? set.add(el.dataset.rev) : set.delete(el.dataset.rev);
      r.reviewers = (state.data.reviewer_kinds || []).filter((k) => set.has(k));
      markDirty("reviewers"); return;
    }
    if (el.dataset.grid) {
      const rows = r[el.dataset.grid], row = rows[+el.dataset.row], col = el.dataset.col;
      row[col] = col.endsWith("_cents") ? cents(el.value) : el.value;
      // Lesser of UCR and billed fills itself until someone types it.
      if (el.dataset.grid === "claims" && (col === "billed_cents" || col === "ucr_cents") && row.billed_cents != null && row.ucr_cents != null && !row._lesserTyped) {
        row.lesser_cents = Math.min(row.billed_cents, row.ucr_cents);
        const lesser = document.querySelector(`[data-grid="claims"][data-row="${el.dataset.row}"][data-col="lesser_cents"]`);
        if (lesser) lesser.value = dollars(row.lesser_cents);
      }
      if (col === "lesser_cents") row._lesserTyped = true;
      markDirty(el.dataset.grid); return;
    }
    const f = el.dataset.f;
    if (!f) return;
    if (f === "report_type") { r.report_type = el.value; markDirty(f); render(); return; }
    if (f === "outcome") r.outcome = el.value || null;
    else r[f] = cents(el.value);
    markDirty(f);
    const b = document.getElementById("dr-balance");
    if (b) b.innerHTML = balanceText(r.awarded_cents, r.already_paid_cents, state.data.case?.disputed_cents);
    const o = document.getElementById("dr-owed");
    if (o) o.value = owedText(r.awarded_cents, r.already_paid_cents);
  }

  function markDirty(part) {
    state.dirty = true;
    state.changed = state.changed || new Set();
    state.changed.add(part);
    const s = document.getElementById("dr-saved");
    if (s) s.textContent = "Unsaved changes";
  }

  function addRow(name) { state.report[name] = [...(state.report[name] || []), {}]; markDirty(name); render(); }
  function delRow(name, i) { state.report[name].splice(i, 1); markDirty(name); render(); }
  function fromCase() {
    state.report.claims = (state.data.case.claims || []).map((c) => ({ claim_number: c.claim_number, cpt: c.cpt, dos: c.dos, billed_cents: c.billed_cents, paid_cents: c.paid_cents }));
    markDirty("claims"); render();
  }

  // The parts that changed and that the caller may write: the server refuses anything else.
  function body() {
    const ce = state.data.can_edit || {}, r = state.report, ch = state.changed || new Set(), out = {};
    for (const f of ["report_type", "outcome", "awarded_cents", "already_paid_cents", "reviewers"])
      if (ch.has(f) && ce.decision) out[f] = r[f] ?? null;
    if (ch.has("sections")) {
      const secs = {};
      for (const [k, v] of Object.entries(r.sections || {})) if (ce[k]) secs[k] = v;
      if (Object.keys(secs).length) out.sections = secs;
    }
    const clean = (rows) => (rows || []).filter((x) => Object.entries(x).some(([k, v]) => !k.startsWith("_") && v !== null && v !== "")).map((x) => Object.fromEntries(Object.entries(x).filter(([k]) => !k.startsWith("_"))));
    if (ch.has("claims") && ce.claims) out.claims = clean(r.claims);
    if (ch.has("disallowed") && ce.claims) out.disallowed = clean(r.disallowed);
    return out;
  }

  async function save(quiet) {
    if (!state) return true;
    const b = body();
    if (!Object.keys(b).length) { if (!quiet) UI.toast("Nothing to save"); return true; }
    try {
      await Api.letters.saveReport(state.id, b);
      state.dirty = false; state.changed = new Set();
      if (!quiet) UI.toast("Decision report saved");
      await load(state.id);
      return true;
    } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); return false; }
  }

  async function gen(id, key, fromReport) {
    if (fromReport && state && state.dirty && !(await save(true))) return;
    // A partially accepted case: first which of its claims are ineligible, and why (the letter names both).
    if (key === "partial_acceptance_letter" && !(await partialClaims(id))) return;
    try {
      await Api.cases.lettergen(id, key);
      UI.toast("Generating — the letter appears under Documents and in the QA queue in a few seconds. If something it needs is missing, the case’s activity says what.", { duration: 8000 });
    } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
  }

  async function partialClaims(id) {
    let claims = [];
    try { const r = await Api.cases.claims(id); claims = r.claims || r || []; } catch (e) { UI.toast(e.message, { kind: "warn" }); return false; }
    const nums = claims.map((c) => c.claim_number).filter(Boolean);
    if (nums.length < 2) { UI.toast("A partial acceptance needs at least two claims on the case: one accepted, one not.", { kind: "warn", duration: 8000 }); return false; }
    const marked = claims.filter((c) => c.status === "INELIGIBLE").map((c) => c.claim_number);
    const v = await UI.modal({ title: "Partial acceptance", submitLabel: "Save and generate",
      body: `Claims on this case: <b>${nums.map(esc).join(", ")}</b>. Name the ones that are not eligible; the rest are accepted for review.`,
      fields: [{ name: "ineligible", label: "Ineligible claims (claim numbers, separated by commas)", required: true, value: marked.join(", ") },
        { name: "reason", label: "Why they are ineligible", type: "textarea", required: true,
          value: esc((claims.find((c) => c.status === "INELIGIBLE") || {}).reason || ""),
          hint: "Goes in the letter after the claim numbers, e.g. “were filed more than 12 months after the final determination.”" }] });
    if (!v) return false;
    try {
      await Api.cases.claimEligibility(id, { ineligible: v.ineligible.split(/[,;\s]+/).filter(Boolean), reason: v.reason.trim() });
      return true;
    } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); return false; }
  }

  // ---- letterhead & invoice details (Billing and Templates pages) --------------------------------------------
  const LS_LABELS = {
    letterhead: ["Letterhead — also fills {org_name}, {org_short}, {program_name}, {contact_email} and {contact_phone} in every email template", { org_name: "Organization name", org_short: "Short name (signature)", program_name: "Program name in emails", program_title: "Program line on reports", contractor_line: "Line under the logo on invoices", state: "State", contact_email: "Contact email", contact_phone: "Contact phone" }],
    agency: ["The Agency's contacts", { email_salutation: "How emails to the Agency open — {agency_salutation}", salutation: "Cover letter salutation", line1: "Cover letter address line 1", line2: "Address line 2", line3: "Address line 3", line4: "Address line 4" }],
    invoice: ["Invoice notes", { payment_note: "Payment note (bold)", bank_changed_notice: "Bank change notice (red)", account_use_note: "Account use note (red)" }],
    remit: ["Remit to (bank details)", { payee: "Payee", bank: "Bank", routing: "Routing number", account: "Account number", remittance_email: "Remittance advice email" }],
  };

  function settingsCardHtml() {
    if (!can("PM", "FINANCE", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) return "";
    afterRender(loadSettings);
    return `<details class="card" id="ls-card"><summary><b>Letterhead &amp; invoice</b> <span class="muted">— what generated letters and invoices print</span></summary>
      <div id="ls-body"><p class="muted">Loading…</p></div></details>`;
  }

  async function loadSettings() {
    const box = document.getElementById("ls-body");
    if (!box) return;
    let d;
    try { d = await Api.letters.settings(); }
    catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; return; }
    const def = d.defaults || {}, set = d.settings || {};
    const defFor = (g, k) => (g === "letterhead" ? def[k] : g === "agency" ? (k === "email_salutation" ? def.agency_email_salutation
      : k === "salutation" ? def.agency_addressee?.salutation : def.agency_addressee?.lines?.[+k.slice(4) - 1])
      : g === "invoice" ? def.invoice?.[k] : "") || "";
    const groups = Object.keys(LS_LABELS).map((g) => {
      const [title, labels] = LS_LABELS[g];
      const ro = g === "remit" ? !d.can_edit_remit : !d.can_edit;
      return `<fieldset class="ls-group" data-group="${g}"><legend>${esc(title)}${g === "remit" ? ` <span class="muted">— Finance or an administrator changes these${d.can_edit_remit ? "" : "; numbers are masked"}</span>` : ""}</legend>
        ${(d.fields[g] || []).map((k) => `<label>${esc(labels[k] || k)}<input name="${k}" value="${esc(set[g]?.[k] || "")}" placeholder="${esc(defFor(g, k))}"${ro ? " disabled" : ""}${k === "routing" || k === "account" ? ' inputmode="numeric" autocomplete="off"' : ""} /></label>`).join("")}
      </fieldset>`;
    }).join("");
    box.innerHTML = `<p class="muted">Blank fields use the program’s standard wording, shown greyed out. Invoices are not generated until the bank, routing and account numbers are set.</p>
      <form id="ls-form" class="ls-form" onsubmit="event.preventDefault(); Letters.saveSettings(this)">${groups}
      ${d.can_edit ? `<div class="actions"><button>Save letterhead &amp; invoice details</button></div>` : ""}</form>
      ${d.updated_by ? `<p class="muted">Last changed by ${esc(d.updated_by)}, ${new Date(d.updated_at).toLocaleString()}.</p>` : ""}`;
  }

  async function saveSettings(form) {
    const out = {};
    form.querySelectorAll("fieldset[data-group]").forEach((fs) => {
      const inputs = [...fs.querySelectorAll("input:not([disabled])")];
      if (!inputs.length) return;
      out[fs.dataset.group] = Object.fromEntries(inputs.map((i) => [i.name, i.value.trim()]));
    });
    try { await Api.letters.saveSettings(out); UI.toast("Letterhead & invoice details saved"); loadSettings(); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
  }

  // ---- template variables (Templates page) --------------------------------------------------------------------
  // Named values set once for the whole tenant and used as {name} in any email template or uploaded letter:
  // the office's own (organization name, contact details, the Agency's salutation) and any the office adds,
  // like {agency_contacts} = "Mr. Hoeler and Ms. Rooks".
  let tvData = null;
  function variablesCardHtml() {
    if (!can(...STAFF)) return "";
    afterRender(loadVariables);
    return `<details class="card" id="tv-card" open><summary><b>Template variables</b> <span class="muted">— names and values every template can use</span></summary>
      <div id="tv-body"><p class="muted">Loading…</p></div></details>`;
  }
  async function loadVariables() {
    const box = document.getElementById("tv-body");
    if (!box) return;
    try { tvData = await Api.letters.variables(); }
    catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; return; }
    tvData.rows = (tvData.variables || []).map((v) => ({ ...v }));
    tvRender();
  }
  function tvRender() {
    const box = document.getElementById("tv-body");
    if (!box || !tvData) return;
    const ed = tvData.can_edit, dis = ed ? "" : " disabled";
    box.innerHTML = `<p class="muted">Write <span class="mono">{name}</span> in any template and it is replaced by the value here when the
        email or letter is drafted. One change here applies to every case.${ed ? "" : " The PM or an administrator changes these."}</p>
      <h4>Your variables</h4>
      <table class="tbl" data-nosearch><thead><tr><th>In a template</th><th>Value</th><th></th></tr></thead><tbody>
      ${tvData.rows.map((v, i) => `<tr>
        <td>${v.standard ? `<span class="mono">{${esc(v.name)}}</span>` : `<input class="mono" data-tv="${i}" data-k="name" value="${esc(v.name)}" placeholder="name_in_lowercase" maxlength="41"${dis} />`}</td>
        <td><input data-tv="${i}" data-k="value" value="${esc(v.value)}" maxlength="2000"${dis} />
          ${v.standard && v.value !== v.standard_value ? `<span class="muted dr-hint">Standard: ${esc(v.standard_value)}</span>` : ""}</td>
        <td>${ed ? (v.standard ? `<button type="button" class="mini" title="Back to the standard value" onclick="Letters.tvReset(${i})">Reset</button>`
          : `<button type="button" class="mini" title="Remove this variable" onclick="Letters.tvDel(${i})">×</button>`) : ""}</td></tr>`).join("")
        || `<tr><td colspan="3" class="muted">None yet.</td></tr>`}</tbody></table>
      ${ed ? `<button type="button" class="mini" onclick="Letters.tvAdd()">Add a variable</button>` : ""}
      <h4>Your office</h4>
      <table class="tbl" data-nosearch><thead><tr><th>In a template</th><th>What it is</th><th>Value</th></tr></thead><tbody>
      ${(tvData.builtin || []).map((b) => `<tr><td class="mono">{${esc(b.name)}}</td><td>${esc(b.meaning)}</td>
        <td><input data-tvb="${esc(b.name)}" value="${b.set ? esc(b.value) : ""}" placeholder="${esc(b.value)}" maxlength="300"${dis} /></td></tr>`).join("")}
      </tbody></table>
      <p class="muted">An office value left blank uses the standard wording shown greyed out.</p>
      ${ed ? `<div class="actions"><button type="button" onclick="Letters.tvSave()">Save variables</button></div>` : ""}`;
    box.oninput = (ev) => {
      const el = ev.target;
      if (el.dataset.tv !== undefined) tvData.rows[+el.dataset.tv][el.dataset.k] = el.value;
    };
  }
  function tvAdd() { tvData.rows.push({ name: "", value: "" }); tvRender(); }
  function tvDel(i) { tvData.rows.splice(i, 1); tvRender(); }
  function tvReset(i) { const v = tvData.rows[i]; v.value = v.standard_value; tvRender(); }
  async function tvSave() {
    const variables = {};
    for (const v of tvData.rows) {
      const name = (v.name || "").trim();
      if (!name && !(v.value || "").trim()) continue;
      variables[name] = v.value;
    }
    const builtin = {};
    document.querySelectorAll("#tv-body [data-tvb]").forEach((el) => { builtin[el.dataset.tvb] = el.value; });
    try { await Api.letters.saveVariables({ builtin, variables }); UI.toast("Template variables saved"); loadVariables(); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
  }

  return { caseSecHtml, gen, save, setMode, upload, addRow, delRow, fromCase, settingsCardHtml, saveSettings, variablesCardHtml, tvAdd, tvDel, tvReset, tvSave };
})();
