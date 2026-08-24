// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearxngAdapter_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json, got %q", r.URL.Query().Get("format"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"query":"llama.cpp","results":[
			{"title":"llama.cpp","url":"https://github.com/ggml-org/llama.cpp","content":"LLM inference in C/C++","engine":"github","score":1.0}
		]}`))
	}))
	defer srv.Close()

	a := &searxngAdapter{client: http.DefaultClient, userAgent: "test"}
	results, att := a.search(context.Background(), ProviderConfig{BaseURL: srv.URL, Enabled: true}, "llama.cpp", 5)
	if !att.OK {
		t.Fatalf("expected OK, got %+v", att)
	}
	if len(results) != 1 || results[0].Title != "llama.cpp" || results[0].URL != "https://github.com/ggml-org/llama.cpp" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestSearxngAdapter_EmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"query":"x","results":[]}`))
	}))
	defer srv.Close()

	a := &searxngAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.search(context.Background(), ProviderConfig{BaseURL: srv.URL}, "x", 5)
	if att.OK {
		t.Fatal("empty results should not be OK")
	}
}

func TestSearxngAdapter_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	a := &searxngAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.search(context.Background(), ProviderConfig{BaseURL: srv.URL}, "x", 5)
	if att.OK {
		t.Fatal("malformed JSON should not be OK")
	}
}

func TestSearxngAdapter_NonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := &searxngAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.search(context.Background(), ProviderConfig{BaseURL: srv.URL}, "x", 5)
	if att.OK || att.Status != 500 {
		t.Fatalf("expected a non-OK 500 attempt, got %+v", att)
	}
}

func TestSearxngAdapter_LimitRespected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"title":"a","url":"https://a"},{"title":"b","url":"https://b"},{"title":"c","url":"https://c"}]}`))
	}))
	defer srv.Close()

	a := &searxngAdapter{client: http.DefaultClient, userAgent: "test"}
	results, att := a.search(context.Background(), ProviderConfig{BaseURL: srv.URL}, "x", 2)
	if !att.OK || len(results) != 2 {
		t.Fatalf("expected 2 results capped by limit, got %d (att=%+v)", len(results), att)
	}
}
