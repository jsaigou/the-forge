// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/gguf"
)

// gemmaLikeMetadata mirrors the real gemma4-26b-a4b GGUF ground-truthed on
// ForgeHost during the 2026-09-07 incident investigation: 30 layers, a 5 SWA :
// 1 global repeating pattern, key/value_length 512 (global) vs. 256 (SWA),
// sliding_window 1024.
func gemmaLikeMetadata() gguf.Metadata {
	const layers = 30
	headCountKV := make([]int, layers)
	pattern := make([]bool, layers)
	for i := 0; i < layers; i++ {
		if i%6 == 5 {
			headCountKV[i] = 2 // global layer
			pattern[i] = false
		} else {
			headCountKV[i] = 8 // SWA layer
			pattern[i] = true
		}
	}
	return gguf.Metadata{
		Architecture:    "gemma4",
		BlockCount:      layers,
		EmbeddingLength: 2816,
		HeadCount:       16,
		HeadCountKV:     headCountKV,
		KeyLength:       512,
		ValueLength:     512,
		KeyLengthSWA:    256,
		ValueLengthSWA:  256,
		SlidingWindow:   1024,
		SWAPattern:      pattern,
	}
}

// gib is a var, not a const: several tests multiply it by a non-integer
// float literal and truncate back to int64 at runtime — Go rejects that
// conversion at compile time for a constant expression.
var gib = int64(1) << 30

func TestKVCacheBytesGemmaSWAFull(t *testing.T) {
	meta := gemmaLikeMetadata()
	bpe := cacheBytesPerElem["q8_0"]
	got, ok := kvCacheBytes(meta, 262144, bpe, bpe, true)
	if !ok {
		t.Fatal("kvCacheBytes returned ok=false")
	}
	// Hand-calculated: 25 SWA layers + 5 global layers, all materialized to
	// the full 262144 context under --swa-full ⇒ ~29.2 GiB.
	want := int64(29.21875 * float64(gib))
	tolerance := gib / 100 // within ~10 MiB
	if diff := got - want; diff < -tolerance || diff > tolerance {
		t.Errorf("got %.3f GiB, want ~%.3f GiB (swa-full)", float64(got)/float64(gib), float64(want)/float64(gib))
	}
}

// Without --swa-full, SWA layers are windowed (1024 cells, not 262144) —
// the resulting estimate must be dramatically smaller, illustrating exactly
// why the 2026-09-07 incident's config (which DOES set --swa-full) needed
// the full-context term and a flat weight-only guess could never see it.
func TestKVCacheBytesGemmaWindowed(t *testing.T) {
	meta := gemmaLikeMetadata()
	bpe := cacheBytesPerElem["q8_0"]
	got, ok := kvCacheBytes(meta, 262144, bpe, bpe, false)
	if !ok {
		t.Fatal("kvCacheBytes returned ok=false")
	}
	want := int64(2.76 * float64(gib))
	tolerance := gib / 20 // within ~50 MiB — window padding leaves more slack
	if diff := got - want; diff < -tolerance || diff > tolerance {
		t.Errorf("got %.3f GiB, want ~%.3f GiB (windowed)", float64(got)/float64(gib), float64(want)/float64(gib))
	}
	fullResult, _ := kvCacheBytes(meta, 262144, bpe, bpe, true)
	if got >= fullResult {
		t.Errorf("windowed (%d) must be far smaller than swa-full (%d)", got, fullResult)
	}
}

// A plain dense/GQA model (no SWA pattern at all) reduces to the standard
// 2 * n_layer * n_head_kv * head_dim * n_ctx * bytes_per_elem formula.
func TestKVCacheBytesPlainGQA(t *testing.T) {
	const layers, kvHeads, headDim, nCtx = 32, 8, 128, 8192
	meta := gguf.Metadata{
		BlockCount:  layers,
		HeadCountKV: repeatInt(kvHeads, layers),
		KeyLength:   headDim,
		ValueLength: headDim,
	}
	bpe := cacheBytesPerElem["f16"]
	got, ok := kvCacheBytes(meta, nCtx, bpe, bpe, false)
	if !ok {
		t.Fatal("kvCacheBytes returned ok=false")
	}
	want := int64(layers) * int64(kvHeads) * int64(headDim) * int64(nCtx) * 2 /* K+V */ * int64(bpe)
	if got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

// The embedding_length/head_count fallback (llama.cpp's own default when
// key_length/value_length are absent — llama-model.cpp:1329-1333) must be
// used, not a refusal, when the explicit lengths aren't in the file.
func TestKVCacheBytesEmbeddingHeadCountFallback(t *testing.T) {
	const layers, kvHeads, nHead, embd, nCtx = 4, 4, 32, 4096, 8192
	meta := gguf.Metadata{
		BlockCount:      layers,
		HeadCountKV:     repeatInt(kvHeads, layers),
		HeadCount:       nHead,
		EmbeddingLength: embd,
		// KeyLength/ValueLength deliberately absent.
	}
	bpe := cacheBytesPerElem["f16"]
	got, ok := kvCacheBytes(meta, nCtx, bpe, bpe, false)
	if !ok {
		t.Fatal("kvCacheBytes returned ok=false, want the embd/head_count fallback to resolve")
	}
	headDim := embd / nHead
	want := int64(layers) * int64(kvHeads) * int64(headDim) * int64(nCtx) * 2 * int64(bpe)
	if got != want {
		t.Errorf("got %d, want %d (head_dim=%d via fallback)", got, want, headDim)
	}
}

func TestKVCacheBytesHybridAbstains(t *testing.T) {
	meta := gemmaLikeMetadata()
	meta.Hybrid = true
	if _, ok := kvCacheBytes(meta, 262144, cacheBytesPerElem["q8_0"], cacheBytesPerElem["q8_0"], true); ok {
		t.Error("ok = true, want false for a hybrid/recurrent architecture")
	}
}

func TestKVCacheBytesMissingLayerDataAbstains(t *testing.T) {
	cases := []struct {
		name string
		meta gguf.Metadata
	}{
		{"no block count", gguf.Metadata{HeadCountKV: []int{8}, KeyLength: 128, ValueLength: 128}},
		{"head_count_kv length mismatch", gguf.Metadata{BlockCount: 4, HeadCountKV: []int{8, 8}, KeyLength: 128, ValueLength: 128}},
		{"no key/value length or fallback", gguf.Metadata{BlockCount: 4, HeadCountKV: repeatInt(8, 4)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := kvCacheBytes(c.meta, 8192, 2, 2, false); ok {
				t.Error("ok = true, want false")
			}
		})
	}
}

func repeatInt(v, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestParseCacheTypesDefaultsToF16(t *testing.T) {
	k, v := parseCacheTypes(nil)
	if k != "f16" || v != "f16" {
		t.Errorf("got %q/%q, want f16/f16", k, v)
	}
}

func TestParseCacheTypesReadsFlags(t *testing.T) {
	args := []string{"--jinja", "--cache-type-k", "q8_0", "--cache-type-v", "q8_0", "--flash-attn", "on"}
	k, v := parseCacheTypes(args)
	if k != "q8_0" || v != "q8_0" {
		t.Errorf("got %q/%q, want q8_0/q8_0", k, v)
	}
}

func TestHasFlagDetectsSWAFull(t *testing.T) {
	if !hasFlag([]string{"--jinja", "--swa-full"}, "--swa-full") {
		t.Error("hasFlag = false, want true")
	}
	if hasFlag([]string{"--jinja"}, "--swa-full") {
		t.Error("hasFlag = true, want false")
	}
}

// writeSizedFile creates a file of exactly size bytes (sparse — content
// doesn't matter, only os.Stat's reported size, which is all
// modeWeightBytes/collector.WeightSetSizeBytes reads).
func writeSizedFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

// TestModeNeedEstimateFloorsCuratedFigure reproduces the 2026-09-07
// incident's exact shape at the modeNeedEstimate level: a curated
// safe_memory_bytes far below the real weights+KV need for a
// 262144-context, --swa-full, q8_0-cache mode. The curated figure must
// never win over the computed floor.
func TestModeNeedEstimateFloorsCuratedFigure(t *testing.T) {
	cfg := testConfig(t)
	modelPath := filepath.Join(cfg.Paths.ModelsDir, "gemma4.gguf")
	const weightBytes = int64(18) << 30 // stand-in for the real ~18 GiB weight set
	writeSizedFile(t, modelPath, weightBytes)

	mode := cfg.Modes["gemma"]
	mode.ConfigID = 1
	mode.Services = []config.Service{{
		Model:     "gemma4.gguf",
		Alias:     "gemma",
		Context:   262144,
		PortRole:  "a1",
		Backend:   "vulkan",
		ExtraArgs: []string{"--swa-full", "--cache-type-k", "q8_0", "--cache-type-v", "q8_0"},
	}}
	cfg.Modes["gemma"] = mode

	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil)
	meta := gemmaLikeMetadata()
	m.d.ReadMeta = func(path string) (gguf.Metadata, error) {
		if path == modelPath {
			return meta, nil
		}
		return gguf.Metadata{}, nil
	}
	const curatedBytes = int64(25) << 30 // the real incident's exact wrong figure
	m.d.WeightEstimateBytes = func(configID int64) (int64, bool) {
		if configID == 1 {
			return curatedBytes, true
		}
		return 0, false
	}

	got, ok := m.modeNeedEstimate(cfg, "gemma")
	if !ok {
		t.Fatal("modeNeedEstimate returned ok=false")
	}
	if got <= curatedBytes {
		t.Fatalf("need estimate = %.1f GiB, must exceed the curated %.1f GiB (computed KV floor must win)",
			float64(got)/float64(gib), float64(curatedBytes)/float64(gib))
	}
	// Sanity: should land near weights (~18 GiB) + the swa-full KV estimate
	// (~29.2 GiB) — i.e. real operating range, not a small nudge above 25.
	wantApprox := weightBytes + int64(29.21875*float64(gib))
	tolerance := gib // 1 GiB slack
	if diff := got - wantApprox; diff < -tolerance || diff > tolerance {
		t.Errorf("need estimate = %.1f GiB, want ~%.1f GiB (weights + swa-full KV)",
			float64(got)/float64(gib), float64(wantApprox)/float64(gib))
	}
}

// TestModeNeedEstimateUnaffectedWhenFormulaCantResolve confirms existing
// unprofiled-mode behavior (curated figure, or file size) is completely
// unchanged when the KV formula can't resolve — e.g. every existing engine
// test's fake ReadMeta, which returns a bare gguf.Metadata{TrainedCtx: ...}
// with no layer data.
func TestModeNeedEstimateUnaffectedWhenFormulaCantResolve(t *testing.T) {
	cfg := testConfig(t)
	mode := cfg.Modes["gemma"]
	mode.ConfigID = 1
	cfg.Modes["gemma"] = mode

	m, _, _ := newTestManager(t, cfg, newFakeSys(), nil) // default fake ReadMeta: bare TrainedCtx, no BlockCount
	const curatedBytes = int64(25) << 30
	m.d.WeightEstimateBytes = func(configID int64) (int64, bool) {
		if configID == 1 {
			return curatedBytes, true
		}
		return 0, false
	}

	got, ok := m.modeNeedEstimate(cfg, "gemma")
	if !ok {
		t.Fatal("modeNeedEstimate returned ok=false")
	}
	if got != curatedBytes {
		t.Errorf("got %d, want the curated figure %d unchanged (formula must abstain, not guess)", got, curatedBytes)
	}
}
