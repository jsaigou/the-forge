// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

func TestRecoveryCodesSaveAndList(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	uid, _ := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})

	codes := []RecoveryCode{
		{UserID: uid, CodeHash: "hash1", CreatedAt: time.Now()},
		{UserID: uid, CodeHash: "hash2", CreatedAt: time.Now()},
		{UserID: uid, CodeHash: "hash3", CreatedAt: time.Now()},
	}
	if err := db.RecoveryCodes().Save(ctx, codes); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := db.RecoveryCodes().ListByUser(ctx, uid)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListByUser = %d, want 3", len(got))
	}
	for _, c := range got {
		if !c.UsedAt.IsZero() {
			t.Error("new codes should have zero UsedAt")
		}
	}
}

func TestRecoveryCodesMarkUsed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	uid, _ := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})
	db.RecoveryCodes().Save(ctx, []RecoveryCode{
		{UserID: uid, CodeHash: "hash1", CreatedAt: time.Now()},
	})

	codes, _ := db.RecoveryCodes().ListByUser(ctx, uid)
	if len(codes) != 1 {
		t.Fatalf("expected 1 code, got %d", len(codes))
	}

	// Mark it used.
	if err := db.RecoveryCodes().MarkUsed(ctx, codes[0].ID, time.Now()); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	// Verify it's used.
	codes, _ = db.RecoveryCodes().ListByUser(ctx, uid)
	if codes[0].UsedAt.IsZero() {
		t.Error("UsedAt should be set after MarkUsed")
	}

	// Marking again should fail (already used).
	if err := db.RecoveryCodes().MarkUsed(ctx, codes[0].ID, time.Now()); err != ErrNotFound {
		t.Errorf("MarkUsed on already-used code = %v, want ErrNotFound", err)
	}
}

func TestRecoveryCodesDeleteAll(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	uid, _ := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})
	db.RecoveryCodes().Save(ctx, []RecoveryCode{
		{UserID: uid, CodeHash: "h1", CreatedAt: time.Now()},
		{UserID: uid, CodeHash: "h2", CreatedAt: time.Now()},
	})

	if err := db.RecoveryCodes().DeleteAll(ctx, uid); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	codes, _ := db.RecoveryCodes().ListByUser(ctx, uid)
	if len(codes) != 0 {
		t.Errorf("after DeleteAll = %d, want 0", len(codes))
	}
}

func TestRecoveryCodesEmptyList(t *testing.T) {
	db := openTestDB(t)
	uid, _ := db.Users().Create(context.Background(), User{
		Username: "testuser", PasswordHash: "hash", Role: "admin", CreatedAt: time.Now(),
	})
	codes, err := db.RecoveryCodes().ListByUser(context.Background(), uid)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(codes) != 0 {
		t.Errorf("ListByUser = %d, want 0", len(codes))
	}
}
