// caselayout.js — the shape of the case page: a header that says what the dispute is and where it stands, one
// "next step" banner, and tabs by what people come to do. views.js builds the sections; this places them.
//
//   Overview      the steps, deadlines, notes
//   Documents     what was asked for and what arrived
//   Review        eligibility, case information, the summaries and the decision report
//   Letters & email   writing to the parties, generating letters, secure links
//   Billing       invoices, claim lines, money and time
//   Activity      what happened, calls, inquiries, related disputes
//
// A program section says where it goes with data-tab; one without goes to Review. Sections keep their ids, so
// a step button anywhere can open its section (reveal switches to the tab first).
const CaseLayout = (() => {
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmtDate = (d) => (d ? new Date(/^\d{4}-\d{2}-\d{2}$/.test(d) ? d + "T12:00:00Z" : d).toLocaleDateString("en-US", { dateStyle: "medium" }) : "—");
  const usd = (c) => (c == null ? "—" : (Number(c) / 100).toLocaleString("en-US", { style: "currency", currency: "USD" }));
  const sentence = (s) => { const t = String(s || "").replace(/_/g, " ").toLowerCase(); return t.charAt(0).toUpperCase() + t.slice(1); };
  const TABS = [["overview", "Overview"], ["documents", "Documents"], ["review", "Review"], ["mail", "Letters & email"], ["billing", "Billing"], ["activity", "Activity"]];
  const TAB_KEY = "idre.case.tab";
  const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

  // ---- header ----------------------------------------------------------------------------------------------
  function header(c, prog, clocks) {
    const d = c.details || {};
    const programmed = !!(prog && prog.config);
    const provider = d.provider_org || (d.filing_party_type !== "HEALTH_PLAN" ? d.requester_org : "") || "Provider not named yet";
    const plan = d.health_plan || (d.filing_party_type === "HEALTH_PLAN" ? d.requester_org : "") || "Health plan not named yet";
    const status = programmed ? (c.internal_status || sentence(c.status)) : sentence(c.status);
    const amount = programmed ? c.disputed_amount_cents : c.qpa_cents;
    const facts = [
      [programmed ? "In dispute" : "QPA", usd(amount)],
      ["Claims", c.num_claims || d.claims_submitted || "—"],
      ...(d.line_of_business ? [["Line of business", d.line_of_business]] : []),
      ["Opened", fmtDate(c.opened_at)],
    ];
    const chips = (clocks || []).filter((cl) => cl.state !== "done" && cl.remaining !== undefined).map((cl) => {
      const late = cl.remaining < 0;
      const txt = late ? `${Math.abs(cl.remaining)} days overdue` : cl.remaining === 0 ? "due today" : `${cl.remaining} days left`;
      return `<span class="case-clock ${late ? "late" : cl.remaining <= 3 ? "soon" : ""}" title="${esc(cl.cite || "")} — due ${esc(cl.due || "")}">
        <b>${esc(txt)}</b> ${esc(cl.label)}</span>`;
    }).join("");
    return `<header class="case-head">
      <div class="case-head-top">
        <div class="case-title"><h1 class="mono">${esc(c.case_number)}</h1>
          <span class="case-status">${esc(status)}</span>
          ${programmed && c.agency_status ? `<span class="muted" title="What the Agency sees">Agency: ${esc(c.agency_status)}</span>` : ""}</div>
        <!--case-more-->
      </div>
      <p class="case-parties"><b>${esc(provider)}</b> <span class="muted">v.</span> <b>${esc(plan)}</b>
        ${d.filing_party_type ? `<span class="chip">Filed by the ${d.filing_party_type === "HEALTH_PLAN" ? "health plan" : "provider"}</span>` : ""}</p>
      <dl class="case-facts">${facts.map(([k, v]) => `<div><dt>${esc(k)}</dt><dd>${esc(v)}</dd></div>`).join("")}
        <div><dt>Assigned to</dt><dd id="case-assignee" data-case="${esc(c.id)}"><span class="muted">…</span></dd></div></dl>
      ${chips ? `<div class="case-clocks">${chips}</div>` : ""}
    </header>`;
  }

  // The More menu: occasional actions, grouped, each with an icon and a line saying what it does.
  // acts: [label, run, {group, hint, icon}] — the third part is optional (ungrouped, no hint).
  const MORE_ICON = {
    assign: '<path d="M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z"/><path d="M4 20a8 8 0 0 1 16 0"/>',
    review: '<circle cx="11" cy="11" r="6"/><path d="m20 20-4.5-4.5"/>',
    escalate: '<path d="M5 21V4"/><path d="M5 4h11l-2 4 2 4H5"/>',
    link: '<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>',
    withdraw: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5"/><path d="M21 12H9"/>',
    inbox: '<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.5 5h13l3.5 7v6a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2v-6Z"/>',
    phone: '<path d="M22 16.9v3a2 2 0 0 1-2.2 2 19.8 19.8 0 0 1-8.6-3.1 19.5 19.5 0 0 1-6-6A19.8 19.8 0 0 1 2.1 4.2 2 2 0 0 1 4.1 2h3a2 2 0 0 1 2 1.7c.1 1 .4 1.9.7 2.8a2 2 0 0 1-.5 2.1L8 9.9a16 16 0 0 0 6 6l1.3-1.3a2 2 0 0 1 2.1-.5c.9.3 1.8.6 2.8.7a2 2 0 0 1 1.7 2Z"/>',
    money: '<path d="M12 2v20"/><path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/>',
    transfer: '<path d="M7 17h14"/><path d="m17 13 4 4-4 4"/><path d="M17 7H3"/><path d="m7 3-4 4 4 4"/>',
    letter: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8Z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h5"/>',
    dot: '<circle cx="12" cy="12" r="3"/>',
  };
  const moreIcon = (k) => `<svg class="case-more-ico" viewBox="0 0 24 24" aria-hidden="true">${MORE_ICON[k] || MORE_ICON.dot}</svg>`;
  function moreMenu(acts) {
    if (!acts.length) return "";
    const groups = [];
    acts.forEach((a, i) => {
      const meta = a[2] || {};
      const name = meta.group || "";
      let g = groups.find((x) => x.name === name);
      if (!g) groups.push(g = { name, items: [] });
      g.items.push({ i, label: a[0], hint: meta.hint || "", icon: meta.icon || "dot" });
    });
    // The same order for everyone, whichever actions their role has.
    const ORDER = ["Case", "Record what happened", "Money", "Federal IDR"];
    const rank = (n) => (ORDER.includes(n) ? ORDER.indexOf(n) : ORDER.length);
    groups.sort((a, b) => rank(a.name) - rank(b.name));
    const body = groups.map((g) => `<div class="case-more-group" role="group"${g.name ? ` aria-label="${esc(g.name)}"` : ""}>
        ${g.name ? `<div class="case-more-head">${esc(g.name)}</div>` : ""}
        ${g.items.map((it) => `<button type="button" role="menuitem" data-act="${it.i}">${moreIcon(it.icon)}
          <span class="case-more-text"><span class="case-more-label">${esc(it.label)}</span>${it.hint ? `<span class="case-more-hint">${esc(it.hint)}</span>` : ""}</span></button>`).join("")}
      </div>`).join("");
    return `<details class="case-more"><summary class="button" aria-haspopup="menu">More <span class="case-more-caret" aria-hidden="true">▾</span></summary>
      <div class="case-more-menu" role="menu">${body}</div></details>`;
  }

  // ---- next step -------------------------------------------------------------------------------------------
  function nextStep(steps, next, buttonLabel) {
    const box = document.getElementById("case-next");
    if (!box) return;
    const total = steps.length, done = steps.filter((x) => x.state === "done" || x.state === "skipped").length;
    const bar = `<div class="case-progress" aria-label="${done} of ${total} steps done"><span style="width:${total ? Math.round(100 * done / total) : 0}%"></span></div>`;
    if (!next) {
      box.innerHTML = `<div class="case-next-body"><div><span class="case-next-label">All steps done</span>
        <span class="muted">Nothing further is due on this case.</span></div></div>${bar}`;
      return;
    }
    box.innerHTML = `<div class="case-next-body">
        <div><span class="case-next-label">Next step · ${done + 1} of ${total}</span>
          <b class="case-next-title">${esc(next.label)}</b> <span class="chip">${esc(next.owner)}</span>
          ${next.hint ? `<div class="muted">${esc(next.hint)}</div>` : ""}</div>
        <div class="case-next-act">${buttonLabel
          ? `<button type="button" class="primary" onclick="Views.wfAct('${esc(next.key)}')">${esc(buttonLabel)}</button>`
          : `<span class="muted">Waiting on the ${esc(next.owner)}</span>`}
          <a href="javascript:void(0)" onclick="CaseLayout.showSteps()">All steps</a></div>
      </div>${bar}`;
  }
  function showSteps() {
    const el = document.getElementById("wf-steps");
    if (el) { reveal(el); el.classList.add("wf-open"); const d = el.querySelector("details.wf-all"); if (d) d.open = true; el.scrollIntoView({ behavior: "smooth", block: "start" }); }
  }

  // ---- documents -------------------------------------------------------------------------------------------
  const FOLDERS = ["GENERAL", "INTAKE", "EVIDENCE", "CORRESPONDENCE", "OFFERS", "DETERMINATION", "INVOICES", "PARTY_UPLOADS"];
  const folderName = (f) => sentence(f || "GENERAL");
  function who(by) {
    const b = String(by || "");
    if (!b) return "—";
    if (b.startsWith("lettergen:")) return `Generated for ${esc(b.slice(10))}`;
    if (UUID.test(b)) return "Staff";
    const m = b.match(/^(health-plan|tpa|provider|party):(.+)$/);
    if (m) return `${esc(sentence(m[1]))} · ${esc(m[2])}`;
    return esc(b);
  }
  const size = (n) => (n > 1048576 ? (n / 1048576).toFixed(1) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB");
  function documents(id, docs) {
    const mayMove = Auth.can("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN");
    const byFolder = {};
    docs.forEach((d) => { (byFolder[d.folder || "GENERAL"] = byFolder[d.folder || "GENERAL"] || []).push(d); });
    const analysis = (d) => {
      if (d.sealed) return `<span class="muted">sealed</span>`;
      if (String(d.uploaded_by || "").startsWith("lettergen:")) return `<span class="muted">Generated letter</span>`;
      if (!d.analysis_status) return `<span class="muted">—</span>`;
      if (/^ANALYZED/.test(d.analysis_status)) return `<span class="doc-type">${esc(sentence(d.doc_type || "read"))}</span>${d.analysis_status !== "ANALYZED" ? ` <span class="badge warn" title="Some values need checking">check</span>` : ""}`;
      return `<span class="muted">${esc(sentence(d.analysis_status))}</span>`;
    };
    const rows = (items) => items.map((d) => `<tr>
      <td><a href="javascript:void(0)" onclick="Views.downloadDoc('${id}','${d.doc_id}')" class="doc-name">${d.sealed ? "🔒 " : ""}${esc(d.filename || d.doc_id.slice(0, 8) + "…")}</a>
        <div class="muted">${esc((d.content_type || "").split("/").pop().toUpperCase())} · ${size(d.size_bytes || 0)}${d.scan_status && d.scan_status !== "CLEAN" ? ` · <span class="badge warn">${esc(sentence(d.scan_status))}</span>` : ""}</div></td>
      <td>${who(d.uploaded_by)}<div class="muted">${fmtDate(d.uploaded_at)}</div></td>
      <td>${analysis(d)}</td>
      <td class="doc-acts">${!d.sealed && !String(d.uploaded_by || "").startsWith("lettergen:") ? `<a href="javascript:void(0)" onclick="Views.showAnalysis('${id}','${d.doc_id}')">What was read</a>` : ""}
        ${/\.(txt|835|edi|x12)$/i.test(d.filename || "") ? ` · <a href="javascript:void(0)" onclick="Views.era835Fn('${id}','${d.doc_id}')">835 → table</a>` : ""}
        ${mayMove ? `<select class="mini" aria-label="Move to folder" onchange="Views.moveDoc('${id}','${d.doc_id}',this.value,this)"><option value="">Move…</option>
          ${FOLDERS.filter((f) => f !== (d.folder || "GENERAL")).map((f) => `<option value="${f}">${folderName(f)}</option>`).join("")}</select>` : ""}</td></tr>`).join("");
    return `<section class="case-card" id="case-docs">
      <div class="case-card-head"><h3>Documents <span class="muted">${docs.length || ""}</span></h3>
        <div class="actions">${docs.length ? `<button type="button" class="mini" onclick="Views.downloadZip('${id}')">Download all (zip)</button>` : ""}
          <button type="button" class="mini primary" onclick="CaseLayout.toggleUpload()">Upload a document</button></div></div>
      <form id="up" class="upload case-upload" hidden>
        <input type="file" name="file" required aria-label="Choose file" />
        <label>Folder <select name="folder">${FOLDERS.map((f) => `<option value="${f}">${folderName(f)}</option>`).join("")}</select></label>
        <label><input type="checkbox" name="sealed" /> Sealed — encrypted, hidden until lawful reveal</label>
        <button>Upload</button></form>
      ${docs.length ? Object.entries(byFolder).map(([folder, items]) => `<details class="doc-folder-sec" open><summary>${esc(folderName(folder))} <span class="muted">${items.length}</span></summary>
        <table class="doc-table" data-nosearch><thead><tr><th>Document</th><th>Added</th><th>Recognised as</th><th></th></tr></thead><tbody>${rows(items)}</tbody></table></details>`).join("")
        : `<p class="muted">No documents yet.</p>`}
    </section>`;
  }
  function toggleUpload() {
    const f = document.getElementById("up");
    if (f) { f.hidden = !f.hidden; if (!f.hidden) f.querySelector("input[type=file]").focus(); }
  }

  // ---- activity --------------------------------------------------------------------------------------------
  function timeline(activities) {
    if (!activities.length) return `<section class="case-card"><h3>Activity</h3><p class="muted">Nothing has happened on this case yet.</p></section>`;
    const SHOW = 20;
    const row = (a, i) => `<li class="${i >= SHOW ? "tl-more" : ""}"${i >= SHOW ? " hidden" : ""}><span class="tl-type">${esc(sentence(a.type))}</span>
      <span class="tl-body">${esc(a.body)}</span><time class="muted">${fmtDate(a.at)}</time></li>`;
    return `<section class="case-card" id="case-activity"><h3>Activity <span class="muted">${activities.length}</span></h3>
      <ol class="timeline">${activities.map(row).join("")}</ol>
      ${activities.length > SHOW ? `<button type="button" class="mini" onclick="this.previousElementSibling.querySelectorAll('.tl-more').forEach((x) => x.hidden = false); this.remove()">Show all ${activities.length}</button>` : ""}
    </section>`;
  }

  // ---- tabs ------------------------------------------------------------------------------------------------
  function current(id) {
    try { const v = JSON.parse(sessionStorage.getItem(TAB_KEY) || "{}"); return v[id] || "overview"; } catch { return "overview"; }
  }
  function remember(id, tab) {
    try { const v = JSON.parse(sessionStorage.getItem(TAB_KEY) || "{}"); v[id] = tab; sessionStorage.setItem(TAB_KEY, JSON.stringify(v)); } catch { /* private window */ }
  }
  function tabs(id, P, opts) {
    const want = current(id);
    const shown = TABS.filter(([k]) => k !== "billing" || opts.billing || opts.programmed);
    const active = shown.some(([k]) => k === want) ? want : "overview";
    return `<nav class="case-tabs" role="tablist" data-case="${esc(id)}">${shown.map(([k, label]) =>
      `<button type="button" role="tab" data-tab-btn="${k}" aria-selected="${k === active}">${esc(label)}<span class="tab-count" data-count="${k}"></span></button>`).join("")}</nav>
      ${shown.map(([k]) => `<div class="case-pane" role="tabpanel" data-pane="${k}"${k === active ? "" : " hidden"}>${P[k] || ""}</div>`).join("")}`;
  }
  function select(tab) {
    const nav = document.querySelector(".case-tabs");
    if (!nav || !nav.querySelector(`[data-tab-btn="${tab}"]`)) return;
    nav.querySelectorAll("[data-tab-btn]").forEach((b) => b.setAttribute("aria-selected", String(b.dataset.tabBtn === tab)));
    document.querySelectorAll(".case-pane").forEach((p) => { p.hidden = p.dataset.pane !== tab; });
    remember(nav.dataset.case, tab);
  }
  function reveal(el) {
    const pane = el.closest(".case-pane");
    if (pane) select(pane.dataset.pane);
  }
  // The program's sections, rendered off-page, each moved into its tab (order kept).
  function distribute(holder) {
    // Program sections go before what the page already had in each tab, in their own order.
    const firsts = {};
    document.querySelectorAll(".case-pane").forEach((p) => { firsts[p.dataset.pane] = p.firstElementChild; });
    [...holder.children].forEach((el) => {
      const tab = el.dataset.tab || "review";
      const pane = document.querySelector(`.case-pane[data-pane="${tab}"]`) || document.querySelector('.case-pane[data-pane="review"]');
      if (!pane) return;
      if (tab === "overview") {
        const anchor = document.getElementById("case-overview-prog");
        anchor ? anchor.appendChild(el) : pane.prepend(el);
      } else if (tab === "review") {
        const first = pane.querySelector("#case-copilot, #case-checklist");
        first ? pane.insertBefore(el, first) : pane.appendChild(el);
      } else if (tab === "activity") {
        pane.appendChild(el);
      } else {
        const anchor = firsts[pane.dataset.pane];
        anchor && anchor.parentNode === pane ? pane.insertBefore(el, anchor) : pane.appendChild(el);
      }
    });
    markTabs();
  }
  // The tab holding the section of this person's current step gets a dot.
  function markTabs() {
    document.querySelectorAll(".case-pane").forEach((p) => {
      const open = [...p.querySelectorAll("[data-wf]")].some((el) => !el.classList.contains("wf-locked"));
      const c = document.querySelector(`[data-count="${p.dataset.pane}"]`);
      if (c) { c.textContent = open ? "•" : ""; c.title = open ? "Your current step is here" : ""; }
    });
  }
  function bind() {
    const nav = document.querySelector(".case-tabs");
    nav?.addEventListener("click", (ev) => { const b = ev.target.closest("[data-tab-btn]"); if (b) select(b.dataset.tabBtn); });
    // The More menu closes when a choice is made or the click is elsewhere.
    const more = document.querySelector(".case-more");
    if (more) {
      const items = () => [...more.querySelectorAll("[data-act]")];
      more.addEventListener("click", (ev) => { if (ev.target.closest("[data-act]")) more.open = false; });
      document.addEventListener("click", (ev) => { if (!more.contains(ev.target)) more.open = false; }, { capture: true });
      // Opening it puts the first action under the keyboard; arrows move, Esc closes and returns to the button.
      more.addEventListener("toggle", () => { if (more.open) setTimeout(() => items()[0]?.focus(), 0); });
      more.addEventListener("keydown", (ev) => {
        const list = items(), at = list.indexOf(document.activeElement);
        if (ev.key === "Escape" && more.open) { ev.preventDefault(); more.open = false; more.querySelector("summary").focus(); }
        else if ((ev.key === "ArrowDown" || ev.key === "ArrowUp") && more.open && list.length) {
          ev.preventDefault();
          const n = ev.key === "ArrowDown" ? (at + 1) % list.length : (at - 1 + list.length) % list.length;
          list[n].focus();
        } else if ((ev.key === "Home" || ev.key === "End") && more.open && list.length) {
          ev.preventDefault(); list[ev.key === "Home" ? 0 : list.length - 1].focus();
        }
      });
    }
  }

  return { header, moreMenu, nextStep, showSteps, documents, toggleUpload, timeline, tabs, select, reveal, distribute, markTabs, bind };
})();
