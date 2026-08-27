// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_models.go — Model CRUD (split from catalog_handlers.go, Sprint 5
// code-quality cleanup, #33). Mutating routes are requireRole(admin) +
// requireAssurance(page.settings); see httpapi.go's route table.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

type modelJSON struct {
	ID             int64  `json:"id"`
	FamilyID       int64  `json:"family_id"`
	Name           string `json:"name"`
	Architecture   string `json:"architecture"`
	ParameterCount string `json:"parameter_count"`
	Description    string `json:"description"`
	Creator        string `json:"creator"`
	LicenseName    string `json:"license_name"`
	LicenseURL     string `json:"license_url"`
	HFRepo         string `json:"hf_repo"`
	// Logo is an Icon manifest slug (web/src/assets/icons/manifest.ts) or an
	// uploaded data-URL (PUT …/icon with paths.icons_dir unset — Sprint A2).
	Logo        string   `json:"logo"`
	LogoDark    string   `json:"logo_dark"` // dark-theme override; "" falls back to Logo
	KeyFeatures []string `json:"key_features"`
	// Modalities (Sprint J1) is what this model's architecture supports:
	// a subset of text/vision/audio. See store.Model.Modalities's doc
	// comment and registry.resolveModalities for how a Config narrows this.
	Modalities []string `json:"modalities"`
	// Visibility is visible (Models gallery) or hidden (Settings only) —
	// model-level decommission flag (0062). Default "visible".
	Visibility string `json:"visibility"`
}

func modelToJSON(m store.Model) modelJSON {
	return modelJSON{
		ID: m.ID, FamilyID: m.FamilyID, Name: m.Name, Architecture: m.Architecture,
		ParameterCount: m.ParameterCount, Description: m.Description, Creator: m.Creator,
		LicenseName: m.LicenseName, LicenseURL: m.LicenseURL, HFRepo: m.HFRepo,
		Logo: m.Logo, LogoDark: m.LogoDark, KeyFeatures: m.KeyFeatures, Modalities: m.Modalities,
		Visibility: m.Visibility,
	}
}

func (s *Server) handleCatalogModelsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []modelJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListModels(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "models query failed")
		return
	}
	out := make([]modelJSON, 0, len(list))
	for _, m := range list {
		out = append(out, modelToJSON(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogModelGet(w http.ResponseWriter, r *http.Request) {
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
	m, err := cat.GetModel(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, modelToJSON(m))
}

func (s *Server) handleCatalogModelCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b modelBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateModel(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateModel(ctx, store.Model{
		FamilyID: b.FamilyID, Name: b.Name, Architecture: b.Architecture,
		ParameterCount: b.ParameterCount, Description: b.Description, Creator: b.Creator,
		LicenseName: b.LicenseName, LicenseURL: b.LicenseURL, HFRepo: b.HFRepo,
		Logo: b.Logo, LogoDark: b.LogoDark, KeyFeatures: b.KeyFeatures, Modalities: b.Modalities,
		Visibility: b.Visibility,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_create", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	s.invalidateCfg()
	m, _ := cat.GetModel(ctx, id)
	writeJSON(w, http.StatusCreated, modelToJSON(m))
}

func (s *Server) handleCatalogModelUpdate(w http.ResponseWriter, r *http.Request) {
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
	var b modelBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateModel(r.Context(), b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateModel(ctx, store.Model{
		ID: id, FamilyID: b.FamilyID, Name: b.Name, Architecture: b.Architecture,
		ParameterCount: b.ParameterCount, Description: b.Description, Creator: b.Creator,
		LicenseName: b.LicenseName, LicenseURL: b.LicenseURL, HFRepo: b.HFRepo,
		Logo: b.Logo, LogoDark: b.LogoDark, KeyFeatures: b.KeyFeatures, Modalities: b.Modalities,
		Visibility: b.Visibility,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_update", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	s.invalidateCfg()
	m, _ := cat.GetModel(ctx, id)
	writeJSON(w, http.StatusOK, modelToJSON(m))
}

func (s *Server) handleCatalogModelDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := cat.DeleteModel(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model not found")
			return
		}
		writeError(w, http.StatusConflict, "model has dependent variants — delete those first")
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_delete", strconv.FormatInt(id, 10), withReason("", r.URL.Query().Get("reason")))
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogModelValidate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b modelBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateModel(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

type modelBody struct {
	FamilyID       int64  `json:"family_id"`
	Name           string `json:"name"`
	Architecture   string `json:"architecture"`
	ParameterCount string `json:"parameter_count"`
	Description    string `json:"description"`
	Creator        string `json:"creator"`
	LicenseName    string `json:"license_name"`
	LicenseURL     string `json:"license_url"`
	HFRepo         string `json:"hf_repo"`
	// Logo is an Icon manifest slug (web/src/assets/icons/manifest.ts) or an
	// uploaded data-URL (PUT …/icon with paths.icons_dir unset — Sprint A2).
	Logo        string   `json:"logo"`
	LogoDark    string   `json:"logo_dark"`
	KeyFeatures []string `json:"key_features"`
	// Modalities (Sprint J1) is a subset of text/vision/audio -- validated
	// against that enum in validateModel. See modelJSON.Modalities.
	Modalities []string `json:"modalities"`
	// Visibility is visible (Models gallery) or hidden (Settings only).
	// Empty on create means "visible". Validated in validateModel.
	Visibility string `json:"visibility"`
	// Reason is an optional operator note on WHY this change was made —
	// Sprint C. Never persisted on the model row itself, only folded into
	// the audit_log detail (withReason) so it has a real read surface
	// (GET /api/v1/audit) instead of being pure write-only ceremony.
	Reason string `json:"reason"`
}

// validateModel checks field constraints + family existence.
func (s *Server) validateModel(ctx context.Context, b modelBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 256 {
		fields["name"] = "must be ≤256 characters"
	}
	if msg := validateModalityList(b.Modalities); msg != "" {
		fields["modalities"] = msg
	}
	switch b.Visibility {
	case "", "visible", "hidden":
	default:
		fields["visibility"] = "must be visible or hidden"
	}
	if s.deps.Catalog != nil && b.FamilyID != 0 {
		families, err := s.deps.Catalog.ListFamilies(ctx)
		if err != nil {
			fields["family_id"] = "could not verify"
		} else {
			found := false
			for _, f := range families {
				if f.ID == b.FamilyID {
					found = true
					break
				}
			}
			if !found {
				fields["family_id"] = "does not exist"
			}
		}
	}
	return fields
}
