// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"
)

type favoritesView struct{ d *DB }

// Favorites returns the per-operator starred-subject surface (product/QA
// sprint, 2026-07-29).
func (d *DB) Favorites() Favorites { return favoritesView{d} }

// Add stars subject for username. INSERT OR IGNORE makes a double-star a
// no-op rather than a UNIQUE-constraint error — the caller (a toggle
// button) doesn't need to check existence first.
func (v favoritesView) Add(ctx context.Context, username, subjectType string, subjectID int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO favorites (username, subject_type, subject_id, created_at)
		 VALUES (?, ?, ?, ?)`,
		username, subjectType, subjectID, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: favorites.add: %w", err)
	}
	return nil
}

// Remove un-stars. A no-op (not an error) when the row doesn't exist —
// the caller doesn't need to check existence first.
func (v favoritesView) Remove(ctx context.Context, username, subjectType string, subjectID int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM favorites WHERE username = ? AND subject_type = ? AND subject_id = ?`,
		username, subjectType, subjectID,
	)
	if err != nil {
		return fmt.Errorf("store: favorites.remove: %w", err)
	}
	return nil
}

// List returns every subject username has starred, of subjectType, most
// recently starred first.
func (v favoritesView) List(ctx context.Context, username, subjectType string) ([]Favorite, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, username, subject_type, subject_id, created_at
		 FROM favorites WHERE username = ? AND subject_type = ?
		 ORDER BY created_at DESC`,
		username, subjectType,
	)
	if err != nil {
		return nil, fmt.Errorf("store: favorites.list: %w", err)
	}
	defer rows.Close()
	out := []Favorite{}
	for rows.Next() {
		var f Favorite
		var created int64
		if err := rows.Scan(&f.ID, &f.Username, &f.SubjectType, &f.SubjectID, &created); err != nil {
			return nil, fmt.Errorf("store: favorites.list: %w", err)
		}
		f.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}
