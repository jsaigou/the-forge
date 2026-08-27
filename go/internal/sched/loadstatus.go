// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"sync"
	"time"
)

// outcomeRing remembers each model's most recent terminal EnsureLoaded
// outcome for a short window after the call returns (Sprint 1, a0 load
// visibility, 2026-08-27) — the live queue only holds ACTIVE tickets;
// dequeue (queue.go) deletes a ticket's row on every return path, success
// or failure, so without this a poll arriving just after a failure would
// see nothing and report "idle", losing the one genuinely useful signal
// (why it failed). Bounded by count (evict oldest) and by age (outcomeTTL)
// so a stale failure never masquerades as the current state once enough
// time has passed that the original caller has certainly already gotten
// its own answer and moved on.
const (
	outcomeRingCap = 32
	outcomeTTL     = 10 * time.Minute
)

type outcomeRecord struct {
	status  string
	reason  RefusalReason
	message string
	slot    string
	at      time.Time
}

type outcomeRing struct {
	mu      sync.Mutex
	order   []string // insertion order of live keys, oldest first
	entries map[string]outcomeRecord
}

func newOutcomeRing() *outcomeRing {
	return &outcomeRing{entries: map[string]outcomeRecord{}}
}

func (r *outcomeRing) record(model string, rec outcomeRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[model]; !exists {
		r.order = append(r.order, model)
		if len(r.order) > outcomeRingCap {
			oldest := r.order[0]
			r.order = r.order[1:]
			delete(r.entries, oldest)
		}
	}
	r.entries[model] = rec
}

// clear drops any remembered outcome for model — called on a fresh success
// so a later poll doesn't resurface a now-stale prior failure.
func (r *outcomeRing) clear(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[model]; !exists {
		return
	}
	delete(r.entries, model)
	for i, k := range r.order {
		if k == model {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

func (r *outcomeRing) get(model string, now time.Time) (outcomeRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.entries[model]
	if !ok || now.Sub(rec.at) > outcomeTTL {
		return outcomeRecord{}, false
	}
	return rec, true
}

// LoadStatus answers "what is model doing right now" without blocking or
// mutating scheduler state — see sched.Scheduler's doc comment. Resolution
// order: currently loaded (authoritative, always wins) > actively
// queued/loading (scanned from the live in-memory queue — a ticket is only
// ever "queued" or "loading" while still enqueued, since a terminal status
// change and the deferred dequeue in EnsureLoaded happen together) > the
// last terminal outcome remembered in the short-lived ring > idle (never
// requested, or too long ago to still be relevant).
func (c *Core) LoadStatus(model string) LoadState {
	if slot := c.findLoaded(model, ""); slot != "" {
		return LoadState{Model: model, State: StatusLoaded, Slot: slot}
	}

	c.mu.Lock()
	for i, t := range c.queue {
		if t.Model != model {
			continue
		}
		enqueuedAt := t.EnqueuedAt
		ls := LoadState{
			Model:         model,
			State:         t.Status,
			QueuePosition: i + 1,
			Slot:          t.TargetSlot,
			Reason:        t.Reason,
			Message:       t.Message,
			Since:         &enqueuedAt,
		}
		c.mu.Unlock()
		return ls
	}
	c.mu.Unlock()

	if rec, ok := c.outcomes.get(model, c.d.Now()); ok {
		at := rec.at
		return LoadState{
			Model:   model,
			State:   rec.status,
			Slot:    rec.slot,
			Reason:  rec.reason,
			Message: rec.message,
			Since:   &at,
		}
	}

	return LoadState{Model: model, State: "idle"}
}
