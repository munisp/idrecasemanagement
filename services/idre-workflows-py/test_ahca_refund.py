"""The refund path of AhcaDisputeWorkflow, run in Temporal's time-skipping test server: the fee is paid but the
filing packet never completes; the Reviewer does outreach; 7 days later the case is closed "Closed-Refunded", the
filer gets the refund letter and Finance is told to refund the initial fee from Financials -> Refunds due. A packet
that completes inside the window keeps the case open.
   python -m pytest test_ahca_refund.py"""
from __future__ import annotations

import asyncio
import uuid
from datetime import timedelta

import pytest
from temporalio import activity
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

import test_ahca_resume as rec
from ahca_workflow import AhcaDisputeWorkflow

notices: list[tuple] = []


@activity.defn(name="notify_roles")
async def notify_roles(tenant, case_id, roles, typ, body):
    notices.append((tuple(roles), typ, body))
    rec.calls.append(("notify_roles", typ))


ACTIVITIES = [a for a in rec.ACTIVITIES if a is not rec.notify_roles] + [notify_roles]


async def _settle(h):
    for _ in range(30):
        await asyncio.sleep(0.05)
        await h.describe()


async def _start(env, queue):
    return await env.client.start_workflow(
        AhcaDisputeWorkflow.run, {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1"},
        id=f"wf-{uuid.uuid4()}", task_queue=queue)


@pytest.mark.asyncio
async def test_no_packet_seven_days_after_outreach_closes_for_a_refund():
    rec.calls.clear(); notices.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await _start(env, queue)
            await _settle(h)
            await h.signal("OUTREACH_DONE", {})
            await _settle(h)
            await env.sleep(timedelta(days=8))  # past the 7-day refund window
            await _settle(h)
    got = list(rec.calls)
    assert ("set_program_date", "outreach_at") in got
    assert ("send_correspondence", "refund") in got                 # the filer is told
    assert ("set_dual_status", "Closed-Refunded") in got             # the case is closed for a refund
    finance = [n for n in notices if n[1] == "REFUND_DUE"]
    assert finance and "FINANCE" in finance[0][0]                    # Finance is asked to refund
    assert "Refunds due" in finance[0][2]                            # ... from Financials -> Refunds due


@pytest.mark.asyncio
async def test_packet_inside_the_window_keeps_the_case_open():
    rec.calls.clear(); notices.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await _start(env, queue)
            await _settle(h)
            await h.signal("OUTREACH_DONE", {})
            await _settle(h)
            await env.sleep(timedelta(days=3))
            await h.signal("PACKET_COMPLETE", {})
            await _settle(h)
            await env.sleep(timedelta(days=5))
            await _settle(h)
    got = list(rec.calls)
    assert ("set_dual_status", "Closed-Refunded") not in got
    assert not [n for n in notices if n[1] == "REFUND_DUE"]
    assert ("set_program_date", "packet_complete_at") in got
