"""eobshape.py -- reading a billing document's fields whatever shape they
arrive in.

Two things vary from one EOB to the next, and neither is under our control:

  * how the payer lays the page out (a totals box, a table with one row per
    service, a remittance with no totals at all), and
  * how the extraction model answers (the flat field list it was asked for;
    the same fields under a wrapper key; "billedAmount" for
    "billed_amount_usd"; a provider as {"name": ..., "npi": ...}; service
    lines as rows, as strings, or as one string).

Code downstream used to assume one shape and broke on the others -- an EOB
whose service lines came back as rows crashed the provider-type step, and
nothing from that EOB reached the case. Everything here takes ANY value and
returns either a usable one or None. Nothing in this module raises on odd
input, and nothing imports the heavy pipeline, so it is tested on its own.

The rule that has not changed: a value is taken from what the document
says. Where the model's answer lacks a field, the text itself is searched
for a labelled value; a field with no labelled value stays empty.
"""
from __future__ import annotations

import re
from typing import Any

EMPTY = (None, "", [], {})


def _empty(v: Any) -> bool:
    return v is None or (isinstance(v, (str, list, dict, tuple)) and len(v) == 0) \
        or (isinstance(v, str) and not v.strip())


# ---------------------------------------------------------------------------
# Coercion: any value -> text / money / rows
# ---------------------------------------------------------------------------

_NAME_KEYS = ("name", "value", "text", "description", "label", "status", "code")


def as_text(v: Any) -> str:
    """Any value as one string. A dict gives its naming value ("name",
    "value", ...) when it has one, otherwise all its values; a list is joined.
    Never raises."""
    if v is None or isinstance(v, bool):
        return "" if v is None else ("true" if v else "false")
    if isinstance(v, str):
        return v.strip()
    if isinstance(v, (int, float)):
        return str(v)
    if isinstance(v, dict):
        for k in _NAME_KEYS:
            for key, val in v.items():
                if canon_key(key) == k and not _empty(val) and not isinstance(val, (dict, list)):
                    return as_text(val)
        return " ".join(t for t in (as_text(x) for x in v.values()) if t)
    if isinstance(v, (list, tuple, set)):
        return " ".join(t for t in (as_text(x) for x in v) if t)
    return str(v).strip()


_RE_MONEY = re.compile(r"\(?-?\s*\$?\s*-?\d[\d,]*(?:\.\d+)?\s*\)?")
_AMOUNT_KEYS = ("amount", "value", "total", "usd", "amount_usd")


def as_money(v: Any) -> float | None:
    """A dollar amount from a number, "$1,850.00", "1850 USD", "(82.40)",
    {"amount": 1850} or a one-item list. None when the value does not hold
    exactly one amount -- "330.00 of 1,450.00" is two, and is not guessed."""
    if v is None or isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        return round(float(v), 2) if v == v and abs(v) != float("inf") else None
    if isinstance(v, dict):
        for k in _AMOUNT_KEYS:
            for key, val in v.items():
                if canon_key(key) == k:
                    m = as_money(val)
                    if m is not None:
                        return m
        found = [m for m in (as_money(x) for x in v.values()) if m is not None]
        return found[0] if len(found) == 1 else None
    if isinstance(v, (list, tuple)):
        found = [m for m in (as_money(x) for x in v) if m is not None]
        return found[0] if len(found) == 1 else None
    s = str(v).strip()
    if not s:
        return None
    body = re.sub(r"(?i)\b(usd|us\s*dollars?|dollars?)\b", " ", s)
    hits = [h for h in _RE_MONEY.findall(body) if re.search(r"\d", h)]
    if len(hits) != 1:
        return None
    # Anything left besides the amount means this is prose, not a figure.
    if re.sub(r"[\s$:=.,]", "", body.replace(hits[0], "", 1)):
        return None
    h = hits[0]
    neg = "(" in h or "-" in h
    try:
        n = float(re.sub(r"[^\d.]", "", h))
    except ValueError:
        return None
    return round(-n if neg else n, 2)


def as_lines(v: Any) -> list:
    """Service lines as a list of rows (dicts) or strings, from a list, one
    row, a dict of columns ({"service": [...], "billed": [...]}) or one
    string with a line per service."""
    if _empty(v):
        return []
    if isinstance(v, str):
        return [p.strip() for p in re.split(r"[\n;]+", v) if p.strip()]
    if isinstance(v, dict):
        cols = [x for x in v.values() if isinstance(x, list)]
        if cols and len(cols) == len(v) and len({len(c) for c in cols}) == 1:
            keys = list(v.keys())
            return [{k: v[k][i] for k in keys} for i in range(len(cols[0]))]
        # {"1": {...}, "2": {...}} -- rows keyed by number
        if all(isinstance(x, dict) for x in v.values()):
            return list(v.values())
        return [v]
    if isinstance(v, (list, tuple)):
        out = []
        for x in v:
            if _empty(x):
                continue
            out.extend(x if isinstance(x, (list, tuple)) and x and isinstance(x[0], dict) else [x])
        return out
    return [str(v)]


_DESC_KEYS = ("service", "description", "service_description", "procedure", "procedure_description",
              "desc", "item", "name", "service_name", "cpt", "cpt_code", "code", "procedure_code",
              "hcpcs", "cpt_hcpcs", "cpt_hcpcs_code", "revenue_code")


def line_text(row: Any) -> str:
    """What one service line describes: the description and any procedure
    code. For a row, only its describing cells -- never the column names and
    never the amounts (a row's "patient_responsibility" key is not text
    about an emergency room)."""
    if isinstance(row, dict):
        parts = [as_text(val) for key, val in row.items()
                 if canon_key(key) in _DESC_KEYS and not _empty(val)]
        if parts:
            return " ".join(parts)
        # No recognised describing column: every cell that is words, not a number.
        return " ".join(t for t in (as_text(x) for x in row.values())
                        if t and as_money(t) is None and not re.fullmatch(r"[\d/\-.]+", t))
    return as_text(row)


def lines_text(v: Any) -> str:
    return " ".join(t for t in (line_text(r) for r in as_lines(v)) if t)


_BILLED_KEYS = ("billed", "billed_amount", "billed_amount_usd", "charge", "charges", "charge_amount",
                "billed_charges", "amount_billed", "submitted", "submitted_amount", "submitted_charges",
                "total_charge", "total_charges")


_ALLOWED_KEYS = ("allowed", "allowed_amount", "allowed_amount_usd", "amount_allowed", "plan_allowed",
                 "eligible_amount", "allowed_charges", "approved_amount")
_PATIENT_KEYS = ("patient_responsibility", "patient_responsibility_usd", "patient_resp", "pt_resp",
                 "member_responsibility", "patient_owes", "you_owe", "patient_liability", "patient_share")
_PAID_KEYS = ("plan_paid", "plan_paid_usd", "paid", "paid_amount", "amount_paid", "plan_payment", "payment", "net_paid")
# claim-level field -> the names its per-line figure goes by in a service-line row
LINE_KEYS = {"billed_amount_usd": _BILLED_KEYS, "allowed_amount_usd": _ALLOWED_KEYS,
             "patient_responsibility_usd": _PATIENT_KEYS}


def line_amounts(v: Any, keys: tuple) -> list[float] | None:
    """One figure from every service line, or None unless EVERY line has it:
    a list with a gap cannot be summed to a total."""
    rows = as_lines(v)
    if not rows:
        return None
    out = []
    for row in rows:
        if not isinstance(row, dict):
            return None
        amt = next((as_money(val) for key, val in row.items()
                    if canon_key(key) in keys and as_money(val) is not None), None)
        if amt is None:
            return None
        out.append(amt)
    return out


def column_total(vals: list[float]) -> float | None:
    """The total of a column of amounts. When one row equals the sum of all
    the others it IS the total -- a Totals row whose label the scan lost --
    and adding it in would double the claim (seen on a faint fax: four rows,
    the fourth the total of the other three)."""
    if not vals:
        return None
    if len(vals) >= 3:
        for i, v in enumerate(vals):
            rest = vals[:i] + vals[i + 1:]
            if v > 0 and abs(v - sum(rest)) < 0.01:
                return round(v, 2)
    return round(sum(vals), 2)


def lines_billed_total(v: Any) -> float | None:
    """The sum of the lines' billed amounts -- only when EVERY line has one.
    A partial sum would be a wrong total, so it is not returned."""
    amts = line_amounts(v, _BILLED_KEYS)
    total = column_total(amts) if amts else 0
    return total if total and total > 0 else None


def _on_page(amount: float, text: str) -> bool:
    """Whether the page prints this amount (1,850.00 / 1850.00 / 1850)."""
    hay = re.sub(r"[,$]", "", str(text or ""))   # keep the spaces: they are what separates one amount from the next
    forms = {f"{amount:.2f}"}
    if float(amount).is_integer():
        forms.add(str(int(amount)))
    return any(re.search(rf"(?<![\d.]){re.escape(f)}(?!\d)", hay) for f in forms)


def reconcile_totals(extracted: dict, text: str) -> tuple[dict, list[dict]]:
    """Check each claim total against its own service lines.

    A total and the lines it sums are printed on the same page, so they can
    be checked against each other -- and a faint page made that worth doing:
    the first line's allowed amount was taken for the claim's, and because
    that figure IS on the page it passed the "is it in the document" check.

    For each of billed / allowed / patient responsibility where every line
    carries the figure and there are at least two lines:
      * total equals the lines' sum          -> nothing to say
      * total is one line's own figure, and the page prints the sum
                                             -> the total is corrected to the sum
      * anything else that disagrees         -> left alone and reported
    A correction is only ever to a figure the page itself prints. Returns
    (extracted, notes); each note is {field, was, now|None, lines_sum}.
    Never raises.
    """
    out = dict(extracted) if isinstance(extracted, dict) else {}
    notes: list[dict] = []
    try:
        lines = out.get("service_lines")
        for field, keys in LINE_KEYS.items():
            total = as_money(out.get(field))
            amts = line_amounts(lines, keys)
            if total is None or not amts or len(amts) < 2:
                continue
            line_sum = round(sum(amts), 2)
            if abs(line_sum - total) < 0.005:
                continue
            note = {"field": field, "was": total, "now": None, "lines_sum": line_sum}
            if any(abs(a - total) < 0.005 for a in amts) and _on_page(line_sum, text):
                out[field] = line_sum
                note["now"] = line_sum
            notes.append(note)
        # Within one line: what the plan allowed is what it paid plus what the
        # patient owes. A line that does not add up has a misread figure in it
        # (a faint "6" read as "8"). Reported, never corrected: nothing says
        # which of the three is the wrong one.
        for i, row in enumerate(as_lines(lines), start=1):
            if not isinstance(row, dict):
                continue
            pick = lambda keys: next((as_money(val) for k, val in row.items()  # noqa: E731
                                      if canon_key(k) in keys and as_money(val) is not None), None)
            allowed, patient, paid = pick(_ALLOWED_KEYS), pick(_PATIENT_KEYS), pick(_PAID_KEYS)
            if None in (allowed, patient, paid) or abs(allowed - patient - paid) < 0.005:
                continue
            notes.append({"field": "service_lines", "line": i, "what": line_text(row),
                          "allowed": allowed, "patient": patient, "paid": paid})
    except Exception:  # noqa: BLE001 -- a cross-check must never cost the extraction
        return (dict(extracted) if isinstance(extracted, dict) else {}), []
    return out, notes


# ---------------------------------------------------------------------------
# Shaping the model's answer onto the schema
# ---------------------------------------------------------------------------

def canon_key(k: Any) -> str:
    """A key in one spelling: billedAmount, "Billed Amount", BILLED-AMOUNT
    and billed_amount are the same key."""
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", str(k))
    return re.sub(r"[^a-z0-9]+", "_", s.lower()).strip("_")


# canonical field -> other names the same thing goes by (already canon_key'd)
ALIASES: dict[str, tuple[str, ...]] = {
    "billed_amount_usd": ("billed_amount", "billed", "total_billed", "total_billed_amount", "total_billed_usd",
                          "total_charges", "total_charge", "charges", "charge_amount", "billed_charges",
                          "amount_billed", "submitted_amount", "submitted_charges", "total_submitted_charges",
                          "provider_billed", "provider_charges"),
    "allowed_amount_usd": ("allowed_amount", "allowed", "total_allowed", "total_allowed_amount", "amount_allowed",
                           "plan_allowed", "eligible_amount", "allowed_charges", "allowed_usd", "approved_amount"),
    "patient_responsibility_usd": ("patient_responsibility", "patient_resp", "member_responsibility",
                                   "patient_owes", "you_owe", "amount_you_owe", "patient_balance",
                                   "total_patient_responsibility", "member_liability", "patient_liability",
                                   "patient_share", "member_share"),
    "qpa_usd": ("qpa", "qualifying_payment_amount", "qualifying_payment_amount_usd"),
    "claim_number": ("claim_no", "claim_id", "claim_num", "claim", "icn", "dcn", "claim_control_number",
                     "internal_control_number", "claim_reference", "claim_reference_number"),
    "denial_codes": ("denial_code", "adjustment_codes", "adjustment_code", "reason_codes", "reason_code", "carc",
                     "carc_codes", "remark_codes", "adjustment_reason_codes", "claim_adjustment_reason_codes",
                     "claim_adjustment_reason_code", "denial_reason_codes"),
    "service_lines": ("lines", "line_items", "services", "items", "claim_lines", "service_line_items",
                      "procedures", "service_details", "claim_details", "charges_detail", "details"),
    "network_status": ("network", "network_participation", "participation_status", "provider_network_status",
                       "provider_status", "network_tier", "participation"),
    "service_date": ("date_of_service", "dos", "dates_of_service", "service_dates", "service_from_date",
                     "service_from", "from_date", "date_of_services"),
    "determination_date": ("eob_date", "processed_date", "process_date", "date_processed", "statement_date",
                           "paid_date", "payment_date", "check_date", "issue_date", "issued_date", "remit_date",
                           "date_of_determination", "date_paid", "date_issued", "remittance_date",
                           "adjudication_date", "date_adjudicated"),
    "place_of_service": ("pos", "place_of_service_code", "pos_code", "service_location", "facility_type"),
    "provider_name": ("provider", "rendering_provider", "billing_provider", "provider_org",
                      "provider_organization", "rendering_provider_name", "billing_provider_name", "payee",
                      "payee_name", "servicing_provider", "provider_of_service"),
    "payer_name": ("payer", "plan", "health_plan", "plan_name", "insurer", "insurance_company",
                   "payer_organization", "health_plan_name", "carrier", "insurance", "insurer_name",
                   "payor", "payor_name"),
    "line_of_business": ("lob", "coverage_type", "plan_type", "product", "product_type", "program",
                         "coverage", "business_line"),
    "cpt_hcpcs_codes": ("cpt_codes", "cpt", "hcpcs", "procedure_codes", "codes", "cpt_code", "hcpcs_codes",
                        "procedure_code", "cpt_hcpcs", "cpt_hcpcs_code"),
    "provider_npi": ("npi", "provider_npi_number", "billing_npi", "rendering_npi"),
    "provider_tin": ("tin", "tax_id", "provider_tax_id", "ein", "federal_tax_id"),
    "member_id": ("member", "member_number", "subscriber_id", "member_id_number", "insured_id", "patient_id"),
    "provider_street": ("street", "address_line_1", "address1", "street_address", "provider_address_line_1"),
    "provider_city": ("city",),
    "provider_state": ("state",),
    "provider_zip": ("zip", "zip_code", "postal_code", "provider_postal", "provider_postal_code", "zipcode"),
}

# Fields whose value is a list of things; every other field is one value.
LIST_FIELDS = ("service_lines", "denial_codes", "cpt_hcpcs_codes")
# Keys whose contents are rows of a table: fields are not looked for inside
# them (a row's "billed_amount_usd" is that line's charge, not the claim's).
_ROW_CONTAINERS = set(ALIASES["service_lines"]) | {"service_lines"}


def _network_text(key: str, v: Any) -> Any:
    """in_network: true / out_of_network: "yes" -> the wording the rest of
    the code reads."""
    yes = v is True or (isinstance(v, str) and v.strip().lower() in ("true", "yes", "y", "1"))
    no = v is False or (isinstance(v, str) and v.strip().lower() in ("false", "no", "n", "0"))
    if not (yes or no):
        return v if isinstance(v, str) and v.strip() else None
    if key in ("in_network", "is_in_network", "participating", "is_participating", "contracted"):
        return "In-network" if yes else "Out-of-network"
    return "Out-of-network" if yes else "In-network"


_SCALAR_KEYS = ("name", "value", "text", "number", "id", "code", "status", "organization", "org")
_NETWORK_FLAG_KEYS = ("in_network", "is_in_network", "participating", "is_participating", "contracted",
                      "out_of_network", "is_out_of_network", "non_participating", "oon")


def shape_extraction(parsed: Any, fields: list[str]) -> dict:
    """The model's answer as a flat {field: value} for the schema's fields.

    Looks for each field under its own name, then its other names, at the top
    level first and then inside nested objects (a wrapper like {"eob": {...}},
    {"totals": {"billed": 1850}}, {"provider": {"name": ...}}). The shallowest
    match wins. Rows of a service-line table are never searched for claim-level
    fields. Keys outside the schema are dropped, as before."""
    out: dict[str, Any] = {}
    if isinstance(parsed, list) and len(parsed) == 1:
        parsed = parsed[0]
    if not isinstance(parsed, dict):
        return out
    want: dict[str, str] = {}
    for f in fields:
        want.setdefault(canon_key(f), f)
    for f in fields:
        for a in ALIASES.get(f, ()):
            want.setdefault(a, f)

    level: list[tuple[str, dict]] = [("", parsed)]
    for _depth in range(4):
        nxt: list[tuple[str, dict]] = []
        for parent, d in level:
            for raw_key, v in d.items():
                key = canon_key(raw_key)
                if _empty(v):
                    continue
                if "network_status" in fields and key in _NETWORK_FLAG_KEYS and "network_status" not in out:
                    txt = _network_text(key, v)
                    if txt:
                        out["network_status"] = txt
                        continue
                # "provider": {"name": X} reads as provider_name; "name" alone does not.
                cands = [f"{parent}_{key}", key] if parent else [key]
                if parent and key in _SCALAR_KEYS and parent in want:
                    cands.insert(0, parent)
                field = next((want[c] for c in cands if c in want), None)
                if field and field not in out:
                    if isinstance(v, dict) and field not in LIST_FIELDS:
                        # An object where one value was asked for: take its
                        # amount, or the cell that names it ({"name": ...}).
                        # An object with neither is a group of other fields
                        # ({"claim": {"number": ..., "billed": ...}}) and is
                        # searched on the next level instead.
                        one = as_money(v) if field.endswith("_usd") else next(
                            (as_text(val) for k2, val in v.items()
                             if canon_key(k2) in _SCALAR_KEYS and not _empty(val)
                             and not isinstance(val, (dict, list))), None)
                        if not _empty(one):
                            out[field] = one
                    else:
                        out[field] = v
                if isinstance(v, dict) and key not in _ROW_CONTAINERS:
                    nxt.append((key, v))
                elif isinstance(v, list) and len(v) == 1 and isinstance(v[0], dict) and key not in _ROW_CONTAINERS \
                        and want.get(key) not in LIST_FIELDS:
                    nxt.append((key, v[0]))
        level = nxt
        if not level:
            break
    for f in LIST_FIELDS:
        if f in out and f == "service_lines":
            out[f] = as_lines(out[f])
        elif f in out and not isinstance(out[f], list):
            out[f] = split_codes(out[f])
    return out


_RE_CARC = re.compile(r"\b(?:CO|PR|OA|PI|CR)[\s-]?\d{1,3}\b|\b(?:MA|M|N)\d{1,4}\b|\b[A-Z]?\d{4}[A-Z0-9]?\b|\b\d{1,3}\b")


def split_codes(v: Any) -> list[str]:
    """Codes from a list, a row, or "CO-45, PR-1" / "CO 45; N130"."""
    if isinstance(v, (list, tuple)):
        out: list[str] = []
        for x in v:
            for c in split_codes(x) if not isinstance(x, (int, float)) else [str(x)]:
                if c not in out:
                    out.append(c)
        return out
    text = as_text(v).upper()
    if not text:
        return []
    found = [re.sub(r"\s+", "-", c.strip()) for c in _RE_CARC.findall(text)]
    return list(dict.fromkeys(found)) or [text]


# ---------------------------------------------------------------------------
# Reading labelled values straight off the page
# ---------------------------------------------------------------------------

_AMT = r"\(?-?\$?\s*\d[\d,]*\.\d{2}\)?"

_MONEY_LABELS: dict[str, str] = {
    "billed_amount_usd": r"BILLED(?:\s+AMOUNT|\s+CHARGES?)?|(?:SUBMITTED|PROVIDER)\s+CHARGES?|AMOUNT\s+BILLED|"
                         r"CHARGES?\s+SUBMITTED|AMOUNT\s+CHARGED",
    "allowed_amount_usd": r"ALLOWED(?:\s+AMOUNT|\s+CHARGES?)?|AMOUNT\s+ALLOWED|PLAN\s+ALLOWED|ELIGIBLE\s+AMOUNT|"
                          r"APPROVED\s+AMOUNT",
    "patient_responsibility_usd": r"(?:PATIENT|MEMBER)\s+(?:RESPONSIBILITY|RESP\.?|LIABILITY|BALANCE|SHARE)|"
                                  r"(?:AMOUNT\s+)?YOU\s+(?:MAY\s+)?OWE|WHAT\s+YOU\s+OWE|PT\.?\s+RESP\.?",
}

# Headings that mean the same thing only as a table's column heading: "Charges"
# alone inside a sentence ("Allowed charges: ...") is not the billed amount.
_MONEY_HEADINGS: dict[str, str] = {"billed_amount_usd": r"CHARGES?|CHARGED|CHARGE\s+AMOUNT|BILLED\s+AMT\.?"}

_TEXT_LABELS: dict[str, str] = {
    "claim_number": r"CLAIM\s*(?:NUMBER|NO\.?|NUM\.?|#|ID|CONTROL\s+(?:NUMBER|NO\.?))|\bICN\b",
    "provider_name": r"(?:RENDERING\s+|BILLING\s+|SERVICING\s+)?PROVIDER(?:\s+NAME)?|PAYEE",
    "payer_name": r"PAYER(?:\s+NAME)?|PAYOR(?:\s+NAME)?|HEALTH\s+PLAN|PLAN\s+NAME|INSURER|INSURANCE\s+COMPANY|CARRIER",
    "network_status": r"NETWORK\s+STATUS|PROVIDER\s+(?:NETWORK\s+)?STATUS|NETWORK\s+PARTICIPATION|PARTICIPATION|NETWORK",
    "line_of_business": r"LINE\s+OF\s+BUSINESS|COVERAGE\s+TYPE|PLAN\s+TYPE|PRODUCT",
    "place_of_service": r"PLACE\s+OF\s+SERVICE|\bPOS\b",
}
_DATE = r"\d{1,2}[/-]\d{1,2}[/-]\d{2,4}|\d{4}-\d{2}-\d{2}|[A-Z][a-z]{2,8}\.?\s+\d{1,2},?\s+\d{4}"
_DATE_LABELS: dict[str, str] = {
    "service_date": r"DATES?\s+OF\s+SERVICES?|SERVICE\s+DATES?|\bDOS\b",
    "determination_date": r"(?:EOB|PROCESS(?:ED|ING)?|STATEMENT|PAID|PAYMENT|DETERMINATION|ISSUE[D]?|CHECK|"
                          r"REMIT(?:TANCE)?|ADJUDICATION)\s+DATE|DATE\s+(?:PROCESSED|PAID|ISSUED|OF\s+DETERMINATION|"
                          r"ADJUDICATED)",
}
_NETWORK_WORDS = re.compile(
    r"(?i)\b(out[\s-]*of[\s-]*network|non[\s-]*participating|non[\s-]*contracted|not\s+contracted|non[\s-]*par\b|"
    r"\boon\b|in[\s-]*network|participating|contracted|\binn\b|\bpar\b)")


_ALL_LABELS = re.compile("(?i)(?:TOTAL\\s+)?(?:" + "|".join(
    list(_MONEY_LABELS.values()) + list(_MONEY_HEADINGS.values()) + list(_TEXT_LABELS.values())
    + list(_DATE_LABELS.values()) + [r"DESCRIPTION|SERVICE|PROCEDURE|CODE|AMOUNT|DATE|PAID|PLAN\s+PAID|UNITS?|QTY"]) + ")")


def _clean(text: str) -> str:
    return re.sub(r"[*`]|<br\s*/?>", " ", str(text or "")).replace(" ", " ")


def _table_rows(text: str) -> list[list[list[str]]]:
    """GitHub-style tables in the markdown, each as a list of rows of cells."""
    tables: list[list[list[str]]] = []
    cur: list[list[str]] = []
    for line in text.splitlines():
        s = line.strip()
        if s.startswith("|") and s.count("|") >= 2:
            cells = [c.strip() for c in s.strip("|").split("|")]
            if all(re.fullmatch(r":?-{2,}:?", c) or c == "" for c in cells):
                continue  # the |---|---| rule under the header
            cur.append(cells)
        elif cur:
            tables.append(cur)
            cur = []
    if cur:
        tables.append(cur)
    return tables


def _money_from_tables(text: str, label_re: str) -> float | None:
    """A column headed by the label: its Total row if the table has one,
    its only row if there is one, else the sum of its rows -- when every
    row of the column holds an amount."""
    pat = re.compile(rf"(?i)^\s*(?:TOTAL\s+)?(?:{label_re})\s*(?:\(?(?:USD|\$)\)?)?\s*$")
    for rows in _table_rows(text):
        if len(rows) < 2:
            continue
        header, body = rows[0], rows[1:]
        col = next((i for i, c in enumerate(header) if pat.match(c)), None)
        if col is None:
            continue
        total = [r for r in body if r and re.search(r"(?i)\btotals?\b", " ".join(r[:max(1, col)]))]
        if total and col < len(total[-1]) and as_money(total[-1][col]) is not None:
            return as_money(total[-1][col])
        cells = [r[col] for r in body if col < len(r) and r not in total]
        vals = [as_money(c) for c in cells]
        if vals and all(v is not None for v in vals):
            return column_total(vals)
    return None


def _money_inline(text: str, label_re: str) -> float | None:
    """"Total Billed: $1,850.00" on a line. A line that says Total wins; with
    no Total line, the label must point at one single amount."""
    totals, plain = [], []
    for m in re.finditer(rf"(?im)(?<![A-Za-z])(TOTAL\s+)?(?:{label_re})\s*(?:\(?USD\)?)?\s*[:=|\-–]?\s*({_AMT})", text):
        amt = as_money(m.group(2))
        if amt is not None:
            (totals if m.group(1) else plain).append(amt)
    for group in (totals, plain):
        distinct = list(dict.fromkeys(group))
        if len(distinct) == 1:
            return distinct[0]
        if distinct:
            return None  # the label points at several different amounts: not guessed
    return None


def _label_blocks(text: str) -> list[tuple[str, str]]:
    """Forms read as all their labels first, then all their values:

        Claim Number:   Provider:   Service Date:
        BMC-2026-1044   Bayfront    03/02/2026

    comes out of OCR as a run of "Label:" lines followed by as many value
    lines. Pair them by position. A label line is short and ends with a
    colon; the pairing is used only when the value run is exactly as long."""
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    pairs: list[tuple[str, str]] = []
    is_label = lambda s: s.endswith(":") and len(s) <= 40 and not re.search(r"\d{3}", s)  # noqa: E731
    i = 0
    while i < len(lines):
        if not is_label(lines[i]):
            i += 1
            continue
        j = i
        while j < len(lines) and is_label(lines[j]):
            j += 1
        labels = lines[i:j]
        k = j
        while k < len(lines) and k - j < len(labels) and not is_label(lines[k]) and not lines[k].startswith(("|", "#")):
            k += 1
        values = lines[j:k]
        if len(labels) >= 2 and len(values) == len(labels):
            pairs += [(lab.rstrip(":").strip(), val) for lab, val in zip(labels, values)]
        i = max(k, i + 1)
    return pairs


def _labelled(text: str, label_re: str, value_re: str = r"[^|\n]{1,120}") -> str | None:
    """"Label: value" or "| Label | value |" -- the first one found."""
    for lab, val in _label_blocks(text):  # labels-first layouts, paired by position
        if re.fullmatch(rf"(?i)\s*(?:{label_re})\s*", lab):
            m = re.fullmatch(rf"\s*({value_re})\s*", val)
            return m.group(1).strip(" \t|:;,") if m and not _is_label(m.group(1)) else None
    for rows in _table_rows(text):  # a column headed by the label: its first row
        if len(rows) >= 2:
            col = next((i for i, c in enumerate(rows[0]) if re.fullmatch(rf"(?i)\s*(?:{label_re})\s*:?\s*", c)), None)
            if col is not None and col < len(rows[1]):
                m = re.fullmatch(rf"\s*({value_re})\s*", rows[1][col])
                if m and not _is_label(m.group(1)):
                    return m.group(1).strip(" \t|:;,") or None
    for m in re.finditer(rf"(?im)(?<![A-Za-z])(?:{label_re})[ \t]*(?:[:=#]|\|)\s*#?\s*({value_re})", text):
        # Two fields printed side by side on one line: the value ends at the gap.
        val = re.split(r"\s{2,}|\t", m.group(1).strip())[0].strip(" \t|;,")
        # A "value" that ends in a colon is the next label ("Claim Number:"
        # followed on the next line by a faded "rovider:"), never a value.
        if val.endswith(":"):
            continue
        val = val.strip(":")
        if val and not _is_label(val):
            return val
    return None


def _is_label(val: str) -> bool:
    """A "value" that is really the next column's heading."""
    return bool(_ALL_LABELS.fullmatch(val.strip(" :")))


def fill_from_text(extracted: dict, text: str, doc_type: str | None) -> tuple[dict, list[str], list[str]]:
    """Fill the fields the model left empty from labelled values printed on
    the page. Returns (extracted, fields read off the page, fields summed
    from the service lines). Only for EOBs and claims; only a field that is
    empty; only a value sitting next to its own label. Never raises."""
    out = dict(extracted) if isinstance(extracted, dict) else {}
    if doc_type not in ("eob", "idr_claim"):
        return out, [], []
    filled: list[str] = []
    t = _clean(text)

    def missing(f: str) -> bool:
        return _empty(out.get(f))

    def attempt(f: str, fn) -> None:
        if not missing(f):
            return
        try:
            v = fn()
        except Exception:  # noqa: BLE001 -- one field's pattern must not cost the others
            v = None
        if not _empty(v):
            out[f] = v
            filled.append(f)

    def money(f: str) -> float | None:
        heading = _MONEY_LABELS[f] + ("|" + _MONEY_HEADINGS[f] if f in _MONEY_HEADINGS else "")
        v = _money_from_tables(t, heading)
        return v if v is not None else _money_inline(t, _MONEY_LABELS[f])

    for f in _MONEY_LABELS:
        if f in out or doc_type == "eob" or f == "billed_amount_usd":
            attempt(f, lambda f=f: money(f))
    for f, label in _DATE_LABELS.items():
        if f in out or doc_type == "eob":
            attempt(f, lambda label=label: _labelled(t, label, _DATE))
    attempt("claim_number", lambda: _labelled(t, _TEXT_LABELS["claim_number"], r"[A-Z0-9][A-Z0-9\-/.]{3,39}"))
    attempt("place_of_service", lambda: _labelled(t, _TEXT_LABELS["place_of_service"], r"\d{2}\b[^|\n]{0,40}"))
    for f in ("provider_name", "payer_name", "line_of_business"):
        attempt(f, lambda f=f: _labelled(t, _TEXT_LABELS[f]))

    def network():
        v = _labelled(t, _TEXT_LABELS["network_status"])
        m = _NETWORK_WORDS.search(v or "")
        return m.group(1) if m else None
    attempt("network_status", network)

    # A claim total the page never prints, but whose lines all carry a charge.
    summed: list[str] = []
    if missing("billed_amount_usd"):
        attempt("billed_amount_usd", lambda: lines_billed_total(out.get("service_lines")))
        if "billed_amount_usd" in filled:
            filled.remove("billed_amount_usd")
            summed.append("billed_amount_usd")
    return out, filled, summed
