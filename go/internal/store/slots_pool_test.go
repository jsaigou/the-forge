// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// ADR-0022: a slot belongs to a pool; every existing row and every write that does not say otherwise is the LLM pool.
func TestSlotPool_DefaultsAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := db.Catalog()

	llmID, err := cat.CreateSlot(ctx, Slot{Name: "a1", Unit: "forge-a1", Port: 8080, Label: "A1"})
	if err != nil {
		t.Fatal(err)
	}
	specID, err := cat.CreateSlot(ctx, Slot{Name: "s1", Unit: "forge-pool@s1", Port: 8100, Label: "S1", Pool: PoolSpecialist})
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := cat.GetSlot(ctx, llmID); g.Pool != PoolLLM {
		t.Errorf("slot written with no pool reads back %q, want %q", g.Pool, PoolLLM)
	}
	if g, _ := cat.SlotByName(ctx, "s1"); g.Pool != PoolSpecialist || g.ID != specID {
		t.Errorf("SlotByName(s1) = %+v, want pool %q", g, PoolSpecialist)
	}
	all, err := cat.ListSlots(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListSlots = %v, %v", all, err)
	}

	// An update that does not mention the pool must not silently move a specialist slot into the LLM pool's code path
	// unnoticed: it is written as the LLM pool, so callers must carry the pool through (the full-replace rule).
	g, _ := cat.GetSlot(ctx, specID)
	g.Label = "S1x"
	if err := cat.UpdateSlot(ctx, g); err != nil {
		t.Fatal(err)
	}
	if g2, _ := cat.GetSlot(ctx, specID); g2.Pool != PoolSpecialist || g2.Label != "S1x" {
		t.Errorf("update that carried the pool lost it: %+v", g2)
	}

	if _, err := cat.CreateSlot(ctx, Slot{Name: "x1", Unit: "u", Port: 1, Label: "X", Pool: "gpu"}); err == nil {
		t.Error("an unknown pool was accepted")
	}
	g.Pool = "gpu"
	if err := cat.UpdateSlot(ctx, g); err == nil {
		t.Error("an update to an unknown pool was accepted")
	}
}
