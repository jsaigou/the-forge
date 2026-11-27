// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"context"
	"errors"
	"testing"
)

// Protocol §10 cases that do not need an HTTP server or a WebSocket. Cases
// that are purely wire-level (kind byte, JSON control messages, auth) stay in
// WS2; docs/duplex/notes/ws4-conformance.md lists them as follow-ups.
//
// Everything here runs against the frozen contract in contract.go plus the
// test-only fakeDriver. When WS1's driver exists, swap newFakeDriver for it and
// keep the assertions: that is the point of a conformance suite.

// §10.1 (sample-rate half): 24 kHz is the only supported rate, and the ready
// info must describe the session the client actually gets.
func TestSessionStartNegotiation(t *testing.T) {
	ctx := context.Background()

	for _, rate := range []int{0, SampleRate} {
		d := newFakeDriver(t, NewLoopbackTenant(), fakeDriverConfig{})
		info, err := d.Start(ctx, SessionConfig{SampleRate: rate, Text: true})
		if err != nil {
			t.Fatalf("Start(rate=%d): %v", rate, err)
		}
		if info.SessionID != "dup_test" || info.Tenant != "loopback" {
			t.Fatalf("info %+v, want session id dup_test and tenant loopback", info)
		}
		if info.SampleRate != SampleRate || info.FrameSamples != FrameSamples {
			t.Fatalf("info rates %d/%d, want %d/%d", info.SampleRate, info.FrameSamples, SampleRate, FrameSamples)
		}
		if info.FrameMillis < 133.333 || info.FrameMillis > 133.334 {
			t.Fatalf("frame millis %.6f, want 133.333", info.FrameMillis)
		}
		if info.MaxMinutes != 30 {
			t.Fatalf("max minutes %d, want the 30 default", info.MaxMinutes)
		}
		if info.Capabilities.Text {
			t.Fatal("ready promised text; the loopback has none (§7.3)")
		}
		if len(info.Capabilities.InputRates) != 1 || info.Capabilities.InputRates[0] != SampleRate {
			t.Fatalf("input rates %v, want [%d]", info.Capabilities.InputRates, SampleRate)
		}
	}

	for _, rate := range []int{16000, 48000} {
		d := newFakeDriver(t, NewLoopbackTenant(), fakeDriverConfig{})
		_, err := d.Start(ctx, SessionConfig{SampleRate: rate})
		requireCode(t, err, ErrBadRequest)
	}
}

// §10.3 (first half): session.ready is only possible after the tenant's Start
// succeeds. The driver must not report a session it cannot serve.
func TestReadyOnlyAfterTenantStart(t *testing.T) {
	spy := &spyTenant{inner: NewLoopbackTenant(), startErr: errors.New("reference audio unreadable")}
	d := newFakeDriver(t, spy, fakeDriverConfig{})

	info, err := d.Start(context.Background(), SessionConfig{SampleRate: SampleRate})
	if err == nil {
		t.Fatalf("Start succeeded with info %+v, want tenant failure", info)
	}
	// A generic tenant failure is tenant_unavailable (§7.2).
	requireCode(t, err, ErrTenantUnavailable)
	if spy.starts != 1 {
		t.Fatalf("tenant Start called %d times, want 1", spy.starts)
	}
	if len(spy.sessions) != 0 {
		t.Fatal("a lane was reported for a failed start")
	}
	// No audio is accepted before ready (§4).
	requireCode(t, d.PushInput(context.Background(), rampFrame(0), 0), ErrBadRequest)
	if evs := collect(t, d); len(evs) != 0 {
		t.Fatalf("failed start emitted events: %+v", evs)
	}
}

// §10.2 (frame half): wrong length is bad_frame, a gap or repeat is
// bad_sequence, and both are fatal instead of being silently repaired.
func TestFrameLengthAndSequenceValidation(t *testing.T) {
	t.Run("bad_frame", func(t *testing.T) {
		d := newFakeDriver(t, NewLoopbackTenant(), fakeDriverConfig{})
		if _, err := d.Start(context.Background(), SessionConfig{SampleRate: SampleRate}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		requireCode(t, d.PushInput(context.Background(), make(Frame, FrameSamples-1), 0), ErrBadFrame)
	})
	t.Run("gap", func(t *testing.T) {
		d := newFakeDriver(t, NewLoopbackTenant(), fakeDriverConfig{})
		if _, err := d.Start(context.Background(), SessionConfig{SampleRate: SampleRate}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		requireCode(t, d.PushInput(context.Background(), rampFrame(0), 1), ErrBadSequence)
	})
	t.Run("duplicate", func(t *testing.T) {
		d := newFakeDriver(t, NewLoopbackTenant(), fakeDriverConfig{})
		if _, err := d.Start(context.Background(), SessionConfig{SampleRate: SampleRate}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := d.PushInput(context.Background(), rampFrame(0), 0); err != nil {
			t.Fatalf("first PushInput: %v", err)
		}
		requireCode(t, d.PushInput(context.Background(), rampFrame(0), 0), ErrBadSequence)
	})
}

// §10.5: loopback echo is bit-exact, text is empty, and the outbound sequence
// mirrors the client's. Zero-delay mode is the oracle; the delayed mode is
// checked separately below with its documented offset.
func TestLoopbackEchoThroughDriverIsBitExact(t *testing.T) {
	ctx := context.Background()
	d := newFakeDriver(t, NewZeroDelayLoopbackTenant(), fakeDriverConfig{OutputCap: 16})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	in := []Frame{adversarialFrame(), rampFrame(0.1), rampFrame(-0.4), rampFrame(0.9), rampFrame(0)}
	for i, f := range in {
		if err := d.PushInput(ctx, f, uint32(i)); err != nil {
			t.Fatalf("PushInput %d: %v", i, err)
		}
		if err := d.Tick(); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}

	evs := collect(t, d)
	audio := eventsOfKind(evs, EventAudio)
	if len(audio) != len(in) {
		t.Fatalf("got %d audio events, want %d", len(audio), len(in))
	}
	for i := range in {
		requireSameBits(t, audio[i].Audio, in[i])
	}
	if text := eventsOfKind(evs, EventText); len(text) != 0 {
		t.Fatalf("loopback emitted %d text events, want 0", len(text))
	}
	for _, ev := range evs {
		if ev.Kind == EventError {
			t.Fatalf("unexpected error event: %s", ev.Error)
		}
	}
	st := d.Stats()
	if st.InFrames != uint64(len(in)) || st.OutFrames != uint64(len(in)) {
		t.Fatalf("frames in/out %d/%d, want %d/%d", st.InFrames, st.OutFrames, len(in), len(in))
	}
	if st.Dropped != 0 || st.LateInputs != 0 {
		t.Fatalf("stats dropped=%d late=%d, want 0/0", st.Dropped, st.LateInputs)
	}
	for i, seq := range d.outSeqs {
		if seq != uint32(i) {
			t.Fatalf("outbound seqs %v, want 0..%d", d.outSeqs, len(in)-1)
		}
	}
	// Zero-delay mode has nothing pending: Drain must not fabricate a frame.
	if err := d.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if evs := collect(t, d); len(evs) != 0 {
		t.Fatalf("zero-delay Drain emitted %d events, want 0", len(evs))
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// §10.5/§7.3: the delay is exactly one frame, and the drain step means no
// input frame is lost. The count is the assertion: N in, N out.
func TestLoopbackOneFrameDelayThroughDriver(t *testing.T) {
	ctx := context.Background()
	d := newFakeDriver(t, NewLoopbackTenant(), fakeDriverConfig{OutputCap: 16})
	if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	in := []Frame{rampFrame(0.1), rampFrame(0.2), rampFrame(0.3), rampFrame(0.4), adversarialFrame()}
	for i, f := range in {
		if err := d.PushInput(ctx, f, uint32(i)); err != nil {
			t.Fatalf("PushInput %d: %v", i, err)
		}
		if err := d.Tick(); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}
	// Before the drain, exactly one frame is still held by the tenant.
	if got, want := d.Stats().OutFrames, uint64(len(in)-1); got != want {
		t.Fatalf("outframes before drain %d, want %d (one-frame delay)", got, want)
	}
	if err := d.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := d.Drain(); err != nil { // a second drain must not emit the zero flush frame
		t.Fatalf("second Drain: %v", err)
	}

	evs := collect(t, d)
	audio := eventsOfKind(evs, EventAudio)
	if len(audio) != len(in) {
		t.Fatalf("got %d audio events, want %d (drain must flush the tail)", len(audio), len(in))
	}
	for i := range in {
		requireSameBits(t, audio[i].Audio, in[i])
	}
	if text := eventsOfKind(evs, EventText); len(text) != 0 {
		t.Fatalf("loopback emitted %d text events, want 0", len(text))
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st := d.Stats(); st.InFrames != uint64(len(in)) {
		t.Fatalf("inframes %d, want %d (drain must not count as input)", st.InFrames, len(in))
	}
}

// §10.6: two lanes share no state. Cancelling one leaves the other running,
// with its own session id, counters and audio.
func TestTwoLanesAreIsolated(t *testing.T) {
	ctx := context.Background()
	tenant := NewZeroDelayLoopbackTenant() // deliberately shared, as the service would
	a := newFakeDriver(t, tenant, fakeDriverConfig{SessionID: "dup_a"})
	b := newFakeDriver(t, tenant, fakeDriverConfig{SessionID: "dup_b"})
	for _, d := range []*fakeDriver{a, b} {
		if _, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate}); err != nil {
			t.Fatalf("%s Start: %v", d.cfg.SessionID, err)
		}
	}

	fa, fb := rampFrame(0.2), rampFrame(-0.8)
	if err := a.PushInput(ctx, fa, 0); err != nil {
		t.Fatalf("a PushInput: %v", err)
	}
	if err := b.PushInput(ctx, fb, 0); err != nil {
		t.Fatalf("b PushInput: %v", err)
	}
	if err := a.Tick(); err != nil {
		t.Fatalf("a Tick: %v", err)
	}
	if err := b.Tick(); err != nil {
		t.Fatalf("b Tick: %v", err)
	}

	// Cancel lane a; lane b must keep working.
	if err := a.Cancel("user_hangup"); err != nil {
		t.Fatalf("a Cancel: %v", err)
	}
	aEvents := collect(t, a)
	dones := eventsOfKind(aEvents, EventDone)
	if len(dones) != 1 || dones[0].Reason != "user_hangup" {
		t.Fatalf("lane a done events %+v, want one with reason user_hangup", dones)
	}

	fb2 := rampFrame(0.55)
	if err := b.PushInput(ctx, fb2, 1); err != nil {
		t.Fatalf("b PushInput after a.Cancel: %v", err)
	}
	if err := b.Tick(); err != nil {
		t.Fatalf("b Tick after a.Cancel: %v", err)
	}
	bAudio := eventsOfKind(collect(t, b), EventAudio)
	if len(bAudio) != 2 {
		t.Fatalf("lane b produced %d audio events, want 2", len(bAudio))
	}
	requireSameBits(t, bAudio[0].Audio, fb)
	requireSameBits(t, bAudio[1].Audio, fb2)

	if a.Stats().SessionID != "dup_a" || b.Stats().SessionID != "dup_b" {
		t.Fatalf("session ids mixed: %q / %q", a.Stats().SessionID, b.Stats().SessionID)
	}
}

// §10.5: the loopback never emits text even when the client negotiates it, and
// the capability is what warns the client up front.
func TestLoopbackNeverEmitsText(t *testing.T) {
	ctx := context.Background()
	d := newFakeDriver(t, NewLoopbackTenant(), fakeDriverConfig{})
	info, err := d.Start(ctx, SessionConfig{SampleRate: SampleRate, Text: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Capabilities.Text {
		t.Fatal("ready said text is available")
	}
	if err := d.PushInput(ctx, rampFrame(0), 0); err != nil {
		t.Fatalf("PushInput: %v", err)
	}
	if err := d.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if text := eventsOfKind(collect(t, d), EventText); len(text) != 0 {
		t.Fatalf("got %d text events, want 0", len(text))
	}
}

// The tenant must see the card and reference opaquely: the platform is not
// allowed to drop or reinterpret them (§7.2).
func TestTenantConfigPassesCardAndReference(t *testing.T) {
	ctx := context.Background()
	spy := &spyTenant{inner: NewZeroDelayLoopbackTenant()}
	d := newFakeDriver(t, spy, fakeDriverConfig{})
	card := map[string]any{"name": "Haru", "persona": "x"}
	ref := Reference{ID: "sana", Transcript: "hello"}
	if _, err := d.Start(ctx, SessionConfig{
		SampleRate: SampleRate,
		MaxMinutes: 5,
		Text:       false,
		Card:       card,
		Reference:  ref,
		Client:     ClientInfo{Name: "forge-web", Version: "0.1"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(spy.sessions) != 1 {
		t.Fatalf("lanes %d, want 1", len(spy.sessions))
	}
	got := spy.sessions[0].cfg
	if got.Card["name"] != "Haru" || got.Reference.ID != "sana" || got.Reference.Transcript != "hello" {
		t.Fatalf("tenant config %+v, want the card and reference passed through", got)
	}
	if got.SampleRate != SampleRate || got.MaxMinutes != 5 || got.SessionID != "dup_test" {
		t.Fatalf("tenant config %+v, want rate 24000, 5 minutes, id dup_test", got)
	}
}
