// SPDX-License-Identifier: Apache-2.0

package httpapi

// recovery_adapters.go — bridges store.RecoveryCodes to
// authz.RecoveryCodeStore (the authz package doesn't import store).

import (
	"context"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

type recoveryStoreAdapter struct {
	store store.RecoveryCodes
}

// NewRecoveryStoreAdapter returns an authz.RecoveryCodeStore backed by the
// given store implementation.
func NewRecoveryStoreAdapter(s store.RecoveryCodes) authz.RecoveryCodeStore {
	return &recoveryStoreAdapter{store: s}
}

func (a *recoveryStoreAdapter) Save(ctx context.Context, codes []authz.RecoveryCodeRecord) error {
	records := make([]store.RecoveryCode, len(codes))
	for i, c := range codes {
		records[i] = store.RecoveryCode{
			UserID:    c.UserID,
			CodeHash:  c.CodeHash,
			CreatedAt: c.CreatedAt,
			UsedAt:    c.UsedAt,
		}
	}
	return a.store.Save(ctx, records)
}

func (a *recoveryStoreAdapter) ListByUser(ctx context.Context, userID int64) ([]authz.RecoveryCodeRecord, error) {
	codes, err := a.store.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]authz.RecoveryCodeRecord, len(codes))
	for i, c := range codes {
		out[i] = authz.RecoveryCodeRecord{
			ID:        c.ID,
			UserID:     c.UserID,
			CodeHash:  c.CodeHash,
			CreatedAt: c.CreatedAt,
			UsedAt:    c.UsedAt,
		}
	}
	return out, nil
}

func (a *recoveryStoreAdapter) MarkUsed(ctx context.Context, id int64, at time.Time) error {
	return a.store.MarkUsed(ctx, id, at)
}

func (a *recoveryStoreAdapter) DeleteAll(ctx context.Context, userID int64) error {
	return a.store.DeleteAll(ctx, userID)
}
