// crm-views.js — CRM screens: pipeline kanban, accounts 360, leads, tasks, search.
const CrmViews = (() => {
  // See views.js: bind after router innerHTML injection (macrotask, not microtask).
  const afterRender = (fn) => setTimeout(fn, 0);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmtDate = (d) => (d ? new Date(d).toLocaleDateString("en-US", { dateStyle: "medium" }) : "—");
  const badge = (s) => `<span class="badge s-${esc(s).toLowerCase().replace(/_/g, "-")}">${esc(s)}</span>`;
  const err = (e) => `<p class="error">${esc(e.message)}</p>`;
  const CRM_PAGE = 100;
  // Offset pager shared by accounts/leads/tasks: footer markup + a binder that
  // appends the next page's rows in place (busy state on the button).
  const pagerHtml = (id, shown, total, nextOffset) =>
    `<p class="pager" id="${id}-pg"><span class="muted">Showing ${shown} of ${total}</span>
      ${nextOffset >= 0 ? `<button class="mini" id="${id}-more">Load more (${Math.min(CRM_PAGE, total - shown)} remaining)</button>` : ""}</p>`;
  function bindPager(id, tbodySel, state, fetchPage, rowsHtml) {
    document.getElementById(id + "-more")?.addEventListener("click", async (ev) => {
      await UI.run(ev.currentTarget, async () => {
        try {
          const r = await fetchPage(state.next);
          state.loaded = state.loaded.concat(r.rows);
          state.next = r.next;
          document.querySelector(tbodySel).insertAdjacentHTML("beforeend", rowsHtml(r.rows));
          document.getElementById(id + "-pg").outerHTML = pagerHtml(id, state.loaded.length, state.total, state.next);
          bindPager(id, tbodySel, state, fetchPage, rowsHtml);
        } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      }, "Loading…");
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
    ["Decided - Invoice Paid", "Decided — invoice paid"],
    ["Plan Opt-Out", "Plan opt-out"], ["Ineligible", "Ineligible"],
    ["Dismissed", "Dismissed"], ["Withdrawn", "Withdrawn"],
    ["Plan Notification Packet Issued", "Plan notified"],
  ];
  // Stages the board treats as "done" -- de-emphasized column styling, and
  // excluded from the default empty-stage hiding logic's "likely to fill up
  // soon" assumption (a closed stage being empty is normal, not a gap).
  const TERMINAL_STAGES = new Set([
    "CLOSED_PAID", "CLOSED_DISMISSED",
    "Decided - Invoice Paid", "Plan Opt-Out", "Ineligible", "Dismissed",
    "Withdrawn", "Provider Closure Letter Issued", "Provider - Withdrawal",
  ]);
  const KANBAN_CARD_CAP = 8;
  async function pipeline() {
    try {
      const [{ cases }, prog] = await Promise.all([Api.cases.list({ limit: 200 }), Api.program.get().catch(() => null)]);
      // Programmed tenants (e.g. FL AHCA) never populate the federal `status`
      // column -- they track real progress via internal_status/agency_status
      // instead (confirmed in programops.go: dual-status writers only ever
      // SET internal_status, never status), so a PIPELINE keyed on `status`
      // -- even one with FL's own vocabulary hardcoded in -- still shows
      // every column empty for them. Confirmed live. Fall back to the
      // federal grouping when no program config exists for this tenant.
      const internalStatuses = prog?.config?.statuses?.internal;
      const byField = internalStatuses?.length ? "internal_status" : "status";
      const knownStages = internalStatuses?.length ? internalStatuses.map((s) => [s, s]) : PIPELINE;
      // Safety net: a status the board doesn't know yet still gets a column
      // instead of its cases disappearing silently.
      const known = new Set(knownStages.map(([s]) => s));
      const withStatus = cases.filter((c) => c[byField]);
      const noStatus = cases.filter((c) => !c[byField]);
      const extra = [...new Set(withStatus.filter((c) => !known.has(c[byField])).map((c) => c[byField]))];
      // Cases with NO status set yet (e.g. a freshly-opened PENDING_INTAKE
      // case, before the first eligibility/status call) matched nothing in
      // `known` AND got filtered out of `extra` by its own Boolean check --
      // they vanished from the board with no column to land in at all.
      // Confirmed live: ~1 in 6 FL cases had no internal_status and were
      // simply invisible here. Give them an explicit leading column instead.
      const stages = [
        ...(noStatus.length ? [["__NONE__", "Not yet reviewed"]] : []),
        ...knownStages,
        ...extra.map((s) => [s, s.replace(/_/g, " ").toLowerCase()]),
      ];
      const amtField = (prog && prog.config) ? "disputed_amount_cents" : "qpa_cents";
      const amtLabel = (prog && prog.config) ? "Disputed" : "QPA";
      const usd = (cents) => "$" + ((cents || 0) / 100).toLocaleString();
      const cols = stages.map(([status, label]) => {
        const items = status === "__NONE__" ? noStatus : withStatus.filter((c) => c[byField] === status);
        const terminal = TERMINAL_STAGES.has(status);
        const shown = items.slice(0, KANBAN_CARD_CAP);
        const rest = items.length - shown.length;
        return `<div class="kanban-col ${terminal ? "is-terminal" : ""} ${items.length ? "" : "is-empty"}">
          <h3>${esc(label)} <span class="kanban-count">${items.length}</span></h3>` +
          shown.map((c) => `<div class="kanban-card" onclick="location.hash='#/cases/${c.id}'">
            <b>${esc(c.case_number)}</b>
            <span class="muted">${esc(c.service_line || "—")} · ${usd(c[amtField])}</span></div>`).join("") +
          (rest > 0 ? `<div class="kanban-more">+${rest} more</div>` : "") +
          `</div>`;
      });
      const totalShown = withStatus.length + noStatus.length;
      afterRender(() => $("#pipe-show-empty")?.addEventListener("change", (ev) =>
        $("#pipe-board")?.classList.toggle("hide-empty", !ev.target.checked)));
      return `<div class="view-head"><h1>Pipeline</h1>
          <span class="muted">${totalShown} dispute${totalShown === 1 ? "" : "s"} across ${stages.length} stage${stages.length === 1 ? "" : "s"}
            · amounts shown as ${esc(amtLabel)}</span></div>
        <label class="pipe-toggle"><input type="checkbox" id="pipe-show-empty" /> Show empty stages</label>
        <div class="kanban hide-empty" id="pipe-board">${cols.join("")}</div>`;
    } catch (e) { return `<h1>Pipeline</h1>` + err(e); }
  }

  // ---- Accounts ---------------------------------------------------------------
  async function accounts() {
    try {
      const page = await Api.crm.accounts(null, { limit: CRM_PAGE });
      const state = { loaded: page.accounts.slice(), next: page.next_offset, total: page.total };
      const rowsHtml = (list) => list.map((a) => `<tr class="click" onclick="location.hash='#/crm/accounts/${a.id}'">
            <td>${esc(a.legal_name)}</td><td>${badge(a.type)}</td><td>${esc(a.npi)}</td><td>${esc(a.phone)}</td></tr>`).join("");
      if (!state.loaded.length)
        return `<h1>Accounts</h1><p><a class="button" href="#/crm/accounts/new">New account</a></p>
          <p class="muted">No accounts yet — convert leads or create one.</p>`;
      afterRender(() => bindPager("acc", "#acc-tb tbody", state,
        (off) => Api.crm.accounts(null, { limit: CRM_PAGE, offset: off }).then((r) => ({ rows: r.accounts, next: r.next_offset })),
        rowsHtml));
      return `<h1>Accounts</h1><p><a class="button" href="#/crm/accounts/new">New account</a></p>
        <table id="acc-tb"><thead><tr><th>Legal name</th><th>Type</th><th>NPI</th><th>Phone</th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("acc", state.loaded.length, state.total, state.next);
    } catch (e) { return `<h1>Accounts</h1>` + err(e); }
  }

  async function account360(id) {
    try {
      const d = await Api.crm.account360(id);
      const a = d.account;
      Palette.remember("account", a.id, a.legal_name);
      let html = `<h1>${esc(a.legal_name)}</h1><p>${badge(a.type)} · NPI ${esc(a.npi) || "—"} · ${esc(a.phone) || "—"}</p>`;
      if (d.health)
        html += `<div class="cards"><div class="card health-${esc(d.health.band)}">
          <div class="num">${d.health.score}</div><div class="lbl">Relationship health — ${esc(d.health.band)}</div>
          <div class="muted">${d.health.open_disputes} open disputes · ${d.health.sla_breaches} SLA breaches · ${esc(d.health.formula)}</div></div></div>`;
      html += `<h2>Contacts (${d.contacts.length})</h2>` +
        (d.contacts.length ? `<table><tbody>` + d.contacts.map((c) =>
          `<tr><td>${esc(c.name)}</td><td>${esc(c.role_title)}</td><td>${esc(c.email)}</td><td>${esc(c.phone)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No contacts.</p>`) +
        `<form id="nc" class="form"><b>Add contact</b>
           <input name="name" placeholder="Name" required />
           <input name="role_title" placeholder="Role (e.g. Billing lead)" />
           <input name="email" type="email" placeholder="Email" />
           <input name="phone" placeholder="Phone" /><button>Add</button></form>`;
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
  async function leads() {
    try {
      const page = await Api.crm.leads({ limit: CRM_PAGE });
      const state = { loaded: page.leads.slice(), next: page.next_offset, total: page.total };
      const rowsHtml = (list) => list.map((l) => `<tr><td>${esc(l.name)}</td><td>${esc(l.organization)}</td><td>${badge(l.source)}</td>
            <td>${esc((l.summary || "").slice(0, 80))}</td><td>${badge(l.status)}</td>
            <td>${l.status !== "CONVERTED" ? `<button onclick="CrmViews.convert('${l.id}')">Convert</button>` : ""}</td></tr>`).join("");
      if (!state.loaded.length)
        return `<h1>Leads</h1><p class="muted">Voice intake becomes a lead automatically; convert qualified leads into accounts.</p><p class="muted">No leads.</p>`;
      afterRender(() => bindPager("lead", "#lead-tb tbody", state,
        (off) => Api.crm.leads({ limit: CRM_PAGE, offset: off }).then((r) => ({ rows: r.leads, next: r.next_offset })),
        rowsHtml));
      return `<h1>Leads</h1><p class="muted">Voice intake becomes a lead automatically; convert qualified leads into accounts.</p>
        <table id="lead-tb"><thead><tr><th>Name</th><th>Organization</th><th>Source</th><th>Summary</th><th>Status</th><th></th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("lead", state.loaded.length, state.total, state.next);
    } catch (e) { return `<h1>Leads</h1>` + err(e); }
  }

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
  async function tasks() {
    const mine = new URLSearchParams(location.hash.split("?")[1] || "").get("all") !== "1";
    const title = mine ? "My tasks" : "All tasks";
    try {
      const page = await Api.crm.tasks(mine, { limit: CRM_PAGE });
      const state = { loaded: page.tasks.slice(), next: page.next_offset, total: page.total };
      let html = `<div class="view-head"><h1>${title}</h1></div>
        <div class="tabs">
          <a class="tab ${mine ? "active" : ""}" href="#/crm/tasks">My tasks</a>
          <a class="tab ${!mine ? "active" : ""}" href="#/crm/tasks?all=1">All tasks</a>
        </div>
        <form id="nt" class="form"><b>New task</b>
          <input name="subject" placeholder="Subject" required />
          <input name="case_id" placeholder="Case ID (optional)" />
          <input name="due_date" type="date" /><button>Create</button></form>`;
      const rowsHtml = (list) => list.map((t) => `<tr><td class="mono">${esc(t.task_ref || "—")}</td>
          <td>${esc(t.subject)}</td>
          ${mine ? "" : `<td>${esc(t.assignee || "—")}</td>`}
          <td>${t.case_id ? `<a href="#/cases/${esc(t.case_id)}" class="mono">${esc(t.case_id.slice(0, 8))}…</a>` : "—"}</td>
          <td>${esc(t.due_date)}</td>
          <td>${badge(t.status)}</td>
          <td>${t.status === "OPEN" ? `<button onclick="CrmViews.done('${t.id}')">Done</button>` : ""}</td></tr>`).join("");
      html += state.loaded.length ? `<table id="task-tb"><thead><tr><th>Ref</th><th>Subject</th>
          ${mine ? "" : "<th>Assignee</th>"}<th>Case</th><th>Due</th><th>Status</th><th></th></tr></thead>
        <tbody>${rowsHtml(state.loaded)}</tbody></table>` + pagerHtml("task", state.loaded.length, state.total, state.next)
        : `<p class="muted">${mine ? "No tasks assigned to you." : "No tasks for this tenant."}</p>`;
      afterRender(() => bindPager("task", "#task-tb tbody", state,
        (off) => Api.crm.tasks(mine, { limit: CRM_PAGE, offset: off }).then((r) => ({ rows: r.tasks, next: r.next_offset })),
        rowsHtml));
      afterRender(() => document.querySelector("#nt")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = Object.fromEntries(new FormData(ev.target));
        try { const r = await Api.crm.createTask(f); UI.toast(`Task ${r.task_ref || ""} created`.trim()); App.rerender(); }
        catch (e) { UI.toast(e.message, { kind: "warn" }); }
      }));
      return html;
    } catch (e) { return `<h1>${title}</h1>` + err(e); }
  }

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
        : h.kind === "account" ? `#/crm/accounts/${h.id}` : h.kind === "lead" ? "#/crm/leads" : "#/crm/accounts";
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
    const chipCls = (k) => k === "OFFER_WINDOW" ? "cal-chip-stat" : "cal-chip-task";
    let cells = "";
    for (let i = 0; i < first.getDay(); i++) cells += `<div class="cal-cell empty"></div>`;
    for (let d = 1; d <= daysInMonth; d++) {
      const iso = `${y}-${String(m + 1).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
      const dayItems = byDay[iso] || [];
      cells += `<div class="cal-cell ${iso === todayISO ? "today" : ""} ${dayItems.length ? "has-items" : ""}">
        <span class="cal-num">${d}</span>
        ${dayItems.slice(0, 3).map((i) => `<div class="cal-chip ${chipCls(i.kind)}"
            title="${esc(i.kind)} — ${esc(i.label)}">${i.case_id
              ? `<a href="#/cases/${esc(i.case_id)}">${esc(i.label)}</a>`
              : esc(i.label)}</div>`).join("")}
        ${dayItems.length > 3 ? `<span class="muted cal-more">+${dayItems.length - 3} more</span>` : ""}</div>`;
    }
    return `<div class="cal-grid">
      ${["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"].map((d) => `<div class="cal-dow">${d}</div>`).join("")}
      ${cells}</div>`;
  }

  async function calendar() {
    try {
      const items = await Api.cm.calendar();
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
        const cls = d === today ? "is-today" : d < today ? "is-overdue" : "";
        return `<div class="cal-group ${cls}">
          <div class="cal-date"><span class="dow">${dow(d)}</span><span class="dom">${dom(d)}</span></div>
          <div class="cal-col">
            <div class="cal-heading">${heading(d)} <span class="muted">(${groups[d].length})</span></div>
            <div class="cal-rows">${groups[d].map((i) => {
              const linkable = !!i.case_id;
              return `<div class="cal-item ${linkable ? "link" : ""}" ${linkable ? `onclick="location.hash='#/cases/${esc(i.case_id)}'"` : ""}>
                <span class="cal-kind ${i.kind === "OFFER_WINDOW" ? "k-offer" : "k-task"}" title="${esc(i.kind)}">${i.kind === "OFFER_WINDOW" ? "⏱" : "✓"}</span>
                <span class="cal-label" title="${esc(i.label)}">${esc(i.label)}</span>
                ${i.kind === "TASK" ? `<button class="mini" onclick="event.stopPropagation(); CrmViews.done('${i.ref}')">Done</button>` : ""}
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
      return `<div class="view-head"><h1>Calendar</h1>
        <span class="muted">statutory clocks, offer windows, and task due dates</span></div>
        <div class="cal-nav">
          <button class="mini" id="cal-prev">‹ prev</button>
          <b id="cal-month-label"></b>
          <button class="mini" id="cal-next">next ›</button>
          <span class="cal-legend"><span class="cal-chip cal-chip-stat">statutory / offer window</span>
            <span class="cal-chip cal-chip-task">task</span></span></div>
        <div id="cal-grid-box"></div>
        <h2>Agenda</h2>` + (agenda || `<p class="muted">No upcoming deadlines or tasks.</p>`);
    } catch (e) { return `<h1>Calendar</h1>` + err(e); }
  }

  return { pipeline, accounts, account360, accountNew, leads, convert, tasks, done, search, calendar };
})();
