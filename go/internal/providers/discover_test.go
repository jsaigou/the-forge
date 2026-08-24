// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

const deepSeekShapeBody = `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"42.17","granted_balance":"0.00","topped_up_balance":"42.17"}]}`

// recordingClient is a fake httpClient that records every request URL and
// always answers 404 — used to prove WHICH candidates Discover tries
// without making any real network call (deepseek's curated endpoint is a
// real https:// URL that must never actually be dialed from a unit test).
type recordingClient struct {
	urls []string
}

func (c *recordingClient) Do(req *http.Request) (*http.Response, error) {
	c.urls = append(c.urls, req.URL.String())
	return &http.Response{StatusCode: 404, Body: http.NoBody, Header: http.Header{}}, nil
}

func TestDiscoverKnownProviderTriesCuratedEndpointOnly(t *testing.T) {
	rec := &recordingClient{}
	row := store.ProviderRow{Name: "deepseek", APIKey: "sk-x", BillingEnabled: true}
	result := Discover(context.Background(), Deps{HTTPClient: rec}, row)
	if len(rec.urls) != 1 || rec.urls[0] != "https://api.deepseek.com/user/balance" {
		t.Fatalf("expected exactly the curated deepseek endpoint dialed, got %+v", rec.urls)
	}
	if len(result.Tried) != 1 || result.Tried[0].URL != "https://api.deepseek.com/user/balance" {
		t.Fatalf("expected exactly the curated deepseek endpoint tried, got %+v", result.Tried)
	}
}

// TestDiscoverOpenRouterFallsThroughToGenericGuesses documents a deliberate
// choice (Sprint E, credits.go/discover.go doc comments): "openrouter" is
// NOT in knownBillingEndpoints even though credits.go has a real dispatch
// case for it, because Discover's detection signal (genericCreditsClient,
// the DeepSeek shape) can never recognize OpenRouter's real response shape
// ({"data":{"limit_remaining":...}}) — adding it would make candidateURLs
// return that one real endpoint exclusively and then guarantee a false
// "not found" on every call. This test would fail loudly if someone "fixed"
// that by adding openrouter back to the curated map without also fixing
// the detection parser.
func TestDiscoverOpenRouterFallsThroughToGenericGuesses(t *testing.T) {
	rec := &recordingClient{}
	row := store.ProviderRow{Name: "openrouter", APIKey: "sk-or-x", TargetURL: "https://openrouter.ai/api/v1", BillingEnabled: true}
	_ = Discover(context.Background(), Deps{HTTPClient: rec}, row)
	for _, u := range rec.urls {
		if u == "https://openrouter.ai/api/v1/key" {
			t.Fatalf("Discover dialed OpenRouter's real key-info endpoint (%s) via the generic guesses — if knownBillingEndpoints now includes it, the detection parser must be fixed too, not just the candidate list", u)
		}
	}
}

func TestDiscoverGenericHostFindsBalanceEndpoint(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		if r.URL.Path == "/v1/credits" {
			_, _ = w.Write([]byte(deepSeekShapeBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	row := store.ProviderRow{Name: "newprov", APIKey: "sk-x", TargetURL: srv.URL + "/v1", BillingEnabled: true}
	result := Discover(context.Background(), Deps{}, row)
	if !result.Found {
		t.Fatalf("expected Found=true, tried: %+v (last hit path %q)", result.Tried, hitPath)
	}
	if result.URL != srv.URL+"/v1/credits" {
		t.Errorf("URL = %q, want the /v1/credits candidate", result.URL)
	}
	if result.Cred.BalanceNative == nil || *result.Cred.BalanceNative != 42.17 {
		t.Errorf("Cred.BalanceNative = %v, want 42.17", result.Cred.BalanceNative)
	}
	// Candidates are tried in order; /v1/credits is not the first
	// genericBillingPaths entry, so earlier ones must have been tried and
	// failed (proving order + short-circuit-on-first-hit both work).
	if len(result.Tried) < 2 {
		t.Errorf("expected at least 2 candidates tried before the hit, got %+v", result.Tried)
	}
	for _, a := range result.Tried[:len(result.Tried)-1] {
		if a.Parsed {
			t.Errorf("candidate %q before the real winner reported Parsed=true unexpectedly", a.URL)
		}
	}
}

func TestDiscoverNoHostNoCandidates(t *testing.T) {
	row := store.ProviderRow{Name: "newprov", APIKey: "sk-x", BillingEnabled: true} // no TargetURL
	result := Discover(context.Background(), Deps{}, row)
	if result.Found || len(result.Tried) != 0 {
		t.Fatalf("expected no candidates without a target_url, got %+v", result)
	}
}

func TestDiscoverNothingFoundReportsAllAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	row := store.ProviderRow{Name: "newprov", APIKey: "sk-x", TargetURL: srv.URL, BillingEnabled: true}
	result := Discover(context.Background(), Deps{}, row)
	if result.Found {
		t.Fatalf("expected Found=false when every candidate 404s, got %+v", result)
	}
	if len(result.Tried) != len(genericBillingPaths) {
		t.Errorf("expected all %d generic candidates tried (no openapi.json present), got %d: %+v",
			len(genericBillingPaths), len(result.Tried), result.Tried)
	}
	for _, a := range result.Tried {
		if a.Parsed {
			t.Errorf("candidate %q: Parsed=true on a 404 server", a.URL)
		}
	}
}

func TestDiscoverOpenAPIProbeFindsBillingPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi.json":
			_, _ = w.Write([]byte(`{"paths":{"/v2/chat":{},"/v2/account/credit-balance":{}}}`))
		case "/v2/account/credit-balance":
			_, _ = w.Write([]byte(deepSeekShapeBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	row := store.ProviderRow{Name: "spec-provider", APIKey: "sk-x", TargetURL: srv.URL, BillingEnabled: true}
	result := Discover(context.Background(), Deps{}, row)
	if !result.Found {
		t.Fatalf("expected the openapi.json-discovered path to be found, tried: %+v", result.Tried)
	}
	if result.URL != srv.URL+"/v2/account/credit-balance" {
		t.Errorf("URL = %q, want the openapi-discovered candidate", result.URL)
	}
}

func TestCreditsFetchDisabledShortCircuits(t *testing.T) {
	// A billing_enabled=false provider must never be probed at all — the
	// handler proves this with a server that fails the test if hit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("credits.fetch made an HTTP request despite billing_enabled=false")
	}))
	defer srv.Close()

	c := newCreditsClients(http.DefaultClient, 0)
	row := store.ProviderRow{Name: "deepseek", APIKey: "sk-x", CreditsURL: srv.URL, BillingEnabled: false}
	got := c.fetch(context.Background(), row)
	if got.Supported {
		t.Errorf("Supported = true, want false when billing_enabled=false")
	}
}
