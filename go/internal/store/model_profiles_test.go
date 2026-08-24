// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
)

// seedConfig inserts a minimal configs row and returns its id —
// model_profiles/model_prefill_stats are FK'd to configs.id since the 0042
// surrogate-key migration. Foreign keys toggled off just for this insert
// since a fully-seeded families/models/variants/artifacts/engines chain
// isn't otherwise needed by these tests.
func seedConfig(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	if _, err := db.SQL().Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("pragma off: %v", err)
	}
	res, err := db.SQL().Exec(
		`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id) VALUES (?, 1, 1, 1)`, name)
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

func TestModelProfilesSaveGet(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	configID := seedConfig(t, db, "qwen3")

	p := ModelProfile{
		ConfigID: configID, Mode: "qwen3", ModelID: "qwen3-35b", NCtx: 32768, Backend: "vulkan",
		Parallel: 2, SafeMemoryBytes: 24576, PrefillTPS: 1050.5, DecodeTPS: 55.2,
		ActualNCtx: 32768, Fingerprint: "abc123", MeasuredAt: ts(1700000000),
	}
	if err := db.ModelProfiles().Save(ctx, p, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := db.ModelProfiles().Get(ctx, configID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Mode != p.Mode || got.SafeMemoryBytes != p.SafeMemoryBytes || got.DecodeTPS != p.DecodeTPS {
		t.Errorf("round trip mismatch:\n got  %+v\n want %+v", got, p)
	}
	if got.Fingerprint != p.Fingerprint {
		t.Errorf("fingerprint mismatch: got %q want %q", got.Fingerprint, p.Fingerprint)
	}
	if !got.MeasuredAt.Equal(p.MeasuredAt) {
		t.Errorf("measured_at mismatch: got %v want %v", got.MeasuredAt, p.MeasuredAt)
	}
}

// TestModelProfilesBenchmarksRoundTrip covers the depth-sweep child table
// (product/QA sprint, 2026-07-29) and its cascade-on-replace behavior: a
// re-run for the same (config_id, backend, parallel, n_ctx) combo must drop
// the OLD benchmark rows, not accumulate them — SQLite's INSERT OR REPLACE
// deletes the conflicting model_profiles row (a new id, not an in-place
// update), which cascades to the old benchmark rows via their FK.
func TestModelProfilesBenchmarksRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	configID := seedConfig(t, db, "qwen3")

	p := ModelProfile{
		ConfigID: configID, Mode: "qwen3", NCtx: 32768, Backend: "vulkan", Parallel: 1,
		SafeMemoryBytes: 1000, PrefillTPS: 400, DecodeTPS: 40,
		Fingerprint: "v1", MeasuredAt: ts(100),
	}
	benchmarks := []ModelProfileBenchmark{
		{DepthTokens: 0, PP2048TPS: 400, TG128TPS: 40},
		{DepthTokens: 8192, PP2048TPS: 350, TG128TPS: 30},
		{DepthTokens: 16384, PP2048TPS: 300, TG128TPS: 20},
		{DepthTokens: 32000, PP2048TPS: 250, TG128TPS: 10},
	}
	if err := db.ModelProfiles().Save(ctx, p, benchmarks); err != nil {
		t.Fatalf("Save: %v", err)
	}

	stored, err := db.ModelProfiles().Get(ctx, configID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := db.ModelProfiles().Benchmarks(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Benchmarks: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 benchmark rows, got %d", len(got))
	}
	for i, b := range got {
		if b.DepthTokens != benchmarks[i].DepthTokens || b.PP2048TPS != benchmarks[i].PP2048TPS || b.TG128TPS != benchmarks[i].TG128TPS {
			t.Errorf("row %d: got %+v, want %+v", i, b, benchmarks[i])
		}
		if b.DepthTokens != 0 && b.DepthTokens < got[i-1].DepthTokens {
			t.Errorf("rows not ordered by depth_tokens ascending: %+v", got)
		}
	}

	// Re-profiling the same mode must replace, not accumulate.
	p2 := p
	p2.Fingerprint = "v2"
	newBenchmarks := []ModelProfileBenchmark{
		{DepthTokens: 0, PP2048TPS: 500, TG128TPS: 50},
	}
	if err := db.ModelProfiles().Save(ctx, p2, newBenchmarks); err != nil {
		t.Fatalf("Save (re-profile): %v", err)
	}
	stored2, err := db.ModelProfiles().Get(ctx, configID)
	if err != nil {
		t.Fatalf("Get after re-profile: %v", err)
	}
	got2, err := db.ModelProfiles().Benchmarks(ctx, stored2.ID)
	if err != nil {
		t.Fatalf("Benchmarks after re-profile: %v", err)
	}
	if len(got2) != 1 || got2[0].PP2048TPS != 500 {
		t.Errorf("expected old benchmarks replaced with 1 fresh row, got %+v", got2)
	}
	// The OLD profile id's benchmarks must be gone too (cascade), not just
	// unreachable via the new profile id.
	if stored2.ID != stored.ID {
		oldGone, err := db.ModelProfiles().Benchmarks(ctx, stored.ID)
		if err != nil {
			t.Fatalf("Benchmarks(old id): %v", err)
		}
		if len(oldGone) != 0 {
			t.Errorf("expected old profile id's benchmarks cascade-deleted, got %+v", oldGone)
		}
	}
}

func TestModelProfilesUpsert(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	configID := seedConfig(t, db, "qwen3")

	p1 := ModelProfile{ConfigID: configID, Mode: "qwen3", NCtx: 32768, Backend: "vulkan", Parallel: 1,
		SafeMemoryBytes: 20000, DecodeTPS: 50, Fingerprint: "v1", MeasuredAt: ts(100)}
	if err := db.ModelProfiles().Save(ctx, p1, nil); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	p2 := ModelProfile{ConfigID: configID, Mode: "qwen3", NCtx: 32768, Backend: "vulkan", Parallel: 1,
		SafeMemoryBytes: 24576, DecodeTPS: 55, Fingerprint: "v2", MeasuredAt: ts(200)}
	if err := db.ModelProfiles().Save(ctx, p2, nil); err != nil {
		t.Fatalf("Save 2 (upsert): %v", err)
	}

	got, err := db.ModelProfiles().Get(ctx, configID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SafeMemoryBytes != 24576 || got.Fingerprint != "v2" {
		t.Errorf("upsert did not replace: got safe=%d fp=%q, want safe=24576 fp=v2",
			got.SafeMemoryBytes, got.Fingerprint)
	}
}

func TestModelProfilesGetNotFound(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	_, err := db.ModelProfiles().Get(ctx, 999999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: got %v, want ErrNotFound", err)
	}
}

func TestModelProfilesList(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	qwenID := seedConfig(t, db, "qwen3")
	gptOSSID := seedConfig(t, db, "gpt-oss")

	for _, p := range []ModelProfile{
		{ConfigID: qwenID, Mode: "qwen3", NCtx: 32768, Backend: "vulkan", Parallel: 1,
			SafeMemoryBytes: 20000, DecodeTPS: 50, Fingerprint: "a", MeasuredAt: ts(100)},
		{ConfigID: gptOSSID, Mode: "gpt-oss", NCtx: 131072, Backend: "rocm", Parallel: 1,
			SafeMemoryBytes: 95000, DecodeTPS: 35, Fingerprint: "b", MeasuredAt: ts(200)},
	} {
		if err := db.ModelProfiles().Save(ctx, p, nil); err != nil {
			t.Fatalf("Save %s: %v", p.Mode, err)
		}
	}

	list, err := db.ModelProfiles().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List: got %d profiles, want 2", len(list))
	}
	// Ordered by config name ASC → gpt-oss before qwen3
	if list[0].Mode != "gpt-oss" || list[1].Mode != "qwen3" {
		t.Errorf("List order: got %q, %q, want gpt-oss, qwen3", list[0].Mode, list[1].Mode)
	}
}

func TestModelProfilesDelete(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	configID := seedConfig(t, db, "qwen3")

	p := ModelProfile{ConfigID: configID, Mode: "qwen3", NCtx: 32768, Backend: "vulkan", Parallel: 1,
		SafeMemoryBytes: 20000, DecodeTPS: 50, Fingerprint: "a", MeasuredAt: ts(100)}
	if err := db.ModelProfiles().Save(ctx, p, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := db.ModelProfiles().Delete(ctx, configID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := db.ModelProfiles().Get(ctx, configID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete: got %v, want ErrNotFound", err)
	}
}
