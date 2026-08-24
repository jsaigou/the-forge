// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type keysView struct{ d *DB }

// Keys returns the bearer-key surface. One row per keyid so exactly one
// Argon2 verify runs per request (Contract 1 §1). Secret hashes never appear
// in error messages or logs.
func (d *DB) Keys() Keys { return keysView{d} }

func (v keysView) Create(ctx context.Context, k APIKey) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO api_keys (keyid, kind, name, secret_hash, role, created_at,
		                       last_used_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.KeyID, k.Kind, k.Name, k.SecretHash, nullStr(k.Role),
		unixOf(orNow(k.CreatedAt)), nullUnix(k.LastUsedAt), nullUnix(k.RevokedAt),
	)
	if err != nil {
		return fmt.Errorf("store: keys.create: %w", err)
	}
	return nil
}

func (v keysView) Get(ctx context.Context, keyid string) (APIKey, error) {
	var k APIKey
	var role sql.NullString
	var created int64
	var lastUsed, revoked sql.NullInt64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT keyid, kind, name, secret_hash, role, created_at, last_used_at, revoked_at
		 FROM api_keys WHERE keyid = ?`, keyid,
	).Scan(&k.KeyID, &k.Kind, &k.Name, &k.SecretHash, &role, &created, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("store: keys.get: %w", err)
	}
	k.Role = strOf(role)
	k.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
	k.LastUsedAt = timeOf(lastUsed)
	k.RevokedAt = timeOf(revoked)
	return k, nil
}

// List returns keys of one kind, or all kinds when kind == "".
func (v keysView) List(ctx context.Context, kind string) ([]APIKey, error) {
	query := `SELECT keyid, kind, name, secret_hash, role, created_at, last_used_at, revoked_at
	          FROM api_keys`
	args := []any{}
	if kind != "" {
		query += ` WHERE kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY created_at, keyid`
	rows, err := v.d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: keys.list: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var role sql.NullString
		var created int64
		var lastUsed, revoked sql.NullInt64
		if err := rows.Scan(&k.KeyID, &k.Kind, &k.Name, &k.SecretHash, &role,
			&created, &lastUsed, &revoked); err != nil {
			return nil, fmt.Errorf("store: keys.list: %w", err)
		}
		k.Role = strOf(role)
		k.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
		k.LastUsedAt = timeOf(lastUsed)
		k.RevokedAt = timeOf(revoked)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: keys.list: %w", err)
	}
	return out, nil
}

// Revoke soft-revokes a key (revoked_at set). Revoking an already-revoked
// key is a no-op; ErrNotFound only when the keyid does not exist at all.
func (v keysView) Revoke(ctx context.Context, keyid string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE keyid = ? AND revoked_at IS NULL`,
		unixOf(time.Now()), keyid)
	if err != nil {
		return fmt.Errorf("store: keys.revoke: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := v.Get(ctx, keyid); err != nil {
			return err
		}
	}
	return nil
}

func (v keysView) TouchUsed(ctx context.Context, keyid string, at time.Time) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE keyid = ?`, unixOf(at), keyid)
	if err != nil {
		return fmt.Errorf("store: keys.touch: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
