package duplexwire

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jsaigou/the-forge/internal/duplex"
)

// Client message types.
const (
	TypeSessionStart  = "session.start"
	TypeSessionClose  = "session.close"
	TypeSessionCancel = "session.cancel"
	TypePing          = "ping"

	TypeSessionReady = "session.ready"
	TypeTextDelta    = "text.delta"
	TypeStats        = "stats"
	TypeSessionDone  = "session.done"
	TypeError        = "error"
	TypePong         = "pong"
)

// ClientMessage is one decoded text message. Only the fields for Type are set.
type ClientMessage struct {
	Type   string
	Start  *StartRequest
	Reason string
	T      float64
}

// StartRequest is a validated session.start. MaxMinutes is the client's
// request (0 = server default); the transport clamps it.
type StartRequest struct {
	Version    int
	Tenant     string
	SampleRate int
	MaxMinutes int
	Text       bool
	Card       map[string]any
	Reference  duplex.Reference
	Client     duplex.ClientInfo
}

func badRequest(param, format string, args ...any) *duplex.Error {
	e := &duplex.Error{Code: duplex.ErrBadRequest, Message: fmt.Sprintf(format, args...), Fatal: true}
	if param != "" {
		e.Message = param + ": " + e.Message
	}
	return e
}

// DecodeClientMessage parses and validates one text control message. Unknown
// JSON fields are ignored; an unknown type is bad_request. The returned error,
// when non-nil, is always a *duplex.Error with a stable wire code.
func DecodeClientMessage(b []byte) (ClientMessage, error) {
	tooBig := func(limit int) error {
		return &duplex.Error{
			Code:    duplex.ErrBadRequest,
			Message: fmt.Sprintf("control message is %d bytes, limit is %d", len(b), limit),
			Fatal:   true,
		}
	}
	if len(b) > MaxStartMessage {
		return ClientMessage{}, tooBig(MaxStartMessage)
	}

	var env startEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return ClientMessage{}, badRequest("", "invalid control JSON: %v", err)
	}
	// Only the session.start may be large (it can carry the reference voice); every other message keeps the small limit.
	if len(b) > MaxControlMessage && env.Type != TypeSessionStart {
		return ClientMessage{}, tooBig(MaxControlMessage)
	}

	switch env.Type {
	case TypeSessionStart:
		start, err := validateStart(&env)
		if err != nil {
			return ClientMessage{}, err
		}
		return ClientMessage{Type: env.Type, Start: start}, nil
	case TypeSessionClose, TypeSessionCancel:
		if len(env.Reason) > 256 {
			return ClientMessage{}, badRequest("reason", "reason is longer than 256 bytes")
		}
		return ClientMessage{Type: env.Type, Reason: env.Reason}, nil
	case TypePing:
		return ClientMessage{Type: env.Type, T: env.T}, nil
	case "":
		return ClientMessage{}, badRequest("type", "missing message type")
	default:
		return ClientMessage{}, badRequest("type", "unknown message type %q", env.Type)
	}
}

type startEnvelope struct {
	Type    string          `json:"type"`
	Version int             `json:"version"`
	Tenant  string          `json:"tenant"`
	Rate    int             `json:"sample_rate"`
	Minutes int             `json:"max_minutes"`
	Text    *bool           `json:"text"`
	Card    json.RawMessage `json:"card"`
	Ref     *struct {
		ID         string `json:"id"`
		AudioB64   string `json:"audio_b64"`
		Transcript string `json:"transcript"`
	} `json:"reference"`
	Client *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"client"`
	Reason string  `json:"reason"`
	T      float64 `json:"t"`
}

func validateStart(env *startEnvelope) (*StartRequest, error) {
	if env.Version != Version {
		return nil, &duplex.Error{
			Code:    duplex.ErrUnsupported,
			Message: fmt.Sprintf("protocol version %d is not supported (want %d)", env.Version, Version),
			Fatal:   true,
		}
	}

	rate := env.Rate
	if rate == 0 {
		rate = duplex.SampleRate
	}
	if rate != duplex.SampleRate {
		return nil, badRequest("sample_rate", "sample rate %d Hz is not supported; v1 is 24000 Hz mono", rate)
	}

	if len(env.Tenant) > MaxTenantNameBytes {
		return nil, badRequest("tenant", "tenant name is longer than %d bytes", MaxTenantNameBytes)
	}

	if env.Minutes < 0 {
		return nil, badRequest("max_minutes", "max_minutes must not be negative")
	}
	if env.Minutes > 7*24*60 {
		return nil, badRequest("max_minutes", "max_minutes is unreasonably large")
	}

	start := &StartRequest{
		Version:    env.Version,
		Tenant:     strings.TrimSpace(env.Tenant),
		SampleRate: rate,
		MaxMinutes: env.Minutes,
		Text:       env.Text != nil && *env.Text,
	}

	if len(env.Card) > 0 && string(env.Card) != "null" {
		if len(env.Card) > MaxCardBytes {
			return nil, badRequest("card", "card is %d bytes, limit is %d", len(env.Card), MaxCardBytes)
		}
		if err := json.Unmarshal(env.Card, &start.Card); err != nil {
			return nil, badRequest("card", "card must be a JSON object")
		}
	}

	if env.Ref != nil {
		if len(env.Ref.ID) > 256 {
			return nil, badRequest("reference.id", "reference id is longer than 256 bytes")
		}
		start.Reference.ID = env.Ref.ID
		start.Reference.Transcript = env.Ref.Transcript
		if len(env.Ref.Transcript) > MaxTranscriptBytes {
			return nil, badRequest("reference.transcript", "transcript is longer than %d bytes", MaxTranscriptBytes)
		}
		if env.Ref.AudioB64 != "" {
			audio, err := decodeB64(env.Ref.AudioB64)
			if err != nil {
				return nil, badRequest("reference.audio_b64", "reference audio is not valid base64")
			}
			if len(audio) > MaxReferenceAudioBytes {
				return nil, badRequest("reference.audio_b64", "reference audio is %d bytes decoded, limit is %d", len(audio), MaxReferenceAudioBytes)
			}
			start.Reference.Audio = audio
		}
	}

	if env.Client != nil {
		if len(env.Client.Name) > MaxClientFieldBytes || len(env.Client.Version) > MaxClientFieldBytes {
			return nil, badRequest("client", "client name/version is longer than %d bytes", MaxClientFieldBytes)
		}
		start.Client.Name = env.Client.Name
		start.Client.Version = env.Client.Version
	}

	return start, nil
}

func decodeB64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// ReadyMessage is session.ready.
type ReadyMessage struct {
	Type         string            `json:"type"`
	SessionID    string            `json:"session_id"`
	Version      int               `json:"version"`
	Tenant       string            `json:"tenant"`
	SampleRate   int               `json:"sample_rate"`
	FrameSamples int               `json:"frame_samples"`
	FrameMillis  float64           `json:"frame_ms"`
	MaxMinutes   int               `json:"max_minutes"`
	Capabilities ReadyCapabilities `json:"capabilities"`
}

// ReadyCapabilities is the additive capability block in session.ready.
type ReadyCapabilities struct {
	Text       bool  `json:"text"`
	InputRates []int `json:"input_rates"`
}

// NewReady builds session.ready from the driver's negotiated info.
func NewReady(info duplex.SessionInfo) ReadyMessage {
	rates := info.Capabilities.InputRates
	if len(rates) == 0 {
		rates = []int{duplex.SampleRate}
	}
	frameSamples := info.FrameSamples
	if frameSamples == 0 {
		frameSamples = duplex.FrameSamples
	}
	frameMillis := info.FrameMillis
	if frameMillis == 0 {
		frameMillis = float64(duplex.FrameDuration.Microseconds()) / 1000
	}
	sampleRate := info.SampleRate
	if sampleRate == 0 {
		sampleRate = duplex.SampleRate
	}
	return ReadyMessage{
		Type:         TypeSessionReady,
		SessionID:    info.SessionID,
		Version:      Version,
		Tenant:       info.Tenant,
		SampleRate:   sampleRate,
		FrameSamples: frameSamples,
		FrameMillis:  frameMillis,
		MaxMinutes:   info.MaxMinutes,
		Capabilities: ReadyCapabilities{Text: info.Capabilities.Text, InputRates: rates},
	}
}

// TextDeltaMessage is text.delta.
type TextDeltaMessage struct {
	Type string `json:"type"`
	Seq  uint32 `json:"seq"`
	Text string `json:"text"`
}

// NewTextDelta builds a text.delta event.
func NewTextDelta(seq uint32, text string) TextDeltaMessage {
	return TextDeltaMessage{Type: TypeTextDelta, Seq: seq, Text: text}
}

// PongMessage answers a client ping.
type PongMessage struct {
	Type string  `json:"type"`
	T    float64 `json:"t"`
}

// ErrorMessage is a terminal (or, only for model_not_ready, retryable) error.
type ErrorMessage struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// NewError builds an error message from a platform error.
func NewError(e *duplex.Error) ErrorMessage {
	return ErrorMessage{Type: TypeError, Code: string(e.Code), Message: e.Message, Fatal: e.Fatal}
}

// DoneMessage ends a session after a graceful close, cancel or time limit.
type DoneMessage struct {
	Type   string       `json:"type"`
	Reason string       `json:"reason"`
	Stats  StatsMessage `json:"stats"`
}

// StatsMessage is the stats event shape (protocol.md §3.2). GPU accounting is
// filled by the driver when it exposes it; the transport reports zeros.
type StatsMessage struct {
	Type      string    `json:"type"`
	Seq       uint64    `json:"seq"`
	UptimeMS  int64     `json:"uptime_ms"`
	InFrames  uint64    `json:"in_frames"`
	OutFrames uint64    `json:"out_frames"`
	LateIn    uint64    `json:"late_inputs"`
	LateOut   uint64    `json:"late_outputs"`
	QueueIn   int       `json:"queue_in"`
	QueueOut  int       `json:"queue_out"`
	Dropped   uint64    `json:"dropped"`
	StepMS    LatencyMS `json:"step_ms"`
	E2EMS     LatencyMS `json:"end_to_end_ms"`
	GPU       GPUBytes  `json:"gpu"`
}

// LatencyMS is a p50/p95(/max) millisecond block.
type LatencyMS struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max,omitempty"`
}

// GPUBytes is the reserved/peak GPU memory block.
type GPUBytes struct {
	UsedBytes int64 `json:"used_bytes"`
	PeakBytes int64 `json:"peak_bytes"`
}

// NewStats converts platform accounting to the wire shape.
func NewStats(seq uint64, s duplex.Stats) StatsMessage {
	return StatsMessage{
		Type:      TypeStats,
		Seq:       seq,
		UptimeMS:  s.Uptime.Milliseconds(),
		InFrames:  s.InFrames,
		OutFrames: s.OutFrames,
		LateIn:    s.LateInputs,
		LateOut:   s.LateOutputs,
		QueueIn:   s.QueueIn,
		QueueOut:  s.QueueOut,
		Dropped:   s.Dropped,
		StepMS:    LatencyMS{P50: s.StepP50MS, P95: s.StepP95MS, Max: s.StepMaxMS},
		E2EMS:     LatencyMS{P50: s.E2EP50MS, P95: s.E2EP95MS},
	}
}

// NewDone builds session.done with the final stats.
func NewDone(reason string, seq uint64, s duplex.Stats) DoneMessage {
	return DoneMessage{Type: TypeSessionDone, Reason: reason, Stats: NewStats(seq, s)}
}
