// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
)

// testProviderID looks up a router_providers row's id by name — a test
// helper for the many pre-0042 fixtures that seed the table via raw SQL
// (INSERT INTO router_providers (name, ...)) and then need the surrogate id
// CreateOffering now requires.
func testProviderID(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.SQL().QueryRow(`SELECT id FROM router_providers WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatalf("testProviderID(%q): %v", name, err)
	}
	return id
}

// TestCatalogFullRoundTrip exercises the full create → list chain for every
// catalog entity type, verifying that the FK relationships are wired correctly
// and data round-trips through the store. This is the store-surface DoD test —
// if this passes, the migrate-v4 seed path has a working write/read surface.
func TestCatalogFullRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	// ── Family ──
	famID, err := cat.CreateFamily(ctx, Family{Name: "Gemma"})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}
	if famID == 0 {
		t.Fatal("family ID should be non-zero")
	}
	// Duplicate family name: INSERT OR IGNORE returns existing ID.
	famID2, err := cat.CreateFamily(ctx, Family{Name: "Gemma"})
	if err != nil {
		t.Fatalf("CreateFamily duplicate: %v", err)
	}
	if famID2 != famID {
		t.Errorf("duplicate family ID: got %d, want %d", famID2, famID)
	}

	// ── Model ──
	mdlID, err := cat.CreateModel(ctx, Model{
		FamilyID:     famID,
		Name:         "Gemma 4 31B (MTP)",
		Architecture: "gemma",
		Creator:      "Google",
		LicenseName:  "Gemma",
		LicenseURL:   "https://ai.google.dev/gemma/license",
		Description:  "Gemma 4 31B TrevorJS EGA Q8_0 + MTP head",
		HFRepo:       "TrevorJS/gemma-4-31B-it-uncensored-GGUF",
		Logo:         "google",
		KeyFeatures:  []string{"Abliterated", "Dense", "Multimodal", "MTP"},
	})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}

	// ── Variant ──
	varID, err := cat.CreateVariant(ctx, Variant{
		ModelID:             mdlID,
		Name:                "TrevorJS EGA Q8_0 + MTP",
		DerivationType:      "abliteration",
		TrainedCtx:          262144,
		IsAbliterated:       true,
		AbliterationQuality: "High",
	})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}

	// ── Quantization / Format (static-seeded in migration) ──
	q, err := cat.QuantizationByName(ctx, "Q8_0")
	if err != nil {
		t.Fatalf("QuantizationByName Q8_0: %v", err)
	}
	if q.ID == 0 {
		t.Fatal("quantization Q8_0 ID should be non-zero")
	}
	if _, err := cat.QuantizationByName(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("QuantizationByName nonexistent: got %v, want ErrNotFound", err)
	}

	f, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName GGUF: %v", err)
	}
	if f.ID == 0 {
		t.Fatal("format GGUF ID should be non-zero")
	}

	// ── Artifact (weight) ──
	weightID, err := cat.CreateArtifact(ctx, Artifact{
		VariantID:      varID,
		QuantizationID: q.ID,
		FormatID:       f.ID,
		FilePath:       "gemma4-31b-Q6_K.gguf",
		ArtifactType:   "weight",
		FileSizeBytes:  32768000000,
	})
	if err != nil {
		t.Fatalf("CreateArtifact (weight): %v", err)
	}

	// ── Artifact (mmproj auxiliary) ──
	mmprojID, err := cat.CreateArtifact(ctx, Artifact{
		VariantID:    varID,
		FormatID:     f.ID,
		FilePath:     "gemma4-mmproj/mmproj-31b-F32.gguf",
		IsAuxiliary:  true,
		ArtifactType: "mmproj",
	})
	if err != nil {
		t.Fatalf("CreateArtifact (mmproj): %v", err)
	}

	// ── Compatibility (mmproj ↔ variant) ──
	if err := cat.CreateCompatibility(ctx, Compatibility{
		AuxiliaryArtifactID: mmprojID,
		VariantID:           varID,
	}); err != nil {
		t.Fatalf("CreateCompatibility: %v", err)
	}
	// Duplicate compatibility: INSERT OR IGNORE is a no-op.
	if err := cat.CreateCompatibility(ctx, Compatibility{
		AuxiliaryArtifactID: mmprojID,
		VariantID:           varID,
	}); err != nil {
		t.Errorf("CreateCompatibility duplicate: %v", err)
	}

	// ── Engine / Build ──
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName llama.cpp: %v", err)
	}
	if eng.ID == 0 {
		t.Fatal("engine llama.cpp ID should be non-zero")
	}
	buildID, err := cat.CreateBuild(ctx, Build{
		EngineID:   eng.ID,
		Name:       "vulkan-build",
		BinaryPath: "/opt/forge/llama.cpp/build-vulkan/bin/llama-server",
		Backend:    "vulkan",
		Reason:     "default vulkan build",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	builds, err := cat.ListBuilds(ctx)
	if err != nil {
		t.Fatalf("ListBuilds: %v", err)
	}
	var gotBuild Build
	for _, b := range builds {
		if b.ID == buildID {
			gotBuild = b
		}
	}
	if gotBuild.Backend != "vulkan" {
		t.Errorf("build backend: got %q, want 'vulkan'", gotBuild.Backend)
	}

	// ── Config ──
	cfgID, err := cat.CreateConfig(ctx, Config{
		Name:             "gemma4-31b",
		VariantID:        varID,
		WeightArtifactID: weightID,
		EngineID:         eng.ID,
		BuildID:          buildID,
		MMProjArtifactID: mmprojID,
		NCtx:             262144,
		Parallel:         2,
		ExtraArgs:        []string{"--no-mmap", "--jinja", "--parallel", "2"},
		Status:           "unverified",
		Visibility:       "visible",
		IsDefault:        true,
		Fingerprint:      "abc123",
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if cfgID == 0 {
		t.Fatal("config ID should be non-zero")
	}
	// Duplicate config name must fail (UNIQUE constraint).
	if _, err := cat.CreateConfig(ctx, Config{
		Name: "gemma4-31b", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID,
	}); err == nil {
		t.Error("duplicate config name should fail UNIQUE constraint")
	}

	// ── Service ──
	svcID, err := cat.CreateService(ctx, Service{
		Name:        "comfyui",
		Label:       "ComfyUI",
		Description: "Image, video, and 3D generation",
		Icon:        "comfy.svg",
		Color:       "#E8871E",
		Unit:        "ai-mode-comfyui",
		HealthCheck: `{"type":"systemd_unit","arg":"ai-mode-comfyui"}`,
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if svcID == 0 {
		t.Fatal("service ID should be non-zero")
	}
	// Duplicate service name: INSERT OR IGNORE returns existing ID.
	svcID2, err := cat.CreateService(ctx, Service{Name: "comfyui"})
	if err != nil {
		t.Fatalf("CreateService duplicate: %v", err)
	}
	if svcID2 != svcID {
		t.Errorf("duplicate service ID: got %d, want %d", svcID2, svcID)
	}

	// ── Offering ──
	// First need a provider in router_providers. The compressor surface
	// is the write path for that table.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "deepseek", APIKey: "sk-test", CreatedAt: ts(100),
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	dsProvider, ok, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil || !ok {
		t.Fatalf("ProviderByName: ok=%v err=%v", ok, err)
	}
	offID, err := cat.CreateOffering(ctx, Offering{
		ModelID:    mdlID,
		VariantID:  varID,
		ProviderID: dsProvider.ID,
		WireModel:  "deepseek-chat",
		Currency:   "USD",
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}
	if offID == 0 {
		t.Fatal("offering ID should be non-zero")
	}

	// ── Benchmark (self_measured — no source_url required) ──
	benchID, err := cat.CreateBenchmark(ctx, Benchmark{
		Metric:      "decode_tps",
		Value:       "18.0",
		Source:      "self_measured",
		SubjectType: "variant",
		SubjectID:   varID,
	})
	if err != nil {
		t.Fatalf("CreateBenchmark (self_measured): %v", err)
	}
	if benchID == 0 {
		t.Fatal("benchmark ID should be non-zero")
	}

	// ── Note ──
	noteID, err := cat.CreateNote(ctx, Note{
		SubjectType: "model",
		SubjectID:   mdlID,
		Author:      "migrate-v4",
		Body:        "Tends to truncate long outputs",
	})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if noteID == 0 {
		t.Fatal("note ID should be non-zero")
	}

	// ── Verify lists round-trip ──
	fams, err := cat.ListFamilies(ctx)
	if err != nil {
		t.Fatalf("ListFamilies: %v", err)
	}
	if len(fams) != 1 || fams[0].Name != "Gemma" {
		t.Errorf("ListFamilies: %+v", fams)
	}

	mdls, err := cat.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(mdls) != 1 || mdls[0].Name != "Gemma 4 31B (MTP)" {
		t.Errorf("ListModels: %+v", mdls)
	}
	if len(mdls[0].KeyFeatures) != 4 {
		t.Errorf("KeyFeatures round-trip: got %d, want 4: %+v", len(mdls[0].KeyFeatures), mdls[0].KeyFeatures)
	}
	if mdls[0].FamilyID != famID {
		t.Errorf("model family_id: got %d, want %d", mdls[0].FamilyID, famID)
	}

	vars, err := cat.ListVariants(ctx)
	if err != nil {
		t.Fatalf("ListVariants: %v", err)
	}
	if len(vars) != 1 || !vars[0].IsAbliterated || vars[0].AbliterationQuality != "High" {
		t.Errorf("ListVariants: %+v", vars)
	}

	arts, err := cat.ListArtifacts(ctx)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Errorf("ListArtifacts: got %d, want 2", len(arts))
	}

	cfgs, err := cat.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("ListConfigs: got %d, want 1", len(cfgs))
	}
	c := cfgs[0]
	if c.Name != "gemma4-31b" || c.NCtx != 262144 || c.Parallel != 2 || !c.IsDefault {
		t.Errorf("config round-trip mismatch: %+v", c)
	}
	if len(c.ExtraArgs) != 4 || c.ExtraArgs[0] != "--no-mmap" {
		t.Errorf("ExtraArgs round-trip: %+v", c.ExtraArgs)
	}
	if c.MMProjArtifactID != mmprojID || c.BuildID != buildID {
		t.Errorf("config FK mismatch: mmproj=%d want=%d, build=%d want=%d",
			c.MMProjArtifactID, mmprojID, c.BuildID, buildID)
	}

	svcs, err := cat.ListServices(ctx)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(svcs) != 1 || svcs[0].Name != "comfyui" || svcs[0].Unit != "ai-mode-comfyui" {
		t.Errorf("ListServices: %+v", svcs)
	}

	offs, err := cat.ListOfferings(ctx)
	if err != nil {
		t.Fatalf("ListOfferings: %v", err)
	}
	if len(offs) != 1 || offs[0].WireModel != "deepseek-chat" || offs[0].ProviderName != "deepseek" {
		t.Errorf("ListOfferings: %+v", offs)
	}
	if !offs[0].Enabled {
		t.Error("offering should be enabled")
	}

	benches, err := cat.ListBenchmarks(ctx)
	if err != nil {
		t.Fatalf("ListBenchmarks: %v", err)
	}
	if len(benches) != 1 || benches[0].Metric != "decode_tps" || benches[0].Value != "18.0" {
		t.Errorf("ListBenchmarks: %+v", benches)
	}

	notes, err := cat.ListNotes(ctx)
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if len(notes) != 1 || notes[0].Body != "Tends to truncate long outputs" {
		t.Errorf("ListNotes: %+v", notes)
	}
}

// TestCatalogSlots exercises the Slot CRUD surface added by the TOML
// decommission Phase 0 schema (docs/v5-toml-decommission.md §3.2). Slot has
// no seed data of its own — the cutover migration (§5, not built yet) is
// what populates real rows from forge.toml's [slots.*] on ForgeHost.
func TestCatalogSlots(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	id, err := cat.CreateSlot(ctx, Slot{
		Name: "a1", Unit: "forge-a1", Port: 8080, Label: "A1", SortOrder: 1,
	})
	if err != nil {
		t.Fatalf("CreateSlot: %v", err)
	}
	if id == 0 {
		t.Fatal("slot ID should be non-zero")
	}

	// Duplicate name: INSERT OR IGNORE returns the existing ID.
	id2, err := cat.CreateSlot(ctx, Slot{Name: "a1", Unit: "other", Port: 1, Label: "X"})
	if err != nil {
		t.Fatalf("CreateSlot duplicate: %v", err)
	}
	if id2 != id {
		t.Errorf("duplicate slot ID: got %d, want %d", id2, id)
	}

	if _, err := cat.CreateSlot(ctx, Slot{
		Name: "a3", Unit: "forge-a3", Port: 8087, Label: "A3", SortOrder: 3,
	}); err != nil {
		t.Fatalf("CreateSlot a3: %v", err)
	}

	slots, err := cat.ListSlots(ctx)
	if err != nil {
		t.Fatalf("ListSlots: %v", err)
	}
	if len(slots) != 2 || slots[0].Name != "a1" || slots[1].Name != "a3" {
		t.Errorf("ListSlots order/content: %+v", slots)
	}

	got, err := cat.GetSlot(ctx, id)
	if err != nil {
		t.Fatalf("GetSlot: %v", err)
	}
	if got.Unit != "forge-a1" || got.Port != 8080 || got.Label != "A1" {
		t.Errorf("GetSlot: %+v", got)
	}

	byName, err := cat.SlotByName(ctx, "a1")
	if err != nil {
		t.Fatalf("SlotByName: %v", err)
	}
	if byName.ID != id {
		t.Errorf("SlotByName ID: got %d, want %d", byName.ID, id)
	}
	if _, err := cat.SlotByName(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SlotByName nonexistent: got %v, want ErrNotFound", err)
	}

	got.Port = 9090
	got.Label = "A1-renamed"
	if err := cat.UpdateSlot(ctx, got); err != nil {
		t.Fatalf("UpdateSlot: %v", err)
	}
	reGot, err := cat.GetSlot(ctx, id)
	if err != nil {
		t.Fatalf("GetSlot after update: %v", err)
	}
	if reGot.Port != 9090 || reGot.Label != "A1-renamed" {
		t.Errorf("UpdateSlot did not persist: %+v", reGot)
	}
	if err := cat.UpdateSlot(ctx, Slot{ID: 99999, Name: "ghost"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateSlot missing ID: got %v, want ErrNotFound", err)
	}

	if err := cat.DeleteSlot(ctx, id); err != nil {
		t.Fatalf("DeleteSlot: %v", err)
	}
	if _, err := cat.GetSlot(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSlot after delete: got %v, want ErrNotFound", err)
	}
	if err := cat.DeleteSlot(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteSlot already-deleted: got %v, want ErrNotFound", err)
	}
}

// TestCatalogF7Gate verifies the F7 fabrication-prevention gate (ADR-0005):
// published benchmarks require source_url + source_date. The CHECK constraint
// in the migration rejects the insert structurally — not just at validation.
func TestCatalogF7Gate(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	// Set up a model to attach the benchmark to.
	mdlID, err := cat.CreateModel(ctx, Model{Name: "TestModel"})
	if err != nil {
		t.Fatal(err)
	}

	// published without source_url → must fail.
	_, err = cat.CreateBenchmark(ctx, Benchmark{
		Metric: "GPQA Diamond", Value: "0.843", Source: "published",
		SubjectType: "model", SubjectID: mdlID,
	})
	if err == nil {
		t.Error("published benchmark without source_url should fail F7 gate")
	}

	// published without source_date → must fail.
	_, err = cat.CreateBenchmark(ctx, Benchmark{
		Metric: "GPQA Diamond", Value: "0.843", Source: "published",
		SourceURL:   "https://example.com/bench",
		SubjectType: "model", SubjectID: mdlID,
	})
	if err == nil {
		t.Error("published benchmark without source_date should fail F7 gate")
	}

	// published with both source_url + source_date → must succeed.
	_, err = cat.CreateBenchmark(ctx, Benchmark{
		Metric: "GPQA Diamond", Value: "0.843", Source: "published",
		SourceURL: "https://example.com/bench", SourceDate: "2026-07-23",
		SubjectType: "model", SubjectID: mdlID,
	})
	if err != nil {
		t.Errorf("published benchmark with source_url + source_date: %v", err)
	}

	// self_measured without source_url → must succeed (no F7 requirement).
	_, err = cat.CreateBenchmark(ctx, Benchmark{
		Metric: "decode_tps", Value: "18.0", Source: "self_measured",
		SubjectType: "model", SubjectID: mdlID,
	})
	if err != nil {
		t.Errorf("self_measured benchmark without source_url: %v", err)
	}

	// provider_reported without source_url → must succeed.
	_, err = cat.CreateBenchmark(ctx, Benchmark{
		Metric: "context_length", Value: "131072", Source: "provider_reported",
		SubjectType: "model", SubjectID: mdlID,
	})
	if err != nil {
		t.Errorf("provider_reported benchmark without source_url: %v", err)
	}
}

// TestCatalogEnumLookups verifies the static-seeded enum tables
// (quantizations, formats, engines) are populated by the migration.
func TestCatalogEnumLookups(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	for _, q := range []string{"Q4_K_M", "Q6_K", "Q8_0", "MXFP4", "BF16"} {
		if _, err := cat.QuantizationByName(ctx, q); err != nil {
			t.Errorf("QuantizationByName(%q): %v", q, err)
		}
	}
	for _, f := range []string{"GGUF", "safetensors"} {
		if _, err := cat.FormatByName(ctx, f); err != nil {
			t.Errorf("FormatByName(%q): %v", f, err)
		}
	}
	for _, e := range []string{"llama.cpp", "vLLM"} {
		if _, err := cat.EngineByName(ctx, e); err != nil {
			t.Errorf("EngineByName(%q): %v", e, err)
		}
	}
}

// TestCatalogModelNoFamily verifies that a Model with FamilyID=0 stores a NULL
// family_id (not 0, which would violate the FK constraint).
func TestCatalogModelNoFamily(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, err := cat.CreateModel(ctx, Model{
		Name:    "Orphan Model",
		Creator: "Unknown",
	})
	if err != nil {
		t.Fatalf("CreateModel without family: %v", err)
	}
	mdls, err := cat.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mdls) != 1 || mdls[0].ID != mdlID || mdls[0].FamilyID != 0 {
		t.Errorf("model without family: %+v", mdls)
	}
}

// TestCatalogModelVisibility covers the model-level decommission flag
// (0062): empty defaults to visible, hidden round-trips through CRUD, and
// reads never return an empty value.
func TestCatalogModelVisibility(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	visID, err := cat.CreateModel(ctx, Model{Name: "Visible Model"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	hiddenID, err := cat.CreateModel(ctx, Model{Name: "Hidden Model", Visibility: "hidden"})
	if err != nil {
		t.Fatalf("CreateModel hidden: %v", err)
	}

	// Default on create is "visible".
	m, err := cat.GetModel(ctx, visID)
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if m.Visibility != "visible" {
		t.Errorf("default visibility = %q, want visible", m.Visibility)
	}

	m, err = cat.GetModel(ctx, hiddenID)
	if err != nil {
		t.Fatalf("GetModel hidden: %v", err)
	}
	if m.Visibility != "hidden" {
		t.Errorf("hidden visibility = %q, want hidden", m.Visibility)
	}

	// List returns hidden models too (Settings-only manage surface).
	mdls, err := cat.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[int64]Model{}
	for _, mm := range mdls {
		byID[mm.ID] = mm
	}
	if byID[hiddenID].Visibility != "hidden" {
		t.Errorf("ListModels missing hidden model: %+v", byID)
	}

	// Update toggles hidden -> visible and back.
	if err := cat.UpdateModel(ctx, Model{ID: visID, Name: "Visible Model", Visibility: "hidden"}); err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}
	m, _ = cat.GetModel(ctx, visID)
	if m.Visibility != "hidden" {
		t.Errorf("after update visibility = %q, want hidden", m.Visibility)
	}
}

// TestListOfferingsPriceCachedInPer1M covers the cost/savings sprint Phase 4
// addition — CreateOffering doesn't yet expose this column (no CRUD UI
// wired for it), so it's set via a direct SQL UPDATE, matching how an
// operator would set it today (forge config-style direct edit) until a
// CRUD form exists.
func TestListOfferingsPriceCachedInPer1M(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, err := cat.CreateModel(ctx, Model{Name: "TestModel"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO router_providers (name, api_key, created_at) VALUES ('deepseek', 'key', 0)`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	offID, err := cat.CreateOffering(ctx, Offering{
		ModelID: mdlID, ProviderID: testProviderID(t, db, "deepseek"), WireModel: "deepseek-chat",
		PriceInPer1M: 0.27, PriceOutPer1M: 1.10, Currency: "USD", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}

	offs, err := cat.ListOfferings(ctx)
	if err != nil {
		t.Fatalf("ListOfferings: %v", err)
	}
	if len(offs) != 1 || offs[0].PriceCachedInPer1M != nil {
		t.Fatalf("PriceCachedInPer1M should default nil: %+v", offs)
	}

	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE offerings SET price_cached_in_per_1m = ? WHERE id = ?`, 0.027, offID); err != nil {
		t.Fatalf("set price_cached_in_per_1m: %v", err)
	}
	offs, err = cat.ListOfferings(ctx)
	if err != nil {
		t.Fatalf("ListOfferings after update: %v", err)
	}
	if len(offs) != 1 || offs[0].PriceCachedInPer1M == nil || *offs[0].PriceCachedInPer1M != 0.027 {
		t.Fatalf("PriceCachedInPer1M round-trip failed: %+v", offs)
	}
}

// TestOfferingPriorityRoundTripAndOrdering covers the 0032 priority column:
// it round-trips through Create/Get/Update, and ListOfferings orders by
// (priority, provider, wire_model) — the exact order the router's group
// selection relies on for primary choice + deterministic tie-breaks.
func TestOfferingPriorityRoundTripAndOrdering(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, err := cat.CreateModel(ctx, Model{Name: "glm-5.2"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	for _, p := range []string{"aiand", "qwen"} {
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO router_providers (name, api_key, created_at) VALUES (?, 'key', 0)`, p); err != nil {
			t.Fatalf("seed provider %s: %v", p, err)
		}
	}

	// Insert out of order: the list must still come back priority-first.
	offAland, err := cat.CreateOffering(ctx, Offering{
		ModelID: mdlID, ProviderID: testProviderID(t, db, "aiand"), WireModel: "zai-org/glm-5.2",
		Enabled: true, Priority: 100,
	})
	if err != nil {
		t.Fatalf("CreateOffering(aiand): %v", err)
	}
	offQwen, err := cat.CreateOffering(ctx, Offering{
		ModelID: mdlID, ProviderID: testProviderID(t, db, "qwen"), WireModel: "glm-5.2",
		Enabled: true, Priority: 10,
	})
	if err != nil {
		t.Fatalf("CreateOffering(qwen): %v", err)
	}
	// Same priority as qwen, alphabetically-later provider → tie-break check.
	offTie, err := cat.CreateOffering(ctx, Offering{
		ModelID: mdlID, ProviderID: testProviderID(t, db, "qwen"), WireModel: "glm-5.2-alias",
		Enabled: true, Priority: 10,
	})
	if err != nil {
		t.Fatalf("CreateOffering(tie): %v", err)
	}

	offs, err := cat.ListOfferings(ctx)
	if err != nil {
		t.Fatalf("ListOfferings: %v", err)
	}
	if len(offs) != 3 {
		t.Fatalf("ListOfferings = %d rows, want 3", len(offs))
	}
	wantIDs := []int64{offQwen, offTie, offAland}
	for i, want := range wantIDs {
		if offs[i].ID != want {
			t.Errorf("order[%d] = offering %d (%s/%s), want %d",
				i, offs[i].ID, offs[i].ProviderName, offs[i].WireModel, want)
		}
	}

	// Get + Update round-trip.
	got, err := cat.GetOffering(ctx, offQwen)
	if err != nil || got.Priority != 10 {
		t.Fatalf("GetOffering priority = %d, err %v; want 10", got.Priority, err)
	}
	got.Priority = 1
	if err := cat.UpdateOffering(ctx, got); err != nil {
		t.Fatalf("UpdateOffering: %v", err)
	}
	offs, _ = cat.ListOfferings(ctx)
	if offs[0].ID != offQwen || offs[0].Priority != 1 {
		t.Errorf("after re-prioritize, first = offering %d priority %d; want %d priority 1",
			offs[0].ID, offs[0].Priority, offQwen)
	}

	// ListOfferingsForModel honors the same ordering.
	forModel, err := cat.ListOfferingsForModel(ctx, mdlID)
	if err != nil {
		t.Fatalf("ListOfferingsForModel: %v", err)
	}
	if len(forModel) != 3 || forModel[0].ID != offQwen {
		t.Errorf("ListOfferingsForModel order wrong: %+v", forModel)
	}
}
