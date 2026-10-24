// Author: Patrick Munis at newwave
package main

import "testing"

// A case found ineligible can still ask for the documents a re-check needs;
// one awaiting its fee cannot (it is not open yet).
func TestDocumentRequestsOnFrozenCases(t *testing.T) {
	if !caseActionAllowed(lockIneligible, "document-requests") {
		t.Error("an ineligible case must allow a document request for its re-check")
	}
	if caseActionAllowed(lockAwaitingPayment, "document-requests") {
		t.Error("a case awaiting its fee must not take document requests")
	}
	if !caseActionAllowed(lockIneligible, "document-requests/123/close") {
		t.Error("closing a request on an ineligible case is allowed")
	}
}

func TestDocumentRequestReasons(t *testing.T) {
	for _, k := range []string{"VERIFICATION", "ELIGIBILITY_RECHECK", "MISSING_DOCUMENT", "CLEARER_COPY", "RFI", "OTHER"} {
		if docRequestReasons[k] == "" {
			t.Errorf("reason %s has no words", k)
		}
	}
}
