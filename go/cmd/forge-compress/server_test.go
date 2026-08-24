// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/compress"
)

// fakeTokenizer/fakeScorer mirror internal/compress's own unexported test
// fakes (fakes_test.go) — reproduced here since those aren't exported, and
// this package needs a cgo-free compress.Engine to test its own HTTP
// plumbing without linking real native libs.
type fakeTokenizer struct{}

func (fakeTokenizer) EncodeWords(words []string) (compress.Encoding, error) {
	ids := []int64{-1}
	mask := []int64{1}
	widx := []int{-1}
	for i := range words {
		ids = append(ids, int64(i))
		mask = append(mask, 1)
		widx = append(widx, i)
	}
	ids = append(ids, -2)
	mask = append(mask, 1)
	widx = append(widx, -1)
	return compress.Encoding{IDs: ids, AttentionMask: mask, WordIndex: widx}, nil
}

// fakeScorer keeps only even-indexed words — enough to prove real
// compression happened (content actually shrinks) without needing a real
// model.
type fakeScorer struct{}

func (fakeScorer) Score(inputIDs, _ []int64) ([]float32, error) {
	scores := make([]float32, len(inputIDs))
	for i, id := range inputIDs {
		if id < 0 {
			continue
		}
		if id%2 == 0 {
			scores[i] = 1
		}
	}
	return scores, nil
}

func testEngine() *compress.Engine {
	return &compress.Engine{
		Tokenizer: fakeTokenizer{},
		Scorer:    fakeScorer{},
		Config:    compress.Config{ScoreThreshold: 0.5, MinWords: 1, ByteThreshold: 0},
	}
}

func testServer(t *testing.T, cfg config) *server {
	t.Helper()
	m := newMetrics()
	return newServer(cfg, testEngine(), m)
}

func TestResolveUpstream(t *testing.T) {
	t.Run("x-compress-base-url takes precedence, appends /v1", func(t *testing.T) {
		s := testServer(t, config{TargetAPIURL: "https://fallback.example.com/v1"})
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("x-compress-base-url", "http://127.0.0.1:8080")
		got, err := s.resolveUpstream(r)
		if err != nil {
			t.Fatal(err)
		}
		if want := "http://127.0.0.1:8080/v1"; got != want {
			t.Errorf("resolveUpstream = %q, want %q", got, want)
		}
	})

	t.Run("trailing slash on the header is normalized", func(t *testing.T) {
		s := testServer(t, config{})
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("x-compress-base-url", "http://127.0.0.1:8080/")
		got, err := s.resolveUpstream(r)
		if err != nil {
			t.Fatal(err)
		}
		if want := "http://127.0.0.1:8080/v1"; got != want {
			t.Errorf("resolveUpstream = %q, want %q", got, want)
		}
	})

	t.Run("falls back to TargetAPIURL when no header", func(t *testing.T) {
		s := testServer(t, config{TargetAPIURL: "https://api.deepseek.com/v1"})
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		got, err := s.resolveUpstream(r)
		if err != nil {
			t.Fatal(err)
		}
		if want := "https://api.deepseek.com/v1"; got != want {
			t.Errorf("resolveUpstream = %q, want %q", got, want)
		}
	})

	t.Run("errors when neither is available", func(t *testing.T) {
		s := testServer(t, config{})
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		if _, err := s.resolveUpstream(r); err == nil {
			t.Error("expected an error with no header and no configured target")
		}
	})
}

func TestHandleChatCompletions_CompressesAndForwards(t *testing.T) {
	var receivedBody map[string]any
	var receivedAuth, receivedCompressorHeader string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedCompressorHeader = r.Header.Get("x-compress-base-url")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	longContent := strings.Repeat("word ", 50) // 50 words, well over MinWords=1/ByteThreshold=0
	reqBody := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": longContent},
		},
	}
	reqJSON, _ := json.Marshal(reqBody)

	s := testServer(t, config{FailOpenBudgetMS: 2000})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqJSON))
	req.Header.Set("x-compress-base-url", upstream.URL) // bare origin, as a real slot root would be
	req.Header.Set("Authorization", "Bearer should-not-be-forwarded-by-us-but-untouched-if-present")
	s.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if receivedCompressorHeader != "" {
		t.Errorf("x-compress-base-url leaked to upstream: %q", receivedCompressorHeader)
	}
	if receivedAuth != "Bearer should-not-be-forwarded-by-us-but-untouched-if-present" {
		t.Errorf("Authorization was not forwarded verbatim: got %q", receivedAuth)
	}

	msgs, _ := receivedBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message forwarded upstream, got %d", len(msgs))
	}
	got := msgs[0].(map[string]any)["content"].(string)
	if got == longContent {
		t.Error("expected content to be compressed (shorter), got byte-identical original")
	}
	gotWords := len(strings.Fields(got))
	if gotWords == 0 || gotWords >= 50 {
		t.Errorf("compressed word count = %d, want fewer than 50 and more than 0 (must-keep-free fixture)", gotWords)
	}

	if rec.Body.String() != `{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}` {
		t.Errorf("client did not receive upstream's response verbatim: %s", rec.Body.String())
	}

	if got := s.metrics.requests.load(); got != 1 {
		t.Errorf("requests counter = %d, want 1", got)
	}
	if got := s.metrics.tokensSaved.load(); got <= 0 {
		t.Errorf("tokensSaved = %d, want > 0 for a genuinely-compressed message", got)
	}
	if got := s.metrics.requestsByModel.snapshot(); len(got) != 1 || got[0].key != "test-model" {
		t.Errorf("requestsByModel = %v, want one entry for %q", got, "test-model")
	}
}

func TestHandleChatCompletions_NonJSONBodyPassesThroughUntouched(t *testing.T) {
	var receivedRaw []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRaw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	s := testServer(t, config{})
	rec := httptest.NewRecorder()
	notJSON := "not actually json"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(notJSON))
	req.Header.Set("x-compress-base-url", upstream.URL)
	s.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if string(receivedRaw) != notJSON {
		t.Errorf("malformed body was not passed through unmutated: got %q, want %q", receivedRaw, notJSON)
	}
}

// Sprint 4 (resource bounding + monitoring): a client disconnect must not
// read as a compressor failure — see server.go's ErrorHandler doc comment.

func TestHandleChatCompletions_CanceledRequestNotCountedAsFailed(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test cancels the client's context
	}))
	defer upstream.Close()
	defer close(release)

	s := testServer(t, config{})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("x-compress-base-url", upstream.URL)
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	s.handleChatCompletions(rec, req)

	if got := s.metrics.requestsCanceled.load(); got != 1 {
		t.Errorf("requestsCanceled = %d, want 1", got)
	}
	if got := s.metrics.requestsFailed.load(); got != 0 {
		t.Errorf("requestsFailed = %d, want 0 — a canceled request must not count as a failure", got)
	}
}

func TestHandleChatCompletions_TimeoutCountedSeparately(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang past the client's deadline
	}))
	defer upstream.Close()
	defer close(release)

	s := testServer(t, config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("x-compress-base-url", upstream.URL)
	rec := httptest.NewRecorder()

	s.handleChatCompletions(rec, req)

	if got := s.metrics.requestsTimeout.load(); got != 1 {
		t.Errorf("requestsTimeout = %d, want 1", got)
	}
	if got := s.metrics.requestsFailed.load(); got != 1 {
		t.Errorf("requestsFailed = %d, want 1 — unlike a cancellation, a timeout IS a real failure", got)
	}
}

// The X-Forge-Compress[-Error] headers are what lets a0's router tell
// "the compressor reached upstream and is relaying its response" from "the
// compressor itself is what broke" — see internal/router/proxy.go's layer
// classification.

func TestHandleChatCompletions_ReachedHeaderOnRelayedFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	s := testServer(t, config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("x-compress-base-url", upstream.URL)
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := rec.Header().Get(headerCompressReached); got != "1" {
		t.Errorf("%s = %q, want %q — this response was relayed from a real upstream", headerCompressReached, got, "1")
	}
	if got := rec.Header().Get(headerCompressError); got != "" {
		t.Errorf("%s = %q, want empty — a relayed response is not a self-generated failure", headerCompressError, got)
	}
}

func TestHandleChatCompletions_ErrorHeaderOnSelfGeneratedFailure(t *testing.T) {
	s := testServer(t, config{}) // no TargetAPIURL, no x-compress-base-url header → no_upstream
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := rec.Header().Get(headerCompressError); got != "no_upstream" {
		t.Errorf("%s = %q, want %q", headerCompressError, got, "no_upstream")
	}
	if got := rec.Header().Get(headerCompressReached); got != "" {
		t.Errorf("%s = %q, want empty — no upstream was ever reached", headerCompressReached, got)
	}
}

func TestHandleChatCompletions_NoUpstreamConfigured(t *testing.T) {
	s := testServer(t, config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	s.handleChatCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := s.metrics.requestsFailed.load(); got != 1 {
		t.Errorf("requestsFailed = %d, want 1", got)
	}
}

// TestHandleChatCompletions_StreamingNoBuffering mirrors this repo's own
// hard requirement for the a0 router (internal/router's
// TestChatCompletions_StreamingNoBuffering) — SSE chunks must reach the
// client as they arrive, not all at once after the full response buffers.
func TestHandleChatCompletions_StreamingNoBuffering(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	chunkDelay := 80 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, c := range chunks {
			io.WriteString(w, c)
			flusher.Flush()
			time.Sleep(chunkDelay)
		}
	}))
	defer upstream.Close()

	s := testServer(t, config{})
	proxySrv := httptest.NewServer(s.mux())
	defer proxySrv.Close()

	start := time.Now()
	reqBody := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("x-compress-base-url", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	var arrivals []time.Time
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			arrivals = append(arrivals, time.Now())
		}
		if err != nil {
			break
		}
	}
	if len(arrivals) < len(chunks) {
		t.Fatalf("got %d non-empty lines, want at least %d", len(arrivals), len(chunks))
	}
	// If buffered, every arrival would land at ~the same instant near the
	// end of the total delay. Streaming means the first chunk arrives
	// close to t=0, well before the last chunk's delay has elapsed.
	firstGap := arrivals[0].Sub(start)
	if firstGap >= time.Duration(len(chunks))*chunkDelay {
		t.Errorf("first chunk arrived after %v — looks buffered, not streamed", firstGap)
	}
}
