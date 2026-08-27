// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
)

// fakeSystemd is a scriptable collector.Systemd.
type fakeSystemd struct {
	mu     sync.Mutex
	states map[string]UnitState
	err    error
}

func (f *fakeSystemd) set(unit, active, sub string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = map[string]UnitState{}
	}
	f.states[unit] = UnitState{ActiveState: active, SubState: sub}
}

func (f *fakeSystemd) UnitStates(_ context.Context, units []string) (map[string]UnitState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]UnitState{}
	for _, u := range units {
		if st, ok := f.states[u]; ok {
			out[u] = st
		} else {
			out[u] = UnitState{ActiveState: "inactive", SubState: "dead"}
		}
	}
	return out, nil
}

// fakeLlama serves /metrics and /props for one slot.
type fakeLlama struct {
	mu                 sync.Mutex
	up                 bool
	processing         float64
	promptTotal        float64
	predictedTotal     float64
	promptSecondsTotal float64
	nDecodeTotal       float64
	nctx               int
	modelAlias         string
	modelPath          string
	srv                *httptest.Server
}

func newFakeLlama(t *testing.T, nctx int) *fakeLlama {
	f := &fakeLlama{up: true, nctx: nctx}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprintf(w, "llamacpp:requests_processing %g\n", f.processing)
			fmt.Fprintf(w, "llamacpp:prompt_tokens_seconds 0\n")
			fmt.Fprintf(w, "llamacpp:predicted_tokens_seconds 0\n")
			fmt.Fprintf(w, "llamacpp:prompt_tokens_total %g\n", f.promptTotal)
			fmt.Fprintf(w, "llamacpp:tokens_predicted_total %g\n", f.predictedTotal)
			fmt.Fprintf(w, "llamacpp:prompt_seconds_total %g\n", f.promptSecondsTotal)
			fmt.Fprintf(w, "llamacpp:n_decode_total %g\n", f.nDecodeTotal)
		case "/props":
			fmt.Fprintf(w, `{"default_generation_settings":{"n_ctx":%d},"model_alias":%q,"model_path":%q}`,
				f.nctx, f.modelAlias, f.modelPath)
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fakeSlots is a static SlotStateSource.
type fakeSlots struct {
	mu sync.Mutex
	m  map[string]SlotAssignment
}

func (f *fakeSlots) SlotStates(map[string]UnitState) map[string]SlotAssignment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]SlotAssignment{}
	for k, v := range f.m {
		out[k] = v
	}
	return out
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	sysconfig := t.TempDir()
	models := t.TempDir()
	return &config.Config{
		Paths: config.Paths{SysconfigDir: sysconfig, ModelsDir: models, StateDir: t.TempDir()},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
			"a3": {Unit: "forge-a3", Port: 8087, Label: "A3", Order: 3},
		},
		Modes: map[string]config.Mode{
			"vk-mode": {Services: []config.Service{{
				Model: "vk.gguf", Alias: "vk", Context: 32768, PortRole: "a1", Backend: "vulkan",
			}}},
			"nemotron": {Services: []config.Service{{
				Model: "nemotron.gguf", Alias: "nemotron", Context: 131072, PortRole: "a1", Backend: "rocm",
			}}},
		},
		Monitor: config.Monitor{
			PollIntervalS: 4, HangTPSThousand: 100, HangSustainS: 90, SwitchCooldownS: 120, GTTWarnPct: 85,
		},
	}
}

func fakeGPU(t *testing.T, usedMB, totalMB int64) *GPU {
	t.Helper()
	root := t.TempDir()
	dev := filepath.Join(root, "card0", "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "gpu_busy_percent"), []byte("25\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte(itoa(int(usedMB*1024*1024))+"\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte(itoa(int(totalMB*1024*1024))+"\n"), 0o644)
	return &GPU{DRMRoot: root}
}

// TestAdditiveInferenceRSS is THE crown-jewels memory test:
// inference_rss = gtt_used + unified_rss(ROCm slots only). ADD, never max()
// — and Vulkan slots' RSS must NOT be added (double count).
func TestAdditiveInferenceRSS(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running") // vk-mode (vulkan)
	sys.set("forge-a3", "active", "running") // nemotron (rocm)

	proc := writeFakeProc(t, map[int]fakePid{
		// Vulkan slot process: RSS must be ignored (already in gtt_used).
		201: {comm: "llama-server", args: []string{"llama-server", "--port", "8080"}, rssKB: 50 * 1024 * 1024},
		// ROCm+unified slot process: RSS must be added.
		202: {comm: "llama-server", args: []string{"llama-server", "--port", "8087"}, rssKB: 91 * 1024 * 1024},
	})

	slots := &fakeSlots{m: map[string]SlotAssignment{
		"a1": {Mode: "vk-mode"},
		"a3": {Mode: "nemotron"},
	}}

	// ComfyUI-style classic-GTT consumer is inside gtt_used=10000 already.
	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    slots,
		GPU:      fakeGPU(t, 10000, 122880),
		Proc:     proc,
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" }, // unreachable — not under test
	})
	snap := c.ProbeNow(context.Background())

	if snap.Metrics.InferenceRSSBytes == nil {
		t.Fatal("InferenceRSSBytes is nil")
	}
	// gtt_used was 10000 MiB (sysfs bytes), RSS was 91 GiB (kB in /proc).
	// Both bytes now — additive semantics unchanged.
	want := int64(10000*1024*1024) + int64(91)*1024*1024*1024
	if *snap.Metrics.InferenceRSSBytes != want {
		t.Errorf("InferenceRSSBytes = %d, want %d (additive: gtt 10000 MiB + unified 91 GiB)",
			*snap.Metrics.InferenceRSSBytes, want)
	}
}

// A ROCm slot whose RSS exceeds gtt_used must still ADD (the V4 max() bug
// silently dropped ComfyUI's classic-GTT footprint in exactly this case).
func TestAdditiveNotMax(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a3", "active", "running")

	proc := writeFakeProc(t, map[int]fakePid{
		301: {comm: "llama-server", args: []string{"llama-server", "--port", "8087"}, rssKB: 91 * 1024 * 1024},
	})
	slots := &fakeSlots{m: map[string]SlotAssignment{"a3": {Mode: "nemotron"}}}

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    slots,
		GPU:      fakeGPU(t, 6000, 122880), // ComfyUI holding 6 GB classic GTT
		Proc:     proc,
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
	})
	snap := c.ProbeNow(context.Background())

	want := int64(6000*1024*1024) + int64(91)*1024*1024*1024
	if snap.Metrics.InferenceRSSBytes == nil || *snap.Metrics.InferenceRSSBytes != want {
		t.Fatalf("InferenceRSSBytes = %v, want %d — max() semantics would report %d and drop ComfyUI",
			deref(snap.Metrics.InferenceRSSBytes), want, int64(91)*1024*1024*1024)
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// Systemd probe failure must reuse the previous cycle's unit map — an empty
// map would hide a deactivating unit and let slot state clear (crown jewels).
func TestUnitProbeFailureKeepsPreviousStates(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "deactivating", "stop-sigterm")

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
	})
	snap := c.ProbeNow(context.Background())
	if !snap.Units["forge-a1"].Deactivating() {
		t.Fatal("setup: expected deactivating")
	}

	sys.mu.Lock()
	sys.err = errors.New("dbus down")
	sys.mu.Unlock()
	snap = c.ProbeNow(context.Background())
	if !snap.Units["forge-a1"].Deactivating() {
		t.Error("probe failure must keep the previous units map (deactivating stays visible)")
	}
}

// Token deltas: baseline on first sight, delta on growth, reset detection,
// and per-slot session reset when the unit goes inactive.
func TestTokenSampleDeltas(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")

	llama := newFakeLlama(t, 32768)
	slots := &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "vk-mode"}}}

	type sample struct {
		slot, mode        string
		prompt, predicted int64
	}
	var samples []sample

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    slots,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return llama.srv.URL },
		DialPort: func(int) bool { return false },
		OnTokenSample: func(slot, mode string, p, pr int64) {
			samples = append(samples, sample{slot, mode, p, pr})
		},
	})
	ctx := context.Background()

	llama.mu.Lock()
	llama.promptTotal, llama.predictedTotal = 1000, 500
	llama.mu.Unlock()
	c.ProbeNow(ctx) // baseline — no sample
	if len(samples) != 0 {
		t.Fatalf("baseline cycle must not record: %v", samples)
	}

	llama.mu.Lock()
	llama.promptTotal, llama.predictedTotal = 1600, 900
	llama.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 1 || samples[0] != (sample{"a1", "vk-mode", 600, 400}) {
		t.Fatalf("delta sample wrong: %+v", samples)
	}

	// Counter reset (hot restart): new < prev → new value IS the delta.
	llama.mu.Lock()
	llama.promptTotal, llama.predictedTotal = 50, 30
	llama.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 2 || samples[1] != (sample{"a1", "vk-mode", 50, 30}) {
		t.Fatalf("reset sample wrong: %+v", samples)
	}

	// Idle cycle: no growth → no sample.
	c.ProbeNow(ctx)
	if len(samples) != 2 {
		t.Fatalf("idle cycle must not record: %+v", samples)
	}

	// Slot goes inactive → baseline dropped; reload must re-baseline, not
	// credit the pre-reload counter to the new mode.
	sys.set("forge-a1", "inactive", "dead")
	c.ProbeNow(ctx)
	sys.set("forge-a1", "active", "running")
	llama.mu.Lock()
	llama.promptTotal, llama.predictedTotal = 9000, 7000
	llama.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 2 {
		t.Fatalf("first cycle after reload must re-baseline: %+v", samples)
	}
}

// OnPrefillSample: real Δtokens/Δseconds delta, counter-reset handling
// matching OnTokenSample's, idle/zero-growth intervals skipped, and no
// firing at all when the build doesn't expose prompt_seconds_total (older
// llama.cpp — nil field, not just zero).
func TestPrefillSampleDeltas(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")

	llama := newFakeLlama(t, 32768)
	slots := &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "vk-mode"}}}

	type sample struct {
		slot, mode string
		tokens     int64
		seconds    float64
	}
	var samples []sample

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    slots,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return llama.srv.URL },
		DialPort: func(int) bool { return false },
		OnPrefillSample: func(slot, mode string, tokens int64, seconds float64) {
			samples = append(samples, sample{slot, mode, tokens, seconds})
		},
	})
	ctx := context.Background()

	llama.mu.Lock()
	llama.promptTotal, llama.promptSecondsTotal = 1000, 10
	llama.mu.Unlock()
	c.ProbeNow(ctx) // baseline — no sample
	if len(samples) != 0 {
		t.Fatalf("baseline cycle must not record: %v", samples)
	}

	llama.mu.Lock()
	llama.promptTotal, llama.promptSecondsTotal = 1500, 12
	llama.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 1 || samples[0] != (sample{"a1", "vk-mode", 500, 2}) {
		t.Fatalf("delta sample wrong: %+v", samples)
	}

	// Counter reset (hot restart of llama-server): new < prev on BOTH
	// counters → the new value IS the delta, same as OnTokenSample.
	llama.mu.Lock()
	llama.promptTotal, llama.promptSecondsTotal = 200, 3
	llama.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 2 || samples[1] != (sample{"a1", "vk-mode", 200, 3}) {
		t.Fatalf("reset sample wrong: %+v", samples)
	}

	// Idle cycle: no growth → no sample (would otherwise divide by ~0).
	c.ProbeNow(ctx)
	if len(samples) != 2 {
		t.Fatalf("idle cycle must not record: %+v", samples)
	}
}

// NCtx is fetched from /props once per slot session and dropped when the
// slot goes inactive (crown jewels: actual context recorded per load).
func TestNCtxPerSlotSession(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")
	llama := newFakeLlama(t, 30000)
	llama.modelAlias = "vk-mode"
	llama.modelPath = "/opt/forge/models/vk-mode.gguf"

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "vk-mode"}}},
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return llama.srv.URL },
		DialPort: func(int) bool { return false },
	})
	ctx := context.Background()
	snap := c.ProbeNow(ctx)
	if snap.Inference["a1"].NCtx != 30000 {
		t.Fatalf("NCtx = %d", snap.Inference["a1"].NCtx)
	}
	if snap.Inference["a1"].ModelAlias != "vk-mode" || snap.Inference["a1"].ModelPath != "/opt/forge/models/vk-mode.gguf" {
		t.Errorf("ModelAlias/ModelPath = %q/%q, want vk-mode//opt/forge/models/vk-mode.gguf",
			snap.Inference["a1"].ModelAlias, snap.Inference["a1"].ModelPath)
	}

	// Reload with a different context AND a different actually-running
	// model: session cache must reset for both, not just NCtx.
	sys.set("forge-a1", "inactive", "dead")
	c.ProbeNow(ctx)
	llama.mu.Lock()
	llama.nctx = 8192 // kernel silently reduced it this time
	llama.modelAlias = "other-mode"
	llama.modelPath = "/opt/forge/models/other-mode.gguf"
	llama.mu.Unlock()
	sys.set("forge-a1", "active", "running")
	snap = c.ProbeNow(ctx)
	if snap.Inference["a1"].NCtx != 8192 {
		t.Errorf("NCtx after reload = %d, want 8192 (stale session cache?)", snap.Inference["a1"].NCtx)
	}
	if snap.Inference["a1"].ModelAlias != "other-mode" {
		t.Errorf("ModelAlias after reload = %q, want other-mode (stale identity cache?)", snap.Inference["a1"].ModelAlias)
	}
}

func TestGTTHighAlert(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		GPU:      fakeGPU(t, 110000, 122880), // 89.5% > 85%
		Proc:     Proc{Root: t.TempDir()},
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
	})
	snap := c.ProbeNow(context.Background())
	found := false
	for _, a := range snap.Alerts {
		if a.Code == "GTT_HIGH" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected GTT_HIGH alert, got %+v", snap.Alerts)
	}
}

// TestHangNoAlertOnAdvancingDecode is the end-to-end crown jewel for the
// llama.cpp #26920 live-progress fix: a long single generation that reads
// 0.0 TPS (gauges frozen until slot reset) must NOT fire INFERENCE_HANG as
// long as n_decode_total is advancing — that is a healthy long run, not a
// KFD stall. A slot whose decode counter freezes while requests sit active
// must still alert.
func TestHangNoAlertOnAdvancingDecode(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")

	llama := newFakeLlama(t, 262144)
	slots := &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "vk-mode"}}}

	now := time.Now()
	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    slots,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return llama.srv.URL },
		DialPort: func(int) bool { return false },
		Now:      func() time.Time { return now },
	})
	ctx := context.Background()

	hasHang := func(snap *Snapshot) bool {
		for _, a := range snap.Alerts {
			if a.Code == "INFERENCE_HANG" {
				return true
			}
		}
		return false
	}

	// Long generation: request in flight, gauges pinned at 0 (builds with
	// the #26920 refactor flush them only at slot reset), but the decode
	// counter climbs every scrape — live GPU progress.
	llama.mu.Lock()
	llama.processing = 1
	llama.promptTotal, llama.predictedTotal = 500000, 70000 // frozen totals
	llama.nDecodeTotal = 100
	llama.mu.Unlock()

	for i := 0; i < 40; i++ { // ~160s of "generation" at 4s poll
		now = now.Add(4 * time.Second)
		llama.mu.Lock()
		llama.nDecodeTotal += 25
		llama.mu.Unlock()
		if snap := c.ProbeNow(ctx); hasHang(snap) {
			t.Fatalf("cycle %d: INFERENCE_HANG fired on advancing decode counter: %+v", i, snap.Alerts)
		}
	}

	// Decode stops (genuine KFD-eviction-style stall): requests still
	// active, gauges still 0, counter frozen. Must fire after sustain.
	for i := 0; i < 40; i++ { // up to 160s of frozen decode
		now = now.Add(4 * time.Second)
		if snap := c.ProbeNow(ctx); hasHang(snap) {
			return // fired — correct
		}
	}
	t.Fatalf("INFERENCE_HANG never fired after decode counter froze for %ds", 40*4)
}

func TestSlotErrorStormAlert(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	stormCalls := 0
	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		GPU:      fakeGPU(t, 0, 122880),
		Proc:     Proc{Root: t.TempDir()},
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
		SlotErrorCount: func(port int, window int64) (int, int64) {
			stormCalls++
			if port == 8080 {
				return 5, 20 // a1: 5 failures in window, 20 lifetime
			}
			return 1, 1 // a3: below threshold
		},
	})
	snap := c.ProbeNow(context.Background())
	found := false
	for _, a := range snap.Alerts {
		if a.Code == "SLOT_ERROR_STORM" {
			found = true
			if a.Port != 8080 {
				t.Errorf("SLOT_ERROR_STORM port = %d, want 8080", a.Port)
			}
		}
	}
	if !found {
		t.Errorf("expected SLOT_ERROR_STORM alert, got %+v", snap.Alerts)
	}
	if stormCalls == 0 {
		t.Errorf("SlotErrorCount seam was never called")
	}
}

func TestBookmarkHealthAndSlots(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")
	sys.set("ai-mode-comfyui", "active", "running")

	now := time.Unix(1_800_000_000, 0)
	c := New(Options{
		Cfg:     func() *config.Config { return cfg },
		Systemd: sys,
		Slots: &fakeSlots{m: map[string]SlotAssignment{
			"a1": {Mode: "vk-mode"},
			"a3": {Unloading: &Transition{Mode: "nemotron", StartedAt: now}},
		}},
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
		DialPort: func(port int) bool { return port == 8080 },
		Bookmarks: func() []BookmarkProbe {
			return []BookmarkProbe{
				{Label: "ComfyUI", Health: "systemd_unit", HealthArg: "ai-mode-comfyui"},
				{Label: "Always", AlwaysOnline: true},
				{Label: "ClientProbed"},
			}
		},
	})
	snap := c.ProbeNow(context.Background())

	if !snap.BookmarkHealth["ComfyUI"] || !snap.BookmarkHealth["Always"] {
		t.Errorf("bookmark health wrong: %+v", snap.BookmarkHealth)
	}
	if _, ok := snap.BookmarkHealth["ClientProbed"]; ok {
		t.Error("client-probed bookmark must have no server-side entry")
	}
	if snap.Slots["a1"].Mode != "vk-mode" || snap.Slots["a1"].Label != "A1" {
		t.Errorf("a1 slot wrong: %+v", snap.Slots["a1"])
	}
	if snap.Slots["a3"].Unloading == nil || snap.Slots["a3"].Unloading.Mode != "nemotron" {
		t.Errorf("a3 unloading transition lost: %+v", snap.Slots["a3"])
	}
	if !snap.Ports[8080] || snap.Ports[8087] {
		t.Errorf("ports wrong: %+v", snap.Ports)
	}
}

// Compressor proxies are dynamic units (created/removed at runtime, Phase 9b),
// not derivable from config — unitNames() must discover them from
// CompressorTargets (the same store-backed source the savings scrape reads)
// so their live Active state reaches GET /api/v1/compressor/config.
func TestCompressorProxyUnitsDiscovered(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-compress@aiand", "active", "running")

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
		DialPort: func(port int) bool { return false },
		CompressorTargets: func() map[string]int {
			return map[string]int{"aiand": 9000}
		},
	})
	snap := c.ProbeNow(context.Background())

	u, ok := snap.Units["forge-compress@aiand"]
	if !ok {
		t.Fatal("forge-compress@aiand not probed — CompressorTargets not feeding unitNames()")
	}
	if !u.Active() {
		t.Errorf("forge-compress@aiand state = %+v, want active", u)
	}
}

// TestCompressorProxyUnitsUseRealUnitName is the regression test for a real
// bug found live on ForgeHost 2026-07-28: the "aiand" provider's actual systemd
// unit was "headroom-external" (a provider nickname vs. its real unit,
// assigned independently), not "headroom-aiand" — the hardcoded naming
// convention could never watch it, so it showed inactive on the dashboard
// regardless of whether it was really running. CompressorUnits
// (store.ProxyRow.Unit) is the fix: it must be consulted ahead of the
// naming convention, not just as a fallback for a nil map.
func TestCompressorProxyUnitsUseRealUnitName(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("headroom-external", "active", "running")
	// The conventional-but-wrong name must NOT be what's watched — if it
	// were, this fake unit's "inactive" state would leak through instead.
	sys.set("forge-compress@aiand", "inactive", "dead")

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
		DialPort: func(port int) bool { return false },
		CompressorTargets: func() map[string]int {
			return map[string]int{"aiand": 9000}
		},
		CompressorUnits: func() map[string]string {
			return map[string]string{"aiand": "headroom-external"}
		},
	})
	snap := c.ProbeNow(context.Background())

	if _, ok := snap.Units["forge-compress@aiand"]; ok {
		t.Error("watched the wrong (conventional) unit name forge-compress@aiand — CompressorUnits should override it")
	}
	u, ok := snap.Units["headroom-external"]
	if !ok {
		t.Fatal("headroom-external not probed — CompressorUnits not feeding unitNames()")
	}
	if !u.Active() {
		t.Errorf("headroom-external state = %+v, want active", u)
	}
}

// TestTTSEngineUnitsDiscoveredLive: Tier 1 Sprint 2 (Voice & Speech
// settings) — tts.engines' configured units (forge-tts-custom/-base,
// kokoro) must be watched so their restart-loop alerts actually fire (the
// Sprint 0 incident: they crash-looped for 5 days undetected because
// nothing probed them). Unlike ExtraUnits (a static snapshot from daemon
// startup), TTSEngineUnits is a closure re-consulted every cycle — this
// test proves that by changing what it returns between two ProbeNow calls
// on the SAME Collector, with no restart.
func TestTTSEngineUnitsDiscoveredLive(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-tts-custom", "active", "running")

	var configured []string
	c := New(Options{
		Cfg:            func() *config.Config { return cfg },
		Systemd:        sys,
		GPU:            &GPU{DRMRoot: t.TempDir()},
		Proc:           Proc{Root: t.TempDir()},
		BaseURL:        func(port int) string { return "http://127.0.0.1:1" },
		DialPort:       func(port int) bool { return false },
		TTSEngineUnits: func() []string { return configured },
	})

	snap := c.ProbeNow(context.Background())
	if _, ok := snap.Units["forge-tts-custom"]; ok {
		t.Fatal("forge-tts-custom probed before it was configured — TTSEngineUnits should gate this")
	}

	// Simulate an operator saving Settings -> Voice & Speech with a real
	// unit filled in, with no daemon restart in between.
	configured = []string{"forge-tts-custom"}
	snap = c.ProbeNow(context.Background())
	u, ok := snap.Units["forge-tts-custom"]
	if !ok {
		t.Fatal("forge-tts-custom not probed after TTSEngineUnits started returning it — not read live per cycle")
	}
	if !u.Active() {
		t.Errorf("forge-tts-custom state = %+v, want active", u)
	}
}

// fakeCompressorState is a mutable, mutex-guarded set of Compressor /metrics
// counter values a test can script across ProbeNow cycles.
type fakeCompressorState struct {
	mu                                                            sync.Mutex
	tokensIn, tokensOut, tokensSaved                              float64
	requests, requestsCached, requestsFailed, requestsRateLimited float64
	ttfbCount, ttfbSum, ttfbMin, ttfbMax                          float64
	latencyCount, latencySum, latencyMin, latencyMax              float64
	overheadCount, overheadSum, overheadMin, overheadMax          float64
	byProvider, byModel                                           map[string]float64
}

func (s *fakeCompressorState) render() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "compress_tokens_input_total %g\n", s.tokensIn)
	fmt.Fprintf(&b, "compress_tokens_output_total %g\n", s.tokensOut)
	fmt.Fprintf(&b, "compress_tokens_saved_total %g\n", s.tokensSaved)
	fmt.Fprintf(&b, "compress_requests_total %g\n", s.requests)
	fmt.Fprintf(&b, "compress_requests_cached_total %g\n", s.requestsCached)
	fmt.Fprintf(&b, "compress_requests_failed_total %g\n", s.requestsFailed)
	fmt.Fprintf(&b, "compress_requests_rate_limited_total %g\n", s.requestsRateLimited)
	fmt.Fprintf(&b, "compress_ttfb_ms_count %g\n", s.ttfbCount)
	fmt.Fprintf(&b, "compress_ttfb_ms_sum %g\n", s.ttfbSum)
	fmt.Fprintf(&b, "compress_ttfb_ms_min %g\n", s.ttfbMin)
	fmt.Fprintf(&b, "compress_ttfb_ms_max %g\n", s.ttfbMax)
	fmt.Fprintf(&b, "compress_latency_ms_count %g\n", s.latencyCount)
	fmt.Fprintf(&b, "compress_latency_ms_sum %g\n", s.latencySum)
	fmt.Fprintf(&b, "compress_latency_ms_min %g\n", s.latencyMin)
	fmt.Fprintf(&b, "compress_latency_ms_max %g\n", s.latencyMax)
	fmt.Fprintf(&b, "compress_overhead_ms_count %g\n", s.overheadCount)
	fmt.Fprintf(&b, "compress_overhead_ms_sum %g\n", s.overheadSum)
	fmt.Fprintf(&b, "compress_overhead_ms_min %g\n", s.overheadMin)
	fmt.Fprintf(&b, "compress_overhead_ms_max %g\n", s.overheadMax)
	for k, v := range s.byProvider {
		fmt.Fprintf(&b, "compress_requests_by_provider{provider=%q} %g\n", k, v)
	}
	for k, v := range s.byModel {
		fmt.Fprintf(&b, "compress_requests_by_model{model=%q} %g\n", k, v)
	}
	return b.String()
}

func newCompressorServer(t *testing.T, s *fakeCompressorState) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, s.render())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCompressorSampleDeltas(t *testing.T) {
	cfg := testConfig(t)
	state := &fakeCompressorState{tokensIn: 10000, tokensOut: 500, requests: 100}
	srv := newCompressorServer(t, state)

	var samples []CompressorSample
	c := New(Options{
		Cfg:             func() *config.Config { return cfg },
		Systemd:         &fakeSystemd{},
		GPU:             &GPU{DRMRoot: t.TempDir()},
		Proc:            Proc{Root: t.TempDir()},
		DialPort:        func(int) bool { return false },
		BaseURL:         func(port int) string { return srv.URL },
		CompressorTargets: func() map[string]int { return map[string]int{"local": 8788} },
		OnCompressorSample: func(service string, s CompressorSample) {
			samples = append(samples, s)
		},
	})
	ctx := context.Background()
	c.ProbeNow(ctx) // baseline
	if len(samples) != 0 {
		t.Fatalf("baseline must not record: %+v", samples)
	}

	state.mu.Lock()
	state.tokensIn, state.tokensOut, state.requests = 15000, 750, 105
	state.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d: %+v", len(samples), samples)
	}
	got := samples[0]
	if got.TokensInDelta != 5000 || got.TokensOutDelta != 250 || got.RequestsDelta != 5 {
		t.Errorf("deltas wrong: %+v", got)
	}
}

func TestCompressorSampleFailedRequestsOnlyStillRecorded(t *testing.T) {
	// Regression test: the old skip condition (inputDelta<=0 && savedDelta<=0)
	// would silently drop an interval where only requests_failed_total moved
	// — exactly when latency/failure data matters most. AllZero must catch
	// this as a non-idle interval.
	cfg := testConfig(t)
	state := &fakeCompressorState{tokensIn: 1000, requests: 10}
	srv := newCompressorServer(t, state)

	var samples []CompressorSample
	c := New(Options{
		Cfg:             func() *config.Config { return cfg },
		Systemd:         &fakeSystemd{},
		GPU:             &GPU{DRMRoot: t.TempDir()},
		Proc:            Proc{Root: t.TempDir()},
		DialPort:        func(int) bool { return false },
		BaseURL:         func(port int) string { return srv.URL },
		CompressorTargets: func() map[string]int { return map[string]int{"local": 8788} },
		OnCompressorSample: func(service string, s CompressorSample) {
			samples = append(samples, s)
		},
	})
	ctx := context.Background()
	c.ProbeNow(ctx) // baseline

	state.mu.Lock()
	state.requestsFailed = 3 // tokens/requests unchanged, only failures moved
	state.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 1 {
		t.Fatalf("want 1 sample (failed-requests-only must not be skipped as idle), got %d", len(samples))
	}
	if samples[0].RequestsFailedDelta != 3 {
		t.Errorf("RequestsFailedDelta = %v, want 3", samples[0].RequestsFailedDelta)
	}
	if samples[0].TokensInDelta != 0 || samples[0].RequestsDelta != 0 {
		t.Errorf("unrelated deltas should stay 0: %+v", samples[0])
	}
}

func TestCompressorSampleProviderSumming(t *testing.T) {
	cfg := testConfig(t)
	// requests_total increments in lockstep with requests_by_provider on a
	// real proxy (every request increments both); AllZero only inspects
	// scalar deltas, so the scalar must move too for a realistic scenario.
	state := &fakeCompressorState{
		requests:   14,
		byProvider: map[string]float64{"openai": 10, "anthropic": 4},
	}
	srv := newCompressorServer(t, state)

	var samples []CompressorSample
	c := New(Options{
		Cfg:             func() *config.Config { return cfg },
		Systemd:         &fakeSystemd{},
		GPU:             &GPU{DRMRoot: t.TempDir()},
		Proc:            Proc{Root: t.TempDir()},
		DialPort:        func(int) bool { return false },
		BaseURL:         func(port int) string { return srv.URL },
		CompressorTargets: func() map[string]int { return map[string]int{"local": 8788} },
		OnCompressorSample: func(service string, s CompressorSample) {
			samples = append(samples, s)
		},
	})
	ctx := context.Background()
	c.ProbeNow(ctx) // baseline

	state.mu.Lock()
	state.requests = 25
	state.byProvider = map[string]float64{"openai": 16, "anthropic": 9}
	state.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	want := map[string]int64{"openai": 6, "anthropic": 5}
	got := samples[0].RequestsByProviderDelta
	if len(got) != len(want) {
		t.Fatalf("RequestsByProviderDelta = %+v, want %+v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("RequestsByProviderDelta[%q] = %v, want %v", k, got[k], v)
		}
	}
}

func TestCompressorSampleProxyRestartTreatedAsNewBaseline(t *testing.T) {
	cfg := testConfig(t)
	state := &fakeCompressorState{tokensIn: 50000, requests: 200}
	srv := newCompressorServer(t, state)

	var samples []CompressorSample
	c := New(Options{
		Cfg:             func() *config.Config { return cfg },
		Systemd:         &fakeSystemd{},
		GPU:             &GPU{DRMRoot: t.TempDir()},
		Proc:            Proc{Root: t.TempDir()},
		DialPort:        func(int) bool { return false },
		BaseURL:         func(port int) string { return srv.URL },
		CompressorTargets: func() map[string]int { return map[string]int{"local": 8788} },
		OnCompressorSample: func(service string, s CompressorSample) {
			samples = append(samples, s)
		},
	})
	ctx := context.Background()
	c.ProbeNow(ctx) // baseline: 50000/200

	// Proxy restart: counters reset to near-0, then a little traffic.
	state.mu.Lock()
	state.tokensIn, state.requests = 300, 4
	state.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 1 {
		t.Fatalf("want 1 sample after restart, got %d: %+v", len(samples), samples)
	}
	got := samples[0]
	if got.TokensInDelta != 300 || got.RequestsDelta != 4 {
		t.Errorf("post-restart delta = %+v, want the reset counter's own value (300/4), not a negative diff against the pre-restart total", got)
	}
}

func TestCompressorSampleVanishedProxyDropsBaseline(t *testing.T) {
	cfg := testConfig(t)
	state := &fakeCompressorState{tokensIn: 90000, requests: 500}
	srv := newCompressorServer(t, state)

	var samples []CompressorSample
	targeted := true
	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  &fakeSystemd{},
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return srv.URL },
		CompressorTargets: func() map[string]int {
			if targeted {
				return map[string]int{"local": 8788}
			}
			return map[string]int{}
		},
		OnCompressorSample: func(service string, s CompressorSample) {
			samples = append(samples, s)
		},
	})
	ctx := context.Background()
	c.ProbeNow(ctx) // baseline: 90000/500

	// The proxy row is torn down (no longer targeted) for a cycle...
	targeted = false
	c.ProbeNow(ctx)
	if len(samples) != 0 {
		t.Fatalf("untargeted proxy must not record: %+v", samples)
	}

	// ...then reappears (recreated) with a low counter value, simulating a
	// fresh process. Without dropping the stale baseline, this would diff
	// against the pre-teardown total and either wrongly report a "reset" (if
	// delta() happens to treat it as one) or an implausible stale-derived
	// figure. It must instead be treated as a fresh baseline (no sample this
	// cycle) with the new low value establishing the next baseline.
	targeted = true
	state.mu.Lock()
	state.tokensIn, state.requests = 50, 1
	state.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 0 {
		t.Fatalf("proxy reappearing after teardown must re-baseline (no sample yet), got %+v", samples)
	}

	state.mu.Lock()
	state.tokensIn, state.requests = 250, 3
	state.mu.Unlock()
	c.ProbeNow(ctx)
	if len(samples) != 1 {
		t.Fatalf("want 1 sample after the re-baseline settles, got %d: %+v", len(samples), samples)
	}
	if samples[0].TokensInDelta != 200 || samples[0].RequestsDelta != 2 {
		t.Errorf("delta after re-baseline = %+v, want 200/2 (against the post-reappearance baseline of 50/1)", samples[0])
	}
}

// buildSlots must attribute each occupied slot's real live GPU memory
// (VRAM+GTT via fdinfo — Sprint follow-up, 2026-08-04) using the PID found
// at the slot's configured port, and leave empty slots at 0.
func TestBuildSlotsMemoryBytes(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")

	proc := writeFakeProc(t, map[int]fakePid{
		401: {comm: "llama-server", args: []string{"llama-server", "--port", "8080"}, fdinfo: map[string]string{
			"3": amdgpuFdinfo("6", "625120", "232960"),
			"4": amdgpuFdinfo("6", "625120", "232960"), // same client, must not double
		}},
	})
	slots := &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "vk-mode"}}}

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    slots,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     proc,
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
	})
	snap := c.ProbeNow(context.Background())

	a1, ok := snap.Slots["a1"]
	if !ok {
		t.Fatal("a1 missing from snapshot")
	}
	want := int64((625120 + 232960) * 1024)
	if a1.MemoryBytes != want {
		t.Errorf("a1.MemoryBytes = %d, want %d (deduped fdinfo reading)", a1.MemoryBytes, want)
	}

	a3, ok := snap.Slots["a3"]
	if !ok {
		t.Fatal("a3 missing from snapshot")
	}
	if a3.MemoryBytes != 0 {
		t.Errorf("a3.MemoryBytes = %d, want 0 (empty slot)", a3.MemoryBytes)
	}
}

// TestSlotActivityFiresOnEdgeOnly is Sprint K's crown jewel for the
// push-based activity signal: OnSlotActivity must fire once per
// busy↔idle transition, never once per cycle regardless of state — at a
// 2s collector cadence, a naive per-cycle push would flood the SSE bus for
// the ordinary case of one slot serving a long streaming response. It must
// also fire a final false when a slot drops out of the active set
// entirely (unloaded), so a listener never has to infer "gone" from
// silence.
func TestSlotActivityFiresOnEdgeOnly(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")

	llama := newFakeLlama(t, 32768)
	slots := &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "vk-mode"}}}

	type edge struct {
		slot   string
		active bool
	}
	var edges []edge

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    slots,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return llama.srv.URL },
		DialPort: func(int) bool { return false },
		OnSlotActivity: func(slot string, active bool) {
			edges = append(edges, edge{slot, active})
		},
	})
	ctx := context.Background()

	// First sighting: no edge, regardless of state — a connecting/
	// reconnecting client already gets the starting state from the polled
	// statusResponse.slot_activity; the push channel only carries
	// transitions after that.
	c.ProbeNow(ctx)
	if len(edges) != 0 {
		t.Fatalf("first-sighting cycle must not fire: %v", edges)
	}

	// Goes busy: exactly one true edge.
	llama.mu.Lock()
	llama.processing = 1
	llama.mu.Unlock()
	c.ProbeNow(ctx)
	if len(edges) != 1 || edges[0] != (edge{"a1", true}) {
		t.Fatalf("busy edge wrong: %+v", edges)
	}

	// Stays busy across two more cycles: must NOT re-fire.
	c.ProbeNow(ctx)
	c.ProbeNow(ctx)
	if len(edges) != 1 {
		t.Fatalf("sustained-busy cycles must not re-fire: %+v", edges)
	}

	// Goes idle: exactly one false edge.
	llama.mu.Lock()
	llama.processing = 0
	llama.mu.Unlock()
	c.ProbeNow(ctx)
	if len(edges) != 2 || edges[1] != (edge{"a1", false}) {
		t.Fatalf("idle edge wrong: %+v", edges)
	}

	// Stays idle: must NOT re-fire.
	c.ProbeNow(ctx)
	if len(edges) != 2 {
		t.Fatalf("sustained-idle cycle must not re-fire: %+v", edges)
	}

	// Goes busy again, then the slot is unloaded before going idle again:
	// must get the busy edge, then exactly one false on drop-out (never
	// left stuck at true).
	llama.mu.Lock()
	llama.processing = 1
	llama.mu.Unlock()
	c.ProbeNow(ctx)
	if len(edges) != 3 || edges[2] != (edge{"a1", true}) {
		t.Fatalf("re-busy edge wrong: %+v", edges)
	}
	sys.set("forge-a1", "inactive", "dead")
	c.ProbeNow(ctx)
	if len(edges) != 4 || edges[3] != (edge{"a1", false}) {
		t.Fatalf("drop-out edge wrong: %+v", edges)
	}
	c.ProbeNow(ctx) // still gone: must not re-fire
	if len(edges) != 4 {
		t.Fatalf("cycle after drop-out must not re-fire: %+v", edges)
	}
}

// TestActivityDetectedBetweenGaugePolls covers the pre-release feedback
// round's "impossible idle times" bug: RequestsProcessing is an
// instantaneous gauge sampled once per ~2s cycle, so a request that both
// starts and finishes between two polls was previously invisible —
// LastActivity would never advance even though the slot genuinely served
// tokens. The fix adds the cumulative prompt/predicted token-total delta as
// a second activity signal. This test never sets llama.processing non-zero
// (it stays 0 at every scrape, simulating the gap), so it fails against the
// pre-fix code (activity gated on the gauge alone) and passes once the
// token-delta signal is wired in.
func TestActivityDetectedBetweenGaugePolls(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")

	llama := newFakeLlama(t, 32768)
	slots := &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "vk-mode"}}}

	now := time.Now()
	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    slots,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(port int) string { return llama.srv.URL },
		DialPort: func(int) bool { return false },
		Now:      func() time.Time { return now },
	})
	ctx := context.Background()

	llama.mu.Lock()
	llama.promptTotal, llama.predictedTotal = 1000, 500
	llama.mu.Unlock()
	c.ProbeNow(ctx) // baseline cycle
	baseline := c.Current().Slots["a1"].LastActivity

	// Advance the clock and the cumulative token totals, but leave
	// RequestsProcessing at 0 — exactly what a request-start-and-finish
	// gap between two polls looks like from the outside.
	now = now.Add(30 * time.Second)
	llama.mu.Lock()
	llama.promptTotal, llama.predictedTotal = 1200, 650
	llama.mu.Unlock()
	c.ProbeNow(ctx)

	got := c.Current().Slots["a1"].LastActivity
	if !got.Equal(now) {
		t.Fatalf("LastActivity = %v, want %v (bumped by token-total delta despite RequestsProcessing==0)", got, now)
	}
	if !got.After(baseline) {
		t.Fatalf("LastActivity did not advance past baseline: baseline=%v got=%v", baseline, got)
	}

	// A genuinely idle cycle (no gauge activity, no token growth) must not
	// re-bump LastActivity.
	now = now.Add(30 * time.Second)
	c.ProbeNow(ctx)
	if stuck := c.Current().Slots["a1"].LastActivity; !stuck.Equal(got) {
		t.Fatalf("idle cycle must not advance LastActivity: got %v, want unchanged %v", stuck, got)
	}
}

// TestSlowProbesGatedToEveryOtherCycle verifies Sprint K's cadence-halving
// mitigation: probePorts' TCP dials and bookmarkHealth's real health checks
// (TailscaleOnline is the actual network-facing cost — a "tailscale_node"
// bookmark, not "systemd_unit", so this is never conflated with unitNames'
// unconditional-every-cycle Bookmarks() read, which only needs the label
// list to build the D-Bus watch set and is not itself expensive) must not
// double in frequency just because PollIntervalS did — they run on odd
// cycles only, and the prior cycle's result carries over on even cycles
// rather than going empty.
func TestSlowProbesGatedToEveryOtherCycle(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")

	var tailscaleCalls, portCalls int
	c := New(Options{
		Cfg:     func() *config.Config { return cfg },
		Systemd: sys,
		Slots:   &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "vk-mode"}}},
		GPU:     &GPU{DRMRoot: t.TempDir()},
		Proc:    Proc{Root: t.TempDir()},
		BaseURL: func(port int) string { return "http://127.0.0.1:1" },
		DialPort: func(port int) bool {
			portCalls++
			return port == 8080
		},
		Bookmarks: func() []BookmarkProbe {
			return []BookmarkProbe{{Label: "Tailnet", Health: "tailscale_node", HealthArg: "node1"}}
		},
		TailscaleOnline: func(context.Context, string) bool {
			tailscaleCalls++
			return true
		},
	})
	ctx := context.Background()

	snap1 := c.ProbeNow(ctx) // cycle 1 (odd): slow probes run
	if tailscaleCalls != 1 {
		t.Fatalf("cycle 1 must check tailscale health: calls=%d", tailscaleCalls)
	}
	if portCalls != 2 { // testConfig has 2 slots (a1, a3)
		t.Fatalf("cycle 1 must dial ports: calls=%d", portCalls)
	}
	if !snap1.Ports[8080] || snap1.Ports[8087] {
		t.Fatalf("cycle 1 ports wrong: %+v", snap1.Ports)
	}
	if !snap1.BookmarkHealth["Tailnet"] {
		t.Fatalf("cycle 1 bookmark health wrong: %+v", snap1.BookmarkHealth)
	}

	snap2 := c.ProbeNow(ctx) // cycle 2 (even): slow probes skipped, reuse prior result
	if tailscaleCalls != 1 {
		t.Fatalf("cycle 2 must not re-check tailscale health: calls=%d", tailscaleCalls)
	}
	if portCalls != 2 {
		t.Fatalf("cycle 2 must not re-dial ports: calls=%d", portCalls)
	}
	if !snap2.BookmarkHealth["Tailnet"] {
		t.Fatalf("cycle 2 must carry over cycle 1's bookmark result: %+v", snap2.BookmarkHealth)
	}
	if !snap2.Ports[8080] {
		t.Fatalf("cycle 2 must carry over cycle 1's port result: %+v", snap2.Ports)
	}

	c.ProbeNow(ctx) // cycle 3 (odd): slow probes run again
	if tailscaleCalls != 2 {
		t.Fatalf("cycle 3 must re-check tailscale health: calls=%d", tailscaleCalls)
	}
	if portCalls != 4 {
		t.Fatalf("cycle 3 must re-dial ports: calls=%d", portCalls)
	}
}

// TestCPUUtilAndNetworkRatesAcrossCycles is the end-to-end Phase 4
// (2026-08-12) counterpart to the proc_test.go unit tests: both CPU.Pct and
// the network rates are cumulative-counter deltas, meaningless on a single
// cycle — the first ProbeNow must report "no data yet" (0 / nil), and only
// the second, diffed against the first, must report real numbers.
func TestCPUUtilAndNetworkRatesAcrossCycles(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}
	procRoot := t.TempDir()

	writeStat := func(user uint64) {
		// idle (field 4) held fixed at 900 across both cycles so the delta
		// is driven entirely by the varying user field.
		stat := fmt.Sprintf("cpu  %d 0 0 900 0 0 0 0 0 0\n", user)
		if err := os.WriteFile(filepath.Join(procRoot, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeNet := func(rx, tx uint64) {
		if err := os.MkdirAll(filepath.Join(procRoot, "net"), 0o755); err != nil {
			t.Fatal(err)
		}
		dev := fmt.Sprintf("Inter-|   Receive\n face |bytes\n  eth0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\n", rx, tx)
		if err := os.WriteFile(filepath.Join(procRoot, "net", "dev"), []byte(dev), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeStat(100)
	writeNet(1000, 2000)

	now := time.Unix(1000, 0)
	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: procRoot},
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
		Now:      func() time.Time { return now },
	})

	snap1 := c.ProbeNow(context.Background())
	if snap1.Metrics.CPU.Pct != 0 {
		t.Errorf("first-cycle CPU.Pct = %v, want 0 (no baseline yet)", snap1.Metrics.CPU.Pct)
	}
	if snap1.Metrics.NetRxBytesPerSec != nil || snap1.Metrics.NetTxBytesPerSec != nil {
		t.Errorf("first-cycle net rates must be nil, got rx=%v tx=%v",
			snap1.Metrics.NetRxBytesPerSec, snap1.Metrics.NetTxBytesPerSec)
	}

	// Second cycle, 10s later: user jiffies 100->200 (total 1000->1100,
	// idle unchanged) = 100% busy over the interval; rx +5000/tx +2000
	// bytes over 10s = 500/200 B/s.
	writeStat(200)
	writeNet(6000, 4000)
	now = now.Add(10 * time.Second)

	snap2 := c.ProbeNow(context.Background())
	if snap2.Metrics.CPU.Pct != 100 {
		t.Errorf("second-cycle CPU.Pct = %v, want 100", snap2.Metrics.CPU.Pct)
	}
	if snap2.Metrics.NetRxBytesPerSec == nil || *snap2.Metrics.NetRxBytesPerSec != 500 {
		t.Errorf("NetRxBytesPerSec = %v, want 500", deref2(snap2.Metrics.NetRxBytesPerSec))
	}
	if snap2.Metrics.NetTxBytesPerSec == nil || *snap2.Metrics.NetTxBytesPerSec != 200 {
		t.Errorf("NetTxBytesPerSec = %v, want 200", deref2(snap2.Metrics.NetTxBytesPerSec))
	}
}

func deref2(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestCycleSamplesStorageAndHostSensors checks the collector wires
// sampleStorageMounts and HostSensors into a real cycle's Metrics (Phase 4,
// 2026-08-12) — not just that the helpers work in isolation.
func TestCycleSamplesStorageAndHostSensors(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}

	hwRoot := t.TempDir()
	cpuDir := filepath.Join(hwRoot, "hwmon0")
	if err := os.MkdirAll(cpuDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cpuDir, "name"), []byte("k10temp\n"), 0o644)
	os.WriteFile(filepath.Join(cpuDir, "temp1_label"), []byte("Tctl\n"), 0o644)
	os.WriteFile(filepath.Join(cpuDir, "temp1_input"), []byte("55000\n"), 0o644)

	c := New(Options{
		Cfg:         func() *config.Config { return cfg },
		Systemd:     sys,
		GPU:         &GPU{DRMRoot: t.TempDir()},
		Proc:        Proc{Root: t.TempDir()},
		HostSensors: HostSensors{HWMonRoot: hwRoot},
		DialPort:    func(int) bool { return false },
		BaseURL:     func(port int) string { return "http://127.0.0.1:1" },
	})
	snap := c.ProbeNow(context.Background())

	if len(snap.Metrics.Storage) < 1 {
		t.Fatal("Metrics.Storage is empty, want at least the root mount")
	}
	if snap.Metrics.CPUPackageTempC == nil || *snap.Metrics.CPUPackageTempC != 55 {
		t.Errorf("CPUPackageTempC = %v, want 55", deref2(snap.Metrics.CPUPackageTempC))
	}
}
