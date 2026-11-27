// SPDX-License-Identifier: Apache-2.0

package duplexremote

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
)

func u32(v uint32) *uint32 { return &v }

func testFrame(seed int) duplex.Frame {
	f := make(duplex.Frame, duplex.FrameSamples)
	for i := range f {
		f[i] = float32((i*7+seed*13)%2001-1000) / 1000
	}
	return f
}

func sameBits(t *testing.T, got []float32, want duplex.Frame) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// newPair starts a mock engine and returns it with a Tenant pointed at it.
func newPair(t *testing.T, opts MockEngineOptions) (*MockEngine, *Tenant) {
	t.Helper()
	m := NewMockEngine(opts)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	name := opts.Tenant
	if name == "" {
		name = "mock"
	}
	tn := New(Config{BaseURL: srv.URL, Tenant: name, StepTimeout: 400 * time.Millisecond})
	t.Cleanup(func() { _ = tn.Close() })
	return m, tn
}

func wantCode(t *testing.T, err error, code duplex.ErrorCode) {
	t.Helper()
	var de *duplex.Error
	if !errors.As(err, &de) {
		t.Fatalf("err = %v (%T), want *duplex.Error %s", err, err, code)
	}
	if de.Code != code {
		t.Fatalf("code = %s (%s), want %s", de.Code, de.Message, code)
	}
}

func waitActive(t *testing.T, m *MockEngine, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Active() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("engine active lanes = %d, want %d", m.Active(), want)
}

func TestLoadReportsEngineCapabilities(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{Text: true})
	info, err := tn.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info.Name != "mock" || !info.Capabilities.Text {
		t.Fatalf("info = %+v", info)
	}
}

func TestLoadRefusesEngineThatIsNotReady(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{NotReady: true})
	_, err := tn.Load(context.Background())
	wantCode(t, err, duplex.ErrModelNotReady)
}

func TestLoadRefusesUnknownContractVersion(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{Contract: "9"})
	_, err := tn.Load(context.Background())
	wantCode(t, err, duplex.ErrTenantUnavailable)
}

func TestLoadRefusesUnknownTenant(t *testing.T) {
	m := NewMockEngine(MockEngineOptions{Tenant: "haru"})
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	tn := New(Config{BaseURL: srv.URL, Tenant: "other"})
	_, err := tn.Load(context.Background())
	wantCode(t, err, duplex.ErrTenantUnavailable)
}

func TestLoadRefusesEngineWithDifferentFrameFormat(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{FrameSamples: 1600})
	_, err := tn.Load(context.Background())
	wantCode(t, err, duplex.ErrTenantUnavailable)
}

func TestEchoIsBitExact(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	ctx := context.Background()
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s1", SampleRate: duplex.SampleRate})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 5; i++ {
		in := testFrame(i)
		out, err := s.Step(ctx, in, uint32(i))
		if err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
		sameBits(t, out.Audio, in)
		if out.Text != "" {
			t.Fatalf("unexpected text %q", out.Text)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitActive(t, m, 0)
}

func TestStartCarriesCardReferenceAndLimits(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{Text: true})
	ref := []byte("not-really-audio-but-bytes")
	s, err := tn.Start(context.Background(), duplex.TenantConfig{
		SessionID:  "sess-42",
		SampleRate: duplex.SampleRate,
		MaxMinutes: 7,
		Text:       true,
		Card:       map[string]any{"name": "Haru", "n": float64(3)},
		Reference:  duplex.Reference{ID: "voice-a", Audio: ref, Transcript: "hi"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	got := m.Starts()
	if len(got) != 1 {
		t.Fatalf("engine saw %d starts", len(got))
	}
	st := got[0]
	if st.SessionID != "sess-42" || st.Tenant != "mock" || !st.Text || st.MaxMinutes != 7 {
		t.Fatalf("start = %+v", st)
	}
	card, ok := st.Card.(map[string]any)
	if !ok {
		t.Fatalf("card = %v (%T), want the structured object", st.Card, st.Card)
	}
	if card["name"] != "Haru" || card["n"] != float64(3) {
		t.Fatalf("card = %v", st.Card)
	}
	if st.Reference.ID != "voice-a" || st.Reference.Transcript != "hi" || st.Reference.Bytes != len(ref) {
		t.Fatalf("reference = %+v", st.Reference)
	}
	if string(st.ReferenceAudio) != string(ref) {
		t.Fatalf("reference audio = %q", st.ReferenceAudio)
	}
}

func TestCardRendererSendsRenderedText(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	tn.cfg.CardRenderer = func(card map[string]any) (any, error) {
		if card["name"] != "Haru" {
			t.Fatalf("renderer received card %v", card)
		}
		return "rendered card", nil
	}
	s, err := tn.Start(context.Background(), duplex.TenantConfig{
		SessionID: "s",
		Card:      map[string]any{"name": "Haru"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	st := m.Starts()
	if len(st) != 1 {
		t.Fatalf("engine saw %d starts", len(st))
	}
	if got := st[0].Card; got != "rendered card" {
		t.Fatalf("engine card = %v (%T), want the rendered string", got, got)
	}
}

func TestCardRendererFailureIsBadRequestBeforeConnecting(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	tn.cfg.CardRenderer = func(map[string]any) (any, error) {
		return nil, errors.New("card is not acceptable")
	}
	_, err := tn.Start(context.Background(), duplex.TenantConfig{
		SessionID: "s",
		Card:      map[string]any{"name": "Haru"},
	})
	wantCode(t, err, duplex.ErrBadRequest)
	var de *duplex.Error
	if !errors.As(err, &de) || !de.Fatal {
		t.Fatalf("error must be fatal: %v", err)
	}
	if len(m.Starts()) != 0 {
		t.Fatalf("the engine saw a start for a rejected card")
	}
}

func TestStartWithoutCardSendsNoCard(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	if got := m.Starts()[0].Card; got != nil {
		t.Fatalf("engine card = %v, want omitted", got)
	}
}

func TestStartWithoutReferenceSendsNoAudio(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	st := m.Starts()[0]
	if st.Reference.Bytes != 0 || len(st.ReferenceAudio) != 0 {
		t.Fatalf("reference = %+v audio=%d", st.Reference, len(st.ReferenceAudio))
	}
}

func TestEngineStartErrorKeepsItsCode(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{FailStartCode: "session_limit"})
	_, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s"})
	wantCode(t, err, duplex.ErrSessionLimit)
}

func TestEngineLaneLimitIsEnforcedByTheEngine(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{MaxLanes: 1})
	a, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "a"})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	_, err = tn.Start(context.Background(), duplex.TenantConfig{SessionID: "b"})
	wantCode(t, err, duplex.ErrSessionLimit)
	_ = a.Close()
	waitActive(t, m, 0)
	b, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "c"})
	if err != nil {
		t.Fatalf("Start after release: %v", err)
	}
	_ = b.Close()
}

func TestTextAndSilentRepliesAreSurfaced(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{Text: true, DelayFrames: 1})
	ctx := context.Background()
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s", Text: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	out, err := s.Step(ctx, testFrame(1), 0)
	if err != nil {
		t.Fatalf("Step 0: %v", err)
	}
	if len(out.Audio) != 0 {
		t.Fatalf("first delayed step returned audio")
	}
	if out.Text != "f0" {
		t.Fatalf("text = %q, want f0", out.Text)
	}
	out, err = s.Step(ctx, testFrame(2), 1)
	if err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	sameBits(t, out.Audio, testFrame(1))
}

func TestFlushRecoversTheBufferedFrame(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{DelayFrames: 1})
	ctx := context.Background()
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	if _, err := s.Step(ctx, testFrame(9), 0); err != nil {
		t.Fatalf("Step: %v", err)
	}
	fl, ok := s.(duplex.Flusher)
	if !ok {
		t.Fatalf("session does not implement duplex.Flusher")
	}
	out, err := fl.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	sameBits(t, out.Audio, testFrame(9))
	out, err = fl.Flush(ctx)
	if err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if len(out.Audio) != 0 {
		t.Fatalf("second flush returned audio")
	}
}

func TestWrongLengthFrameIsRejectedBeforeSending(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	ctx := context.Background()
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	_, err = s.Step(ctx, make(duplex.Frame, 10), 0)
	wantCode(t, err, duplex.ErrBadFrame)
	if m.Steps() != 0 {
		t.Fatalf("engine saw %d steps, want 0", m.Steps())
	}
	// A rejected frame must not poison the lane.
	if _, err := s.Step(ctx, testFrame(1), 0); err != nil {
		t.Fatalf("Step after reject: %v", err)
	}
}

func TestEngineErrorMidSessionIsFatalAndKeepsItsCode(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{FailAtSeq: u32(2), FailCode: "internal"})
	ctx := context.Background()
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		if _, err := s.Step(ctx, testFrame(i), uint32(i)); err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
	}
	_, err = s.Step(ctx, testFrame(2), 2)
	wantCode(t, err, duplex.ErrInternal)
	var de *duplex.Error
	if !errors.As(err, &de) || !de.Fatal {
		t.Fatalf("error must be fatal: %v", err)
	}
	// The lane is broken: no further traffic.
	_, err = s.Step(ctx, testFrame(3), 3)
	if err == nil {
		t.Fatalf("Step after fatal error succeeded")
	}
}

func TestEngineThatEchoesTheWrongSeqIsRejected(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{SeqOffset: 1})
	ctx := context.Background()
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	_, err = s.Step(ctx, testFrame(0), 0)
	wantCode(t, err, duplex.ErrBadSequence)
}

func TestStalledEngineHitsTheStepDeadline(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{StallAtSeq: u32(1)})
	ctx := context.Background()
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	if _, err := s.Step(ctx, testFrame(0), 0); err != nil {
		t.Fatalf("Step 0: %v", err)
	}
	begin := time.Now()
	_, err = s.Step(ctx, testFrame(1), 1)
	wantCode(t, err, duplex.ErrTimeout)
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("stalled step took %v; the deadline is not enforced", d)
	}
}

func TestContextCancelAbortsAStep(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{StallAtSeq: u32(0)})
	tn.cfg.StepTimeout = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	time.AfterFunc(50*time.Millisecond, cancel)
	begin := time.Now()
	_, err = s.Step(ctx, testFrame(0), 0)
	wantCode(t, err, duplex.ErrCancelled)
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

func TestEngineDroppingTheSocketIsTenantUnavailable(t *testing.T) {
	_, tn := newPair(t, MockEngineOptions{DropAtSeq: u32(1)})
	ctx := context.Background()
	s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	if _, err := s.Step(ctx, testFrame(0), 0); err != nil {
		t.Fatalf("Step 0: %v", err)
	}
	_, err = s.Step(ctx, testFrame(1), 1)
	wantCode(t, err, duplex.ErrTenantUnavailable)
}

func TestCloseIsIdempotentAndReleasesTheLane(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitActive(t, m, 1)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	waitActive(t, m, 0)
	if _, err := s.Step(context.Background(), testFrame(0), 0); err == nil {
		t.Fatalf("Step after Close succeeded")
	}
}

func TestDialFailureIsTenantUnavailable(t *testing.T) {
	m := NewMockEngine(MockEngineOptions{})
	srv := httptest.NewServer(m.Handler())
	url := srv.URL
	srv.Close() // nothing listens any more
	tn := New(Config{BaseURL: url, Tenant: "mock"})
	_, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s"})
	wantCode(t, err, duplex.ErrTenantUnavailable)
}

// The M1 shape: the real driver, the remote tenant and a mock engine, with the
// one-frame delay and the Flusher recovering the last frame while draining.
func TestThroughTheRealDriverNoFrameIsLost(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{DelayFrames: 1})
	d := duplex.NewDriver(tn, nil)
	ctx := context.Background()
	if _, err := d.Start(ctx, duplex.SessionConfig{Tenant: "mock"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	in := []duplex.Frame{testFrame(1), testFrame(2), testFrame(3)}
	for i, f := range in {
		if err := d.PushInput(ctx, f, uint32(i)); err != nil {
			t.Fatalf("PushInput %d: %v", i, err)
		}
	}
	var got []duplex.Frame
	collect := func(max time.Duration) {
		deadline := time.Now().Add(max)
		for time.Now().Before(deadline) {
			ev, err := d.NextEvent(ctx, 100*time.Millisecond)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					continue
				}
				t.Fatalf("NextEvent: %v", err)
			}
			switch ev.Kind {
			case duplex.EventAudio:
				got = append(got, ev.Audio)
			case duplex.EventError:
				t.Fatalf("driver error: %v", ev.Error)
			case duplex.EventDone:
				return
			}
		}
	}
	// Let the three frames run, then close gracefully.
	time.Sleep(700 * time.Millisecond)
	if err := d.Cancel("client_done"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	collect(3 * time.Second)
	if len(got) != len(in) {
		t.Fatalf("got %d audio frames, want %d", len(got), len(in))
	}
	for i := range in {
		sameBits(t, got[i], in[i])
	}
	_ = d.Close()
	waitActive(t, m, 0)
}

func TestLargeReferenceIsSentInBoundedChunks(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	ref := make([]byte, 5<<20+3)
	for i := range ref {
		ref[i] = byte(i * 7)
	}
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "big", Reference: duplex.Reference{Audio: ref}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	st := m.Starts()[0]
	if string(st.ReferenceAudio) != string(ref) {
		t.Fatalf("reference arrived with %d bytes, want %d, or differs", len(st.ReferenceAudio), len(ref))
	}
	if st.ReferenceChunks < 6 || st.LargestChunk > MaxReferenceChunk {
		t.Fatalf("chunks = %d, largest = %d; want at least 6 chunks of at most %d", st.ReferenceChunks, st.LargestChunk, MaxReferenceChunk)
	}
}

func TestSmallReferenceIsOneMessage(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{})
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s", Reference: duplex.Reference{Audio: make([]byte, 4096)}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	if st := m.Starts()[0]; st.ReferenceChunks != 1 {
		t.Fatalf("a small reference took %d messages, want 1", st.ReferenceChunks)
	}
}

func TestReferenceAboveTheEnginesLimitIsRefusedBeforeConnecting(t *testing.T) {
	m, tn := newPair(t, MockEngineOptions{MaxReferenceBytes: 1 << 20})
	_, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s", Reference: duplex.Reference{Audio: make([]byte, 2<<20)}})
	wantCode(t, err, duplex.ErrBadRequest)
	if len(m.Starts()) != 0 {
		t.Fatalf("the engine saw a start for a reference it had said it would refuse")
	}
}
