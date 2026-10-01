// api.js — typed fetch wrapper: JWT + tenant scoping + offline awareness.
const Api = (() => {
  let tenant = localStorage.getItem("idre.tenant") || null;

  function setTenant(t) { tenant = t; localStorage.setItem("idre.tenant", t); }
  function getTenant() { return tenant; }

  async function req(method, path, body, isForm) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    const headers = { Authorization: `Bearer ${tok}` };
    if (body && !isForm) headers["Content-Type"] = "application/json";
    const resp = await fetch(window.IDRE_CONFIG.apiBase + path, {
      method, headers,
      body: isForm ? body : body ? JSON.stringify(body) : undefined,
    });
    if (resp.status === 401) { Auth.login(); throw new Error("redirecting"); }
    const data = resp.headers.get("content-type")?.includes("json") ? await resp.json() : await resp.text();
    if (!resp.ok) {
      const msg = typeof data === "object" && data.error ? data.error : `HTTP ${resp.status}`;
      const err = new Error(msg); err.status = resp.status; throw err;
    }
    return data;
  }

  const t = () => `/v1/tenants/${tenant}`;
  return {
    setTenant, getTenant,
    cases: {
      list: () => req("GET", `${t()}/cases`),
      get: (id) => req("GET", `${t()}/cases/${id}`),
      initiate: (payload) => req("POST", `${t()}/cases/initiate`, payload),
      signal: (id, signal, data) => req("POST", `${t()}/cases/${id}/signal`, { signal, data }),
      upload: (id, file, sealed) => {
        const fd = new FormData();
        fd.append("file", file);
        if (sealed) fd.append("sealed", "true");
        return req("POST", `${t()}/cases/${id}/documents`, fd, true);
      },
      documents: (id) => req("GET", `${t()}/cases/${id}/documents`),
      analysis: (id, docId) => req("GET", `${t()}/cases/${id}/documents/${docId}/analysis`),
      downloadUrl: (id, docId) => `${t()}/cases/${id}/documents/${docId}/download`,
      activities: (id) => req("GET", `${t()}/cases/${id}/activities`),
    },
    onboarding: {
      submit: (payload) => req("POST", `${t()}/onboarding/applications`, payload),
      list: () => req("GET", `${t()}/onboarding/applications`),
      decide: (id, decision, reason) => req("POST", `${t()}/onboarding/applications/${id}/decision`, { decision, reason }),
    },
    voice: {
      intake: () => req("GET", `${t()}/voice/intake`),
      logs: () => req("GET", `${t()}/voice/logs`),
      outbound: (to, caseNumber, script) =>
        req("POST", `${t()}/voice/outbound`, { to, case_number: caseNumber, script }),
    },
    reports: {
      sla: () => req("GET", `${t()}/reports/sla`),
      summary: () => req("GET", `${t()}/reports/summary`),
    },
    crm: {
      accounts: (type) => req("GET", `${t()}/accounts${type ? `?type=${type}` : ""}`),
      createAccount: (p) => req("POST", `${t()}/accounts`, p),
      account360: (id) => req("GET", `${t()}/accounts/${id}/360`),
      createContact: (p) => req("POST", `${t()}/contacts`, p),
      leads: () => req("GET", `${t()}/leads`),
      convertLead: (id, type) => req("POST", `${t()}/leads/${id}/convert`, { type }),
      tasks: (mine) => req("GET", `${t()}/tasks${mine ? "?mine=true" : ""}`),
      createTask: (p) => req("POST", `${t()}/tasks`, p),
      completeTask: (id) => req("POST", `${t()}/tasks/${id}/complete`),
      addNote: (p) => req("POST", `${t()}/notes`, p),
      search: (q) => req("GET", `${t()}/search?q=${encodeURIComponent(q)}`),
    },
    cm: {
      assign: (caseId, role, assignee) => req("POST", `${t()}/cases/${caseId}/assign`, { role, assignee }),
      escalate: (caseId, clock, detail) => req("POST", `${t()}/cases/${caseId}/escalate`, { clock, detail }),
      relate: (p) => req("POST", `${t()}/cases/relate`, p),
      relationships: (caseId) => req("GET", `${t()}/cases/${caseId}/relationships`),
      checklist: (caseId) => req("GET", `${t()}/cases/${caseId}/checklist`),
      checkItem: (itemId) => req("POST", `${t()}/checklists/${itemId}/check`),
      calendar: () => req("GET", `${t()}/calendar`),
      notifications: () => req("GET", `${t()}/notifications`),
      readNotif: (id) => req("POST", `${t()}/notifications/${id}/read`),
      views: () => req("GET", `${t()}/views`),
      saveView: (p) => req("POST", `${t()}/views`, p),
      clocks: (id) => req("GET", `${t()}/cases/${id}/clocks`),
      allClocks: () => req("GET", `${t()}/cases/clocks`),
      bulk: (p) => req("POST", `${t()}/cases/bulk`, p),
      grabNext: () => req("POST", `${t()}/queues/grab-next`),
      letter: (caseId, template, qs) => req("POST", `${t()}/cases/${caseId}/letters/${template}${qs || ""}`),
    },
    fees: {
      transfer: (p) => req("POST", `${t()}/fees/transfer`, p),
    },
    geo: {
      mapUrl: () => `${config.caseApiBase}/geo/map`,
    },
  };
})();
