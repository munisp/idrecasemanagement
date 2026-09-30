"""Temporal worker entrypoint: python worker.py"""

import asyncio
import os

from temporalio.client import Client
from temporalio.worker import Worker

from activities import (
    set_case_status, post_ledger_transfer, request_lawful_reveal,
    notify_party, flag_cms_breach, run_cms_monthly_report,
)
from onboarding_activities import (
    set_application_status, check_ein_npi, check_idre_certification,
    check_state_requirements, provision_keycloak_account,
    provision_idre_ledger_accounts, send_portal_invite, record_onboarding_audit,
)
from workflows import IdrCaseWorkflow, CmsMonthlyReportWorkflow
from onboarding import StakeholderOnboardingWorkflow, TenantOnboardingWorkflow


async def main() -> None:
    client = await Client.connect(
        os.environ.get("TEMPORAL_HOST", "localhost:7233"),
        namespace=os.environ.get("TEMPORAL_NAMESPACE", "idre"),
    )
    worker = Worker(
        client,
        task_queue="idre-cases",
        workflows=[IdrCaseWorkflow, CmsMonthlyReportWorkflow],
        activities=[
            set_case_status, post_ledger_transfer, request_lawful_reveal,
            notify_party, flag_cms_breach, run_cms_monthly_report,
        ],
    )
    onboarding_worker = Worker(
        client,
        task_queue="idre-onboarding",
        workflows=[StakeholderOnboardingWorkflow, TenantOnboardingWorkflow],
        activities=[
            set_application_status, check_ein_npi, check_idre_certification,
            check_state_requirements, provision_keycloak_account,
            provision_idre_ledger_accounts, send_portal_invite, record_onboarding_audit,
        ],
    )
    import asyncio
    await asyncio.gather(worker.run(), onboarding_worker.run())


if __name__ == "__main__":
    asyncio.run(main())
