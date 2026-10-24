package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testLoc, _ = time.LoadLocation("America/New_York")

func at(v string) time.Time { t, _ := time.ParseInLocation("2006-01-02T15:04", v, testLoc); return t }

func TestCheckFees(t *testing.T) {
	ok := `{"initial_fee_schedule":[{"effective_from":"2026-01-01","cents":12359},{"effective_from":"2027-01-01","cents":12900}]}`
	if msg := checkFees(json.RawMessage(ok), at("2026-10-22T10:00"), testLoc); msg != "" {
		t.Fatalf("valid schedule refused: %s", msg)
	}
	for name, body := range map[string]string{
		"empty":         `{"initial_fee_schedule":[]}`,
		"bad date":      `{"initial_fee_schedule":[{"effective_from":"2026-13-01","cents":100}]}`,
		"same day":      `{"initial_fee_schedule":[{"effective_from":"2026-01-01","cents":100},{"effective_from":"2026-01-01","cents":200}]}`,
		"zero fee":      `{"initial_fee_schedule":[{"effective_from":"2026-01-01","cents":0}]}`,
		"none today":    `{"initial_fee_schedule":[{"effective_from":"2027-01-01","cents":12900}]}`,
		"refund window": `{"refund_window_days":-1}`,
	} {
		if checkFees(json.RawMessage(body), at("2026-10-22T10:00"), testLoc) == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	got := mergedFees(map[string]any{"initial_fee_cents": 12359.0}, json.RawMessage(ok), at("2027-02-01T00:00"), testLoc)
	if got["initial_fee_cents"] != int64(12900) {
		t.Errorf("stored fee should follow the schedule: %v", got["initial_fee_cents"])
	}
}

func TestApplyClocksAndBilling(t *testing.T) {
	stored := []any{map[string]any{"name": "PLAN_RESPONSE", "days": 15.0, "max_days": 0.0}}
	cl, msg := applyClocks(stored, json.RawMessage(`[{"name":"PLAN_RESPONSE","days":20}]`))
	if msg != "" || cl[0].(map[string]any)["days"] != 20 {
		t.Fatalf("clock not changed: %s %v", msg, cl)
	}
	if _, msg := applyClocks(stored, json.RawMessage(`[{"name":"NOPE","days":5}]`)); msg == "" {
		t.Error("an unknown clock was accepted")
	}
	if _, msg := applyClocks(stored, json.RawMessage(`[{"name":"PLAN_RESPONSE","days":20,"max_days":10}]`)); msg == "" {
		t.Error("a hard limit shorter than the deadline was accepted")
	}
	b, msg := applyBilling(map[string]any{}, json.RawMessage(`{"due_days":45,"tax_pct":0,"sneaky":"x"}`))
	if msg != "" || b["due_days"] != 45.0 || b["sneaky"] != nil {
		t.Errorf("billing merge: %s %v", msg, b)
	}
}

// A fee given a time takes effect at that moment on the program's clock, not at the start of its day.
func TestFeeAtTime(t *testing.T) {
	sch := []FeeStep{{EffectiveFrom: "2026-01-01", Cents: 12359}, {EffectiveFrom: "2027-01-01T09:00", Cents: 12900}}
	for now, want := range map[string]int64{"2026-12-31T23:59": 12359, "2027-01-01T08:59": 12359, "2027-01-01T09:00": 12900} {
		if got, _ := feeAt(sch, at(now), testLoc); got != want {
			t.Errorf("fee at %s = %d, want %d", now, got, want)
		}
	}
	// 09:00 in New York is 14:00 UTC: the same instant read on another clock.
	utc := time.Date(2027, 1, 1, 13, 59, 0, 0, time.UTC)
	if got, _ := feeAt(sch, utc, testLoc); got != 12359 {
		t.Errorf("13:59 UTC is before 09:00 Eastern: got %d", got)
	}
	if msg := checkFees(json.RawMessage(`{"initial_fee_schedule":[{"effective_from":"2026-10-22T11:00","cents":100}]}`), at("2026-10-22T10:00"), testLoc); msg == "" {
		t.Error("a schedule that starts later today has no fee in force now")
	}
	if msg := checkFees(json.RawMessage(`{"initial_fee_schedule":[{"effective_from":"2026-01-01","cents":100},{"effective_from":"2026-01-01T00:00","cents":200}]}`), at("2026-10-22T10:00"), testLoc); msg == "" {
		t.Error("a day and its midnight are the same moment")
	}
}

func TestCheckOutcomes(t *testing.T) {
	list, defs, msg := checkOutcomes(json.RawMessage(`[{"name":" Full Award ","definition":"Awarded in full."},{"name":"No Award"}]`))
	if msg != "" || len(list) != 2 || list[0] != "Full Award" || defs["Full Award"] != "Awarded in full." {
		t.Fatalf("valid list refused or not tidied: %q %v %v", msg, list, defs)
	}
	for name, body := range map[string]string{"empty": `[]`, "no name": `[{"name":" "}]`,
		"duplicate": `[{"name":"Withdrawn"},{"name":"withdrawn"}]`} {
		if _, _, msg := checkOutcomes(json.RawMessage(body)); msg == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDropEstimateParagraphs(t *testing.T) {
	in := "Dear Pat,\n\nWe accepted the dispute.\n\nIf you received an estimated cost of the Full Review, please respond.\n\nThanks."
	if got := dropEstimateParagraphs(in); got != "Dear Pat,\n\nWe accepted the dispute.\n\nThanks." {
		t.Errorf("got %q", got)
	}
}

func TestStepDayOf(t *testing.T) {
	for _, c := range []struct {
		date     any
		day, bad string
	}{
		{nil, "2026-10-22", ""},
		{"", "2026-10-22", ""},
		{"2026-10-20", "2026-10-20", ""},
		{" 2026-10-22 ", "2026-10-22", ""},
		{"2026-10-23", "", "the date can't be later than today"},
		{"10/20/2026", "", "the date must be a date (YYYY-MM-DD)"},
	} {
		data := map[string]any{}
		if c.date != nil {
			data["date"] = c.date
		}
		day, bad := stepDayOf(data, "2026-10-22")
		if day != c.day || bad != c.bad {
			t.Errorf("stepDayOf(%v) = %q, %q; want %q, %q", c.date, day, bad, c.day, c.bad)
		}
	}
	if day, _ := stepDayOf(nil, "2026-10-22"); day != "2026-10-22" {
		t.Errorf("no data: got %q", day)
	}
}

func TestLateChargesDue(t *testing.T) {
	l := &LateFee{Cents: 5000, AfterDays: 35, EveryDays: 30}
	for days, want := range map[int]int{0: 0, 35: 0, 36: 1, 65: 1, 66: 2, 95: 2, 96: 3, 126: 4} {
		if got := l.lateChargesDue(days); got != want {
			t.Errorf("day %d: %d charges, want %d", days, got, want)
		}
	}
	if (*LateFee)(nil).lateChargesDue(400) != 0 || (&LateFee{Cents: 0, AfterDays: 35}).lateChargesDue(400) != 0 {
		t.Error("no late fee configured must charge nothing")
	}
	if (&LateFee{Cents: 5000, AfterDays: 35}).lateChargesDue(400) != 1 {
		t.Error("without every_days the fee is charged once")
	}
}

func TestCardFeeByDay(t *testing.T) {
	sched := []FeeStep{{EffectiveFrom: "2025-01-01", Cents: 12000, CardFeeCents: 388}, {EffectiveFrom: "2026-01-01", Cents: 12360, CardFeeCents: 399}}
	loc := programZone("")
	for day, want := range map[string]int64{"2025-06-01": 388, "2026-01-01": 399, "2026-10-22": 399} {
		at, _ := time.ParseInLocation("2006-01-02", day, loc)
		if st, ok := stepAt(sched, at.Add(time.Hour), loc); !ok || st.CardFeeCents != want {
			t.Errorf("%s: card fee %d, want %d", day, st.CardFeeCents, want)
		}
	}
	if fee, _ := feeAt(sched, time.Date(2026, 3, 1, 12, 0, 0, 0, loc), loc); fee != 12360 {
		t.Errorf("fee %d, want 12360 (the card fee is not part of the fee)", fee)
	}
}

func TestPayMethodAndLinks(t *testing.T) {
	for in, want := range map[string]string{"": payByCard, "card": payByCard, "bank": payByBank, "ach": payByBank, "us_bank_account": payByBank, "x": payByCard} {
		if got := payMethodOf(in); got != want {
			t.Errorf("payMethodOf(%q) = %q", in, got)
		}
	}
	txt := payLinksText(12360, 399, "https://card", "https://bank")
	if !strings.Contains(txt, "Pay by card ($127.59, which includes a $3.99 credit card transaction fee): https://card") ||
		!strings.Contains(txt, "Pay from a bank account ($123.60, no card fee") {
		t.Errorf("links text: %s", txt)
	}
	if strings.Contains(payLinksText(12360, 399, "https://card", ""), "bank account") {
		t.Error("no bank link when the bank page could not be opened")
	}
}
