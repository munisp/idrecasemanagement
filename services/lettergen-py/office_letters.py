"""The office's own letters (assets/office), filled from the case.

The Capitol Bridge Word letters themselves -- converted once by tools/make_office_templates.py, each blank a
{field} -- are what every FL letter is made from now (a tenant's uploaded template still wins; without one of either,
builtin_letters builds the letter in code):

  acceptance_letter          acceptance.docx            Results of Preliminary Review — accepted
  partial_acceptance_letter  partial_acceptance.docx    — partially accepted (the claims marked ineligible)
  ineligible_letter          ineligible.docx            — ineligible
  dismissal_letter           dismissal.docx
  withdrawal_letter          withdrawal.docx
  plan_optout_letter         plan_optout.docx
  plan_notification_letter   plan_notification.docx     Notify Health Plan of Acceptance
  fl_plan_packet             fl_plan_packet.pdf         the FL Plan Packet, sent as the office's own PDF form
  cover_letter_final_notice  cover_to_rationale.docx    the cover to the Agency's attorney: who the parties are
  final_order_rationale      rationale*.docx            the rationale and letter: overturned or upheld; partially
                                                        overturned; default (the plan did not respond)
  default_determination      rationale_default.docx
  invoice_health_plan,       invoice_full.docx          100% of the review cost to one party (full award / no award)
  invoice_provider           invoice_split.docx         a share of it (partial overturn: split invoices)

Every field comes from the same case context builtin_letters loads; what a letter cannot do without (an outcome, an
invoice) is refused with builtin_letters.LetterDataMissing, as the built-in letters are.
"""
from __future__ import annotations

import os
from datetime import timedelta

import psycopg

import builtin_letters as bl
import docxfill

OFFICE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "assets", "office")

KEYS = {"acceptance_letter", "partial_acceptance_letter", "ineligible_letter", "dismissal_letter", "withdrawal_letter",
        "plan_optout_letter", "plan_notification_letter", "fl_plan_packet", "cover_letter_final_notice",
        "final_order_rationale", "default_determination", "invoice_health_plan", "invoice_provider"}

STATIC = {"acceptance_letter": "acceptance", "partial_acceptance_letter": "partial_acceptance",
          "ineligible_letter": "ineligible", "dismissal_letter": "dismissal", "withdrawal_letter": "withdrawal",
          "plan_optout_letter": "plan_optout", "plan_notification_letter": "plan_notification",
          "cover_letter_final_notice": "cover_to_rationale", "default_determination": "rationale_default"}

# The standing dismissal reason of the program: no authorization (permission) to proceed within the 15 days.
DISMISSAL_REASON = ("Authorization to proceed with the full review was not received within fifteen (15) calendar "
                    "days of the acceptance and cost estimate.")
SUBJECT = "External Review of Denied Claims"
# The remit-to lines the office's invoices carry, used until Finance sets them on the Billing page.
REMIT_DEFAULT = {"bank": "First Business Bank", "routing": "075905787", "account": "164474499"}


def has_template(key: str) -> bool:
    return key in KEYS


def _mmddyyyy(v) -> str:
    d = bl.as_date(v)
    return d.strftime("%m/%d/%Y") if d else ""


def _split_address(lines: list[str]) -> tuple[str, str]:
    lines = [x for x in (lines or []) if x]
    if len(lines) >= 2:
        return ", ".join(lines[:-1]), lines[-1]
    return (lines[0], "") if lines else ("", "")


def _party_fields(prefix: str, p: dict) -> dict:
    a1, city = _split_address(p.get("address"))
    contact = p.get("contact") or ""
    return {f"{prefix}_business": p.get("name") or "", f"{prefix}_contact": contact,
            f"{prefix}_attn": f"ATTN: {contact}" if contact else "", f"{prefix}_contact_title": p.get("title") or "",
            f"{prefix}_address1": a1, f"{prefix}_city_state_zip": city, f"{prefix}_email": p.get("email") or "",
            f"{prefix}_salutation": contact or p.get("name") or ""}


def _extra(dsn: str, ctx: dict) -> dict:
    """What the office letters need that the built-in context does not load: the case's status and outcome, the
    claims' eligibility, the plan's day-13 reminder, the late-fee terms."""
    t, cid = ctx["tenant"], ctx["case_id"]
    with psycopg.connect(dsn) as c:
        st = c.execute(f"SELECT coalesce(internal_status,''), coalesce(details->>'case_outcome','') FROM tenant_{t}.cases WHERE id=%s",
                       (cid,)).fetchone() or ("", "")
        claims = c.execute("""SELECT coalesce(claim_number,''), coalesce(status,'') FROM public.case_claims
                              WHERE tenant=%s AND case_id=%s ORDER BY claim_number""", (t, cid)).fetchall()
        rem = c.execute("""SELECT min(created_at) FROM public.case_activities WHERE tenant=%s AND case_id=%s
                             AND type='FOLLOWUP_ACTION' AND body ILIKE '%%reminder%%'""", (t, cid)).fetchone()
        fees = c.execute("SELECT config->'fees'->'late_fee' FROM public.program_rules WHERE tenant=%s", (t,)).fetchone()
    return {"internal_status": st[0], "case_outcome": st[1], "claim_status": claims,
            "reminder_at": rem[0] if rem else None, "late_fee": (fees[0] if fees else None) or {}}


def _is_default(ctx: dict, ex: dict) -> bool:
    return ex["case_outcome"] == "Default Award" or ex["internal_status"] in ("Plan - No Response", "Default Determination")


def template_for(key: str, ctx: dict, ex: dict) -> str | None:
    """The office letter a key is made from on this case; None leaves it to the built-in letter."""
    if key in STATIC:
        return STATIC[key]
    if key == "final_order_rationale":
        if bl.pasted_report(ctx.get("report")):
            return None  # the pasted report prints as written (builtin_letters.build_pasted_report)
        if _is_default(ctx, ex):
            return "rationale_default"
        rep = bl._decision(ctx)
        if rep.get("report_type") == "IRO":
            return None  # the external review decision keeps its own layout
        return "rationale_partial" if rep["outcome"] == "PARTIALLY_OVERTURNED" else "rationale"
    if key in ("invoice_health_plan", "invoice_provider"):
        inv = bl.invoice_for(ctx, "HEALTH_PLAN" if key == "invoice_health_plan" else "PROVIDER")
        return "invoice_full" if inv["share"] >= 100 else "invoice_split"
    return None


def _common(ctx: dict) -> dict:
    prov, plan = ctx["provider"], ctx["plan"]
    dos = sorted({bl.as_date(c[4]) for c in ctx["claims"] if c[4]} - {None})
    members = {(c[5] or "", c[6] or "") for c in ctx["claims"] if (c[5] or c[6])}
    f = {"today": bl.long_date(ctx["today"]), "case_number": ctx["case_number"],
         "provider_name": prov["name"], "plan_name": plan["name"],
         "provider_name_upper": prov["name"].upper(), "plan_name_upper": plan["name"].upper(),
         "dates_of_service": (_mmddyyyy(dos[0]) if len(dos) == 1 else f"Multiple ({len(dos)})" if dos else
                              _mmddyyyy(ctx["details"].get("date_of_service")) or ""),
         "member_names": (" ".join(x for x in next(iter(members)) if x) if len(members) == 1 else
                          f"Multiple ({len(members)})" if members else ctx["details"].get("member_name", "")),
         "claims_count": bl.counted(ctx["num_claims"], "claim"),
         "patients_count": bl.counted(ctx["num_patients"], "patient"),
         "claims_filed_by": f"{bl.counted(ctx['num_claims'], 'claim')} filed by {prov['name']}",
         "amount_in_controversy": bl.money(ctx.get("disputed_cents")) or "$0.00",
         "case_subject": SUBJECT,
         "claims_that_comprise": "The claims that comprise this case were" if ctx["num_claims"] != 1
         else "The claim that comprises this case was"}
    f.update(_party_fields("provider", prov))
    f.update(_party_fields("plan", plan))
    d = ctx["dates"]
    accepted = d.get("date_case_accepted") or d.get("date_acknowledgment_letter_sent_to_provider")
    notified = d.get("plan_notified_at") or d.get("date_acknowledgment_letter_sent_to_health_plan")
    f["provider_notice_date"] = f["initial_review_date"] = bl.long_date(accepted)
    f["plan_notice_date"] = bl.long_date(notified)
    due = d.get("date_health_plan_response_due") or (bl.as_date(notified) + timedelta(days=15) if bl.as_date(notified) else None)
    f["plan_due_date"] = bl.long_date(due)
    return f


def _reviewers(rep: dict | None) -> str:
    who = [w for w in ((rep or {}).get("reviewers") or []) if w] or ["attorney", "coding consultant"]
    art = lambda w: "an" if w[0].lower() in "aeiou" else "a"  # noqa: E731
    if len(who) == 1:
        return f"{art(who[0])} {who[0]}"
    return f"{art(who[0])} {who[0]} and {art(who[1])} {', '.join(who[1:])}"


def _decision_fields(ctx: dict, f: dict) -> dict:
    rep = bl._decision(ctx)
    org, sec = ctx["letters"]["org_short"], rep.get("sections") or {}
    rec = bl.recommendation(ctx, rep)
    f["determination_summary"] = rec.removeprefix(f"{org} recommends ").rstrip(".")
    f["decision_text"] = bl.recommendation(ctx, rep, verb="has determined that").removeprefix(f"{org} has determined ").rstrip(".")
    aw, paid = rep.get("awarded_cents"), rep.get("already_paid_cents") or 0
    f["case_determination"] = ("should be upheld." if rep["outcome"] == "UPHELD" else
                               f"should be overturned and the amount of {bl.money(aw)} be awarded to the Provider.")
    f["award_total"], f["already_paid"], f["award_additional"] = bl.money(aw), bl.money(paid), bl.money(max((aw or 0) - paid, 0))
    f["claim_no"] = bl._claim_no(ctx, rep)
    f["claim_summary"] = sec.get("claim_summary") or (
        f"This case concerns {bl.counted(ctx['num_claims'], 'claim')} brought by {ctx['provider']['name']} for services "
        f"rendered to {bl.counted(ctx['num_patients'], 'patient')} enrolled in {ctx['plan']['name']}.")
    rationale = []
    if sec.get("dispute_issue"):
        rationale.append(sec["dispute_issue"])
    for k, title in (("provider_position", "Provider’s Position:"), ("plan_position", "Health Plan’s Position:"),
                     ("applicable_law", "Applicable Law:")):
        if sec.get(k):
            rationale += [("b", title), sec[k]]
    if sec.get("analysis"):
        rationale.append(sec["analysis"])
    f["rationale"] = rationale or [sec.get("closing_note") or ""]
    f["physician"] = bool(sec.get("physician_review"))
    f["physician_review"] = sec.get("physician_review") or ""
    priced = [c for c in (rep.get("claims") or [])
              if any(c.get(k) is not None for k in ("ucr_cents", "lesser_cents", "reimbursement_cents"))]
    f["coding_rows"] = [{"dos": c.get("dos", ""), "code": c.get("cpt", ""), "billed": bl.money(c.get("billed_cents")),
                         "allowed": bl.money(c.get("paid_cents")), "medicare200": bl.money(c.get("ucr_cents")),
                         "additional": bl.money(c.get("reimbursement_cents"))} for c in priced]
    text = (sec.get("coding_review") or sec.get("pricing_method") or "").strip()
    f["coding"] = bool(text or priced)
    if text and (text[0].islower() or text.lower().startswith("that ")):
        text = f"The {org} coding consultant indicated {text}"
    f["coding_review_paragraph"] = text
    f["case_file_summary"] = sec.get("case_file_summary") or sec.get("claim_summary") or f["claim_summary"]
    f["reviewers_phrase"] = _reviewers(rep)
    return f


def _default_fields(ctx: dict, ex: dict, f: dict) -> dict:
    rep = ctx.get("report") or {}
    amount = rep.get("awarded_cents") if rep.get("awarded_cents") is not None else ctx.get("disputed_cents")
    f["award_amount"] = bl.money(amount) or "$0.00"
    f["claim_no"] = bl._claim_no(ctx, rep) if rep else ("ALL" if ctx["num_claims"] != 1 else
                                                         next((c[0] for c in ctx["claims"] if c[0]), "ALL"))
    # The day-13 reminder: when the case recorded it, otherwise the day it was due.
    notified = bl.as_date(ctx["dates"].get("plan_notified_at") or ctx["dates"].get("date_acknowledgment_letter_sent_to_health_plan"))
    reminder = bl.as_date(ex["reminder_at"]) or (notified + timedelta(days=13) if notified else None)
    f["plan_call_date"] = f["plan_reminder_date"] = bl.long_date(reminder)
    f["reviewers_phrase"] = _reviewers(rep)
    return f


def _invoice_fields(ctx: dict, ex: dict, party: str, f: dict) -> tuple[dict, dict]:
    inv = bl.invoice_for(ctx, party)
    who = ctx["plan"] if party == "HEALTH_PLAN" else ctx["provider"]
    p = _party_fields("bill", who)
    f.update({k.replace("bill_contact_title", "bill_title"): v for k, v in p.items()})
    terms, late = ctx["letters"]["invoice"], ex["late_fee"] or {}
    fee = int(late.get("cents") or terms.get("late_fee_cents") or 5000)
    after = int(late.get("after_days") or terms.get("late_after_days") or 35)
    every = int(late.get("every_days") or terms.get("late_every_days") or 30)
    amt = inv["amount_cents"]
    remit = {**REMIT_DEFAULT, **{k: v for k, v in (ctx["letters"].get("remit") or {}).items() if v}}
    f.update({"date_prepared": _mmddyyyy(ctx["today"]), "invoice_number": inv["number"],
              "responsible_party": "Plan" if party == "HEALTH_PLAN" else "Provider", "share_pct": str(inv["share"]),
              "review_cost": bl.money(amt), "late_fee": bl.money(fee).replace(".00", ""), "late_after": str(after),
              "late_every": str(every), "late_after2": str(after + every),
              "total_late1": bl.money(amt + fee), "total_late2": bl.money(amt + 2 * fee),
              "remit_bank": remit["bank"], "remit_routing": remit["routing"], "remit_account": remit["account"]})
    return f, {"invoice_number": inv["number"]}


def _stage_fields(key: str, ctx: dict, ex: dict, f: dict) -> dict:
    det = ctx["details"]
    if key == "ineligible_letter":
        el = ctx.get("eligibility") or {}
        if el.get("result") != "INELIGIBLE":
            raise bl.LetterDataMissing("the latest eligibility review did not find this dispute ineligible")
        import re
        reasons = [r for r in re.split(r"[,;]\s*", el.get("reason") or "") if r]
        out = [bl.ELIG_REASON_TEXT.get(r, r.replace("_", " ").capitalize()) for r in reasons]
        ev = el.get("evidence") or {}
        if "below_threshold" in reasons and ev.get("threshold_min_cents"):
            out.append(f"The minimum amount in dispute for this type of provider is {bl.money(ev['threshold_min_cents'])}.")
        f["ineligible_reasons"] = out or ["See the enclosed review."]
    elif key == "dismissal_letter":
        r = (det.get("withdrawal_dismissed_reason") or "").strip()
        f["dismissal_reasons"] = r if r and r not in ("N/A", "Other") else DISMISSAL_REASON
    elif key == "withdrawal_letter":
        f["withdrawal_request_date"] = bl.long_date(ctx["dates"].get("withdrawal_requested_at")) or f["today"]
    elif key == "plan_notification_letter":
        f["provider_delivery"] = det.get("plan_copy_method") or "email"
        t = det.get("plan_copy_tracking")
        f["provider_tracking"] = f" (tracking # {t})" if t else ""
    elif key == "partial_acceptance_letter":
        bad = [n for n, s in ex["claim_status"] if s == "INELIGIBLE" and n]
        good = [n for n, s in ex["claim_status"] if s != "INELIGIBLE" and n]
        if not bad or not good:
            raise bl.LetterDataMissing("mark which claims are ineligible (Generate → Partial acceptance letter asks "
                                       "for them); at least one claim must stay accepted")
        join = lambda xs: ", ".join(xs[:-1]) + " and " + xs[-1] if len(xs) > 1 else xs[0]  # noqa: E731
        f["accepted_claims"], f["ineligible_claims"] = join(good), join(bad)
        f["partial_ineligible_reason"] = (det.get("partial_ineligible_reason") or "").strip() or \
            "are ineligible for review under the program."
    return f


def build(dsn: str, tenant: str, case_id: str, key: str):
    """(bytes, kind, extra): the filled letter as DOCX ("docx") or the office's PDF as is ("pdf"), and the extra
    filename fields. None when the key has no office letter on this case."""
    if key == "fl_plan_packet":
        with open(os.path.join(OFFICE, "fl_plan_packet.pdf"), "rb") as fh:
            return fh.read(), "pdf", {}
    ctx = bl.load_context(dsn, tenant, case_id)
    ex = _extra(dsn, ctx)
    name = template_for(key, ctx, ex)
    if name is None:
        return None
    f, extra = _common(ctx), {}
    if name in ("rationale", "rationale_partial"):
        _decision_fields(ctx, f)
    elif name == "rationale_default":
        _default_fields(ctx, ex, f)
    elif name in ("invoice_full", "invoice_split"):
        f, extra = _invoice_fields(ctx, ex, "HEALTH_PLAN" if key == "invoice_health_plan" else "PROVIDER", f)
    else:
        _stage_fields(key, ctx, ex, f)
    with open(os.path.join(OFFICE, name + ".docx"), "rb") as fh:
        return docxfill.fill(fh.read(), f), "docx", extra
