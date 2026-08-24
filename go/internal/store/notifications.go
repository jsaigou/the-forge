// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type notificationsView struct{ d *DB }

// Notifications returns the notifications surface (product/QA sprint,
// 2026-07-29 — docs/v5-plan.md-style: additive Store surface).
func (d *DB) Notifications() Notifications { return notificationsView{d} }

// Upsert records one observed alert occurrence, keyed by dedupeKey. A
// level-triggered alert (e.g. a hang that persists across many collector
// cycles) collapses into one row: repeated Upserts bump last_seen and
// occurrences instead of inserting duplicates. A recurring alert after the
// operator dismissed it is treated as a new occurrence worth surfacing
// again, so Upsert clears dismissed_at on conflict too.
func (v notificationsView) Upsert(ctx context.Context, code, severity, subject, message, dedupeKey string, at time.Time) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO notifications (code, severity, subject, message, dedupe_key, first_seen, last_seen, occurrences)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT(dedupe_key) DO UPDATE SET
		   last_seen = excluded.last_seen,
		   message = excluded.message,
		   occurrences = occurrences + 1,
		   dismissed_at = NULL`,
		code, severity, subject, message, dedupeKey, at.Unix(), at.Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("store: notifications.upsert: %w", err)
	}
	// SQLite's ON CONFLICT ... DO UPDATE doesn't repopulate LastInsertId on
	// the update path — look the row up by its unique key instead.
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		return id, nil
	}
	row := v.d.sql.QueryRowContext(ctx, `SELECT id FROM notifications WHERE dedupe_key = ?`, dedupeKey)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, fmt.Errorf("store: notifications.upsert: lookup after conflict: %w", err)
	}
	return id, nil
}

// List returns notifications, most-recently-seen first.
func (v notificationsView) List(ctx context.Context, includeDismissed bool) ([]Notification, error) {
	q := `SELECT id, code, severity, subject, message, dedupe_key, first_seen, last_seen,
	             occurrences, acknowledged_at, dismissed_at
	      FROM notifications`
	if !includeDismissed {
		q += ` WHERE dismissed_at IS NULL`
	}
	q += ` ORDER BY last_seen DESC`
	rows, err := v.d.sql.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: notifications.list: %w", err)
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: notifications.list: %w", err)
	}
	return out, nil
}

// Acknowledge sets acknowledged_at. Idempotent — acknowledging an already-
// acknowledged notification just refreshes the timestamp.
func (v notificationsView) Acknowledge(ctx context.Context, id int64, at time.Time) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE notifications SET acknowledged_at = ? WHERE id = ?`, at.Unix(), id)
	if err != nil {
		return fmt.Errorf("store: notifications.acknowledge: %w", err)
	}
	return nil
}

// Dismiss sets dismissed_at. Idempotent.
func (v notificationsView) Dismiss(ctx context.Context, id int64, at time.Time) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE notifications SET dismissed_at = ? WHERE id = ?`, at.Unix(), id)
	if err != nil {
		return fmt.Errorf("store: notifications.dismiss: %w", err)
	}
	return nil
}

// AcknowledgeAll acknowledges every currently-active notification.
func (v notificationsView) AcknowledgeAll(ctx context.Context, at time.Time) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE notifications SET acknowledged_at = ? WHERE dismissed_at IS NULL AND acknowledged_at IS NULL`,
		at.Unix())
	if err != nil {
		return fmt.Errorf("store: notifications.acknowledge_all: %w", err)
	}
	return nil
}

func scanNotification(s scanner) (Notification, error) {
	var n Notification
	var firstSeen, lastSeen int64
	var ackAt, dismissAt sql.NullInt64
	if err := s.Scan(&n.ID, &n.Code, &n.Severity, &n.Subject, &n.Message, &n.DedupeKey,
		&firstSeen, &lastSeen, &n.Occurrences, &ackAt, &dismissAt); err != nil {
		if err == sql.ErrNoRows {
			return Notification{}, fmt.Errorf("%w: notification", ErrNotFound)
		}
		return Notification{}, fmt.Errorf("store: notifications.scan: %w", err)
	}
	n.FirstSeen = time.Unix(firstSeen, 0).UTC()
	n.LastSeen = time.Unix(lastSeen, 0).UTC()
	if ackAt.Valid {
		t := time.Unix(ackAt.Int64, 0).UTC()
		n.AcknowledgedAt = &t
	}
	if dismissAt.Valid {
		t := time.Unix(dismissAt.Int64, 0).UTC()
		n.DismissedAt = &t
	}
	return n, nil
}
