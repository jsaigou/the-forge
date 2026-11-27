// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
)

func bearerCall(t *testing.T, s *Server, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ADR-0020 F11: a catalog-scoped key reaches the catalog API and nothing else, even though its role is admin.
func TestCatalogScopedKeyReachesOnlyTheCatalog(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	scoped, err := auth.MintScopedKey(t.Context(), "agent", "", authz.RoleAdmin, "", time.Now().Add(time.Hour), authz.ScopeCatalog)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := auth.MintKey(t.Context(), authz.KindForge, "plain-admin", "", authz.RoleAdmin, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if code, body := bearerCall(t, s, scoped, "GET", "/api/v1/catalog/builds", ""); code != http.StatusOK {
		t.Errorf("scoped key on the catalog: %d %v, want 200", code, body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/status", ""},
		{"POST", "/api/v1/load", `{"mode":"x","slot":"a1"}`},
		{"POST", "/api/v1/unload", `{"slot":"a1"}`},
		{"POST", "/api/v1/keys", `{"kind":"router","name":"escalate"}`},
		{"DELETE", "/api/v1/keys/aaaaaaaaaaaa", ""},
		{"GET", "/api/v1/keys", ""},
		{"PUT", "/api/v1/router/config", `{}`},
		{"GET", "/api/v1/catalogue/builds", ""}, // a lookalike prefix is not the catalog
	} {
		code, body := bearerCall(t, s, scoped, c.method, c.path, c.body)
		if code != http.StatusForbidden || body["code"] != "scope_denied" {
			t.Errorf("scoped %s %s: %d %v, want 403 scope_denied", c.method, c.path, code, body)
		}
	}
	// The same routes are not scope-denied for an unscoped admin key (they may fail for other reasons in this
	// minimal test server, but never with scope_denied).
	for _, p := range []string{"/api/v1/status", "/api/v1/keys"} {
		if _, body := bearerCall(t, s, plain, "GET", p, ""); body["code"] == "scope_denied" {
			t.Errorf("unscoped key was scope-denied on %s", p)
		}
	}
}

func TestKeyRevokesItselfWithoutStepUpAndNothingElse(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	scoped, _ := auth.MintScopedKey(t.Context(), "agent", "", authz.RoleAdmin, "", time.Now().Add(time.Hour), authz.ScopeCatalog)
	other, _ := auth.MintKey(t.Context(), authz.KindForge, "other-admin", "", authz.RoleAdmin, "", time.Time{})

	if code, _ := bearerCall(t, s, scoped, "GET", "/api/v1/catalog/builds", ""); code != 200 {
		t.Fatalf("scoped key not usable before revoke: %d", code)
	}
	if code, body := bearerCall(t, s, scoped, "DELETE", "/api/v1/keys/self", ""); code != 200 || body["ok"] != true {
		t.Fatalf("self-revoke: %d %v, want 200", code, body)
	}
	if code, _ := bearerCall(t, s, scoped, "GET", "/api/v1/catalog/builds", ""); code != http.StatusUnauthorized {
		t.Errorf("a revoked key still works: %d, want 401", code)
	}
	// Revoking ANOTHER key by id still needs the interactive step-up a bearer cannot give.
	if code, _ := bearerCall(t, s, other, "DELETE", "/api/v1/keys/aaaaaaaaaaaa", ""); code != http.StatusForbidden {
		t.Errorf("a bearer key revoking another key: %d, want 403", code)
	}
	// The other key was not touched by the self-revoke.
	if code, body := bearerCall(t, s, other, "GET", "/api/v1/catalog/builds", ""); code != 200 {
		t.Errorf("an unrelated key was affected: %d %v", code, body)
	}
}

func TestScopedKeyCreationRules(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	admin, _ := auth.MintKey(t.Context(), authz.KindForge, "admin-bot", "", authz.RoleAdmin, "", time.Time{})
	for name, body := range map[string]string{
		"no ttl":         `{"kind":"forge","name":"a","role":"admin","scope":"catalog"}`,
		"ttl over a day": `{"kind":"forge","name":"a","role":"admin","scope":"catalog","ttl_seconds":90000}`,
		"unknown scope":  `{"kind":"forge","name":"a","role":"admin","scope":"all","ttl_seconds":3600}`,
		"router kind":    `{"kind":"router","name":"a","scope":"catalog","ttl_seconds":3600}`,
		// The speech scope needs a loopback binding and a year-long life: it is minted on the host with `forge mint-key`, never over HTTP.
		"speech scope over http": `{"kind":"forge","name":"a","role":"operator","scope":"speech","ttl_seconds":3600}`,
	} {
		code, out := bearerCall(t, s, admin, "POST", "/api/v1/keys", body)
		if code != http.StatusUnprocessableEntity || out["fields"] == nil {
			t.Errorf("%s: %d %v, want 422 with fields", name, code, out)
		}
	}
	// A valid scoped request passes validation and then meets the step-up a bearer cannot give.
	code, out := bearerCall(t, s, admin, "POST", "/api/v1/keys", `{"kind":"forge","name":"a","role":"admin","scope":"catalog","ttl_seconds":3600}`)
	if code != http.StatusForbidden || out["error"] != "step_up_required" {
		t.Errorf("valid scoped create via bearer: %d %v, want 403 step_up_required", code, out)
	}
}

func TestScopeAllowsTable(t *testing.T) {
	for _, c := range []struct {
		scope, method, path string
		want                bool
	}{
		{"catalog", "GET", "/api/v1/catalog/configs", true},
		{"catalog", "PATCH", "/api/v1/catalog/builds/3", true},
		{"catalog", "GET", "/api/v1/session", true},
		{"catalog", "POST", "/api/v1/session", false},
		{"catalog", "DELETE", "/api/v1/keys/self", true},
		{"catalog", "DELETE", "/api/v1/keys/abc", false},
		{"catalog", "GET", "/api/v1/catalog", false},
		{"catalog", "POST", "/v1/chat/completions", false},
		{"other", "GET", "/api/v1/catalog/configs", false},
		// The speech scope reaches the two speech-engine calls and nothing else, not even the catalog or the specialist list.
		{"speech", "POST", "/api/v1/speech/resolve", true},
		{"speech", "POST", "/api/v1/speech/release", true},
		{"speech", "GET", "/api/v1/speech/resolve", false},
		{"speech", "PUT", "/api/v1/speech/defaults", false},
		{"speech", "GET", "/api/v1/speech/readiness", true},
		{"speech", "POST", "/api/v1/speech/readiness", false},
		{"catalog", "GET", "/api/v1/speech/readiness", false},
		{"speech", "GET", "/api/v1/speech/defaults", true}, // the read of the engine list, so voice-studio can offer a choice
		{"catalog", "GET", "/api/v1/speech/defaults", false},
		{"speech", "GET", "/api/v1/session", true},
		{"speech", "GET", "/api/v1/health", true},
		{"speech", "DELETE", "/api/v1/keys/self", true},
		{"speech", "DELETE", "/api/v1/keys/abc", false},
		{"speech", "GET", "/api/v1/specialists", false},
		{"speech", "POST", "/api/v1/specialists/load", false},
		{"speech", "GET", "/api/v1/catalog/configs", false},
		{"speech", "POST", "/v1/audio/speech", false},
		{"catalog", "POST", "/api/v1/speech/resolve", false},
		// The duplex scope reaches the three duplex-engine calls and nothing else: not speech, not the catalog, not the specialist list.
		{"duplex", "POST", "/api/v1/duplex/resolve", true},
		{"duplex", "POST", "/api/v1/duplex/renew", true},
		{"duplex", "POST", "/api/v1/duplex/release", true},
		{"duplex", "GET", "/api/v1/duplex/resolve", false},
		{"duplex", "POST", "/api/v1/speech/resolve", false},
		{"duplex", "GET", "/api/v1/speech/defaults", false},
		{"duplex", "GET", "/api/v1/specialists", false},
		{"duplex", "POST", "/api/v1/specialists/load", false},
		{"duplex", "GET", "/api/v1/catalog/configs", false},
		{"duplex", "GET", "/api/v1/session", true},
		{"duplex", "DELETE", "/api/v1/keys/self", true},
		{"duplex", "DELETE", "/api/v1/keys/abc", false},
		{"speech", "POST", "/api/v1/duplex/resolve", false},
		{"catalog", "POST", "/api/v1/duplex/resolve", false},
	} {
		if got := scopeAllows(c.scope, c.method, c.path); got != c.want {
			t.Errorf("scopeAllows(%q, %s %s) = %v, want %v", c.scope, c.method, c.path, got, c.want)
		}
	}
}
