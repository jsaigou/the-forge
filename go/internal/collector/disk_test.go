// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
)

func TestSampleDiskRealPath(t *testing.T) {
	d := sampleDisk(t.TempDir())
	if d.TotalBytes <= 0 {
		t.Fatalf("sampleDisk(tempdir) = %+v, want TotalBytes > 0", d)
	}
	if d.FreeBytes < 0 || d.FreeBytes > d.TotalBytes {
		t.Errorf("FreeBytes=%d out of range for TotalBytes=%d", d.FreeBytes, d.TotalBytes)
	}
	if d.UsedBytes != d.TotalBytes-d.FreeBytes {
		t.Errorf("UsedBytes=%d, want TotalBytes-FreeBytes=%d", d.UsedBytes, d.TotalBytes-d.FreeBytes)
	}
	if d.Pct < 0 || d.Pct > 100 {
		t.Errorf("Pct=%v out of [0,100]", d.Pct)
	}
}

func TestSampleDiskEmptyPath(t *testing.T) {
	if got := sampleDisk(""); got != (Disk{}) {
		t.Errorf("sampleDisk(\"\") = %+v, want zero value", got)
	}
}

func TestSampleDiskMissingPath(t *testing.T) {
	if got := sampleDisk("/definitely/does/not/exist/forge-test"); got != (Disk{}) {
		t.Errorf("sampleDisk(missing) = %+v, want zero value", got)
	}
}

// TestCycleSamplesDisk checks the collector wires sampleDisk into a real
// cycle's Metrics — not just that the helper itself works in isolation.
// testConfig's Paths.ModelsDir is a real t.TempDir(), so the probe should
// report real numbers from the test machine's filesystem.
func TestCycleSamplesDisk(t *testing.T) {
	cfg := testConfig(t)
	sys := &fakeSystemd{}

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		DialPort: func(int) bool { return false },
		BaseURL:  func(port int) string { return "http://127.0.0.1:1" },
	})
	snap := c.ProbeNow(context.Background())

	if snap.Metrics.Disk.TotalBytes <= 0 {
		t.Fatalf("snap.Metrics.Disk = %+v, want TotalBytes > 0 (Paths.ModelsDir=%s)",
			snap.Metrics.Disk, cfg.Paths.ModelsDir)
	}
}

// TestSampleStorageMounts checks the per-mount sampler (Phase 4, 2026-08-12):
// root/models/state each get their own row, and a duplicate path (models ==
// state) collapses to one row instead of reporting the same mount twice.
func TestSampleStorageMounts(t *testing.T) {
	models := t.TempDir()
	state := t.TempDir()

	mounts := sampleStorageMounts(config.Paths{ModelsDir: models, StateDir: state})
	names := map[string]bool{}
	for _, m := range mounts {
		names[m.Name] = true
		if m.Disk.TotalBytes <= 0 {
			t.Errorf("mount %q: TotalBytes <= 0", m.Name)
		}
	}
	for _, want := range []string{"root", "models", "state"} {
		if !names[want] {
			t.Errorf("mounts missing %q: %+v", want, mounts)
		}
	}
}

func TestSampleStorageMountsDedupesSharedPath(t *testing.T) {
	shared := t.TempDir()
	mounts := sampleStorageMounts(config.Paths{ModelsDir: shared, StateDir: shared})
	n := 0
	for _, m := range mounts {
		if m.Path == shared {
			n++
		}
	}
	if n != 1 {
		t.Errorf("shared path appeared %d times, want 1: %+v", n, mounts)
	}
}

func TestSampleStorageMountsEmptyPathsSkipped(t *testing.T) {
	mounts := sampleStorageMounts(config.Paths{})
	for _, m := range mounts {
		if m.Name != "root" {
			t.Errorf("unexpected mount with empty Paths: %+v", mounts)
		}
	}
}
