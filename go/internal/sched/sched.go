// SPDX-License-Identifier: Apache-2.0

// Package sched ports forge/scheduler.py + docs/scheduler.md: on-demand
// ensure_loaded, the priority queue with queue-jump, reservations (including
// ComfyUI as a reservable resource), and idle-/reservation-aware
// smallest-footprint-first eviction. Phase 5 implements it in-process behind
// one mutex, persisting to store for restart recovery. slots.json,
// queue.json, and all FileLock use are deleted by design.
//
// The Scheduler interface and types below are Contract 2 — frozen; additive
// changes go through the Phase 9 integration owner. JSON-facing shapes
// mirror Contract 1 (docs/v5-api-contract.md).
package sched

import (
	"context"
	"time"
)

// Scheduler is consumed by httpapi (track C), the a0 router (track D), and
// mcp (Phase 7).
type Scheduler interface {
	// EnsureLoaded returns immediately if model is already loaded,
	// otherwise places/queues a load. requested_by identity drives
	// reservation ownership and priority.
	EnsureLoaded(ctx context.Context, req EnsureRequest) (Ticket, error)

	// Unload evicts one slot (or "all") through the scheduler so queue and
	// reservation state stay consistent.
	Unload(ctx context.Context, slot, requestedBy string) error

	// Status mirrors GET /api/v1/scheduler/status (Contract 1).
	Status() Status

	// Config / SetConfig mirror GET/PUT /api/v1/scheduler/config. Runtime
	// tunables persist to the store (settings table), not the config file.
	Config() Config
	SetConfig(Config) error

	// Reservation CRUD, mirroring /api/v1/reservations. Cancel/Update
	// enforce allow_agent_* flags against requestedBy identity kind.
	Reservations() []Reservation
	CreateReservation(ctx context.Context, r Reservation) error
	UpdateReservation(ctx context.Context, label string, r Reservation) error
	CancelReservation(ctx context.Context, label, requestedBy string) error
}

// EnsureRequest asks for a model to be available.
type EnsureRequest struct {
	Model       string
	RequestedBy string // key name / username / "a0"
	TargetSlot  string // "" = scheduler chooses
	SmallJob    bool   // eligible for priority queue-jump
	Priority    int
}

// Ticket mirrors the queue entries in scheduler status (Contract 1:
// QueueTicket).
type Ticket struct {
	TicketID    string
	Model       string
	RequestedBy string
	TargetSlot  string // "" = unassigned
	Status      string // "loaded" | "queued" | "loading" | "failed" | ...
	SmallJob    bool
	EnqueuedAt  time.Time
}

// Status mirrors GET /api/v1/scheduler/status.
type Status struct {
	Slots       map[string]string // slot -> mode ("" = empty)
	SlotLabels  map[string]string
	IdleSeconds map[string]*float64 // nil = unknown/empty
	// SlotMemoryBytes is each occupied slot's real live GPU footprint
	// (collector.SlotState.MemoryBytes — VRAM+GTT via fdinfo, includes KV
	// cache), 0/absent for empty slots or when the probe couldn't read it.
	SlotMemoryBytes map[string]int64
	// UnitMemoryBytes is the NON-slot watched units' real GPU footprints by
	// unit name (S2 attribution: ComfyUI, always-on services, compressor
	// proxies — whoever holds GTT while no slot does). Units with 0 bytes or
	// unknown state are absent. Slot units never appear here — their figures
	// live in SlotMemoryBytes so consumers can sum both maps without
	// double-counting.
	UnitMemoryBytes map[string]int64
	MemoryBudget    Budget
	Queue           []Ticket
}

// Budget mirrors memory_budget in scheduler status. A1 bytes retrofit:
// fields are bytes (were MB). engine.Budget and this struct stay in lockstep.
type Budget struct {
	TotalBytes int64
	UsedBytes  int64
	FreeBytes  int64
}

// Config mirrors GET/PUT /api/v1/scheduler/config (Contract 1), defaults per
// validators.SchedulerConfigRequest.
type Config struct {
	IdleUnloadS            int `json:"idle_unload_s"`
	SmallJobTokenThreshold int `json:"small_job_token_threshold"`
	PriorityJumpCap        int `json:"priority_jump_cap"`
	ReservationSoonMin     int `json:"reservation_soon_min"`

	// ComfyUIEvictable is the operator opt-out for stopping the deployment's
	// ComfyUI service when a load needs its memory (S1, feedback F3). Nil
	// means "not set" → enabled: pre-S1 stored configs lack the field, and
	// an absent field must not silently flip the default off. Explicit
	// false is the opt-out.
	ComfyUIEvictable *bool `json:"comfyui_evictable,omitempty"`
}

// ComfyEvictable resolves the opt-out (default on).
func (c Config) ComfyEvictable() bool {
	return c.ComfyUIEvictable == nil || *c.ComfyUIEvictable
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		IdleUnloadS:            180,
		SmallJobTokenThreshold: 1500,
		PriorityJumpCap:        2,
		ReservationSoonMin:     10,
	}
}

// Reservation mirrors the /api/v1/reservations entries (Contract 1). Scope
// is "bay" | "whole_box" | "comfyui"; Bay is set iff scope == "bay".
type Reservation struct {
	Label                  string
	Model                  string
	Start                  time.Time
	End                    time.Time
	Scope                  string
	Bay                    string // "" unless Scope == "bay"
	CreatedBy              string
	AllowAgentReschedule   bool
	AllowAgentCancellation bool
}

// Stub is a compile-time placeholder Scheduler for tracks C/D and Phase 7.
type Stub struct {
	Cfg Config
}

var _ Scheduler = (*Stub)(nil)

func (s *Stub) EnsureLoaded(_ context.Context, req EnsureRequest) (Ticket, error) {
	return Ticket{
		TicketID:    "stub",
		Model:       req.Model,
		RequestedBy: req.RequestedBy,
		TargetSlot:  req.TargetSlot,
		Status:      "loaded",
		SmallJob:    req.SmallJob,
		EnqueuedAt:  time.Now(),
	}, nil
}

func (s *Stub) Unload(context.Context, string, string) error { return nil }

func (s *Stub) Status() Status {
	return Status{
		Slots:       map[string]string{},
		SlotLabels:  map[string]string{},
		IdleSeconds: map[string]*float64{},
	}
}

func (s *Stub) Config() Config {
	if s.Cfg == (Config{}) {
		return DefaultConfig()
	}
	return s.Cfg
}

func (s *Stub) SetConfig(c Config) error { s.Cfg = c; return nil }

func (s *Stub) Reservations() []Reservation { return nil }

func (s *Stub) CreateReservation(context.Context, Reservation) error { return nil }

func (s *Stub) UpdateReservation(context.Context, string, Reservation) error { return nil }

func (s *Stub) CancelReservation(context.Context, string, string) error { return nil }
