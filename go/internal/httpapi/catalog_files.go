// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_files.go — GET /api/v1/models/files, the filesystem browse
// endpoint backing the Config editor's model picker (split from
// catalog_handlers.go, Sprint 5 code-quality cleanup, #33).

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jsaigou/the-forge/internal/gguf"
)

type modelFileJSON struct {
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	Arch       string `json:"arch"`
	TrainedCtx int    `json:"trained_ctx"`
	IsShardSet bool   `json:"is_shard_set"`
}

// handleModelFiles — walks cfg.Paths.ModelsDir recursively, shard-aware,
// returns {path, sizeMB, arch, trainedCtx, isShardSet} per GGUF. This is the
// model picker for the Config editor (Q1 of the grill — the registry is card
// data, not file discovery).
func (s *Server) handleModelFiles(w http.ResponseWriter, r *http.Request) {
	dir := s.deps.Config().Paths.ModelsDir
	if dir == "" {
		writeJSON(w, http.StatusOK, []modelFileJSON{})
		return
	}

	var files []modelFileJSON
	shardSeen := map[string]bool{}

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		// return nil here (and below) is the filepath.WalkDir "skip this
		// entry, keep walking" idiom, not a discarded failure — returning
		// err instead would abort the whole listing over one bad entry.
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".gguf") {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)

		// Shard detection: filenames like "model-00001-of-00005.gguf".
		shardID, isShard := shardSetID(rel)
		if isShard {
			if shardSeen[shardID] {
				return nil // already reported this shard set
			}
			shardSeen[shardID] = true
		}

		fi, err := os.Stat(path)
		if err != nil {
			// A file that vanished (or became unreadable) between WalkDir's
			// listing and this Stat is skipped, not fatal to the rest of the
			// directory scan.
			return nil
		}
		sizeBytes := fi.Size()

		// Read GGUF header metadata (arch + trained_ctx). Non-fatal on error.
		var arch string
		var trainedCtx int
		if md, err := gguf.ReadMetadata(path); err == nil {
			arch = md.Architecture
			trainedCtx = md.TrainedCtx
		}

		files = append(files, modelFileJSON{
			Path:       rel,
			SizeBytes:  sizeBytes,
			Arch:       arch,
			TrainedCtx: trainedCtx,
			IsShardSet: isShard,
		})
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "models dir walk failed")
		return
	}

	if files == nil {
		files = []modelFileJSON{}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	writeJSON(w, http.StatusOK, files)
}

// shardSetID extracts the shard-set identifier from a filename like
// "model-00001-of-00005.gguf" → "model". Returns ("", false) for non-shard
// files.
func shardSetID(filename string) (string, bool) {
	base := strings.TrimSuffix(filename, ".gguf")
	base = strings.TrimSuffix(base, ".GGUF")
	idx := strings.LastIndex(base, "-")
	if idx < 0 {
		return "", false
	}
	tail := base[idx+1:]
	if !strings.HasPrefix(tail, "0000") && !strings.HasPrefix(tail, "0001") {
		return "", false
	}
	// Must match "-NNNNN-of-NNNNN" pattern.
	parts := strings.SplitN(base, "-of-", 2)
	if len(parts) != 2 {
		return "", false
	}
	prefix := parts[0]
	// Strip the trailing "-NNNNN" from prefix.
	lastDash := strings.LastIndex(prefix, "-")
	if lastDash < 0 {
		return "", false
	}
	return prefix[:lastDash], true
}
