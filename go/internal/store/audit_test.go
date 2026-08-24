// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// TestAuditListFiltersByActionPrefixAndTarget guards Sprint C's audit_log
// read path (its first ever — see audit_handlers.go in httpapi). The core
// invariant under test: target IDs collide across entity types (a config
// #7 and a model #7 are both the string "7"), so actionPrefix and target
// must be ANDed together, never target alone.
func TestAuditListFiltersByActionPrefixAndTarget(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	audit := db.Audit()

	base := time.Now().Add(-time.Hour)
	entries := []AuditEntry{
		{TS: base, Actor: "testuser", Action: "catalog_config_update", Target: "7", Detail: "qwen36"},
		{TS: base.Add(time.Minute), Actor: "testuser", Action: "catalog_model_update", Target: "7", Detail: "Qwen3.6-35B"},
		{TS: base.Add(2 * time.Minute), Actor: "testuser", Action: "catalog_config_delete", Target: "9", Detail: "old-config"},
		{TS: base.Add(3 * time.Minute), Actor: "testuser", Action: "headroom_proxy_restart", Target: "local", Detail: ""},
	}
	for _, e := range entries {
		if err := audit.Write(ctx, e); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// target "7" alone must NOT be treated as unique — it should return
	// both the config and the model row sharing that id.
	got, err := audit.List(ctx, "", "7", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(\"\", \"7\", 50) returned %d entries, want 2 (config+model collision)", len(got))
	}

	// actionPrefix + target together must disambiguate to exactly the
	// config row.
	got, err = audit.List(ctx, "catalog_config_", "7", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Action != "catalog_config_update" {
		t.Fatalf("List(\"catalog_config_\", \"7\", 50) = %+v, want exactly the config_update entry", got)
	}

	// actionPrefix alone (no target) should match both config actions
	// (update + delete), most-recent-first.
	got, err = audit.List(ctx, "catalog_config_", "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(\"catalog_config_\", \"\", 50) returned %d entries, want 2", len(got))
	}
	if got[0].Action != "catalog_config_delete" {
		t.Errorf("most-recent-first ordering broken: got[0].Action = %q, want catalog_config_delete", got[0].Action)
	}

	// limit is respected.
	got, err = audit.List(ctx, "", "", 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List with limit=2 returned %d entries, want 2", len(got))
	}

	// A real ID and RemoteAddr/Detail round-trip.
	got, err = audit.List(ctx, "catalog_model_", "7", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 model entry, got %d", len(got))
	}
	if got[0].ID == 0 {
		t.Error("expected a non-zero autoincrement ID")
	}
	if got[0].Detail != "Qwen3.6-35B" {
		t.Errorf("Detail = %q, want %q", got[0].Detail, "Qwen3.6-35B")
	}
}
