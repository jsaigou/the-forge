// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexsafety"
	"github.com/jsaigou/the-forge/internal/duplexwire"
	"golang.org/x/net/websocket"
)

var scriptedSessionSeq atomic.Uint64

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type scriptedDriver struct {
	mu sync.Mutex

	events []duplex.Event
	next   int

	cancelled    bool
	cancelReason string
	doneSent     bool
	notify       chan struct{}

	sessionID string
	inFrames  uint64
	outFrames uint64
}

func newScriptedDriver(events []duplex.Event) *scriptedDriver {
	return &scriptedDriver{
		events:    events,
		notify:    make(chan struct{}, 1),
		sessionID: fmt.Sprintf("dup_script_%d", scriptedSessionSeq.Add(1)),
	}
}

func (d *scriptedDriver) Start(_ context.Context, cfg duplex.SessionConfig) (duplex.SessionInfo, error) {
	maxMinutes := cfg.MaxMinutes
	if maxMinutes == 0 {
		maxMinutes = defaultMaxMinutes
	}
	return duplex.SessionInfo{
		SessionID:    d.sessionID,
		Tenant:       cfg.Tenant,
		SampleRate:   duplex.SampleRate,
		FrameSamples: duplex.FrameSamples,
		FrameMillis:  float64(duplex.FrameDuration.Microseconds()) / 1000,
		MaxMinutes:   maxMinutes,
		Capabilities: duplex.Capabilities{Text: true, InputRates: []int{duplex.SampleRate}},
	}, nil
}

func (d *scriptedDriver) PushInput(_ context.Context, _ duplex.Frame, _ uint32) error {
	d.mu.Lock()
	d.inFrames++
	d.mu.Unlock()
	return nil
}

func (d *scriptedDriver) NextEvent(ctx context.Context, timeout time.Duration) (duplex.Event, error) {
	for {
		d.mu.Lock()
		if d.next < len(d.events) {
			ev := d.events[d.next]
			d.next++
			if ev.Kind == duplex.EventAudio {
				d.outFrames++
			}
			d.mu.Unlock()
			return ev, nil
		}
		if !d.doneSent {
			d.doneSent = true
			reason := "done"
			if d.cancelled {
				reason = d.cancelReason
				if reason == "" {
					reason = "cancelled"
				}
			}
			d.mu.Unlock()
			return duplex.Event{Kind: duplex.EventDone, Reason: reason}, nil
		}
		d.mu.Unlock()

		timer := time.NewTimer(timeout)
		select {
		case <-ctx.Done():
			timer.Stop()
			return duplex.Event{}, ctx.Err()
		case <-d.notify:
			timer.Stop()
		case <-timer.C:
			return duplex.Event{}, context.DeadlineExceeded
		}
	}
}

func (d *scriptedDriver) Cancel(reason string) error {
	d.mu.Lock()
	if !d.cancelled {
		d.cancelled = true
		d.cancelReason = reason
	}
	d.mu.Unlock()
	select {
	case d.notify <- struct{}{}:
	default:
	}
	return nil
}

func (d *scriptedDriver) Close() error {
	d.mu.Lock()
	if !d.cancelled {
		d.cancelled = true
		d.cancelReason = "closed"
	}
	d.mu.Unlock()
	select {
	case d.notify <- struct{}{}:
	default:
	}
	return nil
}

func (d *scriptedDriver) Stats() duplex.Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return duplex.Stats{InFrames: d.inFrames, OutFrames: d.outFrames}
}

func scriptedAudioFrame(v float32) duplex.Frame {
	f := make(duplex.Frame, duplex.FrameSamples)
	for i := range f {
		f[i] = v
	}
	return f
}

func scriptedEvents(frames []duplex.Frame, texts []string) []duplex.Event {
	events := make([]duplex.Event, 0, len(frames)+len(texts))
	for _, f := range frames {
		events = append(events, duplex.Event{Kind: duplex.EventAudio, Audio: f})
	}
	for _, t := range texts {
		events = append(events, duplex.Event{Kind: duplex.EventText, Text: t})
	}
	return events
}

type sessionTrace struct {
	audioPCM   [][]byte
	texts      []string
	doneReason string
}

func readSessionUntilDone(t *testing.T, conn *websocket.Conn) sessionTrace {
	t.Helper()
	var tr sessionTrace
	for {
		m := readWire(t, conn)
		if m.Type == websocket.BinaryFrame {
			_, pcm, err := duplexwire.DecodeAudioFrame(m.Data)
			if err != nil {
				t.Fatalf("decode audio: %v", err)
			}
			tr.audioPCM = append(tr.audioPCM, append([]byte(nil), pcm...))
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(m.Data, &v); err != nil {
			t.Fatalf("json: %v (%s)", err, m.Data)
		}
		switch v["type"] {
		case duplexwire.TypeTextDelta:
			text, _ := v["text"].(string)
			tr.texts = append(tr.texts, text)
		case duplexwire.TypeStats:
		case duplexwire.TypeSessionDone:
			tr.doneReason, _ = v["reason"].(string)
			return tr
		case duplexwire.TypeError:
			t.Fatalf("session error: %s", m.Data)
		default:
			t.Fatalf("unexpected message: %s", m.Data)
		}
	}
}

func expectSafetyMetric(t *testing.T, env *testEnv, code string, want int) {
	t.Helper()
	wantLine := fmt.Sprintf("duplex_safety_stops_total{code=%q} %d\n", code, want)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(metricsText(t, env), wantLine) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics missing %q:\n%s", wantLine, metricsText(t, env))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func expectLaneFree(t *testing.T, env *testEnv) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		second, err := env.dial(t, nil, nil, "")
		if err == nil {
			sendStart(t, second, "")
			if v := readJSON(t, second); v["type"] == duplexwire.TypeSessionReady {
				_ = second.Close()
				return
			}
			_ = second.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("lane was not released after the safety stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSafetyStopEndsSessionAndReleasesLane(t *testing.T) {
	loop := []string{
		"I think it is ", "I think it is ", "I think it is ",
		"I think it is ", "I think it is ", "I think it is ",
	}
	frames := []duplex.Frame{
		scriptedAudioFrame(0.25),
		scriptedAudioFrame(0.5),
		scriptedAudioFrame(-0.5),
	}
	events := scriptedEvents(frames, loop)
	var logs syncBuffer

	env := newTestEnv(t, Options{
		MaxLanes: 1,
		Logger:   log.New(&logs, "", 0),
		NewDriver: func() duplex.DuplexDriver {
			return newScriptedDriver(events)
		},
		NewSafety: func() duplexsafety.Policy {
			return duplexsafety.NewLoopDetector(duplexsafety.Default())
		},
	})
	conn := env.mustDial(t, []string{dupVersionProtocol})
	sendStart(t, conn, "")
	expectReady(t, conn)

	tr := readSessionUntilDone(t, conn)
	if tr.doneReason != "safety_stop" {
		t.Fatalf("done reason = %q, want safety_stop", tr.doneReason)
	}
	if len(tr.audioPCM) != len(frames) {
		t.Fatalf("received %d audio frames, want %d", len(tr.audioPCM), len(frames))
	}
	for i, f := range frames {
		want, err := duplexwire.PCMFromFrame(f)
		if err != nil {
			t.Fatalf("encode frame %d: %v", i, err)
		}
		if !bytes.Equal(tr.audioPCM[i], want) {
			t.Fatalf("audio frame %d was altered", i)
		}
	}
	if len(tr.texts) != 3 {
		t.Fatalf("received %d text deltas before the stop, want 3: %q", len(tr.texts), tr.texts)
	}

	logText := logs.String()
	if !strings.Contains(logText, "session safety stop id=") || !strings.Contains(logText, "code=loop") {
		t.Fatalf("missing safety log line:\n%s", logText)
	}
	if strings.Contains(logText, "I think") {
		t.Fatalf("safety log leaked transcript text:\n%s", logText)
	}

	expectSafetyMetric(t, env, "loop", 1)
	expectLaneFree(t, env)
}

func TestSafetyOffLeavesLoopStreamUntouched(t *testing.T) {
	loop := []string{
		"I think it is ", "I think it is ", "I think it is ",
		"I think it is ", "I think it is ", "I think it is ",
	}
	env := newTestEnv(t, Options{
		NewDriver: func() duplex.DuplexDriver {
			return newScriptedDriver(scriptedEvents(nil, loop))
		},
	})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	tr := readSessionUntilDone(t, conn)
	if tr.doneReason != "done" {
		t.Fatalf("done reason = %q, want done", tr.doneReason)
	}
	if len(tr.texts) != len(loop) {
		t.Fatalf("received %d text deltas, want %d", len(tr.texts), len(loop))
	}
	if strings.Contains(metricsText(t, env), "duplex_safety_stops_total") {
		t.Fatalf("safety metric present with the policy off:\n%s", metricsText(t, env))
	}
}

func TestNormalStreamNotStoppedThroughSession(t *testing.T) {
	normal := []string{
		"Hello there. ",
		"How are you today? ",
		"I am fine, thanks. ",
	}
	env := newTestEnv(t, Options{
		NewDriver: func() duplex.DuplexDriver {
			return newScriptedDriver(scriptedEvents(nil, normal))
		},
		NewSafety: func() duplexsafety.Policy {
			return duplexsafety.NewLoopDetector(duplexsafety.Default())
		},
	})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	tr := readSessionUntilDone(t, conn)
	if tr.doneReason != "done" {
		t.Fatalf("done reason = %q, want done", tr.doneReason)
	}
	if len(tr.texts) != len(normal) {
		t.Fatalf("received %d text deltas, want %d", len(tr.texts), len(normal))
	}
	if strings.Contains(metricsText(t, env), "duplex_safety_stops_total") {
		t.Fatalf("normal stream counted a safety stop:\n%s", metricsText(t, env))
	}
}
