// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

func TestNotificationsUpsertDedupes(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	id1, err := db.Notifications().Upsert(ctx, "INFERENCE_HANG", "crit", "8080",
		"Port 8080 stalled", "INFERENCE_HANG:8080", ts(100))
	if err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	id2, err := db.Notifications().Upsert(ctx, "INFERENCE_HANG", "crit", "8080",
		"Port 8080 stalled (still)", "INFERENCE_HANG:8080", ts(200))
	if err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected same row id for repeated dedupe_key, got %d and %d", id1, id2)
	}

	list, err := db.Notifications().List(ctx, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 deduped row, got %d", len(list))
	}
	n := list[0]
	if n.Occurrences != 2 {
		t.Errorf("occurrences = %d, want 2", n.Occurrences)
	}
	if !n.LastSeen.Equal(ts(200)) {
		t.Errorf("last_seen = %v, want %v", n.LastSeen, ts(200))
	}
	if n.Message != "Port 8080 stalled (still)" {
		t.Errorf("message not updated to latest: %q", n.Message)
	}
}

func TestNotificationsAckDismissList(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	id, err := db.Notifications().Upsert(ctx, "GTT_HIGH", "warn", "",
		"GTT at 95%", "GTT_HIGH:", ts(100))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Dismissed rows are excluded from the default (active-only) list.
	if err := db.Notifications().Dismiss(ctx, id, ts(150)); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	active, err := db.Notifications().List(ctx, false)
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active after dismiss, got %d", len(active))
	}
	all, err := db.Notifications().List(ctx, true)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 1 || all[0].DismissedAt == nil {
		t.Fatalf("expected 1 row with dismissed_at set, got %+v", all)
	}

	// A recurring alert after dismissal un-dismisses (it's a new occurrence
	// worth surfacing again).
	if _, err := db.Notifications().Upsert(ctx, "GTT_HIGH", "warn", "",
		"GTT at 96%", "GTT_HIGH:", ts(300)); err != nil {
		t.Fatalf("re-Upsert after dismiss: %v", err)
	}
	active, err = db.Notifications().List(ctx, false)
	if err != nil {
		t.Fatalf("List active after re-upsert: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected re-upsert to clear dismissed_at, got %d active rows", len(active))
	}

	if err := db.Notifications().Acknowledge(ctx, id, ts(310)); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	all, err = db.Notifications().List(ctx, true)
	if err != nil {
		t.Fatalf("List after ack: %v", err)
	}
	if all[0].AcknowledgedAt == nil {
		t.Fatalf("expected acknowledged_at set")
	}
}

func TestNotificationsAcknowledgeAll(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if _, err := db.Notifications().Upsert(ctx, "UNIT_OOM", "crit", "ai-mode-comfyui",
		"OOM killed", "UNIT_OOM:ai-mode-comfyui", ts(100)); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	if _, err := db.Notifications().Upsert(ctx, "UNIT_CRASH", "crit", "forge-tts",
		"crashed", "UNIT_CRASH:forge-tts", ts(100)); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}

	if err := db.Notifications().AcknowledgeAll(ctx, ts(200)); err != nil {
		t.Fatalf("AcknowledgeAll: %v", err)
	}
	list, err := db.Notifications().List(ctx, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(list))
	}
	for _, n := range list {
		if n.AcknowledgedAt == nil {
			t.Errorf("row %d (%s) not acknowledged", n.ID, n.Code)
		}
	}
}
