// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// TestMergedConfig_EngineKind pins that the engine kind reaches the launch spec from the config's engines row (ADR-0025): llama.cpp by
// default, vLLM from the vLLM row (even with a plain GPU backend), and a kind this binary does not know drops the mode loudly.
func TestMergedConfig_EngineKind(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "K"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	format, _ := cat.FormatByName(ctx, "safetensors")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{VariantID: varID, FormatID: format.ID, FilePath: "k", ArtifactType: "weight"})
	llama, _ := cat.EngineByName(ctx, "llama.cpp")
	vllm, _ := cat.EngineByName(ctx, "vLLM")

	add := func(name string, engineID int64, backend string) {
		t.Helper()
		bid, err := cat.CreateBuild(ctx, store.Build{EngineID: engineID, Name: "b-" + name, Backend: backend, BinaryPath: "/opt/x"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cat.CreateConfig(ctx, store.Config{Name: name, VariantID: varID, WeightArtifactID: weightID, EngineID: engineID, BuildID: bid, NCtx: 4096}); err != nil {
			t.Fatal(err)
		}
	}
	add("k-llama", llama.ID, "rocm")
	add("k-vllm-engine", vllm.ID, "rocm")
	add("k-vllm-legacy", llama.ID, "vllm")

	res, err := db.SQL().ExecContext(ctx, `INSERT INTO engines (name, kind) VALUES ('mystery', 'bogus')`)
	if err != nil {
		t.Fatal(err)
	}
	mid, _ := res.LastInsertId()
	add("k-mystery", mid, "rocm")

	provider := newMergedConfigProvider(func() *config.Config { return &config.Config{Modes: map[string]config.Mode{}} }, db.Catalog())
	got := provider.Get()

	for name, want := range map[string]string{"k-llama": "llamacpp", "k-vllm-engine": "vllm", "k-vllm-legacy": "vllm"} {
		m, ok := got.Modes[name]
		if !ok {
			t.Fatalf("mode %q missing", name)
		}
		spec, err := m.Services[0].Engine()
		if err != nil || spec.Name != want {
			t.Errorf("%s: engine = %q, %v; want %q", name, spec.Name, err, want)
		}
	}
	if _, ok := got.Modes["k-mystery"]; ok {
		t.Error("a mode on an engine of unknown kind was merged; it must be omitted")
	}
}
