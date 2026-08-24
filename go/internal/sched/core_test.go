// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/store"
)

func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestEnsureLoadedIdempotentWhenAlreadyLoaded(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a2", "llama")
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))

	tk, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if tk.Status != StatusLoaded || tk.TargetSlot != "a2" {
		t.Fatalf("ticket = %+v, want loaded in a2", tk)
	}
	if n := eng.loadCount(); n != 0 {
		t.Fatalf("engine.Load called %d times, want 0 (idempotent)", n)
	}
	if q := c.Status().Queue; len(q) != 0 {
		t.Fatalf("queue = %v, want empty", q)
	}
}

func TestEnsureLoadedLoadsIntoFreeSlot(t *testing.T) {
	eng := newFakeEngine()
	db := openTestStore(t)
	var routeMu sync.Mutex
	var routeCalls []string
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.Sched = db.Sched()
		d.Settings = db.Settings()
		d.RouteSync = func(slot, mode string) {
			routeMu.Lock()
			routeCalls = append(routeCalls, slot+"="+mode)
			routeMu.Unlock()
		}
	})

	tk, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "a0"})
	if err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if tk.Status != StatusLoaded || tk.TargetSlot != "a1" {
		t.Fatalf("ticket = %+v, want loaded in a1 (first free slot)", tk)
	}
	loads, _ := eng.snapshotCalls()
	if len(loads) != 1 || loads[0] != "llama->a1" {
		t.Fatalf("loads = %v, want [llama->a1]", loads)
	}
	routeMu.Lock()
	defer routeMu.Unlock()
	if len(routeCalls) != 1 || routeCalls[0] != "a1=llama" {
		t.Fatalf("routeSync = %v, want [a1=llama] (_sync_router_route home)", routeCalls)
	}
	slots, err := db.Sched().Slots(context.Background())
	if err != nil {
		t.Fatalf("store slots: %v", err)
	}
	if slots["a1"] != "llama" {
		t.Fatalf("persisted slots = %v, want a1=llama", slots)
	}
	// The ticket must not linger in the persisted queue.
	rows, err := db.Sched().Queue(context.Background())
	if err != nil {
		t.Fatalf("store queue: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("persisted queue = %v, want empty after completion", rows)
	}
}

func TestEnsureLoadedPinnedIgnoresOtherSlots(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a2", "llama")
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))

	tk, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "a0", TargetSlot: "a3"})
	if err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if tk.TargetSlot != "a3" {
		t.Fatalf("ticket = %+v, want pinned load into a3", tk)
	}
	loads, _ := eng.snapshotCalls()
	if len(loads) != 1 || loads[0] != "llama->a3" {
		t.Fatalf("loads = %v, want [llama->a3]", loads)
	}
}

func TestEnsureLoadedTerminalFailure(t *testing.T) {
	eng := newFakeEngine()
	eng.loadFn = func(mode, slot string) engine.Result {
		return engine.Result{Success: false, Message: "unit did not reach running state"}
	}
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))

	tk, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err == nil || !strings.Contains(err.Error(), "unit did not reach running state") {
		t.Fatalf("err = %v, want engine failure message", err)
	}
	if tk.Status != StatusFailed {
		t.Fatalf("ticket = %+v, want failed", tk)
	}
	// Terminal means exactly one attempt — no retry loop on a real
	// engine failure (V4 semantics).
	if n := eng.loadCount(); n != 1 {
		t.Fatalf("engine.Load called %d times, want 1", n)
	}
	if q := c.Status().Queue; len(q) != 0 {
		t.Fatalf("queue = %v, want empty after failure", q)
	}
}

func TestEnsureLoadedFailsFastWhenMaintenanceBlocked(t *testing.T) {
	eng := newFakeEngine()
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.MaintenanceBlocked = func(context.Context) (bool, string) {
			return true, "maintenance mode active (test) — no loads/unloads/restarts until it ends"
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	tk, err := c.EnsureLoaded(ctx, EnsureRequest{Model: "llama", RequestedBy: "test"})
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "maintenance mode active") {
		t.Fatalf("err = %v, want a maintenance-blocked message", err)
	}
	if tk.Status != StatusFailed {
		t.Fatalf("ticket = %+v, want failed", tk)
	}
	if n := eng.loadCount(); n != 0 {
		t.Fatalf("engine.Load called %d times, want 0 — must fail before ever touching the engine", n)
	}
	// The whole point of the advisory check: fail immediately, never sit
	// out the poll-retry loop waiting for a window that isn't going to end.
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("EnsureLoaded took %v — expected an immediate fail-fast, not a poll loop", elapsed)
	}
}

// An already-loaded model must keep serving during a maintenance window —
// only new loads/evictions are blocked.
func TestEnsureLoadedAlreadyLoadedIgnoresMaintenanceBlock(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a2", "llama")
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.MaintenanceBlocked = func(context.Context) (bool, string) {
			return true, "maintenance mode active"
		}
	})

	tk, err := c.EnsureLoaded(context.Background(), EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if tk.Status != StatusLoaded || tk.TargetSlot != "a2" {
		t.Fatalf("ticket = %+v, want already-loaded to pass through maintenance", tk)
	}
}

func TestEnsureLoadedTimesOutWhenNothingEvictable(t *testing.T) {
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{
		"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
	})
	// All busy (recent activity) — normal tier never evicts.
	idle := map[string]time.Duration{
		"a1": time.Second, "a2": time.Second,
		"a3": time.Second, "a4": time.Second,
	}
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), idle))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	tk, err := c.EnsureLoaded(ctx, EnsureRequest{Model: "llama", RequestedBy: "test"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if tk.Status != StatusFailed {
		t.Fatalf("ticket = %+v, want failed on timeout", tk)
	}
	if n := eng.loadCount(); n != 0 {
		t.Fatalf("engine.Load called %d times, want 0 (nothing evictable)", n)
	}
	if q := c.Status().Queue; len(q) != 0 {
		t.Fatalf("queue = %v, want ticket dequeued after timeout", q)
	}
}

// TestEnsureLoadedRacingSameModel: N concurrent callers for one model must
// coalesce onto a single engine load (run with -race).
func TestEnsureLoadedRacingSameModel(t *testing.T) {
	eng := newFakeEngine()
	eng.loadDelay = 20 * time.Millisecond
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.DefaultTimeout = 2 * time.Second
	})

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	tickets := make([]Ticket, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tickets[i], errs[i] = c.EnsureLoaded(context.Background(),
				EnsureRequest{Model: "llama", RequestedBy: "racer"})
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if tickets[i].Status != StatusLoaded {
			t.Fatalf("caller %d ticket = %+v, want loaded", i, tickets[i])
		}
	}
	if n := eng.loadCount(); n != 1 {
		t.Fatalf("engine.Load called %d times, want exactly 1 (coalesced)", n)
	}
	if eng.maxInFlight > 1 {
		t.Fatalf("maxInFlight = %d, want 1 (loads serialized)", eng.maxInFlight)
	}
	if q := c.Status().Queue; len(q) != 0 {
		t.Fatalf("queue = %v, want drained", q)
	}
}

// TestEnsureLoadedSerializesDistinctModels: concurrent loads of different
// models both succeed but never overlap on the GPU (one load at a time).
func TestEnsureLoadedSerializesDistinctModels(t *testing.T) {
	eng := newFakeEngine()
	eng.loadDelay = 10 * time.Millisecond
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.DefaultTimeout = 2 * time.Second
	})

	var wg sync.WaitGroup
	models := []string{"llama", "gemma", "qwen"}
	errs := make([]error, len(models))
	for i, m := range models {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			_, errs[i] = c.EnsureLoaded(context.Background(),
				EnsureRequest{Model: m, RequestedBy: "racer"})
		}(i, m)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("model %s: %v", models[i], err)
		}
	}
	if n := eng.loadCount(); n != len(models) {
		t.Fatalf("engine.Load called %d times, want %d", n, len(models))
	}
	if eng.maxInFlight > 1 {
		t.Fatalf("maxInFlight = %d, want 1 (loadBusy token serializes)", eng.maxInFlight)
	}
}

func TestUnloadThroughScheduler(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a3", "llama")
	db := openTestStore(t)
	var routeMu sync.Mutex
	var routeCalls []string
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.Sched = db.Sched()
		d.RouteSync = func(slot, mode string) {
			routeMu.Lock()
			routeCalls = append(routeCalls, slot+"="+mode)
			routeMu.Unlock()
		}
	})

	if err := c.Unload(context.Background(), "a3", HumanIdentity); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	_, unloads := eng.snapshotCalls()
	if len(unloads) != 1 || unloads[0] != "a3" {
		t.Fatalf("unloads = %v, want [a3]", unloads)
	}
	routeMu.Lock()
	if len(routeCalls) != 1 || routeCalls[0] != "a3=" {
		t.Fatalf("routeSync = %v, want [a3=] (cleared)", routeCalls)
	}
	routeMu.Unlock()
	slots, _ := db.Sched().Slots(context.Background())
	if slots["a3"] != "" {
		t.Fatalf("persisted slots = %v, want a3 empty", slots)
	}

	// Failed engine unload propagates and syncs nothing.
	eng.unloadFn = func(slot string) engine.Result {
		return engine.Result{Success: false, Message: "still deactivating"}
	}
	eng.setOcc("a4", "gemma")
	if err := c.Unload(context.Background(), "a4", HumanIdentity); err == nil ||
		!strings.Contains(err.Error(), "still deactivating") {
		t.Fatalf("err = %v, want deactivating failure", err)
	}
}

func TestStatusShapes(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a1", "llama")
	now := time.Now()
	src := staticSource(now, eng.Slots(), map[string]time.Duration{
		"a1": 42 * time.Second,
	})
	snap := src.Current()
	gtt := int64(120000 * 1024 * 1024) // 120000 MiB in bytes
	used := int64(90000 * 1024 * 1024) // 90000 MiB in bytes
	snap.Metrics.GTTTotalBytes = &gtt
	snap.Metrics.InferenceRSSBytes = &used
	// a1's real live GPU footprint (fdinfo-derived, includes KV cache) —
	// distinct from the aggregate InferenceRSSBytes above.
	a1State := snap.Slots["a1"]
	a1State.MemoryBytes = 42 * 1024 * 1024 * 1024
	snap.Slots["a1"] = a1State
	c := newTestCore(t, eng, src, func(d *Deps) {
		d.Now = func() time.Time { return now }
	})

	st := c.Status()
	if st.Slots["a1"] != "llama" || st.Slots["a2"] != "" {
		t.Fatalf("slots = %v", st.Slots)
	}
	if st.SlotLabels["a1"] != "A1" {
		t.Fatalf("labels = %v, want snapshot label A1", st.SlotLabels)
	}
	if st.IdleSeconds["a1"] == nil || *st.IdleSeconds["a1"] != 42 {
		t.Fatalf("idle[a1] = %v, want 42", st.IdleSeconds["a1"])
	}
	// Empty slots and unknown-activity slots report nil, never 0.
	if st.IdleSeconds["a2"] != nil {
		t.Fatalf("idle[a2] = %v, want nil (empty slot)", *st.IdleSeconds["a2"])
	}
	if _, ok := st.IdleSeconds["a3"]; !ok {
		t.Fatal("idle map must carry every slot key")
	}
	if st.MemoryBudget.TotalBytes != 120000*1024*1024 || st.MemoryBudget.UsedBytes != 90000*1024*1024 || st.MemoryBudget.FreeBytes != 30000*1024*1024 {
		t.Fatalf("budget = %+v", st.MemoryBudget)
	}
	if want := int64(42 * 1024 * 1024 * 1024); st.SlotMemoryBytes["a1"] != want {
		t.Fatalf("SlotMemoryBytes[a1] = %d, want %d (threaded from snapshot.Slots[a1].MemoryBytes)", st.SlotMemoryBytes["a1"], want)
	}
	if st.SlotMemoryBytes["a2"] != 0 {
		t.Fatalf("SlotMemoryBytes[a2] = %d, want 0 (empty slot)", st.SlotMemoryBytes["a2"])
	}
}

// TestStatusIdleUsesSnapshotTimeNotLiveClock covers the pre-release
// feedback round's "impossible idle times" fix: idle must be measured
// against the snapshot's own TakenAt, not the live clock. Before the fix,
// Status() used c.d.Now() directly, so a stalled/delayed collector cycle
// (live clock moves on, snapshot doesn't get any fresher) would inflate
// idle by exactly the staleness gap on top of the real idle window.
func TestStatusIdleUsesSnapshotTimeNotLiveClock(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a1", "llama")
	snapTime := time.Now()
	src := staticSource(snapTime, eng.Slots(), map[string]time.Duration{
		"a1": 5 * time.Second,
	})
	live := snapTime.Add(10 * time.Minute)
	c := newTestCore(t, eng, src, func(d *Deps) {
		d.Now = func() time.Time { return live }
	})

	st := c.Status()
	if st.IdleSeconds["a1"] == nil || *st.IdleSeconds["a1"] != 5 {
		t.Fatalf("idle[a1] = %v, want 5 (measured against snapshot TakenAt, not the live clock)", st.IdleSeconds["a1"])
	}
}

func TestConfigValidationAndPersistence(t *testing.T) {
	eng := newFakeEngine()
	db := openTestStore(t)
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.Settings = db.Settings()
	})

	if got := c.Config(); got != DefaultConfig() {
		t.Fatalf("initial config = %+v, want defaults", got)
	}

	bad := []Config{
		{IdleUnloadS: 29, SmallJobTokenThreshold: 1500, PriorityJumpCap: 2, ReservationSoonMin: 10},
		{IdleUnloadS: 3601, SmallJobTokenThreshold: 1500, PriorityJumpCap: 2, ReservationSoonMin: 10},
		{IdleUnloadS: 180, SmallJobTokenThreshold: 0, PriorityJumpCap: 2, ReservationSoonMin: 10},
		{IdleUnloadS: 180, SmallJobTokenThreshold: 1500, PriorityJumpCap: -1, ReservationSoonMin: 10},
		{IdleUnloadS: 180, SmallJobTokenThreshold: 1500, PriorityJumpCap: 2, ReservationSoonMin: 0},
		{IdleUnloadS: 180, SmallJobTokenThreshold: 1500, PriorityJumpCap: 2, ReservationSoonMin: 121},
	}
	for i, cfg := range bad {
		if err := c.SetConfig(cfg); err == nil {
			t.Fatalf("bad[%d] %+v accepted", i, cfg)
		}
	}
	if got := c.Config(); got != DefaultConfig() {
		t.Fatalf("config mutated by rejected SetConfig: %+v", got)
	}

	want := Config{IdleUnloadS: 300, SmallJobTokenThreshold: 2000, PriorityJumpCap: 1, ReservationSoonMin: 15}
	if err := c.SetConfig(want); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if got := c.Config(); got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}

	// A fresh Core on the same store recovers the persisted config.
	c2 := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		d.Settings = db.Settings()
	})
	if got := c2.Config(); got != want {
		t.Fatalf("recovered config = %+v, want %+v", got, want)
	}
}

func TestSmallJobFromHint(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		hint int
		want bool
	}{
		{0, true}, // V4 parity: absent hint (0) is small
		{1500, true},
		{1501, false},
		{150000, false},
	}
	for _, tc := range cases {
		if got := SmallJobFromHint(cfg, tc.hint); got != tc.want {
			t.Errorf("SmallJobFromHint(%d) = %v, want %v", tc.hint, got, tc.want)
		}
	}
}

func TestNewRequiresEngineAndSource(t *testing.T) {
	if _, err := New(Deps{Source: collector.NewStatic(nil)}); err == nil {
		t.Fatal("New without Engine must fail")
	}
	if _, err := New(Deps{Engine: newFakeEngine()}); err == nil {
		t.Fatal("New without Source must fail")
	}
}
