package main

import "testing"

func TestNormalizeNAIC(t *testing.T) {
	for in, want := range map[string]string{"12345": "12345", " NAIC 12-345 ": "12345", "6319": "06319", "": "",
		"123456": "", "abc": "", "00000": "", "Sunshine Health": ""} {
		if got := normalizeNAIC(in); got != want {
			t.Errorf("normalizeNAIC(%q) = %q, want %q", in, got, want)
		}
	}
}
