// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"math"
	"syscall"

	"github.com/jsaigou/the-forge/internal/config"
)

// sampleDisk reads free/total space for the filesystem containing path via
// statfs (the models/data volume, config.Paths.ModelsDir — Sprint 0 §0.4).
// Returns the zero Disk when path is empty or the probe fails, matching the
// frozen metricsDisk contract (zero, not null, for an unsampled disk).
//
// A1 (bytes retrofit): statfs reports bytes natively (Bsize×Blocks); the
// probe no longer divides to MB. Consumers receive bytes.
//
// syscall.Statfs_t's field set (Bsize/Blocks/Bavail) is common to the
// darwin and linux definitions, differing only in the integer width of
// Bsize — the explicit int64/uint64 conversions below compile and behave
// correctly on both, which matters because `go build`/`go test` for this
// package also run on macOS dev machines, not just the Linux target.
func sampleDisk(path string) Disk {
	if path == "" {
		return Disk{}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}
	}
	bsize := uint64(st.Bsize)
	totalBytes := uint64(st.Blocks) * bsize
	freeBytes := uint64(st.Bavail) * bsize
	if totalBytes == 0 {
		return Disk{}
	}
	total := int64(totalBytes)
	free := int64(freeBytes)
	used := total - free
	if used < 0 {
		used = 0
	}
	var pct float64
	if total > 0 {
		pct = math.Round(float64(used)/float64(total)*1000) / 10
	}
	return Disk{TotalBytes: total, FreeBytes: free, UsedBytes: used, Pct: pct}
}

// sampleStorageMounts reports per-mount storage for the paths this daemon
// actually cares about (Phase 4 collector metrics, 2026-08-12: storage used
// to be a single statfs of ModelsDir only). Rather than parsing /proc/mounts
// and filtering pseudo-filesystems (tmpfs, overlay, cgroup, ...) — fragile
// and not needed for what the dashboard shows — this samples the three
// named paths that are actually configured/meaningful: root ("/"), the
// models volume, and the state dir, skipping any that are empty or resolve
// to a path already sampled under a different name (avoids showing the same
// mount twice when ModelsDir/StateDir happen to share a filesystem with
// root, common on a single-disk box).
func sampleStorageMounts(paths config.Paths) []StorageMount {
	type named struct{ name, path string }
	candidates := []named{
		{"root", "/"},
		{"models", paths.ModelsDir},
		{"state", paths.StateDir},
	}
	var out []StorageMount
	seen := map[string]bool{}
	for _, c := range candidates {
		if c.path == "" || seen[c.path] {
			continue
		}
		seen[c.path] = true
		d := sampleDisk(c.path)
		if d.TotalBytes == 0 {
			continue // path missing or statfs failed — omit rather than emit a fake zero row
		}
		out = append(out, StorageMount{Name: c.name, Path: c.path, Disk: d})
	}
	return out
}
