"""Remittances, payer EOBs and UB-04s read for the claims table. Fixtures are the Exhibit 2 samples (redacted by the
filer) as the pipeline itself read them -- testdata/remit/*.ocr.txt is Docling/PaddleOCR's text, bank numbers zeroed;
eob_molina.pdf is the born-digital EOB, read from its own text layer. And the case-fill rules on top (main.py):
only empty fields are filled, a disagreeing figure is recorded, a UB-04 joins the claim billed the same total, a
replacement claim is flagged.
   python -m unittest test_remit_docs"""
import json
import os
import unittest

import main
import pipeline
import remit_docs as rd

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testdata", "remit")


def ocr(name: str) -> str:
    with open(os.path.join(HERE, f"{name}.ocr.txt"), encoding="utf-8") as f:
        return f.read()


def read(name: str, doc_type: str = "eob", raw: bytes | None = None):
    return rd.extract(doc_type, ocr(name), raw)


class Epic835(unittest.TestCase):
    def test_each_printout_classifies_as_an_eob(self):
        for name in ("eop1", "eop2", "eop3", "eop4"):
            dtype, _, margin = pipeline.classify_text(name + ".pdf", ocr(name))
            self.assertEqual(dtype, "eob", name)
            self.assertGreaterEqual(margin, pipeline.CLASSIFY_MIN_MARGIN, name)

    def test_ambetter_denial(self):
        key, f, issues = read("eop1")
        self.assertEqual((key, f["layout"]), ("remit", "epic_835"))
        self.assertEqual((f["claim_charge_cents"], f["claim_paid_cents"]), (2809516, 0))
        self.assertEqual((f["payer_control_number"], f["facility_code"], f["frequency_code"], f["drg"]),
                         ("Z149MP5A1008", "11", "7", "794"))
        self.assertEqual(f["filing_indicator"], "HM")
        self.assertEqual(f["reason_codes"], ["CO197"])
        self.assertEqual((f["issue_date"], f["trace_number"]), ("2026-06-02", "0932619507"))
        self.assertEqual(f["payer"]["name"], "AMBETTER")
        self.assertEqual((f["payee"]["name"], f["payee"]["id"]), ("SHANDS JACKSONVILLE MEDICAL CENTER I", "1821186313"))
        lines = f["service_lines"]
        self.assertEqual([ln["rev_code"] for ln in lines], ["0171", "0250", "0300", "0301", "0410", "0471"])
        self.assertEqual(lines[0], {"line": 1, "rev_code": "0171", "billed_cents": 2496000, "paid_cents": 0,
                                    "adjustment_cents": 2496000, "units": 6, "reason_codes": ["CO197"],
                                    "service_date": "2026-01-22"})
        self.assertEqual(sum(ln["billed_cents"] for ln in lines), 2809516)
        self.assertEqual(issues, [])
        self.assertEqual(f["claims"], [{
            "claim_number": "Z149MP5A1008", "billed_cents": 2809516, "paid_cents": 0, "disputed_cents": 2809516,
            "date_of_service": "2026-01-22", "date_of_denial": "2026-06-02",
            "denial_reason": "CO197: Precertification/authorization/notification absent",
            "type_of_service": "Hospital Inpatient", "provider_name": "SHANDS JACKSONVILLE MEDICAL CENTER I"}])
        self.assertEqual(f["case"], {"health_plan": "AMBETTER", "provider_org": "SHANDS JACKSONVILLE MEDICAL CENTER I",
                                     "provider_npi": "1821186313", "provider_type": "hospital_inpatient",
                                     "disputed_issue": "Denial"})

    def test_labels_then_values_layout(self):
        # EOP2 prints every label first and the values after, with the DRG row before the frequency row.
        _, f, _ = read("eop2")
        self.assertEqual((f["claim_charge_cents"], f["payer_control_number"], f["drg"], f["frequency_code"]),
                         (4781180, "Y024FLE46676", "6962", "1"))
        self.assertEqual((f["filing_indicator"], f["reason_codes"], f["remark_codes"]), ("MC", ["CO16"], ["M20"]))
        self.assertEqual(f["payer"], {"name": "SUNSHINE STATE HEALTH PLAN",
                                      "address": "PO BOX 459089, SUITE 100 FORT LAUDERDALE, FL 33345", "id": "68069"})
        self.assertEqual(f["case"]["line_of_business"], "Medicaid")
        self.assertEqual(f["claims"][0]["denial_reason"],
                         "CO16: Claim lacks information or has submission/billing errors; M20: Missing/incomplete/invalid HCPCS")
        self.assertEqual(f["claims"][0]["date_of_service"], "2025-01-13")

    def test_one_cell_layout(self):
        # EOP3 puts the whole Claim Details block in one table cell, and repeats merged cells across columns.
        _, f, issues = read("eop3")
        self.assertEqual((f["claim_charge_cents"], f["payer_control_number"], f["facility_code"], f["frequency_code"], f["drg"]),
                         (918158, "X359FLE32025", "11", "1", "0532"))
        self.assertEqual([(ln["rev_code"], ln["billed_cents"]) for ln in f["service_lines"]],
                         [("0123", 433900), ("0250", 3758), ("0740", 480500)])
        self.assertEqual(issues, [])

    def test_bank_numbers_are_never_read(self):
        for name in ("eop1", "eop2", "eop3", "eop4"):
            _, f, _ = read(name)
            self.assertNotRegex(json.dumps(f), r"0{9,}", name)


class PayerEOB(unittest.TestCase):
    def test_molina_from_its_text_layer(self):
        with open(os.path.join(HERE, "eob_molina.pdf"), "rb") as fh:
            key, f, issues = read("eob_molina", raw=fh.read())
        self.assertEqual((key, f["layout"]), ("remit", "payer_eob"))
        self.assertEqual((f["claim_number"], f["claim_date"], f["facility_code"]), ("25118100868", "2024-05-24", "11"))
        self.assertEqual((f["billed_cents"], f["benefit_cents"]), (72446961, 0))
        self.assertEqual(len(f["service_lines"]), 19)
        self.assertEqual(sum(r["billed_cents"] for r in f["service_lines"]), 72446961)
        self.assertEqual(f["reason_legend"], {"197": "Precertification/authorization/notification absent"})
        self.assertEqual((f["payment_cents"], f["payment_date"]), (416, "2025-05-09"))
        self.assertEqual(f["payer"]["name"], "MOLINA HEALTHCARE OF FLORIDA")
        self.assertEqual(issues, [])
        claim = f["claims"][0]
        self.assertEqual((claim["claim_number"], claim["disputed_cents"], claim["date_of_service"], claim["date_of_denial"]),
                         ("25118100868", 72446961, "2024-05-24", "2025-05-09"))
        self.assertEqual(claim["denial_reason"], "197: Precertification/authorization/notification absent")

    def test_docling_table_view_loses_a_line_and_says_so(self):
        # Docling's table merges the two $1,522.05 lines; the sum check against the printed total catches it.
        _, f, issues = read("eob_molina")
        self.assertEqual(len(f["service_lines"]), 18)
        self.assertTrue(issues and "not the $724,469.61 billed" in issues[0], issues)


class UB04(unittest.TestCase):
    def test_reads_the_form_locators(self):
        key, f, _ = read("ub04", "idr_claim")
        self.assertEqual(pipeline.classify_text("ub04.pdf", ocr("ub04"))[0], "idr_claim")
        self.assertEqual((key, f["type_of_bill"], f["billing_npi"], f["attending_npi"]),
                         ("ub04", "0117", "1821186313", "1801322813"))
        self.assertEqual((f["patient_control_number"], f["health_plan_id"], f["pps_code"]), ("50005496040010", "51062", "862"))
        self.assertEqual(f["total_charges_cents"], 72446961)
        self.assertEqual(f["payer"]["name"], "MEDICAID MOLINA HEALTHCARE")
        self.assertIn("0110", f["revenue_codes"])
        self.assertNotIn("0117", f["revenue_codes"])  # the type of bill, not a revenue code
        self.assertEqual(f["case"], {"provider_org": "SHANDS JACKSONVILLE", "provider_street": "655 W 8TH ST",
                                     "provider_city": "JACKSONVILLE", "provider_state": "FL", "provider_postal": "32209-6511",
                                     "provider_npi": "1821186313", "health_plan": "MEDICAID MOLINA HEALTHCARE",
                                     "line_of_business": "Medicaid", "provider_type": "hospital_inpatient"})
        self.assertEqual(f["claims"], [{"match_billed_cents": 72446961, "billed_cents": 72446961,
                                        "type_of_service": "Hospital Inpatient", "facility_name": "SHANDS JACKSONVILLE"}])

    def test_other_documents_are_not_read(self):
        self.assertEqual(rd.extract("determination_letter", ocr("ub04")), (None, {}, []))
        self.assertEqual(rd.extract("eob", "Explanation of benefits. Allowed amount $10.00"), (None, {}, []))


class CaseFill(unittest.TestCase):
    def test_only_empty_fields_are_filled_and_disagreements_recorded(self):
        _, remit, _ = read("eop1")
        details = {"provider_org": "UF Health Jacksonville", "contracted": True}
        updates, disputed, conflicts, changed = main.claim_doc_updates(details, None, {"remit": remit}, "eop1.pdf")
        self.assertNotIn("provider_org", updates)  # staff typed it
        self.assertEqual(updates["health_plan"], "AMBETTER")
        self.assertEqual(updates["provider_type"], "hospital_inpatient")
        self.assertIsNone(disputed)  # an 835 states the claim charge, not the amount in dispute
        self.assertEqual(conflicts, [])
        _, upd2, conflicts2, _ = main.claim_doc_updates({"provider_type": "physician_dentist"}, 100, {"remit": remit}, "eop1.pdf")
        self.assertEqual(conflicts2, [{"field": "provider_type", "case_value": "physician_dentist",
                                       "document_value": "hospital_inpatient", "document": "eop1.pdf"}])

    def test_packet_sets_the_disputed_amount_and_report_fields(self):
        packet = {"case": {"disputed_amount_cents": 75256477, "contracted": False, "plan_type": "Medicaid HMO",
                           "provider_type_other": ""}, "claims": [],
                  "attestation": {"interest_only": True}, "documents_submitted": ["claim_forms"]}
        updates, disputed, _, changed = main.claim_doc_updates({}, 0, {"packet": packet}, "packet.pdf")
        self.assertEqual(disputed, 75256477)
        self.assertEqual(updates["out_of_network"], "Yes")
        self.assertEqual(updates["packet_attestation"], {"interest_only": True})
        self.assertNotIn("disputed_amount_cents", updates)
        self.assertIn("disputed amount $752,564.77", changed)

    def test_claim_rows_merge(self):
        _, molina, _ = read("eob_molina")
        _, ub, _ = read("ub04", "idr_claim")
        rows, conflicts = main.merge_claim_rows({}, molina["claims"] + ub["claims"], "exhibit2.pdf")
        self.assertEqual([r["claim_number"] for r in rows], ["25118100868", "25118100868"])  # the UB-04 joins it
        self.assertEqual(rows[1]["facility_name"], "SHANDS JACKSONVILLE")
        self.assertEqual(conflicts, [])
        rows, conflicts = main.merge_claim_rows({"25118100868": {"billed_cents": 70000000}}, molina["claims"], "eob.pdf")
        self.assertEqual(conflicts[0]["field"], "billed_cents")

    def test_replacement_claim_is_flagged(self):
        _, original, _ = read("eop4")
        _, replacement, _ = read("eop1")
        existing = {"Z035MPEB6165": {"billed_cents": 2809516, "paid_cents": 0, "date_of_service": "2026-01-22"}}
        self.assertEqual(original["claims"][0]["claim_number"], "Z035MPEB6165")
        _, conflicts = main.merge_claim_rows(existing, replacement["claims"], "eop1.pdf")
        self.assertEqual(conflicts, [{"claim_number": "Z149MP5A1008", "field": "same_service_as", "case_value": "Z035MPEB6165",
                                      "document_value": "Z149MP5A1008", "document": "eop1.pdf"}])


if __name__ == "__main__":
    unittest.main()


class ClaimNumberAndCodes(unittest.TestCase):
    def test_packet_title_is_not_a_claim_number(self):
        import fields
        packet_title = "THE FLORIDA STATEWIDE PROVIDER AND HEALTH PLAN\nCLAIM DISPUTE RESOLUTION PROGRAM FILING PACKET"
        self.assertNotIn("claim_number", fields.extract_fields(packet_title).get("identifiers", {}))
        self.assertEqual(fields.extract_fields(packet_title + "\nClaim Number: 25118100868")["identifiers"]["claim_number"],
                         "25118100868")

    def test_layout_documents_state_their_own_claim_number_and_no_regex_codes(self):
        ctx = pipeline.stage_fields({"doc_type": "eob", "text": ocr("eop1")}, {})
        ex = ctx["extracted_fields"]
        self.assertEqual(ex["identifiers"]["claim_number"], "Z149MP5A1008")
        self.assertEqual(ex.get("cpt_codes"), [])  # zip codes and amounts are not procedure codes
        ctx = pipeline.stage_fields({"doc_type": "idr_claim", "text": ocr("ub04")}, {})
        self.assertNotIn("claim_number", ctx["extracted_fields"].get("identifiers", {}))  # a UB-04 prints none
