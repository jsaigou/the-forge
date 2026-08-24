// SPDX-License-Identifier: Apache-2.0

package httpapi

// handlers.go — after the Sprint 0 domain split
// (docs/v5-sprint0-contract-freeze.md §0.1) this file holds only the
// package-shared handler helpers plus the two handlers that belong to no
// feature domain: the read-only modes list and the generic not-implemented
// stub. Every feature endpoint now lives in its own <domain>_handlers.go.
//
// Handlers read collector snapshots / config / sched / store interfaces
// only — never probe systemd, sysfs, or llama-server directly (design
// decision 2).

import (
	"net/http"
	"strings"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/store"
)

// handleModesList returns the config-defined modes (Contract 1 §5 parity
// surface). C1-Q3: mutations return 501 by design (read-only config).
func (s *Server) handleModesList(w http.ResponseWriter, _ *http.Request) {
	modes := map[string]any{}
	if s.deps.Config != nil {
		cfg := s.deps.Config()
		for name, m := range cfg.Modes {
			modes[name] = map[string]any{
				"label":       m.Label,
				"family":      m.Family,
				"description": m.Description,
				"color":       m.Color,
				"icon":        m.Icon,
				"tags":        m.Tags,
				"default":     m.Default,
				"type":        m.Type,
				"services":    m.Services,
			}
		}
	}
	writeJSON(w, http.StatusOK, modesListResponse{Modes: modes})
}

// handleNotImplemented is the stub for endpoints pending Contract
// amendments (C1-Q2 systemctl-needing, C1-Q3 modes CRUD, C1-Q5 router
// settings PUT). Returns 501 with a clear error message identifying the
// open question.
func (s *Server) handleNotImplemented(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "not implemented in Phase 4 — see internal/httpapi package doc (C1-Q2/Q3/Q5)",
		"path":  r.URL.Path,
	})
}

// ── Small helpers ────────────────────────────────────────────────────────────

// snapshot returns the latest collector snapshot (nil if no source wired).
func (s *Server) snapshot() *collector.Snapshot {
	if s.deps.Snapshots == nil {
		return nil
	}
	return s.deps.Snapshots.Current()
}

// unitActive reports whether the named systemd unit is active in the
// snapshot. False when the unit is absent (treats unknown as inactive,
// matching V4 _service_active's OSError fallback).
func unitActive(snap *collector.Snapshot, unit string) bool {
	if snap == nil || unit == "" {
		return false
	}
	if u, ok := snap.Units[unit]; ok {
		return u.Active()
	}
	return false
}

// firstNonEmpty returns s when non-empty, else fallback.
func firstNonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// deref returns *p's value, or fallback when p is nil.
func deref(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// derefBool returns *p, or fallback when nil.
func derefBool(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

// derefInt returns *p, or fallback when nil.
func derefInt(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

// isConflict reports whether err is a scheduler reservation conflict
// (V4 raises ValueError for conflicts; sched.CreateReservation returns a
// wrapped error in V5 — the stub never produces these, but real
// implementations should use a sentinel type).
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "conflict") || strings.Contains(err.Error(), "overlaps")
}

// isNotFound reports whether err is a not-found error.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if err == store.ErrNotFound {
		return true
	}
	return strings.Contains(err.Error(), "not found")
}

// isPermissionDenied reports whether err is a reservation permission
// denial (V4 scheduler.ReservationPermissionError).
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "permission") || strings.Contains(err.Error(), "denied")
}
