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
    let data = resp.headers.get("content-type")?.includes("json") ? await resp.json() : await resp.text();
    // case-api answers refusals with Go's http.Error, which labels a JSON body
    // text/plain: read it as JSON anyway, or every refusal on every page shows
    // as a bare "HTTP 409" instead of the reason the server gave.
    if (typeof data === "string" && /^\s*\{/.test(data)) {
      try { data = JSON.parse(data); } catch { /* keep the text */ }
    }
    if (!resp.ok) {
      const msg = typeof data === "object" && data.error ? data.error : `HTTP ${resp.status}`;
      const err = new Error(msg); err.status = resp.status; err.data = data;
      if (typeof data === "object" && data.details) err.details = data.details;
      throw err;
    }
    return data;
  }

  // reqRaw: raw-text bodies (NACHA return files are not JSON).
  async function reqRaw(method, path, raw) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    const resp = await fetch(window.IDRE_CONFIG.apiBase + path, {
      method, headers: { Authorization: `Bearer ${tok}`, "Content-Type": "text/plain" }, body: raw,
    });
    if (resp.status === 401) { Auth.login(); throw new Error("redirecting"); }
    const data = resp.headers.get("content-type")?.includes("json") ? await resp.json() : await resp.text();
    if (!resp.ok) throw new Error(typeof data === "object" && data.error ? data.error : `HTTP ${resp.status}`);
    return data;
  }

  // download: every file-serving endpoint requires the same Bearer auth as
  // every other API call -- a plain <a href> can never attach it, which is
  // exactly why "download" and the documents.zip link both 403'd with
  // "missing bearer token" when clicked directly. Fetch with the token,
  // then hand the browser a blob: URL to save instead.
  async function blobURL(path) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    const resp = await fetch(window.IDRE_CONFIG.apiBase + path, { headers: { Authorization: `Bearer ${tok}` } });
    if (resp.status === 401) { Auth.login(); throw new Error("redirecting"); }
    if (!resp.ok) throw new Error(`fetch failed (HTTP ${resp.status})`);
    return URL.createObjectURL(await resp.blob());
  }

  async function download(path, fallbackName) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    const resp = await fetch(window.IDRE_CONFIG.apiBase + path, { headers: { Authorization: `Bearer ${tok}` } });
    if (resp.status === 401) { Auth.login(); throw new Error("redirecting"); }
    if (!resp.ok) throw new Error(`download failed (HTTP ${resp.status})`);
    const cd = resp.headers.get("content-disposition") || "";
    const name = (cd.match(/filename="?([^"]+)"?/) || [])[1] || fallbackName || "download";
    const blobUrl = URL.createObjectURL(await resp.blob());
    const a = document.createElement("a");
    a.href = blobUrl; a.download = name;
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(blobUrl), 10000);
  }

  const t = () => `/v1/tenants/${tenant}`;
  // query-string builder: drops empty values, returns "" or "?a=1&b=2"
  const qs = (params) => {
    const s = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== "" && v != null)).toString();
    return s ? `?${s}` : "";
  };
  return {
    setTenant, getTenant, download,
    analytics: {
      payers: () => req("GET", `${t()}/analytics/payers`),
      fraudSignals: () => req("GET", `${t()}/analytics/fraud-signals`),
      networkAdequacy: () => req("GET", `${t()}/analytics/network-adequacy`),
    },
    offers: {
      submit: (caseId, p) => req("POST", `${t()}/cases/${caseId}/offers`, p),
      list: (caseId) => req("GET", `${t()}/cases/${caseId}/offers`),
      reveal: (caseId) => req("POST", `${t()}/cases/${caseId}/offers/reveal`),
      determine: (caseId, p) => req("POST", `${t()}/cases/${caseId}/determination`, p),
    },
    reviews: {
      request: (caseId, p) => req("POST", `${t()}/cases/${caseId}/reviews`, p),
      queue: (discipline, all, issue) => req("GET", `${t()}/queues/${discipline}${all || issue ? `?${all ? "all=1&" : ""}${issue ? `issue=${issue}` : ""}`.replace(/&$/, "") : ""}`),
      pmQueue: () => req("GET", `${t()}/queues/pm`),
      claim: (id) => req("POST", `${t()}/reviews/${id}/claim`),
      // extra: what the case's Steps panel asks for the same step (coding: clinical_requested).
      complete: (id, note, extra) => req("POST", `${t()}/reviews/${id}/complete`, { note, ...(extra || {}) }),
    },
    party: {
      cases: () => req("GET", `${t()}/party/cases`),
      caseDetail: (id) => req("GET", `${t()}/party/cases/${id}`),
      invite: (caseId, p) => req("POST", `${t()}/cases/${caseId}/party-invites`, p),
      accounts: (caseId) => req("GET", `${t()}/cases/${caseId}/party-accounts`),
    },
    admin: {
      // Not tenant-scoped -- creates the /tenant/<xx> group a new tenant
      // needs to be reachable at all. FEDERAL_ADMIN/PLATFORM_ADMIN only.
      createTenant: (payload) => req("POST", "/v1/admin/tenants", payload),
      setFeatures: (tenant, features) => req("PUT", `/v1/admin/tenants/${tenant}/features`, { features }),
      // Cross-tenant: creates a FEDERAL_ADMIN/PLATFORM_ADMIN account.
      // FEDERAL_ADMIN/PLATFORM_ADMIN only.
      createFederalAdmin: (payload) => req("POST", "/v1/admin/federal-admins", payload),
      // Adds staff to an EXISTING tenant. FEDERAL_ADMIN/PLATFORM_ADMIN for
      // any tenant, or CASE_MANAGER for their own tenant only.
      createTenantStaff: (payload) => req("POST", "/v1/admin/tenant-staff", payload),
      listFederalAdmins: () => req("GET", "/v1/admin/federal-admins"),
      listTenantStaff: (tenant) => req("GET", `/v1/admin/tenant-staff?tenant=${encodeURIComponent(tenant)}`),
      // Names only, for picking a colleague; open to any case staff of the tenant.
      listStaffNames: (tenant) => req("GET", `/v1/admin/tenant-staff?tenant=${encodeURIComponent(tenant)}&brief=1`),
      setTenantStaffEnabled: (tenant, username, enabled) =>
        req("PATCH", `/v1/admin/tenant-staff/${encodeURIComponent(username)}`, { tenant, enabled }),
      deleteTenantStaff: (tenant, username) =>
        req("DELETE", `/v1/admin/tenant-staff/${encodeURIComponent(username)}?tenant=${encodeURIComponent(tenant)}`),
      setFederalAdminEnabled: (username, enabled) =>
        req("PATCH", `/v1/admin/federal-admins/${encodeURIComponent(username)}`, { enabled }),
      deleteFederalAdmin: (username) =>
        req("DELETE", `/v1/admin/federal-admins/${encodeURIComponent(username)}`),
    },
    auditLog: {
      list: (params = {}) => {
        const qs = new URLSearchParams(Object.entries(params).filter(([, v]) => v)).toString();
        return req("GET", `${t()}/audit-log${qs ? "?" + qs : ""}`);
      },
    },
    cases: {
      // Keyset-paginated: {cases, next_cursor, total}. Legacy bare-array
      // responses are normalized so older backends still work.
      list: (params = {}) => {
        const qs = new URLSearchParams(Object.entries(params).filter(([, v]) => v)).toString();
        return req("GET", `${t()}/cases${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { cases: r, next_cursor: "", total: r.length } : r));
      },
      get: (id) => req("GET", `${t()}/cases/${id}`),
      initiate: (payload) => req("POST", `${t()}/cases/initiate`, payload),
      signal: (id, signal, data) => req("POST", `${t()}/cases/${id}/signal`, { signal, data }),
      benchmarks: (id) => req("GET", `${t()}/cases/${id}/benchmarks`),
      winProbability: (id) => req("GET", `${t()}/cases/${id}/win-probability`),
      batchable: (id) => req("GET", `${t()}/cases/${id}/batchable`),
      upload: (id, file, sealed, folder) => {
        const fd = new FormData();
        fd.append("file", file);
        if (sealed) fd.append("sealed", "true");
        if (folder) fd.append("folder", folder);
        return req("POST", `${t()}/cases/${id}/documents`, fd, true);
      },
      moveDoc: (id, docId, folder) => req("PATCH", `${t()}/cases/${id}/documents/${docId}`, { folder }),
      zipUrl: (id) => `${t()}/cases/${id}/documents.zip`,
      setDetails: (id, details) => req("PATCH", `${t()}/cases/${id}/details`, details),
      lettergen: (id, templateKey) => req("POST", `${t()}/cases/${id}/lettergen/${templateKey}`),
      documents: (id) => req("GET", `${t()}/cases/${id}/documents`),
      timeAdd: (id, p) => req("POST", `${t()}/cases/${id}/time`, p),
      timeList: (id) => req("GET", `${t()}/cases/${id}/time`),
      analysis: (id, docId) => req("GET", `${t()}/cases/${id}/documents/${docId}/analysis`),
      retryAnalysis: (id, docId) => req("POST", `${t()}/cases/${id}/documents/${docId}/analysis/retry`),
      analysisFeedback: (id, docId, p) => req("POST", `${t()}/cases/${id}/documents/${docId}/analysis/feedback`, p),
      downloadUrl: (id, docId) => `${t()}/cases/${id}/documents/${docId}/download`,
      activities: (id) => req("GET", `${t()}/cases/${id}/activities`),
    },
    // Personal timesheet: daily entry screen with day/week/month totals.
    time: {
      mine: (from, to) => req("GET", `${t()}/time/mine${from||to ? `?from=${from||""}&to=${to||""}` : ""}`),
      summary: () => req("GET", `${t()}/time/summary`),
      prompts: () => req("GET", `${t()}/time/prompts`),
      update: (id, p) => req("PUT", `${t()}/time/${id}`, p),
      remove: (id) => req("DELETE", `${t()}/time/${id}`),
    },
    onboarding: {
      submit: (payload) => req("POST", `${t()}/onboarding/applications`, payload),
      list: (params) => req("GET", `${t()}/onboarding/applications${qs(params || {})}`),
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
      // Offset-paginated: {accounts, total, next_offset} (next_offset = -1 at end).
      accounts: (type, params = {}) => {
        const qs = new URLSearchParams({ ...(type ? { type } : {}), ...params }).toString();
        return req("GET", `${t()}/accounts${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { accounts: r, total: r.length, next_offset: -1 } : r));
      },
      createAccount: (p) => req("POST", `${t()}/accounts`, p),
      account360: (id) => req("GET", `${t()}/accounts/${id}/360`),
      createContact: (p) => req("POST", `${t()}/contacts`, p),
      leads: (params = {}) => {
        const qs = new URLSearchParams(params).toString();
        return req("GET", `${t()}/leads${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { leads: r, total: r.length, next_offset: -1 } : r));
      },
      convertLead: (id, type) => req("POST", `${t()}/leads/${id}/convert`, { type }),
      tasks: (mine, params = {}) => {
        const qs = new URLSearchParams({ ...(mine ? { mine: "true" } : {}), ...params }).toString();
        return req("GET", `${t()}/tasks${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { tasks: r, total: r.length, next_offset: -1 } : r));
      },
      createTask: (p) => req("POST", `${t()}/tasks`, p),
      completeTask: (id) => req("POST", `${t()}/tasks/${id}/complete`),
      addNote: (p) => req("POST", `${t()}/notes`, p),
      notes: (recordType, recordId) => req("GET", `${t()}/notes?record_type=${encodeURIComponent(recordType)}&record_id=${encodeURIComponent(recordId)}`),
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
      readAllNotifs: () => req("POST", `${t()}/notifications/read-all`),
      views: () => req("GET", `${t()}/views`),
      saveView: (p) => req("POST", `${t()}/views`, p),
      clocks: (id) => req("GET", `${t()}/cases/${id}/clocks`),
      allClocks: () => req("GET", `${t()}/cases/clocks`),
      bulk: (p) => req("POST", `${t()}/cases/bulk`, p),
      grabNext: () => req("POST", `${t()}/queues/grab-next`),
      letter: (caseId, template, qs) => req("POST", `${t()}/cases/${caseId}/letters/${template}${qs || ""}`),
    },
    graph: {
      ask: (question, k) => req("POST", `${t()}/graph/ask`, { question, k }),
      hints: () => req("GET", `${t()}/graph/hints`),
      feedback: (logId, rating) => req("POST", `${t()}/graph/feedback`, { log_id: logId, rating }),
      related: (caseId) => req("GET", `${t()}/cases/${caseId}/related`),
      neighbors: (caseId) => req("GET", `${t()}/cases/${caseId}/graph-neighbors`),
      sync: () => req("POST", `${t()}/graph/sync`),
      train: (epochs) => req("POST", `${t()}/graph/train`, epochs ? { epochs } : {}),
    },
    prefs: {
      all: () => req("GET", `${t()}/prefs`),
      put: (key, value) => req("PUT", `${t()}/prefs`, { key, value }),
    },
    program: {
      get: () => req("GET", `${t()}/program`),
      setLabel: (label) => req("PUT", `${t()}/program/label`, { label }),
      tenantLabels: () => req("GET", `${t()}/admin/tenant-labels`),
      rules: () => req("GET", `${t()}/rules`),
      saveRules: (rules, note) => req("PUT", `${t()}/rules`, { rules, note }),
      rulesAudit: () => req("GET", `${t()}/rules/audit`),
      setDate: (caseId, key, value) => req("POST", `${t()}/cases/${caseId}/program-date`, { key, value }),
      setStatus: (caseId, p) => req("POST", `${t()}/cases/${caseId}/status`, p),
      eligibility: (caseId, p) => req("POST", `${t()}/cases/${caseId}/eligibility`, p),
      eligibilityAuto: (caseId) => req("POST", `${t()}/cases/${caseId}/eligibility/auto`),
      copilotBrief: (caseId) => req("POST", `${t()}/cases/${caseId}/copilot/brief`),
      copilotBriefLatest: (caseId) => req("GET", `${t()}/cases/${caseId}/copilot/brief`),
      copilotDraft: (caseId, kind, instructions, to) => req("POST", `${t()}/cases/${caseId}/copilot/draft`, { kind, instructions, to }),
      copilotProposeActions: (caseId) => req("POST", `${t()}/cases/${caseId}/copilot/actions`),
      copilotListActions: (caseId) => req("GET", `${t()}/cases/${caseId}/copilot/actions`),
      copilotDecideActions: (caseId, batchId, decision) => req("POST", `${t()}/cases/${caseId}/copilot/actions/${batchId}/decision`, { decision }),
      copilotChat: (caseId, message) => req("POST", `${t()}/cases/${caseId}/copilot/chat`, { message }),
      copilotChatHistory: (caseId) => req("GET", `${t()}/cases/${caseId}/copilot/chat`),
      copilotChatLog: (caseId, user, assistant) => req("POST", `${t()}/cases/${caseId}/copilot/chat/log`, { user, assistant }),
      copilotGeneralChat: (message) => req("POST", `${t()}/copilot/chat`, { message }),
      copilotGeneralChatHistory: () => req("GET", `${t()}/copilot/chat`),
      copilotGeneralChatLog: (user, assistant) => req("POST", `${t()}/copilot/chat/log`, { user, assistant }),
      copilotThreadShares: (caseId) => req("GET", `${t()}/cases/${caseId}/copilot/chat/shares`),
      copilotShareThread: (caseId, userId) => req("POST", `${t()}/cases/${caseId}/copilot/chat/shares`, { user_id: userId }),
      copilotUnshareThread: (caseId, userId) => req("DELETE", `${t()}/cases/${caseId}/copilot/chat/shares/${userId}`),
      briefing: () => req("GET", `${t()}/assistant/briefing`),
      eligibilityHistory: (caseId) => req("GET", `${t()}/cases/${caseId}/eligibility`),
      workflow: (caseId) => req("GET", `${t()}/cases/${caseId}/workflow`),
      eligibilityChecklist: (caseId) => download(`${t()}/cases/${caseId}/eligibility/checklist`, "Eligibility Checklist.html"),
      setReviewNote: (caseId, field, text) => req("PUT", `${t()}/cases/${caseId}/review-notes`, { field, text }),
      setDisputeSummary: (caseId, body) => req("PUT", `${t()}/cases/${caseId}/dispute-summary`, body),
      confirmAor: (caseId, valid, reason) => req("POST", `${t()}/cases/${caseId}/aor-confirmation`, { valid, reason }),
      optOut: (caseId, eligible, rationale) => req("POST", `${t()}/cases/${caseId}/opt-out`, { eligible, rationale }),
      send: (caseId, p) => req("POST", `${t()}/cases/${caseId}/correspondence`, p),
      correspondence: (caseId) => req("GET", `${t()}/cases/${caseId}/correspondence`),
      shareLink: (caseId, kind, days) => req("POST", `${t()}/cases/${caseId}/share-links`, { kind, days_ttl: days }),
      invoices: (caseId) => req("GET", `${t()}/cases/${caseId}/invoices`),
      issueInvoice: (caseId, p) => req("POST", `${t()}/cases/${caseId}/invoices`, p),
      settleInvoice: (invId, action, ref) => req("POST", `${t()}/invoices/${invId}/settle`, { action, remittance_ref: ref }),
      receivables: () => req("GET", `${t()}/reports/receivables`),
      financial: () => req("GET", `${t()}/reports/financial`),
      timeReport: (opts) => req("GET", `${t()}/reports/time${qs(opts || {})}`),
      timeReportSend: (p) => req("POST", `${t()}/time/report/send`, p),
      timeRates: () => req("GET", `${t()}/reports/time/rates`),
      timeRateSet: (role, rate_cents_per_hour) => req("PUT", `${t()}/reports/time/rates`, { role, rate_cents_per_hour }),
      opsDashboard: () => req("GET", `${t()}/ops/dashboard`),
      caseFinancials: (caseId) => req("GET", `${t()}/cases/${caseId}/financials`),
      // Service-fee invoicing engine
      billingGenerate: (params, body) => req("POST", `${t()}/billing/invoices${qs(params || {})}`, body || {}),
      billingList: (opts) => req("GET", `${t()}/billing/invoices${qs(opts || {})}`),
      billingGet: (id) => req("GET", `${t()}/billing/invoices/${id}`),
      billingAct: (id, action) => req("POST", `${t()}/billing/invoices/${id}/${action}`, {}),
      billingPay: (id, body) => req("POST", `${t()}/billing/invoices/${id}/payments`, body),
      billingExport: async (id) => download(`${t()}/billing/invoices/${id}/export`, "invoice.csv"),
      // Reconciliation engine + adapters
      reconImportCsv: (mapping, file) => {
        const fd = new FormData();
        fd.append("source", "csv_generic"); fd.append("mapping", mapping); fd.append("file", file);
        return req("POST", `${t()}/recon/import`, fd, true);
      },
      reconFetch: (feed) => req("POST", `${t()}/recon/import`, { source: "http_json", feed }),
      reconMatch: (id) => req("POST", `${t()}/recon/batches/${id}/match`, {}),
      reconBatches: () => req("GET", `${t()}/recon/batches`),
      reconBatch: (id) => req("GET", `${t()}/recon/batches/${id}`),
      reconResolve: (itemId, body) => req("POST", `${t()}/recon/items/${itemId}/resolve`, body),
      reconSummary: (opts) => req("GET", `${t()}/recon/summary${qs(opts || {})}`),
      // AR/AP subledger
      receivablesAging: (asOf) => req("GET", `${t()}/arap/receivables${qs(asOf ? { as_of: asOf } : {})}`),
      payablesList: (status, asOf) => req("GET", `${t()}/arap/payables${qs({ status: status || "OPEN", ...(asOf ? { as_of: asOf } : {}) })}`),
      payableCreate: (body) => req("POST", `${t()}/arap/payables`, body),
      payableAct: (id, action, body) => req("POST", `${t()}/arap/payables/${id}/${action}`, body || {}),
      flWeeklyReport: (from, to) => download(`${t()}/reports/fl/weekly${qs({ from: from || "", to: to || "" })}`, "fl-weekly.csv"),
      flMonthlyReport: (from, to) => download(`${t()}/reports/fl/monthly${qs({ from: from || "", to: to || "" })}`, "fl-monthly.csv"),
      flFinalReport: () => download(`${t()}/reports/fl/final`, "fl-final.csv"),
      flAdhocReport: (o) => download(`${t()}/reports/fl/adhoc${qs(o || {})}`, "fl-adhoc.csv"),
      flReportColumns: () => req("GET", `${t()}/reports/fl/columns`),
      documentAs835: (caseId, docId) => download(`${t()}/cases/${caseId}/documents/${docId}/835.csv`, "835.csv"),
      qaSend: (id, to, cc) => req("POST", `${t()}/qa/${id}/send`, { to, cc }),
      arapSummary: (asOf) => req("GET", `${t()}/arap/summary${qs(asOf ? { as_of: asOf } : {})}`),
      nachaPayout: (body) => req("POST", `${t()}/arap/payouts/nacha`, body || {}),
      payoutBatches: () => req("GET", `${t()}/arap/payouts`),
      payoutBatchFile: (id) => req("GET", `${t()}/arap/payouts/${id}/file`),
      // Third-party administrators (file/pay/track on behalf of initiating parties)
      tpaRegister: (body) => req("POST", "/api/public/tpa/register", body),
      tpaClaim: (code) => req("POST", `${t()}/tpa/claim`, { code }),
      tpaUnlink: () => req("POST", `${t()}/tpa/unlink`, {}),
      tpaInviteCode: () => req("POST", `${t()}/tpa/invite-code`, {}),
      tpaMe: () => req("GET", `${t()}/tpa/me`),
      tpaClients: () => req("GET", `${t()}/tpa/clients`),
      tpaAddClient: (body) => req("POST", `${t()}/tpa/clients`, body),
      tpaClientStatus: (id, status) => req("POST", `${t()}/tpa/clients/${id}/status`, { status }),
      tpaIntake: (body) => req("POST", `${t()}/tpa/intake`, body),
      tpaEditDispute: (caseId, body) => req("PATCH", `${t()}/tpa/disputes/${caseId}`, body),
      tpaDashboard: (tpaId) => req("GET", `${t()}/tpa/dashboard${qs(tpaId ? { tpa_id: tpaId } : {})}`),
      adminTpas: () => req("GET", `${t()}/admin/tpas`),
      adminTpaStatus: (id, status) => req("POST", `${t()}/admin/tpas/${id}/status`, { status }),
      tpaOnboarding: () => req("GET", `${t()}/tpa/onboarding`),
      tpaClientProfile: (clientId, body) => req("PUT", `${t()}/tpa/clients/${clientId}/profile`, body),
      tpaImportClients: (file) => {
        const fd = new FormData();
        fd.append("file", file);
        return req("POST", `${t()}/tpa/clients/import`, fd, true);
      },
      tpaPayments: (clientId) => req("GET", `${t()}/tpa/clients/${clientId}/payments`),
      tpaAddPayment: (clientId, body) => req("POST", `${t()}/tpa/clients/${clientId}/payments`, body),
      tpaCreatePackage: (clientId, mode) => req("POST", `${t()}/tpa/clients/${clientId}/packages`, { mode }),
      tpaPackage: (packageId) => req("GET", `${t()}/tpa/packages/${packageId}`),
      tpaAttachDoc: (packageId, slot, file) => {
        const fd = new FormData();
        fd.append("slot", slot);
        fd.append("file", file);
        return req("POST", `${t()}/tpa/packages/${packageId}/docs`, fd, true);
      },
      tpaUploadMerged: (packageId, file) => {
        const fd = new FormData();
        fd.append("file", file);
        return req("POST", `${t()}/tpa/packages/${packageId}/merged`, fd, true);
      },
      tpaBulkPackages: (file) => {
        const fd = new FormData();
        fd.append("file", file);
        return req("POST", `${t()}/tpa/packages/bulk`, fd, true);
      },
      // Many disputes, each with its own documents: disputes.csv + a folder per dispute.
      tpaBulkDisputes: (batchRef, file) => {
        const fd = new FormData(); fd.append("batch_ref", batchRef); fd.append("file", file);
        return req("POST", `${t()}/tpa/disputes/bulk`, fd, true);
      },
      tpaBulkDisputeBatches: () => req("GET", `${t()}/tpa/disputes/bulk`),
      tpaBulkDisputeBatch: (id) => req("GET", `${t()}/tpa/disputes/bulk/${id}`),
      healthPlanRegister: (body) => req("POST", "/api/public/healthplan/register", body),
      hpClaim: (code) => req("POST", `${t()}/hp/claim`, { code }),
      hpUnlink: () => req("POST", `${t()}/hp/unlink`, {}),
      onboardingReviews: () => req("GET", `${t()}/onboarding/reviews`),
      onboardingReviewFile: (kind, docId) => blobURL(`${t()}/onboarding/reviews/${kind}/${docId}/file`),
      onboardingReviewDecide: (kind, docId, decision, note) => req("POST", `${t()}/onboarding/reviews/${kind}/${docId}`, { decision, note }),
      hpMe: () => req("GET", `${t()}/hp/me`),
      hpOnboarding: () => req("GET", `${t()}/hp/onboarding`),
      hpAddParty: (body) => req("POST", `${t()}/hp/parties`, body),
      hpPartyProfile: (partyId, body) => req("PUT", `${t()}/hp/parties/${partyId}/profile`, body),
      hpImportParties: (file) => {
        const fd = new FormData();
        fd.append("file", file);
        return req("POST", `${t()}/hp/parties/import`, fd, true);
      },
      hpPayments: (partyId) => req("GET", `${t()}/hp/parties/${partyId}/payments`),
      hpAddPayment: (partyId, body) => req("POST", `${t()}/hp/parties/${partyId}/payments`, body),
      hpCreatePackage: (partyId, mode) => req("POST", `${t()}/hp/parties/${partyId}/packages`, { mode }),
      hpPackage: (packageId) => req("GET", `${t()}/hp/packages/${packageId}`),
      hpAttachDoc: (packageId, slot, file) => {
        const fd = new FormData();
        fd.append("slot", slot);
        fd.append("file", file);
        return req("POST", `${t()}/hp/packages/${packageId}/docs`, fd, true);
      },
      hpUploadMerged: (packageId, file) => {
        const fd = new FormData();
        fd.append("file", file);
        return req("POST", `${t()}/hp/packages/${packageId}/merged`, fd, true);
      },
      hpBulkPackages: (file) => {
        const fd = new FormData();
        fd.append("file", file);
        return req("POST", `${t()}/hp/packages/bulk`, fd, true);
      },
      hpIntake: (body) => req("POST", `${t()}/hp/intake`, body),
      adminHealthPlans: () => req("GET", `${t()}/admin/healthplans`),
      adminHealthPlanStatus: (id, status) => req("POST", `${t()}/admin/healthplans/${id}/status`, { status }),
      pingPresence: (name) => req("POST", `${t()}/presence/ping`, { name }),
      checks: (status, opts) => req("GET", `${t()}/checks${qs({ status: status || "", ...(opts || {}) })}`),
      clearCheck: (checkId, remittanceRef) => req("POST", `${t()}/checks/${checkId}/clear`, { remittance_ref: remittanceRef }),
      matchCheck: (checkId, body) => req("POST", `${t()}/checks/${checkId}/match`, body),
      checkFeedback: (checkId, p) => req("POST", `${t()}/checks/${checkId}/feedback`, p),
      checkImage: (checkId) => blobURL(`${t()}/checks/${checkId}/image`),
      uploadCheck: (file) => {
        const fd = new FormData();
        fd.append("check", file);
        return req("POST", `${t()}/checks`, fd, true);
      },
      payments: (caseId, opts) => req("GET", (caseId ? `${t()}/cases/${caseId}/payments` : `${t()}/payments`) + qs(opts || {})),
      checkout: (invId) => req("POST", `${t()}/invoices/${invId}/checkout`),
      claims: (caseId) => req("GET", `${t()}/cases/${caseId}/claims`),
      importClaims: (caseId, claims) => req("POST", `${t()}/cases/${caseId}/claims`, { claims }),
      qaQueue: (caseId) => req("GET", `${t()}/qa${caseId ? `?case_id=${encodeURIComponent(caseId)}` : ""}`),
      qaGet: (id) => req("GET", `${t()}/qa/${id}`),
      qaDecision: (id, decision, note, editedBody, to, cc) => req("POST", `${t()}/qa/${id}/decision`, { decision, note, edited_body: editedBody || undefined, to, cc }),
      mailJournal: (opts) => req("GET", `${t()}/correspondence${qs(opts || {})}`),
      // Payment rails (moov-io): vault-sealed accounts, ACH/wire/ICL/Metro2 files, OFAC screening
      railAccounts: () => req("GET", `${t()}/payments/accounts`),
      allInvoices: () => req("GET", `${t()}/invoices?limit=500`),
      railAccountAdd: (body) => req("POST", `${t()}/payments/accounts`, body),
      railAccountDel: (id) => req("DELETE", `${t()}/payments/accounts/${id}`),
      railRouting: (num) => req("GET", `${t()}/payments/routing/${encodeURIComponent(num)}`),
      railAchDebits: (body) => req("POST", `${t()}/payments/ach/debits`, body),
      railAchReturns: (raw) => reqRaw("POST", `${t()}/payments/ach/returns`, raw),
      railAttachAuth: (id, raw, name) =>
        reqRaw("POST", `${t()}/payments/accounts/${id}/authorization?name=${encodeURIComponent(name)}`, raw),
      railWire: (body) => req("POST", `${t()}/payments/wire`, body),
      railIcl: () => req("POST", `${t()}/payments/icl`, {}),
      railMetro2: (body) => req("POST", `${t()}/payments/metro2`, body),
      railFiles: (kind) => req("GET", `${t()}/payments/files${qs(kind ? { kind } : {})}`),
      railFileItems: (id) => req("GET", `${t()}/payments/files/${id}/items`),
      railFileBody: (id) => req("GET", `${t()}/payments/files/${id}/file`),
      screeningStatus: () => req("GET", `${t()}/screening/status`),
      screeningCheck: (body) => req("POST", `${t()}/screening/check`, body),
      screeningChecks: (opts) => req("GET", `${t()}/screening/checks${qs(opts || {})}`),
      intake: (opts) => req("GET", `${t()}/intake${qs(opts || {})}`),
      createIntake: (p) => req("POST", `${t()}/intake`, p),
      intakeBulk: (p) => req("POST", `${t()}/intake/bulk`, p),
      intakeBatches: (submitter) => req("GET", `${t()}/intake/bulk${submitter ? `?submitter=${encodeURIComponent(submitter)}` : ""}`),
      intakeBatch: (id) => req("GET", `${t()}/intake/bulk/${id}`),
      intakeConverse: (message, fields, history) => req("POST", `${t()}/intake/converse`, { message, fields, history }),
      advanceIntake: (id, status, caseId) => req("POST", `${t()}/intake/${id}/advance`, { status, case_id: caseId }),
      deliverables: () => req("GET", `${t()}/deliverables`),
      recipients: () => req("GET", `${t()}/program/recipients`),
      setRecipients: (body) => req("PUT", `${t()}/program/recipients`, body),
      templates: () => req("GET", `${t()}/program/templates`),
      templateSave: (key, body) => req("PUT", `${t()}/program/templates/${encodeURIComponent(key)}`, body),
      templateDelete: (key) => req("DELETE", `${t()}/program/templates/${encodeURIComponent(key)}`),
      templateHistory: (key) => req("GET", `${t()}/program/templates/${encodeURIComponent(key)}/history`),
      templatePreview: (body) => req("POST", `${t()}/program/templates/preview`, body),
      templateUsageSet: (body) => req("PUT", `${t()}/program/templates/usage`, body),
      templateUsageDelete: (id) => req("DELETE", `${t()}/program/templates/usage/${id}`),
      caseTemplates: (caseId) => req("GET", `${t()}/cases/${caseId}/correspondence/templates`),
      setContractDates: (dates) => req("PUT", `${t()}/program/contract-dates`, dates),
      recordOutcome: (caseId, body) => req("POST", `${t()}/cases/${caseId}/outcome`, body),
      fwa: (caseId, body) => req("POST", `${t()}/cases/${caseId}/fwa`, body),
      submitDeliverable: (p) => req("POST", `${t()}/deliverables`, p),
      requestDeliverable: (name, contract_ref) => req("POST", `${t()}/deliverables/request`, { name, contract_ref }),
      calls: (caseId) => req("GET", `${t()}/cases/${caseId}/calls`),
      logCall: (caseId, p) => req("POST", `${t()}/cases/${caseId}/calls`, p),
      inquiries: (caseId) => req("GET", `${t()}/cases/${caseId}/inquiries`),
      logInquiry: (caseId, p) => req("POST", `${t()}/cases/${caseId}/inquiries`, p),
    },
    fees: {
      transfer: (p) => req("POST", `${t()}/fees/transfer`, p),
    },
    geo: {
      mapUrl: () => `${config.caseApiBase}/geo/map`,
    },
  };
})();
