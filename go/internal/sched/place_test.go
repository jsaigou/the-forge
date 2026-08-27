// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/engine"
)

var testNow = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

// placeCore builds a Core with a fixed clock over the given occupancy and
// idle map. idleFor entries are "how long ago the slot last served
// tokens"; absent = unknown activity.
func placeCore(t *testing.T, eng Engine, slots []string, idleFor map[string]time.Duration) *Core {
	t.Helper()
	src := staticSource(testNow, slots, idleFor)
	return newTestCore(t, eng, src, func(d *Deps) {
		d.Now = func() time.Time { return testNow }
	})
}

func occupyAll(eng *fakeEngine, modes map[string]string) {
	for slot, mode := range modes {
		eng.setOcc(slot, mode)
	}
}

func TestPlaceFreeSlotPreferred(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a1", "gemma")
	c := placeCore(t, eng, eng.Slots(), nil)

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if pl.slot != "a2" || len(pl.evict) != 0 {
		t.Fatalf("place = %+v, want a2 with no evictions", pl)
	}
}

func TestPlaceIdleEvictionThreshold(t *testing.T) {
	cases := []struct {
		name     string
		idleFor  map[string]time.Duration
		wantSlot string
		wantMsg  string
	}{
		{
			name: "idle past threshold is evictable",
			idleFor: map[string]time.Duration{
				"a1": 10 * time.Second, "a2": 181 * time.Second,
				"a3": 10 * time.Second, "a4": 20 * time.Second,
			},
			wantSlot: "a2",
		},
		{
			name: "idle below threshold is not evictable",
			idleFor: map[string]time.Duration{
				"a1": 10 * time.Second, "a2": 179 * time.Second,
				"a3": 10 * time.Second, "a4": 20 * time.Second,
			},
			wantMsg: "No idle, unreserved slot",
		},
		{
			name: "unknown activity is never idle-eligible",
			idleFor: map[string]time.Duration{
				"a1": 10 * time.Second,
				"a3":      10 * time.Second, "a4": 20 * time.Second,
				// a2 absent — zero LastActivity
			},
			wantMsg: "No idle, unreserved slot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := newFakeEngine()
			occupyAll(eng, map[string]string{
				"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
			})
			c := placeCore(t, eng, eng.Slots(), tc.idleFor)

			pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
			if tc.wantSlot != "" {
				if pl.slot != tc.wantSlot {
					t.Fatalf("place = %+v, want slot %s", pl, tc.wantSlot)
				}
				if len(pl.evict) != 1 || pl.evict[0] != tc.wantSlot {
					t.Fatalf("evict = %v, want [%s]", pl.evict, tc.wantSlot)
				}
			} else {
				if pl.slot != "" || !strings.Contains(pl.message, tc.wantMsg) {
					t.Fatalf("place = %+v, want retryable %q", pl, tc.wantMsg)
				}
				if pl.terminal {
					t.Fatal("no-evictable-slot must be retryable, not terminal")
				}
			}
		})
	}
}

// TestPlaceIdleUsesSnapshotTimeNotLiveClock covers the same
// snapshot-vs-live-clock fix as core_test.go's Status test, but for the
// path that actually drives eviction: idleSeconds() in place.go. All four
// slots were idle only 10s as of the snapshot's own capture time — well
// under the 180s threshold — but the live clock has moved on 5 minutes
// (a stalled/delayed collector cycle). Before the fix this would read every
// slot as idle 5m+10s and evict one; the fix keeps eligibility anchored to
// what the snapshot itself actually observed.
func TestPlaceIdleUsesSnapshotTimeNotLiveClock(t *testing.T) {
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{
		"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
	})
	src := staticSource(testNow, eng.Slots(), map[string]time.Duration{
		"a1": 10 * time.Second, "a2": 10 * time.Second,
		"a3": 10 * time.Second, "a4": 10 * time.Second,
	})
	c := newTestCore(t, eng, src, func(d *Deps) {
		d.Now = func() time.Time { return testNow.Add(5 * time.Minute) }
	})

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if pl.slot != "" {
		t.Fatalf("place = %+v, want no evictable slot (all genuinely fresh as of the snapshot)", pl)
	}
}

func TestPlaceSmallestFootprintFirst(t *testing.T) {
	base := newFakeEngine()
	occupyAll(base, map[string]string{
		"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
	})
	eng := &fakeEngineFP{fakeEngine: base, footprints: map[string]float64{
		"a1": 90000, "a2": 40000, "a3": 8000, "a4": 20000,
	}}
	// Everything long idle — footprint is the only discriminator.
	idle := map[string]time.Duration{
		"a1": time.Hour, "a2": time.Hour, "a3": time.Hour, "a4": time.Hour,
	}
	c := placeCore(t, eng, base.Slots(), idle)

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if pl.slot != "a3" || len(pl.evict) != 1 || pl.evict[0] != "a3" {
		t.Fatalf("place = %+v, want smallest-footprint slot a3", pl)
	}
}

func TestPlaceFootprintFallbackUsesSlotOrder(t *testing.T) {
	// Without the Footprints seam and with a Fits-plan (empty Evict), the
	// fallback ordering is engine slot order among eligible slots.
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{
		"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
	})
	idle := map[string]time.Duration{
		"a1": 10 * time.Second, "a2": time.Hour,
		"a3": time.Hour, "a4": time.Hour,
	}
	c := placeCore(t, eng, eng.Slots(), idle)

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if pl.slot != "a2" {
		t.Fatalf("place = %+v, want first eligible in slot order (a2)", pl)
	}
}

func TestPlaceReservationProtection(t *testing.T) {
	activeRes := mustReservation("nightly", "m2", "whole_box", "", HumanIdentity,
		testNow.Add(-time.Hour), testNow.Add(time.Hour))
	soonRes := mustReservation("soon", "m2", "whole_box", "", HumanIdentity,
		testNow.Add(5*time.Minute), testNow.Add(time.Hour))
	laterRes := mustReservation("later", "m2", "whole_box", "", HumanIdentity,
		testNow.Add(30*time.Minute), testNow.Add(time.Hour))

	cases := []struct {
		name         string
		reservations []Reservation
		wantSlot     string
		wantMsg      string
	}{
		{
			name:         "active reservation gives full immunity even when idle",
			reservations: []Reservation{activeRes},
			wantMsg:      "No idle, unreserved slot",
		},
		{
			name:         "soon-starting reservation pre-protects",
			reservations: []Reservation{soonRes},
			wantMsg:      "No idle, unreserved slot",
		},
		{
			name:         "reservation outside the soon window does not protect",
			reservations: []Reservation{laterRes},
			wantSlot:     "a2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := newFakeEngine()
			occupyAll(eng, map[string]string{
				"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
			})
			// Only a2 is idle-eligible; its model m2 carries the
			// reservation under test.
			idle := map[string]time.Duration{
				"a1": 10 * time.Second, "a2": time.Hour,
				"a3": 10 * time.Second, "a4": 10 * time.Second,
			}
			c := placeCore(t, eng, eng.Slots(), idle)

			pl := c.place(context.Background(), "llama", "", DefaultConfig(), tc.reservations, 30*time.Second)
			if tc.wantSlot != "" {
				if pl.slot != tc.wantSlot {
					t.Fatalf("place = %+v, want %s evicted", pl, tc.wantSlot)
				}
			} else if pl.slot != "" || !strings.Contains(pl.message, tc.wantMsg) {
				t.Fatalf("place = %+v, want blocked with %q", pl, tc.wantMsg)
			}
		})
	}
}

func TestPlaceReservationTierForcesBusySlot(t *testing.T) {
	// "llama" holds an active reservation; every slot is busy (recent
	// activity). The reservation tier may evict a busy slot.
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{
		"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
	})
	idle := map[string]time.Duration{
		"a1": 5 * time.Second, "a2": 5 * time.Second,
		"a3": 5 * time.Second, "a4": 5 * time.Second,
	}
	c := placeCore(t, eng, eng.Slots(), idle)

	res := []Reservation{mustReservation("batch", "llama", "whole_box", "", "agent-x",
		testNow.Add(-time.Minute), testNow.Add(time.Hour))}
	pl := c.place(context.Background(), "llama", "", DefaultConfig(), res, 30*time.Second)
	if pl.slot == "" || len(pl.evict) != 1 {
		t.Fatalf("place = %+v, want a forced eviction", pl)
	}
}

func TestPlaceReservationTierPrefersIdleAndSparesActive(t *testing.T) {
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{
		"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
	})
	// a4 idle-eligible; m1 (a1) itself under an active reservation —
	// immune even to forced eviction.
	idle := map[string]time.Duration{
		"a1": time.Hour, "a2": 5 * time.Second,
		"a3": 5 * time.Second, "a4": time.Hour,
	}
	c := placeCore(t, eng, eng.Slots(), idle)

	res := []Reservation{
		mustReservation("batch", "llama", "whole_box", "", "agent-x",
			testNow.Add(-time.Minute), testNow.Add(time.Hour)),
		mustReservation("hold-m1", "m1", "whole_box", "", HumanIdentity,
			testNow.Add(-time.Minute), testNow.Add(time.Hour)),
	}
	pl := c.place(context.Background(), "llama", "", DefaultConfig(), res, 30*time.Second)
	if pl.slot != "a4" {
		t.Fatalf("place = %+v, want idle unprotected slot a4", pl)
	}

	// With every slot's model under an active reservation, nothing moves.
	res2 := []Reservation{res[0]}
	for _, m := range []string{"m1", "m2", "m3", "m4"} {
		res2 = append(res2, mustReservation("hold-"+m, m, "whole_box", "", HumanIdentity,
			testNow.Add(-time.Minute), testNow.Add(time.Hour)))
	}
	pl2 := c.place(context.Background(), "llama", "", DefaultConfig(), res2, 30*time.Second)
	if pl2.slot != "" || !strings.Contains(pl2.message, "protected by active reservations") {
		t.Fatalf("place = %+v, want all-protected refusal", pl2)
	}
	if pl2.terminal {
		t.Fatal("all-protected must be retryable (windows end), not terminal")
	}
}

func TestPlaceMemoryDrivenEviction(t *testing.T) {
	// The engine says llama does not fit and a3 (smallest) must go, even
	// though a1 is a free bay.
	eng := newFakeEngine()
	occupyAll(eng, map[string]string{"a2": "m2", "a3": "m3"})
	eng.plan = engine.Plan{Fits: false, Evict: []string{"a3"}, NeedBytes: 90000 * 1024 * 1024, FreeBytes: 10000 * 1024 * 1024,
		Message: "Needs [a3] evicted to fit"}
	idle := map[string]time.Duration{"a2": 5 * time.Second, "a3": time.Hour}
	c := placeCore(t, eng, eng.Slots(), idle)

	pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if pl.slot != "a1" || len(pl.evict) != 1 || pl.evict[0] != "a3" {
		t.Fatalf("place = %+v, want free bay a1 with memory eviction of a3", pl)
	}

	// Same memory need but a3 is busy: the idle countdown (2m55s to reach
	// the 180s threshold) exceeds the 30s horizon, so S1's bounded-horizon
	// rule fails the request NOW with the reason and ETA instead of
	// silently polling to the deadline.
	idleBusy := map[string]time.Duration{"a2": 5 * time.Second, "a3": 5 * time.Second}
	c2 := placeCore(t, eng, eng.Slots(), idleBusy)
	pl2 := c2.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
	if pl2.slot != "" || !strings.Contains(pl2.message, "not enough VRAM") || strings.Contains(pl2.message, "idle only 0s") || !pl2.terminal {
		t.Fatalf("place = %+v, want terminal not-enough-VRAM refusal with ETA", pl2)
	}
}

func TestPlaceTerminalWhenModelCannotFit(t *testing.T) {
	eng := newFakeEngine()
	eng.plan = engine.Plan{Fits: false, NeedBytes: 200000 * 1024 * 1024, FreeBytes: 100000 * 1024 * 1024,
		Message: "Won't fit even after evicting every loaded slot"}
	c := placeCore(t, eng, eng.Slots(), nil)

	pl := c.place(context.Background(), "huge", "", DefaultConfig(), nil, 30*time.Second)
	if !pl.terminal || pl.slot != "" {
		t.Fatalf("place = %+v, want terminal refusal", pl)
	}

	// Unknown mode rides the same path (FitPlan message, empty evict).
	eng.plan = engine.Plan{Fits: false, Message: "Unknown mode: nope"}
	pl2 := c.place(context.Background(), "nope", "", DefaultConfig(), nil, 30*time.Second)
	if !pl2.terminal || !strings.Contains(pl2.message, "Unknown mode") {
		t.Fatalf("place = %+v, want terminal unknown-mode", pl2)
	}
}

// TestPlaceRefusalReasons locks down the RefusalReason code on a
// representative sample of place()'s refusal paths — a future consumer
// (e.g. an a0 load-status surface) will switch on these codes, so a code
// silently drifting off its documented meaning should fail a test, not
// just look different in a log line.
func TestPlaceRefusalReasons(t *testing.T) {
	t.Run("model too large", func(t *testing.T) {
		eng := newFakeEngine()
		eng.plan = engine.Plan{Fits: false, NeedBytes: 200000 * 1024 * 1024, FreeBytes: 100000 * 1024 * 1024,
			Message: "Won't fit even after evicting every loaded slot"}
		c := placeCore(t, eng, eng.Slots(), nil)

		pl := c.place(context.Background(), "huge", "", DefaultConfig(), nil, 30*time.Second)
		if pl.reason != ReasonModelTooLarge {
			t.Fatalf("reason = %q, want %q", pl.reason, ReasonModelTooLarge)
		}
	})

	t.Run("no evictable idle slot", func(t *testing.T) {
		eng := newFakeEngine()
		occupyAll(eng, map[string]string{
			"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
		})
		idle := map[string]time.Duration{
			"a1": 10 * time.Second, "a2": 179 * time.Second,
			"a3": 10 * time.Second, "a4": 20 * time.Second,
		}
		c := placeCore(t, eng, eng.Slots(), idle)

		pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
		if pl.reason != ReasonNoEvictableIdle {
			t.Fatalf("reason = %q, want %q", pl.reason, ReasonNoEvictableIdle)
		}
	})

	t.Run("activity unknown", func(t *testing.T) {
		eng := newFakeEngine()
		occupyAll(eng, map[string]string{
			"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
		})
		idle := map[string]time.Duration{
			"a1": 10 * time.Second, "a3": 10 * time.Second, "a4": 20 * time.Second,
			// a2 absent — unknown activity. FitPlan needs it evicted so the
			// activity-unknown branch (not the plain idle-threshold one) fires.
		}
		eng.plan = engine.Plan{Fits: false, Evict: []string{"a2"}, NeedBytes: 90000 * 1024 * 1024, FreeBytes: 10000 * 1024 * 1024,
			Message: "Needs [a2] evicted to fit"}
		c := placeCore(t, eng, eng.Slots(), idle)

		pl := c.place(context.Background(), "llama", "", DefaultConfig(), nil, 30*time.Second)
		if pl.reason != ReasonActivityUnknown {
			t.Fatalf("reason = %q, want %q", pl.reason, ReasonActivityUnknown)
		}
		if !pl.terminal {
			t.Fatal("activity-unknown must be terminal — it will never auto-clear")
		}
	})

	t.Run("no evictable slot — all reserved", func(t *testing.T) {
		eng := newFakeEngine()
		occupyAll(eng, map[string]string{
			"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4",
		})
		idle := map[string]time.Duration{
			"a1": 5 * time.Second, "a2": 5 * time.Second,
			"a3": 5 * time.Second, "a4": 5 * time.Second,
		}
		c := placeCore(t, eng, eng.Slots(), idle)
		res := []Reservation{mustReservation("batch", "llama", "whole_box", "", "agent-x",
			testNow.Add(-time.Minute), testNow.Add(time.Hour))}
		for _, m := range []string{"m1", "m2", "m3", "m4"} {
			res = append(res, mustReservation("hold-"+m, m, "whole_box", "", HumanIdentity,
				testNow.Add(-time.Minute), testNow.Add(time.Hour)))
		}

		pl := c.place(context.Background(), "llama", "", DefaultConfig(), res, 30*time.Second)
		if pl.reason != ReasonNoEvictableReserved {
			t.Fatalf("reason = %q, want %q", pl.reason, ReasonNoEvictableReserved)
		}
	})
}

func TestPlacePinnedTargetSlot(t *testing.T) {
	activeRes := func(model string) Reservation {
		return mustReservation("r-"+model, model, "whole_box", "", HumanIdentity,
			testNow.Add(-time.Minute), testNow.Add(time.Hour))
	}
	cases := []struct {
		name         string
		occ          map[string]string
		idleFor      map[string]time.Duration
		reservations []Reservation
		wantSlot     string
		wantEvict    []string
		wantMsg      string
		wantTerminal bool
	}{
		{
			name:      "free pinned slot places immediately",
			occ:       map[string]string{"a1": "m1"},
			wantSlot:  "a3",
			wantEvict: nil,
		},
		{
			// S1: busy pinned slot, idle countdown beyond the 30s horizon →
			// terminal-with-reason instead of a silent poll to the deadline.
			name:         "busy pinned slot refuses under normal tier",
			occ:          map[string]string{"a3": "m3"},
			idleFor:      map[string]time.Duration{"a3": 5 * time.Second},
			wantMsg:      "not enough VRAM yet",
			wantTerminal: true,
		},
		{
			name:      "idle pinned slot is evicted",
			occ:       map[string]string{"a3": "m3"},
			idleFor:   map[string]time.Duration{"a3": time.Hour},
			wantSlot:  "a3",
			wantEvict: []string{"a3"},
		},
		{
			// S1: the active reservation's end lies beyond the 30s horizon →
			// terminal-with-reason.
			name:         "pinned slot under an active reservation is immune",
			occ:          map[string]string{"a3": "m3"},
			idleFor:      map[string]time.Duration{"a3": time.Hour},
			reservations: []Reservation{activeRes("m3")},
			wantMsg:      "protected by an active reservation",
			wantTerminal: true,
		},
		{
			name:         "reservation tier forces the busy pinned slot",
			occ:          map[string]string{"a3": "m3"},
			idleFor:      map[string]time.Duration{"a3": 5 * time.Second},
			reservations: []Reservation{activeRes("llama")},
			wantSlot:     "a3",
			wantEvict:    []string{"a3"},
		},
		{
			name:         "pinned slot soon-reserved model is pre-protected",
			occ:          map[string]string{"a3": "m3"},
			idleFor:      map[string]time.Duration{"a3": time.Hour},
			reservations: []Reservation{mustReservation("soon", "m3", "whole_box", "", HumanIdentity, testNow.Add(5*time.Minute), testNow.Add(time.Hour))},
			// S1: the protection outlives the caller's 30s horizon, so the
			// refusal is terminal-with-reason instead of a silent poll to
			// the deadline.
			wantMsg:      "protected by an upcoming reservation",
			wantTerminal: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := newFakeEngine()
			occupyAll(eng, tc.occ)
			c := placeCore(t, eng, eng.Slots(), tc.idleFor)

			pl := c.place(context.Background(), "llama", "a3", DefaultConfig(), tc.reservations, 30*time.Second)
			if tc.wantMsg != "" {
				if pl.slot != "" || !strings.Contains(pl.message, tc.wantMsg) || pl.terminal != tc.wantTerminal {
					t.Fatalf("place = %+v, want (terminal=%v) %q", pl, tc.wantTerminal, tc.wantMsg)
				}
				return
			}
			if pl.slot != tc.wantSlot {
				t.Fatalf("place = %+v, want slot %s", pl, tc.wantSlot)
			}
			if len(pl.evict) != len(tc.wantEvict) {
				t.Fatalf("evict = %v, want %v", pl.evict, tc.wantEvict)
			}
			for i := range pl.evict {
				if pl.evict[i] != tc.wantEvict[i] {
					t.Fatalf("evict = %v, want %v", pl.evict, tc.wantEvict)
				}
			}
		})
	}
}

func TestFindLoadedScoping(t *testing.T) {
	eng := newFakeEngine()
	eng.setOcc("a2", "llama")
	c := placeCore(t, eng, eng.Slots(), nil)

	if got := c.findLoaded("llama", ""); got != "a2" {
		t.Fatalf("findLoaded unpinned = %q, want a2", got)
	}
	// Pinned to a3: llama loaded elsewhere does not count (a0's fixed
	// backend-to-port mapping).
	if got := c.findLoaded("llama", "a3"); got != "" {
		t.Fatalf("findLoaded pinned = %q, want none", got)
	}
	if got := c.findLoaded("llama", "a2"); got != "a2" {
		t.Fatalf("findLoaded pinned-match = %q, want a2", got)
	}
}
