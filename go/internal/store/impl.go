// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"time"
)

// *DB implements the frozen Store interface (Contract 2). Each domain
// surface lives in its own file (users.go, sessions.go, keys.go, sched.go,
// usage.go, compressor.go, settings.go, audit.go).
var _ Store = (*DB)(nil)

// Shared column conversion helpers. Contract 3 conventions: timestamps are
// unix seconds UTC (INTEGER), booleans INTEGER 0/1; nullable columns map to
// Go zero values (zero time.Time, "" strings) and back.

func unixOf(t time.Time) int64 { return t.UTC().Unix() }

// orNow substitutes now for a zero time — used for NOT NULL created_at /
// updated_at columns so callers may leave them unset.
func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// nullUnix maps the zero time to NULL.
func nullUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Unix()
}

// nullStr maps "" to NULL (needed where a CHECK constraint rejects ”,
// e.g. api_keys.role and reservations.bay).
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func timeOf(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

func strOf(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func intOf(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func floatOf(v sql.NullFloat64) float64 {
	if !v.Valid {
		return 0
	}
	return v.Float64
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
