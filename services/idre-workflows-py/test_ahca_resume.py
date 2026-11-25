# Author: Patrick Munis at newwave
"""A resumed AhcaDisputeWorkflow catches up without repeating anything.

Runs the real workflow on Temporal's time-skipping test server with fake
activities that only record their calls.
"""

from __future__ import annotations

import asyncio
import uuid

import pytest
from temporalio import activity
from temporalio.client import WorkflowExecutionStatus
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from ahca_workflow import AhcaDisputeWorkflow

calls: list[tuple] = []
dates: dict = {}  # the last value each program date was set to


@activity.defn(name="set_dual_status")
async def set_dual_status(tenant, case_id, internal_status, agency_status=""):
    calls.append(("set_dual_status", internal_status))


@activity.defn(name="set_program_date")
async def set_program_date(tenant, case_id, key, value):
    calls.append(("set_program_date", key))
    dates[key] = value


@activity.defn(name="recommendation_days_left")
async def recommendation_days_left(tenant, case_id):
    calls.append(("recommendation_days_left",))
    return 40


@activity.defn(name="notify_roles")
async def notify_roles(tenant, case_id, roles, typ, body):
    calls.append(("notify_roles", typ))


@activity.defn(name="send_correspondence")
async def send_correspondence(tenant, case_id, template, body):
    calls.append(("send_correspondence", template))


@activity.defn(name="set_case_outcome")
async def set_case_outcome(tenant, case_id, *args):
    calls.append(("set_case_outcome",))


@activity.defn(name="record_followup_action")
async def record_followup_action(tenant, case_id, action, detail):
    calls.append(("record_followup_action", action))


@activity.defn(name="flag_cms_breach")
async def flag_cms_breach(tenant, case_id, clock, detail):
    calls.append(("flag_cms_breach", clock))


ACTIVITIES = [set_dual_status, set_program_date, recommendation_days_left, notify_roles,
              send_correspondence, set_case_outcome, record_followup_action, flag_cms_breach]


async def _run(signals, then=None):
    """Start a resumed run, let it settle, optionally signal it, return its handle."""
    calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await env.client.start_workflow(
                AhcaDisputeWorkflow.run,
                {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1",
                 "resume": {"signals": signals}},
                id=f"wf-{uuid.uuid4()}", task_queue=queue)
            # Let the workflow task run without skipping time past any clock.
            for _ in range(20):
                await asyncio.sleep(0.05)
                await h.describe()
            if then:
                await then(h)
                for _ in range(20):
                    await asyncio.sleep(0.05)
                    await h.describe()
            desc = await h.describe()
            return desc.status, list(calls)


@pytest.mark.asyncio
async def test_resume_at_stage1_does_nothing():
    status, got = await _run([{"name": "DOC_ANALYZED", "data": {}},
                              {"name": "ELIGIBILITY_RESULT", "data": {"result": "ELIGIBLE"}}])
    assert status == WorkflowExecutionStatus.RUNNING
    # No received_at, no statuses, no follow-ups: only the 60-day clock check.
    assert got == [("recommendation_days_left",)]


@pytest.mark.asyncio
async def test_resume_mid_case_skips_done_steps_and_continues():
    signals = [
        {"name": "PACKET_COMPLETE", "data": {}},
        {"name": "ELIGIBILITY_RESULT", "data": {"result": "ELIGIBLE"}},
        {"name": "LETTER_SENT", "data": {"template": "acceptance"}},
    ]

    async def send_plan_notice(h):
        await h.signal("LETTER_SENT", {"template": "plan_notification"})

    status, got = await _run(signals, then=send_plan_notice)
    assert status == WorkflowExecutionStatus.RUNNING
    # It was waiting for the plan notification: the old run had already asked
    # for it, and nothing from stages 1-3 is repeated either.
    assert ("record_followup_action", "send_plan_notification") not in got
    assert ("set_dual_status", "Initial Review Pending") not in got
    assert ("set_dual_status", "Provider Acceptance Letter Issued") not in got
    assert ("record_followup_action", "send_acceptance_letter") not in got
    # After the new signal it carries on as normal.
    assert ("set_dual_status", "Plan Notification Packet Issued") in got
    assert ("set_program_date", "plan_notified_at") in got


@pytest.mark.asyncio
async def test_resume_waiting_withdrawal_letter():
    status, got = await _run([{"name": "PROVIDER_WITHDRAW", "data": {}}])
    assert status == WorkflowExecutionStatus.RUNNING
    # The withdrawal follow-up was already recorded by the old run.
    assert ("record_followup_action", "send_withdrawal_letter") not in got


@pytest.mark.asyncio
async def test_new_case_unchanged():
    calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await env.client.start_workflow(
                AhcaDisputeWorkflow.run, {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1"},
                id=f"wf-{uuid.uuid4()}", task_queue=queue)
            await h.signal("PACKET_COMPLETE", {})
            for _ in range(20):
                await asyncio.sleep(0.05)
                await h.describe()
    assert calls[0] == ("set_program_date", "received_at")
    assert ("set_dual_status", "Initial Review Pending") in calls
