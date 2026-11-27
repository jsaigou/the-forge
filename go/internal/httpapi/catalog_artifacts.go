// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_artifacts.go — the artifacts write API (ADR-0020 F8, docs/api-contract-implementation-plan.md phase 4).
//
// An artifact is one file (or directory, for safetensors) a variant ships as. Until now they were created only by the
// Add Model downloader, so a file already on disk could not be registered. The same checks the downloader's registrar
// makes apply here, and the file facts are read from the file rather than trusted from the request: a GGUF must have
// a valid header, and its architecture, trained context, parameter count, quantization and size come from it.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

// toStore is the one place a request body becomes a store.Artifact; the module fills in the file facts and the mmproj auxiliary flag.
func (b artifactBody) toStore() store.Artifact {
	return store.Artifact{VariantID: b.VariantID, QuantizationID: b.QuantizationID, FormatID: b.FormatID, FilePath: b.FilePath,
		ShardSetID: b.ShardSetID, IsAuxiliary: b.IsAuxiliary, ArtifactType: b.ArtifactType, SHA256: b.SHA256}
}

// artifactWrites builds the Artifact write module from the live dependencies: every create, update and delete of an Artifact goes through it.
func (s *Server) artifactWrites() store.ArtifactWrites {
	return store.ArtifactWrites{Cat: s.deps.Catalog, Changed: s.invalidateCfg, ModelsDir: s.modelsDir}
}

// writeArtifactError maps the Artifact write module's errors onto the API's codes.
func writeArtifactError(w http.ResponseWriter, err error) {
	var inv *store.Invalid
	var inUse *store.ArtifactInUseError
	switch {
	case errors.As(err, &inv):
		writeValidationErrorCodes(w, inv.Fields, inv.Codes)
	case errors.As(err, &inUse):
		writeErrorCode(w, http.StatusConflict, "artifact_in_use",
			map[string]any{"count": len(inUse.Configs), "configs": strings.Join(inUse.Configs, ", ")}, inUse.Error())
	case isNotFound(err):
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "artifact"}, "artifact not found")
	default:
		writeInternalError(w, err)
	}
}

type artifactBody struct {
	VariantID      int64  `json:"variant_id"`
	QuantizationID int64  `json:"quantization_id"`
	FormatID       int64  `json:"format_id"`
	FilePath       string `json:"file_path"`
	ShardSetID     string `json:"shard_set_id"`
	IsAuxiliary    bool   `json:"is_auxiliary"`
	ArtifactType   string `json:"artifact_type"`
	SHA256         string `json:"sha256"`
}

// modelsDir returns the configured models directory ("" when unconfigured).
func (s *Server) modelsDir() string {
	if s.deps.Config == nil {
		return ""
	}
	if cfg := s.deps.Config(); cfg != nil {
		return cfg.Paths.ModelsDir
	}
	return ""
}

func (s *Server) handleCatalogArtifactGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	a, err := cat.GetArtifact(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "artifact"}, "artifact not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, artifactToJSON(a))
}

func (s *Server) handleCatalogArtifactCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b artifactBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	got, err := s.artifactWrites().Create(ctx, b.toStore())
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_artifact_create", strconv.FormatInt(got.ID, 10), b.FilePath)
	writeJSON(w, http.StatusCreated, artifactToJSON(got))
}

func (s *Server) handleCatalogArtifactUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	var b artifactBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	got, err := s.artifactWrites().Update(ctx, id, b.toStore())
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_artifact_update", strconv.FormatInt(id, 10), b.FilePath)
	writeJSON(w, http.StatusOK, artifactToJSON(got))
}

func (s *Server) handleCatalogArtifactDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	// Only the catalog row goes; the file on disk is never touched by the API.
	if err := s.artifactWrites().Delete(ctx, id); err != nil {
		writeArtifactError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_artifact_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogArtifactValidate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b artifactBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	inv, _, err := s.artifactWrites().Validate(ctx, b.toStore(), validateExcludeID(r))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if inv != nil {
		writeValidationErrorCodes(w, inv.Fields, inv.Codes)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}
