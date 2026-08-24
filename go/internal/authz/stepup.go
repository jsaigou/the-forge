// SPDX-License-Identifier: Apache-2.0

package authz

// stepup.go — Sprint 0-AUTH §3.5: step-up verification primitives.
//
// The Authorizer gains a VerifyPassword method that checks a password
// against the user's stored hash WITHOUT creating a new session (unlike
// Login). The step-up handler uses this to verify the password factor,
// then calls sessions.ElevateSession to rotate the session ID + set the
// assurance level.

import (
	"context"
	"fmt"
)

// StepUpVerifier verifies a factor for step-up authentication (§3.5). The
// *Authorizer implements this; tests may inject fakes.
type StepUpVerifier interface {
	// VerifyPassword checks password against the user's stored hash.
	// Returns nil on success, ErrUnauthenticated on mismatch.
	VerifyPassword(ctx context.Context, userID int64, password string) error
}

// VerifyPassword checks whether password matches the user's stored hash.
// Used by step-up (§3.5) to verify a password factor without creating a
// new session. Returns nil on success, ErrUnauthenticated on mismatch.
func (a *Authorizer) VerifyPassword(ctx context.Context, userID int64, password string) error {
	if password == "" || len(password) > maxSecretLen {
		return ErrUnauthenticated
	}
	u, err := a.userByID(ctx, userID)
	if err != nil {
		return ErrUnauthenticated
	}
	if u.Disabled {
		return ErrUnauthenticated
	}
	ok, err := a.hasher.Verify(u.PasswordHash, password)
	if err != nil || !ok {
		return ErrUnauthenticated
	}
	return nil
}

// RandomToken generates a random token for session ID / CSRF rotation.
// Exposed so the step-up handler can generate new session IDs + CSRF tokens
// without reaching into the unexported randToken helper.
func RandomToken(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("authz: RandomToken: n must be > 0")
	}
	return randToken(n)
}

// IdentityByID resolves a user ID to an Identity (username + role). Used by
// the network bootstrap path to create an Identity for a linked network
// principal without going through the session/bearer verification paths.
func (a *Authorizer) IdentityByID(ctx context.Context, userID int64) (Identity, error) {
	u, err := a.userByID(ctx, userID)
	if err != nil {
		return Identity{}, err
	}
	if u.Disabled {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{Name: u.Username, Role: Role(u.Role)}, nil
}

// ── Settings adapter for PolicyStore ─────────────────────────────────────────

// SettingsAdapter adapts store.Settings to the authz.PolicySettings seam.
// Kept in authz (not httpapi) so the policy store can be constructed from
// any store.Settings implementation without importing the httpapi package.
type SettingsAdapter struct {
	GetFn func(ctx context.Context, key string) ([]byte, error)
	SetFn func(ctx context.Context, key string, value []byte) error
}

func (s SettingsAdapter) Get(ctx context.Context, key string) ([]byte, error) {
	return s.GetFn(ctx, key)
}
func (s SettingsAdapter) Set(ctx context.Context, key string, value []byte) error {
	return s.SetFn(ctx, key, value)
}

// KeyManager is the API-key management surface (§6). The *Authorizer
// implements this; the HTTP routes for list/mint/revoke use it.
type KeyManager interface {
	MintKey(ctx context.Context, kind KeyKind, name string, role Role) (string, error)
	RevokeKey(ctx context.Context, keyid string) error
}
