// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// TestRunSlotStateSyncClearsStaleRow reproduces the bug found 2026-07-29:
// killLingering's (now-fixed) cross-slot collateral kill left slot_state
// rows stuck showing a mode that hadn't actually been running for hours.
// RunSlotStateSync must clear a recorded slot the moment live reality says
// it's empty.
func TestRunSlotStateSyncClearsStaleRow(t *testing.T) {
	db := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched := db.Sched()
	if err := sched.SaveSlot(ctx, "a3", "laguna-s-21-q4_k_m", time.Now()); err != nil {
		t.Fatalf("SaveSlot: %v", err)
	}
	if err := sched.SaveSlot(ctx, "a1", "gemma4-e2b", time.Now()); err != nil {
		t.Fatalf("SaveSlot: %v", err)
	}

	// Live reality: a3 died outside any tracked call; a1 is still
	// genuinely loaded and must be left alone (loaded_at untouched).
	live := func() map[string]string {
		return map[string]string{"a3": "", "a1": "gemma4-e2b"}
	}

	tick := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		RunSlotStateSync(ctx, tick, sched, live, nil)
		close(done)
	}()

	tick <- time.Now() // fire one sync pass

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := sched.Slots(ctx)
		if err != nil {
			t.Fatalf("Slots: %v", err)
		}
		if got["a3"] == "" && got["a1"] == "gemma4-e2b" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync did not clear stale row within deadline; got=%v", got)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	<-done
}

// TestRunSlotStateSyncNeverInventsALoadedRow checks the deliberately
// one-directional behavior: a slot the table has never heard of (or
// already shows empty) is left alone even if liveSlots reports it loaded —
// that direction self-heals via the engine's own startup reconciliation,
// not this loop.
func TestRunSlotStateSyncNeverInventsALoadedRow(t *testing.T) {
	db := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched := db.Sched()
	live := func() map[string]string {
		return map[string]string{"a4": "qwen25coder"} // table has never heard of a4
	}

	tick := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		RunSlotStateSync(ctx, tick, sched, live, nil)
		close(done)
	}()

	tick <- time.Now()
	// No deadline-loop assertion of absence is reliable, so give the sync
	// goroutine a generous moment to (not) act, then check once.
	time.Sleep(50 * time.Millisecond)

	got, err := sched.Slots(ctx)
	if err != nil {
		t.Fatalf("Slots: %v", err)
	}
	if got["a4"] != "" {
		t.Errorf("a4 = %q, want untouched/empty — sync must never invent a loaded row", got["a4"])
	}

	cancel()
	<-done
}
