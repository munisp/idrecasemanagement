// app.js — hash router + bootstrap. Role-aware navigation.
(async function () {
  const view = document.getElementById("view");
  const nav = document.getElementById("nav");
  const whoami = document.getElementById("whoami");

  // Service worker (PWA)
  if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js");

  // OIDC callback
  if (await Auth.handleCallback()) location.hash = "#/dashboard";

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

  // Role-aware navigation (mirrors docs/STAKEHOLDERS.md coverage matrix)
  const has = (...rs) => rs.some((r) => me.roles.includes(r));
  const links = [["#/dashboard", "Dashboard"], ["#/pipeline", "Pipeline"], ["#/cases", "Disputes"],
    ["#/crm/accounts", "Accounts"], ["#/crm/leads", "Leads"], ["#/crm/tasks", "Tasks"]];
  if (has("PARTY", "CASE_MANAGER")) links.push(["#/new", "New dispute"]);
  links.push(["#/onboarding", "Onboarding"]);
  if (has("CASE_MANAGER")) links.push(["#/voice", "Voice console"]);
  links.push(["#/calendar", "Calendar"]);
  if (has("FEDERAL_ADMIN", "STATE_AUDITOR", "PLATFORM_ADMIN")) links.push(["#/reports", "Reports"]);
  nav.innerHTML = links.map(([h, l]) => `<a href="${h}">${l}</a>`).join("") +
    `<button id="bell" class="bell" title="Notifications">🔔<span id="bell-n" class="bell-n"></span></button>` +
    `<input id="gq" placeholder="Search…" style="padding:.25rem .5rem;border-radius:6px;border:0" />`;
  document.getElementById("bell").onclick = async () => {
    const n = await Api.cm.notifications().catch(() => []);
    const w = window.__notifPanel || (window.__notifPanel = document.createElement("div"));
    w.className = "notif-panel";
    w.innerHTML = `<h3>Notifications</h3>` + (n.length ? n.map((x) =>
      `<div class="notif ${x.read_at ? "" : "unread"}" onclick="Api.cm.readNotif('${x.id}').then(()=>location.reload())">
         <b>${x.type}</b> — ${x.message} <span class="muted">${new Date(x.created_at).toLocaleString()}</span></div>`).join("")
      : `<p class="muted">No notifications.</p>`);
    document.body.appendChild(w);
    setTimeout(() => document.addEventListener("click", (e) => { if (!w.contains(e.target) && e.target.id !== "bell") w.remove(); }, { once: true }), 0);
  };
  (async () => {
    const n = await Api.cm.notifications().catch(() => []);
    const unread = n.filter((x) => !x.read_at).length;
    const b = document.getElementById("bell-n");
    if (unread) { b.textContent = unread; b.style.display = "inline-block"; }
  })();
  whoami.innerHTML = `${me.name} · ${Api.getTenant().toUpperCase()} · <a href="javascript:void(0)" id="lo">sign out</a>`;
  document.getElementById("gq").addEventListener("keydown", (e) => {
    if (e.key === "Enter") location.hash = `#/search/${encodeURIComponent(e.target.value)}`;
  });
  document.getElementById("lo").onclick = Auth.logout;

  const routes = [
    [/^#\/dashboard$/, Views.dashboard],
    [/^#\/pipeline$/, CrmViews.pipeline],
    [/^#\/cases$/, Views.cases],
    [/^#\/cases\/([\w-]+)$/, (m) => Views.caseDetail(m[1])],
    [/^#\/new$/, Views.newDispute],
    [/^#\/crm\/accounts$/, CrmViews.accounts],
    [/^#\/crm\/accounts\/new$/, CrmViews.accountNew],
    [/^#\/crm\/accounts\/([\w-]+)$/, (m) => CrmViews.account360(m[1])],
    [/^#\/crm\/leads$/, CrmViews.leads],
    [/^#\/crm\/tasks$/, CrmViews.tasks],
    [/^#\/search\/(.+)$/, (m) => CrmViews.search(decodeURIComponent(m[1]))],
    [/^#\/calendar$/, CrmViews.calendar],
    [/^#\/onboarding$/, Views.onboarding],
    [/^#\/onboarding\/new$/, Views.onboardingNew],
    [/^#\/voice$/, Views.voice],
    [/^#\/reports$/, Views.reports],
  ];

  async function render() {
    const h = location.hash || "#/dashboard";
    for (const [re, fn] of routes) {
      const m = h.match(re);
      if (m) { view.innerHTML = await fn(m); return; }
    }
    view.innerHTML = `<h1>Not found</h1>`;
  }
  addEventListener("hashchange", render);
  if (!location.hash) location.hash = "#/dashboard";
  render();
})();
