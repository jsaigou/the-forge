// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
)

// TestModelAliasCRUD covers the model_aliases table added by
// 0086_model_aliases.sql (per-request thinking control, Sprint T3,
// 2026-09-14) — round-trip CRUD plus the safety property the whole feature
// depends on: ON DELETE CASCADE, so deleting a config can never leave an
// alias dangling and silently unroutable.
func TestModelAliasCRUD(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	targetID := seedConfig(t, db, "gemma4-26b-a4b")

	id, err := cat.CreateModelAlias(ctx, ModelAlias{
		Name:     "gemma4-26b-a4b-nothink",
		ConfigID: targetID,
		RequestDefaults: map[string]any{
			"reasoning_effort": "none",
		},
	})
	if err != nil {
		t.Fatalf("CreateModelAlias: %v", err)
	}

	a, err := cat.GetModelAlias(ctx, id)
	if err != nil {
		t.Fatalf("GetModelAlias: %v", err)
	}
	if a.Name != "gemma4-26b-a4b-nothink" || a.ConfigID != targetID || a.Visibility != "visible" {
		t.Errorf("GetModelAlias = %+v", a)
	}
	if a.RequestDefaults["reasoning_effort"] != "none" {
		t.Errorf("RequestDefaults = %+v", a.RequestDefaults)
	}

	if _, err := cat.GetModelAlias(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetModelAlias missing: got %v, want ErrNotFound", err)
	}

	byName, err := cat.ModelAliasByName(ctx, "gemma4-26b-a4b-nothink")
	if err != nil || byName.ID != id {
		t.Errorf("ModelAliasByName = %+v, %v", byName, err)
	}
	if _, err := cat.ModelAliasByName(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ModelAliasByName missing: got %v, want ErrNotFound", err)
	}

	a.Visibility = "hidden"
	a.RequestDefaults = map[string]any{"reasoning_effort": "low"}
	if err := cat.UpdateModelAlias(ctx, a); err != nil {
		t.Fatalf("UpdateModelAlias: %v", err)
	}
	a2, _ := cat.GetModelAlias(ctx, id)
	if a2.Visibility != "hidden" || a2.RequestDefaults["reasoning_effort"] != "low" {
		t.Errorf("after update: %+v", a2)
	}
	if err := cat.UpdateModelAlias(ctx, ModelAlias{ID: 99999, Name: "x", ConfigID: targetID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateModelAlias missing: got %v, want ErrNotFound", err)
	}

	// A second alias pointed at an unrelated config, to prove ListModelAliases
	// returns everything and DeleteConfig below only removes the one whose
	// target was actually deleted.
	otherID := seedConfig(t, db, "qwen38-flash-next")
	if _, err := cat.CreateModelAlias(ctx, ModelAlias{Name: "other-alias", ConfigID: otherID}); err != nil {
		t.Fatalf("CreateModelAlias (other): %v", err)
	}

	list, err := cat.ListModelAliases(ctx)
	if err != nil {
		t.Fatalf("ListModelAliases: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListModelAliases = %+v, want 2", list)
	}

	// ON DELETE CASCADE: deleting the target config must delete its alias
	// too, never leave a dangling config_id — and must not touch the
	// unrelated alias.
	if err := cat.DeleteConfig(ctx, targetID); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
	if _, err := cat.GetModelAlias(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("alias survived its target config's deletion: got %v, want ErrNotFound", err)
	}
	stillThere, err := cat.ModelAliasByName(ctx, "other-alias")
	if err != nil || stillThere.ConfigID != otherID {
		t.Errorf("unrelated alias was affected by an unrelated config delete: %+v, %v", stillThere, err)
	}

	if err := cat.DeleteModelAlias(ctx, stillThere.ID); err != nil {
		t.Fatalf("DeleteModelAlias: %v", err)
	}
	if err := cat.DeleteModelAlias(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteModelAlias missing: got %v, want ErrNotFound", err)
	}
}
