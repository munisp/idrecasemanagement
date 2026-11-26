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
