// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
)

// TestMigration0094_BackfillsParallelFlag: llama.cpp configs with no explicit slot count get `--parallel <column>`,
// configs that already pin it (any spelling) and vLLM configs are untouched, and a second run changes nothing.
func TestMigration0094_BackfillsParallelFlag(t *testing.T) {
	sqlDB := openThrough(t, 93)
	defer sqlDB.Close()

	var llama, vllm int64
	if err := sqlDB.QueryRow(`SELECT id FROM engines WHERE name = 'llama.cpp'`).Scan(&llama); err != nil {
		t.Fatalf("llama.cpp engine: %v", err)
	}
	if err := sqlDB.QueryRow(`SELECT id FROM engines WHERE name = 'vLLM'`).Scan(&vllm); err != nil {
		t.Fatalf("vLLM engine: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO models (id, family_id, name) VALUES (9401, NULL, 'm0094')`,
		`INSERT INTO variants (id, model_id, name) VALUES (9401, 9401, 'v0094')`,
		`INSERT INTO artifacts (id, variant_id, format_id, file_path) VALUES (9401, 9401, (SELECT id FROM formats WHERE name = 'GGUF'), '/x0094.gguf')`,
	} {
		if _, err := sqlDB.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	seed := func(name string, engine int64, parallel int, args []string) {
		t.Helper()
		b, _ := json.Marshal(args)
		if _, err := sqlDB.Exec(`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id, parallel, extra_args) VALUES (?, 9401, 9401, ?, ?, ?)`,
			name, engine, parallel, string(b)); err != nil {
			t.Fatalf("seed config %s: %v", name, err)
		}
	}
	seed("no-flag-p1", llama, 1, []string{"--jinja", "--flash-attn", "on"})
	seed("no-flag-p3", llama, 3, []string{"--jinja"})
	seed("empty-args", llama, 1, []string{})
	seed("long-flag", llama, 1, []string{"--parallel", "4", "--jinja"})
	seed("short-flag", llama, 1, []string{"-np", "2"})
	seed("eq-flag", llama, 1, []string{"--parallel=2"})
	seed("vllm-cfg", vllm, 1, []string{"--max-model-len", "4096"})

	apply0094(t, sqlDB)
	got := readArgs(t, sqlDB)
	apply0094(t, sqlDB) // idempotent
	if again := readArgs(t, sqlDB); !reflect.DeepEqual(got, again) {
		t.Fatalf("second run changed configs:\nfirst:  %v\nsecond: %v", got, again)
	}

	want := map[string][]string{
		"no-flag-p1": {"--jinja", "--flash-attn", "on", "--parallel", "1"},
		"no-flag-p3": {"--jinja", "--parallel", "3"},
		"empty-args": {"--parallel", "1"},
		"long-flag":  {"--parallel", "4", "--jinja"},
		"short-flag": {"-np", "2"},
		"eq-flag":    {"--parallel=2"},
		"vllm-cfg":   {"--max-model-len", "4096"},
	}
	for name, w := range want {
		if !reflect.DeepEqual(got[name], w) {
			t.Errorf("%s: extra_args = %v, want %v", name, got[name], w)
		}
	}
}

func readArgs(t *testing.T, sqlDB *sql.DB) map[string][]string {
	t.Helper()
	rows, err := sqlDB.Query(`SELECT name, extra_args FROM configs WHERE variant_id = 9401`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			t.Fatal(err)
		}
		var a []string
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			t.Fatalf("%s: extra_args %q is not a JSON string array: %v", name, raw, err)
		}
		out[name] = a
	}
	return out
}

func apply0094(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	body, err := migrationsFS.ReadFile("migrations/0094_backfill_parallel_flag.sql")
	if err != nil {
		t.Fatalf("read 0094: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0094: %v", err)
	}
}
