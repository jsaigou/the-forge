// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

func TestWebAuthnCredentialsCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	uid, _ := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})

	// Save a credential.
	cred := WebAuthnCredential{
		ID:         "cred-id-1",
		UserID:     uid,
		PublicKey:  []byte{0x04, 0x01, 0x02, 0x03},
		SignCount:  0,
		Transports: `["usb","nfc"]`,
		Label:      "YubiKey",
		CreatedAt:  time.Now(),
	}
	if err := db.WebAuthnCredentials().Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Get.
	got, err := db.WebAuthnCredentials().Get(ctx, "cred-id-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != uid {
		t.Errorf("UserID = %d, want %d", got.UserID, uid)
	}
	if got.Label != "YubiKey" {
		t.Errorf("Label = %q, want YubiKey", got.Label)
	}
	if len(got.PublicKey) != 4 {
		t.Errorf("PublicKey len = %d, want 4", len(got.PublicKey))
	}

	// ListByUser.
	creds, err := db.WebAuthnCredentials().ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("ListByUser = %d, want 1", len(creds))
	}

	// UpdateSignCount.
	if err := db.WebAuthnCredentials().UpdateSignCount(ctx, "cred-id-1", 42, time.Now()); err != nil {
		t.Fatalf("UpdateSignCount: %v", err)
	}
	got, _ = db.WebAuthnCredentials().Get(ctx, "cred-id-1")
	if got.SignCount != 42 {
		t.Errorf("SignCount = %d, want 42", got.SignCount)
	}
	if got.LastUsedAt.IsZero() {
		t.Error("LastUsedAt should be set after UpdateSignCount")
	}

	// Save replaces (upsert).
	cred.Label = "Backup Key"
	cred.SignCount = 100
	if err := db.WebAuthnCredentials().Save(ctx, cred); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	got, _ = db.WebAuthnCredentials().Get(ctx, "cred-id-1")
	if got.Label != "Backup Key" {
		t.Errorf("Label = %q, want Backup Key", got.Label)
	}
	if got.SignCount != 100 {
		t.Errorf("SignCount = %d, want 100", got.SignCount)
	}

	// Delete.
	if err := db.WebAuthnCredentials().Delete(ctx, "cred-id-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := db.WebAuthnCredentials().Get(ctx, "cred-id-1"); err != ErrNotFound {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestWebAuthnCredentialsDeleteNotFound(t *testing.T) {
	db := openTestDB(t)
	err := db.WebAuthnCredentials().Delete(context.Background(), "nonexistent")
	if err != ErrNotFound {
		t.Errorf("Delete nonexistent = %v, want ErrNotFound", err)
	}
}

func TestWebAuthnCredentialsListByUserEmpty(t *testing.T) {
	db := openTestDB(t)
	uid, _ := db.Users().Create(context.Background(), User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})
	creds, err := db.WebAuthnCredentials().ListByUser(context.Background(), uid)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(creds) != 0 {
		t.Errorf("ListByUser = %d, want 0", len(creds))
	}
}

func TestEncodeDecodeTransports(t *testing.T) {
	// Round-trip.
	original := []string{"usb", "nfc", "ble"}
	raw, err := EncodeTransports(original)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got := DecodeTransports(raw)
	if len(got) != 3 {
		t.Fatalf("Decode = %d, want 3", len(got))
	}
	for i, s := range original {
		if got[i] != s {
			t.Errorf("Decode[%d] = %q, want %q", i, got[i], s)
		}
	}

	// Empty.
	raw, _ = EncodeTransports(nil)
	if raw != "" {
		t.Errorf("Encode(nil) = %q, want empty", raw)
	}
	if DecodeTransports("") != nil {
		t.Error("Decode('') should return nil")
	}
}
