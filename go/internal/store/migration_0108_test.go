// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// Migration 0108 seeds the FP8 quantization name the first Radiance model needs (the quantizations table is GET-only through the API).
func TestMigration0108SeedsFP8Quantization(t *testing.T) {
	db := openTestDB(t)
	var n int
	if err := db.sql.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM quantizations WHERE name = 'FP8'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("FP8 quantization rows = %d, want 1", n)
	}
	// Idempotent: INSERT OR IGNORE keeps an existing row, so re-applying the statement adds nothing.
	if _, err := db.sql.ExecContext(context.Background(), `INSERT OR IGNORE INTO quantizations (name) VALUES ('FP8')`); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM quantizations WHERE name = 'FP8'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("FP8 quantization rows after re-insert = %d, want 1", n)
	}
}
