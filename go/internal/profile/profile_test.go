// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeEngine implements engine.Engine for profile tests.
type fakeEngine struct {
	mu          sync.Mutex
	slots       []string
	loaded      map[string]string // slot → mode
	budgetUsed  int64
	unloadAllOK bool
	loadOK      bool

	// loadCalls/unloadCalls record every slot passed to Load/Unload, in
	// order — selective eviction tests assert on exactly which slots were
	// touched (e.g. an already-loaded target must never appear in either).
	loadCalls   []string
	unloadCalls []string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		slots:       []string{"a1", "a2", "a3", "a4"},
		loaded:      map[string]string{},
		budgetUsed:  50000 * 1024 * 1024, // 50000 MiB expressed as bytes (A1)
		unloadAllOK: true,
		loadOK:      true,
	}
}

func (f *fakeEngine) CurrentMode() string { return "" }
func (f *fakeEngine) Slots() []string     { return f.slots }
func (f *fakeEngine) SwitchMode(context.Context, string) engine.Result {
	return engine.Result{Success: true}
}
func (f *fakeEngine) Restart(context.Context) engine.Result { return engine.Result{Success: true} }
func (f *fakeEngine) Load(_ context.Context, mode, slot string) engine.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCalls = append(f.loadCalls, slot)
	if !f.loadOK {
		return engine.Result{Success: false, Message: "load failed (test)"}
	}
	f.loaded[slot] = mode
	return engine.Result{Success: true}
}
func (f *fakeEngine) Unload(_ context.Context, slot string) engine.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unloadCalls = append(f.unloadCalls, slot)
	// unloadAllOK gates every Unload call, not just "all" — selective
	// eviction (product/QA sprint, 2026-07-29) unloads slots individually
	// now, so a test simulating "eviction fails" must fail on the first
	// individual slot too, not just a literal "all" call that no longer
	// happens.
	if !f.unloadAllOK {
		return engine.Result{Success: false, Message: "unload failed (test)"}
	}
	if slot == "all" {
		f.loaded = map[string]string{}
		return engine.Result{Success: true}
	}
	delete(f.loaded, slot)
	return engine.Result{Success: true}
}
func (f *fakeEngine) CanFit(string) (engine.CanFit, error) {
	return engine.CanFit{Fits: true}, nil
}
func (f *fakeEngine) MemoryBudget() (engine.Budget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A1 bytes retrofit: budget figures are bytes. budgetUsed is a byte
	// count (see newFakeRunner: 50000 MiB expressed as bytes).
	return engine.Budget{TotalBytes: 120000 * 1024 * 1024, UsedBytes: f.budgetUsed, FreeBytes: 120000*1024*1024 - f.budgetUsed}, nil
}
func (f *fakeEngine) StartUnit(context.Context, string) error { return nil }
func (f *fakeEngine) StopUnit(context.Context, string) error  { return nil }

// testConfig builds a minimal config with one inference mode + 4 slots.

// seedConfigsRow inserts a minimal configs row (id=1, name "qwen3") so
// model_profiles/model_prefill_stats' FK to configs.id (0042 surrogate-key
// migration) has a real parent to point at — testConfig()'s
// Modes["qwen3"].ConfigID is set to match. Foreign keys toggled off just
// for this insert since a fully-seeded families/models/variants/artifacts/
// engines chain isn't otherwise needed by any test in this file.
func seedConfigsRow(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.SQL().Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("pragma off: %v", err)
	}
	if _, err := db.SQL().Exec(
		`INSERT INTO configs (id, name, variant_id, weight_artifact_id, engine_id) VALUES (1, 'qwen3', 1, 1, 1)`,
	); err != nil {
		t.Fatalf("seed configs: %v", err)
	}
	if _, err := db.SQL().Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("pragma on: %v", err)
	}
}

func testConfig() *config.Config {
	return &config.Config{
		Paths: config.Paths{
			ModelsDir: "/tmp/test-models",
			VulkanBin: "/tmp/test-vulkan-bin",
		},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
			"a2": {Unit: "forge-a2", Port: 8081, Label: "A2", Order: 2},
			"a3": {Unit: "forge-a3", Port: 8087, Label: "A3", Order: 3},
			"a4": {Unit: "forge-a4", Port: 8088, Label: "A4", Order: 4},
		},
		Modes: map[string]config.Mode{
			"qwen3": {
				// ConfigID matches seedConfigsRow's inserted configs.id=1
				// row (0042 surrogate-key migration — model_profiles is
				// keyed by config_id now, not the mode name).
				ConfigID: 1,
				Services: []config.Service{{
					Model:     "qwen3.gguf",
					Alias:     "qwen3-35b",
					Context:   4096,
					Backend:   "vulkan",
					ExtraArgs: []string{"--parallel", "2", "--ctx-checkpoints", "0"},
				}},
			},
		},
	}
}

// fakeReadMeta returns deterministic GGUF metadata without touching disk.
func fakeReadMeta(path string) (gguf.Metadata, error) {
	return gguf.Metadata{
		Architecture: "qwen2",
		QuantType:    "Q4_K_M",
		TrainedCtx:   32768,
	}, nil
}

func intSliceToCSV(tokens []int) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = strconv.Itoa(t)
	}
	return fmt.Sprintf("%s", strings.Join(parts, ","))
}

// newTestRunner builds a Runner wired to a fake engine, an httptest llama
// server, and an in-memory store. Returns the runner, the store, and the
// events bus for assertion.
func newTestRunner(t *testing.T, fe *fakeEngine) (*Runner, *store.DB, *bus.Bus, *httptest.Server) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	seedConfigsRow(t, db)

	events := bus.New()
	cfg := testConfig()

	// httptest server that responds to /props, /metrics, /tokenize, and /v1/completions.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/props":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"n_ctx": 4096}`)
		case r.URL.Path == "/metrics":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "llamacpp:prompt_tokens_seconds 800\nllamacpp:predicted_tokens_seconds 50\n")
		case r.URL.Path == "/tokenize":
			// Return a token count proportional to the input length (~3.5 chars/token).
			body, _ := io.ReadAll(r.Body)
			n := len(string(body)) / 7 // content field JSON overhead, rough
			if n < 10 {
				n = 10
			}
			tokens := make([]int, n)
			for i := range tokens {
				tokens[i] = i
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tokens":[%s]}`, intSliceToCSV(tokens))
		case r.URL.Path == "/v1/completions":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices":[{"text":"x"}],"timings":{"prompt_n":4096,"prompt_ms":5000,"predicted_n":128,"predicted_ms":2500,"prompt_per_second":819.2,"predicted_per_second":51.2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { srv.Close() })

	runner := New(Deps{
		Engine:   fe,
		Llama:    collector.NewLlamaClient(func(int) string { return srv.URL }),
		Profiles: db.ModelProfiles(),
		Publish:  events,
		Cfg:      func() *config.Config { return cfg },
		ReadMeta: fakeReadMeta,
		BaseURL:  func(int) string { return srv.URL },
		// Speed up test timing.
		SampleDuration:   100 * time.Millisecond,
		SampleInterval:   20 * time.Millisecond,
		GenerationTokens: 128,
		WarmupTokens:     16,
		HTTPClient:       &http.Client{Timeout: 5 * time.Second},
	})
	return runner, db, events, srv
}

func TestRunSuccess(t *testing.T) {
	fe := newFakeEngine()
	runner, db, events, _ := newTestRunner(t, fe)
	ctx := context.Background()

	// Subscribe to SSE events.
	sub := events.Subscribe(ctx)
	go func() {
		for ev := range sub {
			_ = ev // drain
		}
	}()

	result, err := runner.Run(ctx, RunRequest{Mode: "qwen3"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify measured values.
	if result.Mode != "qwen3" {
		t.Errorf("Mode: got %q want qwen3", result.Mode)
	}
	if result.ActualNCtx != 4096 {
		t.Errorf("ActualNCtx: got %d want 4096", result.ActualNCtx)
	}
	if result.Backend != "vulkan" {
		t.Errorf("Backend: got %q want vulkan", result.Backend)
	}
	if result.Parallel != 2 {
		t.Errorf("Parallel: got %d want 2", result.Parallel)
	}
	if result.DecodeTPS != 51.2 {
		t.Errorf("DecodeTPS: got %.1f want 51.2", result.DecodeTPS)
	}
	if result.PrefillTPS != 819.2 {
		t.Errorf("PrefillTPS: got %.1f want 819.2", result.PrefillTPS)
	}
	if result.SafeMemoryBytes <= 0 {
		t.Errorf("SafeMemoryBytes: got %d, want > 0", result.SafeMemoryBytes)
	}
	// 50000 MiB * 1.05 = 52500 MiB, in bytes.
	wantSafe := int64(52500 * 1024 * 1024)
	if result.SafeMemoryBytes != wantSafe {
		t.Errorf("SafeMemoryBytes: got %d want %d (50000 MiB * 1.05)", result.SafeMemoryBytes, wantSafe)
	}
	if result.Fingerprint == "" {
		t.Error("Fingerprint: empty")
	}

	// Verify the profile was stored.
	stored, err := db.ModelProfiles().Get(ctx, 1)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if stored.SafeMemoryBytes != result.SafeMemoryBytes {
		t.Errorf("stored SafeMemoryBytes: got %d want %d", stored.SafeMemoryBytes, result.SafeMemoryBytes)
	}
	if stored.DecodeTPS != result.DecodeTPS {
		t.Errorf("stored DecodeTPS: got %.1f want %.1f", stored.DecodeTPS, result.DecodeTPS)
	}

	// Verify the target slot was unloaded after the run.
	fe.mu.Lock()
	loaded := len(fe.loaded)
	fe.mu.Unlock()
	if loaded != 0 {
		t.Errorf("expected all slots unloaded after run, got %d loaded", loaded)
	}

	// Depth-sweep benchmarks (product/QA sprint, 2026-07-29): one row per
	// benchmarkDepthFractions entry, strictly increasing depth, and the
	// scalar Prefill/DecodeTPS must equal the first (TYPICAL, depth-0) row.
	if len(result.DepthBenchmarks) != len(benchmarkDepthFractions) {
		t.Fatalf("DepthBenchmarks: got %d rows, want %d", len(result.DepthBenchmarks), len(benchmarkDepthFractions))
	}
	if result.DepthBenchmarks[0].DepthTokens != 0 {
		t.Errorf("first depth benchmark should be at depth 0, got %d", result.DepthBenchmarks[0].DepthTokens)
	}
	for i := 1; i < len(result.DepthBenchmarks); i++ {
		if result.DepthBenchmarks[i].DepthTokens <= result.DepthBenchmarks[i-1].DepthTokens {
			t.Errorf("depths not strictly increasing: %+v", result.DepthBenchmarks)
		}
	}
	if result.DepthBenchmarks[0].PP2048TPS != result.PrefillTPS || result.DepthBenchmarks[0].TG128TPS != result.DecodeTPS {
		t.Errorf("scalar Prefill/DecodeTPS should equal the depth-0 (TYPICAL) row: scalar=(%.1f,%.1f) depth0=%+v",
			result.PrefillTPS, result.DecodeTPS, result.DepthBenchmarks[0])
	}

	// The benchmarks must also have round-tripped through the store.
	storedBenchmarks, err := db.ModelProfiles().Benchmarks(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Benchmarks: %v", err)
	}
	if len(storedBenchmarks) != len(result.DepthBenchmarks) {
		t.Errorf("stored benchmark count = %d, want %d", len(storedBenchmarks), len(result.DepthBenchmarks))
	}
}

// TestRunSelectiveEvictionSkipsAlreadyLoadedSlot covers the operator's own
// framing (product/QA sprint, 2026-07-29): "check if the model to be
// profiled is actually loaded first — if it is, only unload unneeded
// models." When qwen3 is already running in a2, profiling it must never
// call Load (it's already there) or Unload on a2 (evict only the OTHER
// slots), and must leave a2 loaded afterward (it wasn't ours to tear down).
func TestRunSelectiveEvictionSkipsAlreadyLoadedSlot(t *testing.T) {
	fe := newFakeEngine()
	fe.loaded["a2"] = "qwen3" // pre-existing load, before profiling starts
	runner, _, events, _ := newTestRunner(t, fe)
	runner.d.Snapshots = collector.NewStatic(&collector.Snapshot{
		Slots: map[string]collector.SlotState{
			"a2": {Slot: "a2", Mode: "qwen3", Port: 8081},
		},
	})

	ctx := context.Background()
	sub := events.Subscribe(ctx)
	go func() {
		for range sub {
		}
	}()

	result, err := runner.Run(ctx, RunRequest{Mode: "qwen3"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Mode != "qwen3" {
		t.Errorf("Mode: got %q", result.Mode)
	}

	fe.mu.Lock()
	defer fe.mu.Unlock()

	for _, s := range fe.loadCalls {
		if s == "a2" {
			t.Errorf("Load was called for the already-loaded slot a2: %v", fe.loadCalls)
		}
	}
	for _, s := range fe.unloadCalls {
		if s == "a2" {
			t.Errorf("Unload was called for the already-loaded slot a2: %v", fe.unloadCalls)
		}
	}
	// The other three slots must have been evicted (selective, not "all").
	evicted := map[string]bool{}
	for _, s := range fe.unloadCalls {
		evicted[s] = true
	}
	for _, s := range []string{"a1", "a3", "a4"} {
		if !evicted[s] {
			t.Errorf("expected slot %q to be evicted, unloadCalls=%v", s, fe.unloadCalls)
		}
	}
	// a2 must still be loaded after the run — it's the operator's
	// pre-existing state, not ours to remove just because profiling ran.
	if fe.loaded["a2"] != "qwen3" {
		t.Errorf("expected a2 to remain loaded with qwen3 after the run, got %+v", fe.loaded)
	}
}

// TestRunAbortsOnInferenceHang verifies that a stalled /v1/completions call
// is aborted using the collector's existing hang detector (the same
// requests_processing>0 + stalled-TPS signal V4's monitor.py used) rather
// than waiting out a blind wall-clock timeout. A completions handler that
// never returns would otherwise hold the profile run until completionTimeout
// expires (5+ minutes) — this test's Snapshots source reports an
// INFERENCE_HANG alert for the target port up front, so hangWatch must
// cancel the call on its first poll (HangWatchInterval, sped up here) well
// under a second.
func TestRunAbortsOnInferenceHang(t *testing.T) {
	fe := newFakeEngine()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	seedConfigsRow(t, db)

	events := bus.New()
	cfg := testConfig()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/props":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"n_ctx": 4096}`)
		case r.URL.Path == "/tokenize":
			body, _ := io.ReadAll(r.Body)
			n := len(string(body)) / 7
			if n < 10 {
				n = 10
			}
			tokens := make([]int, n)
			for i := range tokens {
				tokens[i] = i
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tokens":[%s]}`, intSliceToCSV(tokens))
		case r.URL.Path == "/v1/completions":
			// Simulate a genuinely stalled request: far outlast hangWatch's
			// poll interval (50ms in this test) before ever responding, so
			// the client must be the one to give up — not net/http's
			// server-side connection-close detection, which isn't a
			// reliable test signal (an abruptly closed socket doesn't
			// dependably cancel an already-dispatched handler's Context()).
			// Bounded, not infinite, so httptest.Server.Close() in cleanup
			// can't hang waiting for this handler to return.
			time.Sleep(500 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"timings":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { srv.Close() })

	// a1's configured port (testConfig) is 8080 — the hang alert must
	// be scoped to that port for hangWatch to match it.
	snapshots := collector.NewStatic(&collector.Snapshot{
		Alerts: []collector.Alert{{
			Code: "INFERENCE_HANG",
			Port: 8080,
			Msg:  "port 8080: active request stalled 90s (PP 0.0 tps, TG 0.0 tps)",
		}},
	})

	runner := New(Deps{
		Engine:            fe,
		Llama:             collector.NewLlamaClient(func(int) string { return srv.URL }),
		Profiles:          db.ModelProfiles(),
		Publish:           events,
		Cfg:               func() *config.Config { return cfg },
		ReadMeta:          fakeReadMeta,
		BaseURL:           func(int) string { return srv.URL },
		Snapshots:         snapshots,
		HangWatchInterval: 50 * time.Millisecond,
		SampleDuration:    100 * time.Millisecond,
		SampleInterval:    20 * time.Millisecond,
		GenerationTokens:  128,
		WarmupTokens:      16,
		Logf:              t.Logf,
	})

	start := time.Now()
	_, err = runner.Run(context.Background(), RunRequest{Mode: "qwen3"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run: expected an error from the simulated hang, got nil")
	}
	if !strings.Contains(err.Error(), "inference hang detected") {
		t.Errorf("Run error = %q, want it to mention the detected inference hang", err.Error())
	}
	if elapsed > 5*time.Second {
		t.Errorf("Run took %v to fail — hangWatch should have aborted within a few poll intervals, not waited on completionTimeout's backstop", elapsed)
	}
}

// TestRunDepthSweepPromptsNeverRepeat pins the actual bug the depth sweep
// replaces: the original single-benchmark design resent the SAME already-
// cached fill text (plus a tiny suffix) as the "prefill" measurement,
// making the reported number reflect ~10-20 fresh tokens, not a real
// prefill. This test's fake server records every /v1/completions prompt
// verbatim and fails if any two are byte-identical, and asserts each
// successive prompt is strictly longer than the last (append-only growth,
// never a reset-and-resend) — the actual property that makes a depth's
// pp2048 figure a genuine fresh-token measurement rather than a cache hit.
func TestRunDepthSweepPromptsNeverRepeat(t *testing.T) {
	fe := newFakeEngine()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	seedConfigsRow(t, db)

	events := bus.New()
	cfg := testConfig()

	var mu sync.Mutex
	seenPrompts := map[string]bool{}
	var promptLens []int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/props":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"n_ctx": 4096}`)
		case r.URL.Path == "/tokenize":
			body, _ := io.ReadAll(r.Body)
			n := len(string(body)) / 4
			if n < 10 {
				n = 10
			}
			tokens := make([]int, n)
			for i := range tokens {
				tokens[i] = i
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tokens":[%s]}`, intSliceToCSV(tokens))
		case r.URL.Path == "/v1/completions":
			var body struct {
				Prompt string `json:"prompt"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)

			mu.Lock()
			if seenPrompts[body.Prompt] {
				mu.Unlock()
				t.Errorf("a /v1/completions prompt was sent twice, byte-identical (len=%d) — this is exactly the prefix-cache-contamination bug the depth sweep must avoid", len(body.Prompt))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			seenPrompts[body.Prompt] = true
			promptLens = append(promptLens, len(body.Prompt))
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"timings":{"prompt_n":2048,"prompt_ms":1000,"predicted_n":128,"predicted_ms":2000,"prompt_per_second":2048.0,"predicted_per_second":64.0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { srv.Close() })

	runner := New(Deps{
		Engine:           fe,
		Llama:            collector.NewLlamaClient(func(int) string { return srv.URL }),
		Profiles:         db.ModelProfiles(),
		Publish:          events,
		Cfg:              func() *config.Config { return cfg },
		ReadMeta:         fakeReadMeta,
		BaseURL:          func(int) string { return srv.URL },
		SampleDuration:   50 * time.Millisecond,
		SampleInterval:   10 * time.Millisecond,
		GenerationTokens: 128,
		WarmupTokens:     16,
		HTTPClient:       &http.Client{Timeout: 5 * time.Second},
		Logf:             t.Logf,
	})

	ctx := context.Background()
	sub := events.Subscribe(ctx)
	go func() {
		for range sub {
		}
	}()

	result, err := runner.Run(ctx, RunRequest{Mode: "qwen3"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// One fill call per non-zero depth delta (3, since depth 0 has nothing
	// to fill) + one probe call per depth (4) = 7, at this small n_ctx.
	if len(promptLens) < len(benchmarkDepthFractions) {
		t.Fatalf("expected at least %d completions calls (one probe per depth), got %d", len(benchmarkDepthFractions), len(promptLens))
	}
	for i := 1; i < len(promptLens); i++ {
		if promptLens[i] <= promptLens[i-1] {
			t.Errorf("prompt length did not grow monotonically: call %d len=%d, call %d len=%d",
				i-1, promptLens[i-1], i, promptLens[i])
		}
	}
	if len(result.DepthBenchmarks) != len(benchmarkDepthFractions) {
		t.Errorf("DepthBenchmarks: got %d, want %d", len(result.DepthBenchmarks), len(benchmarkDepthFractions))
	}
}

func TestRunAlreadyRunning(t *testing.T) {
	fe := newFakeEngine()
	runner, _, _, _ := newTestRunner(t, fe)

	// Simulate an in-progress run.
	runner.mu.Lock()
	runner.running = true
	runner.mu.Unlock()

	_, err := runner.Run(context.Background(), RunRequest{Mode: "qwen3"})
	if err != ErrAlreadyRunning {
		t.Errorf("got %v, want ErrAlreadyRunning", err)
	}
}

// TestRunningMode (Sprint K, 2026-08-05): backs statusResponse.profiling,
// so a client reconnecting mid-run (SSE drop, or a fresh page load) sees
// which mode is holding the slots rather than nothing until the next
// profile:progress event.
func TestRunningMode(t *testing.T) {
	fe := newFakeEngine()
	runner, _, _, _ := newTestRunner(t, fe)

	if mode, running := runner.RunningMode(); running || mode != "" {
		t.Fatalf("idle runner: got mode=%q running=%v, want \"\"/false", mode, running)
	}

	runner.mu.Lock()
	runner.running = true
	runner.current = RunRequest{Mode: "qwen3"}
	runner.mu.Unlock()

	if mode, running := runner.RunningMode(); !running || mode != "qwen3" {
		t.Fatalf("running runner: got mode=%q running=%v, want \"qwen3\"/true", mode, running)
	}

	runner.mu.Lock()
	runner.running = false
	runner.mu.Unlock()

	if mode, running := runner.RunningMode(); running {
		t.Fatalf("after finish: got mode=%q running=%v, want running=false", mode, running)
	}
}

func TestRunEvictFails(t *testing.T) {
	fe := newFakeEngine()
	fe.unloadAllOK = false
	runner, _, events, _ := newTestRunner(t, fe)

	ctx := context.Background()
	sub := events.Subscribe(ctx)
	go func() {
		for range sub {
		}
	}()

	_, err := runner.Run(ctx, RunRequest{Mode: "qwen3"})
	if err == nil {
		t.Fatal("expected error from failed evict, got nil")
	}

	// Verify no profile was stored.
	_, storeErr := runner.d.Profiles.Get(ctx, 1)
	if storeErr == nil {
		t.Error("profile was stored despite failure")
	}
}

// TestLastError verifies the polling fallback's data source
// (docs/v5-profiling-benchmarks.md §10): a failed run's error is retrievable
// via LastError(mode) — used by handleProfileGet so a client polling GET
// /api/v1/profile/{mode} sees the failure without depending on the
// profile:failed SSE event — and is cleared once a later run for the same
// mode succeeds.
func TestLastError(t *testing.T) {
	fe := newFakeEngine()
	fe.unloadAllOK = false
	runner, _, events, _ := newTestRunner(t, fe)

	ctx := context.Background()
	sub := events.Subscribe(ctx)
	go func() {
		for range sub {
		}
	}()

	if _, _, ok := runner.LastError("qwen3"); ok {
		t.Fatal("expected no LastError before any run")
	}

	if _, err := runner.Run(ctx, RunRequest{Mode: "qwen3"}); err == nil {
		t.Fatal("expected error from failed evict, got nil")
	}

	msg, at, ok := runner.LastError("qwen3")
	if !ok {
		t.Fatal("expected LastError after failed run")
	}
	if msg == "" {
		t.Error("expected a non-empty error message")
	}
	if at == 0 {
		t.Error("expected a non-zero timestamp")
	}
	if _, _, ok := runner.LastError("other-mode"); ok {
		t.Error("LastError should be scoped to the mode that failed")
	}

	// A later successful run for the same mode clears it.
	fe.unloadAllOK = true
	if _, err := runner.Run(ctx, RunRequest{Mode: "qwen3"}); err != nil {
		t.Fatalf("expected success on retry, got: %v", err)
	}
	if _, _, ok := runner.LastError("qwen3"); ok {
		t.Error("expected LastError to be cleared after a successful run")
	}
}

func TestRunLoadFails(t *testing.T) {
	fe := newFakeEngine()
	fe.loadOK = false
	runner, _, _, _ := newTestRunner(t, fe)

	ctx := context.Background()
	_, err := runner.Run(ctx, RunRequest{Mode: "qwen3"})
	if err == nil {
		t.Fatal("expected error from failed load, got nil")
	}
}

func TestRunUnknownMode(t *testing.T) {
	fe := newFakeEngine()
	runner, _, _, _ := newTestRunner(t, fe)

	_, err := runner.Run(context.Background(), RunRequest{Mode: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown mode, got nil")
	}
}

func TestRunVLLMRejected(t *testing.T) {
	fe := newFakeEngine()
	runner, _, _, _ := newTestRunner(t, fe)
	// Override config to use vllm backend.
	runner.d.Cfg = func() *config.Config {
		cfg := testConfig()
		cfg.Modes["qwen3"].Services[0].Backend = "vllm"
		return cfg
	}

	_, err := runner.Run(context.Background(), RunRequest{Mode: "qwen3"})
	if err == nil {
		t.Fatal("expected error for vLLM mode, got nil")
	}
}

func TestRunContextReductionAborts(t *testing.T) {
	fe := newFakeEngine()
	runner, _, events, _ := newTestRunner(t, fe)

	// Override the httptest server to return a reduced n_ctx (2048 < 4096).
	// We need a new server that responds to /props with a smaller n_ctx.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/props":
			fmt.Fprintf(w, `{"n_ctx": 2048}`)
		case r.URL.Path == "/tokenize":
			fmt.Fprintf(w, `{"tokens": [1,2,3]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	runner.baseURL = func(int) string { return srv.URL }
	runner.d.Llama = collector.NewLlamaClient(func(int) string { return srv.URL })

	ctx := context.Background()
	sub := events.Subscribe(ctx)
	go func() {
		for range sub {
		}
	}()

	_, err := runner.Run(ctx, RunRequest{Mode: "qwen3"})
	if err == nil {
		t.Fatal("expected error from context reduction, got nil")
	}
	if !strings.Contains(err.Error(), "silently reduced") {
		t.Errorf("expected 'silently reduced' in error, got: %v", err)
	}

	// Verify no profile was stored (the profile would give false numbers).
	_, storeErr := runner.d.Profiles.Get(ctx, 1)
	if storeErr == nil {
		t.Error("profile was stored despite context reduction abort")
	}

	// Verify the target slot was unloaded (cleanup ran).
	fe.mu.Lock()
	loaded := len(fe.loaded)
	fe.mu.Unlock()
	if loaded != 0 {
		t.Errorf("expected all slots unloaded after abort, got %d loaded", loaded)
	}
}

func TestLookupFreshProfile(t *testing.T) {
	fe := newFakeEngine()
	runner, db, _, _ := newTestRunner(t, fe)
	ctx := context.Background()

	// Store a profile with a matching fingerprint.
	fp, err := runner.Fingerprint("qwen3")
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if err := db.ModelProfiles().Save(ctx, store.ModelProfile{
		ConfigID: 1, Mode: "qwen3", NCtx: 4096, Backend: "vulkan", Parallel: 2,
		SafeMemoryBytes: 24000 * 1024 * 1024, DecodeTPS: 50, PrefillTPS: 800,
		Fingerprint: fp, MeasuredAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// SafeMemoryBytes should return the profiled value.
	b, ok := runner.SafeMemoryBytes("qwen3")
	if !ok {
		t.Error("SafeMemoryBytes: not found")
	}
	if want := int64(24000 * 1024 * 1024); b != want {
		t.Errorf("SafeMemoryBytes: got %d want %d", b, want)
	}

	// DecodeTPS.
	tps, ok := runner.DecodeTPS("qwen3")
	if !ok {
		t.Error("DecodeTPS: not found")
	}
	if tps != 50 {
		t.Errorf("DecodeTPS: got %.1f want 50", tps)
	}

	// Profiled.
	if !runner.Profiled("qwen3") {
		t.Error("Profiled: got false want true")
	}
}

func TestLookupStaleProfile(t *testing.T) {
	fe := newFakeEngine()
	runner, db, _, _ := newTestRunner(t, fe)
	ctx := context.Background()

	// Store a profile with a WRONG fingerprint → stale.
	if err := db.ModelProfiles().Save(ctx, store.ModelProfile{
		ConfigID: 1, Mode: "qwen3", NCtx: 4096, Backend: "vulkan", Parallel: 2,
		SafeMemoryBytes: 24000 * 1024 * 1024, DecodeTPS: 50, PrefillTPS: 800,
		Fingerprint: "stale_fingerprint", MeasuredAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// SafeMemoryBytes should return ok=false (stale).
	_, ok := runner.SafeMemoryBytes("qwen3")
	if ok {
		t.Error("SafeMemoryBytes: expected ok=false for stale profile")
	}

	// Profiled should be false.
	if runner.Profiled("qwen3") {
		t.Error("Profiled: expected false for stale profile")
	}

	// But Get should still return the profile with Stale=true.
	result, found := runner.Get("qwen3")
	if !found {
		t.Fatal("Get: expected found=true")
	}
	if !result.Stale {
		t.Error("Get: expected Stale=true")
	}
}

// TestGetPopulatesConfigID is Phase 8's (pre-release feedback sprint)
// config_id wire-shape addition: Get must surface the config id it used to
// fetch the row, so the FE can join profiles to configs by id instead of by
// mode name. testConfig()'s "qwen3" mode has ConfigID: 1, matching
// seedConfigsRow's inserted configs.id=1 row.
func TestGetPopulatesConfigID(t *testing.T) {
	fe := newFakeEngine()
	runner, db, _, _ := newTestRunner(t, fe)
	ctx := context.Background()

	if err := db.ModelProfiles().Save(ctx, store.ModelProfile{
		ConfigID: 1, Mode: "qwen3", NCtx: 4096, Backend: "vulkan", Parallel: 2,
		SafeMemoryBytes: 24000 * 1024 * 1024, DecodeTPS: 50, PrefillTPS: 800,
		Fingerprint: "qwen2:Q4_K_M:32768:vulkan:2:4096::--parallel,2,--ctx-checkpoints,0",
		MeasuredAt:  time.Now(),
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}

	result, found := runner.Get("qwen3")
	if !found {
		t.Fatal("Get: expected found=true")
	}
	if result.ConfigID != 1 {
		t.Errorf("Get: ConfigID = %d, want 1", result.ConfigID)
	}
}

func TestLookupMissingProfile(t *testing.T) {
	fe := newFakeEngine()
	runner, _, _, _ := newTestRunner(t, fe)

	_, ok := runner.SafeMemoryBytes("nonexistent")
	if ok {
		t.Error("SafeMemoryBytes: expected ok=false for missing mode")
	}
	if runner.Profiled("nonexistent") {
		t.Error("Profiled: expected false for missing mode")
	}
}

// TestCompletionTimeoutScalesWithTokens pins the fix for a real bug: a flat
// 5-minute HTTP client timeout failed profiling runs on large-context modes
// regardless of what was actually being measured (e.g. nemotron's configured
// 1,048,576-token context can need far longer than 5 minutes just to
// prefill). The timeout must scale with token count instead of capping
// everything at the same arbitrary ceiling.
func TestCompletionTimeoutScalesWithTokens(t *testing.T) {
	runner := &Runner{d: Deps{MinTokensPerSecond: 20}}

	if got := runner.completionTimeout(0); got != 5*time.Minute {
		t.Errorf("zero tokens: got %v want floor 5m", got)
	}
	if got := runner.completionTimeout(100); got != 5*time.Minute {
		t.Errorf("small token count: got %v want floor 5m", got)
	}

	const nemotronCtx = 1048576
	got := runner.completionTimeout(nemotronCtx)
	want := time.Duration(float64(nemotronCtx) / 20.0 * float64(time.Second))
	if got != want {
		t.Errorf("large token count: got %v want %v", got, want)
	}
	if got <= 5*time.Minute {
		t.Errorf("large token count timeout %v should exceed the old flat 5-minute cap", got)
	}
}

func TestParseParallel(t *testing.T) {
	tests := []struct {
		args []string
		want int
	}{
		{[]string{"--parallel", "4"}, 4},
		{[]string{"--parallel=3"}, 3},
		{[]string{"--ctx-checkpoints", "0", "--parallel", "2"}, 2},
		{[]string{}, 1}, // default
		{[]string{"--parallel", "abc"}, 1},
	}
	for _, tt := range tests {
		got := parseParallel(tt.args)
		if got != tt.want {
			t.Errorf("parseParallel(%v) = %d, want %d", tt.args, got, tt.want)
		}
	}
}

func TestGenerateFill(t *testing.T) {
	// Deterministic: same nCtx → same output.
	a := generateFill(4096)
	b := generateFill(4096)
	if a != b {
		t.Error("generateFill is not deterministic")
	}
	// Size scales with nCtx.
	small := generateFill(1024)
	large := generateFill(8192)
	if len(large) <= len(small) {
		t.Error("generateFill does not scale with nCtx")
	}
	// Contains heterogeneous content (code + prose).
	if len(a) < 1024 {
		t.Errorf("fill too short: %d chars", len(a))
	}
}

// TestSizeFillConvergesUnderNonUniformDensity pins the fix for a real bug
// found live-profiling nemotron-puzzle at its full 1,048,576-token context:
// a single average-density proportional cut sized a fill to 1,086,904
// tokens — over both the target and the actual context size — because the
// retained prefix was locally denser than the corpus-wide average used to
// size the cut. This test's /tokenize handler models exactly that: token
// count includes a large constant term, so a shorter slice's token count
// doesn't shrink proportionally with its length the way a single
// average-based cut assumes. sizeFill must re-measure the actual candidate
// and iterate rather than trust the first estimate.
func TestSizeFillConvergesUnderNonUniformDensity(t *testing.T) {
	fe := newFakeEngine()
	_ = fe

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	seedConfigsRow(t, db)

	var tokenizeCalls int32
	const denseOffset = 2000 // tokens present regardless of text length
	const charsPerToken = 6

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&tokenizeCalls, 1)
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := len(body.Content)/charsPerToken + denseOffset
		tokens := make([]int, n)
		for i := range tokens {
			tokens[i] = i
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tokens":[%s]}`, intSliceToCSV(tokens))
	}))
	t.Cleanup(func() { srv.Close() })

	runner := New(Deps{
		Profiles: db.ModelProfiles(),
		BaseURL:  func(int) string { return srv.URL },
	})

	const targetTokens = 8000 // achievable: denseOffset(2000) < targetTokens
	fillText, err := runner.sizeFill(context.Background(), 8080, targetTokens)
	if err != nil {
		t.Fatalf("sizeFill: %v", err)
	}

	// Verify against the exact same formula the fake server used — the
	// invariant sizeFill must uphold regardless of how many iterations it took.
	actualTokens := len(fillText)/charsPerToken + denseOffset
	if actualTokens > targetTokens {
		t.Errorf("sizeFill overshot: fill measures %d tokens, want <= %d (target)", actualTokens, targetTokens)
	}

	calls := atomic.LoadInt32(&tokenizeCalls)
	if calls < 2 {
		t.Errorf("expected sizeFill to iterate (>1 /tokenize call) under non-uniform density, got %d call(s)", calls)
	}
}

func TestFingerprintDeterministic(t *testing.T) {
	fe := newFakeEngine()
	runner, _, _, _ := newTestRunner(t, fe)

	fp1, err := runner.Fingerprint("qwen3")
	if err != nil {
		t.Fatalf("Fingerprint 1: %v", err)
	}
	fp2, err := runner.Fingerprint("qwen3")
	if err != nil {
		t.Fatalf("Fingerprint 2: %v", err)
	}
	if fp1 != fp2 {
		t.Error("Fingerprint is not deterministic")
	}
	if len(fp1) != 64 { // sha256 hex
		t.Errorf("Fingerprint length: got %d want 64", len(fp1))
	}
}

func TestSSEEventsPublished(t *testing.T) {
	fe := newFakeEngine()
	runner, _, events, _ := newTestRunner(t, fe)
	ctx := context.Background()

	// Collect SSE events.
	sub := events.Subscribe(ctx)
	var names []string
	var done int32
	go func() {
		for ev := range sub {
			names = append(names, ev.Name)
			if ev.Name == EventDone || ev.Name == EventFailed {
				atomic.AddInt32(&done, 1)
				return
			}
		}
	}()

	_, err := runner.Run(ctx, RunRequest{Mode: "qwen3"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait for event drain.
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&done) == 0 {
		t.Fatal("no profile:done event received")
	}

	// Verify expected event sequence.
	hasStarted := false
	hasProgress := false
	hasDone := false
	for _, n := range names {
		switch n {
		case EventStarted:
			hasStarted = true
		case EventProgress:
			hasProgress = true
		case EventDone:
			hasDone = true
		}
	}
	if !hasStarted {
		t.Error("missing profile:started event")
	}
	if !hasProgress {
		t.Error("missing profile:progress event")
	}
	if !hasDone {
		t.Error("missing profile:done event")
	}

	// Verify progress phases.
	var progressPhases []string
	for _, n := range names {
		if n == EventProgress {
			progressPhases = append(progressPhases, n)
		}
	}
	if len(progressPhases) < 3 {
		t.Errorf("expected at least 3 progress events, got %d", len(progressPhases))
	}
}

func TestProfileJSONSerialization(t *testing.T) {
	r := Result{
		Mode: "qwen3", SafeMemoryBytes: 24000 * 1024 * 1024, DecodeTPS: 55.2,
		PrefillTPS: 800.5, ActualNCtx: 4096, Stale: false,
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Result
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Mode != r.Mode || back.SafeMemoryBytes != r.SafeMemoryBytes || back.DecodeTPS != r.DecodeTPS {
		t.Errorf("round trip mismatch:\n got  %+v\n want %+v", back, r)
	}
}
