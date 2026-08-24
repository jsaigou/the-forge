// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

// deactivatingWaitS bounds waits for a unit to leave "deactivating":
// TimeoutStopSec=300 plus margin (virtual seconds).
const deactivatingWaitS = 330

// SwitchMode implements Engine (port of engine.switch_mode): stop managed
// units → write sysconfig env/args → start target units → verify /health and
// /props (n_ctx). Blocking.
func (m *Manager) SwitchMode(ctx context.Context, modeName string) Result {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	cfg := m.d.Cfg()
	mode, ok := cfg.Modes[modeName]
	if !ok {
		return Result{Success: false, Message: fmt.Sprintf("Unknown mode: %s. Valid: %v", modeName, modeNames(cfg))}
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

	start := m.d.Now()
	var verifiedCtx int

	for _, svc := range mode.Services {
		if _, ok := cfg.Slots[svc.PortRole]; ok {
			if err := m.writeServiceFiles(svc.PortRole, svc); err != nil {
				return Result{Success: false, Message: err.Error()}
			}
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
			if !m.waitServiceRunning(ctx, unit, port, svc) {
				_ = m.d.Sys.Stop(ctx, unit)
				m.recordHistory(modeName, svc, nil, m.d.Now().Sub(start), true)
				return Result{Success: false, Message: fmt.Sprintf("%s did not reach running state", unit)}
			}
			actual := m.verifiedContext(ctx, port, svc)
			m.recordHistory(modeName, svc, actualPtr(actual), m.d.Now().Sub(start), false)
			m.setSlotMode(svc.PortRole, modeName)
			if verifiedCtx == 0 {
				verifiedCtx = actual
			}
		}
	}

	m.saveMode(modeName)
	m.notifySwitchComplete()
	m.logf("mode %s active", modeName)
	return Result{Success: true, Message: fmt.Sprintf("Switched to %s", modeName), NCtx: verifiedCtx}
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

	if err := m.writeServiceFiles(slot, svc); err != nil {
		return Result{Success: false, Message: err.Error()}
	}

	if err := m.d.Sys.Start(ctx, unit); err != nil {
		m.recordHistory(modeName, svc, nil, m.d.Now().Sub(start), true)
		return Result{Success: false, Message: fmt.Sprintf("Failed to start %s", unit)}
	}
	if !m.waitServiceRunning(ctx, unit, port, svc) {
		_ = m.d.Sys.Stop(ctx, unit)
		m.recordHistory(modeName, svc, nil, m.d.Now().Sub(start), true)
		return Result{Success: false, Message: fmt.Sprintf("%s did not reach running state", unit)}
	}

	actual := m.verifiedContext(ctx, port, svc)
	m.recordHistory(modeName, svc, actualPtr(actual), m.d.Now().Sub(start), false)
	m.setSlotMode(slot, modeName)
	m.notifySwitchComplete()
	m.logf("slot %s active with %s", slot, modeName)
	return Result{Success: true, Message: fmt.Sprintf("Loaded %s into %s", modeName, slot), NCtx: actual}
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

	m.mu.Lock()
	prevMode := ""
	if rec, ok := m.slots[slot]; ok {
		prevMode = rec.mode
	}
	m.mu.Unlock()
	m.setTransition(slot, nil, &collector.Transition{Mode: prevMode, StartedAt: m.d.Now()})
	defer m.setTransition(slot, nil, nil)

	_ = m.d.Sys.Stop(ctx, unit)
	if !m.waitUnitGone(ctx, unit, deactivatingWaitS) {
		// Still deactivating: the slot is NOT free. Keep its mode.
		return Result{Success: false, Message: fmt.Sprintf("%s is still deactivating; slot not freed yet", unit)}
	}

	m.killLingering(slotCfg.Port)
	m.waitGTTDrain()

	m.setSlotMode(slot, "")
	m.recordUsageEvent("unload", "", slot, "")
	return Result{Success: true, Message: fmt.Sprintf("Unloaded %s slot", slot)}
}

func (m *Manager) unloadAll(ctx context.Context) Result {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	cfg := m.d.Cfg()
	m.logf("unloading all inference slots")
	m.stopAll(ctx, cfg)
	m.clearAllSlots()
	m.logf("all slots unloaded")
	return Result{Success: true, Message: "All slots unloaded"}
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
	units := make([]string, 0, len(canonicalUnits(cfg)))
	for u := range canonicalUnits(cfg) {
		units = append(units, u)
	}
	sort.Strings(units)

	for _, u := range units {
		if err := m.d.Sys.Stop(ctx, u); err != nil {
			m.logf("stop %s returned error: %v", u, err)
		}
	}

	ok := true
	for _, u := range units {
		if !m.waitUnitGone(ctx, u, 5) {
			st, _ := m.d.Sys.State(ctx, u)
			m.logf("WARNING: %s still %s/%s after stop", u, st.ActiveState, st.SubState)
			ok = false
		}
	}

	var allPorts []int
	for _, s := range cfg.Slots {
		allPorts = append(allPorts, s.Port)
	}
	m.killLingering(allPorts...)
	m.waitGTTDrain()
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
func (m *Manager) waitServiceRunning(ctx context.Context, unit string, port int, svc config.Service) bool {
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
				return false
			}
		}
		if elapsed >= timeoutS {
			m.logf("TIMEOUT: %s not running after %ds", unit, timeoutS)
			return false
		}
		m.pause(2)
		elapsed += 2
	}

	if port == 0 {
		m.logf("%s running (no health endpoint)", unit)
		return true
	}

	deadline := elapsed + timeoutS
	for elapsed < deadline {
		st, err := m.d.Sys.State(ctx, unit)
		if err == nil {
			switch st.SubState {
			case "failed", "inactive", "dead", "auto-restart":
				m.logf("%s crashed during startup (SubState=%s)", unit, st.SubState)
				return false
			}
		}
		if m.llama.Healthy(ctx, port) {
			m.logf("%s ready (port %d healthy)", unit, port)
			return true
		}
		m.pause(3)
		elapsed += 3
	}
	m.logf("TIMEOUT: %s /health on port %d never returned ok", unit, port)
	return false
}

// verifiedContext runs the n_ctx verification for a freshly started service
// (crown jewels: the kernel may silently lower context on failed contiguous
// GTT allocation — always check /props and record the actual value).
// Returns the actual n_ctx (0 when unavailable). vLLM services skip /props
// and report their configured context (V4 parity).
func (m *Manager) verifiedContext(ctx context.Context, port int, svc config.Service) int {
	if svc.Backend == "vllm" {
		return svc.Context
	}
	if port == 0 {
		return 0
	}
	_, actual := m.verifyModelContext(ctx, port, svc.Context)
	return actual
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
	for _, name := range []string{"llama-server", "vllm"} {
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

// waitGTTDrain polls until the GTT counter drops to near-baseline (port of
// engine._wait_gtt_drain: amdgpu TTM can hold GTT pages briefly after
// process exit on Strix Halo; loading immediately would OOM on memory that
// is already logically free).
//
// A1 (bytes retrofit): thresholds were 1024 MB; now 1 GiB in bytes.
func (m *Manager) waitGTTDrain() {
	const baselineBytes = 1 << 30 // 1 GiB — "near zero" floor (was 1024 MB)
	before, ok := m.d.GPU.GTTUsedBytes()
	if !ok || before < baselineBytes {
		return
	}
	m.logf("waiting for GTT to drain (in-use: %d bytes)...", before)
	const timeoutS = 20
	for elapsed := 0; elapsed < timeoutS; elapsed += 2 {
		m.pause(2)
		now, ok := m.d.GPU.GTTUsedBytes()
		if !ok {
			continue
		}
		if now < baselineBytes || float64(now) < float64(before)*0.15 {
			m.logf("GTT drained: %d bytes → %d bytes", before, now)
			return
		}
	}
	after, _ := m.d.GPU.GTTUsedBytes()
	m.logf("WARNING: GTT still %d bytes after %ds (was %d bytes). Lingering GPU allocation or driver delay.",
		after, timeoutS, before)
	if m.d.OnGTTDrainTimeout != nil {
		m.d.OnGTTDrainTimeout(before, after)
	}
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
