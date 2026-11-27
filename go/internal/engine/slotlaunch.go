// SPDX-License-Identifier: Apache-2.0

package engine

// Phase 1b (ADR-0026 section 2): the daemon writes a slot_launches row, disarms it when the slot's unit is gone, and adopts the rows of slots that were
// already running when the daemon started. The slot shim (`forge slot-exec`) launches from the row; since step d2 the per-slot env/args files are no
// longer written and the row is also what the engine reads back (SlotStates, slotHolding, the collector's weight accounting).
//
// Order of a load's writes, and why (writeSlotLaunch):
//
//  1. validate the slot name and engine kind, build the launch (pure; a refusal writes nothing at all);
//  2. Put the row. If that fails, the old armed row still describes the OLD config, so it is disarmed (compare-and-set) and the load fails loudly.
//     The unit is inactive and is not started, so nothing can run from either.
//
// The whole sequence runs inside opMu while the slot's unit is inactive, which is what makes "a row is only replaced while its unit is inactive" true.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/launch"
	"github.com/jsaigou/the-forge/internal/store"
)

// SlotLaunchStore is the part of the store's slot_launches API the engine uses (store.DB.SlotLaunches() satisfies it). Nil in Deps turns the dual write
// off: the engine then behaves exactly as before step d1.
type SlotLaunchStore interface {
	Put(ctx context.Context, l store.SlotLaunch) (int64, error)
	Get(ctx context.Context, slot string) (store.SlotLaunch, error)
	Disarm(ctx context.Context, slot string, generation int64) (bool, error)
}

// ErrProcUnavailable is returned by a ProcInspector that cannot read process command lines on this host (not Linux, or no /proc). Adoption then does
// nothing.
var ErrProcUnavailable = errors.New("engine: process inspection unavailable")

// ProcInspector reads a live process's command line and environment. It is an interface so adoption is testable without /proc.
type ProcInspector interface {
	// Cmdline returns the full argv of pid (argv[0] first).
	Cmdline(pid uint32) ([]string, error)
	// Environ returns pid's environment.
	Environ(pid uint32) (map[string]string, error)
}

// hostProc reads /proc/<pid>/{cmdline,environ}. Root "" means /proc, and then only on Linux.
type hostProc struct{ root string }

// NewHostProc returns the production ProcInspector. root overrides /proc (tests); "" uses /proc and reports ErrProcUnavailable off Linux.
func NewHostProc(root string) ProcInspector { return hostProc{root: root} }

func (h hostProc) read(pid uint32, name string) ([]byte, error) {
	root := h.root
	if root == "" {
		if runtime.GOOS != "linux" {
			return nil, ErrProcUnavailable
		}
		root = "/proc"
	}
	b, err := fs.ReadFile(os.DirFS(root), strconv.FormatUint(uint64(pid), 10)+"/"+name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && h.root == "" {
			if _, serr := os.Stat(root); serr != nil {
				return nil, ErrProcUnavailable
			}
		}
		return nil, err
	}
	return b, nil
}

func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

func (h hostProc) Cmdline(pid uint32) ([]string, error) {
	b, err := h.read(pid, "cmdline")
	if err != nil {
		return nil, err
	}
	return splitNUL(b), nil
}

func (h hostProc) Environ(pid uint32) (map[string]string, error) {
	b, err := h.read(pid, "environ")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, kv := range splitNUL(b) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

// BuildSlotLaunchRow is the one place that turns (config, slot, mode, service) into the slot_launches row the daemon writes: the launch from
// launch.BuildService, the row metadata, and the store's own validation. The daemon's load and adopt paths and `forge launch-check` all call it, so the
// preflight can never disagree with what a load would do. WrittenAt and WrittenBy are left for the caller.
func BuildSlotLaunchRow(cfg *config.Config, slot, modeName string, configID int64, svc config.Service) (store.SlotLaunch, error) {
	l, err := launch.BuildService(cfg, slot, svc)
	if err != nil {
		return store.SlotLaunch{}, err
	}
	in, err := launch.FromService(cfg, slot, svc) // the paths and port for the row; BuildService used the same inputs
	if err != nil {
		return store.SlotLaunch{}, err
	}
	spec, err := svc.Engine()
	if err != nil {
		return store.SlotLaunch{}, err
	}
	row := l.Row(slot, in.Port)
	row.ConfigName = modeName
	if configID > 0 {
		id := configID
		row.ConfigID = &id
	}
	row.EngineKind = spec.Name
	row.Backend = svc.Backend
	row.ModelPath = in.ModelPath
	row.MmprojPath = in.MmprojPath
	row.NCtx = svc.Context
	if bad := row.Validate(); bad != nil {
		return store.SlotLaunch{}, bad
	}
	return row, nil
}

// BuildSlotLaunchRowOn is BuildSlotLaunchRow for a slot whose port the caller supplies: a specialist pool slot, which is not in cfg.Slots. The pool
// hosts an engine whose launch is a row (Radiance) by writing this row and starting a unit whose launcher runs `forge slot-exec <slot>`.
func BuildSlotLaunchRowOn(cfg *config.Config, slot string, port int, modeName string, configID int64, svc config.Service) (store.SlotLaunch, error) {
	l, err := launch.BuildServiceOn(cfg, slot, port, svc)
	if err != nil {
		return store.SlotLaunch{}, err
	}
	in, err := launch.FromServicePort(cfg, slot, port, svc)
	if err != nil {
		return store.SlotLaunch{}, err
	}
	spec, err := svc.Engine()
	if err != nil {
		return store.SlotLaunch{}, err
	}
	row := l.Row(slot, in.Port)
	row.ConfigName = modeName
	if configID > 0 {
		id := configID
		row.ConfigID = &id
	}
	row.EngineKind = spec.Name
	row.Backend = svc.Backend
	row.ModelPath = in.ModelPath
	row.MmprojPath = in.MmprojPath
	row.NCtx = svc.Context
	if bad := row.Validate(); bad != nil {
		return store.SlotLaunch{}, bad
	}
	return row, nil
}

// writeSlotLaunch is the single call both load paths make before they start a slot's unit (see the file comment for the order).
//
// Shadow vs enforce (Deps.SlotLaunchEnforce). Production enforces since step e5 (the shim reads the row). In shadow mode (the Deps zero value, tests
// only) a failure to BUILD or WRITE the row does not fail the call: the previous row is disarmed by compare-and-set so it can never be taken for the
// new launch, the problem is logged at WARN, counted, reported through Deps.OnSlotLaunchWarning and returned as warning (the caller puts it in
// Result.Detail). Since step d2 there are no env/args files, so a unit started after a shadow-mode failure has nothing to launch from. With enforce
// the same failures return an error and the load fails. An invalid slot name or unknown engine kind is an error in both modes.
func (m *Manager) writeSlotLaunch(ctx context.Context, slot, modeName string, configID int64, svc config.Service) (warning string, err error) {
	if err := validateSlotService(slot, svc); err != nil {
		return "", err
	}
	if m.d.SlotLaunches == nil {
		return "", nil
	}
	// fail handles a row-write problem in shadow mode: record it, disarm the old row and carry on.
	fail := func(err error) (string, error) {
		return m.shadowWarning(ctx, slot, modeName, err), nil
	}
	cfg := m.d.Cfg()
	row, err := BuildSlotLaunchRow(cfg, slot, modeName, configID, svc)
	if err != nil {
		berr := fmt.Errorf("slot launch for %s could not be built: %w", slot, err)
		if m.d.SlotLaunchEnforce {
			m.logf("ERROR: slot launch for %s (%s) could not be built, nothing written: %v", slot, modeName, err)
			return "", berr
		}
		return m.shadowWarning(ctx, slot, modeName, berr), nil
	}

	row.WrittenAt = m.d.Now()
	row.WrittenBy = "engine"
	gen, err := m.d.SlotLaunches.Put(ctx, row)
	if err != nil {
		m.logf("ERROR: slot launch row for %s (%s) could not be written: %v", slot, modeName, err)
		if m.d.SlotLaunchEnforce {
			if derr := m.disarmSlotRow(ctx, slot); derr != nil {
				m.logf("ERROR: and the previous row for %s could not be disarmed either (stale armed row): %v", slot, derr)
				return "", fmt.Errorf("slot launch row for %s could not be written: %w (previous row also not disarmed: %v)", slot, err, derr)
			}
			return "", fmt.Errorf("slot launch row for %s could not be written: %w", slot, err)
		}
		return fail(fmt.Errorf("slot launch row for %s could not be written: %w", slot, err))
	}
	m.logf("slot launch row written: slot=%s config=%s generation=%d spec_hash=%s", slot, modeName, gen, store.SlotLaunchSpecHash(row.Exe, row.Argv, row.Env, row.Cwd))
	return "", nil
}

// shadowWarning is the shadow-mode reaction to a build or row-write failure: WARN log, counter, callback, disarm of the previous row (so it cannot
// be mistaken for the new launch). It returns the text for Result.Detail, which also says if the disarm itself failed.
func (m *Manager) shadowWarning(ctx context.Context, slot, modeName string, cause error) string {
	m.launchWarnings.Add(1)
	msg := fmt.Sprintf("slot launch warning (shadow mode, load continues): slot=%s config=%s: %v", slot, modeName, cause)
	if derr := m.disarmSlotRow(ctx, slot); derr != nil {
		msg += fmt.Sprintf("; previous row also not disarmed: %v", derr)
	}
	m.logf("WARN: %s", msg)
	if m.d.OnSlotLaunchWarning != nil {
		m.d.OnSlotLaunchWarning(slot, modeName, msg)
	}
	return msg
}

// SlotLaunchWarnings is the number of shadow-mode build/row failures since the daemon started.
func (m *Manager) SlotLaunchWarnings() int64 { return m.launchWarnings.Load() }

// disarmSlotRow disarms the slot's current row by compare-and-set on the generation it reads now. No row, or a row that is already disarmed or moved
// to another generation, is not an error; a store failure is returned. Callers hold opMu and have confirmed the unit is gone.
func (m *Manager) disarmSlotRow(ctx context.Context, slot string) error {
	if m.d.SlotLaunches == nil {
		return nil
	}
	row, err := m.d.SlotLaunches.Get(ctx, slot)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read row of %s: %w", slot, err)
	}
	if !row.Armed {
		return nil
	}
	changed, err := m.d.SlotLaunches.Disarm(ctx, slot, row.Generation)
	if err != nil {
		return fmt.Errorf("disarm row of %s at generation %d: %w", slot, row.Generation, err)
	}
	if !changed {
		m.logf("slot launch row of %s moved past generation %d before it could be disarmed (left as is)", slot, row.Generation)
	}
	return nil
}

// disarmAfterStop disarms slot's row once its unit is confirmed gone; a failure is logged and remembered for the caller's Result (never swallowed).
func (m *Manager) disarmAfterStop(ctx context.Context, slot string) {
	if err := m.disarmSlotRow(ctx, slot); err != nil {
		m.logf("ERROR: slot launch row of %s was not disarmed after unload: %v", slot, err)
		m.disarmFailures = append(m.disarmFailures, fmt.Sprintf("%s: %v", slot, err))
	}
}

// takeDisarmFailures returns (and clears) the failures disarmAfterStop recorded, as one stable-prefixed line, or "". Callers hold opMu.
func (m *Manager) takeDisarmFailures() string {
	if len(m.disarmFailures) == 0 {
		return ""
	}
	s := "slot launch row not disarmed: " + strings.Join(m.disarmFailures, "; ")
	m.disarmFailures = nil
	return s
}

// adoptRunningSlots is the daemon-start adopt step: for every slot whose unit is active and has no armed row, write a row only if the live process
// is exactly what the builder produces for the recorded config. Anything else writes nothing and is logged (argument index or variable name only,
// never a value). Runs under opMu.
func (m *Manager) adoptRunningSlots(ctx context.Context) {
	if m.d.SlotLaunches == nil || m.d.Procs == nil {
		return
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	cfg := m.d.Cfg()
	names := make([]string, 0, len(cfg.Slots))
	for n := range cfg.Slots {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, slot := range names {
		stop, err := m.adoptSlot(ctx, cfg, slot)
		if err != nil {
			m.logf("adopt %s: skipped: %v", slot, err)
		}
		if stop {
			return
		}
	}
}

// adoptSlot returns stop=true when /proc is unavailable (no point asking for the other slots).
func (m *Manager) adoptSlot(ctx context.Context, cfg *config.Config, slot string) (stop bool, err error) {
	unit := cfg.Slots[slot].Unit
	st, serr := m.d.Sys.State(ctx, unit)
	if serr != nil || !st.Active() {
		return false, nil // inactive unit: nothing to adopt, whatever its row says
	}
	row, gerr := m.d.SlotLaunches.Get(ctx, slot)
	switch {
	case gerr == nil && row.Armed:
		return false, nil // already has an armed row; adoption never replaces a row
	case gerr != nil && !errors.Is(gerr, store.ErrNotFound):
		return false, fmt.Errorf("read existing row: %w", gerr)
	}

	// The config is named by the slot's last row (armed or not); the live-process comparison below is what proves it is still true.
	modeName := ""
	if gerr == nil {
		modeName = modeFromRow(cfg, row)
	}
	if modeName == "" {
		return false, errors.New("running unit with no launch row naming a known config; nothing adopted")
	}
	mode := cfg.Modes[modeName]
	svc, ok := adoptService(mode, slot)
	if !ok {
		return false, fmt.Errorf("config %s has no service to compare", modeName)
	}
	r, berr := BuildSlotLaunchRow(cfg, slot, modeName, mode.ConfigID, svc)
	if berr != nil {
		return false, fmt.Errorf("config %s cannot be built: %w", modeName, berr)
	}
	want := launch.Launch{Exe: r.Exe, Argv: r.Argv, Env: r.Env, Cwd: r.Cwd}

	pid, perr := m.d.Sys.MainPID(ctx, unit)
	if perr != nil || pid == 0 {
		return false, fmt.Errorf("no main PID for %s (err=%v)", unit, perr)
	}
	cmd, cerr := m.d.Procs.Cmdline(pid)
	if errors.Is(cerr, ErrProcUnavailable) {
		m.logf("adopt: process inspection unavailable on this host; no slot adopted")
		return true, nil
	}
	if cerr != nil {
		return false, fmt.Errorf("read command line of pid %d: %w", pid, cerr)
	}
	env, eerr := m.d.Procs.Environ(pid)
	if eerr != nil {
		return false, fmt.Errorf("read environment of pid %d: %w", pid, eerr)
	}
	if again, aerr := m.d.Sys.MainPID(ctx, unit); aerr != nil || again != pid {
		return false, fmt.Errorf("main PID of %s changed during inspection", unit)
	}
	if why := compareLive(want, cmd, env); why != "" {
		return false, fmt.Errorf("live process of %s does not match config %s: %s; nothing written", unit, modeName, why)
	}

	r.WrittenAt = m.d.Now()
	r.WrittenBy = "engine-adopt"
	gen, perr2 := m.d.SlotLaunches.Put(ctx, r)
	if perr2 != nil {
		return false, fmt.Errorf("write adopted row: %w", perr2)
	}
	m.logf("adopt %s: running process matches config %s exactly; row written generation=%d spec_hash=%s",
		slot, modeName, gen, store.SlotLaunchSpecHash(want.Exe, want.Argv, want.Env, want.Cwd))
	return false, nil
}

// adoptService picks the service of mode that runs in slot: the one whose port role is the slot (a SwitchMode-loaded mode), else the first (what
// Load uses, overriding the port role).
func adoptService(mode config.Mode, slot string) (config.Service, bool) {
	for _, s := range mode.Services {
		if s.PortRole == slot {
			return s, true
		}
	}
	if len(mode.Services) > 0 {
		s := mode.Services[0]
		s.PortRole = slot
		return s, true
	}
	return config.Service{}, false
}

// compareLive returns "" when cmd is exactly [Exe, Argv...] and every variable the launch adds is present with the same value, else a reason that
// names an argument index or a variable name and never a value.
func compareLive(want launch.Launch, cmd []string, env map[string]string) string {
	exp := append([]string{want.Exe}, want.Argv...)
	for i := 0; i < len(exp) && i < len(cmd); i++ {
		if exp[i] != cmd[i] {
			if i == 0 {
				return "executable (argv[0]) differs"
			}
			return fmt.Sprintf("argument %d differs", i-1)
		}
	}
	if len(exp) != len(cmd) {
		return fmt.Sprintf("argument count differs (expected %d, live %d)", len(exp)-1, len(cmd)-1)
	}
	names := make([]string, 0, len(want.Env))
	for k := range want.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v, ok := env[k]
		if !ok {
			return fmt.Sprintf("environment variable %s is missing", k)
		}
		if v != want.Env[k] {
			return fmt.Sprintf("environment variable %s differs", k)
		}
	}
	return ""
}
