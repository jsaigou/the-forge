// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"time"

	"github.com/jsaigou/the-forge/internal/maintenance"
)

// handleMaintenanceGet — GET /api/v1/maintenance. Always 200 with the
// current state (Active: false when no window is open); 503 only when the
// gate itself was never wired.
func (s *Server) handleMaintenanceGet(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Maintenance == nil {
		writeError(w, http.StatusServiceUnavailable, "maintenance gate not wired")
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Maintenance.Status())
}

// maintenanceEnterBody is POST /api/v1/maintenance's request body.
// DurationMinutes is optional; zero/absent falls back to
// maintenance.DefaultMaxDuration (the Gate clamps either way).
type maintenanceEnterBody struct {
	Reason           string   `json:"reason"`
	DurationMinutes  int      `json:"duration_minutes"`
	AffectedSlots    []string `json:"affected_slots"`
	AffectedServices []string `json:"affected_services"`
}

// handleMaintenanceEnter — POST /api/v1/maintenance. Opens a new window.
// 409 if one is already active (the operator/procedure must end it first —
// see maintenance.Gate.Enter's doc comment for why there's no silent
// extend-in-place).
func (s *Server) handleMaintenanceEnter(w http.ResponseWriter, r *http.Request) {
	if s.deps.Maintenance == nil {
		writeError(w, http.StatusServiceUnavailable, "maintenance gate not wired")
		return
	}
	var b maintenanceEnterBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Reason == "" {
		writeValidationError(w, map[string]string{"reason": "required"})
		return
	}

	actor := identity(r).Name
	st, err := s.deps.Maintenance.Enter(maintenance.EnterRequest{
		Reason:           b.Reason,
		EnteredBy:        actor,
		AffectedSlots:    b.AffectedSlots,
		AffectedServices: b.AffectedServices,
		Duration:         time.Duration(b.DurationMinutes) * time.Minute,
	})
	if err == maintenance.ErrAlreadyActive {
		writeError(w, http.StatusConflict, "a maintenance window is already active")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, actor, "maintenance_enter", st.LeaseID, b.Reason)
	writeJSON(w, http.StatusOK, st)
}

// handleMaintenanceExit — DELETE /api/v1/maintenance. Always a forced exit:
// only an operator can reach this route at all, and the Console's "end
// maintenance" control must work regardless of who (or what procedure)
// opened the window. 409 if nothing is active.
func (s *Server) handleMaintenanceExit(w http.ResponseWriter, r *http.Request) {
	if s.deps.Maintenance == nil {
		writeError(w, http.StatusServiceUnavailable, "maintenance gate not wired")
		return
	}
	actor := identity(r).Name
	prev, err := s.deps.Maintenance.Exit("", true)
	if err == maintenance.ErrNotActive {
		writeError(w, http.StatusConflict, "no maintenance window is active")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, actor, "maintenance_exit", prev.LeaseID, "")
	writeJSON(w, http.StatusOK, s.deps.Maintenance.Status())
}
