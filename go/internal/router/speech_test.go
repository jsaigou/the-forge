// SPDX-License-Identifier: Apache-2.0

package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTTSUpstream returns a fake forge-tts that records the request path +
// headers and echoes a minimal OK response.
func newTTSUpstream(t *testing.T) (*httptest.Server, *struct {
	path  string
	token string
}) {
	t.Helper()
	rec := &struct {
		path  string
		token string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.token = r.Header.Get("X-Forge-Internal-Token")
		switch {
		case strings.HasSuffix(r.URL.Path, "/audio/speech"):
			w.Header().Set("Content-Type", "audio/wav")
			w.Write([]byte("FAKEWAV"))
		case strings.HasSuffix(r.URL.Path, "/voices"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"voices":[{"id":"billie"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestSpeech_Passthrough(t *testing.T) {
	upstream, rec := newTTSUpstream(t)
	cfg := testCfg(nil, nil)
	cfg.TTSURL = upstream.URL + "/v1"
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"qwen3-tts","voice":"billie","input":"hi"}`))
	req.RemoteAddr = "100.64.0.1:1234" // tailnet
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.path != "/v1/audio/speech" {
		t.Errorf("upstream path = %q, want /v1/audio/speech", rec.path)
	}
	if w.Body.String() != "FAKEWAV" {
		t.Errorf("body = %q, want FAKEWAV", w.Body.String())
	}
}

func TestVoices_Passthrough(t *testing.T) {
	upstream, rec := newTTSUpstream(t)
	cfg := testCfg(nil, nil)
	cfg.TTSURL = upstream.URL + "/v1"
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("GET", "/v1/voices", nil)
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if rec.path != "/v1/voices" {
		t.Errorf("upstream path = %q, want /v1/voices", rec.path)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["voices"]; !ok {
		t.Errorf("response missing voices key: %v", resp)
	}
}

// TestVoices_OnlyGET: a mutating verb must not route to this passthrough at
// all — forge-tts's own auth only guards /v1/voices* for non-GET methods,
// so this endpoint was deliberately never registered for POST/PUT/DELETE.
func TestVoices_OnlyGET(t *testing.T) {
	upstream, _ := newTTSUpstream(t)
	cfg := testCfg(nil, nil)
	cfg.TTSURL = upstream.URL + "/v1"
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("POST", "/v1/voices", strings.NewReader(`{}`))
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("POST /v1/voices should not route to the GET-only passthrough, got 200")
	}
}

// TestSpeech_StripsInboundInternalToken: a consumer must never be able to
// smuggle its own X-Forge-Internal-Token value through to forge-tts.
func TestSpeech_StripsInboundInternalToken(t *testing.T) {
	upstream, rec := newTTSUpstream(t)
	cfg := testCfg(nil, nil)
	cfg.TTSURL = upstream.URL + "/v1"
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}})

	req := httptest.NewRequest("GET", "/v1/voices", nil)
	req.Header.Set("X-Forge-Internal-Token", "smuggled-value")
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.token != "" {
		t.Errorf("upstream saw X-Forge-Internal-Token = %q, want stripped (empty)", rec.token)
	}
}

func TestSpeech_TailnetAuth(t *testing.T) {
	upstream, _ := newTTSUpstream(t)
	cfg := testCfg(nil, nil)
	cfg.TTSURL = upstream.URL + "/v1"
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"}})

	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"input":"x"}`))
	req.RemoteAddr = "8.8.8.8:1234" // non-tailnet, no bearer
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-tailnet no-bearer = %d, want 401", w.Code)
	}
}

func TestSpeech_NotConfigured(t *testing.T) {
	cfg := testCfg(nil, nil) // TTSURL empty
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}})
	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"input":"x"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d, want 503", w.Code)
	}
}
