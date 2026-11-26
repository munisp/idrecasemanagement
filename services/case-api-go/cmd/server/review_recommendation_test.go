package main

import "testing"

func TestRecommendedReportType(t *testing.T) {
	for _, c := range []struct{ sig, sc, want string }{
		{"CODING_REVIEW_COMPLETE", "coding", "CLAIM_DISPUTE"},
		{"CODING_REVIEW_COMPLETE", "med_nec_coding", "IRO"},
		{"CODING_REVIEW_COMPLETE", "med_nec", "IRO"},
		{"CLINICAL_REVIEW_COMPLETE", "", "IRO"},
	} {
		if got := recommendedReportType(c.sig, c.sc); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.sig, c.sc, got, c.want)
		}
	}
}

func TestFurtherReviewersInStep10(t *testing.T) {
	f := wfBase()
	f.Act["PLAN_RESPONSE_RECEIVED"] = wfAct{}
	f.Act["FULL_REVIEW_INITIATED"] = wfAct{}
	f.Reviewers = []wfReviewer{{ID: "u2", Name: "Ben", Role: "CODER"}, {ID: "u3", Name: "Cara", Role: "NURSE_PHYSICIAN", Done: true}}
	keys := map[string]int{}
	for i, st := range buildWorkflowSteps(f) {
		keys[st.Key] = i
	}
	if _, ok := keys["review:u2"]; !ok {
		t.Fatalf("Ben's coding review is not a step: %v", keys)
	}
	if keys["review:u2"] > keys["coding_review"] {
		t.Fatal("a further review comes before the main coding review")
	}
	if _, ok := keys["review:u3"]; !ok {
		t.Fatal("a named Nurse Reviewer brings the clinical reviews in")
	}
	if left := pendingReviewers(f.Reviewers, "CODER"); len(left) != 1 || left[0] != "Ben" {
		t.Fatalf("pending coders: %v", left)
	}
	if left := pendingReviewers(f.Reviewers, "NURSE_PHYSICIAN"); len(left) != 0 {
		t.Fatalf("Cara has reviewed: %v", left)
	}
}
