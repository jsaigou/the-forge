// SPDX-License-Identifier: Apache-2.0

// Package profile implements the model profiling + benchmark feature
// (docs/v5-profiling-benchmarks.md). An operator triggers a profile run from
// the Settings page; the run unloads ALL of A1–A4, loads one model alone at
// max context, fills the KV cache with heterogeneous data, measures the
// stable safe peak memory footprint (additive gtt_used + unified_RSS per
// docs/pitfalls.md), and runs a fixed generation to read prefill/decode T/s
// from llama.cpp's own timings. Results are stored in model_profiles and
// consumed by the fit check (engine.FitPlan needMB), the cost formula
// (BE-COST decode_tps), and the model card (FE-2).
//
// The run is DESTRUCTIVE on prod: it evicts live models that OpenCode/LibreChat
// and users may be using. The HTTP handler gates it behind admin + step-up and
// a confirmation dialog. Progress streams over the SSE bus
// (profile:started|progress|done|failed).
package profile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/store"
)

// SSE event names (Contract 1 amendment — colon-form, like registry:refreshed
// and tts:job_update).
const (
	EventStarted  = "profile:started"
	EventProgress = "profile:progress"
	EventDone     = "profile:done"
	EventFailed   = "profile:failed"
)

// ErrAlreadyRunning is returned when a profile run is already in progress.
var ErrAlreadyRunning = errors.New("profile: a run is already in progress")

// RunRequest is the input to a profile run.
type RunRequest struct {
	Mode string // the config mode name to profile
}

// Result is the measured outcome of a successful profile run.
//
// A1 (bytes retrofit, 2026-07-24): SafeMemoryBytes is bytes (was
// SafeMemoryMB). The wire key changed to safe_memory_bytes; the PWA types
// move with it.
type Result struct {
	Mode string `json:"mode"`
	// ConfigID is the catalog config this profile belongs to (Phase 8,
	// pre-release feedback sprint — model_profiles.config_id, real since
	// the Phase 6 surrogate-key migration). The FE joins profiles to
	// configs by this id now, with Mode==configs.name kept only as a
	// fallback for the deploy window where a new bundle meets an older
	// binary.
	ConfigID        int64  `json:"config_id"`
	ModelID         string `json:"model_id"`
	NCtx            int    `json:"n_ctx"`
	ActualNCtx      int    `json:"actual_n_ctx"`
	Backend         string `json:"backend"`
	Parallel        int    `json:"parallel"`
	SafeMemoryBytes int64  `json:"safe_memory_bytes"`
	// PrefillTPS/DecodeTPS are the TYPICAL figures — measured at depth 0
	// (empty context), the first row of DepthBenchmarks. Kept as scalars
	// (not folded into DepthBenchmarks-only) because the fit check
	// (engine.FitPlan) and cost formula (BE-COST) already consume exactly
	// these two fields and don't need the full curve.
	PrefillTPS  float64 `json:"prefill_tps"`
	DecodeTPS   float64 `json:"decode_tps"`
	Fingerprint string  `json:"fingerprint"`
	Stale       bool    `json:"stale"`
	MeasuredAt  int64   `json:"measured_at"`

	// DepthBenchmarks is the full depth-sweep curve (product/QA sprint,
	// 2026-07-29), ordered by DepthTokens ascending: index 0 is the same
	// measurement as PrefillTPS/DecodeTPS above (TYPICAL); the last entry
	// is WORST CASE (measured at ~full context). The FE shows first+last
	// by default and the full curve behind "Show more".
	DepthBenchmarks []DepthBenchmark `json:"depth_benchmarks"`
}

// DepthBenchmark is one throughput measurement at a specific KV-cache
// depth. PP2048TPS is prefill throughput for a FRESH 2048-token prompt
// appended at that depth — never a repeat of already-cached content, which
// was the bug in the original single-benchmark design (see runDepthSweep).
// TG128TPS is decode throughput for the 128 tokens generated right after.
type DepthBenchmark struct {
	DepthTokens int     `json:"depth_tokens"`
	PP2048TPS   float64 `json:"pp2048_tps"`
	TG128TPS    float64 `json:"tg128_tps"`
}

// benchmarkDepthFractions are the KV-cache depths (as a fraction of n_ctx)
// the throughput sweep measures: empty, 25%, 50%, ~full. Decided in the
// product/QA sprint (2026-07-29) specifically to fix two things: (1) the
// original design's single benchmark call measured prefill against an
// already-warmed cache (near-zero fresh tokens, meaningless number) — see
// this file's package doc; (2) decode-at-max-context is valuable and
// rarely tested elsewhere, but showing ONLY that number with no depth
// label made it look like an arbitrary, unlabeled figure.
var benchmarkDepthFractions = []float64{0, 0.25, 0.5, 1.0}

// Lookup is the read-side interface consumed by the fit check
// (engine.FitPlan needBytes), the cost formula (BE-COST), and the card
// (FE-2). The Runner implements it; consumers get it injected. Lookups
// never block on the run lock — they read from the store and recompute the
// fingerprint (cheap: file stat + GGUF header, both O(1)).
//
// A1 (bytes retrofit): SafeMemoryBytes returns bytes (was SafeMemoryMB).
type Lookup interface {
	// SafeMemoryBytes returns the profiled safe-memory footprint (bytes)
	// for a mode at its current config, or ok=false when no fresh
	// (non-stale) profile exists. This is the authoritative needBytes for
	// engine.FitPlan.
	SafeMemoryBytes(mode string) (bytes int64, ok bool)

	// DecodeTPS returns the measured decode tok/s for a mode, or ok=false.
	// BE-COST derives cost_per_1M = power_kW × (1e6/(tps×3600)) × rate.
	DecodeTPS(mode string) (tps float64, ok bool)

	// PrefillTPS returns the measured prefill tok/s, or ok=false.
	PrefillTPS(mode string) (tps float64, ok bool)

	// Profiled reports whether a fresh (non-stale) profile exists for a mode.
	Profiled(mode string) bool

	// Get returns the full profile for a mode (including staleness), or
	// ok=false when no profile exists. Used by the API + card.
	Get(mode string) (Result, bool)
}

// Deps wires a Runner.
type Deps struct {
	Engine   engine.Engine                            // for Unload("all"), Load(mode, slot), MemoryBudget()
	Llama    *collector.LlamaClient                   // for NCtx (/props) + Metrics (/metrics fallback)
	Profiles store.ModelProfiles                      // persistence
	Publish  bus.Publisher                            // SSE progress
	Cfg      func() *config.Config                    // mode/slot/port config
	ReadMeta func(path string) (gguf.Metadata, error) // fingerprinting (default gguf.ReadMetadata)
	BaseURL  func(port int) string                    // llama-server probe URL (tests override)
	Now      func() time.Time
	Logf     func(format string, args ...any)

	// Probe optionally triggers an immediate collector poll. Called after
	// each engine lifecycle op (unload/load) so the Dashboard/Console
	// reflect slot state changes immediately rather than waiting for the
	// collector cadence (default 2s). nil = no early probe (tests).
	Probe func(ctx context.Context)

	// MarginPct is the safety margin added to the measured peak (default 5).
	// Overridable via the profile.memory_margin_pct setting.
	MarginPct float64

	// SampleDuration is how long to sample memory at steady state (default 10s).
	SampleDuration time.Duration

	// SampleInterval is the cadence of memory samples (default 2s).
	SampleInterval time.Duration

	// GenerationTokens is the fixed generation length for the T/s benchmark
	// (default 128 — this is the "128" in "tg128").
	GenerationTokens int

	// PrefillProbeTokens is the fixed fresh-prompt size for the depth-sweep
	// prefill measurement (default 2048 — the "2048" in "pp2048"). Appended
	// fresh (never a repeat of already-cached content) at each sweep depth.
	PrefillProbeTokens int

	// WarmupTokens is the warm-up generation length, discarded (default 16).
	WarmupTokens int

	// HTTPClient overrides the default client for /v1/completions (tests).
	// The default client carries no fixed Timeout — per-call context
	// deadlines (see completionTimeout) bound requests instead, scaled to
	// how many tokens the call will actually process.
	HTTPClient *http.Client

	// Snapshots gives an in-flight /v1/completions call a real liveness
	// signal instead of trusting a wall-clock guess alone: while a fill or
	// benchmark call is outstanding, hangWatch polls the collector's
	// existing hang detector (collector.hangDetector — the same
	// requests_processing>0 + stalled-TPS rule V4's monitor.py used) for
	// the target port and cancels the call the moment a genuine stall is
	// reported, well before completionTimeout's backstop would fire. nil
	// disables this (tests) — completionTimeout alone still bounds the call.
	Snapshots collector.Source

	// HangWatchInterval is the poll cadence for hangWatch (default 5s,
	// matching the collector's own scrape cadence closely enough to catch a
	// real stall promptly). Overridable to speed up tests.
	HangWatchInterval time.Duration

	// MinTokensPerSecond is the conservative floor throughput assumed when
	// sizing the /v1/completions and /tokenize timeout for a given token
	// count (default 20). A flat wall-clock timeout regardless of context
	// size failed real profiling runs on large-context modes (nemotron at
	// 1,048,576 tokens can need far longer to prefill than a 4K-context
	// mode) — this scales the budget with the actual size being measured
	// instead. Profiling is a rare, operator-triggered, already-destructive
	// run, so erring toward "wait longer" costs nothing but time.
	MinTokensPerSecond float64
}

// Runner executes profile runs and implements Lookup.
type Runner struct {
	d       Deps
	http    *http.Client
	baseURL func(port int) string

	mu      sync.Mutex
	running bool
	current RunRequest

	// lastErr* record the most recent failure per mode, so the FE's polling
	// fallback (GET /api/v1/profile/{mode}) can surface a failure without
	// depending on SSE delivery — see docs/v5-profiling-benchmarks.md §10.
	// Cleared when a later run for the same mode succeeds.
	lastErrMode string
	lastErrMsg  string
	lastErrAt   time.Time
}

// New returns a Runner ready to execute profile runs.
func New(d Deps) *Runner {
	if d.ReadMeta == nil {
		d.ReadMeta = gguf.ReadMetadata
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.BaseURL == nil {
		d.BaseURL = func(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }
	}
	if d.MarginPct == 0 {
		d.MarginPct = 5
	}
	if d.SampleDuration == 0 {
		d.SampleDuration = 10 * time.Second
	}
	if d.SampleInterval == 0 {
		d.SampleInterval = 2 * time.Second
	}
	if d.GenerationTokens == 0 {
		d.GenerationTokens = 128
	}
	if d.PrefillProbeTokens == 0 {
		d.PrefillProbeTokens = 2048
	}
	if d.WarmupTokens == 0 {
		d.WarmupTokens = 16
	}
	if d.MinTokensPerSecond <= 0 {
		d.MinTokensPerSecond = 20
	}
	if d.HangWatchInterval == 0 {
		d.HangWatchInterval = 5 * time.Second
	}
	hc := d.HTTPClient
	if hc == nil {
		// No client-level Timeout: a flat cap here would re-impose the same
		// arbitrary ceiling completionTimeout is designed to avoid. Per-call
		// context deadlines (set by callers via completionTimeout) bound
		// each request instead.
		hc = &http.Client{}
	}
	return &Runner{d: d, http: hc, baseURL: d.BaseURL}
}

var _ Lookup = (*Runner)(nil)

// IsRunning reports whether a profile run is currently in progress.
func (r *Runner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// RunningMode reports the mode currently being profiled, and whether a run
// is in progress at all (Sprint K, 2026-08-05 — statusResponse.profiling,
// so Bay.tsx can show which slots a run is holding even on a fresh page
// load / SSE reconnect, not just while profile:progress events are live).
func (r *Runner) RunningMode() (mode string, running bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current.Mode, r.running
}

// Delete clears a stored profile for a mode. Used to remove a bad profile
// (e.g. one measured at the wrong n_ctx before the context-reduction guard).
func (r *Runner) Delete(ctx context.Context, mode string) error {
	configID, ok := r.configID(mode)
	if !ok {
		return fmt.Errorf("profile: delete: unknown mode %q", mode)
	}
	return r.d.Profiles.Delete(ctx, configID)
}

// configID resolves a mode name to its catalog config id (0042 — model_profiles
// is keyed by config_id, not mode name) via the merged-config seam's
// already-populated config.Mode.ConfigID (models.toml -> DB sprint, Phase B2).
func (r *Runner) configID(mode string) (int64, bool) {
	m, ok := r.d.Cfg().Modes[mode]
	if !ok || m.ConfigID == 0 {
		return 0, false
	}
	return m.ConfigID, true
}

// Run executes one profile run. Blocking — callers goroutine it. The run:
// evict all → load target → verify n_ctx → fill → measure memory →
// warm-up → benchmark → record → unload. Progress is published on the SSE
// bus at each phase. On any error, profile:failed is published and the
// target slot is unloaded (A1–A4 are left unloaded per the §8 decision).
func (r *Runner) Run(ctx context.Context, req RunRequest) (Result, error) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return Result{}, ErrAlreadyRunning
	}
	r.running = true
	r.current = req
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.current = RunRequest{}
		r.mu.Unlock()
	}()

	cfg := r.d.Cfg()
	mode, ok := cfg.Modes[req.Mode]
	if !ok {
		err := fmt.Errorf("profile: unknown mode %q", req.Mode)
		r.publishFailed(ctx, req.Mode, "config", err)
		return Result{}, err
	}
	if mode.Type == "service" || len(mode.Services) == 0 {
		err := fmt.Errorf("profile: mode %q is not an inference mode", req.Mode)
		r.publishFailed(ctx, req.Mode, "config", err)
		return Result{}, err
	}
	svc := mode.Services[0]
	if svc.Backend == "vllm" {
		err := fmt.Errorf("profile: vLLM profiling not yet supported (mode %q)", req.Mode)
		r.publishFailed(ctx, req.Mode, "config", err)
		return Result{}, err
	}

	startedAt := r.d.Now()
	r.d.Publish.Publish(EventStarted, map[string]any{
		"mode":       req.Mode,
		"started_at": startedAt.Unix(),
	})
	r.logf("profile: started for mode %q", req.Mode)

	result := Result{Mode: req.Mode, Backend: svc.Backend, NCtx: svc.Context}
	result.Parallel = parseParallel(svc.ExtraArgs)

	// Resolve target slot. Selective eviction (product/QA sprint,
	// 2026-07-29 — the operator's own framing: "check if the model to be
	// profiled is actually loaded first; if it is, only unload unneeded
	// models"): if req.Mode is already running in some slot, profile it
	// there in place and never touch that slot — only the OTHER slots get
	// evicted. This also means we must not unload it afterward (Phase 8)
	// or on failure (cleanup below) — it wasn't ours to evict, so it isn't
	// ours to remove either. If it isn't loaded anywhere, fall back to the
	// original behavior: first slot by Order.
	slots := r.d.Engine.Slots()
	if len(slots) == 0 {
		err := errors.New("profile: no configured slots")
		r.publishFailed(ctx, req.Mode, "config", err)
		return Result{}, err
	}
	targetSlot, wasAlreadyLoaded := r.findLoadedSlot(req.Mode)
	if targetSlot == "" {
		targetSlot = slots[0]
	}
	slotCfg, ok := cfg.Slots[targetSlot]
	if !ok {
		err := fmt.Errorf("profile: slot %q not in config", targetSlot)
		r.publishFailed(ctx, req.Mode, "config", err)
		return Result{}, err
	}
	port := slotCfg.Port

	// Phase 1: Evict every slot EXCEPT the target (or every slot, if the
	// target isn't loaded anywhere — same as evicting "all" used to mean,
	// just expressed per-slot so the target is provably untouched when it
	// was already running).
	r.publishProgress(ctx, req.Mode, "evicting", map[string]any{"target_slot": targetSlot, "already_loaded": wasAlreadyLoaded})
	for _, slot := range slots {
		if slot == targetSlot {
			continue
		}
		if res := r.d.Engine.Unload(ctx, slot); !res.Success {
			err := fmt.Errorf("profile: evict %q failed: %s", slot, res.Message)
			r.publishFailed(ctx, req.Mode, "evicting", err)
			return Result{}, err
		}
	}
	r.logf("profile: evicted all slots except target %q (already_loaded=%v)", targetSlot, wasAlreadyLoaded)
	r.probe(ctx) // refresh snapshot so Dashboard sees the slot changes immediately

	// Phase 2: Load target — unless it's already running there.
	if !wasAlreadyLoaded {
		r.publishProgress(ctx, req.Mode, "loading", map[string]any{"slot": targetSlot})
		if res := r.d.Engine.Load(ctx, req.Mode, targetSlot); !res.Success {
			err := fmt.Errorf("profile: load %q into %q failed: %s", req.Mode, targetSlot, res.Message)
			r.publishFailed(ctx, req.Mode, "loading", err)
			return Result{}, err
		}
		r.logf("profile: loaded %q into slot %q (port %d)", req.Mode, targetSlot, port)
		r.probe(ctx) // refresh snapshot so Dashboard sees the loaded slot
	}

	// Ensure the target is unloaded on any failure after this point — but
	// only if we were the ones who loaded it. A pre-existing load is the
	// operator's state, not ours to tear down just because profiling failed.
	cleanup := func(phase string, err error) error {
		wrapped := fmt.Errorf("profile: %s: %w", phase, err)
		r.publishFailed(ctx, req.Mode, phase, wrapped)
		if !wasAlreadyLoaded {
			if ures := r.d.Engine.Unload(ctx, targetSlot); !ures.Success {
				r.logf("profile: cleanup unload of %q failed: %s", targetSlot, ures.Message)
			}
		}
		return wrapped
	}

	// Phase 3: Verify actual n_ctx via /props.
	actualCtx, err := r.d.Llama.NCtx(ctx, port)
	if err != nil {
		return Result{}, cleanup("verifying", fmt.Errorf("/props: %w", err))
	}
	result.ActualNCtx = actualCtx
	r.publishProgress(ctx, req.Mode, "verifying", map[string]any{
		"actual_n_ctx": actualCtx,
		"target_n_ctx": result.NCtx,
	})

	// ABORT if the kernel silently reduced the context. A profile at the
	// wrong n_ctx gives false memory numbers — the KV cache allocation
	// scales with n_ctx, so a reduced context means a smaller footprint
	// than the operator will actually see at the configured context.
	// The operator must fix the GTT/allocation issue and retry.
	if actualCtx < result.NCtx {
		err := fmt.Errorf(
			"actual n_ctx %d < configured %d — the kernel silently reduced the context (GTT allocation failure). "+
				"The profile would give false memory numbers. Fix the allocation issue and retry.",
			actualCtx, result.NCtx)
		return Result{}, cleanup("verifying", err)
	}
	// The profile is valid at the actual ctx.
	if actualCtx > 0 {
		result.NCtx = actualCtx
	}

	// Phases 4-6: depth sweep. Grows one conversation across all
	// benchmarkDepthFractions checkpoints (empty → 25% → 50% → ~full),
	// measuring real pp2048+tg128 throughput at each depth, and samples
	// peak memory once the conversation reaches its deepest point (still
	// "measure at full context" like the original design, just now the
	// last step of the sweep rather than a separate fill).
	benchmarks, peakBytes, err := r.runDepthSweep(ctx, req.Mode, port, result.NCtx)
	if err != nil {
		return Result{}, cleanup("benchmarking", err)
	}
	margin := 1.0 + r.d.MarginPct/100.0
	safeBytes := int64(float64(peakBytes) * margin)
	result.SafeMemoryBytes = safeBytes
	r.logf("profile: peak memory %d bytes → safe %d bytes (+%.0f%%)", peakBytes, safeBytes, r.d.MarginPct)
	result.DepthBenchmarks = benchmarks
	// TYPICAL = depth 0, the first sweep point — see Result's doc comment.
	if len(benchmarks) > 0 {
		result.PrefillTPS = benchmarks[0].PP2048TPS
		result.DecodeTPS = benchmarks[0].TG128TPS
	}
	if result.DecodeTPS == 0 {
		// Fallback: read /metrics gauges (older builds without timings).
		if m := r.d.Llama.Metrics(ctx, port); m != nil {
			result.PrefillTPS = m.PromptTPS
			result.DecodeTPS = m.PredictedTPS
		}
	}
	r.logf("profile: typical prefill %.1f t/s, decode %.1f t/s (%d depth points)",
		result.PrefillTPS, result.DecodeTPS, len(benchmarks))

	// Phase 7: Compute fingerprint + record.
	fp, err := r.Fingerprint(req.Mode)
	if err != nil {
		return Result{}, cleanup("fingerprint", err)
	}
	result.Fingerprint = fp
	result.MeasuredAt = r.d.Now().Unix()
	result.ModelID = resolveModelID(cfg, req.Mode)

	storeBenchmarks := make([]store.ModelProfileBenchmark, len(benchmarks))
	for i, b := range benchmarks {
		storeBenchmarks[i] = store.ModelProfileBenchmark{
			DepthTokens: b.DepthTokens, PP2048TPS: b.PP2048TPS, TG128TPS: b.TG128TPS,
		}
	}
	configID, ok := r.configID(req.Mode)
	if !ok {
		return Result{}, cleanup("recording", fmt.Errorf("profile: unknown mode %q (no catalog config_id)", req.Mode))
	}
	if err := r.d.Profiles.Save(ctx, store.ModelProfile{
		ConfigID:        configID,
		Mode:            result.Mode,
		ModelID:         result.ModelID,
		NCtx:            result.NCtx,
		Backend:         result.Backend,
		Parallel:        result.Parallel,
		SafeMemoryBytes: result.SafeMemoryBytes,
		PrefillTPS:      result.PrefillTPS,
		DecodeTPS:       result.DecodeTPS,
		ActualNCtx:      result.ActualNCtx,
		Fingerprint:     result.Fingerprint,
		MeasuredAt:      r.d.Now(),
	}, storeBenchmarks); err != nil {
		return Result{}, cleanup("recording", err)
	}
	r.logf("profile: recorded for mode %q", req.Mode)
	r.mu.Lock()
	if r.lastErrMode == req.Mode {
		r.lastErrMode = ""
		r.lastErrMsg = ""
	}
	r.mu.Unlock()

	// Phase 8: Unload target — but only if we were the ones who loaded it
	// (selective eviction, see the resolve-target comment above). A
	// pre-existing load is left running exactly as the operator had it.
	if !wasAlreadyLoaded {
		if res := r.d.Engine.Unload(ctx, targetSlot); !res.Success {
			r.logf("profile: final unload of %q failed: %s", targetSlot, res.Message)
		}
	}
	r.probe(ctx) // refresh snapshot so Dashboard sees the slot changes

	// Publish done.
	result.Stale = false
	r.d.Publish.Publish(EventDone, map[string]any{"result": result})
	return result, nil
}

// findLoadedSlot reports which slot (if any) currently has mode loaded, per
// the latest collector snapshot. "", false when unknown/not loaded — the
// caller falls back to the original first-slot-by-Order behavior in that
// case, same as before selective eviction existed.
func (r *Runner) findLoadedSlot(mode string) (slot string, loaded bool) {
	if r.d.Snapshots == nil {
		return "", false
	}
	snap := r.d.Snapshots.Current()
	if snap == nil {
		return "", false
	}
	for name, st := range snap.Slots {
		if st.Mode == mode {
			return name, true
		}
	}
	return "", false
}

// ── Lookup implementation ───────────────────────────────────────────────────

// SafeMemoryBytes implements Lookup.
func (r *Runner) SafeMemoryBytes(mode string) (int64, bool) {
	p, ok := r.Get(mode)
	if !ok || p.Stale {
		return 0, false
	}
	return p.SafeMemoryBytes, true
}

// DecodeTPS implements Lookup.
func (r *Runner) DecodeTPS(mode string) (float64, bool) {
	p, ok := r.Get(mode)
	if !ok || p.Stale {
		return 0, false
	}
	return p.DecodeTPS, true
}

// PrefillTPS implements Lookup.
func (r *Runner) PrefillTPS(mode string) (float64, bool) {
	p, ok := r.Get(mode)
	if !ok || p.Stale {
		return 0, false
	}
	return p.PrefillTPS, true
}

// Profiled implements Lookup.
func (r *Runner) Profiled(mode string) bool {
	p, ok := r.Get(mode)
	return ok && !p.Stale
}

// Get implements Lookup — returns the stored profile + staleness.
func (r *Runner) Get(mode string) (Result, bool) {
	ctx := context.Background()
	configID, ok := r.configID(mode)
	if !ok {
		return Result{}, false
	}
	stored, err := r.d.Profiles.Get(ctx, configID)
	if err != nil {
		return Result{}, false
	}
	currentFP, fpErr := r.Fingerprint(mode)
	stale := true
	if fpErr == nil && currentFP == stored.Fingerprint {
		stale = false
	}

	var depthBenchmarks []DepthBenchmark
	if rows, err := r.d.Profiles.Benchmarks(ctx, stored.ID); err == nil {
		depthBenchmarks = make([]DepthBenchmark, len(rows))
		for i, row := range rows {
			depthBenchmarks[i] = DepthBenchmark{
				DepthTokens: row.DepthTokens, PP2048TPS: row.PP2048TPS, TG128TPS: row.TG128TPS,
			}
		}
	}

	return Result{
		Mode:            stored.Mode,
		ConfigID:        configID,
		ModelID:         stored.ModelID,
		NCtx:            stored.NCtx,
		ActualNCtx:      stored.ActualNCtx,
		Backend:         stored.Backend,
		Parallel:        stored.Parallel,
		SafeMemoryBytes: stored.SafeMemoryBytes,
		PrefillTPS:      stored.PrefillTPS,
		DecodeTPS:       stored.DecodeTPS,
		Fingerprint:     stored.Fingerprint,
		Stale:           stale,
		MeasuredAt:      stored.MeasuredAt.Unix(),
		DepthBenchmarks: depthBenchmarks,
	}, true
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func (r *Runner) logf(format string, args ...any) {
	r.d.Logf("profile: "+format, args...)
}

func (r *Runner) publishProgress(ctx context.Context, mode, phase string, extra map[string]any) {
	data := map[string]any{"mode": mode, "phase": phase}
	for k, v := range extra {
		data[k] = v
	}
	r.d.Publish.Publish(EventProgress, data)
}

func (r *Runner) publishFailed(ctx context.Context, mode, phase string, err error) {
	r.mu.Lock()
	r.lastErrMode = mode
	r.lastErrMsg = err.Error()
	r.lastErrAt = r.d.Now()
	r.mu.Unlock()

	r.d.Publish.Publish(EventFailed, map[string]any{
		"mode":  mode,
		"phase": phase,
		"error": err.Error(),
	})
	r.logf("FAILED at %s: %v", phase, err)
}

// LastError returns the most recent failure recorded for mode, if any. Used
// by handleProfileGet so a polling client sees the failure reason even if
// the profile:failed SSE event never reached it. Cleared by a subsequent
// successful run for the same mode (see Run's Phase 7).
func (r *Runner) LastError(mode string) (message string, at int64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastErrMode != mode || r.lastErrMsg == "" {
		return "", 0, false
	}
	return r.lastErrMsg, r.lastErrAt.Unix(), true
}

// probe triggers an immediate collector poll so the Dashboard/Console
// reflect slot state changes in real time during a profile run. The FE
// also invalidates qk.status on every profile:progress event, so the
// status refetch reads the fresh snapshot. No-op when Probe is nil (tests).
func (r *Runner) probe(ctx context.Context) {
	if r.d.Probe != nil {
		r.d.Probe(ctx)
	}
}

// runDepthSweep measures pp2048+tg128 throughput at each of
// benchmarkDepthFractions' KV-cache depths, and samples peak memory once
// the conversation reaches its deepest point.
//
// Design (product/QA sprint, 2026-07-29 — replaces the original single
// benchmark call, which had a real bug: it resent the SAME already-cached
// fill text with only a tiny appended suffix, so "prefill" measured the
// throughput of ~10-20 fresh tokens, not a meaningful figure). One
// conversation grows across all four checkpoints instead of resetting
// between them:
//   - at each depth, first fill the gap between the current position and
//     the target depth with heterogeneous filler (skipped at depth 0,
//     where there's nothing to fill);
//   - then append a FRESH prefillProbeTokens-token chunk — generated with
//     a depth-specific seed, so it is never a repeat of anything already
//     cached — and measure that call's prompt_per_second (real pp2048 at
//     this depth) and predicted_per_second for GenerationTokens more (real
//     tg128 at this depth);
//   - the probe's own tokens are absorbed into the running conversation
//     before moving to the next (deeper) checkpoint, so no work is
//     duplicated between depths.
//
// Peak memory is sampled right after the FILL step for the deepest depth
// (before its probe is appended) — the same "measure with a full KV cache"
// intent the original design had, just reached as the sweep's last fill
// rather than a dedicated one-off.
func (r *Runner) runDepthSweep(ctx context.Context, mode string, port, nCtx int) ([]DepthBenchmark, int64, error) {
	var conversation strings.Builder
	filled := 0
	var peakBytes int64
	benchmarks := make([]DepthBenchmark, 0, len(benchmarkDepthFractions))

	for i, frac := range benchmarkDepthFractions {
		intendedDepth := int(float64(nCtx) * frac)
		last := i == len(benchmarkDepthFractions)-1
		if last {
			// Leave room for the probe + generation + safety margin, same
			// reasoning the original design used for its one full-context fill.
			intendedDepth = nCtx - r.d.PrefillProbeTokens - r.d.GenerationTokens - 10
		}

		// filled tracks the TRUE cumulative position, not the intended
		// target — at small n_ctx (PrefillProbeTokens comparable to the
		// gap between depth fractions), a single probe can overshoot the
		// next depth's intended target entirely, in which case there's
		// nothing to fill (delta <= 0) and this loop must record the
		// benchmark at where the cache actually is, not the target it
		// already blew past. Recording the (wrong, smaller) intended
		// target here was a real bug caught by this file's own test
		// (TestRunDepthSweepPromptsNeverRepeat) before it shipped.
		if delta := intendedDepth - filled; delta > 0 {
			r.publishProgress(ctx, mode, "filling", map[string]any{"depth_target": intendedDepth})
			// Seed offset by depth index so each fill chunk is genuinely
			// different content, not the same corpus restarting at
			// paragraph 0 (see generateFillSeeded's doc comment).
			chunk, err := r.sizeFillSeeded(ctx, port, delta, int64(1000+i))
			if err != nil {
				return nil, 0, fmt.Errorf("depth %d: size fill: %w", intendedDepth, err)
			}
			conversation.WriteString(chunk)

			fillCtx, fillCancel := context.WithTimeout(ctx, r.completionTimeout(delta))
			stop, hungMsg := r.hangWatch(fillCtx, fillCancel, port)
			timings, err := r.completions(fillCtx, port, conversation.String(), 1)
			stop()
			fillCancel()
			if err != nil {
				if *hungMsg != "" {
					err = fmt.Errorf("inference hang detected mid-fill: %s", *hungMsg)
				}
				return nil, 0, fmt.Errorf("depth %d: fill completion: %w", intendedDepth, err)
			}
			filled += delta
			r.logf("profile: sweep fill to depth %d done (this call's fresh prompt_n=%d)", filled, timings.PromptN)
		}
		depthForRow := filled

		if last {
			r.publishProgress(ctx, mode, "measuring", nil)
			peak, err := r.samplePeakMemory(ctx)
			if err != nil {
				return nil, 0, fmt.Errorf("measuring at depth %d: %w", depthForRow, err)
			}
			peakBytes = peak
			r.publishProgress(ctx, mode, "measuring", map[string]any{"peak_bytes": peakBytes})
		}

		// Probe: append FRESH prefillProbeTokens tokens (distinct seed —
		// never a repeat of the fill content above or any prior probe) and
		// measure real pp2048 + tg128 at this depth.
		r.publishProgress(ctx, mode, "benchmarking", map[string]any{"depth_tokens": depthForRow})
		probeChunk, err := r.sizeFillSeeded(ctx, port, r.d.PrefillProbeTokens, int64(2000+i))
		if err != nil {
			return nil, 0, fmt.Errorf("depth %d: probe fill: %w", depthForRow, err)
		}
		probePrompt := conversation.String() + probeChunk

		benchCtx, benchCancel := context.WithTimeout(ctx, r.completionTimeout(r.d.PrefillProbeTokens+r.d.GenerationTokens))
		stop, hungMsg := r.hangWatch(benchCtx, benchCancel, port)
		timings, err := r.completions(benchCtx, port, probePrompt, r.d.GenerationTokens)
		stop()
		benchCancel()
		if err != nil {
			if *hungMsg != "" {
				err = fmt.Errorf("inference hang detected mid-benchmark: %s", *hungMsg)
			}
			return nil, 0, fmt.Errorf("depth %d: benchmark completion: %w", depthForRow, err)
		}
		b := DepthBenchmark{DepthTokens: depthForRow, PP2048TPS: timings.PromptPerSecond, TG128TPS: timings.PredictedPerSecond}
		benchmarks = append(benchmarks, b)
		r.publishProgress(ctx, mode, "benchmarking", map[string]any{
			"depth_tokens": depthForRow, "pp2048_tps": b.PP2048TPS, "tg128_tps": b.TG128TPS,
		})
		r.logf("profile: depth %d: pp2048 %.1f t/s, tg128 %.1f t/s", depthForRow, b.PP2048TPS, b.TG128TPS)

		// Absorb the probe into the running conversation so the next
		// (deeper) checkpoint continues from here rather than redoing this
		// work — the model's KV cache already holds it.
		conversation.WriteString(probeChunk)
		filled += r.d.PrefillProbeTokens
	}

	return benchmarks, peakBytes, nil
}

// samplePeakMemory probes MemoryBudget at SampleInterval cadence for
// SampleDuration, returning the peak UsedBytes. The additive footprint
// (gtt_used + unified_RSS) is what MemoryBudget returns (bytes since A1).
func (r *Runner) samplePeakMemory(ctx context.Context) (int64, error) {
	deadline := r.d.Now().Add(r.d.SampleDuration)
	var peak int64
	for {
		budget, err := r.d.Engine.MemoryBudget()
		if err != nil {
			return 0, fmt.Errorf("memory budget probe: %w", err)
		}
		if budget.UsedBytes > peak {
			peak = budget.UsedBytes
		}
		if r.d.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(r.d.SampleInterval):
		}
	}
	if peak == 0 {
		return 0, errors.New("memory budget returned 0 used (GPU probe failed?)")
	}
	return peak, nil
}

// completionTimeout returns a per-call deadline sized to the number of
// tokens the request will process (prefill and/or generation), using
// MinTokensPerSecond as a conservative floor throughput. Never shorter than
// 5 minutes, so small-context modes are unaffected; scales up for
// large-context modes instead of failing them against a flat cap.
func (r *Runner) completionTimeout(tokens int) time.Duration {
	const floor = 5 * time.Minute
	if tokens <= 0 {
		return floor
	}
	scaled := time.Duration(float64(tokens) / r.d.MinTokensPerSecond * float64(time.Second))
	if scaled < floor {
		return floor
	}
	return scaled
}

// hangWatch polls the collector's existing hang detector (Alerts on the
// current Snapshot) for a genuine INFERENCE_HANG on port while an in-flight
// completions call runs, and cancels the call the instant one is reported —
// a real "is this still making progress" signal (requests_processing>0 AND
// stalled TPS, sustained), not a blind wall-clock guess. Returns a stop func
// the caller must invoke (via defer, after the call returns) and a pointer
// that holds the detected alert message, if any, so the caller can produce
// a clearer error than a generic context-deadline message. No-op when
// Snapshots is nil (tests, or profiling wired without a live collector).
func (r *Runner) hangWatch(cctx context.Context, cancel context.CancelFunc, port int) (stop func(), hungMsg *string) {
	hungMsg = new(string)
	if r.d.Snapshots == nil {
		return func() {}, hungMsg
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(r.d.HangWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-cctx.Done():
				return
			case <-ticker.C:
				snap := r.d.Snapshots.Current()
				if snap == nil {
					continue
				}
				for _, a := range snap.Alerts {
					if a.Code == "INFERENCE_HANG" && a.Port == port {
						*hungMsg = a.Msg
						cancel()
						return
					}
				}
			}
		}
	}()
	return func() { close(done) }, hungMsg
}

// parseParallel extracts the --parallel value from ExtraArgs (default 1).
func parseParallel(args []string) int {
	for i, a := range args {
		if a == "--parallel" && i+1 < len(args) {
			if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
				return n
			}
		}
		if strings.HasPrefix(a, "--parallel=") {
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--parallel=")); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}

// resolveModelID maps a mode name to its registry model ID. Until the
// registry is wired, this returns the mode name itself (the registry's
// modeToModelID does the same first-wins reverse map; this is a placeholder
// that returns the mode name when no registry link is available).
func resolveModelID(cfg *config.Config, modeName string) string {
	mode, ok := cfg.Modes[modeName]
	if !ok || len(mode.Services) == 0 {
		return modeName
	}
	svc := mode.Services[0]
	if svc.Alias != "" {
		return svc.Alias
	}
	if svc.Model != "" {
		return svc.Model
	}
	return modeName
}

// Fingerprint computes the composite fingerprint for a mode's current config:
// sha256(model_path | file_size | quant_type | n_ctx | backend | llama_bin |
// llama_bin_mtime | sorted_extra_args).
// Cheap to recompute on every read (file stat + GGUF header, both cached by
// other subsystems). Changes on any invalidation condition (model replaced,
// re-quantized, context changed, backend swapped, binary updated, flags changed).
func (r *Runner) Fingerprint(modeName string) (string, error) {
	cfg := r.d.Cfg()
	mode, ok := cfg.Modes[modeName]
	if !ok || len(mode.Services) == 0 {
		return "", fmt.Errorf("fingerprint: mode %q not found", modeName)
	}
	svc := mode.Services[0]

	var parts []string

	// Model path + file size.
	modelPath := ""
	if svc.Model != "" {
		modelPath = cfg.Paths.ResolveModelPath(svc.Model)
		parts = append(parts, modelPath)
		if st, err := os.Stat(modelPath); err == nil {
			parts = append(parts, strconv.FormatInt(st.Size(), 10))
		} else {
			parts = append(parts, "stat_err")
		}
	}

	// Quant type from GGUF header.
	if modelPath != "" && strings.HasSuffix(modelPath, ".gguf") {
		if md, err := r.d.ReadMeta(modelPath); err == nil {
			parts = append(parts, md.QuantType)
		} else {
			parts = append(parts, "gguf_err")
		}
	}

	// Configured n_ctx + backend — these are profile axes (the footprint
	// and T/s depend on them). Including them means a profile measured at
	// a reduced n_ctx (from a silent GTT reduction) won't match when the
	// allocation issue is fixed and the full n_ctx loads.
	parts = append(parts, strconv.Itoa(svc.Context))
	parts = append(parts, svc.Backend)

	// llama.cpp binary path + mtime.
	llamaBin := svc.LlamaBin
	if llamaBin == "" {
		switch svc.Backend {
		case "rocm":
			llamaBin = cfg.Paths.RocmBin
		default:
			llamaBin = cfg.Paths.VulkanBin
		}
	}
	parts = append(parts, llamaBin)
	if st, err := os.Stat(llamaBin); err == nil {
		parts = append(parts, strconv.FormatInt(st.ModTime().Unix(), 10))
	} else {
		parts = append(parts, "stat_err")
	}

	// Sorted extra args (--parallel, --ctx-checkpoints, etc.).
	sorted := make([]string, len(svc.ExtraArgs))
	copy(sorted, svc.ExtraArgs)
	sort.Strings(sorted)
	parts = append(parts, strings.Join(sorted, ","))

	joined := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:]), nil
}
