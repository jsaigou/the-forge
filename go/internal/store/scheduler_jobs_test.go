// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSchedulerJobsCRUD exercises the P3 scheduler-jobs surface round-trip
// against the real migration (0066).
func TestSchedulerJobsCRUD(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	jobs := db.SchedulerJobs()

	if _, err := jobs.List(ctx); err != nil {
		t.Fatalf("List on fresh table: %v", err)
	}

	id, err := jobs.Create(ctx, SchedulerJob{
		Name: "nightly-batch", Cron: "0 3 * * *", ConfigName: "qwen3",
		Slot: "a1", Enabled: true, CreatedBy: "testuser", CreatedAt: time.Unix(1756000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Create id = %d, want > 0", id)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "nightly-batch" || got.Cron != "0 3 * * *" || got.ConfigName != "qwen3" ||
		got.Slot != "a1" || !got.Enabled || got.CreatedBy != "testuser" {
		t.Fatalf("Get roundtrip mismatch: %+v", got)
	}
	if !got.LastRunAt.IsZero() || !got.NextRunAt.IsZero() {
		t.Errorf("fresh job run times should be zero (NULL), got last=%v next=%v", got.LastRunAt, got.NextRunAt)
	}

	// Duplicate names are rejected by the UNIQUE constraint.
	if _, err := jobs.Create(ctx, SchedulerJob{Name: "nightly-batch", Cron: "* * * * *", ConfigName: "x"}); err == nil {
		t.Error("duplicate name Create should fail")
	}

	// Update fields; run times untouched.
	got.ConfigName = "gemma"
	got.Slot = ""
	got.Enabled = false
	if err := jobs.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	upd, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if upd.ConfigName != "gemma" || upd.Slot != "" || upd.Enabled {
		t.Fatalf("Update not persisted: %+v", upd)
	}
	if !upd.LastRunAt.IsZero() {
		t.Errorf("Update must not touch run times, last_run_at=%v", upd.LastRunAt)
	}

	// SetRunTimes with sub-minute precision survives the REAL columns.
	last := time.Unix(1756000100, 250_000_000).UTC()
	next := time.Unix(1756086300, 0).UTC()
	if err := jobs.SetRunTimes(ctx, id, last, next); err != nil {
		t.Fatalf("SetRunTimes: %v", err)
	}
	fired, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after SetRunTimes: %v", err)
	}
	if fired.LastRunAt.Unix() != 1756000100 {
		t.Errorf("last_run_at = %v, want unix 1756000100", fired.LastRunAt)
	}
	if !fired.NextRunAt.Equal(next) {
		t.Errorf("next_run_at = %v, want %v", fired.NextRunAt, next)
	}

	if err := jobs.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := jobs.Delete(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
	if _, err := jobs.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get deleted = %v, want ErrNotFound", err)
	}
	if err := jobs.Update(ctx, SchedulerJob{ID: id}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update missing = %v, want ErrNotFound", err)
	}
}
