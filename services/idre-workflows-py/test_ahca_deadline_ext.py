"""A PM deadline extension (DEADLINE_EXTENDED) moves the workflow's own timers, not just the projected date.

Runs the real workflow on Temporal's time-skipping test server with the fake, call-recording activities of
test_ahca_resume.py.
"""

from __future__ import annotations

import asyncio
import uuid
from datetime import timedelta

import pytest
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from ahca_workflow import AhcaDisputeWorkflow
from test_ahca_resume import ACTIVITIES, calls


async def _settle(h):
    for _ in range(20):
        await asyncio.sleep(0.05)
        await h.describe()


def _breaches(clock):
    return [c for c in calls if c == ("flag_cms_breach", clock)]


async def _initial_review(extend_days: int | None):
    """New case, packet complete, nobody decides eligibility (13 calendar days). Returns (breached by day 14,
    breached by day 19)."""
    calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await env.client.start_workflow(
                AhcaDisputeWorkflow.run, {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1"},
                id=f"wf-{uuid.uuid4()}", task_queue=queue)
            await _settle(h)
            await h.signal("PACKET_COMPLETE", {})
            await _settle(h)
            if extend_days:
                await h.signal("DEADLINE_EXTENDED", {"clock": "INITIAL_REVIEW", "days": extend_days, "by": "pm"})
                await _settle(h)
            await env.sleep(timedelta(days=14))
            await _settle(h)
            by_day_14 = bool(_breaches("INITIAL_REVIEW"))
            await env.sleep(timedelta(days=5))
            await _settle(h)
            by_day_19 = bool(_breaches("INITIAL_REVIEW"))
            return by_day_14, by_day_19


@pytest.mark.asyncio
async def test_no_extension_breaches_after_day_13():
    assert await _initial_review(None) == (True, True)


@pytest.mark.asyncio
async def test_extension_moves_the_breach():
    # 13 days + 5 extended: nothing on day 14, the breach by day 19.
    assert await _initial_review(5) == (False, True)


@pytest.mark.asyncio
async def test_extension_for_another_clock_changes_nothing():
    calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await env.client.start_workflow(
                AhcaDisputeWorkflow.run, {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1"},
                id=f"wf-{uuid.uuid4()}", task_queue=queue)
            await _settle(h)
            await h.signal("PACKET_COMPLETE", {})
            await h.signal("DEADLINE_EXTENDED", {"clock": "PLAN_RESPONSE", "days": 9})
            await _settle(h)
            await env.sleep(timedelta(days=14))
            await _settle(h)
            assert _breaches("INITIAL_REVIEW")


@pytest.mark.asyncio
async def test_resume_replays_an_extension():
    # A case moved to another namespace keeps its extension: the signal is in the resume list.
    calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=ACTIVITIES):
            h = await env.client.start_workflow(
                AhcaDisputeWorkflow.run,
                {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1",
                 "resume": {"signals": [{"name": "PACKET_COMPLETE", "data": {}},
                                        {"name": "DEADLINE_EXTENDED", "data": {"clock": "INITIAL_REVIEW", "days": 5}}]}},
                id=f"wf-{uuid.uuid4()}", task_queue=queue)
            await _settle(h)
            await env.sleep(timedelta(days=14))
            await _settle(h)
            assert not _breaches("INITIAL_REVIEW")
            await env.sleep(timedelta(days=5))
            await _settle(h)
            assert _breaches("INITIAL_REVIEW")
