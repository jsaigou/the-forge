// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// The probe asks the binary itself which flag spelling it takes, so a build's path is the one value here that ends
// up in an exec. Two floors: the probe runs nothing its own check rejects, and the Service also runs nothing the
// injected build guard (internal/buildguard, ADR-0020 F8) rejects — a refusal is the --load-mode default, not an
// error.
func TestLoadModeProbe_NeverExecsAnUnvettedPath(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho --ctx-size\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "llama-server", "./llama-server", "llama-server;rm -rf /", good + ";true"} {
		if supported, known := probeLoadModeBinary(context.Background(), bad); supported || known {
			t.Errorf("probeLoadModeBinary(%q) = (%v,%v), want (false,false)", bad, supported, known)
		}
	}
	if _, known := probeLoadModeBinary(context.Background(), dir); known {
		t.Error("a directory is not a binary: the probe must report it could not tell")
	}
	if supported, known := probeLoadModeBinary(context.Background(), good); !known || supported {
		t.Errorf("probeLoadModeBinary(good) = (%v,%v), want (false,true): --ctx-size present, --load-mode absent", supported, known)
	}
}

func TestLoadModeArgs_SkipsProbeWhenTheBuildGuardRefuses(t *testing.T) {
	svc, _, _ := newTestService(t, func(http.ResponseWriter, *http.Request) {})
	probeRan := false
	svc.d.ProbeLoadMode = func(context.Context, string) (bool, bool) { probeRan = true; return false, true }
	svc.d.VetBinaryPath = func(_ context.Context, path string) string {
		if strings.HasPrefix(path, "/opt/forge/") {
			return ""
		}
		return "must be under an allowed root"
	}

	if got := svc.loadModeArgs(context.Background(), store.Build{BinaryPath: "/tmp/llama-server"}); strings.Join(got, " ") != "--load-mode none" {
		t.Errorf("guarded-away build = %v, want the --load-mode default", got)
	}
	if probeRan {
		t.Error("the probe ran on a path the build guard refused")
	}
	if got := svc.loadModeArgs(context.Background(), store.Build{BinaryPath: "/opt/forge/legacy/llama-server"}); strings.Join(got, " ") != "--no-mmap" {
		t.Errorf("allowed legacy build = %v, want --no-mmap", got)
	}
	if !probeRan {
		t.Error("the probe did not run on a path the build guard accepted")
	}
}
