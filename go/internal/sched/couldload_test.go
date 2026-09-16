// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/engine"
)

// TestCouldLoadAlreadyLoadedFastPath mirrors EnsureLoaded's own fast path
// (core.go's findLoaded check at the top of EnsureLoaded) — CouldLoad must
// answer the same way without engaging place()/FitPlan at all.
func TestCouldLoadAlreadyLoadedFastPath(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a1", "gemma")
	c := placeCore(t, eng, eng.Slots(), nil)

	pl, err := c.CouldLoad(context.Background(), "gemma", 30*time.Second)
	if err != nil {
		t.Fatalf("CouldLoad: %v", err)
	}
	if pl.Slot != "a1" || pl.Terminal {
		t.Fatalf("CouldLoad = %+v, want already-loaded on a1, non-terminal", pl)
	}
}

func TestCouldLoadRequiresModel(t *testing.T) {
	eng := newFakeEngine()
	c := placeCore(t, eng, eng.Slots(), nil)

	if _, err := c.CouldLoad(context.Background(), "", 30*time.Second); err == nil {
		t.Fatal("CouldLoad(\"\") = nil error, want an error")
	}
}

// TestCouldLoadAgreesWithPlace is the property the whole feature depends on
// (see CouldLoad's doc comment: never write a second fit estimator) — for
// identical inputs, CouldLoad's export of place()'s verdict must carry
// exactly the same slot/evict/terminal/reason as calling place() directly.
func TestCouldLoadAgreesWithPlace(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a1", "gemma")
	c := placeCore(t, eng, eng.Slots(), nil)

	want := c.place(context.Background(), "llama", "", c.Config(), nil, 30*time.Second)
	got, err := c.CouldLoad(context.Background(), "llama", 30*time.Second)
	if err != nil {
		t.Fatalf("CouldLoad: %v", err)
	}
	if got.Slot != want.slot || got.Terminal != want.terminal || got.Reason != want.reason ||
		len(got.Evict) != len(want.evict) || got.EvictComfy != want.evictComfy {
		t.Fatalf("CouldLoad = %+v, place() = %+v — must agree", got, want)
	}
}

// TestCouldLoadTerminalVsRetryable exercises the horizon-sensitivity
// CouldLoad's doc comment calls out: the same blocked placement is
// retryable ("wait") under a horizon long enough for the idle threshold to
// clear, and terminal ("never, within this budget") under one too short —
// a caller must read Terminal, not Slot=="" alone. The horizon-aware branch
// (idleHorizonRefusal) only fires for the memory-driven-eviction path
// (plan.Fits=false naming a specific candidate), so the fixture forces that
// path rather than the free-choice normal tier, which is always retryable
// regardless of horizon (see TestPlaceReservationProtection's "no idle,
// unreserved slot" cases for that unconditional-retry behavior).
func TestCouldLoadTerminalVsRetryable(t *testing.T) {
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4"})
	// a2 is 170s idle — 10s short of the 180s default IdleUnloadS threshold.
	idle := map[string]time.Duration{
		"a1": 10 * time.Second, "a2": 170 * time.Second,
		"a3": 10 * time.Second, "a4": 20 * time.Second,
	}
	eng.plan = engine.Plan{Fits: false, Evict: []string{"a2"}, NeedBytes: 90000 * 1024 * 1024, FreeBytes: 10000 * 1024 * 1024,
		Message: "Needs [a2] evicted to fit"}
	c := placeCore(t, eng, eng.Slots(), idle)

	// Horizon too short for a2's remaining 10s countdown to clear: terminal.
	short, err := c.CouldLoad(context.Background(), "llama", 5*time.Second)
	if err != nil {
		t.Fatalf("CouldLoad (short horizon): %v", err)
	}
	if !short.Terminal || short.Reason != ReasonIdleThresholdNotMet {
		t.Fatalf("CouldLoad (short horizon) = %+v, want terminal idle_threshold_not_met", short)
	}

	// Horizon long enough for a2 to cross the threshold: retryable, not terminal.
	long, err := c.CouldLoad(context.Background(), "llama", 30*time.Second)
	if err != nil {
		t.Fatalf("CouldLoad (long horizon): %v", err)
	}
	if long.Terminal {
		t.Fatalf("CouldLoad (long horizon) = %+v, want non-terminal (retryable)", long)
	}
}

// TestCouldLoadNeverMutates confirms CouldLoad's central promise — calling
// it repeatedly must never change engine occupancy, unlike EnsureLoaded.
func TestCouldLoadNeverMutates(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a1", "gemma")
	c := placeCore(t, eng, eng.Slots(), nil)

	for range 5 {
		if _, err := c.CouldLoad(context.Background(), "llama", 30*time.Second); err != nil {
			t.Fatalf("CouldLoad: %v", err)
		}
	}
	if len(eng.loadCalls) != 0 || len(eng.unloadCalls) != 0 {
		t.Fatalf("CouldLoad mutated the engine: loads=%v unloads=%v", eng.loadCalls, eng.unloadCalls)
	}
}
