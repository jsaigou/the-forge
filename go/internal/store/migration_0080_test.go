// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"testing"
)

// TestMigration0080_RestoresRegressedMTPVision covers the real gap found
// live while verifying 0079's deploy: two of 0028's three named models
// ('Gemma 4 26B A4B (MTP)', 'Qwen3.6 35B MTP') had silently regressed back
// to modalities = '[]' by some later catalog edit; the third ('Qwen3.6 35B
// (Aggressive)') had not. 0080 restores the first two and must never touch
// an unrelated model, a model already correct, or a model that's genuinely
// been set to '[]' deliberately by an operator after this migration ships.
func TestMigration0080_RestoresRegressedMTPVision(t *testing.T) {
	sqlDB := openThrough(t, 79)
	defer sqlDB.Close()

	seed := func(name, modalities string) int64 {
		res, err := sqlDB.Exec(`INSERT INTO models (family_id, name, modalities) VALUES (NULL, ?, ?)`, name, modalities)
		if err != nil {
			t.Fatalf("seed model %q: %v", name, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	regressedGemma := seed("Gemma 4 26B A4B (MTP)", `[]`)
	regressedQwenMTP := seed("Qwen3.6 35B MTP", `[]`)
	alreadyCorrect := seed("Qwen3.6 35B (Aggressive)", `["text","vision"]`)
	unrelated := seed("Some Other Model", `[]`)

	apply0080(t, sqlDB)

	assertModalities := func(id int64, want string) {
		t.Helper()
		var got string
		if err := sqlDB.QueryRow(`SELECT modalities FROM models WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read model %d: %v", id, err)
		}
		if got != want {
			t.Errorf("model %d modalities = %q, want %q", id, got, want)
		}
	}

	assertModalities(regressedGemma, `["text","vision"]`)
	assertModalities(regressedQwenMTP, `["text","vision"]`)
	assertModalities(alreadyCorrect, `["text","vision"]`) // untouched, already correct
	assertModalities(unrelated, `[]`)                     // untouched, not a named target

	// Idempotence.
	apply0080(t, sqlDB)
	assertModalities(regressedGemma, `["text","vision"]`)
	assertModalities(regressedQwenMTP, `["text","vision"]`)
}

func apply0080(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	body, err := migrationsFS.ReadFile("migrations/0080_restore_mtp_vision_modalities.sql")
	if err != nil {
		t.Fatalf("read 0080: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0080: %v", err)
	}
}
