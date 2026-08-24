// SPDX-License-Identifier: Apache-2.0

package registry_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/store"
)

// seedConfigCatalog is a smaller sibling of seedFullCatalog (registry_catalog_test.go)
// that also returns modelID/variantID — needed here to attach benchmarks at
// every scope independently. Deliberately not widening seedFullCatalog's own
// signature: that would ripple into the 3 other test files calling it for
// scenarios that don't care about these ids.
func seedConfigCatalog(t *testing.T, ctx context.Context, cat store.Catalog) (modelID, variantID, configID int64) {
	t.Helper()
	famID, _ := cat.CreateFamily(ctx, store.Family{Name: "Gemma"})
	modelID, _ = cat.CreateModel(ctx, store.Model{
		FamilyID: famID, Name: "Gemma 4 31B (MTP)", Creator: "Google",
	})
	variantID, _ = cat.CreateVariant(ctx, store.Variant{
		ModelID: modelID, Name: "base", TrainedCtx: 262144,
	})
	fmtRow, _ := cat.FormatByName(ctx, "GGUF")
	q, _ := cat.QuantizationByName(ctx, "Q8_0")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: variantID, QuantizationID: q.ID, FormatID: fmtRow.ID,
		FilePath: "gemma4-31b-Q8_0.gguf", ArtifactType: "weight",
		FileSizeBytes: 30 * 1024 * 1024 * 1024,
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	buildID, _ := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "rocm-build", Backend: "rocm",
		BinaryPath: "/opt/llama.cpp/build-rocm/bin/llama-server",
	})
	configID, _ = cat.CreateConfig(ctx, store.Config{
		Name: "gemma4-31b-mtp", VariantID: variantID, WeightArtifactID: weightID,
		EngineID: eng.ID, BuildID: buildID,
		NCtx: 262144, Parallel: 1, ExtraArgs: []string{},
		Status: "verified", Visibility: "visible", IsDefault: true,
	})
	return modelID, variantID, configID
}

func findCapability(caps []registry.Capability, id string) []registry.Capability {
	var out []registry.Capability
	for _, c := range caps {
		if c.ID == id {
			out = append(out, c)
		}
	}
	return out
}

func configCard(t *testing.T, reg registry.Registry, configID int64) registry.ConfigCard {
	t.Helper()
	cards, err := reg.Cards(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	for _, c := range cards {
		if c.ID == configID {
			return c
		}
	}
	t.Fatalf("config %d not found among %d cards", configID, len(cards))
	return registry.ConfigCard{}
}

func modelCardByID(t *testing.T, reg registry.Registry, modelID int64) registry.Card {
	t.Helper()
	cards, err := reg.ModelCards(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	want := strconv.FormatInt(modelID, 10)
	for _, c := range cards {
		if c.ID == want {
			return c
		}
	}
	t.Fatalf("model %d not found among %d cards", modelID, len(cards))
	return registry.Card{}
}

// TestBenchesForConfig_ConfigScopedReachesConfigButNotModelCard is the core
// fix this test file exists for: subject_type="config" benchmarks were
// validated, stored, and listable, but reached no card anywhere (Phase 8,
// pre-release feedback sprint). This proves the union reaches the config
// card and — the leak guard — does NOT reach the model card built from the
// same model/variant.
func TestBenchesForConfig_ConfigScopedReachesConfigButNotModelCard(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	modelID, _, configID := seedConfigCatalog(t, ctx, cat)

	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "custom_capability", Value: "0.91", Source: "self_measured",
		SubjectType: "config", SubjectID: configID, Notes: "in-house eval",
	})

	reg := registry.New(cat, nil, nil)

	cc := configCard(t, reg, configID)
	got := findCapability(cc.Capabilities, "custom_capability")
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 config-scoped capability on the config card, got %d: %+v", len(got), cc.Capabilities)
	}
	if got[0].Score != 0.91 {
		t.Fatalf("score = %v, want 0.91", got[0].Score)
	}

	mc := modelCardByID(t, reg, modelID)
	if leaked := findCapability(mc.Capabilities, "custom_capability"); len(leaked) != 0 {
		t.Fatalf("config-scoped benchmark leaked onto the model card: %+v", leaked)
	}
}

// TestBenchesForConfig_ModelAndVariantBothReachConfigCard is the Sprint D
// regression guard, re-asserted against the widened union: model- and
// variant-scoped benchmarks must both still reach the config card.
func TestBenchesForConfig_ModelAndVariantBothReachConfigCard(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	modelID, variantID, configID := seedConfigCatalog(t, ctx, cat)

	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "knowledge", Value: "0.5", Source: "self_measured",
		SubjectType: "model", SubjectID: modelID, Notes: "GPQA Diamond",
	})
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "reasoning", Value: "0.6", Source: "self_measured",
		SubjectType: "variant", SubjectID: variantID, Notes: "AIME",
	})

	reg := registry.New(cat, nil, nil)
	cc := configCard(t, reg, configID)

	if got := findCapability(cc.Capabilities, "knowledge"); len(got) != 1 {
		t.Fatalf("expected model-scoped capability to reach the config card, capabilities=%+v", cc.Capabilities)
	}
	if got := findCapability(cc.Capabilities, "reasoning"); len(got) != 1 {
		t.Fatalf("expected variant-scoped capability to reach the config card, capabilities=%+v", cc.Capabilities)
	}
}

// TestBenchesForConfig_ConfigScopedMemoryWinsOverVariant is the
// first-wins/last-wins trap: before the dedupe, WeightEstimateBytes
// (first-wins) and performanceFromBenchmarks (last-wins) could read
// different values when both a variant- and a config-scoped
// safe_memory_bytes existed. Both consumers must now agree, and both must
// prefer the more specific (config) scope.
func TestBenchesForConfig_ConfigScopedMemoryWinsOverVariant(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	_, variantID, configID := seedConfigCatalog(t, ctx, cat)

	const variantBytes = int64(30 * 1024 * 1024 * 1024)
	const configBytes = int64(33 * 1024 * 1024 * 1024)
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "safe_memory_bytes", Value: strconv.FormatInt(variantBytes, 10),
		Source: "self_measured", SubjectType: "variant", SubjectID: variantID,
	})
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "safe_memory_bytes", Value: strconv.FormatInt(configBytes, 10),
		Source: "self_measured", SubjectType: "config", SubjectID: configID,
	})

	reg := registry.New(cat, nil, nil)

	gotBytes, ok := reg.WeightEstimateBytes(configID)
	if !ok {
		t.Fatalf("WeightEstimateBytes: not ok")
	}
	if gotBytes != configBytes {
		t.Fatalf("WeightEstimateBytes = %d, want the config-scoped value %d (not the variant-scoped %d)", gotBytes, configBytes, variantBytes)
	}

	cc := configCard(t, reg, configID)
	if cc.Derived.MemoryReqBytes == nil || *cc.Derived.MemoryReqBytes != configBytes {
		t.Fatalf("config card Derived.MemoryReqBytes = %v, want %d", cc.Derived.MemoryReqBytes, configBytes)
	}
}

// TestBenchesForConfig_ConfigScopedDecodeTPSWinsOverVariant mirrors the
// memory test for the other performance metric consumer path
// (performanceFromBenchmarks, historically last-wins).
func TestBenchesForConfig_ConfigScopedDecodeTPSWinsOverVariant(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	_, variantID, configID := seedConfigCatalog(t, ctx, cat)

	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "decode_tps", Value: "18.0", Source: "self_measured",
		SubjectType: "variant", SubjectID: variantID,
	})
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "decode_tps", Value: "24.4", Source: "self_measured",
		SubjectType: "config", SubjectID: configID,
	})

	reg := registry.New(cat, nil, nil)
	cc := configCard(t, reg, configID)

	if cc.Performance.MeasuredTS == nil || *cc.Performance.MeasuredTS != 24.4 {
		t.Fatalf("Performance.MeasuredTS = %v, want the config-scoped 24.4 (not the variant-scoped 18.0)", cc.Performance.MeasuredTS)
	}
}

// TestBenchesForConfig_OfferingScopedNeverReachesCard asserts the
// deliberate gap documented on catalogSnapshot's loadSnapshot switch:
// subject_type="offering" is never indexed, so it reaches no card — even
// under an artificial subject_id collision with the config's own id, which
// is what would expose a keying mistake in benchByConfig.
func TestBenchesForConfig_OfferingScopedNeverReachesCard(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	_, _, configID := seedConfigCatalog(t, ctx, cat)

	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "offering_only_metric", Value: "0.5", Source: "provider_reported",
		SubjectType: "offering", SubjectID: configID, // deliberate id collision
	})

	reg := registry.New(cat, nil, nil)
	cc := configCard(t, reg, configID)

	if got := findCapability(cc.Capabilities, "offering_only_metric"); len(got) != 0 {
		t.Fatalf("offering-scoped benchmark reached the config card: %+v", got)
	}
}

// TestBenchesForConfig_DuplicateMetricNotesCollapseDistinctSurvive checks
// the dedupe key itself: two capability rows sharing (metric, notes) at
// different scopes collapse to the more specific one; two rows sharing
// only the metric (different notes — a real case, e.g. "reasoning" scored
// by both GPQA and AIME) both survive.
func TestBenchesForConfig_DuplicateMetricNotesCollapseDistinctSurvive(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()
	modelID, _, configID := seedConfigCatalog(t, ctx, cat)

	// Same (metric, notes) at model and config scope -> config wins, one row.
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "reasoning", Value: "0.50", Source: "self_measured",
		SubjectType: "model", SubjectID: modelID, Notes: "GPQA Diamond",
	})
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "reasoning", Value: "0.70", Source: "self_measured",
		SubjectType: "config", SubjectID: configID, Notes: "GPQA Diamond",
	})
	// Same metric, different notes -> both survive as distinct rows.
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "reasoning", Value: "0.60", Source: "self_measured",
		SubjectType: "model", SubjectID: modelID, Notes: "AIME",
	})

	reg := registry.New(cat, nil, nil)
	cc := configCard(t, reg, configID)

	reasoning := findCapability(cc.Capabilities, "reasoning")
	if len(reasoning) != 2 {
		t.Fatalf("expected 2 distinct 'reasoning' capability rows (GPQA Diamond, AIME), got %d: %+v", len(reasoning), reasoning)
	}
	byNotes := map[string]float64{}
	for _, c := range reasoning {
		byNotes[c.Benchmark] = c.Score
	}
	if byNotes["GPQA Diamond"] != 0.70 {
		t.Fatalf("GPQA Diamond score = %v, want 0.70 (config-scoped should shadow model-scoped)", byNotes["GPQA Diamond"])
	}
	if byNotes["AIME"] != 0.60 {
		t.Fatalf("AIME score = %v, want 0.60", byNotes["AIME"])
	}
}
