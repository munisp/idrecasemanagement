"""case_guard: a case-scoped activity whose case is gone ends its workflow instead of retrying forever; a case that
exists, or one missing only briefly, behaves as before. Run: python -m pytest test_case_guard.py
"""

import dataclasses
import uuid
from datetime import timedelta

import pytest
from temporalio import activity, workflow
from temporalio.client import WorkflowFailureError
from temporalio.exceptions import ActivityError, ApplicationError
from temporalio.testing import ActivityEnvironment, WorkflowEnvironment
from temporalio.worker import Worker

import case_guard


def _env(attempt: int) -> ActivityEnvironment:
    env = ActivityEnvironment()
    env.info = dataclasses.replace(env.info, attempt=attempt)
    return env


@pytest.fixture
def exists(monkeypatch):
    state = {"value": True, "calls": 0}

    async def fake(dsn, tenant, case_id):
        state["calls"] += 1
        return state["value"]

    monkeypatch.setattr(case_guard, "case_exists", fake)
    return state


@pytest.mark.asyncio
async def test_existing_case_passes(exists):
    await _env(1).run(case_guard.ensure_case, "dsn", "fl", "c1")
    assert exists["calls"] == 1


@pytest.mark.asyncio
async def test_missing_case_retries_before_the_third_attempt(exists):
    exists["value"] = False
    for attempt in (1, 2):
        with pytest.raises(ApplicationError) as err:
            await _env(attempt).run(case_guard.ensure_case, "dsn", "fl", "c1")
        assert err.value.type == "CaseMissing" and not err.value.non_retryable


@pytest.mark.asyncio
async def test_missing_case_is_final_on_the_third_attempt(exists):
    exists["value"] = False
    with pytest.raises(ApplicationError) as err:
        await _env(3).run(case_guard.ensure_case, "dsn", "fl", "c1")
    assert err.value.type == "CaseGone" and err.value.non_retryable


@pytest.mark.asyncio
async def test_unsafe_tenant_or_empty_case_is_not_checked(exists):
    exists["value"] = False
    await _env(5).run(case_guard.ensure_case, "dsn", "fl; drop", "c1")
    await _env(5).run(case_guard.ensure_case, "dsn", "fl", "")
    assert exists["calls"] == 0


@pytest.mark.asyncio
async def test_lookup_error_keeps_retrying(monkeypatch):
    async def broken(dsn, tenant, case_id):
        raise OSError("database unreachable")

    monkeypatch.setattr(case_guard, "case_exists", broken)
    with pytest.raises(OSError):
        await _env(9).run(case_guard.ensure_case, "dsn", "fl", "c1")


# End to end: a workflow whose case vanished closes as failed after three quick attempts.
ATTEMPTS = []


@activity.defn(name="guarded_step")
async def guarded_step(tenant: str, case_id: str) -> None:
    ATTEMPTS.append(activity.info().attempt)
    await case_guard.ensure_case("dsn", tenant, case_id)


@workflow.defn(sandboxed=False)
class GuardedWorkflow:
    @workflow.run
    async def run(self, tenant: str, case_id: str) -> None:
        await workflow.execute_activity(guarded_step, args=[tenant, case_id],
                                        start_to_close_timeout=timedelta(seconds=30))


@pytest.mark.asyncio
async def test_workflow_closes_when_its_case_is_gone(exists):
    exists["value"] = False
    ATTEMPTS.clear()
    async with await WorkflowEnvironment.start_time_skipping() as env:
        async with Worker(env.client, task_queue="tq", workflows=[GuardedWorkflow], activities=[guarded_step]):
            with pytest.raises(WorkflowFailureError) as err:
                await env.client.execute_workflow(GuardedWorkflow.run, args=["fl", "gone-case"],
                                                  id=f"wf-{uuid.uuid4()}", task_queue="tq")
    cause = err.value.cause
    assert isinstance(cause, ActivityError) and isinstance(cause.cause, ApplicationError)
    assert cause.cause.type == "CaseGone"
    assert ATTEMPTS == [1, 2, 3]
