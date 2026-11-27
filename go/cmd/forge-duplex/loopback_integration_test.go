// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	"github.com/jsaigou/the-forge/internal/duplex"
	"golang.org/x/net/websocket"
)

// TestLoopbackDriverEchoAndDrain is the M1 integration check: the WS2
// transport, the WS1 driver and the WS4 loopback tenant, with the one-frame
// delay that protocol.md §7.3 documents. The third frame is only recoverable
// through the driver's Flusher hook while draining, so this also tests that
// the last frame is not silently lost.
func TestLoopbackDriverEchoAndDrain(t *testing.T) {
	env := newTestEnv(t, Options{NewDriver: func() duplex.DuplexDriver {
		return duplex.NewDriver(duplex.NewLoopbackTenant(), nil)
	}})
	conn := env.mustDial(t, []string{dupVersionProtocol})

	sendStart(t, conn, "")
	expectReady(t, conn)

	var want [][]byte
	for i := uint32(0); i < 3; i++ {
		want = append(want, sendAudio(t, conn, i, byte(i*7)))
	}

	// One-frame delay: the first two echoes arrive while the session runs.
	for i := uint32(0); i < 2; i++ {
		seq, pcm := readAudio(t, conn)
		if seq != i {
			t.Fatalf("echo seq = %d, want %d", seq, i)
		}
		if !bytes.Equal(pcm, want[i]) {
			t.Fatalf("echo %d payload differs", i)
		}
	}

	if err := websocket.Message.Send(conn, `{"type":"session.close","reason":"client_done"}`); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The third frame is flushed while draining, before stats/session.done.
	seq, pcm := readAudio(t, conn)
	if seq != 2 {
		t.Fatalf("flushed seq = %d, want 2", seq)
	}
	if !bytes.Equal(pcm, want[2]) {
		t.Fatalf("flushed payload differs")
	}

	done := expectStatsThenDone(t, conn, "client_done")
	st := done["stats"].(map[string]any)
	if st["in_frames"].(float64) != 3 || st["out_frames"].(float64) != 3 {
		t.Fatalf("final stats = %v", st)
	}
}
