// SPDX-License-Identifier: Apache-2.0

package sched

// queue.go — the in-memory ticket queue with V4's priority queue-jump
// (docs/scheduler.md "Priority Queue-Jump", forge/scheduler.py
// _enqueue_ticket). All functions here require c.mu held.

// ticket is a queued EnsureLoaded caller. The embedded Ticket is the
// Contract 1 shape; jumpedCount and priority are internal.
type ticket struct {
	Ticket

	// jumpedCount counts how many times this (non-small) ticket has been
	// jumped by a small job. At PriorityJumpCap it becomes a wall nothing
	// can be inserted ahead of — the starvation guard.
	jumpedCount int

	// priority is EnsureRequest.Priority, persisted with the ticket for
	// observability. V4 has no numeric priority and it does not affect
	// queue ordering; SmallJob is the only jump mechanism. Kept so the
	// frozen field round-trips, not silently dropped.
	priority int
}

// enqueueLocked inserts t applying the small-job jump rule: a small ticket
// may be inserted ahead of a queued non-small ticket that has not yet been
// jumped PriorityJumpCap times; a capped non-small ticket acts as a wall.
// Every non-small ticket that ends up behind the insertion point was just
// delayed by it and is charged one more jump (V4 parity, exact).
//
// Small tickets never jump other small tickets — the scan skips smalls, so
// smalls stay FIFO among themselves.
func (c *Core) enqueueLocked(t *ticket) {
	if !t.SmallJob {
		c.queue = append(c.queue, t)
		return
	}
	jumpCap := c.cfg.PriorityJumpCap
	insertAt := len(c.queue)
	for i, o := range c.queue {
		if !o.SmallJob && o.jumpedCount < jumpCap {
			insertAt = i
			break
		}
	}
	c.queue = append(c.queue, nil)
	copy(c.queue[insertAt+1:], c.queue[insertAt:])
	c.queue[insertAt] = t
	for _, o := range c.queue[insertAt+1:] {
		if !o.SmallJob {
			o.jumpedCount++
		}
	}
}

// dequeue removes a ticket by id (idempotent) and deletes its persisted
// row. Takes the mutex itself — call without c.mu held.
func (c *Core) dequeue(id string) {
	c.mu.Lock()
	for i, t := range c.queue {
		if t.TicketID == id {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
	c.persistDeleteTicket(id)
}
