// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSSEBusEventsDeliverOverRealWire is an end-to-end check distinct from
// TestSSEEmitsUnderscoreEventNames: that test Subscribes directly to the bus,
// bypassing handleSSE's HTTP write loop entirely, so it could never have
// caught a bug in how handleSSE serializes bus events onto the wire. This
// test opens a real net/http streaming GET to /api/v1/events — exactly like
// a browser's EventSource — and asserts the bytes it reads are a
// spec-correct "event: <name>\n" frame.
//
// Regression test for: handleSSE's bus-event branch called
// fmt.Fprint(w, payload) where payload is a []byte from encodeSSEEvent.
// fmt.Fprint formats a []byte with its default %v verb ("[101 118 ...]",
// decimal byte values) instead of writing it raw, so every bus-routed event
// (load_*, switch_*, profile:*, unload_complete, config_updated) was sent
// as unparseable garbage — no SSE client could ever see them. The
// initial-connect and per-connection-heartbeat status_update writes used a
// string payload (encodeSSEStatus), so they were unaffected and masked the
// bug in ad hoc testing. Fixed by using w.Write(payload) directly.
func TestSSEBusEventsDeliverOverRealWire(t *testing.T) {
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

	// Fire a real load request, synchronously, so load_started has
	// definitely been published by the time we read the next SSE frame.
	loadReq, _ := http.NewRequest("POST", srv.URL+"/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1"}`))
	loadReq.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	loadReq.Header.Set("Content-Type", "application/json")
	lr, err := http.DefaultClient.Do(loadReq)
	if err != nil {
		t.Fatalf("load request: %v", err)
	}
	lr.Body.Close()
	if lr.StatusCode != http.StatusOK {
		t.Fatalf("load request status = %d, want 200", lr.StatusCode)
	}

	nameLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading load_started event line: %v", err)
	}
	if nameLine != "event: load_started\n" {
		t.Fatalf("first line after load = %q, want \"event: load_started\\n\" (bus event not delivered as valid SSE)", nameLine)
	}
	dataLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading load_started data line: %v", err)
	}
	if !strings.HasPrefix(dataLine, "data: {") {
		t.Errorf("data line = %q, want a JSON data: line", dataLine)
	}
}
