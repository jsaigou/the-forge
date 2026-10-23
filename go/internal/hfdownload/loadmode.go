// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Upstream llama.cpp removed --mmap/--no-mmap/--mlock/--direct-io in favor
// of --load-mode (ggml-org/llama.cpp #28334). The September kintsugi builds
// and standard-vulkan accept both spellings; builds from the 2026-10 rebase
// accept only --load-mode; the oldest fork builds (poolside rocm715) accept
// only --no-mmap. A new config must carry the spelling its OWN build
// understands, so the flag is chosen by asking the binary, not by a global
// constant.

// loadModeArgs returns the "load the whole file into memory up front"
// argument tokens (what --no-mmap always meant) for b's binary.
func (s *Service) loadModeArgs(ctx context.Context, b store.Build) []string {
	probe := s.d.ProbeLoadMode
	if probe == nil {
		probe = probeLoadModeBinary
	}
	if b.BinaryPath != "" {
		if supported, known := probe(ctx, b.BinaryPath); known && !supported {
			return []string{"--no-mmap"}
		}
	}
	// Supported, or the probe could not tell (no build resolved, binary not
	// runnable from this process): every current and future upstream-based
	// build takes --load-mode, so that is the safe default.
	return []string{"--load-mode", "none"}
}

type loadModeProbeKey struct {
	path string
	size int64
	mod  int64
}

var (
	loadModeProbeMu    sync.Mutex
	loadModeProbeCache = map[loadModeProbeKey]bool{}
)

// probeLoadModeBinary runs `<bin> --help` and reports whether the binary
// advertises --load-mode. known=false means the probe itself failed (the
// caller must not treat that as "unsupported"). Results are cached per
// path+size+mtime, so a rebuilt binary is re-probed.
func probeLoadModeBinary(ctx context.Context, path string) (supported, known bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	key := loadModeProbeKey{path, fi.Size(), fi.ModTime().UnixNano()}
	loadModeProbeMu.Lock()
	v, ok := loadModeProbeCache[key]
	loadModeProbeMu.Unlock()
	if ok {
		return v, true
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--help").CombinedOutput()
	text := string(out)
	// --help exits 0 on llama-server, but accept any run that produced the
	// option table: the signal is the text, not the exit status.
	if err != nil && !strings.Contains(text, "--ctx-size") {
		return false, false
	}
	supported = strings.Contains(text, "--load-mode")
	loadModeProbeMu.Lock()
	loadModeProbeCache[key] = supported
	loadModeProbeMu.Unlock()
	return supported, true
}
