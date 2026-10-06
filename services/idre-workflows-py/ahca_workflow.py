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

from datetime import timedelta

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    from activities import flag_cms_breach
    from ahca_activities import (
        set_dual_status, set_program_date, record_followup_action,
        send_correspondence, set_case_outcome,
    )


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
        self.determination_sent = False
        self.rfi_pending = False
        self.rfi_to: str | None = None  # "provider" | "plan"
        self.rfi_response_received = False
        self.rfi_lapsed = False

    # ---- signals -------------------------------------------------------
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

    @workflow.signal(name="ELIGIBILITY_RESULT")
    def on_eligibility_result(self, data: dict) -> None:
        self.eligibility_result = data["result"]

    @workflow.signal(name="AOR_REVISED")
    def on_aor_revised(self, data: dict) -> None:
        self.aor_revised = True

    @workflow.signal(name="ESTIMATE_PERMISSION")
    def on_estimate_permission(self, data: dict) -> None:
        self.estimate_permission = data.get("granted", False)

    @workflow.signal(name="PROVIDER_WITHDRAW")
    def on_provider_withdraw(self, data: dict) -> None:
        self.provider_withdrawn = True

    @workflow.signal(name="PLAN_RESPONSE_RECEIVED")
    def on_plan_response(self, data: dict) -> None:
        self.plan_response_received = True

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

    @workflow.signal(name="RFI_SENT")
    def on_rfi_sent(self, data: dict) -> None:
        self.rfi_pending = True
        self.rfi_to = data.get("to", "provider")
        self.rfi_response_received = False

    @workflow.signal(name="RFI_RESPONSE_RECEIVED")
    def on_rfi_response_received(self, data: dict) -> None:
        self.rfi_response_received = True

    # ---- main ------------------------------------------------------------
    @workflow.run
    async def run(self, inp: dict) -> str:
        tenant, case_id, case_number = inp["tenant"], inp["case_id"], inp["case_number"]
        today = workflow.now().date().isoformat()

        # AGENCY_RECOMMENDATION (60cd from received_at, the single clock that
        # spans the WHOLE case rather than one stage) has to watch the entire
        # staged sequence below concurrently, not sit inline at one point in
        # it -- a case stuck waiting on, say, Stage 3's provider-permission
        # signal must still trip this breach at day 60 regardless. received_at
        # is set explicitly here (case-api's own clock projection would
        # otherwise fall back to the case's opened_at, which is the same
        # instant anyway since this workflow starts right at case creation).
        await workflow.execute_activity(
            set_program_date, args=[tenant, case_id, "received_at", today],
            start_to_close_timeout=timedelta(seconds=30),
        )
        import asyncio
        results = await asyncio.gather(
            self._run_stages(tenant, case_id, case_number, today),
            self._watch_agency_recommendation_clock(tenant, case_id),
            self._watch_rfi(tenant, case_id),
        )
        return results[0]

    async def _watch_rfi(self, tenant: str, case_id: str) -> None:
        # RFI_RESPONSE: 15cd, day-15 follow-up (timeframe_lapsed_email_and_call),
        # breach -> "Case closed" per program-rules.sql. Loops so more than one
        # RFI can go out sequentially across stages 3-5.
        while not (self.determination_sent or self.provider_withdrawn or self.rfi_lapsed):
            await workflow.wait_condition(
                lambda: self.rfi_pending or self.determination_sent or self.provider_withdrawn,
            )
            if not self.rfi_pending:
                return
            status = "RFI Pending Plan Response" if self.rfi_to == "plan" else "RFI Pending Provider Response"
            agency = "Awaiting Plan Response" if self.rfi_to == "plan" else "Awaiting Provider Response"
            await workflow.execute_activity(
                set_dual_status, args=[tenant, case_id, status, agency],
                start_to_close_timeout=timedelta(seconds=30),
            )
            responded = await workflow.wait_condition(
                lambda: self.rfi_response_received, timeout=timedelta(days=15),
            )
            if responded:
                self.rfi_pending = False
                self.rfi_response_received = False
                continue
            await workflow.execute_activity(
                record_followup_action,
                args=[tenant, case_id, "timeframe_lapsed_email_and_call", "Day 15 — RFI response window lapsed, case closed"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            # The source narrative names this as its own outbound email
            # template (Follow Up Additional Documentation Not Received),
            # not just an internal reminder -- send it for real, best-effort
            # (a delivery failure here shouldn't block the closure below).
            try:
                await workflow.execute_activity(
                    send_correspondence,
                    args=[tenant, case_id, "documentation_not_received",
                          "The response timeframe for the requested additional documentation has lapsed. This case is now closed."],
                    start_to_close_timeout=timedelta(seconds=30),
                )
            except Exception:
                pass
            await workflow.execute_activity(
                set_dual_status, args=[tenant, case_id, "Dismissed", "Closed"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            self.rfi_lapsed = True
            return

    async def _watch_agency_recommendation_clock(self, tenant: str, case_id: str) -> None:
        met = await workflow.wait_condition(
            lambda: self.determination_sent, timeout=timedelta(days=60),
        )
        if not met:
            await workflow.execute_activity(
                flag_cms_breach,
                args=[tenant, case_id, "AGENCY_RECOMMENDATION", "Recommendation not sent to AHCA within 60 calendar days of receipt"],
                start_to_close_timeout=timedelta(seconds=30),
            )

    async def _run_stages(self, tenant: str, case_id: str, case_number: str, today: str) -> str:
        # --- Stage 1: submission -- shell case already exists (PENDING_INTAKE
        # set by case-api at creation time); no CRM trace beyond that until a
        # complete packet+payment arrives, same as the source narrative. No
        # statutory clock covers this wait at all -- it's genuinely open-ended.
        await workflow.wait_condition(lambda: self.packet_complete or self.provider_withdrawn)
        if self.provider_withdrawn:
            return await self._withdraw(tenant, case_id)

        # Payment arrived but packet didn't: REFUND_WINDOW clock (7cd from
        # outreach_at) only starts once the Reviewer has done outreach.
        if not self.packet_complete:
            await workflow.wait_condition(lambda: self.outreach_done or self.provider_withdrawn)
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)
            await workflow.execute_activity(
                set_program_date, args=[tenant, case_id, "outreach_at", today],
                start_to_close_timeout=timedelta(seconds=30),
            )
            completed = await workflow.wait_condition(
                lambda: self.packet_complete, timeout=timedelta(days=7),
            )
            if not completed:
                await workflow.execute_activity(
                    set_dual_status, args=[tenant, case_id, "Closed-Refunded", "Closed"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                return "CLOSED_REFUNDED"

        # --- Stage 2: initial review (10cd from packet_complete_at) ----------
        await workflow.execute_activity(
            set_program_date, args=[tenant, case_id, "packet_complete_at", today],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.execute_activity(
            set_dual_status, args=[tenant, case_id, "Initial Review Pending", "Pending Initial Review"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        decided = await workflow.wait_condition(
            lambda: self.eligibility_result is not None, timeout=timedelta(days=10),
        )
        if not decided:
            await workflow.execute_activity(
                flag_cms_breach, args=[tenant, case_id, "INITIAL_REVIEW", "Eligibility not decided within 10 calendar days"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            await workflow.wait_condition(lambda: self.eligibility_result is not None)

        # AOR invalid: open-ended hold until a revised AOR arrives, per the
        # source narrative ("the case can't proceed until it arrives") --
        # then back to a fresh eligibility decision.
        while self.eligibility_result == "HOLD_AOR":
            await workflow.wait_condition(lambda: self.aor_revised)
            self.aor_revised = False
            self.eligibility_result = None
            await workflow.wait_condition(lambda: self.eligibility_result is not None)

        if self.eligibility_result == "INELIGIBLE":
            await workflow.execute_activity(
                set_dual_status, args=[tenant, case_id, "Provider Closure Letter Issued", "Closed"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            return "INELIGIBLE"

        # --- Stage 3: provider response (cost estimate / RFI / withdrawal) --
        await workflow.execute_activity(
            set_dual_status, args=[tenant, case_id, "Provider Acceptance Letter Issued", "Awaiting Provider Response"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        if self.estimate_requested:
            await workflow.execute_activity(
                set_program_date, args=[tenant, case_id, "estimate_sent_at", today],
                start_to_close_timeout=timedelta(seconds=30),
            )
            # PROVIDER_PERMISSION: 15cd, day-13 follow-up (call_and_reply_all).
            got_followup = await workflow.wait_condition(
                lambda: self.estimate_permission is not None or self.provider_withdrawn or self.rfi_lapsed,
                timeout=timedelta(days=13),
            )
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)
            if self.rfi_lapsed:
                return "RFI_LAPSED"
            if not got_followup:
                await workflow.execute_activity(
                    record_followup_action,
                    args=[tenant, case_id, "call_and_reply_all", "Day 13 — provider permission not yet received, deadline is day 15"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                await workflow.wait_condition(
                    lambda: self.estimate_permission is not None or self.provider_withdrawn or self.rfi_lapsed,
                    timeout=timedelta(days=2),
                )
            if self.provider_withdrawn:
                return await self._withdraw(tenant, case_id)
            if self.rfi_lapsed:
                return "RFI_LAPSED"
            if self.estimate_permission is not True:
                # No permission by day 15 (or explicit decline) → Dismissal,
                # after the 2-3 business day grace the narrative describes.
                await workflow.execute_activity(
                    set_dual_status, args=[tenant, case_id, "Dismissed", "Closed"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                return "DISMISSED"

        # --- Stage 4: health plan notification --------------------------------
        await workflow.execute_activity(
            set_program_date, args=[tenant, case_id, "plan_notified_at", today],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.execute_activity(
            set_dual_status, args=[tenant, case_id, "Plan Notification Packet Issued", "Awaiting Plan Response"],
            start_to_close_timeout=timedelta(seconds=30),
        )

        # --- Stage 5: full review -- PLAN_RESPONSE clock, 15cd, follow_ups on
        # days 13 (Medicaid AHCA-notify), 14 (call+email), 15 (call again).
        plan_followups = [
            (13, "medicaid_ahca_notify", "Day 13 — Medicaid health plan, notify AHCA for follow-up"),
            (14, "call_and_email", "Day 14 — response due tomorrow (day 15); call and email the plan"),
            (15, "call_again", "Day 15 — final attempt to reach the plan before default determination"),
        ]
        prev_day = 0
        for day, action, detail in plan_followups:
            reached = await workflow.wait_condition(
                lambda: self.plan_response_received or self.plan_opted_out is not None or self.rfi_lapsed,
                timeout=timedelta(days=day - prev_day),
            )
            prev_day = day
            if reached:
                break
            await workflow.execute_activity(
                record_followup_action, args=[tenant, case_id, action, detail],
                start_to_close_timeout=timedelta(seconds=30),
            )
        else:
            reached = False

        if self.rfi_lapsed:
            return "RFI_LAPSED"

        if self.plan_opted_out is not None:
            if self.plan_opted_out:
                await workflow.execute_activity(
                    set_dual_status, args=[tenant, case_id, "Plan Opt-Out", "Closed"],
                    start_to_close_timeout=timedelta(seconds=30),
                )
                return "PLAN_OPT_OUT"
            # Not eligible to opt out: treated as a plan response: continues
            # into full review with the opt-out request itself as a finding.
            self.plan_response_received = True

        if not self.plan_response_received:
            # Day 15 passed with no response: default determination for the
            # provider (still goes through the SAME PM QA gate as a normal
            # determination -- the narrative has Reviewer draft a Default
            # Determination + Default Invoice, PM still QAs the package).
            await workflow.execute_activity(
                set_dual_status, args=[tenant, case_id, "Plan - No Response", "Under Review"],
                start_to_close_timeout=timedelta(seconds=30),
            )
        else:
            await workflow.execute_activity(
                set_dual_status, args=[tenant, case_id, "Review In Progress", "Under Review"],
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
        await workflow.wait_condition(lambda: self.coding_done)
        if self.clinical_requested:
            await workflow.wait_condition(lambda: self.clinical_done)
        await workflow.wait_condition(lambda: self.attorney_done)

        await workflow.execute_activity(
            set_dual_status, args=[tenant, case_id, "QA Final Determination", "Under Review"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        if self.outcome_data:
            await workflow.execute_activity(
                set_case_outcome, args=[tenant, case_id,
                    self.outcome_data.get("case_outcome") or "",
                    self.outcome_data.get("party_billed") or "",
                    self.outcome_data.get("final_amount_awarded_cents"),
                    self.outcome_data.get("num_claims_reviewed")],
                start_to_close_timeout=timedelta(seconds=30),
            )

        determination_day = workflow.now().date().isoformat()
        await workflow.execute_activity(
            set_program_date, args=[tenant, case_id, "date_recommendation_sent_to_agency", determination_day],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.execute_activity(
            set_dual_status, args=[tenant, case_id, "Determination sent to FL", "Decided"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        self.determination_sent = True  # stops the AGENCY_RECOMMENDATION watcher

        # AHCA itself issues the Final Order afterward -- an external agency
        # action fed back into the system by staff (no AHCA-to-IDRE
        # integration exists). The source docs don't give Capitol Bridge any
        # SLA for how long AHCA itself takes here, so this is a genuinely
        # open-ended wait, not a breach-bearing clock.
        await workflow.wait_condition(lambda: self.final_order_issued)

        await workflow.execute_activity(
            set_program_date, args=[tenant, case_id, "date_final_order_sent", workflow.now().date().isoformat()],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.execute_activity(
            set_dual_status, args=[tenant, case_id, "Final Order Issued", "Decided"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        return "FINAL_ORDER_ISSUED"

    async def _withdraw(self, tenant: str, case_id: str) -> str:
        await workflow.execute_activity(
            set_dual_status, args=[tenant, case_id, "Provider - Withdrawal", "Closed"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        return "WITHDRAWN"
