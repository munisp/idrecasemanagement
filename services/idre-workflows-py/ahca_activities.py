# Author: Patrick Munis at newwave
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
async def recommendation_days_left(tenant: str, case_id: str) -> int:
    """Days until the recommendation is due to the agency: Date Received + 60
    calendar days, counted from today. 0 or less means the date has passed.
    A case with no Date Received on record has no days left (the caller's own
    60-day timer has already run)."""
    with _conn() as c:
        row = c.execute(
            f"SELECT (program_dates->>'received_at')::date + 60 - current_date FROM tenant_{tenant}.cases "
            f"WHERE id=%s AND coalesce(program_dates->>'received_at','') ~ '^\\d{{4}}-\\d{{2}}-\\d{{2}}'",
            (case_id,),
        ).fetchone()
    return int(row[0]) if row and row[0] is not None else 0


@activity.defn
async def notify_roles(tenant: str, case_id: str, roles: list[str], typ: str, body: str) -> None:
    """Tell the people who must act, not the whole tenant: one notification
    per role (user_sub 'role:<ROLE>', which case-api shows to holders of that
    role), plus a line on the case timeline."""
    with _conn() as c:
        c.execute(
            "INSERT INTO public.case_activities (tenant, case_id, type, body) VALUES (%s,%s,%s,%s)",
            (tenant, case_id, typ, body),
        )
        for role in roles:
            c.execute(
                "INSERT INTO public.notifications (tenant, user_sub, type, body, link) VALUES (%s,%s,%s,%s,%s)",
                (tenant, f"role:{role}", typ, f"Case {case_id}: {body}", f"#/cases/{case_id}"),
            )


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
    """A deadline follow-up firing (day 13/14/15 calls and emails, "send
    this letter" prompts): something a person must now do, not yet a breach.

    It becomes three things: a line on the case timeline, a notification to
    the Reviewers (the role that chases deadlines -- not the whole tenant),
    and an open TASK due today, assigned to whoever holds the case, so it
    sits in someone's task list until it is done.

    The "tell AHCA at day 13" follow-up applies to Medicaid plans only: for a
    case recorded as another line of business it is skipped."""
    with _conn() as c:
        row = c.execute(
            f"SELECT case_number, coalesce(assigned_to,''), coalesce(details->>'line_of_business','') "
            f"FROM tenant_{tenant}.cases WHERE id=%s", (case_id,),
        ).fetchone()
        case_number, assignee, lob = row if row else ("", "", "")
        if action == "medicaid_ahca_notify":
            if lob and lob.lower() != "medicaid":
                return
            if not lob:
                detail += " (only if this is a Medicaid plan — the line of business is not recorded on the case)"
        c.execute(
            "INSERT INTO public.case_activities (tenant, case_id, type, body) VALUES (%s,%s,'FOLLOWUP_ACTION',%s)",
            (tenant, case_id, f"{action}: {detail}"),
        )
        c.execute(
            "INSERT INTO public.notifications (tenant, user_sub, type, body, link) VALUES (%s,'role:CASE_MANAGER','FOLLOWUP_ACTION',%s,%s)",
            (tenant, f"Case {case_number or case_id}: {detail}", f"#/cases/{case_id}"),
        )
        c.execute(
            "INSERT INTO public.tasks (tenant, subject, case_id, assignee, due_date, created_by) "
            "VALUES (%s,%s,%s,%s,current_date,'system:deadlines')",
            (tenant, f"{case_number or 'Case'}: {detail}"[:480], case_id, assignee),
        )
