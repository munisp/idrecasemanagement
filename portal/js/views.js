// views.js — one render function per screen; role-aware actions.
// Meridian upgrade: statutory-clock projections, L4 peek panel, bulk bar,
// density toggle, modal+toast everywhere (no prompt/alert), sealed-offer flow.
const Views = (() => {
  const $ = (sel) => document.querySelector(sel);
  // Bind after the router injects innerHTML: queueMicrotask races ahead of the
  // `view.innerHTML = await fn()` continuation (microtask order) and bound null;
  // a macrotask runs strictly after it. Every form/handler binding MUST use this.
  const afterRender = (fn) => setTimeout(fn, 0);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmtDate = (d) => (d ? new Date(d).toLocaleString("en-US", { dateStyle: "medium", timeStyle: "short" }) : "—");
  const badge = (s) => `<span class="badge s-${esc(s).toLowerCase().replace(/_/g, "-")}">${esc(s)}</span>`;
  const err = (e) => `<div class="err-box"><b>Something didn't load.</b> ${esc(e.message)} <button class="mini" onclick="App.rerender()">Retry</button></div>`;
  const role = (r) => (Auth.claims()?.roles || []).includes(r);
  const can = (...rs) => Auth.can(...rs);
  // Federal NSA cases price on QPA (qpa_cents); programmed tenants (FL AHCA
  // etc.) have no such concept and always read $0 there -- they use
  // disputed_amount_cents instead (accumulates from imported claims, or a
  // one-time figure set when a case is opened via the generic New Dispute
  // form). Every QPA-labeled amount in the grid/dashboard was unconditional
  // before this -- confirmed live, FL showed "QPA $0" on every open case.
  const amtField = (prog) => (prog && prog.config) ? "disputed_amount_cents" : "qpa_cents";
  const amtLabel = (prog) => (prog && prog.config) ? "Disputed" : "QPA";
  // Programmed tenants (FL AHCA) track progress on internal_status, not the
  // federal status column -- summaryReport (reports.go) already coalesces
  // to internal_status server-side; the dashboard's client-side rollup
  // below grouped/filtered on the raw status column unconditionally,
  // which for FL cases is mostly a stale/empty value set once at intake.
  // Confirmed live: this is why the home dashboard's open-case count never
  // matched the Reports page's for FL.
  const caseStatus = (prog, c) => (prog && prog.config) ? (c.internal_status || c.status) : c.status;
  // "Closed" is a single federal status prefix, but FL's internal_status
  // vocabulary has no such prefix at all (its terminal labels are things
  // like "Dismissed"/"Ineligible"/"Decided - Invoice Paid") -- a
  // startsWith("CLOSED") check against internal_status matches nothing,
  // so every FL case always counted as open. agency_status='Closed' is
  // the one consistent marker the backend actually sets across every
  // terminal internal_status (confirmed against live data).
  const caseClosed = (prog, c) => (prog && prog.config) ? c.agency_status === "Closed" : String(c.status).startsWith("CLOSED");

  // Client-side CSV export: reports were read-only on screen with no way to
  // get the data out at all. Same blob+temp-<a> pattern api.js's download()
  // already uses for real files; here the "file" is generated from data
  // already on the page, so no backend round-trip is needed.
  function downloadCSV(filename, headers, rows) {
    const cell = (v) => { const s = String(v ?? ""); return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s; };
    const text = [headers, ...rows].map((r) => r.map(cell).join(",")).join("\n");
    const url = URL.createObjectURL(new Blob([text], { type: "text/csv;charset=utf-8;" }));
    const a = document.createElement("a");
    a.href = url; a.download = filename;
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  }

  // ---- Client-side pagination --------------------------------------------------
  // Lists the backend returns in one shot are rendered whole; the shared
  // Pager (ui.js) pages every table on the page by itself -- rows-per-page
  // choice plus Previous / Next -- so there is nothing to wire per view.
  // These two remain so the views that used the old 25-row pager read the same.
  function initClientPager(id, allRows, rowsHtml) {
    return { bodyHtml: rowsHtml(allRows), pagerHtml: "" };
  }
  function bindClientPager() {}

  // ---- SOP fields: one owner role each ----------------------------------------
  const SOP_DATE_LABELS = {
    date_case_accepted: "Date Case Accepted",
    date_acknowledgment_letter_sent_to_provider: "Date Acknowledgment Letter Sent to Provider",
    date_acknowledgment_letter_sent_to_health_plan: "Date Acknowledgment Letter Sent to Health Plan",
    date_provider_responded: "Date Provider Responded",
    date_provider_response_due: "Date Provider's Response is Due",
    estimate_sent_at: "Cost estimate email sent",
    rfi_sent_at: "RFI sent",
    withdrawal_requested_at: "Withdrawal requested",
    date_health_plan_response_due: "Date Health Plan's Response is Due",
    date_plan_responded: "Date Plan Responded",
    date_recommendation_sent_to_agency: "Date the Recommendation is Sent to the Agency",
    date_final_order_sent: "Date Final Order Sent",
  };
  const SOP_NOTE_FIELDS = [
    ["internal_case_review", "Internal Case Review notes", ["CASE_MANAGER", "PM"], "Reviewer / PM"],
    ["coder_review", "Coder Review notes", ["CODER"], "Coder"],
    ["clinical_review", "Clinical Review notes", ["NURSE_PHYSICIAN", "DOCTOR", "NURSE"], "Nurse / Physician"],
    ["legal_review", "Legal Review notes", ["ATTORNEY"], "Attorney"],
    ["external", "Comments/Notes (External for Florida)", ["CASE_MANAGER", "PM"], "Reviewer / PM"],
  ];
  const sopStamp = (x) => (x && x.by ? `<span class="muted"> — ${esc(x.by)}, ${fmtDate(x.at)}</span>` : "");
  function sopNotesHtml(id, c) {
    const rn = (c.details && c.details.review_notes) || {};
    // Each person sees only the field(s) their role writes; admins see all.
    const fields = SOP_NOTE_FIELDS.filter(([, , roles]) => can(...roles, "FEDERAL_ADMIN", "PLATFORM_ADMIN"));
    if (!fields.length) return "";
    return `<details class="prog-sec"><summary>Review notes</summary>` +
      fields.map(([f, label]) => {
        const n = rn[f] || {};
        return `<form class="form" onsubmit="event.preventDefault(); Views.saveReviewNote('${id}','${f}',this)">
          <label><b>${esc(label)}</b>${sopStamp(n)}
            <textarea name="text" rows="3">${esc(n.text || "")}</textarea></label>
          <button class="mini">Save</button></form>`;
      }).join("") + `</details>`;
  }
  function sopSummaryHtml(id, c, cfg) {
    const ds = (c.details && c.details.dispute_summary) || {};
    const mine = can("CODER", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const ro = mine ? "" : "disabled";
    const matrix = (cfg.estimate && cfg.estimate.cost_matrix) || [];
    const est = ds.full_review_estimate_cents != null ? (ds.full_review_estimate_cents / 100).toFixed(2) : "";
    return `<details class="prog-sec" id="p-summary-details" data-wf="summary" data-wf-roles="CODER"><summary>Dispute Summary (Coder)</summary>
      <p class="muted">Completed by the Coder: the plan type as verified from the plan's letterhead, the summary of the dispute,
        and — when the filer asked for one — the full review cost estimate from the Estimated Cost Matrix.${sopStamp(ds)}</p>
      <form class="form" onsubmit="event.preventDefault(); Views.saveDisputeSummary('${id}',this)">
        <div class="inline-form">
          <input name="plan_type" placeholder="health plan type" value="${esc(ds.plan_type || "")}" ${ro} />
          <label>verified from the plan's letterhead <input type="checkbox" name="verified" ${ds.plan_type_verified_from_letterhead ? "checked" : ""} ${ro} /></label>
          <label>coding review required <input type="checkbox" name="coding" ${ds.coding_review_required ? "checked" : ""} ${ro} /></label>
        </div>
        <label>Summary <textarea name="summary" rows="3" ${mine ? "" : "readonly"}>${esc(ds.summary || "")}</textarea></label>
        <div class="inline-form">
          ${matrix.length ? `<select name="matrix" ${ro} onchange="if(this.value){const [c,l]=this.value.split('|');this.form.estimate.value=(c/100).toFixed(2);this.form.basis.value=l;}">
            <option value="">Estimated Cost Matrix…</option>
            ${matrix.map((m) => `<option value="${esc(m.cents)}|${esc(m.label)}">${esc(m.label)} — ${usdC(m.cents)}</option>`).join("")}</select>` : ""}
          <input name="estimate" type="number" step="0.01" min="0" placeholder="full review cost estimate $" value="${est}" ${ro} />
          <input name="basis" placeholder="matrix row / basis for the estimate" value="${esc(ds.estimate_basis || "")}" ${ro} />
        </div>
        ${mine ? `<button class="mini">Save Dispute Summary</button>` : ""}</form></details>`;
  }
  // Documents that disagree with what the case carries: the case value is
  // kept, the disagreement is listed here and sends the review for a second look.
  function sopConflictsHtml(c) {
    const cs = (c.details && c.details.eligibility_conflicts) || [];
    if (!cs.length) return "";
    const show = (f, v) => (f === "disputed_amount_cents" ? usdC(v) : String(v));
    return `<div class="err-box"><b>⚠ Documents disagree with the case.</b> The case value was kept — check which is right before deciding.
      <ul>${cs.map((x) => `<li>${esc(String(x.field).replace(/_cents$/, "").replace(/_/g, " "))}: case has <b>${esc(show(x.field, x.case_value))}</b>,
        ${esc(x.document || "a document")} says <b>${esc(show(x.field, x.document_value))}</b></li>`).join("")}</ul></div>`;
  }
  // Case information: the fields the Reviewer completes after payment
  // (Provider & Health Plan Info), which the monthly report to Florida reads.
  function sopCaseInfoHtml(id, c, cfg) {
    const d = c.details || {}, fs = cfg.field_schema || {};
    const mine = can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const ro = mine ? "" : "disabled";
    const sel = (name, label, list) => `<label>${label} <select name="${name}" ${ro}><option value="">—</option>
      ${(list || []).map((x) => `<option ${d[name] === x ? "selected" : ""}>${esc(x)}</option>`).join("")}</select></label>`;
    const inp = (name, label, extra = "") => `<label>${label} <input name="${name}" value="${esc(d[name] ?? "")}" ${extra} ${ro} /></label>`;
    const missing = ["line_of_business", "disputed_issue", "provider_street", "provider_postal"].filter((k) => !d[k]).length;
    return `<details class="prog-sec"><summary>Case information${missing && mine ? ` — ${missing} report field(s) not filled` : ""}</summary>
      <p class="muted">Completed by the Reviewer. These go on the monthly report to Florida; the provider's name and the health plan come from the linked accounts unless typed here.</p>
      <form class="form" onsubmit="event.preventDefault(); Views.saveCaseInfo('${id}', this)">
        <div class="inline-form">
          ${sel("line_of_business", "Line of business", fs.line_of_business)}
          ${sel("disputed_issue", "Disputed issue", fs.disputed_issue)}
          ${sel("out_of_network", "Out-of-network provider", fs.out_of_network || ["Yes", "No"])}
          ${inp("claims_submitted", "Claims in the filing packet", 'type="number" min="0" style="width:7em"')}
          ${d.claims_detected && !d.claims_submitted ? `<span class="muted">the documents show ${esc(d.claims_detected)} claim(s)</span>` : ""}
        </div>
        <div class="inline-form">
          ${inp("provider_org", "Provider organization")}${inp("provider_contact", "Provider contact")}${inp("health_plan", "Health plan")}
        </div>
        <div class="inline-form">
          ${inp("provider_street", "Street address", 'style="min-width:18em"')}${inp("provider_city", "City")}
          ${inp("provider_state", "State", 'maxlength="2" style="width:4em" placeholder="FL"')}${inp("provider_postal", "Postal code", 'style="width:8em" placeholder="12345"')}
        </div>
        ${mine ? `<button class="mini">Save case information</button>` : ""}</form></details>`;
  }
  async function saveCaseInfo(id, form) {
    const body = {};
    for (const k of ["line_of_business", "disputed_issue", "out_of_network", "provider_org", "provider_contact", "health_plan",
      "provider_street", "provider_city", "provider_state", "provider_postal"]) {
      const v = form[k].value.trim();
      if (v) body[k] = v;
    }
    if (form.claims_submitted.value !== "") body.claims_submitted = parseInt(form.claims_submitted.value, 10) || 0;
    if (!Object.keys(body).length) { UI.toast("Nothing to save", { kind: "info" }); return; }
    try { await Api.cases.setDetails(id, body); UI.toast("Case information saved"); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }

  // Fraud, waste and abuse: anyone on the case can raise it; the PM assigns
  // an independent reviewer and records the outcome.
  const FWA_LABEL = { REPORTED: "Reported — awaiting the PM", UNDER_REVIEW: "With the independent reviewer",
    REFERRED_TO_AGENCY: "Referred to the agency", NOT_SUBSTANTIATED: "Reviewed — not substantiated" };
  function sopFwaHtml(id, c) {
    const f = (c.details && c.details.fwa) || {};
    const isPM = can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const open = f.status === "REPORTED" || f.status === "UNDER_REVIEW";
    let body = f.status ? `<p><b>${esc(FWA_LABEL[f.status] || f.status)}</b></p>
      <p class="muted">Reason: ${esc(f.reason || "—")} — ${esc(f.reported_by || "")}, ${fmtDate(f.reported_at)}
      ${f.reviewer ? `<br/>Independent reviewer: <b>${esc(f.reviewer)}</b> (assigned by ${esc(f.assigned_by || "")})` : ""}
      ${f.summary ? `<br/>Findings: ${esc(f.summary)} — ${esc(f.concluded_by || "")}, ${fmtDate(f.concluded_at)}` : ""}</p>` : "";
    if (!open) body += `<form class="inline-form" onsubmit="event.preventDefault(); Views.fwaAct('${id}','report',this)">
      <input name="reason" required placeholder="why you suspect fraud, waste or abuse" style="flex:1;min-width:260px" />
      <button class="mini">Report to the PM</button></form>`;
    if (isPM && open) body += `<form class="inline-form" onsubmit="event.preventDefault(); Views.fwaAct('${id}','assign',this)">
      <input name="reviewer" required placeholder="independent reviewer (medical expertise)" value="${esc(f.reviewer || "")}" />
      <button class="mini">${f.reviewer ? "Change reviewer" : "Assign reviewer"}</button></form>`;
    if (isPM && f.status === "UNDER_REVIEW") body += `<form class="inline-form" onsubmit="event.preventDefault(); Views.fwaAct('${id}','conclude',this)">
      <select name="agree" required><option value="">outcome…</option><option value="true">Reviewer agrees — refer to the agency</option><option value="false">Not substantiated</option></select>
      <input name="summary" required placeholder="findings and summary" style="flex:1;min-width:260px" /><button class="mini">Record outcome</button></form>`;
    return `<details class="prog-sec" ${open ? "open" : ""}><summary>Fraud, waste or abuse${open ? " — review open" : ""}</summary>
      <p class="muted">Any staff member who suspects fraud, waste or abuse records the reason here; it goes to the PM, who assigns an independent reviewer. Disputes of $1,000,000 or more are raised automatically.</p>${body}</details>`;
  }
  async function fwaAct(id, action, form) {
    const body = { action };
    if (action === "report") body.reason = form.reason.value;
    if (action === "assign") body.reviewer = form.reviewer.value;
    if (action === "conclude") { body.agree = form.agree.value === "true"; body.summary = form.summary.value; }
    try { await Api.program.fwa(id, body); UI.toast("Recorded"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }
  function sopAorHtml(id, c) {
    const a = c.details && c.details.aor_confirmation;
    const shown = a ? `<p>Attorney confirmation: <b>AOR ${a.valid ? "valid" : "invalid"}</b> — ${esc(a.reason)}${sopStamp(a)}</p>` : "";
    if (!can("ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) return shown;
    return shown + `<form class="inline-form" onsubmit="event.preventDefault(); Views.confirmAor('${id}',this)">
      <b>Attorney: confirm AOR</b>
      <select name="valid" required><option value="">ruling…</option><option value="false">AOR is invalid</option><option value="true">AOR is valid</option></select>
      <input name="reason" placeholder="reason" required style="flex:1;min-width:220px" /><button class="mini">Record</button></form>`;
  }
  async function saveReviewNote(id, field, form) {
    try { await Api.program.setReviewNote(id, field, form.text.value); UI.toast("Notes saved"); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }
  async function saveDisputeSummary(id, form) {
    const body = { plan_type: form.plan_type.value, plan_type_verified_from_letterhead: form.verified.checked,
      coding_review_required: form.coding.checked, summary: form.summary.value, estimate_basis: form.basis.value };
    if (form.estimate.value !== "") body.full_review_estimate_cents = Math.round(Number(form.estimate.value) * 100);
    try { await Api.program.setDisputeSummary(id, body); UI.toast("Dispute Summary saved"); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }
  async function confirmAor(id, form) {
    try { await Api.program.confirmAor(id, form.valid.value === "true", form.reason.value); UI.toast("AOR confirmation recorded"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }
  async function eligChecklist(id) {
    try { await Api.program.eligibilityChecklist(id); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }

  // ---- Workflow steps: one next step, owned by one role -------------------------
  // The server derives the SOP's steps from the case and says which ONE is
  // next and whether this user owns it (GET .../workflow). This renders that
  // list and makes the rest of the panel agree with it: sections that serve a
  // step are greyed out until that step is next, and hidden from roles that
  // never perform it. The server refuses out-of-order actions regardless.
  const WF_MARK = { done: "✓", skipped: "–", next: "▶", upcoming: "○" };
  const WF_STAGE_LETTERS = ["acceptance", "ineligible", "estimate_cost", "plan_notification", "final_order", "final_order_rationale_copy"];
  const WF_BUTTON = (a) => (a.startsWith("signal:") ? "Record it" : a.startsWith("letter:") ? "Draft it" : a.startsWith("qa:") ? "Open the QA gate" : a.startsWith("send:") ? "Send it"
    : a === "eligibility" ? "Run the review" : a === "summary" ? "Open the Dispute Summary" : a === "aor_confirm" ? "Give the ruling"
    : a === "invoice" ? "Create the invoice" : a === "settle" ? "Open Financials" : a === "outcome" ? "Record it" : "Open");
  async function wfLoad(id, cfg) {
    const box = document.getElementById("wf-steps");
    if (!box) return;
    let r;
    try { r = await Api.program.workflow(id); }
    catch (e) { box.innerHTML = `<p class="muted">⚠ Steps could not be loaded: ${esc(e.message)}</p>`; return; }
    if (!r.programmed) { box.remove(); return; }
    const steps = r.steps || [];
    const next = steps.find((x) => x.state === "next");
    window._wf = { id, cfg, steps, next, isAdmin: !!r.is_admin };
    box.innerHTML = `<h3>Steps</h3>
      <p class="muted">${next ? `Only the next step can be acted on, by the ${esc(next.owner)} (or an admin). Everything else is greyed out until its turn.`
        : "No open step — nothing further is due on this case."}</p>
      <ol class="wf">${steps.map((x) => `<li class="wf-${x.state}">
        <span class="wf-mark" aria-hidden="true">${WF_MARK[x.state] || ""}</span>
        <span class="wf-label">${esc(x.label)}</span> <span class="chip">${esc(x.owner)}</span>
        ${x.state === "next" ? (x.can_act
          ? `<button type="button" class="mini" onclick="Views.wfAct('${esc(x.key)}')">${WF_BUTTON(x.action)}</button>`
          : `<span class="muted">waiting on the ${esc(x.owner)}</span>`) : ""}
        ${x.state === "next" && x.hint ? `<div class="muted wf-hint">${esc(x.hint)}</div>` : ""}</li>`).join("")}</ol>`;
    wfApplyLocks();
  }
  function wfApplyLocks() {
    const { next, isAdmin } = window._wf || {};
    const mine = (action) => !!(next && next.can_act && next.action === action);
    document.querySelectorAll("[data-wf-admin]").forEach((el) => { if (!isAdmin) el.remove(); });
    document.querySelectorAll("[data-wf]").forEach((el) => {
      const roles = (el.dataset.wfRoles || "").split(" ").filter(Boolean);
      if (el.dataset.wfHide !== undefined && roles.length && !can(...roles, "FEDERAL_ADMIN", "PLATFORM_ADMIN")) { el.remove(); return; }
      const open = el.dataset.wf.split(" ").some(mine);
      el.classList.toggle("wf-locked", !open);
      const inner = el.tagName === "DETAILS" ? [...el.children].filter((c) => c.tagName !== "SUMMARY") : [el];
      inner.forEach((c) => { c.inert = !open; });
      if (el.tagName === "DETAILS") {
        const sum = el.querySelector("summary");
        sum.querySelector(".wf-note")?.remove();
        if (!open) sum.insertAdjacentHTML("beforeend", ` <span class="muted wf-note">— not the current step</span>`);
        else el.open = true;
      }
    });
    // Stage letters can only be drafted on their own step.
    const sel = document.querySelector('#p-send select[name="template"]');
    if (sel) {
      [...sel.options].forEach((o) => {
        if (!WF_STAGE_LETTERS.includes(o.value)) return;
        o.disabled = !mine("letter:" + o.value);
        o.textContent = o.textContent.replace(/ — not yet$/, "") + (o.disabled ? " — not yet" : "");
      });
      if (sel.selectedOptions[0]?.disabled) {
        const first = [...sel.options].find((o) => !o.disabled);
        if (first) { sel.value = first.value; corrTemplateBody(sel); }
      }
    }
  }
  async function wfSend(id, signal, data, label) {
    try { await Api.cases.signal(id, signal, data || {}); UI.toast(label || "Recorded"); setTimeout(() => App.rerender(), 1200); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
  }
  const WF_YESNO = [["", "choose…"], ["yes", "Yes"], ["no", "No"]];
  async function wfAct(key) {
    const { id, cfg, steps } = window._wf || {};
    const st = (steps || []).find((x) => x.key === key);
    if (!st || st.state !== "next" || !st.can_act) return;
    const a = st.action;
    const open = (sel) => { const el = document.querySelector(sel); if (el) { el.open = true; el.scrollIntoView({ behavior: "smooth", block: "start" }); } };
    if (a.startsWith("letter:")) {
      const tpl = a.slice(7), sel = document.querySelector('#p-send select[name="template"]');
      if (sel) { sel.value = tpl; corrTemplateBody(sel); }
      return open("#p-send-details");
    }
    if (a.startsWith("qa:") || a.startsWith("send:")) { location.hash = "#/qa"; return; }
    if (a === "eligibility" || a === "aor_confirm") return open("#p-elig-details");
    if (a === "summary") return open("#p-summary-details");
    if (a === "invoice") return open("#p-inv-details");
    if (a === "settle") { location.hash = "#/finance"; return; }
    if (a === "outcome") {
      const fs = (cfg && cfg.field_schema) || {};
      const opts = (xs) => [["", "choose…"]].concat((xs || []).map((x) => [x, x]));
      const v = await UI.modal({ title: "Record the outcome", submitLabel: "Record", wide: true,
        body: "These go on the reports to Florida. Each party's review cost comes from its invoice.",
        fields: [{ name: "case_outcome", label: "Case outcome", required: true, options: opts(fs.case_outcome) },
          { name: "party_billed", label: "Party billed", required: true, options: opts(fs.party_billed) },
          { name: "amount", label: "Final amount awarded (USD)", type: "number", step: "0.01" },
          { name: "num_claims", label: "Number of claims reviewed", type: "number", hint: "May differ from the number submitted." }] });
      if (!v) return;
      const body = { case_outcome: v.case_outcome, party_billed: v.party_billed };
      if (v.amount) body.final_amount_awarded_cents = Math.round(parseFloat(v.amount) * 100);
      if (v.num_claims) body.num_claims_reviewed = parseInt(v.num_claims, 10);
      try { await Api.program.recordOutcome(id, body); UI.toast("Outcome recorded"); setTimeout(() => App.rerender(), 800); }
      catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
      return;
    }
    if (!a.startsWith("signal:")) return;
    const sig = a.slice(7);
    if (sig === "PACKET_COMPLETE") {
      const v = await UI.modal({ title: "Filing packet complete", body: "Payment and documents have both arrived.", submitLabel: "Confirm",
        fields: [{ name: "est", label: "Did the filer tick “estimate requested” in the packet?", required: true, options: WF_YESNO },
          { name: "claims", label: "Number of claims in the filing packet", type: "number", required: true,
            hint: "Reported to Florida as “Number of Claims Submitted with Initial Filing”." }] });
      if (v) wfSend(id, sig, { estimate_requested: v.est === "yes", claims_submitted: parseInt(v.claims, 10) || 0 }, "Packet recorded as complete");
    } else if (sig === "ESTIMATE_PERMISSION") {
      const v = await UI.modal({ title: "Provider's permission to proceed", submitLabel: "Record",
        fields: [{ name: "g", label: "Did the provider give permission to proceed with the full review?", required: true, options: WF_YESNO }] });
      if (v) wfSend(id, sig, { granted: v.g === "yes" }, "Permission recorded");
    } else if (sig === "CODING_REVIEW_COMPLETE") {
      const v = await UI.modal({ title: "Coding review complete", submitLabel: "Complete",
        fields: [{ name: "c", label: "Is clinical review by the Nurse / Physician needed?", required: true, options: WF_YESNO }] });
      if (v) wfSend(id, sig, { clinical_requested: v.c === "yes" }, "Coding review recorded");
    } else if (await UI.confirm(st.label, "Record this step as done?", "Record")) {
      wfSend(id, sig, {}, "Recorded");
    }
  }
  async function wfAnytime(id, signal) {
    const what = { PROVIDER_WITHDRAW: "The provider asked to withdraw. The case will close when the Withdrawal letter has been sent through QA.",
      RFI_RESPONSE_RECEIVED: "The response to the request for additional documentation has arrived.",
      OUTREACH_DONE: "Payment arrived but the packet did not, and you have contacted the filer. This starts the 7-day refund window." }[signal];
    if (await UI.confirm("Record this?", what, "Record", signal === "PROVIDER_WITHDRAW")) wfSend(id, signal, {}, "Recorded");
  }

  // ---- Statutory clock helpers -------------------------------------------------
  const clockChip = (c) => {
    const txt = c.remaining >= 0 ? `${c.remaining} ${c.basis === "business" ? "bd" : "cd"} left` : `Breached ${Math.abs(c.remaining)}d ago`;
    return `<span class="sla ${c.state}" title="${esc(c.label)} · ${esc(c.cite)} · basis: ${esc(c.basis_note)}">◷ ${txt}</span>`;
  };
  const nearestClock = (clocks) => clocks && clocks.length
    ? clocks.reduce((a, b) => (a.remaining < b.remaining ? a : b)) : null;

  async function clockMap() {
    try {
      const all = await Api.cm.allClocks();
      return Object.fromEntries(all.map((x) => [x.case_id, x.clocks]));
    } catch { return {}; }
  }

  // ---- Dashboard -----------------------------------------------------------
  async function dashboard() {
    // TPA's home IS the TPA dashboard -- the generic rollup below is built
    // entirely on Api.cases.list(), the whole tenant's cases, which TPA is
    // deliberately blocked from (access-control follow-up); it has its own
    // correctly-scoped "Tracking" card on this same page instead.
    if (can("TPA")) return tpaView();
    const me = Auth.claims();
    let html = `<div class="view-head"><h1>Good day, ${esc((me.name || "").split(/[.\s]/)[0] || me.name)}</h1>
      <span class="muted">tenant <b>${esc(Api.getTenant()).toUpperCase()}</b> · ${me.roles.map(esc).join(", ")}</span></div>`;
    try {
      const [{ cases }, clocks, prog] = await Promise.all([Api.cases.list({ limit: 200 }), clockMap(), Api.program.get().catch(() => null)]);
      if (prog?.tenant_label) {
        html = `<div class="view-head"><h1>Good day, ${esc((me.name || "").split(/[.\s]/)[0] || me.name)}</h1>
          <span class="muted">tenant <b>${esc(prog.tenant_label)}</b> · ${me.roles.map(esc).join(", ")}</span></div>`;
      }
      // Status rollup computed client-side from the case list the dashboard
      // already fetched, not Api.reports.summary() -- that endpoint is
      // gated to CASE_MANAGER/PM/FEDERAL_ADMIN/STATE_AUDITOR/PLATFORM_ADMIN
      // (it matches the Reports page's own nav gate), but the home
      // dashboard has to render for every real staff role, ARBITRATOR/
      // CODER/NURSE_PHYSICIAN/ATTORNEY/FINANCE included. Promise.all was
      // rejecting whole on that one 403, breaking the entire dashboard for
      // those roles -- confirmed live. Approximate beyond the first 200
      // cases (the limit this call already used); the authoritative,
      // unlimited rollup stays on the Reports page.
      const amtF = amtField(prog), amtL = amtLabel(prog);
      const byStatus = {};
      cases.forEach((c) => {
        const st = caseStatus(prog, c);
        const b = byStatus[st] || (byStatus[st] = { status: st, count: 0, _sum: 0 });
        b.count++; b._sum += (c[amtF] || 0) / 100;
      });
      const summary = Object.values(byStatus).map((b) => ({
        status: b.status, count: b.count, avg_amount_usd: b.count ? b._sum / b.count : 0, amount_label: amtL,
      }));
      html += `<div class="cards">` + summary.map((s) =>
        `<div class="card"><div class="num">${s.count}</div><div class="lbl">${badge(s.status)}</div>
         <div class="muted">avg ${esc(s.amount_label || "QPA")} $${(s.avg_amount_usd ?? 0).toFixed(0)}</div></div>`).join("") + `</div>`;
      const open = cases.filter((c) => !caseClosed(prog, c));
      const attention = open.filter((c) => clocks[c.id] && clocks[c.id].length)
        .sort((a, b) => nearestClock(clocks[a.id]).remaining - nearestClock(clocks[b.id].remaining));
      html += `<h2>Needs your attention (${attention.length})</h2>`;
      html += attention.length
        ? `<div class="panel">` + attention.slice(0, 8).map((c) => {
            const cl = nearestClock(clocks[c.id]);
            return `<div class="att-item" onclick="location.hash='#/cases/${c.id}'">
              <span class="att-id">${esc(c.case_number)}</span>
              <span class="att-title">${esc(c.service_line)} · ${amtLabel(prog)} $${((c[amtField(prog)] || 0) / 100).toLocaleString()}</span>
              ${badge(c.status)} ${clockChip(cl)}</div>`;
          }).join("") + `</div>`
        : `<p class="muted">Nothing needs you right now. New assignments and deadline risk appear here.</p>`;
      const { bodyHtml, pagerHtml } = initClientPager("od", open, (list) => caseTable(list, clocks, "", prog));
      html += `<h2>Open disputes (${open.length})</h2><div id="od-tb">${bodyHtml}</div>${pagerHtml}`;
      return { html, wire: () => { bindGrid(open); bindClientPager("od", "#od-tb", () => bindGrid(open)); } };
    } catch (e) { html += err(e); return html; }
  }

  // ---- Disputes grid: clocks, selection, peek, density ------------------------
  let selection = new Set();

  // Server-driven column sort, stored in the hash so it survives refresh and
  // is shareable: '#/cases?sort=-qpa_cents' (prefix '-' = descending).
  const SORTABLE = new Set(["case_number", "status", "service_line", "qpa_cents", "opened_at", "sla"]);
  // Lever 2/3/4: triage lane chip, signed SLA badge, batch group headers.
  const laneChip = (lane) => lane === "AUTO_REVIEW" ? `<span class="chip lane-auto" title="Everything needed for a one-click confirm is present">AUTO</span>`
    : lane === "COMPLEX" ? `<span class="chip lane-complex" title="Batched, duplicated, escalated, or high-dollar">COMPLEX</span>`
    : `<span class="chip lane-std">STANDARD</span>`;
  // A frozen case (awaiting payment, or found ineligible) allows only
  // correspondence -- the API enforces that (cmd/server/caselock.go); this
  // disables the controls so they are not offered in the first place.
  // Sections load at different moments, so it keeps watching the page.
  const FROZEN_OK_SECTIONS = ["correspondence", "call log", "inquiries log"];
  const FROZEN_OK_HANDLERS = /download|showAnalysis|peek|letter\(|mailLog|Filter\(|sort|App\.rerender|location\.hash/;
  function freezeCasePage(lock) {
    const view = document.getElementById("view");
    if (!view) return;
    const marker = view.querySelector(".case-lock");
    const allowed = (el) => {
      if (el.closest(".case-lock, .tabs, .pager, .view-head")) return true;
      const sec = el.closest("details.prog-sec");
      if (sec) {
        const title = (sec.querySelector("summary")?.textContent || "").trim().toLowerCase();
        if (FROZEN_OK_SECTIONS.includes(title)) return true;
        if (lock === "INELIGIBLE" && title === "eligibility review") return true;
      }
      if (el.type === "search" || el.matches("summary, details")) return true;
      return FROZEN_OK_HANDLERS.test(el.getAttribute("onclick") || "") || FROZEN_OK_HANDLERS.test(el.getAttribute("onchange") || "");
    };
    const sweep = () => {
      if (!marker.isConnected) { obs.disconnect(); return; }
      view.querySelectorAll("button, input, select, textarea").forEach((el) => {
        if (el.dataset.frozen || allowed(el)) return;
        el.dataset.frozen = "1"; el.disabled = true;
        el.title = "Not available while this case is frozen — only correspondence is allowed";
      });
    };
    const obs = new MutationObserver(sweep);
    obs.observe(view, { childList: true, subtree: true });
    sweep();
  }

  // Plain-language labels for the program's ineligibility reason codes (the
  // SOP's exclusion list). An unlisted code falls back to the code itself.
  const ELIG_REASON_LABELS = {
    late_payment_only: "Concerns only late payment",
    interest_only: "Concerns only interest",
    medicare_grievance: "Part of an open Medicare managed care grievance",
    medicare_reconsideration: "Qualifies for Medicare reconsideration",
    plan_not_fl_regulated: "Plan is not regulated by Florida",
    provider_not_fl_licensed: "Provider is not licensed in Florida",
    medicaid_fair_hearing: "Part of a Medicaid fair hearing",
    pending_court_action: "Pending state or federal court action",
    over_12_months: "Filed more than 12 months after the final determination",
    pre_2000_binding_process: "Subject to a binding pre-October 1, 2000 internal resolution process",
    below_threshold: "Below the minimum disputed amount",
    improper_aggregation: "Claims aggregated incorrectly",
    internal_process_not_exhausted: "Contract requires the internal dispute process, and it was not exhausted",
  };
  const slaBadge = (days) => {
    if (days === undefined || days === null) return '<span class="muted">—</span>';
    const cls = days < 0 ? "sla breach" : days <= 3 ? "sla risk" : days <= 7 ? "sla watch" : "sla ok";
    const label = days < 0 ? `${-days}bd overdue` : days === 0 ? "due today" : `${days}bd left`;
    return `<span class="${cls}" title="Statutory determination clock">${label}</span>`;
  };
  function sortableTh(col, label, cur) {
    if (!cur && cur !== "") return `<th>${label}</th>`;
    const active = cur === col || cur === "-" + col;
    const arrow = cur === col ? " ↑" : cur === "-" + col ? " ↓" : "";
    return `<th class="sortable${active ? " sorted" : ""}" onclick="Views.sortCases('${col}')">${label}${arrow}</th>`;
  }
  function sortCases(col) {
    if (!SORTABLE.has(col)) return;
    const q = new URLSearchParams(location.hash.split("?")[1] || "");
    const cur = q.get("sort") || "";
    // Cycle: none -> asc -> desc -> none
    const next = cur === col ? "-" + col : cur === "-" + col ? "" : col;
    next ? q.set("sort", next) : q.delete("sort");
    const s = q.toString();
    location.hash = "#/cases" + (s ? "?" + s : "");
  }
  const caseTable = (rows, clocks = {}, sort = "", prog = null, groupBatch = false) => {
    if (!rows.length) return `<p class="muted">No disputes.</p>`;
    const row = (c) => `<tr class="click" data-case="${c.id}">
        <td class="selcol"><input type="checkbox" class="sel-one" data-id="${c.id}" ${selection.has(c.id) ? "checked" : ""} aria-label="Select ${esc(c.case_number)}"></td>
        <td class="mono">${esc(c.case_number)}</td><td>${badge(c.status)}</td><td>${esc(c.service_line)}</td>
        <td class="num">$${((c[amtField(prog)] || 0) / 100).toLocaleString()}</td>
        <td>${laneChip(c.triage_lane)}</td>
        <td>${slaBadge(c.sla_days_remaining)}</td>
        <td>${clocks[c.id] && clocks[c.id].length ? clockChip(nearestClock(clocks[c.id])) : '<span class="muted">—</span>'}</td>
        <td class="muted">${fmtDate(c.opened_at)}</td>
        <td><button class="mini peek" data-id="${c.id}" title="Peek without losing your place">▸</button></td></tr>`;
    let body = "";
    if (groupBatch) {
      // Lever 3: batch-as-one — batched disputes collapse under a group header
      // so a 40-claim batch reads as ONE unit of work, not 40 queue rows.
      const groups = new Map();
      const singles = [];
      for (const c of rows) {
        if (c.batch_id) {
          if (!groups.has(c.batch_id)) groups.set(c.batch_id, []);
          groups.get(c.batch_id).push(c);
        } else singles.push(c);
      }
      for (const [bid, members] of groups) {
        const sum = members.reduce((a, c) => a + (c[amtField(prog)] || 0), 0);
        const worst = Math.min(...members.map((c) => (c.sla_days_remaining ?? 9999)));
        body += `<tr class="batch-head"><td colspan="10">▦ Batch ${esc(bid.slice(0, 8))} — ${members.length} disputes ·
          $${(sum / 100).toLocaleString()} combined · ${slaBadge(worst)}
          <span class="muted">(review as one: same payer, same fact pattern)</span></td></tr>` +
          members.map((c) => row(c).replace('<tr class="click"', '<tr class="click batched"')).join("");
      }
      body += singles.map(row).join("");
    } else {
      body = rows.map(row).join("");
    }
    return `
    <div class="dg-wrap">
      <table class="dg"><thead><tr>
        <th class="selcol"><input type="checkbox" id="sel-all" aria-label="Select all"></th>
        ${[["case_number", "Case #"], ["status", "Status"], ["service_line", "Service"], [amtField(prog), amtLabel(prog)]]
          .map(([c, l]) => sortableTh(c, l, sort)).join("")}
        <th>Lane</th>${sortableTh("sla", "SLA", sort)}
        <th>Statutory clock</th>${sortableTh("opened_at", "Opened", sort)}<th></th></tr></thead><tbody>` +
      body + `</tbody></table></div>`;
  };

  function bindGrid(rows) {
    $("#sel-all")?.addEventListener("change", (e) => {
      selection = e.target.checked ? new Set(rows.map((c) => c.id)) : new Set();
      document.querySelectorAll(".sel-one").forEach((cb) => (cb.checked = e.target.checked));
      paintBulkBar();
    });
    document.querySelectorAll(".sel-one").forEach((cb) =>
      cb.addEventListener("change", () => { cb.checked ? selection.add(cb.dataset.id) : selection.delete(cb.dataset.id); paintBulkBar(); }));
    document.querySelectorAll("tr.click").forEach((tr) =>
      tr.addEventListener("click", (e) => {
        if (e.target.closest("input,button")) return;
        location.hash = `#/cases/${tr.dataset.case}`;
      }));
    document.querySelectorAll(".peek").forEach((b) => b.addEventListener("click", () => peek(b.dataset.id)));
  }

  function paintBulkBar() {
    document.querySelector(".bulk-bar")?.remove();
    if (!selection.size) return;
    const bar = document.createElement("div");
    bar.className = "bulk-bar";
    bar.setAttribute("aria-live", "polite");
    bar.innerHTML = `<b>${selection.size} selected</b>
      <button class="mini" id="bk-assign">Assign to me</button>
      <button class="mini" id="bk-status">Set status…</button>
      <button class="mini" id="bk-clear">Clear</button>`;
    document.body.appendChild(bar);
    $("#bk-clear").onclick = () => { selection.clear(); document.querySelectorAll(".sel-one,#sel-all").forEach((cb) => (cb.checked = false)); paintBulkBar(); };
    $("#bk-assign").onclick = async (ev) => {
      await UI.run(ev.currentTarget, async () => {
        const r = await Api.cm.bulk({ action: "assign", case_ids: [...selection] });
        const ok = r.results.filter((x) => x.ok).length;
        UI.toast(`Assigned ${ok} of ${r.results.length} disputes to you`);
        selection.clear(); App.rerender();
      }, "Assigning…");
    };
    $("#bk-status").onclick = async (ev) => {
      const v = await UI.modal({ title: `Set status for ${selection.size} disputes`, fields: [
        { name: "status", label: "New status", options: [["INITIATED", "Initiated"], ["OFFER_WINDOW_OPEN", "Offer window open"], ["IN_REVIEW", "In review"], ["DETERMINED", "Determined"], ["PAYMENT_PENDING", "Payment pending"], ["CLOSED_DISMISSED", "Closed — dismissed"]], required: true },
      ], submitLabel: "Apply to selection" });
      if (!v) return;
      await UI.run(ev.currentTarget, async () => {
        const r = await Api.cm.bulk({ action: "status", status: v.status, case_ids: [...selection] });
        const ok = r.results.filter((x) => x.ok).length;
        UI.toast(`Status updated on ${ok} of ${r.results.length} disputes`);
        selection.clear(); App.rerender();
      }, "Applying…");
    };
  }

  // L4 peek panel — preview a case without losing list position
  async function peek(id) {
    document.querySelector(".peek-panel")?.remove();
    const p = document.createElement("aside");
    p.className = "peek-panel";
    p.setAttribute("role", "complementary");
    p.setAttribute("aria-label", "Case preview");
    p.innerHTML = `<div class="peek-h"><b>Preview</b><button class="icon-btn" aria-label="Close preview">✕</button></div>
      <div class="peek-b"><p class="muted">Loading…</p></div>`;
    document.body.appendChild(p);
    p.querySelector(".icon-btn").onclick = () => p.remove();
    p.addEventListener("keydown", (e) => { if (e.key === "Escape") p.remove(); });
    try {
      const [c, clocks, prog] = await Promise.all([Api.cases.get(id), Api.cm.clocks(id).catch(() => []), Api.program.get().catch(() => null)]);
      p.querySelector(".peek-b").innerHTML = `
        <div class="mono muted">${esc(c.case_number)}</div>
        <h3>${esc(c.service_line)} · $${((c[amtField(prog)] || 0) / 100).toLocaleString()}</h3>
        <p>${badge(c.status)}</p>
        ${clocks.map((cl) => `<div class="sla-card"><span class="sla ${cl.state}">${clockChip(cl)}</span>
          <span class="muted" style="font-size:11px">${esc(cl.label)} · ${esc(cl.cite)}</span></div>`).join("")}
        <p style="margin-top:14px"><button class="mini" onclick="location.hash='#/cases/${c.id}'">Open workspace →</button></p>`;
    } catch (e) { p.querySelector(".peek-b").innerHTML = err(e); }
  }

  // ---- Disputes list -----------------------------------------------------------
  // Server-side pagination: one page at a time (Pager.size() rows), Previous /
  // Next. Saved-view status filters are pushed to the server so a filter never
  // applies to a partial page. Counts come from the server total, not the page.
  async function cases() {
    try {
      const hq = new URLSearchParams(location.hash.split("?")[1] || "");
      const v = hq.get("view");
      const sort = hq.get("sort") || "";
      const lane = hq.get("lane") || "";
      const groupBatch = hq.get("batch") === "1";
      const saved = await Api.cm.views().catch(() => []);
      const sv = saved.find((s) => s.id === v);
      const status = sv && sv.filters && sv.filters.status ? sv.filters.status : "";
      const reqParams = { limit: Pager.size(), ...(status ? { status } : {}), ...(sort ? { sort } : {}), ...(lane ? { lane } : {}) };
      const [page, clocks, prog] = await Promise.all([Api.cases.list(reqParams), clockMap(), Api.program.get().catch(() => null)]);
      let loaded = page.cases.slice();   // the rows of the page on screen
      // The default sort pages by keyset, so going back needs the cursor each
      // page started from: cursors[n] opens page n.
      const cursors = ["", page.next_cursor];
      const total = page.total;
      let grandTotal = total;
      const opts = saved.map((s) => `<option value="${s.id}" ${s.id === v ? "selected" : ""}>${s.pinned ? "★ " : ""}${esc(s.name)}</option>`).join("");
      const density = localStorage.getItem("idre.density") || "comfortable";
      document.body.classList.toggle("density-compact", density === "compact");
      afterRender(() => {
        bindGrid(loaded);
        // Lever 2: lane tabs re-filter server-side; Lever 3: batch grouping
        // toggles batch-as-one rows. Both live in the hash = shareable.
        document.querySelectorAll(".lane-tab").forEach((b) => b.addEventListener("click", () => {
          const q = new URLSearchParams(location.hash.split("?")[1] || "");
          b.dataset.lane ? q.set("lane", b.dataset.lane) : q.delete("lane");
          const s = q.toString();
          location.hash = "#/cases" + (s ? "?" + s : "");
        }));
        $("#batch-tgl")?.addEventListener("click", () => {
          const q = new URLSearchParams(location.hash.split("?")[1] || "");
          groupBatch ? q.delete("batch") : q.set("batch", "1");
          const s = q.toString();
          location.hash = "#/cases" + (s ? "?" + s : "");
        });
        $("#density").onclick = () => {
          const next = (localStorage.getItem("idre.density") || "comfortable") === "comfortable" ? "compact" : "comfortable";
          (window.Prefs ? Prefs.push("density", next) : localStorage.setItem("idre.density", next));
          document.body.classList.toggle("density-compact", next === "compact");
          UI.toast(`Density: ${next}`);
        };
        $("#grab").onclick = async (ev) => {
          await UI.run(ev.currentTarget, async () => {
            const r = await Api.cm.grabNext();
            if (r.claimed) { UI.toast(`Claimed ${r.case_number} from the queue`); location.hash = `#/cases/${r.case_id}`; }
            else UI.toast(r.message, { kind: "warn" });
          }, "Grabbing…");
        };
        Pager.server($("#pg"), { total, shown: loaded.length }, async (limit, _offset, pg) => {
          if (pg === 0) cursors.length = 1; // also the page-size change path: old cursors are stale
          const more = { limit, ...(status ? { status } : {}), ...(sort ? { sort } : {}), ...(lane ? { lane } : {}) };
          const cursor = cursors[pg] || "";
          if (cursor.startsWith("offset:")) more.offset = cursor.slice(7); else if (cursor) more.cursor = cursor;
          const next = await Api.cases.list(more);
          loaded = next.cases;
          cursors[pg + 1] = next.next_cursor;
          document.querySelector(".dg-wrap").outerHTML = caseTable(loaded, clocks, sort || "opened_at", prog, groupBatch);
          bindGrid(loaded);
          // Past the first page a keyset query counts only what is left
          // after the cursor, so the list's size is the first page's total.
          if (pg === 0 && next.total != null) grandTotal = next.total;
          return { total: grandTotal, shown: loaded.length };
        });
      });
      return `<div class="view-head"><h1>Disputes</h1><span class="muted">${total} total</span>

        <span style="flex:1"></span>
        <button class="mini" id="grab">⇪ Grab next</button>
        <button class="mini" id="density">Density: ${density}</button></div>
        <p class="lane-tabs" role="tablist" aria-label="Triage lanes">
          ${[["", "All"], ["AUTO_REVIEW", "Auto-review"], ["STANDARD", "Standard"], ["COMPLEX", "Complex"]]
            .map(([l, lab]) => `<button class="mini lane-tab${lane === l ? " active" : ""}" data-lane="${l}" role="tab" aria-selected="${lane === l}">${lab}</button>`).join("")}
          <button class="mini" id="batch-tgl" aria-pressed="${groupBatch}" title="Collapse batched disputes into one reviewable group">${groupBatch ? "▦ Batches: grouped" : "▦ Group batches"}</button>
        </p>
        <p class="viewbar">
          <select onchange="location.hash='#/cases?view='+this.value" aria-label="Saved views">
            <option value="">All disputes</option>${opts}</select>
          <button class="mini" onclick="Views.saveCurrentView()">Save current view</button>
          ${sv ? `<span class="muted">filter: status = ${esc(sv.filters.status)}</span>` : ""}</p>` +
        `<div data-serverpaged>` + caseTable(loaded, clocks, sort || "opened_at", prog, groupBatch) + `</div><div id="pg"></div>`;
    } catch (e) { return `<h1>Disputes</h1>` + err(e); }
  }

  // ---- Actions (all modal-based now) ----------------------------------------------
  async function escalate(caseId) {
    const v = await UI.modal({ title: "Escalate case", danger: true, submitLabel: "Escalate",
      body: "Supervisors and federal administrators are notified immediately. This is logged to the audit trail.",
      fields: [
        { name: "clock", label: "Statutory clock", options: [["DETERMINATION_30BD", "Determination (30bd)"], ["OFFER_WINDOW_10BD", "Offer window (10bd)"], ["PAYMENT_30CD", "Payment (30cd)"], ["NEGOTIATION_30BD", "Negotiation (30bd)"], ["INITIATION_4BD", "Initiation (4bd)"]], required: true },
        { name: "detail", label: "Reason", type: "textarea", placeholder: "Why does this need supervisory attention?", required: true },
      ] });
    if (!v) return;
    try {
      await Api.cm.escalate(caseId, v.clock, v.detail);
      UI.toast("Escalated — supervisors and federal administrators notified");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function relate(caseId) {
    const v = await UI.modal({ title: "Link related case", fields: [
      { name: "related", label: "Related case ID", placeholder: "UUID", required: true },
      { name: "type", label: "Relationship", options: [["BATCH", "Batch"], ["PARENT_CHILD", "Parent / child"], ["DUPLICATE", "Duplicate"]], required: true },
    ] });
    if (!v) return;
    try {
      await Api.cm.relate({ case_id: caseId, related_case_id: v.related, rel_type: v.type });
      UI.toast("Cases linked");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function feeTransfer(caseId) {
    const v = await UI.modal({ title: "Post fee transfer", submitLabel: "Post to ledger",
      body: "Double-entry transfer on the tenant's TigerBeetle ledger. Idempotent; subject to the Permify ledger permission.",
      fields: [
        { name: "kind", label: "Transfer kind", options: [["ADMIN_FEE", "Admin fee ($15)"], ["IDRE_FEE_RESERVE", "IDRE fee reserve"], ["REFUND", "Refund"], ["SETTLEMENT", "Settlement"]], required: true },
        { name: "party", label: "Party ID", value: "party", required: true },
        { name: "amount", label: "Amount (USD)", type: "number", step: "0.01", value: "15.00", required: true },
        { name: "post", label: "Posting", options: [["PENDING", "Pending (two-phase)"], ["POST", "Post immediately"], ["VOID", "Void pending"]], required: true },
      ] });
    if (!v) return;
    try {
      await Api.fees.transfer({ case_id: caseId, kind: v.kind, party_id: v.party,
        amount_cents: Math.round(parseFloat(v.amount) * 100), post_kind: v.post });
      UI.toast("Ledger transfer created (double-entry, idempotent)");
      App.rerender();
    } catch (e) { UI.toast("Ledger rejected the transfer: " + e.message, { kind: "warn", duration: 9000 }); }
  }

  async function saveCurrentView() {
    const v = await UI.modal({ title: "Save current view", fields: [
      { name: "name", label: "View name", required: true },
      { name: "status", label: "Filter by status", placeholder: "leave blank for all" },
    ], submitLabel: "Save view" });
    if (!v) return;
    try {
      await Api.cm.saveView({ object: "CASES", name: v.name, filters: v.status ? { status: v.status } : {} });
      UI.toast("View saved");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function assign(caseId) {
    const v = await UI.modal({ title: "Assign / route", fields: [
      { name: "role", label: "Assign as", options: [["CASE_MANAGER", "Case manager"], ["ARBITRATOR", "Arbitrator"]], required: true },
      { name: "assignee", label: "Assignee", placeholder: "blank = workload-balanced auto" },
    ] });
    if (!v) return;
    try {
      const r = await Api.cm.assign(caseId, v.role, v.assignee || "");
      UI.toast(`Assigned to ${r.assigned_to}`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function letter(caseId, template) {
    let qs = "";
    if (template === "determination_letter") {
      const v = await UI.modal({ title: "Determination letter", fields: [
        { name: "winning", label: "Winning party", required: true },
        { name: "rationale", label: "Rationale", type: "textarea", required: true },
      ], submitLabel: "Generate letter" });
      if (!v) return;
      qs = `?winning_party=${encodeURIComponent(v.winning)}&rationale=${encodeURIComponent(v.rationale)}`;
    }
    try { await Api.cm.letter(caseId, template, qs); UI.toast("Letter generated — see Documents"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // Sealed offer — value goes only to the vault via the signal; never rendered back.
  async function offerForm(caseId) {
    const v = await UI.modal({ title: "Submit sealed offer", submitLabel: "Seal and submit",
      body: "Your offer is encrypted in the vault the moment you submit. It is visible to no one — including arbitrators — until the offer window closes and the lawful-reveal gate passes.",
      fields: [{ name: "amount", label: "Offer amount (USD)", type: "number", step: "0.01", required: true }] });
    if (!v) return;
    try {
      await Api.cases.signal(caseId, "OFFER_SUBMITTED", { party_id: Auth.claims().sub, amount_cents: Math.round(parseFloat(v.amount) * 100) });
      UI.toast("Offer received and sealed — receipt logged");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function determinationForm(caseId) {
    const v = await UI.modal({ title: "Issue determination", submitLabel: "Issue determination", danger: true,
      body: "This selects the prevailing offer under 45 CFR 149.510(c)(4) and starts the 30-calendar-day payment clock. It cannot be undone.",
      fields: [
        { name: "winning", label: "Winning offer (party id)", required: true },
        { name: "rationale", label: "Determination rationale", type: "textarea", required: true,
          hint: "Required — becomes part of the certified determination letter." },
      ] });
    if (!v) return;
    try {
      await Api.cases.signal(caseId, "DETERMINATION_ISSUED", { winning_offer_party: v.winning, rationale: v.rationale });
      UI.toast("Determination issued — payment clock started");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Case detail workspace -------------------------------------------------
  async function caseDetail(id) {
    try {
      const [c, docs, activities, checklist, rels, clocks, notesRes, prog] = await Promise.all([
        Api.cases.get(id), Api.cases.documents(id), Api.cases.activities(id),
        Api.cm.checklist(id), Api.cm.relationships(id), Api.cm.clocks(id).catch(() => []),
        Api.crm.notes("CASE", id).catch(() => ({ notes: [] })),
        Api.program.get().catch(() => null),
      ]);
      Palette.remember("case", c.id, c.case_number);
      let html = `<div class="view-head"><h1><span class="mono">${esc(c.case_number)}</span></h1>
        ${badge(c.status)}</div>
        <p class="muted">${esc(c.service_line)} · ${amtLabel(prog)} $${((c[amtField(prog)] || 0) / 100).toLocaleString()} · opened ${fmtDate(c.opened_at)}</p>`;
      if (c.lock) {
        html += `<div class="case-lock" role="status"><b>${c.lock === "AWAITING_PAYMENT" ? "Awaiting payment" : "Ineligible"} — this case is frozen.</b>
          ${c.lock === "AWAITING_PAYMENT"
            ? "Nothing can be done on it until the initial fee is paid. You can still write to the party (Correspondence, letters, call and inquiry log) to send payment reminders."
            : "The latest eligibility review found this dispute ineligible. You can still write to the party (Correspondence, letters, call and inquiry log); a new eligibility review is the only other action available."}</div>`;
        afterRender(() => freezeCasePage(c.lock));
      }

      // Statutory clock cluster (server-projected)
      if (clocks.length)
        html += `<div class="sla-cluster">` + clocks.map((cl) => `
          <div class="sla-card"><span class="sla ${cl.state}" style="font-size:16px">${clockChip(cl)}</span>
            <span class="lbl">${esc(cl.label)}</span>
            <span class="cite">${esc(cl.cite)} · due ${esc(cl.due)} · ${esc(cl.basis_note)}</span></div>`).join("") + `</div>`;

      // Workflow actions by role + status
      const acts = [];
      if (can("PARTY") && c.status === "INITIATED")
        acts.push(["Respond filed", () => Api.cases.signal(id, "RESPONSE_FILED", {})]);
      if (can("PARTY") && ["OFFER_WINDOW_OPEN", "INITIATED"].includes(c.status))
        acts.push(["Submit sealed offer", () => offerForm(id)]);
      if (can("PARTY"))
        acts.push(["Mark fees paid", () => Api.cases.signal(id, "FEES_PAID", { party_id: Auth.claims().sub })]);
      if (can("ARBITRATOR"))
        acts.push(["Issue determination", () => determinationForm(id)]);
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        acts.push(["Finalize IDRE selection", () => Api.cases.signal(id, "SELECTION_FINALIZED", {})]);
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        acts.push(["Escalate", () => Views.escalate(id)]);
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        acts.push(["Link related case", () => Views.relate(id)]);
      if (can("FINANCE") && ["DETERMINED", "CLOSED_PAID"].includes(c.status))
        acts.push(["Record payment", () => Api.cases.signal(id, "PAYMENT_RECORDED", { amount_cents: c.qpa_cents })]);
      if (can("FINANCE"))
        acts.push(["Post fee transfer", () => Views.feeTransfer(id)]);
      if (acts.length)
        html += `<div class="actions">` + acts.map((a, i) =>
          `<button data-act="${i}">${a[0]}</button>`).join("") + `</div>`;

      // Copilot (Phases 1-3): grounded brief; QA-gated drafts; bounded,
      // human-approved action batches via Temporal. Everything the model
      // produces is advisory until a person approves it.
      if (can("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        html += `<h2>Copilot <span class="badge s-review">DRAFT · advisory</span></h2>
          <div id="copilot"><p class="muted">Loading…</p></div>
          <div class="actions">
            <button class="mini" onclick="Views.copilotDraftQA('${id}','determination_rationale',this)">✍ Draft determination rationale</button>
            <button class="mini" onclick="Views.copilotDraftQA('${id}','correspondence',this)">✉ Draft correspondence</button>
            <button class="mini" onclick="Views.copilotPropose('${id}',this)">⚙ Propose action batch</button>
          </div>
          <div id="copilot-batches"></div>`;
        Api.program.copilotBriefLatest(id).then((r) => {
          const b = document.getElementById("copilot");
          if (b) b.innerHTML = copilotCard(r.brief, r.generated_at, id);
        }).catch(() => {
          const b = document.getElementById("copilot");
          if (b) b.innerHTML = copilotCard(null, null, id);
        });
        copilotLoadBatches(id);
      }

      // Documents — docket grouped by folder with full metadata + RBAC controls
      const FOLDERS = ["GENERAL", "INTAKE", "EVIDENCE", "CORRESPONDENCE", "OFFERS", "DETERMINATION", "INVOICES", "PARTY_UPLOADS"];
      html += `<h2>Docket</h2>
        <p><a class="button" href="javascript:void(0)" onclick="Views.downloadZip('${id}')">⬇ Download all as zip (plan-notification bundle)</a></p>
        <form id="up" class="upload"><input type="file" name="file" required aria-label="Choose file" />
        <select name="folder" aria-label="Folder">${FOLDERS.map((f) => `<option>${f}</option>`).join("")}</select>
        <label><input type="checkbox" name="sealed" /> sealed (offer justification — encrypted in the vault)</label>
        <button>Upload</button></form>`;
      if (docs.length) {
        const byFolder = {};
        docs.forEach((d) => { (byFolder[d.folder || "GENERAL"] = byFolder[d.folder || "GENERAL"] || []).push(d); });
        html += Object.entries(byFolder).map(([folder, items]) => `
          <h3 class="doc-folder">📁 ${esc(folder)} <span class="muted">(${items.length})</span></h3>
          <table><thead><tr><th>File</th><th>Type</th><th>Size</th><th>Scan</th><th>Sealed</th><th>Uploaded</th><th>Analysis</th><th></th></tr></thead><tbody>` +
          items.map((d) => `<tr>
            <td class="mono">${esc(d.filename || d.doc_id.slice(0, 8) + "…")}</td>
            <td>${esc(d.content_type || "—")}</td>
            <td>${d.size_bytes > 1048576 ? (d.size_bytes / 1048576).toFixed(1) + " MB" : (d.size_bytes / 1024).toFixed(0) + " KB"}</td>
            <td>${d.scan_status === "CLEAN" ? '<span class="badge s-paid">✓ clean</span>' : badge(d.scan_status || "PENDING")}</td>
            <td>${d.sealed ? '<span class="badge s-sealed">🔒 sealed</span>' : "—"}</td>
            <td class="muted">${esc(d.uploaded_by || "")} · ${fmtDate(d.uploaded_at)}</td>
            <td>${badge(d.analysis_status)}${d.doc_type ? " · " + esc(d.doc_type) : ""}</td>
            <td><a href="javascript:void(0)" onclick="Views.downloadDoc('${id}','${d.doc_id}')">download</a>
            ${/\.(txt|835|edi|x12)$/i.test(d.filename || "") ? ` · <a href="javascript:void(0)" title="If this is an 835 remittance (ERA), download it as a spreadsheet table" onclick="Views.era835Fn('${id}','${d.doc_id}')">835 → table</a>` : ""}
            ${!d.sealed ? ` · <a href="javascript:void(0)" onclick="Views.showAnalysis('${id}','${d.doc_id}')">analysis</a>` : ""}
            ${can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ?
              ` · <select class="mini" onchange="Views.moveDoc('${id}','${d.doc_id}',this.value,this)"><option value="">move…</option>
                ${FOLDERS.filter((f) => f !== d.folder).map((f) => `<option>${f}</option>`).join("")}</select>` : ""}</td></tr>`).join("") +
          `</tbody></table>`).join("");
      } else html += `<p class="muted">No documents yet.</p>`;
      html += `<div id="analysis"></div>`;

      // Time — per-role effort ledger on this dispute. Append-only; the
      // role stamped is the logger's real role. Eligibility decides whether
      // the work is billable; this records that it happened.
      if (can("CASE_MANAGER", "PM", "CODER", "DOCTOR", "NURSE", "NURSE_PHYSICIAN", "ARBITRATOR", "ATTORNEY", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        html += `<h2>Time</h2><div id="time-box"><p class="muted">Loading…</p></div>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.timeAdd('${id}', this)">
            <input name="hours" type="number" step="0.25" min="0.25" max="24" placeholder="hours" required aria-label="Hours worked" />
            ${timeRoleSelect()}
            <input name="entry_date" type="date" aria-label="Date worked" />
            <input name="note" placeholder="what was done" maxlength="500" />
            <label><input type="checkbox" name="billable" checked /> billable</label>
            <button class="mini">Log time</button>
            <span class="muted">recorded under your name and role — corrections are compensating entries, never edits</span></form>`;
        timeCaseBox(id).then((h) => { const b = document.getElementById("time-box"); if (b) b.innerHTML = h; });
      }

      // Financials — every dollar tied to this dispute: fee invoices and
      // payments, service-fee lines and allocated payments, payables
      // (awards), and the raw event stream.
      if (can("CASE_MANAGER", "PM", "FINANCE", "STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        html += `<h2>Financials</h2><div id="fin-box"><p class="muted">Loading…</p></div>`;
        caseFinBox(id).then((h) => { const b = document.getElementById("fin-box"); if (b) b.innerHTML = h; });
      }

      // Program panels (per-state rules: eligibility, correspondence/QA,
      // invoices, claims, dual status, program dates, opt-out) — rendered
      // only when the tenant runs a custom program.
      html += `<div id="prog"></div>`;
      programPanel(id).then((h) => {
        const b = document.getElementById("prog");
        if (b) b.innerHTML = h;
      });

      if (rels.length)
        html += `<h2>Related cases</h2><table><tbody>` + rels.map((r) =>
          `<tr><td>${badge(r.rel_type)}</td><td class="mono">${esc(r.related_case_id)}</td></tr>`).join("") + `</tbody></table>`;

      // GNN link-prediction panel (graph-intel; degrades gracefully offline)
      html += `<div id="gnn-rel"></div>`;
      Api.graph.related(id).then((g) => {
        const box = document.getElementById("gnn-rel");
        if (!box) return;
        const preds = (g && g.predictions) || [];
        box.innerHTML = `<h2>Suggested related disputes <span class="muted">· GraphSAGE ${esc(g.model_version || "")}</span></h2>` +
          (preds.length ? `<table><thead><tr><th>Case</th><th>Link score</th><th></th></tr></thead><tbody>` +
            preds.map((p) => `<tr><td class="mono">${esc(p.case_id.slice(0, 8))}…</td>
              <td><span class="gnn-score" style="--s:${p.score}">${(p.score * 100).toFixed(0)}%</span></td>
              <td><a href="#/cases/${esc(p.case_id)}">open</a> ·
                  <a href="javascript:void(0)" onclick="Views.relate('${id}')">confirm link</a></td></tr>`).join("") +
            `</tbody></table>` :
            `<p class="muted">${esc(g.reason || "No link predictions yet — train the model (POST /graph/train) after a few cases exist.")}</p>`);
      }).catch(() => { const b = document.getElementById("gnn-rel"); if (b) b.innerHTML = ""; });
      const stages = {};
      checklist.forEach((i) => { (stages[i.stage] = stages[i.stage] || []).push(i); });
      // Role-based: the server says whose each item is (owner) and whether
      // this user may tick it (can_check). Items with no owner keep the old
      // rule. Stages are listed in the order a case moves through them.
      const STAGE_ORDER = ["INTAKE", "ELIGIBILITY", "PROVIDER_PERMISSION", "PLAN_NOTIFICATION", "DETERMINATION", "PAYMENT"];
      const stageRank = (x) => (STAGE_ORDER.indexOf(x) < 0 ? 99 : STAGE_ORDER.indexOf(x));
      const mayTick = (i) => (i.owner_role ? i.can_check : can("CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN"));
      html += `<h2>Stage checklists</h2>` + Object.entries(stages).sort((a, b) => stageRank(a[0]) - stageRank(b[0])).map(([stage, items]) =>
        `<h3>${esc(stage.replace(/_/g, " "))}</h3><ul class="checklist">` + items.map((i) =>
          `<li>${i.done ? "✅" : (mayTick(i)
            ? `<button class="mini" onclick="Views.check('${i.id}')">check</button>` : "⬜")}
            ${esc(i.item)}${i.required ? " *" : ""}${i.owner ? ` <span class="chip">${esc(i.owner)}</span>` : ""}${i.done_by ? ` <span class="muted">(${esc(i.done_by)})</span>` : ""}</li>`).join("") +
        `</ul>`).join("");
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        html += `<div class="actions">
          ${can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<button onclick="Views.assign('${id}')">Assign / route</button>` : ""}
          <button onclick="Views.letter('${id}','offer_window_notice')">Generate offer-window notice</button>
          <button onclick="Views.letter('${id}','determination_letter')">Generate determination letter</button>
        </div>`;

      // Notes — addNote (POST /notes) existed with no way to read them back
      // at all; case notes also mirror onto the activity timeline below but
      // without the stream field, so internal/coder/clinical/legal/
      // external_agency notes were indistinguishable there. Confirmed live.
      const notes = notesRes.notes || [];
      // Only offer streams this role can actually write — the server has
      // enforced this for a while, but the dropdown kept listing all five
      // regardless, so picking a blocked one just produced a confusing
      // 403 on submit instead of not being offered at all.
      const streamOpts = [
        ["internal", "Internal"], ["coder", "Coder"], ["clinical", "Clinical"],
        ["legal", "Legal"], ["external_agency", "External agency"],
      ];
      const streamRoles = prog?.config?.notes_stream_roles || {};
      const allowedStreams = streamOpts.filter(([key]) => {
        const roles = streamRoles[key];
        return !roles || !roles.length || can(...roles);
      });
      html += `<h2>Notes</h2>
        <form id="note-form" class="inline-form">
          <select name="stream" aria-label="Note stream">
            ${(allowedStreams.length ? allowedStreams : streamOpts).map(([v, label]) =>
              `<option value="${v}">${label}</option>`).join("")}
          </select>
          <textarea name="body" rows="2" placeholder="Add a note…" required></textarea>
          <button>Add note</button></form>` +
        (notes.length ? `<table><tbody>` + notes.map((n) => `<tr>
            <td>${badge(n.stream)}</td><td>${esc(n.body)}</td>
            <td class="muted">${esc(n.author || "")} · ${fmtDate(n.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No notes yet.</p>`);

      html += `<h2>Activity timeline</h2>` + (activities.length ? `<table><tbody>` +
        activities.map((a) => `<tr><td>${badge(a.type)}</td><td>${esc(a.body)}</td>
          <td class="muted">${fmtDate(a.at)}</td></tr>`).join("") +
        `</tbody></table>` : `<p class="muted">No activity yet — voice calls and milestones attach automatically.</p>`);

      afterRender(() => {
        acts.forEach((a, i) =>
          document.querySelector(`[data-act="${i}"]`)?.addEventListener("click", async () => {
            try { await a[1](); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }));
        $("#up").addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = ev.target.file.files[0];
          await UI.run(ev.target.querySelector("button"), async () => {
            try { await Api.cases.upload(id, f, ev.target.sealed.checked, ev.target.folder.value); UI.toast("Document uploaded"); App.rerender(); }
            catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }, "Uploading…");
        });
        $("#note-form")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = ev.target;
          await UI.run(f.querySelector("button"), async () => {
            try {
              await Api.crm.addNote({ record_type: "CASE", record_id: id, stream: f.stream.value, body: f.body.value });
              UI.toast("Note added"); App.rerender();
            } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }, "Adding…");
        });
      });
      return html;
    } catch (e) { return err(e); }
  }

  async function moveDoc(caseId, docId, folder, sel) {
    if (!folder) return;
    await UI.run(sel, async () => {
      try { await Api.cases.moveDoc(caseId, docId, folder); UI.toast(`Moved to ${folder}`); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  async function downloadDoc(caseId, docId) {
    try { await Api.download(Api.cases.downloadUrl(caseId, docId)); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function downloadZip(caseId) {
    try { await Api.download(Api.cases.zipUrl(caseId), `${caseId}-documents.zip`); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function showAnalysis(caseId, docId) {
    const box = $("#analysis");
    box.innerHTML = `<p class="muted">Loading analysis…</p>`;
    const retryBtn = `<button onclick="Views.retryAnalysis('${caseId}','${docId}')">↻ Retry analysis</button>`;
    try {
      const a = await Api.cases.analysis(caseId, docId);
      const r = a.result || {};
      const hasFindings = (r.findings || []).length > 0;
      box.innerHTML = `<h3>Document analysis ${badge(a.status)}</h3>
        <p class="muted">type: ${esc(a.doc_type)} · seal detected: ${r.seal_detected ? "yes" : "no"} · tables: ${r.table_count ?? 0}</p>
        <pre>${esc(JSON.stringify(r.extracted || {}, null, 2))}</pre>
        ${hasFindings ? `<p class="error">Findings: ${esc(JSON.stringify(r.findings))}</p>` : ""}
        <p>${retryBtn}</p>`;
    } catch (e) {
      box.innerHTML = e.status === 404
        ? `<h3>Document analysis</h3><p class="muted">No analysis result yet — the document is still queued for the OCR/extraction pipeline (or was uploaded before doc-intel processed it). Try again shortly.</p><p>${retryBtn}</p>`
        : err(e);
    }
  }

  async function retryAnalysis(caseId, docId) {
    try {
      await Api.cases.retryAnalysis(caseId, docId);
      UI.toast("Re-analysis queued — refreshing shortly");
      setTimeout(() => showAnalysis(caseId, docId), 4000);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function check(itemId) {
    try { await Api.cm.checkItem(itemId); UI.toast("Checklist item completed"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- New dispute -------------------------------------------------------------
  function newDispute() {
    afterRender(() => $("#nd").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      const file = ev.target.doc.files[0];
      await UI.run(ev.target.querySelector("button"), async () => {
        try {
          const r = await Api.cases.initiate({
            case_number: f.case_number, service_line: f.service_line, plan_type: f.plan_type,
            qpa_cents: f.qpa ? Math.round(parseFloat(f.qpa) * 100) : 0, provider_id: f.provider_id,
            payer_id: f.payer_id, open_negotiation_end: f.one_end,
          });
          if (file) {
            // Upload is async analysis -- QPA/service line fill in on their
            // own once doc-intel finishes (usually well under a minute);
            // don't block navigation on it.
            Api.cases.upload(r.case_id, file).catch(() => {});
            UI.toast("Dispute initiated — analyzing attached document, QPA/service line will fill in automatically", { sticky: true });
          } else {
            UI.toast("Dispute initiated — statutory clocks started");
          }
          location.hash = `#/cases/${r.case_id}`;
        } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
      }, "Initiating…");
    }));
    return `<h1>New dispute</h1><form id="nd" class="form">

      <label>CMS case number <input name="case_number" required placeholder="CMS-TX-2026-00002" /></label>
      <label>Attach claim/EOB document <span class="muted">(optional — QPA and service line fill in automatically once analyzed, instead of typing them below)</span>
        <input name="doc" type="file" accept=".pdf,.png,.jpg,.jpeg,.tiff,.webp" /></label>
      <label>Service line <select name="service_line"><option value="">— leave to auto-fill —</option><option>ER</option><option>AIR_AMBULANCE</option><option>ANESTHESIA</option><option>RADIOLOGY</option><option>LAB</option><option>OTHER</option></select></label>
      <label>Plan type <select name="plan_type"><option>SELF_FUNDED</option><option>FULLY_INSURED</option></select></label>
      <label>QPA (USD) <input name="qpa" type="number" step="0.01" placeholder="leave blank to auto-fill from document" /></label>
      <label>Provider ID <input name="provider_id" required /></label>
      <label>Payer ID <input name="payer_id" required /></label>
      <label>Open negotiation end <input name="one_end" type="date" required /></label>
      <button>Initiate (starts statutory clocks)</button></form>`;
  }

  // ---- Onboarding --------------------------------------------------------------
  function onboarding() {
    let html = `<h1>Onboarding</h1>`;
    if (can("CASE_MANAGER", "FEDERAL_ADMIN")) {
      afterRender(() => onboardingBox("PENDING_APPROVAL"));
      const statusFilter = `<select id="onb-status" onchange="Views.onboardingFilter(this.value)">
        <option value="PENDING_APPROVAL" selected>Pending</option><option value="">All</option>
        <option value="APPROVED">Approved</option><option value="REJECTED">Rejected</option></select>`;
      html += `<div class="view-head"><h2>Review queue</h2>${statusFilter}</div>
        <div id="onb-box"><p class="muted">Loading…</p></div>`;
    }
    // CASE_MANAGER keeps the review queue above (deciding a pending
    // application) but not this -- starting a new one is a creation
    // action, per the "reviewer only reviews" follow-up.
    if (!can("CASE_MANAGER") || can("TENANT_ADMIN")) {
      html += `<h2>New application</h2><p class="muted">Self-service registration for provider, payer, IDRE and auditor organizations.</p>
        <p><a class="button" href="#/onboarding/new">Start application</a></p>`;
    }
    return html;
  }

  async function onboardingBox(status) {
    const box = document.getElementById("onb-box");
    if (!box) return;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const apps = await Api.onboarding.list({ status });
      if (!apps.length) { box.innerHTML = `<p class="muted">Queue empty.</p>`; return; }
      const rowsHtml = (list) => list.map((a) => `<tr><td>${esc(a.legal_name)}</td><td>${esc(a.type)}</td>
        <td>${badge(a.status)}</td><td class="muted">${esc(a.status_reason || "—")}</td><td>${fmtDate(a.submitted_at)}</td>
        <td>${["PENDING_APPROVAL"].includes(a.status) ? `
          <button onclick="Views.decide('${a.id}','APPROVE')">Approve</button>
          <button class="danger" onclick="Views.decide('${a.id}','REJECT')">Reject</button>` : ""}</td></tr>`).join("");
      const { bodyHtml, pagerHtml } = initClientPager("onb", apps, rowsHtml);
      box.innerHTML = `<table id="onb-tb"><thead><tr>
        <th>Legal name</th><th>Type</th><th>Status</th><th>Reason</th><th>Submitted</th><th></th></tr></thead>
        <tbody>${bodyHtml}</tbody></table>${pagerHtml}`;
      bindClientPager("onb", "#onb-tb tbody");
    } catch (e) { box.innerHTML = err(e); }
  }
  function onboardingFilter(status) { onboardingBox(status); }

  function onboardingNew() {
    // Reviewer has no "Start application" button and the API refuses the
    // submit; don't render a form that can only fail if the URL is typed in.
    if (can("CASE_MANAGER") && !can("TENANT_ADMIN")) return `<h1>New application</h1><p class="muted">Your role reviews applications; it can't start one.</p>`;
    afterRender(() => $("#ob").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      const payload = {};
      f.requirements.split(",").map((s) => s.trim()).filter(Boolean).forEach((k) => (payload[k] = "provided"));
      try {
        await Api.onboarding.submit({
          type: f.type, legal_name: f.legal_name, ein: f.ein, npi: f.npi || undefined,
          payload: { ...payload, contact_email: f.contact_email },
        });
        UI.toast("Application submitted for verification");
        location.hash = "#/onboarding";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }));
    return `<h1>Organization application</h1><form id="ob" class="form">
      <label>Organization type <select name="type">
        <option value="PROVIDER_ORG">Provider / facility organization</option>
        <option value="PAYER_ORG">Payer / health plan organization</option>
        <option value="IDRE_ENTITY">IDRE entity (dispute resolution)</option>
        <option value="STATE_AUDITOR_ORG">State auditor organization</option>
      </select></label>
      <label>Legal name <input name="legal_name" required /></label>
      <label>EIN <input name="ein" placeholder="12-3456789" required /></label>
      <label>NPI (providers) <input name="npi" /></label>
      <label>Contact email <input name="contact_email" type="email" required /></label>
      <label>Documents provided (comma-separated keys, e.g. w9, npi, fee_schedule) <input name="requirements" placeholder="w9, npi" /></label>
      <button>Submit for verification</button></form>`;
  }

  async function decide(id, decision) {
    let reason = "";
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject application", danger: true, submitLabel: "Reject",
        fields: [{ name: "reason", label: "Rejection reason", type: "textarea", required: true,
          hint: "Sent to the applicant and recorded in the audit trail." }] });
      if (!v) return;
      reason = v.reason;
    } else if (!(await UI.confirm("Approve application?", "The organization is provisioned into its tenant and notified.", "Approve"))) return;
    try { await Api.onboarding.decide(id, decision, reason); UI.toast(`Application ${decision.toLowerCase()}d`); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Voice console -------------------------------------------------------------
  function voiceIntakeBox(status) {
    const all = window._voiceIntake || [];
    const list = status ? all.filter((v) => v.status === status) : all;
    const rowsHtml = (l) => l.map((v) => `<tr><td>${esc(v.caller_name)}<br/><span class="muted">${esc(v.caller_phone)}</span></td>
          <td>${esc(v.organization)}</td><td>${esc(v.summary)}</td><td>${badge(v.status)}</td><td>${fmtDate(v.created_at)}</td></tr>`).join("");
    const page = initClientPager("vintake", list, rowsHtml);
    const box = document.getElementById("vintake-box");
    if (!box) return;
    box.innerHTML = list.length ? `<table id="vintake-tb"><thead><tr><th>Caller</th><th>Organization</th><th>Summary</th><th>Status</th><th>At</th></tr></thead>
        <tbody>${page.bodyHtml}</tbody></table>${page.pagerHtml}` : `<p class="muted">No intake requests${status ? ` with status ${status}` : ""}.</p>`;
    bindClientPager("vintake", "#vintake-tb tbody");
  }
  function voiceIntakeFilter(status) { voiceIntakeBox(status); }

  function voiceLogBox(direction) {
    const all = window._voiceLogs || [];
    const list = direction ? all.filter((l) => l.direction === direction) : all;
    const rowsHtml = (l) => l.map((x) => `<tr><td>${esc(x.direction)}</td><td>${esc(x.tool)}</td><td>${esc(x.case_number)}</td>
          <td>${badge(x.status)}</td><td>${fmtDate(x.created_at)}</td></tr>`).join("");
    const page = initClientPager("vlog", list, rowsHtml);
    const box = document.getElementById("vlog-box");
    if (!box) return;
    box.innerHTML = list.length ? `<table id="vlog-tb"><thead><tr><th>Direction</th><th>Tool/event</th><th>Case</th><th>Status</th><th>At</th></tr></thead>
        <tbody>${page.bodyHtml}</tbody></table>${page.pagerHtml}` : `<p class="muted">No calls${direction ? ` (${direction})` : ""} yet.</p>`;
    bindClientPager("vlog", "#vlog-tb tbody");
  }
  function voiceLogFilter(direction) { voiceLogBox(direction); }

  async function voice() {
    try {
      const [intake, logs] = await Promise.all([Api.voice.intake(), Api.voice.logs()]);
      window._voiceIntake = intake; window._voiceLogs = logs;
      const intakeStatuses = [...new Set(intake.map((v) => v.status))].sort();
      const logDirections = [...new Set(logs.map((l) => l.direction))].sort();
      afterRender(() => {
        $("#ob-call")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = Object.fromEntries(new FormData(ev.target));
          await UI.run(ev.target.querySelector("button"), async () => {
            try {
              const r = await Api.voice.outbound(f.to, f.case_number, f.script);
              UI.toast(`Outbound call ${r.status}`);
              App.rerender();
            } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }, "Dialing…");
        });
        voiceIntakeBox(""); voiceLogBox("");
      });
      return `<h1>Voice console</h1>

        <h2>Outbound call</h2>
        <form id="ob-call" class="form">
          <label>Phone number <input name="to" placeholder="+1…" required /></label>
          <label>Case number <input name="case_number" placeholder="CMS-TX-2026-00001" /></label>
          <label>Script <select name="script">
            <option value="window_closing">Offer window closing reminder</option>
            <option value="determination_issued">Determination issued</option>
            <option value="fee_reminder">Fee payment reminder</option>
            <option value="general">General update</option>
          </select></label>
          <button>Place outbound call</button>
          <p class="muted">Requires outbound calling enabled in the tenant voice config.</p>
        </form>
        <div class="view-head"><h2>Intake requests (${intake.length})</h2>
          ${intakeStatuses.length > 1 ? `<select onchange="Views.voiceIntakeFilter(this.value)"><option value="">All statuses</option>
            ${intakeStatuses.map((s) => `<option value="${esc(s)}">${esc(s)}</option>`).join("")}</select>` : ""}</div>
        <div id="vintake-box"><p class="muted">Loading…</p></div>
        <div class="view-head"><h2>Call log</h2>
          ${logDirections.length > 1 ? `<select onchange="Views.voiceLogFilter(this.value)"><option value="">All directions</option>
            ${logDirections.map((d) => `<option value="${esc(d)}">${esc(d)}</option>`).join("")}</select>` : ""}</div>
        <div id="vlog-box"><p class="muted">Loading…</p></div>`;
    } catch (e) { return `<h1>Voice console</h1>` + err(e); }
  }

  // ---- Reports ---------------------------------------------------------------------
  // CSV download helper — every report card offers one (stakeholder handoff).
  function csvDownload(name, headers, rows) {
    const q = (v) => `"${String(v ?? "").replace(/"/g, '""')}"`;
    const csv = [headers.map(q).join(",")]
      .concat(rows.map((r) => r.map(q).join(","))).join("\n");
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([csv], { type: "text/csv" }));
    a.download = name;
    a.click();
    URL.revokeObjectURL(a.href);
  }

  // Reports: runnable on-platform, each card runs its query, renders a chart
  // and a table, and exports CSV. No external BI tool needed for the
  // statutory reporting loop.
  // Reports: every card loads automatically on open (these are cheap
  // aggregate queries, not expensive jobs -- making a reporting LANDING
  // page show nothing until four separate clicks was the wrong default)
  // and each still carries its own ↻ refresh + CSV export. A KPI strip up
  // top answers "how are we doing" in one glance before anyone reads a
  // single chart, same pattern Financials already uses.
  const RPT_PALETTE = ["#2E6B52", "#B08D3E", "#1D4E7E", "#5B3E8C", "#9C2B1F", "#3F7E6B", "#7A5C2E"];
  // UI.run(ctrl, fn) early-returns with ctrl undefined (no button to
  // animate) -- fine for a click handler, wrong for the initial auto-load
  // call, which has no button at all and would silently never run.
  const runOrDirect = (btn, fn) => btn ? UI.run(btn, fn, "") : fn();
  // A toast disappears in a few seconds; the panel itself was left showing
  // its initial "Loading…" placeholder forever on any error (confirmed
  // live: a 403 from a role mismatch looked indistinguishable from a
  // panel that's still fetching). Replace the placeholder with a visible,
  // persistent message too, not just the transient toast.
  const rptError = (outId, e) => {
    const el = document.getElementById(outId);
    if (el) el.innerHTML = `<p class="error">Couldn't load this report: ${esc(e.message || "unknown error")}</p>`;
    UI.toast(e.message, { kind: "warn" });
  };
  async function loadRptStatus(btn) {
    await runOrDirect(btn, async () => {
      try {
        const rows = await Api.reports.summary();
        // Federal NSA cases price on QPA; programmed tenants (FL AHCA) have
        // no QPA concept and return 0 there -- the backend picks the right
        // column server-side and tells us which one via amount_label.
        const label = rows[0]?.amount_label || "QPA";
        // Programmed tenants (FL AHCA): agency_status='Closed' is the one
        // consistent "done" marker across every terminal internal_status
        // label -- a prefix/regex guess against the label text previously
        // missed "Provider Closure Letter Issued"/"Provider - Withdrawal"
        // entirely and overcounted open disputes (confirmed live, this is
        // why the Reports KPI never matched the Dashboard's open count).
        const programmed = label === "Disputed";
        const open = programmed
          ? rows.filter((s) => s.agency_status !== "Closed").reduce((n, s) => n + s.count, 0)
          : rows.filter((s) => !String(s.status).startsWith("CLOSED")).reduce((n, s) => n + s.count, 0);
        document.getElementById("kpi-open").textContent = open.toLocaleString();
        document.getElementById("rpt-status-out").innerHTML = `<div class="chart-card">` +
          chartDonut(rows.map((s, i) => ({ label: s.status, value: s.count, color: RPT_PALETTE[i % RPT_PALETTE.length] }))) +
          `</div><table><thead><tr><th>Status</th><th>Count</th><th>Avg ${esc(label)}</th></tr></thead><tbody>` +
          rows.map((s) => `<tr><td>${badge(s.status)}</td><td>${s.count}</td><td>$${Number(s.avg_amount_usd || 0).toFixed(0)}</td></tr>`).join("") +
          `</tbody></table>`;
        document.getElementById("rpt-status-csv").onclick = () =>
          csvDownload("case_status_rollup.csv", ["status", "count", `avg_${label.toLowerCase()}_usd`],
            rows.map((s) => [s.status, s.count, Number(s.avg_amount_usd || 0).toFixed(2)]));
      } catch (e) { rptError("rpt-status-out", e); }
    });
  }
  function renderRptSla(sla, clockFilter) {
    const byClock = {};
    sla.forEach((b) => { byClock[b.clock] = (byClock[b.clock] || 0) + 1; });
    const clocks = Object.keys(byClock).sort();
    const shown = clockFilter ? sla.filter((b) => b.clock === clockFilter) : sla;
    document.getElementById("rpt-sla-out").innerHTML = (sla.length
      ? (clocks.length > 1 ? `<p><select onchange="Views.rptSlaFilter(this.value)">
            <option value="">All clocks (${sla.length})</option>
            ${clocks.map((c) => `<option value="${esc(c)}" ${c === clockFilter ? "selected" : ""}>${esc(c)} (${byClock[c]})</option>`).join("")}
          </select></p>` : "") +
        `<div class="chart-card">` + chartHBars(
          Object.entries(byClock).map(([k, v]) => ({ label: k, value: v }))) + `</div>` +
        `<table><thead><tr><th>Case</th><th>Clock</th><th>Detail</th><th>At</th></tr></thead><tbody>` +
        shown.map((b) => `<tr><td class="mono">${esc(b.case_id)}</td><td>${badge(b.clock)}</td>
          <td>${esc(b.detail)}</td><td>${fmtDate(b.at)}</td></tr>`).join("") + `</tbody></table>`
      : `<p class="muted">No breaches recorded — all statutory clocks held. ✔</p>`);
    document.getElementById("rpt-sla-csv").onclick = () => csvDownload("sla_breaches.csv", ["case_id", "clock", "detail", "at"],
      shown.map((x) => [x.case_id, x.clock, x.detail, x.at]));
  }
  async function loadRptSla(btn) {
    await runOrDirect(btn, async () => {
      try {
        const sla = await Api.reports.sla();
        window._rptSla = sla;
        document.getElementById("kpi-breaches").textContent = sla.length.toLocaleString();
        renderRptSla(sla, "");
      } catch (e) { rptError("rpt-sla-out", e); }
    });
  }
  function rptSlaFilter(clock) { renderRptSla(window._rptSla || [], clock); }
  async function loadRptFin(btn) {
    await runOrDirect(btn, async () => {
      try {
        const fin = await Api.program.financial();
        const k = fin.kpi || {};
        const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
        document.getElementById("kpi-collected").textContent = usd(k.collected_30d_cents);
        const buckets = ["current", "1-30", "31-60", "60+"].map((b) => {
          const row = (fin.aging || []).find((a) => a.bucket === b);
          return { label: b === "current" ? "Current" : b + "d", value: row ? row.total_cents / 100 : 0 };
        });
        document.getElementById("rpt-fin-out").innerHTML =
          `<div class="kpi-row">
             <div class="kpi"><span class="kpi-n">${usd(k.collected_cents)}</span><span class="kpi-l">Collected</span></div>
             <div class="kpi"><span class="kpi-n">${usd(k.collected_30d_cents)}</span><span class="kpi-l">Last 30 days</span></div>
             <div class="kpi"><span class="kpi-n">${usd(k.refunded_cents)}</span><span class="kpi-l">Refunded</span></div></div>
           <div class="chart-card">${chartHBars(buckets, (v) => "$" + v.toLocaleString())}</div>
           <p class="muted">A/R aging, open invoices, USD</p>`;
        document.getElementById("rpt-fin-csv").onclick = () =>
          csvDownload("financial_summary.csv", ["metric", "value_usd"], [
            ["collected_all_time", (k.collected_cents || 0) / 100],
            ["collected_30d", (k.collected_30d_cents || 0) / 100],
            ["refunded", (k.refunded_cents || 0) / 100],
            ["payments_settled", k.payments_count ?? 0]]);
      } catch (e) { rptError("rpt-fin-out", e); }
    });
  }
  async function loadRptTrend(btn) {
    await runOrDirect(btn, async () => {
      try {
        const d = await Api.program.opsDashboard();
        const mk = (arr) => (arr || []).map((p) => ({ x: p.d || p.day || "", y: p.n || 0 }));
        document.getElementById("rpt-trend-out").innerHTML =
          `<div class="chart-grid">
             <div class="chart-card"><h3>Intake — last 30 days</h3>${chartArea(mk(d.cases_trend), { label: "rpt-intake" })}</div>
             <div class="chart-card"><h3>Throughput (tasks completed)</h3>${chartArea(mk(d.throughput_trend), { label: "rpt-thru", color: "#1D4E7E" })}</div>
             <div class="chart-card"><h3>Collections</h3>${chartArea(mk(d.collections_trend), { label: "rpt-coll", color: "#B08D3E", fmt: (v) => "$" + (v / 100).toLocaleString() })}</div>
           </div>`;
        document.getElementById("rpt-trend-csv").onclick = () =>
          csvDownload("trends_30d.csv", ["day", "intake", "tasks_done", "collections_cents"],
            (d.cases_trend || []).map((p, i) => [p.d || p.day, p.n,
              (d.throughput_trend || [])[i]?.n ?? 0, (d.collections_trend || [])[i]?.n ?? 0]));
      } catch (e) { rptError("rpt-trend-out", e); }
    });
  }
  const RPT_TABS = [
    { key: "status", icon: "◔", label: "Case status", accent: 1,
      desc: "dispute counts and average amount by lifecycle status" },
    { key: "sla", icon: "◷", label: "SLA breaches", accent: 2,
      desc: "every clock breach, grouped by clock, with case references" },
    { key: "fin", icon: "◍", label: "Financial", accent: 3,
      desc: "collections, refunds, A/R aging — the money picture" },
    { key: "trend", icon: "▲", label: "Trends", accent: 4,
      desc: "30-day intake, completed tasks, collections" },
  ];
  function switchRptTab(key) {
    RPT_TABS.forEach((t) => {
      document.getElementById(`rpt-tab-${t.key}`)?.classList.toggle("active", t.key === key);
      const panel = document.getElementById(`rpt-panel-${t.key}`);
      if (panel) panel.hidden = t.key !== key;
    });
  }
  async function reports() {
    if (!can("PM", "FEDERAL_ADMIN", "STATE_AUDITOR", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Reports</h1></div><p class="muted">Your role doesn't have access to Reports.</p>`;
    const geoLink = (window.IDRE_CONFIG.geoMapUrl || "")
      ? `<a class="button" href="${window.IDRE_CONFIG.geoMapUrl}" target="_blank">Geospatial audit map (GeoLibre) ↗</a>` : "";
    afterRender(() => {
      flAdhocBox();
      RPT_TABS.forEach((t) => document.getElementById(`rpt-tab-${t.key}`)
        ?.addEventListener("click", () => switchRptTab(t.key)));
      document.getElementById("rpt-status-refresh")?.addEventListener("click", (ev) => loadRptStatus(ev.currentTarget));
      document.getElementById("rpt-sla-refresh")?.addEventListener("click", (ev) => loadRptSla(ev.currentTarget));
      document.getElementById("rpt-fin-refresh")?.addEventListener("click", (ev) => loadRptFin(ev.currentTarget));
      document.getElementById("rpt-trend-refresh")?.addEventListener("click", (ev) => loadRptTrend(ev.currentTarget));
      // All four load up front (tab switching is then instant, no reload
      // wait) -- same auto-load behavior as before tabs, just reshuffled
      // into panels so a wide table/chart row never fights 3 siblings for
      // horizontal space.
      loadRptStatus(); loadRptSla(); loadRptFin(); loadRptTrend();
    });
    const loading = `<p class="muted">Loading…</p>`;
    const panelBody = { status: loading, sla: loading, fin: loading, trend: loading };
    return `<div class="view-head"><h1>Reports</h1>
      <span class="muted">charted on-platform, exportable — statutory reporting without external BI</span></div>
      ${geoLink ? `<p>${geoLink}</p>` : ""}
      <div class="kpi-row rpt-kpi-strip">
        <div class="kpi"><span class="kpi-n" id="kpi-open">—</span><span class="kpi-l">Open disputes</span></div>
        <div class="kpi kpi-danger"><span class="kpi-n" id="kpi-breaches">—</span><span class="kpi-l">SLA breaches</span></div>
        <div class="kpi"><span class="kpi-n" id="kpi-collected">—</span><span class="kpi-l">Collected (30 days)</span></div>
      </div>
      <div class="card"><h2>FL AHCA contract reports</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.flReportFn(this)">
          <input name="from" type="date" /> <input name="to" type="date" />
          <select name="kind"><option value="weekly">Weekly (16 cols A–P, task 2.4.2)</option><option value="monthly">Monthly (33 fields, task 2.4.3)</option>
            <option value="final">Final (whole contract, task 2.4.5)</option></select>
          <button class="mini">Download CSV</button></form>
        <p class="muted">Exact contract column order; dates MM/DD/YYYY; unrecorded values render N/A; internal-only fields are never included.
          With no dates, the weekly covers last week and the monthly last month.</p>
        <details class="prog-sec"><summary>Ad hoc report (task 2.4.4 — due within 10 business days of the request)</summary>
          <p class="muted">Pick the columns AHCA asked for, the period, and optionally the statuses. Leave columns unticked for all of them.</p>
          <form class="form" onsubmit="event.preventDefault(); Views.flAdhocFn(this)">
            <div class="inline-form"><label>From <input name="from" type="date" /></label> <label>To <input name="to" type="date" /></label></div>
            <div id="adhoc-cols" class="elig-flags"><span class="muted">Loading columns…</span></div>
            <button class="mini">Download ad hoc CSV</button></form></details></div>
      <div class="rpt-tabs" role="tablist">
        ${RPT_TABS.map((t, i) => `<button class="rpt-tab rpt-accent-${t.accent} ${i === 0 ? "active" : ""}"
          id="rpt-tab-${t.key}" role="tab">${t.icon} ${esc(t.label)}</button>`).join("")}
      </div>
      ${RPT_TABS.map((t, i) => `<div class="rpt-panel rpt-accent-${t.accent}" id="rpt-panel-${t.key}" ${i === 0 ? "" : "hidden"}>
        <div class="rpt-card-head"><h2>${t.icon} ${esc(t.label)}</h2>
          <div class="rpt-card-actions"><button class="mini" id="rpt-${t.key}-refresh" title="Refresh">↻</button>
            <button class="mini" id="rpt-${t.key}-csv">⬇ CSV</button></div></div>
        <p class="muted">${esc(t.desc)}</p>
        <div id="rpt-${t.key}-out">${panelBody[t.key]}</div></div>`).join("")}`;
  }

  // ---- Ask the graph (EPR-KGQA) ------------------------------------------------
  function askGraph() {
    const html = `<div class="view-head"><h1>Ask the dispute graph</h1></div>
      <p class="muted">Ask in plain English about this program's disputes. Answers come from two places: the dispute
      graph (how cases, providers and plans connect) and figures counted from the program's own records. Both are
      shown under each answer; nothing leaves the platform.</p>
      <form id="kgqa" class="kgqa-bar">
        <input name="q" required minlength="6" autocomplete="off"
          placeholder="e.g. Which organizations file the most disputes?" aria-label="Ask the dispute graph" />
        <button>Ask</button>
      </form>
      <div id="kgqa-out" aria-live="polite"></div>
      <h3>Try asking</h3>
      <div id="kgqa-cats">${kgqaCategories({})}</div>`;
    afterRender(() => {
      const out = $("#kgqa-out");
      const run = async (q) => {
        out.innerHTML = `<p class="muted">Linking entities, retrieving paths, ranking, generating…</p>`;
        try {
          const r = await Api.graph.ask(q, 5);
          Palette.remember("kgqa", r.log_id, q.slice(0, 60));
          out.innerHTML = `<div class="kgqa-answer">
              <div class="kgqa-head"><b>Answer</b>
                <span class="muted">${esc(r.generator)} · ${r.latency_ms} ms · log <span class="mono">${esc(r.log_id.slice(0, 8))}</span></span>
                <span class="kgqa-fb">
                  <button class="mini" data-fb="1" title="Helpful">▲ helpful</button>
                  <button class="mini" data-fb="0" title="Not helpful">▽</button>
                </span></div>
              <p>${esc(r.answer).replace(/\n/g, "<br/>")}</p></div>
            ${(r.entities || []).length ? `<h3>Entities linked</h3><div class="kgqa-ents">` +
              r.entities.map((e) => `<span class="kgqa-ent">${e.kind === "Case"
                ? `<a href="#/cases/${esc(e.id)}">${esc(e.label)}</a>` : esc(e.label)}
                <span class="muted">${esc(e.kind)}${e.detail ? " · " + esc(e.detail) : ""}</span></span>`).join("") + `</div>` : ""}
            ${(r.facts || []).length ? `<h3>Figures used <span class="muted">· counted from this program's records</span></h3>
              <ul class="kgqa-facts">` + r.facts.map((f) => `<li>${esc(f)}</li>`).join("") + `</ul>` : ""}
            ${(r.citations || []).length ? `<h3>Evidence paths <span class="muted">· every claim traces to one of these</span></h3>
              <ol class="kgqa-paths">` + r.citations.map((c) => `<li class="mono">${esc(c.path)}</li>`).join("") + `</ol>` : ""}
            ${(r.gnn_ranked || []).length ? `<h3>GNN-ranked related cases</h3><table><tbody>` +
              r.gnn_ranked.map((g) => `<tr><td class="mono"><a href="#/cases/${esc(g.case_id)}">${esc(g.case_id.slice(0, 8))}…</a></td>
                <td><span class="gnn-score" style="--s:${g.score}">${(g.score * 100).toFixed(0)}%</span></td></tr>`).join("") +
              `</tbody></table>` : ""}`;
          out.querySelectorAll("[data-fb]").forEach((b) => b.addEventListener("click", async () => {
            try {
              const fb = await Api.graph.feedback(r.log_id, Number(b.dataset.fb));
              UI.toast(b.dataset.fb === "1"
                ? `Thanks — ${fb.edges_reinforced || 0} evidence edge(s) reinforced for the next GNN round`
                : "Thanks — recorded on the interaction log");
            } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }));
        } catch (e) {
          out.innerHTML = `<p class="error">${esc(e.message)}</p>
            <p class="muted">Is graph-intel running? Admins: trigger a sync first (⌘K → “Sync dispute graph”).</p>`;
        }
      };
      $("#kgqa").addEventListener("submit", (ev) => { ev.preventDefault(); run(ev.target.q.value.trim()); });
      // One listener for every suggestion, present and future: the list is
      // redrawn once the tenant's real case numbers and names arrive.
      $("#kgqa-cats").addEventListener("click", (ev) => {
        const tab = ev.target.closest(".kgqa-tab");
        if (tab) {
          kgqaTab = tab.dataset.cat;
          $("#kgqa-cats").querySelectorAll(".kgqa-tab").forEach((t) => {
            const on = t === tab; t.classList.toggle("active", on); t.setAttribute("aria-selected", on);
          });
          $("#kgqa-cats").querySelectorAll(".kgqa-panel").forEach((p) => { p.hidden = p.dataset.cat !== kgqaTab; });
          return;
        }
        const b = ev.target.closest(".kgqa-hint");
        if (!b) return;
        $("#kgqa").q.value = b.dataset.q; run(b.dataset.q);
        window.scrollTo({ top: 0, behavior: "smooth" });
      });
      Api.graph.hints().then((h) => { const el = $("#kgqa-cats"); if (el) el.innerHTML = kgqaCategories(h || {}); })
        .catch(() => {});
    });
    return html;
  }

  // The questions people ask a claims-dispute program, by category, five
  // each. Questions about one case or one organization are written with this
  // tenant's own case numbers and names (from /graph/hints); where the tenant
  // has none yet, a question that needs no name takes its place.
  function kgqaCategories(h) {
    const [c1, c2] = h.case_numbers || [];
    const [f1] = h.filers || [];
    const [pairA, pairB] = h.pair || [];   // a provider and a plan that really appear on the same dispute
    const svc = ((h.service_lines || [])[0] || "").replace(/_/g, " ").toLowerCase();
    // The server says which kinds of figures this role may ask about (it
    // refuses the others whatever the page shows). Until it has answered,
    // only what every staff role may ask is offered.
    const may = new Set(h.categories || ["caseload", "services"]);
    const cats = [
      ["Caseload and status", "caseload", [
        "How many disputes do we have, and what stage is each in?",
        "How many disputes are open right now versus closed?",
        "How many disputes were opened in the last 30 days?",
        "Which internal status holds the most cases?",
        "How many cases are waiting on the provider and how many on the plan?"]],
      ["Who is filing", "parties", [
        "Which organizations file the most disputes?",
        "Which health plans are named in the most disputes?",
        "How many disputes were filed by providers versus health plans?",
        f1 ? `Which cases did ${f1} file?` : "Which providers are named in the most disputes?",
        pairA && pairB ? `What connects ${pairA} and ${pairB}?` : "Which provider and plan pairs appear together most often?"]],
      ["Related cases", "caseload", c1 ? [
        `Tell me about ${c1}`,
        `Which cases are most related to ${c1}?`,
        `Which disputes share a filer or plan with ${c1}?`,
        c2 ? `What connects ${c1} and ${c2}?` : `Who filed ${c1}, and against which plan?`,
        c2 ? `Show cases related to ${c2} and why` : `Show cases related to ${c1} and why`] : []],
      ["Services in dispute", "services", [
        "Which service lines have the most disputes?",
        svc ? `Which disputes involve ${svc} services?` : "Which disputes involve emergency services?",
        may.has("money") ? "Which service line has the highest average disputed amount?" : "Which service line has the fewest disputes?",
        "How many disputes have no service line recorded?",
        "How many emergency (ER) disputes do we have?"]],
      ["Deadlines and turnaround", "deadlines", [
        "Which open cases are older than 60 days?",
        "How many open cases are past the 90-day limit?",
        "Which deadlines have been missed, by type?",
        "Which contract deliverables are due or late?",
        "How long do cases take from opening to closing on average?"]],
      ["Eligibility", "eligibility", [
        "How many disputes were found eligible versus ineligible?",
        "What are the most common reasons a dispute is ineligible?",
        "How many cases are on hold for an invalid authorization of representative?",
        "How many eligibility decisions were borderline and sent for a second look?",
        "How many open cases past payment have no eligibility decision yet?"]],
      ["Money and outcomes", "money", [
        "What is the total amount in dispute?",
        "What are the five largest disputes by amount?",
        "How many disputes are $1 million or more?",
        "What is the average disputed amount?",
        may.has("outcomes") ? "How many cases have been decided, and what was awarded?" : "How much has been awarded so far?"]],
    ];
    // One tab per category; the open tab survives the redraw that brings
    // in the tenant's real names.
    const shown = cats.filter(([, need, qs]) => qs.length && may.has(need)).map(([name, , qs]) => [name, qs]);
    const open = shown.some(([name]) => name === kgqaTab) ? kgqaTab : shown[0][0];
    return `<div class="kgqa-tabs" role="tablist" aria-label="Question categories">${shown.map(([name]) =>
        `<button type="button" role="tab" class="kgqa-tab${name === open ? " active" : ""}" data-cat="${esc(name)}"
          aria-selected="${name === open}">${esc(name)}</button>`).join("")}</div>` +
      shown.map(([name, qs]) => `<div class="kgqa-panel" role="tabpanel" data-cat="${esc(name)}" ${name === open ? "" : "hidden"}>
        ${qs.map((q) => `<button type="button" class="kgqa-hint" data-q="${esc(q)}">${esc(q)}</button>`).join("")}</div>`).join("");
  }
  let kgqaTab = "";

  // ---- Program rules UI (G1–G15 surfaces) ---------------------------------

  async function programPanel(id) {
    const prog = await Api.program.get().catch(() => null);
    if (!prog || !prog.config) return ""; // federal NSA tenants: nothing extra
    const cfg = prog.config;
    const c = await Api.cases.get(id).catch(() => ({}));
    const det = c.details || {};
    const facts = [];
    if (det.filing_party_type) facts.push(`<span class="chip">Filed by: ${det.filing_party_type === "HEALTH_PLAN" ? "Health plan" : "Provider"}</span>`);
    if (det.packet_complete_at) facts.push(`<span class="chip">Packet complete: ${fmtDate(det.packet_complete_at)} (10-day review clock basis)</span>`);
    if (det.volume_large) {
      const vv = det.volume_violations || [];
      facts.push(vv.length
        ? `<span class="badge warn" title="${esc(vv.join("; "))}">⚠ Large-volume policy: ${vv.length} violation(s) — ineligible per Capitol Bridge policy, resubmission permitted</span>`
        : `<span class="chip">Large-volume dispute — policy compliant</span>`);
    }
    let html = `<h2>Program — ${esc(prog.program || "state rules")}</h2>
      ${facts.length ? `<p class="fact-row">${facts.join(" ")}</p>` : ""}
      <div id="wf-steps" class="card wf-card"><p class="muted">Loading the steps…</p></div>
      <details class="prog-sec" data-wf-admin><summary>Dual status (admin override)</summary>
      <p class="muted">Current: ${c.internal_status ? badge(c.internal_status) : "<em>none</em>"} / ${c.agency_status ? badge(c.agency_status) : "<em>none</em>"}</p>
      <p class="muted">Case details: ${c.details && Object.keys(c.details).length
        ? Object.entries(c.details).filter(([, v]) => v === null || typeof v !== "object").map(([k, v]) => `<span class="mono">${esc(k)}=${esc(v)}</span>`).join(", ")
        : "<em>none set</em>"}</p>
      <form id="p-status" class="inline-form">
        <select name="internal"><option value="">internal status…</option>
          ${(cfg.statuses?.internal || []).map((s) => `<option ${s === c.internal_status ? "selected" : ""}>${esc(s)}</option>`).join("")}</select>
        <select name="agency"><option value="">agency status…</option>
          ${(cfg.statuses?.agency || []).map((s) => `<option ${s === c.agency_status ? "selected" : ""}>${esc(s)}</option>`).join("")}</select>
        <button>Update status</button></form></details>

      <details class="prog-sec"><summary>Program dates (clock bases)</summary>
      <p class="muted">Statutory clocks run <b>from</b> these dates — recording one starts or restarts its clock.
        Record the date once; every deadline that depends on it re-projects automatically.</p>
      <div id="p-dates-list"></div>
      <div id="p-clock-proj"></div>
      <form id="p-date" class="inline-form">
        <select name="key">${(cfg.clocks || []).map((cl) => `<option value="${esc(cl.basis)}">${esc(cl.basis)} (${esc(cl.label)})</option>`).join("")}</select>
        <input type="date" name="value" required /><button>Record date</button></form></details>

      <details class="prog-sec" id="p-elig-details" data-wf="eligibility aor_confirm letter:acceptance letter:ineligible" data-wf-roles="CASE_MANAGER ATTORNEY" data-wf-hide><summary>Eligibility review</summary>
      <p class="muted">The engine applies this program's rules in order and records the evidence with the verdict:
        <b>1)</b> an explicit ineligibility flag wins immediately; <b>2)</b> the filing window (e.g. final
        determination + 12 months) is checked; <b>3)</b> the disputed amount is tested against the provider-type
        threshold matrix (contracted status included); <b>4)</b> an invalid AOR puts the case on attorney hold.
        Result is ELIGIBLE, INELIGIBLE (with reason code), or HOLD_AOR — and INELIGIBLE/HOLD move the case status
        automatically.</p>
      <div id="p-elig-history"></div>
      <div class="actions"><button type="button" class="mini" onclick="Views.eligChecklist('${id}')">Download Eligibility Checklist</button></div>
      ${sopConflictsHtml(c)}
      ${sopAorHtml(id, c)}
      <div class="actions"><button id="p-elig-auto" title="Derives every rule input from the case record and analyzed documents, then decides — or tells you exactly which inputs are still missing">⚡ Auto-adjudicate from case + documents</button></div>
      <div id="p-elig-auto-out"></div>
      <form id="p-elig" class="form">
        <div class="inline-form">
        <select name="provider_type" required title="provider type"><option value="">provider type…</option>
          ${[...new Set(((cfg.eligibility || {}).thresholds || []).map((x) => x.provider_type))].map((x) => `<option>${esc(x)}</option>`).join("")}</select>
        <label>contracted <input type="checkbox" name="contracted" /></label>
        <input name="amount" type="number" step="0.01" placeholder="aggregate disputed $" required />
        <label>final determination <input name="fd" type="date" required title="Final determination date — the last EOB date, service date or appeal documents are accepted as proof. The 12-month filing window runs from it." /></label>
        <label>date of service <input name="service_date" type="date" title="Earliest date of service in dispute${(cfg.eligibility || {}).services_after ? ` — must be after ${esc(cfg.eligibility.services_after)}` : ""}" /></label>
        ${((cfg.eligibility || {}).dispute_types || []).length ? `<select name="dispute_type" title="claim type"><option value="">claim type…</option>
          ${cfg.eligibility.dispute_types.map((x) => `<option value="${esc(x)}">${esc(x.replace(/_/g, " "))}</option>`).join("")}</select>` : ""}
        <label>AOR valid <input type="checkbox" name="aor_valid" checked /></label>
        <input name="aor_reason" placeholder="AOR: reason it is valid / not valid" title="Recorded with the review. If the AOR is not valid, the Attorney confirms." style="min-width:18em" />
        </div>
        <fieldset class="elig-flags"><legend>Tick any that apply — each one makes the dispute ineligible</legend>
          ${((cfg.eligibility || {}).ineligibility_reasons || []).map((x) => `<label><input type="checkbox" name="flag" value="${esc(x)}" /> ${esc(ELIG_REASON_LABELS[x] || x.replace(/_/g, " "))}</label>`).join("")}
        </fieldset>
        <button>Compute eligibility</button></form><div id="p-elig-out"></div></details>

      <details class="prog-sec" id="p-send-details"><summary>Correspondence</summary>
      <form id="p-send" class="inline-form">
        ${corrTplCache = cfg.correspondence?.templates || [], ""}
        <select name="template" onchange="Views.corrTemplateBody(this)">${(cfg.correspondence?.templates || []).map((tp) =>
          `<option value="${esc(tp.key)}">${esc(tp.key)}${tp.qa_role ? " (QA: " + esc(tp.qa_role) + ")" : ""}</option>`).join("")}</select>
        <select name="rfi_to" title="only used for the rfi template"><option value="provider">RFI to: provider</option><option value="plan">RFI to: health plan</option></select>
        <input name="to" placeholder="to emails (comma-separated)" required />
        <input name="cc" placeholder="cc emails" />
        <textarea name="body" rows="3" placeholder="message body — {share_link} inserts a secure upload link, {download_link} a link to the latest generated document; both are minted on send" required></textarea>
        <label><input type="checkbox" name="auto_share" checked /> Attach a secure upload link (minted on send)</label>
        <label><input type="checkbox" name="auto_download" /> Attach a download link to the latest generated document (minted on send)</label>
        <button>Send / submit for QA</button></form><div id="p-corr"></div></details>

      <details class="prog-sec" id="p-inv-details"><summary>Invoices & claims</summary>
      ${can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<form id="p-inv" class="inline-form" data-wf="invoice">
        <select name="party"><option>HEALTH_PLAN</option><option>PROVIDER</option></select>
        <select name="kind"><option>INITIAL_FEE</option><option>FULL_REVIEW</option><option>DEFAULT</option></select>
        <input name="amount" type="number" step="0.01" placeholder="amount $" required />
        <button>Issue invoice</button></form>` : `<p class="muted">Invoices are created by the PM.</p>`}<div id="p-inv-list" data-case-id="${esc(id)}"></div>
      <form id="p-claims" class="inline-form">
        <textarea name="csv" rows="3" placeholder="claim_number,cpt,billed,paid per line (bulk import)"></textarea>
        <p class="mhint">For disputes at/crossing 100 claims (Capitol Bridge large-volume policy), add 9 more
          columns per line: patient_first_name,patient_last_name,type_of_service,denial_reason,
          date_of_service(YYYY-MM-DD),date_claim_submitted(YYYY-MM-DD),provider_name,facility_name,evidence_location
          — the import is rejected with the exact missing fields per line if any are blank.</p>
        <button>Import claims</button></form><div id="p-claims-list"></div></details>

      <details class="prog-sec"><summary>Share links & opt-out</summary>
      <div class="actions">
        <button id="p-share-up">Create upload link</button>
        <button id="p-share-dl">Create download link</button></div><div id="p-share-out"></div>
      ${can("ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<form id="p-optout" class="inline-form">
        <label>eligible to opt out <input type="checkbox" name="eligible" /></label>
        <input name="rationale" placeholder="rationale" required /><button>Record opt-out decision</button></form>` : ""}</details>

      ${can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<details class="prog-sec"><summary>At any time</summary>
      <p class="muted">These are not steps: they can happen at any point in a case. Everything else follows the step list at the top.</p>
      <div class="actions">
        <button type="button" onclick="Views.wfAnytime('${id}','PROVIDER_WITHDRAW')">Provider asked to withdraw</button>
        <button type="button" onclick="Views.wfAnytime('${id}','RFI_RESPONSE_RECEIVED')">RFI response received</button>
        <button type="button" onclick="Views.wfAnytime('${id}','OUTREACH_DONE')">Outreach done (payment arrived, packet didn't)</button>
      </div></details>` : ""}

      ${sopCaseInfoHtml(id, c, cfg)}
      ${sopFwaHtml(id, c)}
      ${sopNotesHtml(id, c)}
      ${sopSummaryHtml(id, c, cfg)}

      <details class="prog-sec"><summary>Call log</summary>
      <form id="p-call" class="inline-form">
        <select name="direction"><option>OUTBOUND</option><option>INBOUND</option></select>
        <input name="phone" placeholder="phone number" required />
        <input name="summary" placeholder="brief description" required /><button>Log call</button></form>
      <div id="p-call-list"></div></details>

      <details class="prog-sec"><summary>Inquiries log</summary>
      <form id="p-inquiry" class="inline-form">
        <select name="method"><option>EMAIL</option><option>PHONE_CALL</option></select>
        <select name="inquiry_type"><option value="GENERAL_QUESTION">General question</option>
          <option value="GENERAL_INQUIRY">General inquiry</option>
          <option value="SUBMISSION_DOCUMENTS">Submission of documents</option></select>
        <input name="detail" placeholder="detail (optional)" /><button>Log inquiry</button></form>
      <div id="p-inquiry-list"></div></details>`;

    afterRender(() => {
      const money = (v) => Math.round(parseFloat(v) * 100);
      wfLoad(id, cfg);
      $("#p-status")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { const sr = await Api.program.setStatus(id, { internal_status: f.internal.value, agency_status: f.agency.value });
          (sr.warnings || []).forEach((w) => UI.toast("⚠ " + w, { kind: "warn", sticky: true }));
          UI.toast("Status updated"); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      const loadDates = () => {
        Api.cases.get(id).then((cc) => {
          const el = document.getElementById("p-dates-list"); if (!el) return;
          const pd = cc.program_dates || {};
          const rows = (cfg.clocks || []).map((cl) => {
            const rec = pd[cl.basis];
            return `<tr><td>${esc(cl.label)}</td><td class="mono">${esc(cl.basis)}</td>
              <td>${rec ? `<b>${esc(rec)}</b>` : `<span class="muted">not recorded</span>`}</td></tr>`;
          });
          const bases = new Set((cfg.clocks || []).map((cl) => cl.basis));
          const others = Object.entries(pd).filter(([k]) => !bases.has(k)).sort()
            .map(([k, v]) => `<tr><td>${esc(SOP_DATE_LABELS[k] || k.replace(/_/g, " "))}</td><td class="mono">${esc(k)}</td><td><b>${esc(v)}</b></td></tr>`);
          el.innerHTML = `<table><thead><tr><th>Clock basis</th><th>Key</th><th>Recorded</th></tr></thead>
            <tbody>${rows.join("")}</tbody></table>` + (others.length
            ? `<p><b>Other recorded dates</b></p><table><thead><tr><th>Date</th><th>Key</th><th>Recorded</th></tr></thead><tbody>${others.join("")}</tbody></table>` : "");
        }).catch(() => {});
        Api.cm.clocks(id).then((cls) => {
          const el = document.getElementById("p-clock-proj"); if (!el) return;
          const arr = Array.isArray(cls) ? cls : (cls.clocks || []);
          if (!arr.length) { el.innerHTML = ""; return; }
          el.innerHTML = `<p><b>Projected deadlines</b></p><table><thead><tr><th>Clock</th><th>Due</th><th>State</th><th>Basis</th></tr></thead><tbody>` +
            arr.map((cl) => `<tr><td>${esc(cl.label || cl.clock || "")}</td>
              <td>${fmtDate(cl.due)}</td><td>${badge(cl.state || "")}</td>
              <td class="muted">${esc(cl.basis_note || cl.basis || "")}</td></tr>`).join("") + `</tbody></table>`;
        }).catch(() => {});
      };
      loadDates();
      const eligBadge = (v) => `${badge(v.result)}${v.borderline ? ` <span class="badge warn" title="Borderline call — PM second review required">borderline</span>` : ""}${v.override ? ` <span class="badge" title="Human override of the computed result">override</span>` : ""}`;
      const loadElig = () => Api.program.eligibilityHistory(id).then((r) => {
        const el = document.getElementById("p-elig-history"); if (!el) return;
        const revs = r.reviews || [];
        el.innerHTML = revs.length ? `<table><thead><tr><th>Result</th><th>Reason</th><th>Rules</th><th>By</th><th>When</th></tr></thead><tbody>` +
          revs.map((v) => `<tr><td>${eligBadge(v)}</td><td>${esc(v.reason || "—")}${v.override_reason ? `<div class="muted">override: ${esc(v.override_reason)}</div>` : ""}</td>
            <td class="mono muted" title="rule version that produced this decision">${esc(v.rule_version || "—")}</td>
            <td>${esc(v.decided_by || "")}</td><td class="muted">${fmtDate(v.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No eligibility review on record yet.</p>`;
      }).catch(() => {});
      // Result renderer shared by manual + auto paths: verdict, rule version,
      // borderline callout, and — on INELIGIBLE — the override form (a written
      // reason is mandatory; the server rejects anything thinner).
      const showEligResult = (r, out, resubmit) => {
        const bl = r.borderline ? `<p class="warn-box">⚠ <b>Borderline decision</b> — ${(r.evidence?.borderline_reasons || []).map(esc).join("; ") || "within margin of a rule threshold"}. PM has been notified for a second review.</p>` : "";
        const ov = r.result === "INELIGIBLE" && resubmit ? `<details class="warn-box"><summary><b>Override to ELIGIBLE</b> — requires a written legal reason</summary>
          <p class="muted">The override, your identity, and the computed result are all recorded permanently; PM is notified.</p>
          <form onsubmit="event.preventDefault(); const t=this.reason.value.trim(); if(t.length<10){UI.toast('Give a substantive reason (min 10 chars)',{kind:'warn'});return;} resubmit(t);">
            <textarea name="reason" rows="2" style="width:100%" placeholder="Why is the computed INELIGIBLE wrong? (facts, citations)"></textarea>
            <button class="mini">Override to ELIGIBLE</button></form></details>` : "";
        out.innerHTML = `<p>${badge(r.result)} ${r.reason ? esc(r.reason) : ""}
          <span class="muted">review ${esc(r.review_id || "")}${r.rule_version ? " · rules " + esc(r.rule_version) : ""}</span></p>${bl}${ov}`;
      };
      loadElig();
      $("#p-date")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.setDate(id, f.key.value, f.value.value); UI.toast("Program date recorded — clocks re-projected"); loadDates(); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-elig-auto")?.addEventListener("click", async (ev) => {
        ev.preventDefault();
        const out = document.getElementById("p-elig-auto-out");
        await UI.run(ev.currentTarget, async () => {
          try {
            const r = await Api.program.eligibilityAuto(id);
            if (r.result === "NEEDS_HUMAN") {
              out.innerHTML = `<p class="warn-box">⚠ Can't auto-decide — missing rule inputs:
                <b>${(r.missing || []).map(esc).join(", ")}</b>. Fill them in the form below and compute manually.</p>`;
            } else {
              // A decided result (ELIGIBLE/INELIGIBLE/HOLD_AOR) moves the
              // case's internal/agency status server-side -- rerender so the
              // status badge elsewhere on this page reflects it immediately
              // instead of only updating once the user navigates away and
              // back. Confirmed live: the DB write was always correct: this
              // view just never asked for the new state. The remounted view
              // re-fetches eligibility history on its own, so the borderline
              // /override styling from showEligResult still shows up there.
              App.rerender();
              return;
            }
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Evaluating…");
      });
      $("#p-elig")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        const payload = () => ({
          provider_type: f.provider_type.value, contracted: f.contracted.checked,
          disputed_amount_cents: money(f.amount.value),
          final_determination_at: f.fd.value || "",
          service_date: f.service_date.value || "",
          dispute_type: f.dispute_type ? f.dispute_type.value : "",
          flags: [...f.querySelectorAll('input[name="flag"]:checked')].map((x) => x.value),
          aor_valid: f.aor_valid.checked, aor_reason: f.aor_reason.value,
        });
        const out = document.getElementById("p-elig-out");
        const resubmit = async (reason) => {
          try {
            const r = await Api.program.eligibility(id, { ...payload(), override: true, override_reason: reason });
            UI.toast("Override recorded — case ELIGIBLE, PM notified");
            // Override moves the case to ELIGIBLE server-side same as the
            // non-override path -- rerender so the status badge keeps up.
            App.rerender();
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        };
        try {
          const r = await Api.program.eligibility(id, payload());
          if (r.result === "INELIGIBLE") {
            // INELIGIBLE offers a same-panel override form (resubmit, below)
            // that needs to stay on screen for the user to act on -- an
            // immediate rerender here would wipe it before they could. Only
            // the override's own success path (resubmit) moves the status,
            // so the rerender belongs there instead; see its own comment.
            showEligResult(r, out, resubmit);
            loadElig();
          } else {
            // Same as the auto-eligibility handler above: the result just
            // moved this case's status server-side -- rerender so the
            // status badge updates without a manual reload. The remounted
            // view re-fetches eligibility history on its own.
            UI.toast(`${r.result}${r.reason ? " — " + r.reason : ""}`);
            App.rerender();
          }
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-send")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        const split = (v) => v.split(",").map((x) => x.trim()).filter(Boolean);
        try {
          const r = await Api.program.send(id, { template: f.template.value, body: f.body.value, to: split(f.to.value), cc: split(f.cc.value), rfi_to: f.rfi_to.value, auto_share: f.auto_share.checked, auto_download: f.auto_download.checked });
          UI.toast(r.status === "PENDING" ? "Draft submitted to QA gate" : "Sent — logged to correspondence");
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      Api.program.correspondence(id).then((r) => {
        const el = document.getElementById("p-corr"); if (!el) return;
        const rows = r.correspondence || [];
        el.innerHTML = rows.length ? `<table><tbody>` + rows.slice(0, 10).map((m) =>
          `<tr><td>${badge(m.direction)} ${mailStatusBadge(m)}</td><td>${esc(m.subject || "")}</td>
           <td class="muted">${esc(m.template || "")} · ${fmtDate(m.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : "";
      }).catch(() => {});
      // #p-inv-list's id is NOT unique across case views -- every caseDetail()
      // render creates one with the same id, so a stale poller from a
      // PREVIOUSLY viewed case can find a live element by that id and assume
      // it's still its own. Confirmed live: navigating case A -> case B left
      // A's 20s poller running, its getElementById("p-inv-list") existence
      // check passed against B's element (same id, wrong case), and it
      // overwrote B's invoice table with A's data -- A's invoice ids baked
      // into the pay/mark-paid/refund links, so clicking "refund" on what
      // LOOKED like case B's invoice acted on case A's. data-case-id lets
      // both the poller and the renderer verify they still own the element
      // they're about to touch, not just that AN element with this id exists.
      const renderInvoices = (r) => {
        const el = document.getElementById("p-inv-list");
        if (!el || el.dataset.caseId !== id) return;
        // "mark paid"/"refund" had no role gate at all here, despite
        // settleInvoice itself always having required FINANCE/admin (PM
        // too, until the access-control audit removed it) -- CASE_MANAGER
        // could already see these links and get a silent 403 on click.
        // Gated now to match the backend exactly; "pay by card" stays open
        // to everyone who can see invoices, since it's the payer-facing
        // checkout link, not a staff settlement action.
        const canSettle = can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
        el.innerHTML = (r.invoices || []).length ? `<table><tbody>` + r.invoices.map((v) =>
          `<tr><td class="mono">${esc(v.invoice_no)}</td><td>${esc(v.party)}</td><td>$${(v.amount_cents / 100).toFixed(2)}</td>
           <td>${badge(v.status)}</td>
           <td>${v.status === "OPEN" ? `<a href="javascript:void(0)" onclick="Views.payInvoice('${v.id}')">💳 pay by card</a>${canSettle ? ` ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','PAY',this)">mark paid</a> ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','REFUND',this)">refund</a>` : ""}` : ""}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No invoices on this case.</p>`;
      };
      const refreshInv = () => Api.program.invoices(id).then(renderInvoices).catch(() => {});
      refreshInv();
      // Card payments settle via Stripe webhook — poll so status flips live
      // instead of requiring a manual reload.
      const invPoll = setInterval(() => {
        const el = document.getElementById("p-inv-list");
        if (!el || el.dataset.caseId !== id) { clearInterval(invPoll); return; }
        refreshInv();
      }, 20000);
      $("#p-inv")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.issueInvoice(id, { party: f.party.value, kind: f.kind.value, amount_cents: money(f.amount.value) });
          UI.toast("Invoice issued (number = case number)"); refreshInv(); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-claims")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const lines = ev.target.csv.value.split("\n").map((l) => l.trim()).filter(Boolean).map((l) => {
          const [claim_number, cpt, billed, paid, patient_first_name, patient_last_name, type_of_service,
                 denial_reason, date_of_service, date_claim_submitted, provider_name, facility_name,
                 evidence_location] = l.split(",").map((s) => s.trim());
          return {
            claim_number, cpt, billed_cents: money(billed || 0), paid_cents: money(paid || 0),
            patient_first_name, patient_last_name, type_of_service, denial_reason,
            date_of_service, date_claim_submitted, provider_name, facility_name, evidence_location,
          };
        });
        try {
          const r = await Api.program.importClaims(id, lines);
          const vc = r.volume_check;
          if (vc && vc.large_volume && (vc.violations || []).length) {
            UI.toast(`⚠ ${r.imported} imported — large-volume policy violation(s): ${vc.violations.join("; ")}`, { kind: "warn", sticky: true });
          } else if (vc && vc.large_volume) {
            UI.toast(`${r.imported} claim lines imported — large-volume dispute, policy compliant`, { kind: "info" });
          } else {
            UI.toast(`${r.imported} claim lines imported`);
          }
          App.rerender();
        }
        catch (e) {
          // e.details echoes back claim_number from the submitted CSV --
          // escape before it reaches toast's innerHTML.
          const msg = e.details
            ? `${esc(e.message)}<br>${e.details.map(esc).join("<br>")}`
            : esc(e.message);
          UI.toast(msg, { kind: "warn", sticky: !!e.details });
        }
      });
      const share = async (kind) => {
        try {
          const r = await Api.program.shareLink(id, kind, 7);
          // apiBase is "" (same-origin), so this was rendering a bare
          // relative path -- useless to copy into a different private
          // window, which is exactly how this link is meant to be used.
          const full = location.origin + r.path;
          document.getElementById("p-share-out").innerHTML =
            `<p class="mono">share link: <a href="${esc(full)}" target="_blank">${esc(full)}</a></p>`;
          // Feed the link straight into the Correspondence draft instead of
          // leaving it for staff to copy out of here and paste into the
          // send form by hand -- that hop was the whole friction point.
          const sendForm = $("#p-send");
          if (sendForm) {
            const sentence = kind === "upload"
              ? `Please upload your documents using this secure link: ${full}`
              : `You can download your filing instructions and documents here: ${full}`;
            sendForm.body.value = sendForm.body.value ? `${sendForm.body.value}\n\n${sentence}` : sentence;
            if (!sendForm.to.value && det.requester_email) sendForm.to.value = det.requester_email;
            document.getElementById("p-send-details").open = true;
            sendForm.scrollIntoView({ behavior: "smooth", block: "center" });
            UI.toast("Link added to the Correspondence draft below — review and send");
          }
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      };
      document.getElementById("p-share-up")?.addEventListener("click", () => share("upload"));
      document.getElementById("p-share-dl")?.addEventListener("click", () => share("download"));
      $("#p-optout")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        try { await Api.program.optOut(id, ev.target.eligible.checked, ev.target.rationale.value);
          UI.toast("Opt-out decision recorded"); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      $("#p-signal")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        const data = {};
        if (f.signal.value === "PACKET_COMPLETE") data.estimate_requested = f.flag.checked;
        if (f.signal.value === "ESTIMATE_PERMISSION") data.granted = f.flag.checked;
        if (f.signal.value === "CODING_REVIEW_COMPLETE") data.clinical_requested = f.flag.checked;
        if (f.signal.value === "ATTORNEY_REVIEW_COMPLETE") {
          if (f.case_outcome.value) data.case_outcome = f.case_outcome.value;
          if (f.party_billed.value) data.party_billed = f.party_billed.value;
          if (f.amount.value) data.final_amount_awarded_cents = Math.round(parseFloat(f.amount.value) * 100);
          if (f.num_claims.value) data.num_claims_reviewed = parseInt(f.num_claims.value, 10);
        }
        try { await Api.cases.signal(id, f.signal.value, data); UI.toast(`Signal sent: ${f.signal.value}`); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      const loadCalls = () => Api.program.calls(id).then((r) => {
        const el = document.getElementById("p-call-list"); if (!el) return;
        const rows = r.calls || [];
        el.innerHTML = rows.length ? `<table><tbody>` + rows.map((c) =>
          `<tr><td>${badge(c.direction)}</td><td class="mono">${esc(c.phone)}</td><td>${esc(c.summary)}</td>
           <td class="muted">${esc(c.reviewer)} · ${fmtDate(c.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No calls logged on this case.</p>`;
      }).catch(() => {});
      $("#p-call")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.logCall(id, { direction: f.direction.value, phone: f.phone.value, summary: f.summary.value });
          f.reset(); loadCalls(); UI.toast("Call logged"); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      loadCalls();
      const loadInquiries = () => Api.program.inquiries(id).then((r) => {
        const el = document.getElementById("p-inquiry-list"); if (!el) return;
        const rows = r.inquiries || [];
        el.innerHTML = rows.length ? `<table><tbody>` + rows.map((q) =>
          `<tr><td>${badge(q.method)}</td><td>${esc(q.inquiry_type)}</td><td>${esc(q.detail || "")}</td>
           <td class="muted">${esc(q.logged_by)} · ${fmtDate(q.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No inquiries logged on this case.</p>`;
      }).catch(() => {});
      $("#p-inquiry")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.logInquiry(id, { method: f.method.value, inquiry_type: f.inquiry_type.value, detail: f.detail.value });
          f.reset(); loadInquiries(); UI.toast("Inquiry logged"); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      loadInquiries();
    });
    return html;
  }

  async function payInvoice(invId) {
    try {
      const r = await Api.program.checkout(invId);
      if (r.checkout_url) { location.href = r.checkout_url; return; } // Stripe-hosted checkout
      UI.toast("Checkout session created");
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function settleInvoice(invId, action, el) {
    let ref = "";
    if (action === "PAY") {
      const v = await UI.modal({ title: "Mark invoice paid", submitLabel: "Mark paid",
        fields: [{ name: "ref", label: "Remittance reference (RA/ERA)", placeholder: "Optional" }] });
      if (!v) return;
      ref = v.ref;
    } else if (!(await UI.confirm("Refund this invoice?", "A refund entry is posted to the ledger and the payer is notified.", "Refund", true))) return;
    await UI.run(el, async () => {
      try { await Api.program.settleInvoice(invId, action, ref); UI.toast(`Invoice ${action === "PAY" ? "paid" : "refunded"}`); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  async function qaQueue() {
    try {
      const r = await Api.program.qaQueue();
      const q = r.queue || [];
      const recent = r.recent || [];
      const toSend = r.to_send || [];
      const me = Auth.claims()?.sub;
      const maySend = (i) => i.requested_by_sub === me || can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
      const list = (v) => (Array.isArray(v) ? v : []).join(", ");
      const sendHtml = toSend.length
        ? `<h2>Passed QA — ready to send</h2>
          <p class="muted">These drafts were approved and are back with the Reviewer. Check the addresses and send; the send is logged and the case's dates are set.</p>
          <table><thead><tr><th>Subject</th><th>Approved by</th><th>To</th><th></th></tr></thead><tbody>` +
          toSend.map((i) => `<tr><td>${esc(i.subject)}</td><td>${esc(i.reviewed_by || "—")}</td><td class="muted">${esc(list(i.to_recipients)) || "<i>none yet</i>"}</td>
            <td>${maySend(i) ? `<button class="mini" onclick="Views.qaSendFn('${i.id}','${esc(list(i.to_recipients))}','${esc(list(i.cc_recipients))}')">Send…</button>` : `<span class="muted">with the Reviewer</span>`}</td></tr>`).join("") +
          `</tbody></table>` : "";
      const intro = sendHtml + `<details class="prog-sec" ${q.length ? "" : "open"}><summary>What the QA gate does</summary>
        <p class="muted">Every correspondence template in this program's rules either sends <b>immediately</b>
        or carries a <b>QA role</b> (e.g. attorney, program manager). Drafts on gated templates stop here as
        <b>PENDING</b> — the named role reviews the exact subject, body, recipients, and any secure links, then
        approves (email is delivered and logged) or rejects with a note back to the drafter. Ungated templates
        never appear here. An empty queue simply means no gated draft is waiting right now.</p></details>`;
      const recentHtml = recent.length
        ? `<h2>Recently decided</h2><table><thead><tr><th>Subject</th><th>Status</th><th>Reviewed by</th><th>When</th></tr></thead><tbody>` +
          recent.map((i) => `<tr><td>${esc(i.subject)}</td><td>${badge(i.status)}</td>
            <td>${esc(i.reviewed_by || "—")}</td><td class="muted">${fmtDate(i.reviewed_at)}</td></tr>`).join("") +
          `</tbody></table>` : "";
      if (!q.length)
        return `<div class="view-head"><h1>QA gate</h1>
          <span class="muted">nothing reaches a party without approval on gated templates</span></div>
          ${intro}<p class="muted">Queue empty — no drafts awaiting review.</p><div id="qa-detail"></div>${recentHtml}`;
      const rowsHtml = (list) => list.map((i) => `<tr><td>${esc(i.subject)}</td><td class="mono">${esc((i.case_id || "").slice(0, 8))}…</td>
            <td>${esc(i.drafted_by)}</td>
            <td><button class="mini" onclick="Views.qaReview('${i.id}')">review</button></td></tr>`).join("");
      const { bodyHtml, pagerHtml } = initClientPager("qa", q, rowsHtml);
      afterRender(() => bindClientPager("qa", "#qa-tb tbody"));
      return `<div class="view-head"><h1>QA gate</h1>
        <span class="muted">nothing reaches a party without approval on gated templates</span></div>
        ${intro}<h2>Awaiting review (${q.length})</h2>
        <table id="qa-tb"><thead><tr><th>Subject</th><th>Case</th><th>Drafted by</th><th></th></tr></thead>
        <tbody>${bodyHtml}</tbody></table>${pagerHtml}<div id="qa-detail"></div>${recentHtml}`;
    } catch (e) { return err(e); }
  }

  async function qaReview(qaId) {
    const box = $("#qa-detail");
    try {
      const d = await Api.program.qaGet(qaId);
      const to = (d.to_recipients || []).join(", ");
      const cc = (d.cc_recipients || []).join(", ");
      const isNote = d.channel === "note";
      const isCopilot = (d.artifact || "").startsWith("copilot_");
      box.dataset.channel = d.channel || "email";
      // Copilot drafts are accept/EDIT/reject: the reviewer rewrites in place
      // and the edited text is what gets approved (server records the edit).
      const bodyHtml = isCopilot
        ? `<textarea id="qa-edit" rows="14" style="width:100%">${esc(d.body)}</textarea>
           <p class="muted">Copilot draft — edit freely; the approved text is what gets ${isNote ? "filed" : "sent"}, and the edit is recorded.</p>`
        : `<pre class="qa-body">${esc(d.body)}</pre>`;
      // Recipients are editable here, not just displayed -- confirmed live:
      // a case with no provider/payer account linked resolves to ZERO
      // recipients at draft time, nothing caught that before the draft
      // reached this screen, and approving it used to fail at the raw SMTP
      // layer ("503 need RCPT command") with no way to actually supply an
      // address and retry, despite the error text promising one.
      const recipientsHtml = isNote ? "" : `
        <div class="form" style="margin:8px 0">
          <label>To <input id="qa-to" value="${esc(to)}" placeholder="recipient emails, comma-separated" /></label>
          <label>Cc <input id="qa-cc" value="${esc(cc)}" placeholder="cc emails, comma-separated" /></label>
        </div>`;
      box.innerHTML = `<div class="card"><h3>${esc(d.subject)}</h3>
        <p class="muted">${isNote ? "determination rationale · files to the case timeline on approval" : `channel ${esc(d.channel)}`}
          ${isCopilot ? ' · <span class="badge s-review">COPILOT DRAFT</span>' : ""}</p>
        ${recipientsHtml}
        ${bodyHtml}
        <div class="actions">
          <button onclick="Views.qaDecide('${qaId}','APPROVE',this)">${isNote ? "Approve & file" : "Approve & send"}</button>
          <button class="danger" onclick="Views.qaDecide('${qaId}','REJECT',this)">Reject</button></div></div>`;
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // The Reviewer sends a draft that has passed QA (SOP stage 0): the
  // addresses are entered or corrected here, the approved text is not editable.
  async function qaSendFn(qaId, to, cc) {
    const v = await UI.modal({ title: "Send the approved email", submitLabel: "Send now",
      body: "This email passed QA. Enter or correct the addresses; it is delivered immediately and logged.",
      fields: [{ name: "to", label: "To (comma-separated)", required: true, value: to },
        { name: "cc", label: "CC (comma-separated)", value: cc }] });
    if (!v) return;
    const split = (x) => (x || "").split(/[,;]+/).map((e) => e.trim()).filter(Boolean);
    try {
      const r = await Api.program.qaSend(qaId, split(v.to), split(v.cc));
      if (r.email_delivery_error) UI.toast(`Not sent: ${r.email_delivery_error}`, { kind: "warn", sticky: true });
      else UI.toast("Sent and logged");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
  }
  async function qaDecide(qaId, decision, el) {
    let note = "";
    const isNote = $("#qa-detail")?.dataset.channel === "note";
    const edited = $("#qa-edit") ? $("#qa-edit").value : "";
    const splitEmails = (v) => (v || "").split(",").map((s) => s.trim()).filter(Boolean);
    const to = isNote ? undefined : splitEmails($("#qa-to")?.value);
    const cc = isNote ? undefined : splitEmails($("#qa-cc")?.value);
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject draft", danger: true, submitLabel: "Reject",
        fields: [{ name: "note", label: "Rejection note", type: "textarea", required: true,
          hint: "Returned to the drafter with the draft." }] });
      if (!v) return;
      note = v.note;
    } else if (isNote) {
      if (!(await UI.confirm("Approve and file?", "The rationale is recorded on the case timeline. Nothing is emailed.", "Approve & file"))) return;
    } else if (!to.length && !cc.length) {
      UI.toast("Add at least one recipient before approving — this draft has none on file.", { kind: "warn" });
      return;
    } else if (!(await UI.confirm("Approve and send?", "The email is delivered to all recipients now and logged to correspondence.", "Approve & send"))) return;
    await UI.run(el, async () => {
      try {
        const r = await Api.program.qaDecision(qaId, decision, note, edited, to, cc);
        if (r.next && r.status === "APPROVED" && !r.first_check) {
          UI.toast(`Approved — the draft is now ${r.next}. Nothing has been sent yet.`, { kind: "info", duration: 9000 });
        } else if (r.first_check) {
          UI.toast(`Your check is recorded — the draft is now ${r.next}. Nothing has been sent yet.`, { kind: "info", duration: 9000 });
        } else if (r.email_delivery_error) {
          UI.toast(`Approved, but email delivery failed: ${r.email_delivery_error} — still APPROVED, fix the address above and approve again to retry`,
            { kind: "warn", sticky: true });
        } else {
          UI.toast(decision === "APPROVE" ? (isNote ? "Approved — filed to case timeline" : "Approved — sent and logged") : "Rejected");
        }
        App.rerender();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  // Intake statuses that end the lifecycle — no further advancement.
  const INTAKE_TERMINAL = ["CONVERTED", "CLOSED_REFUNDED", "INELIGIBLE"];
  // Day-13 completeness gate (AHCA): docs not received within 13 days of
  // outreach => case found incomplete, ineligible letter issues. The backend
  // sweep enforces it; this countdown makes it visible before it bites.
  function day13Countdown(i) {
    if (i.packet_complete_at || INTAKE_TERMINAL.includes(i.status) || !i.outreach_at) return "";
    const left = 13 - Math.floor((Date.now() - new Date(i.outreach_at)) / 864e5);
    if (left < 0) return `<span class="badge warn">past day 13</span>`;
    return `<span class="${left <= 3 ? "badge warn" : "muted"}">day 13 in ${left}d</span>`;
  }

  const INTAKE_STATUSES = ["DOCS_RECEIVED", "PACKET_COMPLETE", "PAID", "CONVERTED", "INELIGIBLE", "CLOSED_REFUNDED"];
  function intake() {
    // The Reviewer owns intake (SOP): it sends the submission instructions,
    // which is what opening an intake does. Matches createIntake/bulkIntake.
    const canCreate = can("CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN");
    afterRender(() => {
      intakeBox("");
      $("#intake-form")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        await newIntake(ev.target);
      });
      // Bulk intake (CSV): parse on file pick, submit posts the idempotent
      // batch and renders the per-row receipt.
      window._intakeBulk = { items: [] };
      $("#intake-bulk-file")?.addEventListener("change", (ev) => bulkIntakeFile(ev.target));
      $("#intake-bulk-btn")?.addEventListener("click", (ev) => { ev.preventDefault(); bulkIntakeSubmit(ev.target); });
    });
    const statusFilter = `<select id="intake-status" onchange="Views.intakeFilter(this.value)">
      <option value="" selected>All statuses</option>
      ${INTAKE_STATUSES.map((s) => `<option value="${s}">${s}</option>`).join("")}</select>`;
    return `<div class="view-head"><h1>Pre-case intake</h1>${statusFilter}</div>
      <p class="muted">every request to open a dispute, newest first — legacy tracker rows and real cases opened directly both land here</p>` +
      (canCreate ? `<details class="card" style="margin-bottom:12px"><summary><b>Bulk intake (CSV)</b> — third-party batch filing; idempotent by batch reference, up to 500 rows</summary>
        <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin:10px 0">
          <input type="file" id="intake-bulk-file" accept=".csv,text/csv" />
          <input id="intake-bulk-ref" placeholder="batch reference — the idempotency key" style="flex:1;min-width:220px" />
          <button class="mini" id="intake-bulk-btn" disabled>Submit batch</button></div>
        <p class="muted">Header row required: <code>email, contact_name, org, filing_party_type, amount, external_ref, notes</code>
          (amount in dollars; filing_party_type PROVIDER or HEALTH_PLAN). Resubmitting the same batch reference replays the receipt — nothing files twice.</p>
        <div id="intake-bulk-preview"></div>
        <div id="intake-bulk-result"></div></details>
      <form id="intake-form" class="inline-form">
        <input name="email" type="email" placeholder="requester email" required />
        <input name="contact_name" placeholder="contact" /><input name="org" placeholder="organization" />
        <button>New intake request</button></form>
      <p class="muted">Filing party defaults to Provider. Everything else — amount, service line,
        eligibility inputs — comes from the claim packet the filing party uploads via the link emailed to them,
        not manual entry here. Need to fill this out by talking it through instead? Use the <a href="#/assistant">Assistant</a>.</p>` : "") +
      `<div id="intake-box"><p class="muted">Loading…</p></div>`;
  }

  async function intakeBox(status) {
    const box = document.getElementById("intake-box");
    if (!box) return;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const [r, prog] = await Promise.all([Api.program.intake({ limit: Pager.size(), status }), Api.program.get().catch(() => null)]);
      const rows = r.intake || [];
      const intakeTotal = r.total ?? rows.length;
      // Federal NSA intakes price on QPA; programmed tenants (FL AHCA) have
      // no QPA concept and use a disputed amount instead -- same split as
      // everywhere else amounts show in this app. Only the results table's
      // column label needs this; the amount itself is never hand-typed --
      // it's backfilled from the filing party's uploaded documents.
      const label = amtLabel(prog);
      box.innerHTML = (rows.length ? `<table id="intake-tb" data-serverpaged><thead><tr><th>Case #</th><th>Email</th><th>Org</th><th>Filing party</th><th>Service</th><th>${esc(label)}</th><th>Status</th><th>Outreach</th><th>Packet complete</th><th></th></tr></thead><tbody>` +
          rows.map(intakeRowHtml).join("") +
          `</tbody></table>` : `<p class="muted">No intake requests${status ? ` with status ${status}` : ""}.</p>`) +
        (rows.length ? `<div id="intake-pg"></div>` : "");
      Pager.server(document.getElementById("intake-pg"), { total: intakeTotal, shown: rows.length }, async (limit, offset) => {
        const pr = await Api.program.intake({ limit, offset, status });
        const prows = pr.intake || [];
        document.querySelector("#intake-tb tbody").innerHTML = prows.map(intakeRowHtml).join("");
        return { total: pr.total ?? intakeTotal, shown: prows.length };
      });
    } catch (e) { box.innerHTML = err(e); }
  }
  function intakeFilter(status) { intakeBox(status); }

  // Shared by intake()'s initial render and every later page --
  // a real-case row (origin:"case", from startAhcaCase) has no intake
  // lifecycle left to advance and no outreach-based day-13 clock; only
  // legacy intake_requests rows get those. The amount cell reads whichever
  // of qpa_cents/disputed_amount_cents the backend actually populated for
  // that row's tenant (only one of the two is ever non-null per row).
  function intakeRowHtml(i) {
    const amtCents = i.disputed_amount_cents ?? i.qpa_cents;
    return `<tr><td>${i.case_id ?
        `<a href="#/cases/${i.case_id}">${esc(i.case_number || "view case")}</a>` :
        `<span class="muted">—</span>`}</td>
      <td>${esc(i.email)}</td><td>${esc(i.org || "")}</td>
      <td>${i.filing_party_type === "HEALTH_PLAN" ? badge("HEALTH_PLAN") : `<span class="muted">Provider</span>`}</td>
      <td>${i.service_line ? esc(i.service_line) : `<span class="muted">—</span>`}</td>
      <td>${amtCents ? `$${(amtCents / 100).toFixed(2)}` : `<span class="muted">—</span>`}</td>
      <td>${badge(i.status)} ${i.origin === "legacy" ? day13Countdown(i) : ""}</td>
      <td class="muted">${fmtDate(i.outreach_at)}</td>
      <td class="muted">${i.packet_complete_at ? fmtDate(i.packet_complete_at) : "—"}</td>
      <td>${i.origin === "legacy" && !INTAKE_TERMINAL.includes(i.status) ?
        `<select onchange="Views.advanceIntake('${i.id}', this.value, this)">
          <option value="">advance…</option><option>DOCS_RECEIVED</option>
          <option value="PACKET_COMPLETE">PACKET_COMPLETE (starts 10-day review)</option>
          <option>PAID</option><option>CONVERTED</option>
          <option>INELIGIBLE</option><option>CLOSED_REFUNDED</option></select>` : ""}</td></tr>`;
  }

  // Minimal CSV parser: quotes, escaped quotes, CRLF. Bulk files come from
  // filers' spreadsheets — Excel's default export is exactly this shape.
  function bulkCsvRows(text) {
    const rows = []; let row = [], cur = "", inQ = false;
    for (let i = 0; i < text.length; i++) {
      const c = text[i];
      if (inQ) {
        if (c === '"' && text[i + 1] === '"') { cur += '"'; i++; }
        else if (c === '"') inQ = false;
        else cur += c;
      } else if (c === '"') inQ = true;
      else if (c === ",") { row.push(cur); cur = ""; }
      else if (c === "\n" || c === "\r") {
        if (c === "\r" && text[i + 1] === "\n") i++;
        row.push(cur); cur = "";
        if (row.some((v) => v.trim() !== "")) rows.push(row);
        row = [];
      } else cur += c;
    }
    row.push(cur);
    if (row.some((v) => v.trim() !== "")) rows.push(row);
    return rows;
  }

  function bulkIntakeFile(input) {
    const file = input.files && input.files[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      const rows = bulkCsvRows(String(reader.result || ""));
      const head = (rows.shift() || []).map((h) => h.trim().toLowerCase().replace(/^\uFEFF/, ""));
      if (!head.includes("email")) {
        $("#intake-bulk-preview").innerHTML = `<p class="badge warn">header row must include at least: email</p>`;
        return;
      }
      const items = rows.map((r) => {
        const cell = (name) => { const i = head.indexOf(name); return i >= 0 ? (r[i] || "").trim() : ""; };
        const cents = cell("amount") ? Math.round(parseFloat(cell("amount").replace(/[$,]/g, "")) * 100) : 0;
        return {
          external_ref: cell("external_ref"), email: cell("email"),
          contact_name: cell("contact_name"), org: cell("org"), notes: cell("notes"),
          filing_party_type: cell("filing_party_type").toUpperCase(),
          disputed_amount_cents: Number.isFinite(cents) ? cents : 0,
          qpa_cents: Number.isFinite(cents) ? cents : 0,
        };
      }).filter((it) => it.email);
      window._intakeBulk = { items };
      const bad = rows.length - items.length;
      $("#intake-bulk-preview").innerHTML =
        `<p class="muted">${items.length} row(s) ready${bad ? ` — ${bad} row(s) skipped (no email)` : ""}${items.length > 500 ? " — <b>over the 500-row limit, split the file</b>" : ""}.</p>`;
      $("#intake-bulk-btn").disabled = !items.length || items.length > 500;
    };
    reader.readAsText(file);
  }

  async function bulkIntakeSubmit(btn) {
    const ref = ($("#intake-bulk-ref")?.value || "").trim();
    if (!ref) { UI.toast("batch reference required — it is the idempotency key", { kind: "warn" }); return; }
    const { items } = window._intakeBulk || { items: [] };
    if (!items.length) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.intakeBulk({ batch_ref: ref, items });
        const rows = (r.results || []).map((x) => `<tr>
          <td class="muted">${esc(x.external_ref || "")}</td>
          <td>${x.status === "CREATED" ? badge("CREATED") : `<span class="badge warn">ERROR</span>`}${x.payment_required ? ` <span class="badge warn" title="Party must pay the initial review fee before the case number is released and upload unlocks">fee due</span>` : ""}</td>
          <td>${x.case_number ? `<a href="#/cases/${x.case_id}">${esc(x.case_number)}</a>` : esc(x.intake_id || "")}</td>
          <td class="muted">${esc(x.error || "")}${x.payment_required ? "Payment link emailed to the filing party — case activates on settlement." : ""}</td></tr>`).join("");
        $("#intake-bulk-result").innerHTML = `<div class="card" style="margin-top:10px">
          <b>Batch ${esc(r.batch_ref || ref)}</b> — ${r.created} filed, ${r.errors} error(s)${r.idempotent_replay ? " — <b>idempotent replay</b>: this reference was already submitted; showing the recorded receipt, nothing re-filed" : ""}
          <table style="margin-top:8px"><thead><tr><th>Row</th><th>Outcome</th><th>Intake / case</th><th>Error</th></tr></thead><tbody>${rows}</tbody></table></div>`;
        UI.toast(r.idempotent_replay ? "Batch already filed — receipt replayed" : `Batch filed: ${r.created} created, ${r.errors} errors`,
          { kind: r.errors ? "warn" : "ok" });
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Filing batch…");
  }

  async function newIntake(form) {
    try {
      // Only contact info is collected here -- filing_party_type defaults
      // to PROVIDER server-side (createIntake, programops.go), and amount/
      // service_line/eligibility inputs are intentionally left unset: the
      // filing party uploads the real claim packet via the link in their
      // automated instructions email, and doc-intel's analysis backfills
      // all of that from the documents (backfill_case_fields in doc-intel-
      // py/main.py) -- never hand-typed at intake.
      const r = await Api.program.createIntake({
        email: form.email.value, contact_name: form.contact_name.value, org: form.org.value,
      });
      if (r.programmed) {
        UI.toast(r.payment_required
          ? `Case ${r.case_number} created — AWAITING PAYMENT: fee link emailed to ${esc(form.email.value)}; case number and upload link release on payment`
          : `Case ${r.case_number} opened — filing instructions emailed to ${esc(form.email.value)}`, { sticky: true });
        location.hash = `#/cases/${r.case_id}`;
      } else {
        UI.toast("Intake request opened — submission instructions queued");
        App.rerender();
      }
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function advanceIntake(id, status, sel) {
    if (!status) return;
    let caseId = "";
    if (status === "PACKET_COMPLETE") {
      const ok = await UI.modal({ title: "Mark packet complete", submitLabel: "Start the 10-day clock",
        body: "The 10-day initial-review clock starts NOW (complete-packet receipt). This first mark is permanent — it cannot be restarted." });
      if (ok === null) { if (sel) sel.value = ""; return; }
    }
    if (status === "INELIGIBLE") {
      const ok = await UI.modal({ title: "Find intake ineligible", submitLabel: "Mark ineligible", danger: true,
        body: "The party may resubmit once the ineligibility reason is cured. Staff must issue the ineligibility letter." });
      if (ok === null) { if (sel) sel.value = ""; return; }
    }
    if (status === "CONVERTED") {
      const v = await UI.modal({ title: "Convert intake to case", submitLabel: "Link case",
        fields: [{ name: "case_id", label: "Case ID to link", required: true, placeholder: "UUID" }] });
      if (!v) { if (sel) sel.value = ""; return; }
      caseId = v.case_id;
    }
    await UI.run(sel, async () => {
      try { await Api.program.advanceIntake(id, status, caseId); UI.toast(`Intake → ${status}`); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); if (sel) sel.value = ""; }
    });
  }

  // How a delivery compared with its deadline: the target date, and the
  // contract's hard limit where it allows longer.
  const DELIV_GRADE = {
    ON_TIME: `<span class="badge s-paid">on time</span>`,
    LATE_WITHIN_LIMIT: `<span class="badge warn" title="After the target date, inside the contract's limit">late, within limit</span>`,
    BREACH: `<span class="badge s-denied" title="Past the contract's limit">breach</span>`,
  };
  const DELIV_STATE = { ...DELIV_GRADE, ON_TRACK: `<span class="badge s-paid">on track</span>`, DUE_SOON: `<span class="badge warn">due soon</span>`,
    LATE_WITHIN_LIMIT: `<span class="badge warn" title="Past the target date, inside the contract's limit">late, within limit</span>` };
  function delivOutstandingHtml(rows) {
    if (!rows.length) return `<h2>Owed per case</h2><p class="muted">No open case is waiting on a contract deliverable.</p>`;
    const rank = { BREACH: 0, LATE_WITHIN_LIMIT: 1, DUE_SOON: 2, ON_TRACK: 3 };
    rows = rows.slice().sort((a, b) => (rank[a.state] - rank[b.state]) || a.days_left - b.days_left);
    return `<h2>Owed per case</h2>
      <p class="muted">Each open case owes these under the contract. They are marked delivered automatically when the letter that carries them is sent.</p>
      <table><thead><tr><th>Case</th><th>Deliverable</th><th>Target date</th><th>Contract limit</th><th>Days left</th><th>State</th></tr></thead><tbody>` +
      rows.map((x) => `<tr><td><a href="#/cases/${esc(x.case_id)}">${esc(x.case_number)}</a></td>
        <td>${esc(x.name)} <span class="muted">§${esc(x.contract_ref)}</span></td><td>${esc(x.due_date)}</td>
        <td class="muted">${x.hard_due_date !== x.due_date ? esc(x.hard_due_date) : "—"}</td>
        <td>${x.days_left}</td><td>${DELIV_STATE[x.state] || esc(x.state)}</td></tr>`).join("") + `</tbody></table>`;
  }
  function delivContractHtml(c) {
    if (!can("FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) return "";
    return `<details class="prog-sec" ${c.execution_date || c.effective_date || c.end_date ? "" : "open"}><summary>Contract dates</summary>
      <p class="muted">The contract-level deliverables (background screening policy, disaster recovery plan, transition documents, final report) are dated from these.</p>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.saveContractDates(this)">
        <label>executed <input type="date" name="execution_date" value="${esc(c.execution_date || "")}" /></label>
        <label>effective <input type="date" name="effective_date" value="${esc(c.effective_date || "")}" /></label>
        <label>ends <input type="date" name="end_date" value="${esc(c.end_date || "")}" /></label>
        <button class="mini">Save contract dates</button></form></details>`;
  }
  async function saveContractDates(form) {
    try {
      await Api.program.setContractDates({ execution_date: form.execution_date.value, effective_date: form.effective_date.value, end_date: form.end_date.value });
      UI.toast("Contract dates saved"); App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }

  async function deliverables() {
    try {
      const r = await Api.program.deliverables();
      const d = r.deliverables || [];
      const hist = r.history || [];
      const lastDelivered = (name) => hist.filter((h) => h.name === name)
        .sort((a, b) => String(b.delivered_at).localeCompare(String(a.delivered_at)))[0];
      const daysUntil = (due) => Math.ceil((new Date(due + "T00:00:00") - Date.now()) / 864e5);
      const dueChip = (due) => {
        if (!due || !/^\d{4}-\d{2}-\d{2}$/.test(due)) return `<span class="muted">—</span>`;
        const n = daysUntil(due);
        if (n < 0) return `<span class="badge s-denied">${Math.abs(n)}d overdue</span>`;
        if (n <= 3) return `<span class="badge warn">due in ${n}d</span>`;
        return `<span class="badge s-paid">due in ${n}d</span>`;
      };
      return `<div class="view-head"><h1>Contract deliverables</h1>
        <span class="muted">what the program owes the agency, when it's due, and proof it shipped</span></div>
      <details class="prog-sec" open><summary>How deliverables work</summary>
        <p class="muted">Each program contract defines scheduled reports (the <b>rule</b> — e.g. monthly, per quarter).
        The platform computes the <b>next due date</b> from the rule and delivery history. When a report ships,
        press <b>mark delivered</b> — that records the delivery timestamp (your proof to the agency) and rolls the
        schedule forward. Ad hoc agency requests get a due date of +10 business days automatically.
        Anything overdue or due within 3 days is highlighted; due soon is amber, on track is green.</p></details>` +
      (can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") ? `<form class="inline-form" onsubmit="event.preventDefault(); Views.requestDeliverable(new FormData(event.target), event.target.querySelector('button'))">
        <input name="name" required placeholder="Ad hoc report name" />
        <input name="ref" placeholder="Contract ref (optional)" size="10" />
        <button class="mini">request ad hoc (due +10 business days)</button></form>` : "") +
        (d.length ? `<div class="deliv-grid">` + d.map((x) => {
          const last = lastDelivered(x.name);
          return `<div class="deliv-card">
            <div class="deliv-head"><b>${esc(x.name)}</b>${dueChip(x.next_due)}</div>
            <div class="muted mono">${x.contract_ref ? "§" + esc(x.contract_ref) + " · " : ""}${esc(x.due_rule)}</div>
            <div class="deliv-meta">next due <b>${esc(x.next_due || "—")}</b><br/>
              last delivered ${last ? fmtDate(last.delivered_at) : `<span class="muted">never</span>`}</div>
            <button class="mini" onclick="Views.submitDeliverable('${esc(x.name)}', this)">✓ mark delivered</button></div>`;
        }).join("") + `</div>` : `<p class="muted">No deliverables configured for this program.</p>`) +
        delivOutstandingHtml(r.outstanding || []) + delivContractHtml(r.contract || {}) +
        (hist.length ? `<h2>Delivery history</h2><table><thead><tr><th>Deliverable</th><th>Case / period</th><th>Due</th><th>Status</th><th>Against the deadline</th><th>Delivered</th></tr></thead><tbody>` +
          hist.map((h) => `<tr><td>${esc(h.name)}${h.contract_ref ? ` <span class="muted">§${esc(h.contract_ref)}</span>` : ""}</td>
            <td>${h.case_id ? `<a href="#/cases/${esc(h.case_id)}">${esc(h.case_number || "case")}</a>` : esc(h.period || "—")}</td>
            <td class="muted">${esc(h.due_date || "—")}</td><td>${badge(h.status)}</td>
            <td>${DELIV_GRADE[h.timeliness] || (h.status === "DUE" ? dueChip(h.due_date) : `<span class="muted">—</span>`)}</td>
            <td class="muted">${h.delivered_at ? fmtDate(h.delivered_at) : "—"}</td></tr>`).join("") + `</tbody></table>` : "");
    } catch (e) { return err(e); }
  }

  async function requestDeliverable(f, btn) {
    await UI.run(btn, async () => {
      try { const r = await Api.program.requestDeliverable(f.get("name"), f.get("ref"));
        UI.toast("Ad hoc report requested — due " + r.due_date); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Requesting…");
  }

  async function submitDeliverable(name, el) {
    await UI.run(el, async () => {
      try { await Api.program.submitDeliverable({ name }); UI.toast("Deliverable recorded"); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  // ---- Financial dashboard (all money movement through the platform) -------

  async function finance() {
    // CASE_MANAGER deliberately excluded -- standalone finance dashboard,
    // not case-detail info; matches the backend's financialReadRoles
    // (access-control follow-up). Direct-URL guard: the nav link is
    // already hidden, but the API calls below would otherwise just 403.
    if (!can(...["PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"]))
      return `<div class="view-head"><h1>Financials</h1></div><p class="muted">Requires PM, FINANCE, or admin.</p>`;
    try {
      const FIN_PAGE = Pager.size();
      const [fin, pays, chks] = await Promise.all([
        Api.program.financial(),
        Api.program.payments(null, { limit: FIN_PAGE }).catch(() => ({ payments: [] })),
        Api.program.checks("", { limit: FIN_PAGE }).catch(() => ({ checks: [] })),
      ]);
      // first page of each server-paged list
      finPager = {
        checks: { rows: chks.checks || [], next: chks.next_offset ?? -1, total: chks.total ?? (chks.checks || []).length },
        payments: { rows: pays.payments || [], next: pays.next_offset ?? -1, total: pays.total ?? (pays.payments || []).length },
      };
      const k = fin.kpi || {};
      const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
      let html = `<div class="view-head"><h1>Financials</h1>
        <span class="muted">every payment and transaction through the platform · tenant <b>${esc(Api.getTenant()).toUpperCase()}</b>
        ${fin.stripe_enabled ? ' · <span class="badge s-paid">stripe live</span>' : ' · <span class="badge">stripe not configured</span>'}</span></div>
        <div class="kpi-row">
          <div class="kpi"><span class="kpi-n">${usd(k.collected_cents)}</span><span class="kpi-l">Collected (all time)</span></div>
          <div class="kpi"><span class="kpi-n">${usd(k.collected_30d_cents)}</span><span class="kpi-l">Collected (30 days)</span></div>
          <div class="kpi"><span class="kpi-n">${usd(k.refunded_cents)}</span><span class="kpi-l">Refunded</span></div>
          <div class="kpi"><span class="kpi-n">${esc(String(k.payments_count ?? 0))}</span><span class="kpi-l">Payments settled</span></div>
        </div>`;

      // Receivables + aging
      const rec = fin.receivables || [];
      if (rec.length)
        html += `<h2>Receivables by status</h2><table><thead><tr><th>Status</th><th>Party</th><th>#</th><th>Total</th></tr></thead><tbody>` +
          rec.map((x) => `<tr><td>${badge(x.status)}</td><td>${esc(x.party)}</td><td>${x.n}</td><td>${usd(x.total_cents)}</td></tr>`).join("") +
          `</tbody></table>`;
      const aging = fin.aging || [];
      if (aging.length)
        html += `<h2>A/R aging (open invoices)</h2><div class="aging-row">` +
          ["current", "1-30", "31-60", "60+"].map((b) => {
            const row = aging.find((a) => a.bucket === b);
            return `<div class="aging-cell ${b === "60+" ? "bad" : b === "current" ? "" : "warn"}">
              <span class="kpi-n">${row ? usd(row.total_cents) : "$0"}</span>
              <span class="kpi-l">${b === "current" ? "Current" : b + " days past due"}${row ? ` · ${row.n} inv` : ""}</span></div>`;
          }).join("") + `</div>`;

      // Payment rails
      const bm = fin.by_method || [];
      if (bm.length)
        html += `<h2>Payment rails</h2><table><thead><tr><th>Provider</th><th>Status</th><th>#</th><th>Total</th></tr></thead><tbody>` +
          bm.map((x) => `<tr><td>${esc(x.provider)}</td><td>${badge(x.status)}</td><td>${x.n}</td><td>${usd(x.total_cents)}</td></tr>`).join("") +
          `</tbody></table>`;

      // Physical check intake (OCR/ICR — image is evidence, clearing settles)
      // Field names here match checks.go's real SELECT (confirmed directly):
      // courtesy_amount_cents (aliased from amount_cents), invoice_id (NOT
      // matched_invoice_id), and confidence is a text enum ("high"/"medium"/
      // "low", written by check_processor.py) -- not a 0-1 float, so no
      // percent math on it.
      checkRow = (c) => `<tr>
            <td class="mono">${esc((c.id || "").slice(0, 8))}…</td>
            <td>${esc(c.payer_name || "—")}</td>
            <td class="mono">${esc(c.check_date || "—")}</td>
            <td class="mono">${esc(c.routing_number || "?")} · ${esc(c.account_number || "?")}</td>
            <td>${c.courtesy_amount_cents != null ? usd(c.courtesy_amount_cents) : "—"}</td>
            <td>${c.legal_amount_cents != null ? usd(c.legal_amount_cents) : "—"}${c.amount_mismatch ? ' <span class="badge s-denied">mismatch</span>' : ""}</td>
            <td class="mono">${esc(c.memo || "")}</td>
            <td>${c.invoice_id ? `<span class="mono">${esc(c.invoice_id.slice(0, 8))}…</span>` : "—"}</td>
            <td>${badge(c.status)}</td>
            <td class="muted">${c.confidence ? esc(c.confidence) : "—"}</td>
            <td>${c.status === "MATCHED" ? `<button class="mini" onclick="Views.clearCheck('${c.id}', this)">✓ clear funds</button>` : ""}
                ${c.status === "REVIEW" && c.case_id ? `<button class="mini" onclick="Views.requestRescan('${c.id}','${c.case_id}', this)">↻ request rescan</button>` : ""}</td></tr>`;
      payRow = (p) => `<tr><td class="mono">${esc((p.case_id || "").slice(0, 8))}…</td>
            <td>${esc(p.payer_email || "—")}</td><td>${usd(p.amount_cents)}</td><td>${badge(p.status)}</td>
            <td class="mono">${esc(p.payment_intent || p.session_id || "")}</td>
            <td class="muted">${fmtDate(p.created_at)}</td></tr>`;
      const ck = finPager.checks.rows;
      const reviewCk = ck.filter((c) => c.status === "REVIEW");
      html += `<h2>Check intake (OCR/ICR)</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.uploadCheck(event.target.check.files[0], event.target.querySelector('button'))">
          <input type="file" name="check" accept="image/jpeg,image/png,image/tiff,image/webp" required />
          <button class="mini">📷 scan / upload check</button>
          <span class="muted">MICR + courtesy + legal amount extracted automatically; a human clears before funds move</span></form>` +
        (ck.length ? `<table data-serverpaged><thead><tr><th>Check</th><th>Payee</th><th>Date</th><th>Routing · Account</th><th>Courtesy</th><th>Legal (ICR)</th><th>Memo</th><th>Match</th><th>Status</th><th>Conf</th><th></th></tr></thead><tbody id="fin-ck-body">` +
          ck.map(checkRow).join("") + `</tbody></table>` +
          (reviewCk.length ? `<p class="muted">⚠ ${reviewCk.length} check(s) awaiting manual review — OCR could not match them to an open invoice.</p>` : "") :
          `<p class="muted">No checks received yet — upload a scan or photo to start intake.</p>`) +
        (ck.length ? `<div id="fin-ck-pg"></div>` : "");

      // Card payments
      const plist = finPager.payments.rows;
      if (plist.length)
        html += `<h2>Card payments</h2><table data-serverpaged><thead><tr><th>Case</th><th>Payer</th><th>Amount</th><th>Status</th><th>Stripe ref</th><th>When</th></tr></thead><tbody id="fin-pay-body">` +
          plist.map(payRow).join("") + `</tbody></table>` +
          `<div id="fin-pay-pg"></div>`;

      // Unified event stream
      const ev = fin.events || [];
      if (ev.length) {
        const evRowsHtml = (list) => list.map((e) => `<tr><td>${badge(e.kind)}</td>
            <td>${e.direction === "IN" ? "↓ in" : e.direction === "OUT" ? "↑ out" : "—"}</td>
            <td>${usd(e.amount_cents)}</td><td>${esc(e.party || "—")}</td>
            <td class="mono">${esc(e.ref || "")}</td><td>${esc(e.actor || "")}</td>
            <td class="muted">${fmtDate(e.created_at)}</td></tr>`).join("");
        const evPage = initClientPager("txn", ev, evRowsHtml);
        html += `<h2>Transaction stream</h2><table id="txn-tb"><thead><tr><th>Event</th><th>Dir</th><th>Amount</th><th>Party</th><th>Reference</th><th>Actor</th><th>When</th></tr></thead>
          <tbody>${evPage.bodyHtml}</tbody></table>${evPage.pagerHtml}`;
      } else {
        html += `<h2>Transaction stream</h2><p class="muted">No financial events yet — issue an invoice to start the stream.</p>`;
      }
      afterRender(() => {
        for (const kind of ["checks", "payments"]) {
          const st = finPager[kind];
          Pager.server(document.getElementById(kind === "checks" ? "fin-ck-pg" : "fin-pay-pg"),
            { total: st.total, shown: st.rows.length }, async (limit, offset) => {
              const r = kind === "checks"
                ? await Api.program.checks("", { limit, offset })
                : await Api.program.payments(null, { limit, offset });
              st.rows = kind === "checks" ? (r.checks || []) : (r.payments || []);
              st.total = r.total ?? st.total;
              document.getElementById(kind === "checks" ? "fin-ck-body" : "fin-pay-body").innerHTML =
                st.rows.map(kind === "checks" ? checkRow : payRow).join("");
              return { total: st.total, shown: st.rows.length };
            });
        }
      });
      return html;
    } catch (e) { return err(e); }
  }

  // finance() pager state — survives only for the rendered page
  let finPager = null;
  let checkRow = null, payRow = null; // row renderers set by finance()

  async function requestRescan(checkId, caseId, btn) {
    await UI.run(btn, async () => {
      try {
        const r = await Api.crm.createTask({
          subject: `Request clearer scan of check ${checkId.slice(0, 8)}… (OCR confidence too low) — send the payer a secure upload link from the case Correspondence panel`,
          case_id: caseId, due_date: new Date(Date.now() + 3 * 864e5).toISOString().slice(0, 10),
        });
        UI.toast(`Rescan task ${r.task_ref || ""} created on the case`.trim());
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Creating task…");
  }

  // Copilot brief (Phase 1) — renders the latest grounded, advisory-only
  // brief plus the (re)generate action. The server returns 502 with facts
  // when the local model is unreachable; surface that via the catch path.
  function copilotCard(brief, generatedAt, caseId) {
    const genBtn = `<button class="mini" onclick="Views.copilotBrief('${caseId}', this)">↻ ${brief ? "Regenerate" : "Generate"} brief</button>`;
    if (!brief)
      return `<p class="muted">No brief generated yet. The copilot drafts a grounded eligibility brief, evidence comparison, and uncertainty list from platform-verified case facts only — it never changes case state.</p><p>${genBtn}</p>`;
    return `<pre class="copilot-brief" style="white-space:pre-wrap;font:inherit;line-height:1.45">${esc(brief)}</pre>
      <p class="muted">Generated ${generatedAt ? new Date(generatedAt).toLocaleString() : "—"} · advisory only, not a determination · audit-logged</p>
      <p>${genBtn}</p>`;
  }

  async function copilotBrief(caseId, btn) {
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.copilotBrief(caseId);
        const b = document.getElementById("copilot");
        if (b) b.innerHTML = copilotCard(r.brief, r.generated_at, caseId);
        UI.toast("Copilot brief generated (DRAFT — advisory only)");
      } catch (e) {
        const b = document.getElementById("copilot");
        if (e.data && e.data.facts) {
          // 502 fallback: model unreachable, server returned verified facts
          if (b) b.innerHTML = copilotCard(null, null, caseId) +
            `<p class="muted">Model unreachable — raw verified facts returned instead.</p>
             <pre style="white-space:pre-wrap;font:12px monospace">${esc(JSON.stringify(e.data.facts, null, 2))}</pre>`;
          UI.toast("Copilot model unreachable — showing verified facts only", { kind: "warn" });
        } else {
          if (b) b.innerHTML = copilotCard(null, null, caseId);
          UI.toast(e.message, { kind: "warn" });
        }
      }
    }, "Drafting brief…");
  }

  // Phase 2: copilot drafts enter the QA gate — accept/edit/reject there.
  async function copilotDraftQA(caseId, kind, btn) {
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.copilotDraft(caseId, kind);
        UI.toast(`${kind === "correspondence" ? "Correspondence" : "Determination rationale"} draft queued for QA review`);
        location.hash = "#/qa";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Drafting for QA…");
  }

  // Phase 3: bounded action batches — propose, list, approve/reject.
  function copilotBatchCard(caseId, b) {
    const acts = (b.actions || []).map((a, i) => {
      const param = a.params ? Object.values(a.params).filter((v) => typeof v === "string").join(" — ") : "";
      const outcome = a.result ? ` <span class="ok">✓ ${esc(a.result)}</span>`
        : a.error ? ` <span class="err">✗ ${esc(a.error)}</span>` : "";
      return `<li><b>${esc(a.type.replace(/_/g, " "))}</b>${param ? ` — ${esc(param)}` : ""}${outcome}</li>`;
    }).join("");
    const pending = b.status === "PENDING_APPROVAL";
    return `<div class="card"><p><b>Batch ${esc(b.batch_id.slice(0, 8))}…</b>
        <span class="badge ${pending ? "s-review" : b.status === "APPLIED" ? "s-ok" : "s-closed"}">${esc(b.status)}</span></p>
      ${b.rationale ? `<p class="muted">${esc(b.rationale)}</p>` : ""}
      <ul>${acts || "<li class='muted'>no actions proposed</li>"}</ul>
      <p class="muted">proposed by ${esc(b.proposed_by)} · model ${esc(b.model)}${b.decided_by ? ` · decided by ${esc(b.decided_by)}` : ""}</p>
      ${pending ? `<div class="actions">
        <button class="mini" onclick="Views.copilotDecideBatch('${caseId}','${b.batch_id}','APPROVE',this)">✓ Approve & execute</button>
        <button class="mini danger" onclick="Views.copilotDecideBatch('${caseId}','${b.batch_id}','REJECT',this)">✗ Reject</button></div>` : ""}
    </div>`;
  }

  async function copilotLoadBatches(caseId) {
    try {
      const r = await Api.program.copilotListActions(caseId);
      const html = (r.batches || []).length
        ? `<h3>Action batches</h3>` + r.batches.map((b) => copilotBatchCard(caseId, b)).join("")
        : "";
      // Render into whichever surface is open — the case page panel and the
      // assistant thread show the same batches off the same endpoints.
      for (const elId of ["copilot-batches", "asst-batches"]) {
        const box = document.getElementById(elId);
        if (box) box.innerHTML = html;
      }
    } catch { /* list is best-effort on load */ }
  }

  async function copilotPropose(caseId, btn) {
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.copilotProposeActions(caseId);
        UI.toast(`Copilot proposed ${(r.actions || []).length} action(s) — review and approve below`);
        copilotLoadBatches(caseId);
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Proposing actions…");
  }

  async function copilotDecideBatch(caseId, batchId, decision, btn) {
    if (decision === "APPROVE" &&
        !(await UI.confirm("Approve and execute?", "The listed actions run now via the workflow — each is applied and recorded on the case.", "Approve & execute"))) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.copilotDecideActions(caseId, batchId, decision);
        if (r.error) UI.toast(r.error, { kind: "warn", sticky: true });
        else UI.toast(decision === "APPROVE" ? "Approved — actions executing via workflow" : "Batch rejected");
        copilotLoadBatches(caseId);
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, decision === "APPROVE" ? "Executing…" : "Rejecting…");
  }

  // ---- Assistant (conversational surface) -----------------------------------
  // The per-case thread over the copilot primitives: grounded Q&A in plain
  // language, with the Phase 1-3 actions as chips — the thread advises, the
  // chips act, and every action still passes its human gate.
  async function assistant(caseId) {
    if (!can("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Assistant</h1></div><p class="muted">Requires a case staff role.</p>`;
    if (!caseId) {
      // Morning briefing (conversation-first step 2): the Assistant home opens
      // with the worker-scoped digest, narrated — the case picker sits below
      // it. The briefing is read-only and never fails: when the model is down
      // the server narrates the digest itself (fallback badge).
      // Wiring runs via the {html, wire} return shape, not afterRender:
      // afterRender's setTimeout(fn, 0) races against the await below --
      // confirmed live, the macrotask fired before view.innerHTML was ever
      // assigned (it only gets assigned once this whole async function's
      // returned promise resolves), so document.getElementById found
      // nothing and the submit listener never attached. wire() is called
      // by app.js synchronously right after the HTML lands, every time.
      const wireHome = () => {
        (async () => {
          const box = document.getElementById("asst-briefing");
          if (!box) return;
          try {
            const r = await Api.program.briefing();
            const d = r.digest || {};
            const stat = (n, label) => n ? `<span class="chip-stat"><b>${n}</b> ${label}</span>` : "";
            const risk = (d.at_risk_sla || []).map((c) =>
              // Deep-link both ways: the case number opens the case SCREEN
              // (docket/documents), the row opens its conversation thread.
              `<tr class="click" onclick="location.hash='#/assistant/${c.case_id}'"><td class="mono"><a href="#/cases/${c.case_id}" onclick="event.stopPropagation()">${esc(c.case_number)}</a></td>
               <td>${badge(c.status)}</td><td>${esc(c.lane)}</td><td class="sla-hot">${c.sla_days_remaining}d left</td></tr>`).join("");
            box.innerHTML = `<div class="asst-turn asst-ai">
              <div class="asst-who">briefing${r.model ? ` · ${esc(r.model)}` : " · structured digest"}</div>
              <div class="asst-body">${esc(r.narration || "")}</div></div>
              <div class="asst-chips" style="margin:8px 0 0 0">
                ${stat(d.my_open_cases, "open assigned")}${stat((d.at_risk_sla || []).length, "SLA risk")}
                ${stat(d.pending_qa, "at QA gate")}${d.checks_in_review ? `<a href="#/finance"><span class="chip-stat"><b>${d.checks_in_review}</b> checks in review ↗</span></a>` : ""}
                ${stat(d.new_docs_24h, "docs analyzed 24h")}${stat((d.tasks_due || []).length, "tasks due")}
              </div>
              ${risk ? `<h3 style="margin:10px 0 4px">SLA risk — open a thread to act</h3>
                <table><thead><tr><th>Case</th><th>Status</th><th>Lane</th><th>SLA</th></tr></thead><tbody>${risk}</tbody></table>` : ""}`;
          } catch (e) {
            box.innerHTML = `<p class="muted">Briefing unavailable: ${esc(e.message)}</p>`;
          }
        })();
        document.getElementById("asst-general-form")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const input = ev.target.message;
          const msg = input.value;
          input.value = "";
          if (msg.trim()) await handleAssistantMessage(msg, "", "asst-general-thread");
        });
        (async () => {
          const gthread = document.getElementById("asst-general-thread");
          if (!gthread) return;
          try {
            const h = await Api.program.copilotGeneralChatHistory();
            gthread.innerHTML = (h.turns || []).map(asstTurnHtml).join("") || ASST_EMPTY_HTML;
            gthread.scrollTop = gthread.scrollHeight;
          } catch (e) { gthread.innerHTML = `<div class="muted" style="padding:12px">${esc(e.message)}</div>`; }
        })();
      };
      try {
        const r = await Api.cases.list({ limit: 50 });
        const rows = (r.cases || []).map((c) =>
          `<tr class="click" onclick="location.hash='#/assistant/${c.id}'"><td class="mono"><a href="#/cases/${c.id}" onclick="event.stopPropagation()">${esc(c.case_number)}</a></td>
           <td>${esc(c.service_line || "")}</td><td>${badge(c.status)}</td></tr>`).join("");
        return { html: `<div class="view-head"><h1>Assistant</h1>
          <span class="muted">grounded on platform-verified case facts · advisory only · every turn is on the record</span></div>
          <div class="asst-panel" style="margin-bottom:14px">
            <div id="asst-briefing"><p class="muted">Preparing your briefing…</p></div>
            <div id="asst-general-thread" class="asst-thread" style="margin-top:10px"><p class="muted" style="padding:12px">Loading…</p></div>
            <div class="asst-chips">
              <button class="mini" type="button" onclick="Views.handleAssistantMessage('my open cases','','asst-general-thread')">📂 My open cases</button>
              <button class="mini" type="button" onclick="Views.handleAssistantMessage('my tasks','','asst-general-thread')">✓ My tasks</button>
            </div>
            <form id="asst-general-form" class="asst-form">
              <input name="message" autocomplete="off" placeholder="Ask anything — my open cases, my tasks, open a new intake…" aria-label="Message the assistant" />
              <button>Send</button>
            </form>
          </div>
          <h2 style="margin-top:14px">Case threads</h2>
          <p class="muted">Pick a case to open its thread — private to you, shareable if you choose.</p>
          <table><thead><tr><th>Case</th><th>Line</th><th>Status</th></tr></thead><tbody>${rows}</tbody></table>`, wire: wireHome };
      } catch (e) { return err(e); }
    }
    // Same {html, wire} shape as the no-case branch above, for the same
    // reason -- this happened to work before only because nothing awaited
    // between here and the old afterRender() call, which made the ordering
    // an accident of this function's current shape rather than guaranteed.
    const wireCase = async () => {
      const thread = document.getElementById("asst-thread");
      try {
        const c = await Api.cases.get(caseId);
        document.getElementById("asst-case").innerHTML =
          `Case <a href="#/cases/${caseId}">${esc(c.case_number)}</a> · ${esc(c.status)}`;
        const h = await Api.program.copilotChatHistory(caseId);
        thread.innerHTML = (h.turns || []).map(asstTurnHtml).join("") || ASST_EMPTY_HTML;
        thread.scrollTop = thread.scrollHeight;
      } catch (e) {
        thread.innerHTML = `<div class="muted" style="padding:12px">${esc(e.message)}</div>`;
      }
      const form = document.getElementById("asst-form");
      const fileInput = document.getElementById("asst-file");
      fileInput?.addEventListener("change", () => {
        form.querySelector(".asst-fab-attach")?.classList.toggle("has-file", !!fileInput.files[0]);
      });
      form?.addEventListener("submit", async (e) => {
        e.preventDefault();
        const input = form.message;
        const msg = input.value;
        input.value = "";
        const file = fileInput?.files[0];
        if (file) {
          fileInput.value = "";
          form.querySelector(".asst-fab-attach")?.classList.remove("has-file");
          await asstDoUpload("asst-thread", caseId, file);
        }
        if (msg.trim()) await handleAssistantMessage(msg, caseId, "asst-thread");
      });
      asstQaLoad(caseId);
      asstShareLoad(caseId);
      copilotLoadBatches(caseId); // pending action batches render inline — decide without leaving the thread
    };
    return { html: `<div class="view-head"><h1>Assistant</h1>
      <span class="muted" id="asst-case">loading case…</span></div>
      <div class="asst-panel">
        <div id="asst-share" style="margin-bottom:10px"></div>
        <div id="asst-thread" class="asst-thread"></div>
        <div id="asst-qa"></div>
        <div id="asst-batches"></div>
        <div id="asst-tools"></div>
        <div class="asst-chips">
          <button class="mini" onclick="Views.assistantTool('${caseId}','docs')">📄 Documents</button>
          <button class="mini" onclick="Views.assistantTool('${caseId}','checklist')">☑ Stage checklist</button>
          <button class="mini" onclick="Views.assistantTool('${caseId}','mail')">✉ Send mail</button>
          <button class="mini" onclick="Views.assistantTool('${caseId}','checks')">🧾 Manual checks</button>
          <button class="mini" onclick="Views.assistantTool('${caseId}','time')">⏱ Log time</button>
        </div>
        <div class="asst-chips">
          <button class="mini" onclick="Views.assistantChip('${caseId}','brief',this)">▤ Brief me</button>
          <button class="mini" onclick="Views.assistantChip('${caseId}','determination_rationale',this)">✍ Draft rationale</button>
          <button class="mini" onclick="Views.assistantChip('${caseId}','correspondence',this)">✉ Draft correspondence</button>
          <button class="mini" onclick="Views.assistantChip('${caseId}','actions',this)">⚙ Propose actions</button>
        </div>
        <form id="asst-form" class="asst-form">
          <label class="mini asst-fab-attach" title="Attach a document to this case">📎
            <input type="file" id="asst-file" style="display:none" /></label>
          <input name="message" autocomplete="off" placeholder="Ask about this case, or tell me to do something — create a task, open an intake, pull a report…" aria-label="Message the assistant" />
          <button>Send</button>
        </form>
        <p class="asst-foot">Chips and direct requests (create a task, open an intake, pull a report, upload a document) execute immediately — correspondence still needs a second reviewer, same as everywhere else in the app. This thread is private to you by default; share it above if you want a colleague to see it. Every turn is recorded. Replies run on local infrastructure and can take up to a couple of minutes.</p>
      </div>`, wire: wireCase };
  }

  const ASST_EMPTY_HTML = `<div class="asst-empty"><span class="asst-empty-icon">❖</span>
    <span>No turns yet — ask anything about this case, or use a quick action below.</span></div>`;

  function asstTurnHtml(t) {
    const who = t.role === "user" ? "you" : `assistant${t.model ? ` · ${esc(t.model)}` : ""}`;
    return `<div class="asst-turn ${t.role === "user" ? "asst-user" : "asst-ai"}">
      <div class="asst-who">${who}</div>
      <div class="asst-body">${esc(t.body)}</div></div>`;
  }

  function asstAppend(threadId, role, body, model) {
    const thread = document.getElementById(threadId);
    if (!thread) return;
    thread.querySelector(".asst-empty")?.remove();
    thread.insertAdjacentHTML("beforeend", asstTurnHtml({ role, body, model }));
    thread.scrollTop = thread.scrollHeight;
  }

  // A literal "…" read as frozen/broken on a model this slow (confirmed
  // live: a real brief took 92s) -- an animated indicator + an explicit
  // time-expectation stops that from looking like a hang.
  function asstThinking(threadId) {
    const thread = document.getElementById(threadId);
    if (!thread) return;
    thread.querySelector(".asst-empty")?.remove();
    thread.insertAdjacentHTML("beforeend",
      `<div class="asst-turn asst-ai asst-thinking"><span class="asst-dots"><span></span><span></span><span></span></span>
       <span class="asst-wait-hint">thinking — can take up to ~2 min on this model</span></div>`);
    thread.scrollTop = thread.scrollHeight;
  }

  async function assistantSend(caseId, msg, threadId = "asst-thread") {
    asstAppend(threadId, "user", msg);
    asstThinking(threadId);
    try {
      const r = await Api.program.copilotChat(caseId, msg);
      document.getElementById(threadId)?.querySelector(".asst-turn:last-child")?.remove();
      asstAppend(threadId, "assistant", r.reply, r.model);
    } catch (e) {
      document.getElementById(threadId)?.querySelector(".asst-turn:last-child")?.remove();
      asstAppend(threadId, "assistant", `⚠ ${e.message}`, "");
    }
  }

  // Same shape as assistantSend, grounded on the worker's own open cases/
  // tasks instead of one case's record (copilot_general_chat.go) — the
  // floating widget's fallback for anything that isn't one of its fast,
  // deterministic intents and isn't asked with a case open.
  async function generalChatSend(msg, threadId) {
    asstAppend(threadId, "user", msg);
    asstThinking(threadId);
    try {
      const r = await Api.program.copilotGeneralChat(msg);
      document.getElementById(threadId)?.querySelector(".asst-turn:last-child")?.remove();
      asstAppend(threadId, "assistant", r.reply, r.model);
    } catch (e) {
      document.getElementById(threadId)?.querySelector(".asst-turn:last-child")?.remove();
      asstAppend(threadId, "assistant", `⚠ ${e.message}`, "");
    }
  }

  // Chips run the Phase 1-3 primitives and narrate the outcome into the
  // thread — same endpoints, same gates, conversational surface. threadId
  // lets the same chip logic serve both the full #/assistant page and the
  // floating site-wide widget (mountAssistantFab) without duplicating it.
  async function assistantChip(caseId, kind, btn, threadId = "asst-thread") {
    await UI.run(btn, async () => {
      const chipLabel = kind === "brief" ? "Brief me on this case."
        : kind === "actions" ? "Propose an action batch."
        : kind === "correspondence" ? "Draft correspondence." : "Draft a determination rationale.";
      asstAppend(threadId, "user", chipLabel);
      asstThinking(threadId);
      const popThinking = () => document.getElementById(threadId)?.querySelector(".asst-turn:last-child")?.remove();
      try {
        if (kind === "brief") {
          const r = await Api.program.copilotBrief(caseId);
          popThinking();
          asstAppend(threadId, "assistant", r.brief, r.model || "");
        } else if (kind === "actions") {
          const r = await Api.program.copilotProposeActions(caseId);
          popThinking();
          const acts = (r.actions || []).map((a) => `• ${a.type.replace(/_/g, " ")}`).join("\n") || "• (none)";
          asstAppend(threadId, "assistant",
            `Proposed ${(r.actions || []).length} action(s) — review and approve in the batch card below, without leaving this thread:\n${r.rationale || ""}\n${acts}`, r.model || "");
          copilotLoadBatches(caseId);
        } else {
          const r = await Api.program.copilotDraft(caseId, kind);
          popThinking();
          asstAppend(threadId, "assistant",
            `Draft queued in the QA gate (${r.subject}). Approve, edit, or reject it there — nothing is sent or filed automatically.`, "");
        }
      } catch (e) {
        popThinking();
        asstAppend(threadId, "assistant", `⚠ ${e.message}`, "");
      }
    }, "Working…");
  }

  // Floating assistant launcher — called once from app.js after login so the
  // same thread is reachable from anywhere, not just #/assistant. Grounds on
  // whatever case the URL currently points at (#/cases/:id or
  // #/assistant/:id); off a case page it offers a quick case search instead
  // of going ungrounded. Reuses assistantChip/assistantSend/asstTurnHtml via
  // the threadId param so this is the same code path as the full page, not
  // a parallel implementation that can drift from it.
  // Patterns the floating widget answers itself, fast and deterministically,
  // instead of routing to the case-scoped LLM (which is grounded on ONE
  // case and, confirmed live, can take 60-90s+ on this hardware). "My open
  // cases" / "my tasks" are platform-wide questions the LLM endpoint
  // can't even answer (it has no cross-case context) -- these were never
  // going to work as an LLM prompt, they're just REST list calls.
  const ASST_INTENT_CASES = /\b(open cases|my cases|cases?.*(today|due|open)|what cases)\b/i;
  const ASST_INTENT_TASKS = /\b(my )?tasks?\b/i;
  const ASST_INTENT_LEFT = /\b(what.?s left|whats left|outstanding|still (need|required)|what.?s (missing|remaining)|checklist)\b/i;
  // Action intents (full autonomy, user-directed): these actually WRITE --
  // create a task, complete one, open an intake, pull a report -- executed
  // directly on send, no separate confirm click. Checked before
  // ASST_INTENT_TASKS (list) so "create a task" doesn't get read as "show
  // my tasks". Deliberately NOT extended to anything needing a second
  // reviewer by platform design (sending correspondence) -- that gate
  // exists independent of who/what drafts it, so it stays in force here.
  const ASST_INTENT_TASK_CREATE = /\b(create|add|log|make)\b[^.?!]{0,8}\btask\b|^remind me to\b/i;
  const ASST_INTENT_TASK_COMPLETE = /\b(complete|finish|close|mark)\b[^.?!]{0,15}\btask\b|\btask\b[^.?!]{0,15}\b(done|complete|finished)\b/i;
  const ASST_INTENT_INTAKE_CREATE = /\b(create|open|start|file)\b[^.?!]{0,10}\b(intake|dispute)\b|^new (dispute|intake|case)\b/i;
  const ASST_INTENT_REPORT = /\b(report|receivables|financial summary|sla breach(es)?)\b/i;
  // "send/write/draft correspondence" routes through the SAME copilotDraft
  // chip logic as clicking "Draft correspondence" -- real AI-generated
  // content grounded in case facts, landing in the QA gate exactly like a
  // human-drafted one. There's no execute-immediately path for correspondence
  // triggered by free text any more than there is from the chip itself --
  // the second-reviewer requirement is a property of the action, not of how
  // it was requested.
  // "send them an upload link" has no "email"/"correspondence"/"letter"
  // in it but is exactly a correspondence-draft request (confirmed live:
  // this exact phrasing fell through to the ungrounded general chat,
  // which correctly said it couldn't do it, since nothing routed it
  // anywhere that could).
  const ASST_INTENT_DRAFT_CORR = /\b(draft|write|send|compose)\b[^.?!]{0,15}\b(correspondence|email|letter)\b|\b(upload|share)[\s-]?link\b/i;
  const ASST_INTENT_DRAFT_RATIONALE = /\b(draft|write)\b[^.?!]{0,15}\b(rationale|determination)\b/i;
  const ASST_CASE_NUM_RE = /\b[A-Za-z]{2,4}\d{2,4}-\d{2,6}\b/;

  // Lets every caseId-gated branch below be reached from the GENERAL
  // assistant thread (no case open) by just naming the case in the
  // message -- confirmed live: "send them an upload link ... for case
  // FL26-181" typed from #/assistant (no case open) had no way to reach
  // that case's actions at all, and silently fell through to the
  // ungrounded general chat instead. Exact case_number match only (never
  // "closest match") -- grounding on the wrong case silently is worse
  // than not grounding at all.
  async function asstResolveCaseId(msg) {
    const m = msg.match(ASST_CASE_NUM_RE);
    if (!m) return "";
    try {
      const r = await Api.cases.list({ q: m[0], limit: 5 });
      const hit = (r.cases || []).find((c) => (c.case_number || "").toUpperCase() === m[0].toUpperCase());
      return hit ? hit.id : "";
    } catch { return ""; }
  }

  function asstAppendRich(threadId, titleHtml, rowsHtml) {
    const thread = document.getElementById(threadId);
    if (!thread) return;
    thread.querySelector(".asst-empty")?.remove();
    thread.querySelector(".asst-turn.asst-thinking")?.remove();
    thread.insertAdjacentHTML("beforeend", `<div class="asst-turn asst-ai">
      <div class="asst-who">assistant</div>
      <div class="asst-body">${titleHtml}</div>
      ${rowsHtml ? `<div class="asst-fab-list" style="margin-top:8px">${rowsHtml}</div>` : ""}
      </div>`);
    thread.scrollTop = thread.scrollHeight;
  }

  // Each asstAnswer*/asstDo* fast-path below returns the plain-text summary
  // it rendered (or null on a caught error, which is already shown inline
  // and isn't worth persisting) so handleAssistantMessage can log the turn
  // -- see copilotChatLog's comment for why that log call exists at all.
  async function asstAnswerMyCases(threadId) {
    try {
      const r = await Api.cases.list({ mine: "true", sort: "sla", limit: 8 });
      const rows = r.cases || [];
      if (!rows.length) { asstAppendRich(threadId, "No cases are currently assigned to you."); return "No cases are currently assigned to you."; }
      const rowsHtml = rows.map((c) => `<a class="asst-fab-list-row" href="#/cases/${c.id}">
        <span class="afr-main"><b class="mono">${esc(c.case_number)}</b>
          <span class="afr-sub">${esc(c.service_line || "—")}</span></span>
        ${badge(c.status)}<span class="afr-sub">${c.sla_days_remaining}d left</span></a>`).join("");
      const summary = `${rows.length} case${rows.length === 1 ? "" : "s"} assigned to you, soonest deadline first:`;
      asstAppendRich(threadId, summary, rowsHtml);
      return summary + " " + rows.map((c) => `${c.case_number} (${c.status}, ${c.sla_days_remaining}d left)`).join("; ");
    } catch (e) { asstAppendRich(threadId, `⚠ ${esc(e.message)}`); return null; }
  }

  async function asstAnswerMyTasks(threadId) {
    try {
      const r = await Api.crm.tasks(true, { limit: 8 });
      const rows = (r.tasks || []).filter((t) => t.status !== "DONE");
      if (!rows.length) { asstAppendRich(threadId, "No open tasks assigned to you."); return "No open tasks assigned to you."; }
      const rowsHtml = rows.map((t) => `<a class="asst-fab-list-row" href="${t.case_id ? `#/cases/${t.case_id}` : "#/crm/tasks"}">
        <span class="afr-main"><b>${esc(t.subject)}</b><span class="afr-sub">${esc(t.task_ref || "")}</span></span>
        ${badge(t.status)}<span class="afr-sub">${t.due_date ? "due " + esc(t.due_date) : "no due date"}</span></a>`).join("");
      const summary = `${rows.length} open task${rows.length === 1 ? "" : "s"} assigned to you:`;
      asstAppendRich(threadId, summary, rowsHtml);
      return summary + " " + rows.map((t) => `${t.subject} (${t.task_ref || "no ref"}${t.due_date ? ", due " + t.due_date : ""})`).join("; ");
    } catch (e) { asstAppendRich(threadId, `⚠ ${esc(e.message)}`); return null; }
  }

  async function asstAnswerWhatsLeft(threadId, caseId) {
    if (!caseId) {
      asstAppendRich(threadId, "Open a case first — I'll read its checklist from there.");
      return null;
    }
    try {
      const items = await Api.cm.checklist(caseId);
      const open_ = (items || []).filter((i) => !i.done);
      if (!open_.length) { asstAppendRich(threadId, "Nothing outstanding — every checklist item on this case is done."); return "Nothing outstanding — every checklist item on this case is done."; }
      const byStage = {};
      open_.forEach((i) => { (byStage[i.stage] ||= []).push(i); });
      const rowsHtml = Object.entries(byStage).map(([stage, items2]) =>
        `<div class="asst-fab-list-row" style="background:none;padding:4px 0 0"><b>${esc(stage)}</b></div>` +
        items2.map((i) => `<div class="asst-fab-list-row"><span class="afr-main">${esc(i.item)}</span>
          ${i.required ? badge("REQUIRED") : ""}</div>`).join("")
      ).join("");
      const summary = `${open_.length} outstanding checklist item${open_.length === 1 ? "" : "s"}:`;
      asstAppendRich(threadId, summary, rowsHtml);
      return summary + " " + open_.map((i) => `${i.item} (${i.stage}${i.required ? ", required" : ""})`).join("; ");
    } catch (e) { asstAppendRich(threadId, `⚠ ${esc(e.message)}`); return null; }
  }

  // Lightweight slot extraction, not an LLM call -- a task's subject/due
  // date are simple enough that a lookup table of trigger phrases beats
  // paying ~60-90s of Ollama latency for something a regex gets right.
  function extractTaskFields(msg) {
    let subject = msg
      .replace(/^(please\s+)?(create|add|log|make)\s+(a\s+)?task\s*(to|for|:|-)?\s*/i, "")
      .replace(/^remind me to\s*/i, "")
      .trim();
    let due = null;
    const explicit = subject.match(/\b(by|due|before)\s+(\d{4}-\d{2}-\d{2})\b/i);
    if (explicit) {
      due = explicit[2];
      subject = (subject.slice(0, explicit.index) + subject.slice(explicit.index + explicit[0].length)).trim();
    } else if (/\btomorrow\b/i.test(subject)) {
      const d = new Date(); d.setDate(d.getDate() + 1);
      due = d.toISOString().slice(0, 10);
      subject = subject.replace(/\btomorrow\b/i, "").trim();
    } else if (/\btoday\b/i.test(subject)) {
      due = new Date().toISOString().slice(0, 10);
      subject = subject.replace(/\btoday\b/i, "").trim();
    }
    subject = subject.replace(/\s{2,}/g, " ").replace(/^[-:\s]+|[-:\s]+$/g, "").trim();
    return { subject: subject || msg.trim(), due };
  }

  async function asstDoCreateTask(threadId, msg, caseId) {
    const { subject, due } = extractTaskFields(msg);
    try {
      const me = Auth.claims();
      const r = await Api.crm.createTask({
        subject, case_id: caseId || "", assignee: me?.sub || "", due_date: due || "",
      });
      const summary = `✓ Task created: ${subject}${due ? ` (due ${due})` : ""}${r.task_ref ? ` — ${r.task_ref}` : ""}`;
      asstAppendRich(threadId,
        `✓ Task created: <b>${esc(subject)}</b>${due ? ` (due ${esc(due)})` : ""}${r.task_ref ? ` — ${esc(r.task_ref)}` : ""}`);
      return summary;
    } catch (e) { asstAppendRich(threadId, `⚠ ${esc(e.message)}`); return null; }
  }

  async function asstDoCompleteTask(threadId, msg) {
    try {
      const refMatch = msg.match(/TASK-\d{4}-\d{5}/i);
      const r = await Api.crm.tasks(true, { limit: 100 });
      let candidates = (r.tasks || []).filter((t) => t.status !== "DONE");
      if (refMatch) {
        candidates = candidates.filter((t) => (t.task_ref || "").toUpperCase() === refMatch[0].toUpperCase());
      } else {
        const kw = msg.replace(/\b(complete|finish|close|mark|done|finished|with|the|task|my)\b/gi, "").trim().toLowerCase();
        if (kw) candidates = candidates.filter((t) => t.subject.toLowerCase().includes(kw));
      }
      if (!candidates.length) {
        const msg2 = "Couldn't find a matching open task of yours — try including part of its exact title or its TASK-#### reference.";
        asstAppendRich(threadId, msg2);
        return msg2;
      }
      if (candidates.length > 1) {
        const rowsHtml = candidates.slice(0, 6).map((t) =>
          `<div class="asst-fab-list-row"><span class="afr-main">${esc(t.subject)}</span><span class="afr-sub">${esc(t.task_ref)}</span></div>`).join("");
        const summary = "Multiple open tasks match — tell me the exact reference (e.g. TASK-2026-00042):";
        asstAppendRich(threadId, summary, rowsHtml);
        return summary + " " + candidates.slice(0, 6).map((t) => `${t.subject} (${t.task_ref})`).join("; ");
      }
      await Api.crm.completeTask(candidates[0].id);
      const summary = `✓ Marked done: ${candidates[0].subject} (${candidates[0].task_ref})`;
      asstAppendRich(threadId, `✓ Marked done: <b>${esc(candidates[0].subject)}</b> (${esc(candidates[0].task_ref)})`);
      return summary;
    } catch (e) { asstAppendRich(threadId, `⚠ ${esc(e.message)}`); return null; }
  }

  // Multi-turn, so module-scoped: a brand-new case gets opened only once
  // intakeConverse's own extraction says every required field is present
  // (r.ready) -- auto-submitting instead of waiting for a button click.
  //
  // The card below is read-only by design -- nobody clicks into it, every
  // field only ever changes because the conversation said so. It renders
  // once (stable id, keyed to the thread) and gets its innerHTML replaced
  // in place on every turn, so filling it out reads as live rather than as
  // a new message appearing each time you add a detail.
  let asstIntakeDraft = null;
  function asstIntakeCardHtml(fields, bodyText, statusHtml) {
    const row = (label, val) => `<div class="asst-fab-list-row"><span class="afr-main">${esc(label)}</span><span class="afr-sub">${val ? esc(val) : "—"}</span></div>`;
    return `<div class="asst-who">assistant</div>
      <div class="asst-body">${esc(bodyText)}</div>
      <div class="asst-fab-list" style="margin-top:8px">
        ${row("Email", fields.email)}${row("Contact", fields.contact_name)}${row("Organization", fields.org)}
      </div>
      ${statusHtml || ""}`;
  }
  async function asstDoCreateIntake(threadId, msg) {
    const cardId = `${threadId}-intake-card`;
    if (!asstIntakeDraft) asstIntakeDraft = { fields: {}, history: [] };
    const thread = document.getElementById(threadId);
    thread?.querySelector(".asst-turn.asst-thinking")?.remove();
    thread?.querySelector(".asst-empty")?.remove();
    let card = document.getElementById(cardId);
    if (!card) {
      thread?.insertAdjacentHTML("beforeend", `<div class="asst-turn asst-ai" id="${cardId}"></div>`);
      card = document.getElementById(cardId);
    }
    const thinkingHtml = `<p class="asst-wait-hint" style="display:flex;align-items:center;gap:6px;margin:6px 0 0">
      <span class="asst-dots"><span></span><span></span><span></span></span> thinking…</p>`;
    card.innerHTML = asstIntakeCardHtml(asstIntakeDraft.fields,
      "New intake — read-only, filled in from what you tell me.", thinkingHtml);
    if (thread) thread.scrollTop = thread.scrollHeight;
    try {
      const r = await Api.program.intakeConverse(msg, asstIntakeDraft.fields, asstIntakeDraft.history);
      asstIntakeDraft.history.push(msg);
      asstIntakeDraft.fields = r.fields || {};
      const fields = asstIntakeDraft.fields;
      const bodyText = r.reply || "New intake — read-only, filled in from what you tell me.";
      if (r.ready) {
        card.innerHTML = asstIntakeCardHtml(fields, bodyText, `<p class="asst-wait-hint" style="margin:6px 0 0">✓ ready — filing…</p>`);
        asstIntakeDraft = null;
        const created = await Api.program.createIntake({
          email: fields.email, contact_name: fields.contact_name, org: fields.org,
        });
        const successMsg = created.programmed
          ? `✓ Case <a href="#/cases/${created.case_id}">${esc(created.case_number)}</a> opened — filing instructions emailed to ${esc(fields.email || "")}.`
          : "✓ Intake request opened — submission instructions queued.";
        card.innerHTML = asstIntakeCardHtml(fields, bodyText, `<p style="margin:6px 0 0;font-weight:600">${successMsg}</p>`);
      } else {
        const missHtml = (r.missing || []).length
          ? `<p class="muted" style="margin:6px 0 0;font-size:11.5px">Still needed: ${esc(r.missing.join(", "))}</p>` : "";
        card.innerHTML = asstIntakeCardHtml(fields, bodyText, missHtml);
      }
    } catch (e) {
      card.innerHTML = asstIntakeCardHtml(asstIntakeDraft?.fields || {}, "New intake",
        `<p style="margin:6px 0 0;color:var(--danger-fg)">⚠ ${esc(e.message)}</p>`);
      asstIntakeDraft = null;
    }
    if (thread) thread.scrollTop = thread.scrollHeight;
  }

  async function asstDoReport(threadId, msg) {
    try {
      let title, rowsHtml;
      if (/receivable/i.test(msg)) {
        const r = await Api.program.receivables();
        const rows = r.receivables || [];
        title = "Receivables (by party/kind/status):";
        rowsHtml = rows.slice(0, 10).map((x) =>
          `<div class="asst-fab-list-row"><span class="afr-main">${esc(x.party)} · ${esc(x.kind)}</span><span class="afr-sub">${esc(x.status)} · ${x.n} · $${((x.total_cents || 0) / 100).toLocaleString()}</span></div>`).join("");
      } else if (/\bsla\b/i.test(msg)) {
        const r = await Api.reports.sla();
        title = `${(r || []).length} SLA breach(es) on record:`;
        rowsHtml = (r || []).slice(0, 10).map((x) =>
          `<div class="asst-fab-list-row"><a class="afr-main" href="#/cases/${x.case_id}">${esc(x.clock)}</a><span class="afr-sub">${esc(x.detail || "")}</span></div>`).join("");
      } else if (/financial/i.test(msg)) {
        const r = await Api.program.financial();
        const k = r.kpi || {};
        title = "Financial summary:";
        rowsHtml = `<div class="asst-fab-list-row"><span class="afr-main">Collected</span><span class="afr-sub">$${((k.collected_cents || 0) / 100).toLocaleString()}</span></div>
          <div class="asst-fab-list-row"><span class="afr-main">Collected (30d)</span><span class="afr-sub">$${((k.collected_30d_cents || 0) / 100).toLocaleString()}</span></div>
          <div class="asst-fab-list-row"><span class="afr-main">Refunded</span><span class="afr-sub">$${((k.refunded_cents || 0) / 100).toLocaleString()}</span></div>
          <div class="asst-fab-list-row"><span class="afr-main">Payments</span><span class="afr-sub">${k.payments_count || 0}</span></div>`;
      } else {
        const r = await Api.reports.summary();
        title = "Case status summary:";
        rowsHtml = (r || []).map((x) =>
          `<div class="asst-fab-list-row"><span class="afr-main">${esc(x.status)}</span><span class="afr-sub">${x.count} · avg ${esc(x.amount_label)} $${Math.round(x.avg_amount_usd || 0).toLocaleString()}</span></div>`).join("");
      }
      asstAppendRich(threadId, title, rowsHtml || `<p class="muted" style="margin:0">No data.</p>`);
      return title;
    } catch (e) { asstAppendRich(threadId, `⚠ ${esc(e.message)}`); return null; }
  }

  // Free-text correspondence requests route through the SAME copilotDraft
  // LLM-drafted path as the "Draft correspondence" chip -- it lands in the
  // human QA gate exactly like a chip-triggered or human-typed draft; the
  // second-reviewer requirement is a property of sending mail, not of how
  // the draft was requested. Two things the chip's fixed label can't do,
  // which free text can: pull a recipient address straight out of the
  // message, and -- when the ask is specifically for an upload/share link --
  // mint a REAL one first (same endpoint behind the case page's "share
  // link" button) and hand the model the exact URL to drop in verbatim,
  // instead of asking a small local model to either invent one or leave a
  // placeholder. The reviewer still checks the draft before it sends,
  // same as everywhere else.
  async function asstDoDraftCorrespondence(threadId, caseId, msg) {
    const to = msg.match(/[\w.+-]+@[\w-]+\.[\w.-]+/g) || [];
    let instructions = msg;
    if (/\b(upload|share)[\s-]?link\b/i.test(msg)) {
      try {
        const link = await Api.program.shareLink(caseId, "upload", 7);
        const full = location.origin + link.path;
        instructions += `\n\nInclude this exact secure upload link verbatim, character for character -- do not alter, shorten, or retype it: ${full}`;
      } catch (e) {
        instructions += `\n\n(Could not mint a real upload link -- ${e.message}. Note this for the reviewer rather than inventing a link.)`;
      }
    }
    try {
      const r = await Api.program.copilotDraft(caseId, "correspondence", instructions, to);
      const summary = `✉ Draft queued in the QA gate (${r.subject || "correspondence"})${to.length ? ` — to ${to.join(", ")}` : ""}. Approve, edit, or reject it there — nothing is sent automatically.`;
      asstAppendRich(threadId,
        `✉ Draft queued in the QA gate (${esc(r.subject || "correspondence")})${to.length ? ` — to ${esc(to.join(", "))}` : ""}. Approve, edit, or reject it there — nothing is sent automatically.`);
      return summary;
    } catch (e) { asstAppendRich(threadId, `⚠ ${esc(e.message)}`); return null; }
  }

  async function asstDoUpload(threadId, caseId, file) {
    asstAppend(threadId, "user", `📎 ${file.name}`);
    asstThinking(threadId);
    try {
      await Api.cases.upload(caseId, file);
      asstAppendRich(threadId, `✓ Uploaded <b>${esc(file.name)}</b> — analysis queued; fields (QPA, service line, eligibility inputs) backfill automatically once it finishes.`);
    } catch (e) { asstAppendRich(threadId, `⚠ ${esc(e.message)}`); }
  }

  // ---- Case-scoped tool panel (ported from munisp's parallel branch,
  // adapted to this file's threadId-aware helpers) -- conversational reach
  // into docs/checklist/mail/manual-checks without leaving the case thread.
  // Only ever rendered from the case-scoped Assistant page, hence the
  // literal "asst-thread" threadId rather than a parameter -- the floating
  // widget and the general thread have no case-scoped tool panel to render
  // into.
  async function assistantTool(caseId, tool) {
    const box = document.getElementById("asst-tools");
    if (!box) return;
    if (box.dataset.open === tool) { box.innerHTML = ""; box.dataset.open = ""; return; }
    box.dataset.open = tool;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      if (tool === "docs") box.innerHTML = await asstDocsHtml(caseId);
      else if (tool === "checklist") box.innerHTML = await asstChecklistHtml(caseId);
      else if (tool === "mail") box.innerHTML = await asstMailHtml(caseId);
      else if (tool === "checks") box.innerHTML = await asstChecksHtml();
      else if (tool === "time") box.innerHTML = await asstTimeHtml(caseId);
    } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
  }

  // Documents: docket status at a glance, per-document download, and the
  // party-request flow (mint a secure upload link) — upload/move/deep
  // inspection stay on the case screen via the deep-link.
  async function asstDocsHtml(caseId) {
    const docs = await Api.cases.documents(caseId);
    const byFolder = {};
    (docs || []).forEach((d) => { byFolder[d.folder || "GENERAL"] = (byFolder[d.folder || "GENERAL"] || 0) + 1; });
    const pending = (docs || []).filter((d) => d.analysis_status && !["COMPLETE", "FAILED", "SKIPPED", ""].includes(d.analysis_status));
    const rows = (docs || []).map((d) => `<tr>
      <td class="mono">${esc(d.filename || d.doc_id.slice(0, 8) + "…")}</td>
      <td>${esc(d.folder || "GENERAL")}</td>
      <td>${d.sealed ? '<span class="badge s-sealed">🔒 sealed</span>' : badge(d.analysis_status || "—")}</td>
      <td><a href="javascript:void(0)" onclick="Views.downloadDoc('${caseId}','${d.doc_id}')">download</a></td></tr>`).join("");
    return `<div class="card"><h3>📄 Documents — ${(docs || []).length} on the docket</h3>
      <p class="muted">${Object.entries(byFolder).map(([f, n]) => `${esc(f)} ${n}`).join(" · ") || "none yet"}${pending.length ? ` · ⚠ ${pending.length} awaiting analysis` : ""}</p>
      ${rows ? `<table><thead><tr><th>File</th><th>Folder</th><th>Analysis</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : ""}
      <div class="actions">
        <button class="mini" onclick="Views.asstRequestUpload('${caseId}',this)">🔗 Request party upload link</button>
        <a class="button mini" href="#/cases/${caseId}">Open full docket ↗</a>
      </div></div>`;
  }

  // Mint the upload link and narrate it into the thread so the worker can
  // paste it into correspondence (or use {share_link} in Send mail below).
  async function asstRequestUpload(caseId, btn) {
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.shareLink(caseId, "upload", 7);
        const full = location.origin + r.path;
        asstAppend("asst-thread", "user", "Request a party upload link.");
        asstAppend("asst-thread", "assistant", `Secure upload link minted (expires in 7 days):\n${full}\n\nSend it via Send mail — the {share_link} placeholder mints one automatically on send.`, "");
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Minting link…");
  }

  // Stage checklist: same items as the case screen, checkable in place via
  // the same checkItem endpoint — the thread never rerenders away.
  async function asstChecklistHtml(caseId) {
    const items = await Api.cm.checklist(caseId);
    const stages = {};
    (items || []).forEach((i) => { (stages[i.stage] = stages[i.stage] || []).push(i); });
    const total = (items || []).length, done = (items || []).filter((i) => i.done).length;
    const canCheck = can("CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN");
    return `<div class="card"><h3>☑ Stage checklists — ${done} of ${total} complete</h3>` +
      Object.entries(stages).map(([stage, its]) =>
        `<h4 style="margin:10px 0 4px">${esc(stage)}</h4><ul class="checklist">` + its.map((i) =>
          `<li>${i.done ? "✅" : canCheck
            ? `<button class="mini" onclick="Views.asstCheck('${caseId}','${i.id}',this)">check</button>` : "⬜"}
            ${esc(i.item)}${i.required ? " *" : ""}${i.done_by ? ` <span class="muted">(${esc(i.done_by)})</span>` : ""}</li>`).join("") +
        `</ul>`).join("") + `</div>`;
  }

  async function asstCheck(caseId, itemId, btn) {
    await UI.run(btn, async () => {
      try {
        await Api.cm.checkItem(itemId);
        const box = document.getElementById("asst-tools");
        if (box) box.innerHTML = await asstChecklistHtml(caseId);
        UI.toast("Checklist item completed — recorded on the case");
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Recording…");
  }

  // Mail: compose in the thread against the program's live templates; send
  // routes through the SAME correspondence endpoint — QA-flagged templates
  // land in the gate (whose card renders inline above) and nothing sends
  // without the human approve.
  async function asstMailHtml(caseId) {
    const prog = await Api.program.get().catch(() => null);
    const tpls = prog?.config?.correspondence?.templates || [];
    if (!tpls.length)
      return `<div class="card"><h3>✉ Send mail</h3><p class="muted">This program defines no correspondence templates — use the case screen's correspondence tools.</p></div>`;
    corrTplCache = tpls;
    return `<div class="card"><h3>✉ Send mail</h3>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.asstSendMail('${caseId}', this)">
        <select name="template" onchange="Views.corrTemplateBody(this)">${tpls.map((tp) =>
          `<option value="${esc(tp.key)}">${esc(tp.key)}${tp.qa_role ? " (QA: " + esc(tp.qa_role) + ")" : ""}</option>`).join("")}</select>
        <select name="rfi_to" title="only used for the rfi template"><option value="provider">RFI to: provider</option><option value="plan">RFI to: health plan</option></select>
        <input name="to" placeholder="to emails (comma-separated)" required />
        <input name="cc" placeholder="cc emails" />
        <textarea name="body" rows="3" placeholder="message body — {share_link} mints a secure upload link on send, {download_link} a link to the latest generated document" required></textarea>
        <label><input type="checkbox" name="auto_share" checked /> Attach secure upload link</label>
        <label><input type="checkbox" name="auto_download" /> Attach latest-document link</label>
        <button>Send / submit for QA</button></form>
      <p class="muted">Same endpoint as the case screen — QA-flagged templates wait in the gate above until a human approves.</p></div>`;
  }

  async function asstSendMail(caseId, form) {
    const split = (s) => (s || "").split(",").map((x) => x.trim()).filter(Boolean);
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.send(caseId, {
          template: form.template.value, body: form.body.value,
          to: split(form.to.value), cc: split(form.cc.value),
          rfi_to: form.rfi_to.value, auto_share: form.auto_share.checked, auto_download: form.auto_download.checked,
        });
        asstAppend("asst-thread", "user", `Send mail: ${form.template.value} to ${form.to.value}.`);
        asstAppend("asst-thread", "assistant",
          r.status === "PENDING" ? `Submitted — the draft is waiting in the QA gate above. Approve it there and it sends; nothing goes out automatically.`
            : `Sent and logged to correspondence.`, "");
        form.body.value = "";
        asstQaLoad(caseId); // a QA-gated template appears inline immediately
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Sending…");
  }

  // Manual checks: the OCR/ICR review queue, reachable without leaving the
  // conversation. Clearing calls the same clearCheck endpoint as finance —
  // a human matches the remittance; the thread just removes the navigation.
  async function asstChecksHtml() {
    const r = await Api.program.checks("REVIEW", { limit: 50 });
    const ck = r.checks || [];
    if (!ck.length)
      return `<div class="card"><h3>🧾 Manual checks</h3><p class="muted">No checks awaiting review — the OCR queue is clear.</p></div>`;
    const rows = ck.map((c) => `<tr>
      <td class="mono">${esc((c.check_id || c.id || "").slice(0, 8))}…</td>
      <td>${esc(c.payee || "—")}</td>
      <td>${c.courtesy_amount_cents != null ? "$" + (c.courtesy_amount_cents / 100).toLocaleString() : "—"}</td>
      <td>${esc(c.memo || "")}</td>
      <td><input id="asst-ck-ref-${c.check_id || c.id}" placeholder="remittance ref" style="width:130px" />
          <button class="mini" onclick="Views.asstClearCheck('${c.check_id || c.id}',this)">clear</button></td></tr>`).join("");
    return `<div class="card"><h3>🧾 Manual checks — ${ck.length} awaiting review</h3>
      <p class="muted">OCR could not match these to an open invoice. Match each to its remittance and clear — or <a href="#/finance">open finance ↗</a> for the full queue.</p>
      <table><thead><tr><th>Check</th><th>Payee</th><th>Courtesy</th><th>Memo</th><th>Clear</th></tr></thead><tbody>${rows}</tbody></table></div>`;
  }

  async function asstClearCheck(checkId, btn) {
    const ref = document.getElementById(`asst-ck-ref-${checkId}`)?.value.trim() || "";
    if (!ref) { UI.toast("Enter the remittance reference first", { kind: "warn" }); return; }
    if (!(await UI.confirm("Clear this check?", `Matched to remittance ${ref} — funds settle against the open invoice.`, "Clear check"))) return;
    await UI.run(btn, async () => {
      try {
        await Api.program.clearCheck(checkId, ref);
        UI.toast("Check cleared");
        const box = document.getElementById("asst-tools");
        if (box) box.innerHTML = await asstChecksHtml();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Clearing…");
  }

  // Fast-path intents answer locally (REST list calls, not the LLM) and
  // used to never reach the thread's own persistence at all -- confirmed
  // live as "the chat isn't storing" for perfectly ordinary typed messages
  // that happened to contain a word like "task" (ASST_INTENT_TASKS matches
  // any message with "task"/"tasks" in it, by design, not just the quick
  // button). copilotChatLog/copilotGeneralChatLog persist the turn without
  // re-running it through the model. Best-effort: a failed log call never
  // blocks or breaks the reply the worker already sees on screen.
  function logFastPathTurn(caseId, msg, summary) {
    if (!summary) return;
    const p = caseId ? Api.program.copilotChatLog(caseId, msg, summary) : Api.program.copilotGeneralChatLog(msg, summary);
    p.catch(() => {});
  }

  // Shared router for BOTH surfaces (full #/assistant page and the floating
  // widget) so action intents, file uploads, and the fallbacks behave
  // identically no matter which one the worker is talking to.
  async function handleAssistantMessage(raw, caseId, threadId) {
    const msg = raw.trim();
    if (!msg) return;
    if (!caseId) caseId = await asstResolveCaseId(msg);
    if (ASST_INTENT_TASK_CREATE.test(msg)) {
      asstAppend(threadId, "user", msg); asstThinking(threadId);
      logFastPathTurn(caseId, msg, await asstDoCreateTask(threadId, msg, caseId));
    } else if (ASST_INTENT_TASK_COMPLETE.test(msg)) {
      asstAppend(threadId, "user", msg); asstThinking(threadId);
      logFastPathTurn(caseId, msg, await asstDoCompleteTask(threadId, msg));
    } else if (ASST_INTENT_CASES.test(msg)) {
      asstAppend(threadId, "user", msg); asstThinking(threadId);
      logFastPathTurn(caseId, msg, await asstAnswerMyCases(threadId));
    } else if (ASST_INTENT_INTAKE_CREATE.test(msg) || asstIntakeDraft) {
      asstAppend(threadId, "user", msg); asstThinking(threadId);
      await asstDoCreateIntake(threadId, msg);
    } else if (ASST_INTENT_LEFT.test(msg) && caseId) {
      asstAppend(threadId, "user", msg); asstThinking(threadId);
      logFastPathTurn(caseId, msg, await asstAnswerWhatsLeft(threadId, caseId));
    } else if (ASST_INTENT_REPORT.test(msg)) {
      asstAppend(threadId, "user", msg); asstThinking(threadId);
      logFastPathTurn(caseId, msg, await asstDoReport(threadId, msg));
    } else if (ASST_INTENT_TASKS.test(msg)) {
      asstAppend(threadId, "user", msg); asstThinking(threadId);
      logFastPathTurn(caseId, msg, await asstAnswerMyTasks(threadId));
    } else if (ASST_INTENT_DRAFT_CORR.test(msg) && caseId) {
      asstAppend(threadId, "user", msg); asstThinking(threadId);
      logFastPathTurn(caseId, msg, await asstDoDraftCorrespondence(threadId, caseId, msg));
    } else if (ASST_INTENT_DRAFT_RATIONALE.test(msg) && caseId) {
      await assistantChip(caseId, "determination_rationale", document.createElement("button"), threadId);
    } else if (caseId) {
      await assistantSend(caseId, msg, threadId);
    } else {
      await generalChatSend(msg, threadId);
    }
  }

  function mountAssistantFab() {
    if (!can("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) return;
    if (document.getElementById("asst-fab")) return;

    const fab = document.createElement("button");
    fab.id = "asst-fab";
    fab.className = "asst-fab";
    fab.type = "button";
    fab.setAttribute("aria-label", "Talk to the assistant");
    fab.textContent = "❖";
    document.body.appendChild(fab);

    const panel = document.createElement("div");
    panel.id = "asst-fab-panel";
    panel.className = "asst-fab-panel";
    panel.innerHTML = `
      <div class="asst-fab-head">
        <div class="asst-fab-title">Assistant<span class="asst-fab-case" id="asst-fab-case">no case open — ask about cases, tasks, or open one</span></div>
        <button type="button" class="mini asst-fab-close" id="asst-fab-close" aria-label="Close assistant">✕</button>
      </div>
      <div class="asst-fab-quick" id="asst-fab-quick">
        <button type="button" class="mini" data-q="cases">📂 My open cases</button>
        <button type="button" class="mini" data-q="tasks">✓ My tasks</button>
        <button type="button" class="mini" data-q="left" id="asst-fab-q-left" style="display:none">☑ What's left here</button>
      </div>
      <div class="asst-fab-body" id="asst-fab-body">
        <div class="asst-thread" id="asst-fab-thread"
          style="border:none;background:none;padding:0;min-height:140px;max-height:none">${ASST_EMPTY_HTML}</div>
      </div>
      <div class="asst-fab-foot">
        <div class="asst-chips" id="asst-fab-case-chips" style="display:none">
          <button type="button" class="mini" id="asst-fab-brief">▤ Brief</button>
          <button type="button" class="mini" id="asst-fab-actions">⚙ Actions</button>
        </div>
        <form id="asst-fab-form" class="asst-form">
          <label class="mini asst-fab-attach" title="Attach a document to the open case">📎
            <input type="file" id="asst-fab-file" style="display:none" /></label>
          <input name="message" autocomplete="off" placeholder="Ask anything — e.g. my open cases, what's left here…" aria-label="Message the assistant" />
          <button>Send</button>
        </form>
      </div>`;
    document.body.appendChild(panel);

    let open = false;
    let currentCaseId = "";
    let historyLoaded = false;

    const caseIdFromHash = () => (location.hash.match(/^#\/(?:cases|assistant)\/([\w-]+)/) || [])[1] || "";

    function syncCaseChrome() {
      const caseLbl = document.getElementById("asst-fab-case");
      const caseChips = document.getElementById("asst-fab-case-chips");
      const leftBtn = document.getElementById("asst-fab-q-left");
      if (currentCaseId) {
        caseChips.style.display = "flex";
        leftBtn.style.display = "inline-flex";
        caseLbl.textContent = "loading case…";
        Api.cases.get(currentCaseId).then((c) => { caseLbl.textContent = `grounded on ${c.case_number}`; }).catch(() => {});
      } else {
        caseChips.style.display = "none";
        leftBtn.style.display = "none";
        caseLbl.textContent = "no case open — ask about cases, tasks, or open one";
      }
    }

    // Case-grounded history only loads once per case (same real thread the
    // full #/assistant page reads) -- general Q&A (my cases/tasks/what's
    // left) stays local to this panel and isn't persisted anywhere, since
    // none of it is case-scoped server state.
    async function loadCaseHistory(caseId) {
      const thread = document.getElementById("asst-fab-thread");
      try {
        const h = await Api.program.copilotChatHistory(caseId);
        thread.innerHTML = (h.turns || []).map(asstTurnHtml).join("") || ASST_EMPTY_HTML;
        thread.scrollTop = thread.scrollHeight;
      } catch (e) {
        thread.innerHTML = `<div class="muted" style="padding:8px;font-size:12px">${esc(e.message)}</div>`;
      }
    }

    // Mirrors loadCaseHistory, reading the worker's own persisted general
    // thread (copilot_general_threads) instead of one case's.
    async function loadGeneralHistory() {
      const thread = document.getElementById("asst-fab-thread");
      try {
        const h = await Api.program.copilotGeneralChatHistory();
        thread.innerHTML = (h.turns || []).map(asstTurnHtml).join("") || ASST_EMPTY_HTML;
        thread.scrollTop = thread.scrollHeight;
      } catch (e) {
        thread.innerHTML = `<div class="muted" style="padding:8px;font-size:12px">${esc(e.message)}</div>`;
      }
    }

    function setOpen(next) {
      open = next;
      fab.dataset.open = open ? "1" : "0";
      panel.dataset.open = open ? "1" : "0";
      if (open && !historyLoaded) {
        historyLoaded = true;
        currentCaseId = caseIdFromHash();
        syncCaseChrome();
        if (currentCaseId) loadCaseHistory(currentCaseId);
        else loadGeneralHistory();
      }
    }

    async function handleMessage(raw) {
      await handleAssistantMessage(raw, currentCaseId, "asst-fab-thread");
    }

    fab.addEventListener("click", () => setOpen(!open));
    document.getElementById("asst-fab-close").addEventListener("click", () => setOpen(false));
    document.getElementById("asst-fab-brief").addEventListener("click", (e) => {
      if (currentCaseId) assistantChip(currentCaseId, "brief", e.currentTarget, "asst-fab-thread");
    });
    document.getElementById("asst-fab-actions").addEventListener("click", (e) => {
      if (currentCaseId) assistantChip(currentCaseId, "actions", e.currentTarget, "asst-fab-thread");
    });
    document.getElementById("asst-fab-quick").addEventListener("click", (e) => {
      const btn = e.target.closest("button[data-q]");
      if (!btn) return;
      if (btn.dataset.q === "cases") handleMessage("my open cases");
      else if (btn.dataset.q === "tasks") handleMessage("my tasks");
      else if (btn.dataset.q === "left") handleMessage("what's left on this case");
    });
    const fabFileInput = document.getElementById("asst-fab-file");
    fabFileInput.addEventListener("change", () => {
      document.getElementById("asst-fab-form").querySelector(".asst-fab-attach")
        .classList.toggle("has-file", !!fabFileInput.files[0]);
    });
    document.getElementById("asst-fab-form").addEventListener("submit", async (e) => {
      e.preventDefault();
      const input = e.target.message;
      const msg = input.value;
      input.value = "";
      const file = fabFileInput.files[0];
      if (file) {
        fabFileInput.value = "";
        document.getElementById("asst-fab-form").querySelector(".asst-fab-attach").classList.remove("has-file");
        if (!currentCaseId) asstAppendRich("asst-fab-thread", "Open a case first — documents upload to a specific case.");
        else await asstDoUpload("asst-fab-thread", currentCaseId, file);
      }
      if (msg.trim()) await handleMessage(msg);
    });

    // Re-ground on the new case whenever navigation changes it. General
    // Q&A keeps working regardless; this only affects "what's left"/brief/
    // actions/open-ended-chat, which need to know which case is current.
    addEventListener("hashchange", () => {
      const id = caseIdFromHash();
      if (id === currentCaseId) return;
      currentCaseId = id;
      if (open) {
        syncCaseChrome();
        if (id) loadCaseHistory(id);
        else loadGeneralHistory();
      }
    });
  }

  // ---- Thread sharing (private-by-default Assistant threads) --------------
  // Each worker's thread on a case is their own; this renders who it's
  // currently shared with (as removable chips) and a picker to add someone
  // from the tenant staff list the Team page already uses.
  async function asstShareLoad(caseId) {
    const box = document.getElementById("asst-share");
    if (!box) return;
    try {
      const [shares, staff] = await Promise.all([
        Api.program.copilotThreadShares(caseId),
        Api.admin.listTenantStaff(Api.getTenant()).catch(() => []),
      ]);
      const byId = {};
      (staff || []).forEach((s) => { byId[s.id] = s.username; });
      const chips = (shares.shares || []).map((s) =>
        `<span class="badge s-info" style="display:inline-flex;align-items:center;gap:5px">
          ${esc(byId[s.shared_with_sub] || s.shared_with_sub.slice(0, 8))}
          <button class="mini" style="padding:0 4px;min-height:auto" title="Stop sharing"
            onclick="Views.asstShareRemove('${caseId}','${s.shared_with_sub}',this)">✕</button></span>`).join(" ");
      box.innerHTML = `<span class="muted" style="font-size:11.5px">Your thread${chips ? " shared with: " : " is private — only you see it. "}</span>${chips}
        <button class="mini" onclick="Views.asstShareAdd('${caseId}')">+ Share</button>`;
    } catch { box.innerHTML = ""; }
  }

  async function asstShareAdd(caseId) {
    try {
      const staff = await Api.admin.listTenantStaff(Api.getTenant());
      const me = Auth.claims()?.sub;
      const options = (staff || []).filter((s) => s.id && s.id !== me).map((s) => [s.id, s.username]);
      if (!options.length) { UI.toast("No other staff found on this tenant", { kind: "warn" }); return; }
      const v = await UI.modal({ title: "Share your Assistant thread", submitLabel: "Share",
        fields: [{ name: "user_id", label: "Share with", options, required: true }] });
      if (!v) return;
      await Api.program.copilotShareThread(caseId, v.user_id);
      UI.toast("Thread shared");
      asstShareLoad(caseId);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function asstShareRemove(caseId, userId, btn) {
    await UI.run(btn, async () => {
      try { await Api.program.copilotUnshareThread(caseId, userId); asstShareLoad(caseId); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  // ---- Conversational QA gate (conversation-first step 3) -----------------
  // Pending gate items for THIS case render inline in the thread as cards;
  // approve / edit & approve / reject call the SAME qaDecision endpoint the
  // QA screen uses — the gate moves into the conversation, its semantics
  // (audit note, [human-edited] marker, send-on-approve) are untouched.
  async function asstQaLoad(caseId) {
    const box = document.getElementById("asst-qa");
    if (!box) return;
    try {
      const r = await Api.program.qaQueue(caseId);
      const q = r.queue || [];
      if (!q.length) { box.innerHTML = ""; return; }
      const cards = await Promise.all(q.map(async (i) => {
        const d = await Api.program.qaGet(i.id);
        const isNote = d.channel === "note";
        const isCopilot = (d.artifact || "").startsWith("copilot_");
        const bodyHtml = isCopilot
          ? `<textarea id="asst-qa-edit-${d.id}" rows="10" style="width:100%">${esc(d.body)}</textarea>
             <p class="muted">Copilot draft — edit freely; the approved text is what gets ${isNote ? "filed" : "sent"}, and the edit is recorded.</p>`
          : `<pre class="qa-body">${esc(d.body)}</pre>`;
        return `<div class="card asst-qa-card" data-qa="${d.id}" data-channel="${esc(d.channel || "email")}">
          <h3>⛨ Gate: ${esc(d.subject)}</h3>
          <p class="muted">${esc(d.drafted_by)} · ${isNote ? "determination rationale · files to timeline" : `to: ${esc((d.to_recipients || []).join(", "))}`}
            ${isCopilot ? ' · <span class="badge s-review">COPILOT DRAFT</span>' : ""}</p>
          ${bodyHtml}
          <div class="actions">
            <button onclick="Views.asstQaDecide('${caseId}','${d.id}','APPROVE',this)">${isNote ? "Approve & file" : "Approve & send"}</button>
            <button class="danger" onclick="Views.asstQaDecide('${caseId}','${d.id}','REJECT',this)">Reject</button></div></div>`;
      }));
      box.innerHTML = `<h3 style="margin:10px 0 6px">${q.length} item(s) at the QA gate for this case</h3>` + cards.join("");
    } catch (e) { box.innerHTML = ""; }
  }

  async function asstQaDecide(caseId, qaId, decision, btn) {
    const card = document.querySelector(`.asst-qa-card[data-qa="${qaId}"]`);
    const isNote = card?.dataset.channel === "note";
    const edited = document.getElementById(`asst-qa-edit-${qaId}`)?.value || "";
    let note = "";
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject draft", danger: true, submitLabel: "Reject",
        fields: [{ name: "note", label: "Rejection note", type: "textarea", required: true,
          hint: "Returned to the drafter with the draft." }] });
      if (!v) return;
      note = v.note;
    } else if (isNote) {
      if (!(await UI.confirm("Approve and file?", "The rationale is recorded on the case timeline. Nothing is emailed.", "Approve & file"))) return;
    } else if (!(await UI.confirm("Approve and send?", "The email is delivered to all recipients now and logged to correspondence.", "Approve & send"))) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.qaDecision(qaId, decision, note, edited);
        card?.remove();
        asstAppend(caseId, "assistant",
          decision === "APPROVE"
            ? (r.email_delivery_error ? `⚠ Approved but email failed: ${r.email_delivery_error}`
              : isNote ? "Rationale approved and filed to the case timeline." : "Draft approved and sent — logged to correspondence.")
            : "Draft rejected and returned to the drafter.", "");
        const box = document.getElementById("asst-qa");
        if (box && !box.querySelector(".asst-qa-card")) box.innerHTML = "";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, decision === "APPROVE" ? "Approving…" : "Rejecting…");
  }

  // Log time from the thread — same POST /cases/{id}/time endpoint the case
  // screen's Time section uses; role stamping is server-side either way.
  async function asstTimeHtml(caseId) {
    const r = await Api.cases.timeList(caseId);
    const recent = (r.entries || []).slice(0, 10).map((e) => `<tr>
      <td>${esc(e.entry_date)}</td><td>${esc(e.subject)}</td><td>${badge(e.role)}</td>
      <td>${fmtMins(e.minutes)}</td><td class="muted">${esc(e.note || "")}</td></tr>`).join("");
    return `<div class="card"><h3>⏱ Time on this dispute — ${fmtMins(r.total_minutes)} logged</h3>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.asstTimeAdd('${caseId}', this)">
        <input name="hours" type="number" step="0.25" min="0.25" max="24" placeholder="hours" required />
        ${timeRoleSelect()}
        <input name="entry_date" type="date" />
        <input name="note" placeholder="what was done" maxlength="500" />
        <label><input type="checkbox" name="billable" checked /> billable</label>
        <button class="mini">Log</button></form>
      ${recent ? `<table><thead><tr><th>Date</th><th>Who</th><th>Role</th><th>Time</th><th>Note</th></tr></thead><tbody>${recent}</tbody></table>` : `<p class="muted">No entries yet.</p>`}
      <p class="muted">Append-only, recorded under your name and role${can("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") ? ` — <a href="#/time">team weekly/monthly rollups ↗</a>` : ""}</p></div>`;
  }

  async function asstTimeAdd(caseId, form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.cases.timeAdd(caseId, {
          hours: parseFloat(form.hours.value),
          entry_date: form.entry_date.value || "",
          note: form.note.value.trim(),
          billable: form.billable.checked,
          role: form.role ? form.role.value : "",
        });
        asstAppend(caseId, "user", `Log ${form.hours.value}h${form.note.value.trim() ? " — " + form.note.value.trim() : ""}.`);
        asstAppend(caseId, "assistant", "Time logged against this dispute under your name and role. The entry is append-only — a correction is a compensating entry, never an edit.", "");
        const box = document.getElementById("asst-tools");
        if (box) box.innerHTML = await asstTimeHtml(caseId);
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Logging…");
  }

  // Correspondence compose: pre-fill the body with the program template's
  // standard text (FL: verbatim from the "Email Templates FL AHCA" source
  // document). Fills only when the body is empty or still holds another
  // template's untouched text — staff edits are never clobbered.
  let corrTplCache = [];
  function corrTemplateBody(sel) {
    const form = sel.form;
    const tp = corrTplCache.find((t) => t.key === sel.value);
    if (!tp || !tp.body || !form || !form.body) return;
    const ta = form.body;
    const untouched = corrTplCache.some((t) => t.body && t.body === ta.value);
    if (!ta.value.trim() || untouched) ta.value = tp.body;
  }

  // ---- Time entries (per-role effort, per dispute) ------------------------
  const fmtMins = (m) => `${(m / 60).toFixed(m % 60 ? 2 : 0)}h`;

  // AHCA 2026 time recording: the role is chosen PER ENTRY from the roles
  // the user actually holds — one person can code one entry and QA-as-PM
  // the next. Blank means the server records the primary role.
  // Mirrors timeEntryRole's priority list in time_entries.go -- the only
  // roles the server actually accepts for a time entry's "role performed".
  // The dropdown used to list every role in the JWT verbatim, Keycloak
  // system roles (offline_access, uma_authorization, etc.) included --
  // picking one always 422'd server-side, but nothing stopped it showing
  // as a selectable option. Confirmed live.
  const TIME_ENTRY_ROLES = ["DOCTOR", "NURSE", "CODER", "ARBITRATOR", "ATTORNEY", "CASE_MANAGER",
    "PM", "FINANCE", "STATE_AUDITOR", "NURSE_PHYSICIAN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"];
  const timeRoleSelect = () => {
    const roles = (Auth.claims()?.roles || []).filter((r) => TIME_ENTRY_ROLES.includes(r));
    if (roles.length < 2) return "";
    return `<select name="role" aria-label="Role performed" title="Role being performed for this entry">
      <option value="">role: primary</option>
      ${roles.map((r) => `<option value="${esc(r)}">${esc(r)}</option>`).join("")}</select>`;
  };

  async function timeCaseBox(caseId) {
    try {
      const r = await Api.cases.timeList(caseId);
      const rows = (r.entries || []).map((e) => `<tr>
        <td>${esc(e.entry_date)}</td><td>${esc(e.subject_name || e.subject)}</td><td>${badge(e.role)}</td>
        <td>${fmtMins(e.minutes)}</td><td>${e.billable ? "✓" : "—"}</td><td class="muted">${esc(e.note || "")}</td></tr>`).join("");
      return rows
        ? `<p><b>${fmtMins(r.total_minutes)}</b> logged on this dispute</p>
           <table><thead><tr><th>Date</th><th>Who</th><th>Role</th><th>Time</th><th>Billable</th><th>Note</th></tr></thead><tbody>${rows}</tbody></table>`
        : `<p class="muted">No time logged yet.</p>`;
    } catch (e) { return `<p class="muted">${esc(e.message)}</p>`; }
  }

  async function timeAdd(caseId, form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.cases.timeAdd(caseId, {
          hours: parseFloat(form.hours.value),
          entry_date: form.entry_date.value || "",
          note: form.note.value.trim(),
          billable: form.billable.checked,
          role: form.role ? form.role.value : "",
        });
        UI.toast("Time logged");
        form.hours.value = ""; form.note.value = "";
        const h = await timeCaseBox(caseId);
        const b = document.getElementById("time-box");
        if (b) b.innerHTML = h;
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Logging…");
  }

  // Weekly (per dispute) and monthly (whole team, per dispute) rollups —
  // the billing-review tables.
  // ---- My timesheet: the daily time-entry screen ------------------------
  // Entries across all disputes the user works on, with daily/weekly/monthly
  // totals. Entries are fully editable by their owner; the server keeps the
  // complete before/after history (audit trail / revisions).
  let tsCache = {}; // entry id -> entry (for inline editing)

  async function myTimesheet() {
    afterRender(() => myTimesheetBoxes());
    const today = new Date().toISOString().slice(0, 10);
    return `<div class="view-head"><h1>My timesheet</h1>
      <span class="muted">log the hours you work each day — totals roll up daily, weekly, and monthly</span></div>
      <div id="ts-summary" class="cards"></div>
      <div id="ts-prompts"></div>
      <div class="panel"><h2>Log time</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.myTimeAdd(this)">
          <select name="case_id" id="ts-case" required><option value="">dispute…</option></select>
          <input name="entry_date" type="date" value="${today}" required />
          <input name="hours" type="number" step="0.25" min="0.25" max="24" placeholder="hours" required style="width:6em" />
          ${timeRoleSelect()}
          <input name="note" placeholder="what you worked on" style="min-width:16em" />
          <label class="chk"><input name="billable" type="checkbox" checked /> billable</label>
          <button>Log</button></form></div>
      <div class="panel"><h2>My entries</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.myTimeRange(this)">
          <label>From <input name="from" type="date" /></label>
          <label>To <input name="to" type="date" /></label>
          <button class="mini">Apply</button>
          <button class="mini ghost" type="button" onclick="Views.myTimeRange(this.form, true)">Last 14 days</button></form>
        <div id="ts-list"><p class="muted">Loading…</p></div></div>`;
  }

  let tsFrom = "", tsTo = "";

  async function myTimesheetBoxes() {
    // Summary cards
    const sum = document.getElementById("ts-summary");
    if (sum) {
      try {
        const r = await Api.time.summary();
        sum.innerHTML = [["Today", r.today_minutes], ["This week", r.week_minutes], ["This month", r.month_minutes]]
          .map(([l, m]) => `<div class="card"><div class="card-n">${fmtMins(m || 0)}</div><div class="card-l">${l}</div></div>`).join("");
      } catch (e) { sum.innerHTML = ""; }
    }
    // Case picker for the log form
    const sel = document.getElementById("ts-case");
    if (sel && sel.options.length <= 1) {
      try {
        const r = await Api.cases.list({ limit: 200 });
        const cs = r.cases || [];
        sel.innerHTML = `<option value="">dispute…</option>` + cs.map((c) =>
          `<option value="${esc(c.id)}">${esc(c.case_number || c.id.slice(0, 8))}${c.title ? " — " + esc(c.title.slice(0, 40)) : ""}</option>`).join("");
      } catch (e) { /* leave placeholder */ }
    }
    await myTimeListBox();
    myTimePrompts();
  }
  // Disputes this user worked on (from the audit trail) on days they have
  // logged no time for them. "Log" fills the form above; nothing is recorded
  // until they enter the hours themselves.
  async function myTimePrompts() {
    const box = document.getElementById("ts-prompts");
    if (!box) return;
    try {
      const ps = (await Api.time.prompts()).prompts || [];
      box.innerHTML = ps.length ? `<div class="panel"><h2>Time not yet recorded</h2>
        <p class="muted">You worked on these disputes on these days and have no time entry for them.</p>
        <table><thead><tr><th>Day</th><th>Dispute</th><th>What you did</th><th></th></tr></thead><tbody>` +
        ps.map((x) => `<tr><td>${esc(x.day)}</td><td><a href="#/cases/${esc(x.case_id)}">${esc(x.case_number)}</a></td>
          <td class="muted">${esc(x.what)} (${x.actions})</td>
          <td><button class="mini" type="button" onclick="Views.myTimePromptFill('${esc(x.case_id)}','${esc(x.case_number)}','${esc(x.day)}')">Log time</button></td></tr>`).join("") +
        `</tbody></table></div>` : "";
    } catch (e) { box.innerHTML = ""; }
  }
  function myTimePromptFill(caseId, caseNumber, day) {
    const form = document.querySelector('form[onsubmit*="myTimeAdd"]');
    if (!form) return;
    const sel = form.case_id;
    if (![...sel.options].some((o) => o.value === caseId)) sel.insertAdjacentHTML("beforeend", `<option value="${esc(caseId)}">${esc(caseNumber)}</option>`);
    sel.value = caseId;
    form.entry_date.value = day;
    form.scrollIntoView({ behavior: "smooth", block: "center" });
    form.hours.focus();
  }

  function myTimeRange(form, reset) {
    if (reset) { tsFrom = ""; tsTo = ""; form.from.value = ""; form.to.value = ""; }
    else { tsFrom = form.from.value || ""; tsTo = form.to.value || ""; }
    myTimeListBox();
  }

  async function myTimeListBox() {
    const box = document.getElementById("ts-list");
    if (!box) return;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const r = await Api.time.mine(tsFrom, tsTo);
      const entries = r.entries || [];
      tsCache = {};
      entries.forEach((e) => { tsCache[e.id] = e; });
      const totals = {};
      (r.day_totals || []).forEach((d) => { totals[String(d.entry_date).slice(0, 10)] = d.minutes; });
      let lastDay = null;
      const rows = entries.map((e) => {
        const day = String(e.entry_date).slice(0, 10);
        let head = "";
        if (day !== lastDay) {
          lastDay = day;
          head = `<tr class="day-head"><td colspan="7"><b>${esc(day)}</b> <span class="muted">— ${fmtMins(totals[day] || 0)} total</span></td></tr>`;
        }
        return head + `<tr id="ts-row-${e.id}">
          <td class="mono"><a href="#/cases/${e.case_id}">${esc(e.case_number || String(e.case_id).slice(0, 8))}</a></td>
          <td>${badge(e.role)}</td><td>${fmtMins(e.minutes)}</td>
          <td>${String(e.billable) === "true" || e.billable === true ? "✓" : "—"}</td>
          <td class="muted">${esc(e.note || "")}</td>
          <td><button class="mini ghost" onclick="Views.myTimeEditStart(${e.id})">Edit</button>
              <button class="mini ghost bad" onclick="Views.myTimeDel(${e.id}, this)">Delete</button></td></tr>`;
      }).join("");
      box.innerHTML = entries.length
        ? `<table><thead><tr><th>Dispute</th><th>Role</th><th>Time</th><th>Billable</th><th>Note</th><th></th></tr></thead><tbody>${rows}</tbody></table>
           <p class="muted">Showing ${esc(r.from)} → ${esc(r.to)} · ${entries.length} entr${entries.length === 1 ? "y" : "ies"}</p>`
        : `<p class="muted">No entries in this range yet — log your first day above.</p>`;
    } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
  }

  async function myTimeAdd(form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.cases.timeAdd(form.case_id.value, {
          hours: parseFloat(form.hours.value),
          entry_date: form.entry_date.value || "",
          note: form.note.value.trim(),
          billable: form.billable.checked,
          role: form.role ? form.role.value : "",
        });
        UI.toast("Time logged");
        form.hours.value = ""; form.note.value = "";
        myTimesheetBoxes();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Logging…");
  }

  function myTimeEditStart(id) {
    const e = tsCache[id];
    const row = document.getElementById(`ts-row-${id}`);
    if (!e || !row) return;
    row.innerHTML = `<td colspan="6"><form class="inline-form" onsubmit="event.preventDefault(); Views.myTimeSave(${id}, this)">
      <input name="entry_date" type="date" value="${esc(String(e.entry_date).slice(0, 10))}" required />
      <input name="hours" type="number" step="0.25" min="0.25" max="24" value="${(e.minutes / 60).toFixed(2)}" required style="width:6em" />
      ${timeRoleSelect()}
      <input name="note" value="${esc(e.note || "")}" style="min-width:14em" />
      <label class="chk"><input name="billable" type="checkbox" ${(e.billable === true || String(e.billable) === "true") ? "checked" : ""} /> billable</label>
      <button class="mini">Save</button>
      <button class="mini ghost" type="button" onclick="Views.myTimeListBox()">Cancel</button></form></td>`;
    const rs = row.querySelector('select[name="role"]');
    if (rs) rs.value = e.role || "";
  }

  async function myTimeSave(id, form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.time.update(id, {
          hours: parseFloat(form.hours.value),
          entry_date: form.entry_date.value || "",
          note: form.note.value.trim(),
          billable: form.billable.checked,
          role: form.role ? form.role.value : "",
        });
        UI.toast("Entry updated");
        myTimesheetBoxes();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Saving…");
  }

  async function myTimeDel(id, btn) {
    const e = tsCache[id];
    if (!confirm(`Delete this time entry (${e ? fmtMins(e.minutes) + " on " + String(e.entry_date).slice(0, 10) : id})? The change is kept in the audit history.`)) return;
    await UI.run(btn, async () => {
      try { await Api.time.remove(id); UI.toast("Entry deleted"); myTimesheetBoxes(); }
      catch (err) { UI.toast(err.message, { kind: "warn" }); }
    }, "Deleting…");
  }

  async function timeReport() {
    if (!can("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Team time</h1></div><p class="muted">Requires PM or FINANCE.</p>`;
    const now = new Date();
    const defMonth = now.toISOString().slice(0, 7);
    // Role-based billable rates: only PM/admin may set them; only
    // PM/FINANCE/admin ever see dollar amounts.
    if (can("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      afterRender(() => timeRatesBox());
    return `<div class="view-head"><h1>Team time</h1>
      <span class="muted">hours worked per dispute — weekly and monthly rollups for billing review</span></div>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.timeReportRun(this)">
        <label>Week containing <input name="week" type="date" /></label>
        <label>or month <input name="month" type="month" value="${defMonth}" /></label>
        <label>or range <input name="start" type="date" /> → <input name="end" type="date" /></label>
        <button>Run report</button></form>
      <div id="time-rpt"></div>
      ${can("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `
      <div class="card"><h2>Send this report out</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.timeReportSend(this)">
          <input name="emails" placeholder="recipients (comma-separated emails)" required style="min-width:20em" />
          <input name="message" placeholder="optional message" style="min-width:14em" />
          <button class="mini">Email report</button>
          <button class="mini ghost" type="button" onclick="Views.timeReportCsv()">Download CSV</button></form>
        <p class="muted">Sends the period currently shown above — run the report first. Body carries the summary; the CSV attachment carries per-person and per-dispute detail with billable amounts. Sends are audit-logged.</p></div>` : ""}
      <div id="time-rates"></div>`;
  }

  async function timeRatesBox() {
    const box = document.getElementById("time-rates");
    if (!box) return;
    const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
    try {
      const r = await Api.program.timeRates();
      const rates = r.rates || [];
      const rows = rates.map((x) => `<tr>
        <td>${badge(x.role)}</td><td>${usd(x.rate_cents_per_hour)}/h</td>
        <td class="muted">${esc(x.updated_by)} · ${fmtDate(x.updated_at)}</td></tr>`).join("");
      const manage = r.can_manage
        ? `<form class="inline-form" onsubmit="event.preventDefault(); Views.timeRateSet(this)">
            <select name="role">${["DOCTOR","NURSE","CODER","NURSE_PHYSICIAN","CASE_MANAGER","PM","ARBITRATOR","ATTORNEY","FINANCE","STATE_AUDITOR"].map((x) => `<option>${x}</option>`).join("")}</select>
            <input name="rate" type="number" step="0.01" min="0" placeholder="$/hour" required />
            <button class="mini">Set rate</button></form>`
        : "";
      box.innerHTML = `<h2>Billable rates by role</h2>
        ${rows ? `<table><thead><tr><th>Role</th><th>Rate</th><th>Last set</th></tr></thead><tbody>${rows}</tbody></table>`
               : `<p class="muted">No rates configured — reports show hours only until rates are set.</p>`}
        ${manage || `<p class="muted">Rates are set by PM / administrators.</p>`}`;
    } catch (e) { box.innerHTML = ""; }
  }

  async function timeRateSet(form) {
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        await Api.program.timeRateSet(form.role.value, Math.round(parseFloat(form.rate.value) * 100));
        UI.toast(`Rate set: ${form.role.value}`);
        form.rate.value = "";
        timeRatesBox();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Saving…");
  }

  async function timeReportRun(form) {
    const box = document.getElementById("time-rpt");
    const btn = form.querySelector("button");
    box.innerHTML = `<p class="muted">Computing…</p>`;
    await UI.run(btn, async () => {
      try {
        const opts = form.start.value && form.end.value
          ? { start: form.start.value, end: form.end.value }
          : form.week.value ? { week: form.week.value } : { month: form.month.value };
        window._timeRptOpts = opts;
        const r = await Api.program.timeReport(opts);
        window._timeRpt = r;
        const usd2 = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
        const money = r.total_amount_cents !== undefined; // server attaches amounts only to PM/FINANCE/admin
        const caseRows = (r.by_case || []).map((c) => `<tr>
          <td class="mono"><a href="#/cases/${c.case_id}">${esc(c.case_number || c.case_id.slice(0, 8) + "…")}</a></td>
          <td>${fmtMins(c.minutes)}</td><td>${fmtMins(c.billable_minutes)}</td><td>${c.people}</td>
          ${money ? `<td>${c.amount_cents != null ? usd2(c.amount_cents) : "—"}${c.unrated_billable_minutes ? ` <span class="muted" title="billable minutes with no configured role rate">+${fmtMins(c.unrated_billable_minutes)} unrated</span>` : ""}</td>` : ""}</tr>`).join("");
        const personRows = (r.by_person || []).map((p) => `<tr>
          <td>${esc(p.who)}</td><td>${badge(p.role)}</td><td>${fmtMins(p.minutes)}</td><td>${fmtMins(p.billable_minutes)}</td>
          ${money ? `<td>${p.amount_cents != null ? usd2(p.amount_cents) : "—"}</td>` : ""}</tr>`).join("");
        box.innerHTML = `<h2>${esc(r.period)}</h2>
          <p><b>${fmtMins(r.total_minutes)}</b> total · <b>${fmtMins(r.billable_minutes)}</b> billable${money ? ` · <b>${usd2(r.total_amount_cents)}</b> billable amount` : ""}${money && r.unrated_billable_minutes ? ` · <span class="muted">${fmtMins(r.unrated_billable_minutes)} unrated</span>` : ""}</p>
          <h3>By dispute</h3>
          ${caseRows ? `<table><thead><tr><th>Case</th><th>Hours</th><th>Billable</th><th>People</th>${money ? "<th>Amount</th>" : ""}</tr></thead><tbody>${caseRows}</tbody></table>` : `<p class="muted">No time logged in this period.</p>`}
          <h3>By team member</h3>
          ${personRows ? `<table><thead><tr><th>Who</th><th>Role</th><th>Hours</th><th>Billable</th>${money ? "<th>Amount</th>" : ""}</tr></thead><tbody>${personRows}</tbody></table>` : ""}`;
      } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
    }, "Computing…");
  }

  // Email / export the team-hours report currently on screen.
  async function timeReportSend(form) {
    if (!window._timeRptOpts) { UI.toast("Run the report for a period first", { kind: "warn" }); return; }
    const emails = form.emails.value.split(",").map((x) => x.trim()).filter(Boolean);
    if (!emails.length) { UI.toast("At least one recipient", { kind: "warn" }); return; }
    const btn = form.querySelector("button");
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.timeReportSend({ ...window._timeRptOpts, emails, message: form.message.value.trim() });
        UI.toast(`Report emailed to ${r.sent} recipient(s) — ${esc(r.period)}`);
        form.emails.value = ""; form.message.value = "";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Sending…");
  }

  function timeReportCsv() {
    const r = window._timeRpt;
    if (!r) { UI.toast("Run the report for a period first", { kind: "warn" }); return; }
    const money = r.total_amount_cents !== undefined;
    const hrs = (m) => (m / 60).toFixed(2);
    const cell = (v) => /[",\n]/.test(String(v)) ? `"${String(v).replace(/"/g, '""')}"` : String(v);
    const lines = [["section", "who_or_case", "hours", "billable_hours"].concat(money ? ["amount"] : []).join(",")];
    (r.by_person || []).forEach((p) => lines.push(["person", cell(`${p.who} (${p.role})`), hrs(p.minutes), hrs(p.billable_minutes)].concat(money ? [p.amount_cents != null ? (p.amount_cents / 100).toFixed(2) : ""] : []).join(",")));
    (r.by_case || []).forEach((c) => lines.push(["case", cell(c.case_number || c.case_id), hrs(c.minutes), hrs(c.billable_minutes)].concat(money ? [c.amount_cents != null ? (c.amount_cents / 100).toFixed(2) : ""] : []).join(",")));
    const blob = new Blob([lines.join("\n")], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `timesheet-${(r.period || "report").replace(/\s+/g, "_")}.csv`;
    a.click();
    URL.revokeObjectURL(a.href);
  }

  // ---- Mail log: every inbound + outbound mail, referable ---------------
  function mailLog() {
    if (!can("CASE_MANAGER", "PM", "FINANCE", "ARBITRATOR", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"))
      return `<div class="view-head"><h1>Mail log</h1></div><p class="muted">Requires a staff role.</p>`;
    afterRender(mailLogBox);
    afterRender(mailRecipientsBox);
    return `<div class="view-head"><h1>Mail log</h1>
      <span class="muted">every inbound and outbound mail across the platform, with its delivery status — pending approval, sent, delivered, bounced or failed</span></div>
      <div id="mail-recipients"></div>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.mailLogFilter(this)">
        <select name="direction"><option value="">IN + OUT</option><option value="IN">Inbound</option><option value="OUT">Outbound</option></select>
        <select name="status" title="delivery status"><option value="">Any delivery status</option>
          ${["PENDING", "SENT", "DELIVERED", "BOUNCED", "FAILED", "COMPLAINED", "REJECTED"].map((x) => `<option>${x}</option>`).join("")}</select>
        <input name="q" placeholder="search subject / body / case number" style="min-width:18em" />
        <label class="muted">From <input name="from" type="date" /></label>
        <label class="muted">To <input name="to" type="date" /></label>
        <button class="mini">Search</button></form>
      <div id="mail-log"><p class="muted">Loading…</p></div>`;
  }

  let mailLogFilterState = {};

  // Delivery state of one mail-log row, shown on EVERY row (it used to show
  // only FAILED, so a successful send looked the same as an unknown one).
  const MAIL_STATUS = {
    PENDING: ["warn", "Held for QA approval — not sent yet"],
    SENT: ["s-paid", "Accepted by the mail server"],
    DELIVERED: ["s-paid", "Delivered to the recipient's mail server"],
    BOUNCED: ["s-denied", "Returned by the recipient's mail server"],
    COMPLAINED: ["s-denied", "The recipient marked it as spam"],
    FAILED: ["s-denied", "The mail server refused it — it was not sent"],
    REJECTED: ["", "Rejected in QA — never sent"],
  };
  function mailStatusBadge(m) {
    if (m.direction === "IN") return `<span class="badge" title="Inbound mail">RECEIVED</span>`;
    const st = m.delivery_status || "SENT";
    const [cls, tip] = MAIL_STATUS[st] || ["", ""];
    return `<span class="badge ${cls}" title="${esc(tip)}">${esc(st)}</span>` +
      (m.delivery_error ? `<div class="muted" style="font-size:.85em;max-width:22em">${esc(m.delivery_error)}</div>` : "");
  }

  // Who at the agency the program writes to, and which PM address is copied
  // on the Final Order. Letters that copy "AHCA" use the first list; with it
  // empty, nobody at the agency is copied -- so an empty list is shown loudly.
  async function mailRecipientsBox() {
    const box = document.getElementById("mail-recipients");
    if (!box) return;
    let r;
    try { r = await Api.program.recipients(); } catch (e) { box.innerHTML = ""; return; }
    const admin = can("FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN");
    const warn = !(r.agency || []).length ? `<div class="err-box"><b>No agency contacts are set.</b> Letters that copy AHCA, and the Final Order email, currently reach nobody at the agency.${admin ? " Add the addresses below." : " Ask an admin to add them."}</div>` : "";
    if (!admin) { box.innerHTML = warn; return; }
    box.innerHTML = warn + `<details class="prog-sec" ${warn ? "open" : ""}><summary>Program email recipients</summary>
      <form class="form" onsubmit="event.preventDefault(); Views.saveMailRecipients(this)">
        <label>Agency (AHCA) contacts — copied on letters, and the recipients of the Final Order email
          <input name="agency" placeholder="name@agency.gov, second@agency.gov" value="${esc((r.agency || []).join(", "))}" /></label>
        <label>PM address(es) copied on the Final Order email
          <input name="pm" placeholder="pm@yourcompany.com" value="${esc((r.pm || []).join(", "))}" /></label>
        <button class="mini">Save recipients</button></form></details>`;
  }
  async function saveMailRecipients(form) {
    const split = (v) => v.split(/[,;\s]+/).map((x) => x.trim()).filter(Boolean);
    try { await Api.program.setRecipients({ agency: split(form.agency.value), pm: split(form.pm.value) }); UI.toast("Recipients saved"); mailRecipientsBox(); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }

  function mailLogFilter(form) {
    mailLogFilterState = { direction: form.direction.value || "", status: form.status.value || "", q: form.q.value.trim() || "",
      from: form.from.value || "", to: form.to.value || "" };
    mailLogBox();
  }

  async function mailLogBox() {
    const box = document.getElementById("mail-log");
    if (!box) return;
    try {
      const r = await Api.program.mailJournal({ ...mailLogFilterState, limit: Pager.size() });
      const ms = r.mail || [];
      const rowsHtml = (list) => list.map((m) => {
        const rc = m.recipients || {};
        const who = m.direction === "IN" ? (rc.from || m.sent_by || "") : (rc.to || []).join(", ");
        return `<tr>
          <td>${badge(m.direction)}</td>
          <td>${m.case_id ? `<a href="#/cases/${m.case_id}">${esc(m.case_number || "case")}</a>` : `<span class="muted" title="pre-case or unmatched mail">—</span>`}</td>
          <td>${esc(m.subject || "")}${m.template && m.template !== "inbound" ? ` <span class="muted">(${esc(m.template)})</span>` : ""}</td>
          <td class="muted">${esc(who)}</td>
          <td>${mailStatusBadge(m)}</td>
          <td class="muted">${fmtDate(m.created_at)}</td>
          <td><button class="mini ghost" onclick="Views.mailLogBody(${m.id})">View</button></td></tr>
          <tr id="mail-body-${m.id}" class="row-detail" style="display:none"><td colspan="7"><pre style="white-space:pre-wrap;max-height:16em;overflow:auto">${esc(m.body || "")}</pre></td></tr>`;
      }).join("");
      box.innerHTML = ms.length
        ? `<table id="mail-tb" data-serverpaged><thead><tr><th>Dir</th><th>Case</th><th>Subject</th><th>From / To</th><th>Delivery</th><th>When</th><th></th></tr></thead><tbody>${rowsHtml(ms)}</tbody></table><div id="mail-pg"></div>`
        : `<p class="muted">No mail recorded for this filter.</p>`;
      Pager.server(document.getElementById("mail-pg"), { total: r.total ?? ms.length, shown: ms.length }, async (limit, offset) => {
        const pr = await Api.program.mailJournal({ ...mailLogFilterState, limit, offset });
        const pm = pr.mail || [];
        document.querySelector("#mail-tb tbody").innerHTML = rowsHtml(pm);
        return { total: pr.total ?? pm.length, shown: pm.length };
      });
    } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
  }

  function mailLogBody(id) {
    const row = document.getElementById(`mail-body-${id}`);
    if (row) row.style.display = row.style.display === "none" ? "" : "none";
  }

  async function uploadCheck(file, btn) {
    if (!file) { UI.toast("Choose a check image first", { kind: "warn" }); return; }
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.uploadCheck(file);
        UI.toast(`Check ${String(r.id || "").slice(0, 8)}… received — OCR processing`);
        App.rerender();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Uploading…");
  }

  async function clearCheck(checkId, el) {
    const ref = prompt("Bank clearing reference (deposit slip / lockbox ref):", "");
    if (ref === null) return;
    await UI.run(el, async () => {
      try { await Api.program.clearCheck(checkId, ref); UI.toast("Check cleared — invoice settled, ledger posted"); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  // ---- Inline SVG charts (dependency-free; match the app palette) ---------
  const CH_COLORS = ["#2E6B52", "#B08D3E", "#1D4E7E", "#5B3E8C", "#9C2B1F", "#1E5B3C", "#8A5E10", "#6E6A5E"];

  function chartDonut(segs, size = 180) {
    const total = segs.reduce((a, s) => a + s.value, 0);
    if (!total) return `<div class="chart-empty muted">No data yet</div>`;
    const R = 70, C = 2 * Math.PI * R;
    let off = 0;
    const arcs = segs.filter((s) => s.value > 0).map((s, i) => {
      const frac = s.value / total, len = frac * C;
      const el = `<circle r="${R}" cx="${size/2}" cy="${size/2}" fill="none"
        stroke="${s.color || CH_COLORS[i % CH_COLORS.length]}" stroke-width="26"
        stroke-dasharray="${len} ${C - len}" stroke-dashoffset="${-off}"
        transform="rotate(-90 ${size/2} ${size/2})"><title>${esc(s.label)}: ${s.value}</title></circle>`;
      off += len;
      return el;
    }).join("");
    const legend = segs.filter((s) => s.value > 0).map((s, i) =>
      `<span class="legend-item"><i style="background:${s.color || CH_COLORS[i % CH_COLORS.length]}"></i>${esc(s.label)} <b>${s.value}</b></span>`).join("");
    return `<div class="donut-wrap"><svg viewBox="0 0 ${size} ${size}" width="${size}" height="${size}" role="img">
      ${arcs}<text x="${size/2}" y="${size/2 - 4}" text-anchor="middle" class="donut-n">${total}</text>
      <text x="${size/2}" y="${size/2 + 16}" text-anchor="middle" class="donut-l">total</text></svg>
      <div class="legend">${legend}</div></div>`;
  }

  function chartHBars(rows, fmt = (v) => v) {
    const max = Math.max(...rows.map((r) => r.value), 1);
    return `<div class="hbars">` + rows.map((r) => `<div class="hbar-row">
      <span class="hbar-label" title="${esc(r.label)}">${esc(r.label)}</span>
      <span class="hbar-track"><span class="hbar-fill${r.warn ? " warn" : ""}" style="width:${Math.max(2, (r.value / max) * 100)}%"></span>
        ${r.warnValue ? `<span class="hbar-fill bad" style="width:${(r.warnValue / max) * 100}%"></span>` : ""}</span>
      <span class="hbar-val">${esc(String(fmt(r.value)))}${r.warnValue ? ` <b class="bad-t">${fmt(r.warnValue)} overdue</b>` : ""}</span>
      </div>`).join("") + `</div>`;
  }

  function chartArea(points, { w = 560, h = 150, color = "#2E6B52", fmt = (v) => v, label = "" } = {}) {
    if (!points.length) return `<div class="chart-empty muted">No data yet</div>`;
    const max = Math.max(...points.map((p) => p.y), 1);
    const px = (i) => (i / Math.max(points.length - 1, 1)) * (w - 44) + 36;
    const py = (v) => h - 24 - (v / max) * (h - 40);
    const line = points.map((p, i) => `${i ? "L" : "M"}${px(i).toFixed(1)},${py(p.y).toFixed(1)}`).join(" ");
    const area = `${line} L${px(points.length - 1).toFixed(1)},${h - 24} L${px(0).toFixed(1)},${h - 24} Z`;
    const gid = "g" + Math.abs(label.split("").reduce((a, c) => a + c.charCodeAt(0), 0));
    const ticks = [0, Math.floor(points.length / 2), points.length - 1].map((i) =>
      `<text x="${px(i)}" y="${h - 8}" text-anchor="middle" class="axis">${esc(points[i].x)}</text>`).join("");
    return `<svg viewBox="0 0 ${w} ${h}" class="area-chart" role="img" aria-label="${esc(label)}">
      <defs><linearGradient id="${gid}" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stop-color="${color}" stop-opacity="0.35"/><stop offset="1" stop-color="${color}" stop-opacity="0.02"/>
      </linearGradient></defs>
      <line x1="36" y1="${h - 24}" x2="${w - 8}" y2="${h - 24}" class="axis-line"/>
      <text x="4" y="${py(max) + 4}" class="axis">${fmt(max)}</text>
      <path d="${area}" fill="url(#${gid})"/>
      <path d="${line}" fill="none" stroke="${color}" stroke-width="2.5" stroke-linejoin="round"/>
      ${points.map((p, i) => `<circle cx="${px(i)}" cy="${py(p.y)}" r="2.4" fill="${color}"><title>${esc(p.x)}: ${fmt(p.y)}</title></circle>`).join("")}
      ${ticks}</svg>`;
  }

  // ---- Operations dashboard (staff roles; presence + workload + KPIs) -----

  async function opsDashboard() {
    if (!can("PM", "ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"))
      return `<div class="view-head"><h1>Ops</h1></div><p class="muted">Your role doesn't have access to Ops.</p>`;
    try {
      const d = await Api.program.opsDashboard();
      const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
      const tk = (d.task_kpis && d.task_kpis[0]) || {};
      const ck = (d.case_kpis && d.case_kpis[0]) || {};
      const sla = (d.sla && d.sla[0]) || {};
      const fin = (d.financial && d.financial[0]) || {};
      const q = (d.queues && d.queues[0]) || {};

      // Auto-refresh every 30s while the view stays open (presence + queues move).
      afterRender(() => setTimeout(() => { if (location.hash === "#/ops") App.rerender(); }, 30000));

      let html = `<div class="view-head"><h1>Operations dashboard</h1>
        <span class="muted">live workload, presence and stakeholder KPIs · tenant <b>${esc(Api.getTenant()).toUpperCase()}</b> · refreshes every 30s</span></div>
        <div class="kpi-row">
          <div class="kpi"><span class="kpi-n">${tk.completion_pct_30d ?? "—"}%</span><span class="kpi-l">Task completion (30d)</span></div>
          <div class="kpi"><span class="kpi-n">${tk.open ?? 0}</span><span class="kpi-l">Open tasks${tk.overdue ? ` · <b class="bad-t">${tk.overdue} overdue</b>` : ""}</span></div>
          <div class="kpi"><span class="kpi-n">${ck.unassigned_open ?? 0}</span><span class="kpi-l">Unassigned open cases</span></div>
          <div class="kpi"><span class="kpi-n">${sla.breaches_7d ?? 0}</span><span class="kpi-l">SLA breaches (7d) · ${sla.breaches_total ?? 0} total</span></div>
          <div class="kpi"><span class="kpi-n">${usd(fin.collected_30d_cents)}</span><span class="kpi-l">Collected (30d)</span></div>
          <div class="kpi"><span class="kpi-n">${(d.online || []).length}</span><span class="kpi-l">Staff online now</span></div>
        </div>`;

      // Queue alerts strip
      const alerts = [
        [q.checks_review, "checks awaiting OCR review", "#/finance"],
        [q.checks_awaiting_clear, "matched checks awaiting clearing", "#/finance"],
        [q.qa_pending, "letters in the QA gate", "#/qa"],
        [q.intake_open, "pre-case intake requests open", "#/intake"],
        [q.onboarding_pending, "onboarding applications pending", "#/onboarding"],
      ].filter(([n]) => Number(n) > 0);
      if (alerts.length)
        html += `<div class="ops-alerts">` + alerts.map(([n, label, href]) =>
          `<a class="ops-alert" href="${href}"><b>${n}</b> ${label}</a>`).join("") + `</div>`;

      // Charts row 1: pipeline donut + intake trend
      const statuses = (d.cases || []).map((c) => ({ label: c.status, value: Number(c.n) }));
      const opened = (d.cases_trend || []).map((x) => ({ x: String(x.day).slice(5), y: Number(x.opened) }));
      html += `<div class="chart-grid">
        <div class="chart-card"><h2>Case pipeline by status</h2>${chartDonut(statuses)}
          <p class="muted chart-foot">${ck.opened_7d ?? 0} opened in 7d · ${ck.opened_30d ?? 0} in 30d${ck.avg_open_age_days ? ` · avg open age ${ck.avg_open_age_days}d` : ""}</p></div>
        <div class="chart-card"><h2>Intake pace — cases opened, 30 days</h2>
          ${chartArea(opened, { label: "cases opened", color: "#2E6B52" })}</div></div>`;

      // Charts row 2: workload by assignee + throughput
      const byAssignee = (d.tasks_by_assignee || []).map((a) => ({
        label: a.assignee, value: Number(a.open), warnValue: Number(a.overdue) || 0, warn: Number(a.overdue) > 0 }));
      const done = (d.throughput_trend || []).map((x) => ({ x: String(x.day).slice(5), y: Number(x.done) }));
      html += `<div class="chart-grid">
        <div class="chart-card"><h2>Workload by assignee (open tasks${byAssignee.some((a) => a.warn) ? ", red = overdue" : ""})</h2>
          ${byAssignee.length ? chartHBars(byAssignee) : '<p class="muted">No tasks yet.</p>'}</div>
        <div class="chart-card"><h2>Throughput — tasks completed, 14 days</h2>
          ${chartArea(done, { label: "tasks completed", color: "#B08D3E" })}</div></div>`;

      // Collections trend (full width)
      const coll = (d.collections_trend || []).map((x) => ({ x: String(x.day).slice(5), y: Math.round(Number(x.collected_cents) / 100) }));
      html += `<div class="chart-card"><h2>Collections — payments settled per day, 30 days</h2>
        ${chartArea(coll, { label: "collections", color: "#1D4E7E", fmt: (v) => "$" + v.toLocaleString(), w: 1120 })}</div>`;

      // Presence + outstanding side by side
      const online = d.online || [];
      html += `<div class="chart-grid">
        <div class="chart-card"><h2>Who's online</h2>` +
          (online.length ? `<table><thead><tr><th></th><th>Staff</th><th>Roles</th><th>Last seen</th></tr></thead><tbody>` +
            online.map((u) => `<tr><td><span class="presence-dot"></span></td>
              <td>${esc(u.display_name || u.user_sub)}</td>
              <td class="muted">${esc((Array.isArray(u.roles) ? u.roles : []).filter((r) => !String(r).startsWith("default")).join(", ") || "—")}</td>
              <td class="muted">${fmtDate(u.last_seen)}</td></tr>`).join("") + `</tbody></table>`
          : `<p class="muted">No staff active in the last 3 minutes.</p>`) + `</div>
        <div class="chart-card"><h2>Outstanding receivables</h2>` +
          ((d.outstanding || []).length ? `<table><thead><tr><th>Party</th><th>Open</th><th>Total</th><th>Overdue</th></tr></thead><tbody>` +
            d.outstanding.map((o) => `<tr><td>${badge(o.party)}</td><td>${o.open_invoices}</td>
              <td>${usd(o.open_cents)}</td><td class="${Number(o.overdue_cents) > 0 ? "bad-t" : ""}">${usd(o.overdue_cents)}</td></tr>`).join("") +
            `</tbody></table>` : `<p class="muted">No open invoices.</p>`) + `</div></div>`;

      // Escalation trail
      if ((d.escalations || []).length)
        html += `<h2>Recent escalations</h2><table><thead><tr><th>Case</th><th>Clock</th><th>Level</th><th>To</th><th>When</th></tr></thead><tbody>` +
          d.escalations.map((e) => `<tr><td class="mono">${esc((e.case_id || "").slice(0, 8))}…</td><td>${badge(e.clock)}</td>
            <td>L${e.level}</td><td>${esc(e.escalated_to || "—")}</td><td class="muted">${fmtDate(e.created_at)}</td></tr>`).join("") + `</tbody></table>`;
      return html;
    } catch (e) { return err(e); }
  }

  // ---- Rules admin (FEDERAL_ADMIN / PLATFORM_ADMIN; every save audited) -----
  let rulesDraft = null; // working copy; saved as one unit so the audit diff is meaningful

  const condText = (c) => `${esc(c.field)} ${esc(c.op)} ${c.value === undefined ? "" : esc(JSON.stringify(c.value))}`;
  const actText = (a) => `${esc(a.type)} ${Object.entries(a.params || {}).map(([k, v]) => `${esc(k)}=${esc(String(v))}`).join(" ")}`;

  async function rulesAdmin() {
    try {
      const [lr, ar, pr] = await Promise.all([Api.program.rules(), Api.program.rulesAudit().catch(() => ({ changes: [] })), Api.program.get().catch(() => null)]);
      rulesDraft = (lr.rules || []).map((r) => ({ ...r }));
      afterRender(bindRulesAdmin);
      window._rulesAudit = ar.changes || [];
      const curLabel = pr?.tenant_label || "";
      return `<div class="view-head"><h1>Program rules</h1>
        <span class="muted">live policy — changes take effect immediately and are permanently audited</span></div>
        ${can("FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `
        <p><button onclick="Views.createTenantFlow()">＋ Create tenant</button>
           <span class="muted">activates a pre-provisioned state's Keycloak group + optional first user</span></p>
        <p><button onclick="Views.createFederalAdminFlow()">＋ Create federal admin</button>
           <span class="muted">cross-tenant FEDERAL_ADMIN or PLATFORM_ADMIN account</span></p>` : ""}
        <p><button onclick="Views.renameTenantFlow('${esc(curLabel)}')">✎ ${curLabel ? `Rename "${esc(curLabel)}"` : "Set display name"}</button>
           <span class="muted">shown in place of the bare "${esc(Api.getTenant().toUpperCase())}" code throughout the portal</span></p>
        <p><button id="rule-add">＋ New rule</button>
           <button id="rules-save" class="btn-primary">Save all changes</button></p>
        <div id="rules-list">` +
        (rulesDraft.length ? rulesDraft.map((r, i) => `
          <div class="rule-card ${r.enabled === false ? "rule-off" : ""}">
            <div class="rule-head"><b>${esc(r.name || "(unnamed)")}</b> ${badge(r.event)}
              <label class="rule-toggle"><input type="checkbox" data-idx="${i}" class="rule-en" ${r.enabled === false ? "" : "checked"} /> enabled</label>
              <span style="flex:1"></span>
              <button class="mini" onclick="Views.ruleEdit(${i})">edit</button>
              <button class="mini danger" onclick="Views.ruleDelete(${i})">delete</button></div>
            ${r._basis ? `<p class="muted rule-basis">${esc(r._basis)}</p>` : ""}
            <div class="rule-cols"><div><h4>When (all must hold)</h4><ul>${
              (r.conditions || []).map((c) => `<li class="mono">${condText(c)}</li>`).join("") || "<li class='muted'>always</li>"}</ul></div>
            <div><h4>Then</h4><ul>${
              (r.actions || []).map((a) => `<li class="mono">${actText(a)}</li>`).join("")}</ul></div></div>
          </div>`).join("") : `<p class="muted">No rules configured — hardcoded defaults apply.</p>`) +
        `</div>
        <h2>Change history <span class="muted">append-only audit trail, newest first</span></h2>
        ${window._rulesAudit.length ? `<p><input placeholder="Filter by who changed it or the note…" oninput="Views.rulesAuditFilter(this.value)" style="min-width:280px" /></p>` : ""}
        <div id="rules-audit-box">${rulesAuditHtml(window._rulesAudit)}</div>`;
    } catch (e) { return err(e); }
  }

  function rulesAuditHtml(audit) {
    return audit.length ? `<table><thead><tr><th>When</th><th>Changed by</th><th>Note</th><th>Rules</th><th></th></tr></thead><tbody>` +
        audit.map((c) => `<tr><td class="muted">${fmtDate(c.changed_at)}</td><td class="mono">${esc(c.changed_by.slice(0, 12))}…</td>
          <td>${esc(c.note || "—")}</td><td>${(c.before || []).length} → ${(c.after || []).length}</td>
          <td><details><summary class="mini">diff</summary><pre class="rule-diff">${esc(JSON.stringify(c.after, null, 1))}</pre></details></td></tr>`).join("") +
        `</tbody></table>` : `<p class="muted">No changes recorded yet.</p>`;
  }
  function rulesAuditFilter(q) {
    q = q.trim().toLowerCase();
    const filtered = q ? (window._rulesAudit || []).filter((c) =>
      (c.changed_by || "").toLowerCase().includes(q) || (c.note || "").toLowerCase().includes(q)) : (window._rulesAudit || []);
    document.getElementById("rules-audit-box").innerHTML = rulesAuditHtml(filtered);
  }

  function ruleEdit(idx) {
    const r = rulesDraft[idx];
    UI.modal({
      title: `Edit rule: ${r.name || ""}`, submitLabel: "Apply", wide: true,
      fields: [{ name: "json", label: "Rule definition (JSON)", type: "textarea", required: true,
        value: JSON.stringify(r, null, 2),
        hint: "events: intake.advance | doc.upload | doc.analyzed | claims.imported | invoice.settled | sweep.intake · ops: eq neq in contains gt gte lt lte is_null not_null days_older_than · actions: set_status set_detail notify log_activity block_request flag_review" }],
    }).then((v) => {
      if (v === null) return;
      try {
        const parsed = JSON.parse(v.json);
        if (!parsed.name || !parsed.event) throw new Error("rule needs name + event");
        rulesDraft[idx] = parsed;
        UI.toast(`Rule "${parsed.name}" staged — Save all changes to apply`);
        App.rerender();
      } catch (e) { UI.toast(`Invalid rule JSON: ${e.message}`, { kind: "error" }); }
    });
  }

  function ruleDelete(idx) {
    UI.modal({ title: `Delete rule "${rulesDraft[idx].name}"?`, danger: true, submitLabel: "Delete",
      body: "The deletion is staged until you Save all changes; the audit trail records it permanently." })
      .then((ok) => { if (ok === null) return; rulesDraft.splice(idx, 1); UI.toast("Rule staged for deletion"); App.rerender(); });
  }

  async function rulesSave() {
    const v = await UI.modal({ title: "Save program rules", submitLabel: "Save & audit", fields: [
      { name: "note", label: "Change note (audit trail)", required: true, placeholder: "e.g. AHCA memo 2026-03: day-13 gate" }] });
    if (!v) return;
    try {
      const r = await Api.program.saveRules(rulesDraft, v.note);
      UI.toast(`${r.saved} rule(s) saved — change recorded in the audit trail`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  // Tenant codes are pre-provisioned at the DB level for all 50 US states
  // (schema + cases/documents tables); what actually gates login access is
  // the Keycloak /tenant/<xx> group, which until now only ever got created
  // by hand against the live realm. This activates one from the admin page
  // instead, optionally seeding its first user in the same call.
  async function createTenantFlow() {
    const v = await UI.modal({
      title: "Create tenant", submitLabel: "Create",
      fields: [
        { name: "tenant", label: "State code", required: true, placeholder: "ga",
          hint: "2-letter lowercase USPS code — the tenant_<xx> schema must already exist" },
        { name: "label", label: "Display label", placeholder: "Georgia IDRE" },
        { name: "username", label: "First user — username" },
        { name: "email", label: "First user — email" },
        { name: "role", label: "First user — role", value: "CASE_MANAGER", options: [
          ["CASE_MANAGER", "Case Manager"], ["ARBITRATOR", "Arbitrator"], ["PM", "PM"],
          ["CODER", "Coder"], ["DOCTOR", "Doctor"], ["NURSE", "Nurse"], ["ATTORNEY", "Attorney"],
          ["FINANCE", "Finance"], ["STATE_AUDITOR", "State Auditor"], ["PARTY", "Party"]] },
      ],
    });
    if (!v) return;
    const tenant = (v.tenant || "").trim().toLowerCase();
    if (!/^[a-z]{2}$/.test(tenant)) { UI.toast("State code must be exactly 2 lowercase letters", { kind: "error" }); return; }
    if ((v.username && !v.email) || (!v.username && v.email)) {
      UI.toast("First user needs both username and email, or leave both blank", { kind: "error" }); return;
    }
    const payload = { tenant, label: v.label || "" };
    if (v.username && v.email) payload.first_user = { username: v.username, email: v.email, role: v.role };
    try {
      const r = await Api.admin.createTenant(payload);
      let msg = `Tenant "${tenant}" ${r.group_already_existed ? "group already existed" : "group created"} (${r.group})`;
      let kind;
      if (r.first_user?.role_assignment_error) {
        msg += ` — user "${r.first_user.username}" created, but role assignment FAILED: ${r.first_user.role_assignment_error} — fix this in Keycloak directly before relying on this account`;
        kind = "error";
      } else if (r.first_user?.temporary_password) {
        msg += ` — user "${r.first_user.username}" created, temporary password: ${r.first_user.temporary_password}`;
      } else if (r.first_user_error) {
        msg += ` — ${r.first_user_error}`;
      }
      UI.toast(msg, kind ? { kind, sticky: true } : { sticky: true });
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  // Sets the display name shown in place of the bare tenant code (e.g.
  // "AHCA" instead of "FL") -- the tenant chip (app.js), cross-tenant
  // switcher, and dashboard header all read it once set.
  async function renameTenantFlow(curLabel) {
    const v = await UI.modal({
      title: "Tenant display name", submitLabel: "Save",
      fields: [{ name: "label", label: "Display name", required: true, value: curLabel, placeholder: "AHCA" }],
    });
    if (!v) return;
    try {
      await Api.program.setLabel(v.label.trim());
      UI.toast(`Display name set to "${v.label.trim()}"`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  // Creates a cross-tenant FEDERAL_ADMIN/PLATFORM_ADMIN account -- there was
  // previously no way to do this through the app at all; federal.admin
  // itself was seeded by hand-editing the Keycloak realm.
  async function createFederalAdminFlow() {
    const v = await UI.modal({
      title: "Create federal admin", submitLabel: "Create",
      fields: [
        { name: "username", label: "Username", required: true },
        { name: "email", label: "Email", required: true },
        { name: "role", label: "Role", value: "FEDERAL_ADMIN",
          options: [["FEDERAL_ADMIN", "Federal admin"], ["PLATFORM_ADMIN", "Platform admin"]] },
      ],
    });
    if (!v) return;
    try {
      const r = await Api.admin.createFederalAdmin({ username: v.username, email: v.email, role: v.role });
      if (r.role_assignment_error) {
        UI.toast(`User "${r.username}" created, but role assignment FAILED: ${r.role_assignment_error} — fix this in Keycloak directly before relying on this account`,
          { kind: "error", sticky: true });
      } else {
        UI.toast(`User "${r.username}" created (${r.role}) — temporary password: ${r.temporary_password}`, { sticky: true });
      }
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  // #/team -- the one page a PM has a standing right to (their own
  // tenant, enforced server-side by createTenantStaff/listTenantStaff),
  // independent of the Rules page's FEDERAL_ADMIN/PLATFORM_ADMIN-only gate.
  // CASE_MANAGER (reviewer) no longer has any access here, by request.
  // scope is "tenant" (acts via the tenant-staff endpoints, tenant required)
  // or "federal" (cross-tenant admin endpoints, no tenant). The caller's own
  // row never gets action buttons -- the backend rejects self-suspend/
  // self-delete too, but hiding them here avoids a round-trip just to find
  // that out, and stops an admin from locking themselves out by accident.
  function staffTableHtml(members, scope, tenant) {
    if (!members.length) return `<p class="muted">No staff found.</p>`;
    const me = Auth.claims()?.name;
    return `<table><thead><tr><th>Username</th><th>Email</th><th>Roles</th><th>Status</th><th></th></tr></thead><tbody>` +
      members.map((m) => {
        const isSelf = m.username === me;
        const actions = isSelf ? '<span class="muted">(you)</span>' : (
          (m.enabled
            ? `<button class="mini" onclick="Views.setStaffEnabled('${scope}','${esc(tenant || "")}','${esc(m.username)}',false)">Suspend</button>`
            : `<button class="mini" onclick="Views.setStaffEnabled('${scope}','${esc(tenant || "")}','${esc(m.username)}',true)">Reactivate</button>`) +
          ` <button class="mini" onclick="Views.deleteStaffMember('${scope}','${esc(tenant || "")}','${esc(m.username)}')">Delete</button>`
        );
        return `<tr><td class="mono">${esc(m.username)}</td><td>${esc(m.email || "—")}</td>
        <td>${(m.roles || []).map((r) => badge(r)).join(" ") || "—"}</td>
        <td>${m.enabled ? '<span class="badge s-paid">enabled</span>' : '<span class="badge s-denied">disabled</span>'}</td>
        <td>${actions}</td></tr>`;
      }).join("") +
      `</tbody></table>`;
  }
  async function teamAdmin() {
    if (!can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Team</h1></div><p class="muted">Your role doesn't have access to Team.</p>`;
    const isAdmin = can("FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const tenant = Api.getTenant();
    let html = `<div class="view-head"><h1>Team</h1>
      <span class="muted">add staff accounts to ${isAdmin ? "any activated tenant" : "your own tenant"}</span></div>
      <p><button onclick="Views.addTenantStaffFlow()">＋ Add tenant staff</button>
      ${isAdmin ? ` <button onclick="Views.createFederalAdminFlow()">＋ Create federal admin</button>` : ""}</p>`;
    try {
      const [staff, federal] = await Promise.all([
        Api.admin.listTenantStaff(tenant),
        isAdmin ? Api.admin.listFederalAdmins().catch(() => []) : Promise.resolve(null),
      ]);
      html += `<h2>Staff — tenant ${esc(tenant.toUpperCase())}</h2>${staffTableHtml(staff, "tenant", tenant)}`;
      if (federal) html += `<h2>Federal / platform admins <span class="muted">cross-tenant</span></h2>${staffTableHtml(federal, "federal")}`;
      return html;
    } catch (e) { return html + err(e); }
  }

  // Suspend (enabled=false) or reactivate (enabled=true) a staff account.
  // Backend re-checks role/tenant scoping and the self-action guard
  // independently -- this is UX, not the security boundary.
  async function setStaffEnabled(scope, tenant, username, enabled) {
    try {
      if (scope === "federal") await Api.admin.setFederalAdminEnabled(username, enabled);
      else await Api.admin.setTenantStaffEnabled(tenant, username, enabled);
      UI.toast(`${username} ${enabled ? "reactivated" : "suspended"}`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  async function deleteStaffMember(scope, tenant, username) {
    if (!(await UI.confirm(`Delete ${username}?`, "This permanently removes the account. It cannot be undone.", "Delete", true))) return;
    try {
      if (scope === "federal") await Api.admin.deleteFederalAdmin(username);
      else await Api.admin.deleteTenantStaff(tenant, username);
      UI.toast(`${username} deleted`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  // Adds a second (or third...) staff member to an already-activated
  // tenant, without resubmitting a whole "create tenant" request just to
  // piggyback a new first_user. Tenant field is pre-filled with the
  // caller's own tenant -- the only one a CASE_MANAGER can actually target.
  async function addTenantStaffFlow() {
    const v = await UI.modal({
      title: "Add tenant staff", submitLabel: "Create",
      fields: [
        { name: "tenant", label: "State code", required: true, value: Api.getTenant(),
          hint: "2-letter lowercase USPS code — must already be an activated tenant" },
        { name: "username", label: "Username", required: true },
        { name: "email", label: "Email", required: true },
        { name: "role", label: "Role", value: "CASE_MANAGER", options: [
          ["CASE_MANAGER", "Case Manager"], ["ARBITRATOR", "Arbitrator"], ["PM", "PM"],
          ["CODER", "Coder"], ["DOCTOR", "Doctor"], ["NURSE", "Nurse"], ["ATTORNEY", "Attorney"],
          ["FINANCE", "Finance"], ["STATE_AUDITOR", "State Auditor"], ["PARTY", "Party"],
          // Minting a TENANT_ADMIN is federal/platform-admin-only server-side
          // (avoids a self-escalating admin chain within one state) -- the
          // option only appears here when the caller could actually use it.
          ...(can("FEDERAL_ADMIN", "PLATFORM_ADMIN") ? [["TENANT_ADMIN", "Tenant Admin (super admin)"]] : [])] },
      ],
    });
    if (!v) return;
    const tenant = (v.tenant || "").trim().toLowerCase();
    if (!/^[a-z]{2}$/.test(tenant)) { UI.toast("State code must be exactly 2 lowercase letters", { kind: "error" }); return; }
    try {
      const r = await Api.admin.createTenantStaff({ tenant, username: v.username, email: v.email, role: v.role });
      if (r.role_assignment_error) {
        UI.toast(`User "${r.username}" created, but role assignment FAILED: ${r.role_assignment_error} — fix this in Keycloak directly before relying on this account`,
          { kind: "error", sticky: true });
      } else {
        UI.toast(`User "${r.username}" created (${r.role}) on tenant ${tenant} — temporary password: ${r.temporary_password}`, { sticky: true });
      }
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  function bindRulesAdmin() {
    document.querySelectorAll(".rule-en").forEach((cb) => cb.addEventListener("change", () => {
      rulesDraft[+cb.dataset.idx].enabled = cb.checked;
      UI.flash(cb.closest(".rule-card"));
    }));
    document.getElementById("rule-add")?.addEventListener("click", () => {
      rulesDraft.push({ name: "new-rule", event: "intake.advance", enabled: false, conditions: [], actions: [] });
      App.rerender();
      ruleEdit(rulesDraft.length - 1);
    });
    document.getElementById("rules-save")?.addEventListener("click", () => UI.run(document.getElementById("rules-save"), rulesSave, "Saving…"));
  }

  // ---- Audit log -----------------------------------------------------------
  // public.audit_log previously had zero read access anywhere, despite being
  // written to throughout the backend (NPI verification, onboarding
  // decisions, escalations, intake sweeps). FEDERAL_ADMIN/PLATFORM_ADMIN and
  // STATE_AUDITOR see every tenant (same cross-tenant switcher as the rest of
  // the portal); CASE_MANAGER/PM see their own tenant only -- all enforced
  // server-side, the nav link here is just the matching visibility.
  function auditRows(rows) {
    if (!rows.length) return `<p class="muted">No audit entries match.</p>`;
    const rowsHtml = (list) => list.map((e) => {
      // e.payload may come back as a decoded object or as raw JSON text
      // depending on the column path -- handle both rather than assume.
      const payloadStr = e.payload && typeof e.payload === "object" ? JSON.stringify(e.payload) : String(e.payload || "");
      // by_name is the backend's resolved display name for payload.by (a
      // raw Keycloak subject -- logAudit's own stable-identity convention,
      // unchanged); falls back to "—" when payload carried no "by" at all.
      const who = e.by_name || "—";
      return `<tr>
        <td class="muted">${fmtDate(e.created_at)}</td>
        <td>${e.case_id ? `<a href="#/cases/${esc(e.case_id)}">${esc(String(e.case_id).slice(0, 8))}…</a>` : `<span class="muted">—</span>`}</td>
        <td>${badge(e.action)}</td>
        <td>${esc(who)}</td>
        <td class="mono" style="max-width:360px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${esc(payloadStr)}">${esc(payloadStr)}</td>
        <td class="mono" title="${esc(e.hash || "")}">${esc(String(e.hash || "").slice(0, 12))}…</td>
      </tr>`;
    }).join("");
    const { bodyHtml, pagerHtml } = initClientPager("audit", rows, rowsHtml);
    return `<table id="audit-tb"><thead><tr><th>When</th><th>Case</th><th>Action</th><th>By</th><th>Payload</th><th>Hash</th></tr></thead>
      <tbody>${bodyHtml}</tbody></table>${pagerHtml}`;
  }

  async function auditLog() {
    // Same afterRender-before-await race as intake() -- see its comment.
    // The list itself rendered fine (auditRows is inline in the returned
    // HTML), but the Filter form's submit listener never attached, so
    // clicking Filter fell back to a bare native submit: page reload,
    // nothing happens. Almost certainly what "audit log isn't functional"
    // actually was.
    try {
      const r = await Api.auditLog.list({ limit: 500 });
      afterRender(() => {
        $("#al-filter")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = Object.fromEntries(new FormData(ev.target));
          try {
            const rf = await Api.auditLog.list({ limit: 500, case_id: f.case_id, action: f.action });
            document.getElementById("al-list").innerHTML = auditRows(rf.entries || []);
            bindClientPager("audit", "#audit-tb tbody");
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        });
        bindClientPager("audit", "#audit-tb tbody");
      });
      return `<div class="view-head"><h1>Audit log</h1>
        <span class="muted">hash-chained, tamper-evident — every entry's hash covers its payload plus the previous entry's hash</span></div>
        <form id="al-filter" class="inline-form">
          <input name="case_id" placeholder="filter by case id" />
          <input name="action" placeholder="filter by action (e.g. NPI_VERIFICATION_DEGRADED)" />
          <button>Filter</button>
        </form>
        <div id="al-list">${auditRows(r.entries || [])}</div>`;
    } catch (e) { return err(e); }
  }

  // ---------- Service-fee invoicing, AR/AP, reconciliation ----------
  const usdC = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
  // CASE_MANAGER deliberately excluded -- these are standalone finance
  // screens, not case-detail info (caseFinBox, below, is separate and
  // still available to CASE_MANAGER on a case's own page). Matches the
  // backend's billingViewRoles/arapReadRoles/reconReadRoles/
  // financialReadRoles (access-control follow-up).
  const finRoles = ["PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"];

  async function caseFinBox(caseId) {
    try {
      const r = await Api.program.caseFinancials(caseId);
      const t = r.totals || {};
      const feeRows = (r.fee_invoices || []).map((i) => `<tr><td class="mono">${esc(i.invoice_no)}</td>
        <td>${esc(i.party)}</td><td>${esc(i.kind)}</td><td>${usdC(i.amount_cents)}</td>
        <td>${usdC(i.paid_cents)}</td><td>${badge(i.status)}</td></tr>`).join("");
      const svcRows = (r.service_lines || []).map((l) => `<tr><td class="mono">${esc(l.invoice_no)}</td>
        <td>${badge(l.role)}</td><td>${fmtMins(l.minutes)}</td><td>${usdC(l.amount_cents)}</td>
        <td>${badge(l.invoice_status)}</td></tr>`).join("");
      const svcPays = (r.service_payments || []).map((p) => `<tr><td class="mono">${esc(p.invoice_no)}</td>
        <td>${usdC(p.amount_cents)}</td><td>${esc(p.method)}</td><td class="mono">${esc(p.ref || "")}</td>
        <td>${esc((p.received_at || "").slice(0, 10))}</td></tr>`).join("");
      const apRows = (r.payables || []).map((p) => `<tr><td>${esc(p.payee)}</td><td>${badge(p.source)}</td>
        <td>${usdC(p.amount_cents)}</td><td>${badge(p.status)}</td><td class="mono">${esc(p.settle_ref || "")}</td></tr>`).join("");
      return `<div class="stat-grid">
          <div class="stat"><div class="stat-num">${usdC(t.ar_due_cents)}</div><div class="muted">AR due (fees)</div></div>
          <div class="stat"><div class="stat-num">${usdC(t.service_billed_cents)}</div><div class="muted">Service billed</div></div>
          <div class="stat"><div class="stat-num">${usdC(t.collected_total_cents)}</div><div class="muted">Collected total</div></div>
          <div class="stat"><div class="stat-num">${usdC(t.ap_open_cents)}</div><div class="muted">AP open (awards…)</div></div></div>
        <h3>Fee invoices (payer/provider)</h3>
        ${feeRows ? `<table class="tbl"><thead><tr><th>Invoice</th><th>Party</th><th>Kind</th><th>Amount</th><th>Paid</th><th>Status</th></tr></thead><tbody>${feeRows}</tbody></table>` : '<p class="muted">No fee invoices on this dispute.</p>'}
        <h3>Service-fee lines</h3>
        ${svcRows ? `<table class="tbl"><thead><tr><th>Invoice</th><th>Role</th><th>Time</th><th>Amount</th><th>Status</th></tr></thead><tbody>${svcRows}</tbody></table>` : '<p class="muted">No service billing on this dispute.</p>'}
        ${svcPays ? `<h3>Service payments allocated to this dispute</h3><table class="tbl"><thead><tr><th>Invoice</th><th>Allocated</th><th>Method</th><th>Ref</th><th>Received</th></tr></thead><tbody>${svcPays}</tbody></table>` : ""}
        <h3>Payables</h3>
        ${apRows ? `<table class="tbl"><thead><tr><th>Payee</th><th>Source</th><th>Amount</th><th>Status</th><th>Settle ref</th></tr></thead><tbody>${apRows}</tbody></table>` : '<p class="muted">No obligations recorded.</p>'}`;
    } catch (e) { return `<p class="error">${esc(e.message)}</p>`; }
  }

  // ===== Invoices view (#/billing) =====
  function billingInvoices() {
    if (!can(...finRoles))
      return `<div class="view-head"><h1>Billing</h1></div><p class="muted">Requires PM or FINANCE.</p>`;
    afterRender(() => billingList());
    const now = new Date().toISOString();
    return `<div class="view-head"><h1>Billing — service-fee invoices</h1>
      <span class="muted">generated from the time ledger × role rates; every dollar ties back to disputes</span></div>
      <div class="card"><h2>Generate</h2>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.billingGen(this)">
        <label>Week <input name="week" type="date" /></label>
        <label>Month <input name="month" type="month" value="${now.slice(0, 7)}" /></label>
        <label>Range <input name="start" type="date" /> → <input name="end" type="date" /></label>
        <label><input type="checkbox" name="percase" /> per-case invoices</label>
        <button>Generate DRAFT</button></form>
      <p class="muted">Idempotent per period — an existing active invoice for the same period is reported, never duplicated. Billable time without a role rate blocks generation (configurable).</p>
      <div id="billing-gen-out"></div></div>
      <div id="billing-list"><p class="muted">Loading…</p></div>
      <div id="billing-detail"></div>`;
  }

  async function billingGen(form) {
    const out = document.getElementById("billing-gen-out");
    const params = form.start.value && form.end.value ? { start: form.start.value, end: form.end.value }
      : form.week.value ? { week: form.week.value } : { month: form.month.value };
    out.innerHTML = `<p class="muted">Generating…</p>`;
    try {
      const r = await Api.program.billingGenerate(params, { consolidate: !form.percase.checked });
      const made = (r.invoices || []).map((i) => `<div>✅ <b class="mono">${esc(i.invoice_no)}</b> — ${usdC(i.total_cents)} · ${i.lines} line(s) ${i.case_id ? `· case ${esc(i.case_id.slice(0, 8))}…` : "· consolidated"}</div>`).join("");
      const skipped = (r.skipped || []).map((x) => `<div class="muted">⏭ ${esc(x.case_id || "consolidated")}: ${esc(x.reason)} ${x.existing_invoice_no ? `(existing <b class="mono">${esc(x.existing_invoice_no)}</b>)` : ""}</div>`).join("");
      const unrated = (r.excluded_unrated || []).map((u) => `<div class="muted">⚠ unrated: ${esc(u.role)} ${fmtMins(u.billable_minutes)} on ${esc((u.case_id || "").slice(0, 8))}…</div>`).join("");
      out.innerHTML = `<p><b>${esc(r.period)}</b></p>${made}${skipped}${unrated}` || `<p class="muted">No billable time in period.</p>`;
      billingList();
    } catch (e) {
      const unr = e.data && e.data.unrated ? e.data.unrated.map((u) => `<div>⚠ ${esc(u.role)} — ${fmtMins(u.billable_minutes)} (case ${esc((u.case_id || "").slice(0, 8))}…)</div>`).join("") : "";
      out.innerHTML = `<p class="error">${esc(e.message)}</p>${unr}`;
    }
  }

  async function billingList(opts) {
    opts = opts || {};
    const box = document.getElementById("billing-list");
    if (!box) return;
    try {
      const r = await Api.program.billingList(opts);
      const rows = (r.invoices || []).map((i) => {
        const due = Number(i.total_cents) - Number(i.paid_cents || 0);
        let actions = "";
        if (i.status === "DRAFT" && can("CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
          actions += `<button class="mini" onclick="Views.billingActFn('${i.id}','approve')">Approve</button> `;
        if (i.status === "APPROVED" && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
          actions += `<button class="mini" onclick="Views.billingActFn('${i.id}','issue')">Issue</button> `;
        if (["DRAFT", "APPROVED", "ISSUED"].includes(i.status) && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
          actions += `<button class="mini danger" onclick="Views.billingActFn('${i.id}','void')">Void</button> `;
        if (i.status === "ISSUED" && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
          actions += `<button class="mini" onclick="Views.billingPayForm('${i.id}')">Record payment</button> `;
        actions += `<button class="mini" onclick="Views.billingDetail('${i.id}')">Detail</button>
          <button class="mini" onclick="Views.billingExportFn('${i.id}')">CSV</button>`;
        return `<tr><td class="mono">${esc(i.invoice_no)}</td>
          <td>${esc(i.period_start)} → ${esc(i.period_end)}</td>
          <td>${esc(i.bill_to_name || "")}</td><td>${usdC(i.total_cents)}</td>
          <td>${i.status === "ISSUED" ? usdC(due) : "—"}</td>
          <td>${badge(i.status)}</td><td>${actions}</td></tr>`;
      }).join("");
      box.innerHTML = `<h2>Invoices</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.billingFilter(this)">
          <select name="status"><option value="">all statuses</option>${["DRAFT", "APPROVED", "ISSUED", "PAID", "VOID"].map((x) => `<option ${x === (opts.status || "") ? "selected" : ""}>${x}</option>`).join("")}</select>
          <label class="muted">Period from <input name="start" type="date" value="${esc(opts.start || "")}" /></label>
          <label class="muted">to <input name="end" type="date" value="${esc(opts.end || "")}" /></label>
          <button class="mini">Filter</button></form>
        ${rows ? `<table class="tbl"><thead><tr><th>Invoice</th><th>Period</th><th>Bill to</th><th>Total</th><th>Due</th><th>Status</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : '<p class="muted">No invoices yet — generate one above.</p>'}`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function billingActFn(id, action) {
    if (action === "void" && !confirm("Void this invoice?")) return;
    try { await Api.program.billingAct(id, action); billingList(); }
    catch (e) { alert(e.message); }
  }

  async function billingDetail(id) {
    const box = document.getElementById("billing-detail");
    try {
      const r = await Api.program.billingGet(id);
      const inv = r.invoice || {};
      const lines = (r.lines || []).map((l) => `<tr><td class="mono">${esc(l.case_number || "")}</td>
        <td>${badge(l.role)}</td><td>${fmtMins(l.minutes)}</td><td>${usdC(l.rate_cents_per_hour)}/h</td>
        <td>${usdC(l.amount_cents)}</td><td>${esc(l.description)}</td></tr>`).join("");
      const pays = (r.payments || []).map((p) => `<tr><td>${usdC(p.amount_cents)}</td><td>${esc(p.method)}</td>
        <td class="mono">${esc(p.ref || "")}</td><td>${esc((p.received_at || "").slice(0, 10))}</td><td>${esc(p.recorded_by)}</td></tr>`).join("");
      const evs = (r.events || []).map((e) => `<div class="muted">${esc(e.event)} · ${esc(e.actor)} · ${fmtDate(e.created_at)}</div>`).join("");
      box.innerHTML = `<div class="card"><h2 class="mono">${esc(inv.invoice_no)}</h2>
        <p>${badge(inv.status)} ${esc(inv.bill_to_name || "")} · due ${esc(inv.due_date || "—")} · ${esc(inv.memo || "")}</p>
        <table class="tbl"><thead><tr><th>Case</th><th>Role</th><th>Time</th><th>Rate</th><th>Amount</th><th>Description</th></tr></thead><tbody>${lines}</tbody></table>
        <p><b>Subtotal ${usdC(inv.subtotal_cents)} · Tax ${usdC(inv.tax_cents)} · Total ${usdC(inv.total_cents)}</b></p>
        ${pays ? `<h3>Payments</h3><table class="tbl"><thead><tr><th>Amount</th><th>Method</th><th>Ref</th><th>Received</th><th>By</th></tr></thead><tbody>${pays}</tbody></table>` : ""}
        <div id="pay-form-${id}"></div><h3>Trail</h3>${evs}</div>`;
      box.scrollIntoView({ behavior: "smooth" });
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function billingExportFn(id) {
    try { await Api.program.billingExport(id); } catch (e) { alert(e.message); }
  }

  function billingPayForm(id) {
    const box = document.getElementById(`pay-form-${id}`) || document.getElementById("billing-detail");
    box.insertAdjacentHTML("beforeend", `<form class="inline-form" onsubmit="event.preventDefault(); Views.billingPayRun('${id}', this)">
      <input name="amount" type="number" step="0.01" min="0.01" placeholder="amount $" required />
      <select name="method"><option>ach</option><option>wire</option><option>check</option><option>card</option></select>
      <input name="ref" placeholder="remittance / bank ref" />
      <input name="received_at" type="date" />
      <button class="mini">Record payment</button></form>
      <p class="muted">Split across the invoice's disputes automatically (proportional), so collections roll up per case.</p>`);
  }

  async function billingPayRun(id, form) {
    try {
      await Api.program.billingPay(id, {
        amount_cents: Math.round(Number(form.amount.value) * 100),
        method: form.method.value, ref: form.ref.value,
        received_at: form.received_at.value || undefined,
      });
      billingList(); billingDetail(id);
    } catch (e) { alert(e.message); }
  }

  // ===== AR/AP view (#/arap) =====
  function arapView() {
    if (!can(...finRoles))
      return `<div class="view-head"><h1>AR / AP</h1></div><p class="muted">Requires PM or FINANCE.</p>`;
    afterRender(() => { arapSummaryBox(); arapArBox(); arapApBox(); });
    return `<div class="view-head"><h1>Receivables & payables</h1>
      <span class="muted">AR derived live from invoice stores; AP obligations recorded and settled here</span></div>
      <div id="arap-summary"></div>
      <div class="card"><h2>Accounts receivable — aging</h2><div id="arap-ar"><p class="muted">Loading…</p></div></div>
      <div class="card"><h2>Accounts payable</h2>
        ${can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<form class="inline-form" onsubmit="event.preventDefault(); Views.arapRecord(this)">
          <input name="payee" placeholder="payee" required />
          <input name="amount" type="number" step="0.01" min="0.01" placeholder="amount $" required />
          <select name="source"><option>award</option><option>refund</option><option>vendor</option><option>tax</option><option>other</option></select>
          <input name="case_id" placeholder="case id (optional)" />
          <input name="due_date" type="date" />
          <button class="mini">Record obligation</button></form>` : ""}
        ${can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<p><button class="mini" onclick="Views.arapNacha()">Generate ACH (NACHA) payout file</button>
          <span class="muted"> from open payables that carry bank destinations</span></p>` : ""}
        <p><label class="muted">Status
          <select id="arap-ap-status" onchange="Views.arapApFilter(this.value)">
            ${["OPEN", "SETTLED", "VOID"].map((s) => `<option value="${s}" ${s === "OPEN" ? "selected" : ""}>${s}</option>`).join("")}
          </select></label></p>
        <div id="arap-ap"><p class="muted">Loading…</p></div>
        <div id="arap-nacha"></div></div>`;
  }

  async function arapSummaryBox() {
    const box = document.getElementById("arap-summary");
    try {
      const r = await Api.program.arapSummary();
      box.innerHTML = `<div class="stat-grid">
        <div class="stat"><div class="stat-num">${usdC(r.ar && r.ar.total_cents)}</div><div class="muted">Open receivables</div></div>
        <div class="stat"><div class="stat-num">${usdC(r.ap && r.ap.open_cents)}</div><div class="muted">Open payables</div></div>
        <div class="stat"><div class="stat-num">${usdC(r.net_position_cents)}</div><div class="muted">Net position</div></div></div>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  function agingTable(rows, partyLabel) {
    if (!rows || !rows.length) return '<p class="muted">Nothing open.</p>';
    return `<table class="tbl"><thead><tr><th>${partyLabel}</th><th>Ref</th><th>Case</th><th>Amount</th><th>Due</th><th>Basis</th><th>Days</th><th>Bucket</th></tr></thead><tbody>
      ${rows.map((x) => `<tr class="${x.bucket === "90+" ? "row-breach" : ""}">
        <td>${esc(x.party)}</td><td class="mono">${esc(x.number || x.kind || "")}</td>
        <td>${x.case_id ? `<a href="#/cases/${x.case_id}">${esc(x.case_id.slice(0, 8))}…</a>` : "—"}</td>
        <td>${usdC(x.amount_cents)}</td><td><b>${usdC(x.due_cents)}</b></td>
        <td>${esc(x.basis_date)}</td><td>${x.days_outstanding}</td><td>${badge(x.bucket)}</td></tr>`).join("")}</tbody></table>`;
  }

  function arapArResultsHtml(rows, bucket, q) {
    let shown = rows || [];
    if (bucket) shown = shown.filter((x) => x.bucket === bucket);
    if (q) shown = shown.filter((x) => (x.party || "").toLowerCase().includes(q));
    return agingTable(shown, "Owes");
  }
  function arapArFilter() {
    const bucket = document.getElementById("arap-ar-bucket")?.value || "";
    const q = (document.getElementById("arap-ar-q")?.value || "").trim().toLowerCase();
    const box = document.getElementById("arap-ar-results");
    if (box) box.innerHTML = arapArResultsHtml(window._arapAr || [], bucket, q);
  }

  async function arapArBox() {
    const box = document.getElementById("arap-ar");
    try {
      const r = await Api.program.receivablesAging();
      window._arapAr = r.receivables || [];
      const ag = r.aging || {};
      const buckets = ["current", "1-30", "31-60", "61-90", "90+"];
      box.innerHTML = `<p>${buckets.map((b) => `<span class="chip">${b}: <b>${usdC(ag[b] || 0)}</b></span> `).join("")}</p>
        ${window._arapAr.length ? `<p>
          <select id="arap-ar-bucket" onchange="Views.arapArFilter()"><option value="">All buckets</option>
            ${buckets.map((b) => `<option value="${b}">${b}</option>`).join("")}</select>
          <input id="arap-ar-q" placeholder="Filter by party…" oninput="Views.arapArFilter()" /></p>` : ""}
        <div id="arap-ar-results">${arapArResultsHtml(window._arapAr, "", "")}</div>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function arapApBox(status) {
    status = status || "OPEN";
    const box = document.getElementById("arap-ap");
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const r = await Api.program.payablesList(status);
      const rows = (r.payables || []).map((x) => {
        const canWrite = status === "OPEN" && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
        const acts = canWrite ? `<button class="mini" onclick="Views.arapSettle('${x.id}')">Settle</button>
          <button class="mini danger" onclick="Views.arapVoid('${x.id}')">Void</button>` : "";
        return `<tr><td>${esc(x.party)}</td><td>${badge(x.kind)}</td><td>${usdC(x.amount_cents)}</td>
          <td>${x.case_id ? `<a href="#/cases/${x.case_id}">${esc(x.case_id.slice(0, 8))}…</a>` : "—"}</td>
          <td>${esc(x.basis_date)}</td><td>${x.days_outstanding}</td><td>${badge(x.bucket)}</td><td>${acts}</td></tr>`;
      }).join("");
      const ag = r.aging || {};
      box.innerHTML = `<p>${["current", "1-30", "31-60", "61-90", "90+"].map((b) => `<span class="chip">${b}: <b>${usdC(ag[b] || 0)}</b></span> `).join("")}</p>
        ${rows ? `<table class="tbl"><thead><tr><th>Payee</th><th>Source</th><th>Amount</th><th>Case</th><th>Basis</th><th>Days</th><th>Bucket</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : '<p class="muted">No open obligations.</p>'}`;
      nachaBatchesBox();
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  function saveTextFile(name, text) {
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
    a.download = name; a.click(); URL.revokeObjectURL(a.href);
  }

  async function arapNacha() {
    const box = document.getElementById("arap-nacha");
    try {
      const r = await Api.program.nachaPayout({});
      saveTextFile(r.filename, r.file);
      box.innerHTML = `<p class="ok">Batch ${esc(r.batch_id.slice(0, 8))}… — ${r.entry_count} entries, ${usdC(r.total_cents)} — file <b>${esc(r.filename)}</b> downloaded. Upload it to your bank portal.</p>`;
      arapApBox();
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function nachaBatchesBox() {
    const box = document.getElementById("arap-nacha");
    if (!box || box.dataset.busy) return;
    try {
      const r = await Api.program.payoutBatches();
      const bs = r.batches || [];
      if (!bs.length) return;
      box.innerHTML = `<h3>ACH payout batches</h3>` + bs.map((b) => `<span class="chip">${esc(b.file_reference)} · ${b.entry_count} entries · ${usdC(b.total_cents)} · ${badge(b.status)}
        ${can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<a href="#" onclick="event.preventDefault(); Views.arapNachaFile('${b.id}')">download</a>` : ""}</span> `).join(" ");
    } catch { /* batches are supplementary */ }
  }

  async function arapNachaFile(id) {
    try {
      const r = await Api.program.payoutBatchFile(id);
      saveTextFile(r.filename, r.file);
    } catch (e) { alert(e.message); }
  }

  async function arapRecord(form) {
    try {
      await Api.program.payableCreate({
        payee: form.payee.value, amount_cents: Math.round(Number(form.amount.value) * 100),
        source: form.source.value, case_id: form.case_id.value || undefined,
        due_date: form.due_date.value || undefined,
      });
      form.reset(); arapApBox(); arapSummaryBox();
    } catch (e) { alert(e.message); }
  }

  async function arapSettle(id) {
    const method = prompt("Settle via (ach/wire/check):", "ach");
    if (!method) return;
    const ref = prompt("Remittance / bank reference (recon matches on this):", "");
    try { await Api.program.payableAct(id, "settle", { method, ref: ref || "" }); arapApBox(); arapSummaryBox(); }
    catch (e) { alert(e.message); }
  }

  async function arapVoid(id) {
    if (!confirm("Void this obligation?")) return;
    try { await Api.program.payableAct(id, "void", {}); arapApBox(); arapSummaryBox(); }
    catch (e) { alert(e.message); }
  }

  function arapApFilter(status) { arapApBox(status); }

  // ===== Reconciliation view (#/recon) =====
  function reconView() {
    if (!can(...finRoles))
      return `<div class="view-head"><h1>Reconciliation</h1></div><p class="muted">Requires PM or FINANCE.</p>`;
    afterRender(() => reconBatchList());
    const canWrite = can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    return `<div class="view-head"><h1>Reconciliation</h1>
      <span class="muted">match accounting/bank feeds against platform money records — exact ref, then amount within tolerance and date window; ambiguity always goes to a human</span></div>
      ${canWrite ? `<div class="card"><h2>Import external feed</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.reconImportRun(this)">
          <select name="mapping"><option value="quickbooks">QuickBooks CSV</option><option value="xero">Xero CSV</option>
            <option value="sage">Sage CSV</option><option value="bank_generic">Bank (generic CSV)</option></select>
          <input name="file" type="file" accept=".csv" required />
          <button class="mini">Import</button></form>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.reconFetchRun(this)">
          <input name="feed" placeholder="configured http_json feed name" />
          <button class="mini">Fetch from accounting platform</button>
          <span class="muted">feeds are configured per tenant (reconciliation.http_feeds)</span></form>
        <div id="recon-import-out"></div></div>` : ""}
      <div id="recon-batches"><p class="muted">Loading…</p></div>
      <div id="recon-detail"></div>`;
  }

  async function reconImportRun(form) {
    const out = document.getElementById("recon-import-out");
    out.innerHTML = `<p class="muted">Importing…</p>`;
    try {
      const r = await Api.program.reconImportCsv(form.mapping.value, form.file.files[0]);
      out.innerHTML = `<p>✅ Batch #${r.batch_id} — ${r.items} items (${esc(r.period)}). Running auto-match…</p>`;
      const m = await Api.program.reconMatch(r.batch_id);
      out.innerHTML += `<p>Matched <b>${m.matched}</b> · exceptions <b>${m.exceptions}</b> · unmatched <b>${m.unmatched}</b></p>`;
      reconBatchList();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function reconFetchRun(form) {
    const out = document.getElementById("recon-import-out");
    try {
      const r = await Api.program.reconFetch(form.feed.value);
      out.innerHTML = `<p>✅ Batch #${r.batch_id} — ${r.items} items. Running auto-match…</p>`;
      const m = await Api.program.reconMatch(r.batch_id);
      out.innerHTML += `<p>Matched <b>${m.matched}</b> · exceptions <b>${m.exceptions}</b> · unmatched <b>${m.unmatched}</b></p>`;
      reconBatchList();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  function reconBatchRowsHtml(batches, status) {
    const shown = status ? batches.filter((b) => b.status === status) : batches;
    return shown.length ? `<table class="tbl"><thead><tr><th></th><th>Source</th><th>Period</th><th>Items</th><th>Matched</th><th>Exceptions</th><th>Unmatched</th><th>Status</th><th>Imported by</th><th></th></tr></thead><tbody>` +
      shown.map((b) => `<tr><td>#${b.id}</td><td>${esc(b.source)}</td>
        <td>${esc(b.period_start)} → ${esc(b.period_end)}</td><td>${b.total_items}</td>
        <td>${b.matched}</td><td>${b.exceptions}</td><td>${b.unmatched}</td><td>${badge(b.status)}</td>
        <td>${esc(b.imported_by || "—")}</td>
        <td><button class="mini" onclick="Views.reconOpen(${b.id})">Open</button>
        ${can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<button class="mini" onclick="Views.reconMatchRun(${b.id})">Re-run match</button>` : ""}</td></tr>`).join("") + `</tbody></table>`
      : `<p class="muted">${status ? `No ${status.toLowerCase()} batches.` : "No imports yet."}</p>`;
  }
  function reconBatchFilter(status) {
    const box = document.getElementById("recon-batches-list");
    if (box) box.innerHTML = reconBatchRowsHtml(window._reconBatches || [], status);
  }
  async function reconBatchList() {
    const box = document.getElementById("recon-batches");
    if (!box) return;
    try {
      const r = await Api.program.reconBatches();
      window._reconBatches = r.batches || [];
      const statuses = [...new Set(window._reconBatches.map((b) => b.status))].sort();
      const filter = window._reconBatches.length && statuses.length > 1
        ? `<select onchange="Views.reconBatchFilter(this.value)"><option value="">All statuses</option>
            ${statuses.map((s) => `<option value="${esc(s)}">${esc(s)}</option>`).join("")}</select>` : "";
      box.innerHTML = `<div class="view-head"><h2>Batches</h2>${filter}</div>
        <div id="recon-batches-list">${reconBatchRowsHtml(window._reconBatches, "")}</div>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function reconMatchRun(id) {
    try { const m = await Api.program.reconMatch(id);
      alert(`matched ${m.matched} · exceptions ${m.exceptions} · unmatched ${m.unmatched}`);
      reconBatchList(); reconOpen(id);
    } catch (e) { alert(e.message); }
  }

  async function reconOpen(id) {
    const box = document.getElementById("recon-detail");
    try {
      const r = await Api.program.reconBatch(id);
      const rows = (r.items || []).map((it) => {
        let act = "";
        if (can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") && it.status !== "MATCHED" && it.status !== "IGNORED")
          act = `<button class="mini" onclick="Views.reconResolveFn(${it.id}, 'match')">Match…</button>
            <button class="mini" onclick="Views.reconResolveFn(${it.id}, 'exception')">Exception</button>
            <button class="mini" onclick="Views.reconResolveFn(${it.id}, 'ignore')">Ignore</button>`;
        return `<tr class="${it.status === "EXCEPTION" ? "row-breach" : ""}">
          <td>${esc(it.txn_date)}</td><td>${usdC(it.amount_cents)}</td><td class="mono">${esc(it.reference || "")}</td>
          <td>${esc(it.description || "")}</td><td>${badge(it.status)}</td>
          <td>${it.platform_kind ? `${esc(it.platform_kind)} ${usdC(it.platform_amount_cents)}` : ""} <span class="muted">${esc(it.match_kind || "")}</span></td>
          <td class="muted">${esc(it.note || "")}</td><td>${act}</td></tr>`;
      }).join("");
      box.innerHTML = `<div class="card"><h2>Batch #${id} — ${esc(r.batch.source)}</h2>
        <table class="tbl"><thead><tr><th>Date</th><th>Amount</th><th>Reference</th><th>Description</th><th>Status</th><th>Platform record</th><th>Note</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`;
      box.scrollIntoView({ behavior: "smooth" });
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function reconResolveFn(itemId, action) {
    try {
      if (action === "match") {
        const ev = prompt("Platform financial_event id to match (amounts must tie):");
        if (!ev) return;
        await Api.program.reconResolve(itemId, { action: "match", event_id: Number(ev) });
      } else if (action === "exception") {
        const note = prompt("Exception note (required):");
        if (!note) return;
        await Api.program.reconResolve(itemId, { action: "exception", note });
      } else {
        await Api.program.reconResolve(itemId, { action });
      }
      const det = document.querySelector("#recon-detail h2");
      if (det) { const m = det.textContent.match(/#(\d+)/); if (m) reconOpen(Number(m[1])); }
      reconBatchList();
    } catch (e) { alert(e.message); }
  }

  // ── Third-party administrators ─────────────────────────────────────
  function tpaView() {
    if (!can("TPA", "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Third-party administrators</h1></div><p class="muted">Requires a TPA, CASE_MANAGER, or FINANCE account.</p>`;
    afterRender(tpaBoot);
    return `<div class="view-head"><h1>Third-party administrators</h1>
      <span class="muted">File, pay, and track disputes on behalf of your initiating parties</span></div>
      <div id="tpa-org"></div>
      <div id="tpa-body" style="display:none">
        <div class="card"><h2>Initiating parties (clients)</h2>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaAddClientFn(this)">
            <input name="party_name" placeholder="organization name" required />
            <select name="party_type"><option value="PROVIDER">Provider</option><option value="HEALTH_PLAN">Health plan</option></select>
            <input name="contact_email" type="email" placeholder="contact email" />
            <button class="mini">Add</button></form>
          <div id="tpa-clients"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>File a dispute on behalf of a client</h2>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaFileFn(this)">
            <select name="client_id" id="tpa-file-client" required></select>
            <input name="contact_name" placeholder="client contact for this dispute" />
            <input name="amount" type="number" step="0.01" min="0" placeholder="disputed amount $" />
            <input name="notes" placeholder="notes" />
            <button class="mini">File dispute</button></form>
          <p class="muted">The payment link and upload link come to your TPA contact email — you pay and manage documents on behalf of the initiating party.</p>
          <div id="tpa-file-result"></div></div>
        <div class="card"><h2>Bulk upload — file many disputes at once</h2>
          <p class="muted">CSV with header row: <code>email</code> (required), <code>contact_name</code>, <code>org</code>, <code>amount</code>, <code>filing_party_type</code> (PROVIDER/HEALTH_PLAN), <code>external_ref</code>, <code>notes</code>. Up to 500 rows per batch.</p>
          <form class="inline-form" onsubmit="event.preventDefault()">
            <input id="tpa-bulk-ref" placeholder="batch reference (your idempotency key)" required style="min-width:16em" />
            <input id="tpa-bulk-file" type="file" accept=".csv,text/csv" />
            <button class="mini" id="tpa-bulk-btn" disabled>File batch</button></form>
          <div id="tpa-bulk-preview"></div><div id="tpa-bulk-result"></div></div>
        <div class="card"><h2>My batch filings — status</h2>
          <div id="tpa-batches"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>Tracking — all your parties' disputes</h2>
          <div id="tpa-rollup"></div><div id="tpa-cases-filters"></div>
          <div id="tpa-cases"><p class="muted">Loading…</p></div></div>
      </div>
      ${can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `
      <div class="card"><h2>State oversight — all TPAs</h2><div id="tpa-admin"><p class="muted">Loading…</p></div></div>` : ""}`;
  }

  async function tpaBoot() {
    const orgBox = document.getElementById("tpa-org");
    try {
      const r = await Api.program.tpaMe();
      const code = r.tpa.invite_code;
      const isStaff = can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
      orgBox.innerHTML = `<div class="stat-grid"><div class="stat"><div class="stat-num">${esc(r.tpa.name)}</div>
        <div class="muted">${esc(r.tpa.contact_email)} · ${badge(r.tpa.status)}</div>
        ${isStaff ? `<button class="mini" style="margin-top:8px" onclick="Views.tpaUnlinkFn()">Stop acting as this TPA</button>` : ""}</div></div>
        <div class="card" style="margin-top:10px"><h2>Add a teammate</h2>
          <p class="muted">Give this code to a colleague to let them log into their own portal account and join your organization — Profile → Claim TPA, or the form here when they're not linked to any org yet. Generating a new code replaces the old one.</p>
          <p id="tpa-invite-code">${code ? `<span class="mono" style="font-weight:600;font-size:1.1em">${esc(code)}</span> <button class="mini" onclick="Views.tpaInviteCodeFn()">Generate new code</button>`
            : `<span class="muted">No code pending.</span> <button class="mini" onclick="Views.tpaInviteCodeFn()">Generate invite code</button>`}</p></div>`;
      document.getElementById("tpa-body").style.display = "";
      tpaClientsBox(); tpaDashBox(); tpaBatchesBox();
      window._tpaBulk = { items: [] };
      const bref = document.getElementById("tpa-bulk-ref");
      if (bref && !bref.value) bref.value = "TPA-" + new Date().toISOString().slice(0, 16).replace(/[-:T]/g, "");
      $("#tpa-bulk-file")?.addEventListener("change", (ev) => tpaBulkFile(ev.target));
      $("#tpa-bulk-btn")?.addEventListener("click", (ev) => { ev.preventDefault(); tpaBulkSubmit(ev.target); });
    } catch (e) {
      // CASE_MANAGER excluded: claimTPA (backing "Act as") now 403s for
      // reviewer per the "reviewer only reviews" follow-up, so this form
      // would just fail -- reviewer still sees the oversight table below,
      // read-only.
      orgBox.innerHTML = (can("CASE_MANAGER") && !can("TENANT_ADMIN")) ? "" : `<div class="card"><h2>Link your TPA organization</h2>
        <p class="muted">${can("TPA") ? "Enter the one-time claim code from your TPA registration." : "To act on a TPA's behalf, enter a claim code that TPA has given you. Only the TPA can create one."}</p>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaClaimFn(this)">
          <input name="code" placeholder="TPA-XXXXXX" required /><button class="mini">Claim</button></form></div>`;
    }
    if (can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) tpaAdminBox();
  }

  async function tpaClaimFn(form) {
    try { await Api.program.tpaClaim(form.code.value.trim()); tpaBoot(); }
    catch (e) { alert(e.message); }
  }

  async function tpaInviteCodeFn() {
    try { await Api.program.tpaInviteCode(); tpaBoot(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaClientsBox() {
    const box = document.getElementById("tpa-clients");
    try {
      const r = await Api.program.tpaClients();
      const cs = r.clients || [];
      box.innerHTML = cs.length ? `<table class="tbl"><thead><tr><th>Party</th><th>Type</th><th>Email</th><th>Cases</th><th>Status</th><th></th></tr></thead><tbody>
        ${cs.map((c) => `<tr><td>${esc(c.party_name)}</td><td>${badge(c.party_type)}</td><td>${esc(c.contact_email || "—")}</td>
          <td>${c.case_count}</td><td>${badge(c.status)}</td>
          <td><button class="mini" onclick="Views.tpaClientStatusFn('${c.id}','${c.status === "ACTIVE" ? "SUSPENDED" : "ACTIVE"}')">${c.status === "ACTIVE" ? "Suspend" : "Reactivate"}</button></td></tr>`).join("")}</tbody></table>` : '<p class="muted">No initiating parties yet — add one above.</p>';
      const sel = document.getElementById("tpa-file-client");
      if (sel) sel.innerHTML = cs.filter((c) => c.status === "ACTIVE").map((c) => `<option value="${c.id}">${esc(c.party_name)} (${esc(c.party_type)})</option>`).join("");
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaAddClientFn(form) {
    try {
      await Api.program.tpaAddClient({ party_name: form.party_name.value, party_type: form.party_type.value, contact_email: form.contact_email.value });
      form.reset(); tpaClientsBox();
    } catch (e) { alert(e.message); }
  }

  async function tpaClientStatusFn(id, status) {
    try { await Api.program.tpaClientStatus(id, status); tpaClientsBox(); }
    catch (e) { alert(e.message); }
  }

  async function tpaFileFn(form) {
    const out = document.getElementById("tpa-file-result");
    try {
      const r = await Api.program.tpaIntake({
        client_id: form.client_id.value, contact_name: form.contact_name.value,
        disputed_amount_cents: Math.round(Number(form.amount.value || 0) * 100), notes: form.notes.value,
      });
      out.innerHTML = `<p class="ok">Filed — case <a href="#/cases/${r.case_id}">${esc(r.case_number)}</a> (${esc(r.status)}) for <b>${esc(r.initiating_party)}</b>. ${r.payment_required ? "Payment link sent to your TPA email." : ""}</p>`;
      form.reset(); tpaDashBox(); tpaClientsBox();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  function tpaCasesHtml(cases, status, q) {
    let shown = cases;
    if (status) shown = shown.filter((x) => x.status === status);
    if (q) shown = shown.filter((x) => (x.client_name || "").toLowerCase().includes(q) || (x.case_number || "").toLowerCase().includes(q));
    if (!shown.length) return '<p class="muted">No disputes match this filter.</p>';
    return `<table class="tbl"><thead><tr><th>Initiating party</th><th>Type</th><th>Case</th><th>Status</th><th>Disputed</th><th>Invoiced</th><th>Paid</th><th>Filed</th><th></th></tr></thead><tbody>` +
      shown.map((x) => `<tr><td>${esc(x.client_name)}</td><td>${badge(x.initiating_party_type)}</td>
        <td>${x.case_id ? `<a href="#/cases/${x.case_id}">${esc(x.case_number || x.case_id.slice(0, 8))}</a>` : "—"}</td>
        <td>${badge(x.status)}</td><td>${usdC(x.disputed_amount_cents)}</td><td>${usdC(x.invoiced_cents)}</td><td>${usdC(x.paid_cents)}</td><td>${esc(x.filed)}</td>
        <td>${x.status === "AWAITING_PAYMENT" && window._tpaCanEdit ? `<button class="mini" onclick="Views.tpaEditDisputeFn('${x.case_id}')">Edit</button>` : ""}</td></tr>`).join("")
      + `</tbody></table>`;
  }
  // A TPA may correct its own filing until the fee is paid; after that the
  // server refuses (409) and the row loses its Edit button on the next load.
  async function tpaEditDisputeFn(caseId) {
    const x = (window._tpaCases || []).find((c) => c.case_id === caseId);
    if (!x) return;
    let clients = [];
    try { clients = ((await Api.program.tpaClients()).clients || []).filter((c) => c.status === "ACTIVE" || c.id === x.client_id); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    const v = await UI.modal({
      title: `Edit dispute ${x.case_number || ""}`,
      body: "You can correct this filing until the initial fee is paid. After payment, contact the case team for changes.",
      fields: [
        { name: "client_id", label: "Initiating party", required: true, value: x.client_id,
          options: clients.map((c) => [c.id, `${esc(c.party_name)} (${esc(c.party_type)})`]) },
        { name: "contact_name", label: "Party contact name", value: esc(x.contact_name || "") },
        { name: "amount", label: "Disputed amount (USD)", type: "number", step: "0.01",
          value: x.disputed_amount_cents ? (x.disputed_amount_cents / 100).toFixed(2) : "" },
        { name: "notes", label: "Notes", type: "textarea", value: esc(x.notes || "") },
      ],
    });
    if (!v) return;
    const amount = Number(v.amount || 0);
    if (!Number.isFinite(amount) || amount < 0) { UI.toast("Enter a valid disputed amount", { kind: "warn" }); return; }
    try {
      const r = await Api.program.tpaEditDispute(caseId, {
        client_id: v.client_id, contact_name: v.contact_name, notes: v.notes,
        disputed_amount_cents: Math.round(amount * 100),
      });
      UI.toast((r.changed || []).length ? `Dispute ${r.case_number} updated` : "No changes to save");
      tpaDashBox(); tpaClientsBox();
    } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); tpaDashBox(); }
  }
  function tpaCasesFilter() {
    const status = document.getElementById("tpa-cases-status")?.value || "";
    const q = (document.getElementById("tpa-cases-q")?.value || "").trim().toLowerCase();
    const box = document.getElementById("tpa-cases");
    if (box) box.innerHTML = tpaCasesHtml(window._tpaCases || [], status, q);
  }
  async function tpaDashBox(tpaId) {
    const roll = document.getElementById("tpa-rollup"), box = document.getElementById("tpa-cases"),
      filtersBox = document.getElementById("tpa-cases-filters");
    try {
      const r = await Api.program.tpaDashboard(tpaId);
      window._tpaCases = r.cases || [];
      // Editable only on the caller's own org view, not staff oversight of another TPA.
      window._tpaCanEdit = !tpaId;
      roll.innerHTML = (r.by_client || []).map((x) => `<span class="chip">${esc(x.initiating_party)}: <b>${x.cases}</b> cases (${x.open} open) · invoiced ${usdC(x.invoiced_cents)} · paid ${usdC(x.paid_cents)}</span> `).join("");
      const statuses = [...new Set(window._tpaCases.map((x) => x.status))].sort();
      if (filtersBox) filtersBox.innerHTML = window._tpaCases.length ? `<p>
        <select id="tpa-cases-status" onchange="Views.tpaCasesFilter()"><option value="">All statuses</option>
          ${statuses.map((s) => `<option value="${esc(s)}">${esc(s)}</option>`).join("")}</select>
        <input id="tpa-cases-q" placeholder="Filter by party or case #…" oninput="Views.tpaCasesFilter()" /></p>` : "";
      box.innerHTML = tpaCasesHtml(window._tpaCases, "", "");
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  // TPA bulk upload: same parser and idempotent batch semantics as the
  // staff intake screen, but scoped here so TPA-role users never need staff
  // screens. Batches are recorded under the filer's own subject — "My batch
  // filings" below is the status view.
  function tpaBulkFile(input) {
    const file = input.files && input.files[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      const rows = bulkCsvRows(String(reader.result || ""));
      const head = (rows.shift() || []).map((h) => h.trim().toLowerCase().replace(/^\uFEFF/, ""));
      if (!head.includes("email")) {
        $("#tpa-bulk-preview").innerHTML = `<p class="badge warn">header row must include at least: email</p>`;
        return;
      }
      const items = rows.map((r) => {
        const cell = (name) => { const i = head.indexOf(name); return i >= 0 ? (r[i] || "").trim() : ""; };
        const cents = cell("amount") ? Math.round(parseFloat(cell("amount").replace(/[$,]/g, "")) * 100) : 0;
        return {
          external_ref: cell("external_ref"), email: cell("email"),
          contact_name: cell("contact_name"), org: cell("org"), notes: cell("notes"),
          filing_party_type: cell("filing_party_type").toUpperCase(),
          disputed_amount_cents: Number.isFinite(cents) ? cents : 0,
          qpa_cents: Number.isFinite(cents) ? cents : 0,
        };
      }).filter((it) => it.email);
      window._tpaBulk = { items };
      const bad = rows.length - items.length;
      $("#tpa-bulk-preview").innerHTML =
        `<p class="muted">${items.length} row(s) ready${bad ? ` — ${bad} row(s) skipped (no email)` : ""}${items.length > 500 ? " — <b>over the 500-row limit, split the file</b>" : ""}.</p>`;
      $("#tpa-bulk-btn").disabled = !items.length || items.length > 500;
    };
    reader.readAsText(file);
  }

  async function tpaBulkSubmit(btn) {
    const ref = ($("#tpa-bulk-ref")?.value || "").trim();
    if (!ref) { UI.toast("batch reference required — it is the idempotency key", { kind: "warn" }); return; }
    const { items } = window._tpaBulk || { items: [] };
    if (!items.length) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.intakeBulk({ batch_ref: ref, items });
        const rows = (r.results || []).map((x) => `<tr>
          <td class="muted">${esc(x.external_ref || "")}</td>
          <td>${x.status === "CREATED" ? badge("CREATED") : `<span class="badge warn">ERROR</span>`}${x.payment_required ? ` <span class="badge warn">fee due</span>` : ""}</td>
          <td>${x.case_number ? `<a href="#/cases/${x.case_id}">${esc(x.case_number)}</a>` : esc(x.intake_id || "")}</td>
          <td class="muted">${esc(x.error || "")}</td></tr>`).join("");
        $("#tpa-bulk-result").innerHTML = `<p><b>Batch ${esc(r.batch_ref || ref)}</b> — ${r.created} filed, ${r.errors} error(s)${r.idempotent_replay ? " — <b>idempotent replay</b>: already submitted; nothing re-filed" : ""}</p>
          <table><thead><tr><th>Row</th><th>Outcome</th><th>Intake / case</th><th>Error</th></tr></thead><tbody>${rows}</tbody></table>`;
        UI.toast(r.idempotent_replay ? "Batch already filed — receipt replayed" : `Batch filed: ${r.created} created, ${r.errors} errors`, { kind: r.errors ? "warn" : "ok" });
        tpaBatchesBox();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Filing batch…");
  }

  async function tpaBatchesBox() {
    const box = document.getElementById("tpa-batches");
    if (!box) return;
    try {
      const r = await Api.program.intakeBatches();
      const bs = r.batches || [];
      box.innerHTML = bs.length ? `<table><thead><tr><th>Batch ref</th><th>Submitted</th><th>Rows</th><th>Filed</th><th>Errors</th><th></th></tr></thead><tbody>
        ${bs.map((b) => `<tr><td class="mono">${esc(b.batch_ref)}</td><td>${esc(String(b.created_at || "").slice(0, 16).replace("T", " "))}</td>
          <td>${b.item_count}</td><td>${b.created_count}</td><td>${b.error_count ? `<span class="badge warn">${b.error_count}</span>` : "0"}</td>
          <td><button class="mini ghost" onclick="Views.tpaBatchDetail(${b.id})">Receipt</button></td></tr>
          <tr id="tpa-batch-${b.id}" class="row-detail" style="display:none"><td colspan="6"></td></tr>`).join("")}</tbody></table>`
        : '<p class="muted">No batches yet — upload a CSV above.</p>';
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaBatchDetail(id) {
    const row = document.getElementById(`tpa-batch-${id}`);
    if (!row) return;
    if (row.style.display !== "none") { row.style.display = "none"; return; }
    row.style.display = "";
    row.firstElementChild.innerHTML = `<p class="muted">Loading receipt…</p>`;
    try {
      const r = await Api.program.intakeBatch(id);
      const rows = (r.results || []).map((x) => `<tr>
        <td class="muted">${esc(x.external_ref || "")}</td>
        <td>${x.status === "CREATED" ? badge("CREATED") : `<span class="badge warn">ERROR</span>`}</td>
        <td>${x.case_number ? `<a href="#/cases/${x.case_id}">${esc(x.case_number)}</a>` : esc(x.intake_id || "—")}</td>
        <td class="muted">${esc(x.error || "")}</td></tr>`).join("");
      row.firstElementChild.innerHTML = `<b>${esc(r.batch_ref)}</b> · submitted ${esc(String(r.submitted_at || "").slice(0, 16).replace("T", " "))} · ${r.created} filed / ${r.errors} errors
        <table><thead><tr><th>Row</th><th>Outcome</th><th>Case</th><th>Error</th></tr></thead><tbody>${rows}</tbody></table>`;
    } catch (e) { row.firstElementChild.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaAdminBox() {
    const box = document.getElementById("tpa-admin");
    if (!box) return;
    try {
      const r = await Api.program.adminTpas();
      // CASE_MANAGER excluded per the "reviewer only reviews" follow-up --
      // matches adminSetTPAStatus/claimTPA's backend
      // gates. Reviewer still sees this table, just not the action buttons.
      const canAct = can("FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN");
      const rows = (r.tpas || []).map((t) => `<tr><td>${esc(t.name)}</td><td>${esc(t.contact_email)}</td><td>${t.clients}</td><td>${t.cases_filed}</td>
        <td>${badge(t.status)}</td>
        <td>${canAct ? `<button class="mini ${t.status === "ACTIVE" ? "danger" : ""}" onclick="Views.tpaAdminStatusFn('${t.id}','${t.status === "ACTIVE" ? "SUSPENDED" : "ACTIVE"}')">${t.status === "ACTIVE" ? "Suspend" : "Reactivate"}</button>` : ""}</td></tr>`).join("");
      box.innerHTML = rows ? `<table class="tbl"><thead><tr><th>TPA</th><th>Contact</th><th>Clients</th><th>Cases</th><th>Status</th><th></th></tr></thead><tbody>${rows}</tbody></table>
${canAct ? `<p class="muted" style="margin-top:6px">To act on a TPA's behalf, ask the TPA for a claim code -- they generate it on their own TPA page -- and enter it under "Link your TPA organization" above. Staff cannot see or create a TPA's codes.</p>` : ""}`
        : '<p class="muted">No TPAs registered.</p>';
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaAdminStatusFn(id, status) {
    try { await Api.program.adminTpaStatus(id, status); tpaAdminBox(); }
    catch (e) { alert(e.message); }
  }

  // Undo a claim made with a TPA-supplied code -- back to the oversight-only
  // view of #/tpa (the claim form + admin list, same as never having
  // claimed anything).
  async function tpaUnlinkFn() {
    if (!(await UI.confirm("Stop acting as this TPA?", "Unlinks your own account from this organization. You'll go back to the oversight list; claim a code again any time to act on a TPA's behalf.", "Unlink"))) return;
    try { await Api.program.tpaUnlink(); UI.toast("Unlinked"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ── Physical check intake + scan preview ───────────────────────────
  function checksView() {
    if (!can(...finRoles))
      return `<div class="view-head"><h1>Checks</h1></div><p class="muted">Requires PM or FINANCE.</p>`;
    afterRender(() => checksBox(""));
    return `<div class="view-head"><h1>Checks</h1>
      <span class="muted">photo/scan → MICR + OCR extraction → invoice match → clearing</span></div>
      <div class="card"><h2>Deposit a check</h2>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.checkUploadFn(this)">
          <input name="check" type="file" accept="image/jpeg,image/png,image/tiff,image/webp" required />
          <button class="mini">Upload & scan</button></form>
        <div id="check-upload-result"></div></div>
      <div class="card"><h2>Scanned checks</h2>
        <p>${["", "RECEIVED", "PROCESSED", "MATCHED", "REVIEW", "CLEARED", "REJECTED"].map((st) =>
          `<button class="mini" onclick="Views.checksFilter('${st}')">${st || "All"}</button>`).join(" ")}</p>
        <div id="checks-list"><p class="muted">Loading…</p></div></div>
      <div class="card" id="check-detail-card" style="display:none"><h2>Scan preview</h2>
        <div id="check-detail"></div></div>`;
  }

  async function checksBox(status) {
    const box = document.getElementById("checks-list");
    try {
      const r = await Api.program.checks(status, { limit: 500 });
      const cs = r.checks || [];
      box.innerHTML = cs.length ? `<table class="tbl"><thead><tr><th>Received</th><th>Status</th><th>Amount</th><th>Payer</th><th>Check #</th><th>Case</th><th>Confidence</th><th></th></tr></thead><tbody>
        ${cs.map((c) => `<tr class="${c.amount_mismatch ? "row-breach" : ""}">
          <td>${esc((c.created_at || "").slice(0, 10))}</td><td>${badge(c.status)}</td>
          <td>${usdC(c.courtesy_amount_cents)}</td><td>${esc(c.payer_name || "—")}</td>
          <td>${esc(c.check_number || "—")}</td>
          <td>${c.case_id ? `<a href="#/cases/${c.case_id}">${esc(c.case_number || c.case_id.slice(0, 8) + "…")}</a>` : "—"}</td>
          <td>${badge(c.confidence || "—")}${c.amount_mismatch ? " ⚠ mismatch" : ""}</td>
          <td><button class="mini" onclick="Views.checkOpen('${c.id}')">Preview</button></td></tr>`).join("")}</tbody></table>`
        : '<p class="muted">No checks scanned yet.</p>';
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function checkUploadFn(form) {
    const out = document.getElementById("check-upload-result");
    const f = form.check.files[0];
    if (!f) return;
    out.innerHTML = '<p class="muted">Uploading — extraction runs asynchronously…</p>';
    try {
      const r = await Api.program.uploadCheck(f);
      out.innerHTML = `<p class="ok">Received (${esc(r.check_id.slice(0, 8))}…) — scan queued. Refresh in a few seconds to see the extraction.</p>`;
      form.reset();
      setTimeout(() => checksBox(""), 4000);
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function checkOpen(id) {
    const card = document.getElementById("check-detail-card"), box = document.getElementById("check-detail");
    card.style.display = "";
    box.innerHTML = '<p class="muted">Loading scan…</p>';
    card.scrollIntoView({ behavior: "smooth" });
    try {
      const r = await Api.program.checks("", { limit: 500 });
      const c = (r.checks || []).find((x) => x.id === id);
      const imgUrl = await Api.program.checkImage(id).catch(() => null);
      const mask = (v) => v ? "••••" + esc(String(v).slice(-4)) : "—";
      box.innerHTML = `
        ${imgUrl ? `<p><img src="${imgUrl}" alt="check scan" style="max-width:100%;border:1px solid var(--line,#ccc);border-radius:8px" /></p>` : '<p class="muted">Image unavailable.</p>'}
        ${c ? `<div class="stat-grid">
          <div class="stat"><div class="stat-num">${usdC(c.courtesy_amount_cents)}</div><div class="muted">Courtesy amount</div></div>
          <div class="stat"><div class="stat-num">${usdC(c.legal_amount_cents)}</div><div class="muted">Legal amount</div></div>
          <div class="stat"><div class="stat-num">${badge(c.confidence || "—")}</div><div class="muted">Confidence</div></div></div>
        <table class="tbl"><tbody>
          <tr><td>Status</td><td>${badge(c.status)}${c.amount_mismatch ? ' <span class="error">⚠ courtesy/legal mismatch — REVIEW</span>' : ""}</td></tr>
          <tr><td>Routing</td><td class="mono">${mask(c.routing_number)}</td></tr>
          <tr><td>Account</td><td class="mono">${mask(c.account_number)}</td></tr>
          <tr><td>Check #</td><td>${esc(c.check_number || "—")}</td></tr>
          <tr><td>Date</td><td>${esc(c.check_date || "—")}</td></tr>
          <tr><td>Payer</td><td>${esc(c.payer_name || "—")}</td></tr>
          <tr><td>Memo</td><td>${esc(c.memo || "—")}</td></tr>
          <tr><td>Case</td><td>${c.case_id ? `<a href="#/cases/${c.case_id}">${esc(c.case_number || c.case_id.slice(0, 8) + "…")}</a>` : "—"}</td></tr></tbody></table>
        ${c.status === "MATCHED" && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `
          <form class="inline-form" onsubmit="event.preventDefault(); Views.checkClearFn('${c.id}', this)">
            <input name="ref" placeholder="bank deposit / lockbox ref" />
            <button class="mini">Confirm cleared</button></form>` : ""}
        ${c.status === "REVIEW" && can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `
          <form class="inline-form" onsubmit="event.preventDefault(); Views.checkMatchFn('${c.id}', this)">
            <input name="case_number" placeholder="case number (e.g. FL26-000123)" required />
            <input name="amount_cents" type="number" step="0.01" min="0" placeholder="${c.courtesy_amount_cents ? "amount $" : "amount $ (required — not extracted)"}" />
            <button class="mini">Link to case</button></form>
          <p class="muted">Didn't auto-match — ${c.amount_mismatch ? "courtesy/legal amount mismatch" : !c.courtesy_amount_cents ? "amount wasn't extracted" : c.confidence === "low" ? "low OCR confidence" : "no single open invoice matched"}. Confirm the case number (and amount, if blank) to link it manually.</p>` : ""}` : '<p class="error">check metadata not found</p>'}`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function checkClearFn(id, form) {
    try {
      await Api.program.clearCheck(id, form.ref.value || "");
      document.getElementById("check-detail-card").style.display = "none";
      checksBox("");
    } catch (e) { alert(e.message); }
  }

  async function checkMatchFn(id, form) {
    try {
      const body = { case_number: form.case_number.value.trim() };
      if (form.amount_cents.value) body.amount_cents = Math.round(parseFloat(form.amount_cents.value) * 100);
      await Api.program.matchCheck(id, body);
      UI.toast("Check linked — now awaiting clearing");
      document.getElementById("check-detail-card").style.display = "none";
      checksBox("");
    } catch (e) {
      // 409 with "candidates" means the case has more than one open invoice --
      // show them so staff can pick, rather than a bare error message.
      if (e.data?.candidates?.length) {
        const list = e.data.candidates.map((c) => `${c.party} ${c.kind}: $${((c.amount_cents || 0) / 100).toLocaleString()} (invoice ${c.invoice_id})`).join("\n");
        alert(`${e.message}\n\n${list}\n\nRe-submit with the exact invoice_id via the API, or ask an admin.`);
      } else {
        alert(e.message);
      }
    }
  }

  async function flReportFn(form) {
    try {
      if (form.kind.value === "weekly") await Api.program.flWeeklyReport(form.from.value, form.to.value);
      else if (form.kind.value === "final") await Api.program.flFinalReport();
      else await Api.program.flMonthlyReport(form.from.value, form.to.value);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function era835Fn(caseId, docId) {
    try { await Api.program.documentAs835(caseId, docId); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
  }
  async function flAdhocBox() {
    const box = document.getElementById("adhoc-cols");
    if (!box) return;
    try {
      const r = await Api.program.flReportColumns();
      box.innerHTML = `<fieldset class="elig-flags"><legend>Columns</legend>${(r.columns || []).map((c) =>
        `<label><input type="checkbox" name="col" value="${esc(c)}" /> ${esc(c)}</label>`).join("")}</fieldset>
        <fieldset class="elig-flags"><legend>FL AHCA Status (none ticked = all)</legend>${(r.statuses || []).map((c) =>
        `<label><input type="checkbox" name="st" value="${esc(c)}" /> ${esc(c)}</label>`).join("")}</fieldset>`;
    } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
  }
  async function flAdhocFn(form) {
    const ticked = (name) => [...form.querySelectorAll(`input[name="${name}"]:checked`)].map((i) => i.value);
    try { await Api.program.flAdhocReport({ columns: ticked("col").join("|"), status: ticked("st").join("|"), from: form.from.value, to: form.to.value }); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  return { dashboard, cases, caseDetail, newDispute, sortCases, onboarding, onboardingFilter, onboardingNew, decide, voice, reports, showAnalysis, retryAnalysis, check, assign, letter, saveCurrentView, escalate, relate, feeTransfer, peek, askGraph, rptSlaFilter, voiceIntakeFilter, voiceLogFilter, settleInvoice, qaQueue, qaReview, qaDecide, intake, intakeFilter, newIntake, advanceIntake, deliverables, submitDeliverable, requestDeliverable, finance, payInvoice, moveDoc, downloadDoc, downloadZip, rulesAdmin, rulesAuditFilter, ruleEdit, ruleDelete, rulesSave, bindRulesAdmin, uploadCheck, clearCheck, requestRescan, copilotBrief, copilotDraftQA, copilotPropose, copilotDecideBatch, assistant, assistantChip, mountAssistantFab, handleAssistantMessage, asstQaDecide, asstShareAdd, asstShareRemove, assistantTool, asstRequestUpload, asstCheck, asstSendMail, asstClearCheck, corrTemplateBody, timeAdd, myTimesheet, myTimeRange, myTimeListBox, myTimeAdd, myTimeEditStart, myTimeSave, myTimeDel, timeReport, timeReportRun, timeReportSend, timeReportCsv, timeRateSet, asstTimeAdd, bulkIntakeFile, bulkIntakeSubmit, createTenantFlow, renameTenantFlow, createFederalAdminFlow, addTenantStaffFlow, teamAdmin, setStaffEnabled, deleteStaffMember, auditLog, opsDashboard, billingInvoices, billingGen, billingActFn, billingDetail, billingExportFn, billingPayForm, billingPayRun, billingFilter: (f) => billingList({ status: f.status.value, start: f.start.value, end: f.end.value }), arapView, arapRecord, arapSettle, arapVoid, arapApFilter, arapArFilter, arapNacha, arapNachaFile, reconView, reconImportRun, reconFetchRun, reconMatchRun, reconOpen, reconResolveFn, reconBatchFilter, flReportFn, checksView, checksFilter: (st) => checksBox(st), checkUploadFn, checkOpen, checkClearFn, checkMatchFn, tpaView, tpaClaimFn, tpaUnlinkFn, tpaInviteCodeFn, tpaAddClientFn, tpaClientStatusFn, tpaFileFn, tpaDashBox, tpaCasesFilter, tpaEditDisputeFn, tpaAdminStatusFn, tpaBulkFile, tpaBulkSubmit, tpaBatchesBox, tpaBatchDetail, mailLog, mailLogFilter, mailLogBody, saveReviewNote, saveDisputeSummary, confirmAor, eligChecklist, wfAct, wfAnytime, fwaAct, saveContractDates, myTimePromptFill, saveCaseInfo, saveMailRecipients, qaSendFn, flAdhocFn, era835Fn };
})();
