// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/store"
)

// Slot states.
const (
	StateEmpty    = "empty"
	StateStarting = "starting"
	StateRunning  = "running"
	StateFailed   = "failed"
	StateStopped  = "stopped"  // the operator stopped an always-on member; it is not restarted until loaded again
	StateStopping = "stopping" // an unload is in progress (the unit is being stopped; a member mid-generation can take up to ~75 s)
)

// failedAnomalyAttempts is how many failed starts in a row raise the OnFailed hook (a Smith anomaly), once per streak.
const failedAnomalyAttempts = 3

// backoffSchedule is the always-on restart delay after the Nth consecutive failure; the last entry repeats (ADR-0022 decision 3).
var backoffSchedule = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute}

// SlotState is the observable state of one specialist slot.
type SlotState struct {
	Slot        string
	Unit        string
	Port        int
	Occupant    string
	Policy      string
	Functions   []string
	State       string
	Err         string
	Attempts    int
	NextRetry   time.Time
	LoadedAt    time.Time
	LastUse     time.Time
	MemoryBytes int64
	// Leases is how many requests (of any function) currently hold this engine (it is not evicted while that is above zero).
	Leases int
}

// PoolFullError: no slot is free and nothing on-demand can be evicted right now. a0 answers 503 specialist_pool_full.
type PoolFullError struct {
	Occupant   string
	Holders    []string // "s1 kokoro (always_on, running)"
	RetryAfter time.Duration
}

func (e *PoolFullError) Error() string {
	return fmt.Sprintf("specialist pool is full, cannot place %s: %s", e.Occupant, strings.Join(e.Holders, "; "))
}

// ReasonDisabled is UnavailableError.Reason for an occupant whose Behavior is Disabled (callers tell "off on purpose" from "broken").
const ReasonDisabled = "disabled"

// ReasonStopped is UnavailableError.Reason for an always-on engine the operator stopped (it stays stopped until loaded again).
const ReasonStopped = "stopped by the operator; load it to start it again"

// UnavailableError: the occupant cannot answer now (failed, stopped by the operator, disabled, not enough memory). a0 answers 503
// specialist_unavailable.
type UnavailableError struct {
	Occupant string
	Reason   string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("specialist %s unavailable: %s", e.Occupant, e.Reason)
}

// ErrNotFound: no such occupant.
var ErrNotFound = errors.New("specialist: occupant not found")

// Deps wires a Manager. Cfg, Sys, Store and EnvDir are required.
type Deps struct {
	Cfg    func() *config.Config
	Sys    engine.Systemd
	Store  store.Specialist
	EnvDir string // testuser-writable, e.g. /var/lib/forge/pool; one <slot>.env per slot
	// ModelRoots are the directories model files may live under. Nil = none allowed.
	ModelRoots func() []string
	// Health probes GET http://127.0.0.1:port/path. Nil = a real HTTP probe.
	Health func(ctx context.Context, port int, path string) error
	// FreeBytes reports free GPU memory for the fit gate. Nil = no fit gate (the launcher's own OOM handling remains).
	FreeBytes func() int64
	// IdleUnload is how long an on-demand member must be idle before it may be evicted. Nil = 180 s, like the LLM slots.
	IdleUnload func() time.Duration
	Now        func() time.Time
	// Sleep waits d or until ctx ends. Nil = a real sleep. Tests make it instant.
	Sleep func(ctx context.Context, d time.Duration)
	Logf  func(format string, args ...any)
	// OnFailed fires once per failure streak, after failedAnomalyAttempts consecutive failed starts (the Smith anomaly hook).
	OnFailed func(slot, occupant string, attempts int, err error)
	// PollInterval is the health and supervisor cadence. Zero = 500 ms for health, 5 s for the supervisor.
	PollInterval time.Duration
	// Catalog resolves `catalog-config` occupants (a catalog config hosted in the pool). Nil = such an occupant cannot start.
	Catalog CatalogResolver
	// NCtx reads a llama-server's actual n_ctx from /props, for the post-start context check. Nil = the real probe.
	NCtx func(ctx context.Context, port int) (int, error)
	// Busy reports whether a llama-server has a request in flight (/metrics requests_processing), so a long generation is never evicted for
	// being "idle". Nil = the real probe. Only catalog-config members are asked (other kinds do not expose it).
	Busy func(ctx context.Context, port int) bool
	// SpeechDefaults reads the default engine per kind of speech (mode -> engine name): the `tts.defaults` setting. Nil = no defaults.
	SpeechDefaults func(ctx context.Context) (map[string]string, error)
	// SlotLaunches holds the launch rows of row-launched engines (Radiance): the pool writes the row before it starts the unit and disarms it once the
	// unit is down. Nil = such a config cannot start in the pool.
	SlotLaunches SlotLaunchStore
	// LeaseTTL is how long a lease (any function) protects an engine from eviction if its holder never releases it. Zero = 10 minutes.
	LeaseTTL time.Duration
}

type slotRec struct {
	occupant  string
	state     string
	err       string
	attempts  int
	nextRetry time.Time
	loadedAt  time.Time
	lastUse   time.Time
	memory    int64
	done      chan struct{} // closed when a start in flight finishes
	alerted   bool
}

// Manager runs the specialist pool.
type Manager struct {
	d  Deps
	mu sync.Mutex
	// slots holds the runtime record per specialist slot name.
	slots map[string]*slotRec
	// leases are the requests (any function) currently using an engine (speech.go); guarded by mu.
	leases map[string]*speechLease
}

// New builds a Manager.
func New(d Deps) (*Manager, error) {
	if d.Cfg == nil || d.Sys == nil || d.Store == nil || d.EnvDir == "" {
		return nil, errors.New("specialist: Cfg, Sys, Store and EnvDir are required")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Sleep == nil {
		d.Sleep = func(ctx context.Context, dur time.Duration) {
			t := time.NewTimer(dur)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	if d.IdleUnload == nil {
		d.IdleUnload = func() time.Duration { return 180 * time.Second }
	}
	if d.ModelRoots == nil {
		d.ModelRoots = func() []string { return nil }
	}
	if d.Health == nil {
		d.Health = httpHealth
	}
	if d.NCtx == nil || d.Busy == nil {
		lc := collector.NewLlamaClient(nil)
		if d.NCtx == nil {
			d.NCtx = lc.NCtx
		}
		if d.Busy == nil {
			d.Busy = func(ctx context.Context, port int) bool {
				n, err := lc.InFlight(ctx, port)
				if err == nil {
					return n > 0
				}
				// A server that answers 404/501 has /metrics switched off: that says nothing about load, so it can be evicted when idle by the
				// clock. Any other failure (timeout, refused, 5xx) means busy cannot be told, and a llama-server mid-generation can be too busy to
				// answer: keep the member rather than evict it under a running request.
				var se *collector.HTTPStatusError
				if errors.As(err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusNotImplemented) {
					return false
				}
				if d.Logf != nil {
					d.Logf("busy probe on port %d failed (%v): treating the member as busy", port, err)
				}
				return true
			}
		}
	}
	return &Manager{d: d, slots: map[string]*slotRec{}, leases: map[string]*speechLease{}}, nil
}

func httpHealth(ctx context.Context, port int, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		return err
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("health answered %d", resp.StatusCode)
	}
	return nil
}

func (m *Manager) logf(f string, a ...any) { m.d.Logf("specialist: "+f, a...) }

func (m *Manager) poll() time.Duration {
	if m.d.PollInterval > 0 {
		return m.d.PollInterval
	}
	return 500 * time.Millisecond
}

// slotNames returns the configured specialist slots in order.
func (m *Manager) slotNames() []string {
	cfg := m.d.Cfg()
	names := make([]string, 0, len(cfg.SpecialistSlots))
	for n := range cfg.SpecialistSlots {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := cfg.SpecialistSlots[names[i]], cfg.SpecialistSlots[names[j]]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return names[i] < names[j]
	})
	return names
}

func (m *Manager) rec(slot string) *slotRec {
	r, ok := m.slots[slot]
	if !ok {
		r = &slotRec{state: StateEmpty}
		m.slots[slot] = r
	}
	return r
}

// Snapshot returns every specialist slot's state, in slot order.
func (m *Manager) Snapshot(ctx context.Context) []SlotState {
	occ := m.occupantsByName(ctx)
	cfg := m.d.Cfg()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []SlotState{}
	for _, name := range m.slotNames() {
		r := m.rec(name)
		s := SlotState{Slot: name, Unit: cfg.SpecialistSlots[name].Unit, Port: cfg.SpecialistSlots[name].Port, Occupant: r.occupant,
			State: r.state, Err: r.err, Attempts: r.attempts, NextRetry: r.nextRetry, LoadedAt: r.loadedAt, LastUse: r.lastUse, MemoryBytes: r.memory, Leases: m.leaseCountLocked(name)}
		if o, ok := occ[r.occupant]; ok {
			s.Policy, s.Functions = o.Policy, o.Functions
			// A kind whose application fixes its own port (Kokoro's 8880) listens there, not on the slot's port.
			if k, ok := KindByName(o.Kind); ok && k.FixedPort != 0 {
				s.Port = k.FixedPort
			}
		}
		out = append(out, s)
	}
	return out
}

func (m *Manager) occupantsByName(ctx context.Context) map[string]store.Occupant {
	list, err := m.d.Store.ListOccupants(ctx)
	if err != nil {
		m.logf("list occupants: %v", err)
	}
	out := map[string]store.Occupant{}
	for _, o := range list {
		out[o.Name] = o
	}
	return out
}

// Touch records a request served by slot (the on-demand idle clock).
func (m *Manager) Touch(slot string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.slots[slot]; ok {
		r.lastUse = m.d.Now()
	}
}

// slotOf finds the slot an occupant currently holds ("" if none). Caller holds m.mu.
func (m *Manager) slotOf(name string) string {
	for slot, r := range m.slots {
		if r.occupant == name && r.state != StateEmpty {
			return slot
		}
	}
	return ""
}

// reserved reports whether slot is the home of an enabled always-on occupant other than except. Caller passes the occupant table.
func reserved(occ map[string]store.Occupant, slot, except string) bool {
	for _, o := range occ {
		if o.Enabled && o.Policy == store.PolicyAlwaysOn && o.HomeSlot == slot && o.Name != except {
			return true
		}
	}
	return false
}

// Ensure makes the named occupant loaded and returns its slot state, loading it on demand. It blocks while a start is in flight
// (its own or another caller's for the same occupant). It returns *PoolFullError, *UnavailableError or ErrNotFound.
func (m *Manager) Ensure(ctx context.Context, name string) (SlotState, error) {
	for {
		o, err := m.d.Store.OccupantByName(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return SlotState{}, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		if err != nil {
			return SlotState{}, err
		}
		if !o.Enabled {
			return SlotState{}, &UnavailableError{Occupant: name, Reason: ReasonDisabled}
		}
		if o.Stopped {
			// A stop is a stop: only an explicit load clears it. (It survives daemon restarts, so the slot record may be empty here.)
			return SlotState{}, &UnavailableError{Occupant: name, Reason: ReasonStopped}
		}
		occ := m.occupantsByName(ctx)
		busy := m.busyMembers(ctx, occ) // network probes: done before taking the lock
		// The free-memory probe must NOT run under the lock: in the daemon it reads the engine's memory budget, which asks this manager for
		// ReservedBytes (it takes the same lock). Doing it inside the locked region self-deadlocked the whole pool (found live, v0.6.62). It is
		// only needed when this request will place a member, i.e. when the occupant is not already running.
		free := int64(-1)
		if m.d.FreeBytes != nil {
			m.mu.Lock()
			running := m.slotOf(name) != ""
			m.mu.Unlock()
			if !running {
				free = m.d.FreeBytes()
			}
		}

		m.mu.Lock()
		if slot := m.slotOf(name); slot != "" {
			r := m.slots[slot]
			switch r.state {
			case StateRunning:
				m.mu.Unlock()
				// The supervisor only looks every few seconds; a member that died in between must not be handed to a request (it would reach a dead
				// port and come back as a 502). Check the unit now: a dead always-on member is a fast, specific 503, a dead on-demand one is reloaded.
				if !m.unitActive(ctx, slot) {
					m.markDead(ctx, slot, name)
					continue
				}
				m.mu.Lock()
				if r := m.slots[slot]; r != nil && r.state == StateRunning && r.occupant == name {
					r.lastUse = m.d.Now()
				}
				m.mu.Unlock()
				return m.stateOf(ctx, slot), nil
			case StateStarting:
				done := r.done
				m.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return SlotState{}, ctx.Err()
				}
			case StateStopped:
				// The persisted stop flag was checked above and is clear, so this record is stale (the stop was superseded by an enable): start.
				r.occupant, r.state = "", StateEmpty
			case StateStopping:
				m.mu.Unlock()
				return SlotState{}, &UnavailableError{Occupant: name, Reason: "it is being unloaded; try again in a moment"}
			case StateFailed:
				if o.Policy == store.PolicyAlwaysOn {
					msg := r.err
					m.mu.Unlock()
					return SlotState{}, &UnavailableError{Occupant: name, Reason: "failed: " + msg}
				}
				// An on-demand member that failed gets a fresh attempt on the next request.
				r.occupant, r.state = "", StateEmpty
			}
		}
		slot, victim, perr := m.place(o, occ, busy)
		if perr != nil {
			m.mu.Unlock()
			return SlotState{}, perr
		}
		// An always-on member going into its own home slot needs no free memory: its declared memory is already outside the model budget, loaded or
		// not (ADR-0027), so checking it against that budget would count it twice. The supervisor starts it without this check too.
		if m.d.FreeBytes != nil && (o.Policy != store.PolicyAlwaysOn || slot != o.HomeSlot) {
			credit := int64(0)
			if victim != "" {
				credit = m.slots[slot].memory
			}
			need := o.MemoryBytes
			if need == 0 && o.Kind == KindCatalogConfig && m.d.Catalog != nil {
				need = m.d.Catalog.EstimateBytes(ctx, o.Params["config"]) // not registered with an estimate: use the catalog's
			}
			if free >= 0 && need > 0 && need > free+credit {
				m.mu.Unlock()
				return SlotState{}, &UnavailableError{Occupant: name, Reason: fmt.Sprintf("not enough GPU memory (needs about %d MiB, %d MiB free)", need>>20, (free+credit)>>20)}
			}
		}
		r := m.rec(slot)
		prev := r.occupant
		r.occupant, r.state, r.err, r.done = name, StateStarting, "", make(chan struct{})
		done := r.done
		m.mu.Unlock()

		if victim != "" {
			m.logf("evicting idle %s from %s to place %s", prev, slot, name)
			if err := m.stopUnit(ctx, slot); err != nil {
				m.finish(slot, name, o, err)
				close(done)
				return SlotState{}, &UnavailableError{Occupant: name, Reason: "could not free " + slot + ": " + err.Error()}
			}
		}
		lerr := m.start(ctx, slot, o)
		m.finish(slot, name, o, lerr)
		close(done)
		if lerr != nil {
			return SlotState{}, &UnavailableError{Occupant: name, Reason: "failed to start: " + lerr.Error()}
		}
		return m.stateOf(ctx, slot), nil
	}
}

func (m *Manager) stateOf(ctx context.Context, slot string) SlotState {
	for _, s := range m.Snapshot(ctx) {
		if s.Slot == slot {
			return s
		}
	}
	return SlotState{Slot: slot}
}

// place picks the slot for o (caller holds m.mu): its reserved home for always-on, otherwise a free unreserved slot, otherwise the
// least recently used idle on-demand member. victim is the evicted occupant's name ("" = the slot was free).
func (m *Manager) place(o store.Occupant, occ map[string]store.Occupant, busy map[string]bool) (slot, victim string, err error) {
	names := m.slotNames()
	free := func(s string) bool { r := m.rec(s); return r.state == StateEmpty }
	if o.Policy == store.PolicyAlwaysOn && o.HomeSlot != "" {
		if _, ok := m.d.Cfg().SpecialistSlots[o.HomeSlot]; !ok {
			return "", "", &UnavailableError{Occupant: o.Name, Reason: "its home slot " + o.HomeSlot + " does not exist"}
		}
		r := m.rec(o.HomeSlot)
		switch {
		case r.state == StateEmpty:
			return o.HomeSlot, "", nil
		case r.state == StateStarting:
			return "", "", &PoolFullError{Occupant: o.Name, Holders: m.holders(occ), RetryAfter: 5 * time.Second}
		default:
			// Its reserved slot is held by a guest (an on-demand member, or a stale failed record): the reservation wins.
			if g, ok := occ[r.occupant]; ok && g.Policy == store.PolicyAlwaysOn && g.Name != o.Name {
				return "", "", &UnavailableError{Occupant: o.Name, Reason: "its home slot " + o.HomeSlot + " is held by always-on " + g.Name}
			}
			return o.HomeSlot, r.occupant, nil
		}
	}
	for _, s := range names {
		if free(s) && !reserved(occ, s, o.Name) {
			return s, "", nil
		}
	}
	idle := m.d.IdleUnload()
	now := m.d.Now()
	var best string
	var bestUse time.Time
	for _, s := range names {
		r := m.rec(s)
		g, known := occ[r.occupant]
		// "Idle" is not enough for a member that serves long requests (an LLM generation): one with a request in flight is never evicted.
		// A speech request in flight (a lease) keeps an engine too: the daemon cannot see those requests, so it is told about them.
		evictable := r.state == StateRunning && known && g.Policy == store.PolicyOnDemand && now.Sub(r.lastUse) >= idle && !busy[s] && m.leaseCountLocked(s) == 0
		if r.state == StateFailed && !reserved(occ, s, o.Name) && (!known || g.Policy == store.PolicyOnDemand) {
			evictable = true
		}
		if evictable && (best == "" || r.lastUse.Before(bestUse)) {
			best, bestUse = s, r.lastUse
		}
	}
	if best != "" {
		return best, m.rec(best).occupant, nil
	}
	return "", "", &PoolFullError{Occupant: o.Name, Holders: m.holders(occ), RetryAfter: m.retryAfter(occ)}
}

func (m *Manager) holders(occ map[string]store.Occupant) []string {
	out := []string{}
	for _, s := range m.slotNames() {
		r := m.rec(s)
		if r.state == StateEmpty {
			out = append(out, s+" free (reserved)")
			continue
		}
		out = append(out, fmt.Sprintf("%s %s (%s, %s)", s, r.occupant, occ[r.occupant].Policy, r.state))
	}
	return out
}

// retryAfter is when the first on-demand member becomes evictable (0 when none ever will without operator action).
func (m *Manager) retryAfter(occ map[string]store.Occupant) time.Duration {
	idle, now := m.d.IdleUnload(), m.d.Now()
	best := time.Duration(0)
	for _, s := range m.slotNames() {
		r := m.rec(s)
		if g, ok := occ[r.occupant]; ok && g.Policy == store.PolicyOnDemand && r.state == StateRunning {
			if d := idle - now.Sub(r.lastUse); d > 0 && (best == 0 || d < best) {
				best = d
			}
		}
	}
	return best
}

func (m *Manager) envPath(slot string) string { return filepath.Join(m.d.EnvDir, slot+".env") }

// start writes the slot's env file and runs the unit until its health path answers. It does not touch m.slots.
func (m *Manager) start(ctx context.Context, slot string, o store.Occupant) error {
	cfg := m.d.Cfg()
	sc, ok := cfg.SpecialistSlots[slot]
	if !ok {
		return fmt.Errorf("slot %s is not a specialist slot", slot)
	}
	k, _ := KindByName(o.Kind)
	var env, argsFile string
	var spec LaunchSpec
	var row *store.SlotLaunch // the launch row of a row-launched engine, written once the unit is down
	if o.Kind == KindCatalogConfig {
		if m.d.Catalog == nil {
			return fmt.Errorf("the catalog is not wired, so %s cannot start", o.Name)
		}
		if err := Validate(o, m.d.ModelRoots()); err != nil {
			return err
		}
		var err error
		if spec, err = m.d.Catalog.LaunchSpec(ctx, o.Params["config"]); err != nil {
			return fmt.Errorf("resolve catalog config %q: %w", o.Params["config"], err)
		}
		if err := ValidateLaunchSpec(spec, m.d.ModelRoots()); err != nil {
			return fmt.Errorf("catalog config %q cannot run in the pool: %w", o.Params["config"], err)
		}
		env, argsFile = RenderCatalogEnv(o, slot, sc.Port, spec), RenderCatalogArgs(spec)
		if spec.Row {
			if m.d.SlotLaunches == nil {
				return fmt.Errorf("catalog config %q launches from a slot_launches row, and the pool has no row store on this server", o.Params["config"])
			}
			// Build the row now (pure: a refusal writes nothing); it is written below, after the unit is confirmed down.
			r, err := m.d.Catalog.LaunchRow(ctx, o.Params["config"], slot, sc.Port)
			if err != nil {
				return fmt.Errorf("catalog config %q cannot be launched in the pool: %w", o.Params["config"], err)
			}
			r.WrittenAt, r.WrittenBy = m.d.Now(), "specialist"
			row = &r
		}
	} else {
		var err error
		if env, err = RenderEnv(o, slot, sc.Port, m.d.ModelRoots()); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(m.d.EnvDir, 0o700); err != nil {
		return fmt.Errorf("env dir: %w", err)
	}
	if err := os.WriteFile(m.envPath(slot), []byte(env), 0o600); err != nil {
		return fmt.Errorf("write env: %w", err)
	}
	// The args file belongs to the occupant: write it for a catalog config, and remove a stale one a previous occupant of this slot left behind.
	argsPath := filepath.Join(m.d.EnvDir, slot+".args")
	if o.Kind == KindCatalogConfig {
		if err := os.WriteFile(argsPath, []byte(argsFile), 0o600); err != nil {
			return fmt.Errorf("write args: %w", err)
		}
	} else if err := os.Remove(argsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale args: %w", err)
	}
	// Start on an already-active unit is a silent no-op, so make sure it is down first.
	if err := m.stopUnit(ctx, slot); err != nil {
		return fmt.Errorf("stop before start: %w", err)
	}
	if row != nil {
		// Only while the unit is down may the row be replaced (stopUnit disarmed the previous one). If the write fails nothing is started: the unit would
		// have no row to launch from.
		gen, err := m.d.SlotLaunches.Put(ctx, *row)
		if err != nil {
			return fmt.Errorf("write the launch row of %s: %w", slot, err)
		}
		m.logf("slot launch row written: slot=%s config=%s generation=%d", slot, o.Params["config"], gen)
	}
	if err := m.d.Sys.Start(ctx, sc.Unit); err != nil {
		return fmt.Errorf("start %s: %w", sc.Unit, err)
	}
	port := sc.Port
	if k.FixedPort != 0 {
		port = k.FixedPort
	}
	deadline := m.d.Now().Add(k.HealthTimeout)
	var last error
	for {
		st, err := m.d.Sys.State(ctx, sc.Unit)
		if err == nil && (st.ActiveState == "failed" || st.ActiveState == "inactive") {
			_ = m.d.Sys.Stop(ctx, sc.Unit)
			return fmt.Errorf("unit %s exited (%s/%s) before it was healthy%s", sc.Unit, st.ActiveState, st.SubState, errSuffix(last))
		}
		if last = m.d.Health(ctx, port, k.HealthPath); last == nil {
			if k.VerifyContext && spec.Ctx > 0 {
				// The kernel can fail a contiguous allocation and silently lower the context: verify it, as every LLM load does.
				actual, err := m.d.NCtx(ctx, port)
				if err != nil {
					_ = m.d.Sys.Stop(ctx, sc.Unit)
					return fmt.Errorf("could not verify n_ctx on port %d: %w", port, err)
				}
				if float64(actual) < float64(spec.Ctx)*0.95 {
					_ = m.d.Sys.Stop(ctx, sc.Unit)
					return fmt.Errorf("n_ctx was silently reduced to %d (the config asks for %d)", actual, spec.Ctx)
				}
			}
			return nil
		}
		if !m.d.Now().Before(deadline) {
			_ = m.d.Sys.Stop(ctx, sc.Unit)
			return fmt.Errorf("not healthy within %s: %v", k.HealthTimeout, last)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.d.Sleep(ctx, m.poll())
	}
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " (last health: " + err.Error() + ")"
}

// disarmRow disarms the slot's launch row by compare-and-set on the generation it reads now, once the slot's unit is confirmed down (so the row can never
// be taken for a launch that is not wanted). No store, no row, or a row already disarmed is not an error. A store failure is logged, never swallowed: the
// next start replaces the row, and the pool units do not restart themselves.
func (m *Manager) disarmRow(ctx context.Context, slot string) {
	if m.d.SlotLaunches == nil {
		return
	}
	r, err := m.d.SlotLaunches.Get(ctx, slot)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		m.logf("ERROR: reading the launch row of %s to disarm it: %v", slot, err)
		return
	}
	if !r.Armed {
		return
	}
	if _, err := m.d.SlotLaunches.Disarm(ctx, slot, r.Generation); err != nil {
		m.logf("ERROR: the launch row of %s was not disarmed after its unit stopped: %v", slot, err)
	}
}

// stopUnit stops the slot's unit and waits until it is down (TimeoutStopSec=60 plus a margin).
func (m *Manager) stopUnit(ctx context.Context, slot string) error {
	sc, ok := m.d.Cfg().SpecialistSlots[slot]
	if !ok {
		return fmt.Errorf("slot %s is not a specialist slot", slot)
	}
	if err := m.d.Sys.Stop(ctx, sc.Unit); err != nil {
		return err
	}
	deadline := m.d.Now().Add(75 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopping %s: %w", sc.Unit, err) // a cancelled caller must not leave this spinning until the deadline
		}
		st, err := m.d.Sys.State(ctx, sc.Unit)
		if err == nil && (st.ActiveState == "inactive" || st.ActiveState == "failed") {
			m.disarmRow(ctx, slot)
			return nil
		}
		if !m.d.Now().Before(deadline) {
			return fmt.Errorf("%s still %s/%s after stop", sc.Unit, st.ActiveState, st.SubState)
		}
		m.d.Sleep(ctx, m.poll())
	}
}

// finish records the outcome of a start for slot.
func (m *Manager) finish(slot, name string, o store.Occupant, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rec(slot)
	now := m.d.Now()
	if err == nil {
		r.state, r.err, r.attempts, r.alerted = StateRunning, "", 0, false
		r.loadedAt, r.lastUse, r.memory = now, now, o.MemoryBytes
		m.logf("%s running in %s", name, slot)
		return
	}
	r.state, r.err = StateFailed, err.Error()
	r.attempts++
	r.nextRetry = now.Add(backoffFor(r.attempts))
	m.logf("%s failed in %s (attempt %d): %v", name, slot, r.attempts, err)
	if r.attempts >= failedAnomalyAttempts && !r.alerted && m.d.OnFailed != nil {
		r.alerted = true
		go m.d.OnFailed(slot, name, r.attempts, err)
	}
}

func backoffFor(attempts int) time.Duration {
	i := attempts - 1
	if i < 0 {
		i = 0
	}
	if i >= len(backoffSchedule) {
		i = len(backoffSchedule) - 1
	}
	return backoffSchedule[i]
}

// Load puts the named occupant into slot ("" = let the pool choose), the operator's explicit action. It clears an operator stop and the
// failure streak.
func (m *Manager) Load(ctx context.Context, name, slot string) (SlotState, error) {
	if slot == "" {
		return m.Ensure(ctx, name)
	}
	return m.loadSlot(ctx, name, slot, true)
}

// loadSlot is Load for a named slot. resetStreak is false for the supervisor's own retries, so consecutive failures keep counting and the
// backoff keeps growing.
func (m *Manager) loadSlot(ctx context.Context, name, slot string, resetStreak bool) (SlotState, error) {
	o, err := m.d.Store.OccupantByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return SlotState{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return SlotState{}, err
	}
	if _, ok := m.d.Cfg().SpecialistSlots[slot]; !ok {
		return SlotState{}, fmt.Errorf("specialist: no such slot %q", slot)
	}
	if o.Stopped && resetStreak {
		// An explicit start by the operator clears the stop. The supervisor's own retries (resetStreak false) never do.
		if err := m.d.Store.SetStopped(ctx, name, false); err != nil {
			return SlotState{}, fmt.Errorf("clear the stop on %s: %w", name, err)
		}
	}
	m.mu.Lock()
	r := m.rec(slot)
	if r.state == StateStarting {
		m.mu.Unlock()
		return SlotState{}, &UnavailableError{Occupant: name, Reason: slot + " is starting something else"}
	}
	if r.state == StateStopping {
		m.mu.Unlock()
		return SlotState{}, &UnavailableError{Occupant: name, Reason: slot + " is still being unloaded"}
	}
	if other := m.slotOf(name); other != "" && other != slot {
		m.mu.Unlock()
		return SlotState{}, &UnavailableError{Occupant: name, Reason: "already loaded in " + other}
	}
	if resetStreak || r.occupant != name {
		r.attempts, r.alerted = 0, false
	}
	r.occupant, r.state, r.err, r.done = name, StateStarting, "", make(chan struct{})
	done := r.done
	m.mu.Unlock()
	lerr := m.start(ctx, slot, o)
	m.finish(slot, name, o, lerr)
	close(done)
	if lerr != nil {
		return SlotState{}, &UnavailableError{Occupant: name, Reason: "failed to start: " + lerr.Error()}
	}
	return m.stateOf(ctx, slot), nil
}

// Unload stops whatever runs in slot. An always-on member the operator stops stays stopped (state "stopped") until loaded again.
//
// The slot reads StateStopping for the whole stop: stopping a member that is mid-generation can take up to stopUnit's 75 s, far longer than a
// client's HTTP timeout, so callers that must answer promptly run Unload in the background and let the operator watch the state. A second
// Unload of a slot already stopping is a no-op (nil): the first one finishes the job. A stop that fails puts the slot back as it was, with the
// reason in Err, so the failure is visible rather than lost. The lock is never held across the stop.
func (m *Manager) Unload(ctx context.Context, slot string) error {
	if _, ok := m.d.Cfg().SpecialistSlots[slot]; !ok {
		return fmt.Errorf("specialist: no such slot %q", slot)
	}
	m.mu.Lock()
	r := m.rec(slot)
	if r.state == StateStarting {
		m.mu.Unlock()
		return fmt.Errorf("specialist: %s is starting; wait for it to finish", slot)
	}
	if r.state == StateStopping {
		m.mu.Unlock()
		return nil
	}
	name, prev := r.occupant, r.state
	r.state = StateStopping
	m.mu.Unlock()
	// Read the occupant with its error (the list helper logs and swallows one): a failed read must not turn an always-on member into an on-demand one.
	alwaysOn := false
	if name != "" {
		g, err := m.d.Store.OccupantByName(ctx, name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			m.mu.Lock()
			if r := m.rec(slot); r.state == StateStopping {
				r.state, r.err = prev, err.Error()
			}
			m.mu.Unlock()
			return fmt.Errorf("read %s before stopping it: %w", name, err)
		}
		alwaysOn = err == nil && g.Policy == store.PolicyAlwaysOn
	}
	restore := func(cause error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r := m.rec(slot); r.state == StateStopping {
			r.state, r.err = prev, cause.Error()
		}
	}
	if alwaysOn {
		// Persist the stop BEFORE stopping: it must survive a daemon restart or a store error in the window, either of which used to start the
		// member again. If the stop then fails the flag is taken back, so a failed stop does not leave a running member flagged as stopped.
		if err := m.d.Store.SetStopped(ctx, name, true); err != nil {
			err = fmt.Errorf("record the stop of %s: %w", name, err)
			restore(err)
			return err
		}
	}
	if err := m.stopUnit(ctx, slot); err != nil {
		if alwaysOn {
			if rerr := m.d.Store.SetStopped(context.WithoutCancel(ctx), name, false); rerr != nil {
				err = fmt.Errorf("%w (and the stop flag could not be taken back: %v)", err, rerr)
			}
		}
		restore(err)
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r = m.rec(slot)
	if alwaysOn {
		r.state, r.err = StateStopped, ""
		return nil
	}
	*r = slotRec{state: StateEmpty}
	return nil
}

// ReservedBytes is the memory still owed to always-on members that are meant to be running but are not (starting, recovering from a failure,
// or not started yet), so an LLM load cannot take the memory a restarting specialist needs. Members the operator stopped, disabled members and
// members already running (their memory is already in use) are not counted.
func (m *Manager) ReservedBytes() int64 {
	ctx := context.Background()
	occ := m.occupantsByName(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, o := range occ {
		if !o.Enabled || o.Stopped || o.Policy != store.PolicyAlwaysOn || o.HomeSlot == "" {
			continue
		}
		if _, ok := m.d.Cfg().SpecialistSlots[o.HomeSlot]; !ok {
			continue
		}
		if r := m.rec(o.HomeSlot); r.state == StateRunning && r.occupant == o.Name {
			continue
		}
		total += o.MemoryBytes
	}
	return total
}

// DeclaredBytes is the declared peak footprint of every enabled, always-on member that has a home slot, running or not. It is the specialists'
// share of the host-memory partition (internal/memledger): reserved up front so a model load can never take the memory a specialist will need,
// and so a restart does not change what the models are allowed. Members the operator stopped or disabled are not counted.
func (m *Manager) DeclaredBytes() int64 {
	occ := m.occupantsByName(context.Background())
	var total int64
	for _, o := range occ {
		if !o.Enabled || o.Stopped || o.Policy != store.PolicyAlwaysOn || o.HomeSlot == "" {
			continue
		}
		if _, ok := m.d.Cfg().SpecialistSlots[o.HomeSlot]; !ok {
			continue
		}
		total += o.MemoryBytes
	}
	return total
}

// AlwaysOnProblems lists the always-on members that should be running and are not (starting too long, failed, or never started), for health
// checks. A member the operator stopped is not a problem.
func (m *Manager) AlwaysOnProblems(ctx context.Context) []SlotState {
	var out []SlotState
	for _, s := range m.Snapshot(ctx) {
		if s.Occupant == "" || s.Policy != store.PolicyAlwaysOn || s.State == StateRunning || s.State == StateStopped || s.State == StateEmpty {
			continue
		}
		out = append(out, s)
	}
	return out
}

// busyMembers asks every running catalog-config member (the only kind that exposes it) whether a request is in flight, keyed by slot.
func (m *Manager) busyMembers(ctx context.Context, occ map[string]store.Occupant) map[string]bool {
	type probe struct {
		slot string
		port int
	}
	var probes []probe
	cfg := m.d.Cfg()
	m.mu.Lock()
	for _, name := range m.slotNames() {
		r := m.rec(name)
		if o, ok := occ[r.occupant]; ok && o.Kind == KindCatalogConfig && r.state == StateRunning {
			probes = append(probes, probe{name, cfg.SpecialistSlots[name].Port})
		}
	}
	m.mu.Unlock()
	if len(probes) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, p := range probes {
		out[p.slot] = m.d.Busy(ctx, p.port)
	}
	return out
}
