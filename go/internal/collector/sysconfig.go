// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ReadSlotEnv parses /etc/sysconfig/forge-<slot>-env into a map — empty
// when absent/unreadable (port of engine._read_slot_env). Exported because
// the engine (same track) writes these files and reads them back for
// reconciliation; the collector reads them for weight accounting.
func ReadSlotEnv(sysconfigDir, slot string) map[string]string {
	env := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(sysconfigDir, "forge-"+slot+"-env"))
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return env
}

// shardRe is the trailing "-\d+-of-\d+\.gguf" pattern that marks a GGUF as
// one shard of a multi-file shard set (e.g.
// "nvidia_Nemotron-3-Super-120B-A12B-Q5_K_L-00001-of-00003.gguf").
var shardRe = regexp.MustCompile(`^(.+)-\d+-of-\d+\.gguf$`)

// shardPrefix returns the shared basename prefix of a shard file (everything
// before the "-NNNN-of-NNNN.gguf" suffix) and true when p is a shard.
func shardPrefix(p string) (string, bool) {
	m := shardRe.FindStringSubmatch(filepath.Base(p))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// shardSetBytes sums the sizes of every *.gguf in dir whose basename begins
// with prefix — the sibling shards of one shard set. Only files of the same
// set are counted, never unrelated ggufs that happen to share the directory
// (a sibling quantization, an mmproj, a different model).
func shardSetBytes(dir, prefix string) int64 {
	shards, err := filepath.Glob(filepath.Join(dir, prefix+"-*.gguf"))
	if err != nil {
		return 0
	}
	var total int64
	for _, s := range shards {
		if st, err := os.Stat(s); err == nil && st.Mode().IsRegular() {
			total += st.Size()
		}
	}
	return total
}

// WeightSetSizeBytes returns the on-disk size (bytes) of a model file plus
// an optional mmproj file (port of engine._weight_set_size_mb, now bytes —
// A1 retrofit).
//
// Size rule (found live 2026-08-20 on qwen38-27b: a directory holding two
// sibling quantizations AND an mmproj — Q5_K_M + UD-Q8_K_XL + mmproj, 52.2GB —
// was summed whole for the model path and again for the mmproj path, reporting
// ~97 GiB for a 19.8 GiB model and blocking every load):
//   - a shard file (-NNNN-of-NNNN.gguf) counts its whole shard set — but only
//     siblings sharing its basename prefix, never every *.gguf in the dir;
//   - any other file counts exactly itself (a directory of unrelated ggufs —
//     sibling quants, an mmproj — is NOT summed wholesale);
//   - the model and mmproj passes de-duplicate, so a directory shared by both
//     (the common layout) is never summed twice.
func WeightSetSizeBytes(modelPath, mmprojPath, modelsDir string) int64 {
	seenFiles := map[string]bool{}
	seenSets := map[string]bool{}
	var total int64
	for _, p := range []string{modelPath, mmprojPath} {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		if prefix, isShard := shardPrefix(p); isShard {
			setKey := filepath.Dir(p) + "\x00" + prefix
			if !seenSets[setKey] {
				seenSets[setKey] = true
				total += shardSetBytes(filepath.Dir(p), prefix)
			}
			continue
		}
		if seenFiles[p] {
			continue
		}
		seenFiles[p] = true
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			total += st.Size()
		}
	}
	return total
}

// ActiveWeightsBytes sums the on-disk size (bytes) of every model + mmproj
// referenced by the sysconfig env files of the given slots, deduplicating
// shared files and shard sets (port of engine._get_all_model_sizes_mb, now
// bytes — A1 retrofit). Applies the same per-file/per-shard-set size rule as
// WeightSetSizeBytes, so a directory holding multiple distinct artifacts is
// never summed wholesale.
func ActiveWeightsBytes(sysconfigDir, modelsDir string, slots []string) int64 {
	seenFiles := map[string]bool{}
	seenSets := map[string]bool{}
	var total int64
	for _, slot := range slots {
		env := ReadSlotEnv(sysconfigDir, slot)
		for _, key := range []string{"FORGE_MODEL_PATH", "FORGE_MMPROJ"} {
			p := env[key]
			if p == "" {
				continue
			}
			p = filepath.Clean(p)
			if prefix, isShard := shardPrefix(p); isShard {
				setKey := filepath.Dir(p) + "\x00" + prefix
				if !seenSets[setKey] {
					seenSets[setKey] = true
					total += shardSetBytes(filepath.Dir(p), prefix)
				}
				continue
			}
			if seenFiles[p] {
				continue
			}
			seenFiles[p] = true
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				total += st.Size()
			}
		}
	}
	return total
}
