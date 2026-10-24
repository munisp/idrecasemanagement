// config-views.js — the Configuration page: the program's settings in one place (configuration.go). Fees and pay
// rates are dated: a new amount starts on the day it is given and never changes what was already charged or worked.
const ConfigViews = (() => {
  const afterRender = (fn) => setTimeout(fn, 0);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmtDate = (d) => (d ? new Date(/^\d{4}-\d{2}-\d{2}$/.test(d) ? d + "T12:00:00Z" : d).toLocaleDateString("en-US", { dateStyle: "medium", ...(/^\d{4}-\d{2}-\d{2}$/.test(d) ? { timeZone: "UTC" } : {}) }) : "—");
  const can = (...rs) => Auth.can(...rs);
  const money = (c) => c == null ? "—" : "$" + (Number(c) / 100).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  const cents = (v) => Math.round(Number(String(v).replace(/[$,\s]/g, "")) * 100);
  const ROLE = { CASE_MANAGER: "Reviewer", PM: "Project manager", CODER: "Coder", NURSE_PHYSICIAN: "Nurse / physician", DOCTOR: "Physician",
    NURSE: "Nurse", ATTORNEY: "Attorney", ARBITRATOR: "Arbitrator", FINANCE: "Finance" };
  const PTYPE = { hospital_inpatient: "Hospital — inpatient", hospital_outpatient: "Hospital — outpatient", physician_dentist: "Physician / dentist",
    rural_hospital: "Rural hospital", other: "Other" };
  let C = null; // the loaded configuration

  // A step's start as "YYYY-MM-DDTHH:MM" (a bare day starts at midnight), so starts compare as text.
  const startKey = (v) => (/^\d{4}-\d{2}-\d{2}$/.test(v) ? v + "T00:00" : String(v).replace(" ", "T").slice(0, 16));
  // The schedule's status for each entry, against the program's clock now: in force, upcoming, or past.
  const stepState = (sched, i, now) => {
    const from = startKey(sched[i].effective_from), next = sched[i + 1] && startKey(sched[i + 1].effective_from);
    if (from > now) return "upcoming";
    return !next || next > now ? "in force" : "past";
  };
  // "Jan 1, 2027, 9:00 AM ET" -- the program's clock, never the viewer's.
  const fmtStart = (v) => {
    if (v === "2000-01-01") return "From the start";
    const [d, t] = startKey(v).split("T");
    const [h, m] = t.split(":").map(Number);
    const time = h === 0 && m === 0 ? "midnight" : `${(h % 12) || 12}:${String(m).padStart(2, "0")} ${h < 12 ? "AM" : "PM"}`;
    return `${fmtDate(d)}, ${time} ${esc(C.zone_abbrev || "")}`;
  };
  const ZONES = ["America/New_York", "America/Chicago", "America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Anchorage", "Pacific/Honolulu", "America/Puerto_Rico"];

  async function page() {
    if (!can("TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "PM", "FINANCE"))
      return `<div class="view-head"><h1>Configuration</h1></div><p class="muted">Only a tenant admin, the PM or Finance can open the configuration.</p>`;
    try { C = await Api.program.configuration(); }
    catch (e) { return `<div class="view-head"><h1>Configuration</h1></div><p class="error">${esc(e.message)}</p>`; }
    afterRender(() => document.querySelectorAll("[data-cfg-section]").forEach((el) => render(el.dataset.cfgSection)));
    const ro = C.can_edit ? "" : `<p class="muted">You can see the configuration; a tenant admin changes it.</p>`;
    return `<div class="view-head"><h1>Configuration</h1>
        <span class="muted">The program's fees, pay rates, eligibility rules, deadlines and billing — changed here, never in code. Every change is recorded with who made it.</span></div>
      ${ro}
      <nav class="cfg-jump" aria-label="Sections">${[["general", "Date and time"], ["fees", "Dispute fee"], ["rates", "Pay rates"], ["eligibility", "Eligibility"], ["outcomes", "Case outcomes"], ["clocks", "Deadlines"], ["billing", "Billing"], ["more", "Other settings"]]
        .map(([k, l]) => `<a href="#/configuration" onclick="event.preventDefault(); document.getElementById('cfg-${k}').scrollIntoView({behavior:'smooth'})">${l}</a>`).join("")}</nav>
      ${["general", "fees", "rates", "eligibility", "outcomes", "clocks", "billing"].map((k) => `<div class="card" id="cfg-${k}" data-cfg-section="${k}"><p class="muted">Loading…</p></div>`).join("")}
      <div class="card" id="cfg-more"><h2>Other settings</h2>
        <ul class="cfg-links">
          <li><a href="#/features">Feature toggles</a> — switch parts of the platform on or off (step order, payment lock, fraud review…)</li>
          <li><a href="#/templates">Templates and letter settings</a> — letter wording, letterhead, the Agency addressee, remit-to and invoice terms</li>
          <li><a href="#/rules">Program rules</a> — automatic actions, the program's display name and brand</li>
          <li><a href="#/team">Team</a> — staff accounts, roles and the assignment rotation</li></ul></div>`;
  }

  function render(section) {
    const el = document.getElementById("cfg-" + section);
    if (!el || !C) return;
    el.innerHTML = ({ outcomes: outcomesHtml, general: generalHtml, fees: feesHtml, rates: ratesHtml, eligibility: eligHtml, clocks: clocksHtml, billing: billingHtml })[section]();
  }

  // ── Date and time ──
  function generalHtml() {
    const edit = C.can_edit, zones = ZONES.includes(C.time_zone) ? ZONES : [C.time_zone, ...ZONES];
    const [d, t] = C.now.split("T");
    return `<h2>Date and time</h2>
      <p class="muted">The program's clock. Fees and pay rates take effect, and each day starts, on this clock — whatever time zone someone is signed in from.</p>
      <p class="cfg-now">Now on the program's clock: <b>${fmtDate(d)}, ${fmtStart(C.now).split(", ").pop()}</b></p>
      <form class="inline-form" onsubmit="event.preventDefault(); ConfigViews.zoneSave(this)">
        <label>Time zone <select name="time_zone" ${edit ? "" : "disabled"}>${zones.map((z) => `<option value="${esc(z)}"${z === C.time_zone ? " selected" : ""}>${esc(z.replace(/_/g, " "))}</option>`).join("")}</select></label>
        ${edit ? `<button class="mini">Save</button>` : ""}</form>`;
  }
  function zoneSave(form) {
    if (form.time_zone.value === C.time_zone) return;
    saveSection("general", { time_zone: form.time_zone.value }, "Time zone saved").then(() => document.querySelectorAll("[data-cfg-section]").forEach((el) => render(el.dataset.cfgSection)));
  }

  // ── Dispute fee ──
  function feesHtml() {
    const f = C.fees, sched = [...(f.initial_fee_schedule || [])].sort((a, b) => startKey(a.effective_from).localeCompare(startKey(b.effective_from)));
    const edit = C.can_edit;
    return `<h2>Initial dispute fee</h2>
      <p class="muted">Charged when a dispute is filed. Each dispute pays the fee in force at the moment it's filed, on the program's clock — add next year's fee ahead of time with the date and time it starts; disputes already filed keep the fee they were charged.</p>
      <p class="cfg-now">Fee now: <b>${money(f.initial_fee_today_cents)}</b>${f.card_fee_today_cents ? ` — by card <b>${money(f.initial_fee_today_cents + f.card_fee_today_cents)}</b>, which adds the ${money(f.card_fee_today_cents)} credit card transaction fee; a bank account or check pays the fee alone` : ""}</p>
      <table class="tbl"><thead><tr><th>Takes effect</th><th>Fee</th><th>Card transaction fee</th><th>Status</th>${edit ? "<th></th>" : ""}</tr></thead><tbody>
        ${sched.map((st, i) => `<tr><td>${fmtStart(st.effective_from)}</td><td>${money(st.cents)}</td>
          <td>${st.card_fee_cents ? `${money(st.card_fee_cents)} <span class="muted">(${money(st.cents + st.card_fee_cents)} by card)</span>` : '<span class="muted">none</span>'}</td>
          <td>${esc(stepState(sched, i, C.now))}</td>
          ${edit ? `<td>${sched.length > 1 ? `<button class="mini ghost" onclick="ConfigViews.feeRemove('${st.effective_from}')">Remove</button>` : ""}</td>` : ""}</tr>`).join("")}</tbody></table>
      ${edit ? `<form class="inline-form" onsubmit="event.preventDefault(); ConfigViews.feeAdd(this)">
        <label>New fee <input name="amount" inputmode="decimal" placeholder="e.g. 129.00" required /></label>
        <label>Card transaction fee <input name="card" inputmode="decimal" placeholder="e.g. 3.99" /></label>
        <label>Takes effect <input name="from" type="date" required value="${nextJan1()}" /></label>
        <label>at <input name="at" type="time" value="00:00" /> <span class="muted">${esc(C.zone_abbrev || "")}</span></label>
        <button class="mini">Add fee</button></form>` : ""}
      <h3>Refunds and invoices</h3>
      <form class="inline-form" onsubmit="event.preventDefault(); ConfigViews.feeTerms(this)">
        <label>Refund window (days) <input name="refund_window_days" type="number" min="0" max="365" value="${f.refund_window_days ?? ""}" ${edit ? "" : "disabled"} /></label>
        <label>Invoice due (days) <input name="invoice_due_days" type="number" min="0" max="365" value="${f.invoice_due_days ?? ""}" ${edit ? "" : "disabled"} /></label>
        <label>Payment reminder (days before due) <input name="reminder_lead_days" type="number" min="0" max="365" value="${f.reminder_lead_days ?? ""}" ${edit ? "" : "disabled"} /></label>
        ${edit ? `<button class="mini">Save</button>` : ""}</form>
      <h3>Late fee</h3>
      <p class="muted">Charged on a review invoice that isn't paid within the set days of the case's Final Order, and again every so many days until it's paid. Each charge adds to the case's late fee invoice, and Finance and the PM are told.</p>
      <form class="inline-form" onsubmit="event.preventDefault(); ConfigViews.lateFeeSave(this)">
        <label>Late fee <input name="amount" inputmode="decimal" placeholder="e.g. 50.00" value="${f.late_fee && f.late_fee.cents ? (f.late_fee.cents / 100).toFixed(2) : ""}" ${edit ? "" : "disabled"} /></label>
        <label>Not paid within (days of the Final Order) <input name="after" type="number" min="1" max="365" value="${f.late_fee ? f.late_fee.after_days : 35}" ${edit ? "" : "disabled"} /></label>
        <label>Then again every (days) <input name="every" type="number" min="0" max="365" value="${f.late_fee ? f.late_fee.every_days : 30}" ${edit ? "" : "disabled"} /></label>
        ${edit ? `<button class="mini">Save</button>` : ""}</form>
      ${f.late_fee && f.late_fee.cents ? `<p class="muted">Now: ${money(f.late_fee.cents)} on day ${f.late_fee.after_days + 1} after the Final Order${f.late_fee.every_days ? `, then ${money(f.late_fee.cents)} every ${f.late_fee.every_days} days until paid` : ""}.</p>` : '<p class="muted">No late fee is charged.</p>'}`;
  }
  function lateFeeSave(form) {
    const c = form.amount.value.trim() === "" ? 0 : cents(form.amount.value);
    if (!(c >= 0)) return UI.toast("Enter the late fee in dollars, e.g. 50.00 (empty for none)", { kind: "warn" });
    saveSection("fees", { late_fee: { cents: c, after_days: Number(form.after.value), every_days: Number(form.every.value) } },
      c ? `Late fee ${money(c)} saved` : "Late fee turned off");
  }
  const nextJan1 = () => `${Number(C.today.slice(0, 4)) + 1}-01-01`;

  async function saveSection(section, body, done) {
    try {
      await Api.program.configurationSave(section, body);
      C = await Api.program.configuration();
      render(section); UI.toast(done || "Saved");
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  function feeAdd(form) {
    const c = cents(form.amount.value);
    if (!(c > 0)) return UI.toast("Enter the fee in dollars, e.g. 129.00", { kind: "warn" });
    const card = form.card.value.trim() === "" ? 0 : cents(form.card.value);
    if (!(card >= 0)) return UI.toast("Enter the card transaction fee in dollars, e.g. 3.99 (empty for none)", { kind: "warn" });
    const at = form.at.value || "00:00", start = at === "00:00" ? form.from.value : `${form.from.value}T${at}`;
    const sched = (C.fees.initial_fee_schedule || []).filter((s) => startKey(s.effective_from) !== startKey(start));
    sched.push(card ? { effective_from: start, cents: c, card_fee_cents: card } : { effective_from: start, cents: c });
    saveSection("fees", { initial_fee_schedule: sched }, `${money(c)}${card ? ` (+${money(card)} by card)` : ""} from ${fmtStart(start)} added`);
  }
  function feeRemove(from) {
    if (!confirm(`Remove the fee that takes effect ${fmtStart(from)}?`)) return;
    saveSection("fees", { initial_fee_schedule: (C.fees.initial_fee_schedule || []).filter((s) => s.effective_from !== from) }, "Fee removed");
  }
  function feeTerms(form) {
    const n = (v) => (v === "" ? undefined : Number(v));
    saveSection("fees", { refund_window_days: n(form.refund_window_days.value), invoice_due_days: n(form.invoice_due_days.value), reminder_lead_days: n(form.reminder_lead_days.value) });
  }

  // ── Pay rates ──
  function ratesHtml() {
    const R = C.rates, edit = C.can_edit_rates;
    const byRole = {};
    for (const x of R.schedule || []) (byRole[x.role] = byRole[x.role] || []).push(x);
    return `<h2>Pay rates for the timesheet</h2>
      <p class="muted">The hourly rate each role's billable time is charged at. A new rate starts on the day you give and prices time worked from then on; time already worked keeps the rate it had. A role with no rate isn't billed until it has one.</p>
      <table class="tbl"><thead><tr><th>Role</th><th>Rate today</th><th>Changes</th></tr></thead><tbody>
        ${R.roles.map((role) => {
          const list = (byRole[role] || []).sort((a, b) => a.effective_from.localeCompare(b.effective_from));
          return `<tr><td>${esc(ROLE[role] || role)}</td><td>${R.current[role] != null ? `<b>${money(R.current[role])}</b>/h`
            : role === "NURSE_PHYSICIAN" ? '<span class="muted">none — time is logged as Nurse or Physician</span>' : '<span class="muted">no rate</span>'}</td>
            <td>${list.length ? list.map((x) => `${money(x.rate_cents_per_hour)} ${x.effective_from === "2000-01-01" ? "from the start" : `from ${fmtDate(x.effective_from)}`}${x.effective_from > C.today ? " (upcoming)" : ""}`).join(" · ") : "—"}</td></tr>`;
        }).join("")}</tbody></table>
      ${edit ? `<form class="inline-form" onsubmit="event.preventDefault(); ConfigViews.rateSet(this)">
        <label>Role <select name="role">${R.roles.map((r) => `<option value="${r}">${esc(ROLE[r] || r)}</option>`).join("")}</select></label>
        <label>Hourly rate <input name="amount" inputmode="decimal" placeholder="e.g. 175.00" required /></label>
        <label>Takes effect <input name="from" type="date" required value="${C.today}" /></label>
        <button class="mini">Set rate</button></form>` : ""}`;
  }
  async function rateSet(form) {
    const c = cents(form.amount.value);
    if (!(c >= 0) || form.amount.value.trim() === "") return UI.toast("Enter the hourly rate in dollars, e.g. 175.00", { kind: "warn" });
    try {
      await Api.program.timeRateSet(form.role.value, c, form.from.value);
      C = await Api.program.configuration(); render("rates");
      UI.toast(`${ROLE[form.role.value] || form.role.value}: ${money(c)}/h from ${fmtDate(form.from.value)}`);
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ── Eligibility ──
  function eligHtml() {
    const E = C.eligibility, edit = C.can_edit, dis = edit ? "" : "disabled";
    const net = (t) => (t.contracted == null ? "any" : t.contracted ? "in network" : "out of network");
    return `<h2>Eligibility</h2>
      <p class="muted">What the automatic eligibility review checks: the smallest amount in dispute for each kind of provider, how long after the plan's final decision a dispute can be filed, and the program's start date.</p>
      <form onsubmit="event.preventDefault(); ConfigViews.eligSave(this)">
        <table class="tbl"><thead><tr><th>Provider type</th><th>Network</th><th>Minimum in dispute</th></tr></thead><tbody>
          ${(E.thresholds || []).map((t, i) => `<tr><td>${esc(PTYPE[t.provider_type] || t.provider_type)}</td><td>${net(t)}</td>
            <td><input name="th${i}" inputmode="decimal" value="${(t.min_cents / 100).toFixed(2)}" ${dis} aria-label="Minimum for ${esc(t.provider_type)} ${net(t)}" /></td></tr>`).join("")}</tbody></table>
        <div class="inline-form">
          <label>Filing window (months after the final decision) <input name="window" type="number" min="0" max="120" value="${E.filing_window_months ?? ""}" ${dis} /></label>
          <label>Services covered after <input name="after" type="date" value="${esc(E.services_after || "")}" ${dis} /></label>
          ${edit ? `<button class="mini">Save eligibility</button>` : ""}</div></form>`;
  }
  function eligSave(form) {
    const thresholds = (C.eligibility.thresholds || []).map((t, i) => ({ ...t, min_cents: cents(form["th" + i].value) }));
    if (thresholds.some((t) => !(t.min_cents >= 0))) return UI.toast("Each minimum must be an amount in dollars", { kind: "warn" });
    saveSection("eligibility", { thresholds, filing_window_months: Number(form.window.value), services_after: form.after.value });
  }

  // ── Case outcomes ──
  // Kept in the page while it is edited (C.outcomes), saved as one list: its order is the order people choose from.
  function outcomesHtml() {
    const edit = C.can_edit, list = C.outcomes || [];
    return `<h2>Case outcomes</h2>
      <p class="muted">The outcomes a case can end with, and what each means. The Reviewer picks one when recording the outcome; it goes on the reports to the Agency and at the top of the case page. Cases keep the outcome they were given if it is later renamed or removed.</p>
      <table class="tbl cfg-outcomes" data-nosearch data-csort="kept-in-order"><thead><tr><th>Outcome</th><th>Definition</th>${edit ? "<th></th>" : ""}</tr></thead><tbody>
        ${list.map((o, i) => `<tr><td>${edit ? `<input data-o="${i}" data-f="name" value="${esc(o.name)}" aria-label="Outcome name" />` : `<b>${esc(o.name)}</b>`}</td>
          <td>${edit ? `<textarea data-o="${i}" data-f="definition" rows="2" aria-label="Definition of ${esc(o.name)}">${esc(o.definition || "")}</textarea>` : esc(o.definition || "—")}</td>
          ${edit ? `<td class="cfg-row-acts"><button class="mini ghost" title="Move up" onclick="ConfigViews.outcomeMove(${i},-1)" ${i ? "" : "disabled"}>↑</button>
            <button class="mini ghost" title="Move down" onclick="ConfigViews.outcomeMove(${i},1)" ${i < list.length - 1 ? "" : "disabled"}>↓</button>
            <button class="mini ghost" title="Remove" onclick="ConfigViews.outcomeRemove(${i})">Remove</button></td>` : ""}</tr>`).join("")}</tbody></table>
      ${edit ? `<p><button class="mini" onclick="ConfigViews.outcomeAdd()">＋ Add an outcome</button>
        <button class="mini primary" onclick="ConfigViews.outcomesSave()">Save outcomes</button></p>` : ""}
      ${(C.outcome_groups || []).length ? `<h3>Outcome structure</h3><div class="cfg-groups">${C.outcome_groups.map((g) =>
        `<div class="cfg-group"><b>${esc(g.group)}</b><ul>${(g.outcomes || []).map((o) => `<li>${esc(o)}</li>`).join("")}</ul></div>`).join("")}</div>` : ""}`;
  }
  // Typed values are read back before any re-draw, so moving or adding never loses an edit.
  function outcomesRead() {
    document.querySelectorAll("#cfg-outcomes [data-o]").forEach((el) => { C.outcomes[+el.dataset.o][el.dataset.f] = el.value; });
  }
  function outcomeMove(i, d) { outcomesRead(); const l = C.outcomes, j = i + d; [l[i], l[j]] = [l[j], l[i]]; render("outcomes"); }
  function outcomeRemove(i) { outcomesRead(); C.outcomes.splice(i, 1); render("outcomes"); }
  function outcomeAdd() { outcomesRead(); C.outcomes.push({ name: "", definition: "" }); render("outcomes");
    document.querySelector(`#cfg-outcomes [data-o="${C.outcomes.length - 1}"][data-f="name"]`)?.focus(); }
  function outcomesSave() {
    outcomesRead();
    saveSection("outcomes", C.outcomes.map((o) => ({ name: o.name.trim(), definition: (o.definition || "").trim() })), "Outcomes saved");
  }

  // ── Deadlines ──
  function clocksHtml() {
    const edit = C.can_edit, dis = edit ? "" : "disabled";
    const list = C.clocks || [];
    return `<h2>Deadlines</h2>
      <p class="muted">How long each step of a dispute has. Past the deadline a case is late; past the hard limit (if there is one) it's in breach. A change applies to deadlines that start after you save.</p>
      <form onsubmit="event.preventDefault(); ConfigViews.clocksSave(this)">
        <table class="tbl"><thead><tr><th>Deadline</th><th>Days</th><th>Hard limit (days)</th><th>Counted in</th><th>Counted from</th></tr></thead><tbody>
          ${list.map((c, i) => `<tr><td>${esc(c.label || c.name)}${c.cite ? `<div class="muted">${esc(c.cite)}</div>` : ""}</td>
            <td><input name="d${i}" type="number" min="1" max="730" value="${c.days}" ${dis} aria-label="Days for ${esc(c.label || c.name)}" /></td>
            <td><input name="m${i}" type="number" min="0" max="730" value="${c.max_days || ""}" placeholder="none" ${dis} aria-label="Hard limit for ${esc(c.label || c.name)}" /></td>
            <td><select name="t${i}" ${dis}><option value="calendar"${c.day_type !== "business" ? " selected" : ""}>calendar days</option><option value="business"${c.day_type === "business" ? " selected" : ""}>business days</option></select></td>
            <td class="muted">${esc(basisText(c.basis))}</td></tr>`).join("")}</tbody></table>
        ${edit && list.length ? `<button class="mini">Save deadlines</button>` : ""}</form>`;
  }
  // "plan_notified_at" -> "plan notified"
  const BASIS = { received_at: "dispute received", packet_complete_at: "packet complete", estimate_sent_at: "estimate sent",
    rfi_sent_at: "information requested", plan_notified_at: "plan notified", outreach_at: "provider contacted",
    withdrawal_requested_at: "withdrawal requested", final_determination_at: "plan's final decision" };
  const basisText = (b) => BASIS[b] || String(b || "").replace(/_at$/, "").replace(/_/g, " ");
  function clocksSave(form) {
    const body = (C.clocks || []).map((c, i) => ({ name: c.name, days: Number(form["d" + i].value), max_days: Number(form["m" + i].value || 0), day_type: form["t" + i].value }));
    saveSection("clocks", body);
  }

  // ── Billing ──
  function billingHtml() {
    const B = C.billing, edit = C.can_edit, dis = edit ? "" : "disabled";
    return `<h2>Billing for review time</h2>
      <p class="muted">How the office's invoices for review time are made: numbering, payment terms, tax and who they're addressed to.</p>
      <form class="cfg-grid" onsubmit="event.preventDefault(); ConfigViews.billingSave(this)">
        <label>Invoice number prefix <input name="invoice_prefix" value="${esc(B.invoice_prefix || "")}" ${dis} /></label>
        <label>Due (days) <input name="due_days" type="number" min="0" max="365" value="${B.due_days ?? ""}" ${dis} /></label>
        <label>Tax (%) <input name="tax_pct" type="number" min="0" max="50" step="0.01" value="${B.tax_pct ?? 0}" ${dis} /></label>
        <label>Smallest invoice ($) <input name="min_invoice" inputmode="decimal" value="${((B.min_invoice_cents || 0) / 100).toFixed(2)}" ${dis} /></label>
        <label>Bill to <input name="bill_to_name" value="${esc(B.bill_to_name || "")}" ${dis} /></label>
        <label>Bill-to email <input name="bill_to_email" type="email" value="${esc(B.bill_to_email || "")}" ${dis} /></label>
        <label class="cfg-wide">Memo ({period} becomes the billing period) <input name="memo" value="${esc(B.memo || "")}" ${dis} /></label>
        <label><input type="checkbox" name="consolidate" ${B.consolidate !== false ? "checked" : ""} ${dis} /> One invoice per period (off: one per case)</label>
        <label><input type="checkbox" name="block_on_unrated" ${B.block_on_unrated !== false ? "checked" : ""} ${dis} /> Don't invoice while some billable time has no rate</label>
        ${edit ? `<div><button class="mini">Save billing</button></div>` : ""}</form>`;
  }
  function billingSave(form) {
    saveSection("billing", {
      invoice_prefix: form.invoice_prefix.value.trim(), due_days: Number(form.due_days.value || 0), tax_pct: Number(form.tax_pct.value || 0),
      min_invoice_cents: cents(form.min_invoice.value || 0), bill_to_name: form.bill_to_name.value.trim(), bill_to_email: form.bill_to_email.value.trim(),
      memo: form.memo.value, consolidate: form.consolidate.checked, block_on_unrated: form.block_on_unrated.checked,
    });
  }

  return { page, outcomeMove, outcomeRemove, outcomeAdd, outcomesSave, zoneSave, feeAdd, feeRemove, feeTerms, lateFeeSave, rateSet, eligSave, clocksSave, billingSave };
})();
