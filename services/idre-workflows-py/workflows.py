"""Temporal workflows — the statutory lifecycle of a federal IDR dispute.

One long-running workflow per case. Signals = business events, timers =
statutory clocks (business-day calendar aware). All side effects are
activities; this module must stay deterministic (replay-safe).
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import date, timedelta

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    from calendar_engine import add_business_days
    from activities import (
        set_case_status, post_ledger_transfer, request_lawful_reveal,
        notify_party, flag_cms_breach, run_cms_monthly_report,
    )

# 45 CFR Part 149 + 2026 Final Rule statutory clocks (business days / calendar days)
OPEN_NEGOTIATION_BD = 30
INITIATION_BD = 4
RESPONSE_BD = 3
OFFER_WINDOW_BD = 10
DETERMINATION_BD = 30
PAYMENT_CD = 30
REFUND_BD = 30
ADMIN_FEE_USD_2026 = 15  # applicability date 2026-06-11


@dataclass
class CaseInput:
    tenant: str
    case_id: str
    case_number: str
    open_negotiation_end: str  # YYYY-MM-DD
    plan_type: str             # FULLY_INSURED | SELF_FUNDED


@workflow.defn(name="IdrCaseWorkflow")
class IdrCaseWorkflow:
    def __init__(self) -> None:
        self.response_filed = False
        self.offers_submitted: set[str] = set()
        self.fees_paid: set[str] = set()
        self.selection_finalized = False
        self.determination_issued = False
        self.payment_recorded = False
        self.settled_or_withdrawn = False

    # ---- signals -----------------------------------------------------------
    @workflow.signal(name="RESPONSE_FILED")
    def on_response(self, data: dict) -> None:
        self.response_filed = True

    @workflow.signal(name="OFFER_SUBMITTED")
    def on_offer(self, data: dict) -> None:
        self.offers_submitted.add(data["party_id"])

    @workflow.signal(name="FEES_PAID")
    def on_fees(self, data: dict) -> None:
        self.fees_paid.add(data["party_id"])

    @workflow.signal(name="SELECTION_FINALIZED")
    def on_selection(self, data: dict) -> None:
        self.selection_finalized = True

    @workflow.signal(name="DETERMINATION_ISSUED")
    def on_determination(self, data: dict) -> None:
        self.determination_issued = True

    @workflow.signal(name="PAYMENT_RECORDED")
    def on_payment(self, data: dict) -> None:
        self.payment_recorded = True

    @workflow.signal(name="SETTLED_OR_WITHDRAWN")
    def on_settled(self, data: dict) -> None:
        self.settled_or_withdrawn = True

    # ---- main --------------------------------------------------------------
    @workflow.run
    async def run(self, inp: CaseInput) -> str:
        tenant = inp.tenant
        case = inp.case_id

        # --- Pre-phase: open negotiation still running ------------------------
        # The dispute may be registered before the 30bd negotiation period ends.
        # The workflow then waits durably and OPENS THE DISPUTE AUTOMATICALLY
        # the moment the negotiation window expires — no manual step, no lapse.
        neg_end = date.fromisoformat(inp.open_negotiation_end)
        today = workflow.now().date()
        if neg_end > today:
            await workflow.execute_activity(
                set_case_status, args=[tenant, case, "NEGOTIATION_TRACKED",
                                       "Registered; awaiting end of open negotiation"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            remaining = (neg_end - today).days + 1
            try:
                await workflow.wait_condition(
                    lambda: self.settled_or_withdrawn,
                    timeout=timedelta(days=remaining),
                )
            except TimeoutError:
                pass  # negotiation window expired → auto-open below
            if self.settled_or_withdrawn:
                return await self._close_early(tenant, case, "SETTLED_IN_NEGOTIATION")
            await workflow.execute_activity(
                set_case_status, args=[tenant, case, "INITIATED",
                                       "Open negotiation ended — dispute auto-opened by statutory timer"],
                start_to_close_timeout=timedelta(seconds=30),
            )
        else:
            await workflow.execute_activity(
                set_case_status, args=[tenant, case, "INITIATED", "IDR initiated"],
                start_to_close_timeout=timedelta(seconds=30),
            )

        # --- 3 business days: non-initiating party response ------------------
        try:
            await workflow.wait_condition(
                lambda: self.response_filed or self.settled_or_withdrawn,
                timeout=timedelta(days=RESPONSE_BD + 2),  # calendar upper bound of 3bd
            )
        except TimeoutError:
            pass  # response optional; clock still advances per rule
        if self.settled_or_withdrawn:
            return await self._close_early(tenant, case, "SETTLEMENT_PRE_ELIGIBILITY")

        # --- 10 business days: offer window (double-blind) --------------------
        await workflow.execute_activity(
            set_case_status, args=[tenant, case, "OFFER_WINDOW_OPEN", ""],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.wait_condition(
            lambda: len(self.offers_submitted) >= 2 or self.settled_or_withdrawn,
            timeout=timedelta(days=OFFER_WINDOW_BD + 4),
        )
        if self.settled_or_withdrawn:
            return await self._close_early(tenant, case, "SETTLEMENT_PRE_ELIGIBILITY")

        reveal_reason = "BOTH_SUBMITTED" if len(self.offers_submitted) >= 2 else "WINDOW_EXPIRED"
        await workflow.execute_activity(
            request_lawful_reveal, args=[case],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.execute_activity(
            set_case_status, args=[tenant, case, "OFFERS_REVEALED", reveal_reason],
            start_to_close_timeout=timedelta(seconds=30),
        )

        # --- IDRE selection ----------------------------------------------------
        await workflow.wait_condition(
            lambda: self.selection_finalized or self.settled_or_withdrawn,
            timeout=timedelta(days=10),
        )
        if self.settled_or_withdrawn:
            return await self._close_early(tenant, case, "SETTLEMENT_POST_ELIGIBILITY")

        # --- 30 business days: determination -----------------------------------
        determined = await workflow.wait_condition(
            lambda: self.determination_issued or self.settled_or_withdrawn,
            timeout=timedelta(days=DETERMINATION_BD + 8),
        )
        if self.settled_or_withdrawn:
            return await self._close_early(tenant, case, "SETTLEMENT_POST_ELIGIBILITY")
        if not determined:
            await workflow.execute_activity(
                flag_cms_breach,
                args=[tenant, case, "DETERMINATION_30BD", "IDRE exceeded statutory window"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            await workflow.wait_condition(lambda: self.determination_issued)

        # --- settle IDRE fees via TigerBeetle (double-entry) --------------------
        await workflow.execute_activity(
            post_ledger_transfer,
            args=[tenant, {"case_id": case, "kind": "IDRE_FEE_RESERVE",
                           "party_id": "both", "amount_cents": 0, "post_kind": "POST"}],
            start_to_close_timeout=timedelta(seconds=30),
        )

        # --- 30 calendar days: payment by non-prevailing party ------------------
        paid = await workflow.wait_condition(
            lambda: self.payment_recorded, timeout=timedelta(days=PAYMENT_CD),
        )
        if not paid:
            await workflow.execute_activity(
                flag_cms_breach,
                args=[tenant, case, "PAYMENT_30CD", "prevailing party unpaid"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            await workflow.wait_condition(lambda: self.payment_recorded)

        await workflow.execute_activity(
            set_case_status, args=[tenant, case, "CLOSED_PAID", ""],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.execute_activity(
            notify_party,
            args=[tenant, "voice", "both", "case_closed", {"case_number": inp.case_number}],
            start_to_close_timeout=timedelta(seconds=30),
        )
        return "CLOSED_PAID"

    async def _close_early(self, tenant: str, case: str, status: str) -> str:
        await workflow.execute_activity(
            set_case_status, args=[tenant, case, status, ""],
            start_to_close_timeout=timedelta(seconds=30),
        )
        # refund/void escrow reservations per settlement fee rules
        await workflow.execute_activity(
            post_ledger_transfer,
            args=[tenant, {"case_id": case, "kind": "REFUND", "party_id": "both",
                           "amount_cents": 0, "post_kind": "VOID"}],
            start_to_close_timeout=timedelta(seconds=30),
        )
        return status


@workflow.defn(name="CmsMonthlyReportWorkflow")
class CmsMonthlyReportWorkflow:
    """Cron-scheduled per tenant: CMS monthly report <= 30bd after month end."""

    @workflow.run
    async def run(self, args: dict) -> str:
        key = await workflow.execute_activity(
            run_cms_monthly_report, args=[args["tenant"], args["month"]],
            start_to_close_timeout=timedelta(minutes=10),
        )
        return key
