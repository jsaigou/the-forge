// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"

	"github.com/jsaigou/the-forge/internal/collector"
)

// Systemd is the engine's unit-control surface. The production
// implementation is the D-Bus adapter in dbus.go (build-tagged until the
// coreos/go-systemd dependency lands in go.mod — integrator action, see
// docs/v5-go-contracts.md); it also satisfies collector.Systemd so one
// connection serves both packages. NO systemctl shell-outs (locked stack).
type Systemd interface {
	// Start begins starting unit (name without ".service"). It returns
	// once the job is enqueued/completed; readiness is verified separately
	// via SubState + /health polling (V4 semantics).
	Start(ctx context.Context, unit string) error

	// Stop begins stopping unit. Large models sit in "deactivating" for
	// minutes afterwards (TimeoutStopSec=300) — callers poll State.
	Stop(ctx context.Context, unit string) error

	// State returns the unit's current ActiveState/SubState with
	// `systemctl is-active` semantics (crown jewels: "deactivating" must
	// be visible, not collapsed into inactive).
	State(ctx context.Context, unit string) (collector.UnitState, error)

	// MainPID returns the unit's main process PID (0 when the unit is not
	// running or has no main process). The fit check uses it to measure a
	// slot's real live GPU footprint (Proc.GPUMemoryBytes) instead of
	// crediting only the on-disk weight size when planning evictions.
	MainPID(ctx context.Context, unit string) (uint32, error)
}
