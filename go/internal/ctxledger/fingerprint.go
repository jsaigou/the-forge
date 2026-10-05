// SPDX-License-Identifier: Apache-2.0

package ctxledger

// fingerprint.go — C4 conversation fingerprint LRU. Holds only digests and
// counts per conversation: {fingerprint -> message count, total chars,
// prefix-hash}. No content, ever.

import (
	"container/list"
	"crypto/sha256"
	"sync"
	"time"
)

const (
	convLRUMax = 10000
	convTTL    = 6 * time.Hour
)

type convEntry struct {
	key        [32]byte
	count      int
	totalChars int64
	prefix     [32]byte // sha256 over per-message digests of messages[0:count]
	short      bool     // stored under the first-message-only key (conversation had <2 non-system messages)
	seen       time.Time
	elem       *list.Element
}

type convLRU struct {
	mu  sync.Mutex
	max int
	ttl time.Duration
	now func() time.Time
	m   map[[32]byte]*convEntry
	l   *list.List // front = most recent
}

func newConvLRU(max int, ttl time.Duration, now func() time.Time) *convLRU {
	if now == nil {
		now = time.Now
	}
	return &convLRU{max: max, ttl: ttl, now: now, m: map[[32]byte]*convEntry{}, l: list.New()}
}

func (c *convLRU) get(k [32]byte) *convEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[k]
	if e == nil {
		return nil
	}
	if c.now().Sub(e.seen) > c.ttl {
		c.l.Remove(e.elem)
		delete(c.m, k)
		return nil
	}
	cp := *e
	return &cp
}

func (c *convLRU) put(e convEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.seen = c.now()
	if old := c.m[e.key]; old != nil {
		e.elem = old.elem
		*old = e
		old.elem = e.elem
		c.l.MoveToFront(old.elem)
		return
	}
	ne := e
	ne.elem = c.l.PushFront(&ne)
	c.m[e.key] = &ne
	for c.l.Len() > c.max {
		back := c.l.Back()
		be := back.Value.(*convEntry)
		c.l.Remove(back)
		delete(c.m, be.key)
	}
}

func (c *convLRU) del(k [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.m[k]; e != nil {
		c.l.Remove(e.elem)
		delete(c.m, k)
	}
}

func (c *convLRU) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.l.Len()
}

// fingerprints returns the full key (first two non-system messages + consumer)
// when two exist, and always the short key (first non-system message only).
// full is false when the conversation has fewer than two non-system messages.
func fingerprints(msgs []any, hs [][32]byte, consumer string) (full, short [32]byte, hasFull, hasShort bool) {
	var idx []int
	for i, m := range msgs {
		if !isSystemRole(roleOf(m)) {
			idx = append(idx, i)
			if len(idx) == 2 {
				break
			}
		}
	}
	if len(idx) == 0 {
		return
	}
	h := sha256.New()
	h.Write([]byte(consumer))
	h.Write([]byte{0, 1})
	h.Write(hs[idx[0]][:])
	copy(short[:], h.Sum(nil))
	hasShort = true
	if len(idx) == 2 {
		h2 := sha256.New()
		h2.Write([]byte(consumer))
		h2.Write([]byte{0, 2})
		h2.Write(hs[idx[0]][:])
		h2.Write(hs[idx[1]][:])
		copy(full[:], h2.Sum(nil))
		hasFull = true
	}
	return
}

// prefixSums returns sha256(hs[0]||...||hs[k-1]) for k = at and k = len(hs)
// in one pass. ok=false when at > len(hs).
func prefixSums(hs [][32]byte, at int) (atSum, fullSum [32]byte, ok bool) {
	if at > len(hs) || at < 0 {
		return
	}
	h := sha256.New()
	if at == 0 {
		copy(atSum[:], h.Sum(nil))
	}
	for i := range hs {
		h.Write(hs[i][:])
		if i+1 == at {
			copy(atSum[:], h.Sum(nil))
		}
	}
	copy(fullSum[:], h.Sum(nil))
	return atSum, fullSum, true
}
