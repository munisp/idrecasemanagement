package main

import (
	"strings"
	"testing"
)

func TestScreeningFilter(t *testing.T) {
	where, args := screeningFilter("fl", "", "", "")
	if where != "tenant=$1" || len(args) != 1 {
		t.Fatalf("no filters: got %q %v", where, args)
	}
	where, args = screeningFilter("fl", "50%_off", "match", "tpa")
	for _, want := range []string{"query_name ILIKE $2", "AND matched", "subject_type=$3"} {
		if !strings.Contains(where, want) {
			t.Errorf("missing %q in %q", want, where)
		}
	}
	if args[1] != `%50\%\_off%` || args[2] != "TPA" {
		t.Errorf("a typed percent sign or underscore is matched literally and the type upper-cased; got %v", args)
	}
	if w, _ := screeningFilter("fl", "", "clear", ""); !strings.Contains(w, "NOT matched") {
		t.Errorf("clear filter: %q", w)
	}
}
