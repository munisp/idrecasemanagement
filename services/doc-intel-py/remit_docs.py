"""What a payer's remittance and a hospital's claim form state about a claim, read deterministically so the claims
table and the case fill themselves:

  epic_835   an 835 remittance printed from Epic ("Claim Details" / "Service Line Information" / "Payee Information"):
             the claim charge and paid amounts, the claim filing indicator, the payer control number, the facility
             and frequency codes, the DRG, each revenue-code line (billed, paid, units, reason codes, service date),
             the issue date, the trace number, the payer and the payee with its NPI.
  payer_eob  a payer's own EOB with a claim number, a facility type code, dated service lines and an adjustment
             reason legend (Molina's layout): the claim number and date, the billed total and the benefit paid, each
             line's billed amount and dates, the reason codes with the payer's own wording, the payment date.
  ub04       a UB-04 (CMS-1450): the type of bill (FL 4), the billing provider and its NPI (FL 1, FL 56), the patient
             control number (FL 3a), the payer and its health plan id (FL 38/50/51), the total charges (FL 47 totals),
             the revenue codes (FL 42), the PPS code (FL 71), the attending NPI (FL 76).

The text is what Docling / OCR made of the page -- table cells split by "|", scanned boxes out of reading order --
so every value is anchored on the form's own label, and a label without a readable value leaves the value out rather
than borrowing a neighbour's. Bank routing and account numbers, which an 835 printout carries, are never read.

extract() returns (key, facts, issues): key "remit" or "ub04", facts {"layout", "case": {...}, "claims": [...], ...}.
"""
from __future__ import annotations

import re

from fields import valid_npi

# Claim adjustment reason codes (X12 CARC) and remark codes (RARC) the program sees most, in short words; a code
# not listed keeps the wording the document itself prints, or just the code.
CARC = {
    "1": "Deductible amount", "2": "Coinsurance amount", "3": "Co-payment amount",
    "4": "Procedure code inconsistent with the modifier", "11": "Diagnosis inconsistent with the procedure",
    "16": "Claim lacks information or has submission/billing errors", "18": "Exact duplicate claim/service",
    "22": "May be covered by another payer (coordination of benefits)", "23": "Impact of prior payer adjudication",
    "27": "Expenses incurred after coverage terminated", "29": "Time limit for filing has expired",
    "45": "Charge exceeds fee schedule/maximum allowable", "50": "Not deemed a medical necessity by the payer",
    "96": "Non-covered charge(s)", "97": "Benefit included in payment for another service (bundled)",
    "109": "Not covered by this payer/contractor", "119": "Benefit maximum reached",
    "151": "Payer deems the information does not support this many services",
    "167": "Diagnosis not covered", "170": "Payment denied when performed by this provider type",
    "177": "Patient has not met the required eligibility", "181": "Procedure code was invalid on the date of service",
    "182": "Procedure modifier was invalid on the date of service",
    "197": "Precertification/authorization/notification absent", "198": "Precertification/authorization exceeded",
    "204": "Service not covered under the patient's current benefit plan", "226": "Information requested was not provided",
    "242": "Services not provided by network/primary care providers", "252": "An attachment/other documentation is required",
}
RARC = {
    "M20": "Missing/incomplete/invalid HCPCS", "M15": "Separately billed services bundled", "M127": "Missing medical record",
    "N30": "Patient ineligible for this service", "N290": "Missing/incomplete/invalid rendering provider identifier",
    "N382": "Missing/incomplete/invalid patient identifier", "MA130": "Claim contains incomplete/invalid information",
    "N54": "Claim information inconsistent with pre-certified/authorized services",
}
GROUP = {"CO": "contractual obligation", "PR": "patient responsibility", "OA": "other adjustment",
         "PI": "payer-initiated reduction", "CR": "correction/reversal"}
FILING_INDICATOR = {"MC": "Medicaid", "MA": "Medicare Part A", "MB": "Medicare Part B", "HM": "HMO",
                    "CI": "Commercial insurance", "BL": "Blue Cross/Blue Shield", "16": "Medicare Advantage HMO",
                    "12": "PPO", "13": "POS", "14": "EPO", "15": "Indemnity", "17": "Dental", "CH": "TRICARE"}
# Facility code (UB-04 type of bill digits 2-3): the care setting the claim states.
FACILITY_SERVICE = {"11": "Hospital Inpatient", "12": "Hospital Inpatient", "13": "Hospital Outpatient",
                    "14": "Hospital Outpatient", "18": "Hospital Inpatient", "21": "Other", "71": "Other", "83": "Hospital Outpatient"}
FACILITY_PROVIDER_TYPE = {"11": "hospital_inpatient", "12": "hospital_inpatient", "13": "hospital_outpatient"}

_AMT = r"(-?[\d,]*\d\.\d{2})"
_MONEY_TOK = re.compile(r"^-?[\d,]*\d\.\d{2}$")
_DATE_TOK = re.compile(r"^(\d{1,2})[/-](\d{1,2})[/-](\d{2}|\d{4})$")
_REASON_TOK = re.compile(r"^(CO|PR|OA|PI|CR)-?(\d{1,3})$")
_REMARK_TOK = re.compile(r"^(MA\d{2,3}|M\d{1,3}|N\d{1,4})$")
_BANK = re.compile(r"(?i)\b(DFI|account\s*number|routing)\b")


def _cents(s: str | None) -> int | None:
    s = str(s or "").replace(",", "").strip()
    try:
        return round(float(s) * 100) if s else None
    except ValueError:
        return None


def _iso(tok: str | None) -> str | None:
    m = _DATE_TOK.match(str(tok or "").strip())
    if not m:
        return None
    mo, d, y = int(m.group(1)), int(m.group(2)), int(m.group(3))
    y += 2000 if y < 100 else 0
    return f"{y:04d}-{mo:02d}-{d:02d}" if 1 <= mo <= 12 and 1 <= d <= 31 and 1990 <= y <= 2100 else None


def _flat(text: str) -> str:
    """Table pipes and markdown marks to plain separators, one cell per line (labels and values stay in order)."""
    out = []
    for ln in (text or "").splitlines():
        if re.fullmatch(r"\s*\|?[\s|:-]*\|?\s*", ln) and "-" in ln:
            continue  # a markdown table rule
        row = []
        for cell in ln.split("|"):
            cell = re.sub(r"^#+\s*", "", cell).strip()
            if cell and cell != "<!-- image -->" and cell not in row:
                row.append(cell)  # Docling repeats a merged cell's text in every column it spans
        out.extend(row)
    return "\n".join(out)


def _after(label: str, flat: str, value: str, span: int = 60) -> str | None:
    """The value written after a label, on its line or the next cell (a label's value never jumps past another
    label: a line ending in ':' stops the search)."""
    m = re.search(rf"(?is){label}\s*:?\s*(?:[^\n]{{0,{span}}}?)?{value}", flat)
    return m.group(1) if m else None


def reason_text(code: str, printed: dict | None = None) -> str:
    """'CO197' -> 'CO197: Precertification/authorization/notification absent'."""
    m = _REASON_TOK.match(code) or re.match(r"^()(\d{1,3})$", code)
    num = m.group(2) if m else code
    words = (printed or {}).get(num) or CARC.get(num) or RARC.get(code)
    return f"{code}: {words}" if words else code


def layout(text: str) -> str | None:
    t = (text or "").lower()
    if "claim charge amount" in t and ("rev code" in t or "service line" in t or "payee" in t):
        return "epic_835"
    if re.search(r"\bub-?04\b", t) and ("type of bill" in t or "rev. cd" in t or "rev cd" in t):
        return "ub04"
    if "claim number" in t and ("adjustment reason codes" in t or "benefit amount" in t) and "billed amount" in t:
        return "payer_eob"
    return None


# --------------------------------------------------------------------------- 835 printout

def _party(flat: str, heading: str) -> dict:
    """The name/address block under 'Payer Information' or 'Payee Information': the first line that is not a label."""
    m = re.search(rf"(?i){heading}\s*\n(.*?)(?:\n(?:Payer Contact|Payee Information|Payer Information|Reference Information|$))", flat + "\n", re.S)
    if not m:
        return {}
    block = m.group(1)
    for ln in block.splitlines():
        ln = ln.strip()
        if not ln or ln.endswith(":") or not re.search(r"[A-Za-z]{3}", ln) or _BANK.search(ln):
            continue
        if re.fullmatch(r"(?i)(name|address|payer qualifier|payee qualifier|payer id|payee id|xx|fi|pi)\s*:?", ln):
            continue
        nm = re.match(r"^(.*?)(?:\s+(?:P\.?\s*O\.?\s*BOX|\d{2,6}\s)\s*(.*))?$", ln)
        name = (nm.group(1) if nm else ln).strip(" ,")
        out = {"name": name}
        if nm and nm.group(2):
            out["address"] = ln[len(nm.group(1)):].strip(" ,")
        ident = re.search(r"(?i)(?:payee|payer)\s*id\s*:?\s*(?:\n\s*)*?(?:[A-Z]{2}\s*\n\s*)?(\d{5,10})\b", block)
        if ident:
            out["id"] = ident.group(1)
        else:
            ids = [i for i in re.findall(r"\b(\d{10})\b", block)]
            if ids:
                out["id"] = ids[-1]
        return out
    return {}


def _service_lines_835(flat: str) -> list[dict]:
    parts = re.split(r"(?im)^\s*(\d{1,3})\s*[-–—]\s*Rev\s*Code\s*(\d{3,4})\s*$", flat)
    lines = []
    for i in range(1, len(parts) - 2, 3):
        body = parts[i + 2]
        body = re.split(r"(?im)^(?:Provider Adjustment|Financial Information|Claim Information\s*$)", body)[0] \
            if i + 3 >= len(parts) else body
        toks = body.split()
        money = [t for t in toks if _MONEY_TOK.match(t)]
        reasons = sorted({f"{m.group(1)}{m.group(2)}" for t in toks for m in [_REASON_TOK.match(t)] if m})
        remarks = sorted({t for t in toks if _REMARK_TOK.match(t)})
        dates = [d for d in (_iso(t) for t in toks) if d]
        qty = None
        if len(money) >= 2:
            after_paid = toks[toks.index(money[1]) + 1:]
            q = next((t for t in after_paid if re.fullmatch(r"\d{1,4}", t)), None)
            qty = int(q) if q else None
        line = {"line": int(parts[i]), "rev_code": parts[i + 1].zfill(4)}
        if money:
            line["billed_cents"] = _cents(money[0])
        if len(money) > 1:
            line["paid_cents"] = _cents(money[1])
        if len(money) > 2:
            line["adjustment_cents"] = _cents(money[2])
        if qty is not None:
            line["units"] = qty
        if reasons:
            line["reason_codes"] = reasons
        if remarks:
            line["remark_codes"] = remarks
        if dates:
            line["service_date"] = dates[0]
        lines.append(line)
    return lines


_CLAIM_DETAIL_LABELS = re.compile(
    r"(?i)transaction\s+group\s+number|claim\s+status\s+code|claim\s+charge\s+amount|claim\s+paid\s+amount|"
    r"patient\s+responsibility\s+amount|claim\s+filing\s+indicator\s+code|payer\s+control\s+number|"
    r"facility\s+code\s+value|claim\s+frequency\s+type\s+code|diagnosis\s+related\s+group\s+(?:code|weight)|"
    r"discharge\s+fraction|claim\s+entity\s+information|entity\s+type|id\s+type|patient|insured\s+or\s+subscriber|"
    r"additional\s+claim\s+inf\w*|claim\s+details|claim\s+information|\(processed\s+as\s+\w+\)|\b(?:MI|MR)\b")


def _claim_details(flat: str) -> dict:
    """The Claim Details block's values by their shape, in Epic's print order. OCR prints the block three ways --
    label then value, every label then every value, or all of it in one cell -- and moves the DRG/frequency rows,
    so values are not paired with the label beside them: money is charge then paid then patient share; the
    two-letter code before a bracketed description is the filing indicator; the mixed letter-and-digit id is the
    payer control number; after it, two digits are the facility code, one digit (1-8) the frequency, three or four
    digits the DRG."""
    m = re.search(r"(?is)Claim\s+Details(.*?)(?:Service\s+Line\s+Summary|Service\s+Line\s+Information)", flat)
    block = m.group(1) if m else ""
    out: dict = {}
    ind = re.search(r"\b([A-Z]{2}|1[2-7])\s*\((?!processed)([A-Za-z][^)\n]{2,60})\)", block)
    if ind:
        out["filing_indicator"] = ind.group(1)
    rest = _CLAIM_DETAIL_LABELS.sub(" ", block.replace(ind.group(0), " ") if ind else block)
    toks = rest.replace(":", " ").split()
    money = [t for t in toks if _MONEY_TOK.match(t)]
    if money:
        out["charge"] = _cents(money[0])
    if len(money) > 1:
        out["paid"] = _cents(money[1])
    pcn_i = next((i for i, t in enumerate(toks) if re.fullmatch(r"(?=[A-Z0-9]*[A-Z])(?=[A-Z0-9]*\d)[A-Z0-9]{9,20}", t)), None)
    if pcn_i is not None:
        out["payer_control_number"] = toks[pcn_i]
        tail = [t for t in toks[pcn_i + 1:] if re.fullmatch(r"\d{1,4}", t)]
        fac = next((t for t in tail if len(t) == 2), None)
        freq = next((t for t in tail if len(t) == 1 and t in "12345678"), None)
        drg = next((t for t in tail if len(t) in (3, 4)), None)
        if fac:
            out["facility_code"] = fac
        if freq:
            out["frequency_code"] = freq
        if drg:
            out["drg"] = drg
    return out


def _epic_835(text: str) -> tuple[dict, list[str]]:
    flat = _flat(text)
    details = _claim_details(flat)
    # The Service Line Summary prints the claim's billed and paid totals under their own labels: the first two
    # amounts after it. Read before the Claim Details guess, which it agrees with on a clean page.
    summ = re.search(r"(?is)Service\s+Line\s+Summary(.*?)(?:Service\s+Line\s+Information|\d{1,3}\s*[-–—]\s*Rev\s*Code)", flat)
    summ_money = [t for t in (summ.group(1).split() if summ else []) if _MONEY_TOK.match(t)]
    charge = _cents(summ_money[0]) if summ_money else details.get("charge")
    paid = _cents(summ_money[1]) if len(summ_money) > 1 else details.get("paid")
    resp = None
    pcn, fac, drg = details.get("payer_control_number"), details.get("facility_code"), details.get("drg")
    freq_code = details.get("frequency_code")
    claim_no = _after(r"Claim (?:Number|#)", flat, r"\b(?=[A-Z0-9-]*\d)([A-Z0-9][A-Z0-9-]{5,30})\b", span=0)
    issue = re.search(r"(?is)Issue Date:.{0,120}?\b(\d{1,2}/\d{1,2}/\d{2,4})\b", flat)
    prod = _after(r"Production Date", flat, r"\b(\d{1,2}/\d{1,2}/\d{2,4})\b", span=0)
    trace = _after(r"Check/EFT Trace Number", flat, r"\b([A-Z0-9]{6,30})\b", span=0)
    lines = _service_lines_835(flat)
    payer, payee = _party(flat, r"Payer Information"), _party(flat, r"Payee Information")

    reasons = sorted({r for ln in lines for r in ln.get("reason_codes", [])} |
                     {f"{m.group(1)}{m.group(2)}" for t in (summ.group(1).split() if summ else []) for m in [_REASON_TOK.match(t)] if m})
    remarks = sorted({r for ln in lines for r in ln.get("remark_codes", [])})
    denied_on = _iso(issue.group(1)) if issue else _iso(prod)
    dates = sorted(ln["service_date"] for ln in lines if ln.get("service_date"))
    remit = {"layout": "epic_835", "claim_charge_cents": charge, "claim_paid_cents": paid,
             "patient_responsibility_cents": resp, "payer_control_number": pcn, "facility_code": fac,
             "frequency_code": freq_code, "drg": drg, "reason_codes": reasons,
             "remark_codes": remarks, "issue_date": denied_on, "trace_number": trace,
             "filing_indicator": details.get("filing_indicator"), "payer": payer, "payee": payee, "service_lines": lines}
    remit = {k: v for k, v in remit.items() if v not in (None, "", [], {})}
    issues = []
    if lines and charge is not None:
        total = sum(ln.get("billed_cents") or 0 for ln in lines)
        if total and total != charge and len(lines) == max(ln["line"] for ln in lines):
            issues.append(f"The remittance's service lines bill ${total / 100:,.2f} but its claim charge is ${charge / 100:,.2f}")
    case: dict = {}
    if payer.get("name"):
        case["health_plan"] = payer["name"]
    if payee.get("name"):
        case["provider_org"] = payee["name"]
    if payee.get("id") and valid_npi(payee["id"]):
        case["provider_npi"] = payee["id"]
    lob = FILING_INDICATOR.get(remit.get("filing_indicator", ""))
    if lob in ("Medicaid",) or (lob or "").startswith("Medicare"):
        case["line_of_business"] = "Medicaid" if lob == "Medicaid" else ("Medicare Advantage" if "Advantage" in lob else lob)
    if fac in FACILITY_PROVIDER_TYPE:
        case["provider_type"] = FACILITY_PROVIDER_TYPE[fac]
    if charge and paid == 0:
        case["disputed_issue"] = "Denial"
    elif charge and paid is not None and 0 < paid < charge:
        case["disputed_issue"] = "Underpayment"
    claim = {"claim_number": claim_no or pcn}
    if charge is not None:
        claim["billed_cents"] = charge
    if paid is not None:
        claim["paid_cents"] = paid
    if charge is not None and paid is not None:
        claim["disputed_cents"] = charge - paid
    if dates:
        claim["date_of_service"] = dates[0]
    if denied_on and paid is not None and charge and paid < charge:
        claim["date_of_denial"] = denied_on
    if reasons or remarks:
        claim["denial_reason"] = "; ".join(reason_text(r) for r in reasons + remarks)[:500]
    if FACILITY_SERVICE.get(fac or ""):
        claim["type_of_service"] = FACILITY_SERVICE[fac]
    if payee.get("name"):
        claim["provider_name"] = payee["name"]
    remit["case"] = case
    remit["claims"] = [claim] if claim["claim_number"] else []
    return remit, issues


# --------------------------------------------------------------------------- payer EOB (Molina layout)

def _payer_eob(text: str) -> tuple[dict, list[str]]:
    flat = _flat(text)
    claim_no = _after(r"Claim Number", flat, r"\b([A-Z0-9][A-Z0-9-]{5,30})\b", span=0)
    claim_date = _after(r"Claim Date", flat, r"\b(\d{1,2}-\d{1,2}-\d{4}|\d{1,2}/\d{1,2}/\d{4})\b", span=0)
    fac = _after(r"Facility Type Code", flat, r"\b(\d{2})\b", span=0)
    billed = _cents(_after(r"Billed Amount:", flat, _AMT, span=0))
    benefit = re.search(r"(?is)Total Benefit.{0,80}?Amount:\s*" + _AMT, flat)
    benefit = _cents(benefit.group(1)) if benefit else None
    legend = {k: v.strip().rstrip(".") for k, v in re.findall(r"(?m)^\s*(\d{1,3}):\s*([^\n]{5,160})$", flat)}
    pay = re.search(r"(?is)Payment Information.*?Amount:\s*" + _AMT + r".*?Date:\s*(\d{1,2}[-/]\d{1,2}[-/]\d{4})", flat)
    payee = re.search(r"(?is)Payment Made To:\s*\n?([^\n]+)", flat)
    payee_name = re.split(r"\s+(?:P\.?\s*O\.?\s*BOX|\d{2,6}\s)", payee.group(1).strip())[0].strip(" ,") if payee else None
    # The payer's letterhead: the first all-capitals line naming a plan, before "Payment Made To".
    head = (text or "").split("Payment Made To")[0]
    payer = next((ln for ln in (re.sub(r"^#+\s*", "", x).strip() for x in head.splitlines())
                  if re.fullmatch(r"[A-Z][A-Z0-9 .,&'-]{4,80}", ln) and re.search(r"HEALTH|PLAN|INSURANCE|CARE|MUTUAL", ln)
                  and not re.search(r"EXPLANATION|PAYMENT|ADJUSTMENT|REASON", ln)), None)
    # Service lines: "from to billed allowed" -- Docling repeats a row's text across its merged cells, so a row
    # counts once (its first cell); the rows' sum is checked against the printed billed total.
    rows = []
    for ln in (text or "").splitlines():
        cells = [c.strip() for c in ln.split("|") if c.strip()]
        if not cells:
            continue
        # A table row can hold two printed lines in different cells (Docling merges them): every distinct line
        # in the row counts once.
        found = []
        for cell in cells:
            for m in re.finditer(r"(\d{2}-\d{2}-\d{4})\s*(\d{2}-\d{2}-\d{4})\s+" + _AMT + r"\s+" + _AMT, cell):
                if m.groups() not in found:
                    found.append(m.groups())
        for g in found:
            rows.append({"from": _iso(g[0]), "through": _iso(g[1]), "billed_cents": _cents(g[2]), "allowed_cents": _cents(g[3])})
    codes = sorted(set(legend))
    remit = {"layout": "payer_eob", "claim_number": claim_no, "claim_date": _iso(claim_date), "facility_code": fac,
             "billed_cents": billed, "benefit_cents": benefit, "reason_codes": codes,
             "reason_legend": legend, "payment_cents": _cents(pay.group(1)) if pay else None,
             "payment_date": _iso(pay.group(2)) if pay else None, "payer": {"name": payer} if payer else {},
             "payee": {"name": payee_name} if payee_name else {}, "service_lines": rows}
    remit = {k: v for k, v in remit.items() if v not in (None, "", [], {})}
    issues = []
    total = sum(r["billed_cents"] or 0 for r in rows)
    if rows and billed and total != billed:
        issues.append(f"The EOB's {len(rows)} service lines read add up to ${total / 100:,.2f}, not the "
                      f"${billed / 100:,.2f} billed it prints -- a line may be misread or missing")
    case: dict = {}
    if payer:
        case["health_plan"] = payer
    if payee_name:
        case["provider_org"] = payee_name
    if fac in FACILITY_PROVIDER_TYPE:
        case["provider_type"] = FACILITY_PROVIDER_TYPE[fac]
    if billed and benefit == 0:
        case["disputed_issue"] = "Denial"
    elif billed and benefit and benefit < billed:
        case["disputed_issue"] = "Underpayment"
    claim = {"claim_number": claim_no} if claim_no else {}
    if claim:
        if billed is not None:
            claim["billed_cents"] = billed
        if benefit is not None:
            claim["paid_cents"] = benefit
        if billed is not None and benefit is not None:
            claim["disputed_cents"] = billed - benefit
        froms = sorted(r["from"] for r in rows if r.get("from"))
        if froms:
            claim["date_of_service"] = froms[0]
        elif claim_date:
            claim["date_of_service"] = _iso(claim_date)
        if remit.get("payment_date") and benefit is not None and billed and benefit < billed:
            claim["date_of_denial"] = remit["payment_date"]
        if codes:
            claim["denial_reason"] = "; ".join(reason_text(c, legend) for c in codes)[:500]
        if FACILITY_SERVICE.get(fac or ""):
            claim["type_of_service"] = FACILITY_SERVICE[fac]
        if payee_name:
            claim["provider_name"] = payee_name
    remit["case"] = case
    remit["claims"] = [claim] if claim else []
    return remit, issues


# --------------------------------------------------------------------------- UB-04

def _ub04(text: str) -> tuple[dict, list[str]]:
    flat = _flat(text)
    lines = [ln.strip() for ln in flat.splitlines() if ln.strip()]
    tob = re.search(r"(?is)TYPE\s+OF\s+BILL\s*\n?\s*(0?\d{3})\b", flat)
    npi = re.search(r"(?i)\b56\s*NPI\s*:?\s*(\d{10})\b", flat)
    att = re.search(r"(?is)76\s*ATTENDING\s*\n?\s*NPI\s*:?\s*(\d{10})\b", flat)
    pcn = re.search(r"(?i)\b3a\s*PAT\.?\s*(?:CNTL\s*#?)?\s*(\d{6,24})\b", flat)
    mrn = re.search(r"(?i)MED\.?\s*REC\.?\s*#?\s*([A-Z0-9]{4,24})\b", flat)
    plan_id = re.search(r"(?is)51\s*HEALTH\s+PLAN\s+ID\s*\n(?:[^\n]*\n){0,2}?[^\n]*?[A-Z](\d{5,15})\s*$", flat, re.M)
    payer = re.search(r"(?m)^38\s*([A-Z][A-Z .,&'-]{3,60}?)\s+(?:P\.?\s*O\.?\s*BOX|\d)", flat) or \
        re.search(r"(?is)50\s*PAYER\s+NAME\s*\n(?:[^\n]*\n){0,2}?([A-Z][A-Z .,&'-]{3,60}?)\d", flat)
    pps = re.search(r"(?is)71\s*PPS(?:\s*\n[^\n]{0,30}){0,3}?\s*\n\s*(\d{3})\s*$", flat, re.M)
    tob_code = tob.group(1).zfill(4) if tob else None
    revs = sorted({ln for ln in lines if re.fullmatch(r"0\d{3}", ln) and ln not in ("0001", tob_code)})
    # FL 47 totals: the charges column total, printed after TOTALS without its decimal point ("72446961"). The
    # largest of the totals row's amounts is the total charges (the others are units and non-covered amounts).
    total = None
    m = re.search(r"(?is)TOTALS\s*\n(.*?)\n\s*50\s*PAYER", flat)
    if m:
        nums = [int(x) for x in re.findall(r"(?m)^\s*(\d{3,9})\s*$", m.group(1))]
        if nums:
            total = max(nums)
    # FL 1 (billing provider) comes before the payer (FL 38) and the service lines (FL 42); OCR puts the form's box
    # numbers first and interleaves FL 3a/FL 2, so the block is everything up to the payer, not a line count.
    head = lines[:next((i for i, ln in enumerate(lines) if re.match(r"(?i)^(38\s*[A-Z]|42\s*REV|50\s*PAYER)", ln)), 80)]
    provider = next((ln for ln in head if re.fullmatch(r"[A-Z][A-Z .,&'-]{5,60}", ln)
                     and not re.search(r"(?i)\bUB-?04\b|PATIENT|CLAIM|CODE|DATE", ln)), None)
    street = next((ln for ln in head if re.fullmatch(r"\d{1,6}\s+[A-Z0-9 .#'-]{3,60}", ln)), None)
    csz = next((m2 for ln in head for m2 in [re.match(r"^([A-Z][A-Z .'-]+?)\s+([A-Z]{2})\s+(\d{5})(\d{4})?", ln)] if m2), None)
    ub = {"layout": "ub04", "type_of_bill": tob_code,
          "billing_npi": npi.group(1) if npi and valid_npi(npi.group(1)) else None,
          "attending_npi": att.group(1) if att and valid_npi(att.group(1)) else None,
          "patient_control_number": pcn.group(1) if pcn else None, "medical_record_number": mrn.group(1) if mrn else None,
          "payer": {"name": payer.group(1).strip()} if payer else {}, "health_plan_id": plan_id.group(1) if plan_id else None,
          "total_charges_cents": total, "revenue_codes": revs, "pps_code": pps.group(1) if pps else None,
          "provider": {k: v for k, v in {"name": provider, "street": street,
                                         "city": csz.group(1) if csz else None, "state": csz.group(2) if csz else None,
                                         "postal": (csz.group(3) + ("-" + csz.group(4) if csz.group(4) else "")) if csz else None}.items() if v}}
    ub = {k: v for k, v in ub.items() if v not in (None, "", [], {})}
    case: dict = {}
    prov = ub.get("provider", {})
    if prov.get("name"):
        case["provider_org"] = prov["name"]
    for k in ("street", "city", "state", "postal"):
        if prov.get(k):
            case[f"provider_{k}"] = prov[k]
    if ub.get("billing_npi"):
        case["provider_npi"] = ub["billing_npi"]
    if ub.get("payer", {}).get("name"):
        case["health_plan"] = ub["payer"]["name"]
        if "medicaid" in ub["payer"]["name"].lower():
            case["line_of_business"] = "Medicaid"
    fac = (ub.get("type_of_bill") or "")[1:3]
    if fac in FACILITY_PROVIDER_TYPE:
        case["provider_type"] = FACILITY_PROVIDER_TYPE[fac]
    # A UB-04 prints no payer claim number: its row joins the claims table by its billed total (fill_from_claim_docs
    # matches it to the one claim with that amount), never as a claim of its own.
    claim = {}
    if total:
        claim = {"match_billed_cents": total, "billed_cents": total}
        if FACILITY_SERVICE.get(fac):
            claim["type_of_service"] = FACILITY_SERVICE[fac]
        if prov.get("name"):
            claim["facility_name"] = prov["name"]
    ub["case"] = case
    ub["claims"] = [claim] if claim else []
    return ub, []


def text_layer(raw: bytes | None) -> str:
    """A born-digital PDF's own text, line by line, or "" for a scan / not a PDF. Read before Docling's table view
    because that view can merge two printed lines into one row (two equal amounts then count once)."""
    if not raw or not raw.lstrip()[:5].startswith(b"%PDF"):
        return ""
    try:
        from pypdf import PdfReader
        text = "\n".join(p.extract_text() or "" for p in PdfReader(__import__("io").BytesIO(raw)).pages[:30])
    except Exception:  # noqa: BLE001 -- the OCR text is read instead
        return ""
    return text if len(text.strip()) >= 200 else ""


def extract(doc_type: str, text: str, raw: bytes | None = None) -> tuple[str | None, dict, list[str]]:
    """(key, facts, issues) for an EOB/remittance or a claim form whose layout is recognised; (None, {}, []) else.
    The PDF's own text layer is used when it shows the same layout; Docling/OCR text otherwise."""
    if doc_type not in ("eob", "idr_claim", "unrelated", ""):
        return None, {}, []
    layer = text_layer(raw)
    if layer and layout(layer):
        text = layer
    kind = layout(text)
    if kind == "epic_835":
        return ("remit", *_epic_835(text))
    if kind == "payer_eob":
        return ("remit", *_payer_eob(text))
    if kind == "ub04":
        return ("ub04", *_ub04(text))
    return None, {}, []
