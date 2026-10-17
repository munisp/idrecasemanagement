"""Built-in letters, in the program's own layout.

Every FL letter is built here from the case record when the tenant has not uploaded a DOCX template of its own for
that key (an uploaded template always wins, so an office can still take over any letter without a deploy):

  final_order_rationale      the decision report: the claim dispute report (overturned / partially overturned /
                             upheld), or the external review (IRO) decision -- from public.decision_reports
  cover_letter_final_notice  the cover letter to the Agency that goes with it
  invoice_health_plan        the review-cost invoice to the health plan ("...H") and to the provider, from
  invoice_provider           public.invoices, with the late-charge totals and the remit-to block
  acceptance_letter, ineligible_letter, dismissal_letter, withdrawal_letter, plan_notification_letter,
  plan_optout_letter, rfi_letter, default_determination, fl_plan_packet
                             the stage letters

The wording follows the office's own letters (Capitol Bridge's decision reports, cover letters and invoices). The
organization's name, program lines, Agency addressee, invoice terms and bank details come from the program
config's "letters" block overlaid with public.letter_settings -- never from this file -- so a bank change is an
edit on the Billing page.

A letter that lacks what it must say (no outcome on the decision report, an invoice with no remit-to bank) is
refused with a LetterDataMissing that says what to fill in; main.py turns it into the case's failure note.
"""
from __future__ import annotations

import io
import os
import re
from datetime import date, datetime

import psycopg
from docx import Document
from docx.enum.section import WD_SECTION
from docx.enum.table import WD_TABLE_ALIGNMENT
from docx.enum.text import WD_ALIGN_PARAGRAPH
from docx.oxml import OxmlElement
from docx.oxml.ns import qn
from docx.shared import Inches, Pt, RGBColor

FONT = "Arial"  # LibreOffice renders it with Liberation Sans, which has Arial's metrics
ASSETS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "assets")


class LetterDataMissing(Exception):
    """The case does not yet hold what this letter must say."""


# ---- formatting -------------------------------------------------------------------------------------------------

def money(cents) -> str:
    if cents is None:
        return ""
    return f"${cents / 100:,.2f}"


def as_date(v) -> date | None:
    if v is None or v == "":
        return None
    if isinstance(v, datetime):
        return v.date()
    if isinstance(v, date):
        return v
    s = str(v).strip()
    for fmt in ("%Y-%m-%d", "%m/%d/%Y", "%B %d, %Y"):
        try:
            return datetime.strptime(s[:10] if fmt == "%Y-%m-%d" else s, fmt).date()
        except ValueError:
            pass
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00")).date()
    except ValueError:
        return None


def long_date(v) -> str:
    d = as_date(v)
    return f"{d.strftime('%B')} {d.day}, {d.year}" if d else (str(v) if v else "")


_WORDS = ["zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven",
          "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen", "twenty"]


def counted(n: int, noun: str) -> str:
    """'seven (7) claims', 'one (1) claim' -- the reports' way of counting."""
    word = _WORDS[n] if 0 <= n < len(_WORDS) else str(n)
    return f"{word} ({n}) {noun}{'' if n == 1 else 's'}"


def pct(v) -> str:
    if v is None:
        return ""
    f = float(v)
    return f"{f:.0f}%" if abs(f - round(f)) < 0.005 else f"{f:.2f}%"


# ---- context ----------------------------------------------------------------------------------------------------

def _merge(base: dict, over: dict) -> dict:
    out = dict(base or {})
    for k, v in (over or {}).items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = _merge(out[k], v)
        elif v not in (None, ""):
            out[k] = v
    return out


def _addr_lines(addr: dict | None) -> list[str]:
    a = addr or {}
    lines = [a.get(k) for k in ("line1", "street", "line2") if a.get(k)]
    city = ", ".join(x for x in (a.get("city"), " ".join(y for y in (a.get("state"), a.get("postal") or a.get("zip")) if y)) if x)
    if city:
        lines.append(city)
    return lines


def _party(c, tenant: str, side: str, account_id: str | None, fallback_name: str, details: dict) -> dict:
    """The provider or the health plan as the letters address it: name, contact, address, email."""
    out = {"name": "", "contact": "", "email": "", "address": []}
    r = c.execute(
        """SELECT name, coalesce(email,''), account_id::text FROM public.case_respondents
           WHERE tenant=%s AND case_id=%s AND side=%s ORDER BY created_at LIMIT 1""",
        (tenant, details["_case_id"], side),
    ).fetchone()
    if r:
        out["name"], out["email"] = r[0] or "", r[1]
        account_id = account_id or r[2]
    if account_id:
        a = c.execute(
            "SELECT legal_name, address FROM public.accounts WHERE tenant=%s AND id::text=%s", (tenant, account_id),
        ).fetchone()
        if a:
            out["name"] = out["name"] or a[0] or ""
            out["address"] = _addr_lines(a[1] if isinstance(a[1], dict) else None)
        ct = c.execute(
            "SELECT name, coalesce(email,'') FROM public.contacts WHERE tenant=%s AND account_id::text=%s ORDER BY created_at LIMIT 1",
            (tenant, account_id),
        ).fetchone()
        if ct:
            out["contact"] = ct[0] or ""
            out["email"] = out["email"] or ct[1]
    out["name"] = out["name"] or fallback_name or ("the Provider" if side == "PROVIDER" else "the Health Plan")
    if side == "PROVIDER":
        out["contact"] = out["contact"] or details.get("requester_contact_name") or ""
        out["email"] = out["email"] or details.get("requester_email") or ""
        if not out["address"]:
            out["address"] = _addr_lines({"street": details.get("provider_street"), "city": details.get("provider_city"),
                                          "state": details.get("provider_state"), "postal": details.get("provider_postal")})
    return out


def load_context(dsn: str, tenant: str, case_id: str) -> dict:
    with psycopg.connect(dsn) as c:
        row = c.execute(
            f"""SELECT case_number, provider_id, payer_id, details, coalesce(program_dates,'{{}}'::jsonb),
                       disputed_amount_cents, num_claims, opened_at
                FROM tenant_{tenant}.cases WHERE id=%s""",
            (case_id,),
        ).fetchone()
        if not row:
            raise KeyError(f"case {case_id} not found")
        details = dict(row[3] or {})
        details["_case_id"] = case_id
        prog = c.execute("SELECT config->'letters' FROM public.program_rules WHERE tenant=%s", (tenant,)).fetchone()
        ls = c.execute("SELECT settings FROM public.letter_settings WHERE tenant=%s", (tenant,)).fetchone()
        rep = c.execute(
            """SELECT report_type, outcome, awarded_cents, already_paid_cents, percent_awarded, sections, claims,
                      disallowed, reviewers
               FROM public.decision_reports WHERE tenant=%s AND case_id=%s""",
            (tenant, case_id),
        ).fetchone()
        inv = c.execute(
            """SELECT party, kind, invoice_no, amount_cents, status, due_date FROM public.invoices
               WHERE tenant=%s AND case_id=%s AND kind <> 'INITIAL_FEE' AND status <> 'VOID'
               ORDER BY created_at""",
            (tenant, case_id),
        ).fetchall()
        claims = c.execute(
            """SELECT claim_number, cpt, billed_cents, paid_cents, date_of_service, patient_first_name, patient_last_name
               FROM public.case_claims WHERE tenant=%s AND case_id=%s ORDER BY claim_number LIMIT 500""",
            (tenant, case_id),
        ).fetchall()
        elig = c.execute(
            """SELECT result, reason, evidence, created_at FROM public.eligibility_reviews
               WHERE tenant=%s AND case_id=%s ORDER BY created_at DESC LIMIT 1""",
            (tenant, case_id),
        ).fetchone()
        rfi = c.execute(
            """SELECT side, items, message, due_date, created_at FROM public.document_requests
               WHERE tenant=%s AND case_id=%s AND reason='RFI' ORDER BY created_at DESC LIMIT 1""",
            (tenant, case_id),
        ).fetchone()
        provider = _party(c, tenant, "PROVIDER", row[1], details.get("provider_org") or details.get("requester_org"), details)
        plan = _party(c, tenant, "HEALTH_PLAN", row[2], details.get("health_plan"), details)

    # The settings table holds groups (letterhead, agency, invoice, remit); the program's "letters" block is flat
    # with nested agency_addressee / invoice. Both become one view here.
    defaults = (prog[0] if prog and prog[0] else {}) or {}
    st = (ls[0] if ls and ls[0] else {}) or {}
    letters = _merge(defaults, st.get("letterhead") or {})
    agency = _merge({"salutation": (defaults.get("agency_addressee") or {}).get("salutation", ""),
                     "lines": (defaults.get("agency_addressee") or {}).get("lines", [])}, {})
    if st.get("agency"):
        a = st["agency"]
        if a.get("salutation"):
            agency["salutation"] = a["salutation"]
        lines = [a.get(k) for k in ("line1", "line2", "line3", "line4") if a.get(k)]
        if lines:
            agency["lines"] = lines
    letters["agency"] = agency
    letters["invoice"] = _merge(defaults.get("invoice") or {}, st.get("invoice") or {})
    letters["remit"] = st.get("remit") or {}
    letters.setdefault("org_name", "Capitol Bridge LLC")
    letters.setdefault("org_short", letters["org_name"].replace(" LLC", ""))

    report = None
    if rep:
        report = {"report_type": rep[0], "outcome": rep[1], "awarded_cents": rep[2], "already_paid_cents": rep[3],
                  "percent_awarded": float(rep[4]) if rep[4] is not None else None, "sections": rep[5] or {},
                  "claims": rep[6] or [], "disallowed": rep[7] or [], "reviewers": rep[8] or []}
    patients = {(cl[5] or "", cl[6] or "") for cl in claims if (cl[5] or cl[6])}
    pd = row[4] or {}
    return {
        "tenant": tenant, "case_id": case_id, "case_number": row[0], "details": details, "dates": pd,
        "disputed_cents": row[5], "num_claims": row[6] or len(claims) or 1,
        "num_patients": len(patients) or 1, "claims": claims, "opened_at": row[7],
        "provider": provider, "plan": plan, "letters": letters, "report": report,
        "invoices": [{"party": i[0], "kind": i[1], "invoice_no": i[2], "amount_cents": i[3], "status": i[4],
                      "due_date": i[5]} for i in inv],
        "eligibility": {"result": elig[0], "reason": elig[1], "evidence": elig[2] or {}, "at": elig[3]} if elig else None,
        "rfi": {"side": rfi[0], "items": rfi[1] or [], "message": rfi[2] or "", "due_date": rfi[3], "at": rfi[4]} if rfi else None,
        "today": date.today(),
    }


# ---- document building ------------------------------------------------------------------------------------------

def _set_cell_borders(cell, sz=6, color="000000"):
    tcPr = cell._tc.get_or_add_tcPr()
    borders = OxmlElement("w:tcBorders")
    for edge in ("top", "left", "bottom", "right"):
        el = OxmlElement(f"w:{edge}")
        el.set(qn("w:val"), "single")
        el.set(qn("w:sz"), str(sz))
        el.set(qn("w:color"), color)
        borders.append(el)
    tcPr.append(borders)


def _no_borders(table):
    tblPr = table._tbl.tblPr
    borders = OxmlElement("w:tblBorders")
    for edge in ("top", "left", "bottom", "right", "insideH", "insideV"):
        el = OxmlElement(f"w:{edge}")
        el.set(qn("w:val"), "nil")
        borders.append(el)
    tblPr.append(borders)


def _shade(cell, fill="D9D9D9"):
    tcPr = cell._tc.get_or_add_tcPr()
    shd = OxmlElement("w:shd")
    shd.set(qn("w:val"), "clear")
    shd.set(qn("w:color"), "auto")
    shd.set(qn("w:fill"), fill)
    tcPr.append(shd)


def set_widths(table, widths_in: list[float]):
    """Column widths LibreOffice honours: a fixed layout, the grid columns and every cell."""
    tbl = table._tbl
    tblPr = tbl.tblPr
    layout = OxmlElement("w:tblLayout")
    layout.set(qn("w:type"), "fixed")
    tblPr.append(layout)
    tw = OxmlElement("w:tblW")
    tw.set(qn("w:w"), str(int(sum(widths_in) * 1440)))
    tw.set(qn("w:type"), "dxa")
    for old in tblPr.findall(qn("w:tblW")):
        tblPr.remove(old)
    tblPr.append(tw)
    grid = tbl.tblGrid
    cols = grid.findall(qn("w:gridCol"))
    for gc, wd in zip(cols, widths_in):
        gc.set(qn("w:w"), str(int(wd * 1440)))
    for row in table.rows:
        for cell, wd in zip(row.cells, widths_in):
            cell.width = Inches(wd)


def repeat_header(table):
    """The first row repeats on every page and no row splits across a page."""
    for i, row in enumerate(table.rows):
        trPr = row._tr.get_or_add_trPr()
        cant = OxmlElement("w:cantSplit")
        trPr.append(cant)
        if i == 0:
            h = OxmlElement("w:tblHeader")
            trPr.append(h)


class Letter:
    """A letter page in the house layout: the wordmark top right (with a case line under it), Arial 11pt, headings
    in bold, body justified."""

    def __init__(self, ctx: dict, header_line: str = "", logo: bytes | None = None, header_left: str = ""):
        self.ctx = ctx
        self.doc = Document()
        sec = self.doc.sections[0]
        sec.page_width, sec.page_height = Inches(8.5), Inches(11)
        sec.left_margin = sec.right_margin = Inches(1)
        sec.top_margin, sec.bottom_margin = Inches(0.9), Inches(0.8)
        sec.header_distance = Inches(0.4)
        st = self.doc.styles["Normal"]
        st.font.name, st.font.size = FONT, Pt(11)
        st.element.rPr.rFonts.set(qn("w:eastAsia"), FONT)
        st.paragraph_format.space_after = Pt(8)
        st.paragraph_format.space_before = Pt(0)
        st.paragraph_format.line_spacing = 1.0
        hdr = sec.header
        hp = hdr.paragraphs[0]
        hp.alignment = WD_ALIGN_PARAGRAPH.LEFT if header_left else WD_ALIGN_PARAGRAPH.RIGHT
        if logo:
            hp.add_run().add_picture(io.BytesIO(logo), width=Inches(2.35))
        else:
            r = hp.add_run(ctx["letters"]["org_short"].upper())
            r.font.size, r.font.name = Pt(20), FONT
        hp.paragraph_format.space_after = Pt(0)
        if header_left:
            p2 = hdr.add_paragraph()
            r = p2.add_run(header_left)
            r.font.size = Pt(8)
            p2.paragraph_format.space_after = Pt(0)
        if header_line:
            p3 = hdr.add_paragraph()
            p3.alignment = WD_ALIGN_PARAGRAPH.RIGHT
            r = p3.add_run(header_line)
            r.font.size, r.bold = Pt(9), True
            p3.paragraph_format.space_after = Pt(0)

    # -- text
    def para(self, text: str = "", bold: bool = False, lead: str = "", align=WD_ALIGN_PARAGRAPH.JUSTIFY,
             size: float | None = None, color: RGBColor | None = None, space_after: float | None = None, italic=False):
        p = self.doc.add_paragraph()
        p.alignment = align
        if lead:
            r = p.add_run(lead + " ")
            r.bold = True
            if size:
                r.font.size = Pt(size)
        if text:
            r = p.add_run(text)
            r.bold, r.italic = bold, italic
            if size:
                r.font.size = Pt(size)
            if color is not None:
                r.font.color.rgb = color
        if space_after is not None:
            p.paragraph_format.space_after = Pt(space_after)
        return p

    def heading(self, text: str, center=False, space_after: float = 8):
        p = self.doc.add_paragraph()
        p.alignment = WD_ALIGN_PARAGRAPH.CENTER if center else WD_ALIGN_PARAGRAPH.LEFT
        r = p.add_run(text)
        r.bold = True
        p.paragraph_format.space_after = Pt(space_after)
        p.paragraph_format.space_before = Pt(0 if center else 4)
        p.paragraph_format.keep_with_next = True
        return p

    def text_block(self, text: str):
        """Free text as the reviewer wrote it: paragraphs on blank lines, single line breaks kept."""
        for chunk in re.split(r"\n\s*\n", (text or "").strip()):
            if not chunk.strip():
                continue
            p = self.doc.add_paragraph()
            p.alignment = WD_ALIGN_PARAGRAPH.JUSTIFY
            lines = chunk.strip().split("\n")
            for i, line in enumerate(lines):
                run = p.add_run(line.rstrip())
                if i < len(lines) - 1:
                    run.add_break()

    def bullets(self, items: list[str]):
        for i, it in enumerate(items):
            p = self.doc.add_paragraph(style="List Bullet")
            p.paragraph_format.space_after = Pt(8 if i == len(items) - 1 else 2)
            p.add_run(it)

    def two_col(self, left: str, right: str, bold=True):
        t = self.doc.add_table(rows=1, cols=2)
        _no_borders(t)
        set_widths(t, [3.25, 3.25])
        for cell, txt, al in ((t.cell(0, 0), left, WD_ALIGN_PARAGRAPH.LEFT), (t.cell(0, 1), right, WD_ALIGN_PARAGRAPH.RIGHT)):
            p = cell.paragraphs[0]
            p.alignment = al
            p.paragraph_format.space_after = Pt(0)
            self._labelled(p, txt, bold)
        self.doc.add_paragraph().paragraph_format.space_after = Pt(2)

    @staticmethod
    def _labelled(p, txt: str, bold=True):
        """'LABEL: value' with the label bold."""
        if ":" in txt and bold:
            lab, val = txt.split(":", 1)
            r = p.add_run(lab + ":")
            r.bold = True
            p.add_run(val)
        else:
            r = p.add_run(txt)
            r.bold = bold

    def grid(self, headers: list[str], rows: list[list[str]], widths: list[float] | None = None, size=8,
             right_cols: tuple = (), bold_rows: tuple = ()):
        t = self.doc.add_table(rows=1 + len(rows), cols=len(headers))
        t.alignment = WD_TABLE_ALIGNMENT.CENTER
        for j, h in enumerate(headers):
            c = t.cell(0, j)
            _set_cell_borders(c)
            _shade(c)
            p = c.paragraphs[0]
            p.alignment = WD_ALIGN_PARAGRAPH.CENTER
            p.paragraph_format.space_after = Pt(0)
            r = p.add_run(h)
            r.bold, r.font.size = True, Pt(size)
        for i, row in enumerate(rows, start=1):
            for j, v in enumerate(row):
                c = t.cell(i, j)
                _set_cell_borders(c)
                p = c.paragraphs[0]
                p.alignment = WD_ALIGN_PARAGRAPH.RIGHT if j in right_cols else WD_ALIGN_PARAGRAPH.LEFT
                p.paragraph_format.space_after = Pt(0)
                r = p.add_run(v or "")
                r.font.size = Pt(size)
                r.bold = (i - 1) in bold_rows
        if widths:
            set_widths(t, widths)
        repeat_header(t)
        self.doc.add_paragraph().paragraph_format.space_after = Pt(2)
        return t

    def sign_off(self):
        self.para("Sincerely,", align=WD_ALIGN_PARAGRAPH.LEFT, space_after=18)
        self.para(self.ctx["letters"]["org_short"], align=WD_ALIGN_PARAGRAPH.LEFT)

    def address_block(self, lines: list[str]):
        for i, l in enumerate([x for x in lines if x]):
            self.para(l, align=WD_ALIGN_PARAGRAPH.LEFT, space_after=0)
        self.para("", space_after=6)

    def save(self) -> bytes:
        buf = io.BytesIO()
        self.doc.save(buf)
        return buf.getvalue()


def _logo(minio, bucket: str, tenant: str) -> bytes | None:
    """The tenant's wordmark: an uploaded one (templates bucket, {tenant}/letterhead_logo.png|jpg) or the one the
    image ships for the tenant."""
    for name in ("letterhead_logo.png", "letterhead_logo.jpg"):
        try:
            return minio.get_object(bucket, f"{tenant}/{name}").read()
        except Exception:  # noqa: BLE001 -- none uploaded
            pass
    for name in ("letterhead_logo.jpg", "letterhead_logo.png"):
        p = os.path.join(ASSETS, tenant, name)
        if os.path.exists(p):
            with open(p, "rb") as f:
                return f.read()
    return None


# ---- the decision ------------------------------------------------------------------------------------------------

def _decision(ctx: dict, need_amount=True) -> dict:
    rep = ctx["report"]
    if not rep or not rep.get("outcome"):
        raise LetterDataMissing("the decision report has no outcome yet: open the case's Decision report, choose "
                                "overturned, partially overturned or upheld, and save")
    if need_amount and rep.get("report_type") != "IRO" and rep.get("awarded_cents") is None:
        raise LetterDataMissing("the decision report has no amount awarded yet: enter it on the case's Decision report")
    return rep


def recommendation(ctx: dict, rep: dict, verb="recommends", short=False) -> str:
    """The sentence every part of the decision repeats, in the report's own words."""
    org = ctx["letters"]["org_short"]
    aw, paid = rep.get("awarded_cents"), rep.get("already_paid_cents")
    o = rep["outcome"]
    if o == "UPHELD":
        tail = f" and the amount of {money(aw)} be awarded to the Health Plan" if aw is not None else ""
        return f"{org} {verb} the Health Plan’s denial for these services be upheld{tail}."
    what = ("the Health Plan’s denial or partial payment for these services be overturned" if o == "OVERTURNED"
            else ("that " if verb == "recommends" else "") +
            "the Health Plan’s denial of payment for these services be partially overturned")
    s = f"{org} {verb} {what} and the amount of {money(aw)} be awarded to the Provider"
    if paid:
        bal = max((aw or 0) - paid, 0)
        s += (f", of which {money(paid)} was already paid to the Provider. Therefore, the balance of {money(bal)} "
              f"is owed to the Provider.")
    else:
        s += "."
    return s


def _claim_no(ctx: dict, rep: dict) -> str:
    nums = {c.get("claim_number") for c in (rep.get("claims") or []) if c.get("claim_number")} or \
           {c[0] for c in ctx["claims"] if c[0]}
    if len(nums) == 1:
        return next(iter(nums))
    return "ALL" if nums else ""


def build_decision_report(ctx: dict, logo: bytes | None) -> Letter:
    rep = _decision(ctx)
    if rep.get("report_type") == "IRO":
        return build_iro_decision(ctx, rep, logo)
    L, sec = ctx["letters"], rep.get("sections") or {}
    org, prov, plan = L["org_short"], ctx["provider"], ctx["plan"]
    d = Letter(ctx, header_line=ctx["case_number"], logo=logo)
    d.heading(L["org_name"].upper() + ".", center=True, space_after=0)
    d.heading(L.get("program_title", "PROVIDER AND HEALTH PLAN CLAIM DISPUTE RESOLUTION PROGRAM"), center=True, space_after=10)
    d.two_col(f"CASE NO: {ctx['case_number']}", f"CLAIM NO: {_claim_no(ctx, rep)}")
    rec = recommendation(ctx, rep)
    d.para(rec.replace(f"{org} recommends", f"{L['org_name']} ({org}) recommends", 1), bold=True, lead="Summary:")

    d.heading("Claim Summary:")
    d.text_block(sec.get("claim_summary") or
                 f"This case concerns {counted(ctx['num_claims'], 'claim')} brought by {prov['name']} for services "
                 f"rendered to {counted(ctx['num_patients'], 'patient')} enrolled in {plan['name']}.")
    d.heading("Standard of Review:")
    d.text_block(sec.get("standard_of_review") or L.get("standard_of_review", ""))
    d.heading("Recommended Determination:")
    d.para(rec)

    d.heading("Rationale:")
    accepted = ctx["dates"].get("date_case_accepted") or ctx["dates"].get("date_acknowledgment_letter_sent_to_provider")
    if accepted:
        d.para(f"On {long_date(accepted)}, {org} notified {prov['name']} that the case was accepted for review and "
               f"notified {plan['name']} of said acceptance. Both parties participated in full with the process and "
               f"submitted full statements and responses.")
    if sec.get("dispute_issue"):
        d.text_block(sec["dispute_issue"])
    for key, title in (("provider_position", "Provider’s Position:"), ("plan_position", "Health Plan’s Position:"),
                       ("applicable_law", "Applicable Law:")):
        if sec.get(key):
            d.heading(title)
            d.text_block(sec[key])
    if sec.get("analysis"):
        d.text_block(sec["analysis"])
    if sec.get("physician_review"):
        d.heading("Independent Physician Review:")
        d.text_block(sec["physician_review"])

    claims = rep.get("claims") or []
    dis = rep.get("disallowed") or []
    if sec.get("coding_review") or sec.get("pricing_method") or claims or dis:
        d.heading("Coding Consultant Review:")
        if sec.get("coding_review"):
            d.text_block(sec["coding_review"])
        if sec.get("pricing_method"):
            d.text_block(sec["pricing_method"])
        priced = [c for c in claims if any(c.get(k) is not None for k in ("ucr_cents", "lesser_cents", "reimbursement_cents"))]
        if priced:
            d.para("The chart below provides a comparison of billed charges and reimbursement rates for the codes "
                   "and dates of service at issue.")
            rows = [[c.get("claim_number", ""), c.get("cpt", ""), c.get("dos", ""), c.get("zip", ""),
                     money(c.get("billed_cents")), money(c.get("ucr_cents")), money(c.get("lesser_cents")),
                     money(c.get("paid_cents")), money(c.get("reimbursement_cents"))] for c in priced]
            tot = lambda k: sum(c.get(k) or 0 for c in priced)  # noqa: E731
            rows.append(["Total", "", "", "", money(tot("billed_cents")), money(tot("ucr_cents")),
                         money(tot("lesser_cents")), money(tot("paid_cents")), money(tot("reimbursement_cents"))])
            d.grid(["Claim Number", "CPT Code", "Date of Service", "Zip Code", "Amount Billed", "UCR Pricing",
                    "Lesser of the UCR & Billed Amount", "Amount Paid", "Reimbursement Amount"], rows,
                   widths=[0.9, 0.55, 0.75, 0.55, 0.7, 0.7, 0.8, 0.7, 0.85], size=7.5,
                   right_cols=(4, 5, 6, 7, 8), bold_rows=(len(rows) - 1,))
        if dis:
            d.para("The table below summarizes the sum of disallowed charges.")
            groups: dict[str, list[dict]] = {}
            for x in dis:
                groups.setdefault(x.get("reason") or "", []).append(x)
            rows, bold = [], []
            for reason, items in groups.items():
                for i, x in enumerate(items):
                    rows.append([reason if i == 0 else "", x.get("description", ""), x.get("revenue_code", ""),
                                 x.get("quantity", ""), money(x.get("amount_cents"))])
                qty = 0
                for x in items:
                    try:
                        qty += int(str(x.get("quantity") or "0").replace(",", ""))
                    except ValueError:
                        pass
                amt = sum(x.get("amount_cents") or 0 for x in items)
                bold.append(len(rows))
                rows.append(["", "", "Total", str(qty), money(amt) if amt else ""])
            total = sum(x.get("amount_cents") or 0 for x in dis)
            d.grid(["Denial Reason", "Description", "Revenue Code", "Quantity", "Disallowed"], rows,
                   widths=[2.0, 2.0, 0.8, 0.7, 0.9], size=8, right_cols=(3, 4), bold_rows=tuple(bold))
            if total:
                d.para(f"{org} has found that {money(total)} of disallowed charges will remain denied.")

    closing = "Based on the foregoing, " + rec
    pa = rep.get("percent_awarded")
    if pa is None and rep.get("awarded_cents") is not None and ctx.get("disputed_cents"):
        pa = round(100 * rep["awarded_cents"] / ctx["disputed_cents"], 2)
    if rep["outcome"] == "PARTIALLY_OVERTURNED" and pa is not None:
        closing += f" {org} is awarding {money(rep.get('awarded_cents'))} or {pct(pa)} of the disputed charge to the Provider."
    elif rep["outcome"] == "OVERTURNED":
        closing += f" 100% of the {counted(ctx['num_claims'], 'claim')} reviewed have been overturned." \
            if ctx["num_claims"] > 1 else ""
    d.para(closing)
    if sec.get("closing_note"):
        d.text_block(sec["closing_note"])
    d.sign_off()
    return d


def build_iro_decision(ctx: dict, rep: dict, logo: bytes | None) -> Letter:
    L, sec = ctx["letters"], rep.get("sections") or {}
    org = L["org_short"]
    d = Letter(ctx, header_line=f"IRO Number: {sec['iro_number']}" if sec.get("iro_number") else ctx["case_number"], logo=logo)
    d.heading(L["org_name"].upper() + ".", center=True, space_after=0)
    if sec.get("iro_program_title"):
        d.heading(sec["iro_program_title"].upper(), center=True, space_after=4)
    if sec.get("agency_case_no"):
        d.heading(f"CASE NO: {sec['agency_case_no']}", center=True, space_after=10)
    d.two_col(f"{L['org_short'].upper()} CASE NO: {ctx['case_number']}", f"PLAN REFERENCE NO: {sec.get('plan_reference_no', '')}")
    d.two_col(f"DATE RECEIVED: {long_date(sec.get('date_received'))}", f"DECISION DATE: {long_date(sec.get('decision_date'))}")
    word = {"OVERTURNED": "overturned", "PARTIALLY_OVERTURNED": "partially overturned", "UPHELD": "upheld"}[rep["outcome"]]
    d.para(f"{L['org_name']} ({org}) recommends the Health Plan’s denial of payment for services be {word}.",
           lead="Decision Summary:")
    d.heading("Claim Summary:")
    if sec.get("date_received"):
        src = sec.get("review_received_from") or "the Department of Insurance"
        recv = f" and on {long_date(sec['records_received'])}, received the case documentation from the health plan" \
            if sec.get("records_received") else ""
        conducted = f" This review was conducted on {long_date(sec['decision_date'])}." if sec.get("decision_date") else ""
        d.para(f"On {long_date(sec['date_received'])}, {org} received this review assignment from {src}{recv}.{conducted}")
    d.text_block(sec.get("claim_summary", ""))
    facts = [f"Date(s) of service: {sec.get('dates_of_service') or ''}",
             f"Health care Provider: {ctx['provider']['name']}",
             f"Health Plan: {ctx['plan']['name']}",
             f"Denial reason: {sec.get('denial_reason') or ''}"]
    d.bullets(facts)
    d.heading("Standard of Review:")
    d.text_block(sec.get("standard_of_review") or L.get("standard_of_review", ""))
    d.para(f"{org} review personnel and Medical Professional Reviewers are impartial and have no conflict of interest "
           f"with the parties involved in this dispute.")
    if sec.get("reviewer_credentials"):
        d.text_block(sec["reviewer_credentials"])
    d.heading("Recommended Determination:")
    because = f" because {sec['determination_reason'].rstrip('.')}." if sec.get("determination_reason") else "."
    d.para(f"{org} recommends the Insurance Carrier’s denial of payment for these services be {word}{because}")
    if sec.get("evidence"):
        d.heading("Evidence Supporting the Determination:")
        for line in [x for x in sec["evidence"].split("\n") if x.strip()]:
            d.para(line.strip(), align=WD_ALIGN_PARAGRAPH.LEFT, space_after=6)
    d.heading("Rationale:")
    d.text_block(sec.get("professional_review", ""))
    if sec.get("binding_statute"):
        d.para(f"This decision is final and binding on the insurance carrier and the covered person pursuant to "
               f"{sec['binding_statute'].rstrip('.')}.")
    d.sign_off()
    return d


def build_cover_letter(ctx: dict, logo: bytes | None) -> Letter:
    rep = _decision(ctx)
    L, sec = ctx["letters"], rep.get("sections") or {}
    org, prov, plan = L["org_short"], ctx["provider"], ctx["plan"]
    d = Letter(ctx, header_line=ctx["case_number"], logo=logo)
    d.para(long_date(ctx["today"]), align=WD_ALIGN_PARAGRAPH.LEFT)
    d.address_block(L["agency"].get("lines") or [L.get("agency_name", "")])
    d.para("RE: External Review of Denied Claims", bold=True, align=WD_ALIGN_PARAGRAPH.LEFT)
    d.two_col(f"CASE NO: {ctx['case_number']}", f"CLAIM NO: {_claim_no(ctx, rep)}")
    d.heading("Summary:")
    lob = ctx["details"].get("line_of_business")
    enrolled = f"{plan['name']}{' – ' + lob if lob else ''}"
    s = (f"This case involved {counted(ctx['num_claims'], 'claim')} submitted by {prov['name']} involving "
         f"{counted(ctx['num_patients'], 'patient')} enrolled in {enrolled}.")
    if ctx.get("disputed_cents"):
        s += f" The amount in controversy is {money(ctx['disputed_cents'])}."
    d.para(s)
    d.para(recommendation(ctx, rep, verb="has determined that").replace(f"{org} has determined that",
           f"{L['org_name']}. has determined that", 1))
    if L["agency"].get("salutation"):
        d.para(L["agency"]["salutation"], align=WD_ALIGN_PARAGRAPH.LEFT)
    d.para(f"{org} is an organization that contracts with the {L.get('state', 'Florida')} "
           f"{L.get('agency_name', 'Agency for Health Care Administration')} to independently review Provider and "
           f"Managed Care Organization Claim Disputes in response to claims brought under "
           f"{L.get('statute', 'Chapter 408.7057, Florida Statutes')}. {org} review personnel and Consultant Coding "
           f"and Medical Professional Reviewers are impartial. They do not work for Providers or Health Plans in the "
           f"State of {L.get('state', 'Florida')}.")
    accepted = ctx["dates"].get("date_case_accepted") or ctx["dates"].get("date_acknowledgment_letter_sent_to_provider")
    reviewers = rep.get("reviewers") or ["attorney", "physician", "coding consultant", "administrative"]
    who = (", ".join(reviewers[:-1]) + " and " + reviewers[-1]) if len(reviewers) > 1 else reviewers[0]
    first = f"{org} completed its initial review of this case on {long_date(accepted)} and accepted this case for full review. " \
        if accepted else ""
    d.para(f"{first}The claims that comprise this case were reviewed by {'an' if who[0] in 'aeiou' else 'a'} {who} "
           f"reviewers on the {org} professional appeals staff.")
    d.heading("Case File Summary:")
    d.text_block(sec.get("case_file_summary") or sec.get("claim_summary") or
                 f"This case involves {counted(ctx['num_claims'], 'claim')} submitted by {prov['name']} involving "
                 f"{counted(ctx['num_patients'], 'patient')} enrolled in {enrolled}.")
    d.heading("Standard of Review:")
    d.text_block(sec.get("standard_of_review") or L.get("standard_of_review", ""))
    d.heading("Decision:")
    d.para(recommendation(ctx, rep, verb="has determined that"))
    d.para("Enclosed with this letter is the analysis and basis for decision for the issue raised in the claim "
           "included in this case.")
    d.sign_off()
    return d


# ---- invoices ---------------------------------------------------------------------------------------------------

def invoice_for(ctx: dict, party: str) -> dict:
    mine = [i for i in ctx["invoices"] if i["party"] == party]
    if not mine:
        who = "health plan" if party == "HEALTH_PLAN" else "provider"
        raise LetterDataMissing(f"there is no review invoice to the {who} on this case: the PM issues it first "
                                f"(Billing on the case)")
    inv = mine[-1]
    total = sum(i["amount_cents"] for i in ctx["invoices"] if i["kind"] == inv["kind"])
    share = round(100 * inv["amount_cents"] / total) if total else 100
    suffix = ctx["letters"]["invoice"].get("health_plan_suffix", "H") if party == "HEALTH_PLAN" else ""
    return {**inv, "share": share, "number": f"{inv['invoice_no']}{suffix}"}


def build_invoice(ctx: dict, party: str, logo: bytes | None) -> tuple[Letter, dict]:
    inv = invoice_for(ctx, party)
    L, terms, remit = ctx["letters"], ctx["letters"]["invoice"], ctx["letters"]["remit"]
    missing = [label for k, label in (("bank", "bank name"), ("routing", "routing number"), ("account", "account number"))
               if not remit.get(k)]
    if missing:
        raise LetterDataMissing("the remit-to bank details are not set (" + ", ".join(missing) +
                                "): Finance or an administrator adds them under Letterhead & invoice, on the Billing or Templates page")
    org = L["org_short"]
    who = ctx["plan"] if party == "HEALTH_PLAN" else ctx["provider"]
    role = "Health Plan" if party == "HEALTH_PLAN" else "Provider"
    fee, after, every = int(terms.get("late_fee_cents", 5000)), int(terms.get("late_after_days", 35)), int(terms.get("late_every_days", 30))
    amt = inv["amount_cents"]
    d = Letter(ctx, logo=logo, header_left=L.get("contractor_line", ""))
    p = d.para("INVOICE", bold=True, align=WD_ALIGN_PARAGRAPH.RIGHT, space_after=2)
    # The INVOICE box, top right.
    t = d.doc.add_table(rows=2, cols=3)
    t.alignment = WD_TABLE_ALIGNMENT.RIGHT
    for j, (h, v) in enumerate((("DATE\nPREPARED", ctx["today"].strftime("%m/%d/%Y")),
                                ("DATE\nISSUED", "See Final\nOrder Date"), ("INVOICE #", inv["number"]))):
        for i, txt in enumerate((h, v)):
            c = t.cell(i, j)
            _set_cell_borders(c)
            c.width = Inches(1.15)
            cp = c.paragraphs[0]
            cp.alignment = WD_ALIGN_PARAGRAPH.CENTER
            cp.paragraph_format.space_after = Pt(0)
            r = cp.add_run(txt)
            r.bold, r.font.size = True, Pt(10)
    set_widths(t, [1.15, 1.15, 1.15])
    d.para("", space_after=4)
    d.para("BILL TO:", bold=True, align=WD_ALIGN_PARAGRAPH.LEFT, space_after=6)
    for line in [who["name"], f"ATTN: {who['contact']}" if who.get("contact") else "ATTN:"] + who.get("address", []) + \
            ([who["email"]] if who.get("email") else []):
        d.para(line, align=WD_ALIGN_PARAGRAPH.LEFT, space_after=0)
    d.para("", space_after=4)
    rule = terms.get("rule_full", "FL Admin Code R 59A-12.030(4)(a)") if inv["share"] >= 100 else \
        terms.get("rule_split", "FL Admin Code R 59A-12.030(4)(b)")
    t = d.doc.add_table(rows=3, cols=2)
    widths = (Inches(4.2), Inches(2.3))
    for j, h in enumerate(("DESCRIPTION", "AMOUNT")):
        c = t.cell(0, j)
        _set_cell_borders(c)
        _shade(c)
        c.width = widths[j]
        cp = c.paragraphs[0]
        cp.alignment = WD_ALIGN_PARAGRAPH.CENTER
        cp.paragraph_format.space_after = Pt(0)
        cp.add_run(h).bold = True
    desc, amount = t.cell(1, 0), t.cell(1, 1)
    for c, w in ((desc, widths[0]), (amount, widths[1])):
        _set_cell_borders(c)
        c.width = w

    def cell_para(cell, text, bold=False, first=False, size=10, align=WD_ALIGN_PARAGRAPH.LEFT):
        cp = cell.paragraphs[0] if first else cell.add_paragraph()
        cp.alignment = align
        cp.paragraph_format.space_after = Pt(6)
        r = cp.add_run(text)
        r.bold, r.font.size = bold, Pt(size)
        return cp

    cell_para(desc, "External Review of Denied Claims", bold=True, first=True)
    cell_para(desc, f"Review costs: {money(amt)}\n{role} is responsible for {inv['share']}% of the review cost per the "
                    f"determination in the Final Order ({rule})", bold=True)
    cell_para(desc, f"{org} Case Number: {inv['invoice_no']}", bold=True)
    cell_para(desc, f"A {money(fee).replace('.00', '')} late charge will be added if payment is not received within "
                    f"{after} days. An additional {money(fee).replace('.00', '')} late charge will accrue every {every} "
                    f"days until the invoice is paid.")
    cell_para(amount, money(amt), bold=True, first=True, align=WD_ALIGN_PARAGRAPH.RIGHT)
    blank, totals = t.cell(2, 0), t.cell(2, 1)
    for c, w in ((blank, widths[0]), (totals, widths[1])):
        _set_cell_borders(c)
        c.width = w
    blank.paragraphs[0].paragraph_format.space_after = Pt(0)
    cell_para(totals, f"TOTAL:      {money(amt)}", bold=True, first=True)
    cell_para(totals, f"TOTAL if paid after {after} days: {money(amt + fee)}")
    cell_para(totals, f"TOTAL if paid after {after + every} days: {money(amt + 2 * fee)}")
    set_widths(t, [4.2, 2.3])
    d.para("", space_after=4)
    d.para(f"**{terms.get('payment_note', org + ' only accepts ACH payment for invoices.')}**", bold=True,
           align=WD_ALIGN_PARAGRAPH.LEFT, space_after=0)
    email = remit.get("remittance_email") or L.get("contact_email", "")
    if email:
        d.para(f"If you have any issues with payment, please email {email}.", align=WD_ALIGN_PARAGRAPH.LEFT, size=9, space_after=4)
    red = RGBColor(0xC0, 0x00, 0x00)
    if terms.get("bank_changed_notice"):
        d.para(f"**{terms['bank_changed_notice']}**", bold=True, align=WD_ALIGN_PARAGRAPH.CENTER, color=red, size=10, space_after=0)
    if terms.get("account_use_note"):
        d.para(terms["account_use_note"], align=WD_ALIGN_PARAGRAPH.CENTER, color=red, size=9, space_after=4)
    d.para("REMIT TO:", bold=True, align=WD_ALIGN_PARAGRAPH.LEFT, space_after=4)
    rt = d.doc.add_table(rows=0, cols=2)
    _no_borders(rt)
    rows = [("", remit.get("payee") or L["org_name"]), ("Bank:", remit["bank"]), ("Routing No.:", remit["routing"]),
            ("Account No.:", remit["account"]),
            ("Remittance Advice:", f"Email a copy of the remittance advice to {email} with the case number "
                                   f"{inv['invoice_no']} listed on the document." if email else f"{inv['number']} - FL CDR")]
    for lab, val in rows:
        cells = rt.add_row().cells
        for c, txt in ((cells[0], lab), (cells[1], val)):
            cp = c.paragraphs[0]
            cp.paragraph_format.space_after = Pt(3)
            cp.add_run(txt).font.size = Pt(10)
    set_widths(rt, [1.4, 4.4])
    tblPr = rt._tbl.tblPr
    ind = OxmlElement("w:tblInd")
    ind.set(qn("w:w"), str(int(1.0 * 1440)))
    ind.set(qn("w:type"), "dxa")
    tblPr.append(ind)
    return d, {"invoice_number": inv["number"]}


# ---- stage letters ----------------------------------------------------------------------------------------------

ELIG_REASON_TEXT = {
    "late_payment_only": "The dispute concerns only the late payment of a claim.",
    "interest_only": "The dispute concerns only the payment of interest on a claim.",
    "medicare_grievance": "The claim is the subject of an open Medicare managed care grievance.",
    "medicare_reconsideration": "The claim qualifies for Medicare reconsideration.",
    "plan_not_fl_regulated": "The member’s health plan is not regulated by the State of Florida.",
    "provider_not_fl_licensed": "The provider is not licensed in the State of Florida.",
    "medicaid_fair_hearing": "The claim is the subject of a Medicaid fair hearing.",
    "pending_court_action": "The claim is the basis of an action pending in state or federal court.",
    "over_12_months": "The dispute was filed more than 12 months after the final determination on the claim.",
    "pre_2000_binding_process": "The claim is subject to a binding internal claim dispute resolution process entered "
                                "into before October 1, 2000.",
    "below_threshold": "The amount in dispute is below the minimum amount for this type of provider.",
    "improper_aggregation": "The claims in the filing were aggregated in a way the program does not allow.",
    "internal_process_not_exhausted": "The contract between the parties requires its internal dispute process, and "
                                      "that process has not been exhausted.",
}


def _stage_letter(ctx: dict, logo: bytes | None, to: dict, title: str) -> Letter:
    L = ctx["letters"]
    d = Letter(ctx, header_line=ctx["case_number"], logo=logo)
    d.para(long_date(ctx["today"]), align=WD_ALIGN_PARAGRAPH.LEFT)
    d.address_block([to["name"]] + ([f"ATTN: {to['contact']}"] if to.get("contact") else []) + to.get("address", []))
    d.para(f"RE: {title}", bold=True, align=WD_ALIGN_PARAGRAPH.LEFT, space_after=0)
    d.para(f"Case Number: {ctx['case_number']}", bold=True, align=WD_ALIGN_PARAGRAPH.LEFT, space_after=0)
    d.para(f"Provider: {ctx['provider']['name']}", align=WD_ALIGN_PARAGRAPH.LEFT, space_after=0)
    d.para(f"Health Plan: {ctx['plan']['name']}", align=WD_ALIGN_PARAGRAPH.LEFT)
    d.para(f"Dear {to.get('contact') or to['name']}:", align=WD_ALIGN_PARAGRAPH.LEFT)
    return d


def _contact_close(d: Letter):
    L = d.ctx["letters"]
    d.para(f"If you have any questions regarding this correspondence, please email {L.get('contact_email', '')}"
           f"{' or call ' + L['contact_phone'] if L.get('contact_phone') else ''}.")
    d.sign_off()


def build_acceptance(ctx, logo):
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    d = _stage_letter(ctx, logo, ctx["provider"], "Results of Preliminary Review")
    d.para(f"{L['org_name']} ({org}) received the completed filing of the claims dispute submitted by your "
           f"organization under the {L.get('state', 'Florida')} {L.get('agency_name', '')} Claims Dispute Resolution "
           f"program. {org} has completed its preliminary review and has accepted this dispute for full review.")
    amt = f" The amount in dispute is {money(ctx['disputed_cents'])}." if ctx.get("disputed_cents") else ""
    d.para(f"The dispute concerns {counted(ctx['num_claims'], 'claim')} for services rendered to members of "
           f"{ctx['plan']['name']}.{amt}")
    d.para(f"{org} will now notify the Health Plan of the dispute. The Health Plan has 15 calendar days from that "
           f"notice to respond with all supporting documentation. If you received an estimated cost of the Full "
           f"Review, please respond to that email within 15 calendar days of its receipt so {org} may proceed; if no "
           f"response is received, {org} will close the dispute request and no further action will be taken.")
    _contact_close(d)
    return d


def build_ineligible(ctx, logo):
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    el = ctx.get("eligibility") or {}
    if el.get("result") != "INELIGIBLE":
        raise LetterDataMissing("the latest eligibility review did not find this dispute ineligible")
    d = _stage_letter(ctx, logo, ctx["provider"], "Results of Preliminary Review")
    d.para(f"{L['org_name']} ({org}) received the completed filing of the claims dispute submitted by your "
           f"organization and has completed its preliminary review. {org} has determined that the dispute is not "
           f"eligible for review under the {L.get('state', 'Florida')} Claims Dispute Resolution program "
           f"({L.get('statute', '')}) for the following reason:")
    reasons = [r for r in re.split(r"[,;]\s*", el.get("reason") or "") if r]
    d.bullets([ELIG_REASON_TEXT.get(r, r.replace("_", " ").capitalize()) for r in reasons] or ["See the enclosed review."])
    ev = el.get("evidence") or {}
    if "below_threshold" in reasons and ev.get("threshold_min_cents"):
        d.para(f"The minimum amount in dispute for this type of provider is {money(ev['threshold_min_cents'])}.")
    d.para("No further action will be taken on this dispute request.")
    _contact_close(d)
    return d


def build_dismissal(ctx, logo):
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    reason = ctx["details"].get("withdrawal_dismissed_reason") or ""
    d = _stage_letter(ctx, logo, ctx["provider"], "Dismissal")
    d.para(f"{L['org_name']} ({org}) has dismissed the claims dispute submitted by your organization for the case "
           f"noted above.")
    if reason and reason not in ("N/A", "Other"):
        d.para(f"Reason for dismissal: {reason}.")
    d.para(f"{org} will take no further action on this dispute. The Health Plan copied on this correspondence does "
           f"not need to take any action.")
    _contact_close(d)
    return d


def build_withdrawal(ctx, logo):
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    when = ctx["dates"].get("withdrawal_requested_at")
    d = _stage_letter(ctx, logo, ctx["provider"], "Withdrawal Request")
    d.para(f"{L['org_name']} ({org}) received your organization’s request{' on ' + long_date(when) if when else ''} to "
           f"withdraw the claims dispute noted above. The dispute has been withdrawn and the case is closed.")
    d.para(f"{org} will take no further action on this dispute. The Health Plan copied on this correspondence does "
           f"not need to take any action.")
    _contact_close(d)
    return d


def build_plan_notification(ctx, logo):
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    prov, plan = ctx["provider"], ctx["plan"]
    d = _stage_letter(ctx, logo, plan, "Notification of Claims Dispute")
    amt = f", with an amount in dispute of {money(ctx['disputed_cents'])}" if ctx.get("disputed_cents") else ""
    d.para(f"A claims dispute has been submitted to {L['org_name']} ({org}) by {prov['name']} and involves "
           f"{plan['name']} under the {L.get('state', 'Florida')} {L.get('agency_name', '')} Claims Dispute "
           f"Resolution program. The dispute concerns {counted(ctx['num_claims'], 'claim')}{amt}. {org} has conducted "
           f"an initial review and has accepted this case for review.")
    d.para(f"{plan['name']} must respond with all supporting documentation within 15 calendar days after receiving "
           f"this notice. Please complete the enclosed Health Plan Response Form and submit it, with your supporting "
           f"documentation, through the secure upload link in the email that accompanies this letter. Please do not "
           f"email documentation to {org}.")
    d.para(f"FAILURE TO SUBMIT ALL SUPPORTING DOCUMENTATION TO {org.upper()} WITHIN 15 CALENDAR DAYS SHALL RESULT IN "
           f"IMMEDIATE ISSUANCE OF A DEFAULT DETERMINATION RECOMMENDATION TO THE "
           f"{L.get('agency_name', 'AGENCY FOR HEALTH CARE ADMINISTRATION').upper()} FULLY IN FAVOR OF "
           f"{prov['name'].upper()}, AND AGAINST {plan['name'].upper()}.", bold=True)
    _contact_close(d)
    return d


def build_plan_optout(ctx, logo):
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    d = _stage_letter(ctx, logo, ctx["provider"], "Plan Opt-out")
    d.para(f"{L['org_name']} ({org}) received the Health Plan documentation for the case noted above. "
           f"{ctx['plan']['name']} has responded to {org} declining to participate in the claims dispute resolution "
           f"process.")
    d.para(f"Because the Health Plan has opted out, {org} cannot complete a review of this dispute and the case is "
           f"closed. No further action will be taken on this dispute request.")
    _contact_close(d)
    return d


def build_rfi(ctx, logo):
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    rfi = ctx.get("rfi")
    if not rfi or not rfi.get("items"):
        raise LetterDataMissing("there is no request for information with items on this case: create the request "
                                "first (Document requests on the case)")
    to = ctx["plan"] if (rfi.get("side") or "").upper() in ("HEALTH_PLAN", "PLAN") else ctx["provider"]
    d = _stage_letter(ctx, logo, to, "Request for Additional Documentation")
    d.para(f"{L['org_name']} ({org}) is requesting additional information to continue its review of the case noted "
           f"above. Please provide the following:")
    d.bullets([str(i) for i in rfi["items"]])
    if rfi.get("message"):
        d.text_block(rfi["message"])
    due = rfi.get("due_date")
    d.para(f"Please upload all requested documentation within 15 calendar days of this request"
           f"{' (by ' + long_date(due) + ')' if due else ''}, using the secure upload link in the email that "
           f"accompanies this letter. If any of the required documentation cannot be provided, please submit an "
           f"explanation in lieu thereof as to why it could not be provided.")
    _contact_close(d)
    return d


def build_default_determination(ctx, logo):
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    prov, plan = ctx["provider"], ctx["plan"]
    rep = ctx.get("report") or {}
    notified = ctx["dates"].get("plan_notified_at") or ctx["dates"].get("date_acknowledgment_letter_sent_to_health_plan")
    d = Letter(ctx, header_line=ctx["case_number"], logo=logo)
    d.heading(L["org_name"].upper() + ".", center=True, space_after=0)
    d.heading(L.get("program_title", ""), center=True, space_after=10)
    d.two_col(f"CASE NO: {ctx['case_number']}", "DEFAULT DETERMINATION")
    amount = rep.get("awarded_cents") if rep.get("awarded_cents") is not None else ctx.get("disputed_cents")
    award = f" and the amount of {money(amount)} be awarded to the Provider" if amount else ""
    d.para(f"{L['org_name']} ({org}) recommends a default determination fully in favor of {prov['name']} and against "
           f"{plan['name']}{award}.", bold=True, lead="Summary:")
    d.heading("Rationale:")
    d.para(f"{org} accepted this dispute for review and notified {plan['name']} of the dispute"
           f"{' on ' + long_date(notified) if notified else ''}, requesting all supporting documentation within 15 "
           f"calendar days. The notice stated that failure to submit all supporting documentation within 15 calendar "
           f"days shall result in the immediate issuance of a default determination recommendation to the "
           f"{L.get('agency_name', 'Agency')} fully in favor of the Provider. {plan['name']} did not submit its "
           f"supporting documentation within that time.")
    d.para(f"Based on the foregoing, {org} recommends a default determination fully in favor of the Provider{award}.")
    d.sign_off()
    return d


def build_plan_packet(ctx, logo):
    """The Health Plan Response Form: what the plan fills in and returns with its documentation."""
    L, org = ctx["letters"], ctx["letters"]["org_short"]
    d = Letter(ctx, header_line=ctx["case_number"], logo=logo)
    d.heading("HEALTH PLAN RESPONSE FORM", center=True, space_after=2)
    d.heading(L.get("program_title", ""), center=True, space_after=10)
    d.grid(["", ""], [["Case Number", ctx["case_number"]], ["Provider", ctx["provider"]["name"]],
                      ["Health Plan", ctx["plan"]["name"]],
                      ["Amount in dispute", money(ctx["disputed_cents"]) if ctx.get("disputed_cents") else ""],
                      ["Number of claims", str(ctx["num_claims"])]], widths=[2.0, 4.5], size=10)
    d.para(f"Please complete every item below and return this form, with all supporting documentation, through the "
           f"secure upload link within 15 calendar days of the notice of this dispute.")
    d.grid(["Item", "Health Plan response"],
           [["Health Plan contact name, title, email and phone", ""],
            ["Does the Health Plan agree to participate in the dispute resolution process? (Yes / No — if No, "
             "state the reason)", ""],
            ["Line of business (Medicaid, Commercial, Medicare Advantage, Marketplace, Other)", ""],
            ["Is the provider contracted with the Health Plan for these services? (Yes / No)", ""],
            ["Amount paid to date on the claim(s)", ""],
            ["The Health Plan’s position on the dispute (attach a statement)", ""],
            ["Supporting documentation attached (contract, fee schedule, medical policy, claim history, "
             "correspondence)", ""],
            ["Name, title and signature of the person completing this form, and the date", ""]],
           widths=[3.5, 3.0], size=10)
    d.para(f"FAILURE TO SUBMIT ALL SUPPORTING DOCUMENTATION TO {org.upper()} WITHIN 15 CALENDAR DAYS SHALL RESULT "
           f"IN IMMEDIATE ISSUANCE OF A DEFAULT DETERMINATION RECOMMENDATION FULLY IN FAVOR OF THE PROVIDER.", bold=True, size=9)
    return d


# key -> (builder, needs extra filename fields)
BUILDERS = {
    "final_order_rationale": build_decision_report,
    "cover_letter_final_notice": build_cover_letter,
    "acceptance_letter": build_acceptance,
    "ineligible_letter": build_ineligible,
    "dismissal_letter": build_dismissal,
    "withdrawal_letter": build_withdrawal,
    "plan_notification_letter": build_plan_notification,
    "plan_optout_letter": build_plan_optout,
    "rfi_letter": build_rfi,
    "default_determination": build_default_determination,
    "fl_plan_packet": build_plan_packet,
}


def has_builder(key: str) -> bool:
    return key in BUILDERS or key in ("invoice_health_plan", "invoice_provider")


def build(dsn: str, minio, template_bucket: str, tenant: str, case_id: str, key: str) -> tuple[bytes, dict]:
    """DOCX bytes for a built-in letter, and the extra fields its filename and subject use."""
    ctx = load_context(dsn, tenant, case_id)
    logo = _logo(minio, template_bucket, tenant)
    if key == "invoice_health_plan":
        d, extra = build_invoice(ctx, "HEALTH_PLAN", logo)
    elif key == "invoice_provider":
        d, extra = build_invoice(ctx, "PROVIDER", logo)
    else:
        d, extra = BUILDERS[key](ctx, logo), {}
    return d.save(), extra
