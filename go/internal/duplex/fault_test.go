// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// Fault cases from plan.md §8 and workstreams.md DUP-042. Each one asserts the
// documented error code or event (protocol.md §5–§6) — never a panic, never a
// silent drop. They run against the fake driver's bounded ring/queue, which is
// the documented behaviour WS1 must reproduce; the note says so explicitly.

// §5: a frame that arrives after a tick found the ring empty is counted late
// and still consumed in order — not skipped, not synthesized.
func TestLateInputIsCountedAndStillDelivered(t *testing.T) {
	ctx := context.Background()
	d := newFakeDriver(t, NewZeroDelayLoopbackTenant(), fakeDriverConfig{})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// One empty tick: the client was late. Three are allowed before timeout.
	if err := d.Tick(); err != nil {
		t.Fatalf("empty Tick: %v", err)
	}
	in := rampFrame(0.3)
	// The sequence is still the next one expected: lateness is timing, not a gap.
	if err := d.PushInput(ctx, in, 0); err != nil {
		t.Fatalf("late PushInput: %v", err)
	}
	if err := d.Tick(); err != nil {
		t.Fatalf("Tick after late input: %v", err)
	}

	if st := d.Stats(); st.LateInputs != 1 {
		t.Fatalf("late inputs %d, want 1", st.LateInputs)
	}
	audio := eventsOfKind(collect(t, d), EventAudio)
	if len(audio) != 1 {
		t.Fatalf("audio events %d, want 1 (a late frame is still delivered)", len(audio))
	}
	requireSameBits(t, audio[0].Audio, in)
	if st := d.Stats(); st.InFrames != 1 || st.OutFrames != 1 || st.Dropped != 0 {
		t.Fatalf("stats in/out/dropped %d/%d/%d, want 1/1/0", st.InFrames, st.OutFrames, st.Dropped)
	}
}

// §5: the input ring is bounded and overflow is queue_full (fatal in v1). The
// frames already accepted are still presented before the error, so a burst
// cannot lose audio silently.
func TestBurstInputIsQueueFull(t *testing.T) {
	ctx := context.Background()
	d := newFakeDriver(t, NewZeroDelayLoopbackTenant(), fakeDriverConfig{InputCap: 4, OutputCap: 16})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Accept three, turn them into audio, then overrun the four-frame ring.
	for i := 0; i < 3; i++ {
		if err := d.PushInput(ctx, rampFrame(float32(i)), uint32(i)); err != nil {
			t.Fatalf("PushInput %d: %v", i, err)
		}
		if err := d.Tick(); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}
	var burstErr error
	for seq := 3; seq < 12; seq++ {
		if err := d.PushInput(ctx, rampFrame(0), uint32(seq)); err != nil {
			burstErr = err
			break
		}
	}
	requireCode(t, burstErr, ErrQueueFull)

	evs := collect(t, d)
	audio := eventsOfKind(evs, EventAudio)
	if len(audio) != 3 {
		t.Fatalf("audio events %d, want the 3 already produced", len(audio))
	}
	errs := eventsOfKind(evs, EventError)
	if len(errs) != 1 || errs[0].Error.Code != ErrQueueFull || !errs[0].Error.Fatal {
		t.Fatalf("error events %+v, want one fatal queue_full", errs)
	}
	// The queue_full error is last: nothing was appended after it.
	if evs[len(evs)-1].Kind != EventError {
		t.Fatalf("last event %v, want the queue_full error", evs[len(evs)-1].Kind)
	}
	// Further input is refused with the same terminal error, not a new one.
	requireCode(t, d.PushInput(ctx, rampFrame(0), 12), ErrQueueFull)
}

// §5: a full output queue pauses the tenant and records output_overflow; only
// 2 s of no drain is fatal. The queue is never trimmed to make room.
func TestOutputOverflowBackpressureThenFatal(t *testing.T) {
	ctx := context.Background()
	d := newFakeDriver(t, NewZeroDelayLoopbackTenant(), fakeDriverConfig{OutputCap: 2})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 6; i++ {
		if err := d.PushInput(ctx, rampFrame(float32(i)), uint32(i)); err != nil {
			t.Fatalf("PushInput %d: %v", i, err)
		}
	}
	// Fill the two-frame output queue; the third tick must pause, not drop.
	if err := d.Tick(); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if err := d.Tick(); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if err := d.Tick(); err != nil {
		t.Fatalf("paused Tick: %v", err)
	}
	st := d.Stats()
	if st.InFrames != 2 || st.OutFrames != 2 {
		t.Fatalf("in/out %d/%d, want the tenant paused at 2/2", st.InFrames, st.OutFrames)
	}
	if st.QueueOut != 2 {
		t.Fatalf("queue out %d, want 2 (no drop to make room)", st.QueueOut)
	}
	// A single tick after 2 s of a full queue is the fatal form.
	d.clock.Advance(2 * time.Second)
	requireCode(t, d.Tick(), ErrOutputOverflow)

	evs := collect(t, d)
	overflow := 0
	for _, ev := range evs {
		if ev.Kind == EventError && ev.Error.Code == ErrOutputOverflow {
			if overflow == 0 && ev.Error.Fatal {
				t.Fatal("first output_overflow was fatal; the warning must come first")
			}
			overflow++
		}
	}
	if overflow != 2 {
		t.Fatalf("output_overflow events %d, want a warning then a fatal", overflow)
	}
	audio := eventsOfKind(evs, EventAudio)
	if len(audio) != 2 {
		t.Fatalf("audio events %d, want the 2 queued frames still present", len(audio))
	}
	for i, ev := range audio {
		requireSameBits(t, ev.Audio, rampFrame(float32(i)))
	}
}

// A tenant slower than the frame period is a latency problem, not an error: no
// frame may be dropped or fabricated, and the cost must appear in the stats.
func TestSlowTenantIsCountedNotDropped(t *testing.T) {
	ctx := context.Background()
	const cost = 200 * time.Millisecond // 1.5x the 133.33 ms frame period
	d := newFakeDriver(t, NewZeroDelayLoopbackTenant(), fakeDriverConfig{
		OutputCap: 16,
		StepDelay: func(uint32) time.Duration { return cost },
	})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	in := make([]Frame, 5)
	for i := range in {
		in[i] = rampFrame(float32(i) / 10)
		if err := d.PushInput(ctx, in[i], uint32(i)); err != nil {
			t.Fatalf("PushInput %d: %v", i, err)
		}
		if err := d.Tick(); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}
	evs := collect(t, d)
	for _, ev := range evs {
		if ev.Kind == EventError {
			t.Fatalf("slow tenant produced %s; slowness is not a session error", ev.Error)
		}
	}
	audio := eventsOfKind(evs, EventAudio)
	if len(audio) != len(in) {
		t.Fatalf("audio events %d, want %d", len(audio), len(in))
	}
	for i := range in {
		requireSameBits(t, audio[i].Audio, in[i])
	}
	st := d.Stats()
	if st.StepP50MS != 200 || st.StepP95MS != 200 || st.StepMaxMS != 200 {
		t.Fatalf("step p50/p95/max %.1f/%.1f/%.1f, want 200 each", st.StepP50MS, st.StepP95MS, st.StepMaxMS)
	}
	if st.Dropped != 0 {
		t.Fatalf("dropped %d, want 0", st.Dropped)
	}
}

// §5/§4: more than three empty ticks is timeout, fatal, with the documented
// code — not an unbounded wait.
func TestTimeoutAfterEmptyTicks(t *testing.T) {
	ctx := context.Background()
	d := newFakeDriver(t, NewLoopbackTenant(), fakeDriverConfig{})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := d.Tick(); err != nil {
			t.Fatalf("empty Tick %d: %v, want the three allowed empty ticks", i, err)
		}
	}
	requireCode(t, d.Tick(), ErrTimeout)
	errs := eventsOfKind(collect(t, d), EventError)
	if len(errs) != 1 || errs[0].Error.Code != ErrTimeout || !errs[0].Error.Fatal {
		t.Fatalf("error events %+v, want one fatal timeout", errs)
	}
	// Timeout is terminal: no audio after it, and input is refused.
	requireCode(t, d.PushInput(ctx, rampFrame(0), 0), ErrTimeout)
}

// §7.1: cancel is idempotent, releases the lane immediately, and ends with
// final stats then session.done carrying the reason.
func TestCancelIsIdempotentAndReleasesLane(t *testing.T) {
	ctx := context.Background()
	spy := &spyTenant{inner: NewZeroDelayLoopbackTenant()}
	d := newFakeDriver(t, spy, fakeDriverConfig{})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.PushInput(ctx, rampFrame(0.4), 0); err != nil {
		t.Fatalf("PushInput: %v", err)
	}
	if err := d.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := d.Cancel("user_hangup"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := d.Cancel("again"); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if got := spy.sessions[0].closes; got != 1 {
		t.Fatalf("lane closed %d times, want exactly 1", got)
	}
	requireCode(t, d.PushInput(ctx, rampFrame(0), 1), ErrCancelled)

	evs := collect(t, d)
	if len(evs) < 3 {
		t.Fatalf("events %+v, want audio, stats, done", evs)
	}
	// Audio already produced stays ahead of the terminal pair (no silent loss).
	if evs[0].Kind != EventAudio {
		t.Fatalf("first event %v, want the queued audio", evs[0].Kind)
	}
	if evs[len(evs)-2].Kind != EventStats || evs[len(evs)-1].Kind != EventDone {
		t.Fatalf("last events %v/%v, want stats then done", evs[len(evs)-2].Kind, evs[len(evs)-1].Kind)
	}
	if evs[len(evs)-1].Reason != "user_hangup" {
		t.Fatalf("done reason %q, want user_hangup", evs[len(evs)-1].Reason)
	}
	// Terminal after done: further reads are EOF, not a repeated done.
	if _, err := d.NextEvent(ctx, FrameDuration); !errors.Is(err, io.EOF) {
		t.Fatalf("NextEvent after done: %v, want io.EOF", err)
	}
}

// Close is idempotent and terminal. It must not call the process-scoped tenant
// Close per lane: that would tear down other lanes' model state.
func TestCloseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	spy := &spyTenant{inner: NewLoopbackTenant()}
	d := newFakeDriver(t, spy, fakeDriverConfig{})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if spy.sessions[0].closes != 1 {
		t.Fatalf("lane closed %d times, want 1", spy.sessions[0].closes)
	}
	if spy.closes != 0 {
		t.Fatalf("tenant.Close called %d times, want 0 (process-scoped)", spy.closes)
	}
	requireCode(t, d.PushInput(ctx, rampFrame(0), 0), ErrCancelled)
	requireCode(t, d.Tick(), ErrCancelled)

	evs := collect(t, d)
	if len(evs) < 2 || evs[len(evs)-2].Kind != EventStats || evs[len(evs)-1].Kind != EventDone {
		t.Fatalf("events %+v, want a final stats then done", evs)
	}
	if evs[len(evs)-1].Reason != "closed" {
		t.Fatalf("done reason %q, want closed", evs[len(evs)-1].Reason)
	}
}

// A failing tenant Step is internal (fatal), with an error event — not a panic
// and not a silently empty frame.
func TestTenantStepErrorIsMapped(t *testing.T) {
	ctx := context.Background()
	spy := &spyTenant{inner: NewZeroDelayLoopbackTenant(), stepErr: errors.New("codec state corrupted")}
	d := newFakeDriver(t, spy, fakeDriverConfig{})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.PushInput(ctx, rampFrame(0), 0); err != nil {
		t.Fatalf("PushInput: %v", err)
	}
	requireCode(t, d.Tick(), ErrInternal)
	errs := eventsOfKind(collect(t, d), EventError)
	if len(errs) != 1 || errs[0].Error.Code != ErrInternal || !errs[0].Error.Fatal {
		t.Fatalf("error events %+v, want one fatal internal", errs)
	}
	if got := spy.sessions[0].closes; got != 1 {
		t.Fatalf("lane closed %d times after step error, want 1 (released immediately)", got)
	}
}
