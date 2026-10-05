// SPDX-License-Identifier: Apache-2.0

package router

// ctxledger_hook_test.go — WS-N1: drives the REAL a0 handler against a fake
// local-slot upstream emitting llama.cpp-style timings and checks that the
// observe-only creation ledger (a) measures correctly, (b) reads cache reuse
// from non-stream bodies and the final SSE chunk, (c) never changes what the
// upstream receives or the client gets, and (d) never buffers a stream.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/ctxledger"
	"github.com/jsaigou/the-forge/internal/store"
)

type slotFake struct {
	mu       sync.Mutex
	received [][]byte
	cacheN   []int // per request
	promptN  []int
	delay    time.Duration
}

func (f *slotFake) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.received = append(f.received, b)
	i := len(f.received) - 1
	c, p := 0, 100
	if i < len(f.cacheN) {
		c, p = f.cacheN[i], f.promptN[i]
	}
	f.mu.Unlock()
	var req struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(b, &req)
	timings := fmt.Sprintf(`"timings":{"cache_n":%d,"prompt_n":%d,"predicted_n":4}`, c, p)
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],%s}`, timings)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	for _, ch := range []string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n",
		`data: {"choices":[{"finish_reason":"stop","delta":{}}],` + timings + "}\n\n",
		"data: [DONE]\n\n",
	} {
		io.WriteString(w, ch)
		fl.Flush()
		time.Sleep(f.delay)
	}
}

func ledgerRig(t *testing.T, f *slotFake) (*httptest.Server, *ctxledger.Ledger, *store.DB) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(up.Close)
	port := portFromURL(t, up.URL)
	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/models/test.gguf"})
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	led := ctxledger.New(ctxledger.Config{Sink: db.ContextCreation()})
	led.Start(context.Background())
	t.Cleanup(led.Close)
	srv := httptest.NewServer(NewWithDeps(Deps{
		Cfg:       testCfg([]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}}, []Route{{Model: "m", Primary: "a1"}}),
		Catalog:   cat,
		Auth:      &stubAuth{validToken: "x", identity: authz.Identity{DisplayName: "opencode"}},
		CtxLedger: led,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, led, db
}

func post(t *testing.T, srv *httptest.Server, body []byte) []byte {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	return out
}

type m = map[string]any

func chatBody(stream bool, msgs ...m) []byte {
	b, _ := json.Marshal(m{"model": "m", "stream": stream, "messages": msgs,
		"tools": []any{m{"type": "function", "function": m{"name": "web_fetch_mcp_webfetch"}}}})
	return b
}

func TestLedgerRealHandlerMultiTurn(t *testing.T) {
	f := &slotFake{cacheN: []int{0, 990, 990, 5}, promptN: []int{1000, 10, 10, 995}}
	srv, led, _ := ledgerRig(t, f)

	u1 := m{"role": "user", "content": "fetch the page"}
	a1 := m{"role": "assistant", "content": "", "tool_calls": []any{m{"id": "c1", "type": "function",
		"function": m{"name": "web_fetch_mcp_webfetch", "arguments": `{"url":"https://x"}`}}}}
	tr := m{"role": "tool", "tool_call_id": "c1", "content": strings.Repeat("p", 12000)}
	u2 := m{"role": "user", "content": "summarize"}

	post(t, srv, chatBody(false, u1))             // turn 1
	post(t, srv, chatBody(true, u1, a1, tr))      // turn 2 (stream): new = a1, tr
	post(t, srv, chatBody(false, u1, a1, tr, u2)) // turn 3: new = [assistant? none] -> hmm u2 only
	edited := m{"role": "tool", "tool_call_id": "c1", "content": "[trimmed by client]"}
	post(t, srv, chatBody(false, u1, a1, edited, u2)) // turn 4: history edited
	time.Sleep(100 * time.Millisecond)                // let reuse events (async) enqueue

	res, ok, err := led.Query(context.Background(), "24h", "tool")
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("by=tool:\n%s", out)
	resC, _, _ := led.Query(context.Background(), "24h", "consumer")
	outC, _ := json.MarshalIndent(resC, "", "  ")
	t.Logf("by=consumer:\n%s", outC)

	var web *ctxledger.Row
	for i := range res.Rows {
		if res.Rows[i].Key == "web_fetch" {
			web = &res.Rows[i]
		}
	}
	if web == nil {
		t.Fatalf("no web_fetch row: %s", out)
	}
	// turn 2: 12000 new; turn 4: edited history => whole body new, 19 chars.
	if web.CreatedChars != 12000+19 || web.Big8k != 1 || web.MaxResultChars != 12000 {
		t.Fatalf("web_fetch row: %+v", *web)
	}
	if res.CacheBustSuspects != 1 || res.TotalRequests != 4 {
		t.Fatalf("suspects=%d requests=%d", res.CacheBustSuspects, res.TotalRequests)
	}
	if res.ReuseSamples != 4 || res.ReuseP50 == nil {
		t.Fatalf("reuse samples=%d p50=%v (timings not read from body/final SSE chunk)", res.ReuseSamples, res.ReuseP50)
	}
	if len(resC.Rows) != 1 || !strings.HasPrefix(resC.Rows[0].Key, "loopback") {
		t.Fatalf("consumer rows: %+v", resC.Rows)
	}
	// P0: the upstream received the same messages the client sent.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) != 4 {
		t.Fatalf("upstream got %d requests", len(f.received))
	}
	for i, want := range [][]byte{chatBody(false, u1), chatBody(true, u1, a1, tr), chatBody(false, u1, a1, tr, u2), chatBody(false, u1, a1, edited, u2)} {
		var g, w struct{ Messages json.RawMessage }
		json.Unmarshal(f.received[i], &g)
		json.Unmarshal(want, &w)
		var gi, wi any
		json.Unmarshal(g.Messages, &gi)
		json.Unmarshal(w.Messages, &wi)
		gb, _ := json.Marshal(gi)
		wb, _ := json.Marshal(wi)
		if !bytes.Equal(gb, wb) {
			t.Fatalf("request %d messages changed in flight", i)
		}
	}
}

// T3: with the ledger tap installed, SSE chunks still reach the client as the
// upstream emits them (same shape as TestChatCompletions_StreamingNoBuffering).
func TestLedgerStreamingStillUnbuffered(t *testing.T) {
	f := &slotFake{delay: 120 * time.Millisecond}
	srv, led, _ := ledgerRig(t, f)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		bytes.NewReader(chatBody(true, m{"role": "user", "content": "hi"})))
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	var arrivals []time.Time
	for {
		line, err := rd.ReadBytes('\n')
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			arrivals = append(arrivals, time.Now())
		}
		if err != nil {
			break
		}
	}
	if len(arrivals) < 3 {
		t.Fatalf("only %d chunks", len(arrivals))
	}
	if span := arrivals[len(arrivals)-1].Sub(arrivals[0]); span < 200*time.Millisecond {
		t.Fatalf("chunks arrived together over %v: stream was buffered", span)
	}
	time.Sleep(100 * time.Millisecond)
	led.Flush(context.Background())
	if led.Stats().Observed != 1 {
		t.Fatalf("observed=%d", led.Stats().Observed)
	}
}

// T5 at the handler level: a dead ledger sink never changes the response.
func TestLedgerDownDoesNotAffectResponse(t *testing.T) {
	f := &slotFake{}
	up := httptest.NewServer(http.HandlerFunc(f.handler))
	defer up.Close()
	port := portFromURL(t, up.URL)
	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/models/test.gguf"})
	mk := func(led *ctxledger.Ledger) *httptest.Server {
		return httptest.NewServer(NewWithDeps(Deps{
			Cfg:     testCfg([]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}}, []Route{{Model: "m", Primary: "a1"}}),
			Catalog: cat, Auth: &stubAuth{validToken: "x"}, CtxLedger: led,
		}).Handler())
	}
	bad := ctxledger.New(ctxledger.Config{Sink: failingSink{}})
	bad.Start(context.Background())
	defer bad.Close()
	s1, s0 := mk(bad), mk(nil)
	defer s1.Close()
	defer s0.Close()
	b := chatBody(false, m{"role": "user", "content": "hi"})
	if got, want := post(t, s1, b), post(t, s0, b); !bytes.Equal(got, want) {
		t.Fatalf("response differs with a failing ledger:\n%s\n%s", got, want)
	}
}

type failingSink struct{}

func (failingSink) Upsert(context.Context, []store.ContextCreationRow) error {
	panic("sink exploded")
}
func (failingSink) Since(context.Context, int64) ([]store.ContextCreationRow, error) { return nil, nil }
func (failingSink) Prune(context.Context, int64) (int64, error)                      { return 0, nil }
