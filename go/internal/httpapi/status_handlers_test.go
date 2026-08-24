// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
)

// TestStatusResponseSlotActivity (Sprint K, 2026-08-05): snap.Inference
// already carried RequestsProcessing per slot with zero readers outside the
// collector package before this — buildStatusResponse must surface it as
// slot_activity, keyed the same as Slots, so a fresh page load / SSE
// reconnect gets a correct picture rather than depending entirely on the
// low-latency slot:activity SSE event landing first.
func TestStatusResponseSlotActivity(t *testing.T) {
	s := newTestServer(t)
	snap := &collector.Snapshot{
		TakenAt:  time.Now(),
		Hostname: "test-host",
		Units:    map[string]collector.UnitState{},
		Slots: map[string]collector.SlotState{
			"a1": {Slot: "a1", Label: "A1", Mode: "qwen3"},
			"a2": {Slot: "a2", Label: "A2", Mode: "swallow"},
		},
		Inference: map[string]collector.SlotInference{
			"a1": {RequestsProcessing: 1},
			"a2": {RequestsProcessing: 0},
		},
		Ports:          map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}
	s.deps.Snapshots = collector.NewStatic(snap)

	resp := s.buildStatusResponse()

	if !resp.SlotActivity["a1"] {
		t.Errorf("a1 should be active: %+v", resp.SlotActivity)
	}
	if resp.SlotActivity["a2"] {
		t.Errorf("a2 should be idle: %+v", resp.SlotActivity)
	}
	if _, ok := resp.SlotActivity["a3"]; ok {
		t.Errorf("empty/unloaded slot must have no entry (not a false one): %+v", resp.SlotActivity)
	}
	// Profiling not wired in newTestServer's Deps — must stay nil, not a
	// zero-value {running:false} object (omitempty on the JSON tag depends
	// on this).
	if resp.Profiling != nil {
		t.Errorf("Profiling should be nil when Deps.Profiles is unwired: %+v", resp.Profiling)
	}
}

// TestInfraUnitOpPublishesFullStatus is a regression test for a real bug
// (Sprint K, 2026-08-05): runUnitOp used to Publish("status_update",
// map[string]any{"action": ..., "unit": ...}) — a bare two-field payload.
// sse.ts parses every status_update as a full Status and writes it straight
// into the query cache with no merge, so any infra-service start/stop
// (ServicesBar's play/stop buttons) blanked slots/services/slot_labels for
// every connected client until the next 15s poll. Fixed to reuse
// probeAndPush, the same "fresh snapshot, then publish the real full
// status" helper profile_handlers.go already uses. This test opens a real
// SSE connection (like TestSSEBusEventsDeliverOverRealWire) rather than
// subscribing directly to the bus, so it actually exercises the wire
// encoding a browser would see.
func TestInfraUnitOpPublishesFullStatus(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE GET: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	// Drain the initial status_update event.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading initial event: %v", err)
		}
		if line == "\n" {
			break
		}
	}

	ttsReq, _ := http.NewRequest("POST", srv.URL+"/api/v1/tts/start", nil)
	ttsReq.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	tr, err := http.DefaultClient.Do(ttsReq)
	if err != nil {
		t.Fatalf("tts start request: %v", err)
	}
	tr.Body.Close()
	if tr.StatusCode != http.StatusOK {
		t.Fatalf("tts start status = %d, want 200", tr.StatusCode)
	}

	nameLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status_update event line: %v", err)
	}
	if nameLine != "event: status_update\n" {
		t.Fatalf("event after tts start = %q, want \"event: status_update\\n\"", nameLine)
	}
	dataLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status_update data line: %v", err)
	}
	payload := strings.TrimPrefix(strings.TrimSuffix(dataLine, "\n"), "data: ")
	var status statusResponse
	if err := json.Unmarshal([]byte(payload), &status); err != nil {
		t.Fatalf("status_update payload not a full Status: %v (raw: %s)", err, payload)
	}
	if status.Slots == nil {
		t.Errorf("status_update payload after infra unit op has no slots — the bug this regression-tests for: %s", payload)
	}
}
