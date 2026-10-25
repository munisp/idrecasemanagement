"""The FL timelines exactly as the office's "Streamline Workflows, Timelines, and Outcomes" document gives them, in
Temporal's time-skipping test server: the provider's permission is waited for up to 15 calendar days and then the
case is dismissed (no reminder, no further grace); the health plan gets one reminder of its due date on day 13 and a
default determination after day 15; a withdrawal is confirmed by Capitol Bridge before its letter.
   python -m pytest test_ahca_timelines.py"""
from __future__ import annotations

import asyncio
import uuid
from datetime import timedelta

import pytest
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

import test_ahca_resume as rec
from ahca_workflow import AhcaDisputeWorkflow


async def _settle(h):
    for _ in range(30):
        await asyncio.sleep(0.05)
        await h.describe()


async def _accepted(env, queue, estimate: bool):
    """A case through eligibility and its acceptance letter."""
    h = await env.client.start_workflow(
        AhcaDisputeWorkflow.run, {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1"},
        id=f"wf-{uuid.uuid4()}", task_queue=queue)
    await _settle(h)
    await h.signal("PACKET_COMPLETE", {"estimate_requested": estimate})
    await h.signal("ELIGIBILITY_RESULT", {"result": "ELIGIBLE"})
    await h.signal("LETTER_SENT", {"template": "acceptance"})
    if estimate:
        await h.signal("LETTER_SENT", {"template": "estimate_cost"})
    await _settle(h)
    return h


def _followups():
    return [c[1] for c in rec.calls if c[0] == "record_followup_action"]


@pytest.mark.asyncio
async def test_no_permission_in_15_days_is_a_dismissal_with_no_reminder_or_grace():
    rec.calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await _accepted(env, queue, estimate=True)
            await env.sleep(timedelta(days=14))
            await _settle(h)
            assert "send_dismissal_letter" not in _followups()       # still inside the 15 days
            await env.sleep(timedelta(days=2))
            await _settle(h)
            f = _followups()
            assert "send_dismissal_letter" in f                       # day 15 passed: the Dismissal letter
            assert "call_and_reply_all" not in f                       # no day-13 reminder
            assert "day15_no_permission" not in f                      # no further 3 business days
            await h.signal("LETTER_SENT", {"template": "dismissal"})
            await _settle(h)
    assert ("set_dual_status", "Dismissed") in rec.calls


@pytest.mark.asyncio
async def test_plan_gets_one_reminder_on_day_13_then_default_after_day_15():
    rec.calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await _accepted(env, queue, estimate=False)
            await h.signal("LETTER_SENT", {"template": "plan_notification"})
            await _settle(h)
            await env.sleep(timedelta(days=12))
            await _settle(h)
            assert "plan_due_date_reminder" not in _followups()
            await env.sleep(timedelta(days=2))
            await _settle(h)
            assert _followups().count("plan_due_date_reminder") == 1   # day 13
            await env.sleep(timedelta(days=3))
            await _settle(h)
    f = _followups()
    assert "call_and_email" not in f and "call_again" not in f and "medicaid_ahca_notify" not in f
    assert ("set_dual_status", "Plan - No Response") in rec.calls     # default determination


@pytest.mark.asyncio
async def test_withdrawal_is_confirmed_before_its_letter():
    rec.calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await _accepted(env, queue, estimate=False)
            await h.signal("PROVIDER_WITHDRAW", {})
            await _settle(h)
            f = _followups()
            assert "confirm_withdrawal" in f and "send_withdrawal_letter" not in f
            await h.signal("WITHDRAWAL_CONFIRMED", {})
            await _settle(h)
            assert "send_withdrawal_letter" in _followups()
            await h.signal("LETTER_SENT", {"template": "withdrawal"})
            await _settle(h)
    assert ("set_dual_status", "Provider - Withdrawal") in rec.calls


@pytest.mark.asyncio
async def test_plan_response_date_is_the_day_given_not_today():
    rec.calls.clear(); rec.dates.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await _accepted(env, queue, estimate=False)
            await h.signal("LETTER_SENT", {"template": "plan_notification"})
            await _settle(h)
            await env.sleep(timedelta(days=5))
            await h.signal("PLAN_RESPONSE_RECEIVED", {"date": "2000-01-03"})   # received earlier, recorded late
            await _settle(h)
            assert rec.dates.get("date_plan_responded") == "2000-01-03"
            await h.signal("PLAN_RESPONSE_RECEIVED", {"date": "2999-01-01"})   # ignored: already moved on
            await _settle(h)
    assert rec.dates.get("date_plan_responded") == "2000-01-03"


@pytest.mark.asyncio
async def test_outreach_recorded_late_counts_its_7_days_from_the_day_given():
    rec.calls.clear(); rec.dates.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await env.client.start_workflow(
                AhcaDisputeWorkflow.run, {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1"},
                id=f"wf-{uuid.uuid4()}", task_queue=queue)
            await _settle(h)
            today = (await env.get_current_time()).date()
            await h.signal("OUTREACH_DONE", {"date": (today - timedelta(days=4)).isoformat()})
            await _settle(h)
            assert rec.dates.get("outreach_at") == (today - timedelta(days=4)).isoformat()
            await env.sleep(timedelta(days=2))
            await _settle(h)
            assert ("notify_roles", "REFUND_REVIEW_DUE") not in rec.calls     # day 6 from the outreach
            await env.sleep(timedelta(days=2))                                # day 8
            await _settle(h)
            assert ("notify_roles", "REFUND_REVIEW_DUE") in rec.calls         # after 3 days here, not 7


@pytest.mark.asyncio
async def test_confirming_the_cost_estimate_decides_the_path():
    """Confirm if Cost Estimate requested: the packet said no, the Reviewer confirms yes — the estimate comes next."""
    rec.calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await env.client.start_workflow(
                AhcaDisputeWorkflow.run, {"tenant": "fl", "case_id": "c1", "case_number": "FL26-1"},
                id=f"wf-{uuid.uuid4()}", task_queue=queue)
            await _settle(h)
            await h.signal("PACKET_COMPLETE", {})
            await h.signal("ELIGIBILITY_RESULT", {"result": "ELIGIBLE"})
            await h.signal("ESTIMATE_CONFIRMED", {"requested": True})
            await h.signal("LETTER_SENT", {"template": "acceptance"})
            await _settle(h)
    assert ("notify_roles", "ESTIMATE_DUE") in rec.calls


@pytest.mark.asyncio
async def test_default_goes_to_the_determination_without_the_reviews():
    """SOP 7: no response from the plan by day 15 is a default determination -- no Coder, Nurse or Attorney review;
    the Final Order package to AHCA is next."""
    rec.calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await _accepted(env, queue, estimate=False)
            await h.signal("LETTER_SENT", {"template": "plan_notification"})
            await _settle(h)
            await env.sleep(timedelta(days=16))
            await _settle(h)
            # No CODING/ATTORNEY_REVIEW_COMPLETE signals: the case still reaches the determination stage.
    assert ("set_dual_status", "Plan - No Response") in rec.calls
    assert ("set_dual_status", "QA Final Determination") in rec.calls
    assert "send_final_order_package" in _followups()


@pytest.mark.asyncio
async def test_a_plan_that_responds_still_gets_the_full_review():
    rec.calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await _accepted(env, queue, estimate=False)
            await h.signal("LETTER_SENT", {"template": "plan_notification"})
            await _settle(h)
            await h.signal("PLAN_RESPONSE_RECEIVED", {})
            await _settle(h)
            await env.sleep(timedelta(days=20))
            await _settle(h)
            assert ("set_dual_status", "QA Final Determination") not in rec.calls  # waits for the reviews
            await h.signal("CODING_REVIEW_COMPLETE", {})
            await h.signal("ATTORNEY_REVIEW_COMPLETE", {})
            await _settle(h)
    assert ("set_dual_status", "QA Final Determination") in rec.calls


@pytest.mark.asyncio
async def test_a_withdrawal_in_the_plans_window_closes_the_case():
    # SOP 5: the plan has been notified; the provider asks to withdraw. Capitol Bridge confirms, the Withdrawal letter
    # goes, and the case closes then -- not on day 13, and never as a default.
    rec.calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await _accepted(env, queue, estimate=False)
            await h.signal("LETTER_SENT", {"template": "plan_notification"})
            await _settle(h)
            await h.signal("PROVIDER_WITHDRAW", {"by": "provider"})
            await h.signal("WITHDRAWAL_CONFIRMED", {})
            await h.signal("LETTER_SENT", {"template": "withdrawal"})
            assert await h.result() == "WITHDRAWN"
    assert ("set_dual_status", "Provider - Withdrawal") in rec.calls
    assert ("set_dual_status", "Plan - No Response") not in rec.calls


@pytest.mark.asyncio
async def test_a_withdrawal_during_the_reviews_closes_the_case():
    rec.calls.clear()
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=rec.ACTIVITIES):
            h = await _accepted(env, queue, estimate=False)
            await h.signal("LETTER_SENT", {"template": "plan_notification"})
            await _settle(h)
            await h.signal("PLAN_RESPONSE_RECEIVED", {})
            await h.signal("CODING_REVIEW_COMPLETE", {})
            await _settle(h)
            await h.signal("PROVIDER_WITHDRAW", {"by": "plan"})
            await h.signal("WITHDRAWAL_CONFIRMED", {})
            await h.signal("LETTER_SENT", {"template": "withdrawal"})
            assert await h.result() == "WITHDRAWN"
    assert ("set_dual_status", "QA Final Determination") not in rec.calls
