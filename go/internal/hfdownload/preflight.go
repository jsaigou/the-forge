// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsaigou/the-forge/internal/hf"
)

// preflight.go — read-only pre-download validation. Nothing here writes
// anything; Preflight is safe to call as often as a UI wants (e.g. on
// every keystroke of a quant picker) without side effects.

// vulkanCeilingBytes is the documented Vulkan (RADV STRIX_HALO) load
// ceiling (CLAUDE.md Critical Hardware Facts) — used here only to flag
// which backend a newly-registered Config should request, not to block a
// download outright (a model too big for Vulkan may still be intended for
// the ROCm+unified-memory path).
const vulkanCeilingBytes = 63 << 30

// diskHeadroomFactor is the safety margin over raw file size Preflight
// requires before it will call disk "OK" — leaves room for the .part file
// existing alongside the eventual final file during finalize's rename, and
// for filesystem overhead.
const diskHeadroomFactor = 1.10

// PreflightFile is one file Preflight is asked to validate together (a
// multi-entry slice models a sharded GGUF as one unit — see FileSet).
type PreflightFile struct {
	Filename  string `json:"filename"` // path within the HF repo tree
	SizeBytes int64  `json:"size_bytes"`
}

// FileSet turns ranked hf.QuantCandidate results (or a raw hf.Tree
// listing) into the []PreflightFile shape Preflight and job creation
// share. For a sharded model, pass every shard's File — RankCandidates
// already excludes non-.gguf entries.
func FileSet(files []hf.File) []PreflightFile {
	out := make([]PreflightFile, 0, len(files))
	for _, f := range files {
		if f.IsDir {
			continue
		}
		out = append(out, PreflightFile{Filename: f.Path, SizeBytes: f.SizeBytes})
	}
	return out
}

// PreflightCheck is one validation result. Severity is "ok" | "warn" |
// "block" — a UI renders these directly, no further interpretation
// needed.
type PreflightCheck struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// PreflightReport is Preflight's result.
type PreflightReport struct {
	Repo       string           `json:"repo"`
	Files      []PreflightFile  `json:"files"`
	TotalBytes int64            `json:"total_bytes"`
	DestDir    string           `json:"dest_dir"`
	Blocked    bool             `json:"blocked"`
	Checks     []PreflightCheck `json:"checks"`
	// RequiresBackend is "" (either backend is fine) or "rocm" (the total
	// size exceeds the Vulkan ceiling — a generated Config must request
	// backend=rocm with unified memory, never guessed from a name; see
	// store.Build.Backend's doc comment for the incident that rule
	// prevents).
	RequiresBackend string `json:"requires_backend,omitempty"`
}

// DefaultDestDir picks a destination subdirectory the same way
// docs/adding-a-model.md's manual flow does: a single flat file downloads
// straight into the models dir; a multi-file (sharded) set gets its own
// subdirectory named after the repo, so shard files never mix with an
// unrelated model's files of the same generic name.
func DefaultDestDir(repo string, fileCount int) string {
	if fileCount <= 1 {
		return ""
	}
	repo = strings.Trim(repo, "/")
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	return repo
}

// DestRelPath is the on-disk path Preflight and the worker both compute
// for one file, relative to Paths.ModelsDir. Shard files are flattened
// into destDir by basename — HF shard filenames already sort correctly
// (…-00001-of-00003.gguf, …-00002-of-00003.gguf) without preserving any
// upstream subdirectory structure. Both inputs are caller-supplied, so the
// result is always a RELATIVE path: absolute prefixes and "." / ".."
// components are dropped, keeping every download inside ModelsDir.
func DestRelPath(destDir, filename string) string {
	return filepath.Join(safeRelDir(destDir), filepath.Base(filename))
}

func safeRelDir(destDir string) string {
	var parts []string
	for _, c := range strings.Split(filepath.ToSlash(destDir), "/") {
		if c == "" || c == "." || c == ".." {
			continue
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, "/")
}

// Preflight validates repo/files/destDir without writing anything. A
// caller (the frontend's pre-download screen, or smith's hf_preflight
// tool) is expected to call this and show Blocked/Checks before offering
// a Download button.
func (s *Service) Preflight(ctx context.Context, repo string, files []PreflightFile, destDir string) (PreflightReport, error) {
	report := PreflightReport{Repo: repo, Files: files, DestDir: destDir}
	for _, f := range files {
		report.TotalBytes += f.SizeBytes
	}

	cfg := s.d.Cfg()
	var modelsDir string
	if cfg != nil {
		modelsDir = cfg.Paths.ModelsDir
	}

	report.Checks = append(report.Checks, s.preflightDisk(report.TotalBytes)...)
	report.Checks = append(report.Checks, s.preflightMemory(report.TotalBytes)...)

	if report.TotalBytes > vulkanCeilingBytes {
		report.RequiresBackend = "rocm"
		report.Checks = append(report.Checks, PreflightCheck{
			ID: "backend", Severity: "warn",
			Summary: "total size exceeds the ~63 GB Vulkan ceiling — this model requires the ROCm + unified-memory backend",
		})
	} else {
		report.Checks = append(report.Checks, PreflightCheck{
			ID: "backend", Severity: "ok", Summary: "fits within the Vulkan backend's memory ceiling",
		})
	}

	if modelsDir != "" {
		report.Checks = append(report.Checks, s.preflightExistingFiles(modelsDir, destDir, files)...)
	}

	for _, c := range report.Checks {
		if c.Severity == "block" {
			report.Blocked = true
			break
		}
	}
	return report, nil
}

func (s *Service) preflightDisk(totalBytes int64) []PreflightCheck {
	if s.d.Source == nil {
		return []PreflightCheck{{ID: "disk", Severity: "warn", Summary: "disk headroom unavailable — no collector snapshot wired"}}
	}
	snap := s.d.Source.Current()
	if snap == nil || snap.Metrics.Disk.TotalBytes <= 0 {
		return []PreflightCheck{{ID: "disk", Severity: "warn", Summary: "disk headroom unavailable — no collector snapshot yet"}}
	}
	need := int64(float64(totalBytes) * diskHeadroomFactor)
	free := snap.Metrics.Disk.FreeBytes
	if need > free {
		return []PreflightCheck{{
			ID: "disk", Severity: "block",
			Summary: humanBytes(totalBytes) + " needed (+10% margin) but only " + humanBytes(free) + " free",
		}}
	}
	return []PreflightCheck{{ID: "disk", Severity: "ok", Summary: humanBytes(free) + " free, " + humanBytes(totalBytes) + " needed"}}
}

func (s *Service) preflightMemory(totalBytes int64) []PreflightCheck {
	if s.d.Source == nil {
		return nil
	}
	snap := s.d.Source.Current()
	if snap == nil || snap.Metrics.GTTTotalBytes == nil {
		return nil
	}
	estVRAM := int64(float64(totalBytes) * hf.OneTwoXOverhead)
	budget := *snap.Metrics.GTTTotalBytes
	if estVRAM > budget {
		return []PreflightCheck{{
			ID: "memory", Severity: "warn",
			Summary: "estimated " + humanBytes(estVRAM) + " VRAM exceeds this host's " + humanBytes(budget) + " GTT budget — the model can still be downloaded, just not loaded as-is",
		}}
	}
	return []PreflightCheck{{ID: "memory", Severity: "ok", Summary: "estimated " + humanBytes(estVRAM) + " VRAM fits the current GTT budget"}}
}

func (s *Service) preflightExistingFiles(modelsDir, destDir string, files []PreflightFile) []PreflightCheck {
	var conflicts []string
	for _, f := range files {
		final := filepath.Join(modelsDir, DestRelPath(destDir, f.Filename))
		if _, err := os.Stat(final); err == nil {
			conflicts = append(conflicts, DestRelPath(destDir, f.Filename))
		}
	}
	if len(conflicts) == 0 {
		return []PreflightCheck{{ID: "existing_file", Severity: "ok", Summary: "no destination file conflicts"}}
	}
	return []PreflightCheck{{
		ID: "existing_file", Severity: "block",
		Summary: "already exists on disk, refusing to overwrite: " + strings.Join(conflicts, ", "),
	}}
}

// humanBytes mirrors smith/fetch_model_ops.go's helper of the same name
// (a different package — no shared import without creating a dependency
// smith would then need on this package's sibling code).
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
