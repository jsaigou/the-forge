// SPDX-License-Identifier: Apache-2.0

package duplex

import "sync"

// ring is a fixed-capacity FIFO buffer. It is the single primitive behind
// both the input ring and the output queue, so the "no silent loss" rule has
// one implementation. Push reports whether there was room — it never drops
// an item on its own; the caller decides (queue_full, backpressure, etc.).
// Pop reports whether an item was present. Full/late accounting is split:
// the ring owns fullness (Push returns false, Full/Len/Cap are explicit), and
// the driver owns lateness (sequence watermark for inputs, overflow timing
// for outputs), because lateness needs session state the ring does not have.
type ring[T any] struct {
	mu    sync.Mutex
	buf   []T
	head  int // index of the oldest item
	count int // number of items
}

// newRing returns a ring holding up to cap items. cap is floored at 1.
func newRing[T any](cap int) *ring[T] {
	if cap < 1 {
		cap = 1
	}
	return &ring[T]{buf: make([]T, cap)}
}

// Push appends v and reports whether there was room. On a full ring it
// returns false and leaves the ring unchanged.
func (r *ring[T]) Push(v T) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == len(r.buf) {
		return false
	}
	idx := (r.head + r.count) % len(r.buf)
	r.buf[idx] = v
	r.count++
	return true
}

// Pop removes and returns the oldest item, reporting whether one was present.
func (r *ring[T]) Pop() (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var zero T
	if r.count == 0 {
		return zero, false
	}
	v := r.buf[r.head]
	r.buf[r.head] = zero // drop the reference so it can be GC'd
	r.head = (r.head + 1) % len(r.buf)
	r.count--
	return v, true
}

// Len returns the number of items currently buffered.
func (r *ring[T]) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// Cap returns the ring's capacity.
func (r *ring[T]) Cap() int {
	return len(r.buf)
}

// Full reports whether the ring cannot accept another item.
func (r *ring[T]) Full() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count == len(r.buf)
}

// Empty reports whether the ring holds no items.
func (r *ring[T]) Empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count == 0
}
