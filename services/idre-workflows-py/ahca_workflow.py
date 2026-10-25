# Author: Patrick Munis at newwave
"""AhcaDisputeWorkflow — the Florida AHCA Claims Dispute Resolution
lifecycle. One long-running workflow per case, started the moment a
Filing Party's shell case is created (status PENDING_INTAKE), same timing
convention as StakeholderOnboardingWorkflow starting at application
submission rather than at a later "accepted" event.

Clock names, day counts, follow_ups[], and status strings below are taken
verbatim from the FL AHCA CDR program profile (scripts/program-rules.sql)
so this workflow and the read-time clock projection in case-api
(program.go's projectProgramClocks) never drift out of sync.

RFI (Request for Additional Information) can be sent at stages 3-5 per the
source workflow narrative ("3-5 Any") rather than at one fixed point, so
it runs as its own concurrent watcher (_watch_rfi) alongside the main
stage sequence and the 60-day AGENCY_RECOMMENDATION clock, rather than as
an inline step in the stage sequence -- the 15-day response clock and its
day-15 lapse->closed outcome apply regardless of which stage the main
sequence happens to be in when the RFI goes out.
"""

from __future__ import annotations

import asyncio
from datetime import date, timedelta

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    from activities import flag_cms_breach
    from ahca_activities import (
        set_dual_status, set_program_date, record_followup_action,
        send_correspondence, set_case_outcome, notify_roles, recommendation_days_left,
    )
    from calendar_engine import add_business_days


@workflow.defn(name="AhcaDisputeWorkflow")
class AhcaDisputeWorkflow:
    def __init__(self) -> None:
        self.packet_complete = False
        self.outreach_done = False
        self.eligibility_result: str | None = None  # ELIGIBLE | INELIGIBLE | HOLD_AOR
        self.aor_revised = False
        self.estimate_requested = False
        self.estimate_permission: bool | None = None  # True = proceed, False = declined
        self.provider_withdrawn = False
        self.withdrawal_confirmed = False  # Capitol Bridge verified the withdrawal request (WITHDRAWAL_CONFIRMED)
        self.case_over = False  # the stages have finished: the case closed or reached its Final Order
        # A person decided the fee is to be refunded (REFUND_APPROVED): the refund is never decided by the clock.
        self.refund_approved = False
        self.plan_response_received = False
        self.plan_opted_out: bool | None = None  # set on PLAN_OPT_OUT signal
        # Stage 5's Coder -> (optional Nurse/Physician) -> Attorney handoff,
        # modeled explicitly instead of one generic "review complete" flag
        # that collapsed all three roles' sign-off into a single gate.
        self.coding_done = False
        self.clinical_requested = False
        self.clinical_done = False
        self.attorney_done = False
        self.outcome_data: dict = {}
        self.final_order_issued = False
        # The day a step actually happened, as its dialog gave it (signal -> YYYY-MM-DD); see _day.
        self.step_days: dict[str, str] = {}
        self.determination_sent = False
        self.rfi_pending = False
        self.rfi_to: str | None = None  # "provider" | "plan"
        self.rfi_response_received = False
        self.rfi_lapsed = False
        # Templates of the letters that have really gone out (case-api
        # signals each send). The stage gates in _run_stages wait on these.
        self.letters_sent: set[str] = set()
        # PM deadline extensions (case-api deadline_extensions.go): clock → days granted, and how many of those
        # days a wait has already served (_deadline).
        self.deadline_ext: dict[str, int] = {}
        self._ext_used: dict[str, int] = {}
        self._rfi_round = 0  # numbers each RFI wait (see _wait's `site`)
        self.closed_early = False  # dismissed / refunded / opted out: stops the 60-day watcher
        # A resumed run (see run()) replays the case's recorded signals and
        # walks the stages again without repeating what already happened:
        # while this is True, activities are skipped, and the first stage wait
        # that is not already satisfied turns it off.
        self._catching_up = False

    async def _activity(self, fn, **kwargs):
        if self._catching_up:
            return None
        return await workflow.execute_activity(fn, **kwargs)

    def _caught_up_unless(self, cond) -> None:
        if self._catching_up and not cond():
            self._catching_up = False

    async def _until(self, cond) -> None:
        self._caught_up_unless(cond)
        await workflow.wait_condition(cond)

    async def _wait(self, cond, timeout: timedelta, site: str, stage: bool = True) -> bool:
        """Wait for cond, at most `timeout`. True: it happened. False: time ran out.

        wait_condition returns None when the condition is met and raises
        asyncio.TimeoutError when the time runs out. This workflow used to
        read that None as "timed out", so every deadline was backwards: a
        case decided on day 2 was flagged as a 10-day breach, a plan that
        answered got its day 13/14/15 chasers, an RFI that was answered
        dismissed the case -- and a deadline that really passed raised out of
        the workflow and stalled it.

        The patched() call sits AFTER the wait on purpose. A history that
        already recorded the old, wrong branch replays it (no marker ->
        False); a wait that finishes from now on returns the truth. `site`
        must be unique per wait, because patched() remembers its answer per
        id for the whole run. The timeout branch needs no marker: the old
        code never got past it, so no history holds anything after it.

        `stage` is False for the concurrent watchers: only the stage sequence
        decides where a resumed run has caught up to.
        """
        if stage:
            self._caught_up_unless(cond)
        try:
            await workflow.wait_condition(cond, timeout=timeout)
        except asyncio.TimeoutError:
            return False
        return workflow.patched(f"wait-result-{site}")

    async def _deadline(self, cond, days: int, site: str, clock: str, stage: bool = True) -> bool:
        """_wait for a program deadline the PM may extend. When the timer runs out, the days granted on `clock`
        (DEADLINE_EXTENDED) and not yet served are waited out before the caller's follow-up or breach. A history
        without an extension never reaches the patched() call, so it replays exactly as before."""
        met = await self._wait(cond, timedelta(days=days), site, stage)
        n = 0
        while not met:
            extra = self.deadline_ext.get(clock, 0) - self._ext_used.get(clock, 0)
            if extra <= 0 or not workflow.patched("deadline-extensions"):
                return False
            self._ext_used[clock] = self._ext_used.get(clock, 0) + extra
            n += 1
            met = await self._wait(cond, timedelta(days=extra), f"{site}-ext{n}", stage)
        return True

    # ---- signals -------------------------------------------------------
    @workflow.signal(name="DEADLINE_EXTENDED")
    def on_deadline_extended(self, data: dict) -> None:
        clock, days = str(data.get("clock") or ""), int(data.get("days") or 0)
        if clock and days > 0:
            self.deadline_ext[clock] = self.deadline_ext.get(clock, 0) + days

    @workflow.signal(name="PACKET_COMPLETE")
    def on_packet_complete(self, data: dict) -> None:
        self.packet_complete = True
        self.estimate_requested = data.get("estimate_requested", False)

    @workflow.signal(name="OUTREACH_DONE")
    def on_outreach_done(self, data: dict) -> None:
        # Fired by the Reviewer once they've confirmed payment arrived but
        # the packet didn't, and have done outreach -- this is what starts
        # the 7-day REFUND_WINDOW clock (basis=outreach_at), not case receipt.
        self.outreach_done = True
        self.step_days["OUTREACH_DONE"] = str(data.get("date") or "")

    @workflow.signal(name="ELIGIBILITY_RESULT")
    def on_eligibility_result(self, data: dict) -> None:
        self.eligibility_result = data["result"]

    @workflow.signal(name="AOR_REVISED")
    def on_aor_revised(self, data: dict) -> None:
        self.aor_revised = True

    @workflow.signal(name="ESTIMATE_PERMISSION")
    def on_estimate_permission(self, data: dict) -> None:
        self.estimate_permission = data.get("granted", False)
        self.step_days["ESTIMATE_PERMISSION"] = str(data.get("date") or "")

    @workflow.signal(name="ESTIMATE_CONFIRMED")
    def on_estimate_confirmed(self, data: dict) -> None:
        # "Confirm if Cost Estimate requested", the step after the case is deemed eligible: it decides, once the
        # acceptance letter is out, whether the cost estimate and the provider's permission come next.
        self.estimate_requested = bool(data.get("requested", False))

    @workflow.signal(name="WITHDRAWAL_CONFIRMED")
    def on_withdrawal_confirmed(self, data: dict) -> None:
        self.withdrawal_confirmed = True

    @workflow.signal(name="REFUND_APPROVED")
    def on_refund_approved(self, data: dict) -> None:
        self.refund_approved = True

    @workflow.signal(name="PROVIDER_WITHDRAW")
    def on_provider_withdraw(self, data: dict) -> None:
        self.provider_withdrawn = True

    @workflow.signal(name="PLAN_RESPONSE_RECEIVED")
    def on_plan_response(self, data: dict) -> None:
        self.plan_response_received = True
        self.step_days["PLAN_RESPONSE_RECEIVED"] = str(data.get("date") or "")

    @workflow.signal(name="PLAN_OPT_OUT")
    def on_plan_opt_out(self, data: dict) -> None:
        self.plan_opted_out = data.get("eligible", False)

    @workflow.signal(name="CODING_REVIEW_COMPLETE")
    def on_coding_review_complete(self, data: dict) -> None:
        self.coding_done = True
        self.clinical_requested = data.get("clinical_requested", False)

    @workflow.signal(name="CLINICAL_REVIEW_COMPLETE")
    def on_clinical_review_complete(self, data: dict) -> None:
        self.clinical_done = True

    @workflow.signal(name="ATTORNEY_REVIEW_COMPLETE")
    def on_attorney_review_complete(self, data: dict) -> None:
        self.attorney_done = True
        # Case Outcome / Party Billed / Final Amount Awarded / Number of
        # Claims Reviewed ride along on the same signal that closes out
        # review, rather than being a separate step easy to forget --
        # _run_stages reads these off self and fires set_case_outcome.
        self.outcome_data = {
            "case_outcome": data.get("case_outcome", ""),
            "party_billed": data.get("party_billed", ""),
            "final_amount_awarded_cents": data.get("final_amount_awarded_cents"),
            "num_claims_reviewed": data.get("num_claims_reviewed"),
        }

    @workflow.signal(name="FINAL_ORDER_ISSUED")
    def on_final_order_issued(self, data: dict) -> None:
        self.final_order_issued = True
        self.step_days["FINAL_ORDER_ISSUED"] = str(data.get("date") or "")

    @workflow.signal(name="LETTER_SENT")
    def on_letter_sent(self, data: dict) -> None:
        self.letters_sent.add(data.get("template", ""))

    @workflow.signal(name="RFI_SENT")
    def on_rfi_sent(self, data: dict) -> None:
        self.rfi_pending = True
        self.rfi_to = data.get("to", "provider")
        self.rfi_response_received = False

    @workflow.signal(name="RFI_RESPONSE_RECEIVED")
    def on_rfi_response_received(self, data: dict) -> None:
        self.rfi_response_received = True

    def _day(self, signal: str) -> str:
        """The day the step a signal recorded happened: its dialog's date when one was given (and is not in the
        future), otherwise today."""
        today = workflow.now().date()
        try:
            d = date.fromisoformat(self.step_days.get(signal, ""))
            return d.isoformat() if d <= today else today.isoformat()
        except ValueError:
            return today.isoformat()

    # ---- main ------------------------------------------------------------
    @workflow.run
    async def run(self, inp: dict) -> str:
        tenant, case_id, case_number = inp["tenant"], inp["case_id"], inp["case_number"]
        today = workflow.now().date().isoformat()

        # Resume: a case whose workflow has to be started again (moved to
        # another Temporal namespace, or lost) passes the signals its old run
        # received, oldest first. They are applied before anything runs, and
        # the stages are walked again with every activity skipped -- the
        # statuses, dates, follow-ups and emails already happened -- until
        # the first wait the case is really still in. Its clock starts then.
        resume = inp.get("resume")
        if resume:
            handlers = {
                "PACKET_COMPLETE": self.on_packet_complete,
                "OUTREACH_DONE": self.on_outreach_done,
                "ELIGIBILITY_RESULT": self.on_eligibility_result,
                "AOR_REVISED": self.on_aor_revised,
                "ESTIMATE_PERMISSION": self.on_estimate_permission,
                "PROVIDER_WITHDRAW": self.on_provider_withdraw,
                "REFUND_APPROVED": self.on_refund_approved,
                "WITHDRAWAL_CONFIRMED": self.on_withdrawal_confirmed, "ESTIMATE_CONFIRMED": self.on_estimate_confirmed,
                "PLAN_RESPONSE_RECEIVED": self.on_plan_response,
                "PLAN_OPT_OUT": self.on_plan_opt_out,
                "CODING_REVIEW_COMPLETE": self.on_coding_review_complete,
                "CLINICAL_REVIEW_COMPLETE": self.on_clinical_review_complete,
                "ATTORNEY_REVIEW_COMPLETE": self.on_attorney_review_complete,
                "FINAL_ORDER_ISSUED": self.on_final_order_issued,
                "LETTER_SENT": self.on_letter_sent,
                "RFI_SENT": self.on_rfi_sent,
                "RFI_RESPONSE_RECEIVED": self.on_rfi_response_received,
                "DEADLINE_EXTENDED": self.on_deadline_extended,
            }
            for sig in resume.get("signals") or []:
                handler = handlers.get(sig.get("name"))
                if handler:  # DOC_ANALYZED and the like change nothing here
                    handler(sig.get("data") or {})
            self._catching_up = True

        # AGENCY_RECOMMENDATION (60cd from received_at, the single clock that
        # spans the WHOLE case rather than one stage) has to watch the entire
        # staged sequence below concurrently, not sit inline at one point in
        # it -- a case stuck waiting on, say, Stage 3's provider-permission
        # signal must still trip this breach at day 60 regardless. received_at
        # is set explicitly here (case-api's own clock projection would
        # otherwise fall back to the case's opened_at, which is the same
        # instant anyway since this workflow starts right at case creation).
        await self._activity(
            set_program_date, args=[tenant, case_id, "received_at", today],
            start_to_close_timeout=timedelta(seconds=30),
        )
        import asyncio
        results = await asyncio.gather(
            self._stages(tenant, case_id, case_number, today),
            self._watch_agency_recommendation_clock(tenant, case_id, resumed=bool(resume)),
            self._watch_rfi(tenant, case_id),
        )
        return results[0]

    async def _stages(self, tenant: str, case_id: str, case_number: str, today: str) -> str:
        result = await self._run_stages(tenant, case_id, case_number, today)
        # A resumed case that was already finished ends here without having
        # waited; the watchers must not stay in catch-up for ever.
        self._catching_up = False
        # The case is over (closed, or the Final Order recorded): the watchers end with it. They used to outlive a case
        # that closed early -- ineligible, opted out, dismissed, refunded: its workflow never finished, an RFI sent on
        # it afterwards relabelled it Dismissed 15 days later, and a dismissed or refunded case was flagged for the
        # 60-day AHCA breach. Runs from before this keep their watchers.
        if workflow.patched("watchers-end-with-the-case"):
            self.case_over = True
        return result

    async def _watch_rfi(self, tenant: str, case_id: str) -> None:
        # RFI_RESPONSE: 15cd, day-15 follow-up (timeframe_lapsed_email_and_call),
        # breach -> "Case closed" per program-rules.sql. Loops so more than one
        # RFI can go out sequentially across stages 3-5.
        while not (self.determination_sent or self.provider_withdrawn or self.rfi_lapsed or self.case_over):
            await workflow.wait_condition(
                lambda: self.rfi_pending or self.determination_sent or self.provider_withdrawn or self.case_over,
            )
            if not self.rfi_pending or self.case_over:
                return
            # The "RFI Pending ..." Internal Statuses are marked for removal in
            # the program's status mapping: an RFI no longer moves the case's
            # status, it only runs its 15-day clock. (Histories that already
            # set the old status replay it.)
            if not workflow.patched("rfi-keeps-status"):
                status = "RFI Pending Plan Response" if self.rfi_to == "plan" else "RFI Pending Provider Response"
                agency = "Awaiting Plan Response" if self.rfi_to == "plan" else "Awaiting Provider Response"
                await self._activity(
                    set_dual_status, args=[tenant, case_id, status, agency],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            self._rfi_round += 1
            responded = await self._deadline(
                lambda: self.rfi_response_received or self.case_over, 15, f"rfi-{self._rfi_round}", "RFI_RESPONSE", stage=False)
            if self.case_over:
                return  # the case closed while the RFI was out: nothing to dismiss
            if responded:
                self.rfi_pending = False
                self.rfi_response_received = False
                continue
            await self._activity(
                record_followup_action,
                args=[tenant, case_id, "timeframe_lapsed_email_and_call", "Day 15 — RFI response window lapsed, case closed"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            # The source narrative names this as its own outbound email
            # template (Follow Up Additional Documentation Not Received),
            # not just an internal reminder -- send it for real, best-effort
            # (a delivery failure here shouldn't block the closure below).
            try:
                await self._activity(
                    send_correspondence,
                    args=[tenant, case_id, "documentation_not_received",
                          "The response timeframe for the requested additional documentation has lapsed. This case is now closed."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            except Exception:
                pass
            await self._activity(
                set_dual_status, args=[tenant, case_id, "Dismissed", "Closed"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            self.rfi_lapsed = True
            return

    async def _watch_agency_recommendation_clock(self, tenant: str, case_id: str, resumed: bool = False) -> None:
        if resumed:
            # The 60 days began with the case, not with this run: wait out
            # only what the case has left (0 or less: check right away).
            await workflow.wait_condition(lambda: not self._catching_up)
            left = await self._activity(
                recommendation_days_left, args=[tenant, case_id],
                start_to_close_timeout=timedelta(seconds=30),
            )
            done = lambda: (self.determination_sent or self.provider_withdrawn or self.closed_early
                            or self.rfi_lapsed or self.eligibility_result == "INELIGIBLE" or self.case_over)
            met = done()
            # recommendation_days_left already counts the extensions granted so far; only later ones remain.
            self._ext_used["AGENCY_RECOMMENDATION"] = self.deadline_ext.get("AGENCY_RECOMMENDATION", 0)
            if not met and (left or 0) > 0:
                met = await self._deadline(done, left, "agency-recommendation-resumed", "AGENCY_RECOMMENDATION", stage=False)
            if not met:
                await self._activity(
                    flag_cms_breach,
                    args=[tenant, case_id, "AGENCY_RECOMMENDATION", "Recommendation not sent to AHCA within 60 calendar days of receipt"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            return
        # This runs concurrently with _run_stages via asyncio.gather in run(),
        # which only completes once EVERY gathered coroutine finishes -- this
        # one previously watched determination_sent alone, so a case that
        # exited _run_stages early via withdrawal or an RFI lapse left the
        # workflow "Running" for up to the full 60 days anyway, AND would
        # fire a false "recommendation not sent" CMS breach on an
        # already-closed case once that timeout elapsed. Confirmed live:
        # signaling PROVIDER_WITHDRAW correctly closed the case in the DB,
        # but the workflow execution itself stayed Running.
        # provider_withdrawn/rfi_lapsed/INELIGIBLE are the early-return paths
        # with an existing dedicated flag to check; DISMISSED and
        # CLOSED_REFUNDED (Stage 1/3's other early returns) have no such
        # flag today and are a known remaining gap, not fixed here.
        met = await self._deadline(
            lambda: (self.determination_sent or self.provider_withdrawn or self.closed_early
                     or self.rfi_lapsed or self.eligibility_result == "INELIGIBLE" or self.case_over),
            60, "agency-recommendation", "AGENCY_RECOMMENDATION", stage=False)
        # The timer above started when the request to file came in, but the 60
        # days run from Date Received -- the day the fee was paid, which can
        # be later. Before raising the alert, ask how long the case really
        # has left and keep waiting for that long.
        if not met and workflow.patched("recommendation-clock-from-date-received"):
            for extra in range(1, 8):
                left = await self._activity(
                    recommendation_days_left, args=[tenant, case_id],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                if left <= 0:
                    break
                met = await self._wait(
                    lambda: (self.determination_sent or self.provider_withdrawn or self.closed_early
                             or self.rfi_lapsed or self.eligibility_result == "INELIGIBLE" or self.case_over),
                    timedelta(days=left), f"agency-recommendation-extra-{extra}", stage=False)
                if met:
                    break
        if not met:
            await self._activity(
                flag_cms_breach,
                args=[tenant, case_id, "AGENCY_RECOMMENDATION", "Recommendation not sent to AHCA within 60 calendar days of receipt"],
                start_to_close_timeout=timedelta(seconds=30),
            )

    async def _run_stages(self, tenant: str, case_id: str, case_number: str, today: str) -> str:
        # --- Stage 1: submission -- shell case already exists (PENDING_INTAKE
        # set by case-api at creation time); no CRM trace beyond that until a
        # complete packet+payment arrives, same as the source narrative. No
        # statutory clock covers this wait at all -- it's genuinely open-ended.
        # The refund path starts with outreach, which happens BEFORE the
        # packet is complete -- so this wait must also end on it. (It used to
        # wait for the packet alone, which made the refund path unreachable.)
        if workflow.patched("refund-path-reachable"):
            await self._until(lambda: self.packet_complete or self.outreach_done or self.provider_withdrawn)
        else:
            await self._until(lambda: self.packet_complete or self.provider_withdrawn)
        if self.provider_withdrawn:
            return await self._withdraw(tenant, case_id)

        # Payment arrived but packet didn't: REFUND_WINDOW clock (7cd from
        # outreach_at) only starts once the Reviewer has done outreach.
        if not self.packet_complete:
            await self._until(lambda: self.outreach_done or self.provider_withdrawn)
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)
            outreach_day = self._day("OUTREACH_DONE")
            await self._activity(
                set_program_date, args=[tenant, case_id, "outreach_at", outreach_day],
                start_to_close_timeout=timedelta(seconds=30),
            )
            manual = workflow.patched("manual-refund")
            # The 7 days run from the day of the outreach (recorded late, fewer are left; at least one).
            window = max(1, 7 - (workflow.now().date() - date.fromisoformat(outreach_day)).days)
            completed = await self._deadline(
                lambda: self.packet_complete or self.provider_withdrawn or (manual and self.refund_approved),
                window, "refund-window", "REFUND_WINDOW")
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)
            # A run that was already inside its refund window when refunds became manual recorded no marker above;
            # its window running out now is decided by a person too.
            if not manual and not completed and workflow.patched("manual-refund-late"):
                manual = True
            if manual and not self.refund_approved and not self.packet_complete:
                # 7 days after outreach with no complete packet the fee MAY be refunded -- a person decides. The
                # Reviewer and the PM are told; the case stays open until the packet arrives, the provider
                # withdraws, or someone chooses Close and refund (REFUND_APPROVED).
                await self._activity(
                    notify_roles,
                    args=[tenant, case_id, ["CASE_MANAGER", "PM"], "REFUND_REVIEW_DUE",
                          "Potential refund: no complete filing packet 7 days after outreach. If the fee should be "
                          "refunded, choose Close and refund on the case; otherwise keep waiting for the packet."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                await self._until(lambda: self.packet_complete or self.provider_withdrawn or self.refund_approved)
                if self.provider_withdrawn:
                    return await self._withdraw(tenant, case_id)
            if manual:
                completed = not self.refund_approved
            if not completed:
                # The refund is approved (or, on a run from before this was manual, the
                # window ran out): the case is closed and the fee refunded. The filer
                # is told, and Finance is asked to make the refund.
                if workflow.patched("refund-notice"):
                    try:
                        await self._activity(
                            send_correspondence,
                            args=[tenant, case_id, "refund", ""],
                            start_to_close_timeout=timedelta(seconds=30),
                        )
                    except Exception:
                        pass  # the closure and the refund request must not wait on mail
                    await self._activity(
                        notify_roles,
                        args=[tenant, case_id, ["FINANCE", "CASE_MANAGER"], "REFUND_DUE",
                              ("Refund approved — case closed. Finance: refund the initial fee "
                               "(Financials → Refunds due → Refund)." if self.refund_approved else
                               "No complete filing packet 7 days after outreach — case closed. Finance: refund the initial fee "
                               "(Financials → Refunds due → Refund).")],
                        start_to_close_timeout=timedelta(seconds=30),
                    )
                self.closed_early = True
                await self._activity(
                    set_dual_status, args=[tenant, case_id, "Closed-Refunded", "Closed"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                return "CLOSED_REFUNDED"

        # --- Stage 2: initial review (10cd from packet_complete_at) ----------
        await self._activity(
            set_program_date, args=[tenant, case_id, "packet_complete_at", workflow.now().date().isoformat()],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await self._activity(
            set_dual_status, args=[tenant, case_id, "Initial Review Pending", "Pending Initial Review"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        # Eligibility review: complete within 13 calendar days (the letter -- acceptance or ineligibility -- on or
        # before day 13). Runs from before this kept their 10-day target.
        review_days = 13 if workflow.patched("eligibility-13-days") else 10
        decided = await self._deadline(
            lambda: self.eligibility_result is not None, review_days, "initial-review", "INITIAL_REVIEW")
        if not decided:
            await self._activity(
                flag_cms_breach, args=[tenant, case_id, "INITIAL_REVIEW", f"Eligibility not decided within {review_days} calendar days"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            await self._until(lambda: self.eligibility_result is not None)

        # AOR invalid: open-ended hold until a revised AOR arrives, per the
        # source narrative ("the case can't proceed until it arrives") --
        # then back to a fresh eligibility decision.
        while self.eligibility_result == "HOLD_AOR":
            await self._until(lambda: self.aor_revised)
            self.aor_revised = False
            self.eligibility_result = None
            await self._until(lambda: self.eligibility_result is not None)

        # An ineligible dispute closes with its letter (Ineligible Dispute or
        # Dismissal, through QA), not at the moment of the finding. Until the
        # letter is out a corrected review or an override can still turn the
        # case around, so this also listens for a new result.
        if workflow.patched("gate-ineligible-letter-sent"):
            closing = {"ineligible", "dismissal"}
            while self.eligibility_result == "INELIGIBLE" and not (closing & self.letters_sent):
                await self._activity(
                    record_followup_action,
                    args=[tenant, case_id, "send_ineligible_letter",
                          "Dispute found ineligible — send the Ineligible Dispute letter through QA. The case closes when it has been sent."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                await self._until(
                    lambda: bool(closing & self.letters_sent) or self.eligibility_result != "INELIGIBLE" or self.provider_withdrawn)
                if self.provider_withdrawn:
                    return await self._withdraw(tenant, case_id)
                while self.eligibility_result == "HOLD_AOR":
                    await self._until(lambda: self.aor_revised)
                    self.aor_revised = False
                    self.eligibility_result = None
                    await self._until(lambda: self.eligibility_result is not None)

        if self.eligibility_result == "INELIGIBLE":
            await self._activity(
                set_dual_status, args=[tenant, case_id, "Provider Closure Letter Issued", "Closed"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            return "INELIGIBLE"

        # The dispute is eligible, but "Provider Acceptance Letter Issued" is a
        # fact about a letter: wait until the acceptance letter has passed QA
        # and gone out. (patched(): cases already past this point keep the
        # history they have.)
        if workflow.patched("gate-acceptance-letter-sent"):
            if "acceptance" not in self.letters_sent:
                await self._activity(
                    record_followup_action,
                    args=[tenant, case_id, "send_acceptance_letter",
                          "Dispute is eligible — send the Acceptance letter through QA. The case moves on when it has been sent."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            await self._until(lambda: "acceptance" in self.letters_sent or self.provider_withdrawn)
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)

        # --- Stage 3: provider response (cost estimate / RFI / withdrawal) --
        await self._activity(
            set_dual_status, args=[tenant, case_id, "Provider Acceptance Letter Issued", "Awaiting Provider Response"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        if self.estimate_requested:
            if workflow.patched("gate-estimate-email-sent"):
                # The provider's 15 days run from the estimate email, so the
                # clock waits for it (case-api records estimate_sent_at when
                # the email goes out).
                if "estimate_cost" not in self.letters_sent:
                    await self._activity(
                        notify_roles,
                        args=[tenant, case_id, ["CODER"], "ESTIMATE_DUE",
                              "The filer asked for a cost estimate — record it in the Dispute Summary and send the "
                              "Full Review Estimated Cost email (same day as the acceptance letter)."],
                        start_to_close_timeout=timedelta(seconds=30),
                    )
                await self._until(
                    lambda: "estimate_cost" in self.letters_sent or self.provider_withdrawn or self.rfi_lapsed)
                if self.provider_withdrawn:
                    return await self._withdraw(tenant, case_id)
                if self.rfi_lapsed:
                    return "RFI_LAPSED"
            else:
                await self._activity(
                    set_program_date, args=[tenant, case_id, "estimate_sent_at", today],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            # PROVIDER_PERMISSION: up to 15 calendar days; no permission by then is a dismissal -- no reminder,
            # no further grace. (Runs from before this kept the day-13 follow-up and the 3 business days.)
            exact15 = workflow.patched("permission-exact-15")
            got_followup = await self._deadline(
                lambda: self.estimate_permission is not None or self.provider_withdrawn or self.rfi_lapsed,
                15 if exact15 else 13, "permission-day15-exact" if exact15 else "permission-day13", "PROVIDER_PERMISSION")
            if exact15:
                got_followup = True  # nothing more to wait for: on to the permission or the dismissal
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)
            if self.rfi_lapsed:
                return "RFI_LAPSED"
            if not got_followup:
                await self._activity(
                    record_followup_action,
                    args=[tenant, case_id, "call_and_reply_all", "Day 13 — provider permission not yet received, deadline is day 15"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                await self._deadline(
                    lambda: self.estimate_permission is not None or self.provider_withdrawn or self.rfi_lapsed,
                    2, "permission-day15", "PROVIDER_PERMISSION")
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)
            if self.rfi_lapsed:
                return "RFI_LAPSED"
            if self.estimate_permission is not True and workflow.patched("gate-dismissal-letter-sent"):
                # No permission by day 15: a dismissal is a letter (Attorney QA), not a status click. A late "yes"
                # before it goes out still proceeds. (The older runs' further 3 business days stay theirs.)
                if self.estimate_permission is None and not exact15:
                    start = workflow.now().date()
                    grace_days = (add_business_days(start, 3, tenant) - start).days
                    await self._activity(
                        record_followup_action,
                        args=[tenant, case_id, "day15_no_permission",
                              "Day 15 — no permission to proceed received. Final 3 business days before the case is dismissed."],
                        start_to_close_timeout=timedelta(seconds=30),
                    )
                    await self._wait(
                        lambda: self.estimate_permission is not None or self.provider_withdrawn,
                        timedelta(days=grace_days), "dismissal-grace")
                    if self.provider_withdrawn:
                        return await self._withdraw(tenant, case_id)
                if self.estimate_permission is not True:
                    if "dismissal" not in self.letters_sent:
                        await self._activity(
                            record_followup_action,
                            args=[tenant, case_id, "send_dismissal_letter",
                                  "No permission to proceed — send the Dismissal letter (Attorney QA). The case is dismissed when it has been sent."],
                            start_to_close_timeout=timedelta(seconds=30),
                        )
                    await self._until(
                        lambda: "dismissal" in self.letters_sent or self.estimate_permission is True or self.provider_withdrawn)
                    if self.provider_withdrawn:
                        return await self._withdraw(tenant, case_id)
            if self.estimate_permission is not True:
                # No permission (or explicit decline) → Dismissal.
                self.closed_early = True
                await self._activity(
                    set_dual_status, args=[tenant, case_id, "Dismissed", "Closed"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                return "DISMISSED"

        if self.estimate_requested and self.estimate_permission is True and workflow.patched("date-provider-responded"):
            await self._activity(
                set_program_date, args=[tenant, case_id, "date_provider_responded", self._day("ESTIMATE_PERMISSION")],
                start_to_close_timeout=timedelta(seconds=30),
            )

        # --- Stage 4: health plan notification --------------------------------
        # The plan's 15-day clock starts when the notification email is sent,
        # so nothing below runs until it has passed QA and gone out.
        plan_notified_day = today
        if workflow.patched("gate-plan-notification-sent"):
            if "plan_notification" not in self.letters_sent:
                await self._activity(
                    record_followup_action,
                    args=[tenant, case_id, "send_plan_notification",
                          "Send the Health Plan Notification and FL Plan Packet through QA. The plan's 15-day clock starts when it has been sent."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            await self._until(
                lambda: "plan_notification" in self.letters_sent or self.provider_withdrawn or self.rfi_lapsed)
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)
            if self.rfi_lapsed:
                return "RFI_LAPSED"
            plan_notified_day = workflow.now().date().isoformat()
        await self._activity(
            set_program_date, args=[tenant, case_id, "plan_notified_at", plan_notified_day],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await self._activity(
            set_dual_status, args=[tenant, case_id, "Plan Notification Packet Issued", "Awaiting Plan Response"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        if workflow.patched("date-plan-response-due"):
            due = date.fromisoformat(plan_notified_day) + timedelta(days=15)
            await self._activity(
                set_program_date, args=[tenant, case_id, "date_health_plan_response_due", due.isoformat()],
                start_to_close_timeout=timedelta(seconds=30),
            )

        # --- Stage 5: full review -- PLAN_RESPONSE clock, 15cd, follow_ups on
        # days 13 (Medicaid AHCA-notify), 14 (call+email), 15 (call again).
        # Up to 15 calendar days, with one reminder to the plan of its due date on day 13; no response by day 15 is
        # a default determination. (Older runs keep their day 13/14/15 follow-ups.)
        if workflow.patched("plan-reminder-day13"):
            plan_followups = [
                (13, "plan_due_date_reminder", "Day 13 — remind the Health Plan its response is due on day 15"),
                (15, "plan_response_lapsed", "Day 15 — no response from the Health Plan: default determination"),
            ]
        else:
            plan_followups = [
                (13, "medicaid_ahca_notify", "Day 13 — Medicaid health plan, notify AHCA for follow-up"),
                (14, "call_and_email", "Day 14 — response due tomorrow (day 15); call and email the plan"),
                (15, "call_again", "Day 15 — final attempt to reach the plan before default determination"),
            ]
        # A withdrawal is allowed until the determination has gone to AHCA (SOP 5): from here on every wait -- the
        # plan's 15 days, the reviews, the Final Order package -- also ends on one. They didn't: a withdrawal after
        # the plan's notice sat until day 13, then ran on to the plan's chasers or a default. Runs from before this
        # keep their waits.
        wd = workflow.patched("withdraw-until-determination")
        withdrawn = lambda: wd and self.provider_withdrawn
        prev_day = 0
        for day, action, detail in plan_followups:
            reached = await self._deadline(
                lambda: self.plan_response_received or self.plan_opted_out is not None or self.rfi_lapsed or withdrawn(),
                day - prev_day, f"plan-day{day}", "PLAN_RESPONSE")
            prev_day = day
            if reached:
                break
            await self._activity(
                record_followup_action, args=[tenant, case_id, action, detail],
                start_to_close_timeout=timedelta(seconds=30),
            )
        else:
            reached = False

        if withdrawn():
            return await self._withdraw(tenant, case_id)
        if self.rfi_lapsed:
            return "RFI_LAPSED"

        if self.plan_opted_out is not None:
            if self.plan_opted_out:
                # The Attorney allowed the opt-out: the case closes with the
                # Plan Opt-out letter to the provider, through QA.
                if workflow.patched("gate-opt-out-letter-sent"):
                    if "plan_opt_out" not in self.letters_sent:
                        await self._activity(
                            notify_roles,
                            args=[tenant, case_id, ["CASE_MANAGER"], "OPT_OUT_LETTER_DUE",
                                  "The Attorney allowed the plan's opt-out — send the Plan Opt-out letter to the provider "
                                  "through QA (copy AHCA and the plan). The case closes when it has been sent."],
                            start_to_close_timeout=timedelta(seconds=30),
                        )
                    await self._until(lambda: "plan_opt_out" in self.letters_sent)
                self.closed_early = True
                await self._activity(
                    set_dual_status, args=[tenant, case_id, "Plan Opt-Out", "Closed"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                return "PLAN_OPT_OUT"
            if workflow.patched("opt-out-refused-keeps-clock"):
                # The Attorney refused the opt-out: the plan is told to respond
                # within its 15 days, which keep running. No response by then
                # is still a default determination.
                await self._activity(
                    notify_roles,
                    args=[tenant, case_id, ["CASE_MANAGER"], "OPT_OUT_REFUSED",
                          "The Attorney refused the plan's opt-out — email the plan (template plan_opt_out_refused) to "
                          "respond within its 15 days."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                elapsed = (workflow.now().date() - date.fromisoformat(plan_notified_day)).days
                await self._deadline(lambda: self.plan_response_received or self.rfi_lapsed or withdrawn(),
                                     max(1, 15 - elapsed), "plan-after-opt-out-refused", "PLAN_RESPONSE")
                if withdrawn():
                    return await self._withdraw(tenant, case_id)
                if self.rfi_lapsed:
                    return "RFI_LAPSED"
            else:
                # Not eligible to opt out: treated as a plan response.
                self.plan_response_received = True

        if not self.plan_response_received:
            # Day 15 passed with no response: default determination for the
            # provider (still goes through the SAME PM QA gate as a normal
            # determination -- the narrative has Reviewer draft a Default
            # Determination + Default Invoice, PM still QAs the package).
            await self._activity(
                set_dual_status, args=[tenant, case_id, "Plan - No Response", "Under Review"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            if workflow.patched("default-determination-notice"):
                await self._activity(
                    notify_roles,
                    args=[tenant, case_id, ["PM", "CASE_MANAGER"], "DEFAULT_DETERMINATION",
                          "The health plan did not respond by day 15 — default determination for the provider. "
                          "Reviewer: draft the Default Determination. PM: QA it and create the default invoice."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
        else:
            await self._activity(
                set_dual_status, args=[tenant, case_id, "Review In Progress", "Under Review"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            if workflow.patched("plan-responded-date-and-notice"):
                await self._activity(
                    set_program_date, args=[tenant, case_id, "date_plan_responded", self._day("PLAN_RESPONSE_RECEIVED")],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                await self._activity(
                    notify_roles,
                    args=[tenant, case_id, ["ATTORNEY", "CODER"], "PLAN_RESPONDED",
                          "The health plan has responded — the case is Under Review. Coder: decide whether the plan's documents need coding review."],
                    start_to_close_timeout=timedelta(seconds=30),
                )

        # --- Stage 5-6: Coder/Nurse/Attorney review -> PM QA -> determination.
        # No statutory sub-clock governs this internally; the 60-day
        # AGENCY_RECOMMENDATION clock watching in parallel (see run()) is
        # what actually bounds getting here, for both the default and the
        # normal-outcome path alike -- the seeded status vocabulary has no
        # separate "default determination sent" state, so is_default only
        # changed which internal_status preceded this point, not this one.
        # Coder -> (optional Nurse/Physician) -> Attorney, per the narrative's
        # own branching ("Coder decides whether the plan's documents need
        # coding review... if clinical review is needed, Nurse/Physician
        # reviews... Coder and Attorney (or Nurse/Physician and Attorney)
        # complete the Final Order package"). No new status strings are
        # introduced for the sub-steps -- "Review In Progress"/"Under
        # Review" already cover all of stage 5 in the seeded vocabulary,
        # and inventing an unseeded status would 400 on setDualStatus
        # (confirmed live earlier this session with "Closed-Refunded").
        # A default (SOP 7: no response from the plan by day 15) is decided without the Coder, Nurse or Attorney
        # reviews: the Reviewer generates the Default Determination and the PM QAs the Final Order package. A full
        # review (the plan responded) goes through them. Runs from before this keep waiting for the reviews.
        defaulted = not self.plan_response_received and workflow.patched("default-skips-full-review")
        if not defaulted:
            await self._until(lambda: self.coding_done or withdrawn())
            if self.clinical_requested and not withdrawn():
                await self._until(lambda: self.clinical_done or withdrawn())
            if not withdrawn():
                await self._until(lambda: self.attorney_done or withdrawn())
            if withdrawn():
                return await self._withdraw(tenant, case_id)

        await self._activity(
            # FL AHCA Status is "Decided" for the whole determination stage
            # (status mapping: QA Final Determination → Determination Sent to
            # FL → Final Order Issued).
            set_dual_status, args=[tenant, case_id, "QA Final Determination", "Decided"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        if self.outcome_data:
            await self._activity(
                set_case_outcome, args=[tenant, case_id,
                    self.outcome_data.get("case_outcome") or "",
                    self.outcome_data.get("party_billed") or "",
                    self.outcome_data.get("final_amount_awarded_cents"),
                    self.outcome_data.get("num_claims_reviewed")],
                start_to_close_timeout=timedelta(seconds=30),
            )

        # "Determination Sent to FL" is also a fact about a letter: the PM
        # QAs the Final Order package, and only when the email to AHCA has
        # gone out does the case take that status and its date.
        if workflow.patched("gate-final-order-sent"):
            if "final_order" not in self.letters_sent:
                await self._activity(
                    record_followup_action,
                    args=[tenant, case_id, "send_final_order_package",
                          "Review complete — PM to QA the Final Order package and create the invoice; then send it to AHCA. The case is marked Determination Sent to FL when it has been sent."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            await self._until(lambda: "final_order" in self.letters_sent or withdrawn())
            if withdrawn():
                return await self._withdraw(tenant, case_id)
        determination_day = workflow.now().date().isoformat()
        await self._activity(
            set_program_date, args=[tenant, case_id, "date_recommendation_sent_to_agency", determination_day],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await self._activity(
            set_dual_status, args=[tenant, case_id, "Determination Sent to FL", "Decided"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        self.determination_sent = True  # stops the AGENCY_RECOMMENDATION watcher

        # AHCA itself issues the Final Order afterward -- an external agency
        # action fed back into the system by staff (no AHCA-to-IDRE
        # integration exists). The source docs don't give Capitol Bridge any
        # SLA for how long AHCA itself takes here, so this is a genuinely
        # open-ended wait, not a breach-bearing clock.
        await self._until(lambda: self.final_order_issued)

        await self._activity(
            set_program_date, args=[tenant, case_id, "date_final_order_sent", self._day("FINAL_ORDER_ISSUED")],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await self._activity(
            set_dual_status, args=[tenant, case_id, "Final Order Issued", "Decided"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        return "FINAL_ORDER_ISSUED"

    async def _withdraw(self, tenant: str, case_id: str) -> str:
        # A withdrawal: the provider or the health plan asks; Capitol Bridge verifies and confirms the request; the
        # Withdrawal letter goes out (QA, AHCA and the plan copied); the case closes, with no invoice.
        if workflow.patched("withdrawal-confirm-step"):
            if not self.withdrawal_confirmed:
                await self._activity(
                    record_followup_action,
                    args=[tenant, case_id, "confirm_withdrawal",
                          "A withdrawal was requested — verify it with the party and confirm it on the case. The Withdrawal letter follows."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            await self._until(lambda: self.withdrawal_confirmed)
            if "withdrawal" not in self.letters_sent:
                await self._activity(
                    record_followup_action,
                    args=[tenant, case_id, "send_withdrawal_letter",
                          "Withdrawal confirmed — send the Withdrawal letter through QA, copying AHCA and the Health Plan. The case closes when it has been sent."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            await self._until(lambda: "withdrawal" in self.letters_sent)
        elif workflow.patched("gate-withdrawal-letter-sent"):
            if "withdrawal" not in self.letters_sent:
                await self._activity(
                    record_followup_action,
                    args=[tenant, case_id, "send_withdrawal_letter",
                          "Provider asked to withdraw — send the Withdrawal letter through QA within 1 business day, copying AHCA and the Health Plan. The case closes when it has been sent."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            await self._until(lambda: "withdrawal" in self.letters_sent)
        await self._activity(
            set_dual_status, args=[tenant, case_id, "Provider - Withdrawal", "Closed"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        return "WITHDRAWN"
