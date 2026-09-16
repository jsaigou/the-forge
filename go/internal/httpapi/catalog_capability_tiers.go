// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_capability_tiers.go — CRUD for CapabilityTier (capability-tier substitution,
// Sprint P1, 2026-09-13; see store.CapabilityTier's doc comment). Mirrors
// catalog_taxonomy.go's Genealogy CRUD pattern exactly — a small (id, name)
// vocabulary table with one extra field (mode). Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

// capabilityTierJSON mirrors store.CapabilityTier.
type capabilityTierJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Mode overrides router.capability_substitution for every config in this class;
	// "" inherits the global setting. One of "", "off", "fallback_only",
	// "prefer_smarter".
	Mode  string `json:"mode"`
	Notes string `json:"notes"`
}

func capabilityTierToJSON(p store.CapabilityTier) capabilityTierJSON {
	return capabilityTierJSON{ID: p.ID, Name: p.Name, Mode: p.Mode, Notes: p.Notes}
}

type capabilityTierBody struct {
	Name  string `json:"name"`
	Mode  string `json:"mode"`
	Notes string `json:"notes"`
}

func validateCapabilityTier(b capabilityTierBody) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 256 {
		fields["name"] = "must be ≤256 characters"
	}
	switch b.Mode {
	case "", "off", "fallback_only", "prefer_smarter":
	default:
		fields["mode"] = "must be one of: (empty), off, fallback_only, prefer_smarter"
	}
	return fields
}

func (s *Server) handleCatalogCapabilityTiersList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []capabilityTierJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListCapabilityTiers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "capability tiers query failed")
		return
	}
	out := make([]capabilityTierJSON, 0, len(list))
	for _, p := range list {
		out = append(out, capabilityTierToJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogCapabilityTierGet(w http.ResponseWriter, r *http.Request) {
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
	p, err := cat.GetCapabilityTier(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "capability tier not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, capabilityTierToJSON(p))
}

func (s *Server) handleCatalogCapabilityTierCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b capabilityTierBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateCapabilityTier(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateCapabilityTier(ctx, store.CapabilityTier{Name: b.Name, Mode: b.Mode, Notes: b.Notes})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_capability_tier_create", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	p, _ := cat.GetCapabilityTier(ctx, id)
	writeJSON(w, http.StatusCreated, capabilityTierToJSON(p))
}

func (s *Server) handleCatalogCapabilityTierUpdate(w http.ResponseWriter, r *http.Request) {
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
	var b capabilityTierBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateCapabilityTier(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.UpdateCapabilityTier(ctx, store.CapabilityTier{ID: id, Name: b.Name, Mode: b.Mode, Notes: b.Notes}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "capability tier not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_capability_tier_update", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	p, _ := cat.GetCapabilityTier(ctx, id)
	writeJSON(w, http.StatusOK, capabilityTierToJSON(p))
}

// handleCatalogCapabilityTierDelete deletes a capability tier. Configs
// referencing it fall back to no class (ON DELETE SET NULL) — i.e. they
// stop participating in substitution — rather than being deleted or
// refused.
func (s *Server) handleCatalogCapabilityTierDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := cat.DeleteCapabilityTier(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "capability tier not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_capability_tier_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
