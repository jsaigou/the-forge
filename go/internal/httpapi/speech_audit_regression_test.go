// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
)

// Path/method variants of the two speech calls and of the keys/self call must either be refused by the scope check or reach exactly the intended handler.
func TestAudit_SpeechScopePathVariants(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	key, err := auth.MintScopedKey(t.Context(), "forge-tts", "", authz.RoleOperator, "127.0.0.1", time.Now().Add(time.Hour), authz.ScopeSpeech)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := auth.MintKey(t.Context(), authz.KindForge, "victim", "", authz.RoleAdmin, "", time.Time{})
	for _, c := range []struct{ method, target string }{
		{"POST", "/api/v1/speech/resolve/"}, {"POST", "//api/v1/speech/resolve"}, {"POST", "/api/v1/speech/../keys"},
		{"POST", "/api/v1/speech/resolve/../../keys"}, {"POST", "/api/v1/speech/%72esolve"}, {"POST", "/api/v1/Speech/resolve"},
		{"POST", "/api/v1/speech/resolve?x=/api/v1/keys"}, {"POST", "/api/v1/speech%2Fresolve"}, {"POST", "/api/v1/speech/resolve%2f..%2fkeys"},
		{"HEAD", "/api/v1/speech/resolve"}, {"OPTIONS", "/api/v1/speech/resolve"}, {"GET", "/api/v1/speech/resolve"},
		{"DELETE", "/api/v1/keys/self/"}, {"DELETE", "/api/v1/keys%2Fself"}, {"DELETE", "/api/v1/keys/./self"},
		{"GET", "/api/v1/session/"}, {"GET", "/api/v1/health/x"},
	} {
		req := httptest.NewRequest(c.method, "http://x"+c.target, bytes.NewBufferString(`{"mode":"base"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-HTTP-Method-Override", "DELETE")
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		t.Logf("%-7s %-45s -> %d %.80s", c.method, c.target, rec.Code, rec.Body.String())
	}
	// forged XFF from a non-loopback peer must not satisfy a loopback-bound key
	for _, xff := range []string{"127.0.0.1", "127.0.0.1, 100.64.0.9", "::1"} {
		req := httptest.NewRequest("POST", "/api/v1/speech/resolve", bytes.NewBufferString(`{"mode":"base"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Forwarded-For", xff)
		req.RemoteAddr = "100.64.0.9:1"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("forged XFF %q from tailnet peer: %d", xff, rec.Code)
		}
		// via tailscale serve: loopback peer, real client first in XFF; a client-forged prefix puts its own value FIRST
		req = httptest.NewRequest("POST", "/api/v1/speech/resolve", bytes.NewBufferString(`{"mode":"base"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Forwarded-For", xff)
		req.RemoteAddr = "127.0.0.1:1"
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		t.Logf("loopback peer, XFF %q -> %d", xff, rec.Code)
	}
	{
		req := httptest.NewRequest("DELETE", "http://x/api/v1/keys/%73elf", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		t.Logf("encoded self revoke -> %d %s", rec.Code, rec.Body.String())
	}
	// the self-revoke variants must never have touched the other key
	if code, _ := bearerCall(t, s, other, "GET", "/api/v1/health", ""); code != 200 {
		t.Errorf("other key damaged: %d", code)
	}
}

type ctxPool struct {
	*fakeSpecialistPool
	err error
}

func (p *ctxPool) ResolveSpeech(context.Context, specialist.SpeechRequest) (specialist.SpeechLease, error) {
	return specialist.SpeechLease{}, p.err
}
func (p *ctxPool) ReleaseSpeech(string) bool { return false }

// Regression (was a bug, fixed): a request-context cancellation / deadline from the pool is reported as a hard 500 "internal" instead of a transient 503.
func TestAudit_SpeechResolveContextErrorIsNot500(t *testing.T) {
	for _, e := range []error{context.Canceled, context.DeadlineExceeded} {
		s, base := newSpecialistServer(t, authz.RoleOperator)
		s.deps.Specialists = &ctxPool{fakeSpecialistPool: base, err: e}
		req := httptest.NewRequest("POST", "/api/v1/speech/resolve", bytes.NewBufferString(`{"mode":"base"}`))
		req.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
		req.RemoteAddr = "100.64.0.1:1234"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		t.Logf("%v -> %d %s", e, rec.Code, rec.Body.String())
		if rec.Code == http.StatusInternalServerError {
			t.Errorf("%v became %d %s", e, rec.Code, rec.Body.String())
		}
	}
}
