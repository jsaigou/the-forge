// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestProbeLoadModeRealBinaries runs the real probe against actual
// llama-server binaries. Opt-in: FORGE_PROBE_BINS="path=wantSupported,..."
// (e.g. "/opt/.../llama-server=true,/opt/.../old/llama-server=false").
func TestProbeLoadModeRealBinaries(t *testing.T) {
	spec := os.Getenv("FORGE_PROBE_BINS")
	if spec == "" {
		t.Skip("FORGE_PROBE_BINS not set")
	}
	for _, item := range strings.Split(spec, ",") {
		path, want, ok := strings.Cut(item, "=")
		if !ok {
			t.Fatalf("bad spec item %q", item)
		}
		supported, known := probeLoadModeBinary(context.Background(), path)
		t.Logf("%s: supported=%v known=%v", path, supported, known)
		if !known {
			t.Errorf("%s: probe could not tell", path)
			continue
		}
		if supported != (want == "true") {
			t.Errorf("%s: supported=%v, want %s", path, supported, want)
		}
	}
}
