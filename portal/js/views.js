// views.js — one render function per screen; role-aware actions.
const Views = (() => {
  const $ = (sel) => document.querySelector(sel);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmtDate = (d) => (d ? new Date(d).toLocaleString("en-US", { dateStyle: "medium", timeStyle: "short" }) : "—");
  const badge = (s) => `<span class="badge s-${esc(s).toLowerCase().replace(/_/g, "-")}">${esc(s)}</span>`;
  const err = (e) => `<p class="error">${esc(e.message)}</p>`;
  const role = (r) => (Auth.claims()?.roles || []).includes(r);
  const can = (...rs) => rs.some(role);

  // ---- Dashboard -----------------------------------------------------------
  async function dashboard() {
    const me = Auth.claims();
    let html = `<h1>Dashboard</h1><p class="muted">${esc(me.name)} · tenant <b>${esc(Api.getTenant())}</b> · ${me.roles.map(esc).join(", ")}</p>`;
    try {
      const [cases, summary] = await Promise.all([Api.cases.list(), Api.reports.summary()]);
      html += `<div class="cards">` + summary.map((s) =>
        `<div class="card"><div class="num">${s.count}</div><div class="lbl">${badge(s.status)}</div>
         <div class="muted">avg QPA $${s.avg_qpa_usd.toFixed(0)}</div></div>`).join("") + `</div>`;
      const open = cases.filter((c) => !String(c.status).startsWith("CLOSED"));
      html += `<h2>Open disputes (${open.length})</h2>` + caseTable(open);
    } catch (e) { html += err(e); }
    return html;
  }

  const caseTable = (rows) => rows.length ? `<table><thead><tr>
      <th>Case #</th><th>Status</th><th>Service</th><th>QPA</th><th>Opened</th></tr></thead><tbody>` +
      rows.map((c) => `<tr onclick="location.hash='#/cases/${c.id}'" class="click">
        <td>${esc(c.case_number)}</td><td>${badge(c.status)}</td><td>${esc(c.service_line)}</td>
        <td>$${(c.qpa_cents / 100).toLocaleString()}</td><td>${fmtDate(c.opened_at)}</td></tr>`).join("") +
      `</tbody></table>` : `<p class="muted">No disputes.</p>`;

  // ---- Case list / detail ----------------------------------------------------
  async function cases() {
    try { return `<h1>Disputes</h1>` + caseTable(await Api.cases.list()); }
    catch (e) { return `<h1>Disputes</h1>` + err(e); }
  }

  async function caseDetail(id) {
    try {
      const [c, docs, activities] = await Promise.all([
        Api.cases.get(id), Api.cases.documents(id), Api.cases.activities(id),
      ]);
      let html = `<h1>${esc(c.case_number)}</h1><p>${badge(c.status)} · ${esc(c.service_line)} · QPA $${(c.qpa_cents / 100).toLocaleString()} · opened ${fmtDate(c.opened_at)}</p>`;

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
      if (acts.length)
        html += `<div class="actions">` + acts.map((a, i) =>
          `<button data-act="${i}">${a[0]}</button>`).join("") + `</div>`;

      // Documents + analysis
      html += `<h2>Documents</h2>
        <form id="up" class="upload"><input type="file" name="file" required />
        <label><input type="checkbox" name="sealed" /> sealed (offer justification)</label>
        <button>Upload</button></form>`;
      html += docs.length ? `<table><thead><tr><th>Type</th><th>Size</th><th>Sealed</th><th>Analysis</th><th></th></tr></thead><tbody>` +
        docs.map((d) => `<tr><td>${esc(d.content_type)}</td><td>${(d.size_bytes / 1024).toFixed(0)} KB</td>
          <td>${d.sealed ? "🔒" : "—"}</td>
          <td>${badge(d.analysis_status)}${d.doc_type ? " · " + esc(d.doc_type) : ""}</td>
          <td><a href="${Api.cases.downloadUrl(id, d.doc_id)}" target="_blank">download</a>
          ${!d.sealed ? ` · <a href="javascript:void 0)" onclick="Views.showAnalysis('${id}','${d.doc_id}')">analysis</a>` : ""}</td></tr>`).join("") +
        `</tbody></table>` : `<p class="muted">No documents yet.</p>`;
      html += `<div id="analysis"></div>`;

      // CRM activity timeline (voice calls auto-attached, milestones, notes)
      html += `<h2>Activity timeline</h2>` + (activities.length ? `<table><tbody>` +
        activities.map((a) => `<tr><td>${badge(a.type)}</td><td>${esc(a.body)}</td>
          <td class="muted">${fmtDate(a.at)}</td></tr>`).join("") +
        `</tbody></table>` : `<p class="muted">No activity yet — voice calls and milestones attach automatically.</p>`);

      queueMicrotask(() => {
        acts.forEach((a, i) =>
          document.querySelector(`[data-act="${i}"]`)?.addEventListener("click", async () => {
            try { await a[1](); location.reload(); } catch (e) { alert(e.message); }
          }));
        $("#up").addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = ev.target.file.files[0];
          try { await Api.cases.upload(id, f, ev.target.sealed.checked); location.reload(); }
          catch (e) { alert(e.message); }
        });
      });
      return html;
    } catch (e) { return err(e); }
  }

  async function showAnalysis(caseId, docId) {
    const box = $("#analysis");
    box.innerHTML = `<p class="muted">Loading analysis…</p>`;
    try {
      const a = await Api.cases.analysis(caseId, docId);
      const r = a.result || {};
      box.innerHTML = `<h3>Document analysis ${badge(a.status)}</h3>
        <p class="muted">type: ${esc(a.doc_type)} · seal detected: ${r.seal_detected ? "yes" : "no"} · tables: ${r.table_count ?? 0}</p>
        <pre>${esc(JSON.stringify(r.extracted || {}, null, 2))}</pre>
        ${(r.findings || []).length ? `<p class="error">Findings: ${esc(JSON.stringify(r.findings))}</p>` : ""}`;
    } catch (e) { box.innerHTML = err(e); }
  }

  function offerForm(caseId) {
    const amount = prompt("Sealed offer amount (USD):");
    if (!amount) return Promise.resolve();
    const cents = Math.round(parseFloat(amount) * 100);
    return Api.cases.signal(caseId, "OFFER_SUBMITTED", { party_id: Auth.claims().sub, amount_cents: cents });
  }

  function determinationForm(caseId) {
    const winning = prompt("Winning offer (party id of prevailing offer):");
    if (!winning) return Promise.resolve();
    const rationale = prompt("Determination rationale (required):") || "";
    if (!rationale.trim()) { alert("Rationale is required."); return Promise.resolve(); }
    return Api.cases.signal(caseId, "DETERMINATION_ISSUED", { winning_offer_party: winning, rationale });
  }

  // ---- New dispute -------------------------------------------------------------
  function newDispute() {
    queueMicrotask(() => $("#nd").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      try {
        const r = await Api.cases.initiate({
          case_number: f.case_number, service_line: f.service_line, plan_type: f.plan_type,
          qpa_cents: Math.round(parseFloat(f.qpa) * 100), provider_id: f.provider_id,
          payer_id: f.payer_id, open_negotiation_end: f.one_end,
        });
        location.hash = `#/cases/${r.case_id}`;
      } catch (e) { alert(e.message); }
    }));
    return `<h1>New dispute</h1><form id="nd" class="form">
      <label>CMS case number <input name="case_number" required placeholder="CMS-TX-2026-00002" /></label>
      <label>Service line <select name="service_line"><option>ER</option><option>AIR_AMBULANCE</option><option>ANESTHESIA</option><option>RADIOLOGY</option><option>LAB</option><option>OTHER</option></select></label>
      <label>Plan type <select name="plan_type"><option>SELF_FUNDED</option><option>FULLY_INSURED</option></select></label>
      <label>QPA (USD) <input name="qpa" type="number" step="0.01" required /></label>
      <label>Provider ID <input name="provider_id" required /></label>
      <label>Payer ID <input name="payer_id" required /></label>
      <label>Open negotiation end <input name="one_end" type="date" required /></label>
      <button>Initiate (starts statutory clocks)</button></form>`;
  }

  // ---- Onboarding --------------------------------------------------------------
  async function onboarding() {
    let html = `<h1>Onboarding</h1>`;
    if (can("CASE_MANAGER", "FEDERAL_ADMIN")) {
      try {
        const apps = await Api.onboarding.list();
        html += `<h2>Review queue</h2>` + (apps.length ? `<table><thead><tr>
          <th>Legal name</th><th>Type</th><th>Status</th><th>Submitted</th><th></th></tr></thead><tbody>` +
          apps.map((a) => `<tr><td>${esc(a.legal_name)}</td><td>${esc(a.type)}</td>
            <td>${badge(a.status)}</td><td>${fmtDate(a.submitted_at)}</td>
            <td>${["PENDING_APPROVAL"].includes(a.status) ? `
              <button onclick="Views.decide('${a.id}','APPROVE')">Approve</button>
              <button class="danger" onclick="Views.decide('${a.id}','REJECT')">Reject</button>` : ""}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">Queue empty.</p>`);
      } catch (e) { html += err(e); }
    }
    html += `<h2>New application</h2><p class="muted">Self-service registration for provider, payer, IDRE and auditor organizations.</p>
      <p><a class="button" href="#/onboarding/new">Start application</a></p>`;
    return html;
  }

  function onboardingNew() {
    queueMicrotask(() => $("#ob").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      const payload = {};
      f.requirements.split(",").map((s) => s.trim()).filter(Boolean).forEach((k) => (payload[k] = "provided"));
      try {
        await Api.onboarding.submit({
          type: f.type, legal_name: f.legal_name, ein: f.ein, npi: f.npi || undefined,
          payload: { ...payload, contact_email: f.contact_email },
        });
        location.hash = "#/onboarding";
      } catch (e) { alert(e.message); }
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
    const reason = decision === "REJECT" ? prompt("Rejection reason:") || "" : "";
    try { await Api.onboarding.decide(id, decision, reason); location.reload(); }
    catch (e) { alert(e.message); }
  }

  // ---- Voice console -------------------------------------------------------------
  async function voice() {
    try {
      const [intake, logs] = await Promise.all([Api.voice.intake(), Api.voice.logs()]);
      queueMicrotask(() => $("#ob-call")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = Object.fromEntries(new FormData(ev.target));
        try {
          const r = await Api.voice.outbound(f.to, f.case_number, f.script);
          alert(`Outbound call ${r.status}`);
          location.reload();
        } catch (e) { alert(e.message); }
      }));
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
        <h2>Intake requests (${intake.length})</h2>` +
        (intake.length ? `<table><thead><tr><th>Caller</th><th>Organization</th><th>Summary</th><th>Status</th><th>At</th></tr></thead><tbody>` +
          intake.map((v) => `<tr><td>${esc(v.caller_name)}<br/><span class="muted">${esc(v.caller_phone)}</span></td>
            <td>${esc(v.organization)}</td><td>${esc(v.summary)}</td><td>${badge(v.status)}</td><td>${fmtDate(v.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No intake requests.</p>`) +
        `<h2>Call log</h2>` +
        (logs.length ? `<table><thead><tr><th>Direction</th><th>Tool/event</th><th>Case</th><th>Status</th><th>At</th></tr></thead><tbody>` +
          logs.map((l) => `<tr><td>${esc(l.direction)}</td><td>${esc(l.tool)}</td><td>${esc(l.case_number)}</td>
            <td>${badge(l.status)}</td><td>${fmtDate(l.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No calls yet.</p>`);
    } catch (e) { return `<h1>Voice console</h1>` + err(e); }
  }

  // ---- Reports ---------------------------------------------------------------------
  async function reports() {
    try {
      const [sla, summary] = await Promise.all([Api.reports.sla(), Api.reports.summary()]);
      return `<h1>Compliance reports</h1><h2>Case status rollup</h2>` +
        `<table><thead><tr><th>Status</th><th>Count</th><th>Avg QPA</th></tr></thead><tbody>` +
        summary.map((s) => `<tr><td>${badge(s.status)}</td><td>${s.count}</td><td>$${s.avg_qpa_usd.toFixed(0)}</td></tr>`).join("") +
        `</tbody></table><h2>Statutory SLA breaches (${sla.length})</h2>` +
        (sla.length ? `<table><thead><tr><th>Case</th><th>Clock</th><th>Detail</th><th>At</th></tr></thead><tbody>` +
          sla.map((b) => `<tr><td>${esc(b.case_id)}</td><td>${badge(b.clock)}</td><td>${esc(b.detail)}</td><td>${fmtDate(b.at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No breaches recorded.</p>`);
    } catch (e) { return `<h1>Compliance reports</h1>` + err(e); }
  }

  return { dashboard, cases, caseDetail, newDispute, onboarding, onboardingNew, decide, voice, reports, showAnalysis };
})();
