// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"
)

// Driver default queue sizes and failure thresholds. The input ring holds up
// to 24 frames (3.2 s) of early/absorbed input (it was 8; a network stall of a second or more delivers its whole backlog at once, and an overflow ends the
// session); the output queue holds 4 frames of
// model-ahead-of-transport slack; the text queue is small and bounded.
const (
	defaultInputRing   = 24
	defaultOutputQueue = 4
	defaultTextQueue   = 16
	// maxEmptyTicks is how many consecutive frame periods may pass with no input before the session fails (about 4 s). It used to be 3 (0.4 s), so a
	// browser tab that lost focus, or a network hiccup, ended a live conversation; a stall now just pauses the model until input resumes.
	maxEmptyTicks = 30
	// catchUpBacklog and maxCatchUp: when more than catchUpBacklog input frames are queued (a burst after a stall leaves a standing backlog that
	// otherwise stays as extra latency for the rest of the session, because the clock consumes one frame per period), step again at once, at most
	// maxCatchUp extra steps per period. A step takes about 75 ms against a 133 ms period, so the backlog drains in a few periods.
	catchUpBacklog      = 1
	maxCatchUp          = 3
	outputOverflowAfter = 2 * time.Second
	defaultMaxMinutes   = 30
)

// inputEntry is one queued input frame with its sequence and arrival time.
type inputEntry struct {
	frame Frame
	seq   uint32
	at    time.Time
}

type driverState uint8

const (
	dsIdle driverState = iota
	dsRunning
	dsClosed
)

// Driver is the v1 DuplexDriver: a frame clock, a bounded input ring, a
// bounded output queue and per-session accounting wrapped around one
// DuplexTenant. It is transport-agnostic (no WebSocket/HTTP/JSON) and owns no
// model state.
//
// Threads: PushInput is the transport thread; NextEvent is the session
// thread; the tenant is stepped by a single internal engine goroutine started
// in Start and paced by the Clock. The engine consumes at most one input frame
// per tick and produces at most one output frame, so the clock paces the
// session, not the model; the output queue absorbs slack between the engine
// and the session thread. When the output queue is full the engine pauses the
// tenant step (backpressure) rather than dropping output.
type Driver struct {
	tenant DuplexTenant
	clock  *Clock

	mu          sync.Mutex
	state       driverState
	draining    bool
	termSet     bool
	term        Event
	pendingErr  *Error
	emitStats   bool
	statsSent   bool
	stopReason  string
	sessionID   string
	started     time.Time
	cfg         SessionConfig
	session     TenantSession
	sessionOnce sync.Once
	flushOnce   sync.Once

	in   *ring[inputEntry]
	out  *ring[Frame]
	text *ring[string]

	// engine-owned (single goroutine); no lock needed.
	nextIn        uint32
	emptyTicks    int
	overflowSince time.Time

	st *stats

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	notify chan struct{}
}

var _ DuplexDriver = (*Driver)(nil)

// NewDriver returns a Driver around t. A nil clock uses the real ticker-backed
// frame clock; tests inject a manual clock.
func NewDriver(t DuplexTenant, clk *Clock) *Driver {
	if clk == nil {
		clk = NewClock()
	}
	return &Driver{
		tenant: t,
		clock:  clk,
		in:     newRing[inputEntry](defaultInputRing),
		out:    newRing[Frame](defaultOutputQueue),
		text:   newRing[string](defaultTextQueue),
		st:     newStats(),
		notify: make(chan struct{}, 1),
	}
}

// Start validates the config, allocates a lane, starts the tenant and spawns
// the engine goroutine. It returns the negotiated SessionInfo; the session is
// ready (accepting input) when Start returns. Start is called once.
func (d *Driver) Start(ctx context.Context, cfg SessionConfig) (SessionInfo, error) {
	if err := d.validateConfig(cfg); err != nil {
		return SessionInfo{}, err
	}
	cfg = normalizeConfig(cfg)

	d.mu.Lock()
	if d.state != dsIdle {
		d.mu.Unlock()
		return SessionInfo{}, &Error{Code: ErrInternal, Message: "driver already started", Fatal: true}
	}
	sessionID := newSessionID()
	d.sessionID = sessionID
	d.started = d.clock.Now()
	d.cfg = cfg
	d.mu.Unlock()

	ts, err := d.tenant.Start(ctx, TenantConfig{
		SessionID:  sessionID,
		SampleRate: cfg.SampleRate,
		MaxMinutes: cfg.MaxMinutes,
		Text:       cfg.Text,
		Card:       cfg.Card,
		Reference:  cfg.Reference,
	})
	if err != nil {
		// A tenant's own typed error keeps its code (a remote engine's
		// session_limit or model_not_ready); anything else is unavailable.
		var te *Error
		if errors.As(err, &te) {
			return SessionInfo{}, te
		}
		return SessionInfo{}, &Error{Code: ErrTenantUnavailable, Message: err.Error(), Fatal: true}
	}
	if ts == nil {
		return SessionInfo{}, &Error{Code: ErrTenantUnavailable, Message: "tenant returned nil session", Fatal: true}
	}
	d.session = ts

	// The session lifetime is owned by the driver, not by the caller's Start
	// context: the transport cancels its start timeout once Start returns, and
	// that must not tear the session down. Cancel/Close are the lifecycle.
	d.ctx, d.cancel = context.WithCancel(context.Background())

	d.mu.Lock()
	d.state = dsRunning
	d.mu.Unlock()

	d.clock.Start()
	d.wg.Add(1)
	go d.run()

	return d.sessionInfo(), nil
}

// PushInput enqueues one frame for the engine. It is safe to call from the
// transport thread concurrently with NextEvent. The caller transfers ownership
// of the underlying slice: it must not reuse it after the call. Returns
// queue_full on a full ring and bad_frame on a wrong-length frame; never
// drops.
func (d *Driver) PushInput(_ context.Context, in Frame, seq uint32) error {
	if !in.Valid() {
		return &Error{Code: ErrBadFrame, Message: "invalid frame length", Fatal: true}
	}
	d.mu.Lock()
	accepting := d.state == dsRunning && !d.draining
	d.mu.Unlock()
	if !accepting {
		return &Error{Code: ErrBadRequest, Message: "session not accepting input", Fatal: true}
	}
	if !d.in.Push(inputEntry{frame: in, seq: seq, at: d.clock.Now()}) {
		return &Error{Code: ErrQueueFull, Message: "input queue full", Fatal: true}
	}
	return nil
}

// NextEvent returns the next session event: an audio frame, a text delta, a
// stats snapshot or the terminal done/error. It blocks until an event is
// available, the timeout elapses, or ctx is done. On timeout/ctx with no event
// it returns a zero Event and the context error (context.DeadlineExceeded when
// timeout fires), which the caller treats as "nothing yet".
func (d *Driver) NextEvent(ctx context.Context, timeout time.Duration) (Event, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		d.mu.Lock()
		if d.termSet {
			if d.emitStats && !d.statsSent {
				d.statsSent = true
				ev := Event{Kind: EventStats, Stats: d.st.snapshot(d.sessionID, d.clock.Now().Sub(d.started), d.in.Len(), d.out.Len())}
				d.mu.Unlock()
				return ev, nil
			}
			term := d.term
			d.mu.Unlock()
			return term, nil
		}
		draining := d.draining
		d.mu.Unlock()

		if f, ok := d.out.Pop(); ok {
			return Event{Kind: EventAudio, Audio: f}, nil
		}
		if txt, ok := d.text.Pop(); ok {
			return Event{Kind: EventText, Text: txt}, nil
		}

		if draining {
			d.enterTerminal()
			continue
		}

		select {
		case <-d.notify:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-deadline:
			return Event{}, context.DeadlineExceeded
		}
	}
}

// Cancel stops the session and releases the tenant. It is idempotent and safe
// to call from any thread. The final event is EventDone with the given reason.
func (d *Driver) Cancel(reason string) error {
	d.mu.Lock()
	if d.termSet {
		d.mu.Unlock()
		return nil
	}
	if d.state == dsIdle {
		d.mu.Unlock()
		return &Error{Code: ErrBadRequest, Message: "session not started", Fatal: true}
	}
	if d.stopReason == "" {
		d.stopReason = reason
	}
	d.mu.Unlock()
	d.stop()
	return nil
}

// Close releases the driver and its tenant. It is idempotent and safe to call
// from any thread. A graceful stop yields EventDone with reason "closed".
func (d *Driver) Close() error {
	d.mu.Lock()
	if d.termSet {
		d.mu.Unlock()
		return nil
	}
	if d.state == dsIdle {
		d.state = dsClosed
		d.termSet = true
		d.term = Event{Kind: EventDone, Reason: "closed"}
		d.stopReason = "closed"
		d.mu.Unlock()
		return nil
	}
	if d.stopReason == "" {
		d.stopReason = "closed"
	}
	d.mu.Unlock()
	d.stop()
	return nil
}

// Stats returns a live snapshot of the session's accounting.
func (d *Driver) Stats() Stats {
	d.mu.Lock()
	id := d.sessionID
	started := d.started
	d.mu.Unlock()
	var uptime time.Duration
	if !started.IsZero() {
		uptime = d.clock.Now().Sub(started)
	}
	return d.st.snapshot(id, uptime, d.in.Len(), d.out.Len())
}

// stop cancels the engine context and waits for it to drain and close the
// tenant session. Callers must not hold d.mu.
func (d *Driver) stop() {
	if d.cancel != nil {
		d.cancel()
	}
	d.wg.Wait()
}

// run is the engine goroutine: one boundary, one input frame, one tenant step,
// at most one output frame. It exits after the first drain.
func (d *Driver) run() {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			d.drain(d.stopReasonOrClosed(), nil)
			return
		case <-d.clock.C():
			d.processBoundary()
			for i := 0; i < maxCatchUp && !d.draining && d.in.Len() > catchUpBacklog && !d.out.Full(); i++ {
				d.processBoundary()
			}
			d.clock.Ack()
			if d.draining {
				return
			}
		}
	}
}

// processBoundary handles a single frame boundary: step the tenant or
// transition to a terminal state on error/time-limit.
func (d *Driver) processBoundary() {
	if err := d.step(); err != nil {
		if d.ctx.Err() != nil {
			d.drain(d.stopReasonOrClosed(), nil)
		} else {
			d.drain("", mapError(err))
		}
		return
	}
	if d.timeExpired() {
		d.drain("time_limit", nil)
	}
}

// step consumes one input frame, does sequence accounting, steps the tenant,
// and queues any output. A full output queue pauses the step (backpressure)
// and, after outputOverflowAfter, fails with output_overflow.
func (d *Driver) step() error {
	if d.out.Full() {
		now := d.clock.Now()
		if d.overflowSince.IsZero() {
			d.overflowSince = now
		} else if now.Sub(d.overflowSince) >= outputOverflowAfter {
			return &Error{Code: ErrOutputOverflow, Message: "output queue stalled", Fatal: true}
		}
		d.st.addLateOutput(1)
		return nil
	}
	d.overflowSince = time.Time{}

	entry, ok := d.in.Pop()
	if !ok {
		d.emptyTicks++
		if d.emptyTicks >= maxEmptyTicks {
			return &Error{Code: ErrTimeout, Message: "no input frames", Fatal: true}
		}
		return nil
	}
	d.emptyTicks = 0

	// Sequence accounting. A gap (seq jumped forward) is unrecoverable for a
	// stateful model: bad_sequence. A frame behind the watermark (duplicate or
	// out of order) is late: counted, still consumed, never skipped.
	if entry.seq < d.nextIn {
		d.st.addLateInput(1)
	} else if entry.seq > d.nextIn {
		return &Error{Code: ErrBadSequence, Message: "input sequence gap", Fatal: true}
	} else {
		d.nextIn = entry.seq + 1
	}

	start := d.clock.Now()
	tf, err := d.session.Step(d.ctx, entry.frame, entry.seq)
	d.st.addStep(d.clock.Now().Sub(start))
	if err != nil {
		return err
	}

	d.st.addInFrame(1)
	if len(tf.Audio) > 0 {
		if !Frame(tf.Audio).Valid() {
			return &Error{Code: ErrInternal, Message: "tenant returned invalid frame", Fatal: true}
		}
		if !d.out.Push(Frame(tf.Audio)) {
			return &Error{Code: ErrOutputOverflow, Message: "output queue full", Fatal: true}
		}
		d.st.addOutFrame(1)
	}
	if tf.Text != "" {
		if !d.text.Push(tf.Text) {
			d.st.addDropped(1)
		}
	}
	d.st.addE2E(d.clock.Now().Sub(entry.at))
	d.signal()
	return nil
}

// drain marks the session draining: stop accepting input and stepping, release
// the tenant, and let NextEvent flush the output queue before the terminal
// event. reason is used for a graceful done; pendingErr for a fatal error.
func (d *Driver) drain(reason string, pendingErr *Error) {
	// Give an optional Flusher tenant its held frame before the session is
	// marked draining, so NextEvent cannot enter the terminal state first.
	d.flushOnce.Do(d.flushSession)

	d.mu.Lock()
	if d.draining || d.termSet {
		d.mu.Unlock()
		return
	}
	d.draining = true
	if reason != "" {
		d.stopReason = reason
	}
	d.pendingErr = pendingErr
	d.mu.Unlock()

	d.closeSession()
	d.clock.Stop()
	d.signal()
}

// flushSession gives an optional Flusher tenant one chance to return a held
// frame while draining. The loopback's one-frame delay needs it; a model tenant
// that has no Flush is unchanged.
func (d *Driver) flushSession() {
	f, ok := d.session.(Flusher)
	if !ok {
		return
	}
	// d.ctx is already canceled on the Close/Cancel path; flushing is local
	// cleanup, so it must not inherit that cancellation.
	tf, err := f.Flush(context.Background())
	if err != nil {
		return
	}
	if len(tf.Audio) > 0 {
		if !Frame(tf.Audio).Valid() {
			d.st.addDropped(1)
			return
		}
		if d.out.Push(Frame(tf.Audio)) {
			d.st.addOutFrame(1)
		} else {
			d.st.addDropped(1)
		}
	}
	if tf.Text != "" && !d.text.Push(tf.Text) {
		d.st.addDropped(1)
	}
}

// enterTerminal flips a fully-drained session into its terminal state. Called
// only from NextEvent, holding no lock.
func (d *Driver) enterTerminal() {
	d.mu.Lock()
	if !d.termSet {
		d.termSet = true
		d.state = dsClosed
		if d.pendingErr != nil {
			d.term = Event{Kind: EventError, Error: d.pendingErr, Reason: d.pendingErr.Message}
			d.emitStats = false
		} else {
			reason := d.stopReason
			if reason == "" {
				reason = "closed"
			}
			d.term = Event{Kind: EventDone, Reason: reason}
			d.emitStats = true
		}
	}
	d.mu.Unlock()
}

func (d *Driver) closeSession() {
	d.sessionOnce.Do(func() {
		if d.session != nil {
			_ = d.session.Close()
		}
	})
}

func (d *Driver) stopReasonOrClosed() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopReason != "" {
		return d.stopReason
	}
	return "closed"
}

func (d *Driver) timeExpired() bool {
	if d.cfg.MaxMinutes <= 0 {
		return false
	}
	return d.clock.Now().Sub(d.started) >= time.Duration(d.cfg.MaxMinutes)*time.Minute
}

func (d *Driver) signal() {
	select {
	case d.notify <- struct{}{}:
	default:
	}
}

func (d *Driver) validateConfig(cfg SessionConfig) error {
	if cfg.SampleRate != 0 && cfg.SampleRate != SampleRate {
		return &Error{Code: ErrUnsupported, Message: "sample rate must be 24000", Fatal: true}
	}
	return nil
}

func (d *Driver) sessionInfo() SessionInfo {
	return SessionInfo{
		SessionID:    d.sessionID,
		Tenant:       d.cfg.Tenant,
		SampleRate:   d.cfg.SampleRate,
		FrameSamples: FrameSamples,
		FrameMillis:  float64(FrameDuration) / float64(time.Millisecond),
		MaxMinutes:   d.cfg.MaxMinutes,
		Capabilities: Capabilities{
			Text:       d.cfg.Text,
			InputRates: []int{SampleRate},
		},
	}
}

func normalizeConfig(cfg SessionConfig) SessionConfig {
	if cfg.SampleRate == 0 {
		cfg.SampleRate = SampleRate
	}
	if cfg.MaxMinutes <= 0 {
		cfg.MaxMinutes = defaultMaxMinutes
	}
	return cfg
}

func mapError(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Code: ErrInternal, Message: err.Error(), Fatal: true}
}

func newSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "dup_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "dup_" + hex.EncodeToString(b[:])
}
