// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
	"time"
)

func TestJournalTailArgs(t *testing.T) {
	since := time.Date(2026, 11, 1, 20, 30, 5, 0, time.Local)
	got, ok := journalTailArgs("forge-a1", since, 12)
	want := []string{"--no-pager", "-o", "cat", "-n", "12", "-u", "forge-a1", "--since", "2026-11-01 20:30:05"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v (ok=%v), want %v", got, ok, want)
	}
	if got, _ := journalTailArgs("forge-a2.service", time.Time{}, 0); reflect.DeepEqual(got, nil) || contains(got, "--since") {
		t.Errorf("a zero time must not add --since, and a non-positive line count must fall back: %v", got)
	}
	if got, _ := journalTailArgs("forge-a1", time.Time{}, 100000); !contains(got, "12") {
		t.Errorf("an absurd line count must be clamped: %v", got)
	}
	for _, bad := range []string{"", "forge-a1; rm -rf /", "-u", "--vacuum-size=1", "a b", "forge-a1\n", "$(id)", "../x"} {
		if args, ok := journalTailArgs(bad, since, 12); ok || args != nil {
			t.Errorf("unit %q must be refused, got %v", bad, args)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
