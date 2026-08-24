// SPDX-License-Identifier: Apache-2.0

package authz

// recovery.go — Sprint 0-AUTH Phase C (§8): recovery codes for TOTP/passkey
// lockout.
//
// A lost authenticator must not be a permanent lockout. Recovery codes are
// Argon2id-hashed at rest (same posture as passwords and API keys). The
// plaintext is shown to the user exactly once on generation. A recovery
// code can be used as a step-up factor to re-gain access, after which the
// user should re-enroll TOTP/passkeys.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// RecoveryCodeStore is the persistence surface for recovery codes
// (mirrors store.RecoveryCodes; declared here so authz doesn't import store).
type RecoveryCodeStore interface {
	Save(ctx context.Context, codes []RecoveryCodeRecord) error
	ListByUser(ctx context.Context, userID int64) ([]RecoveryCodeRecord, error)
	MarkUsed(ctx context.Context, id int64, at time.Time) error
	DeleteAll(ctx context.Context, userID int64) error
}

// RecoveryCodeRecord mirrors store.RecoveryCode.
type RecoveryCodeRecord struct {
	ID        int64
	UserID    int64
	CodeHash  string
	CreatedAt time.Time
	UsedAt    time.Time
}

// RecoveryService generates + verifies recovery codes.
//
// Hashing rationale — DO NOT "upgrade" this to Argon2id without reading this:
// recovery codes are HIGH-ENTROPY random look-up secrets, not passwords, so
// they are hashed at rest with HMAC-SHA256 keyed by a server pepper — a fast,
// approved one-way function — NOT a memory-hard password KDF.
//
//   - Each code carries 114 bits of entropy: GenerateCodes draws 16 random
//     bytes (128 bits) but formatRecoveryCode truncates the base64 encoding
//     to 19 chars (114 bits) for readability. NIST SP 800-63B §5.1.2 says
//     look-up secrets with ≥112 bits of entropy "SHALL be hashed with an
//     approved one-way function"; only sub-112-bit secrets require a
//     memory-hard KDF. Argon2id here would be a category error: a KDF's
//     slowness only buys resistance to brute-forcing GUESSABLE inputs,
//     which a 114-bit random code is not.
//   - It is also actively harmful here: VerifyCode compares against every
//     stored code, so an Argon2id verify path turns each wrong-code submission
//     into N × 64 MiB hashes — an attacker-triggerable DoS amplifier — and
//     GenerateCodes' N sequential Argon2id hashes routinely blew the request
//     timeout under CPU load. HMAC-SHA256 makes both paths ~microseconds.
//   - The pepper (HMAC key) is defense-in-depth per OWASP's peppering guidance
//     (keyed hash, key stored apart from the hashes); see loadRecoveryPepper in
//     cmd/forge. It is not required for compliance at 128-bit entropy, so an
//     empty pepper (tests) is acceptable — hashing then degrades to unkeyed
//     SHA-256, still an approved one-way function for 114-bit codes.
//   - Per-code salting is omitted deliberately: salt defends against
//     precomputation / rainbow-table attacks on low-entropy or repeated inputs;
//     114-bit random codes make precomputation and cross-user collision
//     infeasible, and the pepper already blocks offline attack without the key.
//
// Full write-up: docs/v5-sprint0-auth-design.md "Recovery-code hashing".
type RecoveryService struct {
	store  RecoveryCodeStore
	pepper []byte
}

// NewRecoveryService returns a RecoveryService backed by the given store. pepper
// keys the HMAC-SHA256 at-rest hash (see the type doc); it may be nil in tests,
// where hashing degrades to unkeyed SHA-256.
func NewRecoveryService(store RecoveryCodeStore, pepper []byte) *RecoveryService {
	return &RecoveryService{store: store, pepper: pepper}
}

// hashCode is the at-rest hash of a recovery code: HMAC-SHA256(pepper, code),
// hex-encoded. Deterministic (no per-code salt — see the type doc), so
// VerifyCode can recompute and constant-time-compare.
func (s *RecoveryService) hashCode(code string) string {
	mac := hmac.New(sha256.New, s.pepper)
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

// GenerateCodes generates n random recovery codes, hashes them, stores the
// hashes, and returns the plaintext codes (shown to the user exactly once).
// Any existing codes for the user are deleted first (regenerate = replace).
const recoveryCodeCount = 10

// GenerateCodes generates a fresh set of recovery codes for the user.
// Existing codes are deleted first. Returns the plaintext codes (shown once).
func (s *RecoveryService) GenerateCodes(ctx context.Context, userID int64) ([]string, error) {
	// Delete existing codes (regenerate = replace all).
	if err := s.store.DeleteAll(ctx, userID); err != nil {
		return nil, fmt.Errorf("authz: recovery: delete old: %w", err)
	}
	plaintextCodes := make([]string, recoveryCodeCount)
	records := make([]RecoveryCodeRecord, recoveryCodeCount)
	now := time.Now()
	for i := 0; i < recoveryCodeCount; i++ {
		raw := make([]byte, 16) // 128 bits random; truncated to 19 base64
		if _, err := rand.Read(raw); err != nil { // chars (114 bits) by formatRecoveryCode —
			return nil, fmt.Errorf("authz: recovery: rand: %w", err) // above NIST 800-63B §5.1.2's 112-bit threshold
		}
		code := formatRecoveryCode(base64.RawURLEncoding.EncodeToString(raw))
		plaintextCodes[i] = code
		records[i] = RecoveryCodeRecord{
			UserID:    userID,
			CodeHash:  s.hashCode(code),
			CreatedAt: now,
		}
	}
	if err := s.store.Save(ctx, records); err != nil {
		return nil, fmt.Errorf("authz: recovery: save: %w", err)
	}
	return plaintextCodes, nil
}

// VerifyCode checks whether code is a valid, unused recovery code for the
// user. On success, the code is marked as used (one-time use). Returns the
// code's record ID on success, or ErrUnauthenticated on failure.
func (s *RecoveryService) VerifyCode(ctx context.Context, userID int64, code string) error {
	if code == "" || len(code) > maxSecretLen {
		return ErrUnauthenticated
	}
	codes, err := s.store.ListByUser(ctx, userID)
	if err != nil {
		return ErrUnauthenticated
	}
	want := s.hashCode(code) // same pepper + algorithm for every stored code
	for _, c := range codes {
		if !c.UsedAt.IsZero() {
			continue // already used
		}
		// Constant-time compare so timing can't reveal a partial-hash match.
		if subtle.ConstantTimeCompare([]byte(c.CodeHash), []byte(want)) != 1 {
			continue
		}
		// Found a match — mark it used.
		if err := s.store.MarkUsed(ctx, c.ID, time.Now()); err != nil {
			return fmt.Errorf("authz: recovery: mark used: %w", err)
		}
		return nil
	}
	return ErrUnauthenticated
}

// HasRecoveryCodes reports whether the user has any recovery codes set.
func (s *RecoveryService) HasRecoveryCodes(ctx context.Context, userID int64) (bool, error) {
	codes, err := s.store.ListByUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return len(codes) > 0, nil
}

// CountUnused returns the number of unused recovery codes remaining.
func (s *RecoveryService) CountUnused(ctx context.Context, userID int64) (int, error) {
	codes, err := s.store.ListByUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, c := range codes {
		if c.UsedAt.IsZero() {
			count++
		}
	}
	return count, nil
}

// formatRecoveryCode formats a raw base64 string as XXXX-XXXX-XXXX-XXXX-XXX
// for readability (users read + type these manually on lockout).
func formatRecoveryCode(raw string) string {
	// Truncate to 19 chars (114 bits of entropy — above NIST SP 800-63B
	// §5.1.2's 112-bit fast-hash threshold), group in 4s with dashes.
	s := raw
	if len(s) > 19 {
		s = s[:19]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, c)
	}
	return string(out)
}
