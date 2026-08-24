// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type recoveryCodesView struct{ d *DB }

func (d *DB) RecoveryCodes() RecoveryCodes { return recoveryCodesView{d} }

func (v recoveryCodesView) Save(ctx context.Context, codes []RecoveryCode) error {
	if len(codes) == 0 {
		return nil
	}
	tx, err := v.d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: recovery_codes.save: %w", err)
	}
	defer tx.Rollback()
	for _, c := range codes {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO recovery_codes (user_id, code_hash, created_at, used_at) VALUES (?, ?, ?, ?)`,
			c.UserID, c.CodeHash, unixOf(orNow(c.CreatedAt)), nullUnix(c.UsedAt),
		)
		if err != nil {
			return fmt.Errorf("store: recovery_codes.save: %w", err)
		}
	}
	return tx.Commit()
}

func (v recoveryCodesView) ListByUser(ctx context.Context, userID int64) ([]RecoveryCode, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, user_id, code_hash, created_at, used_at FROM recovery_codes WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: recovery_codes.list: %w", err)
	}
	defer rows.Close()
	var out []RecoveryCode
	for rows.Next() {
		var c RecoveryCode
		var created int64
		var usedAt sql.NullInt64
		if err := rows.Scan(&c.ID, &c.UserID, &c.CodeHash, &created, &usedAt); err != nil {
			return nil, fmt.Errorf("store: recovery_codes.scan: %w", err)
		}
		c.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
		c.UsedAt = timeOf(usedAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recovery_codes.rows: %w", err)
	}
	return out, nil
}

func (v recoveryCodesView) MarkUsed(ctx context.Context, id int64, at time.Time) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE recovery_codes SET used_at = ? WHERE id = ? AND used_at IS NULL`,
		unixOf(at), id)
	if err != nil {
		return fmt.Errorf("store: recovery_codes.mark_used: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v recoveryCodesView) DeleteAll(ctx context.Context, userID int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM recovery_codes WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("store: recovery_codes.delete_all: %w", err)
	}
	return nil
}
