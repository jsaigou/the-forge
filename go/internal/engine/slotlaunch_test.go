// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/launch"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeProc is a scriptable ProcInspector.
type fakeProc struct {
	cmd map[uint32][]string
	env map[uint32]map[string]string
	err error
}

func (f *fakeProc) Cmdline(pid uint32) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	c, ok := f.cmd[pid]
	if !ok {
		return nil, os.ErrNotExist
	}
	return c, nil
}

func (f *fakeProc) Environ(pid uint32) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.env[pid], nil
}

// hookedStore wraps a real SlotLaunchStore with failure and interleaving hooks.
type hookedStore struct {
	SlotLaunchStore
	putErr     error
	disarmErr  error
	afterGet   func() // runs between the engine's Get and its next call
	disarmCall int
}

func (h *hookedStore) Put(ctx context.Context, l store.SlotLaunch) (int64, error) {
	if h.putErr != nil {
		return 0, h.putErr
	}
	return h.SlotLaunchStore.Put(ctx, l)
}

func (h *hookedStore) Get(ctx context.Context, slot string) (store.SlotLaunch, error) {
	row, err := h.SlotLaunchStore.Get(ctx, slot)
	if h.afterGet != nil {
		h.afterGet()
	}
	return row, err
}

func (h *hookedStore) Disarm(ctx context.Context, slot string, gen int64) (bool, error) {
	h.disarmCall++
	if h.disarmErr != nil {
		return false, h.disarmErr
	}
	return h.SlotLaunchStore.Disarm(ctx, slot, gen)
}

func newLaunchManager(t *testing.T, cfg *config.Config, sys *fakeSys, stub *llamaStub, ss SlotLaunchStore, procs ProcInspector) *Manager {
	t.Helper()
	baseURL := func(port int) string { return "http://127.0.0.1:1" }
	if stub != nil {
		baseURL = func(port int) string { return stub.srv.URL }
	}
	m, err := NewManager(Deps{
		Cfg:               func() *config.Config { return cfg },
		Sys:               sys,
		GPU:               &collector.GPU{DRMRoot: t.TempDir()},
		Proc:              collector.Proc{Root: t.TempDir()},
		Usage:             &fakeUsage{},
		BaseURL:           baseURL,
		Kill:              func(pid int) error { return nil },
		PollInterval:      time.Millisecond,
		Logf:              t.Logf,
		SlotLaunches:      ss,
		SlotLaunchEnforce: true, // the tests below the shadow section assert enforcement; shadow tests flip it
		Procs:             procs,
		ReadMeta:          func(string) (gguf.Metadata, error) { return gguf.Metadata{TrainedCtx: 131072}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func wsl(m *Manager, slot, mode string, id int64, svc config.Service) error {
	_, err := m.writeSlotLaunch(context.Background(), slot, mode, id, svc)
	return err
}

func openLaunchStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var richSvc = config.Service{Model: "q.gguf", Alias: "q", Context: 262144, Backend: "rocm", MMProj: "mm/q-mmproj.gguf",
	LlamaBin: "/opt/forge/llama.cpp-kintsugi/build-rocm/bin/llama-server", ExtraArgs: []string{"--parallel", "1", "--jinja"}, StartupTimeoutS: 120}

// The row equals launch.BuildService's output and carries the config metadata the engine reads back (no env/args files exist any more, step d2).
func TestRowEqualsBuilderOutput(t *testing.T) {
	cfgA := testConfig(t)
	db := openLaunchStore(t)
	mA := newLaunchManager(t, cfgA, newFakeSys(), nil, db.SlotLaunches(), &fakeProc{})

	if err := wsl(mA, "a3", "qwen", 42, richSvc); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(cfgA.Paths.SysconfigDir); err != nil || len(entries) != 0 {
		t.Fatalf("a load must not write into the sysconfig dir: %v %v", entries, err)
	}

	want, err := launch.BuildService(cfgA, "a3", richSvc)
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.SlotLaunches().Get(context.Background(), "a3")
	if err != nil {
		t.Fatal(err)
	}
	if row.Exe != want.Exe || !reflect.DeepEqual(row.Argv, want.Argv) || len(row.Env) != len(want.Env) || row.Cwd != want.Cwd {
		t.Errorf("row != builder output\n row: %+v\n want: %+v", row, want)
	}
	if row.SpecHash != store.SlotLaunchSpecHash(want.Exe, want.Argv, want.Env, want.Cwd) {
		t.Errorf("spec hash mismatch")
	}
	if row.Generation != 1 || !row.Armed || row.ConfigName != "qwen" || row.ConfigID == nil || *row.ConfigID != 42 ||
		row.EngineKind != "llamacpp" || row.Port != 8087 || row.NCtx != 262144 || row.WrittenBy != "engine" ||
		row.MmprojPath != cfgA.Paths.ResolveModelPath("mm/q-mmproj.gguf") {
		t.Errorf("row metadata wrong: %+v", row)
	}
	if err := wsl(mA, "a3", "qwen", 42, richSvc); err != nil {
		t.Fatal(err)
	}
	if row, _ = db.SlotLaunches().Get(context.Background(), "a3"); row.Generation != 2 {
		t.Errorf("generation = %d, want 2", row.Generation)
	}
}

// Load writes the row (armed) before the unit is started; Unload disarms it afterwards.
func TestLoadWritesRowBeforeStartAndUnloadDisarms(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	db := openLaunchStore(t)
	m := newLaunchManager(t, cfg, sys, newLlamaStub(t, 131072), db.SlotLaunches(), &fakeProc{})
	var atStart store.SlotLaunch
	sys.onStart = func(string) {
		r, err := db.SlotLaunches().Get(context.Background(), "a1")
		if err != nil {
			t.Errorf("no row at Start: %v", err)
		}
		atStart = r
	}
	sys.setSeq("forge-a1", st("inactive", "dead"), st("activating", "start"), st("active", "running"))
	if res := m.Load(context.Background(), "gemma", "a1"); !res.Success {
		t.Fatalf("load: %+v", res)
	}
	if atStart.Generation != 1 || !atStart.Armed || atStart.ConfigName != "gemma" {
		t.Errorf("row at Start = %+v", atStart)
	}

	sys.setSeq("forge-a1", st("inactive", "dead"))
	if res := m.Unload(context.Background(), "a1"); !res.Success || res.Detail != "" {
		t.Fatalf("unload: %+v", res)
	}
	r, _ := db.SlotLaunches().Get(context.Background(), "a1")
	if r.Armed || r.Generation != 1 {
		t.Errorf("after unload row = armed %v gen %d", r.Armed, r.Generation)
	}
}

// Failure order 1: the launch cannot be built. Nothing is written.
func TestBuildFailureWritesNothing(t *testing.T) {
	cfg := testConfig(t)
	db := openLaunchStore(t)
	m := newLaunchManager(t, cfg, newFakeSys(), nil, db.SlotLaunches(), &fakeProc{})
	bad := config.Service{Model: "m.gguf", Alias: "m", Context: 4096, Backend: "bogus-backend"}
	err := wsl(m, "a1", "gemma", 1, bad)
	if err == nil || !strings.Contains(err.Error(), "could not be built") {
		t.Fatalf("err = %v", err)
	}
	if _, gerr := db.SlotLaunches().Get(context.Background(), "a1"); !errors.Is(gerr, store.ErrNotFound) {
		t.Errorf("row written despite build failure: %v", gerr)
	}
	// Through Load: Success=false, unit never started.
	sys := newFakeSys()
	cfg.Modes["bad"] = config.Mode{Services: []config.Service{{Model: "m.gguf", Alias: "m", Context: 1, Backend: "bogus-backend", PortRole: "a1"}}}
	m2 := newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), &fakeProc{})
	res := m2.Load(context.Background(), "bad", "a1")
	if res.Success || !strings.Contains(res.Message, "could not be built") || len(sys.started) != 0 {
		t.Errorf("res = %+v started=%v", res, sys.started)
	}
}

// Failure order 2: the row write fails. The load fails loudly and the old armed row is disarmed so it cannot be mistaken for the new launch.
func TestRowFailureDisarmsPreviousRowAndFailsLoad(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	db := openLaunchStore(t)
	hs := &hookedStore{SlotLaunchStore: db.SlotLaunches()}
	m := newLaunchManager(t, cfg, sys, nil, hs, &fakeProc{})
	svc := config.Service{Model: "gemma.gguf", Alias: "gemma", Context: 8192, Backend: "vulkan"}
	if err := wsl(m, "a1", "gemma", 1, svc); err != nil {
		t.Fatal(err)
	}
	hs.putErr = errors.New("disk full")
	svc.Alias = "new"
	err := wsl(m, "a1", "qwen", 2, svc)
	if err == nil || !strings.Contains(err.Error(), "could not be written") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if r, _ := db.SlotLaunches().Get(context.Background(), "a1"); r.Armed {
		t.Error("previous row still armed")
	}

	// Through Load: Success=false and the unit is never started.
	res := m.Load(context.Background(), "gemma", "a1")
	if res.Success || !strings.Contains(res.Message, "slot launch row for a1 could not be written") || len(sys.started) != 0 {
		t.Errorf("res = %+v started=%v", res, sys.started)
	}

	// If the disarm also fails the error says so (nothing swallowed).
	hs.disarmErr = errors.New("locked")
	if _, err := db.SlotLaunches().Put(context.Background(), store.SlotLaunch{Slot: "a1", ConfigName: "x", EngineKind: "llama.cpp", Exe: "/bin/x", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := wsl(m, "a1", "gemma", 1, svc); err == nil || !strings.Contains(err.Error(), "also not disarmed") {
		t.Errorf("err = %v", err)
	}
}

// Unload: disarm CAS semantics, including a reload (Put) racing between the read of the generation and the disarm.
func TestDisarmCASAgainstConcurrentReload(t *testing.T) {
	cfg := testConfig(t)
	db := openLaunchStore(t)
	hs := &hookedStore{SlotLaunchStore: db.SlotLaunches()}
	m := newLaunchManager(t, cfg, newFakeSys(), nil, hs, &fakeProc{})
	svc := config.Service{Model: "gemma.gguf", Alias: "gemma", Context: 8192, Backend: "vulkan"}
	if err := wsl(m, "a1", "gemma", 1, svc); err != nil {
		t.Fatal(err)
	}
	// A reload lands right after the unload read generation 1.
	hs.afterGet = func() {
		hs.afterGet = nil
		if _, err := db.SlotLaunches().Put(context.Background(), store.SlotLaunch{Slot: "a1", ConfigName: "newer", EngineKind: "llama.cpp",
			Exe: "/bin/x", Port: 8080}); err != nil {
			t.Error(err)
		}
	}
	if err := m.disarmSlotRow(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	r, _ := db.SlotLaunches().Get(context.Background(), "a1")
	if !r.Armed || r.Generation != 2 || r.ConfigName != "newer" {
		t.Errorf("late disarm clobbered the newer row: %+v", r)
	}
	// Plain cases: a matching generation disarms; no row and already-disarmed rows are fine.
	if err := m.disarmSlotRow(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	if r, _ = db.SlotLaunches().Get(context.Background(), "a1"); r.Armed {
		t.Error("row still armed")
	}
	if err := m.disarmSlotRow(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.disarmSlotRow(context.Background(), "a2"); err != nil {
		t.Fatalf("no row: %v", err)
	}
}

// A failed disarm is surfaced in Result.Detail and logged; the unload itself still reports the (genuinely freed) slot.
func TestUnloadSurfacesDisarmFailure(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	db := openLaunchStore(t)
	hs := &hookedStore{SlotLaunchStore: db.SlotLaunches()}
	m := newLaunchManager(t, cfg, sys, nil, hs, &fakeProc{})
	svc := config.Service{Model: "gemma.gguf", Alias: "gemma", Context: 8192, Backend: "vulkan"}
	if err := wsl(m, "a2", "gemma", 1, svc); err != nil {
		t.Fatal(err)
	}
	hs.disarmErr = errors.New("database is locked")
	res := m.Unload(context.Background(), "a2")
	if !res.Success || !strings.Contains(res.Detail, "slot launch row not disarmed: a2") || !strings.Contains(res.Detail, "database is locked") {
		t.Errorf("res = %+v", res)
	}
	res = m.Unload(context.Background(), "all")
	if !res.Success || !strings.Contains(res.Detail, "a2") {
		t.Errorf("unload all res = %+v", res)
	}
	// A unit that did not stop keeps its row armed.
	hs.disarmErr = nil
	hs.disarmCall = 0
	sys.setSeq("forge-a2", st("deactivating", "stop-sigterm"))
	if res := m.Unload(context.Background(), "a2"); res.Success {
		t.Errorf("unload of a stuck unit succeeded: %+v", res)
	}
	if hs.disarmCall != 0 {
		t.Errorf("disarmed a slot whose unit was still deactivating")
	}
}

// ── adopt ──

func adoptFixture(t *testing.T) (*config.Config, *fakeSys, *store.DB, *fakeProc, launch.Launch) {
	t.Helper()
	cfg := testConfig(t)
	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"))
	sys.mainPIDs["forge-a1"] = 4242
	svc := cfg.Modes["gemma"].Services[0]
	want, err := launch.BuildService(cfg, "a1", svc)
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakeProc{
		cmd: map[uint32][]string{4242: append([]string{want.Exe}, want.Argv...)},
		env: map[uint32]map[string]string{4242: {"PATH": "/usr/bin"}},
	}
	// The slot's last row, as the load that started the process wrote it; disarmed (what a daemon restart finds when the old row was disarmed, or the
	// only evidence of which config the process runs). Adoption names the config from it and then proves it against /proc.
	db := openLaunchStore(t)
	seedAdoptRow(t, db, cfg, "gemma")
	return cfg, sys, db, fp, want
}

// seedAdoptRow writes slot a1's row for the named config and disarms it (generation 1, armed = 0).
func seedAdoptRow(t *testing.T, db *store.DB, cfg *config.Config, name string) {
	t.Helper()
	row := store.SlotLaunch{Slot: "a1", ConfigName: name, EngineKind: "llamacpp", Exe: "/bin/old", Port: 8080, WrittenBy: "engine"}
	if _, err := db.SlotLaunches().Put(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SlotLaunches().Disarm(context.Background(), "a1", 1); err != nil {
		t.Fatal(err)
	}
}

func armedRows(t *testing.T, db *store.DB) int {
	t.Helper()
	rows, err := db.SlotLaunches().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.Armed {
			n++
		}
	}
	return n
}

func TestAdoptWritesRowOnExactMatch(t *testing.T) {
	cfg, sys, db, fp, want := adoptFixture(t)
	newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), fp)
	r, err := db.SlotLaunches().Get(context.Background(), "a1")
	if err != nil {
		t.Fatalf("no adopted row: %v", err)
	}
	if r.Exe != want.Exe || !reflect.DeepEqual(r.Argv, want.Argv) || !r.Armed || r.Generation != 2 || r.ConfigName != "gemma" || r.WrittenBy != "engine-adopt" {
		t.Errorf("adopted row = %+v", r)
	}
	if rows, _ := db.SlotLaunches().List(context.Background()); len(rows) != 1 {
		t.Errorf("rows = %d, want 1 (inactive slots get nothing)", len(rows))
	}
}

func TestAdoptMismatchWritesNothingAndNamesIndex(t *testing.T) {
	cfg, sys, db, fp, _ := adoptFixture(t)
	fp.cmd[4242][5] = "SECRET-LOOKING-VALUE" // argv index 4 (cmdline[0] is the exe)
	var mu sync.Mutex
	var logs []string
	m := newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), fp)
	m.d.Logf = func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }
	m.adoptRunningSlots(context.Background())
	if n := armedRows(t, db); n != 0 {
		t.Fatalf("row armed on mismatch: %d", n)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "argument 4 differs") || strings.Contains(joined, "SECRET-LOOKING-VALUE") {
		t.Errorf("log = %q", joined)
	}
}

func TestCompareLive(t *testing.T) {
	cases := []struct {
		name string
		want launch.Launch
		cmd  []string
		env  map[string]string
		sub  string // "" = match
	}{
		{"match", launch.Launch{Exe: "/x", Argv: []string{"a"}, Env: map[string]string{"K": "v"}}, []string{"/x", "a"}, map[string]string{"K": "v", "Z": "1"}, ""},
		{"extra arg", launch.Launch{Exe: "/x", Argv: []string{"a"}}, []string{"/x", "a", "b"}, nil, "count"},
		{"missing arg", launch.Launch{Exe: "/x", Argv: []string{"a", "b"}}, []string{"/x", "a"}, nil, "count"},
		{"exe", launch.Launch{Exe: "/x"}, []string{"/y"}, nil, "executable"},
		{"env missing", launch.Launch{Exe: "/x", Env: map[string]string{"K": "v"}}, []string{"/x"}, map[string]string{}, "K is missing"},
		{"env differs", launch.Launch{Exe: "/x", Env: map[string]string{"K": "v"}}, []string{"/x"}, map[string]string{"K": "w"}, "K differs"},
	}
	for _, tc := range cases {
		got := compareLive(tc.want, tc.cmd, tc.env)
		if tc.sub == "" && got != "" || tc.sub != "" && !strings.Contains(got, tc.sub) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.sub)
		}
	}
}

func TestAdoptSkipsInactiveUnitAndArmedRow(t *testing.T) {
	cfg, sys, db, fp, _ := adoptFixture(t)
	sys.setSeq("forge-a1", st("inactive", "dead"))
	newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), fp)
	if n := armedRows(t, db); n != 0 {
		t.Errorf("adopted an inactive unit: %d armed rows", n)
	}

	// Active, row already armed: untouched (generation stays 1).
	cfg, sys, db, fp, _ = adoptFixture(t)
	if err := db.SlotLaunches().Delete(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	pre := store.SlotLaunch{Slot: "a1", ConfigName: "keep", EngineKind: "llama.cpp", Exe: "/bin/keep", Port: 8080}
	if _, err := db.SlotLaunches().Put(context.Background(), pre); err != nil {
		t.Fatal(err)
	}
	newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), fp)
	r, _ := db.SlotLaunches().Get(context.Background(), "a1")
	if r.ConfigName != "keep" || r.Generation != 1 {
		t.Errorf("armed row replaced: %+v", r)
	}

	// Active, row present but disarmed and naming a known config: adopted (a new generation).
	if err := db.SlotLaunches().Delete(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	seedAdoptRow(t, db, cfg, "gemma")
	sys.setSeq("forge-a1", st("active", "running"))
	newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), fp)
	r, _ = db.SlotLaunches().Get(context.Background(), "a1")
	if r.ConfigName != "gemma" || !r.Armed || r.Generation != 2 {
		t.Errorf("disarmed row not adopted: %+v", r)
	}
}

func TestAdoptUnavailableProcDoesNothing(t *testing.T) {
	cfg, sys, db, fp, _ := adoptFixture(t)
	fp.err = ErrProcUnavailable
	newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), fp)
	if n := armedRows(t, db); n != 0 {
		t.Errorf("rows armed with /proc unavailable: %d", n)
	}
}

func TestAdoptUnidentifiableConfigWritesNothing(t *testing.T) {
	cfg, sys, db, fp, _ := adoptFixture(t)
	// A row naming a config the catalog no longer has, and a slot with no row at all, are both unidentifiable.
	if err := db.SlotLaunches().Delete(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	seedAdoptRow(t, db, cfg, "nobody")
	newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), fp)
	if n := armedRows(t, db); n != 0 {
		t.Errorf("adopted an unknown config name")
	}
	if err := db.SlotLaunches().Delete(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), fp)
	if rows, _ := db.SlotLaunches().List(context.Background()); len(rows) != 0 {
		t.Errorf("adopted a running unit that has no row at all")
	}
}

func TestHostProcReadsCmdlineAndEnviron(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/bin/x\x00--a\x00\x00--b\x00"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "environ"), []byte("K=v=1\x00Z=\x00"), 0o644)
	p := NewHostProc(root)
	got, err := p.Cmdline(7)
	if err != nil || !reflect.DeepEqual(got, []string{"/bin/x", "--a", "", "--b"}) {
		t.Errorf("cmdline = %q, %v", got, err)
	}
	env, err := p.Environ(7)
	if err != nil || env["K"] != "v=1" || env["Z"] != "" {
		t.Errorf("environ = %v, %v", env, err)
	}
	if _, err := p.Cmdline(8); err == nil {
		t.Error("missing pid should error")
	}
}

// ── shadow mode (SlotLaunchEnforce=false): row problems never block a load ──

func shadowManager(t *testing.T, cfg *config.Config, sys *fakeSys, ss SlotLaunchStore) (*Manager, *[]string) {
	t.Helper()
	m := newLaunchManager(t, cfg, sys, newLlamaStub(t, 131072), ss, &fakeProc{})
	m.d.SlotLaunchEnforce = false
	var got []string
	m.d.OnSlotLaunchWarning = func(slot, mode, msg string) { got = append(got, slot+"/"+mode+": "+msg) }
	return m, &got
}

func TestShadowBuildFailureStillLoadsAndWarns(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	db := openLaunchStore(t)
	m, warns := shadowManager(t, cfg, sys, db.SlotLaunches())
	good := config.Service{Model: "gemma.gguf", Alias: "gemma", Context: 8192, Backend: "vulkan"}
	if err := wsl(m, "a1", "gemma", 1, good); err != nil {
		t.Fatal(err)
	}
	cfg.Modes["bad"] = config.Mode{Services: []config.Service{{Model: "m.gguf", Alias: "m", Context: 1, Backend: "bogus-backend", PortRole: "a1"}}}
	sys.setSeq("forge-a1", st("inactive", "dead"), st("activating", "start"), st("active", "running"))
	res := m.Load(context.Background(), "bad", "a1")
	if !res.Success {
		t.Fatalf("shadow mode must not block the load: %+v", res)
	}
	if !strings.Contains(res.Detail, "slot launch warning") || !strings.Contains(res.Detail, "slot=a1 config=bad") {
		t.Errorf("Detail = %q", res.Detail)
	}
	r, _ := db.SlotLaunches().Get(context.Background(), "a1")
	if r.Armed || r.ConfigName != "gemma" {
		t.Errorf("stale row must be disarmed and unchanged: %+v", r)
	}
	if m.SlotLaunchWarnings() != 1 || len(*warns) != 1 {
		t.Errorf("warnings counter=%d callbacks=%v", m.SlotLaunchWarnings(), *warns)
	}
}

func TestShadowRowWriteFailureStillLoadsAndWarns(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	db := openLaunchStore(t)
	hs := &hookedStore{SlotLaunchStore: db.SlotLaunches()}
	m, warns := shadowManager(t, cfg, sys, hs)
	old := config.Service{Model: "qwen.gguf", Alias: "qwen", Context: 8192, Backend: "vulkan"}
	if err := wsl(m, "a1", "qwen", 1, old); err != nil {
		t.Fatal(err)
	}
	hs.putErr = errors.New("disk full")
	sys.setSeq("forge-a1", st("inactive", "dead"), st("activating", "start"), st("active", "running"))
	res := m.Load(context.Background(), "gemma", "a1")
	if !res.Success || !strings.Contains(res.Detail, "disk full") {
		t.Fatalf("res = %+v", res)
	}
	if r, _ := db.SlotLaunches().Get(context.Background(), "a1"); r.Armed || r.ConfigName != "qwen" {
		t.Errorf("row = %+v, want old content, disarmed", r)
	}
	if m.SlotLaunchWarnings() != 1 || len(*warns) != 1 {
		t.Errorf("counter=%d callbacks=%v", m.SlotLaunchWarnings(), *warns)
	}
	// If the disarm fails too, the warning says so and the load still goes ahead.
	hs.disarmErr = errors.New("locked")
	if _, err := db.SlotLaunches().Put(context.Background(), store.SlotLaunch{Slot: "a1", ConfigName: "x", EngineKind: "llamacpp", Exe: "/bin/x", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	sys.setSeq("forge-a1", st("inactive", "dead"), st("activating", "start"), st("active", "running"))
	if res := m.Load(context.Background(), "qwen", "a1"); !res.Success || !strings.Contains(res.Detail, "also not disarmed") {
		t.Errorf("res = %+v", res)
	}
}

// The enforce flag defaults to false in Deps (shadow) and the success path is unaffected by it.
func TestShadowSuccessHasNoDetail(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	db := openLaunchStore(t)
	m, _ := shadowManager(t, cfg, sys, db.SlotLaunches())
	sys.setSeq("forge-a1", st("inactive", "dead"), st("activating", "start"), st("active", "running"))
	if res := m.Load(context.Background(), "gemma", "a1"); !res.Success || res.Detail != "" {
		t.Errorf("res = %+v", res)
	}
}
