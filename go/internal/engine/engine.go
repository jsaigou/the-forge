// SPDX-License-Identifier: Apache-2.0

// Package engine ports forge/engine.py: mode switching, slot load/unload,
// env/args sysconfig writing, n_ctx verification, memory budgeting. Owned by
// track A (Phase 2). The Engine interface below is Contract 2 — frozen;
// additive changes go through the Phase 9 integration owner.
//
// Crown-jewels requirements (docs/v5-plan.md) bind every implementation:
// slot state must not clear while a unit is deactivating; n_ctx is verified
// via /props after every load; --parallel is always explicit; inference_rss
// is additive (gtt + unified), never max().
package engine

import (
	"context"
	"sync"
)

// Engine is the mode/slot lifecycle surface consumed by httpapi (track C)
// and sched (Phase 5).
type Engine interface {
	// CurrentMode returns the active mode name ("" when unloaded). Backed
	// by the state file tiebreaker semantics of V4's get_current_mode().
	CurrentMode() string

	// Slots returns the configured slot names in display order
	// (config-driven; default a1, a2, a3, a4).
	Slots() []string

	// SwitchMode performs a full mode switch: stop managed units → write
	// sysconfig env/args → start target units → verify /health and /props
	// (n_ctx). Blocking; callers run it in a goroutine and stream progress
	// via the event bus.
	SwitchMode(ctx context.Context, mode string) Result

	// Restart restarts the current mode's units and re-verifies.
	Restart(ctx context.Context) Result

	// Load loads mode into one slot without touching other slots.
	Load(ctx context.Context, mode, slot string) Result

	// Unload stops one slot's unit ("all" for every slot) and waits for it
	// to fully leave deactivating before reporting the slot free.
	Unload(ctx context.Context, slot string) Result

	// CanFit reports whether mode fits in memory right now, using the
	// additive inference-RSS accounting and the registry/GGUF-derived
	// memory requirement.
	CanFit(mode string) (CanFit, error)

	// MemoryBudget returns the current budget used by CanFit and exposed
	// in GET /api/v1/scheduler/status.
	MemoryBudget() (Budget, error)

	// StartUnit starts an auxiliary (non-inference) systemd unit — service
	// modes and the TTS service — without touching inference slots. unit is
	// the name without ".service". This is the Contract 2 amendment (C1-Q2)
	// that lets httpapi control these units without shelling out from a
	// handler (design decision 2); the engine already owns the D-Bus adapter.
	StartUnit(ctx context.Context, unit string) error

	// StopUnit stops an auxiliary systemd unit (see StartUnit).
	StopUnit(ctx context.Context, unit string) error
}

// Result is the uniform outcome shape for lifecycle operations, mirroring
// V4's {"success": bool, "message": str} dicts (Contract 1).
type Result struct {
	Success bool
	Message string

	// NCtx carries the verified actual context after a successful
	// load/switch (0 when not applicable).
	NCtx int
}

// CanFit is the fit-check outcome.
//
// A1 (bytes retrofit, 2026-07-24): the budget figures are bytes (the
// kernel's native unit). The wire keys changed to *_bytes; the PWA types
// move with them.
type CanFit struct {
	Fits          bool
	RequiredBytes int64
	FreeBytes     int64
	Reason        string // human-readable explanation when !Fits
}

// Budget mirrors scheduler status memory_budget (Contract 1). Bytes-native
// since A1 (2026-07-24).
type Budget struct {
	TotalBytes int64
	UsedBytes  int64
	FreeBytes  int64
}

// Stub is a compile-time placeholder Engine for tracks B/C/D. Every
// operation reports success without doing anything.
//
// Stub is safe for concurrent use: CurrentMode and SwitchMode synchronize
// on mu. The Engine interface is called from multiple goroutines in
// production (the heartbeat goroutine calls CurrentMode while background
// lifecycle goroutines call SwitchMode/Load/Unload), so implementations
// must be thread-safe.
type Stub struct {
	mu        sync.Mutex
	Mode      string
	SlotNames []string
}

var _ Engine = (*Stub)(nil)

func (s *Stub) CurrentMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Mode
}

func (s *Stub) Slots() []string {
	if len(s.SlotNames) == 0 {
		return []string{"a1", "a2", "a3", "a4"}
	}
	return s.SlotNames
}

func (s *Stub) SwitchMode(_ context.Context, mode string) Result {
	s.mu.Lock()
	s.Mode = mode
	s.mu.Unlock()
	return Result{Success: true, Message: "stub switch"}
}

func (s *Stub) Restart(context.Context) Result {
	return Result{Success: true, Message: "stub restart"}
}

func (s *Stub) Load(_ context.Context, mode, slot string) Result {
	return Result{Success: true, Message: "stub load"}
}

func (s *Stub) Unload(_ context.Context, slot string) Result {
	return Result{Success: true, Message: "stub unload"}
}

func (s *Stub) CanFit(string) (CanFit, error) {
	return CanFit{Fits: true, FreeBytes: 1 << 40}, nil
}

func (s *Stub) MemoryBudget() (Budget, error) {
	return Budget{TotalBytes: 1 << 40, FreeBytes: 1 << 40}, nil
}

func (s *Stub) StartUnit(context.Context, string) error { return nil }

func (s *Stub) StopUnit(context.Context, string) error { return nil }
