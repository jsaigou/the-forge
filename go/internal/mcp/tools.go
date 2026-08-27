// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/sched"
)

// toolSpec is one entry in the /v1/tools discovery listing. Mirrors the
// TOOLS list in forge/mcp_server.py at the freeze commit.
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Mutating    bool           `json:"mutating"`
	InputSchema map[string]any `json:"input_schema"`
}

// tools is the MCP tool surface: the Contract 1 §8 freeze set (status,
// can_fit, ensure_loaded, unload, reservation CRUD) plus list_models,
// added by the audit roadmap R2 (docs/v5-mcp-audit.md). It is read-only
// and non-mutating, backed by the same catalog seam a0's /v1/models reads
// — the historical reason for omitting it (no local-model inventory seam)
// no longer applies now that the catalog DB is the source of truth.
// Freeze-set descriptions/schemas match forge/mcp_server.py.
var tools = []toolSpec{
	{
		Name:        "list_models",
		Description: "Loadable/routeable model inventory: visible local configs (ensure_loaded targets) + enabled remote offerings (a0 routes). Same catalog source as GET /v1/models.",
		Mutating:    false,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		Name:        "status",
		Description: "Fleet + queue snapshot: loaded slots, idle times, memory budget, queue depth/positions.",
		Mutating:    false,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		Name:        "can_fit",
		Description: "Would `model` fit right now (read-only fit check; does not trigger any load or eviction)?",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"model": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			},
			"required": []any{"model"},
		},
	},
	{
		Name:        "ensure_loaded",
		Description: "Trigger a load, blocking with a timeout — mirrors what A0 does internally for ordinary traffic, but callable directly for pre-warming.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"model":       map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
				"token_hint":  map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000},
				"timeout":     map[string]any{"type": "number", "minimum": 1, "maximum": 300},
				"target_slot": map[string]any{"type": "string", "maxLength": 32},
			},
			"required": []any{"model"},
		},
	},
	{
		Name:        "unload",
		Description: "Unload a model from whatever slot it's currently loaded in.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"model": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			},
			"required": []any{"model"},
		},
	},
	{
		Name:        "list_reservations",
		Description: "List all reservations (optionally filtered by model, scope, or bay via query params).",
		Mutating:    false,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		Name:        "create_reservation",
		Description: "Create a reservation (subject to the scheduler's ownership/permission model — see docs/scheduler.md).",
		Mutating:    true,
		InputSchema: map[string]any{"type": "object"},
	},
	{
		Name:        "update_reservation",
		Description: "Modify a reservation by label (subject to ownership rules — own reservations always editable; others need allow_agent_reschedule).",
		Mutating:    true,
		InputSchema: map[string]any{"type": "object"},
	},
	{
		Name:        "cancel_reservation",
		Description: "Cancel a reservation by label (subject to ownership rules — own reservations always cancelable; others need allow_agent_cancellation).",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"label": map[string]any{"type": "string", "minLength": 1, "maxLength": 64},
			},
			"required": []any{"label"},
		},
	},
}

// toolFn is a dispatched tool handler. agent is the authenticated key name
// (requested_by / created_by for scheduler calls).
type toolFn func(s *Server, w http.ResponseWriter, r *http.Request, agent string)

// dispatch maps tool name → handler. Built from tools so the discovery
// listing and the callable set can never drift.
var dispatch = map[string]toolFn{
	"list_models":        (*Server).toolListModels,
	"status":             (*Server).toolStatus,
	"can_fit":            (*Server).toolCanFit,
	"ensure_loaded":      (*Server).toolEnsureLoaded,
	"unload":             (*Server).toolUnload,
	"list_reservations":  (*Server).toolListReservations,
	"create_reservation": (*Server).toolCreateReservation,
	"update_reservation": (*Server).toolUpdateReservation,
	"cancel_reservation": (*Server).toolCancelReservation,
}

// toolNames returns the sorted registered tool names (for the 404 body).
func toolNames() []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

// mutationTimeout bounds reservation CRUD scheduler calls, matching
// httpapi's 5s ceiling.
const mutationTimeout = 5 * time.Second

// ── list_models ─────────────────────────────────────────────────────────────

// toolListModels returns the loadable/routeable inventory (audit roadmap
// R2): visible local Configs + enabled remote Offerings, read from the
// same catalog seam a0's BuildModelsResponse consumes, with the same
// visibility/enabled filters and dedup — so an agent choosing a target
// sees exactly what a0 will accept.
func (s *Server) toolListModels(w http.ResponseWriter, r *http.Request, _ string) {
	if s.deps.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	out, err := listModelsJSON(r.Context(), s.deps.Catalog)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "tool_failed", "message": "catalog read failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "total": len(out)})
}

// ── status ────────────────────────────────────────────────────────────────

func (s *Server) toolStatus(w http.ResponseWriter, _ *http.Request, _ string) {
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}
	st := s.deps.Sched.Status()
	writeJSON(w, http.StatusOK, statusJSON(st))
}

// ── can_fit ───────────────────────────────────────────────────────────────

func (s *Server) toolCanFit(w http.ResponseWriter, r *http.Request, _ string) {
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeBadJSON(w, err)
		return
	}
	if fields := validateModelField(body.Model); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine not wired")
		return
	}
	fit, err := s.deps.Engine.CanFit(body.Model)
	if err != nil {
		writeSchedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":          body.Model,
		"fits":           fit.Fits,
		"required_bytes": fit.RequiredBytes,
		"free_bytes":     fit.FreeBytes,
		"reason":         fit.Reason,
	})
}

// ── ensure_loaded ─────────────────────────────────────────────────────────

func (s *Server) toolEnsureLoaded(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		Model      string   `json:"model"`
		TokenHint  *int     `json:"token_hint"`
		Timeout    *float64 `json:"timeout"`
		TargetSlot string   `json:"target_slot"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeBadJSON(w, err)
		return
	}
	fields := validateModelField(body.Model)
	if body.TokenHint != nil && (*body.TokenHint < 0 || *body.TokenHint > 1_000_000) {
		fields["token_hint"] = "must be between 0 and 1000000"
	}
	timeout := 150.0
	if body.Timeout != nil {
		timeout = *body.Timeout
		if timeout < 1 || timeout > 300 {
			fields["timeout"] = "must be between 1 and 300"
		}
	}
	if len(body.TargetSlot) > 32 {
		fields["target_slot"] = "must be at most 32 characters"
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}

	// token_hint resolves to the SmallJob queue-jump bool via the frozen
	// helper (absent hint → 0 → small, V4 parity). SmallJobFromHint needs
	// the live config for the threshold.
	tokenHint := 0
	if body.TokenHint != nil {
		tokenHint = *body.TokenHint
	}
	small := sched.SmallJobFromHint(s.deps.Sched.Config(), tokenHint)

	ctx, cancel := context.WithTimeout(r.Context(),
		time.Duration(timeout*float64(time.Second)))
	defer cancel()

	ticket, err := s.deps.Sched.EnsureLoaded(ctx, sched.EnsureRequest{
		Model:       body.Model,
		RequestedBy: agent,
		TargetSlot:  body.TargetSlot,
		SmallJob:    small,
	})
	if err != nil {
		// A load timeout is a normal outcome for an MCP caller (pre-warm
		// gave up), not a server error — 200 with a failure body, mirroring
		// V4's success=False/"Timed out" dict. The load was still triggered
		// and may be in flight, so the attempt is audited.
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out") {
			s.audit(r, agent, "mcp_ensure_loaded", body.Model, "status=timeout")
			resp := map[string]any{
				"success": false,
				"model":   body.Model,
				"message": err.Error(),
			}
			// reason is a stable sched.RefusalReason code (Sprint 1, a0 load
			// visibility) recovered from LoadStatus's outcome ring — freshly
			// written by the EnsureLoaded call just above — so a caller can
			// switch on it instead of parsing message's prose. "" when the
			// timeout wasn't a placement refusal (e.g. still queued behind a
			// busy load with no refusal recorded yet).
			if reason := s.deps.Sched.LoadStatus(body.Model).Reason; reason != "" {
				resp["reason"] = reason
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		writeSchedError(w, err)
		return
	}
	s.audit(r, agent, "mcp_ensure_loaded", body.Model, "status="+ticket.Status)
	writeJSON(w, http.StatusOK, ensureResultJSON(ticket))
}

// ── unload ────────────────────────────────────────────────────────────────

func (s *Server) toolUnload(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeBadJSON(w, err)
		return
	}
	if fields := validateModelField(body.Model); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}

	// V4's scheduler.unload(model) took a model; the frozen V5
	// sched.Unload takes a slot, so resolve model → slot from the live
	// status snapshot first. A model that is not loaded anywhere is a
	// no-op success (V4 parity — unloading is idempotent and unowned;
	// see the Phase 7 handoff, "Unload performs no reservation check").
	slot := ""
	for sl, mode := range s.deps.Sched.Status().Slots {
		if mode == body.Model {
			slot = sl
			break
		}
	}
	if slot == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"model":   body.Model,
			"message": "model " + body.Model + " is not loaded",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mutationTimeout)
	defer cancel()
	if err := s.deps.Sched.Unload(ctx, slot, agent); err != nil {
		writeSchedError(w, err)
		return
	}
	s.audit(r, agent, "mcp_unload", body.Model, "slot="+slot)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"model":   body.Model,
		"slot":    slot,
		"message": "Unloaded " + body.Model + " from " + slot,
	})
}

// ── list_reservations ─────────────────────────────────────────────────────

func (s *Server) toolListReservations(w http.ResponseWriter, r *http.Request, _ string) {
	out := []map[string]any{}
	if s.deps.Sched == nil {
		writeJSON(w, http.StatusOK, map[string]any{"reservations": out, "total": 0})
		return
	}
	q := r.URL.Query()
	modelFilter := q.Get("model")
	scopeFilter := q.Get("scope")
	bayFilter := q.Get("bay")
	for _, rsv := range s.deps.Sched.Reservations() {
		if modelFilter != "" && rsv.Model != modelFilter {
			continue
		}
		if scopeFilter != "" && rsv.Scope != scopeFilter {
			continue
		}
		if bayFilter != "" && rsv.Bay != bayFilter {
			continue
		}
		out = append(out, reservationJSON(rsv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"reservations": out, "total": len(out)})
}

// ── create_reservation ────────────────────────────────────────────────────

func (s *Server) toolCreateReservation(w http.ResponseWriter, r *http.Request, agent string) {
	body, fields, err := decodeReservationBody(r)
	if err != nil {
		writeBadJSON(w, err)
		return
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}

	// created_by is the agent key name (never taken from the body). The
	// allow_agent_* tri-state resolves through ResolveAgentFlags — an
	// agent-created reservation defaults open (true/true) when the fields
	// are absent (Phase 7 handoff).
	allowResched, allowCancel := sched.ResolveAgentFlags(agent, body.AllowAgentReschedule, body.AllowAgentCancellation)
	rsv := body.toReservation(agent, allowResched, allowCancel)

	ctx, cancel := context.WithTimeout(r.Context(), mutationTimeout)
	defer cancel()
	if err := s.deps.Sched.CreateReservation(ctx, rsv); err != nil {
		writeSchedError(w, err)
		return
	}
	s.audit(r, agent, "mcp_reservation_create", rsv.Label, "")
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "label": rsv.Label})
}

// ── update_reservation ────────────────────────────────────────────────────

func (s *Server) toolUpdateReservation(w http.ResponseWriter, r *http.Request, agent string) {
	body, fields, err := decodeReservationBody(r)
	if err != nil {
		writeBadJSON(w, err)
		return
	}
	// The label may come from a ?label= query param (V4 mcp semantics) or
	// the body; the query param wins when present.
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if label == "" {
		label = body.Label
	}
	if label == "" {
		writeError(w, http.StatusBadRequest, "label query parameter or body field required")
		return
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}

	// The frozen UpdateReservation carries the requester identity in
	// r.CreatedBy (Phase 7 handoff); the scheduler preserves the stored
	// creator and never transfers ownership. allow_agent_* here are the
	// desired new values on the reservation, resolved with the agent
	// identity's tri-state default.
	allowResched, allowCancel := sched.ResolveAgentFlags(agent, body.AllowAgentReschedule, body.AllowAgentCancellation)
	rsv := body.toReservation(agent, allowResched, allowCancel)
	rsv.Label = label

	ctx, cancel := context.WithTimeout(r.Context(), mutationTimeout)
	defer cancel()
	if err := s.deps.Sched.UpdateReservation(ctx, label, rsv); err != nil {
		writeSchedError(w, err)
		return
	}
	s.audit(r, agent, "mcp_reservation_update", label, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "label": label})
}

// ── cancel_reservation ────────────────────────────────────────────────────

func (s *Server) toolCancelReservation(w http.ResponseWriter, r *http.Request, agent string) {
	var body struct {
		Label string `json:"label"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeBadJSON(w, err)
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" || len(label) > 64 {
		writeValidationError(w, map[string]string{"label": "must be 1–64 characters"})
		return
	}
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mutationTimeout)
	defer cancel()
	if err := s.deps.Sched.CancelReservation(ctx, label, agent); err != nil {
		writeSchedError(w, err)
		return
	}
	s.audit(r, agent, "mcp_reservation_cancel", label, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "label": label})
}

// decodeBody decodes the request JSON into v. An empty body is treated as
// {} (read-only tools ignore the body; validation catches missing required
// fields). A malformed non-empty body is an error.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
