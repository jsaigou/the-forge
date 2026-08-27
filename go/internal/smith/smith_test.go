// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── test fixtures ────────────────────────────────────────────────────────────

// stubSched overrides Status on the stock sched.Stub.
type stubSched struct {
	*sched.Stub
	status sched.Status
}

func (s *stubSched) Status() sched.Status { return s.status }

func newStubSched(slots map[string]string) *stubSched {
	labels := map[string]string{}
	mem := map[string]int64{}
	for slot := range slots {
		labels[slot] = strings.ToUpper(slot)
	}
	return &stubSched{
		Stub: &sched.Stub{},
		status: sched.Status{
			Slots:           slots,
			SlotLabels:      labels,
			SlotMemoryBytes: mem,
			MemoryBudget:    sched.Budget{TotalBytes: 120 << 30, UsedBytes: 10 << 30, FreeBytes: 110 << 30},
		},
	}
}

// openDB opens a real in-memory store (migrations applied — smith tables
// exist).
func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// setSetting writes a raw JSON settings value.
func setSetting(t *testing.T, db *store.DB, key, jsonValue string) {
	t.Helper()
	if err := db.Settings().Set(context.Background(), key, []byte(jsonValue)); err != nil {
		t.Fatalf("settings.Set(%s): %v", key, err)
	}
}

// seedBrainCatalog seeds a minimal real catalog: a local Config
// "ornith-35b" (n_ctx 262144) and an enabled Offering with wire_model
// "deepseek-chat" via provider "deepseek".
func seedBrainCatalog(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, err := cat.CreateModel(ctx, store.Model{Name: "ornith"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "35b"})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	gguf, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName: %v", err)
	}
	artID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: gguf.ID, ArtifactType: "weight",
		FilePath: "ornith-35b.gguf", FileSizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	if _, err := cat.CreateConfig(ctx, store.Config{
		Name: "ornith-35b", VariantID: varID, WeightArtifactID: artID, EngineID: eng.ID,
		NCtx: 262144, Parallel: 1, Status: "unverified", Visibility: "visible",
	}); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{Name: "deepseek", APIKey: "sk-test"}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseek, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	if _, err := cat.CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseek.ID, WireModel: "deepseek-chat",
		Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}
}

// snapWith builds a snapshot carrying the given metrics (plus empty maps so
// checks can iterate safely).
func snapWith(m collector.Metrics) *collector.Snapshot {
	return &collector.Snapshot{
		TakenAt:        time.Now(),
		Hostname:       "forgehost",
		Metrics:        m,
		Units:          map[string]collector.UnitState{},
		Slots:          map[string]collector.SlotState{},
		Inference:      map[string]collector.SlotInference{},
		Ports:          map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}
}

func int64p(v int64) *int64       { return &v }
func float64p(v float64) *float64 { return &v }

// newSmith builds a Smith over a Static collector source with the given
// snapshot and optional store, for check-level tests.
func newSmith(snap *collector.Snapshot, db *store.DB, extra func(*Deps)) *Smith {
	d := Deps{
		Source: collector.NewStatic(snap),
		Now:    time.Now,
		Logf:   func(string, ...any) {},
	}
	if db != nil {
		d.Store = db
		d.Settings = db.Settings()
	}
	if extra != nil {
		extra(&d)
	}
	return New(d)
}

// ── Check severity tests (crafted snapshots → expected severity) ────────────

func TestGTTCeilingSeverity(t *testing.T) {
	cases := []struct {
		name     string
		usedPct  float64 // fraction of total
		noData   bool
		want     Severity
		wantSkip bool
	}{
		{"crit at 97%", 0.97, false, SeverityCrit, false},
		{"crit at boundary 95%", 0.95, false, SeverityCrit, false},
		{"warn at 90%", 0.90, false, SeverityWarn, false},
		{"warn at boundary 85%", 0.85, false, SeverityWarn, false},
		{"ok at 50%", 0.50, false, SeverityOK, false},
		{"skip when GTT absent", 0, true, SeverityInfo, true},
	}
	total := int64(120 << 30)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var snap *collector.Snapshot
			if tc.noData {
				snap = snapWith(collector.Metrics{})
			} else {
				snap = snapWith(collector.Metrics{
					GTTUsedBytes:  int64p(int64(float64(total) * tc.usedPct)),
					GTTTotalBytes: int64p(total),
				})
			}
			s := newSmith(snap, nil, nil)
			f := runOne(context.Background(), registry[0], s.checkEnv(context.Background()))
			if f.Severity != tc.want {
				t.Errorf("severity = %s, want %s (summary %q)", f.Severity, tc.want, f.Summary)
			}
		})
	}
}

func TestDiskSpaceSeverity(t *testing.T) {
	cases := []struct {
		name string
		pct  float64
		want Severity
	}{
		{"crit at 96%", 96, SeverityCrit},
		{"warn at 88%", 88, SeverityWarn},
		{"ok at 40%", 40, SeverityOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := snapWith(collector.Metrics{Disk: collector.Disk{
				TotalBytes: 1000, UsedBytes: int64(tc.pct * 10), FreeBytes: int64((100 - tc.pct) * 10), Pct: tc.pct,
			}})
			s := newSmith(snap, nil, nil)
			f := runOne(context.Background(), findCheck(t, "disk_space"), s.checkEnv(context.Background()))
			if f.Severity != tc.want {
				t.Errorf("severity = %s, want %s (summary %q)", f.Severity, tc.want, f.Summary)
			}
		})
	}
}

func TestBinaryPathsSeverity(t *testing.T) {
	t.Run("ok when every configured/catalog path exists", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "llama-server")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{Paths: config.Paths{VulkanBin: bin, RocmBin: bin}}
		s := newSmith(snapWith(collector.Metrics{}), nil, func(d *Deps) {
			d.Cfg = func() *config.Config { return cfg }
		})
		f := runOne(context.Background(), findCheck(t, "binary_paths"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("warn when infra.paths.rocm_bin is stale", func(t *testing.T) {
		dir := t.TempDir()
		vulkan := filepath.Join(dir, "llama-server")
		if err := os.WriteFile(vulkan, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{Paths: config.Paths{
			VulkanBin: vulkan,
			RocmBin:   filepath.Join(dir, "does-not-exist", "llama-server"), // sprint-4 finding #1
		}}
		s := newSmith(snapWith(collector.Metrics{}), nil, func(d *Deps) {
			d.Cfg = func() *config.Config { return cfg }
		})
		f := runOne(context.Background(), findCheck(t, "binary_paths"), s.checkEnv(context.Background()))
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("warn when a catalog build's binary_path is missing", func(t *testing.T) {
		db, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ctx := context.Background()
		if _, err := db.Catalog().CreateBuild(ctx, store.Build{
			EngineID: 1, Name: "llama.cpp-kintsugi/build-rocm-new",
			BinaryPath: "/opt/forge/llama.cpp-kintsugi/build-rocm-new/bin/llama-server", Backend: "rocm",
		}); err != nil {
			t.Fatal(err)
		}
		s := newSmith(snapWith(collector.Metrics{}), db, func(d *Deps) { d.Catalog = db.Catalog() })
		f := runOne(context.Background(), findCheck(t, "binary_paths"), s.checkEnv(context.Background()))
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("empty binary_path on a retired build is not flagged", func(t *testing.T) {
		db, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ctx := context.Background()
		if _, err := db.Catalog().CreateBuild(ctx, store.Build{
			EngineID: 1, Name: "(retired) standard-rocm", BinaryPath: "", Backend: "rocm",
		}); err != nil {
			t.Fatal(err)
		}
		s := newSmith(snapWith(collector.Metrics{}), db, func(d *Deps) { d.Catalog = db.Catalog() })
		f := runOne(context.Background(), findCheck(t, "binary_paths"), s.checkEnv(context.Background()))
		if f.Severity != SeverityInfo {
			t.Errorf("severity = %s, want info/skipped for an all-empty-path catalog (summary %q)", f.Severity, f.Summary)
		}
	})
}

func TestGPUHangSeverity(t *testing.T) {
	t.Run("crit on INFERENCE_HANG alert", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Alerts = []collector.Alert{{Code: "INFERENCE_HANG", Msg: "stalled", Port: 8080}}
		s := newSmith(snap, nil, nil)
		f := runOne(context.Background(), findCheck(t, "gpu_hang"), s.checkEnv(context.Background()))
		if f.Severity != SeverityCrit {
			t.Errorf("severity = %s, want crit", f.Severity)
		}
	})
	t.Run("crit on metrics stall (requests>0, tps~0)", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Inference["a1"] = collector.SlotInference{RequestsProcessing: 2, TokensPerSecond: 0.01}
		s := newSmith(snap, nil, nil)
		f := runOne(context.Background(), findCheck(t, "gpu_hang"), s.checkEnv(context.Background()))
		if f.Severity != SeverityCrit {
			t.Errorf("severity = %s, want crit", f.Severity)
		}
	})
	t.Run("ok when healthy", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Inference["a1"] = collector.SlotInference{RequestsProcessing: 1, TokensPerSecond: 25}
		s := newSmith(snap, nil, nil)
		f := runOne(context.Background(), findCheck(t, "gpu_hang"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok", f.Severity)
		}
	})
}

func TestGPUDeviceLostSeverity(t *testing.T) {
	t.Run("crit on kernel journal ring timeout", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		s := newSmith(snap, nil, func(d *Deps) {
			d.KernelJournal = func(_ context.Context, n int, _ time.Time) ([]string, error) {
				return []string{
					"amdgpu 0000:c5:00.0: ring comp_1.2.0 timeout, signaled seq=1, emitted seq=4",
					"amdgpu 0000:c5:00.0: [drm] device wedged, but no recovery needed",
				}, nil
			}
		})
		f := runOne(context.Background(), findCheck(t, "gpu_device_lost"), s.checkEnv(context.Background()))
		if f.Severity != SeverityCrit {
			t.Errorf("severity = %s, want crit", f.Severity)
		}
	})
	t.Run("crit on unit journal ErrorDeviceLost", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		s := newSmith(snap, nil, func(d *Deps) {
			d.JournalErrors = func(_ context.Context, n int, _ time.Time) ([]string, error) {
				return []string{"send_error: task id = 876, error: decode() failed: vk::Queue::submit: ErrorDeviceLost"}, nil
			}
		})
		f := runOne(context.Background(), findCheck(t, "gpu_device_lost"), s.checkEnv(context.Background()))
		if f.Severity != SeverityCrit {
			t.Errorf("severity = %s, want crit", f.Severity)
		}
	})
	t.Run("ok when journals clean", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		s := newSmith(snap, nil, func(d *Deps) {
			d.KernelJournal = func(_ context.Context, n int, _ time.Time) ([]string, error) {
				return []string{"amdgpu: normal boot"}, nil
			}
			d.JournalErrors = func(_ context.Context, n int, _ time.Time) ([]string, error) {
				return []string{"forge-a1: started"}, nil
			}
		})
		f := runOne(context.Background(), findCheck(t, "gpu_device_lost"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok", f.Severity)
		}
	})
	t.Run("skipped when no journal seams wired", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		s := newSmith(snap, nil, nil)
		f := runOne(context.Background(), findCheck(t, "gpu_device_lost"), s.checkEnv(context.Background()))
		if f.Severity != SeverityInfo {
			t.Errorf("severity = %s, want info (skip)", f.Severity)
		}
	})
}

func TestNCtxActualSeverity(t *testing.T) {
	cfg := &config.Config{Modes: map[string]config.Mode{
		"ornith-35b": {Services: []config.Service{{Context: 262144}}},
	}}
	withCfg := func(d *Deps) { d.Cfg = func() *config.Config { return cfg } }

	t.Run("crit when kernel reduced the context", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Slots["a1"] = collector.SlotState{Slot: "a1", Mode: "ornith-35b"}
		snap.Inference["a1"] = collector.SlotInference{NCtx: 131072}
		s := newSmith(snap, nil, withCfg)
		f := runOne(context.Background(), findCheck(t, "n_ctx_actual"), s.checkEnv(context.Background()))
		if f.Severity != SeverityCrit {
			t.Errorf("severity = %s, want crit (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("ok when actual matches configured", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Slots["a1"] = collector.SlotState{Slot: "a1", Mode: "ornith-35b"}
		snap.Inference["a1"] = collector.SlotInference{NCtx: 262144}
		s := newSmith(snap, nil, withCfg)
		f := runOne(context.Background(), findCheck(t, "n_ctx_actual"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
}

func TestSlotAgreementSeverity(t *testing.T) {
	t.Run("warn on mismatch", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		// Collector still sees qwen3 in a1; scheduler says a1 is empty and
		// it is NOT an in-progress unload → disagreement.
		snap.Slots["a1"] = collector.SlotState{Slot: "a1", Mode: "qwen3"}
		s := newSmith(snap, nil, func(d *Deps) {
			d.Sched = newStubSched(map[string]string{"a1": ""})
		})
		f := runOne(context.Background(), findCheck(t, "slot_agreement"), s.checkEnv(context.Background()))
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("ok when views agree", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Slots["a1"] = collector.SlotState{Slot: "a1", Mode: "qwen3"}
		s := newSmith(snap, nil, func(d *Deps) {
			d.Sched = newStubSched(map[string]string{"a1": "qwen3"})
		})
		f := runOne(context.Background(), findCheck(t, "slot_agreement"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("expected transient during unload is not a finding", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Slots["a1"] = collector.SlotState{
			Slot: "a1", Mode: "qwen3",
			Unloading: &collector.Transition{Mode: "qwen3", StartedAt: time.Now()},
		}
		s := newSmith(snap, nil, func(d *Deps) {
			d.Sched = newStubSched(map[string]string{"a1": ""})
		})
		f := runOne(context.Background(), findCheck(t, "slot_agreement"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok for unload transient (summary %q)", f.Severity, f.Summary)
		}
	})
}

func TestSlotModelIdentitySeverity(t *testing.T) {
	cfg := &config.Config{Modes: map[string]config.Mode{
		"gemma4-26b-a4b-nothink": {Services: []config.Service{{Alias: "gemma4-26b-a4b-nothink"}}},
	}}
	withCfg := func(d *Deps) { d.Cfg = func() *config.Config { return cfg } }

	t.Run("crit when the running process reports a different alias", func(t *testing.T) {
		// The real a4 incident (sprint-4 follow-up, docs/pitfalls.md): engine
		// believes the -nothink sibling is loaded; the actually-running
		// process (frozen FOUNDRY_MODEL_ALIAS) reports the base sibling.
		snap := snapWith(collector.Metrics{})
		snap.Slots["a4"] = collector.SlotState{Slot: "a4", Mode: "gemma4-26b-a4b-nothink"}
		snap.Inference["a4"] = collector.SlotInference{ModelAlias: "gemma4-26b-a4b"}
		s := newSmith(snap, nil, withCfg)
		f := runOne(context.Background(), findCheck(t, "slot_model_identity"), s.checkEnv(context.Background()))
		if f.Severity != SeverityCrit {
			t.Errorf("severity = %s, want crit (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("ok when the running process agrees", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Slots["a1"] = collector.SlotState{Slot: "a1", Mode: "gemma4-26b-a4b-nothink"}
		snap.Inference["a1"] = collector.SlotInference{ModelAlias: "gemma4-26b-a4b-nothink"}
		s := newSmith(snap, nil, withCfg)
		f := runOne(context.Background(), findCheck(t, "slot_model_identity"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("skipped when the slot has no live /props scrape yet", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Slots["a1"] = collector.SlotState{Slot: "a1", Mode: "gemma4-26b-a4b-nothink"}
		// No snap.Inference["a1"] entry at all — not scraped.
		s := newSmith(snap, nil, withCfg)
		f := runOne(context.Background(), findCheck(t, "slot_model_identity"), s.checkEnv(context.Background()))
		if f.Severity != SeverityInfo {
			t.Errorf("severity = %s, want info/skipped (summary %q)", f.Severity, f.Summary)
		}
	})
}

func TestAlwaysOnPortsSeverity(t *testing.T) {
	cfg := &config.Config{Ports: map[string]int{"embedding": 8083, "stt": 8084}}
	withCfg := func(d *Deps) { d.Cfg = func() *config.Config { return cfg } }

	t.Run("warn when a port is down", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Ports = map[int]bool{8083: true, 8084: false}
		s := newSmith(snap, nil, withCfg)
		f := runOne(context.Background(), findCheck(t, "always_on_ports"), s.checkEnv(context.Background()))
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("ok when all ports up", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Ports = map[int]bool{8083: true, 8084: true}
		s := newSmith(snap, nil, withCfg)
		f := runOne(context.Background(), findCheck(t, "always_on_ports"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
}

func TestKernelParamsSeverity(t *testing.T) {
	writeCmdline := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "cmdline")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		return p
	}
	run := func(t *testing.T, d Deps) Finding {
		t.Helper()
		s := New(d)
		return runOne(context.Background(), findCheck(t, "kernel_params"), s.checkEnv(context.Background()))
	}

	t.Run("ok when mitigations present", func(t *testing.T) {
		p := writeCmdline(t, "BOOT_IMAGE=/vmlinuz amdgpu.mcbp=0 amdgpu.vm_fragment_size=9 quiet")
		f := run(t, Deps{CmdlinePath: p})
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("warn when a mitigation is missing", func(t *testing.T) {
		p := writeCmdline(t, "BOOT_IMAGE=/vmlinuz amdgpu.mcbp=0 quiet")
		f := run(t, Deps{CmdlinePath: p})
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
		if !strings.Contains(f.Summary, "amdgpu.vm_fragment_size=9") {
			t.Errorf("summary should name the missing param, got %q", f.Summary)
		}
	})
	t.Run("skip when the file is unreadable", func(t *testing.T) {
		f := run(t, Deps{CmdlinePath: filepath.Join(t.TempDir(), "nope")})
		if f.Severity != SeverityInfo {
			t.Errorf("severity = %s, want info (skipped)", f.Severity)
		}
	})
}

func TestA0ReachabilitySeverity(t *testing.T) {
	t.Run("ok against a live healthz", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()
		port := portOf(t, ts.URL)
		s := New(Deps{
			Cfg: func() *config.Config {
				return &config.Config{Server: config.Server{RouterListen: ":" + strconv.Itoa(port)}}
			},
		})
		f := runOne(context.Background(), findCheck(t, "a0_reachability"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("crit when nothing listens", func(t *testing.T) {
		s := New(Deps{
			Cfg: func() *config.Config {
				return &config.Config{Server: config.Server{RouterListen: ":1"}}
			},
			HTTPClient: &http.Client{Timeout: 500 * time.Millisecond},
		})
		f := runOne(context.Background(), findCheck(t, "a0_reachability"), s.checkEnv(context.Background()))
		if f.Severity != SeverityCrit {
			t.Errorf("severity = %s, want crit (summary %q)", f.Severity, f.Summary)
		}
	})
}

func TestCompressorHealthFallback(t *testing.T) {
	t.Run("warn when a compressor unit is inactive (no store wired)", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Units["forge-compress@local"] = collector.UnitState{ActiveState: "failed"}
		snap.Units["forge-a1"] = collector.UnitState{ActiveState: "active"} // not a proxy
		s := newSmith(snap, nil, nil)
		f := runOne(context.Background(), findCheck(t, "compressor_reachability"), s.checkEnv(context.Background()))
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("ok when compressor units are active", func(t *testing.T) {
		snap := snapWith(collector.Metrics{})
		snap.Units["forge-compress@local"] = collector.UnitState{ActiveState: "active", SubState: "running"}
		s := newSmith(snap, nil, nil)
		f := runOne(context.Background(), findCheck(t, "compressor_reachability"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
}

// TestCompressorHealthStorePath covers the registered-proxy path. Regression
// (live-verify 2026-08-06): the check read Snap.Ports for proxy ports, but
// the collector's probePorts only dials configured ports (cfg.Slots +
// cfg.Ports) — dynamic compressor_proxies ports are never in that map, so the
// zero value read as "down" and every healthy proxy flagged unhealthy. The
// check must dial the port itself.
func TestCompressorHealthStorePath(t *testing.T) {
	proxy := store.ProxyRow{Service: "local", Label: "local", Port: 8788,
		TargetURL: "http://127.0.0.1:8080", Unit: "forge-compress@local"}

	t.Run("ok when unit active and port answers the direct dial", func(t *testing.T) {
		db := openDB(t)
		if err := db.Routing().SaveProxy(context.Background(), proxy); err != nil {
			t.Fatalf("SaveProxy: %v", err)
		}
		snap := snapWith(collector.Metrics{})
		snap.Units["forge-compress@local"] = collector.UnitState{ActiveState: "active", SubState: "running"}
		// Deliberately NO snap.Ports entry — that is the production reality
		// for proxy ports and the bug this test pins.
		s := newSmith(snap, db, func(d *Deps) {
			d.DialPort = func(int) bool { return true }
		})
		f := runOne(context.Background(), findCheck(t, "compressor_reachability"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("warn when the port dial fails", func(t *testing.T) {
		db := openDB(t)
		if err := db.Routing().SaveProxy(context.Background(), proxy); err != nil {
			t.Fatalf("SaveProxy: %v", err)
		}
		snap := snapWith(collector.Metrics{})
		snap.Units["forge-compress@local"] = collector.UnitState{ActiveState: "active", SubState: "running"}
		s := newSmith(snap, db, func(d *Deps) {
			d.DialPort = func(int) bool { return false }
		})
		f := runOne(context.Background(), findCheck(t, "compressor_reachability"), s.checkEnv(context.Background()))
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
	})
	t.Run("orphaned proxies are skipped", func(t *testing.T) {
		db := openDB(t)
		p := proxy
		p.OrphanedAt = time.Now()
		if err := db.Routing().SaveProxy(context.Background(), p); err != nil {
			t.Fatalf("SaveProxy: %v", err)
		}
		s := newSmith(snapWith(collector.Metrics{}), db, func(d *Deps) {
			d.DialPort = func(int) bool { return false }
		})
		f := runOne(context.Background(), findCheck(t, "compressor_reachability"), s.checkEnv(context.Background()))
		if f.Severity != SeverityOK || !strings.Contains(f.Summary, "no compressor proxies") {
			t.Errorf("got %s %q, want ok / no compressor proxies registered", f.Severity, f.Summary)
		}
	})
}

func TestForgeSelf(t *testing.T) {
	db := openDB(t)
	// Seed an FK violation so the info path is exercised (compatibilities is
	// the known-live offender). FK enforcement must be off to insert an
	// orphan row, exactly how the known-live violations got there; restore it
	// after so the check runs under the production posture.
	if _, err := db.SQL().Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("foreign_keys off: %v", err)
	}
	if _, err := db.SQL().Exec(
		`INSERT INTO compatibilities (auxiliary_artifact_id, variant_id) VALUES (999999, 999999)`); err != nil {
		t.Fatalf("seed FK violation: %v", err)
	}
	if _, err := db.SQL().Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("foreign_keys on: %v", err)
	}
	s := New(Deps{Store: db})
	f := runOne(context.Background(), findCheck(t, "forge_self"), s.checkEnv(context.Background()))
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info (FK violations surfaced, summary %q)", f.Severity, f.Summary)
	}
	if !strings.Contains(f.Summary, "foreign-key violation") {
		t.Errorf("summary should mention FK violations, got %q", f.Summary)
	}
}

// ── Brain resolution ─────────────────────────────────────────────────────────

func TestBrainResolution(t *testing.T) {
	ctx := context.Background()

	t.Run("empty smith.model → deterministic_only", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{Settings: db.Settings(), Catalog: db.Catalog()})
		b := s.Brain(ctx)
		if b.Resolution != BrainDeterministicOnly {
			t.Errorf("resolution = %s, want deterministic_only", b.Resolution)
		}
	})

	t.Run("local config loaded on a slot → local_slot", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"ornith-35b"`)
		s := New(Deps{
			Settings: db.Settings(), Catalog: db.Catalog(),
			Sched: newStubSched(map[string]string{"a3": "ornith-35b"}),
		})
		b := s.Brain(ctx)
		if b.Resolution != BrainLocalSlot || b.Slot != "a3" {
			t.Errorf("brain = %+v, want local_slot on a3", b)
		}
	})

	t.Run("local config not loaded → deterministic_only with explanation", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"ornith-35b"`)
		s := New(Deps{
			Settings: db.Settings(), Catalog: db.Catalog(),
			Sched: newStubSched(map[string]string{"a1": "other"}),
		})
		b := s.Brain(ctx)
		if b.Resolution != BrainDeterministicOnly {
			t.Errorf("resolution = %s, want deterministic_only", b.Resolution)
		}
		if !strings.Contains(b.Detail, "not currently loaded") {
			t.Errorf("detail should explain the not-loaded state, got %q", b.Detail)
		}
	})

	t.Run("offering wire_model → remote", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"deepseek-chat"`)
		s := New(Deps{Settings: db.Settings(), Catalog: db.Catalog()})
		b := s.Brain(ctx)
		if b.Resolution != BrainRemote || b.Provider != "deepseek" {
			t.Errorf("brain = %+v, want remote via deepseek", b)
		}
	})

	t.Run("unresolvable model → deterministic_only", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"no-such-model"`)
		s := New(Deps{Settings: db.Settings(), Catalog: db.Catalog()})
		b := s.Brain(ctx)
		if b.Resolution != BrainDeterministicOnly {
			t.Errorf("resolution = %s, want deterministic_only", b.Resolution)
		}
	})
}

// ── SelfContext ──────────────────────────────────────────────────────────────

func TestSelfContext(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)

	used := int64(100 << 30)
	total := int64(120 << 30)
	snap := snapWith(collector.Metrics{
		GTTUsedBytes:  int64p(used),
		GTTTotalBytes: int64p(total),
		Memory:        collector.Memory{TotalBytes: 128 << 30, UsedBytes: 40 << 30, AvailBytes: 88 << 30, Pct: 31.25},
	})
	snap.Alerts = []collector.Alert{{Code: "GTT_HIGH", Msg: "high"}}

	s := New(Deps{
		Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
		Source: collector.NewStatic(snap),
		Sched:  newStubSched(map[string]string{"a3": "ornith-35b"}),
	})

	sc := s.SelfContext(context.Background())

	// smith P3: Tier now reflects brain resolvability (a brain is loaded on
	// a3 here), not a hardcoded "always deterministic in P1" value.
	if sc.Tier != TierReasoning {
		t.Errorf("tier = %q, want %q", sc.Tier, TierReasoning)
	}
	if sc.Hostname != "forgehost" {
		t.Errorf("hostname = %q, want forgehost", sc.Hostname)
	}
	if sc.Metrics == nil || sc.Metrics.GTTUsedBytes == nil || *sc.Metrics.GTTUsedBytes != used {
		t.Errorf("metrics summary missing GTT used bytes: %+v", sc.Metrics)
	}
	if len(sc.Alerts) != 1 || sc.Alerts[0].Code != "GTT_HIGH" {
		t.Errorf("alerts = %+v, want the GTT_HIGH alert", sc.Alerts)
	}
	alloc, ok := sc.Slots["a3"]
	if !ok || alloc.Mode != "ornith-35b" || alloc.Label != "A3" {
		t.Errorf("slot a3 allocation = %+v (ok=%v), want ornith-35b/A3", alloc, ok)
	}
	if sc.MemoryBudget.TotalBytes != 120<<30 {
		t.Errorf("memory budget = %+v, want total 120GiB", sc.MemoryBudget)
	}
	if sc.Brain.Resolution != BrainLocalSlot || sc.Brain.Slot != "a3" {
		t.Errorf("brain = %+v, want local_slot:a3", sc.Brain)
	}
	if sc.CheckCount != len(registry) || sc.FastCheckCount != fastCheckCount() {
		t.Errorf("check counts = %d/%d, want %d/%d",
			sc.CheckCount, sc.FastCheckCount, len(registry), fastCheckCount())
	}
}

// ── Sweep orchestration + persistence ───────────────────────────────────────

func TestSweepPersistsFindings(t *testing.T) {
	db := openDB(t)
	// Craft a snapshot that makes GTT crit; the rest of the quick checks
	// degrade to info/ok — all of them must land in smith_findings.
	total := int64(120 << 30)
	snap := snapWith(collector.Metrics{
		GTTUsedBytes:  int64p(int64(float64(total) * 0.97)),
		GTTTotalBytes: int64p(total),
	})
	s := New(Deps{Store: db, Settings: db.Settings(), Source: collector.NewStatic(snap)})
	ctx := context.Background()

	findings, err := s.RunChecks(ctx, ScopeQuick, nil, SweepManual)
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if len(findings) != fastCheckCount() {
		t.Fatalf("quick sweep ran %d checks, want %d", len(findings), fastCheckCount())
	}
	var gtt Finding
	for _, f := range findings {
		if f.CheckID == "gtt_ceiling" {
			gtt = f
		}
	}
	if gtt.Severity != SeverityCrit {
		t.Fatalf("gtt_ceiling = %s, want crit", gtt.Severity)
	}

	stored, err := s.ListFindings(ctx, time.Time{}, "", 0)
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(stored) != len(findings) {
		t.Fatalf("stored %d findings, want %d", len(stored), len(findings))
	}
	// Newest-first ordering + sweep_kind attribution.
	if stored[0].SweepKind != SweepManual {
		t.Errorf("sweep_kind = %q, want manual", stored[0].SweepKind)
	}
	// Severity filter.
	crits, err := s.ListFindings(ctx, time.Time{}, "crit", 0)
	if err != nil {
		t.Fatalf("ListFindings(crit): %v", err)
	}
	if len(crits) != 1 || crits[0].CheckID != "gtt_ceiling" {
		t.Errorf("crit findings = %+v, want exactly gtt_ceiling", crits)
	}
}

func TestSweepSelection(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Source: collector.NewStatic(snapWith(collector.Metrics{}))})
	ctx := context.Background()

	t.Run("deep runs every check except ManualOnly ones", func(t *testing.T) {
		// comfyui_prune is ManualOnly (S7-followup smith UX sprint) — a
		// delete-file proposal must never come from a background sweep the
		// operator never asked for, only from an explicit selection (the
		// "explicit check ids" subtest below covers that path).
		var manualOnly int
		for _, c := range registry {
			if c.ManualOnly {
				manualOnly++
			}
		}
		findings, err := s.RunChecks(ctx, ScopeDeep, nil, SweepManual)
		if err != nil {
			t.Fatalf("RunChecks(deep): %v", err)
		}
		if want := len(registry) - manualOnly; len(findings) != want {
			t.Errorf("deep sweep ran %d checks, want %d (registry minus %d ManualOnly)", len(findings), want, manualOnly)
		}
	})
	t.Run("explicit check ids", func(t *testing.T) {
		findings, err := s.RunChecks(ctx, "", []string{"gtt_ceiling", "disk_space"}, SweepManual)
		if err != nil {
			t.Fatalf("RunChecks(ids): %v", err)
		}
		if len(findings) != 2 || findings[0].CheckID != "gtt_ceiling" || findings[1].CheckID != "disk_space" {
			t.Errorf("findings = %+v, want gtt_ceiling + disk_space in order", findings)
		}
	})
	t.Run("unknown check id errors", func(t *testing.T) {
		if _, err := s.RunChecks(ctx, "", []string{"bogus"}, SweepManual); err == nil {
			t.Error("expected error for unknown check id")
		}
	})
	t.Run("unknown scope errors", func(t *testing.T) {
		if _, err := s.RunChecks(ctx, "sideways", nil, SweepManual); err == nil {
			t.Error("expected error for unknown scope")
		}
	})
}

func TestSweepWithoutStoreStillReturnsFindings(t *testing.T) {
	s := New(Deps{Source: collector.NewStatic(snapWith(collector.Metrics{}))})
	findings, err := s.RunChecks(context.Background(), ScopeQuick, nil, SweepManual)
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if len(findings) != fastCheckCount() {
		t.Errorf("findings = %d, want %d", len(findings), fastCheckCount())
	}
	if _, err := s.ListFindings(context.Background(), time.Time{}, "", 0); err != ErrStoreUnwired {
		t.Errorf("ListFindings without store = %v, want ErrStoreUnwired", err)
	}
}

// ── Settings parsing ─────────────────────────────────────────────────────────

func TestThresholdsParsing(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Settings: db.Settings()})
	ctx := context.Background()

	if got := s.Thresholds(ctx); got != DefaultThresholds() {
		t.Errorf("default thresholds = %+v, want %+v", got, DefaultThresholds())
	}
	setSetting(t, db, SettingThresholds, `{"gtt_warn_pct":70,"disk_crit_pct":99}`)
	got := s.Thresholds(ctx)
	want := Thresholds{
		GTTWarnPct: 70, GTTCritPct: 95, DiskWarnPct: 85, DiskCritPct: 99, DeviceLostWindowMinutes: 15,
		CompressorRSSWindowHours: 6, CompressorRSSGrowthWarnPct: 40, CompressorRestartsWarnPerHour: 3,
		BuildRefreshBehindN: 500, CompressorFailOpenWarnPct: 10,
	}
	if got != want {
		t.Errorf("thresholds = %+v, want %+v (partial override + defaults)", got, want)
	}
	// Invalid JSON falls back wholesale.
	setSetting(t, db, SettingThresholds, `not json`)
	if got := s.Thresholds(ctx); got != DefaultThresholds() {
		t.Errorf("thresholds after invalid JSON = %+v, want defaults", got)
	}
	// device_lost_window_minutes overrides independently of the pct fields.
	setSetting(t, db, SettingThresholds, `{"device_lost_window_minutes":45}`)
	if got := s.Thresholds(ctx); got.DeviceLostWindowMinutes != 45 {
		t.Errorf("device_lost_window_minutes = %d, want 45", got.DeviceLostWindowMinutes)
	}
	// A non-positive override is rejected, same footgun-avoidance as the pct fields.
	setSetting(t, db, SettingThresholds, `{"device_lost_window_minutes":0}`)
	if got := s.Thresholds(ctx); got.DeviceLostWindowMinutes != DefaultThresholds().DeviceLostWindowMinutes {
		t.Errorf("device_lost_window_minutes with 0 override = %d, want default %d", got.DeviceLostWindowMinutes, DefaultThresholds().DeviceLostWindowMinutes)
	}
}

func TestScheduleParsing(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Settings: db.Settings()})
	ctx := context.Background()

	if got := s.Schedule(ctx); got != DefaultSchedule() {
		t.Errorf("default schedule = %+v, want %+v", got, DefaultSchedule())
	}
	setSetting(t, db, SettingSchedule, `{"quick":"15m","enabled":false}`)
	got := s.Schedule(ctx)
	if got.Quick != "15m" || got.Deep != "24h" || got.Enabled {
		t.Errorf("schedule = %+v, want quick=15m deep=24h enabled=false", got)
	}
}

func TestNextDue(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	sched := Schedule{Quick: "60m", Deep: "24h", Enabled: true}

	cases := []struct {
		name                string
		sched               Schedule
		lastQuick, lastDeep time.Time
		want                string
		wantOK              bool
	}{
		{"disabled → nothing due", Schedule{Quick: "60m", Deep: "24h", Enabled: false}, now, now, "", false},
		{"never ran → deep wins first", sched, time.Time{}, time.Time{}, ScopeDeep, true},
		{"deep due beats quick", sched, now.Add(-30 * time.Minute), now.Add(-25 * time.Hour), ScopeDeep, true},
		{"quick due when deep not", sched, now.Add(-61 * time.Minute), now.Add(-1 * time.Hour), ScopeQuick, true},
		{"nothing due yet", sched, now.Add(-5 * time.Minute), now.Add(-1 * time.Hour), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nextDue(tc.sched, tc.lastQuick, tc.lastDeep, now)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("nextDue = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// findCheck returns the registered check with the given ID (test fails if
// the registry loses it).
func findCheck(t *testing.T, id string) Check {
	t.Helper()
	for _, c := range registry {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q not in registry", id)
	return Check{}
}

// portOf extracts the port from an httptest URL like http://127.0.0.1:PORT.
func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	idx := strings.LastIndex(rawURL, ":")
	if idx < 0 {
		t.Fatalf("no port in %q", rawURL)
	}
	p, err := strconv.Atoi(strings.TrimSuffix(rawURL[idx+1:], "/"))
	if err != nil {
		t.Fatalf("parse port from %q: %v", rawURL, err)
	}
	return p
}

// engine.Stub satisfies the Deps.Engine fallback path — a compile-time
// proof that SelfContext's Engine-only slot listing stays wired.
var _ engine.Engine = (*engine.Stub)(nil)
