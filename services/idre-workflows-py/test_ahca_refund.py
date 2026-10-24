"""The refund path of AhcaDisputeWorkflow, run in Temporal's time-skipping test server: the fee is paid but the
filing packet never completes; the Reviewer does outreach; 7 days later the Reviewer and the PM are told a refund
may be due -- nothing closes on its own. Only when a person chooses Close and refund (REFUND_APPROVED) is the case
closed "Closed-Refunded", the filer sent the refund letter and Finance told to refund the initial fee from
Financials -> Refunds due. A packet that completes, inside the window or after it, keeps the case open.
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
async def test_no_packet_seven_days_after_outreach_only_flags_a_potential_refund():
    rec.calls.clear(); notices.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await _start(env, queue)
            await _settle(h)
            await h.signal("OUTREACH_DONE", {})
            await _settle(h)
            await env.sleep(timedelta(days=30))  # well past the 7-day window: still nobody has decided
            await _settle(h)
            flagged = list(rec.calls)
            await h.signal("REFUND_APPROVED", {})
            await _settle(h)
    review = [n for n in notices if n[1] == "REFUND_REVIEW_DUE"]
    assert review and set(review[0][0]) == {"CASE_MANAGER", "PM"}      # the people who decide are told
    assert ("set_dual_status", "Closed-Refunded") not in flagged         # ... and the case did not close itself
    assert ("send_correspondence", "refund") not in flagged              # ... nor was the filer told of a refund
    got = list(rec.calls)
    assert ("set_dual_status", "Closed-Refunded") in got                 # a person chose Close and refund
    assert ("send_correspondence", "refund") in got
    finance = [n for n in notices if n[1] == "REFUND_DUE"]
    assert finance and "FINANCE" in finance[0][0] and "Refunds due" in finance[0][2]


@pytest.mark.asyncio
async def test_refund_can_be_approved_inside_the_window():
    rec.calls.clear(); notices.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await _start(env, queue)
            await _settle(h)
            await h.signal("OUTREACH_DONE", {})
            await _settle(h)
            await env.sleep(timedelta(days=2))
            await h.signal("REFUND_APPROVED", {})
            await _settle(h)
    assert ("set_dual_status", "Closed-Refunded") in rec.calls
    assert not [n for n in notices if n[1] == "REFUND_REVIEW_DUE"]


@pytest.mark.asyncio
async def test_packet_after_the_window_still_continues():
    rec.calls.clear(); notices.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await _start(env, queue)
            await _settle(h)
            await h.signal("OUTREACH_DONE", {})
            await _settle(h)
            await env.sleep(timedelta(days=10))
            await _settle(h)
            await h.signal("PACKET_COMPLETE", {})
            await _settle(h)
    got = list(rec.calls)
    assert ("set_dual_status", "Closed-Refunded") not in got
    assert ("set_program_date", "packet_complete_at") in got


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
