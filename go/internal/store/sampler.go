// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"
)

// RunSampler ticks samples into m until ctx is done (Sprint 0 §0.4
// background sampler; default interval 60s via metrics.sample_interval_s —
// the caller owns interval selection and hands in the ticker channel so
// tests can inject a synthetic one instead of waiting on real time).
//
// sample builds one MetricSample per tick (typically reading the latest
// collector snapshot — store cannot import collector, so the callback is
// the seam). A sample or write failure is reported to onErr (nil-safe, may
// be nil) and the loop continues — a missed tick must never stop the
// sampler or crash the daemon.
func RunSampler(
	ctx context.Context,
	tick <-chan time.Time,
	m Metrics,
	sample func(ctx context.Context) MetricSample,
	onErr func(error),
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			s := sample(ctx)
			if err := m.RecordSample(ctx, s); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// RunSlotStateSync periodically corrects the slot_state crash-recovery
// table (Sched.SaveSlot's own doc: "restart recovery only — in-memory
// state is authoritative at runtime") when a slot's process has died
// outside a tracked Load/Switch/Unload call — a crash, an OOM, or the
// killLingering cross-slot collateral-kill bug fixed 2026-07-29 — and the
// table was never told the slot is now empty. Found live: after that bug
// silently killed sibling slots for who knows how long, `slot_state` rows
// stayed stuck showing modes that hadn't actually been running for days.
//
// Deliberately one-directional: only clears a stale "loaded" row to match
// live reality, never invents a "loaded" state the table doesn't already
// half-agree with and never touches loaded_at on a row that's already
// correct. The opposite gap (table says empty, a slot is actually loaded)
// self-heals via the engine's own sysconfig-inferred reconciliation at
// startup (SlotStates' "unit ACTIVE but untracked" case), so it doesn't
// need this loop too.
//
// liveSlots returns the current reconciled slot->mode map ("" = empty) —
// typically the latest collector snapshot, same source the dashboard
// reads. A tick failure is reported to onErr (nil-safe) and the loop
// continues — a missed tick must never stop the sync or crash the daemon.
func RunSlotStateSync(
	ctx context.Context,
	tick <-chan time.Time,
	sched Sched,
	liveSlots func() map[string]string,
	onErr func(error),
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			recorded, err := sched.Slots(ctx)
			if err != nil {
				if onErr != nil {
					onErr(err)
				}
				continue
			}
			for slot, live := range liveSlots() {
				if live == "" && recorded[slot] != "" {
					if err := sched.SaveSlot(ctx, slot, "", time.Time{}); err != nil && onErr != nil {
						onErr(err)
					}
				}
			}
		}
	}
}

// RunRetention ticks a prune of rows older than retentionDays() until ctx is
// done (Sprint 0 §0.4: 90-day default, daily cadence). retentionDays is
// resolved fresh each tick so a live settings change (metrics.retention_days)
// takes effect without a restart; days <= 0 skips the tick (retention
// disabled) rather than pruning everything. now defaults to time.Now.
func RunRetention(
	ctx context.Context,
	tick <-chan time.Time,
	m Metrics,
	retentionDays func() int,
	now func() time.Time,
	onErr func(error),
) {
	if now == nil {
		now = time.Now
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			days := retentionDays()
			if days <= 0 {
				continue
			}
			cutoff := now().Add(-time.Duration(days) * 24 * time.Hour)
			if _, err := m.Prune(ctx, cutoff); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
