// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_services.go — Service CRUD (split from catalog_handlers.go,
// Sprint 5 code-quality cleanup, #33). Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

type serviceJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Color       string `json:"color"`
	Unit        string `json:"unit"`
	HealthCheck string `json:"health_check"`
}

func serviceToJSON(s store.Service) serviceJSON {
	return serviceJSON{
		ID: s.ID, Name: s.Name, Label: s.Label, Description: s.Description,
		Icon: s.Icon, Color: s.Color, Unit: s.Unit, HealthCheck: s.HealthCheck,
	}
}

func (s *Server) handleCatalogServicesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []serviceJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListServices(ctx)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "services query failed")
		return
	}
	out := make([]serviceJSON, 0, len(list))
	for _, sv := range list {
		out = append(out, serviceToJSON(sv))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogServiceGet(w http.ResponseWriter, r *http.Request) {
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
	sv, err := cat.GetService(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "service"}, "service not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, serviceToJSON(sv))
}

func (s *Server) handleCatalogServiceCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b serviceBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields, codes := s.validateService(r.Context(), b, 0); len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateService(ctx, store.Service{
		Name: b.Name, Label: b.Label, Description: b.Description,
		Icon: b.Icon, Color: b.Color, Unit: b.Unit, HealthCheck: b.HealthCheck,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_service_create", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	sv, _ := cat.GetService(ctx, id)
	writeJSON(w, http.StatusCreated, serviceToJSON(sv))
}

func (s *Server) handleCatalogServiceUpdate(w http.ResponseWriter, r *http.Request) {
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
	var b serviceBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields, codes := s.validateService(r.Context(), b, id); len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateService(ctx, store.Service{
		ID: id, Name: b.Name, Label: b.Label, Description: b.Description,
		Icon: b.Icon, Color: b.Color, Unit: b.Unit, HealthCheck: b.HealthCheck,
	})
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "service"}, "service not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_service_update", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	sv, _ := cat.GetService(ctx, id)
	writeJSON(w, http.StatusOK, serviceToJSON(sv))
}

func (s *Server) handleCatalogServiceDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := cat.DeleteService(ctx, id); err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "service"}, "service not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_service_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type serviceBody struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Color       string `json:"color"`
	Unit        string `json:"unit"`
	HealthCheck string `json:"health_check"`
}

// validateService checks field constraints + name uniqueness. The second
// return value is codes' i18n Phase 2 companion (field -> stable error
// code) — see writeValidationErrorCodes' doc comment in httpapi.go.
func (s *Server) validateService(ctx context.Context, b serviceBody, excludeID int64) (map[string]string, map[string]string) {
	fields := map[string]string{}
	codes := map[string]string{}
	if !modeNameRE.MatchString(b.Name) {
		fields["name"] = "must match " + modeNamePattern
	}
	if s.deps.Catalog != nil {
		if existing, err := s.deps.Catalog.ServiceByName(ctx, b.Name); err == nil && existing.ID != excludeID {
			fields["name"] = "already exists"
			codes["name"] = "already_exists"
		}
	}
	return fields, codes
}
