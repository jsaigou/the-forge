// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"sync/atomic"
	"time"
)

// Source is what every consumer of system state depends on (Contract 2).
// The real collector (Phase 2, track A) implements it with a probe loop;
// other tracks code against it with Static fakes.
type Source interface {
	// Current returns the latest snapshot. Never nil after the collector
	// has completed its first cycle; callers must tolerate a stale
	// snapshot (the collector cadence, default 2s as of Sprint K / was 4s, bounds staleness).
	Current() *Snapshot
}

// Static is a fixed-snapshot Source for tests and for wiring the skeleton.
type Static struct {
	snap atomic.Pointer[Snapshot]
}

// NewStatic returns a Static source holding snap (a minimal empty snapshot
// when snap is nil).
func NewStatic(snap *Snapshot) *Static {
	s := &Static{}
	if snap == nil {
		snap = &Snapshot{
			TakenAt:        time.Now(),
			Units:          map[string]UnitState{},
			Slots:          map[string]SlotState{},
			Inference:      map[string]SlotInference{},
			Ports:          map[int]bool{},
			BookmarkHealth: map[string]bool{},
		}
	}
	s.snap.Store(snap)
	return s
}

// Set replaces the snapshot (test helper).
func (s *Static) Set(snap *Snapshot) { s.snap.Store(snap) }

// Current implements Source.
func (s *Static) Current() *Snapshot { return s.snap.Load() }
