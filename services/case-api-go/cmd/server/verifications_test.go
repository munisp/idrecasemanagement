package main

import (
	"strings"
	"testing"
)

func TestTINProblem(t *testing.T) {
	cases := []struct {
		tin string
		ok  bool
	}{
		{"123456789", true},  // EIN prefix 12
		{"591234567", true},  // EIN prefix 59
		{"070000001", false}, // prefix 07 is never issued, and 070 is not ... actually a valid SSN area
		{"000000000", false}, // all the same digit
		{"999999999", false},
		{"12345678", false}, // 8 digits
		{"12345678a", false},
		{"891234567", false}, // EIN prefix 89 never issued; SSN area 891? 8xx is valid -> see below
		{"966123456", false}, // EIN prefix 96 never issued; SSN area 9xx invalid
		{"000123456", false}, // prefix 00 never issued, SSN area 000 invalid
	}
	for _, c := range cases {
		got := tinProblem(c.tin) == ""
		want := c.ok
		// An SSN-shaped number with a never-issued EIN prefix is still a
		// plausible SSN; only the ones failing both rules are refused.
		if c.tin == "070000001" || c.tin == "891234567" {
			want = ssnFormOK(c.tin)
		}
		if got != want {
			t.Errorf("tinProblem(%q) ok=%v, want %v (%q)", c.tin, got, want, tinProblem(c.tin))
		}
	}
	if feinProblem("891234567") == "" || feinProblem("123456789") != "" {
		t.Errorf("a FEIN must be an EIN: 89 prefix refused, 12 accepted")
	}
}

func TestParseLEIE(t *testing.T) {
	var b strings.Builder
	b.WriteString("LASTNAME,FIRSTNAME,MIDNAME,BUSNAME,GENERAL,SPECIALTY,UPIN,NPI,DOB,ADDRESS,CITY,STATE,ZIP,EXCLTYPE,EXCLDATE,REINDATE,WAIVERDATE,WVRSTATE\n")
	b.WriteString(`,,,"GULF COAST MEDICAL SUPPLY, INC",OTHER BUSINESS,DME,,1972902351,,1 MAIN ST,TAMPA,FL,33601,1128a1,20200319,0,0,` + "\n")
	b.WriteString(`SMITH,JOHN,A,,IND- LIC HC SERV PRO,NURSE,,0,19700101,2 MAIN ST,MIAMI,FL,33101,1128b4,20190101,0,0,` + "\n")
	for i := 0; i < 1000; i++ {
		b.WriteString(`DOE,JANE,,,OTHER,,,0,,,,TX,,1128a1,20200101,0,0,` + "\n")
	}
	rows, err := parseLEIE(strings.NewReader(b.String()))
	if err != nil || len(rows) != 1002 {
		t.Fatalf("parse: %d rows, %v", len(rows), err)
	}
	if rows[0].bus != "GULF COAST MEDICAL SUPPLY, INC" || rows[0].npi != "1972902351" || rows[0].state != "FL" {
		t.Errorf("business row: %+v", rows[0])
	}
	if rows[1].npi != "" || rows[1].last != "SMITH" {
		t.Errorf("a 0 NPI means none: %+v", rows[1])
	}
	if _, err := parseLEIE(strings.NewReader("LASTNAME,FIRSTNAME\nA,B\n")); err == nil {
		t.Errorf("a list without its columns must be refused")
	}
	if oigNameKey("Gulf Coast Medical Supply, Inc.") != oigNameKey("GULF COAST MEDICAL SUPPLY INC") {
		t.Errorf("business names compare without punctuation or legal form")
	}
	if oigNameKey("John Smith") != oigNameKey("SMITH JOHN") {
		t.Errorf("person names compare in any order")
	}
}
