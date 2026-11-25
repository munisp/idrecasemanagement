"""The Capitol Bridge filing packet read into the case: the blank 2026 packet (testdata/filing_packet_blank.pdf) is
filled in memory the way a filer's PDF viewer fills it, then read back -- so the field map is checked against the
real form, not against itself. Values are the Exhibit 2 sample (Shands Jacksonville / Molina claim 25118100868).
   python -m unittest test_packet_docs"""
import io
import os
import unittest

import packet_docs as pk
import pipeline

HERE = os.path.dirname(os.path.abspath(__file__))
BLANK = os.path.join(HERE, "testdata", "filing_packet_blank.pdf")

FILLED = {
    "Group1": "/Choice1",
    "Text2": "Shands Jacksonville Medical Center Inc", "Text3": "Shands Jacksonville Medical Center, Inc.",
    "Text4": "UF Health Jacksonville", "Text5": "59-1234567 / 1821186313", "Text6": "H-4521",
    "Text7": "655 W 8th St", "Text8": "Jacksonville", "Text9": "Florida", "Text10": "32209-6511",
    "Text11": "Pat Example", "Text13": "(904) 555-0100", "Text15": "appeals@example.test",
    "Group2": "/0", "Text16": "Molina Healthcare of Florida", "Text17": "Molina Healthcare", "Group3": "/1",
    "Group4": "/1", "Group5": "/0", "Group6": "/0", "Text21": "2", "Text22": "$752,564.77",
    "Claim Row1": "25118100868", "Patient NameRow1": "Doe, Jane", "Type of ServiceRow1": "Inpatient",
    "Date of ServiceRow1": "05/24/2024-03/15/2025", "Amount BilledRow1": "$724,469.61", "Amount PaidRow1": "0.00",
    "Amount in DisputeRow1": "724,469.61", "Date Claim SubmittedRow1": "04/04/2025", "Date of DenialRow1": "05/09/2025",
    "Date of Final DeterminationRow1": "07/01/2025",
    "Claim Row2": "26030500112", "Patient NameRow2": "John Roe", "Type of ServiceRow2": "Inpatient",
    "Date of ServiceRow2": "01/22/26", "Amount BilledRow2": "28,095.16", "Amount PaidRow2": "0",
    "Group7": "/0",
    "Text23": "Medically necessary inpatient stay.\nAuthorization 2414591439 was obtained.",
    "Check Box27": "/Yes", "Check Box28": "/Yes", "Check Box30": "/Yes", "Check Box35": "/Yes",
    "Check Box40": "/Yes", "Check Box41": "/Yes", "Check Box42": "/Yes", "Check Box43": "/Yes", "Check Box44": "/Yes",
    "Check Box45": "/Yes", "Check Box46": "/Yes", "Check Box47": "/Yes",
    "Text39": "Pat Example", "Text200": "Director, Patient Financial Services", "Date49_af_date": "10/15/2025",
    "Text40": "Shands Jacksonville Medical Center Inc", "Text41": "Molina Healthcare of Florida",
    "Text42": "Acme Recovery LLC",
}


def filled_packet(values: dict) -> bytes:
    from pypdf import PdfReader, PdfWriter
    w = PdfWriter(clone_from=PdfReader(BLANK))
    for page in w.pages:
        w.update_page_form_field_values(page, values, auto_regenerate=False)
    buf = io.BytesIO()
    w.write(buf)
    return buf.getvalue()


class FilledPacket(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.facts, cls.issues = pk.extract("filing_packet", "", filled_packet(FILLED))
        cls.case = cls.facts["case"]

    def test_read_from_the_form_fields(self):
        self.assertEqual(self.facts["source"], "form")

    def test_provider_block(self):
        c = self.case
        self.assertEqual(c["filing_party_type"], "PROVIDER")
        self.assertEqual(c["provider_org"], "Shands Jacksonville Medical Center Inc")
        self.assertEqual(c["provider_legal_entity"], "Shands Jacksonville Medical Center, Inc.")
        self.assertEqual(c["provider_trade_name"], "UF Health Jacksonville")
        self.assertEqual(c["provider_npi"], "1821186313")
        self.assertEqual(c["provider_tin"], "59-1234567")
        self.assertEqual(c["provider_license"], "H-4521")
        self.assertEqual((c["provider_street"], c["provider_city"], c["provider_state"], c["provider_postal"]),
                         ("655 W 8th St", "Jacksonville", "FL", "32209-6511"))
        self.assertEqual(c["provider_contact"], "Pat Example")
        self.assertEqual(c["provider_phone"], "(904) 555-0100")
        self.assertEqual(c["provider_email"], "appeals@example.test")

    def test_plan_and_answers(self):
        c = self.case
        self.assertEqual(c["health_plan"], "Molina Healthcare of Florida")
        self.assertEqual(c["health_plan_trade_name"], "Molina Healthcare")
        self.assertEqual(c["plan_type"], "Medicaid HMO")
        self.assertEqual(c["line_of_business"], "Medicaid")
        self.assertIs(c["contracted"], False)
        self.assertEqual(c["internal_dispute_exhausted"], "Yes")
        self.assertEqual(c["type_of_service"], "Hospital Inpatient")
        self.assertEqual(c["packet_provider_type"], "Hospital")
        self.assertEqual(c["provider_type"], "hospital_inpatient")
        self.assertIs(c["fee_estimate_requested"], True)
        self.assertEqual(c["claims_submitted"], 2)
        self.assertEqual(c["disputed_amount_cents"], 75256477)
        self.assertEqual(c["provider_position_basis"], "Medically necessary inpatient stay.\nAuthorization 2414591439 was obtained.")

    def test_claim_rows(self):
        first, second = self.facts["claims"]
        self.assertEqual(first, {
            "claim_number": "25118100868", "patient_first_name": "Jane", "patient_last_name": "Doe",
            "type_of_service": "Inpatient", "billed_cents": 72446961, "paid_cents": 0, "disputed_cents": 72446961,
            "date_of_service": "2024-05-24", "date_of_service_through": "2025-03-15",
            "date_claim_submitted": "2025-04-04", "date_of_denial": "2025-05-09",
            "date_of_final_determination": "2025-07-01"})
        self.assertEqual((second["patient_first_name"], second["patient_last_name"]), ("John", "Roe"))
        self.assertEqual(second["date_of_service"], "2026-01-22")
        self.assertEqual(second["disputed_cents"], 2809516)  # billed less paid when the row leaves it blank

    def test_documents_and_attestation(self):
        self.assertEqual(self.facts["documents_submitted"],
                         ["claim_forms", "denials_eobs", "final_determination_letter", "medical_records"])
        att = self.facts["attestation"]
        self.assertTrue(att["interest_only"] and att["over_12_months"])
        self.assertFalse(att["rural_hospital"])
        self.assertIn("Required documentation not ticked on the packet: florida license", self.issues)

    def test_representative_page(self):
        self.assertEqual(self.case["aor_representative"], "Acme Recovery LLC")
        self.assertEqual(self.case["attested_on"], "2025-10-15")

    def test_rows_that_do_not_add_up_are_flagged(self):
        self.assertFalse(any("aggregated disputed amount" in i for i in self.issues), self.issues)
        _, issues = pk.extract("filing_packet", "", filled_packet({**FILLED, "Text22": "$800,000.00"}))
        self.assertIn("The packet's claim rows dispute $752,564.77 but its aggregated disputed amount is $800,000.00", issues)


class Other(unittest.TestCase):
    def test_blank_packet_reads_nothing(self):
        with open(BLANK, "rb") as f:
            self.assertEqual(pk.extract("filing_packet", "", f.read()), ({}, []))

    def test_other_types_are_not_read(self):
        self.assertEqual(pk.extract("eob", "Legal Entity Foo", filled_packet(FILLED)), ({}, []))

    def test_scanned_packet_read_from_text(self):
        text = """Filing Party Provider Health Plan
Legal Entity Gulf Coast Clinic LLC
Trade Name Gulf Coast Clinic
Tax ID/NPI No 1234567893
Street Address 100 Main St
City Tampa
State FL
Zip Code 33602
Contact Person Sam Lee
Health Plan Bay Health Plan
Trade Name Bay Health
Number of claims included 3
Aggregated Disputed Amount $12,400.00"""
        facts, _ = pk.extract("filing_packet", text, b"")
        c = facts["case"]
        self.assertEqual(facts["source"], "text")
        self.assertEqual(c["provider_org"], "Gulf Coast Clinic LLC")
        self.assertEqual(c["provider_trade_name"], "Gulf Coast Clinic")
        self.assertEqual(c["health_plan"], "Bay Health Plan")
        self.assertEqual(c["health_plan_trade_name"], "Bay Health")
        self.assertEqual((c["provider_city"], c["provider_state"], c["provider_postal"]), ("Tampa", "FL", "33602"))
        self.assertEqual(c["claims_submitted"], 3)
        self.assertEqual(c["disputed_amount_cents"], 1240000)
        self.assertNotIn("contracted", c)  # tick boxes are not read from a scan

    def test_blank_packet_docling_text_reads_nothing(self):
        # What Docling makes of the blank packet: every label, no values -- nothing to fill.
        with open(os.path.join(HERE, "testdata", "filing_packet_blank.ocr.txt"), encoding="utf-8") as f:
            text = f.read()
        self.assertEqual(pipeline.classify_text("Claim Dispute Resolution Program Filing Packet.pdf", text)[0], "filing_packet")
        self.assertEqual(pk.extract("filing_packet", text, b""), ({}, []))

    def test_a_label_is_a_whole_word(self):
        facts, _ = pk.extract("filing_packet", "Statement of the dispute follows\nCity Tampa", b"")
        self.assertEqual(facts["case"], {"provider_city": "Tampa"})

    def test_portal_form_uses_the_same_field_map(self):
        # portal/js/packetform.js pre-fills its form from a packet PDF with its own copy of the map.
        import json
        import re
        path = os.path.join(HERE, "..", "..", "portal", "js", "packetform.js")
        if not os.path.exists(path):
            self.skipTest("the portal is not next to this service (inside the doc-intel image)")
        with open(path, encoding="utf-8") as f:
            src = f.read()
        block = re.search(r"FORM_FIELDS-BEGIN\s*const FORM_FIELDS = (\{.*?\});\s*// FORM_FIELDS-END", src, re.S)
        self.assertIsNotNone(block, "FORM_FIELDS block not found in packetform.js")
        self.assertEqual(json.loads(block.group(1)), pk.FORM_FIELDS)

    def test_program_provider_type(self):
        self.assertEqual(pk.program_provider_type("Rural Hospital", ""), "rural_hospital")
        self.assertEqual(pk.program_provider_type("Hospital", "Hospital Outpatient"), "hospital_outpatient")
        self.assertIsNone(pk.program_provider_type("Hospital", ""))
        self.assertEqual(pk.program_provider_type("Dentist", "Professional"), "physician_dentist")
        self.assertEqual(pk.program_provider_type("Pharmacy", "Other"), "other")

    def test_blank_packet_classifies_as_a_filing_packet(self):
        from pypdf import PdfReader
        with open(BLANK, "rb") as f:
            text = "\n".join(p.extract_text() or "" for p in PdfReader(f).pages)
        self.assertEqual(pipeline.classify_text("packet.pdf", text)[0], "filing_packet")


if __name__ == "__main__":
    unittest.main()
