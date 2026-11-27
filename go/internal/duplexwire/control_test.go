package duplexwire

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/duplex"
)

func decodeErr(t *testing.T, raw string) *duplex.Error {
	t.Helper()
	_, err := DecodeClientMessage([]byte(raw))
	if err == nil {
		t.Fatalf("want an error for %s", raw)
	}
	pe, ok := err.(*duplex.Error)
	if !ok {
		t.Fatalf("error is %T, want *duplex.Error: %v", err, err)
	}
	return pe
}

func TestDecodeStart(t *testing.T) {
	audio := base64.StdEncoding.EncodeToString([]byte("hello"))
	msg, err := DecodeClientMessage([]byte(`{
		"type":"session.start","version":1,"tenant":"loopback","sample_rate":24000,
		"max_minutes":30,"text":true,
		"card":{"name":"Haru","nested":{"n":1}},
		"reference":{"id":"sana","audio_b64":"` + audio + `","transcript":"hi"},
		"client":{"name":"forge-web","version":"0.1"},
		"future_field":"ignored"
	}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Type != TypeSessionStart || msg.Start == nil {
		t.Fatalf("got %+v", msg)
	}
	s := msg.Start
	if s.Version != 1 || s.Tenant != "loopback" || s.SampleRate != 24000 || s.MaxMinutes != 30 || !s.Text {
		t.Fatalf("start = %+v", s)
	}
	if s.Card["name"] != "Haru" {
		t.Fatalf("card = %#v", s.Card)
	}
	if s.Reference.ID != "sana" || string(s.Reference.Audio) != "hello" || s.Reference.Transcript != "hi" {
		t.Fatalf("reference = %+v", s.Reference)
	}
	if s.Client.Name != "forge-web" || s.Client.Version != "0.1" {
		t.Fatalf("client = %+v", s.Client)
	}
}

func TestDecodeStartDefaultsAndRate(t *testing.T) {
	msg, err := DecodeClientMessage([]byte(`{"type":"session.start","version":1}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Start.SampleRate != duplex.SampleRate {
		t.Fatalf("default rate = %d", msg.Start.SampleRate)
	}
	if msg.Start.Tenant != "" || msg.Start.Text {
		t.Fatalf("unexpected defaults: %+v", msg.Start)
	}
}

func TestDecodeStartUnsupportedVersion(t *testing.T) {
	e := decodeErr(t, `{"type":"session.start","version":2}`)
	if e.Code != duplex.ErrUnsupported || !e.Fatal {
		t.Fatalf("got %+v", e)
	}
	e = decodeErr(t, `{"type":"session.start"}`)
	if e.Code != duplex.ErrUnsupported {
		t.Fatalf("missing version: %+v", e)
	}
}

func TestDecodeStartBadRate(t *testing.T) {
	e := decodeErr(t, `{"type":"session.start","version":1,"sample_rate":16000}`)
	if e.Code != duplex.ErrBadRequest || !strings.Contains(e.Message, "sample_rate") {
		t.Fatalf("got %+v", e)
	}
}

func TestDecodeStartCardCap(t *testing.T) {
	big := strings.Repeat("x", MaxCardBytes+1)
	e := decodeErr(t, `{"type":"session.start","version":1,"card":{"name":"`+big+`"}}`)
	if e.Code != duplex.ErrBadRequest || !strings.Contains(e.Message, "card") {
		t.Fatalf("got %+v", e)
	}
}

func TestDecodeStartReferenceCaps(t *testing.T) {
	e := decodeErr(t, `{"type":"session.start","version":1,"reference":{"audio_b64":"!!!"}}`)
	if e.Code != duplex.ErrBadRequest {
		t.Fatalf("bad base64: %+v", e)
	}

	big := base64.StdEncoding.EncodeToString(make([]byte, MaxReferenceAudioBytes+1))
	e = decodeErr(t, `{"type":"session.start","version":1,"reference":{"audio_b64":"`+big+`"}}`)
	if e.Code != duplex.ErrBadRequest {
		t.Fatalf("oversize audio: %+v", e)
	}

	long := strings.Repeat("t", MaxTranscriptBytes+1)
	e = decodeErr(t, `{"type":"session.start","version":1,"reference":{"transcript":"`+long+`"}}`)
	if e.Code != duplex.ErrBadRequest {
		t.Fatalf("oversize transcript: %+v", e)
	}
}

func TestDecodeControlTypes(t *testing.T) {
	for _, raw := range []string{`{"type":"session.close","reason":"client_done"}`, `{"type":"session.cancel","reason":"user_hangup"}`} {
		msg, err := DecodeClientMessage([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if msg.Reason == "" {
			t.Fatalf("%s: missing reason", raw)
		}
	}

	msg, err := DecodeClientMessage([]byte(`{"type":"ping","t":1700000000.5}`))
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if msg.Type != TypePing || msg.T != 1700000000.5 {
		t.Fatalf("ping = %+v", msg)
	}
}

func TestDecodeUnknownAndMalformed(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"type":"nope"}`,
		`{}`,
		`{"type":"session.start","version":1,"card":"not-an-object"}`,
	} {
		if e := decodeErr(t, raw); e.Code != duplex.ErrBadRequest {
			t.Fatalf("%s: got %+v", raw, e)
		}
	}
}

func TestDecodeControlTooLarge(t *testing.T) {
	raw := `{"type":"session.close","reason":"` + strings.Repeat("x", MaxControlMessage) + `"}`
	e := decodeErr(t, raw)
	if e.Code != duplex.ErrBadRequest {
		t.Fatalf("got %+v", e)
	}
}

func TestReadyDefaults(t *testing.T) {
	info := duplex.SessionInfo{SessionID: "dup_x", Tenant: "loopback"}
	m := NewReady(info)
	if m.Version != Version || m.FrameSamples != duplex.FrameSamples || m.SampleRate != duplex.SampleRate {
		t.Fatalf("ready = %+v", m)
	}
	if len(m.Capabilities.InputRates) != 1 || m.Capabilities.InputRates[0] != duplex.SampleRate {
		t.Fatalf("rates = %v", m.Capabilities.InputRates)
	}
	if m.FrameMillis < 133 || m.FrameMillis > 134 {
		t.Fatalf("frame_ms = %v", m.FrameMillis)
	}
}

func TestStatsShape(t *testing.T) {
	s := duplex.Stats{InFrames: 3, OutFrames: 2, LateInputs: 1, StepP50MS: 34.2, StepP95MS: 48, StepMaxMS: 51.1}
	b, err := json.Marshal(NewStats(7, s))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != TypeStats || got["seq"].(float64) != 7 || got["in_frames"].(float64) != 3 {
		t.Fatalf("stats = %s", b)
	}
	step, ok := got["step_ms"].(map[string]any)
	if !ok || step["p50"].(float64) != 34.2 {
		t.Fatalf("step_ms = %v", got["step_ms"])
	}
	if _, ok := got["gpu"].(map[string]any); !ok {
		t.Fatalf("missing gpu block: %s", b)
	}
}

func TestErrorAndDone(t *testing.T) {
	e := NewError(&duplex.Error{Code: duplex.ErrQueueFull, Message: "input queue is full", Fatal: true})
	b, _ := json.Marshal(e)
	if !strings.Contains(string(b), `"code":"queue_full"`) || !strings.Contains(string(b), `"fatal":true`) {
		t.Fatalf("error json = %s", b)
	}
	if errors.Is(nil, ErrSequenceGap) {
		t.Fatal("unreachable")
	}
}

// The reference voice travels inline in session.start, so that message (and only that one) may exceed the 64 KiB control limit.
func TestDecodeStartMayCarryAReferenceVoiceButNothingElseMayBeLarge(t *testing.T) {
	voice := base64.StdEncoding.EncodeToString(make([]byte, 700<<10)) // about 15 s of 24 kHz PCM16
	raw := `{"type":"session.start","version":1,"reference":{"id":"reina","audio_b64":"` + voice + `"}}`
	if len(raw) <= MaxControlMessage {
		t.Fatalf("test message is only %d bytes: it must exceed the control limit to mean anything", len(raw))
	}
	msg, err := DecodeClientMessage([]byte(raw))
	if err != nil {
		t.Fatalf("a start carrying a 700 KiB voice was refused: %v", err)
	}
	if len(msg.Start.Reference.Audio) != 700<<10 {
		t.Fatalf("decoded reference is %d bytes, want %d", len(msg.Start.Reference.Audio), 700<<10)
	}
	for name, raw := range map[string]string{
		"close":  `{"type":"session.close","reason":"` + strings.Repeat("x", MaxControlMessage) + `"}`,
		"ping":   `{"type":"ping","t":1,"pad":"` + strings.Repeat("x", MaxControlMessage) + `"}`,
		"cancel": `{"type":"session.cancel","reason":"` + strings.Repeat("x", MaxControlMessage) + `"}`,
	} {
		if e := decodeErr(t, raw); e.Code != duplex.ErrBadRequest {
			t.Errorf("%s over the control limit: %+v", name, e)
		}
	}
	huge := `{"type":"session.start","version":1,"client":{"name":"` + strings.Repeat("x", MaxStartMessage) + `"}}`
	if e := decodeErr(t, huge); e.Code != duplex.ErrBadRequest {
		t.Errorf("a start over MaxStartMessage: %+v", e)
	}
}
