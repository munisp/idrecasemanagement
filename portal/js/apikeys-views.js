"use strict";
// apikeys-views.js — API keys for TPAs and health plans (api_keys.go), the
// tenant admins' list of every partner key, and the staff API reference.
const ApiKeysViews = (() => {
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const can = (...rs) => Auth.can(...rs);
  const when = (d) => (d ? new Date(d).toLocaleString("en-US", { dateStyle: "medium", timeStyle: "short" }) : "—");
  const outside = () => can("TPA", "HEALTH_PLAN") && !can("CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "FINANCE");
  const orgKind = () => (can("TPA") ? "tpa" : "hp");
  const later = (id, fn) => (async () => {
    for (let i = 0; i < 50 && !document.getElementById(id); i++) await new Promise((r) => setTimeout(r, 100));
    fn();
  })();

  // #/api-keys — an organization's own keys, or (tenant admins) every partner key.
  function page() {
    const staff = !outside();
    const kind = staff ? "staff" : orgKind();
    later("api-keys-box", () => listBox(kind));
    const docs = kind === "hp" ? "/developers.html#health-plan" : "/developers.html#tpa";
    return `<div class="view-head"><h1>${staff ? "Partner API keys" : "API keys"}</h1>
        ${staff ? "" : `<button onclick="ApiKeysViews.create('${kind}')">New key</button>`}</div>
      <div class="card"><p class="muted">${staff
        ? "Keys that TPAs and health plans use to connect their own systems. Each key acts as a member of its organization, with the same limits as its people. Revoking a key stops it at once."
        : `Connect your ${kind === "tpa" ? "TPA" : "plan"}'s own software to this portal. A key can do what your ${kind === "tpa" ? "staff" : "logins"} can do in the portal, and everything it does shows in your audit log under its name.`}
        <a href="${docs}" target="_blank" rel="noopener">Read the API guide</a>.</p>
        <div id="api-keys-box"><p class="muted">Loading…</p></div></div>`;
  }

  async function listBox(kind) {
    const box = document.getElementById("api-keys-box");
    if (!box) return;
    try {
      const res = await Api.apiKeys.list(kind);
      const keys = res.api_keys || [];
      if (!keys.length) {
        box.innerHTML = `<p class="muted">No keys yet.${kind === "staff" ? "" : " Make one with <strong>New key</strong>."}</p>`;
        return;
      }
      box.innerHTML = `<div class="tbl-wrap"><table class="tbl"><thead><tr>
          <th>Name</th>${kind === "staff" ? "<th>Organization</th>" : ""}<th>Client id</th><th>Access</th><th>Last used</th><th>Made by</th><th>Status</th><th></th></tr></thead><tbody>
        ${keys.map((k) => `<tr>
          <td>${esc(k.label)}</td>
          ${kind === "staff" ? `<td>${esc(k.org_name)} <span class="badge">${k.org_kind === "tpa" ? "TPA" : "Health plan"}</span></td>` : ""}
          <td class="mono">${esc(k.client_id)}</td>
          <td>${esc(k.team_role === "FINANCE" ? "Payments only" : k.org_kind === "tpa" ? "Staff" : "Plan")}${k.limited_to_client_name ? ` · ${esc(k.limited_to_client_name)} only` : ""}</td>
          <td>${when(k.last_used_at)}</td>
          <td>${esc(k.created_by)}<br><span class="muted">${when(k.created_at)}</span></td>
          <td>${k.revoked_at ? `<span class="badge s-rejected">Revoked</span>` : `<span class="badge s-filed">Active</span>`}</td>
          <td>${k.revoked_at ? "" : `<button class="mini danger" onclick="ApiKeysViews.revoke('${kind}', '${esc(k.id)}', ${esc(JSON.stringify(k.label))})">Revoke</button>`}</td>
        </tr>`).join("")}</tbody></table></div>`;
    } catch (e) {
      box.innerHTML = `<p class="muted">${esc(e.status === 403 ? (kind === "tpa" ? "Only your TPA's admins can see and manage API keys." : e.message) : e.message)}</p>`;
    }
  }

  async function create(kind) {
    const fields = [{ name: "label", label: "Name", required: true, placeholder: "e.g. Claims system", hint: "So you can tell your keys apart." }];
    if (kind === "tpa") {
      fields.push({ name: "team_role", label: "What it may do", value: "STAFF", options: [["STAFF", "File and follow disputes (staff)"], ["FINANCE", "Payments only (finance)"]] });
      let clients = [];
      try { clients = (await Api.program.tpaClientsAll()).clients || []; } catch { /* keep the whole-TPA option */ }
      fields.push({ name: "client_id", label: "Clients", value: "", options: [["", "All your clients"], ...clients.map((c) => [c.id, `Only ${c.party_name}`])] });
    }
    const v = await UI.modal({ title: "New API key", fields, submitLabel: "Make key" });
    if (!v) return;
    let key;
    try {
      key = await Api.apiKeys.create(kind, { label: v.label.trim(), team_role: v.team_role, client_id: v.client_id || "" });
    } catch (e) {
      UI.toast(e.message, { kind: "error" });
      return;
    }
    const copy = (val, what) => `<button class="mini ghost" type="button" onclick="Views.copyText(${esc(JSON.stringify(val))}, '${what} copied')">Copy</button>`;
    const curl = `curl -s ${key.token_url} \\\n  -d grant_type=client_credentials \\\n  -d client_id=${key.client_id} \\\n  -d client_secret=<secret>`;
    await UI.modal({
      title: "Your new API key", submitLabel: "I've stored the secret", wide: true,
      html: `<p><strong>Copy the secret now.</strong> It is shown only this once; if you lose it, revoke this key and make another.</p>
        <p>Client id<br><code class="mono">${esc(key.client_id)}</code> ${copy(key.client_id, "Client id")}</p>
        <p>Client secret<br><code class="mono">${esc(key.client_secret)}</code> ${copy(key.client_secret, "Secret")}</p>
        <p class="muted">Exchange them for a token (valid 5 minutes), then call <code>${esc(key.api_base)}/…</code> with <code>Authorization: Bearer &lt;token&gt;</code>:</p>
        <pre class="mono" style="white-space:pre-wrap">${esc(curl)}</pre>`,
    });
    listBox(kind);
  }

  async function revoke(kind, id, label) {
    if (!(await UI.confirm("Revoke this key?", `<strong>${esc(label)}</strong> stops working at once. Software using it will be refused.`, "Revoke key", true))) return;
    try {
      await Api.apiKeys.revoke(kind, id);
      UI.toast("Key revoked");
    } catch (e) {
      UI.toast(e.message, { kind: "error" });
    }
    listBox(kind);
  }

  // #/api-reference — every endpoint, for office staff (the internal OpenAPI document).
  function reference() {
    later("api-ref", async () => {
      const box = document.getElementById("api-ref");
      try {
        const [spec] = await Promise.all([Api.apiKeys.internalSpec(), loadRedoc()]);
        box.innerHTML = "";
        window.Redoc.init(spec, {
          expandResponses: "200,201", jsonSampleExpandLevel: 2, hideDownloadButton: false, scrollYOffset: 0,
          theme: { typography: { fontFamily: "inherit", headings: { fontFamily: "inherit" } } },
        }, box);
      } catch (e) {
        box.innerHTML = `<p class="muted">${esc(e.message)}</p>`;
      }
    });
    return `<div class="view-head"><h1>API reference</h1></div>
      <div class="card"><p class="muted">Every endpoint of this portal's API, read from the running service, with each one's implementation notes.
        Partners have their own guides at <a href="/developers.html" target="_blank" rel="noopener">/developers.html</a>${can("TENANT_ADMIN", "PLATFORM_ADMIN", "FEDERAL_ADMIN") ? `, and their keys are under <a href="#/api-keys">Partner API keys</a>` : ""}.</p></div>
      <div id="api-ref" class="api-ref"><p class="muted">Loading…</p></div>`;
  }

  let redocLoading = null;
  function loadRedoc() {
    if (window.Redoc) return Promise.resolve();
    redocLoading = redocLoading || new Promise((resolve, reject) => {
      const s = document.createElement("script");
      s.src = "/js/vendor/redoc-2.1.5.standalone.js";
      s.onload = () => resolve();
      s.onerror = () => reject(new Error("The reference viewer could not be loaded."));
      document.head.appendChild(s);
    });
    return redocLoading;
  }

  return { page, create, revoke, reference };
})();
