"""Python client for the dispute resolution portal's TPA API.

File independent dispute resolution (IDR) disputes for the providers you represent, from your own systems:
add a client, get its documents accepted, file one dispute or a whole batch, pay the fees, and follow each
dispute through to its decision. Everything here is also documented at <site>/developers.html.

    from idre_tpa import Claim, IdreClient

    idre = IdreClient(client_id="api-key-…", client_secret="…", environment="test")
    print(idre.me()["tpa"]["name"])

One file, one dependency (`requests`). Python 3.9+.
"""
from __future__ import annotations

import mimetypes
import os
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import IO, Any, Iterable, Optional, Union

import requests

__version__ = "1.0.0"
__all__ = ["IdreClient", "IdreError", "Claim", "ENVIRONMENTS", "DOCUMENT_SLOTS"]

#: Where each environment lives. Keys belong to one environment: make test keys on the test site.
ENVIRONMENTS = {
    "test": {
        "base_url": "https://idre.newfire.app",
        "token_url": "https://keycloak-servers.newfire.app/realms/idre/protocol/openid-connect/token",
    },
    "production": {
        "base_url": "https://idre-prod.newfire.app",
        "token_url": "https://keycloak-servers.newfire.app/realms/idre-prod/protocol/openid-connect/token",
    },
}

#: The six documents every dispute needs, one per slot of a document package.
DOCUMENT_SLOTS = (
    "EOB", "ITEMIZED_BILL", "PROOF_OF_COVERAGE", "OPEN_NEGOTIATION_NOTICE", "OFFER_SUBSTANTIATION", "IDRE_ATTESTATION",
)

_READING = ("PENDING", "ANALYZING")

FileLike = Union[str, os.PathLike, IO[bytes]]


class IdreError(Exception):
    """The API refused a request or couldn't be reached.

    `status` is the HTTP status (0 when the service couldn't be reached) and `message` the API's own plain-language
    reason. A 404 also means "not yours": you never learn whether another organization's record exists.
    """

    def __init__(self, status: int, message: str, body: Any = None, method: str = "", path: str = ""):
        super().__init__(f"{status} on {method} {path}: {message}" if status else message)
        self.status = status
        self.message = message
        self.body = body
        self.method = method
        self.path = path


@dataclass
class Claim:
    """One claim in a dispute. Amounts are in cents; dates are YYYY-MM-DD."""

    claim_number: str
    date_of_service: str
    billed_cents: int
    paid_cents: int
    disputed_cents: int
    type_of_service: Optional[str] = None
    patient_first_name: Optional[str] = None
    patient_last_name: Optional[str] = None
    date_claim_submitted: Optional[str] = None
    date_of_denial: Optional[str] = None
    date_of_final_determination: Optional[str] = None

    def as_json(self) -> dict:
        return {key: value for key, value in asdict(self).items() if value is not None}


@dataclass
class _Token:
    value: str = ""
    expires_at: float = 0.0


class IdreClient:
    """A TPA's connection to the API, authenticated with one API key.

    Make the key in the portal: menu → API keys → New key (TPA admins). Tokens are fetched and renewed for you.

    Args:
        client_id, client_secret: the API key (the secret is shown once when the key is made).
        environment: "test" or "production" (see ENVIRONMENTS), or pass base_url and token_url yourself.
        tenant: your state program, e.g. "fl".
        timeout: seconds per request.
    """

    def __init__(self, client_id: str, client_secret: str, environment: str = "test", tenant: str = "fl",
                 base_url: Optional[str] = None, token_url: Optional[str] = None, timeout: float = 60.0,
                 session: Optional[requests.Session] = None):
        if not client_id or not client_secret:
            raise ValueError("client_id and client_secret are required (make an API key in the portal)")
        if environment not in ENVIRONMENTS and not (base_url and token_url):
            raise ValueError(f"environment must be one of {sorted(ENVIRONMENTS)}, or pass base_url and token_url")
        env = ENVIRONMENTS.get(environment, {})
        self.base_url = (base_url or env["base_url"]).rstrip("/")
        self.token_url = token_url or env["token_url"]
        self.tenant = tenant
        self.timeout = timeout
        self._client_id = client_id
        self._client_secret = client_secret
        self._token = _Token()
        self._http = session or requests.Session()
        self._http.headers["User-Agent"] = f"idre-tpa-python/{__version__}"

    @classmethod
    def from_env(cls, **overrides: Any) -> "IdreClient":
        """Reads IDRE_CLIENT_ID, IDRE_CLIENT_SECRET and optionally IDRE_ENVIRONMENT (test) and IDRE_TENANT (fl)."""
        return cls(client_id=os.environ.get("IDRE_CLIENT_ID", ""), client_secret=os.environ.get("IDRE_CLIENT_SECRET", ""),
                   environment=os.environ.get("IDRE_ENVIRONMENT", "test"), tenant=os.environ.get("IDRE_TENANT", "fl"),
                   **overrides)

    # ── Your organization ────────────────────────────────────────────────────────────────────────────────

    def me(self) -> dict:
        """Your TPA and this key's standing in it. A good first call to check a key works."""
        return self.get("/tpa/me")

    def team(self) -> dict:
        return self.get("/tpa/team")

    # ── Clients (the providers you file for) ─────────────────────────────────────────────────────────────

    def clients(self) -> list[dict]:
        """Your clients, with each one's onboarding status."""
        return self.get("/tpa/clients").get("clients", [])

    def add_client(self, party_name: str, contact_email: str, party_type: str = "PROVIDER") -> dict:
        """Adds a provider you file for. Next: its profile, a payment method and its document package."""
        return self.post("/tpa/clients", json={"party_name": party_name, "contact_email": contact_email, "party_type": party_type})

    def set_client_profile(self, client_id: str, npi: str, tin: str, address: str) -> dict:
        """NPI, TIN and address. The NPI is checked against the national registry (see `npi_status`)."""
        return self.put(f"/tpa/clients/{client_id}/profile", json={"npi": npi, "tin": tin, "address": address})

    def add_payment_method(self, client_id: str, method: str, *, label: str = "", last4: str = "", brand: str = "",
                           routing: str = "", processor_ref: str = "", auth_received: bool = True) -> dict:
        """Records the client's payment method on file. It isn't charged when you file; you pay fees yourself."""
        body = {"method": method, "label": label, "last4": last4, "brand": brand, "routing": routing,
                "processor_ref": processor_ref, "auth_received": auth_received}
        return self.post(f"/tpa/clients/{client_id}/payments", json={k: v for k, v in body.items() if v not in ("", None)})

    # ── Document packages (the six documents a dispute needs) ────────────────────────────────────────────

    def start_package(self, client_id: str, merged: bool = False) -> dict:
        """Starts a document set for one dispute. Upload into its slots (`upload_document`), or with merged=True
        send one PDF holding all six (`upload_merged_pdf`), which is split into the slots for you."""
        return self.post(f"/tpa/clients/{client_id}/packages", json={"mode": "MERGED" if merged else "DISCRETE"})

    def upload_document(self, package_id: str, slot: str, file: FileLike, filename: Optional[str] = None) -> dict:
        """Uploads one document (PDF or image) into a slot, e.g. "EOB". Extra documents go in "MORE_<KIND>" slots.
        Each document is checked automatically: wait for the verdicts with `wait_for_package`."""
        if slot not in DOCUMENT_SLOTS and not slot.startswith("MORE_"):
            raise ValueError(f"unknown slot {slot!r}: use one of {DOCUMENT_SLOTS} or MORE_<KIND>")
        return self.post(f"/tpa/packages/{package_id}/docs", data={"slot": slot}, files=_file_field(file, filename))

    def upload_merged_pdf(self, package_id: str, file: FileLike, filename: Optional[str] = None) -> dict:
        """Uploads all six documents as one PDF; it is split into the slots for you."""
        return self.post(f"/tpa/packages/{package_id}/merged", files=_file_field(file, filename))

    def package(self, package_id: str) -> dict:
        """The set's status and each slot's verdict: ACCEPTED, NEEDS_REVIEW, REJECTED, MISSING (no file), or
        PENDING/ANALYZING while it is being read. A verdict is a note for the reviewer, never a block."""
        return self.get(f"/tpa/packages/{package_id}")

    def wait_for_package(self, package_id: str, timeout: float = 600, interval: float = 5) -> dict:
        """Waits until every uploaded document has been read (usually a minute or two), then returns the set.

        A dispute can be filed with the set from then on. Slots the reviewer will look at more closely are in
        `notes_for_reviewer(state)`.
        """
        deadline = time.monotonic() + timeout
        while True:
            state = self.package(package_id)
            slots = state.get("slots", []) + state.get("more", [])
            uploaded = [slot for slot in slots if slot.get("status") != "MISSING"]
            reading = [slot for slot in uploaded if slot.get("status") in _READING]
            if state.get("status") != "VALIDATING" and uploaded and not reading:
                return state
            if time.monotonic() >= deadline:
                raise IdreError(0, f"package {package_id} was still being read after {timeout:.0f}s", state)
            time.sleep(interval)

    @staticmethod
    def notes_for_reviewer(package_state: dict) -> list[dict]:
        """Slots that aren't ACCEPTED (missing, needing review or rejected), with the reasons given."""
        return [{"slot": s.get("slot"), "status": s.get("status"), "reasons": s.get("reasons")}
                for s in package_state.get("slots", []) if s.get("status") != "ACCEPTED"]

    # ── Filing ───────────────────────────────────────────────────────────────────────────────────────────

    def file_dispute(self, client_id: str, other_party_name: str, claims: Iterable[Claim], *, package_id: str = "",
                     other_party_email: str = "", other_party_naic: str = "", in_network: str = "",
                     provider_type: str = "", contact_name: str = "", notes: str = "",
                     disputed_amount_cents: Optional[int] = None, details: Optional[dict] = None) -> dict:
        """Files one dispute for one of your clients, against `other_party_name` (the health plan).

        `package_id` is the dispute's document set (see `start_package`); once given, its documents must have been
        read (`wait_for_package`). Your state program may require one. The client's NPI must not be blocked. The
        disputed amount defaults to the sum of the claims' `disputed_cents`. The answer has `case_id`, `case_number` and whether a fee is due
        (`payment_required`); pay it with `pay_fees` or `pay_dispute`.
        """
        claims = list(claims)
        total = sum(c.disputed_cents for c in claims) if disputed_amount_cents is None else disputed_amount_cents
        if total <= 0:
            raise ValueError("the disputed amount must be above zero")
        packet: dict[str, Any] = {"claims": [c.as_json() for c in claims], "disputed_amount_cents": total}
        if details:
            packet["details"] = details
        body = {"client_id": client_id, "package_id": package_id, "other_party_name": other_party_name,
                "disputed_amount_cents": total, "packet": packet, "other_party_email": other_party_email,
                "other_party_naic": other_party_naic, "in_network": in_network, "provider_type": provider_type,
                "contact_name": contact_name, "notes": notes}
        return self.post("/tpa/intake", json={k: v for k, v in body.items() if v not in ("", None)})

    def bulk_template(self, save_to: Union[str, os.PathLike]) -> Path:
        """Downloads the bulk filing template: `disputes.csv` plus an example folder of documents."""
        path = Path(save_to)
        path.write_bytes(self._request("GET", "/tpa/disputes/bulk/sample.zip", raw=True))
        return path

    def file_bulk(self, file: FileLike, batch_ref: str) -> dict:
        """Files many disputes at once, from either:

        - a ZIP: `disputes.csv` plus one folder per `dispute_ref` with that dispute's six documents (or one
          merged.pdf). Each row waits as WAITING_DOCS until all six of its documents are accepted, then is filed.
        - `disputes.csv` on its own: every row is filed at once; send documents to each case afterwards.

        Rows are matched to your clients by `party_name`. `batch_ref` is your own reference: sending the same one
        again never files twice, so a retry is safe. Follow the rows with `wait_for_bulk`.
        """
        if not batch_ref:
            raise ValueError("batch_ref is required: it is what makes a retry safe")
        try:
            return self.post("/tpa/disputes/bulk", data={"batch_ref": batch_ref}, files=_file_field(file, None))
        except IdreError as error:
            # The same batch_ref is still being uploaded (e.g. a retry overlapping the first attempt): nothing is filed
            # twice, so answer with that batch instead of an error.
            if error.status != 409 or "batch_ref" not in error.message:
                raise
            for _ in range(30):
                for row in self.bulk_batches().get("batches", []):
                    if row.get("batch_ref") == batch_ref:
                        return {"batch_id": row.get("id"), "batch_ref": batch_ref, "already_sent": True}
                time.sleep(2)
            raise

    def bulk_batch(self, batch_id: str) -> dict:
        """Each row's outcome: FILED (with its case number), waiting for documents, or refused and why."""
        return self.get(f"/tpa/disputes/bulk/{batch_id}")

    def bulk_batches(self) -> dict:
        """Your batches, newest first: `{"batches": [{"id", "batch_ref", "rows", "filed", "refused", "waiting",
        "unpaid", "unpaid_cents", "created_at"}, …]}`."""
        return self.get("/tpa/disputes/bulk")

    def wait_for_bulk(self, batch_id: str, timeout: float = 900, interval: float = 10, until_filed: bool = False) -> dict:
        """Polls a bulk filing until every row has an outcome: FILED (with `case_number`), ERROR (with `error`),
        or WAITING_DOCS (its documents are still being read, or one needs replacing; see the row's `docs`).
        With until_filed=True it keeps waiting while rows are WAITING_DOCS."""
        deadline = time.monotonic() + timeout
        while True:
            state = self.bulk_batch(batch_id)
            waiting = ("WAITING_DOCS",) if until_filed else ()
            busy = [i for i in state.get("items", []) if str(i.get("status", "")).upper() in ("PENDING", "PROCESSING", "QUEUED", "RECEIVED", "FILING") + waiting
                    or (str(i.get("status", "")).upper() == "WAITING_DOCS" and any(d.get("status") in _READING for d in i.get("docs") or []))]
            if state.get("items") and not busy:
                return state
            if time.monotonic() >= deadline:
                raise IdreError(0, f"bulk filing {batch_id} still had {len(busy)} rows in progress after {timeout:.0f}s", state)
            time.sleep(interval)

    # ── Fees ─────────────────────────────────────────────────────────────────────────────────────────────

    def fees(self) -> dict:
        """What you owe: `amount_cents`, `card_fee_cents` and how many disputes are `unpaid`."""
        return self.get("/tpa/fees")

    def dispute_fee(self, case_id: str) -> dict:
        return self.get(f"/tpa/disputes/{case_id}/fee")

    def pay_fees(self, case_ids: Optional[list[str]] = None, method: str = "card") -> dict:
        """A payment page (`checkout_url`) for every unpaid fee, or only `case_ids`. Payment is confirmed
        automatically; check with `fees()` or the dispute afterwards. `method`: "card" (adds a card fee) or "bank"."""
        body: dict[str, Any] = {"source": "CHECKOUT", "method": method}
        if case_ids:
            body["case_ids"] = case_ids
        return self.post("/tpa/fees/checkout", json=body)

    def pay_dispute(self, case_id: str, method: str = "card") -> dict:
        return self.post(f"/tpa/disputes/{case_id}/pay", json={"source": "CHECKOUT", "method": method})

    def pay_bulk(self, batch_id: str, method: str = "card") -> dict:
        return self.post(f"/tpa/disputes/bulk/{batch_id}/checkout", json={"source": "CHECKOUT", "method": method})

    # ── Following disputes ───────────────────────────────────────────────────────────────────────────────

    def dashboard(self) -> dict:
        """Every dispute you filed with its stage, plus totals per client."""
        return self.get("/tpa/dashboard")

    def dispute(self, case_id: str) -> dict:
        """One dispute at a glance: stage, key dates, claims and fee."""
        return self.get(f"/tpa/disputes/{case_id}/summary")

    def dispute_deadlines(self, case_id: str) -> dict:
        return self.get(f"/cases/{case_id}/clocks")

    def dispute_documents(self, case_id: str) -> dict:
        return self.get(f"/cases/{case_id}/documents")

    def download_dispute_document(self, case_id: str, doc_id: str, save_to: Union[str, os.PathLike]) -> Path:
        path = Path(save_to)
        path.write_bytes(self._request("GET", f"/cases/{case_id}/documents/{doc_id}/download", raw=True))
        return path

    def letters(self) -> dict:
        """Letters the office sent you (notices, decisions)."""
        return self.get("/tpa/sent-documents")

    # ── When the office asks for something ───────────────────────────────────────────────────────────────

    def document_requests(self) -> dict:
        """Documents the office asked for, with due dates (`open` = how many still need an answer)."""
        return self.get("/tpa/document-requests")

    def add_request_file(self, request_id: str, file: FileLike, filename: Optional[str] = None) -> dict:
        return self.post(f"/tpa/document-requests/{request_id}/files", files=_file_field(file, filename))

    def submit_request(self, request_id: str, note: str = "", decision: Optional[str] = None) -> dict:
        """Sends the files you added. For a permission request, answer with decision="PROCEED" or "DECLINE"."""
        body: dict[str, Any] = {}
        if note:
            body["note"] = note
        if decision:
            body["decision"] = decision
        return self.post(f"/tpa/document-requests/{request_id}/submit", json=body)

    # ── Any endpoint ─────────────────────────────────────────────────────────────────────────────────────
    # Paths are relative to /v1/tenants/{tenant}, e.g. client.get("/tpa/onboarding").

    def get(self, path: str, **params: Any) -> Any:
        return self._request("GET", path, params=params or None)

    def post(self, path: str, json: Any = None, data: Optional[dict] = None, files: Optional[dict] = None) -> Any:
        return self._request("POST", path, json=json, data=data, files=files)

    def put(self, path: str, json: Any = None) -> Any:
        return self._request("PUT", path, json=json)

    def patch(self, path: str, json: Any = None) -> Any:
        return self._request("PATCH", path, json=json)

    def delete(self, path: str) -> Any:
        return self._request("DELETE", path)

    # ── Plumbing ─────────────────────────────────────────────────────────────────────────────────────────

    def _access_token(self, refresh: bool = False) -> str:
        if refresh or not self._token.value or time.time() >= self._token.expires_at:
            try:
                response = self._http.post(self.token_url, timeout=self.timeout, data={
                    "grant_type": "client_credentials", "client_id": self._client_id, "client_secret": self._client_secret})
            except requests.RequestException as exc:
                raise IdreError(0, f"the sign-in service couldn't be reached: {exc}") from exc
            if response.status_code != 200:
                raise IdreError(response.status_code, "the API key was refused: check the client id and secret, "
                                "that the key wasn't revoked, and that it belongs to this environment", _body(response), "POST", self.token_url)
            payload = response.json()
            # Renew 30 seconds early so a token never expires mid-request.
            self._token = _Token(payload["access_token"], time.time() + max(int(payload.get("expires_in", 300)) - 30, 5))
        return self._token.value

    def _request(self, method: str, path: str, raw: bool = False, **kwargs: Any) -> Any:
        url = f"{self.base_url}/v1/tenants/{self.tenant}{path}"
        files = kwargs.get("files")
        for attempt in (1, 2):
            if files and attempt == 2:
                for _name, value in files.items():  # rewind uploads before the retry
                    handle = value[1] if isinstance(value, tuple) else value
                    if hasattr(handle, "seek"):
                        handle.seek(0)
            headers = {"Authorization": f"Bearer {self._access_token(refresh=attempt == 2)}", "Accept": "application/json"}
            try:
                response = self._http.request(method, url, headers=headers, timeout=self.timeout, **kwargs)
            except requests.RequestException as exc:
                raise IdreError(0, f"the API couldn't be reached: {exc}", None, method, path) from exc
            if response.status_code == 401 and attempt == 1:
                continue  # the token was revoked or expired early: get a fresh one once
            break
        if response.status_code >= 400:
            body = _body(response)
            message = body.get("error") if isinstance(body, dict) and isinstance(body.get("error"), str) else (
                body.get("error", {}).get("message") if isinstance(body, dict) and isinstance(body.get("error"), dict) else None)
            raise IdreError(response.status_code, message or f"request failed with HTTP {response.status_code}", body, method, path)
        if raw:
            return response.content
        return response.json() if response.content else None


def _body(response: requests.Response) -> Any:
    try:
        return response.json()
    except ValueError:
        return response.text[:500]


def _file_field(file: FileLike, filename: Optional[str], content_type: Optional[str] = None) -> dict:
    """A multipart `file` field from a path or an open binary file."""
    if isinstance(file, (str, os.PathLike)):
        path = Path(file)
        name = filename or path.name
        handle: IO[bytes] = open(path, "rb")  # noqa: SIM115 - requests reads it during the call
    else:
        handle = file
        name = filename or Path(getattr(file, "name", "document.pdf")).name
    kind = content_type or mimetypes.guess_type(name)[0] or "application/octet-stream"
    return {"file": (name, handle, kind)}
