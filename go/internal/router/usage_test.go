// SPDX-License-Identifier: Apache-2.0

package router

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// ── applyStreamUsageOptions ──────────────────────────────────────────────

func TestApplyStreamUsageOptionsInjectsWhenStreaming(t *testing.T) {
	body := map[string]any{"model": "m", "stream": true}
	out := applyStreamUsageOptions(body, true)
	so, ok := out["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %+v", out)
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", so["include_usage"])
	}
	// Original body must be untouched (a fresh map is returned).
	if _, has := body["stream_options"]; has {
		t.Error("original body map was mutated")
	}
}

func TestApplyStreamUsageOptionsSkipsNonStreaming(t *testing.T) {
	body := map[string]any{"model": "m"}
	out := applyStreamUsageOptions(body, true)
	if _, has := out["stream_options"]; has {
		t.Error("stream_options injected for a non-streaming request")
	}
}

func TestApplyStreamUsageOptionsRespectsClientChoice(t *testing.T) {
	body := map[string]any{"model": "m", "stream": true, "stream_options": map[string]any{"include_usage": false}}
	out := applyStreamUsageOptions(body, true)
	so := out["stream_options"].(map[string]any)
	if so["include_usage"] != false {
		t.Errorf("client's own stream_options was overwritten: %+v", so)
	}
}

func TestApplyStreamUsageOptionsDisabled(t *testing.T) {
	body := map[string]any{"model": "m", "stream": true}
	out := applyStreamUsageOptions(body, false)
	if _, has := out["stream_options"]; has {
		t.Error("stream_options injected while the setting is disabled")
	}
}

// ── usage parsing ─────────────────────────────────────────────────────────

func TestParseNonStreamingUsage(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	prompt, completion, cached, ok := parseNonStreamingUsage(body)
	if !ok || prompt != 100 || completion != 20 || cached != 0 {
		t.Errorf("got (%d,%d,%d,%v), want (100,20,0,true)", prompt, completion, cached, ok)
	}
}

func TestParseNonStreamingUsageOpenAICacheField(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`)
	_, _, cached, ok := parseNonStreamingUsage(body)
	if !ok || cached != 30 {
		t.Errorf("cached = %d ok=%v, want 30/true", cached, ok)
	}
}

func TestParseNonStreamingUsageDeepSeekCacheField(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":40}}`)
	_, _, cached, ok := parseNonStreamingUsage(body)
	if !ok || cached != 40 {
		t.Errorf("cached = %d ok=%v, want 40/true", cached, ok)
	}
}

func TestParseNonStreamingUsageAbsent(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)
	_, _, _, ok := parseNonStreamingUsage(body)
	if ok {
		t.Error("ok = true with no usage object present")
	}
}

func TestParseNonStreamingUsageMalformed(t *testing.T) {
	_, _, _, ok := parseNonStreamingUsage([]byte(`not json`))
	if ok {
		t.Error("ok = true for malformed JSON")
	}
}

func TestParseStreamingUsage(t *testing.T) {
	tail := "data: {\"choices\":[{\"delta\":{}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":10}}\n\n" +
		"data: [DONE]\n\n"
	prompt, completion, _, ok := parseStreamingUsage([]byte(tail))
	if !ok || prompt != 50 || completion != 10 {
		t.Errorf("got (%d,%d,ok=%v), want (50,10,true)", prompt, completion, ok)
	}
}

func TestParseStreamingUsageAbsent(t *testing.T) {
	tail := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	_, _, _, ok := parseStreamingUsage([]byte(tail))
	if ok {
		t.Error("ok = true with no usage chunk in the tail")
	}
}

func TestParseStreamingUsagePicksLastUsageFrame(t *testing.T) {
	// Some providers emit a zero-usage frame mid-stream before the real
	// trailing one — scanning backwards must find the last (real) one, not
	// stop at whichever comes first.
	tail := "data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":75,\"completion_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"
	prompt, completion, _, ok := parseStreamingUsage([]byte(tail))
	if !ok || prompt != 75 || completion != 15 {
		t.Errorf("got (%d,%d,ok=%v), want (75,15,true)", prompt, completion, ok)
	}
}

// ── computeCostNative ──────────────────────────────────────────────────────

func TestComputeCostNativeNoOffering(t *testing.T) {
	_, ok := computeCostNative(ResolvedBackend{}, 1000, 500, 0)
	if ok {
		t.Error("ok = true with no PriceCurrency (no offering matched)")
	}
}

func TestComputeCostNativeBasic(t *testing.T) {
	rb := ResolvedBackend{PriceInPer1M: 1.0, PriceOutPer1M: 2.0, PriceCurrency: "USD"}
	cost, ok := computeCostNative(rb, 1_000_000, 500_000, 0)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	want := 1.0 + 1.0 // 1M in @ $1/1M + 0.5M out @ $2/1M
	if cost != want {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}

func TestComputeCostNativeCachedTokensUpperBoundWhenUnmodelled(t *testing.T) {
	rb := ResolvedBackend{PriceInPer1M: 1.0, PriceOutPer1M: 2.0, PriceCurrency: "USD"}
	cost, ok := computeCostNative(rb, 1_000_000, 0, 500_000) // half the input was cached
	if !ok {
		t.Fatal("ok = false")
	}
	// No PriceCachedInPer1M modelled -> cached tokens priced at the full
	// input rate too (upper bound, never an under-estimate).
	want := 1.0
	if cost != want {
		t.Errorf("cost = %v, want %v (cached tokens at full input rate)", cost, want)
	}
}

func TestComputeCostNativeCachedTokensDiscounted(t *testing.T) {
	cachedRate := 0.1
	rb := ResolvedBackend{PriceInPer1M: 1.0, PriceOutPer1M: 2.0, PriceCurrency: "USD", PriceCachedInPer1M: &cachedRate}
	cost, ok := computeCostNative(rb, 1_000_000, 0, 500_000)
	if !ok {
		t.Fatal("ok = false")
	}
	// 500k billable @ $1/1M + 500k cached @ $0.1/1M
	want := 0.5 + 0.05
	if cost != want {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}

// ── usageTap ────────────────────────────────────────────────────────────────

func TestUsageTapNonStreamingPassesThroughAndCapturesAll(t *testing.T) {
	src := "hello world, this is the full response body"
	var captured []byte
	tap := newUsageTap(io.NopCloser(strings.NewReader(src)), false, func(buf []byte) {
		captured = append([]byte(nil), buf...)
	})
	got, err := io.ReadAll(tap)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != src {
		t.Errorf("tap altered the bytes passed through: got %q, want %q", got, src)
	}
	if err := tap.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(captured) != src {
		t.Errorf("captured = %q, want %q", captured, src)
	}
}

func TestUsageTapStreamingKeepsOnlyRollingTail(t *testing.T) {
	// A body far larger than the streaming cap must still pass through
	// byte-for-byte, while the tap only ever retains the trailing window.
	big := strings.Repeat("A", streamingUsageTailCap*3)
	suffix := "TRAILING-MARKER"
	src := big + suffix
	var captured []byte
	tap := newUsageTap(io.NopCloser(strings.NewReader(src)), true, func(buf []byte) {
		captured = append([]byte(nil), buf...)
	})
	got, err := io.ReadAll(tap)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got2 := string(got); got2 != src {
		t.Fatalf("streaming tap altered pass-through bytes (len got=%d want=%d)", len(got2), len(src))
	}
	tap.Close()
	if len(captured) > streamingUsageTailCap {
		t.Errorf("captured tail length = %d, want <= %d (cap)", len(captured), streamingUsageTailCap)
	}
	if !strings.HasSuffix(string(captured), suffix) {
		t.Errorf("captured tail lost the trailing marker: %q", string(captured[max(0, len(captured)-40):]))
	}
}

func TestUsageTapCloseOnlyFiresOnce(t *testing.T) {
	calls := 0
	tap := newUsageTap(io.NopCloser(strings.NewReader("x")), false, func([]byte) { calls++ })
	tap.Close()
	tap.Close()
	if calls != 1 {
		t.Errorf("onClose called %d times, want 1", calls)
	}
}

// ── integration: recording through the real proxy path ─────────────────────

// remoteTestDeps wires Compressor off the SAME real store as usage (when
// usage is non-nil) — production always does (cmd/forge/main.go passes
// one *store.DB to both), and since the 0042 surrogate-key migration
// usage_events.provider_id is a real FK resolved via a LEFT JOIN against
// router_providers in that same database. A fake in-memory Compressor (the
// pre-0042 shape of this helper) can no longer round-trip a resolvable
// ProviderName: the join would run against usage's own (empty) table.
// usage==nil (the one "Deps.Usage unwired" test) still needs a real,
// separately-opened store so routing/credential resolution works.
func remoteTestDeps(t *testing.T, upstreamURL string, usage *store.DB) Deps {
	t.Helper()
	compressorDB := usage
	if compressorDB == nil {
		var err error
		compressorDB, err = store.Open(":memory:")
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { compressorDB.Close() })
	}
	if err := compressorDB.Routing().SaveProvider(context.Background(), store.ProviderRow{
		Name: "testprov", APIKey: "sk-test", TargetURL: upstreamURL, BillCurrency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	cfg := testCfg(
		[]Backend{{
			Name: "m", Kind: "remote", BaseURL: upstreamURL + "/v1", WireModel: "wire-m",
			Credential: "testprov", PriceInPer1M: 1.0, PriceOutPer1M: 2.0, PriceCurrency: "USD",
		}},
		[]Route{{Model: "m", Primary: "m"}},
	)
	var usageIface store.Usage
	if usage != nil {
		usageIface = usage.Usage()
	}
	return Deps{
		Cfg: cfg, Routing: compressorDB.Routing(), Auth: &stubAuth{validToken: "x"}, Usage: usageIface,
	}
}

func openTestUsageDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// awaitEvents polls until at least want usage events are recorded. The
// usage tap's onClose fires when the SERVER closes the upstream body, which
// can legitimately happen a beat after the client's resp.Body.Close()
// returns — asserting on a single immediate read is racy (seen live as a
// flaky "events = 0, want 1").
func awaitEvents(t *testing.T, usage store.Usage, want int) []store.UsageEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := usage.Events(context.Background(), time.Now().Add(-time.Minute), 10)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(events) >= want {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %d after 3s, want %d: %+v", len(events), want, events)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChatCompletions_NonStreamingRecordsUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
			"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 200},
		})
	}))
	defer upstream.Close()

	usage := openTestUsageDB(t)
	deps := remoteTestDeps(t, upstream.URL, usage)
	srv := httptest.NewServer(NewWithDeps(deps).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	events := awaitEvents(t, usage.Usage(), 1)
	e := events[0]
	if e.Kind != "external_request" || e.ProviderName != "testprov" || e.Model != "wire-m" {
		t.Errorf("event = %+v, want kind=external_request provider=testprov model=wire-m", e)
	}
	if e.PromptTokens != 1000 || e.CompletionTokens != 200 {
		t.Errorf("tokens = (%d,%d), want (1000,200)", e.PromptTokens, e.CompletionTokens)
	}
	if e.Unmetered {
		t.Error("Unmetered = true, want false (usage was present)")
	}
	if e.CostNative == nil {
		t.Fatal("CostNative = nil, want populated")
	}
	wantCost := 1000.0/1e6*1.0 + 200.0/1e6*2.0
	if *e.CostNative != wantCost {
		t.Errorf("CostNative = %v, want %v", *e.CostNative, wantCost)
	}
	if e.CostCurrency != "USD" {
		t.Errorf("CostCurrency = %q, want USD", e.CostCurrency)
	}
}

func TestChatCompletions_NonStreamingUnmeteredWhenUsageAbsent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
		})
	}))
	defer upstream.Close()

	usage := openTestUsageDB(t)
	deps := remoteTestDeps(t, upstream.URL, usage)
	srv := httptest.NewServer(NewWithDeps(deps).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer x")
	resp, _ := http.DefaultClient.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	events := awaitEvents(t, usage.Usage(), 1)
	if !events[0].Unmetered {
		t.Error("Unmetered = false, want true (no usage object in the response)")
	}
	if events[0].CostNative != nil {
		t.Errorf("CostNative = %v, want nil for an unmetered row", *events[0].CostNative)
	}
}

func TestChatCompletions_4xxDoesNotRecordUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer upstream.Close()

	usage := openTestUsageDB(t)
	deps := remoteTestDeps(t, upstream.URL, usage)
	srv := httptest.NewServer(NewWithDeps(deps).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer x")
	resp, _ := http.DefaultClient.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	events, _ := usage.Usage().Events(context.Background(), time.Now().Add(-time.Minute), 10)
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0 (4xx must not record usage)", len(events))
	}
}

func TestChatCompletions_StreamingRemoteRecordsUsageWithoutBuffering(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":40}}\n\n",
		"data: [DONE]\n\n",
	}
	chunkDelay := 80 * time.Millisecond
	var sawStreamOptions bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if so, ok := body["stream_options"].(map[string]any); ok && so["include_usage"] == true {
			sawStreamOptions = true
		}
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

	usage := openTestUsageDB(t)
	deps := remoteTestDeps(t, upstream.URL, usage)
	srv := httptest.NewServer(NewWithDeps(deps).Handler())
	defer srv.Close()

	start := time.Now()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var arrivals []time.Time
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 && bytes.Contains(buf[:n], []byte("data:")) {
			arrivals = append(arrivals, time.Now())
		}
		if err != nil {
			break
		}
	}

	if !sawStreamOptions {
		t.Error("stream_options.include_usage was not injected into the upstream request")
	}
	if len(arrivals) < 2 {
		t.Fatalf("only %d reads observed data, want >=2 (streaming may be buffered)", len(arrivals))
	}
	if firstToLast := arrivals[len(arrivals)-1].Sub(arrivals[0]); firstToLast < chunkDelay {
		t.Errorf("chunks arrived within %v — likely buffered by the usage tap; want >= %v gap", firstToLast, chunkDelay)
	}
	if elapsed := time.Since(start); elapsed < 2*chunkDelay {
		t.Errorf("total elapsed %v — response may have been buffered", elapsed)
	}

	events := awaitEvents(t, usage.Usage(), 1)
	if events[0].PromptTokens != 300 || events[0].CompletionTokens != 40 {
		t.Errorf("streaming-parsed tokens = (%d,%d), want (300,40)", events[0].PromptTokens, events[0].CompletionTokens)
	}
}

func TestChatCompletions_NoUsageDepsSkipsRecordingSilently(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 5}})
	}))
	defer upstream.Close()

	deps := remoteTestDeps(t, upstream.URL, nil) // Usage nil
	srv := httptest.NewServer(NewWithDeps(deps).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (must proxy normally with Usage nil): %s", resp.StatusCode, body)
	}
}
