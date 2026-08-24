// SPDX-License-Identifier: Apache-2.0

package registry_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── Test helpers ─────────────────────────────────────────────────────────────

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedFullCatalog seeds a family → model → variant → config → artifacts →
// benchmarks. Returns the config ID and the DB for further queries.
func seedFullCatalog(t *testing.T, ctx context.Context, cat store.Catalog) (configID int64) {
	t.Helper()
	famID, _ := cat.CreateFamily(ctx, store.Family{Name: "Gemma"})
	mdlID, _ := cat.CreateModel(ctx, store.Model{
		FamilyID: famID, Name: "Gemma 4 31B (MTP)", Description: "test model",
		Creator: "Google", LicenseName: "Gemma",
		LicenseURL: "https://ai.google.dev/gemma/license",
		HFRepo:     "TrevorJS/gemma-4-31B-it-uncensored-GGUF", Logo: "google",
		KeyFeatures: []string{"Abliterated", "Dense", "Multimodal", "MTP"},
	})
	varID, _ := cat.CreateVariant(ctx, store.Variant{
		ModelID: mdlID, Name: "base", IsAbliterated: true,
		AbliterationQuality: "High", TrainedCtx: 262144,
	})
	fmt, _ := cat.FormatByName(ctx, "GGUF")
	q, _ := cat.QuantizationByName(ctx, "Q8_0")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, QuantizationID: q.ID, FormatID: fmt.ID,
		FilePath: "gemma4-31b-Q8_0.gguf", ArtifactType: "weight",
		FileSizeBytes: 30 * 1024 * 1024 * 1024, // 30 GiB
		GGUFArch:      "gemma3", GGUFTrainedCtx: 262144,
	})
	mmprojID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID, FilePath: "mmproj.gguf",
		IsAuxiliary: true, ArtifactType: "mmproj",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	buildID, _ := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "rocm-build", Backend: "rocm",
		BinaryPath: "/opt/llama.cpp/build-rocm/bin/llama-server",
	})
	configID, _ = cat.CreateConfig(ctx, store.Config{
		Name: "gemma4-31b-mtp", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, BuildID: buildID, MMProjArtifactID: mmprojID,
		NCtx: 262144, Parallel: 2, ExtraArgs: []string{"--no-mmap", "--parallel", "2"},
		Status: "unverified", Visibility: "visible", IsDefault: true,
	})

	// Benchmarks: decode_tps (18.0) + safe_memory_bytes (30.5 GiB).
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "decode_tps", Value: "18.0", Source: "self_measured",
		SubjectType: "variant", SubjectID: varID,
	})
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "safe_memory_bytes", Value: "32749125632", // 30.5 * 1024^3
		Source: "self_measured", SubjectType: "variant", SubjectID: varID,
	})

	return configID
}

// ── Cards (config-scoped, B1) ────────────────────────────────────────────────

func TestCards_EmptyCatalogIsEmptyNotError(t *testing.T) {
	db := openStore(t)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, err := reg.Cards(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("expected empty card list, got %d", len(cards))
	}
}

func TestCards_ConfigScoped_BasicAssembly(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	configID := seedFullCatalog(t, ctx, cat)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, err := reg.Cards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	c := cards[0]

	// Config-scoped fields.
	if c.ID != configID {
		t.Errorf("ID: got %d, want %d", c.ID, configID)
	}
	if c.Name != "gemma4-31b-mtp" {
		t.Errorf("Name: got %q", c.Name)
	}
	if c.NCtx != 262144 {
		t.Errorf("NCtx: got %d", c.NCtx)
	}
	if c.Status != "unverified" {
		t.Errorf("Status: got %q", c.Status)
	}
	if c.Visibility != "visible" {
		t.Errorf("Visibility: got %q", c.Visibility)
	}
	if !c.IsDefault {
		t.Error("should be default")
	}

	// Model identity (denormalized from the model entity).
	if c.ModelName != "Gemma 4 31B (MTP)" {
		t.Errorf("ModelName: got %q", c.ModelName)
	}
	if c.Creator != "Google" {
		t.Errorf("Creator: got %q", c.Creator)
	}
	if c.LicenseName != "Gemma" {
		t.Errorf("LicenseName: got %q", c.LicenseName)
	}
	if c.Family != "Gemma" {
		t.Errorf("Family: got %q", c.Family)
	}
	if c.HFRepo != "TrevorJS/gemma-4-31B-it-uncensored-GGUF" {
		t.Errorf("HFRepo: got %q", c.HFRepo)
	}
	if len(c.KeyFeatures) != 4 {
		t.Errorf("KeyFeatures: got %d, want 4", len(c.KeyFeatures))
	}

	// Badges derived from key_features + is_abliterated (Sprint J2: Dense/
	// Multimodal/MTP are suppressed entirely — no glyph, no text pill — and
	// is_abliterated + the "Abliterated" key_feature both dedupe onto the
	// single "uncensored" badge).
	if len(c.Badges) != 1 {
		t.Fatalf("Badges: want 1 (uncensored), got %+v", c.Badges)
	}
	if c.Badges[0].ID != "uncensored" {
		t.Errorf("first badge: got %q, want uncensored", c.Badges[0].ID)
	}

	// Quality from variant.
	if c.Quality.IsAbliterated == nil || !*c.Quality.IsAbliterated {
		t.Errorf("Quality.IsAbliterated: %+v", c.Quality.IsAbliterated)
	}
	if c.Quality.AbliterationQuality != "High" {
		t.Errorf("Quality.AbliterationQuality: got %q", c.Quality.AbliterationQuality)
	}

	// Performance from benchmarks.
	if c.Performance.MeasuredTS == nil || *c.Performance.MeasuredTS != 18.0 {
		t.Errorf("Performance.MeasuredTS: %+v", c.Performance.MeasuredTS)
	}
	if c.Performance.MemoryReqBytes == nil || *c.Performance.MemoryReqBytes != int64(30.5*1024*1024*1024) {
		t.Errorf("Performance.MemoryReqBytes: %+v", c.Performance.MemoryReqBytes)
	}
	if c.Performance.PowerCostPer1k != 0 {
		t.Errorf("PowerCostPer1k: got %v (deprecated, should be 0)", c.Performance.PowerCostPer1k)
	}

	// PowerEstPer1m computed from decode_tps=18.0 at default power/rate,
	// wall-adjusted (+25W overhead, /0.9 PSU efficiency — cost/savings
	// sprint 2026-07-30): (140+25)/0.9=183.33W instead of the raw 140W.
	wantWallKW := (0.14*1000 + 25) / 0.9 / 1000
	wantPowerEst := wantWallKW * 0.21 * 1e6 / (3600 * 18.0)
	if c.Performance.PowerEstPer1m == nil || !floatsClose(*c.Performance.PowerEstPer1m, wantPowerEst) {
		t.Errorf("Performance.PowerEstPer1m: got %+v, want ~%v", c.Performance.PowerEstPer1m, wantPowerEst)
	}

	// Derived: curated benchmark wins over file size.
	if c.Derived.MemoryReqBytes == nil || *c.Derived.MemoryReqBytes != int64(30.5*1024*1024*1024) {
		t.Errorf("Derived.MemoryReqBytes: %+v", c.Derived.MemoryReqBytes)
	}

	// No usage store wired → history/reliability are nil.
	if c.Derived.History != nil || c.Derived.Reliability != nil {
		t.Errorf("expected nil history/reliability with no store wired: %+v", c.Derived)
	}

	// Capabilities: NOT seeded (published benchmarks need F7 gate). Empty.
	if len(c.Capabilities) != 0 {
		t.Errorf("Capabilities: expected empty (not seeded), got %+v", c.Capabilities)
	}

	// Sprint B: load-recipe fields (extra_args/backend/variant_name).
	if len(c.ExtraArgs) != 3 || c.ExtraArgs[0] != "--no-mmap" || c.ExtraArgs[1] != "--parallel" || c.ExtraArgs[2] != "2" {
		t.Errorf("ExtraArgs: got %+v", c.ExtraArgs)
	}
	if c.Backend != "rocm" {
		t.Errorf("Backend: got %q, want rocm (from the linked Build)", c.Backend)
	}
	if c.VariantName != "base" {
		t.Errorf("VariantName: got %q", c.VariantName)
	}
}

// TestCards_ModelScopedBenchmarkReachesCard is the Sprint D regression test
// for the subject_type trap (docs/v5-prerelease-readiness.md): a benchmark
// row saved with subject_type="model" (the correct scope for a capability
// score — intrinsic to the weights, not the quant/build) must reach the
// config card's Capabilities, unioned alongside the variant-scoped
// performance benchmarks seedFullCatalog already sets up. Before the fix,
// loadSnapshot silently dropped anything that wasn't subject_type="variant".
func TestCards_ModelScopedBenchmarkReachesCard(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	configID := seedFullCatalog(t, ctx, cat)

	cfg, err := cat.GetConfig(ctx, configID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	vt, err := cat.GetVariant(ctx, cfg.VariantID)
	if err != nil {
		t.Fatalf("GetVariant: %v", err)
	}
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "reasoning", Value: "0.843", Source: "published",
		SourceURL:  "https://tech-insider.org/google-gemma-4-open-model-benchmarks-2026",
		SourceDate: "2026-07-23", Notes: "GPQA Diamond",
		SubjectType: "model", SubjectID: vt.ModelID,
	})

	reg := registry.New(cat, nil, nil)
	cards, err := reg.Cards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	c := cards[0]

	if len(c.Capabilities) != 1 {
		t.Fatalf("Capabilities: expected 1 (model-scoped GPQA), got %+v", c.Capabilities)
	}
	cap := c.Capabilities[0]
	if cap.ID != "reasoning" || cap.Score != 0.843 || cap.Benchmark != "GPQA Diamond" {
		t.Errorf("Capabilities[0]: got %+v", cap)
	}

	// The union must not displace the variant-scoped performance benchmarks
	// seedFullCatalog already sets — model capability and variant
	// performance coexist on the same card.
	if c.Performance.MeasuredTS == nil || *c.Performance.MeasuredTS != 18.0 {
		t.Errorf("Performance.MeasuredTS should survive the union: %+v", c.Performance.MeasuredTS)
	}
	if c.Performance.MemoryReqBytes == nil {
		t.Errorf("Performance.MemoryReqBytes should survive the union")
	}
}

// TestModelCards_ModelScopedBenchmarkReachesCard is the ModelCards()
// (model-gallery) counterpart of the test above — same trap, different
// call site (registry.go's ModelCards, not Cards).
func TestModelCards_ModelScopedBenchmarkReachesCard(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	configID := seedFullCatalog(t, ctx, cat)

	cfg, err := cat.GetConfig(ctx, configID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	vt, err := cat.GetVariant(ctx, cfg.VariantID)
	if err != nil {
		t.Fatalf("GetVariant: %v", err)
	}
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "knowledge", Value: "0.190", Source: "published",
		SourceURL: "https://arxiv.org/html/2508.10925v1", SourceDate: "2025-08-05",
		Notes:       "Humanity's Last Exam",
		SubjectType: "model", SubjectID: vt.ModelID,
	})

	reg := registry.New(cat, nil, nil)
	cards, err := reg.ModelCards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	c := cards[0]

	if len(c.Capabilities) != 1 {
		t.Fatalf("Capabilities: expected 1 (model-scoped HLE), got %+v", c.Capabilities)
	}
	if c.Capabilities[0].ID != "knowledge" || c.Capabilities[0].Score != 0.190 {
		t.Errorf("Capabilities[0]: got %+v", c.Capabilities[0])
	}
}

// TestCards_PrefillTPSBenchmark_NotACapability is the regression test for a
// real bug found while building the Compressor local-savings prefill sprint
// (2026-08-06): compressor_summary_handlers.go's catalog fallback step
// explicitly invites operators to create a variant-scoped prefill_tps
// benchmark row, but capabilitiesFromBenchmarks' old two-entry denylist
// (decode_tps, safe_memory_bytes) didn't know about it — so that row used
// to render as a capability literally labelled "Prefill Tps" at a
// four-digit percentage, sorted to the very top of the card (capabilities
// sort by score descending). It must instead surface via
// Performance.PrefillTS, exactly like decode_tps surfaces via MeasuredTS.
func TestCards_PrefillTPSBenchmark_NotACapability(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	configID := seedFullCatalog(t, ctx, cat)

	cfg, err := cat.GetConfig(ctx, configID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if _, err := cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "prefill_tps", Value: "427.0", Source: "self_measured",
		SubjectType: "variant", SubjectID: cfg.VariantID,
	}); err != nil {
		t.Fatalf("CreateBenchmark: %v", err)
	}

	reg := registry.New(cat, nil, nil)
	cards, err := reg.Cards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	c := cards[0]

	for _, cap := range c.Capabilities {
		if cap.ID == "prefill_tps" {
			t.Fatalf("prefill_tps leaked into Capabilities as a fabricated score: %+v", cap)
		}
	}
	if c.Performance.PrefillTS == nil || *c.Performance.PrefillTS != 427.0 {
		t.Errorf("Performance.PrefillTS = %v, want 427.0", c.Performance.PrefillTS)
	}
	// decode_tps (seeded by seedFullCatalog) must still coexist unaffected.
	if c.Performance.MeasuredTS == nil {
		t.Error("Performance.MeasuredTS should still be populated alongside PrefillTS")
	}
}

func TestCards_ModelIDField(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	configID := seedFullCatalog(t, ctx, db.Catalog())
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.Cards(ctx, time.Time{})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if cards[0].ModelID == "" {
		t.Errorf("ModelID should not be empty")
	}
	if cards[0].ModelID != "1" {
		t.Errorf("ModelID: got %q, want '1' (first model)", cards[0].ModelID)
	}
	_ = configID
}

// ── ModelCards (model-scoped) ────────────────────────────────────────────────

func TestModelCards_BasicAssembly(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedFullCatalog(t, ctx, db.Catalog())
	reg := registry.New(db.Catalog(), nil, nil)

	cards, err := reg.ModelCards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	c := cards[0]

	if c.Name != "Gemma 4 31B (MTP)" {
		t.Errorf("Name: got %q", c.Name)
	}
	if c.Creator != "Google" {
		t.Errorf("Creator: got %q", c.Creator)
	}
	if c.Family != "Gemma" {
		t.Errorf("Family: got %q, want %q", c.Family, "Gemma")
	}
	if len(c.Modes) != 1 || c.Modes[0] != "gemma4-31b-mtp" {
		t.Errorf("Modes: got %+v, want [gemma4-31b-mtp]", c.Modes)
	}
	// Sprint J2: Dense/Multimodal/MTP suppressed; is_abliterated + the
	// "Abliterated" key_feature dedupe onto one "uncensored" badge.
	if len(c.Badges) != 1 || c.Badges[0].ID != "uncensored" {
		t.Errorf("Badges: got %+v, want [uncensored]", c.Badges)
	}
	if c.Quality.IsAbliterated == nil || !*c.Quality.IsAbliterated {
		t.Errorf("Quality.IsAbliterated: %+v", c.Quality.IsAbliterated)
	}
	// seedFullCatalog's family has no genealogy — must not default to
	// something non-empty.
	if c.Genealogy != "" {
		t.Errorf("Genealogy: got %q, want \"\" (family has no genealogy)", c.Genealogy)
	}
}

// TestModelCards_Genealogy covers the field added in the product/QA sprint
// (2026-07-29 — genealogies migration): both the with-variant path
// (assembleModelCard) and the no-variant fallback path must populate it
// from the model's family's genealogy.
func TestModelCards_Genealogy(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()

	genID, err := cat.CreateGenealogy(ctx, store.Genealogy{Name: "Nemotron"})
	if err != nil {
		t.Fatalf("CreateGenealogy: %v", err)
	}
	famID, err := cat.CreateFamily(ctx, store.Family{Name: "Nemotron 3", GenealogyID: genID})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}

	// No-variant model — the ModelCards() fallback branch.
	if _, err := cat.CreateModel(ctx, store.Model{FamilyID: famID, Name: "Nemotron Bare"}); err != nil {
		t.Fatalf("CreateModel (no variant): %v", err)
	}

	// With-variant model — the assembleModelCard branch. Mirrors
	// seedFullCatalog's shape minimally (a variant + weight artifact + config
	// is enough to route through assembleModelCard rather than the fallback).
	mdlID, err := cat.CreateModel(ctx, store.Model{FamilyID: famID, Name: "Nemotron Full"})
	if err != nil {
		t.Fatalf("CreateModel (with variant): %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	fm, _ := cat.FormatByName(ctx, "GGUF")
	q, _ := cat.QuantizationByName(ctx, "Q8_0")
	weightID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, QuantizationID: q.ID, FormatID: fm.ID,
		FilePath: "nemotron-full.gguf", ArtifactType: "weight",
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	engines, _ := cat.ListEngines(ctx)
	if _, err := cat.CreateConfig(ctx, store.Config{
		Name: "nemotron-full", VariantID: varID, WeightArtifactID: weightID,
		EngineID: engines[0].ID, NCtx: 4096, Status: "verified", Visibility: "visible",
	}); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	reg := registry.New(cat, nil, nil)
	cards, err := reg.ModelCards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	byName := map[string]string{}
	for _, c := range cards {
		byName[c.Name] = c.Genealogy
	}
	if g := byName["Nemotron Bare"]; g != "Nemotron" {
		t.Errorf("no-variant model Genealogy = %q, want Nemotron", g)
	}
	if g := byName["Nemotron Full"]; g != "Nemotron" {
		t.Errorf("with-variant model Genealogy = %q, want Nemotron", g)
	}
}

// ── Icon inheritance chain (Sprint I — docs/v5-prerelease-readiness.md) ────
// resolveLogo/catalogSnapshot are unexported, so these exercise the chain
// through the public Cards()/ModelCards() API, mirroring
// TestModelCards_Genealogy's minimal-graph shape (variant + weight artifact
// + config, no benchmarks/build required).

// logoChainCatalog seeds one genealogy -> family -> model -> variant ->
// config graph with the given logo at each level ("" to leave unset) and
// returns the config ID. Every rung after model is optional (GenealogyID /
// FamilyID 0 when the level is skipped).
func logoChainCatalog(t *testing.T, ctx context.Context, cat store.Catalog, genLogo, famLogo, mdlLogo, cfgLogo string) int64 {
	t.Helper()
	return logoChainCatalogDark(t, ctx, cat, genLogo, "", famLogo, "", mdlLogo, "", cfgLogo, "")
}

// logoChainCatalogDark is logoChainCatalog with a dark-variant value at each
// level too (Phase 3 icon-variant work), for exercising resolveLogos'
// level-first pairing.
func logoChainCatalogDark(t *testing.T, ctx context.Context, cat store.Catalog,
	genLogo, genLogoDark, famLogo, famLogoDark, mdlLogo, mdlLogoDark, cfgLogo, cfgLogoDark string,
) int64 {
	t.Helper()
	var genID, famID int64
	if genLogo != "" || genLogoDark != "" {
		var err error
		genID, err = cat.CreateGenealogy(ctx, store.Genealogy{Name: "TestGenealogy", Logo: genLogo, LogoDark: genLogoDark})
		if err != nil {
			t.Fatalf("CreateGenealogy: %v", err)
		}
	}
	famID, err := cat.CreateFamily(ctx, store.Family{Name: "TestFamily", GenealogyID: genID, Logo: famLogo, LogoDark: famLogoDark})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}
	mdlID, err := cat.CreateModel(ctx, store.Model{FamilyID: famID, Name: "TestModel", Logo: mdlLogo, LogoDark: mdlLogoDark})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	fm, _ := cat.FormatByName(ctx, "GGUF")
	q, _ := cat.QuantizationByName(ctx, "Q8_0")
	weightID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, QuantizationID: q.ID, FormatID: fm.ID,
		FilePath: "test-model.gguf", ArtifactType: "weight",
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	engines, _ := cat.ListEngines(ctx)
	cfgID, err := cat.CreateConfig(ctx, store.Config{
		Name: "test-config", VariantID: varID, WeightArtifactID: weightID,
		EngineID: engines[0].ID, NCtx: 4096, Status: "verified", Visibility: "visible",
		Logo: cfgLogo, LogoDark: cfgLogoDark,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	return cfgID
}

func TestCards_LogoInheritance(t *testing.T) {
	cases := []struct {
		name                               string
		genLogo, famLogo, mdlLogo, cfgLogo string
		want                               string
	}{
		{"config override wins over everything", "genealogy-logo", "family-logo", "model-logo", "config-logo", "config-logo"},
		{"model wins over family and genealogy", "genealogy-logo", "family-logo", "model-logo", "", "model-logo"},
		{"family wins over genealogy when model unset", "genealogy-logo", "family-logo", "", "", "family-logo"},
		{"genealogy used when family and model unset", "genealogy-logo", "", "", "", "genealogy-logo"},
		{"empty everything falls through to empty", "", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openStore(t)
			ctx := context.Background()
			cat := db.Catalog()
			logoChainCatalog(t, ctx, cat, tc.genLogo, tc.famLogo, tc.mdlLogo, tc.cfgLogo)
			reg := registry.New(cat, nil, nil)

			cards, err := reg.Cards(ctx, time.Time{})
			if err != nil {
				t.Fatalf("Cards: %v", err)
			}
			if len(cards) != 1 {
				t.Fatalf("expected 1 card, got %d", len(cards))
			}
			if got := cards[0].Logo; got != tc.want {
				t.Errorf("ConfigCard.Logo = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestModelCards_LogoInheritance_NoConfigOverride is the invariant the plan
// called out explicitly: a model-scoped card must NOT inherit a config-level
// icon override, since a model can have many configs — only Cards()
// (config-scoped) may apply cfgLogo.
func TestModelCards_LogoInheritance_NoConfigOverride(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	logoChainCatalog(t, ctx, cat, "genealogy-logo", "family-logo", "model-logo", "config-logo")
	reg := registry.New(cat, nil, nil)

	cards, err := reg.ModelCards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if got := cards[0].Logo; got != "model-logo" {
		t.Errorf("Card.Logo = %q, want %q (config override must not leak into a model-scoped card)", got, "model-logo")
	}
}

// TestModelCards_LogoInheritance_NoVariantFallback covers the ModelCards()
// branch for a model with no variants at all (registry.go's separate
// no-config-in-scope code path) — family fallback must still work there.
func TestModelCards_LogoInheritance_NoVariantFallback(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	famID, err := cat.CreateFamily(ctx, store.Family{Name: "BareFamily", Logo: "family-logo"})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}
	if _, err := cat.CreateModel(ctx, store.Model{FamilyID: famID, Name: "BareModel"}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	reg := registry.New(cat, nil, nil)

	cards, err := reg.ModelCards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if got := cards[0].Logo; got != "family-logo" {
		t.Errorf("Card.Logo = %q, want %q", got, "family-logo")
	}
}

// ── Icon dark-theme variant resolution (Phase 3) ────────────────────────────
// resolveLogos resolves level-first: it stops at the first level with EITHER
// a light or dark value and returns that level's pair, so a level's dark
// mark can never pair with a different level's light mark.

func TestCards_LogoDarkInheritance_LevelFirst(t *testing.T) {
	cases := []struct {
		name                                                                   string
		genLogo, genDark, famLogo, famDark, mdlLogo, mdlDark, cfgLogo, cfgDark string
		wantLogo, wantDark                                                     string
	}{
		{
			name:    "config light+dark both win",
			genLogo: "gen", famLogo: "fam", mdlLogo: "mdl",
			cfgLogo: "cfg", cfgDark: "cfg-dark",
			wantLogo: "cfg", wantDark: "cfg-dark",
		},
		{
			name:    "model has only a light mark: dark falls back to that same light mark, not the genealogy's dark",
			genLogo: "gen", genDark: "gen-dark",
			mdlLogo:  "mdl",
			wantLogo: "mdl", wantDark: "mdl",
		},
		{
			name:     "model has only a dark mark: the model level still wins outright, light falls back to it",
			genLogo:  "gen",
			mdlDark:  "mdl-dark",
			wantLogo: "", wantDark: "mdl-dark",
		},
		{
			name:    "family provides both when model unset",
			famLogo: "fam", famDark: "fam-dark",
			wantLogo: "fam", wantDark: "fam-dark",
		},
		{
			name:    "genealogy used when nothing else set",
			genLogo: "gen", genDark: "gen-dark",
			wantLogo: "gen", wantDark: "gen-dark",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openStore(t)
			ctx := context.Background()
			cat := db.Catalog()
			logoChainCatalogDark(t, ctx, cat, tc.genLogo, tc.genDark, tc.famLogo, tc.famDark, tc.mdlLogo, tc.mdlDark, tc.cfgLogo, tc.cfgDark)
			reg := registry.New(cat, nil, nil)

			cards, err := reg.Cards(ctx, time.Time{})
			if err != nil {
				t.Fatalf("Cards: %v", err)
			}
			if len(cards) != 1 {
				t.Fatalf("expected 1 card, got %d", len(cards))
			}
			if got := cards[0].Logo; got != tc.wantLogo {
				t.Errorf("ConfigCard.Logo = %q, want %q", got, tc.wantLogo)
			}
			if got := cards[0].LogoDark; got != tc.wantDark {
				t.Errorf("ConfigCard.LogoDark = %q, want %q", got, tc.wantDark)
			}
		})
	}
}

// ── WeightEstimateBytes (B2: configID-based) ────────────────────────────────

func TestWeightEstimateBytes_ReturnsBenchmark(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	configID := seedFullCatalog(t, ctx, db.Catalog())
	reg := registry.New(db.Catalog(), nil, nil)

	b, ok := reg.WeightEstimateBytes(configID)
	if !ok {
		t.Fatalf("WeightEstimateBytes: ok=false")
	}
	want := int64(30.5 * 1024 * 1024 * 1024)
	if b != want {
		t.Errorf("WeightEstimateBytes: got %d, want %d", b, want)
	}
}

func TestWeightEstimateBytes_NoBenchmark(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()

	// Seed a config with no benchmarks.
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "M"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID,
		FilePath: "test.gguf", ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	configID, _ := cat.CreateConfig(ctx, store.Config{
		Name: "test", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, NCtx: 32768,
	})

	reg := registry.New(db.Catalog(), nil, nil)
	b, ok := reg.WeightEstimateBytes(configID)
	if ok {
		t.Errorf("expected ok=false with no benchmark, got (%d, true)", b)
	}
}

func TestWeightEstimateBytes_UnknownConfig(t *testing.T) {
	db := openStore(t)
	reg := registry.New(db.Catalog(), nil, nil)

	if _, ok := reg.WeightEstimateBytes(99999); ok {
		t.Errorf("expected ok=false for unknown config ID")
	}
}

// ── PowerEstPer1m (B2: configID-based) ─────────────────────────────────────

func TestPowerEstPer1m_Computed(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	configID := seedFullCatalog(t, ctx, db.Catalog())
	reg := registry.New(db.Catalog(), nil, nil)

	v, ok := reg.PowerEstPer1m(configID)
	if !ok {
		t.Fatalf("PowerEstPer1m: ok=false")
	}
	// decode_tps=18.0, default power=0.14kW wall-adjusted via WallWatts
	// (+25W overhead, /0.90 PSU efficiency) to (140+25)/0.9=183.33W, rate=$0.21/kWh.
	// Cost/savings sprint 2026-07-30: powerRate() now returns wall power,
	// not the raw package-power constant, so every card figure is ~28-31%
	// higher than before at the defaults — deliberate, not a regression.
	wallKW := (0.14*1000 + 25) / 0.9 / 1000
	want := wallKW * 0.21 * 1e6 / (3600 * 18.0)
	if !floatsClose(v, want) {
		t.Errorf("PowerEstPer1m: got %v, want ~%v", v, want)
	}
}

func TestPowerEstPer1m_NoTPS(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()

	// Seed a config with no decode_tps benchmark.
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "M"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID,
		FilePath: "test.gguf", ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	configID, _ := cat.CreateConfig(ctx, store.Config{
		Name: "test", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, NCtx: 32768,
	})

	reg := registry.New(db.Catalog(), nil, nil)
	if _, ok := reg.PowerEstPer1m(configID); ok {
		t.Errorf("expected ok=false with no decode_tps benchmark")
	}
}

func TestPowerEstPer1m_RateIsConfigurable(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	configID := seedFullCatalog(t, ctx, db.Catalog())

	// Custom config: 0.36 kW, $1.0/kWh, OverheadW/PSUEfficiency unset (a
	// directly-constructed literal, not run through applyDefaults) — must
	// fall back to config.Default{OverheadW,PSUEfficiency} rather than
	// divide by a zero PSU efficiency. Wall-adjusted: (360+25)/0.9=427.78W.
	cfg := &config.Config{Cost: config.Cost{PowerKW: 0.36, RatePerKWh: 1.0}}
	reg := registry.New(db.Catalog(), func() *config.Config { return cfg }, nil)

	v, ok := reg.PowerEstPer1m(configID)
	if !ok {
		t.Fatalf("PowerEstPer1m: ok=false")
	}
	wallKW := (0.36*1000 + config.DefaultOverheadW) / config.DefaultPSUEfficiency / 1000
	want := wallKW * 1.0 * 1e6 / (3600 * 18.0)
	if !floatsClose(v, want) {
		t.Errorf("custom [cost] config: got %v, want %v", v, want)
	}
}

// ── CostPer1k (deprecated → always 0) ───────────────────────────────────────

func TestCostPer1k_AlwaysZero(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	configID := seedFullCatalog(t, ctx, db.Catalog())
	reg := registry.New(db.Catalog(), nil, nil)

	if v := reg.CostPer1k(configID); v != 0 {
		t.Errorf("CostPer1k: got %v, want 0 (deprecated)", v)
	}
}

// ── Profile-aware pricing (profiling/pricing sprint 2026-08-07) ──────────────

// TestPowerEstPer1m_ProfilePreferred confirms a wired, fresh profile's
// measured decode_tps drives PowerEstPer1m over the curated decode_tps
// benchmark (BE-COST per docs/v5-profiling-benchmarks.md §5).
func TestPowerEstPer1m_ProfilePreferred(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	configID := seedFullCatalog(t, ctx, db.Catalog())

	// Profile reports a measured decode of 30.0 t/s for the seeded mode
	// ("gemma4-31b-mtp"); the curated benchmark is 18.0.
	reg := registry.New(db.Catalog(), nil, nil,
		registry.WithProfileDecodeTPS(func(mode string) (float64, bool) {
			if mode != "gemma4-31b-mtp" {
				return 0, false
			}
			return 30.0, true
		}))

	v, ok := reg.PowerEstPer1m(configID)
	if !ok {
		t.Fatalf("PowerEstPer1m: ok=false")
	}
	wallKW := (0.14*1000 + 25) / 0.9 / 1000
	want := wallKW * 0.21 * 1e6 / (3600 * 30.0)
	if !floatsClose(v, want) {
		t.Errorf("PowerEstPer1m with profile: got %v, want ~%v (profile tps 30 over curated 18)", v, want)
	}
}

// TestPowerEstPer1m_ProfileStaleFallsBack confirms a stale/missing profile
// (ok=false) falls back to the curated decode_tps benchmark — a config edit
// must not silently drop the price.
func TestPowerEstPer1m_ProfileStaleFallsBack(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	configID := seedFullCatalog(t, ctx, db.Catalog())

	reg := registry.New(db.Catalog(), nil, nil,
		registry.WithProfileDecodeTPS(func(mode string) (float64, bool) {
			return 0, false // stale / unwired
		}))

	v, ok := reg.PowerEstPer1m(configID)
	if !ok {
		t.Fatalf("PowerEstPer1m: ok=false")
	}
	wallKW := (0.14*1000 + 25) / 0.9 / 1000
	want := wallKW * 0.21 * 1e6 / (3600 * 18.0)
	if !floatsClose(v, want) {
		t.Errorf("PowerEstPer1m: got %v, want ~%v (curated fallback 18)", v, want)
	}
}

// TestCards_PowerEstFromProfile confirms the card wire uses the profiled
// decode_tps too (not just the configID-scoped lookup), so the gallery and
// the usage cost never disagree about a model's price basis.
func TestCards_PowerEstFromProfile(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedFullCatalog(t, ctx, db.Catalog())

	reg := registry.New(db.Catalog(), nil, nil,
		registry.WithProfileDecodeTPS(func(mode string) (float64, bool) {
			return 30.0, true
		}))

	cards, err := reg.Cards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	got := cards[0].Performance.PowerEstPer1m
	if got == nil {
		t.Fatal("card PowerEstPer1m: nil")
	}
	wallKW := (0.14*1000 + 25) / 0.9 / 1000
	want := wallKW * 0.21 * 1e6 / (3600 * 30.0)
	if !floatsClose(*got, want) {
		t.Errorf("card PowerEstPer1m: got %v, want ~%v (profile tps 30)", *got, want)
	}
}

// ── History and reliability (live-derived from store.Usage) ─────────────────

func TestCards_HistoryAndReliability(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	configID := seedFullCatalog(t, ctx, db.Catalog())

	// Two history entries: one ok, one ctx_reduced.
	db.Usage().RecordHistory(ctx, store.ModeHistoryEntry{
		Mode: "gemma4-31b-mtp", TS: time.Now().Add(-time.Hour),
		TrainedCtx: 262144, ConfiguredCtx: 262144, ActualCtx: 262144,
		LoadTimeS: 10.0, Result: "ok",
	})
	db.Usage().RecordHistory(ctx, store.ModeHistoryEntry{
		Mode: "gemma4-31b-mtp", TS: time.Now(),
		TrainedCtx: 262144, ConfiguredCtx: 262144, ActualCtx: 131072,
		LoadTimeS: 20.0, Result: "ctx_reduced",
	})

	// Reliability events. "kfd_evict" is deliberately included and expected
	// to have NO effect: the read path for it was removed (product/QA
	// sprint, 2026-07-29) since no writer ever emits that kind and never
	// will without dmesg access (out of scope) — see
	// Reliability.KFDEvictions' doc comment. This proves usageByMode
	// ignores it rather than silently double-counting it as something else.
	for _, ev := range []store.UsageEvent{
		{Kind: "load_ok", Model: "gemma4-31b-mtp"},
		{Kind: "load_ok", Model: "gemma4-31b-mtp"},
		{Kind: "load_failed", Model: "gemma4-31b-mtp"},
		{Kind: "inference_hang", Model: "gemma4-31b-mtp"},
		{Kind: "kfd_evict", Model: "gemma4-31b-mtp"},
	} {
		ev.TS = time.Now()
		db.Usage().Record(ctx, ev)
	}

	reg := registry.New(db.Catalog(), nil, db.Usage())
	cards, err := reg.Cards(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	c := cards[0]

	if c.Derived.History == nil {
		t.Fatalf("expected history to be populated")
	}
	h := c.Derived.History
	if h.LastResult == nil || *h.LastResult != "ctx_reduced" {
		t.Errorf("last_result: %+v", h.LastResult)
	}
	if h.CtxReductionRate != 0.5 {
		t.Errorf("ctx_reduction_rate: got %v, want 0.5", h.CtxReductionRate)
	}
	if h.AvgLoadTimeS == nil || *h.AvgLoadTimeS != 15.0 {
		t.Errorf("avg_load_time_s: %+v", h.AvgLoadTimeS)
	}

	if c.Derived.Reliability == nil {
		t.Fatalf("expected reliability to be populated")
	}
	rel := c.Derived.Reliability
	if rel.LoadsOK != 2 || rel.LoadFailures != 1 || rel.InferenceHangs != 1 || rel.KFDEvictions != 0 {
		t.Errorf("reliability: %+v", rel)
	}
	_ = configID
}

func TestCards_ReliabilityNilWithNoActivity(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedFullCatalog(t, ctx, db.Catalog())

	reg := registry.New(db.Catalog(), nil, db.Usage())
	cards, _ := reg.Cards(ctx, time.Now().Add(-24*time.Hour))
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if cards[0].Derived.Reliability != nil {
		t.Errorf("expected nil reliability with no activity")
	}
	if cards[0].Derived.History != nil {
		t.Errorf("expected nil history with no records")
	}
}

// ── File size fallback (no safe_memory_bytes benchmark) ─────────────────────

func TestCards_FileSizeFallback(t *testing.T) {
	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	os.MkdirAll(modelsDir, 0o755)
	// 2 MiB fake weight file.
	weightPath := filepath.Join(modelsDir, "test.gguf")
	os.WriteFile(weightPath, make([]byte, 2*1024*1024), 0o644)

	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()

	// Seed a config with no safe_memory_bytes benchmark.
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "Sized"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID,
		FilePath: "test.gguf", ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	cat.CreateConfig(ctx, store.Config{
		Name: "sized", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, NCtx: 32768,
	})

	cfg := &config.Config{Paths: config.Paths{ModelsDir: modelsDir}}
	reg := registry.New(db.Catalog(), func() *config.Config { return cfg }, nil)

	cards, err := reg.Cards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	c := cards[0]
	if c.Derived.FileSizeBytes == nil {
		t.Fatalf("expected file_size_bytes to be derived")
	}
	if *c.Derived.FileSizeBytes <= 0 {
		t.Errorf("file_size_bytes should be positive: %v", *c.Derived.FileSizeBytes)
	}
	// No benchmark → derived memory_req falls back to file size.
	if c.Derived.MemoryReqBytes == nil {
		t.Errorf("expected derived memory_req_bytes to fall back to file size")
	}
}

// ── Snapshot caching ─────────────────────────────────────────────────────────

func TestCards_SnapshotCache(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	seedFullCatalog(t, ctx, cat)
	reg := registry.New(db.Catalog(), nil, nil)

	// First call builds the cache.
	cards1, _ := reg.Cards(ctx, time.Time{})
	// Second call within TTL uses the cached snapshot.
	cards2, _ := reg.Cards(ctx, time.Time{})
	if len(cards1) != len(cards2) {
		t.Errorf("cache: card count changed: %d → %d", len(cards1), len(cards2))
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func floatsClose(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
