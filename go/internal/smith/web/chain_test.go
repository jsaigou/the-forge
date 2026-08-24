// SPDX-License-Identifier: Apache-2.0

package web

// chain_test.go — the smith P5 exit criterion (docs/v5-smith.md §9):
// "fallback-chain test with providers down". Exercises the real Service
// (via New) against httptest servers that can be closed mid-test to
// simulate a provider going down, asserting the chain falls through
// correctly and never hangs.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newChainService(t *testing.T, settings *fakeSettings, allowHost func(string) bool) Service {
	t.Helper()
	db := newTestDB(t)
	return New(Deps{
		Cache:           NewSQLCache(db),
		Settings:        settings,
		AllowDirectHost: allowHost,
	})
}

func allowAllHosts(string) bool { return true }

func chainSettings(searxngURL, firecrawlURL string) *fakeSettings {
	s := newFakeSettings()
	s.setRaw(SettingEnabled, `true`)
	s.setRaw(SettingProviderOrder, `["searxng","firecrawl","direct"]`)
	s.setRaw(SettingCacheTTL, `"1h"`)
	s.setRaw(SettingSearxng, `{"base_url":"`+searxngURL+`","enabled":true}`)
	s.setRaw(SettingFirecrawl, `{"base_url":"`+firecrawlURL+`","enabled":true}`)
	s.setRaw(SettingDirect, `{"enabled":true}`)
	return s
}

func searxngServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"title":"llama.cpp","url":"https://github.com/ggml-org/llama.cpp","content":"C/C++ inference"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func firecrawlServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"markdown":"# Firecrawl content","metadata":{"title":"Firecrawl page"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func plainPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Direct page</title></head><body><p>direct content</p></body></html>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedServer returns a URL that is guaranteed unreachable (a server
// started then immediately closed) — simulates "provider down" without any
// real network dependency or sleep.
func closedServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	return srv.URL
}

func TestChain_AllUp(t *testing.T) {
	searxng := searxngServer(t)
	firecrawl := firecrawlServer(t)
	svc := newChainService(t, chainSettings(searxng.URL, firecrawl.URL), allowAllHosts)

	results, err := svc.Search(context.Background(), "llama.cpp", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Title != "llama.cpp" {
		t.Fatalf("unexpected results: %+v", results)
	}

	doc, err := svc.Fetch(context.Background(), "https://example.com/anything")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.Provider != "firecrawl" {
		t.Fatalf("Provider = %q, want firecrawl (it's up and first in chain)", doc.Provider)
	}
}

func TestChain_FirecrawlDown_FallsToDirect(t *testing.T) {
	searxng := searxngServer(t)
	firecrawlDeadURL := closedServer(t) // firecrawl's own service is down
	target := plainPageServer(t)        // the page being fetched is reachable directly

	svc := newChainService(t, chainSettings(searxng.URL, firecrawlDeadURL), allowAllHosts)
	doc, err := svc.Fetch(context.Background(), target.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.Provider != "direct" {
		t.Fatalf("Provider = %q, want direct (firecrawl's service is down)", doc.Provider)
	}
	if !strings.Contains(doc.Text, "direct content") {
		t.Fatalf("unexpected text: %q", doc.Text)
	}
}

func TestChain_SearxngDown_ErrorNoHang(t *testing.T) {
	firecrawl := firecrawlServer(t)
	searxngDeadURL := closedServer(t)
	svc := newChainService(t, chainSettings(searxngDeadURL, firecrawl.URL), allowAllHosts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := svc.Search(ctx, "x", 5)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNoSearchProvider) {
		t.Fatalf("err = %v, want ErrNoSearchProvider", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Search took %v, expected a fast failure (connection refused), not a hang", elapsed)
	}
}

func TestChain_AllFetchProvidersDown(t *testing.T) {
	firecrawlDeadURL := closedServer(t)
	targetDeadURL := closedServer(t) // the target page itself is also unreachable
	svc := newChainService(t, chainSettings("", firecrawlDeadURL), allowAllHosts)

	_, err := svc.Fetch(context.Background(), targetDeadURL)
	if !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("err = %v, want ErrAllProvidersFailed", err)
	}
}

// TestChain_AttemptOrder proves firecrawl is tried before direct — the
// operator's configured order, with direct as the terminus — by having a
// server play the "target page" role, recording which adapter reached it.
func TestChain_AttemptOrder(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	firecrawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, "firecrawl")
		mu.Unlock()
		w.Write([]byte(`{"success":false,"error":"simulated failure"}`)) // fails → falls through
	}))
	defer firecrawl.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, "direct")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("target content"))
	}))
	defer target.Close()

	svc := newChainService(t, chainSettings("", firecrawl.URL), allowAllHosts)
	doc, err := svc.Fetch(context.Background(), target.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.Provider != "direct" {
		t.Fatalf("Provider = %q, want direct", doc.Provider)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "firecrawl" || calls[1] != "direct" {
		t.Fatalf("call order = %v, want [firecrawl direct]", calls)
	}
}

// TestChain_MalformedFirecrawlResponse_FallsThrough proves a degenerate
// (non-JSON) firecrawl response is treated as a failure, not a crash, and
// the chain continues to direct.
func TestChain_MalformedFirecrawlResponse_FallsThrough(t *testing.T) {
	firecrawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not valid json`))
	}))
	defer firecrawl.Close()
	target := plainPageServer(t)

	svc := newChainService(t, chainSettings("", firecrawl.URL), allowAllHosts)
	doc, err := svc.Fetch(context.Background(), target.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.Provider != "direct" {
		t.Fatalf("Provider = %q, want direct", doc.Provider)
	}
}

// TestChain_NeverProbed_StillAttempted (D4): a provider with zero probe
// history — CheckedAt is nil — is still tried on first real use.
// Reachability status is purely observational and never gates an attempt.
func TestChain_NeverProbed_StillAttempted(t *testing.T) {
	searxng := searxngServer(t)
	svc := newChainService(t, chainSettings(searxng.URL, ""), allowAllHosts)

	statuses := svc.Providers(context.Background())
	for _, ps := range statuses {
		if ps.Name == "searxng" && ps.CheckedAt != nil {
			t.Fatalf("expected searxng to have no probe history yet, got CheckedAt=%v", ps.CheckedAt)
		}
	}

	if _, err := svc.Search(context.Background(), "x", 5); err != nil {
		t.Fatalf("Search on a never-probed-but-up provider should succeed, got %v", err)
	}
}

// TestChain_NoBlockUnderExpiredContext proves the chain returns promptly
// (never hangs) when the caller's context is already past its deadline.
func TestChain_NoBlockUnderExpiredContext(t *testing.T) {
	searxng := searxngServer(t)
	firecrawl := firecrawlServer(t)
	svc := newChainService(t, chainSettings(searxng.URL, firecrawl.URL), allowAllHosts)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // ensure the deadline has actually passed

	done := make(chan struct{})
	go func() {
		_, _ = svc.Search(ctx, "x", 5)
		_, _ = svc.Fetch(ctx, "https://example.com")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("chain walk did not return within 5s under an already-expired context")
	}
}

func TestChain_FetchUsesCache_NoSecondHTTPCall(t *testing.T) {
	var hits int
	var mu sync.Mutex
	firecrawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte(`{"success":true,"data":{"markdown":"cached content","metadata":{"title":"t"}}}`))
	}))
	defer firecrawl.Close()

	svc := newChainService(t, chainSettings("", firecrawl.URL), allowAllHosts)
	first, err := svc.Fetch(context.Background(), "https://example.com/page")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if first.Cached {
		t.Fatal("first fetch should not be marked cached")
	}
	second, err := svc.Fetch(context.Background(), "https://example.com/page")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !second.Cached {
		t.Fatal("second fetch should be served from cache")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", hits)
	}
}

func TestChain_Disabled(t *testing.T) {
	s := newFakeSettings()
	s.setRaw(SettingEnabled, `false`)
	svc := newChainService(t, s, allowAllHosts)
	if _, err := svc.Search(context.Background(), "x", 5); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Search err = %v, want ErrDisabled", err)
	}
	if _, err := svc.Fetch(context.Background(), "https://example.com"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Fetch err = %v, want ErrDisabled", err)
	}
}

// TestChain_BlocksNonPublicURL verifies the SSRF guard runs before firecrawl.
// Even with firecrawl up and first in the chain, a non-public URL (loopback,
// tailnet CGNAT) must be rejected — firecrawl bypasses the direct adapter's
// dial-time guard, so validateFetchURL in doFetch is the only check that
// covers it.
func TestChain_BlocksNonPublicURL(t *testing.T) {
	firecrawl := firecrawlServer(t)
	svc := newChainService(t, chainSettings("", firecrawl.URL), nil) // nil = no allow override

	for _, url := range []string{
		"http://127.0.0.1:9999/internal",
		"http://100.100.100.100:8095/v1/tools/status", // tailnet CGNAT
	} {
		_, err := svc.Fetch(context.Background(), url)
		if err == nil {
			t.Fatalf("Fetch(%q): expected SSRF block, got nil", url)
		}
		if !strings.Contains(err.Error(), "non-public") && !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("Fetch(%q): expected SSRF error, got: %v", url, err)
		}
	}
}
