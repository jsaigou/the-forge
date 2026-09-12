// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"testing"
)

func apply0079(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	body, err := migrationsFS.ReadFile("migrations/0079_a0_vision_modalities.sql")
	if err != nil {
		t.Fatalf("read 0079: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0079: %v", err)
	}
}

func TestMigration0079_DeepSeekFlashGetsVision(t *testing.T) {
	sqlDB := openThrough(t, 78)
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(`INSERT INTO router_providers (name, api_key, created_at) VALUES ('deepseek', 'sk-x', 0)`); err != nil {
		t.Fatalf("seed router_providers: %v", err)
	}
	var providerID int64
	if err := sqlDB.QueryRow(`SELECT id FROM router_providers WHERE name = 'deepseek'`).Scan(&providerID); err != nil {
		t.Fatalf("resolve provider id: %v", err)
	}

	if _, err := sqlDB.Exec(`INSERT INTO models (family_id, name) VALUES (NULL, 'DeepSeek V4.1 Flash')`); err != nil {
		t.Fatalf("seed models: %v", err)
	}
	var modelID int64
	if err := sqlDB.QueryRow(`SELECT id FROM models WHERE name = 'DeepSeek V4.1 Flash'`).Scan(&modelID); err != nil {
		t.Fatalf("resolve model id: %v", err)
	}

	// A second, unrelated model+offering must be left untouched.
	if _, err := sqlDB.Exec(`INSERT INTO models (family_id, name) VALUES (NULL, 'DeepSeek V4 Pro')`); err != nil {
		t.Fatalf("seed second model: %v", err)
	}
	var otherModelID int64
	if err := sqlDB.QueryRow(`SELECT id FROM models WHERE name = 'DeepSeek V4 Pro'`).Scan(&otherModelID); err != nil {
		t.Fatalf("resolve other model id: %v", err)
	}

	if _, err := sqlDB.Exec(
		`INSERT INTO offerings (model_id, provider_id, wire_model, price_in_per_1m, price_out_per_1m, currency, enabled, priority)
		 VALUES (?, ?, 'deepseek-flash', 0.14, 0.28, 'USD', 1, 100)`,
		modelID, providerID,
	); err != nil {
		t.Fatalf("seed offerings: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO offerings (model_id, provider_id, wire_model, price_in_per_1m, price_out_per_1m, currency, enabled, priority)
		 VALUES (?, ?, 'deepseek-v4-pro', 0.5, 1.5, 'USD', 1, 100)`,
		otherModelID, providerID,
	); err != nil {
		t.Fatalf("seed second offering: %v", err)
	}

	apply0079(t, sqlDB)

	var mods string
	if err := sqlDB.QueryRow(`SELECT modalities FROM models WHERE id = ?`, modelID).Scan(&mods); err != nil {
		t.Fatalf("read model modalities: %v", err)
	}
	if mods != `["text","vision"]` {
		t.Errorf("deepseek-flash model modalities = %q, want [\"text\",\"vision\"]", mods)
	}

	var otherMods string
	if err := sqlDB.QueryRow(`SELECT modalities FROM models WHERE id = ?`, otherModelID).Scan(&otherMods); err != nil {
		t.Fatalf("read other model modalities: %v", err)
	}
	if otherMods != `["text"]` {
		t.Errorf("unrelated deepseek-v4-pro model modalities changed to %q, want unchanged [\"text\"]", otherMods)
	}

	// Idempotence: a second application changes nothing further (row is no
	// longer '["text"]', so the guard no-ops).
	apply0079(t, sqlDB)
	var modsAgain string
	if err := sqlDB.QueryRow(`SELECT modalities FROM models WHERE id = ?`, modelID).Scan(&modsAgain); err != nil {
		t.Fatalf("read model modalities after re-apply: %v", err)
	}
	if modsAgain != `["text","vision"]` {
		t.Errorf("modalities changed on re-apply: %q", modsAgain)
	}
}

func TestMigration0079_Qwen38FlashNextGetsOverride(t *testing.T) {
	sqlDB := openThrough(t, 78)
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("pragma off: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id, extra_args)
		 VALUES ('qwen38-flash-next', 1, 1, 1, '["--no-mmap","--mmproj","/opt/forge/models/qwen3.8-flash-next/mmproj/mmproj-Qwen3.8-Flash-Next-f16.gguf"]')`,
	); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("pragma on: %v", err)
	}

	apply0079(t, sqlDB)

	var mods any
	if err := sqlDB.QueryRow(`SELECT modalities FROM configs WHERE name = 'qwen38-flash-next'`).Scan(&mods); err != nil {
		t.Fatalf("read config modalities: %v", err)
	}
	modsStr, ok := mods.(string)
	if !ok || modsStr != `["text","vision"]` {
		t.Errorf("qwen38-flash-next config modalities = %v, want [\"text\",\"vision\"]", mods)
	}

	// Idempotence.
	apply0079(t, sqlDB)
	var modsAgain string
	if err := sqlDB.QueryRow(`SELECT modalities FROM configs WHERE name = 'qwen38-flash-next'`).Scan(&modsAgain); err != nil {
		t.Fatalf("read config modalities after re-apply: %v", err)
	}
	if modsAgain != `["text","vision"]` {
		t.Errorf("modalities changed on re-apply: %q", modsAgain)
	}
}

func TestMigration0079_NoMMProjFlagIsNoOp(t *testing.T) {
	sqlDB := openThrough(t, 78)
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("pragma off: %v", err)
	}
	// Same name, but no --mmproj in extra_args -- the EXISTS guard must
	// leave this alone (this config genuinely has no wired vision).
	if _, err := sqlDB.Exec(
		`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id, extra_args)
		 VALUES ('qwen38-flash-next', 1, 1, 1, '["--no-mmap","--jinja"]')`,
	); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("pragma on: %v", err)
	}

	apply0079(t, sqlDB)

	var mods any
	if err := sqlDB.QueryRow(`SELECT modalities FROM configs WHERE name = 'qwen38-flash-next'`).Scan(&mods); err != nil {
		t.Fatalf("read config modalities: %v", err)
	}
	if mods != nil {
		t.Errorf("config with no --mmproj flag got modalities = %v, want NULL (no-op)", mods)
	}
}

func TestMigration0079_ExistingOverrideNotClobbered(t *testing.T) {
	sqlDB := openThrough(t, 78)
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("pragma off: %v", err)
	}
	// Has --mmproj AND an operator already set an explicit override (e.g.
	// asserting text-only despite the flag, for whatever reason) -- the IS
	// NULL guard must never clobber that.
	if _, err := sqlDB.Exec(
		`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id, extra_args, modalities)
		 VALUES ('qwen38-flash-next', 1, 1, 1, '["--mmproj","/x.gguf"]', '["text"]')`,
	); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("pragma on: %v", err)
	}

	apply0079(t, sqlDB)

	var mods string
	if err := sqlDB.QueryRow(`SELECT modalities FROM configs WHERE name = 'qwen38-flash-next'`).Scan(&mods); err != nil {
		t.Fatalf("read config modalities: %v", err)
	}
	if mods != `["text"]` {
		t.Errorf("operator override clobbered: modalities = %q, want unchanged [\"text\"]", mods)
	}
}
