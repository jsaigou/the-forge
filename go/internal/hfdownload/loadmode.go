// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	if b.BinaryPath != "" && s.binaryPathProbeable(ctx, b.BinaryPath) {
		if supported, known := probe(ctx, b.BinaryPath); known && !supported {
			return []string{"--no-mmap"}
		}
	}
	// Supported, or the probe could not tell (no build resolved, binary not
	// runnable from this process): every current and future upstream-based
	// build takes --load-mode, so that is the safe default.
	return []string{"--load-mode", "none"}
}

// binaryPathProbeable decides whether a catalog build's binary may be exec'd to learn its flag spelling. The path is
// operator-supplied (the builds API, smith's build_refresh, migrations all write it), so it goes through the same
// build guard the write paths apply (internal/buildguard, ADR-0020 F8) before anything runs it. A rejected path is
// logged and treated as "the probe could not tell", i.e. the safe --load-mode default, not an error.
func (s *Service) binaryPathProbeable(ctx context.Context, path string) bool {
	vet := s.d.VetBinaryPath
	if vet == nil {
		return binaryPathSafe(path) == ""
	}
	if reason := vet(ctx, path); reason != "" {
		s.d.logf("hfdownload: not probing build binary %s: %s (using the --load-mode default)", path, reason)
		return false
	}
	return true
}

// binaryPathSafe is the probe's own floor under that guard, for callers that reach ProbeLoadMode directly and for a
// daemon wired without Deps.VetBinaryPath: absolute, and no shell metacharacters. "" means acceptable.
func binaryPathSafe(path string) string {
	switch {
	case path == "":
		return "empty path"
	case !filepath.IsAbs(path):
		return "not an absolute path"
	case strings.ContainsAny(path, "\t\n;|&$`"):
		return "contains disallowed characters"
	}
	return ""
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
// path+size+mtime, so a rebuilt binary is re-probed. Nothing is exec'd until
// the path clears binaryPathSafe, so a bogus or hostile path is a "could not
// tell", never a command.
func probeLoadModeBinary(ctx context.Context, path string) (supported, known bool) {
	if reason := binaryPathSafe(path); reason != "" {
		return false, false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
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

// ProbeLoadMode reports whether the llama-server binary at path takes the --load-mode flag family (known is false
// when it could not be run). It is the same probe a registered config's flag spelling is chosen with, cached by the
// binary's size and mtime. Callers holding a catalog build should gate the path through the build guard
// (internal/buildguard) first: that is tighter than the check this probe makes for itself.
func ProbeLoadMode(ctx context.Context, path string) (supported, known bool) {
	return probeLoadModeBinary(ctx, path)
}
