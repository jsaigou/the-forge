// SPDX-License-Identifier: Apache-2.0

package httpapi

// favorites_handlers.go — Console config-card starring (product/QA sprint,
// 2026-07-29). Unlike most mutations in this file (providers, catalog,
// keys), starring affects only the calling user's own view, not shared
// system state — so these routes are gated on authentication alone (any
// role), not requireRole(operator/admin), matching the read-only status/
// usage routes' gating rather than the write routes'.

import (
	"net/http"
	"strconv"
)

type favoritesResponse struct {
	SubjectIDs []int64 `json:"subject_ids"`
}

// handleFavoritesList — GET /api/v1/favorites?subject_type=config.
func (s *Server) handleFavoritesList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Favorites == nil {
		writeJSON(w, http.StatusOK, favoritesResponse{SubjectIDs: []int64{}})
		return
	}
	subjectType := r.URL.Query().Get("subject_type")
	if subjectType == "" {
		subjectType = "config"
	}
	list, err := s.deps.Favorites.List(r.Context(), identity(r).Name, subjectType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list favorites")
		return
	}
	ids := make([]int64, len(list))
	for i, f := range list {
		ids[i] = f.SubjectID
	}
	writeJSON(w, http.StatusOK, favoritesResponse{SubjectIDs: ids})
}

// handleFavoriteAdd — PUT /api/v1/favorites/{subject_type}/{id} (star).
func (s *Server) handleFavoriteAdd(w http.ResponseWriter, r *http.Request) {
	if s.deps.Favorites == nil {
		writeError(w, http.StatusServiceUnavailable, "favorites not wired")
		return
	}
	subjectType, id, ok := favoriteSubject(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	if err := s.deps.Favorites.Add(r.Context(), identity(r).Name, subjectType, id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to add favorite")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFavoriteRemove — DELETE /api/v1/favorites/{subject_type}/{id} (un-star).
func (s *Server) handleFavoriteRemove(w http.ResponseWriter, r *http.Request) {
	if s.deps.Favorites == nil {
		writeError(w, http.StatusServiceUnavailable, "favorites not wired")
		return
	}
	subjectType, id, ok := favoriteSubject(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	if err := s.deps.Favorites.Remove(r.Context(), identity(r).Name, subjectType, id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove favorite")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func favoriteSubject(r *http.Request) (subjectType string, id int64, ok bool) {
	subjectType = r.PathValue("subject_type")
	if subjectType == "" {
		subjectType = "config"
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return "", 0, false
	}
	return subjectType, id, true
}
