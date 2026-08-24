// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // pure-Go driver — keeps the static build (no cgo)
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps the SQLite handle. Phase 3 implements the Store interface on it;
// Phase 1 provides Open + the migration runner (Contract 3 scaffolding).
type DB struct {
	sql *DBHandle

	auditMirror string // optional JSONL mirror path ("" = off)
	auditMu     sync.Mutex
}

// DBHandle aliases *sql.DB so Phase 3 can extend without re-exporting.
type DBHandle = sql.DB

// Option configures Open.
type Option func(*DB)

// WithAuditMirror enables the optional JSONL mirror of the audit log
// (docs/v5-store-schema.md: DB is primary; the mirror is a Phase 3 knob for
// V4-style /var/log/forge/audit.log tailing). Mirror writes are
// best-effort — a mirror failure never fails the DB write.
func WithAuditMirror(path string) Option {
	return func(d *DB) { d.auditMirror = path }
}

// Open opens (creating if needed) the state database at path and applies any
// pending migrations. Use path ":memory:" for tests.
func Open(path string, opts ...Option) (*DB, error) {
	// WAL + busy_timeout + foreign_keys per the V5 risk register
	// (docs/v5-plan.md): single-writer discipline lives in the store API,
	// these pragmas keep readers cheap and writes durable.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection, always: SQLite serializes writes anyway, a second
	// pooled connection would only add SQLITE_BUSY paths, and for
	// ":memory:" each pooled connection is a *separate* database — the
	// single connection is what makes the test fixture correct.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	d := &DB{sql: db}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

// SQL exposes the underlying handle (Phase 3 internal use and tests).
func (d *DB) SQL() *DBHandle { return d.sql }

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// migrationVersion extracts N from "migrations/NNNN_name.sql".
func migrationVersion(name string) (int, error) {
	base := strings.TrimPrefix(name, "migrations/")
	idx := strings.IndexByte(base, '_')
	if idx < 1 {
		return 0, fmt.Errorf("migration %q: want NNNN_name.sql", name)
	}
	return strconv.Atoi(base[:idx])
}

// migrate applies embedded migrations, in version order, that are newer than
// the recorded schema version. Each migration runs in one transaction.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`,
	); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRow(
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`,
	).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if version <= current {
			continue
		}
		body, err := migrationsFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, datetime('now'))`,
			version,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}
