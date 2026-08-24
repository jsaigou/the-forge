// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// TestPrefillStatsAccumulate: repeated observations for the same
// (config, fingerprint) accumulate token-weighted, not overwrite — the whole
// point of a durable rolling aggregate rather than a "latest sample" row.
func TestPrefillStatsAccumulate(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	configID := seedConfig(t, db, "qwen36-mtp")

	if err := db.PrefillStats().AddObservation(ctx, configID, "fp1", 500, 5); err != nil {
		t.Fatalf("AddObservation 1: %v", err)
	}
	if err := db.PrefillStats().AddObservation(ctx, configID, "fp1", 1500, 10); err != nil {
		t.Fatalf("AddObservation 2: %v", err)
	}

	got, err := db.PrefillStats().ByMode(ctx)
	if err != nil {
		t.Fatalf("ByMode: %v", err)
	}
	p, ok := got["qwen36-mtp"]
	if !ok {
		t.Fatalf("ByMode missing qwen36-mtp: %+v", got)
	}
	if p.PromptTokens != 2000 || p.PromptSeconds != 15 || p.Samples != 2 {
		t.Errorf("accumulate wrong: got tokens=%d seconds=%v samples=%d, want 2000/15/2",
			p.PromptTokens, p.PromptSeconds, p.Samples)
	}
	// Token-weighted TPS, not a mean of the two per-call rates (100 and 150):
	// 2000/15 ≈ 133.33, which correctly weights the larger second interval.
	if got := p.TPS(); got < 133.0 || got > 133.4 {
		t.Errorf("TPS() = %v, want ~133.33 (token-weighted, not mean-of-ratios)", got)
	}
}

// TestPrefillStatsFingerprintChangeStartsFreshRow: a config change (new
// fingerprint) must NOT blend into the old regime's accumulated stats — see
// the sprint decision that typical prefill speed is a property of the
// model+config, not a query window, and two different regimes averaged
// together would misrepresent both.
func TestPrefillStatsFingerprintChangeStartsFreshRow(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	configID := seedConfig(t, db, "qwen36-mtp")

	// Old regime: slow (100 tok/s).
	if err := db.PrefillStats().AddObservation(ctx, configID, "fp-old", 1000, 10); err != nil {
		t.Fatalf("AddObservation old: %v", err)
	}
	// Config changes (e.g. n_ctx bump) → new fingerprint. New regime: fast
	// (500 tok/s) — must not be diluted by the old regime's slower figure.
	if err := db.PrefillStats().AddObservation(ctx, configID, "fp-new", 5000, 10); err != nil {
		t.Fatalf("AddObservation new: %v", err)
	}

	got, err := db.PrefillStats().ByMode(ctx)
	if err != nil {
		t.Fatalf("ByMode: %v", err)
	}
	p, ok := got["qwen36-mtp"]
	if !ok {
		t.Fatalf("ByMode missing qwen36-mtp: %+v", got)
	}
	// ByMode must select the current (most-recently-touched) regime only —
	// fp-new's 500 tok/s, not a blend with fp-old's 100 tok/s.
	if p.Fingerprint != "fp-new" {
		t.Errorf("ByMode picked stale fingerprint %q, want fp-new", p.Fingerprint)
	}
	if p.PromptTokens != 5000 || p.PromptSeconds != 10 {
		t.Errorf("ByMode blended regimes: got tokens=%d seconds=%v, want 5000/10 (fp-new only)",
			p.PromptTokens, p.PromptSeconds)
	}
}

// TestPrefillStatsMultipleModes: ByMode returns one current-regime row per
// mode, independent of other modes' accumulation.
func TestPrefillStatsMultipleModes(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	gemmaID := seedConfig(t, db, "gemma4-mtp")
	qwenID := seedConfig(t, db, "qwen36-mtp")

	if err := db.PrefillStats().AddObservation(ctx, gemmaID, "fpA", 2000, 2); err != nil {
		t.Fatalf("AddObservation gemma4-mtp: %v", err)
	}
	if err := db.PrefillStats().AddObservation(ctx, qwenID, "fpB", 800, 10); err != nil {
		t.Fatalf("AddObservation qwen36-mtp: %v", err)
	}

	got, err := db.PrefillStats().ByMode(ctx)
	if err != nil {
		t.Fatalf("ByMode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ByMode = %d modes, want 2: %+v", len(got), got)
	}
	if got["gemma4-mtp"].TPS() != 1000 {
		t.Errorf("gemma4-mtp TPS = %v, want 1000", got["gemma4-mtp"].TPS())
	}
	if got["qwen36-mtp"].TPS() != 80 {
		t.Errorf("qwen36-mtp TPS = %v, want 80", got["qwen36-mtp"].TPS())
	}
}

// TestPrefillStatsRejectsNonPositiveInput: AddObservation must reject
// zero/negative tokens or seconds rather than silently corrupting the
// aggregate (a zero-seconds observation would make TPS() divide by ~0 for
// every reader downstream). The validation happens before any DB write, so
// an arbitrary configID (no real configs row needed) is fine here.
func TestPrefillStatsRejectsNonPositiveInput(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if err := db.PrefillStats().AddObservation(ctx, 1, "fp", 0, 5); err == nil {
		t.Error("AddObservation with tokens=0 should error")
	}
	if err := db.PrefillStats().AddObservation(ctx, 1, "fp", 100, 0); err == nil {
		t.Error("AddObservation with seconds=0 should error")
	}
	if err := db.PrefillStats().AddObservation(ctx, 1, "fp", -5, 5); err == nil {
		t.Error("AddObservation with negative tokens should error")
	}
}

// TestPrefillStatByModeEmpty: no observations yet → empty map, not an error.
func TestPrefillStatByModeEmpty(t *testing.T) {
	db := openTest(t)
	got, err := db.PrefillStats().ByMode(context.Background())
	if err != nil {
		t.Fatalf("ByMode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ByMode = %+v, want empty", got)
	}
}
