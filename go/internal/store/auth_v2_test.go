// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

func TestSessionAssuranceColumns(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create a user for the session.
	uid, err := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Create a session with network assurance.
	now := time.Now()
	sess := Session{
		ID: "test-sess-1", UserID: uid, CSRFToken: "csrf1",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Assurance: "network", AssuranceAt: now, NetworkPrincipal: "user@github",
	}
	if err := db.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Read it back — assurance fields must persist.
	got, err := db.Sessions().Get(ctx, "test-sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Assurance != "network" {
		t.Errorf("assurance = %q, want network", got.Assurance)
	}
	if got.NetworkPrincipal != "user@github" {
		t.Errorf("network_principal = %q, want user@github", got.NetworkPrincipal)
	}
	if got.AssuranceAt.IsZero() {
		t.Error("assurance_at should be set")
	}
}

func TestSessionAssuranceDefaults(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	uid, _ := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})
	// Create with empty assurance — should default to "password" (column DEFAULT).
	sess := Session{
		ID: "test-sess-2", UserID: uid, CSRFToken: "csrf2",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.Sessions().Get(ctx, "test-sess-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Assurance != "password" {
		t.Errorf("assurance = %q, want password (default)", got.Assurance)
	}
}

func TestElevateSession(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	uid, _ := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})
	now := time.Now()
	sess := Session{
		ID: "test-sess-3", UserID: uid, CSRFToken: "csrf3",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), Assurance: "network",
	}
	if err := db.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Elevate: rotate ID + CSRF, set assurance.
	at := time.Now()
	err := db.Sessions().ElevateSession(ctx, "test-sess-3", "new-sess-3", "new-csrf3", "password", at)
	if err != nil {
		t.Fatalf("ElevateSession: %v", err)
	}

	// Old ID should not exist.
	if _, err := db.Sessions().Get(ctx, "test-sess-3"); err == nil {
		t.Error("old session ID should not exist after elevation")
	}

	// New ID should have the elevated assurance.
	got, err := db.Sessions().Get(ctx, "new-sess-3")
	if err != nil {
		t.Fatalf("get elevated session: %v", err)
	}
	if got.CSRFToken != "new-csrf3" {
		t.Errorf("csrf = %q, want new-csrf3", got.CSRFToken)
	}
	if got.Assurance != "password" {
		t.Errorf("assurance = %q, want password", got.Assurance)
	}
}

func TestElevateSessionNotFound(t *testing.T) {
	db := openTestDB(t)
	err := db.Sessions().ElevateSession(context.Background(), "nonexistent", "new", "csrf", "password", time.Now())
	if err != ErrNotFound {
		t.Errorf("ElevateSession on nonexistent = %v, want ErrNotFound", err)
	}
}

// ── Identity links ───────────────────────────────────────────────────────────

func TestIdentityLinksCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	uid, _ := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})

	// Create a link.
	link := IdentityLink{
		Provider: "tailscale", Principal: "user@github",
		UserID: uid, CreatedAt: time.Now(),
	}
	if err := db.IdentityLinks().Create(ctx, link); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Lookup.
	gotUID, err := db.IdentityLinks().Lookup(ctx, "tailscale", "user@github")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if gotUID != uid {
		t.Errorf("lookup uid = %d, want %d", gotUID, uid)
	}

	// ListByUser.
	links, err := db.IdentityLinks().ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("list by user: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("list by user = %d links, want 1", len(links))
	}

	// List (all).
	links, err = db.IdentityLinks().List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("list = %d links, want 1", len(links))
	}

	// Delete.
	if err := db.IdentityLinks().Delete(ctx, "tailscale", "user@github"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Lookup should fail.
	_, err = db.IdentityLinks().Lookup(ctx, "tailscale", "user@github")
	if err != ErrNotFound {
		t.Errorf("lookup after delete = %v, want ErrNotFound", err)
	}
}

func TestIdentityLinksLookupNotFound(t *testing.T) {
	db := openTestDB(t)
	_, err := db.IdentityLinks().Lookup(context.Background(), "tailscale", "nobody")
	if err != ErrNotFound {
		t.Errorf("lookup nonexistent = %v, want ErrNotFound", err)
	}
}

// ── TOTP ─────────────────────────────────────────────────────────────────────

func TestTOTPCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	uid, _ := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})

	// Save (unconfirmed).
	secret := store_TOTPSecret(uid, "JBSWY3DPEHPK3PXP", false)
	if err := db.TOTP().Save(ctx, secret); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Get.
	got, err := db.TOTP().Get(ctx, uid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Secret != "JBSWY3DPEHPK3PXP" {
		t.Errorf("secret = %q, want JBSWY3DPEHPK3PXP", got.Secret)
	}
	if got.Confirmed {
		t.Error("confirmed should be false initially")
	}

	// Confirm.
	if err := db.TOTP().Confirm(ctx, uid); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	got, _ = db.TOTP().Get(ctx, uid)
	if !got.Confirmed {
		t.Error("confirmed should be true after Confirm()")
	}

	// Save replaces (re-enroll).
	newSecret := store_TOTPSecret(uid, "NEWSECRET12345678", false)
	if err := db.TOTP().Save(ctx, newSecret); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, _ = db.TOTP().Get(ctx, uid)
	if got.Secret != "NEWSECRET12345678" {
		t.Errorf("secret = %q, want NEWSECRET12345678", got.Secret)
	}
	if got.Confirmed {
		t.Error("confirmed should be false after re-enroll")
	}

	// Delete.
	if err := db.TOTP().Delete(ctx, uid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.TOTP().Get(ctx, uid); err != ErrNotFound {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
}

func TestTOTPGetNotFound(t *testing.T) {
	db := openTestDB(t)
	_, err := db.TOTP().Get(context.Background(), 999)
	if err != ErrNotFound {
		t.Errorf("get nonexistent = %v, want ErrNotFound", err)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func store_TOTPSecret(uid int64, secret string, confirmed bool) TOTPSecret {
	return TOTPSecret{
		UserID:    uid,
		Secret:    secret,
		Confirmed: confirmed,
		CreatedAt: time.Now(),
	}
}
