// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_variants.go — Variant CRUD (split from catalog_handlers.go,
// Sprint 5 code-quality cleanup, #33). Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

type variantJSON struct {
	ID                  int64  `json:"id"`
	ModelID             int64  `json:"model_id"`
	Name                string `json:"name"`
	DerivationType      string `json:"derivation_type"`
	SourceVariantID     int64  `json:"source_variant_id"`
	TrainedCtx          int    `json:"trained_ctx"`
	IsAbliterated       bool   `json:"is_abliterated"`
	AbliterationQuality string `json:"abliteration_quality"`
}

func variantToJSON(v store.Variant) variantJSON {
	return variantJSON{
		ID: v.ID, ModelID: v.ModelID, Name: v.Name, DerivationType: v.DerivationType,
		SourceVariantID: v.SourceVariantID, TrainedCtx: v.TrainedCtx,
		IsAbliterated: v.IsAbliterated, AbliterationQuality: v.AbliterationQuality,
	}
}

func (s *Server) handleCatalogVariantsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []variantJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Variant
	if mid := r.URL.Query().Get("model_id"); mid != "" {
		id, err := strconv.ParseInt(mid, 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"model_id": "must be an integer"})
			return
		}
		list, err = cat.ListVariantsForModel(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "variants query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListVariants(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "variants query failed")
			return
		}
	}
	out := make([]variantJSON, 0, len(list))
	for _, v := range list {
		out = append(out, variantToJSON(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogVariantGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	v, err := cat.GetVariant(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "variant not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, variantToJSON(v))
}

func (s *Server) handleCatalogVariantCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b variantBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateVariant(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateVariant(ctx, store.Variant{
		ModelID: b.ModelID, Name: b.Name, DerivationType: b.DerivationType,
		SourceVariantID: b.SourceVariantID, TrainedCtx: b.TrainedCtx,
		IsAbliterated: b.IsAbliterated, AbliterationQuality: b.AbliterationQuality,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_variant_create", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	v, _ := cat.GetVariant(ctx, id)
	writeJSON(w, http.StatusCreated, variantToJSON(v))
}

func (s *Server) handleCatalogVariantUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b variantBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateVariant(r.Context(), b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateVariant(ctx, store.Variant{
		ID: id, ModelID: b.ModelID, Name: b.Name, DerivationType: b.DerivationType,
		SourceVariantID: b.SourceVariantID, TrainedCtx: b.TrainedCtx,
		IsAbliterated: b.IsAbliterated, AbliterationQuality: b.AbliterationQuality,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "variant not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_variant_update", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	v, _ := cat.GetVariant(ctx, id)
	writeJSON(w, http.StatusOK, variantToJSON(v))
}

func (s *Server) handleCatalogVariantDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteVariant(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "variant not found")
			return
		}
		writeError(w, http.StatusConflict, "variant has dependent configs — delete those first")
		return
	}
	s.audit(r, identity(r).Name, "catalog_variant_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogVariantValidate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b variantBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateVariant(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

type variantBody struct {
	ModelID             int64  `json:"model_id"`
	Name                string `json:"name"`
	DerivationType      string `json:"derivation_type"`
	SourceVariantID     int64  `json:"source_variant_id"`
	TrainedCtx          int    `json:"trained_ctx"`
	IsAbliterated       bool   `json:"is_abliterated"`
	AbliterationQuality string `json:"abliteration_quality"`
}

// validateVariant checks field constraints + model existence.
func (s *Server) validateVariant(ctx context.Context, b variantBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	}
	if b.ModelID == 0 {
		fields["model_id"] = "is required"
	} else if s.deps.Catalog != nil {
		if _, err := s.deps.Catalog.GetModel(ctx, b.ModelID); err != nil {
			fields["model_id"] = "does not exist"
		}
	}
	if b.SourceVariantID != 0 && s.deps.Catalog != nil {
		if _, err := s.deps.Catalog.GetVariant(ctx, b.SourceVariantID); err != nil {
			fields["source_variant_id"] = "does not exist"
		}
	}
	switch b.DerivationType {
	case "", "abliteration", "finetune", "merge", "mtp-head-add", "uncensor", "base":
	default:
		fields["derivation_type"] = "must be one of: abliteration, finetune, merge, mtp-head-add, uncensor, base"
	}
	return fields
}
