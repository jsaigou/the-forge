// SPDX-License-Identifier: Apache-2.0

package router

// Radiance (engine kind "radiance") against a0: slot probe without /props, busy series, ledger reuse from OpenAI usage, and 429/503 handling.
// The response shapes follow core/server/oai.cpp (usage_json, usage_chunk), admin.cpp (/v1/models capabilities, /health) and
// http.cpp/server.cpp (429 with Retry-After when the admission queue is full).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/ctxledger"
	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/store"
)

const radModels = `{"object":"list","data":[{"id":"qwen35moe","object":"model","owned_by":"radiance","max_model_len":262144,` +
	`"capabilities":{"chat":true,"tools":true,"reasoning":true,"decision":null,"reasoning_efforts":null}}]}`

func radianceSpecOf(int) enginekind.Spec {
	s, _ := enginekind.Lookup(enginekind.Radiance)
	return s
}

func TestProbeSlotRadianceFallsBackToModels(t *testing.T) {
	var propsHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"ok","model":"qwen35moe","in_flight":0,"queue_cap":0}`)
		case "/v1/models":
			fmt.Fprint(w, radModels)
		case "/props":
			propsHits.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	port := portFromURL(t, up.URL)

	cat := newTTLCatalog(nil)
	cat.specOf = radianceSpecOf
	p := cat.Probe(port, time.Millisecond)
	if !p.Healthy || p.NCtx != 262144 || p.ModelPath != "qwen35moe" || !p.SupportsTools {
		t.Fatalf("probe = %+v", p)
	}
	if propsHits.Load() != 0 {
		t.Errorf("/props asked %d times for a kind that does not serve it", propsHits.Load())
	}

	// Without a lookup the slot is llama.cpp: /props is used, and a missing one leaves the probe healthy but empty (unchanged behaviour).
	plain := newTTLCatalog(nil)
	if p := plain.Probe(port, time.Millisecond); !p.Healthy || p.NCtx != 0 {
		t.Fatalf("llama.cpp-default probe = %+v", p)
	}
	if propsHits.Load() == 0 {
		t.Error("llama.cpp path no longer reads /props")
	}
}

func TestProbeSlotRadianceModelsDownStaysHealthy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, `{"status":"ok"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	cat := newTTLCatalog(nil)
	cat.specOf = radianceSpecOf
	if p := cat.Probe(portFromURL(t, up.URL), time.Millisecond); !p.Healthy || p.NCtx != 0 || p.SupportsTools {
		t.Fatalf("probe = %+v", p)
	}
}

func TestSlotHealthyRejectsRadianceStuckAndStopping(t *testing.T) {
	for _, status := range []string{"stuck", "stopping"} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
			fmt.Fprintf(w, `{"status":%q,"model":"m","in_flight":1,"queue_cap":0}`, status)
		}))
		cat := newTTLCatalog(nil)
		cat.specOf = radianceSpecOf
		if p := cat.Probe(portFromURL(t, up.URL), time.Millisecond); p.Healthy {
			t.Errorf("%s slot reported healthy", status)
		}
		up.Close()
	}
}

func TestProbeBusySelectsSeriesByKind(t *testing.T) {
	// Busy in Radiance's terms (vllm series 2) and idle in llama.cpp's (series absent), and the reverse.
	radBusy := `vllm:num_requests_running{model_name="m",engine="0"} 2` + "\n"
	llamaBusy := "llamacpp:requests_processing 1\n"
	serve := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	}
	rb, lb, idle := serve(radBusy), serve(llamaBusy), serve(`vllm:num_requests_running{model_name="m",engine="0"} 0`+"\n")
	defer rb.Close()
	defer lb.Close()
	defer idle.Close()

	rad := newTTLCatalog(nil)
	rad.specOf = radianceSpecOf
	if !rad.IsBusy(portFromURL(t, rb.URL), time.Millisecond) {
		t.Error("a busy Radiance slot looks idle")
	}
	if rad.IsBusy(portFromURL(t, idle.URL), time.Millisecond) {
		t.Error("an idle Radiance slot looks busy")
	}
	if rad.IsBusy(portFromURL(t, lb.URL), time.Millisecond) {
		t.Error("a radiance slot must not read the llamacpp series")
	}
	llama := newTTLCatalog(nil)
	if !llama.IsBusy(portFromURL(t, lb.URL), time.Millisecond) {
		t.Error("llama.cpp busy series no longer read")
	}
	if llama.IsBusy(portFromURL(t, rb.URL), time.Millisecond) {
		t.Error("a llama.cpp slot must not read the vllm series")
	}
}

func TestSlotSpecFromDeps(t *testing.T) {
	s := NewWithDeps(Deps{SlotKind: func(port int) string {
		switch port {
		case 1:
			return enginekind.Radiance
		case 2:
			return "no-such-kind"
		}
		return ""
	}})
	if got := s.slotSpec(1).Name; got != enginekind.Radiance {
		t.Errorf("port 1 = %q", got)
	}
	for _, port := range []int{2, 3} {
		if got := s.slotSpec(port).Name; got != enginekind.LlamaCpp {
			t.Errorf("port %d = %q, want llama.cpp default", port, got)
		}
	}
	if got := NewWithDeps(Deps{}).slotSpec(1).Name; got != enginekind.LlamaCpp {
		t.Errorf("no lookup = %q", got)
	}
}

// radianceLedgerRig is ledgerRig with the slot declared a Radiance one.
func radianceLedgerRig(t *testing.T, h http.HandlerFunc) (*httptest.Server, *ctxledger.Ledger) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	port := portFromURL(t, up.URL)
	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "qwen35moe"})
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
		SlotKind:  func(int) string { return enginekind.Radiance },
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, led
}

func TestLedgerRadianceReuseFromUsage(t *testing.T) {
	// Request 1 is non-stream: 1536 of 2048 prompt tokens cached. Request 2 streams with include_usage: 768 of 1000.
	// Request 3 streams WITHOUT usage: unmetered, must add no sample.
	var n atomic.Int32
	h := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch n.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],`+
				`"usage":{"prompt_tokens":2048,"completion_tokens":2,"total_tokens":2050,"prompt_tokens_details":{"cached_tokens":1536}}}`)
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":null}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":1,\"total_tokens\":1001,"+
				"\"prompt_tokens_details\":{\"cached_tokens\":768}}}\n\ndata: [DONE]\n\n")
		default:
			if bytes.Contains(body, []byte("include_usage")) {
				t.Error("test premise: request 3 must not ask for usage")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n")
		}
	}
	srv, led := radianceLedgerRig(t, h)
	u := m{"role": "user", "content": "hi"}
	post(t, srv, chatBody(false, u))
	post(t, srv, chatBody(true, u))
	post(t, srv, chatBody(true, u))
	time.Sleep(150 * time.Millisecond)
	res, ok, err := led.Query(context.Background(), "24h", "consumer")
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	if res.TotalRequests != 3 {
		t.Fatalf("requests = %d", res.TotalRequests)
	}
	if res.ReuseSamples != 2 {
		t.Fatalf("reuse samples = %d, want 2 (the stream without usage is unmetered, not zero)", res.ReuseSamples)
	}
}

// 429 (admission queue full, Retry-After) is an overloaded-but-alive answer: it reaches the client as 429 with its header, the slot takes no
// error mark, and a probe still calls it healthy. A 503 is the opposite (existing behaviour, pinned here beside it).
func TestRadiance429IsNotADeadBackend(t *testing.T) {
	for _, tc := range []struct {
		code     int
		hasError bool // the existing rule counts a 5xx (twice: ModifyResponse and ErrorHandler), a 4xx never
	}{{429, false}, {503, true}} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					fmt.Fprint(w, `{"status":"ok"}`)
					return
				}
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.code)
				fmt.Fprintf(w, `{"error":{"message":"the admission queue is full","type":"rate_limit_error","code":%d}}`, tc.code)
			}))
			defer up.Close()
			port := portFromURL(t, up.URL)
			cat := newFakeCatalog()
			cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "qwen35moe"})
			rs := NewWithDeps(Deps{
				Cfg:      testCfg([]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}}, []Route{{Model: "m", Primary: "a1"}}),
				Catalog:  cat,
				Auth:     &stubAuth{validToken: "x"},
				SlotKind: func(int) string { return enginekind.Radiance },
			})
			srv := httptest.NewServer(rs.Handler())
			defer srv.Close()
			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(chatBody(false, m{"role": "user", "content": "hi"})))
			req.Header.Set("Authorization", "Bearer x")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if tc.code == 429 {
				if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "2" {
					t.Errorf("client saw %d Retry-After=%q, want the engine's own 429", resp.StatusCode, resp.Header.Get("Retry-After"))
				}
			}
			if n, _ := rs.SlotErrorCount(port, 60); (n > 0) != tc.hasError {
				t.Errorf("slot error count = %d, counted-as-error want %v", n, tc.hasError)
			}
			if tc.code == 429 {
				c := newTTLCatalog(nil)
				c.specOf = radianceSpecOf
				if !c.Probe(port, time.Millisecond).Healthy {
					t.Error("a slot that answers 429 on chat must still probe healthy")
				}
			}
		})
	}
}
