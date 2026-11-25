// rebuttalform.js — the FL Plan Packet's Responding Party Claim Rebuttal Form (6.18.24) as an online form, filled in
// by the responding party on its "Disputes against us" screen and submitted with its response (respondents.go,
// rebuttal.go). Its Claim Information Worksheet takes rows by hand or from an Excel workbook (PacketForm's reader).
//
//   RebuttalForm.html(side, values)  -> the form ("HEALTH_PLAN" | "PROVIDER": which position page it shows)
//   RebuttalForm.read(root)          -> {rebuttal, problems}
window.RebuttalForm = (() => {
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const EXCLUSIONS = [
    ["interest_payment", "This claim is related to an interest payment."],
    ["prompt_payment", "This claim is related solely to an issue of prompt payment."],
    ["medicare_grievance", "This claim is part of an internal grievance in a Medicare Managed Care Organization or a reconsideration appeal through the Medicare reconsideration process."],
    ["medicaid_fair_hearing", "This claim is a part of a Medicaid fair hearing pursued under 42 CFR §431.220 et seq."],
    ["pending_court_action", "This claim is the basis for an action pending in state or federal court."],
    ["pre_2000_binding", "This claim is subject to a binding claim dispute resolution process provided by contract entered into prior to October 1, 2000."],
    ["internal_not_exhausted", "This claim is subject to an internal dispute resolution process provided by contract entered into or renewed on or after October 1, 2000, which process has not been exhausted."],
    ["over_12_months", "More than 12 months have elapsed since the final determination was made on this claim by the Managed Care Organization."],
    ["plan_not_fl_regulated", "This Health Plan is not regulated by the State of Florida."],
  ];
  const DOCS = [["claim_forms", "Claims form(s) submitted to Managed Care Organization"], ["denials_eobs", "Denial(s)/Explanation of Benefits received from Managed Care Organization"],
    ["final_determination", "Final Determination Letter/Decision with documentation of the appeal process and related information"],
    ["contract", "Contract between Provider and Managed Care Organization"], ["medical_records", "Medical Records"],
    ["coding_rules", "Coding Rules/Policies", true], ["other", "Other", true]];
  const WS_COLS = [["claim_number", "Claim #", "text"], ["patient_name", "Patient name", "text"], ["type_of_service", "Type of service", "text"],
    ["date_of_service", "Date(s) of service", "date"], ["billed", "Amount billed", "money"], ["paid", "Amount paid", "money"],
    ["disputed", "Amount in dispute", "money"], ["date_claim_submitted", "Date claim submitted", "date"], ["date_of_denial", "Date of denial", "date"],
    ["date_grievance_completed", "Date internal grievance process completed", "date"]];
  const POSITION = {
    HEALTH_PLAN: ["Describe with all appropriate technical detail the basis for the denial and amount of payment, if any, made for these services.",
      "Provide specific references to the contract, provider manual and other supporting documentation to support your position. Provide copies of all referenced materials.",
      "State your understanding of the basis for the claim calculation made by the Provider. Provide your rebuttal argument against the Provider's calculation if not stated in question 1."],
    PROVIDER: ["Describe with all appropriate technical detail the basis for the determination that you did not receive an overpayment for these services.",
      "Provide specific references to the contract, provider manual and other supporting documentation to support your position. Provide copies of all referenced materials.",
      "State your understanding of the basis for the Health Plan's determination that an overpayment was made. Provide your rebuttal argument against the Health Plan's calculation if not stated in question 1."],
  };
  const R = '<span class="pf-req" title="required">*</span>';
  const money = (c) => (c == null || c === "" ? "" : (Number(c) / 100).toFixed(2));
  const cents = (v) => { const t = String(v ?? "").replace(/[$,\s]/g, ""); if (t === "") return null; const n = Number(t); return Number.isFinite(n) ? Math.round(n * 100) : NaN; };
  const inp = (name, label, v = "", attrs = "", req = false) => `<label>${esc(label)}${req ? R : ""} <input name="${name}" value="${esc(v)}" ${attrs} /></label>`;
  // The typed name and title, and a place to sign (change log #7): the drawn signature is kept as a PNG in <p>.drawn.
  const sigBlock = (p, v = {}) => `<div class="pf-grid">${inp(`${p}.printed_name`, "Printed name", v.printed_name, "", true)}${inp(`${p}.title`, "Title", v.title, "", true)}
      ${inp(`${p}.signature`, "Electronic signature (type your full name)", v.signature, "", true)}${inp(`${p}.date`, "Date", v.date, 'type="date"', true)}</div>
      ${PacketForm.sigPad({ [`${p}.drawn`]: v.drawn || "" }, `${p}.drawn`, true).replace(`name="d.${p}.drawn"`, `name="${p}.drawn"`)}`;
  const wsRow = (w = {}) => `<tr class="pf-claim">${WS_COLS.map(([k, label, type]) => {
    const v = type === "money" ? money(w[k + "_cents"]) : (w[k] ?? "");
    return `<td><input data-col="${k}" aria-label="${esc(label)}" ${type === "date" ? 'type="date"' : type === "money" ? 'inputmode="decimal" placeholder="0.00"' : ""} value="${esc(v)}" /></td>`;
  }).join("")}<td><button type="button" class="pf-del" title="Remove this claim" onclick="this.closest('tr').remove()">×</button></td></tr>`;

  function html(side, v = {}) {
    side = side === "PROVIDER" ? "PROVIDER" : "HEALTH_PLAN";
    const p = v.party || {}, plan = side === "HEALTH_PLAN", ex = v.exclusions || [], docs = v.documents || [], info = v.claim_info || {}, pos = v.position || {};
    const ws = v.worksheet || [];
    return `<div class="pf rb" data-side="${side}">
      <p class="pf-hint pf-req-hint">The Responding Party Claim Rebuttal Form, online. Fields marked ${R} are required. Respond within 15 days of receiving the request.</p>
      <fieldset class="pf-sec"><legend>Responding party</legend><div class="pf-grid">
        ${inp("party.legal_name", "Legal name", p.legal_name, "", true)}${inp("party.trade_name", "Trade name (d/b/a)", p.trade_name)}
        ${inp("party.address", "Address", p.address)}${inp("party.city", "City", p.city)}${inp("party.state", "State", p.state, 'maxlength="2" placeholder="FL"')}${inp("party.zip", "ZIP code", p.zip)}
        ${plan ? inp("party.product_name", "Product name", p.product_name) : ""}${inp("party.contact_person", "Contact person", p.contact_person, "", true)}
        ${inp("party.physical_address", "Physical address (if different)", p.physical_address)}${inp("party.physical_city", "City", p.physical_city)}
        ${inp("party.physical_state", "State", p.physical_state, 'maxlength="2"')}${inp("party.physical_zip", "ZIP code", p.physical_zip)}
        ${inp("party.phone", "Telephone number", p.phone, 'type="tel"')}${inp("party.fax", "Fax number", p.fax, 'type="tel"')}${inp("party.email", "E-mail", p.email, 'type="email"', true)}</div></fieldset>
      ${plan ? `<fieldset class="pf-sec"><legend>Participation</legend>
        <label class="pf-choice"><input type="checkbox" name="smmc" ${v.smmc ? "checked" : ""} /> We are a Statewide Medicaid Managed Care (SMMC) contracted plan — SMMC plans must participate in the arbitration process.</label>
        <div class="rb-nonsmmc" ${v.smmc ? "hidden" : ""}>
          <p class="pf-q">For non-SMMC contract plans only: do you wish to participate in the dispute process?${R}</p>
          <div class="pf-choices pf-inline">${["yes", "no"].map((x) => `<label class="pf-choice"><input type="radio" name="participate" value="${x}" ${v.participate === x ? "checked" : ""} /> ${x === "yes" ? "Yes" : "No"}</label>`).join("")}</div>
          <p class="pf-q">Do you wish to challenge the eligibility of this claim for review?${R}</p>
          <div class="pf-choices pf-inline">${["yes", "no"].map((x) => `<label class="pf-choice"><input type="radio" name="challenge" value="${x}" ${v.challenge_eligibility === x ? "checked" : ""} /> ${x === "yes" ? "Yes" : "No"}</label>`).join("")}</div>
        </div></fieldset>` : ""}
      <fieldset class="pf-sec rb-challenge" ${v.challenge_eligibility === "yes" ? "" : "hidden"}><legend>Eligibility attestation</legend>
        <p class="pf-hint">If you are challenging the eligibility of this claim: tick every exclusion it meets and attach the supporting documentation.</p>
        <div class="pf-checks">${EXCLUSIONS.filter(([k]) => plan || k !== "plan_not_fl_regulated").map(([k, label]) => `<label class="pf-choice"><input type="checkbox" name="ex" value="${k}" ${ex.includes(k) ? "checked" : ""} /> ${esc(label)}</label>`).join("")}</div>
        <label class="pf-choice pf-certify"><input type="checkbox" name="ex_certified" ${v.exclusions_certified ? "checked" : ""} /> I certify that I am legally authorized to complete this attestation on behalf of the Responding Party.</label></fieldset>
      <fieldset class="pf-sec"><legend>Signature${R}</legend>
        <p class="pf-hint">Required, even if not challenging eligibility or participating.</p>${sigBlock("att", v.attest_signature)}</fieldset>
      <fieldset class="pf-sec"><legend>Claim information</legend><div class="pf-grid">
        ${inp("info.claims_count", "Number of claims included", info.claims_count, 'type="number" min="0" step="1"')}${inp("info.dates_of_service", "Date(s) of service", info.dates_of_service)}
        ${inp("info.type_of_service", "Type of service", info.type_of_service)}${inp("info.patient_names", "Patient name(s)", info.patient_names)}</div></fieldset>
      <fieldset class="pf-sec rb-pos"><legend>Your position regarding this claim</legend>
        ${POSITION[side].map((q, i) => `<label class="pf-area">${i + 1}. ${esc(q)}${i === 0 ? R : ""}<textarea name="pos.${["basis", "references", "rebuttal"][i]}" rows="4">${esc(pos[["basis", "references", "rebuttal"][i]] || "")}</textarea></label>`).join("")}</fieldset>
      <fieldset class="pf-sec"><legend>Supporting documentation being submitted</legend>
        <p class="pf-hint">If any of the required documentation cannot be filed, submit an explanation in its place. Add the documents themselves above.</p>
        <div class="pf-checks">${DOCS.map(([k, label, spec]) => `<label class="pf-choice"><input type="checkbox" name="doc" value="${k}" ${docs.includes(k) ? "checked" : ""} /> ${esc(label)}${spec ? ` — specify <input name="spec.${k}" value="${esc(v[k + "_specify"] || "")}" class="rb-spec" />` : ""}</label>`).join("")}</div></fieldset>
      <fieldset class="pf-sec rb-cert"><legend>Certification${R}</legend>
        <label class="pf-choice pf-certify"><input type="checkbox" name="cert.agreed" ${(v.certification || {}).agreed ? "checked" : ""} /> I certify that I am legally authorized to complete this certification on behalf of the Responding Party. I understand and agree that if the Responding Party does not prevail it will be responsible for the full or apportioned review costs of this case; that if those costs are not paid within 35 days after the Agency's final order is rendered, the Responding Party will be subject to a fine; and that if this case is settled prior to the time a decision is rendered, the parties will be responsible for the review costs for this case incurred prior to the settlement.</label>
        ${sigBlock("cert", v.certification)}</fieldset>
      <fieldset class="pf-sec rb-ws"><legend>Claim information worksheet</legend>
        <p class="pf-hint">One line for each disputed claim — or upload them from an Excel workbook.</p>
        <div class="pf-csv"><label>Upload claims from an Excel file (.xlsx) <input type="file" accept=".xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" onchange="RebuttalForm.fromXlsx(this)" /></label>
          <a href="#" onclick="RebuttalForm.template(event)">Download the Excel template</a><span class="pf-csv-msg" aria-live="polite"></span></div>
        <div class="pf-scroll"><table class="pf-claims"><thead><tr>${WS_COLS.map(([, l]) => `<th>${esc(l)}</th>`).join("")}<th></th></tr></thead>
          <tbody>${ws.map(wsRow).join("") || wsRow() + wsRow() + wsRow()}</tbody></table></div>
        <button type="button" class="pf-add" onclick="this.closest('.pf-sec').querySelector('tbody').insertAdjacentHTML('beforeend', RebuttalForm.row())">Add a claim</button></fieldset>
    </div>`;
  }

  function bind(root) {
    const pf = root.querySelector(".rb");
    if (!pf) return;
    PacketForm.bindSigPads(pf);
    pf.addEventListener("change", (ev) => {
      const t = ev.target;
      if (t.name === "smmc") pf.querySelector(".rb-nonsmmc").hidden = t.checked;
      if (t.name === "challenge") pf.querySelector(".rb-challenge").hidden = t.value !== "yes";
      if (t.name === "participate") { // not taking part: the position and the certification are not asked
        const no = t.value === "no";
        pf.querySelectorAll(".rb-pos, .rb-cert").forEach((x) => { x.classList.toggle("rb-optional", no); });
      }
    });
  }

  function read(root) {
    const pf = root.querySelector(".rb") || root, problems = [];
    const val = (n) => (pf.querySelector(`[name="${n}"]`)?.value || "").trim();
    const group = (prefix, keys) => Object.fromEntries(keys.map((k) => [k, val(`${prefix}.${k}`)]).filter(([, x]) => x));
    const side = pf.dataset.side, plan = side === "HEALTH_PLAN";
    const rb = {
      party: group("party", ["legal_name", "trade_name", "address", "city", "state", "zip", "product_name", "contact_person", "physical_address", "physical_city", "physical_state", "physical_zip", "phone", "fax", "email"]),
      attest_signature: group("att", ["printed_name", "title", "signature", "date", "drawn"]),
      claim_info: group("info", ["claims_count", "dates_of_service", "type_of_service", "patient_names"]),
      position: group("pos", ["basis", "references", "rebuttal"]),
      documents: [...pf.querySelectorAll('input[name="doc"]:checked')].map((x) => x.value),
      certification: { agreed: !!pf.querySelector('[name="cert.agreed"]')?.checked, ...group("cert", ["printed_name", "title", "signature", "date", "drawn"]) },
    };
    for (const k of ["coding_rules", "other"]) if (val(`spec.${k}`)) rb[k + "_specify"] = val(`spec.${k}`);
    if (plan) {
      rb.smmc = !!pf.querySelector('[name="smmc"]')?.checked;
      if (!rb.smmc) {
        rb.participate = pf.querySelector('[name="participate"]:checked')?.value || "";
        rb.challenge_eligibility = pf.querySelector('[name="challenge"]:checked')?.value || "";
      }
    }
    if (rb.challenge_eligibility === "yes") {
      rb.exclusions = [...pf.querySelectorAll('input[name="ex"]:checked')].map((x) => x.value);
      rb.exclusions_certified = !!pf.querySelector('[name="ex_certified"]')?.checked;
    }
    rb.worksheet = [];
    pf.querySelectorAll(".rb-ws tr.pf-claim").forEach((tr, i) => {
      const w = Object.fromEntries([...tr.querySelectorAll("input[data-col]")].map((x) => [x.dataset.col, x.value.trim()]));
      if (!Object.values(w).some(Boolean)) return;
      if (!w.claim_number) { problems.push(`Worksheet row ${i + 1}: enter the claim #`); return; }
      const row = { claim_number: w.claim_number, patient_name: w.patient_name, type_of_service: w.type_of_service, date_of_service: w.date_of_service,
        date_claim_submitted: w.date_claim_submitted, date_of_denial: w.date_of_denial, date_grievance_completed: w.date_grievance_completed };
      for (const k of ["billed", "paid", "disputed"]) {
        const c = cents(w[k]);
        if (Number.isNaN(c)) problems.push(`Worksheet claim ${w.claim_number}: "${w[k]}" is not an amount`); else if (c != null) row[k + "_cents"] = c;
      }
      rb.worksheet.push(row);
    });
    // The same required items rebuttal.go checks.
    if (!rb.party.legal_name) problems.push("Responding party: legal name is required");
    if (!rb.party.contact_person) problems.push("Responding party: contact person is required");
    if (!(rb.party.email || "").includes("@")) problems.push("Responding party: e-mail is required");
    if (plan && !rb.smmc) {
      if (!rb.participate) problems.push("Say whether you wish to participate in the dispute process");
      if (!rb.challenge_eligibility) problems.push("Say whether you challenge the eligibility of this claim");
    }
    if (rb.challenge_eligibility === "yes") {
      if (!rb.exclusions.length) problems.push("Eligibility attestation: tick the exclusion(s) this claim meets");
      if (!rb.exclusions_certified) problems.push("Eligibility attestation: certify you are authorized to complete it");
    }
    const sig = rb.attest_signature;
    if (!sig.printed_name || !sig.title || !sig.signature || !sig.date) problems.push("Signature: printed name, title, electronic signature and date are required");
    if (!sig.drawn) problems.push("Signature: sign in the signature box");
    if (rb.participate !== "no") {
      if (!rb.position.basis) problems.push("Your position: describe the basis (question 1)");
      const c = rb.certification;
      if (!c.agreed || !c.printed_name || !c.title || !c.signature || !c.date) problems.push("Certification: tick it and enter your printed name, title, electronic signature and the date");
      if (!c.drawn) problems.push("Certification: sign in the signature box");
    }
    return { rebuttal: rb, problems };
  }

  async function fromXlsx(input) {
    const sec = input.closest(".pf-sec"), msg = sec.querySelector(".pf-csv-msg"), file = input.files[0];
    if (!file) return;
    try {
      if (!/\.xlsx$/i.test(file.name)) throw new Error("Upload an Excel workbook (.xlsx). In Excel, use File › Save As › Excel Workbook.");
      const { claims, problems } = PacketForm.claimsFromRows(await PacketForm.xlsxRows(await file.arrayBuffer()));
      const tb = sec.querySelector("tbody");
      tb.querySelectorAll("tr.pf-claim").forEach((tr) => { if (![...tr.querySelectorAll("input")].some((x) => x.value.trim())) tr.remove(); });
      const have = new Set([...tb.querySelectorAll('input[data-col="claim_number"]')].map((x) => x.value.trim()).filter(Boolean));
      let added = 0;
      for (const c of claims) {
        if (have.has(c.claim_number) || have.size >= 500) continue;
        tb.insertAdjacentHTML("beforeend", wsRow({ ...c, billed_cents: c.billed_cents, paid_cents: c.paid_cents, disputed_cents: c.disputed_cents }));
        have.add(c.claim_number); added++;
      }
      msg.textContent = `Added ${added} claim${added === 1 ? "" : "s"} from ${file.name}.` + (problems.length ? " " + problems.slice(0, 5).join("; ") + "." : "");
    } catch (e) { msg.textContent = e.message; } finally { input.value = ""; }
  }
  function template(ev) {
    ev.preventDefault();
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([PacketForm.xlsxBytes([WS_COLS.map(([, l]) => l)])], { type: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" }));
    a.download = "claim-information-worksheet.xlsx"; document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  }
  return { html, bind, read, fromXlsx, template, row: () => wsRow() };
})();
