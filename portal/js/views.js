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
  // A plain date (YYYY-MM-DD: a due date, a date of service) is a calendar
  // day, not midnight UTC -- shown as that day, with no time, in every zone.
  // A date with no time of its own (a date column, often sent as midnight UTC) reads as a date, never "…, 1:00 AM".
  const dateOnly = (d) => typeof d === "string" && (/^\d{4}-\d{2}-\d{2}$/.test(d) || /^\d{4}-\d{2}-\d{2}T00:00:00(\.0+)?(Z|[+-]00:?00)$/.test(d));
  const fmtDate = (d) => (!d ? "—" : dateOnly(d)
    ? new Date(d.slice(0, 10) + "T12:00:00Z").toLocaleDateString("en-US", { dateStyle: "medium", timeZone: "UTC" })
    : new Date(d).toLocaleString("en-US", { dateStyle: "medium", timeStyle: "short" }));
  const badge = (s) => `<span class="badge s-${esc(s).toLowerCase().replace(/_/g, "-")}">${esc(s)}</span>`;
  const err = (e) => `<div class="err-box"><b>Something didn't load.</b> ${esc(e.message)} <button class="mini" onclick="App.rerender()">Retry</button></div>`;
  const role = (r) => (Auth.claims()?.roles || []).includes(r);
  const can = (...rs) => Auth.can(...rs);
  // An outside organization's login (a TPA, a health plan) with no staff role.
  const STAFF_ROLES = ["CASE_MANAGER", "PM", "FINANCE", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "ARBITRATOR",
    "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"];
  const outsideOnly = () => can("TPA", "HEALTH_PLAN", "PARTY") && !can(...STAFF_ROLES);
  const outsideHome = () => (can("TPA") ? "#/tpa" : can("HEALTH_PLAN") ? "#/healthplan" : "#/party");
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
  // The status a case row shows: a programmed case's Internal Status (with the
  // agency status on hover), else its status.
  const caseBadge = (c) => c.internal_status
    ? `<span title="${esc(c.agency_status ? "Agency: " + c.agency_status : "")}">${badge(c.internal_status)}</span>` : badge(c.status);
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
        and — when the filer asked for one — the full review cost estimate from the Estimated Cost Matrix.${ds.by ? `<br/><span class="form-stamp">Last saved by ${esc(ds.by)}, ${fmtDate(ds.at)}</span>` : ""}</p>
      <form class="form" onsubmit="event.preventDefault(); Views.saveDisputeSummary('${id}',this)">
        <div class="field-group"><h4>Health plan</h4>
          <div class="field-grid">
            <label>Health plan type
              <input name="plan_type" placeholder="e.g. Medicaid managed care" value="${esc(ds.plan_type || (c.details || {}).plan_type || "")}" ${ro} />
              ${!ds.plan_type && (c.details || {}).plan_type ? `<span class="field-hint">as ticked on the filing packet — verify against the letterhead</span>` : ""}</label>
            <label><input type="checkbox" name="verified" ${ds.plan_type_verified_from_letterhead ? "checked" : ""} ${ro} /> Verified from the plan's letterhead</label>
            <label><input type="checkbox" name="coding" ${ds.coding_review_required ? "checked" : ""} ${ro} /> Coding review required</label>
          </div></div>
        <div class="field-group"><h4>Dispute</h4>
          <label>Summary of the dispute
            <textarea name="summary" rows="4" placeholder="What is in dispute and why, in a sentence or two" ${mine ? "" : "readonly"}>${esc(ds.summary || "")}</textarea></label></div>
        <div class="field-group"><h4>Full review cost estimate <span class="field-hint">— only when the filer asked for one</span></h4>
          <div class="field-grid">
            ${matrix.length ? `<label>From the Estimated Cost Matrix
              <select name="matrix" ${ro} onchange="if(this.value){const [c,l]=this.value.split('|');this.form.estimate.value=(c/100).toFixed(2);this.form.basis.value=l;}">
              <option value="">Choose a row…</option>
              ${matrix.map((m) => `<option value="${esc(m.cents)}|${esc(m.label)}">${esc(m.label)} — ${usdC(m.cents)}</option>`).join("")}</select></label>` : ""}
            <label>Estimate ($)
              <input name="estimate" type="number" step="0.01" min="0" placeholder="0.00" value="${est}" ${ro} /></label>
            <label>Basis for the estimate
              <input name="basis" placeholder="matrix row or reason" value="${esc(ds.estimate_basis || "")}" ${ro} /></label>
          </div></div>
        ${mine ? `<button class="mini">Save Dispute Summary</button>` : ""}</form></details>`;
  }
  // Documents that disagree with what the case carries: the case value is
  // kept, the disagreement is listed here and sends the review for a second look.
  function sopConflictsHtml(c) {
    const cs = (c.details && c.details.eligibility_conflicts) || [];
    if (!cs.length) return "";
    const show = (f, v) => (f === "disputed_amount_cents" ? usdC(v) : f === "provider_type" ? providerTypeLabel(v)
      : f === "contracted" ? (v === true || v === "true" ? "in the plan's network" : "not in the plan's network") : String(v));
    const name = (f) => (f === "contracted" ? "network status" : String(f).replace(/_cents$/, "").replace(/_/g, " "));
    return `<div class="err-box"><b>⚠ Documents disagree with the case.</b> The case value was kept — check which is right before deciding.
      <ul>${cs.map((x) => `<li>${esc(name(x.field))}: case has <b>${esc(show(x.field, x.case_value))}</b>,
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
        <div class="field-grid">
          ${sel("line_of_business", "Line of business", fs.line_of_business)}
          ${sel("disputed_issue", "Disputed issue", fs.disputed_issue)}
          ${sel("out_of_network", "Out-of-network provider", fs.out_of_network || ["Yes", "No"])}
          ${inp("claims_submitted", "Claims in the filing packet" + (d.claims_detected && !d.claims_submitted ? ` <span class="field-hint">— the documents show ${esc(d.claims_detected)}</span>` : ""), 'type="number" min="0"')}
        </div>
        <h4 class="sub">Provider and health plan</h4><div class="field-grid">
          ${inp("provider_org", "Provider organization")}${inp("provider_contact", "Provider contact")}${inp("health_plan", "Health plan")}
        </div>
        <h4 class="sub">Provider address</h4><div class="field-grid">
          ${inp("provider_street", "Street address", '')}${inp("provider_city", "City")}
          ${inp("provider_state", "State", 'maxlength="2" placeholder="FL"')}${inp("provider_postal", "Postal code", 'placeholder="12345"')}
        </div>
        ${mine ? `<button class="mini">Save case information</button>` : ""}</form>${sopPacketFactsHtml(d)}</details>`;
  }
  // The rest of what the filing packet (and the remittances / UB-04) stated, read by document analysis: shown so
  // nobody re-types it, and so the unticked attestations and missing required documents stand out.
  const PACKET_FACTS = [
    ["filing_party_type", "Filing party"], ["provider_legal_entity", "Legal entity"], ["provider_trade_name", "Trade name"],
    ["provider_npi", "NPI"], ["provider_tin", "Tax ID"], ["provider_license", "License no."], ["provider_phone", "Telephone"],
    ["provider_fax", "Fax"], ["provider_email", "E-mail"], ["provider_alt_address", "Contact address"],
    ["packet_provider_type", "Provider type (packet)"], ["provider_type", "Provider type (program)"],
    ["health_plan_trade_name", "Plan trade name"], ["plan_type", "Health plan type"], ["contracted", "Contracted with the plan"],
    ["internal_dispute_exhausted", "Internal dispute exhausted"], ["type_of_service", "Type of service"],
    ["fee_estimate_requested", "Full review estimate requested"], ["aor_representative", "Appointed representative"],
    ["attested_by_name", "Attested by"], ["attested_by_title", "Title"], ["attested_on", "Attested on"],
  ];
  const PACKET_POSITIONS = [
    ["provider_position_basis", "Provider — basis and amount"], ["provider_position_references", "Provider — contract references"],
    ["provider_position_rebuttal", "Provider — rebuttal"], ["plan_position_basis", "Health plan — basis for overpayment"],
    ["plan_position_references", "Health plan — references"], ["plan_position_rebuttal", "Health plan — rebuttal"],
  ];
  const PACKET_DOCS = { claim_forms: "Claim form(s)", denials_eobs: "Denials / EOBs", florida_license: "Florida license",
    final_determination_letter: "Final determination letter" };
  // The filing packet form (js/packetform.js) on a screen: a block with its PDF picker, its init, and reading it
  // back for a submit (null when the filer did not use it, false when it has problems -- shown in `out`).
  function packetBlockHtml(hostId, summary) {
    return `<details class="pf-wrap"><summary>${summary}</summary>${PacketForm.pdfPicker("#" + hostId)}<div id="${hostId}"></div></details>`;
  }
  function packetInit(hostId, values, opts = {}) {
    const h = document.getElementById(hostId);
    if (!h) return;
    h.innerHTML = PacketForm.html(values || {}, { id: hostId + "-pf", ...opts });
    PacketForm.bind(h);
    if (opts.readOnly) h.querySelectorAll("input,textarea,button").forEach((x) => { x.disabled = true; });
    // A packet PDF read into the form also fills the filing screen's own short fields that are still empty.
    if (h.dataset.shortBound) return;
    h.dataset.shortBound = "1";
    // Typing the other side's name or email into the packet form fills the short form's fields as well.
    h.addEventListener("input", (ev) => {
      if (!/^d\.(health_plan|health_plan_email|provider_org|provider_email|provider_npi)$/.test(ev.target.name || "")) return;
      const f = h.closest(".card")?.querySelector("form");
      if (f) packetToShortForm(f, PacketForm.read(h).packet.details, opts.partyFixed === "HEALTH_PLAN");
    });
    h.addEventListener("packetfilled", (ev) => {
      const f = h.closest(".card")?.querySelector("form");
      if (!f) return;
      const d = ev.detail.details || {}, plan = opts.partyFixed === "HEALTH_PLAN";
      const set = (name, v) => { const el = f.elements[name]; if (el && !el.value && v != null && v !== "") el.value = v; };
      if (ev.detail.disputed_amount_cents) set("amount", (ev.detail.disputed_amount_cents / 100).toFixed(2));
      packetToShortForm(f, d, plan);
      set("contact_name", d.provider_contact);
      if (d.contracted != null) set("in_network", d.contracted ? "yes" : "no");
      const ptype = { "Rural Hospital": "rural_hospital", "Physician/Medical Group": "physician_dentist", Dentist: "physician_dentist" }[d.packet_provider_type]
        || (d.packet_provider_type === "Hospital" ? ({ "Hospital Inpatient": "hospital_inpatient", "Hospital Outpatient": "hospital_outpatient" }[d.type_of_service]) : "");
      if (ptype) set("provider_type", ptype);
    });
  }
  // The other side, from the packet, into a filing screen's own fields that are still empty: the health plan and
  // its notice email (TPA, staff intake), or the provider, its email and NPI (a health plan filing).
  function packetToShortForm(f, d, providerIsOther) {
    const set = (name, v) => { const el = f.elements[name]; if (el && !el.value && v) el.value = v; };
    set("other_party_name", providerIsOther ? (d.provider_org || d.provider_legal_entity) : d.health_plan);
    set("other_party_email", providerIsOther ? d.provider_email : d.health_plan_email);
    if (providerIsOther) set("other_party_npi", d.provider_npi);
  }
  function packetFrom(hostId, out) {
    const h = document.getElementById(hostId);
    if (!h || !PacketForm.touched(h)) return null;
    const { packet, problems } = PacketForm.read(h);
    if (problems.length) { out.innerHTML = `<p class="error">Fix the filing packet: ${esc(problems.join("; "))}</p>`; return false; }
    return packet;
  }
  const packetErr = (e) => esc(e.message) + (e.problems ? ": " + esc(e.problems.join("; ")) : "");
  // The staff case page's "Filing packet": the packet's sections as the filer entered them or as document analysis
  // read them from the uploaded packet; the Reviewer or PM corrects and saves (their values replace the case's).
  const PACKET_EDITORS = ["CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"];
  function sopPacketFormHtml(id, c) {
    const staff = can(...PACKET_EDITORS);
    // Its own claims fetch: the program panel's wiring can run before this section is on the page.
    setTimeout(async () => {
      const values = { details: c.details || {}, disputed_amount_cents: c.disputed_amount_cents };
      packetInit("p-packet", values, { readOnly: !staff });
      const claims = (await Api.program.claims(id).catch(() => ({}))).claims || [];
      const h = document.getElementById("p-packet");
      if (h && claims.length && !PacketForm.touched(h)) packetInit("p-packet", { ...values, claims }, { readOnly: !staff });
    }, 0);
    return `<details class="prog-sec" id="p-packet-details"><summary>Filing packet</summary>
      <p class="muted">The filing packet's sections — as the filer entered them, or as read from the uploaded packet.
        ${staff ? "Correct anything here and save: your values replace the case's." : "Kept by the Reviewer and the PM."}</p>
      ${staff ? PacketForm.pdfPicker("#p-packet") : ""}<div id="p-packet"></div>
      ${staff ? `<button class="mini" onclick="Views.savePacketForm('${id}', this)">Save filing packet</button>` : ""}
      <div id="p-packet-out"></div></details>`;
  }
  async function savePacketForm(id, btn) {
    const out = document.getElementById("p-packet-out"), h = document.getElementById("p-packet");
    const { packet, problems } = PacketForm.read(h);
    if (problems.length) { out.innerHTML = `<p class="error">Fix the filing packet: ${esc(problems.join("; "))}</p>`; return; }
    await UI.run(btn, async () => {
      try {
        const r = await Api.cases.filingPacket(id, packet);
        out.innerHTML = "";
        UI.toast(`Filing packet saved — ${r.fields.length} field(s), ${r.claims} claim(s)`);
        App.rerender();
      } catch (e) { out.innerHTML = `<p class="error">${packetErr(e)}</p>`; }
    }, "Saving…");
  }
  function sopPacketFactsHtml(d) {
    const val = (v) => (v === true ? "Yes" : v === false ? "No" : String(v));
    const rows = PACKET_FACTS.filter(([k]) => d[k] != null && d[k] !== "")
      .map(([k, label]) => `<tr><th>${esc(label)}</th><td>${esc(k === "provider_type" ? providerTypeLabel(d[k]) : val(d[k]))}</td></tr>`);
    const pos = PACKET_POSITIONS.filter(([k]) => d[k]).map(([k, label]) => `<h5>${esc(label)}</h5><p class="prewrap">${esc(d[k])}</p>`);
    const notes = [];
    const att = d.packet_attestation;
    if (att && typeof att === "object") {
      const unticked = Object.entries(att).filter(([k, v]) => !v && k !== "rural_hospital").map(([k]) => ELIG_REASON_LABELS[k] || k.replace(/_/g, " "));
      if (unticked.length) notes.push(`Not attested (the filer left these unticked): ${unticked.map(esc).join("; ")}`);
      if (att.rural_hospital) notes.push("Filed as a rural hospital — rural hospital documentation should be attached");
    }
    if (Array.isArray(d.packet_documents_submitted)) {
      const missing = Object.keys(PACKET_DOCS).filter((k) => !d.packet_documents_submitted.includes(k)).map((k) => PACKET_DOCS[k]);
      if (missing.length) notes.push(`Required documentation not ticked: ${missing.map(esc).join(", ")}`);
    }
    for (const cf of Array.isArray(d.claim_conflicts) ? d.claim_conflicts : []) {
      notes.push(cf.field === "same_service_as"
        ? `Claim ${esc(cf.claim_number)} is the same service, date and charge as claim ${esc(cf.case_value)} — a corrected or replacement claim?`
        : `Claim ${esc(cf.claim_number)}: ${esc(cf.document)} says ${esc(cf.field.replace(/_/g, " "))} ${esc(cf.document_value)}, the claims table has ${esc(cf.case_value)}`);
    }
    if (!rows.length && !pos.length && !notes.length) return "";
    return `<h4 class="sub">Read from the filing packet and remittances</h4>
      ${notes.length ? `<ul class="warn-list">${notes.map((n) => `<li>${n}</li>`).join("")}</ul>` : ""}
      ${rows.length ? `<table class="tbl kv"><tbody>${rows.join("")}</tbody></table>` : ""}
      ${pos.length ? `<details><summary>Positions stated in the packet</summary>${pos.join("")}</details>` : ""}
      <p class="muted">Read by document analysis — check against the documents before relying on it.</p>`;
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
    if (!App.feature("fwa_review")) return ""; // turned off by a tenant admin (Feature toggles)
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
  // A document step's timing: met (and when), or its due date with a live countdown; a working target says so.
  function wfClockHtml(d) {
    const v = d.clock_view;
    if (v) {
      if (v.state === "met" || v.state === "met_late") return `<span class="badge ${v.state === "met" ? "s-ck-clear" : "s-ck-review"}">${v.state === "met" ? "Met" : "Met late"}${v.met_on ? " " + esc(fmtDate(v.met_on)) : ""}</span>`;
      if (!v.due) return "";
      const days = v.remaining >= 0 ? `${v.remaining} ${v.basis === "business" ? "business" : "calendar"} day${v.remaining === 1 ? "" : "s"} left` : `${Math.abs(v.remaining)} day${v.remaining === -1 ? "" : "s"} over`;
      return `<span class="sla ${esc(v.state)}" title="${esc(v.cite || "")}">${v.target ? "Target" : "Due"} ${esc(fmtDate(v.due))} · ${days}</span> ${d.state === "done" ? "" : countdown(v.due)}`;
    }
    if (d.due) return `<span class="sla ok">Due ${esc(fmtDate(d.due))}</span> ${d.state === "done" ? "" : countdown(d.due)}`;
    return d.state === "upcoming" && d.clock ? '<span class="muted">its clock starts when the step before it is done</span>' : "";
  }
  const WF_STAGE_LETTERS = ["acceptance", "ineligible", "estimate_cost", "plan_notification", "final_order", "final_order_rationale_copy"];
  const WF_BUTTON = (a) => (a.startsWith("signal:") ? "Record it" : a.startsWith("letter:") ? "Draft it" : a.startsWith("qa:") ? "Open the QA gate" : a.startsWith("send:") ? "Send it"
    : a === "eligibility" ? "Run the review" : a === "summary" ? "Open the Dispute Summary" : a === "aor_confirm" ? "Give the ruling"
    : a === "invoice" ? "Create the invoice" : a === "settle" ? "Open Financials" : a === "outcome" ? "Record it"
    : a === "report:save" ? "Open the decision report" : a.startsWith("report:") ? "Generate it" : "Open");
  async function wfLoad(id, cfg) {
    const box = document.getElementById("wf-steps");
    if (!box) return;
    // The step list is for the people who work the case (and Finance, for its steps).
    if (!can("CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) { box.remove(); return; }
    let r;
    try { r = await Api.program.workflow(id); }
    catch (e) {
      // Reassigned to someone else while open: for a Reviewer the case is no longer theirs to see.
      if (e.status === 404 && Auth.reviewerScoped()) { caseGone(); return; }
      box.innerHTML = `<p class="muted">⚠ Steps could not be loaded: ${esc(e.message)}</p>`; return;
    }
    if (!r.programmed) { box.remove(); return; }
    const steps = r.steps || [];
    const next = steps.find((x) => x.state === "next");
    // A workflow that has ended (finished, or stopped by an admin) can't take a step: say so instead of a button.
    const ended = r.workflow_running === false && !!next;
    const inOrder = r.step_order !== false; // a tenant admin can let steps be taken out of order (Feature toggles)
    window._wf = { id, cfg, steps, next, isAdmin: !!r.is_admin, ended, inOrder };
    // The office's "Streamline Workflows, Timelines, and Outcomes": the case's workflow, its numbered steps in the
    // document's words, each with the countdown of the clock that times it, and the tasks that carry it out.
    const doc = r.doc || { workflow: "", steps: [] };
    const task = Object.fromEntries(steps.map((x) => [x.key, x]));
    const numbered = doc.steps.filter((d) => d.n);
    const nextDoc = doc.steps.find((d) => next && (d.keys || []).includes(next.key));
    const docDone = numbered.filter((d) => d.state === "done" || d.state === "skipped").length;
    CaseLayout.nextStep(steps, next, next && next.can_act && !ended ? WF_BUTTON(next.action) : "",
      ended ? "This case's workflow has ended (it finished, or an admin stopped it), so its steps can't be moved. Reopen the case, or ask an admin." : "",
      nextDoc ? { heading: `${doc.workflow} · step ${nextDoc.n || "—"} of ${numbered.length}`, title: nextDoc.title, detail: nextDoc.detail, clock: wfClockHtml(nextDoc) } : null);
    const wasOpen = box.querySelector("details.wf-all")?.open;
    const taskLi = (x) => !x ? "" : `<li class="wf-${x.state}">
        <span class="wf-mark" aria-hidden="true">${WF_MARK[x.state] || ""}</span>
        <span class="wf-label">${esc(x.label)}</span> <span class="chip">${esc(x.owner)}</span>
        ${x.state === "next" || (!inOrder && x.can_act) ? (ended ? `<span class="muted">workflow ended</span>` : x.can_act
          ? `<button type="button" class="mini" onclick="Views.wfAct('${esc(x.key)}')">${WF_BUTTON(x.action)}</button>`
          : `<span class="muted">waiting on the ${esc(x.owner)}</span>`) : ""}
        ${x.state === "next" && x.hint ? `<div class="muted wf-hint">${esc(x.hint)}</div>` : ""}</li>`;
    let phase = "";
    box.innerHTML = `<details class="wf-all"${wasOpen === false ? "" : " open"}><summary><b>${esc(doc.workflow || "Steps")}</b>
      <span class="muted">— ${docDone} of ${numbered.length} steps done. ${inOrder
        ? `Only the next task can be acted on, by ${r.step_owner === false ? "anyone on the case" : "its owner (or an admin)"}.`
        : `Tasks can be taken in any order, by ${r.step_owner === false ? "anyone on the case" : "their owner (or an admin)"}.`}</span></summary>
      <ol class="wfdoc">${doc.steps.map((d) => {
        const head = d.phase && d.phase !== phase ? `<li class="wfdoc-phase">${esc((phase = d.phase))}</li>` : "";
        return `${head}<li class="wfdoc-step wf-${d.state}">
          <div class="wfdoc-row"><span class="wf-mark" aria-hidden="true">${WF_MARK[d.state] || (d.state === "optional" ? "·" : "")}</span>
            <span class="wfdoc-n">${d.n ? d.n + "." : ""}</span>
            <span class="wfdoc-title"><b>${esc(d.title)}</b>${d.detail ? ` <span class="muted">— ${esc(d.detail)}</span>` : ""}</span>
            <span class="wfdoc-clock">${wfClockHtml(d)}</span></div>
          ${d.note ? `<div class="wfdoc-note">${esc(d.note)}</div>` : ""}
          ${(d.keys || []).length ? `<ul class="wf wfdoc-tasks">${d.keys.map((k) => taskLi(task[k])).join("")}</ul>` : ""}</li>`;
      }).join("")}</ol></details>`;
    wfApplyLocks();
  }
  // Keeping the steps current without a reload. After anything is saved on the
  // open case (api.js fires idre:changed) the step list and the next-step
  // banner are read again -- and again a few times, since a step can move on a
  // moment later, once the workflow has taken the change in. While a case is
  // open they are also re-read every 20 seconds, so a step someone else
  // finished shows up here too. Only the steps are redrawn: what is being typed
  // in the forms stays.
  let wfTimer = null, wfPoll = null;
  async function wfRefresh(tries = [0, 1500, 3500, 7000, 12000, 20000, 30000]) {
    const w = window._wf;
    if (!w || !document.getElementById("wf-steps") || !location.hash.includes(w.id)) return;
    const before = w.next ? w.next.key : "";
    clearTimeout(wfTimer);
    const step = async (i) => {
      if (i >= tries.length) return;
      wfTimer = setTimeout(async () => {
        const cur = window._wf;
        if (!cur || cur.id !== w.id || !location.hash.includes(w.id)) return;
        await wfLoad(w.id, w.cfg);
        const after = window._wf && window._wf.next ? window._wf.next.key : "";
        if (after !== before) { wfMoved(window._wf.next); return; }
        step(i + 1);
      }, tries[i]);
    };
    step(0);
  }
  // The case was reassigned away from this Reviewer while they had it open.
  function caseGone() {
    clearInterval(wfPoll); clearTimeout(wfTimer); window._wf = null;
    UI.toast("This case was reassigned to someone else and is no longer on your list.", { kind: "warn", duration: 9000 });
    location.hash = "#/cases";
  }
  function wfMoved(next) {
    if (!next) { UI.toast("All steps are done"); return; }
    UI.toast(next.can_act ? `Next: ${next.label} — it's yours` : `Next: ${next.label} — with the ${next.owner}`);
  }
  window.addEventListener("idre:changed", () => wfRefresh());
  function wfStartPolling() {
    clearInterval(wfPoll);
    wfPoll = setInterval(async () => {
      const w = window._wf;
      if (!w || !document.getElementById("wf-steps") || !location.hash.includes(w.id)) { clearInterval(wfPoll); return; }
      if (document.hidden) return;
      const before = w.next ? w.next.key : "";
      try {
        const r = await Api.program.workflow(w.id);
        const n = (r.steps || []).find((x) => x.state === "next");
        if ((n ? n.key : "") !== before) { await wfLoad(w.id, w.cfg); wfMoved(window._wf.next); }
      } catch (e) { if (e.status === 404 && Auth.reviewerScoped()) caseGone(); /* else the next round tries again */ }
    }, 20000);
  }
  function wfApplyLocks() {
    const { next, isAdmin, steps, inOrder } = window._wf || {};
    const mine = (action) => inOrder === false
      ? (steps || []).some((x) => x.can_act && x.action === action)
      : !!(next && next.can_act && next.action === action);
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
    CaseLayout.markTabs();
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
    catch (e) {
      UI.toast(e.message, { kind: "warn", duration: 9000 });
      // The case's workflow has ended: redraw the steps, which then offer nothing to press.
      if (e.workflowEnded && window._wf) wfLoad(window._wf.id, window._wf.cfg);
    }
  }
  const WF_YESNO = [["", "choose…"], ["yes", "Yes"], ["no", "No"]];
  async function wfAct(key) {
    const { id, cfg, steps } = window._wf || {};
    const st = (steps || []).find((x) => x.key === key);
    if (!st || !st.can_act) return; // can_act already says whether it is open (the next step, or any with order off)
    const a = st.action;
    const open = (sel) => { const el = document.querySelector(sel); if (el) { CaseLayout.reveal(el); el.open = true; el.scrollIntoView({ behavior: "smooth", block: "start" }); } };
    if (a.startsWith("letter:")) {
      const tpl = a.slice(7), sel = document.querySelector('#p-send select[name="template"]');
      if (sel) { sel.value = tpl; corrTemplateBody(sel); }
      return open("#p-send-details");
    }
    if (a.startsWith("qa:") || a.startsWith("send:")) { location.hash = "#/qa"; return; }
    if (a === "eligibility" || a === "aor_confirm") return open("#p-elig-details");
    if (a === "summary") return open("#p-summary-details");
    if (a === "invoice") return open("#p-inv-details");
    if (a === "report:save") return open("#p-dr-details");
    if (a.startsWith("report:")) {
      // Generating is quick to ask for and takes a few seconds to land: the
      // step moves on once the generated document reaches the QA gate.
      await Letters.gen(id, a.slice(7), true);
      wfRefresh([2500, 5000, 9000, 15000, 25000, 40000]);
      return;
    }
    if (a === "settle") { location.hash = "#/finance"; return; }
    if (a === "outcome") {
      const fs = (cfg && cfg.field_schema) || {};
      const opts = (xs) => [["", "choose…"]].concat((xs || []).map((x) => [x, x]));
      // What each outcome means, from the program's own definitions, right where one is chosen.
      const defs = (cfg && cfg.outcome_definitions) || {};
      // Grouped as the program's outcome structure has them (Closed Without Review, Decided With Determination...).
      const groups = (cfg && cfg.outcome_groups) || [];
      const grouped = groups.length ? groups.map((g) => {
        const rows = (g.outcomes || []).filter((o) => defs[o]).map((o) => `<dt>${esc(o)}</dt><dd>${esc(defs[o])}</dd>`).join("");
        return rows ? `<dt class="outcome-group" style="grid-column:1/-1">${esc(g.group)}</dt>${rows}` : "";
      }).join("") : "";
      const defList = grouped || (fs.case_outcome || []).filter((o) => defs[o]).map((o) => `<dt>${esc(o)}</dt><dd>${esc(defs[o])}</dd>`).join("");
      const v = await UI.modal({ title: "Record the outcome", submitLabel: "Record", wide: true,
        body: `These go on the reports to Florida. Each party's review cost comes from its invoice.
          ${defList ? `<details class="outcome-defs"><summary>What each outcome means</summary><dl>${defList}</dl></details>` : ""}`,
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
      // What the uploaded filing packet itself says, read by document analysis -- offered, still confirmed here.
      const det = (await Api.cases.get(id).catch(() => ({}))).details || {};
      const est = det.fee_estimate_requested === true ? "yes" : det.fee_estimate_requested === false ? "no" : undefined;
      const read = est !== undefined || det.claims_submitted != null ? " Answers below are as read from the uploaded filing packet — check them." : "";
      void est;
      const v = await UI.modal({ title: "Filing packet complete", body: "Payment and documents have both arrived." + read, submitLabel: "Confirm",
        fields: [{ name: "claims", label: "Number of claims in the filing packet", type: "number", required: true,
            value: det.claims_submitted ?? det.claims_detected ?? "",
            hint: "Reported to Florida as “Number of Claims Submitted with Initial Filing”." }] });
      if (v) wfSend(id, sig, { claims_submitted: parseInt(v.claims, 10) || 0 }, "Packet recorded as complete");
    } else {
      wfForm(id, sig, st.label);
    }
  }
  // What each step asks for when it is recorded: the day it actually happened (the program's clocks run from it),
  // who and how where that matters, and a note. The answers go to the workflow and onto the case as a note.
  const wfToday = () => new Date().toLocaleDateString("en-CA", { timeZone: (window._wf && window._wf.cfg && window._wf.cfg.time_zone) || "America/New_York" });
  const wfDate = (label, hint) => ({ name: "date", label, type: "date", required: true, value: wfToday(), max: wfToday(), hint });
  const wfNote = (label = "Note", required = false, hint = "Kept on the case as a note.") => ({ name: "note", label, type: "textarea", required, hint });
  const WF_HOW = [["", "choose…"], ["Email", "Email"], ["Phone", "Phone"], ["Portal", "Portal"], ["Mail", "Mail"], ["Fax", "Fax"]];
  const WF_REVIEW_NOTE = "Kept on the review record.";
  const WF_FORMS = {
    ESTIMATE_CONFIRMED: { title: "Confirm if Cost Estimate requested", submit: "Confirm", toast: "Cost estimate answer recorded",
      body: "With a cost estimate: the Acceptance letter and the Cost Estimate go out by day 13, then the provider has 15 calendar days to give permission. Without one: no cost estimate email is sent, the estimate language comes out of the Acceptance letter and email, and the Acceptance and the Health Plan Notification go out by day 13.",
      fields: [{ name: "r", label: "Did the filer request a cost estimate (the filing packet's “estimate requested” box)?", required: true, options: WF_YESNO }, wfNote()],
      prefill: async (id) => {
        const det = (await Api.cases.get(id).catch(() => ({}))).details || {};
        return { r: det.fee_estimate_requested === true ? "yes" : det.fee_estimate_requested === false ? "no" : "" };
      },
      data: (v) => ({ requested: v.r === "yes" }) },
    ESTIMATE_PERMISSION: { title: "Provider's permission to proceed", toast: "Permission recorded",
      fields: [{ name: "g", label: "Did the provider give permission to proceed with the full review?", required: true, options: WF_YESNO },
        wfDate("Answer received on", "The provider has 15 calendar days from the acceptance and cost estimate."),
        { name: "how", label: "Received by", options: WF_HOW }, wfNote()],
      data: (v) => ({ granted: v.g === "yes" }) },
    AOR_REVISED: { title: "Revised AOR received", toast: "Revised AOR recorded",
      body: "The filer sent a corrected Assignment of Rights; eligibility is checked again next.",
      fields: [wfDate("Revised AOR received on"), { name: "how", label: "Received by", options: WF_HOW }, wfNote("What was corrected")] },
    PLAN_RESPONSE_RECEIVED: { title: "Health plan's response received", toast: "Plan response recorded",
      body: "The health plan's response has arrived; the case goes Under Review.",
      fields: [wfDate("Response received on", "The plan has up to 15 calendar days from its notification."),
        { name: "how", label: "Received by", options: WF_HOW }, { name: "contact", label: "Sent by (name at the plan)" }, wfNote("What the plan sent")] },
    CODING_REVIEW_COMPLETE: { title: "Coding review complete", submit: "Complete", toast: "Coding review recorded",
      fields: [{ name: "c", label: "Is clinical review by the Nurse / Physician needed?", required: true, options: WF_YESNO, hint: "Yes puts it in the clinical queue." },
        wfNote("Your findings", true, WF_REVIEW_NOTE)],
      data: (v) => ({ clinical_requested: v.c === "yes" }) },
    CLINICAL_REVIEW_COMPLETE: { title: "Clinical review complete", submit: "Complete", toast: "Clinical review recorded",
      fields: [wfNote("Your findings", true, WF_REVIEW_NOTE)] },
    ATTORNEY_REVIEW_COMPLETE: { title: "Attorney review complete", submit: "Complete", toast: "Attorney review recorded",
      body: "This records the final rationale and the Final Order package. The outcome itself is recorded in its own step.",
      fields: [wfNote("Final rationale", true, WF_REVIEW_NOTE)] },
    FINAL_ORDER_ISSUED: { title: "AHCA's Final Order", toast: "Final Order recorded", body: "AHCA has issued its Final Order on this case. Upload the order the Agency sent; it is filed under Determination.",
      fields: [{ name: "file", label: "AHCA's Final Order (PDF)", type: "file", required: true, accept: ".pdf,application/pdf" },
        wfDate("Final Order issued on"), { name: "order_number", label: "Final Order number", placeholder: "As on the order, if it has one" }, wfNote()] },
    PROVIDER_WITHDRAW: { title: "Withdrawal requested", submit: "Record the request", danger: true, toast: "Withdrawal request recorded",
      body: "The provider or the health plan asked to withdraw the dispute. Next, verify the request with them and confirm it here; then the Withdrawal letter goes out and the case closes, with no invoice.",
      fields: [{ name: "by", label: "Requested by", required: true, options: [["", "choose…"], ["provider", "The provider"], ["health_plan", "The health plan"]] },
        wfDate("Request received on"), { name: "how", label: "Received by", required: true, options: WF_HOW },
        { name: "contact", label: "Name of the person who asked" }, wfNote("Reason given")] },
    WITHDRAWAL_CONFIRMED: { title: "Confirm the withdrawal", submit: "Confirm the withdrawal", toast: "Withdrawal confirmed",
      body: "You have verified the withdrawal request with the party who asked. The Withdrawal letter is next; the case closes when it has been sent.",
      fields: [{ name: "contact", label: "Verified with (name)", required: true }, { name: "how", label: "Verified by", required: true, options: WF_HOW },
        wfDate("Verified on"), wfNote()] },
    RFI_RESPONSE_RECEIVED: { title: "Additional documentation received", toast: "RFI response recorded",
      body: "The response to the request for additional documentation has arrived.",
      fields: [wfDate("Received on"), { name: "how", label: "Received by", options: WF_HOW }, wfNote("What was received", true)] },
    OUTREACH_DONE: { title: "Outreach to the filer", toast: "Outreach recorded",
      body: "Payment arrived but the packet did not, and you have contacted the filer. After 7 days without a complete packet you'll be asked whether to refund — nothing is refunded on its own.",
      fields: [{ name: "how", label: "Contacted by", required: true, options: WF_HOW }, { name: "contact", label: "Who you spoke or wrote to" },
        wfDate("Contacted on", "The 7 days run from this day."), wfNote("What was said", true)] },
    REFUND_APPROVED: { title: "Close and refund?", submit: "Close and refund", danger: true, toast: "Closing for a refund",
      body: "Close this case and refund the initial fee? The filer is sent the refund letter (through QA) and Finance is asked to refund the fee. Only do this when the filing packet isn't coming.",
      fields: [wfNote("Reason for the refund", true, "Kept on the case and shown to Finance.")] },
  };
  // The answers as a readable note: "Title / Label: answer" lines, option values shown as their labels.
  const wfRecord = (title, fields, v) => [title].concat(fields.filter((f) => f.type !== "file" && String(v[f.name] || "").trim()).map((f) => {
    const o = f.options && f.options.find((x) => x[0] === v[f.name]);
    return `${f.label}: ${o ? o[1] : String(v[f.name]).trim()}`;
  })).join("\n");
  async function wfForm(id, signal, label) {
    const f = WF_FORMS[signal] || { title: label || "Record this step", body: "Record this step as done?", fields: [wfNote()] };
    // A dialog that starts from what the case already says (the packet's answer, read from the filing).
    const pre = f.prefill ? await f.prefill(id).catch(() => ({})) : {};
    const fields = f.fields.map((x) => (pre[x.name] != null && pre[x.name] !== "" ? { ...x, value: pre[x.name] } : x));
    const v = await UI.modal({ title: f.title, body: f.body || "", submitLabel: f.submit || "Record", danger: !!f.danger, fields });
    if (!v) return;
    const data = f.data ? f.data(v) : {};
    for (const k of ["date", "how", "contact", "by", "order_number", "note"]) if (String(v[k] || "").trim()) data[k] = String(v[k]).trim();
    data.record = wfRecord(f.title, f.fields, v);
    // A document the step brings (AHCA's Final Order) is uploaded first and named on the step.
    const file = f.fields.find((x) => x.type === "file");
    if (file && v[file.name] instanceof File) {
      try {
        const up = await Api.cases.upload(id, v[file.name], false, "DETERMINATION");
        data.doc_id = up.doc_id;
        data.record += `\n${file.label}: ${v[file.name].name}`;
      } catch (e) { UI.toast(`The file could not be uploaded: ${e.message}`, { kind: "warn", duration: 9000 }); return; }
    }
    wfSend(id, signal, data, f.toast || "Recorded");
  }
  const wfAnytime = (id, signal) => wfForm(id, signal);

  // ---- Statutory clock helpers -------------------------------------------------
  const clockChip = (c) => {
    const txt = c.remaining >= 0 ? `${c.remaining} ${c.basis === "business" ? "bd" : "cd"} left` : `Breached ${Math.abs(c.remaining)}d ago`;
    return `<span class="sla ${c.state}" title="${esc(c.label)} · ${esc(c.cite)} · basis: ${esc(c.basis_note)}">◷ ${txt}</span>`;
  };
  // A live countdown to a deadline: an RFC 3339 instant, or a date (then the
  // end of that day, Eastern -- the program's day). Every .cdown on the page
  // ticks from one timer, so a list of them costs nothing extra.
  const etEndOfDay = (ymd) => {
    const g = new Date(ymd + "T23:59:59Z");
    const et = new Date(g.toLocaleString("en-US", { timeZone: "America/New_York" })), utc = new Date(g.toLocaleString("en-US", { timeZone: "UTC" }));
    return new Date(g.getTime() + (utc - et));
  };
  const cdState = (at) => {
    const ms = new Date(at) - Date.now(), late = ms < 0, s = Math.floor(Math.abs(ms) / 1000);
    const d = Math.floor(s / 86400), h = Math.floor(s / 3600) % 24, m = Math.floor(s / 60) % 60, sec = s % 60, p = (n) => String(n).padStart(2, "0");
    const span = d ? `${d}d ${p(h)}h ${p(m)}m ${p(sec)}s` : `${p(h)}:${p(m)}:${p(sec)}`;
    return { text: late ? `late by ${span}` : `${span} left`, cls: late ? "late" : ms <= 3 * 864e5 ? "soon" : "ok" };
  };
  const countdown = (due) => {
    if (!due) return "";
    const at = /^\d{4}-\d{2}-\d{2}$/.test(due) ? etEndOfDay(due).toISOString() : due;
    if (isNaN(new Date(at))) return "";
    const st = cdState(at);
    return `<span class="cdown ${st.cls}" data-at="${esc(at)}" title="Due ${esc(new Date(at).toLocaleString())}">${st.text}</span>`;
  };
  setInterval(() => document.querySelectorAll(".cdown[data-at]").forEach((el) => {
    const st = cdState(el.dataset.at);
    if (el.textContent !== st.text) el.textContent = st.text;
    el.className = "cdown " + st.cls;
  }), 1000);
  // The nearest running deadline: a met step's clock and a working target don't count.
  const nearestClock = (clocks) => {
    const run = (clocks || []).filter((c) => !["met", "met_late"].includes(c.state) && !c.target);
    return run.length ? run.reduce((a, b) => (a.remaining < b.remaining ? a : b)) : null;
  };

  async function clockMap() {
    try {
      const all = await Api.cm.allClocks();
      return Object.fromEntries(all.map((x) => [x.case_id, x.clocks]));
    } catch { return {}; }
  }

  // ---- Home for a TPA or a health plan: a summary of its own data in cards --
  // Every card opens the tab of the organization page it summarizes.
  function orgGo(kind, tab) {
    try { localStorage.setItem(kind === "tpa" ? "idre.tpaTab" : "idre.hpTab", tab); } catch { /* private window */ }
    location.hash = kind === "tpa" ? "#/tpa" : "#/healthplan";
  }
  const homeCard = (title, big, lines, go, extra = "", tone = "") => `<div class="home-card ${tone}" ${go ? `role="link" tabindex="0" onclick="${go}" onkeydown="if(event.key==='Enter'){${go}}"` : ""}>
      <div class="hc-title">${title}</div><div class="hc-big">${big}</div>
      ${lines.filter(Boolean).map((l) => `<div class="hc-line">${l}</div>`).join("")}${extra}</div>`;
  const plural = (n, w) => `${n} ${w}${n === 1 ? "" : "s"}`;
  const CLOSED_ST = ["CLOSED", "WITHDRAWN", "INELIGIBLE", "CLOSED_PAID"];
  async function orgHome(kind) {
    const tpa = kind === "tpa";
    const safe = (p, d) => p.catch(() => d);
    const [me, fees, onb, dash, reqs, notifs, cal, batches, rsp] = await Promise.all([
      safe(tpa ? Api.program.tpaMe() : Api.program.hpMe(), {}),
      safe(tpa ? Api.program.tpaFees() : Api.program.hpFees(), { unpaid: 0, amount_cents: 0 }),
      safe(tpa ? Api.program.tpaOnboarding() : Api.program.hpOnboarding(), {}),
      tpa ? safe(Api.program.tpaDashboard(), { cases: [] }) : Promise.resolve({ cases: [] }),
      safe(Api.program.orgDocRequests(kind), { requests: [] }),
      safe(Api.cm.notifications(), []),
      tpa ? safe(Api.cm.calendar(), []) : Promise.resolve([]),
      tpa ? safe(Api.program.tpaBulkDisputeBatches(), { batches: [] }) : Promise.resolve({ batches: [] }),
      tpa ? Promise.resolve({ cases: [] }) : safe(Api.program.respondList("hp"), { cases: [] }),
    ]);
    const org = tpa ? (me.tpa || {}) : (me.health_plan || {});
    const g = (tab) => `Views.orgGo('${kind}','${tab}')`;
    const cards = [];
    // Money owed now
    cards.push(homeCard("Fees owed", fees.unpaid ? usdC(fees.amount_cents) : "Nothing owed",
      [fees.unpaid ? `${plural(fees.unpaid, "dispute")} waiting for the initial fee` : "Every filed dispute is paid"],
      fees.unpaid ? "" : g("disputes"),
      fees.unpaid ? `<button class="mini hc-btn" onclick="event.stopPropagation(); ${tpa ? "Views.tpaFeesPay(this)" : "Views.hpFeesPay(this)"}">Pay ${usdC(fees.amount_cents)}</button>` : "",
      fees.unpaid ? "tone-pay" : ""));
    if (tpa) {
      const cs = dash.cases || [];
      // A programmed (FL) case moves on its Agency status; the basic status stays at intake.
      const isClosed = (c) => CLOSED_ST.includes(c.status) || c.agency_status === "Closed";
      const open = cs.filter((c) => !isClosed(c) && c.status !== "AWAITING_PAYMENT").length;
      const waitPay = cs.filter((c) => c.status === "AWAITING_PAYMENT").length;
      const closed = cs.filter(isClosed).length;
      cards.unshift(homeCard("Disputes", String(cs.length), [`${open} open`, waitPay ? `${waitPay} awaiting payment` : "", closed ? `${closed} closed` : ""], g("disputes")));
      const clients = onb.clients || [];
      const ready = clients.filter((c) => ["READY", "DOCS_PENDING"].includes(c.onboarding_status)).length;
      const needProfile = clients.filter((c) => c.onboarding_status === "PROFILE_INCOMPLETE").length;
      const needPay = clients.filter((c) => c.onboarding_status === "PAYMENT_PENDING").length;
      cards.push(homeCard("Clients ready to file", `${ready} of ${clients.length}`,
        [needProfile ? `${needProfile} need a profile` : "", needPay ? `${needPay} need a payment method` : "", !clients.length ? "Add your first client" : ""], g("clients"),
        "", clients.length && ready < clients.length ? "tone-warn" : ""));
      const plan = cal.filter((i) => i.kind === "PLAN_RESPONSE").sort((a, b) => a.due.localeCompare(b.due));
      cards.push(homeCard("Waiting on health plans", String(plan.length),
        [plan.length ? `soonest due ${fmtDate(plan[0].due)} — ${plan[0].case_number}` : "No plan is due to respond", plan.length ? countdown(plan[0].due) : ""], "location.hash='#/calendar'"));
      const today = new Date().toISOString().slice(0, 10), week = new Date(Date.now() + 7 * 864e5).toISOString().slice(0, 10);
      const soon = cal.filter((i) => i.due <= week).sort((a, b) => a.due.localeCompare(b.due));
      const overdue = soon.filter((i) => i.due < today).length;
      cards.push(homeCard("Due in the next 7 days", String(soon.length - overdue),
        [overdue ? `<span class="error">${overdue} overdue</span>` : "",
         ...soon.slice(0, 3).map((i) => `${fmtDate(i.due)} · ${esc(i.label)}`), !soon.length ? "Nothing due this week" : ""],
        "location.hash='#/calendar'", "", overdue ? "tone-bad" : ""));
      const bs = batches.batches || [];
      cards.push(homeCard("Bulk filings", String(bs.length), [bs[0] ? `latest: ${esc(bs[0].batch_ref || "")} · ${fmtDate(bs[0].created_at)}` : "File many disputes from one ZIP"], g("bulk")));
      const paid = cs.reduce((n, c) => n + Number(c.paid_cents || 0), 0);
      cards.push(homeCard("Paid to date", usdC(paid), [`across ${plural(cs.filter((c) => Number(c.paid_cents) > 0).length, "dispute")}`], g("disputes")));
    } else {
      const rows = rsp.cases || [];
      const wait = rows.filter((x) => x.response_status === "AWAITING").sort((a, b) => String(a.response_due_at || a.response_due).localeCompare(String(b.response_due_at || b.response_due)));
      const late = wait.filter((x) => x.response_due_at && new Date(x.response_due_at) < new Date()).length;
      cards.unshift(homeCard("Waiting for your response", String(wait.length),
        [wait.length ? `soonest due ${fmtDate(wait[0].response_due)} — ${esc(wait[0].case_number)}` : "No dispute is waiting for your response",
         wait.length ? countdown(wait[0].response_due_at || wait[0].response_due) : "", late ? `<span class="error">${late} past the deadline</span>` : ""],
        g("against"), "", late ? "tone-bad" : wait.length ? "tone-warn" : ""));
      cards.splice(1, 0, homeCard("Responded", String(rows.filter((x) => x.response_status === "SUBMITTED").length),
        [`${rows.filter((x) => x.response_status === "OPT_OUT_REQUESTED").length} opt-out requests`, `${rows.length} disputes against your plan in all`], g("against")));
      const ps = onb.parties || [];
      const ready = ps.filter((c) => ["READY", "DOCS_PENDING"].includes(c.onboarding_status)).length;
      cards.push(homeCard("Parties ready to file", `${ready} of ${ps.length}`, [!ps.length ? "Add the groups, networks and facilities you file for" : `${ps.length - ready} still onboarding`], g("parties")));
    }
    const perm = (reqs.requests || []).filter((x) => x.status === "OPEN" && x.reason === "PERMISSION").sort((a, b) => String(a.due_date).localeCompare(String(b.due_date)));
    if (perm.length) {
      cards.push(homeCard("Permission to proceed", String(perm.length),
        [`Your permission is needed to start the full review — soonest due ${esc(fmtDate(perm[0].due_date))} (${esc(perm[0].case_number)})`,
          countdown(perm[0].due_date), "Without it within 15 calendar days the dispute is dismissed."],
        "location.hash='#/document-requests'", "", perm.some((x) => x.overdue) ? "tone-bad" : "tone-warn"));
    }
    const ext = (reqs.requests || []).filter((x) => x.status === "OPEN" && x.reason === "EXTENSION").sort((a, b) => String(a.due_date).localeCompare(String(b.due_date)));
    if (ext.length) {
      cards.push(homeCard("More time requested", String(ext.length),
        [`The review team asks for more time — soonest answer due ${esc(fmtDate(ext[0].due_date))} (${esc(ext[0].case_number)})`,
          countdown(ext[0].due_date), "Agree or decline under Document requests."],
        "location.hash='#/document-requests'", "", ext.some((x) => x.overdue) ? "tone-bad" : "tone-warn"));
    }
    const rq = (reqs.requests || []).filter((x) => x.status === "OPEN" && x.reason !== "PERMISSION" && x.reason !== "EXTENSION");
    const od = rq.filter((x) => x.overdue).length;
    cards.push(homeCard("Document requests", String(rq.length),
      [rq.length ? "waiting for your documents" : "Nothing requested", od ? `<span class="error">${od} overdue</span>` : ""], "location.hash='#/document-requests'", "", od ? "tone-bad" : rq.length ? "tone-warn" : ""));
    const recent = (notifs || []).slice(0, 4);
    const activity = `<div class="home-activity"><h2>Recent activity</h2>${recent.length ? `<ul>${recent.map((n) =>
      `<li class="${n.read_at ? "" : "unread"}"><b>${esc(String(n.type || "").replace(/_/g, " ").toLowerCase())}</b> — ${esc(n.body)} <span class="muted">${fmtDate(n.at)}</span></li>`).join("")}</ul>`
      : '<p class="muted">Nothing yet.</p>'}</div>`;
    return `<div class="view-head"><h1>${esc(org.name || (tpa ? "Your TPA" : "Your health plan"))}</h1>
        <span class="muted">${tpa ? "a summary of your clients' disputes" : "a summary of your plan's disputes"} · ${badge(org.status || "ACTIVE")}</span></div>
      <div class="home-cards">${cards.join("")}</div>
      ${activity}
      <p><a class="button" href="${tpa ? "#/tpa" : "#/healthplan"}">${tpa ? "Open the TPA page" : "Open the health plan page"} →</a></p>`;
  }

  // ---- Dashboard -----------------------------------------------------------
  async function dashboard() {
    // TPA's home IS the TPA dashboard -- the generic rollup below is built
    // entirely on Api.cases.list(), the whole tenant's cases, which TPA is
    // deliberately blocked from (access-control follow-up); it has its own
    // correctly-scoped "Tracking" card on this same page instead.
    if (can("TPA")) return orgHome("tpa"); // a summary of its own data; the full page is under TPA
    if (outsideOnly() && can("HEALTH_PLAN")) return orgHome("hp");
    if (outsideOnly()) return partyCases(); // an invited party's home is its own disputes
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
      const attention = open.filter((c) => nearestClock(clocks[c.id]))
        .sort((a, b) => nearestClock(clocks[a.id]).remaining - nearestClock(clocks[b.id]).remaining);
      html += `<h2>Needs your attention (${attention.length})</h2>`;
      html += attention.length
        ? `<div class="panel">` + attention.slice(0, 8).map((c) => {
            const cl = nearestClock(clocks[c.id]);
            return `<div class="att-item" onclick="location.hash='#/cases/${c.id}'">
              <span class="att-id">${esc(c.case_number)}</span>
              <span class="att-title">${esc(c.service_line)} · ${amtLabel(prog)} $${((c[amtField(prog)] || 0) / 100).toLocaleString()}</span>
              ${caseBadge(c)} ${clockChip(cl)}</div>`;
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
  const SORTABLE = new Set(["assigned", "case_number", "status", "service_line", "qpa_cents", "disputed_amount_cents", "opened_at", "sla"]);
  // Lever 2/3/4: triage lane chip, signed SLA badge, batch group headers.
  // The triage lane: how much review a dispute needs (triage.go). The chip says what it means on hover.
  const laneChip = (lane) => lane === "AUTO_REVIEW" ? `<span class="chip lane-auto" tabindex="0" data-tip="Auto-review lane: everything needed for a one-click confirm is on file — simple, low-value, decided on the documents.">AUTO</span>`
    : lane === "COMPLEX" ? `<span class="chip lane-complex" tabindex="0" data-tip="Complex lane: batched, duplicated, escalated or high-dollar — it gets the most review.">COMPLEX</span>`
    : `<span class="chip lane-std" tabindex="0" data-tip="Standard lane: the normal review — not simple enough for auto-review, not complex.">STANDARD</span>`;
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
      // .case-tabs: the case page's own tab bar -- frozen with everything else, it kept staff from reaching the
      // "Letters & email" tab the banner says is still open (payment reminders).
      if (el.closest(".case-lock, .tabs, .case-tabs, .pager, .view-head")) return true;
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
  // Plain-language names for the program's provider types (the threshold
  // matrix). An unlisted type falls back to the code itself.
  const PROVIDER_TYPE_LABELS = {
    hospital_inpatient: "Hospital — inpatient",
    hospital_outpatient: "Hospital — outpatient",
    physician_dentist: "Physician or dentist",
    rural_hospital: "Rural hospital",
    other: "Other provider",
  };
  const providerTypeLabel = (x) => PROVIDER_TYPE_LABELS[x] || String(x || "").replace(/_/g, " ");
  // Fills a provider-type select with the program's types, keeping its first
  // ("not sure") option.
  async function fillProviderTypes(sel) {
    if (!sel || sel.dataset.filled) return;
    const prog = await Api.program.get().catch(() => null);
    const cfg = (prog && prog.config) || {};
    const types = [...new Set(((cfg.eligibility || {}).thresholds || []).map((x) => x.provider_type))];
    sel.insertAdjacentHTML("beforeend", types.map((x) => `<option value="${esc(x)}">${esc(providerTypeLabel(x))}</option>`).join(""));
    sel.dataset.filled = "1";
  }
  // The eligibility inputs, as the Reviewer knows them.
  const ELIG_INPUT_LABELS = {
    provider_type: ["Provider type", "what kind of provider this is"],
    contracted: ["In the plan's network", "whether the provider is in the health plan's network"],
    final_determination_at: ["Final determination date", "when the health plan made its final decision on the claim (the last EOB date works)"],
    service_date: ["Date of service", "the earliest date of service in dispute"],
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
  let caseListPoll = null;
  // The grid's Opened column: this year's dates without the year (the full date is the cell's tooltip).
  const openedShort = (d) => {
    const t = d ? new Date(d) : null;
    if (!t || isNaN(t)) return fmtDate(d);
    const sameYear = t.getFullYear() === new Date().getFullYear();
    return t.toLocaleString(undefined, { month: "short", day: "numeric", ...(sameYear ? {} : { year: "numeric" }), hour: "numeric", minute: "2-digit" });
  };
  const caseTable = (rows, clocks = {}, sort = "", prog = null, groupBatch = false) => {
    if (!rows.length) return `<p class="muted">No disputes.</p>`;
    const row = (c) => `<tr class="click" data-case="${c.id}">
        <td class="selcol"><input type="checkbox" class="sel-one" data-id="${c.id}" ${selection.has(c.id) ? "checked" : ""} aria-label="Select ${esc(c.case_number)}"></td>
        <td class="mono">${esc(c.case_number)}</td><td>${caseBadge(c)}</td><td>${assigneeCell(c)}</td><td>${esc(c.service_line)}</td>
        <td class="num">$${((c[amtField(prog)] || 0) / 100).toLocaleString()}</td>
        <td>${laneChip(c.triage_lane)}</td>
        <td>${caseClosed(prog, c) ? '<span class="muted">—</span>' : slaBadge(c.sla_days_remaining)}</td>
        <td>${nearestClock(clocks[c.id]) ? clockChip(nearestClock(clocks[c.id])) : '<span class="muted">—</span>'}</td>
        <td class="muted" title="${esc(fmtDate(c.opened_at))}">${openedShort(c.opened_at)}</td>
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
        body += `<tr class="batch-head"><td colspan="11">▦ Batch ${esc(bid.slice(0, 8))} — ${members.length} disputes ·
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
        ${[["case_number", "Case #"], ["status", "Status"], ["assigned", "Assigned"], ["service_line", "Service"], [amtField(prog), amtLabel(prog)]]
          .map(([c, l]) => sortableTh(c, l, sort)).join("")}
        <th>Lane</th>${sortableTh("sla", "SLA", sort)}
        <th title="The nearest statutory clock on the case">Clock</th>${sortableTh("opened_at", "Opened", sort)}<th></th></tr></thead><tbody>` +
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
    bindAssignButtons(document.querySelector(".dg-wrap") || document);
  }

  function paintBulkBar() {
    document.querySelector(".bulk-bar")?.remove();
    if (!selection.size) return;
    const bar = document.createElement("div");
    bar.className = "bulk-bar";
    bar.setAttribute("aria-live", "polite");
    bar.innerHTML = `<b>${selection.size} selected</b>
      <button class="mini" id="bk-assign">Assign to me</button>
      ${can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<button class="mini" id="bk-assign-to">Assign to…</button>` : ""}
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
    // PM: route the whole selection, any roles at once (e.g. when someone leaves or is overloaded): the Reviewer holds
    // each case, the specialists are added alongside. Rows left on "Keep" are not touched.
    const assignTo = $("#bk-assign-to");
    if (assignTo) assignTo.onclick = async (ev) => {
      const lists = await Promise.all(ASSIGN_ROLES.map(([r]) => Api.cm.assignees(r).catch(() => ({ assignees: [] }))));
      const fields = ASSIGN_ROLES.map(([r, label], i) => ({ name: "p_" + r, label: r === "CASE_MANAGER" ? `${label} — holds the case` : label, value: "",
        options: [["", "Keep as it is"], ...(lists[i].assignees || []).map((a) => [a.id, `${esc(a.username)} — ${a.open_cases} open`])] }));
      const v = await UI.modal({ title: `People on ${selection.size} dispute${selection.size === 1 ? "" : "s"}`, wide: true, fields, submitLabel: "Assign selection" });
      if (!v) return;
      const picks = ASSIGN_ROLES.filter(([r]) => v["p_" + r]);
      if (!picks.length) { UI.toast("No changes"); return; }
      await UI.run(ev.currentTarget, async () => {
        const said = [];
        for (const [r, label] of picks) {
          try {
            const res = await Api.cm.bulk({ action: "assign", case_ids: [...selection], role: r, assignee: v["p_" + r] });
            said.push(`${label}: ${res.results.filter((x) => x.ok).length} of ${res.results.length}`);
          } catch (e) { said.push(`${label}: ${e.message}`); }
        }
        UI.toast(said.join(" · "), { duration: 9000 });
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
        <p>${caseBadge(c)}</p>
        ${clocks.map((cl) => `<div class="sla-card"><span class="sla ${cl.state}">${clockChip(cl)}</span>
          <span class="muted" style="font-size:11px">${esc(cl.label)} · ${esc(cl.cite)}</span></div>`).join("")}
        <p style="margin-top:14px"><button class="mini" onclick="location.hash='#/cases/${c.id}'">Open workspace →</button></p>`;
    } catch (e) { p.querySelector(".peek-b").innerHTML = err(e); }
  }

  // ---- Disputes list -----------------------------------------------------------
  // The Disputes address with one filter changed (the others kept), for the filter switches.
  function casesHref(key, val) {
    const q = new URLSearchParams(location.hash.split("?")[1] || "");
    val ? q.set(key, val) : q.delete(key);
    const s = q.toString();
    return "#/cases" + (s ? "?" + s : "");
  }
  // The summary figures over every dispute this person can see (the search applies), from the stage counts.
  function disputesSummary(all, mine) {
    if (!all) return "";
    const st = all.stages || [];
    const sum = (k) => st.reduce((t, x) => t + (Number(x[k]) || 0), 0);
    // Unassigned and paid: the ones waiting for a Reviewer. An unpaid filing is not given out until its fee is paid.
    const un = sum("unassigned_paid"), unpaid = sum("awaiting_payment");
    return ListUI.summary([
      { label: "Disputes", value: Number(all.total) || 0, sub: "$" + Math.round(sum("disputed_cents") / 100).toLocaleString() + " disputed" },
      { label: "Waiting for a reviewer", value: un, sub: un ? "paid, nobody assigned yet" : "every paid dispute has a reviewer", tone: un ? "warn" : "good" },
      { label: "Awaiting payment", value: unpaid, sub: "given out once the fee is paid" },
      mine ? { label: "Assigned to me", value: Number(mine.total) || 0, sub: "on your list" } : null,
    ]);
  }
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
      const search = (hq.get("q") || "").trim();
      const assigned = ["mine", "unassigned", "assigned"].includes(hq.get("assigned")) ? hq.get("assigned") : "";
      const assignee = canAssign() ? (hq.get("assignee") || "") : "";
      const saved = await Api.cm.views().catch(() => []);
      const sv = saved.find((s) => s.id === v);
      const status = sv && sv.filters && sv.filters.status ? sv.filters.status : "";
      const asgParams = { ...(assigned ? { assigned } : {}), ...(assignee ? { assignee } : {}) };
      const reqParams = { limit: Pager.size(), ...(status ? { status } : {}), ...(sort ? { sort } : {}), ...(lane ? { lane } : {}), ...(search ? { q: search } : {}), ...asgParams };
      // The reviewers, for the Reviewer filter (their workload is on the Team page).
      const team = canAssign() ? Api.cm.assignees("CASE_MANAGER").then((r) => r.assignees || []).catch(() => []) : Promise.resolve([]);
      // Summary figures for the whole list (the search applies; the other filters are the switches below).
      const sumOpts = search ? { q: search } : {};
      const [page, clocks, prog, reviewers, allCounts, mineCounts] = await Promise.all([Api.cases.list(reqParams), clockMap(), Api.program.get().catch(() => null), team,
        Api.cases.pipelineCounts("internal_status", sumOpts).catch(() => null),
        Auth.reviewerScoped() ? Promise.resolve(null) : Api.cases.pipelineCounts("internal_status", { ...sumOpts, assigned: "mine" }).catch(() => null)]);
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
        // Statuses move while the list is open (someone finishes a step, a
        // payment lands): every 30 seconds the rows on screen get their
        // current status and SLA again. Only those cells change; the scroll
        // position, the selection and the page stay as they are.
        clearInterval(caseListPoll);
        caseListPoll = setInterval(async () => {
          if (!location.hash.startsWith("#/cases") || location.hash.startsWith("#/cases/")) { clearInterval(caseListPoll); return; }
          if (document.hidden) return;
          const ids = [...document.querySelectorAll("#view tr[data-case]")].map((tr) => tr.dataset.case);
          if (!ids.length) return;
          try {
            const fresh = await Api.cases.list({ ...reqParams, limit: Math.max(ids.length, reqParams.limit || 0) });
            for (const c of fresh.cases || []) {
              const tr = document.querySelector(`#view tr[data-case="${c.id}"]`);
              if (!tr) continue;
              const cells = tr.children;
              if (cells[2]) cells[2].innerHTML = caseBadge(c);
              if (cells[3] && (cells[3].dataset.asg || "") !== (c.assigned_to || "")) {
                cells[3].innerHTML = assigneeCell(c); cells[3].dataset.asg = c.assigned_to || ""; bindAssignButtons(cells[3]);
              }
              if (cells[7]) cells[7].innerHTML = caseClosed(prog, c) ? '<span class="muted">—</span>' : slaBadge(c.sla_days_remaining);
              const old = loaded.find((x) => x.id === c.id);
              if (old) Object.assign(old, c);
            }
          } catch { /* the next round tries again */ }
        }, 30000);
        // Search: case number, parties (provider, health plan, TPA client) by name, email or NPI, statuses and the
        // case's details. Kept in the hash like the lane, so a search can be shared and survives a reload.
        const box = $("#case-search");
        if (box) {
          let tmr;
          const go = () => {
            const q = new URLSearchParams(location.hash.split("?")[1] || "");
            const val = box.value.trim();
            val ? q.set("q", val) : q.delete("q");
            const s = q.toString();
            const next = "#/cases" + (s ? "?" + s : "");
            if (next !== location.hash) location.hash = next;
          };
          box.addEventListener("input", () => { clearTimeout(tmr); tmr = setTimeout(go, 350); });
          box.addEventListener("keydown", (e) => { if (e.key === "Enter") { clearTimeout(tmr); go(); } if (e.key === "Escape") { box.value = ""; go(); } });
          if (search) { box.focus(); box.setSelectionRange(box.value.length, box.value.length); }
        }
        $("#case-search-clear")?.addEventListener("click", () => { const b = $("#case-search"); if (b) { b.value = ""; b.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter" })); } });
        // Lever 2: lane tabs re-filter server-side; Lever 3: batch grouping
        // toggles batch-as-one rows. Both live in the hash = shareable.
        document.querySelectorAll(".lane-tab").forEach((b) => b.addEventListener("click", () => {
          const q = new URLSearchParams(location.hash.split("?")[1] || "");
          b.dataset.lane ? q.set("lane", b.dataset.lane) : q.delete("lane");
          const s = q.toString();
          location.hash = "#/cases" + (s ? "?" + s : "");
        }));
        // Assignment filters, in the hash like the lane.
        const setQ = (k, val) => {
          const q = new URLSearchParams(location.hash.split("?")[1] || "");
          val ? q.set(k, val) : q.delete(k);
          const s = q.toString();
          location.hash = "#/cases" + (s ? "?" + s : "");
        };
        $("#asg-filter")?.addEventListener("change", (e) => setQ("assigned", e.target.value));
        $("#asg-person")?.addEventListener("change", (e) => setQ("assignee", e.target.value));
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
        if ($("#grab")) $("#grab").onclick = async (ev) => {
          await UI.run(ev.currentTarget, async () => {
            const r = await Api.cm.grabNext();
            if (r.claimed) { UI.toast(`Claimed ${r.case_number} from the queue`); location.hash = `#/cases/${r.case_id}`; }
            else UI.toast(r.message, { kind: "warn" });
          }, "Grabbing…");
        };
        Pager.server($("#pg"), { total, shown: loaded.length }, async (limit, _offset, pg) => {
          if (pg === 0) cursors.length = 1; // also the page-size change path: old cursors are stale
          const more = { limit, ...(status ? { status } : {}), ...(sort ? { sort } : {}), ...(lane ? { lane } : {}), ...(search ? { q: search } : {}), ...asgParams };
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
      // The whole width of the page: the grid has eleven columns. Who holds how many cases is on the Team page.
      return `<div class="wide-view"><div class="view-head"><h1>Disputes</h1><span class="muted">${search ? `${total} matching “${esc(search)}”` : `${total} total`}</span>
        <div class="case-search"><input id="case-search" type="search" value="${esc(search)}" maxlength="100"
          placeholder="Search case number, provider, health plan, NPI, email…" aria-label="Search disputes" />
          ${search ? `<button class="mini" id="case-search-clear" aria-label="Clear search">Clear</button>` : ""}</div>

        <span style="flex:1"></span>
        ${Auth.reviewerScoped() ? "" : `<button class="mini" id="grab">⇪ Grab next</button>`}
        <button class="mini" id="density">Density: ${density}</button></div>
        ${disputesSummary(allCounts, mineCounts)}
        <div class="list-bar">
          ${Auth.reviewerScoped() ? "" : ListUI.segs("Whose disputes", [{ value: "", label: "All" }, { value: "unassigned", label: "Unassigned" }, { value: "mine", label: "Mine" },
            ...(canAssign() ? [{ value: "assigned", label: "Assigned" }] : [])].map((o) => ({ ...o, href: casesHref("assigned", o.value) })), assigned)}
          ${ListUI.segs("Triage lane", [["", "All lanes"], ["AUTO_REVIEW", "Auto-review"], ["STANDARD", "Standard"], ["COMPLEX", "Complex"]]
            .map(([value, label]) => ({ value, label, href: casesHref("lane", value) })), lane)}
          <button class="mini" id="batch-tgl" aria-pressed="${groupBatch}" title="Collapse batched disputes into one reviewable group">${groupBatch ? "▦ Batches: grouped" : "▦ Group batches"}</button>
        </div>
        <p class="viewbar">
          ${canAssign() ? `<label class="asg-filter">Reviewer
            <select id="asg-person"><option value="">Anyone</option>
              ${reviewers.map((a) => `<option value="${esc(a.id)}"${assignee === a.id ? " selected" : ""}>${esc(a.username)}</option>`).join("")}</select></label>` : ""}
          ${Auth.reviewerScoped() ? `<span class="muted">You see the cases assigned to you. New ones are given out in turn by the PM's rotation.</span>` : ""}
          <select onchange="location.hash='#/cases?view='+this.value" aria-label="Saved views">
            <option value="">All disputes</option>${opts}</select>
          <button class="mini" onclick="Views.saveCurrentView()">Save current view</button>
          ${sv ? `<span class="muted">filter: status = ${esc(sv.filters.status)}</span>` : ""}</p>` +
        `<div data-serverpaged>` + caseTable(loaded, clocks, sort || "opened_at", prog, groupBatch) + `</div><div id="pg"></div></div>`;
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

  const ASSIGN_ROLES = [["CASE_MANAGER", "Reviewer"], ["ARBITRATOR", "Arbitrator"], ["CODER", "Coder"],
    ["NURSE_PHYSICIAN", "Clinical reviewer"], ["ATTORNEY", "Attorney"]];
  const NEXT_IN_TURN = "__next";
  const assigneeOptions = (list) => [["", "Auto — whoever has the fewest open cases"],
    ...list.map((a) => [a.id, `${esc(a.username)} — ${a.open_cases} open case${a.open_cases === 1 ? "" : "s"}`])];

  // ---- Assignment: who holds a case, and who may change that --------------------------------------------------
  // The PM and the Tenant Admin assign and reassign; a Tenant Admin can also take a case themselves. A Reviewer
  // takes an unassigned case ("Take"); they cannot route cases to others. The server enforces all of it.
  const canAssign = () => can("PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
  const canTakeOwn = () => (Auth.claims()?.roles || []).some((r) => ["TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"].includes(r));
  const canClaim = () => canAssign(); // a Reviewer sees only their own cases (casescope.go): nothing to take
  const myId = () => Auth.claims()?.sub || "";
  // The list's Assigned cell: the person, or Unassigned; with the one action this viewer has on it.
  function assigneeCell(c) {
    const id = c.assigned_to || "";
    const who = id ? `<span class="asg-name${id === myId() ? " me" : ""}">${esc(id === myId() ? "You" : (c.assigned_name || "Staff"))}</span>`
      : `<span class="asg-none">Unassigned</span>`;
    let act = "";
    if (!id && canClaim() && !canAssign()) act = `<button class="mini asg-act" data-claim="${esc(c.id)}" data-num="${esc(c.case_number)}">Take</button>`;
    else if (canAssign()) act = `<button class="mini ghost asg-act" data-assign="${esc(c.id)}" data-cur="${esc(id)}">${id ? "Reassign" : "Assign"}</button>`;
    return `<span class="asg">${who}${act}</span>`;
  }
  function bindAssignButtons(root = document) {
    root.querySelectorAll("[data-claim]").forEach((b) => b.addEventListener("click", (e) => { e.stopPropagation(); claim(b.dataset.claim, b.dataset.num); }));
    root.querySelectorAll("[data-assign]").forEach((b) => b.addEventListener("click", (e) => { e.stopPropagation(); assign(b.dataset.assign); }));
  }
  // An answer that lost a race: tell them plainly who has it now, and redraw.
  function assignConflict(e) {
    UI.toast(e.message || "The assignment changed — look again.", { kind: "warn", duration: 9000 });
    App.rerender();
  }

  // "Assigned to" on the case page: the person (or Unassigned) and the actions this viewer has.
  async function loadCaseAssignee(caseId, isAssigner) {
    const el = document.getElementById("case-assignee");
    if (!el) return;
    try {
      const a = await Api.cm.assignment(caseId);
      const mine = a.assigned_to && a.assigned_to === myId();
      // One line per person on the case: their role, their name, and the few things this viewer can do about it.
      const link = (label, call, danger) => `<button type="button" class="team-link${danger ? " team-link-danger" : ""}" onclick="${call}">${label}</button>`;
      const acts = [];
      if (!a.assigned_to && canClaim()) acts.push(link("Take this case", `Views.claim('${esc(caseId)}')`));
      if (isAssigner) {
        acts.push(link(a.assigned_to ? "Reassign" : "Assign", `Views.assign('${esc(caseId)}')`));
        if (canTakeOwn() && !mine && a.assigned_to) acts.push(link("Assign to me", `Views.assignToMe('${esc(caseId)}')`));
        if (a.assigned_to) acts.push(link("Unassign", `Views.unassign('${esc(caseId)}')`, true));
      }
      const row = (role, name, isMe, rowActs) => `<div class="team-row">
          <span class="team-role">${esc(role)}</span>
          <span class="team-name${isMe ? " me" : ""}">${name}</span>
          ${rowActs.length ? `<span class="team-acts">${rowActs.join("")}</span>` : ""}</div>`;
      const helpers = (a.helpers || []).map((h) => row(h.role_label, esc(h.id === myId() ? "You" : (h.name || "Staff")), h.id === myId(),
        isAssigner ? [link("Change", `Views.assign('${esc(caseId)}')`), link("Remove", `Views.unassignHelper('${esc(caseId)}','${esc(h.role)}','${esc(h.id)}')`, true)] : [])).join("");
      const missing = ASSIGN_ROLES.filter(([r]) => r !== "CASE_MANAGER" && !(a.helpers || []).some((h) => h.role === r));
      el.innerHTML = row("Reviewer", a.assigned_to ? esc(mine ? "You" : a.name) : '<span class="asg-none">Nobody yet</span>', mine, acts)
        + helpers
        + (isAssigner && missing.length ? `<button type="button" class="team-add" onclick="Views.assign('${esc(caseId)}')">＋ Add a specialist</button>` : "");
      el.dataset.assignee = a.assigned_to || "";
    } catch { el.innerHTML = ""; }
  }

  // A Reviewer (or the PM / Tenant Admin) takes an unassigned case. Only one person can win it; the other is told.
  async function claim(caseId, caseNumber) {
    const ok = await UI.modal({ title: "Take this case?", submitLabel: "Take it",
      body: `${caseNumber ? `<b>${esc(caseNumber)}</b> ` : "It "}moves to your list as its Reviewer. Other reviewers will no longer see it.` });
    if (!ok) return;
    try {
      const r = await Api.cm.claim(caseId);
      UI.toast(r.unchanged ? "It's already yours" : `${caseNumber || "The case"} is yours now`);
      App.rerender();
    } catch (e) { e.status === 409 ? assignConflict(e) : UI.toast(e.message, { kind: "warn" }); }
  }

  // A Tenant Admin takes ownership of a case someone else holds.
  async function assignToMe(caseId) {
    const cur = await Api.cm.assignment(caseId).catch(() => ({}));
    const v = await UI.modal({ title: "Assign to me", submitLabel: "Take it over", danger: !!cur.name,
      body: cur.name ? `It is with <b>${esc(cur.name)}</b> now; they'll be told it moved to you.` : "You become its Reviewer.",
      fields: [{ name: "reason", label: "Reason", type: "textarea", placeholder: "Optional — kept on the case history" }] });
    if (!v) return;
    try {
      await Api.cm.assign(caseId, "CASE_MANAGER", myId(), v.reason || "", "", cur.assigned_to || "");
      UI.toast("Assigned to you");
      App.rerender();
    } catch (e) { e.status === 409 ? assignConflict(e) : UI.toast(e.message, { kind: "warn" }); }
  }

  // Assign the people on a case, every role at once: the Reviewer, who holds the case, and the specialists named
  // alongside (Coder, Clinical reviewer, Attorney, Arbitrator). Each row offers "Next in turn" (the role's rotation),
  // "Auto" (fewest open cases), a person from the list (least loaded first), or removing a specialist; a row left
  // on "Keep" is not touched. Whoever loses a place is told.
  const KEEP = "__keep", REMOVE = "__remove";
  async function assign(caseId) {
    try {
      const cur = await Api.cm.assignment(caseId).catch(() => ({}));
      const holderOf = (r) => (r === "CASE_MANAGER" ? cur.assigned_to || "" : ((cur.helpers || []).find((h) => h.role === r) || {}).id || "");
      const nameOf = (r) => (r === "CASE_MANAGER" ? cur.name : ((cur.helpers || []).find((h) => h.role === r) || {}).name);
      const lists = await Promise.all(ASSIGN_ROLES.map(([r]) => Api.cm.assignees(r).catch(() => ({ assignees: [] }))));
      const fields = ASSIGN_ROLES.map(([r, label], i) => {
        const res = lists[i], list = res.assignees || [], held = holderOf(r);
        const opts = [[KEEP, held ? `Keep ${esc(nameOf(r) || "current")}` : (r === "CASE_MANAGER" ? "— nobody yet —" : "— none —")]];
        if (res.next_in_turn && res.next_in_turn.id !== held) opts.push([NEXT_IN_TURN, `Next in turn — ${esc(res.next_in_turn.username)}`]);
        if (list.length) opts.push(["", "Auto — whoever has the fewest open cases"]);
        if (r === "CASE_MANAGER" && canTakeOwn() && myId() !== held && !list.some((a) => a.id === myId())) opts.push([myId(), "Me (take it myself)"]);
        list.filter((a) => a.id !== held).forEach((a) => opts.push([a.id, `${esc(a.username)} — ${a.open_cases} open case${a.open_cases === 1 ? "" : "s"}`]));
        if (held && r !== "CASE_MANAGER") opts.push([REMOVE, `Remove ${esc(nameOf(r) || "them")}`]);
        return { name: "p_" + r, label: r === "CASE_MANAGER" ? `${label} — holds the case` : label, options: opts, value: KEEP,
          hint: !list.length ? `No active ${label} on this tenant yet` : "" };
      });
      fields.push({ name: "reason", label: "Reason", type: "textarea", placeholder: "Optional — kept on the case history" });
      const v = await UI.modal({ title: "People on this case", wide: true, fields, submitLabel: "Save",
        body: "Change any row; rows on <b>Keep</b> stay as they are. The Reviewer holds the case; the others are added alongside." });
      if (!v) return;
      const done = [], failed = [];
      for (const [r, label] of ASSIGN_ROLES) {
        const pick = v["p_" + r];
        if (pick === undefined || pick === KEEP) continue;
        const expected = holderOf(r);
        try {
          const res = pick === NEXT_IN_TURN ? await Api.cm.assignNext(caseId, r, v.reason || "", expected)
            : pick === REMOVE ? await Api.cm.assign(caseId, r, "", v.reason || "", "unassign", expected)
            : await Api.cm.assign(caseId, r, pick, v.reason || "", "", expected);
          done.push(pick === REMOVE ? `${label} removed` : `${label}: ${res.name || "staff"}`);
        } catch (e) { failed.push(`${label}: ${e.message}`); }
      }
      if (done.length) UI.toast(done.join(" · "), { duration: 8000 });
      if (failed.length) UI.toast(failed.join(" · "), { kind: "warn", sticky: true });
      if (!done.length && !failed.length) UI.toast("No changes");
      App.rerender();
    } catch (e) { e.status === 409 ? assignConflict(e) : UI.toast(e.message, { kind: "warn" }); }
  }

  async function unassignHelper(caseId, role, who) {
    const label = (ASSIGN_ROLES.find(([k]) => k === role) || [role, role])[1];
    const v = await UI.modal({ title: `Remove the ${label}`, body: "The Reviewer keeps the case; the person removed is told.",
      fields: [{ name: "reason", label: "Reason", type: "textarea", placeholder: "Optional — kept on the case history" }],
      submitLabel: "Remove", danger: true });
    if (!v) return;
    try { await Api.cm.assign(caseId, role, "", v.reason || "", "unassign", who); UI.toast(`${label} removed`); App.rerender(); }
    catch (e) { e.status === 409 ? assignConflict(e) : UI.toast(e.message, { kind: "warn" }); }
  }

  async function unassign(caseId) {
    const v = await UI.modal({ title: "Unassign case", body: "The case goes back to the unassigned queue; the person who had it is told.",
      fields: [{ name: "reason", label: "Reason", type: "textarea", placeholder: "Optional — kept on the case history" }],
      submitLabel: "Unassign", danger: true });
    if (!v) return;
    const cur = document.getElementById("case-assignee")?.dataset.assignee;
    try { await Api.cm.assign(caseId, "CASE_MANAGER", "", v.reason || "", "unassign", cur); UI.toast("Unassigned"); App.rerender(); }
    catch (e) { e.status === 409 ? assignConflict(e) : UI.toast(e.message, { kind: "warn" }); }
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

  // Sealed offer — the amount is sealed in the vault (AES-256-GCM, AAD
  // tenant:case:offer:party) before it touches Postgres; the double-blind is
  // only lifted server-side once the workflow reaches OFFERS_REVEALED.
  async function offerForm(caseId, partyId) {
    let pid = partyId;
    if (!pid && can("PARTY")) {
      // A mapped party account knows its own side; no selector needed.
      try { pid = (await Api.party.caseDetail(caseId)).your_party; } catch { /* fall through */ }
    }
    const fields = [];
    if (!pid) fields.push({ name: "party_id", label: "Submitting for", type: "select", required: true,
      options: [["INITIATING", "Initiating party"], ["RESPONDING", "Responding party"]] });
    fields.push({ name: "amount", label: "Offer amount (USD)", type: "number", step: "0.01", required: true });
    fields.push({ name: "justification", label: "Justification (sealed with the offer)", type: "textarea", required: false,
      hint: "Optional. Sealed alongside the amount; revealed only when the offer window closes." });
    const v = await UI.modal({ title: "Submit sealed offer", submitLabel: "Seal and submit",
      body: "Your offer is encrypted in the vault the moment you submit. It is visible to no one — including arbitrators — until the offer window closes and the lawful-reveal gate passes. One offer per party; resubmission is refused.",
      fields });
    if (!v) return;
    try {
      await Api.offers.submit(caseId, {
        party_id: pid || v.party_id,
        amount_cents: Math.round(parseFloat(v.amount) * 100),
        justification: v.justification || "",
      });
      UI.toast("Offer sealed in the vault — receipt logged");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function determinationForm(caseId) {
    let offers;
    try { offers = await Api.offers.list(caseId); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    const rows = offers.offers || offers || [];
    if (!rows.length) { UI.toast("No offers on file", { kind: "warn" }); return; }
    const revealed = rows.filter((o) => o.revealed_amount_cents != null);
    if (revealed.length < rows.length) {
      const go = await UI.confirm("Offers not yet revealed",
        "The double-blind is still on. Reveal is lawful only once the workflow reaches OFFERS_REVEALED. Attempt reveal now?", "Reveal");
      if (!go) return;
      try { await Api.offers.reveal(caseId); UI.toast("Offers revealed — double-blind lifted"); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
      try { offers = await Api.offers.list(caseId); } catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    }
    const final = (offers.offers || offers || []).filter((o) => o.revealed_amount_cents != null);
    if (!final.length) { UI.toast("No revealed offers to select from", { kind: "warn" }); return; }
    const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
    const v = await UI.modal({ title: "Issue determination", submitLabel: "Issue determination", danger: true,
      body: "Baseball-style arbitration: you select exactly one of the revealed offers — no splitting, no middle figure (45 CFR 149.520(b)). Issuing starts the 30-calendar-day payment clock and cannot be undone.",
      fields: [
        { name: "selected", label: "Prevailing offer", type: "select", required: true,
          options: final.map((o) => [o.id, `${o.party_id} — ${usd(o.revealed_amount_cents)}`]) },
        { name: "rationale", label: "Determination rationale", type: "textarea", required: true,
          hint: "Required — becomes part of the certified determination letter." },
      ] });
    if (!v) return;
    try {
      const r = await Api.offers.determine(caseId, { selected_offer_id: v.selected, rationale: v.rationale });
      UI.toast(`Determination issued (${r.payment_due || "payment due in 30 calendar days"})`);
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // 45 CFR 149.510(c): same payer + same/similar codes can batch into one IDR.
  async function batchableModal(caseId) {
    let d;
    try { d = await Api.cases.batchable(caseId); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    const rows = d.batchable || [];
    const usd = (c) => (c == null ? "—" : "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 }));
    await UI.modal({ title: "Batchable disputes", wide: true, submitLabel: "Close",
      body: `<p>Same payer (<span class="mono">${esc(d.payer_id || "?")}</span>) with at least one extracted procedure code in common, still early enough to batch lawfully (45 CFR 149.510(c)).</p>` +
        (rows.length ? `<table class="tbl"><thead><tr><th>Case</th><th>Status</th><th>Service line</th><th>QPA</th><th>Opened</th></tr></thead><tbody>` +
          rows.map((r) => `<tr><td><a href="#/cases/${esc(r.id)}" class="mono">${esc(r.case_number)}</a></td><td>${badge(r.status)}</td><td>${esc(r.service_line || "—")}</td><td>${usd(r.qpa_cents)}</td><td>${fmtDate(r.opened_at)}</td></tr>`).join("") + `</tbody></table>`
          : `<p class="muted">${esc(d.note || "No batchable disputes found.")}</p>`),
      fields: [] });
  }

  // ---- Sealed-offer arbitration panel (case staff / arbitrator) --------------
  async function caseOffers(caseId) {
    let c, offers, bench;
    try {
      [c, offers, bench, winp] = await Promise.all([
        Api.cases.get(caseId), Api.offers.list(caseId),
        Api.cases.benchmarks(caseId).catch(() => null),
        Api.cases.winProbability(caseId).catch(() => null)]);
    } catch (e) { return `<div class="view-head"><h1>Offers</h1></div><p class="muted">${esc(e.message)}</p>`; }
    const usd = (c2) => "$" + ((Number(c2) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
    const rows = (offers.offers || offers || []);
    const blind = offers.double_blind !== false && rows.some((o) => o.revealed_amount_cents == null);
    let html = `<div class="view-head"><h1>Offers — <span class="mono">${esc(c.case_number)}</span></h1>${badge(c.status)}</div>
      <p class="muted">Double-blind sealed bidding (45 CFR 149.520(b)). ${blind ? "Amounts are vault-sealed until lawful reveal." : "Double-blind lifted."}</p>`;
    html += `<table class="tbl"><thead><tr><th>Party</th><th>Submitted</th><th>By</th><th>Amount</th><th>Reveal</th></tr></thead><tbody>` +
      (rows.length ? rows.map((o) => `<tr>
        <td>${badge(o.party_id)}</td><td>${fmtDate(o.submitted_at)}</td><td>${esc(o.submitted_by || "—")}</td>
        <td>${o.revealed_amount_cents != null ? `<strong>${usd(o.revealed_amount_cents)}</strong>` : '<span class="muted">🔒 sealed</span>'}</td>
        <td>${o.revealed_at ? `${fmtDate(o.revealed_at)} <span class="muted">(${esc(o.reveal_reason || "")})</span>` : "—"}</td></tr>`).join("")
        : `<tr><td colspan="5" class="muted">No offers submitted yet.</td></tr>`) + `</tbody></table>`;
    const acts = [];
    if (["OFFER_WINDOW_OPEN"].includes(c.status) && can("PARTY", "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      acts.push(["Submit sealed offer", () => offerForm(caseId)]);
    if (can("ARBITRATOR") && c.status === "OFFERS_REVEALED" && blind)
      acts.push(["Lift double-blind (reveal)", async () => {
        try { await Api.offers.reveal(caseId); UI.toast("Offers revealed"); App.rerender(); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      }]);
    if (can("ARBITRATOR") && ["OFFERS_REVEALED", "SELECTION_FINALIZED"].includes(c.status))
      acts.push(["Issue determination", () => determinationForm(caseId)]);
    const bl = (bench && bench.benchmarks) || [];
    if (winp && winp.sufficient)
      html += `<div class="card"><strong>Historical read:</strong> in ${winp.n} comparable determinations (${esc(winp.basis)}), the initiating party's offer prevailed ${winp.initiating_win_pct.toFixed(0)}% of the time.</div>`;
    else if (winp && winp.note)
      html += `<p class="muted">${esc(winp.note)}</p>`;
    if (bl.length)
      html += `<h2>QPA benchmarks — this tenant</h2>
        <p class="muted">Distribution of qualifying payment amounts on this tenant's cases carrying the same extracted procedure codes. The QPA is the primary statutory reasonableness factor (45 CFR 149.520(b)(2)(ii)).</p>
        <table class="tbl"><thead><tr><th>Code</th><th>Cases</th><th>P25</th><th>Median</th><th>P75</th></tr></thead><tbody>` +
        bl.map((b) => `<tr><td class="mono">${esc(b.code)}</td><td>${b.n}</td><td>${usd(b.p25_cents)}</td><td><strong>${usd(b.median_cents)}</strong></td><td>${usd(b.p75_cents)}</td></tr>`).join("") + `</tbody></table>`;
    if (acts.length) html += `<div class="actions">` + acts.map((a, i) => `<button class="btn" data-act="${i}">${esc(a[0])}</button>`).join("") + `</div>`;
    return { html, wire: () => {
      document.querySelectorAll("[data-act]").forEach((b) => b.addEventListener("click", () => acts[Number(b.dataset.act)][1]()));
    } };
  }

  // ---- Specialty review queues -------------------------------------------------
  const DISCIPLINES = {
    CODING: { title: "Coding review queue", who: "coder", blurb: "Bundling, unbundling and code-accuracy questions waiting for a coder. Claim one to take it; completing it moves the case on." },
    CLINICAL: { title: "Clinical review queue", who: "clinical reviewer", blurb: "Medical-necessity and level-of-care questions waiting for a doctor or nurse. Claim one to take it; completing it moves the case on." },
    LEGAL: { title: "Legal review queue", who: "attorney", blurb: "Opt-out, eligibility and process questions waiting for an attorney. Claim one to take it; completing it moves the case on." },
  };

  async function queueView(discipline) {
    const meta = DISCIPLINES[discipline] || { title: "Review queue", blurb: "" };
    const issue = new URLSearchParams((location.hash.split("?")[1] || "")).get("issue") || "";
    let rows;
    try { rows = await Api.reviews.queue(discipline, false, issue); }
    catch (e) { return `<div class="view-head"><h1>${esc(meta.title)}</h1></div><p class="muted">${esc(e.message)}</p>`; }
    // The server answers {queue: [...]}: status is the CASE's status,
    // review_status the review's (REQUESTED / IN_REVIEW / COMPLETE).
    rows = rows.queue || rows.reviews || (Array.isArray(rows) ? rows : []);
    const usd = (c) => (c == null ? "—" : "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 }));
    const dxOf = (r) => { try { return typeof r.diagnosis_codes === "string" ? JSON.parse(r.diagnosis_codes) : (r.diagnosis_codes || []); } catch { return []; } };
    // Bundling is a coding question: the switch is offered on the coding
    // queue only, with both counts (the server filters by ?issue=bundling).
    const bundlingOn = discipline === "CODING" && (window.App?.feature ? App.feature("bundling_filter") : true);
    let counts = null;
    if (bundlingOn) {
      const other = await Api.reviews.queue(discipline, false, issue ? "" : "bundling").catch(() => null);
      const n = (x) => (x ? (x.queue || x.reviews || []).length : null);
      counts = issue ? { all: n(other), bundling: rows.length } : { all: rows.length, bundling: n(other) };
    }
    const seg = (val, label, n) => `<a href="#/queues/${discipline}${val ? `?issue=${val}` : ""}" aria-current="${issue === val}">${label}${n == null ? "" : `<span class="n">${n}</span>`}</a>`;
    const waiting = rows.filter((r) => r.review_status === "REQUESTED").length;
    const inReview = rows.filter((r) => r.review_status === "IN_REVIEW").length;
    const mineN = rows.filter((r) => r.mine).length;
    const disputedSum = rows.reduce((t, r) => t + (Number(r.disputed_amount_cents) || 0), 0);
    const html = `<div class="view-head"><h1>${esc(meta.title)}</h1></div>
      <p class="muted q-blurb">${esc(meta.blurb)}</p>
      ${ListUI.summary([
        { label: "Waiting", value: waiting, sub: "not yet claimed", tone: waiting ? "warn" : "good" },
        { label: "In review", value: inReview, sub: "claimed by a reviewer" },
        { label: "On your list", value: mineN, sub: "cases you're the specialist on" },
        { label: "Disputed", value: usd(disputedSum), sub: `across ${rows.length} open review${rows.length === 1 ? "" : "s"}` },
      ])}
      <div class="q-toolbar">
        <div>${bundlingOn ? `<nav class="q-seg" aria-label="Filter">${seg("", "All open", counts.all)}${seg("bundling", "Bundling", counts.bundling)}</nav>` : ""}</div>
        <div class="q-right"><span class="q-summary">${waiting} waiting · ${inReview} in review</span>
          <button type="button" class="mini" data-all>Show completed</button></div>
      </div>
      ${issue === "bundling" ? `<p class="muted q-note">Showing cases flagged by document analysis (NCCI edits, modifier 59, bundled-denial wording) or whose disputed issue mentions bundling.</p>` : ""}
      <div class="q-table"><table><thead><tr><th>Case</th><th>Case status</th><th>Service line · codes</th><th class="num">Disputed</th><th>Review</th><th>Requested</th><th></th></tr></thead><tbody>` +
      (rows.length ? rows.map((r) => `<tr>
        <td><a href="#/cases/${esc(r.case_id)}" class="mono">${esc(r.case_number || r.case_id)}</a>${r.mine ? ' <span class="badge s-paid" title="You are the specialist on this case">yours</span>' : ""}</td>
        <td>${badge(r.status || "")}</td><td>${esc(r.service_line || "—")}${r.bundling_suspected === "true" ? ' <span class="badge s-flag" title="Bundling wording found in the case documents">bundling</span>' : ""}${dxOf(r).length ? `<div class="q-codes" title="Diagnosis codes from the documents">${dxOf(r).slice(0, 6).map(esc).join(" · ")}${dxOf(r).length > 6 ? ` +${dxOf(r).length - 6}` : ""}</div>` : ""}</td><td class="num">${usd(r.disputed_amount_cents)}</td>
        <td>${badge(r.review_status)}${r.reviewer ? ` <span class="muted">${esc(r.reviewer)}</span>` : ""}${r.note ? `<div class="muted">${esc(r.note)}</div>` : ""}</td>
        <td>${fmtDate(r.requested_at)}</td>
        <td class="q-act">${r.review_status === "REQUESTED" ? `<button type="button" class="q-btn" data-claim="${r.id}">Claim</button>`
          : r.review_status === "IN_REVIEW" ? `<button type="button" class="q-btn" data-complete="${r.id}">Complete</button>` : ""}</td></tr>`).join("")
        : `<tr><td colspan="7" class="q-empty">${issue === "bundling" ? "No bundling questions waiting." : `Nothing waiting for a ${esc(meta.who || "reviewer")} — the queue is clear.`}</td></tr>`) +
      `</tbody></table></div>`;
    return { html, wire: () => {
      document.querySelectorAll("[data-claim]").forEach((b) => b.addEventListener("click", async () => {
        try { await Api.reviews.claim(b.dataset.claim); UI.toast("Review claimed"); App.rerender(); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      }));
      document.querySelectorAll("[data-complete]").forEach((b) => b.addEventListener("click", async () => {
        // The same answers the case's Steps panel asks for this step; the
        // server records the step there too and moves the case on.
        const fields = [];
        if (discipline === "CODING") fields.push({ name: "c", label: "Does the case need clinical review by a nurse or physician?", required: true, options: WF_YESNO,
          hint: "Yes puts it in the clinical queue." });
        fields.push({ name: "note", label: "Your findings", type: "textarea", required: true, hint: "Kept on the review record." });
        const v = await UI.modal({ title: "Complete the review", submitLabel: "Complete review",
          body: discipline === "LEGAL" ? "This records the attorney review step on the case. The outcome itself is recorded afterwards in the case's Steps." : "This records the review step on the case and moves it on.",
          fields });
        if (!v) return;
        const extra = discipline === "CODING" ? { clinical_requested: v.c === "yes" } : {};
        try {
          await Api.reviews.complete(b.dataset.complete, v.note, extra);
          UI.toast(discipline === "CODING" && v.c === "yes" ? "Review complete — sent on to the clinical queue" : "Review complete");
          App.rerender();
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      }));
      const all = document.querySelector("[data-all]");
      if (all) all.addEventListener("click", async (ev) => {
        ev.preventDefault();
        await UI.run(all, async () => {
        try {
          const full = await Api.reviews.queue(discipline, true);
          const list = full.queue || full.reviews || (Array.isArray(full) ? full : []);
          const done = list.filter((r) => r.review_status === "COMPLETE");
          await UI.modal({ title: "Completed reviews", wide: true, submitLabel: "Close",
            body: done.length ? `<table><thead><tr><th>Case</th><th>Reviewer</th><th>Completed</th><th>Note</th></tr></thead><tbody>` +
              done.map((r) => `<tr><td class="mono">${esc(r.case_number || r.case_id)}</td><td>${esc(r.reviewer || "")}</td><td>${fmtDate(r.completed_at)}</td><td>${esc(r.note || "")}</td></tr>`).join("") + `</tbody></table>`
              : "<p class='muted'>No completed reviews yet.</p>",
            fields: [] });
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        });
      });
    } };
  }

  // ---- Program-manager escalation board ----------------------------------------
  async function pmQueueView() {
    let data;
    try { data = await Api.reviews.pmQueue(); }
    catch (e) { return `<div class="view-head"><h1>PM board</h1></div><p class="muted">${esc(e.message)}</p>`; }
    const escRows = data.escalations || data.queue || [];
    const open = data.open_cases || [];
    const lvl2 = escRows.filter((r) => (r.level || 1) > 1).length;
    const html = `<div class="view-head"><h1>Program manager board</h1><span class="muted">escalated deadlines and every open case</span></div>
      ${ListUI.summary([
        { label: "Open escalations", value: escRows.length, sub: escRows.length ? "a deadline at risk" : "nothing escalated", tone: escRows.length ? "bad" : "good" },
        { label: "Level 2 or higher", value: lvl2, sub: "escalated more than once", tone: lvl2 ? "bad" : "" },
        { label: "Open cases", value: open.length, sub: "on the board" },
      ])}
      <section class="panel"><h2>Escalations <span class="n">${escRows.length}</span></h2>
      <table class="tbl"><thead><tr><th>Case</th><th>Status</th><th>Reason</th><th>Raised</th></tr></thead><tbody>` +
      (escRows.length ? escRows.map((r) => `<tr><td><a href="#/cases/${esc(r.case_id)}" class="mono">${esc(r.case_number || r.case_id)}</a></td><td>${badge(r.status || "")}</td><td>${esc(r.clock || "")}${r.level > 1 ? ` · <span class="due-over">level ${r.level}</span>` : ""}${r.detail ? `<div class="muted">${esc(r.detail)}</div>` : ""}</td><td>${fmtDate(r.created_at)}</td></tr>`).join("")
        : `<tr><td colspan="4" class="muted">No open escalations.</td></tr>`) + `</tbody></table></section>
      <section class="panel"><h2>Open cases <span class="n">${open.length}</span></h2>
      <table class="tbl"><thead><tr><th>Case</th><th>Status</th><th>Opened</th></tr></thead><tbody>` +
      (open.length ? open.map((r) => `<tr><td><a href="#/cases/${esc(r.id)}" class="mono">${esc(r.case_number)}</a></td><td>${badge(r.status)}</td><td>${fmtDate(r.opened_at)}</td></tr>`).join("")
        : `<tr><td colspan="3" class="muted">No open cases.</td></tr>`) + `</tbody></table></section>`;
    return html;
  }

  // ---- Party workspace ----------------------------------------------------------
  async function partyCases() {
    let rows;
    try { rows = await Api.party.cases(); }
    catch (e) { return `<div class="view-head"><h1>My disputes</h1></div><p class="muted">${esc(e.message)}</p>`; }
    rows = rows.cases || rows || [];
    const usd = (c) => (c == null ? "—" : "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 }));
    return `<div class="view-head"><h1>My disputes</h1></div>
      <p class="muted">Every dispute your account is a mapped party on. Access is granted per-case by the IDRE entity — your login alone opens nothing else.</p>
      <table class="tbl"><thead><tr><th>Case</th><th>Side</th><th>Status</th><th>Service line</th><th>Amount</th><th>Opened</th></tr></thead><tbody>` +
      (rows.length ? rows.map((r) => `<tr>
        <td><a href="#/party/cases/${esc(r.case_id)}" class="mono">${esc(r.case_number)}</a></td>
        <td>${badge(r.party_id)}</td><td>${badge(r.status)}</td><td>${esc(r.service_line || "—")}</td>
        <td>${usd(r.disputed_amount_cents)}</td><td>${fmtDate(r.opened_at)}</td></tr>`).join("")
        : `<tr><td colspan="6" class="muted">You are not mapped to any disputes yet.</td></tr>`) + `</tbody></table>`;
  }

  async function partyCaseDetail(caseId) {
    let d;
    try { d = await Api.party.caseDetail(caseId); }
    catch (e) { return `<div class="view-head"><h1>Dispute</h1></div><p class="muted">${esc(e.message)}</p>`; }
    const c = d.case || d;
    const usd = (x) => (x == null ? "—" : "$" + ((Number(x) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 }));
    let html = `<div class="view-head"><h1><span class="mono">${esc(c.case_number)}</span></h1>${badge(c.status)} ${badge(d.your_party)}</div>
      <p class="muted">${esc(c.service_line || "")} · disputed ${usd(c.disputed_amount_cents)} · opened ${fmtDate(c.opened_at)}</p>`;
    const acts = [];
    if (App.feature("sealed_offers") && ["OFFER_WINDOW_OPEN"].includes(c.status)) acts.push(["Submit sealed offer", () => offerForm(c.id, d.your_party)]);
    if (acts.length) html += `<div class="actions">` + acts.map((a, i) => `<button class="btn" data-act="${i}">${esc(a[0])}</button>`).join("") + `</div>`;
    if (d.determination)
      html += `<h2>Determination</h2><div class="card"><p><strong>${usd(d.determination.amount_cents)}</strong> — issued ${fmtDate(d.determination.issued_at)}</p>
        <p>${esc(d.determination.rationale || "")}</p><p class="muted">Payment due within 30 calendar days of this determination (45 CFR 149.520(b)(4)).</p></div>`;
    if (d.invoices && d.invoices.length)
      html += `<h2>Invoices</h2><table class="tbl"><thead><tr><th>Invoice</th><th>Status</th><th>Amount</th><th>Due</th></tr></thead><tbody>` +
        d.invoices.map((i) => `<tr><td class="mono">${esc(i.invoice_number || i.id)}</td><td>${badge(i.status)}</td><td>${usd(i.amount_cents)}</td><td>${fmtDate(i.due_date)}</td></tr>`).join("") + `</tbody></table>`;
    if (d.your_party === "RESPONDING")
      html += `<div class="card tpa-pkg" id="party-rsp-panel"><p class="muted">Loading your response…</p></div>`;
    return { html, wire: () => {
      document.querySelectorAll("[data-act]").forEach((b) => b.addEventListener("click", () => acts[Number(b.dataset.act)][1]()));
      if (d.your_party === "RESPONDING") respondOpen("party", caseId);
    } };
  }

  async function reviewRequestForm(caseId) {
    const v = await UI.modal({ title: "Request specialty review", submitLabel: "Request review",
      body: "Queues the case for a discipline reviewer. Only one active review per discipline per case.",
      fields: [
        { name: "discipline", label: "Discipline", type: "select", required: true,
          options: [["CODING", "Coding (bundling / code accuracy)"], ["CLINICAL", "Clinical (medical necessity)"], ["LEGAL", "Legal (eligibility / process)"]] },
        { name: "note", label: "Request note", type: "textarea", required: true,
          hint: "What should the reviewer answer? Becomes part of the review record." },
      ] });
    if (!v) return;
    try { await Api.reviews.request(caseId, { discipline: v.discipline, note: v.note }); UI.toast("Review requested"); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function partyInviteForm(caseId) {
    const v = await UI.modal({ title: "Invite party", submitLabel: "Create account and map",
      body: "Creates (or reuses) a portal login for the party representative and maps it to this case. The temporary password is shown once in the response — copy it now; it is never emailed, logged, or shown again.",
      fields: [
        { name: "email", label: "Party email", type: "email", required: true },
        { name: "party_id", label: "Side", type: "select", required: true,
          options: [["INITIATING", "Initiating party"], ["RESPONDING", "Responding party"]] },
      ] });
    if (!v) return;
    try {
      const r = await Api.party.invite(caseId, { email: v.email, party_id: v.party_id });
      await UI.modal({ title: "Party invited", submitLabel: "Done",
        body: `<p>Mapped <strong>${esc(v.email)}</strong> as <strong>${esc(v.party_id)}</strong>.</p>
          <p>Temporary password (shown once): <code class="mono">${esc(r.temp_password || "(existing account — no new password)")}</code></p>`,
        fields: [] });
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Insights: downstream analytics (oversight + staff) ---------------------
  async function insightsView() {
    const usd = (c) => (c == null ? "—" : "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 0 }));
    let html = `<div class="view-head"><h1>Insights</h1></div>
      <p class="muted">Patterns computed from extracted document data and the determination record. Tenant-isolated; parties never see any of this.</p>`;
    if (App.feature("payer_analytics")) {
      const payers = (await Api.analytics.payers().catch(() => ({ payers: [] }))).payers || [];
      html += `<h2>Payer denial patterns</h2><table class="tbl"><thead><tr><th>Payer</th><th>Disputes</th><th>Resolved</th><th>Bundling</th><th>Necessity</th><th>Duplicates</th><th>Avg disputed</th><th>Avg denial→filing</th></tr></thead><tbody>` +
        (payers.length ? payers.map((r) => `<tr><td class="mono">${esc(r.payer_id)}</td><td>${r.disputes}</td><td>${r.resolved}</td><td>${r.bundling_flags}</td><td>${r.necessity_flags}</td><td>${r.duplicate_flags}</td><td>${usd(r.avg_disputed_cents)}</td><td>${r.avg_denial_to_filing_days ?? "—"}d</td></tr>`).join("")
          : `<tr><td colspan="8" class="muted">No payer data yet.</td></tr>`) + `</tbody></table>`;
    }
    if (App.feature("fraud_signals") && can("STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
      const prov = (await Api.analytics.fraudSignals().catch(() => ({ providers: [] }))).providers || [];
      html += `<h2>Provider tripwire clusters <span class="muted">(oversight surface — patterns for human review, not accusations)</span></h2>
        <table class="tbl"><thead><tr><th>Provider</th><th>Disputes</th><th>Necessity flags</th><th>Duplicate claims</th><th>Bundling</th><th>Avg QPA</th></tr></thead><tbody>` +
        (prov.length ? prov.map((r) => `<tr><td class="mono">${esc(r.provider_id)}</td><td>${r.disputes}</td><td>${r.necessity_flags}</td><td>${r.duplicate_claims}</td><td>${r.bundling_flags}</td><td>${usd(r.avg_qpa_cents)}</td></tr>`).join("")
          : `<tr><td colspan="6" class="muted">No tripwires triggered.</td></tr>`) + `</tbody></table>`;
    }
    if (App.feature("network_adequacy")) {
      const segs = (await Api.analytics.networkAdequacy().catch(() => ({ segments: [] }))).segments || [];
      html += `<h2>Network-adequacy evidence <span class="muted">(OON dispute concentration)</span></h2>
        <table class="tbl"><thead><tr><th>Service line</th><th>Payer</th><th>OON disputes</th><th>Avg disputed</th><th>Span</th></tr></thead><tbody>` +
        (segs.length ? segs.map((r) => `<tr><td>${esc(r.service_line)}</td><td class="mono">${esc(r.payer_id)}</td><td>${r.oon_disputes}</td><td>${usd(r.avg_disputed_cents)}</td><td class="muted">${fmtDate(r.first_seen)} → ${fmtDate(r.latest)}</td></tr>`).join("")
          : `<tr><td colspan="5" class="muted">No dispute segments yet.</td></tr>`) + `</tbody></table>`;
    }
    return html;
  }

  // ---- Platform-admin feature toggles -----------------------------------------
  const FEATURE_LABELS = {
    deid: "De-identification: scan outbound letters, scrub the lakehouse, read documents' clinical terms",
    deid_hold_mail: "Hold a letter the scan finds direct identifiers in (off: it is flagged and goes out)",
    sealed_offers: "Sealed offers (double-blind bidding)",
    review_queues: "Specialty review queues (coding/clinical/legal)",
    party_workspace: "Party workspace (Keycloak party accounts)",
    benchmarks: "QPA benchmarks",
    batch_suggestions: "Batch-consolidation suggestions",
    bundling_filter: "Bundling queue filter + auto-flag",
    necessity_screen: "dx↔CPT necessity screen",
    deadline_verification: "Deadline verification from document dates",
    duplicate_detection: "Duplicate-claim detection",
    win_probability: "Win-probability triage",
    payer_analytics: "Payer denial-pattern analytics",
    fraud_signals: "Fraud-signal surface (oversight)",
    network_adequacy: "Network-adequacy evidence",
    letter_citation: "Determination-letter auto-citation",
    tpa_onboarding: "TPA onboarding (profile + payment + 6-doc packages)",
    healthplan_onboarding: "Health plan onboarding (parties + payment + 6-doc packages)",
    onboarding_required: "TPA / health plan filings need a READY package and a verified payment method (off: onboarding is optional — any active client can be filed for)",
    entity360: "Entity 360 (cross-rail organization aggregate: TPA / provider / health plan)",
  };

  // Case workflow rules: how strictly a case follows its SOP. Each is on unless a tenant admin turns it off.
  const WORKFLOW_RULES = {
    step_order: ["Follow the steps in order", "Only a case's next step can be acted on. Off: staff can take any step that isn't done yet, in any order."],
    step_owner: ["Only a step's owner can do it", "Each step is done by the role that owns it (or an admin). Off: anyone who works the case can do any step."],
    payment_lock: ["Freeze a case until its fee is paid", "A case awaiting its initial fee allows only correspondence. Off: staff can work it before payment."],
    ineligible_lock: ["Freeze an ineligible case", "A case found ineligible allows only correspondence until a new eligibility review. Off: staff can keep working it."],
    fwa_review: ["Fraud, waste or abuse review", "Staff can report suspected fraud, waste or abuse on a case; the PM assigns an independent reviewer and records the outcome. Disputes of $1,000,000 or more are flagged automatically. Off: the section is hidden and nothing is flagged (large disputes are still escalated to the PM)."],
  };

  async function featuresAdmin() {
    if (!can("TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) return `<div class="view-head"><h1>Feature toggles</h1></div><p class="muted">Only a tenant admin can change these.</p>`;
    const p = await Api.program.get().catch(() => ({ features: {} }));
    const flags = p.features || {};
    const tenant = Api.getTenant();
    const DEFAULT_OFF = { onboarding_required: true, deid_hold_mail: true }; // features.go featureDefaultOff
    const row = (k, label, desc) => {
      const on = DEFAULT_OFF[k] ? flags[k] === true : flags[k] !== false;
      return `<tr><td><b>${esc(label)}</b>${desc ? `<br><span class="muted">${esc(desc)}</span>` : ""}<br><span class="muted mono" style="font-size:11px">${esc(k)}</span></td>
        <td class="feat-state">${on ? '<span class="badge s-active">ON</span>' : '<span class="badge">OFF</span>'}
          <button type="button" class="mini ${on ? "ghost" : ""}" data-feat="${k}" data-to="${on ? "0" : "1"}">${on ? "Turn off" : "Turn on"}</button></td></tr>`;
    };
    // The switch sits in the State cell: a separate column ended up scrolled out of sight beside the long descriptions.
    const table = (rows) => `<table class="tbl feat-tbl"><thead><tr><th>Setting</th><th>State</th></tr></thead><tbody>${rows}</tbody></table>`;
    const html = `<div class="view-head"><h1>Feature toggles</h1><span class="badge">${esc(tenant).toUpperCase()}</span></div>
      <p class="muted">Settings for this tenant only. The server enforces each one, so turning it off changes what the platform allows, not just what it shows. Changes apply at once; others see them on their next page load.</p>
      <h2>Case workflow rules</h2>
      ${table(Object.entries(WORKFLOW_RULES).map(([k, [label, desc]]) => row(k, label, desc)).join(""))}
      <h2>Capabilities</h2>
      ${table(Object.entries(FEATURE_LABELS).map(([k, label]) => row(k, label, "")).join(""))}`;
    return { html, wire: () => {
      document.querySelectorAll("[data-feat]").forEach((b) => b.addEventListener("click", async () => {
        const k = b.dataset.feat, to = b.dataset.to === "1";
        const label = (WORKFLOW_RULES[k] || [FEATURE_LABELS[k]])[0];
        if (!to && WORKFLOW_RULES[k] && !(await UI.confirm(`Turn off "${label}"?`, WORKFLOW_RULES[k][1], "Turn off", true))) return;
        try {
          await Api.program.setFeatures({ [k]: to });
          await App.featuresChanged();
          UI.toast(`${label}: ${to ? "on" : "off"}`);
          App.rerender();
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      }));
    } };
  }

  // ---- Case detail workspace -------------------------------------------------
  // What a TPA sees of a dispute it filed: where it stands, its deadlines and
  // its documents. The office's working page -- notes, review, letters,
  // money -- is not a filer's, and the API gives a filer none of it.
  // The fee box on a filer's own dispute: its state and a Pay now button.
  async function filerFeeBox(id, kind) {
    // The page is drawn after its data arrives: wait for the box to be on it.
    let box = null;
    for (let i = 0; i < 50 && !(box = document.getElementById("filer-fee-" + id)); i++) await new Promise((r) => setTimeout(r, 200));
    if (!box) return;
    try {
      const f = await Api.program.disputeFee(kind, id);
      box.innerHTML = ` ${feePill(f)} ${feePayable(f) ? `<button class="mini" onclick="Views.payDisputeFn('${kind}', '${id}', this, 'case')">Pay now — ${usdC(f.fee_cents)}</button>` : ""}`;
    } catch { box.innerHTML = ""; }
  }
  // The dispute's record for its TPA or health plan: every letter sent to it, as it was sent, with the documents
  // it carried, which download here as well as from the email's link.
  const secureLinkRe = /https?:\/\/\S*\/(?:s|api\/share)\/[0-9a-f]{48}\S*|\/(?:s|api\/share)\/[0-9a-f]{48}\S*/g;
  function sentRecordHtml(k, rec) {
    const mail = rec.mail || [], docs = rec.documents || [];
    if (!mail.length && !docs.length) return `<p class="muted">Nothing has been sent to you on this dispute yet. Letters and the documents they carry are kept here as they are sent.</p>`;
    const docBtn = (d) => `<li><button type="button" class="linkish" onclick="Views.previewSentDoc('${k}','${d.doc_id}', this.dataset.name)" data-name="${esc(d.filename)}">${esc(prettyDocName(d.filename))}</button>
      <button type="button" class="mini ghost" onclick="Views.sentDocGetFn('${k}','${d.doc_id}', this)" data-name="${esc(d.filename)}">Download</button></li>`;
    const used = new Set();
    const items = mail.map((m) => {
      const mine = docs.filter((d) => d.case_id === m.case_id && d.subject === m.subject);
      mine.forEach((d) => used.add(d.doc_id));
      return `<li class="sent-mail"><details><summary><b>${esc(m.subject || "Letter")}</b> <span class="muted">· ${fmtDate(m.sent_at)}${mine.length ? ` · ${mine.length} document${mine.length === 1 ? "" : "s"}` : ""}</span></summary>
        <div class="sent-body">${esc(String(m.body || "").replace(secureLinkRe, "[secure link — the documents are listed below]"))}</div></details>
        ${mine.length ? `<ul class="sent-docs">${mine.map(docBtn).join("")}</ul>` : ""}</li>`;
    }).join("");
    const rest = docs.filter((d) => !used.has(d.doc_id));
    return `${mail.length ? `<ul class="sent-list">${items}</ul>` : ""}
      ${rest.length ? `<h3>Other documents sent to you</h3><ul class="sent-docs">${rest.map(docBtn).join("")}</ul>` : ""}`;
  }
  async function sentRecordInto(el, k, caseId) {
    if (!el) return;
    try { el.innerHTML = sentRecordHtml(k, await Api.program.sentRecord(k, caseId)); }
    catch (e) { el.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  async function sentRecordOpen(k, caseId, caseNumber) {
    let rec;
    try { rec = await Api.program.sentRecord(k, caseId); } catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    await UI.modal({ title: `Letters and documents — ${esc(caseNumber || "")}`, wide: true, submitLabel: "Close", html: sentRecordHtml(k, rec) });
  }
  async function sentDocGetFn(k, docId, btn) {
    try { await Api.program.sentDocGet(k, docId, btn?.dataset.name); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // A dispute as its TPA or health plan sees it (party_dispute_view.go): the parties, what is disputed, where it
  // stands, its dates, claims and fees, and the Agency's outcome once its Final Order is issued.
  const FEE_KIND = { INITIAL_FEE: "Initial fee", FULL_REVIEW: "Full review", DEFAULT: "Default", LATE_FEE: "Late fee" };
  function disputeSummaryHtml(sum, kind) {
    const c = sum.case || {}, dates = sum.dates || [], claims = sum.claims || [], fees = sum.fees || [];
    const fact = (label, v) => v ? `<div><dt>${label}</dt><dd>${v}</dd></div>` : "";
    return `<dl class="dsum-facts">
        ${fact("Filed for", esc(c.client || c.initiating_party || "—") + (c.provider_group ? ` <span class="muted">· ${esc(c.provider_group)}</span>` : ""))}
        ${fact("Against", esc(c.health_plan || "—"))}
        ${fact("Your side", sum.side === "HEALTH_PLAN" ? "The health plan" : "The provider")}
        ${fact("In dispute", usdC(c.disputed_amount_cents))}
        ${fact("Filed", fmtDate(c.opened_at))}
        ${fact("Status", c.agency_status ? `<span class="badge">${esc(c.agency_status)}</span>` : badge(c.status))}
        ${fact("What it's about", esc(c.disputed_issue))}
      </dl>
      ${c.case_outcome ? `<div class="dsum-outcome"><b>Outcome:</b> ${esc(c.case_outcome)}${c.awarded_cents != null ? ` · awarded ${usdC(c.awarded_cents)}` : ""}${c.final_order_number ? ` · Final Order ${esc(c.final_order_number)}` : ""}</div>` : ""}
      ${dates.length ? `<h3>Dates</h3><ol class="dsum-dates">${dates.map((d) => `<li><span>${esc(d.label)}</span><b>${fmtDate(d.date)}</b></li>`).join("")}</ol>` : ""}
      ${claims.length ? `<h3>Claims <span class="muted">${claims.length}</span></h3><div class="tbl-wrap"><table class="tbl" data-nosearch><thead><tr><th>Claim</th><th>Date of service</th><th>Service</th><th>Billed</th><th>Paid</th><th>Disputed</th><th>Status</th></tr></thead><tbody>
        ${claims.map((x) => `<tr><td class="mono">${esc(x.claim_number)}</td><td>${esc(x.date_of_service ? fmtDate(x.date_of_service) : "—")}</td><td>${esc(x.type_of_service || x.cpt || "—")}</td>
          <td>${x.billed_cents != null ? usdC(x.billed_cents) : "—"}</td><td>${x.paid_cents != null ? usdC(x.paid_cents) : "—"}</td><td>${x.disputed_cents != null ? usdC(x.disputed_cents) : "—"}</td><td>${x.status ? badge(x.status) : "—"}</td></tr>`).join("")}</tbody></table></div>` : ""}
      ${fees.length ? `<h3>Fees billed to you</h3><div class="tbl-wrap"><table class="tbl" data-nosearch><thead><tr><th>Invoice</th><th>For</th><th>Amount</th><th>Status</th><th>Due</th></tr></thead><tbody>
        ${fees.map((f) => `<tr><td class="mono">${esc(f.invoice_no || "—")}</td><td>${esc(FEE_KIND[f.kind] || f.kind)}</td><td>${usdC(f.amount_cents)}</td><td>${badge(f.status)}${f.paid_at ? ` <span class="muted">${fmtDate(f.paid_at)}</span>` : ""}</td><td>${f.due_date ? fmtDate(f.due_date) : "—"}</td></tr>`).join("")}</tbody></table></div>` : ""}`;
  }
  async function disputeSummaryInto(el, kind, caseId) {
    if (!el) return;
    try { el.innerHTML = disputeSummaryHtml(await Api.program.disputeSummary(kind, caseId), kind); }
    catch (e) { el.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function filerCase(id) {
    const kind = can("TPA") ? "tpa" : can("HEALTH_PLAN") ? "hp" : ""; // an invited party pays and answers elsewhere
    if (kind) {
      filerFeeBox(id, kind); feeReturn(kind, () => filerFeeBox(id, kind)); filerDocReqBanner(id, kind);
      (async () => { let el = null; for (let i = 0; i < 50 && !(el = document.getElementById("filer-record-" + id)); i++) await new Promise((r) => setTimeout(r, 200)); sentRecordInto(el, kind, id);
        disputeSummaryInto(document.getElementById("filer-summary-" + id), kind, id); })();
    }
    let c;
    try { c = await Api.cases.get(id); }
    catch { return `<div class="view-head"><h1>Dispute not found</h1></div>
      <p class="muted">This dispute isn't one your organization filed, or it no longer exists.</p><p><a href="${outsideHome()}">Back to your disputes</a></p>`; }
    const [docs, clocks] = await Promise.all([Api.cases.documents(id).catch(() => []), Api.cm.clocks(id).catch(() => [])]);
    const d = c.details || {};
    const list = Array.isArray(docs) ? docs : (docs.documents || []);
    return `<div class="view-head"><h1><span class="mono">${esc(c.case_number)}</span></h1>${badge(c.status)}</div>
      <p class="muted">Filed for ${esc(d.initiating_party || "your client")} · opened ${fmtDate(c.opened_at)}</p>
      <div id="filer-docreq-${id}"></div>
      ${kind ? `<div class="card"><h2>Dispute details</h2><div id="filer-summary-${id}"><p class="muted">Loading…</p></div></div>` : ""}
      ${c.lock === "AWAITING_PAYMENT" ? `<div class="case-lock" role="status"><b>Awaiting payment.</b> The review starts once the initial fee is paid.
        <span id="filer-fee-${id}"></span></div>` : ""}
      ${clocks.length ? `<div class="sla-cluster">` + clocks.map((cl) => `<div class="sla-card"><span class="sla ${cl.state}" style="font-size:16px">${clockChip(cl)}</span>
        <span class="lbl">${esc(cl.label)}</span><span class="cite">due ${esc(cl.due)}</span>${cl.due && cl.state !== "closed" && cl.state !== "paused" ? countdown(cl.due) : ""}</div>`).join("") + `</div>` : ""}
      <div class="card"><h2>Documents</h2>${list.length && (can("TPA") || can("HEALTH_PLAN")) ? `<p><button class="mini secondary" onclick="Views.orgDocsDownload('${can("TPA") ? "tpa" : "hp"}', {case_id: '${id}'}, this)">Download all documents</button></p>` : ""}${list.length ? `<table><thead><tr><th>File</th><th>Type</th><th>Received</th><th></th></tr></thead><tbody>` +
        list.map((x) => `<tr><td>${esc(x.filename || x.name || "document")}</td><td>${esc(x.doc_type || "")}</td><td>${fmtDate(x.uploaded_at || x.created_at)}</td>
          <td><a href="javascript:void(0)" onclick="Views.previewCaseDoc('${id}','${x.doc_id}', this.dataset.name)" data-name="${esc(x.filename || "")}">view</a> ·
            <a href="javascript:void(0)" onclick="Views.downloadDoc('${id}','${x.doc_id}')">download</a></td></tr>`).join("") + `</tbody></table>`
        : `<p class="muted">No documents yet. Use the upload link in your email to send the claim documents.</p>`}</div>
      ${kind ? `<div class="card"><h2>Letters and documents sent to you</h2><div id="filer-record-${id}"><p class="muted">Loading…</p></div></div>` : ""}
      <p><a href="${outsideHome()}">Back to your disputes</a></p>`;
  }

  async function caseDetail(id) {
    if (outsideOnly()) return filerCase(id);
    try {
      const [c, docs, activities, checklist, rels, clocks, notesRes, prog] = await Promise.all([
        Api.cases.get(id), Api.cases.documents(id), Api.cases.activities(id),
        Api.cm.checklist(id), Api.cm.relationships(id), Api.cm.clocks(id).catch(() => []),
        Api.crm.notes("CASE", id).catch(() => ({ notes: [] })),
        Api.program.get().catch(() => null),
      ]);
      Palette.remember("case", c.id, c.case_number);
      // The case page: a header that says what this dispute is and where it stands, one "next step" banner, and
      // the rest in tabs by what people come to do (read the documents, review, write to the parties, bill,
      // look back). Every section keeps its own id, so the step buttons still find and open it (CaseLayout.reveal).
      const programmed = !!(prog && prog.config);
      const det = c.details || {};
      const P = { overview: "", documents: "", review: "", mail: "", billing: "", activity: "" };
      let html = CaseLayout.header(c, prog, clocks) + `<div id="next-action"></div>`;
      // The next-action recommender (nextaction.go) under the step banner: the steps say what is next on the
      // dispute; this adds what else is worth doing (a QA draft, documents to look at, possible duplicates).
      // Accepting it emits next_action_accept, as its telemetry expects; ignoring it is the implicit override.
      afterRender(() => Api.cases.nextAction(id).then((na) => {
        const a = na && na.next_action, box = document.getElementById("next-action");
        if (!a || !box || ["review_case", "triage", "submit_offer", "log_negotiation"].includes(a.action_id)) return;
        box.innerHTML = `<div class="na-also"><span class="muted">Also suggested:</span>
          <button type="button" class="mini" id="na-go">${esc(a.label)}</button> <span class="muted">${esc(a.reason)}</span></div>`;
        document.getElementById("na-go").addEventListener("click", () => {
          if (window.Telemetry) Telemetry.track("next_action_accept", a.action_id, { confidence: a.confidence });
          // Its links name a section ("#/cases/<id>#documents"): open that section of this page.
          const section = { documents: "case-docs", correspondence: "p-send-details", checklist: "case-checklist", relationships: "case-rel" }[(a.href.split("#")[2] || "")];
          const el = section && document.getElementById(section);
          if (el) { CaseLayout.reveal(el); if (el.tagName === "DETAILS") el.open = true; el.scrollIntoView({ behavior: "smooth", block: "start" }); }
          else location.hash = a.href.split("#").slice(0, 2).join("#");
        });
      }).catch(() => {}));
      if (c.lock) {
        html += `<div class="case-lock" role="status"><b>${c.lock === "AWAITING_PAYMENT" ? "Awaiting payment" : "Ineligible"} — this case is frozen.</b>
          ${c.lock === "AWAITING_PAYMENT"
            ? "Nothing can be done on it until the initial fee is paid. You can still write to the party (Letters &amp; email, call and inquiry log) to send payment reminders."
            : "The latest eligibility review found this dispute ineligible. You can still write to the party (Letters &amp; email, call and inquiry log); a new eligibility review is the only other action available."}</div>`;
        afterRender(() => freezeCasePage(c.lock));
      }
      html += programmed ? `<div id="case-next" class="case-next"><p class="muted">Loading the next step…</p></div>` : "";

      // Occasional actions live in one "More" menu. The federal IDR actions (offers, IDRE selection, party
      // invites, batching, the federal letters) only exist on federal cases.
      const acts = [];
      if (!programmed) {
        if (can("PARTY") && c.status === "INITIATED") acts.push(["Respond filed", () => Api.cases.signal(id, "RESPONSE_FILED", {}), { group: "Federal IDR", icon: "inbox", hint: "Record that your response has been filed" }]);
        if (can("PARTY") && c.status === "OFFER_WINDOW_OPEN" && App.feature("sealed_offers")) acts.push(["Submit sealed offer", () => offerForm(id), { group: "Federal IDR", icon: "money", hint: "Your offer, sealed until the window closes" }]);
        if (can("PARTY")) acts.push(["Mark fees paid", () => Api.cases.signal(id, "FEES_PAID", { party_id: Auth.claims().sub }), { group: "Money", icon: "money", hint: "Record that your administrative fee is paid" }]);
        if (can("ARBITRATOR", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") && App.feature("sealed_offers")) acts.push(["Offers panel", () => { location.hash = `#/cases/${id}/offers`; }, { group: "Federal IDR", icon: "money", hint: "See and manage both parties' offers" }]);
        if (can("ARBITRATOR")) acts.push(["Issue determination", () => determinationForm(id), { group: "Federal IDR", icon: "letter", hint: "Choose the winning offer and record why" }]);
        if (can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") && App.feature("party_workspace")) acts.push(["Invite party", () => partyInviteForm(id), { group: "Federal IDR", icon: "assign", hint: "Give a party access to this dispute" }]);
        if (can("CASE_MANAGER", "FEDERAL_ADMIN")) acts.push(["Finalize IDRE selection", () => Api.cases.signal(id, "SELECTION_FINALIZED", {}), { group: "Federal IDR", icon: "assign", hint: "Lock in the certified IDR entity" }]);
        if (can("CASE_MANAGER", "ARBITRATOR", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") && App.feature("batch_suggestions")) acts.push(["Batchable cases", () => batchableModal(id), { group: "Case", icon: "link", hint: "Disputes that could be batched with this one" }]);
        if (can("FINANCE") && ["DETERMINED", "CLOSED_PAID"].includes(c.status)) acts.push(["Record payment", () => Api.cases.signal(id, "PAYMENT_RECORDED", { amount_cents: c.qpa_cents }), { group: "Money", icon: "money", hint: "Record the determination payment" }]);
        if (can("CASE_MANAGER", "FEDERAL_ADMIN")) {
          acts.push(["Generate offer-window notice", () => Views.letter(id, "offer_window_notice"), { group: "Federal IDR", icon: "letter", hint: "The notice that opens the offer window" }]);
          acts.push(["Generate determination letter", () => Views.letter(id, "determination_letter"), { group: "Federal IDR", icon: "letter", hint: "The letter announcing the determination" }]);
        }
      }
      const isAssigner = can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
      if (isAssigner) acts.push([c.assigned_to ? "Reassign" : "Assign", () => Views.assign(id),
        { group: "Case", icon: "assign", hint: c.assigned_to ? "Hand this case to another reviewer" : "Give this case to a reviewer" }]);
      if (programmed && can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        const pd = c.program_dates || {};
        if (!pd.withdrawal_requested_at)
          acts.push(["Withdrawal requested", () => Views.wfAnytime(id, "PROVIDER_WITHDRAW"),
            { group: "Record what happened", icon: "withdraw", hint: "The provider or the health plan asked to withdraw" }]);
        else if (!pd.withdrawal_confirmed_at)
          acts.push(["Confirm the withdrawal", () => Views.wfAnytime(id, "WITHDRAWAL_CONFIRMED"),
            { group: "Record what happened", icon: "withdraw", hint: "Verified with the party: the Withdrawal letter is next" }]);
        acts.push(["RFI response received", () => Views.wfAnytime(id, "RFI_RESPONSE_RECEIVED"),
          { group: "Record what happened", icon: "inbox", hint: "The party answered the request for information" }]);
        acts.push(["Outreach done", () => Views.wfAnytime(id, "OUTREACH_DONE"),
          { group: "Record what happened", icon: "phone", hint: "Paid but no packet yet: the filer has been contacted" }]);
      }
      // A refund is decided by a person, never by the clock: the workflow only flags a potential refund.
      if (programmed && can("CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") && !(c.program_dates || {}).packet_complete_at
          && !/^Closed/.test(c.internal_status || ""))
        acts.push(["Close and refund", () => Views.wfAnytime(id, "REFUND_APPROVED"),
          { group: "Record what happened", icon: "money", hint: "No complete packet is coming: close the case and refund the initial fee" }]);
      if (can("CASE_MANAGER", "PM", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN") && App.feature("review_queues")) acts.push(["Request specialty review", () => reviewRequestForm(id), { group: "Case", icon: "review", hint: "Ask a coder, clinician or attorney to look at it" }]);
      if (can("CASE_MANAGER", "FEDERAL_ADMIN")) {
        acts.push(["Escalate", () => Views.escalate(id), { group: "Case", icon: "escalate", hint: "Notify supervisors that a deadline needs attention" }]);
        acts.push(["Link a related case", () => Views.relate(id), { group: "Case", icon: "link", hint: "Connect it to another dispute (duplicate, same claim…)" }]);
      }
      if (can("FINANCE")) acts.push(["Post fee transfer", () => Views.feeTransfer(id), { group: "Money", icon: "transfer", hint: "Admin fee, IDRE fee reserve, refund or settlement on the ledger" }]);
      html = html.replace("<!--case-more-->", CaseLayout.moreMenu(acts));

      // ---- Overview: steps (filled by wfLoad), deadlines (program panel), notes ----
      P.overview += `<div id="case-overview-prog"></div>`;
      const notes = notesRes.notes || [];
      const streamOpts = [["internal", "Internal"], ["coder", "Coder"], ["clinical", "Clinical"], ["legal", "Legal"], ["external_agency", "External agency"]];
      const streamRoles = prog?.config?.notes_stream_roles || {};
      const allowedStreams = streamOpts.filter(([key]) => { const roles = streamRoles[key]; return !roles || !roles.length || can(...roles); });
      P.overview += `<section class="case-card" id="case-notes"><h3>Notes <span class="muted">${notes.length || ""}</span></h3>
        <form id="note-form" class="note-form">
          <textarea name="body" rows="2" placeholder="Add a note for the team…" required></textarea>
          <div class="note-form-row"><select name="stream" aria-label="Who can read it">
            ${(allowedStreams.length ? allowedStreams : streamOpts).map(([v, label]) => `<option value="${v}">${label}</option>`).join("")}</select>
            <button class="mini">Add note</button></div></form>` +
        (notes.length ? `<ul class="note-list">` + notes.map((n) => `<li><div>${esc(n.body)}</div>
            <div class="muted">${badge(n.stream)} ${esc(n.author_name || n.author || "")} · ${fmtDate(n.created_at)}</div></li>`).join("") + `</ul>`
          : `<p class="muted">No notes yet.</p>`) + `</section>`;

      // ---- Documents: requests, then the documents by folder ----
      P.documents += `<section class="case-card"><h3>Document requests</h3><div id="docreq-box"><p class="muted">Loading…</p></div></section>`;
      caseDocReqBox(id);
      P.documents += CaseLayout.documents(id, docs);
      P.documents += `<div id="analysis"></div>`;

      // ---- Review: clinical terms read from the documents, other disputes sharing them, and what each outbound
      // letter's privacy scan found (deid.go). Loaded when opened. ----
      if (can("CASE_MANAGER", "PM", "ATTORNEY", "CODER", "NURSE_PHYSICIAN", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        P.review += `<details class="prog-sec" id="case-privacy" data-tab="review"><summary>Clinical terms &amp; privacy <span class="muted">— from the documents, possible duplicates, and each letter's privacy scan</span></summary>
          <div id="case-privacy-box" data-case-id="${esc(id)}"><p class="muted">Loading…</p></div></details>`;
        afterRender(() => document.getElementById("case-privacy")?.addEventListener("toggle", (ev) => { if (ev.target.open) casePrivacyBox(id); }));
      }
      // ---- Review: the Copilot's advisory drafts and the SOP checklist (the program's sections join here) ----
      if (can("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        P.review += `<details class="prog-sec" id="case-copilot"><summary>Copilot drafts <span class="muted">— advisory; nothing goes out without approval</span></summary>
          <div id="copilot"><p class="muted">Loading…</p></div>
          <div class="actions">
            <button class="mini" onclick="Views.copilotDraftQA('${id}','determination_rationale',this)">✍ Draft determination rationale</button>
            <button class="mini" onclick="Views.copilotDraftQA('${id}','correspondence',this)">✉ Draft correspondence</button>
            <button class="mini" onclick="Views.copilotPropose('${id}',this)">⚙ Propose action batch</button>
          </div><div id="copilot-batches"></div></details>`;
        Api.program.copilotBriefLatest(id).then((r) => { const b = document.getElementById("copilot"); if (b) b.innerHTML = copilotCard(r.brief, r.generated_at, id); })
          .catch(() => { const b = document.getElementById("copilot"); if (b) b.innerHTML = copilotCard(null, null, id); });
        copilotLoadBatches(id);
      }
      const stages = {};
      checklist.forEach((i) => { (stages[i.stage] = stages[i.stage] || []).push(i); });
      const STAGE_ORDER = ["INTAKE", "ELIGIBILITY", "PROVIDER_PERMISSION", "PLAN_NOTIFICATION", "DETERMINATION", "PAYMENT"];
      const stageRank = (x) => (STAGE_ORDER.indexOf(x) < 0 ? 99 : STAGE_ORDER.indexOf(x));
      const mayTick = (i) => (i.owner_role ? i.can_check : can("CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN"));
      if (checklist.length) {
        const done = checklist.filter((i) => i.done).length;
        P.review += `<details class="prog-sec" id="case-checklist"><summary>SOP checklist <span class="muted">— ${done} of ${checklist.length} done</span></summary>` +
          Object.entries(stages).sort((x, y) => stageRank(x[0]) - stageRank(y[0])).map(([stage, items]) =>
            `<h4>${esc(stage.charAt(0) + stage.slice(1).toLowerCase().replace(/_/g, " "))}</h4><ul class="checklist">` + items.map((i) =>
              `<li>${i.done ? "✅" : (mayTick(i) ? `<button class="mini" onclick="Views.check('${i.id}')">Done</button>` : "⬜")}
                ${esc(i.item)}${i.owner ? ` <span class="chip">${esc(i.owner)}</span>` : ""}${i.done_by ? ` <span class="muted">(${esc(i.done_by)})</span>` : ""}</li>`).join("") + `</ul>`).join("") + `</details>`;
      }

      // ---- Billing: money on this dispute and the time spent on it ----
      if (can("CASE_MANAGER", "PM", "FINANCE", "STATE_AUDITOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        P.billing += `<section class="case-card"><h3>Money on this dispute</h3><div id="fin-box"><p class="muted">Loading…</p></div></section>`;
        caseFinBox(id).then((h) => { const b = document.getElementById("fin-box"); if (b) b.innerHTML = h; });
      }
      if (can("CASE_MANAGER", "PM", "CODER", "DOCTOR", "NURSE", "NURSE_PHYSICIAN", "ARBITRATOR", "ATTORNEY", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
        P.billing += `<section class="case-card" id="case-time"><h3>Time on this dispute</h3><div id="time-box"><p class="muted">Loading…</p></div>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.timeAdd('${id}', this)">
            <input name="hours" type="number" step="0.25" min="0.25" max="24" placeholder="hours" required aria-label="Hours worked" />
            ${timeRoleSelect()}
            <input name="entry_date" type="date" aria-label="Date worked" />
            <input name="note" placeholder="what was done" maxlength="500" />
            <label><input type="checkbox" name="billable" checked /> billable</label>
            <button class="mini">Log time</button></form>
          <p class="muted">Recorded under your name and role. A correction is a new entry, never an edit.</p></section>`;
        timeCaseBox(id).then((h) => { const b = document.getElementById("time-box"); if (b) b.innerHTML = h; });
      }

      // ---- Activity: what happened, related disputes ----
      P.activity += CaseLayout.timeline(activities);
      if (rels.length)
        P.activity += `<section class="case-card"><h3>Related disputes</h3><ul class="plain">` + rels.map((r) =>
          `<li>${badge(r.rel_type)} <a href="#/cases/${esc(r.related_case_id)}" class="mono">${esc(r.related_case_number || r.related_case_id.slice(0, 8) + "…")}</a></li>`).join("") + `</ul></section>`;
      P.activity += `<div id="gnn-rel"></div>`;
      Api.graph.related(id).then((g) => {
        const box = document.getElementById("gnn-rel");
        const preds = ((g && g.predictions) || []).filter((x) => x.score >= 0.5);
        if (!box || !preds.length) { if (box) box.innerHTML = ""; return; }
        box.innerHTML = `<details class="prog-sec"><summary>Possibly related disputes <span class="muted">— suggested from shared parties and claims</span></summary>
          <ul class="plain">${preds.map((x) => `<li><a href="#/cases/${esc(x.case_id)}" class="mono">${esc(x.case_number || x.case_id.slice(0, 8) + "…")}</a>
            <span class="muted">${(x.score * 100).toFixed(0)}% match</span> · <a href="javascript:void(0)" onclick="Views.relate('${id}')">link it</a></li>`).join("")}</ul></details>`;
      }).catch(() => { const b = document.getElementById("gnn-rel"); if (b) b.innerHTML = ""; });

      html += CaseLayout.tabs(id, P, { programmed, billing: !!P.billing });
      // The program's own sections (state rules) arrive after; each says which tab it belongs to.
      html += `<div id="prog" hidden></div>`;
      programPanel(id).then((h) => {
        const b = document.getElementById("prog");
        if (!b) return;
        b.innerHTML = h;
        CaseLayout.distribute(b);
      });
      afterRender(() => { loadCaseAssignee(id, isAssigner); CaseLayout.story(id); CaseLayout.bind(id); });

      afterRender(() => {
        acts.forEach((a, i) =>
          document.querySelector(`[data-act="${i}"]`)?.addEventListener("click", async () => {
            try { await a[1](); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }));
        $("#up")?.addEventListener("submit", async (ev) => {
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
    } catch (e) {
      // A Reviewer opening a case that is someone else's (a shared link, an old bookmark): say so plainly.
      if (e.status === 404 && Auth.reviewerScoped()) {
        return `<div class="empty-state"><h1>This case isn't on your list</h1>
          <p class="muted">You see unassigned cases and the ones assigned to you. This one is with another reviewer, or it no longer exists.
          Ask your PM if you need it.</p><p><a class="button" href="#/cases">Back to your disputes</a></p></div>`;
      }
      return err(e);
    }
  }

  async function moveDoc(caseId, docId, folder, sel) {
    if (!folder) return;
    await UI.run(sel, async () => {
      try { await Api.cases.moveDoc(caseId, docId, folder); UI.toast(`Moved to ${folder}`); App.rerender(); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  // The document viewer: a PDF, an image, text or one of the office's HTML pages shows in place, with Download;
  // a file the browser can't show (Word, a ZIP) says so and downloads.
  const VIEW_TYPES = { pdf: "application/pdf", png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif",
    webp: "image/webp", tif: "image/tiff", tiff: "image/tiff", txt: "text/plain", csv: "text/plain", "835": "text/plain",
    edi: "text/plain", html: "text/html", htm: "text/html" };
  async function previewDoc(path, name) {
    let got;
    try { got = await Api.fetchBlob(path); } catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    const fname = name || got.name || "document";
    const ext = ((fname.match(/\.([a-z0-9]+)$/i) || [])[1] || "").toLowerCase();
    const type = VIEW_TYPES[ext] || got.blob.type || "";
    const blob = new Blob([got.blob], { type });
    const url = URL.createObjectURL(blob);
    let body;
    if (type === "application/pdf") body = `<iframe class="doc-view" src="${url}" title="${esc(fname)}"></iframe>`;
    else if (type.startsWith("image/") && type !== "image/tiff") body = `<div class="doc-view doc-view-img"><img src="${url}" alt="${esc(fname)}" /></div>`;
    else if (type === "text/html") body = `<iframe class="doc-view" sandbox src="${url}" title="${esc(fname)}"></iframe>`;
    else if (type === "text/plain") body = `<pre class="doc-view doc-view-text">${esc((await blob.text()).slice(0, 300000))}</pre>`;
    else body = `<p class="muted">This kind of file${ext ? ` (.${esc(ext)})` : ""} can't be shown here — download it to open it.</p>`;
    await UI.modal({ title: esc(fname), wide: true, submitLabel: "Close",
      html: `${body}<p class="doc-view-acts"><a class="button mini" href="${url}" download="${esc(fname)}">Download</a></p>` });
    setTimeout(() => URL.revokeObjectURL(url), 2000);
  }
  // The compose form as a draft body (the send and the preview read it the same way).
  function corrFormBody(f) {
    const split = (v) => String(v || "").split(",").map((x) => x.trim()).filter(Boolean);
    return { template: f.template.value, body: f.body.value, to: split(f.to.value), cc: split(f.cc.value),
      rfi_to: f.rfi_to ? f.rfi_to.value : "", auto_share: !!f.auto_share?.checked, auto_download: !!f.auto_download?.checked };
  }
  // How the email will read when it goes out: its recipients, subject, text, what its links carry, and the
  // fill-ins still to do. Nothing is sent or created.
  async function corrPreviewFn(form, caseId) {
    let pv;
    try { pv = await Api.cases.corrPreview(caseId, corrFormBody(form)); } catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    const list = (xs) => (xs || []).length ? xs.map(esc).join(", ") : '<span class="error">nobody yet — add an address</span>';
    await UI.modal({ title: "Preview of the email", wide: true, submitLabel: "Close", html: `
      <dl class="mail-pv-head"><dt>To</dt><dd>${list(pv.to)}</dd>${(pv.cc || []).length ? `<dt>Cc</dt><dd>${pv.cc.map(esc).join(", ")}</dd>` : ""}
        <dt>Subject</dt><dd><b>${esc(pv.subject)}</b></dd></dl>
      ${(pv.unfilled || []).length ? `<p class="due-over">Still to fill in: ${pv.unfilled.map((x) => `<code>${esc(x)}</code>`).join(" ")} — edit the message before sending.</p>` : ""}
      <pre class="qa-body mail-pv-body">${esc(pv.body)}</pre>
      ${pv.download_carries ? `<p class="muted"><b>The download link will carry:</b> ${esc(pv.download_carries)}</p>` : ""}
      <p class="muted">${pv.qa_role ? `Sending puts it in the QA gate for the ${esc(String(pv.qa_role).replace(/_/g, " ").toLowerCase())}; it goes out once approved.` : "It goes out as soon as you send it."}
        Secure links are created when it is sent.</p>` });
  }
  // Clinical terms & privacy on the case page (deid.go).
  const CLIN_TYPE = { DRUG: "Drugs", CONDITION: "Conditions", PROCEDURE: "Procedures", ANATOMY: "Anatomy" };
  async function casePrivacyBox(caseId) {
    const box = document.getElementById("case-privacy-box");
    if (!box || box.dataset.loaded === caseId) return;
    box.dataset.loaded = caseId;
    const [cl, ov, dr] = await Promise.all([Api.cases.clinical(caseId).catch(() => null), Api.cases.clinicalOverlaps(caseId).catch(() => null),
      Api.cases.deidReports(caseId).catch(() => null)]);
    const terms = {};
    for (const d of (cl && cl.documents) || []) for (const e of d.entities || []) {
      const k = e.type || "OTHER", t = String(e.text || "").toLowerCase();
      if (t) ((terms[k] ||= {})[t] = ((terms[k] || {})[t] || 0) + 1);
    }
    const termHtml = Object.keys(terms).length ? `<div class="clin-terms">${Object.entries(terms).map(([k, m]) => `<div><b>${esc(CLIN_TYPE[k] || k)}</b>
        <p>${Object.entries(m).sort((a, b) => b[1] - a[1]).slice(0, 30).map(([t, n]) => `<span class="chip">${esc(t)}${n > 1 ? ` ×${n}` : ""}</span>`).join(" ")}</p></div>`).join("")}</div>`
      : `<p class="muted">No clinical terms read yet. They are read from documents analyzed after de-identification was turned on.</p>`;
    const overlaps = (ov && ov.overlaps) || [];
    const ovHtml = overlaps.length ? `<table class="tbl" data-nosearch><thead><tr><th>Dispute</th><th>Shared terms</th></tr></thead><tbody>
        ${overlaps.map((o) => `<tr><td><a class="mono" href="#/cases/${esc(o.case_id)}">${esc(o.case_number || o.case_id.slice(0, 8))}</a></td><td>${o.overlap}: ${(o.shared_terms || []).map(esc).join(", ")}</td></tr>`).join("")}</tbody></table>`
      : `<p class="muted">No other dispute shares two or more of these clinical terms.</p>`;
    const reps = (dr && dr.reports) || [];
    const ACTION = { SCAN: ["Scanned", ""], FLAG: ["Flagged — sent", "s-ck-review"], BLOCK: ["Held", "s-rejected"] };
    const repHtml = reps.length ? `<table class="tbl" data-nosearch><thead><tr><th>When</th><th>What</th><th>Result</th><th>Found</th></tr></thead><tbody>
        ${reps.map((x) => { const [t, c] = ACTION[x.action] || [x.action, ""]; const counts = Object.entries(x.counts || {}).map(([k, n]) => `${esc(k.toLowerCase().replace(/_/g, " "))} ${n}`).join(", ");
          return `<tr><td>${fmtDate(x.created_at)}</td><td>${esc(String(x.artifact || "").replace(/_/g, " "))}</td><td><span class="badge ${c}">${t}</span></td><td class="muted">${counts || "nothing"}</td></tr>`; }).join("")}</tbody></table>`
      : `<p class="muted">No letter on this dispute has been scanned yet.</p>`;
    box.innerHTML = `<h3>Clinical terms in the documents</h3>${termHtml}
      <h3>Other disputes with the same clinical picture</h3><p class="muted">Two or more shared drugs, conditions or procedures — a possible duplicate or the same episode of care.</p>${ovHtml}
      <h3>Privacy scan of outgoing letters</h3><p class="muted">Each letter is scanned for direct identifiers before it goes out.
        ${App.feature("deid_hold_mail") ? "A letter with them is held until it is redacted." : "Holding is off: a letter with them is flagged here and still goes (Feature toggles → Hold a letter…)."}</p>${repHtml}`;
  }
  // QA draft quality by template (qa_learning.go): how often each template's drafts pass QA untouched.
  async function qaDraftQualityBox() {
    const box = document.getElementById("qa-dq-box");
    if (!box || box.dataset.loaded) return;
    box.dataset.loaded = "1";
    let r;
    try { r = await Api.cases.qaDraftQuality(90); } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; return; }
    const ts = r.templates || [];
    const pct = (a, b) => (b ? Math.round((100 * a) / b) + "%" : "—");
    box.innerHTML = ts.length ? `<div class="tbl-wrap"><table class="tbl qa-tbl"><thead><tr><th>Template</th><th>Drafts</th><th>Approved as written</th><th>Approved after edits</th><th>Sent back</th><th>Edited, on average</th><th>Reviewers</th></tr></thead><tbody>
        ${ts.map((x) => `<tr><td class="qa-tbl-main">${esc(qaTpl(x.artifact))} <span class="muted">${esc(x.channel || "")}</span></td><td data-label="Drafts">${x.drafts}</td>
          <td data-label="As written">${pct(x.approved_unedited, x.drafts)}</td><td data-label="After edits">${pct(x.approved - x.approved_unedited, x.drafts)}</td>
          <td data-label="Sent back">${pct(x.rejected, x.drafts)}</td><td data-label="Edited">${x.avg_edit_ratio != null ? Math.round(x.avg_edit_ratio * 100) + "% of the text" : "—"}</td><td data-label="Reviewers">${x.reviewers}</td></tr>`).join("")}</tbody></table></div>`
      : `<p class="muted">No QA decisions recorded yet in the last ${r.days || 90} days. Each approval or send-back from now on is counted here.</p>`;
  }
  // Portal productivity (telemetry.go): how much work each screen takes, and how often the suggestions are taken.
  async function productivityBox() {
    const box = document.getElementById("rpt-productivity");
    if (!box) return;
    let r;
    try { r = await Api.cases.telemetryMetrics(30); } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; return; }
    const ws = r.workflows || [], tasks = r.tasks || [];
    const rate = (v) => (v == null ? "—" : Math.round(v * 100) + "%");
    const TASK = { file_dispute: "File a dispute", bulk_filing: "Bulk filing", qa_decision: "QA decision", send_letter: "Send a letter",
      answer_document_request: "Answer a document request" };
    const secs = (v) => (v == null ? "—" : v < 90 ? `${Math.round(v)} s` : `${(v / 60).toFixed(1)} min`);
    const taskHtml = tasks.length ? `<h3>Tasks</h3><p class="muted">Each job from its first touch to done: how many, how often refused, and what a typical one takes.</p>
      <div class="tbl-wrap"><table class="tbl"><thead><tr><th>Task</th><th>Where</th><th>Done</th><th>Refused</th><th>Typical time</th><th>Clicks</th><th>Keystrokes</th><th>With prefill</th></tr></thead><tbody>
      ${tasks.map((t) => `<tr><td><b>${esc(TASK[t.task] || t.task.replace(/_/g, " "))}</b></td><td class="muted">${esc(String(t.workflow).replace(/_/g, " "))}</td>
        <td>${t.done}</td><td>${t.errors ? `<span class="badge s-ck-review">${t.errors}</span>` : 0}</td><td>${secs(t.median_seconds)}</td>
        <td>${t.avg_clicks == null ? "—" : Number(t.avg_clicks).toFixed(1)}</td><td>${t.avg_keys == null ? "—" : Math.round(t.avg_keys)}</td><td>${t.with_prefill || 0}</td></tr>`).join("")}</tbody></table></div>` : "";
    const n1 = (v) => (v == null ? "—" : Number(v).toFixed(1));
    box.innerHTML = taskHtml + (ws.length ? `<h3>Screens</h3><div class="tbl-wrap"><table class="tbl"><thead><tr><th>Screen</th><th>Sessions</th><th>Clicks per session</th><th>Keystrokes per session</th><th>Submits</th><th>Prefill kept</th><th>Suggestion taken</th></tr></thead><tbody>
        ${ws.map((x) => `<tr><td>${esc(String(x.workflow || "").replace(/_/g, " "))}</td><td>${x.sessions}</td><td>${n1(x.clicks_per_session)}</td><td>${n1(x.keystrokes_per_session)}</td><td>${x.submits}</td>
          <td>${rate(x.prefill_accept_rate)}</td><td>${rate(x.next_action_accept_rate)}</td></tr>`).join("")}</tbody></table></div>`
      : `<p class="muted">Nothing recorded yet in the last ${r.days || 30} days. The portal records clicks, typing and submits per screen (never what is typed).</p>`);
  }

  // The mail tracker (mail_tracker.go): each email of the case with where it stands, from draft to delivery.
  const MAIL_STATE = {
    IN_QA: ["Waiting for QA", "s-ck-review"], APPROVED: ["Approved — not sent yet", "s-ck-review"], SENT_BACK: ["Sent back", "s-rejected"],
    PENDING: ["Sending", "s-ck-review"], SENT: ["Sent", "s-filed"], DELIVERED: ["Delivered", "s-paid"], BOUNCED: ["Bounced", "s-rejected"],
    FAILED: ["Failed to send", "s-rejected"], REJECTED: ["Sent back", "s-rejected"], RECEIVED: ["Received", ""] };
  const mailState = (st) => { const [t, c] = MAIL_STATE[st] || [st, ""]; return `<span class="badge ${c}">${esc(t)}</span>`; };
  async function mailTrackerBox(caseId, filter) {
    const box = document.getElementById("p-mail-tracker");
    if (!box || box.dataset.caseId !== caseId) return;
    let r;
    try { r = await Api.cases.mailTracker(caseId); } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; return; }
    if (box.dataset.caseId !== caseId) return;
    const all = r.mail || [], n = r.counts || {};
    mailTrackerBox._data = all;
    const groups = { "": all.length, waiting: (n.IN_QA || 0) + (n.APPROVED || 0) + (n.PENDING || 0), sent: (n.SENT || 0) + (n.DELIVERED || 0),
      problem: (n.FAILED || 0) + (n.BOUNCED || 0) + (n.SENT_BACK || 0) + (n.REJECTED || 0), received: n.RECEIVED || 0 };
    const inGroup = (m, g) => !g || (g === "waiting" && ["IN_QA", "APPROVED", "PENDING"].includes(m.status)) || (g === "sent" && ["SENT", "DELIVERED"].includes(m.status))
      || (g === "problem" && ["FAILED", "BOUNCED", "SENT_BACK", "REJECTED"].includes(m.status)) || (g === "received" && m.status === "RECEIVED");
    const f = filter ?? mailTrackerBox._filter ?? "";
    mailTrackerBox._filter = f;
    const shown = all.filter((m) => inGroup(m, f));
    const linkLine = (l) => {
      const used = l.kind === "upload" ? (l.uploads ? `${plural(l.uploads, "file")} uploaded` : "nothing uploaded yet")
        : (l.downloads ? `opened ${l.downloads} time${l.downloads === 1 ? "" : "s"}` : "not opened yet");
      return `<li>${l.kind === "upload" ? "Upload link" : "Download link"}: <b>${used}</b>${l.last_used ? ` · last ${fmtDate(l.last_used)}` : ""}
        <span class="muted">· ${Math.max(l.max_uses - l.uses, 0)} of ${l.max_uses} uses left · ${new Date(l.expires_at) < new Date() ? "expired" : "expires"} ${fmtDate(l.expires_at)}</span></li>`;
    };
    const opened = (m) => (m.links || []).some((l) => l.downloads || l.uploads);
    box.innerHTML = all.length ? `<div class="list-bar">${ListUI.segs("Show", [
        { value: "", label: "All", count: groups[""] }, { value: "waiting", label: "Waiting", count: groups.waiting },
        { value: "sent", label: "Sent", count: groups.sent }, { value: "problem", label: "Problems", count: groups.problem },
        { value: "received", label: "Received", count: groups.received }].filter((o) => !o.value || o.count), f, "Views.mailTrackerFilter")}</div>
      ${shown.length ? `<ul class="mail-track">${shown.map((m, i) => `<li class="mail-track-row">
        <details><summary>
          <span class="mt-when">${fmtDate(m.at)}</span>
          <span class="mt-subj"><b>${esc(m.subject || "(no subject)")}</b><span class="muted">${esc(m.direction === "IN" ? "from " + (m.by || "—") : "to " + ((m.to || []).join(", ") || "nobody"))}</span></span>
          <span class="mt-state">${mailState(m.status)}${opened(m) ? ' <span class="badge s-paid" title="The recipient used a link in it">link used</span>' : ""}</span>
        </summary>
        <div class="mt-detail">
          <dl class="mail-pv-head">${m.direction === "IN" ? `<dt>From</dt><dd>${esc(m.by || "—")}</dd>` : `<dt>To</dt><dd>${esc((m.to || []).join(", ") || "—")}</dd>${(m.cc || []).length ? `<dt>Cc</dt><dd>${esc(m.cc.join(", "))}</dd>` : ""}`}
            ${m.template ? `<dt>Template</dt><dd>${esc(m.template.replace(/_/g, " "))}</dd>` : ""}
            ${m.drafted_by ? `<dt>Drafted by</dt><dd>${esc(m.drafted_by)}</dd>` : m.direction === "OUT" && m.by ? `<dt>Sent by</dt><dd>${esc(m.by)}</dd>` : ""}
            ${m.approved_by ? `<dt>QA</dt><dd>${esc(m.approved_by)}${m.approved_at ? ", " + fmtDate(m.approved_at) : ""}</dd>` : ""}
            ${m.status_at && m.status !== "IN_QA" ? `<dt>Status since</dt><dd>${fmtDate(m.status_at)}</dd>` : ""}</dl>
          ${m.error ? `<p class="due-over">${esc(m.status === "SENT_BACK" ? "QA note: " : "")}${esc(m.error)}</p>` : ""}
          ${(m.links || []).length ? `<ul class="mt-links">${m.links.map(linkLine).join("")}</ul>` : ""}
          ${(m.documents || []).length ? `<div><b>Documents it carries</b><ul class="sent-docs">${m.documents.map((d) => `<li><button type="button" class="linkish" data-name="${esc(d.filename)}" onclick="Views.previewCaseDoc('${esc(caseId)}','${d.doc_id}', this.dataset.name)">${esc(d.filename || "document")}</button></li>`).join("")}</ul></div>` : ""}
          <pre class="qa-body mail-pv-body">${esc(m.body || "")}</pre>
          ${m.qa_id || m.status === "IN_QA" ? `<p><a class="button mini" href="#/qa">Open the QA gate</a></p>` : ""}
        </div></details></li>`).join("")}</ul>` : `<p class="muted">No email in this group.</p>`}`
      : `<p class="muted">No email on this dispute yet. Letters you write below, and the ones its steps draft, are tracked here from draft to delivery.</p>`;
  }
  function mailTrackerFilter(g) { const b = document.getElementById("p-mail-tracker"); if (b) mailTrackerBox(b.dataset.caseId, g); }

  function previewCaseDoc(caseId, docId, name) { return previewDoc(Api.cases.downloadUrl(caseId, docId), name); }
  function previewSentDoc(k, docId, name) { return previewDoc(Api.program.sentDocPath(k, docId), name); }

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
      // Every extracted field is reviewable in place: a corrected value is
      // posted as doc-intel feedback (the learning loop's training signal)
      // and shown immediately in a "correction sent" state. Corrections are
      // append-only facts — recurring ones are mined into extraction
      // patterns, so the platform gets measurably better on the payer
      // templates this tenant actually sees.
      const ex = r.extracted || {};
      const rows = Object.entries(ex).filter(([, v]) => v !== null && v !== "" && v !== undefined)
        .map(([k, v]) => `<tr>
          <td>${esc(k)}</td><td>${esc(typeof v === "object" ? JSON.stringify(v) : String(v))}</td>
          <td><button class="link" onclick="Views.correctField('${caseId}','${docId}','${esc(k)}','${esc(String(typeof v === "object" ? JSON.stringify(v) : v)).replace(/'/g, "\\'")}', this)">correct</button></td>
        </tr>`).join("");
      box.innerHTML = `<h3>Document analysis ${badge(a.status)}</h3>
        <p class="muted">type: ${esc(a.doc_type)} · seal detected: ${r.seal_detected ? "yes" : "no"} · tables: ${r.table_count ?? 0}</p>
        ${rows ? `<table class="table"><thead><tr><th>Field</th><th>Extracted value</th><th></th></tr></thead><tbody>${rows}</tbody></table>
        <p class="muted">Wrong value? Click <b>correct</b> — the correction trains future extractions on documents like this one.</p>` : `<p class="muted">No fields extracted.</p>`}
        <details><summary>Raw extraction JSON</summary><pre>${esc(JSON.stringify(ex, null, 2))}</pre></details>
        ${hasFindings ? `<p class="error">Findings: ${esc(JSON.stringify(r.findings))}</p>` : ""}
        <p>${retryBtn}</p>`;
    } catch (e) {
      box.innerHTML = e.status === 404
        ? `<h3>Document analysis</h3><p class="muted">No analysis result yet — the document is still queued for the OCR/extraction pipeline (or was uploaded before doc-intel processed it). Try again shortly.</p><p>${retryBtn}</p>`
        : err(e);
    }
  }

  async function correctField(caseId, docId, field, extractedValue, btn) {
    const corrected = window.prompt(`Correct value for "${field}":`, extractedValue === "null" ? "" : extractedValue);
    if (corrected === null) return;  // cancelled
    try {
      await Api.cases.analysisFeedback(caseId, docId, {
        field, extracted_value: extractedValue === "null" ? "" : extractedValue,
        corrected_value: corrected,
      });
      btn.textContent = "✓ sent";
      btn.disabled = true;
      UI.toast("Correction recorded — it will train future extractions");
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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
    // Phase 2 prefill: suggestions arrive as {value, confidence, source,
    // evidence} per field. Applying one fills the input and shows a "why"
    // badge; on submit, final values are compared against every suggestion
    // shown and prefill_accept / prefill_override telemetry is emitted —
    // that stream calibrates (and later replaces) the suggester.
    const suggested = {}; // field -> suggested value actually shown
    const applyPrefill = (fields) => {
      for (const [name, s] of Object.entries(fields || {})) {
        const input = document.querySelector(`#nd [name="${name === "qpa" ? "qpa" : name}"]`);
        if (!input || !s || !s.value) continue;
        // Never clobber something the user already typed.
        if (input.value && input.value !== suggested[name]) continue;
        input.value = s.value;
        suggested[name] = s.value;
        let badge = input.parentElement.querySelector(".pf-badge");
        if (!badge) {
          badge = document.createElement("span");
          badge.className = "pf-badge muted";
          badge.style.cssText = "font-size:.8em;margin-left:.5em";
          input.parentElement.appendChild(badge);
        }
        badge.textContent = `suggested (${s.source === "doc" ? "from document" : "from history"}, ${s.confidence} confidence): ${s.evidence}`;
      }
    };
    afterRender(async () => {
      try {
        const base = await Api.cases.prefill();
        applyPrefill(base.fields);
        const sel = $("#pf-doc");
        if (sel && base.recent_docs && base.recent_docs.length) {
          sel.closest("label").style.display = "";
          sel.innerHTML = `<option value="">— prefill from a recently analyzed document —</option>` +
            base.recent_docs.map((d) => `<option value="${esc(d.doc_id)}">${esc(d.doc_type || "document")} · ${esc((d.analyzed_at || "").slice(0, 10))} · case ${esc((d.case_id || "").slice(0, 8))}</option>`).join("");
          sel.addEventListener("change", async () => {
            if (!sel.value) return;
            try { applyPrefill((await Api.cases.prefill(sel.value)).fields); }
            catch (e) { UI.toast(e.message, { kind: "warn" }); }
          });
        }
      } catch { /* prefill is advisory — the blank form still works */ }

      $("#nd").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      // Accept/override bookkeeping for every suggestion that was shown.
      const shown = { ...suggested };
      for (const k of Object.keys(suggested)) delete suggested[k];
      const finalVal = { case_number: f.case_number, qpa: f.qpa || "", service_line: f.service_line,
                         plan_type: f.plan_type, provider_id: f.provider_id, payer_id: f.payer_id };
      for (const [k, v] of Object.entries(shown)) {
        if (window.Telemetry) Telemetry.track(finalVal[k] === v ? "prefill_accept" : "prefill_override", k);
      }
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
    }); });
    return `<h1>New dispute</h1><form id="nd" class="form">

      <label id="pf-doc-label" style="display:none">Prefill from a recent document <select id="pf-doc"></select></label>
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
      ${(afterRender(productivityBox), "")}
      <details class="prog-sec" open><summary>Portal productivity <span class="muted">— last 30 days, per screen</span></summary><div id="rpt-productivity"><p class="muted">Loading…</p></div></details>
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
    if (det.volume_large) {
      const vv = det.volume_violations || [];
      facts.push(vv.length
        ? `<span class="badge warn" title="${esc(vv.join("; "))}">⚠ Large-volume policy: ${vv.length} violation(s) — ineligible per Capitol Bridge policy, resubmission permitted</span>`
        : `<span class="chip">Large-volume dispute — policy compliant</span>`);
    }
    let html = `${facts.length ? `<p class="fact-row" data-tab="overview">${facts.join(" ")}</p>` : ""}
      <div id="wf-steps" class="card wf-card" data-tab="overview"><p class="muted">Loading the steps…</p></div>
      <details class="prog-sec" data-wf-admin data-tab="activity"><summary>Status override (admin)</summary>
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

      <details class="prog-sec" id="p-deadlines" data-tab="overview"><summary>Deadlines</summary>
      <p class="muted">Each deadline as it stands, from its program date.${can("PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
        ? " As the PM or an admin you can extend any of them except the statutory filing window — the reason is kept on the case." : ""}</p>
      <div id="p-clock-proj"><p class="muted">Loading…</p></div></details>

      <details class="prog-sec" data-tab="overview"><summary>Program dates <span class="muted">— the dates the deadlines run from</span></summary>
      <p class="muted">Statutory clocks run <b>from</b> these dates — recording one starts or restarts its clock.
        Record the date once; every deadline that depends on it re-projects automatically.</p>
      <div id="p-dates-list"></div>
      <form id="p-date" class="inline-form">
        <label>Date <select name="key">${(cfg.clocks || []).map((cl) => `<option value="${esc(cl.basis)}">${esc(cl.label)}</option>`).join("")}</select></label>
        <label>On <input type="date" name="value" required /></label><button>Record date</button></form></details>

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
        <div class="field-grid">
        <label>Provider type <select name="provider_type" required><option value="">choose…</option>
          ${[...new Set(((cfg.eligibility || {}).thresholds || []).map((x) => x.provider_type))].map((x) => `<option value="${esc(x)}">${esc(providerTypeLabel(x))}</option>`).join("")}</select></label>
        <label>In the plan's network <select name="contracted" title="Whether the provider has a contract with the health plan — it picks a hospital's minimum amount"><option value="">not stated</option><option value="no">No</option><option value="yes">Yes</option></select></label>
        <label>Disputed amount, all claims ($) <input name="amount" type="number" step="0.01" min="0" placeholder="0.00" required /></label>
        <label>Final determination date <input name="fd" type="date" required title="Final determination date — the last EOB date, service date or appeal documents are accepted as proof. The 12-month filing window runs from it." /></label>
        <label>Date of service <input name="service_date" type="date" title="Earliest date of service in dispute${(cfg.eligibility || {}).services_after ? ` — must be after ${esc(cfg.eligibility.services_after)}` : ""}" /></label>
        ${((cfg.eligibility || {}).dispute_types || []).length ? `<label>Claim type <select name="dispute_type"><option value="">choose…</option>
          ${cfg.eligibility.dispute_types.map((x) => `<option value="${esc(x)}">${esc(x.charAt(0).toUpperCase() + x.slice(1).replace(/_/g, " "))}</option>`).join("")}</select></label>` : ""}
        </div>
        <div class="field-grid">
        <label><input type="checkbox" name="aor_valid" checked /> The AOR is valid</label>
        <label class="wide">Why the AOR is valid or not <input name="aor_reason" placeholder="e.g. signed by the provider, on file" title="Recorded with the review. If the AOR is not valid, the Attorney confirms." /></label>
        </div>
        <fieldset class="elig-flags"><legend>Tick any that apply — each one makes the dispute ineligible</legend>
          ${((cfg.eligibility || {}).ineligibility_reasons || []).map((x) => `<label><input type="checkbox" name="flag" value="${esc(x)}" /> ${esc(ELIG_REASON_LABELS[x] || x.replace(/_/g, " "))}</label>`).join("")}
        </fieldset>
        <label class="elig-notes">Notes <span class="muted">(optional — saved with this review; the PM and Attorney can add to them)</span>
          <textarea name="notes" rows="2" maxlength="4000" placeholder="Your working notes on this review"></textarea></label>
        <button>Compute eligibility</button></form><div id="p-elig-out"></div></details>

      <details class="prog-sec" id="p-mail-track" data-tab="mail" open><summary>Mail tracker <span class="muted">— every email of this dispute: where it is, who got it, what they did with its links</span></summary>
        <div id="p-mail-tracker" data-case-id="${esc(id)}"><p class="muted">Loading…</p></div></details>

      <details class="prog-sec" id="p-send-details" data-tab="mail" open><summary>Write to the parties</summary>
      <form id="p-send" class="form" data-task="send_letter" data-case-id="${esc(id)}">
        ${corrTplCache = cfg.correspondence?.templates || [], ""}
        ${(setTimeout(() => corrTemplatesFor(id, "#p-send"), 0), "")}
        <div class="field-grid">
          <label>Template <select name="template" onchange="Views.corrTemplateBody(this)">${corrTemplateOptions(cfg.correspondence?.templates || [])}</select></label>
          <label>Request for information goes to <select name="rfi_to" title="only used for the Request for Additional Information template" onchange="Views.corrFillRecipientsFn(this.form)"><option value="provider">The provider</option><option value="plan">The health plan</option></select></label>
          <label>To <input name="to" type="text" placeholder="name@example.com, …" required /></label>
          <label>Cc <input name="cc" type="text" placeholder="optional" /></label>
          <span class="muted rcpt-hint wide"></span>
          <label class="wide">Message
            <textarea name="body" rows="5" placeholder="Write the message. {share_link} puts in a secure upload link and {download_link} a link to the latest generated document; both are created when it is sent." required></textarea></label>
          <div class="wide" style="display:flex;flex-wrap:wrap;gap:4px 24px">
            <label><input type="checkbox" name="auto_share" checked /> Attach a secure upload link</label>
            <label><input type="checkbox" name="auto_download" /> Attach a link to the latest generated document</label></div>
        </div>
        <div class="actions"><button type="button" class="btn-secondary" onclick="Views.corrPreviewFn(this.form, '${esc(id)}')">Preview</button>
          <button>Send / submit for QA</button></div></form><div id="p-corr"></div></details>

      <details class="prog-sec" id="p-inv-details" data-tab="billing" open><summary>Invoices &amp; claim lines</summary>
      ${can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<form id="p-inv" class="inline-form" data-wf="invoice">
        <label>Bill to <select name="party"><option value="HEALTH_PLAN">Health plan</option><option value="PROVIDER">Provider</option></select></label>
        <label>Invoice for <select name="kind"><option value="INITIAL_FEE">Initial fee</option><option value="FULL_REVIEW">Full review</option><option value="DEFAULT">Default</option></select></label>
        <label>Amount ($) <input name="amount" type="number" step="0.01" min="0" placeholder="0.00" required /></label>
        <button>Issue invoice</button></form>` : `<p class="muted">Invoices are created by the PM.</p>`}<div id="p-inv-list" data-case-id="${esc(id)}"></div>
      <div class="claims-csv">
        <h3>Claims</h3>
        <p class="muted">Upload the dispute's claims as a CSV, one row per claim, with a header row. Only the claim number is
          required; a claim already on the case is updated from its row. Disputes of 100 claims or more need every column
          filled (Capitol Bridge large-volume policy).</p>
        <div class="inline-form">
          <label class="mini button-like">Upload claims (CSV)…<input type="file" hidden accept=".csv,text/csv" onchange="Views.claimsCsvUpload('${esc(id)}', this)" /></label>
          <button type="button" class="mini ghost" onclick="Views.claimsCsvTemplate()">Download the CSV template</button></div>
      </div><div id="p-claims-list"></div></details>

      ${Letters.caseSecHtml(id, cfg)}

      <details class="prog-sec" data-tab="mail"><summary>Secure links &amp; plan opt-out</summary>
      <div class="actions">
        <button id="p-share-up">Create upload link</button>
        <button id="p-share-dl">Create download link</button></div><div id="p-share-out"></div>
      ${can("ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? `<form id="p-optout" class="inline-form">
        <label>eligible to opt out <input type="checkbox" name="eligible" /></label>
        <input name="rationale" placeholder="rationale" required /><button>Record opt-out decision</button></form>` : ""}</details>



      ${sopCaseInfoHtml(id, c, cfg)}
      ${sopPacketFormHtml(id, c)}
      ${sopFwaHtml(id, c)}
      ${sopNotesHtml(id, c)}
      ${sopSummaryHtml(id, c, cfg)}

      <details class="prog-sec" data-tab="activity"><summary>Call log</summary>
      <form id="p-call" class="inline-form">
        <label>Direction <select name="direction"><option value="OUTBOUND">We called</option><option value="INBOUND">They called</option></select></label>
        <label>Phone number <input name="phone" type="tel" placeholder="(555) 123-4567" required /></label>
        <label style="flex:1;min-width:240px">What was discussed <input name="summary" placeholder="a brief description" required /></label><button>Log call</button></form>
      <div id="p-call-list"></div></details>

      <details class="prog-sec" data-tab="activity"><summary>Inquiries log</summary>
      <form id="p-inquiry" class="inline-form">
        <label>How <select name="method"><option value="EMAIL">Email</option><option value="PHONE_CALL">Phone call</option></select></label>
        <label>Kind <select name="inquiry_type"><option value="GENERAL_QUESTION">General question</option>
          <option value="GENERAL_INQUIRY">General inquiry</option>
          <option value="SUBMISSION_DOCUMENTS">Submission of documents</option></select></label>
        <label style="flex:1;min-width:240px">Detail <input name="detail" placeholder="optional" /></label><button>Log inquiry</button></form>
      <div id="p-inquiry-list"></div></details>`;

    afterRender(() => {
      const money = (v) => Math.round(parseFloat(v) * 100);
      wfLoad(id, cfg); wfStartPolling();
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
          if (!arr.length) { el.innerHTML = `<p class="muted">No deadline is running yet — they start when their program dates are recorded.</p>`; return; }
          // PM deadline extensions: an extended clock shows its extra days (each grant in the tooltip), and the PM
          // can extend any clock except the statutory filing window.
          const canExtend = Auth.can("PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
          const extNote = (cl) => cl.extended_days ? ` <span class="badge" title="${esc((cl.extensions || []).map((x) =>
            `+${x.days}d by ${x.granted_by} (${fmtDate(x.at)}): ${x.reason}`).join("\n"))}">+${cl.extended_days}d extended</span>` : "";
          // A step's clock stops when the step is done: met on a day, on time or late. A working target is shown, never a breach.
          const stateHtml = (cl) => cl.state === "met" ? `<span class="badge s-paid">Met ${esc(fmtDate(cl.met_on))}</span>`
            : cl.state === "met_late" ? `<span class="badge warn">Met late ${esc(fmtDate(cl.met_on))}</span>`
            : cl.state === "over_target" ? `<span class="badge warn">Over target</span>`
            : badge(cl.state || "");
          el.innerHTML = `<p><b>Timelines</b></p><table><thead><tr><th>Step</th><th>Due</th><th>State</th><th>Rule</th>${canExtend ? "<th></th>" : ""}</tr></thead><tbody>` +
            arr.map((cl) => `<tr><td>${esc(cl.label || cl.clock || "")}${cl.target ? ' <span class="chip" title="A working target, not a contract deadline">target</span>' : ""}</td>
              <td>${fmtDate(cl.due)}${extNote(cl)}</td><td>${stateHtml(cl)}</td>
              <td class="muted">${esc(cl.cite || cl.basis_note || cl.basis || "")}</td>
              ${canExtend ? `<td>${cl.extendable && !cl.met_on ? `<details class="clock-extend"><summary>Extend</summary>
                <form data-clock="${esc(cl.clock)}" class="clock-extend-form">
                  <label>Days <input name="days" type="number" min="1" max="90" value="5" required /></label>
                  <textarea name="reason" rows="2" minlength="10" maxlength="2000" required placeholder="Why more time is needed (kept on the case record, and shown to the side you ask)"></textarea>
                  <button class="mini">Extend deadline</button>
                  <span class="clock-ask"><span class="muted">or ask a side to agree first:</span>
                    <select name="side" aria-label="Who is asked"><option value="">The side that filed (the TPA)</option><option value="PROVIDER">The provider's side</option><option value="HEALTH_PLAN">The health plan</option></select>
                    <label>Answer within <input name="due_days" type="number" min="1" max="30" value="5" /> days</label>
                    <button type="button" class="mini secondary" data-ask="1">Ask for agreement</button></span></form></details>` : `<span class="muted" title="Statutory window — cannot be extended">fixed</span>`}</td>` : ""}</tr>`).join("") + `</tbody></table>`;
          // Asking for agreement: the side answers in its portal; agreeing extends the deadline by itself.
          el.querySelectorAll(".clock-extend-form [data-ask]").forEach((b) => b.addEventListener("click", async () => {
            const f = b.form, days = parseInt(f.days.value, 10), reason = f.reason.value.trim();
            if (!(days >= 1 && days <= 90)) { UI.toast("An extension is 1 to 90 days", { kind: "warn" }); return; }
            if (reason.length < 10) { UI.toast("Give the reason for the extension (at least 10 characters)", { kind: "warn" }); return; }
            await UI.run(b, async () => {
              try {
                const r = await Api.cm.requestExtension(id, f.dataset.clock, { days, reason, side: f.side.value, due_days: parseInt(f.due_days.value, 10) || 5 });
                UI.toast(`Asked ${r.asked || "the side"} for ${days} more day${days === 1 ? "" : "s"} — the deadline moves when they agree`, { duration: 7000 });
                loadDates();
              } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
            }, "Asking…");
          }));
          el.querySelectorAll(".clock-extend-form").forEach((f) => f.addEventListener("submit", async (ev) => {
            ev.preventDefault();
            const days = parseInt(f.days.value, 10), reason = f.reason.value.trim();
            if (!(days >= 1 && days <= 90)) { UI.toast("An extension is 1 to 90 days", { kind: "warn" }); return; }
            if (reason.length < 10) { UI.toast("Give the reason for the extension (at least 10 characters)", { kind: "warn" }); return; }
            await UI.run(f.querySelector("button"), async () => {
              try {
                const r = await Api.cm.extendClock(id, f.dataset.clock, days, reason);
                UI.toast(r.workflow_signalled ? `Deadline extended by ${days} day${days === 1 ? "" : "s"}`
                  : `Deadline extended by ${days} day${days === 1 ? "" : "s"} (the case's workflow wasn't reachable — its reminders keep the old date)`,
                  r.workflow_signalled ? {} : { kind: "warn" });
                loadDates();
              } catch (e) { UI.toast(e.message, { kind: "warn" }); }
            }, "Extending…");
          }));
        }).catch(() => {});
      };
      loadDates();
      const eligBadge = (v) => `${badge(v.result)}${v.borderline ? ` <span class="badge warn" title="Borderline call — PM second review required">borderline</span>` : ""}${v.override ? ` <span class="badge" title="Human override of the computed result">override</span>` : ""}`;
      // What a review decided and on what: the reason in words, and the figures it used (recorded with it).
      const ELIG_REASON = {
        over_filing_window: "Filed after the filing window", below_threshold: "Below the minimum disputed amount",
        service_before_program_start: "Service before the program's start date", unrecognized_provider_type: "Provider type not recognised",
        not_an_eligible_dispute_type: "Not an eligible type of claim dispute", aor_invalid_pending_attorney: "AOR not valid — with the Attorney",
      };
      const eligReasonHtml = (v) => {
        let ev = v.evidence || {};
        if (typeof ev === "string") { try { ev = JSON.parse(ev); } catch { ev = {}; } }
        const d = (x) => (x ? fmtDate(x + "T12:00:00") : "—");
        const usd0 = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
        const facts = [];
        if (ev.final_determination_at) facts.push(`final determination ${d(ev.final_determination_at)}${ev.determination_from ? ` (from ${esc(ev.determination_from)})` : ""}`);
        if (ev.filing_deadline) facts.push(`deadline ${d(ev.filing_deadline)}`);
        if (ev.filed_on) facts.push(`filed ${d(ev.filed_on)}`);
        if (ev.threshold_min_cents != null) facts.push(`minimum ${usd0(ev.threshold_min_cents)}${ev.disputed_amount_cents != null ? `, disputed ${usd0(ev.disputed_amount_cents)}` : ""}`);
        if (ev.service_date && ev.services_after) facts.push(`service ${d(ev.service_date)}, program starts ${d(ev.services_after)}`);
        const label = v.reason ? (ELIG_REASON[v.reason] || v.reason.replace(/_/g, " ")) : "";
        return `${label ? esc(label) : '<span class="muted">—</span>'}${v.reason ? ` <span class="mono muted" style="font-size:11px">${esc(v.reason)}</span>` : ""}${facts.length ? `<div class="muted elig-facts">${facts.join(" · ")}</div>` : ""}`;
      };
      const loadElig = () => Api.program.eligibilityHistory(id).then((r) => {
        const el = document.getElementById("p-elig-history"); if (!el) return;
        const revs = r.reviews || [];
        const canNote = Auth.can("CASE_MANAGER", "PM", "ATTORNEY", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
        const ROLE_LABEL = { CASE_MANAGER: "Reviewer", PM: "PM", ATTORNEY: "Attorney", TENANT_ADMIN: "Admin", FEDERAL_ADMIN: "Admin", PLATFORM_ADMIN: "Admin" };
        const notesRow = (v) => {
          const ns = v.notes || [];
          if (!ns.length && !canNote) return "";
          return `<tr class="elig-notes-row"><td colspan="5">
            ${ns.map((n) => `<div class="elig-note"><div class="elig-note-head"><b>${esc(n.author || "")}</b>
              <span class="muted">${esc(ROLE_LABEL[n.author_role] || n.author_role || "")} · ${fmtDate(n.created_at)}</span></div>
              <div class="elig-note-body">${esc(n.body || "")}</div></div>`).join("")}
            ${canNote ? `<details class="elig-note-add"><summary>${ns.length ? "Add a note" : "Add a note to this review"}</summary>
              <form data-review="${esc(v.id)}" class="elig-note-form"><textarea name="body" rows="2" maxlength="4000" required placeholder="Note on this review"></textarea>
              <button class="mini">Save note</button></form></details>` : ""}
          </td></tr>`;
        };
        el.innerHTML = revs.length ? `<table><thead><tr><th>Result</th><th>Reason</th><th>Rules</th><th>By</th><th>When</th></tr></thead><tbody>` +
          revs.map((v) => `<tr><td>${eligBadge(v)}</td><td>${eligReasonHtml(v)}${v.override_reason ? `<div class="muted">override: ${esc(v.override_reason)}</div>` : ""}</td>
            <td class="mono muted" title="rule version that produced this decision">${esc(v.rule_version || "—")}</td>
            <td>${esc(v.decided_by || "")}</td><td class="muted">${fmtDate(v.created_at)}</td></tr>${notesRow(v)}`).join("") +
          `</tbody></table>` : `<p class="muted">No eligibility review on record yet.</p>`;
        el.querySelectorAll(".elig-note-form").forEach((f) => f.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const body = f.body.value.trim();
          if (!body) return;
          await UI.run(f.querySelector("button"), async () => {
            try { await Api.program.eligibilityNote(id, f.dataset.review, body); UI.toast("Note saved"); loadElig(); }
            catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }, "Saving…");
        }));
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
              const missing = r.missing || [];
              const f = document.getElementById("p-elig");
              const filled = f ? eligPrefill(f, r.inputs || {}, missing) : [];
              const bad = /not a valid calendar date/.test(r.detail || "");
              out.innerHTML = `<div class="warn-box elig-need"><p><b>We need a little more to decide this one automatically.</b>
                ${bad ? "A date read from the documents isn't a real calendar date, so it needs entering by hand:" : "The case and its documents don't say:"}</p>
                <ul>${missing.map((k) => `<li>${esc((ELIG_INPUT_LABELS[k] || [])[1] || k.replace(/_/g, " "))}</li>`).join("")}</ul>
                <p>${filled.length ? `We've filled in what we found (${filled.map(esc).join(", ")}). ` : ""}Complete the highlighted ${missing.length === 1 ? "field" : "fields"} in the form below, then click <b>Compute eligibility</b>.</p></div>`;
              const first = f && f.querySelector(".need");
              if (first) { first.scrollIntoView({ behavior: "smooth", block: "center" }); first.focus({ preventScroll: true }); }
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
      // Fills the eligibility form with what auto-adjudication found and marks
      // the inputs it could not; returns the labels of the fields it filled.
      const eligPrefill = (f, inp, missing) => {
        const filled = [];
        const put = (el, v, label) => { if (el && v !== undefined && v !== null && v !== "" && !el.value) { el.value = v; filled.push(label); } };
        put(f.provider_type, inp.provider_type, "provider type");
        if (inp.contracted === true || inp.contracted === false) { f.contracted.value = inp.contracted ? "yes" : "no"; filled.push("network status"); }
        if (inp.disputed_amount_cents) put(f.amount, (inp.disputed_amount_cents / 100).toFixed(2), "disputed amount");
        put(f.fd, inp.final_determination_at, "final determination date");
        put(f.service_date, inp.service_date, "date of service");
        if (f.dispute_type) put(f.dispute_type, inp.dispute_type, "claim type");
        const field = { provider_type: f.provider_type, contracted: f.contracted, final_determination_at: f.fd, service_date: f.service_date };
        f.querySelectorAll(".need").forEach((el) => el.classList.remove("need"));
        missing.forEach((k) => { const el = field[k]; if (el) { el.classList.add("need"); el.addEventListener("change", () => el.classList.remove("need"), { once: true }); } });
        return filled;
      };
      $("#p-elig")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        const payload = () => ({
          provider_type: f.provider_type.value, contracted: f.contracted.value === "" ? null : f.contracted.value === "yes",
          disputed_amount_cents: money(f.amount.value),
          final_determination_at: f.fd.value || "",
          service_date: f.service_date.value || "",
          dispute_type: f.dispute_type ? f.dispute_type.value : "",
          flags: [...f.querySelectorAll('input[name="flag"]:checked')].map((x) => x.value),
          aor_valid: f.aor_valid.checked, aor_reason: f.aor_reason.value,
          notes: f.notes ? f.notes.value : "",
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
          // Fill-ins left in the text ("[insert amount]") are asked about before anything is sent.
          const pv = await Api.cases.corrPreview(id, corrFormBody(f)).catch(() => null);
          if (pv && (pv.unfilled || []).length && !(await UI.confirm("Some fill-ins aren't filled in", `The email still says ${pv.unfilled.map(esc).join(", ")}. Send it like this?`, "Send anyway", true))) return;
          const r = await Api.program.send(id, { template: f.template.value, body: f.body.value, to: split(f.to.value), cc: split(f.cc.value), rfi_to: f.rfi_to.value, auto_share: f.auto_share.checked, auto_download: f.auto_download.checked });
          UI.toast(r.status === "PENDING" ? "Draft submitted to QA gate" : "Sent — logged to correspondence");
          if (window.Telemetry) Telemetry.taskDone("send_letter", { template: f.template.value });
          mailTrackerBox(id);
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
      mailTrackerBox(id);
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
           <td>${v.status === "OPEN" ? `<button class="mini" onclick="Views.payInvoice('${v.id}')">Pay now</button>${canSettle ? ` ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','PAY',this)">mark paid</a>` : ""}`
             : v.status === "PAID" && canSettle ? `<a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','REFUND',this)">refund</a>`
             : v.status === "REFUND_PENDING" ? '<span class="muted">waiting on Stripe</span>' : ""}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No invoices on this case.</p>`;
      };
      // Invoices belong to the money roles; everyone else's case page leaves the list out.
      const seesInvoices = can("FINANCE", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", "TENANT_ADMIN");
      const refreshInv = () => (seesInvoices ? Api.program.invoices(id).then(renderInvoices).catch(() => {}) : Promise.resolve());
      refreshInv();
      // The claims table: lines imported by CSV and the ones document analysis read from the filing packet,
      // remittances and UB-04s (evidence_location names the document).
      Api.program.claims(id).then((r) => {
        const el = document.getElementById("p-claims-list");
        if (!el) return;
        const rows = r.claims || [];
        const usd = (x) => (x == null ? "—" : "$" + (Number(x) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 }));
        const name = (x) => [x.patient_first_name, x.patient_last_name].filter(Boolean).join(" ") || "—";
        el.innerHTML = rows.length ? `<div class="tbl-scroll"><table class="tbl"><thead><tr><th>Claim #</th><th>Patient</th><th>Type of service</th>
          <th>Date of service</th><th>Billed</th><th>Paid</th><th>In dispute</th><th>Submitted</th><th>Denied</th><th>Final determination</th>
          <th>Denial reason</th><th>From</th></tr></thead><tbody>` + rows.map((x) => `<tr><td class="mono">${esc(x.claim_number)}</td>
          <td>${esc(name(x))}</td><td>${esc(x.type_of_service || "—")}</td><td>${fmtDate(x.date_of_service)}</td>
          <td>${usd(x.billed_cents)}</td><td>${usd(x.paid_cents)}</td><td>${usd(x.disputed_cents)}</td><td>${fmtDate(x.date_claim_submitted)}</td>
          <td>${fmtDate(x.date_of_denial)}</td><td>${fmtDate(x.date_of_final_determination)}</td><td>${esc(x.denial_reason || "—")}</td>
          <td class="muted">${esc(x.evidence_location || "")}</td></tr>`).join("") + `</tbody></table></div>`
          : `<p class="muted">No claim lines yet — they fill in when the filing packet or remittances are uploaded, or import them above.</p>`;
      }).catch(() => {});
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
          UI.toast(f.kind.value === "INITIAL_FEE" ? "Invoice issued (number = case number)"
            : "Invoice issued — its PDF is being made; when it passes QA, the email sending it to the billed party is drafted for you", { duration: 9000 }); refreshInv(); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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

  // Not applied: money received that pays no fee (a check for more than its fee, a payment for a fee already
  // paid). Each amount is resolved by refunding it to the payer or applying it to another open fee (unapplied.go).
  function unappliedHtml(d, usd) {
    const items = (d && d.items) || [];
    if (!items.length) return "";
    return `<section class="panel" id="fin-unapplied"><h2>Not applied <span class="n">${usd(d.total_cents)}</span></h2>
      <p class="panel-sub">Money received that pays no fee — kept out of Collected. Refund it to the payer, or apply it to another fee they owe.</p>
      <div class="dg-wrap"><table><thead><tr><th>Received</th><th>Case</th><th>Came with</th><th class="num">Amount</th><th class="num">Left to resolve</th><th></th></tr></thead><tbody>` +
      items.map((x) => `<tr><td>${fmtDate(x.at)}</td>
        <td>${x.case_id ? `<a class="mono" href="#/cases/${esc(x.case_id)}">${esc(x.case_number || x.case_id.slice(0, 8))}</a>` : "—"}${x.invoice_no ? `<span class="cell-sub">against invoice ${esc(x.invoice_no)}</span>` : ""}</td>
        <td class="mono">${esc(x.source && x.source !== "—" ? x.source : "check")}</td>
        <td class="num">${usd(x.amount_cents)}</td><td class="num"><b>${usd(x.left_cents)}</b></td>
        <td>${d.can_resolve ? `<button class="mini" onclick="Views.resolveUnappliedFn(${x.id}, ${x.left_cents}, this)">Resolve…</button>` : '<span class="muted">Finance resolves</span>'}</td></tr>`).join("") +
      `</tbody></table></div></section>`;
  }
  async function resolveUnappliedFn(id, left, el) {
    const dollars = (c) => "$" + (c / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
    const v = await UI.modal({ title: `Resolve ${dollars(left)} not applied`, submitLabel: "Resolve",
      body: "Refund: you return the money to the payer by check or ACH, then record it here. Apply: it pays another open fee in full — enter that fee's invoice number (or the case number, when the case has one open fee).",
      fields: [
        { name: "action", label: "What happens to it", required: true, value: "refund", options: [["refund", "Refund to the payer (check / ACH)"], ["apply", "Apply to another fee"]] },
        { name: "amount", label: "Amount to refund ($)", type: "number", step: "0.01", placeholder: `all of it — ${dollars(left)}`, hint: "Refund only. Leave empty to refund all that is left." },
        { name: "reference", label: "Check / ACH reference", placeholder: "Refund only — required" },
        { name: "invoice", label: "Invoice or case number of the fee to pay", placeholder: "Apply only — e.g. FL26-021345" },
      ] });
    if (!v) return;
    const body = { action: v.action };
    if (v.action === "refund") {
      if (!String(v.reference || "").trim()) { UI.toast("Enter the check or ACH reference of the refund.", { kind: "warn" }); return; }
      body.reference = v.reference.trim();
      if (v.amount) body.amount_cents = Math.round(parseFloat(v.amount) * 100);
    } else {
      if (!String(v.invoice || "").trim()) { UI.toast("Enter the invoice or case number of the fee to pay.", { kind: "warn" }); return; }
      body.invoice = v.invoice.trim();
    }
    await UI.run(el, async () => {
      try {
        const r = await Api.program.resolveUnapplied(id, body);
        UI.toast(r.status === "refunded" ? `${dollars(r.amount_cents)} recorded as refunded` : `${dollars(r.amount_cents)} applied — that fee is paid`);
        App.rerender();
      } catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
    });
  }

  // Refunds due: cases closed "Closed-Refunded" by a person's Close and refund (no complete packet) whose
  // initial fee is still paid. Refund here does what the case page's Billing tab does (settleInvoice REFUND): a
  // Stripe refund of the fee for a card payment (in progress until Stripe confirms), otherwise recorded as refunded
  // outside the platform with the check / ACH reference.
  // Potential refunds: no complete packet 7 days after outreach. Nothing is closed or refunded until a person chooses.
  function potentialRefundsHtml(d, usd) {
    const rows = (d && d.potential) || [];
    if (!rows.length) return "";
    return `<section class="fin-sec fin-refunds"><h2 id="refunds-potential">Potential refunds <span class="badge">${rows.length} waiting for a decision</span></h2>
      <p class="fin-sec-sub">Paid filings with no complete packet 7 days after outreach. Nothing has been refunded: if the packet isn't coming, choose <b>Close and refund</b>; otherwise keep waiting — the case carries on when the packet arrives.</p>
      <div class="dg-wrap"><table><thead><tr><th>Case</th><th>Filer</th><th class="num">Fee paid</th><th>Flagged</th><th class="num">Waiting</th><th></th></tr></thead><tbody>` +
      rows.map((x) => `<tr><td class="mono"><a href="#/cases/${esc(x.case_id)}">${esc(x.case_number)}</a></td><td>${esc(x.filer || "—")}</td>
        <td class="num">${x.amount_cents ? usd(x.amount_cents) : "—"}</td><td>${fmtDate(x.flagged_at)}</td>
        <td class="num">${x.days_waiting} day${x.days_waiting === 1 ? "" : "s"}</td>
        <td>${d.can_approve ? `<button class="mini" onclick="Views.wfAnytime('${esc(x.case_id)}','REFUND_APPROVED')">Close and refund</button>` : '<span class="muted">The Reviewer or PM decides</span>'}</td></tr>`).join("") +
      `</tbody></table></div></section>`;
  }

  function refundsDueHtml(d, usd) {
    if (!d) return "";
    const rows = d.refunds || [];
    const when = (t) => (t ? fmtDate(t) : "—");
    const head = `<h2 id="refunds-due">Refunds due ${rows.length ? `<span class="badge warn">${rows.length} · ${usd(d.total_cents)}</span>` : ""}</h2>`;
    if (!rows.length) return potentialRefundsHtml(d, usd) + `<section class="fin-sec fin-refunds">${head}<p class="muted">No refunds due. A case appears here once someone chooses Close and refund on it, until its initial fee is refunded.</p></section>`;
    return potentialRefundsHtml(d, usd) + `<section class="fin-sec fin-refunds">${head}
      <p class="fin-sec-sub">Cases closed for a refund (someone chose Close and refund). The filer has been told; refund the initial fee.
        A card payment goes back to the card through Stripe; anything else you return yourself (check / ACH) and record here with its reference.</p>
      <div class="dg-wrap"><table id="refunds-due-tb" data-page-size="10"><thead><tr><th>Case</th><th>Filer</th><th>Bill to</th><th class="num">Amount</th><th>Paid</th>
        <th>Paid via</th><th>Refund ordered</th><th class="num">Waiting</th><th></th></tr></thead><tbody>` +
      rows.map((x) => `<tr><td class="mono"><a href="#/cases/${esc(x.case_id)}">${esc(x.case_number)}</a></td>
        <td>${esc(x.filer || "—")}${x.filer_email ? `<br><span class="muted">${esc(x.filer_email)}</span>` : ""}</td>
        <td>${esc(x.party === "HEALTH_PLAN" ? "Health plan" : "Provider")}</td>
        <td class="num">${usd(x.amount_cents)}</td><td>${when(x.paid_at)}</td>
        <td>${x.by_card ? '<span class="badge s-paid">card — Stripe refund</span>' : '<span class="badge">check / ACH / other</span>'}</td>
        <td>${when(x.ordered_at)}</td>
        <td class="num">${x.days_waiting >= 3 ? `<span class="badge warn">${x.days_waiting} days</span>` : `${x.days_waiting} day${x.days_waiting === 1 ? "" : "s"}`}</td>
        <td>${x.state === "IN_PROGRESS" ? '<span class="badge">Refund in progress — waiting on Stripe</span>'
          : d.can_refund ? `<button class="mini" onclick="Views.settleInvoice('${esc(x.invoice_id)}','REFUND',this,${x.by_card ? "true" : "false"})">Refund ${usd(x.amount_cents)}</button>` : '<span class="muted">Finance refunds</span>'}
          ${x.last_failure && x.state !== "IN_PROGRESS" ? `<br><span class="badge warn" title="${esc(x.last_failure)}">last attempt failed: ${esc(x.last_failure)}</span>` : ""}</td></tr>`).join("") +
      `</tbody></table></div></section>`;
  }

  // byCard: true / false when the caller knows how the fee was paid (Refunds due), undefined otherwise.
  async function settleInvoice(invId, action, el, byCard) {
    let ref = "", refundMethod = "";
    if (action === "PAY") {
      const v = await UI.modal({ title: "Mark invoice paid", submitLabel: "Mark paid",
        fields: [{ name: "ref", label: "Remittance reference (RA/ERA)", placeholder: "Optional" }] });
      if (!v) return;
      ref = v.ref;
    } else {
      // A card payment goes back to the card unless Finance chooses otherwise: after a card refund failed, the card
      // usually can't take another, and the money goes back by check or ACH instead.
      const how = byCard === true
        ? "This fee was paid by card. Back to the card: Stripe refunds this fee's amount (5–10 days to reach the payer) and it shows Refund in progress until Stripe confirms. Outside Stripe: you return the money by check or ACH and record it here."
        : byCard === false
          ? "This fee was not paid by card: return the money yourself (check / ACH), then record it here with the reference. It is recorded as refunded now."
          : "Paid by card: choose back to the card (Stripe refunds this fee's amount) or outside Stripe. Paid another way: return the money yourself (check / ACH) and enter the reference; it is recorded as refunded now.";
      const fields = [];
      if (byCard !== false) fields.push({ name: "method", label: "How is it refunded?", value: "", options: [["", "Back to the card (Stripe)"], ["outside", "Outside Stripe — check or ACH"]] });
      fields.push({ name: "ref", label: "Check / ACH reference", placeholder: "For a refund made outside Stripe" });
      const v = await UI.modal({ title: "Refund this fee", submitLabel: "Refund", danger: true, body: how, fields });
      if (!v) return;
      ref = v.ref || "";
      refundMethod = v.method || "";
    }
    await UI.run(el, async () => {
      try {
        const r = await Api.program.settleInvoice(invId, action, ref, refundMethod);
        UI.toast(action === "PAY" ? "Invoice paid" : r && r.status === "REFUND_PENDING" ? "Refund sent to Stripe — waiting for confirmation" : "Refunded");
        App.rerender();
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    });
  }

  // ---- QA gate ---------------------------------------------------------------------
  // A list of the drafts waiting on the left, the one being reviewed on the right.
  const QA_CHANNEL = { email: "Email", letter: "Letter (PDF)", note: "Rationale note" };
  const qaTpl = (a) => String(a || "").replace(/^copilot_/, "copilot · ").replace(/_/g, " ");
  function qaAge(t) {
    if (!t) return "";
    const m = Math.max(0, Math.round((Date.now() - new Date(t)) / 60000));
    return m < 60 ? `${m || 1}m` : m < 1440 ? `${Math.round(m / 60)}h` : `${Math.round(m / 1440)}d`;
  }
  const qaCase = (i) => i.case_id ? `<a class="mono" href="#/cases/${esc(i.case_id)}" onclick="event.stopPropagation()">${esc(i.case_number || i.case_id.slice(0, 8) + "…")}</a>` : "";

  async function qaQueue() {
    try {
      const r = await Api.program.qaQueue();
      const q = r.queue || [];
      const recent = r.recent || [];
      const toSend = r.to_send || [];
      const me = Auth.claims()?.sub;
      // A drafter's own view (Coder) lists only what is theirs: their drafts and those on cases they're now named on.
      const maySend = (i) => r.own_only || i.requested_by_sub === me || can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
      const list = (v) => (Array.isArray(v) ? v : []).join(", ");
      const oldest = q.length ? qaAge(q[q.length - 1].created_at) : ""; // newest first: the oldest is last
      const sum = ListUI.summary([
        { label: "Awaiting review", value: q.length, sub: q.length ? `oldest waiting ${oldest}` : "nothing waiting", tone: q.length ? "warn" : "good" },
        { label: "Ready to send", value: toSend.length, sub: "approved, back with the drafter", tone: toSend.length ? "warn" : "" },
        { label: "Recently decided", value: recent.length, sub: "approved or sent back" },
      ]);
      const sendHtml = toSend.length
        ? `<section class="panel"><h2>Passed QA — ready to send <span class="n">${toSend.length}</span></h2>
          <p class="panel-sub">Approved and back with the person who drafted them. Check the addresses and send; the send is logged and the case's dates are set.</p>
          <table class="qa-tbl"><thead><tr><th>Subject</th><th>Case</th><th>Approved by</th><th>To</th><th></th></tr></thead><tbody>` +
          toSend.map((i) => `<tr><td class="qa-tbl-main"><span class="cell-main">${esc(i.subject)}</span><span class="cell-sub">${esc(qaTpl(i.artifact))}</span></td><td data-label="Case">${qaCase(i)}</td>
            <td data-label="Approved by">${esc(i.reviewed_by || "—")}</td><td class="muted" data-label="To">${esc(list(i.to_recipients)) || "<i>none yet</i>"}</td>
            <td class="qa-tbl-act">${maySend(i) ? `<button class="mini" onclick="Views.qaSendFn('${i.id}','${esc(list(i.to_recipients))}','${esc(list(i.cc_recipients))}')">Send…</button>` : `<span class="muted">with the Reviewer</span>`}</td></tr>`).join("") +
          `</tbody></table></section>` : "";
      const items = q.length ? q.map((i) => `<button type="button" class="qa-item" data-qa="${esc(i.id)}" onclick="Views.qaReview('${esc(i.id)}')">
          <span class="qa-item-top"><span class="qa-subj">${esc(i.subject || "(no subject)")}</span><span class="qa-age" title="waiting since ${esc(fmtDate(i.created_at))}">${qaAge(i.created_at)}</span></span>
          <span class="qa-item-meta">${qaCase(i)}<span class="kc-tag">${esc(QA_CHANNEL[i.channel] || i.channel || "Email")}</span><span class="qa-tpl">${esc(qaTpl(i.artifact))}</span></span>
          <span class="qa-item-by">drafted by ${esc(i.drafted_by || "—")}${r.own_only ? " · waiting for QA" : ""}</span></button>`).join("")
        : `<p class="kc-empty">Queue empty — no drafts awaiting review.</p>`;
      const detailEmpty = q.length
        ? `<div class="qa-empty"><b>Pick a draft to review it.</b><p class="muted">You'll see exactly what the party will get — subject, recipients and text — and approve it or send it back with a note.</p></div>`
        : `<div class="qa-empty"><b>Nothing to review.</b><p class="muted">Drafts on templates that need QA stop here before anything reaches a party.</p></div>`;
      const recentHtml = recent.length
        ? `<section class="panel"><h2>Recently decided <span class="n">${recent.length}</span></h2><table class="qa-tbl"><thead><tr><th>Subject</th><th>Case</th><th>Status</th><th>Reviewed by</th><th>When</th></tr></thead><tbody>` +
          recent.map((i) => `<tr><td class="qa-tbl-main">${esc(i.subject)}</td><td data-label="Case">${qaCase(i)}</td><td data-label="Status">${badge(i.status)}</td>
            <td data-label="Reviewed by">${esc(i.reviewed_by || "—")}</td><td class="muted" data-label="When">${fmtDate(i.reviewed_at)}</td></tr>`).join("") +
          `</tbody></table></section>` : "";
      const explain = `<details class="prog-sec"><summary>What the QA gate does</summary>
        <p class="muted">Every correspondence template in this program's rules either sends <b>immediately</b>
        or carries a <b>QA role</b> (e.g. attorney, program manager). Drafts on gated templates stop here as
        <b>PENDING</b> — the named role reviews the exact subject, body, recipients, and any secure links, then
        approves (email is delivered and logged) or rejects with a note back to the drafter. Ungated templates
        never appear here. An empty queue simply means no gated draft is waiting right now.</p></details>`;
      return `<div class="wide-view qa-view"><div class="view-head"><h1>QA gate</h1>
        <span class="muted">${r.own_only ? "your drafts: waiting for QA, and ready for you to send" : "nothing reaches a party without approval on gated templates"}</span></div>
        ${sum}${sendHtml}
        <div class="qa-layout">
          <section class="panel qa-list"><h2>Awaiting review <span class="n">${q.length}</span></h2><div class="qa-items">${items}</div></section>
          <section class="panel qa-pane" id="qa-detail" aria-live="polite">${detailEmpty}</section>
        </div>
        ${recentHtml}
        <details class="prog-sec" id="qa-dq" ontoggle="if (this.open) Views.qaDraftQualityBox()"><summary>Draft quality by template <span class="muted">— last 90 days: how often each template's drafts pass QA as written</span></summary>
          <div id="qa-dq-box"><p class="muted">Loading…</p></div></details>
        ${explain}</div>`;
    } catch (e) { return err(e); }
  }

  async function qaReview(qaId) {
    const box = $("#qa-detail");
    if (!box) return;
    document.querySelectorAll(".qa-item").forEach((b) => b.classList.toggle("on", b.dataset.qa === qaId));
    box.innerHTML = `<p class="muted">Loading the draft…</p>`;
    if (window.Telemetry) Telemetry.taskStart("qa_decision");
    // On a phone the draft is under the list: go to it.
    const narrow = matchMedia("(max-width:1100px)").matches;
    if (narrow) box.scrollIntoView({ behavior: "smooth", block: "start" });
    try {
      const d = await Api.program.qaGet(qaId);
      const to = (d.to_recipients || []).join(", ");
      const cc = (d.cc_recipients || []).join(", ");
      const isNote = d.channel === "note";
      const isLetter = d.channel === "letter"; // a generated PDF: approved as a document, emailed later with its email
      const isCopilot = (d.artifact || "").startsWith("copilot_");
      box.dataset.channel = d.channel || "email";
      // Copilot drafts are accept/EDIT/reject: the reviewer rewrites in place
      // and the edited text is what gets approved (server records the edit).
      const bodyHtml = isCopilot
        ? `<textarea id="qa-edit" rows="16" class="qa-editor">${esc(d.body)}</textarea>
           <p class="muted">Copilot draft — edit freely; the approved text is what gets ${isNote ? "filed" : "sent"}, and the edit is recorded.</p>`
        : `<pre class="qa-body">${esc(d.body)}</pre>`;
      // Recipients are editable here, not just displayed: a case with no party account linked resolves to no
      // recipients at draft time, and approving it then needs an address supplied here.
      const recipientsHtml = isNote || isLetter ? "" : `
        <div class="qa-rcpt">
          <label>To <input id="qa-to" value="${esc(to)}" placeholder="recipient emails, comma-separated" /></label>
          <label>Cc <input id="qa-cc" value="${esc(cc)}" placeholder="cc emails, comma-separated" /></label>
          ${!to && !cc ? `<p class="due-over">No recipients on file — add at least one before approving.</p>` : ""}
        </div>`;
      const what = isNote ? "A determination rationale — approving files it to the case timeline. Nothing is emailed."
        : isLetter ? "A generated letter — approving clears it to go out with the email that carries it. Nothing is emailed now."
        : "An email — approving delivers it to the recipients now and logs it.";
      box.innerHTML = `<div class="qa-head">
          <div><h3>${esc(d.subject)}</h3>
            <p class="qa-head-meta"><span class="kc-tag">${esc(QA_CHANNEL[d.channel] || d.channel || "Email")}</span>
              <span class="qa-tpl">${esc(qaTpl(d.artifact))}</span>${isCopilot ? ' <span class="badge s-review">COPILOT DRAFT</span>' : ""}
              ${d.case_id ? ` · <a class="mono" href="#/cases/${esc(d.case_id)}">open the case</a>` : ""} · drafted by ${esc(d.drafted_by || "—")}, ${esc(fmtDate(d.created_at))}</p></div>
          <div class="qa-actions">${qaActionsHtml(d, qaId, isNote, isLetter)}</div>
        </div>
        <p class="panel-sub">${what}</p>
        ${recipientsHtml}
        ${qaUnfilledHtml(d)}
        ${isLetter && (d.documents || []).length ? `<div class="qa-letter" id="qa-letter-view"><p class="muted">Loading the letter as it will print…</p></div>` : ""}
        <div class="qa-sheet">${bodyHtml}</div>
        ${!isLetter && (d.documents || []).length ? `<div class="qa-carries"><b>This email's link carries</b>
          <ul class="sent-docs">${d.documents.map((x) => `<li><button type="button" class="linkish" data-name="${esc(x.filename)}" onclick="Views.previewCaseDoc('${esc(d.case_id)}','${x.doc_id}', this.dataset.name)">${esc(x.filename || "document")}</button></li>`).join("")}</ul></div>` : ""}`;
      // A letter is reviewed as the party will get it: its PDF, in place.
      if (isLetter && (d.documents || []).length) {
        const doc = d.documents[0], el = document.getElementById("qa-letter-view");
        Api.fetchBlob(Api.cases.downloadUrl(d.case_id, doc.doc_id)).then(({ blob }) => {
          if (!el || !el.isConnected) return;
          const pdf = /\.pdf$/i.test(doc.filename || "") || blob.type === "application/pdf";
          if (!pdf) { el.innerHTML = `<p><button type="button" class="mini" data-name="${esc(doc.filename)}" onclick="Views.previewCaseDoc('${esc(d.case_id)}','${doc.doc_id}', this.dataset.name)">Open ${esc(doc.filename)}</button></p>`; return; }
          const url = URL.createObjectURL(new Blob([blob], { type: "application/pdf" }));
          el.innerHTML = `<iframe class="doc-view" src="${url}" title="${esc(doc.filename)}"></iframe>
            <p class="doc-view-acts"><a class="button mini" href="${url}" download="${esc(doc.filename)}">Download</a></p>`;
        }).catch((e2) => { if (el) el.innerHTML = `<p class="error">${esc(e2.message)}</p>`; });
      }
    } catch (e) { box.innerHTML = err(e); UI.toast(e.message, { kind: "warn" }); }
  }
  // Fill-ins the case couldn't answer, still in the text ("[insert amount]"): fix them before it goes out.
  const UNFILLED_RE = /\[[A-Za-z][^\[\]\n]{1,60}\]/g;
  function unfilledOf(...texts) { return [...new Set(texts.join("\n").match(UNFILLED_RE) || [])].filter((x) => !/^\[secure /i.test(x)); }
  function qaUnfilledHtml(d) {
    const left = unfilledOf(d.subject || "", d.body || "");
    return left.length ? `<p class="due-over">Still to fill in before this goes out: ${left.map((x) => `<code>${esc(x)}</code>`).join(" ")}</p>` : "";
  }

  // What a draft's detail offers follows its state: only a pending draft can be approved or sent back. One that has
  // passed QA waits to be sent (by its drafter or a Reviewer); a decided one just says what happened. Offering
  // Approve on an approved draft sent a second approval the server then refused.
  function qaActionsHtml(d, qaId, isNote, isLetter) {
    if (d.status === "PENDING")
      return `<button class="danger" onclick="Views.qaDecide('${qaId}','REJECT',this)">Send back</button>
        <button onclick="Views.qaDecide('${qaId}','APPROVE',this)">${isNote ? "Approve & file" : isLetter ? "Approve the letter" : "Approve & send"}</button>`;
    const by = d.reviewed_by ? ` by ${esc(d.reviewed_by)}` : "", when = d.reviewed_at ? `, ${esc(fmtDate(d.reviewed_at))}` : "";
    if (d.status === "APPROVED" && !d.sent_at && d.channel === "email") {
      const maySend = d.requested_by_sub === Auth.claims()?.sub || can("CASE_MANAGER", "CODER", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
      const list = (v) => (Array.isArray(v) ? v : []).join(", ");
      return `<span class="badge s-paid">Passed QA${by}${when}</span>
        ${maySend ? `<button onclick="Views.qaSendFn('${qaId}','${esc(list(d.to_recipients))}','${esc(list(d.cc_recipients))}')">Send it</button>` : '<span class="muted">waiting for the drafter or a Reviewer to send it</span>'}`;
    }
    const label = { APPROVED: d.channel === "email" ? "Approved" : isNote ? "Approved and filed" : "Letter approved", SENT: "Sent", REJECTED: "Sent back" }[d.status] || d.status;
    return `<span class="badge">${esc(label)}${by}${when}</span>${d.status === "REJECTED" && d.review_note ? ` <span class="muted">— ${esc(d.review_note)}</span>` : ""}`;
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
    const isLetter = $("#qa-detail")?.dataset.channel === "letter";
    const edited = $("#qa-edit") ? $("#qa-edit").value : "";
    const splitEmails = (v) => (v || "").split(",").map((s) => s.trim()).filter(Boolean);
    const to = isNote || isLetter ? undefined : splitEmails($("#qa-to")?.value);
    const cc = isNote || isLetter ? undefined : splitEmails($("#qa-cc")?.value);
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject draft", danger: true, submitLabel: "Reject",
        fields: [{ name: "note", label: "Rejection note", type: "textarea", required: true,
          hint: "Returned to the drafter with the draft." }] });
      if (!v) return;
      note = v.note;
    } else if (isNote) {
      if (!(await UI.confirm("Approve and file?", "The rationale is recorded on the case timeline. Nothing is emailed.", "Approve & file"))) return;
    } else if (isLetter) {
      if (!(await UI.confirm("Approve this letter?", "The letter is cleared to go out with the email that carries it. Nothing is emailed now.", "Approve the letter"))) return;
    } else if (!to.length && !cc.length) {
      UI.toast("Add at least one recipient before approving — this draft has none on file.", { kind: "warn" });
      return;
    } else if (!(await UI.confirm("Approve and send?", "The email is delivered to all recipients now and logged to correspondence.", "Approve & send"))) return;
    await UI.run(el, async () => {
      try {
        const r = await Api.program.qaDecision(qaId, decision, note, edited, to, cc);
        if (window.Telemetry) Telemetry.taskDone("qa_decision", { decision });
        if (r.unchanged) {
          UI.toast(`Already approved — the draft is ${r.next}. Nothing changed.`, { kind: "info", duration: 7000 });
        } else if (r.letter) {
          UI.toast("Letter approved — ready to go out with its email", { duration: 6000 });
        } else if (r.next && r.status === "APPROVED" && !r.first_check) {
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
      intakeSegs();
      intakeBox(intakeStatus);
      $("#intake-form")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        await newIntake(ev.target);
      });
      // The filing packet, when the request came by phone or email with the packet: a PDF the filer sent fills
      // it in, and the requester fields above it fill from the packet's provider (or plan) contact.
      packetInit("intake-packet", {});
      document.getElementById("intake-packet")?.addEventListener("packetfilled", (ev) => {
        const f = document.getElementById("intake-form"), d = ev.detail.details || {};
        const set = (name, v) => { if (f && f.elements[name] && !f.elements[name].value && v) f.elements[name].value = v; };
        set("email", d.provider_email); set("contact_name", d.provider_contact);
        set("org", d.filing_party_type === "HEALTH_PLAN" ? d.health_plan : (d.provider_org || d.provider_legal_entity));
        if (f) packetToShortForm(f, d, d.filing_party_type === "HEALTH_PLAN");
      });
      document.getElementById("intake-packet")?.addEventListener("input", () => {
        const f = document.getElementById("intake-form"), h = document.getElementById("intake-packet");
        if (f && h) { const d = PacketForm.read(h).packet.details; packetToShortForm(f, d, d.filing_party_type === "HEALTH_PLAN"); }
      });
      // Bulk intake (CSV): parse on file pick, submit posts the idempotent
      // batch and renders the per-row receipt.
      window._intakeBulk = { items: [] };
      $("#intake-bulk-file")?.addEventListener("change", (ev) => bulkIntakeFile(ev.target));
      $("#intake-bulk-btn")?.addEventListener("click", (ev) => { ev.preventDefault(); bulkIntakeSubmit(ev.target); });
    });
    return `<div class="view-head"><h1>Pre-case intake</h1><span class="muted">every request to open a dispute, newest first — legacy tracker rows and real cases opened directly both land here</span></div>` +
      (canCreate ? `<details class="panel" id="intake-new"><summary>New intake request — one filer, or a CSV batch</summary>
      <details class="card" style="margin-bottom:12px"><summary><b>Bulk intake (CSV)</b> — third-party batch filing; idempotent by batch reference, up to 500 rows</summary>
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
        <input name="other_party_name" placeholder="health plan the dispute is against" required />
        <input name="other_party_email" type="email" data-email-check placeholder="health plan's email for notices" required />
        <button>New intake request</button></form>
      ${packetBlockHtml("intake-packet", "<b>Filing packet</b> — took the request by phone or email? Fill in the packet's sections here, or pick the filled PDF the filer sent")}
      <div id="intake-packet-out"></div>
      <p class="muted">Filing party defaults to Provider. Everything else — amount, service line,
        eligibility inputs — comes from the claim packet the filing party uploads via the link emailed to them,
        not manual entry here. Need to fill this out by talking it through instead? Use the <a href="#/assistant">Assistant</a>.</p></details>` : "") +
      `<div class="list-bar">${SrvSearch.html("intake", "Search email, contact, organization, case #…")}<div id="intake-segs"></div></div>` +
      `<div id="intake-box"><p class="muted">Loading…</p></div>`;
  }
  SrvSearch.on("intake", () => { intakeSegs(); intakeBox(intakeStatus); });
  let intakeStatus = "";
  // The status switch, each status with its number of requests (the search applies).
  async function intakeSegs() {
    const box = document.getElementById("intake-segs");
    if (!box) return;
    const sts = ["", ...INTAKE_STATUSES];
    const counts = await Promise.all(sts.map((st) => Api.program.intake({ limit: 1, status: st, ...SrvSearch.params("intake") }).then((r) => r.total ?? null).catch(() => null)));
    const label = (st) => st ? st.replace(/_/g, " ").toLowerCase().replace(/^./, (c) => c.toUpperCase()) : "All";
    box.innerHTML = ListUI.segs("Intake status", sts.map((st, i) => ({ value: st, label: label(st), count: counts[i] })), intakeStatus, "Views.intakeFilter");
  }

  async function intakeBox(status) {
    const box = document.getElementById("intake-box");
    if (!box) return;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const [r, prog] = await Promise.all([Api.program.intake({ limit: Pager.size(), status, ...SrvSearch.params("intake") }), Api.program.get().catch(() => null)]);
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
          `</tbody></table>` : `<p class="muted">${SrvSearch.q("intake") ? `No intake request matches “${esc(SrvSearch.q("intake"))}”` : `No intake requests${status ? ` with status ${status}` : ""}`}.</p>`) +
        (rows.length ? `<div id="intake-pg"></div>` : "");
      Pager.server(document.getElementById("intake-pg"), { total: intakeTotal, shown: rows.length }, async (limit, offset) => {
        const pr = await Api.program.intake({ limit, offset, status, ...SrvSearch.params("intake") });
        const prows = pr.intake || [];
        document.querySelector("#intake-tb tbody").innerHTML = prows.map(intakeRowHtml).join("");
        return { total: pr.total ?? intakeTotal, shown: prows.length };
      });
    } catch (e) { box.innerHTML = err(e); }
  }
  function intakeFilter(status) { intakeStatus = status; intakeSegs(); intakeBox(status); }

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
      // The packet's own answers, when staff filled in the filing packet form below the intake fields.
      const packet = packetFrom("intake-packet", document.getElementById("intake-packet-out"));
      if (packet === false) return;
      const r = await Api.program.createIntake({
        email: form.email.value, contact_name: form.contact_name.value, org: form.org.value,
        other_party_name: form.other_party_name.value.trim(), other_party_email: form.other_party_email.value.trim(),
        ...(packet ? { packet, filing_party_type: packet.details.filing_party_type || "PROVIDER",
          disputed_amount_cents: packet.disputed_amount_cents || 0 } : {}),
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
    } catch (e) { UI.toast(packetErr(e), { kind: "warn", duration: 12000 }); }
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
      const [fin, pays, chks, due, chkReview] = await Promise.all([
        Api.program.financial(),
        Api.program.payments(null, { limit: FIN_PAGE, ...SrvSearch.params("fin-pay") }).catch(() => ({ payments: [] })),
        Api.program.checks(finCkStatus, { limit: FIN_PAGE, ...SrvSearch.params("fin-ck") }).catch(() => ({ checks: [] })),
        Api.program.refundsDue().catch(() => null),
        Api.program.checks("REVIEW", { limit: 1 }).catch(() => null), // only for the count in Needs attention
      ]);
      const unap = Number(fin.kpi?.unapplied_cents) > 0 ? await Api.program.unapplied().catch(() => null) : null;
      // first page of each server-paged list
      finPager = {
        checks: { rows: chks.checks || [], next: chks.next_offset ?? -1, total: chks.total ?? (chks.checks || []).length },
        payments: { rows: pays.payments || [], next: pays.next_offset ?? -1, total: pays.total ?? (pays.payments || []).length },
      };
      const k = fin.kpi || {};
      const usd = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
      const num = (n) => (Number(n) || 0).toLocaleString();
      const partyName = (pt) => ({ PROVIDER: "Provider", HEALTH_PLAN: "Health plan" })[pt] || (pt || "—");
      const rec = fin.receivables || [];
      const aging = fin.aging || [];
      const sumOf = (rows, f) => rows.reduce((t, x) => t + (f(x) ? Number(x.total_cents) || 0 : 0), 0);
      const cntOf = (rows, f) => rows.reduce((t, x) => t + (f(x) ? Number(x.n) || 0 : 0), 0);
      const openCents = sumOf(rec, (x) => x.status === "OPEN"), openN = cntOf(rec, (x) => x.status === "OPEN");
      const overdueCents = sumOf(aging, (x) => x.bucket !== "current"), overdueN = cntOf(aging, (x) => x.bucket !== "current");
      const dueRows = (due && due.refunds) || [];
      const reviewN = chkReview && chkReview.total != null ? chkReview.total : (chkReview?.checks || []).length;

      let html = `<div class="view-head"><h1>Financials</h1>
        <span class="muted">Every payment, refund and receivable for ${esc(Api.getTenant()).toUpperCase()}</span>
        ${fin.stripe_enabled ? '<span class="badge s-paid">Stripe connected</span>' : '<span class="badge">Stripe not configured</span>'}</div>
        <div class="fin-kpis">
          <div class="fin-kpi"><span class="fin-kpi-l">Collected</span><span class="fin-kpi-n">${usd(k.collected_cents)}</span><span class="fin-kpi-s">${num(k.payments_count)} payments, all time</span></div>
          <div class="fin-kpi"><span class="fin-kpi-l">Last 30 days</span><span class="fin-kpi-n">${usd(k.collected_30d_cents)}</span><span class="fin-kpi-s">collected</span></div>
          <div class="fin-kpi"><span class="fin-kpi-l">Outstanding</span><span class="fin-kpi-n">${usd(openCents)}</span><span class="fin-kpi-s">${num(openN)} open invoice${openN === 1 ? "" : "s"}</span></div>
          <div class="fin-kpi ${overdueCents > 0 ? "is-warn" : ""}"><span class="fin-kpi-l">Overdue</span><span class="fin-kpi-n">${usd(overdueCents)}</span><span class="fin-kpi-s">${overdueN ? `${num(overdueN)} past due` : "nothing past due"}</span></div>
          <div class="fin-kpi"><span class="fin-kpi-l">Refunded</span><span class="fin-kpi-n">${usd(k.refunded_cents)}</span><span class="fin-kpi-s">all time</span></div>
          ${Number(k.unapplied_cents) > 0 ? `<div class="fin-kpi is-warn"><span class="fin-kpi-l" data-tip="Money received that pays no fee: a check for more than its fee, or a payment for a fee already paid. Refund it to the payer or apply it to another fee. It is not in Collected.">Not applied</span><span class="fin-kpi-n">${usd(k.unapplied_cents)}</span><span class="fin-kpi-s">received, pays no fee</span></div>` : ""}
        </div>`;

      // What needs a person today, each linking to its section.
      const attn = [];
      if (dueRows.length) attn.push(`<a class="fin-attn-i is-warn" href="javascript:void(0)" onclick="document.getElementById('refunds-due').scrollIntoView({behavior:'smooth'})"><b>${dueRows.length}</b> refund${dueRows.length === 1 ? "" : "s"} to make · ${usd(due.total_cents)}</a>`);
      if (reviewN) attn.push(`<a class="fin-attn-i is-warn" href="javascript:void(0)" onclick="Views.finChecksFilter('REVIEW')"><b>${num(reviewN)}</b> check${reviewN === 1 ? "" : "s"} to review</a>`);
      if (Number(k.unapplied_cents) > 0) attn.push(`<a class="fin-attn-i is-warn" href="javascript:void(0)" onclick="document.getElementById('fin-unapplied')?.scrollIntoView({behavior:'smooth'})"><b>${usd(k.unapplied_cents)}</b> received but not applied to a fee — refund or apply</a>`);
      if (overdueN) attn.push(`<a class="fin-attn-i is-bad" href="javascript:void(0)" onclick="document.getElementById('fin-aging').scrollIntoView({behavior:'smooth'})"><b>${num(overdueN)}</b> overdue invoice${overdueN === 1 ? "" : "s"} · ${usd(overdueCents)}</a>`);
      html += `<div class="fin-attn" role="status"><span class="fin-attn-l">Needs attention</span>${attn.length ? attn.join("") : '<span class="muted">Nothing waiting — no refunds due, no checks to review, nothing overdue.</span>'}</div>`;

      // Receivables (status × party) and aging, side by side.
      const parties = [...new Set(rec.map((x) => x.party))].sort();
      // Collected = card + scanned checks + fees recorded by hand: the rails below add up to it.
      const statuses = ["OPEN", "PAID", "REFUND_PENDING", "REFUNDED", "VOID"].filter((st) => rec.some((x) => x.status === st))
        .concat([...new Set(rec.map((x) => x.status))].filter((st) => !["OPEN", "PAID", "REFUND_PENDING", "REFUNDED", "VOID"].includes(st)));
      const cell = (rows) => rows.length ? `<span class="fin-amt">${usd(sumOf(rows, () => true))}</span><span class="fin-cnt">${num(cntOf(rows, () => true))}</span>` : '<span class="muted">—</span>';
      const recHtml = rec.length ? `<table class="fin-matrix"><thead><tr><th>Status</th>${parties.map((pt) => `<th class="num">${esc(partyName(pt))}</th>`).join("")}<th class="num">Total</th></tr></thead><tbody>` +
        statuses.map((st) => `<tr><td>${badge(st)}</td>${parties.map((pt) => `<td class="num">${cell(rec.filter((x) => x.status === st && x.party === pt))}</td>`).join("")}
          <td class="num">${cell(rec.filter((x) => x.status === st))}</td></tr>`).join("") + `</tbody></table>`
        : '<p class="muted">No invoices yet.</p>';
      const buckets = [["current", "Current", "ok"], ["1-30", "1–30 days", "warn"], ["31-60", "31–60 days", "warn2"], ["60+", "60+ days", "bad"]];
      const agingTotal = sumOf(aging, () => true);
      const agingHtml = agingTotal > 0 ? `<div class="aging-bar" role="img" aria-label="Open invoices by age">${buckets.map(([b, , cls]) => {
          const c = sumOf(aging, (x) => x.bucket === b);
          return c > 0 ? `<span class="aging-seg ${cls}" style="flex:${c}" title="${esc(b)}: ${usd(c)}"></span>` : "";
        }).join("")}</div>
        <ul class="aging-list">${buckets.map(([b, label, cls]) => {
          const row = aging.find((x) => x.bucket === b);
          return `<li><span class="aging-dot ${cls}"></span><span>${label}${b === "current" ? "" : " past due"}</span>
            <span class="fin-amt">${row ? usd(row.total_cents) : "$0.00"}</span><span class="fin-cnt">${row ? num(row.n) : 0}</span></li>`;
        }).join("")}</ul>` : '<p class="muted">No open invoices.</p>';
      html += `<div class="fin-grid">
        <section class="fin-sec"><h2>Receivables</h2><p class="fin-sec-sub">Invoices by status and who is billed: amount, then count.</p>${recHtml}</section>
        <section class="fin-sec" id="fin-aging"><h2>A/R aging</h2><p class="fin-sec-sub">Open invoices by how far past due: ${usd(agingTotal)} in all.</p>${agingHtml}</section>
      </div>`;

      // Payment rails: one row per rail, its money by outcome.
      const bm = fin.by_method || [];
      if (bm.length) {
        const rails = [...new Set(bm.map((x) => x.provider))];
        const outcome = { Settled: ["PAID", "CLEARED", "SUCCEEDED"], Pending: ["PENDING", "PROCESSING", "OPEN"],
          "Failed / expired": ["FAILED", "EXPIRED", "CANCELLED", "CANCELED", "REJECTED"], Refunded: ["REFUNDED", "REFUNDING", "REFUND_DUE"] };
        const railName = (r) => ({ stripe: "Card (Stripe)", check: "Check (scanned)", mojaloop: "Mojaloop", manual: "Recorded by hand (check / ACH / other)" })[r] || r;
        html += `<section class="fin-sec"><h2>Payment rails</h2><p class="fin-sec-sub">How money arrives, and what became of each payment: amount, then count.</p>
          <table class="fin-matrix"><thead><tr><th>Rail</th>${Object.keys(outcome).map((o) => `<th class="num">${o}</th>`).join("")}</tr></thead><tbody>` +
          rails.map((r) => `<tr><td><b>${esc(railName(r))}</b></td>${Object.values(outcome).map((sts) => `<td class="num">${cell(bm.filter((x) => x.provider === r && sts.includes(x.status)))}</td>`).join("")}</tr>`).join("") +
          `</tbody></table></section>`;
      }

      html += unappliedHtml(unap, usd);
      html += refundsDueHtml(due, usd);

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
                ${["PROCESSED", "REVIEW", "MATCHED"].includes(c.status) ? `<button class="mini" onclick="Views.correctCheck('${c.id}', this)">✎ correct</button>` : ""}
                ${c.status === "REVIEW" && c.case_id ? `<button class="mini" onclick="Views.requestRescan('${c.id}','${c.case_id}', this)">↻ request rescan</button>` : ""}
                <button class="mini secondary" onclick="Views.checkOpen('${c.id}')">Preview</button></td></tr>`;
      payRow = (p) => `<tr><td class="mono">${esc((p.case_id || "").slice(0, 8))}…</td>
            <td>${esc(p.payer_email || "—")}</td><td class="num">${usd(p.amount_cents)}</td><td>${badge(p.status)}</td>
            <td class="mono">${esc(p.payment_intent || p.session_id || "")}</td>
            <td class="muted">${fmtDate(p.created_at)}</td></tr>`;
      const ck = finPager.checks.rows;
      const reviewCk = ck.filter((c) => c.status === "REVIEW");
      // Checks live here (the old Checks page sends you to this section): deposit, filter, preview, link, clear.
      html += `<section class="fin-sec" id="fin-checks"><h2>Checks</h2>
        <p class="fin-sec-sub">Scan or photograph a check: its amounts are read and matched to a fee, and a person clears it before the money counts.</p>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.uploadCheck(event.target.check.files[0], event.target.querySelector('button'))">
          <input type="file" name="check" accept="image/jpeg,image/png,image/tiff,image/webp" required />
          <button class="mini">📷 scan / upload check</button>
          </form>
        <p class="fin-ck-filter">${["", "RECEIVED", "PROCESSED", "MATCHED", "REVIEW", "CLEARED", "REJECTED"].map((st) =>
          `<button class="mini ${st === finCkStatus ? "" : "secondary"}" aria-pressed="${st === finCkStatus}" onclick="Views.finChecksFilter('${st}')">${st ? st.charAt(0) + st.slice(1).toLowerCase() : "All"}</button>`).join(" ")}</p>` +
        (ck.length || SrvSearch.q("fin-ck") ? SrvSearch.html("fin-ck", "Search check #, payer, memo, case, invoice…") : "") +
        (ck.length ? `<div class="dg-wrap"><table data-serverpaged><thead><tr><th>Check</th><th>Payee</th><th>Date</th><th>Routing · Account</th><th>Courtesy</th><th>Legal (ICR)</th><th>Memo</th><th>Match</th><th>Status</th><th>Conf</th><th></th></tr></thead><tbody id="fin-ck-body">` +
          ck.map(checkRow).join("") + `</tbody></table></div>` +
          (reviewCk.length ? `<p class="muted">⚠ ${reviewCk.length} check(s) awaiting manual review — OCR could not match them to an open invoice.</p>` : "") :
          (SrvSearch.q("fin-ck") ? `<p class="muted">No check matches “${esc(SrvSearch.q("fin-ck"))}”.</p>`
            : finCkStatus ? `<p class="muted">No ${esc(finCkStatus.toLowerCase())} checks.</p>` : `<p class="muted">No checks received yet — upload a scan or photo to start intake.</p>`)) +
        (ck.length ? `<div id="fin-ck-pg"></div>` : "") +
        `<div class="card" id="check-detail-card" style="display:none"><h2>Scan preview</h2><div id="check-detail"></div></div></section>`;

      // Card payments
      const plist = finPager.payments.rows;
      if (!plist.length && SrvSearch.q("fin-pay"))
        html += `<section class="fin-sec"><h2>Card payments</h2>${SrvSearch.html("fin-pay", "Search payer, email, case, status, Stripe ref…")}<p class="muted">No card payment matches “${esc(SrvSearch.q("fin-pay"))}”.</p></section>`;
      if (plist.length)
        html += `<section class="fin-sec"><h2>Card payments</h2><p class="fin-sec-sub">Every card checkout, newest first.</p>${SrvSearch.html("fin-pay", "Search payer, email, case, status, Stripe ref…")}<div class="dg-wrap"><table data-serverpaged><thead><tr><th>Case</th><th>Payer</th><th class="num">Amount</th><th>Status</th><th>Stripe ref</th><th>When</th></tr></thead><tbody id="fin-pay-body">` +
          plist.map(payRow).join("") + `</tbody></table></div>` +
          `<div id="fin-pay-pg"></div></section>`;

      // Unified event stream
      const ev = fin.events || [];
      if (ev.length) {
        const evRowsHtml = (list) => list.map((e) => `<tr><td>${badge(e.kind)}</td>
            <td>${e.direction === "IN" ? '<span class="fin-in">↓ in</span>' : e.direction === "OUT" ? '<span class="fin-out">↑ out</span>' : "—"}</td>
            <td class="num">${usd(e.amount_cents)}</td><td>${esc(partyName(e.party) || "—")}</td>
            <td class="mono">${esc(e.ref || "")}</td><td>${esc(e.actor || "")}</td>
            <td class="muted">${fmtDate(e.created_at)}</td></tr>`).join("");
        const evPage = initClientPager("txn", ev, evRowsHtml);
        html += `<section class="fin-sec"><h2>Transaction stream</h2><p class="fin-sec-sub">Every money event in order: invoices, payments, refunds.</p><div class="dg-wrap"><table id="txn-tb"><thead><tr><th>Event</th><th>Direction</th><th class="num">Amount</th><th>Party</th><th>Reference</th><th>By</th><th>When</th></tr></thead>
          <tbody>${evPage.bodyHtml}</tbody></table></div>${evPage.pagerHtml}</section>`;
      } else {
        html += `<section class="fin-sec"><h2>Transaction stream</h2><p class="muted">No financial events yet — issue an invoice to start the stream.</p></section>`;
      }
      afterRender(() => {
        for (const kind of ["checks", "payments"]) {
          const st = finPager[kind];
          Pager.server(document.getElementById(kind === "checks" ? "fin-ck-pg" : "fin-pay-pg"),
            { total: st.total, shown: st.rows.length }, async (limit, offset) => {
              const r = kind === "checks"
                ? await Api.program.checks(finCkStatus, { limit, offset, ...SrvSearch.params("fin-ck") })
                : await Api.program.payments(null, { limit, offset, ...SrvSearch.params("fin-pay") });
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
  let finCkStatus = ""; // the Checks section's status filter ("" = all)
  function finChecksFilter(st) {
    finCkStatus = st;
    App.rerender();
    setTimeout(() => scrollToChecks(), 500); // after the redraw, not the page being replaced
  }
  // Financials draws after its data arrives: wait (up to ~5 s) for the Checks section, then bring it into view.
  function scrollToChecks(tries = 25) {
    const el = document.getElementById("fin-checks");
    if (el) el.scrollIntoView({ block: "start" });
    else if (tries > 0) setTimeout(() => scrollToChecks(tries - 1), 200);
  }
  SrvSearch.on("fin-ck", () => App.rerender());
  SrvSearch.on("fin-pay", () => App.rerender());
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
        ${row("Dispute against", fields.other_party_name)}${row("Their notices to", fields.other_party_email)}
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
          other_party_name: fields.other_party_name || "", other_party_email: fields.other_party_email || "",
          ...(fields.filing_party_type ? { filing_party_type: fields.filing_party_type } : {}),
          ...(fields.disputed_amount_cents ? { disputed_amount_cents: fields.disputed_amount_cents } : {}),
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
      <td><a href="javascript:void(0)" onclick="Views.previewCaseDoc('${caseId}','${d.doc_id}', this.dataset.name)" data-name="${esc(d.filename || "")}">view</a> ·
        <a href="javascript:void(0)" onclick="Views.downloadDoc('${caseId}','${d.doc_id}')">download</a></td></tr>`).join("");
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
    let tpls = await corrTemplatesFor(caseId, "");
    if (!tpls.length) {
      const prog = await Api.program.get().catch(() => null);
      tpls = prog?.config?.correspondence?.templates || [];
    }
    if (!tpls.length)
      return `<div class="card"><h3>✉ Send mail</h3><p class="muted">This program defines no correspondence templates — use the case screen's correspondence tools.</p></div>`;
    corrTplCache = tpls;
    return `<div class="card"><h3>✉ Send mail</h3>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.asstSendMail('${caseId}', this)">
        <select name="template" onchange="Views.corrTemplateBody(this)">${corrTemplateOptions(tpls)}</select>
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
        Api.admin.listStaffNames(Api.getTenant()).catch(() => []),
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
      const staff = await Api.admin.listStaffNames(Api.getTenant());
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
        const isNote = d.channel === "note", isLetter = d.channel === "letter";
        const isCopilot = (d.artifact || "").startsWith("copilot_");
        const bodyHtml = isCopilot
          ? `<textarea id="asst-qa-edit-${d.id}" rows="10" style="width:100%">${esc(d.body)}</textarea>
             <p class="muted">Copilot draft — edit freely; the approved text is what gets ${isNote ? "filed" : "sent"}, and the edit is recorded.</p>`
          : `<pre class="qa-body">${esc(d.body)}</pre>`;
        return `<div class="card asst-qa-card" data-qa="${d.id}" data-channel="${esc(d.channel || "email")}">
          <h3>⛨ Gate: ${esc(d.subject)}</h3>
          <p class="muted">${esc(d.drafted_by)} · ${isNote ? "determination rationale · files to timeline" : isLetter ? "generated letter · goes out with its email" : `to: ${esc((d.to_recipients || []).join(", "))}`}
            ${isCopilot ? ' · <span class="badge s-review">COPILOT DRAFT</span>' : ""}</p>
          ${bodyHtml}
          <div class="actions">
            <button onclick="Views.asstQaDecide('${caseId}','${d.id}','APPROVE',this)">${isNote ? "Approve & file" : isLetter ? "Approve the letter" : "Approve & send"}</button>
            <button class="danger" onclick="Views.asstQaDecide('${caseId}','${d.id}','REJECT',this)">Reject</button></div></div>`;
      }));
      box.innerHTML = `<h3 style="margin:10px 0 6px">${q.length} item(s) at the QA gate for this case</h3>` + cards.join("");
    } catch (e) { box.innerHTML = ""; }
  }

  async function asstQaDecide(caseId, qaId, decision, btn) {
    const card = document.querySelector(`.asst-qa-card[data-qa="${qaId}"]`);
    const isNote = card?.dataset.channel === "note", isLetter = card?.dataset.channel === "letter";
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
    } else if (isLetter) {
      if (!(await UI.confirm("Approve this letter?", "The letter is cleared to go out with the email that carries it. Nothing is emailed now.", "Approve the letter"))) return;
    } else if (!(await UI.confirm("Approve and send?", "The email is delivered to all recipients now and logged to correspondence.", "Approve & send"))) return;
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.qaDecision(qaId, decision, note, edited);
        card?.remove();
        asstAppend(caseId, "assistant",
          decision === "APPROVE"
            ? (r.email_delivery_error ? `⚠ Approved but email failed: ${r.email_delivery_error}`
              : isNote ? "Rationale approved and filed to the case timeline." : isLetter ? "Letter approved — ready to go out with its email."
              : "Draft approved and sent — logged to correspondence.")
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
  // The templates a letter on this case can be drafted from, with the wording
  // the "When to use" rules choose for it. Fills the compose form's list (and
  // the cache its pre-fill reads) once the case's own list arrives.
  async function corrTemplatesFor(caseId, formSel) {
    // Only the roles that write letters have templates to choose from.
    if (!can("CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) return [];
    try {
      const r = await Api.program.caseTemplates(caseId);
      const tpls = r.templates || [];
      if (!tpls.length) return tpls;
      corrTplCache = tpls;
      const sel = formSel && document.querySelector(formSel + " select[name=template]");
      if (sel) {
        const cur = sel.value;
        sel.innerHTML = corrTemplateOptions(tpls);
        if (tpls.some((t) => t.key === cur)) sel.value = cur;
        corrFillRecipients(sel.form);
      }
      return tpls;
    } catch { return []; }
  }
  function corrTemplateOptions(tpls) {
    return tpls.map((tp) => `<option value="${esc(tp.key)}">${esc(tplName(tp))}${tp.custom ? " (added)" : ""}${tp.qa_role ? " (QA: " + esc(tp.qa_role) + ")" : ""}</option>`).join("");
  }

  function corrTemplateBody(sel) {
    const form = sel.form;
    const tp = corrTplCache.find((t) => t.key === sel.value);
    if (!tp || !tp.body || !form || !form.body) return;
    const ta = form.body;
    const untouched = corrTplCache.some((t) => t.body && t.body === ta.value);
    if (!ta.value.trim() || untouched) ta.value = tp.body;
    corrFillRecipients(form);
  }
  // To and Cc from the dispute itself (the template's parties, as the send resolves them: CRM contacts, else the
  // filing -- the TPA or filer, the health plan's email for notices). Only fields nobody typed in are changed.
  async function corrFillRecipients(form) {
    const caseId = form && form.dataset.caseId;
    if (!caseId || !form.to || !form.template || !form.template.value) return;
    const mine = (el) => el && el.value.trim() && el.dataset.auto !== el.value;
    if (mine(form.to) && mine(form.cc)) return;
    let pv;
    try { pv = await Api.cases.corrPreview(caseId, { template: form.template.value, body: "", to: [], cc: [], rfi_to: form.rfi_to ? form.rfi_to.value : "" }); }
    catch { return; }
    for (const [el, list] of [[form.to, pv.to], [form.cc, pv.cc]]) {
      if (!el || mine(el)) continue;
      el.value = (list || []).join(", ");
      el.dataset.auto = el.value;
    }
    const hint = form.querySelector(".rcpt-hint");
    if (hint) hint.textContent = (pv.to || []).length ? "Filled in from the dispute's parties — change them if needed." : "No address on file for this template's recipients — type one.";
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
  // A Nurse / Physician logs each entry as a Nurse or as a Physician: the two are billed at different rates, so
  // the combined role is never an entry's role (the server refuses it) and there is no "primary" to fall back on.
  const TIME_ROLE_WORDS = { DOCTOR: "Physician", NURSE: "Nurse", CODER: "Coder", ARBITRATOR: "Arbitrator", ATTORNEY: "Attorney",
    CASE_MANAGER: "Reviewer", PM: "PM", FINANCE: "Finance", STATE_AUDITOR: "State auditor", FEDERAL_ADMIN: "Federal admin", PLATFORM_ADMIN: "Platform admin" };
  const timeRoleSelect = () => {
    const held = (Auth.claims()?.roles || []).filter((r) => TIME_ENTRY_ROLES.includes(r));
    const combined = held.includes("NURSE_PHYSICIAN");
    const roles = [...new Set(held.flatMap((r) => (r === "NURSE_PHYSICIAN" ? ["NURSE", "DOCTOR"] : [r])))];
    if (roles.length < 2) return "";
    return `<select name="role" aria-label="Role performed" title="Role being performed for this entry" ${combined ? "required" : ""}>
      <option value="">${combined ? "role: Nurse or Physician…" : "role: primary"}</option>
      ${roles.map((r) => `<option value="${esc(r)}">${esc(TIME_ROLE_WORDS[r] || r)}</option>`).join("")}</select>`;
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

  // ---- Templates: the program's emails, and which one is used when ----------
  // Two tabs. "Templates" lists every email template; the PM and admins can
  // change the wording and recipients, add their own and retire those. "When
  // to use" has one row per point in the process and says which template is
  // drafted there -- for every case, or differently for some. Whatever is
  // chosen, the step keeps its own approver: the wording changes, the QA gate
  // does not.
  const TPL_ROLE_LABELS = { requester: "The person who asked to file", filing_party: "Filing party", provider: "Provider",
    health_plan: "Health plan", provider_or_plan: "Provider or plan (chosen when sent)", billed_party: "Whoever is billed",
    all_parties: "Both parties", agency: "The Agency (AHCA)", pm: "Program Manager" };
  const tplRole = (r) => TPL_ROLE_LABELS[r] || r;
  const tplName = (t) => t.name || t.key.replace(/_/g, " ").replace(/^./, (c) => c.toUpperCase()).replace(/^Rfi$/, "RFI");
  let tplData = null, tplTab = "templates";

  async function templatesPage() {
    afterRender(tplLoad);
    return `<div class="view-head"><h1>Templates</h1>
      <span class="muted">the emails this program sends, and which one is used when</span></div>
      ${Letters.variablesCardHtml()}
      ${Letters.settingsCardHtml()}
      <div id="tpl-root"><p class="muted">Loading…</p></div>`;
  }

  async function tplLoad() {
    const root = document.getElementById("tpl-root");
    if (!root) return;
    try { tplData = await Api.program.templates(); }
    catch (e) { root.innerHTML = `<p class="error">${esc(e.message)}</p>`; return; }
    tplRender();
  }

  function tplRender() {
    const root = document.getElementById("tpl-root");
    if (!root || !tplData) return;
    const d = tplData, edit = d.can_edit;
    const approver = (t) => t.qa_role ? (t.qa_pre_role ? `${t.qa_pre_role} checks, then ${t.qa_role}` : t.qa_role) : "Sent without approval";
    const status = (t) => t.inactive ? `<span class="badge">Retired</span>` : t.custom ? `<span class="badge s-info">Added</span>`
      : t.edited ? `<span class="badge warn">Edited</span>` : `<span class="badge s-paid">Standard</span>`;
    const rows = d.templates.map((t) => `<tr class="${t.inactive ? "tpl-retired" : ""}">
      <td><button type="button" class="tt-link" onclick="Views.tplEdit('${esc(t.key)}')">${esc(tplName(t))}</button>
        <div class="muted mono">${esc(t.key)}</div></td>
      <td>${t.custom ? (t.used_by.length ? `Used for: ${esc(t.used_by.map((e) => (d.events.find((x) => x.event === e) || {}).label || e).join("; "))}` : `<span class="muted">Picked by hand</span>`) : esc(t.when || "")}</td>
      <td>${esc((t.to || []).map(tplRole).join(", "))}</td>
      <td>${esc(approver(t))}</td><td>${status(t)}</td>
      <td class="muted">${t.updated_by ? `${esc(t.updated_by)} · ${fmtDate(t.updated_at)}` : "—"}</td></tr>`).join("");
    const active = d.templates.filter((t) => !t.inactive);
    const options = (sel) => active.map((t) => `<option value="${esc(t.key)}" ${t.key === sel ? "selected" : ""}>${esc(tplName(t))}${t.custom ? " (added)" : ""}</option>`).join("");
    const cond = (u) => [u.filing_party_type && (u.filing_party_type === "HEALTH_PLAN" ? "filed by a health plan" : "filed by a provider"),
      u.line_of_business && `line of business ${u.line_of_business}`].filter(Boolean).join(" and ");
    const eventRows = d.events.map((ev) => {
      const def = ev.rules.find((u) => !u.filing_party_type && !u.line_of_business);
      const exceptions = ev.rules.filter((u) => u.filing_party_type || u.line_of_business);
      const nameOf = (k) => { const t = d.templates.find((x) => x.key === k); return t ? tplName(t) : k; };
      return `<tr><td><b>${esc(ev.label)}</b><div class="muted mono">${esc(ev.event)}</div></td>
        <td>${edit ? `<select onchange="Views.tplUsageDefault('${esc(ev.event)}', this.value)" aria-label="Template for ${esc(ev.label)}">${options(def ? def.template : ev.event)}</select>`
                   : esc(nameOf(def ? def.template : ev.event))}</td>
        <td>${exceptions.map((u) => `<div class="tpl-exc">When ${esc(cond(u))}: <b>${esc(nameOf(u.template))}</b>
            ${edit ? `<button type="button" class="tt-link tt-only" onclick="Views.tplUsageRemove(${u.id})">remove</button>` : ""}</div>`).join("") || `<span class="muted">None</span>`}
          ${edit ? `<details class="tpl-add"><summary>Add an exception</summary>
            <form class="inline-form" onsubmit="event.preventDefault(); Views.tplUsageAdd('${esc(ev.event)}', this)">
              <select name="party" aria-label="Filed by"><option value="">Filed by anyone</option><option value="PROVIDER">Filed by a provider</option><option value="HEALTH_PLAN">Filed by a health plan</option></select>
              <select name="lob" aria-label="Line of business"><option value="">Any line of business</option>${(d.lines_of_business || []).map((x) => `<option>${esc(x)}</option>`).join("")}</select>
              <select name="template" aria-label="Template">${options("")}</select>
              <button class="mini">Add</button></form></details>` : ""}</td></tr>`;
    }).join("");
    root.innerHTML = `
      <div class="rpt-tabs" role="tablist">
        <button type="button" role="tab" class="rpt-tab rpt-accent-1 ${tplTab === "templates" ? "active" : ""}" onclick="Views.tplShowTab('templates')">Templates (${d.templates.length})</button>
        <button type="button" role="tab" class="rpt-tab rpt-accent-1 ${tplTab === "usage" ? "active" : ""}" onclick="Views.tplShowTab('usage')">When to use</button>
      </div>
      <div class="tpl-panel" ${tplTab === "templates" ? "" : "hidden"}>
        <div class="tpl-bar">${edit ? `<button onclick="Views.tplEdit('')">New template</button>` : `<span class="muted">Templates are changed by the Program Manager or an administrator.</span>`}</div>
        <table><thead><tr><th>Template</th><th>When it is used</th><th>Goes to</th><th>Approved by</th><th>Status</th><th>Last change</th></tr></thead><tbody>${rows}</tbody></table>
        <div id="tpl-editor"></div>
      </div>
      <div class="tpl-panel" ${tplTab === "usage" ? "" : "hidden"}>
        <p class="muted">Each row is a point in the process. Choose which template is drafted there. An exception uses a different one for some cases; the most specific match wins.
          The approver belongs to the step and does not change with the template.</p>
        <table><thead><tr><th>When</th><th>Template used</th><th>Exceptions</th></tr></thead><tbody>${eventRows}</tbody></table>
      </div>`;
  }

  function tplShowTab(tab) { tplTab = tab; tplRender(); }

  // The editor, for an existing template or (key "") a new one.
  function tplEdit(key) {
    const d = tplData, box = document.getElementById("tpl-editor");
    if (!d || !box) return;
    const t = d.templates.find((x) => x.key === key) || { key: "", custom: true, to: [], cc: [], subject: "", body: "" };
    const isNew = !key, edit = d.can_edit, ro = edit ? "" : "disabled";
    const checks = (name, chosen) => d.recipient_roles.map((r) =>
      `<label class="tpl-check"><input type="checkbox" name="${name}" value="${esc(r)}" ${(chosen || []).includes(r) ? "checked" : ""} ${ro} /> ${esc(tplRole(r))}</label>`).join("");
    box.innerHTML = `<form class="card tpl-form" onsubmit="event.preventDefault(); Views.tplSave(this)">
      <h2>${isNew ? "New template" : esc(tplName(t))}</h2>
      ${isNew ? "" : `<p class="muted">${t.custom ? "A template added from this screen." : `Standard template — ${esc(t.when || "")}.`}
        ${t.qa_role ? `Approved by ${esc(t.qa_pre_role ? t.qa_pre_role + " then " + t.qa_role : t.qa_role)} before it is sent.` : "Sent without approval."}</p>`}
      <input type="hidden" name="key" value="${esc(t.key)}" />
      <div class="tt-grid">
        <label>Name <input name="name" value="${esc(t.name || (isNew ? "" : tplName(t)))}" maxlength="120" placeholder="e.g. Holiday closure notice" ${ro} ${isNew ? `oninput="Views.tplSlug(this)"` : ""} /></label>
        ${isNew ? `<label>Short name <input name="newkey" required pattern="[a-z][a-z0-9_]{2,40}" placeholder="holiday_closure_notice" title="Lowercase letters, digits and underscores" /></label>` : ""}
      </div>
      <label class="tpl-field">Subject <input name="subject" value="${esc(t.subject || "")}" maxlength="300" required ${ro} /></label>
      <label class="tpl-field">Message
        <textarea name="body" rows="14" required ${ro}>${esc(t.body || "")}</textarea></label>
      ${edit ? `<div class="tpl-ph"><span class="muted">Insert:</span> ${d.placeholders.map((p) =>
        `<button type="button" class="tt-preset" title="${esc(p.meaning)}" onclick="Views.tplInsert('${esc(p.token)}')">${esc(p.token)}</button>`).join("")}</div>` : ""}
      <fieldset class="tpl-set"><legend>Goes to</legend>${checks("to", t.to)}</fieldset>
      <fieldset class="tpl-set"><legend>Copied to</legend>${checks("cc", t.cc)}</fieldset>
      ${edit && t.custom && !isNew ? `<label class="tpl-check"><input type="checkbox" name="active" ${t.inactive ? "" : "checked"} /> In use (untick to retire it)</label>` : ""}
      <div class="tt-actions">
        ${edit ? `<button>Save</button>` : ""}
        <button type="button" class="mini ghost" onclick="Views.tplPreview(this.form)">Preview with a real case</button>
        ${isNew ? "" : `<button type="button" class="mini ghost" onclick="Views.tplHistory('${esc(t.key)}')">History</button>`}
        ${edit && !isNew && t.custom ? `<button type="button" class="mini danger" onclick="Views.tplDelete('${esc(t.key)}', false)">Delete</button>` : ""}
        ${edit && !isNew && !t.custom && t.edited ? `<button type="button" class="mini ghost" onclick="Views.tplDelete('${esc(t.key)}', true)">Restore the standard wording</button>` : ""}
        <button type="button" class="mini ghost" onclick="document.getElementById('tpl-editor').innerHTML=''">Close</button>
      </div>
      <div id="tpl-extra"></div></form>`;
    box.scrollIntoView({ behavior: "smooth", block: "start" });
  }

  // A new template's short name follows its name until the person types one.
  function tplSlug(input) {
    const f = input.form;
    if (!f.newkey || f.newkey.dataset.touched) return;
    f.newkey.value = input.value.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^[^a-z]+|_+$/g, "").slice(0, 41);
    f.newkey.oninput = () => { f.newkey.dataset.touched = "1"; };
  }

  function tplInsert(token) {
    const ta = document.querySelector("#tpl-editor textarea[name=body]");
    if (!ta) return;
    const a = ta.selectionStart ?? ta.value.length, b = ta.selectionEnd ?? ta.value.length;
    ta.value = ta.value.slice(0, a) + token + ta.value.slice(b);
    ta.focus(); ta.selectionStart = ta.selectionEnd = a + token.length;
  }

  const tplChecked = (form, name) => [...form.querySelectorAll(`input[name=${name}]:checked`)].map((i) => i.value);

  async function tplSave(form) {
    const key = form.key.value || (form.newkey ? form.newkey.value.trim() : "");
    const body = { name: form.name.value.trim(), subject: form.subject.value, body: form.body.value,
      to: tplChecked(form, "to"), cc: tplChecked(form, "cc") };
    if (form.active) body.active = form.active.checked;
    if (!form.key.value && tplData.templates.some((t) => t.key === key)) {
      UI.toast(`A template called ${key} already exists — pick another short name`, { kind: "warn" }); return;
    }
    await UI.run(form.querySelector(".tt-actions button"), async () => {
      try {
        await Api.program.templateSave(key, body);
        UI.toast("Template saved");
        await tplLoad();
        tplEdit(key);
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Saving…");
  }

  async function tplDelete(key, restore) {
    const ok = await UI.confirm(restore ? "Restore the standard wording?" : "Delete this template?",
      restore ? "Your edits are removed and the program's own text is used again. The edited version stays in the history."
              : "It will no longer be offered. Its history is kept.", restore ? "Restore" : "Delete");
    if (!ok) return;
    try { await Api.program.templateDelete(key); UI.toast(restore ? "Standard wording restored" : "Template deleted"); await tplLoad(); if (restore) tplEdit(key); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tplPreview(form) {
    const out = document.getElementById("tpl-extra");
    out.innerHTML = `<p class="muted">Filling in the placeholders…</p>`;
    try {
      const r = await Api.program.templatePreview({ subject: form.subject.value, body: form.body.value });
      out.innerHTML = `<div class="tpl-preview"><div class="muted">As it would read for case ${esc(r.case_number || "—")}. Links are created only when a message is really sent.</div>
        <div class="tpl-prev-subject">${esc(r.subject)}</div><pre>${esc(r.body)}</pre></div>`;
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tplHistory(key) {
    const out = document.getElementById("tpl-extra");
    try {
      const r = await Api.program.templateHistory(key);
      const label = { CREATED: "Created", EDITED: "Edited", DELETED: "Deleted", RESTORED_TO_STANDARD: "Restored to the standard wording" };
      out.innerHTML = (r.history || []).length ? `<div class="tpl-preview"><b>History</b>` + r.history.map((h) =>
        `<details><summary>${esc(label[h.action] || h.action)} by ${esc(h.changed_by || "—")} · ${fmtDate(h.changed_at)}</summary>
          ${h.before ? `<div class="muted">Before this change${h.before.source ? ` (${esc(h.before.source)})` : ""}:</div>
            <div class="tpl-prev-subject">${esc(h.before.subject || "")}</div><pre>${esc(h.before.body || "")}</pre>` : `<div class="muted">Nothing before it — this is where the template was created.</div>`}
        </details>`).join("") + `</div>` : `<p class="muted">No changes recorded — this is the program's standard wording.</p>`;
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tplUsageDefault(event, template) {
    try { await Api.program.templateUsageSet({ event, template }); UI.toast("Saved"); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
    await tplLoad();
  }

  async function tplUsageAdd(event, form) {
    if (!form.party.value && !form.lob.value) { UI.toast("Choose who filed or a line of business — or change the main template for this row instead", { kind: "warn" }); return; }
    try {
      await Api.program.templateUsageSet({ event, template: form.template.value, filing_party_type: form.party.value, line_of_business: form.lob.value });
      UI.toast("Exception added");
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    await tplLoad();
  }

  async function tplUsageRemove(id) {
    try { await Api.program.templateUsageDelete(id); UI.toast("Exception removed"); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
    await tplLoad();
  }

  // ---- Team time: the whole team's hours, searchable and filterable --------
  // One filter bar drives everything on the page: the date range, a team
  // member (search by name), a case, a role, billable or not. The summary,
  // the three tables, the CSV and the emailed report all show the same slice.
  const TT_PRESETS = [["this-week", "This week"], ["last-week", "Last week"], ["this-month", "This month"],
    ["last-month", "Last month"], ["last-30", "Last 30 days"]];
  let ttTab = "people";

  function ttIso(d) { return new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 10); }
  // [from, to], both inclusive, for a preset.
  function ttPresetRange(key) {
    const t = new Date(); t.setHours(0, 0, 0, 0);
    const monday = new Date(t); monday.setDate(t.getDate() - ((t.getDay() + 6) % 7));
    const day = (base, n) => { const d = new Date(base); d.setDate(d.getDate() + n); return d; };
    if (key === "this-week") return [monday, day(monday, 6)];
    if (key === "last-week") return [day(monday, -7), day(monday, -1)];
    if (key === "last-month") return [new Date(t.getFullYear(), t.getMonth() - 1, 1), new Date(t.getFullYear(), t.getMonth(), 0)];
    if (key === "last-30") return [day(t, -29), t];
    return [new Date(t.getFullYear(), t.getMonth(), 1), new Date(t.getFullYear(), t.getMonth() + 1, 0)];
  }

  async function timeReport() {
    if (!can("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Team time</h1></div><p class="muted">Requires PM or FINANCE.</p>`;
    afterRender(() => { timeRatesBox(); timeReportPreset("this-month"); });
    return `<div class="view-head"><h1>Team time</h1>
      <span class="muted">who worked on what, and for how long</span></div>
      <form id="tt-filters" class="card tt-filters" onsubmit="event.preventDefault(); Views.timeReportRun()">
        <div class="tt-presets" role="group" aria-label="Date range shortcuts">
          ${TT_PRESETS.map(([k, label]) => `<button type="button" class="tt-preset" data-preset="${k}" onclick="Views.timeReportPreset('${k}')">${label}</button>`).join("")}
        </div>
        <div class="tt-grid">
          <label>From <input name="from" type="date" required onchange="Views.timeReportDates()" /></label>
          <label>To <input name="to" type="date" required onchange="Views.timeReportDates()" /></label>
          <label>Team member <input name="who" list="tt-people" placeholder="Search by name" autocomplete="off" /></label>
          <datalist id="tt-people"></datalist>
          <label>Case <input name="case" placeholder="Case number" autocomplete="off" /></label>
          <label>Role <select name="role"><option value="">Any role</option></select></label>
          <label>Time <select name="billable"><option value="">Billable and non-billable</option>
            <option value="yes">Billable only</option><option value="no">Non-billable only</option></select></label>
        </div>
        <div class="tt-actions"><button>Apply</button>
          <button type="button" class="mini ghost" onclick="Views.timeReportReset()">Clear filters</button>
          <span id="tt-active" class="muted"></span></div>
      </form>
      <div id="time-rpt"><p class="muted">Loading…</p></div>
      <details class="card tt-more"><summary>Email or download this report</summary>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.timeReportSend(this)">
          <input name="emails" placeholder="recipients (comma-separated emails)" required style="min-width:20em" />
          <input name="message" placeholder="optional message" style="min-width:14em" />
          <button class="mini">Email report</button>
          <button class="mini ghost" type="button" onclick="Views.timeReportCsv()">Download CSV</button></form>
        <p class="muted">Sends exactly what is shown above, filters included. The CSV carries every entry. Sends are audit-logged.</p></details>
      <details class="card tt-more"><summary>Billable rates by role</summary><div id="time-rates"></div></details>`;
  }

  function ttForm() { return document.getElementById("tt-filters"); }

  function timeReportPreset(key) {
    const f = ttForm(); if (!f) return;
    const [from, to] = ttPresetRange(key);
    f.from.value = ttIso(from); f.to.value = ttIso(to);
    timeReportDates();
    timeReportRun();
  }

  // Lights the shortcut that matches the dates in the boxes, if one does.
  function timeReportDates() {
    const f = ttForm(); if (!f) return;
    f.querySelectorAll(".tt-preset").forEach((b) => {
      const [from, to] = ttPresetRange(b.dataset.preset);
      b.classList.toggle("active", ttIso(from) === f.from.value && ttIso(to) === f.to.value);
    });
  }

  function timeReportReset() {
    const f = ttForm(); if (!f) return;
    f.who.value = ""; f.case.value = ""; f.role.value = ""; f.billable.value = "";
    timeReportRun();
  }

  // Clicking a name or a case in the results narrows the report to it.
  function timeReportPick(field, value) {
    const f = ttForm(); if (!f) return;
    f[field].value = value;
    if (field === "who") ttTab = "cases";
    if (field === "case") ttTab = "people";
    timeReportRun();
  }

  function timeReportTab(key) {
    ttTab = key;
    document.querySelectorAll("#time-rpt .tt-tab").forEach((b) => {
      const on = b.dataset.tab === key; b.classList.toggle("active", on); b.setAttribute("aria-selected", on);
    });
    document.querySelectorAll("#time-rpt .tt-panel").forEach((p) => { p.hidden = p.dataset.tab !== key; });
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
      box.innerHTML = `
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

  async function timeReportRun() {
    const f = ttForm();
    const box = document.getElementById("time-rpt");
    if (!f || !box) return;
    if (!f.from.value || !f.to.value) { UI.toast("Pick a start and an end date", { kind: "warn" }); return; }
    if (f.to.value < f.from.value) { UI.toast("The end date is before the start date", { kind: "warn" }); return; }
    // The boxes show the last day included; the API takes the day after it.
    const end = new Date(f.to.value + "T00:00:00"); end.setDate(end.getDate() + 1);
    const opts = { start: f.from.value, end: ttIso(end), who: f.who.value.trim(), case: f.case.value.trim(),
      role: f.role.value, billable: f.billable.value };
    Object.keys(opts).forEach((k) => { if (!opts[k]) delete opts[k]; });
    const btn = f.querySelector(".tt-actions button");
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.timeReport(opts);
        window._timeRptOpts = opts;
        window._timeRpt = r;
        ttRender(r, f);
      } catch (e) { box.innerHTML = `<p class="muted">⚠ ${esc(e.message)}</p>`; }
    }, "Loading…");
  }

  function ttRender(r, f) {
    const box = document.getElementById("time-rpt");
    const usd2 = (c) => "$" + ((Number(c) || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
    const money = r.total_amount_cents !== undefined; // the server attaches amounts only for roles that may see money
    const attr = (v) => esc(String(v)).replace(/'/g, "&#39;");

    // What can be picked: everyone and every role with time in this period.
    document.getElementById("tt-people").innerHTML = (r.people || []).map((p) => `<option value="${attr(p.name)}"></option>`).join("");
    const role = f.role.value;
    f.role.innerHTML = `<option value="">Any role</option>` + (r.roles || []).map((x) => `<option ${x === role ? "selected" : ""}>${esc(x)}</option>`).join("");
    const active = [f.who.value && `team member “${f.who.value}”`, f.case.value && `case “${f.case.value}”`,
      f.role.value && `role ${f.role.value}`, f.billable.value && (f.billable.value === "yes" ? "billable only" : "non-billable only")].filter(Boolean);
    document.getElementById("tt-active").textContent = active.length ? `Filtered by ${active.join(", ")}` : "";

    const people = r.by_person || [], cases = r.by_case || [], entries = r.entries || [];
    if (!entries.length) {
      box.innerHTML = `<div class="card tt-empty"><b>No time logged ${active.length ? "for these filters" : "in this period"}.</b>
        <p class="muted">${esc(String(r.from))} to ${esc(f.to.value)}${active.length ? ` — try clearing a filter or widening the dates.` : "."}</p></div>`;
      return;
    }
    const tiles = [[fmtMins(r.total_minutes), "Total time"], [fmtMins(r.billable_minutes), "Billable"]]
      .concat(money ? [[usd2(r.total_amount_cents), "Billable amount" + (r.unrated_billable_minutes ? ` (+${fmtMins(r.unrated_billable_minutes)} with no rate)` : "")]] : [])
      .concat([[r.people_count, r.people_count === 1 ? "Team member" : "Team members"], [cases.length, cases.length === 1 ? "Dispute" : "Disputes"], [r.entry_count, r.entry_count === 1 ? "Entry" : "Entries"]]);
    const personRows = people.map((p) => `<tr>
      <td><button type="button" class="tt-link" title="Show only this team member" onclick="Views.timeReportPick('who','${attr(p.who)}')">${esc(p.who)}</button></td>
      <td>${badge(p.role)}</td><td class="num">${fmtMins(p.minutes)}</td><td class="num">${fmtMins(p.billable_minutes)}</td>
      ${money ? `<td class="num">${p.amount_cents != null ? usd2(p.amount_cents) : "—"}</td>` : ""}</tr>`).join("");
    const caseRows = cases.map((c) => { const num = c.case_number || c.case_id.slice(0, 8) + "…"; return `<tr>
      <td class="mono"><a href="#/cases/${c.case_id}">${esc(num)}</a>
        <button type="button" class="tt-link tt-only" title="Show only this case" onclick="Views.timeReportPick('case','${attr(c.case_number || c.case_id)}')">only this</button></td>
      <td class="num">${fmtMins(c.minutes)}</td><td class="num">${fmtMins(c.billable_minutes)}</td><td class="num">${c.people}</td>
      ${money ? `<td class="num">${c.amount_cents != null ? usd2(c.amount_cents) : "—"}${c.unrated_billable_minutes ? ` <span class="muted" title="billable time logged under a role with no rate set">+${fmtMins(c.unrated_billable_minutes)} no rate</span>` : ""}</td>` : ""}</tr>`; }).join("");
    const entryRows = entries.map((e) => `<tr>
      <td class="mono">${esc(e.entry_date)}</td><td>${esc(e.who)}</td><td>${badge(e.role)}</td>
      <td class="mono"><a href="#/cases/${e.case_id}">${esc(e.case_number || e.case_id.slice(0, 8) + "…")}</a></td>
      <td class="num">${fmtMins(e.minutes)}</td><td>${e.billable ? "Billable" : `<span class="muted">Non-billable</span>`}</td>
      <td class="tt-note">${esc(e.note || "")}</td></tr>`).join("");
    const tabs = [["people", `By team member (${people.length})`], ["cases", `By dispute (${cases.length})`], ["entries", `Entries (${r.entry_count})`]];
    box.innerHTML = `<h2 class="tt-period">${esc(r.period)}</h2>
      <div class="kpi-row">${tiles.map(([n, l]) => `<div class="kpi"><span class="kpi-n">${esc(String(n))}</span><span class="kpi-l">${esc(l)}</span></div>`).join("")}</div>
      <div class="rpt-tabs" role="tablist">${tabs.map(([k, label]) =>
        `<button type="button" role="tab" class="rpt-tab rpt-accent-1 tt-tab" data-tab="${k}" onclick="Views.timeReportTab('${k}')">${esc(label)}</button>`).join("")}</div>
      <div class="tt-panel" data-tab="people"><table><thead><tr><th>Team member</th><th>Role</th><th class="num">Time</th><th class="num">Billable</th>${money ? `<th class="num">Amount</th>` : ""}</tr></thead><tbody>${personRows}</tbody></table></div>
      <div class="tt-panel" data-tab="cases"><table><thead><tr><th>Case</th><th class="num">Time</th><th class="num">Billable</th><th class="num">People</th>${money ? `<th class="num">Amount</th>` : ""}</tr></thead><tbody>${caseRows}</tbody></table></div>
      <div class="tt-panel" data-tab="entries"><table><thead><tr><th>Date</th><th>Team member</th><th>Role</th><th>Case</th><th class="num">Time</th><th>Type</th><th>Note</th></tr></thead><tbody>${entryRows}</tbody></table>
        ${r.entry_count > entries.length ? `<p class="muted">Showing the ${entries.length} most recent of ${r.entry_count} entries — the totals above cover all of them. Narrow the filters to see the rest.</p>` : ""}</div>`;
    timeReportTab(ttTab);
  }

  // Email / export the team-hours report currently on screen.
  async function timeReportSend(form) {
    if (!window._timeRptOpts) { UI.toast("The report has not loaded yet", { kind: "warn" }); return; }
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
    if (!r) { UI.toast("The report has not loaded yet", { kind: "warn" }); return; }
    const money = r.total_amount_cents !== undefined;
    const hrs = (m) => (m / 60).toFixed(2);
    const cell = (v) => /[",\n]/.test(String(v)) ? `"${String(v).replace(/"/g, '""')}"` : String(v);
    const lines = [["section", "who_or_case", "hours", "billable_hours"].concat(money ? ["amount"] : []).join(",")];
    (r.by_person || []).forEach((p) => lines.push(["person", cell(`${p.who} (${p.role})`), hrs(p.minutes), hrs(p.billable_minutes)].concat(money ? [p.amount_cents != null ? (p.amount_cents / 100).toFixed(2) : ""] : []).join(",")));
    (r.by_case || []).forEach((c) => lines.push(["case", cell(c.case_number || c.case_id), hrs(c.minutes), hrs(c.billable_minutes)].concat(money ? [c.amount_cents != null ? (c.amount_cents / 100).toFixed(2) : ""] : []).join(",")));
    lines.push("");
    lines.push("date,team_member,role,case,hours,billable,note");
    (r.entries || []).forEach((e) => lines.push([e.entry_date, cell(e.who), e.role, cell(e.case_number || e.case_id), hrs(e.minutes), e.billable ? "yes" : "no", cell(e.note || "")].join(",")));
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

  // Human correction of an extracted check field — feeds the doc-intel
  // learning loop (memo-noise phrases / payer aliases mined + promoted).
  async function correctCheck(checkId, el) {
    const field = prompt("Field to correct (memo, payer_name, check_date, check_number, routing_number, account_number, amount):", "memo");
    if (!field) return;
    const corrected = prompt(`Corrected value for ${field}:`, "");
    if (corrected === null || corrected === "") return;
    await UI.run(el, async () => {
      try {
        await Api.program.checkFeedback(checkId, { field: field.trim(), corrected_value: corrected.trim() });
        UI.toast("Correction recorded — the extraction model learns from it");
        el.disabled = true; el.textContent = "✓ sent";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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
        <p><button onclick="Views.rebrandTenantFlow('${esc(pr?.brand_name || "")}')">✎ Brand name: "${esc(pr?.brand_name || "Capitol Bridge")}"</button>
           <span class="muted">the portal's name for this tenant — in the sidebar, the page title and the public pages</span></p>
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
        <div id="rules-audit-box" data-nosearch>${rulesAuditHtml(window._rulesAudit)}</div>`;
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
          ["CASE_MANAGER", "Reviewer (Case Manager)"], ["PM", "PM"], ["CODER", "Coder"],
          ["NURSE_PHYSICIAN", "Nurse / Physician"], ["ATTORNEY", "Attorney"], ["ARBITRATOR", "Arbitrator"],
          ["DOCTOR", "Doctor (older role)"], ["NURSE", "Nurse (older role)"],
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
  async function rebrandTenantFlow(cur) {
    const v = await UI.modal({
      title: "Brand name", submitLabel: "Save",
      body: "The name this tenant's portal shows. Leave it empty to use the platform's name, Capitol Bridge.",
      fields: [{ name: "brand", label: "Brand name", value: cur, placeholder: "Capitol Bridge" }],
    });
    if (!v) return;
    try {
      const r = await Api.program.setBrand((v.brand || "").trim());
      UI.toast(`Brand set to "${r.brand_name}" — reloading`);
      setTimeout(() => location.reload(), 900);
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }
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
  // The tenant's staff roles, as the Team page shows and edits them (staffroles.go: staffEditableRoles).
  const TEAM_ROLES = [["CASE_MANAGER", "Reviewer"], ["PM", "PM"], ["CODER", "Coder"], ["NURSE_PHYSICIAN", "Clinical reviewer"],
    ["ATTORNEY", "Attorney"], ["ARBITRATOR", "Arbitrator"], ["FINANCE", "Finance"], ["STATE_AUDITOR", "State Auditor"],
    ["DOCTOR", "Doctor (older role)"], ["NURSE", "Nurse (older role)"], ["TENANT_ADMIN", "Tenant Admin"]];
  const staffRoleLabel = (r) => (TEAM_ROLES.find(([k]) => k === r) || [r, r])[1];
  const staffRolesOf = (m) => (m.roles || []).filter((r) => TEAM_ROLES.some(([k]) => k === r));
  let teamStaff = [], staffRoleFilter = "";

  function staffTableHtml(members, scope, tenant) {
    if (!members.length) return `<p class="muted">No staff found.</p>`;
    const me = Auth.claims()?.name;
    const tenantScope = scope === "tenant";
    const present = tenantScope ? TEAM_ROLES.filter(([r]) => members.some((m) => (m.roles || []).includes(r))) : [];
    // Staff have at least one staff role; the rest are portal sign-ins (TPA, health plan, party logins), kept apart.
    const isPortal = (m) => tenantScope && !staffRolesOf(m).length;
    const chip = (key, label, n) => `<button type="button" class="wl-chip${staffRoleFilter === key ? " active" : ""}" data-staff-role="${key}"
        onclick="Views.staffFilter('${key}')">${esc(label)} <b>${n}</b></button>`;
    const filter = tenantScope ? `<div class="staff-filter" role="group" aria-label="Filter by role">
        ${chip("", "All staff", members.filter((m) => !isPortal(m)).length)}
        ${present.map(([r, l]) => chip(r, l, members.filter((m) => (m.roles || []).includes(r)).length)).join("")}
        ${members.some(isPortal) ? chip("__portal", "Portal sign-ins", members.filter(isPortal).length) : ""}</div>` : "";
    return filter + `<div class="dg-wrap"><table id="${tenantScope ? "staff-table" : ""}" class="staff-tbl" data-page-size="15"><thead><tr><th>Name</th><th>Email</th><th>Roles</th>
        ${tenantScope ? `<th class="num">Open cases</th>` : ""}<th>Status</th><th></th></tr></thead><tbody>` +
      members.map((m) => {
        const isSelf = m.username === me;
        const actions = isSelf ? '<span class="muted">(you)</span>' : (
          (m.enabled
            ? `<button class="mini ghost" onclick="Views.setStaffEnabled('${scope}','${esc(tenant || "")}','${esc(m.username)}',false)">Suspend</button>`
            : `<button class="mini ghost" onclick="Views.setStaffEnabled('${scope}','${esc(tenant || "")}','${esc(m.username)}',true)">Reactivate</button>`) +
          ` <button class="mini ghost danger-text" onclick="Views.deleteStaffMember('${scope}','${esc(tenant || "")}','${esc(m.username)}')">Delete</button>`
        );
        const roles = m.roles || [];
        const portal = isPortal(m);
        const shown = !tenantScope || (staffRoleFilter === "__portal" ? portal : staffRoleFilter ? roles.includes(staffRoleFilter) : !portal);
        const name = tenantScope ? `<button class="linkish" onclick="Views.staffOpen('${esc(m.username)}')">${esc(m.username)}</button>` : esc(m.username);
        const open = teamOpen[m.id];
        return `<tr data-roles="${esc(roles.join(" "))}" data-portal="${portal ? 1 : 0}"${shown ? "" : ' class="flt-out"'}><td>${name}</td><td class="muted">${esc(m.email || "—")}</td>
        <td>${(tenantScope ? staffRolesOf(m) : roles).map((r) => `<span class="badge s-${esc(r).toLowerCase().replace(/_/g, "-")}">${esc(tenantScope ? staffRoleLabel(r) : r)}</span>`).join(" ") || '<span class="muted">portal sign-in</span>'}</td>
        ${tenantScope ? `<td class="num">${open != null ? `<a href="#/cases?assignee=${encodeURIComponent(m.id)}">${open.toLocaleString()}</a>` : '<span class="muted">—</span>'}</td>` : ""}
        <td>${m.enabled ? '<span class="badge s-paid">active</span>' : '<span class="badge s-denied">suspended</span>'}</td>
        <td class="row-actions">${actions}</td></tr>`;
      }).join("") +
      `</tbody></table></div>`;
  }

  // "" = every staff member (not portal sign-ins), "__portal" = the portal sign-ins, else one role.
  function staffFilter(role) {
    staffRoleFilter = role;
    document.querySelectorAll("#staff-table tbody tr").forEach((tr) => {
      const portal = tr.dataset.portal === "1";
      const show = role === "__portal" ? portal : role ? tr.dataset.roles.split(" ").includes(role) : !portal;
      tr.classList.toggle("flt-out", !show); // the pager counts and pages only the rows the chip keeps
    });
    const t = document.getElementById("staff-table");
    if (t && t._pg) t._pg.page = 0;
    Pager.scan();
    document.querySelectorAll("[data-staff-role]").forEach((b) => b.classList.toggle("active", b.dataset.staffRole === role));
  }

  // One staff member: what they do, their open cases and the rotations they are in; the PM / Tenant Admin changes
  // their roles here. Giving or taking away Tenant Admin stays with the platform admins; nobody edits their own.
  async function staffOpen(username) {
    const m = teamStaff.find((x) => x.username === username);
    if (!m) return;
    const isAdmin = can("FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const isSelf = m.username === Auth.claims()?.name;
    const theirs = ASSIGN_ROLES.filter(([r]) => (m.roles || []).includes(r));
    const rot = await Promise.all(theirs.map(([r]) => Api.cm.autoAssign(r).catch(() => null)));
    let open = 0;
    const rotLines = theirs.map(([r, label], i) => {
      const a = rot[i];
      const row = a && (a.people || []).find((x) => x.id === m.id);
      if (row) open = Math.max(open, row.open_cases);
      if (!a) return `<li>${esc(label)}: <span class="muted">unavailable</span></li>`;
      const state = !row ? "not active" : !row.in_rotation ? "taken out of the rotation" : !a.enabled ? "rotation is off"
        : a.next === m.id ? "in the rotation — <b>next in turn</b>" : "in the rotation";
      return `<li>${esc(label)}: ${state}</li>`;
    }).join("");
    const boxes = TEAM_ROLES.map(([r, label]) => {
      const has = (m.roles || []).includes(r);
      const locked = isSelf || (r === "TENANT_ADMIN" && !isAdmin);
      return `<label class="sr-role"><input type="checkbox" name="role_${r}" ${has ? "checked" : ""} ${locked ? "disabled" : ""} /> ${esc(label)}</label>`;
    }).join("");
    const v = await UI.modal({
      title: esc(m.username), submitLabel: isSelf ? "Close" : "Save roles",
      body: `${esc(m.email || "")}${m.enabled ? "" : " · <b>suspended</b>"} · ${open} open case${open === 1 ? "" : "s"}
        ${rotLines ? `</p><ul class="sr-rot">${rotLines}</ul><p class="modal-body">` : "<br>"}
        <span class="sr-head">Roles</span></p><div class="sr-roles">${boxes}</div><p class="mhint">
        ${isSelf ? "You can't change your own roles — ask another PM or an admin." : "Changes apply the next time they sign in."}
        ${!isAdmin ? " Tenant Admin is given or taken away by a platform admin." : ""}`,
    });
    if (!v || isSelf) return;
    const roles = TEAM_ROLES.filter(([r]) => {
      const locked = r === "TENANT_ADMIN" && !isAdmin;
      return locked ? (m.roles || []).includes(r) : v["role_" + r] === "on";
    }).map(([r]) => r);
    if (!roles.length) { UI.toast("Keep at least one role — suspend the account instead to stop them working.", { kind: "warn" }); return; }
    try {
      const r = await Api.admin.setTenantStaffRoles(Api.getTenant(), m.username, roles);
      const said = [...(r.added || []).map((x) => "+" + staffRoleLabel(x)), ...(r.removed || []).map((x) => "−" + staffRoleLabel(x))];
      UI.toast(said.length ? `${m.username}: ${said.join(", ")}` : "No change");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  async function teamAdmin() {
    if (!can("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Team</h1></div><p class="muted">Your role doesn't have access to Team.</p>`;
    const isAdmin = can("FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const tenant = Api.getTenant();
    const head = (n) => `<div class="view-head"><h1>Team</h1>${n || ""}<span style="flex:1"></span>
      <button class="mini" onclick="Views.addTenantStaffFlow()">＋ Add staff</button>
      ${isAdmin ? `<button class="mini secondary" onclick="Views.createFederalAdminFlow()">＋ Create federal admin</button>` : ""}</div>`;
    try {
      const [staff, federal] = await Promise.all([
        Api.admin.listTenantStaff(tenant),
        isAdmin ? Api.admin.listFederalAdmins().catch(() => []) : Promise.resolve(null),
      ]);
      teamStaff = staff;
      // Who does the case work: each assignable role that someone holds, with its rotation (open cases per person).
      const held = ASSIGN_ROLES.filter(([r]) => staff.some((m) => (m.roles || []).includes(r)));
      const rotations = await Promise.all(held.map(([r]) => Api.cm.autoAssign(r).catch(() => null)));
      teamOpen = {};
      rotations.forEach((a) => (a?.people || []).forEach((x) => { teamOpen[x.id] = x.open_cases; }));
      const staffN = staff.filter((m) => staffRolesOf(m).length).length;
      let html = head(`<span class="muted">${staffN} staff · ${staff.length - staffN} portal sign-ins · tenant ${esc(tenant.toUpperCase())}</span>`);
      html += teamWorkloadHtml(held, rotations);
      html += `<div id="aa-box">${await autoAssignBox()}</div>`;
      html += `<section class="team-sec"><h2>People</h2>${staffTableHtml(staff, "tenant", tenant)}</section>`;
      if (federal) html += `<section class="team-sec"><h2>Federal / platform admins <span class="muted">cross-tenant</span></h2>${staffTableHtml(federal, "federal")}</section>`;
      return `<div class="wide-view team">${html}</div>`;
    } catch (e) { return head() + err(e); }
  }

  // Workload: for each role that does case work, everyone holding it with their open cases (a bar against the
  // busiest), where they stand in the rotation, and a link to their cases; for Reviewers, the cases nobody holds.
  let teamOpen = {};
  function teamWorkloadHtml(held, rotations) {
    if (!held.length) return "";
    const panels = held.map(([role, label], i) => {
      const a = rotations[i];
      if (!a) return `<div class="load-role"><h3>${esc(label)}s</h3><p class="muted">Workload unavailable.</p></div>`;
      const people = (a.people || []).slice().sort((x, y) => y.open_cases - x.open_cases || x.username.localeCompare(y.username));
      const max = Math.max(1, ...people.map((x) => x.open_cases));
      const total = people.reduce((n, x) => n + x.open_cases, 0);
      const state = (x) => !a.enabled ? `<span class="load-state">rotation off</span>`
        : !x.in_rotation ? `<span class="load-state out">out of rotation</span>`
        : a.next === x.id ? `<span class="load-state next">next in turn</span>` : `<span class="load-state">in rotation</span>`;
      const rows = people.map((x) => `<tr>
          <td><button class="linkish" onclick="Views.staffOpen('${esc(x.username)}')">${esc(x.username)}</button></td>
          <td class="load-bar-cell"><a class="load-bar" href="#/cases?assignee=${encodeURIComponent(x.id)}" title="See ${esc(x.username)}'s open cases">
            <span class="load-fill" style="width:${Math.round((x.open_cases / max) * 100)}%"></span></a></td>
          <td class="num"><a href="#/cases?assignee=${encodeURIComponent(x.id)}">${x.open_cases.toLocaleString()}</a></td>
          <td>${state(x)}</td></tr>`).join("");
      const backlog = a.new_cases && a.unassigned_open
        ? `<a class="load-unassigned" href="#/cases?assigned=unassigned">${a.unassigned_open.toLocaleString()} open case${a.unassigned_open === 1 ? "" : "s"} waiting for a Reviewer →</a>` : "";
      return `<div class="load-role"><h3>${esc(label)}s <span class="muted">${people.length} · ${total.toLocaleString()} open case${total === 1 ? "" : "s"}</span></h3>
        ${backlog}
        ${people.length ? `<table class="load-tbl" data-page-size="5" data-nosearch><thead><tr><th>Person</th><th>Open cases</th><th class="num"></th><th>Rotation</th></tr></thead><tbody>${rows}</tbody></table>`
          : `<p class="muted">Nobody active in this role.</p>`}</div>`;
    }).join("");
    return `<section class="team-sec"><h2>Workload</h2>
      <p class="muted">Open cases each person holds. Click a number or a bar to see those cases; click a name to see or change their roles.</p>
      <div class="load-grid">${panels}</div></section>`;
  }

  // Automatic assignment: one rotation per role -- the fewest open cases first, ties in turn. Reviewers get new cases
  // as they come in; for every role the PM
  // can hand a case to "Next in turn". The PM / Tenant Admin turns a rotation on or off and takes people out of it;
  // any assignment can still be changed by hand on the case.
  let aaRole = "CASE_MANAGER";
  async function autoAssignBox() {
    if (!canAssign()) return "";
    let a;
    try { a = await Api.cm.autoAssign(aaRole); } catch (e) {
      return `<h2>Automatic assignment</h2><p class="muted">${esc(e.message)}</p>`;
    }
    const label = a.role_label || "Reviewer";
    const people = a.people || [];
    const next = people.find((p) => p.id === a.next);
    const inTurn = people.filter((p) => p.in_rotation).length;
    const status = a.new_cases
      ? (!a.enabled ? "Off — new cases wait in the unassigned pool for the PM to assign or a Reviewer to take."
        : !inTurn ? "On, but nobody is in the rotation — new cases wait in the unassigned pool."
        : `On — the next new case goes to <strong>${esc(next?.username || "—")}</strong>.`)
      : (!a.enabled ? `Off — a ${esc(label)} is named on a case by hand.`
        : !inTurn ? `On, but nobody is in the rotation — name a ${esc(label)} on the case by hand.`
        : `On — the next case that reaches a ${esc(label)} step (or is handed to one with "Next in turn") goes to <strong>${esc(next?.username || "—")}</strong>. The Reviewer keeps the case.`);
    const tabs = ASSIGN_ROLES.map(([r, l]) =>
      `<button type="button" class="tab ${r === aaRole ? "active" : ""}" onclick="Views.autoAssignRole('${r}')">${esc(l)}</button>`).join("");
    const rows = people.map((p) => `<tr>
        <td><button class="linkish mono" onclick="Views.staffOpen('${esc(p.username)}')">${esc(p.username)}</button>${p.id === a.next && a.enabled ? ' <span class="badge s-paid">next</span>' : ""}</td>
        <td class="num">${p.open_cases}</td>
        <td><label class="aa-in"><input type="checkbox" ${p.in_rotation ? "checked" : ""}
          onchange="Views.autoAssignRotation('${esc(p.id)}', this.checked, this)" /> In rotation</label></td></tr>`).join("");
    return `<h2>Automatic assignment</h2>
      <div class="tabs aa-tabs" role="tablist">${tabs}</div>
      <div class="card aa-card">
        <label class="aa-toggle"><input type="checkbox" ${a.enabled ? "checked" : ""} onchange="Views.autoAssignToggle(this.checked, this)" />
          ${a.new_cases ? "Assign new cases to Reviewers automatically, in turn" : `Add a ${esc(label)} automatically, in turn, when a case reaches their step`}</label>
        <p class="muted">${status} Whoever in the rotation has the fewest open cases goes first; people with the same number take turns. You can change who has any case on the case itself.${a.updated_by ? ` Last changed by ${esc(a.updated_by)}.` : ""}</p>
        ${a.new_cases && a.unassigned_open ? `<div class="aa-backlog"><span><strong>${a.unassigned_open}</strong> open case${a.unassigned_open === 1 ? "" : "s"}
          nobody holds (from before automatic assignment, or filed while it was off). Reviewers can't see them until they're assigned.</span>
          <button class="mini" onclick="Views.autoAssignBacklog(this)" ${!a.enabled || !inTurn ? "disabled" : ""}>Assign them now, in turn</button></div>` : ""}
        ${people.length
          ? `<table data-page-size="10"><thead><tr><th>${esc(label)}</th><th class="num">Open cases</th><th>Takes cases in turn</th></tr></thead><tbody>${rows}</tbody></table>`
          : `<p class="muted">No active ${esc(label)} on this tenant yet — add one above, or give someone the role.</p>`}
      </div>`;
  }

  async function aaRefresh() {
    const box = document.getElementById("aa-box");
    if (box) box.innerHTML = await autoAssignBox(); else App.rerender();
  }

  async function autoAssignRole(role) { aaRole = role; await aaRefresh(); }

  // The unassigned backlog to the Reviewers, by the same rule as new cases; 500 per round, repeated until done.
  async function autoAssignBacklog(el) {
    const a = await Api.cm.autoAssign("CASE_MANAGER").catch(() => null);
    const n = a?.unassigned_open || 0;
    if (!n || !(await UI.confirm(`Assign ${n} case${n === 1 ? "" : "s"} now?`,
      "They go to the Reviewers in the rotation, oldest first: whoever has the fewest open cases goes first. Each Reviewer is told how many they got. Cases waiting on their fee stay as they are.",
      "Assign them"))) return;
    await UI.run(el, async () => {
      let total = 0;
      const per = {};
      try {
        for (let round = 0; round < 40; round++) {
          const r = await Api.cm.assignOpen();
          total += r.assigned;
          (r.people || []).forEach((p) => { per[p.username] = (per[p.username] || 0) + p.cases; });
          if (!r.remaining || !r.assigned) break;
        }
        UI.toast(`Assigned ${total} case${total === 1 ? "" : "s"}: ${Object.entries(per).map(([u, c]) => `${u} ${c}`).join(", ") || "none"}`, { duration: 9000 });
      } catch (e) { UI.toast(e.message, { kind: "error", sticky: true }); }
      await aaRefresh();
    }, "Assigning…");
  }

  async function autoAssignToggle(on, el) {
    try {
      const a = await Api.cm.setAutoAssign({ role: aaRole, enabled: on });
      UI.toast(on ? `${a.role_label} rotation is on` : `${a.role_label} rotation is off`);
      await aaRefresh();
    } catch (e) { if (el) el.checked = !on; UI.toast(e.message, { kind: "error", sticky: true }); }
  }

  async function autoAssignRotation(id, inRotation, el) {
    try {
      const a = await Api.cm.autoAssign(aaRole);
      const out = (a.people || []).filter((p) => (p.id === id ? !inRotation : !p.in_rotation)).map((p) => p.id);
      await Api.cm.setAutoAssign({ role: aaRole, excluded: out });
      await aaRefresh();
    } catch (e) { if (el) el.checked = !inRotation; UI.toast(e.message, { kind: "error", sticky: true }); }
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
          ["CASE_MANAGER", "Reviewer (Case Manager)"], ["PM", "PM"], ["CODER", "Coder"],
          ["NURSE_PHYSICIAN", "Nurse / Physician"], ["ATTORNEY", "Attorney"], ["ARBITRATOR", "Arbitrator"],
          ["DOCTOR", "Doctor (older role)"], ["NURSE", "Nurse (older role)"],
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
        <p><label class="muted"><input type="checkbox" id="al-mine" onchange="Views.auditMineToggle(this.checked)"> Only my actions</label></p>
        <div id="al-all">
        <form id="al-filter" class="inline-form">
          <input name="case_id" placeholder="filter by case id" />
          <input name="action" placeholder="filter by action (e.g. NPI_VERIFICATION_DEGRADED)" />
          <button>Filter</button>
        </form>
        <div id="al-list" data-nosearch>${auditRows(r.entries || [])}</div></div>
        <div id="al-own" hidden><form class="inline-form" onsubmit="event.preventDefault(); Views.orgAuditBox('al-own-box', 'mine')">
          <input name="q" placeholder="Search: dispute, action" aria-label="Search your actions">
          <label class="muted"><input type="checkbox" name="views" onchange="Views.orgAuditBox('al-own-box', 'mine')"> Include page views</label>
          <button>Search</button></form><div id="al-own-box" data-nosearch></div></div>`;
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
      ${Letters.settingsCardHtml()}
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
    try { await Api.program.billingExport(id); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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
        <div id="arap-ar-results" data-nosearch>${arapArResultsHtml(window._arapAr, "", "")}</div>`;
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
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function arapRecord(form) {
    try {
      await Api.program.payableCreate({
        payee: form.payee.value, amount_cents: Math.round(Number(form.amount.value) * 100),
        source: form.source.value, case_id: form.case_id.value || undefined,
        due_date: form.due_date.value || undefined,
      });
      form.reset(); arapApBox(); arapSummaryBox();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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
            ${statuses.map((s) => `<option value="${esc(s)}">${esc(s)}</option>`).join("")}
          <optgroup label="Payment"><option value="UNPAID">Unpaid</option><option value="PENDING">Pending</option><option value="FAILED">Failed</option><option value="PAID">Paid</option></optgroup></select>` : "";
      box.innerHTML = `<div class="view-head"><h2>Batches</h2>${filter}</div>
        <div id="recon-batches-list">${reconBatchRowsHtml(window._reconBatches, "")}</div>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function reconMatchRun(id) {
    try { const m = await Api.program.reconMatch(id);
      alert(`matched ${m.matched} · exceptions ${m.exceptions} · unmatched ${m.unmatched}`);
      reconBatchList(); reconOpen(id);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ── Third-party administrators ─────────────────────────────────────
  function tpaView() {
    if (!can("TPA", "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Third-party administrators</h1></div><p class="muted">Requires a TPA, CASE_MANAGER, or FINANCE account.</p>`;
    afterRender(tpaBoot);
    const onb = App.feature("tpa_onboarding");
    const staff = can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const tab = (k, label, ws) => `<button type="button" role="tab" class="rpt-tab rpt-accent-4${ws ? " tpa-ws" : ""}" data-tab="${k}" onclick="Views.tpaTab('${k}')"${ws ? ' style="display:none"' : ""}>${label} <span class="tab-count" id="tpa-tc-${k}"></span></button>`;
    return `<div class="view-head"><h1>Third-party administrators</h1>
      <span class="muted">File, pay and track disputes for the providers and plans you represent</span></div>
      <div id="tpa-org"></div>
      <div class="rpt-tabs" role="tablist" id="tpa-tabs">
        ${tab("disputes", "Disputes", true)}${tab("clients", "Clients", true)}${tab("groups", "Provider groups", true)}${tab("file", "File a dispute", true)}${tab("bulk", "Bulk filing", true)}${tab("team", "Team", true)}${tab("paymethods", "Payment methods", true)}${tab("docreq", "Document requests", true)}${tab("audit", "Audit log", true)}
        ${staff ? tab("oversight", "All TPAs", false) + (onb ? tab("reviews", "Documents to review", false) : "") : ""}
      </div>
      ${onb ? `<div class="card tpa-pkg" id="tpa-pkg-card" style="display:none"><div class="tpa-pkg-head"><h2>Documents for one dispute</h2>
        <button class="mini" onclick="Views.tpaPkgClose()">Close</button></div><div id="tpa-pkg"></div></div>` : ""}
      <div id="tpa-body" style="display:none">

        <section data-tpa-panel="disputes">
          <div class="card"><h2>Your parties' disputes</h2>
            <p class="muted">Every dispute you filed, for every client. Open a case number for its details, payment and documents.</p>
            <div id="tpa-fees"></div>
            <div id="tpa-rollup"></div><div id="tpa-cases-filters"></div>
            <div id="tpa-cases" data-nosearch><p class="muted">Loading…</p></div></div>
        </section>


        <section data-tpa-panel="paymethods" style="display:none">
          <div class="card"><h2>Your organization's cards and bank accounts</h2>
            <p class="muted">Used when you choose "Your organization's card or bank account" at Pay. Each client's own are in the Clients tab (Payment). Saved on Stripe's page — no card or account number is kept here. A bank account (ACH) is verified by Stripe; a debit takes 3–5 business days to clear.</p>
            <div id="tpa-own-methods"><p class="muted">Loading…</p></div></div>
        </section>

        <section data-tpa-panel="groups" style="display:none">
          <div class="card"><div class="cl-head"><h2>Provider groups</h2>
            <span class="cl-head-act"><button class="mini" onclick="Views.tpaGroupAddOpen()">Add a provider group</button></span></div>
            <p class="muted">The practices and health systems your providers belong to. Put a provider in a group from the Clients tab; in a bulk filing, a row's <code>provider_group</code> names one of these.</p>
            <div id="tpa-groups"><p class="muted">Loading…</p></div></div>
        </section>

        <section data-tpa-panel="clients" style="display:none">
          <div class="card">
            <div class="cl-head"><h2>Clients</h2><span class="muted" id="tpa-cl-summary"></span>
              <span class="cl-head-act"><button class="mini" onclick="Views.tpaClientAddOpen()">Add a client</button>
                <button class="mini ghost" onclick="Views.tpaGroupAddOpen()">Add a provider group</button></span></div>
            <p class="muted">The providers and plans you file for, each under the provider group (practice or health system) it belongs to.
              ${onb ? "A client is <b>ready to file</b> once its profile (NPI, TIN, address) and a payment method are set. Documents belong to each dispute: you add them when you file it." : ""}</p>
            <div class="cl-tools">
              <input type="search" placeholder="Find a client" aria-label="Find a client" oninput="Views.tpaClientFilter({ q: this.value })" />
              <select aria-label="Show" onchange="Views.tpaClientFilter({ show: this.value })">
                <option value="">All clients</option><option value="setup">Needs setting up</option><option value="ready">Ready to file</option>
                <option value="owes">Fees owed</option><option value="suspended">Suspended</option></select></div>
            <div id="tpa-clients"><p class="muted">Loading…</p></div>
            ${onb ? `<details class="tpa-more"><summary>Add many clients from a spreadsheet (CSV)</summary>
              <p class="muted">Columns: <code>party_name,party_type,contact_email,npi,tin,address</code>. Each row is checked on its own; the report lists every row.</p>
              <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaImportFn(this)">
                <input name="file" type="file" accept=".csv,text/csv" required />
                <button class="mini">Import clients</button></form>
              <div id="tpa-import-result"></div></details>` : ""}
          </div>
        </section>

        <section data-tpa-panel="file" style="display:none">
          <div class="card"><h2>File one dispute</h2>
            <p class="muted">${onb ? "Pick the client and add this dispute's documents. To file many disputes, each with its own documents, use Bulk filing." : "Pick the client and enter the disputed amount."}</p>
            <form class="inline-form" data-task="file_dispute" onsubmit="event.preventDefault(); Views.tpaFileFn(this)">
              <select name="client_id" id="tpa-file-client" required onchange="Views.tpaFileDocsReset()"></select>
              <input type="hidden" name="package_id" id="tpa-file-package" />
              <input name="contact_name" placeholder="client contact for this dispute" />
              <input name="amount" type="number" step="0.01" min="0" placeholder="disputed amount $" />
              <input name="other_party_name" placeholder="health plan the dispute is against" required />
              <input name="other_party_email" type="email" data-email-check placeholder="health plan's email for notices" required />
              <input name="other_party_naic" placeholder="plan NAIC (optional)" />
              <select name="provider_type" id="tpa-file-ptype" title="What kind of provider the client is — used to check the dispute meets the minimum amount"><option value="">Provider type — not sure</option></select>
              <select name="in_network" title="Whether the client has a contract with this health plan"><option value="">In the plan's network? — not sure</option><option value="no">Not in the plan's network</option><option value="yes">In the plan's network</option></select>
              <input name="notes" placeholder="notes" />
              <button class="mini">File dispute</button></form>
            <div id="tpa-file-prefill"></div>
            <div id="tpa-file-result" aria-live="assertive"></div>
            ${onb ? `<div class="file-docs"><b>Documents for this dispute</b> <span class="muted">— optional: every document the dispute relies on, as many as it needs — claim forms, EOBs and denials, the itemized bill, proof of coverage, the Florida license, the contract, medical records, correspondence… Each one is read and flagged if it doesn't look like what it's filed as; nothing is refused for that, and the reviewer checks them.</span>
              <div id="tpa-file-docs"></div></div>` : ""}
            ${packetBlockHtml("tpa-packet", "<b>Filing packet</b> — fill in the packet's sections here, or pick the filled packet PDF")}
            ${(setTimeout(() => packetInit("tpa-packet", {}), 0), "")}
            <p class="muted">Provider type and network status decide the minimum amount a dispute must reach. If you're not sure, leave them: we read them from the documents where they say, and the reviewer confirms.</p>
            <p class="muted">You pay the fee in the portal (Pay now on the dispute, or Pay all). Once the reviewer finds it eligible, the health plan is emailed that the dispute is against it and sees it on its dashboard.</p></div>
        </section>

        <section data-tpa-panel="bulk" style="display:none">
          ${onb ? `<div class="card"><h2>File many disputes at once</h2>
            <p class="muted">Upload one ZIP with a list of the disputes and a folder of documents for each. Every dispute is filed by itself as soon as its documents have been read — whatever documents it has, as many as it relies on. A document that doesn't look like what it's named is flagged for the reviewer, not refused.</p>
            <details class="tpa-more" open><summary>How to prepare the ZIP</summary>
              <div class="tpa-zip-howto">
                <pre class="tpa-tree">disputes.zip
├── disputes.csv
├── CLAIM-001/            ← one folder per dispute, named after its dispute_ref
│   ├── eob.pdf
│   ├── itemized_bill.pdf
│   ├── proof_of_coverage.pdf
│   ├── open_negotiation_notice.pdf
│   ├── offer_substantiation.pdf
│   └── idre_attestation.pdf
│   └── medical_records.pdf ← any other document, under any name
└── CLAIM-002/
    └── merged.pdf        ← or one PDF holding them</pre>
                <p class="muted" style="grid-column:1/-1;margin:0">The six named files are the usual documents, not a limit or a requirement: put in every document the dispute relies on and leave out any that don't apply.</p>
                <div><p>Any other file in a dispute's folder (medical records, the contract, claim forms, the Florida license…) goes onto that dispute too, as one more of its documents.</p><p><b>disputes.csv</b> — one row per dispute:</p>
                  <pre class="tpa-tree">dispute_ref,provider_group,party_name,disputed_amount,other_party_name,other_party_email,other_party_naic,contact_name,notes,provider_type,in_network
CLAIM-001,Gulf Coast Medical Group,Gulf Coast Clinic,1980,Sunshine Health,disputes@sunshine.test,12345,Dr Lee,ER visit 08/14,physician_dentist,no
CLAIM-002,,Bay Imaging,2450.00,Bay Health Plan,claims@bayhealth.test,,,,,</pre>
                  <p class="muted"><code>party_name</code> must match a client in the Clients tab that is ready (profile and payment done). <code>provider_group</code> is the provider group that client is in (optional): fill it in when two of your providers share a name — a provider that isn't in the group named is refused, so nothing is filed for the wrong provider. <code>other_party_name</code> and <code>other_party_email</code> name the health plan each dispute is against and where its notices go. <code>provider_type</code> (<code>hospital_inpatient</code>, <code>hospital_outpatient</code>, <code>physician_dentist</code>, <code>rural_hospital</code> or <code>other</code>) and <code>in_network</code> (<code>yes</code> or <code>no</code>) are optional: leave them blank if you're not sure. Up to 500 disputes per ZIP.</p>
                  <button type="button" class="mini" onclick="Views.tpaBdTemplate(this)">Download disputes.csv with my ready clients</button></div></div>
              <div class="tpa-samples"><p><b>Sample ZIPs to copy</b> — open one to see the exact layout. They are filled in with your clients where you have them;
                every sample document says what the real one must show. Replace the sample PDFs before uploading: a ZIP that still holds them is refused.</p>
                <button type="button" class="mini" onclick="Views.tpaBdSample('separate', this)">Download sample — six separate documents</button>
                <button type="button" class="mini" onclick="Views.tpaBdSample('merged', this)">Download sample — one merged PDF per dispute</button></div></details>
            <p class="muted upl-rules">Accepted: PDF up to 100 MB · Word, Excel, PowerPoint up to 50 MB · images up to 30 MB · text and CSV up to 10 MB · ZIP up to 512 MB (at most 2,000 files, no passwords, no archives inside). Every file is scanned for viruses. Files over 8 MB go up in parts you can pause and resume.</p>
            <form class="inline-form" data-task="bulk_filing" onsubmit="event.preventDefault(); Views.tpaBulkDisputesFn(this)">
              <input name="batch_ref" placeholder="your batch name, e.g. BATCH-2026-10-14" required style="min-width:16em" />
              <input name="file" type="file" accept=".zip,application/zip" required />
              <button class="mini">Upload disputes ZIP</button></form>
            <div id="tpa-bd-result"></div></div>
          <div class="card"><h2>Your bulk filings</h2>
            <div id="tpa-bd-batches"><p class="muted">Loading…</p></div>
            <div id="tpa-bd-report"></div></div>` : `
          <div class="card"><h2>Bulk upload — file many disputes at once</h2>
          <p class="muted">CSV with header row: <code>email</code> (required), <code>contact_name</code>, <code>org</code>, <code>amount</code>, <code>filing_party_type</code> (PROVIDER/HEALTH_PLAN), <code>external_ref</code>, <code>notes</code>. Up to 500 rows per batch.</p>
          <form class="inline-form" onsubmit="event.preventDefault()">
            <input id="tpa-bulk-ref" placeholder="batch reference (your idempotency key)" required style="min-width:16em" />
            <input id="tpa-bulk-file" type="file" accept=".csv,text/csv" />
            <button class="mini" id="tpa-bulk-btn" disabled>File batch</button></form>
          <div id="tpa-bulk-preview"></div><div id="tpa-bulk-result"></div></div>
        <div class="card"><h2>My batch filings — status</h2>
          <div id="tpa-batches"><p class="muted">Loading…</p></div></div>`}
        </section>

        <section data-tpa-panel="team" style="display:none"><div id="tpa-team"></div></section>
        <section data-tpa-panel="docreq" style="display:none"><div class="card"><h2>Document requests</h2>
          <p class="muted">Documents the review team asked you for, on the disputes you filed. Upload and send them here.</p>
          <div id="tpa-docreq"><p class="muted">Loading…</p></div></div></section>
        <section data-tpa-panel="audit" style="display:none"><div class="card"><h2>Audit log</h2>
          <p class="muted">Everything done in your organization's account — by your team, by the office on your behalf, and by the system — newest first. Each entry is chained to the one before it, so a removed or altered entry shows.</p>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.orgAuditBox('tpa-audit', 'tpa')">
            <input name="q" placeholder="Search: dispute, person, action" aria-label="Search the audit log">
            <label class="muted"><input type="checkbox" name="views" onchange="Views.orgAuditBox('tpa-audit', 'tpa')"> Include page views</label>
            <button>Search</button></form>
          <div id="tpa-audit" data-nosearch><p class="muted">Loading…</p></div></div></section>
      </div>
      ${staff ? `
      <section data-tpa-panel="oversight" style="display:none">
        <div class="card"><h2>All TPAs in this program</h2><div id="tpa-admin"><p class="muted">Loading…</p></div></div></section>
      ${onb ? `<section data-tpa-panel="reviews" style="display:none">
        <div class="card"><h2>Documents waiting for review</h2>
          <p class="muted">Documents the reader could not decide on. Open each one, then accept it or say why it is rejected — the filer sees the note.</p>
          <div id="tpa-reviews" class="onb-reviews"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>Possible exclusion-list matches</h2>
          <p class="muted">These clients match an entry on the HHS-OIG List of Excluded Individuals/Entities by NPI or by name. Compare the details (address, specialty, dates) and decide: <b>Clear</b> when it is a different provider, <b>Excluded</b> when it is the same one. Either way, say how you decided. The client can't file until then.</p>
          <div id="tpa-ex-reviews" class="onb-reviews"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>NPIs waiting for review</h2>
          <p class="muted">The national NPI registry holds these NPIs under a different name than the client's. Confirm when it is the same provider (a former or trading name); reject with a note when it is not — the TPA sees the note and must enter the right NPI. The client can't file until then.</p>
          <div id="tpa-npi-reviews" class="onb-reviews"><p class="muted">Loading…</p></div></div></section>` : ""}` : ""}`;
  }

  // TPA page tabs. The workspace tabs show once the account is linked to a
  // TPA; staff also get the program-wide tabs. The choice is remembered.
  let tpaTabCur = "";
  // auto: chosen by the page (start-up, "not acting for a TPA"), not by a
  // click -- not remembered, or it would win over the workspace next time.
  function tpaTab(k, auto) {
    const btn = document.querySelector(`#tpa-tabs [data-tab="${k}"]`);
    if (!btn || btn.style.display === "none") return;
    tpaTabCur = k;
    if (!auto) try { localStorage.setItem("idre.tpaTab", k); } catch { /* private window */ }
    document.querySelectorAll("#tpa-tabs [data-tab]").forEach((b) => b.classList.toggle("active", b.dataset.tab === k));
    document.querySelectorAll("[data-tpa-panel]").forEach((p) => { p.style.display = p.dataset.tpaPanel === k ? "" : "none"; });
    // Opening a tab shows it as it is now: disputes file and payments settle in
    // the background, so what was loaded with the page goes stale.
    if (tpaTab._booted) ({
      disputes: () => { tpaFeesBox(); tpaDashBox(); },
      clients: () => tpaClientsBox(),
      groups: () => tpaGroupsBox(),
      bulk: () => tpaBdBatchesBox(),
      reviews: () => { onboardingReviewsBox("tpa-reviews", "tpa"); npiReviewsBox(); exclusionReviewsBox("tpa-ex-reviews", "tpa"); },
      oversight: () => tpaAdminBox(),
      audit: () => orgAuditBox("tpa-audit", "tpa"),
      docreq: () => orgDocReqBox("tpa-docreq", "tpa"),
      paymethods: () => tpaOwnMethodsBox(),
    }[k] || (() => {}))();
    if (k === "paymethods" && !tpaTab._booted) tpaOwnMethodsBox();
    if (k === "groups" && !tpaTab._booted) tpaGroupsBox();
    if (k === "audit" && !tpaTab._booted) orgAuditBox("tpa-audit", "tpa");
    if (k === "docreq" && !tpaTab._booted) orgDocReqBox("tpa-docreq", "tpa");
    tpaTab._booted = true;
  }
  function tpaTabCount(k, text) {
    const el = document.getElementById("tpa-tc-" + k);
    if (el) el.textContent = text ? String(text) : "";
  }
  const ONB_STATUS = { PROFILE_INCOMPLETE: "Needs profile", NPI_REVIEW: "NPI under review", NPI_REJECTED: "NPI rejected", EXCLUSION_REVIEW: "Exclusion check under review", EXCLUDED: "On the exclusion list", PAYMENT_PENDING: "Needs payment", DOCS_PENDING: "Ready", READY: "Ready" };
  // The national NPI registry's answer on a client's NPI.
  function npiLine(c) {
    if (!c.npi) return "";
    const reg = c.npi_registry_name && c.npi_registry_name !== "(test NPI)" ? ` — registry: ${esc(c.npi_registry_name)}` : "";
    const st = { VERIFIED: ["npi-verified", "NPI verified"], NEEDS_REVIEW: ["npi-review", "NPI under review"], REJECTED: ["npi-rejected", "NPI rejected"],
      UNVERIFIED: ["npi-unverified", "NPI not checked yet"] }[c.npi_status] || ["", "NPI " + esc(c.npi)];
    const why = c.npi_status === "NEEDS_REVIEW" ? `<div class="muted">The registry holds NPI ${esc(c.npi)} under another name${reg}. The review team confirms it before filing can go ahead.</div>`
      : c.npi_status === "REJECTED" ? `<div class="error">${esc(c.npi_review_note || "Rejected by the review team")}. Enter the correct NPI under Profile.</div>`
      : c.npi_status === "UNVERIFIED" ? `<div class="muted">The registry could not be reached; it is checked again automatically.</div>`
      : `<div class="muted">${esc(c.npi)}${reg}</div>`;
    return `<div><span class="badge s-${st[0]}">${st[1]}</span>${why}</div>${checksLine(c)}`;
  }
  // The other checks: the federal exclusion list, and the TIN.
  function checksLine(c) {
    const ck = c.checks || {};
    const ex = ck.EXCLUSION && ck.EXCLUSION.status, tin = ck.TIN && ck.TIN.status;
    const exb = { CLEAR: ["ck-clear", "Not on the exclusion list"], CLEARED: ["ck-clear", "Exclusion list: cleared by the review team"],
      NEEDS_REVIEW: ["ck-review", "Possible exclusion-list match — under review"], EXCLUDED: ["ck-excluded", "On the federal exclusion list"],
      UNAVAILABLE: ["ck-unavailable", "Exclusion list not checked yet"] }[ex];
    const parts = [];
    if (exb) parts.push(`<span class="badge s-${exb[0]}" title="HHS-OIG List of Excluded Individuals/Entities">${exb[1]}</span>`);
    if (ex === "EXCLUDED" && ck.EXCLUSION.note) parts.push(`<div class="error">${esc(ck.EXCLUSION.note)}</div>`);
    if (ex === "UNAVAILABLE") parts.push('<div class="muted">The list could not be loaded yet; it is checked automatically once it is.</div>');
    if (tin === "NOT_CONFIGURED") parts.push('<div class="muted">TIN: format checked · matching against IRS records is not set up</div>');
    return parts.length ? `<div class="checks-line">${parts.join("")}</div>` : "";
  }
  const PKG_STATUS = { ASSEMBLING: "started", VALIDATING: "reading the documents", REJECTED: "could not be read", READY: "ready to file", FILED: "filed" };
  const onbStatus = (st) => `<span class="badge s-${esc(String(st || "")).toLowerCase().replace(/_/g, "-")}">${esc(ONB_STATUS[st] || st || "—")}</span>`;
  function tpaPkgClose() {
    const card = document.getElementById("tpa-pkg-card");
    if (card) card.style.display = "none";
  }


  async function tpaBoot() {
    const orgBox = document.getElementById("tpa-org");
    try {
      // Finance sees the oversight table only; it never acts as a TPA.
      if (!can("TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) { orgBox.innerHTML = ""; tpaAdminBox(); return; }
      const r = await Api.program.tpaMe();
      const code = r.tpa.invite_code;
      const isStaff = can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
      orgBox.innerHTML = `<div class="tpa-org-line"><span class="tpa-org-name">${esc(r.tpa.name)}</span>
        <span class="muted">${esc(r.tpa.contact_email)}</span> ${badge(r.tpa.status)}
        ${App.feature("entity360") && isStaff ? `<button class="mini" onclick="CrmViews.openEntity360('tpas','${r.tpa.id}')">Entity 360</button>` : ""}
        ${isStaff ? `<button class="mini" onclick="Views.tpaUnlinkFn()">Stop acting as this TPA</button>` : ""}</div>`;
      document.getElementById("tpa-team").innerHTML = `
        <div class="card"><h2>Employees</h2>
          <p class="muted">Everyone at ${esc(r.tpa.name)} who signs in to this portal. <b>Admins</b> add and remove employees; <b>Staff</b> file, pay and track disputes. Each employee gets their own login, emailed to them.</p>
          <div id="tpa-team-list"><p class="muted">Loading…</p></div>
          <div id="tpa-team-add"></div></div>
        <div class="card"><details class="tpa-more"><summary>Already have a portal login? Join with a code</summary>
          <p class="muted">A colleague who already has a portal account can join your organization with this code — Profile → Claim TPA. Generating a new code replaces the old one.</p>
          <p id="tpa-invite-code">${code ? `<span class="mono" style="font-weight:600;font-size:1.1em">${esc(code)}</span> <button class="mini" onclick="Views.tpaInviteCodeFn()">Generate new code</button>`
            : `<span class="muted">No code pending.</span> <button class="mini" onclick="Views.tpaInviteCodeFn()">Generate invite code</button>`}</p></details></div>`;
      tpaTeamBox();
      const tpaBody = document.getElementById("tpa-body");
      if (!tpaBody) return; // the page was left while it loaded
      tpaBody.style.display = "";
      document.querySelectorAll("#tpa-tabs .tpa-ws").forEach((b) => { b.style.display = ""; });
      let saved = ""; try { saved = localStorage.getItem("idre.tpaTab") || ""; } catch { /* private window */ }
      tpaTab(saved && document.querySelector(`#tpa-tabs [data-tab="${saved}"]`) ? saved : "disputes", true);
      tpaGroupsBox().then(tpaClientsBox); tpaDashBox(); tpaFeesBox(); if (App.feature("tpa_onboarding")) tpaBdBatchesBox(); else tpaBatchesBox();
      feeReturn("tpa", () => { tpaFeesBox(); tpaDashBox(); if (App.feature("tpa_onboarding")) tpaBdBatchesBox(); });
      tpaSetupReturn();
      if (App.feature("tpa_onboarding")) tpaOnboardingBox();
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
    if (can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
      tpaAdminBox(); onboardingReviewsBox("tpa-reviews", "tpa"); npiReviewsBox(); exclusionReviewsBox("tpa-ex-reviews", "tpa");
      if (document.getElementById("tpa-body")?.style.display === "none") tpaTab("oversight", true);  // not acting for a TPA
    }
  }

  // Where each client stands is part of the Clients list now (tpaClientsBox).
  async function tpaOnboardingBox() { await tpaClientsBox(); }

  async function tpaImportFn(form) {
    const out = document.getElementById("tpa-import-result");
    try {
      const r = await Api.program.tpaImportClients(form.file.files[0]);
      out.innerHTML = `<p class="ok">Imported ${r.imported} of ${r.total} rows.</p>
        <table class="tbl"><thead><tr><th>Row</th><th>Party</th><th>Result</th></tr></thead><tbody>
        ${(r.rows || []).map((x) => `<tr><td>${x.row}</td><td>${esc(x.party_name || "—")}</td>
          <td>${x.ok ? badge("IMPORTED") + (x.npi_status === "NEEDS_REVIEW" ? ' <span class="badge s-npi-review">NPI under review</span>' : x.npi_status === "UNVERIFIED" ? ' <span class="badge s-npi-unverified">NPI not checked yet</span>' : "") : `<span class="error">${esc(x.error || "failed")}</span>`}</td></tr>`).join("")}</tbody></table>`;
      form.reset(); tpaClientsBox();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaProfileForm(clientId) {
    const v = await UI.modal({ title: "Client profile", submitLabel: "Save profile", fields: [
      { name: "npi", label: "NPI (10 digits, checked against the national NPI registry)", required: true, placeholder: "1234567893" },
      { name: "tin", label: "TIN (9 digits)", required: true, placeholder: "123456789" },
      { name: "address", label: "Address", type: "textarea", required: true },
    ]});
    if (!v) return;
    try {
      const r = await Api.program.tpaClientProfile(clientId, {
        npi: v.npi.trim(), tin: v.tin.trim(), address: v.address.trim() });
      tpaClientsBox();
      const npiMsg = { VERIFIED: "NPI verified in the national registry.",
        NEEDS_REVIEW: `The registry holds this NPI under another name${r.npi_registry_name ? ` (${r.npi_registry_name})` : ""}. The review team will confirm it; until then this client can't file.`,
        UNVERIFIED: "The NPI registry could not be reached; the NPI is checked again automatically." }[r.npi_status] || "";
      UI.toast(`Profile saved. ${npiMsg}`, r.npi_status === "NEEDS_REVIEW" ? { kind: "warn" } : undefined);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaPaymentForm(clientId) {
    let ms = [];
    try { ms = (await Api.program.tpaSavedMethods(clientId)).payment_methods || []; } catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    const { scrim } = payScrim(`<div class="modal-h"><h2>This client's cards and bank accounts</h2><button class="icon-btn modal-x" aria-label="Close dialog">✕</button></div>
      <div class="modal-b"><p class="modal-body">When you pay "from each client's own", this client's disputes are charged here. Cards and bank accounts are saved on Stripe's page; a bank account is verified by Stripe and the debit authorization is recorded there.</p>
        ${methodList(ms, clientId)}
        <div class="modal-f"><button type="button" class="btn-secondary m-cancel">Close</button>
          <button type="button" class="btn-primary" onclick="Views.tpaMethodAdd('${esc(clientId)}')">Add a card or bank account</button></div></div>`);
    scrim.querySelector(".btn-primary")?.focus();
  }

  async function tpaPackageStart(clientId, mode) {
    try {
      const r = await Api.program.tpaCreatePackage(clientId, mode);
      tpaPackageBox(r.package_id);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaPackageBox(packageId) {
    const card = document.getElementById("tpa-pkg-card");
    const box = document.getElementById("tpa-pkg");
    if (!card || !box) return;
    card.style.display = "";
    if (!tpaPackageBox._open || tpaPackageBox._open !== packageId) { tpaPackageBox._open = packageId; card.scrollIntoView({ behavior: "smooth", block: "start" }); }
    try {
      const p = await Api.program.tpaPackage(packageId);
      // Until the set is filed, any slot can be uploaded, replaced or removed; every earlier file is kept and shown.
      const open = p.status !== "FILED";
      const when = (t) => (t ? fmtDate(t) : "");
      // The automatic check flags a document that doesn't look like its slot; it never stops the filing.
      const checkBadge = (st) => st === "MISSING" ? `<span class="badge">optional — not added</span>` : st === "ACCEPTED" ? `<span class="badge s-ck-clear">looks right</span>`
        : st === "REJECTED" || st === "NEEDS_REVIEW" ? `<span class="badge s-ck-review" title="The reviewer looks at it; it does not stop the filing">flagged</span>` : badge(st);
      const verdict = (s) => `${checkBadge(s.status)}${s.uploaded_by ? `<div class="muted">${esc(s.uploaded_by)}${s.uploaded_at ? " · " + when(s.uploaded_at) : ""}</div>` : ""}`;
      const why = (rs) => (rs || []).map((r2) => esc(r2.detail || r2.code)).join("; ");
      const change = (s) => !open ? "" : `<label class="mini button-like">${s.status === "MISSING" ? "Add…" : "Replace…"}
          <input type="file" hidden accept=".pdf,application/pdf,image/*" onchange="Views.tpaSlotUpload('${packageId}','${s.slot}',this,'${esc(s.status)}')" /></label>
        ${s.status !== "MISSING" ? `<button type="button" class="mini ghost" onclick="Views.tpaSlotRemove('${packageId}','${s.slot}','${esc(s.filename || "")}')">Remove</button>` : ""}`;
      const history = (s) => (s.history || []).length ? `<tr class="pkg-hist"><td></td><td colspan="4"><details><summary class="muted">Earlier versions (${s.history.length})</summary>
          <table class="tbl" data-nosearch><thead><tr><th>File</th><th>Verdict</th><th>Why</th><th>Uploaded</th><th>What happened</th></tr></thead><tbody>
          ${s.history.map((h) => `<tr><td>${esc(h.filename || "—")}</td><td>${badge(h.status)}</td><td class="muted">${why(h.reasons) || "—"}</td>
            <td class="muted">${esc(h.uploaded_by || "")}${h.uploaded_at ? " · " + when(h.uploaded_at) : ""}</td>
            <td class="muted">${h.ended_how === "REMOVED" ? "Removed" : "Replaced"} by ${esc(h.ended_by || "—")} · ${when(h.ended_at)}</td></tr>`).join("")}
          </tbody></table></details></td></tr>` : "";
      const needsFix = p.slots.filter((s) => ["REJECTED", "NEEDS_REVIEW"].includes(s.status)).length;
      box.innerHTML = `<p>${badge(p.status)} · ${p.mode === "MERGED" ? "merged PDF" : "separate files"} · set <code>${esc(packageId)}</code>
          <button class="mini" onclick="Views.tpaPackageBox('${packageId}')">Refresh</button></p>
        ${needsFix && open ? `<p class="warn-line">${needsFix} document${needsFix === 1 ? " was" : "s were"} flagged by the automatic check — the reason is beside each. That doesn't stop the filing: every document goes onto the dispute and the reviewer looks at the flagged ones. Use <b>Replace…</b> only if it's the wrong file.</p>` : ""}
        ${open ? `<p class="muted">You can upload, replace or remove any document until this set is filed with a dispute. Every earlier file is kept and every change is in your Audit log.</p>`
          : `<p class="muted">This set has been filed with a dispute; its documents can no longer be changed. To send a corrected document, use the dispute's document requests.</p>`}
        ${open ? `<p class="muted"><b>All of these are optional.</b> Add the ones you have, and any other document the dispute relies on below — a dispute can be filed with any of them, or with none.</p>` : ""}
        <table class="tbl" data-nosearch><thead><tr><th>Document</th><th>File</th><th>Check</th><th>Why</th><th>${open ? "Change" : ""}</th></tr></thead><tbody>
        ${p.slots.map((s) => `<tr class="${["REJECTED", "NEEDS_REVIEW"].includes(s.status) ? "pkg-bad" : s.status === "MISSING" ? "pkg-optional" : ""}"><td>${esc(SLOT_NAME[s.slot] || s.slot)} <span class="muted">(optional)</span></td>
          <td>${esc(s.filename || "—")}${s.page_start && !/\(pages /.test(s.filename || "") ? ` <span class="muted">(pages ${s.page_start}-${s.page_end})</span>` : ""}</td>
          <td>${verdict(s)}</td>
          <td class="muted">${why(s.reasons) || "—"}</td>
          <td>${change(s)}</td></tr>${history(s)}`).join("")}</tbody></table>
        ${p.mode === "MERGED" && open ? `
          <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaMergedFn(this,'${packageId}')">
            <input name="file" type="file" accept=".pdf,application/pdf" required />
            <button class="mini">Upload a new merged PDF</button></form>` : ""}
        ${p.status === "READY" ? '<p class="ok">Every document has been read — this set can be used to file a dispute.</p>' : ""}
        ${moreDocsHtml(p, "tpa")}`;
      // The dispute being filed shows where its documents stand.
      if (tpaFileDocs && tpaFileDocs.id === packageId) {
        const six = p.slots.filter((x) => x.status !== "MISSING").length, more = (p.more || []).length;
        if (tpaFileDocs.status !== p.status || tpaFileDocs.six !== six || tpaFileDocs.more !== more) {
          Object.assign(tpaFileDocs, { status: p.status, six, more }); tpaFileDocsDraw();
        }
      }
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaSlotUpload(packageId, slot, input, current) {
    if (!input.files[0]) return;
    if (current === "ACCEPTED" && !(await UI.confirm("Replace an accepted document?",
      `The ${slot.replace(/_/g, " ").toLowerCase()} was accepted. Replacing it sends the set back to be checked; the current file is kept in its history.`, "Replace"))) {
      input.value = ""; return;
    }
    try {
      await Api.program.tpaAttachDoc(packageId, slot, input.files[0]);
      UI.toast(current && current !== "MISSING" ? "Replaced — the new file is being checked" : "Uploaded — the file is being checked");
      setTimeout(() => tpaPackageBox(packageId), 300);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaSlotRemove(packageId, slot, filename) {
    if (!(await UI.confirm("Remove this document?",
      `${filename || "The file"} is taken out of the ${slot.replace(/_/g, " ").toLowerCase()} slot. It stays in the slot's history, and the change is in your Audit log.`, "Remove", true))) return;
    try { await Api.program.tpaRemoveDoc(packageId, slot); UI.toast("Removed"); tpaPackageBox(packageId); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaMergedFn(form, packageId) {
    try {
      await Api.program.tpaUploadMerged(packageId, form.file.files[0]);
      form.reset();
      setTimeout(() => tpaPackageBox(packageId), 1500); // splitter is async
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaZipFn(form) {
    const out = document.getElementById("tpa-zip-result");
    try {
      const r = await Api.program.tpaBulkPackages(form.file.files[0]);
      out.innerHTML = `<table class="tbl"><thead><tr><th>Client</th><th>Package</th><th>Attached</th><th>Errors</th></tr></thead><tbody>
        ${(r.reports || []).map((x) => `<tr><td>${esc(x.client)}</td>
          <td>${x.package_id ? `<code>${esc(x.package_id.slice(0, 8))}</code> ${esc(x.mode)}` : "—"}</td>
          <td>${(x.attached || []).map(esc).join(", ") || "—"}</td>
          <td class="error">${(x.errors || []).map(esc).join("; ") || ""}</td></tr>`).join("")}</tbody></table>`;
      form.reset(); tpaOnboardingBox();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  // ---- Health plan onboarding (feature: healthplan_onboarding) ----

  function hpView() {
    if (!App.feature("healthplan_onboarding"))
      return `<div class="view-head"><h1>Health plans</h1></div><p class="muted">Health plan onboarding is disabled for this program.</p>`;
    if (!can("HEALTH_PLAN", "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"))
      return `<div class="view-head"><h1>Health plans</h1></div><p class="muted">Requires a HEALTH_PLAN, CASE_MANAGER, or FINANCE account.</p>`;
    afterRender(hpBoot);
    const staff = can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const tab = (k, label, ws) => `<button type="button" role="tab" class="rpt-tab rpt-accent-4${ws ? " hp-ws" : ""}" data-tab="${k}" onclick="Views.hpTab('${k}')"${ws ? ' style="display:none"' : ""}>${label} <span class="tab-count" id="hp-tc-${k}"></span></button>`;
    return `<div class="view-head"><h1>Health plans</h1>
      <span class="muted">Answer the disputes filed against your plan, and file disputes for your employer groups, networks and facilities</span></div>
      <div id="hp-org"></div>
      <div class="rpt-tabs" role="tablist" id="hp-tabs">
        ${tab("against", "Disputes against us", true)}${tab("parties", "Your parties", true)}${tab("file", "File a dispute", true)}${tab("docreq", "Document requests", true)}${tab("audit", "Audit log", true)}
        ${staff ? tab("oversight", "All plans", false) + tab("reviews", "Documents to review", false) : ""}
      </div>
      <div id="hp-body" style="display:none">
        <section data-hp-panel="against">
          <div class="card"><h2>Disputes against your plan</h2>
            <p class="muted">Every dispute a provider (or its TPA) filed against your plan. Open one to see what was submitted, add your documents — your QPA methodology, the EOB, the contract — and submit your response before the deadline.</p>
            <div id="hp-rsp-list"><p class="muted">Loading…</p></div></div>
          <div class="card tpa-pkg" id="hp-rsp-panel" style="display:none"></div>
        </section>
        <section data-hp-panel="parties" style="display:none">
        <div class="card"><h2>Parties you submit for</h2>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.hpAddPartyFn(this)">
            <input name="party_name" placeholder="party organization name" required />
            <select name="party_type"><option value="EMPLOYER_GROUP">Employer group (ASO)</option>
              <option value="DELEGATED_NETWORK">Delegated network / IPA</option>
              <option value="FACILITY">Facility</option></select>
            <input name="contact_email" type="email" placeholder="contact email" />
            <button class="mini">Add</button></form>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.hpImportFn(this)">
            <input name="file" type="file" accept=".csv,text/csv" required />
            <button class="mini">Import roster (CSV)</button></form>
          <p class="muted">CSV header: <code>party_name,party_type,contact_email,npi,tin,address</code>. NPI is required for networks/facilities. Every row is validated on its own.</p>
          <div id="hp-import-result"></div></div>
        <div class="card"><h2>Onboarding — readiness board</h2>
          <p class="muted">Each party needs a verified profile, a payment rail (card or ACH), and a validated six-document package before you can submit for them.</p>
          <div id="hp-onboarding"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>Onboarding — bulk packages (ZIP)</h2>
          <p class="muted">One ZIP, many parties: top-level folder = party name. Inside each folder, either the six slot-named files (<code>eob.pdf</code>, <code>itemized_bill.pdf</code>, <code>proof_of_coverage.pdf</code>, <code>open_negotiation_notice.pdf</code>, <code>offer_substantiation.pdf</code>, <code>idre_attestation.pdf</code>) or one <code>merged.pdf</code> — split and validated page by page. Plan-side offer substantiation must document the QPA methodology.</p>
          <p class="muted upl-rules">Accepted: PDF up to 100 MB · Word, Excel, PowerPoint up to 50 MB · images up to 30 MB · text and CSV up to 10 MB · ZIP up to 512 MB (at most 2,000 files, no passwords, no archives inside). Every file is scanned for viruses. Files over 8 MB go up in parts you can pause and resume.</p>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.hpZipFn(this)">
            <input name="file" type="file" accept=".zip,application/zip" required />
            <button class="mini">Upload packages ZIP</button></form>
          <div id="hp-zip-result"></div></div>
        <div class="card" id="hp-pkg-card" style="display:none"><h2>Package — slot verdicts</h2>
          <div id="hp-pkg"></div></div>
        </section>
        <section data-hp-panel="file" style="display:none">
        <div id="hp-fees"></div>
        <div class="card"><h2>Submit a dispute on behalf of a party</h2>
          <form class="inline-form" data-task="file_dispute" onsubmit="event.preventDefault(); Views.hpFileFn(this)">
            <select name="party_id" id="hp-file-party" required onchange="Views.filingPackagesFilter('hp-file-party','hp-file-package','party')"></select>
            <select name="package_id" id="hp-file-package" onchange="if (this.value) Views.filePrefillFn('hp', this.form, this.value, true)"><option value="">— no package —</option></select>
            <input name="amount" type="number" step="0.01" min="0" placeholder="disputed amount $" />
            <input name="other_party_name" placeholder="provider the dispute is against" required />
            <input name="other_party_email" type="email" data-email-check placeholder="provider's email for notices" required />
            <input name="other_party_npi" placeholder="provider NPI (optional)" />
            <input name="notes" placeholder="notes" />
            <button class="mini">Submit dispute</button></form>
          <div id="hp-file-prefill"></div>
          <div id="hp-file-result" aria-live="assertive"></div>
          ${packetBlockHtml("hp-packet", "<b>Filing packet</b> — fill in the packet's sections here, or pick the filled packet PDF")}
          ${(setTimeout(() => packetInit("hp-packet", {}, { partyFixed: "HEALTH_PLAN" }), 0), "")}
          <p class="muted">You pay the fee in the portal (Pay now on the dispute, or Pay all above) and manage documents on behalf of the party. Once the reviewer finds it eligible, the provider is emailed that the dispute is against it, with a secure link to send its response.</p>
          </div>
        </section>
        <section data-hp-panel="docreq" style="display:none"><div class="card"><h2>Document requests</h2>
          <p class="muted">Documents the review team asked you for. Upload and send them here.</p>
          <div id="hp-docreq"><p class="muted">Loading…</p></div></div></section>
        <section data-hp-panel="audit" style="display:none"><div class="card"><h2>Audit log</h2>
          <p class="muted">Everything done in your organization's account — by your team, by the office on your behalf, and by the system — newest first. Each entry is chained to the one before it, so a removed or altered entry shows.</p>
          <form class="inline-form" onsubmit="event.preventDefault(); Views.orgAuditBox('hp-audit', 'hp')">
            <input name="q" placeholder="Search: dispute, person, action" aria-label="Search the audit log">
            <label class="muted"><input type="checkbox" name="views" onchange="Views.orgAuditBox('hp-audit', 'hp')"> Include page views</label>
            <button>Search</button></form>
          <div id="hp-audit" data-nosearch><p class="muted">Loading…</p></div></div></section>
      </div>
      ${staff ? `
      <section data-hp-panel="oversight" style="display:none">
        <div class="card"><h2>All health plans in this program</h2><div id="hp-admin"><p class="muted">Loading…</p></div></div></section>
      <section data-hp-panel="reviews" style="display:none">
        <div class="card"><h2>Documents waiting for review</h2><div id="hp-reviews" class="onb-reviews"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>Possible exclusion-list matches</h2>
          <p class="muted">These parties match an entry on the HHS-OIG exclusion list by NPI or by name. Clear it when it is a different provider, mark it Excluded when it is the same one, and say how you decided. The party can't file until then.</p>
          <div id="hp-ex-reviews" class="onb-reviews"><p class="muted">Loading…</p></div></div>
        <div class="card"><h2>NPIs waiting for review</h2>
          <p class="muted">The national NPI registry holds these networks' and facilities' NPIs under a different name. Confirm when it is the same provider; reject with a note when it is not — the health plan sees the note. The party can't file until then.</p>
          <div id="hp-npi-reviews" class="onb-reviews"><p class="muted">Loading…</p></div></div></section>` : ""}`;
  }

  function hpTab(k, auto) {
    const btn = document.querySelector(`#hp-tabs [data-tab="${k}"]`);
    if (!btn || btn.style.display === "none") return;
    if (!auto) try { localStorage.setItem("idre.hpTab", k); } catch { /* private window */ }
    document.querySelectorAll("#hp-tabs [data-tab]").forEach((b) => b.classList.toggle("active", b.dataset.tab === k));
    document.querySelectorAll("[data-hp-panel]").forEach((p) => { p.style.display = p.dataset.hpPanel === k ? "" : "none"; });
    if (hpTab._booted) ({
      against: () => respondListBox("hp"),
      parties: () => hpOnboardingBox(),
      file: () => hpOnboardingBox(),
      reviews: () => { onboardingReviewsBox("hp-reviews", "hp"); npiReviewsBox("hp-npi-reviews", "hp"); exclusionReviewsBox("hp-ex-reviews", "hp"); },
      oversight: () => hpAdminBox(),
      audit: () => orgAuditBox("hp-audit", "hp"),
      docreq: () => orgDocReqBox("hp-docreq", "hp"),
    }[k] || (() => {}))();
    if (k === "audit" && !hpTab._booted) orgAuditBox("hp-audit", "hp");
    if (k === "docreq" && !hpTab._booted) orgDocReqBox("hp-docreq", "hp");
    hpTab._booted = true;
  }
  function hpTabCount(k, text) {
    const el = document.getElementById("hp-tc-" + k);
    if (el) el.textContent = text ? String(text) : "";
  }


  async function hpBoot() {
    const orgBox = document.getElementById("hp-org");
    try {
      const r = await Api.program.hpMe();
      orgBox.innerHTML = `<div class="stat-grid"><div class="stat"><div class="stat-num">${esc(r.health_plan.name)}</div>
        <div class="muted">${esc(r.health_plan.contact_email)} · ${r.health_plan.naic ? "NAIC " + esc(r.health_plan.naic) + " · " : ""}${esc(r.health_plan.line_of_business || "")} ${badge(r.health_plan.status)}</div>
        ${App.feature("entity360") && can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") ? `<button class="mini" style="margin-top:8px" onclick="CrmViews.openEntity360('health_plans','${r.health_plan.id}')">Entity 360</button>` : ""}
        ${can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") && !can("HEALTH_PLAN") ? `<button class="mini" style="margin-top:8px" onclick="Views.hpUnlinkFn()">Stop acting as this plan</button>` : ""}</div></div>`;
      const hpBody = document.getElementById("hp-body");
      if (!hpBody) return; // the page was left while it loaded
      hpBody.style.display = "";
      document.querySelectorAll("#hp-tabs .hp-ws").forEach((b) => { b.style.display = ""; });
      let saved = ""; try { saved = localStorage.getItem("idre.hpTab") || ""; } catch { /* private window */ }
      hpTab(saved && document.querySelector(`#hp-tabs [data-tab="${saved}"]`) ? saved : "against", true);
      hpOnboardingBox(); respondListBox("hp"); hpFeesBox();
      feeReturn("hp", () => { hpFeesBox(); hpOnboardingBox(); });
    } catch (e) {
      orgBox.innerHTML = `<div class="card"><h2>Link your health plan organization</h2>
        <p class="muted">Enter the one-time claim code from your health plan registration.</p>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.hpClaimFn(this)">
          <input name="code" placeholder="HP-XXXXXX" required /><button class="mini">Claim</button></form></div>`;
    }
    if (can("CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
      hpAdminBox(); onboardingReviewsBox("hp-reviews", "hp");
      if (document.getElementById("hp-body")?.style.display === "none") hpTab("oversight", true);  // not acting for a plan
    }
  }

  async function hpClaimFn(form) {
    try { await Api.program.hpClaim(form.code.value.trim()); hpBoot(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function hpAddPartyFn(form) {
    try {
      await Api.program.hpAddParty({ party_name: form.party_name.value, party_type: form.party_type.value, contact_email: form.contact_email.value });
      form.reset(); hpOnboardingBox();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function hpOnboardingBox() {
    const box = document.getElementById("hp-onboarding");
    if (!box) return;
    try {
      const r = await Api.program.hpOnboarding();
      const ps = r.parties || [];
      const readyPkgs = [];
      box.innerHTML = ps.length ? `<table class="tbl"><thead><tr>
          <th>Party</th><th>Profile</th><th>Payment</th><th>Package</th><th>Onboarding</th><th>Actions</th></tr></thead><tbody>
        ${ps.map((c) => {
          (c.open_packages || []).forEach((p) => { if (p.status === "READY") readyPkgs.push({ party: c.party_id, id: p.id, name: c.party_name }); });
          return `<tr><td>${esc(c.party_name)}<br><span class="muted">${badge(c.party_type)}</span></td>
          <td>${c.profile_complete ? badge("COMPLETE") : badge("INCOMPLETE")}${npiLine(c)}</td>
          <td>${c.verified_payments > 0 ? badge(c.verified_payments + " VERIFIED") : badge("NONE")}</td>
          <td>${c.ready_packages > 0 ? badge(c.ready_packages + " READY") : badge("NONE")}</td>
          <td>${badge(c.onboarding_status)}</td>
          <td>
            <button class="mini" onclick="Views.hpProfileForm('${c.party_id}','${c.party_type}')">Profile</button>
            <button class="mini" onclick="Views.hpPaymentForm('${c.party_id}')">Payment</button>
            <button class="mini" onclick="Views.hpPackageStart('${c.party_id}','DISCRETE')">6-file package</button>
            <button class="mini" onclick="Views.hpPackageStart('${c.party_id}','MERGED')">Merged PDF</button>
            ${(c.open_packages || []).map((p) => `<button class="mini" onclick="Views.hpPackageBox('${p.id}')">${esc(p.mode)} ${esc(p.status)}</button>`).join("")}
          </td></tr>`;
        }).join("")}</tbody></table>` : '<p class="muted">No parties yet — add one above or import a roster CSV.</p>';
      const sel = document.getElementById("hp-file-package");
      if (sel) sel.innerHTML = `<option value="">— choose the party\'s READY package —</option>` +
        readyPkgs.map((p) => `<option value="${p.id}" data-party="${p.party}">${esc(p.name)} · READY package ${esc(p.id.slice(0, 8))}</option>`).join("");
      filingPackagesFilter('hp-file-party','hp-file-package','party');
      const psel = document.getElementById("hp-file-party");
      if (psel) psel.innerHTML = ps.filter((c) => c.status === "ACTIVE")
        .map((c) => `<option value="${c.party_id}">${esc(c.party_name)} (${esc(c.party_type)})</option>`).join("");
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function hpImportFn(form) {
    const out = document.getElementById("hp-import-result");
    try {
      const r = await Api.program.hpImportParties(form.file.files[0]);
      out.innerHTML = `<p class="ok">Imported ${r.imported} of ${r.total} rows.</p>
        <table class="tbl"><thead><tr><th>Row</th><th>Party</th><th>Result</th></tr></thead><tbody>
        ${(r.rows || []).map((x) => `<tr><td>${x.row}</td><td>${esc(x.party_name || "—")}</td>
          <td>${x.ok ? badge("IMPORTED") + (x.npi_status === "NEEDS_REVIEW" ? ' <span class="badge s-npi-review">NPI under review</span>' : x.npi_status === "UNVERIFIED" ? ' <span class="badge s-npi-unverified">NPI not checked yet</span>' : "") : `<span class="error">${esc(x.error || "failed")}</span>`}</td></tr>`).join("")}</tbody></table>`;
      form.reset(); hpOnboardingBox();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function hpProfileForm(partyId, partyType) {
    const fields = [
      { name: "tin", label: "TIN (9 digits)", required: true, placeholder: "123456789" },
      { name: "address", label: "Address", type: "textarea", required: true },
    ];
    if (partyType !== "EMPLOYER_GROUP")
      fields.unshift({ name: "npi", label: "NPI (10 digits, checked against the national NPI registry)", required: true, placeholder: "1234567893" });
    const v = await UI.modal({ title: "Party profile", submitLabel: "Save profile", fields });
    if (!v) return;
    try {
      const r = await Api.program.hpPartyProfile(partyId, {
        npi: (v.npi || "").trim(), tin: v.tin.trim(), address: v.address.trim() });
      hpOnboardingBox();
      const npiMsg = { VERIFIED: "NPI verified in the national registry.",
        NEEDS_REVIEW: `The registry holds this NPI under another name${r.npi_registry_name ? ` (${r.npi_registry_name})` : ""}. The review team will confirm it; until then this party can't file.`,
        UNVERIFIED: "The NPI registry could not be reached; the NPI is checked again automatically." }[r.npi_status] || "";
      UI.toast(`Profile saved. ${npiMsg}`, r.npi_status === "NEEDS_REVIEW" ? { kind: "warn" } : undefined);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function hpPaymentForm(partyId) {
    let methods = [];
    try { methods = (await Api.program.hpPayments(partyId)).payment_methods || []; } catch { /* none yet */ }
    if (methods.length) {
      UI.toast("Existing rails:\n" + methods.map((m) =>
        `${m.method} ${m.label || ""} ${m.method === "ACH" ? "routing " + m.routing : m.brand} ····${m.last4} — ${m.verified ? "VERIFIED" : "PENDING"}`).join("\n"));
    }
    const v = await UI.modal({ title: "Payment method on file", submitLabel: "Save payment method", body: "Kept on file for this party. Filing fees are paid by you at checkout.", fields: [
      { name: "method", label: "Method", options: [["CARD", "Credit card"], ["ACH", "ACH debit"]] },
      { name: "label", label: "Label", placeholder: "Employer group billing account" },
      { name: "processor_ref", label: "Card: processor token ref", placeholder: "tok_... (never a raw card number)" },
      { name: "brand", label: "Card brand", placeholder: "VISA" },
      { name: "routing", label: "ACH: routing number (9 digits)", placeholder: "021000021" },
      { name: "last4", label: "Last 4", required: true, placeholder: "1234" },
      { name: "auth_received", label: "ACH: type 'YES' to confirm debit authorization captured", placeholder: "YES" },
    ]});
    if (!v) return;
    try {
      const r = await Api.program.hpAddPayment(partyId, {
        method: v.method, label: v.label, brand: v.brand,
        last4: (v.last4 || "").trim(), routing: (v.routing || "").trim(),
        processor_ref: (v.processor_ref || "").trim(),
        auth_received: (v.auth_received || "").trim().toUpperCase() === "YES" });
      hpOnboardingBox();
      UI.toast("Payment method saved (" + (r.verified ? "verified" : "pending") + ")");
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function hpPackageStart(partyId, mode) {
    try {
      const r = await Api.program.hpCreatePackage(partyId, mode);
      hpPackageBox(r.package_id);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function hpPackageBox(packageId) {
    const card = document.getElementById("hp-pkg-card");
    const box = document.getElementById("hp-pkg");
    if (!card || !box) return;
    card.style.display = "";
    try {
      const p = await Api.program.hpPackage(packageId);
      box.innerHTML = `<p>${badge(p.status)} · mode ${esc(p.mode)} · package <code>${esc(packageId)}</code>
          <button class="mini" onclick="Views.hpPackageBox('${packageId}')">Refresh</button></p>
        ${!["FILED"].includes(p.status) ? `<p class="muted"><b>All of these are optional.</b> Add the ones you have — a dispute can be filed with any of them, or with none.</p>` : ""}
        <table class="tbl"><thead><tr><th>Document</th><th>File</th><th>Check</th><th>Why</th><th>Add</th></tr></thead><tbody>
        ${p.slots.map((s) => `<tr class="${s.status === "MISSING" ? "pkg-optional" : ""}"><td>${esc(SLOT_NAME[s.slot] || s.slot)} <span class="muted">(optional)</span></td>
          <td>${esc(s.filename || "—")}${s.page_start && !/\(pages /.test(s.filename || "") ? ` <span class="muted">(pages ${s.page_start}-${s.page_end})</span>` : ""}</td>
          <td>${s.status === "MISSING" ? '<span class="badge">optional — not added</span>' : ["REJECTED", "NEEDS_REVIEW"].includes(s.status) ? '<span class="badge s-ck-review" title="The reviewer looks at it; it does not stop the filing">flagged</span>' : badge(s.status)}</td>
          <td class="muted">${(s.reasons || []).map((r2) => esc(r2.detail || r2.code)).join("; ") || "—"}</td>
          <td>${p.mode === "DISCRETE" && ["MISSING", "REJECTED", "NEEDS_REVIEW"].includes(s.status)
            ? `<input type="file" id="hppkg-${s.slot}" onchange="Views.hpSlotUpload('${packageId}','${s.slot}',this)" />` : ""}</td>
        </tr>`).join("")}</tbody></table>
        ${p.mode === "MERGED" && !["READY", "FILED"].includes(p.status) ? `
          <form class="inline-form" onsubmit="event.preventDefault(); Views.hpMergedFn(this,'${packageId}')">
            <input name="file" type="file" accept=".pdf,application/pdf" required />
            <button class="mini">Upload merged PDF</button></form>` : ""}
        ${p.status === "READY" ? '<p class="ok">All six documents ACCEPTED — this package can be referenced when submitting.</p>' : ""}
        ${moreDocsHtml(p, "hp")}`;
      // The filing list is filled by the board: redraw it once the package is
      // ready, or a READY package only showed up there after a page reload.
      if (p.status === "READY" || p.status === "FILED") hpOnboardingBox();
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  // Every other document of the dispute, besides its six (tpa_onboarding.go, moreDocKinds): as many as it relies
  // on, each with what it is. Taken as filed and put on the dispute with the six; they never hold a set back.
  const MORE_LABEL = { CLAIM_FORMS: "Claim forms", DENIALS_EOBS: "Denials / EOBs", FLORIDA_LICENSE: "Florida license",
    FINAL_DETERMINATION_LETTER: "Final determination letter", CONTRACT: "Contract", WAIVER_OF_PAYMENT: "Waiver of payment",
    NOTICE_OF_OVERPAYMENT: "Notice of overpayment", OVERPAYMENT_RESPONSE: "Overpayment response", MEDICAL_RECORDS: "Medical records",
    CODING_RULES: "Coding rules / policies", OTHER: "Other" };
  function moreDocsHtml(p, kind) {
    const open = p.status !== "FILED", more = p.more || [], canRemove = open && kind === "tpa";
    const kinds = p.more_kinds || Object.keys(MORE_LABEL);
    return `<h3 class="pkg-more-h">All the dispute's other documents${more.length ? ` <span class="muted">(${more.length})</span>` : ""}</h3>
      <p class="muted">Every other document the dispute relies on: claim forms, denials, the Florida license, the final determination letter, the contract, medical records, coding rules — as many as you have. They go onto the dispute with the rest. They aren't checked one by one and never hold the set back.</p>
      ${more.length ? `<table class="tbl" data-nosearch><thead><tr><th>Document</th><th>File</th><th>Added</th>${canRemove ? "<th></th>" : ""}</tr></thead><tbody>
        ${more.map((d) => `<tr><td>${esc(MORE_LABEL[d.kind] || d.kind)}</td><td>${esc(d.filename || "—")}</td>
          <td class="muted">${esc(d.uploaded_by || "")}${d.uploaded_at ? " · " + fmtDate(d.uploaded_at) : ""}</td>
          ${canRemove ? `<td><button type="button" class="mini ghost" onclick="Views.tpaSlotRemove('${p.package_id}','${d.slot}','${esc((d.filename || "").replace(/'/g, ""))}')">Remove</button></td>` : ""}</tr>`).join("")}</tbody></table>`
        : '<p class="muted">None added yet.</p>'}
      ${open ? `<form class="inline-form" onsubmit="event.preventDefault(); Views.moreDocsAdd(this,'${kind}','${p.package_id}')">
        <select name="kind" aria-label="What the document is">${kinds.map((k) => `<option value="${k}">${esc(MORE_LABEL[k] || k)}</option>`).join("")}</select>
        <input name="file" type="file" multiple accept=".pdf,application/pdf,image/*" required aria-label="Files" />
        <button class="mini">Add documents</button></form>` : ""}`;
  }
  async function moreDocsAdd(form, kind, packageId) {
    const files = [...form.file.files];
    if (!files.length) return;
    const btn = form.querySelector("button");
    btn.disabled = true; btn.textContent = "Adding…";
    let ok = 0;
    for (const f of files) {
      try { await (kind === "hp" ? Api.program.hpAttachDoc : Api.program.tpaAttachDoc)(packageId, "MORE_" + form.kind.value, f); ok++; }
      catch (e) { UI.toast(`${f.name}: ${e.message}`, { kind: "warn", duration: 8000 }); }
    }
    if (ok) UI.toast(`${plural(ok, "document")} added`);
    (kind === "hp" ? hpPackageBox : tpaPackageBox)(packageId);
  }

  async function hpSlotUpload(packageId, slot, input) {
    if (!input.files[0]) return;
    try {
      await Api.program.hpAttachDoc(packageId, slot, input.files[0]);
      setTimeout(() => hpPackageBox(packageId), 300);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function hpMergedFn(form, packageId) {
    try {
      await Api.program.hpUploadMerged(packageId, form.file.files[0]);
      form.reset();
      setTimeout(() => hpPackageBox(packageId), 1500);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function hpZipFn(form) {
    const out = document.getElementById("hp-zip-result");
    try {
      const r = await Api.program.hpBulkPackages(form.file.files[0]);
      out.innerHTML = `<table class="tbl"><thead><tr><th>Party</th><th>Package</th><th>Attached</th><th>Errors</th></tr></thead><tbody>
        ${(r.reports || []).map((x) => `<tr><td>${esc(x.party)}</td>
          <td>${x.package_id ? `<code>${esc(x.package_id.slice(0, 8))}</code> ${esc(x.mode)}` : "—"}</td>
          <td>${(x.attached || []).map(esc).join(", ") || "—"}</td>
          <td class="error">${(x.errors || []).map(esc).join("; ") || ""}</td></tr>`).join("")}</tbody></table>`;
      form.reset(); hpOnboardingBox();
    } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function hpFileFn(form) {
    const out = document.getElementById("hp-file-result");
    const btn = form.querySelector("button");
    out.innerHTML = "";
    const packetHost = document.getElementById("hp-packet");
    const packetProblems = packetHost && PacketForm.touched(packetHost) ? PacketForm.read(packetHost).problems : [];
    if (packetProblems.length) { fileAlert(form, out, "This dispute wasn't filed.", "The filing packet isn't complete:", packetProblems); return; }
    const packet = packetFrom("hp-packet", out);
    if (packet === false) return;
    if (btn) { btn.disabled = true; btn.dataset.label = btn.textContent; btn.textContent = "Filing…"; }
    try {
      const r = await Api.program.hpIntake({
        packet: packet || undefined,
        party_id: form.party_id.value, package_id: form.package_id ? form.package_id.value : "",
        disputed_amount_cents: Math.round(Number(form.amount.value || 0) * 100), notes: form.notes.value,
        other_party_name: form.other_party_name.value, other_party_email: form.other_party_email.value, other_party_npi: form.other_party_npi.value,
      });
      out.innerHTML = `<p class="ok">Filed — case <a href="#/cases/${r.case_id}">${esc(r.case_number)}</a> (${esc(r.status)}) for <b>${esc(r.party)}</b>. ${r.payment_required ? `Pay its fee to open it: <a href="#/cases/${r.case_id}">Pay now</a>.` : ""}</p>`;
      if (window.Telemetry) Telemetry.taskDone("file_dispute", prefillOutcome(form));
      const pn = document.getElementById("hp-file-prefill"); if (pn) pn.innerHTML = "";
      form.reset(); packetInit("hp-packet", {}, { partyFixed: "HEALTH_PLAN" }); hpOnboardingBox();
      out.scrollIntoView({ behavior: "smooth", block: "center" });
      UI.toast(`Dispute ${r.case_number} filed`);
    } catch (e) { if (window.Telemetry) Telemetry.taskError("file_dispute", e.message); fileAlert(form, out, "This dispute wasn't filed.", e.message, e.problems); }
    finally { if (btn) { btn.disabled = false; btn.textContent = btn.dataset.label || "Submit dispute"; } }
  }

  async function hpAdminBox() {
    const box = document.getElementById("hp-admin");
    if (!box) return;
    try {
      const r = await Api.program.adminHealthPlans();
      const hs = r.health_plans || [];
      box.innerHTML = hs.length ? `<table class="tbl"><thead><tr><th>Plan</th><th>NAIC</th><th>LOB</th><th>Parties</th><th>Status</th><th></th></tr></thead><tbody>
        ${hs.map((h) => `<tr><td>${esc(h.name)}<br><span class="muted">${esc(h.contact_email)}</span></td>
          <td>${esc(h.naic || "—")}</td><td>${esc(h.line_of_business || "—")}</td><td>${h.parties}</td>
          <td>${badge(h.status)}</td>
          <td><button class="mini" onclick="Views.hpAdminStatusFn('${h.id}','${h.status === "ACTIVE" ? "SUSPENDED" : "ACTIVE"}')">${h.status === "ACTIVE" ? "Suspend" : "Reactivate"}</button></td></tr>`).join("")}</tbody></table>`
        : '<p class="muted">No health plans registered.</p>';
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function hpAdminStatusFn(id, status) {
    try { await Api.program.adminHealthPlanStatus(id, status); hpAdminBox(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaClaimFn(form) {
    try { await Api.program.tpaClaim(form.code.value.trim()); tpaBoot(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaInviteCodeFn() {
    try { await Api.program.tpaInviteCode(); tpaBoot(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ── Clients: one list, each client under the provider group it belongs to ──
  // The roster (group, email, disputes, status), where each client stands (profile, payment method, NPI and
  // exclusion-list checks; tpa_onboarding) and what it owes (the dashboard), drawn as one row per client.
  const tpaCl = { rows: [], q: "", show: "", onb: false };
  const tpaClReady = (c) => c.status === "ACTIVE" && (!tpaCl.onb || ["READY", "DOCS_PENDING"].includes(c.onb && c.onb.onboarding_status));
  async function tpaClientsBox() {
    const box = document.getElementById("tpa-clients");
    const onb = App.feature("tpa_onboarding");
    try {
      const [r, o, dash] = await Promise.all([Api.program.tpaClients(),
        onb ? Api.program.tpaOnboarding().catch(() => ({ clients: [] })) : Promise.resolve({ clients: [] }),
        Api.program.tpaDashboard().catch(() => ({ cases: [] }))]);
      if (!window._tpaGroups) await tpaGroupsBox();
      const st = Object.fromEntries((o.clients || []).map((c) => [c.client_id, c]));
      const owed = {};
      for (const x of dash.cases || []) {
        if (!feePayable(x)) continue;
        const w = owed[x.client_id] || (owed[x.client_id] = { n: 0, cents: 0 });
        w.n++; w.cents += Number(x.fee_cents || 0);
      }
      const cs = r.clients || [];
      tpaCl.rows = cs.map((c) => ({ ...c, onb: st[c.id] || null, owed: owed[c.id] || null }));
      tpaCl.onb = onb;
      tpaClientsDraw();
      fillProviderTypes(document.getElementById("tpa-file-ptype"));
      // The filing list puts each group's providers under the group's name.
      const sel = document.getElementById("tpa-file-client");
      if (sel) {
        const keep = sel.value;
        const active = cs.filter((c) => c.status === "ACTIVE");
        const opt = (c) => `<option value="${c.id}">${esc(c.party_name)} (${esc(c.party_type)})</option>`;
        const byGroup = new Map();
        for (const c of active) { const k = c.group_name || ""; if (!byGroup.has(k)) byGroup.set(k, []); byGroup.get(k).push(c); }
        sel.innerHTML = [...byGroup.entries()].map(([g, list]) => g ? `<optgroup label="${esc(g)}">${list.map(opt).join("")}</optgroup>`
          : (byGroup.size > 1 ? `<optgroup label="Not in a group">${list.map(opt).join("")}</optgroup>` : list.map(opt).join(""))).join("");
        if (keep && active.some((c) => c.id === keep)) sel.value = keep;
      }
      tpaFileDocsDraw();
    } catch (e) { if (box) box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  function tpaClientFilter(f) { Object.assign(tpaCl, f); tpaClientsDraw(); }
  function tpaClientsDraw() {
    const box = document.getElementById("tpa-clients");
    if (!box) return;
    const { rows, q, show, onb } = tpaCl;
    const nReady = rows.filter(tpaClReady).length;
    const sum = document.getElementById("tpa-cl-summary");
    if (sum) sum.textContent = rows.length ? `${plural(rows.length, "client")}${onb ? ` · ${nReady} ready to file` : ""}` : "";
    tpaTabCount("clients", rows.length && onb ? `${nReady}/${rows.length} ready` : rows.length || "");
    if (!rows.length) { box.innerHTML = '<p class="muted">No clients yet. Add one, or import a spreadsheet of them below.</p>'; return; }
    const needle = (q || "").trim().toLowerCase();
    const match = (c) => (!needle || [c.party_name, c.contact_email, c.group_name].join(" ").toLowerCase().includes(needle))
      && (!show || (show === "ready" ? tpaClReady(c) : show === "setup" ? c.status === "ACTIVE" && !tpaClReady(c)
        : show === "owes" ? !!c.owed : show === "suspended" ? c.status !== "ACTIVE" : true));
    const groups = window._tpaGroups || [];
    const sections = groups.map((g) => ({ g, list: rows.filter((c) => c.group_id === g.id) }));
    sections.push({ g: null, list: rows.filter((c) => !c.group_id || !groups.some((g) => g.id === c.group_id)) });
    const filtering = !!(needle || show);
    const shown = sections.map((x) => ({ ...x, list: x.list.filter(match) })).filter((x) => x.list.length || (x.g && !filtering));
    if (!shown.some((x) => x.list.length) && filtering) { box.innerHTML = '<p class="muted">No client matches.</p>'; return; }
    const cols = onb ? 5 : 4;
    const groupHead = (g) => `<b>${esc(g.name)}</b>
      <span class="muted">${[g.tin && "TIN " + esc(g.tin), g.npi && "NPI " + esc(g.npi), g.contact_email && esc(g.contact_email)].filter(Boolean).join(" · ")}</span>
      ${g.status !== "ACTIVE" ? '<span class="badge">Suspended</span>' : ""}
      <span class="cl-grp-act"><button class="mini ghost" onclick="Views.tpaGroupRenameFn('${g.id}', '${esc(g.name.replace(/'/g, ""))}')">Rename</button>
        <button class="mini ghost" onclick="Views.tpaGroupStatusFn('${g.id}','${g.status === "ACTIVE" ? "SUSPENDED" : "ACTIVE"}')">${g.status === "ACTIVE" ? "Suspend" : "Reactivate"}</button></span>`;
    const setup = (c) => {
      if (c.status !== "ACTIVE") return '<span class="badge">Suspended</span>';
      if (!onb) return '<span class="badge s-ck-clear">Active</span>';
      const o = c.onb || {};
      const step = (done, label, fn) => done ? `<span class="cl-step">✓ ${label}</span>` : `<button class="mini" onclick="${fn}">Add ${label.toLowerCase()}</button>`;
      const pill = tpaClReady(c) ? '<span class="badge s-ck-clear">Ready to file</span>'
        : `<span class="badge ${["NPI_REJECTED", "EXCLUDED"].includes(o.onboarding_status) ? "s-ck-excluded" : "s-ck-review"}">${esc(ONB_STATUS[o.onboarding_status] || "Needs setting up")}</span>`;
      return `${pill}<div class="cl-steps">${step(o.profile_complete, "Profile", `Views.tpaProfileForm('${c.id}')`)}${step(o.verified_payments > 0, "Payment method", `Views.tpaPaymentForm('${c.id}')`)}</div>`;
    };
    const checks = (c) => {
      const o = c.onb || {}, out = [];
      if (o.npi) {
        const m = { VERIFIED: ["ck-clear", "NPI ✓"], NEEDS_REVIEW: ["ck-review", "NPI under review"], REJECTED: ["ck-excluded", "NPI rejected"],
          UNVERIFIED: ["ck-unavailable", "NPI not checked yet"] }[o.npi_status] || ["", "NPI"];
        const reg = o.npi_registry_name && o.npi_registry_name !== "(test NPI)" ? ` — registry: ${o.npi_registry_name}` : "";
        out.push(`<span class="badge s-${m[0]}" title="${esc(`NPI ${o.npi}${reg}`)}">${m[1]}</span>`);
      }
      const ex = o.checks && o.checks.EXCLUSION && o.checks.EXCLUSION.status;
      const e = { CLEAR: ["ck-clear", "Exclusion list ✓"], CLEARED: ["ck-clear", "Exclusion list ✓"], NEEDS_REVIEW: ["ck-review", "Exclusion list: under review"],
        EXCLUDED: ["ck-excluded", "On the exclusion list"], UNAVAILABLE: ["ck-unavailable", "Exclusion list: not checked yet"] }[ex];
      if (e) out.push(`<span class="badge s-${e[0]}" title="HHS-OIG List of Excluded Individuals/Entities">${e[1]}</span>`);
      const note = o.npi_status === "REJECTED" ? `${o.npi_review_note || "Rejected by the review team"} — correct it under Profile`
        : o.npi_status === "NEEDS_REVIEW" ? "The registry holds this NPI under another name; the review team confirms it before filing"
        : ex === "EXCLUDED" ? (o.checks.EXCLUSION.note || "") : "";
      return out.length ? `<div class="cl-chips">${out.join("")}</div>${note ? `<div class="muted cl-note">${esc(note)}</div>` : ""}`
        : '<span class="muted">After the profile</span>';
    };
    const disputes = (c) => `${c.case_count || 0} filed${c.owed ? `<div class="cl-owed"><span class="badge s-ck-review">${c.owed.n} unpaid · ${usdC(c.owed.cents)}</span>
      <button class="mini" onclick="Views.payClientFn('${c.id}', this)">Pay ${usdC(c.owed.cents)}</button></div>` : ""}`;
    const groupSel = (c) => groups.length ? `<select class="mini" aria-label="Provider group for ${esc(c.party_name)}" onchange="Views.tpaClientGroupFn('${c.id}', this.value)">
        <option value="">No group</option>${groups.map((g) => `<option value="${g.id}"${g.id === c.group_id ? " selected" : ""}>${esc(g.name)}</option>`).join("")}</select>` : "";
    const actions = (c) => `<div class="cl-act">${tpaClReady(c) ? `<button class="mini" onclick="Views.tpaFileFor('${c.id}')">File a dispute</button>` : ""}
      ${onb ? `<button class="mini ghost" onclick="Views.tpaProfileForm('${c.id}')">Profile</button><button class="mini ghost" onclick="Views.tpaPaymentForm('${c.id}')">Payment</button>` : ""}
      ${groupSel(c)}
      <button class="mini ghost" onclick="Views.tpaClientStatusFn('${c.id}','${c.status === "ACTIVE" ? "SUSPENDED" : "ACTIVE"}')">${c.status === "ACTIVE" ? "Suspend" : "Reactivate"}</button></div>`;
    const row = (c) => `<tr class="${c.status !== "ACTIVE" ? "cl-off" : ""}"><td><b>${esc(c.party_name)}</b>
        <div class="muted">${c.party_type === "HEALTH_PLAN" ? "Health plan" : "Provider"} · ${esc(c.contact_email || "no email")}</div></td>
      <td>${setup(c)}</td>${onb ? `<td>${checks(c)}</td>` : ""}<td>${disputes(c)}</td><td>${actions(c)}</td></tr>`;
    box.innerHTML = `<table class="tbl cl-tbl" data-nosearch><thead><tr><th>Client</th><th>Set up</th>${onb ? "<th>Checks</th>" : ""}<th>Disputes</th><th></th></tr></thead>
      ${shown.map(({ g, list }) => `<tbody>${g || groups.length ? `<tr class="cl-grp"><td colspan="${cols}">${g ? groupHead(g) : "<b>Not in a provider group</b>"}</td></tr>` : ""}
        ${list.length ? list.map(row).join("") : `<tr><td colspan="${cols}" class="muted">No providers in this group yet: pick the group in a client's row.</td></tr>`}</tbody>`).join("")}</table>`;
  }
  async function tpaClientAddOpen() {
    const gs = (window._tpaGroups || []).filter((g) => g.status === "ACTIVE");
    const v = await UI.modal({ title: "Add a client", submitLabel: "Add client", fields: [
      { name: "party_name", label: "Organization name", required: true },
      { name: "party_type", label: "Type", options: [["PROVIDER", "Provider"], ["HEALTH_PLAN", "Health plan"]], value: "PROVIDER" },
      { name: "contact_email", label: "Contact email", type: "email" },
      ...(gs.length ? [{ name: "group_id", label: "Provider group", options: [["", "Not in a group"], ...gs.map((g) => [g.id, g.name])] }] : []),
    ], body: tpaCl.onb ? "Next, add its profile (NPI, TIN, address) and a payment method; then it's ready to file." : "" });
    if (!v) return;
    try {
      await Api.program.tpaAddClient({ party_name: v.party_name.trim(), party_type: v.party_type, contact_email: v.contact_email.trim(), group_id: v.group_id || "" });
      UI.toast(`${v.party_name.trim()} added`);
      if (v.group_id) await tpaGroupsBox();
      tpaClientsBox();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function tpaGroupAddOpen() {
    const v = await UI.modal({ title: "Add a provider group", submitLabel: "Add group",
      body: "A practice or health system you act for. Put its providers in it from each client's row.", fields: [
        { name: "name", label: "Group name", required: true, placeholder: "e.g. Coastal Physicians Group" },
        { name: "tin", label: "Group TIN", placeholder: "optional" },
        { name: "npi", label: "Group NPI", placeholder: "optional" },
        { name: "contact_email", label: "Contact email", type: "email", placeholder: "optional" }] });
    if (!v) return;
    try {
      await Api.program.tpaGroupAdd({ name: v.name, tin: v.tin, npi: v.npi, contact_email: v.contact_email });
      UI.toast("Provider group added");
      await tpaGroupsBox(); tpaClientsBox();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  // "File a dispute" on a ready client: the File tab, with the client picked.
  function tpaFileFor(clientId) {
    tpaTab("file");
    const sel = document.getElementById("tpa-file-client");
    if (sel) { sel.value = clientId; tpaFileDocsReset(); sel.focus(); }
  }

  // ── Documents for the dispute being filed ──
  // Its own set, made for it here: six files or one merged PDF, each checked before the dispute is filed (a set
  // named on a filing must have all six accepted). A set started earlier for the same client and not filed yet
  // can be used instead.
  let tpaFileDocs = null; // { id, client, mode, status }
  function tpaFileDocsReset() {
    tpaFileDocs = null;
    const h = document.getElementById("tpa-file-package");
    if (h) h.value = "";
    tpaFileDocsDraw();
  }
  function tpaFileDocsUse(id, client, mode, status) {
    tpaFileDocs = { id, client, mode, status };
    const h = document.getElementById("tpa-file-package");
    if (h) h.value = id;
    tpaFileDocsDraw();
  }
  async function tpaFileDocsStart(mode) {
    const client = (document.getElementById("tpa-file-client") || {}).value;
    if (!client) { UI.toast("Pick the client first.", { kind: "warn" }); return; }
    try {
      const r = await Api.program.tpaCreatePackage(client, mode);
      tpaFileDocsUse(r.package_id, client, mode, "ASSEMBLING");
      Object.assign(tpaFileDocs, { six: 0, more: 0 });
      tpaPackageBox(r.package_id);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  function tpaFileDocsDraw() {
    const box = document.getElementById("tpa-file-docs");
    if (!box) return;
    const client = (document.getElementById("tpa-file-client") || {}).value;
    if (tpaFileDocs && tpaFileDocs.client !== client) {
      tpaFileDocs = null;
      const h = document.getElementById("tpa-file-package");
      if (h) h.value = "";
    }
    if (tpaFileDocs) {
      // Filed with the dispute once every document has been read; a flagged one goes too, for the reviewer.
      const d = tpaFileDocs, more = d.more || 0, onlyMore = !d.six && more > 0;
      const ready = d.status === "READY" || onlyMore;
      const others = more ? ` and ${plural(more, "other document")}` : "";
      if (ready && d.prefilledFor !== d.id) {
        d.prefilledFor = d.id;
        filePrefillFn("tpa", document.getElementById("tpa-file-client")?.form, d.id, true);
      }
      box.innerHTML = `<p>${d.mode === "MERGED" ? "The documents as one PDF" : "The documents"}: <span class="badge ${ready ? "s-ck-clear" : "s-ck-review"}">${onlyMore ? "ready to file" : esc(PKG_STATUS[d.status] || d.status)}</span>${more ? ` <span class="muted">+ ${plural(more, "other document")}</span>` : ""}
          <button type="button" class="mini" onclick="Views.tpaPackageBox('${d.id}')">${ready ? "See or add documents" : "Upload the documents"}</button>
          <button type="button" class="mini ghost" onclick="Views.filePrefillFn('tpa', document.getElementById('tpa-file-client').form, '${d.id}', false)">Fill in from the documents</button>
          <button type="button" class="mini ghost" onclick="Views.tpaFileDocsReset()">File without them</button></p>
        <p class="muted">${ready ? `Every document has been read${others}: they are all filed with this dispute, flagged ones too.`
          : "The documents are being read — this takes a minute. File once they are, or file without them."}</p>`;
      return;
    }
    const c = (tpaCl.rows || []).find((x) => x.id === client);
    const started = ((c && c.onb && c.onb.open_packages) || []).filter((pk) => pk.status !== "FILED");
    box.innerHTML = `<p><button type="button" class="mini" onclick="Views.tpaFileDocsStart('DISCRETE')">Add the documents</button>
      <button type="button" class="mini" onclick="Views.tpaFileDocsStart('MERGED')">Add them as one merged PDF</button>
      ${started.length ? `<span class="muted">or use one started earlier:</span> ${started.map((pk) => `<button type="button" class="mini ghost" onclick="Views.tpaFileDocsUse('${pk.id}','${esc(client)}','${pk.mode}','${pk.status}')">${pk.mode === "MERGED" ? "Merged PDF" : "Six files"} — ${esc(PKG_STATUS[pk.status] || pk.status)}</button>`).join(" ")}` : ""}</p>`;
  }

  // ── TPA employees (tpa_team.go) ──
  async function tpaTeamBox() {
    const list = document.getElementById("tpa-team-list"), add = document.getElementById("tpa-team-add");
    if (!list) return;
    try {
      const r = await Api.program.tpaTeam();
      const team = r.team || [], admin = r.can_manage;
      const roleCell = (m) => admin && !m.is_me
        ? `<select class="mini" aria-label="Role for ${esc(m.full_name || m.email)}" onchange="Views.tpaTeamRoleFn('${m.user_sub}', this.value)">
            <option value="STAFF"${m.team_role === "STAFF" ? " selected" : ""}>Staff</option><option value="ADMIN"${m.team_role === "ADMIN" ? " selected" : ""}>Admin</option></select>`
        : badge(m.team_role === "ADMIN" ? "Admin" : "Staff");
      list.innerHTML = team.length ? `<table class="tbl"><thead><tr><th>Name</th><th>Email</th><th>Username</th><th>Role</th><th>Since</th><th></th></tr></thead><tbody>
        ${team.map((m) => `<tr><td>${esc(m.full_name || "—")}${m.is_me ? ' <span class="muted">(you)</span>' : ""}</td><td>${esc(m.email || "—")}</td>
          <td class="mono">${esc(m.username || "—")}</td><td>${roleCell(m)}</td><td>${fmtDate(m.linked_at)}</td>
          <td>${admin && !m.is_me ? `<button class="mini ghost" onclick="Views.tpaTeamRemoveFn('${m.user_sub}', '${esc((m.full_name || m.email || "").replace(/'/g, ""))}')">Remove</button>` : ""}</td></tr>`).join("")}</tbody></table>`
        : '<p class="muted">No employees yet.</p>';
      add.innerHTML = admin ? `<h3>Add an employee</h3>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.tpaTeamAddFn(this)">
          <input name="full_name" placeholder="full name" required />
          <input name="email" type="email" data-email-check placeholder="work email — sign-in details go here" required />
          <select name="team_role"><option value="STAFF">Staff</option><option value="ADMIN">Admin</option></select>
          <button class="mini">Add employee</button></form><div id="tpa-team-msg"></div>`
        : '<p class="muted">Ask one of your admins to add or remove employees.</p>';
    } catch (e) { list.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaTeamAddFn(form) {
    const msg = document.getElementById("tpa-team-msg"), btn = form.querySelector("button");
    btn.disabled = true;
    try {
      const r = await Api.program.tpaTeamAdd({ full_name: form.full_name.value, email: form.email.value, team_role: form.team_role.value });
      msg.innerHTML = `<p class="ok">Added — ${esc(r.note)}. Their username is <span class="mono">${esc(r.username)}</span>.</p>`;
      form.reset(); tpaTeamBox();
    } catch (e) { msg.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
    finally { btn.disabled = false; }
  }

  async function tpaTeamRoleFn(sub, role) {
    try { await Api.program.tpaTeamRole(sub, role); UI.toast(role === "ADMIN" ? "Now an admin" : "Now staff"); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
    tpaTeamBox();
  }

  async function tpaTeamRemoveFn(sub, name) {
    if (!confirm(`Remove ${name || "this employee"}? Their login stops working for your organization.`)) return;
    try { await Api.program.tpaTeamRemove(sub); UI.toast("Employee removed"); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
    tpaTeamBox();
  }

  // ── Provider groups: TPA -> groups -> providers ──
  async function tpaGroupsBox() {
    const box = document.getElementById("tpa-groups");
    try {
      const [r, cl] = await Promise.all([Api.program.tpaGroups(), box ? Api.program.tpaClients().catch(() => ({ clients: [] })) : Promise.resolve({ clients: [] })]);
      const gs = window._tpaGroups = r.groups || [];
      const members = (g) => (cl.clients || []).filter((c) => c.group_id === g.id).map((c) => c.party_name);
      if (box) box.innerHTML = gs.length ? `<div class="tbl-wrap"><table class="tbl"><thead><tr><th>Group</th><th>TIN</th><th>NPI</th><th>Contact</th><th>Providers</th><th>Disputes</th><th>Status</th><th></th></tr></thead><tbody>
        ${gs.map((g) => `<tr><td><b>${esc(g.name)}</b>${members(g).length ? `<div class="muted">${members(g).map(esc).join(", ")}</div>` : ""}</td><td class="mono">${esc(g.tin || "—")}</td><td class="mono">${esc(g.npi || "—")}</td>
          <td>${esc(g.contact_email || "—")}</td><td>${g.provider_count}</td><td>${g.case_count}</td><td>${badge(g.status)}</td>
          <td><button class="mini" onclick="Views.tpaGroupRenameFn('${g.id}', '${esc(g.name.replace(/'/g, ""))}')">Rename</button>
            <button class="mini ghost" onclick="Views.tpaGroupStatusFn('${g.id}','${g.status === "ACTIVE" ? "SUSPENDED" : "ACTIVE"}')">${g.status === "ACTIVE" ? "Suspend" : "Reactivate"}</button></td></tr>`).join("")}</tbody></table></div>`
        : '<p class="muted">No provider groups yet. Add one, then put its providers in it from the Clients tab.</p>';
      document.querySelectorAll(".tpa-group-select").forEach((sel) => {
        const keep = sel.value;
        sel.innerHTML = `<option value="">No provider group</option>` + gs.filter((g) => g.status === "ACTIVE").map((g) => `<option value="${g.id}">${esc(g.name)}</option>`).join("");
        sel.value = keep;
      });
    } catch (e) { if (box) box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }

  async function tpaGroupAddFn(form) {
    try {
      await Api.program.tpaGroupAdd({ name: form.name.value, tin: form.tin.value, npi: form.npi.value, contact_email: form.contact_email.value });
      form.reset(); UI.toast("Provider group added");
      await tpaGroupsBox(); tpaClientsBox();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaGroupRenameFn(id, name) {
    const next = prompt("New name for the provider group", name);
    if (!next || next.trim() === name) return;
    try { await Api.program.tpaGroupUpdate(id, { name: next }); await tpaGroupsBox(); tpaClientsBox(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaGroupStatusFn(id, status) {
    try { await Api.program.tpaGroupUpdate(id, { status }); await tpaGroupsBox(); tpaClientsBox(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaClientGroupFn(clientId, groupId) {
    try { await Api.program.tpaClientGroup(clientId, groupId); UI.toast(groupId ? "Moved to the group" : "Taken out of its group"); await tpaGroupsBox(); tpaClientsBox(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); tpaClientsBox(); }
  }

  async function tpaAddClientFn(form) {
    try {
      await Api.program.tpaAddClient({ party_name: form.party_name.value, party_type: form.party_type.value, contact_email: form.contact_email.value,
        group_id: form.group_id ? form.group_id.value : "" });
      if (form.group_id && form.group_id.value) tpaGroupsBox();
      // The readiness board lists clients too: without this the new client
      // only showed up there after a reload, so it couldn't be onboarded.
      form.reset(); tpaClientsBox();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function tpaClientStatusFn(id, status) {
    try { await Api.program.tpaClientStatus(id, status); tpaClientsBox(); }
    catch (e) { alert(e.message); }
  }

  // The six usual documents, by the name people use. None is required.
  const SLOT_NAME = { EOB: "EOB (explanation of benefits)", ITEMIZED_BILL: "Itemized bill", PROOF_OF_COVERAGE: "Proof of coverage",
    OPEN_NEGOTIATION_NOTICE: "Open negotiation notice", OFFER_SUBSTANTIATION: "Offer substantiation", IDRE_ATTESTATION: "Attestation" };
  // File a dispute filled in from its own document set (org_prefill.go): empty fields only -- never over what the
  // filer typed -- each marked with the document it came from. The filer checks and files; on filing, each field
  // kept or changed is counted (telemetry: prefill_accept / prefill_override).
  const PREFILL_LABEL = { client_id: "client", amount: "amount", other_party_name: "other side", other_party_email: "email for notices",
    other_party_naic: "NAIC", other_party_npi: "NPI", provider_type: "provider type", in_network: "network" };
  async function filePrefillFn(kind, form, packageId, auto) {
    const note = document.getElementById(kind === "hp" ? "hp-file-prefill" : "tpa-file-prefill");
    if (!form || !packageId) return;
    let r;
    try { r = await Api.program.orgPrefill(kind, packageId); } catch (e) { if (!auto) UI.toast(e.message, { kind: "warn" }); return; }
    const filled = [], skipped = [];
    for (const [name, v] of Object.entries(r.fields || {})) {
      const el = form.elements[name];
      if (!el || !v.value) continue;
      if (name === "client_id") { // the client is chosen before the documents: only say if they point elsewhere
        if (el.value && el.value !== v.value) skipped.push(`these documents look like they are for another client (${esc(v.why)})`);
        continue;
      }
      if (el.tagName === "SELECT" && ![...el.options].some((o) => o.value === v.value)) continue;
      if (String(el.value || "").trim() && el.dataset.prefill === undefined) continue; // typed by the filer
      el.value = v.value;
      el.dataset.prefill = v.value;
      el.classList.add("prefilled");
      el.title = `Filled in from ${v.from}: ${v.why}`;
      const clear = () => { el.classList.remove("prefilled"); };
      el.addEventListener("input", clear, { once: true }); el.addEventListener("change", clear, { once: true });
      filled.push(`<li><b>${esc(PREFILL_LABEL[name] || name)}</b>: ${esc(v.value)} <span class="muted">— ${esc(v.why)}</span></li>`);
    }
    if (note) note.innerHTML = filled.length || skipped.length ? `<div class="prefill-note"><b>Filled in from your documents — check before filing.</b>
        ${filled.length ? `<ul>${filled.join("")}</ul>` : ""}${skipped.map((x) => `<p class="warn-line">${x}</p>`).join("")}</div>`
      : auto ? "" : `<p class="muted">The documents don't state any of the form's fields${r.documents ? "" : " yet"}.</p>`;
  }
  // On filing: what was kept as filled in, and what the filer changed.
  function prefillOutcome(form) {
    let kept = 0, changed = 0;
    form.querySelectorAll("[data-prefill]").forEach((el) => {
      const same = String(el.value) === el.dataset.prefill;
      same ? kept++ : changed++;
      if (window.Telemetry) Telemetry.track(same ? "prefill_accept" : "prefill_override", el.name);
      delete el.dataset.prefill; el.classList.remove("prefilled");
    });
    return { prefilled: kept + changed, kept, changed };
  }

  // Why a filing was refused, where it can't be missed: a red box under the form (scrolled to), a toast, and the
  // field it is about outlined until it's changed.
  const FILE_FIELD_HINTS = [
    [/e-?mail/i, "other_party_email", "the health plan's email"], [/naic/i, "other_party_naic", "the plan NAIC"],
    [/minimum|threshold|amount/i, "amount", "the disputed amount"], [/provider type/i, "provider_type", "the provider type"],
    [/network/i, "in_network", "the network answer"],
    [/health plan|other.?party|plan name|respondent/i, "other_party_name", "the health plan"],
    [/contact/i, "contact_name", "the client contact"],
    [/client|party|npi|tin|profile|payment method|suspended|roster/i, "client_id", "the client"],
    [/npi|provider/i, "other_party_npi", "the provider's NPI"],
    [/party|profile|payment method|suspended|roster/i, "party_id", "the party you file for"],
  ];
  function fileAlert(form, out, title, msg, problems) {
    form.querySelectorAll(".field-bad").forEach((el) => { el.classList.remove("field-bad"); el.removeAttribute("aria-invalid"); });
    const text = [msg, ...(problems || [])].join(" ");
    const hit = /packet/i.test(text) && !(problems || []).length ? null : FILE_FIELD_HINTS.find(([re, name]) => re.test(text) && form.elements[name]);
    const field = hit && form.elements[hit[1]];
    if (field) {
      field.classList.add("field-bad"); field.setAttribute("aria-invalid", "true");
      const clear = () => { field.classList.remove("field-bad"); field.removeAttribute("aria-invalid"); };
      field.addEventListener("input", clear, { once: true }); field.addEventListener("change", clear, { once: true });
    }
    const packetBad = /packet/i.test(text);
    out.innerHTML = `<div class="file-alert" role="alert"><b>${esc(title)}</b>
      <p>${esc(msg)}</p>${(problems || []).length ? `<ul>${problems.map((x) => `<li>${esc(x)}</li>`).join("")}</ul>` : ""}
      ${field ? `<p class="muted">Check ${esc(hit[2])} — it's outlined in red above.</p>` : packetBad ? `<p class="muted">Check the filing packet below.</p>` : ""}</div>`;
    out.scrollIntoView({ behavior: "smooth", block: "center" });
    if (field) setTimeout(() => field.focus({ preventScroll: true }), 400);
    UI.toast(`${title} ${msg}`, { kind: "warn", duration: 9000 });
  }

  async function tpaFileFn(form) {
    const out = document.getElementById("tpa-file-result");
    const btn = form.querySelector("button");
    out.innerHTML = "";
    const packetHost = document.getElementById("tpa-packet");
    const packetProblems = packetHost && PacketForm.touched(packetHost) ? PacketForm.read(packetHost).problems : [];
    if (packetProblems.length) { fileAlert(form, out, "This dispute wasn't filed.", "The filing packet isn't complete:", packetProblems); return; }
    const packet = packetFrom("tpa-packet", out);
    if (packet === false) return;
    if (btn) { btn.disabled = true; btn.dataset.label = btn.textContent; btn.textContent = "Filing…"; }
    try {
      const r = await Api.program.tpaIntake({
        packet: packet || undefined,
        client_id: form.client_id.value, package_id: form.package_id ? form.package_id.value : "",
        contact_name: form.contact_name.value,
        disputed_amount_cents: Math.round(Number(form.amount.value || 0) * 100), notes: form.notes.value,
        other_party_name: form.other_party_name.value, other_party_email: form.other_party_email.value, other_party_naic: form.other_party_naic.value,
        provider_type: form.provider_type ? form.provider_type.value : "", in_network: form.in_network ? form.in_network.value : "",
      });
      out.innerHTML = `<p class="ok">Filed — case <a href="#/cases/${r.case_id}">${esc(r.case_number)}</a> (${esc(r.status)}) for <b>${esc(r.initiating_party)}</b>. ${r.payment_required ? `Pay its fee to open it: <a href="#/cases/${r.case_id}">Pay now</a>.` : ""}</p>`;
      if (window.Telemetry) Telemetry.taskDone("file_dispute", prefillOutcome(form));
      const pn = document.getElementById("tpa-file-prefill"); if (pn) pn.innerHTML = "";
      // A filed package leaves the board and the "file with" list.
      form.reset(); packetInit("tpa-packet", {}); tpaFileDocsReset(); tpaPkgClose(); tpaDashBox(); tpaClientsBox();
      out.scrollIntoView({ behavior: "smooth", block: "center" });
      UI.toast(`Dispute ${r.case_number} filed`);
    } catch (e) { if (window.Telemetry) Telemetry.taskError("file_dispute", e.message); fileAlert(form, out, "This dispute wasn't filed.", e.message, e.problems); }
    finally { if (btn) { btn.disabled = false; btn.textContent = btn.dataset.label || "File dispute"; } }
  }

  // The TPA's disputes, grouped under the provider (client) they were filed
  // for: each provider opens and closes, shows what it owes, and pays all of
  // its unpaid disputes at once; each dispute can also be paid on its own.
  window._tpaProvOpen = window._tpaProvOpen || {};
  function tpaCasesHtml(cases, status, q, pay = "") {
    let shown = cases;
    if (pay) shown = shown.filter((x) => x.fee_state === pay);
    if (status) shown = shown.filter((x) => x.status === status || x.fee_state === status);
    if (q) shown = shown.filter((x) => (x.client_name || "").toLowerCase().includes(q) || (x.case_number || "").toLowerCase().includes(q));
    if (!shown.length) return '<p class="muted">No disputes match this filter.</p>';
    const groups = new Map();
    for (const x of shown) {
      const k = x.client_id || x.client_name;
      if (!groups.has(k)) groups.set(k, { id: x.client_id, name: x.client_name, rows: [] });
      groups.get(k).rows.push(x);
    }
    const canPay = window._tpaCanEdit;
    return [...groups.values()].map((g) => {
      const unpaid = g.rows.filter(feePayable), due = unpaid.reduce((n, x) => n + Number(x.fee_cents || 0), 0);
      const paid = g.rows.filter((x) => x.fee_state === "PAID").length, pending = g.rows.filter((x) => x.fee_state === "PENDING").length;
      const open = window._tpaProvOpen[g.id] ?? (unpaid.length > 0 || groups.size === 1);
      return `<details class="prov" ${open ? "open" : ""} ontoggle="Views.tpaProvToggle('${g.id}', this.open)">
        <summary><span class="prov-name">${esc(g.name || "—")}</span>
          <span class="muted">${g.rows.length} dispute${g.rows.length === 1 ? "" : "s"} · ${paid} paid${pending ? ` · ${pending} pending` : ""}</span>
          <span class="prov-pay">${unpaid.length
            ? `<span class="badge s-needs-review">${unpaid.length} unpaid · ${usdC(due)}</span>${canPay ? ` <button class="mini" onclick="event.preventDefault(); event.stopPropagation(); Views.payClientFn('${g.id}', this)">Pay all — ${usdC(due)}</button>` : ""}`
            : '<span class="badge s-filed">Nothing owed</span>'}${canPay && g.id ? ` <button class="mini secondary" onclick="event.preventDefault(); event.stopPropagation(); Views.orgDocsDownload('tpa', {client_id: '${g.id}'}, this)">Download documents</button>` : ""}</span></summary>
        <div class="tbl-wrap"><table class="tbl"><thead><tr><th>Case</th><th>Status</th><th>Disputed</th><th>Fee</th><th>Payment</th><th>Filed</th><th></th></tr></thead><tbody>
        ${g.rows.map((x) => `<tr>
          <td>${x.case_id ? `<a href="#/cases/${x.case_id}">${esc(x.case_number || x.case_id.slice(0, 8))}</a>` : "—"}</td>
          <td>${x.agency_status && x.status !== "AWAITING_PAYMENT" ? `<span class="badge" title="The dispute's status with the Agency">${esc(x.agency_status)}</span>` : badge(x.status)}</td><td>${usdC(x.disputed_amount_cents)}</td><td>${x.fee_cents ? usdC(x.fee_cents) : "—"}</td>
          <td>${feePill(x)}</td><td>${esc(x.filed)}</td>
          <td class="row-actions">${(window._tpaSent || {})[x.case_id] ? `<button class="mini ghost" onclick="Views.sentRecordOpen('tpa', '${x.case_id}', '${esc(x.case_number || "")}')" title="The letters sent to you on this dispute, with their documents">Letters (${window._tpaSent[x.case_id]})</button>` : ""}
            ${canPay && feePayable(x) ? `<button class="mini" onclick="Views.payDisputeFn('tpa', '${x.case_id}', this)">Pay now</button>` : ""}
            ${x.status === "AWAITING_PAYMENT" && canPay ? `<button class="mini secondary" onclick="Views.tpaEditDisputeFn('${x.case_id}')">Edit</button>` : ""}</td></tr>`).join("")}
        </tbody></table></div></details>`;
    }).join("");
  }
  function tpaProvToggle(id, open) { window._tpaProvOpen[id] = open; }
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
    if (box) box.innerHTML = tpaCasesHtml(window._tpaCases || [], status, q, window._tpaPay || "");
  }
  // The payment switch over the TPA's disputes, each state with its count.
  function tpaPaySegs() {
    const cs = window._tpaCases || [];
    const n = (st) => cs.filter((x) => x.fee_state === st).length;
    return ListUI.segs("Fee payment", [{ value: "", label: "All", count: cs.length }, { value: "UNPAID", label: "Unpaid", count: n("UNPAID") },
      { value: "PENDING", label: "Pending", count: n("PENDING") }, { value: "PAID", label: "Paid", count: n("PAID") }, { value: "FAILED", label: "Failed", count: n("FAILED") }]
      .filter((o) => !o.value || o.count), window._tpaPay || "", "Views.tpaPayFilter");
  }
  function tpaPayFilter(st) {
    window._tpaPay = st;
    const segs = document.getElementById("tpa-pay-segs");
    if (segs) segs.innerHTML = tpaPaySegs();
    tpaCasesFilter();
  }
  async function tpaDashBox(tpaId) {
    const roll = document.getElementById("tpa-rollup"), box = document.getElementById("tpa-cases"),
      filtersBox = document.getElementById("tpa-cases-filters");
    try {
      const [r, rec] = await Promise.all([Api.program.tpaDashboard(tpaId),
        tpaId ? Promise.resolve({}) : Api.program.sentRecord("tpa").catch(() => ({}))]);
      window._tpaCases = r.cases || [];
      window._tpaSent = {};
      for (const m of rec.mail || []) window._tpaSent[m.case_id] = (window._tpaSent[m.case_id] || 0) + 1;
      // Editable only on the caller's own org view, not staff oversight of another TPA.
      window._tpaCanEdit = !tpaId;
      const cs = window._tpaCases;
      const by = (st) => cs.filter((x) => x.fee_state === st);
      const unpaid = cs.filter(feePayable);
      roll.innerHTML = cs.length ? ListUI.summary([
        { label: "Disputes", value: cs.length, sub: `for ${new Set(cs.map((x) => x.client_id || x.client_name)).size} client(s)` },
        { label: "Fees unpaid", value: unpaid.length, sub: unpaid.length ? usdC(unpaid.reduce((n, x) => n + Number(x.fee_cents || 0), 0)) + " to pay" : "nothing owed", tone: unpaid.length ? "warn" : "good" },
        { label: "Paid", value: by("PAID").length, sub: by("PENDING").length ? `${by("PENDING").length} waiting for the bank` : "fees settled" },
        { label: "Disputed", value: usdC(cs.reduce((n, x) => n + Number(x.disputed_amount_cents || 0), 0)), sub: "across all your disputes" },
      ]) : "";
      window._tpaPay = window._tpaPay || "";
      const statuses = [...new Set(cs.map((x) => x.status))].sort();
      if (filtersBox) filtersBox.innerHTML = cs.length ? `<div class="list-bar">
        <input id="tpa-cases-q" class="pipe-q" placeholder="Search client or case #…" oninput="Views.tpaCasesFilter()" aria-label="Search disputes" />
        <div id="tpa-pay-segs">${tpaPaySegs()}</div>
        <select id="tpa-cases-status" onchange="Views.tpaCasesFilter()" aria-label="Case status"><option value="">Any case status</option>
          ${statuses.map((s) => `<option value="${esc(s)}">${esc(s)}</option>`).join("")}</select>
        <span class="spacer"></span>
        ${window._tpaCanEdit ? `<button class="mini secondary" onclick="Views.orgDocsDownload('tpa', {}, this)">Download all documents</button>` : ""}</div>` : "";
      box.innerHTML = tpaCasesHtml(cs, "", "", window._tpaPay);
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

  // ---- Bulk filing with documents per dispute (tpa_bulk_disputes.go) ----
  async function tpaBulkDisputesFn(form) {
    const out = document.getElementById("tpa-bd-result");
    const btn = form.querySelector("button"); btn.disabled = true;
    out.innerHTML = '<p class="muted">Uploading and reading the ZIP…</p>';
    try {
      const r = await Api.program.tpaBulkDisputes(form.batch_ref.value.trim(), form.file.files[0]);
      if (r.idempotent_replay) { out.innerHTML = `<p class="muted">${esc(r.note)}</p>`; tpaBdToggle(r.batch_id, true); return; }
      out.innerHTML = `<p>${r.waiting_for_documents} dispute(s) accepted and waiting for their documents to be checked${r.refused ? `, <b>${r.refused} refused</b>` : ""}.</p>
        <table class="tbl"><thead><tr><th>Row</th><th>Dispute</th><th>Client</th><th>Documents</th><th>Result</th></tr></thead><tbody>
        ${(r.rows || []).map((x) => `<tr><td>${x.row}</td><td>${esc(x.dispute_ref)}</td><td>${esc(x.party)}</td>
          <td>${x.mode ? esc(x.mode === "MERGED" ? "merged PDF" : (x.attached || []).length + " of 6 files") : "—"}</td>
          <td>${x.status === "WAITING_DOCS" ? '<span class="badge s-validating">Checking documents</span>' : '<span class="badge s-rejected">Refused</span>'}${(x.errors || []).length ? `<br><span class="muted">${(x.errors || []).map(esc).join("<br>")}</span>` : ""}</td></tr>`).join("")}
        </tbody></table>`;
      if (window.Telemetry) Telemetry.taskDone("bulk_filing", { rows: (r.rows || []).length, refused: r.refused || 0 });
      form.reset(); await tpaBdBatchesBox(); tpaBdToggle(r.batch_id, true);
    } catch (e) { if (window.Telemetry) Telemetry.taskError("bulk_filing", e.message); out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
    finally { btn.disabled = false; }
  }
  async function tpaBdBatchesBox() {
    const box = document.getElementById("tpa-bd-batches");
    if (!box) return;
    try {
      const bs = (await Api.program.tpaBulkDisputeBatches()).batches || [];
      // A refresh redraws the list: keep the filings someone has open, open.
      const open = [...box.querySelectorAll("tr.row-detail")].filter((r) => r.style.display !== "none").map((r) => r.id.slice("bd-detail-".length));
      tpaTabCount("bulk", bs.reduce((n, b) => n + Number(b.waiting || 0), 0) || "");
      box.innerHTML = bs.length ? `<p class="muted">Click a filing to see its disputes.</p><table class="tbl bd-batches"><thead><tr><th>Batch</th><th>Uploaded</th><th>Disputes</th><th>Filed</th><th>Waiting</th><th>Refused</th><th>Fees unpaid</th><th></th></tr></thead><tbody>
        ${bs.map((b) => `<tr class="bd-row" tabindex="0" onclick="Views.tpaBdToggle('${b.id}')" onkeydown="if(event.key==='Enter')Views.tpaBdToggle('${b.id}')">
          <td class="mono">${esc(b.batch_ref)}</td><td>${esc(String(b.created_at || "").slice(0, 16))}</td>
          <td>${b.rows}</td><td>${b.filed}</td><td>${b.waiting}</td><td>${b.refused ? `<span class="badge warn">${b.refused}</span>` : "0"}</td>
          <td>${Number(b.unpaid) ? `${b.unpaid} · ${usdC(b.unpaid_cents)}` : '<span class="muted">—</span>'}</td>
          <td><span class="bd-chevron" id="bd-chev-${b.id}">▸</span></td></tr>
          <tr id="bd-detail-${b.id}" class="row-detail" style="display:none"><td colspan="8"><div id="bd-report-${b.id}"></div></td></tr>`).join("")}</tbody></table>`
        : '<p class="muted">No bulk filings yet — upload a disputes ZIP above.</p>';
      open.forEach((id) => tpaBdToggle(id, true));
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  function tpaBdToggle(batchId, forceOpen) {
    const row = document.getElementById(`bd-detail-${batchId}`), chev = document.getElementById(`bd-chev-${batchId}`);
    if (!row) return;
    const open = forceOpen || row.style.display === "none";
    row.style.display = open ? "" : "none";
    if (chev) chev.textContent = open ? "▾" : "▸";
    if (open) tpaBdReport(batchId);
  }
  // A bulk row's state, in words: filed, refused, or waiting on its documents.
  function bdStatus(x) {
    const b = (cls, t) => `<span class="badge s-${cls}">${t}</span>`;
    if (x.status === "FILED") return b("filed", "Filed");
    if (x.status === "ERROR") return b("rejected", "Refused");
    if (x.status === "FILING") return b("validating", "Filing…");
    if (x.rejected) return b("rejected", "Needs a fix — document rejected");
    if (x.needs_review) return b("needs-review", "Waiting for review");
    return b("validating", "Checking documents");
  }
  // What the TPA owes across all its disputes, paid with one checkout.
  async function tpaFeesBox() {
    const box = document.getElementById("tpa-fees");
    if (!box) return;
    try {
      const f = await Api.program.tpaFees();
      tpaTabCount("disputes", f.unpaid ? `${f.unpaid} unpaid` : "");
      box.innerHTML = f.unpaid ? `<div class="tpa-paybar"><div><b>${f.unpaid} dispute${f.unpaid === 1 ? "" : "s"} waiting for the initial fee — ${usdC(f.amount_cents)}</b>
          <div class="muted">One payment covers all of them, whether filed one by one or in bulk. Each dispute opens, with its case number and document link, once paid.</div></div>
          <button onclick="Views.tpaFeesPay(this)">Pay ${usdC(f.amount_cents)} for ${f.unpaid} dispute${f.unpaid === 1 ? "" : "s"}</button></div>` : "";
    } catch { box.innerHTML = ""; }
  }
  // One ZIP of an organization's own disputes' documents (org_documents_zip.go).
  async function orgDocsDownload(kind, params, btn) {
    const label = btn ? btn.textContent : "";
    if (btn) { btn.disabled = true; btn.textContent = "Preparing the ZIP…"; }
    try { await Api.program.orgDocsZip(kind, params); }
    catch (e) { UI.toast(e.message, { kind: "warn", duration: 8000 }); }
    finally { if (btn) { btn.disabled = false; btn.textContent = label; } }
  }
  // A dispute's initial fee, in words (fee_payments.go feeStates).
  const FEE_PILL = { PAID: ["filed", "Paid"], UNPAID: ["needs-review", "Unpaid"], PENDING: ["validating", "Pending"],
    FAILED: ["rejected", "Failed"], REFUNDED: ["", "Refunded"] };
  const feePayable = (x) => x.fee_state === "UNPAID" || x.fee_state === "FAILED";
  function feePill(x) {
    if (!x.fee_state) return '<span class="muted">—</span>';
    const [cls, t] = FEE_PILL[x.fee_state] || ["", x.fee_state];
    return `<span class="badge s-${cls}">${t}</span>`
      + (x.fee_state === "FAILED" && x.fee_error ? `<div class="muted fee-note">${esc(x.fee_error)}</div>` : "")
      + (x.fee_state === "PENDING" ? '<div class="muted fee-note">Payment taken, waiting for the bank</div>' : "")
      + (x.fee_state === "UNPAID" && x.fee_checkout_open ? '<div class="muted fee-note">A checkout is open</div>' : "");
  }
  // Every Pay button: the server works out the amount and opens Stripe's checkout.
  // ---- Claims from a CSV (the dispute's Billing tab) ----
  // Columns are matched by name, in any order, the way people head them ("Claim #", "Amount billed", "DOS"...).
  const CLAIM_COLS = [
    ["claim_number", ["claimnumber", "claim", "claimno", "claimid"]],
    ["cpt", ["cpt", "cptcode", "code", "procedurecode", "hcpcs", "cpthcpcs"]],
    ["billed", ["billed", "amountbilled", "billedamount", "charges", "billedcharges", "totalcharges"]],
    ["paid", ["paid", "amountpaid", "paidamount", "allowed", "amountallowed"]],
    ["disputed", ["disputed", "amountindispute", "disputedamount", "indispute", "amountdisputed"]],
    ["patient_first_name", ["patientfirstname", "firstname", "memberfirstname"]],
    ["patient_last_name", ["patientlastname", "lastname", "memberlastname"]],
    ["patient_name", ["patientname", "membername", "patient", "member"]],
    ["type_of_service", ["typeofservice", "servicetype"]],
    ["denial_reason", ["denialreason", "reasonfordenial"]],
    ["date_of_service", ["dateofservice", "dos", "servicedate", "datesofservice"]],
    ["date_claim_submitted", ["dateclaimsubmitted", "datesubmitted", "submitted", "submissiondate"]],
    ["date_of_denial", ["dateofdenial", "denialdate", "datedenied"]],
    ["date_of_final_determination", ["dateoffinaldetermination", "finaldeterminationdate", "finaldetermination"]],
    ["provider_name", ["providername", "provider", "renderingprovider"]],
    ["facility_name", ["facilityname", "facility"]],
    ["evidence_location", ["evidencelocation", "evidence", "document", "source"]],
  ];
  const CLAIM_TEMPLATE_HEAD = ["claim_number", "cpt", "billed", "paid", "disputed", "patient_first_name", "patient_last_name",
    "type_of_service", "denial_reason", "date_of_service", "date_claim_submitted", "date_of_denial",
    "date_of_final_determination", "provider_name", "facility_name", "evidence_location"];
  function csvRows(text) {
    const rows = []; let row = [], cell = "", q = false;
    text = text.replace(/^\uFEFF/, "");
    for (let i = 0; i < text.length; i++) {
      const ch = text[i];
      if (q) {
        if (ch === '"' && text[i + 1] === '"') { cell += '"'; i++; } else if (ch === '"') q = false; else cell += ch;
      } else if (ch === '"') q = true;
      else if (ch === ",") { row.push(cell); cell = ""; }
      else if (ch === "\n" || ch === "\r") { if (ch === "\r" && text[i + 1] === "\n") i++; row.push(cell); rows.push(row); row = []; cell = ""; }
      else cell += ch;
    }
    if (cell !== "" || row.length) { row.push(cell); rows.push(row); }
    return rows.filter((r) => r.some((c) => c.trim() !== ""));
  }
  const csvDate = (v) => {
    v = (v || "").trim();
    if (!v) return "";
    let m = v.match(/^(\d{4})-(\d{1,2})-(\d{1,2})$/);
    if (m) return `${m[1]}-${m[2].padStart(2, "0")}-${m[3].padStart(2, "0")}`;
    m = v.match(/^(\d{1,2})[/-](\d{1,2})[/-](\d{2}|\d{4})$/);
    if (m) { const y = m[3].length === 2 ? "20" + m[3] : m[3]; return `${y}-${m[1].padStart(2, "0")}-${m[2].padStart(2, "0")}`; }
    return null;
  };
  const csvCents = (v) => {
    v = (v || "").trim().replace(/[$,\s]/g, "");
    if (!v) return 0;
    if (/^\(.*\)$/.test(v)) return null;
    const n = Number(v);
    return Number.isFinite(n) && n >= 0 ? Math.round(n * 100) : null;
  };
  function claimsFromCsv(text) {
    const rows = csvRows(text);
    if (rows.length < 2) return { error: "The file needs a header row and at least one claim." };
    const norm = (h) => h.toLowerCase().replace(/[^a-z0-9]/g, "");
    const idx = {};
    rows[0].forEach((h, i) => { const n = norm(h); const hit = CLAIM_COLS.find(([, names]) => names.includes(n)); if (hit && idx[hit[0]] == null) idx[hit[0]] = i; });
    if (idx.claim_number == null) return { error: "No claim number column found — head it “claim_number” (see the CSV template)." };
    const unknown = rows[0].filter((h) => h.trim() && !CLAIM_COLS.some(([, names]) => names.includes(norm(h))));
    const out = [], problems = [];
    rows.slice(1).forEach((r, n) => {
      const get = (k) => (idx[k] == null ? "" : (r[idx[k]] || "").trim());
      const line = n + 2, num = get("claim_number");
      if (!num) { problems.push(`Row ${line}: no claim number — skipped`); return; }
      const c = { claim_number: num, cpt: get("cpt"), type_of_service: get("type_of_service"), denial_reason: get("denial_reason"),
        provider_name: get("provider_name"), facility_name: get("facility_name"), evidence_location: get("evidence_location"),
        patient_first_name: get("patient_first_name"), patient_last_name: get("patient_last_name") };
      if (!c.patient_first_name && !c.patient_last_name && get("patient_name")) {
        const parts = get("patient_name").split(/\s+/);
        c.patient_last_name = parts.length > 1 ? parts.pop() : ""; c.patient_first_name = parts.join(" ");
      }
      for (const [k, key] of [["billed", "billed_cents"], ["paid", "paid_cents"], ["disputed", "disputed_cents"]]) {
        const v = csvCents(get(k));
        if (v === null) problems.push(`Row ${line} (${num}): “${get(k)}” isn't an amount`); else c[key] = v;
      }
      for (const k of ["date_of_service", "date_claim_submitted", "date_of_denial", "date_of_final_determination"]) {
        const v = csvDate(get(k));
        if (v === null) problems.push(`Row ${line} (${num}): “${get(k)}” isn't a date (use MM/DD/YYYY or YYYY-MM-DD)`); else c[k] = v;
      }
      out.push(c);
    });
    return { claims: out, problems, unknown };
  }
  async function claimsCsvUpload(caseId, input) {
    const file = input.files && input.files[0];
    input.value = "";
    if (!file) return;
    const parsed = claimsFromCsv(await file.text());
    if (parsed.error) { UI.toast(parsed.error, { kind: "warn", duration: 9000 }); return; }
    const { claims, problems, unknown } = parsed;
    if (!claims.length) { UI.toast("No claims found in the file.", { kind: "warn" }); return; }
    const usd = (c) => "$" + (c / 100).toLocaleString(undefined, { minimumFractionDigits: 2 });
    const preview = claims.slice(0, 8).map((c) => `<tr><td class="mono">${esc(c.claim_number)}</td><td>${esc(c.cpt || "—")}</td>
      <td>${esc([c.patient_first_name, c.patient_last_name].filter(Boolean).join(" ") || "—")}</td><td>${esc(c.date_of_service || "—")}</td>
      <td class="num">${usd(c.billed_cents || 0)}</td><td class="num">${usd(c.paid_cents || 0)}</td></tr>`).join("");
    const total = claims.reduce((n, c) => n + (c.billed_cents || 0), 0);
    const ok = await UI.modal({ title: `Import ${claims.length} claim${claims.length === 1 ? "" : "s"}`, wide: true,
      submitLabel: problems.length ? "Import the rest" : "Import claims",
      body: `From <b>${esc(file.name)}</b> — ${usd(total)} billed in all.
        <div class="tbl-scroll"><table class="tbl" data-nosearch><thead><tr><th>Claim #</th><th>CPT</th><th>Patient</th><th>Date of service</th><th class="num">Billed</th><th class="num">Paid</th></tr></thead>
        <tbody>${preview}</tbody></table></div>${claims.length > 8 ? `<p class="muted">…and ${claims.length - 8} more.</p>` : ""}
        ${problems.length ? `<p class="warn-line">${problems.length} problem${problems.length === 1 ? "" : "s"} — the cells named are left blank:</p><ul class="muted">${problems.slice(0, 12).map((x) => `<li>${esc(x)}</li>`).join("")}${problems.length > 12 ? `<li>…${problems.length - 12} more</li>` : ""}</ul>` : ""}
        ${unknown.length ? `<p class="muted">Columns not used: ${unknown.map(esc).join(", ")}.</p>` : ""}` });
    if (ok === null) return;
    try {
      const r = await Api.program.importClaims(caseId, claims);
      const vc = r.volume_check;
      const said = `${r.added ?? r.imported} added${r.updated ? `, ${r.updated} updated` : ""}`;
      if (vc && vc.large_volume && (vc.violations || []).length) UI.toast(`${said} — large-volume policy: ${vc.violations.join("; ")}`, { kind: "warn", sticky: true });
      else UI.toast(`Claims imported: ${said}`);
      App.rerender();
    } catch (e) {
      const msg = e.details ? `${esc(e.message)}<br>${e.details.map(esc).join("<br>")}` : esc(e.message);
      UI.toast(msg, { kind: "warn", sticky: true });
    }
  }
  function claimsCsvTemplate() {
    const rows = [CLAIM_TEMPLATE_HEAD,
      ["CLAIM-001", "99285", "1980.00", "0.00", "1980.00", "Ann", "Lee", "Professional", "Not medically necessary", "08/14/2026",
        "08/20/2026", "09/15/2026", "10/01/2026", "Gulf Coast Clinic", "", "UB-04 page 2"]];
    const csv = rows.map((r) => r.map((c) => (/[",\n]/.test(c) ? `"${c.replace(/"/g, '""')}"` : c)).join(",")).join("\r\n") + "\r\n";
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([csv], { type: "text/csv" }));
    a.download = "claims-template.csv";
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 2000);
  }

  async function payGo(req, btn) {
    if (btn) btn.disabled = true;
    try { window.location.assign((await req).checkout_url); }
    catch (e) { if (btn) btn.disabled = false; UI.toast(e.message, { kind: "warn" }); }
  }
  // How a fee is paid on Stripe's page: a card adds the credit card transaction fee, a bank account doesn't.
  async function payWay(amount, cardFee) {
    const known = amount != null;
    const v = await UI.modal({ title: "How would you like to pay?", submitLabel: "Continue to payment",
      fields: [{ name: "method", label: "Pay with", required: true, value: "card", options: [
        ["card", known ? `Card — ${usdC(amount + (cardFee || 0))}${cardFee ? ` (includes the ${usdC(cardFee)} credit card transaction fee)` : ""}` : "Card — adds the credit card transaction fee"],
        ["bank", known ? `Bank account (ACH) — ${usdC(amount)}, no card fee; clears in 3–5 business days` : "Bank account (ACH) — no card fee; clears in 3–5 business days"]] }] });
    return v ? v.method : null;
  }
  const payDisputeFn = async (kind, caseId, btn, from) => {
    if (kind === "tpa") return tpaPayDialog({ case_id: caseId }, btn, from);
    const method = await payWay();
    if (method) payGo(Api.program.payDispute(kind, caseId, from, { method }), btn);
  };
  const payClientFn = (clientId, btn) => tpaPayDialog({ client_id: clientId }, btn);

  // ---- A TPA pays: it chooses each time who pays (tpa_saved_payments.go) ----
  // Each client's own saved card or bank account (one charge per client), the
  // TPA's own, or Stripe's page. Saved-method payments are run by Temporal
  // (FeePaymentWorkflow): a card answers at once, a bank debit (ACH) clears in
  // a few business days and the disputes open when it does.
  const methodWords = (m) => m.method === "ACH" ? `${esc(m.bank_name || "Bank account")} ····${esc(m.last4)} <span class="muted">(ACH)</span>` : `${esc(m.brand ? m.brand[0] + m.brand.slice(1).toLowerCase() : "Card")} ····${esc(m.last4)}`;
  const methodState = (m) => m.chargeable ? "" : m.status === "PENDING_VERIFICATION"
    ? ` <span class="badge s-validating">Confirm the micro-deposits</span>${m.verify_url ? ` <a href="${esc(m.verify_url)}" target="_blank" rel="noopener">confirm</a>` : ""}`
    : m.status === "FAILED" ? ' <span class="badge s-rejected">Not verified</span>' : ' <span class="badge">Saved before Stripe — add it again to charge it</span>';
  function payScrim(inner) {
    const prev = document.activeElement, scrim = document.createElement("div");
    scrim.className = "modal-scrim";
    scrim.innerHTML = `<div class="modal modal-wide" role="dialog" aria-modal="true">${inner}</div>`;
    document.body.appendChild(scrim);
    const close = () => { scrim.remove(); prev?.focus?.(); };
    scrim.addEventListener("mousedown", (e) => { if (e.target === scrim) close(); });
    scrim.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); });
    scrim.querySelectorAll(".modal-x,.m-cancel").forEach((b) => { b.onclick = close; });
    return { scrim, close };
  }
  async function tpaPayDialog(scope, btn, from) {
    if (btn) btn.disabled = true;
    let o;
    try { o = await Api.program.tpaFeeOptions(scope); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); return; }
    finally { if (btn) btn.disabled = false; }
    if (!o.disputes) { UI.toast("Nothing to pay: these disputes are paid (or no fee is due)."); return; }
    const clients = o.clients || [], own = o.tpa_methods || [];
    const ready = (ms) => (ms || []).filter((m) => m.chargeable);
    const clientsReady = clients.filter((c) => ready(c.methods).length).length;
    const clientRows = clients.map((c) => {
      const ms = ready(c.methods);
      return `<tr><td>${esc(c.name || "—")}</td><td class="num">${c.disputes}</td><td class="num">${usdC(c.amount_cents)}</td>
        <td>${ms.length ? `<select name="cm-${esc(c.client_id)}">${ms.map((m) => `<option value="${esc(m.id)}">${methodWords(m).replace(/<[^>]+>/g, "")}</option>`).join("")}</select>`
          : c.client_id ? `<span class="muted">No card or bank account ready</span> <button type="button" class="mini" onclick="Views.tpaMethodAdd('${esc(c.client_id)}')">Add one</button>` : '<span class="muted">—</span>'}</td></tr>`;
    }).join("");
    const ownReady = ready(own);
    const { scrim, close } = payScrim(`
      <div class="modal-h"><h2>Pay ${o.disputes} dispute${o.disputes === 1 ? "" : "s"} — ${usdC(o.amount_cents)}</h2><button class="icon-btn modal-x" aria-label="Close dialog">✕</button></div>
      <form class="modal-b pay-choose" novalidate>
        <p class="modal-body">Who pays this time?</p>
        <label class="pay-opt"><input type="radio" name="source" value="CLIENT" ${clientsReady ? "" : "disabled"} ${clientsReady ? "checked" : ""} />
          <span><b>Each client's own card or bank account</b><span class="muted"> — one charge per client, on the method saved for it</span></span></label>
        <div class="pay-sub" data-for="CLIENT"><table class="tbl"><thead><tr><th>Client</th><th class="num">Disputes</th><th class="num">Fees</th><th>Charged to</th></tr></thead><tbody>${clientRows}</tbody></table>
          ${clientsReady < clients.length ? `<p class="muted">${clients.length - clientsReady} client${clients.length - clientsReady === 1 ? " has" : "s have"} nothing ready on file; ${clients.length - clientsReady === 1 ? "its" : "their"} disputes stay unpaid this time.</p>` : ""}</div>
        <label class="pay-opt"><input type="radio" name="source" value="TPA" ${ownReady.length ? "" : "disabled"} ${!clientsReady && ownReady.length ? "checked" : ""} />
          <span><b>Your organization's card or bank account</b><span class="muted"> — one charge for all of them</span></span></label>
        <div class="pay-sub" data-for="TPA">${ownReady.length ? `<select name="own">${ownReady.map((m) => `<option value="${esc(m.id)}">${methodWords(m).replace(/<[^>]+>/g, "")}</option>`).join("")}</select>`
          : `<span class="muted">Nothing saved yet.</span> <button type="button" class="mini" onclick="Views.tpaMethodAdd('')">Add a card or bank account</button>`}</div>
        <label class="pay-opt"><input type="radio" name="source" value="CHECKOUT" ${!clientsReady && !ownReady.length ? "checked" : ""} />
          <span><b>Card or bank account on Stripe's page</b><span class="muted"> — enter it now, nothing is saved</span></span></label>
        <div class="pay-sub" data-for="CHECKOUT">
          <label class="pay-opt"><input type="radio" name="method" value="card" checked /> <span>Card — <b>${usdC(o.amount_cents + (o.card_fee_cents || 0))}</b>${o.card_fee_cents ? ` <span class="muted">(includes ${usdC(o.card_fee_cents)} in credit card transaction fees)</span>` : ""}</span></label>
          <label class="pay-opt"><input type="radio" name="method" value="bank" /> <span>Bank account (ACH) — <b>${usdC(o.amount_cents)}</b> <span class="muted">no card fee</span></span></label></div>
        <p class="muted">${o.card_fee_cents ? `Paying by card adds the credit card transaction fee to each dispute's fee (${usdC(o.card_fee_cents)} in all here); a bank account or check pays the fee alone. ` : ""}A bank account (ACH debit) takes 3–5 business days to clear; the disputes open when it does. A card opens them straight away.</p>
        <div class="modal-f"><button type="button" class="btn-secondary m-cancel">Cancel</button><button type="submit" class="btn-primary">Pay</button></div>
      </form>`);
    const form = scrim.querySelector("form");
    // What the button pays: by card on Stripe's page, the fees and their card fees; a saved card adds them too.
    const payLabel = () => {
      const v = (form.querySelector("input[name=source]:checked") || {}).value;
      const card = (form.querySelector("input[name=method]:checked") || {}).value !== "bank";
      if (v === "CHECKOUT") return `Pay ${usdC(o.amount_cents + (card ? o.card_fee_cents || 0 : 0))}`;
      return `Pay ${usdC(o.amount_cents)}${o.card_fee_cents ? " + card fees if by card" : ""}`;
    };
    const sync = () => { const v = (form.querySelector("input[name=source]:checked") || {}).value;
      form.querySelectorAll(".pay-sub").forEach((d) => { d.classList.toggle("dim", d.dataset.for !== v); });
      form.querySelector("button[type=submit]").textContent = payLabel(); };
    form.addEventListener("change", sync); sync();
    form.querySelector("input[name=source]:checked")?.focus();
    form.onsubmit = async (e) => {
      e.preventDefault();
      const source = (form.querySelector("input[name=source]:checked") || {}).value;
      if (!source) return;
      const body = { source };
      if (source === "CHECKOUT") body.method = (form.querySelector("input[name=method]:checked") || {}).value || "card";
      if (source === "TPA") body.payment_method_id = form.own.value;
      if (source === "CLIENT") { body.client_methods = {}; form.querySelectorAll("select[name^=cm-]").forEach((s) => { body.client_methods[s.name.slice(3)] = s.value; }); }
      const sbtn = form.querySelector("button[type=submit]"); sbtn.disabled = true; sbtn.textContent = "Paying…";
      const call = scope.case_id ? Api.program.payDispute("tpa", scope.case_id, from, body) : scope.client_id ? Api.program.payClient(scope.client_id, body)
        : scope.batch_id ? Api.program.tpaBulkCheckout(scope.batch_id, body) : Api.program.tpaFeesCheckout(body);
      try {
        const r = await call;
        if (r.checkout_url) { window.location.assign(r.checkout_url); return; }
        close(); tpaRunWatch(r);
      } catch (err) { sbtn.disabled = false; sbtn.textContent = payLabel(); UI.toast(err.message, { kind: "warn", duration: 8000 }); }
    };
  }
  // A saved-method payment as it goes: each charge, until all have an outcome.
  const RUN_STATE = { PAID: ["filed", "Paid"], PROCESSING: ["validating", "Clearing"], FAILED: ["rejected", "Not charged"], REVIEW: ["needs-review", "With finance"] };
  async function tpaRunWatch(r) {
    const { scrim, close } = payScrim(`<div class="modal-h"><h2>Payment</h2><button class="icon-btn modal-x" aria-label="Close dialog">✕</button></div>
      <div class="modal-b"><div id="pay-run"></div>
        ${(r.skipped || []).length ? `<p class="muted">Not charged — nothing ready on file: ${r.skipped.map((x) => `${esc(x.name)} (${x.disputes})`).join(", ")}.</p>` : ""}
        <p class="muted">You can close this; it carries on, and you are told when it finishes.</p>
        <div class="modal-f"><button type="button" class="btn-primary m-cancel">Close</button></div></div>`);
    const box = scrim.querySelector("#pay-run");
    const draw = (charges) => {
      box.innerHTML = `<table class="tbl"><thead><tr><th>For</th><th class="num">Disputes</th><th class="num">Amount</th><th>Charged to</th><th>Status</th></tr></thead><tbody>${charges.map((c) => {
        const [cls, t] = RUN_STATE[c.status] || ["", c.status];
        const m = c.method ? esc(c.method) : methodWords({ method: c.rail, brand: c.brand, bank_name: c.bank_name, last4: c.last4 });
        return `<tr><td>${esc(c.for)}</td><td class="num">${c.disputes}</td><td class="num">${usdC(c.amount_cents)}</td><td>${m}</td>
          <td><span class="badge s-${cls}">${t}</span>${c.error ? `<div class="muted fee-note">${esc(c.error)}</div>` : ""}</td></tr>`; }).join("")}</tbody></table>`;
    };
    draw((r.charges || []).map((c) => ({ ...c, status: "PROCESSING" })));
    for (let i = 0; i < 40 && document.body.contains(box); i++) {
      await new Promise((ok) => setTimeout(ok, i < 10 ? 1500 : 4000));
      let run; try { run = await Api.program.tpaFeeRun(r.run_id); } catch { continue; }
      draw(run.charges || []);
      if ((run.charges || []).every((c) => c.status !== "PROCESSING" || c.rail === "ACH")) break;
    }
    if (window.Views && document.getElementById("tpa-fees")) { tpaFeesBox(); tpaDashBox(); if (App.feature("tpa_onboarding")) tpaBdBatchesBox(); }
  }
  // Save a card or bank account on Stripe's page (for the TPA, or one client).
  async function tpaMethodAdd(clientId) {
    try { window.location.assign((await Api.program.tpaMethodSetup(clientId)).setup_url); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function tpaMethodRemove(id, clientId) {
    if (!(await UI.modal({ title: "Remove this payment method?", body: "It is removed at Stripe too and can no longer be charged.", submitLabel: "Remove", danger: true }))) return;
    try { await Api.program.tpaMethodRemove(id); UI.toast("Removed."); clientId ? tpaPaymentForm(clientId) : tpaOwnMethodsBox(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  const methodList = (ms, clientId) => ms.length ? `<ul class="pm-list">${ms.map((m) => `<li><span>${methodWords(m)}${m.label ? ` <span class="muted">· ${esc(m.label)}</span>` : ""}${methodState(m)}</span>
      <button type="button" class="mini" onclick="Views.tpaMethodRemove('${esc(m.id)}', '${esc(clientId || "")}')">Remove</button></li>`).join("")}</ul>`
    : '<p class="muted">Nothing saved yet.</p>';
  async function tpaOwnMethodsBox() {
    const box = document.getElementById("tpa-own-methods");
    if (!box) return;
    try {
      const ms = (await Api.program.tpaSavedMethods()).payment_methods || [];
      box.innerHTML = methodList(ms, "") + `<p><button class="mini" onclick="Views.tpaMethodAdd('')">Add a card or bank account</button></p>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  // Back from saving a card or bank account on Stripe's page.
  async function tpaSetupReturn() {
    const qs = new URLSearchParams(location.hash.split("?")[1] || "");
    if (!qs.has("setup") && !qs.has("setup_cancelled")) return;
    history.replaceState(null, "", location.pathname + location.hash.split("?")[0]);
    if (qs.has("setup_cancelled")) { UI.toast("Nothing was saved."); return; }
    try {
      const m = await Api.program.tpaMethodSetupConfirm(qs.get("setup"));
      UI.toast(m.status === "ACTIVE" ? `Saved: ${methodWords(m).replace(/<[^>]+>/g, "")} — it can be charged now.`
        : m.status === "PENDING_VERIFICATION" ? "Bank account saved. Two small deposits arrive in 1–2 business days; confirm them (Payment methods) and it can be charged."
        : "Stripe could not verify it — try another card or account.", { duration: 9000 });
      tpaOwnMethodsBox(); if (App.feature("tpa_onboarding")) tpaOnboardingBox();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function hpFeesPay(btn) {
    let f = {};
    try { f = await Api.program.hpFees(); } catch { /* amounts unknown: the choice still says what a card adds */ }
    const method = await payWay(f.amount_cents, f.card_fee_cents);
    if (method) payGo(Api.program.hpFeesCheckout({ method }), btn);
  }
  // Back from Stripe (?paid=1&g=<checkout>) or cancelled at it (?cancelled=<checkout>).
  async function feeReturn(kind, refresh) {
    const qs = new URLSearchParams(location.hash.split("?")[1] || "");
    if (!qs.has("paid") && !qs.has("cancelled")) return;
    history.replaceState(null, "", location.pathname + location.hash.split("?")[0]);
    if (qs.has("cancelled")) {
      let st = "";
      try { st = (await Api.program.checkoutCancel(kind, qs.get("cancelled"))).status; } catch { /* already closed */ }
      UI.toast(st === "PAID" ? "Payment confirmed — your disputes are open." : "Payment cancelled — nothing was charged.");
      refresh();
      return;
    }
    UI.toast("Payment sent — confirming it with Stripe…");
    let st = "";
    if (qs.get("g")) { try { st = (await Api.program.checkoutConfirm(kind, qs.get("g"))).status; } catch { /* the background check settles it */ } }
    if (st === "PAID") { UI.toast("Payment confirmed — your disputes are open."); refresh(); return; }
    if (st === "PROCESSING") { UI.toast("Your payment is being processed — the disputes open when it clears."); refresh(); return; }
    if (kind === "tpa") tpaAwaitPayment(); else refresh();
  }
  async function hpFeesBox() {
    const box = document.getElementById("hp-fees");
    if (!box) return;
    try {
      const f = await Api.program.hpFees();
      box.innerHTML = f.unpaid ? `<div class="tpa-paybar"><div><b>${f.unpaid} dispute${f.unpaid === 1 ? "" : "s"} you filed ${f.unpaid === 1 ? "is" : "are"} waiting for the initial fee — ${usdC(f.amount_cents)}</b>
          <div class="muted">One payment covers all of them. Each dispute opens once paid.</div></div>
          <button onclick="Views.hpFeesPay(this)">Pay all — ${usdC(f.amount_cents)}</button></div>` : "";
      box.innerHTML += `<p><button class="mini secondary" onclick="Views.orgDocsDownload('hp', {}, this)">Download the documents of every dispute you filed</button></p>`;
    } catch { box.innerHTML = ""; }
  }
  // Back from Stripe: the payment is confirmed a few seconds later, so check
  // until nothing is owed (or two minutes pass) and refresh what it changes.
  async function tpaAwaitPayment() {
    for (let i = 0; i < 24; i++) {
      await new Promise((r) => setTimeout(r, 5000));
      if (!document.getElementById("tpa-fees")) return;
      let f; try { f = await Api.program.tpaFees(); } catch { continue; }
      if (!f.unpaid) {
        UI.toast("Payment confirmed — your disputes are open.");
        tpaFeesBox(); tpaDashBox(); if (App.feature("tpa_onboarding")) tpaBdBatchesBox();
        return;
      }
    }
  }
  const tpaFeesPay = (btn) => tpaPayDialog({}, btn);
  // A disputes.csv already holding the TPA's ready clients' exact names, one
  // row each to copy -- a misspelt party_name is the usual reason a row is refused.
  async function tpaBdTemplate(btn) {
    btn.disabled = true;
    try {
      const cs = ((await Api.program.tpaOnboarding()).clients || [])
        .filter((c) => c.status === "ACTIVE" && ["READY", "DOCS_PENDING"].includes(c.onboarding_status));
      if (!cs.length) { UI.toast("No client is ready yet: finish a client's profile and payment method in the Clients tab first.", { kind: "warn" }); return; }
      const q = (v) => /[",\n]/.test(v) ? `"${String(v).replace(/"/g, '""')}"` : v;
      const csv = "dispute_ref,provider_group,party_name,disputed_amount,other_party_name,other_party_email,other_party_naic,contact_name,notes,provider_type,in_network\n" +
        cs.map((c, i) => [`CLAIM-${String(i + 1).padStart(3, "0")}`, q(c.provider_group || ""), q(c.party_name), "", "", "", "", "", "", "", ""].join(",")).join("\n") + "\n";
      const a = document.createElement("a");
      a.href = URL.createObjectURL(new Blob([csv], { type: "text/csv" }));
      a.download = "disputes.csv"; document.body.appendChild(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 10000);
      UI.toast(`disputes.csv with ${cs.length} ready client${cs.length === 1 ? "" : "s"} downloaded — add amounts, one row per dispute.`);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    finally { btn.disabled = false; }
  }
  async function tpaBdSample(layout, btn) {
    await UI.run(btn, async () => {
      try { await Api.program.tpaBulkSample(layout); UI.toast("Sample ZIP downloaded — see README.txt inside"); }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Preparing…");
  }
  const tpaBdPay = (batchId, btn) => tpaPayDialog({ batch_id: batchId }, btn);
  // Eligibility as decided by the Reviewer (eligibility_reviews), in words.
  const ELIG = { ELIGIBLE: ["filed", "Eligible"], INELIGIBLE: ["rejected", "Not eligible"], HOLD_AOR: ["needs-review", "On hold — authorization (AOR)"] };
  const eligBadge = (x) => {
    if (x.eligibility) { const [cls, t] = ELIG[x.eligibility] || ["", x.eligibility]; return `<span class="badge s-${cls}">${esc(t)}</span>`; }
    return x.status === "FILED" ? '<span class="muted">Not decided yet</span>' : '<span class="muted">—</span>';
  };
  const SLOT_LABEL = { EOB: "EOB", ITEMIZED_BILL: "Itemized bill", PROOF_OF_COVERAGE: "Proof of coverage", OPEN_NEGOTIATION_NOTICE: "Open negotiation notice", OFFER_SUBSTANTIATION: "Offer substantiation", IDRE_ATTESTATION: "Attestation" };
  const DOC_VERDICT = { ACCEPTED: ["filed", "Accepted"], NEEDS_REVIEW: ["needs-review", "Waiting for review"], REJECTED: ["rejected", "Rejected"], PENDING: ["validating", "Checking"], ANALYZING: ["validating", "Checking"] };
  // The other side's response, as the filer reads it.
  const theirResponse = (st) => ({ AWAITING: '<span class="badge s-validating">No response yet</span>', SUBMITTED: '<span class="badge s-filed">Responded</span>',
    OPT_OUT_REQUESTED: '<span class="badge s-needs-review">Asked to opt out</span>' }[st] || esc(st));
  async function tpaBdReport(batchId) {
    const box = document.getElementById(`bd-report-${batchId}`) || document.getElementById("tpa-bd-report");
    if (!box) return;
    box.innerHTML = '<p class="muted">Loading…</p>';
    try {
      const items = (await Api.program.tpaBulkDisputeBatch(batchId)).items || [];
      const docs = (x) => !x.package_id ? "—" : `${x.accepted} of 6 accepted${x.needs_review ? ` · ${x.needs_review} need review` : ""}${x.rejected ? ` · <b>${x.rejected} rejected</b>` : ""}`;
      const unpaid = items.filter((x) => x.status === "FILED" && feePayable(x));
      const due = unpaid.reduce((n, x) => n + Number(x.fee_cents || 0), 0);
      const paid = items.filter((x) => x.fee_status === "PAID").length;
      const counts = { ELIGIBLE: 0, INELIGIBLE: 0, HOLD_AOR: 0, none: 0 };
      items.filter((x) => x.status === "FILED").forEach((x) => { counts[x.eligibility || "none"] = (counts[x.eligibility || "none"] || 0) + 1; });
      const detail = (x) => `<div class="bd-item">
          <div><h4>Documents</h4>${(x.docs || []).length ? `<ul class="bd-docs">${x.docs.map((d) => { const [cls, t] = DOC_VERDICT[d.status] || ["", d.status]; return `<li>${esc(SLOT_LABEL[d.slot] || d.slot)} <span class="badge s-${cls}">${esc(t)}</span></li>`; }).join("")}</ul>` : '<p class="muted">None.</p>'}
            ${x.package_id && x.status !== "FILED" ? `<button class="mini" onclick="Views.tpaPackageBox('${x.package_id}')">Open the documents</button>` : ""}</div>
          <div><h4>Eligibility</h4>${eligBadge(x)}${x.eligibility_override ? ' <span class="muted">(override)</span>' : ""}
            ${x.eligibility_reason ? `<p>${esc(x.eligibility_reason)}</p>` : ""}${x.eligibility_at ? `<p class="muted">Decided ${fmtDate(x.eligibility_at)}</p>` : ""}
            ${!x.eligibility && x.status === "FILED" ? `<p class="muted">The reviewer decides eligibility once the dispute is open (its fee paid).</p>` : ""}</div>
          <div><h4>Case</h4>${x.case_number ? `<p><a href="#/cases/${esc(x.case_id)}">${esc(x.case_number)}</a> · ${esc(x.internal_status || x.case_status)}</p>` : '<p class="muted">Not filed yet.</p>'}
            <h4>Against</h4><p>${esc(x.other_party || "—")}${x.response_status ? ` · ${theirResponse(x.response_status)}` : ""}</p>
            ${x.response_status === "AWAITING" && x.response_due ? `<p class="muted">Their response is due ${fmtDate(x.response_due)} · ${countdown(x.response_due_at || x.response_due)}</p>` : ""}
            ${x.notes ? `<p class="muted">${esc(x.notes)}</p>` : ""}${x.error ? `<p class="error">${esc(x.error)}</p>` : ""}</div></div>`;
      box.innerHTML = `
        ${items.some((x) => x.case_id) ? `<p><button class="mini secondary" onclick="Views.orgDocsDownload('tpa', {batch_id: '${batchId}'}, this)">Download this upload's documents</button></p>` : ""}
        ${unpaid.length ? `<div class="tpa-paybar"><div><b>${unpaid.length} filed dispute${unpaid.length === 1 ? "" : "s"} waiting for their fee — ${usdC(due)}</b>
            <div class="muted">One payment covers all of them. Each dispute opens, with its case number and document link, once paid.</div></div>
            <button onclick="Views.tpaBdPay('${batchId}', this)">Pay ${usdC(due)} for ${unpaid.length} dispute${unpaid.length === 1 ? "" : "s"}</button></div>`
          : paid ? `<p class="ok">Every filed dispute in this batch is paid.</p>` : ""}
        <p class="muted">Eligibility of the filed disputes: ${counts.ELIGIBLE} eligible · ${counts.INELIGIBLE} not eligible${counts.HOLD_AOR ? ` · ${counts.HOLD_AOR} on hold` : ""} · ${counts.none} not decided yet. Click a dispute for its details.
          <button class="mini" onclick="Views.tpaBdReport('${batchId}')">Refresh</button></p>
        <div class="bd-wrap"><table class="tbl"><thead><tr><th>Row</th><th>Dispute</th><th>Client</th><th>Against</th><th>Amount</th><th>Documents</th><th>Eligibility</th><th>Status</th><th>Fee</th><th>Case</th></tr></thead><tbody>
        ${items.map((x, n) => `<tr class="bd-row" tabindex="0" onclick="Views.tpaBdItem('${batchId}',${n})" onkeydown="if(event.key==='Enter')Views.tpaBdItem('${batchId}',${n})">
          <td>${x.row_no}</td><td>${esc(String(x.dispute_ref).replace(/ \(row \d+\)$/, ""))}</td><td>${esc(x.party || "—")}${x.provider_group ? `<br><span class="muted">${esc(x.provider_group)}</span>` : ""}</td><td>${esc(x.other_party || "—")}</td>
          <td>${x.disputed_amount_cents ? usdC(x.disputed_amount_cents) : "—"}</td>
          <td>${docs(x)}</td><td>${eligBadge(x)}</td>
          <td>${bdStatus(x)}${x.status === "ERROR" && x.error ? `<br><span class="muted">${esc(x.error)}</span>` : ""}</td>
          <td>${feePill(x)}${feePayable(x) && x.case_id ? `<div><button class="mini" onclick="event.stopPropagation(); Views.payDisputeFn('tpa', '${x.case_id}', this)">Pay now — ${usdC(x.fee_cents)}</button></div>` : ""}</td>
          <td>${x.case_number ? `<a href="#/cases/${esc(x.case_id)}" onclick="event.stopPropagation()">${esc(x.case_number)}</a>` : "—"}</td></tr>
          <tr id="bd-item-${batchId}-${n}" class="row-detail" style="display:none"><td colspan="10">${detail(x)}</td></tr>`).join("")}
        </tbody></table></div>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  function tpaBdItem(batchId, n) {
    const row = document.getElementById(`bd-item-${batchId}-${n}`);
    if (row) row.style.display = row.style.display === "none" ? "" : "none";
  }

  // ---- The other side of a dispute (respondents.go) ----
  // k: "hp" (a health plan answering disputes against it), "tpa" (a TPA
  // answering for its clients) or "party" (an invited responding party).
  const RSP_STATUS = { AWAITING: ["needs-review", "Waiting for your response"], SUBMITTED: ["filed", "Response submitted"], OPT_OUT_REQUESTED: ["validating", "Opt-out requested"] };
  const rspBadge = (st) => { const [cls, label] = RSP_STATUS[st] || ["", st || "—"]; return `<span class="badge s-${cls}">${esc(label)}</span>`; };
  function prettyDocName(fn) {
    const m = /^([a-z][a-z ]+) — (.*)$/i.exec(fn || "");
    if (!m) return fn || "document";
    const label = m[1].toLowerCase() === "eob" ? "EOB" : m[1][0].toUpperCase() + m[1].slice(1);
    const rest = m[2], slug = m[1].toLowerCase().replace(/ /g, "_");
    if (rest.toLowerCase().startsWith(slug)) {
      const pages = /\(pages [^)]*\)/.exec(rest);
      return pages ? `${label} ${pages[0]}` : (rest.toLowerCase() === slug + ".pdf" ? label : `${label} · ${rest}`);
    }
    return `${label} · ${rest}`;
  }
  // ---- Document requests (document_requests.go) ----
  const DOCREQ_REASONS = [["VERIFICATION", "Verification"], ["ELIGIBILITY_RECHECK", "Eligibility re-check"], ["MISSING_DOCUMENT", "Missing document"],
    ["CLEARER_COPY", "Clearer copy needed"], ["RFI", "Request for information"], ["OTHER", "Other"]];
  const DOCREQ_STATUS = { OPEN: ["needs-review", "Waiting for documents"], SUBMITTED: ["validating", "Documents received"],
    COMPLETE: ["filed", "Complete"], CANCELLED: ["", "Withdrawn"] };
  // A request answered with a yes or no (permission, more time) waits for an answer, not documents.
  const docReqBadge = (x) => { const [c, t0] = DOCREQ_STATUS[x.status] || ["", x.status];
    const t = x.status === "OPEN" && (x.reason === "PERMISSION" || x.reason === "EXTENSION") ? "Waiting for an answer"
      : x.status === "SUBMITTED" && x.reason === "EXTENSION" ? "Answered" : t0; return `<span class="badge s-${c}">${t}</span>` + (x.overdue ? ' <span class="badge s-rejected">Overdue</span>' : ""); };
  const docReqItems = (x) => (x.items || []).length ? `<ul class="docreq-items">${x.items.map((i) => `<li>${esc(i)}</li>`).join("")}</ul>` : "";
  const docReqFiles = (x) => (x.files || []).length ? `<div class="muted">Received: ${x.files.map((f) => esc(f.filename)).join(", ")}</div>` : "";
  const docReqCanAsk = () => can("CASE_MANAGER", "PM", "ARBITRATOR", "ATTORNEY", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
  async function caseDocReqBox(caseId) {
    let box = null;
    for (let i = 0; i < 50 && !(box = document.getElementById("docreq-box")); i++) await new Promise((r) => setTimeout(r, 200));
    if (!box) return;
    try {
      const rs = (await Api.program.caseDocRequests(caseId)).requests || [];
      box.innerHTML = (docReqCanAsk() ? `<p><button class="mini" onclick="Views.docReqNew('${caseId}')">Request documents</button>
          <span class="muted">A TPA or health plan on the platform gets it in its portal; anyone else gets a secure upload link by email.</span></p>` : "")
        + (rs.length ? `<table class="tbl"><thead><tr><th>Asked</th><th>Of</th><th>Why</th><th>Documents</th><th>Due</th><th>Status</th><th></th></tr></thead><tbody>
          ${rs.map((x) => `<tr><td>${fmtDate(x.created_at)}<div class="muted">${esc(x.requested_by || "")}</div></td>
            <td>${x.side === "PROVIDER" ? "Provider" : "Health plan"}<div class="muted">${x.in_portal ? "in the portal" : "by email"}</div></td>
            <td>${esc(x.reason_label)}${x.source === "rfi_letter" ? '<div class="muted">with the RFI letter</div>' : ""}</td>
            <td>${docReqItems(x)}${x.message ? `<div class="muted">${esc(x.message)}</div>` : ""}${docReqFiles(x)}${x.response_note ? `<div>“${esc(x.response_note)}”</div>` : ""}</td>
            <td>${esc(x.due_date)}</td><td>${docReqBadge(x)}</td>
            <td class="row-actions">${docReqCanAsk() && (x.status === "OPEN" || x.status === "SUBMITTED") ? `<button class="mini" onclick="Views.docReqClose('${caseId}','${x.id}','complete')">Mark complete</button>
              <button class="mini secondary" onclick="Views.docReqClose('${caseId}','${x.id}','cancel')">Cancel</button>` : ""}</td></tr>`).join("")}</tbody></table>`
          : '<p class="muted">No documents requested on this case.</p>');
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  async function docReqNew(caseId) {
    const v = await UI.modal({
      title: "Request documents",
      body: "The side you ask is notified at once. A TPA or health plan on the platform uploads in its portal; anyone else gets a secure upload link by email.",
      fields: [
        { name: "side", label: "Ask", required: true, options: [["PROVIDER", "The provider's side"], ["HEALTH_PLAN", "The health plan's side"]] },
        { name: "reason", label: "Why", required: true, options: DOCREQ_REASONS },
        { name: "items", label: "Documents needed (one per line)", type: "textarea", required: true, placeholder: "e.g. Proof of coverage for the date of service" },
        { name: "message", label: "Message (optional)", type: "textarea" },
        { name: "due_days", label: "Due in (days)", type: "number", value: "15" },
      ],
      submitLabel: "Send request",
    });
    if (!v) return;
    try {
      const r = await Api.program.createDocRequest(caseId, { side: v.side, reason: v.reason, items: String(v.items || "").split("\n"),
        message: v.message || "", due_days: Number(v.due_days) || 15 });
      UI.toast(r.in_portal ? "Request sent — it is in their portal, and they were emailed" : "Request sent by email with a secure upload link");
      caseDocReqBox(caseId);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function docReqClose(caseId, reqId, action) {
    if (!(await UI.confirm(action === "complete" ? "Mark this request complete?" : "Withdraw this request?",
      action === "complete" ? "You have what you asked for." : "The other side is told nothing more is needed.", action === "complete" ? "Mark complete" : "Withdraw"))) return;
    try { await Api.program.closeDocRequest(caseId, reqId, action); caseDocReqBox(caseId); } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  // The outside organization's side: what it is asked for, and where it answers.
  function docReqPage() {
    const kind = can("TPA") ? "tpa" : "hp";
    orgDocReqBox("docreq-org", kind);
    return `<div class="view-head"><h1>Document requests</h1></div>
      <div class="card"><p class="muted">Documents the review team asked you for. Upload them here and send them — that is your answer; there is no need to reply by email.</p>
        <p class="muted upl-rules">Accepted: PDF up to 100 MB · Word, Excel, PowerPoint up to 50 MB · images up to 30 MB · text and CSV up to 10 MB · ZIP up to 512 MB (at most 2,000 files, no passwords, no archives inside). Every file is scanned for viruses. Files over 8 MB go up in parts you can pause and resume.</p>
        <div id="docreq-org"><p class="muted">Loading…</p></div></div>`;
  }
  async function orgDocReqBox(boxId, kind) {
    let box = null;
    for (let i = 0; i < 50 && !(box = document.getElementById(boxId)); i++) await new Promise((r) => setTimeout(r, 200));
    if (!box) return;
    try {
      const r = await Api.program.orgDocRequests(kind);
      const rs = r.requests || [];
      const tc = document.getElementById((kind === "hp" ? "hp" : "tpa") + "-tc-docreq");
      if (tc) tc.textContent = r.open ? String(r.open) : "";
      box.innerHTML = rs.length ? rs.map((x) => `<div class="docreq ${x.status === "OPEN" ? "open" : ""}" id="docreq-${x.id}">
          <div class="docreq-head"><b class="mono">${esc(x.case_number)}</b> ${docReqBadge(x)} <span class="muted">${esc(x.reason_label)} · asked ${fmtDate(x.created_at)} · due ${esc(x.due_date)}</span></div>
          <div class="muted">${esc(x.filer || "")}${x.other_party ? " · against " + esc(x.other_party) : ""}</div>
          ${docReqItems(x)}${x.message ? `<p>${esc(x.message)}</p>` : ""}
          ${(x.files || []).length ? `<div><b>Uploaded:</b> ${x.files.map((f) => esc(f.filename)).join(", ")}</div>` : ""}
          ${x.status === "OPEN" && x.reason === "PERMISSION" ? `<div class="docreq-perm">
              <p><b>Step: Provider submits authorization/permission</b> — within 15 calendar days · due ${esc(fmtDate(x.due_date))} ${countdown(x.due_date)}</p>
              <form class="inline-form" onsubmit="event.preventDefault(); Views.docReqUploadFn(this, '${kind}', '${x.id}', '${boxId}')">
                <input type="file" name="file" multiple required aria-label="Signed authorization (optional)"><button class="ghost">Upload a signed authorization (optional)</button></form>
              <form class="inline-form" onsubmit="event.preventDefault()">
                <textarea name="note" rows="2" placeholder="A note for the reviewer (optional)"></textarea>
                <button type="button" class="primary" onclick="Views.docReqPermissionFn(this.form, '${kind}', '${x.id}', '${boxId}', 'PROCEED')">Give permission to proceed</button>
                <button type="button" class="btn-secondary" onclick="Views.docReqPermissionFn(this.form, '${kind}', '${x.id}', '${boxId}', 'DECLINE')">Do not proceed</button></form></div>`
          : x.status === "OPEN" && x.reason === "EXTENSION" ? `<div class="docreq-perm">
              <p><b>The review team asks for more time.</b> Answer by ${esc(fmtDate(x.due_date))} ${countdown(x.due_date)}</p>
              <form class="inline-form" onsubmit="event.preventDefault()">
                <textarea name="note" rows="2" placeholder="A note for the review team (optional)"></textarea>
                <button type="button" class="primary" onclick="Views.docReqExtensionFn(this.form, '${kind}', '${x.id}', '${boxId}', 'AGREE')">Agree to the extra time</button>
                <button type="button" class="btn-secondary" onclick="Views.docReqExtensionFn(this.form, '${kind}', '${x.id}', '${boxId}', 'DECLINE')">Decline</button></form></div>`
          : x.status === "OPEN" ? `<form class="inline-form" onsubmit="event.preventDefault(); Views.docReqUploadFn(this, '${kind}', '${x.id}', '${boxId}')">
              <input type="file" name="file" multiple required aria-label="Choose files"><button>Upload</button></form>
            <form class="inline-form" onsubmit="event.preventDefault(); Views.docReqSubmitFn(this, '${kind}', '${x.id}', '${boxId}')">
              <textarea name="note" rows="2" placeholder="A note for the reviewer (optional)"></textarea>
              <button ${(x.files || []).length ? "" : "disabled title=\"Upload the documents first\""}>Send the documents</button></form>`
            : x.response_note ? `<div class="muted">Your note: “${esc(x.response_note)}”</div>` : ""}
        </div>`).join("") : '<p class="muted">No documents have been requested from you.</p>';
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  async function docReqUploadFn(form, kind, reqId, boxId) {
    const btn = form.querySelector("button"); btn.disabled = true;
    try {
      for (const f of form.file.files) await Api.program.orgDocRequestUpload(kind, reqId, f);
      UI.toast(`${form.file.files.length} file${form.file.files.length === 1 ? "" : "s"} uploaded — send them when you have everything`);
      orgDocReqBox(boxId, kind);
    } catch (e) { btn.disabled = false; UI.toast(e.message, { kind: "warn" }); }
  }
  async function docReqPermissionFn(form, kind, reqId, boxId, decision) {
    const yes = decision === "PROCEED";
    if (!(await UI.confirm(yes ? "Give permission to proceed?" : "Decline to proceed?", yes
      ? "The full review goes ahead at the estimated cost, and the health plan is notified of the dispute."
      : "The dispute will not go to full review: it is dismissed and closed, and no invoice is generated.", yes ? "Give permission" : "Do not proceed", !yes))) return;
    try {
      await Api.program.orgDocRequestSubmit(kind, reqId, form.note.value.trim(), decision);
      UI.toast(yes ? "Permission given — the review team is told" : "Recorded: you do not wish to proceed"); orgDocReqBox(boxId, kind);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function docReqExtensionFn(form, kind, reqId, boxId, decision) {
    const yes = decision === "AGREE";
    if (!(await UI.confirm(yes ? "Agree to the extra time?" : "Decline the extra time?", yes
      ? "The deadline moves by the days asked for, and the review team is told."
      : "The deadline stays as it is, and the review team is told.", yes ? "Agree" : "Decline", !yes))) return;
    try {
      await Api.program.orgDocRequestSubmit(kind, reqId, form.note.value.trim(), decision);
      UI.toast(yes ? "Agreed — the deadline is extended" : "Declined — the review team is told"); orgDocReqBox(boxId, kind);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function docReqSubmitFn(form, kind, reqId, boxId) {
    if (!(await UI.confirm("Send these documents?", "The reviewer is told at once. You can't add to this request afterwards.", "Send"))) return;
    try {
      await Api.program.orgDocRequestSubmit(kind, reqId, form.note.value.trim());
      if (window.Telemetry) Telemetry.taskDone("answer_document_request");
      UI.toast("Documents sent to the reviewer"); orgDocReqBox(boxId, kind);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  // An outside filer's dispute page: its open requests, with a link to answer them.
  async function filerDocReqBanner(caseId, kind) {
    let box = null;
    for (let i = 0; i < 50 && !(box = document.getElementById("filer-docreq-" + caseId)); i++) await new Promise((r) => setTimeout(r, 200));
    if (!box) return;
    try {
      const open = ((await Api.program.orgDocRequests(kind)).requests || []).filter((x) => x.case_id === caseId && x.status === "OPEN");
      const perm = open.find((x) => x.reason === "PERMISSION");
      const ext = open.find((x) => x.reason === "EXTENSION");
      if (ext && !perm) {
        box.innerHTML = `<div class="case-lock" role="status"><b>The review team asks for more time.</b> ${esc((ext.items || [])[0] || "")} — answer by ${esc(fmtDate(ext.due_date))} ${countdown(ext.due_date)}
          <a class="button mini" href="#/document-requests">Answer it</a></div>`;
        return;
      }
      if (perm) {
        box.innerHTML = `<div class="case-lock" role="status"><b>Your permission to proceed is needed.</b> ${esc((perm.items || [])[0] || "")} — due ${esc(fmtDate(perm.due_date))} ${countdown(perm.due_date)}
          <a class="button mini" href="#/document-requests">Answer it</a></div>`;
        return;
      }
      box.innerHTML = open.length ? `<div class="case-lock" role="status"><b>Documents requested.</b> The review team needs ${open.length === 1 ? "a document" : open.length + " sets of documents"} for this dispute (due ${esc(open[0].due_date)}).
        <a class="button mini" href="#/document-requests">Upload them</a></div>` : "";
    } catch { box.innerHTML = ""; }
  }
  // My activity: what the signed-in person did, and nothing of anyone else's.
  function auditMineToggle(on) {
    document.getElementById("al-all").hidden = on;
    document.getElementById("al-own").hidden = !on;
    if (on) orgAuditBox("al-own-box", "mine");
  }
  function myActivityPage() {
    (async () => {
      for (let i = 0; i < 50 && !document.getElementById("my-audit"); i++) await new Promise((r) => setTimeout(r, 200));
      orgAuditBox("my-audit", "mine");
    })();
    return `<div class="view-head"><h1>Audit log</h1><span class="muted">your own actions</span></div>
      <div class="card"><p class="muted">Everything you did in the portal, newest first. Your role sees its own actions; the program-wide log is for administrators and the state auditor. Each entry is chained to the one before it, so a removed or altered entry shows.</p>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.orgAuditBox('my-audit', 'mine')">
          <input name="q" placeholder="Search: dispute, action" aria-label="Search your activity">
          <label class="muted"><input type="checkbox" name="views" onchange="Views.orgAuditBox('my-audit', 'mine')"> Include page views</label>
          <button>Search</button></form>
        <div id="my-audit" data-nosearch><p class="muted">Loading…</p></div></div>`;
  }
  // The sidebar's Audit log for a TPA or plan account: its organization's trail.
  function orgAuditPage() {
    const kind = can("TPA") ? "tpa" : "hp";
    (async () => {
      for (let i = 0; i < 50 && !document.getElementById("org-audit"); i++) await new Promise((r) => setTimeout(r, 200));
      orgAuditBox("org-audit", kind);
    })();
    return `<div class="view-head"><h1>Audit log</h1></div>
      <div class="card"><p class="muted">Everything done in your organization's account — by your team, by the office on your behalf, and by the system — newest first. Each entry is chained to the one before it, so a removed or altered entry shows.</p>
        <form class="inline-form" onsubmit="event.preventDefault(); Views.orgAuditBox('org-audit', '${kind}')">
          <input name="q" placeholder="Search: dispute, person, action" aria-label="Search the audit log">
          <label class="muted"><input type="checkbox" name="mine" onchange="Views.orgAuditBox('org-audit', '${kind}')"> Only my actions</label>
          <label class="muted"><input type="checkbox" name="views" onchange="Views.orgAuditBox('org-audit', '${kind}')"> Include page views</label>
          <button>Search</button></form>
        <div id="org-audit" data-nosearch><p class="muted">Loading…</p></div></div>`;
  }
  // An organization's own audit trail (orgaudit.go): the TPA or plan page's "Audit log" tab.
  async function orgAuditBox(boxId, kind) {
    const box = document.getElementById(boxId);
    if (!box) return;
    const form = box.parentElement.querySelector("form");
    const params = {};
    if (form?.q.value.trim()) params.q = form.q.value.trim();
    if (form?.views.checked) params.views = "1";
    if (form?.mine?.checked) kind = "mine";
    box.innerHTML = '<p class="muted">Loading…</p>';
    try {
      const es = (await Api.program.orgAudit(kind, params)).entries || [];
      if (!es.length) { box.innerHTML = '<p class="muted">Nothing recorded yet' + (params.q ? " for this search" : "") + ".</p>"; return; }
      const res = (o) => o === "ok" ? '<span class="badge s-filed">Done</span>'
        : o === "denied" || o === "not_found" ? '<span class="badge s-rejected">Refused</span>' : `<span class="badge s-needs-review">${esc(o)}</span>`;
      box.innerHTML = `<div class="tbl-wrap"><table class="tbl audit-tbl"><thead><tr><th>When</th><th>Who</th><th>What</th><th>Dispute</th><th>Result</th><th>Record</th></tr></thead><tbody>
        ${es.map((e) => `<tr><td class="mono">${esc(String(e.at).replace("T", " ").slice(0, 19))}</td>
          <td>${esc(e.who || "—")}${e.office ? ' <span class="badge">office</span>' : ""}</td>
          <td>${esc(e.what)}${e.detail ? `<div class="muted">${esc(e.detail)}</div>` : ""}</td>
          <td class="mono">${esc(e.case_number || "")}</td><td>${res(e.outcome)}</td>
          <td class="mono muted" title="Entry fingerprint (chained to the previous entry)">${esc(e.hash)}</td></tr>`).join("")}</tbody></table></div>
        <p class="muted">Showing the latest ${es.length} entries.</p>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  async function respondListBox(k) {
    const box = document.getElementById(`${k}-rsp-list`);
    if (!box) return;
    try {
      const all = (await Api.program.respondList(k)).cases || [];
      const waiting = all.filter((x) => x.response_status === "AWAITING").length;
      if (k === "hp") hpTabCount("against", waiting || "");
      const dueSoon = all.filter((x) => x.response_status === "AWAITING" && x.response_due
        && (new Date(x.response_due_at || x.response_due) - Date.now()) / 864e5 <= 3).length;
      const st = respondListBox.filter[k] || "";
      const rows = st ? all.filter((x) => (x.response_status || "") === st) : all;
      const count = (v) => all.filter((x) => (x.response_status || "") === v).length;
      const head = ListUI.summary([
          { label: "Against you", value: all.length, sub: usdC(all.reduce((n, x) => n + Number(x.disputed_amount_cents || 0), 0)) + " disputed" },
          { label: "Waiting for you", value: waiting, sub: waiting ? "send your response" : "nothing waiting", tone: waiting ? "warn" : "good" },
          { label: "Due within 3 days", value: dueSoon, sub: "response deadlines", tone: dueSoon ? "bad" : "" },
          { label: "Responded", value: count("SUBMITTED"), sub: count("OPT_OUT_REQUESTED") ? `${count("OPT_OUT_REQUESTED")} opt-out requested` : "responses sent" },
        ]) + (all.length ? `<div class="list-bar">${ListUI.segs("Response", [{ value: "", label: "All", count: all.length }, { value: "AWAITING", label: "Waiting for you", count: waiting },
          { value: "SUBMITTED", label: "Responded", count: count("SUBMITTED") }, { value: "OPT_OUT_REQUESTED", label: "Opt-out requested", count: count("OPT_OUT_REQUESTED") }]
          .filter((o) => !o.value || o.count), st, `Views.respondFilterFor('${k}')`)}</div>` : "");
      box.innerHTML = head + (rows.length ? `<table class="tbl"><thead><tr><th>Case</th><th>Filed by</th>${k === "tpa" ? "<th>Your client</th>" : ""}<th>Disputed</th><th>Response due</th><th>Status</th><th></th></tr></thead><tbody>
        ${rows.map((x) => `<tr><td class="mono">${esc(x.case_number)}<br><span class="muted">${esc(x.internal_status || x.status)}</span></td>
          <td>${esc(x.initiating_party || "—")}${x.filed_by ? `<br><span class="muted">filed by ${esc(x.filed_by)}</span>` : ""}</td>
          ${k === "tpa" ? `<td>${esc(x.your_client || "—")}</td>` : ""}
          <td>${usdC(x.disputed_amount_cents)}</td><td>${x.response_status !== "AWAITING" ? '<span class="muted">—</span>' : x.response_due ? `${fmtDate(x.response_due)}<br>${countdown(x.response_due_at || x.response_due)}` : '<span class="muted">starts when you are notified</span>'}</td>
          <td>${rspBadge(x.response_status)}</td>
          <td><button class="mini" onclick="Views.respondOpen('${k}','${x.case_id}')">${x.response_status === "AWAITING" ? "Respond" : "Open"}</button></td></tr>`).join("")}</tbody></table>`
        : all.length ? `<p class="muted">No disputes with this response status.</p>`
        : `<p class="muted">No disputes against you right now. When the review team finds a dispute against you eligible, it appears here with its response deadline, and you are emailed.</p>`);
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  respondListBox.filter = {};
  // The response switch's handler (the switch calls fn('<status>')): k is the portal (hp | tpa).
  function respondFilterFor(k) { return (st) => { respondListBox.filter[k] = st; respondListBox(k); }; }
  async function respondOpen(k, caseId) {
    const panel = document.getElementById(`${k}-rsp-panel`);
    if (!panel) return;
    panel.style.display = "";
    try {
      const d = await Api.program.respondGet(k, caseId);
      const c = d.case || {};
      const theirs = (d.documents || []).filter((x) => !x.yours), ours = (d.documents || []).filter((x) => x.yours);
      const docRows = (list) => list.length ? `<ul class="rsp-docs">${list.map((x) => `<li><button type="button" class="linkish" onclick="Views.respondDocOpen('${k}','${caseId}','${x.id}')">${esc(prettyDocName(x.filename))}</button> <span class="muted">${fmtDate(x.created_at)}</span></li>`).join("")}</ul>` : '<p class="muted">None yet.</p>';
      const open = c.response_status === "AWAITING";
      panel.innerHTML = `<div class="tpa-pkg-head"><h2>${esc(c.case_number)} — ${esc(c.initiating_party || "")} vs ${esc(c.respondent || "")}</h2>
          <button class="mini" onclick="document.getElementById('${k}-rsp-panel').style.display='none'">Close</button></div>
        <p>${rspBadge(c.response_status)} · disputed ${usdC(c.disputed_amount_cents)}${c.disputed_issue ? ` · ${esc(c.disputed_issue)}` : ""}${c.filed_by ? ` · filed by ${esc(c.filed_by)}` : ""}</p>
        ${open && c.response_due ? `<div class="rsp-due"><div><span class="lbl">Health Plan response period — up to 15 calendar days</span></div>
          <div><span class="lbl">Your response is due</span> <b>${fmtDate(c.response_due)}</b> <span class="muted">end of day, Eastern · a reminder of the due date is sent on the 13th day</span></div>
          <div class="rsp-due-cd">${countdown(c.response_due_at || c.response_due)}</div>
          <div class="muted">${c.response_clock && c.response_clock.source === "recorded" ? "Date set by the review team." : c.response_clock ? `${c.response_clock.days} ${c.response_clock.day_type} days from when you were notified${c.response_clock.started ? ` (${fmtDate(c.response_clock.started)})` : ""}. No response by then becomes a default determination.` : ""}</div></div>` : ""}
        <details class="rsp-record" open><summary><b>Dispute details</b></summary><div id="${k}-rsp-summary"><p class="muted">Loading…</p></div></details>
        <details class="rsp-record" open><summary><b>Letters and documents sent to you</b></summary><div id="${k}-rsp-record"><p class="muted">Loading…</p></div></details>
        <div class="rsp-grid"><div><h3>What the filer submitted</h3>${docRows(theirs)}</div>
          <div><h3>Your response documents</h3>${docRows(ours)}
          ${open ? `<form class="inline-form" style="padding-left:0" onsubmit="event.preventDefault(); Views.respondUploadFn(this,'${k}','${caseId}')">
              <input name="file" type="file" required /><button class="mini">Add document</button></form>` : ""}</div></div>
        ${open ? `<form class="rsp-submit" onsubmit="event.preventDefault(); Views.respondSubmitFn(this,'${k}','${caseId}')">
            <label>Your response (optional when you added documents)<textarea name="note" rows="3" placeholder="e.g. QPA methodology attached; the claim was paid at the median contracted rate."></textarea></label>
            <div class="rsp-actions"><button>Submit response</button>
              ${k === "hp" ? `<button type="button" class="mini" onclick="Views.respondOptOutFn('${caseId}')">Ask to opt out</button>` : ""}</div>
            <p class="muted">Submitting counts as your response straight away; the reviewer is told and the case moves on.</p></form>`
          : `<p class="muted">${c.response_status === "SUBMITTED" ? `Submitted ${fmtDate(c.responded_at)}.` : `Opt-out requested ${fmtDate(c.responded_at)} — the attorney decides.`}${c.response_note ? ` “${esc(c.response_note)}”` : ""}</p>`}`;
      sentRecordInto(document.getElementById(`${k}-rsp-record`), k, caseId);
      disputeSummaryInto(document.getElementById(`${k}-rsp-summary`), k, caseId);
      panel.scrollIntoView({ behavior: "smooth", block: "start" });
    } catch (e) { panel.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  async function respondDocOpen(k, caseId, docId) {
    try {
      const url = await Api.program.respondFile(k, caseId, docId);
      await UI.modal({ title: "Document", wide: true, submitLabel: "Close",
        body: `<iframe class="onb-doc" src="${url}" title="Document"></iframe><p class="muted"><a href="${url}" download="document.pdf">Download</a></p>` });
      URL.revokeObjectURL(url);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function respondUploadFn(form, k, caseId) {
    try { await Api.program.respondUpload(k, caseId, form.file.files[0]); UI.toast("Document added"); respondOpen(k, caseId); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function respondSubmitFn(form, k, caseId) {
    if (!(await UI.confirm("Submit your response?", "It counts as your response straight away; you can't change it afterwards.", "Submit"))) return;
    try {
      const r = await Api.program.respondSubmit(k, caseId, form.note.value.trim());
      UI.toast(r.workflow_signalled === false ? "Response recorded — the reviewer will move the case on." : "Response submitted");
      respondOpen(k, caseId); if (k !== "party") respondListBox(k);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function respondOptOutFn(caseId) {
    const reason = (prompt("Why does the plan ask to opt out? The attorney decides.") || "").trim();
    if (!reason) return;
    try { await Api.program.respondOptOut(caseId, reason); UI.toast("Opt-out requested — the attorney decides"); respondOpen("hp", caseId); respondListBox("hp"); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
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
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // Undo a claim made with a TPA-supplied code -- back to the oversight-only
  // view of #/tpa (the claim form + admin list, same as never having
  // claimed anything).
  async function tpaUnlinkFn() {
    if (!(await UI.confirm("Stop acting as this TPA?", "Unlinks your own account from this organization. You'll go back to the oversight list; claim a code again any time to act on a TPA's behalf.", "Unlink"))) return;
    try { await Api.program.tpaUnlink(); UI.toast("Unlinked"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function hpUnlinkFn() {
    if (!(await UI.confirm("Stop acting as this plan?", "Unlinks your own account from this health plan. You'll go back to the oversight list; claim a code again any time to act for a plan.", "Unlink"))) return;
    try { await Api.program.hpUnlink(); UI.toast("Unlinked"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // Documents doc-intel could not decide (illegible, ambiguous type, a slot
  // the merged-PDF split did not find, a plan offer without the QPA): a person
  // opens each one and accepts or rejects it. The reviewer and the PM decide.
  const ONB_REASONS = {
    LEGIBILITY_LOW: "Too little readable text", TYPE_UNCLEAR: "Document type not recognised",
    TYPE_AMBIGUOUS: "Document type unclear", FIELDS_MISSING: "Claim number / codes not found",
    KEYWORDS_MISSING: "Doesn't say what it is", QPA_DOC_MISSING: "Offer doesn't document the QPA",
    SLOT_NOT_FOUND_IN_MERGED: "Not found in the merged PDF", ANALYZER_ERROR: "Could not be read",
  };
  async function onboardingReviewsBox(boxId, kind) {
    const box = document.getElementById(boxId);
    if (!box) return;
    const decides = can("CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    try {
      const rows = ((await Api.program.onboardingReviews()).reviews || []).filter((x) => x.kind === kind);
      if (kind === "tpa") tpaTabCount("reviews", rows.length || "");
      if (!rows.length) { box.innerHTML = '<p class="muted">Nothing waiting — every document has a decision.</p>'; return; }
      box.innerHTML = `<table class="tbl"><thead><tr><th>Organization · party</th><th>Document</th><th>Why it needs a person</th><th></th></tr></thead><tbody>
        ${rows.map((x) => `<tr><td>${esc(x.organization)}<br><span class="muted">${esc(x.party)}</span></td>
          <td>${esc(x.slot.replace(/_/g, " ").toLowerCase())}<br><span class="muted">${esc(x.filename)}</span></td>
          <td>${(x.reasons || []).filter((r) => r.code !== "SPLIT_FROM_MERGED").map((r) => `${esc(ONB_REASONS[r.code] || r.code)}<br><span class="muted">${esc(r.detail || "")}</span>`).join("<br>")}</td>
          <td style="white-space:nowrap"><button class="mini" onclick="Views.onbReviewOpen('${kind}','${x.doc_id}')">Open</button>
            ${decides ? `<button class="mini" onclick="Views.onbReviewDecide('${boxId}','${kind}','${x.doc_id}','ACCEPT')">Accept</button>
            <button class="mini danger" onclick="Views.onbReviewDecide('${boxId}','${kind}','${x.doc_id}','REJECT')">Reject</button>` : ""}</td></tr>`).join("")}</tbody></table>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  async function npiReviewsBox(boxId = "tpa-npi-reviews", kind = "tpa") {
    const box = document.getElementById(boxId);
    if (!box) return;
    const decides = can("CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    try {
      const rows = ((await Api.program.npiReviews()).reviews || []).filter((x) => (x.kind || "tpa") === kind);
      if (!rows.length) { box.innerHTML = `<p class="muted">Nothing waiting — every ${kind === "hp" ? "party" : "client"} NPI matches the registry or has a decision.</p>`; return; }
      box.innerHTML = `<table class="tbl"><thead><tr><th>${kind === "hp" ? "Health plan · party" : "TPA · client"}</th><th>NPI</th><th>The registry holds it as</th><th></th></tr></thead><tbody>
        ${rows.map((x) => `<tr><td>${esc(x.organization)}<br><span class="muted">${esc(x.party_name)}</span></td>
          <td>${esc(x.npi)}<br><a class="muted" href="https://npiregistry.cms.hhs.gov/provider-view/${esc(x.npi)}" target="_blank" rel="noopener">open in NPPES</a></td>
          <td>${x.registry_found ? `<b>${esc(x.registry_name)}</b>${(x.other_names || []).length ? `<br><span class="muted">also: ${x.other_names.map(esc).join("; ")}</span>` : ""}
            <br><span class="muted">${esc([x.enumeration_type === "NPI-1" ? "an individual" : x.enumeration_type === "NPI-2" ? "an organization" : "", x.taxonomy, x.state].filter(Boolean).join(" · "))}</span>`
            : '<span class="error">Not in the registry any more (deactivated since it was entered)</span>'}</td>
          <td style="white-space:nowrap">${decides ? `<button class="mini" onclick="Views.npiReviewDecideFn('${kind}','${x.id}','CONFIRM','${boxId}')">Confirm</button>
            <button class="mini danger" onclick="Views.npiReviewDecideFn('${kind}','${x.id}','REJECT','${boxId}')">Reject</button>` : ""}</td></tr>`).join("")}</tbody></table>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  async function exclusionReviewsBox(boxId, kind) {
    const box = document.getElementById(boxId);
    if (!box) return;
    const decides = can("CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    try {
      const r = await Api.program.verificationReviews();
      const rows = (r.reviews || []).filter((x) => x.kind === kind);
      const listNote = r.list_rows ? `<p class="muted">Exclusion list loaded ${fmtDate(r.list_loaded_at)} (${Number(r.list_rows).toLocaleString()} entries); it refreshes weekly.</p>`
        : '<p class="warn">The exclusion list has not loaded yet, so nothing has been checked against it. It loads automatically; clients are checked once it does.</p>';
      if (!rows.length) { box.innerHTML = listNote + '<p class="muted">Nothing waiting — no possible matches.</p>'; return; }
      box.innerHTML = listNote + `<table class="tbl"><thead><tr><th>${kind === "hp" ? "Health plan · party" : "TPA · client"}</th><th>NPI</th><th>The exclusion list holds</th><th></th></tr></thead><tbody>
        ${rows.map((x) => `<tr><td>${esc(x.organization)}<br><span class="muted">${esc(x.party_name)}</span></td><td>${esc(x.npi || "—")}</td>
          <td>${(x.matches || []).map((m) => `<b>${esc(m.name)}</b> <span class="badge s-ck-review">matched on ${m.matched_on === "npi" ? "NPI" : "name"}</span>
            <br><span class="muted">${esc([m.npi && "NPI " + m.npi, m.general, m.specialty, m.state, m.excluded_since && "excluded since " + m.excluded_since.replace(/(\d{4})(\d{2})(\d{2})/, "$2/$3/$1"), m.exclusion_type].filter(Boolean).join(" · "))}</span>`).join("<br>")}
            <br><a class="muted" href="https://exclusions.oig.hhs.gov/" target="_blank" rel="noopener">search the OIG site</a></td>
          <td style="white-space:nowrap">${decides ? `<button class="mini" onclick="Views.exclusionDecideFn('${x.review_id}','CLEAR','${boxId}','${kind}')">Clear</button>
            <button class="mini danger" onclick="Views.exclusionDecideFn('${x.review_id}','EXCLUDED','${boxId}','${kind}')">Excluded</button>` : ""}</td></tr>`).join("")}</tbody></table>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  async function exclusionDecideFn(reviewId, decision, boxId, kind) {
    const q = decision === "CLEAR" ? "How do you know this is a different provider? (for example: different address and specialty)" : "What confirmed it is the same provider? The filer sees this note.";
    const note = (prompt(q) || "").trim();
    if (!note) return;
    try {
      await Api.program.verificationReviewDecide(reviewId, decision, note);
      UI.toast(decision === "CLEAR" ? "Cleared — filing can go ahead" : "Marked as excluded — filing stays blocked");
      exclusionReviewsBox(boxId, kind);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function npiReviewDecideFn(kind, id, decision, boxId) {
    let note = "";
    if (decision === "REJECT") {
      note = (prompt("Why is this NPI rejected? The TPA sees this note.") || "").trim();
      if (!note) return;
    } else if (!(await UI.confirm("Confirm this NPI?", "It belongs to this provider under the name the registry shows. Filing can then go ahead.", "Confirm"))) return;
    try {
      await Api.program.npiReviewDecide(kind, id, decision, note);
      UI.toast(decision === "CONFIRM" ? "NPI confirmed — filing can go ahead" : `NPI rejected — the ${kind === "hp" ? "health plan" : "TPA"} is shown your note`);
      npiReviewsBox(boxId, kind);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  // Only the chosen client's (or party's) READY packages can be filed with it.
  function filingPackagesFilter(ownerSelId, pkgSelId, attr) {
    const owner = document.getElementById(ownerSelId), sel = document.getElementById(pkgSelId);
    if (!owner || !sel) return;
    for (const o of sel.options) if (o.value) o.hidden = o.dataset[attr] !== owner.value;
    if (sel.selectedOptions[0]?.hidden) sel.value = "";
  }
  async function onbReviewOpen(kind, docId) {
    // Shown inside the portal: a new tab can't load a file this page fetched
    // (pop-up blockers stop a tab opened after the download, and the browser
    // keeps the downloaded file to this page).
    try {
      const url = await Api.program.onboardingReviewFile(kind, docId);
      await UI.modal({ title: "Document under review", wide: true, submitLabel: "Close",
        body: `<iframe class="onb-doc" src="${url}" title="Document under review"></iframe>
          <p class="muted"><a href="${url}" download="review-${esc(docId.slice(0, 8))}.pdf">Download</a> — then accept or reject it in the list.</p>` });
      URL.revokeObjectURL(url);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function onbReviewDecide(boxId, kind, docId, decision) {
    let note = "";
    if (decision === "REJECT") {
      note = (prompt("Why is this document rejected? The filer sees this note.") || "").trim();
      if (!note) return;
    } else if (!(await UI.confirm("Accept this document?", "It counts as checked for its slot. The package is ready once all six slots are accepted.", "Accept"))) return;
    try {
      const r = await Api.program.onboardingReviewDecide(kind, docId, decision, note);
      UI.toast(`${decision === "ACCEPT" ? "Accepted" : "Rejected"} — package ${String(r.package_status || "").toLowerCase()}`);
      onboardingReviewsBox(boxId, kind);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ── Physical check intake + scan preview ───────────────────────────
  // Checks are part of Financials now: the old address opens that section.
  function checksView() {
    location.replace("#/finance");
    scrollToChecks();
    return `<p class="muted">Checks are on Financials — opening…</p>`;
  }

  async function checksBox(status) {
    const box = document.getElementById("checks-list");
    if (!box) { App.rerender(); return; } // on Financials: redraw its Checks section
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
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
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


  // -------------------------------------------------------------------------
  // Payments: bank accounts, bank files (moov-io: ACH, Fedwire, X9.37, Metro 2)
  // and OFAC sanctions screening. One page, four tabs; the data is loaded once
  // into payData and each tab renders from it.
  // -------------------------------------------------------------------------

  function money(c) { return "$" + (Number(c || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }); }

  const PAY_KIND = { ACH_DEBIT: "ACH fee debit", ACH_RETURN: "ACH return file", WIRE: "Wire transfer", ICL: "Check deposit (cash letter)", METRO2: "Credit-bureau file" };
  const PAY_OWNER = { PARTY: "Provider or health plan", TPA: "Third-party administrator", VENDOR: "Vendor", ARBITRATOR: "Independent reviewer" };
  let payTab = "accounts", payData = null;

  async function paymentsView() {
    if (!can("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN"))
      return `<div class="view-head"><h1>Payments</h1></div><p class="muted">Requires PM, FINANCE, or admin.</p>`;
    const html = `<div class="view-head"><h1>Payments</h1>
        <span class="muted">Bank accounts, the files your bank needs, and sanctions screening for this program</span></div>
      <div id="pay-sum" class="pay-sum"><div class="pay-tile"><div class="k">Loading…</div></div></div>
      <div class="rpt-tabs" role="tablist" id="pay-tabs"></div>
      <div id="pay-panel"><p class="muted">Loading…</p></div>`;
    return { html, wire: () => payLoad() };
  }

  async function payLoad() {
    const fin = can("FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const [prog, accts, files, status] = await Promise.all([
      Api.program.get().catch(() => null),
      Api.program.railAccounts().catch((e) => ({ error: e.message })),
      Api.program.railFiles("").catch((e) => ({ error: e.message })),
      Api.program.screeningStatus().catch((e) => ({ error: e.message })),
    ]);
    const n = prog?.config?.bank?.nacha || {};
    payData = {
      fin, status, bankReady: !!(n.originating_dfi && n.immediate_destination && n.immediate_origin),
      accounts: accts.accounts || [], accountsError: accts.error,
      files: files.files || [], filesError: files.error,
      invoices: null, payables: null, orgs: null,
    };
    payRender();
  }

  function payTabFn(tab) { payTab = tab; payRender(); }

  function payRender() {
    const d = payData, sum = document.getElementById("pay-sum"), tabs = document.getElementById("pay-tabs"), panel = document.getElementById("pay-panel");
    if (!d || !sum || !panel) return;
    const authorized = d.accounts.filter((a) => a.ach_authorization_name).length;
    const returns = d.files.filter((f) => f.kind === "ACH_RETURN").length;
    const st = d.status || {};
    sum.innerHTML = `
      <div class="pay-tile ${d.bankReady ? "" : "warn"}"><div class="k">Program bank details</div>
        <div class="v">${d.bankReady ? "On file" : "Not set"}</div>
        <div class="s">${d.bankReady ? "ACH, wires and check deposits can be built" : "Needed for ACH, wires and check deposits"}</div></div>
      <div class="pay-tile"><div class="k">Bank accounts</div><div class="v">${d.accounts.length}</div>
        <div class="s">${authorized} authorized for ACH debit</div></div>
      <div class="pay-tile"><div class="k">Files built</div><div class="v">${d.files.length - returns}</div>
        <div class="s">${returns} return file${returns === 1 ? "" : "s"} received</div></div>
      <div class="pay-tile ${st.error || st.last_error || !st.enabled ? "warn" : ""}"><div class="k">Sanctions list (OFAC)</div>
        <div class="v">${st.enabled ? (st.entity_count || 0).toLocaleString() + " names" : "Off"}</div>
        <div class="s">${st.error ? esc(st.error) : st.last_error ? "Last update failed: " + esc(st.last_error) : st.refreshed_at ? "Updated " + fmtDate(st.refreshed_at) : "Not downloaded yet"}</div></div>`;
    const tab = (k, label) => `<button type="button" role="tab" class="rpt-tab rpt-accent-4 ${payTab === k ? "active" : ""}" onclick="Views.payTabFn('${k}')">${label}</button>`;
    tabs.innerHTML = tab("accounts", `Bank accounts (${d.accounts.length})`) + tab("files", "Bank files") + tab("history", `History (${d.files.length})`) + tab("screening", "Sanctions screening");
    panel.innerHTML = payTab === "accounts" ? payAccountsPanel() : payTab === "files" ? payFilesPanel() : payTab === "history" ? payHistoryPanel() : payScreeningPanel();
    if (payTab === "files") payLoadPickers();
    if (payTab === "screening") screenLoad(false);
    if (payTab === "accounts" && d.fin && !d.orgs) {
      Api.crm.accounts("", { limit: 500 }).then((r) => {
        d.orgs = (r.accounts || []).map((a) => a.name).filter(Boolean);
        const dl = document.getElementById("pay-orgs");
        if (dl) dl.innerHTML = d.orgs.map((o) => `<option value="${esc(o)}">`).join("");
      }).catch(() => {});
    }
  }

  // ---- Bank accounts ----
  function payAccountsPanel() {
    const d = payData;
    const rows = d.accounts.map((a) => `<tr>
        <td><b>${esc(a.owner_ref)}</b><div class="muted">${esc(PAY_OWNER[a.owner_type] || a.owner_type)}${a.label ? " · " + esc(a.label) : ""}</div></td>
        <td class="mono">${esc(a.routing_number)}${a.bank_name ? `<div class="muted">${esc(a.bank_name)}</div>` : ""}</td>
        <td class="mono">•••• ${esc(a.account_last4)}<div class="muted">${a.account_type === "SAVINGS" ? "Savings" : "Checking"}</div></td>
        <td>${a.ach_authorization_name
            ? `<span class="badge s-paid">Authorized</span><div class="muted">Signed form: ${esc(a.ach_authorization_name)}${a.ach_authorization_at ? " · " + esc(String(a.ach_authorization_at).slice(0, 10)) : ""}</div>`
            : a.ach_debit_authorized ? '<span class="badge pay-badge-bad">Signed form missing</span>' : '<span class="muted">Not authorized</span>'}</td>
        <td class="pay-actions">${d.fin ? `<label class="mini ghost pay-file-btn">${a.ach_authorization_name ? "Replace signed form" : "Attach signed form"}
            <input type="file" accept=".pdf,.png,.jpg,.jpeg,.tif,.tiff" onchange="Views.railAttachAuthFn('${a.id}', this)" hidden /></label>
          <button class="mini ghost" onclick="Views.railAccountDelFn('${a.id}', this)">Remove</button>` : ""}</td></tr>`).join("");
    return `<p class="muted pay-lead">Accounts that money is collected from or paid to. The full account number is encrypted the moment it is saved; only the last four digits are ever shown.</p>
      ${d.accountsError ? `<p class="error">${esc(d.accountsError)}</p>` : ""}
      ${d.fin ? `<details class="pay-add" ${d.accounts.length ? "" : "open"}><summary><b>Add a bank account</b></summary>
        <form class="pay-form" onsubmit="event.preventDefault(); Views.railAccountAddFn(this)">
          <div class="pay-row">
            <label>Belongs to<select name="owner_type">${Object.entries(PAY_OWNER).map(([k, v]) => `<option value="${k}">${v}</option>`).join("")}</select></label>
            <label>Organization or person<input name="owner_ref" list="pay-orgs" placeholder="Start typing a name" required autocomplete="off" /></label>
            <label><span>Nickname <span class="muted">(optional)</span></span><input name="label" placeholder="e.g. Operating account" /></label>
          </div>
          <div class="pay-row">
            <label>Routing number<input name="routing_number" inputmode="numeric" maxlength="9" placeholder="9 digits" required onblur="Views.railRoutingCheck(this)" />
              <span class="pay-hint" id="pay-rt-hint"></span></label>
            <label>Account number<input name="account_number" inputmode="numeric" maxlength="17" placeholder="4–17 digits" required autocomplete="off" /></label>
            <label>Account type<select name="account_type"><option value="CHECKING">Checking</option><option value="SAVINGS">Savings</option></select></label>
          </div>
          <label class="chk"><input type="checkbox" name="ach_debit_authorized" /> The owner has signed an ACH debit authorization — attach the signed form after saving; fees are only collected from accounts with the form on file</label>
          <div><button>Save account</button> <span id="pay-acct-msg"></span></div>
        </form><datalist id="pay-orgs">${(d.orgs || []).map((o) => `<option value="${esc(o)}">`).join("")}</datalist></details>`
        : `<p class="muted">Only Finance adds or removes bank accounts.</p>`}
      ${rows ? `<div class="pay-table-wrap"><table><thead><tr><th>Belongs to</th><th>Routing / bank</th><th>Account</th><th>ACH debit</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`
        : `<p class="muted">No bank accounts yet.</p>`}`;
  }

  async function railRoutingCheck(input) {
    const hint = document.getElementById("pay-rt-hint"), v = input.value.trim();
    if (!hint) return;
    if (!v) { hint.textContent = ""; return; }
    if (!/^\d{9}$/.test(v)) { hint.className = "pay-hint pay-bad"; hint.textContent = "A routing number is 9 digits."; return; }
    try {
      const r = await Api.program.railRouting(v);
      hint.className = "pay-hint pay-good"; hint.textContent = "✓ Valid routing number" + (r.bank_name ? " — " + r.bank_name : "");
    } catch { hint.className = "pay-hint pay-bad"; hint.textContent = "✗ Not a valid routing number — check the digits."; }
  }

  async function railAccountAddFn(form) {
    const f = form, msg = document.getElementById("pay-acct-msg");
    const btn = f.querySelector("button");
    btn.disabled = true;
    try {
      await Api.program.railAccountAdd({
        owner_type: f.owner_type.value, owner_ref: f.owner_ref.value.trim(), label: f.label.value.trim(),
        routing_number: f.routing_number.value.trim(), account_number: f.account_number.value.trim(),
        account_type: f.account_type.value, ach_debit_authorized: f.ach_debit_authorized.checked,
      });
      UI.toast("Bank account saved");
      await payLoad();
    } catch (e) { if (msg) { msg.className = "pay-bad"; msg.textContent = e.message; } btn.disabled = false; }
  }

  async function railAttachAuthFn(id, input) {
    const file = input && input.files && input.files[0];
    if (!file) return;
    if (file.size > 8 * 1024 * 1024) { UI.toast("The signed form must be 8 MB or smaller", { kind: "warn" }); input.value = ""; return; }
    try {
      // ArrayBuffer, not text(): PDFs/scans are binary and must not round through UTF-8.
      await Api.program.railAttachAuth(id, await file.arrayBuffer(), file.name);
      UI.toast("Signed authorization attached");
      await payLoad();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); input.value = ""; }
  }

  async function railAccountDelFn(id, btn) {
    const a = payData.accounts.find((x) => x.id === id);
    if (!(await UI.confirm("Remove this bank account?", `The account ending ${a ? a.account_last4 : ""}${a ? " for " + a.owner_ref : ""} will no longer be offered for debits or wires. Files already built are not affected.`, "Remove", true))) return;
    if (btn) btn.disabled = true;
    try { await Api.program.railAccountDel(id); UI.toast("Bank account removed"); await payLoad(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); if (btn) btn.disabled = false; }
  }

  // ---- Bank files ----
  function payFilesPanel() {
    const d = payData, off = d.bankReady ? "" : " disabled";
    if (!d.fin) return `<p class="muted pay-lead">Only Finance builds bank files. Everything built so far is under History.</p>`;
    const authorized = d.accounts.filter((a) => a.ach_authorization_name);
    const acctOpt = (list) => list.map((a) => `<option value="${a.id}">${esc(a.owner_ref)} · •••• ${esc(a.account_last4)}</option>`).join("");
    return `${d.bankReady ? "" : `<div class="pay-note">ACH debits, wires and check deposits go out under <b>this program's own bank details</b>: its bank's routing number, the receiving bank, and its company ID. None are on file yet, so those three are switched off. They are set by the platform team when the program's bank account is opened, not on this screen.</div>`}
      <div id="rail-gen-result"></div>
      <div class="pay-cards">
        <div class="card pay-card"><h3>Collect fees by ACH debit</h3>
          <p class="what">Builds a NACHA file that pulls open invoices straight from each payer's bank account. Your bank runs it.</p>
          <p class="needs">Needs: program bank details · the payer's account with its signed debit authorization attached.</p>
          <form class="pay-form" onsubmit="event.preventDefault(); Views.railAchDebitFn(this)">
            <div id="pay-ach-rows">${authorized.length ? '<p class="muted">Loading open invoices…</p>' : '<p class="muted">No account has a signed debit authorization yet — attach one under Bank accounts first.</p>'}</div>
            <label>Debit date<input name="effective_date" type="date" /></label>
            <div><button${off || (authorized.length ? "" : " disabled")}>Build debit file</button></div></form></div>

        <div class="card pay-card"><h3>Pay by wire</h3>
          <p class="what">Builds a Fedwire transfer for one open payable — for large or same-day payments.</p>
          <p class="needs">Needs: program bank details · an open payable · the payee's bank account.</p>
          <form class="pay-form" onsubmit="event.preventDefault(); Views.railWireFn(this)">
            <label>What to pay<select name="payable_id" id="pay-wire-payable" required><option value="">Loading open payables…</option></select></label>
            <label>Send to account<select name="payment_account_id" required>${d.accounts.length ? '<option value="">Choose the account</option>' + acctOpt(d.accounts) : '<option value="">No bank accounts yet</option>'}</select></label>
            <div><button${off || (d.accounts.length ? "" : " disabled")}>Build wire file</button></div></form></div>

        <div class="card pay-card"><h3>Deposit cleared checks</h3>
          <p class="what">Bundles every cleared paper check that hasn't been deposited yet into one X9.37 cash letter, with the check images, for your bank's remote deposit.</p>
          <p class="needs">Needs: program bank details · checks marked cleared on the Checks page.</p>
          <div class="pay-form"><div><button onclick="Views.railIclFn(this)"${off}>Build cash letter</button></div></div></div>
      </div>
      <details class="card pay-card pay-regulated"><summary><b>Credit-bureau reporting (Metro 2)</b> <span class="muted">— regulated; check with compliance first</span></summary>
        <p class="pay-note">Metro 2 is the format for reporting unpaid debts to the credit bureaus, and doing so is regulated by the Fair Credit Reporting Act. It is built for reporting consumers; the parties on this program are providers and health plans. Use it only if your compliance team has approved it.</p>
        <form class="pay-form" onsubmit="event.preventDefault(); Views.railMetro2Fn(this)">
          <div class="pay-row">
            <label>Your furnisher code<input name="identification_number" minlength="5" placeholder="assigned by the bureaus (5+ characters)" required /></label>
            <label>Report invoices unpaid for at least<select name="threshold_days"><option value="60">60 days</option><option value="90">90 days</option><option value="120">120 days</option><option value="180">180 days</option></select></label>
          </div>
          <div><button>Build credit-bureau file</button></div></form></details>`;
  }

  async function payLoadPickers() {
    const d = payData;
    if (!d.fin) return;
    if (!d.invoices) d.invoices = await Api.program.allInvoices().then((r) => (r.invoices || []).filter((v) => v.status === "OPEN")).catch(() => []);
    if (!d.payables) d.payables = await Api.program.payablesList("OPEN").then((r) => r.payables || []).catch(() => []);
    const rows = document.getElementById("pay-ach-rows");
    const authorized = d.accounts.filter((a) => a.ach_authorization_name);
    if (rows && authorized.length) {
      rows.innerHTML = d.invoices.length ? `<div class="pay-table-wrap"><table class="pay-pick"><thead><tr><th></th><th>Invoice</th><th>Bill to</th><th>Amount</th><th>Due</th><th>Debit from</th></tr></thead><tbody>
        ${d.invoices.map((v) => `<tr><td><input type="checkbox" name="inv" value="${v.id}" aria-label="Include ${esc(v.invoice_no)}" /></td>
          <td class="mono">${esc(v.invoice_no)}</td><td>${esc(v.party === "HEALTH_PLAN" ? "Health plan" : v.party === "PROVIDER" ? "Provider" : v.party)}</td>
          <td>${money(v.amount_cents)}</td><td>${v.due_date ? esc(String(v.due_date).slice(0, 10)) : "—"}</td>
          <td><select name="acct-${v.id}" aria-label="Account for ${esc(v.invoice_no)}">${authorized.map((a) => `<option value="${a.id}">${esc(a.owner_ref)} · •••• ${esc(a.account_last4)}</option>`).join("")}</select></td></tr>`).join("")}
        </tbody></table></div>` : `<p class="muted">No open invoices to collect.</p>`;
    }
    const sel = document.getElementById("pay-wire-payable");
    if (sel) sel.innerHTML = d.payables.length
      ? `<option value="">Choose the payable</option>` + d.payables.map((p) => `<option value="${p.id}">${esc(p.payee)} — ${money(p.amount_cents)}${p.due_date ? " · due " + esc(String(p.due_date).slice(0, 10)) : ""}</option>`).join("")
      : `<option value="">No open payables</option>`;
  }

  function railFail(e, elId = "rail-gen-result") {
    const msg = /bank\.nacha not configured/.test(e.message)
      ? "This program's bank details aren't on file yet, so this file can't be built."
      : /no invoices past the delinquency threshold/.test(e.message) ? "No invoices are unpaid for that long — there is nothing to report."
      : /no cleared checks/.test(e.message) ? "There are no cleared checks waiting to be deposited."
      : e.message;
    const box = document.getElementById(elId);
    if (box) box.innerHTML = `<p class="pay-note pay-bad-note">${esc(msg)}</p>`; else UI.toast(msg, { kind: "warn" });
  }

  function railShowGenerated(r, elId = "rail-gen-result") {
    const box = document.getElementById(elId);
    if (!box) return;
    const items = r.entry_count || r.records || r.check_count || 1;
    box.innerHTML = `<div class="pay-done"><b>File built: ${esc(r.reference)}</b> — ${items} item${items === 1 ? "" : "s"}, ${money(r.total_cents)}.
      Download it and send it to your bank. <button class="mini" onclick="Views.railDownload('${r.file_id}', '${esc(r.filename || "")}')">Download</button></div>`;
    Api.program.railFiles("").then((f) => { payData.files = f.files || []; }).catch(() => {});
  }

  async function railDownload(fileId, filename) {
    try {
      const r = await Api.program.railFileBody(fileId);
      const blob = new Blob([r.file], { type: "text/plain" });
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob); a.download = filename || r.filename || "payment-file.txt"; a.click();
      URL.revokeObjectURL(a.href);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function railAchDebitFn(form) {
    const items = [...form.querySelectorAll('input[name="inv"]:checked')].map((c) => ({
      invoice_id: c.value, payment_account_id: form.querySelector(`select[name="acct-${c.value}"]`)?.value || "" }));
    if (!items.length) { railFail(new Error("Tick at least one invoice to collect.")); return; }
    await UI.run(form.querySelector("button"), async () => {
      try { railShowGenerated(await Api.program.railAchDebits({ items, effective_date: form.effective_date.value || undefined })); }
      catch (e) { railFail(e); }
    });
  }

  async function railWireFn(form) {
    await UI.run(form.querySelector("button"), async () => {
      try { railShowGenerated(await Api.program.railWire({ payable_id: form.payable_id.value, payment_account_id: form.payment_account_id.value })); }
      catch (e) { railFail(e); }
    });
  }

  async function railIclFn(btn) {
    if (!(await UI.confirm("Build a cash letter?", "Every cleared check that hasn't been deposited yet goes into one file for your bank.", "Build cash letter"))) return;
    await UI.run(btn, async () => {
      try { railShowGenerated(await Api.program.railIcl()); } catch (e) { railFail(e); }
    });
  }

  async function railMetro2Fn(form) {
    if (!(await UI.confirm("Report to the credit bureaus?", "Credit-bureau reporting is regulated by the Fair Credit Reporting Act. Build this file only if your compliance team has approved reporting these parties.", "Build file", true))) return;
    await UI.run(form.querySelector("button"), async () => {
      try { railShowGenerated(await Api.program.railMetro2({ identification_number: form.identification_number.value.trim(), threshold_days: Number(form.threshold_days.value) || 60 })); }
      catch (e) { railFail(e); }
    });
  }

  // ---- History ----
  let payKind = "";
  function payHistoryFilter(k) { payKind = k; payRender(); }

  function payHistoryPanel() {
    const d = payData;
    const files = d.files.filter((f) => !payKind || f.kind === payKind);
    const statusBadge = (s) => s === "RETURNED" || s === "FAILED" ? `<span class="badge pay-badge-bad">${esc(s === "RETURNED" ? "Returned" : "Failed")}</span>`
      : `<span class="badge">${esc(s.charAt(0) + s.slice(1).toLowerCase())}</span>`;
    return `${d.fin ? `<div class="card pay-card pay-returns"><h3>Got a return file from your bank?</h3>
        <p class="what">When debits fail — a closed account, not enough funds — the bank sends back a return file. Upload it and each failed payment is marked with the bank's reason.</p>
        <form class="pay-form pay-inline" onsubmit="event.preventDefault(); Views.railReturnsFn(this)">
          <input type="file" name="returns" accept=".ach,.txt" required aria-label="ACH return file" />
          <button>Read return file</button></form><div id="rail-returns-result"></div></div>` : ""}
      <div class="pay-filter"><label>Show <select onchange="Views.payHistoryFilter(this.value)">
        <option value="">All files</option>${Object.entries(PAY_KIND).map(([k, v]) => `<option value="${k}" ${payKind === k ? "selected" : ""}>${v}</option>`).join("")}</select></label></div>
      ${d.filesError ? `<p class="error">${esc(d.filesError)}</p>` : ""}
      ${files.length ? `<div class="pay-table-wrap"><table><thead><tr><th>Built</th><th>File</th><th>Reference</th><th class="num">Items</th><th class="num">Total</th><th>Status</th><th></th></tr></thead><tbody>
        ${files.map((f) => `<tr><td>${fmtDate(f.created_at)}</td><td>${esc(PAY_KIND[f.kind] || f.kind)}</td><td class="mono">${esc(f.reference)}</td>
          <td class="num">${f.item_count}</td><td class="num">${money(f.total_cents)}</td><td>${f.kind === "ACH_RETURN" && f.status === "GENERATED" ? '<span class="badge">Received</span>' : statusBadge(f.status || "")}</td>
          <td class="pay-actions"><button class="mini ghost" onclick="Views.railFileDetailFn('${f.id}', this)">Items</button>
            ${d.fin ? `<button class="mini ghost" onclick="Views.railDownload('${f.id}')">Download</button>` : ""}</td></tr>
          <tr class="pay-items" id="pay-items-${f.id}" hidden><td colspan="7"></td></tr>`).join("")}
        </tbody></table></div>` : `<p class="muted">${payKind ? "No files of this kind yet." : "No bank files built yet."}</p>`}`;
  }

  async function railReturnsFn(form) {
    const f = form.returns.files[0];
    if (!f) return;
    await UI.run(form.querySelector("button"), async () => {
      try {
        const r = await Api.program.railAchReturns(await f.text());
        document.getElementById("rail-returns-result").innerHTML = `<div class="pay-done">Read ${r.returns} returned payment${r.returns === 1 ? "" : "s"}:
          ${r.applied} matched to payments we sent${r.unmatched ? `, <b>${r.unmatched} not recognised</b> (not from a file built here)` : ""}.</div>`;
        const files = await Api.program.railFiles("").catch(() => null);
        if (files) { payData.files = files.files || []; }
      } catch (e) { railFail(e, "rail-returns-result"); }
    });
  }

  async function railFileDetailFn(id, btn) {
    const row = document.getElementById("pay-items-" + id);
    if (!row) return;
    if (!row.hidden) { row.hidden = true; return; }
    row.hidden = false;
    const cell = row.firstElementChild;
    cell.innerHTML = '<span class="muted">Loading…</span>';
    try {
      const items = (await Api.program.railFileItems(id)).items || [];
      cell.innerHTML = items.length ? `<table class="pay-sub"><thead><tr><th class="num">Amount</th><th>Trace number</th><th>Status</th><th>Bank's reason</th></tr></thead><tbody>
        ${items.map((i) => `<tr><td class="num">${money(i.amount_cents)}</td><td class="mono">${esc(i.trace_number || "—")}</td>
          <td>${i.status === "RETURNED" ? '<span class="badge pay-badge-bad">Returned</span>' : esc(i.status.charAt(0) + i.status.slice(1).toLowerCase())}</td>
          <td>${i.return_code ? esc(i.return_code + " — " + (i.return_reason || "")) : "—"}</td></tr>`).join("")}</tbody></table>`
        : '<span class="muted">This file has no individual items recorded.</span>';
    } catch (e) { cell.innerHTML = `<span class="error">${esc(e.message)}</span>`; }
  }

  // ---- Sanctions screening ----
  function payScreeningPanel() {
    const st = payData.status || {};
    return `<p class="muted pay-lead">Checks a name against the U.S. Treasury's sanctions list (OFAC SDN) before you work with or pay someone. A match is a lead to review, not a finding — every check is kept for the compliance record.</p>
      ${st.enabled === false ? `<div class="pay-note">Screening is switched off for this deployment.</div>` : ""}
      <form class="pay-form pay-inline" onsubmit="event.preventDefault(); Views.railScreenFn(this)">
        <label>Name<input name="name" placeholder="person or organization" required /></label>
        <label>Who is this<select name="subject_type"><option value="MANUAL">Other</option><option value="PARTY">Provider or health plan</option><option value="TPA">Third-party administrator</option><option value="VENDOR">Vendor</option><option value="ARBITRATOR">Independent reviewer</option></select></label>
        <button${st.enabled === false ? " disabled" : ""}>Check name</button></form>
      <div id="screen-result"></div>
      <div class="pay-history-head"><h3>Past checks</h3><span class="muted" id="scr-count"></span></div>
      <form class="pay-filter-bar" onsubmit="event.preventDefault(); Views.screenListFn(this)" onchange="Views.screenListFn(this)">
        <input name="q" type="search" placeholder="Search names" aria-label="Search names" value="${esc(scrFilter.q)}" />
        <select name="result" aria-label="Result">
          <option value="" ${scrFilter.result === "" ? "selected" : ""}>All results</option>
          <option value="match" ${scrFilter.result === "match" ? "selected" : ""}>Possible matches</option>
          <option value="clear" ${scrFilter.result === "clear" ? "selected" : ""}>No match</option></select>
        <select name="type" aria-label="Who">
          <option value="">Anyone</option>${Object.entries(SCR_TYPE).map(([k, v]) => `<option value="${k}" ${scrFilter.type === k ? "selected" : ""}>${v}</option>`).join("")}</select>
      </form>
      <div id="scr-list" data-nosearch><p class="muted">Loading…</p></div>`;
  }

  const SCR_TYPE = { MANUAL: "Other", PARTY: "Provider or health plan", TPA: "Third-party administrator", VENDOR: "Vendor", ARBITRATOR: "Independent reviewer",
    PROVIDER: "Provider applicant", PAYER: "Health plan applicant", IDRE: "Review organization applicant", AUDITOR: "Auditor applicant", OTHER: "Other applicant" };
  let scrFilter = { q: "", result: "", type: "" }, scrRows = [], scrTotal = 0;

  function screenListFn(form) {
    scrFilter = { q: form.q.value.trim(), result: form.result.value, type: form.type.value };
    clearTimeout(screenListFn.t);
    screenListFn.t = setTimeout(() => screenLoad(false), 250);
  }

  async function screenLoad(more) {
    const box = document.getElementById("scr-list");
    if (!box) return;
    try {
      const r = await Api.program.screeningChecks({ ...scrFilter, limit: 50, offset: more ? scrRows.length : 0 });
      scrRows = more ? scrRows.concat(r.checks || []) : (r.checks || []);
      scrTotal = r.total || 0;
      const cnt = document.getElementById("scr-count");
      if (cnt) cnt.textContent = `${scrTotal.toLocaleString()} check${scrTotal === 1 ? "" : "s"}${r.matches ? ` · ${r.matches} possible match${r.matches === 1 ? "" : "es"}` : ""}`;
      const who = (b) => !b ? "—" : b === "system:onboarding" ? "Automatic, on onboarding approval" : /^[0-9a-f]{8}-[0-9a-f-]{27}$/.test(b) ? "Staff member" : esc(b);
      box.innerHTML = scrRows.length ? `<div class="pay-table-wrap"><table><thead><tr><th>When</th><th>Name checked</th><th>Who</th><th>Result</th><th>Closest name on the list</th><th>Checked by</th></tr></thead><tbody>
        ${scrRows.map((c) => `<tr class="${c.matched ? "pay-row-hit" : ""}"><td>${fmtDate(c.created_at)}</td><td><b>${esc(c.query_name)}</b>${c.subject_ref ? `<div class="muted">${esc(c.subject_ref)}</div>` : ""}</td>
          <td>${esc(SCR_TYPE[c.subject_type] || c.subject_type)}</td>
          <td>${c.matched ? `<span class="badge pay-badge-bad">Possible match · ${Number(c.top_score).toFixed(2)}</span>` : '<span class="badge s-paid">No match</span>'}</td>
          <td>${c.matched && c.top_match ? esc(c.top_match) + (c.match_count > 1 ? ` <span class="muted">+${c.match_count - 1} more</span>` : "") : "—"}</td>
          <td>${who(c.screened_by)}</td></tr>`).join("")}
        </tbody></table></div>${scrRows.length < scrTotal ? `<button class="mini ghost" onclick="Views.screenMore()">Show more (${(scrTotal - scrRows.length).toLocaleString()} older)</button>` : ""}`
        : `<p class="muted">${scrFilter.q || scrFilter.result || scrFilter.type ? "No checks match these filters." : "No names checked yet."}</p>`;
    } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
  }
  function screenMore() { screenLoad(true); }

  async function railScreenFn(form) {
    const box = document.getElementById("screen-result");
    await UI.run(form.querySelector("button"), async () => {
      try {
        const name = form.name.value.trim();
        const r = await Api.program.screeningCheck({ name, subject_type: form.subject_type.value });
        box.innerHTML = r.matched
          ? `<div class="pay-note pay-bad-note"><b>Possible match for “${esc(name)}”</b> (closest score ${Number(r.top_score).toFixed(2)} of 1.00). Don't proceed until compliance has reviewed it; this check is recorded.</div>
             <div class="pay-table-wrap"><table><thead><tr><th>Name on the list</th><th class="num">Score</th><th>List entry</th></tr></thead><tbody>
             ${(r.matches || []).map((m) => `<tr><td>${esc(m.name)}</td><td class="num">${Number(m.score).toFixed(2)}</td><td class="mono">${esc(m.source_id || "")}</td></tr>`).join("")}</tbody></table></div>`
          : `<div class="pay-done">No match for “${esc(name)}” on the sanctions list. Check recorded.</div>`;
        screenLoad(false);
      } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
    });
  }


  return { savePacketForm, npiReviewDecideFn, claim, assignToMe, exclusionDecideFn, dashboard, cases, caseDetail, newDispute, sortCases, onboarding, onboardingFilter, onboardingNew, decide, voice, reports, showAnalysis, retryAnalysis, check, assign, unassign, unassignHelper, letter, saveCurrentView, escalate, relate, feeTransfer, peek, askGraph, rptSlaFilter, voiceIntakeFilter, voiceLogFilter, settleInvoice, qaQueue, qaReview, qaDecide, intake, intakeFilter, newIntake, advanceIntake, deliverables, submitDeliverable, requestDeliverable, finance, payInvoice, moveDoc, downloadDoc, downloadZip, rulesAdmin, rulesAuditFilter, ruleEdit, ruleDelete, rulesSave, bindRulesAdmin, uploadCheck, clearCheck, requestRescan, copilotBrief, copilotDraftQA, copilotPropose, copilotDecideBatch, assistant, assistantChip, mountAssistantFab, handleAssistantMessage, asstQaDecide, asstShareAdd, asstShareRemove, assistantTool, asstRequestUpload, asstCheck, asstSendMail, asstClearCheck, corrTemplateBody, timeAdd, myTimesheet, myTimeRange, myTimeListBox, myTimeAdd, myTimeEditStart, myTimeSave, myTimeDel, timeReport, templatesPage, tplShowTab, tplEdit, tplSlug, tplInsert, tplSave, tplDelete, tplPreview, tplHistory, tplUsageDefault, tplUsageAdd, tplUsageRemove, corrTemplatesFor, timeReportRun, timeReportPreset, timeReportDates, timeReportReset, timeReportPick, timeReportTab, timeReportSend, timeReportCsv, timeRateSet, asstTimeAdd, bulkIntakeFile, bulkIntakeSubmit, createTenantFlow, renameTenantFlow, createFederalAdminFlow, addTenantStaffFlow, teamAdmin, autoAssignToggle, autoAssignRotation, autoAssignRole, autoAssignBacklog, staffFilter, staffOpen, setStaffEnabled, deleteStaffMember, auditLog, opsDashboard, billingInvoices, billingGen, billingActFn, billingDetail, billingExportFn, billingPayForm, billingPayRun, billingFilter: (f) => billingList({ status: f.status.value, start: f.start.value, end: f.end.value }), arapView, arapRecord, arapSettle, arapVoid, arapApFilter, arapArFilter, arapNacha, arapNachaFile, reconView, reconImportRun, reconFetchRun, reconMatchRun, reconOpen, reconResolveFn, reconBatchFilter, flReportFn, checksView, checksFilter: (st) => checksBox(st), checkUploadFn, checkOpen, checkClearFn, checkMatchFn, tpaView, tpaClaimFn, tpaUnlinkFn, tpaInviteCodeFn, tpaAddClientFn, tpaTeamAddFn, tpaTeamRoleFn, tpaTeamRemoveFn, tpaGroupAddFn, tpaGroupRenameFn, tpaGroupStatusFn, tpaClientGroupFn, tpaClientStatusFn, tpaFileFn, tpaDashBox, tpaCasesFilter, tpaEditDisputeFn, tpaAdminStatusFn, tpaBulkFile, tpaBulkSubmit, tpaBatchesBox, tpaBatchDetail, mailLog, mailLogFilter, mailLogBody, saveReviewNote, saveDisputeSummary, confirmAor, eligChecklist, wfAct, wfAnytime, fwaAct, saveContractDates, myTimePromptFill, saveCaseInfo, saveMailRecipients, qaSendFn, flAdhocFn, era835Fn, correctField, correctCheck, paymentsView, payTabFn, payHistoryFilter, railRoutingCheck, railAccountAddFn, railAccountDelFn, railDownload, railAchDebitFn, railWireFn, railIclFn, railMetro2Fn, railReturnsFn, railFileDetailFn, railScreenFn, screenListFn, screenMore, railAttachAuthFn, offerForm, determinationForm, caseOffers, queueView, pmQueueView, partyCases, partyCaseDetail, partyInviteForm, batchableModal, insightsView, featuresAdmin, billingFilter: (f) => billingList(f.status.value), tpaOnboardingBox, moreDocsAdd, docReqPermissionFn, claimsCsvUpload, claimsCsvTemplate, tpaClientAddOpen, tpaGroupAddOpen, tpaClientFilter, tpaFileFor, tpaFileDocsStart, tpaFileDocsUse, tpaFileDocsReset, tpaImportFn, tpaProfileForm, tpaPaymentForm, tpaPackageStart, tpaPackageBox, tpaSlotUpload, tpaSlotRemove, tpaMergedFn, tpaZipFn, hpView, hpBoot, hpClaimFn, hpAddPartyFn, hpOnboardingBox, hpImportFn, hpProfileForm, hpPaymentForm, hpPackageStart, hpPackageBox, hpSlotUpload, hpMergedFn, hpZipFn, hpFileFn, hpAdminBox, hpAdminStatusFn, railShowGenerated, hpUnlinkFn, onbReviewOpen, onbReviewDecide, filingPackagesFilter, tpaBulkDisputesFn, tpaBdBatchesBox, tpaBdReport, tpaTab, tpaPkgClose, tpaBdPay, tpaFeesPay, tpaBdTemplate, tpaBdSample, respondOpen, respondDocOpen, respondUploadFn, respondSubmitFn, respondOptOutFn, hpTab, tpaBdToggle, tpaBdItem, orgAuditBox, payDisputeFn, payClientFn, hpFeesPay, tpaMethodAdd, tpaMethodRemove, tpaProvToggle, orgAuditPage, docReqNew, docReqClose, docReqPage, orgDocReqBox, docReqUploadFn, docReqSubmitFn, rebrandTenantFlow, finChecksFilter, resolveUnappliedFn, tpaPayFilter, respondFilterFor, myActivityPage, auditMineToggle, orgGo, orgDocsDownload, sentRecordOpen, sentDocGetFn, docReqExtensionFn, previewDoc, previewCaseDoc, previewSentDoc, corrPreviewFn, mailTrackerFilter, qaDraftQualityBox, filePrefillFn, corrFillRecipientsFn: corrFillRecipients };
})();
