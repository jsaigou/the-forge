// SPDX-License-Identifier: Apache-2.0

package httpapi

// webauthn_adapters.go — bridges store types to authz.WebAuthn* interfaces
// (the authz package deliberately doesn't import store to avoid a cycle;
// the httpapi layer adapts the concrete store implementations).

import (
	"context"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

// storeCredentialAdapter wraps store.WebAuthnCredentials to implement
// authz.WebAuthnCredentialStore.
type storeCredentialAdapter struct {
	store store.WebAuthnCredentials
}

// NewWebAuthnCredentialStoreAdapter returns an authz.WebAuthnCredentialStore
// backed by the given store implementation. Used by cmd/forge wiring.
func NewWebAuthnCredentialStoreAdapter(s store.WebAuthnCredentials) authz.WebAuthnCredentialStore {
	return &storeCredentialAdapter{store: s}
}

func (a *storeCredentialAdapter) Save(ctx context.Context, c authz.WebAuthnCredentialRecord) error {
	return a.store.Save(ctx, store.WebAuthnCredential{
		ID:         c.ID,
		UserID:     c.UserID,
		PublicKey:  c.PublicKey,
		SignCount:  c.SignCount,
		Transports: c.Transports,
		Label:      c.Label,
		CreatedAt:  c.CreatedAt,
		LastUsedAt: c.LastUsedAt,
	})
}

func (a *storeCredentialAdapter) Get(ctx context.Context, id string) (authz.WebAuthnCredentialRecord, error) {
	c, err := a.store.Get(ctx, id)
	if err != nil {
		return authz.WebAuthnCredentialRecord{}, err
	}
	return toAuthzCredential(c), nil
}

func (a *storeCredentialAdapter) ListByUser(ctx context.Context, userID int64) ([]authz.WebAuthnCredentialRecord, error) {
	creds, err := a.store.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]authz.WebAuthnCredentialRecord, len(creds))
	for i, c := range creds {
		out[i] = toAuthzCredential(c)
	}
	return out, nil
}

func (a *storeCredentialAdapter) Delete(ctx context.Context, id string) error {
	return a.store.Delete(ctx, id)
}

func (a *storeCredentialAdapter) UpdateSignCount(ctx context.Context, id string, signCount uint32, at time.Time) error {
	return a.store.UpdateSignCount(ctx, id, signCount, at)
}

func toAuthzCredential(c store.WebAuthnCredential) authz.WebAuthnCredentialRecord {
	return authz.WebAuthnCredentialRecord{
		ID:         c.ID,
		UserID:     c.UserID,
		PublicKey:  c.PublicKey,
		SignCount:  c.SignCount,
		Transports: c.Transports,
		Label:      c.Label,
		CreatedAt:  c.CreatedAt,
		LastUsedAt: c.LastUsedAt,
	}
}

// storeUserAdapter wraps store.Users to implement authz.WebAuthnUserStore.
type storeUserAdapter struct {
	users store.Users
}

// NewWebAuthnUserStoreAdapter returns an authz.WebAuthnUserStore backed by
// the given store implementation. Used by cmd/forge wiring.
func NewWebAuthnUserStoreAdapter(u store.Users) authz.WebAuthnUserStore {
	return &storeUserAdapter{users: u}
}

func (a *storeUserAdapter) ByUsername(ctx context.Context, username string) (authz.WebAuthnUser, error) {
	u, err := a.users.ByUsername(ctx, username)
	if err != nil {
		return authz.WebAuthnUser{}, err
	}
	return authz.WebAuthnUser{
		ID:           u.ID,
		Username:     u.Username,
		PasswordHash: u.PasswordHash,
		Role:         u.Role,
		CreatedAt:    u.CreatedAt,
		Disabled:     u.Disabled,
	}, nil
}
