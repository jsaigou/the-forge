// SPDX-License-Identifier: Apache-2.0

package httpapi

// fit_check_test.go — BE-2 (F3, docs/v5-review-fixes.md): the direct
// POST /api/v1/load path (handleLoad) previously called Engine.Load with no
// memory check at all — unlike the scheduler's EnsureLoaded (a0/MCP), which
// at least went through engine.FitPlan. These tests cover the guard added
// in scheduler_handlers.go: reject with a clear reason when CanFit says the
// load won't fit, but don't reject a same-slot reload just because the
// target slot's own (about-to-be-freed) occupant is still counted as used.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
)

// canFitStubEngine is a controllable Engine double: CanFit returns a fixed
// answer, Load always "succeeds" (optionally with a caller-set NCtx so the
// ctx-reduction annotation can be exercised too).
type canFitStubEngine struct {
	engine.Stub
	fit       engine.CanFit
	fitErr    error
	loadNCtx  int
	footprint map[string]int64 // non-nil → also implements SlotFootprintBytes
}

func (e *canFitStubEngine) CanFit(string) (engine.CanFit, error) { return e.fit, e.fitErr }

func (e *canFitStubEngine) Load(_ context.Context, mode, slot string) engine.Result {
	return engine.Result{Success: true, Message: "loaded", NCtx: e.loadNCtx}
}

// footprintEngine adds the optional SlotFootprintBytes seam (mirrors
// sched.Footprints) on top of canFitStubEngine.
type footprintEngine struct{ canFitStubEngine }

func (e *footprintEngine) SlotFootprintBytes(slot string) int64 {
	if e.footprint == nil {
		return 0
	}
	return e.footprint[slot]
}

func newFitTestServer(t *testing.T, eng engine.Engine, snap *collector.Snapshot) *Server {
	t.Helper()
	events := bus.New()
	cfg, err := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1":   {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
			"a2": {Unit: "forge-a2", Port: 8081, Label: "A2", Order: 2},
		},
		Modes: map[string]config.Mode{
			"qwen3": {Label: "Qwen3", Services: []config.Service{{Model: "qwen3.gguf", Alias: "qwen3", Context: 131072}}},
		},
	})
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}
	s := New(Deps{
		Snapshots: collector.NewStatic(snap),
		Engine:    eng,
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })
	return s
}

// TestHandleLoadRejectsWontFit: CanFit says no → 409 wont_fit with the
// reason surfaced, and Load is never called (no silent over-subscription,
// no retry).
func TestHandleLoadRejectsWontFit(t *testing.T) {
	eng := &canFitStubEngine{fit: engine.CanFit{Fits: false, RequiredBytes: 90000 * 1024 * 1024, FreeBytes: 1000 * 1024 * 1024, Reason: "Won't fit even after evicting every loaded slot (1000 MB < 90000 MB needed)"}}
	s := newFitTestServer(t, eng, &collector.Snapshot{
		Slots: map[string]collector.SlotState{
			"a1":   {Slot: "a1", Unit: "forge-a1", Port: 8080, Label: "A1"},
			"a2": {Slot: "a2", Unit: "forge-a2", Port: 8081, Label: "A2"},
		},
	})

	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	if resp["error"] != "wont_fit" {
		t.Errorf("error = %v, want wont_fit", resp["error"])
	}
	if resp["message"] == "" || resp["message"] == nil {
		t.Error("message must carry the fit-check reason")
	}
	if resp["required_bytes"] == nil || resp["free_bytes"] == nil {
		t.Errorf("required_bytes/free_bytes must be present: %v", resp)
	}
}

// TestHandleLoadFitErrorRefusesRatherThanProceeding: a budget-probe error
// must not fall through to an unchecked load either — fail closed on the
// probe error too, not just on a determinate "won't fit" answer.
func TestHandleLoadFitErrorRefusesRatherThanProceeding(t *testing.T) {
	eng := &canFitStubEngine{fitErr: context.DeadlineExceeded}
	s := newFitTestServer(t, eng, &collector.Snapshot{
		Slots: map[string]collector.SlotState{
			"a1": {Slot: "a1", Unit: "forge-a1", Port: 8080, Label: "A1"},
		},
	})
	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", w.Code)
	}
}

// TestHandleLoadAllowsSameSlotReloadDespiteItsOwnFootprint: CanFit reports
// !Fits using the pre-load snapshot (which still counts the target slot's
// current occupant as "used"), but the target slot is occupied and freeing
// it (which Engine.Load does unconditionally before starting the new
// service) covers the shortfall — this must be ALLOWED, not rejected, or
// every routine same-slot reload would 409 forever.
func TestHandleLoadAllowsSameSlotReloadDespiteItsOwnFootprint(t *testing.T) {
	eng := &footprintEngine{
		canFitStubEngine: canFitStubEngine{
			fit: engine.CanFit{Fits: false, RequiredBytes: 5000 * 1024 * 1024, FreeBytes: 4000 * 1024 * 1024, Reason: "won't fit"},
		},
	}
	eng.footprint = map[string]int64{"a1": 2000 * 1024 * 1024} // freeing a1 covers the 1000 MiB shortfall
	s := newFitTestServer(t, eng, &collector.Snapshot{
		Slots: map[string]collector.SlotState{
			"a1": {Slot: "a1", Mode: "qwen3", Unit: "forge-a1", Port: 8080, Label: "A1"},
		},
	})

	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (target slot's own footprint should cover the shortfall): body=%s", w.Code, w.Body.String())
	}
}

// TestHandleLoadStillRejectsWhenTargetFootprintInsufficient: same setup, but
// freeing the target slot still isn't enough — must still 409.
func TestHandleLoadStillRejectsWhenTargetFootprintInsufficient(t *testing.T) {
	eng := &footprintEngine{
		canFitStubEngine: canFitStubEngine{
			fit: engine.CanFit{Fits: false, RequiredBytes: 50000 * 1024 * 1024, FreeBytes: 4000 * 1024 * 1024, Reason: "won't fit"},
		},
	}
	eng.footprint = map[string]int64{"a1": 2000 * 1024 * 1024} // nowhere near enough
	s := newFitTestServer(t, eng, &collector.Snapshot{
		Slots: map[string]collector.SlotState{
			"a1": {Slot: "a1", Mode: "qwen3", Unit: "forge-a1", Port: 8080, Label: "A1"},
		},
	})

	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
}

// TestHandleLoadAnnotatesCtxReduction: BE-2 Q4 — a load that succeeds but
// comes back under 95% of the mode's configured context must carry a
// visible warning in the load_complete SSE payload, not a silent success.
func TestHandleLoadAnnotatesCtxReduction(t *testing.T) {
	eng := &canFitStubEngine{
		fit:      engine.CanFit{Fits: true, RequiredBytes: 100 * 1024 * 1024, FreeBytes: 100000 * 1024 * 1024},
		loadNCtx: 65536, // configured is 131072 — well under 95%
	}
	s := newFitTestServer(t, eng, &collector.Snapshot{
		Slots: map[string]collector.SlotState{
			"a1": {Slot: "a1", Unit: "forge-a1", Port: 8080, Label: "A1"},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := s.deps.Events.Subscribe(ctx)

	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Name != "load_complete" {
				continue
			}
			data, ok := ev.Data.(map[string]any)
			if !ok {
				t.Fatalf("load_complete data = %T, want map[string]any", ev.Data)
			}
			result, ok := data["result"].(lifecycleResult)
			if !ok {
				t.Fatalf("result = %T, want lifecycleResult", data["result"])
			}
			if !strings.Contains(result.Message, "context reduced") {
				t.Errorf("message = %q, want it to mention the context reduction", result.Message)
			}
			if !result.Success {
				t.Error("Success must stay true — a reduced context is a warning, not a hard block (Q4)")
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for load_complete")
		}
	}
}

// unloadRecordingEngine records Unload calls (in order) on top of
// footprintEngine.
type unloadRecordingEngine struct {
	footprintEngine
	mu       sync.Mutex
	unloaded []string
	loaded   chan string
}

func (e *unloadRecordingEngine) Unload(_ context.Context, slot string) engine.Result {
	e.mu.Lock()
	e.unloaded = append(e.unloaded, slot)
	e.mu.Unlock()
	return engine.Result{Success: true}
}

func (e *unloadRecordingEngine) Load(_ context.Context, mode, slot string) engine.Result {
	e.loaded <- slot
	return engine.Result{Success: true, Message: "loaded"}
}

func evictTestSnapshot() *collector.Snapshot {
	now := time.Now()
	return &collector.Snapshot{
		TakenAt: now,
		Slots: map[string]collector.SlotState{
			"a1": {Slot: "a1", Unit: "forge-a1", Port: 8080, Label: "A1"},
			"a2": {Slot: "a2", Mode: "busy", Unit: "forge-a2", Port: 8081, Label: "A2", LastActivity: now.Add(-1 * time.Minute)},
			"a3": {Slot: "a3", Mode: "stale", Unit: "forge-a3", Port: 8082, Label: "A3", LastActivity: now.Add(-3 * time.Hour)},
		},
	}
}

// TestHandleLoadWontFitOffersLongestIdleFirst: a refused load names the
// other loaded slots longest-idle first, and suggests the shortest prefix
// that covers the shortfall.
func TestHandleLoadWontFitOffersLongestIdleFirst(t *testing.T) {
	eng := &unloadRecordingEngine{loaded: make(chan string, 1)}
	eng.fit = engine.CanFit{Fits: false, RequiredBytes: 50 << 30, FreeBytes: 20 << 30, Reason: "won't fit"}
	eng.footprint = map[string]int64{"a2": 40 << 30, "a3": 35 << 30}
	s := newFitTestServer(t, eng, evictTestSnapshot())

	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
	var resp struct {
		Candidates []evictCandidate `json:"evict_candidates"`
		Suggested  []string         `json:"suggested_evict"`
	}
	decodeJSON(t, w.Body, &resp)
	if len(resp.Candidates) != 2 || resp.Candidates[0].Slot != "a3" || resp.Candidates[1].Slot != "a2" {
		t.Fatalf("candidates = %+v, want a3 (idle 3h) before a2 (idle 1m)", resp.Candidates)
	}
	// a3 alone frees 35 GiB ≥ the 30 GiB shortfall.
	if len(resp.Suggested) != 1 || resp.Suggested[0] != "a3" {
		t.Errorf("suggested = %v, want [a3]", resp.Suggested)
	}
}

// TestHandleLoadWithConfirmedEvictUnloadsThenLoads: the confirmed evict
// list is honored — fit is re-checked counting those slots as freed, they
// are unloaded, and only then does the load run.
func TestHandleLoadWithConfirmedEvictUnloadsThenLoads(t *testing.T) {
	eng := &unloadRecordingEngine{loaded: make(chan string, 1)}
	eng.fit = engine.CanFit{Fits: false, RequiredBytes: 50 << 30, FreeBytes: 20 << 30, Reason: "won't fit"}
	eng.footprint = map[string]int64{"a2": 40 << 30, "a3": 35 << 30}
	s := newFitTestServer(t, eng, evictTestSnapshot())

	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1","evict":["a3"]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	select {
	case <-eng.loaded:
	case <-time.After(2 * time.Second):
		t.Fatal("load never ran")
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.unloaded) != 1 || eng.unloaded[0] != "a3" {
		t.Errorf("unloaded = %v, want [a3]", eng.unloaded)
	}
}

// TestHandleLoadConfirmedEvictStillTooSmallRefuses: a confirmed list that
// doesn't actually free enough is a plain refusal — nothing is unloaded.
func TestHandleLoadConfirmedEvictStillTooSmallRefuses(t *testing.T) {
	eng := &unloadRecordingEngine{loaded: make(chan string, 1)}
	eng.fit = engine.CanFit{Fits: false, RequiredBytes: 90 << 30, FreeBytes: 20 << 30, Reason: "won't fit"}
	eng.footprint = map[string]int64{"a2": 40 << 30, "a3": 35 << 30}
	s := newFitTestServer(t, eng, evictTestSnapshot())

	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1","evict":["a3"]}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.unloaded) != 0 {
		t.Errorf("unloaded = %v, want none", eng.unloaded)
	}
}
