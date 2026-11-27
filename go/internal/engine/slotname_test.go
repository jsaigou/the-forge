// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"testing"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// TestModeFromRow_SiblingsResolveByConfigName replaces the 2026-08-25 alias-vs-model-path inference tests: the row's config_name is the exact catalog
// config, so two same-weights siblings can never be confused, whatever the map iteration order.
func TestModeFromRow_SiblingsResolveByConfigName(t *testing.T) {
	cfg := &config.Config{Modes: map[string]config.Mode{
		"gemma4-26b-a4b":         {Services: []config.Service{{Model: "gemma4-26b-a4b.gguf", Alias: "gemma4-26b-a4b"}}},
		"gemma4-26b-a4b-nothink": {Services: []config.Service{{Model: "gemma4-26b-a4b.gguf", Alias: "gemma4-26b-a4b-nothink"}}},
	}}
	for i := 0; i < 50; i++ {
		for _, name := range []string{"gemma4-26b-a4b-nothink", "gemma4-26b-a4b"} {
			row := store.SlotLaunch{ConfigName: name, ModelPath: "/models/gemma4-26b-a4b.gguf"}
			if got := modeFromRow(cfg, row); got != name {
				t.Fatalf("iteration %d: modeFromRow = %q, want %q", i, got, name)
			}
		}
	}
}

// A row naming a config the catalog no longer has resolves to "" rather than guessing.
func TestModeFromRow_UnknownConfig(t *testing.T) {
	cfg := &config.Config{Modes: map[string]config.Mode{"swallow-8b": {}}}
	if got := modeFromRow(cfg, store.SlotLaunch{ConfigName: "renamed-away"}); got != "" {
		t.Fatalf("modeFromRow(unknown) = %q, want \"\"", got)
	}
	if got := modeFromRow(cfg, store.SlotLaunch{}); got != "" {
		t.Fatalf("modeFromRow(empty) = %q, want \"\"", got)
	}
}

// A disarmed row is history: SlotStates must not read an active unit's mode from it (a stale record is never truth).
func TestSlotStatesIgnoresDisarmedRow(t *testing.T) {
	cfg := testConfig(t)
	writeSlotEnv(t, cfg, "a4", "qwen", "qwen.gguf")
	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)
	if _, err := seededRowDB(t, cfg).SlotLaunches().Disarm(t.Context(), "a4", 1); err != nil {
		t.Fatal(err)
	}
	got := m.SlotStates(map[string]collector.UnitState{"forge-a4": st("active", "running")})
	if got["a4"].Mode != "" {
		t.Errorf("mode = %q from a disarmed row, want \"\"", got["a4"].Mode)
	}
	// Footprint fallback still knows the slot's last weights.
	if mp, _ := m.SlotWeightPaths("a4"); mp == "" {
		t.Error("SlotWeightPaths lost the last launch's model path")
	}
}
