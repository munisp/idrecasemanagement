// Author: Patrick Munis at newwave
package main

import (
	"fmt"
	"testing"
)

func TestTimeEntryNormalize(t *testing.T) {
	// minutes win over hours
	m, d, err := (&timeEntryIn{Minutes: 45, Hours: 2, EntryDate: "2026-10-08"}).normalize()
	if err != nil || m != 45 || d != "2026-10-08" {
		t.Fatalf("minutes precedence: %d %s %v", m, d, err)
	}
	// hours convert
	m, _, err = (&timeEntryIn{Hours: 1.5}).normalize()
	if err != nil || m != 90 {
		t.Fatalf("hours conversion: %d %v", m, err)
	}
	// zero/negative/over-day rejected
	for _, bad := range []timeEntryIn{{Minutes: 0}, {Minutes: -5}, {Minutes: 1441}, {Hours: 25}} {
		if _, _, err := bad.normalize(); err == nil {
			t.Errorf("expected rejection: %+v", bad)
		}
	}
	// bad date rejected
	if _, _, err := (&timeEntryIn{Minutes: 30, EntryDate: "10/08/2026"}).normalize(); err == nil {
		t.Error("non-ISO date accepted")
	}
}

func TestTimeEntryRole(t *testing.T) {
	// clinical split: DOCTOR/NURSE record as themselves, not the legacy blend
	if got := timeEntryRole(principal{Subject: "x", Roles: []string{"DOCTOR"}}); got != "DOCTOR" {
		t.Errorf("doctor: %s", got)
	}
	if got := timeEntryRole(principal{Subject: "x", Roles: []string{"NURSE", "PLATFORM_ADMIN"}}); got != "NURSE" {
		t.Errorf("nurse preferred over admin: %s", got)
	}
	if got := timeEntryRole(principal{Subject: "x", Roles: []string{"PLATFORM_ADMIN"}}); got != "PLATFORM_ADMIN" {
		t.Errorf("admin fallback: %s", got)
	}
}

func TestToInt64(t *testing.T) {
	if toInt64(int64(7)) != 7 || toInt64(2.0) != 2 || toInt64("42") != 42 || toInt64(nil) != 0 {
		t.Error("toInt64 coercion wrong")
	}
}

func TestTimeReportPeriod(t *testing.T) {
	// Range wins over month/week.
	from, to, _, err := timeReportPeriod("2026-10-05", "2026-10", "2026-10-01", "2026-10-08")
	if err != nil || from != "2026-10-01" || to != "2026-10-08" {
		t.Fatalf("range: %s %s %v", from, to, err)
	}
	// Month preset.
	from, to, _, err = timeReportPeriod("", "2026-02", "", "")
	if err != nil || from != "2026-02-01" || to != "2026-03-01" {
		t.Fatalf("month: %s %s %v", from, to, err)
	}
	// Week preset: any day resolves to the Monday..+7d window.
	from, to, label, err := timeReportPeriod("2026-10-08", "", "", "") // a Thursday
	if err != nil || from != "2026-10-05" || to != "2026-10-12" {
		t.Fatalf("week: %s %s %v", from, to, err)
	}
	if label != "week of 2026-10-05" {
		t.Fatalf("label: %s", label)
	}
	// Errors.
	if _, _, _, err = timeReportPeriod("", "", "2026-10-08", "2026-10-01"); err == nil {
		t.Fatal("reversed range must fail")
	}
	if _, _, _, err = timeReportPeriod("", "", "2025-01-01", "2026-02-01"); err == nil {
		t.Fatal(">366 days must fail")
	}
	if _, _, _, err = timeReportPeriod("", "2026-13", "", ""); err == nil {
		t.Fatal("bad month must fail")
	}
}

func TestCsvCell(t *testing.T) {
	if got := csvCell("plain"); got != "plain" {
		t.Fatal(got)
	}
	if got := csvCell(`a,b`); got != `"a,b"` {
		t.Fatal(got)
	}
	if got := csvCell(`she said "hi"`); got != `"she said ""hi"""` {
		t.Fatal(got)
	}
	if got := moneyCell(nil); got != "" {
		t.Fatal(got)
	}
	var c int64 = 123456
	if got := moneyCell(&c); got != "1234.56" {
		t.Fatal(got)
	}
}

// ---- team-time filters -------------------------------------------------------

func timeFixture() []timeEntryRow {
	return []timeEntryRow{
		{ID: 1, CaseID: "c1", CaseNumber: "FL26-001", Subject: "u-ada", Who: "Ada Reviewer", Role: "CASE_MANAGER", Date: "2026-10-05", Minutes: 60, Billable: true},
		{ID: 2, CaseID: "c1", CaseNumber: "FL26-001", Subject: "u-ben", Who: "Ben Coder", Role: "CODER", Date: "2026-10-06", Minutes: 30, Billable: true},
		{ID: 3, CaseID: "c2", CaseNumber: "FL26-002", Subject: "u-ada", Who: "Ada Reviewer", Role: "CASE_MANAGER", Date: "2026-10-06", Minutes: 45, Billable: false},
		{ID: 4, CaseID: "c2", CaseNumber: "FL26-002", Subject: "u-cy", Who: "Cy Attorney", Role: "ATTORNEY", Date: "2026-10-07", Minutes: 120, Billable: true},
	}
}

func keptIDs(f timeReportFilter) []int64 {
	var ids []int64
	for _, e := range timeFixture() {
		if f.keeps(e) {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

func TestTimeReportFilterKeeps(t *testing.T) {
	cases := []struct {
		name string
		f    timeReportFilter
		want []int64
	}{
		{"no filter keeps everything", timeReportFilter{}, []int64{1, 2, 3, 4}},
		{"by part of a name, any case", timeReportFilter{Who: "ada"}, []int64{1, 3}},
		{"by exact account id", timeReportFilter{Who: "u-ben"}, []int64{2}},
		{"by part of a case number", timeReportFilter{Case: "26-002"}, []int64{3, 4}},
		{"by exact case id", timeReportFilter{Case: "c1"}, []int64{1, 2}},
		{"by role", timeReportFilter{Role: "attorney"}, []int64{4}},
		{"billable only", timeReportFilter{Billable: "yes"}, []int64{1, 2, 4}},
		{"non-billable only", timeReportFilter{Billable: "no"}, []int64{3}},
		{"person and case together", timeReportFilter{Who: "Ada", Case: "FL26-001"}, []int64{1}},
		{"nobody by that name", timeReportFilter{Who: "zed"}, nil},
	}
	for _, c := range cases {
		got := keptIDs(c.f)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: kept %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTimeReportFilterFromIgnoresJunk(t *testing.T) {
	f := timeReportFilterFrom(func(k string) string {
		return map[string]string{"who": "  Ada ", "role": "coder", "billable": "maybe"}[k]
	})
	if f.Who != "Ada" || f.Role != "CODER" || f.Billable != "" {
		t.Errorf("got %+v", f)
	}
	if !(timeReportFilter{}).empty() || f.empty() {
		t.Error("empty() is wrong")
	}
	if d := (timeReportFilter{Who: "Ada", Billable: "yes"}).describe(); d != `team member "Ada", billable only` {
		t.Errorf("describe: %q", d)
	}
}

func TestSummarizeTimeTotalsAndMoney(t *testing.T) {
	rates := map[string]int64{"CASE_MANAGER": 6000, "ATTORNEY": 30000} // cents per hour; CODER has no rate
	r := summarizeTime(timeFixture(), rates, true)
	if r["total_minutes"].(int64) != 255 || r["billable_minutes"].(int64) != 210 {
		t.Errorf("totals: %v / %v", r["total_minutes"], r["billable_minutes"])
	}
	// 60 min at $60/h + 120 min at $300/h; the Coder's 30 billable minutes have no rate.
	if r["total_amount_cents"].(int64) != 6000+60000 || r["unrated_billable_minutes"].(int64) != 30 {
		t.Errorf("money: %v unrated %v", r["total_amount_cents"], r["unrated_billable_minutes"])
	}
	if r["people_count"].(int) != 3 || r["entry_count"].(int) != 4 {
		t.Errorf("counts: %v people, %v entries", r["people_count"], r["entry_count"])
	}
	byCase := r["by_case"].([]*timeReportAcc)
	if len(byCase) != 2 || byCase[0].CaseNumber != "FL26-002" || byCase[0].People != 2 || byCase[0].Minutes != 165 {
		t.Errorf("by case: %+v", byCase[0])
	}
	byPerson := r["by_person"].([]*timeReportAcc)
	if byPerson[0].Who != "Cy Attorney" || byPerson[1].Who != "Ada Reviewer" || byPerson[1].Minutes != 105 || byPerson[1].Billable != 60 {
		t.Errorf("by person: %+v %+v", byPerson[0], byPerson[1])
	}
	// A viewer who may not see money gets hours and no amounts at all.
	plain := summarizeTime(timeFixture(), rates, false)
	if _, ok := plain["total_amount_cents"]; ok || plain["by_person"].([]*timeReportAcc)[0].Amount != nil {
		t.Error("amounts leaked to a viewer without the money screens")
	}
	if empty := summarizeTime(nil, rates, true); empty["total_minutes"].(int64) != 0 || len(empty["by_case"].([]*timeReportAcc)) != 0 {
		t.Error("an empty period should summarize to zero")
	}
}

func TestTimeReportRangeLabelNamesItsLastDay(t *testing.T) {
	from, to, label, err := timeReportPeriod("", "", "2026-10-01", "2026-11-01")
	if err != nil || from != "2026-10-01" || to != "2026-11-01" || label != "2026-10-01 to 2026-10-31" {
		t.Errorf("got %s %s %q %v", from, to, label, err)
	}
}
