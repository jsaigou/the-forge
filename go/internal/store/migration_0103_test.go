// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// TestMigration0103_EngineKindBackfill: the seeded vLLM engine becomes 'vllm', llama.cpp and any engine an operator added stay 'llamacpp',
// and the catalog reads the kind back.
func TestMigration0103_EngineKindBackfill(t *testing.T) {
	sqlDB := openThrough(t, 102)
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(`INSERT INTO engines (name) VALUES ('operator-added')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, err := migrationsFS.ReadFile("migrations/0103_engine_kind.sql")
	if err != nil {
		t.Fatalf("read 0103: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0103: %v", err)
	}
	for name, want := range map[string]string{"llama.cpp": "llamacpp", "vLLM": "vllm", "operator-added": "llamacpp"} {
		var kind string
		if err := sqlDB.QueryRow(`SELECT kind FROM engines WHERE name = ?`, name).Scan(&kind); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if kind != want {
			t.Errorf("engine %q kind = %q, want %q", name, kind, want)
		}
	}
}

func TestCatalog_EnginesCarryKind(t *testing.T) {
	db := openTestDB(t)
	engines, err := db.Catalog().ListEngines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range engines {
		got[e.Name] = e.Kind
	}
	if got["llama.cpp"] != "llamacpp" || got["vLLM"] != "vllm" {
		t.Errorf("engine kinds = %v", got)
	}
	e, err := db.Catalog().EngineByName(context.Background(), "vLLM")
	if err != nil || e.Kind != "vllm" {
		t.Errorf("EngineByName(vLLM) = %+v, %v", e, err)
	}
}
