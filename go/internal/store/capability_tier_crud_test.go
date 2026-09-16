// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
)

// seedConfigInCapabilityTier is seedConfig's pattern (FK off, placeholder
// variant/weight/engine IDs — a real chain isn't needed for these tests)
// extended to set capability_tier_id/capability_rank at insert time, since those two
// columns' own FK target (capability_tiers) is real and a later UPDATE re-
// specifying the still-placeholder variant/weight/engine IDs would fail
// once foreign_keys is back on.
func seedConfigInCapabilityTier(t *testing.T, db *DB, name string, capabilityTierID int64, capabilityRank int) int64 {
	t.Helper()
	if _, err := db.SQL().Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("pragma off: %v", err)
	}
	res, err := db.SQL().Exec(
		`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id, capability_tier_id, capability_rank)
		 VALUES (?, 1, 1, 1, ?, ?)`, name, capabilityTierID, capabilityRank)
	if err != nil {
		t.Fatalf("seed config %q: %v", name, err)
	}
	if _, err := db.SQL().Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("pragma on: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed config %q: last insert id: %v", name, err)
	}
	return id
}

// TestCapabilityTierCRUD covers the CapabilityTier vocabulary table added alongside
// 0082_perf_classes.sql (capability-tier substitution, Sprint P1, 2026-09-13) —
// mirrors the existing Genealogy CRUD test pattern, including the
// ON DELETE SET NULL check, which is the safety property the whole feature
// depends on (a deleted class must degrade member configs to "no
// substitution", never leave a dangling reference).
func TestCapabilityTierCRUD(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	id, err := cat.CreateCapabilityTier(ctx, CapabilityTier{Name: "flagship-chat", Notes: "large general chat models"})
	if err != nil {
		t.Fatalf("CreateCapabilityTier: %v", err)
	}

	p, err := cat.GetCapabilityTier(ctx, id)
	if err != nil {
		t.Fatalf("GetCapabilityTier: %v", err)
	}
	if p.Name != "flagship-chat" || p.Mode != "" || p.Notes != "large general chat models" {
		t.Errorf("GetCapabilityTier = %+v", p)
	}

	if _, err := cat.GetCapabilityTier(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetCapabilityTier missing: got %v, want ErrNotFound", err)
	}

	if err := cat.UpdateCapabilityTier(ctx, CapabilityTier{ID: id, Name: "flagship-chat", Mode: "prefer_smarter", Notes: "large general chat models"}); err != nil {
		t.Fatalf("UpdateCapabilityTier: %v", err)
	}
	p, _ = cat.GetCapabilityTier(ctx, id)
	if p.Mode != "prefer_smarter" {
		t.Errorf("after update, mode = %q", p.Mode)
	}
	if err := cat.UpdateCapabilityTier(ctx, CapabilityTier{ID: 99999, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateCapabilityTier missing: got %v, want ErrNotFound", err)
	}

	byName, err := cat.CapabilityTierByName(ctx, "flagship-chat")
	if err != nil || byName.ID != id {
		t.Errorf("CapabilityTierByName = %+v, %v", byName, err)
	}
	if _, err := cat.CapabilityTierByName(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CapabilityTierByName missing: got %v, want ErrNotFound", err)
	}

	list, err := cat.ListCapabilityTiers(ctx)
	if err != nil {
		t.Fatalf("ListCapabilityTiers: %v", err)
	}
	if len(list) != 1 || list[0].Name != "flagship-chat" {
		t.Errorf("ListCapabilityTiers: %+v", list)
	}

	// Two member configs at different ranks — ListConfigsForCapabilityTier must
	// return them most-capable-first (lower CapabilityRank first).
	_ = seedConfigInCapabilityTier(t, db, "flagship-big", id, 0)
	smallID := seedConfigInCapabilityTier(t, db, "flagship-small", id, 10)
	// A config outside the class must never show up as a member.
	_ = seedConfig(t, db, "unrelated")

	members, err := cat.ListConfigsForCapabilityTier(ctx, id)
	if err != nil {
		t.Fatalf("ListConfigsForCapabilityTier: %v", err)
	}
	if len(members) != 2 || members[0].Name != "flagship-big" || members[1].Name != "flagship-small" {
		t.Errorf("ListConfigsForCapabilityTier order/membership = %+v", members)
	}

	// Deleting the class must fall every member config back to
	// CapabilityTierID == 0 (NULL), never leave them referencing a dead row and
	// never delete the configs themselves.
	if err := cat.DeleteCapabilityTier(ctx, id); err != nil {
		t.Fatalf("DeleteCapabilityTier: %v", err)
	}
	smallAfter, err := cat.GetConfig(ctx, smallID)
	if err != nil {
		t.Fatalf("GetConfig after capability tier delete: %v", err)
	}
	if smallAfter.CapabilityTierID != 0 {
		t.Errorf("config.CapabilityTierID = %d after its class was deleted, want 0 (NULL)", smallAfter.CapabilityTierID)
	}
	if smallAfter.CapabilityRank != 10 {
		t.Errorf("config.CapabilityRank = %d after its class was deleted, want 10 unchanged (only the class link is nulled)", smallAfter.CapabilityRank)
	}

	if err := cat.DeleteCapabilityTier(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteCapabilityTier missing: got %v, want ErrNotFound", err)
	}
}
