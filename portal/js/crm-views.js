// crm-views.js — CRM screens: pipeline kanban, accounts 360, leads, tasks, search.
const CrmViews = (() => {
  // See views.js: bind after router innerHTML injection (macrotask, not microtask).
  const afterRender = (fn) => setTimeout(fn, 0);
  const $ = (sel) => document.querySelector(sel);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmtDate = (d) => (d ? new Date(/^\d{4}-\d{2}-\d{2}$/.test(d) ? d + "T12:00:00Z" : d).toLocaleDateString("en-US", { dateStyle: "medium", ...(/^\d{4}-\d{2}-\d{2}$/.test(d) ? { timeZone: "UTC" } : {}) }) : "—");
  const badge = (s) => `<span class="badge s-${esc(s).toLowerCase().replace(/_/g, "-")}">${esc(s)}</span>`;
  const err = (e) => `<p class="error">${esc(e.message)}</p>`;
  const role = (r) => (Auth.claims()?.roles || []).includes(r);
  const can = (...rs) => Auth.can(...rs);
  // Server pager shared by accounts/leads/tasks: Pager (ui.js) draws the
  // rows-per-page choice and Previous / Next; each page replaces the rows.
  const pagerHtml = (id) => `<div id="${id}-pg"></div>`;
  function bindPager(id, tbodySel, state, fetchPage, rowsHtml) {
    Pager.server(document.getElementById(id + "-pg"), { total: state.total, shown: state.loaded.length }, async (limit, offset) => {
      const r = await fetchPage(offset, limit);
      state.loaded = r.rows;
      state.total = r.total ?? state.total;
      document.querySelector(tbodySel).innerHTML = rowsHtml(r.rows);
      return { total: state.total, shown: r.rows.length };
    });
  }

  // ---- Pipeline (kanban over case statuses) ----------------------------------
  // Covers EVERY status a case can hold — a status missing here makes those
  // disputes silently vanish from the board.
  const PIPELINE = [
    ["INITIATED", "Initiated"], ["NEGOTIATION_TRACKED", "Negotiation"],
    ["OFFER_WINDOW_OPEN", "Offer window"], ["OFFERS_REVEALED", "Revealed"],
    ["IN_REVIEW", "In review"], ["DETERMINED", "Determined"],
    ["PAYMENT_PENDING", "Payment pending"],
    ["CLOSED_PAID", "Closed (paid)"], ["CLOSED_DISMISSED", "Closed (dismissed)"],
    // FL AHCA vocabulary (2026): completed cases REST at Decided - Invoice
    // Paid; only these four states are true closures.
    ["Decided - Invoice Paid", "Decided — invoice paid"], ["Invoice Paid", "Invoice paid — closed"],
    ["Plan Opt-Out", "Plan opt-out"], ["Ineligible", "Ineligible"],
    ["Dismissed", "Dismissed"], ["Withdrawn", "Withdrawn"],
    ["Plan Notification Packet Issued", "Plan notified"],
  ];
  // Stages the board treats as "done" -- de-emphasized column styling, and
  // excluded from the default empty-stage hiding logic's "likely to fill up
  // soon" assumption (a closed stage being empty is normal, not a gap).
  const TERMINAL_STAGES = new Set([
    "CLOSED_PAID", "CLOSED_DISMISSED",
    "Decided - Invoice Paid", "Invoice Paid", "Plan Opt-Out", "Ineligible", "Dismissed",
    "Withdrawn", "Provider Closure Letter Issued", "Provider - Withdrawal",
  ]);
  // Pipeline: every column shows its stage's true count (pipelineCounts) and pages its own cards through /cases,
  // PIPE_PAGE at a time, so a stage with hundreds of disputes stays usable and nothing past the first 200 cases
  // goes missing (the board used to load one list of 200 and group it client-side).
  const PIPE_PAGE = 20;
  const pipeCols = {};
  // The board's filters (kept while the page is open): who the cases are with, and a search term.
  const pipeFilter = { assigned: "", q: "" };
  // A stage's phase, for the colour on its column: from the words in its name (the program's stages are configured).
  function stagePhase(status) {
    if (status === "__NONE__") return "intake";
    if (TERMINAL_STAGES.has(status) || /closed|withdraw|dismiss|opt-out|ineligible|invoice paid/i.test(status)) return "done";
    if (/letter|notification|packet|issued|notice/i.test(status)) return "notice";
    if (/pending|awaiting|rfi|response/i.test(status)) return "waiting";
    if (/intake|initiated|new/i.test(status)) return "intake";
    return "review";
  }
  const pipeUsd = (cents) => "$" + Math.round((Number(cents) || 0) / 100).toLocaleString();
  const initials = (name) => String(name || "?").split(/[\s._@-]+/).filter(Boolean).slice(0, 2).map((w) => w[0].toUpperCase()).join("") || "?";
  function pipeSla(days, closed) {
    if (closed || days == null) return "";
    const cls = days <= 3 ? "is-bad" : days <= 10 ? "is-warn" : "";
    return `<span class="kc-sla ${cls}" title="Business days left on the statutory clock">${days <= 0 ? "due" : days + "d left"}</span>`;
  }

  async function pipeline() {
    try {
      const prog = await Api.program.get().catch(() => null);
      // Programmed tenants (e.g. FL AHCA) never populate the federal `status`
      // column -- they track real progress via internal_status/agency_status
      // instead (programops.go only ever SETs internal_status), so the board
      // groups by internal_status for them and by status otherwise.
      const internalStatuses = prog?.config?.statuses?.internal;
      const byField = internalStatuses?.length ? "internal_status" : "status";
      const knownStages = internalStatuses?.length ? internalStatuses.map((s) => [s, s]) : PIPELINE;
      const opts = {};
      if (pipeFilter.assigned) opts.assigned = pipeFilter.assigned;
      if (pipeFilter.q) opts.q = pipeFilter.q;
      const counts = await Api.cases.pipelineCounts(byField, opts);
      const countOf = new Map((counts.stages || []).map((x) => [x.stage, x]));
      // Safety net: a status the board doesn't know yet still gets a column
      // instead of its cases disappearing silently; cases with no status at
      // all get a leading "Not yet reviewed" column.
      const known = new Set(knownStages.map(([s]) => s));
      const extra = (counts.stages || []).map((x) => x.stage).filter((s) => s && !known.has(s));
      const stages = [
        ...(countOf.get("")?.count ? [["__NONE__", "Not yet reviewed"]] : []),
        ...knownStages,
        ...extra.map((s) => [s, s.replace(/_/g, " ").toLowerCase()]),
      ];
      const amtLabel = (prog && prog.config) ? "Disputed" : "QPA";
      const amtKey = (prog && prog.config) ? "disputed_cents" : "qpa_cents";
      for (const k of Object.keys(pipeCols)) delete pipeCols[k];
      let openN = 0, openAmt = 0, doneN = 0, doneAmt = 0, unassigned = 0;
      const cols = stages.map(([status, label], i) => {
        const c = countOf.get(status === "__NONE__" ? "" : status) || { count: 0 };
        const n = Number(c.count) || 0, amt = Number(c[amtKey]) || 0, un = Number(c.unassigned) || 0;
        const phase = stagePhase(status);
        if (phase === "done") { doneN += n; doneAmt += amt; } else { openN += n; openAmt += amt; unassigned += Number(c.unassigned_paid) || 0; }
        const colId = `pipe-col-${i}`;
        if (n) pipeCols[colId] = { status, byField, total: n, cursors: [""], page: 0, closed: phase === "done",
          amtField: (prog && prog.config) ? "disputed_amount_cents" : "qpa_cents" };
        return `<section class="kanban-col ph-${phase} ${phase === "done" ? "is-terminal" : ""} ${n ? "" : "is-empty"}" aria-label="${esc(label)}: ${n}">
          <header class="kc-head"><h3>${esc(label)}</h3><span class="kanban-count">${n.toLocaleString()}</span>
            <div class="kc-sum">${pipeUsd(amt)} ${esc(amtLabel.toLowerCase())}${un && phase !== "done" ? ` · <span class="kc-un">${un.toLocaleString()} unassigned</span>` : ""}</div></header>
          <div class="kanban-col-body" id="${colId}">${n ? `<p class="muted">Loading…</p>` : `<p class="kc-empty">Nothing here</p>`}</div>
          ${n > PIPE_PAGE ? `<div class="kanban-pager" id="${colId}-pager"></div>` : ""}</section>`;
      });
      afterRender(() => {
        $("#pipe-show-empty")?.addEventListener("change", (ev) => {
          $("#pipe-board")?.classList.toggle("hide-empty", !ev.target.checked);
          $("#pipe-stages")?.classList.toggle("show-empty", ev.target.checked);
          pipeEdges();
        });
        const board = $("#pipe-board");
        if (board) {
          board.addEventListener("scroll", pipeEdges, { passive: true });
          board.addEventListener("keydown", (ev) => {
            if (ev.key === "ArrowRight" || ev.key === "ArrowLeft") { ev.preventDefault(); pipelineScroll(ev.key === "ArrowRight" ? 1 : -1); }
          });
          new ResizeObserver(pipeEdges).observe(board);
        }
        pipeEdges();
        const search = $("#pipe-q");
        let tmr;
        search?.addEventListener("input", () => { clearTimeout(tmr); tmr = setTimeout(() => { pipeFilter.q = search.value.trim(); App.rerender(); }, 350); });
        if (pipeFilter.q && search) { search.focus(); search.setSelectionRange(search.value.length, search.value.length); }
        Object.keys(pipeCols).forEach((colId) => pipelinePage(colId, 0));
      });
      const total = Number(counts.total) || 0;
      const seg = (v, l) => `<button type="button" class="seg ${pipeFilter.assigned === v ? "on" : ""}" aria-pressed="${pipeFilter.assigned === v}" onclick="CrmViews.pipelineAssigned('${v}')">${l}</button>`;
      return `<div class="wide-view"><div class="view-head"><h1>Pipeline</h1>
          <span class="muted">${total.toLocaleString()} dispute${total === 1 ? "" : "s"}${pipeFilter.q ? ` matching “${esc(pipeFilter.q)}”` : ""} · amounts are ${esc(amtLabel.toLowerCase())}</span></div>
        <div class="pipe-bar">
          <input type="search" id="pipe-q" class="pipe-q" placeholder="Search case #, service line, party…" value="${esc(pipeFilter.q)}" aria-label="Search the pipeline" maxlength="100" />
          <div class="segs" role="group" aria-label="Whose cases">${seg("", "All")}${seg("mine", "Mine")}${seg("unassigned", "Unassigned")}</div>
          <label class="pipe-toggle"><input type="checkbox" id="pipe-show-empty" /> Show empty stages</label>
        </div>
        <div class="pipe-sum">
          <div><span class="ps-l">Open</span><span class="ps-n">${openN.toLocaleString()}</span><span class="ps-s">${pipeUsd(openAmt)}</span></div>
          <div><span class="ps-l">Waiting for a reviewer</span><span class="ps-n ${unassigned ? "is-warn" : ""}">${unassigned.toLocaleString()}</span><span class="ps-s">paid, nobody assigned yet</span></div>
          <div><span class="ps-l">Closed</span><span class="ps-n">${doneN.toLocaleString()}</span><span class="ps-s">${pipeUsd(doneAmt)}</span></div>
          <div><span class="ps-l">Stages in use</span><span class="ps-n">${stages.filter(([st]) => (countOf.get(st === "__NONE__" ? "" : st)?.count || 0) > 0).length}</span><span class="ps-s">of ${stages.length}</span></div>
        </div>
        <nav class="pipe-stages" id="pipe-stages" aria-label="Jump to a stage">${stages.map(([status, label], i) => {
          const n = Number(countOf.get(status === "__NONE__" ? "" : status)?.count) || 0;
          return `<button type="button" class="pst ph-${stagePhase(status)} ${n ? "" : "is-empty"}" data-col="${i}" onclick="CrmViews.pipelineJump(${i})">${esc(label)} <span>${n.toLocaleString()}</span></button>`;
        }).join("")}</nav>
        <div class="kanban-wrap" id="pipe-wrap">
          <button type="button" class="kb-nav prev" aria-label="Scroll to earlier stages" onclick="CrmViews.pipelineScroll(-1)">‹</button>
          <div class="kanban hide-empty" id="pipe-board" tabindex="0" aria-label="Pipeline stages — use the arrow keys to scroll">${cols.join("")}</div>
          <button type="button" class="kb-nav next" aria-label="Scroll to later stages" onclick="CrmViews.pipelineScroll(1)">›</button>
        </div>
        <p class="pipe-hint" id="pipe-hint" hidden>Scroll sideways to see all <b id="pipe-hint-n"></b> stages — drag the bar below the board, use the ‹ › buttons, the arrow keys, or pick a stage above.</p></div>`;
    } catch (e) { return `<h1>Pipeline</h1>` + err(e); }
  }
  function pipelineAssigned(v) { pipeFilter.assigned = v; App.rerender(); }
  // Showing there is more board to either side: fades + arrows at the edges that have more, the hint while the
  // board is wider than the screen, and the stage strip lighting up the columns now in view.
  function pipeEdges() {
    const board = $("#pipe-board"), wrap = $("#pipe-wrap");
    if (!board || !wrap) return;
    const max = board.scrollWidth - board.clientWidth;
    wrap.classList.toggle("can-left", board.scrollLeft > 4);
    wrap.classList.toggle("can-right", board.scrollLeft < max - 4);
    const hint = $("#pipe-hint");
    const shown = [...board.querySelectorAll(".kanban-col")].filter((c) => c.offsetParent !== null);
    if (hint) { hint.hidden = max <= 4; const n = $("#pipe-hint-n"); if (n) n.textContent = shown.length; }
    const left = board.getBoundingClientRect().left, right = left + board.clientWidth;
    board.querySelectorAll(".kanban-col").forEach((c, i) => {
      const r = c.getBoundingClientRect();
      document.querySelector(`#pipe-stages [data-col="${i}"]`)?.classList.toggle("in-view", r.width > 0 && r.right > left + 40 && r.left < right - 40);
    });
  }
  function pipelineScroll(dir) {
    const board = $("#pipe-board");
    if (board) board.scrollBy({ left: dir * Math.max(280, board.clientWidth - 280), behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
  }
  function pipelineJump(i) {
    const board = $("#pipe-board"), col = board?.querySelectorAll(".kanban-col")[i];
    if (!col) return;
    if (col.classList.contains("is-empty") && board.classList.contains("hide-empty")) { // show empty stages first
      const t = $("#pipe-show-empty"); if (t) { t.checked = true; t.dispatchEvent(new Event("change")); }
    }
    board.scrollTo({ left: col.offsetLeft - board.offsetLeft - 8, behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
    col.classList.add("flash"); setTimeout(() => col.classList.remove("flash"), 900);
  }

  // One page of one column: `delta` +1 = next, -1 = previous, 0 = reload the current page. Pages are keyset
  // cursors from /cases; the column keeps the cursor of every page it has opened, so Previous needs no offset.
  async function pipelinePage(colId, delta) {
    const col = pipeCols[colId];
    const body = document.getElementById(colId);
    if (!col || !body) return;
    const page = Math.max(0, col.page + delta);
    if (page >= col.cursors.length) return;
    const params = { limit: PIPE_PAGE, cursor: col.cursors[page] };
    if (col.status === "__NONE__") params.unset = col.byField; else params[col.byField] = col.status;
    if (pipeFilter.assigned) params.assigned = pipeFilter.assigned;
    if (pipeFilter.q) params.q = pipeFilter.q;
    body.classList.add("is-loading");
    try {
      const r = await Api.cases.list(params);
      col.page = page;
      if (r.next_cursor && col.cursors.length === page + 1) col.cursors.push(r.next_cursor);
      body.innerHTML = (r.cases || []).map((c) => {
        const who = c.assigned_to ? (c.assigned_name || "Assigned") : "";
        return `<a class="kanban-card" href="#/cases/${esc(c.id)}">
          <span class="kc-top"><b class="kc-no">${esc(c.case_number)}</b><span class="kc-amt">${pipeUsd(c[col.amtField])}</span></span>
          ${c.service_line || (c.triage_lane && c.triage_lane !== "STANDARD") ? `<span class="kc-tags">${c.service_line ? `<span class="kc-tag">${esc(c.service_line.replace(/_/g, " ").toLowerCase())}</span>` : ""}${c.triage_lane && c.triage_lane !== "STANDARD" ? `<span class="kc-tag lane-${esc(c.triage_lane.toLowerCase())}">${esc(c.triage_lane.replace(/_/g, " ").toLowerCase())}</span>` : ""}</span>` : ""}
          <span class="kc-foot">${who ? `<span class="kc-who"><span class="kc-av" aria-hidden="true">${esc(initials(who))}</span>${esc(who)}</span>` : `<span class="kc-who is-un">Unassigned</span>`}${pipeSla(c.sla_days_remaining, col.closed)}</span></a>`;
      }).join("") || `<p class="kc-empty">No disputes on this page.</p>`;
      body.scrollTop = 0;
      const pager = document.getElementById(colId + "-pager");
      if (pager) {
        const from = page * PIPE_PAGE + 1, to = Math.min(col.total, (page + 1) * PIPE_PAGE);
        const hasNext = col.cursors.length > page + 1 && to < col.total;
        pager.innerHTML = `<button class="mini" ${page ? "" : "disabled"} onclick="CrmViews.pipelinePage('${colId}', -1)" aria-label="Previous page">‹</button>
          <span class="muted">${from}–${to} of ${col.total.toLocaleString()}</span>
          <button class="mini" ${hasNext ? "" : "disabled"} onclick="CrmViews.pipelinePage('${colId}', 1)" aria-label="Next page">›</button>`;
      }
    } catch (e) { body.innerHTML = err(e); }
    finally { body.classList.remove("is-loading"); }
  }

  // ---- Accounts ---------------------------------------------------------------
  const ACC_TYPES = [["", "All"], ["PROVIDER", "Providers"], ["PAYER", "Payers"], ["IDRE", "IDRE entities"], ["AUDITOR", "Auditors"], ["OTHER", "Other"]];
  let accType = "";
  function accounts() {
    afterRender(() => { accountsSegs(); accountsBox(accType); });
    SrvSearch.on("acc", () => { accountsSegs(); accountsBox(accType); });
    return `<div class="view-head"><h1>Accounts</h1><span class="muted">providers, health plans and the other organizations on the platform</span>
        <span class="spacer" style="flex:1"></span><a class="button" href="#/crm/accounts/new">New account</a></div>
      <div class="list-bar">${SrvSearch.html("acc", "Search name, NPI, phone, website…")}<div id="acc-segs"></div></div>
      <div id="acc-box"><p class="muted">Loading…</p></div>`;
  }
  // The type switch, each type with its number of accounts (the search applies).
  async function accountsSegs() {
    const box = $("#acc-segs");
    if (!box) return;
    const counts = await Promise.all(ACC_TYPES.map(([t]) => Api.crm.accounts(t || null, { limit: 1, ...SrvSearch.params("acc") }).then((r) => r.total).catch(() => null)));
    box.innerHTML = ListUI.segs("Account type", ACC_TYPES.map(([value, label], i) => ({ value, label, count: counts[i] })), accType, "CrmViews.accountsFilter");
  }

  async function accountsBox(type) {
    const box = $("#acc-box");
    if (!box) return;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const page = await Api.crm.accounts(type || null, { limit: Pager.size(), ...SrvSearch.params("acc") });
      const state = { loaded: page.accounts.slice(), next: page.next_offset, total: page.total };
      const rowsHtml = (list) => list.map((a) => `<tr class="click" onclick="location.hash='#/crm/accounts/${a.id}'">
            <td><span class="cell-main">${esc(a.legal_name)}</span></td><td>${badge(a.type)}</td><td class="mono">${esc(a.npi) || '<span class="muted">—</span>'}</td><td>${esc(a.phone) || '<span class="muted">—</span>'}</td></tr>`).join("");
      if (!state.loaded.length) {
        box.innerHTML = `<p class="muted">${SrvSearch.q("acc") ? `No account matches “${esc(SrvSearch.q("acc"))}”.` : type ? `No ${type.toLowerCase()} accounts.` : "No accounts yet — convert leads or create one."}</p>`;
        return;
      }
      box.innerHTML = `<table id="acc-tb" data-serverpaged><thead><tr><th>Legal name</th><th>Type</th><th>NPI</th><th>Phone</th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("acc");
      bindPager("acc", "#acc-tb tbody", state,
        (off, limit) => Api.crm.accounts(type || null, { limit, offset: off, ...SrvSearch.params("acc") }).then((r) => ({ rows: r.accounts, total: r.total })),
        rowsHtml);
    } catch (e) { box.innerHTML = err(e); }
  }
  function accountsFilter(type) { accType = type; accountsSegs(); accountsBox(type); }

  async function account360(id) {
    try {
      const d = await Api.crm.account360(id);
      const a = d.account;
      Palette.remember("account", a.id, a.legal_name);
      let html = `<h1>${esc(a.legal_name)}</h1><p>${badge(a.type)} · NPI ${esc(a.npi) || "—"} · ${esc(a.phone) || "—"}</p>`;
      if (App.feature("entity360"))
        html += `<div class="actions"><button class="mini" onclick="CrmViews.openEntity360('accounts','${esc(a.id)}')">Entity 360 — every rail this organization appears on</button></div>`;
      if (d.health)
        html += `<div class="cards"><div class="card health-${esc(d.health.band)}">
          <div class="num">${d.health.score}</div><div class="lbl">Relationship health — ${esc(d.health.band)}</div>
          <div class="muted">${d.health.open_disputes} open disputes · ${d.health.sla_breaches} SLA breaches · ${esc(d.health.formula)}</div></div></div>`;
      html += `<h2>Contacts (${d.contacts.length})</h2>` +
        (d.contacts.length ? `<table><tbody>` + d.contacts.map((c) =>
          `<tr><td>${esc(c.name)}</td><td>${esc(c.role_title)}</td><td>${esc(c.email)}</td><td>${esc(c.phone)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No contacts.</p>`) +
        (!can("CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? "" : `<form id="nc" class="form"><b>Add contact</b>
           <input name="name" placeholder="Name" required />
           <input name="role_title" placeholder="Role (e.g. Billing lead)" />
           <input name="email" type="email" placeholder="Email" />
           <input name="phone" placeholder="Phone" /><button>Add</button></form>`);
      html += `<h2>Disputes (${d.cases.length})</h2>` +
        (d.cases.length ? `<table><tbody>` + d.cases.map((c) =>
          `<tr class="click" onclick="location.hash='#/cases/${c.id}'"><td>${esc(c.case_number)}</td>
           <td>${badge(c.status)}</td><td>${esc(c.service_line)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No disputes for this account.</p>`);
      html += `<h2>Notes</h2>` +
        (d.notes.length ? `<table><tbody>` + d.notes.map((n) =>
          `<tr><td>${esc(n.body)}</td><td class="muted">${fmtDate(n.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No notes.</p>`) +
        `<form id="nn" class="form"><b>Add note</b><input name="body" required /><button>Add note</button></form>`;
      afterRender(() => {
        document.querySelector("#nc")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = Object.fromEntries(new FormData(ev.target));
          try { await Api.crm.createContact({ account_id: id, ...f }); UI.toast("Contact added"); App.rerender(); }
          catch (e) { UI.toast(e.message, { kind: "warn" }); }
        });
        document.querySelector("#nn")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = Object.fromEntries(new FormData(ev.target));
          try { await Api.crm.addNote({ record_type: "ACCOUNT", record_id: id, body: f.body }); UI.toast("Note added"); App.rerender(); }
          catch (e) { UI.toast(e.message, { kind: "warn" }); }
        });
      });
      return html;
    } catch (e) { return err(e); }
  }

  function accountNew() {
    afterRender(() => document.querySelector("#na").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = Object.fromEntries(new FormData(ev.target));
      try { await Api.crm.createAccount(f); UI.toast("Account created"); location.hash = "#/crm/accounts"; }
      catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }));
    return `<h1>New account</h1><form id="na" class="form">
      <label>Type <select name="type"><option>PROVIDER</option><option>PAYER</option><option>IDRE</option><option>AUDITOR</option><option>OTHER</option></select></label>
      <label>Legal name <input name="legal_name" required /></label>
      <label>NPI <input name="npi" /></label>
      <label>Phone <input name="phone" /></label>
      <button>Create account</button></form>`;
  }

  // ---- Leads ---------------------------------------------------------------------
  const LEAD_STATUSES = [["OPEN", "Not converted"], ["CONVERTED", "Converted"], ["", "All"]];
  let leadStatus = "OPEN";
  function leads() {
    afterRender(() => { leadsSegs(); leadsBox(leadStatus); });
    SrvSearch.on("lead", () => { leadsSegs(); leadsBox(leadStatus); });
    return `<div class="view-head"><h1>Leads</h1><span class="muted">voice intake becomes a lead automatically; convert qualified leads into accounts</span></div>
      <div class="list-bar">${SrvSearch.html("lead", "Search name, organization, phone, email, summary…")}<div id="lead-segs"></div></div>
      <div id="lead-box"><p class="muted">Loading…</p></div>`;
  }
  async function leadsSegs() {
    const box = $("#lead-segs");
    if (!box) return;
    const counts = await Promise.all(LEAD_STATUSES.map(([st]) => Api.crm.leads({ limit: 1, status: st, ...SrvSearch.params("lead") }).then((r) => r.total).catch(() => null)));
    box.innerHTML = ListUI.segs("Lead status", LEAD_STATUSES.map(([value, label], i) => ({ value, label, count: counts[i] })), leadStatus, "CrmViews.leadsFilter");
  }

  async function leadsBox(status) {
    const box = $("#lead-box");
    if (!box) return;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const page = await Api.crm.leads({ limit: Pager.size(), status, ...SrvSearch.params("lead") });
      const state = { loaded: page.leads.slice(), next: page.next_offset, total: page.total };
      const rowsHtml = (list) => list.map((l) => `<tr><td><span class="cell-main">${esc(l.name) || '<span class="muted">—</span>'}</span>${l.organization ? `<span class="cell-sub">${esc(l.organization)}</span>` : ""}</td>
            <td>${badge(l.source)}</td>
            <td><span class="cell-clamp" title="${esc(l.summary || "")}">${esc(l.summary || "—")}</span></td><td>${badge(l.status)}</td>
            <td>${l.status !== "CONVERTED" ? `<button class="mini" onclick="CrmViews.convert('${l.id}')">Convert</button>` : ""}</td></tr>`).join("");
      if (!state.loaded.length) {
        box.innerHTML = `<p class="muted">${SrvSearch.q("lead") ? `No lead matches “${esc(SrvSearch.q("lead"))}”.` : `No ${status === "OPEN" ? "unconverted " : status ? status.toLowerCase() + " " : ""}leads.`}</p>`;
        return;
      }
      box.innerHTML = `<table id="lead-tb" data-serverpaged><thead><tr><th>Lead</th><th>Source</th><th>Summary</th><th>Status</th><th></th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("lead");
      bindPager("lead", "#lead-tb tbody", state,
        (off, limit) => Api.crm.leads({ limit, offset: off, status, ...SrvSearch.params("lead") }).then((r) => ({ rows: r.leads, total: r.total })),
        rowsHtml);
    } catch (e) { box.innerHTML = err(e); }
  }
  function leadsFilter(status) { leadStatus = status; leadsSegs(); leadsBox(status); }

  async function convert(id) {
    const v = await UI.modal({ title: "Convert lead to account", submitLabel: "Convert",
      body: "Creates the account, links the lead's history, and marks the lead converted.",
      fields: [{ name: "type", label: "Account type", options: [["PROVIDER", "Provider"], ["PAYER", "Payer"], ["IDRE", "IDRE entity"], ["OTHER", "Other"]], required: true }] });
    if (!v) return;
    try { await Api.crm.convertLead(id, v.type); UI.toast("Lead converted to account"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Tasks ------------------------------------------------------------------------
  // "My tasks" used to be the ONLY view -- Api.crm.tasks(true, ...) hardcoded
  // the mine=true filter, so anything unassigned (e.g. the escalation tasks
  // casemgmt.go auto-creates with no assignee) or assigned to someone else
  // was invisible here with no way to see it at all. mine now comes from the
  // hash query string (same pattern cases() already uses for ?view=/?sort=)
  // so the toggle is a real navigation, not just a DOM patch, and survives
  // refresh/back-button.
  const TASK_STATUSES = [["OPEN", "Open"], ["DONE", "Done"], ["", "All"]];
  let taskStatus = "OPEN";
  function tasks() {
    const mine = new URLSearchParams(location.hash.split("?")[1] || "").get("all") !== "1";
    SrvSearch.on("task", () => { tasksSegs(mine); tasksBox(mine, taskStatus); });
    afterRender(() => {
      tasksSegs(mine);
      tasksBox(mine, taskStatus);
      document.querySelector("#nt")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = Object.fromEntries(new FormData(ev.target));
        try { const r = await Api.crm.createTask(f); UI.toast(`Task ${r.task_ref || ""} created`.trim()); App.rerender(); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      });
    });
    return `<div class="view-head"><h1>Tasks</h1><span class="muted">follow-ups on disputes and accounts, with their due dates</span></div>
      <div class="list-bar">
        ${ListUI.segs("Whose tasks", [{ value: "mine", label: "My tasks", href: "#/crm/tasks" }, { value: "all", label: "All tasks", href: "#/crm/tasks?all=1" }], mine ? "mine" : "all")}
        <div id="task-segs"></div>
        ${SrvSearch.html("task", "Search subject, task ref, case…")}
      </div>
      ${!can("CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ? "" : `<details class="panel"><summary>New task</summary>
        <form id="nt" class="inline-form">
        <input name="subject" placeholder="Subject" required />
        <input name="case_id" placeholder="Case ID (optional)" />
        <input name="due_date" type="date" aria-label="Due date" /><button>Create task</button></form></details>`}
      <div id="task-box"><p class="muted">Loading…</p></div>`;
  }
  async function tasksSegs(mine) {
    const box = $("#task-segs");
    if (!box) return;
    const counts = await Promise.all(TASK_STATUSES.map(([st]) => Api.crm.tasks(mine, { limit: 1, ...(st ? { status: st } : {}), ...SrvSearch.params("task") }).then((r) => r.total).catch(() => null)));
    box.innerHTML = ListUI.segs("Task status", TASK_STATUSES.map(([value, label], i) => ({ value, label, count: counts[i] })), taskStatus, `CrmViews.tasksFilterFor(${mine})`);
  }
  // A due date, red once it has passed and amber in the last three days (open tasks only).
  function dueCell(t) {
    if (!t.due_date) return '<span class="muted">—</span>';
    if (t.status !== "OPEN") return esc(t.due_date);
    const days = Math.floor((new Date(t.due_date + "T00:00:00") - new Date(new Date().toDateString())) / 864e5);
    return days < 0 ? `<span class="due-over">${esc(t.due_date)} · ${-days}d overdue</span>`
      : days <= 3 ? `<span class="due-soon">${esc(t.due_date)} · ${days ? `in ${days}d` : "today"}</span>` : esc(t.due_date);
  }

  async function tasksBox(mine, status) {
    const box = $("#task-box");
    if (!box) return;
    box.innerHTML = `<p class="muted">Loading…</p>`;
    try {
      const page = await Api.crm.tasks(mine, { limit: Pager.size(), ...(status ? { status } : {}), ...SrvSearch.params("task") });
      const state = { loaded: page.tasks.slice(), next: page.next_offset, total: page.total };
      const rowsHtml = (list) => list.map((t) => `<tr><td class="mono">${esc(t.task_ref || "—")}</td>
          <td><span class="cell-main">${esc(t.subject)}</span></td>
          ${mine ? "" : `<td>${esc(t.assignee || "—")}</td>`}
          <td>${t.case_id ? `<a href="#/cases/${esc(t.case_id)}" class="mono">${esc(t.case_id.slice(0, 8))}…</a>` : '<span class="muted">—</span>'}</td>
          <td>${dueCell(t)}</td>
          <td>${badge(t.status)}</td>
          <td>${t.status === "OPEN" ? `<button class="mini" onclick="CrmViews.done('${t.id}')">Mark done</button>` : ""}</td></tr>`).join("");
      if (!state.loaded.length) {
        box.innerHTML = `<p class="muted">${SrvSearch.q("task") ? `No task matches “${esc(SrvSearch.q("task"))}”.` : mine ? "No tasks assigned to you." : "No tasks for this tenant."}</p>`;
        return;
      }
      box.innerHTML = `<table id="task-tb" data-serverpaged><thead><tr><th>Ref</th><th>Subject</th>
          ${mine ? "" : "<th>Assignee</th>"}<th>Case</th><th>Due</th><th>Status</th><th></th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("task");
      bindPager("task", "#task-tb tbody", state,
        (off, limit) => Api.crm.tasks(mine, { limit, offset: off, ...(status ? { status } : {}), ...SrvSearch.params("task") }).then((r) => ({ rows: r.tasks, total: r.total })),
        rowsHtml);
    } catch (e) { box.innerHTML = err(e); }
  }
  function tasksFilter(mine, status) { taskStatus = status; tasksSegs(mine); tasksBox(mine, status); }
  // The status switch's handler: the switch calls fn('<status>'), so `mine` is bound here.
  function tasksFilterFor(mine) { return (status) => tasksFilter(mine, status); }

  async function done(id) {
    try { await Api.crm.completeTask(id); UI.toast("Task completed"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // ---- Global search -------------------------------------------------------------------
  async function search(q) {
    if (!q) return `<h1>Search</h1><p class="muted">Type in the search box above.</p>`;
    try {
      const hits = await Api.crm.search(q);
      const link = (h) => h.kind === "case" ? `#/cases/${h.id}`
        : h.kind === "document" ? `#/cases/${h.case_id || ""}`
        : h.kind === "task" ? (h.case_id ? `#/cases/${h.case_id}` : "#/crm/tasks")
        : h.kind === "invoice" ? (h.case_id ? `#/cases/${h.case_id}` : "#/finance")
        : h.kind === "account" ? `#/crm/accounts/${h.id}` : h.kind === "entity" ? `#/crm/360/${h.id}`
        : h.kind === "lead" ? "#/crm/leads" : "#/crm/accounts";
      return `<h1>Search: “${esc(q)}”</h1>` +
        (hits.length ? `<table><tbody>` + hits.map((h) =>
          `<tr class="click" onclick="location.hash='${link(h)}'"><td>${badge(h.kind)}</td>
           <td>${esc(h.label)}</td><td class="muted">${esc(h.detail)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No matches.</p>`);
    } catch (e) { return err(e); }
  }

  // ---- Calendar (month grid + agenda) ----------------------------------------------------
  // Statutory deadlines deserve a real calendar, not a list: month grid with
  // color-coded chips (statutory clocks = brass, tasks = green, offers = blue),
  // today highlighted, month navigation, and an agenda under it.
  // Field names here match what Api.cm.calendar() actually returns
  // (ref/kind/label/due/case_id, from casemgmt.go's calendar() handler) --
  // this was originally written against due_date/type/title/case_number,
  // a shape that endpoint has never returned, so it rendered "undefined"
  // throughout until adapted here during the main-branch merge.
  function calMonthGrid(items, y, m) {
    const first = new Date(y, m, 1);
    const daysInMonth = new Date(y, m + 1, 0).getDate();
    const todayISO = new Date().toISOString().slice(0, 10);
    const byDay = {};
    items.forEach((i) => {
      const d = String(i.due || "").slice(0, 10);
      if (d) (byDay[d] ||= []).push(i);
    });
    const chipCls = (k) => "cal-chip-" + calKind(k)[0];
    let cells = "";
    for (let i = 0; i < first.getDay(); i++) cells += `<div class="cal-cell empty"></div>`;
    for (let d = 1; d <= daysInMonth; d++) {
      const iso = `${y}-${String(m + 1).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
      const dayItems = byDay[iso] || [];
      cells += `<div class="cal-cell ${iso === todayISO ? "today" : ""} ${dayItems.length ? "has-items" : ""}">
        <span class="cal-num">${d}</span>
        ${dayItems.slice(0, 3).map((i) => `<div class="cal-chip ${chipCls(i.kind)}${calDone(i) ? " cal-done" : ""}"
            title="${calDone(i) ? "Done — " : esc(i.kind) + " — "}${esc(i.label)}">${calDone(i) ? "✓ " : ""}${i.case_id
              ? `<a href="#/cases/${esc(i.case_id)}">${esc(i.label)}</a>`
              : esc(i.label)}</div>`).join("")}
        ${dayItems.length > 3 ? `<span class="muted cal-more">+${dayItems.length - 3} more</span>` : ""}</div>`;
    }
    return `<div class="cal-grid">
      ${["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"].map((d) => `<div class="cal-dow">${d}</div>`).join("")}
      ${cells}</div>`;
  }

  // What each calendar item is: [colour group, icon, words]. A TPA's items are
  // the deadlines on its own disputes and its team's tasks (tpa_calendar.go).
  const CAL_KIND = {
    OFFER_WINDOW: ["stat", "⏱", "Offer window"], PERMISSION_DUE: ["stat", "⏱", "Permission due"],
    FEE_DUE: ["pay", "$", "Fee due"], INVOICE_DUE: ["pay", "$", "Invoice due"],
    DOCS_DUE: ["docs", "⇪", "Documents due"],
    PLAN_RESPONSE: ["watch", "◷", "Plan's response due"], DECISION_DUE: ["watch", "§", "Recommendation due"],
    TASK: ["task", "✓", "Task"], TASK_DONE: ["done", "✓", "Task done"],
  };
  // A task done stays on the calendar, ticked: the system's own tick themselves off when the case records the step.
  const calDone = (i) => !!i.done || i.kind === "TASK_DONE";
  const calDoneNote = (i) => {
    const at = i.done_at || String(i.detail || "").replace(/^done /, "");
    return `Done${at ? " " + fmtDate(at) : ""}${i.automatic ? " · automatically, when the case recorded it" : ""}`;
  };
  const calKind = (k) => CAL_KIND[k] || ["task", "•", k];
  async function calTaskDone(id) {
    try {
      await (Auth.can("TPA") ? Api.program.tpaTaskDone(id) : Api.crm.completeTask(id));
      UI.toast("Task done"); App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function calTaskAdd(form) {
    try {
      await Api.program.tpaTaskCreate({ subject: form.subject.value, due_date: form.due.value, case_id: form.case_id.value });
      UI.toast("Task added"); App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }
  async function calendar() {
    try {
      const items = await Api.cm.calendar();
      const isTPA = Auth.can("TPA");
      items.sort((a, b) => String(a.due).localeCompare(String(b.due)));
      const now = new Date();
      const state = { y: now.getFullYear(), m: now.getMonth() };
      const render = () => {
        const label = new Date(state.y, state.m, 1)
          .toLocaleDateString("en-US", { month: "long", year: "numeric" });
        const grid = document.getElementById("cal-grid-box");
        if (grid) {
          grid.innerHTML = calMonthGrid(items, state.y, state.m);
          document.getElementById("cal-month-label").textContent = label;
        }
      };
      const dateOnly = (s) => String(s).slice(0, 10);
      const groups = {};
      items.forEach((i) => (groups[dateOnly(i.due)] ||= []).push(i));
      const today = new Date().toISOString().slice(0, 10);
      const tomorrow = new Date(Date.now() + 86400000).toISOString().slice(0, 10);
      const dow = (d) => new Date(d + "T00:00:00").toLocaleDateString("en-US", { weekday: "short" }).toUpperCase();
      const dom = (d) => new Date(d + "T00:00:00").getDate();
      const heading = (d) => {
        const full = new Date(d + "T00:00:00").toLocaleDateString("en-US", { weekday: "long", month: "short", day: "numeric" });
        if (d === today) return "Today";
        if (d === tomorrow) return "Tomorrow";
        if (d < today) return `Overdue — ${full}`;
        return full;
      };
      const agenda = Object.keys(groups).map((d) => {
        const open = groups[d].filter((i) => !calDone(i)).length, doneN = groups[d].length - open;
        const cls = d === today ? "is-today" : d < today ? (open ? "is-overdue" : "is-past") : "";
        return `<div class="cal-group ${cls}">
          <div class="cal-date"><span class="dow">${dow(d)}</span><span class="dom">${dom(d)}</span></div>
          <div class="cal-col">
            <div class="cal-heading">${open || d >= today ? heading(d) : new Date(d + "T00:00:00").toLocaleDateString("en-US", { weekday: "long", month: "short", day: "numeric" })} <span class="muted">(${groups[d].length}${doneN ? `, ${doneN} done` : ""})</span></div>
            <div class="cal-rows">${groups[d].map((i) => {
              const linkable = !!i.case_id;
              const done = calDone(i);
              return `<div class="cal-item ${linkable ? "link" : ""}${done ? " is-done" : ""}" ${linkable ? `onclick="location.hash='#/cases/${esc(i.case_id)}'"` : ""}>
                <span class="cal-kind k-${done ? "done" : calKind(i.kind)[0]}" title="${esc(done ? "Done" : calKind(i.kind)[2])}">${done ? "✓" : calKind(i.kind)[1]}</span>
                <span class="cal-label" title="${esc(i.label)}">${esc(i.label)}${done ? `<span class="cal-detail">${esc(calDoneNote(i))}</span>` : i.detail ? `<span class="cal-detail">${esc(i.detail)}</span>` : ""}</span>
                ${i.kind === "TASK" && !done ? `<button class="mini" onclick="event.stopPropagation(); CrmViews.calTaskDone('${esc(i.ref)}')">Done</button>` : done ? `<span class="badge s-paid">Done</span>` : ""}
              </div>`;
            }).join("")}</div>
          </div>
        </div>`;
      }).join("");
      afterRender(() => {
        render();
        document.getElementById("cal-prev")?.addEventListener("click", () => {
          state.m--; if (state.m < 0) { state.m = 11; state.y--; } render();
        });
        document.getElementById("cal-next")?.addEventListener("click", () => {
          state.m++; if (state.m > 11) { state.m = 0; state.y++; } render();
        });
      });
      const caseOpts = [...new Map(items.filter((i) => i.case_id && i.case_number).map((i) => [i.case_id, i.case_number])).entries()];
      const taskForm = isTPA ? `<form class="inline-form cal-add" onsubmit="event.preventDefault(); CrmViews.calTaskAdd(this)">
          <input name="subject" placeholder="Add a task for your team, e.g. Call the clinic about the EOB" required maxlength="300" style="flex:1;min-width:260px" />
          <input name="due" type="date" required aria-label="Due date" />
          <select name="case_id" aria-label="Dispute"><option value="">no dispute</option>${caseOpts.map(([id, n]) => `<option value="${esc(id)}">${esc(n)}</option>`).join("")}</select>
          <button class="mini">Add task</button></form>` : "";
      return `<div class="view-head"><h1>Calendar</h1>
        <span class="muted">${isTPA ? "fees, documents and deadlines on your disputes, and your team's tasks" : "statutory clocks, offer windows, and task due dates"}</span></div>${taskForm}
        <div class="cal-nav">
          <button class="mini" id="cal-prev">‹ prev</button>
          <b id="cal-month-label"></b>
          <button class="mini" id="cal-next">next ›</button>
          <span class="cal-legend">${isTPA
            ? `<span class="cal-chip cal-chip-pay">payment due</span><span class="cal-chip cal-chip-docs">documents due</span><span class="cal-chip cal-chip-stat">deadline</span><span class="cal-chip cal-chip-watch">to watch</span><span class="cal-chip cal-chip-task">task</span><span class="cal-chip cal-chip-done cal-done">✓ done</span>`
            : `<span class="cal-chip cal-chip-stat">statutory / offer window</span><span class="cal-chip cal-chip-task">task</span><span class="cal-chip cal-chip-done cal-done">✓ done</span>`}</span></div>
        <div id="cal-grid-box"></div>
        <h2>Agenda</h2>` + (agenda || `<p class="muted">${isTPA ? "Nothing due on your disputes right now. Fees, document requests, deadlines and your tasks appear here as they arise." : "No upcoming deadlines or tasks."}</p>`);
    } catch (e) { return `<h1>Calendar</h1>` + err(e); }
  }

  // ---- Entity 360 ------------------------------------------------------------
  // One organization across every rail that collects data on it: CRM account,
  // TPA registry/clients, health plan registry/parties, cases, invoices,
  // payments, correspondence. Identity keys (NPI/TIN/NAIC/FEIN) link
  // automatically; name-only matches need a person's confirmation.
  const money = (c) => c == null ? "—" : "$" + (Number(c) / 100).toLocaleString("en-US", { minimumFractionDigits: 2 });

  // From any screen that knows a source row: resolve to the entity, offer
  // same-name entities for a human-confirmed merge when there are no
  // identity keys to settle it, then open the 360.
  async function openEntity360(sourceTable, sourceId) {
    try {
      const r = await Api.crm.entityLookup(sourceTable, sourceId);
      if (r.suggestions && r.suggestions.length) {
        const v = await UI.modal({
          title: "Same-name organizations found",
          body: "No identity key (NPI / TIN / NAIC) settles which organization this is. Link it to an existing entity only if you know they are the same organization — otherwise it keeps its own.",
          submitLabel: "Link",
          fields: [{ name: "entity_id", label: "Link to", options: [["", "Keep separate — none of these"]] .concat(
            r.suggestions.map((s) => [s.id, `${s.canonical_name} — ${s.entity_type}${s.npi ? " · NPI " + s.npi : ""}${s.naic ? " · NAIC " + s.naic : ""}${s.score != null ? ` · match ${(s.score * 100).toFixed(0)}%` : ""}${s.registry_confirmed ? " · ✓ NPPES" : ""}`])) }],
        });
        if (v && v.entity_id) {
          await Api.crm.entityLink(v.entity_id, { source_table: sourceTable, source_id: sourceId });
          UI.toast("Linked — opening that entity's 360");
          location.hash = `#/crm/360/${v.entity_id}`;
          return;
        }
      }
      location.hash = `#/crm/360/${r.entity_id}`;
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  function entitySearchPage() {
    afterRender(() => {
      $("#ent-q")?.addEventListener("input", async (e) => {
        const q = e.target.value.trim();
        const box = $("#ent-results");
        if (q.length < 2) { box.innerHTML = ""; return; }
        try {
          const list = (await Api.crm.entitySearch(q)).entities || [];
          box.innerHTML = list.length ? `<table><thead><tr><th>Name</th><th>Type</th><th>NPI</th><th>NAIC</th><th>Linked records</th><th></th></tr></thead><tbody>` +
            list.map((x) => `<tr class="click" onclick="location.hash='#/crm/360/${x.id}'"><td>${esc(x.canonical_name)}</td>
              <td>${badge(x.entity_type)}</td><td class="mono">${esc(x.npi) || "—"}</td><td class="mono">${esc(x.naic) || "—"}</td>
              <td class="num">${x.links}</td><td>›</td></tr>`).join("") + `</tbody></table>`
            : `<p class="muted">No entities match. Entities are created automatically as organizations register, onboard, or get CRM accounts.</p>`;
        } catch (e2) { box.innerHTML = err(e2); }
      });
      $("#ent-q")?.focus();
      // CRM outreach list (#14): established entities that have gone quiet —
      // enough disputes to be a relationship, nothing heard in a while.
      (async () => {
        const box = $("#ent-outreach");
        if (!box) return;
        try {
          const r = await Api.crm.entityOutreach(3, 90);
          const rows = r.outreach || [];
          box.innerHTML = rows.length ? `<h2>Outreach — established but quiet (${rows.length})</h2>
            <p class="muted">${r.min_cases}+ disputes, no note or correspondence in ${r.quiet_days} days.</p>
            <table><thead><tr><th>Organization</th><th>Type</th><th>Disputes</th><th>Last engagement</th><th></th></tr></thead><tbody>` +
            rows.map((x) => `<tr class="click" onclick="location.hash='#/crm/360/${x.entity_id}'"><td>${esc(x.canonical_name)}</td>
              <td>${badge(x.entity_type)}</td><td class="num">${x.cases}</td>
              <td class="mono">${x.last_engagement ? fmtDate(x.last_engagement) : "never"}</td>
              <td><button class="mini" onclick="event.stopPropagation();CrmViews.entityOutreachLead('${esc(x.canonical_name)}',${x.cases},'${x.last_engagement || ""}')">Add to leads</button></td></tr>`).join("") + `</tbody></table>`
            : "";
        } catch (e2) { box.innerHTML = ""; }
      })();
    });
    return `<div class="view-head"><h1>Entity 360</h1></div>
      <p class="muted">One organization across every rail — TPA filings, health plan submissions, CRM accounts, disputes, invoices. Search by name, NPI, TIN or NAIC.</p>
      <div class="kgqa-bar"><input id="ent-q" placeholder="Search organizations…" autocomplete="off" /></div>
      <div id="ent-results"></div>
      <div id="ent-outreach"></div>`;
  }

  // entityOutreachLead (#14): one click from the outreach table to a CRM lead.
  async function entityOutreachLead(name, cases, last) {
    try {
      await Api.crm.createLead({ organization: name,
        summary: `Outreach: ${cases} disputes on file${last ? ", last engagement " + last.slice(0, 10) : ", no engagement recorded"}.` });
      UI.toast("Lead added — see CRM → Leads");
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function entity360(id) {
    try {
      const d = await Api.crm.entity360(id);
      if (d.merged_into) {
        UI.toast(`“${d.merged_name || "This entity"}” was merged — opening the surviving record`);
        location.hash = `#/crm/360/${d.merged_into}`;
        return `<h1>Entity 360</h1><p class="muted">Redirecting…</p>`;
      }
      const e = d.entity;
      Palette.remember("entity", e.id, e.canonical_name);
      const srcLink = (l) => l.source_table === "accounts" ? `#/crm/accounts/${l.source_id}`
        : (l.source_table === "tpas" || l.source_table === "tpa_clients") ? "#/tpa" : "#/healthplan";
      let html = `<div class="view-head"><h1>${esc(e.canonical_name)}</h1>${badge(e.entity_type)}
          <a class="button mini" href="#/crm/360">Search</a>
          <button class="mini" onclick="CrmViews.entityLinkPicker('${id}')">Link a record…</button>
          <button class="mini danger" onclick="CrmViews.entityMergeModal('${id}')">Merge with another entity…</button></div>
        <p class="muted">${[e.npi && "NPI " + esc(e.npi), e.naic && "NAIC " + esc(e.naic),
          e.tin && "TIN " + esc(e.tin), e.fein && "FEIN " + esc(e.fein)].filter(Boolean).join(" · ") || "No identity keys on file"}
          · since ${fmtDate(e.created_at)}</p>`;

      // Composite risk score (#13) — first thing a reviewer sees.
      const rk = d.risk || {};
      if (rk.score != null) {
        const bandClr = rk.band === "review" ? "bad" : rk.band === "watch" ? "warn" : "ok";
        html += `<h2>Risk</h2><div class="cards">
          <div class="card"><div class="num ${bandClr}">${rk.score}</div><div class="lbl">Composite risk (${esc(rk.band)})</div></div>` +
          (rk.factors || []).map((x) => `<div class="card"><div class="num">${x.points}</div><div class="lbl">${esc((x.factor || "").replace(/_/g, " "))}${x.count != null ? " (" + x.count + ")" : ""}${x.days != null ? " (" + x.days + "d)" : ""}</div></div>`).join("") + `</div>`;
      }

      // Relationship graph (#10): entities this one has faced in disputes —
      // ego network, edges weighted by shared cases. SVG radial, no library.
      let g = { nodes: [], edges: [] };
      try { g = await Api.crm.entityGraph(id); } catch (e2) { /* graph panel optional */ }
      // Normalize to seed-relative edges (a REPRESENTS edge may point either
      // way) and keep only edges touching this entity for the radial.
      g.edges = (g.edges || []).filter((x) => x.from === id || x.to === id)
        .map((x) => ({ ...x, other: x.from === id ? x.to : x.from }));
      if ((g.nodes || []).length > 1) {
        const cx = 340, cy = 190, R = 140;
        const maxW = Math.max(...g.edges.map((x) => x.cases || 0), 1);
        const seed = g.nodes.find((n) => n.seed);
        const ring = g.nodes.filter((n) => !n.seed && g.edges.some((x) => x.other === n.id));
        const pos = {};
        ring.forEach((n, i) => {
          const a = (2 * Math.PI * i) / ring.length - Math.PI / 2;
          pos[n.id] = [cx + R * Math.cos(a), cy + R * Math.sin(a)];
        });
        html += `<h2>Relationship graph — who it faces in disputes (${ring.length})${g.backend && g.backend !== "sql" ? ` <span class="muted" style="font-size:.75em">via ${esc(g.backend)}</span>` : ""}</h2>
          <svg viewBox="0 0 680 380" style="width:100%;max-width:680px;display:block" role="img" aria-label="Entity relationship graph">` +
          g.edges.map((x) => { const [x1, y1] = pos[x.other] || [cx, cy];
            const rep = x.kind === "REPRESENTS";
            return `<line x1="${cx}" y1="${cy}" x2="${x1}" y2="${y1}" stroke="var(--line,#888)"
              stroke-width="${rep ? 1.5 : 1 + 3 * (x.cases || 0) / maxW}" opacity="0.55"${rep ? ` stroke-dasharray="5 4"` : ""}/>`; }).join("") +
          ring.map((n) => { const [nx, ny] = pos[n.id];
            const lbl = String(n.name || "").length > 22 ? String(n.name).slice(0, 21) + "…" : String(n.name || "");
            return `<g style="cursor:pointer" onclick="location.hash='#/crm/360/${n.id}'">
              <circle cx="${nx}" cy="${ny}" r="10" fill="var(--accent,#4a7)"/><title>${esc(n.name)} — ${esc(n.type)}</title>
              <text x="${nx}" y="${ny + 22}" text-anchor="middle" font-size="10" fill="currentColor">${esc(lbl)}</text></g>`; }).join("") +
          `<circle cx="${cx}" cy="${cy}" r="16" fill="var(--brass,#a83)"/><title>${esc(seed?.name || "")}</title>
           <text x="${cx}" y="${cy + 32}" text-anchor="middle" font-size="11" font-weight="600" fill="currentColor">${esc(String(seed?.name || "").slice(0, 26))}</text></svg>` +
          `<table><thead><tr><th>Related organization</th><th>Relationship</th><th>Type</th><th>Shared disputes</th></tr></thead><tbody>` +
          g.edges.slice(0, 15).map((x) => { const n = g.nodes.find((y) => y.id === x.other);
            return `<tr class="click" onclick="location.hash='#/crm/360/${x.other}'"><td>${esc(n?.name || "")}</td>
              <td>${x.kind === "REPRESENTS" ? "org chart" : "opposes in disputes"}</td>
              <td>${badge(n?.type || "")}</td><td class="num">${x.cases ?? "—"}</td></tr>`; }).join("") + `</tbody></table>`;
      }

      // Panel 1: where the platform knows this organization (#3 deep links).
      html += `<h2>Identity &amp; onboarding — where this organization appears (${d.links.length})</h2>` +
        (d.links.length ? `<table><thead><tr><th>Record</th><th>Name</th><th>Kind</th><th>Status</th><th>Link</th><th></th></tr></thead><tbody>` +
          d.links.map((l) => `<tr class="click" onclick="location.hash='${srcLink(l)}'"><td>${esc(l.source_label)}</td>
            <td>${esc(l.detail?.name || l.source_id)}</td><td>${esc(l.detail?.kind || "")}</td>
            <td>${l.detail?.status ? badge(l.detail.status) : "—"}${l.detail?.npi_status && l.detail.npi_status !== "VERIFIED" ? " " + badge(l.detail.npi_status) : ""}</td>
            <td>${l.link_kind === "CONFIRMED" ? `confirmed by ${esc(l.linked_by)}` : "identity-key match"}</td>
            <td><button class="mini danger" onclick="event.stopPropagation();CrmViews.entityUnlinkFn('${id}',${l.link_id})" title="Remove this link">Unlink</button></td></tr>`).join("") +
          `</tbody></table>`
          : `<p class="muted">No linked records yet. Entities resolve automatically when a record carries an identity key (NPI / TIN / NAIC / FEIN) —
             for anything else, link it yourself:</p>
             <div class="actions"><button onclick="CrmViews.entityLinkPicker('${id}')">Link a record…</button></div>`);

      // Panel 2: pipeline — initiator vs respondent split (#11) + ordered funnel (#7).
      const pl = d.pipeline || {};
      html += `<h2>Pipeline</h2><div class="cards">
        <div class="card"><div class="num">${pl.total ?? 0}</div><div class="lbl">Disputes</div></div>
        <div class="card"><div class="num">${pl.initiating ?? 0}</div><div class="lbl">Filed by this entity</div></div>
        <div class="card"><div class="num">${pl.responding ?? 0}</div><div class="lbl">Answered as respondent</div></div>
        <div class="card"><div class="num">${pl.open ?? 0}</div><div class="lbl">Open</div></div>
        <div class="card"><div class="num">${pl.closed ?? 0}</div><div class="lbl">Closed</div></div></div>` +
        ((pl.funnel || []).length ? `<h3>Funnel</h3><table><thead><tr><th>Stage</th><th>Now at stage</th><th>Reached stage</th><th>Conversion</th></tr></thead><tbody>` +
          pl.funnel.map((s) => `<tr><td>${badge(s.stage)}</td><td class="num">${s.count}</td><td class="num">${s.reached}</td>
            <td class="num">${s.conversion_pct != null ? Number(s.conversion_pct).toFixed(1) + "%" : "—"}</td></tr>`).join("") + `</tbody></table>` : "") +
        ((pl.by_status || []).length ? `<table><thead><tr><th>Status</th><th>Disputes</th></tr></thead><tbody>` +
          pl.by_status.map((s) => `<tr><td>${badge(s.status)}</td><td class="num">${s.count}</td></tr>`).join("") + `</tbody></table>` : "") +
        ((pl.recent || []).length ? `<h3>Most recent</h3><table><thead><tr><th>Case</th><th>Status</th><th>Service line</th><th>Opened</th></tr></thead><tbody>` +
          pl.recent.map((c) => `<tr class="click" onclick="location.hash='#/cases/${c.id}'"><td class="mono">${esc(c.case_number)}</td>
            <td>${badge(c.status)}</td><td>${esc(c.service_line)}</td><td>${fmtDate(c.opened_at)}</td></tr>`).join("") + `</tbody></table>` : "");

      // Panel 3: financials + aging buckets (#8).
      const f = d.financials || {};
      const ag = f.aging || {};
      html += `<h2>Financials</h2><div class="cards">
        <div class="card"><div class="num">${money(f.fee_invoiced_cents)}</div><div class="lbl">Dispute fees invoiced (${f.fee_invoices ?? 0})</div></div>
        <div class="card"><div class="num">${money(f.fee_collected_cents)}</div><div class="lbl">Fees collected</div></div>
        <div class="card"><div class="num">${money(f.fee_open_cents)}</div><div class="lbl">Fees open</div></div>
        <div class="card"><div class="num">${money(f.paid_cents)}</div><div class="lbl">Card/ACH payments (${f.payments ?? 0})</div></div>
        ${f.billing_invoices != null ? `<div class="card"><div class="num">${money(f.billing_collected_cents)}</div><div class="lbl">Service billing collected (${f.billing_invoices})</div></div>
        <div class="card"><div class="num">${money(f.billing_outstanding_cents)}</div><div class="lbl">Service billing outstanding</div></div>` : ""}
        ${f.checkout_checkouts != null ? `<div class="card"><div class="num">${money(f.checkout_paid_cents)}</div><div class="lbl">Fee checkouts paid (${f.checkout_checkouts})</div></div>` : ""}</div>` +
        (f.aging ? `<h3>Open fee aging${ag.oldest_open_days ? ` — oldest ${ag.oldest_open_days} days` : ""}</h3><div class="cards">
          <div class="card"><div class="num">${money(ag.bucket_0_30)}</div><div class="lbl">0–30 days</div></div>
          <div class="card"><div class="num">${money(ag.bucket_31_60)}</div><div class="lbl">31–60 days</div></div>
          <div class="card"><div class="num">${money(ag.bucket_61_90)}</div><div class="lbl">61–90 days</div></div>
          <div class="card"><div class="num">${money(ag.bucket_90_plus)}</div><div class="lbl">90+ days</div></div></div>` : "");

      // Panel 4: performance + tenant-wide benchmark (#9).
      const pf = d.performance || {};
      const bm = pf.benchmark || {};
      html += `<h2>Performance</h2><div class="cards">
        <div class="card"><div class="num">${pf.closed ?? 0}</div><div class="lbl">Disputes closed</div></div>
        <div class="card"><div class="num">${pf.avg_days_to_close ?? "—"}</div><div class="lbl">Avg days to close</div></div>
        <div class="card"><div class="num">${pf.determinations ?? 0}</div><div class="lbl">Determinations</div></div>
        <div class="card"><div class="num">${money(pf.avg_award_cents)}</div><div class="lbl">Avg award</div></div>
        <div class="card"><div class="num">${pf.above_qpa ?? 0}</div><div class="lbl">Awards above QPA</div></div>
        <div class="card"><div class="num">${pf.sla_breaches ?? 0}</div><div class="lbl">SLA breaches</div></div></div>` +
        (Object.keys(bm).length ? `<h3>Against the whole program</h3><div class="cards">
          ${bm.tenant_avg_days_to_close != null ? `<div class="card"><div class="num">${bm.tenant_avg_days_to_close}</div><div class="lbl">Program avg days to close</div></div>` : ""}
          ${bm.tenant_median_days_to_close != null ? `<div class="card"><div class="num">${bm.tenant_median_days_to_close}</div><div class="lbl">Program median days to close</div></div>` : ""}
          ${bm.faster_than_pct != null ? `<div class="card"><div class="num">${bm.faster_than_pct}%</div><div class="lbl">Of program cases closed slower than this entity's average</div></div>` : ""}
          ${bm.entity_above_qpa_pct != null ? `<div class="card"><div class="num">${Number(bm.entity_above_qpa_pct).toFixed(1)}%</div><div class="lbl">Entity awards above QPA</div></div>` : ""}
          ${bm.tenant_above_qpa_pct != null ? `<div class="card"><div class="num">${bm.tenant_above_qpa_pct}%</div><div class="lbl">Program awards above QPA</div></div>` : ""}</div>` : "");

      // Panel 5: compliance & risk.
      const cp = d.compliance || {};
      const dv = cp.doc_verdicts || {};
      html += `<h2>Compliance</h2>` +
        (Object.keys(dv).length ? `<div class="cards">` + Object.entries(dv).map(([st, n]) =>
          `<div class="card"><div class="num">${n}</div><div class="lbl">Package documents ${esc(st.toLowerCase().replace(/_/g, " "))}</div></div>`).join("") + `</div>`
          : `<p class="muted">No onboarding package documents on the linked records.</p>`) +
        ((cp.npi_flags || []).length ? `<h3>NPI registry flags</h3><table><thead><tr><th>Record</th><th>Name</th><th>Status</th><th>Registry name</th></tr></thead><tbody>` +
          cp.npi_flags.map((x) => `<tr><td>${esc(x.source)}</td><td>${esc(x.name)}</td><td>${badge(x.npi_status)}</td><td>${esc(x.npi_registry_name || "—")}</td></tr>`).join("") + `</tbody></table>` : "") +
        ((cp.breach_clocks || []).length ? `<h3>SLA breaches by clock</h3><table><thead><tr><th>Clock</th><th>Breaches</th></tr></thead><tbody>` +
          cp.breach_clocks.map((x) => `<tr><td class="mono">${esc(x.clock)}</td><td class="num">${x.count}</td></tr>`).join("") + `</tbody></table>` : "");

      // Panel 6: unified engagement timeline (#12), legacy sections retained below it.
      const en = d.engagement || {};
      html += `<h2>Engagement</h2>` +
        ((en.timeline || []).length ? `<table><thead><tr><th>When</th><th>Kind</th><th>What</th><th>Case</th></tr></thead><tbody>` +
          en.timeline.map((x) => `<tr${x.case_id ? ` class="click" onclick="location.hash='#/cases/${x.case_id}'"` : ""}>
            <td class="mono">${fmtDate(x.at)}</td><td>${badge(x.kind)}</td><td>${esc(x.summary || "")}</td>
            <td class="mono">${x.case_id ? esc(String(x.case_id).slice(0, 8)) : "—"}</td></tr>`).join("") + `</tbody></table>`
          : `<p class="muted">No engagement recorded yet.</p>`) +
        ((en.notes || []).length ? `<h3>Notes</h3><table><tbody>` + en.notes.map((n) =>
          `<tr><td>${badge(n.stream)}</td><td>${esc((n.body || "").slice(0, 160))}</td><td>${esc(n.author)}</td><td class="mono">${fmtDate(n.created_at)}</td></tr>`).join("") + `</tbody></table>` : "");
      return html;
    } catch (e) { return `<h1>Entity 360</h1>` + err(e); }
  }

  // entityLinkPicker (#4): staff-driven linking — pick any source record not
  // yet on an entity and attach it here (CONFIRMED by the caller).
  async function entityLinkPicker(entityId) {
    try {
      const v = await UI.modal({
        title: "Link a record to this entity",
        body: "Search every rail — CRM accounts, TPAs, TPA clients, health plans, health plan parties. Records already linked elsewhere are marked.",
        submitLabel: "Link",
        fields: [{ name: "q", label: "Search", placeholder: "Name, NPI, TIN or NAIC…", required: true },
                 { name: "pick", label: "Record", type: "select", options: [["", "— search first —"]] }],
        onRefresh: async (vals, setOptions) => {
          if (!vals.q || vals.q.length < 2) return;
          const r = await Api.crm.entitySourceSearch(vals.q);
          setOptions("pick", [["", "— pick a record —"]].concat((r.sources || []).map((x) =>
            [`${x.source_table}:${x.source_id}`, `${x.name} — ${x.source_label}${x.linked_to ? " (already linked)" : ""}`])));
        },
      });
      if (!v || !v.pick) return;
      const [source_table, source_id] = v.pick.split(":");
      await Api.crm.entityLink(entityId, { source_table, source_id });
      UI.toast("Linked");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // entityMergeModal (#1): fold a duplicate entity into this one — its links,
  // CRM account, and identity keys move over; the duplicate is tombstoned.
  async function entityMergeModal(entityId) {
    try {
      const v = await UI.modal({
        title: "Merge another entity into this one",
        body: "The chosen entity's linked records and identity keys move to this entity, and the chosen entity is retired. This cannot be undone from the portal.",
        submitLabel: "Merge",
        danger: true,
        fields: [{ name: "q", label: "Find the duplicate", placeholder: "Name, NPI or NAIC…", required: true },
                 { name: "into", label: "Duplicate to retire", type: "select", options: [["", "— search first —"]] }],
        onRefresh: async (vals, setOptions) => {
          if (!vals.q || vals.q.length < 2) return;
          const r = await Api.crm.entitySearch(vals.q);
          setOptions("into", [["", "— pick the duplicate —"]].concat((r.entities || []).filter((x) => x.id !== entityId).map((x) =>
            [x.id, `${x.canonical_name} — ${x.entity_type}${x.npi ? " · NPI " + x.npi : ""} (${x.links} linked)`])));
        },
      });
      if (!v || !v.into) return;
      await Api.crm.entityMerge(v.into, entityId);
      UI.toast("Merged — the duplicate is retired");
      App.rerender();
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function entityUnlinkFn(entityId, linkId) {
    if (!(await UI.confirm("Unlink this record?", "The source record keeps its data; it just stops counting toward this entity's 360.", "Unlink", true))) return;
    try { await Api.crm.entityUnlink(entityId, linkId); UI.toast("Unlinked"); App.rerender(); }
    catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  return { pipeline, pipelinePage, pipelineAssigned, pipelineScroll, pipelineJump, accounts, accountsFilter, account360, accountNew, leads, leadsFilter, convert, tasks, tasksFilter, tasksFilterFor, done, search, calendar, calTaskDone, calTaskAdd,
    entity360, entitySearchPage, openEntity360, entityUnlinkFn, entityLinkPicker, entityMergeModal, entityOutreachLead };
})();
