// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/statutil"
)

// This file is the conformance harness, not the platform. fakeDriver is a
// deterministic stand-in for WS1's driver (DUP-010..014) that implements the
// documented DuplexDriver behaviour from protocol.md §4–§6 closely enough for
// the contract and fault suites to run before that driver exists. When WS1
// lands, point the suite at the real driver: these tests are its acceptance
// set, not a second implementation to maintain. Every place the harness adds
// behaviour (§5 ring/queue bounds, sequence accounting, timeout) is marked with
// the protocol rule it mirrors.
//
// The harness is single-goroutine by design. Tests drive the clock explicitly,
// so pacing, lateness and the 2 s overflow deadline need no sleeps and cannot
// flake; making the driver safe for a transport thread is WS1's job.

// fakeClock replaces wall time. A test that slept could not assert exact
// counts of empty ticks or an exact 2 s deadline.
type fakeClock struct{ now time.Time }

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time                  { return c.now }
func (c *fakeClock) Advance(d time.Duration)         { c.now = c.now.Add(d) }
func (c *fakeClock) Since(t time.Time) time.Duration { return c.now.Sub(t) }

// fakeDriverConfig overrides the protocol defaults (§5: input ring 8 frames,
// output queue 4 frames) or injects tenant cost.
type fakeDriverConfig struct {
	SessionID string
	InputCap  int
	OutputCap int
	// StepDelay is the tenant cost charged to fake time after Step returns, so
	// step percentiles are exact and not a measure of the test machine.
	StepDelay func(seq uint32) time.Duration
}

type queuedInput struct {
	frame Frame
	seq   uint32
}

type fakeDriver struct {
	tenant     DuplexTenant
	tenantInfo TenantInfo
	clock      *fakeClock
	cfg        fakeDriverConfig
	ctx        context.Context

	started   bool
	startedAt time.Time
	stopped   bool
	sess      TenantSession
	terminal  *Error

	ring        []queuedInput
	expectedSeq uint32
	emptyTicks  int
	starved     bool

	events       []Event
	audioQueued  int
	outFullSince time.Time
	outSeqs      []uint32 // outbound sequence mirroring evidence (see Drain)

	stats       Stats
	stepSamples []float64
}

var _ DuplexDriver = (*fakeDriver)(nil)

func newFakeDriver(t *testing.T, tenant DuplexTenant, cfg fakeDriverConfig) *fakeDriver {
	t.Helper()
	info, err := tenant.Load(context.Background())
	if err != nil {
		t.Fatalf("tenant.Load: %v", err)
	}
	if cfg.SessionID == "" {
		cfg.SessionID = "dup_test"
	}
	if cfg.InputCap == 0 {
		cfg.InputCap = 8
	}
	if cfg.OutputCap == 0 {
		cfg.OutputCap = 4
	}
	return &fakeDriver{
		tenant:     tenant,
		tenantInfo: info,
		clock:      newFakeClock(),
		cfg:        cfg,
		ctx:        context.Background(),
	}
}

func (d *fakeDriver) Start(ctx context.Context, cfg SessionConfig) (SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, err
	}
	if d.started {
		return SessionInfo{}, &Error{Code: ErrBadRequest, Message: "session already started", Fatal: true}
	}
	// §7.1: start validates SessionConfig before the tenant sees it.
	if cfg.SampleRate != 0 && cfg.SampleRate != SampleRate {
		return SessionInfo{}, &Error{
			Code:    ErrBadRequest,
			Message: fmt.Sprintf("sample_rate %d, want %d", cfg.SampleRate, SampleRate),
			Fatal:   true,
		}
	}
	if cfg.MaxMinutes < 0 {
		return SessionInfo{}, &Error{
			Code:    ErrBadRequest,
			Message: fmt.Sprintf("max_minutes %d, want >= 0", cfg.MaxMinutes),
			Fatal:   true,
		}
	}
	sess, err := d.tenant.Start(ctx, TenantConfig{
		SessionID:  d.cfg.SessionID,
		SampleRate: SampleRate,
		MaxMinutes: cfg.MaxMinutes,
		Text:       cfg.Text,
		Card:       cfg.Card,
		Reference:  cfg.Reference,
	})
	if err != nil {
		return SessionInfo{}, d.mapErr(ErrTenantUnavailable, err)
	}
	maxMinutes := cfg.MaxMinutes
	if maxMinutes == 0 {
		maxMinutes = 30 // §3.1 default when the request is absent
	}
	name := d.tenantInfo.Name
	if cfg.Tenant != "" {
		name = cfg.Tenant
	}
	d.sess = sess
	d.started = true
	d.startedAt = d.clock.Now()
	d.ctx = ctx
	d.stats.SessionID = d.cfg.SessionID
	return SessionInfo{
		SessionID:    d.cfg.SessionID,
		Tenant:       name,
		SampleRate:   SampleRate,
		FrameSamples: FrameSamples,
		FrameMillis:  float64(FrameSamples) * 1000 / float64(SampleRate),
		MaxMinutes:   maxMinutes,
		Capabilities: d.tenantInfo.Capabilities,
	}, nil
}

func (d *fakeDriver) PushInput(ctx context.Context, in Frame, seq uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !d.started {
		return &Error{Code: ErrBadRequest, Message: "input before session.ready", Fatal: true}
	}
	if d.stopped {
		return d.terminalOrCancelled()
	}
	if !in.Valid() {
		return d.fail(ErrBadFrame, "frame has %d samples, want %d", len(in), FrameSamples)
	}
	// §2.1/§10.2: per-direction sequence starts at 0 and never gaps or repeats.
	if seq != d.expectedSeq {
		return d.fail(ErrBadSequence, "seq %d, want %d", seq, d.expectedSeq)
	}
	// §5: the ring is bounded; overflow is queue_full and fatal in v1.
	if len(d.ring) >= d.cfg.InputCap {
		return d.fail(ErrQueueFull, "input ring is full (%d frames)", d.cfg.InputCap)
	}
	if d.starved {
		// §5: a frame that arrives after a tick found the ring empty is late.
		// It is still consumed in order; it is never skipped or synthesized.
		d.stats.LateInputs++
		d.starved = false
	}
	// The transport reuses its buffer after PushInput returns, so the driver
	// must own its copy. A test that hands the same slice twice catches it.
	frame := make(Frame, FrameSamples)
	copy(frame, in)
	d.ring = append(d.ring, queuedInput{frame: frame, seq: seq})
	d.expectedSeq++
	return nil
}

// Tick advances the frame clock once: one frame period, then at most one tenant
// Step. It is the deterministic form of §4.2's "one input frame per tick". It
// returns a fatal *Error when the tick terminalized the session, nil otherwise.
func (d *fakeDriver) Tick() error {
	if !d.started {
		return &Error{Code: ErrBadRequest, Message: "tick before session.ready", Fatal: true}
	}
	if d.stopped {
		return d.terminalOrCancelled()
	}
	d.clock.Advance(FrameDuration)

	// §5: the output queue is bounded. When full, pause the tenant and record
	// output_overflow; 2 seconds full is fatal. Audio is never dropped to make
	// room, which is what makes this observable instead of silent.
	if d.audioQueued >= d.cfg.OutputCap {
		if d.outFullSince.IsZero() {
			d.outFullSince = d.clock.Now()
			d.events = append(d.events, Event{Kind: EventError, Error: &Error{
				Code:    ErrOutputOverflow,
				Message: fmt.Sprintf("output queue full (%d frames); tenant paused", d.cfg.OutputCap),
				Fatal:   false,
			}})
			return nil
		}
		if d.clock.Since(d.outFullSince) >= 2*time.Second {
			return d.fail(ErrOutputOverflow, "output queue stayed full for 2s")
		}
		return nil
	}
	d.outFullSince = time.Time{}

	// §5: an empty ring is an empty tick. The driver waits one frame for input;
	// more than three empty ticks is timeout. It never invents a frame.
	if len(d.ring) == 0 {
		d.starved = true
		d.emptyTicks++
		if d.emptyTicks > 3 {
			return d.fail(ErrTimeout, "no input for %d frame periods", d.emptyTicks)
		}
		return nil
	}
	d.starved = false
	d.emptyTicks = 0
	in := d.ring[0]
	d.ring = d.ring[1:]

	start := d.clock.Now()
	tf, err := d.sess.Step(d.ctx, in.frame, in.seq)
	if d.cfg.StepDelay != nil {
		d.clock.Advance(d.cfg.StepDelay(in.seq))
	}
	d.recordStep(d.clock.Since(start))
	if err != nil {
		return d.mapStepErr(err)
	}
	d.stats.InFrames++
	d.enqueueTenantFrame(tf, in.seq)
	return nil
}

// Drain is the documented one-frame drain (§5: draining flushes the output
// queue). A tenant may hold one frame at most (§7.2), so when fewer frames have
// been emitted than consumed, one extra Step with a zero frame returns it. It
// does not count as an input frame, does not advance the input sequence, and is
// not a second tenant output. The accounting guard makes Drain idempotent and
// keeps it a no-op for a zero-delay tenant, which must never be handed a
// fabricated frame.
func (d *fakeDriver) Drain() error {
	if !d.started {
		return &Error{Code: ErrBadRequest, Message: "drain before session.ready", Fatal: true}
	}
	if d.stopped {
		return d.terminalOrCancelled()
	}
	if d.sess == nil || d.stats.OutFrames >= d.stats.InFrames {
		return nil
	}
	start := d.clock.Now()
	tf, err := d.sess.Step(d.ctx, make(Frame, FrameSamples), d.expectedSeq)
	if d.cfg.StepDelay != nil {
		d.clock.Advance(d.cfg.StepDelay(d.expectedSeq))
	}
	d.recordStep(d.clock.Since(start))
	if err != nil {
		return d.mapStepErr(err)
	}
	d.enqueueTenantFrame(tf, d.expectedSeq)
	return nil
}

func (d *fakeDriver) NextEvent(ctx context.Context, _ time.Duration) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	if len(d.events) > 0 {
		ev := d.events[0]
		d.events = d.events[1:]
		if ev.Kind == EventAudio {
			d.audioQueued--
			d.outFullSince = time.Time{}
		}
		return ev, nil
	}
	// The harness never blocks: an empty queue is the end of this run, and a
	// test that wants more drives Tick itself. A real driver waits up to
	// timeout here (contract.go).
	return Event{}, io.EOF
}

func (d *fakeDriver) Cancel(reason string) error {
	if !d.started || d.stopped {
		return nil // idempotent (§7.1)
	}
	if err := d.sess.Close(); err != nil {
		return err
	}
	d.sess = nil
	// §4: draining sends a final stats then session.done; audio already queued
	// stays ahead of them so the client sees no silent loss.
	d.events = append(d.events, Event{Kind: EventStats, Stats: d.Stats()})
	d.events = append(d.events, Event{Kind: EventDone, Reason: reason})
	d.stopped = true
	return nil
}

func (d *fakeDriver) Close() error {
	if !d.started {
		return nil
	}
	if !d.stopped {
		return d.Cancel("closed")
	}
	return nil
}

func (d *fakeDriver) Stats() Stats {
	s := d.stats
	if d.started {
		s.Uptime = d.clock.Since(d.startedAt)
	}
	s.QueueIn = len(d.ring)
	s.QueueOut = d.audioQueued
	if len(d.stepSamples) > 0 {
		s.StepP50MS = statutil.Percentile(d.stepSamples, 50)
		s.StepP95MS = statutil.Percentile(d.stepSamples, 95)
		s.StepMaxMS = d.stepSamples[0]
		for _, v := range d.stepSamples[1:] {
			if v > s.StepMaxMS {
				s.StepMaxMS = v
			}
		}
	}
	// End-to-end needs the client's playout timestamps, which the harness does
	// not have; WS1/WS5 own that figure. It is deliberately left at zero rather
	// than faked, so a test cannot mistake it for a measurement.
	return s
}

func (d *fakeDriver) enqueueTenantFrame(tf TenantFrame, inSeq uint32) {
	if tf.Audio != nil {
		outSeq := inSeq
		if v, ok := tf.Meta["echo_seq"].(uint32); ok {
			outSeq = v
		}
		d.outSeqs = append(d.outSeqs, outSeq)
		d.events = append(d.events, Event{Kind: EventAudio, Audio: Frame(tf.Audio)})
		d.audioQueued++
		d.stats.OutFrames++
	}
	if tf.Text != "" {
		d.events = append(d.events, Event{Kind: EventText, Text: tf.Text})
	}
}

func (d *fakeDriver) recordStep(dur time.Duration) {
	d.stepSamples = append(d.stepSamples, float64(dur)/float64(time.Millisecond))
}

func (d *fakeDriver) fail(code ErrorCode, format string, args ...any) *Error {
	e := &Error{Code: code, Message: fmt.Sprintf(format, args...), Fatal: true}
	if d.terminal == nil {
		d.terminal = e
		d.events = append(d.events, Event{Kind: EventError, Error: e})
		d.stopped = true
		if d.sess != nil {
			// §4: an error is terminal and the lane is released immediately.
			_ = d.sess.Close()
			d.sess = nil
		}
	}
	return e
}

func (d *fakeDriver) mapErr(fallback ErrorCode, err error) *Error {
	var de *Error
	if errors.As(err, &de) {
		return de
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Code: ErrCancelled, Message: err.Error(), Fatal: true}
	}
	return &Error{Code: fallback, Message: err.Error(), Fatal: true}
}

func (d *fakeDriver) mapStepErr(err error) *Error {
	e := d.mapErr(ErrInternal, err)
	return d.fail(e.Code, "%s", e.Message)
}

func (d *fakeDriver) terminalOrCancelled() *Error {
	if d.terminal != nil {
		return d.terminal
	}
	return &Error{Code: ErrCancelled, Message: "session is closed", Fatal: true}
}

// collect reads every queued event until the harness reports io.EOF.
func collect(t *testing.T, d *fakeDriver) []Event {
	t.Helper()
	var out []Event
	for {
		ev, err := d.NextEvent(context.Background(), FrameDuration)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("NextEvent: %v", err)
		}
		out = append(out, ev)
	}
}

// eventsOfKind filters a collected event stream.
func eventsOfKind(events []Event, kind EventKind) []Event {
	var out []Event
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// spyTenant wraps a real tenant so a test can count lifecycle calls, capture
// the TenantConfig each lane received, and inject a Start or Step failure
// without a model.
type spyTenant struct {
	inner    DuplexTenant
	startErr error
	stepErr  error

	loads, starts, closes int
	sessions              []*spySession
}

func (s *spyTenant) Load(ctx context.Context) (TenantInfo, error) {
	s.loads++
	return s.inner.Load(ctx)
}

func (s *spyTenant) Start(ctx context.Context, cfg TenantConfig) (TenantSession, error) {
	s.starts++
	if s.startErr != nil {
		return nil, s.startErr
	}
	inner, err := s.inner.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ss := &spySession{tenant: s, inner: inner, cfg: cfg}
	s.sessions = append(s.sessions, ss)
	return ss, nil
}

func (s *spyTenant) Close() error {
	s.closes++
	return s.inner.Close()
}

type spySession struct {
	tenant *spyTenant
	inner  TenantSession
	cfg    TenantConfig
	steps  int
	closes int
}

func (s *spySession) Step(ctx context.Context, in Frame, seq uint32) (TenantFrame, error) {
	s.steps++
	if s.tenant.stepErr != nil {
		return TenantFrame{}, s.tenant.stepErr
	}
	return s.inner.Step(ctx, in, seq)
}

func (s *spySession) Close() error {
	s.closes++
	return s.inner.Close()
}
