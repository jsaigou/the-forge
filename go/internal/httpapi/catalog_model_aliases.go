// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_model_aliases.go — CRUD for ModelAlias (per-request thinking
// control, Sprint T3, 2026-09-14; see store.ModelAlias's doc comment).
// Mirrors catalog_capability_tiers.go's pattern exactly. Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

// modelAliasJSON mirrors store.ModelAlias.
type modelAliasJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ConfigID int64  `json:"config_id"`
	// RequestDefaults is forced onto every request routed through this
	// alias — overwrites even a client-sent value. Never nil.
	RequestDefaults map[string]any `json:"request_defaults"`
	Visibility      string         `json:"visibility"` // visible | hidden
}

func modelAliasToJSON(a store.ModelAlias) modelAliasJSON {
	defaults := a.RequestDefaults
	if defaults == nil {
		defaults = map[string]any{}
	}
	return modelAliasJSON{ID: a.ID, Name: a.Name, ConfigID: a.ConfigID, RequestDefaults: defaults, Visibility: a.Visibility}
}

type modelAliasBody struct {
	Name            string         `json:"name"`
	ConfigID        int64          `json:"config_id"`
	RequestDefaults map[string]any `json:"request_defaults"`
	Visibility      string         `json:"visibility"`
	// Reason is an optional operator note on WHY this alias exists — same
	// treatment as modelBody.Reason elsewhere in this package.
	Reason string `json:"reason"`
}

func (s *Server) validateModelAlias(ctx context.Context, b modelAliasBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	cat := s.deps.Catalog
	if !modeNameRE.MatchString(b.Name) {
		fields["name"] = "must match " + modeNamePattern
	}
	switch b.Visibility {
	case "", "visible", "hidden":
	default:
		fields["visibility"] = "must be visible or hidden"
	}
	if cat != nil {
		if existing, err := cat.ModelAliasByName(ctx, b.Name); err == nil && existing.ID != excludeID {
			fields["name"] = "already exists"
		}
		if b.ConfigID == 0 {
			fields["config_id"] = "is required"
		} else if _, err := cat.GetConfig(ctx, b.ConfigID); err != nil {
			fields["config_id"] = "does not exist"
		}
	}
	return fields
}

func (s *Server) handleCatalogModelAliasesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []modelAliasJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListModelAliases(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "model aliases query failed")
		return
	}
	out := make([]modelAliasJSON, 0, len(list))
	for _, a := range list {
		out = append(out, modelAliasToJSON(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogModelAliasGet(w http.ResponseWriter, r *http.Request) {
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
	a, err := cat.GetModelAlias(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model alias not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, modelAliasToJSON(a))
}

func (s *Server) handleCatalogModelAliasCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b modelAliasBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if fields := s.validateModelAlias(ctx, b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	id, err := cat.CreateModelAlias(ctx, store.ModelAlias{
		Name: b.Name, ConfigID: b.ConfigID, RequestDefaults: b.RequestDefaults, Visibility: b.Visibility,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_alias_create", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	a, _ := cat.GetModelAlias(ctx, id)
	writeJSON(w, http.StatusCreated, modelAliasToJSON(a))
}

func (s *Server) handleCatalogModelAliasUpdate(w http.ResponseWriter, r *http.Request) {
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
	var b modelAliasBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if fields := s.validateModelAlias(ctx, b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if err := cat.UpdateModelAlias(ctx, store.ModelAlias{
		ID: id, Name: b.Name, ConfigID: b.ConfigID, RequestDefaults: b.RequestDefaults, Visibility: b.Visibility,
	}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model alias not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_alias_update", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	a, _ := cat.GetModelAlias(ctx, id)
	writeJSON(w, http.StatusOK, modelAliasToJSON(a))
}

func (s *Server) handleCatalogModelAliasDelete(w http.ResponseWriter, r *http.Request) {
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
	a, err := cat.GetModelAlias(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model alias not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	if err := cat.DeleteModelAlias(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model alias not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_alias_delete", strconv.FormatInt(id, 10), withReason(a.Name, r.URL.Query().Get("reason")))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
