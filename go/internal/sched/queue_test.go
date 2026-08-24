// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"testing"
	"time"
)

// queueSpec describes one pre-existing ticket for the table tests:
// small?, current jumpedCount.
type queueSpec struct {
	id     string
	small  bool
	jumped int
}

func buildQueue(c *Core, specs []queueSpec) map[string]*ticket {
	byID := map[string]*ticket{}
	for _, s := range specs {
		t := &ticket{Ticket: Ticket{TicketID: s.id, Model: s.id, Status: StatusQueued, SmallJob: s.small, EnqueuedAt: time.Now()}}
		t.jumpedCount = s.jumped
		c.queue = append(c.queue, t)
		byID[s.id] = t
	}
	return byID
}

func queueIDs(c *Core) []string {
	out := make([]string, 0, len(c.queue))
	for _, t := range c.queue {
		out = append(out, t.TicketID)
	}
	return out
}

func TestEnqueueQueueJumpOrdering(t *testing.T) {
	cases := []struct {
		name       string
		existing   []queueSpec
		newSmall   bool
		wantOrder  []string // "new" is the inserted ticket
		wantJumped map[string]int
	}{
		{
			name:      "large appends to empty queue",
			existing:  nil,
			newSmall:  false,
			wantOrder: []string{"new"},
		},
		{
			name:      "large appends behind small",
			existing:  []queueSpec{{id: "s1", small: true}},
			newSmall:  false,
			wantOrder: []string{"s1", "new"},
		},
		{
			name:       "small jumps a large under cap",
			existing:   []queueSpec{{id: "L1", small: false}},
			newSmall:   true,
			wantOrder:  []string{"new", "L1"},
			wantJumped: map[string]int{"L1": 1},
		},
		{
			name:       "small never jumps another small",
			existing:   []queueSpec{{id: "s1", small: true}, {id: "L1", small: false}},
			newSmall:   true,
			wantOrder:  []string{"s1", "new", "L1"},
			wantJumped: map[string]int{"L1": 1},
		},
		{
			name:       "small jumps ahead of multiple larges, all charged",
			existing:   []queueSpec{{id: "L1", small: false, jumped: 1}, {id: "L2", small: false}},
			newSmall:   true,
			wantOrder:  []string{"new", "L1", "L2"},
			wantJumped: map[string]int{"L1": 2, "L2": 1},
		},
		{
			name:       "capped large is a wall — small lands behind it",
			existing:   []queueSpec{{id: "Lcap", small: false, jumped: 2}, {id: "L2", small: false}},
			newSmall:   true,
			wantOrder:  []string{"Lcap", "new", "L2"},
			wantJumped: map[string]int{"Lcap": 2, "L2": 1},
		},
		{
			name:       "all larges capped — small appends at the end",
			existing:   []queueSpec{{id: "Lcap1", small: false, jumped: 2}, {id: "Lcap2", small: false, jumped: 3}},
			newSmall:   true,
			wantOrder:  []string{"Lcap1", "Lcap2", "new"},
			wantJumped: map[string]int{"Lcap1": 2, "Lcap2": 3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := newFakeEngine()
			c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))
			// DefaultConfig: priority_jump_cap = 2.
			c.mu.Lock()
			defer c.mu.Unlock()
			byID := buildQueue(c, tc.existing)
			nt := &ticket{Ticket: Ticket{TicketID: "new", Model: "m", Status: StatusQueued, SmallJob: tc.newSmall, EnqueuedAt: time.Now()}}
			c.enqueueLocked(nt)

			got := queueIDs(c)
			if len(got) != len(tc.wantOrder) {
				t.Fatalf("queue = %v, want %v", got, tc.wantOrder)
			}
			for i := range got {
				if got[i] != tc.wantOrder[i] {
					t.Fatalf("queue = %v, want %v", got, tc.wantOrder)
				}
			}
			for id, want := range tc.wantJumped {
				if byID[id].jumpedCount != want {
					t.Errorf("jumped[%s] = %d, want %d", id, byID[id].jumpedCount, want)
				}
			}
			if nt.jumpedCount != 0 {
				t.Errorf("new ticket jumpedCount = %d, want 0", nt.jumpedCount)
			}
		})
	}
}

// TestStarvationGuardEndToEnd walks the motivating scenario: a large job is
// jumped twice (the cap), then every later small job queues behind it.
func TestStarvationGuardEndToEnd(t *testing.T) {
	eng := newFakeEngine()
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))
	c.mu.Lock()
	defer c.mu.Unlock()

	large := &ticket{Ticket: Ticket{TicketID: "large", SmallJob: false, Status: StatusQueued}}
	c.enqueueLocked(large)
	for i, id := range []string{"s1", "s2"} {
		s := &ticket{Ticket: Ticket{TicketID: id, SmallJob: true, Status: StatusQueued}}
		c.enqueueLocked(s)
		if large.jumpedCount != i+1 {
			t.Fatalf("after %s: jumpedCount = %d, want %d", id, large.jumpedCount, i+1)
		}
	}
	// Cap (2) reached: the next small must NOT pass the large.
	s3 := &ticket{Ticket: Ticket{TicketID: "s3", SmallJob: true, Status: StatusQueued}}
	c.enqueueLocked(s3)

	want := []string{"s1", "s2", "large", "s3"}
	got := queueIDs(c)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue = %v, want %v", got, want)
		}
	}
	if large.jumpedCount != 2 {
		t.Errorf("jumpedCount = %d, want 2 (unchanged past cap)", large.jumpedCount)
	}
}

func TestDequeueRemovesTicket(t *testing.T) {
	eng := newFakeEngine()
	c := newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil))
	c.mu.Lock()
	buildQueue(c, []queueSpec{{id: "a"}, {id: "b"}, {id: "c"}})
	c.mu.Unlock()

	c.dequeue("b")
	c.dequeue("zzz") // idempotent no-op

	c.mu.Lock()
	defer c.mu.Unlock()
	got := queueIDs(c)
	want := []string{"a", "c"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}
