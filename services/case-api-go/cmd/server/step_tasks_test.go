package main

import "testing"

func TestStepTaskSettled(t *testing.T) {
	f := wfFacts{Dates: map[string]string{}, Act: map[string]wfAct{}, Letter: map[string]string{}}
	acc := "FL26-001283: Dispute is eligible — send the Acceptance letter through QA. The case moves on when it has been sent."
	if ok, _ := stepTaskSettled(acc, f, false); ok {
		t.Fatal("not sent yet: still open")
	}
	f.Letter["acceptance"] = "PENDING"
	if ok, _ := stepTaskSettled(acc, f, false); ok {
		t.Fatal("waiting in QA is not sent")
	}
	f.Letter["acceptance"] = "SENT"
	if ok, why := stepTaskSettled(acc, f, false); !ok || why != "the eligibility letter was sent" {
		t.Fatalf("sent: %v %q", ok, why)
	}
	if ok, _ := stepTaskSettled("FL26-2: Dispute found ineligible — send the Ineligible Dispute letter through QA.", f, false); !ok {
		t.Fatal("re-found eligible: the Ineligible letter's task is settled by the Acceptance letter")
	}
	esc := "Escalation: AGENCY_RECOMMENDATION breach on FL26-1003"
	if ok, _ := stepTaskSettled(esc, f, false); ok {
		t.Fatal("recommendation not sent yet")
	}
	f.Dates["date_recommendation_sent_to_agency"] = "2026-10-20"
	if ok, _ := stepTaskSettled(esc, f, false); !ok {
		t.Fatal("recommendation sent settles the breach")
	}
	if ok, why := stepTaskSettled("FL26-1: Send the Health Plan Notification and FL Plan Packet through QA.", f, true); !ok || why != "the case is closed" {
		t.Fatalf("a closed case settles the rest: %v %q", ok, why)
	}
	if ok, _ := stepTaskSettled("Call the clinic about the EOB", f, false); ok {
		t.Fatal("an unknown task stays open on an open case")
	}
}
