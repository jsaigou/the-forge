// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_virtual_models.go — CRUD for VirtualModel (2026-09-15; see
// store.VirtualModel's doc comment). Mirrors catalog_capability_tiers.go's
// pattern exactly. Mutating routes are requireRole(admin) +
// requireAssurance(page.settings); see httpapi.go's route table.

import (
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

// virtualModelJSON mirrors store.VirtualModel.
type virtualModelJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Kind is "capability_tier" (ranks CapabilityTierID's members) or
	// "throughput" (ranks by real measured decode_tps, ignores tiers).
	Kind             string `json:"kind"`
	CapabilityTierID int64  `json:"capability_tier_id"`
	Visibility       string `json:"visibility"`
	Notes            string `json:"notes"`
}

func virtualModelToJSON(m store.VirtualModel) virtualModelJSON {
	return virtualModelJSON{
		ID: m.ID, Name: m.Name, Kind: m.Kind,
		CapabilityTierID: m.CapabilityTierID, Visibility: m.Visibility, Notes: m.Notes,
	}
}

type virtualModelBody struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	CapabilityTierID int64  `json:"capability_tier_id"`
	Visibility       string `json:"visibility"`
	Notes            string `json:"notes"`
}

func validateVirtualModel(b virtualModelBody) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 256 {
		fields["name"] = "must be ≤256 characters"
	}
	switch b.Kind {
	case "capability_tier":
		if b.CapabilityTierID == 0 {
			fields["capability_tier_id"] = "is required when kind is capability_tier"
		}
	case "throughput":
		if b.CapabilityTierID != 0 {
			fields["capability_tier_id"] = "must be empty when kind is throughput"
		}
	default:
		fields["kind"] = "must be one of: capability_tier, throughput"
	}
	switch b.Visibility {
	case "", "visible", "hidden":
	default:
		fields["visibility"] = "must be one of: (empty), visible, hidden"
	}
	return fields
}

func (s *Server) handleCatalogVirtualModelsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []virtualModelJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListVirtualModels(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "virtual models query failed")
		return
	}
	out := make([]virtualModelJSON, 0, len(list))
	for _, m := range list {
		out = append(out, virtualModelToJSON(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogVirtualModelGet(w http.ResponseWriter, r *http.Request) {
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
	m, err := cat.GetVirtualModel(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "virtual model not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, virtualModelToJSON(m))
}

func (s *Server) handleCatalogVirtualModelCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b virtualModelBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateVirtualModel(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateVirtualModel(ctx, store.VirtualModel{
		Name: b.Name, Kind: b.Kind, CapabilityTierID: b.CapabilityTierID,
		Visibility: b.Visibility, Notes: b.Notes,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_virtual_model_create", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	m, _ := cat.GetVirtualModel(ctx, id)
	writeJSON(w, http.StatusCreated, virtualModelToJSON(m))
}

func (s *Server) handleCatalogVirtualModelUpdate(w http.ResponseWriter, r *http.Request) {
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
	var b virtualModelBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateVirtualModel(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.UpdateVirtualModel(ctx, store.VirtualModel{
		ID: id, Name: b.Name, Kind: b.Kind, CapabilityTierID: b.CapabilityTierID,
		Visibility: b.Visibility, Notes: b.Notes,
	}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "virtual model not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_virtual_model_update", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	m, _ := cat.GetVirtualModel(ctx, id)
	writeJSON(w, http.StatusOK, virtualModelToJSON(m))
}

func (s *Server) handleCatalogVirtualModelDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := cat.DeleteVirtualModel(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "virtual model not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_virtual_model_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
