"""Who hears the workflow's case alerts (2026-11-26, as case-api's role_notify.go)."""
from ahca_activities import alert_targets


def targets(helpers, roles):
    return [t for t, _ in alert_targets(helpers, roles)]


def test_always_the_tenant_admin_admin_and_pm():
    assert targets({}, []) == ["role:TENANT_ADMIN", "role:CASE_MANAGER", "role:PM"]


def test_specialists_only_when_named_on_the_case():
    assert targets({"CODER": "ada"}, ["ATTORNEY", "CODER"])[3:] == ["ada"]


def test_qa_falls_back_to_every_qa():
    assert "role:QA" in targets({}, ["QA"])
    assert targets({"QA": "q1"}, ["QA"])[3:] == ["q1"]


def test_finance_by_role_and_no_duplicates():
    t = targets({}, ["FINANCE", "CASE_MANAGER", "PM"])
    assert "role:FINANCE" in t and len(t) == len(set(t))
