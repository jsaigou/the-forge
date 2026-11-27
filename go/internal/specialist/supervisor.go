// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"bufio"
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// superviseEvery is how often Run looks at the pool.
const superviseEvery = 5 * time.Second

// Adopt reads what is already running, so a daemon restart does not stop and restart specialists that are fine. A slot whose env file names an
// occupant, whose unit is active and whose health path answers is adopted as running. Anything else is left empty for the supervisor to fill.
func (m *Manager) Adopt(ctx context.Context) {
	occ := m.occupantsByName(ctx)
	cfg := m.d.Cfg()
	for _, slot := range m.slotNames() {
		name := readEnvValue(m.envPath(slot), "FORGE_POOL_OCCUPANT")
		o, known := occ[name]
		if name == "" || !known || o.Stopped {
			continue
		}
		sc := cfg.SpecialistSlots[slot]
		st, err := m.d.Sys.State(ctx, sc.Unit)
		if err != nil || st.ActiveState != "active" {
			continue
		}
		k, _ := KindByName(o.Kind)
		port := sc.Port
		if k.FixedPort != 0 {
			port = k.FixedPort
		}
		if err := m.d.Health(ctx, port, k.HealthPath); err != nil {
			m.logf("adopt %s in %s: unit is active but not healthy (%v); leaving it to the supervisor", name, slot, err)
			continue
		}
		m.mu.Lock()
		r := m.rec(slot)
		if r.state != StateEmpty {
			// Adopt runs in the background while the daemon already serves: a request that got to this slot first (Ensure marks it Starting) owns
			// the record now, and overwriting it would hide a start in progress from everything that checks it.
			state := r.state
			m.mu.Unlock()
			m.logf("adopt %s in %s: the slot is already %s; leaving the record alone", name, slot, state)
			continue
		}
		now := m.d.Now()
		*r = slotRec{occupant: name, state: StateRunning, loadedAt: now, lastUse: now, memory: o.MemoryBytes}
		m.mu.Unlock()
		m.logf("adopted %s running in %s", name, slot)
	}
}

func readEnvValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && k == key {
			return v
		}
	}
	return ""
}

// Run adopts what is running, then supervises until ctx ends: always-on members are started (smallest first, in the background of daemon
// startup) and restarted with backoff, and a member whose unit died is noticed. It never blocks serving: callers run it in a goroutine.
func (m *Manager) Run(ctx context.Context) {
	m.Adopt(ctx)
	for {
		m.Tick(ctx)
		m.d.Sleep(ctx, superviseEvery)
		if ctx.Err() != nil {
			return
		}
	}
}

// Tick is one supervision pass. It is exported so tests (and an operator "retry now") can drive it.
func (m *Manager) Tick(ctx context.Context) {
	m.noticeDeaths(ctx)
	occ := m.occupantsByName(ctx)
	want := []store.Occupant{}
	for _, o := range occ {
		if o.Enabled && o.Policy == store.PolicyAlwaysOn && o.HomeSlot != "" {
			want = append(want, o)
		}
	}
	// Smallest first: the small, fast ones are usable soonest.
	sort.Slice(want, func(i, j int) bool {
		if want[i].MemoryBytes != want[j].MemoryBytes {
			return want[i].MemoryBytes < want[j].MemoryBytes
		}
		return want[i].Name < want[j].Name
	})
	for _, o := range want {
		if ctx.Err() != nil {
			return
		}
		if o.Stopped {
			// Persisted operator stop: show it as stopped in its slot after a daemon restart, and leave it alone.
			m.mu.Lock()
			if _, ok := m.d.Cfg().SpecialistSlots[o.HomeSlot]; ok {
				if r := m.rec(o.HomeSlot); r.state == StateEmpty {
					*r = slotRec{occupant: o.Name, state: StateStopped}
				}
			}
			m.mu.Unlock()
			continue
		}
		if !m.dueForStart(o) {
			continue
		}
		if _, err := m.loadSlot(ctx, o.Name, o.HomeSlot, false); err != nil {
			m.logf("always-on %s: %v", o.Name, err)
		}
	}
}

// dueForStart reports whether the supervisor should try to start o now.
func (m *Manager) dueForStart(o store.Occupant) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.d.Cfg().SpecialistSlots[o.HomeSlot]; !ok {
		return false
	}
	r := m.rec(o.HomeSlot)
	switch r.state {
	case StateStopped:
		// Tick only asks about members whose persisted stop flag is clear, so a "stopped" slot record here is stale: the operator's stop was
		// superseded (a disable's background stop finished after the member was enabled again). The reserved owner starts.
		return true
	case StateRunning, StateStarting, StateStopping:
		// Running or already starting needs nothing; stopping is the operator's unload in progress.
		return r.occupant != o.Name && r.state == StateRunning
	case StateFailed:
		if r.occupant != o.Name {
			return true // a stale failure of some other occupant never blocks the reserved owner
		}
		return !m.d.Now().Before(r.nextRetry)
	}
	return true // empty
}

// unitActive reports whether the slot's unit is running. An unreadable state is treated as alive: a D-Bus hiccup must not evict a healthy member.
func (m *Manager) unitActive(ctx context.Context, slot string) bool {
	st, err := m.d.Sys.State(ctx, m.d.Cfg().SpecialistSlots[slot].Unit)
	return err != nil || st.ActiveState == "active"
}

// markDead records that the member in slot (named name) is no longer running: an always-on one becomes failed (the supervisor restarts it with
// backoff), an on-demand one becomes empty (the next request loads it again). It is a no-op if the slot has since changed.
func (m *Manager) markDead(ctx context.Context, slot, name string) {
	occ := m.occupantsByName(ctx)
	st, _ := m.d.Sys.State(ctx, m.d.Cfg().SpecialistSlots[slot].Unit)
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rec(slot)
	if r.state != StateRunning || r.occupant != name {
		return
	}
	if g, ok := occ[name]; ok && g.Policy == store.PolicyAlwaysOn {
		r.state, r.err = StateFailed, "unit is "+st.ActiveState+"/"+st.SubState
		r.attempts++
		r.nextRetry = m.d.Now().Add(backoffFor(r.attempts))
		m.logf("%s in %s died (%s/%s); restart at %s", name, slot, st.ActiveState, st.SubState, r.nextRetry.Format(time.TimeOnly))
		return
	}
	*r = slotRec{state: StateEmpty}
	m.logf("%s in %s is gone (%s/%s)", name, slot, st.ActiveState, st.SubState)
}

// noticeDeaths is the supervisor's sweep for members whose unit is no longer active.
func (m *Manager) noticeDeaths(ctx context.Context) {
	for _, slot := range m.slotNames() {
		m.mu.Lock()
		r := m.rec(slot)
		running, name := r.state == StateRunning, r.occupant
		m.mu.Unlock()
		if running && !m.unitActive(ctx, slot) {
			m.markDead(ctx, slot, name)
		}
	}
}

// Forget drops the runtime record of every slot that holds name and is not running or starting, so a deleted occupant does not linger as a
// "stopped" or "failed" ghost in its slot. It never touches a member that is running or mid-start.
func (m *Manager) Forget(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.slots {
		if r.occupant == name && r.state != StateRunning && r.state != StateStarting && r.state != StateStopping {
			*r = slotRec{state: StateEmpty}
		}
	}
}
