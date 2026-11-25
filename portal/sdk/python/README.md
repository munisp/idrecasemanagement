# idre-tpa: Python client for the TPA API

File independent dispute resolution (IDR) disputes for the providers you represent, straight from your own
systems, and follow each one to its decision. It does everything the portal's TPA area does, under the same rules.

- One file (`idre_tpa.py`), one dependency (`requests`), Python 3.9+.
- Tokens are fetched and renewed for you.
- Refusals raise `IdreError` with the API's own plain-language reason.

Full endpoint reference: **https://idre.newfire.app/developers.html** (test) ·
**https://idre-prod.newfire.app/developers.html** (production).

## Install

```bash
curl -O https://idre.newfire.app/sdk/python/idre_tpa.py
python3 -m venv .venv
source .venv/bin/activate
pip install requests
```

On macOS use `python3` (there is no `python` or `pip` until the virtual environment is active). On Windows:
`py -m venv .venv` and `.venv\Scripts\activate`.

## 1. Get an API key

In the portal, open **API keys** in the menu and choose **New key**. Only TPA admins can do this. You get a
**client id** and a **client secret**. The secret is shown once, so store it like a password. Keys belong to one
environment: make test keys on the test site. Every action a key takes appears in your activity log under its name.

## 2. Connect

```python
from idre_tpa import IdreClient

idre = IdreClient(client_id="api-key-…", client_secret="…", environment="test")   # or "production"
print(idre.me()["tpa"]["name"])

# or from IDRE_CLIENT_ID / IDRE_CLIENT_SECRET / IDRE_ENVIRONMENT / IDRE_TENANT:
idre = IdreClient.from_env()
```

## 3. File a dispute

```python
from idre_tpa import Claim, DOCUMENT_SLOTS

# The provider you file for (once per provider).
client_id = idre.add_client("Coral Gables Imaging", contact_email="billing@coralimaging.example")["client_id"]
idre.set_client_profile(client_id, npi="1234567893", tin="59-1234567", address="100 Main St, Miami, FL 33101")
idre.add_payment_method(client_id, "card", label="Visa on file", last4="4242", brand="visa", processor_ref="pm_…")

# This dispute's documents: one per slot (or start_package(client_id, merged=True) + upload_merged_pdf(...)).
package_id = idre.start_package(client_id)["package_id"]
for slot in DOCUMENT_SLOTS:          # EOB, ITEMIZED_BILL, PROOF_OF_COVERAGE, OPEN_NEGOTIATION_NOTICE,
    idre.upload_document(package_id, slot, f"docs/{slot}.pdf")   # OFFER_SUBSTANTIATION, IDRE_ATTESTATION
documents = idre.wait_for_package(package_id)   # each file is read automatically: a minute or two

filed = idre.file_dispute(
    client_id, "Gulf Health Plan",
    [Claim(claim_number="4471", date_of_service="2026-08-02",
           billed_cents=300_000, paid_cents=175_000, disputed_cents=125_000)],
    package_id=package_id, other_party_email="disputes@gulfhealth.example", in_network="no",
)
print(filed["case_number"], filed["status"])     # e.g. FL26-001203 AWAITING_PAYMENT
```

Each document gets a verdict: `ACCEPTED`, `NEEDS_REVIEW` or `REJECTED`. A verdict is a note for the reviewer and
never blocks a filing. `IdreClient.notes_for_reviewer(documents)` lists the slots that weren't accepted, so you can
send a better copy before filing if you like.

A complete script is in [`example_file_dispute.py`](example_file_dispute.py).

## 4. Pay and follow

```python
link = idre.pay_dispute(filed["case_id"])["checkout_url"]   # a payment page; confirmed automatically
idre.pay_fees()                                             # or one page for everything unpaid
idre.fees()                                                 # what you owe
idre.dispute(filed["case_id"])                              # stage, key dates, claims, fee
idre.dashboard()                                            # every dispute you filed
idre.letters()                                              # notices and decisions the office sent you
```

## 5. Many disputes at once

Two ways, both matched to your clients by `party_name`:

```python
idre.bulk_template("template.zip")      # disputes.csv + an example folder of documents per dispute_ref

# a) disputes.csv on its own: every row is filed at once; send documents to each case afterwards.
batch = idre.file_bulk("disputes.csv", batch_ref="2026-11-batch-1")

# b) a ZIP: disputes.csv + one folder per dispute_ref with its six documents (eob.pdf, itemized_bill.pdf, …
#    or one merged.pdf). Each row is filed once all six of its documents are accepted.
batch = idre.file_bulk("disputes.zip", batch_ref="2026-11-batch-2")

result = idre.wait_for_bulk(batch["batch_id"])
for row in result["items"]:
    print(row["dispute_ref"], row["status"], row.get("case_number") or row.get("error"))
    # FILED (with a case number), ERROR (with the reason), or WAITING_DOCS (see row["docs"])
```

`batch_ref` is your own reference. Sending the same one again never files twice, so a retry after a network error
is safe. Replace the template's sample documents and `.example` email addresses with real ones: the server refuses
both.

## 6. When the office asks for something

```python
for request in idre.document_requests()["requests"]:
    if request["status"] == "OPEN":
        idre.add_request_file(request["id"], "itemized-bill.pdf")
        idre.submit_request(request["id"], note="Itemized bill attached.")
```

## Errors

```python
from idre_tpa import IdreError

try:
    idre.file_dispute(...)
except IdreError as error:
    print(error.status, error.message)   # e.g. 409 "these documents are still being read — file once they are …"
```

`404` also means "not yours": you never learn whether another organization's record exists. A refused API key,
because of a wrong secret, a revoked key or the wrong environment, says so.

## Anything else

Every TPA endpoint in the reference can be called directly. Paths are relative to `/v1/tenants/{tenant}`:

```python
idre.get("/tpa/onboarding")
idre.post("/tpa/clients/import", files={"file": open("roster.csv", "rb")})
```
