// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"testing"
	"time"
)

// fakeRecoveryStore is an in-memory RecoveryCodeStore for tests.
type fakeRecoveryStore struct {
	codes []RecoveryCodeRecord
	nextID int64
}

func newFakeRecoveryStore() *fakeRecoveryStore {
	return &fakeRecoveryStore{nextID: 1}
}

func (f *fakeRecoveryStore) Save(_ context.Context, codes []RecoveryCodeRecord) error {
	for i := range codes {
		codes[i].ID = f.nextID
		f.nextID++
		f.codes = append(f.codes, codes[i])
	}
	return nil
}

func (f *fakeRecoveryStore) ListByUser(_ context.Context, userID int64) ([]RecoveryCodeRecord, error) {
	var out []RecoveryCodeRecord
	for _, c := range f.codes {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeRecoveryStore) MarkUsed(_ context.Context, id int64, at time.Time) error {
	for i, c := range f.codes {
		if c.ID == id && c.UsedAt.IsZero() {
			f.codes[i].UsedAt = at
			return nil
		}
	}
	return errNotFoundFake
}

func (f *fakeRecoveryStore) DeleteAll(_ context.Context, userID int64) error {
	var out []RecoveryCodeRecord
	for _, c := range f.codes {
		if c.UserID != userID {
			out = append(out, c)
		}
	}
	f.codes = out
	return nil
}

func TestRecoveryGenerateCodes(t *testing.T) {
	store := newFakeRecoveryStore()
	svc := NewRecoveryService(store, nil) // nil pepper → unkeyed SHA-256 (test)

	codes, err := svc.GenerateCodes(context.Background(), 1)
	if err != nil {
		t.Fatalf("GenerateCodes: %v", err)
	}
	if len(codes) != recoveryCodeCount {
		t.Fatalf("GenerateCodes = %d codes, want %d", len(codes), recoveryCodeCount)
	}
	// Each code should be formatted with dashes (XXXX-XXXX-XXXX-XXXX pattern).
	for i, c := range codes {
		if len(c) < 10 {
			t.Errorf("code[%d] = %q (len %d), too short", i, c, len(c))
		}
		// Should contain at least 2 dashes (3 groups).
		dashCount := 0
		for _, ch := range c {
			if ch == '-' {
				dashCount++
			}
		}
		if dashCount < 2 {
			t.Errorf("code[%d] = %q, expected at least 2 dashes", i, c)
		}
	}
	// Codes should be unique.
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate code: %s", c)
		}
		seen[c] = true
	}
	// Store should have 10 hashed codes.
	stored, _ := store.ListByUser(context.Background(), 1)
	if len(stored) != recoveryCodeCount {
		t.Errorf("stored = %d, want %d", len(stored), recoveryCodeCount)
	}
}

func TestRecoveryGenerateReplaces(t *testing.T) {
	store := newFakeRecoveryStore()
	svc := NewRecoveryService(store, nil)

	// Generate initial set.
	codes1, _ := svc.GenerateCodes(context.Background(), 1)
	if len(codes1) != recoveryCodeCount {
		t.Fatalf("first generate = %d", len(codes1))
	}

	// Generate again — should replace, not append.
	codes2, _ := svc.GenerateCodes(context.Background(), 1)
	if len(codes2) != recoveryCodeCount {
		t.Fatalf("second generate = %d", len(codes2))
	}

	stored, _ := store.ListByUser(context.Background(), 1)
	if len(stored) != recoveryCodeCount {
		t.Errorf("after regenerate, stored = %d, want %d", len(stored), recoveryCodeCount)
	}

	// Old codes should no longer verify.
	for _, c := range codes1 {
		err := svc.VerifyCode(context.Background(), 1, c)
		if err == nil {
			t.Error("old code should not verify after regeneration")
		}
	}
}

func TestRecoveryVerifyCode(t *testing.T) {
	store := newFakeRecoveryStore()
	svc := NewRecoveryService(store, nil)

	codes, _ := svc.GenerateCodes(context.Background(), 1)

	// Verify the first code.
	err := svc.VerifyCode(context.Background(), 1, codes[0])
	if err != nil {
		t.Fatalf("VerifyCode: %v", err)
	}

	// The same code should not verify again (one-time use).
	err = svc.VerifyCode(context.Background(), 1, codes[0])
	if err == nil {
		t.Error("code should not verify twice (one-time use)")
	}

	// A second code should still work.
	err = svc.VerifyCode(context.Background(), 1, codes[1])
	if err != nil {
		t.Fatalf("VerifyCode[1]: %v", err)
	}
}

func TestRecoveryVerifyWrongCode(t *testing.T) {
	store := newFakeRecoveryStore()
	svc := NewRecoveryService(store, nil)
	svc.GenerateCodes(context.Background(), 1)

	err := svc.VerifyCode(context.Background(), 1, "wrong-code-here")
	if err == nil {
		t.Error("wrong code should not verify")
	}
}

func TestRecoveryVerifyEmptyCode(t *testing.T) {
	store := newFakeRecoveryStore()
	svc := NewRecoveryService(store, nil)
	svc.GenerateCodes(context.Background(), 1)

	err := svc.VerifyCode(context.Background(), 1, "")
	if err == nil {
		t.Error("empty code should not verify")
	}
}

func TestRecoveryHasRecoveryCodes(t *testing.T) {
	store := newFakeRecoveryStore()
	svc := NewRecoveryService(store, nil)

	has, _ := svc.HasRecoveryCodes(context.Background(), 1)
	if has {
		t.Error("should have no codes initially")
	}

	svc.GenerateCodes(context.Background(), 1)

	has, _ = svc.HasRecoveryCodes(context.Background(), 1)
	if !has {
		t.Error("should have codes after generation")
	}
}

func TestRecoveryCountUnused(t *testing.T) {
	store := newFakeRecoveryStore()
	svc := NewRecoveryService(store, nil)
	codes, _ := svc.GenerateCodes(context.Background(), 1)

	count, _ := svc.CountUnused(context.Background(), 1)
	if count != recoveryCodeCount {
		t.Errorf("CountUnused = %d, want %d", count, recoveryCodeCount)
	}

	// Use one code.
	svc.VerifyCode(context.Background(), 1, codes[0])

	count, _ = svc.CountUnused(context.Background(), 1)
	if count != recoveryCodeCount-1 {
		t.Errorf("CountUnused after use = %d, want %d", count, recoveryCodeCount-1)
	}
}

func TestFormatRecoveryCode(t *testing.T) {
	got := formatRecoveryCode("abcdefghijklmnop")
	if got != "abcd-efgh-ijkl-mnop" {
		t.Errorf("formatRecoveryCode = %q, want abcd-efgh-ijkl-mnop", got)
	}
	// Shorter string.
	got = formatRecoveryCode("abc")
	if got != "abc" {
		t.Errorf("formatRecoveryCode('abc') = %q, want abc", got)
	}
	// 19-char truncation (114 bits ≥ NIST 800-63B 112-bit threshold).
	got = formatRecoveryCode("abcdefghijklmnopqrst") // 20 chars
	if got != "abcd-efgh-ijkl-mnop-qrs" {
		t.Errorf("formatRecoveryCode(20 chars) = %q, want abcd-efgh-ijkl-mnop-qrs (truncated to 19)", got)
	}
}

func TestRecoveryGenerateCodes_Entropy(t *testing.T) {
	store := newFakeRecoveryStore()
	svc := NewRecoveryService(store, nil)
	codes, err := svc.GenerateCodes(context.Background(), 1)
	if err != nil {
		t.Fatalf("GenerateCodes: %v", err)
	}
	for i, code := range codes {
		// 19 base64 chars + 4 formatting dashes = 23 chars minimum.
		// (base64 RawURLEncoding uses '-' as a valid char, so the
		// formatted code may be longer if a '-' lands in the base64
		// output — 23 is the floor.)
		if len(code) < 23 {
			t.Errorf("code %d: %d chars, want ≥23 (19 base64 + 4 dashes = 114 bits ≥ NIST 112-bit threshold)", i, len(code))
		}
	}
}
