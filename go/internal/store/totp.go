// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type totpView struct{ d *DB }

func (d *DB) TOTP() TOTP { return totpView{d} }

func (v totpView) Save(ctx context.Context, s TOTPSecret) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO totp_secrets (user_id, secret, confirmed, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET secret = excluded.secret, confirmed = excluded.confirmed, created_at = excluded.created_at`,
		s.UserID, s.Secret, boolInt(s.Confirmed), unixOf(orNow(s.CreatedAt)),
	)
	if err != nil {
		return fmt.Errorf("store: totp.save: %w", err)
	}
	return nil
}

func (v totpView) Get(ctx context.Context, userID int64) (TOTPSecret, error) {
	var s TOTPSecret
	var confirmed int64
	var created int64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT user_id, secret, confirmed, created_at
		 FROM totp_secrets WHERE user_id = ?`, userID,
	).Scan(&s.UserID, &s.Secret, &confirmed, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return TOTPSecret{}, ErrNotFound
	}
	if err != nil {
		return TOTPSecret{}, fmt.Errorf("store: totp.get: %w", err)
	}
	s.Confirmed = confirmed != 0
	s.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
	return s, nil
}

func (v totpView) Confirm(ctx context.Context, userID int64) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE totp_secrets SET confirmed = 1 WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("store: totp.confirm: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v totpView) Delete(ctx context.Context, userID int64) error {
	res, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM totp_secrets WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("store: totp.delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
