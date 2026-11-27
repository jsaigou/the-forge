// SPDX-License-Identifier: Apache-2.0

package router

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type classifyCase struct {
	name        string
	status      int
	body        string
	wantStatus  int
	wantFailovr bool
}

// ADR-0020 F5: a 500/501 with a structured deterministic request error is the request's fault, not the backend's.
func TestUpstreamErrorClassification(t *testing.T) {
	notSupported := `{"error":{"code":501,"message":"image input is not supported by this server","type":"not_supported_error"}}`
	cases := []classifyCase{
		{"501 not_supported passes through as 400", 501, notSupported, 400, false},
		{"500 not_supported passes through as 400", 500, notSupported, 400, false},
		{"500 invalid_request passes through as 400", 500, `{"error":{"message":"bad","type":"invalid_request_error"}}`, 400, false},
		// The body a real llama-server (plain upstream build 19) returned for an image sent to a text-only model.
		{"500 server_error 'image input is not supported' passes through as 400", 500, `{"error":{"code":500,"message":"image input is not supported - hint: if this is unexpected, you may need to provide the mmproj","type":"server_error"}}`, 400, false},
		{"500 server_error 'audio input is not supported' passes through as 400", 500, `{"error":{"code":500,"message":"audio input is not supported","type":"server_error"}}`, 400, false},
		{"500 with no structured body fails over", 500, `internal error`, 200, true},
		{"500 server_error type fails over", 500, `{"error":{"message":"oom","type":"server_error"}}`, 200, true},
		{"500 with a string error fails over", 500, `{"error":"boom"}`, 200, true},
		{"503 fails over", 503, `{"error":{"type":"not_supported_error"}}`, 200, true},
		{"502 fails over", 502, ``, 200, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var up2Hits int32
			up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer up1.Close()
			up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&up2Hits, 1)
				_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
			}))
			defer up2.Close()
			p1, p2 := portFromURL(t, up1.URL), portFromURL(t, up2.URL)
			cat := newFakeCatalog()
			cat.setProbe(p1, SlotProbe{Healthy: true})
			cat.setProbe(p2, SlotProbe{Healthy: true})
			cfg := testCfg(
				[]Backend{{Name: "a1", Kind: "foundry_slot", Port: p1}, {Name: "fallback", Kind: "foundry_slot", Port: p2}},
				[]Route{{Model: "m", Primary: "a1", Fallback: []string{"fallback"}}},
			)
			srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			req.RemoteAddr = "100.64.0.1:1234"
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			slotErrs, _ := srv.SlotErrorCount(p1, 0)
			if c.wantFailovr {
				// (a 5xx is currently counted by both ModifyResponse and ErrorHandler, so only "at least one" is pinned)
				if atomic.LoadInt32(&up2Hits) != 1 || slotErrs < 1 {
					t.Errorf("failover=%d slot errors=%d, want 1 and >=1", up2Hits, slotErrs)
				}
				return
			}
			if atomic.LoadInt32(&up2Hits) != 0 || slotErrs != 0 {
				t.Errorf("failover=%d slot errors=%d, want 0 and 0 (a verdict on the request)", up2Hits, slotErrs)
			}
			if rec.Body.String() != c.body {
				t.Errorf("body was altered: %q", rec.Body.String())
			}
			if got := rec.Header().Get(headerUpstreamStatus); got != itoa(c.status) {
				t.Errorf("%s = %q, want %d", headerUpstreamStatus, got, c.status)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// A refused connection is a real backend failure and still fails over and counts as a slot error.
func TestUpstreamRefusedConnectionStillFailsOver(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().(*net.TCPAddr).Port
	l.Close()
	var up2Hits int32
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&up2Hits, 1)
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer up2.Close()
	p2 := portFromURL(t, up2.URL)
	cat := newFakeCatalog()
	cat.setProbe(dead, SlotProbe{Healthy: true})
	cat.setProbe(p2, SlotProbe{Healthy: true})
	cfg := testCfg(
		[]Backend{{Name: "a1", Kind: "foundry_slot", Port: dead}, {Name: "fallback", Kind: "foundry_slot", Port: p2}},
		[]Route{{Model: "m", Primary: "a1", Fallback: []string{"fallback"}}},
	)
	srv := NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if errs, _ := srv.SlotErrorCount(dead, 0); rec.Code != 200 || up2Hits != 1 || errs < 1 {
		t.Errorf("status=%d failover=%d slot errors=%d, want 200, 1, >=1", rec.Code, up2Hits, errs)
	}
}
