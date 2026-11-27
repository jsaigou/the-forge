// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"testing"
)

// TestMigration0093_RetiresKintsugiBuilds: the listed Kintsugi build rows are retired (prefixed, binary_path
// emptied), a row a config still references is left alone, unlisted builds are untouched, and it is idempotent.
func TestMigration0093_RetiresKintsugiBuilds(t *testing.T) {
	sqlDB := openThrough(t, 92)
	defer sqlDB.Close()

	engineID := int64(0)
	if err := sqlDB.QueryRow(`SELECT id FROM engines LIMIT 1`).Scan(&engineID); err != nil {
		if _, e := sqlDB.Exec(`INSERT INTO engines (name) VALUES ('llama.cpp')`); e != nil {
			t.Skipf("cannot seed an engine: %v", e)
		}
		_ = sqlDB.QueryRow(`SELECT id FROM engines LIMIT 1`).Scan(&engineID)
	}
	seed := func(name, path, backend string) int64 {
		t.Helper()
		if _, err := sqlDB.Exec(`INSERT OR IGNORE INTO builds (engine_id, name, binary_path, backend, reason) VALUES (?, ?, ?, ?, 'seed')`,
			engineID, name, path, backend); err != nil {
			t.Fatalf("seed build %s: %v", name, err)
		}
		var id int64
		if err := sqlDB.QueryRow(`SELECT id FROM builds WHERE name = ?`, name).Scan(&id); err != nil {
			t.Fatalf("read back %s: %v", name, err)
		}
		return id
	}
	seed("llama.cpp-kintsugi/build-rocm-new", "/x/k10", "rocm")
	seed("llama.cpp-kintsugi-rebase/build-rocm-rebase", "/x/k16", "rocm")
	inUse := seed("llama.cpp-kintsugi-rebase2/build-rocm-rebase2", "/x/k17", "rocm")
	keep := seed("llama.cpp-upstream-plain/build-rocm-plain", "/x/plain", "rocm")

	// a config that still uses one of the listed builds must keep it live
	for _, q := range []string{
		`INSERT INTO models (id, family_id, name) VALUES (9001, NULL, 'm0093')`,
		`INSERT INTO variants (id, model_id, name) VALUES (9001, 9001, 'v0093')`,
		`INSERT INTO artifacts (id, variant_id, format_id, file_path) VALUES (9001, 9001, (SELECT id FROM formats WHERE name = 'GGUF'), '/x0093.gguf')`,
	} {
		if _, err := sqlDB.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if _, err := sqlDB.Exec(`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id, build_id) VALUES ('uses-r2', 9001, 9001, ?, ?)`, engineID, inUse); err != nil {
		t.Fatalf("seed config referencing build %d: %v", inUse, err)
	}

	apply0093(t, sqlDB)
	apply0093(t, sqlDB) // idempotent

	check := func(name string, wantRetired bool) {
		t.Helper()
		var n, path string
		var found int
		err := sqlDB.QueryRow(`SELECT name, binary_path FROM builds WHERE name = ? OR name = ?`, name, "(retired) "+name).Scan(&n, &path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = found
		retired := n == "(retired) "+name
		if retired != wantRetired {
			t.Errorf("%s: retired=%v want %v (row name %q)", name, retired, wantRetired, n)
		}
		if retired && path != "" {
			t.Errorf("%s: retired row still has binary_path %q", name, path)
		}
		if !retired && path == "" {
			t.Errorf("%s: live row lost its binary_path", name)
		}
	}
	check("llama.cpp-kintsugi/build-rocm-new", true)
	check("llama.cpp-kintsugi-rebase/build-rocm-rebase", true)
	check("llama.cpp-kintsugi-rebase2/build-rocm-rebase2", false) // referenced by a config
	check("llama.cpp-upstream-plain/build-rocm-plain", false)     // not listed
	_ = keep
}

func apply0093(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	body, err := migrationsFS.ReadFile("migrations/0093_retire_kintsugi_builds.sql")
	if err != nil {
		t.Fatalf("read 0093: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0093: %v", err)
	}
}
