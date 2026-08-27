// SPDX-License-Identifier: MIT

package activity

import (
	"testing"
	"time"
)

func TestRegistryMarkAndLabel(t *testing.T) {
	r := New()
	if got := r.Label("a1", time.Minute); got != "" {
		t.Errorf("empty registry Label = %q, want %q", got, "")
	}
	r.Mark("a1", "Examplehost (OpenCode)")
	if got := r.Label("a1", time.Minute); got != "Examplehost (OpenCode)" {
		t.Errorf("Label = %q, want Examplehost (OpenCode)", got)
	}
	if got := r.Label("a2", time.Minute); got != "" {
		t.Errorf("unmarked slot Label = %q, want empty", got)
	}
}

func TestRegistryFreshness(t *testing.T) {
	r := New()
	r.Mark("a3", "SMITH")
	if got := r.Label("a3", 120*time.Second); got != "SMITH" {
		t.Errorf("fresh Label = %q, want SMITH", got)
	}
	if got := r.Label("a3", time.Nanosecond); got != "" {
		t.Errorf("stale-by-window Label = %q, want empty", got)
	}
}

func TestRegistryLatestWriteWins(t *testing.T) {
	r := New()
	r.Mark("a1", "LibreChat")
	r.Mark("a1", "SMITH")
	if got := r.Label("a1", time.Minute); got != "SMITH" {
		t.Errorf("Label = %q, want SMITH (latest Mark wins)", got)
	}
}

func TestRegistryNilSafe(t *testing.T) {
	var r *Registry
	r.Mark("a1", "x") // must not panic
	if got := r.Label("a1", time.Minute); got != "" {
		t.Errorf("nil registry Label = %q, want empty", got)
	}
}

func TestDeriveLabel(t *testing.T) {
	cases := []struct{ key, ua, want string }{
		// UA-derived app + host segment from the operator's key name.
		{"opencode-examplehost", "opencode/1.2.3", "Examplehost (opencode)"},
		{"OPENCODE-EXAMPLEHOST", "OpenCode", "Examplehost (OpenCode)"}, // key match is case-insensitive; UA casing verbatim
		{"librechat", "librechat/2.0", "librechat"},
		{"kakehashi", "kakehashi", "kakehashi"},
		// Distinct app + raw key name.
		{"testuser-laptop", "myagent/0.9", "testuser-laptop (myagent)"},
		// No UA → raw key name; no key → app alone; neither → "".
		{"opencode-core", "", "opencode-core"},
		{"", "opencode/1.0", "opencode"},
		{"", "", ""},
		// Junk UAs yield no app token.
		{"examplehost", "123/456", "examplehost"},
	}
	for _, c := range cases {
		if got := DeriveLabel(c.key, c.ua); got != c.want {
			t.Errorf("DeriveLabel(%q, %q) = %q, want %q", c.key, c.ua, got, c.want)
		}
	}
}
