"""FeePaymentWorkflow — a TPA's fee payment charged to saved cards or bank
accounts (ACH): each client's own, or the TPA's. One workflow per Pay click
(workflow id fee-pay-<run_id>, task queue idre-payments), started by
case-api (tpa_saved_payments.go) with the charges it recorded:

  1. charge every group (one per client, or one for the TPA's method). A
     card answers at once: paid (the disputes open) or declined;
  2. a bank debit stays PROCESSING for days. The workflow waits for it:
     woken by Stripe's webhook (signal payment_update), and checking anyway on
     a back-off (1 min, doubling to 6 h) in case a webhook never comes. Each
     group is settled the moment it clears, or marked failed when the bank
     returns it;
  3. a debit still pending after 10 days goes to finance (never marked failed:
     the money may yet arrive);
  4. the TPA is told how the run went: paid, declined (with the reason), or
     with finance.

The activities call case-api's internal endpoints; case-api holds the Stripe
key, the settlement logic and the idempotency keys, so a retried activity
never charges twice.
"""

from __future__ import annotations

import asyncio
from datetime import timedelta

from temporalio import activity, workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    import os

    import httpx

CASE_API = os.environ.get("CASE_API_URL", "http://localhost:8080")


def _internal() -> dict:
    return {"X-Internal-Auth": os.environ.get("VAULT_INTERNAL_TOKEN", "")}


async def _post(path: str) -> dict:
    async with httpx.AsyncClient(base_url=CASE_API, timeout=60) as client:
        resp = await client.post(path, headers=_internal())
        resp.raise_for_status()  # 5xx (Stripe unreachable) -> the activity retries
        return resp.json()


@activity.defn
async def charge_fee_group(tenant: str, group_id: str) -> str:
    """PAID | PROCESSING | FAILED | REVIEW"""
    return (await _post(f"/internal/payments/{tenant}/groups/{group_id}/charge"))["status"]


@activity.defn
async def check_fee_group(tenant: str, group_id: str) -> str:
    return (await _post(f"/internal/payments/{tenant}/groups/{group_id}/check"))["status"]


@activity.defn
async def give_up_fee_group(tenant: str, group_id: str) -> str:
    return (await _post(f"/internal/payments/{tenant}/groups/{group_id}/give-up"))["status"]


@activity.defn
async def summarize_fee_run(tenant: str, run_id: str) -> dict:
    return await _post(f"/internal/payments/{tenant}/runs/{run_id}/summary")


_RETRY = RetryPolicy(initial_interval=timedelta(seconds=5), backoff_coefficient=2.0,
                     maximum_interval=timedelta(minutes=5), maximum_attempts=20)
_SHORT = dict(start_to_close_timeout=timedelta(seconds=90), retry_policy=_RETRY)

PENDING = {"PROCESSING", "NOT_CHARGED"}
MAX_WAIT = timedelta(days=10)       # ACH settles in ~4 business days; returns can take longer
FIRST_CHECK = timedelta(minutes=1)
LONGEST_CHECK = timedelta(hours=6)


@workflow.defn(name="FeePaymentWorkflow")
class FeePaymentWorkflow:
    def __init__(self) -> None:
        self.updates: set[str] = set()
        self.status: dict[str, str] = {}

    @workflow.signal
    def payment_update(self, group_id: str) -> None:
        """Stripe reported on a charge (case-api's webhook)."""
        self.updates.add(group_id)

    @workflow.query
    def charges(self) -> dict:
        return dict(self.status)

    @workflow.run
    async def run(self, inp: dict) -> dict:
        tenant, run_id, groups = inp["tenant"], inp["run_id"], list(inp["group_ids"])
        for gid in groups:
            self.status[gid] = await workflow.execute_activity(charge_fee_group, args=[tenant, gid], **_SHORT)

        waited, every = timedelta(0), FIRST_CHECK
        while any(st in PENDING for st in self.status.values()) and waited < MAX_WAIT:
            pending = {g for g, st in self.status.items() if st in PENDING}
            started = workflow.now()
            try:
                await workflow.wait_condition(lambda: bool(self.updates & pending), timeout=every)
            except asyncio.TimeoutError:
                every = min(every * 2, LONGEST_CHECK)
            waited += workflow.now() - started
            woken, self.updates = self.updates & pending, self.updates - pending
            for gid in (woken or pending):
                st = await workflow.execute_activity(check_fee_group, args=[tenant, gid], **_SHORT)
                if st == "NOT_CHARGED":  # the charge never reached Stripe: try it again
                    st = await workflow.execute_activity(charge_fee_group, args=[tenant, gid], **_SHORT)
                self.status[gid] = st

        for gid, st in list(self.status.items()):
            if st in PENDING:
                self.status[gid] = await workflow.execute_activity(give_up_fee_group, args=[tenant, gid], **_SHORT)

        summary = await workflow.execute_activity(summarize_fee_run, args=[tenant, run_id], **_SHORT)
        return {"charges": self.status, "summary": summary}
