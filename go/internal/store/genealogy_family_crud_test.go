// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
)

// TestGenealogyCRUD covers the plain (id, name) vocabulary CRUD added
// alongside 0017_genealogies.sql (product/QA sprint, 2026-07-29) — mirrors
// the existing Family CRUD test pattern.
func TestGenealogyCRUD(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	id, err := cat.CreateGenealogy(ctx, Genealogy{Name: "Nemotron"})
	if err != nil {
		t.Fatalf("CreateGenealogy: %v", err)
	}
	// Duplicate name: INSERT OR IGNORE returns the existing ID.
	id2, err := cat.CreateGenealogy(ctx, Genealogy{Name: "Nemotron"})
	if err != nil {
		t.Fatalf("CreateGenealogy duplicate: %v", err)
	}
	if id2 != id {
		t.Errorf("duplicate genealogy ID: got %d, want %d", id2, id)
	}

	g, err := cat.GetGenealogy(ctx, id)
	if err != nil {
		t.Fatalf("GetGenealogy: %v", err)
	}
	if g.Name != "Nemotron" {
		t.Errorf("GetGenealogy name = %q", g.Name)
	}

	if _, err := cat.GetGenealogy(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetGenealogy missing: got %v, want ErrNotFound", err)
	}

	if err := cat.UpdateGenealogy(ctx, Genealogy{ID: id, Name: "Nemotron Family"}); err != nil {
		t.Fatalf("UpdateGenealogy: %v", err)
	}
	g, _ = cat.GetGenealogy(ctx, id)
	if g.Name != "Nemotron Family" {
		t.Errorf("after update, name = %q", g.Name)
	}
	if err := cat.UpdateGenealogy(ctx, Genealogy{ID: 99999, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateGenealogy missing: got %v, want ErrNotFound", err)
	}

	list, err := cat.ListGenealogies(ctx)
	if err != nil {
		t.Fatalf("ListGenealogies: %v", err)
	}
	if len(list) != 1 || list[0].Name != "Nemotron Family" {
		t.Errorf("ListGenealogies: %+v", list)
	}

	// A family referencing this genealogy must fall back to NULL (not be
	// deleted) when the genealogy is deleted — ON DELETE SET NULL.
	famID, err := cat.CreateFamily(ctx, Family{Name: "Nemotron 3", GenealogyID: id})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}
	if err := cat.DeleteGenealogy(ctx, id); err != nil {
		t.Fatalf("DeleteGenealogy: %v", err)
	}
	fam, err := cat.GetFamily(ctx, famID)
	if err != nil {
		t.Fatalf("GetFamily after genealogy delete: %v", err)
	}
	if fam.GenealogyID != 0 {
		t.Errorf("family.GenealogyID = %d after its genealogy was deleted, want 0 (NULL)", fam.GenealogyID)
	}

	if err := cat.DeleteGenealogy(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteGenealogy missing: got %v, want ErrNotFound", err)
	}
}

// TestFamilyCRUDExtended covers Get/Update/Delete, which the family surface
// didn't have at all before this sprint (list+create only).
func TestFamilyCRUDExtended(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	genID, err := cat.CreateGenealogy(ctx, Genealogy{Name: "Qwen"})
	if err != nil {
		t.Fatalf("CreateGenealogy: %v", err)
	}
	famID, err := cat.CreateFamily(ctx, Family{Name: "Qwen 3", GenealogyID: genID})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}

	f, err := cat.GetFamily(ctx, famID)
	if err != nil {
		t.Fatalf("GetFamily: %v", err)
	}
	if f.Name != "Qwen 3" || f.GenealogyID != genID {
		t.Errorf("GetFamily = %+v", f)
	}
	if _, err := cat.GetFamily(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFamily missing: got %v, want ErrNotFound", err)
	}

	if err := cat.UpdateFamily(ctx, Family{ID: famID, Name: "Qwen 3.5", GenealogyID: genID}); err != nil {
		t.Fatalf("UpdateFamily: %v", err)
	}
	f, _ = cat.GetFamily(ctx, famID)
	if f.Name != "Qwen 3.5" {
		t.Errorf("after update, name = %q", f.Name)
	}
	if err := cat.UpdateFamily(ctx, Family{ID: 99999, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateFamily missing: got %v, want ErrNotFound", err)
	}

	// A model referencing this family must fall back to NULL (not be
	// deleted) when the family is deleted — ON DELETE SET NULL.
	mdlID, err := cat.CreateModel(ctx, Model{Name: "Qwen3.5 Test", FamilyID: famID})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := cat.DeleteFamily(ctx, famID); err != nil {
		t.Fatalf("DeleteFamily: %v", err)
	}
	m, err := cat.GetModel(ctx, mdlID)
	if err != nil {
		t.Fatalf("GetModel after family delete: %v", err)
	}
	if m.FamilyID != 0 {
		t.Errorf("model.FamilyID = %d after its family was deleted, want 0 (NULL)", m.FamilyID)
	}

	if err := cat.DeleteFamily(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteFamily missing: got %v, want ErrNotFound", err)
	}
}
