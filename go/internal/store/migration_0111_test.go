// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// Upgrade from 110: the Radiance 2.x engine is seeded with its own kind, the 1.17 engine and every build, config and benchmark row are untouched,
// an engine row an operator already named 'Radiance 2.x' keeps its values, and applying the seed twice is a no-op.
func TestMigration0111UpgradeSeedsRadiance2(t *testing.T) {
	sqlDB := openThrough(t, 110)
	defer sqlDB.Close()
	var before int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM engines`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	body, err := migrationsFS.ReadFile("migrations/0111_radiance2_engine.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // twice: the second application changes nothing
		if _, err := sqlDB.Exec(string(body)); err != nil {
			t.Fatalf("apply 0111 (round %d): %v", i+1, err)
		}
	}
	var kind string
	if err := sqlDB.QueryRow(`SELECT kind FROM engines WHERE name = 'Radiance 2.x'`).Scan(&kind); err != nil || kind != "radiance2" {
		t.Errorf("Radiance 2.x kind = %q, %v", kind, err)
	}
	if err := sqlDB.QueryRow(`SELECT kind FROM engines WHERE name = 'Radiance'`).Scan(&kind); err != nil || kind != "radiance" {
		t.Errorf("the 1.17 engine must keep its kind, got %q, %v", kind, err)
	}
	var after int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM engines`).Scan(&after); err != nil || after != before+1 {
		t.Errorf("engines %d -> %d, want exactly one new row (%v)", before, after, err)
	}
}

func TestMigration0111KeepsAnOperatorAddedRow(t *testing.T) {
	sqlDB := openThrough(t, 110)
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(`INSERT INTO engines (name) VALUES ('Radiance 2.x')`); err != nil { // kind defaults to llamacpp
		t.Fatal(err)
	}
	body, _ := migrationsFS.ReadFile("migrations/0111_radiance2_engine.sql")
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := sqlDB.QueryRow(`SELECT kind FROM engines WHERE name = 'Radiance 2.x'`).Scan(&kind); err != nil || kind != "llamacpp" {
		t.Errorf("an operator-added row keeps its values, got kind %q, %v", kind, err)
	}
}

// A fresh database has the engine through the catalog API surface.
func TestFreshDBHasRadiance2Engine(t *testing.T) {
	db := openTestDB(t)
	e, err := db.Catalog().EngineByName(context.Background(), "Radiance 2.x")
	if err != nil || e.Kind != "radiance2" {
		t.Fatalf("engine = %+v, %v", e, err)
	}
}
