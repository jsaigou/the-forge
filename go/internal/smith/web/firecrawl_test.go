// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFirecrawlAdapter_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/scrape" {
			t.Errorf("expected /v1/scrape, got %s", r.URL.Path)
		}
		w.Write([]byte(`{"success":true,"data":{"markdown":"Example Domain\n===","metadata":{"title":"Example Domain","sourceURL":"https://example.com","statusCode":200,"contentType":"text/html"}}}`))
	}))
	defer srv.Close()

	a := &firecrawlAdapter{client: http.DefaultClient, userAgent: "test"}
	doc, att := a.fetch(context.Background(), ProviderConfig{BaseURL: srv.URL}, "https://example.com")
	if !att.OK {
		t.Fatalf("expected OK, got %+v", att)
	}
	if doc.Title != "Example Domain" || !strings.Contains(doc.Text, "Example Domain") {
		t.Fatalf("unexpected doc: %+v", doc)
	}
	if doc.Provider != "firecrawl" {
		t.Fatalf("Provider = %q, want firecrawl", doc.Provider)
	}
}

func TestFirecrawlAdapter_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":false,"error":"could not render page"}`))
	}))
	defer srv.Close()

	a := &firecrawlAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.fetch(context.Background(), ProviderConfig{BaseURL: srv.URL}, "https://example.com")
	if att.OK {
		t.Fatal("success:false should not be OK")
	}
	if att.Detail != "could not render page" {
		t.Fatalf("Detail = %q, want the upstream error string", att.Detail)
	}
}

func TestFirecrawlAdapter_EmptyMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"markdown":"","metadata":{}}}`))
	}))
	defer srv.Close()

	a := &firecrawlAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.fetch(context.Background(), ProviderConfig{BaseURL: srv.URL}, "https://example.com")
	if att.OK {
		t.Fatal("empty markdown should not be OK (falls through to direct)")
	}
}

func TestFirecrawlAdapter_APIKeySentAsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"success":true,"data":{"markdown":"x","metadata":{}}}`))
	}))
	defer srv.Close()

	a := &firecrawlAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.fetch(context.Background(), ProviderConfig{BaseURL: srv.URL, APIKey: "sk-test-123"}, "https://example.com")
	if !att.OK {
		t.Fatalf("expected OK, got %+v", att)
	}
	if gotAuth != "Bearer sk-test-123" {
		t.Fatalf("Authorization header = %q, want Bearer sk-test-123", gotAuth)
	}
}

func TestFirecrawlAdapter_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a := &firecrawlAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.fetch(context.Background(), ProviderConfig{BaseURL: srv.URL}, "https://example.com")
	if att.OK || att.Status != 503 {
		t.Fatalf("expected non-OK 503, got %+v", att)
	}
}
