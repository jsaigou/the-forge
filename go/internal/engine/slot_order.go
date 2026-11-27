// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"sort"
	"time"
)

// Which slot a new load should take. A slot that was just emptied can still have its port held by closed connections (Linux keeps them in
// TIME-WAIT for 60 s), and an inference server that does not reuse addresses then fails to start: loading a llama.cpp model right after
// unloading a Radiance one failed this way (2026-11-27). So a recently freed slot is passed over while another is available: a1 just
// unloaded means a2, then a3, then a4.

// SlotRest is how long a freed slot is passed over: the kernel's 60 s TIME-WAIT plus a margin for the old server's last connections.
const SlotRest = 90 * time.Second

// SlotOrderer is the optional engine capability callers discover by type assertion.
type SlotOrderer interface {
	// LoadOrder is every slot, best choice for a new load first: rested slots in configured order, then recently freed ones, the one freed
	// longest ago first. It says nothing about occupancy; callers skip the occupied ones.
	LoadOrder() []string
	// RecentlyFreed reports whether slot was emptied less than SlotRest ago.
	RecentlyFreed(slot string) bool
}

var _ SlotOrderer = (*Manager)(nil)

// LoadOrderOf returns e's load order, or slots as given when e does not provide one.
func LoadOrderOf(e any, slots []string) []string {
	if o, ok := e.(SlotOrderer); ok {
		return o.LoadOrder()
	}
	return slots
}

func (m *Manager) noteFreed(slot string) {
	now := m.d.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.freedAt == nil {
		m.freedAt = map[string]time.Time{}
	}
	m.freedAt[slot] = now
}

// RecentlyFreed implements SlotOrderer.
func (m *Manager) RecentlyFreed(slot string) bool {
	now := m.d.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.freedAt[slot]
	return ok && now.Sub(at) < SlotRest
}

// LoadOrder implements SlotOrderer.
func (m *Manager) LoadOrder() []string {
	slots := m.Slots()
	now := m.d.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// The zero time for a rested slot keeps those equal, so the stable sort leaves them in configured order, ahead of every resting one.
	restingSince := func(s string) time.Time {
		if at, ok := m.freedAt[s]; ok && now.Sub(at) < SlotRest {
			return at
		}
		return time.Time{}
	}
	sort.SliceStable(slots, func(i, j int) bool { return restingSince(slots[i]).Before(restingSince(slots[j])) })
	return slots
}
