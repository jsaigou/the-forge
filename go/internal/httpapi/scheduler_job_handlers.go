// SPDX-License-Identifier: Apache-2.0

package httpapi

// scheduler_job_handlers.go — P3 scheduler-jobs CRUD + run-now
// (forge/p3sched track). Scheduler jobs are persisted cron expressions that
// the daemon's jobs runner (internal/sched jobs.go) fires through
// sched.EnsureLoaded with requested_by="cron:<name>", forcing a catalog
// config onto a slot at scheduled times (e.g. off-peak batch windows).
//
// Role posture mirrors the neighboring reservation routes: reads + run-now
// are operator, create/update/delete are admin. No requireAssurance gate —
// these are operational scheduling mutations, not security-area changes.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/cron"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// schedulerJobJSON is one scheduler_jobs row on the wire.
type schedulerJobJSON struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Cron       string  `json:"cron"`
	ConfigName string  `json:"config_name"`
	Slot       *string `json:"slot"`
	Enabled    bool    `json:"enabled"`
	LastRunAt  *string `json:"last_run_at"`
	NextRunAt  *string `json:"next_run_at"`
	CreatedBy  *string `json:"created_by"`
	CreatedAt  *string `json:"created_at"`
}

func jobToJSON(j store.SchedulerJob) schedulerJobJSON {
	out := schedulerJobJSON{
		ID:         j.ID,
		Name:       j.Name,
		Cron:       j.Cron,
		ConfigName: j.ConfigName,
		Enabled:    j.Enabled,
	}
	if j.Slot != "" {
		out.Slot = &j.Slot
	}
	if !j.LastRunAt.IsZero() {
		s := isoFormat(j.LastRunAt)
		out.LastRunAt = &s
	}
	if !j.NextRunAt.IsZero() {
		s := isoFormat(j.NextRunAt)
		out.NextRunAt = &s
	}
	if j.CreatedBy != "" {
		s := j.CreatedBy
		out.CreatedBy = &s
	}
	if !j.CreatedAt.IsZero() {
		s := isoFormat(j.CreatedAt)
		out.CreatedAt = &s
	}
	return out
}

// handleSchedulerJobsList returns all scheduler jobs (operator).
func (s *Server) handleSchedulerJobsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.SchedulerJobs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": []schedulerJobJSON{}, "total": 0})
		return
	}
	rows, err := s.deps.SchedulerJobs.List(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	out := []schedulerJobJSON{}
	for _, j := range rows {
		out = append(out, jobToJSON(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "total": len(out)})
}

// schedulerJobBody is the create/update payload. Enabled is *bool so an
// absent field means "default true" on create and "leave as-is" is NOT
// supported on update (PUT is full-replace, matching the reservation PUT).
type schedulerJobBody struct {
	Name       string `json:"name"`
	Cron       string `json:"cron"`
	ConfigName string `json:"config_name"`
	Slot       string `json:"slot"`
	Enabled    *bool  `json:"enabled"`
}

var validSlots = map[string]bool{"a1": true, "a2": true, "a3": true, "a4": true}

func (b schedulerJobBody) validate(cfg *configView) map[string]string {
	fields := map[string]string{}
	if strings.TrimSpace(b.Name) == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 64 {
		fields["name"] = "must be at most 64 characters"
	}
	if strings.TrimSpace(b.Cron) == "" {
		fields["cron"] = "is required"
	} else if _, err := cron.Parse(b.Cron); err != nil {
		fields["cron"] = err.Error()
	}
	if strings.TrimSpace(b.ConfigName) == "" {
		fields["config_name"] = "is required"
	} else if cfg != nil && !cfg.hasConfig(b.ConfigName) {
		fields["config_name"] = fmt.Sprintf("unknown config %q", b.ConfigName)
	}
	if b.Slot != "" && !validSlots[b.Slot] {
		fields["slot"] = "must be one of: a1, a2, a3, a4 (empty = scheduler chooses)"
	}
	return fields
}

// configView is the minimal read seam over Deps.Config for validation.
type configView struct {
	modes map[string]bool
}

func (c *configView) hasConfig(name string) bool { return c.modes[name] }

func (s *Server) configNamesView() *configView {
	if s.deps.Config == nil {
		return nil
	}
	cfg := s.deps.Config()
	if cfg == nil {
		return nil
	}
	v := &configView{modes: map[string]bool{}}
	for name := range cfg.Modes {
		v.modes[name] = true
	}
	return v
}

// nextFireOf parses expr and computes its next fire time after now; zero
// time when unparsable (callers validate first).
func nextFireOf(expr string, now time.Time) time.Time {
	sch, err := cron.Parse(expr)
	if err != nil {
		return time.Time{}
	}
	return sch.Next(now)
}

// handleSchedulerJobCreate creates a job (admin). next_run_at is computed
// here so the runner only ever consumes persisted fire times.
func (s *Server) handleSchedulerJobCreate(w http.ResponseWriter, r *http.Request) {
	var b schedulerJobBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := b.validate(s.configNamesView()); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.SchedulerJobs == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler-jobs store not wired")
		return
	}

	now := time.Now()
	id, err := s.deps.SchedulerJobs.Create(r.Context(), store.SchedulerJob{
		Name:       strings.TrimSpace(b.Name),
		Cron:       strings.TrimSpace(b.Cron),
		ConfigName: b.ConfigName,
		Slot:       b.Slot,
		Enabled:    derefBool(b.Enabled, true),
		CreatedBy:  identity(r).Name,
		CreatedAt:  now,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeValidationError(w, map[string]string{"name": "already exists"})
			return
		}
		writeInternalError(w, err)
		return
	}
	// Best-effort: persist the first fire time; the runner's AdvanceMissed
	// backfills it on restart if this write fails.
	if next := nextFireOf(strings.TrimSpace(b.Cron), now); !next.IsZero() {
		_ = s.deps.SchedulerJobs.SetRunTimes(r.Context(), id, time.Time{}, next)
	}
	job, getErr := s.deps.SchedulerJobs.Get(r.Context(), id)
	if getErr != nil {
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id})
		return
	}
	s.audit(r, identity(r).Name, "scheduler_job_create", b.Name, "")
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id, "job": jobToJSON(job)})
}

// handleSchedulerJobUpdate full-replaces a job's definition (admin). The
// cron/config/slot/enabled fields are replaceable; run times are recomputed.
func (s *Server) handleSchedulerJobUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := jobIDOf(w, r)
	if !ok {
		return
	}
	var b schedulerJobBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := b.validate(s.configNamesView()); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.deps.SchedulerJobs == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler-jobs store not wired")
		return
	}
	ctx := r.Context()
	existing, err := s.deps.SchedulerJobs.Get(ctx, id)
	if err != nil {
		writeJobStoreError(w, err)
		return
	}
	existing.Name = strings.TrimSpace(b.Name)
	existing.Cron = strings.TrimSpace(b.Cron)
	existing.ConfigName = b.ConfigName
	existing.Slot = b.Slot
	existing.Enabled = derefBool(b.Enabled, true)
	if err := s.deps.SchedulerJobs.Update(ctx, existing); err != nil {
		if isUniqueViolation(err) {
			writeValidationError(w, map[string]string{"name": "already exists"})
			return
		}
		writeInternalError(w, err)
		return
	}
	if next := nextFireOf(existing.Cron, time.Now()); !next.IsZero() {
		_ = s.deps.SchedulerJobs.SetRunTimes(ctx, id, existing.LastRunAt, next)
	}
	job, _ := s.deps.SchedulerJobs.Get(ctx, id)
	s.audit(r, identity(r).Name, "scheduler_job_update", existing.Name, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": jobToJSON(job)})
}

// handleSchedulerJobDelete removes a job (admin).
func (s *Server) handleSchedulerJobDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := jobIDOf(w, r)
	if !ok {
		return
	}
	if s.deps.SchedulerJobs == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler-jobs store not wired")
		return
	}
	if err := s.deps.SchedulerJobs.Delete(r.Context(), id); err != nil {
		writeJobStoreError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "scheduler_job_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSchedulerJobRunNow fires a job immediately (operator): the same
// EnsureLoaded call the runner makes — requested_by="cron:<name>" plus a
// "-manual" suffix for attribution — with last_run_at/next_run_at advanced
// exactly like a scheduled fire. Runs synchronously (EnsureLoaded blocks up
// to its default timeout); operators watching the queue see it live.
func (s *Server) handleSchedulerJobRunNow(w http.ResponseWriter, r *http.Request) {
	id, ok := jobIDOf(w, r)
	if !ok {
		return
	}
	if s.deps.SchedulerJobs == nil || s.deps.Sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	job, err := s.deps.SchedulerJobs.Get(ctx, id)
	if err != nil {
		writeJobStoreError(w, err)
		return
	}
	ticket, err := s.deps.Sched.EnsureLoaded(ctx, sched.EnsureRequest{
		Model:       job.ConfigName,
		RequestedBy: sched.CronRequestedBy(job.Name) + "-manual",
		TargetSlot:  job.Slot,
	})
	resp := map[string]any{"ok": err == nil}
	if ticket.TargetSlot != "" {
		resp["slot"] = ticket.TargetSlot
	}
	if ticket.Status != "" {
		resp["status"] = ticket.Status
	}
	if err != nil {
		resp["message"] = err.Error()
	} else {
		s.audit(r, identity(r).Name, "scheduler_job_run_now", job.Name, ticket.TargetSlot)
	}
	// Record the manual fire even when the load failed — same semantics as
	// the runner (a failed fire must not hot-loop).
	next := nextFireOf(job.Cron, time.Now())
	_ = s.deps.SchedulerJobs.SetRunTimes(ctx, id, time.Now(), next)
	writeJSON(w, http.StatusOK, resp)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func jobIDOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return 0, false
	}
	return id, true
}

func writeJobStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeInternalError(w, err)
}

// isUniqueViolation reports whether err is SQLite's UNIQUE-constraint
// failure (scheduler_jobs.name UNIQUE).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
