// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeHasher stands in for Argon2id in flow tests (the real KDF round-trip
// is covered by hasher_xcrypto_test.go). It counts verifies so the
// one-Argon2-verify-per-request contract point is testable.
type fakeHasher struct{ verifies atomic.Int64 }

func (f *fakeHasher) Hash(secret string) (string, error) { return "fake$" + secret, nil }

func (f *fakeHasher) Verify(encoded, secret string) (bool, error) {
	f.verifies.Add(1)
	if !strings.HasPrefix(encoded, "fake$") {
		return false, errBadHash
	}
	return encoded == "fake$"+secret, nil
}

type fixture struct {
	a      *Authorizer
	st     store.Store
	hasher *fakeHasher
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{st: db, hasher: &fakeHasher{}, now: time.Unix(1_700_000_000, 0)}
	f.a = New(db,
		WithHasher(f.hasher),
		WithClock(func() time.Time { return f.now }),
	)
	return f
}

func (f *fixture) addUser(t *testing.T, username, password, role string) {
	t.Helper()
	if err := f.a.CreateUser(context.Background(), username, password, Role(role)); err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
}

func TestLoginAndSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.addUser(t, "testuser", "hunter2hunter2", "admin")

	sess, id, err := f.a.Login(ctx, "testuser", "hunter2hunter2", "100.100.100.100", "test-ua")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if id.Name != "testuser" || id.Role != RoleAdmin || id.Kind != "" || id.KeyID != "" {
		t.Errorf("identity = %+v", id)
	}
	if len(sess.ID) < 40 || len(sess.CSRFToken) < 40 {
		t.Errorf("session id/csrf too short: %d/%d", len(sess.ID), len(sess.CSRFToken))
	}
	if !sess.ExpiresAt.Equal(f.now.Add(defaultSessionTTL)) {
		t.Errorf("expiry = %v", sess.ExpiresAt)
	}

	// Interface method resolves the cookie to the same identity.
	got, err := f.a.VerifySession(sess.ID)
	if err != nil || got.Name != "testuser" || !got.Role.Allows(RoleAdmin) {
		t.Fatalf("VerifySession: %+v err=%v", got, err)
	}

	// SessionInfo bootstraps the PWA: identity + CSRF token.
	_, csrf, err := f.a.SessionInfo(ctx, sess.ID)
	if err != nil || csrf != sess.CSRFToken {
		t.Fatalf("SessionInfo: csrf=%q err=%v", csrf, err)
	}

	// CSRF validation: exact token passes, anything else fails.
	if !f.a.ValidateCSRF(ctx, sess.ID, sess.CSRFToken) {
		t.Error("valid CSRF token rejected")
	}
	if f.a.ValidateCSRF(ctx, sess.ID, "wrong") || f.a.ValidateCSRF(ctx, sess.ID, "") {
		t.Error("invalid CSRF token accepted")
	}

	// Expiry: advancing past TTL invalidates and deletes the session.
	f.now = f.now.Add(defaultSessionTTL + time.Second)
	if _, err := f.a.VerifySession(sess.ID); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired session: got %v", err)
	}
	if _, err := f.st.Sessions().Get(ctx, sess.ID); !errors.Is(err, store.ErrNotFound) {
		t.Error("expired session should be deleted on verify")
	}
}

func TestLoginFailures(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.addUser(t, "testuser", "hunter2hunter2", "admin")

	if _, _, err := f.a.Login(ctx, "testuser", "wrong-password", "ip1", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("wrong password: %v", err)
	}
	if _, _, err := f.a.Login(ctx, "nobody", "hunter2hunter2", "ip1", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("unknown user: %v", err)
	}
	if _, _, err := f.a.Login(ctx, "testuser", "", "ip1", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("empty password: %v", err)
	}
	// Over-long password is rejected before the KDF (DoS guard).
	if _, _, err := f.a.Login(ctx, "testuser", strings.Repeat("x", 2000), "ip1", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("oversized password: %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.addUser(t, "testuser", "hunter2hunter2", "admin")

	for i := 0; i < 10; i++ {
		if _, _, err := f.a.Login(ctx, "testuser", "wrong", "attacker-ip", ""); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// 11th attempt — even with the RIGHT password — is rate limited.
	if _, _, err := f.a.Login(ctx, "testuser", "hunter2hunter2", "attacker-ip", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("11th attempt: got %v, want ErrRateLimited", err)
	}
	// A different IP is unaffected.
	if _, _, err := f.a.Login(ctx, "testuser", "hunter2hunter2", "other-ip", ""); err != nil {
		t.Fatalf("other IP: %v", err)
	}
	// Window expiry unblocks.
	f.now = f.now.Add(61 * time.Second)
	if _, _, err := f.a.Login(ctx, "testuser", "hunter2hunter2", "attacker-ip", ""); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

func TestDisabledUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.addUser(t, "testuser", "hunter2hunter2", "admin")
	f.addUser(t, "bob", "hunter2hunter2", "viewer")

	sess, _, err := f.a.Login(ctx, "bob", "hunter2hunter2", "ip", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Disable bob directly in the store (no Update in the frozen interface;
	// recreate with disabled set).
	u, _ := f.st.Users().ByUsername(ctx, "bob")
	if err := f.st.Users().Delete(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Users().Create(ctx, store.User{
		Username: "bob", PasswordHash: u.PasswordHash, Role: u.Role, Disabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := f.a.Login(ctx, "bob", "hunter2hunter2", "ip", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("disabled login: %v", err)
	}
	// The old session died with the FK cascade; a surviving session for a
	// disabled user would also be rejected by the userByID disabled check.
	if _, err := f.a.VerifySession(sess.ID); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("disabled session: %v", err)
	}
}

func TestBearerKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	token, err := f.a.MintKey(ctx, KindForge, "opencode", RoleOperator)
	if err != nil {
		t.Fatalf("MintKey: %v", err)
	}
	kind, keyid, _, err := ParseToken(token)
	if err != nil || kind != KindForge {
		t.Fatalf("minted token fails frozen grammar: %v (kind=%s)", err, kind)
	}

	f.hasher.verifies.Store(0)
	id, err := f.a.VerifyBearer(token, KindForge)
	if err != nil {
		t.Fatalf("VerifyBearer: %v", err)
	}
	if id.Name != "opencode" || id.Role != RoleOperator || id.KeyID != keyid || id.Kind != KindForge {
		t.Errorf("identity = %+v", id)
	}
	// Exactly one hash verify per request (keyid routes to one row).
	if n := f.hasher.verifies.Load(); n != 1 {
		t.Errorf("verifies = %d, want exactly 1", n)
	}

	// Kind enforcement: a forge token must not pass as router.
	if _, err := f.a.VerifyBearer(token, KindRouter); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("cross-kind: %v", err)
	}
	// Wrong secret, valid keyid.
	forged := FormatToken(KindForge, keyid, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := f.a.VerifyBearer(forged, KindForge); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("forged secret: %v", err)
	}
	// Garbage token.
	if _, err := f.a.VerifyBearer("not-a-token", KindForge); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("garbage: %v", err)
	}

	// Router/MCP keys carry no role.
	mcpToken, err := f.a.MintKey(ctx, KindMCP, "agent-x", "")
	if err != nil {
		t.Fatalf("MintKey mcp: %v", err)
	}
	mcpID, err := f.a.VerifyBearer(mcpToken, KindMCP)
	if err != nil || mcpID.Role != "" || mcpID.Name != "agent-x" {
		t.Fatalf("mcp identity = %+v err=%v", mcpID, err)
	}
	if mcpID.Role.Allows(RoleViewer) {
		t.Error("role-less identity must not pass any RBAC check")
	}
	if _, err := f.a.MintKey(ctx, KindMCP, "agent-x", RoleAdmin); err == nil {
		t.Error("mcp key with role must be rejected")
	}
	if _, err := f.a.MintKey(ctx, KindForge, "x", "superuser"); err == nil {
		t.Error("invalid role must be rejected")
	}

	// Re-minting the same name revokes the old key.
	token2, err := f.a.MintKey(ctx, KindForge, "opencode", RoleViewer)
	if err != nil {
		t.Fatalf("re-mint: %v", err)
	}
	if _, err := f.a.VerifyBearer(token, KindForge); !errors.Is(err, ErrUnauthenticated) {
		t.Error("old key must be revoked after re-mint")
	}
	if _, err := f.a.VerifyBearer(token2, KindForge); err != nil {
		t.Errorf("new key must verify: %v", err)
	}

	// Explicit revoke.
	_, keyid2, _, _ := ParseToken(token2)
	if err := f.a.RevokeKey(ctx, keyid2); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := f.a.VerifyBearer(token2, KindForge); !errors.Is(err, ErrUnauthenticated) {
		t.Error("revoked key must not verify")
	}
}

func TestBearerRateLimit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	token, err := f.a.MintKey(ctx, KindRouter, "libre", "")
	if err != nil {
		t.Fatal(err)
	}

	bad := FormatToken(KindRouter, "000000000000", "aaaaaaaaaaaaaaaaaaaaaaaa")
	for i := 0; i < 10; i++ {
		if _, err := f.a.VerifyBearerFrom(ctx, "ip9", bad, KindRouter); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := f.a.VerifyBearerFrom(ctx, "ip9", token, KindRouter); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limited: got %v", err)
	}
	if _, err := f.a.VerifyBearerFrom(ctx, "ip-clean", token, KindRouter); err != nil {
		t.Fatalf("clean IP: %v", err)
	}
}

func TestTouchThrottling(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	token, err := f.a.MintKey(ctx, KindForge, "k", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	_, keyid, _, _ := ParseToken(token)

	// First verify touches last_used_at.
	if _, err := f.a.VerifyBearer(token, KindForge); err != nil {
		t.Fatal(err)
	}
	k, _ := f.st.Keys().Get(ctx, keyid)
	first := k.LastUsedAt
	if first.IsZero() {
		t.Fatal("first verify should touch last_used_at")
	}

	// Verifies within the throttle window do not write.
	f.now = f.now.Add(10 * time.Second)
	if _, err := f.a.VerifyBearer(token, KindForge); err != nil {
		t.Fatal(err)
	}
	k, _ = f.st.Keys().Get(ctx, keyid)
	if !k.LastUsedAt.Equal(first) {
		t.Error("touch inside the throttle window should be skipped")
	}

	// After the window, the touch lands.
	f.now = f.now.Add(defaultTouchEvery)
	if _, err := f.a.VerifyBearer(token, KindForge); err != nil {
		t.Fatal(err)
	}
	k, _ = f.st.Keys().Get(ctx, keyid)
	if k.LastUsedAt.Equal(first) {
		t.Error("touch after the throttle window should be recorded")
	}
}

func TestFirstRunWizard(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	required, err := f.a.SetupRequired(ctx)
	if err != nil || !required {
		t.Fatalf("fresh DB: required=%v err=%v", required, err)
	}

	if err := f.a.CompleteSetup(ctx, "testuser", "short"); err == nil {
		t.Error("short password must be rejected")
	}
	if err := f.a.CompleteSetup(ctx, "testuser", "a-real-password"); err != nil {
		t.Fatalf("CompleteSetup: %v", err)
	}

	required, _ = f.a.SetupRequired(ctx)
	if required {
		t.Error("setup must not be required after completion")
	}
	if err := f.a.CompleteSetup(ctx, "eve", "another-password"); err == nil {
		t.Error("second setup must be refused")
	}

	v, err := f.st.Settings().Get(ctx, "wizard.completed")
	if err != nil || string(v) != "true" {
		t.Errorf("wizard.completed = %q err=%v", v, err)
	}

	// The wizard-created account is a working admin.
	_, id, err := f.a.Login(ctx, "testuser", "a-real-password", "ip", "")
	if err != nil || id.Role != RoleAdmin {
		t.Fatalf("wizard admin login: %+v err=%v", id, err)
	}
}

// TestNoSecretsInErrors: every error path that involves a secret must not
// echo it back.
func TestNoSecretsInErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.addUser(t, "testuser", "sup3r-secret-pw", "admin")

	_, _, err := f.a.Login(ctx, "testuser", "sup3r-secret-pw-typo", "ip", "")
	if err != nil && strings.Contains(err.Error(), "sup3r-secret") {
		t.Error("login error leaks the password")
	}

	token, _ := f.a.MintKey(ctx, KindForge, "k", RoleViewer)
	_, _, secret, _ := ParseToken(token)
	_, err = f.a.VerifyBearer(token, KindRouter)
	if err != nil && (strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), token)) {
		t.Error("bearer error leaks the token")
	}
}
