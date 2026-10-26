"""InboundMailWorkflow, in Temporal's time-skipping test server, with case-api's steps stood in for.
   python -m pytest test_mail_workflow.py"""
from __future__ import annotations

import uuid

import pytest
from temporalio import activity
from temporalio.exceptions import ApplicationError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from mail_workflow import InboundMailWorkflow

calls: list[tuple] = []
behaviour: dict = {}


@activity.defn(name="mail_step")
async def fake_step(tenant: str, job_id: str, step: str, item: str = "", why: str = "") -> dict:
    calls.append((step, item, why))
    rule = behaviour.get((step, item))
    if rule == "refuse":
        raise ApplicationError(f"{step} refused", type="MailStepRefused", non_retryable=True)
    if isinstance(rule, int) and rule > 0:        # fail this many times first, as a scanner that is down
        behaviour[(step, item)] = rule - 1
        raise RuntimeError(f"{step}: not now")
    if step == "receive":
        return {"attachments": behaviour.get("attachments", [])}
    return {"status": "ok"}


async def run(job_id="j1"):
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        q = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=q, workflows=[InboundMailWorkflow], activities=[fake_step]):
            return await env.client.execute_workflow(InboundMailWorkflow.run, {"tenant": "fl", "job_id": job_id}, id=f"MAIL-{uuid.uuid4()}", task_queue=q)


@pytest.fixture(autouse=True)
def reset():
    calls.clear(); behaviour.clear()


@pytest.mark.asyncio
async def test_steps_run_in_order_with_each_attachment():
    behaviour["attachments"] = ["a1", "a2"]
    assert await run() == "DONE"
    steps = [c[0] for c in calls]
    assert steps[0] == "receive" and steps[-2:] == ["file", "deliver"]
    assert sorted(c[1] for c in calls if c[0] == "attachment") == ["a1", "a2"]


@pytest.mark.asyncio
async def test_a_scanner_that_is_down_is_waited_for_not_skipped():
    behaviour["attachments"] = ["a1"]
    behaviour[("attachment", "a1")] = 3            # three failures, then the scan passes
    assert await run() == "DONE"
    assert [c for c in calls if c[0] == "attachment"].__len__() == 4
    assert calls[-1][0] == "deliver"               # filed and delivered only after the attachment was scanned


@pytest.mark.asyncio
async def test_a_refused_step_stops_and_tells_the_pms():
    behaviour[("file", "")] = "refuse"
    assert await run() == "FAILED"
    assert calls[-1][0] == "failed" and "refused" in calls[-1][2]
    assert "deliver" not in [c[0] for c in calls]


@pytest.mark.asyncio
async def test_a_mail_without_attachments_goes_straight_to_filing():
    assert await run() == "DONE"
    assert [c[0] for c in calls] == ["receive", "file", "deliver"]
