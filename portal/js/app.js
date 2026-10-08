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
    navigator.serviceWorker.register("/sw.js").then((reg) => {
      reg.addEventListener("updatefound", () => {
        const nw = reg.installing;
        nw?.addEventListener("statechange", () => {
          if (nw.state === "installed" && navigator.serviceWorker.controller) location.reload();
        });
      });
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
  const paintChip = () => {
    const cur = Api.getTenant();
    document.getElementById("t-avatar").textContent = cur.slice(0, 2).toUpperCase();
    if (crossTenant || auditorOnly) {
      document.getElementById("t-name").innerHTML =
        `<select id="t-switch" class="t-select" aria-label="Switch state tenant">` +
        STATES.map((s) => `<option value="${s}"${s === cur ? " selected" : ""}>${s.toUpperCase()} IDRE</option>`).join("") +
        `</select>${auditorOnly ? '<span class="badge s-audit">read-only audit</span>' : ""}`;
      document.getElementById("t-switch").addEventListener("change", (e) => {
        Api.setTenant(e.target.value);
        Prefs.push("last_tenant", e.target.value);
        UI.toast(`Switched to ${e.target.value.toUpperCase()} tenant${auditorOnly ? " (read-only)" : ""}`);
        paintChip();
        location.hash = "#/dashboard";
        location.reload();
      });
    } else {
      document.getElementById("t-name").textContent = `${cur.toUpperCase()} IDRE`;
    }
  };
  paintChip();

  // Demo-mode banner (only when the backend is stubbed)
  if (window.IDRE_DEMO) {
    const bar = document.createElement("div");
    bar.className = "demo-bar";
    bar.innerHTML = `⚠ <b>Demo mode</b> — real portal code rendering built-in sample data; no backend connected. Set <code>demoMode:false</code> and point <code>apiBase</code>/<code>keycloakUrl</code> at your deployment for live data.`;
    document.getElementById("body").insertBefore(bar, view);
  }

  // Role-aware rail navigation (mirrors docs/STAKEHOLDERS.md coverage matrix)
  const has = (...rs) => rs.some((r) => me.roles.includes(r));
  // "New dispute" (#/new) only fits the federal NSA process -- CMS case
  // number format, required provider/payer IDs on file, an open-negotiation
  // -end date. A programmed tenant (FL AHCA CDR today) has none of those
  // concepts and files through Intake instead (email/contact/org only --
  // the real case number, amount, and eligibility inputs all come from the
  // uploaded packet, never typed in up front). Showing New dispute to a
  // programmed tenant's case managers was pure confusion: a form whose
  // required fields don't apply to any case they'd ever open.
  const programmed = !!(await Api.program.get().catch(() => null))?.config;
  const links = [
    ["#/dashboard", "▤", "Home"], ["#/cases", "▦", "Disputes"], ["#/pipeline", "▥", "Pipeline"],
    ["#/crm/accounts", "◈", "Accounts"], ["#/crm/leads", "◎", "Leads"], ["#/crm/tasks", "☑", "Tasks"],
  ];
  if (!programmed && has("PARTY", "CASE_MANAGER")) links.push(["#/new", "＋", "New dispute"]);
  links.push(["#/ask", "✦", "Ask the graph"]);
  if (has("CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/assistant", "❖", "Assistant"]);
  if (has("CASE_MANAGER", "ARBITRATOR", "PM", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
    links.push(["#/qa", "✓", "QA gate"]);
  }
  if (has("CASE_MANAGER", "ARBITRATOR", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) {
    links.push(["#/intake", "⇥", "Intake"]);
    links.push(["#/deliverables", "⎘", "Deliverables"]);
  }
  if (has("FINANCE", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR")) links.push(["#/finance", "◍", "Financials"]);
  if (has("CASE_MANAGER", "ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR")) links.push(["#/ops", "◔", "Ops"]);
  links.push(["#/calendar", "▨", "Calendar"]);
  links.push(["#/onboarding", "⚑", "Onboarding"]);
  if (has("CASE_MANAGER")) links.push(["#/voice", "☎", "Voice console"]);
  if (has("CASE_MANAGER", "PM", "FEDERAL_ADMIN", "STATE_AUDITOR", "PLATFORM_ADMIN")) links.push(["#/reports", "◫", "Reports"]);
  if (has("FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/rules", "§", "Rules"]);
  // Permanent CASE_MANAGER access (own tenant only, enforced server-side),
  // not the Rules page -- CASE_MANAGER never has a standing right to edit
  // program rules, so this stays reachable independent of that gate.
  if (has("CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN")) links.push(["#/team", "⚉", "Team"]);
  if (has("FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", "CASE_MANAGER", "PM")) links.push(["#/audit", "⌘", "Audit log"]);
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
    const linkFor = (h) => h.kind === "case" ? `#/cases/${h.id}`
      : h.kind === "document" ? `#/cases/${h.case_id || ""}`
      : h.kind === "task" ? (h.case_id ? `#/cases/${h.case_id}` : "#/crm/tasks")
      : h.kind === "invoice" ? (h.case_id ? `#/cases/${h.case_id}` : "#/finance")
      : h.kind === "account" ? `#/crm/accounts/${h.id}`
      : h.kind === "lead" ? "#/crm/leads" : "#/crm/accounts";
    const iconFor = (h) => h.kind === "case" ? "▦" : h.kind === "document" ? "🗎"
      : h.kind === "task" ? "☑" : h.kind === "invoice" ? "🧾" : "◈";
    let timer = 0, items = [], sel = -1;
    const closeDrop = () => { drop.hidden = true; items = []; sel = -1; };
    async function refresh() {
      const q = gq.value.trim();
      if (q.length < 2) { closeDrop(); return; }
      let hits = [];
      try { hits = (await Api.crm.search(q)).slice(0, 8); } catch { hits = []; }
      if (gq.value.trim() !== q) return; // a newer keystroke owns the box
      items = hits; sel = -1;
      drop.innerHTML = hits.map((h, i) =>
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
    [/^#\/rules$/, Views.rulesAdmin],
    [/^#\/team$/, Views.teamAdmin],
    [/^#\/audit$/, Views.auditLog],
  ];

  async function render() {
    const h = location.hash || "#/dashboard";
    nav.querySelectorAll("a").forEach((a) => {
      const r = a.dataset.route;
      a.classList.toggle("active", h === "#/" + r || h.startsWith("#/" + r + "/") || (r === "cases" && h.startsWith("#/new")));
    });
    for (const [re, fn] of routes) {
      const m = h.match(re);
      if (m) {
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
  window.App = { rerender: render };
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
