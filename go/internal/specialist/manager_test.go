// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeSys is an in-memory systemd: Start makes a unit active (unless told to fail), Stop makes it inactive.
type fakeSys struct {
	mu       sync.Mutex
	active   map[string]bool
	failOn   map[string]string // unit -> ActiveState a Start leaves it in ("failed")
	starts   []string
	stops    []string
	startErr map[string]error
}

func newFakeSys() *fakeSys {
	return &fakeSys{active: map[string]bool{}, failOn: map[string]string{}, startErr: map[string]error{}}
}
func (f *fakeSys) Start(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, u)
	if err := f.startErr[u]; err != nil {
		return err
	}
	f.active[u] = f.failOn[u] == ""
	return nil
}
func (f *fakeSys) Stop(_ context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, u)
	f.active[u] = false
	return nil
}
func (f *fakeSys) State(_ context.Context, u string) (collector.UnitState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active[u] {
		return collector.UnitState{ActiveState: "active", SubState: "running"}, nil
	}
	if f.failOn[u] != "" {
		return collector.UnitState{ActiveState: "failed", SubState: "failed"}, nil
	}
	return collector.UnitState{ActiveState: "inactive", SubState: "dead"}, nil
}
func (f *fakeSys) MainPID(context.Context, string) (uint32, error) { return 0, nil }
func (f *fakeSys) kill(u string)                                   { f.mu.Lock(); f.active[u] = false; f.mu.Unlock() }

type env struct {
	rows    SlotLaunchStore // the real in-memory slot_launches store; a test wires it into the manager with e.m.d.SlotLaunches = e.rows
	m       *Manager
	sys     *fakeSys
	sp      store.Specialist
	dir     string
	now     time.Time
	mu      sync.Mutex
	healthy map[int]bool
	free    int64
	failed  []string
	cat     *fakeCatalog
	nctx    map[int]int  // port -> the n_ctx /props reports (absent = the requested value is honoured)
	busy    map[int]bool // port -> a request is in flight
}

func (e *env) clock() time.Time        { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

// newEnv builds a pool of three specialist slots (s1-s3, ports 8100-8102) over an in-memory store.
func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{rows: db.SlotLaunches(), sys: newFakeSys(), sp: db.Specialist(), dir: t.TempDir(), now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), healthy: map[int]bool{}, free: 100 << 30, cat: newFakeCatalog(), nctx: map[int]int{}, busy: map[int]bool{}}
	cfg := &config.Config{SpecialistSlots: map[string]config.Slot{
		"s1": {Unit: "forge-pool@s1", Port: 8100, Label: "S1", Order: 1},
		"s2": {Unit: "forge-pool@s2", Port: 8101, Label: "S2", Order: 2},
		"s3": {Unit: "forge-pool@s3", Port: 8102, Label: "S3", Order: 3},
	}}
	m, err := New(Deps{
		Cfg: func() *config.Config { return cfg }, Sys: e.sys, Store: e.sp, EnvDir: e.dir,
		ModelRoots: func() []string { return []string{"/opt/forge/models"} },
		Health: func(_ context.Context, port int, _ string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.healthy[port] {
				return nil
			}
			return errors.New("connection refused")
		},
		// Mirrors the daemon: its free-memory probe is the engine's memory budget, which calls back into this manager (ReservedBytes). A harness
		// whose probe cannot call back hid a self-deadlock in Ensure from every test.
		FreeBytes: func() int64 {
			if e.m != nil {
				_ = e.m.ReservedBytes()
			}
			return e.free
		},
		Catalog: e.cat,
		NCtx: func(_ context.Context, port int) (int, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if n, ok := e.nctx[port]; ok {
				return n, nil
			}
			return e.cat.lastCtx, nil
		},
		Busy:  func(_ context.Context, port int) bool { e.mu.Lock(); defer e.mu.Unlock(); return e.busy[port] },
		Now:   e.clock,
		Sleep: func(context.Context, time.Duration) { e.advance(time.Second) }, // virtual time: every wait is one second
		Logf:  func(f string, a ...any) { t.Logf(f, a...) },
		OnFailed: func(slot, occ string, attempts int, err error) {
			e.mu.Lock()
			e.failed = append(e.failed, fmt.Sprintf("%s/%s/%d", slot, occ, attempts))
			e.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	return e
}

// add registers an occupant (a qwen3-asr-server, whose only required parameter is a model) and makes its port answer health by default.
func (e *env) add(t *testing.T, name, policy, home string, mem int64) {
	t.Helper()
	if _, err := e.sp.CreateOccupant(context.Background(), store.Occupant{Name: name, Kind: "qwen3-asr-server", Policy: policy, HomeSlot: home, Enabled: true,
		Functions: []string{store.FnTranscription}, MemoryBytes: mem, Params: map[string]string{"model": "/opt/forge/models/" + name + ".gguf"}}); err != nil {
		t.Fatal(err)
	}
}
func (e *env) health(port int, ok bool) { e.mu.Lock(); e.healthy[port] = ok; e.mu.Unlock() }

func state(t *testing.T, e *env, slot string) SlotState {
	t.Helper()
	for _, s := range e.m.Snapshot(context.Background()) {
		if s.Slot == slot {
			return s
		}
	}
	t.Fatalf("no slot %s", slot)
	return SlotState{}
}

func TestEnsure_LoadsOnDemandIntoAFreeSlot(t *testing.T) {
	e := newEnv(t)
	e.add(t, "asr", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	got, err := e.m.Ensure(context.Background(), "asr")
	if err != nil {
		t.Fatal(err)
	}
	if got.Slot != "s1" || got.State != StateRunning || got.Occupant != "asr" {
		t.Fatalf("got %+v", got)
	}
	envText, err := os.ReadFile(filepath.Join(e.dir, "s1.env"))
	if err != nil || !strings.Contains(string(envText), "FORGE_POOL_OCCUPANT=asr\n") || !strings.Contains(string(envText), "FORGE_POOL_PORT=8100\n") {
		t.Errorf("env file = %q, %v", envText, err)
	}
	if fi, _ := os.Stat(filepath.Join(e.dir, "s1.env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("env file mode %v, want 0600", fi.Mode().Perm())
	}
	// A second request is served from the running member: no restart.
	n := len(e.sys.starts)
	if _, err := e.m.Ensure(context.Background(), "asr"); err != nil || len(e.sys.starts) != n {
		t.Errorf("second Ensure restarted the unit (%d starts, err %v)", len(e.sys.starts), err)
	}
}

func TestEnsure_ErrorsAreSpecific(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.m.Ensure(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown occupant: %v", err)
	}
	e.add(t, "off", store.PolicyOnDemand, "", 0)
	o, _ := e.sp.OccupantByName(ctx, "off")
	o.Enabled = false
	_ = e.sp.UpdateOccupant(ctx, o)
	var ue *UnavailableError
	if _, err := e.m.Ensure(ctx, "off"); !errors.As(err, &ue) || ue.Reason != "disabled" {
		t.Errorf("disabled: %v", err)
	}
	// A start that never becomes healthy fails fast with the reason, the unit is stopped, the slot shows failed.
	e.add(t, "sick", store.PolicyOnDemand, "", 0)
	_, err := e.m.Ensure(ctx, "sick")
	if !errors.As(err, &ue) || !strings.Contains(ue.Reason, "not healthy within") {
		t.Fatalf("unhealthy start: %v", err)
	}
	if s := state(t, e, "s1"); s.State != StateFailed || s.Attempts != 1 || s.Err == "" {
		t.Errorf("slot = %+v", s)
	}
	if e.sys.active["forge-pool@s1"] {
		t.Error("an unhealthy unit was left running")
	}
	// A unit that dies at once is reported as such, without waiting out the health timeout.
	e.add(t, "crash", store.PolicyOnDemand, "", 0)
	e.sys.failOn["forge-pool@s2"] = "failed"
	t0 := e.clock()
	if _, err := e.m.Ensure(ctx, "crash"); !errors.As(err, &ue) || !strings.Contains(ue.Reason, "exited") {
		t.Fatalf("crashing start: %v", err)
	}
	if e.clock().Sub(t0) > 5*time.Second {
		t.Errorf("waited %v for a unit that had already failed", e.clock().Sub(t0))
	}
}

// A request that arrives before the supervisor has started an always-on member must not be refused for memory that is already set aside for it. On
// a machine with no GPU memory at all the budget is 0, and the first request after a registration got "not enough GPU memory".
func TestEnsure_AnAlwaysOnMemberInItsHomeSlotIsNotFitGated(t *testing.T) {
	e := newEnv(t)
	e.add(t, "res", store.PolicyAlwaysOn, "s2", 2<<30)
	e.health(8101, true)
	e.free = 0
	got, err := e.m.Ensure(context.Background(), "res")
	if err != nil || got.Slot != "s2" || got.State != StateRunning {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestEnsure_FitGate(t *testing.T) {
	e := newEnv(t)
	e.add(t, "big", store.PolicyOnDemand, "", 8<<30)
	e.health(8100, true)
	e.free = 4 << 30
	var ue *UnavailableError
	if _, err := e.m.Ensure(context.Background(), "big"); !errors.As(err, &ue) || !strings.Contains(ue.Reason, "not enough GPU memory") {
		t.Fatalf("got %v", err)
	}
	if len(e.sys.starts) != 0 {
		t.Error("a load that does not fit was started")
	}
}

func TestPlacement_ReservationsEvictionAndPoolFull(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 1<<30) // reserves s1
	e.add(t, "a", store.PolicyOnDemand, "", 1<<30)
	e.add(t, "b", store.PolicyOnDemand, "", 1<<30)
	e.add(t, "c", store.PolicyOnDemand, "", 1<<30)

	// On-demand members never take another member's reserved home, even while it is empty.
	if s, err := e.m.Ensure(ctx, "a"); err != nil || s.Slot != "s2" {
		t.Fatalf("a -> %+v, %v", s, err)
	}
	if s, err := e.m.Ensure(ctx, "b"); err != nil || s.Slot != "s3" {
		t.Fatalf("b -> %+v, %v", s, err)
	}
	// Both unreserved slots are held, neither idle long enough: the pool is full, with the facts a client needs.
	var pf *PoolFullError
	_, err := e.m.Ensure(ctx, "c")
	if !errors.As(err, &pf) || pf.RetryAfter <= 0 || len(pf.Holders) != 3 {
		t.Fatalf("c: %v (%+v)", err, pf)
	}
	// After the idle threshold the least recently used one is evicted: b was used after a.
	e.m.Touch("s3")
	e.advance(181 * time.Second)
	e.m.Touch("s3")
	e.advance(time.Second)
	s, err := e.m.Ensure(ctx, "c")
	if err != nil || s.Slot != "s2" {
		t.Fatalf("c after idle: %+v, %v (want it to take s2, the least recently used)", s, err)
	}
	if st := state(t, e, "s3"); st.Occupant != "b" || st.State != StateRunning {
		t.Errorf("the more recently used member was disturbed: %+v", st)
	}
	// An always-on member takes its reserved home back from a guest.
	e.sys.mu.Lock()
	e.sys.active["forge-pool@s1"] = false
	e.sys.mu.Unlock()
	if s, err := e.m.Ensure(ctx, "ao"); err != nil || s.Slot != "s1" || s.Policy != store.PolicyAlwaysOn {
		t.Fatalf("ao -> %+v, %v", s, err)
	}
}

func TestSupervisor_StartsAlwaysOnSmallestFirstAndRestartsWithBackoff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "big", store.PolicyAlwaysOn, "s1", 5<<30)
	e.add(t, "small", store.PolicyAlwaysOn, "s2", 1<<30)
	e.health(8101, true)
	// big never gets healthy; small does.
	e.m.Tick(ctx)
	if got := e.sys.starts; len(got) < 2 || got[0] != "forge-pool@s2" {
		t.Fatalf("start order = %v, want the smaller member first", got)
	}
	if s := state(t, e, "s2"); s.State != StateRunning {
		t.Errorf("small = %+v", s)
	}
	s := state(t, e, "s1")
	if s.State != StateFailed || s.Attempts != 1 {
		t.Fatalf("big = %+v", s)
	}
	// Not due yet: a tick right away does not retry.
	n := len(e.sys.starts)
	e.m.Tick(ctx)
	if len(e.sys.starts) != n {
		t.Error("a failed member was retried before its backoff elapsed")
	}
	// Backoff grows 10 s, 30 s, 2 min, then 5 min repeating; the anomaly fires once, at the third failure.
	want := []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		e.advance(6 * time.Minute)
		e.m.Tick(ctx)
		s = state(t, e, "s1")
		if s.Attempts != i+2 {
			t.Fatalf("attempt %d: attempts=%d", i+2, s.Attempts)
		}
		if got := s.NextRetry.Sub(e.clock()); got < w-2*time.Minute || got > w { // the load itself spent virtual time
			t.Errorf("after %d failures next retry in %v, want about %v", s.Attempts, got, w)
		}
	}
	time.Sleep(20 * time.Millisecond) // OnFailed runs in a goroutine
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.failed) != 1 || !strings.HasPrefix(e.failed[0], "s1/big/3") {
		t.Errorf("OnFailed calls = %v, want exactly one at attempt 3", e.failed)
	}
}

func TestSupervisor_NoticesADeadMemberAndOperatorStopsStick(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 1<<30)
	e.add(t, "od", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	e.health(8101, true)
	e.m.Tick(ctx)
	if _, err := e.m.Ensure(ctx, "od"); err != nil {
		t.Fatal(err)
	}
	// Both die behind our back.
	e.sys.kill("forge-pool@s1")
	e.sys.kill("forge-pool@s2")
	e.m.noticeDeaths(ctx)
	if s := state(t, e, "s1"); s.State != StateFailed || s.Attempts != 1 {
		t.Errorf("always-on that died: %+v (want failed, restart scheduled)", s)
	}
	if s := state(t, e, "s2"); s.State != StateEmpty {
		t.Errorf("on-demand that died: %+v (want empty, loaded again on the next request)", s)
	}
	// The always-on one comes back at its backoff.
	e.advance(11 * time.Second)
	e.m.Tick(ctx)
	if s := state(t, e, "s1"); s.State != StateRunning {
		t.Errorf("after backoff: %+v", s)
	}
	// An operator stop sticks: the supervisor leaves it alone until it is loaded again.
	if err := e.m.Unload(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	n := len(e.sys.starts)
	e.advance(time.Hour)
	e.m.Tick(ctx)
	if s := state(t, e, "s1"); s.State != StateStopped || len(e.sys.starts) != n {
		t.Errorf("an operator-stopped always-on member was restarted: %+v", s)
	}
	var ue *UnavailableError
	if _, err := e.m.Ensure(ctx, "ao"); !errors.As(err, &ue) || !strings.Contains(ue.Reason, "stopped by the operator") {
		t.Errorf("Ensure of a stopped member: %v", err)
	}
	if s, err := e.m.Load(ctx, "ao", "s1"); err != nil || s.State != StateRunning {
		t.Errorf("explicit load after a stop: %+v, %v", s, err)
	}
}

func TestAdopt_KeepsAHealthyRunningMemberAcrossADaemonRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 1<<30)
	e.health(8100, true)
	e.m.Tick(ctx) // loads it, writes s1.env
	n := len(e.sys.starts)

	// A new Manager (the daemon restarted) over the same units and env dir.
	e2 := &env{sys: e.sys, sp: e.sp, dir: e.dir, now: e.now, healthy: e.healthy}
	m2, err := New(Deps{Cfg: e.m.d.Cfg, Sys: e.sys, Store: e.sp, EnvDir: e.dir, ModelRoots: e.m.d.ModelRoots, Health: e.m.d.Health, Now: e.clock,
		Sleep: e.m.d.Sleep, Logf: e.m.d.Logf})
	if err != nil {
		t.Fatal(err)
	}
	e2.m = m2
	m2.Adopt(ctx)
	m2.Tick(ctx)
	if got := state(t, e2, "s1"); got.State != StateRunning || got.Occupant != "ao" {
		t.Fatalf("adopted = %+v", got)
	}
	if len(e.sys.starts) != n {
		t.Errorf("a healthy member was restarted by a daemon restart (%d starts, was %d)", len(e.sys.starts), n)
	}
}

func TestLoadAndUnload_Explicit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "a", store.PolicyOnDemand, "", 0)
	e.health(8102, true)
	if _, err := e.m.Load(ctx, "a", "s9"); err == nil {
		t.Error("a load into a slot that does not exist was accepted")
	}
	if s, err := e.m.Load(ctx, "a", "s3"); err != nil || s.Slot != "s3" {
		t.Fatalf("load: %+v, %v", s, err)
	}
	if _, err := e.m.Load(ctx, "a", "s2"); err == nil {
		t.Error("the same occupant was loaded into a second slot")
	}
	if err := e.m.Unload(ctx, "s3"); err != nil {
		t.Fatal(err)
	}
	if s := state(t, e, "s3"); s.State != StateEmpty || s.Occupant != "" {
		t.Errorf("after unload: %+v", s)
	}
	if e.sys.active["forge-pool@s3"] {
		t.Error("the unit is still active after Unload")
	}
}

// Two requests for the same cold member must start it once: the second waits for the first's start and shares the result.
func TestEnsure_ConcurrentRequestsStartOnce(t *testing.T) {
	e := newEnv(t)
	e.add(t, "asr", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.m.Ensure(context.Background(), "asr")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}
	e.sys.mu.Lock()
	defer e.sys.mu.Unlock()
	n := 0
	for _, u := range e.sys.starts {
		if u == "forge-pool@s1" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the unit was started %d times for 8 concurrent requests, want 1", n)
	}
}

func TestResolve(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.health(8100, true)
	e.health(8101, true)
	e.add(t, "asr", store.PolicyOnDemand, "", 0)
	if _, err := e.m.Resolve(ctx, store.FnTranscription, "whisper-1"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("no default and an unknown model must be ErrNoMatch, got %v", err)
	}
	if err := e.sp.SetDefault(ctx, store.FnTranscription, "asr"); err != nil {
		t.Fatal(err)
	}
	// An unknown model string falls back to the function's default; an empty one too.
	for _, model := range []string{"", "whisper-1"} {
		tg, err := e.m.Resolve(ctx, store.FnTranscription, model)
		if err != nil || tg.Occupant != "asr" || tg.BaseURL != "http://127.0.0.1:8100" || tg.Slot != "s1" {
			t.Fatalf("model %q -> %+v, %v", model, tg, err)
		}
		tg.Touch()
	}
	// A member named explicitly wins over the default, but only for a function it serves.
	if _, err := e.sp.CreateOccupant(ctx, store.Occupant{Name: "kokoro", Kind: "kokoro", Enabled: true, Functions: []string{store.FnSpeech}}); err != nil {
		t.Fatal(err)
	}
	e.health(8880, true)
	if tg, err := e.m.Resolve(ctx, store.FnSpeech, "kokoro"); err != nil || tg.BaseURL != "http://127.0.0.1:8880" {
		t.Fatalf("kokoro -> %+v, %v (its port is fixed by the kind, not the slot)", tg, err)
	}
	var ue *UnavailableError
	if _, err := e.m.Resolve(ctx, store.FnTranscription, "kokoro"); !errors.As(err, &ue) {
		t.Errorf("a member asked for a function it does not serve: %v", err)
	}
}

// A member that died between supervisor passes must not be handed to a request: an always-on one answers at once with a specific error and is
// scheduled for restart, an on-demand one is reloaded transparently.
func TestEnsure_NoticesAMemberThatDiedBetweenSupervisorPasses(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 1<<30)
	e.add(t, "od", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	e.health(8101, true)
	e.m.Tick(ctx)
	if _, err := e.m.Ensure(ctx, "od"); err != nil {
		t.Fatal(err)
	}
	e.sys.kill("forge-pool@s1")
	e.sys.kill("forge-pool@s2")

	var ue *UnavailableError
	if _, err := e.m.Ensure(ctx, "ao"); !errors.As(err, &ue) || !strings.Contains(ue.Reason, "failed") {
		t.Fatalf("always-on that just died: %v (want an immediate unavailable error, not a hand-off to a dead port)", err)
	}
	if s := state(t, e, "s1"); s.State != StateFailed || s.Attempts != 1 {
		t.Errorf("s1 = %+v (want failed with a restart scheduled)", s)
	}
	got, err := e.m.Ensure(ctx, "od")
	if err != nil || got.State != StateRunning {
		t.Fatalf("on-demand that just died: %+v, %v (want a transparent reload)", got, err)
	}
	if !e.sys.active["forge-pool@"+got.Slot] {
		t.Error("the reloaded member's unit is not running")
	}
}

func TestForget_ClearsAStoppedGhostButNeverARunningMember(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 0)
	e.add(t, "od", store.PolicyOnDemand, "", 0)
	e.health(8100, true)
	e.health(8101, true)
	e.m.Tick(ctx)
	if _, err := e.m.Ensure(ctx, "od"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Unload(ctx, "s1"); err != nil { // an operator stop leaves a "stopped" record
		t.Fatal(err)
	}
	e.m.Forget("ao")
	if s := state(t, e, "s1"); s.State != StateEmpty || s.Occupant != "" {
		t.Errorf("a stopped member's ghost survived Forget: %+v", s)
	}
	e.m.Forget("od")
	if s := state(t, e, "s2"); s.State != StateRunning || s.Occupant != "od" {
		t.Errorf("Forget disturbed a running member: %+v", s)
	}
}

// Kokoro listens on its own fixed port, not on its slot's; the pool must report where it really is.
func TestSnapshot_ReportsAFixedPortKindOnItsRealPort(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.sp.CreateOccupant(ctx, store.Occupant{Name: "kokoro", Kind: "kokoro", Policy: store.PolicyAlwaysOn, HomeSlot: "s1", Enabled: true, Functions: []string{store.FnSpeech}}); err != nil {
		t.Fatal(err)
	}
	e.health(8880, true)
	e.m.Tick(ctx)
	if s := state(t, e, "s1"); s.State != StateRunning || s.Port != 8880 {
		t.Errorf("s1 = %+v, want running on port 8880 (the kind's fixed port, not the slot's 8100)", s)
	}
}

// A fresh Manager over the same store and the same machine: what a daemon restart does.
func (e *env) restart(t *testing.T) *env {
	t.Helper()
	m2, err := New(Deps{Cfg: e.m.d.Cfg, Sys: e.sys, Store: e.sp, EnvDir: e.dir, ModelRoots: e.m.d.ModelRoots, Health: e.m.d.Health, FreeBytes: e.m.d.FreeBytes,
		Now: e.clock, Sleep: e.m.d.Sleep, Logf: e.m.d.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return &env{m: m2, sys: e.sys, sp: e.sp, dir: e.dir, now: e.now, healthy: e.healthy, free: e.free}
}

// The promise: an operator stop survives a daemon restart (it used to be held in memory only, so a restart silently started the member again).
func TestOperatorStopSurvivesADaemonRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 1<<30)
	e.add(t, "other", store.PolicyAlwaysOn, "s2", 1<<30)
	e.health(8100, true)
	e.health(8101, true)
	e.m.Tick(ctx)
	if err := e.m.Unload(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if o, _ := e.sp.OccupantByName(ctx, "ao"); !o.Stopped {
		t.Fatal("the stop was not persisted")
	}

	// The daemon restarts. The other member is adopted, the stopped one is left alone and shown as stopped.
	e2 := e.restart(t)
	starts := len(e.sys.starts)
	e2.m.Adopt(ctx)
	e2.m.Tick(ctx)
	if len(e.sys.starts) != starts {
		t.Errorf("a daemon restart started something: %v", e.sys.starts[starts:])
	}
	if s := state(t, e2, "s1"); s.State != StateStopped || s.Occupant != "ao" {
		t.Errorf("after restart s1 = %+v, want the member shown as stopped", s)
	}
	if s := state(t, e2, "s2"); s.State != StateRunning {
		t.Errorf("the other member was disturbed: %+v", s)
	}
	var ue *UnavailableError
	if _, err := e2.m.Ensure(ctx, "ao"); !errors.As(err, &ue) || !strings.Contains(ue.Reason, "stopped by the operator") {
		t.Errorf("on-demand use of a stopped member: %v", err)
	}
	e2.advance(time.Hour)
	e2.m.Tick(ctx)
	if len(e.sys.starts) != starts {
		t.Error("the supervisor started a stopped member after an hour")
	}

	// Only an explicit start clears it, and then it is a normal member again, across another restart too.
	if s, err := e2.m.Load(ctx, "ao", "s1"); err != nil || s.State != StateRunning {
		t.Fatalf("explicit load: %+v, %v", s, err)
	}
	if o, _ := e.sp.OccupantByName(ctx, "ao"); o.Stopped {
		t.Error("an explicit load did not clear the stop")
	}
	e.sys.kill("forge-pool@s1")
	e3 := e2.restart(t)
	e3.m.Tick(ctx)
	if s := state(t, e3, "s1"); s.State != StateRunning {
		t.Errorf("after the stop was cleared a restart must start it again: %+v", s)
	}
}

// What a cold boot does: nothing is running, every always-on member comes up, smallest first, and nothing the operator stopped.
func TestColdBootStartsEveryAlwaysOnMemberExceptStoppedOnes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "big", store.PolicyAlwaysOn, "s1", 4<<30)
	e.add(t, "small", store.PolicyAlwaysOn, "s2", 1<<30)
	e.add(t, "held", store.PolicyAlwaysOn, "s3", 2<<30)
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	if err := e.sp.SetStopped(ctx, "held", true); err != nil {
		t.Fatal(err)
	}
	e.m.Adopt(ctx) // nothing is running, as after a reboot
	e.m.Tick(ctx)
	if got := e.sys.starts; len(got) != 2 || got[0] != "forge-pool@s2" || got[1] != "forge-pool@s1" {
		t.Errorf("start order = %v, want the smaller member first and the stopped one never", got)
	}
	if s := state(t, e, "s3"); s.State != StateStopped {
		t.Errorf("s3 = %+v", s)
	}
}

func TestReservedBytesIsWhatRestartingMembersStillNeed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "a", store.PolicyAlwaysOn, "s1", 3<<30) // will be running
	e.add(t, "b", store.PolicyAlwaysOn, "s2", 2<<30) // will fail to start
	e.add(t, "c", store.PolicyAlwaysOn, "s3", 5<<30) // stopped by the operator: not owed
	e.add(t, "d", store.PolicyOnDemand, "", 7<<30)   // on-demand: never reserved
	e.health(8100, true)
	if err := e.sp.SetStopped(ctx, "c", true); err != nil {
		t.Fatal(err)
	}
	if got := e.m.ReservedBytes(); got != 5<<30 { // a and b not running yet; c stopped; d on-demand
		t.Errorf("before start: reserved %d GiB, want 5", got>>30)
	}
	e.m.Tick(ctx) // a runs; b never becomes healthy and fails
	if got := e.m.ReservedBytes(); got != 2<<30 {
		t.Errorf("a running, b failing: reserved %d GiB, want 2 (b's)", got>>30)
	}
	if p := e.m.AlwaysOnProblems(ctx); len(p) != 1 || p[0].Occupant != "b" || p[0].State != StateFailed {
		t.Errorf("AlwaysOnProblems = %+v, want only the failed b (a runs, c is stopped by the operator)", p)
	}
}

// Regression (found live, v0.6.62): Ensure called the free-memory probe while holding the manager's lock, and the daemon's probe calls back into
// the manager, so the first on-demand placement wedged the whole pool. The harness's probe now calls back, and this test fails by timeout if the
// lock is ever held across it again.
func TestEnsure_PlacementDoesNotHoldTheLockAcrossTheMemoryProbe(t *testing.T) {
	e := newEnv(t)
	e.add(t, "asr", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	done := make(chan error, 1)
	go func() { _, err := e.m.Ensure(context.Background(), "asr"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ensure deadlocked: the memory probe ran under the manager's lock")
	}
	// The pool must still answer while another placement is being decided.
	if s := e.m.Snapshot(context.Background()); len(s) == 0 {
		t.Error("Snapshot returned nothing")
	}
}

// Regression (idle clock): a request that outlasts idle_unload_s must leave its member freshly used when it FINISHES. Here "a" serves a long
// request (placed first, never touched during it) and "b" sits idle; when the long request completes it touches "a" (what Target.Touch does),
// so the pool evicts the genuinely idle "b" for "c", not the member that just finished work.
func TestIdleClock_LongRequestTouchedAtFinishIsNotEvictable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 1<<30)
	e.add(t, "a", store.PolicyOnDemand, "", 1<<30)
	e.add(t, "b", store.PolicyOnDemand, "", 1<<30)
	e.add(t, "c", store.PolicyOnDemand, "", 1<<30)
	if s, err := e.m.Ensure(ctx, "a"); err != nil || s.Slot != "s2" {
		t.Fatalf("a -> %+v, %v", s, err)
	}
	e.advance(time.Second)
	if s, err := e.m.Ensure(ctx, "b"); err != nil || s.Slot != "s3" {
		t.Fatalf("b -> %+v, %v", s, err)
	}
	e.advance(200 * time.Second) // a's request runs longer than the 180 s idle window
	e.m.Touch("s2")              // ... and finishes
	s, err := e.m.Ensure(ctx, "c")
	if err != nil || s.Slot != "s3" {
		t.Fatalf("c -> %+v, %v (want it to take s3 from idle b, not s2 from the member that just finished)", s, err)
	}
	if st := state(t, e, "s2"); st.Occupant != "a" || st.State != StateRunning {
		t.Errorf("the member that just finished a long request was evicted: %+v", st)
	}
}

// Unload marks the slot "stopping" for the whole stop (so a slow stop is visible and nothing is placed on or restarted into it), never holds the
// manager lock across the stop, and ends empty.
func TestUnload_SlotReadsStoppingWhileTheStopRuns(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "a", store.PolicyOnDemand, "", 0)
	e.health(8102, true)
	if _, err := e.m.Load(ctx, "a", "s3"); err != nil {
		t.Fatal(err)
	}
	// Replace the systemd fake's Stop with one that parks, as a unit finishing a generation does.
	gate := &gatedSys{fakeSys: e.sys, entered: make(chan struct{}), release: make(chan struct{})}
	e.m.d.Sys = gate
	done := make(chan error, 1)
	go func() { done <- e.m.Unload(ctx, "s3") }()
	<-gate.entered
	if st := state(t, e, "s3"); st.State != StateStopping || st.Occupant != "a" { // Snapshot takes the lock: proves it is not held across the stop
		t.Fatalf("during the stop: %+v, want stopping", st)
	}
	var un *UnavailableError
	if _, err := e.m.Ensure(ctx, "a"); !errors.As(err, &un) {
		t.Errorf("Ensure on a stopping member: %v, want UnavailableError", err)
	}
	if err := e.m.Unload(ctx, "s3"); err != nil {
		t.Errorf("a second Unload of a stopping slot should be a no-op, got %v", err)
	}
	e.m.Tick(ctx)
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := state(t, e, "s3"); st.State != StateEmpty {
		t.Errorf("after the stop: %+v", st)
	}
}

type gatedSys struct {
	*fakeSys
	entered, release chan struct{}
	once             sync.Once
}

func (g *gatedSys) Stop(ctx context.Context, u string) error {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.fakeSys.Stop(ctx, u)
}

// A stop that fails puts the slot back as it was, with the reason in Err, instead of leaving it "stopping" forever.
func TestUnload_FailedStopRestoresTheSlotAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "a", store.PolicyOnDemand, "", 0)
	e.health(8102, true)
	if _, err := e.m.Load(ctx, "a", "s3"); err != nil {
		t.Fatal(err)
	}
	e.m.d.Sys = &errStopSys{fakeSys: e.sys}
	if err := e.m.Unload(ctx, "s3"); err == nil {
		t.Fatal("a failed stop returned nil")
	}
	if st := state(t, e, "s3"); st.State != StateRunning || !strings.Contains(st.Err, "stop refused") {
		t.Errorf("after a failed stop: %+v", st)
	}
}

type errStopSys struct{ *fakeSys }

func (errStopSys) Stop(context.Context, string) error { return errors.New("stop refused") }

// Adopt runs in the background while the daemon already serves requests. A slot a request has already claimed (Ensure marks it Starting) must keep
// its record: overwriting it with "running" hid a start in progress.
func TestAdopt_DoesNotOverwriteASlotARequestAlreadyClaimed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 1<<30)
	e.health(8100, true)
	e.m.Tick(ctx) // starts it, writes s1.env, the unit is active

	m2, err := New(Deps{Cfg: e.m.d.Cfg, Sys: e.sys, Store: e.sp, EnvDir: e.dir, ModelRoots: e.m.d.ModelRoots, Health: e.m.d.Health, Now: e.clock,
		Sleep: e.m.d.Sleep, Logf: e.m.d.Logf})
	if err != nil {
		t.Fatal(err)
	}
	// A request got to s1 before Adopt did: the slot is Starting.
	m2.mu.Lock()
	m2.rec("s1").occupant, m2.rec("s1").state = "someone-else", StateStarting
	m2.mu.Unlock()
	m2.Adopt(ctx)
	m2.mu.Lock()
	got := *m2.rec("s1")
	m2.mu.Unlock()
	if got.state != StateStarting || got.occupant != "someone-else" {
		t.Errorf("Adopt overwrote a slot that was already being started: %+v", got)
	}
}

// The declared partition reserves every enabled always-on member's peak whether or not it is running: a restart must not change what the models
// are allowed to take. Stopped members, on-demand members and members without a home slot are not declared.
func TestDeclaredBytesCountsEveryEnabledAlwaysOnMemberRunningOrNot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "a", store.PolicyAlwaysOn, "s1", 3<<30) // will be running
	e.add(t, "b", store.PolicyAlwaysOn, "s2", 2<<30) // will fail to start
	e.add(t, "c", store.PolicyAlwaysOn, "s3", 5<<30) // stopped by the operator
	e.add(t, "d", store.PolicyOnDemand, "", 7<<30)   // on-demand
	e.health(8100, true)
	if err := e.sp.SetStopped(ctx, "c", true); err != nil {
		t.Fatal(err)
	}
	if got := e.m.DeclaredBytes(); got != 5<<30 {
		t.Errorf("before start: declared %d GiB, want 5 (a + b)", got>>30)
	}
	e.m.Tick(ctx) // a runs, b fails
	if got := e.m.DeclaredBytes(); got != 5<<30 {
		t.Errorf("after a runs and b fails: declared %d GiB, want 5 (unchanged: a restart does not move the partition)", got>>30)
	}
}
