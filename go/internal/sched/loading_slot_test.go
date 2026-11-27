package sched

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A request that arrives while the model is still loading must wait for that load. It used to be told the model was loaded, because the
// engine reports the model's name for the slot during the load, and the router then sent it to a port that was not listening yet
// (2026-11-26: two of three concurrent requests failed about 2 s into a 33 s load).
func TestEnsureLoadedWaitsForALoadInProgress(t *testing.T) {
	eng := newFakeEngine()
	eng.loadDelay = 150 * time.Millisecond
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.DefaultTimeout = 5 * time.Second
	})

	first := make(chan error, 1)
	go func() {
		_, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "first"})
		first <- err
	}()
	// Wait until the engine is inside the load, as the second and third callers were.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		if a := eng.SlotStates(nil)["a1"]; a.Loading != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first load never started")
		}
	}
	if st := c.LoadStatus("llama"); st.State == StatusLoaded {
		t.Fatalf("LoadStatus during the load = %+v, want not loaded", st)
	}
	tk, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "second"})
	if err != nil || tk.Status != StatusLoaded {
		t.Fatalf("second caller: ticket = %+v, err = %v", tk, err)
	}
	if !eng.isReady("llama") {
		t.Fatal("the second caller was told the model is loaded before the load finished")
	}
	if err := <-first; err != nil {
		t.Fatalf("first caller: %v", err)
	}
	if n := eng.loadCount(); n != 1 {
		t.Fatalf("engine.Load called %d times, want exactly 1", n)
	}
}

// A load the scheduler did not start (the dashboard, MCP or CLI) is also waited for: no second load, no early "loaded".
func TestEnsureLoadedWaitsForALoadItDidNotStart(t *testing.T) {
	eng := newFakeEngine()
	eng.setLoading("a2", "llama")
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.DefaultTimeout = 60 * time.Millisecond
	})

	if st := c.LoadStatus("llama"); st.State != StatusLoading || st.Slot != "a2" {
		t.Fatalf("LoadStatus = %+v, want loading on a2", st)
	}
	_, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err == nil || !strings.Contains(err.Error(), "already loading on a2") {
		t.Fatalf("err = %v, want a timeout naming the load in progress", err)
	}
	if n := eng.loadCount(); n != 0 {
		t.Fatalf("engine.Load called %d times, want 0", n)
	}

	eng.setOcc("a2", "llama")
	tk, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err != nil || tk.Status != StatusLoaded || tk.TargetSlot != "a2" {
		t.Fatalf("after the load finished: ticket = %+v, err = %v", tk, err)
	}
}
