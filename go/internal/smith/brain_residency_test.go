// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/sched"
)

// ensureLoadedStub records EnsureLoaded calls and, on success, makes a
// subsequent Status() report the model loaded on TargetSlotToReport — never
// a1 or any other literal name the test hardcodes as "the" slot, proving
// ensureBrainLoaded doesn't care which slot the scheduler actually picked.
// maybeEnsureBrainResident fires ensureBrainLoaded in its own goroutine
// (production behavior, brain_residency.go), so every mutable field here is
// guarded by mu — the tests read/reset them from the test goroutine while a
// background call may still be in flight.
type ensureLoadedStub struct {
	*sched.Stub
	TargetSlotToReport string
	Err                error

	mu          sync.Mutex
	calls       int
	loadedSlot  string
	loadedModel string
}

func (e *ensureLoadedStub) EnsureLoaded(_ context.Context, req sched.EnsureRequest) (sched.Ticket, error) {
	if req.TargetSlot != "" {
		panic("ensureBrainLoaded must never pin a TargetSlot")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.Err != nil {
		return sched.Ticket{}, e.Err
	}
	e.loadedSlot = e.TargetSlotToReport
	e.loadedModel = req.Model
	return sched.Ticket{TicketID: "t", Model: req.Model, TargetSlot: e.loadedSlot, Status: "loaded"}, nil
}

func (e *ensureLoadedStub) Status() sched.Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadedModel == "" {
		return sched.Status{Slots: map[string]string{}, SlotLabels: map[string]string{}}
	}
	return sched.Status{
		Slots:      map[string]string{e.loadedSlot: e.loadedModel},
		SlotLabels: map[string]string{e.loadedSlot: e.loadedSlot},
	}
}

// callCount/setLoaded/resetLoaded give tests race-free access to the stub's
// state — never read/write the raw fields directly from a test.
func (e *ensureLoadedStub) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *ensureLoadedStub) resetLoaded() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.loadedModel = ""
	e.loadedSlot = ""
}

func newEnsureLoadedStubResident(slot, model string) *ensureLoadedStub {
	e := &ensureLoadedStub{Stub: &sched.Stub{}}
	e.loadedSlot, e.loadedModel = slot, model
	return e
}

func TestEnsureBrainLoaded_SuccessResolvesOnSlotScheduerPicked(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db) // local config "ornith-35b"
	setSetting(t, db, SettingModel, `"ornith-35b"`)

	stub := &ensureLoadedStub{Stub: &sched.Stub{}, TargetSlotToReport: "a3"}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	before := s.Brain(ctx)
	if before.Resolution != BrainDeterministicOnly {
		t.Fatalf("precondition: Brain() = %q, want deterministic_only (configured but not loaded)", before.Resolution)
	}

	after := s.ensureBrainLoaded(ctx)
	if stub.callCount() != 1 {
		t.Fatalf("EnsureLoaded calls = %d, want 1", stub.callCount())
	}
	if after.Resolution != BrainLocalSlot {
		t.Fatalf("after ensureBrainLoaded, Brain() = %q, want local_slot", after.Resolution)
	}
	if after.Slot != "a3" {
		t.Errorf("resolved slot = %q, want %q (whatever the scheduler picked, not a hardcoded name)", after.Slot, "a3")
	}

	status := s.brainResidencyStatus(ctx)
	if status.LastAttemptAt == nil || !status.LastLoaded || status.LastSlot != "a3" {
		t.Errorf("brainResidencyStatus = %+v, want a recorded successful attempt on slot a3", status)
	}
}

// staleStatusStub succeeds EnsureLoaded but its Status() never reflects the
// new load — modeling sched.Status()'s real, documented staleness
// (sched/core.go's Status() is deliberately snapshot-based, "design
// decision 2", lagging real state by up to one collector cycle). Found
// live on ForgeHost: ensureBrainLoaded's first implementation re-resolved via
// Brain() (which reads Status()) immediately after a successful
// EnsureLoaded and could still see deterministic_only, silently failing
// to escalate a turn whose brain had, in reality, already loaded.
type staleStatusStub struct {
	*sched.Stub
	calls int
}

func (e *staleStatusStub) EnsureLoaded(_ context.Context, req sched.EnsureRequest) (sched.Ticket, error) {
	e.calls++
	return sched.Ticket{TicketID: "t", Model: req.Model, TargetSlot: "a2", Status: "loaded"}, nil
}

func (e *staleStatusStub) Status() sched.Status {
	// Deliberately never reports the model loaded — simulates the
	// collector snapshot never catching up within this call.
	return sched.Status{Slots: map[string]string{}, SlotLabels: map[string]string{}}
}

func TestEnsureBrainLoaded_TrustsTicketOverStaleStatus(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)

	stub := &staleStatusStub{Stub: &sched.Stub{}}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	after := s.ensureBrainLoaded(ctx)
	if stub.calls != 1 {
		t.Fatalf("EnsureLoaded calls = %d, want 1", stub.calls)
	}
	if after.Resolution != BrainLocalSlot {
		t.Fatalf("Resolution = %q, want local_slot — a successful EnsureLoaded must not be undone by a stale Status() read", after.Resolution)
	}
	if after.Slot != "a2" {
		t.Errorf("Slot = %q, want %q (from the ticket, not Status())", after.Slot, "a2")
	}
}

func TestEnsureBrainLoaded_ErrorGracefullyFallsBackToDeterministic(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)

	stub := &ensureLoadedStub{Stub: &sched.Stub{}, Err: errors.New("no idle slot available")}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	after := s.ensureBrainLoaded(ctx)
	if after.Resolution != BrainDeterministicOnly {
		t.Errorf("Resolution = %q, want deterministic_only (graceful fallback on load failure)", after.Resolution)
	}
	status := s.brainResidencyStatus(ctx)
	if status.LastAttemptAt == nil || status.LastLoaded || status.LastError == "" {
		t.Errorf("brainResidencyStatus = %+v, want a recorded failed attempt with an error", status)
	}
}

func TestEnsureBrainLoaded_NoOpWhenAlreadyLoaded(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)

	stub := &ensureLoadedStub{Stub: &sched.Stub{}}
	sc := newStubSched(map[string]string{"a2": "ornith-35b"}) // already resident, on a2 — not a1
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: sc})
	ctx := context.Background()

	before := s.Brain(ctx)
	if before.Resolution != BrainLocalSlot || before.Slot != "a2" {
		t.Fatalf("precondition: Brain() = %+v, want local_slot on a2", before)
	}

	after := s.ensureBrainLoaded(ctx)
	if stub.callCount() != 0 {
		t.Errorf("EnsureLoaded should not be called when already resolvable, got %d calls", stub.callCount())
	}
	if after != before {
		t.Errorf("ensureBrainLoaded should return the unchanged resolution, got %+v want %+v", after, before)
	}
}

func TestEnsureBrainLoaded_NoOpWhenNotALocalConfig(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)                            // includes remote offering wire_model "deepseek-chat"
	setSetting(t, db, SettingModel, `"deepseek-chat"`) // remote, not local
	stub := &ensureLoadedStub{Stub: &sched.Stub{}}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	before := s.Brain(ctx)
	if before.Resolution != BrainRemote {
		t.Fatalf("precondition: Brain() = %q, want remote", before.Resolution)
	}
	after := s.ensureBrainLoaded(ctx)
	if stub.callCount() != 0 {
		t.Errorf("EnsureLoaded should not be called for a remote-offering brain, got %d calls", stub.callCount())
	}
	if after != before {
		t.Errorf("ensureBrainLoaded should return the unchanged resolution for a remote brain")
	}
}

func TestMaybeEnsureBrainResident_RespectsStayResidentSetting(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	// smith.brain_residency left unset — DefaultBrainResidency().StayResident == false

	stub := &ensureLoadedStub{Stub: &sched.Stub{}, TargetSlotToReport: "a4"}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	s.maybeEnsureBrainResident(ctx, time.Now())
	if stub.callCount() != 0 {
		t.Errorf("stay_resident defaults false — maybeEnsureBrainResident must not load anything, got %d calls", stub.callCount())
	}
}

func TestMaybeEnsureBrainResident_LoadsWhenStayResidentEnabled(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	setSetting(t, db, SettingBrainResidency, `{"stay_resident":true}`)

	stub := &ensureLoadedStub{Stub: &sched.Stub{}, TargetSlotToReport: "a4"}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	s.maybeEnsureBrainResident(ctx, time.Now())
	// ensureBrainLoaded runs in a goroutine — wait for it.
	waitFor(t, time.Second, func() bool { return stub.callCount() == 1 })
}

func TestMaybeEnsureBrainResident_RespectsInterval(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	setSetting(t, db, SettingBrainResidency, `{"stay_resident":true}`)

	stub := &ensureLoadedStub{Stub: &sched.Stub{}, TargetSlotToReport: "a4"}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()
	now := time.Now()

	s.maybeEnsureBrainResident(ctx, now)
	waitFor(t, time.Second, func() bool { return stub.callCount() == 1 })

	// Simulate the model getting evicted again, then tick again before the
	// interval elapses — must not fire a second time yet.
	stub.resetLoaded()
	s.maybeEnsureBrainResident(ctx, now.Add(1*time.Minute))
	time.Sleep(50 * time.Millisecond)
	if stub.callCount() != 1 {
		t.Errorf("EnsureLoaded calls = %d, want 1 (interval not yet elapsed)", stub.callCount())
	}

	s.maybeEnsureBrainResident(ctx, now.Add(brainResidencyInterval+time.Minute))
	waitFor(t, time.Second, func() bool { return stub.callCount() == 2 })
}

func TestMaybeEnsureBrainResident_SkipsWhenAlreadyResident(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	setSetting(t, db, SettingBrainResidency, `{"stay_resident":true}`)

	// Already resident from the start (never via EnsureLoaded) — set the
	// stub's post-load state directly so Status() reports it up front.
	stub := newEnsureLoadedStubResident("a1", "ornith-35b")
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	s.maybeEnsureBrainResident(ctx, time.Now())
	time.Sleep(50 * time.Millisecond)
	if stub.callCount() != 0 {
		t.Errorf("already-resident brain should not trigger a load, got %d calls", stub.callCount())
	}
}

// TestMaybeEnsureBrainResident_AlreadyResidentTickDoesNotDelayLaterReload is
// a regression test for a real bug found live on ForgeHost: an early version
// stamped the interval clock even on the already-resident no-op path, so a
// tick that happened to land while the brain was already loaded (e.g. right
// after an unrelated on-demand load) started the 5-minute cooldown — and
// then when the brain genuinely went missing moments later, the passenger
// wouldn't try again until the full interval had re-elapsed from that
// unrelated no-op, not from when a reload actually became necessary.
func TestMaybeEnsureBrainResident_AlreadyResidentTickDoesNotDelayLaterReload(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	setSetting(t, db, SettingBrainResidency, `{"stay_resident":true}`)

	stub := newEnsureLoadedStubResident("a1", "ornith-35b") // already resident
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()
	now := time.Now()

	// Tick 1: already resident — must no-op and must NOT consume the
	// interval budget.
	s.maybeEnsureBrainResident(ctx, now)
	time.Sleep(50 * time.Millisecond)
	if stub.callCount() != 0 {
		t.Fatalf("tick 1 (already resident): calls = %d, want 0", stub.callCount())
	}

	// The brain goes missing shortly after (well within what would have
	// been the 5-minute cooldown if tick 1 had consumed it).
	stub.resetLoaded()
	s.maybeEnsureBrainResident(ctx, now.Add(10*time.Second))
	waitFor(t, time.Second, func() bool { return stub.callCount() == 1 })
}
