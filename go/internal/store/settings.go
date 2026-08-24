// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type settingsView struct{ d *DB }

// Settings returns the JSON KV surface for app-mutated ex-config.toml
// values. Keys are the Contract 3 list (ui.help_button, nfs.shares, ...,
// wizard.completed); new keys are additive but must be recorded there via
// amendment. (ui.theme, ui.bookmarks, notes.sections, and hf.token were
// dropped from this vocabulary in Sprint 12 Phase 1 — zero production
// readers found; see store.go's Settings doc comment.) Any key holding a
// secret must never appear in errors or logs.
func (d *DB) Settings() Settings { return settingsView{d} }

func (v settingsView) Get(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: settings.get %q: %w", key, err)
	}
	return value, nil
}

func (v settingsView) Set(ctx context.Context, key string, value []byte) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   value = excluded.value, updated_at = excluded.updated_at`,
		key, value, unixOf(time.Now()))
	if err != nil {
		return fmt.Errorf("store: settings.set %q: %w", key, err)
	}
	return nil
}
