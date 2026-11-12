package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestCleanDraftSummaryKeepsOnlyListFields(t *testing.T) {
	got := cleanDraftSummary(map[string]string{
		"client": "  TEST Provider 2 ", "claim": "L0001234", "amount": "$2,400.00",
		"patient_name": "Jane Doe", "dob": "1980-01-01", "provider": "", "other_party": strings.Repeat("x", 300),
	})
	if got["client"] != "TEST Provider 2" || got["claim"] != "L0001234" || got["amount"] != "$2,400.00" {
		t.Fatalf("list fields lost or untrimmed: %v", got)
	}
	if _, ok := got["patient_name"]; ok {
		t.Fatal("a patient's name must never reach a draft's list line")
	}
	if _, ok := got["dob"]; ok {
		t.Fatal("unknown keys must be dropped")
	}
	if _, ok := got["provider"]; ok {
		t.Fatal("empty values must be dropped")
	}
	if len(got["other_party"]) != 120 {
		t.Fatalf("long values must be capped at 120, got %d", len(got["other_party"]))
	}
}

func TestDraftRefShape(t *testing.T) {
	re := regexp.MustCompile(`^D-[0-9A-F]{6}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		ref := newDraftRef()
		if !re.MatchString(ref) {
			t.Fatalf("bad reference %q", ref)
		}
		seen[ref] = true
	}
	if len(seen) < 190 {
		t.Fatalf("references repeat too often: %d distinct of 200", len(seen))
	}
}
