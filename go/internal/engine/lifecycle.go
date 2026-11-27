// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/enginekind"
)

// deactivatingWaitS bounds waits for a unit to leave "deactivating":
// TimeoutStopSec=300 plus margin (virtual seconds).
const deactivatingWaitS = 330

// SwitchMode implements Engine (port of engine.switch_mode): stop managed
// units → write the slot launch row → start target units → verify /health and
// /props (n_ctx). Blocking.
func (m *Manager) SwitchMode(ctx context.Context, modeName string) Result {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	cfg := m.d.Cfg()
	mode, ok := cfg.Modes[modeName]
	if !ok {
		return Result{Success: false, Message: fmt.Sprintf("Unknown mode: %s. Valid: %v", modeName, modeNames(cfg))}
	}
	if m.pooled(ctx, modeName) {
		return Result{Success: false, Message: pooledMessage(modeName)}
	}

	// Refuse an engine kind this binary does not know before anything else, including the "already in this mode" shortcut, so the
	// answer never depends on what the units happen to look like (ADR-0025: fail closed).
	for _, svc := range mode.Services {
		if _, err := svc.Engine(); err != nil {
			return Result{Success: false, Message: fmt.Sprintf("refusing to load %s: %v", modeName, err)}
		}
	}

	current := m.CurrentMode()
	if current == modeName {
		if m.modeFullyActive(ctx, cfg, mode) {
			return Result{Success: true, Message: fmt.Sprintf("Already in %s mode", modeName)}
		}
		m.logf("state says %s but services mismatch — re-activating", modeName)
	}

	m.logf("switching %s -> %s: %s", current, modeName, mode.Description)

	if !m.stopAll(ctx, cfg) {
		return Result{Success: false, Message: "Services did not stop cleanly"}
	}
	m.clearAllSlots()
	disarmNote := m.takeDisarmFailures()

	start := m.d.Now()
	var verifiedCtx int

	for _, svc := range mode.Services {
		if _, ok := cfg.Slots[svc.PortRole]; ok {
			warn, err := m.writeSlotLaunch(ctx, svc.PortRole, modeName, mode.ConfigID, svc)
			if err != nil {
				return Result{Success: false, Message: err.Error()}
			}
			disarmNote = joinNotes(disarmNote, warn)
		}
	}

	if len(mode.Services) == 0 {
		m.logf("no services to start (unloaded mode)")
	} else {
		units := make([]string, 0, len(mode.Services))
		for _, svc := range mode.Services {
			if u := serviceUnit(cfg, svc); u != "" {
				units = append(units, u)
			}
		}
		m.logf("starting services: %v", units)
		for _, u := range units {
			if err := m.d.Sys.Start(ctx, u); err != nil {
				m.logf("start %s failed: %v", u, err)
				for _, svc := range mode.Services {
					m.recordHistory(modeName, svc, nil, m.d.Now().Sub(start), true)
				}
				return Result{Success: false, Message: fmt.Sprintf("Failed to start services for %s", modeName)}
			}
		}

		for _, svc := range mode.Services {
			unit := serviceUnit(cfg, svc)
			if unit == "" {
				continue
			}
			port := 0
			if slot, ok := cfg.Slots[svc.PortRole]; ok {
				port = slot.Port
			}
			if ok, why := m.waitServiceRunning(ctx, unit, port, svc); !ok {
				detail := m.startupDetail(ctx, unit, start)
				_ = m.d.Sys.Stop(ctx, unit)
				m.recordHistory(modeName, svc, nil, m.d.Now().Sub(start), true)
				return Result{Success: false, Message: fmt.Sprintf("%s did not reach running state: %s", unit, why), Detail: detail}
			}
			actual := m.verifiedContext(ctx, port, svc)
			m.recordHistory(modeName, svc, actualPtr(actual), m.d.Now().Sub(start), false)
			if actual > 0 && m.probesChatTemplateCaps(svc) {
				m.probeChatTemplateCaps(ctx, port, mode.ConfigID, svc)
			}
			m.setSlotMode(svc.PortRole, modeName)
			if verifiedCtx == 0 {
				verifiedCtx = actual
			}
		}
	}

	m.saveMode(modeName)
	m.notifySwitchComplete()
	m.logf("mode %s active", modeName)
	return Result{Success: true, Message: fmt.Sprintf("Switched to %s", modeName), NCtx: verifiedCtx, Detail: disarmNote}
}

// Restart implements Engine (port of engine.restart_current).
func (m *Manager) Restart(ctx context.Context) Result {
	current := m.CurrentMode()
	if current == "unknown" {
		return Result{Success: false, Message: "Cannot restart: current mode unknown"}
	}
	if current == "unloaded" {
		return Result{Success: true, Message: "Unloaded mode — nothing to restart"}
	}
	m.logf("restarting current mode: %s", current)
	m.opMu.Lock()
	if !m.stopAll(ctx, m.d.Cfg()) {
		m.opMu.Unlock()
		return Result{Success: false, Message: "Services did not stop cleanly during restart"}
	}
	m.clearAllSlots()
	m.opMu.Unlock()
	return m.SwitchMode(ctx, current)
}

// Load implements Engine (port of engine.load_to_slot): loads a mode's first
// service into one slot, overriding its port_role; only that slot's unit is
// touched.
func (m *Manager) Load(ctx context.Context, modeName, slot string) Result {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	cfg := m.d.Cfg()
	slotCfg, ok := cfg.Slots[slot]
	if !ok {
		return Result{Success: false, Message: fmt.Sprintf("slot must be one of %v", m.Slots())}
	}
	mode, ok := cfg.Modes[modeName]
	if !ok {
		return Result{Success: false, Message: fmt.Sprintf("Unknown mode: %s", modeName)}
	}
	if len(mode.Services) == 0 {
		return Result{Success: false, Message: fmt.Sprintf("Mode %s has no services", modeName)}
	}
	if m.pooled(ctx, modeName) {
		return Result{Success: false, Message: pooledMessage(modeName)}
	}

	// §0.6 single-instance guard: a mode already loaded on another slot
	// cannot be loaded again — two instances break a0 routing. This
	// mode-name check is the authoritative guard (ADR 0006: single-instance
	// scope is per-Config, i.e. per mode name, not per-Model — two different
	// configs of the same model may coexist on two slots). The handler-level
	// guard (httpapi/scheduler_handlers.go findModeLoadedElsewhere) mirrors
	// this same mode-name comparison as an early-reject fast path ahead of
	// the goroutine dispatch; this engine-level check is what's actually
	// enforced for every caller, including the scheduler path that bypasses
	// the handler. A slot mid-unload keeps its mode per the crown-jewels
	// rule and is correctly treated as still occupied here.
	m.mu.Lock()
	for name, rec := range m.slots {
		if name != slot && rec.mode == modeName && rec.mode != "" {
			m.mu.Unlock()
			return Result{Success: false, Message: fmt.Sprintf("mode %s is already loaded on slot %s", modeName, name)}
		}
	}
	m.mu.Unlock()

	// Same-weights sibling guard (2026-08-22 incident): a different config
	// resolving to the SAME GGUF weights may coexist with the resident one
	// only when the fit check proves real headroom for a second instance —
	// operator decision 2026-08-22 (think/nothink-style siblings stay
	// permitted because coalescing traffic onto the resident instance would
	// silently change generation flags, but never without room). Refuse only
	// on the terminal shape — nothing fits even after every eviction — so
	// the scheduler's own eviction flow (which re-runs FitPlan after each
	// unload) still works. This choke point sees every caller: scheduler,
	// dashboard load handler, MCP, smith.
	if newID := m.WeightIdentity(modeName); newID != "" {
		sibling := ""
		m.mu.Lock()
		for name, rec := range m.slots {
			if name != slot && rec.mode != "" && rec.mode != modeName && m.WeightIdentity(rec.mode) == newID {
				sibling = fmt.Sprintf("%s on %s", rec.mode, name)
				break
			}
		}
		m.mu.Unlock()
		if sibling != "" {
			if plan, err := m.FitPlan(modeName); err == nil && !plan.Fits && len(plan.Evict) == 0 && !plan.EvictComfyUI {
				return Result{Success: false, Message: fmt.Sprintf(
					"refusing to load %s: identical weights already resident via %s and there is no room for a second instance: %s",
					modeName, sibling, plan.Message)}
			}
			m.logf("same-weights sibling load permitted: %s alongside %s", modeName, sibling)
		}
	}

	svc := mode.Services[0]
	svc.PortRole = slot
	unit, port := slotCfg.Unit, slotCfg.Port
	if _, err := svc.Engine(); err != nil {
		return Result{Success: false, Message: fmt.Sprintf("refusing to load %s: %v", modeName, err)}
	}

	m.logf("loading %s into %s slot (unit=%s, port=%d)", modeName, slot, unit, port)
	start := m.d.Now()
	m.setTransition(slot, &collector.Transition{Mode: modeName, StartedAt: start}, nil)
	defer m.setTransition(slot, nil, nil)

	// Crown jewels: never place a load into a slot whose old process
	// hasn't exited. An active unit is stopped and awaited; a
	// deactivating unit (its own stop already in flight, possibly
	// minutes long under TimeoutStopSec=300) is awaited too.
	st, err := m.d.Sys.State(ctx, unit)
	if err == nil && st.Active() {
		_ = m.d.Sys.Stop(ctx, unit)
		if !m.waitUnitGone(ctx, unit, 15) && !m.waitUnitGone(ctx, unit, deactivatingWaitS-15) {
			return Result{Success: false, Message: fmt.Sprintf("%s did not stop for reload", unit)}
		}
	} else if err == nil && st.Deactivating() {
		m.logf("%s still deactivating — waiting before reload", unit)
		if !m.waitUnitGone(ctx, unit, deactivatingWaitS) {
			return Result{Success: false, Message: fmt.Sprintf("%s is still deactivating; not placing a load into an occupied slot", unit)}
		}
	}

	launchWarn, err := m.writeSlotLaunch(ctx, slot, modeName, mode.ConfigID, svc)
	if err != nil {
		return Result{Success: false, Message: err.Error()}
	}

	if err := m.d.Sys.Start(ctx, unit); err != nil {
		m.recordHistory(modeName, svc, nil, m.d.Now().Sub(start), true)
		return Result{Success: false, Message: fmt.Sprintf("Failed to start %s: %v", unit, err)}
	}
	if ok, why := m.waitServiceRunning(ctx, unit, port, svc); !ok {
		detail := m.startupDetail(ctx, unit, start)
		_ = m.d.Sys.Stop(ctx, unit)
		m.recordHistory(modeName, svc, nil, m.d.Now().Sub(start), true)
		return Result{Success: false, Message: fmt.Sprintf("%s did not reach running state: %s", unit, why), Detail: detail}
	}

	actual := m.verifiedContext(ctx, port, svc)
	m.recordHistory(modeName, svc, actualPtr(actual), m.d.Now().Sub(start), false)
	if actual > 0 && m.probesChatTemplateCaps(svc) {
		m.probeChatTemplateCaps(ctx, port, mode.ConfigID, svc)
	}
	m.setSlotMode(slot, modeName)
	m.notifySwitchComplete()
	m.logf("slot %s active with %s", slot, modeName)
	return Result{Success: true, Message: fmt.Sprintf("Loaded %s into %s", modeName, slot), NCtx: actual, Detail: launchWarn}
}

// Unload implements Engine (port of engine.unload_slot / unload_all). It
// waits for the unit to fully leave "deactivating" before reporting the
// slot free — under TimeoutStopSec=300 this can genuinely take minutes.
func (m *Manager) Unload(ctx context.Context, slot string) Result {
	if slot == "all" {
		return m.unloadAll(ctx)
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	cfg := m.d.Cfg()
	slotCfg, ok := cfg.Slots[slot]
	if !ok {
		return Result{Success: false, Message: fmt.Sprintf("slot must be one of %v", m.Slots())}
	}
	unit := slotCfg.Unit
	m.logf("unloading %s slot (%s)", slot, unit)
	m.disarmFailures = nil

	m.mu.Lock()
	prevMode := ""
	if rec, ok := m.slots[slot]; ok {
		prevMode = rec.mode
	}
	m.mu.Unlock()
	m.setTransition(slot, nil, &collector.Transition{Mode: prevMode, StartedAt: m.d.Now()})
	defer m.setTransition(slot, nil, nil)

	snap := m.snapshotGTT(cfg, slot) // before the stop: the footprint is read from the running process
	// Unloading an empty slot (the profiler clears every other slot) frees nothing and must not mark it as recently freed.
	st, stateErr := m.d.Sys.State(ctx, unit)
	occupied := prevMode != "" || (stateErr == nil && (st.Active() || st.Deactivating()))
	_ = m.d.Sys.Stop(ctx, unit)
	if !m.waitUnitGone(ctx, unit, deactivatingWaitS) {
		// Still deactivating: the slot is NOT free. Keep its mode.
		return Result{Success: false, Message: fmt.Sprintf("%s is still deactivating; slot not freed yet", unit)}
	}

	m.killLingering(slotCfg.Port)
	m.waitGTTDrain(snap)

	m.disarmAfterStop(ctx, slot)
	m.setSlotMode(slot, "")
	if occupied {
		m.noteFreed(slot)
	}
	m.recordUsageEvent("unload", "", slot, "")
	return Result{Success: true, Message: fmt.Sprintf("Unloaded %s slot", slot), Detail: m.takeDisarmFailures()}
}

func (m *Manager) unloadAll(ctx context.Context) Result {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	cfg := m.d.Cfg()
	m.logf("unloading all inference slots")
	occupied := m.occupiedSlots()
	m.stopAll(ctx, cfg)
	m.clearAllSlots()
	for _, slot := range occupied {
		m.noteFreed(slot)
	}
	m.logf("all slots unloaded")
	return Result{Success: true, Message: "All slots unloaded", Detail: m.takeDisarmFailures()}
}

// modeFullyActive checks an already-current mode's units: every expected
// unit active and no other canonical slot unit active (port of the
// switch_mode short-circuit).
func (m *Manager) modeFullyActive(ctx context.Context, cfg *config.Config, mode config.Mode) bool {
	expected := modeUnits(cfg, mode)
	for u := range expected {
		if !m.unitActive(ctx, u) {
			return false
		}
	}
	for u := range canonicalUnits(cfg) {
		if !expected[u] && m.unitActive(ctx, u) {
			return false
		}
	}
	return true
}

// stopAll stops every canonical slot unit and verifies each is dead, then
// kills orphaned inference processes and waits for GTT drain (port of
// engine._stop_all; scoping note: V5 inference services always run in slot
// units, so the canonical set is the whole stop set).
func (m *Manager) stopAll(ctx context.Context, cfg *config.Config) bool {
	m.logf("stopping all inference services")
	m.disarmFailures = nil
	units := make([]string, 0, len(canonicalUnits(cfg)))
	for u := range canonicalUnits(cfg) {
		units = append(units, u)
	}
	sort.Strings(units)

	slotNames := make([]string, 0, len(cfg.Slots))
	for name := range cfg.Slots {
		slotNames = append(slotNames, name)
	}
	snap := m.snapshotGTT(cfg, slotNames...) // before any stop: footprints come from the running processes

	for _, u := range units {
		if err := m.d.Sys.Stop(ctx, u); err != nil {
			m.logf("stop %s returned error: %v", u, err)
		}
	}

	ok := true
	gone := map[string]bool{}
	for _, u := range units {
		if !m.waitUnitGone(ctx, u, 5) {
			st, _ := m.d.Sys.State(ctx, u)
			m.logf("WARNING: %s still %s/%s after stop", u, st.ActiveState, st.SubState)
			ok = false
		} else {
			gone[u] = true
		}
	}

	var allPorts []int
	for _, s := range cfg.Slots {
		allPorts = append(allPorts, s.Port)
	}
	m.killLingering(allPorts...)
	m.waitGTTDrain(snap)

	// Rows are disarmed only for units confirmed gone (a unit still deactivating keeps its armed row).
	for _, name := range slotNames {
		if gone[cfg.Slots[name].Unit] {
			m.disarmAfterStop(ctx, name)
		}
	}
	m.logf("all services stopped")
	return ok
}

// waitUnitGone polls until the unit's SubState is dead/failed/inactive,
// up to timeout virtual seconds (2s poll cadence, V4 parity).
func (m *Manager) waitUnitGone(ctx context.Context, unit string, timeoutS int) bool {
	for elapsed := 0; ; elapsed += 2 {
		st, err := m.d.Sys.State(ctx, unit)
		if err == nil {
			switch st.SubState {
			case "dead", "failed", "inactive":
				return true
			}
			if st.ActiveState == "inactive" || st.ActiveState == "failed" {
				return true
			}
		}
		if elapsed >= timeoutS {
			return false
		}
		m.pause(2)
	}
}

// waitServiceRunning blocks until SubState=running, then polls /health,
// bailing out if the unit crashes or cycles restarts (port of
// engine._wait_service_running).
func (m *Manager) waitServiceRunning(ctx context.Context, unit string, port int, svc config.Service) (bool, string) {
	timeoutS := svc.StartupTimeoutS
	if timeoutS == 0 {
		timeoutS = 120 // config default; belt-and-braces for raw structs
	}

	elapsed := 0
	for {
		st, err := m.d.Sys.State(ctx, unit)
		if err == nil && st.SubState == "running" {
			m.logf("%s process up — waiting for /health", unit)
			break
		}
		if err == nil {
			switch st.SubState {
			case "failed", "inactive", "dead":
				m.logf("%s failed to start (SubState=%s)", unit, st.SubState)
				return false, fmt.Sprintf("failed to start (SubState=%s)", st.SubState)
			}
		}
		if elapsed >= timeoutS {
			m.logf("TIMEOUT: %s not running after %ds", unit, timeoutS)
			return false, fmt.Sprintf("not running after %ds", timeoutS)
		}
		m.pause(2)
		elapsed += 2
	}

	if port == 0 {
		m.logf("%s running (no health endpoint)", unit)
		return true, ""
	}

	deadline := elapsed + timeoutS
	for elapsed < deadline {
		st, err := m.d.Sys.State(ctx, unit)
		if err == nil {
			switch st.SubState {
			case "failed", "inactive", "dead", "auto-restart":
				m.logf("%s crashed during startup (SubState=%s)", unit, st.SubState)
				return false, fmt.Sprintf("crashed during startup (SubState=%s)", st.SubState)
			}
		}
		if m.llama.Healthy(ctx, port) {
			m.logf("%s ready (port %d healthy)", unit, port)
			return true, ""
		}
		m.pause(3)
		elapsed += 3
	}
	m.logf("TIMEOUT: %s /health on port %d never returned ok", unit, port)
	return false, fmt.Sprintf("/health on port %d never returned ok within %ds", port, timeoutS)
}

// verifiedContext runs the n_ctx verification for a freshly started service
// (crown jewels: the kernel may silently lower context on failed contiguous
// GTT allocation — always check /props and record the actual value).
// Returns the actual n_ctx (0 when unavailable). Engine kinds without /props
// (vLLM) report their configured context (V4 parity).
func (m *Manager) verifiedContext(ctx context.Context, port int, svc config.Service) int {
	spec, err := svc.Engine()
	if err != nil {
		m.logf("verifiedContext: %v", err) // unreachable after the launch checks; never guess a context
		return 0
	}
	switch {
	case spec.ContextFromProps:
		if port == 0 {
			return 0
		}
		_, actual := m.verifyModelContext(ctx, port, svc.Context)
		return actual
	case spec.ContextFromModels:
		if port == 0 {
			return 0
		}
		return m.verifyReportedContext(ctx, port, svc.Context, spec)
	}
	return svc.Context
}

// verifyReportedContext is the n_ctx verification for an engine that reports the context it was CONFIGURED with (/v1/models max_model_len) and does
// not lower it, but may hold fewer tokens than that in its pools (Radiance logs a warning only). Configured comes from /v1/models; capacity from the
// kind's ContextCapacitySeries on /metrics; the actual context is the smaller of the two, 0 when /v1/models cannot be read (never a guess). When
// the capacity series is absent or unreadable the configured value stands and a WARN says the capacity was not checked. Under 95% of the configured
// context is a WARN, and recordHistory then stores the load as ctx_reduced (same tolerance as the /props path).
func (m *Manager) verifyReportedContext(ctx context.Context, port, expected int, spec enginekind.Spec) int {
	info, err := m.llama.ModelsInfo(ctx, port)
	if err != nil {
		m.logf("WARN: /v1/models check failed on port %d: %v", port, err)
		return 0
	}
	actual := info.MaxModelLen
	if actual <= 0 {
		m.logf("WARN: /v1/models on port %d reports no max_model_len", port)
		return 0
	}
	if spec.ContextCapacitySeries != "" {
		capTokens, ok, gerr := m.llama.Gauge(ctx, port, spec.ContextCapacitySeries)
		switch {
		case gerr != nil:
			m.logf("WARN: %s could not be read on port %d (capacity not verified): %v", spec.ContextCapacitySeries, port, gerr)
		case !ok:
			m.logf("WARN: %s is absent on port %d (capacity not verified)", spec.ContextCapacitySeries, port)
		case int(capTokens) < actual:
			actual = int(capTokens)
		}
	}
	if expected > 0 && float64(actual) < float64(expected)*0.95 {
		m.logf("WARN: context reduced — expected %d, got %d on port %d", expected, actual, port)
		return actual
	}
	m.logf("context verified: n_ctx=%d on port %d", actual, port)
	return actual
}

// probesChatTemplateCaps reports whether the service's engine kind exposes chat_template_caps on /props.
func (m *Manager) probesChatTemplateCaps(svc config.Service) bool {
	spec, err := svc.Engine()
	if err != nil {
		m.logf("probesChatTemplateCaps: %v", err)
		return false
	}
	return spec.ChatTemplateCapsFromProps || spec.ChatTemplateCapsFromModels
}

// probeChatTemplateCaps captures /props' chat_template_caps for a freshly
// loaded config (T1, per-request thinking control) and persists it via the
// store — best-effort, mirroring recordHistory: a probe or store failure
// here must never fail the load itself, only logged. configID 0 means the
// mode has no catalog config (file-config mode, or a service mode) and is
// silently skipped, same as WeightEstimateBytes/ProfileBytes elsewhere.
func (m *Manager) probeChatTemplateCaps(ctx context.Context, port int, configID int64, svc config.Service) {
	if configID == 0 || port == 0 || m.d.UpdateChatTemplateCaps == nil {
		return
	}
	var caps map[string]bool
	var err error
	if spec, serr := svc.Engine(); serr == nil && !spec.ChatTemplateCapsFromProps && spec.ChatTemplateCapsFromModels {
		caps, err = m.modelsCaps(ctx, port)
	} else {
		caps, err = m.llama.ChatTemplateCaps(ctx, port)
		// A kind that may not serve /props falls back to /v1/models capabilities.
		if err != nil && serr == nil && spec.ChatTemplateCapsFromModels {
			caps, err = m.modelsCaps(ctx, port)
		}
	}
	if err != nil {
		m.logf("WARN: chat_template_caps probe failed on port %d: %v", port, err)
		return
	}
	if err := m.d.UpdateChatTemplateCaps(ctx, configID, caps); err != nil {
		m.logf("WARN: chat_template_caps persist failed for config %d: %v", configID, err)
	}
}

// verifyModelContext queries /props and warns when llama.cpp silently
// reduced n_ctx (port of engine._verify_model_context; ok when actual ≥ 95%
// of expected).
func (m *Manager) verifyModelContext(ctx context.Context, port, expected int) (bool, int) {
	actual, err := m.llama.NCtx(ctx, port)
	if err != nil {
		m.logf("WARN: /props check failed on port %d: %v", port, err)
		return false, 0
	}
	if expected > 0 && float64(actual) < float64(expected)*0.95 {
		m.logf("WARN: context reduced — expected %d, got %d on port %d", expected, actual, port)
		return false, actual
	}
	m.logf("context verified: n_ctx=%d on port %d", actual, port)
	return true, actual
}

// killLingering SIGKILLs inference processes that survived unit stop,
// scoped to the given ports (port of engine._find_lingering_gpu_pids +
// kill, corrected twice).
//
// V4's original matched by process name only (pgrep -x llama-server/vllm,
// no further filter) and was ported faithfully at first — but that matches
// every llama-server process on the host, not just the slot(s) whose unit
// was just stopped. The permanent aux services (embedding, TTS, forced-
// aligner) also run as "llama-server", so every mode switch or slot unload
// was killing them as collateral damage (self-healing via systemd's
// Restart=always, but a real availability blip each time) — caught
// live-verifying a real unload against ForgeHost in the Phase 9b parallel-run,
// where it bounced forge-embedding.service. Scoping by --port (the same
// port-to-slot attribution unifiedRSSMB uses) excludes them: their ports
// are never in cfg.Slots.
//
// Second bug, found 2026-07-29: scoping to *all* canonical slot ports
// (instead of just the port of the unit that was actually just stopped)
// fixed the aux-service collateral damage but introduced cross-slot
// collateral damage — a single-slot Unload() would SIGKILL every *other*
// currently-loaded slot's process too, since their ports are also in the
// canonical set. Live-reproduced on ForgeHost: loading two unrelated modes on
// two slots, then unloading just one, silently killed and auto-restarted
// (via systemd Restart=) the other — same "self-healing, but a real
// availability blip" pattern as the original aux-service bug, just one
// level narrower. Root-caused while investigating a `laguna-s-21` PROFILE
// run that appeared to hang for 2h27min: a routine, unrelated single-slot
// unload during that window collaterally killed it mid-request instead of
// it being a genuine hang (see docs/investigations.md). Now callers pass
// the exact port(s) that should be considered lingering: Unload(slot)
// passes just that slot's port; stopAll passes every canonical port.
func (m *Manager) killLingering(ports ...int) {
	slotPorts := map[int]bool{}
	for _, p := range ports {
		slotPorts[p] = true
	}
	if len(slotPorts) == 0 {
		return
	}

	var pids []int
	for _, name := range enginekind.ProcessNames() {
		for _, pid := range m.d.Proc.ByComm(name) {
			if port, ok := m.d.Proc.PortArg(pid); ok && slotPorts[port] {
				pids = append(pids, pid)
			}
		}
	}
	if len(pids) == 0 {
		return
	}
	m.logf("WARNING: orphaned inference PIDs after unit stop: %v — killing", pids)
	for _, pid := range pids {
		if err := m.d.Kill(pid); err != nil {
			m.logf("kill %d: %v", pid, err)
		}
	}
	m.pause(1)
}

// gttSnapshot is what an unload needs to remember from before it stops anything, so waitGTTDrain can tell "the memory this
// slot held came back" from "nothing happened".
type gttSnapshot struct {
	used     int64 // GTT in use before the stop (0 when unreadable)
	expected int64 // bytes the stopped slot(s) should hand back (their live footprint; 0 when unknown)
}

// snapshotGTT records the GTT counter and the footprint of the given slots. Call it BEFORE stopping them: the footprint is
// read from the running process.
func (m *Manager) snapshotGTT(cfg *config.Config, slots ...string) gttSnapshot {
	var snap gttSnapshot
	if used, ok := m.d.GPU.GTTUsedBytes(); ok {
		snap.used = int64(used)
	}
	for _, slot := range slots {
		snap.expected += m.slotGTTBytes(cfg, slot)
	}
	return snap
}

// slotGTTBytes is what GTT should give back when the slot stops: its process's GPU memory, or the on-disk weight set when that cannot be
// read. The slot's resident memory is not part of it, because the GTT counter never held it.
func (m *Manager) slotGTTBytes(cfg *config.Config, slot string) int64 {
	if gpu := m.slotHolding(cfg, slot).gpu; gpu > 0 {
		return int64(gpu)
	}
	model, mmproj := m.SlotWeightPaths(slot)
	return collector.WeightSetSizeBytes(model, mmproj, cfg.Paths.ModelsDir)
}

// drainedBy reports whether GTT has fallen by most of what the stopped slot(s) held. It is the exit that works while other
// slots or the always-on services keep memory resident (8-25 GB on ForgeHost), when the absolute floor below can never be met.
func (g gttSnapshot) drainedBy(now int64) bool {
	if g.used <= 0 || g.expected <= 0 {
		return false
	}
	return g.used-now >= g.expected*gttDrainShareNum/gttDrainShareDen
}

// A slot's footprint is measured per process (fdinfo) and GTT is a separate counter, so they agree only roughly: count the
// memory as back once 80% of the footprint is gone from GTT.
const (
	gttDrainShareNum = 4
	gttDrainShareDen = 5
)

// waitGTTDrain polls until the GTT counter shows the stopped slot's memory is back (port of engine._wait_gtt_drain:
// amdgpu TTM can hold GTT pages briefly after process exit on Strix Halo; loading immediately would OOM on memory that is
// already logically free). It returns true when it saw the drain and false when it timed out.
//
// Three ways to finish, checked in this order:
//  1. RELATIVE (snap): GTT has fallen by at least 80% of the stopped slot's footprint. Checked first and before any sleep,
//     because on ForgeHost the drain is done within about a second of the unit stopping (measured 2026-11-02).
//  2. ABSOLUTE: GTT is under 1 GiB, or under 15% of its value when the wait began (an otherwise idle GPU).
//  3. Timeout after 20 s, which logs the warning and calls OnGTTDrainTimeout. That is the "memory did not come back" case
//     the Smith GTT_DRAIN_TIMEOUT anomaly is for.
//
// Before the relative check existed, 2 and 3 were the only exits, and 2 cannot be met while any other slot or service holds
// memory: every unload ran the full 20 s and raised the anomaly (122 timeouts and no drains in two days). With no snapshot
// (footprint unknown, counter unreadable) behaviour is exactly the old one.
//
// A1 (bytes retrofit): thresholds were 1024 MB; now 1 GiB in bytes.
func (m *Manager) waitGTTDrain(snap gttSnapshot) bool {
	const baselineBytes = 1 << 30 // 1 GiB, the "near zero" floor (was 1024 MB)
	before, ok := m.d.GPU.GTTUsedBytes()
	if !ok {
		return true
	}
	if snap.drainedBy(int64(before)) {
		m.logf("GTT drained: %d bytes → %d bytes (%d of an expected %d bytes back)", snap.used, before, snap.used-int64(before), snap.expected)
		return true
	}
	if before < baselineBytes {
		return true
	}
	m.logf("waiting for GTT to drain (in-use: %d bytes)...", before)
	const timeoutS = 20
	for elapsed := 0; elapsed < timeoutS; elapsed += 2 {
		m.pause(2)
		now, ok := m.d.GPU.GTTUsedBytes()
		if !ok {
			continue
		}
		if snap.drainedBy(int64(now)) {
			m.logf("GTT drained: %d bytes → %d bytes (%d of an expected %d bytes back)", snap.used, now, snap.used-int64(now), snap.expected)
			return true
		}
		if now < baselineBytes || float64(now) < float64(before)*0.15 {
			m.logf("GTT drained: %d bytes → %d bytes", before, now)
			return true
		}
	}
	after, _ := m.d.GPU.GTTUsedBytes()
	m.logf("WARNING: GTT still %d bytes after %ds (was %d bytes; expected %d bytes back). Lingering GPU allocation or driver delay.",
		after, timeoutS, before, snap.expected)
	if m.d.OnGTTDrainTimeout != nil {
		m.d.OnGTTDrainTimeout(before, after)
	}
	return false
}

func actualPtr(actual int) *int {
	if actual == 0 {
		return nil
	}
	return &actual
}

func modeNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Modes))
	for name := range cfg.Modes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// boot switches to the configured default mode. V5 keeps V4's `ai-mode
// boot` semantics via the default flag on modes; "unloaded" wins when no
// mode is flagged.
func (m *Manager) Boot(ctx context.Context) Result {
	cfg := m.d.Cfg()
	target := "unloaded"
	for name, mode := range cfg.Modes {
		if mode.Default {
			target = name
			break
		}
	}
	m.logf("boot sequence: starting %s mode", target)
	return m.SwitchMode(ctx, target)
}

// StartUnit implements Engine: start an auxiliary (non-inference) systemd
// unit. It refuses inference-slot units so callers can't bypass the
// scheduler's slot-state tracking — those go through Load/SwitchMode. See
// the Contract 2 C1-Q2 amendment.
func (m *Manager) StartUnit(ctx context.Context, unit string) error {
	if err := m.guardAuxUnit(unit); err != nil {
		return err
	}
	return m.d.Sys.Start(ctx, unit)
}

// StopUnit implements Engine: stop an auxiliary systemd unit (see StartUnit).
func (m *Manager) StopUnit(ctx context.Context, unit string) error {
	if err := m.guardAuxUnit(unit); err != nil {
		return err
	}
	return m.d.Sys.Stop(ctx, unit)
}

// guardAuxUnit rejects an empty unit or one that belongs to an inference
// slot (those must go through Load/Unload/SwitchMode so slot state stays
// consistent).
func (m *Manager) guardAuxUnit(unit string) error {
	if unit == "" {
		return fmt.Errorf("empty unit name")
	}
	for _, slot := range m.d.Cfg().Slots {
		if slot.Unit == unit {
			return fmt.Errorf("unit %q is an inference slot — use Load/Unload, not StartUnit/StopUnit", unit)
		}
	}
	return nil
}

// pooled reports whether modeName is a catalog config hosted by the specialist pool, which owns it exclusively.
func (m *Manager) pooled(ctx context.Context, modeName string) bool {
	return m.d.PooledConfig != nil && m.d.PooledConfig(ctx, modeName)
}

// PooledMessage is the plain-language refusal for loading a pooled config into an LLM slot.
func pooledMessage(modeName string) string {
	return fmt.Sprintf("%s is a specialist: it is kept loaded by the Specialists pool, so it is not loaded into an LLM slot. Manage it in the Specialists tab.", modeName)
}

// joinNotes joins non-empty notes for Result.Detail.
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// modelsCaps reads the capability flags a /v1/models entry carries (the chat-template probe's fallback for an engine that serves no /props).
func (m *Manager) modelsCaps(ctx context.Context, port int) (map[string]bool, error) {
	info, err := m.llama.ModelsInfo(ctx, port)
	if err != nil {
		return nil, err
	}
	return info.Caps, nil
}
