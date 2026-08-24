// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// TestMigration0043DropsProviderModels seeds a pre-0043 world (a real
// provider + a provider_models row, so the drop is exercised against
// populated data even though the live deployment had none) and confirms:
// the table is gone afterward, the migration doesn't error on non-empty
// data, and unrelated sibling tables (router_providers, offerings' parent
// tables) are untouched.
func TestMigration0043DropsProviderModels(t *testing.T) {
	sqlDB := openThrough(t, 42)
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(
		`INSERT INTO router_providers (name, api_key, created_at) VALUES ('deepseek', 'sk-x', 0)`,
	); err != nil {
		t.Fatalf("seed router_providers: %v", err)
	}
	var providerID int64
	if err := sqlDB.QueryRow(`SELECT id FROM router_providers WHERE name = 'deepseek'`).Scan(&providerID); err != nil {
		t.Fatalf("resolve provider id: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO provider_models (provider_id, model_id, display_name) VALUES (?, 'deepseek-chat', 'DeepSeek Chat')`,
		providerID,
	); err != nil {
		t.Fatalf("seed provider_models: %v", err)
	}
	var preCount int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM provider_models`).Scan(&preCount); err != nil {
		t.Fatalf("pre-migration count: %v", err)
	}
	if preCount != 1 {
		t.Fatalf("pre-migration provider_models rows = %d, want 1", preCount)
	}

	body, err := migrationsFS.ReadFile("migrations/0043_drop_provider_models.sql")
	if err != nil {
		t.Fatalf("read 0043: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0043: %v", err)
	}

	var name string
	err = sqlDB.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='provider_models'`).Scan(&name)
	if err == nil {
		t.Fatal("provider_models table still exists after 0043")
	}

	// The provider row itself (and the table it lives in) must survive —
	// this migration drops exactly one table, nothing else.
	var stillThere string
	if err := sqlDB.QueryRow(`SELECT name FROM router_providers WHERE id = ?`, providerID).Scan(&stillThere); err != nil {
		t.Fatalf("router_providers row lost across 0043: %v", err)
	}
	if stillThere != "deepseek" {
		t.Errorf("router_providers row = %q, want deepseek", stillThere)
	}
}
