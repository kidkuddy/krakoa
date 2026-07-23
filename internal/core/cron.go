package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A 5-field cron matcher: minute hour day-of-month month day-of-week.
// Supports *, n, a-b, a,b,c and */n (and a-b/n) in every field.
//
// Deliberately not a dependency: the whole surface is "does this minute
// match" plus "find the next one", and Next searches minute by minute from
// a bounded window. That is obviously correct — no month-length or leap-year
// arithmetic to get wrong — and it runs once per fire, not in a hot loop.
type Schedule struct {
	min, hour, dom, mon, dow map[int]bool
	// domRestricted/dowRestricted carry the one classic cron gotcha: when
	// BOTH day fields are restricted the match is their union, not their
	// intersection ("0 0 1 * 1" is the 1st AND every Monday).
	domRestricted, dowRestricted bool
}

type cronField struct {
	name     string
	min, max int
}

var cronFields = []cronField{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 6},
}

// ParseCron parses a 5-field cron expression.
func ParseCron(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (min hour dom mon dow), got %d", expr, len(parts))
	}
	sets := make([]map[int]bool, 5)
	for i, f := range cronFields {
		set, err := parseCronField(parts[i], f)
		if err != nil {
			return nil, err
		}
		sets[i] = set
	}
	return &Schedule{
		min: sets[0], hour: sets[1], dom: sets[2], mon: sets[3], dow: sets[4],
		domRestricted: parts[2] != "*", dowRestricted: parts[4] != "*",
	}, nil
}

func parseCronField(spec string, f cronField) (map[int]bool, error) {
	out := map[int]bool{}
	for _, term := range strings.Split(spec, ",") {
		step := 1
		if i := strings.Index(term, "/"); i >= 0 {
			n, err := strconv.Atoi(term[i+1:])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("cron %s: bad step in %q", f.name, term)
			}
			step, term = n, term[:i]
		}
		lo, hi := f.min, f.max
		switch {
		case term == "*":
		case strings.Contains(term, "-"):
			ab := strings.SplitN(term, "-", 2)
			a, err1 := strconv.Atoi(ab[0])
			b, err2 := strconv.Atoi(ab[1])
			if err1 != nil || err2 != nil || a > b {
				return nil, fmt.Errorf("cron %s: bad range %q", f.name, term)
			}
			lo, hi = a, b
		default:
			n, err := strconv.Atoi(term)
			if err != nil {
				return nil, fmt.Errorf("cron %s: bad value %q", f.name, term)
			}
			lo, hi = n, n
		}
		if lo < f.min || hi > f.max {
			return nil, fmt.Errorf("cron %s: %q out of range %d-%d", f.name, term, f.min, f.max)
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cron %s: %q matches nothing", f.name, spec)
	}
	return out, nil
}

// Matches reports whether t (to the minute) satisfies the schedule.
func (s *Schedule) Matches(t time.Time) bool {
	if !s.min[t.Minute()] || !s.hour[t.Hour()] || !s.mon[int(t.Month())] {
		return false
	}
	dom, dow := s.dom[t.Day()], s.dow[int(t.Weekday())]
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	}
	return true
}

// cronHorizon bounds the search: four years covers every leap-day schedule.
const cronHorizon = 4 * 366 * 24 * 60

// Next returns the first matching minute strictly after t, in t's location.
func (s *Schedule) Next(t time.Time) (time.Time, error) {
	// truncate to the minute, then step
	c := t.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < cronHorizon; i++ {
		if s.Matches(c) {
			return c, nil
		}
		c = c.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron: no match within four years of %s", t)
}
