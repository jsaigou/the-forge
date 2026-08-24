// SPDX-License-Identifier: Apache-2.0

package collector

import "context"

// Systemd is the read side of unit state, satisfied by the D-Bus adapter in
// internal/engine (build-tagged until the go-systemd dependency lands — see
// docs/v5-go-contracts.md go.mod rules) and by fakes in tests.
//
// Semantics contract, per the crown-jewels list: the returned ActiveState
// must follow `systemctl is-active` semantics — in particular a unit
// stopping under TimeoutStopSec=300 must report "deactivating" until its
// process has genuinely exited. Both read the same PID1 Unit.ActiveState
// property, but this is verified against ForgeHost before being trusted
// (docs/v5-plan.md risk register).
type Systemd interface {
	// UnitStates returns the observed state for each named unit (names
	// without ".service"). Units systemd does not know report
	// ActiveState "inactive".
	UnitStates(ctx context.Context, units []string) (map[string]UnitState, error)
}

// SlotAssignment is the engine's authoritative view of one slot, merged
// into Snapshot.Slots by the collector each cycle.
type SlotAssignment struct {
	Mode      string
	Loading   *Transition
	Unloading *Transition
}

// SlotStateSource is implemented by the engine (which owns slot
// reconciliation, including the never-clear-while-deactivating rule). The
// collector passes in the unit states it just probed so reconciliation and
// probing stay one-probe-per-cycle.
type SlotStateSource interface {
	SlotStates(units map[string]UnitState) map[string]SlotAssignment
}
