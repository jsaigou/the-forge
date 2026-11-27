// SPDX-License-Identifier: Apache-2.0

package duplexremote

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"golang.org/x/net/websocket"
)

// MockEngineOptions shape the mock engine's behaviour. The zero value is a
// ready, zero-delay echo engine with one tenant named "mock".
type MockEngineOptions struct {
	Tenant      string
	Text        bool // advertise text and emit "f<seq>" per frame
	DelayFrames int  // 0: echo in the same step; 1: echo one step later (Flush recovers the last)
	MaxLanes    int  // 0: unlimited

	// Fault injection, each off by default.
	NotReady      bool
	Contract      string  // override the advertised contract version
	FrameSamples  int     // override the advertised frame size
	FailStartCode string  // reply to start with this error code
	FailAtSeq     *uint32 // reply with an error to this input seq
	FailCode      string  // code for FailAtSeq (default "internal")
	StallAtSeq    *uint32 // never answer this input seq
	DropAtSeq     *uint32 // close the socket instead of answering this seq
	SeqOffset     uint32  // echo seq+SeqOffset (a misbehaving engine)

	// MaxReferenceBytes is the largest reference total the engine accepts and
	// advertises (default 64 MiB).
	MaxReferenceBytes int64
}

// MockStart is what the engine received in one start message.
type MockStart struct {
	SessionID  string
	Tenant     string
	Text       bool
	MaxMinutes int
	// Card is the JSON value the engine received: usually the structured
	// object, or a string when the tenant rendered it.
	Card      any
	Reference struct {
		ID         string
		Bytes      int
		Transcript string
	}
	ReferenceAudio []byte
	// ReferenceChunks is how many binary messages the reference arrived in and
	// LargestChunk the biggest of them.
	ReferenceChunks int
	LargestChunk    int
}

// MockEngine implements the engine side of docs/duplex/engine-contract.md.
type MockEngine struct {
	opts MockEngineOptions

	mu     sync.Mutex
	active int
	steps  int
	starts []MockStart
}

// NewMockEngine returns a mock engine with the given options.
func NewMockEngine(opts MockEngineOptions) *MockEngine {
	if opts.Tenant == "" {
		opts.Tenant = "mock"
	}
	return &MockEngine{opts: opts}
}

// Active is the number of lanes currently held by the engine.
func (m *MockEngine) Active() int { m.mu.Lock(); defer m.mu.Unlock(); return m.active }

// Steps is the number of input frames the engine has accepted.
func (m *MockEngine) Steps() int { m.mu.Lock(); defer m.mu.Unlock(); return m.steps }

// Starts returns a copy of the start messages received so far.
func (m *MockEngine) Starts() []MockStart {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MockStart(nil), m.starts...)
}

// Handler serves /duplex/v1/info and /duplex/v1/session.
func (m *MockEngine) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathInfo, m.info)
	mux.Handle(pathSession, websocket.Server{Handler: m.session})
	return mux
}

func (m *MockEngine) maxRef() int64 {
	if m.opts.MaxReferenceBytes > 0 {
		return m.opts.MaxReferenceBytes
	}
	return 64 << 20
}

func (m *MockEngine) info(w http.ResponseWriter, _ *http.Request) {
	contract := m.opts.Contract
	if contract == "" {
		contract = ContractVersion
	}
	samples := m.opts.FrameSamples
	if samples == 0 {
		samples = duplex.FrameSamples
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"contract":            contract,
		"engine":              "mock",
		"engine_version":      "test",
		"tenants":             []map[string]any{{"name": m.opts.Tenant, "text": m.opts.Text, "max_lanes": m.opts.MaxLanes}},
		"frame":               map[string]any{"sample_rate": duplex.SampleRate, "samples": samples, "format": "f32le"},
		"max_reference_bytes": m.maxRef(),
		"ready":               !m.opts.NotReady,
	})
}

func mockError(ws *websocket.Conn, code, msg string) {
	b, _ := json.Marshal(map[string]any{"type": "error", "code": code, "message": msg, "fatal": true})
	_ = sendText(ws, b)
}

func (m *MockEngine) session(ws *websocket.Conn) {
	defer ws.Close()
	first, err := recvMsg(ws)
	if err != nil {
		return
	}
	if !first.Text {
		mockError(ws, "bad_request", "first message must be a JSON start")
		return
	}
	var req struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		Tenant    string `json:"tenant"`
		Text      bool   `json:"text"`
		MaxMin    int    `json:"max_minutes"`
		Card      any    `json:"card"`
		Reference struct {
			ID         string `json:"id"`
			Bytes      int    `json:"bytes"`
			Transcript string `json:"transcript"`
		} `json:"reference"`
	}
	if json.Unmarshal(first.Data, &req) != nil || req.Type != "start" {
		mockError(ws, "bad_request", "first message must be start")
		return
	}
	if req.Tenant != m.opts.Tenant {
		mockError(ws, "tenant_unavailable", "unknown tenant")
		return
	}
	if m.opts.FailStartCode != "" {
		mockError(ws, m.opts.FailStartCode, "start refused")
		return
	}
	m.mu.Lock()
	if m.opts.MaxLanes > 0 && m.active >= m.opts.MaxLanes {
		m.mu.Unlock()
		mockError(ws, "session_limit", "no free lane")
		return
	}
	m.active++
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.active--; m.mu.Unlock() }()

	rec := MockStart{SessionID: req.SessionID, Tenant: req.Tenant, Text: req.Text, MaxMinutes: req.MaxMin, Card: req.Card}
	rec.Reference.ID, rec.Reference.Bytes, rec.Reference.Transcript = req.Reference.ID, req.Reference.Bytes, req.Reference.Transcript
	if req.Reference.Bytes > 0 {
		if int64(req.Reference.Bytes) > m.maxRef() {
			mockError(ws, "bad_request", "reference is larger than the engine accepts")
			return
		}
		for len(rec.ReferenceAudio) < req.Reference.Bytes {
			chunk, err := recvMsg(ws)
			if err != nil {
				return
			}
			if chunk.Text || len(chunk.Data) > MaxReferenceChunk ||
				len(rec.ReferenceAudio)+len(chunk.Data) > req.Reference.Bytes {
				mockError(ws, "bad_request", "reference audio: a binary message of at most 1 MiB, adding up to reference.bytes")
				return
			}
			rec.ReferenceAudio = append(rec.ReferenceAudio, chunk.Data...)
			rec.ReferenceChunks++
			if len(chunk.Data) > rec.LargestChunk {
				rec.LargestChunk = len(chunk.Data)
			}
		}
	}
	m.mu.Lock()
	m.starts = append(m.starts, rec)
	m.mu.Unlock()

	ready, _ := json.Marshal(map[string]any{"type": "ready", "text": m.opts.Text, "input_rates": []int{duplex.SampleRate}})
	if sendText(ws, ready) != nil {
		return
	}

	var held []float32 // delay mode: the previous input frame
	var lastSeq, nextSeq uint32
	for {
		msg, err := recvMsg(ws)
		if err != nil {
			return
		}
		if msg.Text {
			var ctl struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(msg.Data, &ctl)
			switch ctl.Type {
			case "close":
				return
			case "flush":
				if sendBinary(ws, encodeStepOut(lastSeq, held, "")) != nil {
					return
				}
				held = nil
			}
			continue
		}
		seq, frame, err := decodeStepIn(msg.Data)
		if err != nil {
			mockError(ws, "bad_frame", err.Error())
			return
		}
		if seq != nextSeq {
			mockError(ws, "bad_sequence", "input seq is not the next expected")
			return
		}
		nextSeq++
		m.mu.Lock()
		m.steps++
		m.mu.Unlock()
		lastSeq = seq

		switch {
		case m.opts.DropAtSeq != nil && *m.opts.DropAtSeq == seq:
			return
		case m.opts.StallAtSeq != nil && *m.opts.StallAtSeq == seq:
			time.Sleep(5 * time.Second)
			return
		case m.opts.FailAtSeq != nil && *m.opts.FailAtSeq == seq:
			code := m.opts.FailCode
			if code == "" {
				code = "internal"
			}
			mockError(ws, code, "injected failure")
			return
		}

		var out []float32
		if m.opts.DelayFrames == 0 {
			out = frame
		} else {
			out, held = held, frame
		}
		text := ""
		if m.opts.Text {
			text = "f" + itoa(seq)
		}
		if sendBinary(ws, encodeStepOut(seq+m.opts.SeqOffset, out, text)) != nil {
			return
		}
	}
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
