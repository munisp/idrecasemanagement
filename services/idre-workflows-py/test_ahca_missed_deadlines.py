"""What the workflow does when a party or the office does not keep to the SOP's timelines, and how the opt-out and the
withdrawal play out over time, in Temporal's time-skipping test server.
   python -m pytest test_ahca_missed_deadlines.py"""
from __future__ import annotations

import uuid
from datetime import timedelta

import pytest
from temporalio import activity
from temporalio.client import WorkflowExecutionStatus
from temporalio.service import RPCError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

import test_ahca_resume as rec
from ahca_workflow import AhcaDisputeWorkflow
from test_ahca_timelines import _accepted, _followups, _settle


def _statuses():
    return [c[1] for c in rec.calls if c[0] == "set_dual_status"]


def _notices():
    return [c[1] for c in rec.calls if c[0] == "notify_roles"]


async def _notified(env, queue):
    h = await _accepted(env, queue, estimate=False)
    await h.signal("LETTER_SENT", {"template": "plan_notification"})
    await _settle(h)
    return h


@activity.defn(name="recommendation_days_left")
async def no_days_left(tenant, case_id):
    rec.calls.append(("recommendation_days_left",))
    return 0


async def _run(body, days_left=None):
    rec.calls.clear()
    acts = [a for a in rec.ACTIVITIES if days_left is None or a is not rec.recommendation_days_left] + ([days_left] if days_left else [])
    env = await WorkflowEnvironment.start_time_skipping()
    async with env:
        queue = f"q-{uuid.uuid4()}"
        async with Worker(env.client, task_queue=queue, workflows=[AhcaDisputeWorkflow], activities=acts):
            await body(env, queue)


# ---- the opt-out -----------------------------------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_an_allowed_opt_out_closes_the_case_when_its_letter_is_sent():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PLAN_OPT_OUT", {"eligible": True})
        await _settle(h)
        assert "OPT_OUT_LETTER_DUE" in _notices()
        assert "Plan Opt-Out" not in _statuses()          # not closed until the letter goes
        await h.signal("LETTER_SENT", {"template": "plan_opt_out"})
        assert await h.result() == "PLAN_OPT_OUT"
    await _run(body)
    assert "Plan Opt-Out" in _statuses() and "Plan - No Response" not in _statuses()


@pytest.mark.asyncio
async def test_an_allowed_opt_out_whose_letter_is_never_sent_waits_and_never_defaults():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PLAN_OPT_OUT", {"eligible": True})
        await env.sleep(timedelta(days=30))
        await _settle(h)
        desc = await h.describe()
        assert desc.status == WorkflowExecutionStatus.RUNNING   # still waiting for the Opt-Out letter
    await _run(body)
    assert "Plan Opt-Out" not in _statuses()
    assert "Plan - No Response" not in _statuses()          # an opt-out is never turned into a default


@pytest.mark.asyncio
async def test_a_refused_opt_out_keeps_the_15_days_and_no_response_is_a_default():
    async def body(env, queue):
        h = await _notified(env, queue)
        await env.sleep(timedelta(days=5))
        await h.signal("PLAN_OPT_OUT", {"eligible": False})
        await _settle(h)
        assert "OPT_OUT_REFUSED" in _notices()
        await env.sleep(timedelta(days=12))                  # day 17: past the plan's 15 days
        await _settle(h)
    await _run(body)
    assert "Plan - No Response" in _statuses()


@pytest.mark.asyncio
async def test_a_refused_opt_out_then_a_response_in_time_goes_to_full_review():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PLAN_OPT_OUT", {"eligible": False})
        await env.sleep(timedelta(days=6))
        await h.signal("PLAN_RESPONSE_RECEIVED", {})
        await _settle(h)
        await env.sleep(timedelta(days=20))
        await _settle(h)
    await _run(body)
    assert "Review In Progress" in _statuses() and "Plan - No Response" not in _statuses()


# ---- the withdrawal --------------------------------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_a_withdrawal_never_confirmed_leaves_the_case_open():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PROVIDER_WITHDRAW", {"by": "provider"})
        await env.sleep(timedelta(days=30))
        await _settle(h)
        desc = await h.describe()
        assert desc.status == WorkflowExecutionStatus.RUNNING
    await _run(body)
    assert "confirm_withdrawal" in _followups()               # the office is told to confirm it
    assert "Provider - Withdrawal" not in _statuses()
    assert "Plan - No Response" not in _statuses()            # and the plan's lapse doesn't turn it into a default


# ---- requests for information ----------------------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_an_rfi_not_answered_in_15_days_dismisses_the_case():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PLAN_RESPONSE_RECEIVED", {})
        await h.signal("RFI_SENT", {"to": "provider"})
        await env.sleep(timedelta(days=16))
        await _settle(h)
    await _run(body)
    assert "timeframe_lapsed_email_and_call" in _followups()
    assert ("send_correspondence", "documentation_not_received") in rec.calls
    assert "Dismissed" in _statuses()


@pytest.mark.asyncio
async def test_an_rfi_answered_in_time_changes_nothing():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PLAN_RESPONSE_RECEIVED", {})
        await h.signal("RFI_SENT", {"to": "provider"})
        await env.sleep(timedelta(days=10))
        await h.signal("RFI_RESPONSE_RECEIVED", {})
        await env.sleep(timedelta(days=10))
        await _settle(h)
    await _run(body)
    assert "Dismissed" not in _statuses()


# ---- the 60 days to AHCA ---------------------------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_no_determination_within_60_days_flags_the_breach():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PLAN_RESPONSE_RECEIVED", {})
        await env.sleep(timedelta(days=62))                  # reviews never finished
        await _settle(h)
    await _run(body, days_left=no_days_left)
    assert ("flag_cms_breach", "AGENCY_RECOMMENDATION") in rec.calls


# ---- a closed case's workflow ends, and nothing reopens it -----------------------------------------------------------

@pytest.mark.asyncio
async def test_an_rfi_on_a_closed_opt_out_case_never_dismisses_it():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PLAN_OPT_OUT", {"eligible": True})
        await h.signal("LETTER_SENT", {"template": "plan_opt_out"})
        assert await h.result() == "PLAN_OPT_OUT"
        try:
            await h.signal("RFI_SENT", {"to": "provider"})  # the workflow has ended: Temporal refuses the signal
        except RPCError:
            pass
        await env.sleep(timedelta(days=20))
    await _run(body)
    assert "Plan Opt-Out" in _statuses() and "Dismissed" not in _statuses()


@pytest.mark.asyncio
async def test_a_dismissed_case_is_never_flagged_for_the_60_days():
    async def body(env, queue):
        h = await _accepted(env, queue, estimate=True)
        await h.signal("ESTIMATE_PERMISSION", {"granted": False})
        await h.signal("LETTER_SENT", {"template": "dismissal"})
        await env.sleep(timedelta(days=70))
        await _settle(h)
    await _run(body, days_left=no_days_left)
    assert ("flag_cms_breach", "AGENCY_RECOMMENDATION") not in rec.calls


@pytest.mark.asyncio
async def test_a_closed_cases_workflow_finishes():
    async def body(env, queue):
        h = await _notified(env, queue)
        await h.signal("PLAN_OPT_OUT", {"eligible": True})
        await h.signal("LETTER_SENT", {"template": "plan_opt_out"})
        assert await h.result() == "PLAN_OPT_OUT"
    await _run(body)
