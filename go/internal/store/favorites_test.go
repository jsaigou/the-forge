// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

func TestFavoritesAddListRemove(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if err := db.Favorites().Add(ctx, "testuser", "config", 1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := db.Favorites().Add(ctx, "testuser", "config", 2); err != nil {
		t.Fatalf("Add 2nd: %v", err)
	}
	// Double-star is a no-op, not an error.
	if err := db.Favorites().Add(ctx, "testuser", "config", 1); err != nil {
		t.Fatalf("Add duplicate: %v", err)
	}

	list, err := db.Favorites().List(ctx, "testuser", "config")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 favorites, got %d: %+v", len(list), list)
	}

	// Scoped per-user: a different username sees nothing.
	otherList, err := db.Favorites().List(ctx, "other", "config")
	if err != nil {
		t.Fatalf("List other: %v", err)
	}
	if len(otherList) != 0 {
		t.Errorf("expected 0 favorites for a different user, got %d", len(otherList))
	}

	if err := db.Favorites().Remove(ctx, "testuser", "config", 1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	list, err = db.Favorites().List(ctx, "testuser", "config")
	if err != nil {
		t.Fatalf("List after remove: %v", err)
	}
	if len(list) != 1 || list[0].SubjectID != 2 {
		t.Errorf("expected only subject 2 remaining, got %+v", list)
	}

	// Un-starring something not starred is a no-op, not an error.
	if err := db.Favorites().Remove(ctx, "testuser", "config", 999); err != nil {
		t.Fatalf("Remove non-existent: %v", err)
	}
}
