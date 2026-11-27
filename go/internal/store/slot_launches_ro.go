// SPDX-License-Identifier: Apache-2.0

package store

// The read-only opener for `forge slot-exec` (ADR-0026 section 4). It is deliberately NOT Open: Open creates the file, sets the journal mode and runs
// migrations, none of which a launcher may do. This path opens an existing database with mode=ro and query_only, never creates a file, never
// migrates, and refuses a database whose schema predates slot_launches. It exposes exactly what the launcher needs: one slot's row and a setting
// (catalog.build_roots).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// minSlotLaunchesSchema is the migration that created slot_launches.
const minSlotLaunchesSchema = 104

var (
	// ErrDBMissing: the database file does not exist (nothing is created).
	ErrDBMissing = errors.New("store: database file does not exist")
	// ErrSchemaTooOld: the database has no schema_migrations table, or its version is older than the one that created slot_launches.
	ErrSchemaTooOld = errors.New("store: database schema predates slot_launches (migration 0104)")
	// ErrSlotLaunchesMissing: the schema version claims 0104 or later but the table is not there.
	ErrSlotLaunchesMissing = errors.New("store: slot_launches table is missing")
)

// ReadOnlyDB is a read-only handle to the state database.
type ReadOnlyDB struct{ sql *sql.DB }

// OpenReadOnly opens the existing database at path read-only. It never writes, creates, migrates or changes the journal mode. Errors: ErrDBMissing,
// ErrSchemaTooOld, ErrSlotLaunchesMissing, or a wrapped driver error (for example the file is not a database or cannot be read).
//
// Reading a WAL database read-only needs the -shm file to be readable or creatable by this process; that is the caller's environment, tested here
// with a concurrent writer and verified on the host in plan step (c).
func OpenReadOnly(path string) (*ReadOnlyDB, error) {
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("store: open read-only: need a database file path, got %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrDBMissing, path)
		}
		return nil, fmt.Errorf("store: open read-only %s: %w", path, err)
	}
	// Escape the path so a file name containing ? or # cannot inject a DSN parameter.
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath()}
	dsn := u.String() + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open read-only %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	r := &ReadOnlyDB{sql: db}
	if err := r.checkSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

func (r *ReadOnlyDB) checkSchema(ctx context.Context) error {
	var hasMigrations int
	if err := r.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&hasMigrations); err != nil {
		return fmt.Errorf("store: open read-only: read schema: %w", err)
	}
	if hasMigrations == 0 {
		return ErrSchemaTooOld
	}
	var version int
	if err := r.sql.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("store: open read-only: read schema version: %w", err)
	}
	if version < minSlotLaunchesSchema {
		return fmt.Errorf("%w (database is at version %d)", ErrSchemaTooOld, version)
	}
	var hasTable int
	if err := r.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'slot_launches'`).Scan(&hasTable); err != nil {
		return fmt.Errorf("store: open read-only: read schema: %w", err)
	}
	if hasTable == 0 {
		return ErrSlotLaunchesMissing
	}
	return nil
}

// Close closes the handle.
func (r *ReadOnlyDB) Close() error { return r.sql.Close() }

// GetSlotLaunch returns the slot's row: ErrNotFound if there is none, ErrSlotLaunchMalformed if its JSON columns do not decode. The caller decides
// what armed and format_version mean.
func (r *ReadOnlyDB) GetSlotLaunch(ctx context.Context, slot string) (SlotLaunch, error) {
	return getSlotLaunch(ctx, r.sql, slot)
}

// GetSetting returns a settings value, or ErrNotFound when unset.
func (r *ReadOnlyDB) GetSetting(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	err := r.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read-only settings.get %q: %w", key, err)
	}
	return value, nil
}

// Settings and Catalog expose the same read views the daemon uses (so `forge launch-check` loads the catalog through the daemon's own code path)
// on this read-only handle. The connection is mode=ro with query_only, so a write method on either view fails in SQLite rather than writing; the
// views touch nothing but this handle (no audit mirror is configured).
func (r *ReadOnlyDB) Settings() Settings { return settingsView{&DB{sql: r.sql}} }

// Catalog: see Settings.
func (r *ReadOnlyDB) Catalog() Catalog { return catalogView{&DB{sql: r.sql}} }
