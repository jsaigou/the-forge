// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc, token string) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: func() string { return token }}
}

func TestSearchParsesResultsAndSendsAuthHeader(t *testing.T) {
	var gotAuth, gotQuery, gotFull string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query().Get("search")
		gotFull = r.URL.Query().Get("full")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"org/model-a","author":"org","downloads":1000,"likes":10,"tags":["gguf"],"gated":false,"pipeline_tag":"text-generation","lastModified":"2026-01-01"},
			{"id":"org/model-b","author":"org","downloads":500,"likes":2,"tags":["gguf"],"gated":"manual","pipeline_tag":"text-generation","lastModified":"2026-01-02"}
		]`))
	}, "secret-token-value")

	got, err := c.Search(context.Background(), Query{Text: "qwen"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotAuth != "Bearer secret-token-value" {
		t.Errorf("Authorization header = %q, want Bearer secret-token-value", gotAuth)
	}
	if gotQuery != "qwen" {
		t.Errorf("search query = %q, want qwen", gotQuery)
	}
	if gotFull != "true" {
		t.Errorf("full param = %q, want true — live-verified 2026-08-26: the non-full response omits author/gated/lastModified entirely", gotFull)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if got[0].ID != "org/model-a" || got[0].Gated != false {
		t.Errorf("result[0] = %+v", got[0])
	}
	if got[1].Gated != true {
		t.Errorf("result[1].Gated = %v, want true (string \"manual\" means gated)", got[1].Gated)
	}
}

func TestSearchNoTokenSendsNoAuthHeader(t *testing.T) {
	var gotAuth string
	sawAuth := false
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		sawAuth = r.Header.Get("Authorization") != ""
		_, _ = w.Write([]byte(`[]`))
	}, "")

	if _, err := c.Search(context.Background(), Query{Text: "x"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if sawAuth {
		t.Errorf("Authorization header = %q, want none when Token() returns empty", gotAuth)
	}
}

func TestSearchLimitClampedToMax(t *testing.T) {
	var gotLimit string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`[]`))
	}, "")
	if _, err := c.Search(context.Background(), Query{Text: "x", Limit: 9999}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotLimit != "50" {
		t.Errorf("limit sent = %q, want clamped to MaxSearchLimit (50)", gotLimit)
	}
}

func TestTreeRecursesAndParsesFiles(t *testing.T) {
	var gotPath, gotQuery string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{"type":"directory","path":"Q4_K_M","size":0},
			{"type":"file","path":"Q4_K_M/model-00001-of-00002.gguf","size":4000000000},
			{"type":"file","path":"Q4_K_M/model-00002-of-00002.gguf","size":3500000000},
			{"type":"file","path":"README.md","size":1200}
		]`))
	}, "")

	files, err := c.Tree(context.Background(), "org/model", "")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if !strings.Contains(gotPath, "/api/models/org/model/tree/main") {
		t.Errorf("path = %q, want the /tree/main endpoint (revision default)", gotPath)
	}
	if !strings.Contains(gotQuery, "recursive=1") {
		t.Errorf("query = %q, want recursive=1", gotQuery)
	}
	var shardCount int
	for _, f := range files {
		if strings.Contains(f.Path, "Q4_K_M/model-") {
			shardCount++
		}
	}
	if shardCount != 2 {
		t.Errorf("found %d shard files under Q4_K_M/, want 2 — this is the recursion fix over sourcing.go's root-only read", shardCount)
	}
}

func TestGatedRepoReturnsErrGated(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gated", http.StatusForbidden)
	}, "")
	_, err := c.Tree(context.Background(), "org/gated-model", "main")
	if !errors.Is(err, ErrGated) {
		t.Fatalf("err = %v, want ErrGated", err)
	}
}

func TestUnauthorizedAlsoReturnsErrGated(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}, "")
	_, err := c.Card(context.Background(), "org/private-model")
	if !errors.Is(err, ErrGated) {
		t.Fatalf("err = %v, want ErrGated", err)
	}
}

func TestNotFoundReturnsErrNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}, "")
	_, err := c.Tree(context.Background(), "org/does-not-exist", "main")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestErrorNeverContainsTheToken(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server exploded", http.StatusInternalServerError)
	}, "sk-hf-super-secret-do-not-leak")
	_, err := c.Tree(context.Background(), "org/model", "main")
	if err == nil {
		t.Fatal("expected an error on 500")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error string leaked the token: %v", err)
	}
}

func TestCardParsesLicenseAndBaseModel(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"tags": ["gguf", "text-generation"],
			"cardData": {"license": "apache-2.0", "base_model": ["org/base-model"]},
			"description": "A fine-tuned model."
		}`))
	}, "")
	card, err := c.Card(context.Background(), "org/model")
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	if card.License != "apache-2.0" || card.BaseModel != "org/base-model" {
		t.Errorf("Card = %+v", card)
	}
}

func TestCardBaseModelAsPlainString(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cardData": {"license": "mit", "base_model": "org/base"}}`))
	}, "")
	card, err := c.Card(context.Background(), "org/model")
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	if card.BaseModel != "org/base" {
		t.Errorf("BaseModel = %q, want org/base (string form)", card.BaseModel)
	}
}

func TestCardWithNoFrontMatterIsNotAnError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, "")
	card, err := c.Card(context.Background(), "org/bare-model")
	if err != nil {
		t.Fatalf("Card on an empty card should not error: %v", err)
	}
	if card.License != "" || card.BaseModel != "" {
		t.Errorf("Card = %+v, want all-empty for a bare response", card)
	}
}

func TestEmptyRepoRejected(t *testing.T) {
	c := &Client{}
	if _, err := c.Tree(context.Background(), "  ", "main"); err == nil {
		t.Error("Tree with an empty repo should be rejected")
	}
	if _, err := c.Card(context.Background(), ""); err == nil {
		t.Error("Card with an empty repo should be rejected")
	}
}

// ── Search's official-fallback enrichment ──────────────────────────────────

func TestSearchAddsOfficialFallbackWhenNoneOfficial(t *testing.T) {
	var repoInfoPath string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			// The gguf-filtered search: every result is a third-party
			// requantization, none from "inclusionAI".
			_, _ = w.Write([]byte(`[
				{"id":"bartowski/Ling-3.0-flash-GGUF","author":"bartowski","downloads":100,"tags":["gguf","base_model:inclusionAI/Ling-3.0-flash","base_model:quantized:inclusionAI/Ling-3.0-flash"],"gated":false},
				{"id":"mradermacher/Ling-3.0-flash-GGUF","author":"mradermacher","downloads":50,"tags":["gguf","base_model:inclusionAI/Ling-3.0-flash","base_model:quantized:inclusionAI/Ling-3.0-flash"],"gated":false}
			]`))
			return
		}
		repoInfoPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":"inclusionAI/Ling-3.0-flash","author":"inclusionAI","downloads":17758,"likes":372,"tags":["safetensors","bailing_hybrid"],"gated":false}`))
	}, "")

	got, err := c.Search(context.Background(), Query{Text: "Ling-3.0"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if repoInfoPath != "/api/models/inclusionAI/Ling-3.0-flash" {
		t.Fatalf("repo-info fetched from %q, want the base model's own path", repoInfoPath)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3 (1 synthetic + 2 real)", len(got))
	}
	if got[0].ID != "inclusionAI/Ling-3.0-flash" || !got[0].NoGGUF {
		t.Errorf("results[0] = %+v, want the official no-gguf entry first", got[0])
	}
	if got[1].NoGGUF || got[2].NoGGUF {
		t.Errorf("real GGUF results must not carry NoGGUF: %+v / %+v", got[1], got[2])
	}
}

func TestSearchSkipsFallbackWhenOfficialAlreadyPresent(t *testing.T) {
	var repoInfoCalls int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			_, _ = w.Write([]byte(`[
				{"id":"Qwen/Qwen2.5-Coder-7B-Instruct-GGUF","author":"Qwen","downloads":1000,"tags":["gguf","base_model:Qwen/Qwen2.5-Coder-7B-Instruct","base_model:quantized:Qwen/Qwen2.5-Coder-7B-Instruct"],"gated":false}
			]`))
			return
		}
		atomic.AddInt32(&repoInfoCalls, 1)
		_, _ = w.Write([]byte(`{}`))
	}, "")

	got, err := c.Search(context.Background(), Query{Text: "Qwen2.5-Coder"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1 — no synthetic entry when an official GGUF already exists", len(got))
	}
	if atomic.LoadInt32(&repoInfoCalls) != 0 {
		t.Error("must not fetch repo info when the publisher already has a real GGUF result — wasted call")
	}
}

func TestSearchFallbackBestEffortOnFetchFailure(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			_, _ = w.Write([]byte(`[
				{"id":"bartowski/Ling-3.0-flash-GGUF","author":"bartowski","downloads":100,"tags":["gguf","base_model:inclusionAI/Ling-3.0-flash","base_model:quantized:inclusionAI/Ling-3.0-flash"],"gated":false}
			]`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}, "")

	got, err := c.Search(context.Background(), Query{Text: "Ling-3.0"})
	if err != nil {
		t.Fatalf("Search must succeed even when the fallback fetch fails: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want just the 1 real result (fallback is best-effort)", len(got))
	}
}

func TestSearchFallbackBoundedToMaxChecks(t *testing.T) {
	var repoInfoCalls int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			_, _ = w.Write([]byte(`[
				{"id":"q1/a-GGUF","author":"q1","downloads":10,"tags":["gguf","base_model:org1/model-a","base_model:quantized:org1/model-a"],"gated":false},
				{"id":"q2/b-GGUF","author":"q2","downloads":10,"tags":["gguf","base_model:org2/model-b","base_model:quantized:org2/model-b"],"gated":false},
				{"id":"q3/c-GGUF","author":"q3","downloads":10,"tags":["gguf","base_model:org3/model-c","base_model:quantized:org3/model-c"],"gated":false},
				{"id":"q4/d-GGUF","author":"q4","downloads":10,"tags":["gguf","base_model:org4/model-d","base_model:quantized:org4/model-d"],"gated":false}
			]`))
			return
		}
		atomic.AddInt32(&repoInfoCalls, 1)
		_, _ = w.Write([]byte(`{}`))
	}, "")

	if _, err := c.Search(context.Background(), Query{Text: "broad query"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := atomic.LoadInt32(&repoInfoCalls); got != maxOfficialFallbackChecks {
		t.Errorf("repo-info fetches = %d, want exactly the bound (%d)", got, maxOfficialFallbackChecks)
	}
}
