// app.js — hash router + bootstrap. Role-aware navigation (Meridian shell).
(async function () {
  const view = document.getElementById("view");
  const nav = document.getElementById("nav");

  // Service worker (PWA). The default fire-and-forget registration leaves
  // an already-open tab on its OLD service worker (and its OLD cached
  // shell) until the browser gets around to its own update check on some
  // later navigation -- during active deploys that reads as "still the
  // old UI" no matter how many times the server is confirmed fresh.
  // reg.update() forces the check now instead of waiting on the browser's
  // schedule, and a new worker installing while one is already active
  // means a newer build landed -- reload once to pick it up.
  if ("serviceWorker" in navigator) {
    // Reload once the new worker has taken over (controllerchange), not when
    // it has merely installed: a reload at "installed" was still answered by
    // the old worker from the old cache, so the new build stayed hidden.
    const hadController = !!navigator.serviceWorker.controller;
    let reloaded = false;
    navigator.serviceWorker.addEventListener("controllerchange", () => {
      if (hadController && !reloaded) { reloaded = true; location.reload(); }
    });
    navigator.serviceWorker.register("/sw.js").then((reg) => {
      reg.update();
      // The check above only ran once, on this page load. A tab left open
      // across several deploys (exactly how this app gets tested -- SPA
      // hash routing, no real reload in between) never re-checked, so a
      // fix already live on the server stayed invisible indefinitely in
      // that tab. Re-check on every route change instead -- cheap (no-op
      // network request if nothing changed) and turns "still broken after
      // you said you fixed it" into "self-heals within one navigation."
      addEventListener("hashchange", () => reg.update());
    });
  }

  // OIDC callback -- handleCallback() already restores the pre-login route
  // via history.replaceState (see auth.js); forcing #/dashboard here is
  // exactly what was stomping every deep link after a fresh login.
  if (await Auth.handleCallback() && !location.hash) location.hash = "#/dashboard";

  const me = Auth.claims();
  if (!me) { Auth.login(); return; }

  // Tenant selection: first tenant group; platform/federal admins may switch later.
  if (!Api.getTenant() || (!me.tenants.includes(Api.getTenant()) && !me.roles.includes("FEDERAL_ADMIN") && !me.roles.includes("PLATFORM_ADMIN"))) {
    if (me.tenants.length) Api.setTenant(me.tenants[0]);
  }
  if (!Api.getTenant()) {
    view.innerHTML = `<h1>No tenant access</h1><p class="muted">Your account is not assigned to any state tenant. Contact your case manager.</p>`;
    return;
  }

  // Server-side user prefs: device-local localStorage is the offline cache;
  // public.user_prefs is the cross-device source of truth (PWA/desktop/native).
  window.Prefs = {
    push(key, value) {
      localStorage.setItem("idre." + key, typeof value === "string" ? value : JSON.stringify(value));
      Api.prefs.put(key, value).catch(() => {}); // offline: server catches up next login
    },
  };
  Api.prefs.all().then((sv) => {
    if (!sv) return;
    if (sv.theme && sv.theme !== (localStorage.getItem("idre.theme") || "")) {
      localStorage.setItem("idre.theme", sv.theme);
      document.documentElement.dataset.theme = sv.theme === "dark" ? "dark" : "";
    }
    if (sv.density) {
      localStorage.setItem("idre.density", sv.density);
      document.body.classList.toggle("density-compact", sv.density === "compact");
    }
    if (sv.recents) localStorage.setItem("idre.recents", JSON.stringify(sv.recents));
    if (sv.last_tenant && sv.last_tenant !== Api.getTenant()) Prefs.push("last_tenant", Api.getTenant());
  }).catch(() => {});

  // Theme (user preference layer; persisted locally)
  const savedTheme = localStorage.getItem("idre.theme");
  if (savedTheme === "dark") document.documentElement.dataset.theme = "dark";
  document.getElementById("theme-toggle").onclick = () => {
    const dark = document.documentElement.dataset.theme !== "dark";
    document.documentElement.dataset.theme = dark ? "dark" : "";
    Prefs.push("theme", dark ? "dark" : "light");
  };

  // Tenant identity chip (always visible, non-removable). Cross-tenant users
  // (PLATFORM_ADMIN / FEDERAL_ADMIN read+write; STATE_AUDITOR read-only) get a
  // switcher; everyone else sees a fixed chip.
  const STATES = ["al","ak","az","ar","ca","co","ct","de","fl","ga","hi","id","il","in","ia","ks","ky","la","me","md","ma","mi","mn","ms","mo","mt","ne","nv","nh","nj","nm","ny","nc","nd","oh","ok","or","pa","ri","sc","sd","tn","tx","ut","vt","va","wa","wv","wi","wy","dc"];
  const crossTenant = me.roles.includes("PLATFORM_ADMIN") || me.roles.includes("FEDERAL_ADMIN");
  const auditorOnly = me.roles.includes("STATE_AUDITOR") &&
    !["CASE_MANAGER", "ARBITRATOR", "FINANCE", "PARTY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"].some((r) => me.roles.includes(r));
  const chip = document.getElementById("tenant-chip");
  // Admin-settable display name (e.g. "AHCA" for FL) -- empty until fetched,
  // so the chip paints with the generic "XX IDRE" fallback immediately and
  // upgrades once the label (if any) arrives, rather than blocking the
  // whole app's first paint on it.
  let tenantLabels = {};
  const labelFor = (code) => tenantLabels[code] || `${code.toUpperCase()} IDRE`;
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const paintChip = () => {
    const cur = Api.getTenant();
    document.getElementById("t-avatar").textContent = cur.slice(0, 2).toUpperCase();
    if (crossTenant || auditorOnly) {
      document.getElementById("t-name").innerHTML =
        `<select id="t-switch" class="t-select" aria-label="Switch state tenant">` +
        STATES.map((s) => `<option value="${s}"${s === cur ? " selected" : ""}>${esc(labelFor(s))}</option>`).join("") +
        `</select>${auditorOnly ? '<span class="badge s-audit">read-only audit</span>' : ""}`;
      document.getElementById("t-switch").addEventListener("change", (e) => {
        Api.setTenant(e.target.value);
        Prefs.push("last_tenant", e.target.value);
        UI.toast(`Switched to ${labelFor(e.target.value)} tenant${auditorOnly ? " (read-only)" : ""}`);
        paintChip();
        location.hash = "#/dashboard";
        location.reload();
      });
    } else {
      document.getElementById("t-name").textContent = labelFor(cur);
    }
  };
  paintChip();
  if (crossTenant || auditorOnly) {
    Api.program.tenantLabels().then((labels) => { tenantLabels = labels || {}; paintChip(); }).catch(() => {});
  } else {
    Api.program.get().then((pr) => {
      const label = pr?.tenant_label;
      if (label) { tenantLabels[Api.getTenant()] = label; paintChip(); }
    }).catch(() => {});
  }

  // Demo-mode banner (only when the backend is stubbed)
  if (window.IDRE_DEMO) {
    const bar = document.createElement("div");
    bar.className = "demo-bar";
    bar.innerHTML = `⚠ <b>Demo mode</b> — real portal code rendering built-in sample data; no backend connected. Set <code>demoMode:false</code> and point <code>apiBase</code>/<code>keycloakUrl</code> at your deployment for live data.`;
    document.getElementById("body").insertBefore(bar, view);
  }

  // Per-tenant feature flags (server-enforced; this only hides the UI).
  // Absent flag = on. Loaded fresh on each full boot; a toggle applies on
  // next reload — the server gate applies immediately.
  let FEATURES = {};
  let BRAND = "Capitol Bridge";
  try { const pr0 = await Api.program.get(); FEATURES = pr0.features || {}; BRAND = pr0.brand_name || BRAND; } catch { FEATURES = {}; }
  // The portal's name for this tenant (tenant_labels.brand_name; editable on the Rules page).
  const brandMark = BRAND.split(/\s+/).filter(Boolean).map((w) => w[0]).join("").slice(0, 2).toUpperCase() || "CB";
  document.querySelectorAll(".rail-word").forEach((el) => { el.textContent = BRAND; });
  document.querySelectorAll(".rail-mark").forEach((el) => { el.textContent = brandMark; });
  document.title = BRAND + " Portal";
  window.APP_BRAND = BRAND;
  const feature = (n) => FEATURES[n] !== false;

  // Role-aware rail navigation (mirrors docs/STAKEHOLDERS.md coverage matrix)
  const has = (...rs) => Auth.can(...rs);
  // "New dispute" (#/new) only fits the federal NSA process -- CMS case
  // number format, required provider/payer IDs on file, an open-negotiation
  // -end date. A programmed tenant (FL AHCA CDR today) has none of those
  // concepts and files through Intake instead (email/contact/org only --
  // the real case number, amount, and eligibility inputs all come from the
  // uploaded packet, never typed in up front). Showing New dispute to a
  // programmed tenant's case managers was pure confusion: a form whose
  // required fields don't apply to any case they'd ever open.
  const programmed = !!(await Api.program.get().catch(() => null))?.config;
  const links = [["#/dashboard", "▤", "Home"]];
  // TPA deliberately excluded from the generic CRM/case screens -- those
  // show the WHOLE tenant's cases/accounts/leads/tasks with no per-caller
  // scoping, but a TPA only has standing to see the disputes it filed
  // itself, which is exactly what its own #/tpa "Tracking" card already
  // shows. Matches the backend's listCases/listAccounts/listLeads/
  // listTasks TPA exclusion.
  // An outside organization's login (a TPA, or a health plan without a
  // staff role) gets its own page, never the tenant-wide screens.
  const outside = has("TPA") || (has("HEALTH_PLAN", "PARTY") && !has("CASE_MANAGER", "PM", "FINANCE", "CODER", "NURSE_PHYSICIAN", "DOCTOR",
    "NURSE", "ATTORNEY", "ARBITRATOR", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"));
  if (!outside) {
    links.push(["#/cases", "▦", "Disputes"], ["#/pipeline", "▥", "Pipeline"],
      ["#/crm/accounts", "◈", "Accounts"], ["#/crm/leads", "◎", "Leads"], ["#/crm/tasks", "☑", "Tasks"]);
  }
  if (!programmed && has("PARTY", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/new", "＋", "New dispute"]);
  // TPA excluded -- Ask the graph answers from the whole tenant's case
  // data; matches the backend's graphAsk/graphFeedback TPA exclusion.
  if (!outside && !has("PARTY")) links.push(["#/ask", "✦", "Ask the graph"]);
  // Party workspace: an invited party's own disputes (party_accounts is the only source of access).
  if (has("PARTY") && feature("party_workspace")) links.push(["#/party", "⚭", "My disputes"]);
  // Specialty review queues: each specialist sees their own, the PM all three
  // (matches queues.go's per-discipline roles). Off when the tenant's
  // review_queues flag is off.
  const queues = feature("review_queues");
  if (queues && has("CODER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/queues/CODING", "⌗", "Coding queue"]);
  if (queues && has("NURSE_PHYSICIAN", "DOCTOR", "NURSE", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/queues/CLINICAL", "✚", "Clinical queue"]);
  if (queues && has("ATTORNEY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/queues/LEGAL", "§", "Legal queue"]);
  if (queues && has("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/queues/pm", "▲", "PM board"]);
  if (has("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/assistant", "❖", "Assistant"]);
  if (has("CASE_MANAGER", "ARBITRATOR", "PM", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
    links.push(["#/qa", "✓", "QA gate"]);
  }
  if (has("CASE_MANAGER", "ARBITRATOR", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
    links.push(["#/intake", "⇥", "Intake"]);
    links.push(["#/deliverables", "⎘", "Deliverables"]);
  }
  // CASE_MANAGER deliberately excluded -- Financials is a standalone
  // dashboard, not case-detail info; matches the backend's
  // financialReadRoles (access-control follow-up).
  if (has("FINANCE", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR")) links.push(["#/finance", "◍", "Financials"]);
  if (has("PM", "ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", "TENANT_ADMIN")) links.push(["#/ops", "◔", "Ops"]);
  // TPA excluded -- billable-hours timesheet logging against disputes is a
  // staff-productivity feature, not part of a TPA's own workflow.
  if (!outside) links.push(["#/timesheet", "⏱", "My timesheet"]);
  if (!outside || has("TPA")) links.push(["#/calendar", "▨", "Calendar"]);
  // TPA excluded -- Onboarding is for registering NEW provider/payer/IDRE
  // entity/auditor organizations and reviewing those applications; a TPA
  // registers through its own dedicated flow (dispute.html) and has no
  // use for this screen.
  if (!outside) links.push(["#/onboarding", "⚑", "Onboarding"]);
  if (has("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/voice", "☎", "Voice console"]);
  if (has("PM", "FEDERAL_ADMIN", "STATE_AUDITOR", "PLATFORM_ADMIN", "TENANT_ADMIN")) links.push(["#/reports", "◫", "Reports"]);
  if (has("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) links.push(["#/time", "⏱", "Team time"]);
  // Everyone who drafts letters can read the templates; the PM and admins change them.
  if (has("CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) links.push(["#/templates", "✎", "Templates"]);
  // CASE_MANAGER deliberately excluded from all four -- standalone finance
  // screens, not case-detail info; matches billingViewRoles/arapReadRoles/
  // reconReadRoles/financialReadRoles (access-control follow-up).
  if (has("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
    links.push(["#/billing", "🧾", "Billing"]);
    links.push(["#/arap", "⚖", "AR / AP"]);
    links.push(["#/recon", "⇄", "Reconciliation"]);
    links.push(["#/checks", "◧", "Checks"]);
  }
  if (has("TPA", "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/tpa", "⛁", "TPA"]);
  if (feature("healthplan_onboarding") && has("HEALTH_PLAN", "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/healthplan", "⛃", "Health plan"]);
  if (has("FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) links.push(["#/rules", "§", "Rules"]);
  if (has("FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/features", "⚙", "Feature toggles"]);
  // Permanent CASE_MANAGER access (own tenant only, enforced server-side),
  // not the Rules page -- CASE_MANAGER never has a standing right to edit
  // program rules, so this stays reachable independent of that gate.
  if (has("PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) links.push(["#/team", "⚉", "Team"]);
  if (has("CASE_MANAGER", "PM", "FINANCE", "ARBITRATOR", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR")) links.push(["#/mail", "✉", "Mail log"]);
  // Payment rails and OFAC screening: money screens, so the money roles (the page itself checks too).
  if (has("PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN")) links.push(["#/payments", "🏦", "Payments"]);
  // Insights (payer analytics, network adequacy): case staff and auditors; flag-gated.
  if ((feature("payer_analytics") || feature("network_adequacy")) && has("CASE_MANAGER", "ARBITRATOR", "ATTORNEY", "DOCTOR", "NURSE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", "CODER", "NURSE_PHYSICIAN", "PM")) links.push(["#/insights", "◈", "Insights"]);
  // Audit trails, isolated: the whole program's for admins and the state
  // auditor; an organization's own for a TPA or plan; and everyone's own actions.
  // One Audit log per person, at the widest scope they may see, with an
  // "Only my actions" switch: the whole program for admins and the state
  // auditor, the organization for a TPA or plan, and otherwise your own.
  if (has("FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", "TENANT_ADMIN")) links.push(["#/audit", "⌘", "Audit log"]);
  else if (outside && has("TPA", "HEALTH_PLAN")) links.push(["#/org-audit", "⌘", "Audit log"]);
  else links.push(["#/my-activity", "⌘", "Audit log"]);
  if (outside) links.push(["#/document-requests", "⇪", "Document requests"]);
  nav.innerHTML = links.map(([h, i, l]) =>
    `<a href="${h}" data-route="${h.slice(2)}"><span class="ri">${i}</span><span class="rl">${l}</span></a>`).join("");

  // Floating assistant launcher: lives outside the router's #view swap (a
  // real DOM element appended once to <body>) so it survives every route
  // change instead of being a per-page thing only #/assistant has.
  Views.mountAssistantFab();

  // Mobile rail: collapsed to icon-only under 860px with no way to see
  // labels or reach anything not already memorized by icon, and sign-out
  // (in #whoami, top-right) disappears entirely at that width -- this
  // toggle expands the rail as an overlay, and sign-out now also lives in
  // the rail's own footer so it survives the collapse instead of needing
  // the hidden #whoami text next to it.
  const railToggle = document.getElementById("rail-toggle");
  const backdrop = document.getElementById("rail-backdrop");
  const closeRail = () => {
    document.getElementById("rail").classList.remove("rail-open");
    backdrop.classList.remove("show");
    railToggle.setAttribute("aria-expanded", "false");
  };
  railToggle.addEventListener("click", () => {
    const open = document.getElementById("rail").classList.toggle("rail-open");
    backdrop.classList.toggle("show", open);
    railToggle.setAttribute("aria-expanded", String(open));
  });
  backdrop.addEventListener("click", closeRail);
  nav.addEventListener("click", (e) => { if (e.target.closest("a")) closeRail(); });

  // Notifications
  document.getElementById("bell").onclick = async () => {
    const n = await Api.cm.notifications().catch(() => []);
    const w = window.__notifPanel || (window.__notifPanel = document.createElement("div"));
    w.className = "notif-panel";
    w.innerHTML = `<h3>Notifications${n.length ?
        ` <a href="javascript:void(0)" class="notif-markall" onclick="Api.cm.readAllNotifs().then(()=>location.reload())">Mark all as read</a>` : ""
      }</h3>` + (n.length ? n.map((x) =>
      `<div class="notif ${x.read_at ? "" : "unread"}" onclick="Api.cm.readNotif('${x.id}').then(()=>{${
        x.link ? `location.hash='${x.link}';location.reload()` : "location.reload()"
      }})">
         <b>${x.type}</b> — ${x.body} <span class="muted">${new Date(x.at).toLocaleString()}</span></div>`).join("")
      : `<p class="muted">No notifications.</p>`);
    document.body.appendChild(w);
    setTimeout(() => document.addEventListener("click", (e) => { if (!w.contains(e.target) && e.target.id !== "bell") w.remove(); }, { once: true }), 0);
  };
  (async () => {
    const n = await Api.cm.notifications().catch(() => []);
    const unread = n.filter((x) => !x.read_at).length;
    const b = document.getElementById("bell-n");
    if (unread) { b.textContent = unread; b.style.display = "block"; }
  })();

  // Identity. The avatar doubles as the sign-out control on narrow
  // viewports, where .who-txt (and the #lo link inside it) is hidden by
  // CSS to save header space -- without this there was no way to sign out
  // on mobile at all.
  const initials = (me.name || "?").split(/[\s._-]+/).map((s) => s[0]).join("").slice(0, 2).toUpperCase();
  document.getElementById("whoami").innerHTML =
    `<span class="avatar" id="avatar-lo" title="Sign out" role="button" tabindex="0">${initials}</span><span class="who-txt">${me.name} · <a href="javascript:void(0)" id="lo">sign out</a></span>`;
  document.getElementById("avatar-lo").addEventListener("click", Auth.logout);
  document.getElementById("rail-foot").innerHTML =
    `<a href="javascript:void(0)" id="rail-lo" title="Sign out"><span class="ri">⏻</span><span class="rl">Sign out — ${me.name}</span></a>`;
  document.getElementById("rail-lo").addEventListener("click", Auth.logout);
  // Global search: live dropdown (debounced) + full-results page on Enter.
  // The field used to respond to Enter ONLY — typing showed nothing, which
  // read as "search does nothing". Now: 2+ chars queries /search after a
  // 250ms debounce, arrows select, Enter opens the hit or the full page.
  (() => {
    const gq = document.getElementById("gq"), drop = document.getElementById("gq-drop");
    const escHtml = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
    // "page" hits carry their own href/icon directly (set when matched
    // below) rather than deriving them from kind, same way document hits
    // already carry case_id -- no backend round-trip needed, this is just
    // the same role-filtered `links` array the rail itself renders from.
    const linkFor = (h) => h.kind === "page" ? h.id
      : h.kind === "case" ? `#/cases/${h.id}`
      : h.kind === "document" ? `#/cases/${h.case_id || ""}`
      : h.kind === "task" ? (h.case_id ? `#/cases/${h.case_id}` : "#/crm/tasks")
      : h.kind === "invoice" ? (h.case_id ? `#/cases/${h.case_id}` : "#/finance")
      : h.kind === "account" ? `#/crm/accounts/${h.id}`
      : h.kind === "lead" ? "#/crm/leads" : "#/crm/accounts";
    const iconFor = (h) => h.kind === "page" ? h.icon
      : h.kind === "case" ? "▦" : h.kind === "document" ? "🗎"
      : h.kind === "task" ? "☑" : h.kind === "invoice" ? "🧾" : "◈";
    let timer = 0, items = [], sel = -1;
    const closeDrop = () => { drop.hidden = true; items = []; sel = -1; };
    async function refresh() {
      const q = gq.value.trim();
      if (q.length < 2) { closeDrop(); return; }
      // Page matches first: typing "pipeline" should offer the Pipeline
      // PAGE, not just records that happen to mention the word -- search
      // only ever covered data rows, never the app's own screens, which
      // read as "search doesn't find pipeline" even though cases named
      // Pipeline-something would have. Client-side, instant, and already
      // role-filtered since it's the exact array the rail itself uses.
      const ql = q.toLowerCase();
      const pageHits = links
        .filter(([, , label]) => label.toLowerCase().includes(ql))
        .slice(0, 4)
        .map(([href, icon, label]) => ({ kind: "page", id: href, icon, label, detail: "page" }));
      let dataHits = [];
      try { dataHits = (await Api.crm.search(q)).slice(0, 8 - pageHits.length); } catch { dataHits = []; }
      if (gq.value.trim() !== q) return; // a newer keystroke owns the box
      items = [...pageHits, ...dataHits]; sel = -1;
      drop.innerHTML = items.map((h, i) =>
        `<div class="palette-item gq-item" data-i="${i}" role="option">
           <span class="ri">${iconFor(h)}</span><span class="pl">${escHtml(h.label)}</span>
           ${h.detail ? `<span class="pd">${escHtml(h.detail)}</span>` : ""}</div>`).join("") ||
        `<div class="palette-empty">No matches — press Enter for full search</div>`;
      drop.hidden = false;
      drop.querySelectorAll(".gq-item").forEach((el) =>
        el.addEventListener("mousedown", () => { location.hash = linkFor(items[+el.dataset.i]); closeDrop(); }));
    }
    gq.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(refresh, 250); });
    gq.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        if (sel >= 0 && items[sel]) { location.hash = linkFor(items[sel]); closeDrop(); }
        else location.hash = `#/search/${encodeURIComponent(gq.value)}`;
      } else if (e.key === "Escape") closeDrop();
      else if ((e.key === "ArrowDown" || e.key === "ArrowUp") && !drop.hidden && items.length) {
        e.preventDefault();
        sel = (sel + (e.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
        drop.querySelectorAll(".gq-item").forEach((el, i) => el.classList.toggle("sel", i === sel));
      }
    });
    gq.addEventListener("blur", () => setTimeout(closeDrop, 150)); // mousedown lands first
    // The box's own placeholder has promised "(⌘K)" the whole time with
    // nothing wired to it -- confirmed live, no listener anywhere bound
    // the shortcut it advertises. ⌘K (Mac) / Ctrl+K (Windows/Linux) jumps
    // in from anywhere, matching the placeholder instead of silently
    // doing nothing.
    addEventListener("keydown", (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        gq.focus();
        gq.select();
      }
    });
  })();
  document.getElementById("lo").onclick = Auth.logout;

  const routes = [
    [/^#\/dashboard$/, Views.dashboard],
    [/^#\/pipeline$/, CrmViews.pipeline],
    [/^#\/cases(\?.*)?$/, Views.cases],
    [/^#\/cases\/([\w-]+)$/, (m) => Views.caseDetail(m[1])],
    [/^#\/cases\/([\w-]+)\/offers$/, (m) => Views.caseOffers(m[1])],
    [/^#\/features$/, Views.featuresAdmin],
    [/^#\/queues\/pm$/, Views.pmQueueView],
    [/^#\/queues\/(CODING|CLINICAL|LEGAL)(\?.*)?$/, (m) => Views.queueView(m[1])],
    [/^#\/insights$/, Views.insightsView],
    [/^#\/party$/, Views.partyCases],
    [/^#\/party\/cases\/([\w-]+)$/, (m) => Views.partyCaseDetail(m[1])],
    [/^#\/new$/, Views.newDispute],
    [/^#\/crm\/accounts$/, CrmViews.accounts],
    [/^#\/crm\/accounts\/new$/, CrmViews.accountNew],
    [/^#\/crm\/accounts\/([\w-]+)$/, (m) => CrmViews.account360(m[1])],
    [/^#\/crm\/leads$/, CrmViews.leads],
    [/^#\/crm\/tasks(\?.*)?$/, CrmViews.tasks],
    [/^#\/search\/(.+)$/, (m) => CrmViews.search(decodeURIComponent(m[1]))],
    [/^#\/ask$/, Views.askGraph],
    [/^#\/assistant$/, () => Views.assistant("")],
    [/^#\/assistant\/([\w-]+)$/, (m) => Views.assistant(m[1])],
    [/^#\/qa$/, Views.qaQueue],
    [/^#\/intake$/, Views.intake],
    [/^#\/deliverables$/, Views.deliverables],
    [/^#\/finance$/, Views.finance],
    [/^#\/ops$/, Views.opsDashboard],
    [/^#\/calendar$/, CrmViews.calendar],
    [/^#\/onboarding$/, Views.onboarding],
    [/^#\/onboarding\/new$/, Views.onboardingNew],
    [/^#\/voice$/, Views.voice],
    [/^#\/reports$/, Views.reports],
    [/^#\/timesheet$/, Views.myTimesheet],
    [/^#\/time$/, Views.timeReport],
    [/^#\/templates$/, Views.templatesPage],
    [/^#\/billing$/, Views.billingInvoices],
    [/^#\/arap$/, Views.arapView],
    [/^#\/recon$/, Views.reconView],
    [/^#\/checks$/, Views.checksView],
    [/^#\/tpa$/, Views.tpaView],
    [/^#\/healthplan$/, Views.hpView],
    [/^#\/rules$/, Views.rulesAdmin],
    [/^#\/team$/, Views.teamAdmin],
    [/^#\/mail$/, Views.mailLog],
    [/^#\/payments$/, Views.paymentsView],
    [/^#\/audit$/, Views.auditLog],
    [/^#\/org-audit$/, Views.orgAuditPage],
    [/^#\/my-activity$/, Views.myActivityPage],
    [/^#\/document-requests$/, Views.docReqPage],
  ];

  // One rule for every way of reaching a page -- the rail, a typed or
  // bookmarked address, a link in an email, the palette, the back button:
  // a page opens only if this account's menu offers it. The menu used to be
  // the only thing that knew, so a hidden page still opened by its address.
  // This is for a clear answer on screen; the API refuses the data itself.
  const menu = new Set(links.map(([h]) => h.slice(2)));
  const pageOf = (h) => {
    const parts = h.replace(/^#\//, "").split("?")[0].split("/");
    return parts[0] === "crm" || parts[0] === "queues" ? parts.slice(0, 2).join("/") : parts[0];
  };
  function canOpen(h) {
    const page = pageOf(h);
    if (menu.has(page)) return true;
    // Pages reached from another page rather than from the menu.
    if (page === "search") return menu.has("cases");
    // Your own actions: always yours to see (bookmarks of the old "My activity").
    if (page === "my-activity") return true;
    // The Coder drafts the cost estimate email and sends it once QA approves it:
    // the step's "Send it" opens the QA gate, which shows the Coder its own drafts.
    if (page === "qa") return has("CODER");
    // A TPA has no Disputes list, but opens the disputes it filed from its own page.
    if (page === "cases" && /^#\/cases\/[\w-]+$/.test(h)) return outside;
    return false;
  }
  const noAccess = () => `<div class="view-head"><h1>No access</h1></div>
    <p class="muted">Your account doesn't include this page. If you think it should, ask your program manager or administrator.</p>
    <p><a href="#/dashboard">Back to Home</a></p>`;

  async function render() {
    const h = location.hash || "#/dashboard";
    nav.querySelectorAll("a").forEach((a) => {
      const r = a.dataset.route;
      a.classList.toggle("active", h === "#/" + r || h.startsWith("#/" + r + "/") || h.startsWith("#/" + r + "?") || (r === "cases" && h.startsWith("#/new")));
    });
    // A route may carry ?... (Stripe's return to "#/tpa?paid=1"): routes that
    // don't take a query match without it, instead of showing "Not found".
    const hBare = h.split("?")[0];
    for (const [re, fn] of routes) {
      const m = h.match(re) || hBare.match(re);
      if (m) {
        if (!canOpen(h)) { view.innerHTML = noAccess(); return; }
        const result = await fn(m);
        // Views that need to wire up listeners after rendering return
        // {html, wire} instead of a plain string: wire() must run AFTER
        // this assignment, never via queueMicrotask inside the view
        // function itself -- that always runs as an earlier microtask
        // than this assignment (confirmed empirically), so every listener
        // it tried to attach found nothing yet and silently no-op'd.
        if (result && typeof result === "object" && "html" in result) {
          view.innerHTML = result.html;
          result.wire?.();
        } else {
          view.innerHTML = result;
        }
        view.classList.remove("view-in");
        void view.offsetWidth; // restart the enter animation on every render
        view.classList.add("view-in");
        view.focus({ preventScroll: true });
        return;
      }
    }
    view.innerHTML = `<h1>Not found</h1>`;
  }
  // Soft refresh: re-render the current route without a full page reload
  // (no Keycloak round-trip, no shell flash, preserves rail/scroll context).
  window.App = { rerender: render, canOpen, feature };
  // Last-resort feedback net: any async handler that still throws without its
  // own try/catch surfaces as an error toast instead of failing silently.
  addEventListener("unhandledrejection", (e) => {
    UI.toast(e.reason?.message || "Unexpected error", { kind: "error" });
  });
  // Immediate feedback when any compact select changes, before the async
  // handler's own toast lands — the control flashes so the user sees the
  // change registered even on slow networks.
  document.addEventListener("change", (e) => {
    if (e.target.matches?.("select.mini")) UI.flash(e.target);
  });
  addEventListener("hashchange", render);
  if (!location.hash) location.hash = "#/dashboard";
  render();

  // Presence heartbeat: report this user as active every 45s; the ops
  // dashboard counts anyone seen within the last 3 minutes as online.
  // Fail-open by design — presence is never a reason to error the shell.
  const pingPresence = () => Api.program.pingPresence(me?.name).catch(() => {});
  pingPresence();
  setInterval(pingPresence, 45000);
})();
