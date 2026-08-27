// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_configs.go — Config CRUD (split from catalog_handlers.go,
// Sprint 5 code-quality cleanup, #33). Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

type configJSON struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name"`
	VariantID        int64    `json:"variant_id"`
	WeightArtifactID int64    `json:"weight_artifact_id"`
	EngineID         int64    `json:"engine_id"`
	BuildID          int64    `json:"build_id"`
	MMProjArtifactID int64    `json:"mmproj_artifact_id"`
	NCtx             int      `json:"n_ctx"`
	Parallel         int      `json:"parallel"`
	ExtraArgs        []string `json:"extra_args"`
	Status           string   `json:"status"`
	Visibility       string   `json:"visibility"`
	IsDefault        bool     `json:"is_default"`
	Fingerprint      string   `json:"fingerprint"`
	// Logo is a config-level icon override (Sprint I) — wins over the
	// model/family/genealogy chain when set; "" inherits. See
	// genealogyJSON.Logo's doc comment.
	Logo     string `json:"logo"`
	LogoDark string `json:"logo_dark"`
	// Modalities overrides the model's default when this config can't
	// deliver everything the model architecturally supports (missing
	// mmproj, a build lacking mtmd support, a quantization that stripped a
	// modality). nil = derive (store.Config.Modalities's doc comment);
	// omitzero keeps a derive-mode config's response body clean instead of
	// emitting a literal `"modalities":null` on every card-adjacent config
	// read.
	Modalities *[]string `json:"modalities,omitzero"`
}

func configToJSON(c store.Config) configJSON {
	return configJSON{
		ID: c.ID, Name: c.Name, VariantID: c.VariantID,
		WeightArtifactID: c.WeightArtifactID, EngineID: c.EngineID, BuildID: c.BuildID,
		MMProjArtifactID: c.MMProjArtifactID, NCtx: c.NCtx, Parallel: c.Parallel,
		ExtraArgs: c.ExtraArgs, Status: c.Status, Visibility: c.Visibility,
		IsDefault: c.IsDefault, Fingerprint: c.Fingerprint, Logo: c.Logo, LogoDark: c.LogoDark,
		Modalities: c.Modalities,
	}
}

func (s *Server) handleCatalogConfigsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []configJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Config
	if vid := r.URL.Query().Get("variant_id"); vid != "" {
		id, err := strconv.ParseInt(vid, 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"variant_id": "must be an integer"})
			return
		}
		list, err = cat.ListConfigsForVariant(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "configs query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListConfigs(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "configs query failed")
			return
		}
	}
	out := make([]configJSON, 0, len(list))
	for _, c := range list {
		out = append(out, configToJSON(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogConfigGet(w http.ResponseWriter, r *http.Request) {
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
	c, err := cat.GetConfig(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, configToJSON(c))
}

func (s *Server) handleCatalogConfigCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b configBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateConfig(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateConfig(ctx, store.Config{
		Name: b.Name, VariantID: b.VariantID, WeightArtifactID: b.WeightArtifactID,
		EngineID: b.EngineID, BuildID: b.BuildID, MMProjArtifactID: b.MMProjArtifactID,
		NCtx: b.NCtx, Parallel: b.Parallel, ExtraArgs: b.ExtraArgs,
		Status: b.Status, Visibility: b.Visibility, IsDefault: b.IsDefault,
		Fingerprint: b.Fingerprint, Logo: b.Logo, LogoDark: b.LogoDark, Modalities: b.Modalities,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_create", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	s.invalidateCfg()
	c, _ := cat.GetConfig(ctx, id)
	writeJSON(w, http.StatusCreated, configToJSON(c))
}

func (s *Server) handleCatalogConfigUpdate(w http.ResponseWriter, r *http.Request) {
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
	var b configBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateConfig(r.Context(), b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateConfig(ctx, store.Config{
		ID: id, Name: b.Name, VariantID: b.VariantID, WeightArtifactID: b.WeightArtifactID,
		EngineID: b.EngineID, BuildID: b.BuildID, MMProjArtifactID: b.MMProjArtifactID,
		NCtx: b.NCtx, Parallel: b.Parallel, ExtraArgs: b.ExtraArgs,
		Status: b.Status, Visibility: b.Visibility, IsDefault: b.IsDefault,
		Fingerprint: b.Fingerprint, Logo: b.Logo, LogoDark: b.LogoDark, Modalities: b.Modalities,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_update", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	s.invalidateCfg()
	c, _ := cat.GetConfig(ctx, id)
	writeJSON(w, http.StatusOK, configToJSON(c))
}

func (s *Server) handleCatalogConfigDelete(w http.ResponseWriter, r *http.Request) {
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
	c, err := cat.GetConfig(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	// Delete semantics (Q4): refuse to delete a Config currently loaded unless
	// force=true is set.
	if !r.URL.Query().Has("force") && s.isConfigLoaded(c.Name) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "config is currently loaded",
			"config": c.Name,
		})
		return
	}
	if err := cat.DeleteConfig(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_delete", strconv.FormatInt(id, 10), withReason(c.Name, r.URL.Query().Get("reason")))
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogConfigValidate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b configBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateConfig(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

// isConfigLoaded checks whether the collector snapshot shows any slot with
// this config name loaded.
func (s *Server) isConfigLoaded(name string) bool {
	if s.deps.Snapshots == nil {
		return false
	}
	snap := s.deps.Snapshots.Current()
	if snap == nil {
		return false
	}
	for _, ss := range snap.Slots {
		if ss.Mode == name {
			return true
		}
	}
	return false
}

type configBody struct {
	Name             string   `json:"name"`
	VariantID        int64    `json:"variant_id"`
	WeightArtifactID int64    `json:"weight_artifact_id"`
	EngineID         int64    `json:"engine_id"`
	BuildID          int64    `json:"build_id"`
	MMProjArtifactID int64    `json:"mmproj_artifact_id"`
	NCtx             int      `json:"n_ctx"`
	Parallel         int      `json:"parallel"`
	ExtraArgs        []string `json:"extra_args"`
	Status           string   `json:"status"`
	Visibility       string   `json:"visibility"`
	IsDefault        bool     `json:"is_default"`
	Fingerprint      string   `json:"fingerprint"`
	// Logo is a config-level icon override (Sprint I); "" inherits via the
	// model/family/genealogy chain. See genealogyJSON.Logo's doc comment.
	Logo     string `json:"logo"`
	LogoDark string `json:"logo_dark"`
	// Modalities overrides the model default; nil = derive. See
	// configJSON.Modalities.
	Modalities *[]string `json:"modalities,omitzero"`
	// Reason is an optional operator note on WHY this change was made —
	// Sprint C. See modelBody.Reason's doc comment; same treatment here.
	Reason string `json:"reason"`
}

// validateConfig checks field constraints + referential integrity + name uniqueness.
func (s *Server) validateConfig(ctx context.Context, b configBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	cat := s.deps.Catalog

	if !modeNameRE.MatchString(b.Name) {
		fields["name"] = "must match " + modeNamePattern
	}
	if b.Modalities != nil {
		if msg := validateModalityList(*b.Modalities); msg != "" {
			fields["modalities"] = msg
		}
	}
	if cat != nil {
		// Name uniqueness (unless updating the same config).
		if existing, err := cat.ConfigByName(ctx, b.Name); err == nil && existing.ID != excludeID {
			fields["name"] = "already exists"
		}
		// Variant existence.
		if b.VariantID == 0 {
			fields["variant_id"] = "is required"
		} else if _, err := cat.GetVariant(ctx, b.VariantID); err != nil {
			fields["variant_id"] = "does not exist"
		}
		// Weight artifact existence + type check.
		if b.WeightArtifactID == 0 {
			fields["weight_artifact_id"] = "is required"
		} else if a, err := cat.GetArtifact(ctx, b.WeightArtifactID); err != nil {
			fields["weight_artifact_id"] = "does not exist"
		} else if a.ArtifactType != "weight" {
			fields["weight_artifact_id"] = "must be a weight artifact"
		}
		// Engine existence.
		if b.EngineID == 0 {
			fields["engine_id"] = "is required"
		} else {
			engines, _ := cat.ListEngines(ctx)
			found := false
			for _, e := range engines {
				if e.ID == b.EngineID {
					found = true
					break
				}
			}
			if !found {
				fields["engine_id"] = "does not exist"
			}
		}
		// Build existence — required, not optional. A Config with no Build
		// has no defined backend/binary; historically this silently defaulted
		// to launching the vulkan binary regardless of what the model
		// actually needed (see store.Build's Backend doc comment). Every
		// Config must reference a real Build explicitly.
		if b.BuildID == 0 {
			fields["build_id"] = "is required"
		} else {
			builds, _ := cat.ListBuilds(ctx)
			found := false
			for _, bd := range builds {
				if bd.ID == b.BuildID {
					found = true
					break
				}
			}
			if !found {
				fields["build_id"] = "does not exist"
			}
		}
		// MMProj artifact existence + type check (optional).
		if b.MMProjArtifactID != 0 {
			if a, err := cat.GetArtifact(ctx, b.MMProjArtifactID); err != nil {
				fields["mmproj_artifact_id"] = "does not exist"
			} else if a.ArtifactType != "mmproj" {
				fields["mmproj_artifact_id"] = "must be an mmproj artifact"
			}
		}
	}
	if b.NCtx < 0 {
		fields["n_ctx"] = "must be ≥ 0"
	}
	if b.Parallel < 0 {
		fields["parallel"] = "must be ≥ 0"
	}
	switch b.Status {
	case "", "unverified", "verified":
	default:
		fields["status"] = "must be unverified or verified"
	}
	switch b.Visibility {
	case "", "visible", "hidden":
	default:
		fields["visibility"] = "must be visible or hidden"
	}
	return fields
}
