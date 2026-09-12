// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_autonomy_handlers.go — GET/PUT /api/v1/smith/autonomy (autonomous-
// remediation Sprint 5, docs/v5-smith.md §13.3). The standing autonomy
// policy: opt-in per-procedure, default off everywhere, gated by a global
// kill switch (smith.autonomy — go/internal/smith/autonomy.go).
//
// Step-up is conditional on the DIRECTION of the change — escalating trust
// (turning the global switch on, or opting in a procedure that wasn't
// already) requires the action.smith.autonomy step-up; lowering it never
// does, the same "always the safe direction" posture reject/checkpoint-abort
// use elsewhere in this API. Because the decision depends on the request
// BODY (which must be parsed first), it can't be expressed as a route-level
// requireAssurance(...)/requireStrictAssurance(...) middleware wrap the way
// every other step-up-gated route in this file is — so this handler calls
// the shared evaluateAssurance helper directly on the escalating path.
//
// It deliberately uses the requireStrictAssurance shape (no bearer bypass),
// not requireAssurance's blanket one: escalating smith's standing autonomy
// is at least as sensitive as key management (#37), since it's the switch
// that lets smith execute system-modifying procedures unattended.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/smith"
)

type smithAutonomyProcedureBody struct {
	Enabled         bool `json:"enabled"`
	CooldownSeconds int  `json:"cooldown_seconds"`
	MaxPerDay       int  `json:"max_per_day"`
}

type smithAutonomyBody struct {
	Enabled    bool                                  `json:"enabled"`
	Procedures map[string]smithAutonomyProcedureBody `json:"procedures"`
}

type smithAutonomyEligibleProcedure struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type smithAutonomyResponse struct {
	Policy             smithAutonomyBody                `json:"policy"`
	EligibleProcedures []smithAutonomyEligibleProcedure `json:"eligible_procedures"`
}

func toAutonomyBody(p smith.AutonomyPolicy) smithAutonomyBody {
	procs := make(map[string]smithAutonomyProcedureBody, len(p.Procedures))
	for id, pa := range p.Procedures {
		procs[id] = smithAutonomyProcedureBody{Enabled: pa.Enabled, CooldownSeconds: pa.CooldownSeconds, MaxPerDay: pa.MaxPerDay}
	}
	return smithAutonomyBody{Enabled: p.Enabled, Procedures: procs}
}

func (s *Server) smithAutonomyView(ctx context.Context) smithAutonomyResponse {
	eligible := []smithAutonomyEligibleProcedure{}
	for _, p := range smith.AutonomyEligibleProcedures() {
		eligible = append(eligible, smithAutonomyEligibleProcedure{ID: p.ID, Title: p.Title})
	}
	return smithAutonomyResponse{
		Policy:             toAutonomyBody(s.deps.Smith.AutonomyPolicy(ctx)),
		EligibleProcedures: eligible,
	}
}

// handleSmithAutonomyGet — GET /api/v1/smith/autonomy (operator).
func (s *Server) handleSmithAutonomyGet(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.smithAutonomyView(r.Context()))
}

// handleSmithAutonomyPut — PUT /api/v1/smith/autonomy (operator; step-up
// required only when the request escalates — see file doc comment).
func (s *Server) handleSmithAutonomyPut(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var b smithAutonomyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}

	eligible := map[string]bool{}
	for _, p := range smith.AutonomyEligibleProcedures() {
		eligible[p.ID] = true
	}
	fieldErrs := map[string]string{}
	for id, pa := range b.Procedures {
		if !eligible[id] {
			fieldErrs["procedures."+id] = "not an autonomy-eligible procedure"
			continue
		}
		if pa.CooldownSeconds < 0 {
			fieldErrs["procedures."+id+".cooldown_seconds"] = "must not be negative"
		}
		if pa.MaxPerDay < 0 {
			fieldErrs["procedures."+id+".max_per_day"] = "must not be negative"
		}
	}
	if len(fieldErrs) > 0 {
		writeValidationError(w, fieldErrs)
		return
	}

	ctx := r.Context()
	current := s.deps.Smith.AutonomyPolicy(ctx)
	escalating := b.Enabled && !current.Enabled
	for id, pa := range b.Procedures {
		if pa.Enabled && !current.Procedures[id].Enabled {
			escalating = true
		}
	}
	if escalating {
		ident, ok := r.Context().Value(identityKey).(authz.Identity)
		if !ok {
			writeError(w, http.StatusUnauthorized, "Authentication required")
			return
		}
		// No bearer bypass here — unlike requireAssurance's blanket one for
		// ordinary a0/MCP traffic, escalating smith's standing autonomy is at
		// least as sensitive as key management (#37's requireStrictAssurance
		// carve-out), since it's the switch that lets smith execute
		// system-modifying procedures unattended. Every identity is
		// evaluated through the same evaluateAssurance path requireAssurance/
		// requireStrictAssurance use; a bearer identity carries no session
		// assurance, so it always fails a password-or-above resource here —
		// only a stepped-up browser session can flip this switch on.
		allowed, decision, err := s.evaluateAssurance(ctx, ident, authz.ResourceActionSmithAutonomy)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "policy load failed")
			return
		}
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "step_up_required", "required": string(decision.Required), "resource": decision.Resource,
			})
			return
		}
	}

	procs := make(map[string]smith.ProcedureAutonomy, len(b.Procedures))
	for id, pa := range b.Procedures {
		procs[id] = smith.ProcedureAutonomy{Enabled: pa.Enabled, CooldownSeconds: pa.CooldownSeconds, MaxPerDay: pa.MaxPerDay}
	}
	raw, err := json.Marshal(smith.AutonomyPolicy{Enabled: b.Enabled, Procedures: procs})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(ctx, smith.SettingAutonomy, raw); err != nil {
		writeInternalError(w, err)
		return
	}

	s.audit(r, identity(r).Name, "smith_autonomy_update", "smith.autonomy", "")
	writeJSON(w, http.StatusOK, s.smithAutonomyView(ctx))
}
