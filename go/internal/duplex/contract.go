// Package duplex defines the v1 platform/tenant contract for a continuous
// audio-in / audio-out session. It is intentionally behavior-free: the driver,
// transport, loopback tenant and future model tenants implement these types.
//
// The wire contract is docs/duplex/protocol.md. This file is the code-level
// freeze point: additive changes are fine; breaking changes need a protocol
// version bump and a note in docs/duplex/decisions.md.
package duplex

import (
	"context"
	"time"
)

const (
	// SampleRate is the platform's internal and wire rate.
	SampleRate = 24000
	// FrameSamples is one 133.33 ms frame at SampleRate.
	FrameSamples = 3200
	// FrameDuration is the frame period.
	FrameDuration = 133*time.Millisecond + 333*time.Microsecond
)

// Frame is one mono audio frame in [-1, 1] at SampleRate. A valid frame has
// exactly FrameSamples samples; implementations may use a slice so a caller can
// avoid a copy.
type Frame []float32

// Valid reports whether the frame has the platform's frame length.
func (f Frame) Valid() bool { return len(f) == FrameSamples }

// SessionConfig is what the transport asks the driver to start.
type SessionConfig struct {
	Tenant     string
	SampleRate int
	MaxMinutes int
	Text       bool
	Card       map[string]any
	Reference  Reference
	Client     ClientInfo
}

// Reference is the voice conditioning for a tenant. Audio is optional; ID names
// a server-side reference when it is set.
type Reference struct {
	ID         string
	Audio      []byte
	Transcript string
}

// ClientInfo is informational metadata from the client.
type ClientInfo struct {
	Name    string
	Version string
}

// SessionInfo is returned once a driver has allocated a lane and started the
// tenant. It is what the transport reports in session.ready.
type SessionInfo struct {
	SessionID    string
	Tenant       string
	SampleRate   int
	FrameSamples int
	FrameMillis  float64
	MaxMinutes   int
	Capabilities Capabilities
}

// Capabilities is additive; clients must ignore unknown fields.
type Capabilities struct {
	Text       bool
	InputRates []int
}

// TenantInfo is what a tenant reports after Load.
type TenantInfo struct {
	Name         string
	Capabilities Capabilities
}

// TenantConfig is the per-session tenant configuration. The platform passes the
// card and reference through opaquely; the tenant validates what it supports.
type TenantConfig struct {
	SessionID  string
	SampleRate int
	MaxMinutes int
	Text       bool
	Card       map[string]any
	Reference  Reference
}

// TenantFrame is one tenant step's output. Audio may be nil when the model is
// silent; Text may be empty. A tenant returns at most one frame per Step.
type TenantFrame struct {
	Audio []float32
	Text  string
	Meta  map[string]any
}

// Stats is the platform's per-session accounting. Durations are wall-clock
// milliseconds. Queue values are samples/frames at the time of the report.
type Stats struct {
	SessionID   string
	Uptime      time.Duration
	InFrames    uint64
	OutFrames   uint64
	LateInputs  uint64
	LateOutputs uint64
	QueueIn     int
	QueueOut    int
	Dropped     uint64
	StepP50MS   float64
	StepP95MS   float64
	StepMaxMS   float64
	E2EP50MS    float64
	E2EP95MS    float64
}

// ErrorCode is a stable wire error name (protocol.md §6).
type ErrorCode string

const (
	ErrAuth              ErrorCode = "auth"
	ErrBadRequest        ErrorCode = "bad_request"
	ErrUnsupported       ErrorCode = "unsupported_version"
	ErrBadFrame          ErrorCode = "bad_frame"
	ErrBadSequence       ErrorCode = "bad_sequence"
	ErrModelNotReady     ErrorCode = "model_not_ready"
	ErrTenantUnavailable ErrorCode = "tenant_unavailable"
	ErrSessionLimit      ErrorCode = "session_limit"
	ErrQueueFull         ErrorCode = "queue_full"
	ErrOutputOverflow    ErrorCode = "output_overflow"
	ErrTimeout           ErrorCode = "timeout"
	ErrCancelled         ErrorCode = "cancelled"
	ErrInternal          ErrorCode = "internal"
)

// Error is a typed platform/tenant error. Fatal reports whether the session
// must close; model_not_ready is the only retryable pre-ready code.
type Error struct {
	Code    ErrorCode
	Message string
	Fatal   bool
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// EventKind is the driver's output event kind.
type EventKind int

const (
	EventAudio EventKind = iota + 1
	EventText
	EventStats
	EventDone
	EventError
)

// Event is one event returned by Driver.NextEvent.
type Event struct {
	Kind   EventKind
	Audio  Frame
	Text   string
	Stats  Stats
	Error  *Error
	Reason string
}

// DuplexTenant is a model implementation. Load is called once per process;
// Start is called once per session/lane. Step must not allocate or block on the
// hot path beyond the frame budget.
type DuplexTenant interface {
	Load(ctx context.Context) (TenantInfo, error)
	Start(ctx context.Context, cfg TenantConfig) (TenantSession, error)
	Close() error
}

// TenantSession is one lane's model state.
type TenantSession interface {
	Step(ctx context.Context, in Frame, seq uint32) (TenantFrame, error)
	Close() error
}

// Flusher is an optional TenantSession hook for a tenant that buffers a frame
// (the loopback's one-frame delay). The driver calls it once while draining, so
// the last input frame is not silently lost. A tenant that has nothing buffered
// returns an empty TenantFrame.
type Flusher interface {
	Flush(ctx context.Context) (TenantFrame, error)
}

// DuplexDriver is the platform's session engine. Start allocates a lane and
// starts the tenant; PushInput is called by the transport; NextEvent by the
// session thread. Cancel and Close are idempotent.
type DuplexDriver interface {
	Start(ctx context.Context, cfg SessionConfig) (SessionInfo, error)
	PushInput(ctx context.Context, in Frame, seq uint32) error
	NextEvent(ctx context.Context, timeout time.Duration) (Event, error)
	Cancel(reason string) error
	Close() error
	Stats() Stats
}
