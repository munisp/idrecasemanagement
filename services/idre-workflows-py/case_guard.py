"""Ends a case's workflow once its case is gone from the database.

Activities run under Temporal's default retry policy, which retries forever. A case deleted out from under its
workflow (test data cleared, a case purged) would otherwise make each later activity fail and retry about every
100 s for good, or write timeline rows and staff notifications for a case nobody can open. Every case-scoped
activity calls ensure_case() first.

A case still missing on the activity's third attempt (seconds apart, so a replica's lag or a case committed a moment
ago cannot trip it) raises a non-retryable CaseGone error: the workflow fails and closes. Until then, and on any
error while checking, the activity retries exactly as before.
"""
from __future__ import annotations

import re

import psycopg
from temporalio import activity
from temporalio.exceptions import ApplicationError


# A tenant as it names a schema: lower-case letters, digits and "_", as case-api provisions them (2026-11-26).
_TENANT_RE = re.compile(r"[a-z][a-z0-9_]{0,40}")


def tenant_schema(tenant: str) -> str:
    """tenant_<x> for a well-formed tenant; anything else is refused before it reaches SQL."""
    if not _TENANT_RE.fullmatch(str(tenant or "")):
        raise ValueError(f"malformed tenant {str(tenant)[:40]!r}")
    return "tenant_" + tenant


_TENANT = re.compile(r"^[a-z]{2,8}$")
CONFIRM_ATTEMPTS = 3


async def case_exists(dsn: str, tenant: str, case_id: str) -> bool:
    async with await psycopg.AsyncConnection.connect(dsn, autocommit=True) as conn:
        cur = await conn.execute(f"SELECT 1 FROM {tenant_schema(tenant)}.cases WHERE id=%s", (case_id,))  # nosec B608  # tenant_schema() validates the tenant; values are bound
        return await cur.fetchone() is not None


async def ensure_case(dsn: str, tenant: str, case_id: str) -> None:
    """Return when the case exists; raise when it does not (retryable until CONFIRM_ATTEMPTS, then final)."""
    if not case_id or not _TENANT.match(tenant or ""):
        return  # nothing safe to check: behave as before
    if await case_exists(dsn, tenant, case_id):
        return
    attempt = activity.info().attempt
    if attempt < CONFIRM_ATTEMPTS:
        raise ApplicationError(f"case {case_id} not found in tenant {tenant} (attempt {attempt}); checking again",
                               type="CaseMissing")
    raise ApplicationError(f"case {case_id} no longer exists in tenant {tenant}; ending its workflow",
                           type="CaseGone", non_retryable=True)
