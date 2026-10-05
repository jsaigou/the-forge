// SPDX-License-Identifier: Apache-2.0

package httpapi

// context_creation_handlers.go — read side of the a0 creation ledger
// (CONTRACTS C2, WS-N1). Sizes and names only; the ledger never holds content.

import (
	"net/http"
)

// handleContextCreation — GET /api/v1/context/creation?window=24h|7d|30d&by=tool|consumer|model.
func (s *Server) handleContextCreation(w http.ResponseWriter, r *http.Request) {
	if s.deps.CtxLedger == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "context ledger"}, "context ledger not wired")
		return
	}
	q := r.URL.Query()
	window, by := q.Get("window"), q.Get("by")
	if window == "" {
		window = "24h"
	}
	if by == "" {
		by = "tool"
	}
	res, ok, err := s.deps.CtxLedger.Query(r.Context(), window, by)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{
			"window": "must be one of 24h, 7d, 30d",
			"by":     "must be one of tool, consumer, model",
		}, map[string]string{"window": "must_be_one_of", "by": "must_be_one_of"})
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleContextCreationMetrics — GET /api/v1/context/creation/metrics:
// Prometheus text exposition of the C2 counters.
func (s *Server) handleContextCreationMetrics(w http.ResponseWriter, _ *http.Request) {
	if s.deps.CtxLedger == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "context ledger"}, "context ledger not wired")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.deps.CtxLedger.WriteMetrics(w)
}
