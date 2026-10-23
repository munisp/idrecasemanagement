"""Turn the office's Word letters (office_src/*.docx, exactly as the office wrote them) into the templates lettergen
fills (assets/office/*.docx): each highlighted blank -- "[insert provider name]", "Month Day, Year", "MM/DD/YYYY",
the sample case in the opt-out letter -- becomes a {field}, and nothing else in the document changes. Run it again
whenever the office changes a letter:

    python tools/make_office_templates.py        (from services/lettergen-py)

It reports, per letter, any highlighted text or "[...]" blank left over, so a new blank in a changed letter is seen.
Field names are those office_letters.py fills.
"""
from __future__ import annotations

import os
import re
import sys

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, HERE)

from docx import Document  # noqa: E402
from docx.oxml.ns import qn  # noqa: E402

import docxfill as dx  # noqa: E402

SRC, OUT = os.path.join(HERE, "office_src"), os.path.join(HERE, "assets", "office")

# The blanks every letter to the provider shares (the address block, the RE lines, the salutation, the cc).
PROVIDER_LETTER = [
    ("ATTN: [Provider Contact Name]", "{provider_attn}"),
    ("[ATTN: Provider Contact Name]", "{provider_attn}"),
    ("[Provider Contact Business Name]", "{provider_business}"),
    ("[Provider Contact Busines", "{provider_business}"),
    ("Dear [Provider Contact Full Name]:", "Dear {provider_salutation}:"),
    ("Dear [Provider Contact Name]:", "Dear {provider_salutation}:"),
    ("[Provider Contact Name]", "{provider_contact}"),
    ("[Address Line 1]", "{provider_address1}"),
    ("[City, State, Zip Code]", "{provider_city_state_zip}"),
    ("[Email address]", "{provider_email}"),
    ("Provider: [insert provider name]", "Provider: {provider_name}"),
    ("Health Plan: [insert plan name]", "Health Plan: {plan_name}"),
    ("Date(s) of Service: MM/DD/YYYY", "Date(s) of Service: {dates_of_service}"),
    ("Member Name(s): [insert member name]", "Member Name(s): {member_names}"),
    ("Capitol Bridge Case No.: [insert case #]", "Capitol Bridge Case No.: {case_number}"),
    ("[insert plan name]", "{plan_name}"),
    ("[insert Plan name]", "{plan_name}"),
    ("Month Day, Year", "{today}"),
]

# The decision: the rationale (page 1) and the letter to the Agency that goes with it (page 2).
DECISION_COMMON = [
    ("CASE NO: [insert case number]", "CASE NO: {case_number}"),
    ("CLAIM NO: [insert claim number or “ALL”]", "CLAIM NO: {claim_no}"),
    ("On [insert date the Acknowledgement letter/acceptance was sent] Capitol Bridge notified [insert provider name] "
     "that the case was accepted for review and on [insert date the health plan acceptance was sent] Capitol Bridge "
     "notified [insert plan name] of said acceptance.",
     "On {provider_notice_date} Capitol Bridge notified {provider_name} that the case was accepted for review and on "
     "{plan_notice_date} Capitol Bridge notified {plan_name} of said acceptance."),
    ("[insert rationale]", "{rationale}"),
    ("The Capitol Bridge coding consultant indicated [insert thorough summary of coding consultant analysis and use "
     "table if needed]", "{coding_review_paragraph}"),
    ("The chart below provides a comparison of billed charges and reimbursement rates for the codes and dates of "
     "service at issue. [Edit table as needed to present analysis]",
     "{?coding_rows}The chart below provides a comparison of billed charges and reimbursement rates for the codes and "
     "dates of service at issue."),
    ("RE: [insert subject of case IE: External Review of Denied Claims]", "RE: {case_subject}"),
    ("Provider: [insert provider name]", "Provider: {provider_name}"),
    ("Health Plan: [insert plan name]", "Health Plan: {plan_name}"),
    ("Capitol Bridge Case Number: [insert case number]", "Capitol Bridge Case Number: {case_number}"),
    ("This case involved [insert number of claims filed by name of filing party.]", "This case involved {claims_filed_by}."),
    ("The amount in controversy is [insert $ amount].", "The amount in controversy is {amount_in_controversy}."),
    ("The amount in controversy is [insert $ disputed amount].", "The amount in controversy is {amount_in_controversy}."),
    ("The claims that comprise this case were reviewed by an [attorney] and a [coding consultant, physician, nurse "
     "consultant, administrative reviewers]", "{claims_that_comprise} reviewed by {reviewers_phrase}"),
    ("This case involves [insert a more detailed summary than the summary above, See examples in Materials from FL "
     "folder in ShareFile]", "{case_file_summary}"),
]

# A coding table's first blank row is the row repeated per priced claim; the second blank row goes.
CODING_TABLE = ("table", 0, ["{#coding_rows}{r.dos}", "{r.code}", "{r.billed}", "{r.allowed}", "{r.medicare200}",
                             "{r.additional}"])

SPECS: dict[str, list] = {
    "acceptance": PROVIDER_LETTER,
    "ineligible": [("[insert reason the dispute is ineligible]", "{ineligible_reasons}")] + PROVIDER_LETTER,
    "dismissal": [("[insert “The application submission is over a year over the final denial of the disputed amount.” "
                   "Or other reason for dismissal]", "{dismissal_reasons}")] + PROVIDER_LETTER,
    "withdrawal": [("your email dated Month Day, Year", "your email dated {withdrawal_request_date}")] + PROVIDER_LETTER,
    "partial_acceptance": [
        ("Capitol Bridge Case No.: ", "Capitol Bridge Case No.: {case_number}"),
        ("Claim [Insert Claim Number(s)] have been accepted for review.", "Claim {accepted_claims} have been accepted for review."),
        ("Claim [Insert Claim Number(s)] [insert reason the dispute is ineligible]", "Claim {ineligible_claims} {partial_ineligible_reason}"),
        ("Claim numbers [Insert Claim Number(s)]", "Claim numbers {accepted_claims}"),
    ] + PROVIDER_LETTER,
    # The office's opt-out letter is a filled-in one: its sample case is what becomes the fields.
    "plan_optout": [
        ("October 15, 2026", "{today}"),
        ("ATTN: Kathy J. Tayon", "{provider_attn}"),
        ("Provider: Roberson Allergy & Asthma, Inc.", "Provider: {provider_name}"),
        ("Roberson Allergy & Asthma, Inc.", "{provider_business}"),
        ("1203 Barons Way", "{provider_address1}"),
        ("Deland, FL 32724", "{provider_city_state_zip}"),
        ("Kathy.tayon@tayonlaw.com", "{provider_email}"),
        ("Health Plan: Sunshine Heatlh", "Health Plan: {plan_name}"),
        ("Date(s) of Service: Multiple (62)", "Date(s) of Service: {dates_of_service}"),
        ("Member Name(s): Multiple (62)", "Member Name(s): {member_names}"),
        ("Capitol Bridge Case No.: FL26-348", "Capitol Bridge Case No.: {case_number}"),
        ("Dear Kathy J. Tayon:", "Dear {provider_salutation}:"),
        ("Sunshine Health", "{plan_name}"),
    ],
    "plan_notification": [
        ("Capitol Bridge Case No. [insert case #]", "Capitol Bridge Case No. {case_number}"),
        ("via [insert delivery method from provider] [insert tracking # if applicable].", "via {provider_delivery}{provider_tracking}."),
        ("[INSERT PROVIDER NAME]", "{provider_name_upper}"),
        ("[INSERT PLAN NAME]", "{plan_name_upper}"),
        ("[insert provider organization name]", "{provider_business}"),
        ("[insert provider name]", "{provider_name}"),
        ("[insert plan name]", "{plan_name}"),
        ("Month Day, Year", "{today}"),
    ],
    "cover_to_rationale": [
        ("RE: FL2X-XXX", "RE: {case_number}"),
        ("Month Day, Year", "{today}"),
        ("[Provider Contact Name]", "{provider_contact}"),
        ("[Provider Contact Business Name]", "{provider_business}"),
        ("next", "[Address Line 1]", "{provider_address1}"),
        ("next", "[City, State, Zip Code]", "{provider_city_state_zip}"),
        ("[email address]", "{provider_email}"),
        ("[Plan Contact Business Name]", "{plan_business}"),
        ("[Plan Contact Name]", "{plan_contact}"),
        ("[Plan Contact Position/Title]", "{plan_contact_title}"),
        ("next", "[Address Line 1]", "{plan_address1}"),
        ("next", "[City, State, Zip Code]", "{plan_city_state_zip}"),
        ("[Plan email address]", "{plan_email}"),
    ],
    "rationale": [
        ("Summary: Capitol Bridge LLC (Capitol Bridge) recommends [insert summary of determination].",
         "Summary: Capitol Bridge LLC (Capitol Bridge) recommends {determination_summary}."),
        ("Capitol Bridge recommends [insert summary of determination].", "Capitol Bridge recommends {determination_summary}."),
        ("[insert claim summary]", "{claim_summary}"),
        ("Coding Consultant Review: [remove section if no coding consultant reviewed]", "{?coding}Coding Consultant Review: "),
        ("completed its initial review of this case on [Month Day, Year] and", "completed its initial review of this case on {initial_review_date} and"),
        ("[Month Day, Year]", "{/coding_rows}{/coding}{today}"),
        ("Capitol Bridge LLC. (Capitol Bridge) has determined that this case [insert determination]",
         "Capitol Bridge LLC. (Capitol Bridge) has determined that this case {case_determination}"),
        ("Capitol Bridge has determined [insert determination].", "Capitol Bridge has determined {decision_text}."),
    ] + DECISION_COMMON + [CODING_TABLE],
    "rationale_partial": [
        ("$[Insert the total, the already paid amount + additional award]", "{award_total}"),
        ("$[insert the total, the already paid amount + additional award]", "{award_total}"),
        ("$[Insert the amount already paid]", "{already_paid}"),
        ("$[insert the amount already paid]", "{already_paid}"),
        ("$[Insert the additional amount to be awarded to provider (based on coders findings)]", "{award_additional}"),
        ("$[insert the additional amount to be awarded to provider]", "{award_additional}"),
        ("This case concerns [insert number of claims filed by name of filing party] claims brought by a provider for "
         "services provided to [insert total number of patients] patients enrolled in the Health Plan.",
         "This case concerns {claims_count} brought by a provider for services provided to {patients_count} enrolled "
         "in the Health Plan."),
        ("Independent Physician Review: [remove section if no Physician reviewed]", "{?physician}Independent Physician Review: "),
        ("[insert claim number in question]", "{physician_review}"),
        ("Coding Consultant Review: [remove section if no coding consultant reviewed]", "{/physician}{?coding}Coding Consultant Review: "),
        ("Based on the foregoing, Capitol Bridge recommends", "{/coding_rows}{/coding}Based on the foregoing, Capitol Bridge recommends"),
        ("[Month Day, Year]", "{today}"),
        ("completed its initial review of this case on [insert date the Acknowledgement letter/acceptance was sent] and",
         "completed its initial review of this case on {initial_review_date} and"),
    ] + DECISION_COMMON + [CODING_TABLE],
    "rationale_default": [
        ("CASE NO: FL2X-XXX", "CASE NO: {case_number}"),
        ("CLAIM NO: [insert claim no. Or ALL]", "CLAIM NO: {claim_no}"),
        ("This case concerns X claims submitted by the provider, [insert Provider name], for services provided to X "
         "patients enrolled in [insert Plan name].",
         "This case concerns {claims_count} submitted by the provider, {provider_name}, for services provided to "
         "{patients_count} enrolled in {plan_name}."),
        ("On [insert long form sent date]", "On {plan_notice_date}"),
        ("[insert long form due date]", "{plan_due_date}"),
        ("[insert long form call date]", "{plan_call_date}"),
        ("[insert long form email date]", "{plan_reminder_date}"),
        ("a default is entered against Cigna Health Care of Florida, Inc.,", "a default is entered against {plan_name},"),
        ("[insert long form date]", "{today}"),
        ("Capitol Bridge Case Number: FL2X-XXX", "Capitol Bridge Case Number: {case_number}"),
        ("This case involves X claims filed by [insert Provider name].", "This case involves {claims_count} filed by {provider_name}."),
        ("The amount in controversy is $XXX.XX.", "The amount in controversy is {amount_in_controversy}."),
        ("$ XXX.XX", "{award_amount}"),
        ("$XXX.XX", "{award_amount}"),
        ("[insert long form Acceptance date]", "{initial_review_date}"),
        ("The claims that comprise this case were reviewed by an [attorney] and a [coding consultant, physician, nurse "
         "consultant, administrative reviewers]", "{claims_that_comprise} reviewed by {reviewers_phrase}"),
        ("This case involves X claims submitted by [insert Provider name] for services provided to X patients enrolled "
         "in [insert Plan name].",
         "This case involves {claims_count} submitted by {provider_name} for services provided to {patients_count} "
         "enrolled in {plan_name}."),
        ("[insert Provider name]", "{provider_name}"),
        ("[insert Plan name]", "{plan_name}"),
    ],
}

INVOICE = [
    ("MM/DD/YYYY", "{date_prepared}"),
    ("FL2X-0XX", "{invoice_number}"),
    ("FL2X-XXXP", "{invoice_number}"),
    ("[Plan or Provider Contact Business Name]", "{bill_business}"),
    ("[Plan or Provider Contact Name]", "{bill_contact}"),
    ("[Plan or Provider Contact Position/Title]", "{bill_title}"),
    ("[Address Line 1]", "{bill_address1}"),
    ("[City, State, Zip Code]", "{bill_city_state_zip}"),
    ("[Plan or Provider email address]", "{bill_email}"),
    ("[Plan or Provider  email address]", "{bill_email}"),
    ("[Insert subject of case IE: External Review of Denied Claims]", "{case_subject}"),
    ("Review costs: $[amount]", "Review costs: {review_cost}"),
    ("Plan is responsible for 100%", "{responsible_party} is responsible for 100%"),
    ("Plan is responsible for XX%", "{responsible_party} is responsible for {share_pct}%"),
    ("Capitol Bridge Case Number: FL2X-XXX", "Capitol Bridge Case Number: {case_number}"),
    ("Capitol Bridge Case Number: FL2X-", "Capitol Bridge Case Number: {case_number}"),
    ("A $50 late charge will be added if payment is not received within 35 days. An additional $50 late charge will "
     "accrue every 30 days", "A {late_fee} late charge will be added if payment is not received within {late_after} "
     "days. An additional {late_fee} late charge will accrue every {late_every} days"),
    ("TOTAL:$[amount]", "TOTAL:{review_cost}"),
    ("TOTAL if paid after 35 days: $ [amount + $50]", "TOTAL if paid after {late_after} days: {total_late1}"),
    ("TOTAL if paid after 65 days: $ [amount + $100]", "TOTAL if paid after {late_after2} days: {total_late2}"),
    ("$[amount]", "{review_cost}"),
    ("with the case number FL2X-XXX listed", "with the case number {case_number} listed"),
    ("First Business Bank", "{remit_bank}"),
    ("075905787", "{remit_routing}"),
    ("164474499", "{remit_account}"),
]
SPECS["invoice_full"] = INVOICE
SPECS["invoice_split"] = INVOICE


def highlighted(doc) -> list[str]:
    out = []
    for p in dx.paragraphs(doc):
        for r in dx.runs(p):
            rpr = r.find(qn("w:rPr"))
            if rpr is not None and rpr.find(qn("w:highlight")) is not None and dx.rtext(r).strip():
                out.append(dx.rtext(r))
    return out


def convert(name: str) -> list[str]:
    doc = Document(os.path.join(SRC, name + ".docx"))
    pars = list(dx.paragraphs(doc))
    cursor = 0
    for step in SPECS[name]:
        if step[0] == "table":
            _, ti, cells = step
            tbl = doc.tables[ti]
            row = tbl.rows[1]
            head_run = next((r for r in tbl.rows[0].cells[0]._tc.iter(qn("w:r"))), None)
            for c, token in zip(row.cells, cells):
                p = c.paragraphs[0]._p
                r = p.makeelement(qn("w:r"), {})
                if head_run is not None and head_run.find(qn("w:rPr")) is not None:
                    rpr = __import__("copy").deepcopy(head_run.find(qn("w:rPr")))
                    for b in rpr.findall(qn("w:b")):
                        rpr.remove(b)
                    r.append(rpr)
                t = r.makeelement(qn("w:t"), {})
                t.text = token
                r.append(t)
                p.append(r)
            for extra in tbl.rows[2:]:
                extra._tr.getparent().remove(extra._tr)
            continue
        if step[0] == "next":  # the first match after the last "next" one
            _, old, new = step
            for i in range(cursor, len(pars)):
                if dx.replace_in(pars[i], old, new) >= 0:
                    cursor = i + 1
                    break
            continue
        old, new = step
        for p in pars:
            at = 0
            while (at := dx.replace_in(p, old, new, at)) >= 0:
                pass
    for p in pars:
        dx.unwrap_hyperlinks_with_fields(p)
    left = [t for t in highlighted(doc) if not dx.FIELD.search(t)]
    # The office highlights only its blanks: what is left highlighted (a space beside a blank) goes plain too.
    for p in pars:
        for r in dx.runs(p):
            dx.unhighlight(r)
    left += [t for p in pars for t in re.findall(r"\[[^\]]{2,}\]", dx.ptext(p))]
    os.makedirs(OUT, exist_ok=True)
    doc.save(os.path.join(OUT, name + ".docx"))
    return left


if __name__ == "__main__":
    bad = 0
    for name in SPECS:
        left = convert(name)
        print(f"{name:20} {'ok' if not left else 'LEFT: ' + repr(left)}")
        bad += bool(left)
    sys.exit(1 if bad else 0)
