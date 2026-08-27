// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLoadStatus_IdleWhenNeverRequested(t *testing.T) {
	eng := newFakeEngine()
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))

	ls := c.LoadStatus("never-asked-for")
	if ls.State != "idle" {
		t.Fatalf("state = %q, want idle", ls.State)
	}
	if ls.Reason != "" || ls.Message != "" {
		t.Fatalf("idle state should carry no reason/message, got %+v", ls)
	}
}

func TestLoadStatus_ReflectsCurrentOccupancy(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a2", "llama")
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))

	ls := c.LoadStatus("llama")
	if ls.State != StatusLoaded || ls.Slot != "a2" {
		t.Fatalf("ls = %+v, want loaded in a2", ls)
	}
}

// TestLoadStatus_QueuedReportsRefusalReason: while EnsureLoaded is stuck
// polling because nothing is evictable yet, a concurrent LoadStatus poll
// must see the live RefusalReason instead of nothing.
func TestLoadStatus_QueuedReportsRefusalReason(t *testing.T) {
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4"})
	idle := map[string]time.Duration{"a1": time.Second, "a2": time.Second, "a3": time.Second, "a4": time.Second}
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), idle))

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.EnsureLoaded(ctx, EnsureRequest{Model: "llama", RequestedBy: "test"})
	}()

	var ls LoadState
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		ls = c.LoadStatus("llama")
		if ls.State == StatusQueued && ls.Reason != "" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	<-done

	if ls.State != StatusQueued {
		t.Fatalf("ls.State = %q, want queued while stuck; got %+v", ls.State, ls)
	}
	if ls.Reason == "" {
		t.Fatalf("ls.Reason empty, want a RefusalReason code while stuck; got %+v", ls)
	}
	if ls.QueuePosition < 1 {
		t.Fatalf("ls.QueuePosition = %d, want >= 1", ls.QueuePosition)
	}
}

// TestLoadStatus_RemembersFailureBriefly: after a terminal timeout, the
// ticket is gone from the live queue (dequeue runs on every return path),
// but a poll arriving right after must still see the failure and its
// reason via the short-lived outcome ring, not silently report "idle".
func TestLoadStatus_RemembersFailureBriefly(t *testing.T) {
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4"})
	idle := map[string]time.Duration{"a1": time.Second, "a2": time.Second, "a3": time.Second, "a4": time.Second}
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), idle))

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := c.EnsureLoaded(ctx, EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}

	if q := c.Status().Queue; len(q) != 0 {
		t.Fatalf("queue = %v, want empty (ticket dequeued on return)", q)
	}

	ls := c.LoadStatus("llama")
	if ls.State != StatusFailed {
		t.Fatalf("ls.State = %q, want failed (remembered via outcome ring), got %+v", ls.State, ls)
	}
	if ls.Message == "" {
		t.Fatalf("ls.Message empty, want the timeout detail preserved, got %+v", ls)
	}
}

// TestLoadStatus_ClearsRememberedFailureAfterLaterSuccess: a stale failure
// must not resurface once the model has since loaded successfully.
func TestLoadStatus_ClearsRememberedFailureAfterLaterSuccess(t *testing.T) {
	eng := newFakeEngine()
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))

	// Seed a remembered failure directly (cheaper than reproducing a real
	// timeout here — TestLoadStatus_RemembersFailureBriefly already covers
	// that the ring gets populated correctly from a real EnsureLoaded call).
	c.outcomes.record("llama", outcomeRecord{status: StatusFailed, message: "boom", at: time.Now()})
	if ls := c.LoadStatus("llama"); ls.State != StatusFailed {
		t.Fatalf("precondition: ls.State = %q, want failed", ls.State)
	}

	tk, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err != nil || tk.Status != StatusLoaded {
		t.Fatalf("EnsureLoaded = %+v, %v, want a clean load", tk, err)
	}

	ls := c.LoadStatus("llama")
	if ls.State != StatusLoaded {
		t.Fatalf("ls.State = %q, want loaded (stale failure must be cleared)", ls.State)
	}
}

func TestOutcomeRing_EvictsOldestBeyondCap(t *testing.T) {
	r := newOutcomeRing()
	now := time.Now()
	for i := 0; i < outcomeRingCap+5; i++ {
		r.record(modelName(i), outcomeRecord{status: StatusFailed, at: now})
	}
	if _, ok := r.get(modelName(0), now); ok {
		t.Fatalf("oldest entry should have been evicted")
	}
	if _, ok := r.get(modelName(outcomeRingCap+4), now); !ok {
		t.Fatalf("newest entry should still be present")
	}
	if len(r.entries) != outcomeRingCap {
		t.Fatalf("len(entries) = %d, want %d", len(r.entries), outcomeRingCap)
	}
}

func TestOutcomeRing_ExpiresAfterTTL(t *testing.T) {
	r := newOutcomeRing()
	now := time.Now()
	r.record("llama", outcomeRecord{status: StatusFailed, at: now})
	if _, ok := r.get("llama", now.Add(outcomeTTL-time.Second)); !ok {
		t.Fatalf("entry should still be live just under the TTL")
	}
	if _, ok := r.get("llama", now.Add(outcomeTTL+time.Second)); ok {
		t.Fatalf("entry should have expired past the TTL")
	}
}

func modelName(i int) string {
	return "model-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}
