// Author: Patrick Munis at newwave
package main

// Due dates for the program's clocks (change log 2026-11-25 #23: how a due date that falls on a weekend or a holiday
// is handled). The rule, as Florida computes time periods (Fla. R. Gen. Prac. & Jud. Admin. 2.514):
//
//   - a calendar-day period that ends on a Saturday, a Sunday or a legal holiday runs to the next business day;
//   - a business-day period counts Monday to Friday only, never a legal holiday.
//
// The legal holidays are the State of Florida's paid holidays (s. 110.117, F.S.): New Year's Day, Martin Luther
// King Jr. Day, Memorial Day, Independence Day, Labor Day, Veterans Day, Thanksgiving Day and the Friday after it,
// and Christmas Day. One that falls on a Saturday is observed the Friday before, on a Sunday the Monday after.
// The federal NSA engine keeps its own calendar (addBusinessDays in engines.go).

import (
	"fmt"
	"time"
)

// nthWeekday: the n-th (1-based; -1 = last) weekday wd of month m in year y.
func nthWeekday(y int, m time.Month, wd time.Weekday, n int) time.Time {
	if n < 0 {
		d := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC)
		for d.Weekday() != wd {
			d = d.AddDate(0, 0, -1)
		}
		return d
	}
	d := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	for d.Weekday() != wd {
		d = d.AddDate(0, 0, 1)
	}
	return d.AddDate(0, 0, 7*(n-1))
}

// observed: a fixed-date holiday on a Saturday is kept the Friday before, on a Sunday the Monday after.
func observed(d time.Time) time.Time {
	switch d.Weekday() {
	case time.Saturday:
		return d.AddDate(0, 0, -1)
	case time.Sunday:
		return d.AddDate(0, 0, 1)
	}
	return d
}

// flHolidays: the State of Florida's legal holidays observed in year y, by name.
func flHolidays(y int) map[string]string {
	thanksgiving := nthWeekday(y, time.November, time.Thursday, 4)
	out := map[string]string{}
	for name, d := range map[string]time.Time{
		"New Year's Day":                observed(time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC)),
		"Martin Luther King Jr. Day":    nthWeekday(y, time.January, time.Monday, 3),
		"Memorial Day":                  nthWeekday(y, time.May, time.Monday, -1),
		"Independence Day":              observed(time.Date(y, time.July, 4, 0, 0, 0, 0, time.UTC)),
		"Labor Day":                     nthWeekday(y, time.September, time.Monday, 1),
		"Veterans Day":                  observed(time.Date(y, time.November, 11, 0, 0, 0, 0, time.UTC)),
		"Thanksgiving Day":              thanksgiving,
		"Friday after Thanksgiving Day": thanksgiving.AddDate(0, 0, 1),
		"Christmas Day":                 observed(time.Date(y, time.December, 25, 0, 0, 0, 0, time.UTC)),
	} {
		out[d.Format("2006-01-02")] = name
	}
	return out
}

// programHoliday: the holiday's name when d is a legal holiday (next year's New Year's Day may be observed on
// this year's December 31).
func programHoliday(d time.Time) string {
	k := d.Format("2006-01-02")
	if n := flHolidays(d.Year())[k]; n != "" {
		return n
	}
	return flHolidays(d.Year() + 1)[k]
}

// programBusinessDay: Monday to Friday and not a legal holiday.
func programBusinessDay(d time.Time) bool {
	wd := d.Weekday()
	return wd != time.Saturday && wd != time.Sunday && programHoliday(d) == ""
}

// programAddBusinessDays: n business days after start.
func programAddBusinessDays(start time.Time, n int) time.Time {
	d := start
	for n > 0 {
		d = d.AddDate(0, 0, 1)
		if programBusinessDay(d) {
			n--
		}
	}
	return d
}

// programBusinessDaysBetween: business days from a to b (negative when b is before a).
func programBusinessDaysBetween(a, b time.Time) int {
	a = time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	b = time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	sign := 1
	if b.Before(a) {
		a, b, sign = b, a, -1
	}
	n := 0
	for d := a; d.Before(b); {
		d = d.AddDate(0, 0, 1)
		if programBusinessDay(d) {
			n++
		}
	}
	return sign * n
}

// programDue: a program clock's due date -- n business days, or n calendar days moved off a weekend or holiday to
// the next business day. why says what moved it ("" when nothing did).
func programDue(basis time.Time, n int, dayType string) (due time.Time, why string) {
	if dayType == "business" {
		return programAddBusinessDays(basis, n), ""
	}
	due = basis.AddDate(0, 0, n)
	if programBusinessDay(due) {
		return due, ""
	}
	from := due
	for !programBusinessDay(due) {
		due = due.AddDate(0, 0, 1)
	}
	what := from.Weekday().String()
	if h := programHoliday(from); h != "" {
		what = h
	}
	return due, fmt.Sprintf("day %d fell on %s %s (%s): due the next business day", n, from.Weekday().String()[:3], from.Format("2006-01-02"), what)
}
