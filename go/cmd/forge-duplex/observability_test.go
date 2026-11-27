// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"golang.org/x/net/websocket"
)

func metricsText(t *testing.T, env *testEnv) string {
	t.Helper()
	resp, err := http.Get(env.ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("metrics body: %v", err)
	}
	return string(body)
}

// TestTwoLaneLoopbackAndMetrics proves two concurrent sessions through the
// real transport, driver and loopback tenant, and that WS5 retires both into
// the process metrics when they close.
func TestTwoLaneLoopbackAndMetrics(t *testing.T) {
	env := newTestEnv(t, Options{NewDriver: func() duplex.DuplexDriver {
		return duplex.NewDriver(duplex.NewLoopbackTenant(), nil)
	}})

	if got := metricsText(t, env); !strings.Contains(got, "duplex_sessions_active 0") {
		t.Fatalf("initial metrics = %s", got)
	}

	type lane struct {
		conn *websocket.Conn
		want [][]byte
	}
	lanes := []*lane{
		{conn: env.mustDial(t, []string{dupVersionProtocol})},
		{conn: env.mustDial(t, []string{dupVersionProtocol})},
	}
	for i, l := range lanes {
		sendStart(t, l.conn, "")
		expectReady(t, l.conn)
		for seq := uint32(0); seq < 3; seq++ {
			l.want = append(l.want, sendAudio(t, l.conn, seq, byte(0x10+i*0x40+int(seq))))
		}
	}
	if got := metricsText(t, env); !strings.Contains(got, "duplex_sessions_active 2") {
		t.Fatalf("two-lane metrics = %s", got)
	}

	for _, l := range lanes {
		for seq := uint32(0); seq < 2; seq++ {
			gotSeq, pcm := readAudio(t, l.conn)
			if gotSeq != seq || !bytes.Equal(pcm, l.want[seq]) {
				t.Fatalf("lane echo seq=%d want=%d equal=%v", gotSeq, seq, bytes.Equal(pcm, l.want[seq]))
			}
		}
		if err := websocket.Message.Send(l.conn, `{"type":"session.close","reason":"client_done"}`); err != nil {
			t.Fatalf("close: %v", err)
		}
		seq, pcm := readAudio(t, l.conn)
		if seq != 2 || !bytes.Equal(pcm, l.want[2]) {
			t.Fatalf("lane flush seq=%d equal=%v", seq, bytes.Equal(pcm, l.want[2]))
		}
		expectStatsThenDone(t, l.conn, "client_done")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := metricsText(t, env)
		if strings.Contains(got, "duplex_sessions_active 0") &&
			strings.Contains(got, "duplex_sessions_total 2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics did not retire both sessions:\n%s", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
