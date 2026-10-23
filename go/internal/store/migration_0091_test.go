// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"testing"
)

// TestMigration0091_RegistersKintsugiRebaseBuilds: the two new build rows
// are derived from the existing kintsugi rows, never overwrite anything,
// are idempotent, and do nothing on a DB without those rows.
func TestMigration0091_RegistersKintsugiRebaseBuilds(t *testing.T) {
	sqlDB := openThrough(t, 90)
	defer sqlDB.Close()

	apply0091(t, sqlDB)
	var n int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM builds WHERE name LIKE 'llama.cpp-kintsugi-rebase/%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no kintsugi source rows: want 0 inserted, got %d", n)
	}

	engineID := int64(0)
	if err := sqlDB.QueryRow(`SELECT id FROM engines LIMIT 1`).Scan(&engineID); err != nil {
		if _, e := sqlDB.Exec(`INSERT INTO engines (name) VALUES ('llama.cpp')`); e != nil {
			t.Skipf("cannot seed an engine: %v", e)
		}
		_ = sqlDB.QueryRow(`SELECT id FROM engines LIMIT 1`).Scan(&engineID)
	}
	for _, r := range [][3]string{
		{"llama.cpp-kintsugi/build-vulkan-new", "/x/vk", "vulkan"},
		{"llama.cpp-kintsugi/build-rocm-new", "/x/rocm", "rocm"},
	} {
		if _, err := sqlDB.Exec(`INSERT INTO builds (engine_id, name, binary_path, backend, reason) VALUES (?, ?, ?, ?, 'seed')`,
			engineID, r[0], r[1], r[2]); err != nil {
			t.Fatalf("seed build %s: %v", r[0], err)
		}
	}

	apply0091(t, sqlDB)
	apply0091(t, sqlDB) // idempotent

	rows, err := sqlDB.Query(`SELECT name, backend, engine_id FROM builds WHERE name LIKE 'llama.cpp-kintsugi-rebase/%' ORDER BY backend`)
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
		got = append(got, backend+":"+name)
	}
	want := []string{"rocm:llama.cpp-kintsugi-rebase/build-rocm-rebase", "vulkan:llama.cpp-kintsugi-rebase/build-vulkan-rebase"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func apply0091(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	body, err := migrationsFS.ReadFile("migrations/0091_kintsugi_rebase_builds.sql")
	if err != nil {
		t.Fatalf("read 0091: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0091: %v", err)
	}
}
