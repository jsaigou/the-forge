// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/store"
)

// SwitchNotifier is what the Manager pings when a switch/load completes so
// hang detection opens its cooldown window (collector.NotifySwitchComplete).
type SwitchNotifier interface {
	NotifySwitchComplete()
}

// Deps wires a Manager. Cfg and Sys are required; the rest degrade to
// sensible defaults or no-ops (nil Usage: history is not persisted).
type Deps struct {
	Cfg func() *config.Config
	Sys Systemd

	GPU  *collector.GPU
	Proc collector.Proc

	// Usage persists mode history + load/unload events (store.Usage).
	Usage store.Usage

	// WeightEstimateBytes is the catalog-backed lookup for curated memory
	// requirements (was MemoryReqBytes in V4, reading models.toml's
	// performance.memory_req_gb; now reads the safe_memory_bytes benchmark
	// from the catalog by config ID — Phase B B2). Nil or !ok falls back
	// to on-disk weight size.
	WeightEstimateBytes func(configID int64) (int64, bool)

	// ProfileBytes is the profiled safe-memory footprint (PROFILE track,
	// docs/v5-profiling-benchmarks.md). When a fresh profile exists this is
	// the authoritative needBytes — consulted before WeightEstimateBytes /
	// weight fallback so the fit check uses measured memory (weights + KV
	// cache) rather than the weight-only estimate. Nil or !ok falls back.
	ProfileBytes func(mode string) (bytes int64, ok bool)

	// ComfyUIFootprintBytes reports the deployment's ComfyUI service live
	// GPU footprint (fdinfo bytes for its main PID), or 0 when unavailable
	// (not running, no unit configured, probe failure). Nil = the feature
	// is off and FitPlan never proposes evicting it. The unit name lives in
	// the smith.comfyui.unit settings key (deployment data — never compiled
	// in); forge wires this closure so the ENGINE stays settings-free
	// while the fit check's recovery math can still count ComfyUI's real
	// footprint (it rides in GTT like any other consumer — S1 of the ops
	// sprint series made it an eviction candidate).
	ComfyUIFootprintBytes func() int64

	// Notify opens the post-switch hang-detection cooldown.
	Notify SwitchNotifier

	// OnGTTDrainTimeout fires when waitGTTDrain's 20s drain window expires
	// with GTT still elevated (lifecycle.go's "WARNING: GTT still ... after
	// 20s" path). This is a strong pre-hang / post-unload signal — it fired
	// before both 2026-08-16 device-lost hangs and on the 22:50 reload — and
	// surfacing it to smith (via main.go wiring it to a bus event) closes a
	// detection gap that only ever lived in the daemon log. Nil ⇒ the
	// warning is logged (unchanged behavior) but not surfaced. Arguments are
	// the before/after GTT bytes.
	OnGTTDrainTimeout func(before, after int64)

	// ReadMeta reads GGUF metadata (default gguf.ReadMetadata).
	ReadMeta func(path string) (gguf.Metadata, error)

	// BaseURL overrides llama-server probe URLs (tests).
	BaseURL func(port int) string

	// Kill terminates a lingering inference PID (default SIGKILL).
	Kill func(pid int) error

	// PollInterval is the real duration of one "virtual second" in wait
	// loops (default 1s; tests shrink it). All V4 timeout arithmetic is
	// preserved in virtual seconds.
	PollInterval time.Duration

	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Manager is the production engine.Engine: the V4 engine.py port. All
// lifecycle operations are blocking (Contract 2); callers goroutine them.
type Manager struct {
	d     Deps
	llama *collector.LlamaClient

	// opMu serializes lifecycle operations (switch/load/unload) — V4's
	// app.py "switching" flag, now engine-owned.
	opMu sync.Mutex

	// mu guards slots.
	mu    sync.Mutex
	slots map[string]*slotRec
}

type slotRec struct {
	mode      string
	loading   *collector.Transition
	unloading *collector.Transition
}

var _ Engine = (*Manager)(nil)
var _ collector.SlotStateSource = (*Manager)(nil)

// NewManager builds a Manager and reconciles initial slot state from
// sysconfig env files + live unit states (restart recovery without any
// slots.json — V5 design decision 3).
func NewManager(d Deps) (*Manager, error) {
	if d.Cfg == nil || d.Sys == nil {
		return nil, fmt.Errorf("engine: Cfg and Sys are required")
	}
	if d.GPU == nil {
		d.GPU = &collector.GPU{}
	}
	if d.ReadMeta == nil {
		d.ReadMeta = gguf.ReadMetadata
	}
	if d.Kill == nil {
		d.Kill = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	}
	if d.PollInterval == 0 {
		d.PollInterval = time.Second
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	m := &Manager{
		d:     d,
		llama: collector.NewLlamaClient(d.BaseURL),
		slots: map[string]*slotRec{},
	}
	for name := range d.Cfg().Slots {
		m.slots[name] = &slotRec{}
	}
	m.reconcileLive(context.Background())
	return m, nil
}

// pause sleeps n virtual seconds (n × PollInterval).
func (m *Manager) pause(n int) {
	time.Sleep(time.Duration(n) * m.d.PollInterval)
}

func (m *Manager) logf(format string, args ...any) { m.d.Logf("engine: "+format, args...) }

// Slots implements Engine: configured slot names in display order.
func (m *Manager) Slots() []string {
	cfg := m.d.Cfg()
	names := make([]string, 0, len(cfg.Slots))
	for name := range cfg.Slots {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := cfg.Slots[names[i]], cfg.Slots[names[j]]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return names[i] < names[j]
	})
	return names
}

// stateFilePath is V4's /var/lib/forge/current.
func (m *Manager) stateFilePath() string {
	return filepath.Join(m.d.Cfg().Paths.StateDir, "current")
}

// saveMode writes the authoritative mode tiebreaker file (single writer in
// V5 — the fcntl.flock dance is gone by design). Atomic via rename.
func (m *Manager) saveMode(mode string) {
	path := m.stateFilePath()
	line := fmt.Sprintf("%s %s\n", mode, m.d.Now().Format(time.RFC3339))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(line), 0o600); err != nil {
		m.logf("WARNING: could not write state file %s: %v", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		m.logf("WARNING: could not write state file %s: %v", path, err)
	}
}

func (m *Manager) recordedMode() string {
	raw, err := os.ReadFile(m.stateFilePath())
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// serviceUnit maps a service to its systemd unit via its port_role's slot
// ("" when the role names no configured slot).
func serviceUnit(cfg *config.Config, svc config.Service) string {
	if slot, ok := cfg.Slots[svc.PortRole]; ok {
		return slot.Unit
	}
	return ""
}

// canonicalUnits is the set of configured inference-slot units.
func canonicalUnits(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, slot := range cfg.Slots {
		if slot.Unit != "" {
			out[slot.Unit] = true
		}
	}
	return out
}

// modeUnits is the set of units a mode's services run in.
func modeUnits(cfg *config.Config, mode config.Mode) map[string]bool {
	out := map[string]bool{}
	for _, svc := range mode.Services {
		if u := serviceUnit(cfg, svc); u != "" {
			out[u] = true
		}
	}
	return out
}

func (m *Manager) unitActive(ctx context.Context, unit string) bool {
	st, err := m.d.Sys.State(ctx, unit)
	return err == nil && st.Active()
}

// CurrentMode implements Engine (port of engine.get_current_mode): trust the
// state file when its mode's expected units are all active and no unexpected
// canonical slot unit is; otherwise reconcile from live canonical-slot state.
// Non-inference services (ComfyUI, embedding, STT) are intentionally outside
// the canonical set and can never cause a false mismatch.
func (m *Manager) CurrentMode() string {
	ctx := context.Background()
	cfg := m.d.Cfg()
	canonical := canonicalUnits(cfg)
	recorded := m.recordedMode()

	if mode, ok := cfg.Modes[recorded]; ok && recorded != "" {
		expected := modeUnits(cfg, mode)
		allUp := true
		for u := range expected {
			if !m.unitActive(ctx, u) {
				allUp = false
				break
			}
		}
		if allUp {
			unexpected := false
			for u := range canonical {
				if !expected[u] && m.unitActive(ctx, u) {
					unexpected = true
					break
				}
			}
			if !unexpected {
				return recorded
			}
		}
	}

	// State file absent or inconsistent — reconcile from canonical units.
	active := map[string]bool{}
	for u := range canonical {
		if m.unitActive(ctx, u) {
			active[u] = true
		}
	}
	for name, mode := range cfg.Modes {
		if mode.Type == "service" {
			continue
		}
		mu := modeUnits(cfg, mode)
		if len(mu) == len(active) && allIn(mu, active) {
			m.saveMode(name)
			return name
		}
	}
	if recorded != "" {
		return recorded
	}
	return "unknown"
}

func allIn(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// SlotStates implements collector.SlotStateSource: the crown-jewels slot
// reconciliation (port of engine._reconcile_slot_state).
//
//   - A slot whose unit is ACTIVE but untracked gets its mode inferred from
//     the sysconfig env file.
//   - A tracked slot whose unit has left is cleared ONLY when the unit is
//     genuinely gone (inactive/failed) — NEVER while "deactivating"
//     (TimeoutStopSec=300: large models shut down for minutes; clearing
//     early would let the scheduler place a load into a slot whose old
//     process still holds its memory). "activating" also keeps state: a
//     restarting unit is not an empty slot.
func (m *Manager) SlotStates(units map[string]collector.UnitState) map[string]collector.SlotAssignment {
	cfg := m.d.Cfg()
	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string]collector.SlotAssignment{}
	for name, slot := range cfg.Slots {
		rec, ok := m.slots[name]
		if !ok {
			rec = &slotRec{}
			m.slots[name] = rec
		}
		st := units[slot.Unit]
		switch {
		case st.Active():
			if rec.mode == "" {
				if inferred := inferSlotMode(cfg, collector.ReadSlotEnv(cfg.Paths.SysconfigDir, name)); inferred != "" {
					rec.mode = inferred
				}
			}
		case rec.mode != "" && (st.ActiveState == "inactive" || st.ActiveState == "failed" || st.ActiveState == "dead"):
			rec.mode = ""
		}
		out[name] = collector.SlotAssignment{
			Mode:      rec.mode,
			Loading:   rec.loading,
			Unloading: rec.unloading,
		}
	}
	return out
}

// reconcileLive queries live unit states for every slot and runs
// reconciliation — used at startup and at scheduling decision points.
func (m *Manager) reconcileLive(ctx context.Context) map[string]collector.SlotAssignment {
	cfg := m.d.Cfg()
	units := map[string]collector.UnitState{}
	for _, slot := range cfg.Slots {
		if st, err := m.d.Sys.State(ctx, slot.Unit); err == nil {
			units[slot.Unit] = st
		}
		// On error the unit is simply absent from the map: SlotStates
		// treats unknown as zero-value ("" ActiveState) which neither
		// infers nor clears — stale-but-safe, same rule as the collector.
	}
	return m.SlotStates(units)
}

func (m *Manager) setTransition(slot string, loading, unloading *collector.Transition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.slots[slot]; ok {
		rec.loading = loading
		rec.unloading = unloading
	}
}

func (m *Manager) setSlotMode(slot, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.slots[slot]; ok {
		rec.mode = mode
	}
}

func (m *Manager) clearAllSlots() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.slots {
		rec.mode = ""
		rec.loading = nil
		rec.unloading = nil
	}
}

func (m *Manager) notifySwitchComplete() {
	if m.d.Notify != nil {
		m.d.Notify.NotifySwitchComplete()
	}
}
