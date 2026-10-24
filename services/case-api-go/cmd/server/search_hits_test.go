package main

import "testing"

func TestDocHitLabel(t *testing.T) {
	for _, c := range []struct{ file, num, typ, want string }{
		{"eob.pdf", "FL26-021372", "eob", "eob.pdf — FL26-021372"},
		{"", "FL26-021372", "medical_records", "medical records — FL26-021372"},
		{"", "", "", "document"},
		{"letter.docx", "", "", "letter.docx"},
	} {
		if got := docHitLabel(c.file, c.num, c.typ); got != c.want {
			t.Errorf("docHitLabel(%q,%q,%q) = %q, want %q", c.file, c.num, c.typ, got, c.want)
		}
	}
}
