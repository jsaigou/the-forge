// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type usersView struct{ d *DB }

// Users returns the user-account surface.
func (d *DB) Users() Users { return usersView{d} }

func (v usersView) Create(ctx context.Context, u User) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, role, created_at, disabled)
		 VALUES (?, ?, ?, ?, ?)`,
		u.Username, u.PasswordHash, u.Role, unixOf(orNow(u.CreatedAt)), boolInt(u.Disabled),
	)
	if err != nil {
		return 0, fmt.Errorf("store: users.create: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: users.create: %w", err)
	}
	return id, nil
}

func (v usersView) ByUsername(ctx context.Context, username string) (User, error) {
	return v.scanOne(v.d.sql.QueryRowContext(ctx,
		`SELECT id, username, password_hash, role, created_at, disabled
		 FROM users WHERE username = ?`, username))
}

func (v usersView) List(ctx context.Context) ([]User, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, username, password_hash, role, created_at, disabled
		 FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: users.list: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created int64
		var disabled int64
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created, &disabled); err != nil {
			return nil, fmt.Errorf("store: users.list: %w", err)
		}
		u.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
		u.Disabled = disabled != 0
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: users.list: %w", err)
	}
	return out, nil
}

func (v usersView) Delete(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: users.delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v usersView) scanOne(row *sql.Row) (User, error) {
	var u User
	var created, disabled int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: users.get: %w", err)
	}
	u.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
	u.Disabled = disabled != 0
	return u, nil
}
