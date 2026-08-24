// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
)

// fakeFullEngine embeds engine.Stub (satisfies engine.Engine) and adds the
// two extra concrete methods engineFull needs — the same shape
// *engine.Manager has in production.
type fakeFullEngine struct {
	*engine.Stub
	loadCalls, unloadCalls, switchCalls, restartCalls, startCalls, stopCalls int
}

func newFakeFullEngine() *fakeFullEngine {
	return &fakeFullEngine{Stub: &engine.Stub{}}
}

func (f *fakeFullEngine) FitPlan(mode string) (engine.Plan, error) { return engine.Plan{}, nil }

func (f *fakeFullEngine) SlotStates(units map[string]collector.UnitState) map[string]collector.SlotAssignment {
	return nil
}

func (f *fakeFullEngine) Load(ctx context.Context, mode, slot string) engine.Result {
	f.loadCalls++
	return f.Stub.Load(ctx, mode, slot)
}

func (f *fakeFullEngine) Unload(ctx context.Context, slot string) engine.Result {
	f.unloadCalls++
	return f.Stub.Unload(ctx, slot)
}

func (f *fakeFullEngine) SwitchMode(ctx context.Context, mode string) engine.Result {
	f.switchCalls++
	return f.Stub.SwitchMode(ctx, mode)
}

func (f *fakeFullEngine) Restart(ctx context.Context) engine.Result {
	f.restartCalls++
	return f.Stub.Restart(ctx)
}

func (f *fakeFullEngine) StartUnit(ctx context.Context, unit string) error {
	f.startCalls++
	return f.Stub.StartUnit(ctx, unit)
}

func (f *fakeFullEngine) StopUnit(ctx context.Context, unit string) error {
	f.stopCalls++
	return f.Stub.StopUnit(ctx, unit)
}

var _ engineFull = (*fakeFullEngine)(nil)

func TestGatedEngine_BlocksEveryMutationWhileActive(t *testing.T) {
	real := newFakeFullEngine()
	g := New(newFakeSettings(), nil, time.Now, nil)
	if _, err := g.Enter(EnterRequest{Reason: "test", Duration: time.Hour}); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	wrapped := WrapEngine(g, real)
	ctx := context.Background()

	if res := wrapped.Load(ctx, "mode", "a1"); res.Success {
		t.Fatal("Load should be refused during a maintenance window")
	}
	if res := wrapped.Unload(ctx, "a1"); res.Success {
		t.Fatal("Unload should be refused during a maintenance window")
	}
	if res := wrapped.SwitchMode(ctx, "mode"); res.Success {
		t.Fatal("SwitchMode should be refused during a maintenance window")
	}
	if res := wrapped.Restart(ctx); res.Success {
		t.Fatal("Restart should be refused during a maintenance window")
	}
	if err := wrapped.StartUnit(ctx, "forge-stt"); err == nil {
		t.Fatal("StartUnit should be refused during a maintenance window")
	}
	if err := wrapped.StopUnit(ctx, "forge-stt"); err == nil {
		t.Fatal("StopUnit should be refused during a maintenance window")
	}

	if real.loadCalls != 0 || real.unloadCalls != 0 || real.switchCalls != 0 ||
		real.restartCalls != 0 || real.startCalls != 0 || real.stopCalls != 0 {
		t.Fatalf("no real mutation should have reached the underlying engine: %+v", real)
	}
}

func TestGatedEngine_AllowsMutationsWhenInactive(t *testing.T) {
	real := newFakeFullEngine()
	g := New(newFakeSettings(), nil, time.Now, nil)
	wrapped := WrapEngine(g, real)
	ctx := context.Background()

	if res := wrapped.Load(ctx, "mode", "a1"); !res.Success {
		t.Fatalf("Load should succeed with no active window: %+v", res)
	}
	if res := wrapped.Unload(ctx, "a1"); !res.Success {
		t.Fatalf("Unload should succeed with no active window: %+v", res)
	}
	if real.loadCalls != 1 || real.unloadCalls != 1 {
		t.Fatalf("expected the real engine to be called: %+v", real)
	}
}

func TestGatedEngine_LeaseHolderBypassesBlock(t *testing.T) {
	real := newFakeFullEngine()
	g := New(newFakeSettings(), nil, time.Now, nil)
	st, err := g.Enter(EnterRequest{Reason: "procedure step", Duration: time.Hour})
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	wrapped := WrapEngine(g, real)

	leased := WithLease(context.Background(), st.LeaseID)
	if res := wrapped.Load(leased, "mode", "a1"); !res.Success {
		t.Fatalf("the lease-holding caller should be allowed to Load: %+v", res)
	}
	if real.loadCalls != 1 {
		t.Fatalf("expected the real engine to receive the leased Load call, got %d calls", real.loadCalls)
	}

	if res := wrapped.Load(context.Background(), "mode", "a1"); res.Success {
		t.Fatal("an unrelated caller must still be blocked during the same window")
	}
}

func TestGatedEngine_ReadsPassThroughUnconditionally(t *testing.T) {
	real := newFakeFullEngine()
	real.Mode = "qwen38-27b"
	g := New(newFakeSettings(), nil, time.Now, nil)
	if _, err := g.Enter(EnterRequest{Reason: "r", Duration: time.Hour}); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	wrapped := WrapEngine(g, real)

	if got := wrapped.CurrentMode(); got != "qwen38-27b" {
		t.Fatalf("CurrentMode should pass through even during maintenance, got %q", got)
	}
	if _, err := wrapped.CanFit("anything"); err != nil {
		t.Fatalf("CanFit should pass through: %v", err)
	}
	if _, err := wrapped.MemoryBudget(); err != nil {
		t.Fatalf("MemoryBudget should pass through: %v", err)
	}
	if _, err := wrapped.FitPlan("anything"); err != nil {
		t.Fatalf("FitPlan should pass through: %v", err)
	}
	_ = wrapped.SlotStates(nil)
	_ = wrapped.Slots()
}

func TestWrapEngine_NilGatePassesThroughUnwrapped(t *testing.T) {
	real := newFakeFullEngine()
	wrapped := WrapEngine(nil, real)
	if wrapped != engineFull(real) {
		t.Fatal("WrapEngine(nil, real) should return real unchanged")
	}
}
