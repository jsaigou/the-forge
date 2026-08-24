// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type webauthnCredentialsView struct{ d *DB }

func (d *DB) WebAuthnCredentials() WebAuthnCredentials { return webauthnCredentialsView{d} }

func (v webauthnCredentialsView) Save(ctx context.Context, c WebAuthnCredential) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO webauthn_credentials (id, user_id, public_key, sign_count, transports, label, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   public_key = excluded.public_key,
		   sign_count = excluded.sign_count,
		   transports = excluded.transports,
		   label = excluded.label,
		   last_used_at = excluded.last_used_at`,
		c.ID, c.UserID, c.PublicKey, c.SignCount, nullStr(c.Transports),
		c.Label, unixOf(orNow(c.CreatedAt)), nullUnix(c.LastUsedAt),
	)
	if err != nil {
		return fmt.Errorf("store: webauthn.save: %w", err)
	}
	return nil
}

func (v webauthnCredentialsView) Get(ctx context.Context, id string) (WebAuthnCredential, error) {
	var c WebAuthnCredential
	var transports sql.NullString
	var created int64
	var lastUsed sql.NullInt64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, user_id, public_key, sign_count, transports, label, created_at, last_used_at
		 FROM webauthn_credentials WHERE id = ?`, id,
	).Scan(&c.ID, &c.UserID, &c.PublicKey, &c.SignCount, &transports, &c.Label, &created, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return WebAuthnCredential{}, ErrNotFound
	}
	if err != nil {
		return WebAuthnCredential{}, fmt.Errorf("store: webauthn.get: %w", err)
	}
	c.Transports = strOf(transports)
	c.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
	c.LastUsedAt = timeOf(lastUsed)
	return c, nil
}

func (v webauthnCredentialsView) ListByUser(ctx context.Context, userID int64) ([]WebAuthnCredential, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, user_id, public_key, sign_count, transports, label, created_at, last_used_at
		 FROM webauthn_credentials WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: webauthn.list: %w", err)
	}
	defer rows.Close()
	var out []WebAuthnCredential
	for rows.Next() {
		var c WebAuthnCredential
		var transports sql.NullString
		var created int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&c.ID, &c.UserID, &c.PublicKey, &c.SignCount, &transports, &c.Label, &created, &lastUsed); err != nil {
			return nil, fmt.Errorf("store: webauthn.scan: %w", err)
		}
		c.Transports = strOf(transports)
		c.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
		c.LastUsedAt = timeOf(lastUsed)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: webauthn.rows: %w", err)
	}
	return out, nil
}

func (v webauthnCredentialsView) Delete(ctx context.Context, id string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: webauthn.delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v webauthnCredentialsView) UpdateSignCount(ctx context.Context, id string, signCount uint32, at time.Time) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE webauthn_credentials SET sign_count = ?, last_used_at = ? WHERE id = ?`,
		signCount, unixOf(at), id)
	if err != nil {
		return fmt.Errorf("store: webauthn.update_sign_count: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// EncodeTransports serializes a list of transport strings as JSON for storage.
func EncodeTransports(transports []string) (string, error) {
	if len(transports) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(transports)
	if err != nil {
		return "", fmt.Errorf("store: encode transports: %w", err)
	}
	return string(raw), nil
}

// DecodeTransports deserializes a JSON transport array from storage.
func DecodeTransports(raw string) []string {
	if raw == "" {
		return nil
	}
	var transports []string
	if err := json.Unmarshal([]byte(raw), &transports); err != nil {
		return nil
	}
	return transports
}
