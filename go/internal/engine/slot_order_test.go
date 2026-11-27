// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestLoadOrderPassesOverFreedSlots(t *testing.T) {
	m, _, _ := newTestManager(t, testConfig(t), newFakeSys(), nil)
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	m.d.Now = func() time.Time { return now }
	order := func(want ...string) {
		t.Helper()
		if got := m.LoadOrder(); !slices.Equal(got, want) {
			t.Fatalf("LoadOrder = %v, want %v", got, want)
		}
	}
	unload := func(slot, held string) {
		t.Helper()
		m.setSlotMode(slot, held)
		if res := m.Unload(context.Background(), slot); !res.Success {
			t.Fatalf("unload %s: %s", slot, res.Message)
		}
	}

	order("a1", "a2", "a3", "a4")
	unload("a1", "qwen")
	order("a2", "a3", "a4", "a1")
	now = now.Add(5 * time.Second)
	unload("a2", "gemma")
	order("a3", "a4", "a1", "a2") // resting slots: the one freed longest ago first
	unload("a3", "")              // nothing was there: not freed
	order("a3", "a4", "a1", "a2")
	if m.RecentlyFreed("a3") || !m.RecentlyFreed("a1") {
		t.Fatal("RecentlyFreed is wrong")
	}
	now = now.Add(SlotRest - 3*time.Second) // a1 rested, a2 not yet
	order("a1", "a3", "a4", "a2")
	now = now.Add(time.Minute)
	order("a1", "a2", "a3", "a4")
}
