// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// scheduler_jobs.go — P3 scheduler-jobs surface (forge/p3sched track): the
// persisted cron-style forced-load definitions the daemon's jobs runner
// fires through sched.EnsureLoaded. Conventions differ from most tables in
// one deliberate way: last_run_at/next_run_at are REAL unix seconds (the
// runner stores sub-minute fire times), per migration 0066. Zero
// time.Time maps to NULL on both (never fired / not yet scheduled).

// SchedulerJob is one cron-style forced-load job row.
type SchedulerJob struct {
	ID         int64
	Name       string
	Cron       string
	ConfigName string
	Slot       string // "" = scheduler-chosen slot
	Enabled    bool
	LastRunAt  time.Time // zero = never fired
	NextRunAt  time.Time // zero = unscheduled
	CreatedBy  string
	CreatedAt  time.Time
}

type SchedulerJobs interface {
	List(ctx context.Context) ([]SchedulerJob, error)
	Get(ctx context.Context, id int64) (SchedulerJob, error)
	Create(ctx context.Context, j SchedulerJob) (int64, error)
	Update(ctx context.Context, j SchedulerJob) error
	Delete(ctx context.Context, id int64) error
	// SetRunTimes persists the post-fire bookkeeping: LastRunAt = when it
	// fired, NextRunAt = recomputed following fire time.
	SetRunTimes(ctx context.Context, id int64, lastRun, nextRun time.Time) error
}

type schedJobsView struct{ d *DB }

// SchedulerJobs returns the scheduler-jobs CRUD surface.
func (d *DB) SchedulerJobs() SchedulerJobs { return schedJobsView{d} }

const schedJobColumns = `id, name, cron, config_name, slot, enabled,
	last_run_at, next_run_at, created_by, created_at`

func realOf(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return float64(t.UnixNano()) / 1e9
}

func timeOfReal(f sql.NullFloat64) time.Time {
	if !f.Valid || f.Float64 == 0 {
		return time.Time{}
	}
	sec := math.Floor(f.Float64)
	nsec := int64((f.Float64 - sec) * 1e9)
	return time.Unix(int64(sec), nsec).UTC()
}

func scanSchedJob(scan func(dest ...any) error) (SchedulerJob, error) {
	var j SchedulerJob
	var slot, createdBy sql.NullString
	var lastRun, nextRun, createdAt sql.NullFloat64
	if err := scan(&j.ID, &j.Name, &j.Cron, &j.ConfigName, &slot, &j.Enabled,
		&lastRun, &nextRun, &createdBy, &createdAt); err != nil {
		return SchedulerJob{}, err
	}
	j.Slot = strOf(slot)
	j.CreatedBy = strOf(createdBy)
	j.LastRunAt = timeOfReal(lastRun)
	j.NextRunAt = timeOfReal(nextRun)
	j.CreatedAt = timeOfReal(createdAt)
	return j, nil
}

func (v schedJobsView) List(ctx context.Context) ([]SchedulerJob, error) {
	rows, err := v.d.sql.QueryContext(ctx, `SELECT `+schedJobColumns+` FROM scheduler_jobs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: scheduler_jobs.list: %w", err)
	}
	defer rows.Close()
	var out []SchedulerJob
	for rows.Next() {
		j, err := scanSchedJob(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scheduler_jobs.list: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scheduler_jobs.list: %w", err)
	}
	return out, nil
}

func (v schedJobsView) Get(ctx context.Context, id int64) (SchedulerJob, error) {
	row := v.d.sql.QueryRowContext(ctx,
		`SELECT `+schedJobColumns+` FROM scheduler_jobs WHERE id = ?`, id)
	j, err := scanSchedJob(row.Scan)
	if err == sql.ErrNoRows {
		return SchedulerJob{}, ErrNotFound
	}
	if err != nil {
		return SchedulerJob{}, fmt.Errorf("store: scheduler_jobs.get: %w", err)
	}
	return j, nil
}

func (v schedJobsView) Create(ctx context.Context, j SchedulerJob) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO scheduler_jobs (name, cron, config_name, slot, enabled, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		j.Name, j.Cron, j.ConfigName, nullStr(j.Slot), boolInt(j.Enabled),
		nullStr(j.CreatedBy), realOf(orNow(j.CreatedAt)),
	)
	if err != nil {
		return 0, fmt.Errorf("store: scheduler_jobs.create: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: scheduler_jobs.create: %w", err)
	}
	return id, nil
}

func (v schedJobsView) Update(ctx context.Context, j SchedulerJob) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE scheduler_jobs SET name = ?, cron = ?, config_name = ?, slot = ?, enabled = ?
		 WHERE id = ?`,
		j.Name, j.Cron, j.ConfigName, nullStr(j.Slot), boolInt(j.Enabled), j.ID,
	)
	if err != nil {
		return fmt.Errorf("store: scheduler_jobs.update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v schedJobsView) Delete(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM scheduler_jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: scheduler_jobs.delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v schedJobsView) SetRunTimes(ctx context.Context, id int64, lastRun, nextRun time.Time) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE scheduler_jobs SET last_run_at = ?, next_run_at = ? WHERE id = ?`,
		realOf(lastRun), realOf(nextRun), id,
	)
	if err != nil {
		return fmt.Errorf("store: scheduler_jobs.set_run_times: %w", err)
	}
	return nil
}
