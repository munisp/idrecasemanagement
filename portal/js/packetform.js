// packetform.js — the Florida claim dispute filing packet (Capitol Bridge) as a form, used by the public filing
// page (dispute.html), the TPA and health-plan filing screens and the staff case page. Its sections and choices
// are the packet's own; it saves into the same case fields doc-intel fills when it reads an uploaded packet
// (services/doc-intel-py/packet_docs.py), through filing_packet.go.
//
//   PacketForm.html(values, opts)  -> the form's markup (values: {details, disputed_amount_cents, claims})
//   PacketForm.read(root)          -> the packet to post, as filing_packet.go reads it
//   PacketForm.fill(root, values)  -> put values into a rendered form
//   PacketForm.fromPdf(file)       -> values read from a filled packet PDF's own form fields (no upload)
/* global PDFLib */
window.PacketForm = (() => {
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  // The packet PDF's form fields (they carry no meaningful names) and what each one is -- read off the blank
  // 2026 packet by position. Must stay identical to FORM_FIELDS in packet_docs.py (test_packet_docs.py checks).
  // FORM_FIELDS-BEGIN
  const FORM_FIELDS = {
    "Text2": "provider_org", "Text3": "provider_legal_entity", "Text4": "provider_trade_name",
    "Text5": "provider_tin_npi", "Text6": "provider_license", "Text7": "provider_street", "Text8": "provider_city",
    "Text9": "provider_state", "Text10": "provider_postal", "Text11": "provider_contact",
    "Text12": "provider_alt_address", "Text13": "provider_phone", "Text14": "provider_fax", "Text15": "provider_email",
    "Text18": "provider_type_other", "Text16": "health_plan", "Text17": "health_plan_trade_name",
    "Text19": "plan_type_other", "Text20": "type_of_service_other", "Text21": "claims_submitted",
    "Text22": "aggregated_disputed_amount",
    "Text23": "provider_position_basis", "Text24": "provider_position_references", "Text25": "provider_position_rebuttal",
    "Text26": "plan_position_basis", "Text27": "plan_position_references", "Text28": "plan_position_rebuttal",
    "Text38": "documents_other", "Text39": "attested_by_name", "Text200": "attested_by_title", "Date49_af_date": "attested_on",
    "Text40": "aor_provider", "Text41": "aor_health_plan", "Text42": "aor_representative",
    "Text43": "aor_signed_by_name", "Text201": "aor_signed_by_title", "Date51_af_date": "aor_signed_on",
    "Text44": "certified_by_name", "Text202": "certified_by_title", "Date52_af_date": "certified_on"
  };
  // FORM_FIELDS-END
  const PROVIDER_TYPES = ["Hospital", "Other Inpatient Facility", "DME/Medical Supplier", "Chiropractor", "Podiatrist",
    "Physical/Occupational/Speech Therapy", "Rural Hospital", "Physician/Medical Group", "Laboratory/Imaging", "Dentist",
    "Home Health Agency", "Transportation", "Pharmacy", "Other"];
  const PLAN_TYPES = [["Commercial HMO", "Commercial Health Maintenance Organization (HMO)"],
    ["Medicaid HMO", "Medicaid Health Maintenance Organization (HMO)"], ["Prepaid Health Plan", "Prepaid Health Plan"],
    ["Exclusive Provider Organization (EPO)", "Exclusive Provider Organization (EPO)"],
    ["Major Medical Expense Health Insurance Policy (PPO)", "Major Medical Expense Health Insurance Policy (including Preferred Provider Organization)"],
    ["Medicaid", "Medicaid"], ["Other", "Other"]];
  const SERVICE_TYPES = [["Hospital Inpatient", "Hospital Inpatient Services"], ["Hospital Outpatient", "Hospital Outpatient Services"],
    ["Professional", "Professional Services"], ["Other", "Other"]];
  const DOCUMENTS = [["claim_forms", "ALL claim form(s) submitted to the health plan", true],
    ["denials_eobs", "ALL denial(s) / Explanation of Benefits received from the health plan", true],
    ["florida_license", "Documentation that the provider is licensed in the state of Florida", true],
    ["final_determination_letter", "Final determination letter / decision, with documentation of the health plan appeal process", true],
    ["contract", "Contract between the provider and health plan"], ["waiver_of_payment", "Waiver of payment form"],
    ["notice_of_overpayment", "Notice of overpayment from the health plan"], ["overpayment_response", "Response to notice of overpayment from the provider"],
    ["medical_records", "Medical records"], ["coding_rules", "Coding rules / policies"], ["other", "Other"]];
  const DOC_BOXES = ["Check Box27", "Check Box28", "Check Box29", "Check Box30", "Check Box31", "Check Box32", "Check Box33",
    "Check Box34", "Check Box35", "Check Box36", "Check Box37"];
  const ATTESTATION = [["interest_only", "This claim is not related to an interest payment."],
    ["late_payment_only", "This claim is not related solely to an issue of prompt payment."],
    ["medicare_grievance", "This claim is not a part of an internal grievance in a Medicare Managed Care Organization or a reconsideration appeal through the Medicare reconsideration process."],
    ["medicaid_fair_hearing", "This claim is not a part of a Medicaid fair hearing pursued under 42 CFR §431.220 et seq."],
    ["pending_court_action", "This claim is not the basis for an action pending in state or federal court."],
    ["pre_2000_binding_process", "This claim is not subject to a binding claim dispute resolution process provided by contract entered into prior to October 1, 2000."],
    ["internal_process_not_exhausted", "This claim is not subject to an internal dispute resolution process provided by contract entered into or renewed on or after October 1, 2000, or this claim is subject to such a process, which has been exhausted."],
    ["over_12_months", "No more than 12 months have elapsed since the final determination was made on this claim."],
    ["rural_hospital", "Claim is being filed by a Rural Hospital (attach documentation that the hospital meets the definition under s. 395.602(2), F.S.)."]];
  const ATT_BOXES = ["Check Box40", "Check Box41", "Check Box42", "Check Box43", "Check Box44", "Check Box45", "Check Box46", "Check Box47", "Check Box48"];
  const CLAIM_COLS = [["claim_number", "Claim #", "text"], ["patient_name", "Patient name", "text"], ["type_of_service", "Type of service", "text"],
    ["date_of_service", "Date of service", "date"], ["billed", "Amount billed", "money"], ["paid", "Amount paid", "money"],
    ["disputed", "Amount in dispute", "money"], ["date_claim_submitted", "Date claim submitted", "date"],
    ["date_of_denial", "Date of denial", "date"], ["date_of_final_determination", "Date of final determination", "date"]];
  const PDF_CLAIM_COLS = ["Claim Row", "Patient NameRow", "Type of ServiceRow", "Date of ServiceRow", "Amount BilledRow", "Amount PaidRow",
    "Amount in DisputeRow", "Date Claim SubmittedRow", "Date of DenialRow", "Date of Final DeterminationRow"];

  const money = (cents) => (cents == null || cents === "" ? "" : (Number(cents) / 100).toFixed(2));
  const toCents = (s) => {
    const v = String(s ?? "").replace(/[$,\s]/g, "");
    if (v === "") return null;
    const n = Number(v);
    return Number.isFinite(n) ? Math.round(n * 100) : NaN;
  };
  const isoDate = (s) => {
    const t = String(s ?? "").trim();
    if (/^\d{4}-\d{2}-\d{2}$/.test(t)) return t;
    const m = t.match(/^(\d{1,2})[/.-](\d{1,2})[/.-](\d{4}|\d{2})(?!\d)/);
    if (!m) return "";
    const y = m[3].length === 2 ? 2000 + Number(m[3]) : Number(m[3]);
    return `${y}-${String(m[1]).padStart(2, "0")}-${String(m[2]).padStart(2, "0")}`;
  };

  function text(d, key, label, attrs = "", after = "") {
    return `<label>${esc(label)}${after} <input name="d.${key}" value="${esc(d[key] ?? "")}" ${attrs} /></label>`;
  }
  function radios(d, key, options, cls = "") {
    const cur = d[key];
    return `<div class="pf-choices ${cls}" role="radiogroup">${options.map(([v, label]) =>
      `<label class="pf-choice"><input type="radio" name="d.${key}" value="${esc(v)}" ${cur === v || (typeof cur === "boolean" && String(cur) === v) ? "checked" : ""} /> ${esc(label)}</label>`).join("")}</div>`;
  }
  function claimRow(c = {}) {
    const name = [c.patient_first_name, c.patient_last_name].filter(Boolean).join(" ");
    const v = { ...c, patient_name: c.patient_name ?? name, billed: money(c.billed_cents), paid: money(c.paid_cents),
      disputed: money(c.disputed_cents) };
    for (const k of ["date_of_service", "date_claim_submitted", "date_of_denial", "date_of_final_determination"]) v[k] = String(v[k] || "").slice(0, 10);
    return `<tr class="pf-claim" data-orig="${esc(c.claim_number || "")}">${CLAIM_COLS.map(([k, label, type]) =>
      `<td><input data-col="${k}" aria-label="${esc(label)}" ${type === "date" ? 'type="date"' : type === "money" ? 'inputmode="decimal" placeholder="0.00"' : ""} value="${esc(v[k] ?? "")}" /></td>`).join("")}
      <td><button type="button" class="pf-del" title="Remove this claim" onclick="PacketForm.removeRow(this)">×</button></td></tr>`;
  }

  // The form. opts: { id, partyFixed: "PROVIDER"|"HEALTH_PLAN" (the filer's side is already known), staff: bool,
  // filer: bool -- the filer's own form (public, TPA, health plan): required fields marked and enforced, the
  // plan's notice contact not asked of a provider, and the AOR letter upload (change log 2026-11-25 #1, #2, #6). }
  function html(values = {}, opts = {}) {
    const R = opts.filer ? '<span class="pf-req" title="required">*</span>' : "";
    const d = { ...(values.details || {}) };
    if (opts.partyFixed) d.filing_party_type = opts.partyFixed;
    if (d.packet_provider_type && String(d.packet_provider_type).startsWith("Other:")) {
      d.provider_type_other = d.provider_type_other || d.packet_provider_type.slice(6).trim();
      d.packet_provider_type = "Other";
    }
    if (d.plan_type && !PLAN_TYPES.some(([v]) => v === d.plan_type)) { d.plan_type_other = d.plan_type; d.plan_type = "Other"; }
    if (d.type_of_service && !SERVICE_TYPES.some(([v]) => v === d.type_of_service)) { d.type_of_service_other = d.type_of_service; d.type_of_service = "Other"; }
    if (!d.provider_tin && !d.provider_npi && d.provider_tin_npi) Object.assign(d, splitTinNpi(d.provider_tin_npi));
    const claims = values.claims && values.claims.length ? values.claims : [];
    const rows = claims.map(claimRow).join("") + Array.from({ length: Math.max(0, 5 - claims.length) }, () => claimRow()).join("");
    const att = d.packet_attestation || {}, docs = d.packet_documents_submitted || [];
    const party = d.filing_party_type || "PROVIDER";
    return `<div class="pf" id="${esc(opts.id || "pf")}" data-party="${esc(party)}"${opts.filer ? ' data-filer="1"' : ""}>
      ${opts.filer ? '<p class="pf-hint pf-req-hint">Fields marked <span class="pf-req">*</span> are required.</p>' : ""}
      <fieldset class="pf-sec"><legend>Filing party</legend>
        ${opts.partyFixed ? `<p class="pf-fixed">${party === "HEALTH_PLAN" ? "Health plan" : "Provider"}</p><input type="hidden" name="d.filing_party_type" value="${esc(party)}" />`
          : radios(d, "filing_party_type", [["PROVIDER", "Provider"], ["HEALTH_PLAN", "Health plan"]], "pf-inline")}</fieldset>
      <fieldset class="pf-sec"><legend>Provider</legend><div class="pf-grid">
        ${text(d, "provider_org", "Provider")}${text(d, "provider_legal_entity", "Legal entity")}${text(d, "provider_trade_name", "Trade name")}
        ${text(d, "provider_tin", "Tax ID", 'placeholder="12-3456789"')}${text(d, "provider_npi", "NPI", 'inputmode="numeric" maxlength="10"')}
        ${text(d, "provider_license", "License no.")}${text(d, "provider_street", "Street address")}${text(d, "provider_city", "City")}
        ${text(d, "provider_state", "State", 'maxlength="2" placeholder="FL"')}${text(d, "provider_postal", "ZIP code", 'placeholder="12345"')}
        ${text(d, "provider_contact", "Contact person")}${text(d, "provider_alt_address", "Address (if different from above)")}
        ${text(d, "provider_phone", "Telephone number", 'type="tel"')}${text(d, "provider_fax", "Fax number", 'type="tel"')}
        ${text(d, "provider_email", "E-mail address", 'type="email"')}</div></fieldset>
      <fieldset class="pf-sec"><legend>Provider type${R}</legend>
        ${radios(d, "packet_provider_type", PROVIDER_TYPES.map((x) => [x, x]), "pf-cols")}
        ${text(d, "provider_type_other", "Other provider type")}</fieldset>
      <fieldset class="pf-sec"><legend>Health plan</legend><div class="pf-grid">
        ${text(d, "health_plan", "Health plan name", "", R)}${text(d, "health_plan_trade_name", "Trade name")}</div>
        <div class="pf-plan-contact"><p class="pf-q">Health plan contact for dispute notices</p>
        <p class="pf-hint">Not on the PDF packet. When a provider files, this is where the health plan is told about the dispute and sent its documents.</p>
        <div class="pf-grid">${text(d, "health_plan_contact", "Contact person")}${text(d, "health_plan_email", "E-mail address", 'type="email"')}
          ${text(d, "health_plan_phone", "Telephone number", 'type="tel"')}${text(d, "health_plan_naic", "NAIC (optional)")}</div></div>
        <p class="pf-q">Health plan type${R}</p>${radios(d, "plan_type", PLAN_TYPES)}${text(d, "plan_type_other", "Other plan type")}</fieldset>
      <fieldset class="pf-sec"><legend>The dispute</legend>
        <p class="pf-q">Is the provider contracted with the health plan?</p>${radios(d, "contracted", [["true", "Yes"], ["false", "No"]], "pf-inline")}
        <p class="pf-q">Has the provider fully exhausted an internal dispute resolution process?</p>${radios(d, "internal_dispute_exhausted", [["Yes", "Yes"], ["No", "No"], ["Unknown", "Unknown"]], "pf-inline")}
        <p class="pf-q">Type of service${R}</p>${radios(d, "type_of_service", SERVICE_TYPES)}${text(d, "type_of_service_other", "Other type of service")}
        <div class="pf-grid">${text(d, "claims_submitted", "Number of claims included", 'type="number" min="0" step="1"', R)}
          <label>Aggregated disputed amount ($)${R} <input name="disputed_amount" inputmode="decimal" placeholder="0.00" value="${esc(money(values.disputed_amount_cents))}" /></label></div></fieldset>
      <fieldset class="pf-sec"><legend>Claim(s) information${R}</legend>
        <p class="pf-hint">One row per claim. Many claims? Upload them from an Excel file (up to ${MAX_CLAIMS}), then check the rows below — you can still add or remove claims by hand.</p>
        <div class="pf-csv"><label>Upload claims from an Excel file (.xlsx) <input type="file" accept=".xlsx,${XLSX_TYPE}"
          onchange="PacketForm.claimsFromXlsx(this)" /></label>
          <a href="#" class="pf-csv-template" onclick="PacketForm.xlsxTemplate(event)">Download the Excel template</a>
          <span class="pf-csv-msg" aria-live="polite"></span></div>
        <div class="pf-scroll"><table class="pf-claims"><thead><tr>${CLAIM_COLS.map(([, l]) => `<th>${esc(l)}</th>`).join("")}<th></th></tr></thead>
          <tbody>${rows}</tbody></table></div>
        <button type="button" class="pf-add" onclick="PacketForm.addRow(this)">Add a claim</button></fieldset>
      <fieldset class="pf-sec"><legend>Full review fee</legend>
        <p class="pf-q">Do you want an estimate of the full review fee for this case?</p>
        ${radios(d, "fee_estimate_requested", [["true", "Yes — if the case is eligible, Capitol Bridge provides an estimate before accepting the case for review"],
          ["false", "No — if the case is eligible, Capitol Bridge accepts the case, issues a recommendation and includes an invoice for the full review"]])}</fieldset>
      <fieldset class="pf-sec pf-pos-provider"><legend>If the filing party is a provider: your position</legend>
        ${area(d, "provider_position_basis", "1. Describe with all appropriate technical detail the basis for and amount of payment you believe you should have received for these services.")}
        ${area(d, "provider_position_references", "2. Provide specific references to the contract, provider manual and other supporting documentation to support your position.")}
        ${area(d, "provider_position_rebuttal", "3. State your understanding of the basis for the claim calculation and payment made by the health plan, and your rebuttal argument against it.")}</fieldset>
      <fieldset class="pf-sec pf-pos-plan"><legend>If the filing party is a health plan: your position</legend>
        ${area(d, "plan_position_basis", "1. Describe with all appropriate technical detail the basis for the determination that an overpayment was made for these services and the amount of that overpayment.")}
        ${area(d, "plan_position_references", "2. Provide specific references to the contract, provider manual and other supporting documentation to support your position.")}
        ${area(d, "plan_position_rebuttal", "3. State your understanding of the basis for the claim calculation made by the provider in denying that an overpayment was made, and your rebuttal argument against it.")}</fieldset>
      <fieldset class="pf-sec"><legend>Supporting documentation being submitted${R}</legend>
        <p class="pf-hint">* Required for all disputes. If a required document can't be filed, upload an explanation in its place.</p>
        <div class="pf-checks">${DOCUMENTS.map(([k, label, req]) => `<label class="pf-choice"><input type="checkbox" name="doc" value="${k}" ${docs.includes(k) ? "checked" : ""} /> ${req ? "* " : ""}${esc(label)}</label>`).join("")}</div>
        ${text(d, "documents_other", "Other documentation")}</fieldset>
      <fieldset class="pf-sec"><legend>Eligibility attestation</legend>
        <p class="pf-hint">To be completed by the owner, officer, director or other legally authorized representative of the filing party. Tick every criterion this claim meets.</p>
        <div class="pf-checks">${ATTESTATION.map(([k, label]) => `<label class="pf-choice"><input type="checkbox" name="att" value="${k}" ${att[k] ? "checked" : ""} /> ${esc(label)}</label>`).join("")}</div>
        <p class="pf-note pf-rural-note" role="status" ${att.rural_hospital ? "" : "hidden"}><b>Rural hospital:</b> remember to include the documentation showing the hospital meets the definition under s. 395.602(2), F.S., with your supporting documents.</p>
        <label class="pf-choice pf-certify"><input type="checkbox" name="d.attestation_certified" ${d.attestation_certified ? "checked" : ""} /> I certify that I am legally authorized to complete this attestation on behalf of the filing party, and that this claim meets every criterion ticked above.</label>
        <div class="pf-grid">${text(d, "attested_by_name", "Name (typed as your electronic signature)")}${text(d, "attested_by_title", "Title & organization name")}
          ${text(d, "attested_on", "Date", 'type="date"')}</div></fieldset>
      <fieldset class="pf-sec"><legend>Appointment of representative</legend>
        <div class="pf-grid">${text(d, "aor_provider", "Provider")}${text(d, "aor_health_plan", "Health plan")}</div>
        ${text(d, "aor_representative", "Is hereby appointed to act as the representative of the provider for this case")}
        <p class="pf-hint">The appointed representative receives all correspondence, including requests for information, the final order and the invoice.</p>
        ${opts.filer ? `<label class="pf-file">Appointment of representative letter <span class="pf-hint">— a PDF on your company letterhead,
          naming who may act for you (<a href="/assets/aor-letter-example.pdf" target="_blank" rel="noopener">see an example</a>)</span>
          <input type="file" name="aor_letter_file" accept="application/pdf,.pdf" /></label>` : ""}
        <div class="pf-grid">${text(d, "aor_signed_by_name", "Name (typed as your electronic signature)")}${text(d, "aor_signed_by_title", "Title & organization name")}
          ${text(d, "aor_signed_on", "Date", 'type="date"')}</div></fieldset>
      <fieldset class="pf-sec"><legend>Certification${R}</legend>
        <label class="pf-choice pf-certify"><input type="checkbox" name="d.certification_agreed" ${d.certification_agreed ? "checked" : ""} /> I certify that I am legally authorized to complete this certification on behalf of the filing party. I understand and agree that if the filing party does not prevail it will be responsible for the full or apportioned review costs of this case; that if those costs are not paid within 35 days after the Agency's final order it will be subject to a fine; that if this case is settled before a decision is rendered, the parties will be responsible for the review costs incurred before the settlement; and that the filing party will be responsible for the initial review fee.</label>
        <div class="pf-grid">${text(d, "certified_by_name", "Name (typed as your electronic signature)")}${text(d, "certified_by_title", "Title & organization name")}
          ${text(d, "certified_on", "Date", 'type="date"')}</div></fieldset>
    </div>`;
  }
  function area(d, key, label) {
    return `<label class="pf-area">${esc(label)}<textarea name="d.${key}" rows="4">${esc(d[key] ?? "")}</textarea></label>`;
  }

  // Shows the position section of the filing party's side only.
  function bind(root) {
    const pf = root.querySelector(".pf") || root;
    const sync = () => {
      const party = (pf.querySelector('[name="d.filing_party_type"]:checked') || pf.querySelector('input[type=hidden][name="d.filing_party_type"]') || {}).value || "PROVIDER";
      pf.dataset.party = party;
    };
    pf.addEventListener("change", (ev) => {
      if (ev.target.name === "d.filing_party_type") sync();
      if (ev.target.name === "att" && ev.target.value === "rural_hospital") {
        const note = pf.querySelector(".pf-rural-note");
        if (note) note.hidden = !ev.target.checked;
      }
    });
    // Whether the filer used the form at all: an untouched form is not sent (its empty tick boxes would read as
    // "nothing attested").
    const touch = () => { pf.dataset.touched = "1"; };
    pf.addEventListener("input", touch);
    pf.addEventListener("change", touch);
    sync();
  }
  const touched = (root) => (root.querySelector(".pf") || root).dataset.touched === "1";
  function addRow(btn) {
    const tb = btn.closest(".pf-sec").querySelector("tbody");
    tb.insertAdjacentHTML("beforeend", claimRow());
    tb.lastElementChild.querySelector("input").focus();
  }
  function removeRow(btn) {
    const tr = btn.closest("tr"), pf = btn.closest(".pf");
    if (tr.dataset.orig) pf.dataset.removed = [...(pf.dataset.removed ? pf.dataset.removed.split("\n") : []), tr.dataset.orig].join("\n");
    tr.remove();
  }

  // Claims from an Excel workbook (.xlsx), read in the browser (nothing is uploaded) -- the change log asks for
  // Excel only (2026-11-25 #3). The rows land in the claims table, where the filer checks them and can still add or
  // remove claims by hand. The first row names the columns: the template's headings, or common variants of them.
  // An .xlsx is a ZIP of XML parts; the browser's own DecompressionStream inflates it (no library, CSP-safe).
  const MAX_CLAIMS = 500; // filing_packet.go refuses more rows than this
  const XLSX_TYPE = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet";
  const CSV_HEADINGS = {
    claim_number: ["claim", "claimno", "claimnumber", "claimid"],
    patient_name: ["patient", "patientname", "member", "membername"],
    patient_first_name: ["patientfirstname", "firstname", "memberfirstname"],
    patient_last_name: ["patientlastname", "lastname", "memberlastname"],
    type_of_service: ["typeofservice", "service", "servicetype"],
    date_of_service: ["dateofservice", "dos", "servicedate"],
    billed: ["amountbilled", "billed", "billedamount", "charges", "totalcharges"],
    paid: ["amountpaid", "paid", "paidamount"],
    disputed: ["amountindispute", "disputed", "disputedamount", "amountdisputed", "indispute"],
    date_claim_submitted: ["dateclaimsubmitted", "submitted", "datesubmitted", "submitteddate", "submissiondate"],
    date_of_denial: ["dateofdenial", "denialdate", "denied", "datedenied"],
    date_of_final_determination: ["dateoffinaldetermination", "finaldeterminationdate", "finaldetermination"],
    // The plan's Claim Information Worksheet (rebuttalform.js)
    date_grievance_completed: ["dateinternalgrievanceprocesscompleted", "internalgrievanceprocesscompleted", "dategrievancecompleted", "grievancecompleted"],
  };
  const DATE_COLS = ["date_of_service", "date_claim_submitted", "date_of_denial", "date_of_final_determination", "date_grievance_completed"];
  const headingKey = (h) => {
    const n = String(h ?? "").toLowerCase().replace(/#/g, "no").replace(/[^a-z0-9]/g, "");
    return Object.keys(CSV_HEADINGS).find((k) => CSV_HEADINGS[k].includes(n)) || null;
  };
  const realDate = (iso) => {
    const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(iso || ""), d = m && new Date(Date.UTC(+m[1], +m[2] - 1, +m[3]));
    return d && d.getUTCMonth() === +m[2] - 1 && d.getUTCDate() === +m[3] ? iso : "";
  };
  // An Excel date is a day count from 1899-12-30 (Excel's 1900 leap-year quirk included).
  const serialDate = (n) => new Date(Date.UTC(1899, 11, 30) + Math.round(n) * 86400000).toISOString().slice(0, 10);
  // "1,250.00", "$1250", "(40.00)" (negative), and "12,50" (a decimal comma); a number cell is used as it is.
  const cellCents = (x) => {
    if (typeof x === "number") return Math.round(x * 100);
    let v = String(x).trim().replace(/^\((.*)\)$/, "-$1");
    if (/^-?[$€£]?\s*\d{1,3}(\.\d{3})*,\d{2}$|^-?[$€£]?\s*\d+,\d{2}$/.test(v)) v = v.replace(/\./g, "").replace(",", ".");
    return toCents(v.replace(/[€£]/g, ""));
  };
  const cellText = (x) => (x == null ? "" : typeof x === "number" ? String(x) : String(x).trim());

  // ---- a minimal .xlsx reader / writer ----
  async function unzip(buf) {
    const dv = new DataView(buf), u8 = new Uint8Array(buf), dec = new TextDecoder();
    let eocd = -1;
    for (let i = u8.length - 22; i >= Math.max(0, u8.length - 65557); i--) if (dv.getUint32(i, true) === 0x06054b50) { eocd = i; break; }
    if (eocd < 0) throw new Error("That file is not an Excel workbook (.xlsx).");
    const out = new Map(), n = dv.getUint16(eocd + 10, true);
    let p = dv.getUint32(eocd + 16, true);
    for (let k = 0; k < n && dv.getUint32(p, true) === 0x02014b50; k++) {
      const method = dv.getUint16(p + 10, true), size = dv.getUint32(p + 20, true), nl = dv.getUint16(p + 28, true),
        el = dv.getUint16(p + 30, true), cl = dv.getUint16(p + 32, true), lo = dv.getUint32(p + 42, true);
      const name = dec.decode(u8.subarray(p + 46, p + 46 + nl));
      p += 46 + nl + el + cl;
      const start = lo + 30 + dv.getUint16(lo + 26, true) + dv.getUint16(lo + 28, true);
      out.set(name.replace(/^\//, ""), { method, data: u8.subarray(start, start + size) });
    }
    return out;
  }
  async function zipText(entries, name) {
    const e = entries.get(name);
    if (!e) return null;
    let bytes = e.data;
    if (e.method === 8) bytes = new Uint8Array(await new Response(new Blob([e.data]).stream().pipeThrough(new DecompressionStream("deflate-raw"))).arrayBuffer());
    else if (e.method !== 0) throw new Error("This workbook is compressed in a way the reader does not know — save it again from Excel.");
    return new TextDecoder().decode(bytes);
  }
  const xml = (t) => new DOMParser().parseFromString(t, "application/xml");
  const tags = (doc, name) => [...doc.getElementsByTagNameNS("*", name)];
  // The first worksheet's cells as rows of strings / numbers.
  async function xlsxRows(buf) {
    const z = await unzip(buf);
    const wb = await zipText(z, "xl/workbook.xml");
    if (!wb) throw new Error("That file is not an Excel workbook (.xlsx).");
    const first = tags(xml(wb), "sheet")[0];
    const rid = first && (first.getAttribute("r:id") || first.getAttributeNS("http://schemas.openxmlformats.org/officeDocument/2006/relationships", "id"));
    let target = "worksheets/sheet1.xml";
    const rels = await zipText(z, "xl/_rels/workbook.xml.rels");
    if (rels && rid) { const r = tags(xml(rels), "Relationship").find((x) => x.getAttribute("Id") === rid); if (r) target = r.getAttribute("Target"); }
    const path = target.startsWith("/") ? target.slice(1) : "xl/" + target.replace(/^\.\//, "");
    const sheet = await zipText(z, path);
    if (!sheet) throw new Error("The workbook's first sheet could not be read.");
    const sstText = await zipText(z, "xl/sharedStrings.xml");
    const sst = sstText ? tags(xml(sstText), "si").map((si) => tags(si, "t").map((t) => t.textContent).join("")) : [];
    const col = (ref) => { let n = 0; for (const ch of ref.replace(/\d+$/, "")) n = n * 26 + ch.charCodeAt(0) - 64; return n - 1; };
    const rows = [];
    for (const row of tags(xml(sheet), "row")) {
      const out = [];
      tags(row, "c").forEach((c, i) => {
        const t = c.getAttribute("t"), v = tags(c, "v")[0], ref = c.getAttribute("r");
        let val = "";
        if (t === "s") val = sst[Number(v && v.textContent)] ?? "";
        else if (t === "inlineStr") val = tags(c, "t").map((x) => x.textContent).join("");
        else if (t === "str" || t === "e") val = v ? v.textContent : "";
        else if (t === "b") val = v && v.textContent === "1" ? "TRUE" : "FALSE";
        else if (v && v.textContent !== "") val = Number(v.textContent);
        out[ref ? col(ref) : i] = val;
      });
      rows[(Number(row.getAttribute("r")) || rows.length + 1) - 1] = Array.from(out, (x) => x ?? "");
    }
    return Array.from(rows, (r) => r || []);
  }
  const CRC = (() => { const t = new Uint32Array(256); for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; t[n] = c >>> 0; } return t; })();
  const crc32 = (b) => { let c = 0xffffffff; for (const x of b) c = CRC[(c ^ x) & 0xff] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };
  function zipStore(files) { // [[name, string]] -> Uint8Array, stored (no compression)
    const enc = new TextEncoder(), parts = [], central = [];
    let off = 0;
    for (const [name, text] of files) {
      const nb = enc.encode(name), data = enc.encode(text), crc = crc32(data);
      const h = new DataView(new ArrayBuffer(30));
      h.setUint32(0, 0x04034b50, true); h.setUint16(4, 20, true); h.setUint32(14, crc, true);
      h.setUint32(18, data.length, true); h.setUint32(22, data.length, true); h.setUint16(26, nb.length, true);
      parts.push(new Uint8Array(h.buffer), nb, data);
      const c = new DataView(new ArrayBuffer(46));
      c.setUint32(0, 0x02014b50, true); c.setUint16(4, 20, true); c.setUint16(6, 20, true); c.setUint32(16, crc, true);
      c.setUint32(20, data.length, true); c.setUint32(24, data.length, true); c.setUint16(28, nb.length, true); c.setUint32(42, off, true);
      central.push(new Uint8Array(c.buffer), nb);
      off += 30 + nb.length + data.length;
    }
    const csize = central.reduce((n, b) => n + b.length, 0), e = new DataView(new ArrayBuffer(22));
    e.setUint32(0, 0x06054b50, true); e.setUint16(8, files.length, true); e.setUint16(10, files.length, true);
    e.setUint32(12, csize, true); e.setUint32(16, off, true);
    const all = [...parts, ...central, new Uint8Array(e.buffer)], out = new Uint8Array(all.reduce((n, b) => n + b.length, 0));
    let p = 0; for (const b of all) { out.set(b, p); p += b.length; }
    return out;
  }
  function xlsxBytes(rows) { // rows of strings / numbers -> a one-sheet workbook
    const x = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
    const ref = (i) => { let s = ""; for (let n = i + 1; n > 0; n = Math.floor((n - 1) / 26)) s = String.fromCharCode(65 + ((n - 1) % 26)) + s; return s; };
    const sheet = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>${
      rows.map((r, ri) => `<row r="${ri + 1}">${r.map((v, ci) => typeof v === "number"
        ? `<c r="${ref(ci)}${ri + 1}"><v>${v}</v></c>`
        : `<c r="${ref(ci)}${ri + 1}" t="inlineStr"${ri === 0 ? ' s="1"' : ""}><is><t>${x(v)}</t></is></c>`).join("")}</row>`).join("")}</sheetData></worksheet>`;
    const M = "http://schemas.openxmlformats.org", R = `${M}/officeDocument/2006/relationships`;
    return zipStore([
      ["[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="${M}/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/></Types>`],
      ["_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="${M}/package/2006/relationships"><Relationship Id="rId1" Type="${R}/officeDocument" Target="xl/workbook.xml"/></Relationships>`],
      ["xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="${M}/spreadsheetml/2006/main" xmlns:r="${R}"><sheets><sheet name="Claims" sheetId="1" r:id="rId1"/></sheets></workbook>`],
      ["xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="${M}/package/2006/relationships"><Relationship Id="rId1" Type="${R}/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="${R}/styles" Target="styles.xml"/></Relationships>`],
      ["xl/styles.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="${M}/spreadsheetml/2006/main"><fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts><fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`],
      ["xl/worksheets/sheet1.xml", sheet],
    ]);
  }

  // {claims, problems} from a sheet's rows. Dates: a real Excel date, MM/DD/YYYY or YYYY-MM-DD; amounts: numbers or text.
  function claimsFromRows(rows) {
    const problems = [], claims = [];
    const h = rows.findIndex((r) => r.some((x) => cellText(x)));
    if (h < 0) return { claims, problems: ["The sheet is empty."] };
    const keys = rows[h].map(headingKey);
    if (!keys.includes("claim_number")) {
      return { claims, problems: ["The first row must name the columns, with a Claim # column — download the Excel template to see them."] };
    }
    rows.forEach((r, i) => {
      if (i <= h) return;
      const line = i + 1, v = {};
      keys.forEach((k, j) => { if (k && r[j] != null && cellText(r[j]) !== "") v[k] = r[j]; });
      if (!Object.keys(v).length) return;
      if (!v.claim_number) { problems.push(`Row ${line}: no claim #, skipped`); return; }
      const c = { claim_number: cellText(v.claim_number).slice(0, 60), type_of_service: cellText(v.type_of_service),
        patient_name: cellText(v.patient_name) || [cellText(v.patient_first_name), cellText(v.patient_last_name)].filter(Boolean).join(" ") };
      for (const k of DATE_COLS) {
        if (v[k] == null) continue;
        c[k] = typeof v[k] === "number" ? (v[k] > 0 && v[k] < 2958466 ? serialDate(v[k]) : "") : realDate(isoDate(v[k]));
        if (!c[k]) problems.push(`Row ${line} (claim ${c.claim_number}): "${cellText(v[k])}" is not a date — left blank`);
      }
      for (const [k, col] of [["billed_cents", "billed"], ["paid_cents", "paid"], ["disputed_cents", "disputed"]]) {
        if (v[col] == null) continue;
        const cents = cellCents(v[col]);
        if (Number.isNaN(cents)) problems.push(`Row ${line} (claim ${c.claim_number}): "${cellText(v[col])}" is not an amount — left blank`);
        else c[k] = cents;
      }
      claims.push(c);
    });
    return { claims, problems };
  }
  async function claimsFromXlsx(input) {
    const sec = input.closest(".pf-sec"), pf = input.closest(".pf"), msg = sec.querySelector(".pf-csv-msg"), file = input.files[0];
    if (!file) return;
    msg.textContent = "Reading the workbook…";
    try {
      if (!/\.xlsx$/i.test(file.name)) throw new Error("Upload an Excel workbook (.xlsx). In Excel, use File › Save As › Excel Workbook.");
      if (file.size > 5 << 20) throw new Error("That file is too large — a workbook of up to 500 claims is well under 5 MB.");
      const { claims, problems } = claimsFromRows(await xlsxRows(await file.arrayBuffer()));
      const tb = sec.querySelector("tbody");
      const filled = (tr) => [...tr.querySelectorAll("input[data-col]")].some((x) => x.value.trim());
      tb.querySelectorAll("tr.pf-claim").forEach((tr) => { if (!filled(tr)) tr.remove(); });
      const have = new Set([...tb.querySelectorAll('input[data-col="claim_number"]')].map((x) => x.value.trim()).filter(Boolean));
      let added = 0, dup = 0, over = 0;
      for (const c of claims) {
        if (have.has(c.claim_number)) { dup++; continue; }
        if (have.size >= MAX_CLAIMS) { over++; continue; }
        tb.insertAdjacentHTML("beforeend", claimRow(c));
        tb.lastElementChild.dataset.orig = ""; // a new row, not a saved claim being edited
        have.add(c.claim_number); added++;
      }
      // Fill the totals the filer hasn't typed themselves.
      const count = pf.querySelector('[name="d.claims_submitted"]'), amount = pf.querySelector('[name="disputed_amount"]');
      if (count && (!count.value || count.dataset.fromCsv === count.value)) { count.value = String(have.size); count.dataset.fromCsv = count.value; }
      if (amount && (!amount.value || amount.dataset.fromCsv === amount.value)) {
        let sum = 0, any = false;
        tb.querySelectorAll('input[data-col="disputed"]').forEach((x) => { const c = toCents(x.value); if (c != null && !Number.isNaN(c)) { sum += c; any = true; } });
        if (any) { amount.value = money(sum); amount.dataset.fromCsv = amount.value; }
      }
      pf.dataset.touched = "1";
      const notes = [`Added ${added} claim${added === 1 ? "" : "s"} from ${file.name}.`];
      if (dup) notes.push(`${dup} already in the table, skipped.`);
      if (over) notes.push(`${over} over the ${MAX_CLAIMS}-claim limit, not added — file the rest as a separate dispute.`);
      if (problems.length) notes.push(problems.slice(0, 5).join("; ") + (problems.length > 5 ? `; and ${problems.length - 5} more` : "") + ".");
      if (added) notes.push("Check the rows below before you send.");
      msg.textContent = notes.join(" ");
    } catch (e) {
      msg.textContent = e.message || "Could not read that workbook.";
    } finally {
      input.value = ""; // picking the same file again re-reads it
    }
  }
  function xlsxTemplate(ev) {
    ev.preventDefault();
    const head = CLAIM_COLS.map(([, label]) => label);
    const example = ["ABC123456", "Jane Doe", "Emergency room visit", "01/15/2026", 1250, 300, 950, "02/01/2026", "03/01/2026", "04/15/2026"];
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([xlsxBytes([head, example])], { type: XLSX_TYPE }));
    a.download = "claims-template.xlsx";
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  }

  function splitTinNpi(v) {
    const s = String(v || ""), out = {};
    const npi = s.match(/\b\d{10}\b/), tin = s.match(/\b\d{2}-?\d{7}\b/);
    if (npi) out.provider_npi = npi[0];
    if (tin && (!npi || tin[0] !== npi[0].slice(0, tin[0].length))) out.provider_tin = tin[0];
    return out;
  }

  // The packet to post. Returns {packet, problems}: problems are what the filer must fix before it can be sent.
  function read(root) {
    const pf = root.querySelector(".pf") || root;
    const details = {}, problems = [];
    pf.querySelectorAll('input[name^="d."], textarea[name^="d."]').forEach((el) => {
      const key = el.name.slice(2);
      if (el.type === "radio") { if (el.checked) details[key] = el.value === "true" ? true : el.value === "false" ? false : el.value; return; }
      if (el.type === "checkbox") { details[key] = el.checked; return; }
      const v = el.value.trim();
      if (v === "") return;
      details[key] = key === "claims_submitted" ? Number(v) : v;
    });
    if (details.provider_state) details.provider_state = details.provider_state.toUpperCase();
    details.packet_documents_submitted = [...pf.querySelectorAll('input[name="doc"]:checked')].map((x) => x.value);
    details.packet_attestation = Object.fromEntries([...pf.querySelectorAll('input[name="att"]')].map((x) => [x.value, x.checked]));
    const amt = toCents(pf.querySelector('[name="disputed_amount"]')?.value);
    if (Number.isNaN(amt)) problems.push("Aggregated disputed amount: enter an amount like 1250.00");
    const claims = [];
    pf.querySelectorAll("tr.pf-claim").forEach((tr, i) => {
      const v = Object.fromEntries([...tr.querySelectorAll("input[data-col]")].map((x) => [x.dataset.col, x.value.trim()]));
      if (!Object.values(v).some(Boolean)) return;
      if (!v.claim_number) { problems.push(`Claim row ${i + 1}: enter the claim #`); return; }
      const parts = v.patient_name.split(/\s+/).filter(Boolean);
      const [first, last] = v.patient_name.includes(",")
        ? [v.patient_name.split(",")[1].trim(), v.patient_name.split(",")[0].trim()]
        : [parts.slice(0, -1).join(" "), parts.slice(-1)[0] || ""];
      const c = { claim_number: v.claim_number, patient_first_name: first, patient_last_name: last, type_of_service: v.type_of_service,
        date_of_service: v.date_of_service, date_claim_submitted: v.date_claim_submitted, date_of_denial: v.date_of_denial,
        date_of_final_determination: v.date_of_final_determination };
      for (const [k, col] of [["billed_cents", "billed"], ["paid_cents", "paid"], ["disputed_cents", "disputed"]]) {
        const cents = toCents(v[col]);
        if (Number.isNaN(cents)) problems.push(`Claim ${v.claim_number}: "${v[col]}" is not an amount`);
        else if (cents != null) c[k] = cents;
      }
      claims.push(c);
    });
    if (pf.dataset.filer === "1") problems.push(...requiredProblems(details, amt, claims));
    const remove = (pf.dataset.removed || "").split("\n").filter((x) => x && !claims.some((c) => c.claim_number === x));
    return { packet: { details, disputed_amount_cents: amt, claims, remove_claims: remove }, problems };
  }

  // The change log's required fields (2026-11-25 #2) -- the same list filing_packet.go requiredProblems enforces.
  const REQUIRED_DOCS = DOCUMENTS.filter(([, , req]) => req).map(([k]) => k);
  function requiredProblems(d, amt, claims) {
    const out = [], other = (k, o) => d[k] && (d[k] !== "Other" || d[o]);
    if (!other("packet_provider_type", "provider_type_other")) out.push("Provider type is required");
    if (!d.health_plan) out.push("Health plan name is required");
    if (!other("plan_type", "plan_type_other")) out.push("Health plan type is required");
    if (!other("type_of_service", "type_of_service_other")) out.push("Type of service is required");
    if (!(Number(d.claims_submitted) >= 1)) out.push("Number of claims is required");
    if (!(amt > 0)) out.push("Aggregated disputed amount is required");
    if (!claims.length) out.push("Claim(s) information: enter at least one claim");
    if (REQUIRED_DOCS.some((k) => !(d.packet_documents_submitted || []).includes(k))) out.push("Supporting documents: tick every required document (*) you are submitting");
    if (!d.certification_agreed || !d.certified_by_name || !d.certified_by_title || !d.certified_on) out.push("Certification: tick it and enter your name, title and the date");
    return out;
  }

  // The files that go with the filing (change log #6, #8): the packet PDF the form was filled from, and the AOR
  // letter. [{kind, filename, data_b64}] for packet.attachments; PDFs up to 10 MB.
  async function attachments(root) {
    const host = root.querySelector(".pf")?.parentElement || root, out = [];
    const b64 = async (f) => { const u8 = new Uint8Array(await f.arrayBuffer()); let s = ""; for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode(...u8.subarray(i, i + 0x8000)); return btoa(s); };
    const add = async (kind, f, what) => {
      if (!f) return;
      if (f.size > 10 << 20) throw new Error(`${what} is over 10 MB.`);
      if (!/\.pdf$/i.test(f.name) && f.type !== "application/pdf") throw new Error(`${what} must be a PDF.`);
      out.push({ kind, filename: f.name, data_b64: await b64(f) });
    };
    await add("filing_packet", host._packetFile, "The packet PDF");
    await add("aor_letter", host.querySelector('input[name="aor_letter_file"]')?.files?.[0], "The appointment of representative letter");
    return out;
  }
  function clearFiles(root) {
    const host = root.querySelector(".pf")?.parentElement || root;
    delete host._packetFile;
    const f = host.querySelector('input[name="aor_letter_file"]'); if (f) f.value = "";
  }

  // Who is who in a read packet: the side filing and the side the dispute is against, from its own fields.
  function sides(packet) {
    const d = packet.details, plan = d.filing_party_type === "HEALTH_PLAN", provider = d.provider_org || d.provider_legal_entity || "";
    return { plan, otherLabel: plan ? "Provider" : "Health plan",
      filerEmail: (plan ? d.health_plan_email : d.provider_email) || "", filerContact: (plan ? d.health_plan_contact : d.provider_contact) || "", filerOrg: (plan ? d.health_plan : provider) || "",
      otherName: (plan ? provider : d.health_plan) || "", otherEmail: (plan ? d.provider_email : d.health_plan_email) || "", otherNpi: plan ? (d.provider_npi || "") : "" };
  }

  // Re-render a form with new values. opts.partyFixed changes the fixed filing party; otherwise it is kept.
  function fill(root, values, opts = {}) {
    const host = root.querySelector(".pf")?.parentElement || root;
    const id = (root.querySelector(".pf") || {}).id;
    const party = opts.partyFixed || root.querySelector('input[type=hidden][name="d.filing_party_type"]')?.value;
    const wasTouched = touched(host), filer = (root.querySelector(".pf") || {}).dataset?.filer === "1";
    host.innerHTML = html(values, { id, partyFixed: party, filer: opts.filer ?? filer });
    bind(host);
    if (wasTouched || opts.touched) host.querySelector(".pf").dataset.touched = "1";
  }

  // Values from a filled packet PDF: its own form fields, read in the browser with pdf-lib (nothing is uploaded).
  // A scanned packet has no form fields -- that one is read after upload, by document analysis.
  async function fromPdf(file) {
    if (!window.PDFLib) {
      await new Promise((ok, fail) => {
        const s = document.createElement("script");
        s.src = "/js/vendor/pdf-lib.min.js"; s.onload = ok; s.onerror = () => fail(new Error("could not load the PDF reader"));
        document.head.appendChild(s);
      });
    }
    const doc = await PDFLib.PDFDocument.load(await file.arrayBuffer(), { ignoreEncryption: true });
    const v = {};
    for (const f of doc.getForm().getFields()) {
      const name = f.getName();
      try {
        if (f instanceof PDFLib.PDFTextField) { v[name] = f.getText() || ""; continue; }
        // Radio groups and tick boxes by their raw value (/V): this packet's export values do not match its
        // appearance states, so pdf-lib's getSelected()/isChecked() miss what a viewer actually wrote.
        const raw = f.acroField.V && f.acroField.V();
        const val = raw ? String(raw.decodeText ? raw.decodeText() : raw.toString()).replace(/^[(/]+|\)$/g, "").replace(/^\//, "") : "";
        if (f instanceof PDFLib.PDFRadioGroup) v[name] = val === "Off" ? "" : val;
        else if (f instanceof PDFLib.PDFCheckBox) v[name] = !!val && val !== "Off";
      } catch (_) { /* a field the reader can't decode is left out */ }
    }
    if (!Object.values(v).some((x) => x === true || (typeof x === "string" && x.trim()))) {
      throw new Error("This PDF has no filled-in form fields (a scanned or printed packet?). Upload it with your documents and it will be read for you, or fill in the form here.");
    }
    const d = {};
    for (const [field, key] of Object.entries(FORM_FIELDS)) if (String(v[field] || "").trim()) d[key] = String(v[field]).trim();
    const pick = (g, list) => { const s = String(v[g] || ""); return /^\d+$/.test(s) && Number(s) < list.length ? list[Number(s)] : undefined; };
    if (v.Group1) d.filing_party_type = v.Group1 === "Choice2" ? "HEALTH_PLAN" : "PROVIDER";
    const pt = pick("Group2", PROVIDER_TYPES); if (pt) d.packet_provider_type = pt;
    const plan = pick("Group3", PLAN_TYPES.map((x) => x[0])); if (plan) d.plan_type = plan;
    const ct = pick("Group4", [true, false]); if (ct !== undefined) d.contracted = ct;
    const ex = pick("Group5", ["Yes", "No", "Unknown"]); if (ex) d.internal_dispute_exhausted = ex;
    const st = pick("Group6", SERVICE_TYPES.map((x) => x[0])); if (st) d.type_of_service = st;
    const fee = pick("Group7", [true, false]); if (fee !== undefined) d.fee_estimate_requested = fee;
    Object.assign(d, splitTinNpi(d.provider_tin_npi)); delete d.provider_tin_npi;
    if (d.provider_state && /^florida$/i.test(d.provider_state)) d.provider_state = "FL";
    if (d.claims_submitted) d.claims_submitted = (String(d.claims_submitted).match(/\d+/) || [""])[0];
    for (const k of ["attested_on", "aor_signed_on", "certified_on"]) if (d[k]) d[k] = isoDate(d[k]);
    const amount = d.aggregated_disputed_amount; delete d.aggregated_disputed_amount;
    if (DOC_BOXES.some((b) => v[b]) || ATT_BOXES.some((b) => v[b])) {
      d.packet_documents_submitted = DOCUMENTS.map(([k], i) => (v[DOC_BOXES[i]] ? k : null)).filter(Boolean);
      d.packet_attestation = Object.fromEntries(ATTESTATION.map(([k], i) => [k, !!v[ATT_BOXES[i]]]));
    }
    const claims = [];
    for (let row = 1; row <= 5; row++) {
      const cell = (i) => String(v[PDF_CLAIM_COLS[i] + row] || "").trim();
      if (!cell(0)) continue;
      const cents = (i) => { const c = toCents(cell(i)); return Number.isNaN(c) ? null : c; };
      claims.push({ claim_number: cell(0), patient_name: cell(1), type_of_service: cell(2), date_of_service: isoDate(cell(3)),
        billed_cents: cents(4), paid_cents: cents(5), disputed_cents: cents(6), date_claim_submitted: isoDate(cell(7)),
        date_of_denial: isoDate(cell(8)), date_of_final_determination: isoDate(cell(9)) });
    }
    const agg = toCents(amount);
    return { details: d, disputed_amount_cents: Number.isNaN(agg) ? null : agg, claims };
  }

  // A "Fill from my packet PDF" control for a form host: picks a file, reads it, refills the form.
  function pdfPicker(hostSel) {
    return `<label class="pf-pdf">Already filled in the packet PDF? <input type="file" accept="application/pdf,.pdf"
      onchange="PacketForm.fillFromPicker(this, '${esc(hostSel)}')" /> <span class="pf-pdf-msg" aria-live="polite"></span></label>`;
  }
  async function fillFromPicker(input, hostSel) {
    const msg = input.parentElement.querySelector(".pf-pdf-msg"), host = document.querySelector(hostSel), file = input.files[0];
    if (!file || !host) return;
    msg.textContent = "Reading the packet…";
    try {
      const values = await fromPdf(file);
      fill(host, values, { touched: true });
      host._packetFile = file; // kept with the filing (change log #8)
      host.dispatchEvent(new CustomEvent("packetfilled", { detail: values, bubbles: true }));
      msg.textContent = `Filled in from ${file.name} — check every section before you send it.`;
    } catch (e) { msg.textContent = e.message; }
  }

  return { html, bind, read, fill, touched, sides, fromPdf, pdfPicker, fillFromPicker, addRow, removeRow, FORM_FIELDS,
    claimsFromXlsx, claimsFromRows, xlsxRows, xlsxBytes, xlsxTemplate, attachments, clearFiles };
})();
