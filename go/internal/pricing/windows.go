// Package pricing evaluates provider time-of-day pricing windows (e.g.
// DeepSeek's UTC peak/off-peak schedule) against a point in time. It knows
// nothing about money — callers pick a price tier by name ("peak"/
// "off_peak"/"flat") and apply their own rates.
package pricing

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// TierPeak/TierOffPeak/TierFlat are the tier names Active/TierAt return.
// "flat" means the provider has no configured windows at all — every
// request is priced at the base (off-peak) rate unconditionally.
const (
	TierPeak    = "peak"
	TierOffPeak = "off_peak"
	TierFlat    = "flat"
)

// Window is one recurring time-of-day window (in the owning Windows'
// timezone — see Windows.TZ), active on the listed weekdays between Start
// (inclusive) and End (exclusive) — a half-open [Start, End) range, so a
// request landing exactly on End is NOT in the window. Start/End are
// "HH:MM", 24h; a window may wrap midnight (Start > End), e.g.
// "22:00"-"02:00".
type Window struct {
	Days  []time.Weekday `json:"days"`
	Start string         `json:"start"`
	End   string         `json:"end"`
}

// Windows is a provider's full peak schedule. The zero value has no
// windows, so every time is off-peak-with-no-peak-concept (Flat).
//
// TZ is an IANA zone name ("America/Los_Angeles", "Asia/Tokyo"); ""
// (the default, and the only value before the 2026-09-13 timezone-input
// sprint) means UTC. A provider's published peak hours are almost always
// quoted in one local timezone ("9am-5pm Pacific"), not UTC — requiring the
// operator to hand-convert to UTC was a real usability trap: it goes stale
// twice a year across a DST transition, since a fixed UTC offset entered in
// January is wrong by an hour in July. Evaluating in the configured zone
// via Go's IANA tzdata (time.Time.In) is correct across DST automatically
// — there is no stored UTC range to go stale, because none is stored;
// every evaluation re-derives the real offset for that exact date.
type Windows struct {
	TZ      string   `json:"tz,omitempty"`
	Windows []Window `json:"windows"`
}

// locCache memoizes time.LoadLocation — Active runs on the remote-request
// hot path (router/usage.go's computeCostNative, once per response), and
// the stdlib re-parses zoneinfo from disk/embedded data on every call with
// no cache of its own. IANA zone data is static for the life of the
// process, so caching by name is safe.
var locCache sync.Map // string -> *time.Location

func loadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if v, ok := locCache.Load(name); ok {
		return v.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("pricing: unknown timezone %q: %w", name, err)
	}
	locCache.Store(name, loc)
	return loc, nil
}

// minutesOfDay parses "HH:MM" into minutes since midnight.
func minutesOfDay(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, fmt.Errorf("pricing: invalid time %q: %w", s, err)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("pricing: time %q out of range", s)
	}
	return h*60 + m, nil
}

// Parse decodes a Windows schedule from its JSON-encoded form (as stored in
// router_providers.peak_windows). An empty string is valid and yields the
// zero value (no windows configured, UTC). TZ (if present) is validated
// against the real IANA database — an unrecognized name (a typo, or an
// abbreviation like "PST" that IANA deliberately doesn't resolve — always
// use the full zone name, e.g. "America/Los_Angeles") is rejected here
// rather than silently degrading to UTC at evaluation time. Every window is
// validated: weekdays in 0-6, well-formed "HH:MM" times, and Start != End
// (a zero-width window can never be intentional and would otherwise
// silently match nothing).
func Parse(raw string) (Windows, error) {
	var w Windows
	if raw == "" {
		return w, nil
	}
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return Windows{}, fmt.Errorf("pricing: parse windows: %w", err)
	}
	if _, err := loadLocation(w.TZ); err != nil {
		return Windows{}, err
	}
	for i, win := range w.Windows {
		for _, d := range win.Days {
			if d < time.Sunday || d > time.Saturday {
				return Windows{}, fmt.Errorf("pricing: window %d: day %d out of range 0-6", i, d)
			}
		}
		if len(win.Days) == 0 {
			return Windows{}, fmt.Errorf("pricing: window %d: no days set", i)
		}
		start, err := minutesOfDay(win.Start)
		if err != nil {
			return Windows{}, fmt.Errorf("pricing: window %d: start: %w", i, err)
		}
		end, err := minutesOfDay(win.End)
		if err != nil {
			return Windows{}, fmt.Errorf("pricing: window %d: end: %w", i, err)
		}
		if start == end {
			return Windows{}, fmt.Errorf("pricing: window %d: start == end (%s), zero-width window", i, win.Start)
		}
	}
	return w, nil
}

// dayIn reports whether d appears in days.
func dayIn(days []time.Weekday, d time.Weekday) bool {
	for _, x := range days {
		if x == d {
			return true
		}
	}
	return false
}

// activeOn reports whether lt — already converted into the schedule's
// configured zone by the caller (Active) — falls inside win. A window that
// wraps midnight (Start > End) is active either on the day it starts (from
// Start through 24:00) or on the following day (from 00:00 up to End), so
// the weekday check is applied to whichever side of the wrap lt's own local
// weekday matches.
func (win Window) activeOn(lt time.Time) bool {
	start, err := minutesOfDay(win.Start)
	if err != nil {
		return false
	}
	end, err := minutesOfDay(win.End)
	if err != nil {
		return false
	}
	nowMin := lt.Hour()*60 + lt.Minute()
	weekday := lt.Weekday()

	if start < end {
		return dayIn(win.Days, weekday) && nowMin >= start && nowMin < end
	}
	// Wraps midnight: the "start day" segment runs [start, 24:00) on the
	// listed weekday; the "end day" segment runs [0:00, end) on the
	// following weekday.
	if dayIn(win.Days, weekday) && nowMin >= start {
		return true
	}
	prevDay := (weekday + 6) % 7 // yesterday, wrapping Sunday->Saturday
	return dayIn(win.Days, prevDay) && nowMin < end
}

// Active reports whether t falls inside any configured window. t is
// converted into the schedule's configured zone (w.TZ, "" = UTC) before
// any day-of-week or time-of-day comparison — a real IANA conversion via
// Go's tzdata, not a fixed offset, so it stays correct across a DST
// transition and correctly shifts which calendar day/hour it is in zones
// far from UTC (e.g. a window quoted "Monday 00:00-04:00 Asia/Tokyo" is
// evaluated as Monday in Tokyo, which is still Sunday afternoon UTC).
func (w Windows) Active(t time.Time) bool {
	if len(w.Windows) == 0 {
		return false
	}
	loc, err := loadLocation(w.TZ)
	if err != nil {
		return false // invalid zone (shouldn't happen post-Parse) — degrade safely, never panic
	}
	lt := t.In(loc)
	for _, win := range w.Windows {
		if win.activeOn(lt) {
			return true
		}
	}
	return false
}

// TierAt returns TierFlat when w has no windows configured at all (the
// provider has no peak/off-peak concept), otherwise TierPeak or
// TierOffPeak depending on whether t falls inside a window.
func (w Windows) TierAt(t time.Time) string {
	if len(w.Windows) == 0 {
		return TierFlat
	}
	if w.Active(t) {
		return TierPeak
	}
	return TierOffPeak
}
