// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_kb.go — smith's P4 knowledge-base API (docs/v5-smith.md §4.7/§5):
//
//	GET /api/v1/smith/kb/search?q=&limit=   → ranked corpus + live-DB matches
//	GET /api/v1/smith/kb/blocked            → operator-local blocked-work tracker, parsed
//	GET /api/v1/smith/kb/{ref}              → resolve one KBRef to its chunk
//
// All routes are operator-gated (registered in httpapi.go), read-only —
// same shape-freeze precedent as smith_handlers.go: smith.KBResult /
// smith.Chunk / smith.BlockedItem carry snake_case JSON tags and are
// marshaled directly, the smith package owns its own wire shapes.

import (
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/smith"
)

// smithKBSearchResponse is the GET /api/v1/smith/kb/search body.
type smithKBSearchResponse struct {
	Count   int              `json:"count"`
	Results []smith.KBResult `json:"results"`
}

// handleSmithKBSearch ranks the embedded doc corpus plus live-DB evidence
// (notifications, mode_history, audit_log, model_profiles, smith_findings)
// against ?q=, capped by ?limit= (kb.go's kbSearchLimitDefault/Max).
func (s *Server) handleSmithKBSearch(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeValidationError(w, map[string]string{"q": "required"})
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeValidationError(w, map[string]string{"limit": "must be a non-negative integer"})
			return
		}
		limit = n
	}
	results, err := s.deps.Smith.KBSearch(r.Context(), q, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "kb search failed")
		return
	}
	writeJSON(w, http.StatusOK, smithKBSearchResponse{Count: len(results), Results: results})
}

// smithKBRefResponse is the GET /api/v1/smith/kb/{ref} body.
type smithKBRefResponse struct {
	Chunk smith.Chunk `json:"chunk"`
}

// handleSmithKBRef resolves one KBRef (e.g. "pitfalls:gtt-ceiling", exactly
// what a Finding's kb_refs carries) to its full chunk — the finding-card
// expansion on Diagnostics. 404 on an unknown ref.
func (s *Server) handleSmithKBRef(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	ref := r.PathValue("ref")
	chunk, ok := s.deps.Smith.KBLookup(ref)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown kb ref")
		return
	}
	writeJSON(w, http.StatusOK, smithKBRefResponse{Chunk: chunk})
}

// smithKBBlockedResponse is the GET /api/v1/smith/kb/blocked body.
type smithKBBlockedResponse struct {
	Count int                 `json:"count"`
	Items []smith.BlockedItem `json:"items"`
}

// handleSmithKBBlocked returns the blocked-work tracker's parsed items, file
// order preserved (not numeric — see smith.BlockedItem's doc comment).
func (s *Server) handleSmithKBBlocked(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	items := s.deps.Smith.ListBlockedItems()
	writeJSON(w, http.StatusOK, smithKBBlockedResponse{Count: len(items), Items: items})
}
