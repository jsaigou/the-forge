// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// writeGGUF creates a regular file of n bytes and returns its path.
func writeGGUF(t *testing.T, path string, n int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWeightSetSizeBytes_SharedDirNotSummedWholesale is the qwen38-27b
// regression (found live 2026-08-20): one model directory holds two sibling
// quantizations AND an mmproj. The old code summed the whole directory once
// for the model path and again for the mmproj path (both in the same dir),
// reporting ~2x the whole dir — ~97 GiB for a 19.8 GiB Q5_K_M model.
func TestWeightSetSizeBytes_SharedDirNotSummedWholesale(t *testing.T) {
	models := t.TempDir()
	dir := filepath.Join(models, "qwen3.8-27b")
	q5 := filepath.Join(dir, "Qwen3.8-27B-Q5_K_M.gguf")
	q8 := filepath.Join(dir, "Qwen3.8-27B-UD-Q8_K_XL.gguf")
	mm := filepath.Join(dir, "mmproj-BF16.gguf")
	writeGGUF(t, q5, 1000)
	writeGGUF(t, q8, 2000)
	writeGGUF(t, mm, 300)

	// Referenced weight set: Q5_K_M + mmproj. The sibling UD-Q8_K_XL quant and
	// the duplicate pass over the shared directory must NOT be counted.
	got := WeightSetSizeBytes(q5, mm, models)
	want := int64(1300)
	if got != want {
		t.Errorf("WeightSetSizeBytes = %d, want %d (model+mmproj only, no sibling quant, no double count)", got, want)
	}
}

// TestWeightSetSizeBytes_ShardSetSumsSiblingsOnly verifies a genuine
// multi-file shard set (nemotron-style "-00001-of-00003") counts all its
// shards but not unrelated ggufs in the same directory.
func TestWeightSetSizeBytes_ShardSetSumsSiblingsOnly(t *testing.T) {
	models := t.TempDir()
	dir := filepath.Join(models, "nemotron-super", "Q5_K_L", "nvidia_Nemotron-3-Super-120B-A12B-Q5_K_L")
	base := filepath.Join(dir, "nvidia_Nemotron-3-Super-120B-A12B-Q5_K_L")
	for i, suffix := range []string{"-00001-of-00003", "-00002-of-00003", "-00003-of-00003"} {
		writeGGUF(t, base+suffix+".gguf", int64(1000*(i+1)))
	}
	// An unrelated gguf in the same directory must be excluded.
	writeGGUF(t, filepath.Join(dir, "some-other-model-Q8_0.gguf"), 90000)

	got := WeightSetSizeBytes(base+"-00001-of-00003.gguf", "", models)
	want := int64(1000 + 2000 + 3000)
	if got != want {
		t.Errorf("WeightSetSizeBytes(shard) = %d, want %d (all 3 shards, no unrelated gguf)", got, want)
	}
}

// TestWeightSetSizeBytes_ShardedModelAndMMProjSharedDir verifies a sharded
// model whose mmproj lives in the same directory is not double-counted (the
// shard set is summed once, the mmproj file exactly once).
func TestWeightSetSizeBytes_ShardedModelAndMMProjSharedDir(t *testing.T) {
	models := t.TempDir()
	dir := filepath.Join(models, "puzzle-75b", "Q4_K_M")
	base := filepath.Join(dir, "Puzzle-75B-A9B-Q4_K_M")
	writeGGUF(t, base+"-00001-of-00002.gguf", 4000)
	writeGGUF(t, base+"-00002-of-00002.gguf", 5000)
	mm := filepath.Join(dir, "mmproj-Puzzle-75B.gguf")
	writeGGUF(t, mm, 600)

	got := WeightSetSizeBytes(base+"-00001-of-00002.gguf", mm, models)
	want := int64(4000 + 5000 + 600)
	if got != want {
		t.Errorf("WeightSetSizeBytes(sharded+mmproj) = %d, want %d (shards once + mmproj once)", got, want)
	}
}

// TestWeightSetSizeBytes_AbsolutePathOutsideModelsDir pins the 2026-07-25
// regression (TestFitPlanAbsoluteModelPath in the engine): an absolute model
// path outside modelsDir must be stat'd exactly, not joined under ModelsDir
// and not globbed as a shard directory.
func TestWeightSetSizeBytes_AbsolutePathOutsideModelsDir(t *testing.T) {
	models := t.TempDir()
	absDir := t.TempDir()
	absPath := filepath.Join(absDir, "laguna-s-21-Q4_K_M.gguf")
	writeGGUF(t, absPath, 700)

	got := WeightSetSizeBytes(absPath, "", models)
	if got != 700 {
		t.Errorf("WeightSetSizeBytes(abs path) = %d, want 700", got)
	}
}

// TestActiveWeightsBytes_SharedDirAcrossSlots verifies ActiveWeightsBytes
// counts a shared directory's referenced files exactly once per slot, without
// summing wholesale sibling artifacts or double-counting a shared mmproj.
func TestActiveWeightsBytes_SharedDirAcrossSlots(t *testing.T) {
	models := t.TempDir()
	sys := t.TempDir()
	dir := filepath.Join(models, "qwen3.8-27b")
	q5 := filepath.Join(dir, "Qwen3.8-27B-Q5_K_M.gguf")
	q8 := filepath.Join(dir, "Qwen3.8-27B-UD-Q8_K_XL.gguf")
	mm := filepath.Join(dir, "mmproj-BF16.gguf")
	writeGGUF(t, q5, 1000)
	writeGGUF(t, q8, 2000)
	writeGGUF(t, mm, 300)

	env := "FORGE_MODEL_PATH=" + q5 + "\nFORGE_MMPROJ=" + mm + "\n"
	if err := os.WriteFile(filepath.Join(sys, "forge-a1-env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ActiveWeightsBytes(sys, models, []string{"a1"})
	want := int64(1300)
	if got != want {
		t.Errorf("ActiveWeightsBytes = %d, want %d (model+mmproj once)", got, want)
	}
}
