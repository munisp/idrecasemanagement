package main

import (
	"strings"
	"testing"
)

func TestDrReportFileName(t *testing.T) {
	cases := map[string]string{
		"Smith review FINAL.pdf": "Review Rationale and Letter FL26-000001.pdf",
		"report.DOCX":            "Review Rationale and Letter FL26-000001.docx",
		"no extension":           "Review Rationale and Letter FL26-000001",
		"Review Rationale and Letter FL26-000001 v2.pdf": "Review Rationale and Letter FL26-000001 v2.pdf",
	}
	for in, want := range cases {
		if got := drReportFileName("FL26-000001", in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestLinkWhat(t *testing.T) {
	if !strings.Contains(linkWhat(caseZipKey, nil), "zip") {
		t.Error("case zip")
	}
	if got := linkWhat(caseSetPrefix+"final_order", []bundleDoc{{name: "Cover Letter X.pdf"}, {name: "Invoice X.pdf"}}); !strings.Contains(got, "Cover Letter X.pdf; Invoice X.pdf") {
		t.Errorf("set: %q", got)
	}
	if !strings.Contains(linkWhat(caseSetPrefix+"final_order", nil), "not filed yet") {
		t.Error("empty set")
	}
	if !strings.Contains(linkWhat("fl/x/y.pdf", nil), "one specific document") {
		t.Error("one doc")
	}
}

func TestShareTokenRe(t *testing.T) {
	tok := strings.Repeat("ab", 24)
	body := "Download: https://idre.newfire.app/s/" + tok + "\nAlso /api/share/" + tok + "/download"
	if m := shareTokenRe.FindAllStringSubmatch(body, -1); len(m) != 2 || m[0][1] != tok {
		t.Fatalf("tokens: %v", m)
	}
}

func TestLetterBundlesCarryTheRationale(t *testing.T) {
	for _, set := range []string{"final_order", "final_order_rationale_copy"} {
		found := false
		for _, p := range letterBundles[set].parts {
			found = found || p.tpl == "final_order_rationale"
		}
		if !found {
			t.Errorf("%s does not carry the Review Rationale and Letter", set)
		}
	}
}

func TestUnfilled(t *testing.T) {
	got := unfilled("Dear [Filing Party full name],", "the fee is $[insert amount]. [secure upload link — created when the email is sent] [Filing Party full name]")
	if len(got) != 2 || got[0] != "[Filing Party full name]" || got[1] != "[insert amount]" {
		t.Fatalf("unfilled: %v", got)
	}
	if len(unfilled("Case FL26-000001 is filed.")) != 0 {
		t.Fatal("no fill-ins")
	}
}
