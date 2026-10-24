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
  function modal({ title, body = "", html = "", fields = [], submitLabel = "Save", danger = false, wide = false, onRefresh = null }) {
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
            ${html ? `<div class="modal-body">${html}</div>` : ""}
            ${fields.map((f) => `
              <label class="mfield"><span>${f.label}${f.required ? ' <span class="req">*</span>' : ""}</span>
                ${f.options
                  ? `<select name="${f.name}" ${f.required ? "required" : ""}>${f.options.map((o) =>
                      `<option value="${o[0]}" ${o[0] === f.value ? "selected" : ""}>${o[1]}</option>`).join("")}</select>`
                  : f.type === "textarea"
                    ? `<textarea name="${f.name}" rows="3" placeholder="${f.placeholder || ""}" ${f.required ? "required" : ""}>${f.value || ""}</textarea>`
                    : `<input name="${f.name}" type="${f.type || "text"}" value="${f.value || ""}"
                        placeholder="${f.placeholder || ""}" ${f.required ? "required" : ""} ${f.step ? `step="${f.step}"` : ""}
                        ${f.min ? `min="${f.min}"` : ""} ${f.max ? `max="${f.max}"` : ""} ${f.accept ? `accept="${f.accept}"` : ""} />`}
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
          const missing = f.type === "file" ? !(data[f.name] instanceof File && data[f.name].size > 0) : !String(data[f.name] || "").trim();
          if (f.required && missing) {
            e.target.querySelector(`[name="${f.name}"]`).focus();
            toast(`Enter ${f.label.toLowerCase()} to continue.`, { kind: "warn" });
            return;
          }
          // A date outside its range (e.g. one in the future for something that has already happened).
          const dv = String(data[f.name] || "");
          if (f.type === "date" && dv && ((f.max && dv > f.max) || (f.min && dv < f.min))) {
            e.target.querySelector(`[name="${f.name}"]`).focus();
            toast(f.max && dv > f.max ? `${f.label} can't be later than ${f.max}.` : `${f.label} can't be earlier than ${f.min}.`, { kind: "warn" });
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

// Pages for every table in the portal: a "Rows per page" choice (default 25,
// remembered per browser) with Previous / Next.
//
// Two kinds of table share the one bar:
//   - Tables whose rows are all on the page already are paged here, in the
//     browser, automatically -- nothing to wire.
//   - Lists the server pages (disputes, CRM, intake, mail log, payments,
//     checks) sit inside [data-serverpaged] and call Pager.server(), which
//     fetches the next page instead of hiding rows.
// A table may set its own page size (data-page-size="10": small lists such as the Team page's cards); its bar then
// has no rows-per-page choice. A row a page's own filter hides carries .flt-out and is not counted or shown.
// Search box for a list the SERVER pages (?q=): it sits outside the list's box, so the box can reload under it
// without the box losing focus. SrvSearch.html(id, placeholder) renders it; SrvSearch.on(id, reload) is called
// (debounced) with the term; SrvSearch.q(id) is the current term for the list's own fetches.
const SrvSearch = (() => {
  const terms = new Map(), handlers = new Map();
  let tmr;
  document.addEventListener("input", (e) => {
    const el = e.target.closest && e.target.closest("input[data-srvsearch]");
    if (!el) return;
    clearTimeout(tmr);
    tmr = setTimeout(() => {
      const id = el.dataset.srvsearch;
      const v = el.value.trim();
      if ((terms.get(id) || "") === v) return;
      v ? terms.set(id, v) : terms.delete(id);
      typing = { id, at: Date.now() };
      const h = handlers.get(id);
      if (h) h(v);
    }, 300);
  });
  // A list that re-renders its whole page re-creates the box: put the cursor back where the person was typing.
  let typing = null;
  new MutationObserver(() => {
    if (!typing || Date.now() - typing.at > 4000) return;
    const el = document.querySelector(`input[data-srvsearch="${typing.id}"]`);
    if (el && document.activeElement !== el) { el.focus(); el.setSelectionRange(el.value.length, el.value.length); }
  }).observe(document.body, { childList: true, subtree: true });
  const esc = (x) => String(x).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  return {
    html: (id, placeholder) => `<div class="tsearch"><input type="search" data-srvsearch="${id}" value="${esc(terms.get(id) || "")}"
      placeholder="${esc(placeholder || "Search…")}" aria-label="${esc(placeholder || "Search")}" maxlength="100" /></div>`,
    on: (id, fn) => handlers.set(id, fn),
    q: (id) => terms.get(id) || "",
    params: (id) => (terms.get(id) ? { q: terms.get(id) } : {}),
  };
})();

const Pager = (() => {
  const SIZES = [25, 50, 100, 250, 500];
  const KEY = "idre.pageSize";
  function size() {
    try { const n = Number(localStorage.getItem(KEY)); if (SIZES.includes(n)) return n; } catch {}
    return 25;
  }
  function setSize(n) {
    try { localStorage.setItem(KEY, String(n)); } catch {}
    document.dispatchEvent(new CustomEvent("idre:pagesize", { detail: n }));
    scan();
  }
  // from/to are 1-based row positions; total < 0 means "unknown".
  function barHtml(from, to, total, page, pages, fixed) {
    const sz = size();
    return `${fixed ? "" : `<label class="muted">Rows per page
        <select class="tp-size" aria-label="Rows per page">${SIZES.map((n) => `<option ${n === sz ? "selected" : ""}>${n}</option>`).join("")}</select></label>`}
      <span class="muted">${total ? `${from}–${to} of ${total}` : "0 rows"}</span>
      <span class="tp-nav"><button type="button" class="mini tp-prev" ${page <= 0 ? "disabled" : ""}>‹ Previous</button>
      <span class="muted">Page ${page + 1} of ${Math.max(1, pages)}</span>
      <button type="button" class="mini tp-next" ${page >= pages - 1 ? "disabled" : ""}>Next ›</button></span>`;
  }
  function wire(bar, onPage) {
    const sel = bar.querySelector(".tp-size");
    if (sel) sel.onchange = (e) => setSize(Number(e.target.value));
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
      if (tr.classList.contains("flt-out")) continue; // filtered out by the page itself: not a record on any page
      if (tr.classList.contains("row-detail") && out.length) { out[out.length - 1].push(tr); continue; }
      if (/(^|\s)\S*-head(\s|$)/.test(tr.className)) { heads.push(tr); continue; }
      out.push(heads.concat(tr));
      heads = [];
    }
    if (heads.length && out.length) out[out.length - 1].push(...heads);
    return out;
  }
  // ---- search: every list of SEARCH_MIN+ records gets a box above it --------
  // Matching records stay; the rest carry .sf-out (display:none), which never touches `hidden`, so paging and a
  // page's own collapsed detail rows are left alone. Paging then runs over the matches only. The term is kept per
  // page + table, so a list that reloads after an action keeps its search. Opt out: data-nosearch on the table (or
  // an ancestor) -- for lists that already have their own search.
  const SEARCH_MIN = 8;
  const queries = new Map();
  const norm = (s) => String(s || "").normalize("NFD").replace(/[\u0300-\u036f]/g, "").toLowerCase();
  const tableKey = (table) => (location.hash.split("?")[0] || "#") + "|" +
    (table.id || "t" + [...document.querySelectorAll("table")].indexOf(table));
  function dropSearch(table) {
    if (table._sfBar) { table._sfBar.remove(); table._sfBar = null; }
    table.querySelectorAll("tr.sf-out").forEach((tr) => tr.classList.remove("sf-out"));
  }
  function ensureSearch(table) {
    if (table._sfBar && table._sfBar.isConnected) return;
    const key = tableKey(table);
    table._sfKey = key;
    table._q = queries.get(key) || "";
    const bar = document.createElement("div");
    bar.className = "tsearch";
    bar.innerHTML = `<input type="search" placeholder="Search this list…" aria-label="Search this list" maxlength="120" />
      <span class="muted tsearch-n" aria-live="polite"></span>`;
    const input = bar.querySelector("input");
    input.value = table._q;
    let tmr;
    input.addEventListener("input", () => {
      clearTimeout(tmr);
      tmr = setTimeout(() => {
        table._q = input.value.trim();
        table._q ? queries.set(key, table._q) : queries.delete(key);
        if (table._pg) table._pg.page = 0;
        apply(table);
      }, 150);
    });
    input.addEventListener("keydown", (e) => { if (e.key === "Escape" && input.value) { input.value = ""; input.dispatchEvent(new Event("input")); } });
    table._sfBar = bar;
    (table.closest(".dg-wrap,.tbl-auto") || table).insertAdjacentElement("beforebegin", bar);
  }
  // records that match the table's search term (all of them when there is none); the rest are marked out
  function searchUnits(table, all) {
    if (all.length < SEARCH_MIN || table.closest("[data-nosearch], .modal")) { dropSearch(table); return all; }
    ensureSearch(table);
    const terms = norm(table._q).split(/\s+/).filter(Boolean);
    const out = [];
    for (const u of all) {
      const ok = !terms.length || terms.every((t) => {
        const r0 = u[u.length - 1]; // the record's own row caches the unit's text
        const txt = r0._sfTxt || (r0._sfTxt = norm(u.map((tr) => tr.textContent).join(" ")));
        return txt.includes(t);
      });
      u.forEach((tr) => tr.classList.toggle("sf-out", !ok));
      if (ok) out.push(u);
    }
    const n = table._sfBar.querySelector(".tsearch-n");
    const msg = terms.length ? (out.length ? `${out.length} of ${all.length}` : `No match in ${all.length}`) : "";
    if (n.textContent !== msg) n.textContent = msg;
    return out;
  }

  const bars = new Set();
  function dropBar(table) {
    if (table._pgBar) { table._pgBar.remove(); bars.delete(table._pgBar); table._pgBar = null; }
  }
  function apply(table) {
    const body = table.tBodies[0];
    if (!table.tHead || !body || table.closest("[data-serverpaged]")) return;
    const us = searchUnits(table, units(body));
    if (table.dataset.nopage !== undefined) return;
    const total = us.length;
    const own = Number(table.dataset.pageSize) || 0;
    if (total <= (own || SIZES[0])) { // fits on one page: nothing to page
      if (table._pgBar) { us.forEach((u) => u.forEach((tr) => { if (tr.hidden) tr.hidden = false; })); dropBar(table); }
      return;
    }
    const sz = own || size();
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
      (table.closest(".dg-wrap,.tbl-auto") || table).insertAdjacentElement("afterend", bar);
    }
    const sig = `${start}|${end}|${total}|${sz}`;
    if (bar._sig !== sig) {
      bar._sig = sig;
      bar.innerHTML = barHtml(start + 1, end, total, st.page, pages, !!own);
      wire(bar, (d) => { st.page += d; apply(table); table.scrollIntoView({ block: "nearest" }); });
    }
  }
  let queued = false;
  function scan() {
    queued = false;
    bars.forEach((b) => { if (!b._table.isConnected) { b.remove(); bars.delete(b); } });
    document.querySelectorAll(".tsearch").forEach((b) => { if (!b.nextElementSibling || !(b.nextElementSibling.matches("table") || b.nextElementSibling.querySelector("table"))) b.remove(); });
    document.querySelectorAll("table").forEach((t) => { contain(t); apply(t); });
  }
  // A wide table scrolls inside its own box: wrap any table not already in a scrolling container (or opted out).
  function contain(table) {
    if (table._contained || !table.parentElement) return;
    table._contained = true;
    if (table.closest(".dg-wrap,.tbl-wrap,.tbl-scroll,.tbl-auto,[data-nocontain],.modal")) return;
    const box = document.createElement("div");
    box.className = "tbl-auto";
    table.parentElement.insertBefore(box, table);
    box.appendChild(table);
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

// Building blocks for list pages, so every list reads the same way: a row of summary figures, segmented filters
// (an option can carry its count), and a toolbar. Plain strings of HTML, like the rest of the views.
const ListUI = (() => {
  const esc = (x) => String(x ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  const fmt = (n) => (typeof n === "number" ? n.toLocaleString() : esc(n));
  return {
    // tiles: [{label, value, sub?, tone?: "warn"|"bad"|"good"}]
    summary: (tiles) => `<div class="sum-row">${tiles.filter(Boolean).map((t) =>
      `<div class="sum-tile${t.tone ? " is-" + t.tone : ""}"><span class="sum-l">${esc(t.label)}</span><span class="sum-n">${fmt(t.value)}</span>${t.sub ? `<span class="sum-s">${esc(t.sub)}</span>` : ""}</div>`).join("")}</div>`,
    // items: [{value, label, count?, href?}] -- an href makes the option a link (filters kept in the address),
    // otherwise onclick calls `fn('<value>')`.
    segs: (label, items, current, fn) => `<div class="segs" role="group" aria-label="${esc(label)}">${items.map((it) => {
      const on = (it.value || "") === (current || "");
      const n = it.count == null ? "" : `<span class="seg-n">${fmt(it.count)}</span>`;
      return it.href
        ? `<a class="seg ${on ? "on" : ""}" href="${esc(it.href)}" aria-current="${on}">${esc(it.label)}${n}</a>`
        : `<button type="button" class="seg ${on ? "on" : ""}" aria-pressed="${on}" onclick="${fn}('${esc(it.value || "")}')">${esc(it.label)}${n}</button>`;
    }).join("")}</div>`,
  };
})();
window.ListUI = ListUI;

// Tips: what a column, chip or figure means, on hover or keyboard focus, across the whole portal.
// Three sources, in order: an element's own data-tip; the glossary below, matched on its text (table headers,
// status and lane chips, summary labels); and a few patterns ("41bd left"). Known headers get a dotted underline
// so people can tell there is something to read. Plain text only; a tip never blocks a click.
const Tips = (() => {
  const G = {
    // ---- disputes / case columns
    "LANE": "Triage lane — how much review a dispute needs. Auto-review: simple and low-value, decided on the documents. Standard: the normal review. Complex: high value, many claims or escalated, so it gets the most review.",
    "SLA": "Business days left on the statutory clock for a decision (bd = business days). Amber: a week or less. Red: 3 days or less, or overdue.",
    "CLOCK": "The nearest program deadline on the case — for example the plan's response or the determination — and how long is left.",
    "DISPUTED": "The amount in dispute: what was billed minus what the plan paid, across the claims in this dispute.",
    "QPA": "Qualifying payment amount — the plan's median contracted rate for the service, the federal benchmark for out-of-network payment.",
    "STATUS": "Where the item stands now. Hover a status chip for what it means.",
    "ASSIGNED": "The Reviewer who owns the case. An unassigned case waits for the PM's rotation or for someone to take it.",
    "ASSIGNEE": "The person the item is assigned to.",
    "SERVICE": "The service line of the disputed claims, e.g. emergency, anesthesia, radiology, lab.",
    "OPENED": "When the dispute was filed.",
    "CASE #": "The program's case number. Click a row to open the case.",
    "FILED": "When the dispute was filed.",
    "FEE": "The initial review fee for this dispute.",
    "PAYMENT": "Whether the initial fee has been paid.",
    "RESPONSE DUE": "The last day to send your response. After it, the review goes ahead with what is on file.",
    "FILING PARTY": "Who filed the dispute: the provider, or the health plan.",
    "OUTREACH": "When the filer was sent the submission instructions and upload link.",
    "PACKET COMPLETE": "When the filing packet was complete, which starts the review clock.",
    "REVIEW": "The specialist review's own status: Requested (waiting to be claimed), In review, or Complete.",
    "REQUESTED": "When the review was asked for.",
    // ---- money
    "COURTESY": "The check amount written in figures.",
    "LEGAL (ICR)": "The check amount written in words, read by handwriting recognition. It must match the figures.",
    "CONF": "How sure the reader is of what it read off the check: high, medium or low.",
    "MATCH": "The invoice the check was matched to.",
    "ROUTING · ACCOUNT": "The bank routing and account numbers read from the check's MICR line.",
    "BILL TO": "Which party the fee was billed to.",
    "PAID VIA": "How the fee was paid: by card through Stripe, or by check / ACH / another way.",
    "REFUND ORDERED": "When the workflow closed the case for a refund (no complete filing packet 7 days after outreach).",
    "WAITING": "Days since the refund was ordered.",
    "SETTLED": "Payments that went through.",
    "PENDING": "Payments started and not yet confirmed.",
    "FAILED / EXPIRED": "Payments that failed, or checkouts left unpaid until they expired.",
    "REFUNDED": "Money returned to the payer.",
    "DIRECTION": "In: money received. Out: money paid back.",
    "STRIPE REF": "Stripe's reference for the payment (payment intent or checkout session).",
    // ---- chips: lanes, statuses
    "COMPLEX": "Complex lane — high value, many claims or escalated; gets the most review.",
    "STANDARD": "Standard lane — the normal review.",
    "AUTO_REVIEW": "Auto-review lane — simple and low-value, decided on the documents.",
    "AUTO-REVIEW": "Auto-review lane — simple and low-value, decided on the documents.",
    "AWAITING_PAYMENT": "The initial fee isn't paid yet. Nothing but correspondence happens on the case until it is.",
    "PENDING_INTAKE": "Filed and paid; waiting for the filing packet and documents before the review starts.",
    "CLOSED-REFUNDED": "Closed because no complete filing packet arrived within 7 days of outreach; the initial fee is refunded.",
    "REFUND_PENDING": "A card refund was sent to Stripe and is waiting for Stripe to confirm it.",
    "IN_REVIEW": "Being reviewed.",
    "COMPLETE": "Done.",
    "OPEN": "Not paid or not done yet.",
    "PAID": "Paid in full.",
    "VOID": "Cancelled; nothing is owed.",
    "MATCHED": "Matched to an invoice; waiting for a person to confirm the funds cleared.",
    "CLEARED": "The funds cleared; the invoice is settled.",
    "UNASSIGNED": "Nobody owns this yet.",
    // ---- summary labels
    "COLLECTED": "Fee money received: card payments, cleared checks and fees recorded as paid by hand. Gross — refunds are shown separately, and money that pays no fee is under Not applied.",
    "LAST 30 DAYS": "Fee money received in the last 30 days.",
    "NOT APPLIED": "Money received that pays no fee: a check for more than its fee, or a payment for a fee already paid. Refund it or apply it to another fee.",
    "OUTSTANDING": "Open invoices not yet paid.",
    "OVERDUE": "Open invoices past their due date.",
    "STAGES IN USE": "Stages that have at least one dispute in them.",
    "AWAITING REVIEW": "Drafts held at the QA gate until the named role approves them.",
    "WAITING FOR A REVIEWER": "Paid disputes nobody is assigned to yet. The PM's rotation gives them out in turn; unpaid filings are not counted — they are given out once their fee is paid.",
    "AWAITING PAYMENT": "Filed, but the initial fee isn't paid yet. Nothing but correspondence happens on these until it is.",
    "READY TO SEND": "Approved drafts back with the person who drafted them, to check the addresses and send.",
  };
  const PATTERNS = [
    [/^(\d+)\s*bd left$/i, (m) => `${m[1]} business days left on the statutory clock.`],
    [/^(\d+)\s*bd overdue$/i, (m) => `The statutory clock ran out ${m[1]} business days ago.`],
    [/^(\d+)d left$/i, (m) => `${m[1]} business days left on the statutory clock.`],
    [/^due today$/i, () => "The statutory clock runs out today."],
  ];
  const norm = (t) => String(t || "").replace(/[↑↓↕]/g, "").replace(/\s+/g, " ").trim().toUpperCase();
  function lookup(el) {
    if (el.dataset.tip) return el.dataset.tip;
    const text = (el.matches("th") ? el.childNodes[0]?.textContent || el.textContent : el.textContent) || "";
    const key = norm(text);
    let tip = G[key] || "";
    if (!tip) for (const [re, f] of PATTERNS) { const m = text.trim().replace(/^◷\s*/, "").match(re); if (m) { tip = f(m); break; } }
    // A native title becomes the same tip (one look everywhere, and no second browser tooltip on top of ours).
    // A detailed title (a clock's name, rule and day basis) says more than a generic pattern does.
    if (el.title) { if (!tip || (!G[key] && el.title.length > 40)) tip = el.title; el.removeAttribute("title"); }
    if (tip) el.dataset.tip = tip;
    return tip;
  }
  const SEL = "[data-tip], th, .badge, .chip, .kc-tag, .sla, .kc-sla, .sum-l, .ps-l, .fin-kpi-l";
  let box = null, cur = null;
  function show(el) {
    const tip = lookup(el);
    if (!tip) return;
    if (!box) { box = document.createElement("div"); box.className = "tip"; box.setAttribute("role", "tooltip"); box.id = "tip"; document.body.appendChild(box); }
    box.textContent = tip;
    box.style.display = "block";
    const r = el.getBoundingClientRect(), b = box.getBoundingClientRect();
    let top = r.bottom + 8, left = Math.min(Math.max(8, r.left + r.width / 2 - b.width / 2), innerWidth - b.width - 8);
    if (top + b.height > innerHeight - 8) top = r.top - b.height - 8;
    box.style.top = top + "px"; box.style.left = left + "px";
    el.setAttribute("aria-describedby", "tip");
    cur = el;
  }
  function hide() {
    if (box) box.style.display = "none";
    if (cur) cur.removeAttribute("aria-describedby");
    cur = null;
  }
  let timer;
  document.addEventListener("mouseover", (e) => {
    const el = e.target.closest && e.target.closest(SEL);
    if (el === cur) return;
    clearTimeout(timer); hide();
    if (el) timer = setTimeout(() => show(el), 250);
  });
  document.addEventListener("focusin", (e) => { const el = e.target.closest && e.target.closest(SEL); if (el) { hide(); show(el); } });
  document.addEventListener("focusout", hide);
  addEventListener("scroll", hide, true);
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") hide(); });
  // Mark the headers that have something to say (dotted underline), as pages draw.
  function mark() {
    document.querySelectorAll("th:not([data-tipmark])").forEach((th) => {
      th.dataset.tipmark = "1";
      if (lookup(th)) th.classList.add("has-tip");
    });
  }
  new MutationObserver(() => { clearTimeout(mark._t); mark._t = setTimeout(mark, 120); }).observe(document.body, { childList: true, subtree: true });
  return { glossary: G };
})();
window.Tips = Tips;
