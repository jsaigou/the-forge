// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDaemon answers POST load/unload with the given body and serves GET /api/v1/operations/{id} from op, which a
// test can change; it counts operation polls and records the last wait= it was asked for.
type fakeDaemon struct {
	srv      *httptest.Server
	start    string // JSON body for POST /load and /unload
	op       atomic.Value
	polls    atomic.Int32
	lastWait atomic.Value
}

func newFakeDaemon(t *testing.T, start string, op Operation) *fakeDaemon {
	t.Helper()
	t.Setenv("FORGE_LANG", "en")
	d := &fakeDaemon{start: start}
	d.op.Store(op)
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(d.start))
	}
	mux.HandleFunc("POST /api/v1/load", reply)
	mux.HandleFunc("POST /api/v1/unload", reply)
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"slots":{"a1":null,"a2":null,"a3":null,"a4":null}}`))
	})
	mux.HandleFunc("GET /api/v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.polls.Add(1)
		d.lastWait.Store(r.URL.Query().Get("wait"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d.op.Load().(Operation))
	})
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) client() *Client {
	return &Client{BaseURL: d.srv.URL, Key: "sk-forge-test", HTTP: d.srv.Client()}
}

const startedWithOp = `{"success":true,"in_progress":true,"message":"Loading m into a1","operation":{"id":"op-x-1","kind":"load","slot":"a1","state":"running"}}`

func TestLoadWaitsAndReportsTheActualContext(t *testing.T) {
	d := newFakeDaemon(t, startedWithOp, Operation{ID: "op-x-1", State: "succeeded", NCtx: 40960})
	var out, errOut bytes.Buffer
	if err := loadWith(d.client(), "carbon-x", "a1", true, &out, &errOut); err != nil {
		t.Fatalf("loadWith: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "loaded carbon-x on a1 (n_ctx=40960)") || strings.Contains(got, "n_ctx=0") {
		t.Errorf("output = %q, want the actual context and never n_ctx=0", got)
	}
	if d.polls.Load() != 1 || d.lastWait.Load() != "20" {
		t.Errorf("polls=%d wait=%v, want one long-poll with wait=20", d.polls.Load(), d.lastWait.Load())
	}
}

func TestLoadFailureShowsReasonAndLogTailAndFails(t *testing.T) {
	d := newFakeDaemon(t, startedWithOp, Operation{ID: "op-x-1", State: "failed",
		Message: "forge-a1 did not reach running state: crashed during startup (SubState=auto-restart)",
		Detail:  "terminate called after throwing an instance of 'std::invalid_argument'\n  what():  entropy acceptance requires"})
	var out, errOut bytes.Buffer
	err := loadWith(d.client(), "clover", "a1", true, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "load failed: forge-a1 did not reach running state: crashed during startup") {
		t.Fatalf("err = %v, want a failure carrying the reason", err)
	}
	if out.Len() != 0 {
		t.Errorf("a failed load must not print a success line: %q", out.String())
	}
	e := errOut.String()
	if !strings.Contains(e, "last log lines from the unit:") || !strings.Contains(e, "  what():  entropy acceptance requires") {
		t.Errorf("stderr lacks the log tail: %q", e)
	}
}

func TestLoadNoWaitStartsAndReturnsWithoutPolling(t *testing.T) {
	d := newFakeDaemon(t, startedWithOp, Operation{State: "running"})
	// If a regression made --no-wait poll, the fake never finishes: a short limit makes that fail fast instead of
	// waiting out the real 20 minutes.
	old := opWaitLimit
	opWaitLimit = 50 * time.Millisecond
	t.Cleanup(func() { opWaitLimit = old })
	var out, errOut bytes.Buffer
	if err := loadWith(d.client(), "m", "a1", false, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if d.polls.Load() != 0 || !strings.Contains(out.String(), "loading m on a1 (operation op-x-1)") {
		t.Errorf("polls=%d out=%q, want no polling and the operation id", d.polls.Load(), out.String())
	}
}

func TestLoadAgainstADaemonWithoutOperationsDoesNotClaimSuccess(t *testing.T) {
	d := newFakeDaemon(t, `{"success":true,"in_progress":true,"message":"Loading m into a1"}`, Operation{})
	var out, errOut bytes.Buffer
	if err := loadWith(d.client(), "m", "", true, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if d.polls.Load() != 0 || strings.Contains(out.String(), "loaded") || strings.Contains(out.String(), "n_ctx") {
		t.Errorf("polls=%d out=%q: an older daemon gives nothing to follow, so say only that it started", d.polls.Load(), out.String())
	}
	if !strings.Contains(out.String(), "loading m on a1") {
		t.Errorf("out = %q", out.String())
	}
}

func TestLoadGivesUpWithAPointerAfterTheLimit(t *testing.T) {
	d := newFakeDaemon(t, startedWithOp, Operation{ID: "op-x-1", State: "running"})
	old := opWaitLimit
	opWaitLimit = time.Millisecond
	t.Cleanup(func() { opWaitLimit = old })
	var out, errOut bytes.Buffer
	err := loadWith(d.client(), "m", "a1", true, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "still running after") || !strings.Contains(err.Error(), "forge status") {
		t.Errorf("err = %v, want a timeout that points at forge status", err)
	}
}

func TestLoadSucceededWithUnknownContextSaysSoHonestly(t *testing.T) {
	d := newFakeDaemon(t, startedWithOp, Operation{ID: "op-x-1", State: "succeeded"})
	var out, errOut bytes.Buffer
	if err := loadWith(d.client(), "vllm-thing", "a2", true, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "loaded vllm-thing on a2" {
		t.Errorf("out = %q, want a line without a made-up n_ctx", got)
	}
}

func TestLoadRefusedByTheDaemonIsReportedAsBefore(t *testing.T) {
	d := newFakeDaemon(t, `{"success":false,"message":"Unknown mode: nope"}`, Operation{})
	var out, errOut bytes.Buffer
	err := loadWith(d.client(), "nope", "a1", true, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "load failed: Unknown mode: nope") {
		t.Errorf("err = %v", err)
	}
}

func TestUnloadFollowsTheOperation(t *testing.T) {
	start := `{"success":true,"in_progress":true,"message":"Unloading a1 slot","operation":{"id":"op-x-2","kind":"unload","slot":"a1","state":"running"}}`
	d := newFakeDaemon(t, start, Operation{ID: "op-x-2", State: "succeeded"})
	var out bytes.Buffer
	if err := unloadWith(d.client(), "a1", true, &out); err != nil || strings.TrimSpace(out.String()) != "unloaded a1" {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
	d.op.Store(Operation{ID: "op-x-2", State: "failed", Message: "slot a1 is still deactivating"})
	out.Reset()
	if err := unloadWith(d.client(), "a1", true, &out); err == nil || !strings.Contains(err.Error(), "unload failed: slot a1 is still deactivating") {
		t.Errorf("a failed unload must fail the command: err=%v", err)
	}
	polls := d.polls.Load()
	out.Reset()
	if err := unloadWith(d.client(), "a1", false, &out); err != nil || d.polls.Load() != polls || !strings.Contains(out.String(), "unloading a1 (operation op-x-2)") {
		t.Errorf("--no-wait must not poll: err=%v out=%q", err, out.String())
	}
}

func TestUnloadAllHasNothingToFollow(t *testing.T) {
	d := newFakeDaemon(t, `{"success":true,"in_progress":true,"message":"Unloading all slots"}`, Operation{})
	var out bytes.Buffer
	if err := unloadWith(d.client(), "all", true, &out); err != nil || d.polls.Load() != 0 {
		t.Fatalf("err=%v polls=%d", err, d.polls.Load())
	}
	if strings.Contains(out.String(), "unloaded all") {
		t.Errorf("unload-all is async and untracked; it must not claim it finished: %q", out.String())
	}
}
