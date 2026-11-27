// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// TestMigration0096_DropsConfigsParallelKeepingTheRows: the column goes, configs survive intact, and the --parallel
// flag migration 0094 copied into extra_args is still there.
func TestMigration0096_DropsConfigsParallelKeepingTheRows(t *testing.T) {
	sqlDB := openThrough(t, 95)
	defer sqlDB.Close()
	var llama int64
	if err := sqlDB.QueryRow(`SELECT id FROM engines WHERE name = 'llama.cpp'`).Scan(&llama); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO models (id, family_id, name) VALUES (9601, NULL, 'm0096')`,
		`INSERT INTO variants (id, model_id, name) VALUES (9601, 9601, 'v0096')`,
		`INSERT INTO artifacts (id, variant_id, format_id, file_path) VALUES (9601, 9601, (SELECT id FROM formats WHERE name = 'GGUF'), '/x0096.gguf')`,
	} {
		if _, err := sqlDB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlDB.Exec(`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id, parallel, extra_args) VALUES ('c0096', 9601, 9601, ?, 3, '["--parallel","3"]')`, llama); err != nil {
		t.Fatal(err)
	}
	body, err := migrationsFS.ReadFile("migrations/0096_drop_configs_parallel.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0096: %v", err)
	}
	var args string
	if err := sqlDB.QueryRow(`SELECT extra_args FROM configs WHERE name = 'c0096'`).Scan(&args); err != nil || args != `["--parallel","3"]` {
		t.Errorf("config after the drop: args=%q err=%v", args, err)
	}
	if _, err := sqlDB.Exec(`SELECT parallel FROM configs`); err == nil {
		t.Error("the parallel column is still there")
	}
}
