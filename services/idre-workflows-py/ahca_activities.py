"""Activities for AhcaDisputeWorkflow — side effects only, workflow stays
deterministic. Mirrors activities.py's direct-DB-plus-HTTP pattern rather
than inventing a new one."""

from __future__ import annotations

import os

import httpx
import psycopg
from temporalio import activity

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
CASE_API = os.environ.get("CASE_API_URL", "http://localhost:8080")


def _conn():
    return psycopg.connect(DSN, autocommit=True)


def _auth_headers() -> dict:
    return {"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"}


@activity.defn
async def set_dual_status(tenant: str, case_id: str, internal_status: str, agency_status: str = "") -> None:
    async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
        resp = await client.post(
            f"/v1/tenants/{tenant}/cases/{case_id}/status",
            json={"internal_status": internal_status, "agency_status": agency_status},
            headers=_auth_headers(),
        )
        resp.raise_for_status()


@activity.defn
async def set_program_date(tenant: str, case_id: str, key: str, value: str) -> None:
    async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
        resp = await client.post(
            f"/v1/tenants/{tenant}/cases/{case_id}/program-date",
            json={"key": key, "value": value},
            headers=_auth_headers(),
        )
        resp.raise_for_status()


@activity.defn
async def send_correspondence(tenant: str, case_id: str, template: str, body: str) -> None:
    """Drafts/sends a real templated email via the same endpoint the portal
    uses, with to/cc left empty so resolveRecipients fills them server-side
    from the template's role list -- the workflow doesn't need to know real
    addresses, same as draftCorrespondence already supports for any caller
    that omits them. Used for the RFI day-15 lapse notice, which the source
    narrative lists as its own named email template, not just a logged
    reminder."""
    async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
        resp = await client.post(
            f"/v1/tenants/{tenant}/cases/{case_id}/correspondence",
            json={"template": template, "body": body, "to": [], "cc": []},
            headers=_auth_headers(),
        )
        resp.raise_for_status()


@activity.defn
async def set_case_outcome(
    tenant: str, case_id: str,
    case_outcome: str = "", party_billed: str = "",
    final_amount_awarded_cents: int | None = None, num_claims_reviewed: int | None = None,
) -> None:
    """Bundles Case Outcome / Party Billed / Final Amount Awarded / Number
    of Claims Reviewed into the same action that closes out Attorney
    review, instead of leaving them as a separate step someone has to
    remember -- fields are only sent when provided, via the same
    setCaseDetails route (and field_schema validation) the portal uses."""
    fields: dict = {}
    if case_outcome:
        fields["case_outcome"] = case_outcome
    if party_billed:
        fields["party_billed"] = party_billed
    if final_amount_awarded_cents is not None:
        fields["final_amount_awarded_cents"] = final_amount_awarded_cents
    if num_claims_reviewed is not None:
        fields["num_claims_reviewed"] = num_claims_reviewed
    if not fields:
        return
    async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
        resp = await client.patch(
            f"/v1/tenants/{tenant}/cases/{case_id}/details",
            json=fields,
            headers=_auth_headers(),
        )
        resp.raise_for_status()


@activity.defn
async def record_followup_action(tenant: str, case_id: str, action: str, detail: str) -> None:
    """A day-13/14/15 follow_ups[] action firing — a reminder to staff to
    make a call/send a reply-all, not yet a breach. Logged on the case
    timeline (same table doc-intel writes analysis results to) and
    broadcast in-app so the Reviewer actually sees it. No HTTP route
    exposes s.notify directly (confirmed -- only called internally by
    handlers), so this writes public.notifications straight, same shape
    as a '*'-broadcast call to s.notify."""
    with _conn() as c:
        c.execute(
            "INSERT INTO public.case_activities (tenant, case_id, type, body) VALUES (%s,%s,'FOLLOWUP_ACTION',%s)",
            (tenant, case_id, f"{action}: {detail}"),
        )
        c.execute(
            "INSERT INTO public.notifications (tenant, user_sub, type, body, link) VALUES (%s,'*','FOLLOWUP_ACTION',%s,%s)",
            (tenant, f"Case {case_id}: {action} — {detail}", f"#/cases/{case_id}"),
        )
