package core

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCronNext(t *testing.T) {
	cases := []struct {
		expr, from, want string
	}{
		// the inbox ritual: 08:00 and 17:00
		{"0 8,17 * * *", "2026-07-23 07:59", "2026-07-23 08:00"},
		{"0 8,17 * * *", "2026-07-23 08:00", "2026-07-23 17:00"},
		{"0 8,17 * * *", "2026-07-23 17:00", "2026-07-24 08:00"},
		// a laptop asleep past both fires resumes at the NEXT one, never replays
		{"0 8,17 * * *", "2026-07-25 23:30", "2026-07-26 08:00"},
		{"*/15 * * * *", "2026-07-23 08:01", "2026-07-23 08:15"},
		{"30 9 * * 1", "2026-07-23 12:00", "2026-07-27 09:30"}, // next Monday
		{"0 0 1 * *", "2026-07-23 12:00", "2026-08-01 00:00"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"}, // leap day, past the horizon of a naive year scan
		// both day fields restricted: cron unions them (1st OR Monday)
		{"0 0 1 * 1", "2026-07-23 12:00", "2026-07-27 00:00"},
		{"0 0 1 * 1", "2026-07-28 00:00", "2026-08-01 00:00"},
	}
	for _, c := range cases {
		s, err := ParseCron(c.expr)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		got, err := s.Next(at(c.from))
		if err != nil {
			t.Errorf("%s from %s: %v", c.expr, c.from, err)
			continue
		}
		if want := at(c.want); !got.Equal(want) {
			t.Errorf("%s from %s: got %s, want %s", c.expr, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
}

func TestCronRejectsGarbage(t *testing.T) {
	for _, expr := range []string{
		"", "0 8 * *", "0 8 * * * *",
		"60 * * * *",  // minute out of range
		"0 24 * * *",  // hour out of range
		"0 8 0 * *",   // day-of-month is 1-based
		"0 8 * * 7",   // day-of-week is 0-6
		"x * * * *",   // not a number
		"0 8-4 * * *", // inverted range
		"*/0 * * * *", // zero step
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) accepted an invalid expression", expr)
		}
	}
}
