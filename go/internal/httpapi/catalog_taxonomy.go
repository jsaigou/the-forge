// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_taxonomy.go — CRUD for the two taxonomy levels above Model:
// genealogies and families (split from catalog_handlers.go, Sprint 5
// code-quality cleanup, #33). Mutating routes are requireRole(admin) +
// requireAssurance(page.settings); see httpapi.go's route table.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

// genealogyJSON mirrors store.Genealogy — the level above Family
// (product/QA sprint, 2026-07-29; see store.Genealogy's doc comment).
type genealogyJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Logo is an Icon manifest slug or an uploaded data-URL (Sprint I — icon
	// inheritance hierarchy). Inherited by families/models/configs that
	// don't set their own; see registry.resolveLogos.
	Logo string `json:"logo"`
	// LogoDark is a dark-theme variant override (Phase 3); "" falls back to
	// Logo. Same inheritance chain, resolved together — see
	// registry.resolveLogos's doc comment.
	LogoDark string `json:"logo_dark"`
}

type familyJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	GenealogyID int64  `json:"genealogy_id"`
	// Logo — see genealogyJSON.Logo's doc comment; same inheritance chain.
	Logo     string `json:"logo"`
	LogoDark string `json:"logo_dark"`
}

func familyToJSON(f store.Family) familyJSON {
	return familyJSON{ID: f.ID, Name: f.Name, GenealogyID: f.GenealogyID, Logo: f.Logo, LogoDark: f.LogoDark}
}

func genealogyToJSON(g store.Genealogy) genealogyJSON {
	return genealogyJSON{ID: g.ID, Name: g.Name, Logo: g.Logo, LogoDark: g.LogoDark}
}

func (s *Server) handleCatalogFamiliesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []familyJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListFamilies(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "families query failed")
		return
	}
	out := make([]familyJSON, 0, len(list))
	for _, f := range list {
		out = append(out, familyToJSON(f))
	}
	writeJSON(w, http.StatusOK, out)
}

type familyBody struct {
	Name        string `json:"name"`
	GenealogyID int64  `json:"genealogy_id"`
	Logo        string `json:"logo"`
	LogoDark    string `json:"logo_dark"`
}

func (s *Server) validateFamily(ctx context.Context, b familyBody) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 256 {
		fields["name"] = "must be ≤256 characters"
	}
	if s.deps.Catalog != nil && b.GenealogyID != 0 {
		if _, err := s.deps.Catalog.GetGenealogy(ctx, b.GenealogyID); err != nil {
			fields["genealogy_id"] = "does not exist"
		}
	}
	return fields
}

func (s *Server) handleCatalogFamilyGet(w http.ResponseWriter, r *http.Request) {
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
	f, err := cat.GetFamily(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "family not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, familyToJSON(f))
}

func (s *Server) handleCatalogFamilyCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b familyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateFamily(r.Context(), b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateFamily(ctx, store.Family{Name: b.Name, GenealogyID: b.GenealogyID, Logo: b.Logo, LogoDark: b.LogoDark})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_family_create", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	f, _ := cat.GetFamily(ctx, id)
	writeJSON(w, http.StatusCreated, familyToJSON(f))
}

func (s *Server) handleCatalogFamilyUpdate(w http.ResponseWriter, r *http.Request) {
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
	var b familyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateFamily(r.Context(), b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.UpdateFamily(ctx, store.Family{ID: id, Name: b.Name, GenealogyID: b.GenealogyID, Logo: b.Logo, LogoDark: b.LogoDark}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "family not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_family_update", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	f, _ := cat.GetFamily(ctx, id)
	writeJSON(w, http.StatusOK, familyToJSON(f))
}

// handleCatalogFamilyDelete deletes a family. Models referencing it fall
// back to no family (ON DELETE SET NULL) rather than being refused or
// cascade-deleted — a family is an organizational label, not ownership
// (unlike a model→variant relationship, which is refused with dependents).
func (s *Server) handleCatalogFamilyDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := cat.DeleteFamily(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "family not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_family_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type genealogyBody struct {
	Name     string `json:"name"`
	Logo     string `json:"logo"`
	LogoDark string `json:"logo_dark"`
}

func validateGenealogy(b genealogyBody) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 256 {
		fields["name"] = "must be ≤256 characters"
	}
	return fields
}

func (s *Server) handleCatalogGenealogiesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []genealogyJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListGenealogies(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "genealogies query failed")
		return
	}
	out := make([]genealogyJSON, 0, len(list))
	for _, g := range list {
		out = append(out, genealogyToJSON(g))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogGenealogyGet(w http.ResponseWriter, r *http.Request) {
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
	g, err := cat.GetGenealogy(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "genealogy not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, genealogyToJSON(g))
}

func (s *Server) handleCatalogGenealogyCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b genealogyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateGenealogy(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateGenealogy(ctx, store.Genealogy{Name: b.Name, Logo: b.Logo, LogoDark: b.LogoDark})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_genealogy_create", strconv.FormatInt(id, 10), b.Name)
	g, _ := cat.GetGenealogy(ctx, id)
	writeJSON(w, http.StatusCreated, genealogyToJSON(g))
}

func (s *Server) handleCatalogGenealogyUpdate(w http.ResponseWriter, r *http.Request) {
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
	var b genealogyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateGenealogy(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.UpdateGenealogy(ctx, store.Genealogy{ID: id, Name: b.Name, Logo: b.Logo, LogoDark: b.LogoDark}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "genealogy not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_genealogy_update", strconv.FormatInt(id, 10), b.Name)
	g, _ := cat.GetGenealogy(ctx, id)
	writeJSON(w, http.StatusOK, genealogyToJSON(g))
}

// handleCatalogGenealogyDelete deletes a genealogy. Families referencing it
// fall back to no genealogy (ON DELETE SET NULL) rather than being refused.
func (s *Server) handleCatalogGenealogyDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := cat.DeleteGenealogy(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "genealogy not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_genealogy_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
