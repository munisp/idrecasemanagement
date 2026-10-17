"""The office's letters recognised in uploads: each type classifies as itself (not as an EOB, claim or federal
determination letter), its amounts are read, and its own arithmetic is checked. Texts are the text layer of
generated letters (made-up parties and amounts).
   python -m unittest test_letter_docs"""
import unittest

import letter_docs as ld

DECISION = """FL26-000216
CAPITOL BRIDGE LLC.
FLORIDA PROVIDER AND HEALTH PLAN CLAIM DISPUTE RESOLUTION PROGRAM
CASE NO: FL26-000216 CLAIM NO: ALL
Summary: Capitol Bridge LLC (Capitol Bridge) recommends that the Health Plan’s denial
of payment for these services be partially overturned and the amount of $640,000.00 be
awarded to the Provider, of which $53,155.54 was already paid to the Provider. Therefore,
the balance of $586,844.46 is owed to the Provider.
Claim Summary:
This case involves a dispute between Gulf Coast Clinic and Bay Health Plan regarding one claim.
Standard of Review:
Recommended Determination:
Rationale:
On February 2, 2026, Capitol Bridge notified Gulf Coast Clinic that the case was accepted for
review and notified Bay Health Plan of said acceptance. Both parties participated in full with the
process and submitted full statements and responses.
Independent Physician Review:
Coding Consultant Review:
Claim Number CPT Code Date of Service Zip Code Amount Billed UCR Pricing
Based on the foregoing, Capitol Bridge recommends ... Capitol Bridge is awarding $640,000.00 or 92% of the
disputed charge to the Provider."""

COVER = """FL26-000216
September 1, 2026
RE: External Review of Denied Claims
CASE NO: FL26-000216 CLAIM NO: ALL
Summary:
This case involved one (1) claim submitted by Gulf Coast Clinic involving one (1) patient
enrolled in Bay Health Plan – Medicaid. The amount in controversy is $695,652.17.
Capitol Bridge LLC. has determined that the Health Plan’s denial of payment for these services
be partially overturned and the amount of $640,000.00 be awarded to the Provider, of which
$53,155.54 was already paid to the Provider. Therefore, the balance of $586,844.46 is owed to
the Provider.
Capitol Bridge is an organization that contracts with the Florida Agency for Health Care
Administration to independently review Provider and Managed Care Organization Claim
Disputes in response to claims brought under Chapter 408.7057, Florida Statutes.
Capitol Bridge completed its initial review of this case on February 2, 2026 and accepted this
case for full review. The claims were reviewed by an attorney on the Capitol Bridge professional appeals staff.
Case File Summary:
Decision:
Enclosed with this letter is the analysis and basis for decision for the issue raised in the claim
included in this case.
Sincerely,"""

INVOICE = """Florida AHCA Claims Dispute Resolution Contractor
INVOICE
DATE
PREPARED
DATE
ISSUED
INVOICE #
09/01/2026 See Final
Order Date
FL26-000216H
BILL TO:
Bay Health Plan
DESCRIPTION AMOUNT
External Review of Denied Claims
Review costs: $3,220.92
Health Plan is responsible for 92% of the review cost per the
determination in the Final Order (FL Admin Code R
59A-12.030(4)(b))
Capitol Bridge Case Number: FL26-000216
A $50 late charge will be added if payment is not received within
35 days. An additional $50 late charge will accrue every 30 days
until the invoice is paid.
$3,220.92
TOTAL:      $3,220.92
TOTAL if paid after 35 days:
$3,270.92
TOTAL if paid after 65 days:
$3,320.92
**Capitol Bridge only accepts ACH payment for FL AHCA dispute invoices.**
REMIT TO:
Capitol Bridge LLC
Bank: Example Bank
Remittance Advice: Email a copy of the remittance advice with the case number listed on the document."""

IRO = """IRO Number: 00051
CAPITOL BRIDGE LLC.
CAPITOL BRIDGE CASE NO: FL26-000301 PLAN REFERENCE NO: PR-77
DATE RECEIVED: March 11, 2026 DECISION DATE: March 15, 2026
Decision Summary: Capitol Bridge LLC (Capitol Bridge) recommends the Health Plan’s
denial of payment for services be overturned.
Recommended Determination:
Capitol Bridge recommends the Insurance Carrier’s denial of payment for these services be
overturned because the requested service is not experimental or investigational.
Evidence Supporting the Determination:
This decision is final and binding on the insurance carrier and the covered person pursuant to the statute."""


class Recognition(unittest.TestCase):
    def classify(self, name, text):
        import pipeline  # the classifier with every type's signals
        return pipeline.classify_text(name, text)[0]

    def test_each_letter_classifies_as_itself(self):
        try:
            import pipeline  # noqa: F401
        except ImportError as e:  # the pipeline's model dependencies are only in the image
            self.skipTest(f"pipeline dependencies not installed: {e}")
        self.assertEqual(self.classify("Review Rationale and Letter FL26-000216.pdf", DECISION), "fl_decision_report")
        self.assertEqual(self.classify("Cover Letter FL26-000216.pdf", COVER), "fl_cover_letter")
        self.assertEqual(self.classify("Invoice FL26-000216H - Health Plan.pdf", INVOICE), "fl_review_invoice")
        self.assertEqual(self.classify("decision.pdf", IRO), "iro_decision")
        # Unnamed uploads still classify by content.
        self.assertEqual(self.classify("scan.pdf", INVOICE), "fl_review_invoice")
        self.assertEqual(self.classify("scan.pdf", DECISION), "fl_decision_report")


class Extraction(unittest.TestCase):
    def test_decision_report(self):
        f, issues = ld.extract("fl_decision_report", DECISION)
        self.assertEqual(f["case_number"], "FL26-000216")
        self.assertEqual(f["outcome"], "PARTIALLY_OVERTURNED")
        self.assertEqual((f["awarded_cents"], f["already_paid_cents"], f["balance_cents"]), (64_000_000, 5_315_554, 58_684_446))
        self.assertEqual(f["awarded_to"], "PROVIDER")
        self.assertEqual(f["percent_awarded"], 92.0)
        self.assertEqual(issues, [])

    def test_amounts_that_do_not_add_up_are_flagged(self):
        _, issues = ld.extract("fl_decision_report", DECISION.replace("$586,844.46", "$586,000.00"))
        self.assertTrue(any("do not add up" in i for i in issues))

    def test_cover_letter(self):
        f, _ = ld.extract("fl_cover_letter", COVER)
        self.assertEqual(f["amount_in_controversy_cents"], 69_565_217)
        self.assertEqual(f["outcome"], "PARTIALLY_OVERTURNED")

    def test_invoice(self):
        f, issues = ld.extract("fl_review_invoice", INVOICE)
        self.assertEqual(f["invoice_number"], "FL26-000216H")
        self.assertEqual(f["case_number"], "FL26-000216")
        self.assertEqual((f["party"], f["responsible_pct"]), ("HEALTH_PLAN", 92))
        self.assertEqual(f["admin_code_rule"], "R 59A-12.030(4)(b)")
        self.assertEqual(f["total_cents"], 322_092)
        self.assertEqual(f["late_totals"], {"35": 327_092, "65": 332_092})
        self.assertEqual(issues, [])
        self.assertNotIn("routing", " ".join(f.keys()))

    def test_invoice_read_as_a_table(self):
        # Document analysis reads the INVOICE box as a table: the number lands far from its label, and the bank
        # change notice may come before the prepared date.
        md = ("**BANK ACCOUNT INFORMATION CHANGED ON 11/18/2025**\n| DATE PREPARED | DATE ISSUED | INVOICE # |\n"
              "|---|---|---|\n| 09/01/2026 | See Final Order Date | FL26-000216H |\n" + INVOICE.split("BILL TO:")[1])
        f, _ = ld.extract("fl_review_invoice", "INVOICE\n" + md)
        self.assertEqual(f["invoice_number"], "FL26-000216H")
        self.assertEqual(f["date_prepared"], "09/01/2026")

    def test_invoice_checks(self):
        _, issues = ld.extract("fl_review_invoice", INVOICE.replace("$3,320.92", "$3,300.92"))
        self.assertTrue(any("65 days" in i for i in issues))
        _, issues = ld.extract("fl_review_invoice", INVOICE.replace("(4)(b)", "(4)(a)"))
        self.assertTrue(any("(4)(a)" in i for i in issues))

    def test_iro(self):
        f, _ = ld.extract("iro_decision", IRO)
        self.assertEqual((f["iro_number"], f["outcome"]), ("00051", "OVERTURNED"))
        self.assertEqual((f["date_received"], f["decision_date"]), ("March 11, 2026", "March 15, 2026"))
        self.assertEqual(f["case_number"], "FL26-000301")

    def test_other_types_untouched(self):
        self.assertEqual(ld.extract("eob", INVOICE), ({}, []))

    def test_case_numbers_compare_by_sequence(self):
        self.assertEqual(ld.case_seq("FL26-216"), ld.case_seq("FL26-000216"))
        self.assertEqual(ld.case_seq("FL25-023H"), ("FL25", 23))
        self.assertNotEqual(ld.case_seq("FL26-216"), ld.case_seq("FL25-216"))


if __name__ == "__main__":
    unittest.main()
