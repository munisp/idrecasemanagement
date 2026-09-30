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
  if (has("FEDERAL_ADMIN", "STATE_AUDITOR", "PLATFORM_ADMIN")) links.push(["#/reports", "Reports"]);
  nav.innerHTML = links.map(([h, l]) => `<a href="${h}">${l}</a>`).join("") +
    `<input id="gq" placeholder="Search…" style="padding:.25rem .5rem;border-radius:6px;border:0" />`;
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
