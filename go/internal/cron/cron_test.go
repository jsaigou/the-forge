// SPDX-License-Identifier: Apache-2.0

package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestParseValid(t *testing.T) {
	for _, expr := range []string{
		"* * * * *",
		"0 0 * * *",
		"30 4 1 * *",
		"*/15 * * * *",
		"0 */6 * * *",
		"0 9-17 * * 1-5",
		"5,35 2,14 1,15 * 0",
		"0-30/10 8 * * 1,3,5",
		"59 23 31 12 6",
	} {
		mustParse(t, expr)
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []struct{ expr, wantSub string }{
		{"", "want 5 fields"},
		{"* * * *", "want 5 fields"},
		{"* * * * * *", "want 5 fields"},
		{"60 * * * *", "minute"},
		{"* 24 * * *", "hour"},
		{"* * 0 * *", "day-of-month"},
		{"* * * 13 *", "month"},
		{"* * * * 7", "day-of-week"},
		{"a * * * *", "minute"},
		{"1- * * * *", "minute"},
		{"*/0 * * * *", "step"},
		{"*-5 * * * *", "minute"},
	}
	for _, c := range cases {
		if _, err := Parse(c.expr); err == nil {
			t.Errorf("Parse(%q) = nil error, want one mentioning %q", c.expr, c.wantSub)
		} else if !contains(err.Error(), c.wantSub) {
			t.Errorf("Parse(%q) error = %q, want it to mention %q", c.expr, err.Error(), c.wantSub)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// at builds a local wall-clock time for table readability.
func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.Local)
}

func TestNextTable(t *testing.T) {
	cases := []struct {
		name  string
		expr  string
		after time.Time
		want  time.Time
	}{
		{"next minute", "* * * * *", at(2026, 8, 25, 10, 0), at(2026, 8, 25, 10, 1)},
		{"same minute excluded", "0 * * * *", at(2026, 8, 25, 10, 0), at(2026, 8, 25, 11, 0)},
		{"hourly step", "30 * * * *", at(2026, 8, 25, 10, 45), at(2026, 8, 25, 11, 30)},
		{"daily", "15 4 * * *", at(2026, 8, 25, 10, 0), at(2026, 8, 26, 4, 15)},
		{"every 15 min wraps hour", "*/15 * * * *", at(2026, 8, 25, 10, 16), at(2026, 8, 25, 10, 30)},
		{"step from zero not anchor", "*/7 * * * *", at(2026, 8, 25, 10, 8), at(2026, 8, 25, 10, 14)},
		{"range with step", "0-20/10 * * * *", at(2026, 8, 25, 9, 55), at(2026, 8, 25, 10, 0)},
		{"list of minutes", "5,35 * * * *", at(2026, 8, 25, 10, 36), at(2026, 8, 25, 11, 5)},
		{"month boundary", "0 0 1 * *", at(2026, 8, 25, 10, 0), at(2026, 9, 1, 0, 0)},
		{"year boundary", "0 0 1 1 *", at(2026, 8, 25, 10, 0), at(2027, 1, 1, 0, 0)},
		{"specific date this year passed", "0 2 10 8 *", at(2026, 8, 25, 10, 0), at(2027, 8, 10, 2, 0)},
		{"feb 29 fires on leap year", "0 3 29 2 *", at(2026, 8, 25, 10, 0), at(2028, 2, 29, 3, 0)},
		{"dow sunday=0 after tuesday", "0 12 * * 0", at(2026, 8, 25, 10, 0), at(2026, 8, 30, 12, 0)}, // Aug 25 2026 = Tue
		{"dow friday=5 before friday", "30 9 * * 5", at(2026, 8, 24, 10, 0), at(2026, 8, 28, 9, 30)},
		{"dom restricted only", "0 0 15 * *", at(2026, 8, 25, 10, 0), at(2026, 9, 15, 0, 0)},
		{"dom+dow OR rule matches dow", "0 0 15 * 1", at(2026, 8, 25, 10, 0), at(2026, 8, 31, 0, 0)}, // Mon Aug 31
		{"dom+dow OR rule matches dom", "0 0 15 * 1", at(2026, 9, 16, 10, 0), at(2026, 9, 21, 0, 0)}, // Mon Sep 21 (Sep 15 was Tue)
		{"end of month rollover", "59 23 * * *", at(2026, 8, 31, 23, 59), at(2026, 9, 1, 23, 59)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mustParse(t, c.expr).Next(c.after)
			if !got.Equal(c.want) {
				t.Errorf("Next(%s) = %s, want %s", c.after.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
			}
		})
	}
}

func TestNextImpossibleReturnsZero(t *testing.T) {
	// Feb never has 30 days — no fire time within the horizon.
	got := mustParse(t, "0 0 30 2 *").Next(at(2026, 8, 25, 10, 0))
	if !got.IsZero() {
		t.Errorf("impossible schedule Next = %v, want zero time", got)
	}
}

func TestNextSubMinutePrecisionDiscarded(t *testing.T) {
	after := at(2026, 8, 25, 10, 0).Add(37 * time.Second)
	got := mustParse(t, "* * * * *").Next(after)
	want := at(2026, 8, 25, 10, 1)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v (sub-minute precision discarded)", got, want)
	}
}
