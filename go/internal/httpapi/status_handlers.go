// SPDX-License-Identifier: Apache-2.0

package httpapi

// status_handlers.go — dashboard status + scheduler status (Contract 1 §2
// #3, #5). Split out of handlers.go by Sprint 0
// (docs/v5-sprint0-contract-freeze.md §0.1); pure move, no behavior change.
// Owner track after split: BE-4.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/activity"
)

// handleStatus returns the full dashboard status payload (Contract 1 §2
// #3). Mirrors V4's _build_status() (forge/app.py) — overlays in-process
// transition state on top of the collector snapshot.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.buildStatusResponse())
}

// buildStatusResponse assembles the Status payload for GET /api/v1/status
// and every SSE status_update push. Mirrors V4 _build_status: snapshot
// core + overlay in-process switch/slot-loading state + read-only config
// (modes_available, service_modes, services) + Settings-backed UI bits
// (ui).
func (s *Server) buildStatusResponse() statusResponse {
	snap := s.snapshot()
	cfg := s.deps.Config()

	resp := statusResponse{
		Hostname:      s.deps.Hostname,
		Version:       s.deps.Version,
		Ports:         map[string]bool{},
		Services:      map[string]string{},
		Slots:         map[string]*string{},
		SlotLabels:    map[string]string{},
		ModesAvail:    map[string]modeAvail{},
		ServiceModes:  map[string]svcModeInfo{},
		UI:            map[string]any{},
		SlotLoading:   map[string]slotStateJSON{},
		SlotUnloading: map[string]slotStateJSON{},
		SlotActivity:  map[string]bool{},
	}

	if snap != nil {
		// Engine may legitimately be nil in tests that exercise only the
		// auth surface; leave Mode empty rather than nil-deref in the
		// heartbeat goroutine.
		if s.deps.Engine != nil {
			resp.Mode = s.deps.Engine.CurrentMode()
		}
		// ports: snapshot's port-listening map keyed by string for JSON.
		for port, listening := range snap.Ports {
			resp.Ports[fmt.Sprintf("%d", port)] = listening
		}
		// slots + slot_labels.
		for name, slot := range snap.Slots {
			resp.SlotLabels[name] = slot.Label
			resp.Slots[name] = slotModeOrNull(slot.Mode)
			// services: every managed unit's active/inactive state.
			if slot.Unit != "" {
				unitKey := slot.Unit + ".service"
				if u, ok := snap.Units[slot.Unit]; ok {
					if u.Active() {
						resp.Services[unitKey] = "active"
					} else if u.Deactivating() {
						resp.Services[unitKey] = "deactivating"
					} else {
						resp.Services[unitKey] = "inactive"
					}
				} else {
					resp.Services[unitKey] = "inactive"
				}
			}
			// slot_activity (Sprint K): snap.Inference already carries
			// RequestsProcessing per slot (collector/run.go's scrapeInference)
			// but had zero readers outside the collector package before this.
			// Only loaded slots get an entry — an empty slot has no
			// inference row to read "not active" from.
			if inf, ok := snap.Inference[name]; ok {
				resp.SlotActivity[name] = inf.RequestsProcessing > 0
			}
		}
		// alerts.
		for _, a := range snap.Alerts {
			aj := alertJSON{Code: a.Code, Msg: a.Msg}
			if a.Port != 0 {
				aj.Port = ptrInt(a.Port)
			}
			resp.Alerts = append(resp.Alerts, aj)
		}
		// tts_active = forge-tts unit is active.
		if u, ok := snap.Units["forge-tts"]; ok {
			resp.TTSActive = u.Active()
		}
	}

	// description: from config (modes[mode].description).
	if cfg != nil && resp.Mode != "" {
		if m, ok := cfg.Modes[resp.Mode]; ok {
			resp.Description = m.Description
		}
	}

	// modes_available + service_modes: from config (read-only).
	if cfg != nil {
		for name, m := range cfg.Modes {
			if m.Type == "service" {
				continue
			}
			context := 0
			if len(m.Services) > 0 {
				context = m.Services[0].Context
			}
			backend := "vulkan"
			if len(m.Services) > 0 && m.Services[0].Backend != "" {
				backend = m.Services[0].Backend
			}
			resp.ModesAvail[name] = modeAvail{
				Label:       firstNonEmpty(m.Label, name),
				Description: m.Description,
				Family:      m.Family,
				Tags:        m.Tags,
				Color:       firstNonEmpty(m.Color, "#00d4e8"),
				Icon:        m.Icon,
				Context:     context,
				SlotCapable: len(m.Services) > 0,
				Backend:     backend,
			}
		}
		for name, m := range cfg.Modes {
			if m.Type != "service" {
				continue
			}
			var unit *string
			if m.Unit != "" {
				u := m.Unit + ".service"
				unit = &u
			}
			active := false
			if snap != nil && unit != nil {
				if u, ok := snap.Units[strings.TrimSuffix(*unit, ".service")]; ok {
					active = u.Active()
				}
			}
			resp.ServiceModes[name] = svcModeInfo{
				Label:  firstNonEmpty(m.Label, name),
				Icon:   m.Icon,
				Unit:   unit,
				Active: active,
			}
		}
	}

	// Switch + slot loading/unloading overlay from in-process state.
	s.mu.Lock()
	resp.Switch = switchStateJSON{
		InProgress: s.switchState.inProgress,
	}
	if s.switchState.target != "" {
		t := s.switchState.target
		resp.Switch.Target = &t
	}
	if !s.switchState.startedAt.IsZero() {
		ts := unixSeconds(s.switchState.startedAt)
		resp.Switch.StartedAt = &ts
	}
	if s.switchState.lastResult != nil {
		resp.Switch.LastResult = &lifecycleResultJSON{
			Success: s.switchState.lastResult.Success,
			Message: s.switchState.lastResult.Message,
			NCtx:    s.switchState.lastResult.NCtx,
		}
	}
	for slot, st := range s.slotLoading {
		resp.SlotLoading[slot] = slotStateJSON{
			InProgress: st.inProgress,
			Mode:       ptrString(st.mode),
		}
		if !st.startedAt.IsZero() {
			ts := unixSeconds(st.startedAt)
			resp.SlotLoading[slot] = slotStateJSON{
				InProgress: st.inProgress,
				Mode:       ptrString(st.mode),
				StartedAt:  &ts,
			}
		}
	}
	for slot, st := range s.slotUnloading {
		resp.SlotUnloading[slot] = slotStateJSON{
			InProgress: st.inProgress,
			Mode:       ptrString(st.mode),
		}
		if !st.startedAt.IsZero() {
			ts := unixSeconds(st.startedAt)
			resp.SlotUnloading[slot] = slotStateJSON{
				InProgress: st.inProgress,
				Mode:       ptrString(st.mode),
				StartedAt:  &ts,
			}
		}
		// A slot currently unloading is authoritatively still occupied by
		// the outgoing model until the unload thread clears the marker —
		// trust that over the (possibly stale) snapshot's "slots" map
		// (V4 _build_status overlay).
		if st.inProgress && st.mode != "" {
			resp.Slots[slot] = ptrString(st.mode)
		}
	}
	s.mu.Unlock()

	// ui: from store.Settings (JSON KV).
	if s.deps.Settings != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if raw, err := s.deps.Settings.Get(ctx, "ui"); err == nil {
			_ = json.Unmarshal(raw, &resp.UI)
		}
		resp.RestartRequired = s.restartRequired(ctx)
	}

	// profiling (Sprint K): additive, so a client reconnecting mid-run (SSE
	// drop, or a fresh page load) sees the same "a run is holding these
	// slots" state that live profile:progress events convey, rather than
	// nothing until the next event.
	if runner, ok := s.profileRunner(); ok {
		if mode, running := runner.RunningMode(); running {
			resp.Profiling = &profilingStatus{Running: true, Mode: mode}
		}
	}

	// slot_consumers (per-slot consumer attribution): the shared registry's
	// fresh labels for every slot we know about. Only non-empty labels are
	// included; stale entries (past activity.ConsumerFreshness) read "" and
	// are dropped.
	if s.deps.Activity != nil {
		names := map[string]struct{}{}
		for name := range resp.Slots {
			names[name] = struct{}{}
		}
		if cfg != nil {
			for name := range cfg.Slots {
				names[name] = struct{}{}
			}
		}
		for name := range names {
			if label := s.deps.Activity.Label(name, activity.ConsumerFreshness); label != "" {
				if resp.SlotConsumers == nil {
					resp.SlotConsumers = map[string]string{}
				}
				resp.SlotConsumers[name] = label
			}
		}
	}

	return resp
}

// handleSchedulerStatus returns the fleet/queue/memory-budget snapshot
// (Contract 1 §2 #5).
func (s *Server) handleSchedulerStatus(w http.ResponseWriter, _ *http.Request) {
	resp := schedulerStatusResponse{
		Slots:           map[string]*string{},
		SlotLabels:      map[string]string{},
		IdleSeconds:     map[string]*float64{},
		SlotMemoryBytes: map[string]int64{},
		UnitMemoryBytes: map[string]int64{},
		Queue:           []queueTicketJSON{},
	}
	if s.deps.Sched == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	st := s.deps.Sched.Status()
	for slot, mode := range st.Slots {
		resp.Slots[slot] = slotModeOrNull(mode)
	}
	for slot, label := range st.SlotLabels {
		resp.SlotLabels[slot] = label
	}
	for slot, idle := range st.IdleSeconds {
		if idle == nil {
			resp.IdleSeconds[slot] = nil
			continue
		}
		v := *idle
		resp.IdleSeconds[slot] = &v
	}
	for slot, mb := range st.SlotMemoryBytes {
		resp.SlotMemoryBytes[slot] = mb
	}
	for unit, mb := range st.UnitMemoryBytes {
		resp.UnitMemoryBytes[unit] = mb
	}
	resp.MemoryBudget = schedBudgetJSON{
		TotalBytes: st.MemoryBudget.TotalBytes,
		UsedBytes:  st.MemoryBudget.UsedBytes,
		FreeBytes:  st.MemoryBudget.FreeBytes,
	}
	for _, t := range st.Queue {
		resp.Queue = append(resp.Queue, queueTicketJSON{
			TicketID:    t.TicketID,
			Model:       t.Model,
			RequestedBy: t.RequestedBy,
			TargetSlot:  ptrString(t.TargetSlot),
			Status:      t.Status,
			SmallJob:    t.SmallJob,
			EnqueuedAt:  unixSeconds(t.EnqueuedAt),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
