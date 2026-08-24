// SPDX-License-Identifier: Apache-2.0

// Package maintenance implements the system-wide quiet-host guarantee a
// smith-executed repair needs (docs/v5-smith.md's autonomous-remediation
// plan, Sprint 1): while a maintenance window is active, no model may load
// or unload, no unit restarts, and the scheduler's own idle eviction stands
// down. Enforcement is a decorator over engine.Engine (engine_wrap.go), not
// a change to the frozen Contract 2 interface — every caller (a0 router,
// MCP, httpapi, the profiler, smith itself) is covered by construction,
// since they all share the one wrapped engine value main.go hands out.
//
// A maintenance window must never be able to get stuck active: it carries a
// hard TTL, exits itself on expiry, and a daemon restart force-exits any
// window it finds still marked active on boot (ReconcileOnBoot) — Sprint 1
// has no resumable procedure yet, so a persisted "active" state surviving a
// restart is by definition orphaned; Sprint 2's procedure runner replaces
// this with real resume-aware reconciliation.
package maintenance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/store"
)

// SettingsKey is the store.Settings key the current window is persisted
// under, so it survives a daemon restart long enough for ReconcileOnBoot to
// find and force-exit it.
const SettingsKey = "maintenance.state"

// EventChanged is the SSE event published on every state transition
// (entered, exited, expired).
const EventChanged = "maintenance:changed"

// DefaultMaxDuration bounds how long a caller may request — a request for
// longer is clamped, never rejected outright, so a bad estimate degrades to
// "too conservative" rather than "can't start". 4h comfortably covers the
// slowest planned procedure (build-refresh's dual-backend build+test).
const DefaultMaxDuration = 4 * time.Hour

// State is the current window, or the zero value when inactive. JSON tags
// match the wire shape GET /api/v1/maintenance returns.
type State struct {
	Active           bool     `json:"active"`
	LeaseID          string   `json:"lease_id,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	EnteredBy        string   `json:"entered_by,omitempty"`
	AffectedSlots    []string `json:"affected_slots,omitempty"`
	AffectedServices []string `json:"affected_services,omitempty"`
	EnteredAt        int64    `json:"entered_at,omitempty"`
	ExpiresAt        int64    `json:"expires_at,omitempty"`
}

// EnterRequest is what a caller (the maintenance API handler today; a
// procedure's own Impact in Sprint 2) asks for.
type EnterRequest struct {
	Reason           string
	EnteredBy        string
	AffectedSlots    []string
	AffectedServices []string
	Duration         time.Duration // clamped to (0, DefaultMaxDuration]
}

// ErrAlreadyActive is returned by Enter when a different window is already
// open — only one maintenance window exists at a time in v1.
var ErrAlreadyActive = fmt.Errorf("maintenance: a window is already active")

// ErrNotActive is returned by Exit when there is nothing to exit.
var ErrNotActive = fmt.Errorf("maintenance: no window is active")

// Gate is the shared, process-wide holder. Reads (Status, blocked) never
// touch the store — they read the in-memory copy behind mu, since
// engine_wrap.go calls blocked on every mutating engine call and this must
// stay cheap. Every mutation persists before returning so a crash between
// "decided" and "acted" always leans toward "still recorded as active",
// never toward "silently forgot a real window" — ReconcileOnBoot is the
// backstop for the opposite failure mode (stuck active).
type Gate struct {
	mu       sync.Mutex
	state    State
	settings store.Settings // nil-tolerant: in-memory only, no restart recovery
	publish  bus.Publisher  // nil-tolerant
	now      func() time.Time
	logf     func(format string, args ...any)

	stopTicker context.CancelFunc
}

// New builds a Gate and loads any persisted state (does NOT reconcile it —
// call ReconcileOnBoot once the rest of the daemon is wired, so the audit
// write and SSE publish land after the bus exists).
func New(settings store.Settings, publish bus.Publisher, now func() time.Time, logf func(format string, args ...any)) *Gate {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	g := &Gate{settings: settings, publish: publish, now: now, logf: logf}
	g.load()
	return g
}

// load hydrates in-memory state from the settings store. Best-effort: an
// unreadable/corrupt value is treated as "no window" rather than blocking
// startup.
func (g *Gate) load() {
	if g.settings == nil {
		return
	}
	raw, err := g.settings.Get(context.Background(), SettingsKey)
	if err != nil || len(raw) == 0 {
		return
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		g.logf("maintenance: discarding unreadable persisted state: %v", err)
		return
	}
	g.state = s
}

// persist writes the current state. Errors are logged, not returned — a
// failed write must not itself block entering/exiting a window; it does
// mean a restart during that window won't be force-exited by
// ReconcileOnBoot until the TTL catches it instead.
func (g *Gate) persist() {
	if g.settings == nil {
		return
	}
	raw, err := json.Marshal(g.state)
	if err != nil {
		g.logf("maintenance: marshal state: %v", err)
		return
	}
	if err := g.settings.Set(context.Background(), SettingsKey, raw); err != nil {
		g.logf("maintenance: persist state: %v", err)
	}
}

func (g *Gate) publishChanged() {
	if g.publish == nil {
		return
	}
	g.publish.Publish(EventChanged, g.state)
}

// Status returns a copy of the current state, lazily expiring it first —
// so a caller that only ever reads (the Console banner poll, the sched
// advisory check) still sees a window end on time even with no ticker
// running (belt; StartTicker below is the suspenders).
func (g *Gate) Status() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	return g.state
}

// expireLocked force-exits an active window past its ExpiresAt. Caller
// must hold mu.
func (g *Gate) expireLocked() {
	if !g.state.Active || g.state.ExpiresAt == 0 || g.now().Unix() < g.state.ExpiresAt {
		return
	}
	g.logf("maintenance: window %q expired (TTL), auto-exiting", g.state.LeaseID)
	g.state = State{}
	g.persist()
	g.publishChanged()
}

// Enter opens a new window. Fails with ErrAlreadyActive if one is already
// open (the operator/procedure must Exit or wait it out first — no
// silent extend-in-place, so a stale "still active" never masks a second,
// unrelated repair starting).
func (g *Gate) Enter(req EnterRequest) (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	if g.state.Active {
		return g.state, ErrAlreadyActive
	}
	d := req.Duration
	if d <= 0 || d > DefaultMaxDuration {
		d = DefaultMaxDuration
	}
	now := g.now()
	g.state = State{
		Active:           true,
		LeaseID:          newLeaseID(),
		Reason:           req.Reason,
		EnteredBy:        req.EnteredBy,
		AffectedSlots:    req.AffectedSlots,
		AffectedServices: req.AffectedServices,
		EnteredAt:        now.Unix(),
		ExpiresAt:        now.Add(d).Unix(),
	}
	g.persist()
	g.publishChanged()
	// The journal is the operator's (and smith's own) ground truth — a
	// window that opens or closes silently there is a window nobody can
	// after-the-fact prove existed (found live in the first build_refresh
	// eval run: the window DID open, but nothing logged it).
	g.logf("maintenance: window opened (lease %s) by %q — %s; expires %s",
		g.state.LeaseID, req.EnteredBy, req.Reason, time.Unix(g.state.ExpiresAt, 0).UTC().Format(time.RFC3339))
	return g.state, nil
}

// Exit closes the active window. leaseID must match the current window's
// lease UNLESS force is true — the operator can always end maintenance
// manually (the Console "end maintenance" control), even if they didn't
// hold the lease that opened it; a procedure's own completion path passes
// force=false with its own lease so it can never accidentally close a
// window some other actor opened after it.
func (g *Gate) Exit(leaseID string, force bool) (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	if !g.state.Active {
		return g.state, ErrNotActive
	}
	if !force && leaseID != g.state.LeaseID {
		return g.state, fmt.Errorf("maintenance: lease %q does not hold the active window", leaseID)
	}
	prev := g.state
	g.state = State{}
	g.persist()
	g.publishChanged()
	g.logf("maintenance: window %s closed (force=%v)", prev.LeaseID, force)
	return prev, nil
}

// Blocked reports whether a mutating engine call should be refused right
// now, and the message to surface if so. A call carrying the active
// window's own lease (leaseCtxKey, set via WithLease) is never blocked —
// that's how a Sprint 2 procedure's own steps act during the window it
// opened. Read via engine_wrap.go on every Load/Unload/SwitchMode/
// Restart/StartUnit/StopUnit, and advisorially by sched.Core.EnsureLoaded.
func (g *Gate) Blocked(ctx context.Context) (bool, string) {
	g.mu.Lock()
	g.expireLocked()
	s := g.state
	g.mu.Unlock()
	if !s.Active {
		return false, ""
	}
	if lease, ok := leaseFromContext(ctx); ok && lease == s.LeaseID {
		return false, ""
	}
	reason := s.Reason
	if reason == "" {
		reason = "a maintenance window is active"
	}
	return true, fmt.Sprintf("maintenance mode active (%s) — no loads/unloads/restarts until it ends", reason)
}

// ReconcileOnBoot force-exits any window found active at startup. Sprint 1
// has no procedure that could have survived a restart, so an active window
// at boot is unconditionally orphaned. Sprint 2's resumable procedure
// runner will replace this blanket exit with "resume if a live run claims
// this lease, else exit" — tracked in docs/v5-smith.md's autonomous-
// remediation plan, not silently dropped.
func (g *Gate) ReconcileOnBoot(auditf func(reason string)) {
	g.mu.Lock()
	wasActive := g.state.Active
	g.state = State{}
	g.mu.Unlock()
	if !wasActive {
		return
	}
	g.logf("maintenance: boot reconcile force-exited a window left active across a restart")
	g.persist()
	g.publishChanged()
	if auditf != nil {
		auditf("boot reconcile: no live process was tracking this window")
	}
}

// StartTicker runs a background sweep every interval that lazily expires
// (via Status) an overrun window even with nobody polling it — the
// suspenders to expireLocked's belt. Returns a stop func; also stoppable
// via the ctx passed in. Safe to call at most once per Gate.
func (g *Gate) StartTicker(ctx context.Context, interval time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	g.stopTicker = cancel
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				g.Status() // side effect: expires + publishes if overdue
			}
		}
	}()
}

// Stop halts the background ticker, if running.
func (g *Gate) Stop() {
	if g.stopTicker != nil {
		g.stopTicker()
	}
}

func newLeaseID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("mw-%d", time.Now().UnixNano())
	}
	return "mw-" + hex.EncodeToString(b[:])
}

type leaseCtxKey struct{}

// WithLease marks ctx as acting under the given maintenance lease — the
// one way a mutating engine call proceeds while a window it itself opened
// is active. Sprint 2's procedure runner is the intended (currently only)
// caller.
func WithLease(ctx context.Context, leaseID string) context.Context {
	return context.WithValue(ctx, leaseCtxKey{}, leaseID)
}

func leaseFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(leaseCtxKey{}).(string)
	return v, ok && v != ""
}
