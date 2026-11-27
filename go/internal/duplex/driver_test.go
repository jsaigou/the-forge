// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeTenant is an in-test DuplexTenant that echoes audio, emits a fixed text
// delta, fails on demand, or blocks on demand — enough to prove the driver
// without the loopback package (WS4).
type fakeTenant struct {
	mu      sync.Mutex
	echo    bool
	text    string
	stepErr error
	// stepBlock, when non-nil, makes Step wait until it is closed (or ctx is
	// done) — used to simulate a slow tenant.
	stepBlock chan struct{}

	sessions    []*fakeSession
	startedN    int
	tenantClose int
}

type fakeSession struct {
	mu     sync.Mutex
	parent *fakeTenant
	steps  int
	seqs   []uint32
	closed bool
}

func (t *fakeTenant) Load(context.Context) (TenantInfo, error) {
	return TenantInfo{Name: "fake"}, nil
}

func (t *fakeTenant) Start(context.Context, TenantConfig) (TenantSession, error) {
	t.mu.Lock()
	t.startedN++
	s := &fakeSession{parent: t}
	t.sessions = append(t.sessions, s)
	t.mu.Unlock()
	return s, nil
}

func (t *fakeTenant) Close() error {
	t.mu.Lock()
	t.tenantClose++
	t.mu.Unlock()
	return nil
}

func (s *fakeSession) Step(ctx context.Context, in Frame, seq uint32) (TenantFrame, error) {
	s.mu.Lock()
	s.steps++
	s.seqs = append(s.seqs, seq)
	s.mu.Unlock()

	t := s.parent
	t.mu.Lock()
	echo := t.echo
	text := t.text
	stepErr := t.stepErr
	block := t.stepBlock
	t.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return TenantFrame{}, ctx.Err()
		}
	}
	if stepErr != nil {
		return TenantFrame{}, stepErr
	}
	if echo {
		return TenantFrame{Audio: []float32(in)}, nil
	}
	return TenantFrame{Text: text}, nil
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func testFrame(v float32) Frame {
	f := make(Frame, FrameSamples)
	for i := range f {
		f[i] = v
	}
	return f
}

func newTestDriver(t *testing.T, tnt *fakeTenant) (*Driver, *Clock) {
	t.Helper()
	clk := NewManualClock(time.Now())
	d := NewDriver(tnt, clk)
	t.Cleanup(func() { _ = d.Close() })
	return d, clk
}

func startTest(t *testing.T, d *Driver) SessionInfo {
	t.Helper()
	info, err := d.Start(context.Background(), SessionConfig{Tenant: "fake", Text: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return info
}

// terminalEvent drains events until a terminal one (done/error) arrives.
func terminalEvent(t *testing.T, d *Driver) Event {
	t.Helper()
	for i := 0; i < 32; i++ {
		ev, err := d.NextEvent(context.Background(), 2*time.Second)
		if err != nil {
			t.Fatalf("NextEvent: %v", err)
		}
		if ev.Kind == EventDone || ev.Kind == EventError {
			return ev
		}
	}
	t.Fatal("no terminal event within 32 events")
	return Event{}
}

func wantCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", code)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *Error, got %T (%v)", err, err)
	}
	if e.Code != code {
		t.Fatalf("expected code %q, got %q", code, e.Code)
	}
}

func TestRingWrapAndFull(t *testing.T) {
	r := newRing[int](4)
	for i := 0; i < 4; i++ {
		if !r.Push(i) {
			t.Fatalf("Push(%d) failed", i)
		}
	}
	if !r.Full() {
		t.Fatal("ring should be full")
	}
	if r.Push(99) {
		t.Fatal("Push on full ring should fail")
	}
	if r.Len() != 4 || r.Cap() != 4 {
		t.Fatalf("len/cap = %d/%d, want 4/4", r.Len(), r.Cap())
	}
	// Drain two, then wrap writes around the end.
	if v, ok := r.Pop(); !ok || v != 0 {
		t.Fatalf("Pop = %d,%v want 0,true", v, ok)
	}
	if v, ok := r.Pop(); !ok || v != 1 {
		t.Fatalf("Pop = %d,%v want 1,true", v, ok)
	}
	if !r.Push(4) || !r.Push(5) {
		t.Fatal("Push after wrap failed")
	}
	for i, want := range []int{2, 3, 4, 5} {
		if v, ok := r.Pop(); !ok || v != want {
			t.Fatalf("Pop %d = %d,%v want %d,true", i, v, ok, want)
		}
	}
	if !r.Empty() {
		t.Fatal("ring should be empty")
	}
}

func TestRingPopEmpty(t *testing.T) {
	r := newRing[int](2)
	if v, ok := r.Pop(); ok || v != 0 {
		t.Fatalf("Pop empty = %d,%v want 0,false", v, ok)
	}
}

func TestDriverEchoRoundTrip(t *testing.T) {
	tnt := &fakeTenant{echo: true}
	d, clk := newTestDriver(t, tnt)
	info := startTest(t, d)

	if info.SampleRate != SampleRate || info.FrameSamples != FrameSamples {
		t.Fatalf("info = %+v", info)
	}
	if info.SessionID == "" {
		t.Fatal("empty session id")
	}

	in := testFrame(0.25)
	if err := d.PushInput(context.Background(), in, 0); err != nil {
		t.Fatalf("PushInput: %v", err)
	}
	clk.Step()

	ev, err := d.NextEvent(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev.Kind != EventAudio {
		t.Fatalf("event kind = %v, want EventAudio", ev.Kind)
	}
	if !reflect.DeepEqual(ev.Audio, in) {
		t.Fatal("echo audio != input audio")
	}

	st := d.Stats()
	if st.InFrames != 1 || st.OutFrames != 1 {
		t.Fatalf("in/out frames = %d/%d, want 1/1", st.InFrames, st.OutFrames)
	}
}

func TestPushBeforeStart(t *testing.T) {
	d, _ := newTestDriver(t, &fakeTenant{})
	wantCode(t, d.PushInput(context.Background(), testFrame(0), 0), ErrBadRequest)
}

func TestPushBadFrame(t *testing.T) {
	d, _ := newTestDriver(t, &fakeTenant{})
	startTest(t, d)
	wantCode(t, d.PushInput(context.Background(), Frame{0.5}, 0), ErrBadFrame)
}

func TestInputQueueFull(t *testing.T) {
	d, _ := newTestDriver(t, &fakeTenant{echo: true})
	startTest(t, d)
	for i := 0; i < defaultInputRing; i++ {
		if err := d.PushInput(context.Background(), testFrame(float32(i)), uint32(i)); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	wantCode(t, d.PushInput(context.Background(), testFrame(99), 99), ErrQueueFull)
}

func TestSequenceGap(t *testing.T) {
	d, clk := newTestDriver(t, &fakeTenant{echo: true})
	startTest(t, d)
	if err := d.PushInput(context.Background(), testFrame(0), 0); err != nil {
		t.Fatal(err)
	}
	clk.Step()
	// seq 2 with seq 1 missing is a gap.
	if err := d.PushInput(context.Background(), testFrame(2), 2); err != nil {
		t.Fatal(err)
	}
	clk.Step()

	ev := terminalEvent(t, d)
	if ev.Kind != EventError {
		t.Fatalf("terminal = %v, want EventError", ev.Kind)
	}
	if ev.Error.Code != ErrBadSequence {
		t.Fatalf("code = %q, want bad_sequence", ev.Error.Code)
	}
}

func TestLateInput(t *testing.T) {
	d, clk := newTestDriver(t, &fakeTenant{echo: true})
	startTest(t, d)
	for seq := uint32(0); seq < 2; seq++ {
		if err := d.PushInput(context.Background(), testFrame(float32(seq)), seq); err != nil {
			t.Fatal(err)
		}
		clk.Step()
	}
	// A duplicate of seq 1 arrives behind the watermark: late, not skipped.
	if err := d.PushInput(context.Background(), testFrame(0.5), 1); err != nil {
		t.Fatal(err)
	}
	clk.Step()

	st := d.Stats()
	if st.LateInputs != 1 {
		t.Fatalf("LateInputs = %d, want 1", st.LateInputs)
	}
	if st.InFrames != 3 {
		t.Fatalf("InFrames = %d, want 3 (late frame still consumed)", st.InFrames)
	}
}

func TestIdleTimeout(t *testing.T) {
	d, clk := newTestDriver(t, &fakeTenant{echo: true})
	startTest(t, d)
	for i := 0; i < maxEmptyTicks; i++ {
		clk.Step()
	}
	ev := terminalEvent(t, d)
	if ev.Kind != EventError || ev.Error.Code != ErrTimeout {
		t.Fatalf("terminal = %+v, want timeout error", ev)
	}
}

func TestOutputOverflow(t *testing.T) {
	d, clk := newTestDriver(t, &fakeTenant{echo: true})
	startTest(t, d)
	// Fill the output queue (4 echo frames) without draining.
	for i := 0; i < defaultOutputQueue; i++ {
		if err := d.PushInput(context.Background(), testFrame(float32(i)), uint32(i)); err != nil {
			t.Fatal(err)
		}
		clk.Step()
	}
	clk.Step() // output full: backpressure begins
	clk.Advance(3 * time.Second)
	clk.Step() // still full past the 2s budget -> output_overflow

	ev := terminalEvent(t, d)
	if ev.Kind != EventError || ev.Error.Code != ErrOutputOverflow {
		t.Fatalf("terminal = %+v, want output_overflow error", ev)
	}
}

func TestCancelIdempotent(t *testing.T) {
	tnt := &fakeTenant{echo: true}
	d, _ := newTestDriver(t, tnt)
	startTest(t, d)

	if err := d.Cancel("user_hangup"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := d.Cancel("again"); err != nil {
		t.Fatalf("Cancel (second): %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close after Cancel: %v", err)
	}

	ev := terminalEvent(t, d)
	if ev.Kind != EventDone || ev.Reason != "user_hangup" {
		t.Fatalf("terminal = %+v, want done/user_hangup", ev)
	}
	if len(tnt.sessions) != 1 || !tnt.sessions[0].closed {
		t.Fatal("tenant session was not released")
	}
}

func TestCloseIdempotent(t *testing.T) {
	tnt := &fakeTenant{echo: true}
	d, _ := newTestDriver(t, tnt)
	startTest(t, d)

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close (second): %v", err)
	}
	ev := terminalEvent(t, d)
	if ev.Kind != EventDone {
		t.Fatalf("terminal = %+v, want done", ev)
	}
}

func TestTwoLanesIsolated(t *testing.T) {
	tnt := &fakeTenant{echo: true}
	d1, c1 := newTestDriver(t, tnt)
	d2, c2 := newTestDriver(t, tnt)
	startTest(t, d1)
	startTest(t, d2)

	if err := d1.PushInput(context.Background(), testFrame(1), 0); err != nil {
		t.Fatal(err)
	}
	c1.Step()
	if err := d2.PushInput(context.Background(), testFrame(2), 0); err != nil {
		t.Fatal(err)
	}
	c2.Step()

	ev1, err := d1.NextEvent(context.Background(), time.Second)
	if err != nil || ev1.Kind != EventAudio || !reflect.DeepEqual(ev1.Audio, testFrame(1)) {
		t.Fatalf("lane1 event = %+v, %v", ev1, err)
	}
	ev2, err := d2.NextEvent(context.Background(), time.Second)
	if err != nil || ev2.Kind != EventAudio || !reflect.DeepEqual(ev2.Audio, testFrame(2)) {
		t.Fatalf("lane2 event = %+v, %v", ev2, err)
	}

	if err := d1.Cancel("test"); err != nil {
		t.Fatal(err)
	}

	// Lane 2 must be unaffected by lane 1's cancel.
	if err := d2.PushInput(context.Background(), testFrame(3), 1); err != nil {
		t.Fatalf("lane2 push after lane1 cancel: %v", err)
	}
	c2.Step()
	ev2, err = d2.NextEvent(context.Background(), time.Second)
	if err != nil || ev2.Kind != EventAudio || !reflect.DeepEqual(ev2.Audio, testFrame(3)) {
		t.Fatalf("lane2 event after lane1 cancel = %+v, %v", ev2, err)
	}
}

func TestTimeLimit(t *testing.T) {
	d, clk := newTestDriver(t, &fakeTenant{echo: true})
	if _, err := d.Start(context.Background(), SessionConfig{Tenant: "fake", MaxMinutes: 1}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.PushInput(context.Background(), testFrame(0), 0); err != nil {
		t.Fatal(err)
	}
	clk.Step()
	clk.Advance(2 * time.Minute)
	clk.Step()

	ev := terminalEvent(t, d)
	if ev.Kind != EventDone || ev.Reason != "time_limit" {
		t.Fatalf("terminal = %+v, want done/time_limit", ev)
	}
}

func TestTextEvent(t *testing.T) {
	d, clk := newTestDriver(t, &fakeTenant{text: "hello"})
	startTest(t, d)
	if err := d.PushInput(context.Background(), testFrame(0), 0); err != nil {
		t.Fatal(err)
	}
	clk.Step()
	ev, err := d.NextEvent(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EventText || ev.Text != "hello" {
		t.Fatalf("event = %+v, want EventText/hello", ev)
	}
}

func TestTenantStepError(t *testing.T) {
	tnt := &fakeTenant{stepErr: errors.New("boom")}
	d, clk := newTestDriver(t, tnt)
	startTest(t, d)
	if err := d.PushInput(context.Background(), testFrame(0), 0); err != nil {
		t.Fatal(err)
	}
	clk.Step()
	ev := terminalEvent(t, d)
	if ev.Kind != EventError || ev.Error.Code != ErrInternal {
		t.Fatalf("terminal = %+v, want internal error", ev)
	}
}

func TestUnsupportedSampleRate(t *testing.T) {
	d, _ := newTestDriver(t, &fakeTenant{})
	_, err := d.Start(context.Background(), SessionConfig{Tenant: "fake", SampleRate: 16000})
	wantCode(t, err, ErrUnsupported)
}

func TestNextEventTimeout(t *testing.T) {
	d, _ := newTestDriver(t, &fakeTenant{echo: true})
	startTest(t, d)
	// No input, no output: NextEvent should return DeadlineExceeded.
	_, err := d.NextEvent(context.Background(), 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NextEvent timeout err = %v, want DeadlineExceeded", err)
	}
}

// errTenant fails Start with a configured error.
type errTenant struct{ err error }

func (e errTenant) Load(context.Context) (TenantInfo, error)                   { return TenantInfo{}, nil }
func (e errTenant) Start(context.Context, TenantConfig) (TenantSession, error) { return nil, e.err }
func (e errTenant) Close() error                                               { return nil }

// A tenant that reports its own typed error (a remote engine saying
// session_limit or model_not_ready) must reach the caller with that code; only
// untyped errors become tenant_unavailable. protocol.md §6 gives the codes
// different retry meaning, so flattening them hides whether to retry.
func TestDriverStartKeepsATenantsTypedError(t *testing.T) {
	d := NewDriver(errTenant{&Error{Code: ErrSessionLimit, Message: "no free lane", Fatal: true}}, nil)
	_, err := d.Start(context.Background(), SessionConfig{})
	requireCode(t, err, ErrSessionLimit)

	d = NewDriver(errTenant{errors.New("boom")}, nil)
	_, err = d.Start(context.Background(), SessionConfig{})
	requireCode(t, err, ErrTenantUnavailable)
}

func TestBacklogCatchesUp(t *testing.T) {
	d, clk := newTestDriver(t, &fakeTenant{echo: true})
	startTest(t, d)
	for i := 0; i < 4; i++ { // a burst after a stall: four frames queued at once
		if err := d.PushInput(context.Background(), testFrame(0.1), uint32(i)); err != nil {
			t.Fatal(err)
		}
	}
	clk.Step() // one frame period
	if got := d.Stats().InFrames; got < 2 {
		t.Fatalf("InFrames = %d after one period with 4 queued, want the backlog to be worked down (>= 2)", got)
	}
	if got := d.Stats().InFrames; got > uint64(1+maxCatchUp) {
		t.Fatalf("InFrames = %d, catch-up must be bounded to %d per period", got, 1+maxCatchUp)
	}
}

func TestInputRingAbsorbsAMultiSecondBurst(t *testing.T) {
	d, _ := newTestDriver(t, &fakeTenant{echo: true})
	startTest(t, d)
	// A network stall of about 2.5 s delivers its whole backlog at once (20 frames at 7.5 per second).
	for i := 0; i < 20; i++ {
		if err := d.PushInput(context.Background(), testFrame(0.1), uint32(i)); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
}
