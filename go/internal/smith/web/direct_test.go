// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDirectAdapter_HTMLExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>Test Page</title><style>.x{color:red}</style></head>
			<body><nav>skip me</nav><p>Hello <b>world</b>.</p><script>evil()</script><footer>skip me too</footer></body></html>`))
	}))
	defer srv.Close()

	a := &directAdapter{client: http.DefaultClient, userAgent: "test"}
	doc, att := a.fetch(context.Background(), ProviderConfig{}, srv.URL)
	if !att.OK {
		t.Fatalf("expected OK, got %+v", att)
	}
	if doc.Title != "Test Page" {
		t.Fatalf("Title = %q, want %q", doc.Title, "Test Page")
	}
	if !strings.Contains(doc.Text, "Hello world") {
		t.Fatalf("Text missing expected content: %q", doc.Text)
	}
	if strings.Contains(doc.Text, "skip me") || strings.Contains(doc.Text, "evil") || strings.Contains(doc.Text, "color:red") {
		t.Fatalf("Text should exclude script/style/nav/footer content: %q", doc.Text)
	}
}

func TestDirectAdapter_PlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("just plain text"))
	}))
	defer srv.Close()

	a := &directAdapter{client: http.DefaultClient, userAgent: "test"}
	doc, att := a.fetch(context.Background(), ProviderConfig{}, srv.URL)
	if !att.OK || doc.Text != "just plain text" {
		t.Fatalf("unexpected result: doc=%+v att=%+v", doc, att)
	}
}

func TestDirectAdapter_JSONPassthrough(t *testing.T) {
	// Used by signals.go's GitHub API check — must not attempt HTML
	// extraction on a JSON body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"state":"closed","merged":true}`))
	}))
	defer srv.Close()

	a := &directAdapter{client: http.DefaultClient, userAgent: "test"}
	doc, att := a.fetch(context.Background(), ProviderConfig{}, srv.URL)
	if !att.OK || doc.Text != `{"state":"closed","merged":true}` {
		t.Fatalf("unexpected result: doc=%+v att=%+v", doc, att)
	}
}

func TestDirectAdapter_UnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.4 binary garbage"))
	}))
	defer srv.Close()

	a := &directAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.fetch(context.Background(), ProviderConfig{}, srv.URL)
	if att.OK {
		t.Fatal("application/pdf should not be OK")
	}
}

func TestDirectAdapter_EmptyExtractedText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><script>only script content</script></body></html>`))
	}))
	defer srv.Close()

	a := &directAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.fetch(context.Background(), ProviderConfig{}, srv.URL)
	if att.OK {
		t.Fatal("a page with only script content should extract to empty and not be OK")
	}
}

func TestDirectAdapter_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := &directAdapter{client: http.DefaultClient, userAgent: "test"}
	_, att := a.fetch(context.Background(), ProviderConfig{}, srv.URL)
	if att.OK || att.Status != 404 {
		t.Fatalf("expected non-OK 404, got %+v", att)
	}
}

func TestDirectAdapter_TruncatedLargeBody(t *testing.T) {
	big := strings.Repeat("a", maxBodyBytes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(big))
	}))
	defer srv.Close()

	a := &directAdapter{client: http.DefaultClient, userAgent: "test"}
	doc, att := a.fetch(context.Background(), ProviderConfig{}, srv.URL)
	if !att.OK {
		t.Fatalf("expected OK, got %+v", att)
	}
	if !doc.Truncated {
		t.Fatal("expected Truncated=true for an over-cap body")
	}
	if len(doc.Text) > maxBodyBytes {
		t.Fatalf("Text length %d exceeds maxBodyBytes %d", len(doc.Text), maxBodyBytes)
	}
}

func TestGuardedHTTPClient_BlocksLoopbackByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never be reached"))
	}))
	defer srv.Close()

	client := newGuardedHTTPClient(nil) // no override — the real SSRF guard
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("expected the guard to reject a loopback httptest server")
	}
}

func TestGuardedHTTPClient_AllowOverridePermitsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := newGuardedHTTPClient(func(host string) bool { return true })
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("expected the override to permit loopback, got %v", err)
	}
	resp.Body.Close()
}

func TestValidateFetchURL_BlocksLoopback(t *testing.T) {
	if err := validateFetchURL(context.Background(), "http://127.0.0.1:9999/test", nil); err == nil {
		t.Fatal("expected validateFetchURL to reject a loopback URL")
	}
}

func TestValidateFetchURL_BlocksCGNAT(t *testing.T) {
	if err := validateFetchURL(context.Background(), "http://100.100.100.100:8095/v1/tools/status", nil); err == nil {
		t.Fatal("expected validateFetchURL to reject a CGNAT (tailnet) URL")
	}
}

func TestValidateFetchURL_BlocksFileScheme(t *testing.T) {
	if err := validateFetchURL(context.Background(), "file:///etc/passwd", nil); err == nil {
		t.Fatal("expected validateFetchURL to reject a file:// URL")
	}
}

func TestValidateFetchURL_AllowOverride(t *testing.T) {
	if err := validateFetchURL(context.Background(), "http://127.0.0.1:9999/test", func(string) bool { return true }); err != nil {
		t.Fatalf("expected validateFetchURL to allow loopback via override, got: %v", err)
	}
}
