// Author: Patrick Munis at newwave
// api.js — typed fetch wrapper: JWT + tenant scoping + offline awareness.
const Api = (() => {
  let tenant = localStorage.getItem("idre.tenant") || null;

  function setTenant(t) { tenant = t; localStorage.setItem("idre.tenant", t); }
  function getTenant() { return tenant; }

  async function req(method, path, body, isForm) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    // A file in the form: checked against the upload rules first; a big one
    // goes up in resumable chunks and the form carries its upload_id instead.
    let sent = null;
    if (isForm && body instanceof FormData && tenant) {
      sent = await Uploads.prepare(body);
      body = sent.form;
    }
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
    if (resp.ok && sent) sent.done();
    if (!resp.ok) {
      const msg = typeof data === "object" && data.error ? data.error : `HTTP ${resp.status}`;
      const err = new Error(msg); err.status = resp.status; err.data = data;
      if (typeof data === "object" && data.details) err.details = data.details;
      if (typeof data === "object" && Array.isArray(data.problems)) err.problems = data.problems;
      if (typeof data === "object" && data.workflow_ended) err.workflowEnded = true;
      throw err;
    }
    // A change was saved: a case page that is open re-reads its steps (views.js, wfRefresh).
    if (method !== "GET") window.dispatchEvent(new CustomEvent("idre:changed", { detail: { method, path } }));
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

  // A file as a Blob, with the name the server gives it (the document viewer).
  async function fetchBlob(path) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    const resp = await fetch(window.IDRE_CONFIG.apiBase + path, { headers: { Authorization: `Bearer ${tok}` } });
    if (resp.status === 401) { Auth.login(); throw new Error("redirecting"); }
    if (!resp.ok) {
      let why = `HTTP ${resp.status}`;
      try { const j = JSON.parse(await resp.text()); if (j.error) why = j.error; } catch { /* keep the status */ }
      throw new Error(`the document could not be opened — ${why}`);
    }
    const cd = resp.headers.get("content-disposition") || "";
    return { blob: await resp.blob(), name: (cd.match(/filename="?([^"]+)"?/) || [])[1] || "" };
  }

  async function download(path, fallbackName) {
    const tok = await Auth.token();
    if (!tok) throw new Error("unauthenticated");
    const resp = await fetch(window.IDRE_CONFIG.apiBase + path, { headers: { Authorization: `Bearer ${tok}` } });
    if (resp.status === 401) { Auth.login(); throw new Error("redirecting"); }
    if (!resp.ok) {
      // Say why (the server explains: too many disputes, nothing to download...).
      let why = `HTTP ${resp.status}`;
      try { const j = JSON.parse(await resp.text()); if (j.error) why = j.error; } catch { /* keep the status */ }
      throw new Error(`download failed — ${why}`);
    }
    const cd = resp.headers.get("content-disposition") || "";
    const name = (cd.match(/filename="?([^"]+)"?/) || [])[1] || fallbackName || "download";
    const blobUrl = URL.createObjectURL(await resp.blob());
    const a = document.createElement("a");
    a.href = blobUrl; a.download = name;
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(blobUrl), 10000);
  }

  const t = () => `/v1/tenants/${tenant}`;

  // ---- Uploads: the rules first, and big files in chunks that survive a
  // pause, a dropped connection or a reload (portal_uploads.go). ----
  const Uploads = (() => {
    let policyP = null;
    const policy = () => (policyP ||= req("GET", `${t()}/upload-policy`).catch(() => { policyP = null; return null; }));
    const mb = (n) => (n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GB" : (n / (1 << 20)).toFixed(1) + " MB");
    const ext = (name) => "." + (String(name).split(".").pop() || "").toLowerCase();
    const fp = (f) => `idre.upload.${tenant}.${f.name}|${f.size}|${f.lastModified}`;
    async function check(file, p) {
      if (!p) return;
      if (file.size === 0) throw new Error(`${file.name} is empty`);
      if (ext(file.name) === ".zip") {
        if (file.size > p.zip_max_bytes) throw new Error(`${file.name} is ${mb(file.size)} — a ZIP may be at most ${mb(p.zip_max_bytes)}; split it into smaller ZIPs`);
        return;
      }
      const type = (p.types || []).find((x) => x.extensions.includes(ext(file.name)));
      // A type the rules do not list (a bank file for reconciliation, say) is the server's to judge.
      if (type && file.size > type.max_bytes) throw new Error(`${file.name} is ${mb(file.size)} — the limit for a ${type.label} file is ${mb(type.max_bytes)}`);
    }
    // The panel: progress, Pause/Resume and Cancel for the file going up.
    function panel(file) {
      const el = document.createElement("div");
      el.className = "upl-panel"; el.setAttribute("role", "status");
      el.innerHTML = `<div class="upl-name"></div><div class="upl-bar"><span></span></div>
        <div class="upl-row"><span class="upl-text muted">Starting…</span>
          <button type="button" class="mini upl-pause">Pause</button><button type="button" class="mini secondary upl-cancel">Cancel</button></div>`;
      el.querySelector(".upl-name").textContent = file.name;
      document.body.appendChild(el);
      const st = { paused: false, cancelled: false, wake: null };
      el.querySelector(".upl-pause").onclick = (e) => {
        st.paused = !st.paused; e.target.textContent = st.paused ? "Resume" : "Pause";
        el.querySelector(".upl-text").textContent = st.paused ? "Paused — resume when you are ready" : "Resuming…";
        if (!st.paused && st.wake) { st.wake(); st.wake = null; }
      };
      el.querySelector(".upl-cancel").onclick = () => { st.cancelled = true; if (st.wake) { st.wake(); st.wake = null; } };
      return {
        st,
        show(done, total, note) {
          el.querySelector(".upl-bar span").style.width = `${Math.floor((done / total) * 100)}%`;
          if (!st.paused) el.querySelector(".upl-text").textContent = note || `${mb(done)} of ${mb(total)}`;
        },
        close() { el.remove(); },
      };
    }
    async function chunk(id, offset, blob) {
      const tok = await Auth.token();
      const resp = await fetch(window.IDRE_CONFIG.apiBase + `${t()}/uploads/${id}`, {
        method: "PATCH", headers: { Authorization: `Bearer ${tok}`, "Upload-Offset": String(offset), "Content-Type": "application/offset+octet-stream" }, body: blob });
      let data = {}; try { data = JSON.parse(await resp.text()); } catch { /* empty */ }
      if (resp.status === 409 && typeof data.offset === "number") return { offset: data.offset }; // a retried chunk had landed
      if (!resp.ok) { const e = new Error(data.error || `HTTP ${resp.status}`); e.status = resp.status; throw e; }
      return data;
    }
    async function send(file, p) {
      const key = fp(file);
      let id = null, offset = 0;
      try { id = localStorage.getItem(key); } catch { /* private window */ }
      if (id) {
        const s = await req("GET", `${t()}/uploads/${id}`).catch(() => null);
        if (s && (s.status === "OPEN" || s.status === "COMPLETE")) offset = s.offset; else id = null;
      }
      if (!id) {
        id = (await req("POST", `${t()}/uploads`, { filename: file.name, size: file.size })).id;
        try { localStorage.setItem(key, id); } catch { /* private window */ }
      }
      const ui = panel(file), size = p.chunk_bytes || 8 << 20;
      try {
        let tries = 0;
        while (offset < file.size) {
          while (ui.st.paused && !ui.st.cancelled) await new Promise((r) => { ui.st.wake = r; });
          if (ui.st.cancelled) {
            await req("DELETE", `${t()}/uploads/${id}`).catch(() => {});
            try { localStorage.removeItem(key); } catch { /* */ }
            throw new Error("upload cancelled");
          }
          ui.show(offset, file.size);
          try {
            offset = (await chunk(id, offset, file.slice(offset, offset + size))).offset;
            tries = 0;
          } catch (e) {
            if (e.status && e.status < 500 && e.status !== 408 && e.status !== 429) throw e; // refused: virus, wrong type...
            if (++tries > 6) throw new Error(`the connection keeps dropping — choose the same file again later to resume from ${mb(offset)}`);
            ui.show(offset, file.size, `Connection lost — retrying (${tries})…`);
            await new Promise((r) => setTimeout(r, 1500 * tries));
          }
        }
        ui.show(file.size, file.size, "Uploaded — checking the file…");
        return { id, key, ui };
      } catch (e) { ui.close(); throw e; }
    }
    async function prepare(form) {
      const file = form.get("file");
      if (!(file instanceof File)) return { form, done() {} };
      const p = await policy();
      await check(file, p);
      if (!p || file.size <= (p.resumable_from_bytes || 8 << 20)) return { form, done() {} };
      const up = await send(file, p);
      const out = new FormData();
      for (const [k, v] of form.entries()) if (k !== "file") out.append(k, v);
      out.append("upload_id", up.id);
      setTimeout(() => up.ui.close(), 4000);
      return { form: out, done() { try { localStorage.removeItem(up.key); } catch { /* */ } } };
    }
    return { prepare, policy };
  })();
  // query-string builder: drops empty values, returns "" or "?a=1&b=2"
  const qs = (params) => {
    const s = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== "" && v != null)).toString();
    return s ? `?${s}` : "";
  };
  return {
    setTenant, getTenant, download, fetchBlob,
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
      mailSetup: () => req("GET", "/v1/admin/mail-setup"),
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
      // A staff member's full set of staff roles (PM / Tenant Admin, own tenant).
      setTenantStaffRoles: (tenant, username, roles) =>
        req("PUT", `/v1/admin/tenant-staff/${encodeURIComponent(username)}/roles`, { tenant, roles }),
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
      pipelineCounts: (by, opts = {}) => req("GET", `${t()}/cases/pipeline/counts?${new URLSearchParams({ by: by || "internal_status", ...opts })}`),
      list: (params = {}) => {
        const qs = new URLSearchParams(Object.entries(params).filter(([, v]) => v)).toString();
        return req("GET", `${t()}/cases${qs ? "?" + qs : ""}`)
          .then((r) => (Array.isArray(r) ? { cases: r, next_cursor: "", total: r.length } : r));
      },
      get: (id) => req("GET", `${t()}/cases/${id}`),
      summary: (id) => req("GET", `${t()}/cases/${id}/summary`),
      prefill: (docId) => req("GET", `${t()}/cases/prefill${docId ? `?doc_id=${encodeURIComponent(docId)}` : ""}`),
      nextAction: (id) => req("GET", `${t()}/cases/${id}/next-action`),
      attention: () => req("GET", `${t()}/cases/attention`),
      queuesSummary: () => req("GET", `${t()}/queues/summary`),
      telemetryMetrics: (days = 30) => req("GET", `${t()}/telemetry/metrics?days=${days}`),
      telemetryPaths: (days = 30, workflow = "") => req("GET", `${t()}/telemetry/paths?days=${days}${workflow ? `&workflow=${encodeURIComponent(workflow)}` : ""}`),
      telemetryActionStats: (days = 90) => req("GET", `${t()}/telemetry/action-stats?days=${days}`),
      telemetryPrefillStats: (days = 90) => req("GET", `${t()}/telemetry/prefill-stats?days=${days}`),
      analyticsTransitions: (axis = "lifecycle") => req("GET", `${t()}/analytics/transitions?axis=${axis}`),
      docReviewQueue: () => req("GET", `${t()}/documents/review-queue`),
      ackDoc: (caseId, docId) => req("POST", `${t()}/cases/${caseId}/documents/${docId}/acknowledge`, {}),
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
      // How a letter will read before it is sent (mail_preview.go); body as for a draft.
      mailTracker: (id) => req("GET", `${t()}/cases/${id}/mail-tracker`),
      // De-identification and clinical terms (deid.go): what each letter's scan found, the clinical terms of the
      // documents, and other disputes sharing them.
      deidReports: (id) => req("GET", `${t()}/cases/${id}/deid-reports`),
      clinical: (id) => req("GET", `${t()}/cases/${id}/clinical`),
      clinicalOverlaps: (id) => req("GET", `${t()}/cases/${id}/clinical/overlaps`),
      qaDraftQuality: (days = 90) => req("GET", `${t()}/qa/draft-quality?days=${days}`),
      corrPreview: (id, body) => req("POST", `${t()}/cases/${id}/correspondence/preview`, body),
      setDetails: (id, details) => req("PATCH", `${t()}/cases/${id}/details`, details),
      filingPacket: (id, packet) => req("PUT", `${t()}/cases/${id}/filing-packet`, packet),
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
    // The program's company mailbox (e.g. where replies to case letters arrive), connected by a tenant admin.
    companyOutlook: {
      status: () => req("GET", `${t()}/company-outlook`),
      connect: (mailbox) => req("GET", `${t()}/company-outlook/connect` + qs({ mailbox })),
      sync: () => req("POST", `${t()}/company-outlook/sync`),
      disconnect: () => req("DELETE", `${t()}/company-outlook`),
    },
    // The person's own email: their platform address and, when connected, their Outlook account.
    myMail: {
      mailbox: () => req("GET", `${t()}/me/mailbox`),
      list: (opts) => req("GET", `${t()}/me/mail` + qs(opts || {})),
      get: (id) => req("GET", `${t()}/me/mail/${id}`),
      attachment: (id, aid) => fetchBlob(`${t()}/me/mail/${id}/attachments/${aid}`),
      send: (body) => req("POST", `${t()}/me/mail/send`, body),
      outlookConnect: () => req("GET", `${t()}/me/outlook/connect`),
      outlookSync: () => req("POST", `${t()}/me/outlook/sync`),
      outlookDisconnect: () => req("DELETE", `${t()}/me/outlook`),
    },
    time: {
      mine: (from, to) => req("GET", `${t()}/time/mine${from||to ? `?from=${from||""}&to=${to||""}` : ""}`),
      summary: () => req("GET", `${t()}/time/summary`),
      prompts: () => req("GET", `${t()}/time/prompts`),
      update: (id, p) => req("PUT", `${t()}/time/${id}`, p),
      remove: (id) => req("DELETE", `${t()}/time/${id}`),
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
      // Entity 360: cross-rail organization aggregate (TPA / provider / health plan).
      entitySearch: (q) => req("GET", `${t()}/entities?q=${encodeURIComponent(q)}`),
      entityCreate: (body) => req("POST", `${t()}/entities`, body),
      entityLookup: (source_table, source_id) => req("GET", `${t()}/entity360/lookup?source_table=${encodeURIComponent(source_table)}&source_id=${encodeURIComponent(source_id)}`),
      entity360: (id) => req("GET", `${t()}/entity360/${id}`),
      entityLink: (id, body) => req("POST", `${t()}/entities/${id}/links`, body),
      entityUnlink: (id, linkId) => req("DELETE", `${t()}/entities/${id}/links/${linkId}`),
      entitySourceSearch: (q) => req("GET", `${t()}/entities/sources/search?q=${encodeURIComponent(q)}`),
      entityMerge: (id, intoId) => req("POST", `${t()}/entities/${id}/merge`, { into_id: intoId }),
      entityOutreach: (minCases, quietDays) => req("GET", `${t()}/entity360/outreach?min_cases=${minCases || 3}&quiet_days=${quietDays || 90}`),
      entityGraph: (id) => req("GET", `${t()}/entity360/${id}/graph`),
      createLead: (body) => req("POST", `${t()}/leads`, body),
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
      // expected: the assignee the page showed ("" = unassigned); if it changed meanwhile the server answers 409.
      assign: (caseId, role, assignee, reason = "", action = "", expected) =>
        req("POST", `${t()}/cases/${caseId}/assign`, { role, assignee, reason, action, ...(expected !== undefined ? { expected } : {}) }),
      // The next person in the role's rotation (Team page → Automatic assignment).
      assignNext: (caseId, role, reason = "", expected) =>
        req("POST", `${t()}/cases/${caseId}/assign`, { role, next: true, reason, ...(expected !== undefined ? { expected } : {}) }),
      claim: (caseId, reason = "") => req("POST", `${t()}/cases/${caseId}/assign`, { action: "claim", reason }),
      assignment: (caseId) => req("GET", `${t()}/cases/${caseId}/assignment`),
      assignees: (role) => req("GET", `${t()}/cases/assignees?role=${encodeURIComponent(role || "CASE_MANAGER")}`),
      // A role's rotation (Reviewer when none): { role, role_label, new_cases, enabled, people: [{id, username, open_cases, in_rotation}], next }.
      autoAssign: (role = "CASE_MANAGER") => req("GET", `${t()}/assignment/auto?role=${encodeURIComponent(role)}`),
      setAutoAssign: (body) => req("PUT", `${t()}/assignment/auto`, body),
      // The open cases nobody holds, to the Reviewers in turn (500 a call): { assigned, people: [{username, cases}], remaining }.
      assignOpen: () => req("POST", `${t()}/assignment/auto/assign-open`),
      escalate: (caseId, clock, detail) => req("POST", `${t()}/cases/${caseId}/escalate`, { clock, detail }),
      relate: (p) => req("POST", `${t()}/cases/relate`, p),
      relationships: (caseId) => req("GET", `${t()}/cases/${caseId}/relationships`),
      checklist: (caseId) => req("GET", `${t()}/cases/${caseId}/checklist`),
      checkItem: (itemId) => req("POST", `${t()}/checklists/${itemId}/check`),
      calendar: () => req("GET", `${t()}/calendar`),
      notifications: (opts) => req("GET", `${t()}/notifications` + qs(opts || {})),
      notifCount: () => req("GET", `${t()}/notifications/count`),
      readNotif: (id) => req("POST", `${t()}/notifications/${id}/read`),
      readAllNotifs: () => req("POST", `${t()}/notifications/read-all`),
      views: () => req("GET", `${t()}/views`),
      saveView: (p) => req("POST", `${t()}/views`, p),
      clocks: (id) => req("GET", `${t()}/cases/${id}/clocks`),
      extendClock: (id, clock, days, reason) => req("POST", `${t()}/cases/${id}/clocks/${encodeURIComponent(clock)}/extend`, { days, reason }),
      // Ask the TPA / health plan to agree to more time (body: {days, reason, side, due_days}).
      requestExtension: (id, clock, body) => req("POST", `${t()}/cases/${id}/clocks/${encodeURIComponent(clock)}/extension-request`, body),
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
      syncStatus: () => req("GET", `${t()}/graph/sync`),
      train: (epochs) => req("POST", `${t()}/graph/train`, epochs ? { epochs } : {}),
    },
    prefs: {
      all: () => req("GET", `${t()}/prefs`),
      put: (key, value) => req("PUT", `${t()}/prefs`, { key, value }),
    },
    program: {
      get: () => req("GET", `${t()}/program`),
      configuration: () => req("GET", `${t()}/program/configuration`),
      configurationSave: (section, body) => req("PUT", `${t()}/program/configuration/${section}`, body),
      setFeatures: (features) => req("PUT", `${t()}/features`, { features }), // a tenant admin's own tenant
      setLabel: (label) => req("PUT", `${t()}/program/label`, { label }),
      setBrand: (brand_name) => req("PUT", `${t()}/program/brand`, { brand_name }),
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
      eligibilityNote: (caseId, reviewId, body) => req("POST", `${t()}/cases/${caseId}/eligibility/${reviewId}/notes`, { body }),
      workflow: (caseId) => req("GET", `${t()}/cases/${caseId}/workflow`),
      eligibilityChecklist: (caseId) => download(`${t()}/cases/${caseId}/eligibility/checklist`, "Eligibility Checklist.html"),
      tpaBulkSample: (layout) => download(`${t()}/tpa/disputes/bulk/sample.zip?layout=${layout === "merged" ? "merged" : "separate"}`, `sample-disputes-${layout === "merged" ? "merged" : "separate"}.zip`),
      setReviewNote: (caseId, field, text) => req("PUT", `${t()}/cases/${caseId}/review-notes`, { field, text }),
      setDisputeSummary: (caseId, body) => req("PUT", `${t()}/cases/${caseId}/dispute-summary`, body),
      confirmAor: (caseId, valid, reason) => req("POST", `${t()}/cases/${caseId}/aor-confirmation`, { valid, reason }),
      optOut: (caseId, eligible, rationale) => req("POST", `${t()}/cases/${caseId}/opt-out`, { eligible, rationale }),
      send: (caseId, p) => req("POST", `${t()}/cases/${caseId}/correspondence`, p),
      correspondence: (caseId) => req("GET", `${t()}/cases/${caseId}/correspondence`),
      shareLink: (caseId, kind, days) => req("POST", `${t()}/cases/${caseId}/share-links`, { kind, days_ttl: days }),
      invoices: (caseId) => req("GET", `${t()}/cases/${caseId}/invoices`),
      issueInvoice: (caseId, p) => req("POST", `${t()}/cases/${caseId}/invoices`, p),
      settleInvoice: (invId, action, ref, refundMethod) => req("POST", `${t()}/invoices/${invId}/settle`, { action, remittance_ref: ref, refund_method: refundMethod || "" }),
      receivables: () => req("GET", `${t()}/reports/receivables`),
      financial: () => req("GET", `${t()}/reports/financial`),
      unapplied: () => req("GET", `${t()}/reports/unapplied`),
      resolveUnapplied: (id, body) => req("POST", `${t()}/finance/unapplied/${id}/resolve`, body),
      refundsDue: () => req("GET", `${t()}/reports/refunds-due`),
      timeReport: (opts) => req("GET", `${t()}/reports/time${qs(opts || {})}`),
      timeReportSend: (p) => req("POST", `${t()}/time/report/send`, p),
      timeRates: () => req("GET", `${t()}/reports/time/rates`),
      timeRateSet: (role, rate_cents_per_hour, effective_from) => req("PUT", `${t()}/reports/time/rates`, { role, rate_cents_per_hour, effective_from }),
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
      tpaTeam: () => req("GET", `${t()}/tpa/team`),
      tpaTeamAdd: (body) => req("POST", `${t()}/tpa/team`, body),
      tpaTeamRole: (sub, role) => req("PATCH", `${t()}/tpa/team/${encodeURIComponent(sub)}`, { team_role: role }),
      tpaTeamRemove: (sub) => req("DELETE", `${t()}/tpa/team/${encodeURIComponent(sub)}`),
      tpaGroups: () => req("GET", `${t()}/tpa/groups`),
      tpaGroupAdd: (body) => req("POST", `${t()}/tpa/groups`, body),
      tpaGroupUpdate: (id, body) => req("PATCH", `${t()}/tpa/groups/${id}`, body),
      tpaClientGroup: (clientId, groupId) => req("PUT", `${t()}/tpa/clients/${clientId}/group`, { group_id: groupId }),
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
      tpaRemoveDoc: (packageId, slot) => req("DELETE", `${t()}/tpa/packages/${packageId}/docs/${slot}`),
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
      tpaBulkCheckout: (id, body) => req("POST", `${t()}/tpa/disputes/bulk/${id}/checkout`, body || {}),
      tpaFees: () => req("GET", `${t()}/tpa/fees`),
      // Fee payments: the server works out what is owed; these only name the scope.
      payDispute: (kind, caseId, from, body) => req("POST", `${t()}/${kind === "hp" ? "hp" : "tpa"}/disputes/${caseId}/pay${from ? "?from=" + from : ""}`, body),
      disputeFee: (kind, caseId) => req("GET", `${t()}/${kind === "hp" ? "hp" : "tpa"}/disputes/${caseId}/fee`),
      payClient: (clientId, body) => req("POST", `${t()}/tpa/clients/${clientId}/fees/checkout`, body),
      // Saved cards and bank accounts (ACH): the TPA's own (no client_id) or a client's.
      tpaFeeOptions: (scope) => req("POST", `${t()}/tpa/fees/options`, scope || {}),
      tpaFeeRun: (runId) => req("GET", `${t()}/tpa/fees/runs/${runId}`),
      tpaTaskCreate: (body) => req("POST", `${t()}/tpa/tasks`, body),
      tpaTaskDone: (id) => req("POST", `${t()}/tpa/tasks/${id}/done`),
      tpaSavedMethods: (clientId) => req("GET", `${t()}/tpa/payment-methods` + (clientId ? `?client_id=${encodeURIComponent(clientId)}` : "")),
      tpaMethodSetup: (clientId, label) => req("POST", `${t()}/tpa/payment-methods/setup`, { client_id: clientId || "", label: label || "" }),
      tpaMethodSetupConfirm: (sessionId) => req("POST", `${t()}/tpa/payment-methods/setup/${encodeURIComponent(sessionId)}/confirm`),
      tpaMethodRemove: (id) => req("DELETE", `${t()}/tpa/payment-methods/${id}`),
      checkoutConfirm: (kind, g) => req("POST", `${t()}/${kind === "hp" ? "hp" : "tpa"}/fees/checkout/${g}/confirm`),
      checkoutCancel: (kind, g) => req("POST", `${t()}/${kind === "hp" ? "hp" : "tpa"}/fees/checkout/${g}/cancel`),
      hpFees: () => req("GET", `${t()}/hp/fees`),
      hpFeesCheckout: (body) => req("POST", `${t()}/hp/fees/checkout`, body || {}),
      // An organization's own disputes' documents in one ZIP: {case_id:[..]} | {client_id} | {batch_id} | {party_id} | {} (all).
      orgDocsZip: (kind, params) => {
        const q = new URLSearchParams();
        for (const [k, v] of Object.entries(params || {})) (Array.isArray(v) ? v : [v]).forEach((x) => x && q.append(k, x));
        return download(`${t()}/${kind === "hp" ? "hp" : "tpa"}/documents.zip${q.toString() ? "?" + q : ""}`, "documents.zip");
      },
      uploadPolicy: () => Uploads.policy(),
      orgAudit: (kind, params) => req("GET", `${t()}/${kind === "mine" ? "audit/mine" : kind === "hp" ? "hp/audit" : "tpa/audit"}?` + new URLSearchParams(params || {})),
      // The other side of a dispute answers it (respondents.go). k: hp | tpa | party.
      respondList: (k) => req("GET", `${t()}/${k}/responding`),
      // Document requests: asked on the case by the review team, answered in the portal.
      caseDocRequests: (caseId) => req("GET", `${t()}/cases/${caseId}/document-requests`),
      createDocRequest: (caseId, body) => req("POST", `${t()}/cases/${caseId}/document-requests`, body),
      closeDocRequest: (caseId, reqId, action) => req("POST", `${t()}/cases/${caseId}/document-requests/${reqId}/close`, { action }),
      // The letters sent to the organization and the documents they carried (party_documents.go).
      sentRecord: (k, caseId) => req("GET", `${t()}/${k === "hp" ? "hp" : "tpa"}/sent-documents${caseId ? `?case_id=${encodeURIComponent(caseId)}` : ""}`),
      // File a dispute filled in from its own document set (org_prefill.go). k: tpa | hp.
      orgPrefill: (k, packageId) => req("GET", `${t()}/${k === "hp" ? "hp" : "tpa"}/prefill?package_id=${encodeURIComponent(packageId)}`),
      sentDocPath: (k, docId) => `${t()}/${k === "hp" ? "hp" : "tpa"}/sent-documents/${docId}`,
      disputeSummary: (k, caseId) => req("GET", `${t()}/${k === "hp" ? "hp" : "tpa"}/disputes/${caseId}/summary`),
      sentDocGet: (k, docId, name) => download(`${t()}/${k === "hp" ? "hp" : "tpa"}/sent-documents/${docId}`, name || "document"),
      orgDocRequests: (k) => req("GET", `${t()}/${k === "hp" ? "hp" : "tpa"}/document-requests`),
      orgDocRequestUpload: (k, reqId, file) => {
        const fd = new FormData(); fd.append("file", file);
        return req("POST", `${t()}/${k === "hp" ? "hp" : "tpa"}/document-requests/${reqId}/files`, fd, true);
      },
      orgDocRequestSubmit: (k, reqId, note, decision) => req("POST", `${t()}/${k === "hp" ? "hp" : "tpa"}/document-requests/${reqId}/submit`, { note, decision }),
      respondGet: (k, id) => req("GET", k === "party" ? `${t()}/party/cases/${id}/respond` : `${t()}/${k}/responding/${id}`),
      respondFile: (k, id, doc) => blobURL((k === "party" ? `${t()}/party/cases/${id}/respond` : `${t()}/${k}/responding/${id}`) + `/documents/${doc}/file`),
      respondUpload: (k, id, file) => {
        const fd = new FormData(); fd.append("file", file);
        return req("POST", (k === "party" ? `${t()}/party/cases/${id}/respond` : `${t()}/${k}/responding/${id}`) + "/documents", fd, true);
      },
      respondSubmit: (k, id, note) => req("POST", (k === "party" ? `${t()}/party/cases/${id}/respond` : `${t()}/${k}/responding/${id}`) + "/submit", { note }),
      respondOptOut: (id, reason) => req("POST", `${t()}/hp/responding/${id}/opt-out`, { reason }),
      tpaFeesCheckout: (body) => req("POST", `${t()}/tpa/fees/checkout`, body || {}),
      healthPlanRegister: (body) => req("POST", "/api/public/healthplan/register", body),
      hpClaim: (code) => req("POST", `${t()}/hp/claim`, { code }),
      hpUnlink: () => req("POST", `${t()}/hp/unlink`, {}),
      onboardingReviews: () => req("GET", `${t()}/onboarding/reviews`),
      onboardingReviewFile: (kind, docId) => blobURL(`${t()}/onboarding/reviews/${kind}/${docId}/file`),
      onboardingReviewDecide: (kind, docId, decision, note) => req("POST", `${t()}/onboarding/reviews/${kind}/${docId}`, { decision, note }),
      npiReviews: () => req("GET", `${t()}/onboarding/npi-reviews`),
      verificationReviews: () => req("GET", `${t()}/onboarding/verification-reviews`),
      verificationReviewDecide: (id, decision, note) => req("POST", `${t()}/onboarding/verification-reviews/${id}`, { decision, note }),
      npiReviewDecide: (kind, id, decision, note) => req("POST", `${t()}/onboarding/npi-reviews/${kind}/${id}`, { decision, note }),
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
      claimEligibility: (caseId, body) => req("PUT", `${t()}/cases/${caseId}/claims/eligibility`, body),
      importClaims: (caseId, claims) => req("POST", `${t()}/cases/${caseId}/claims`, { claims }),
      qaQueue: (caseId) => req("GET", `${t()}/qa${caseId ? `?case_id=${encodeURIComponent(caseId)}` : ""}`),
      qaGet: (id) => req("GET", `${t()}/qa/${id}`),
      qaDraftQuality: (days = 90) => req("GET", `${t()}/qa/draft-quality?days=${days}`),
      qaTemplateHealth: (days = 90) => req("GET", `${t()}/qa/template-health?days=${days}`),
      qaDecision: (id, decision, note, editedBody, to, cc) => req("POST", `${t()}/qa/${id}/decision`, { decision, note, edited_body: editedBody || undefined, to, cc }),
      mailJournal: (opts) => req("GET", `${t()}/correspondence${qs(opts || {})}`),
      fileMail: (id, caseNumber) => req("POST", `${t()}/correspondence/${id}/file`, { case_number: caseNumber }),
      mailAttachments: (id) => req("GET", `${t()}/correspondence/${id}/attachments`),
      mailAttachment: (id, aid) => fetchBlob(`${t()}/correspondence/${id}/attachments/${aid}`),
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
    letters: {
      report: (caseId) => req("GET", `${t()}/cases/${caseId}/decision-report`),
      saveReport: (caseId, body) => req("PUT", `${t()}/cases/${caseId}/decision-report`, body),
      attachReport: (caseId, docId) => req("POST", `${t()}/cases/${caseId}/decision-report/file`, { doc_id: docId }),
      settings: () => req("GET", `${t()}/letter-settings`),
      saveSettings: (body) => req("PUT", `${t()}/letter-settings`, body),
      variables: () => req("GET", `${t()}/template-variables`),
      saveVariables: (body) => req("PUT", `${t()}/template-variables`, body),
    },
    geo: {
      mapUrl: () => `${config.caseApiBase}/geo/map`,
    },
  };
})();
