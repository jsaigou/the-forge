// SPDX-License-Identifier: Apache-1.0

package sched

// ComfyUI eviction gating + execution (S1, feedback F3): the engine's plan
// only PROPOSES stopping ComfyUI; the scheduler gates on config opt-out,
// comfyui reservations, and queue idleness, then executes via the seam.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/engine"
)

// comfyPlan builds a fake-engine plan where slots alone cannot free enough
// but stopping ComfyUI closes the gap.
func comfyPlan(fits bool) engine.Plan {
	return engine.Plan{
		Fits:         fits,
		NeedBytes:    50 << 30,
		FreeBytes:    10 << 30,
		EvictComfyUI: true,
		ComfyUIBytes: 45 << 30,
		Message:      "Fits after ComfyUI is stopped",
	}
}

// comfyCore wires a Core with a comfyPlan and a recording ComfyUI seam.
func comfyCore(t *testing.T, plan engine.Plan, idle bool) (*Core, *fakeEngine, *bool) {
	t.Helper()
	eng := newFakeEngine()
	eng.planFn = func(string) (engine.Plan, error) { return plan, nil }
	stopped := false
	c := newTestCore(t, eng, staticSource(testNow, eng.Slots(), nil), func(d *Deps) {
		d.Now = func() time.Time { return testNow }
		d.ComfyUI = &ComfyUISeam{
			Unit: func() string { return "ai-mode-comfyui-test" },
			Idle: func(context.Context) (bool, string) {
				if idle {
					return true, ""
				}
				return false, "a workflow is queued or running"
			},
			Stop: func(context.Context) error { stopped = true; return nil },
		}
	})
	return c, eng, &stopped
}

func TestPlaceStopsIdleComfyUIForMemory(t *testing.T) {
	c, _, stopped := comfyCore(t, comfyPlan(true), true)

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if pl.slot == "" || !pl.evictComfy {
		t.Fatalf("place = %+v, want a placement that stops ComfyUI", pl)
	}
	if pl.terminal {
		t.Fatalf("place = %+v, want it actionable", pl)
	}
	_ = stopped // execution asserted in TestAttemptLoadStopsComfyUI
}

func TestAttemptLoadStopsComfyUIThenLoads(t *testing.T) {
	c, eng, stopped := comfyCore(t, comfyPlan(false), true)

	out := c.attemptLoad(context.Background(), EnsureRequest{Model: "llama"}, DefaultConfig(), nil, 30*time.Second)
	if !out.success || out.slot == "" {
		t.Fatalf("attemptLoad = %+v, want success", out)
	}
	if !*stopped {
		t.Fatal("ComfyUI.Stop was never called")
	}
	if len(eng.loadCalls) != 1 {
		t.Fatalf("load calls = %v, want exactly one load after the stop", eng.loadCalls)
	}
}

func TestPlaceComfyBusyQueueIsRetryable(t *testing.T) {
	c, _, _ := comfyCore(t, comfyPlan(true), false)

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if pl.slot != "" || pl.terminal {
		t.Fatalf("place = %+v, want retryable while a workflow runs", pl)
	}
	if !strings.Contains(pl.message, "not enough VRAM") || !strings.Contains(pl.message, "not idle") {
		t.Errorf("message = %q, want VRAM-explicit busy reason", pl.message)
	}
}

func TestPlaceComfyOptOutIsTerminalWithReason(t *testing.T) {
	c, _, stopped := comfyCore(t, comfyPlan(true), true)

	off := false
	cfg := DefaultConfig()
	cfg.ComfyUIEvictable = &off

	pl := c.place(context.Background(), "llama", "", cfg, nil, 30*time.Second)
	if !pl.terminal || !strings.Contains(pl.message, "not enough VRAM") || !strings.Contains(pl.message, "disabled") {
		t.Fatalf("place = %+v, want terminal opt-out refusal", pl)
	}
	if *stopped {
		t.Fatal("ComfyUI must not be stopped while evictions are opted out")
	}
}

func TestPlaceComfyActiveReservationBlocks(t *testing.T) {
	reservations := []Reservation{mustReservation("comfy-res", "anything", "comfyui", "", HumanIdentity,
		testNow.Add(-time.Minute), testNow.Add(time.Hour))}
	c, _, stopped := comfyCore(t, comfyPlan(true), true)

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), reservations, 30*time.Second)
	// The reservation ends in an hour — far beyond the 30s horizon.
	if !pl.terminal || !strings.Contains(pl.message, "reservation") {
		t.Fatalf("place = %+v, want terminal reservation refusal", pl)
	}
	if *stopped {
		t.Fatal("ComfyUI must not be stopped under an active comfyui reservation")
	}
}

func TestPlaceComfyWithoutSeamIsTerminal(t *testing.T) {
	eng := newFakeEngine()
	eng.planFn = func(string) (engine.Plan, error) { return comfyPlan(true), nil }
	c := newTestCore(t, eng, staticSource(testNow, eng.Slots(), nil), func(d *Deps) {
		d.Now = func() time.Time { return testNow }
		d.ComfyUI = nil
	})

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if !pl.terminal || !strings.Contains(pl.message, "not enough VRAM") {
		t.Fatalf("place = %+v, want terminal no-ComfyUI-configured refusal", pl)
	}
}

// The S1 headline behavior at the EnsureLoaded boundary: a structural
// refusal returns immediately with its reason instead of burning the whole
// wait budget in silence.
func TestEnsureLoadedStructuralRefusalFailsFast(t *testing.T) {
	eng := newFakeEngine()
	eng.planFn = func(string) (engine.Plan, error) { return comfyPlan(true), nil }
	c := newTestCore(t, eng, staticSource(testNow, eng.Slots(), nil), func(d *Deps) {
		d.Now = func() time.Time { return testNow }
		d.DefaultTimeout = 30 * time.Second
		d.ComfyUI = nil // structural: nothing can act on the proposal
	})

	start := time.Now()
	_, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "test"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("EnsureLoaded succeeded, want structural refusal")
	}
	if !strings.Contains(err.Error(), "not enough VRAM") {
		t.Errorf("error = %v, want the memory-explicit reason", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %s to fail a structurally-impossible load, want immediate", elapsed)
	}
}
