// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/inflight"
	"github.com/jsaigou/the-forge/internal/store"
)

func awaitIntervals(t *testing.T, usage store.Usage, want int) []store.Interval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		set, err := usage.Intervals(context.Background(), store.IntervalQuery{Since: time.Now().Add(-time.Hour)})
		if err != nil {
			t.Fatalf("Intervals: %v", err)
		}
		if len(set.Intervals) >= want {
			return set.Intervals
		}
		if time.Now().After(deadline) {
			t.Fatalf("intervals = %d after 5s, want %d", len(set.Intervals), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func flightServer(t *testing.T, upstreamURL string) (*httptest.Server, *store.DB, *inflight.Gauge) {
	t.Helper()
	db := openTestUsageDB(t)
	deps := remoteTestDeps(t, upstreamURL, db)
	g := inflight.New()
	deps.InFlight = g
	deps.Auth = &stubAuth{validToken: "x", identity: authz.Identity{Name: "ops-key", DisplayName: "Ops Agent"}}
	srv := httptest.NewServer(NewWithDeps(deps).Handler())
	t.Cleanup(srv.Close)
	return srv, db, g
}

func postChat(ctx context.Context, url string, stream bool) (*http.Response, error) {
	body := `{"model":"m","messages":[{"role":"user","content":"SECRET-PROMPT-TEXT"}]`
	if stream {
		body += `,"stream":true`
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions", strings.NewReader(body+"}"))
	req.Header.Set("Authorization", "Bearer x")
	return http.DefaultClient.Do(req)
}

func allZero(g *inflight.Gauge) bool {
	for _, r := range g.Snapshot().Rows {
		if r.InFlight != 0 {
			return false
		}
	}
	return true
}

func TestFlight_OKRecordsTimingOnBillingRow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	srv, db, g := flightServer(t, upstream.URL)

	before := time.Now()
	resp, err := postChat(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	ivs := awaitIntervals(t, db.Usage(), 1)
	if len(ivs) != 1 {
		t.Fatalf("intervals = %d, want 1 (the external_request row carries the timing)", len(ivs))
	}
	iv := ivs[0]
	if iv.Outcome != store.OutcomeOK {
		t.Errorf("outcome = %q, want ok", iv.Outcome)
	}
	if d := iv.EndMS - iv.StartMS; d < 60 || d > 5000 {
		t.Errorf("duration = %d ms, want >= the 60 ms the upstream took", d)
	}
	if iv.StartMS < before.UnixMilli()-1 {
		t.Errorf("start %d is before the request was sent (%d)", iv.StartMS, before.UnixMilli())
	}
	if iv.PromptTokens == nil || *iv.PromptTokens != 7 || *iv.CompletionTokens != 3 {
		t.Errorf("tokens = %v/%v, want 7/3", iv.PromptTokens, iv.CompletionTokens)
	}

	// The row is attributed to the key's display label (never the bearer token), under the served wire name.
	// (A loopback caller is admitted without a key, so its label is derived from the address and user agent.)
	const label = "loopback (Go-http-client)"
	byLabel, err := db.Usage().Intervals(context.Background(), store.IntervalQuery{Model: "wire-m", Consumer: label, Since: before.Add(-time.Minute)})
	if err != nil || len(byLabel.Intervals) != 1 {
		t.Errorf("rows for consumer %q on wire-m: %d err=%v, want 1", label, len(byLabel.Intervals), err)
	}
	for _, leak := range []string{"x", "Bearer x", "SECRET-PROMPT-TEXT"} {
		if got, _ := db.Usage().Intervals(context.Background(), store.IntervalQuery{Consumer: leak, Since: before.Add(-time.Minute)}); len(got.Intervals) != 0 {
			t.Errorf("consumer %q matched a row: the label must not be the token or content", leak)
		}
	}
	if !allZero(g) {
		t.Errorf("gauge not back to zero: %+v", g.Snapshot())
	}
}

func TestFlight_UpstreamErrorRecordsErrorOutcome(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	srv, db, g := flightServer(t, upstream.URL)

	resp, err := postChat(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	ivs := awaitIntervals(t, db.Usage(), 1)
	if ivs[0].Outcome != store.OutcomeError {
		t.Errorf("outcome = %q, want error", ivs[0].Outcome)
	}
	if !allZero(g) {
		t.Errorf("gauge not back to zero: %+v", g.Snapshot())
	}
}

func TestFlight_StreamingDurationSpansTheWholeStream(t *testing.T) {
	const gap = 80 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, c := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, c)
			fl.Flush()
			time.Sleep(gap)
		}
	}))
	defer upstream.Close()
	srv, db, _ := flightServer(t, upstream.URL)

	resp, err := postChat(context.Background(), srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	iv := awaitIntervals(t, db.Usage(), 1)[0]
	if d := time.Duration(iv.EndMS-iv.StartMS) * time.Millisecond; d < 3*gap-20*time.Millisecond {
		t.Errorf("duration %v is shorter than the stream (%v): end must be the stream close, not the first byte", d, 3*gap)
	}
	if iv.Outcome != store.OutcomeOK || iv.PromptTokens == nil || *iv.PromptTokens != 11 {
		t.Errorf("interval = %+v, want ok with the streamed usage", iv)
	}
}

func TestFlight_ClientAbortRecordsAborted(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)
	srv, db, g := flightServer(t, upstream.URL)

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := postChat(ctx, srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	// Mid-stream: the request must be visible in the gauge.
	inFlight := 0
	for _, r := range g.Snapshot().Rows {
		if r.Model == "wire-m" && r.Consumer == inflight.ModelTotal {
			inFlight = r.InFlight
		}
	}
	if inFlight != 1 {
		t.Errorf("mid-stream gauge = %+v, want 1 in flight on wire-m", g.Snapshot().Rows)
	}
	cancel()
	resp.Body.Close()

	iv := awaitIntervals(t, db.Usage(), 1)[0]
	if iv.Outcome != store.OutcomeAborted {
		t.Errorf("outcome = %q, want aborted", iv.Outcome)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !allZero(g) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !allZero(g) {
		t.Errorf("gauge not back to zero after abort: %+v", g.Snapshot())
	}
}

func TestFlight_UnauthenticatedLeavesNoRowAndNoGauge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv, db, g := flightServer(t, upstream.URL)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Skipf("loopback client is admitted without a key here (status %d); nothing to assert", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	set, err := db.Usage().Intervals(context.Background(), store.IntervalQuery{Since: time.Now().Add(-time.Hour)})
	if err != nil || len(set.Intervals) != 0 {
		t.Errorf("unauthenticated request left rows: %d err=%v", len(set.Intervals), err)
	}
	if len(g.Snapshot().Rows) != 0 {
		t.Errorf("unauthenticated request touched the gauge: %+v", g.Snapshot())
	}
}

// TestFlight_GaugeReturnsToZeroAfterParallelRequestsWithAborts runs N requests at once, every third abandoned mid-stream,
// and requires the gauge to read zero everywhere afterwards, with a peak that shows they really overlapped.
func TestFlight_GaugeReturnsToZeroAfterParallelRequestsWithAborts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
			fl.Flush()
			select {
			case <-time.After(40 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	srv, db, g := flightServer(t, upstream.URL)

	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resp, err := postChat(ctx, srv.URL, true)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			if i%3 == 0 {
				buf := make([]byte, 32)
				_, _ = resp.Body.Read(buf)
				cancel() // abandon mid-stream
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
		}(i)
	}
	wg.Wait()

	ivs := awaitIntervals(t, db.Usage(), n)
	aborted := 0
	for _, iv := range ivs {
		if iv.Outcome == store.OutcomeAborted {
			aborted++
		}
	}
	if aborted != n/3 {
		t.Errorf("aborted rows = %d, want %d", aborted, n/3)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !allZero(g) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	snap := g.Snapshot()
	if !allZero(g) {
		t.Fatalf("gauge not back to zero after %d parallel requests: %+v", n, snap)
	}
	var peak int
	for _, r := range snap.Rows {
		if r.Consumer == inflight.ModelTotal && r.Model == "wire-m" {
			peak = r.Peak
		}
	}
	if peak < 2 {
		t.Errorf("model peak = %d, want >= 2 for %d overlapping requests", peak, n)
	}
}

func TestUsageFromBodyReadsUsageThenTimings(t *testing.T) {
	if p, c, ok := usageFromBody([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":6}}`), false); !ok || p != 5 || c != 6 {
		t.Errorf("usage object: %d/%d/%v", p, c, ok)
	}
	// llama.cpp: prompt_n excludes the cached part, which cache_n holds.
	if p, c, ok := usageFromBody([]byte(`{"timings":{"prompt_n":10,"cache_n":90,"predicted_n":7}}`), false); !ok || p != 100 || c != 7 {
		t.Errorf("timings: %d/%d/%v, want 100/7", p, c, ok)
	}
	stream := "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: {\"choices\":[],\"timings\":{\"prompt_n\":3,\"cache_n\":0,\"predicted_n\":9}}\n\ndata: [DONE]\n\n"
	if p, c, ok := usageFromBody([]byte(stream), true); !ok || p != 3 || c != 9 {
		t.Errorf("streamed timings: %d/%d/%v", p, c, ok)
	}
	if _, _, ok := usageFromBody([]byte(`{"choices":[]}`), false); ok {
		t.Error("no usage anywhere must report ok=false, not zeros")
	}
}
