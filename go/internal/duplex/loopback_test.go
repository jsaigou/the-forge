// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// adversarialFrame carries the float32 values a careless "echo" would normalise
// or lose: NaN with a payload, both infinities, negative zero, a subnormal, and
// the finite extremes. Bit equality after Step is the real bit-exact test.
func adversarialFrame() Frame {
	f := make(Frame, FrameSamples)
	for i := range f {
		f[i] = float32(i) / float32(FrameSamples)
	}
	f[0] = math.Float32frombits(0x7fc00001) // quiet NaN, non-default payload
	f[1] = float32(math.Inf(1))
	f[2] = float32(math.Inf(-1))
	f[3] = math.Float32frombits(0x80000000) // negative zero
	f[4] = math.Float32frombits(0x00000001) // smallest positive subnormal
	f[5] = math.MaxFloat32
	f[6] = -math.MaxFloat32
	f[7] = math.SmallestNonzeroFloat32
	return f
}

func newSession(t *testing.T, tenant *LoopbackTenant) TenantSession {
	t.Helper()
	info, err := tenant.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info.Name != "loopback" {
		t.Fatalf("tenant name %q, want loopback", info.Name)
	}
	sess, err := tenant.Start(context.Background(), TenantConfig{SampleRate: SampleRate, MaxMinutes: 30})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return sess
}

// Zero-delay mode is the wire test's oracle: whatever float32 samples went in
// must come back with identical bit patterns, and with no text.
func TestLoopbackZeroDelayIsBitExact(t *testing.T) {
	sess := newSession(t, NewZeroDelayLoopbackTenant())
	defer sess.Close()

	in := adversarialFrame()
	tf, err := sess.Step(context.Background(), in, 0)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	requireSameBits(t, Frame(tf.Audio), in)
	if tf.Text != "" {
		t.Fatalf("text %q, want empty", tf.Text)
	}
	if got, ok := tf.Meta["echo_seq"].(uint32); !ok || got != 0 {
		t.Fatalf("echo_seq %v, want 0", tf.Meta["echo_seq"])
	}
}

// The documented mode holds exactly one frame: tick 0 is silent, tick n+1
// returns tick n's input. The drain Step returns the tail, which is how a
// driver keeps the "no silent loss" promise with a latency-bearing tenant.
func TestLoopbackOneFrameDelay(t *testing.T) {
	sess := newSession(t, NewLoopbackTenant())
	defer sess.Close()
	ctx := context.Background()

	frames := []Frame{adversarialFrame(), rampFrame(0.25), rampFrame(-0.5)}

	tf, err := sess.Step(ctx, frames[0], 0)
	if err != nil {
		t.Fatalf("priming Step: %v", err)
	}
	if tf.Audio != nil {
		t.Fatalf("priming Step returned audio, want nil (one-frame delay)")
	}
	if tf.Text != "" {
		t.Fatalf("text %q, want empty", tf.Text)
	}

	for i := 1; i < len(frames); i++ {
		tf, err := sess.Step(ctx, frames[i], uint32(i))
		if err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
		requireSameBits(t, Frame(tf.Audio), frames[i-1])
		if got, ok := tf.Meta["echo_seq"].(uint32); !ok || got != uint32(i-1) {
			t.Fatalf("step %d echo_seq %v, want %d", i, tf.Meta["echo_seq"], i-1)
		}
	}

	// The documented drain: one extra Step with a zero frame flushes the tail.
	tf, err = sess.Step(ctx, make(Frame, FrameSamples), uint32(len(frames)))
	if err != nil {
		t.Fatalf("drain Step: %v", err)
	}
	requireSameBits(t, Frame(tf.Audio), frames[len(frames)-1])
	if got, ok := tf.Meta["echo_seq"].(uint32); !ok || got != uint32(len(frames)-1) {
		t.Fatalf("drain echo_seq %v, want %d", tf.Meta["echo_seq"], len(frames)-1)
	}
}

// Sequence mirroring is what lets the transport put the client's own sequence
// on the echoed frame (protocol.md §2.1). The loopback reports the sequence of
// the frame it is echoing, not the tick it is running on.
func TestLoopbackMirrorsSequence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay int
		steps int
		want  []uint32
	}{
		{"zero_delay", 0, 3, []uint32{0, 1, 2}},
		// Four steps produce three frames: the fourth input is still buffered.
		{"one_frame_delay", 1, 4, []uint32{0, 1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := newSession(t, &LoopbackTenant{DelayFrames: tc.delay})
			defer sess.Close()

			var got []uint32
			for seq := 0; seq < tc.steps; seq++ {
				tf, err := sess.Step(context.Background(), rampFrame(0), uint32(seq))
				if err != nil {
					t.Fatalf("Step %d: %v", seq, err)
				}
				if tf.Audio == nil {
					continue // the priming tick has no frame and no sequence
				}
				got = append(got, tf.Meta["echo_seq"].(uint32))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("echoed %d frames, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("echo seqs %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A wrong-length frame is bad_frame, not a slice out of range: the tenant is a
// boundary the transport can get wrong.
func TestLoopbackRejectsBadFrame(t *testing.T) {
	sess := newSession(t, NewLoopbackTenant())
	defer sess.Close()

	for _, n := range []int{0, FrameSamples - 1, FrameSamples + 1} {
		_, err := sess.Step(context.Background(), make(Frame, n), 0)
		requireCode(t, err, ErrBadFrame)
	}
}

// Close is idempotent and a Step after it is cancelled, not a panic on a
// released buffer.
func TestLoopbackClosedSessionErrors(t *testing.T) {
	sess := newSession(t, NewLoopbackTenant())
	if err := sess.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_, err := sess.Step(context.Background(), rampFrame(0), 0)
	requireCode(t, err, ErrCancelled)
}

// A cancelled context is the caller's signal, not a silent success.
func TestLoopbackStepHonoursContext(t *testing.T) {
	sess := newSession(t, NewZeroDelayLoopbackTenant())
	defer sess.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := sess.Step(ctx, rampFrame(0), 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Step error %v, want context.Canceled", err)
	}
}

// Start validates only what it cannot honour. The documented rate is the only
// one; a delay other than 0/1 is a harness bug, not a latency to clamp.
func TestLoopbackStartValidation(t *testing.T) {
	ctx := context.Background()

	for _, rate := range []int{0, SampleRate} {
		if _, err := NewLoopbackTenant().Start(ctx, TenantConfig{SampleRate: rate}); err != nil {
			t.Fatalf("Start(rate=%d): %v", rate, err)
		}
	}
	for _, rate := range []int{16000, 48000} {
		_, err := NewLoopbackTenant().Start(ctx, TenantConfig{SampleRate: rate})
		requireCode(t, err, ErrBadRequest)
	}
	if _, err := NewLoopbackTenant().Start(ctx, TenantConfig{MaxMinutes: -1}); err == nil {
		t.Fatal("negative max_minutes accepted")
	}
	if _, err := (&LoopbackTenant{DelayFrames: 2}).Start(ctx, TenantConfig{}); err == nil {
		t.Fatal("delay of 2 frames accepted")
	}
}

// Capabilities are the contract's promise to the client: no text, 24 kHz only.
// Card and reference are accepted opaquely and ignored, so the harness can send
// whatever a real tenant would get.
func TestLoopbackCapabilitiesAndIgnoresCardReference(t *testing.T) {
	lt := NewLoopbackTenant()
	info, err := lt.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info.Capabilities.Text {
		t.Fatal("loopback advertises text, want no text")
	}
	if len(info.Capabilities.InputRates) != 1 || info.Capabilities.InputRates[0] != SampleRate {
		t.Fatalf("input rates %v, want [%d]", info.Capabilities.InputRates, SampleRate)
	}
	sess, err := lt.Start(context.Background(), TenantConfig{
		SampleRate: SampleRate,
		Text:       true, // requested, but the loopback is still silent
		Card:       map[string]any{"name": "Haru", "persona": "anything", "nested": map[string]any{"k": 1}},
		Reference:  Reference{ID: "sana", Audio: []byte{1, 2, 3}},
	})
	if err != nil {
		t.Fatalf("Start with card/reference: %v", err)
	}
	defer sess.Close()
	tf, err := sess.Step(context.Background(), rampFrame(0), 0)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if tf.Text != "" {
		t.Fatalf("text %q, want empty", tf.Text)
	}
}

// Two lanes from one tenant share no state: interleaving them must not mix the
// delayed frames.
func TestLoopbackLanesAreIndependent(t *testing.T) {
	lt := NewLoopbackTenant()
	ctx := context.Background()
	a, err := lt.Start(ctx, TenantConfig{SampleRate: SampleRate})
	if err != nil {
		t.Fatalf("lane a Start: %v", err)
	}
	defer a.Close()
	b, err := lt.Start(ctx, TenantConfig{SampleRate: SampleRate})
	if err != nil {
		t.Fatalf("lane b Start: %v", err)
	}
	defer b.Close()

	fa, fb := rampFrame(0.1), rampFrame(0.6)
	if _, err := a.Step(ctx, fa, 0); err != nil {
		t.Fatalf("a prime: %v", err)
	}
	if _, err := b.Step(ctx, fb, 0); err != nil {
		t.Fatalf("b prime: %v", err)
	}
	// Different second frames so a shared buffer would be visible.
	fa2, fb2 := rampFrame(-0.2), rampFrame(-0.7)
	tfa, err := a.Step(ctx, fa2, 1)
	if err != nil {
		t.Fatalf("a step: %v", err)
	}
	tfb, err := b.Step(ctx, fb2, 1)
	if err != nil {
		t.Fatalf("b step: %v", err)
	}
	requireSameBits(t, Frame(tfa.Audio), fa)
	requireSameBits(t, Frame(tfb.Audio), fb)
	// Closing one lane must not disturb the other.
	if err := a.Close(); err != nil {
		t.Fatalf("a Close: %v", err)
	}
	tf, err := b.Step(ctx, fa2, 2)
	if err != nil {
		t.Fatalf("b step after a.Close: %v", err)
	}
	requireSameBits(t, Frame(tf.Audio), fb2)
}

// A frame handed to Step must not be aliased: the caller is free to reuse its
// buffer after the call, and the echoed frame must not change underneath it.
func TestLoopbackDoesNotAliasCallerBuffer(t *testing.T) {
	sess := newSession(t, NewZeroDelayLoopbackTenant())
	defer sess.Close()

	in := rampFrame(0.3)
	tf, err := sess.Step(context.Background(), in, 0)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	want := append(Frame(nil), in...)
	for i := range in {
		in[i] = -1 // the caller reuses its buffer (contract.go: "callers can avoid a copy")
	}
	requireSameBits(t, Frame(tf.Audio), want)
}

// The fake clock must not be confused with real time: FrameDuration is the
// documented 133.333 ms, and the harness advances time only when told.
func TestFrameDurationIsDocumentedPeriod(t *testing.T) {
	if want := 133*time.Millisecond + 333*time.Microsecond; FrameDuration != want {
		t.Fatalf("FrameDuration %v, want %v", FrameDuration, want)
	}
	if got := float64(FrameSamples) * 1000 / float64(SampleRate); got < 133.333 || got > 133.334 {
		t.Fatalf("frame millis %.6f, want 133.333", got)
	}
}
