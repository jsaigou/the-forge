// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeSys is a scriptable Systemd: each State() call consumes the next
// state in the unit's sequence (the last one sticks).
type fakeSys struct {
	mu       sync.Mutex
	seq      map[string][]collector.UnitState
	mainPIDs map[string]uint32
	started  []string
	stopped  []string
	startErr error
	onStart  func(unit string)
	onStop   func(unit string)
}

func newFakeSys() *fakeSys {
	return &fakeSys{seq: map[string][]collector.UnitState{}, mainPIDs: map[string]uint32{}}
}

func st(active, sub string) collector.UnitState {
	return collector.UnitState{ActiveState: active, SubState: sub}
}

func (f *fakeSys) setSeq(unit string, states ...collector.UnitState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq[unit] = states
}

func (f *fakeSys) State(_ context.Context, unit string) (collector.UnitState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.seq[unit]
	if len(seq) == 0 {
		return st("inactive", "dead"), nil
	}
	head := seq[0]
	if len(seq) > 1 {
		f.seq[unit] = seq[1:]
	}
	return head, nil
}

func (f *fakeSys) MainPID(_ context.Context, unit string) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mainPIDs[unit], nil
}

func (f *fakeSys) Start(_ context.Context, unit string) error {
	f.mu.Lock()
	f.started = append(f.started, unit)
	cb := f.onStart
	err := f.startErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if cb != nil {
		cb(unit)
	}
	return nil
}

func (f *fakeSys) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, unit)
	cb := f.onStop
	f.mu.Unlock()
	if cb != nil {
		cb(unit)
	}
	return nil
}

// fakeUsage records history entries and usage events.
type fakeUsage struct {
	mu      sync.Mutex
	history []store.ModeHistoryEntry
	events  []store.UsageEvent
}

func (f *fakeUsage) Record(_ context.Context, e store.UsageEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}
func (f *fakeUsage) Events(context.Context, time.Time, int) ([]store.UsageEvent, error) {
	return nil, nil
}
func (f *fakeUsage) RecordHistory(_ context.Context, h store.ModeHistoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history = append(f.history, h)
	return nil
}
func (f *fakeUsage) History(context.Context, string, int) ([]store.ModeHistoryEntry, error) {
	return nil, nil
}
func (f *fakeUsage) TokenActivity(context.Context, time.Time) ([]store.TokenActivityRow, error) {
	return nil, nil
}

// llamaStub serves /health and /props with a configurable n_ctx.
type llamaStub struct {
	mu   sync.Mutex
	nctx int
	srv  *httptest.Server
}

func newLlamaStub(t *testing.T, nctx int) *llamaStub {
	s := &llamaStub{nctx: nctx}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/props":
			fmt.Fprintf(w, `{"default_generation_settings":{"n_ctx":%d}}`, s.nctx)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Paths: config.Paths{
			SysconfigDir: t.TempDir(),
			ModelsDir:    t.TempDir(),
			StateDir:     t.TempDir(),
		},
		Slots: map[string]config.Slot{
			"a1":   {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
			"a2": {Unit: "forge-a2", Port: 8081, Label: "A2", Order: 2},
			"a3":        {Unit: "forge-a3", Port: 8087, Label: "A3", Order: 3},
			"a4":        {Unit: "forge-a4", Port: 8088, Label: "A4", Order: 4},
		},
		Modes: map[string]config.Mode{
			"unloaded": {},
			"gemma": {Services: []config.Service{{
				Model: "gemma.gguf", Alias: "gemma", Context: 131072,
				PortRole: "a1", Backend: "vulkan", StartupTimeoutS: 120,
			}}},
			"qwen": {Services: []config.Service{{
				Model: "qwen.gguf", Alias: "qwen", Context: 32768,
				PortRole: "a1", Backend: "vulkan", StartupTimeoutS: 120,
			}}},
			"nemotron": {Services: []config.Service{{
				Model: "nemotron.gguf", Alias: "nemotron", Context: 131072,
				PortRole: "a1", Backend: "rocm", StartupTimeoutS: 120,
			}}},
		},
		Monitor: config.Monitor{
			PollIntervalS: 4, HangTPSThousand: 100, HangSustainS: 90, SwitchCooldownS: 120, GTTWarnPct: 85,
		},
	}
}

type notifyCounter struct{ n int }

func (n *notifyCounter) NotifySwitchComplete() { n.n++ }

func newTestManager(t *testing.T, cfg *config.Config, sys *fakeSys, stub *llamaStub) (*Manager, *fakeUsage, *notifyCounter) {
	t.Helper()
	usage := &fakeUsage{}
	notify := &notifyCounter{}
	baseURL := func(port int) string { return "http://127.0.0.1:1" }
	if stub != nil {
		baseURL = func(port int) string { return stub.srv.URL }
	}
	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          sys,
		GPU:          &collector.GPU{DRMRoot: t.TempDir()},
		Proc:         collector.Proc{Root: t.TempDir()},
		Usage:        usage,
		Notify:       notify,
		BaseURL:      baseURL,
		Kill:         func(pid int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
		ReadMeta: func(path string) (gguf.Metadata, error) {
			return gguf.Metadata{TrainedCtx: 131072}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, usage, notify
}

func writeSlotEnv(t *testing.T, cfg *config.Config, slot, alias, model string) {
	t.Helper()
	content := "FORGE_MODEL_PATH=" + filepath.Join(cfg.Paths.ModelsDir, model) + "\n" +
		"FORGE_MODEL_ALIAS=" + alias + "\n"
	path := filepath.Join(cfg.Paths.SysconfigDir, "forge-"+slot+"-env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ── Aux unit control (C1-Q2 amendment: StartUnit/StopUnit) ──

func TestStartStopUnitAuxService(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)

	if err := m.StartUnit(context.Background(), "forge-tts"); err != nil {
		t.Fatalf("StartUnit: %v", err)
	}
	if err := m.StopUnit(context.Background(), "forge-tts"); err != nil {
		t.Fatalf("StopUnit: %v", err)
	}
	if len(sys.started) != 1 || sys.started[0] != "forge-tts" {
		t.Errorf("started = %v, want [forge-tts]", sys.started)
	}
	if len(sys.stopped) != 1 || sys.stopped[0] != "forge-tts" {
		t.Errorf("stopped = %v, want [forge-tts]", sys.stopped)
	}
}

func TestStartUnitRefusesInferenceSlot(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)

	if err := m.StartUnit(context.Background(), "forge-a1"); err == nil {
		t.Fatal("StartUnit must refuse an inference-slot unit")
	}
	if err := m.StopUnit(context.Background(), "forge-a4"); err == nil {
		t.Fatal("StopUnit must refuse an inference-slot unit")
	}
	if len(sys.started) != 0 || len(sys.stopped) != 0 {
		t.Errorf("slot units must never reach the systemd adapter: started=%v stopped=%v", sys.started, sys.stopped)
	}
}

func TestStartUnitRejectsEmpty(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)
	if err := m.StartUnit(context.Background(), ""); err == nil {
		t.Fatal("StartUnit must reject an empty unit name")
	}
}

// ── Crown jewel: slot state must NOT clear while the unit is deactivating ──

func TestSlotStateNeverClearsDuringDeactivating(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)
	m.setSlotMode("a1", "nemotron")

	cases := []struct {
		name     string
		state    collector.UnitState
		wantMode string
	}{
		{"active keeps mode", st("active", "running"), "nemotron"},
		{"deactivating keeps mode (TimeoutStopSec=300)", st("deactivating", "stop-sigterm"), "nemotron"},
		{"deactivating final-sigkill keeps mode", st("deactivating", "final-sigkill"), "nemotron"},
		{"activating keeps mode (restart in flight)", st("activating", "start"), "nemotron"},
		{"inactive clears", st("inactive", "dead"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m.setSlotMode("a1", "nemotron")
			got := m.SlotStates(map[string]collector.UnitState{"forge-a1": tc.state})
			if got["a1"].Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", got["a1"].Mode, tc.wantMode)
			}
		})
	}
}

func TestSlotStateFailedClears(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)
	m.setSlotMode("a3", "qwen")
	got := m.SlotStates(map[string]collector.UnitState{"forge-a3": st("failed", "failed")})
	if got["a3"].Mode != "" {
		t.Errorf("failed unit must clear slot state, got %q", got["a3"].Mode)
	}
}

// Active-but-untracked slots get their mode inferred from sysconfig env
// (restart recovery without slots.json).
func TestSlotStateInfersFromEnv(t *testing.T) {
	cfg := testConfig(t)
	writeSlotEnv(t, cfg, "a4", "qwen", "qwen.gguf")
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)
	got := m.SlotStates(map[string]collector.UnitState{"forge-a4": st("active", "running")})
	if got["a4"].Mode != "qwen" {
		t.Errorf("inferred mode = %q, want qwen", got["a4"].Mode)
	}
}

// Unknown (unprobed) unit state must neither infer nor clear.
func TestSlotStateUnknownUnitKeepsMode(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)
	m.setSlotMode("a1", "gemma")
	got := m.SlotStates(map[string]collector.UnitState{}) // no data at all
	if got["a1"].Mode != "gemma" {
		t.Errorf("unknown unit state must keep mode, got %q", got["a1"].Mode)
	}
}

// ── Crown jewel: no load placed into a slot whose old process hasn't exited ──

func TestLoadWaitsOutDeactivatingSlot(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	stub := newLlamaStub(t, 131072)
	m, _, _ := newTestManager(t, cfg, sys, stub)
	// Initial check: deactivating. Wait loop sees deactivating twice more,
	// then the unit dies; the load may then proceed.
	sys.setSeq("forge-a1",
		st("deactivating", "stop-sigterm"),
		st("deactivating", "stop-sigterm"),
		st("deactivating", "stop-sigterm"),
		st("inactive", "dead"),
		st("active", "running"), // post-start wait sees running
	)

	res := m.Load(context.Background(), "gemma", "a1")
	if !res.Success {
		t.Fatalf("load failed: %s", res.Message)
	}
	if len(sys.started) != 1 || sys.started[0] != "forge-a1" {
		t.Errorf("started = %v", sys.started)
	}
	if res.NCtx != 131072 {
		t.Errorf("NCtx = %d", res.NCtx)
	}
}

func TestLoadRefusesForeverDeactivatingSlot(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)
	sys.setSeq("forge-a1", st("deactivating", "stop-sigterm")) // sticks forever

	res := m.Load(context.Background(), "gemma", "a1")
	if res.Success {
		t.Fatal("load into a deactivating slot must fail")
	}
	if len(sys.started) != 0 {
		t.Errorf("unit must not be started: %v", sys.started)
	}
}

// ── Crown jewel: n_ctx verified via /props and recorded ──

func TestLoadVerifiesAndRecordsNCtx(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	stub := newLlamaStub(t, 131072)
	m, usage, notify := newTestManager(t, cfg, sys, stub)
	sys.setSeq("forge-a1",
		st("inactive", "dead"),    // pre-check: not active
		st("activating", "start"), // first running-wait poll
		st("active", "running"),
	)

	res := m.Load(context.Background(), "gemma", "a1")
	if !res.Success || res.NCtx != 131072 {
		t.Fatalf("res = %+v", res)
	}
	if len(usage.history) != 1 {
		t.Fatalf("history entries = %d", len(usage.history))
	}
	h := usage.history[0]
	if h.TrainedCtx != 131072 || h.ConfiguredCtx != 131072 || h.ActualCtx != 131072 || h.Result != "ok" {
		t.Errorf("history = %+v", h)
	}
	if notify.n != 1 {
		t.Errorf("switch-complete notifications = %d", notify.n)
	}
	// Slot record updated.
	got := m.SlotStates(map[string]collector.UnitState{"forge-a1": st("active", "running")})
	if got["a1"].Mode != "gemma" {
		t.Errorf("slot mode = %q", got["a1"].Mode)
	}
}

// Silent context reduction (kernel GTT allocation failure) must be recorded
// as ctx_reduced with the ACTUAL value — never trusted from config.
func TestLoadRecordsSilentContextReduction(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	stub := newLlamaStub(t, 65536) // half of the configured 131072
	m, usage, _ := newTestManager(t, cfg, sys, stub)
	sys.setSeq("forge-a1",
		st("inactive", "dead"),
		st("active", "running"),
	)

	res := m.Load(context.Background(), "gemma", "a1")
	if !res.Success {
		t.Fatalf("load reported failure: %s", res.Message)
	}
	if res.NCtx != 65536 {
		t.Errorf("NCtx = %d, want the ACTUAL 65536", res.NCtx)
	}
	h := usage.history[0]
	if h.Result != "ctx_reduced" || h.ActualCtx != 65536 || h.ConfiguredCtx != 131072 {
		t.Errorf("history = %+v", h)
	}
}

func TestLoadRecordsCtxExceedsTrained(t *testing.T) {
	cfg := testConfig(t)
	mode := cfg.Modes["gemma"]
	mode.Services[0].Context = 200000 // above trained 131072
	cfg.Modes["gemma"] = mode

	sys := newFakeSys()
	stub := newLlamaStub(t, 200000)
	m, usage, _ := newTestManager(t, cfg, sys, stub)
	sys.setSeq("forge-a1", st("inactive", "dead"), st("active", "running"))

	if res := m.Load(context.Background(), "gemma", "a1"); !res.Success {
		t.Fatalf("load failed: %s", res.Message)
	}
	if usage.history[0].Result != "ctx_exceeds_trained" {
		t.Errorf("result = %q", usage.history[0].Result)
	}
}

func TestLoadFailureStopsUnitAndRecords(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, usage, _ := newTestManager(t, cfg, sys, nil)
	sys.setSeq("forge-a1",
		st("inactive", "dead"),
		st("activating", "start"),
		st("failed", "failed"), // crashes during startup
	)

	res := m.Load(context.Background(), "gemma", "a1")
	if res.Success {
		t.Fatal("expected failure")
	}
	if len(usage.history) != 1 || usage.history[0].Result != "failed" {
		t.Errorf("history = %+v", usage.history)
	}
	got := m.SlotStates(map[string]collector.UnitState{"forge-a1": st("inactive", "dead")})
	if got["a1"].Mode != "" {
		t.Errorf("failed load must not set slot mode, got %q", got["a1"].Mode)
	}
}

// ── Unload waits out deactivating before freeing the slot ──

func TestUnloadWaitsForDeactivating(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, usage, _ := newTestManager(t, cfg, sys, nil)
	sys.setSeq("forge-a3",
		st("deactivating", "stop-sigterm"),
		st("deactivating", "stop-sigterm"),
		st("inactive", "dead"),
	)
	m.setSlotMode("a3", "qwen")

	res := m.Unload(context.Background(), "a3")
	if !res.Success {
		t.Fatalf("unload failed: %s", res.Message)
	}
	if len(sys.stopped) != 1 || sys.stopped[0] != "forge-a3" {
		t.Errorf("stopped = %v", sys.stopped)
	}
	got := m.SlotStates(map[string]collector.UnitState{"forge-a3": st("inactive", "dead")})
	if got["a3"].Mode != "" {
		t.Errorf("slot mode = %q, want cleared", got["a3"].Mode)
	}
	foundUnload := false
	for _, e := range usage.events {
		if e.Kind == "unload" && e.Slot == "a3" {
			foundUnload = true
		}
	}
	if !foundUnload {
		t.Errorf("unload event missing: %+v", usage.events)
	}
}

func TestUnloadDoesNotFreeSlotStuckDeactivating(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)
	sys.setSeq("forge-a3", st("deactivating", "stop-sigterm")) // never finishes
	m.setSlotMode("a3", "nemotron")

	res := m.Unload(context.Background(), "a3")
	if res.Success {
		t.Fatal("unload must not report success while unit is deactivating")
	}
	got := m.SlotStates(map[string]collector.UnitState{"forge-a3": st("deactivating", "stop-sigterm")})
	if got["a3"].Mode != "nemotron" {
		t.Errorf("slot must stay occupied, got %q", got["a3"].Mode)
	}
}

// ── SwitchMode ──

func TestSwitchModeFullFlow(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	stub := newLlamaStub(t, 131072)
	// All four slot units are stopped by stopAll (default inactive/dead).
	// After start, the a1 unit comes up.
	sys.onStart = func(unit string) {
		sys.setSeq(unit, st("active", "running"))
	}
	m, usage, notify := newTestManager(t, cfg, sys, stub)

	res := m.SwitchMode(context.Background(), "gemma")
	if !res.Success {
		t.Fatalf("switch failed: %s", res.Message)
	}
	if res.NCtx != 131072 {
		t.Errorf("NCtx = %d", res.NCtx)
	}
	// All canonical units stopped first.
	if len(sys.stopped) != 4 {
		t.Errorf("stopped = %v", sys.stopped)
	}
	if len(sys.started) != 1 || sys.started[0] != "forge-a1" {
		t.Errorf("started = %v", sys.started)
	}
	// Env file written with FORGE_ keys.
	env := collector.ReadSlotEnv(cfg.Paths.SysconfigDir, "a1")
	if env["FORGE_MODEL_ALIAS"] != "gemma" || env["FORGE_BACKEND"] != "vulkan" {
		t.Errorf("env = %+v", env)
	}
	if env["FORGE_CONTEXT"] != "131072" {
		t.Errorf("FORGE_CONTEXT = %q", env["FORGE_CONTEXT"])
	}
	// State file records the mode.
	if m.CurrentMode() != "gemma" {
		t.Errorf("CurrentMode = %q", m.CurrentMode())
	}
	if notify.n != 1 {
		t.Errorf("notifications = %d", notify.n)
	}
	if len(usage.history) != 1 || usage.history[0].Result != "ok" {
		t.Errorf("history = %+v", usage.history)
	}
}

func TestSwitchModeUnknown(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)
	res := m.SwitchMode(context.Background(), "nope")
	if res.Success {
		t.Fatal("unknown mode must fail")
	}
}

func TestSwitchModeToUnloaded(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)
	res := m.SwitchMode(context.Background(), "unloaded")
	if !res.Success {
		t.Fatalf("unloaded switch failed: %s", res.Message)
	}
	if len(sys.started) != 0 {
		t.Errorf("nothing should start: %v", sys.started)
	}
	if m.CurrentMode() != "unloaded" {
		t.Errorf("CurrentMode = %q", m.CurrentMode())
	}
}

// Already-in-mode with services healthy short-circuits without stopping.
func TestSwitchModeAlreadyActive(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	stub := newLlamaStub(t, 131072)
	sys.onStart = func(unit string) { sys.setSeq(unit, st("active", "running")) }
	m, _, _ := newTestManager(t, cfg, sys, stub)

	if res := m.SwitchMode(context.Background(), "gemma"); !res.Success {
		t.Fatalf("first switch failed: %s", res.Message)
	}
	sys.mu.Lock()
	sys.stopped = nil
	sys.started = nil
	sys.mu.Unlock()

	res := m.SwitchMode(context.Background(), "gemma")
	if !res.Success || res.Message != "Already in gemma mode" {
		t.Fatalf("res = %+v", res)
	}
	if len(sys.stopped) != 0 || len(sys.started) != 0 {
		t.Errorf("short-circuit must not touch units: stopped=%v started=%v", sys.stopped, sys.started)
	}
}

func TestSwitchModeStartFailure(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	sys.startErr = fmt.Errorf("unit not found")
	m, usage, _ := newTestManager(t, cfg, sys, nil)

	res := m.SwitchMode(context.Background(), "gemma")
	if res.Success {
		t.Fatal("expected failure")
	}
	if len(usage.history) == 0 || usage.history[0].Result != "failed" {
		t.Errorf("history = %+v", usage.history)
	}
}

// ── CurrentMode state-file semantics ──

func TestCurrentModeTrustsStateFileWhenConsistent(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)
	m.saveMode("gemma")
	sys.setSeq("forge-a1", st("active", "running"))
	if got := m.CurrentMode(); got != "gemma" {
		t.Errorf("CurrentMode = %q", got)
	}
}

func TestCurrentModeDetectsDrift(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)
	m.saveMode("gemma")
	// gemma expects a1 active; but only a3 is active — drift. No mode
	// matches {forge-a3}... qwen expects a1 too. Falls back to the
	// recorded name (V4 parity).
	sys.setSeq("forge-a1", st("inactive", "dead"))
	sys.setSeq("forge-a3", st("active", "running"))
	if got := m.CurrentMode(); got != "gemma" {
		t.Errorf("CurrentMode = %q (recorded fallback)", got)
	}
}

func TestCurrentModeUnknownWithoutStateFile(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)
	// No state file, nothing active: no non-service mode with zero units
	// exists except "unloaded" (empty services) — which matches the empty
	// active set, V4 parity.
	if got := m.CurrentMode(); got != "unloaded" {
		t.Errorf("CurrentMode = %q", got)
	}
}

// ── Sysconfig writing ──

func TestWriteServiceFilesPreservesNonForgeKeys(t *testing.T) {
	cfg := testConfig(t)
	envPath := filepath.Join(cfg.Paths.SysconfigDir, "forge-a1-env")
	seed := "FORGE_MODEL_PATH=/old/model.gguf\n" +
		"GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON\n" +
		"LD_LIBRARY_PATH=/opt/rocm-therock-7.13/lib\n" +
		"HSA_OVERRIDE_GFX_VERSION=11.5.1\n"
	if err := os.WriteFile(envPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)

	svc := cfg.Modes["nemotron"].Services[0]
	svc.PortRole = "a1"
	svc.ExtraArgs = []string{"--parallel", "1", "--ctx-checkpoints", "0"}
	if err := m.writeServiceFiles("a1", svc); err != nil {
		t.Fatal(err)
	}

	env := collector.ReadSlotEnv(cfg.Paths.SysconfigDir, "a1")
	if env["GGML_CUDA_ENABLE_UNIFIED_MEMORY"] != "ON" {
		t.Error("GGML_CUDA_ENABLE_UNIFIED_MEMORY lost — ROCm capped at ~63GB without it")
	}
	if env["LD_LIBRARY_PATH"] != "/opt/rocm-therock-7.13/lib" || env["HSA_OVERRIDE_GFX_VERSION"] != "11.5.1" {
		t.Errorf("preserved keys lost: %+v", env)
	}
	if env["FORGE_MODEL_PATH"] != filepath.Join(cfg.Paths.ModelsDir, "nemotron.gguf") {
		t.Errorf("FORGE_MODEL_PATH = %q", env["FORGE_MODEL_PATH"])
	}
	if env["FORGE_BACKEND"] != "rocm" {
		t.Errorf("FORGE_BACKEND = %q", env["FORGE_BACKEND"])
	}

	args, err := os.ReadFile(filepath.Join(cfg.Paths.SysconfigDir, "forge-a1-args"))
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "--parallel\n1\n--ctx-checkpoints\n0\n" {
		t.Errorf("args = %q", string(args))
	}
}

// FORGE_LLAMA_BIN is written only when the service sets a per-service
// override (custom builds like kintsugi/puzzle ahead of upstream llama.cpp);
// when empty the launcher must fall back to its FORGE_BACKEND default, so
// the key must be absent rather than blank.
func TestWriteServiceFilesLlamaBinOverride(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)

	svc := cfg.Modes["gemma"].Services[0]
	svc.PortRole = "a1"

	// No override → key absent.
	if err := m.writeServiceFiles("a1", svc); err != nil {
		t.Fatal(err)
	}
	if _, ok := collector.ReadSlotEnv(cfg.Paths.SysconfigDir, "a1")["FORGE_LLAMA_BIN"]; ok {
		t.Error("FORGE_LLAMA_BIN should be absent when svc.LlamaBin is empty")
	}

	// Override set → key written verbatim.
	svc.LlamaBin = "/opt/forge/llama.cpp-kintsugi/build-vulkan/bin/llama-server"
	if err := m.writeServiceFiles("a1", svc); err != nil {
		t.Fatal(err)
	}
	if got := collector.ReadSlotEnv(cfg.Paths.SysconfigDir, "a1")["FORGE_LLAMA_BIN"]; got != svc.LlamaBin {
		t.Errorf("FORGE_LLAMA_BIN = %q, want %q", got, svc.LlamaBin)
	}
}

// ── Eviction planning: smallest footprint first ──

func TestFitPlanEvictsSmallestFirst(t *testing.T) {
	cfg := testConfig(t)

	// This test exercises smallest-footprint-first eviction ordering at a
	// synthetic MB scale (total budget 1000 MB). BE-2's interim KV-cache
	// estimate (docs/v5-review-fixes.md F3) would swamp that scale — the
	// shared testConfig nemotron mode carries a real-world Context of
	// 131072 — so it's zeroed here to isolate eviction ordering from ctx
	// sizing; the ctx-estimate math itself is covered by
	// TestFitPlanIncludesInterimContextEstimate below.
	nemotron := cfg.Modes["nemotron"]
	nemotronSvc := nemotron.Services[0]
	nemotronSvc.Context = 0
	nemotron.Services = []config.Service{nemotronSvc}
	cfg.Modes["nemotron"] = nemotron

	models := cfg.Paths.ModelsDir

	writeBytes := func(name string, mb int) {
		if err := os.WriteFile(filepath.Join(models, name), make([]byte, mb*1024*1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBytes("gemma.gguf", 2)    // loaded in a1 — small
	writeBytes("qwen.gguf", 8)     // loaded in a3 — bigger
	writeBytes("nemotron.gguf", 6) // the mode we want to place

	// GPU: total 1000 MB, used 995 MB → free 5 MB. nemotron needs 6 MB.
	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte(strconv.Itoa(995*1024*1024)+"\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte(strconv.Itoa(1000*1024*1024)+"\n"), 0o644)

	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"))
	sys.setSeq("forge-a3", st("active", "running"))
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf")
	writeSlotEnv(t, cfg, "a3", "qwen", "qwen.gguf")

	usage := &fakeUsage{}
	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          sys,
		GPU:          &collector.GPU{DRMRoot: gpuRoot},
		Proc:         collector.Proc{Root: t.TempDir()},
		Usage:        usage,
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := m.FitPlan("nemotron")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Fits {
		t.Fatalf("must not fit in 5 MB free: %+v", plan)
	}
	// Smallest first: gemma (2 MB) alone gives 5+2=7 ≥ 6 — qwen survives.
	if len(plan.Evict) != 1 || plan.Evict[0] != "a1" {
		t.Errorf("evict = %v, want [a1] (smallest footprint first)", plan.Evict)
	}
	if plan.Slot != "a1" {
		t.Errorf("slot = %q", plan.Slot)
	}

	// Contract adapter.
	fit, err := m.CanFit("nemotron")
	if err != nil {
		t.Fatal(err)
	}
	if fit.Fits || fit.Reason == "" {
		t.Errorf("CanFit = %+v", fit)
	}
}

// TestFitPlanCreditsLiveFootprintOnEviction pins the Sprint 6
// build_refresh eval finding (2026-08-20): eviction credit must use the
// evicted slot's LIVE measured GPU footprint, not its on-disk weight set.
// The eval's rocm canary failed exactly this way live — a resident canary
// with 16.8 GB of weights but a 52.9 GB runtime footprint was credited
// only its weights, and the fit check refused a load the eviction
// demonstrably made room for.
func TestFitPlanCreditsLiveFootprintOnEviction(t *testing.T) {
	cfg := testConfig(t)

	nemotron := cfg.Modes["nemotron"]
	nemotronSvc := nemotron.Services[0]
	nemotronSvc.Context = 0
	nemotron.Services = []config.Service{nemotronSvc}
	cfg.Modes["nemotron"] = nemotron

	models := cfg.Paths.ModelsDir
	writeBytes := func(name string, mb int) {
		if err := os.WriteFile(filepath.Join(models, name), make([]byte, mb*1024*1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBytes("gemma.gguf", 2)     // loaded in a1: tiny weights…
	writeBytes("nemotron.gguf", 60) // …the mode to place needs 60 MB

	// GPU: total 1000 MB, used 960 → free 40 MB. nemotron needs 60: it
	// fits only if evicting a1 credits the slot's REAL 50 MB footprint —
	// the weight-only credit (40+2 < 60) is the bug this test pins.
	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte(strconv.Itoa(960*1024*1024)+"\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte(strconv.Itoa(1000*1024*1024)+"\n"), 0o644)

	// a1's live process: fdinfo reports 50 MB of GTT for pid 4242.
	procRoot := t.TempDir()
	fdDir := filepath.Join(procRoot, "4242", "fdinfo")
	os.MkdirAll(fdDir, 0o755)
	os.WriteFile(filepath.Join(fdDir, "3"), []byte(
		"pos:\t0\nflags:\t02100002\nmnt_id:\t40\nino:\t694\n"+
			"drm-driver:\tamdgpu\ndrm-client-id:\t7\ndrm-pdev:\t0000:c5:00.0\n"+
			"drm-memory-vram:\t0 KiB\ndrm-memory-gtt: \t"+strconv.Itoa(50*1024)+" KiB\n"+
			"drm-memory-cpu: \t0 KiB\n"), 0o644)

	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"))
	sys.mainPIDs["forge-a1"] = 4242
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf")

	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          sys,
		GPU:          &collector.GPU{DRMRoot: gpuRoot},
		Proc:         collector.Proc{Root: procRoot},
		Usage:        &fakeUsage{},
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := m.FitPlan("nemotron")
	if err != nil {
		t.Fatal(err)
	}
	// Contract: Fits means "fits NOW"; an eviction plan that fits after
	// evicting is Fits:false + non-empty Evict + a landing Slot. With the
	// LIVE credit, evicting a1 frees its real 50 MB (40+50 ≥ 60). Under
	// the old weight-only credit (40+2 < 60) this landed in the terminal
	// "won't fit even after evicting every loaded slot" branch with an
	// EMPTY Evict list — that is the exact shape the live eval hit.
	if plan.Fits {
		t.Fatalf("plan = %+v, want Fits:false (doesn't fit without eviction)", plan)
	}
	if len(plan.Evict) != 1 || plan.Evict[0] != "a1" || plan.Slot != "a1" {
		t.Fatalf("plan = %+v, want eviction plan [a1] landing on a1 — evicting a1 frees its LIVE 50 MB (40 free + 50 ≥ 60 needed)", plan)
	}
	if got := m.SlotFootprintBytes("a1"); got != 50*1024*1024 {
		t.Errorf("SlotFootprintBytes(a1) = %d, want the live 50 MB, not the 2 MB weight set", got)
	}
}

// ── BE-2 (F3): fail-closed + interim context estimate ──

// TestFitPlanFailsClosedWhenSizeUndetermined is the core F3 regression test:
// before this fix, an undeterminable memory requirement (no curated
// memory_req_bytes, model file missing from disk) computed needBytes=0, and
// 0 <= free was always true — Fits:true unconditionally, regardless of how
// much free memory actually existed. That must now refuse instead of
// guessing, even with abundant free budget.
func TestFitPlanFailsClosedWhenSizeUndetermined(t *testing.T) {
	cfg := testConfig(t)
	// "qwen" references qwen.gguf, deliberately never written to
	// cfg.Paths.ModelsDir — modeWeightBytes resolves to 0, and no
	// Deps.MemoryReqBytes is wired (nil), so neither source can size it.

	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte("0\n"), 0o644)
	// Abundant free space — proves the refusal is about not knowing the
	// size, not about lacking room.
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte(strconv.Itoa(100_000*1024*1024)+"\n"), 0o644)

	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          newFakeSys(),
		GPU:          &collector.GPU{DRMRoot: gpuRoot},
		Proc:         collector.Proc{Root: t.TempDir()},
		Usage:        &fakeUsage{},
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := m.FitPlan("qwen")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Fits {
		t.Fatalf("must fail closed when size can't be derived, got Fits=true: %+v", plan)
	}
	if plan.NeedBytes != 0 {
		t.Errorf("NeedBytes = %v, want 0 (undetermined, not guessed)", plan.NeedBytes)
	}
	if plan.Message == "" {
		t.Error("Message must explain the refusal")
	}
	if len(plan.Evict) != 0 {
		t.Errorf("Evict = %v, want none — an undeterminable size has nothing sensible to evict for", plan.Evict)
	}

	fit, err := m.CanFit("qwen")
	if err != nil {
		t.Fatal(err)
	}
	if fit.Fits {
		t.Errorf("CanFit.Fits = true, want false")
	}
	if fit.Reason == "" {
		t.Error("CanFit.Reason must be set when refusing")
	}
	if fit.RequiredBytes != 0 {
		t.Errorf("RequiredBytes = %d, want 0", fit.RequiredBytes)
	}
}

// TestFitPlanAbsoluteModelPath is the regression test for a load bug found live
// 2026-07-25: a catalog Config whose weight Artifact.FilePath is an ABSOLUTE
// path (e.g. "/var/lib/forge/models/laguna-s-21-Q4_K_M.gguf" rather than
// the relative "laguna-s-21-Q4_K_M.gguf") was refused by FitPlan with
// "cannot determine memory requirement ... model weights not found on disk"
// even though the file existed and the registry's card path (which uses
// resolveArtifactPath) showed the correct size.
//
// Root cause: modeWeightBytes did filepath.Join(ModelsDir, svc.Model)
// unconditionally — for an absolute svc.Model, Join produces
// ModelsDir + "/abs/path" (a nonexistent path), so WeightSetSizeBytes
// returned 0 and the fail-closed branch (above) refused the load. The
// registry's resolveArtifactPath handles absolute paths correctly; the
// engine's path builders did not, so the card showed a memory figure
// while the fit check failed.
//
// This test reproduces the user-facing symptom ("Won't fit (needs 0 GB, X GB
// free). cannot determine memory requirement for ...") and pins the fix:
// absolute model paths must be stat'd as-is, not joined under ModelsDir.
func TestFitPlanAbsoluteModelPath(t *testing.T) {
	cfg := testConfig(t)

	// Create the weight file OUTSIDE cfg.Paths.ModelsDir, at an absolute
	// path — mirroring a catalog artifact whose file_path was stored
	// absolute (the case the registry's resolveArtifactPath was written
	// to handle, but the engine's modeWeightBytes was not).
	absDir := t.TempDir()
	absPath := filepath.Join(absDir, "laguna-s-21-Q4_K_M.gguf")
	if err := os.WriteFile(absPath, make([]byte, 5*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Modes["laguna-s-21-q4_k_m"] = config.Mode{
		Services: []config.Service{{
			Model:           absPath, // absolute — the bug
			Alias:           "laguna-s-21",
			Context:         32768,
			PortRole:        "a1",
			Backend:         "vulkan",
			StartupTimeoutS: 120,
		}},
	}

	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte("0\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte(strconv.Itoa(100_000*1024*1024)+"\n"), 0o644)

	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          newFakeSys(),
		GPU:          &collector.GPU{DRMRoot: gpuRoot},
		Proc:         collector.Proc{Root: t.TempDir()},
		Usage:        &fakeUsage{},
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := m.FitPlan("laguna-s-21-q4_k_m")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Fits {
		t.Fatalf("absolute model path must be stat'd as-is, not joined under ModelsDir — got refusal: %+v\nmessage: %s", plan, plan.Message)
	}
	if plan.NeedBytes != 5*1024*1024 {
		t.Errorf("NeedBytes = %v, want %d (the on-disk file size)", plan.NeedBytes, 5*1024*1024)
	}
}

// TestFitPlanWeightOnlySizingWhenUnprofiled verifies needBytes is weight-only
// for a mode with no PROFILE data — no per-token KV padding is added.
//
// This replaces the old interim-context-estimate behavior (F3/BE-2), removed
// 2026-07-25 after it was found live to inflate needBytes by ~24x for
// Nemotron's hybrid Mamba2/Attention modes (calibrated for dense/GQA
// attention, applied uniformly regardless of architecture): computed ~176 GB
// / ~219 GB needed for loads that actually use ~90 GB / ~108 GB, blocking
// loads that fit comfortably. An unprofiled mode is now sized on real weight
// bytes only; only a real PROFILE measurement (which already includes KV
// cache at max context) changes needBytes beyond that.
func TestFitPlanWeightOnlySizingWhenUnprofiled(t *testing.T) {
	cfg := testConfig(t)
	qwen := cfg.Modes["qwen"]
	qwenSvc := qwen.Services[0]
	qwenSvc.Context = 1_000_000 // large configured context must not affect sizing
	qwen.Services = []config.Service{qwenSvc}
	cfg.Modes["qwen"] = qwen

	if err := os.WriteFile(filepath.Join(cfg.Paths.ModelsDir, "qwen.gguf"), make([]byte, 5*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte("0\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte(strconv.Itoa(10*1024*1024)+"\n"), 0o644)

	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          newFakeSys(),
		GPU:          &collector.GPU{DRMRoot: gpuRoot},
		Proc:         collector.Proc{Root: t.TempDir()},
		Usage:        &fakeUsage{},
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := m.FitPlan("qwen")
	if err != nil {
		t.Fatal(err)
	}
	want := float64(5 * 1024 * 1024) // weight only — no per-token addition
	if plan.NeedBytes != want {
		t.Errorf("NeedBytes = %v, want %v (weight-only sizing regressed)", plan.NeedBytes, want)
	}
	if !plan.Fits {
		t.Errorf("expected to fit in the 10 MiB budget on weight alone: %+v", plan)
	}

	// Same check through the curated-registry path (Deps.WeightEstimateBytes).
	qwen.ConfigID = 1 // B2: set ConfigID so the engine can look it up
	cfg.Modes["qwen"] = qwen
	m2, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          newFakeSys(),
		GPU:          &collector.GPU{DRMRoot: gpuRoot},
		Proc:         collector.Proc{Root: t.TempDir()},
		Usage:        &fakeUsage{},
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
		WeightEstimateBytes: func(configID int64) (int64, bool) {
			if configID == 1 {
				return 2 * 1024 * 1024, true // 2 MiB curated
			}
			return 0, false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := m2.FitPlan("qwen")
	if err != nil {
		t.Fatal(err)
	}
	want2 := float64(2 * 1024 * 1024)
	if plan2.NeedBytes != want2 {
		t.Errorf("curated-path NeedBytes = %v, want %v", plan2.NeedBytes, want2)
	}
}

// TestFitPlanLargeContextDoesNotBlockUnprofiledLoad: a mode with a very
// large configured context (Nemotron-hybrid-shaped: small weight, huge
// context) must fit on weight size alone when unprofiled — a large context
// number is no longer treated as a proxy for a large KV-cache requirement.
// Regression guard for the 2026-07-25 nemotron/nemotron-puzzle incident.
func TestFitPlanLargeContextDoesNotBlockUnprofiledLoad(t *testing.T) {
	cfg := testConfig(t)
	qwen := cfg.Modes["qwen"]
	qwenSvc := qwen.Services[0]
	qwenSvc.Context = 1_048_576 // 1M, same as nemotron/nemotron-puzzle
	qwen.Services = []config.Service{qwenSvc}
	cfg.Modes["qwen"] = qwen

	if err := os.WriteFile(filepath.Join(cfg.Paths.ModelsDir, "qwen.gguf"), make([]byte, 5*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte("0\n"), 0o644)
	// Budget covers the 5 MB weight comfortably but is nowhere near enough
	// for the old ~131 MB (1_048_576 tokens * 0.125 MiB) interim guess.
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte(strconv.Itoa(10*1024*1024)+"\n"), 0o644)

	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          newFakeSys(),
		GPU:          &collector.GPU{DRMRoot: gpuRoot},
		Proc:         collector.Proc{Root: t.TempDir()},
		Usage:        &fakeUsage{},
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := m.FitPlan("qwen")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Fits {
		t.Fatalf("large configured context must not block an unprofiled load that fits on weight alone: %+v", plan)
	}

	fit, err := m.CanFit("qwen")
	if err != nil {
		t.Fatal(err)
	}
	if !fit.Fits {
		t.Error("CanFit.Fits = false, want true")
	}
}

func TestMemoryBudgetAddsUnifiedRSS(t *testing.T) {
	cfg := testConfig(t)
	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte(strconv.Itoa(4000*1024*1024)+"\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte(strconv.Itoa(122880)+"000000\n"), 0o644)

	// nemotron (rocm) active in a1; its RSS must ADD to gtt_used.
	procRoot := t.TempDir()
	pidDir := filepath.Join(procRoot, "500")
	os.MkdirAll(pidDir, 0o755)
	os.WriteFile(filepath.Join(pidDir, "comm"), []byte("llama-server\n"), 0o644)
	os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte("llama-server\x00--port\x008080\x00"), 0o644)
	os.WriteFile(filepath.Join(pidDir, "status"), []byte("VmRSS:\t"+strconv.Itoa(91*1024*1024)+" kB\n"), 0o644)

	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"))
	writeSlotEnv(t, cfg, "a1", "nemotron", "nemotron.gguf")

	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          sys,
		GPU:          &collector.GPU{DRMRoot: gpuRoot},
		Proc:         collector.Proc{Root: procRoot},
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	b, err := m.MemoryBudget()
	if err != nil {
		t.Fatal(err)
	}
	// gtt_used was 4000 MiB (sysfs bytes), VmRSS was 91 GiB (kB in /proc).
	// Both are bytes now — additive semantics unchanged.
	want := int64(4000*1024*1024) + int64(91)*1024*1024*1024
	if b.UsedBytes != want {
		t.Errorf("UsedBytes = %d, want %d (gtt + unified, additive)", b.UsedBytes, want)
	}
}

// ── Restart ──

func TestRestartUnloadedIsNoop(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	m, _, _ := newTestManager(t, cfg, sys, nil)
	m.saveMode("unloaded")
	res := m.Restart(context.Background())
	if !res.Success {
		t.Fatalf("res = %+v", res)
	}
	if len(sys.stopped) != 0 {
		t.Errorf("nothing should stop: %v", sys.stopped)
	}
}
