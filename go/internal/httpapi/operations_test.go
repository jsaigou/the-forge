// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
)

// scriptedEngine is an Engine double whose Load and Unload results the test controls.
type scriptedEngine struct {
	engine.Stub
	loadFn   func(mode, slot string) engine.Result
	unloadFn func(slot string) engine.Result
}

func (e *scriptedEngine) Load(_ context.Context, mode, slot string) engine.Result {
	return e.loadFn(mode, slot)
}

func (e *scriptedEngine) Unload(_ context.Context, slot string) engine.Result {
	if e.unloadFn != nil {
		return e.unloadFn(slot)
	}
	return engine.Result{Success: true, Message: "unloaded"}
}

func opsSnapshot(a1Mode string) *collector.Snapshot {
	return &collector.Snapshot{Slots: map[string]collector.SlotState{
		"a1": {Slot: "a1", Unit: "forge-a1", Port: 8080, Label: "A1", Mode: a1Mode},
		"a2": {Slot: "a2", Unit: "forge-a2", Port: 8081, Label: "A2"},
	}}
}

type opBody struct {
	OperationID string        `json:"operation_id"`
	Success     bool          `json:"success"`
	InProgress  bool          `json:"in_progress"`
	Operation   operationJSON `json:"operation"`
}

func startLoad(t *testing.T, s *Server, mode, slot string) opBody {
	t.Helper()
	w := do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"`+mode+`","slot":"`+slot+`"}`)))
	if w.Code != 200 {
		t.Fatalf("POST /load = %d: %s", w.Code, w.Body.String())
	}
	var b opBody
	decodeJSON(t, w.Body, &b)
	if !b.Success || !b.InProgress || b.Operation.ID == "" || b.Operation.Kind != "load" || b.Operation.Slot != slot {
		t.Fatalf("POST /load body lacks a usable operation: %s", w.Body.String())
	}
	return b
}

func getOp(t *testing.T, s *Server, id string, wait int) operationJSON {
	t.Helper()
	path := "/api/v1/operations/" + id
	if wait > 0 {
		path += "?wait=" + itoa(int64(wait))
	}
	w := do(t, s, authedRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	var op operationJSON
	decodeJSON(t, w.Body, &op)
	return op
}

func slotState(t *testing.T, s *Server, slot string) slotOpStateJSON {
	t.Helper()
	w := do(t, s, authedRequest("GET", "/api/v1/status", nil))
	if w.Code != 200 {
		t.Fatalf("GET /status = %d", w.Code)
	}
	var resp struct {
		SlotStates map[string]slotOpStateJSON `json:"slot_states"`
	}
	decodeJSON(t, w.Body, &resp)
	st, ok := resp.SlotStates[slot]
	if !ok {
		t.Fatalf("/status has no slot_states[%q]: %v", slot, resp.SlotStates)
	}
	return st
}

// The incident this phase exists for: a load that crashes on startup used to answer "in progress" and then
// leave no trace a poller could read. It must now say failed, why, and keep saying it until the slot is touched.
func TestFailedLoadIsObservableThroughOperationAndStatus(t *testing.T) {
	eng := &scriptedEngine{loadFn: func(mode, slot string) engine.Result {
		return engine.Result{Success: false,
			Message: "forge-a1 did not reach running state: crashed during startup (SubState=auto-restart)",
			Detail:  "what():  entropy acceptance requires unconstrained greedy host sampling"}
	}}
	s := newFitTestServer(t, eng, opsSnapshot(""))

	b := startLoad(t, s, "qwen3", "a1")
	op := getOp(t, s, b.Operation.ID, 5)
	if op.State != "failed" {
		t.Fatalf("operation state = %q, want failed: %+v", op.State, op)
	}
	if !strings.Contains(op.Message, "crashed during startup") || !strings.Contains(op.Detail, "entropy acceptance") {
		t.Errorf("operation lost the reason: message=%q detail=%q", op.Message, op.Detail)
	}
	if op.FinishedAt == nil || op.NCtx != nil {
		t.Errorf("finished_at must be set and n_ctx absent on a failure: %+v", op)
	}

	st := slotState(t, s, "a1")
	if st.State != "failed" || st.OperationID != op.ID || !strings.Contains(st.Message, "crashed during startup") ||
		!strings.Contains(st.Detail, "entropy acceptance") || st.Mode == nil || *st.Mode != "qwen3" {
		t.Errorf("slot_states[a1] = %+v, want failed with the reason and the operation id", st)
	}
	if other := slotState(t, s, "a2"); other.State != "empty" {
		t.Errorf("an untouched slot is %q, want empty", other.State)
	}

	// Unloading the slot acknowledges the failure: the marker clears.
	w := do(t, s, authedRequest("POST", "/api/v1/unload", strings.NewReader(`{"slot":"a1"}`)))
	if w.Code != 200 {
		t.Fatalf("POST /unload = %d: %s", w.Code, w.Body.String())
	}
	var ub opBody
	decodeJSON(t, w.Body, &ub)
	if ub.Operation.Kind != "unload" || ub.Operation.ID == "" {
		t.Fatalf("unload response lacks an operation: %s", w.Body.String())
	}
	if got := getOp(t, s, ub.Operation.ID, 5); got.State != "succeeded" {
		t.Errorf("unload operation = %q, want succeeded", got.State)
	}
	if st := slotState(t, s, "a1"); st.State != "empty" {
		t.Errorf("after an unload slot_states[a1] = %+v, want empty", st)
	}
}

func TestSuccessfulLoadRecordsTheActualContext(t *testing.T) {
	eng := &scriptedEngine{loadFn: func(mode, slot string) engine.Result {
		return engine.Result{Success: true, Message: "Loaded qwen3 into a1", NCtx: 131072}
	}}
	s := newFitTestServer(t, eng, opsSnapshot("qwen3")) // the collector already sees it loaded

	b := startLoad(t, s, "qwen3", "a1")
	op := getOp(t, s, b.Operation.ID, 5)
	if op.State != "succeeded" || op.NCtx == nil || *op.NCtx != 131072 {
		t.Fatalf("operation = %+v, want succeeded with n_ctx 131072", op)
	}
	st := slotState(t, s, "a1")
	if st.State != "ready" || st.NCtx == nil || *st.NCtx != 131072 || st.OperationID != op.ID {
		t.Errorf("slot_states[a1] = %+v, want ready with the actual n_ctx and the operation id", st)
	}
}

// The collector lags a successful load by a few seconds; slot_states must not report "empty" in that gap.
func TestReadyWhileCollectorLagsThenEmptyAfterGrace(t *testing.T) {
	eng := &scriptedEngine{loadFn: func(mode, slot string) engine.Result {
		return engine.Result{Success: true, Message: "Loaded qwen3 into a1", NCtx: 40960}
	}}
	s := newFitTestServer(t, eng, opsSnapshot("")) // collector has not seen it yet
	b := startLoad(t, s, "qwen3", "a1")
	getOp(t, s, b.Operation.ID, 5)
	st := slotState(t, s, "a1")
	if st.State != "ready" || st.Mode == nil || *st.Mode != "qwen3" || st.NCtx == nil || *st.NCtx != 40960 || st.OperationID != b.Operation.ID {
		t.Fatalf("slot_states[a1] = %+v, want ready from the load record", st)
	}
	s.mu.Lock()
	rec := s.slotLastLoad["a1"]
	rec.at = time.Now().Add(-2 * readyGrace)
	s.slotLastLoad["a1"] = rec
	s.mu.Unlock()
	if st := slotState(t, s, "a1"); st.State != "empty" {
		t.Errorf("past the grace window with nothing loaded, slot_states[a1] = %+v, want empty", st)
	}
}

func TestOperationWaitBlocksUntilTheLoadFinishes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	eng := &scriptedEngine{loadFn: func(mode, slot string) engine.Result {
		<-release
		return engine.Result{Success: true, Message: "ok", NCtx: 4096}
	}}
	s := newFitTestServer(t, eng, opsSnapshot("qwen3"))

	b := startLoad(t, s, "qwen3", "a1")
	if st := slotState(t, s, "a1"); st.State != "loading" {
		t.Errorf("while the engine is still loading, slot_states[a1] = %q, want loading", st.State)
	}
	// A short wait on a load that is still running returns the running state after about the wait, not forever.
	start := time.Now()
	op := getOp(t, s, b.Operation.ID, 1)
	if elapsed := time.Since(start); op.State != "running" || elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("wait=1 on a running load returned %q after %v, want running after about 1s", op.State, elapsed)
	}
	// Releasing the engine wakes a long wait promptly.
	go func() { time.Sleep(200 * time.Millisecond); once.Do(func() { close(release) }) }()
	start = time.Now()
	op = getOp(t, s, b.Operation.ID, 30)
	if op.State != "succeeded" || time.Since(start) > 5*time.Second {
		t.Errorf("wait=30 returned %q after %v, want succeeded promptly after the release", op.State, time.Since(start))
	}
}

func TestOperationEndpointsReportErrorsAndList(t *testing.T) {
	eng := &scriptedEngine{loadFn: func(mode, slot string) engine.Result { return engine.Result{Success: true, Message: "ok"} }}
	s := newFitTestServer(t, eng, opsSnapshot(""))

	w := do(t, s, authedRequest("GET", "/api/v1/operations/op-nope-1", nil))
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"not_found"`) {
		t.Errorf("unknown operation = %d %s, want 404 not_found", w.Code, w.Body.String())
	}
	first := startLoad(t, s, "qwen3", "a1")
	getOp(t, s, first.Operation.ID, 5)
	for _, bad := range []string{"abc", "-1", "301", "1.5"} {
		w := do(t, s, authedRequest("GET", "/api/v1/operations/"+first.Operation.ID+"?wait="+bad, nil))
		if w.Code != 422 {
			t.Errorf("wait=%s = %d, want 422: %s", bad, w.Code, w.Body.String())
		}
	}
	second := startLoad(t, s, "qwen3", "a2")
	getOp(t, s, second.Operation.ID, 5)

	var list struct {
		Operations []operationJSON `json:"operations"`
	}
	w = do(t, s, authedRequest("GET", "/api/v1/operations", nil))
	decodeJSON(t, w.Body, &list)
	if len(list.Operations) != 2 || list.Operations[0].ID != second.Operation.ID || list.Operations[1].ID != first.Operation.ID {
		t.Errorf("list = %+v, want the two operations newest first", list.Operations)
	}
	w = do(t, s, authedRequest("GET", "/api/v1/operations?slot=a1", nil))
	list.Operations = nil
	decodeJSON(t, w.Body, &list)
	if len(list.Operations) != 1 || list.Operations[0].ID != first.Operation.ID {
		t.Errorf("?slot=a1 = %+v, want only the a1 operation", list.Operations)
	}
}

func TestOperationRegistryBoundsAndIdentity(t *testing.T) {
	r := newOperationRegistry()
	now := time.Now()
	seen := map[string]bool{}
	var running *operation
	for i := 0; i < operationRingSize+50; i++ {
		op := r.start("load", "a1", "m", now)
		if seen[op.id] {
			t.Fatalf("duplicate operation id %q", op.id)
		}
		seen[op.id] = true
		if i == 0 {
			running = op // never finished: must survive eviction
			continue
		}
		r.finish(op, true, "ok", "", 0, now)
	}
	if len(r.byID) != operationRingSize || len(r.order) != operationRingSize {
		t.Errorf("registry holds %d/%d operations, want %d", len(r.byID), len(r.order), operationRingSize)
	}
	if _, ok := r.get(running.id); !ok {
		t.Error("a still-running operation was evicted")
	}
	if !strings.HasPrefix(running.id, "op-"+r.nonce+"-") {
		t.Errorf("id %q does not carry the registry nonce", running.id)
	}

	// finish is idempotent and nil-safe: a second result cannot rewrite history.
	r.finish(running, false, "first", "", 0, now)
	r.finish(running, true, "second", "", 0, now)
	if v := r.view(running); v.State != "failed" || v.Message != "first" {
		t.Errorf("second finish rewrote the result: %+v", v)
	}
	r.finish(nil, true, "x", "", 0, now)
	if v := r.view(nil); v.ID != "" {
		t.Errorf("view(nil) = %+v", v)
	}
	// ids from a different daemon run can never alias this run's
	if other := newOperationRegistry(); other.nonce == "" {
		t.Error("empty nonce")
	}
}

func TestOperationJSONShape(t *testing.T) {
	r := newOperationRegistry()
	op := r.start("load", "a1", "qwen3", time.Unix(1700000000, 0))
	r.finish(op, false, "boom", "line1\nline2", 0, time.Unix(1700000005, 0))
	b, _ := json.Marshal(r.view(op))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"id", "kind", "slot", "config", "state", "message", "detail", "started_at", "finished_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("operation JSON lacks %q: %s", k, b)
		}
	}
	if _, ok := m["n_ctx"]; ok {
		t.Errorf("n_ctx must be omitted when unknown: %s", b)
	}
}
