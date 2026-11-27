// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"testing"
)

// TestMigration0092_RegistersRebase20261101Builds: the three new build rows
// are derived from the existing rebased ROCm row, never overwrite anything,
// are idempotent, and do nothing on a DB without that row.
func TestMigration0092_RegistersRebase20261101Builds(t *testing.T) {
	sqlDB := openThrough(t, 91)
	defer sqlDB.Close()

	apply0092(t, sqlDB)
	var n int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM builds WHERE name IN (
		'llama.cpp-kintsugi-rebase2/build-rocm-rebase2',
		'llama.cpp-kintsugi-clover2/build-rocm-clover2',
		'llama.cpp-upstream-plain/build-rocm-plain')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no source row: want 0 inserted, got %d", n)
	}

	engineID := int64(0)
	if err := sqlDB.QueryRow(`SELECT id FROM engines LIMIT 1`).Scan(&engineID); err != nil {
		if _, e := sqlDB.Exec(`INSERT INTO engines (name) VALUES ('llama.cpp')`); e != nil {
			t.Skipf("cannot seed an engine: %v", e)
		}
		_ = sqlDB.QueryRow(`SELECT id FROM engines LIMIT 1`).Scan(&engineID)
	}
	if _, err := sqlDB.Exec(`INSERT OR IGNORE INTO builds (engine_id, name, binary_path, backend, reason) VALUES (?, ?, ?, ?, 'seed')`,
		engineID, "llama.cpp-kintsugi-rebase/build-rocm-rebase", "/x/rocm", "rocm"); err != nil {
		t.Fatalf("seed source build: %v", err)
	}

	apply0092(t, sqlDB)
	apply0092(t, sqlDB) // idempotent

	rows, err := sqlDB.Query(`SELECT name, backend, engine_id FROM builds WHERE name IN (
		'llama.cpp-kintsugi-rebase2/build-rocm-rebase2',
		'llama.cpp-kintsugi-clover2/build-rocm-clover2',
		'llama.cpp-upstream-plain/build-rocm-plain') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name, backend string
		var eid int64
		if err := rows.Scan(&name, &backend, &eid); err != nil {
			t.Fatal(err)
		}
		if eid != engineID {
			t.Errorf("%s engine_id = %d, want %d", name, eid, engineID)
		}
		if backend != "rocm" {
			t.Errorf("%s backend = %q, want rocm", name, backend)
		}
		got = append(got, name)
	}
	want := []string{
		"llama.cpp-kintsugi-clover2/build-rocm-clover2",
		"llama.cpp-kintsugi-rebase2/build-rocm-rebase2",
		"llama.cpp-upstream-plain/build-rocm-plain",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func apply0092(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	body, err := migrationsFS.ReadFile("migrations/0092_kintsugi_rebase_20261101_builds.sql")
	if err != nil {
		t.Fatalf("read 0092: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0092: %v", err)
	}
}
