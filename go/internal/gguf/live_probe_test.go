// SPDX-License-Identifier: Apache-2.0

package gguf

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveHeaders parses real GGUF header excerpts when GGUF_LIVE_DIR is
// set (local live-verification harness; skipped in CI).
func TestLiveHeaders(t *testing.T) {
	dir := os.Getenv("GGUF_LIVE_DIR")
	if dir == "" {
		t.Skip("GGUF_LIVE_DIR not set")
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.gguf"))
	for _, p := range paths {
		t0 := time.Now()
		md, err := ReadMetadata(p)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
			continue
		}
		t.Logf("%s: %s arch=%q name=%q ctx=%d quant=%q params=%d",
			filepath.Base(p), time.Since(t0), md.Architecture, md.Name, md.TrainedCtx, md.QuantType, md.ParameterCount)
		t.Logf("  layers: block_count=%d embd=%d head_count=%d head_count_kv=%v",
			md.BlockCount, md.EmbeddingLength, md.HeadCount, md.HeadCountKV)
		t.Logf("  attn: key_len=%d val_len=%d key_len_swa=%d val_len_swa=%d sliding_window=%d swa_pattern=%v hybrid=%v",
			md.KeyLength, md.ValueLength, md.KeyLengthSWA, md.ValueLengthSWA, md.SlidingWindow, md.SWAPattern, md.Hybrid)
	}
}
