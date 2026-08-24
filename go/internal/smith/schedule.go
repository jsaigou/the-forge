// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"time"
)

// scheduleResolution is the periodic-loop tick. The loop re-reads
// smith.schedule every tick and fires a sweep when quick/deep is due, so
// settings edits take effect within one tick without a restart.
const scheduleResolution = 1 * time.Minute

// Start begins the periodic sweep scheduler (docs/v5-smith.md §4.2
// "Periodic": quick sweep hourly, deep sweep daily by default, both
// disable-able via smith.schedule). Idempotent — a second Start while one is
// running is a no-op. The loop stops when ctx is done or Stop is called.
//
// Start never blocks; it is safe to call with partial deps — sweeps degrade
// instead of failing. Sweeps run on the provided context, NOT the loop's
// context, so a slow scheduled sweep isn't torn down by the caller.
//
// P2 additions: bgCtx is captured here so an approved action's executor
// (execute.go's executeAction, kicked off from ApproveAction) has a
// long-lived context that outlives the HTTP request but is still cancelled
// on Stop(); and resumeProcedureRuns/reconcileExecuting/
// reconcileOpenAnomalies all run once, synchronously, before the periodic
// loop begins — resumeProcedureRuns first, so any action a previous process
// instance left mid-procedure is already resuming in the background before
// reconcileExecuting's wall-clock reap (which deliberately excludes
// kind=procedure rows) even looks at the table; reconcileOpenAnomalies
// rebuilds the anomaly-investigation dedupe map.
func (s *Smith) Start(ctx context.Context) {
	s.mu.Lock()
	if s.stopSchedule != nil {
		s.mu.Unlock()
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	s.stopSchedule = cancel
	s.bgCtx = cctx
	s.mu.Unlock()
	s.resumeProcedureRuns(cctx)
	s.reconcileExecuting(cctx)
	s.reconcileOpenAnomalies(cctx)
	go s.ProbeWeb(cctx) // P5: one-shot reachability probe; must not block Start
	go s.scheduleLoop(cctx)
	s.startAnomalyHook(cctx)
}

// Stop halts the periodic sweep scheduler. Idempotent.
func (s *Smith) Stop() {
	s.mu.Lock()
	stop := s.stopSchedule
	s.stopSchedule = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// scheduleLoop ticks on scheduleResolution, firing quick/deep sweeps when
// due per the live smith.schedule settings.
func (s *Smith) scheduleLoop(ctx context.Context) {
	ticker := time.NewTicker(scheduleResolution)
	defer ticker.Stop()
	var lastQuick, lastDeep time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.maybeProbeWeb(ctx, now)
			s.maybePrune(ctx, now)               // P7 — retention.go, same shape as maybeProbeWeb
			s.maybeSelfReview(ctx, now)          // Thread C — self_review.go, same shape as maybePrune
			s.maybeEnsureBrainResident(ctx, now) // brain_residency.go, same shape as maybeSelfReview
			due, ok := nextDue(s.Schedule(ctx), lastQuick, lastDeep, now)
			if !ok {
				continue
			}
			switch due {
			case ScopeDeep:
				lastDeep, lastQuick = now, now // a deep sweep covers the quick one
				if _, err := s.RunChecks(ctx, ScopeDeep, nil, SweepScheduled); err != nil {
					s.logf("scheduled deep sweep: %v", err)
				}
			case ScopeQuick:
				lastQuick = now
				if _, err := s.RunChecks(ctx, ScopeQuick, nil, SweepScheduled); err != nil {
					s.logf("scheduled quick sweep: %v", err)
				}
			}
		}
	}
}

// nextDue reports which sweep kind is due (ScopeDeep wins when both are), or
// ok=false when scheduling is disabled or nothing is due yet. Pure — the
// scheduleLoop's decision table, extracted for table tests.
func nextDue(sched Schedule, lastQuick, lastDeep, now time.Time) (string, bool) {
	if !sched.Enabled {
		return "", false
	}
	deepEvery := parseDurationOrZero(sched.Deep)
	if deepEvery > 0 && (lastDeep.IsZero() || now.Sub(lastDeep) >= deepEvery) {
		return ScopeDeep, true
	}
	quickEvery := parseDurationOrZero(sched.Quick)
	if quickEvery > 0 && (lastQuick.IsZero() || now.Sub(lastQuick) >= quickEvery) {
		return ScopeQuick, true
	}
	return "", false
}
