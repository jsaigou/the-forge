// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"fmt"
	"time"
)

// retention.go — P7's real retention/pruning (docs/v5-smith.md §9,
// "retention/pruning for findings+cache"). Real data pulled from ForgeHost the
// day this was built: 1351 smith_findings rows in 6 days, 1067 of them
// severity=ok — every check writes a row every hour, forever, with nothing
// pruning any smith_* table. web/cache.go's opportunistic per-write
// expired-row delete named itself "the interim mechanism until P7's real
// pruning sprint" — this supersedes it (that delete is removed).
//
// Findings attached to an investigation are NEVER pruned by any tier, at
// any age — an investigation is an evidence trail, not sweep noise. Only
// standalone findings (investigation_id IS NULL) age out.

// retentionInterval is how often scheduleLoop's 1-minute tick considers
// running a prune — mirrors webProbeInterval's shape exactly
// (web_research.go's maybeProbeWeb), including "dispatch with go so a slow
// prune never delays sweep timing."
const retentionInterval = 6 * time.Hour

// RetentionResult is one prune run's outcome — surfaced on GET /smith/status
// (SelfContext.Retention) so "does retention actually run" is a live-status
// check, not a leap of faith.
type RetentionResult struct {
	RanAt           time.Time `json:"ran_at"`
	DeletedFindings int64     `json:"deleted_findings"`
	DeletedWebCache int64     `json:"deleted_web_cache"`
}

// RetentionStatus is the SelfContext projection of the last prune run.
// LastRunAt is nil until the first run completes (never faked as "now").
type RetentionStatus struct {
	Enabled         bool   `json:"enabled"`
	LastRunAt       *int64 `json:"last_run_at"`
	DeletedFindings int64  `json:"deleted_findings"`
	DeletedWebCache int64  `json:"deleted_web_cache"`
}

// maybePrune runs a prune when the last one is older than retentionInterval,
// called from scheduleLoop's 1-minute tick — the exact maybeProbeWeb shape.
func (s *Smith) maybePrune(ctx context.Context, now time.Time) {
	s.pruneMu.Lock()
	due := now.Sub(s.lastPruneAt) >= retentionInterval
	s.pruneMu.Unlock()
	if due {
		go s.pruneOnce(ctx)
	}
}

// pruneOnce runs one retention pass: severity-tiered findings pruning
// (ok/warn-crit in days, info in hours; a tier value <= 0 skips that tier
// entirely, rather than deleting everything — the same footgun-avoidance as
// store.RunRetention's days<=0 guard, internal/store/sampler.go), a
// non-configurable 30-day hard cap, plus web-cache age+row-count pruning.
// Settings are re-read every call, so an operator edit lands on the next
// scheduled run with no restart, same as smith.schedule.
func (s *Smith) pruneOnce(ctx context.Context) (RetentionResult, error) {
	now := s.d.Now()
	res := RetentionResult{RanAt: now}
	s.pruneMu.Lock()
	s.lastPruneAt = now
	s.pruneMu.Unlock()

	if s.d.Store == nil {
		return res, nil
	}
	cfg := s.RetentionConfig(ctx)
	if !cfg.Enabled {
		s.recordRetentionResult(res)
		return res, nil
	}

	for _, tier := range []struct {
		severity string
		value    int
		hours    bool
	}{
		{"ok", cfg.OKDays, false},
		{"info", cfg.InfoHours, true},
	} {
		if tier.value <= 0 {
			continue
		}
		var cutoff int64
		if tier.hours {
			cutoff = now.Add(-time.Duration(tier.value) * time.Hour).Unix()
		} else {
			cutoff = now.Add(-time.Duration(tier.value) * 24 * time.Hour).Unix()
		}
		r, err := s.d.Store.SQL().ExecContext(ctx,
			`DELETE FROM smith_findings WHERE severity = ? AND investigation_id IS NULL AND created_at < ?`,
			tier.severity, cutoff)
		if err != nil {
			return res, fmt.Errorf("smith: prune findings (%s): %w", tier.severity, err)
		}
		n, _ := r.RowsAffected()
		res.DeletedFindings += n
	}
	if cfg.WarnCritDays > 0 {
		cutoff := now.Add(-time.Duration(cfg.WarnCritDays) * 24 * time.Hour).Unix()
		r, err := s.d.Store.SQL().ExecContext(ctx,
			`DELETE FROM smith_findings WHERE severity IN ('warn','crit') AND investigation_id IS NULL AND created_at < ?`,
			cutoff)
		if err != nil {
			return res, fmt.Errorf("smith: prune findings (warn/crit): %w", err)
		}
		n, _ := r.RowsAffected()
		res.DeletedFindings += n
	}

	// Hard cap: nothing standalone survives past 30 days, regardless of tier
	// settings. Not configurable — a safety net so a warn_crit_days=0 ("keep
	// forever") or a skipped tier can't let rows accumulate indefinitely.
	// Investigation-attached findings are excluded (the evidence-trail rule).
	capCutoff := now.Add(-30 * 24 * time.Hour).Unix()
	r, err := s.d.Store.SQL().ExecContext(ctx,
		`DELETE FROM smith_findings WHERE investigation_id IS NULL AND created_at < ?`,
		capCutoff)
	if err != nil {
		return res, fmt.Errorf("smith: prune findings (30d cap): %w", err)
	}
	n, _ := r.RowsAffected()
	res.DeletedFindings += n

	// Expired-TTL rows always go (this is just correctness, the same check
	// web/cache.go's cachedSearch/cachedDocument already apply at read
	// time — an expired row is never served regardless; pruning it is
	// housekeeping, not a retention "tier").
	if r, err := s.d.Store.SQL().ExecContext(ctx, `DELETE FROM smith_web_cache WHERE expires_at < ?`, now.Unix()); err != nil {
		return res, fmt.Errorf("smith: prune web cache (expired): %w", err)
	} else if n, _ := r.RowsAffected(); n > 0 {
		res.DeletedWebCache += n
	}
	if cfg.WebCacheDays > 0 {
		cutoff := now.Add(-time.Duration(cfg.WebCacheDays) * 24 * time.Hour).Unix()
		r, err := s.d.Store.SQL().ExecContext(ctx, `DELETE FROM smith_web_cache WHERE fetched_at < ?`, cutoff)
		if err != nil {
			return res, fmt.Errorf("smith: prune web cache (age): %w", err)
		}
		n, _ := r.RowsAffected()
		res.DeletedWebCache += n
	}
	if cfg.WebCacheMaxRows > 0 {
		r, err := s.d.Store.SQL().ExecContext(ctx,
			`DELETE FROM smith_web_cache WHERE rowid NOT IN (
				SELECT rowid FROM smith_web_cache ORDER BY fetched_at DESC LIMIT ?)`,
			cfg.WebCacheMaxRows)
		if err != nil {
			return res, fmt.Errorf("smith: prune web cache (row cap): %w", err)
		}
		n, _ := r.RowsAffected()
		res.DeletedWebCache += n
	}

	s.recordRetentionResult(res)
	return res, nil
}

func (s *Smith) recordRetentionResult(res RetentionResult) {
	s.mu.Lock()
	s.lastRetention = res
	s.mu.Unlock()
}

// dedupCritFindings collapses repeated standalone crit findings for the same
// check_id into a single row: the newest survives with repeat_count = the
// summed repeat_count of every member (so a crit recurring across N sweeps
// shows ×N, not N identical rows), the older duplicates are deleted. Run
// after each sweep so the count tracks the recurrence live rather than
// waiting for the 6h prune cycle. Findings attached to an investigation are
// never touched (the evidence-trail rule retention.go's tier pruning follows
// — a dedup is a delete, so the same investigation_id IS NULL guard applies).
func (s *Smith) dedupCritFindings(ctx context.Context) (int64, error) {
	if s.d.Store == nil {
		return 0, nil
	}
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT check_id, COUNT(*), COALESCE(SUM(repeat_count), 0)
		 FROM smith_findings
		 WHERE severity = 'crit' AND investigation_id IS NULL
		 GROUP BY check_id
		 HAVING COUNT(*) > 1`)
	if err != nil {
		return 0, fmt.Errorf("smith: dedup crit (query): %w", err)
	}
	type group struct {
		checkID string
		total   int64
	}
	var groups []group
	for rows.Next() {
		var g group
		var count int64
		if err := rows.Scan(&g.checkID, &count, &g.total); err != nil {
			rows.Close()
			return 0, fmt.Errorf("smith: dedup crit (scan): %w", err)
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("smith: dedup crit (rows): %w", err)
	}

	var deleted int64
	for _, g := range groups {
		var newestID int64
		err := s.d.Store.SQL().QueryRowContext(ctx,
			`SELECT id FROM smith_findings
			 WHERE severity = 'crit' AND investigation_id IS NULL AND check_id = ?
			 ORDER BY created_at DESC, id DESC LIMIT 1`, g.checkID).Scan(&newestID)
		if err != nil {
			return deleted, fmt.Errorf("smith: dedup crit (newest %s): %w", g.checkID, err)
		}
		if _, err := s.d.Store.SQL().ExecContext(ctx,
			`UPDATE smith_findings SET repeat_count = ? WHERE id = ?`, g.total, newestID); err != nil {
			return deleted, fmt.Errorf("smith: dedup crit (update %s): %w", g.checkID, err)
		}
		r, err := s.d.Store.SQL().ExecContext(ctx,
			`DELETE FROM smith_findings
			 WHERE severity = 'crit' AND investigation_id IS NULL AND check_id = ? AND id != ?`,
			g.checkID, newestID)
		if err != nil {
			return deleted, fmt.Errorf("smith: dedup crit (delete %s): %w", g.checkID, err)
		}
		n, _ := r.RowsAffected()
		deleted += n
	}
	return deleted, nil
}

// pruneInfoTier is the frequent info-tier prune run on every sweep (not just
// the 6h retention cycle): with InfoHours defaulting to 1h, leaving info-row
// pruning to the 6h cycle lets info findings pile up to 6h before aging out.
// A single scoped DELETE per sweep keeps the info tier within ~one sweep
// cycle of its configured horizon. Best-effort — logged, never fatal.
func (s *Smith) pruneInfoTier(ctx context.Context) {
	if s.d.Store == nil {
		return
	}
	cfg := s.RetentionConfig(ctx)
	if !cfg.Enabled || cfg.InfoHours <= 0 {
		return
	}
	cutoff := s.d.Now().Add(-time.Duration(cfg.InfoHours) * time.Hour).Unix()
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`DELETE FROM smith_findings WHERE severity = 'info' AND investigation_id IS NULL AND created_at < ?`,
		cutoff); err != nil {
		s.logf("prune info tier: %v", err)
	}
}

// retentionStatus assembles SelfContext.Retention — a pure read of the
// in-memory last-run record, never triggers a prune itself.
func (s *Smith) retentionStatus(ctx context.Context) RetentionStatus {
	s.mu.Lock()
	last := s.lastRetention
	s.mu.Unlock()
	st := RetentionStatus{Enabled: s.RetentionConfig(ctx).Enabled}
	if !last.RanAt.IsZero() {
		ts := last.RanAt.Unix()
		st.LastRunAt = &ts
		st.DeletedFindings = last.DeletedFindings
		st.DeletedWebCache = last.DeletedWebCache
	}
	return st
}
