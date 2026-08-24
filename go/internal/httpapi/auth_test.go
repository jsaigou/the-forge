// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/store"
)

// newAuthTestServer builds a Server with a real authz.Authorizer backed by
// an in-memory store, plus all the Sprint 0-AUTH deps wired (policy store,
// identity links, TOTP, keys, step-up verifier, key manager).
func newAuthTestServer(t *testing.T) (*Server, *authz.Authorizer, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	auth := authz.New(db)
	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
	})

	policyStore := authz.NewPolicyStore(authz.SettingsAdapter{
		GetFn: func(ctx context.Context, key string) ([]byte, error) {
			return db.Settings().Get(ctx, key)
		},
		SetFn: func(ctx context.Context, key string, value []byte) error {
			return db.Settings().Set(ctx, key, value)
		},
	})

	s := New(Deps{
		Snapshots:        collector.NewStatic(nil),
		Auth:             auth,
		AuthSetup:        auth,
		Events:           events,
		Publish:          events,
		Engine:           &engine.Stub{},
		Config:           func() *config.Config { return cfg },
		Hostname:         "test-host",
		Sessions:         db.Sessions(),
		Settings:         db.Settings(),
		Audit:            db.Audit(),
		IdentityLinks:    db.IdentityLinks(),
		TOTPStore:        db.TOTP(),
		Keys:             db.Keys(),
		PolicyStore:      policyStore,
		StepUpTTL:        authz.DefaultStepUpTTL,
		NetworkDefaultRole: authz.RoleViewer,
		StepUpVerifier:   auth,
		KeyManager:       auth,
		NetworkIdentity:  authz.NoNetworkIdentity{},
	})
	t.Cleanup(func() { s.Close() })
	return s, auth, db
}

// loginAndCookie creates a user, logs in, and returns the session cookie.
func loginAndCookie(t *testing.T, s *Server, auth *authz.Authorizer, username, password string) *http.Cookie {
	t.Helper()
	if err := auth.CreateUser(t.Context(), username, password, authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rec := doForm(t, s.Handler(), "POST", "/login", url.Values{
		"username": {username},
		"password": {password},
	})
	cookies := rec.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "forge_session" {
			return c
		}
	}
	t.Fatal("no session cookie after login")
	return nil
}

// ── Step-up ──────────────────────────────────────────────────────────────────

func TestStepUpPassword(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")

	// The session should start at "password" assurance (created by login).
	// Step up with password → elevates (rotates session ID + sets assurance).
	body := bytes.NewBufferString(`{"factor":"password","password":"hunter2hunter2"}`)
	req := httptest.NewRequest("POST", "/api/v1/auth/step-up", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	// Get CSRF from the session.
	csrf := getCSRFToken(t, s, cookie)
	req.Header.Set("X-CSRF-Token", csrf)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("step-up = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp stepUpResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Assurance != "password" {
		t.Errorf("assurance = %q, want password", resp.Assurance)
	}
	if resp.AssuranceAt == nil {
		t.Error("assurance_at should be set")
	}
	// Session should have been rotated — new cookie issued.
	var newCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			newCookie = c
		}
	}
	if newCookie == nil {
		t.Fatal("no rotated session cookie after step-up")
	}
	if newCookie.Value == cookie.Value {
		t.Error("session ID should have been rotated")
	}
}

func TestStepUpWrongPassword(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")

	body := bytes.NewBufferString(`{"factor":"password","password":"wrong-password"}`)
	req := httptest.NewRequest("POST", "/api/v1/auth/step-up", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFToken(t, s, cookie))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("step-up wrong password = %d, want 401", rec.Code)
	}
}

func TestStepUpTOTP(t *testing.T) {
	s, auth, db := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")

	// Enroll TOTP.
	userID := getUserID(t, db, "testuser")
	secret, _ := authz.GenerateTOTPSecret()
	db.TOTP().Save(context.Background(), store.TOTPSecret{
		UserID: userID, Secret: secret, Confirmed: true, CreatedAt: time.Now(),
	})

	// Generate a valid TOTP code.
	now := time.Now()
	code := authz.ComputeTOTPCode(secret, now)

	body := bytes.NewBufferString(`{"factor":"totp","code":"` + code + `"}`)
	req := httptest.NewRequest("POST", "/api/v1/auth/step-up", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFToken(t, s, cookie))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("step-up totp = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp stepUpResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Assurance != "totp" {
		t.Errorf("assurance = %q, want totp", resp.Assurance)
	}
}

// ── TOTP enroll/confirm/delete ───────────────────────────────────────────────

func TestTOTPEnrollAndConfirm(t *testing.T) {
	s, auth, db := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	csrf := getCSRFToken(t, s, cookie)

	// Enroll.
	req := authedReqWithCookie("POST", "/api/v1/auth/totp/enroll", nil, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d, body=%s", rec.Code, rec.Body.String())
	}
	var enrollResp totpEnrollResponse
	json.NewDecoder(rec.Body).Decode(&enrollResp)
	if enrollResp.Secret == "" {
		t.Fatal("secret is empty")
	}
	if !strings.Contains(enrollResp.OTPAuthURI, "otpauth://totp/") {
		t.Errorf("otpauth_uri = %q", enrollResp.OTPAuthURI)
	}

	// Confirm with a valid code.
	code := authz.ComputeTOTPCode(enrollResp.Secret, time.Now())
	body := bytes.NewBufferString(`{"code":"` + code + `"}`)
	req = authedReqWithCookie("POST", "/api/v1/auth/totp/confirm", body, cookie, csrf)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, body=%s", rec.Code, rec.Body.String())
	}
	var confirmResp totpConfirmResponse
	json.NewDecoder(rec.Body).Decode(&confirmResp)
	if !confirmResp.Active {
		t.Error("active should be true after confirm")
	}

	// Verify it's confirmed in the store.
	userID := getUserID(t, db, "testuser")
	totp, _ := db.TOTP().Get(context.Background(), userID)
	if !totp.Confirmed {
		t.Error("TOTP should be confirmed in store")
	}
}

func TestTOTPDelete(t *testing.T) {
	s, auth, db := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	csrf := getCSRFToken(t, s, cookie)
	userID := getUserID(t, db, "testuser")

	db.TOTP().Save(context.Background(), store.TOTPSecret{
		UserID: userID, Secret: "JBSWY3DPEHPK3PXP", Confirmed: true, CreatedAt: time.Now(),
	})

	req := authedReqWithCookie("DELETE", "/api/v1/auth/totp", nil, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	if _, err := db.TOTP().Get(context.Background(), userID); err != store.ErrNotFound {
		t.Errorf("TOTP should be deleted, got err=%v", err)
	}
}

// ── API keys ─────────────────────────────────────────────────────────────────

func TestKeysListAndMint(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	// First, step up to get password assurance (keys are gated by
	// area.settings.security which requires password).
	cookie = performStepUp(t, s, cookie, "password", "hunter2hunter2", "")
	csrf := getCSRFToken(t, s, cookie)

	// Mint a key.
	body := bytes.NewBufferString(`{"kind":"forge","name":"test-key","role":"viewer"}`)
	req := authedReqWithCookie("POST", "/api/v1/keys", body, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d, body=%s", rec.Code, rec.Body.String())
	}
	var createResp apiKeyCreateResponse
	json.NewDecoder(rec.Body).Decode(&createResp)
	if createResp.Token == "" {
		t.Error("token should be returned once")
	}
	if createResp.Key.KeyID == "" {
		t.Error("keyid should be set")
	}
	if createResp.Key.Name != "test-key" {
		t.Errorf("name = %q", createResp.Key.Name)
	}

	// List keys.
	req = authedReqWithCookie("GET", "/api/v1/keys?kind=forge", nil, cookie, csrf)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var listResp apiKeysResponse
	json.NewDecoder(rec.Body).Decode(&listResp)
	if len(listResp.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(listResp.Keys))
	}
	// The token must NEVER appear in the list response.
	if strings.Contains(rec.Body.String(), createResp.Token) {
		t.Error("token leaked in list response")
	}
}

func TestKeyRevoke(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	cookie = performStepUp(t, s, cookie, "password", "hunter2hunter2", "")
	csrf := getCSRFToken(t, s, cookie)

	// Mint a key.
	body := bytes.NewBufferString(`{"kind":"router","name":"test-router"}`)
	req := authedReqWithCookie("POST", "/api/v1/keys", body, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d", rec.Code)
	}
	var createResp apiKeyCreateResponse
	json.NewDecoder(rec.Body).Decode(&createResp)

	// Revoke it.
	req = authedReqWithCookie("DELETE", "/api/v1/keys/"+createResp.Key.KeyID, nil, cookie, csrf)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}

	// List — the revoked key should have revoked_at set.
	req = authedReqWithCookie("GET", "/api/v1/keys?kind=router", nil, cookie, csrf)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var listResp apiKeysResponse
	json.NewDecoder(rec.Body).Decode(&listResp)
	if len(listResp.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(listResp.Keys))
	}
	if listResp.Keys[0].RevokedAt == nil {
		t.Error("revoked_at should be set")
	}
}

// ── Policy ───────────────────────────────────────────────────────────────────

func TestPolicyGetSeedsDefault(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	cookie = performStepUp(t, s, cookie, "password", "hunter2hunter2", "")
	csrf := getCSRFToken(t, s, cookie)

	req := authedReqWithCookie("GET", "/api/v1/auth/policy", nil, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy get = %d", rec.Code)
	}
	var resp authPolicyResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Policy) == 0 {
		t.Fatal("policy should have default seed entries")
	}
	if resp.Policy["page.settings"] != "password" {
		t.Errorf("page.settings = %q, want password", resp.Policy["page.settings"])
	}
}

func TestPolicyPut(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	cookie = performStepUp(t, s, cookie, "password", "hunter2hunter2", "")
	csrf := getCSRFToken(t, s, cookie)

	// Change page.dashboard to require password.
	body := bytes.NewBufferString(`{"policy":{"page.dashboard":"password","page.console":"network","page.scheduling":"network","page.compression":"network","page.settings":"password","area.settings.security":"password","area.settings.provider_keys":"password","action.model.load_unload":"network","action.compressor.teardown":"password","action.reservation.write":"network"}}`)
	req := authedReqWithCookie("PUT", "/api/v1/auth/policy", body, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy put = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp authPolicyResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Policy["page.dashboard"] != "password" {
		t.Errorf("page.dashboard = %q, want password", resp.Policy["page.dashboard"])
	}
}

func TestPolicyPutRejectsInvalid(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	cookie = performStepUp(t, s, cookie, "password", "hunter2hunter2", "")
	csrf := getCSRFToken(t, s, cookie)

	body := bytes.NewBufferString(`{"policy":{"bogus.resource":"network"}}`)
	req := authedReqWithCookie("PUT", "/api/v1/auth/policy", body, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 422 {
		t.Errorf("policy put invalid = %d, want 422", rec.Code)
	}
}

// ── requireAssurance gating ──────────────────────────────────────────────────

func TestRequireAssuranceBlocksNetworkUser(t *testing.T) {
	s, _, db := newAuthTestServer(t)
	// Create a user and link their tailnet identity.
	uid, _ := db.Users().Create(context.Background(), store.User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})
	db.IdentityLinks().Create(context.Background(), store.IdentityLink{
		Provider: "tailscale", Principal: "user@github", UserID: uid, CreatedAt: time.Now(),
	})

	// Wire a fake tailscale identity provider.
	s.deps.NetworkIdentity = &authz.TailscaleIdentityProvider{
		Client: &fakeWhoIsClientHTTP{login: "user@github"},
	}

	// Request from tailnet IP — should bootstrap an L0 session.
	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.RemoteAddr = "100.100.100.100:54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session = %d, body=%s", rec.Code, rec.Body.String())
	}
	var sessResp sessionInfoResponse
	json.NewDecoder(rec.Body).Decode(&sessResp)
	if sessResp.Assurance != "network" {
		t.Errorf("assurance = %q, want network", sessResp.Assurance)
	}
	if sessResp.NetworkPrincipal == nil || *sessResp.NetworkPrincipal != "user@github" {
		t.Errorf("network_principal = %v, want user@github", sessResp.NetworkPrincipal)
	}

	// Extract the session cookie.
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie after network bootstrap")
	}

	// Try to access billing settings — should be gated by page.settings
	// (requires password assurance). L0 session should get 403 step_up_required.
	req = httptest.NewRequest("GET", "/api/v1/billing/settings", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing settings = %d, want 403 (step_up_required)", rec.Code)
	}
	var errResp map[string]string
	json.NewDecoder(rec.Body).Decode(&errResp)
	if errResp["error"] != "step_up_required" {
		t.Errorf("error = %q, want step_up_required", errResp["error"])
	}
	if errResp["required"] != "password" {
		t.Errorf("required = %q, want password", errResp["required"])
	}
}

func TestRequireAssurancePassesAfterStepUp(t *testing.T) {
	s, auth, db := newAuthTestServer(t)
	// Create a user with a known password (for step-up verification).
	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Link the user's tailnet identity.
	uid := getUserID(t, db, "testuser")
	db.IdentityLinks().Create(context.Background(), store.IdentityLink{
		Provider: "tailscale", Principal: "user@github", UserID: uid, CreatedAt: time.Now(),
	})
	s.deps.NetworkIdentity = &authz.TailscaleIdentityProvider{
		Client: &fakeWhoIsClientHTTP{login: "user@github"},
	}

	// Bootstrap L0 session from tailnet.
	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.RemoteAddr = "100.100.100.100:54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var netCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			netCookie = c
		}
	}
	if netCookie == nil {
		t.Fatal("no cookie after network bootstrap")
	}

	// Step up with password.
	body := bytes.NewBufferString(`{"factor":"password","password":"hunter2hunter2"}`)
	req = httptest.NewRequest("POST", "/api/v1/auth/step-up", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(netCookie)
	req.Header.Set("X-CSRF-Token", getCSRFToken(t, s, netCookie))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("step-up = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Get the rotated cookie.
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			netCookie = c
		}
	}

	// Now billing/settings should pass (password assurance).
	req = httptest.NewRequest("GET", "/api/v1/billing/settings", nil)
	req.AddCookie(netCookie)
	req.Header.Set("X-CSRF-Token", getCSRFToken(t, s, netCookie))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		var errResp map[string]string
		json.NewDecoder(rec.Body).Decode(&errResp)
		if errResp["error"] == "step_up_required" {
			t.Fatalf("billing/settings gated after step-up — should pass")
		}
	}
}

// ── Identity links ───────────────────────────────────────────────────────────

func TestIdentityLinksList(t *testing.T) {
	s, auth, db := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	cookie = performStepUp(t, s, cookie, "password", "hunter2hunter2", "")
	csrf := getCSRFToken(t, s, cookie)
	userID := getUserID(t, db, "testuser")

	db.IdentityLinks().Create(context.Background(), store.IdentityLink{
		Provider: "tailscale", Principal: "user@github", UserID: userID, CreatedAt: time.Now(),
	})

	req := authedReqWithCookie("GET", "/api/v1/auth/identity-links", nil, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var resp identityLinksResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Links) != 1 {
		t.Fatalf("links = %d, want 1", len(resp.Links))
	}
	if resp.Links[0].Principal != "user@github" {
		t.Errorf("principal = %q", resp.Links[0].Principal)
	}
}

// ── Auth config ──────────────────────────────────────────────────────────────

func TestAuthConfigGet(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	cookie = performStepUp(t, s, cookie, "password", "hunter2hunter2", "")
	csrf := getCSRFToken(t, s, cookie)

	req := authedReqWithCookie("GET", "/api/v1/auth/config", nil, cookie, csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("config get = %d", rec.Code)
	}
	var resp authConfigResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.StepUpTTLMin != 15 {
		t.Errorf("step_up_ttl_min = %d, want 15", resp.StepUpTTLMin)
	}
	if resp.NetworkDefaultRole != "viewer" {
		t.Errorf("network_default_role = %q, want viewer", resp.NetworkDefaultRole)
	}
	if !resp.A0TailnetBypass {
		t.Error("a0_tailnet_bypass should be true by default")
	}
}

// ── Session info ─────────────────────────────────────────────────────────────

func TestSessionInfoIncludesAssuranceAndPolicy(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")

	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session = %d", rec.Code)
	}
	var resp sessionInfoResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Assurance == "" {
		t.Error("assurance should not be empty")
	}
	if len(resp.Policy) == 0 {
		t.Error("policy map should have entries (default seed)")
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func getCSRFToken(t *testing.T, s *Server, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var resp sessionInfoResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	return resp.CSRFToken
}

func getUserID(t *testing.T, db *store.DB, username string) int64 {
	t.Helper()
	u, err := db.Users().ByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	return u.ID
}

func authedReqWithCookie(method, path string, body *bytes.Buffer, cookie *http.Cookie, csrf string) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, body)
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func performStepUp(t *testing.T, s *Server, cookie *http.Cookie, factor, password, code string) *http.Cookie {
	t.Helper()
	body := bytes.NewBufferString(`{"factor":"` + factor + `","password":"` + password + `","code":"` + code + `"}`)
	req := httptest.NewRequest("POST", "/api/v1/auth/step-up", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFToken(t, s, cookie))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("step-up failed: %d, body=%s", rec.Code, rec.Body.String())
	}
	// Return the rotated session cookie.
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			return c
		}
	}
	t.Fatal("no rotated session cookie after step-up")
	return cookie
}

type fakeWhoIsClientHTTP struct {
	login string
}

func (f *fakeWhoIsClientHTTP) WhoIs(_ context.Context, _ string) (string, bool) {
	return f.login, true
}
