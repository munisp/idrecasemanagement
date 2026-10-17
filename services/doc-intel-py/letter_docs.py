"""The office's own letters, recognised in uploads: the decision report (Review Rationale and Letter), the cover
letter to the Agency, the review-cost invoice and the external review (IRO) decision.

The classifier's TYPE_SIGNALS (pipeline.py) names the types; this module reads what each one says, by regex over
the text -- the case and claim numbers, the outcome, the amounts awarded / already paid / owed, the percentage, the
invoice number, the party billed, its share and the late-charge totals -- and checks the arithmetic the letter
itself states (award less paid is the balance; the late totals add the late charge). Bank numbers on an invoice are
never extracted.
"""
from __future__ import annotations

import re

LETTER_TYPES = ("fl_decision_report", "fl_cover_letter", "fl_review_invoice", "iro_decision")

TYPE_SIGNALS = {
    "fl_decision_report": {
        "strong": ["recommended determination", "coding consultant review", "independent physician review",
                   "claim dispute resolution program", "is owed to the provider", "be awarded to the health plan",
                   "both parties participated in full"],
        "weak": ["rationale", "claim summary", "standard of review", "overturned", "upheld", "based on the foregoing",
                 "applicable law"],
    },
    "fl_cover_letter": {
        "strong": ["case file summary", "enclosed with this letter is the analysis",
                   "independently review provider and managed care organization claim disputes",
                   "professional appeals staff", "amount in controversy"],
        "weak": ["external review of denied claims", "decision:", "sincerely", "initial review of this case"],
    },
    "fl_review_invoice": {
        "strong": ["remit to", "review costs", "total if paid after", "late charge will be added", "date prepared",
                   "remittance advice", "responsible for"],
        "weak": ["invoice", "ach payment", "bill to", "routing no", "account no", "external review of denied claims"],
    },
    "iro_decision": {
        "strong": ["iro number", "decision summary", "evidence supporting the determination",
                   "final and binding on the insurance carrier", "plan reference no"],
        "weak": ["experimental", "investigational", "medical necessity", "date received", "decision date",
                 "insurance carrier"],
    },
}

_AMT = r"\$\s*([\d,]+\.\d{2})"
_CASE_RE = re.compile(r"(?i)\b(?:case\s*(?:no\.?|number)|case\s*#)\s*:?\s*([A-Z]{2}\d{2}-\d{2,7})\b")
_CASE_ANY_RE = re.compile(r"\b([A-Z]{2}\d{2}-\d{2,7})\b")
_CLAIM_RE = re.compile(r"(?i)\bclaim\s*no\.?\s*:?\s*([A-Z0-9][-A-Z0-9]{0,30})")
_AWARD_RE = re.compile(r"(?is)the\s+amount\s+of\s*" + _AMT + r"\s*be\s+awarded\s+to\s+the\s+(provider|health\s+plan)")
_PAID_RE = re.compile(r"(?is)of\s+which\s*" + _AMT + r"\s*was\s+already\s+paid")
_BAL_RE = re.compile(r"(?is)balance\s+of\s*" + _AMT + r"\s*is\s+owed")
_PCT_AWARD_RE = re.compile(r"(?is)or\s+(\d{1,3}(?:\.\d+)?)\s*%\s+of\s+the\s+disputed")
_CONTROVERSY_RE = re.compile(r"(?is)amount\s+in\s+controversy\s+is\s*" + _AMT)
_INV_NO_H_RE = re.compile(r"\b([A-Z]{2}\d{2}-\d{2,7}H)\b")  # a health plan invoice: the number with H, anywhere
_INV_NO_RE = re.compile(r"(?is)invoice\s*#.{0,400}?\b([A-Z]{2}\d{2}-\d{2,7}H?)\b")
_RESP_RE = re.compile(r"(?is)\b(health\s+plan|provider)\s+is\s+responsible\s+for\s+(\d{1,3})\s*%")
_RULE_RE = re.compile(r"59A-12\.030\s*\(4\)\s*\(([ab])\)")
_COST_RE = re.compile(r"(?is)review\s+costs?\s*:\s*" + _AMT)
_TOTAL_RE = re.compile(r"(?is)\btotal\s*:\s*" + _AMT)
_LATE_RE = re.compile(r"(?is)total\s+if\s+paid\s+after\s+(\d+)\s+days\s*:\s*" + _AMT)
_LATE_FEE_RE = re.compile(r"(?is)a\s*\$\s*(\d+(?:\.\d{2})?)\s*late\s+charge")
_DATE_RE = re.compile(r"\b(\d{2}/\d{2}/\d{4})\b")
_NOT_PREPARED = re.compile(r"(?i)changed\s+on\s*$")  # "BANK ACCOUNT INFORMATION CHANGED ON 11/18/2025" is not it
_IRO_RE = re.compile(r"(?i)\bIRO\s+Number\s*:?\s*([A-Z0-9-]{2,20})")
_LONG_DATE = r"([A-Z][a-z]+\s+\d{1,2},\s+\d{4})"
_RECEIVED_RE = re.compile(r"(?i)date\s+received\s*:\s*" + _LONG_DATE)
_DECIDED_RE = re.compile(r"(?i)decision\s+date\s*:\s*" + _LONG_DATE)


def _cents(s: str) -> int:
    return int(round(float(s.replace(",", "")) * 100))


def _outcome(text: str) -> str | None:
    t = text.lower()
    if "partially overturned" in t:
        return "PARTIALLY_OVERTURNED"
    if re.search(r"\bbe\s+overturned\b|\bhave\s+been\s+overturned\b", t):
        return "OVERTURNED"
    if re.search(r"\bbe\s+upheld\b", t):
        return "UPHELD"
    return None


def case_seq(case_number: str | None) -> tuple[str, int] | None:
    """FL26-216 and FL26-000216 are the same case: compare by prefix and sequence number."""
    m = re.fullmatch(r"([A-Z]{2}\d{2})-(\d+)H?", (case_number or "").strip().upper())
    return (m.group(1), int(m.group(2))) if m else None


def extract(doc_type: str, text: str) -> tuple[dict, list[str]]:
    """(fields, issues) for one of LETTER_TYPES; ({}, []) for any other type."""
    if doc_type not in LETTER_TYPES:
        return {}, []
    t = text or ""
    out: dict = {"letter_type": doc_type}
    issues: list[str] = []
    m = _CASE_RE.search(t) or (_CASE_ANY_RE.search(t) if doc_type != "iro_decision" else None)
    if m:
        out["case_number"] = m.group(1)

    if doc_type in ("fl_decision_report", "fl_cover_letter", "iro_decision"):
        o = _outcome(t)
        if o:
            out["outcome"] = o
        m = _CLAIM_RE.search(t)
        if m and m.group(1).upper() not in ("NO", "DATE"):
            out["claim_number"] = m.group(1)
        m = _AWARD_RE.search(t)
        if m:
            out["awarded_cents"] = _cents(m.group(1))
            out["awarded_to"] = "HEALTH_PLAN" if "plan" in m.group(2).lower() else "PROVIDER"
        m = _PAID_RE.search(t)
        if m:
            out["already_paid_cents"] = _cents(m.group(1))
        m = _BAL_RE.search(t)
        if m:
            out["balance_cents"] = _cents(m.group(1))
        m = _PCT_AWARD_RE.search(t)
        if m:
            out["percent_awarded"] = float(m.group(1))
        m = _CONTROVERSY_RE.search(t)
        if m:
            out["amount_in_controversy_cents"] = _cents(m.group(1))
        if {"awarded_cents", "already_paid_cents", "balance_cents"} <= out.keys() and \
                out["awarded_cents"] - out["already_paid_cents"] != out["balance_cents"]:
            issues.append(f"The letter's amounts do not add up: awarded ${out['awarded_cents'] / 100:,.2f} less already "
                          f"paid ${out['already_paid_cents'] / 100:,.2f} is not the balance owed "
                          f"${out['balance_cents'] / 100:,.2f} — check the letter")
        if out.get("outcome") == "UPHELD" and out.get("awarded_to") == "PROVIDER":
            issues.append("The letter says the denial is upheld but awards an amount to the provider — check the letter")

    if doc_type == "iro_decision":
        for key, rx in (("iro_number", _IRO_RE), ("date_received", _RECEIVED_RE), ("decision_date", _DECIDED_RE)):
            m = rx.search(t)
            if m:
                out[key] = m.group(1)

    if doc_type == "fl_review_invoice":
        m = _INV_NO_H_RE.search(t) or _INV_NO_RE.search(t)
        if m:
            out["invoice_number"] = m.group(1)
            out.setdefault("case_number", m.group(1).rstrip("H"))
        m = _RESP_RE.search(t)
        if m:
            out["party"] = "HEALTH_PLAN" if "plan" in m.group(1).lower() else "PROVIDER"
            out["responsible_pct"] = int(m.group(2))
        m = _RULE_RE.search(t)
        if m:
            out["admin_code_rule"] = f"R 59A-12.030(4)({m.group(1)})"
            if out.get("responsible_pct") is not None:
                if m.group(1) == "a" and out["responsible_pct"] != 100:
                    issues.append("The invoice cites R 59A-12.030(4)(a) (one party pays all) but bills "
                                  f"{out['responsible_pct']}% — check the invoice")
                if m.group(1) == "b" and out["responsible_pct"] == 100:
                    issues.append("The invoice cites R 59A-12.030(4)(b) (a split) but bills 100% — check the invoice")
        m = _COST_RE.search(t)
        if m:
            out["review_cost_cents"] = _cents(m.group(1))
        m = _TOTAL_RE.search(t)
        if m:
            out["total_cents"] = _cents(m.group(1))
        start = t.lower().find("prepared")
        for m in _DATE_RE.finditer(t, max(start, 0)):
            if not _NOT_PREPARED.search(t[max(0, m.start() - 40):m.start()]):
                out["date_prepared"] = m.group(1)
                break
        lates = [(int(d), _cents(a)) for d, a in _LATE_RE.findall(t)]
        if lates:
            out["late_totals"] = {str(d): c for d, c in lates}
        m = _LATE_FEE_RE.search(t)
        fee = _cents(m.group(1)) if m else None
        if fee and out.get("total_cents") is not None and lates:
            for i, (days, amt) in enumerate(sorted(lates)):
                want = out["total_cents"] + fee * (i + 1)
                if amt != want:
                    issues.append(f"The total if paid after {days} days reads ${amt / 100:,.2f}; the total plus "
                                  f"{i + 1} late charge(s) is ${want / 100:,.2f} — check the invoice")
        if out.get("party") == "HEALTH_PLAN" and out.get("invoice_number") and not out["invoice_number"].endswith("H"):
            issues.append("A health plan invoice number normally ends in H — check the invoice number")
    return out, issues
