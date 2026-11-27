// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

var wantedPaths = map[string]string{"models_dir": "/var/lib/forge/models", "state_dir": "/var/lib/forge", "icons_dir": "/var/lib/forge/icons"}

func emptyDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSetUpHost_ANewInstallGetsSlotsAndFoldersOnce(t *testing.T) {
	db := emptyDB(t)
	ctx := context.Background()
	done, err := setUpHost(ctx, db, wantedPaths)
	if err != nil || !slices.Equal(done.SlotsAdded, []string{"a1", "a2", "a3", "a4"}) || len(done.PathsSet) != 3 {
		t.Fatalf("first run: %+v %v", done, err)
	}
	// What the daemon reads is what matters.
	cfg, err := config.LoadFromStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Slots) != 4 || cfg.Slots["a1"].Unit != "forge-a1" || cfg.Slots["a1"].Port != 8080 || cfg.Slots["a4"].Port != 8088 || len(cfg.SpecialistSlots) != 0 {
		t.Fatalf("slots: %+v / %+v", cfg.Slots, cfg.SpecialistSlots)
	}
	if cfg.Paths.ModelsDir != "/var/lib/forge/models" || cfg.Paths.StateDir != "/var/lib/forge" {
		t.Fatalf("paths: %+v", cfg.Paths)
	}
	if raw, err := db.Settings().Get(ctx, "catalog.show_new_downloads"); err != nil || string(raw) != "true" {
		t.Fatalf("a new install shows its downloads: %s %v", raw, err)
	}
	done, err = setUpHost(ctx, db, wantedPaths)
	if err != nil || len(done.SlotsAdded) != 0 || len(done.PathsSet) != 0 || done.ShowDownloads {
		t.Fatalf("second run must change nothing: %+v %v", done, err)
	}
	// The operator's choice to hide them again stays.
	if err := db.Settings().Set(ctx, "catalog.show_new_downloads", []byte("false")); err != nil {
		t.Fatal(err)
	}
	if done, err = setUpHost(ctx, db, wantedPaths); err != nil || done.ShowDownloads {
		t.Fatalf("a choice that was made stays: %+v %v", done, err)
	}
}

func TestSetUpHost_KeepsWhatTheHostHas(t *testing.T) {
	db := emptyDB(t)
	ctx := context.Background()
	if _, err := db.Catalog().CreateSlot(ctx, store.Slot{Name: "gpu", Unit: "forge-gpu", Port: 9000, Label: "GPU", SortOrder: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.Settings().Set(ctx, "infra.paths", []byte(`{"models_dir":"/mnt/models","unknown":"kept"}`)); err != nil {
		t.Fatal(err)
	}
	done, err := setUpHost(ctx, db, wantedPaths)
	if err != nil || len(done.SlotsAdded) != 0 || !slices.Equal(done.PathsSet, []string{"state_dir", "icons_dir"}) {
		t.Fatalf("%+v %v", done, err)
	}
	raw, err := db.Settings().Get(ctx, "infra.paths")
	if err != nil || string(raw) != `{"icons_dir":"/var/lib/forge/icons","models_dir":"/mnt/models","state_dir":"/var/lib/forge","unknown":"kept"}` {
		t.Fatalf("stored: %s %v", raw, err)
	}
}

// A pool slot is not a model slot: a host with only speech engines still needs its model slots. A port already taken is said, not overwritten.
func TestSetUpHost_PoolSlotsAndPortClashes(t *testing.T) {
	db := emptyDB(t)
	ctx := context.Background()
	if _, err := db.Catalog().CreateSlot(ctx, store.Slot{Name: "s1", Unit: "forge-pool@s1", Port: 8100, Pool: store.PoolSpecialist}); err != nil {
		t.Fatal(err)
	}
	if done, err := setUpHost(ctx, db, wantedPaths); err != nil || len(done.SlotsAdded) != 4 {
		t.Fatalf("%+v %v", done, err)
	}
	db = emptyDB(t)
	if _, err := db.Catalog().CreateSlot(ctx, store.Slot{Name: "s1", Unit: "forge-pool@s1", Port: 8087, Pool: store.PoolSpecialist}); err != nil {
		t.Fatal(err)
	}
	if _, err := setUpHost(ctx, db, wantedPaths); err == nil {
		t.Fatal("expected the port clash to be reported")
	}
	if slots, err := db.Catalog().ListSlots(ctx); err != nil || len(slots) != 1 {
		t.Fatalf("no model slot may be added when one cannot be: %+v %v", slots, err)
	}
}
