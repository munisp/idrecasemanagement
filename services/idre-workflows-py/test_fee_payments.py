# Author: Patrick Munis at newwave
"""FeePaymentWorkflow against Temporal's time-skipping test server, with the
case-api activities replaced: a card paid at once, a declined card, a bank
debit cleared by the webhook's signal, and a bank debit that never clears
(handed to finance after the wait). Run: python -m pytest test_fee_payments.py
"""

import asyncio
import uuid

import pytest
from temporalio import activity
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from fee_payments import FeePaymentWorkflow

CHARGE = {"card-ok": "PAID", "card-declined": "FAILED", "ach-signal": "PROCESSING", "ach-stuck": "PROCESSING"}
calls: list[tuple[str, str]] = []
cleared: set[str] = set()


@activity.defn(name="charge_fee_group")
async def charge(tenant: str, gid: str) -> str:
    calls.append(("charge", gid))
    return CHARGE[gid]


@activity.defn(name="check_fee_group")
async def check(tenant: str, gid: str) -> str:
    calls.append(("check", gid))
    return "PAID" if gid in cleared else "PROCESSING"


@activity.defn(name="give_up_fee_group")
async def give_up(tenant: str, gid: str) -> str:
    calls.append(("give_up", gid))
    return "REVIEW"


@activity.defn(name="summarize_fee_run")
async def summarize(tenant: str, run_id: str) -> dict:
    calls.append(("summary", run_id))
    return {"ok": True}


@pytest.mark.asyncio
async def test_fee_payment_run():
    async with await WorkflowEnvironment.start_time_skipping() as env:
        async with Worker(env.client, task_queue="t", workflows=[FeePaymentWorkflow],
                          activities=[charge, check, give_up, summarize]):
            h = await env.client.start_workflow(
                FeePaymentWorkflow.run, {"tenant": "fl", "run_id": "r1", "group_ids": list(CHARGE)},
                id="fee-pay-" + uuid.uuid4().hex, task_queue="t")
            await asyncio.sleep(0.5)
            # the bank clears one debit; Stripe's webhook signals the workflow
            cleared.add("ach-signal")
            await h.signal(FeePaymentWorkflow.payment_update, "ach-signal")
            out = await h.result()
    st = out["charges"]
    assert st == {"card-ok": "PAID", "card-declined": "FAILED", "ach-signal": "PAID", "ach-stuck": "REVIEW"}, st
    assert [c for c in calls if c[0] == "charge"] == [("charge", g) for g in CHARGE]   # each charged once
    assert ("give_up", "ach-stuck") in calls and ("give_up", "ach-signal") not in calls
    assert not any(c == ("check", "card-ok") for c in calls)                         # a paid card is not re-checked
    assert calls[-1] == ("summary", "r1")
    # the stuck debit was checked on a back-off, not every minute for 10 days
    assert 5 < sum(1 for c in calls if c == ("check", "ach-stuck")) < 80
