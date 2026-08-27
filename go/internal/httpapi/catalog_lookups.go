// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_lookups.go — read-only list endpoints for the catalog's small
// reference tables (quantizations, formats, engines, builds) plus the
// artifacts list (split from catalog_handlers.go, Sprint 5 code-quality
// cleanup, #33). None of these have create/update/delete routes.

import (
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

type artifactJSON struct {
	ID                 int64  `json:"id"`
	VariantID          int64  `json:"variant_id"`
	QuantizationID     int64  `json:"quantization_id"`
	FormatID           int64  `json:"format_id"`
	FilePath           string `json:"file_path"`
	ShardSetID         string `json:"shard_set_id"`
	IsAuxiliary        bool   `json:"is_auxiliary"`
	ArtifactType       string `json:"artifact_type"`
	Missing            bool   `json:"missing"`
	SHA256             string `json:"sha256"`
	FileSizeBytes      int64  `json:"file_size_bytes"`
	GGUFArch           string `json:"gguf_arch"`
	GGUFTrainedCtx     int    `json:"gguf_trained_ctx"`
	GGUFParameterCount string `json:"gguf_parameter_count"`
	GGUFQuantType      string `json:"gguf_quant_type"`
}

type engineJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type buildJSON struct {
	ID         int64  `json:"id"`
	EngineID   int64  `json:"engine_id"`
	Name       string `json:"name"`
	BinaryPath string `json:"binary_path"`
	Backend    string `json:"backend"`
	Reason     string `json:"reason"`
}

func artifactToJSON(a store.Artifact) artifactJSON {
	return artifactJSON{
		ID: a.ID, VariantID: a.VariantID, QuantizationID: a.QuantizationID,
		FormatID: a.FormatID, FilePath: a.FilePath, ShardSetID: a.ShardSetID,
		IsAuxiliary: a.IsAuxiliary, ArtifactType: a.ArtifactType, Missing: a.Missing,
		SHA256: a.SHA256, FileSizeBytes: a.FileSizeBytes, GGUFArch: a.GGUFArch,
		GGUFTrainedCtx: a.GGUFTrainedCtx, GGUFParameterCount: a.GGUFParameterCount,
		GGUFQuantType: a.GGUFQuantType,
	}
}

func engineToJSON(e store.Engine) engineJSON {
	return engineJSON{ID: e.ID, Name: e.Name}
}

func buildToJSON(b store.Build) buildJSON {
	return buildJSON{ID: b.ID, EngineID: b.EngineID, Name: b.Name,
		BinaryPath: b.BinaryPath, Backend: b.Backend, Reason: b.Reason}
}

func (s *Server) handleCatalogQuantizationsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []store.Quantization{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListQuantizations(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "quantizations query failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCatalogFormatsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []store.Format{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListFormats(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "formats query failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCatalogEnginesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []engineJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListEngines(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "engines query failed")
		return
	}
	out := make([]engineJSON, 0, len(list))
	for _, e := range list {
		out = append(out, engineToJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogBuildsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []buildJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListBuilds(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "builds query failed")
		return
	}
	out := make([]buildJSON, 0, len(list))
	for _, b := range list {
		out = append(out, buildToJSON(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogArtifactsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []artifactJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Artifact
	if vid := r.URL.Query().Get("variant_id"); vid != "" {
		id, err := strconv.ParseInt(vid, 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"variant_id": "must be an integer"})
			return
		}
		list, err = cat.ListArtifactsForVariant(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "artifacts query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListArtifacts(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "artifacts query failed")
			return
		}
	}
	out := make([]artifactJSON, 0, len(list))
	for _, a := range list {
		out = append(out, artifactToJSON(a))
	}
	writeJSON(w, http.StatusOK, out)
}
