// SPDX-License-Identifier: Apache-2.0

// D-Bus systemd adapter — the production implementation of engine.Systemd
// AND collector.Systemd over one shared connection. NO systemctl shell-outs
// (docs/v5-go-contracts.md locked stack).
//
// Semantics note (crown jewels): ActiveState here is read from PID1's
// org.freedesktop.systemd1.Unit ActiveState property — the same property
// `systemctl is-active` prints (verified read-only against ForgeHost; see the
// Phase 2 progress entry). "deactivating" is therefore visible identically.
package engine

import (
	"context"
	"encoding/hex"
	"fmt"
	"reflect"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"

	"github.com/jsaigou/the-forge/internal/collector"
)

// DBus is the shared systemd connection.
type DBus struct {
	conn *sd.Conn
}

var (
	_ Systemd           = (*DBus)(nil)
	_ collector.Systemd = (*DBus)(nil)
)

// NewDBus connects to the system bus.
func NewDBus(ctx context.Context) (*DBus, error) {
	conn, err := sd.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: systemd dbus: %w", err)
	}
	return &DBus{conn: conn}, nil
}

// Close releases the connection.
func (d *DBus) Close() { d.conn.Close() }

// Start implements Systemd. mode "replace" mirrors `systemctl start`.
func (d *DBus) Start(ctx context.Context, unit string) error {
	ch := make(chan string, 1)
	if _, err := d.conn.StartUnitContext(ctx, unit+".service", "replace", ch); err != nil {
		return fmt.Errorf("engine: start %s: %w", unit, err)
	}
	// Job completion ≠ readiness (Type=exec units are "done" at exec);
	// readiness is verified by SubState + /health polling, V4 parity. The
	// job result still surfaces immediate failures (missing unit etc.).
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("engine: start %s: job %s", unit, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop implements Systemd. Deliberately does NOT wait for the stop job:
// large-model stops take minutes (TimeoutStopSec=300) and callers poll
// State for the deactivating → inactive transition instead.
func (d *DBus) Stop(ctx context.Context, unit string) error {
	if _, err := d.conn.StopUnitContext(ctx, unit+".service", "replace", nil); err != nil {
		return fmt.Errorf("engine: stop %s: %w", unit, err)
	}
	return nil
}

// Restart issues a real systemd "restart" job (atomic stop+start) rather
// than a manual Stop-then-Start from a caller, which would race a fast
// process's port rebind. Not part of the engine.Systemd interface (model
// slot lifecycle never needs it — SwitchMode/Load/Unload cover that); added
// for compressorctl.Provisioner's restart lifecycle (Phase 2,
// docs/v5-headroom-topology.md §5).
func (d *DBus) Restart(ctx context.Context, unit string) error {
	ch := make(chan string, 1)
	if _, err := d.conn.RestartUnitContext(ctx, unit+".service", "replace", ch); err != nil {
		return fmt.Errorf("engine: restart %s: %w", unit, err)
	}
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("engine: restart %s: job %s", unit, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// State implements Systemd (single unit).
func (d *DBus) State(ctx context.Context, unit string) (collector.UnitState, error) {
	props, err := d.conn.GetUnitPropertiesContext(ctx, unit+".service")
	if err != nil {
		return collector.UnitState{}, fmt.Errorf("engine: state %s: %w", unit, err)
	}
	return unitStateFromProps(props), nil
}

// MainPID implements Systemd. MainPID lives on the Service interface, which
// GetUnitPropertiesContext (State's lighter read) does not merge — so this
// uses GetAllPropertiesContext, same technique the collector's UnitStates
// uses to surface MainPID. 0 when the unit is inactive or has no main PID.
func (d *DBus) MainPID(ctx context.Context, unit string) (uint32, error) {
	props, err := d.conn.GetAllPropertiesContext(ctx, unit+".service")
	if err != nil {
		return 0, fmt.Errorf("engine: main pid %s: %w", unit, err)
	}
	if v, ok := props["MainPID"].(uint32); ok {
		return v, nil
	}
	return 0, nil
}

// UnitStates implements collector.Systemd (per-unit read for the probe loop).
//
// Previously a single batched ListUnitsByNamesContext call. That only
// returns the Unit interface's ActiveState/SubState/JobId — not the
// Service-interface properties (Result, NRestarts, ExecMainStatus,
// InvocationID) the notifications sprint's crash/OOM detection needs. Those
// require a per-unit Properties.GetAll("") call (same technique State()
// already uses for a single unit, via unitStateFromProps below), which
// systemd merges across every interface the unit object implements. This
// trades one D-Bus round trip for N — fine at this cadence and unit count
// (collector cycle, ~10-15 units, local D-Bus IPC). Unlike the old batch call, one
// unit's query failing (bus hiccup, or a genuinely never-loaded unit name)
// no longer fails the whole cycle: it's reported inactive and the rest of
// the units still get real data.
func (d *DBus) UnitStates(ctx context.Context, units []string) (map[string]collector.UnitState, error) {
	out := make(map[string]collector.UnitState, len(units))
	for _, u := range units {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		props, err := d.conn.GetAllPropertiesContext(ctx, u+".service")
		if err != nil {
			out[u] = collector.UnitState{ActiveState: "inactive"}
			continue
		}
		out[u] = unitStateFromProps(props)
	}
	return out, nil
}

func unitStateFromProps(props map[string]interface{}) collector.UnitState {
	st := collector.UnitState{}
	if v, ok := props["ActiveState"].(string); ok {
		st.ActiveState = v
	}
	if v, ok := props["SubState"].(string); ok {
		st.SubState = v
	}
	// StateChangeTimestamp is µs since epoch.
	if v, ok := props["StateChangeTimestamp"].(uint64); ok && v > 0 {
		st.Since = time.UnixMicro(int64(v))
	}
	// Service-interface properties (present on GetAllPropertiesContext's
	// merged result; absent — fields stay zero — on GetUnitPropertiesContext,
	// which only reads the Unit interface, and on non-service unit types).
	if v, ok := props["Result"].(string); ok {
		st.Result = v
	}
	if v, ok := props["NRestarts"].(uint32); ok {
		st.NRestarts = v
	}
	if v, ok := props["ExecMainStatus"].(int32); ok {
		st.ExecMainStatus = v
	}
	if v, ok := props["InvocationID"].([]byte); ok && len(v) > 0 {
		st.InvocationID = hex.EncodeToString(v)
	}
	if v, ok := props["MainPID"].(uint32); ok {
		st.MainPID = v
	}
	st.ExecStartPath = execStartPath(props["ExecStart"])
	return st
}

// execStartPath extracts the launcher path from the Service interface's
// ExecStart property (D-Bus signature a(sasbttttuii): an array of structs,
// one per ExecStart= line — path, argv, ignore-failure flag, four
// timestamps, pid, exit status; confirmed live against a real unit,
// 2026-09-01: `busctl get-property … Service ExecStart` on forge-comfyui
// returns exactly this shape). godbus decodes each struct's fields
// positionally, but the OUTER and INNER slice's concrete Go type isn't
// pinned by the D-Bus spec — depending on godbus's own generic-Variant
// decode path this can surface as []interface{} of []interface{}, or as a
// properly-typed [][]interface{}, or similar. reflect walks whatever
// slice/array-like value comes back at each level rather than betting on
// one concrete type, and degrades to "" on any shape it doesn't recognize
// — a defensive read only, never authoritative over what systemd itself
// reports for ActiveState/Result (which decode as plain scalars, unaffected
// by this).
func execStartPath(raw interface{}) string {
	first, ok := firstSliceElem(raw)
	if !ok {
		return ""
	}
	field0, ok := firstSliceElem(first)
	if !ok {
		return ""
	}
	path, _ := field0.(string)
	return path
}

// firstSliceElem returns v's first element if v is a non-empty slice or
// array (any element type), via reflection — godbus-decoded D-Bus values
// don't come back as one fixed concrete Go type.
func firstSliceElem(v interface{}) (interface{}, bool) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil, false
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Len() == 0 {
			return nil, false
		}
		return rv.Index(0).Interface(), true
	default:
		return nil, false
	}
}
