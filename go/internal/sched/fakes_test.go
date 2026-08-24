// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
)

// fakeEngine is a concurrency-safe Engine fake: occupancy is a plain map,
// Load/Unload mutate it and record calls, FitPlan returns a canned plan.
type fakeEngine struct {
	mu          sync.Mutex
	slots       []string
	occ         map[string]collector.SlotAssignment
	plan        engine.Plan
	planErr     error
	planFn      func(model string) (engine.Plan, error)
	loadFn      func(mode, slot string) engine.Result
	unloadFn    func(slot string) engine.Result
	loadDelay   time.Duration
	loadCalls   []string
	unloadCalls []string
	inFlight    int
	maxInFlight int
}

func newFakeEngine(slots ...string) *fakeEngine {
	if len(slots) == 0 {
		slots = []string{"a1", "a2", "a3", "a4"}
	}
	occ := map[string]collector.SlotAssignment{}
	for _, s := range slots {
		occ[s] = collector.SlotAssignment{}
	}
	return &fakeEngine{
		slots: slots,
		occ:   occ,
		plan:  engine.Plan{Fits: true, Message: "Fits now"},
	}
}

func (f *fakeEngine) setOcc(slot, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.occ[slot] = collector.SlotAssignment{Mode: mode}
}

func (f *fakeEngine) Slots() []string { return f.slots }

func (f *fakeEngine) SlotStates(map[string]collector.UnitState) map[string]collector.SlotAssignment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]collector.SlotAssignment{}
	for k, v := range f.occ {
		out[k] = v
	}
	return out
}

func (f *fakeEngine) FitPlan(model string) (engine.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.planFn != nil {
		return f.planFn(model)
	}
	return f.plan, f.planErr
}

func (f *fakeEngine) Load(_ context.Context, mode, slot string) engine.Result {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	delay := f.loadDelay
	fn := f.loadFn
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	res := engine.Result{Success: true, Message: "loaded"}
	if fn != nil {
		res = fn(mode, slot)
	}

	f.mu.Lock()
	f.inFlight--
	f.loadCalls = append(f.loadCalls, mode+"->"+slot)
	if res.Success {
		f.occ[slot] = collector.SlotAssignment{Mode: mode}
	}
	f.mu.Unlock()
	return res
}

func (f *fakeEngine) Unload(_ context.Context, slot string) engine.Result {
	f.mu.Lock()
	fn := f.unloadFn
	f.mu.Unlock()
	res := engine.Result{Success: true, Message: "unloaded"}
	if fn != nil {
		res = fn(slot)
	}
	f.mu.Lock()
	f.unloadCalls = append(f.unloadCalls, slot)
	if res.Success {
		if slot == "all" {
			for _, s := range f.slots {
				f.occ[s] = collector.SlotAssignment{}
			}
		} else {
			f.occ[slot] = collector.SlotAssignment{}
		}
	}
	f.mu.Unlock()
	return res
}

func (f *fakeEngine) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.loadCalls)
}

func (f *fakeEngine) snapshotCalls() (loads, unloads []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.loadCalls...), append([]string(nil), f.unloadCalls...)
}

// fakeEngineFP adds per-slot footprints (the optional Footprints seam).
type fakeEngineFP struct {
	*fakeEngine
	footprints map[string]float64
}

func (f *fakeEngineFP) SlotFootprintMB(slot string) float64 { return f.footprints[slot] }

// staticSource builds a collector.Static whose slots carry LastActivity =
// now - idleFor[slot] (slots absent from idleFor get zero LastActivity =
// unknown activity), plus optional metrics for budget tests.
func staticSource(now time.Time, slots []string, idleFor map[string]time.Duration) *collector.Static {
	snap := &collector.Snapshot{
		TakenAt:        now,
		Units:          map[string]collector.UnitState{},
		Slots:          map[string]collector.SlotState{},
		Inference:      map[string]collector.SlotInference{},
		Ports:          map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}
	labels := map[string]string{"a1": "A1", "a2": "A2", "a3": "A3", "a4": "A4"}
	for _, s := range slots {
		st := collector.SlotState{Slot: s, Label: labels[s]}
		if d, ok := idleFor[s]; ok {
			st.LastActivity = now.Add(-d)
		}
		snap.Slots[s] = st
	}
	return collector.NewStatic(snap)
}

// newTestCore builds a Core with fast polling and a short default timeout.
func newTestCore(t *testing.T, eng Engine, src collector.Source, opts ...func(*Deps)) *Core {
	t.Helper()
	deps := Deps{
		Engine:         eng,
		Source:         src,
		PollInterval:   time.Millisecond,
		DefaultTimeout: 250 * time.Millisecond,
		Logf:           t.Logf,
	}
	for _, o := range opts {
		o(&deps)
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// mustReservation is a valid one-hour reservation active around now.
func mustReservation(label, model, scope, bay, createdBy string, start, end time.Time) Reservation {
	return Reservation{
		Label:     label,
		Model:     model,
		Start:     start,
		End:       end,
		Scope:     scope,
		Bay:       bay,
		CreatedBy: createdBy,
	}
}
