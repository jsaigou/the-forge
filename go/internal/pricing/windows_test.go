package pricing

import (
	"testing"
	"time"
)

// deepseekSchedule mirrors the real published DeepSeek peak schedule:
// 01:00-04:00 and 06:00-10:00 UTC, Monday-Friday.
const deepseekSchedule = `{"windows":[
	{"days":[1,2,3,4,5],"start":"01:00","end":"04:00"},
	{"days":[1,2,3,4,5],"start":"06:00","end":"10:00"}
]}`

func mustParse(t *testing.T, raw string) Windows {
	t.Helper()
	w, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	return w
}

func utc(y, m, d, h, min int) time.Time {
	return time.Date(y, time.Month(m), d, h, min, 0, 0, time.UTC)
}

func TestActive_DeepSeekSchedule(t *testing.T) {
	w := mustParse(t, deepseekSchedule)

	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"Tue 02:00 in first window", utc(2026, 9, 15, 2, 0), true},
		{"Tue 04:00 exact end is off-peak (half-open)", utc(2026, 9, 15, 4, 0), false},
		{"Tue 03:59 still in window", utc(2026, 9, 15, 3, 59), true},
		{"Tue 01:00 exact start is peak (half-open)", utc(2026, 9, 15, 1, 0), true},
		{"Tue 05:00 gap between windows is off-peak", utc(2026, 9, 15, 5, 0), false},
		{"Tue 06:00 in second window", utc(2026, 9, 15, 6, 0), true},
		{"Tue 09:59 still in second window", utc(2026, 9, 15, 9, 59), true},
		{"Tue 10:00 exact end is off-peak", utc(2026, 9, 15, 10, 0), false},
		{"Tue 12:00 off-peak", utc(2026, 9, 15, 12, 0), false},
		{"Sat 02:00 weekday excluded", utc(2026, 9, 19, 2, 0), false},
		{"Sun 07:00 weekday excluded", utc(2026, 9, 20, 7, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := w.Active(c.t)
			if got != c.want {
				t.Errorf("Active(%s) = %v, want %v", c.t.Format(time.RFC3339), got, c.want)
			}
		})
	}
}

func TestActive_UTCConversion(t *testing.T) {
	w := mustParse(t, deepseekSchedule)
	// 02:00 UTC on a Tuesday, expressed in a non-UTC location, must still
	// evaluate as UTC (Active must not use the input's own wall-clock
	// fields).
	loc := time.FixedZone("UTC-5", -5*60*60)
	inPST := time.Date(2026, 9, 14, 21, 0, 0, 0, loc) // == Tue 2026-09-15 02:00 UTC
	if !w.Active(inPST) {
		t.Errorf("Active did not convert non-UTC input to UTC before evaluating")
	}
}

func TestActive_MidnightWrap(t *testing.T) {
	// A window from 22:00 Mon through 02:00 (next day) Mon-listed.
	w := mustParse(t, `{"windows":[{"days":[1],"start":"22:00","end":"02:00"}]}`)

	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"Mon 23:00 in start-day segment", utc(2026, 9, 14, 23, 0), true},
		{"Tue 01:00 in end-day segment (wrap)", utc(2026, 9, 15, 1, 0), true},
		{"Tue 02:00 exact end is off (half-open)", utc(2026, 9, 15, 2, 0), false},
		{"Mon 21:59 before start", utc(2026, 9, 14, 21, 59), false},
		{"Wed 01:00 wrong day entirely", utc(2026, 9, 16, 1, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := w.Active(c.t)
			if got != c.want {
				t.Errorf("Active(%s) = %v, want %v", c.t.Format(time.RFC3339), got, c.want)
			}
		})
	}
}

func TestTierAt(t *testing.T) {
	w := mustParse(t, deepseekSchedule)
	if got := w.TierAt(utc(2026, 9, 15, 2, 0)); got != TierPeak {
		t.Errorf("TierAt(peak time) = %q, want %q", got, TierPeak)
	}
	if got := w.TierAt(utc(2026, 9, 15, 12, 0)); got != TierOffPeak {
		t.Errorf("TierAt(off-peak time) = %q, want %q", got, TierOffPeak)
	}

	flat := Windows{}
	if got := flat.TierAt(utc(2026, 9, 15, 2, 0)); got != TierFlat {
		t.Errorf("TierAt(no windows) = %q, want %q", got, TierFlat)
	}
}

func TestParse_Empty(t *testing.T) {
	w, err := Parse("")
	if err != nil {
		t.Fatalf("Parse(\"\") error: %v", err)
	}
	if len(w.Windows) != 0 {
		t.Errorf("Parse(\"\") = %+v, want zero value", w)
	}
}

func TestParse_Rejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"malformed json", `{not json`},
		{"start equals end", `{"windows":[{"days":[1],"start":"04:00","end":"04:00"}]}`},
		{"day out of range", `{"windows":[{"days":[7],"start":"01:00","end":"02:00"}]}`},
		{"negative day", `{"windows":[{"days":[-1],"start":"01:00","end":"02:00"}]}`},
		{"no days", `{"windows":[{"days":[],"start":"01:00","end":"02:00"}]}`},
		{"bad start format", `{"windows":[{"days":[1],"start":"1am","end":"02:00"}]}`},
		{"hour out of range", `{"windows":[{"days":[1],"start":"25:00","end":"02:00"}]}`},
		{"minute out of range", `{"windows":[{"days":[1],"start":"01:60","end":"02:00"}]}`},
		{"unknown timezone", `{"tz":"Not/AZone","windows":[{"days":[1],"start":"01:00","end":"02:00"}]}`},
		{"tz abbreviation not a real IANA name", `{"tz":"PST","windows":[{"days":[1],"start":"01:00","end":"02:00"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(c.raw); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", c.raw)
			}
		})
	}
}

// ── Timezone support (2026-09-13) ───────────────────────────────────────────

// TestActive_TimezoneDST proves the schedule is evaluated in the
// configured zone's REAL local wall-clock time — via Go's IANA tzdata, not
// a fixed offset frozen at entry time — so it stays correct across a DST
// transition with no stored UTC range to go stale. A "09:00-17:00
// America/Los_Angeles" window is active at 16:30 UTC in July (PDT, UTC-7 ->
// 09:30 local) but NOT at the same 16:30 UTC clock time in January (PST,
// UTC-8 -> 08:30 local, before the window opens).
func TestActive_TimezoneDST(t *testing.T) {
	w := mustParse(t, `{"tz":"America/Los_Angeles","windows":[{"days":[1,2,3,4,5],"start":"09:00","end":"17:00"}]}`)

	// 2026-01-15 is a Thursday; 2026-07-15 is a Wednesday. Both weekdays.
	winterInactive := utc(2026, 1, 15, 16, 30) // 08:30 PST -- before the window
	winterActive := utc(2026, 1, 15, 17, 30)   // 09:30 PST -- inside the window
	summerActive := utc(2026, 7, 15, 16, 30)   // 09:30 PDT -- inside the window (DST)

	if w.Active(winterInactive) {
		t.Error("Active(16:30 UTC in January / 08:30 PST) = true, want false")
	}
	if !w.Active(winterActive) {
		t.Error("Active(17:30 UTC in January / 09:30 PST) = false, want true")
	}
	if !w.Active(summerActive) {
		t.Error("Active(16:30 UTC in July / 09:30 PDT, DST) = false, want true — same UTC clock time as the January-inactive case, but DST shifts the local hour into the window")
	}
}

// TestActive_TimezoneShiftsWeekday proves the WEEKDAY the schedule matches
// against is the configured zone's local day, not UTC's — a window quoted
// "Monday 00:00-04:00 Asia/Tokyo" (UTC+9, no DST) is active during Sunday
// afternoon UTC, because it's already Monday in Tokyo.
func TestActive_TimezoneShiftsWeekday(t *testing.T) {
	w := mustParse(t, `{"tz":"Asia/Tokyo","windows":[{"days":[1],"start":"00:00","end":"04:00"}]}`)

	// 2026-01-11 is a Sunday (UTC).
	sundayNoon := utc(2026, 1, 11, 12, 0)      // 21:00 JST Sunday -- still Sunday locally
	sundayAfternoon := utc(2026, 1, 11, 16, 0) // 01:00 JST Monday -- already Monday locally, inside the window

	if w.Active(sundayNoon) {
		t.Error("Active(12:00 UTC Sun / 21:00 JST Sun) = true, want false (still Sunday in Tokyo)")
	}
	if !w.Active(sundayAfternoon) {
		t.Error("Active(16:00 UTC Sun / 01:00 JST Mon) = false, want true (already Monday in Tokyo, inside the window)")
	}
}

// TestActive_EmptyTZDefaultsUTC confirms an unset tz behaves exactly as
// before the timezone-input sprint (the DeepSeek schedule stored live has
// no "tz" key at all).
func TestActive_EmptyTZDefaultsUTC(t *testing.T) {
	withTZ := mustParse(t, `{"tz":"UTC","windows":[{"days":[1,2,3,4,5],"start":"01:00","end":"04:00"}]}`)
	withoutTZ := mustParse(t, deepseekSchedule)
	tPeak := utc(2026, 9, 15, 2, 0)
	tOff := utc(2026, 9, 15, 12, 0)
	if withTZ.Active(tPeak) != withoutTZ.Active(tPeak) {
		t.Error("explicit tz:UTC and omitted tz disagree at a peak time")
	}
	if withTZ.Active(tOff) != withoutTZ.Active(tOff) {
		t.Error("explicit tz:UTC and omitted tz disagree at an off-peak time")
	}
}
