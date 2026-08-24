// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_investigations.go — the smith investigations API (docs/v5-smith.md
// §4.4/§5). This wave ships the P1 CRUD surface: list, manual open, detail
// (with findings), run-more-checks, resolve/dismiss, and the check catalog.
// The anomaly hook (bus subscription → auto-open) lives in the smith package
// itself (investigations.go); this file is the HTTP surface only.
//
// All routes are operator-gated (registered in httpapi.go). No /analyze
// endpoint — that's P3 (Tier 2 reasoning over an investigation).

import (
	"net/http"

	"github.com/jsaigou/the-forge/internal/smith"
)

// smithInvestigationsResponse is the GET /api/v1/smith/investigations body.
type smithInvestigationsResponse struct {
	Count          int                   `json:"count"`
	Investigations []smith.Investigation `json:"investigations"`
}

// handleSmithInvestigationsList lists investigations, newest first
// (?status=open|in_progress|resolved|dismissed).
func (s *Server) handleSmithInvestigationsList(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	status := r.URL.Query().Get("status")
	invs, err := s.deps.Smith.ListInvestigations(r.Context(), status)
	if err == smith.ErrStoreUnwired {
		writeJSON(w, http.StatusOK, smithInvestigationsResponse{Investigations: []smith.Investigation{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list investigations failed")
		return
	}
	writeJSON(w, http.StatusOK, smithInvestigationsResponse{Count: len(invs), Investigations: invs})
}

// smithInvestigationCreateBody is the POST /api/v1/smith/investigations body.
type smithInvestigationCreateBody struct {
	Trigger string `json:"trigger"`
	Summary string `json:"summary"`
}

// handleSmithInvestigationCreate opens a manual investigation.
func (s *Server) handleSmithInvestigationCreate(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	var b smithInvestigationCreateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Trigger == "" {
		b.Trigger = "manual"
	}
	if b.Trigger != "manual" {
		writeValidationError(w, map[string]string{"trigger": "must be 'manual' (anomaly investigations are auto-opened)"})
		return
	}
	id, err := s.deps.Smith.CreateInvestigation(r.Context(), b.Trigger, b.Summary)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create investigation failed")
		return
	}
	// Return the full investigation by fetching it from the store.
	inv, _, err := s.deps.Smith.GetInvestigation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "fetch created investigation failed")
		return
	}
	s.audit(r, identity(r).Name, "smith_investigation_create", b.Trigger, "")
	writeJSON(w, http.StatusCreated, inv)
}

// smithInvestigationDetailResponse is the GET /api/v1/smith/investigations/{id}
// body — the investigation metadata plus its findings trail.
type smithInvestigationDetailResponse struct {
	smith.Investigation
	Findings []smith.StoredFinding `json:"findings"`
}

// handleSmithInvestigationDetail returns an investigation's metadata plus
// its findings.
func (s *Server) handleSmithInvestigationDetail(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid investigation id")
		return
	}
	inv, findings, err := s.deps.Smith.GetInvestigation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "investigation not found")
		return
	}
	writeJSON(w, http.StatusOK, smithInvestigationDetailResponse{
		Investigation: *inv,
		Findings:      findings,
	})
}

// smithInvestigationChecksBody is the POST /api/v1/smith/investigations/{id}/checks
// request body: explicit check_ids or a scope (quick|deep). check_ids win
// when both are present.
type smithInvestigationChecksBody struct {
	CheckIDs []string `json:"check_ids"`
	Scope    string   `json:"scope"`
}

// smithInvestigationChecksResponse is the POST /api/v1/smith/investigations/{id}/checks
// response.
type smithInvestigationChecksResponse struct {
	Count    int             `json:"count"`
	Worst    string          `json:"worst"`
	Findings []smith.Finding `json:"findings"`
}

// handleSmithInvestigationChecks runs more checks into an investigation and
// attaches the findings.
func (s *Server) handleSmithInvestigationChecks(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid investigation id")
		return
	}
	var b smithInvestigationChecksBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	scope := b.Scope
	if scope == "" && len(b.CheckIDs) == 0 {
		scope = smith.ScopeQuick // a bare POST runs the quick sweep
	}
	if len(b.CheckIDs) == 0 && scope != smith.ScopeQuick && scope != smith.ScopeDeep {
		writeValidationError(w, map[string]string{"scope": "must be one of quick, deep (or provide check_ids)"})
		return
	}

	findings, err := s.deps.Smith.RunChecksIntoInvestigation(r.Context(), id, b.CheckIDs, scope, smith.SweepManual)
	if err == smith.ErrAlreadyRunning {
		writeError(w, http.StatusConflict, "a smith sweep is already in progress")
		return
	}
	if err != nil {
		writeValidationError(w, map[string]string{"checks": err.Error()})
		return
	}

	s.audit(r, identity(r).Name, "smith_investigation_checks", scopeOrIDs(scope, b.CheckIDs), "")
	writeJSON(w, http.StatusOK, smithInvestigationChecksResponse{
		Count:    len(findings),
		Worst:    string(worstOf(findings)),
		Findings: findings,
	})
}

// smithInvestigationResolveBody is the PATCH /api/v1/smith/investigations/{id}
// request body.
type smithInvestigationResolveBody struct {
	Status string `json:"status"`
}

// handleSmithInvestigationResolve sets an investigation's status to
// resolved or dismissed (operator+).
func (s *Server) handleSmithInvestigationResolve(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid investigation id")
		return
	}
	var b smithInvestigationResolveBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Status != "resolved" && b.Status != "dismissed" {
		writeValidationError(w, map[string]string{"status": "must be resolved or dismissed"})
		return
	}

	if err := s.deps.Smith.ResolveInvestigation(r.Context(), id, b.Status); err != nil {
		writeError(w, http.StatusInternalServerError, "resolve investigation failed")
		return
	}

	// Return the updated investigation.
	inv, _, err := s.deps.Smith.GetInvestigation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "investigation not found")
		return
	}
	s.audit(r, identity(r).Name, "smith_investigation_resolve", b.Status, "")
	writeJSON(w, http.StatusOK, inv)
}

// smithChecksResponse is the GET /api/v1/smith/checks body.
type smithChecksResponse struct {
	Count  int               `json:"count"`
	Checks []smith.CheckMeta `json:"checks"`
}

// handleSmithChecksList returns the check catalog metadata for the FE picker.
func (s *Server) handleSmithChecksList(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	checks := s.deps.Smith.ListChecks()
	writeJSON(w, http.StatusOK, smithChecksResponse{Count: len(checks), Checks: checks})
}
