// Author: Patrick Munis at newwave
package main

import (
	"testing"
	"time"
)

func TestProgramDue(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse("2006-01-02", s); return v }
	for _, c := range []struct {
		basis   string
		n       int
		typ     string
		want    string
		movedTo bool
	}{
		{"2026-11-10", 15, "calendar", "2026-11-25", false}, // a Wednesday: as it is
		{"2026-11-07", 15, "calendar", "2026-11-23", true},  // Sun 11-22 -> Mon
		{"2026-11-11", 15, "calendar", "2026-11-30", true},  // Thu 11-26 Thanksgiving -> Fri holiday -> weekend -> Mon 11-30
		{"2026-12-10", 15, "calendar", "2026-12-28", true},  // Fri 12-25 Christmas -> weekend -> Mon 12-28
		{"2026-11-20", 5, "business", "2026-12-01", false},  // skips Thanksgiving and the Friday after
		{"2027-06-30", 4, "calendar", "2027-07-06", true},   // Sun 7-4 observed Mon 7-5 -> Tue 7-6
	} {
		got, why := programDue(d(c.basis), c.n, c.typ)
		if got.Format("2006-01-02") != c.want || (why != "") != c.movedTo {
			t.Errorf("%s +%d %s: got %s (%q), want %s", c.basis, c.n, c.typ, got.Format("2006-01-02"), why, c.want)
		}
	}
	if programHoliday(d("2027-12-24")) != "Christmas Day" { // Christmas 2027 is a Saturday: observed Friday
		t.Error("Christmas 2027 should be observed on Friday 12-24")
	}
	if n := programBusinessDaysBetween(d("2026-11-25"), d("2026-11-30")); n != 1 { // Thu, Fri holidays; weekend; Mon
		t.Errorf("business days Wed 11-25 to Mon 11-30: got %d, want 1", n)
	}
}
