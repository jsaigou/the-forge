// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexwire"
	"golang.org/x/net/websocket"
)

// TestTwoLaneSoak is the M1 acceptance soak. It is opt-in because it runs for
// the requested wall-clock duration:
//
//	DUP_SOAK_SECONDS=300 go test ./cmd/forge-duplex -run TestTwoLaneSoak -timeout 6m
//
// Two loopback sessions send one 3200-sample frame per 133 ms tick; the one-
// frame delay means each tick reads the previous tick's echo. It checks the
// payload, sequence, final flush, clean close and metrics retirement.
func readSoakAudio(t *testing.T, conn *websocket.Conn) (uint32, []byte) {
	t.Helper()
	for {
		m := readWire(t, conn)
		if m.Type == websocket.BinaryFrame {
			seq, pcm, err := duplexwire.DecodeAudioFrame(m.Data)
			if err != nil {
				t.Fatalf("decode soak audio: %v", err)
			}
			return seq, pcm
		}
		if m.Type == websocket.TextFrame {
			var v map[string]any
			if err := json.Unmarshal(m.Data, &v); err == nil && v["type"] == "stats" {
				continue // the 10 s stats ticker interleaves with audio
			}
		}
		t.Fatalf("unexpected soak message: %s", m.Data)
	}
}

func TestTwoLaneSoak(t *testing.T) {
	raw := os.Getenv("DUP_SOAK_SECONDS")
	if raw == "" {
		t.Skip("DUP_SOAK_SECONDS not set")
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		t.Fatalf("DUP_SOAK_SECONDS=%q is not a positive number", raw)
	}

	env := newTestEnv(t, Options{NewDriver: func() duplex.DuplexDriver {
		return duplex.NewDriver(duplex.NewLoopbackTenant(), nil)
	}})
	lanes := []*websocket.Conn{
		env.mustDial(t, []string{dupVersionProtocol}),
		env.mustDial(t, []string{dupVersionProtocol}),
	}
	for _, c := range lanes {
		sendStart(t, c, "")
		expectReady(t, c)
	}

	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	ticker := time.NewTicker(133 * time.Millisecond)
	defer ticker.Stop()
	var sent [2]uint32
	var previous [2][]byte
	for time.Now().Before(deadline) {
		<-ticker.C
		for lane, c := range lanes {
			frame := sendAudio(t, c, sent[lane], byte(0x20+lane*0x40+int(sent[lane]&0xff)))
			if sent[lane] > 0 {
				seq, pcm := readSoakAudio(t, c)
				if seq != sent[lane]-1 || !bytes.Equal(pcm, previous[lane]) {
					t.Fatalf("lane %d seq %d: echo mismatch (seq=%d)", lane, sent[lane]-1, seq)
				}
			}
			previous[lane] = frame
			sent[lane]++
		}
	}

	for lane, c := range lanes {
		if err := websocket.Message.Send(c, `{"type":"session.close","reason":"soak_done"}`); err != nil {
			t.Fatalf("lane %d close: %v", lane, err)
		}
		seq, pcm := readSoakAudio(t, c)
		if seq != sent[lane]-1 || !bytes.Equal(pcm, previous[lane]) {
			t.Fatalf("lane %d final flush mismatch (seq=%d)", lane, seq)
		}
		expectStatsThenDone(t, c, "soak_done")
	}

	// The one-frame delay means the last sent frame was flushed above.
	if got := metricsText(t, env); !strings.Contains(got, "duplex_sessions_active 0") {
		t.Fatalf("metrics after soak:\n%s", got)
	}
	if got := metricsText(t, env); !strings.Contains(got, "duplex_sessions_total 2") {
		t.Fatalf("metrics totals after soak:\n%s", got)
	}
}
