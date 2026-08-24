// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeJobs is an in-memory store.SchedulerJobs for runner tests.
type fakeJobs struct {
	rows  map[int64]store.SchedulerJob
	seq   int64
	calls []int64 // SetRunTimes call order
}

func newFakeJobs(jobs ...store.SchedulerJob) *fakeJobs {
	f := &fakeJobs{rows: map[int64]store.SchedulerJob{}}
	for _, j := range jobs {
		f.seq++
		if j.ID == 0 {
			j.ID = f.seq
		}
		f.rows[j.ID] = j
	}
	return f
}

func (f *fakeJobs) List(context.Context) ([]store.SchedulerJob, error) {
	var out []store.SchedulerJob
	for _, j := range f.rows {
		out = append(out, j)
	}
	return out, nil
}

func (f *fakeJobs) Get(_ context.Context, id int64) (store.SchedulerJob, error) {
	j, ok := f.rows[id]
	if !ok {
		return store.SchedulerJob{}, store.ErrNotFound
	}
	return j, nil
}

func (f *fakeJobs) Create(_ context.Context, j store.SchedulerJob) (int64, error) {
	f.seq++
	j.ID = f.seq
	f.rows[j.ID] = j
	return j.ID, nil
}

func (f *fakeJobs) Update(_ context.Context, j store.SchedulerJob) error {
	f.rows[j.ID] = j
	return nil
}

func (f *fakeJobs) Delete(_ context.Context, id int64) error {
	delete(f.rows, id)
	return nil
}

func (f *fakeJobs) SetRunTimes(_ context.Context, id int64, last, next time.Time) error {
	j := f.rows[id]
	j.LastRunAt, j.NextRunAt = last, next
	f.rows[id] = j
	f.calls = append(f.calls, id)
	return nil
}

// ensureSpy records EnsureRequest calls; optionally fails.
type ensureSpy struct {
	reqs    []EnsureRequest
	failFor string
	ticket  Ticket
}

func (e *ensureSpy) ensure(_ context.Context, req EnsureRequest) (Ticket, error) {
	e.reqs = append(e.reqs, req)
	if req.Model == e.failFor {
		return Ticket{Status: StatusFailed}, errFakeEnsure
	}
	return e.ticket, nil
}

var errFakeEnsure = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake ensure failure" }

func baseTime() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 0, time.Local) }

func TestRunnerFiresDueJobs(t *testing.T) {
	now := baseTime()
	spy := &ensureSpy{ticket: Ticket{Model: "qwen3", TargetSlot: "a1", Status: StatusLoaded}}
	jobs := newFakeJobs(
		store.SchedulerJob{Enabled: true, Name: "nightly", Cron: "0 3 * * *", ConfigName: "qwen3", Slot: "a1",
			NextRunAt: now.Add(-time.Second)},
		store.SchedulerJob{Enabled: true, Name: "later", Cron: "0 4 * * *", ConfigName: "gemma",
			NextRunAt: now.Add(time.Hour)}, // not due
		store.SchedulerJob{Name: "disabled", Cron: "* * * * *", ConfigName: "mistral",
			Enabled: false, NextRunAt: now.Add(-time.Hour)}, // disabled — never fires
	)
	r, err := NewJobsRunner(JobsDeps{
		Ensure: spy.ensure, Jobs: jobs,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewJobsRunner: %v", err)
	}
	r.tickOnce(context.Background(), now)

	if len(spy.reqs) != 1 {
		t.Fatalf("ensure calls = %d (%+v), want 1 for the single due job", len(spy.reqs), spy.reqs)
	}
	req := spy.reqs[0]
	if req.Model != "qwen3" || req.TargetSlot != "a1" {
		t.Errorf("request = %+v, want model qwen3 on slot a1", req)
	}
	if req.RequestedBy != "cron:nightly" {
		t.Errorf("requested_by = %q, want cron:nightly", req.RequestedBy)
	}

	fired := jobs.rows[1]
	if fired.LastRunAt != now {
		t.Errorf("last_run_at = %v, want %v", fired.LastRunAt, now)
	}
	wantNext := time.Date(2026, 8, 26, 3, 0, 0, 0, time.Local) // next 03:00 is tomorrow
	if !fired.NextRunAt.Equal(wantNext) {
		t.Errorf("next_run_at = %v, want %v", fired.NextRunAt, wantNext)
	}
	if len(jobs.calls) != 1 || jobs.calls[0] != 1 {
		t.Errorf("SetRunTimes calls = %v, want [1]", jobs.calls)
	}
	// Not-due and disabled rows untouched.
	if !jobs.rows[2].LastRunAt.IsZero() || !jobs.rows[3].LastRunAt.IsZero() {
		t.Error("non-due/disabled jobs must not be fired")
	}
}

func TestRunnerRecordsFailureAndStillAdvances(t *testing.T) {
	now := baseTime()
	spy := &ensureSpy{failFor: "qwen3"}
	jobs := newFakeJobs(store.SchedulerJob{Enabled: true, Name: "nightly", Cron: "*/10 * * * *", ConfigName: "qwen3",
		NextRunAt: now.Add(-time.Minute)})
	r, _ := NewJobsRunner(JobsDeps{
		Ensure: spy.ensure, Jobs: jobs,
		Now:  func() time.Time { return now },
		Logf: func(string, ...any) {},
	})
	r.tickOnce(context.Background(), now)

	fired := jobs.rows[1]
	wantNext := now.Add(10 * time.Minute)
	if fired.LastRunAt.IsZero() {
		t.Error("failed fire must still record last_run_at")
	}
	if !fired.NextRunAt.Equal(wantNext) {
		t.Errorf("next_run_at = %v, want %v (failure must not hot-loop the job)", fired.NextRunAt, wantNext)
	}
}

func TestAdvanceMissedSkipsWithoutFiring(t *testing.T) {
	now := baseTime()
	spy := &ensureSpy{}
	jobs := newFakeJobs(store.SchedulerJob{Enabled: true, Name: "stale", Cron: "0 2 * * *", ConfigName: "qwen3",
		NextRunAt: now.Add(-8 * time.Hour)}) // lapsed while daemon was down
	r, _ := NewJobsRunner(JobsDeps{
		Ensure: spy.ensure, Jobs: jobs,
		Now:  func() time.Time { return now },
		Logf: func(string, ...any) {},
	})
	r.AdvanceMissed(context.Background())

	if len(spy.reqs) != 0 {
		t.Errorf("advance-missed fired %d loads, want 0 (missed runs are skipped)", len(spy.reqs))
	}
	got := jobs.rows[1]
	wantNext := time.Date(2026, 8, 26, 2, 0, 0, 0, time.Local)
	if !got.NextRunAt.Equal(wantNext) {
		t.Errorf("next_run_at = %v, want advanced to %v", got.NextRunAt, wantNext)
	}
	if !got.LastRunAt.IsZero() {
		t.Errorf("last_run_at = %v, want untouched zero", got.LastRunAt)
	}
}

func TestNewJobsRunnerValidation(t *testing.T) {
	if _, err := NewJobsRunner(JobsDeps{}); err == nil {
		t.Error("nil Ensure should be rejected")
	}
	if _, err := NewJobsRunner(JobsDeps{Ensure: func(context.Context, EnsureRequest) (Ticket, error) {
		return Ticket{}, nil
	}}); err == nil {
		t.Error("nil Jobs should be rejected")
	}
}
