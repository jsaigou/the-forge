// SPDX-License-Identifier: Apache-2.0

package sched

// Regression for the 2026-08-22 incident class: a load that polls out its
// whole deadline against retryable refusals must surface WHY it never
// proceeded, not just "timed out".

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEnsureLoadedTimeoutCarriesLastBlocker(t *testing.T) {
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{
		"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
	})
	idle := map[string]time.Duration{
		"a1": time.Second, "a2": time.Second,
		"a3": time.Second, "a4": time.Second,
	}
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), idle))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := c.EnsureLoaded(ctx, EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if !strings.Contains(err.Error(), "(last blocker:") {
		t.Fatalf("err = %v, want the last retryable refusal surfaced", err)
	}
	if eng.loadCount() != 0 {
		t.Fatalf("engine.Load called %d times, want 0", eng.loadCount())
	}
}
