# Author: Patrick Munis at newwave
"""InboundMailWorkflow — every received email, processed step by step and durably (task queue idre-mail).

case-api records a job per message and program and starts this workflow (mail_jobs.go). The steps are case-api's:
receive the message, then each attachment (fetched and malware-scanned before it is kept), then file it under its
dispute (and record a party's answer), then tell the recipients. A step that cannot finish now (a provider or the
scanner unavailable) is retried with backoff for up to three days; a step case-api refuses outright is not retried.
If a step gives up, the job is marked failed and the PMs are told."""

from __future__ import annotations

import asyncio
import os
from datetime import timedelta

from temporalio import activity, workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import ActivityError, ApplicationError

CASE_API = os.environ.get("CASE_API_URL", "http://localhost:8080")

STEP_RETRY = RetryPolicy(initial_interval=timedelta(seconds=10), backoff_coefficient=2.0,
                         maximum_interval=timedelta(minutes=15), non_retryable_error_types=["MailStepRefused"])


@activity.defn
async def mail_step(tenant: str, job_id: str, step: str, item: str = "", why: str = "") -> dict:
    """One step of a job, run by case-api. 503 = not now (retried); any other 4xx = refused (not retried)."""
    import httpx  # here, not at the top: the workflow sandbox loads this module and must not import network code
    path = f"/v1/tenants/{tenant}/internal/mail/jobs/{job_id}/{step}" + (f"/{item}" if item else "")
    async with httpx.AsyncClient(base_url=CASE_API, timeout=120) as client:
        resp = await client.post(path, json={"why": why} if why else {},
                                 headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN', '')}"})
    if resp.status_code == 503 or resp.status_code >= 500:
        raise RuntimeError(f"{step}: not now — {resp.text[:200]}")
    if resp.status_code >= 400:
        raise ApplicationError(f"{step}: {resp.text[:200]}", type="MailStepRefused", non_retryable=True)
    return resp.json()


@workflow.defn(name="InboundMailWorkflow")
class InboundMailWorkflow:
    @workflow.run
    async def run(self, job: dict) -> str:
        tenant, job_id = job["tenant"], job["job_id"]

        async def step(name: str, item: str = "") -> dict:
            return await workflow.execute_activity(
                mail_step, args=[tenant, job_id, name, item, ""], start_to_close_timeout=timedelta(minutes=3),
                schedule_to_close_timeout=timedelta(days=3), retry_policy=STEP_RETRY)

        try:
            received = await step("receive")
            # Each attachment on its own: one that the scanner rejects, or that its provider has lost, never holds
            # up the others.
            await asyncio.gather(*(step("attachment", a) for a in received.get("attachments", [])))
            await step("file")
            await step("deliver")
            return "DONE"
        except ActivityError as e:
            why = str(e.cause or e)[:300]
            await workflow.execute_activity(
                mail_step, args=[tenant, job_id, "failed", "", why], start_to_close_timeout=timedelta(minutes=1),
                retry_policy=RetryPolicy(maximum_attempts=10, initial_interval=timedelta(seconds=10)))
            return "FAILED"
