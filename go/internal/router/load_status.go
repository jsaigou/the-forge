// SPDX-License-Identifier: Apache-2.0

package router

import (
	"net/http"
)

// loadStatus implements GET /v1/load-status?model=<name> (Sprint 1, a0 load
// visibility, 2026-08-27). docs/scheduler.md §"Consumer Model" states, as a
// design property, that a model not currently loaded is invisible to an a0
// consumer — EnsureLoaded just blocks the chat-completion request for up to
// ensure_loaded_timeout_s with zero progress signal. This gives a consumer
// (or an operator/agent watching on its behalf) something to poll from a
// second connection while that request is still in flight.
//
// Deliberately model-keyed, not ticket-keyed: a consumer only ever knows
// the model name it asked for. A ticket ID would arrive too late to be
// useful anyway — both a0 EnsureLoaded call sites (routing.go) discard the
// ticket, and a0 only writes its response after EnsureLoaded returns, by
// which point any load the ticket described is already finished. See
// sched.Scheduler.LoadStatus's doc comment for the resolution order.
func (s *Server) loadStatus(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r).ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
		return
	}
	if s.deps.Sched == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler not configured"})
		return
	}
	model := r.URL.Query().Get("model")
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model query parameter is required"})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Sched.LoadStatus(model))
}
