// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type identityLinksView struct{ d *DB }

func (d *DB) IdentityLinks() IdentityLinks { return identityLinksView{d} }

func (v identityLinksView) Create(ctx context.Context, link IdentityLink) error {
	// Not an upsert: silently re-pointing an existing principal at a different user would hand that network identity to someone else with
	// nothing but an "identity_link_create" audit row. Same user = idempotent no-op; different user = ErrConflict (delete first to replace).
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO identity_links (provider, principal, user_id, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(provider, principal) DO NOTHING`,
		link.Provider, link.Principal, link.UserID, unixOf(orNow(link.CreatedAt)),
	)
	if err != nil {
		return fmt.Errorf("store: identity_links.create: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	var existing int64
	if err := v.d.sql.QueryRowContext(ctx,
		`SELECT user_id FROM identity_links WHERE provider = ? AND principal = ?`, link.Provider, link.Principal).Scan(&existing); err != nil {
		return fmt.Errorf("store: identity_links.create: read existing: %w", err)
	}
	if existing != link.UserID {
		return fmt.Errorf("store: identity_links.create: %s:%s is already linked to another user: %w", link.Provider, link.Principal, ErrConflict)
	}
	return nil
}

func (v identityLinksView) Delete(ctx context.Context, provider, principal string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM identity_links WHERE provider = ? AND principal = ?`,
		provider, principal)
	if err != nil {
		return fmt.Errorf("store: identity_links.delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v identityLinksView) ListByUser(ctx context.Context, userID int64) ([]IdentityLink, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT provider, principal, user_id, created_at
		 FROM identity_links WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: identity_links.list_by_user: %w", err)
	}
	return scanIdentityLinks(rows)
}

func (v identityLinksView) List(ctx context.Context) ([]IdentityLink, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT provider, principal, user_id, created_at
		 FROM identity_links ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: identity_links.list: %w", err)
	}
	return scanIdentityLinks(rows)
}

func (v identityLinksView) Lookup(ctx context.Context, provider, principal string) (int64, error) {
	var userID int64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT user_id FROM identity_links WHERE provider = ? AND principal = ?`,
		provider, principal).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: identity_links.lookup: %w", err)
	}
	return userID, nil
}

func scanIdentityLinks(rows *sql.Rows) ([]IdentityLink, error) {
	defer rows.Close()
	var out []IdentityLink
	for rows.Next() {
		var link IdentityLink
		var created int64
		if err := rows.Scan(&link.Provider, &link.Principal, &link.UserID, &created); err != nil {
			return nil, fmt.Errorf("store: identity_links.scan: %w", err)
		}
		link.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: identity_links.rows: %w", err)
	}
	return out, nil
}
