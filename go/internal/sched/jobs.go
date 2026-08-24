// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"errors"
	"time"

	"github.com/jsaigou/the-forge/internal/cron"
	"github.com/jsaigou/the-forge/internal/store"
)

// jobs.go — P3 scheduler-jobs runner (forge/p3sched track): a 30s tick loop
// that fires enabled scheduler_jobs rows whose next_run_at has passed by
// calling EnsureLoaded (requested_by="cron:<name>"), then advancing
// last_run_at/next_run_at. Started from cmd/forge/main.go next to the
// compressor reconcile goroutine; graceful shutdown via ctx cancellation.
//
// Missed runs are skipped, never replayed: while the daemon is down a job's
// next_run_at can lapse, and on startup AdvanceMissed simply recomputes the
// following fire time from "now" without firing — an operator scheduling an
// off-peak batch window does not want a stampede of every missed slot load
// queued at boot.

// JobsTick is the runner's polling resolution. A cron minute-granularity
// schedule therefore fires within ~30s of its wall-clock time.
const JobsTick = 30 * time.Second

// CronRequestedBy is the requested_by prefix for cron-fired loads. The full
// identity is "cron:<job name>" so queue tickets and reservations attribute
// back to the specific job.
func CronRequestedBy(jobName string) string { return "cron:" + jobName }

// JobsDeps wires the runner. Ensure and Jobs are required.
type JobsDeps struct {
	// Ensure is the same entry point a0/MCP/dashboard share — normally
	// Core.EnsureLoaded; fakes in tests. The runner pins SmallJob=false /
	// Priority=0: a scheduled batch window is ordinary-priority work.
	Ensure func(ctx context.Context, req EnsureRequest) (Ticket, error)

	// Jobs is the persisted scheduler_jobs surface.
	Jobs store.SchedulerJobs

	// Now/Logf are test knobs; zero values mean time.Now / no logging.
	Now  func() time.Time
	Logf func(format string, args ...any)
}

type jobsRunner struct {
	d JobsDeps
}

// NewJobsRunner builds the scheduler-jobs runner.
func NewJobsRunner(deps JobsDeps) (*jobsRunner, error) {
	if deps.Ensure == nil {
		return nil, errors.New("sched: JobsDeps.Ensure is required")
	}
	if deps.Jobs == nil {
		return nil, errors.New("sched: JobsDeps.Jobs is required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	return &jobsRunner{d: deps}, nil
}

// AdvanceMissed reconciles persisted fire times with reality at startup:
// any enabled job whose next_run_at already lapsed (daemon was down, or the
// row was created with next_run_at unset) gets next_run_at recomputed from
// now WITHOUT firing. Called once before Run.
func (r *jobsRunner) AdvanceMissed(ctx context.Context) {
	now := r.d.Now()
	jobs, err := r.d.Jobs.List(ctx)
	if err != nil {
		r.d.Logf("sched: jobs advance-missed list: %v", err)
		return
	}
	for _, j := range jobs {
		if !j.Enabled || j.NextRunAt.IsZero() || j.NextRunAt.After(now) {
			continue
		}
		sch, err := cron.Parse(j.Cron)
		if err != nil {
			r.d.Logf("sched: job %s: unparsable cron %q: %v", j.Name, j.Cron, err)
			continue
		}
		next := sch.Next(now)
		if err := r.d.Jobs.SetRunTimes(ctx, j.ID, j.LastRunAt, next); err != nil {
			r.d.Logf("sched: job %s: advance next_run_at: %v", j.Name, err)
			continue
		}
		r.d.Logf("sched: job %s: skipped missed run(s), next run %s", j.Name, next.Format(time.RFC3339))
	}
}

// Run blocks until ctx is done, ticking every JobsTick and firing due jobs.
func (r *jobsRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(JobsTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tickOnce(ctx, r.d.Now())
		}
	}
}

func (r *jobsRunner) tickOnce(ctx context.Context, now time.Time) {
	jobs, err := r.d.Jobs.List(ctx)
	if err != nil {
		r.d.Logf("sched: jobs tick list: %v", err)
		return
	}
	for _, j := range jobs {
		if !j.Enabled || j.NextRunAt.IsZero() || j.NextRunAt.After(now) {
			continue
		}
		r.Fire(ctx, j, now)
	}
}

// Fire force-loads one job's config through EnsureLoaded and records the
// outcome + recomputed next fire time. Exported so a future manual-trigger
// path (run-now) can reuse exactly the tick loop's firing semantics; the
// loop calls it from tickOnce.
func (r *jobsRunner) Fire(ctx context.Context, j store.SchedulerJob, now time.Time) {
	next := computeNext(r.d.Logf, j, now)

	ticket, err := r.d.Ensure(ctx, EnsureRequest{
		Model:       j.ConfigName,
		RequestedBy: CronRequestedBy(j.Name),
		TargetSlot:  j.Slot,
	})
	if err != nil {
		r.d.Logf("sched: cron job %s: ensure_loaded %s (slot %q): %v", j.Name, j.ConfigName, j.Slot, err)
	} else {
		r.d.Logf("sched: cron job %s: loaded %s onto %s", j.Name, ticket.Model, ticket.TargetSlot)
	}
	if err := r.d.Jobs.SetRunTimes(ctx, j.ID, now, next); err != nil {
		r.d.Logf("sched: cron job %s: record run times: %v", j.Name, err)
	}
}

// computeNext derives the following fire time from now, falling back to a
// far-future sentinel when the expression matches nothing (so a broken cron
// string never hot-loops); the parse itself was validated at create/update
// time and re-checked here defensively.
func computeNext(logf func(string, ...any), j store.SchedulerJob, now time.Time) time.Time {
	sch, err := cron.Parse(j.Cron)
	if err != nil {
		logf("sched: cron job %s: unparsable cron %q: %v", j.Name, j.Cron, err)
		return now.AddDate(100, 0, 0)
	}
	next := sch.Next(now)
	if next.IsZero() {
		logf("sched: cron job %s: cron %q has no future fire time", j.Name, j.Cron)
		return now.AddDate(100, 0, 0)
	}
	return next
}
