"""An EOB is read the same whatever its layout and whatever shape the model
answers in. Each test is one shape that either broke the reader or could."""
import pytest

import eobshape as es

EOB_FIELDS = ["claim_number", "allowed_amount_usd", "patient_responsibility_usd", "denial_codes",
              "service_lines", "network_status", "service_date", "determination_date", "place_of_service",
              "billed_amount_usd", "provider_name", "payer_name", "line_of_business"]


# ---- coercion ---------------------------------------------------------------

@pytest.mark.parametrize("value,want", [
    (1850, 1850.0), (1850.0, 1850.0), ("1850", 1850.0), ("$1,850.00", 1850.0), ("1,850.00 USD", 1850.0),
    ("USD 1850.5", 1850.5), (" $ 412.00 ", 412.0), ("(82.40)", -82.4), ("-82.40", -82.4),
    ({"amount": "1,850.00", "currency": "USD"}, 1850.0), ({"value": 412}, 412.0), ([1850], 1850.0),
    (["$1,850.00"], 1850.0),
    (None, None), ("", None), ("N/A", None), (True, None), ("330.00 of 1,450.00", None),
    ("see attached", None), ([100, 200], None), ({}, None), (float("nan"), None),
])
def test_as_money(value, want):
    assert es.as_money(value) == want


def test_as_text_never_raises_and_prefers_the_naming_cell():
    assert es.as_text({"name": "Northgate Health, P.C.", "npi": "1234567890"}) == "Northgate Health, P.C."
    assert es.as_text(["a", None, {"value": "b"}, 3]) == "a b 3"
    assert es.as_text(None) == "" and es.as_text(0) == "0"
    assert es.as_text(object())  # anything at all


@pytest.mark.parametrize("value,n", [
    ([{"service": "Office visit (99215)"}, {"service": "ECG (93000)"}], 2),
    (["Office visit 99215 $1,450.00", "ECG 93000 $400.00"], 2),
    ("Office visit 99215 $1,450.00\nECG 93000 $400.00", 2),
    ({"service": "Office visit (99215)", "billed_amount_usd": 1450.0}, 1),
    ({"service": ["Office visit", "ECG"], "billed": [1450, 400]}, 2),
    ({"1": {"service": "Office visit"}, "2": {"service": "ECG"}}, 2),
    ([[{"service": "a"}, {"service": "b"}]], 2),
    (None, 0), ("", 0), ([], 0), ([None, ""], 0),
])
def test_as_lines_from_every_shape(value, n):
    assert len(es.as_lines(value)) == n


def test_line_text_reads_the_description_never_the_column_names():
    row = {"date": "03/12/2026", "service": "Office visit, established patient (99215)",
           "plan_paid_usd": 264.0, "billed_amount_usd": 1450.0, "patient_responsibility_usd": 66.0}
    assert es.line_text(row) == "Office visit, established patient (99215)"
    assert "responsibility" not in es.line_text(row).lower()
    assert es.line_text({"Description": "Lab panel", "CPT": "80053", "Charge": "$90.00"}) == "Lab panel 80053"
    # a row with columns nobody listed: its words, not its numbers
    assert es.line_text({"col_a": "Emergency dept visit", "col_b": "1,200.00", "col_c": "03/12/2026"}) == "Emergency dept visit"
    assert es.line_text("ER visit level 4") == "ER visit level 4"


def test_lines_billed_total_only_when_every_line_has_a_charge():
    assert es.lines_billed_total([{"service": "a", "billed_amount_usd": 1450.0}, {"service": "b", "charges": "$400.00"}]) == 1850.0
    assert es.lines_billed_total([{"service": "a", "billed": 1450}, {"service": "b"}]) is None
    assert es.lines_billed_total(["Office visit 1450"]) is None
    assert es.lines_billed_total(None) is None


def test_split_codes():
    assert es.split_codes("CO-45, PR-1") == ["CO-45", "PR-1"]
    assert es.split_codes("CO 45; N130") == ["CO-45", "N130"]
    assert es.split_codes(["CO-45", {"code": "PR-2"}]) == ["CO-45", "PR-2"]
    assert es.split_codes(None) == []


# ---- the model's answer, in whatever shape ----------------------------------

FLAT = {"claim_number": "NGH-2026-004417", "billed_amount_usd": 1850.0, "allowed_amount_usd": 412.0,
        "patient_responsibility_usd": 82.4, "network_status": "Out-of-network",
        "provider_name": "Northgate Health, P.C.", "payer_name": "Sunshine Coast Health Plan",
        "service_date": "03/12/2026", "denial_codes": ["CO-45"],
        "service_lines": [{"service": "Office visit (99215)", "billed_amount_usd": 1450.0},
                          {"service": "Electrocardiogram (93000)", "billed_amount_usd": 400.0}]}

SHAPES = {
    "flat, as asked": FLAT,
    "wrapped": {"eob": FLAT},
    "wrapped twice, in a list": {"result": [{"data": FLAT}]},
    "camelCase and other names": {
        "claimNumber": "NGH-2026-004417", "totalCharges": "$1,850.00", "allowedAmount": "412.00",
        "patientResponsibility": 82.4, "network": "Out-of-network", "provider": "Northgate Health, P.C.",
        "payer": "Sunshine Coast Health Plan", "dateOfService": "03/12/2026", "adjustmentCodes": "CO-45",
        "lineItems": FLAT["service_lines"]},
    "grouped": {
        "claim": {"number": "NGH-2026-004417", "date_of_service": "03/12/2026"},
        "totals": {"billed": 1850.0, "allowed": 412.0, "patient_responsibility": 82.4},
        "provider": {"name": "Northgate Health, P.C.", "npi": "1234567890"},
        "payer": {"name": "Sunshine Coast Health Plan"},
        "out_of_network": True, "denial_codes": ["CO-45"], "services": FLAT["service_lines"]},
    "Title Case keys, amounts as objects": {
        "Claim Number": "NGH-2026-004417", "Billed Amount": {"amount": 1850.0, "currency": "USD"},
        "Allowed Amount": {"value": "412.00"}, "Patient Responsibility": "$82.40",
        "Network Status": "Out-of-network", "Provider Name": "Northgate Health, P.C.",
        "Health Plan": "Sunshine Coast Health Plan", "Service Date": "03/12/2026",
        "Denial Codes": ["CO-45"], "Service Lines": FLAT["service_lines"]},
}


@pytest.mark.parametrize("name", list(SHAPES))
def test_shape_extraction_reads_every_shape_the_same(name):
    got = es.shape_extraction(SHAPES[name], EOB_FIELDS)
    assert got["claim_number"] == "NGH-2026-004417", name
    assert es.as_money(got["billed_amount_usd"]) == 1850.0, name
    assert es.as_money(got["allowed_amount_usd"]) == 412.0, name
    assert es.as_money(got["patient_responsibility_usd"]) == 82.4, name
    assert "out" in es.as_text(got["network_status"]).lower(), name
    assert es.as_text(got["provider_name"]) == "Northgate Health, P.C.", name
    assert es.as_text(got["payer_name"]) == "Sunshine Coast Health Plan", name
    assert got["service_date"] == "03/12/2026", name
    assert got["denial_codes"] == ["CO-45"], name
    assert len(got["service_lines"]) == 2, name
    assert set(got) <= set(EOB_FIELDS), name  # nothing outside the schema gets in


def test_a_line_amount_is_never_taken_for_the_claim_total():
    got = es.shape_extraction({"service_lines": [{"service": "Office visit", "billed_amount_usd": 1450.0,
                                                  "service_date": "03/12/2026"}]}, EOB_FIELDS)
    assert "billed_amount_usd" not in got and "service_date" not in got


@pytest.mark.parametrize("junk", [None, "not json", 42, [], [1, 2], {}, {"a": {"b": {"c": {"d": {"e": 1}}}}}])
def test_shape_extraction_survives_junk(junk):
    assert es.shape_extraction(junk, EOB_FIELDS) == {}


# ---- the page itself, in whatever layout ------------------------------------

TOTALS_BOX = """
EXPLANATION OF BENEFITS
Claim Number: NGH-2026-004417        Date Processed: 05/20/2026
Provider: Northgate Health, P.C.     Network Status: Out-of-Network
Health Plan: Sunshine Coast Health Plan
Date of Service: 03/12/2026

Total Billed: $1,850.00
Total Allowed: $412.00
Patient Responsibility: $82.40
"""

LINE_TABLE_WITH_TOTAL = """
## Explanation of Benefits

| Claim # | Provider | Payer |
|---|---|---|
| NGH-2026-004417 | Northgate Health, P.C. | Sunshine Coast Health Plan |

| Date of Service | Description | Billed Amount | Allowed Amount | Patient Responsibility |
|---|---|---|---|---|
| 03/12/2026 | Office visit (99215) | $1,450.00 | $330.00 | $66.00 |
| 03/12/2026 | Electrocardiogram (93000) | $400.00 | $82.00 | $16.40 |
| Total | | $1,850.00 | $412.00 | $82.40 |
"""

LINE_TABLE_NO_TOTAL = LINE_TABLE_WITH_TOTAL.replace("| Total | | $1,850.00 | $412.00 | $82.40 |\n", "")

REMITTANCE = """
REMITTANCE ADVICE
ICN: 20261400012345
**Payee:** Northgate Health, P.C.
Payer | Sunshine Coast Health Plan
Participation: Non-Participating

| Service | Charges | Allowed | Pt Resp |
|---|---|---|---|
| 99215 Office visit | 1,450.00 | 330.00 | 66.00 |
| 93000 ECG | 400.00 | 82.00 | 16.40 |
"""


@pytest.mark.parametrize("name,text", [
    ("totals box", TOTALS_BOX), ("line table with a total row", LINE_TABLE_WITH_TOTAL),
    ("line table, no total row", LINE_TABLE_NO_TOTAL), ("remittance, other words for everything", REMITTANCE),
])
def test_fill_from_text_reads_the_totals_off_any_layout(name, text):
    empty = {f: None for f in EOB_FIELDS}
    got, filled, summed = es.fill_from_text(empty, text, "eob")
    assert got["billed_amount_usd"] == 1850.0, name
    assert got["allowed_amount_usd"] == 412.0, name
    assert got["patient_responsibility_usd"] == 82.4, name
    assert got["provider_name"] == "Northgate Health, P.C.", name
    assert got["payer_name"] == "Sunshine Coast Health Plan", name
    assert got["claim_number"] in ("NGH-2026-004417", "20261400012345"), name
    assert "billed_amount_usd" in filled and not summed, name


def test_fill_from_text_reads_network_status_only_next_to_its_label():
    got, _, _ = es.fill_from_text({}, TOTALS_BOX, "eob")
    assert got["network_status"].lower() == "out-of-network"
    got, _, _ = es.fill_from_text({}, REMITTANCE, "eob")
    assert got["network_status"].lower() == "non-participating"
    # boilerplate about networks is not a statement about this provider
    got, _, _ = es.fill_from_text({}, "If you use out-of-network providers you may owe more.\nTotal Billed: $10.00", "eob")
    assert "network_status" not in got


def test_fill_from_text_never_overwrites_and_never_guesses():
    got, filled, _ = es.fill_from_text({"billed_amount_usd": 999.0}, TOTALS_BOX, "eob")
    assert got["billed_amount_usd"] == 999.0 and "billed_amount_usd" not in filled
    # two different billed amounts and no total: not guessed
    got, _, _ = es.fill_from_text({}, "Billed: $1,450.00\nBilled: $400.00", "eob")
    assert "billed_amount_usd" not in got
    # "Allowed charges" is not the billed amount
    got, _, _ = es.fill_from_text({}, "Allowed charges: $330.00", "eob")
    assert "billed_amount_usd" not in got
    # a heading row is not a value
    got, _, _ = es.fill_from_text({}, "| Provider | Payer |\n|---|---|\n", "eob")
    assert "provider_name" not in got


def test_fill_from_text_sums_the_lines_when_the_page_prints_no_total():
    lines = [{"service": "Office visit", "billed_amount_usd": 1450.0}, {"service": "ECG", "billed_amount_usd": 400.0}]
    got, filled, summed = es.fill_from_text({"service_lines": lines}, "no totals here", "eob")
    assert got["billed_amount_usd"] == 1850.0 and summed == ["billed_amount_usd"] and filled == []


def test_fill_from_text_leaves_other_documents_alone_and_survives_junk():
    assert es.fill_from_text({"a": 1}, TOTALS_BOX, "determination_letter") == ({"a": 1}, [], [])
    for junk_ex in (None, [], "x", 5):
        for junk_text in (None, "", 5, ["x"]):
            got, filled, summed = es.fill_from_text(junk_ex, junk_text, "eob")
            assert isinstance(got, dict)
