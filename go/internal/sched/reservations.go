// SPDX-License-Identifier: Apache-2.0

package sched

// reservations.go — reservation CRUD, ownership/permission enforcement,
// and window helpers (docs/scheduler.md "Reservations"). Reservations are
// authoritative in-memory and write-through to store.Sched for restart
// recovery. ComfyUI is a first-class reservable resource via
// scope="comfyui" — a pure coordination feature; comfyui reservations are
// listed/enforced for CRUD like any other but inference-slot eviction
// candidates remain the four bays only (ComfyUI was never an eviction
// target).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Sentinel errors. Their texts are load-bearing: httpapi's classifiers map
// them to 404 ("not found"), 409 ("conflict"), and 403 ("permission"/
// "denied") by errors.Is or message substring.
var (
	// ErrNotFound reports a reservation label that matches nothing.
	ErrNotFound = errors.New("not found")
	// ErrConflict reports a duplicate reservation label.
	ErrConflict = errors.New("conflict")
	// ErrPermissionDenied reports an agent touching a reservation it does
	// not own without the matching allow_agent_* flag.
	ErrPermissionDenied = errors.New("permission denied")
)

// ResolveAgentFlags applies V4's tri-state defaults for the allow_agent_*
// flags when a create request omits them: human-created reservations
// default locked (false/false — the UI flow implies deliberate lock-down
// intent), agent-created ones default open (true/true). The frozen
// Reservation struct carries plain bools, so callers (httpapi passes
// explicit values; MCP in Phase 7 resolves absent fields) apply this
// before CreateReservation.
func ResolveAgentFlags(createdBy string, reschedule, cancellation *bool) (allowReschedule, allowCancellation bool) {
	def := createdBy != HumanIdentity
	allowReschedule, allowCancellation = def, def
	if reschedule != nil {
		allowReschedule = *reschedule
	}
	if cancellation != nil {
		allowCancellation = *cancellation
	}
	return allowReschedule, allowCancellation
}

// Reservations returns a copy of all reservations, sorted by insertion
// (store recovery order is start_ts, label).
func (c *Core) Reservations() []Reservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Reservation(nil), c.reservations...)
}

// CreateReservation validates and adds a reservation. A duplicate label is
// a conflict (409 at the HTTP layer). The caller has already resolved the
// allow_agent_* tri-state (see ResolveAgentFlags).
func (c *Core) CreateReservation(ctx context.Context, r Reservation) error {
	if err := validateReservation(r); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.reservations {
		if existing.Label == r.Label {
			return fmt.Errorf("sched: reservation %q already exists: %w", r.Label, ErrConflict)
		}
	}
	if err := c.persistReservationLocked(ctx, r); err != nil {
		return err
	}
	c.reservations = append(c.reservations, r)
	return nil
}

// UpdateReservation modifies the reservation named by label. The frozen
// signature carries no separate requester identity, so r.CreatedBy is the
// requesting identity (httpapi always sends "human"; MCP sends the agent
// key name) — checked against the stored reservation's ownership rules
// with action "reschedule". The stored creator is preserved; an update
// never transfers ownership.
func (c *Core) UpdateReservation(ctx context.Context, label string, r Reservation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.reservationIndexLocked(label)
	if idx < 0 {
		return fmt.Errorf("sched: reservation %q: %w", label, ErrNotFound)
	}
	existing := c.reservations[idx]
	if err := checkReservationPermission(existing, r.CreatedBy, "reschedule"); err != nil {
		return err
	}
	updated := r
	updated.Label = label // label is identity; renames are delete+create
	updated.CreatedBy = existing.CreatedBy
	if err := validateReservation(updated); err != nil {
		return err
	}
	if err := c.persistReservationLocked(ctx, updated); err != nil {
		return err
	}
	c.reservations[idx] = updated
	return nil
}

// CancelReservation deletes the reservation named by label, enforcing the
// ownership rules with action "cancel" against requestedBy.
func (c *Core) CancelReservation(ctx context.Context, label, requestedBy string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.reservationIndexLocked(label)
	if idx < 0 {
		return fmt.Errorf("sched: reservation %q: %w", label, ErrNotFound)
	}
	if err := checkReservationPermission(c.reservations[idx], requestedBy, "cancel"); err != nil {
		return err
	}
	if c.d.Sched != nil {
		if err := c.d.Sched.DeleteReservation(ctx, label); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("sched: delete reservation %q: %w", label, err)
		}
	}
	c.reservations = append(c.reservations[:idx], c.reservations[idx+1:]...)
	return nil
}

// ── internals ────────────────────────────────────────────────────────────────

func (c *Core) reservationIndexLocked(label string) int {
	for i, r := range c.reservations {
		if r.Label == label {
			return i
		}
	}
	return -1
}

func (c *Core) persistReservationLocked(ctx context.Context, r Reservation) error {
	if c.d.Sched == nil {
		return nil
	}
	if err := c.d.Sched.SaveReservation(ctx, reservationToRow(r, c.d.Now())); err != nil {
		return fmt.Errorf("sched: persist reservation %q: %w", r.Label, err)
	}
	return nil
}

func validateReservation(r Reservation) error {
	if r.Label == "" {
		return errors.New("sched: reservation label is required")
	}
	if r.Model == "" {
		return errors.New("sched: reservation model is required")
	}
	switch r.Scope {
	case "bay":
		if r.Bay == "" {
			return errors.New(`sched: scope "bay" requires a bay`)
		}
	case "whole_box", "comfyui":
		if r.Bay != "" {
			return fmt.Errorf("sched: scope %q must not carry a bay", r.Scope)
		}
	default:
		return fmt.Errorf("sched: invalid scope %q (bay|whole_box|comfyui)", r.Scope)
	}
	if r.Start.IsZero() || r.End.IsZero() || !r.End.After(r.Start) {
		return errors.New("sched: reservation end must be after start")
	}
	if r.CreatedBy == "" {
		return errors.New("sched: reservation created_by is required")
	}
	return nil
}

// checkReservationPermission ports V4's _check_reservation_permission:
// a human (dashboard) actor can always act; an agent can always touch its
// own reservations; anything else requires the matching allow_agent_* flag
// on the target reservation. action is "reschedule" or "cancel".
func checkReservationPermission(r Reservation, requestedBy, action string) error {
	if requestedBy == HumanIdentity {
		return nil
	}
	if requestedBy != "" && r.CreatedBy == requestedBy {
		return nil
	}
	allowed := r.AllowAgentReschedule
	if action == "cancel" {
		allowed = r.AllowAgentCancellation
	}
	if allowed {
		return nil
	}
	return fmt.Errorf("sched: %q may not %s reservation %q (created_by=%q): %w",
		requestedBy, action, r.Label, r.CreatedBy, ErrPermissionDenied)
}

// activeReservationForModel returns a reservation for model whose window
// [start, end) contains now, or nil. Like V4, this matches by model across
// all scopes.
func activeReservationForModel(reservations []Reservation, model string, now time.Time) *Reservation {
	for i := range reservations {
		r := &reservations[i]
		if r.Model != model {
			continue
		}
		if !now.Before(r.Start) && now.Before(r.End) {
			return r
		}
	}
	return nil
}

// modelReservedSoon reports whether model has a reservation starting
// within soon from now (the evict-then-immediately-reload churn guard).
func modelReservedSoon(reservations []Reservation, model string, now time.Time, soon time.Duration) bool {
	for _, r := range reservations {
		if r.Model != model {
			continue
		}
		if !r.Start.Before(now) && !r.Start.After(now.Add(soon)) {
			return true
		}
	}
	return false
}

// activeReservationEnd returns when model's active reservation window ends
// (zero time = no active reservation). Feeds place()'s horizon decisions.
func activeReservationEnd(reservations []Reservation, model string, now time.Time) time.Time {
	var end time.Time
	for _, r := range reservations {
		if r.Model != model || r.End.Before(end) {
			continue
		}
		if !now.Before(r.Start) && now.Before(r.End) && r.End.After(end) {
			end = r.End
		}
	}
	return end
}

// soonReservationStart returns when model's upcoming in-window-soon
// reservation starts (zero time = none).
func soonReservationStart(reservations []Reservation, model string, now time.Time, soon time.Duration) time.Time {
	var start time.Time
	for _, r := range reservations {
		if r.Model != model {
			continue
		}
		if !r.Start.Before(now) && !r.Start.After(now.Add(soon)) && (start.IsZero() || r.Start.Before(start)) {
			start = r.Start
		}
	}
	return start
}

// activeComfyReservation reports whether any comfyui-scope reservation is
// in its window — ComfyUI must not be stopped for memory while one stands.
// Unlike the model-scoped helpers, scope=comfyui reservations coordinate the
// service itself and carry arbitrary Model values.
func activeComfyReservation(reservations []Reservation, now time.Time) bool {
	for _, r := range reservations {
		if r.Scope == "comfyui" && !now.Before(r.Start) && now.Before(r.End) {
			return true
		}
	}
	return false
}

func activeComfyReservationEnd(reservations []Reservation, now time.Time) time.Time {
	var end time.Time
	for _, r := range reservations {
		if r.Scope != "comfyui" || r.End.Before(end) {
			continue
		}
		if !now.Before(r.Start) && now.Before(r.End) && r.End.After(end) {
			end = r.End
		}
	}
	return end
}

func soonComfyReservation(reservations []Reservation, now time.Time, soon time.Duration) bool {
	for _, r := range reservations {
		if r.Scope != "comfyui" {
			continue
		}
		if !r.Start.Before(now) && !r.Start.After(now.Add(soon)) {
			return true
		}
	}
	return false
}

// reservationFromRow / reservationToRow convert between the frozen
// Contract 2 shape and track B's store row.
func reservationFromRow(row store.ReservationRow) Reservation {
	return Reservation{
		Label:                  row.Label,
		Model:                  row.Model,
		Start:                  row.Start,
		End:                    row.End,
		Scope:                  row.Scope,
		Bay:                    row.Bay,
		CreatedBy:              row.CreatedBy,
		AllowAgentReschedule:   row.AllowAgentReschedule,
		AllowAgentCancellation: row.AllowAgentCancellation,
	}
}

func reservationToRow(r Reservation, now time.Time) store.ReservationRow {
	return store.ReservationRow{
		Label:                  r.Label,
		Model:                  r.Model,
		Start:                  r.Start,
		End:                    r.End,
		Scope:                  r.Scope,
		Bay:                    r.Bay,
		CreatedBy:              r.CreatedBy,
		AllowAgentReschedule:   r.AllowAgentReschedule,
		AllowAgentCancellation: r.AllowAgentCancellation,
		CreatedAt:              now,
	}
}
