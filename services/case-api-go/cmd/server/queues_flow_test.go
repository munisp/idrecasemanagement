package main

import "testing"

func TestReviewSignalsBelongToTheirQueues(t *testing.T) {
	for sig, want := range map[string]string{"CODING_REVIEW_COMPLETE": "CODING", "CLINICAL_REVIEW_COMPLETE": "CLINICAL",
		"ATTORNEY_REVIEW_COMPLETE": "LEGAL", "PACKET_COMPLETE": ""} {
		if got := reviewDisciplineOf(sig); got != want {
			t.Errorf("reviewDisciplineOf(%s) = %q, want %q", sig, got, want)
		}
	}
}

func TestACodingCompletionMustSayWhetherClinicalReviewIsNeeded(t *testing.T) {
	if _, msg := reviewCompletionData("CODING", "r1", "coder.fl", nil); msg == "" {
		t.Errorf("a coding completion without the clinical answer is refused")
	}
	yes := true
	data, msg := reviewCompletionData("CODING", "r1", "coder.fl", &yes)
	if msg != "" || data["clinical_requested"] != true || data["review_id"] != "r1" {
		t.Errorf("coding completion carries the answer: %v %q", data, msg)
	}
	data, msg = reviewCompletionData("LEGAL", "r2", "attorney.fl", nil)
	if msg != "" || len(data) != 2 {
		t.Errorf("legal completion needs no extra answer (the outcome is its own step): %v %q", data, msg)
	}
}
