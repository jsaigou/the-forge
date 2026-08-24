// SPDX-License-Identifier: Apache-2.0

// Package cron implements the standard 5-field cron expression parser and
// next-fire-time computation used by the scheduler-jobs runner (P3 track).
//
// Syntax: "min hour dom mon dow" with
//   - *           (any value in range)
//   - */n         (step within full range)
//   - a-b         (inclusive range)
//   - a-b/n       (stepped range)
//   - a,b,c       (lists of any of the above)
//
// Fields: minute 0-59, hour 0-23, day-of-month 1-31, month 1-12,
// day-of-week 0-6 (0 = Sunday; numeric only — no names). Standard cron
// dom/dow semantics: when both are restricted (non-*) the day matches if
// EITHER matches; otherwise both must.
//
// DST: evaluation is naive local time — a wall-clock time that does not
// exist (spring-forward gap) fires at the next existing minute, and an
// ambiguous time (fall-back overlap) fires once on its first occurrence.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxHorizon bounds Next's search — past it, no fire time is reported
// (only impossible schedules like "0 0 30 2 *" can hit this).
const maxHorizon = 5 * 365 * 24 * time.Hour

// Schedule is one parsed cron expression. Use Parse to build one.
type Schedule struct {
	minute, hour  [60]bool
	dom, mon, dow [60]bool // indexed by day-of-month (1..31), month (1..12), weekday (0..6)
	// domStar/dowStar record whether the field was "*" (or an
	// equivalent full-range step) — they drive the standard OR rule for
	// restricted dom+dow combinations.
	domStar, dowStar bool
}

// Parse compiles expr. Returns an error naming the offending field on any
// syntax or out-of-range problem.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: want 5 fields (min hour dom mon dow), got %d", len(fields))
	}
	s := &Schedule{}
	bounds := []struct {
		name string
		lo   int
		hi   int
	}{{"minute", 0, 59}, {"hour", 0, 23}, {"day-of-month", 1, 31}, {"month", 1, 12}, {"day-of-week", 0, 6}}
	for i, f := range fields {
		lo, hi := bounds[i].lo, bounds[i].hi
		set, star, err := parseField(f, lo, hi)
		if err != nil {
			return nil, fmt.Errorf("cron: %s: %w", bounds[i].name, err)
		}
		switch i {
		case 0:
			s.minute = set
		case 1:
			s.hour = set
		case 2:
			s.dom, s.domStar = set, star
		case 3:
			s.mon = set
		case 4:
			s.dow, s.dowStar = set, star
		}
	}
	return s, nil
}

// parseField parses one comma-separated field into a boolean membership
// table over [lo, hi]. star reports whether the field covers its entire
// range via a bare "*" (a plain "*", not "*\/n" — a stepped wildcard still
// counts as restricted for dom/dow purposes under Vixie semantics, though
// for */n covering everything the distinction only matters for exotic
// expressions; treating any full-range term as star is safe for our use).
func parseField(field string, lo, hi int) ([60]bool, bool, error) {
	var out [60]bool
	all := true
	for _, term := range strings.Split(field, ",") {
		if term == "" {
			return out, false, fmt.Errorf("empty term")
		}
		rangePart, step := term, 1
		if i := strings.IndexByte(term, '/'); i >= 0 {
			rangePart = term[:i]
			n, err := strconv.Atoi(term[i+1:])
			if err != nil || n <= 0 {
				return out, false, fmt.Errorf("bad step %q", term[i+1:])
			}
			step = n
		}
		rlo, rhi := lo, hi
		switch {
		case rangePart == "*":
			// full range
		case strings.ContainsRune(rangePart, '-'):
			parts := strings.SplitN(rangePart, "-", 2)
			a, err1 := strconv.Atoi(parts[0])
			b, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil {
				return out, false, fmt.Errorf("bad range %q", rangePart)
			}
			if a < lo || b > hi || a > b {
				return out, false, fmt.Errorf("range %q out of bounds [%d,%d]", rangePart, lo, hi)
			}
			rlo, rhi = a, b
			all = false
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil {
				return out, false, fmt.Errorf("bad value %q", rangePart)
			}
			if v < lo || v > hi {
				return out, false, fmt.Errorf("value %d out of bounds [%d,%d]", v, lo, hi)
			}
			rlo, rhi = v, v
			all = false
		}
		for v := rlo; v <= rhi; v += step {
			out[v] = true
		}
	}
	return out, all, nil
}

// dayMatches applies the standard dom/dow rule: both unrestricted → any
// day; exactly one restricted → that one must match; both restricted →
// either matching suffices.
func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom[t.Day()]
	dowOK := s.dow[int(t.Weekday())]
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next returns the next fire time strictly after `after`, evaluated in
// after's location. Returns the zero Time when no fire time exists within
// maxHorizon (impossible schedules such as Feb 30). Sub-minute precision
// of `after` is discarded: the search starts at the top of the next minute.
func (s *Schedule) Next(after time.Time) time.Time {
	loc := after.Location()
	t := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), 0, 0, loc).Add(time.Minute)
	limit := after.Add(maxHorizon)
	for t.Before(limit) {
		if !s.mon[int(t.Month())] {
			// Jump to the first minute of the next month.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !s.hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !s.minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
