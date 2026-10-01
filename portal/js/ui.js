// ui.js — Meridian interaction layer: modal dialogs, toasts with undo, confirm.
// Replaces every window.prompt / alert / confirm (anti-pattern #13) with branded,
// keyboard-complete, screen-reader-announced surfaces.
const UI = (() => {
  // ---- Toasts (aria-live polite; undo where offered) ----
  let region;
  function toast(message, opts = {}) {
    if (!region) {
      region = document.createElement("div");
      region.className = "toast-region";
      region.setAttribute("aria-live", "polite");
      document.body.appendChild(region);
    }
    const t = document.createElement("div");
    t.className = "toast" + (opts.kind ? " t-" + opts.kind : "");
    t.innerHTML = `<span>${message}</span>`;
    if (opts.undo) {
      const b = document.createElement("button");
      b.textContent = "Undo";
      b.onclick = () => { opts.undo(); t.remove(); };
      t.appendChild(b);
    }
    region.appendChild(t);
    setTimeout(() => t.remove(), opts.duration || 6000);
  }

  // ---- Modal (focus-trapped, Esc closes, returns a promise) ----
  // fields: [{name, label, type, value, placeholder, required, options, hint}]
  function modal({ title, body = "", fields = [], submitLabel = "Save", danger = false, wide = false }) {
    return new Promise((resolve) => {
      const prev = document.activeElement;
      const scrim = document.createElement("div");
      scrim.className = "modal-scrim";
      scrim.innerHTML = `
        <div class="modal ${wide ? "modal-wide" : ""}" role="dialog" aria-modal="true" aria-label="${title}">
          <div class="modal-h"><h2>${title}</h2>
            <button class="icon-btn modal-x" aria-label="Close dialog">✕</button></div>
          <form class="modal-b" novalidate>
            ${body ? `<p class="modal-body">${body}</p>` : ""}
            ${fields.map((f) => `
              <label class="mfield">${f.label}${f.required ? ' <span class="req">*</span>' : ""}
                ${f.options
                  ? `<select name="${f.name}" ${f.required ? "required" : ""}>${f.options.map((o) =>
                      `<option value="${o[0]}" ${o[0] === f.value ? "selected" : ""}>${o[1]}</option>`).join("")}</select>`
                  : f.type === "textarea"
                    ? `<textarea name="${f.name}" rows="3" placeholder="${f.placeholder || ""}" ${f.required ? "required" : ""}>${f.value || ""}</textarea>`
                    : `<input name="${f.name}" type="${f.type || "text"}" value="${f.value || ""}"
                        placeholder="${f.placeholder || ""}" ${f.required ? "required" : ""} ${f.step ? `step="${f.step}"` : ""} />`}
                ${f.hint ? `<span class="mhint">${f.hint}</span>` : ""}
              </label>`).join("")}
            <div class="modal-f">
              <button type="button" class="btn-secondary m-cancel">Cancel</button>
              <button type="submit" class="${danger ? "btn-danger" : "btn-primary"}">${submitLabel}</button>
            </div>
          </form>
        </div>`;
      document.body.appendChild(scrim);
      const close = (val) => { scrim.remove(); prev?.focus?.(); resolve(val); };
      scrim.querySelector(".modal-x").onclick = () => close(null);
      scrim.querySelector(".m-cancel").onclick = () => close(null);
      scrim.addEventListener("mousedown", (e) => { if (e.target === scrim) close(null); });
      scrim.addEventListener("keydown", (e) => {
        if (e.key === "Escape") close(null);
        if (e.key === "Tab") { // focus trap
          const els = [...scrim.querySelectorAll("button,input,select,textarea")].filter((x) => !x.disabled);
          const first = els[0], last = els[els.length - 1];
          if (e.shiftKey && document.activeElement === first) { last.focus(); e.preventDefault(); }
          else if (!e.shiftKey && document.activeElement === last) { first.focus(); e.preventDefault(); }
        }
      });
      scrim.querySelector("form").onsubmit = (e) => {
        e.preventDefault();
        const data = Object.fromEntries(new FormData(e.target));
        for (const f of fields) {
          if (f.required && !String(data[f.name] || "").trim()) {
            e.target.querySelector(`[name="${f.name}"]`).focus();
            toast(`Enter ${f.label.toLowerCase()} to continue.`, { kind: "warn" });
            return;
          }
        }
        close(data);
      };
      scrim.querySelector("input,select,textarea,button")?.focus();
    });
  }

  const confirm = (title, body, submitLabel = "Confirm", danger = false) =>
    modal({ title, body, submitLabel, danger }).then((v) => v !== null);

  return { toast, modal, confirm };
})();
