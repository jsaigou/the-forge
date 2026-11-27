// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// Upgrade from 111: the IQ8R_W4 quantization is added once, every existing quantization keeps its id and name, and re-applying is a no-op.
func TestMigration0112AddsTheIQ8RW4Quantization(t *testing.T) {
	sqlDB := openThrough(t, 111)
	defer sqlDB.Close()
	type q struct {
		id   int64
		name string
	}
	read := func() []q {
		rows, err := sqlDB.Query(`SELECT id, name FROM quantizations ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []q
		for rows.Next() {
			var x q
			if err := rows.Scan(&x.id, &x.name); err != nil {
				t.Fatal(err)
			}
			out = append(out, x)
		}
		return out
	}
	before := read()
	body, err := migrationsFS.ReadFile("migrations/0112_iq8r_w4_quantization.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := sqlDB.Exec(string(body)); err != nil {
			t.Fatalf("apply 0112 (round %d): %v", i+1, err)
		}
	}
	after := read()
	if len(after) != len(before)+1 {
		t.Fatalf("quantizations %d -> %d, want exactly one new", len(before), len(after))
	}
	for i, x := range before {
		if after[i] != x {
			t.Errorf("quantization %d changed: %+v -> %+v", x.id, x, after[i])
		}
	}
	if last := after[len(after)-1]; last.name != "IQ8R_W4" {
		t.Errorf("new quantization = %+v", last)
	}
}

func TestFreshDBHasTheIQ8RW4Quantization(t *testing.T) {
	db := openTestDB(t)
	qs, err := db.Catalog().ListQuantizations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range qs {
		if x.Name == "IQ8R_W4" {
			return
		}
	}
	t.Fatalf("IQ8R_W4 not seeded: %v", qs)
}
