// SPDX-License-Identifier: Apache-2.0

package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// portFromURL extracts the port from an httptest.Server URL.
func portFromURL(t *testing.T, rawurl string) int {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(u.Host)
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// testCfg builds a RouterConfig with defaults applied.
func testCfg(backends []Backend, routes []Route) *RouterConfig {
	cfg := &RouterConfig{Backends: backends, Routes: routes}
	cfg.applyDefaults()
	return cfg
}

// stubAuth is an authz.Authenticator that accepts one valid token.
type stubAuth struct {
	validToken string
	identity   authz.Identity
}

func (s *stubAuth) VerifySession(string) (authz.Identity, error) {
	return s.identity, nil
}

func (s *stubAuth) VerifyBearerFrom(_ context.Context, _, token string, _ authz.KeyKind) (authz.Identity, error) {
	if token == s.validToken {
		return s.identity, nil
	}
	return authz.Identity{}, authz.ErrBadToken
}

// --- /healthz ---

func TestHealthz(t *testing.T) {
	srv := New()
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ok"`) || !strings.Contains(body, `"a0"`) {
		t.Fatalf("healthz body = %q, want ok+a0", body)
	}
}

// --- Auth: tailnet-conditional (the crown-jewels CGNAT/XFF check) ---

func TestCheckAuth_TailnetDirect(t *testing.T) {
	// Direct HTTP from a tailnet peer: remote_addr is the real tailnet IP.
	// No XFF needed. Must bypass the bearer check.
	srv := NewWithDeps(Deps{Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	req.RemoteAddr = "100.100.100.100:54321" // ForgeHost's tailnet IP
	if !srv.checkAuth(req).ok {
		t.Error("tailnet direct request should bypass auth")
	}
}

func TestCheckAuth_TailnetViaTailscaleServe(t *testing.T) {
	// HTTPS via tailscale serve: remote_addr is loopback, real client IP in
	// XFF (which tailscale serve sets trustworthily). Must bypass auth.
	srv := NewWithDeps(Deps{Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "100.100.100.100")
	if !srv.checkAuth(req).ok {
		t.Error("tailnet-via-serve request should bypass auth (loopback + XFF)")
	}
}

func TestCheckAuth_NonTailnetDirect(t *testing.T) {
	// Direct HTTP from a non-tailnet address: must require a valid token.
	srv := NewWithDeps(Deps{Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	req.RemoteAddr = "8.8.8.8:54321"
	if srv.checkAuth(req).ok {
		t.Error("non-tailnet direct request without token should be rejected")
	}
	req.Header.Set("Authorization", "Bearer sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx")
	if !srv.checkAuth(req).ok {
		t.Error("non-tailnet direct request with valid token should pass")
	}
}

func TestCheckAuth_SpoofedXFFRejected(t *testing.T) {
	// A direct non-tailnet connection with a spoofed XFF header MUST NOT
	// bypass auth — XFF is trusted only when remote_addr is loopback.
	srv := NewWithDeps(Deps{Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	req.RemoteAddr = "8.8.8.8:54321"
	req.Header.Set("X-Forwarded-For", "100.100.100.100") // spoofed
	if srv.checkAuth(req).ok {
		t.Error("spoofed XFF from non-loopback must not bypass auth")
	}
}

func TestCheckAuth_LoopbackNoXFF(t *testing.T) {
	// Loopback without XFF (e.g., smith's in-process a0 calls, or a local
	// curl on ForgeHost): smith P3 trusts this explicitly (see checkAuth's
	// doc comment for why this isn't a real widening of the existing trust
	// boundary — EffectiveRemoteAddr already trusts a loopback caller's XFF).
	srv := NewWithDeps(Deps{Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:54321"
	if !srv.checkAuth(req).ok {
		t.Error("loopback without XFF should bypass auth (smith in-process calls)")
	}
}

func TestCheckAuth_NonLoopbackNoXFF_StillRequiresAuth(t *testing.T) {
	// The loopback exemption must not leak to non-loopback callers.
	srv := NewWithDeps(Deps{Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	req.RemoteAddr = "8.8.8.8:54321"
	if srv.checkAuth(req).ok {
		t.Error("non-loopback, non-tailnet request without a token should still require auth")
	}
}

func TestCheckAuth_InvalidBearer(t *testing.T) {
	srv := NewWithDeps(Deps{Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"wrong_scheme", "Basic sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"},
		{"wrong_token", "Bearer sk-router-aaaaaaaaaaaa-wrongwrongwrong"},
		{"malformed", "Bearer not-a-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
			req.RemoteAddr = "8.8.8.8:54321"
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if srv.checkAuth(req).ok {
				t.Errorf("bearer %q should be rejected", tc.name)
			}
		})
	}
}

// --- /v1/models ---

func TestModels_List(t *testing.T) {
	// /v1/models is entirely store-backed post-cutover (TOML decommission
	// Phase 3, docs/v5-toml-decommission.md §6) — a visible catalog Config
	// is listed with its static NCtx, no live slot probe involved.
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "gemma4-26b-mtp", 8192, "visible")

	srv := NewWithDeps(Deps{Cfg: testCfg(nil, nil), StoreCatalog: db.Catalog(), Auth: &stubAuth{validToken: "x"}})

	// Tailnet → bypass auth.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want 200", rec.Code)
	}
	var resp ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("models count = %d, want 1", len(resp.Data))
	}
	if resp.Data[0].ID != "gemma4-26b-mtp" {
		t.Errorf("model id = %q, want gemma4-26b-mtp", resp.Data[0].ID)
	}
	if resp.Data[0].ContextLength != 8192 {
		t.Errorf("context_length = %d, want 8192", resp.Data[0].ContextLength)
	}
}

func TestModels_UnloadedConfigStillListed(t *testing.T) {
	// F1 fix (now via the store-backed local-Config listing, not a live
	// slot probe — ADR-0007 retired RouterConfig.Routes entirely): a
	// configured-but-never-loaded model must still be listed, otherwise a
	// consumer can never discover it to request it and trigger the
	// on-demand load path (see TestChatCompletions_CatalogConfig and
	// TestModels_ThenOnDemandLoad).
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "gemma4-26b-mtp", 0, "visible")

	srv := NewWithDeps(Deps{Cfg: testCfg(nil, nil), StoreCatalog: db.Catalog(), Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var resp ModelsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("unloaded model should still be listed, got %d entries", len(resp.Data))
	}
	if resp.Data[0].ID != "gemma4-26b-mtp" {
		t.Errorf("model id = %q, want gemma4-26b-mtp", resp.Data[0].ID)
	}
}

func TestModels_ThenOnDemandLoad(t *testing.T) {
	// End-to-end F1+F2: a model discovered via /v1/models while unloaded
	// (F1) can then be requested via chat completions, which triggers the
	// scheduler's on-demand load (F2) instead of 404ing — the exact
	// consumer-visible flow BE-1 restores, now via a catalog Config
	// (ADR-0007) instead of a router.toml route.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "gemma4-26b-mtp", 0, "visible")

	loadCalled := false
	stubSched := &fixedSlotSched{
		slot:   "a1",
		onLoad: func() { loadCalled = true },
	}

	srv := NewWithDeps(Deps{
		Cfg: testCfg(nil, nil), StoreCatalog: db.Catalog(), Sched: stubSched,
		Slots: map[string]int{"a1": port},
		Auth:  &stubAuth{validToken: "x"},
	})

	// Step 1: /v1/models must list the model even though it's unloaded.
	listReq := httptest.NewRequest("GET", "/v1/models", nil)
	listReq.RemoteAddr = "100.64.0.1:1234"
	listRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(listRec, listReq)
	var listResp ModelsResponse
	json.Unmarshal(listRec.Body.Bytes(), &listResp)
	if len(listResp.Data) != 1 || listResp.Data[0].ID != "gemma4-26b-mtp" {
		t.Fatalf("/v1/models did not list the unloaded model: %+v", listResp.Data)
	}

	// Step 2: requesting that exact model triggers the on-demand load and
	// is served, not 404ed.
	chatReq := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemma4-26b-mtp","messages":[{"role":"user","content":"hi"}]}`))
	chatReq.RemoteAddr = "100.64.0.1:1234"
	chatRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(chatRec, chatReq)

	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat completions status = %d, want 200 (body: %s)", chatRec.Code, chatRec.Body.String())
	}
	if !loadCalled {
		t.Error("scheduler.EnsureLoaded was not called for the discovered-but-unloaded model")
	}
}

// --- /v1/chat/completions ---

func TestChatCompletions_ModelNotFound(t *testing.T) {
	srv := NewWithDeps(Deps{
		Cfg:  testCfg(nil, nil),
		Auth: &stubAuth{validToken: "x"},
	})
	body := strings.NewReader(`{"model":"nonexistent","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestChatCompletions_ValidationErrors(t *testing.T) {
	srv := NewWithDeps(Deps{
		Cfg:  testCfg([]Backend{{Name: "a1", Kind: "foundry_slot", Port: 8080}}, []Route{{Model: "m", Primary: "a1"}}),
		Auth: &stubAuth{validToken: "x"},
	})
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty_model", `{"model":"","messages":[{"role":"user","content":"hi"}]}`},
		{"model_too_long", `{"model":"` + strings.Repeat("x", 129) + `","messages":[{"role":"user","content":"hi"}]}`},
		{"empty_messages", `{"model":"m","messages":[]}`},
		{"missing_messages", `{"model":"m"}`},
		{"invalid_json", `{not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body))
			req.RemoteAddr = "100.64.0.1:1234"
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestChatCompletions_NonStreaming(t *testing.T) {
	// Upstream returns a non-streaming completion. Verify the router
	// overwrites model=wire_model and passes the response through.
	var receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		receivedModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"` + receivedModel + `","choices":[{"message":{"role":"assistant","content":"4"},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/models/gemma4.gguf"})

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
		[]Route{{Model: "gemma4-26b-mtp", Primary: "a1"}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	reqBody := `{"model":"gemma4-26b-mtp","messages":[{"role":"user","content":"What is 2+2?"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The upstream should have received the wire_model (model_path), not the
	// logical model name. This is the structural fix for the hermes-agent bug.
	if receivedModel != "/models/gemma4.gguf" {
		t.Errorf("upstream received model = %q, want /models/gemma4.gguf", receivedModel)
	}
	// Response passes through untouched.
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "chat.completion" {
		t.Errorf("response object = %v, want chat.completion", resp["object"])
	}
}

// TestChatCompletions_UpstreamPathNotDoubled reproduces a bug found
// live-verifying the Phase 9b production cutover: a real chat completion
// against real llama-server and DeepSeek upstreams both came back 404, with
// the upstream's own "File Not Found" / CloudFront error body — the request
// really left the box, but at the wrong path. The bug: the outbound request
// was built with httputil.ProxyRequest.SetURL(targetURL), which JOINS
// target's path onto the inbound request's path. a0's own mount point for
// this handler is the fixed pattern "POST /v1/chat/completions" (not a
// prefix), so the inbound path is always exactly "/v1/chat/completions" —
// SetURL doubled it to "/v1/chat/completions/v1/chat/completions" on every
// single call. Every prior test's fake upstream used a bare
// http.HandlerFunc as the whole server (no internal path routing), which
// matches any path and so never noticed. This test's upstream asserts on
// the exact received path, the way a real OpenAI-compatible server would.
func TestChatCompletions_UpstreamPathNotDoubled(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","index":0}]}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "gemma4.gguf"})

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
		[]Route{{Model: "gemma4-26b-mtp", Primary: "a1"}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemma4-26b-mtp","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if receivedPath != "/v1/chat/completions" {
		t.Errorf("upstream received path = %q, want /v1/chat/completions (not doubled)", receivedPath)
	}
}

func TestChatCompletions_StreamingNoBuffering(t *testing.T) {
	// CLAUDE.local.md hard requirement: "no buffering of upstream SSE chunks
	// — verify with long generations". Upstream sends chunks with delays;
	// the client must receive them as they arrive, not all at once after
	// the full response is buffered.
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n",
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
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/models/test.gguf"})

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
		[]Route{{Model: "m", Primary: "a1"}},
	)
	// Run the router as a real server so we can use a real HTTP client
	// (httptest.NewRecorder buffers and doesn't test true streaming).
	routerSrv := httptest.NewServer(NewWithDeps(Deps{
		Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"},
	}).Handler())
	defer routerSrv.Close()

	start := time.Now()
	req, _ := http.NewRequest("POST", routerSrv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer x") // non-tailnet loopback → require bearer
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Read chunks incrementally; record arrival times.
	reader := bufio.NewReader(resp.Body)
	var arrivals []time.Time
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			arrivals = append(arrivals, time.Now())
		}
		if err != nil {
			break
		}
	}

	if len(arrivals) < 2 {
		t.Fatalf("only %d data chunks received, want ≥2 (streaming not working)", len(arrivals))
	}
	// If streaming works, the first chunk arrives well before the last.
	// If buffered, all chunks arrive ~simultaneously (gap < chunkDelay/2).
	firstToLast := arrivals[len(arrivals)-1].Sub(arrivals[0])
	if firstToLast < chunkDelay {
		t.Errorf("chunks arrived within %v of each other — likely buffered; want ≥%v gap",
			firstToLast, chunkDelay)
	}
	// Sanity: total time should be at least 2*chunkDelay (3 gaps between 4 chunks).
	if elapsed := time.Since(start); elapsed < 2*chunkDelay {
		t.Errorf("total elapsed %v — response may have been buffered", elapsed)
	}
}

func TestChatCompletions_FailoverOn5xx(t *testing.T) {
	// First backend returns 503; second backend returns 200. Router must
	// fail over and return the 200.
	var hits []string
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits = append(hits, "up1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits = append(hits, "up2")
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer up2.Close()

	cat := newFakeCatalog()
	cat.setProbe(portFromURL(t, up1.URL), SlotProbe{Healthy: true})
	cat.setProbe(portFromURL(t, up2.URL), SlotProbe{Healthy: true})

	cfg := testCfg(
		[]Backend{
			{Name: "a1", Kind: "foundry_slot", Port: portFromURL(t, up1.URL)},
			{Name: "fallback", Kind: "foundry_slot", Port: portFromURL(t, up2.URL)},
		},
		[]Route{{Model: "m", Primary: "a1", Fallback: []string{"fallback"}}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (failover failed)", rec.Code)
	}
	if len(hits) != 2 || hits[0] != "up1" || hits[1] != "up2" {
		t.Errorf("backend hit order = %v, want [up1 up2]", hits)
	}
}

func TestChatCompletions_NoRetryOn4xx(t *testing.T) {
	// 4xx is a definitive answer — no failover (V4 parity).
	up1Hits := 0
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up1Hits++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad_request"}`))
	}))
	defer up1.Close()
	up2Hits := 0
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up2Hits++
	}))
	defer up2.Close()

	cat := newFakeCatalog()
	cat.setProbe(portFromURL(t, up1.URL), SlotProbe{Healthy: true})
	cat.setProbe(portFromURL(t, up2.URL), SlotProbe{Healthy: true})

	cfg := testCfg(
		[]Backend{
			{Name: "a1", Kind: "foundry_slot", Port: portFromURL(t, up1.URL)},
			{Name: "fallback", Kind: "foundry_slot", Port: portFromURL(t, up2.URL)},
		},
		[]Route{{Model: "m", Primary: "a1", Fallback: []string{"fallback"}}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if up1Hits != 1 {
		t.Errorf("primary hits = %d, want 1", up1Hits)
	}
	if up2Hits != 0 {
		t.Errorf("fallback hits = %d, want 0 (no failover on 4xx)", up2Hits)
	}
}

func TestChatCompletions_FailoverOnTransportError(t *testing.T) {
	// First backend's port is closed (nothing listening) → transport error.
	// Second backend returns 200.
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer up2.Close()

	cat := newFakeCatalog()
	// Port 1 is closed — set healthy=true so the gate passes; the transport
	// error happens at proxy time, triggering failover.
	cat.setProbe(1, SlotProbe{Healthy: true})
	cat.setProbe(portFromURL(t, up2.URL), SlotProbe{Healthy: true})

	cfg := testCfg(
		[]Backend{
			{Name: "dead", Kind: "foundry_slot", Port: 1},
			{Name: "alive", Kind: "foundry_slot", Port: portFromURL(t, up2.URL)},
		},
		[]Route{{Model: "m", Primary: "dead", Fallback: []string{"alive"}}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (transport-error failover failed)", rec.Code)
	}
}

func TestChatCompletions_ExhaustionReturns502(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer up.Close()

	cat := newFakeCatalog()
	cat.setProbe(portFromURL(t, up.URL), SlotProbe{Healthy: true})

	cfg := testCfg(
		[]Backend{{Name: "only", Kind: "foundry_slot", Port: portFromURL(t, up.URL)}},
		[]Route{{Model: "m", Primary: "only"}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "all_backends_unavailable" {
		t.Errorf("error = %v, want all_backends_unavailable", body["error"])
	}
}

func TestChatCompletions_BusyModeFailFast(t *testing.T) {
	// busy_mode=fail_fast: a busy-but-alive slot is skipped.
	up1Hits := 0
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up1Hits++
	}))
	defer up1.Close()
	up2Hits := 0
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up2Hits++
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer up2.Close()

	cat := newFakeCatalog()
	port1 := portFromURL(t, up1.URL)
	port2 := portFromURL(t, up2.URL)
	cat.setProbe(port1, SlotProbe{Healthy: true}) // alive
	cat.setBusy(port1, true)                      // but busy
	cat.setProbe(port2, SlotProbe{Healthy: true})
	cat.setBusy(port2, false)

	cfg := testCfg(
		[]Backend{
			{Name: "busy", Kind: "foundry_slot", Port: port1},
			{Name: "free", Kind: "foundry_slot", Port: port2},
		},
		[]Route{{Model: "m", Primary: "busy", Fallback: []string{"free"}}},
	)
	cfg.BusyMode = BusyFailFast

	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if up1Hits != 0 {
		t.Errorf("busy slot hit = %d, want 0 (should be skipped in fail_fast)", up1Hits)
	}
	if up2Hits != 1 {
		t.Errorf("free slot hit = %d, want 1", up2Hits)
	}
}

func TestChatCompletions_BusyModeWait(t *testing.T) {
	// busy_mode=wait (default): a busy-but-alive slot is attempted anyway.
	up1Hits := 0
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up1Hits++
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer up1.Close()

	cat := newFakeCatalog()
	port1 := portFromURL(t, up1.URL)
	cat.setProbe(port1, SlotProbe{Healthy: true})
	cat.setBusy(port1, true) // busy, but wait-mode still attempts

	cfg := testCfg(
		[]Backend{{Name: "busy", Kind: "foundry_slot", Port: port1}},
		[]Route{{Model: "m", Primary: "busy"}},
	)
	// BusyMode defaults to "wait" via applyDefaults().
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if up1Hits != 1 {
		t.Errorf("busy slot hit = %d, want 1 (wait-mode should attempt busy slot)", up1Hits)
	}
}

// --- Compressor passthrough (Phase 8) ---
//
// foundry_slot_proxied (the backend kind these tests originally exercised)
// was deleted as dead code (ADR-0007 made it unreachable — local routing
// resolves by catalog Config name, not a declared/proxied backend list; see
// docs/v5-headroom-topology.md §6). compressorBypassed's global/per-service
// passthrough logic is still live, reached today via `remote` backends (a
// linked provider's Compressor proxy) — these tests are rewritten against that
// surviving path rather than deleted outright, to keep the passthrough
// coverage (global flag + cross-service isolation) alive.

func TestChatCompletions_CompressorPassthroughGlobal(t *testing.T) {
	// Global passthrough (compressor.passthrough_all) routes straight to the
	// provider's real upstream, skipping its linked Compressor proxy, even
	// though the proxy's own per-service Passthrough flag is false.
	compressorUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"via-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer compressorUpstream.Close()
	compressorPort := portFromURL(t, compressorUpstream.URL)

	realUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"direct-to-real-upstream","object":"chat.completion","choices":[]}`))
	}))
	defer realUpstream.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", TargetURL: realUpstream.URL,
		Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: "deepseek", Port: compressorPort, TargetURL: realUpstream.URL, Passthrough: false,
		ProviderID: &deepseekProvider.ID,
	}); err != nil {
		t.Fatalf("SaveProxy: %v", err)
	}
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})

	settings := newFakeSettings()
	settings.set("compressor.passthrough_all", []byte("true"))

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Settings:     settings,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"direct-to-real-upstream"`) {
		t.Errorf("response = %s, want the real upstream directly (global passthrough)", rec.Body.String())
	}
}

func TestChatCompletions_CompressorPassthroughPerService(t *testing.T) {
	// Per-service passthrough: bypassing one provider's proxy leaves every
	// other provider's proxy routing untouched (CLAUDE.local.md hard
	// requirement).
	deepseekCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"deepseek-via-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer deepseekCompressor.Close()
	deepseekReal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"deepseek-direct","object":"chat.completion","choices":[]}`))
	}))
	defer deepseekReal.Close()

	kimiCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"kimi-via-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer kimiCompressor.Close()
	kimiReal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"kimi-direct","object":"chat.completion","choices":[]}`))
	}))
	defer kimiReal.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// deepseek: passthrough ON (bypassed). kimi: passthrough OFF (still routed
	// through its proxy).
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", TargetURL: deepseekReal.URL,
		Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider(deepseek): %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName(deepseek): %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: "deepseek", Port: portFromURL(t, deepseekCompressor.URL), TargetURL: deepseekReal.URL, Passthrough: true,
		ProviderID: &deepseekProvider.ID,
	}); err != nil {
		t.Fatalf("SaveProxy(deepseek): %v", err)
	}
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "kimi", APIKey: "sk-test", TargetURL: kimiReal.URL,
		Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider(kimi): %v", err)
	}
	kimiProvider, _, err := db.Routing().ProviderByName(ctx, "kimi")
	if err != nil {
		t.Fatalf("ProviderByName(kimi): %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: "kimi", Port: portFromURL(t, kimiCompressor.URL), TargetURL: kimiReal.URL, Passthrough: false,
		ProviderID: &kimiProvider.ID,
	}); err != nil {
		t.Fatalf("SaveProxy(kimi): %v", err)
	}

	deepseekID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: deepseekID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})
	kimiID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "Kimi K2.7 Code"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: kimiID, ProviderID: kimiProvider.ID, WireModel: "kimi-k2.7-code", Enabled: true,
	})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Auth:         &stubAuth{validToken: "x"},
	})

	// deepseek-v4-pro → bypassed, must hit the real upstream directly.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deepseek status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"deepseek-direct"`) {
		t.Errorf("deepseek response = %s, want direct-to-real-upstream (bypassed)", rec.Body.String())
	}

	// kimi-k2.7-code → NOT bypassed, isolation: deepseek's passthrough must
	// not leak into kimi's routing — still goes via its own Compressor proxy.
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"kimi-k2.7-code","messages":[{"role":"user","content":"hi"}]}`))
	req2.RemoteAddr = "100.64.0.1:1234"
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("kimi status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), `"kimi-via-compressor"`) {
		t.Errorf("kimi response = %s, want via-compressor (isolation: deepseek bypass must not affect kimi)", rec2.Body.String())
	}
}

// --- Shared "external" Compressor proxy (Sprint 3, docs/v5-headroom-replacement.md) ---

func TestChatCompletions_SharedExternalCompressor_RoutesProviderWithNoDedicatedProxy(t *testing.T) {
	externalCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"via-external-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer externalCompressor.Close()
	realUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"direct-to-real-upstream","object":"chat.completion","choices":[]}`))
	}))
	defer realUpstream.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", TargetURL: realUpstream.URL,
		Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	// No per-provider ProxyRow for deepseek at all — only the shared
	// "external" instance, ProviderID nil.
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: externalCompressorService, Port: portFromURL(t, externalCompressor.URL),
	}); err != nil {
		t.Fatalf("SaveProxy(external): %v", err)
	}
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})

	settings := newFakeSettings()
	settings.set("compressor.external_enabled", []byte("true"))

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Settings:     settings,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"via-external-compressor"`) {
		t.Errorf("response = %s, want routed through the shared external proxy", rec.Body.String())
	}
}

func TestChatCompletions_SharedExternalCompressor_DisabledFallsThroughDirect(t *testing.T) {
	// Same setup as above, but compressor.external_enabled is left unset
	// (default false) — a seeded "external" ProxyRow existing must not, by
	// itself, turn routing on (mirrors localCompressorEnabled's own doc
	// comment / test coverage).
	externalCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"via-external-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer externalCompressor.Close()
	realUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"direct-to-real-upstream","object":"chat.completion","choices":[]}`))
	}))
	defer realUpstream.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", TargetURL: realUpstream.URL,
		Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: externalCompressorService, Port: portFromURL(t, externalCompressor.URL),
	}); err != nil {
		t.Fatalf("SaveProxy(external): %v", err)
	}
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"direct-to-real-upstream"`) {
		t.Errorf("response = %s, want direct (external not enabled)", rec.Body.String())
	}
}

func TestChatCompletions_SharedExternalCompressor_DedicatedProxyTakesPrecedence(t *testing.T) {
	// A provider with its OWN dedicated proxy must keep using it, never the
	// shared external instance, even when external is enabled.
	dedicatedCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"via-dedicated-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer dedicatedCompressor.Close()
	externalCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"via-external-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer externalCompressor.Close()
	realUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"direct-to-real-upstream","object":"chat.completion","choices":[]}`))
	}))
	defer realUpstream.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", TargetURL: realUpstream.URL,
		Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: "deepseek", Port: portFromURL(t, dedicatedCompressor.URL), TargetURL: realUpstream.URL,
		ProviderID: &deepseekProvider.ID,
	}); err != nil {
		t.Fatalf("SaveProxy(deepseek): %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: externalCompressorService, Port: portFromURL(t, externalCompressor.URL),
	}); err != nil {
		t.Fatalf("SaveProxy(external): %v", err)
	}
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})

	settings := newFakeSettings()
	settings.set("compressor.external_enabled", []byte("true"))

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Settings:     settings,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"via-dedicated-compressor"`) {
		t.Errorf("response = %s, want the dedicated proxy, not the shared external one", rec.Body.String())
	}
}

// TestChatCompletions_SharedExternalCompressor_ForwardsPerProviderUpstreamHeader
// covers the actual multi-provider dispatch, not just "a request reaches the
// shared proxy" (the tests above use a fixed httptest response regardless of
// what the shared proxy received). Unlike a dedicated per-provider proxy —
// whose real upstream is baked into its own env file at provision time —
// the shared "external" instance has no single fixed target: it fronts
// several providers with different real upstreams, so resolveBackend must
// tell it which one applies via x-compress-base-url on every request
// (UpstreamOverride), the same per-request mechanism the local-fronting
// path already used for slot addresses. Two different providers, two
// different expected headers, same shared proxy instance.
func TestChatCompletions_SharedExternalCompressor_ForwardsPerProviderUpstreamHeader(t *testing.T) {
	var lastHeader string
	externalCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastHeader = r.Header.Get("x-compress-base-url")
		w.Write([]byte(`{"id":"via-external-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer externalCompressor.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-a", TargetURL: "https://api.deepseek.com/v1", Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider(deepseek): %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName(deepseek): %v", err)
	}
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "aiand", APIKey: "sk-b", TargetURL: "https://api.aiand.com/v1", Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider(aiand): %v", err)
	}
	aiandProvider, _, err := db.Routing().ProviderByName(ctx, "aiand")
	if err != nil {
		t.Fatalf("ProviderByName(aiand): %v", err)
	}

	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: externalCompressorService, Port: portFromURL(t, externalCompressor.URL),
	}); err != nil {
		t.Fatalf("SaveProxy(external): %v", err)
	}

	dsModelID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: dsModelID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})
	aiModelID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "Kimi K2.7 Code"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: aiModelID, ProviderID: aiandProvider.ID, WireModel: "kimi-k2.7-code", Enabled: true,
	})

	settings := newFakeSettings()
	settings.set("compressor.external_enabled", []byte("true"))

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Settings:     settings,
		Auth:         &stubAuth{validToken: "x"},
	})

	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req1.RemoteAddr = "100.64.0.1:1234"
	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("deepseek: status = %d, want 200 (body: %s)", rec1.Code, rec1.Body.String())
	}
	if lastHeader != "https://api.deepseek.com" {
		t.Errorf("deepseek: x-compress-base-url seen by shared proxy = %q, want bare origin https://api.deepseek.com", lastHeader)
	}

	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"kimi-k2.7-code","messages":[{"role":"user","content":"hi"}]}`))
	req2.RemoteAddr = "100.64.0.1:1234"
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("aiand: status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
	}
	if lastHeader != "https://api.aiand.com" {
		t.Errorf("aiand: x-compress-base-url seen by shared proxy = %q, want bare origin https://api.aiand.com", lastHeader)
	}
}

func TestChatCompletions_SharedExternalCompressor_BypassIsolatedFromDedicatedProxies(t *testing.T) {
	// Bypassing the shared "external" service must not affect a different
	// provider's own dedicated proxy — same per-service isolation
	// guarantee TestChatCompletions_CompressorPassthroughPerService already
	// covers for two dedicated proxies, extended to the external case.
	externalCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"kimi-via-external-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer externalCompressor.Close()
	kimiReal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"kimi-direct","object":"chat.completion","choices":[]}`))
	}))
	defer kimiReal.Close()
	dedicatedCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"deepseek-via-dedicated-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer dedicatedCompressor.Close()
	deepseekReal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"deepseek-direct","object":"chat.completion","choices":[]}`))
	}))
	defer deepseekReal.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// kimi: no dedicated proxy, routes through the shared external one,
	// which is bypassed — must hit kimi's real upstream directly.
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "kimi", APIKey: "sk-test", TargetURL: kimiReal.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider(kimi): %v", err)
	}
	kimiProvider, _, err := db.Routing().ProviderByName(ctx, "kimi")
	if err != nil {
		t.Fatalf("ProviderByName(kimi): %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: externalCompressorService, Port: portFromURL(t, externalCompressor.URL), Passthrough: true,
	}); err != nil {
		t.Fatalf("SaveProxy(external): %v", err)
	}

	// deepseek: has its OWN dedicated proxy, not bypassed — must keep
	// routing through it, unaffected by external's bypass.
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", TargetURL: deepseekReal.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider(deepseek): %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName(deepseek): %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: "deepseek", Port: portFromURL(t, dedicatedCompressor.URL), TargetURL: deepseekReal.URL,
		ProviderID: &deepseekProvider.ID,
	}); err != nil {
		t.Fatalf("SaveProxy(deepseek): %v", err)
	}

	kimiID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "Kimi K2.7 Code"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: kimiID, ProviderID: kimiProvider.ID, WireModel: "kimi-k2.7-code", Enabled: true,
	})
	deepseekID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: deepseekID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})

	settings := newFakeSettings()
	settings.set("compressor.external_enabled", []byte("true"))

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Settings:     settings,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"kimi-k2.7-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("kimi status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"kimi-direct"`) {
		t.Errorf("kimi response = %s, want direct (external bypassed)", rec.Body.String())
	}

	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req2.RemoteAddr = "100.64.0.1:1234"
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("deepseek status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), `"deepseek-via-dedicated-compressor"`) {
		t.Errorf("deepseek response = %s, want via its own dedicated proxy, unaffected by external's bypass", rec2.Body.String())
	}
}

func TestChatCompletions_ExtraFieldsPassthrough(t *testing.T) {
	// Contract 1 §7: "all other fields pass through untouched". Only model +
	// messages are validated; everything else reaches the upstream.
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
		[]Route{{Model: "m", Primary: "a1"}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.95,"max_tokens":100,"stop":["\n"],"user":"session-42","custom_field":{"nested":[1,2,3]}}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Extra fields must survive to the upstream.
	if received["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want 0.7", received["temperature"])
	}
	if received["user"] != "session-42" {
		t.Errorf("user = %v, want session-42", received["user"])
	}
	custom, _ := received["custom_field"].(map[string]any)
	if custom == nil {
		t.Error("custom_field missing from upstream request")
	}
	// model must be overwritten to wire_model.
	if received["model"] != "/m.gguf" {
		t.Errorf("upstream model = %v, want /m.gguf (wire_model)", received["model"])
	}
}

func TestChatCompletions_UnhealthySlotSkipped(t *testing.T) {
	// Mode-switch dead-window fix: an unhealthy slot is always skipped
	// regardless of busy_mode. With a fallback, the request fails over.
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer up2.Close()

	cat := newFakeCatalog()
	cat.setProbe(8080, SlotProbe{Healthy: false}) // unhealthy (mid switch)
	cat.setProbe(portFromURL(t, up2.URL), SlotProbe{Healthy: true})

	cfg := testCfg(
		[]Backend{
			{Name: "dead", Kind: "foundry_slot", Port: 8080},
			{Name: "alive", Kind: "foundry_slot", Port: portFromURL(t, up2.URL)},
		},
		[]Route{{Model: "m", Primary: "dead", Fallback: []string{"alive"}}},
	)
	// No scheduler wired → ensureBackendLoaded fails → failover to alive.
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unhealthy slot should fail over)", rec.Code)
	}
}

func TestChatCompletions_OnDemandLoad(t *testing.T) {
	// foundry_slot backend is unhealthy → ensureBackendLoaded is called.
	// sched.Stub returns success → the slot is re-gated → request proceeds.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	// Initially unhealthy; after "load" (stub succeeds), the router re-gates.
	// We simulate the load by having the slot become healthy after the first
	// probe (the stub scheduler returns immediately).
	cat.setProbe(port, SlotProbe{Healthy: false})

	loadCalled := false
	stubSched := &recordingSched{
		Stub:   sched.Stub{},
		onLoad: func() { loadCalled = true; cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"}) },
	}

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
		[]Route{{Model: "m", Primary: "a1"}},
	)
	srv := NewWithDeps(Deps{
		Cfg: cfg, Catalog: cat, Sched: stubSched,
		Slots: map[string]int{"a1": port},
		Auth:  &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !loadCalled {
		t.Error("scheduler.EnsureLoaded should have been called for unhealthy slot")
	}
}

// recordingSched wraps sched.Stub and invokes a callback on EnsureLoaded.
type recordingSched struct {
	sched.Stub
	onLoad  func()
	lastReq sched.EnsureRequest
}

func (s *recordingSched) EnsureLoaded(ctx context.Context, req sched.EnsureRequest) (sched.Ticket, error) {
	s.lastReq = req
	if s.onLoad != nil {
		s.onLoad()
	}
	return s.Stub.EnsureLoaded(ctx, req)
}

// --- RequestedBy attribution (smith P3, docs/v5-smith.md §4.3) ---

func TestChatCompletions_RequestedByHeader(t *testing.T) {
	// X-Forge-Requested-By lets an in-process caller (smith's reasoning
	// tier) attribute its own scheduler loads instead of the generic "a0" —
	// visible in the queue, no priority-jump implication.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: false})

	stubSched := &recordingSched{
		Stub: sched.Stub{},
		onLoad: func() {
			cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})
		},
	}

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
		[]Route{{Model: "m", Primary: "a1"}},
	)
	srv := NewWithDeps(Deps{
		Cfg: cfg, Catalog: cat, Sched: stubSched,
		Slots: map[string]int{"a1": port},
		Auth:  &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	req.Header.Set("X-Forge-Requested-By", "smith")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if stubSched.lastReq.RequestedBy != "smith" {
		t.Errorf("RequestedBy = %q, want %q", stubSched.lastReq.RequestedBy, "smith")
	}
}

func TestChatCompletions_RequestedByHeader_DefaultsToA0(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: false})

	stubSched := &recordingSched{
		Stub: sched.Stub{},
		onLoad: func() {
			cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})
		},
	}

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
		[]Route{{Model: "m", Primary: "a1"}},
	)
	srv := NewWithDeps(Deps{
		Cfg: cfg, Catalog: cat, Sched: stubSched,
		Slots: map[string]int{"a1": port},
		Auth:  &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if stubSched.lastReq.RequestedBy != "a0" {
		t.Errorf("RequestedBy = %q, want default %q", stubSched.lastReq.RequestedBy, "a0")
	}
}

// --- Audit ---

func TestAuditOutcome_RecordsDecision(t *testing.T) {
	audit := &fakeAudit{}
	srv := NewWithDeps(Deps{Audit: audit})

	srv.auditOutcome(context.Background(), "test-model", "ok", "a1:ok")
	entry, ok := audit.last()
	if !ok {
		t.Fatal("audit entry not written")
	}
	if entry.Target != "test-model" {
		t.Errorf("audit target = %q, want test-model", entry.Target)
	}
	if entry.Actor != "router" {
		t.Errorf("audit actor = %q, want router", entry.Actor)
	}
	// Detail must be JSON with result+detail; never bodies/prompts/credentials.
	var detail map[string]string
	if err := json.Unmarshal([]byte(entry.Detail), &detail); err != nil {
		t.Fatalf("audit detail not JSON: %v", err)
	}
	if detail["result"] != "ok" {
		t.Errorf("audit result = %q, want ok", detail["result"])
	}
	if detail["detail"] != "a1:ok" {
		t.Errorf("audit detail = %q, want a1:ok", detail["detail"])
	}
}

// --- Recorded request corpus (risk register) ---

func TestRecordedCorpus(t *testing.T) {
	// Risk register: "Phase 6 tested against recorded request corpus".
	// testdata/corpus/*.json holds representative OpenCode/LibreChat-shaped
	// requests; this test replays each through the router against a mock
	// upstream and verifies the router handles every shape correctly.
	corpusDir := filepath.Join("testdata", "corpus")
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Skipf("corpus not found at %s: %v", corpusDir, err)
	}
	if len(entries) == 0 {
		t.Fatal("corpus directory is empty")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back the model the router forwarded (the wire_model).
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		isStream, _ := body["stream"].(bool)
		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", model)
			flusher.Flush()
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"ok","object":"chat.completion","model":%q,"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, model)
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/models/corpus.gguf"})

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
		[]Route{{Model: "gemma4-26b-mtp", Primary: "a1"}, {Model: "deepseek-v4-pro", Primary: "a1"}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(corpusDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw))
			req.RemoteAddr = "100.64.0.1:1234" // tailnet → bypass auth
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("corpus %s: status = %d, want 200 (body: %s)", e.Name(), rec.Code, rec.Body.String())
			}
			// Streaming responses must arrive as text/event-stream.
			ct := rec.Header().Get("Content-Type")
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err == nil {
				if isStream, _ := body["stream"].(bool); isStream {
					if !strings.Contains(ct, "text/event-stream") {
						t.Errorf("corpus %s: Content-Type = %q, want text/event-stream", e.Name(), ct)
					}
				}
			}
		})
	}
}

// --- Skeleton mode ---

func TestSkeletonMode_HealthzOnly(t *testing.T) {
	// New() with no deps: only /healthz answers; /v1/* returns 503.
	srv := New()

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}

	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("skeleton /v1/chat/completions = %d, want 503", rec.Code)
	}
}

// --- Catalog-config routing (a0 local-config visibility fix) ---

// fixedSlotSched is a sched.Scheduler fake that always reports the model
// loaded on a fixed slot, regardless of the request's TargetSlot — used to
// verify the router resolves the port a genuinely unpinned ("") placement
// request would land on, including slots like a3/a4 that have no router.toml
// backend entry at all.
type fixedSlotSched struct {
	sched.Stub
	slot   string
	status string // "" defaults to "loaded"
	onLoad func() // optional: called on every EnsureLoaded, for call-tracking assertions
}

func (s *fixedSlotSched) EnsureLoaded(_ context.Context, req sched.EnsureRequest) (sched.Ticket, error) {
	if s.onLoad != nil {
		s.onLoad()
	}
	status := s.status
	if status == "" {
		status = "loaded"
	}
	return sched.Ticket{
		TicketID:    "fixed",
		Model:       req.Model,
		RequestedBy: req.RequestedBy,
		TargetSlot:  s.slot,
		Status:      status,
	}, nil
}

func TestChatCompletions_CatalogConfig(t *testing.T) {
	// A model with no router.toml route at all, but a matching visible
	// catalog Config, must be loaded on-demand (onto whatever slot the
	// scheduler picks — here "a3", which has no static backend entry) and
	// proxied to it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "catalog-only-model", 16384, "visible")

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil), // no router.toml backends/routes at all
		Catalog:      cat,
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a3"},
		Slots:        map[string]int{"a3": port},
		Auth:         &stubAuth{validToken: "x"},
	})

	chatReq := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"catalog-only-model","messages":[{"role":"user","content":"hi"}]}`))
	chatReq.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, chatReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestChatCompletions_CatalogConfig_HiddenNotFound(t *testing.T) {
	// A hidden catalog Config must not be reachable via a0 — 404, same as a
	// genuinely unknown model.
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "hidden-model", 16384, "hidden")

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &sched.Stub{},
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"hidden-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestChatCompletions_CatalogConfig_LoadFailed(t *testing.T) {
	// A visible catalog Config whose on-demand load fails must surface as a
	// 502, not silently 404 (which would look like "model doesn't exist").
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "unloadable-model", 16384, "visible")

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a3", status: "failed"},
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"unloadable-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestChatCompletions_UnknownModelStillNotFound(t *testing.T) {
	// A model name that is neither a router.toml route nor a catalog Config
	// must still 404, even with StoreCatalog wired — no regression to the
	// existing "unknown model" behavior.
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &sched.Stub{},
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"totally-unknown","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestChatCompletions_CatalogConfigOutranksStaleStaticRoute is the routing.go
// rewrite's core regression test (ADR-0007, docs/adr/0007-routing-resolves-
// by-identity-not-address.md): when a model name matches BOTH a catalog
// Config and a static router.toml route, the catalog resolution must win.
// The static route here points at a "stale" upstream a real operator would
// have left behind after switching what's loaded on that slot (exactly the
// live a1 bug the ADR documents) — if the static route won, this request
// would silently get served by the wrong upstream with no error.
func TestChatCompletions_CatalogConfigOutranksStaleStaticRoute(t *testing.T) {
	freshUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"fresh","object":"chat.completion","choices":[]}`))
	}))
	defer freshUpstream.Close()
	staleUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"stale","object":"chat.completion","choices":[]}`))
	}))
	defer staleUpstream.Close()
	freshPort := portFromURL(t, freshUpstream.URL)
	stalePort := portFromURL(t, staleUpstream.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "drifted-model", 16384, "visible")

	cat := newFakeCatalog()
	cat.setProbe(freshPort, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})
	cat.setProbe(stalePort, SlotProbe{Healthy: true, ModelPath: "/stale.gguf"})

	srv := NewWithDeps(Deps{
		// A static route for the same model name, pointing at a different
		// backend — simulates router.toml never having been updated after
		// the operator switched what's loaded on the slot it used to point
		// at (the live a1 scenario). If resolution fell back to this route
		// instead of the catalog, the response would come from staleUpstream.
		Cfg: testCfg(
			[]Backend{{Name: "stale-backend", Kind: "foundry_slot", Port: stalePort}},
			[]Route{{Model: "drifted-model", Primary: "stale-backend"}},
		),
		Catalog:      cat,
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a3"},
		Slots:        map[string]int{"a3": freshPort},
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"drifted-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"fresh"`) {
		t.Errorf("response = %s, want the catalog-resolved (fresh) upstream, not the stale static route", rec.Body.String())
	}
}

// --- Local Compressor fronting (docs/v5-headroom-topology.md, resolving §11) ---
//
// catalogChain itself is unchanged (still emits a plain foundry_slot
// backend, still never binds a proxy to a physical slot address) — the
// local-Compressor decision lives entirely in resolveBackend, gated by the
// compressor.local_enabled setting (default off) plus the existing
// compressorBypassed passthrough mechanism.

func TestChatCompletions_LocalCompressor_RoutesViaSharedProxyWithHeader(t *testing.T) {
	// compressor.local_enabled=true + a "local" ProxyRow → the request must
	// land on the shared proxy's port, carrying x-compress-base-url set to
	// the *actual resolved slot's* address — never a fixed/pinned one.
	var gotHeader string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-compress-base-url")
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer proxy.Close()
	proxyPort := portFromURL(t, proxy.URL)

	// The slot itself must be a real, healthy-probing server — slotGate
	// checks it directly regardless of whether local Compressor fronts the
	// actual chat request, so it never receives the chat request itself here.
	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// slotGate legitimately probes /health directly, regardless of
		// whether the actual chat request routes via Compressor — only flag
		// the chat-completion path itself landing here.
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path == "/chat/completions" {
			t.Error("chat request reached the raw slot directly — should have gone via the shared proxy")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "local-fronted-model", 16384, "visible")

	settings := newFakeSettings()
	settings.set("compressor.local_enabled", []byte("true"))
	hp := &fakeCompressor{proxies: []store.ProxyRow{{Service: "local", Port: proxyPort}}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Settings:     settings,
		Routing:      hp,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"local-fronted-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// headroom-ai ≥0.35.0 appends /v1 to x-compress-base-url itself, so the
	// server must send the bare slot root (no /v1) or the path doubles.
	wantHeader := fmt.Sprintf("http://127.0.0.1:%d", slotPort)
	if gotHeader != wantHeader {
		t.Errorf("x-compress-base-url = %q, want %q", gotHeader, wantHeader)
	}
}

func TestChatCompletions_LocalCompressor_DisabledByDefault(t *testing.T) {
	// A "local" ProxyRow existing is not enough by itself — compressor.local_enabled
	// must be explicitly set, so seeding the row (the Phase 1 one-time manual
	// verification step) can't silently turn routing on.
	var proxyHit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHit = true
		w.Write([]byte(`{"id":"proxy"}`))
	}))
	defer proxy.Close()
	proxyPort := portFromURL(t, proxy.URL)

	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"slot","object":"chat.completion","choices":[]}`))
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "not-yet-fronted-model", 16384, "visible")

	hp := &fakeCompressor{proxies: []store.ProxyRow{{Service: "local", Port: proxyPort}}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Routing:      hp, // "local" row exists, but compressor.local_enabled is unset
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"not-yet-fronted-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if proxyHit {
		t.Error("request hit the local Compressor proxy while compressor.local_enabled was unset")
	}
	if !strings.Contains(rec.Body.String(), `"slot"`) {
		t.Errorf("response = %s, want the raw slot's response", rec.Body.String())
	}
}

func TestChatCompletions_LocalCompressor_OrphanedProxySkipped(t *testing.T) {
	// Phase 2 (docs/v5-headroom-topology.md): a torn-down proxy's row is
	// soft-orphaned (OrphanedAt set), not deleted, for audit/history. It
	// must never be routed to again — the request should fall straight
	// through to the raw slot, exactly like the "no local row at all" case.
	var proxyHit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHit = true
		w.Write([]byte(`{"id":"proxy"}`))
	}))
	defer proxy.Close()
	proxyPort := portFromURL(t, proxy.URL)

	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"slot","object":"chat.completion","choices":[]}`))
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "orphaned-proxy-model", 16384, "visible")

	settings := newFakeSettings()
	settings.set("compressor.local_enabled", []byte("true"))
	hp := &fakeCompressor{proxies: []store.ProxyRow{
		{Service: "local", Port: proxyPort, OrphanedAt: time.Now()},
	}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Settings:     settings,
		Routing:      hp,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"orphaned-proxy-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if proxyHit {
		t.Error("request hit the orphaned local Compressor proxy — should have been skipped")
	}
	if !strings.Contains(rec.Body.String(), `"slot"`) {
		t.Errorf("response = %s, want the raw slot's response", rec.Body.String())
	}
}

func TestChatCompletions_LocalCompressor_BypassRoutesDirectToSlot(t *testing.T) {
	// Passthrough on for the "local" service must route straight to the slot,
	// same isolation guarantee as the existing per-service passthrough tests.
	var proxyHit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHit = true
		w.Write([]byte(`{"id":"proxy"}`))
	}))
	defer proxy.Close()
	proxyPort := portFromURL(t, proxy.URL)

	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"slot","object":"chat.completion","choices":[]}`))
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "bypassed-model", 16384, "visible")

	settings := newFakeSettings()
	settings.set("compressor.local_enabled", []byte("true"))
	hp := &fakeCompressor{proxies: []store.ProxyRow{{Service: "local", Port: proxyPort, Passthrough: true}}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Settings:     settings,
		Routing:      hp,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"bypassed-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if proxyHit {
		t.Error("request hit the Compressor proxy despite local-service passthrough being on")
	}
	if !strings.Contains(rec.Body.String(), `"slot"`) {
		t.Errorf("response = %s, want the raw slot's response (bypassed)", rec.Body.String())
	}
}

// TestChatCompletions_LocalCompressor_StripsForgedClientHeader is the §5b
// security regression test (docs/v5-headroom-topology.md): a client-supplied
// x-compress-base-url must never survive to any upstream, on any branch —
// not forwarded verbatim to the shared local proxy (which would let an SSRF
// payload override the server-derived value), and not leaked through on a
// branch that doesn't use the header at all (e.g. a plain, non-Compressor-
// fronted foundry_slot request).
func TestChatCompletions_LocalCompressor_StripsForgedClientHeader(t *testing.T) {
	var gotHeader string
	var headerPresent bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader, headerPresent = r.Header.Get("x-compress-base-url"), r.Header.Get("x-compress-base-url") != ""
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer proxy.Close()
	proxyPort := portFromURL(t, proxy.URL)

	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// slotGate legitimately probes /health directly, regardless of
		// whether the actual chat request routes via Compressor — only flag
		// the chat-completion path itself landing here.
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path == "/chat/completions" {
			t.Error("chat request reached the raw slot directly — should have gone via the shared proxy")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "forged-header-model", 16384, "visible")

	settings := newFakeSettings()
	settings.set("compressor.local_enabled", []byte("true"))
	hp := &fakeCompressor{proxies: []store.ProxyRow{{Service: "local", Port: proxyPort}}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Settings:     settings,
		Routing:      hp,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"forged-header-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	req.Header.Set("x-compress-base-url", "http://169.254.169.254/latest/meta-data/") // forged SSRF target
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// Server-derived slot root (no /v1 — compressor ≥0.35.0 appends it itself).
	wantHeader := fmt.Sprintf("http://127.0.0.1:%d", slotPort)
	if !headerPresent {
		t.Fatal("x-compress-base-url missing entirely — expected the server-derived slot address")
	}
	if gotHeader != wantHeader {
		t.Errorf("x-compress-base-url = %q, want the server-derived slot address %q, not the forged one", gotHeader, wantHeader)
	}
}

// TestChatCompletions_ForgedHeaderStrippedOnNonCompressorBranch covers the
// other half of §5b: a branch that never sets UpstreamOverride at all (plain
// foundry_slot, local-Compressor disabled) must still not forward a client's
// raw x-compress-base-url upstream — the strip in the Rewrite hook is
// unconditional, not contingent on this backend using the header.
func TestChatCompletions_ForgedHeaderStrippedOnNonCompressorBranch(t *testing.T) {
	var headerPresent bool
	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerPresent = r.Header.Get("x-compress-base-url") != ""
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: slotPort}},
		[]Route{{Model: "m", Primary: "a1"}},
	)
	cat := newFakeCatalog()
	cat.setProbe(slotPort, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})

	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	req.Header.Set("x-compress-base-url", "http://169.254.169.254/latest/meta-data/")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if headerPresent {
		t.Error("forged x-compress-base-url reached the upstream on a branch that never uses it")
	}
}

// --- Remote routing via store.Offering (ADR-0007 §3.4) ---
//
// Found live 2026-07-28: RouterConfig.Backends/.Routes are always nil once
// router.LoadFromStore replaced router.ParseConfig — which meant every
// remote model (deepseek-v4-pro, kimi-k2.7-code, glm-5.2, ...) 404'd on
// /v1/chat/completions despite still being correctly listed in /v1/models
// (BuildModelsResponse's separate Offering loop). offeringChain is the fix.

func TestChatCompletions_RemoteOffering_ViaCompressorProxy(t *testing.T) {
	// The request must land on the Compressor proxy's port, not the provider's
	// real upstream directly — compression stays in the path for a provider
	// with a linked proxy.
	compressorUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"via-compressor","object":"chat.completion","choices":[]}`))
	}))
	defer compressorUpstream.Close()
	compressorPort := portFromURL(t, compressorUpstream.URL)

	realUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"direct-to-real-upstream","object":"chat.completion","choices":[]}`))
	}))
	defer realUpstream.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", TargetURL: realUpstream.URL,
		Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: "deepseek", Port: compressorPort, TargetURL: realUpstream.URL,
		ProviderID: &deepseekProvider.ID,
	}); err != nil {
		t.Fatalf("SaveProxy: %v", err)
	}
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"via-compressor"`) {
		t.Errorf("response = %s, want the Compressor-proxied upstream", rec.Body.String())
	}
}

func TestChatCompletions_RemoteOffering_NoProxyLinkedGoesDirect(t *testing.T) {
	// A provider with no linked Compressor proxy must route straight to its
	// real upstream (TargetURL) — pure passthrough, no compression to skip.
	realUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"direct","object":"chat.completion","choices":[]}`))
	}))
	defer realUpstream.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "glm", APIKey: "sk-test", TargetURL: realUpstream.URL, // no proxy linked
		Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	glmProvider, _, err := db.Routing().ProviderByName(ctx, "glm")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "GLM 5.2"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: glmProvider.ID, WireModel: "glm-5.2", Enabled: true,
	})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"direct"`) {
		t.Errorf("response = %s, want the direct (no-proxy) upstream", rec.Body.String())
	}
}

func TestChatCompletions_RemoteOffering_DisabledNotFound(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "Disabled Model"})
	// No provider row is seeded — CreateOffering's provider_id FK simply
	// fails to insert (matching the pre-0042 behavior, when provider was a
	// TEXT FK to a nonexistent name), so this offering never exists either
	// way; disabled+missing both 404 the same way, which is what this test
	// actually asserts.
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, WireModel: "disabled-model", Enabled: false,
	})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"disabled-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestChatCompletions_OversizedBody(t *testing.T) {
	srv := NewWithDeps(Deps{
		Cfg:  testCfg(nil, nil),
		Auth: &stubAuth{validToken: "x"},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", infiniteReader{})
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestSlotErrorCount(t *testing.T) {
	srv := NewWithDeps(Deps{})
	srv.recordSlotError(8080)
	srv.recordSlotError(8080)
	srv.recordSlotError(8080)
	srv.recordSlotError(8087)

	n, total := srv.SlotErrorCount(8080, 60)
	if n != 3 {
		t.Errorf("windowed count = %d, want 3", n)
	}
	if total != 3 {
		t.Errorf("lifetime count = %d, want 3", total)
	}
	n, _ = srv.SlotErrorCount(8087, 60)
	if n != 1 {
		t.Errorf("8087 windowed count = %d, want 1", n)
	}
	// Untouched port reads zero.
	n, _ = srv.SlotErrorCount(8081, 60)
	if n != 0 {
		t.Errorf("8081 windowed count = %d, want 0", n)
	}
	// Zero window returns lifetime.
	n, total = srv.SlotErrorCount(8080, 0)
	if n != 3 || total != 3 {
		t.Errorf("zero-window = (%d,%d), want (3,3)", n, total)
	}
	// Nil receiver is safe.
	if n, total := (*Server)(nil).SlotErrorCount(8080, 60); n != 0 || total != 0 {
		t.Errorf("nil receiver = (%d,%d), want (0,0)", n, total)
	}
}

// Sprint 4 (resource bounding + monitoring): the all_backends_unavailable
// body's new "layer" field must name which layer actually failed —
// compressor vs. backend — rather than leaving a consumer to guess from a
// bare status code, same as unavailableMessage's original 2026-07-29
// motivation for adding "message" in the first place.

func TestChatCompletions_AllBackendsUnavailable_DirectFailureLayer(t *testing.T) {
	// No Compressor fronting at all — an exhausted direct backend must be
	// attributed to "backend" unambiguously.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer up.Close()

	cat := newFakeCatalog()
	cat.setProbe(portFromURL(t, up.URL), SlotProbe{Healthy: true})

	cfg := testCfg(
		[]Backend{{Name: "only", Kind: "foundry_slot", Port: portFromURL(t, up.URL)}},
		[]Route{{Model: "m", Primary: "only"}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["layer"] != "backend" {
		t.Errorf("layer = %v, want %q", body["layer"], "backend")
	}
}

// TestChatCompletions_LocalCompressorDown_AutoBypassesToSlot is Sprint 8's
// core case: local Compressor fronting is enabled but the shared proxy
// process itself is down (transport error on its loopback port). Before
// Sprint 8 this was an unconditional 502 attributed to "compressor" — the
// compressor being down made the model itself unusable even though the real
// slot was healthy and reachable the whole time. Now the down compressor is
// attempted first (proving it's genuinely unreachable, not skipped), then
// the same request is retried directly against the real slot — the client
// gets a normal 200, uncompressed, with no operator action needed.
func TestChatCompletions_LocalCompressorDown_AutoBypassesToSlot(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	proxyPort := portFromURL(t, proxy.URL)
	proxy.Close() // nothing listening on proxyPort now

	var sawChatCompletions bool
	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/chat/completions":
			sawChatCompletions = true
			w.Write([]byte(`{"id":"direct-to-slot","object":"chat.completion","choices":[]}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "compressor-down-model", 16384, "visible")

	settings := newFakeSettings()
	settings.set("compressor.local_enabled", []byte("true"))
	hp := &fakeCompressor{proxies: []store.ProxyRow{{Service: "local", Port: proxyPort}}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Settings:     settings,
		Routing:      hp,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"compressor-down-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !sawChatCompletions {
		t.Error("bypass never reached the raw slot's /chat/completions")
	}
	if !strings.Contains(rec.Body.String(), `"direct-to-slot"`) {
		t.Errorf("response = %s, want the raw slot's response", rec.Body.String())
	}
}

// TestChatCompletions_LocalCompressorAndSlotBothDown_StillFails proves the
// auto-bypass doesn't paper over a genuinely dead backend: when both the
// compressor AND the real slot fail, the request must still 502, and the
// final layer attribution must be "backend" — the bypass attempt is what
// proved the backend itself (not just the compressor in front of it) is the
// real problem.
func TestChatCompletions_LocalCompressorAndSlotBothDown_StillFails(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	proxyPort := portFromURL(t, proxy.URL)
	proxy.Close()

	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusBadGateway) // slot itself is wedged/erroring
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "both-down-model", 16384, "visible")

	settings := newFakeSettings()
	settings.set("compressor.local_enabled", []byte("true"))
	hp := &fakeCompressor{proxies: []store.ProxyRow{{Service: "local", Port: proxyPort}}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Settings:     settings,
		Routing:      hp,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"both-down-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["layer"] != "backend" {
		t.Errorf("layer = %v, want %q (bypass proved the backend itself is broken, not just the compressor)", body["layer"], "backend")
	}
}

// TestChatCompletions_DedicatedRemoteProxyDown_AutoBypassesToProvider covers
// the bug Sprint 8 found while building the bypass: a dedicated per-provider
// proxy (e.g. deepseek) has BaseURL pointing at the compressor but an empty
// UpstreamOverride (its target is baked into its own env file, not sent
// per-request) — the old `compressorFronted := UpstreamOverride != ""`
// inference silently treated this as NOT compressor-fronted, so a down
// dedicated proxy misclassified as a "backend" failure and had no bypass
// target at all. ResolvedBackend.CompressorFronted/.DirectUpstreamURL fix
// this: a transport failure against the dedicated proxy now retries directly
// against the provider's real TargetURL.
func TestChatCompletions_DedicatedRemoteProxyDown_AutoBypassesToProvider(t *testing.T) {
	dedicatedCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	compressorPort := portFromURL(t, dedicatedCompressor.URL)
	dedicatedCompressor.Close() // nothing listening now

	var sawDirect bool
	realUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawDirect = true
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", got)
		}
		w.Write([]byte(`{"id":"direct-to-provider","object":"chat.completion","choices":[]}`))
	}))
	defer realUpstream.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", TargetURL: realUpstream.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekProvider, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: "deepseek", Port: compressorPort, TargetURL: realUpstream.URL,
		ProviderID: &deepseekProvider.ID,
	}); err != nil {
		t.Fatalf("SaveProxy(deepseek): %v", err)
	}
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "DeepSeek V4 Pro"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseekProvider.ID, WireModel: "deepseek-v4-pro", Enabled: true,
	})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !sawDirect {
		t.Error("bypass never reached the real provider upstream")
	}
	if !strings.Contains(rec.Body.String(), `"direct-to-provider"`) {
		t.Errorf("response = %s, want the direct provider response", rec.Body.String())
	}
}

// TestChatCompletions_SharedExternalProxyDown_AutoBypassesToProvider mirrors
// the dedicated-proxy case above for the shared "external" instance, which
// DOES set UpstreamOverride (per-request, since it fronts several providers)
// — confirming the bypass path also works on that branch, not just the
// dedicated one.
func TestChatCompletions_SharedExternalProxyDown_AutoBypassesToProvider(t *testing.T) {
	externalCompressor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	compressorPort := portFromURL(t, externalCompressor.URL)
	externalCompressor.Close()

	var sawDirect bool
	realUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sawDirect = true
		w.Write([]byte(`{"id":"direct-to-provider-external","object":"chat.completion","choices":[]}`))
	}))
	defer realUpstream.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "aiand", APIKey: "sk-test", TargetURL: realUpstream.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	aiandProvider, _, err := db.Routing().ProviderByName(ctx, "aiand")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: externalCompressorService, Port: compressorPort,
	}); err != nil {
		t.Fatalf("SaveProxy(external): %v", err)
	}
	mdlID, _ := db.Catalog().CreateModel(ctx, store.Model{Name: "GLM 5.2"})
	db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: aiandProvider.ID, WireModel: "glm-5.2", Enabled: true,
	})

	settings := newFakeSettings()
	settings.set("compressor.external_enabled", []byte("true"))

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:      db.Routing(),
		Settings:     settings,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !sawDirect {
		t.Error("bypass never reached the real provider upstream")
	}
	if !strings.Contains(rec.Body.String(), `"direct-to-provider-external"`) {
		t.Errorf("response = %s, want the direct provider response", rec.Body.String())
	}
}

// TestChatCompletions_CompressorRelayed5xx_DoesNotBypass proves the bypass
// only ever triggers on a genuine connection-level failure — a 5xx the
// compressor itself relayed from a real backend it successfully reached must
// NOT be retried directly (that would re-execute the same request against
// the backend a second time for a failure that had nothing to do with the
// compressor being reachable).
func TestChatCompletions_CompressorRelayed5xx_DoesNotBypass(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Forge-Compress", "1")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	proxyPort := portFromURL(t, proxy.URL)

	var directHits int
	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/chat/completions":
			directHits++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "relayed-5xx-model", 16384, "visible")

	settings := newFakeSettings()
	settings.set("compressor.local_enabled", []byte("true"))
	hp := &fakeCompressor{proxies: []store.ProxyRow{{Service: "local", Port: proxyPort}}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Settings:     settings,
		Routing:      hp,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"relayed-5xx-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	if directHits != 0 {
		t.Errorf("raw slot's /chat/completions was hit %d times — a compressor-relayed 5xx must never trigger a direct bypass retry", directHits)
	}
}

func TestChatCompletions_AllBackendsUnavailable_CompressorRelayedFailureLayer(t *testing.T) {
	// The shared proxy is up and relays a real backend 5xx, marking the
	// response X-Forge-Compress (reached) with no -Error header — the
	// failure must be attributed to "backend", since the compressor did its
	// job and the failure is downstream of it.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Forge-Compress", "1")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	proxyPort := portFromURL(t, proxy.URL)

	rawSlot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer rawSlot.Close()
	slotPort := portFromURL(t, rawSlot.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "compressor-relayed-model", 16384, "visible")

	settings := newFakeSettings()
	settings.set("compressor.local_enabled", []byte("true"))
	hp := &fakeCompressor{proxies: []store.ProxyRow{{Service: "local", Port: proxyPort}}}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a1"},
		Slots:        map[string]int{"a1": slotPort},
		Settings:     settings,
		Routing:      hp,
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"compressor-relayed-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["layer"] != "backend" {
		t.Errorf("layer = %v, want %q", body["layer"], "backend")
	}
}
