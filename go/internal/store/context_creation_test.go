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
	if _, err := db.sql.Exec(`DELETE FROM schema_migrations WHERE version = 90`); err != nil {
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
	if v != 90 {
		t.Fatalf("version=%d", v)
	}
	if raw, err := cp.Settings().Get(context.Background(), "marker"); err != nil || string(raw) != `"keep"` {
		t.Fatalf("existing data lost: %v %s", err, raw)
	}
	if err := cp.ContextCreation().Upsert(context.Background(), []ContextCreationRow{{Hour: 1, Consumer: "c", Model: "m", Requests: 1}}); err != nil {
		t.Fatal(err)
	}
}
