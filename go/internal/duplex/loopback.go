// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"context"
	"fmt"
)

// LoopbackTenant is the echo tenant from protocol.md §7.3. It exists so the
// platform can be built and tested end to end before a model is attached: one
// audio frame in, the same frame out, no text, and card/reference are ignored.
//
// DelayFrames is the echo latency in frames. 1 is the platform's documented
// behaviour: the frame given to Step at tick n is returned by the Step at tick
// n+1, so the audio path carries exactly one frame (133.33 ms) of latency. 0
// returns the frame from the same Step and is what the bit-exact wire test
// uses. Start rejects any other value instead of clamping it, because a
// conformance run must not silently test a latency the platform does not have.
//
// In delay mode one input frame is still buffered when the caller stops
// sending. A driver that wants every input frame back — protocol.md §5 says
// "no silent loss" — performs one extra Step with a zero frame while draining;
// the conformance harness does exactly that in fakeDriver.Drain.
//
// The loopback allocates one output frame per Step. That is deliberate: it is
// a test tenant, and handing the caller's buffer back would alias the input
// ring. Model tenants are the ones held to the no-allocation rule in
// contract.go, not this one. A LoopbackTenant is immutable after construction
// and safe for concurrent sessions (Start from two lanes never shares state).
type LoopbackTenant struct {
	DelayFrames int
}

// NewLoopbackTenant returns the echo tenant with the documented one-frame
// delay. This is the tenant the platform runs for M1.
func NewLoopbackTenant() *LoopbackTenant { return &LoopbackTenant{DelayFrames: 1} }

// NewZeroDelayLoopbackTenant returns the bit-exact echo tenant: Step returns
// exactly the samples it was given. Use it for wire/format tests, where a
// one-frame offset would only make the comparison harder to read.
func NewZeroDelayLoopbackTenant() *LoopbackTenant { return &LoopbackTenant{DelayFrames: 0} }

// Load reports the loopback's capabilities. It is process-scoped and
// idempotent: a service loads every tenant once at boot, not per session.
func (t *LoopbackTenant) Load(ctx context.Context) (TenantInfo, error) {
	if err := ctx.Err(); err != nil {
		return TenantInfo{}, err
	}
	return TenantInfo{
		Name: "loopback",
		Capabilities: Capabilities{
			// The loopback never emits text (protocol.md §7.3); saying so here
			// is what stops session.ready from promising a text.delta stream.
			Text:       false,
			InputRates: []int{SampleRate},
		},
	}, nil
}

// Start validates the per-session config and returns a fresh lane. Card and
// reference are accepted and ignored, as protocol.md §7.3 requires: the
// conformance suite must be able to send whatever a real tenant would receive.
func (t *LoopbackTenant) Start(ctx context.Context, cfg TenantConfig) (TenantSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.DelayFrames != 0 && t.DelayFrames != 1 {
		return nil, &Error{
			Code:    ErrBadRequest,
			Message: fmt.Sprintf("loopback: delay frames %d, want 0 or 1", t.DelayFrames),
			Fatal:   true,
		}
	}
	if cfg.SampleRate != 0 && cfg.SampleRate != SampleRate {
		return nil, &Error{
			Code:    ErrBadRequest,
			Message: fmt.Sprintf("loopback: sample_rate %d, want %d", cfg.SampleRate, SampleRate),
			Fatal:   true,
		}
	}
	if cfg.MaxMinutes < 0 {
		return nil, &Error{
			Code:    ErrBadRequest,
			Message: fmt.Sprintf("loopback: max_minutes %d, want >= 0", cfg.MaxMinutes),
			Fatal:   true,
		}
	}
	return &loopbackSession{delay: t.DelayFrames}, nil
}

// Close releases process-wide state. The loopback has none, so it only marks
// the tenant closed for tests that check idempotency.
func (t *LoopbackTenant) Close() error { return nil }

// loopbackSession is one lane's echo state. The only state is the frame held
// for the one-frame delay, so two lanes can never see each other's audio.
type loopbackSession struct {
	delay      int
	pending    Frame
	pendingSeq uint32
	hasPending bool
	closed     bool
}

// Step echoes one frame. In zero-delay mode the returned frame is a copy of
// in, so it is bit-exact by construction. In delay mode it returns the frame
// and sequence buffered by the previous Step (nil on the priming tick) and
// buffers this one.
//
// Meta["echo_seq"] carries the input sequence of the frame being echoed. The
// contract's Event has no sequence field, so the driver/transport needs this
// to mirror the client's sequence on the wire (protocol.md §2.1); see
// docs/duplex/notes/ws4-conformance.md.
func (s *loopbackSession) Step(ctx context.Context, in Frame, seq uint32) (TenantFrame, error) {
	if err := ctx.Err(); err != nil {
		return TenantFrame{}, err
	}
	if s.closed {
		return TenantFrame{}, &Error{Code: ErrCancelled, Message: "loopback: session closed", Fatal: true}
	}
	if !in.Valid() {
		return TenantFrame{}, &Error{
			Code:    ErrBadFrame,
			Message: fmt.Sprintf("loopback: frame has %d samples, want %d", len(in), FrameSamples),
			Fatal:   true,
		}
	}
	if s.delay == 0 {
		out := make(Frame, FrameSamples)
		copy(out, in)
		return TenantFrame{Audio: out, Meta: map[string]any{"echo_seq": seq}}, nil
	}
	tf := TenantFrame{}
	if s.hasPending {
		tf.Audio = s.pending
		tf.Meta = map[string]any{"echo_seq": s.pendingSeq}
	}
	next := make(Frame, FrameSamples)
	copy(next, in)
	s.pending, s.pendingSeq, s.hasPending = next, seq, true
	return tf, nil
}

// Flush returns the frame held by the one-frame delay, if any. The driver calls
// it once while draining so the last input frame is not lost. It is idempotent.
func (s *loopbackSession) Flush(ctx context.Context) (TenantFrame, error) {
	if err := ctx.Err(); err != nil {
		return TenantFrame{}, err
	}
	if s.closed || !s.hasPending {
		return TenantFrame{}, nil
	}
	tf := TenantFrame{Audio: s.pending, Meta: map[string]any{"echo_seq": s.pendingSeq}}
	s.pending, s.hasPending = nil, false
	return tf, nil
}

// Close releases the lane's held frame. It is idempotent; a Step after Close
// returns cancelled rather than touching a released buffer.
func (s *loopbackSession) Close() error {
	s.closed = true
	s.pending = nil
	s.hasPending = false
	return nil
}
