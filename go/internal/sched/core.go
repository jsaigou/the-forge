// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/store"
)

// Engine is the slice of engine behavior the scheduler needs — a seam
// defined in this package so tests fake it cheaply. *engine.Manager
// satisfies it as-is (compile-checked below): placement/fit decisions go
// through the engine's FitPlan (live memory probe at the decision point —
// the scheduler never caches budget numbers), slot occupancy through
// SlotStates (the engine's authoritative in-memory reconciliation,
// including the never-clear-while-deactivating rule), and the actual GPU
// operations through Load/Unload.
type Engine interface {
	Slots() []string
	SlotStates(units map[string]collector.UnitState) map[string]collector.SlotAssignment
	FitPlan(mode string) (engine.Plan, error)
	Load(ctx context.Context, mode, slot string) engine.Result
	Unload(ctx context.Context, slot string) engine.Result
}

var _ Engine = (*engine.Manager)(nil)

// ComfyUISeam is what the scheduler needs to treat the deployment's ComfyUI
// service as an evictable memory holder (S1, feedback F3: ComfyUI's GTT
// footprint counts against the same budget, and an idle ComfyUI should be
// stopped to make room just like an idle slot). The engine's FitPlan
// proposes it (Plan.EvictComfyUI); this seam gates and executes. Nil = the
// deployment has no (configured) ComfyUI — the plan's proposal then refuses
// with a clear reason instead of acting.
type ComfyUISeam struct {
	// Unit returns the ComfyUI systemd unit name (smith.comfyui.unit).
	Unit func() string

	// Idle reports whether ComfyUI has no work queued or running, with a
	// human reason when it does. Fail-closed: a failed probe counts as not
	// idle.
	Idle func(ctx context.Context) (ok bool, reason string)

	// Stop gracefully stops the unit.
	Stop func(ctx context.Context) error
}

// Footprints is optionally implemented by the Engine seam to expose
// per-slot resident-footprint estimates for smallest-footprint-first
// eviction ordering (V4's engine._slot_footprint_mb). engine.Manager keeps
// this unexported today (slotFootprintMB) — exporting a one-line wrapper is
// a documented integrator request; until then the scheduler falls back to
// the ordering implied by FitPlan's Evict list (already smallest-first)
// plus engine slot order.
type Footprints interface {
	SlotFootprintMB(slot string) float64
}

// Ticket statuses (Contract 1 QueueTicket.status).
const (
	StatusQueued  = "queued"
	StatusLoading = "loading"
	StatusLoaded  = "loaded"
	StatusFailed  = "failed"
)

// HumanIdentity is the requested_by/created_by value for dashboard-driven
// actions. Humans always win reservation permission checks (V4 semantics:
// the UI flow implies deliberate intent).
const HumanIdentity = "human"

// settingsKey is the store.Settings key for the runtime tunables
// (Contract 3 settings key list: `scheduler.config`).
const settingsKey = "scheduler.config"

// Deps wires the Core scheduler. Engine and Source are required; the store
// surfaces are optional (nil = in-memory only, no restart recovery — fine
// for tests, never for a cutover build).
type Deps struct {
	// Engine is the placement/lifecycle seam (see the Engine interface).
	Engine Engine

	// Source supplies collector snapshots: unit states for occupancy
	// reconciliation, per-slot LastActivity for idle-aware eviction, and
	// the memory numbers Status() reports (Status is a read path and only
	// reads snapshots — design decision 2; *decision* paths use
	// Engine.FitPlan, which probes live).
	Source collector.Source

	// Sched persists reservations (authoritative recovery data), queue
	// tickets, and slot occupancy for restart recovery. In-memory state is
	// authoritative at runtime (Contract 2).
	Sched store.Sched

	// Settings persists the runtime tunables under `scheduler.config`.
	Settings store.Settings

	// RouteSync, when set, is called after every successful scheduler
	// load/unload/evict with (slot, mode) — mode "" on unload. This is the
	// V5 home for V4's _sync_router_route (a0 model-label sync on load),
	// which was deliberately not ported into the engine; Phase 9 wires it
	// to the router catalog. Called outside the scheduler mutex.
	RouteSync func(slot, mode string)

	// MaintenanceBlocked is an advisory pre-check (maintenance.Gate.Blocked
	// in production), consulted once at the top of EnsureLoaded — purely
	// for error quality. The real enforcement is the wrapped Engine value
	// itself (every Load/Unload call this package makes already refuses
	// during a window); without this check, a request that needs an
	// eviction first would instead sit polling for the full DefaultTimeout
	// (attemptLoad's "still deactivating, keep polling" branch can't tell
	// "blocked by maintenance" apart from "unit still shutting down" — see
	// go/internal/maintenance's doc comment). nil = no advisory check, the
	// wrapped engine is still the real gate.
	MaintenanceBlocked func(ctx context.Context) (bool, string)

	// Fallback seeds Config when the settings store has no value yet —
	// Phase 9 passes the config file's [scheduler] defaults. Zero value
	// means DefaultConfig().
	Fallback Config

	// ComfyUI, when set, lets placement stop the deployment's ComfyUI
	// service when FitPlan says a load needs its memory (see ComfyUISeam).
	// Gating: scheduler config opt-out, comfyui-scope reservations, and the
	// queue-idle probe all run before a stop is attempted.
	ComfyUI *ComfyUISeam

	// Now, PollInterval, DefaultTimeout, Logf are test/tuning knobs.
	// Defaults: time.Now, 500ms (V4's poll cadence), 150s (V4's
	// ensure_loaded timeout), and a no-op logger.
	Now            func() time.Time
	PollInterval   time.Duration
	DefaultTimeout time.Duration
	Logf           func(format string, args ...any)
}

// Core is the in-process scheduler (Phase 5). One mutex guards all
// scheduler state — queue, reservations, config, and the load-in-flight
// flag. There is no slots.json, no queue.json, and no file locking:
// dashboard, a0, and MCP all share this one instance (design decision 3).
//
// The actual GPU operation (evict + load) runs *outside* the mutex, gated
// by the loadBusy flag, so a minutes-long model load never blocks Status()
// or reservation CRUD.
type Core struct {
	d Deps

	mu           sync.Mutex // THE one scheduler mutex
	cfg          Config
	queue        []*ticket
	reservations []Reservation
	loadBusy     bool
}

var _ Scheduler = (*Core)(nil)

// New builds a Core and performs restart recovery: config from the
// settings store, reservations from the sched store, and a purge of any
// leftover queue rows (queued callers are blocked HTTP requests — they do
// not survive a process restart, so their tickets are dead by definition).
func New(deps Deps) (*Core, error) {
	if deps.Engine == nil {
		return nil, errors.New("sched: Deps.Engine is required")
	}
	if deps.Source == nil {
		return nil, errors.New("sched: Deps.Source is required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.PollInterval <= 0 {
		deps.PollInterval = 500 * time.Millisecond
	}
	if deps.DefaultTimeout <= 0 {
		deps.DefaultTimeout = 150 * time.Second
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}

	c := &Core{d: deps}
	c.cfg = deps.Fallback
	if c.cfg == (Config{}) {
		c.cfg = DefaultConfig()
	}
	if err := validateConfig(c.cfg); err != nil {
		return nil, fmt.Errorf("sched: invalid fallback config: %w", err)
	}

	ctx := context.Background()
	if deps.Settings != nil {
		raw, err := deps.Settings.Get(ctx, settingsKey)
		switch {
		case err == nil:
			var cfg Config
			if jsonErr := json.Unmarshal(raw, &cfg); jsonErr != nil {
				return nil, fmt.Errorf("sched: corrupt %s setting: %w", settingsKey, jsonErr)
			}
			if valErr := validateConfig(cfg); valErr != nil {
				return nil, fmt.Errorf("sched: stored %s out of bounds: %w", settingsKey, valErr)
			}
			c.cfg = cfg
		case errors.Is(err, store.ErrNotFound):
			// first run — fallback stands
		default:
			return nil, fmt.Errorf("sched: load config: %w", err)
		}
	}

	if deps.Sched != nil {
		rows, err := deps.Sched.Reservations(ctx)
		if err != nil {
			return nil, fmt.Errorf("sched: load reservations: %w", err)
		}
		for _, row := range rows {
			c.reservations = append(c.reservations, reservationFromRow(row))
		}

		stale, err := deps.Sched.Queue(ctx)
		if err != nil {
			return nil, fmt.Errorf("sched: load queue: %w", err)
		}
		for _, t := range stale {
			if err := deps.Sched.DeleteTicket(ctx, t.TicketID); err != nil {
				return nil, fmt.Errorf("sched: purge stale ticket %s: %w", t.TicketID, err)
			}
		}
		if len(stale) > 0 {
			deps.Logf("sched: purged %d stale queue tickets from previous run", len(stale))
		}
	}
	return c, nil
}

// ── Scheduler interface ──────────────────────────────────────────────────────

// EnsureLoaded blocks (up to the ctx deadline, defaulting to
// Deps.DefaultTimeout) until req.Model is loaded somewhere — or on
// req.TargetSlot specifically when pinned (a model loaded elsewhere does
// not count for a pinned caller; a0's backend records have fixed
// port/slot mappings). Idempotent: an already-loaded model returns
// immediately without touching the queue.
//
// This is the single entry point A0's on-demand path, MCP's ensure_loaded
// tool, and the dashboard share — one scheduler, many callers, contending
// through the exact same code path (V4 parity).
func (c *Core) EnsureLoaded(ctx context.Context, req EnsureRequest) (Ticket, error) {
	if req.Model == "" {
		return Ticket{Status: StatusFailed}, errors.New("sched: model is required")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.d.DefaultTimeout)
		defer cancel()
	}

	if slot := c.findLoaded(req.Model, req.TargetSlot); slot != "" {
		return Ticket{
			TicketID:    newTicketID(),
			Model:       req.Model,
			RequestedBy: req.RequestedBy,
			TargetSlot:  slot,
			Status:      StatusLoaded,
			SmallJob:    req.SmallJob,
			EnqueuedAt:  c.d.Now(),
		}, nil
	}

	// Fail fast with a clear reason rather than queuing a request that can
	// only ever time out — see the MaintenanceBlocked doc comment above.
	// Already-loaded models (the branch just above) are unaffected: a
	// maintenance window blocks new loads/evictions, never serving.
	if c.d.MaintenanceBlocked != nil {
		if blocked, msg := c.d.MaintenanceBlocked(ctx); blocked {
			return Ticket{TicketID: newTicketID(), Model: req.Model, RequestedBy: req.RequestedBy, Status: StatusFailed}, errors.New("sched: " + msg)
		}
	}

	t := &ticket{
		Ticket: Ticket{
			TicketID:    newTicketID(),
			Model:       req.Model,
			RequestedBy: req.RequestedBy,
			TargetSlot:  req.TargetSlot,
			Status:      StatusQueued,
			SmallJob:    req.SmallJob,
			EnqueuedAt:  c.d.Now(),
		},
		priority: req.Priority,
	}
	c.mu.Lock()
	c.enqueueLocked(t)
	snapshot := t.Ticket
	c.mu.Unlock()
	c.persistTicket(snapshot, t.priority)
	defer c.dequeue(t.TicketID)

	// lastRefusal carries the most recent retryable blocker ("a3 must be
	// evicted for memory but is not idle", …) so a poll that runs out of
	// deadline reports WHY it never proceeded instead of a bare timeout —
	// the 2026-08-22 incident class, where silent refusals burned 320s and
	// surfaced nothing actionable.
	lastRefusal := ""
	deadline := func() time.Time {
		d, _ := ctx.Deadline()
		if d.IsZero() {
			return c.d.Now().Add(c.d.DefaultTimeout)
		}
		return d
	}
	for {
		if ctx.Err() != nil {
			c.mu.Lock()
			t.Status = StatusFailed
			out := t.Ticket
			c.mu.Unlock()
			base := fmt.Sprintf("sched: timed out waiting for %s to load", req.Model)
			if lastRefusal != "" {
				base += " (last blocker: " + lastRefusal + ")"
			}
			return out, fmt.Errorf("%s: %w", base, ctx.Err())
		}

		// Someone else may have loaded this model while we were queued —
		// two callers asking for the same model coalesce onto whichever
		// one wins (V4 parity).
		if slot := c.findLoaded(req.Model, req.TargetSlot); slot != "" {
			c.mu.Lock()
			t.Status = StatusLoaded
			t.TargetSlot = slot
			out := t.Ticket
			c.mu.Unlock()
			return out, nil
		}

		c.mu.Lock()
		myTurn := len(c.queue) > 0 && c.queue[0] == t && !c.loadBusy
		var cfg Config
		var reservations []Reservation
		if myTurn {
			c.loadBusy = true
			t.Status = StatusLoading
			cfg = c.cfg
			reservations = append([]Reservation(nil), c.reservations...)
		}
		snapshot = t.Ticket
		c.mu.Unlock()

		if myTurn {
			c.persistTicket(snapshot, t.priority)
			// horizon is how long this request is still willing to wait —
			// the structural-vs-transient boundary for placement refusals.
			// A candidate that becomes eligible within the horizon stays
			// retryable (idle countdown nearly done); one that cannot become
			// eligible before the deadline fails NOW with its reason instead
			// of silently burning the remaining budget (S1, feedback F3).
			horizon := time.Until(deadline())
			outcome := c.attemptLoad(ctx, req, cfg, reservations, horizon)
			c.mu.Lock()
			c.loadBusy = false
			if outcome.terminal {
				if outcome.success {
					t.Status = StatusLoaded
					t.TargetSlot = outcome.slot
				} else {
					t.Status = StatusFailed
				}
			} else {
				t.Status = StatusQueued
			}
			snapshot = t.Ticket
			c.mu.Unlock()

			if outcome.terminal {
				if outcome.success {
					return snapshot, nil
				}
				return snapshot, fmt.Errorf("sched: load %s: %s", req.Model, outcome.message)
			}
			// Retryable: nothing evictable *yet* — an idle threshold
			// crossing, a reservation window opening, or a busy slot
			// draining can still change conditions before our deadline
			// (V4 parity: sleep and re-poll, never give up early).
			lastRefusal = outcome.message
			c.persistTicket(snapshot, t.priority)
		}

		select {
		case <-ctx.Done():
			// handled at loop top
		case <-time.After(c.d.PollInterval):
		}
	}
}

// loadOutcome is one placement+load attempt's result. terminal=false means
// nothing was evictable right now and the caller should keep polling; a
// real engine load failure is terminal (retrying the identical load
// without anything changing won't help — V4 semantics).
type loadOutcome struct {
	terminal bool
	success  bool
	slot     string
	message  string
}

// attemptLoad runs one placement + eviction + load cycle. Called with the
// loadBusy token held and the scheduler mutex released. horizon is the
// caller's remaining patience: refusals whose candidate cannot become
// eligible within it are terminal-with-reason, not retryable.
func (c *Core) attemptLoad(ctx context.Context, req EnsureRequest, cfg Config, reservations []Reservation, horizon time.Duration) loadOutcome {
	// Re-check under the load token: another caller could have loaded the
	// model between our queue-head check and winning the token.
	if slot := c.findLoaded(req.Model, req.TargetSlot); slot != "" {
		return loadOutcome{terminal: true, success: true, slot: slot}
	}

	pl := c.place(ctx, req.Model, req.TargetSlot, cfg, reservations, horizon)
	if pl.slot == "" && !pl.evictComfy {
		return loadOutcome{terminal: pl.terminal, message: pl.message}
	}

	for _, evictSlot := range pl.evict {
		res := c.d.Engine.Unload(ctx, evictSlot)
		if !res.Success {
			// A still-deactivating unit is a transient condition — the
			// slot is not free, nothing was lost; keep polling.
			return loadOutcome{message: fmt.Sprintf("evict %s: %s", evictSlot, res.Message)}
		}
		c.persistSlot(evictSlot, "")
		c.routeSync(evictSlot, "")
	}

	if pl.evictComfy {
		if err := c.stopComfyForMemory(ctx, req.Model); err != nil {
			return loadOutcome{message: err.Error()}
		}
	}

	res := c.d.Engine.Load(ctx, req.Model, pl.slot)
	if !res.Success {
		return loadOutcome{terminal: true, message: res.Message}
	}
	c.persistSlot(pl.slot, req.Model)
	c.routeSync(pl.slot, req.Model)
	c.d.Logf("sched: loaded %s into %s for %s", req.Model, pl.slot, req.RequestedBy)
	return loadOutcome{terminal: true, success: true, slot: pl.slot}
}

// stopComfyForMemory stops the ComfyUI service to release its GPU memory
// for a waiting load. place() has already gated this on config opt-out,
// comfyui reservations, and the queue-idle probe; this is execution only.
// Transient failures are retryable (non-terminal outcome).
func (c *Core) stopComfyForMemory(ctx context.Context, model string) error {
	seam := c.d.ComfyUI
	if err := seam.Stop(ctx); err != nil {
		return fmt.Errorf("stop ComfyUI (%s) to free memory for %s: %v", seam.Unit(), model, err)
	}
	c.d.Logf("sched: stopped ComfyUI (%s) to free memory for %s", seam.Unit(), model)
	return nil
}

// Unload evicts one slot ("all" for every slot) through the scheduler so
// route sync and persistence stay consistent. Like V4's scheduler.unload
// it performs no reservation/idle checks — an explicit unload is operator
// (or agent) intent, and the engine is the arbiter of whether the unit
// actually stops.
func (c *Core) Unload(ctx context.Context, slot, requestedBy string) error {
	if slot == "" {
		return errors.New("sched: slot is required")
	}
	res := c.d.Engine.Unload(ctx, slot)
	if !res.Success {
		return fmt.Errorf("sched: unload %s: %s", slot, res.Message)
	}
	if slot == "all" {
		for _, s := range c.d.Engine.Slots() {
			c.persistSlot(s, "")
			c.routeSync(s, "")
		}
	} else {
		c.persistSlot(slot, "")
		c.routeSync(slot, "")
	}
	c.d.Logf("sched: unloaded %s (requested by %s)", slot, requestedBy)
	return nil
}

// Status mirrors GET /api/v1/scheduler/status. It is a pure read path:
// occupancy from the engine's in-memory reconciliation over the latest
// snapshot's unit states, idle/budget numbers from the snapshot itself
// (design decision 2 — handlers read snapshots; only *decision* paths
// probe live via FitPlan). This is the V5 form of the Phase 0 Fix A rule.
func (c *Core) Status() Status {
	snap := c.d.Source.Current()
	occ := c.d.Engine.SlotStates(snap.Units)
	// Idle is measured against the snapshot's own capture time, not the
	// live clock: this is a pure read path over the latest snapshot (see
	// doc comment above), and using the live clock here let a stalled or
	// delayed collector cycle manufacture a false idle window on top of
	// the real one — the live clock keeps advancing even while the
	// snapshot itself hasn't gotten any fresher. Falls back to the live
	// clock only if the snapshot never got a capture time.
	now := snap.TakenAt
	if now.IsZero() {
		now = c.d.Now()
	}

	st := Status{
		Slots:           map[string]string{},
		SlotLabels:      map[string]string{},
		IdleSeconds:     map[string]*float64{},
		SlotMemoryBytes: map[string]int64{},
		UnitMemoryBytes: map[string]int64{},
	}
	for _, name := range c.d.Engine.Slots() {
		st.Slots[name] = occ[name].Mode
		label := name
		if s, ok := snap.Slots[name]; ok && s.Label != "" {
			label = s.Label
		}
		st.SlotLabels[name] = label
		st.IdleSeconds[name] = nil
		if occ[name].Mode != "" {
			if s, ok := snap.Slots[name]; ok && !s.LastActivity.IsZero() {
				idle := now.Sub(s.LastActivity).Seconds()
				if idle < 0 {
					idle = 0
				}
				st.IdleSeconds[name] = &idle
			}
			if s, ok := snap.Slots[name]; ok {
				st.SlotMemoryBytes[name] = s.MemoryBytes
			}
		}
	}
	for unit, u := range snap.Units {
		if u.GPUBytes > 0 {
			st.UnitMemoryBytes[unit] = u.GPUBytes
		}
	}

	if snap.Metrics.GTTTotalBytes != nil {
		st.MemoryBudget.TotalBytes = *snap.Metrics.GTTTotalBytes
	}
	if snap.Metrics.InferenceRSSBytes != nil {
		st.MemoryBudget.UsedBytes = *snap.Metrics.InferenceRSSBytes
	}
	if free := st.MemoryBudget.TotalBytes - st.MemoryBudget.UsedBytes; free > 0 {
		st.MemoryBudget.FreeBytes = free
	}

	c.mu.Lock()
	for _, t := range c.queue {
		st.Queue = append(st.Queue, t.Ticket)
	}
	c.mu.Unlock()
	return st
}

// Config returns the current runtime tunables.
func (c *Core) Config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// SetConfig validates against the frozen bounds (Contract 1 /
// validators.SchedulerConfigRequest), persists to the settings store, then
// swaps the in-memory config.
func (c *Core) SetConfig(cfg Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if c.d.Settings != nil {
		raw, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("sched: marshal config: %w", err)
		}
		if err := c.d.Settings.Set(context.Background(), settingsKey, raw); err != nil {
			return fmt.Errorf("sched: persist config: %w", err)
		}
	}
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// validateConfig enforces the frozen validator bounds: idle_unload_s
// 30–3600, small_job_token_threshold ≥ 1, priority_jump_cap ≥ 0,
// reservation_soon_min 1–120.
func validateConfig(cfg Config) error {
	if cfg.IdleUnloadS < 30 || cfg.IdleUnloadS > 3600 {
		return fmt.Errorf("sched: idle_unload_s must be 30..3600, got %d", cfg.IdleUnloadS)
	}
	if cfg.SmallJobTokenThreshold < 1 {
		return fmt.Errorf("sched: small_job_token_threshold must be >= 1, got %d", cfg.SmallJobTokenThreshold)
	}
	if cfg.PriorityJumpCap < 0 {
		return fmt.Errorf("sched: priority_jump_cap must be >= 0, got %d", cfg.PriorityJumpCap)
	}
	if cfg.ReservationSoonMin < 1 || cfg.ReservationSoonMin > 120 {
		return fmt.Errorf("sched: reservation_soon_min must be 1..120, got %d", cfg.ReservationSoonMin)
	}
	return nil
}

// SmallJobFromHint reports whether a request with the given input-token
// hint qualifies for the priority queue-jump under cfg. V4 derived this
// inside the scheduler from token_hint; the frozen V5 EnsureRequest
// carries the resolved SmallJob bool instead, so callers (httpapi, MCP,
// a0) resolve it with this helper. V4 parity: an absent hint is 0, and 0
// is small — a caller that declares nothing gets small-job treatment.
func SmallJobFromHint(cfg Config, tokenHint int) bool {
	return tokenHint <= cfg.SmallJobTokenThreshold
}

func (c *Core) routeSync(slot, mode string) {
	if c.d.RouteSync != nil {
		c.d.RouteSync(slot, mode)
	}
}

// persistSlot / persistTicket / persistDeleteTicket are best-effort
// restart-recovery writes: the in-memory state is authoritative at runtime
// (Contract 2), so a persistence hiccup is logged, never allowed to fail a
// load that already succeeded on the GPU.
func (c *Core) persistSlot(slot, mode string) {
	if c.d.Sched == nil {
		return
	}
	loadedAt := time.Time{}
	if mode != "" {
		loadedAt = c.d.Now()
	}
	if err := c.d.Sched.SaveSlot(context.Background(), slot, mode, loadedAt); err != nil {
		c.d.Logf("sched: persist slot %s: %v", slot, err)
	}
}

func (c *Core) persistTicket(t Ticket, priority int) {
	if c.d.Sched == nil {
		return
	}
	row := store.QueueRow{
		TicketID:    t.TicketID,
		Model:       t.Model,
		RequestedBy: t.RequestedBy,
		TargetSlot:  t.TargetSlot,
		Status:      t.Status,
		SmallJob:    t.SmallJob,
		Priority:    priority,
		EnqueuedAt:  t.EnqueuedAt,
		UpdatedAt:   c.d.Now(),
	}
	if err := c.d.Sched.SaveTicket(context.Background(), row); err != nil {
		c.d.Logf("sched: persist ticket %s: %v", t.TicketID, err)
	}
}

func (c *Core) persistDeleteTicket(id string) {
	if c.d.Sched == nil {
		return
	}
	if err := c.d.Sched.DeleteTicket(context.Background(), id); err != nil {
		c.d.Logf("sched: delete ticket %s: %v", id, err)
	}
}

func newTicketID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is a broken host; fall back to time-based
		// uniqueness rather than panicking a request path.
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
