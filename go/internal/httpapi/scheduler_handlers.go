// SPDX-License-Identifier: Apache-2.0

package httpapi

// scheduler_handlers.go — lifecycle (switch/load/unload), reservations,
// scheduler config, router settings, and aux-unit control (service-mode/TTS)
// (Contract 1 §2 #10–17, #22–26). Split out of handlers.go by Sprint 0
// (docs/v5-sprint0-contract-freeze.md §0.1); pure move, no behavior change.
// Owner track after split: BE-4.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
)

// ── Reservations + scheduler config ──────────────────────────────────────────

// handleReservationsList returns all reservations (Contract 1 §2 #13).
// Optional query filters: ?model=foo&scope=bay&bay=a1.
func (s *Server) handleReservationsList(w http.ResponseWriter, r *http.Request) {
	out := []reservationResponse{}
	if s.deps.Sched == nil {
		writeJSON(w, http.StatusOK, map[string]any{"reservations": out, "total": 0})
		return
	}
	all := s.deps.Sched.Reservations()
	q := r.URL.Query()
	modelFilter := q.Get("model")
	scopeFilter := q.Get("scope")
	bayFilter := q.Get("bay")
	for _, rsv := range all {
		if modelFilter != "" && rsv.Model != modelFilter {
			continue
		}
		if scopeFilter != "" && rsv.Scope != scopeFilter {
			continue
		}
		if bayFilter != "" && rsv.Bay != bayFilter {
			continue
		}
		out = append(out, reservationResponse{
			Label:                  rsv.Label,
			Model:                  rsv.Model,
			Start:                  isoFormat(rsv.Start),
			End:                    isoFormat(rsv.End),
			Scope:                  rsv.Scope,
			Bay:                    ptrString(rsv.Bay),
			CreatedBy:              rsv.CreatedBy,
			AllowAgentReschedule:   rsv.AllowAgentReschedule,
			AllowAgentCancellation: rsv.AllowAgentCancellation,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reservations": out, "total": len(out)})
}

// handleReservationCreate creates a reservation (Contract 1 §2 #14).
// Dashboard-created reservations always pass created_by="human" — the
// operator UI implies deliberate lock-down intent.
func (s *Server) handleReservationCreate(w http.ResponseWriter, r *http.Request) {
	var b reservationCreateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if _, fields := b.validate(); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}

	// created_by=human — V4 dashboard behavior. The PWA never sends this.
	start, _ := parseISOTime(b.Start)
	end, _ := parseISOTime(b.End)
	rsv := sched.Reservation{
		Label:                  b.Label,
		Model:                  b.Model,
		Start:                  start,
		End:                    end,
		Scope:                  b.Scope,
		Bay:                    b.Bay,
		CreatedBy:              "human",
		AllowAgentReschedule:   derefBool(b.AllowAgentReschedule, false),
		AllowAgentCancellation: derefBool(b.AllowAgentCancellation, false),
	}
	ident := identity(r)
	_ = ident // dashboard always uses "human"; identity only matters for agents
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.deps.Sched.CreateReservation(ctx, rsv); err != nil {
		// V4 distinguishes 409 (conflict) from 422 (validation); the
		// scheduler returns a wrapped error for conflicts.
		if isConflict(err) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, "human", "reservation_create", b.Label, "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action": "reservation_created",
			"label":  b.Label,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "label": b.Label})
}

// handleReservationUpdate updates a reservation (Contract 1 §2 PUT).
func (s *Server) handleReservationUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}
	label := r.PathValue("label")
	var b reservationCreateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if _, fields := b.validate(); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	start, _ := parseISOTime(b.Start)
	end, _ := parseISOTime(b.End)
	rsv := sched.Reservation{
		Label:                  label,
		Model:                  b.Model,
		Start:                  start,
		End:                    end,
		Scope:                  b.Scope,
		Bay:                    b.Bay,
		CreatedBy:              "human",
		AllowAgentReschedule:   derefBool(b.AllowAgentReschedule, false),
		AllowAgentCancellation: derefBool(b.AllowAgentCancellation, false),
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.deps.Sched.UpdateReservation(ctx, label, rsv); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if isPermissionDenied(err) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "permission_denied",
				"message": err.Error(),
			})
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, "human", "reservation_update", label, "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action": "reservation_updated",
			"label":  label,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "label": label})
}

// handleReservationCancel cancels a reservation (Contract 1 §2 #15).
func (s *Server) handleReservationCancel(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}
	label := r.PathValue("label")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.deps.Sched.CancelReservation(ctx, label, "human"); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if isPermissionDenied(err) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "permission_denied",
				"message": err.Error(),
			})
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, "human", "reservation_cancel", label, "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action": "reservation_cancelled",
			"label":  label,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "label": label})
}

// handleSchedulerConfigGet returns the scheduler tunables (Contract 1 §2
// #16).
func (s *Server) handleSchedulerConfigGet(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Sched == nil {
		writeJSON(w, http.StatusOK, schedulerConfigResponse{
			IdleUnloadS:            180,
			SmallJobTokenThreshold: 1500,
			PriorityJumpCap:        2,
			ReservationSoonMin:     10,
		})
		return
	}
	c := s.deps.Sched.Config()
	writeJSON(w, http.StatusOK, schedulerConfigResponse{
		IdleUnloadS:            c.IdleUnloadS,
		SmallJobTokenThreshold: c.SmallJobTokenThreshold,
		PriorityJumpCap:        c.PriorityJumpCap,
		ReservationSoonMin:     c.ReservationSoonMin,
	})
}

// handleSchedulerConfigPut updates the scheduler tunables (Contract 1 §2
// #17, admin role).
func (s *Server) handleSchedulerConfigPut(w http.ResponseWriter, r *http.Request) {
	var b schedulerConfigBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b2, fields := b.validate(); len(fields) > 0 {
		writeValidationError(w, fields)
	} else {
		b = b2
	}
	if s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}
	cfg := sched.Config{
		IdleUnloadS:            derefInt(b.IdleUnloadS, 180),
		SmallJobTokenThreshold: derefInt(b.SmallJobTokenThreshold, 1500),
		PriorityJumpCap:        derefInt(b.PriorityJumpCap, 2),
		ReservationSoonMin:     derefInt(b.ReservationSoonMin, 10),
	}
	if err := s.deps.Sched.SetConfig(cfg); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, "human", "scheduler_config_update", "", "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action": "scheduler_config_updated",
		})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── Lifecycle: switch / load / unload ────────────────────────────────────────

// handleSwitch starts a mode switch (Contract 1 §2 #10). The engine call
// runs in a goroutine; this handler publishes switch_started immediately
// and returns 200 accepted.
func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	if s.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine not wired")
		return
	}
	mode := r.PathValue("mode")
	if s.deps.Config != nil {
		cfg := s.deps.Config()
		if _, ok := cfg.Modes[mode]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": fmt.Sprintf("Unknown mode: %s", mode),
			})
			return
		}
	}

	s.mu.Lock()
	if s.switchState.inProgress {
		target := s.switchState.target
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": fmt.Sprintf("Switch already in progress to %s", target),
		})
		return
	}
	s.switchState.inProgress = true
	s.switchState.target = mode
	s.switchState.startedAt = time.Now()
	s.switchState.lastResult = nil
	startedAt := s.switchState.startedAt
	s.mu.Unlock()

	s.audit(r, "human", "switch_mode", mode, "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("switch_started", map[string]any{
			"target":     mode,
			"started_at": isoFormat(startedAt),
		})
	}

	go s.runSwitchBackground(s.bgCtx, mode)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"message":     fmt.Sprintf("Switch to %s queued", mode),
		"in_progress": true,
	})
}

// runSwitchBackground performs the engine call and emits the completion
// event. Mirrors V4's _run_switch_background.
func (s *Server) runSwitchBackground(parent context.Context, mode string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	result := s.deps.Engine.SwitchMode(ctx, mode)
	out := lifecycleResult{
		Success: result.Success,
		Message: result.Message,
		NCtx:    result.NCtx,
	}
	s.mu.Lock()
	s.switchState.inProgress = false
	s.switchState.target = ""
	s.switchState.startedAt = time.Time{}
	s.switchState.lastResult = &out
	s.mu.Unlock()
	if s.deps.Publish == nil {
		return
	}
	name := "switch_complete"
	if !result.Success {
		name = "switch_failed"
	}
	s.deps.Publish.Publish(name, map[string]any{
		"result": out,
		"status": s.buildStatusResponse(),
	})
}

// handleLoad starts a slot load (Contract 1 §2 #11).
func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	var b slotLoadBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if _, fields := b.validate(); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine not wired")
		return
	}
	if s.deps.Config != nil {
		cfg := s.deps.Config()
		if _, ok := cfg.Modes[b.Mode]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": fmt.Sprintf("Unknown mode: %s", b.Mode),
			})
			return
		}
	}

	// BE-2 (F3, docs/v5-review-fixes.md): this direct-slot-load path calls
	// Engine.Load straight through — unlike the scheduler's EnsureLoaded
	// (a0/MCP), it never consulted memory at all before this fix, so a
	// dashboard-triggered load could over-subscribe every slot with no
	// warning ("4 healthy that can't fit", "no eviction warning"). Reject
	// up front with the fit check's reason (which already names the
	// eviction candidates or explains why nothing would help) instead of
	// starting a load that will silently starve every other slot of
	// context — no silent retry loop; the caller gets one clear answer.
	if fits, reason, requiredBytes, freeBytes, err := s.fitsForSlotLoad(b.Mode, b.Slot); err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("fit check failed: %v", err))
		return
	} else if !fits {
		writeJSON(w, http.StatusConflict, map[string]any{
			"success":        false,
			"error":          "wont_fit",
			"message":        reason,
			"required_bytes": requiredBytes,
			"free_bytes":     freeBytes,
		})
		return
	}

	// §0.6 single-instance load guard: only one instance of a given Config
	// may run at once — two instances break a0 routing (ADR 0006: scope is
	// per-Config, not per-Model; two configs of the same model may coexist
	// on different slots). If this same mode is already loaded or loading
	// on any slot, reject with 409 already_loaded (frozen error code; FE-1c
	// shows a friendly message and disables the button).
	if slot := s.findModeLoadedElsewhere(b.Mode, b.Slot); slot != "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "already_loaded",
			"slot":    slot,
			"message": fmt.Sprintf("%s already loaded on slot %s", b.Mode, slot),
		})
		return
	}

	s.mu.Lock()
	if cur, ok := s.slotLoading[b.Slot]; ok && cur.inProgress {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": fmt.Sprintf("Slot %s already loading", b.Slot),
		})
		return
	}
	s.slotLoading[b.Slot] = slotTransition{
		inProgress: true,
		mode:       b.Mode,
		startedAt:  time.Now(),
	}
	s.mu.Unlock()

	s.audit(r, "human", "load_slot", b.Slot+":"+b.Mode, "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("load_started", map[string]any{
			"slot":   b.Slot,
			"mode":   b.Mode,
			"status": s.buildStatusResponse(),
		})
	}
	go s.runLoadBackground(s.bgCtx, b.Mode, b.Slot)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"message":     fmt.Sprintf("Loading %s into %s", b.Mode, b.Slot),
		"in_progress": true,
	})
}

// fitsForSlotLoad answers the fit check for loading mode into slot,
// excluding slot's own current occupant from the memory-in-use figure.
// Engine.CanFit (Contract 2) has no slot parameter: without this
// adjustment, reloading a slot with a bigger model would be wrongly
// rejected by the BE-2 guard above — Engine.Load always frees the target
// slot's occupant before starting the new one, but a plain CanFit still
// counts that occupant as using memory. Falls back to the plain CanFit
// answer when the engine doesn't expose the optional per-slot footprint
// seam (mirrors sched.Footprints; test doubles like engine.Stub don't
// implement it) or the target slot is already empty.
func (s *Server) fitsForSlotLoad(mode, slot string) (fits bool, reason string, requiredBytes, freeBytes int64, err error) {
	fit, err := s.deps.Engine.CanFit(mode)
	if err != nil {
		return false, "", 0, 0, err
	}
	if fit.Fits {
		return true, "", fit.RequiredBytes, fit.FreeBytes, nil
	}
	fp, ok := s.deps.Engine.(interface{ SlotFootprintBytes(string) int64 })
	if !ok {
		return false, fit.Reason, fit.RequiredBytes, fit.FreeBytes, nil
	}
	snap := s.snapshot()
	if snap == nil {
		return false, fit.Reason, fit.RequiredBytes, fit.FreeBytes, nil
	}
	st, ok := snap.Slots[slot]
	if !ok || st.Mode == "" {
		return false, fit.Reason, fit.RequiredBytes, fit.FreeBytes, nil
	}
	freed := fp.SlotFootprintBytes(slot)
	if freed <= 0 {
		return false, fit.Reason, fit.RequiredBytes, fit.FreeBytes, nil
	}
	adjustedFree := fit.FreeBytes + freed
	if fit.RequiredBytes <= adjustedFree {
		return true, "", fit.RequiredBytes, adjustedFree, nil
	}
	return false, fit.Reason, fit.RequiredBytes, adjustedFree, nil
}

// runLoadBackground performs the engine load call.
func (s *Server) runLoadBackground(parent context.Context, mode, slot string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	result := s.deps.Engine.Load(ctx, mode, slot)
	out := lifecycleResult{
		Success: result.Success,
		Message: s.annotateCtxReduction(mode, result),
		NCtx:    result.NCtx,
	}
	s.mu.Lock()
	s.slotLoading[slot] = slotTransition{}
	s.mu.Unlock()
	if s.deps.Publish == nil {
		return
	}
	name := "load_complete"
	if !result.Success {
		name = "load_failed"
	}
	s.deps.Publish.Publish(name, map[string]any{
		"result": out,
		"status": s.buildStatusResponse(),
		"slot":   slot,
	})
}

// annotateCtxReduction completes the partial n_ctx-verification hook (BE-2
// Q4, docs/v5-review-fixes.md): the engine already probes /props and logs a
// WARN when llama.cpp silently reduced context below 95% of configured
// (internal/engine/lifecycle.go verifyModelContext) — a documented ForgeHost
// behavior (kernel fails contiguous GTT allocation) — but that WARN never
// reached the dashboard. A silent state or a hard block are both wrong per
// Q4's resolution; this appends a visible warning to the same
// load_complete/load_failed message the SSE bus already delivers, without
// flipping Success (the load did succeed — just not at the requested
// context).
func (s *Server) annotateCtxReduction(mode string, result engine.Result) string {
	if !result.Success || result.NCtx <= 0 || s.deps.Config == nil {
		return result.Message
	}
	cfg := s.deps.Config()
	m, ok := cfg.Modes[mode]
	if !ok || len(m.Services) == 0 {
		return result.Message
	}
	configured := m.Services[0].Context
	if configured <= 0 || float64(result.NCtx) >= float64(configured)*0.95 {
		return result.Message
	}
	return fmt.Sprintf("%s (WARNING: context reduced — requested %d, actual %d; see docs/pitfalls.md n_ctx silent reduction)",
		result.Message, configured, result.NCtx)
}

// ── §0.6 Single-instance load guard helpers ──────────────────────────────────

// findModeLoadedElsewhere checks whether mode (a Config name) is already
// loaded or loading on any slot other than targetSlot. Returns the slot name
// where it's loaded/loading, or "" when no conflict is found.
//
// ADR 0006: single-instance scope is per-Config, not per-Model — two
// different configs of the same model (e.g. qwen3-coder-256k and
// qwen3-coder-1m) are distinct loadable units and may coexist on two slots.
// This is a direct mode-name comparison, mirroring the engine's
// per-config-name guard (internal/engine/lifecycle.go) as an early-reject
// fast path; the engine guard remains the authoritative check.
//
// Two sources are checked (§0.6: "loaded or loading"):
//   - Loaded: snapshot.Slots[slot].Mode (the engine's authoritative
//     reconciliation, published by the collector each cycle).
//   - Loading: the in-process slotLoading map (the handler's overlay,
//     set in handleLoad before the goroutine starts, cleared in
//     runLoadBackground when it completes).
func (s *Server) findModeLoadedElsewhere(mode, targetSlot string) string {
	// Check loaded slots (from snapshot).
	if snap := s.snapshot(); snap != nil {
		for slotName, slot := range snap.Slots {
			if slotName == targetSlot {
				continue
			}
			if slot.Mode == mode {
				return slotName
			}
		}
	}

	// Check in-process loading state.
	s.mu.Lock()
	defer s.mu.Unlock()
	for slot, st := range s.slotLoading {
		if slot == targetSlot {
			continue
		}
		if st.inProgress && st.mode == mode {
			return slot
		}
	}

	return ""
}

// handleUnload starts a slot unload (Contract 1 §2 #12). "all" is a special
// case that mirrors a switch to "unloaded".
func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	var b slotUnloadBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if _, fields := b.validate(); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine not wired")
		return
	}

	if b.Slot == "all" {
		// Unload-all uses the switch_state machinery, target "unloaded".
		s.mu.Lock()
		if s.switchState.inProgress {
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"success": false,
				"message": "Switch already in progress",
			})
			return
		}
		s.switchState.inProgress = true
		s.switchState.target = "unloaded"
		s.switchState.startedAt = time.Now()
		s.switchState.lastResult = nil
		startedAt := s.switchState.startedAt
		s.mu.Unlock()
		s.audit(r, "human", "unload_all", "", "")
		if s.deps.Publish != nil {
			s.deps.Publish.Publish("switch_started", map[string]any{
				"target":     "unloaded",
				"started_at": isoFormat(startedAt),
			})
		}
		go s.runUnloadAllBackground(s.bgCtx)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":     true,
			"message":     "Unloading all slots",
			"in_progress": true,
		})
		return
	}

	// Single-slot unload.
	s.mu.Lock()
	if cur, ok := s.slotLoading[b.Slot]; ok && cur.inProgress {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": fmt.Sprintf("Slot %s is currently loading", b.Slot),
		})
		return
	}
	if cur, ok := s.slotUnloading[b.Slot]; ok && cur.inProgress {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": fmt.Sprintf("Slot %s is already unloading", b.Slot),
		})
		return
	}
	outgoingMode := ""
	if snap := s.snapshot(); snap != nil {
		if slotState, ok := snap.Slots[b.Slot]; ok {
			outgoingMode = slotState.Mode
		}
	}
	s.slotUnloading[b.Slot] = slotTransition{
		inProgress: true,
		mode:       outgoingMode,
		startedAt:  time.Now(),
	}
	s.mu.Unlock()

	s.audit(r, "human", "unload_slot", b.Slot, "")
	go s.runUnloadBackground(s.bgCtx, b.Slot)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"message":     fmt.Sprintf("Unloading %s slot", b.Slot),
		"in_progress": true,
	})
}

// runUnloadBackground performs a single-slot unload.
func (s *Server) runUnloadBackground(parent context.Context, slot string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	result := s.deps.Engine.Unload(ctx, slot)
	out := lifecycleResult{
		Success: result.Success,
		Message: result.Message,
		NCtx:    result.NCtx,
	}
	s.mu.Lock()
	s.slotUnloading[slot] = slotTransition{}
	s.mu.Unlock()
	if s.deps.Publish == nil {
		return
	}
	s.deps.Publish.Publish("unload_complete", map[string]any{
		"result": out,
		"status": s.buildStatusResponse(),
		"slot":   slot,
	})
}

// runUnloadAllBackground unloads every slot.
func (s *Server) runUnloadAllBackground(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	result := s.deps.Engine.Unload(ctx, "all")
	out := lifecycleResult{
		Success: result.Success,
		Message: result.Message,
		NCtx:    result.NCtx,
	}
	s.mu.Lock()
	s.switchState.inProgress = false
	s.switchState.target = ""
	s.switchState.startedAt = time.Time{}
	s.switchState.lastResult = &out
	s.mu.Unlock()
	if s.deps.Publish == nil {
		return
	}
	name := "switch_complete"
	if !result.Success {
		name = "switch_failed"
	}
	s.deps.Publish.Publish(name, map[string]any{
		"result": out,
		"status": s.buildStatusResponse(),
	})
}

// ── Router settings + aux-unit control ───────────────────────────────────────

// resolvedRouterSettings reads the router behavior-toggle settings-KV keys,
// defaulting each independently exactly as internal/router's own readers do
// (usage.go's injectStreamUsageEnabled, routing.go's localCompressorEnabled,
// routing.go's busyMode, routing.go's providerFailoverEnabled) — duplicated
// rather than shared, since the router package doesn't expose a combined
// read and this is a small, stable amount of logic. All are read fresh per
// request by the router itself, so this GET always reflects live state, not
// a cached snapshot.
func (s *Server) resolvedRouterSettings(ctx context.Context) routerSettingsResponse {
	resp := routerSettingsResponse{BusyMode: "wait", InjectStreamUsage: true, CompressorLocalEnabled: false, ProviderFailover: false}
	if s.deps.Settings == nil {
		return resp
	}
	if raw, err := s.deps.Settings.Get(ctx, "router.busy_mode"); err == nil {
		_ = json.Unmarshal(raw, &resp.BusyMode)
	}
	if raw, err := s.deps.Settings.Get(ctx, "usage.inject_stream_usage"); err == nil {
		_ = json.Unmarshal(raw, &resp.InjectStreamUsage)
	}
	if raw, err := s.deps.Settings.Get(ctx, "compressor.local_enabled"); err == nil {
		_ = json.Unmarshal(raw, &resp.CompressorLocalEnabled)
	}
	if raw, err := s.deps.Settings.Get(ctx, "router.provider_failover"); err == nil {
		_ = json.Unmarshal(raw, &resp.ProviderFailover)
	}
	return resp
}

// handleRouterSettingsGet returns the router busy-mode + the two Sprint 12
// additions (Contract 1 §2 #22, extended).
func (s *Server) handleRouterSettingsGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.resolvedRouterSettings(ctx))
}

// handleRouterSettingsPut persists router.busy_mode / usage.inject_stream_usage
// / compressor.local_enabled / router.provider_failover to store.Settings
// (Contract 1 §2 #22, C1-Q5, extended Sprint 12 Phase 2 + 2026-08-06). All
// are settings-KV, not config-file — the a0 router reads each fresh per
// request, so a save here is immediate, no restart or ReloadConfig needed.
func (s *Server) handleRouterSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not available")
		return
	}
	var body routerSettingsBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if body.BusyMode != nil && *body.BusyMode != "wait" && *body.BusyMode != "fail_fast" {
		writeValidationError(w, map[string]string{"busy_mode": "must be \"wait\" or \"fail_fast\""})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if body.BusyMode != nil {
		raw, _ := json.Marshal(*body.BusyMode)
		if err := s.deps.Settings.Set(ctx, "router.busy_mode", raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if body.InjectStreamUsage != nil {
		raw, _ := json.Marshal(*body.InjectStreamUsage)
		if err := s.deps.Settings.Set(ctx, "usage.inject_stream_usage", raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if body.CompressorLocalEnabled != nil {
		raw, _ := json.Marshal(*body.CompressorLocalEnabled)
		if err := s.deps.Settings.Set(ctx, "compressor.local_enabled", raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if body.ProviderFailover != nil {
		raw, _ := json.Marshal(*body.ProviderFailover)
		if err := s.deps.Settings.Set(ctx, "router.provider_failover", raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	resp := s.resolvedRouterSettings(ctx)
	s.audit(r, identity(r).Name, "router_settings", "busy_mode", resp.BusyMode)
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{"action": "router_settings_updated"})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleServiceMode starts/stops a service-mode's systemd unit (Contract 1
// §2 #23–24, C1-Q2). The unit name comes from the mode's config entry
// (type="service"); the engine's StartUnit/StopUnit refuses inference-slot
// units, so this can never disturb a bay.
func (s *Server) handleServiceMode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.deps.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "config not available")
		return
	}
	cfg := s.deps.Config()
	mode, ok := cfg.Modes[name]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown mode: %s", name))
		return
	}
	if mode.Type != "service" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("mode %q is not a service mode", name))
		return
	}
	if mode.Unit == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("service mode %q has no unit configured", name))
		return
	}
	s.runUnitOp(w, r, mode.Unit, "service_mode", name)
}

// handleTTS starts/stops the TTS systemd unit (Contract 1 §2 #25–26,
// C1-Q2). Unit name from config.Server.TTSUnit (default "forge-tts").
func (s *Server) handleTTS(w http.ResponseWriter, r *http.Request) {
	unit := "forge-tts"
	if s.deps.Config != nil {
		cfg := s.deps.Config()
		if cfg.Server.TTSUnit != "" {
			unit = cfg.Server.TTSUnit
		}
	}
	s.runUnitOp(w, r, unit, "tts", "tts")
}

// handleFixedInfraService starts/stops one of the always-on fixed infra
// services (STT/Embedding/Aligner) — Tier 1 Sprint 2, Voice & Speech
// settings: these had no start/stop route at all before this sprint (only
// TTS did). Unit names are the same literals handleInfraServices already
// uses to build these services' rows (services_handlers.go); presence in
// cfg.Ports gates whether this deployment has the service at all, matching
// handleInfraServices's own gate.
func (s *Server) handleFixedInfraService(name, unit string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Config == nil {
			writeError(w, http.StatusServiceUnavailable, "config not available")
			return
		}
		if _, ok := s.deps.Config().Ports[name]; !ok {
			writeError(w, http.StatusNotFound, name+" is not configured on this deployment")
			return
		}
		s.runUnitOp(w, r, unit, name, name)
	}
}

// runUnitOp dispatches a start/stop (inferred from the URL suffix) to the
// engine's aux-unit control and writes the uniform lifecycle response.
func (s *Server) runUnitOp(w http.ResponseWriter, r *http.Request, unit, action, target string) {
	if s.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine not available")
		return
	}
	start := strings.HasSuffix(r.URL.Path, "/start")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var err error
	verb := "stop"
	if start {
		verb, err = "start", s.deps.Engine.StartUnit(ctx, unit)
	} else {
		err = s.deps.Engine.StopUnit(ctx, unit)
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, action+"_"+verb, target, unit)
	// Sprint K bug fix (2026-08-05): this used to publish a bare
	// {action, unit} payload under the status_update name. sse.ts parses
	// every status_update as a full Status and writes it straight into the
	// query cache with no merge — that blanked slots/services/slot_labels
	// for any client until the next 15s poll, reachable from ServicesBar's
	// play/stop buttons. probeAndPush is the same "force a fresh snapshot,
	// then publish the real full status" helper profile_handlers.go already
	// uses for exactly this reason.
	s.probeAndPush()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": fmt.Sprintf("%s %sed", unit, verb)})
}
