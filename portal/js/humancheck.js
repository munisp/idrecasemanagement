// humancheck.js — Cloudflare Turnstile ("Verify you are human") on the portal's TPA and health plan filing forms
// (2026-11-26). The server asks for it only when the program has keys (GET /api/public/turnstile gives a site key);
// with none, these helpers do nothing. A form holds <div class="human-check" data-for="ID"></div>; just before the
// request, HumanCheck.use(ID) hands its token to the next API call (X-Turnstile-Token), and the widget is reset after.
window.HumanCheck = (() => {
  let key = null, loading = null;
  const widgets = {};
  function load() {
    if (loading) return loading;
    loading = fetch("/api/public/turnstile").then((r) => r.json()).then((d) => {
      key = d.site_key || "";
      if (!key) return;
      return new Promise((ok) => {
        const s = document.createElement("script");
        s.src = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";
        s.onload = ok; s.onerror = ok;
        document.head.appendChild(s);
      });
    }).catch(() => { key = ""; });
    return loading;
  }
  // Draw the widgets now on the page that are not drawn yet.
  async function render() {
    await load();
    if (!key || !window.turnstile) return;
    document.querySelectorAll(".human-check").forEach((el) => {
      if (el.dataset.drawn || !el.isConnected) return;
      el.dataset.drawn = "1";
      widgets[el.dataset.for] = window.turnstile.render(el, { sitekey: key, theme: "auto" });
    });
  }
  const box = (id) => { setTimeout(render, 0); return `<div class="human-check" data-for="${id}"></div>`; };
  function use(id) {
    if (key && window.turnstile && widgets[id] !== undefined) window.__humanToken = window.turnstile.getResponse(widgets[id]) || "";
  }
  function reset(id) {
    window.__humanToken = "";
    if (key && window.turnstile && widgets[id] !== undefined) { try { window.turnstile.reset(widgets[id]); } catch { /* gone */ } }
  }
  return { box, render, use, reset };
})();
