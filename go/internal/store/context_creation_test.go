// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestContextCreationUpsertMergesAndPrunes(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	s := db.ContextCreation()
	a := ContextCreationRow{Hour: 3600, Consumer: "c", Model: "m", Tool: "t", Requests: 1, ToolResults: 2,
		ToolOutputChars: 10, MaxResult: 7, ResultHist: Hist{5: 1, 9: 1}}
	b := ContextCreationRow{Hour: 3600, Consumer: "c", Model: "m", Tool: "t", Requests: 1, ToolResults: 1,
		ToolOutputChars: 3, MaxResult: 9, ResultHist: Hist{5: 2}}
	old := ContextCreationRow{Hour: 0, Consumer: "c", Model: "m", Requests: 1}
	if err := s.Upsert(ctx, []ContextCreationRow{a, old}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ctx, []ContextCreationRow{b}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Since(ctx, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	var got ContextCreationRow
	for _, r := range rows {
		if r.Tool == "t" {
			got = r
		}
	}
	if got.Requests != 2 || got.ToolResults != 3 || got.ToolOutputChars != 13 || got.MaxResult != 9 ||
		got.ResultHist[5] != 3 || got.ResultHist[9] != 1 {
		t.Fatalf("merge wrong: %+v", got)
	}
	if n, err := s.Prune(ctx, 1000); err != nil || n != 1 {
		t.Fatalf("prune n=%d err=%v", n, err)
	}
}

// Migration 0090 dry-run on a COPY (incl. -wal/-shm siblings): a v89-shaped DB
// must upgrade to v90 additively, keeping existing data.
func TestMigration0090DryRunOnCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	db, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Settings().Set(context.Background(), "marker", []byte(`"keep"`)); err != nil {
		t.Fatal(err)
	}
	// Rewind the source to v89 shape (drop the new table, forget version 90).
	if _, err := db.sql.Exec(`DROP TABLE context_creation_hourly`); err != nil {
		t.Fatal(err)
	}
	// 0095 added a column and 0096 dropped one, which (unlike the data-only migrations in between) cannot be applied twice.
	if _, err := db.sql.Exec(`ALTER TABLE builds DROP COLUMN retired`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`ALTER TABLE configs ADD COLUMN parallel INTEGER NOT NULL DEFAULT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`ALTER TABLE api_keys DROP COLUMN scope`); err != nil {
		t.Fatal(err)
	}
	// 0101 added specialist_occupants.stopped, which cannot be applied twice either.
	if _, err := db.sql.Exec(`ALTER TABLE specialist_occupants DROP COLUMN stopped`); err != nil {
		t.Fatal(err)
	}
	// 0100 added two tables (CREATE IF NOT EXISTS, safe to re-run); 0099 added slots.pool, which also cannot be applied twice.
	if _, err := db.sql.Exec(`ALTER TABLE slots DROP COLUMN pool`); err != nil {
		t.Fatal(err)
	}
	// 0098 added the output_modalities columns, which also cannot be applied twice.
	if _, err := db.sql.Exec(`ALTER TABLE models DROP COLUMN output_modalities`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`ALTER TABLE configs DROP COLUMN output_modalities`); err != nil {
		t.Fatal(err)
	}
	// 0103 added engines.kind, which also cannot be applied twice.
	if _, err := db.sql.Exec(`ALTER TABLE engines DROP COLUMN kind`); err != nil {
		t.Fatal(err)
	}
	// 0104 created slot_launches with IF NOT EXISTS, so re-applying it needs no undo.
	// 0105 added five usage_events columns and a partial index.
	if _, err := db.sql.Exec(`DROP INDEX idx_usage_events_started_ms`); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"started_ms", "duration_ms", "outcome", "consumer", "wait_ms"} {
		if _, err := db.sql.Exec(`ALTER TABLE usage_events DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	// 0106 added builds.launch_env and 0107 benchmarks.tuple_json (the seeded engine/format rows are INSERT OR IGNORE).
	if _, err := db.sql.Exec(`ALTER TABLE builds DROP COLUMN launch_env`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`ALTER TABLE benchmarks DROP COLUMN tuple_json`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM schema_migrations WHERE version >= 90`); err != nil {
		t.Fatal(err)
	}
	// Copy the live files while still open (so -wal/-shm exist), then upgrade the copy.
	dst := filepath.Join(dir, "copy.db")
	for _, suf := range []string{"", "-wal", "-shm"} {
		in, err := os.Open(src + suf)
		if err != nil {
			continue
		}
		out, _ := os.Create(dst + suf)
		io.Copy(out, in)
		in.Close()
		out.Close()
	}
	db.Close()
	cp, err := Open(dst)
	if err != nil {
		t.Fatalf("open copy (applies 0090): %v", err)
	}
	defer cp.Close()
	var v int
	cp.sql.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if v != 113 {
		t.Fatalf("version=%d, want 113", v)
	}
	if raw, err := cp.Settings().Get(context.Background(), "marker"); err != nil || string(raw) != `"keep"` {
		t.Fatalf("existing data lost: %v %s", err, raw)
	}
	if err := cp.ContextCreation().Upsert(context.Background(), []ContextCreationRow{{Hour: 1, Consumer: "c", Model: "m", Requests: 1}}); err != nil {
		t.Fatal(err)
	}
}
