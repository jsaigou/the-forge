package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingResolver hands out one engine address and counts how many leases are still out.
type countingResolver struct {
	url  string
	open atomic.Int32
}

func (c *countingResolver) Resolve(ctx context.Context, mode VoiceMode, engine string) (Resolved, error) {
	c.open.Add(1)
	return Resolved{URL: c.url, Engine: "e1", Release: func() { c.open.Add(-1) }}, nil
}
func (c *countingResolver) Status() map[string]any { return map[string]any{} }

func TestCloneIsRegisteredOncePerContentAndAgainWhenItChanges(t *testing.T) {
	var mu sync.Mutex
	registers := 0
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/audio/voices" {
			mu.Lock()
			registers++
			mu.Unlock()
		}
		_, _ = w.Write([]byte("RIFFfake"))
	}))
	defer engine.Close()
	b := staticServerBackend("", "", engine.URL)
	spec := SynthSpec{Mode: ModeBase, ID: "sana", RefAudio: []byte("ref-one"), RefText: "hello"}
	for i := 0; i < 3; i++ {
		if _, err := b.Speech(context.Background(), spec, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	if registers != 1 {
		t.Fatalf("registered %d times for three requests with one reference, want 1", registers)
	}
	mu.Unlock()
	spec.RefAudio = []byte("ref-two") // the voice was re-recorded under the same id
	if _, err := b.Speech(context.Background(), spec, "hi"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if registers != 2 {
		t.Fatalf("registered %d times after the reference changed, want 2", registers)
	}
}

func TestCloneIsRegisteredAgainWhenTheEngineLostItsVoices(t *testing.T) {
	var mu sync.Mutex
	registers, known := 0, false
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/audio/voices":
			registers++
			known = true
			w.WriteHeader(200)
		case "/v1/audio/speech":
			if !known {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"unknown voice 'sana'"}}`))
				return
			}
			_, _ = w.Write([]byte("RIFFfake"))
		}
	}))
	defer engine.Close()
	b := staticServerBackend("", "", engine.URL)
	spec := SynthSpec{Mode: ModeBase, ID: "sana", RefAudio: []byte("ref"), RefText: "hello"}
	if _, err := b.Speech(context.Background(), spec, "hi"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	known = false // the engine restarted and has no voices
	mu.Unlock()
	if _, err := b.Speech(context.Background(), spec, "hi"); err != nil {
		t.Fatalf("a restarted engine must be recovered from, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if registers != 2 {
		t.Fatalf("registered %d times, want 2 (first use, then after the restart)", registers)
	}
}

func TestSpeechStreamPassesAudioThroughAsItIsMadeAndHoldsTheLeaseUntilClosed(t *testing.T) {
	release := make(chan struct{})
	var gotFormat atomic.Value
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotFormat.Store(in["response_format"])
		w.WriteHeader(200)
		_, _ = w.Write([]byte("FIRST"))
		w.(http.Flusher).Flush()
		<-release // the rest of the audio is not made yet
		_, _ = w.Write([]byte("SECOND"))
	}))
	defer engine.Close()
	res := &countingResolver{url: engine.URL}
	b := NewServerBackend(res)
	st, err := b.SpeechStream(context.Background(), SynthSpec{Mode: ModeCustomVoice, Speaker: "ryan"}, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if gotFormat.Load() != "pcm" {
		t.Fatalf("engine was asked for %v, want pcm", gotFormat.Load())
	}
	buf := make([]byte, 5)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(st.Body, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(buf) != "FIRST" {
			t.Fatalf("first read = %q, %v", buf, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first audio did not arrive while the engine was still making the rest")
	}
	if res.open.Load() != 1 {
		t.Fatalf("lease released while the audio was still being read (open=%d)", res.open.Load())
	}
	close(release)
	rest, _ := io.ReadAll(st.Body)
	if string(rest) != "SECOND" {
		t.Fatalf("rest = %q", rest)
	}
	st.Body.Close()
	st.Body.Close() // idempotent
	if res.open.Load() != 0 {
		t.Fatalf("lease not released after Close (open=%d)", res.open.Load())
	}
}

func TestSpeechStreamErrorReleasesTheLease(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer engine.Close()
	res := &countingResolver{url: engine.URL}
	if _, err := NewServerBackend(res).SpeechStream(context.Background(), SynthSpec{Mode: ModeCustomVoice, Speaker: "ryan"}, "hi"); err == nil {
		t.Fatal("expected an error")
	}
	if res.open.Load() != 0 {
		t.Fatalf("lease leaked on error (open=%d)", res.open.Load())
	}
}

// streamingFake is an Engine that can stream.
type streamingFake struct {
	fakeEngine
	pr *io.PipeReader
}

func (f *streamingFake) SpeechStream(ctx context.Context, req SpeechRequest) (*SpeechStream, error) {
	return &SpeechStream{Body: f.pr, Format: "pcm"}, nil
}

func TestHandleSpeechStreamsPCMWithoutWaitingForTheEnd(t *testing.T) {
	pr, pw := io.Pipe()
	srv := &Server{Engine: &streamingFake{pr: pr}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handleSpeech))
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"input":"hi","voice":"v","response_format":"pcm"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "audio/pcm" {
		t.Fatalf("content type %q", ct)
	}
	go func() { _, _ = pw.Write([]byte("0123456789")) }() // the writer stays open: the audio is not finished
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 10)
		_, _ = io.ReadFull(resp.Body, b)
		got <- string(b)
	}()
	select {
	case s := <-got:
		if s != "0123456789" {
			t.Fatalf("got %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first audio was held back until the end of the stream")
	}
	pw.Close()
}

func TestHandleSpeechFallsBackToBufferedWhenNotStreamable(t *testing.T) {
	f := &fakeEngine{speech: &SpeechResult{Audio: []byte("WAVBYTES"), Format: "wav"}}
	srv := &Server{Engine: &nonStreamable{f}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handleSpeech))
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"input":"hi","voice":"v","response_format":"pcm"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "WAVBYTES" {
		t.Fatalf("got %q", b)
	}
}

type nonStreamable struct{ *fakeEngine }

func (nonStreamable) SpeechStream(ctx context.Context, req SpeechRequest) (*SpeechStream, error) {
	return nil, ErrNoStream
}
