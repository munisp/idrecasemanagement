package main

import (
	"strings"
	"testing"
	"time"
)

func TestDaysSince(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	ago := func(h int) *time.Time { x := now.Add(-time.Duration(h) * time.Hour); return &x }
	future := now.Add(48 * time.Hour)
	for _, c := range []struct {
		t    *time.Time
		want int
	}{{nil, 0}, {ago(5), 0}, {ago(24), 1}, {ago(24*9 + 3), 9}, {&future, 0}} {
		if got := daysSince(c.t, now); got != c.want {
			t.Errorf("daysSince(%v) = %d, want %d", c.t, got, c.want)
		}
	}
}

// The list is the workflow's refund closures whose initial fee is still paid (or being refunded) -- not open or already-refunded ones.
func TestRefundsDueSQLScope(t *testing.T) {
	for _, want := range []string{"internal_status = 'Closed-Refunded'", "i.kind='INITIAL_FEE'", "i.status IN ('PAID','REFUND_PENDING')",
		"a.type='REFUND_DUE'", "p.status='PAID'"} {
		if !strings.Contains(refundsDueSQL, want) {
			t.Errorf("refundsDueSQL is missing %q", want)
		}
	}
}
