// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
)

// TestInferSlotMode_AliasWinsOverSiblingModelPath is the regression test for
// the 2026-08-25 incident: two same-weights sibling configs (sharing one
// GGUF, differing only in generation flags — e.g. a "-nothink" variant vs.
// its base) must always resolve to the mode whose alias actually matches,
// never to whichever sibling Go's randomized map iteration happens to visit
// first. Run repeatedly because the bug only manifested on some map
// iteration orders — a single call could pass by luck even on the broken
// code.
func TestInferSlotMode_AliasWinsOverSiblingModelPath(t *testing.T) {
	cfg := &config.Config{Modes: map[string]config.Mode{
		"gemma4-26b-a4b": {
			Services: []config.Service{{Model: "gemma4-26b-a4b.gguf", Alias: "gemma4-26b-a4b"}},
		},
		"gemma4-26b-a4b-nothink": {
			Services: []config.Service{{Model: "gemma4-26b-a4b.gguf", Alias: "gemma4-26b-a4b-nothink"}},
		},
	}}
	env := map[string]string{
		"FORGE_MODEL_ALIAS": "gemma4-26b-a4b-nothink",
		"FORGE_MODEL_PATH":  "/models/gemma4-26b-a4b.gguf",
	}
	for i := 0; i < 50; i++ {
		if got := inferSlotMode(cfg, env); got != "gemma4-26b-a4b-nothink" {
			t.Fatalf("iteration %d: inferSlotMode = %q, want %q (sibling shares modelPath — alias must win)",
				i, got, "gemma4-26b-a4b-nothink")
		}
	}

	// The reverse case: the base config's alias must win over its own
	// "-nothink" sibling just as reliably.
	env2 := map[string]string{
		"FORGE_MODEL_ALIAS": "gemma4-26b-a4b",
		"FORGE_MODEL_PATH":  "/models/gemma4-26b-a4b.gguf",
	}
	for i := 0; i < 50; i++ {
		if got := inferSlotMode(cfg, env2); got != "gemma4-26b-a4b" {
			t.Fatalf("iteration %d: inferSlotMode = %q, want %q", i, got, "gemma4-26b-a4b")
		}
	}
}

// TestInferSlotMode_ModelPathFallbackWhenAliasUnknown covers the legitimate
// fallback path: an env file whose alias doesn't match any configured mode
// (e.g. stale from before a catalog rename) still resolves via modelPath.
func TestInferSlotMode_ModelPathFallbackWhenAliasUnknown(t *testing.T) {
	cfg := &config.Config{Modes: map[string]config.Mode{
		"swallow-8b": {
			Services: []config.Service{{Model: "swallow-8b.gguf", Alias: "swallow-8b"}},
		},
	}}
	env := map[string]string{
		"FORGE_MODEL_ALIAS": "renamed-away-alias",
		"FORGE_MODEL_PATH":  "/models/swallow-8b.gguf",
	}
	if got := inferSlotMode(cfg, env); got != "swallow-8b" {
		t.Fatalf("inferSlotMode = %q, want %q", got, "swallow-8b")
	}
}

// TestInferSlotMode_NoSignal confirms an env file with neither FORGE_ var
// set (or matching nothing at all) resolves to "" rather than guessing.
func TestInferSlotMode_NoSignal(t *testing.T) {
	cfg := &config.Config{Modes: map[string]config.Mode{
		"swallow-8b": {
			Services: []config.Service{{Model: "swallow-8b.gguf", Alias: "swallow-8b"}},
		},
	}}
	if got := inferSlotMode(cfg, map[string]string{}); got != "" {
		t.Fatalf("inferSlotMode(empty env) = %q, want \"\"", got)
	}
	env := map[string]string{
		"FORGE_MODEL_ALIAS": "unknown-alias",
		"FORGE_MODEL_PATH":  "/models/unknown.gguf",
	}
	if got := inferSlotMode(cfg, env); got != "" {
		t.Fatalf("inferSlotMode(no match) = %q, want \"\"", got)
	}
}
