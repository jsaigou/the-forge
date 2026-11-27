// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexremote"
	"golang.org/x/net/websocket"
)

// TestRemoteEngineThroughTransportAndDriver runs the whole M1 path with the
// model in a separate "process": browser-side WebSocket, the transport, the
// real driver, the remote tenant and a mock engine (engine-contract.md). The
// one-frame delay engine also proves the Flusher path works over the wire.
func TestRemoteEngineThroughTransportAndDriver(t *testing.T) {
	engine := duplexremote.NewMockEngine(duplexremote.MockEngineOptions{DelayFrames: 1})
	srv := httptest.NewServer(engine.Handler())
	t.Cleanup(srv.Close)
	tenant := duplexremote.New(duplexremote.Config{BaseURL: srv.URL, Tenant: "mock", StepTimeout: time.Second})

	env := newTestEnv(t, Options{
		DefaultTenant: "mock",
		NewDriver:     func() duplex.DuplexDriver { return duplex.NewDriver(tenant, nil) },
	})
	conn := env.mustDial(t, []string{dupVersionProtocol})
	sendStart(t, conn, "")
	expectReady(t, conn)

	var want [][]byte
	for i := uint32(0); i < 3; i++ {
		want = append(want, sendAudio(t, conn, i, byte(i*5+1)))
	}
	for i := uint32(0); i < 2; i++ {
		seq, pcm := readAudio(t, conn)
		if seq != i || !bytes.Equal(pcm, want[i]) {
			t.Fatalf("echo %d: seq=%d equal=%v", i, seq, bytes.Equal(pcm, want[i]))
		}
	}
	if err := websocket.Message.Send(conn, `{"type":"session.close","reason":"client_done"}`); err != nil {
		t.Fatalf("close: %v", err)
	}
	seq, pcm := readAudio(t, conn)
	if seq != 2 || !bytes.Equal(pcm, want[2]) {
		t.Fatalf("flushed frame: seq=%d equal=%v", seq, bytes.Equal(pcm, want[2]))
	}
	expectStatsThenDone(t, conn, "client_done")

	deadline := time.Now().Add(2 * time.Second)
	for engine.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if engine.Active() != 0 {
		t.Fatalf("engine still holds %d lanes after the session ended", engine.Active())
	}
}
