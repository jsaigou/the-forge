// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
)

// The pool launches what an LLM slot would: the merged config's model, mmproj, binary, context and args.
func TestCatalogResolver_ResolvesLikeAnLLMSlotLaunch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "d1.gguf"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mm.gguf"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Paths: config.Paths{ModelsDir: dir}, Modes: map[string]config.Mode{
		"d1-2b": {ConfigID: 7, Services: []config.Service{{Model: "d1.gguf", Alias: "d1-2b", Context: 128000, Backend: "vulkan", MMProj: "mm.gguf",
			ExtraArgs: []string{"--jinja", "--threads", "16"}}}},
		"big-rocm": {Services: []config.Service{{Model: "d1.gguf", Alias: "big-rocm", Context: 4096, Backend: "rocm", LlamaBin: "/opt/forge/llama.cpp-upstream-plain/build-custom/bin/llama-server"}}},
		"comfy":    {Type: "service", Unit: "ai-mode-comfyui"},
	}}
	r := catalogResolver{cfg: func() *config.Config { return cfg }, estimate: func(id int64) (int64, bool) { return 5 << 30, id == 7 }}

	sp, err := r.LaunchSpec(context.Background(), "d1-2b")
	if err != nil {
		t.Fatal(err)
	}
	if sp.Bin != "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server" || sp.Model != filepath.Join(dir, "d1.gguf") || sp.MMProj != filepath.Join(dir, "mm.gguf") ||
		sp.Alias != "d1-2b" || sp.Ctx != 128000 || sp.Backend != "vulkan" || len(sp.Args) != 3 {
		t.Errorf("spec = %+v", sp)
	}
	if sp, _ := r.LaunchSpec(context.Background(), "big-rocm"); sp.Bin != "/opt/forge/llama.cpp-upstream-plain/build-custom/bin/llama-server" || sp.Backend != "rocm" {
		t.Errorf("a config's own build must win: %+v", sp)
	}
	for _, name := range []string{"nope", "comfy"} {
		if _, err := r.LaunchSpec(context.Background(), name); err == nil {
			t.Errorf("%q was resolved", name)
		}
	}
	// The curated figure wins; without one, the weight files plus a 20% margin.
	if got := r.EstimateBytes(context.Background(), "d1-2b"); got != 5<<30 {
		t.Errorf("curated estimate = %d", got)
	}
	if got := r.EstimateBytes(context.Background(), "big-rocm"); got != 1000+200 {
		t.Errorf("fallback estimate = %d, want the weight file plus 20%%", got)
	}
	if got := r.EstimateBytes(context.Background(), "nope"); got != 0 {
		t.Errorf("unknown config estimate = %d", got)
	}
}

// ADR-0029: a Radiance config is hosted by the pool through the same launch row an LLM slot would get, built for the POOL slot's port (the pool's slots
// are not in cfg.Slots), and a llama.cpp config keeps its llama-server path.
func TestCatalogResolver_ARadianceConfigIsRowLaunchedOnAPoolSlot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "haru-bf16.rad"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Paths: config.Paths{ModelsDir: dir},
		SpecialistSlots: map[string]config.Slot{"s7": {Unit: "forge-pool@s7", Port: 8106, Label: "S7", Order: 7}},
		Modes: map[string]config.Mode{
			"haru-4b": {ConfigID: 9, Services: []config.Service{{
				Model: "haru-bf16.rad", Alias: "haru-4b", Context: 14500, Backend: "rocm", EngineKind: "radiance",
				LlamaBin:  "/opt/radiance/strix-halo-haru/build/bin/radiance",
				LaunchEnv: map[string]string{"RADIANCE_HOME": "/opt/radiance/strix-halo-haru/build/radiance_home", "RADIANCE_KERNELS": "libhalo"},
				ExtraArgs: []string{"--max-num-seqs", "2", "--num-speculative-tokens", "0", "--tp", "1", "--duplex-max-lanes", "2", "--duplex-max-ctx", "14500"}}}},
			"d1-2b": {Services: []config.Service{{Model: "d1.gguf", Alias: "d1-2b", Context: 1000, Backend: "vulkan"}}},
		}}
	r := catalogResolver{cfg: func() *config.Config { return cfg }}
	ctx := context.Background()

	sp, err := r.LaunchSpec(ctx, "haru-4b")
	if err != nil {
		t.Fatal(err)
	}
	if !sp.Row || sp.Engine != "radiance" || sp.Bin != "/opt/radiance/strix-halo-haru/build/bin/radiance" || sp.Model != filepath.Join(dir, "haru-bf16.rad") || sp.Ctx != 14500 {
		t.Errorf("spec = %+v: want a row-launched radiance spec", sp)
	}
	if sp, _ := r.LaunchSpec(ctx, "d1-2b"); sp.Row {
		t.Errorf("a llama.cpp config must keep its llama-server launch: %+v", sp)
	}

	row, err := r.LaunchRow(ctx, "haru-4b", "s7", 8106)
	if err != nil {
		t.Fatal(err)
	}
	argv := " " + strings.Join(row.Argv, " ") + " "
	for _, want := range []string{" --port 8106 ", " --served-model-name haru-4b ", " --max-model-len 14500 ", " --duplex-max-lanes 2 ", " --host 127.0.0.1 "} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q is missing %q", argv, want)
		}
	}
	if row.Slot != "s7" || row.Port != 8106 || row.Exe != "/opt/radiance/strix-halo-haru/build/bin/radiance" || row.EngineKind != "radiance" || row.ConfigName != "haru-4b" ||
		row.NCtx != 14500 || row.Env["RADIANCE_HOME"] != "/opt/radiance/strix-halo-haru/build/radiance_home" || row.Env["RADIANCE_KERNELS"] != "libhalo" {
		t.Errorf("row = %+v", row)
	}
	if row.ConfigID == nil || *row.ConfigID != 9 {
		t.Errorf("config id = %v, want 9", row.ConfigID)
	}
	// A bad slot name or a config that is not a model refuses, and writes nothing (building is pure).
	if _, err := r.LaunchRow(ctx, "haru-4b", "S7!", 8106); err == nil {
		t.Error("an invalid slot name was accepted")
	}
	if _, err := r.LaunchRow(ctx, "no-such", "s7", 8106); err == nil {
		t.Error("an unknown config was accepted")
	}
	// The flags the pool owns are refused by the launch builder, so a config cannot move the engine off its port.
	cfg.Modes["haru-4b"].Services[0].ExtraArgs = append(cfg.Modes["haru-4b"].Services[0].ExtraArgs, "--port", "9999")
	if _, err := r.LaunchRow(ctx, "haru-4b", "s7", 8106); err == nil {
		t.Error("a config that sets --port was accepted")
	}
}
