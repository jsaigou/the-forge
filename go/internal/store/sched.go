// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type schedView struct{ d *DB }

// Sched returns the scheduler persistence surface. Restart recovery only —
// in-memory state is authoritative at runtime (Contract 2); this replaces
// V4's slots.json/queue.json and all FileLock use.
func (d *DB) Sched() Sched { return schedView{d} }

// SaveSlot upserts a slot row. mode == "" records an empty slot (NULL).
func (v schedView) SaveSlot(ctx context.Context, slot, mode string, loadedAt time.Time) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO slot_state (slot, mode, loaded_at, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(slot) DO UPDATE SET
		   mode = excluded.mode, loaded_at = excluded.loaded_at,
		   updated_at = excluded.updated_at`,
		slot, nullStr(mode), nullUnix(loadedAt), unixOf(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("store: sched.save_slot: %w", err)
	}
	return nil
}

// Slots returns every recorded slot; empty slots map to "".
func (v schedView) Slots(ctx context.Context) (map[string]string, error) {
	rows, err := v.d.sql.QueryContext(ctx, `SELECT slot, mode FROM slot_state`)
	if err != nil {
		return nil, fmt.Errorf("store: sched.slots: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var slot string
		var mode sql.NullString
		if err := rows.Scan(&slot, &mode); err != nil {
			return nil, fmt.Errorf("store: sched.slots: %w", err)
		}
		out[slot] = strOf(mode)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sched.slots: %w", err)
	}
	return out, nil
}

func (v schedView) SaveTicket(ctx context.Context, t QueueRow) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO sched_queue (ticket_id, model, requested_by, target_slot,
		                          status, small_job, priority, enqueued_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(ticket_id) DO UPDATE SET
		   model = excluded.model, requested_by = excluded.requested_by,
		   target_slot = excluded.target_slot, status = excluded.status,
		   small_job = excluded.small_job, priority = excluded.priority,
		   enqueued_at = excluded.enqueued_at, updated_at = excluded.updated_at`,
		t.TicketID, t.Model, t.RequestedBy, nullStr(t.TargetSlot), t.Status,
		boolInt(t.SmallJob), t.Priority, unixOf(orNow(t.EnqueuedAt)), unixOf(orNow(t.UpdatedAt)),
	)
	if err != nil {
		return fmt.Errorf("store: sched.save_ticket: %w", err)
	}
	return nil
}

// DeleteTicket is idempotent — completion and cancellation race benignly.
func (v schedView) DeleteTicket(ctx context.Context, ticketID string) error {
	if _, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM sched_queue WHERE ticket_id = ?`, ticketID); err != nil {
		return fmt.Errorf("store: sched.delete_ticket: %w", err)
	}
	return nil
}

func (v schedView) Queue(ctx context.Context) ([]QueueRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT ticket_id, model, requested_by, target_slot, status, small_job,
		        priority, enqueued_at, updated_at
		 FROM sched_queue ORDER BY enqueued_at, ticket_id`)
	if err != nil {
		return nil, fmt.Errorf("store: sched.queue: %w", err)
	}
	defer rows.Close()
	var out []QueueRow
	for rows.Next() {
		var t QueueRow
		var target sql.NullString
		var smallJob, enqueued, updated int64
		if err := rows.Scan(&t.TicketID, &t.Model, &t.RequestedBy, &target, &t.Status,
			&smallJob, &t.Priority, &enqueued, &updated); err != nil {
			return nil, fmt.Errorf("store: sched.queue: %w", err)
		}
		t.TargetSlot = strOf(target)
		t.SmallJob = smallJob != 0
		t.EnqueuedAt = timeOf(sql.NullInt64{Int64: enqueued, Valid: true})
		t.UpdatedAt = timeOf(sql.NullInt64{Int64: updated, Valid: true})
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sched.queue: %w", err)
	}
	return out, nil
}

// SaveReservation upserts by label. The Contract 3 CHECK constraints
// (end > start; bay set iff scope='bay') are enforced by SQLite — Bay must
// be "" unless Scope == "bay".
func (v schedView) SaveReservation(ctx context.Context, r ReservationRow) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO reservations (label, model, start_ts, end_ts, scope, bay,
		   created_by, allow_agent_reschedule, allow_agent_cancellation, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(label) DO UPDATE SET
		   model = excluded.model, start_ts = excluded.start_ts,
		   end_ts = excluded.end_ts, scope = excluded.scope, bay = excluded.bay,
		   created_by = excluded.created_by,
		   allow_agent_reschedule = excluded.allow_agent_reschedule,
		   allow_agent_cancellation = excluded.allow_agent_cancellation`,
		r.Label, r.Model, unixOf(r.Start), unixOf(r.End), r.Scope, nullStr(r.Bay),
		r.CreatedBy, boolInt(r.AllowAgentReschedule), boolInt(r.AllowAgentCancellation),
		unixOf(orNow(r.CreatedAt)),
	)
	if err != nil {
		return fmt.Errorf("store: sched.save_reservation: %w", err)
	}
	return nil
}

func (v schedView) DeleteReservation(ctx context.Context, label string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM reservations WHERE label = ?`, label)
	if err != nil {
		return fmt.Errorf("store: sched.delete_reservation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v schedView) Reservations(ctx context.Context) ([]ReservationRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, label, model, start_ts, end_ts, scope, bay, created_by,
		        allow_agent_reschedule, allow_agent_cancellation, created_at
		 FROM reservations ORDER BY start_ts, label`)
	if err != nil {
		return nil, fmt.Errorf("store: sched.reservations: %w", err)
	}
	defer rows.Close()
	var out []ReservationRow
	for rows.Next() {
		var r ReservationRow
		var bay sql.NullString
		var start, end, resched, cancel, created int64
		if err := rows.Scan(&r.ID, &r.Label, &r.Model, &start, &end, &r.Scope, &bay,
			&r.CreatedBy, &resched, &cancel, &created); err != nil {
			return nil, fmt.Errorf("store: sched.reservations: %w", err)
		}
		r.Start = timeOf(sql.NullInt64{Int64: start, Valid: true})
		r.End = timeOf(sql.NullInt64{Int64: end, Valid: true})
		r.Bay = strOf(bay)
		r.AllowAgentReschedule = resched != 0
		r.AllowAgentCancellation = cancel != 0
		r.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sched.reservations: %w", err)
	}
	return out, nil
}
