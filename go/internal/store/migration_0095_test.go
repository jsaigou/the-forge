// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// TestMigration0095_BackfillsRetiredFromTheNamePrefix: rows already retired by the old convention get retired=1,
// every other row (including one whose name merely contains the word) stays 0, and the live-shaped rows round-trip
// through the store.
func TestMigration0095_BackfillsRetiredFromTheNamePrefix(t *testing.T) {
	sqlDB := openThrough(t, 94)
	defer sqlDB.Close()

	var llama int64
	if err := sqlDB.QueryRow(`SELECT id FROM engines WHERE name = 'llama.cpp'`).Scan(&llama); err != nil {
		t.Fatal(err)
	}
	seed := func(name, path, backend string) {
		t.Helper()
		if _, err := sqlDB.Exec(`INSERT INTO builds (engine_id, name, binary_path, backend, reason) VALUES (?, ?, ?, ?, '')`,
			llama, name, path, backend); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	seed("(retired) standard-vulkan", "", "vulkan")
	seed("(retired) with-path", "/x/old", "rocm")
	seed("live-rocm", "/opt/forge/x/bin/llama-server", "rocm")
	seed("not (retired) in front", "/opt/forge/y/bin/llama-server", "vulkan")
	seed("retired-looking", "/opt/forge/z/bin/llama-server", "rocm")

	body, err := migrationsFS.ReadFile("migrations/0095_builds_retired.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0095: %v", err)
	}
	// The data statement alone is safe to re-run.
	if _, err := sqlDB.Exec(`UPDATE builds SET retired = 1 WHERE name LIKE '(retired) %'`); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"(retired) standard-vulkan": true, "(retired) with-path": true,
		"live-rocm": false, "not (retired) in front": false, "retired-looking": false,
	}
	for name, w := range want {
		var got bool
		if err := sqlDB.QueryRow(`SELECT retired FROM builds WHERE name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != w {
			t.Errorf("%s: retired = %v, want %v", name, got, w)
		}
	}
}
