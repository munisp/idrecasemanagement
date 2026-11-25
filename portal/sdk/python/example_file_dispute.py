"""Worked example: file one dispute for a provider you represent, from start to payment link.

    python3 -m venv .venv && source .venv/bin/activate && pip install requests
    export IDRE_CLIENT_ID=api-key-…  IDRE_CLIENT_SECRET=…   (an API key from the portal: API keys → New key)
    python example_file_dispute.py documents               (a folder with the six PDFs, named by slot)

First fill in the dispute's details in the block at the top of this file. The folder holds EOB.pdf, ITEMIZED_BILL.pdf, PROOF_OF_COVERAGE.pdf, OPEN_NEGOTIATION_NOTICE.pdf,
OFFER_SUBSTANTIATION.pdf and IDRE_ATTESTATION.pdf. Runs against the test site unless IDRE_ENVIRONMENT=production.
"""
import sys
from pathlib import Path

from idre_tpa import DOCUMENT_SLOTS, Claim, IdreClient, IdreError

# ── Fill these in with the real dispute (the server refuses made-up addresses such as *.example) ─────────────
PROVIDER = {"name": "Coral Gables Imaging", "email": "billing@coralimaging.example",
            "npi": "1234567893", "tin": "59-1234567", "address": "100 Main St, Miami, FL 33101"}
PAYMENT = {"method": "card", "label": "Visa on file", "last4": "4242", "brand": "visa", "processor_ref": "pm_…"}
HEALTH_PLAN = {"name": "Gulf Health Plan", "email": "disputes@gulfhealth.example"}
CLAIMS = [Claim(claim_number="4471", date_of_service="2026-08-02",
                billed_cents=300_000, paid_cents=175_000, disputed_cents=125_000)]   # amounts in cents
IN_NETWORK = "no"
# ──────────────────────────────────────────────────────────────────────────────────────────────────────────────


def placeholders() -> list[str]:
    """What still holds the sample values above."""
    left = [f"{where}: {value}" for where, value in (("PROVIDER email", PROVIDER["email"]), ("HEALTH_PLAN email", HEALTH_PLAN["email"]),
                                                     ("PAYMENT processor_ref", PAYMENT["processor_ref"]))
            if value.endswith(".example") or value.endswith("…")]
    return left


def main(folder: Path) -> None:
    left = placeholders()
    if left:
        sys.exit("Fill in the dispute details at the top of this file first; these are still samples:\n  " + "\n  ".join(left))
    missing = [slot for slot in DOCUMENT_SLOTS if not (folder / f"{slot}.pdf").is_file()]
    if missing:
        sys.exit(f"{folder} is missing: " + ", ".join(f"{slot}.pdf" for slot in missing))
    idre = IdreClient.from_env()
    print("Connected as", idre.me()["tpa"]["name"])

    # 1. The provider you file for: reuse it if it's already one of your clients.
    client = next((c for c in idre.clients() if c["party_name"] == PROVIDER["name"]), None)
    if client is None:
        client_id = idre.add_client(PROVIDER["name"], contact_email=PROVIDER["email"])["client_id"]
        idre.set_client_profile(client_id, npi=PROVIDER["npi"], tin=PROVIDER["tin"], address=PROVIDER["address"])
        idre.add_payment_method(client_id, PAYMENT["method"], label=PAYMENT["label"], last4=PAYMENT["last4"],
                                brand=PAYMENT["brand"], processor_ref=PAYMENT["processor_ref"])
    else:
        client_id = client["id"]

    # 2. This dispute's documents: one file per slot, then wait until they have been read (a minute or two).
    package_id = idre.start_package(client_id)["package_id"]
    for slot in DOCUMENT_SLOTS:
        idre.upload_document(package_id, slot, folder / f"{slot}.pdf")
    documents = idre.wait_for_package(package_id)
    for note in IdreClient.notes_for_reviewer(documents):
        print(f"  note for the reviewer: {note['slot']} is {note['status']}")

    # 3. File it. Amounts are in cents.
    filed = idre.file_dispute(client_id, HEALTH_PLAN["name"], CLAIMS, package_id=package_id,
                              other_party_email=HEALTH_PLAN["email"], in_network=IN_NETWORK)
    print("Filed", filed["case_number"], "-", filed["status"])

    # 4. Pay the fee if one is due: open the payment page; payment is confirmed automatically.
    if filed.get("payment_required"):
        print("Pay here:", idre.pay_dispute(filed["case_id"])["checkout_url"])

    # 5. Follow it.
    print("Stage now:", idre.dispute(filed["case_id"])["case"]["status"])


if __name__ == "__main__":
    try:
        main(Path(sys.argv[1] if len(sys.argv) > 1 else "documents"))
    except IdreError as error:
        # error.message is the API's own plain-language reason; error.status the HTTP status.
        sys.exit(f"Refused ({error.status}): {error.message}")
