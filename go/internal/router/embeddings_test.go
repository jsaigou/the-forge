// SPDX-License-Identifier: Apache-2.0

package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newEmbeddingUpstream returns a fake embedding service that records the
// request path + body and echoes an OpenAI-shaped embeddings response.
func newEmbeddingUpstream(t *testing.T) (*httptest.Server, *struct {
	path string
	body string
}) {
	t.Helper()
	rec := &struct {
		path string
		body string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		rec.body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"qwen3-embed"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestEmbeddings_Passthrough(t *testing.T) {
	upstream, rec := newEmbeddingUpstream(t)
	cfg := testCfg(nil, nil)
	cfg.EmbeddingURL = upstream.URL + "/v1"
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}})

	reqBody := `{"model":"qwen3-embed","input":"hello world"}`
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(reqBody))
	req.RemoteAddr = "100.64.0.1:1234" // tailnet
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.path != "/v1/embeddings" {
		t.Errorf("upstream path = %q, want /v1/embeddings", rec.path)
	}
	// Body passes through byte-for-byte — no model rewrite, no inspection.
	if rec.body != reqBody {
		t.Errorf("upstream body = %q, want %q (must pass through untouched)", rec.body, reqBody)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("response object = %v, want list", resp["object"])
	}
}

func TestEmbeddings_TailnetAuth(t *testing.T) {
	upstream, _ := newEmbeddingUpstream(t)
	cfg := testCfg(nil, nil)
	cfg.EmbeddingURL = upstream.URL + "/v1"
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})

	// Non-tailnet without a bearer → 401 (same as the chat path).
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"x"}`))
	req.RemoteAddr = "8.8.8.8:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-tailnet no-bearer = %d, want 401", w.Code)
	}

	// Tailnet source needs no bearer → passes through.
	req2 := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"x"}`))
	req2.RemoteAddr = "100.100.100.100:1234"
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("tailnet no-bearer = %d, want 200", w2.Code)
	}
}

func TestEmbeddings_NotConfigured(t *testing.T) {
	cfg := testCfg(nil, nil) // EmbeddingURL empty
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}})
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"x"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d, want 503", w.Code)
	}
}

func TestEmbeddings_UpstreamDown_502(t *testing.T) {
	cfg := testCfg(nil, nil)
	cfg.EmbeddingURL = "http://127.0.0.1:1/v1" // nothing listening
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}})
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"x"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("upstream down = %d, want 502", w.Code)
	}
}

func TestEmbeddings_ConfigValidationRejectsBadURL(t *testing.T) {
	_, err := NewConfig(RouterConfig{EmbeddingURL: "not-a-url"})
	if err == nil {
		t.Fatal("NewConfig must reject a non-absolute embedding_url")
	}
}

func TestEmbeddings_SkeletonModeUnconfigured(t *testing.T) {
	// New() (no deps) serves only /healthz; /v1/embeddings must not panic and
	// returns 503 (unconfigured), never 200.
	srv := New()
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"x"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("skeleton embeddings = 200, want non-200 (unconfigured)")
	}
}
