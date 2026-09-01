// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/collector"
)

// TestBinaryPaths_UnitExecStartMissing pins the fix for the 2026-09-01
// forge-comfyui incident: a watched unit whose ExecStart= program is
// missing must surface here, on every quick sweep, without anyone having
// tried to load or reach the unit first.
func TestBinaryPaths_UnitExecStartMissing(t *testing.T) {
	env := &CheckEnv{
		Snap: &collector.Snapshot{Units: map[string]collector.UnitState{
			"forge-comfyui": {ExecStartPath: "/usr/local/lib/forge/start-comfyui-does-not-exist.sh"},
		}},
	}
	f := runBinaryPaths(context.Background(), env)
	if f.Severity != SeverityWarn {
		t.Fatalf("severity = %s, want warn", f.Severity)
	}
	if !strings.Contains(f.Summary, "forge-comfyui") {
		t.Errorf("summary = %q, want it to name the unit", f.Summary)
	}
}

// TestBinaryPaths_UnitExecStartPresent proves a real, executable ExecStart
// path doesn't false-positive (uses this test binary's own path, which is
// always present+executable in `go test`).
func TestBinaryPaths_UnitExecStartPresent(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("could not resolve a real executable path for this test:", err)
	}
	env := &CheckEnv{
		Snap: &collector.Snapshot{Units: map[string]collector.UnitState{
			"forge-a1": {ExecStartPath: self},
		}},
	}
	f := runBinaryPaths(context.Background(), env)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok; summary=%q", f.Severity, f.Summary)
	}
}

// TestBinaryPaths_UnitWithNoExecStart proves an unprobed/non-service unit
// (empty ExecStartPath) is silently skipped, not reported as missing.
func TestBinaryPaths_UnitWithNoExecStart(t *testing.T) {
	env := &CheckEnv{
		Snap: &collector.Snapshot{Units: map[string]collector.UnitState{
			"forge-comfyui": {ActiveState: "inactive"},
		}},
	}
	f := runBinaryPaths(context.Background(), env)
	if f.Severity != SeverityInfo || !strings.HasPrefix(f.Summary, "skipped:") {
		t.Errorf("f = %+v, want a skip finding (no configured/catalog paths, no ExecStart to check)", f)
	}
}
