// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type sessionsView struct{ d *DB }

// Sessions returns the browser-session surface. Session IDs are secret —
// they never appear in error messages or logs.
func (d *DB) Sessions() Sessions { return sessionsView{d} }

func (v sessionsView) Create(ctx context.Context, s Session) error {
	assurance := s.Assurance
	if assurance == "" {
		assurance = "password"
	}
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, csrf_token, created_at, expires_at,
		                       last_seen_at, remote_addr, user_agent,
		                       assurance, assurance_at, network_principal)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.UserID, s.CSRFToken, unixOf(orNow(s.CreatedAt)), unixOf(s.ExpiresAt),
		nullUnix(s.LastSeenAt), nullStr(s.RemoteAddr), nullStr(s.UserAgent),
		assurance, nullUnix(s.AssuranceAt), nullStr(s.NetworkPrincipal),
	)
	if err != nil {
		return fmt.Errorf("store: sessions.create: %w", err)
	}
	return nil
}

func (v sessionsView) Get(ctx context.Context, id string) (Session, error) {
	var s Session
	var created, expires int64
	var lastSeen sql.NullInt64
	var remote, ua sql.NullString
	var assurance sql.NullString
	var assuranceAt sql.NullInt64
	var networkPrincipal sql.NullString
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, user_id, csrf_token, created_at, expires_at, last_seen_at,
		        remote_addr, user_agent, assurance, assurance_at, network_principal
		 FROM sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.UserID, &s.CSRFToken, &created, &expires, &lastSeen, &remote, &ua,
		&assurance, &assuranceAt, &networkPrincipal)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: sessions.get: %w", err)
	}
	s.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
	s.ExpiresAt = timeOf(sql.NullInt64{Int64: expires, Valid: true})
	s.LastSeenAt = timeOf(lastSeen)
	s.RemoteAddr = strOf(remote)
	s.UserAgent = strOf(ua)
	if assurance.Valid {
		s.Assurance = assurance.String
	} else {
		s.Assurance = "password"
	}
	s.AssuranceAt = timeOf(assuranceAt)
	s.NetworkPrincipal = strOf(networkPrincipal)
	return s, nil
}

func (v sessionsView) Touch(ctx context.Context, id string, at time.Time) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, unixOf(at), id)
	if err != nil {
		return fmt.Errorf("store: sessions.touch: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete is idempotent — logging out an already-deleted session is not an
// error (logout races with the expiry sweeper).
func (v sessionsView) Delete(ctx context.Context, id string) error {
	if _, err := v.d.sql.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: sessions.delete: %w", err)
	}
	return nil
}

func (v sessionsView) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ?`, unixOf(now))
	if err != nil {
		return 0, fmt.Errorf("store: sessions.sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: sessions.sweep: %w", err)
	}
	return n, nil
}

// ElevateSession rotates the session ID, CSRF token, and sets the assurance
// level + timestamp in one UPDATE (§3.5 step-up). Returns ErrNotFound when
// oldID does not exist. Session fixation is prevented by always issuing a
// fresh ID on elevation.
func (v sessionsView) ElevateSession(ctx context.Context, oldID, newID, newCSRF, assurance string, at time.Time) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE sessions SET id = ?, csrf_token = ?, assurance = ?, assurance_at = ?
		 WHERE id = ?`,
		newID, newCSRF, assurance, unixOf(at), oldID)
	if err != nil {
		return fmt.Errorf("store: sessions.elevate: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
