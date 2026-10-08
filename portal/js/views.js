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
  const can = (...rs) => rs.some(role);
  // Federal NSA cases price on QPA (qpa_cents); programmed tenants (FL AHCA
  // etc.) have no such concept and always read $0 there -- they use
  // disputed_amount_cents instead (accumulates from imported claims, or a
  // one-time figure set when a case is opened via the generic New Dispute
  // form). Every QPA-labeled amount in the grid/dashboard was unconditional
  // before this -- confirmed live, FL showed "QPA $0" on every open case.
  const amtField = (prog) => (prog && prog.config) ? "disputed_amount_cents" : "qpa_cents";
  const amtLabel = (prog) => (prog && prog.config) ? "Disputed" : "QPA";

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
  // For lists the backend already returns in one shot (bounded by a server
  // LIMIT, e.g. 200) that were previously either dumped into one long table
  // or silently truncated with .slice(0, N) -- slices into pages in the
  // browser. cases()/CRM accounts/leads/tasks already have real backend
  // offset/keyset "Load more" pagination against their own endpoints; this
  // is for the rest (intake, audit log, QA queue, onboarding, financials)
  // where the backend has no page param to call. Only one view is ever
  // mounted at a time in this SPA, so state keyed by a short id string is
  // safe -- a stale entry from the previous view is just unused until
  // overwritten on next visit.
  const CLIENT_PAGE = 25;
  const pagerState = {};
  function clientPagerHtml(id, total, page) {
    if (total <= CLIENT_PAGE) return "";
    const totalPages = Math.max(1, Math.ceil(total / CLIENT_PAGE));
    return `<p class="pager" id="${id}-pg">
      <span class="muted">Page ${page + 1} of ${totalPages} (${total} total)</span>
      <button class="mini" id="${id}-prev" ${page === 0 ? "disabled" : ""}>‹ Prev</button>
      <button class="mini" id="${id}-next" ${page >= totalPages - 1 ? "disabled" : ""}>Next ›</button></p>`;
  }
  // initClientPager: call during render to get the first page's rows + pager
  // markup. bindClientPager: call in afterRender to wire Prev/Next.
  function initClientPager(id, allRows, rowsHtml) {
    pagerState[id] = { rows: allRows, page: 0, rowsHtml };
    return { bodyHtml: rowsHtml(allRows.slice(0, CLIENT_PAGE)), pagerHtml: clientPagerHtml(id, allRows.length, 0) };
  }
  function renderClientPage(id, tbodySel, page, onRendered) {
    const st = pagerState[id];
    const totalPages = Math.max(1, Math.ceil(st.rows.length / CLIENT_PAGE));
    st.page = Math.max(0, Math.min(page, totalPages - 1));
    document.querySelector(tbodySel).innerHTML = st.rowsHtml(st.rows.slice(st.page * CLIENT_PAGE, (st.page + 1) * CLIENT_PAGE));
    const pg = document.getElementById(`${id}-pg`);
    const html = clientPagerHtml(id, st.rows.length, st.page);
    if (pg) html ? (pg.outerHTML = html) : pg.remove();
    bindClientPager(id, tbodySel, onRendered);
    onRendered?.();
  }
  // onRendered: optional hook re-run after every page swap, for views that
  // need more than a tbody-innerHTML replace (e.g. dashboard's caseTable()
  // re-render needs bindGrid() re-wired for the newly-rendered rows' row
  // clicks/checkboxes/peek buttons, not just the HTML itself).
  function bindClientPager(id, tbodySel, onRendered) {
    document.getElementById(`${id}-prev`)?.addEventListener("click", () => renderClientPage(id, tbodySel, pagerState[id].page - 1, onRendered));
    document.getElementById(`${id}-next`)?.addEventListener("click", () => renderClientPage(id, tbodySel, pagerState[id].page + 1, onRendered));
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
    const me = Auth.claims();
    let html = `<div class="view-head"><h1>Good day, ${esc((me.name || "").split(/[.\s]/)[0] || me.name)}</h1>
      <span class="muted">tenant <b>${esc(Api.getTenant()).toUpperCase()}</b> · ${me.roles.map(esc).join(", ")}</span></div>`;
    try {
      const [{ cases }, clocks, prog] = await Promise.all([Api.cases.list({ limit: 200 }), clockMap(), Api.program.get().catch(() => null)]);
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
        const b = byStatus[c.status] || (byStatus[c.status] = { status: c.status, count: 0, _sum: 0 });
        b.count++; b._sum += (c[amtF] || 0) / 100;
      });
      const summary = Object.values(byStatus).map((b) => ({
        status: b.status, count: b.count, avg_amount_usd: b.count ? b._sum / b.count : 0, amount_label: amtL,
      }));
      html += `<div class="cards">` + summary.map((s) =>
        `<div class="card"><div class="num">${s.count}</div><div class="lbl">${badge(s.status)}</div>
         <div class="muted">avg ${esc(s.amount_label || "QPA")} $${(s.avg_amount_usd ?? 0).toFixed(0)}</div></div>`).join("") + `</div>`;
      const open = cases.filter((c) => !String(c.status).startsWith("CLOSED"));
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
  // Server-side keyset pagination: page size 50, "Load more" appends the next
  // page; saved-view status filters are pushed to the server so a filter never
  // applies to a partial page. Counts come from the server total, not the page.
  const PAGE_SIZE = 50;
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
      const reqParams = { limit: PAGE_SIZE, ...(status ? { status } : {}), ...(sort ? { sort } : {}), ...(lane ? { lane } : {}) };
      const [page, clocks, prog] = await Promise.all([Api.cases.list(reqParams), clockMap(), Api.program.get().catch(() => null)]);
      let loaded = page.cases.slice();   // accumulated rows across pages
      let cursor = page.next_cursor;     // "" when no more pages
      const total = page.total;
      const opts = saved.map((s) => `<option value="${s.id}" ${s.id === v ? "selected" : ""}>${s.pinned ? "★ " : ""}${esc(s.name)}</option>`).join("");
      const density = localStorage.getItem("idre.density") || "comfortable";
      document.body.classList.toggle("density-compact", density === "compact");
      const pager = () => `<p class="pager"><span class="muted">Showing ${loaded.length} of ${total}</span>
        ${cursor ? `<button class="mini" id="more">Load more (${Math.min(PAGE_SIZE, total - loaded.length)} of ${total - loaded.length} remaining)</button>` : ""}</p>`;
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
        const loadMore = async (ev) => {
          await UI.run(ev.currentTarget, async () => {
            try {
              const more = { limit: PAGE_SIZE, ...(status ? { status } : {}), ...(sort ? { sort } : {}), ...(lane ? { lane } : {}) };
              if (cursor.startsWith("offset:")) more.offset = cursor.slice(7); else more.cursor = cursor;
              const next = await Api.cases.list(more);
              loaded = loaded.concat(next.cases);
              cursor = next.next_cursor;
              document.querySelector(".dg-wrap").outerHTML = caseTable(loaded, clocks, sort || "opened_at", prog, groupBatch);
              $("#pg").innerHTML = pager();
              bindGrid(loaded);
              $("#more")?.addEventListener("click", loadMore);
              UI.toast(`Showing ${loaded.length} of ${total}`, { kind: "info" });
            } catch (e) { UI.toast(e.message, { kind: "warn" }); }
          }, "Loading…");
        };
        $("#more")?.addEventListener("click", loadMore);
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
        caseTable(loaded, clocks, sort || "opened_at", prog, groupBatch) + `<div id="pg">${pager()}</div>`;
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
            ${!d.sealed ? ` · <a href="javascript:void(0)" onclick="Views.showAnalysis('${id}','${d.doc_id}')">analysis</a>` : ""}
            ${can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") ?
              ` · <select class="mini" onchange="Views.moveDoc('${id}','${d.doc_id}',this.value,this)"><option value="">move…</option>
                ${FOLDERS.filter((f) => f !== d.folder).map((f) => `<option>${f}</option>`).join("")}</select>` : ""}</td></tr>`).join("") +
          `</tbody></table>`).join("");
      } else html += `<p class="muted">No documents yet.</p>`;
      html += `<div id="analysis"></div>`;

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
      html += `<h2>Stage checklists</h2>` + Object.entries(stages).map(([stage, items]) =>
        `<h3>${esc(stage)}</h3><ul class="checklist">` + items.map((i) =>
          `<li>${i.done ? "✅" : (can("CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN")
            ? `<button class="mini" onclick="Views.check('${i.id}')">check</button>` : "⬜")}
            ${esc(i.item)}${i.required ? " *" : ""}${i.done_by ? ` <span class="muted">(${esc(i.done_by)})</span>` : ""}</li>`).join("") +
        `</ul>`).join("");
      if (can("CASE_MANAGER", "FEDERAL_ADMIN"))
        html += `<div class="actions">
          <button onclick="Views.assign('${id}')">Assign / route</button>
          <button onclick="Views.letter('${id}','offer_window_notice')">Generate offer-window notice</button>
          <button onclick="Views.letter('${id}','determination_letter')">Generate determination letter</button>
        </div>`;

      // Notes — addNote (POST /notes) existed with no way to read them back
      // at all; case notes also mirror onto the activity timeline below but
      // without the stream field, so internal/coder/clinical/legal/
      // external_agency notes were indistinguishable there. Confirmed live.
      const notes = notesRes.notes || [];
      html += `<h2>Notes</h2>
        <form id="note-form" class="inline-form">
          <select name="stream" aria-label="Note stream">
            <option value="internal">Internal</option>
            <option value="coder">Coder</option>
            <option value="clinical">Clinical</option>
            <option value="legal">Legal</option>
            <option value="external_agency">External agency</option>
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
      await UI.run(ev.target.querySelector("button"), async () => {
        try {
          const r = await Api.cases.initiate({
            case_number: f.case_number, service_line: f.service_line, plan_type: f.plan_type,
            qpa_cents: Math.round(parseFloat(f.qpa) * 100), provider_id: f.provider_id,
            payer_id: f.payer_id, open_negotiation_end: f.one_end,
          });
          UI.toast("Dispute initiated — statutory clocks started");
          location.hash = `#/cases/${r.case_id}`;
        } catch (e) { UI.toast(e.message, { kind: "warn", duration: 9000 }); }
      }, "Initiating…");
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
        if (apps.length) {
          const rowsHtml = (list) => list.map((a) => `<tr><td>${esc(a.legal_name)}</td><td>${esc(a.type)}</td>
            <td>${badge(a.status)}</td><td class="muted">${esc(a.status_reason || "—")}</td><td>${fmtDate(a.submitted_at)}</td>
            <td>${["PENDING_APPROVAL"].includes(a.status) ? `
              <button onclick="Views.decide('${a.id}','APPROVE')">Approve</button>
              <button class="danger" onclick="Views.decide('${a.id}','REJECT')">Reject</button>` : ""}</td></tr>`).join("");
          const { bodyHtml, pagerHtml } = initClientPager("onb", apps, rowsHtml);
          afterRender(() => bindClientPager("onb", "#onb-tb tbody"));
          html += `<h2>Review queue</h2><table id="onb-tb"><thead><tr>
          <th>Legal name</th><th>Type</th><th>Status</th><th>Reason</th><th>Submitted</th><th></th></tr></thead>
          <tbody>${bodyHtml}</tbody></table>${pagerHtml}`;
        } else {
          html += `<h2>Review queue</h2><p class="muted">Queue empty.</p>`;
        }
      } catch (e) { html += err(e); }
    }
    html += `<h2>New application</h2><p class="muted">Self-service registration for provider, payer, IDRE and auditor organizations.</p>
      <p><a class="button" href="#/onboarding/new">Start application</a></p>`;
    return html;
  }

  function onboardingNew() {
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
  async function voice() {
    try {
      const [intake, logs] = await Promise.all([Api.voice.intake(), Api.voice.logs()]);
      const intakeRowsHtml = (list) => list.map((v) => `<tr><td>${esc(v.caller_name)}<br/><span class="muted">${esc(v.caller_phone)}</span></td>
            <td>${esc(v.organization)}</td><td>${esc(v.summary)}</td><td>${badge(v.status)}</td><td>${fmtDate(v.created_at)}</td></tr>`).join("");
      const intakePage = initClientPager("vintake", intake, intakeRowsHtml);
      const logRowsHtml = (list) => list.map((l) => `<tr><td>${esc(l.direction)}</td><td>${esc(l.tool)}</td><td>${esc(l.case_number)}</td>
            <td>${badge(l.status)}</td><td>${fmtDate(l.created_at)}</td></tr>`).join("");
      const logPage = initClientPager("vlog", logs, logRowsHtml);
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
        bindClientPager("vintake", "#vintake-tb tbody");
        bindClientPager("vlog", "#vlog-tb tbody");
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
        <h2>Intake requests (${intake.length})</h2>` +
        (intake.length ? `<table id="vintake-tb"><thead><tr><th>Caller</th><th>Organization</th><th>Summary</th><th>Status</th><th>At</th></tr></thead>
          <tbody>${intakePage.bodyHtml}</tbody></table>${intakePage.pagerHtml}` : `<p class="muted">No intake requests.</p>`) +
        `<h2>Call log</h2>` +
        (logs.length ? `<table id="vlog-tb"><thead><tr><th>Direction</th><th>Tool/event</th><th>Case</th><th>Status</th><th>At</th></tr></thead>
          <tbody>${logPage.bodyHtml}</tbody></table>${logPage.pagerHtml}` : `<p class="muted">No calls yet.</p>`);
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
        const open = rows.filter((s) => !/^CLOSED|^Decided|^Dismissed|^Withdrawn|^Ineligible/.test(s.status))
          .reduce((n, s) => n + s.count, 0);
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
  async function loadRptSla(btn) {
    await runOrDirect(btn, async () => {
      try {
        const sla = await Api.reports.sla();
        document.getElementById("kpi-breaches").textContent = sla.length.toLocaleString();
        const byClock = {};
        sla.forEach((b) => { byClock[b.clock] = (byClock[b.clock] || 0) + 1; });
        document.getElementById("rpt-sla-out").innerHTML = (sla.length
          ? `<div class="chart-card">` + chartHBars(
              Object.entries(byClock).map(([k, v]) => ({ label: k, value: v }))) + `</div>` +
            `<table><thead><tr><th>Case</th><th>Clock</th><th>Detail</th><th>At</th></tr></thead><tbody>` +
            sla.map((b) => `<tr><td class="mono">${esc(b.case_id)}</td><td>${badge(b.clock)}</td>
              <td>${esc(b.detail)}</td><td>${fmtDate(b.at)}</td></tr>`).join("") + `</tbody></table>`
          : `<p class="muted">No breaches recorded — all statutory clocks held. ✔</p>`);
        document.getElementById("rpt-sla-csv").onclick = () => csvDownload("sla_breaches.csv", ["case_id", "clock", "detail", "at"],
          sla.map((x) => [x.case_id, x.clock, x.detail, x.at]));
      } catch (e) { rptError("rpt-sla-out", e); }
    });
  }
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
    const geoLink = (window.IDRE_CONFIG.geoMapUrl || "")
      ? `<a class="button" href="${window.IDRE_CONFIG.geoMapUrl}" target="_blank">Geospatial audit map (GeoLibre) ↗</a>` : "";
    afterRender(() => {
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
      <p class="muted">Natural-language questions over this state's dispute graph — entity linking, path retrieval,
      GraphSAGE ranking, and a local LLM answer. Every claim cites its graph path; nothing leaves the platform.</p>
      <form id="kgqa" class="kgqa-bar">
        <input name="q" required minlength="6" autocomplete="off"
          placeholder="e.g. Which disputes share a payer with case IDR-2026-0004?" aria-label="Ask the dispute graph" />
        <button>Ask</button>
      </form>
      <div id="kgqa-out" aria-live="polite"></div>
      <h3>Try asking</h3>
      <div class="kgqa-hints">
        ${["Which cases are most related to IDR-2026-0004?",
           "What connects Lone Star Imaging and BlueShield of Texas?",
           "Which disputes involve air ambulance service lines?",
           "Show cases related to IDR-2026-0002 and why"].map((q) =>
          `<button class="kgqa-hint" data-q="${esc(q)}">${esc(q)}</button>`).join("")}
      </div>`;
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
      document.querySelectorAll(".kgqa-hint").forEach((b) =>
        b.addEventListener("click", () => { $("#kgqa").q.value = b.dataset.q; run(b.dataset.q); }));
    });
    return html;
  }

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
      <details class="prog-sec" open><summary>Dual status</summary>
      <p class="muted">Current: ${c.internal_status ? badge(c.internal_status) : "<em>none</em>"} / ${c.agency_status ? badge(c.agency_status) : "<em>none</em>"}</p>
      <p class="muted">Case details: ${c.details && Object.keys(c.details).length
        ? Object.entries(c.details).map(([k, v]) => `<span class="mono">${esc(k)}=${esc(v)}</span>`).join(", ")
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

      <details class="prog-sec"><summary>Eligibility review</summary>
      <p class="muted">The engine applies this program's rules in order and records the evidence with the verdict:
        <b>1)</b> an explicit ineligibility flag wins immediately; <b>2)</b> the filing window (e.g. final
        determination + 12 months) is checked; <b>3)</b> the disputed amount is tested against the provider-type
        threshold matrix (contracted status included); <b>4)</b> an invalid AOR puts the case on attorney hold.
        Result is ELIGIBLE, INELIGIBLE (with reason code), or HOLD_AOR — and INELIGIBLE/HOLD move the case status
        automatically.</p>
      <div id="p-elig-history"></div>
      <div class="actions"><button id="p-elig-auto" title="Derives every rule input from the case record and analyzed documents, then decides — or tells you exactly which inputs are still missing">⚡ Auto-adjudicate from case + documents</button></div>
      <div id="p-elig-auto-out"></div>
      <form id="p-elig" class="inline-form">
        <input name="provider_type" placeholder="provider type (e.g. hospital_inpatient)" required />
        <label>contracted <input type="checkbox" name="contracted" /></label>
        <input name="amount" type="number" step="0.01" placeholder="disputed $" required />
        <input name="fd" type="date" title="final determination date" />
        <input name="flags" placeholder="flags (comma-separated reason codes)" />
        <label>AOR valid <input type="checkbox" name="aor_valid" checked /></label>
        <button>Compute eligibility</button></form><div id="p-elig-out"></div></details>

      <details class="prog-sec" id="p-send-details"><summary>Correspondence</summary>
      <form id="p-send" class="inline-form">
        <select name="template">${(cfg.correspondence?.templates || []).map((tp) =>
          `<option value="${esc(tp.key)}">${esc(tp.key)}${tp.qa_role ? " (QA: " + esc(tp.qa_role) + ")" : ""}</option>`).join("")}</select>
        <select name="rfi_to" title="only used for the rfi template"><option value="provider">RFI to: provider</option><option value="plan">RFI to: health plan</option></select>
        <input name="to" placeholder="to emails (comma-separated)" required />
        <input name="cc" placeholder="cc emails" />
        <textarea name="body" rows="3" placeholder="message body — {share_link} inserts a secure upload link, {download_link} a link to the latest generated document; both are minted on send" required></textarea>
        <label><input type="checkbox" name="auto_share" checked /> Attach a secure upload link (minted on send)</label>
        <label><input type="checkbox" name="auto_download" /> Attach a download link to the latest generated document (minted on send)</label>
        <button>Send / submit for QA</button></form><div id="p-corr"></div></details>

      <details class="prog-sec"><summary>Invoices & claims</summary>
      <form id="p-inv" class="inline-form">
        <select name="party"><option>HEALTH_PLAN</option><option>PROVIDER</option></select>
        <select name="kind"><option>INITIAL_FEE</option><option>FULL_REVIEW</option><option>DEFAULT</option></select>
        <input name="amount" type="number" step="0.01" placeholder="amount $" required />
        <button>Issue invoice</button></form><div id="p-inv-list"></div>
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
      <form id="p-optout" class="inline-form">
        <label>eligible to opt out <input type="checkbox" name="eligible" /></label>
        <input name="rationale" placeholder="rationale" required /><button>Record opt-out decision</button></form></details>

      <details class="prog-sec"><summary>Workflow signals</summary>
      <p class="muted">Tells the running case workflow what happened — distinct from the status/date forms above, which only update the database.</p>
      <form id="p-signal" class="inline-form">
        <select name="signal">
          <option value="PACKET_COMPLETE">Packet complete</option>
          <option value="OUTREACH_DONE">Outreach done (payment arrived, packet didn't)</option>
          <option value="PROVIDER_WITHDRAW">Provider withdrew</option>
          <option value="AOR_REVISED">AOR revised (resolves a hold)</option>
          <option value="ESTIMATE_PERMISSION">Provider permission decided</option>
          <option value="PLAN_RESPONSE_RECEIVED">Plan response received</option>
          <option value="CODING_REVIEW_COMPLETE">Coder review complete</option>
          <option value="CLINICAL_REVIEW_COMPLETE">Clinical review complete</option>
          <option value="ATTORNEY_REVIEW_COMPLETE">Attorney review complete</option>
          <option value="FINAL_ORDER_ISSUED">Final order issued (by AHCA)</option>
          <option value="RFI_RESPONSE_RECEIVED">RFI response received</option>
        </select>
        <label>granted / estimate requested / clinical review needed <input type="checkbox" name="flag" /></label>
        <br/><span class="muted">Only used for "Attorney review complete" — leave blank if not applicable:</span><br/>
        <select name="case_outcome"><option value="">case outcome…</option>
          ${(cfg.field_schema?.case_outcome || []).map((s) => `<option>${esc(s)}</option>`).join("")}</select>
        <select name="party_billed"><option value="">party billed…</option>
          ${(cfg.field_schema?.party_billed || []).map((s) => `<option>${esc(s)}</option>`).join("")}</select>
        <input name="amount" type="number" step="0.01" placeholder="final amount awarded $" />
        <input name="num_claims" type="number" placeholder="number of claims reviewed" />
        <button>Send signal</button></form></details>

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
      $("#p-status")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try { await Api.program.setStatus(id, { internal_status: f.internal.value, agency_status: f.agency.value });
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
          el.innerHTML = `<table><thead><tr><th>Clock basis</th><th>Key</th><th>Recorded</th></tr></thead>
            <tbody>${rows.join("")}</tbody></table>`;
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
      const loadElig = () => Api.program.eligibilityHistory(id).then((r) => {
        const el = document.getElementById("p-elig-history"); if (!el) return;
        const revs = r.reviews || [];
        el.innerHTML = revs.length ? `<table><thead><tr><th>Result</th><th>Reason</th><th>By</th><th>When</th></tr></thead><tbody>` +
          revs.map((v) => `<tr><td>${badge(v.result)}</td><td>${esc(v.reason || "—")}</td>
            <td>${esc(v.decided_by || "")}</td><td class="muted">${fmtDate(v.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No eligibility review on record yet.</p>`;
      }).catch(() => {});
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
              out.innerHTML = `<p>${badge(r.result)} ${r.reason ? esc(r.reason) : ""}
                <span class="muted">auto review ${esc(r.review_id)} — inputs derived from case record + analyzed documents</span></p>`;
              loadElig();
            }
          } catch (e) { UI.toast(e.message, { kind: "warn" }); }
        }, "Evaluating…");
      });
      $("#p-elig")?.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const f = ev.target;
        try {
          const r = await Api.program.eligibility(id, {
            provider_type: f.provider_type.value, contracted: f.contracted.checked,
            disputed_amount_cents: money(f.amount.value),
            final_determination_at: f.fd.value || "",
            flags: f.flags.value ? f.flags.value.split(",").map((x) => x.trim()).filter(Boolean) : [],
            aor_valid: f.aor_valid.checked,
          });
          document.getElementById("p-elig-out").innerHTML =
            `<p>${badge(r.result)} ${r.reason ? esc(r.reason) : ""} <span class="muted">review ${esc(r.review_id)}</span></p>`;
          loadElig();
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
          `<tr><td>${badge(m.direction)}</td><td>${esc(m.subject || "")}</td>
           <td class="muted">${esc(m.template || "")} · ${fmtDate(m.created_at)}</td></tr>`).join("") +
          `</tbody></table>` : "";
      }).catch(() => {});
      const renderInvoices = (r) => {
        const el = document.getElementById("p-inv-list"); if (!el) return;
        el.innerHTML = (r.invoices || []).length ? `<table><tbody>` + r.invoices.map((v) =>
          `<tr><td class="mono">${esc(v.invoice_no)}</td><td>${esc(v.party)}</td><td>$${(v.amount_cents / 100).toFixed(2)}</td>
           <td>${badge(v.status)}</td>
           <td>${v.status === "OPEN" ? `<a href="javascript:void(0)" onclick="Views.payInvoice('${v.id}')">💳 pay by card</a> ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','PAY',this)">mark paid</a> ·
             <a href="javascript:void(0)" onclick="Views.settleInvoice('${v.id}','REFUND',this)">refund</a>` : ""}</td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No invoices on this case.</p>`;
      };
      const refreshInv = () => Api.program.invoices(id).then(renderInvoices).catch(() => {});
      refreshInv();
      // Card payments settle via Stripe webhook — poll so status flips live
      // instead of requiring a manual reload.
      const invPoll = setInterval(() => {
        if (!document.getElementById("p-inv-list")) { clearInterval(invPoll); return; }
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
      const intro = `<details class="prog-sec" ${q.length ? "" : "open"}><summary>What the QA gate does</summary>
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
      const isNote = d.channel === "note";
      const isCopilot = (d.artifact || "").startsWith("copilot_");
      box.dataset.channel = d.channel || "email";
      // Copilot drafts are accept/EDIT/reject: the reviewer rewrites in place
      // and the edited text is what gets approved (server records the edit).
      const bodyHtml = isCopilot
        ? `<textarea id="qa-edit" rows="14" style="width:100%">${esc(d.body)}</textarea>
           <p class="muted">Copilot draft — edit freely; the approved text is what gets ${isNote ? "filed" : "sent"}, and the edit is recorded.</p>`
        : `<pre class="qa-body">${esc(d.body)}</pre>`;
      box.innerHTML = `<div class="card"><h3>${esc(d.subject)}</h3>
        <p class="muted">${isNote ? "determination rationale · files to the case timeline on approval" : `to: ${esc(to)} · channel ${esc(d.channel)}`}
          ${isCopilot ? ' · <span class="badge s-review">COPILOT DRAFT</span>' : ""}</p>
        ${bodyHtml}
        <div class="actions">
          <button onclick="Views.qaDecide('${qaId}','APPROVE',this)">${isNote ? "Approve & file" : "Approve & send"}</button>
          <button class="danger" onclick="Views.qaDecide('${qaId}','REJECT',this)">Reject</button></div></div>`;
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  async function qaDecide(qaId, decision, el) {
    let note = "";
    const isNote = $("#qa-detail")?.dataset.channel === "note";
    const edited = $("#qa-edit") ? $("#qa-edit").value : "";
    if (decision === "REJECT") {
      const v = await UI.modal({ title: "Reject draft", danger: true, submitLabel: "Reject",
        fields: [{ name: "note", label: "Rejection note", type: "textarea", required: true,
          hint: "Returned to the drafter with the draft." }] });
      if (!v) return;
      note = v.note;
    } else if (isNote) {
      if (!(await UI.confirm("Approve and file?", "The rationale is recorded on the case timeline. Nothing is emailed.", "Approve & file"))) return;
    } else if (!(await UI.confirm("Approve and send?", "The email is delivered to all recipients now and logged to correspondence.", "Approve & send"))) return;
    await UI.run(el, async () => {
      try {
        const r = await Api.program.qaDecision(qaId, decision, note, edited);
        if (r.email_delivery_error) {
          UI.toast(`Approved, but email delivery failed: ${r.email_delivery_error} — still APPROVED, retry once the address is fixed`,
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

  async function intake() {
    // afterRender() fires AFTER the await, right before return -- see the
    // other views in this file for why that order matters (setTimeout(fn,0)
    // beats an in-flight fetch if called before it).
    try {
      const [r, prog] = await Promise.all([Api.program.intake({ limit: 50 }), Api.program.get().catch(() => null)]);
      const rows = r.intake || [];
      const intakeNext = r.next_offset ?? -1;
      const intakeTotal = r.total ?? rows.length;
      window._intakePager = { next: intakeNext }; // reset on every view render
      // Federal NSA intakes price on QPA; programmed tenants (FL AHCA) have
      // no QPA concept and use a disputed amount instead -- same split as
      // everywhere else amounts show in this app. The amount typed here is
      // sent as both keys (programops.go's createIntake only reads the one
      // that applies to this tenant and ignores the other), so the form
      // itself doesn't need to branch -- only its label does.
      const label = amtLabel(prog);
      afterRender(() => {
        $("#intake-form")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          await newIntake(ev.target);
        });
        // Conversational intake (step 5): the chat EXTRACTS into the form;
        // the human reviews the filled form and files with the same button
        // as always. The model never files.
        window._intakeChat = { fields: {}, history: [] };
        // Human keystrokes win over extraction: once a field is touched, the
        // assistant never overwrites it.
        const f0 = $("#intake-form");
        ["email", "contact_name", "org", "amount"].forEach((n) =>
          f0?.[n]?.addEventListener("input", () => { f0[n].dataset.touched = "1"; }));
        $("#intake-chat-form")?.addEventListener("submit", (ev) => {
          ev.preventDefault();
          const msg = ev.target.message.value.trim();
          if (msg) { ev.target.message.value = ""; intakeChatTurn(msg); }
        });
        // Bulk intake (CSV): parse on file pick, submit posts the idempotent
        // batch and renders the per-row receipt.
        window._intakeBulk = { items: [] };
        $("#intake-bulk-file")?.addEventListener("change", (ev) => bulkIntakeFile(ev.target));
        $("#intake-bulk-btn")?.addEventListener("click", (ev) => { ev.preventDefault(); bulkIntakeSubmit(ev.target); });
      });
      return `<div class="view-head"><h1>Pre-case intake</h1>
        <span class="muted">every request to open a dispute, newest first — legacy tracker rows and real cases opened directly both land here</span></div>
        <details open class="card" style="margin-bottom:12px"><summary><b>✦ Describe it, I'll fill the form</b> — conversational intake (extraction only; you review and file)</summary>
          <div id="intake-chat-thread" class="asst-thread" style="min-height:80px;max-height:30vh;margin:10px 0">
            <div class="asst-turn asst-ai"><div class="asst-who">intake assistant</div>
            <div class="asst-body">Describe the request in your own words — who called, provider or plan, amounts, anything else. I'll fill the form below as we go.</div></div>
          </div>
          <form id="intake-chat-form" class="asst-form">
            <input name="message" autocomplete="off" placeholder="e.g. Dana from Meridian Surgical called about a $4,200 out-of-network dispute…" aria-label="Describe the intake" />
            <button>Send</button></form>
          <p class="muted" id="intake-chat-missing" style="margin:6px 0 0"></p></details>
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
          <select name="filing_party_type" title="filing party">
            <option value="PROVIDER">Provider files</option>
            <option value="HEALTH_PLAN">Health plan files</option></select>
          <input name="amount" type="number" step="0.01" placeholder="${esc(label)} $" />
          <button>New intake request</button></form>` +
        (rows.length ? `<table><thead><tr><th>Case #</th><th>Email</th><th>Org</th><th>Filing party</th><th>${esc(label)}</th><th>Status</th><th>Outreach</th><th>Packet complete</th><th></th></tr></thead><tbody>` +
          rows.map(intakeRowHtml).join("") +
          `</tbody></table>` : `<p class="muted">No intake requests.</p>`) +
        (intakeNext >= 0 ? `<p class="pager" id="intake-pg"><span class="muted">Showing ${rows.length} of ${intakeTotal}</span>
          <button class="mini" onclick="Views.intakeMore(this)">Load more (${Math.min(50, intakeTotal - rows.length)} remaining)</button></p>` : "");
    } catch (e) { return err(e); }
  }

  // Shared by intake()'s initial render and intakeMore()'s appended page --
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

  async function intakeMore(btn) {
    const st = window._intakePager || { next: 50 };
    await UI.run(btn, async () => {
      try {
        const r = await Api.program.intake({ limit: 50, offset: st.next });
        const rows = r.intake || [];
        st.next = r.next_offset ?? -1;
        window._intakePager = st;
        const body = document.querySelector("#intake-pg")?.previousElementSibling?.querySelector("tbody");
        if (body) body.insertAdjacentHTML("beforeend", rows.map(intakeRowHtml).join(""));
        const pg = document.getElementById("intake-pg");
        if (pg && st.next < 0) pg.outerHTML = "";
        else if (pg) pg.querySelector("button").textContent = "Load more";
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Loading…");
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
          <td>${x.status === "CREATED" ? badge("CREATED") : `<span class="badge warn">ERROR</span>`}</td>
          <td>${x.case_number ? `<a href="#/cases/${x.case_id}">${esc(x.case_number)}</a>` : esc(x.intake_id || "")}</td>
          <td class="muted">${esc(x.error || "")}</td></tr>`).join("");
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
      // Sent as both keys -- createIntake (programops.go) only reads the one
      // that applies to this tenant (qpa_cents for federal, disputed_amount_
      // cents for programmed) and ignores the other, so the form doesn't
      // need to know which kind of tenant it's running against.
      const cents = form.amount.value ? Math.round(parseFloat(form.amount.value) * 100) : 0;
      const r = await Api.program.createIntake({
        email: form.email.value, contact_name: form.contact_name.value, org: form.org.value,
        filing_party_type: form.filing_party_type.value,
        disputed_amount_cents: cents, qpa_cents: cents,
      });
      if (r.programmed) {
        UI.toast(`Case ${r.case_number} opened — filing instructions emailed to ${esc(form.email.value)}`, { sticky: true });
        location.hash = `#/cases/${r.case_id}`;
      } else {
        UI.toast(`Intake request opened (${form.filing_party_type.value === "HEALTH_PLAN" ? "health plan" : "provider"} filing) — submission instructions queued`);
        App.rerender();
      }
    } catch (e) { UI.toast(e.message, { kind: "warn" }); }
  }

  // One turn of conversational intake: send the worker's words plus the
  // fields extracted so far (client-carried state, endpoint is stateless),
  // render the follow-up, and prefill the REAL form — filing stays manual.
  async function intakeChatTurn(msg) {
    const thread = document.getElementById("intake-chat-thread");
    const add = (role, body) => thread?.insertAdjacentHTML("beforeend",
      `<div class="asst-turn ${role === "user" ? "asst-user" : "asst-ai"}">
         <div class="asst-who">${role === "user" ? "you" : "intake assistant"}</div>
         <div class="asst-body">${esc(body)}</div></div>`);
    const st = window._intakeChat || (window._intakeChat = { fields: {}, history: [] });
    add("user", msg); add("assistant", "…");
    try {
      const r = await Api.program.intakeConverse(msg, st.fields, st.history);
      thread.lastElementChild.remove();
      add("assistant", r.reply || "");
      st.history.push(msg);
      st.fields = r.fields || {};
      // Prefill the form; never overwrite text the human has typed.
      // Chat-filled fields are tagged "from chat" so the reviewer can see
      // exactly which values the model supplied — the mock's contract.
      const f = document.getElementById("intake-form");
      if (f) {
        const tag = (el) => { el.classList.add("from-chat"); el.title = "Prefilled from the conversation — review before filing"; };
        if (st.fields.email && !f.email.dataset.touched) { f.email.value = st.fields.email; tag(f.email); }
        if (st.fields.contact_name && !f.contact_name.dataset.touched) { f.contact_name.value = st.fields.contact_name; tag(f.contact_name); }
        if (st.fields.org && !f.org.dataset.touched) { f.org.value = st.fields.org; tag(f.org); }
        if (st.fields.filing_party_type) { f.filing_party_type.value = st.fields.filing_party_type; tag(f.filing_party_type); }
        const cents = st.fields.disputed_amount_cents || st.fields.qpa_cents;
        if (cents && !f.amount.dataset.touched) { f.amount.value = (cents / 100).toFixed(2); tag(f.amount); }
      }
      const miss = document.getElementById("intake-chat-missing");
      if (miss) {
        if (r.ready) {
          // The handoff: conversation is done, the screen takes over — scroll
          // the reviewer to the prefilled form; filing stays a human click.
          miss.innerHTML = `✓ Ready — review the prefilled form and file when you're satisfied.
            <button class="mini" type="button" onclick="document.getElementById('intake-form').scrollIntoView({behavior:'smooth',block:'center'});document.getElementById('intake-form').classList.add('chat-handoff')">Review the prefilled form ↓</button>`;
        } else {
          miss.textContent = (r.missing || []).length ? "Still needed: " + r.missing.join(", ") : "";
        }
      }
    } catch (e) {
      thread?.lastElementChild?.remove();
      add("assistant", `⚠ ${e.message}`);
    }
    thread && (thread.scrollTop = thread.scrollHeight);
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

  async function deliverables() {
    try {
      const r = await Api.program.deliverables();
      const d = r.deliverables || [];
      const hist = r.history || [];
      const lastDelivered = (name) => hist.filter((h) => h.name === name)
        .sort((a, b) => String(b.delivered_at).localeCompare(String(a.delivered_at)))[0];
      const daysUntil = (due) => Math.ceil((new Date(due + "T00:00:00") - Date.now()) / 864e5);
      const dueChip = (due) => {
        if (!due) return `<span class="muted">—</span>`;
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
        Anything overdue or due within 3 days is highlighted; due soon is amber, on track is green.</p></details>
      <form class="inline-form" onsubmit="event.preventDefault(); Views.requestDeliverable(new FormData(event.target), event.target.querySelector('button'))">
        <input name="name" required placeholder="Ad hoc report name" />
        <input name="ref" placeholder="Contract ref (optional)" size="10" />
        <button class="mini">request ad hoc (due +10 business days)</button></form>` +
        (d.length ? `<div class="deliv-grid">` + d.map((x) => {
          const last = lastDelivered(x.name);
          return `<div class="deliv-card">
            <div class="deliv-head"><b>${esc(x.name)}</b>${dueChip(x.next_due)}</div>
            <div class="muted mono">${esc(x.due_rule)}</div>
            <div class="deliv-meta">next due <b>${esc(x.next_due || "—")}</b><br/>
              last delivered ${last ? fmtDate(last.delivered_at) : `<span class="muted">never</span>`}</div>
            <button class="mini" onclick="Views.submitDeliverable('${esc(x.name)}', this)">✓ mark delivered</button></div>`;
        }).join("") + `</div>` : `<p class="muted">No deliverables configured for this program.</p>`) +
        (hist.length ? `<h2>Delivery history</h2><table><thead><tr><th>Deliverable</th><th>Status</th><th>Delivered</th></tr></thead><tbody>` +
          hist.map((h) => `<tr><td>${esc(h.name)}</td><td>${badge(h.status)}</td>
            <td class="muted">${fmtDate(h.delivered_at)}</td></tr>`).join("") + `</tbody></table>` : "");
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
    try {
      const FIN_PAGE = 25;
      const [fin, pays, chks] = await Promise.all([
        Api.program.financial(),
        Api.program.payments(null, { limit: FIN_PAGE }).catch(() => ({ payments: [] })),
        Api.program.checks("", { limit: FIN_PAGE }).catch(() => ({ checks: [] })),
      ]);
      // accumulated rows + server offsets for the "Load more" pagers
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
        (ck.length ? `<table><thead><tr><th>Check</th><th>Payee</th><th>Date</th><th>Routing · Account</th><th>Courtesy</th><th>Legal (ICR)</th><th>Memo</th><th>Match</th><th>Status</th><th>Conf</th><th></th></tr></thead><tbody id="fin-ck-body">` +
          ck.map(checkRow).join("") + `</tbody></table>` +
          (reviewCk.length ? `<p class="muted">⚠ ${reviewCk.length} check(s) awaiting manual review — OCR could not match them to an open invoice.</p>` : "") :
          `<p class="muted">No checks received yet — upload a scan or photo to start intake.</p>`) +
        (finPager.checks.next >= 0
          ? `<p class="pager" id="fin-ck-pg"><span class="muted">Showing ${ck.length} of ${finPager.checks.total}</span>
             <button class="mini" onclick="Views.financeMore('checks', this)">Load more (${Math.min(FIN_PAGE, finPager.checks.total - ck.length)} remaining)</button></p>` : "");

      // Card payments
      const plist = finPager.payments.rows;
      if (plist.length)
        html += `<h2>Card payments</h2><table><thead><tr><th>Case</th><th>Payer</th><th>Amount</th><th>Status</th><th>Stripe ref</th><th>When</th></tr></thead><tbody id="fin-pay-body">` +
          plist.map(payRow).join("") + `</tbody></table>` +
          (finPager.payments.next >= 0
            ? `<p class="pager" id="fin-pay-pg"><span class="muted">Showing ${plist.length} of ${finPager.payments.total}</span>
               <button class="mini" onclick="Views.financeMore('payments', this)">Load more (${Math.min(FIN_PAGE, finPager.payments.total - plist.length)} remaining)</button></p>` : "");

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
        bindClientPager("txn", "#txn-tb tbody");
      });
      return html;
    } catch (e) { return err(e); }
  }

  // finance() pager state — survives only for the rendered page
  let finPager = null;
  let checkRow = null, payRow = null; // row renderers set by finance()

  async function financeMore(kind, btn) {
    const st = finPager?.[kind];
    if (!st || st.next < 0) return;
    await UI.run(btn, async () => {
      try {
        const limit = 25;
        const r = kind === "checks"
          ? await Api.program.checks("", { limit, offset: st.next })
          : await Api.program.payments(null, { limit, offset: st.next });
        const rows = kind === "checks" ? (r.checks || []) : (r.payments || []);
        st.rows = st.rows.concat(rows);
        st.next = r.next_offset ?? -1;
        st.total = r.total ?? st.total;
        const body = document.getElementById(kind === "checks" ? "fin-ck-body" : "fin-pay-body");
        const render = kind === "checks" ? checkRow : payRow;
        if (body && render) body.insertAdjacentHTML("beforeend", rows.map(render).join(""));
        const pg = document.getElementById(kind === "checks" ? "fin-ck-pg" : "fin-pay-pg");
        if (pg) pg.outerHTML = st.next >= 0
          ? `<p class="pager" id="${kind === "checks" ? "fin-ck-pg" : "fin-pay-pg"}"><span class="muted">Showing ${st.rows.length} of ${st.total}</span>
             <button class="mini" onclick="Views.financeMore('${kind}', this)">Load more (${Math.min(25, st.total - st.rows.length)} remaining)</button></p>`
          : `<p class="pager"><span class="muted">Showing ${st.rows.length} of ${st.total}</span></p>`;
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
    }, "Loading…");
  }

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
      afterRender(async () => {
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
              ${stat(d.pending_qa, "at QA gate")}${stat(d.checks_in_review, "checks in review")}
              ${stat(d.new_docs_24h, "docs analyzed 24h")}${stat((d.tasks_due || []).length, "tasks due")}
            </div>
            ${risk ? `<h3 style="margin:10px 0 4px">SLA risk — open a thread to act</h3>
              <table><thead><tr><th>Case</th><th>Status</th><th>Lane</th><th>SLA</th></tr></thead><tbody>${risk}</tbody></table>` : ""}`;
        } catch (e) {
          box.innerHTML = `<p class="muted">Briefing unavailable: ${esc(e.message)}</p>`;
        }
      });
      try {
        const r = await Api.cases.list({ limit: 50 });
        const rows = (r.cases || []).map((c) =>
          `<tr class="click" onclick="location.hash='#/assistant/${c.id}'"><td class="mono"><a href="#/cases/${c.id}" onclick="event.stopPropagation()">${esc(c.case_number)}</a></td>
           <td>${esc(c.service_line || "")}</td><td>${badge(c.status)}</td></tr>`).join("");
        return `<div class="view-head"><h1>Assistant</h1>
          <span class="muted">grounded on platform-verified case facts · advisory only · every turn is on the record</span></div>
          <div id="asst-briefing"><p class="muted">Preparing your briefing…</p></div>
          <h2 style="margin-top:14px">Case threads</h2>
          <p>Pick a case to open its thread:</p>
          <table><thead><tr><th>Case</th><th>Line</th><th>Status</th></tr></thead><tbody>${rows}</tbody></table>`;
      } catch (e) { return err(e); }
    }
    afterRender(async () => {
      const thread = document.getElementById("asst-thread");
      try {
        const c = await Api.cases.get(caseId);
        document.getElementById("asst-case").innerHTML =
          `Case <a href="#/cases/${caseId}">${esc(c.case_number)}</a> · ${esc(c.status)}`;
        const h = await Api.program.copilotChatHistory(caseId);
        thread.innerHTML = (h.turns || []).map(asstTurnHtml).join("") ||
          `<div class="muted" style="padding:12px">No turns yet — ask anything about this case, or use a chip below.</div>`;
        thread.scrollTop = thread.scrollHeight;
      } catch (e) {
        thread.innerHTML = `<div class="muted" style="padding:12px">${esc(e.message)}</div>`;
      }
      const form = document.getElementById("asst-form");
      form?.addEventListener("submit", (e) => {
        e.preventDefault();
        const input = form.message;
        const msg = input.value.trim();
        if (msg) { input.value = ""; assistantSend(caseId, msg); }
      });
      asstQaLoad(caseId);
      copilotLoadBatches(caseId); // pending action batches render inline — decide without leaving the thread
    });
    return `<div class="view-head"><h1>Assistant</h1>
      <span class="muted" id="asst-case">loading case…</span></div>
      <div id="asst-thread" class="asst-thread"></div>
      <div id="asst-qa"></div>
      <div id="asst-batches"></div>
      <div class="asst-chips">
        <button class="mini" onclick="Views.assistantChip('${caseId}','brief',this)">▤ Brief me</button>
        <button class="mini" onclick="Views.assistantChip('${caseId}','determination_rationale',this)">✍ Draft rationale</button>
        <button class="mini" onclick="Views.assistantChip('${caseId}','correspondence',this)">✉ Draft correspondence</button>
        <button class="mini" onclick="Views.assistantChip('${caseId}','actions',this)">⚙ Propose actions</button>
      </div>
      <form id="asst-form" class="asst-form">
        <input name="message" autocomplete="off" placeholder="Ask about this case… (e.g. what's blocking eligibility?)" aria-label="Message the assistant" />
        <button>Send</button>
      </form>
      <p class="muted" style="margin-top:6px">Advisory only — the assistant cannot change case state; chips route through the same gates as the screens. Every turn is recorded.</p>`;
  }

  function asstTurnHtml(t) {
    const who = t.role === "user" ? "you" : `assistant${t.model ? ` · ${esc(t.model)}` : ""}`;
    return `<div class="asst-turn ${t.role === "user" ? "asst-user" : "asst-ai"}">
      <div class="asst-who">${who}</div>
      <div class="asst-body">${esc(t.body)}</div></div>`;
  }

  function asstAppend(caseId, role, body, model) {
    const thread = document.getElementById("asst-thread");
    if (!thread) return;
    thread.insertAdjacentHTML("beforeend", asstTurnHtml({ role, body, model }));
    thread.scrollTop = thread.scrollHeight;
  }

  async function assistantSend(caseId, msg) {
    asstAppend(caseId, "user", msg);
    asstAppend(caseId, "assistant", "…", "");
    try {
      const r = await Api.program.copilotChat(caseId, msg);
      const thread = document.getElementById("asst-thread");
      thread?.querySelector(".asst-turn:last-child")?.remove();
      asstAppend(caseId, "assistant", r.reply, r.model);
    } catch (e) {
      const thread = document.getElementById("asst-thread");
      thread?.querySelector(".asst-turn:last-child")?.remove();
      asstAppend(caseId, "assistant", `⚠ ${e.message}`, "");
    }
  }

  // Chips run the Phase 1-3 primitives and narrate the outcome into the
  // thread — same endpoints, same gates, conversational surface.
  async function assistantChip(caseId, kind, btn) {
    await UI.run(btn, async () => {
      try {
        if (kind === "brief") {
          asstAppend(caseId, "user", "Brief me on this case.");
          const r = await Api.program.copilotBrief(caseId);
          asstAppend(caseId, "assistant", r.brief, r.model || "");
        } else if (kind === "actions") {
          asstAppend(caseId, "user", "Propose an action batch.");
          const r = await Api.program.copilotProposeActions(caseId);
          const acts = (r.actions || []).map((a) => `• ${a.type.replace(/_/g, " ")}`).join("\n") || "• (none)";
          asstAppend(caseId, "assistant",
            `Proposed ${(r.actions || []).length} action(s) — review and approve in the batch card below, without leaving this thread:\n${r.rationale || ""}\n${acts}`, r.model || "");
          copilotLoadBatches(caseId);
        } else {
          asstAppend(caseId, "user", kind === "correspondence" ? "Draft correspondence." : "Draft a determination rationale.");
          const r = await Api.program.copilotDraft(caseId, kind);
          asstAppend(caseId, "assistant",
            `Draft queued in the QA gate (${r.subject}). Approve, edit, or reject it there — nothing is sent or filed automatically.`, "");
        }
      } catch (e) { asstAppend(caseId, "assistant", `⚠ ${e.message}`, ""); }
    }, "Working…");
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
      const [lr, ar] = await Promise.all([Api.program.rules(), Api.program.rulesAudit().catch(() => ({ changes: [] }))]);
      rulesDraft = (lr.rules || []).map((r) => ({ ...r }));
      afterRender(bindRulesAdmin);
      const audit = ar.changes || [];
      return `<div class="view-head"><h1>Program rules</h1>
        <span class="muted">live policy — changes take effect immediately and are permanently audited</span></div>
        <p><button onclick="Views.createTenantFlow()">＋ Create tenant</button>
           <span class="muted">activates a pre-provisioned state's Keycloak group + optional first user</span></p>
        <p><button onclick="Views.createFederalAdminFlow()">＋ Create federal admin</button>
           <span class="muted">cross-tenant FEDERAL_ADMIN or PLATFORM_ADMIN account</span></p>
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
        <h2>Change history <span class="muted">append-only audit trail</span></h2>` +
        (audit.length ? `<table><thead><tr><th>When</th><th>Changed by</th><th>Note</th><th>Rules</th><th></th></tr></thead><tbody>` +
          audit.map((c) => `<tr><td class="muted">${fmtDate(c.changed_at)}</td><td class="mono">${esc(c.changed_by.slice(0, 12))}…</td>
            <td>${esc(c.note || "—")}</td><td>${(c.before || []).length} → ${(c.after || []).length}</td>
            <td><details><summary class="mini">diff</summary><pre class="rule-diff">${esc(JSON.stringify(c.after, null, 1))}</pre></details></td></tr>`).join("") +
          `</tbody></table>` : `<p class="muted">No changes recorded yet.</p>`);
    } catch (e) { return err(e); }
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
          ["CODER", "Coder"], ["NURSE_PHYSICIAN", "Nurse/Physician"], ["ATTORNEY", "Attorney"],
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

  // #/team -- the one page a CASE_MANAGER has a standing right to (their
  // own tenant, enforced server-side by createTenantStaff/listTenantStaff),
  // independent of the Rules page's FEDERAL_ADMIN/PLATFORM_ADMIN-only gate.
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
          ["CODER", "Coder"], ["NURSE_PHYSICIAN", "Nurse/Physician"], ["ATTORNEY", "Attorney"],
          ["FINANCE", "Finance"], ["STATE_AUDITOR", "State Auditor"], ["PARTY", "Party"]] },
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
    const rowsHtml = (list) => list.map((e) => `<tr>
        <td class="muted">${fmtDate(e.created_at)}</td>
        <td>${e.case_id ? `<a href="#/cases/${esc(e.case_id)}">${esc(String(e.case_id).slice(0, 8))}…</a>` : `<span class="muted">—</span>`}</td>
        <td>${badge(e.action)}</td>
        <td class="mono" style="max-width:360px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${esc(e.payload || "")}">${esc(e.payload || "")}</td>
        <td class="mono" title="${esc(e.hash || "")}">${esc(String(e.hash || "").slice(0, 12))}…</td>
      </tr>`).join("");
    const { bodyHtml, pagerHtml } = initClientPager("audit", rows, rowsHtml);
    return `<table id="audit-tb"><thead><tr><th>When</th><th>Case</th><th>Action</th><th>Payload</th><th>Hash</th></tr></thead>
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
      const r = await Api.auditLog.list({ limit: 200 });
      afterRender(() => {
        $("#al-filter")?.addEventListener("submit", async (ev) => {
          ev.preventDefault();
          const f = Object.fromEntries(new FormData(ev.target));
          try {
            const rf = await Api.auditLog.list({ limit: 200, case_id: f.case_id, action: f.action });
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

  return { dashboard, cases, caseDetail, newDispute, sortCases, onboarding, onboardingNew, decide, voice, reports, showAnalysis, retryAnalysis, check, assign, letter, saveCurrentView, escalate, relate, feeTransfer, peek, askGraph, settleInvoice, qaQueue, qaReview, qaDecide, intake, newIntake, advanceIntake, deliverables, submitDeliverable, requestDeliverable, finance, payInvoice, moveDoc, downloadDoc, downloadZip, rulesAdmin, ruleEdit, ruleDelete, rulesSave, bindRulesAdmin, uploadCheck, clearCheck, requestRescan, copilotBrief, copilotDraftQA, copilotPropose, copilotDecideBatch, assistant, assistantChip, asstQaDecide, bulkIntakeFile, bulkIntakeSubmit, createTenantFlow, createFederalAdminFlow, addTenantStaffFlow, teamAdmin, setStaffEnabled, deleteStaffMember, auditLog, intakeMore, financeMore, opsDashboard };
})();
