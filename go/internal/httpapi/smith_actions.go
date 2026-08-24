// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_actions.go — the smith action-model API (P2, docs/v5-smith.md §4.6):
// list/create/detail/approve/reject/handoff over smith.Action. The state
// machine, self-eviction stamping, and execution/post-verify all live in
// internal/smith (actions.go/execute.go/handoff.go); this file is the HTTP
// surface + authz only. Same house conventions as smith_handlers.go /
// smith_investigations.go: smithOK's 503 guard, identity(r), s.audit,
// writeError/writeValidationError/writeJSON, decodeJSONBody, parseID.
//
// Gating (docs/v5-smith.md §4.6 / the P2 plan's §6): create + approve carry
// requireAssurance(action.smith.execute) — create because a low-assurance
// session planting a payload for a distracted admin to approve is a real
// confused-deputy path, approve because it's what actually executes.
// reject and handoff are ungated: reject is always the safe direction, and
// handoff-resolution short of approval carries no privilege beyond what
// create already required to exist.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/smith"
)

// smithActionsResponse is the GET /api/v1/smith/actions body.
type smithActionsResponse struct {
	Count        int            `json:"count"`
	PendingCount int            `json:"pending_count"`
	Actions      []smith.Action `json:"actions"`
}

// validActionKinds bounds smithActionCreateBody.Kind.
var validActionKinds = map[string]bool{
	smith.KindRunbook:            true,
	smith.KindLoadConfig:         true,
	smith.KindUnloadSlot:         true,
	smith.KindRestartForgeUnit: true,
	smith.KindSettingsChange:     true,
	smith.KindCatalogChange:      true,
	smith.KindDeleteFiles:        true,
	smith.KindProcedure:          true,
}

// validActionRisks bounds smithActionCreateBody.Risk.
var validActionRisks = map[string]bool{
	smith.RiskInfo: true,
	smith.RiskLow:  true,
	smith.RiskHigh: true,
}

// validHandoffResolutions bounds smithActionHandoffBody.Resolution.
var validHandoffResolutions = map[string]bool{
	"runbook":     true,
	"acknowledge": true,
	"remote":      true,
	"cancel":      true,
}

// handleSmithActionsList lists actions (?status=&investigation_id=).
func (s *Server) handleSmithActionsList(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	q := r.URL.Query()
	status := q.Get("status")

	var invID *int64
	if raw := q.Get("investigation_id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid investigation_id")
			return
		}
		invID = &n
	}

	actions, err := s.deps.Smith.ListActions(r.Context(), status, invID, 0)
	if err == smith.ErrStoreUnwired {
		writeJSON(w, http.StatusOK, smithActionsResponse{Actions: []smith.Action{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list actions failed")
		return
	}
	pending, err := s.deps.Smith.PendingActionCount(r.Context())
	if err != nil {
		// Non-fatal — the list itself succeeded; the chip count degrades to 0
		// rather than failing the whole response.
		pending = 0
	}
	writeJSON(w, http.StatusOK, smithActionsResponse{Count: len(actions), PendingCount: pending, Actions: actions})
}

// smithActionCreateBody is the POST /api/v1/smith/actions request body.
type smithActionCreateBody struct {
	Kind            string          `json:"kind"`
	Title           string          `json:"title"`
	Detail          json.RawMessage `json:"detail"`
	Risk            string          `json:"risk"`
	InvestigationID *int64          `json:"investigation_id"`
}

// smithHandoffRequiredBody is the 409 body shared by create and approve
// (the plan's §6 frozen shape).
type smithHandoffRequiredBody struct {
	Error    string        `json:"error"`
	ActionID int64         `json:"action_id"`
	Handoff  smith.Handoff `json:"handoff"`
	Message  string        `json:"message"`
}

// writeHandoffRequired emits the frozen 409 handoff_required body for e.
func writeHandoffRequired(w http.ResponseWriter, e *smith.HandoffRequiredError, message string) {
	writeJSON(w, http.StatusConflict, smithHandoffRequiredBody{
		Error:    "handoff_required",
		ActionID: e.ActionID,
		Handoff:  e.Handoff,
		Message:  message,
	})
}

// handleSmithActionCreate creates a manual action proposal. Gated
// (action.smith.execute) — the draft's payload is what a subsequent approve
// executes, so planting one is not a safe-by-default operation even though
// creation alone doesn't run anything.
func (s *Server) handleSmithActionCreate(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	var b smithActionCreateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if !validActionKinds[b.Kind] {
		writeError(w, http.StatusBadRequest, "kind must be one of runbook, load_config, unload_slot, restart_forge_unit, settings_change, catalog_change, delete_files, procedure")
		return
	}
	if !validActionRisks[b.Risk] {
		writeError(w, http.StatusBadRequest, "risk must be one of info, low, high")
		return
	}
	if b.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}

	draft := smith.ActionDraft{
		Kind: b.Kind, Title: b.Title, Risk: b.Risk, Detail: b.Detail,
		InvestigationID: b.InvestigationID, CreatedBy: identity(r).Name,
	}
	a, err := s.deps.Smith.CreateAction(r.Context(), draft)
	if err != nil {
		var handoffErr *smith.HandoffRequiredError
		if errors.As(err, &handoffErr) {
			writeHandoffRequired(w, handoffErr, fmt.Sprintf(
				"action %d requires a handoff resolution before it can be approved (smith's brain is on slot %s)",
				handoffErr.ActionID, handoffErr.Handoff.BrainSlot))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, identity(r).Name, "smith_action_create", fmt.Sprintf("%d:%s", a.ID, a.Kind), "")
	writeJSON(w, http.StatusCreated, map[string]any{"action": a})
}

// handleSmithActionDetail returns one action by ID.
func (s *Server) handleSmithActionDetail(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	a, err := s.deps.Smith.GetAction(r.Context(), id)
	if err != nil {
		writeActionFetchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": a})
}

// writeActionFetchError distinguishes "no such action" (404) from a real
// store failure (500) — GetAction wraps sql.ErrNoRows via %w for a missing
// id, so errors.Is sees through the wrap.
func writeActionFetchError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "action lookup failed")
}

// handleSmithActionApprove approves a pending action. Gated
// (action.smith.execute) — this is the mutation that actually executes.
// Returns 202 when execution continues in the background (the common case:
// ApproveAction CASs pending->approved and hands off to a goroutine before
// this handler ever sees the result), or 200 when ApproveAction's own
// runbook short-circuit already reached a terminal status (done_unverified)
// synchronously — a runbook is never executed by smith, so there's nothing
// left running when this handler returns.
func (s *Server) handleSmithActionApprove(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	a, err := s.deps.Smith.ApproveAction(r.Context(), id, identity(r).Name)
	if err != nil {
		var handoffErr *smith.HandoffRequiredError
		if errors.As(err, &handoffErr) {
			writeHandoffRequired(w, handoffErr, fmt.Sprintf(
				"action %d requires a handoff resolution before it can be approved (smith's brain is on slot %s)",
				handoffErr.ActionID, handoffErr.Handoff.BrainSlot))
			return
		}
		if errors.Is(err, smith.ErrInvalidTransition) {
			status := ""
			if cur, gerr := s.deps.Smith.GetAction(r.Context(), id); gerr == nil {
				status = cur.Status
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state", "status": status})
			return
		}
		writeActionFetchError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "smith_action_approve", fmt.Sprintf("%d:%s", a.ID, a.Kind), "")
	code := http.StatusAccepted
	if a.Status != smith.StatusApproved {
		// The runbook short-circuit (approveRunbook) already reached a
		// terminal status synchronously — nothing is executing.
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{"action": a})
}

// handleSmithActionRecheck re-runs a "done — I ran it myself" runbook's
// source check(s) (§5.5). Gated operator-only (like the investigations
// resolve route, which is the closest analogue — a clean re-check for an
// investigation-attached runbook closes the investigation, but nothing is
// executed and no step-up assurance is required beyond the role).
func (s *Server) handleSmithActionRecheck(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	a, err := s.deps.Smith.RecheckRunbook(r.Context(), id, identity(r).Name)
	if err != nil {
		if errors.Is(err, smith.ErrInvalidTransition) {
			status := ""
			if cur, gerr := s.deps.Smith.GetAction(r.Context(), id); gerr == nil {
				status = cur.Status
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state", "status": status})
			return
		}
		writeActionFetchError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "smith_action_recheck", fmt.Sprintf("%d:%s", a.ID, a.Kind), "")
	writeJSON(w, http.StatusOK, map[string]any{"action": a})
}

// handleSmithActionReject rejects a pending action. Ungated — always the
// safe direction.
func (s *Server) handleSmithActionReject(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	a, err := s.deps.Smith.RejectAction(r.Context(), id, identity(r).Name)
	if err != nil {
		if errors.Is(err, smith.ErrInvalidTransition) {
			status := ""
			if cur, gerr := s.deps.Smith.GetAction(r.Context(), id); gerr == nil {
				status = cur.Status
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state", "status": status})
			return
		}
		writeActionFetchError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "smith_action_reject", fmt.Sprintf("%d:%s", a.ID, a.Kind), "")
	writeJSON(w, http.StatusOK, map[string]any{"action": a})
}

// smithActionHandoffBody is the POST /api/v1/smith/actions/{id}/handoff
// request body.
type smithActionHandoffBody struct {
	Resolution string `json:"resolution"`
}

// handleSmithActionHandoff resolves a self-evicting action's handoff state.
// Ungated: approve already carries the step-up gate, and stepping up twice
// in one flow is friction for zero additional safety (both mutations are
// audited either way).
func (s *Server) handleSmithActionHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	var b smithActionHandoffBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if !validHandoffResolutions[b.Resolution] {
		writeError(w, http.StatusBadRequest, "resolution must be one of runbook, acknowledge, remote, cancel")
		return
	}

	a, err := s.deps.Smith.ResolveHandoff(r.Context(), id, b.Resolution, identity(r).Name)
	if err != nil {
		if errors.Is(err, smith.ErrInvalidTransition) {
			status := ""
			if cur, gerr := s.deps.Smith.GetAction(r.Context(), id); gerr == nil {
				status = cur.Status
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state", "status": status})
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "action not found")
			return
		}
		// Every other ResolveHandoff error is a precondition failure (e.g.
		// "not awaiting a runbook") rather than a not-found.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, identity(r).Name, "smith_handoff_"+b.Resolution, fmt.Sprintf("%d:%s", a.ID, a.Kind), "")
	writeJSON(w, http.StatusOK, map[string]any{"action": a})
}

// handleSmithProcedureRun returns actionID's procedure run (steps executed
// so far, current status, checkpoint note when paused) — autonomous-
// remediation Sprint 2. 404 when the action has no run yet (wrong kind, or
// approval hasn't dispatched it), mirroring handleSmithActionDetail's
// not-found convention.
func (s *Server) handleSmithProcedureRun(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	run, err := s.deps.Smith.GetProcedureRun(r.Context(), id)
	if err != nil {
		writeActionFetchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

// handleSmithProcedureCheckpointApprove clears an awaiting_checkpoint run's
// gate and resumes it in the background. Gated (action.smith.execute) —
// same posture as approve, since this is what lets execution continue.
func (s *Server) handleSmithProcedureCheckpointApprove(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	a, err := s.deps.Smith.ApproveProcedureCheckpoint(r.Context(), id, identity(r).Name)
	if err != nil {
		if errors.Is(err, smith.ErrInvalidTransition) {
			status := ""
			if cur, gerr := s.deps.Smith.GetAction(r.Context(), id); gerr == nil {
				status = cur.Status
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state", "status": status})
			return
		}
		writeActionFetchError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "smith_procedure_checkpoint_approve", fmt.Sprintf("%d:%s", a.ID, a.Kind), "")
	writeJSON(w, http.StatusAccepted, map[string]any{"action": a})
}

// handleSmithProcedureCheckpointAbort ends an awaiting_checkpoint run
// without continuing it. Ungated beyond the operator role — always the safe
// direction, same posture as reject.
func (s *Server) handleSmithProcedureCheckpointAbort(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	a, err := s.deps.Smith.AbortProcedureRun(r.Context(), id, identity(r).Name)
	if err != nil {
		if errors.Is(err, smith.ErrInvalidTransition) {
			status := ""
			if cur, gerr := s.deps.Smith.GetAction(r.Context(), id); gerr == nil {
				status = cur.Status
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state", "status": status})
			return
		}
		writeActionFetchError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "smith_procedure_checkpoint_abort", fmt.Sprintf("%d:%s", a.ID, a.Kind), "")
	writeJSON(w, http.StatusOK, map[string]any{"action": a})
}

// handleSmithActionProcedurePreview returns the downtime-disclosure data for
// procedurizing a pending atomic action ("let smith fix it", autonomous-
// remediation Sprint 3, docs/v5-smith.md §13). Read-only, role-only (no
// step-up) — mirrors handleSmithActionDetail's posture, since this is
// exactly that: a read the confirmation modal needs before the operator
// commits to anything. 400 when the action's kind has no mapped procedure.
func (s *Server) handleSmithActionProcedurePreview(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	preview, err := s.deps.Smith.ProcedurePreview(r.Context(), id)
	if err != nil {
		if errors.Is(err, smith.ErrNoProcedureForKind) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeActionFetchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": preview})
}

// handleSmithActionProcedurize converts a pending atomic action into its
// mapped procedure action (creates + approves the replacement, supersedes
// the source). Gated (action.smith.execute) — same posture as approve,
// since under the hood this IS an approve of the newly-created procedure
// action. 409 invalid_state when the source isn't pending (already acted on
// by a concurrent request), 400 when the kind has no mapped procedure.
func (s *Server) handleSmithActionProcedurize(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	a, err := s.deps.Smith.Procedurize(r.Context(), id, identity(r).Name)
	if err != nil {
		if errors.Is(err, smith.ErrNoProcedureForKind) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, smith.ErrInvalidTransition) {
			status := ""
			if cur, gerr := s.deps.Smith.GetAction(r.Context(), id); gerr == nil {
				status = cur.Status
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state", "status": status})
			return
		}
		writeActionFetchError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "smith_action_procedurize", fmt.Sprintf("%d:%s", a.ID, a.Kind), "")
	writeJSON(w, http.StatusAccepted, map[string]any{"action": a})
}

// handleSmithProcedureRunsList returns the most recent procedure runs of
// any status, most-recent-first — the run history the supervision/
// evaluation harness (Sprint 4, docs/v5-smith.md §13) browses ahead of the
// Sprint 6 build-refresh capstone runs. ?limit= caps the page (server-side
// default/max applied in Smith.ListProcedureRuns).
func (s *Server) handleSmithProcedureRunsList(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	runs, err := s.deps.Smith.ListProcedureRuns(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list procedure runs failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleSmithProcedureScorecard returns actionID's evaluation scorecard
// (Sprint 4): unattended completion, checkpoints declared/reached,
// post-verify outcome, downtime estimate vs. actual. 404 when the action
// has no procedure run, mirroring handleSmithProcedureRun.
func (s *Server) handleSmithProcedureScorecard(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return
	}
	sc, err := s.deps.Smith.ProcedureScorecard(r.Context(), id)
	if err != nil {
		writeActionFetchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scorecard": sc})
}
