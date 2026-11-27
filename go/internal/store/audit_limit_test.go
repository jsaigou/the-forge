// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAuditListClampsLimitAndEscapesPrefix(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	audit := db.Audit()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 250; i++ {
		if err := audit.Write(ctx, AuditEntry{TS: base.Add(time.Duration(i) * time.Second), Actor: "t", Action: "catalog_config_update", Target: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := audit.Write(ctx, AuditEntry{TS: base, Actor: "t", Action: "100%_done", Target: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := audit.List(ctx, "catalog", "", 100000)
	if err != nil || len(got) != 200 {
		t.Fatalf("an oversized limit must clamp to 200, not fall back to 50: len=%d err=%v", len(got), err)
	}
	// "%" and "_" in the prefix are literal: "%" alone must not match everything, "100%_" matches only the one row.
	if got, _ := audit.List(ctx, "%", "", 10); len(got) != 0 {
		t.Errorf(`prefix "%%" matched %d rows, want 0`, len(got))
	}
	if got, _ := audit.List(ctx, "100%_", "", 10); len(got) != 1 {
		t.Errorf(`prefix "100%%_" matched %d rows, want 1`, len(got))
	}
	if got, _ := audit.List(ctx, "catalog_config_updat_", "", 10); len(got) != 0 {
		t.Errorf(`"_" in a prefix acted as a wildcard (%d rows)`, len(got))
	}
}

// A session with no expiry must be refused at the door, not stored as year 1 (an already-expired row every reader treats as unknown).
func TestSessionsCreateRequiresExpiresAt(t *testing.T) {
	db := openTestDB(t)
	err := db.Sessions().Create(t.Context(), Session{ID: "s1", UserID: 1, CSRFToken: "c"})
	if err == nil {
		t.Fatal("Create without ExpiresAt must fail")
	}
}
