// Author: Patrick Munis at newwave
package main

import (
	"encoding/base64"
	"testing"
)

func TestRebuttalProblems(t *testing.T) {
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nsig"))
	sig := map[string]any{"printed_name": "Pat", "title": "Claims Director", "signature": "Pat Plan", "date": "2026-11-25", "drawn": png}
	full := map[string]any{
		"party": map[string]any{"legal_name": "Acme Health", "contact_person": "Pat", "email": "pat@acme.test"},
		"smmc":  false, "participate": "yes", "challenge_eligibility": "no",
		"attest_signature": sig, "position": map[string]any{"basis": "Paid per contract."},
		"certification": map[string]any{"agreed": true, "printed_name": "Pat", "title": "Claims Director", "signature": "Pat Plan", "date": "2026-11-25", "drawn": png},
		"documents":     []any{"claim_forms", "contract"},
	}
	if p := rebuttalProblems("HEALTH_PLAN", full); len(p) != 0 {
		t.Fatalf("a complete form refused: %v", p)
	}
	full["challenge_eligibility"] = "yes"
	if p := rebuttalProblems("HEALTH_PLAN", full); len(p) != 2 { // an exclusion, and the attestation's certification
		t.Fatalf("a challenge with no exclusion: want 2 problems, got %v", p)
	}
	full["exclusions"], full["exclusions_certified"] = []any{"over_12_months"}, true
	if p := rebuttalProblems("HEALTH_PLAN", full); len(p) != 0 {
		t.Fatalf("a complete challenge refused: %v", p)
	}
	// Not taking part: the signature is still required, the position and the certification are not.
	no := map[string]any{"party": full["party"], "smmc": false, "participate": "no", "challenge_eligibility": "no", "attest_signature": sig}
	if p := rebuttalProblems("HEALTH_PLAN", no); len(p) != 0 {
		t.Fatalf("declining to take part refused: %v", p)
	}
	delete(no, "attest_signature")
	if p := rebuttalProblems("HEALTH_PLAN", no); len(p) != 2 { // the typed details and the drawn signature
		t.Fatalf("no signature: want 2 problems, got %v", p)
	}
	// An SMMC plan must take part: it is not asked.
	smmc := map[string]any{"party": full["party"], "smmc": true, "attest_signature": sig, "position": full["position"], "certification": full["certification"]}
	if p := rebuttalProblems("HEALTH_PLAN", smmc); len(p) != 0 {
		t.Fatalf("an SMMC plan's form refused: %v", p)
	}
}
