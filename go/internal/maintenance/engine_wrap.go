// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
)

// engineFull is every method any real consumer of the wrapped engine
// value needs — engine.Engine (Contract 2, frozen) plus the two extra
// concrete methods sched.Core and smith.Placer additionally require
// (FitPlan, SlotStates). *engine.Manager satisfies this as-is (see
// var _ below); tests can supply any fake with the same method set.
type engineFull interface {
	engine.Engine
	FitPlan(mode string) (engine.Plan, error)
	SlotStates(units map[string]collector.UnitState) map[string]collector.SlotAssignment
}

var _ engineFull = (*engine.Manager)(nil)

// GatedEngine wraps a real engine, refusing every mutating call while a
// maintenance window is active (Gate.Blocked). Read-only methods pass
// straight through — observability must not go dark during a repair.
// Assign the result wherever main.go currently hands out the bare engine
// value (httpapi.Deps.Engine, sched.Deps.Engine, profile.Deps.Engine,
// smith.Deps.Placer) so every caller is covered by construction.
type GatedEngine struct {
	real engineFull
	gate *Gate
}

var _ engineFull = (*GatedEngine)(nil)

// WrapEngine returns real unchanged if gate is nil (a daemon that never
// wires maintenance — e.g. a stub environment — behaves exactly as before).
func WrapEngine(gate *Gate, real engineFull) engineFull {
	if gate == nil {
		return real
	}
	return &GatedEngine{real: real, gate: gate}
}

// ── read-only passthrough ────────────────────────────────────────────────

func (g *GatedEngine) CurrentMode() string { return g.real.CurrentMode() }
func (g *GatedEngine) Slots() []string     { return g.real.Slots() }

func (g *GatedEngine) CanFit(mode string) (engine.CanFit, error) { return g.real.CanFit(mode) }
func (g *GatedEngine) MemoryBudget() (engine.Budget, error)      { return g.real.MemoryBudget() }
func (g *GatedEngine) FitPlan(mode string) (engine.Plan, error)  { return g.real.FitPlan(mode) }

func (g *GatedEngine) SlotStates(units map[string]collector.UnitState) map[string]collector.SlotAssignment {
	return g.real.SlotStates(units)
}

// ── gated mutations ─────────────────────────────────────────────────────

func (g *GatedEngine) Load(ctx context.Context, mode, slot string) engine.Result {
	if blocked, msg := g.gate.Blocked(ctx); blocked {
		return engine.Result{Success: false, Message: msg}
	}
	return g.real.Load(ctx, mode, slot)
}

func (g *GatedEngine) Unload(ctx context.Context, slot string) engine.Result {
	if blocked, msg := g.gate.Blocked(ctx); blocked {
		return engine.Result{Success: false, Message: msg}
	}
	return g.real.Unload(ctx, slot)
}

func (g *GatedEngine) SwitchMode(ctx context.Context, mode string) engine.Result {
	if blocked, msg := g.gate.Blocked(ctx); blocked {
		return engine.Result{Success: false, Message: msg}
	}
	return g.real.SwitchMode(ctx, mode)
}

func (g *GatedEngine) Restart(ctx context.Context) engine.Result {
	if blocked, msg := g.gate.Blocked(ctx); blocked {
		return engine.Result{Success: false, Message: msg}
	}
	return g.real.Restart(ctx)
}

func (g *GatedEngine) StartUnit(ctx context.Context, unit string) error {
	if blocked, msg := g.gate.Blocked(ctx); blocked {
		return errBlocked(msg)
	}
	return g.real.StartUnit(ctx, unit)
}

func (g *GatedEngine) StopUnit(ctx context.Context, unit string) error {
	if blocked, msg := g.gate.Blocked(ctx); blocked {
		return errBlocked(msg)
	}
	return g.real.StopUnit(ctx, unit)
}

type blockedError string

func (e blockedError) Error() string { return string(e) }

func errBlocked(msg string) error { return blockedError(msg) }
