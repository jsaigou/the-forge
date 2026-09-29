// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// TestMergedConfig_EmptyCatalogHasNoModes verifies the post-TOML-decommission
// behavior (docs/v5-toml-decommission.md §6): with a real (empty) catalog
// wired, Modes comes entirely from the store — an empty configs table means
// zero Modes, not a fallback to the base config's Modes. The base config's
// infra fields still pass through untouched.
func TestMergedConfig_EmptyCatalogHasNoModes(t *testing.T) {
	db := openTestDB(t)
	fileCfg := &config.Config{
		Server: config.Server{Listen: ":5000"},
		Modes: map[string]config.Mode{
			"test-mode": {Label: "Test", Type: "", Services: []config.Service{{
				Model: "test.gguf", Backend: "vulkan", Context: 32768,
			}}},
		},
	}
	provider := newMergedConfigProvider(func() *config.Config { return fileCfg }, db.Catalog())

	got := provider.Get()
	if len(got.Modes) != 0 {
		t.Fatalf("got %d modes, want 0 (no file fallback — catalog is the only source)", len(got.Modes))
	}
	if got.Server.Listen != ":5000" {
		t.Errorf("infra field Server.Listen = %q, want :5000 (should pass through from base config)", got.Server.Listen)
	}
}

// TestMergedConfig_StoreBacked verifies the full merged view: store-backed
// configs/services/artifacts are converted back to config.Mode shape and
// replace the file config's Modes. The file config's infra fields (Slots,
// Ports, etc.) are preserved.
func TestMergedConfig_StoreBacked(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	cat := db.Catalog()

	// Seed a minimal catalog: family → model → variant → artifacts → engine →
	// build → config, plus a service.
	famID, _ := cat.CreateFamily(ctx, store.Family{Name: "Gemma"})
	mdlID, _ := cat.CreateModel(ctx, store.Model{
		FamilyID: famID, Name: "Gemma 4 31B", Description: "test model",
		Creator: "Google", Logo: "google", KeyFeatures: []string{"Dense"},
	})
	varID, _ := cat.CreateVariant(ctx, store.Variant{
		ModelID: mdlID, Name: "Q8_0 + MTP", IsAbliterated: true,
	})
	fmt, _ := cat.FormatByName(ctx, "GGUF")
	q, _ := cat.QuantizationByName(ctx, "Q6_K")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, QuantizationID: q.ID, FormatID: fmt.ID,
		FilePath: "gemma4-31b-Q6_K.gguf", ArtifactType: "weight",
	})
	mmprojID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID,
		FilePath: "mmproj.gguf", IsAuxiliary: true, ArtifactType: "mmproj",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	buildID, _ := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "vulkan-build", Backend: "vulkan",
		BinaryPath: "/opt/llama.cpp/build-vulkan/bin/llama-server",
	})
	_, _ = cat.CreateConfig(ctx, store.Config{
		Name: "gemma4-31b", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, BuildID: buildID, MMProjArtifactID: mmprojID,
		NCtx: 262144, Parallel: 2, ExtraArgs: []string{"--no-mmap", "--parallel", "2"},
		Status: "unverified", Visibility: "visible", IsDefault: true,
	})
	_, _ = cat.CreateService(ctx, store.Service{
		Name: "comfyui", Label: "ComfyUI", Unit: "ai-mode-comfyui",
		Description: "image gen", Icon: "comfy.svg", Color: "#E8871E",
	})

	// File config has a mode that should NOT appear in the merged view
	// (store replaces Modes entirely when populated).
	fileCfg := &config.Config{
		Ports: map[string]int{"embedding": 8083},
		Modes: map[string]config.Mode{
			"old-file-mode": {Label: "Old"},
		},
	}
	provider := newMergedConfigProvider(func() *config.Config { return fileCfg }, db.Catalog())

	got := provider.Get()

	// Infra fields preserved from file.
	if len(got.Ports) != 1 || got.Ports["embedding"] != 8083 {
		t.Errorf("infra ports not preserved: %+v", got.Ports)
	}

	// File-only mode is gone (store replaces Modes).
	if _, exists := got.Modes["old-file-mode"]; exists {
		t.Error("file-only mode 'old-file-mode' should not appear in merged view")
	}

	// Store-backed config mode.
	m, ok := got.Modes["gemma4-31b"]
	if !ok {
		t.Fatal("missing store-backed mode 'gemma4-31b'")
	}
	if m.Label != "Gemma 4 31B" {
		t.Errorf("label: got %q, want 'Gemma 4 31B'", m.Label)
	}
	if m.Family != "Gemma" {
		t.Errorf("family: got %q, want 'Gemma'", m.Family)
	}
	if m.Description != "test model" {
		t.Errorf("description: got %q", m.Description)
	}
	if m.Icon != "google" {
		t.Errorf("icon: got %q", m.Icon)
	}
	if !m.Default {
		t.Error("should be default")
	}
	if len(m.Tags) != 1 || m.Tags[0] != "Dense" {
		t.Errorf("tags: %+v", m.Tags)
	}
	if len(m.Services) != 1 {
		t.Fatalf("services: got %d, want 1", len(m.Services))
	}
	svc := m.Services[0]
	if svc.Model != "gemma4-31b-Q6_K.gguf" {
		t.Errorf("model path: got %q", svc.Model)
	}
	if svc.Backend != "vulkan" {
		t.Errorf("backend: got %q, want 'vulkan'", svc.Backend)
	}
	if svc.MMProj != "mmproj.gguf" {
		t.Errorf("mmproj: got %q", svc.MMProj)
	}
	if svc.LlamaBin != "/opt/llama.cpp/build-vulkan/bin/llama-server" {
		t.Errorf("llama_bin: got %q", svc.LlamaBin)
	}
	if svc.Context != 262144 {
		t.Errorf("context: got %d, want 262144", svc.Context)
	}
	if svc.Alias != "gemma4-31b" {
		t.Errorf("alias: got %q, want 'gemma4-31b' (config name, not variant name)", svc.Alias)
	}
	if len(svc.ExtraArgs) != 3 || svc.ExtraArgs[0] != "--no-mmap" {
		t.Errorf("extra_args: %+v", svc.ExtraArgs)
	}

	// Store-backed service mode.
	svcMode, ok := got.Modes["comfyui"]
	if !ok {
		t.Fatal("missing service mode 'comfyui'")
	}
	if svcMode.Type != "service" {
		t.Errorf("service type: got %q, want 'service'", svcMode.Type)
	}
	if svcMode.Label != "ComfyUI" {
		t.Errorf("service label: got %q", svcMode.Label)
	}
	if svcMode.Unit != "ai-mode-comfyui" {
		t.Errorf("service unit: got %q", svcMode.Unit)
	}
}

// TestMergedConfig_RocmBackend verifies backend comes directly from the
// Build's explicit Backend field (not from name-matching — a build named
// anything, even something that doesn't mention "rocm" at all, must still
// resolve correctly as long as Backend is set explicitly).
func TestMergedConfig_RocmBackend(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "Nemotron"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID,
		FilePath: "nemotron.gguf", ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	buildID, _ := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "some-arbitrary-name", Backend: "rocm",
		BinaryPath: "/opt/llama.cpp/build/bin/llama-server",
	})
	_, _ = cat.CreateConfig(ctx, store.Config{
		Name: "nemotron", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, BuildID: buildID, NCtx: 1048576,
	})

	fileCfg := &config.Config{Modes: map[string]config.Mode{}}
	provider := newMergedConfigProvider(func() *config.Config { return fileCfg }, db.Catalog())

	got := provider.Get()
	m, ok := got.Modes["nemotron"]
	if !ok {
		t.Fatal("mode 'nemotron' missing from merged view")
	}
	if m.Services[0].Backend != "rocm" {
		t.Errorf("backend: got %q, want 'rocm'", m.Services[0].Backend)
	}
}

// TestMergedConfig_VLLMBackend verifies a vLLM Build's explicit Backend
// passes through unchanged.
func TestMergedConfig_VLLMBackend(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "Carbon"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt, _ := cat.FormatByName(ctx, "safetensors")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID,
		FilePath: "carbon-8b", ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "vLLM")
	buildID, _ := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "standard-vllm", Backend: "vllm",
	})
	_, _ = cat.CreateConfig(ctx, store.Config{
		Name: "carbon-8b", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, BuildID: buildID, NCtx: 32768,
	})

	fileCfg := &config.Config{Modes: map[string]config.Mode{}}
	provider := newMergedConfigProvider(func() *config.Config { return fileCfg }, db.Catalog())

	got := provider.Get()
	m, ok := got.Modes["carbon-8b"]
	if !ok {
		t.Fatal("mode 'carbon-8b' missing from merged view")
	}
	if m.Services[0].Backend != "vllm" {
		t.Errorf("backend: got %q, want 'vllm'", m.Services[0].Backend)
	}
}

// TestMergedConfig_NoBuildRefusesToGuess pins the fix for the actual bug: a
// Config with no Build (or a Build with no explicit Backend) must never
// resolve to a guessed backend — it must be omitted from the merged view
// entirely (a safe, loud "unknown mode"), while every other, correctly
// configured mode still merges normally. Before this fix, this exact
// scenario (nemotron: real config, no Build at all) silently produced
// backend "vulkan" and launched the wrong binary in production.
func TestMergedConfig_NoBuildRefusesToGuess(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	cat := db.Catalog()

	// Broken config: no build_id at all (the exact nemotron scenario).
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "Nemotron"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmtGGUF, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmtGGUF.ID,
		FilePath: "nemotron.gguf", ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	_, _ = cat.CreateConfig(ctx, store.Config{
		Name: "nemotron", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, NCtx: 1048576, // BuildID left 0
	})

	// A second, correctly configured mode — must still merge fine.
	mdlID2, _ := cat.CreateModel(ctx, store.Model{Name: "Other"})
	varID2, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID2, Name: "base"})
	weightID2, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID2, FormatID: fmtGGUF.ID,
		FilePath: "other.gguf", ArtifactType: "weight",
	})
	buildID2, _ := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "standard-vulkan", Backend: "vulkan",
	})
	_, _ = cat.CreateConfig(ctx, store.Config{
		Name: "other-mode", VariantID: varID2, WeightArtifactID: weightID2,
		EngineID: eng.ID, BuildID: buildID2, NCtx: 32768,
	})

	fileCfg := &config.Config{Modes: map[string]config.Mode{}}
	provider := newMergedConfigProvider(func() *config.Config { return fileCfg }, db.Catalog())

	got := provider.Get()

	if _, exists := got.Modes["nemotron"]; exists {
		t.Errorf("nemotron should be omitted (no build defined), not guessed — got backend %q",
			got.Modes["nemotron"].Services[0].Backend)
	}
	m, ok := got.Modes["other-mode"]
	if !ok {
		t.Fatal("other-mode should still merge correctly despite nemotron's broken config")
	}
	if m.Services[0].Backend != "vulkan" {
		t.Errorf("other-mode backend: got %q, want 'vulkan'", m.Services[0].Backend)
	}
}

// TestMergedConfig_CacheTTL verifies that the merged view is cached: within
// the TTL window, repeated calls return the same pointer without re-reading
// the store. After invalidation, a fresh read occurs.
func TestMergedConfig_CacheTTL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	cat := db.Catalog()

	// Seed a config so the merged view builds a new *config.Config (not the
	// file config pointer directly).
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "Test"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID, FilePath: "test.gguf",
		ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	buildID, _ := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "standard-vulkan", Backend: "vulkan",
	})
	cat.CreateConfig(ctx, store.Config{
		Name: "test", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, BuildID: buildID, NCtx: 32768,
	})

	fileCfg := &config.Config{Modes: map[string]config.Mode{}}
	provider := newMergedConfigProvider(func() *config.Config { return fileCfg }, db.Catalog())

	// First call builds the cache.
	got1 := provider.Get()
	// Second call within TTL returns the same cached pointer.
	got2 := provider.Get()
	if got1 != got2 {
		t.Error("cache miss: second call within TTL should return same pointer")
	}

	// Invalidate forces a rebuild.
	provider.Invalidate()
	got3 := provider.Get()
	if got3 == got1 {
		t.Error("cache not invalidated: third call should return new pointer")
	}
}

// TestMergedConfig_NilCatalog verifies the degenerate case: nil catalog
// (not wired) → returns file config directly.
func TestMergedConfig_NilCatalog(t *testing.T) {
	fileCfg := &config.Config{
		Modes: map[string]config.Mode{
			"file-mode": {Label: "File"},
		},
	}
	provider := newMergedConfigProvider(func() *config.Config { return fileCfg }, nil)

	got := provider.Get()
	if got != fileCfg {
		t.Error("nil catalog should return file config directly")
	}
}

// TestMergedConfig_StoreErrorFallback verifies that when the store read
// fails (e.g. DB closed), the merged provider falls back to the file config
// rather than crashing.
func TestMergedConfig_StoreErrorFallback(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Close the DB so store reads fail.
	db.Close()

	fileCfg := &config.Config{
		Modes: map[string]config.Mode{
			"file-mode": {Label: "File"},
		},
	}
	provider := newMergedConfigProvider(func() *config.Config { return fileCfg }, db.Catalog())

	// Should fall back to file config, not panic.
	got := provider.Get()
	if len(got.Modes) != 1 || got.Modes["file-mode"].Label != "File" {
		t.Errorf("store error fallback: got %+v", got.Modes)
	}
}

// ── Test helpers ──────────────────────────────────────────────────────────────

// openTestDB opens an in-memory store DB for merged-config tests.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
