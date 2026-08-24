// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"sync"
	"time"
)

// RateLimiter counts authentication failures per client IP in a fixed
// window — the V5 contract is 10 fails / 60s on both the session (login)
// and bearer paths (Contract 1 §1; V4: auth.py's TTLCache pair). Entries
// expire window-relative to their first failure, mirroring the V4 TTL
// semantics.
type RateLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	entries map[string]*rlEntry
	now     func() time.Time // clock hook; nil = time.Now
}

func (r *RateLimiter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

type rlEntry struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter returns a limiter allowing limit-1 failures per window
// before TooMany fires.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:   limit,
		window:  window,
		entries: make(map[string]*rlEntry),
	}
}

// TooMany reports whether ip has reached the failure limit in the current
// window.
func (r *RateLimiter) TooMany(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[ip]
	if !ok || r.clock().After(e.resetAt) {
		return false
	}
	return e.count >= r.limit
}

// Fail records one failed attempt for ip.
func (r *RateLimiter) Fail(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	e, ok := r.entries[ip]
	if !ok || now.After(e.resetAt) {
		r.entries[ip] = &rlEntry{count: 1, resetAt: now.Add(r.window)}
		r.pruneLocked(now)
		return
	}
	e.count++
}

// pruneLocked drops expired entries once the map grows large (bounds memory
// like V4's maxsize=1000 TTLCache).
func (r *RateLimiter) pruneLocked(now time.Time) {
	if len(r.entries) < 1024 {
		return
	}
	for ip, e := range r.entries {
		if now.After(e.resetAt) {
			delete(r.entries, ip)
		}
	}
}
