// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/store"
)

// newWebAuthnTestServer builds a Server with WebAuthnService wired,
// backed by a real store.
func newWebAuthnTestServer(t *testing.T) (*Server, *authz.Authorizer, *store.DB, *authz.WebAuthnService) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	auth := authz.New(db)

	// Create the user + a session.
	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Set up WebAuthn RP config in settings.
	raw, _ := json.Marshal("example-tailnet.ts.net")
	db.Settings().Set(context.Background(), "auth.webauthn.rp_id", raw)
	raw, _ = json.Marshal("Forge")
	db.Settings().Set(context.Background(), "auth.webauthn.rp_name", raw)

	webAuthnSvc := authz.NewWebAuthnService(
		NewWebAuthnCredentialStoreAdapter(db.WebAuthnCredentials()),
		NewWebAuthnUserStoreAdapter(db.Users()),
	)

	policyStore := authz.NewPolicyStore(authz.SettingsAdapter{
		GetFn: func(ctx context.Context, key string) ([]byte, error) {
			return db.Settings().Get(ctx, key)
		},
		SetFn: func(ctx context.Context, key string, value []byte) error {
			return db.Settings().Set(ctx, key, value)
		},
	})

	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
	})
	s := New(Deps{
		Snapshots:          collector.NewStatic(nil),
		Auth:               auth,
		AuthSetup:          auth,
		Events:             events,
		Publish:            events,
		Engine:             &engine.Stub{},
		Config:             func() *config.Config { return cfg },
		Hostname:           "test-host",
		Sessions:           db.Sessions(),
		Settings:           db.Settings(),
		Audit:              db.Audit(),
		IdentityLinks:      db.IdentityLinks(),
		TOTPStore:          db.TOTP(),
		Keys:               db.Keys(),
		PolicyStore:        policyStore,
		StepUpTTL:          authz.DefaultStepUpTTL,
		NetworkDefaultRole: authz.RoleViewer,
		StepUpVerifier:     auth,
		KeyManager:         auth,
		WebAuthnService:    webAuthnSvc,
	})
	t.Cleanup(func() { s.Close() })
	return s, auth, db, webAuthnSvc
}

// loginAndGetCookie logs in and returns the session cookie.
func loginAndGetCookie(t *testing.T, s *Server, username, password string) *http.Cookie {
	t.Helper()
	rec := doForm(t, s.Handler(), "POST", "/login", url.Values{
		"username": {username},
		"password": {password},
	})
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestWebAuthnRegisterBegin(t *testing.T) {
	s, _, _, _ := newWebAuthnTestServer(t)
	cookie := loginAndGetCookie(t, s, "testuser", "hunter2hunter2")

	req := httptest.NewRequest("POST", "/api/v1/auth/webauthn/register/begin", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	req.Header.Set("Origin", "https://ops.example.ts.net")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register begin = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp webauthnBeginRegisterResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Options.Challenge == "" {
		t.Error("challenge should not be empty")
	}
	if resp.Options.RP.ID != "example-tailnet.ts.net" {
		t.Errorf("RP ID = %q, want example-tailnet.ts.net", resp.Options.RP.ID)
	}
	if resp.Options.RP.Name != "Forge" {
		t.Errorf("RP Name = %q, want Forge", resp.Options.RP.Name)
	}
	// Challenge cookie should be set.
	var challengeCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_wa_challenge" {
			challengeCookie = c
		}
	}
	if challengeCookie == nil || challengeCookie.Value == "" {
		t.Error("challenge cookie should be set")
	}
}

func TestWebAuthnRegisterBeginNoService(t *testing.T) {
	s, _, _, _ := newWebAuthnTestServer(t)
	s.deps.WebAuthnService = nil // unplug
	cookie := loginAndGetCookie(t, s, "testuser", "hunter2hunter2")

	req := httptest.NewRequest("POST", "/api/v1/auth/webauthn/register/begin", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("register begin without service = %d, want 503", rec.Code)
	}
}

func TestWebAuthnRegisterFinishMissingChallenge(t *testing.T) {
	s, _, _, _ := newWebAuthnTestServer(t)
	cookie := loginAndGetCookie(t, s, "testuser", "hunter2hunter2")

	body := `{"response":{"id":"x","rawId":"x","type":"public-key","response":{"clientDataJSON":"{}","attestationObject":""}}}`
	req := httptest.NewRequest("POST", "/api/v1/auth/webauthn/register/finish", stringReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	req.Header.Set("Origin", "https://ops.example.ts.net")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("register finish without challenge = %d, want 400", rec.Code)
	}
}

func TestWebAuthnAssertBegin(t *testing.T) {
	s, _, db, _ := newWebAuthnTestServer(t)
	cookie := loginAndGetCookie(t, s, "testuser", "hunter2hunter2")
	userID := getUserIDFromDB(t, db, "testuser")

	// The user must have at least one credential for assert/begin to work.
	db.WebAuthnCredentials().Save(context.Background(), store.WebAuthnCredential{
		ID: "cred-1", UserID: userID, PublicKey: []byte{0x04}, Transports: `["internal"]`,
		CreatedAt: time.Now(),
	})

	req := httptest.NewRequest("POST", "/api/v1/auth/webauthn/assert/begin", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	req.Header.Set("Origin", "https://ops.example.ts.net")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assert begin = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp webauthnBeginAssertResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Options.Challenge == "" {
		t.Error("challenge should not be empty")
	}
	if resp.Options.RPID != "example-tailnet.ts.net" {
		t.Errorf("RPID = %q, want example-tailnet.ts.net", resp.Options.RPID)
	}
	// Challenge cookie should be set.
	var challengeCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_wa_challenge" {
			challengeCookie = c
		}
	}
	if challengeCookie == nil || challengeCookie.Value == "" {
		t.Error("challenge cookie should be set")
	}
}

func TestWebAuthnCredentialsList(t *testing.T) {
	s, _, db, _ := newWebAuthnTestServer(t)
	cookie := loginAndGetCookie(t, s, "testuser", "hunter2hunter2")
	userID := getUserIDFromDB(t, db, "testuser")

	// Save a credential directly in the store.
	db.WebAuthnCredentials().Save(context.Background(), store.WebAuthnCredential{
		ID: "cred-1", UserID: userID, PublicKey: []byte{0x04}, Transports: `["usb"]`,
		Label: "Test Key", CreatedAt: time.Now(),
	})

	req := httptest.NewRequest("GET", "/api/v1/auth/webauthn/credentials", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("credentials list = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp webauthnCredentialsResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials = %d, want 1", len(resp.Credentials))
	}
	if resp.Credentials[0].ID != "cred-1" {
		t.Errorf("ID = %q, want cred-1", resp.Credentials[0].ID)
	}
	if resp.Credentials[0].Label != "Test Key" {
		t.Errorf("Label = %q, want Test Key", resp.Credentials[0].Label)
	}
}

func TestWebAuthnCredentialsListEmpty(t *testing.T) {
	s, _, _, _ := newWebAuthnTestServer(t)
	cookie := loginAndGetCookie(t, s, "testuser", "hunter2hunter2")

	req := httptest.NewRequest("GET", "/api/v1/auth/webauthn/credentials", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("credentials list = %d", rec.Code)
	}
	var resp webauthnCredentialsResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Credentials == nil {
		t.Error("credentials should not be nil (empty array expected)")
	}
	if len(resp.Credentials) != 0 {
		t.Errorf("credentials = %d, want 0", len(resp.Credentials))
	}
}

func TestWebAuthnCredentialDelete(t *testing.T) {
	s, _, db, _ := newWebAuthnTestServer(t)
	cookie := loginAndGetCookie(t, s, "testuser", "hunter2hunter2")
	userID := getUserIDFromDB(t, db, "testuser")

	db.WebAuthnCredentials().Save(context.Background(), store.WebAuthnCredential{
		ID: "cred-1", UserID: userID, PublicKey: []byte{0x04}, CreatedAt: time.Now(),
	})

	req := httptest.NewRequest("DELETE", "/api/v1/auth/webauthn/credentials/cred-1", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("credential delete = %d", rec.Code)
	}
	// Verify it's gone from the store.
	if _, err := db.WebAuthnCredentials().Get(context.Background(), "cred-1"); err != store.ErrNotFound {
		t.Errorf("credential should be deleted, got err=%v", err)
	}
}

// TestWebAuthnCredentialDeleteOtherUserForbidden is the regression test for
// issue #39 (IDOR): a non-admin, non-owning session must not be able to
// delete another user's passkey, and the credential must survive the
// attempt.
func TestWebAuthnCredentialDeleteOtherUserForbidden(t *testing.T) {
	s, auth, db, _ := newWebAuthnTestServer(t)
	ownerID := getUserIDFromDB(t, db, "testuser")
	db.WebAuthnCredentials().Save(context.Background(), store.WebAuthnCredential{
		ID: "cred-1", UserID: ownerID, PublicKey: []byte{0x04}, CreatedAt: time.Now(),
	})

	if err := auth.CreateUser(t.Context(), "mallory", "hunter2hunter2", authz.RoleViewer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cookie := loginAndGetCookie(t, s, "mallory", "hunter2hunter2")

	req := httptest.NewRequest("DELETE", "/api/v1/auth/webauthn/credentials/cred-1", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-user delete = %d, want 404", rec.Code)
	}
	if _, err := db.WebAuthnCredentials().Get(context.Background(), "cred-1"); err != nil {
		t.Errorf("credential should still exist after a rejected cross-user delete, got err=%v", err)
	}
}

func TestWebAuthnCredentialDeleteNotFound(t *testing.T) {
	s, _, _, _ := newWebAuthnTestServer(t)
	cookie := loginAndGetCookie(t, s, "testuser", "hunter2hunter2")

	req := httptest.NewRequest("DELETE", "/api/v1/auth/webauthn/credentials/nonexistent", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFTokenFromSession(t, s, cookie))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete nonexistent = %d, want 404", rec.Code)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func getCSRFTokenFromSession(t *testing.T, s *Server, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var resp sessionInfoResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	return resp.CSRFToken
}

func getUserIDFromDB(t *testing.T, db *store.DB, username string) int64 {
	t.Helper()
	u, err := db.Users().ByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	return u.ID
}

func stringReader(s string) *stringReaderImpl {
	return &stringReaderImpl{s: s}
}

type stringReaderImpl struct {
	s string
	n int
}

func (r *stringReaderImpl) Read(p []byte) (int, error) {
	if r.n >= len(r.s) {
		return 0, errEOFReader
	}
	n := copy(p, r.s[r.n:])
	r.n += n
	return n, nil
}

var errEOFReader = newEOFError()

type eofError struct{}

func (e *eofError) Error() string { return "EOF" }

func newEOFError() error { return &eofError{} }
