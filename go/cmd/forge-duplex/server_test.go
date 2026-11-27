package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexwire"
	"golang.org/x/net/websocket"
)

type testEnv struct {
	ts  *httptest.Server
	srv *Server
}

func newTestEnv(t *testing.T, opts Options) *testEnv {
	t.Helper()
	if opts.NewDriver == nil {
		opts.NewDriver = newMockDriver
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	srv := New(opts)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, srv: srv}
}

func (e *testEnv) wsURL() string {
	return "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/v1/audio/duplex"
}

func (e *testEnv) dial(t *testing.T, protocols []string, header http.Header, query string) (*websocket.Conn, error) {
	t.Helper()
	cfg, err := websocket.NewConfig(e.wsURL()+query, e.ts.URL)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Protocol = protocols
	if header != nil {
		cfg.Header = header
	}
	return websocket.DialConfig(cfg)
}

func (e *testEnv) mustDial(t *testing.T, protocols []string) *websocket.Conn {
	t.Helper()
	conn, err := e.dial(t, protocols, nil, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readWire(t *testing.T, conn *websocket.Conn) wireMessage {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	m, err := readWireMessage(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

func readJSON(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	m := readWire(t, conn)
	if m.Type != websocket.TextFrame {
		t.Fatalf("message type = %d, want text", m.Type)
	}
	var v map[string]any
	if err := json.Unmarshal(m.Data, &v); err != nil {
		t.Fatalf("json: %v (%s)", err, m.Data)
	}
	return v
}

func readAudio(t *testing.T, conn *websocket.Conn) (uint32, []byte) {
	t.Helper()
	m := readWire(t, conn)
	if m.Type != websocket.BinaryFrame {
		t.Fatalf("message type = %d, want binary (%s)", m.Type, m.Data)
	}
	seq, pcm, err := duplexwire.DecodeAudioFrame(m.Data)
	if err != nil {
		t.Fatalf("decode audio: %v", err)
	}
	return seq, pcm
}

func sendStart(t *testing.T, conn *websocket.Conn, extra string) {
	t.Helper()
	body := `{"type":"session.start","version":1,"tenant":"loopback","sample_rate":24000,"text":true` + extra + `}`
	if err := websocket.Message.Send(conn, body); err != nil {
		t.Fatalf("send start: %v", err)
	}
}

func expectReady(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	v := readJSON(t, conn)
	if v["type"] != duplexwire.TypeSessionReady {
		t.Fatalf("first message = %v, want session.ready", v)
	}
	if v["version"].(float64) != 1 || v["sample_rate"].(float64) != 24000 || v["frame_samples"].(float64) != 3200 {
		t.Fatalf("ready = %v", v)
	}
	if !strings.HasPrefix(v["session_id"].(string), "dup_") {
		t.Fatalf("session_id = %v", v["session_id"])
	}
	return v
}

func sendAudio(t *testing.T, conn *websocket.Conn, seq uint32, seed byte) []byte {
	t.Helper()
	pcm := make([]byte, duplexwire.FramePayloadBytes)
	for i := range pcm {
		pcm[i] = byte(int(seed) + i)
	}
	msg, err := duplexwire.EncodeAudioFrame(seq, pcm)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := websocket.Message.Send(conn, msg); err != nil {
		t.Fatalf("send frame: %v", err)
	}
	return pcm
}

func expectError(t *testing.T, conn *websocket.Conn, code duplex.ErrorCode) {
	t.Helper()
	v := readJSON(t, conn)
	if v["type"] != duplexwire.TypeError {
		t.Fatalf("message = %v, want error", v)
	}
	if v["code"] != string(code) {
		t.Fatalf("code = %v, want %s (%v)", v["code"], code, v)
	}
	if v["fatal"] != true {
		t.Fatalf("fatal = %v", v["fatal"])
	}
}

func expectStatsThenDone(t *testing.T, conn *websocket.Conn, reason string) map[string]any {
	t.Helper()
	stats := readJSON(t, conn)
	if stats["type"] != duplexwire.TypeStats {
		t.Fatalf("message = %v, want stats", stats)
	}
	done := readJSON(t, conn)
	if done["type"] != duplexwire.TypeSessionDone {
		t.Fatalf("message = %v, want session.done", done)
	}
	if done["reason"] != reason {
		t.Fatalf("done reason = %v, want %s", done["reason"], reason)
	}
	if _, ok := done["stats"].(map[string]any); !ok {
		t.Fatalf("done has no stats block: %v", done)
	}
	return done
}

func TestEchoSessionAndClose(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, []string{dupVersionProtocol})

	sendStart(t, conn, `,"client":{"name":"test","version":"0"}`)
	ready := expectReady(t, conn)
	if ready["tenant"] != "loopback" || ready["max_minutes"].(float64) != 30 {
		t.Fatalf("ready = %v", ready)
	}
	caps := ready["capabilities"].(map[string]any)
	if caps["text"] != true {
		t.Fatalf("capabilities = %v", caps)
	}

	want := [][]byte{}
	for i := uint32(0); i < 3; i++ {
		want = append(want, sendAudio(t, conn, i, byte(i*7)))
	}
	for i := uint32(0); i < 3; i++ {
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
	done := expectStatsThenDone(t, conn, "client_done")
	st := done["stats"].(map[string]any)
	if st["in_frames"].(float64) != 3 || st["out_frames"].(float64) != 3 {
		t.Fatalf("final stats = %v", st)
	}
}

func TestBadFrameLength(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	if err := websocket.Message.Send(conn, []byte{duplexwire.AudioKind, 0, 0, 0, 0, 1, 2, 3}); err != nil {
		t.Fatalf("send: %v", err)
	}
	expectError(t, conn, duplex.ErrBadFrame)
}

func TestBadFrameKind(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	msg := make([]byte, duplexwire.FrameBytes)
	msg[0] = 0x02
	if err := websocket.Message.Send(conn, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	expectError(t, conn, duplex.ErrBadFrame)
}

func TestAudioBeforeReady(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendAudio(t, conn, 0, 1)
	expectError(t, conn, duplex.ErrBadFrame)
}

func TestSequenceGap(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	sendAudio(t, conn, 1, 1)
	expectError(t, conn, duplex.ErrBadSequence)
}

func TestSequenceDuplicate(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	sendAudio(t, conn, 0, 1)
	readAudio(t, conn)
	sendAudio(t, conn, 0, 2)
	expectError(t, conn, duplex.ErrBadSequence)
}

func TestUnsupportedVersion(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	if err := websocket.Message.Send(conn, `{"type":"session.start","version":2}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	expectError(t, conn, duplex.ErrUnsupported)
}

func TestUnsupportedRate(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	if err := websocket.Message.Send(conn, `{"type":"session.start","version":1,"sample_rate":16000}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	expectError(t, conn, duplex.ErrBadRequest)
}

func TestStartTwice(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)
	sendStart(t, conn, "")
	expectError(t, conn, duplex.ErrBadRequest)
}

func TestPing(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	if err := websocket.Message.Send(conn, `{"type":"ping","t":1700000000.5}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	v := readJSON(t, conn)
	if v["type"] != duplexwire.TypePong || v["t"].(float64) != 1700000000.5 {
		t.Fatalf("pong = %v", v)
	}
}

func TestAuthFailure(t *testing.T) {
	env := newTestEnv(t, Options{Tokens: []string{"s3cret"}})

	if _, err := env.dial(t, nil, nil, ""); err == nil {
		t.Fatal("unauthenticated dial succeeded")
	}
	bad := http.Header{}
	bad.Set("Authorization", "Bearer nope")
	if _, err := env.dial(t, nil, bad, ""); err == nil {
		t.Fatal("wrong bearer dial succeeded")
	}

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/v1/audio/duplex", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw handshake: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAuthSubprotocolAndHeader(t *testing.T) {
	env := newTestEnv(t, Options{Tokens: []string{"s3cret", "second"}})

	conn := env.mustDial(t, []string{dupVersionProtocol, "bearer.s3cret"})
	if got := conn.Config().Protocol; len(got) != 1 || got[0] != dupVersionProtocol {
		t.Fatalf("negotiated protocol = %v", got)
	}
	sendStart(t, conn, "")
	expectReady(t, conn)
	_ = websocket.Message.Send(conn, `{"type":"session.close"}`)
	expectStatsThenDone(t, conn, "client_done")

	h := http.Header{}
	h.Set("Authorization", "Bearer second")
	conn2, err := env.dial(t, nil, h, "")
	if err != nil {
		t.Fatalf("header auth dial: %v", err)
	}
	defer conn2.Close()
	sendStart(t, conn2, "")
	expectReady(t, conn2)
}

func TestTicketAuth(t *testing.T) {
	env := newTestEnv(t, Options{Tokens: []string{"s3cret"}, TicketTTL: time.Second})

	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/v1/audio/duplex/ticket", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ticket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ticket status = %d", resp.StatusCode)
	}
	var tr ticketResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil || tr.Ticket == "" {
		t.Fatalf("ticket body: %v", err)
	}

	conn, err := env.dial(t, []string{dupVersionProtocol}, nil, "?ticket="+tr.Ticket)
	if err != nil {
		t.Fatalf("ticket dial: %v", err)
	}
	sendStart(t, conn, "")
	expectReady(t, conn)
	_ = conn.Close()

	if _, err := env.dial(t, []string{dupVersionProtocol}, nil, "?ticket="+tr.Ticket); err == nil {
		t.Fatal("ticket was reusable")
	}

	noAuth, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/v1/audio/duplex/ticket", nil)
	resp2, err := http.DefaultClient.Do(noAuth)
	if err != nil {
		t.Fatalf("ticket no auth: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("ticket no-auth status = %d, want 403", resp2.StatusCode)
	}
}

func TestSessionLimit(t *testing.T) {
	env := newTestEnv(t, Options{MaxLanes: 1})
	first := env.mustDial(t, nil)
	sendStart(t, first, "")
	expectReady(t, first)

	second := env.mustDial(t, nil)
	sendStart(t, second, "")
	expectError(t, second, duplex.ErrSessionLimit)

	_ = websocket.Message.Send(first, `{"type":"session.close"}`)
	expectStatsThenDone(t, first, "client_done")
}

func TestTimeLimit(t *testing.T) {
	env := newTestEnv(t, Options{MaxSessionDuration: 150 * time.Millisecond})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)
	expectStatsThenDone(t, conn, "time_limit")
}

func TestCancel(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)
	if err := websocket.Message.Send(conn, `{"type":"session.cancel","reason":"user_hangup"}`); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	expectStatsThenDone(t, conn, "user_hangup")
}

func TestIdleTimeout(t *testing.T) {
	env := newTestEnv(t, Options{IdleTimeout: 120 * time.Millisecond})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)
	expectError(t, conn, duplex.ErrTimeout)
}

func TestStatsEmission(t *testing.T) {
	env := newTestEnv(t, Options{StatsInterval: 80 * time.Millisecond})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)
	sendAudio(t, conn, 0, 1)
	readAudio(t, conn)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no stats message within 2s")
		}
		v := readJSON(t, conn)
		if v["type"] == duplexwire.TypeStats {
			if v["in_frames"].(float64) != 1 || v["out_frames"].(float64) != 1 {
				t.Fatalf("stats = %v", v)
			}
			return
		}
	}
}

func TestQueueFull(t *testing.T) {
	env := newTestEnv(t, Options{NewDriver: func() duplex.DuplexDriver {
		return &queueFullDriver{mockDriver: newMockDriver().(*mockDriver)}
	}})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)
	sendAudio(t, conn, 0, 1)
	expectError(t, conn, duplex.ErrQueueFull)
}

type queueFullDriver struct {
	*mockDriver
}

func (q *queueFullDriver) PushInput(context.Context, duplex.Frame, uint32) error {
	return &duplex.Error{Code: duplex.ErrQueueFull, Message: "input queue is full", Fatal: true}
}

func TestOversizeControl(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	big := `{"type":"session.close","reason":"` + strings.Repeat("x", duplexwire.MaxControlMessage+1) + `"}`
	if err := websocket.Message.Send(conn, big); err != nil {
		t.Fatalf("send: %v", err)
	}
	expectError(t, conn, duplex.ErrBadRequest)
}

func TestKillSwitch(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	env.srv.Kill("ops kill")
	expectError(t, conn, duplex.ErrCancelled)

	if _, err := env.dial(t, nil, nil, ""); err == nil {
		t.Fatal("dial succeeded after kill switch")
	}
}

func TestHealthz(t *testing.T) {
	env := newTestEnv(t, Options{MaxLanes: 3})
	resp, err := http.Get(env.ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != true || body["max_lanes"].(float64) != 3 {
		t.Fatalf("body = %v", body)
	}
}

func TestMaxMinutesClamp(t *testing.T) {
	env := newTestEnv(t, Options{MaxMinutes: 5})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, `,"max_minutes":30`)
	ready := expectReady(t, conn)
	if ready["max_minutes"].(float64) != 5 {
		t.Fatalf("max_minutes = %v, want 5", ready["max_minutes"])
	}
}

func TestPCMRoundTripThroughSocket(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	pcm := make([]byte, duplexwire.FramePayloadBytes)
	for i := 0; i < duplex.FrameSamples; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16((i*37)%65536)))
	}
	msg, _ := duplexwire.EncodeAudioFrame(0, pcm)
	if err := websocket.Message.Send(conn, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, got := readAudio(t, conn)
	if !bytes.Equal(got, pcm) {
		t.Fatal("socket echo is not bit-exact")
	}
}

func TestOversizeBinary(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)

	big := make([]byte, duplexwire.MaxBinaryMessage+1)
	big[0] = duplexwire.AudioKind
	if err := websocket.Message.Send(conn, big); err != nil {
		t.Fatalf("send: %v", err)
	}
	expectError(t, conn, duplex.ErrBadFrame)
}

type outputOverflowDriver struct {
	*mockDriver
	fired bool
}

func (o *outputOverflowDriver) NextEvent(ctx context.Context, timeout time.Duration) (duplex.Event, error) {
	if !o.fired {
		o.fired = true
		return duplex.Event{Kind: duplex.EventError, Error: &duplex.Error{
			Code: duplex.ErrOutputOverflow, Message: "output queue stayed full", Fatal: true,
		}}, nil
	}
	return o.mockDriver.NextEvent(ctx, timeout)
}

func TestOutputOverflow(t *testing.T) {
	env := newTestEnv(t, Options{NewDriver: func() duplex.DuplexDriver {
		return &outputOverflowDriver{mockDriver: newMockDriver().(*mockDriver)}
	}})
	conn := env.mustDial(t, nil)
	sendStart(t, conn, "")
	expectReady(t, conn)
	expectError(t, conn, duplex.ErrOutputOverflow)
}

func TestMalformedControl(t *testing.T) {
	env := newTestEnv(t, Options{})
	conn := env.mustDial(t, nil)
	if err := websocket.Message.Send(conn, `{"type":"session.start","version":1,`); err != nil {
		t.Fatalf("send: %v", err)
	}
	expectError(t, conn, duplex.ErrBadRequest)
}

func TestLaneReleasedAfterClose(t *testing.T) {
	env := newTestEnv(t, Options{MaxLanes: 1})
	first := env.mustDial(t, nil)
	sendStart(t, first, "")
	expectReady(t, first)
	_ = websocket.Message.Send(first, `{"type":"session.close"}`)
	expectStatsThenDone(t, first, "client_done")
	_ = first.Close()

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
			t.Fatal("lane was not released after session.close")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A session.start that carries a reference voice is far larger than an audio frame. The WebSocket layer used to cap every message at 128 KiB, so a
// real browser's start was cut off even after the application-level limit was raised (found in a real browser run, 2026-11-20).
func TestStartMayCarryAReferenceVoiceOverTheWire(t *testing.T) {
	e := newTestEnv(t, Options{})
	conn := e.mustDial(t, nil)
	voice := base64.StdEncoding.EncodeToString(make([]byte, 400<<10)) // about 8 s of 24 kHz PCM16, 540 KB once encoded
	sendStart(t, conn, `,"reference":{"id":"v","audio_b64":"`+voice+`"}`)
	expectReady(t, conn)
	sendAudio(t, conn, 0, 3)
	readAudio(t, conn)
	// After the start the frame-sized limit is back: a large message later is refused.
	big := `{"type":"ping","t":1,"pad":"` + strings.Repeat("x", duplexwire.MaxBinaryMessage+10) + `"}`
	_ = websocket.Message.Send(conn, big)
	if _, _, err := func() (int, []byte, error) {
		var s string
		err := websocket.Message.Receive(conn, &s)
		return 0, nil, err
	}(); err == nil {
		// The server may answer with an error message before it closes; either way the connection must not carry on normally.
		var s string
		if err := websocket.Message.Receive(conn, &s); err == nil {
			t.Error("an oversized message after the start did not end the connection")
		}
	}
}

func TestReassembleText(t *testing.T) {
	obj := `{"type":"session.start","version":1,"client":{"name":"` + strings.Repeat("x", 300000) + `"}}`
	cut := func(s string, n int) []wireMessage {
		var out []wireMessage
		for i := 0; i < len(s); i += n {
			e := i + n
			if e > len(s) {
				e = len(s)
			}
			out = append(out, wireMessage{Type: websocket.TextFrame, Data: []byte(s[i:e])})
		}
		return out
	}
	feed := func(parts []wireMessage) func() (wireMessage, error) {
		i := 0
		return func() (wireMessage, error) {
			if i >= len(parts) {
				return wireMessage{}, io.EOF
			}
			i++
			return parts[i-1], nil
		}
	}
	// The pieces a browser sends (131,000 bytes each) come back as the one message.
	parts := cut(obj, 131000)
	got, err := reassembleText(parts[0], feed(parts[1:]))
	if err != nil || string(got.Data) != obj {
		t.Fatalf("reassembled %d bytes (err %v), want %d", len(got.Data), err, len(obj))
	}
	// A message that is already complete, and a small broken one, are left exactly as they were.
	small := wireMessage{Type: websocket.TextFrame, Data: []byte(`{"type":"ping"`)}
	if got, _ := reassembleText(small, func() (wireMessage, error) { t.Fatal("read past a small broken message"); return wireMessage{}, nil }); string(got.Data) != `{"type":"ping"` {
		t.Error("a small broken message was changed")
	}
	whole := wireMessage{Type: websocket.TextFrame, Data: []byte(obj)}
	if got, _ := reassembleText(whole, func() (wireMessage, error) { t.Fatal("read past a complete message"); return wireMessage{}, nil }); string(got.Data) != obj {
		t.Error("a complete message was changed")
	}
	// A binary frame is never reassembled.
	bin := wireMessage{Type: websocket.BinaryFrame, Data: make([]byte, 200000)}
	if got, _ := reassembleText(bin, func() (wireMessage, error) { t.Fatal("read past a binary frame"); return wireMessage{}, nil }); len(got.Data) != 200000 {
		t.Error("a binary frame was changed")
	}
	// Reading stops at the start limit, and a dropped connection mid-message is an error.
	endless := wireMessage{Type: websocket.TextFrame, Data: []byte(`{"a":"` + strings.Repeat("x", 100000))}
	reads := 0
	got, _ = reassembleText(endless, func() (wireMessage, error) {
		reads++
		return wireMessage{Type: websocket.TextFrame, Data: []byte(strings.Repeat("x", 131000))}, nil
	})
	if len(got.Data) > duplexwire.MaxStartMessage+131000 || reads > 40 {
		t.Errorf("an endless message was read for %d pieces (%d bytes)", reads, len(got.Data))
	}
	if _, err := reassembleText(parts[0], feed(nil)); err == nil {
		t.Error("a connection dropped mid-message was not an error")
	}
}
