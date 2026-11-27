package sched

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/engine"
)

// The eviction plan rests on an estimate of what each slot gives back. The load is admitted on a fresh measurement: if the memory did not come
// back, nothing is loaded on top of what is still held.
func TestNoLoadWhenEvictionFreesLessThanEstimated(t *testing.T) {
	eng := newFakeEngine("a1", "a2")
	eng.setOcc("a1", "old")
	eng.plan = engine.Plan{Evict: []string{"a1"}, Slot: "a1", Message: "Needs [a1] evicted to fit"}
	eng.memoryStaysHeld = true
	idle := map[string]time.Duration{"a1": time.Hour}
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), idle), func(d *Deps) {
		d.DefaultTimeout = 80 * time.Millisecond
	})
	_, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "big", RequestedBy: "test"})
	if err == nil || !strings.Contains(err.Error(), "still short of memory") {
		t.Fatalf("err = %v, want a timeout naming the memory still held", err)
	}
	if n := eng.loadCount(); n != 0 {
		t.Fatalf("engine.Load called %d times, want 0", n)
	}
}
