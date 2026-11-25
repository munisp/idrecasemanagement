"""The Florida Statewide Provider and Health Plan Claim Dispute Resolution Program filing packet (Capitol Bridge), read
so nobody re-types it into the case: the filing party, the provider block, the health plan and its type, the contract
and internal-dispute answers, the type of service, the claim count and aggregated amount, the claims table, the
fee-estimate answer, both parties' positions, the supporting-documentation list, the eligibility attestation and the
appointment of representative.

The packet is a fillable PDF. Its fields carry no meaningful names (Text2..Text28, Group1..Group7, Check Box27..48),
so FORM_FIELDS maps each one to what it is by where it sits on the page -- read once off the blank packet, see the
comment above it. A packet filled on a computer is read from those fields exactly. A packet printed, filled by hand
and scanned has no fields: its OCR text is read label by label instead, which recovers the typed or handwritten
lines but not the tick boxes (a tick is not text), so the boxes stay unknown rather than guessed.

extract() returns {"case": {...}, "claims": [...], "attestation": {...}, "documents_submitted": [...],
"representative": {...}, "source": "form"|"text"} -- only what the packet states; an empty field is left out.
"""
from __future__ import annotations

import io
import re

TYPE_SIGNALS = {
    "filing_packet": {
        "strong": ["claim dispute resolution program filing packet", "aggregated disputed amount", "eligibility attestation",
                   "appointment of representative", "filing party", "estimate of the full review fee",
                   "if the filing party is a provider"],
        "weak": ["legal entity", "trade name", "heath plan type", "health plan type", "contact person",
                 "number of claims included", "supporting documentation", "provider type"],
    },
}

# Where each field of the packet sits and what it is. Read off the blank packet (2026 edition, 6 pages) by matching
# every widget's rectangle to the label printed on its line: page 1 is the provider block (Text2..15 run down the
# page from "Provider" to "E-mail Address"), the provider type (Group2: options 0-6 the left column Hospital..Rural
# Hospital, 7-13 the right column Physician/Medical Group..Other), the plan (Text16/17) and plan type (Group3);
# page 2 the yes/no questions, the type of service, the counts and the five claim rows; pages 3/4 the positions;
# page 5 the documentation and attestation boxes; page 6 the appointment of representative and the certification.
FORM_FIELDS = {
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
    "Text44": "certified_by_name", "Text202": "certified_by_title", "Date52_af_date": "certified_on",
}
FILING_PARTY = {"Choice1": "PROVIDER", "Choice2": "HEALTH_PLAN"}  # Group1
PROVIDER_TYPES = ["Hospital", "Other Inpatient Facility", "DME/Medical Supplier", "Chiropractor", "Podiatrist",
                  "Physical/Occupational/Speech Therapy", "Rural Hospital", "Physician/Medical Group",
                  "Laboratory/Imaging", "Dentist", "Home Health Agency", "Transportation", "Pharmacy", "Other"]  # Group2
PLAN_TYPES = ["Commercial HMO", "Medicaid HMO", "Prepaid Health Plan", "Exclusive Provider Organization (EPO)",
              "Major Medical Expense Health Insurance Policy (PPO)", "Medicaid", "Other"]  # Group3
YES_NO = [True, False]                                                                  # Group4 (contracted), Group7
EXHAUSTED = ["Yes", "No", "Unknown"]                                                    # Group5
SERVICE_TYPES = ["Hospital Inpatient", "Hospital Outpatient", "Professional", "Other"]  # Group6
DOCUMENT_BOXES = {
    "Check Box27": "claim_forms", "Check Box28": "denials_eobs", "Check Box29": "florida_license",
    "Check Box30": "final_determination_letter", "Check Box31": "contract", "Check Box32": "waiver_of_payment",
    "Check Box33": "notice_of_overpayment", "Check Box34": "overpayment_response", "Check Box35": "medical_records",
    "Check Box36": "coding_rules", "Check Box37": "other",
}
REQUIRED_DOCUMENTS = ("claim_forms", "denials_eobs", "florida_license", "final_determination_letter")
# The attestation boxes, each named by the eligibility flag (case-api eligibility.go) the criterion rules out -- an
# unticked box is the filer NOT attesting to it, which the eligibility review looks at; it is never a flag by itself.
ATTESTATION_BOXES = {
    "Check Box40": "interest_only", "Check Box41": "late_payment_only", "Check Box42": "medicare_grievance",
    "Check Box43": "medicaid_fair_hearing", "Check Box44": "pending_court_action",
    "Check Box45": "pre_2000_binding_process", "Check Box46": "internal_process_not_exhausted",
    "Check Box47": "over_12_months", "Check Box48": "rural_hospital",
}
CLAIM_COLUMNS = {
    "Claim ": "claim_number", "Patient Name": "patient_name", "Type of Service": "type_of_service",
    "Date of Service": "date_of_service", "Amount Billed": "billed_cents", "Amount Paid": "paid_cents",
    "Amount in Dispute": "disputed_cents", "Date Claim Submitted": "date_claim_submitted",
    "Date of Denial": "date_of_denial", "Date of Final Determination": "date_of_final_determination",
}

_MONEY = re.compile(r"-?\(?\$?\s*-?[\d,]*\d(?:\.\d{1,2})?\)?")
_DATE = re.compile(r"\b(\d{1,2})[/\-.](\d{1,2})[/\-.](\d{2}|\d{4})\b|\b(\d{4})-(\d{2})-(\d{2})\b")


def cents(value) -> int | None:
    """'$724,469.61' / '724469.61' / '(1,200.00)' -> cents; None for anything that is not one amount."""
    s = str(value or "").strip()
    if not s or not _MONEY.fullmatch(s):
        return None
    neg = s.startswith("(") or "-" in s
    digits = re.sub(r"[^\d.]", "", s)
    if not digits or digits.count(".") > 1:
        return None
    return -round(float(digits) * 100) if neg else round(float(digits) * 100)


def iso_dates(value) -> list[str]:
    """Every date written in the value, as ISO: '05/24/2024-03/15/2025' gives both. Two-digit years are 20xx."""
    out = []
    for m in _DATE.finditer(str(value or "")):
        if m.group(4):
            y, mo, d = int(m.group(4)), int(m.group(5)), int(m.group(6))
        else:
            mo, d, y = int(m.group(1)), int(m.group(2)), int(m.group(3))
            y += 2000 if y < 100 else 0
        if 1 <= mo <= 12 and 1 <= d <= 31 and 1990 <= y <= 2100:
            out.append(f"{y:04d}-{mo:02d}-{d:02d}")
    return out


def split_name(name: str) -> tuple[str, str]:
    """'Doe, Jane' or 'Jane Doe' -> ('Jane', 'Doe'). One word is a last name."""
    n = " ".join(str(name or "").split())
    if "," in n:
        last, first = (p.strip() for p in n.split(",", 1))
        return first, last
    parts = n.rsplit(" ", 1)
    return (parts[0], parts[1]) if len(parts) == 2 else ("", n)


def program_provider_type(provider_type: str | None, type_of_service: str | None) -> str | None:
    """The program's threshold provider type from the packet's two answers: a rural hospital is one outright; a
    hospital is inpatient or outpatient by the type of service ticked; a physician group or dentist is
    physician_dentist; any other ticked type is 'other'. A hospital with no service type stays unknown."""
    p = (provider_type or "").lower()
    s = (type_of_service or "").lower()
    if not p:
        return None
    if p == "rural hospital":
        return "rural_hospital"
    if p == "hospital":
        if "inpatient" in s:
            return "hospital_inpatient"
        if "outpatient" in s:
            return "hospital_outpatient"
        return None
    if p in ("physician/medical group", "dentist"):
        return "physician_dentist"
    return "other"


def _plan_line_of_business(plan_type: str | None) -> str | None:
    p = (plan_type or "").lower()
    if "medicaid" in p:
        return "Medicaid"
    if p and p != "other":
        return "Commercial"
    return None


def _choice(value, options: list):
    """A radio group's value ('/3' or '3') -> options[3]; None when nothing is chosen."""
    v = str(value or "").lstrip("/")
    if not v or v == "Off":
        return None
    if v.isdigit() and int(v) < len(options):
        return options[int(v)]
    return None


def form_values(raw: bytes) -> dict[str, str]:
    """The packet's filled form fields as {field name: value}, or {} for a PDF without fields (a scan) or not a PDF.
    Values are strings; radio groups give their chosen option ('/3'), tick boxes '/Yes' or '/Off'."""
    if not raw or not raw.lstrip()[:5].startswith(b"%PDF"):
        return {}
    try:
        from pypdf import PdfReader
        fields = PdfReader(io.BytesIO(raw)).get_fields() or {}
    except Exception:  # noqa: BLE001 -- an unreadable form is read from its text instead
        return {}
    out = {}
    for name, f in fields.items():
        v = f.get("/V")
        if v is None:
            continue
        v = str(v).strip()
        if v and v != "/Off":
            out[name] = v
    return out


def from_form(values: dict[str, str]) -> dict:
    """Read a filled packet's form values (form_values) into the packet's facts."""
    case: dict = {}
    for field, key in FORM_FIELDS.items():
        v = " ".join(str(values.get(field) or "").split()) if key not in _LONG_TEXT else str(values.get(field) or "").strip()
        if v:
            case[key] = v
    party = FILING_PARTY.get(str(values.get("Group1") or "").lstrip("/"))
    if party:
        case["filing_party_type"] = party
    ptype = _choice(values.get("Group2"), PROVIDER_TYPES)
    if ptype:
        case["packet_provider_type"] = ptype if ptype != "Other" else f"Other: {case.get('provider_type_other', '')}".rstrip(": ")
    plan = _choice(values.get("Group3"), PLAN_TYPES)
    if plan:
        case["plan_type"] = plan if plan != "Other" else (case.get("plan_type_other") or "Other")
    contracted = _choice(values.get("Group4"), YES_NO)
    if contracted is not None:
        case["contracted"] = contracted
    exhausted = _choice(values.get("Group5"), EXHAUSTED)
    if exhausted:
        case["internal_dispute_exhausted"] = exhausted
    service = _choice(values.get("Group6"), SERVICE_TYPES)
    if service:
        case["type_of_service"] = service if service != "Other" else (case.get("type_of_service_other") or "Other")
    estimate = _choice(values.get("Group7"), YES_NO)
    if estimate is not None:
        case["fee_estimate_requested"] = estimate
    claims = []
    for row in range(1, 6):
        claim = {}
        for col, key in CLAIM_COLUMNS.items():
            v = " ".join(str(values.get(f"{col}Row{row}" if col != "Claim " else f"Claim Row{row}") or "").split())
            if v:
                claim[key] = v
        if claim:
            claims.append(claim)
    ticked = {k for k, v in values.items() if str(v).lstrip("/") not in ("", "Off")}
    any_box = any(b in ticked for b in (*DOCUMENT_BOXES, *ATTESTATION_BOXES))
    out = _finish(case, claims)
    if any_box:
        out["documents_submitted"] = [DOCUMENT_BOXES[b] for b in DOCUMENT_BOXES if b in ticked]
        out["attestation"] = {flag: (b in ticked) for b, flag in ATTESTATION_BOXES.items()}
    out["source"] = "form"
    return out


_LONG_TEXT = {"provider_position_basis", "provider_position_references", "provider_position_rebuttal",
              "plan_position_basis", "plan_position_references", "plan_position_rebuttal"}

# A packet read from OCR text: each label and what follows it on its line. The labels are the packet's own; a blank
# printed line ("____") is not a value.
_TEXT_LABELS = [
    ("provider_legal_entity", r"legal\s+entity"), ("provider_tin_npi", r"tax\s+id\s*/?\s*npi(?:\s+no\.?)?"),
    ("provider_license", r"license\s+no\.?"), ("provider_street", r"street\s+address"), ("provider_city", r"city"),
    ("provider_state", r"state"), ("provider_postal", r"zip\s+code"), ("provider_contact", r"contact\s+person"),
    ("provider_phone", r"telephone\s+number"), ("provider_fax", r"fax\s+number"), ("provider_email", r"e-?\s*mail\s+address"),
    ("claims_submitted", r"number\s+of\s+claims\s+included"), ("aggregated_disputed_amount", r"aggregated\s+disputed\s+amount"),
]


def from_text(text: str) -> dict:
    """Read a scanned packet's OCR text: the labelled lines of the provider block and the two totals. The first
    'Trade Name' line is the provider's, the second the plan's; 'Health Plan' alone on a line is the plan."""
    case: dict = {}
    lines = [re.sub(r"[*_|]+", " ", ln).strip() for ln in (text or "").splitlines()]
    for key, label in _TEXT_LABELS:
        for ln in lines:
            m = re.match(rf"(?i)^\W*{label}\b\s*[:\-]?\s*(.*)$", ln)
            if m and m.group(1).strip() and key not in case:
                case[key] = " ".join(m.group(1).split())
                break
    trade = [m.group(1).strip() for ln in lines for m in [re.match(r"(?i)^\W*trade\s+name\s*:?\s*(.+)$", ln)] if m]
    if trade:
        case["provider_trade_name"] = trade[0]
    if len(trade) > 1:
        case["health_plan_trade_name"] = trade[1]
    for ln in lines:
        m = re.match(r"(?i)^\W*health\s+plan\s*:?\s+(?!type)(.+)$", ln)
        if m:
            case["health_plan"] = " ".join(m.group(1).split())
            break
    out = _finish(case, [])
    out["source"] = "text"
    return out


def _finish(case: dict, claims: list[dict]) -> dict:
    """Normalise the read values: amounts to cents, dates to ISO, the state to its code, a count to a number; derive
    the program's provider type and line of business; split the claim rows' names and dates."""
    if "provider_state" in case:
        st = re.sub(r"[^A-Za-z]", "", case["provider_state"]).upper()
        case["provider_state"] = st[:2] if st in ("FL", "FLORIDA") or len(st) == 2 else case["provider_state"]
        if st == "FLORIDA":
            case["provider_state"] = "FL"
    if "provider_postal" in case:
        m = re.search(r"\b\d{5}(?:-\d{4})?\b", case["provider_postal"])
        if m:
            case["provider_postal"] = m.group(0)
        else:
            case.pop("provider_postal")
    if "claims_submitted" in case:
        m = re.search(r"\d+", case["claims_submitted"])
        if m:
            case["claims_submitted"] = int(m.group(0))
        else:
            case.pop("claims_submitted")
    if "aggregated_disputed_amount" in case:
        c = cents(case.pop("aggregated_disputed_amount"))
        if c is not None and c > 0:
            case["disputed_amount_cents"] = c
    tin_npi = case.get("provider_tin_npi", "")
    m = re.search(r"\b\d{10}\b", tin_npi)
    if m:
        case["provider_npi"] = m.group(0)
    m = re.search(r"\b\d{2}-?\d{7}\b", tin_npi)
    if m and (not case.get("provider_npi") or m.group(0) != case["provider_npi"][:len(m.group(0))]):
        case["provider_tin"] = m.group(0)
    for key in ("attested_on", "aor_signed_on", "certified_on"):
        if key in case:
            d = iso_dates(case[key])
            if d:
                case[key] = d[0]
    ptype = program_provider_type(case.get("packet_provider_type"), case.get("type_of_service"))
    if ptype:
        case["provider_type"] = ptype
    lob = _plan_line_of_business(case.get("plan_type"))
    if lob:
        case["line_of_business"] = lob
    if not case.get("provider_org"):
        name = case.get("provider_legal_entity") or case.get("provider_trade_name")
        if name:
            case["provider_org"] = name
    out_claims = []
    for row in claims:
        claim = {"claim_number": re.sub(r"\s+", "", row.get("claim_number", ""))}
        if not claim["claim_number"]:
            continue
        if row.get("patient_name"):
            claim["patient_first_name"], claim["patient_last_name"] = split_name(row["patient_name"])
        if row.get("type_of_service"):
            claim["type_of_service"] = row["type_of_service"]
        for key in ("billed_cents", "paid_cents", "disputed_cents"):
            c = cents(row.get(key))
            if c is not None:
                claim[key] = c
        if "disputed_cents" not in claim and "billed_cents" in claim and "paid_cents" in claim:
            claim["disputed_cents"] = claim["billed_cents"] - claim["paid_cents"]
        dos = iso_dates(row.get("date_of_service"))
        if dos:
            claim["date_of_service"] = dos[0]
            if len(dos) > 1:
                claim["date_of_service_through"] = dos[-1]
        for key in ("date_claim_submitted", "date_of_denial", "date_of_final_determination"):
            d = iso_dates(row.get(key))
            if d:
                claim[key] = d[0]
        out_claims.append(claim)
    return {"case": case, "claims": out_claims}


def extract(doc_type: str, text: str, raw: bytes | None = None) -> tuple[dict, list[str]]:
    """The packet's facts and the problems the packet itself shows (its claim rows not adding up to the aggregated
    amount, a required document not ticked, a claim count that is not the number of rows). ({}, []) for any other
    document type, and for a packet with nothing filled in."""
    if doc_type != "filing_packet":
        return {}, []
    values = form_values(raw or b"")
    facts = from_form(values) if values else from_text(text)
    if not facts["case"] and not facts["claims"]:
        return {}, []
    issues = []
    case, claims = facts["case"], facts["claims"]
    total = sum(c.get("disputed_cents", 0) for c in claims)
    if claims and case.get("disputed_amount_cents") and total and total != case["disputed_amount_cents"] \
            and len(claims) == case.get("claims_submitted", len(claims)):
        issues.append(f"The packet's claim rows dispute ${total / 100:,.2f} but its aggregated disputed amount is "
                      f"${case['disputed_amount_cents'] / 100:,.2f}")
    if claims and case.get("claims_submitted") and case["claims_submitted"] < len(claims):
        issues.append(f"The packet lists {len(claims)} claims but says {case['claims_submitted']} are included")
    if "documents_submitted" in facts:
        missing = [d.replace("_", " ") for d in REQUIRED_DOCUMENTS if d not in facts["documents_submitted"]]
        if missing:
            issues.append("Required documentation not ticked on the packet: " + ", ".join(missing))
    return facts, issues
