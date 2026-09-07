// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"os"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/gguf"
)

// 2026-09-07 incident: gemma4-26b-a4b-nothink's catalog safe_memory_bytes
// (25 GiB, weight-adjacent, no KV term) let the fit check admit a load that
// actually needed ~40 GiB, while qwen38-flash-next's real ~90 GiB footprint
// in the other slot left only ~25.5 GiB genuinely free — the host OOM-killed
// several unrelated services before self-healing. modeNeedEstimate's three
// tiers (profile → curated catalog figure → bare weight-file size) had no
// context-dependent term anywhere, so any large-context unprofiled mode was
// exposed the same way. This file computes a real weights+KV-cache floor
// from GGUF metadata so a stale/optimistic curated number can never win.
//
// The formula is ported directly from the kintsugi fork's own C++ (verified
// 2026-09-07 against /opt/forge/llama.cpp-kintsugi/src/), not derived by
// guesswork:
//   - llama-hparams.cpp: n_embd_k_gqa(il) = n_embd_head_k(il) * n_head_kv(il),
//     n_embd_head_k(il) = is_swa(il) ? key_length_swa : key_length (V mirrors K).
//     is_swa(il) comes straight from the GGUF's per-layer
//     attention.sliding_window_pattern array when the file declares one.
//   - llama-model.cpp:1329-1333: when key_length/value_length are absent,
//     llama.cpp itself falls back to embedding_length / head_count — that
//     exact fallback, not a guess, is mirrored below.
//   - llama-kv-cache-iswa.cpp: per layer, k/v tensors are sized
//     [n_embd_k_gqa(il), cells]. cells = n_ctx for non-SWA layers always,
//     and for SWA layers too when --swa-full is set
//     (`if (swa_full) { size_swa = size_base; }` — verified directly).
//     Without --swa-full, SWA layers are windowed:
//     size_swa = min(n_ctx, sliding_window + n_ubatch) padded to 256.
//
// Deliberately NOT covered: hybrid/recurrent architectures (Nemotron's
// Mamba2/attention mix, the kintsugi fork's own experimental qwen4exp /
// Qwen3.8-Flash-Next with SSM state + a block-sparse indexer cache). A flat
// per-token guess used to cover unprofiled modes and was deleted after it
// inflated Nemotron's real need by ~24x (memory.go's modeNeedEstimate doc).
// This estimator inherits that lesson: gguf.Metadata.Hybrid (set from any
// <arch>.ssm.* or <arch>.attention.indexer.* key) makes it abstain
// (ok=false) rather than apply a formula that doesn't model those
// architectures. Those modes stay on today's curated-or-refuse path, which
// is already the correct fail-closed behavior for what we can't model —
// real PROFILE measurements are the sanctioned way to size them.

// cacheBytesPerElem is bytes-per-element for the KV-cache quant types
// llama.cpp's --cache-type-k/--cache-type-v accept, verified against
// ggml.c's type_traits table (type_size / blck_size):
//
//	f32=4, f16=2, bf16=2, q8_0=34/32, q4_0=18/32, q4_1=20/32, q5_0=22/32, q5_1=24/32.
var cacheBytesPerElem = map[string]float64{
	"f32":  4,
	"f16":  2,
	"bf16": 2,
	"q8_0": 34.0 / 32.0,
	"q4_0": 18.0 / 32.0,
	"q4_1": 20.0 / 32.0,
	"q5_0": 22.0 / 32.0,
	"q5_1": 24.0 / 32.0,
}

// parseCacheTypes pulls --cache-type-k/--cache-type-v from a mode's
// extra_args, defaulting to llama.cpp's own default ("f16") when absent.
func parseCacheTypes(extraArgs []string) (kType, vType string) {
	kType, vType = "f16", "f16"
	for i, a := range extraArgs {
		switch a {
		case "--cache-type-k":
			if i+1 < len(extraArgs) {
				kType = extraArgs[i+1]
			}
		case "--cache-type-v":
			if i+1 < len(extraArgs) {
				vType = extraArgs[i+1]
			}
		}
	}
	return kType, vType
}

// hasFlag reports whether extraArgs contains a bare flag (e.g. --swa-full).
func hasFlag(extraArgs []string, name string) bool {
	for _, a := range extraArgs {
		if a == name {
			return true
		}
	}
	return false
}

// parseParallelArg extracts --parallel's value (default 1, matching
// llama.cpp's own default). Small enough to duplicate rather than export
// profile.parseParallel across packages for one helper.
func parseParallelArg(extraArgs []string) int {
	for i, a := range extraArgs {
		if a == "--parallel" && i+1 < len(extraArgs) {
			if n, err := strconv.Atoi(extraArgs[i+1]); err == nil && n > 0 {
				return n
			}
		}
		if strings.HasPrefix(a, "--parallel=") {
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--parallel=")); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}

// padTo256 mirrors llama.cpp's GGML_PAD(x, 256) for the windowed-SWA cell
// count (llama-kv-cache-iswa.cpp: "the SWA cache is always padded to 256").
func padTo256(n int) int {
	const pad = 256
	return ((n + pad - 1) / pad) * pad
}

// kvCacheBytes computes the real KV-cache tensor footprint for one mode's
// configured context. nCtx is the per-sequence cell count actually passed
// to llama-server (this repo enforces --parallel 1 for any full-context
// mode — see CLAUDE.md's "--parallel" note — so the configured Context
// value already IS that cell count; --parallel>1 modes are short-context
// workers where erring high is the safe direction anyway).
//
// ok=false — never a guess — when the architecture is hybrid/recurrent
// (meta.Hybrid) or required layer metadata can't be resolved even via
// llama.cpp's own embedding_length/head_count fallback.
func kvCacheBytes(meta gguf.Metadata, nCtx int, kBpe, vBpe float64, swaFull bool) (int64, bool) {
	if meta.Hybrid || meta.BlockCount <= 0 || nCtx <= 0 {
		return 0, false
	}
	if len(meta.HeadCountKV) != meta.BlockCount {
		return 0, false
	}

	keyLen, valLen := meta.KeyLength, meta.ValueLength
	if keyLen <= 0 && meta.HeadCount > 0 && meta.EmbeddingLength > 0 {
		keyLen = meta.EmbeddingLength / meta.HeadCount
	}
	if valLen <= 0 && meta.HeadCount > 0 && meta.EmbeddingLength > 0 {
		valLen = meta.EmbeddingLength / meta.HeadCount
	}
	if keyLen <= 0 || valLen <= 0 {
		return 0, false
	}

	hasSWAPattern := len(meta.SWAPattern) == meta.BlockCount

	var total float64
	for il := 0; il < meta.BlockCount; il++ {
		isSWA := hasSWAPattern && meta.SWAPattern[il]

		hdK, hdV := keyLen, valLen
		if isSWA && meta.KeyLengthSWA > 0 {
			hdK = meta.KeyLengthSWA
		}
		if isSWA && meta.ValueLengthSWA > 0 {
			hdV = meta.ValueLengthSWA
		}

		cells := nCtx
		if isSWA && !swaFull {
			window := meta.SlidingWindow
			if window <= 0 || window > nCtx {
				window = nCtx
			} else {
				window = padTo256(window)
				if window > nCtx {
					window = nCtx
				}
			}
			cells = window
		}

		nHeadKV := meta.HeadCountKV[il]
		total += float64(cells) * float64(nHeadKV) * (float64(hdK)*kBpe + float64(hdV)*vBpe)
	}
	return int64(total), true
}

// kvAwareNeedBytes returns a mode's weights + real-KV-cache floor: the
// on-disk weight set (modeWeightBytes, already used by the file-size
// fallback tier) plus kvCacheBytes for its configured context and cache
// quant. ok=false when the model's GGUF can't be read or the architecture
// can't be modeled (kvCacheBytes' own abstention) — callers must treat that
// as "no computed floor," never as zero.
func (m *Manager) kvAwareNeedBytes(modeName string) (int64, bool) {
	cfg := m.d.Cfg()
	mode, ok := cfg.Modes[modeName]
	if !ok || len(mode.Services) == 0 || mode.Services[0].Model == "" {
		return 0, false
	}
	svc := mode.Services[0]

	modelPath := cfg.Paths.ResolveModelPath(svc.Model)
	meta, err := m.readMetaCached(modelPath)
	if err != nil {
		return 0, false
	}

	nCtx := svc.Context / parseParallelArg(svc.ExtraArgs)
	if nCtx <= 0 {
		nCtx = svc.Context
	}
	kName, vName := parseCacheTypes(svc.ExtraArgs)
	kBpe, kOK := cacheBytesPerElem[kName]
	vBpe, vOK := cacheBytesPerElem[vName]
	if !kOK || !vOK {
		return 0, false
	}
	swaFull := hasFlag(svc.ExtraArgs, "--swa-full")

	kvBytes, ok := kvCacheBytes(meta, nCtx, kBpe, vBpe, swaFull)
	if !ok {
		return 0, false
	}

	weightBytes := modeWeightBytes(cfg, modeName)
	if weightBytes <= 0 {
		return 0, false
	}
	return weightBytes + kvBytes, true
}

// readMetaCached wraps Deps.ReadMeta with a (size, mtime)-keyed cache: this
// is a real disk read (see gguf.go's doc comment on why it never touches
// the tensor table, only the header), and FitPlan sits on the scheduler's
// hot decision path — polled every PollInterval while a load is in
// progress. Without this, every poll would re-open and re-scan the
// candidate model's GGUF header. Falls back to an uncached read (matching
// every other ReadMeta call site) when the file can't be stat'd, so fake
// paths in tests behave exactly as before.
func (m *Manager) readMetaCached(path string) (gguf.Metadata, error) {
	st, err := os.Stat(path)
	if err != nil {
		return m.d.ReadMeta(path)
	}

	m.metaMu.Lock()
	if e, ok := m.metaCache[path]; ok && e.size == st.Size() && e.modTime.Equal(st.ModTime()) {
		m.metaMu.Unlock()
		return e.meta, nil
	}
	m.metaMu.Unlock()

	meta, err := m.d.ReadMeta(path)
	if err != nil {
		return meta, err
	}

	m.metaMu.Lock()
	m.metaCache[path] = metaCacheEntry{size: st.Size(), modTime: st.ModTime(), meta: meta}
	m.metaMu.Unlock()
	return meta, nil
}
