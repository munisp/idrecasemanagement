// emailcheck.js — checks an email field when the person leaves it: is the
// address well formed, does its domain take mail, is the domain a typo of a
// big provider (with a one-click fix). Any <input type="email" data-email-check>
// on the page gets it. The server checks again on submit; this is the early,
// friendly half. A failed lookup says nothing rather than block anyone.
(() => {
  const cache = new Map();
  const hintFor = (input) => {
    let el = input.parentElement.querySelector(":scope > .email-hint");
    if (!el) {
      el = document.createElement("span");
      el.className = "email-hint"; el.setAttribute("role", "status");
      input.insertAdjacentElement("afterend", el);
    }
    return el;
  };
  async function check(input) {
    const v = input.value.trim(), el = hintFor(input);
    el.className = "email-hint"; el.textContent = ""; input.setCustomValidity("");
    if (!v) return;
    let r = cache.get(v);
    if (!r) {
      try {
        const resp = await fetch("/api/public/email-check?email=" + encodeURIComponent(v));
        if (!resp.ok) return;
        r = await resp.json(); cache.set(v, r);
      } catch { return; }
    }
    if (input.value.trim() !== v) return; // changed while we asked
    if (r.valid) { el.classList.add("ok"); el.textContent = "✓ This address can receive email"; return; }
    el.classList.add("bad");
    el.textContent = "This address will not receive email: " + r.problem + ". ";
    input.setCustomValidity(r.problem);
    if (r.suggestion) {
      const b = document.createElement("button");
      b.type = "button"; b.className = "linkish"; b.textContent = "Use " + r.suggestion;
      b.onclick = () => { input.value = r.suggestion; check(input); };
      el.appendChild(b);
    }
  }
  document.addEventListener("focusout", (e) => {
    const t = e.target;
    if (t && t.matches && t.matches("input[type=email][data-email-check]")) check(t);
  });
  document.addEventListener("input", (e) => {
    const t = e.target;
    if (t && t.matches && t.matches("input[type=email][data-email-check]")) {
      t.setCustomValidity("");
      const el = t.parentElement.querySelector(":scope > .email-hint");
      if (el) { el.className = "email-hint"; el.textContent = ""; }
    }
  });
})();
