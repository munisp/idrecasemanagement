// Author: Patrick Munis at newwave
// ui.js — Meridian interaction layer: modal dialogs, toasts with undo, confirm,
// busy-state runner. Replaces every window.prompt / alert / confirm with branded,
// keyboard-complete, screen-reader-announced surfaces.
const UI = (() => {
  // ---- Toasts -------------------------------------------------------------
  // kinds: success (default for completed actions), info, warn, error.
  // Iconed, animated in/out, hover-to-pause, capped stack, aria-live.
  let region;
  const ICONS = { success: "✓", info: "ℹ", warn: "⚠", error: "✕" };

  function toast(message, opts = {}) {
    if (!region) {
      region = document.createElement("div");
      region.className = "toast-region";
      region.setAttribute("aria-live", "polite");
      region.setAttribute("role", "status");
      document.body.appendChild(region);
    }
    // Cap the stack at 4 — drop the oldest so rapid actions stay readable.
    while (region.children.length >= 4) region.firstChild.remove();

    const kind = opts.kind || "success";
    const t = document.createElement("div");
    t.className = `toast t-${kind} toast-in`;
    t.innerHTML = `<span class="toast-icon" aria-hidden="true">${ICONS[kind] || ICONS.info}</span><span class="toast-msg">${message}</span>`;
    if (opts.undo) {
      const b = document.createElement("button");
      b.textContent = "Undo";
      b.onclick = () => { opts.undo(); dismiss(); };
      t.appendChild(b);
    }
    const x = document.createElement("button");
    x.className = "toast-x";
    x.setAttribute("aria-label", "Dismiss notification");
    x.textContent = "✕";
    x.onclick = () => dismiss();
    t.appendChild(x);
    region.appendChild(t);

    // sticky: no auto-dismiss — reserved for findings the user MUST see
    // (e.g. policy violations); dismissal is via the ✕ button only.
    const sticky = !!opts.sticky;
    let remaining = opts.duration || (kind === "error" || kind === "warn" ? 9000 : 4500);
    let timer = sticky ? null : setTimeout(dismiss, remaining);
    let started = Date.now();
    if (!sticky) {
      t.addEventListener("mouseenter", () => { clearTimeout(timer); remaining -= Date.now() - started; });
      t.addEventListener("mouseleave", () => { started = Date.now(); timer = setTimeout(dismiss, Math.max(remaining, 800)); });
    }

    let gone = false;
    function dismiss() {
      if (gone) return;
      gone = true;
      clearTimeout(timer);
      t.classList.remove("toast-in");
      t.classList.add("toast-out");
      setTimeout(() => t.remove(), 220);
    }
    return dismiss;
  }

  // ---- Busy runner ----------------------------------------------------------
  // UI.run(button, async fn) — disables the control, shows an inline spinner,
  // restores it afterwards (label preserved). Prevents double-submit and gives
  // immediate "something is happening" feedback on every async action.
  async function run(ctrl, fn, busyLabel) {
    if (!ctrl || ctrl.disabled) return;
    const isBtn = /^(BUTTON|A)$/.test(ctrl.tagName);
    const prevHTML = isBtn ? ctrl.innerHTML : null;
    ctrl.disabled = true;
    ctrl.classList.add("is-busy");
    if (isBtn) ctrl.innerHTML = `<span class="spin" aria-hidden="true"></span>${busyLabel || prevHTML}`;
    try { return await fn(); }
    finally {
      ctrl.disabled = false;
      ctrl.classList.remove("is-busy");
      if (isBtn) ctrl.innerHTML = prevHTML;
    }
  }

  // Flash an element (row, field) to confirm a change landed.
  function flash(el, cls = "flash-ok") {
    if (!el) return;
    el.classList.remove(cls);
    void el.offsetWidth; // restart animation
    el.classList.add(cls);
    setTimeout(() => el.classList.remove(cls), 1400);
  }

  // ---- Modal (focus-trapped, Esc closes, returns a promise) ----
  // fields: [{name, label, type, value, placeholder, required, options, hint}]
  // onRefresh(vals, setOptions): optional live hook — fires (debounced) as
  // the user types so a modal can offer search-as-you-type selects;
  // setOptions(name, [[value,label],...]) re-renders one select's options.
  function modal({ title, body = "", fields = [], submitLabel = "Save", danger = false, wide = false, onRefresh = null }) {
    return new Promise((resolve) => {
      const prev = document.activeElement;
      const scrim = document.createElement("div");
      scrim.className = "modal-scrim";
      scrim.innerHTML = `
        <div class="modal ${wide ? "modal-wide" : ""}" role="dialog" aria-modal="true" aria-label="${title}">
          <div class="modal-h"><h2>${title}</h2>
            <button class="icon-btn modal-x" aria-label="Close dialog">✕</button></div>
          <form class="modal-b" novalidate>
            ${body ? `<p class="modal-body">${body}</p>` : ""}
            ${fields.map((f) => `
              <label class="mfield">${f.label}${f.required ? ' <span class="req">*</span>' : ""}
                ${f.options
                  ? `<select name="${f.name}" ${f.required ? "required" : ""}>${f.options.map((o) =>
                      `<option value="${o[0]}" ${o[0] === f.value ? "selected" : ""}>${o[1]}</option>`).join("")}</select>`
                  : f.type === "textarea"
                    ? `<textarea name="${f.name}" rows="3" placeholder="${f.placeholder || ""}" ${f.required ? "required" : ""}>${f.value || ""}</textarea>`
                    : `<input name="${f.name}" type="${f.type || "text"}" value="${f.value || ""}"
                        placeholder="${f.placeholder || ""}" ${f.required ? "required" : ""} ${f.step ? `step="${f.step}"` : ""} />`}
                ${f.hint ? `<span class="mhint">${f.hint}</span>` : ""}
              </label>`).join("")}
            <div class="modal-f">
              <button type="button" class="btn-secondary m-cancel">Cancel</button>
              <button type="submit" class="${danger ? "btn-danger" : "btn-primary"}">${submitLabel}</button>
            </div>
          </form>
        </div>`;
      document.body.appendChild(scrim);
      const close = (val) => { scrim.remove(); prev?.focus?.(); resolve(val); };
      scrim.querySelector(".modal-x").onclick = () => close(null);
      scrim.querySelector(".m-cancel").onclick = () => close(null);
      scrim.addEventListener("mousedown", (e) => { if (e.target === scrim) close(null); });
      scrim.addEventListener("keydown", (e) => {
        if (e.key === "Escape") close(null);
        if (e.key === "Tab") { // focus trap
          const els = [...scrim.querySelectorAll("button,input,select,textarea")].filter((x) => !x.disabled);
          const first = els[0], last = els[els.length - 1];
          if (e.shiftKey && document.activeElement === first) { last.focus(); e.preventDefault(); }
          else if (!e.shiftKey && document.activeElement === last) { first.focus(); e.preventDefault(); }
        }
      });
      if (onRefresh) {
        const form = scrim.querySelector("form");
        const setOptions = (name, opts) => {
          const sel = form.querySelector(`select[name="${name}"]`);
          if (sel) sel.innerHTML = opts.map((o) => `<option value="${o[0]}">${o[1]}</option>`).join("");
        };
        let tmr;
        form.addEventListener("input", () => {
          clearTimeout(tmr);
          tmr = setTimeout(() => onRefresh(Object.fromEntries(new FormData(form)), setOptions), 300);
        });
      }
      scrim.querySelector("form").onsubmit = (e) => {
        e.preventDefault();
        const data = Object.fromEntries(new FormData(e.target));
        for (const f of fields) {
          if (f.required && !String(data[f.name] || "").trim()) {
            e.target.querySelector(`[name="${f.name}"]`).focus();
            toast(`Enter ${f.label.toLowerCase()} to continue.`, { kind: "warn" });
            return;
          }
        }
        close(data);
      };
      scrim.querySelector("input,select,textarea,button")?.focus();
    });
  }

  const confirm = (title, body, submitLabel = "Confirm", danger = false) =>
    modal({ title, body, submitLabel, danger }).then((v) => v !== null);

  return { toast, modal, confirm, run, flash };
})();

// Click-to-sort for every table in the portal. Sorts the rows already on the
// page; tables with their own server-side sort (th.sortable) or rows that span
// columns are left alone. A re-rendered table starts unsorted again.
const TableSort = (() => {
  const MONTH = "jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec";
  const DATE_RE = new RegExp(`^(\\d{4}-\\d{2}-\\d{2}|\\d{1,2}/\\d{1,2}/\\d{2,4}|(${MONTH})[a-z]*\\.? \\d{1,2},? \\d{4})`, "i");
  const text = (cell) => (cell ? (cell.dataset.sort ?? cell.textContent).trim() : "");
  const blank = (s) => s === "" || s === "—" || s === "-";
  function num(s) {
    const hm = s.match(/^(?:(\d+)h)?\s*(?:(\d+)m)?$/);
    if (hm && (hm[1] || hm[2])) return (+hm[1] || 0) * 60 + (+hm[2] || 0);
    if (!/^\(?-?\$?[\d,]*\.?\d+\)?%?$/.test(s)) return NaN;
    const n = parseFloat(s.replace(/[$,()%]/g, ""));
    return s.startsWith("(") ? -n : n;
  }
  const date = (s) => (DATE_RE.test(s) ? Date.parse(s) : NaN);

  function columnKind(rows, idx) {
    const vals = rows.map((r) => text(r.cells[idx])).filter((v) => !blank(v));
    if (!vals.length) return "text";
    if (vals.every((v) => !isNaN(num(v)))) return "num";
    if (vals.every((v) => !isNaN(date(v)))) return "date";
    return "text";
  }

  function apply(table) {
    const { idx, dir } = table._sort;
    const body = table.tBodies[0];
    const rows = Array.from(body.rows);
    const kind = columnKind(rows, idx);
    const key = kind === "num" ? num : kind === "date" ? date : null;
    rows.sort((a, b) => {
      const x = text(a.cells[idx]), y = text(b.cells[idx]);
      if (blank(x) || blank(y)) return blank(x) - blank(y); // blanks always last
      return dir * (key ? key(x) - key(y) : x.localeCompare(y, undefined, { numeric: true, sensitivity: "base" }));
    });
    rows.forEach((r) => body.appendChild(r));
    table._sortedRows = rows.length;
    table.querySelectorAll("thead th").forEach((th, i) => {
      th.classList.toggle("sorted", i === idx);
      th.setAttribute("aria-sort", i === idx ? (dir > 0 ? "ascending" : "descending") : "none");
      const a = th.querySelector(".sort-arrow");
      if (a) a.textContent = i === idx ? (dir > 0 ? " ↑" : " ↓") : " ↕";
    });
  }

  function enhance(table) {
    const head = table.tHead, body = table.tBodies[0];
    if (!head || !body || !head.rows.length) return;
    if (table._sort && body.rows.length !== table._sortedRows) apply(table); // rows changed under a sorted table
    if (table.dataset.csort) return;
    if (head.querySelector("th.sortable") || table.querySelector("td[colspan], th[colspan]")) return;
    table.dataset.csort = "1";
    Array.from(head.rows[0].cells).forEach((th, idx) => {
      if (!th.textContent.trim()) return; // action columns
      th.classList.add("sortable");
      th.tabIndex = 0;
      th.title = "Sort by " + th.textContent.trim();
      th.insertAdjacentHTML("beforeend", '<span class="sort-arrow"> ↕</span>');
      const go = () => {
        const same = table._sort && table._sort.idx === idx;
        // First click: newest/largest first for dates and numbers, A→Z for text.
        const first = columnKind(Array.from(body.rows), idx) === "text" ? 1 : -1;
        table._sort = { idx, dir: same ? -table._sort.dir : first };
        apply(table);
      };
      th.addEventListener("click", go);
      th.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); go(); } });
    });
  }

  let queued = false;
  const scan = () => { queued = false; document.querySelectorAll("table").forEach(enhance); };
  new MutationObserver(() => { if (!queued) { queued = true; setTimeout(scan, 50); } })
    .observe(document.body, { childList: true, subtree: true });
  return { scan };
})();

// Pages for every table in the portal: a "Rows per page" choice (default 100,
// remembered per browser) with Previous / Next.
//
// Two kinds of table share the one bar:
//   - Tables whose rows are all on the page already are paged here, in the
//     browser, automatically -- nothing to wire.
//   - Lists the server pages (disputes, CRM, intake, mail log, payments,
//     checks) sit inside [data-serverpaged] and call Pager.server(), which
//     fetches the next page instead of hiding rows.
const Pager = (() => {
  const SIZES = [25, 50, 100, 250, 500];
  const KEY = "idre.pageSize";
  function size() {
    try { const n = Number(localStorage.getItem(KEY)); if (SIZES.includes(n)) return n; } catch {}
    return 100;
  }
  function setSize(n) {
    try { localStorage.setItem(KEY, String(n)); } catch {}
    document.dispatchEvent(new CustomEvent("idre:pagesize", { detail: n }));
    scan();
  }
  // from/to are 1-based row positions; total < 0 means "unknown".
  function barHtml(from, to, total, page, pages) {
    const sz = size();
    return `<label class="muted">Rows per page
        <select class="tp-size" aria-label="Rows per page">${SIZES.map((n) => `<option ${n === sz ? "selected" : ""}>${n}</option>`).join("")}</select></label>
      <span class="muted">${total ? `${from}–${to} of ${total}` : "0 rows"}</span>
      <span class="tp-nav"><button type="button" class="mini tp-prev" ${page <= 0 ? "disabled" : ""}>‹ Previous</button>
      <span class="muted">Page ${page + 1} of ${Math.max(1, pages)}</span>
      <button type="button" class="mini tp-next" ${page >= pages - 1 ? "disabled" : ""}>Next ›</button></span>`;
  }
  function wire(bar, onPage) {
    bar.querySelector(".tp-size").onchange = (e) => setSize(Number(e.target.value));
    bar.querySelector(".tp-prev").onclick = () => onPage(-1);
    bar.querySelector(".tp-next").onclick = () => onPage(1);
  }

  // ---- in-browser paging ----------------------------------------------------
  // A row that only labels a group (class "...-head") travels with the row
  // after it; a hidden detail row (class "row-detail") travels with the row
  // before it. Neither counts as a record.
  function units(body) {
    const out = [];
    let heads = [];
    for (const tr of body.rows) {
      if (tr.classList.contains("row-detail") && out.length) { out[out.length - 1].push(tr); continue; }
      if (/(^|\s)\S*-head(\s|$)/.test(tr.className)) { heads.push(tr); continue; }
      out.push(heads.concat(tr));
      heads = [];
    }
    if (heads.length && out.length) out[out.length - 1].push(...heads);
    return out;
  }
  const bars = new Set();
  function dropBar(table) {
    if (table._pgBar) { table._pgBar.remove(); bars.delete(table._pgBar); table._pgBar = null; }
  }
  function apply(table) {
    const body = table.tBodies[0];
    if (!table.tHead || !body || table.dataset.nopage !== undefined || table.closest("[data-serverpaged]")) return;
    const us = units(body);
    const total = us.length;
    if (total <= SIZES[0]) { // fits on the smallest page: nothing to page
      if (table._pgBar) { us.forEach((u) => u.forEach((tr) => { if (tr.hidden) tr.hidden = false; })); dropBar(table); }
      return;
    }
    const sz = size();
    const pages = Math.ceil(total / sz);
    const st = table._pg || (table._pg = { page: 0, total });
    if (st.total !== total) { st.page = 0; st.total = total; } // filtered or reloaded: back to the first page
    st.page = Math.max(0, Math.min(st.page, pages - 1));
    const start = st.page * sz, end = Math.min(total, start + sz);
    us.forEach((u, i) => { const hide = i < start || i >= end; u.forEach((tr) => { if (tr.hidden !== hide) tr.hidden = hide; }); });
    let bar = table._pgBar;
    if (!bar || !bar.isConnected) {
      bar = document.createElement("div");
      bar.className = "pager tp";
      bar._table = table;
      table._pgBar = bar;
      bars.add(bar);
      (table.closest(".dg-wrap") || table).insertAdjacentElement("afterend", bar);
    }
    const sig = `${start}|${end}|${total}|${sz}`;
    if (bar._sig !== sig) {
      bar._sig = sig;
      bar.innerHTML = barHtml(start + 1, end, total, st.page, pages);
      wire(bar, (d) => { st.page += d; apply(table); table.scrollIntoView({ block: "nearest" }); });
    }
  }
  let queued = false;
  function scan() {
    queued = false;
    bars.forEach((b) => { if (!b._table.isConnected) { b.remove(); bars.delete(b); } });
    document.querySelectorAll("table").forEach(apply);
  }
  new MutationObserver(() => { if (!queued) { queued = true; setTimeout(scan, 60); } })
    .observe(document.body, { childList: true, subtree: true });

  // ---- server paging ----------------------------------------------------------
  // host: the element that holds the bar. first: {total, shown} of the page
  // already rendered. load(limit, offset, page) fetches and renders one page
  // and resolves {total, shown}. Changing the page size reloads from page 1.
  function server(host, first, load) {
    if (!host) return;
    const st = { page: 0, total: first.total, shown: first.shown, busy: false };
    const draw = () => {
      const sz = size();
      const from = st.page * sz + 1;
      host.className = "pager tp";
      host.innerHTML = barHtml(from, from + st.shown - 1, st.total, st.page, Math.ceil(st.total / sz));
      wire(host, (d) => go(st.page + d));
    };
    async function go(page) {
      if (st.busy || page < 0) return;
      st.busy = true;
      host.querySelectorAll("button, select").forEach((el) => (el.disabled = true));
      try {
        const sz = size();
        const r = await load(sz, page * sz, page);
        st.page = page; st.total = r.total; st.shown = r.shown;
      } catch (e) { UI.toast(e.message, { kind: "warn" }); }
      st.busy = false;
      draw();
    }
    const onSize = () => { if (host.isConnected) go(0); else document.removeEventListener("idre:pagesize", onSize); };
    document.addEventListener("idre:pagesize", onSize);
    draw();
  }
  return { size, server, scan, SIZES };
})();
