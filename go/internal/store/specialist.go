// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Specialist pool occupants and per-function defaults (ADR-0022, slice 2).

// Specialist pool functions: what a request asks for. a0 routes by function.
const (
	FnEmbeddings    = "embeddings"
	FnSpeech        = "speech"
	FnTranscription = "transcription"
	FnAlignment     = "alignment"
	FnDecisions     = "decisions"
	// FnDuplex is a full-duplex speech-to-speech engine (ADR-0029): a session leases it for its whole life.
	FnDuplex = "duplex"
)

// SpecialistFunctions lists every valid function, in display order.
var SpecialistFunctions = []string{FnEmbeddings, FnSpeech, FnTranscription, FnAlignment, FnDecisions, FnDuplex}

// Occupant policies.
const (
	PolicyAlwaysOn = "always_on"
	PolicyOnDemand = "on_demand"
)

// Occupant is a registered launch template for a specialist slot.
type Occupant struct {
	ID          int64
	Name        string
	Label       string
	Kind        string            // launch kind; the registry in internal/specialist validates Params against it
	Params      map[string]string // kind-specific, validated before it is stored or rendered
	Functions   []string
	MemoryBytes int64 // estimate until measured
	Policy      string
	HomeSlot    string // always_on: the reserved specialist slot; on_demand: ""
	Enabled     bool
	// Stopped: the operator stopped this member and it stays stopped (across daemon restarts) until it is started again.
	Stopped bool
	Notes   string
}

// Specialist is the store surface for the pool's occupants and defaults.
type Specialist interface {
	CreateOccupant(ctx context.Context, o Occupant) (int64, error)
	UpdateOccupant(ctx context.Context, o Occupant) error
	DeleteOccupant(ctx context.Context, name string) error
	// SetStopped records (or clears) an operator stop; a narrow write that touches nothing else.
	SetStopped(ctx context.Context, name string, stopped bool) error
	OccupantByName(ctx context.Context, name string) (Occupant, error)
	ListOccupants(ctx context.Context) ([]Occupant, error)
	// SetDefault points a function at an occupant; an empty occupant clears it.
	SetDefault(ctx context.Context, function, occupant string) error
	Defaults(ctx context.Context) (map[string]string, error)
}

type specialistView struct{ d *DB }

func (d *DB) Specialist() Specialist { return specialistView{d} }

func validFunction(f string) bool {
	for _, x := range SpecialistFunctions {
		if x == f {
			return true
		}
	}
	return false
}

// validateOccupant checks what the store can know: the registry (internal/specialist) owns kind and param validity.
func validateOccupant(o *Occupant) error {
	if o.Name == "" || len(o.Name) > 64 {
		return errors.New("store: specialist occupant name must be 1-64 characters")
	}
	if o.Kind == "" {
		return fmt.Errorf("store: specialist occupant %q has no kind", o.Name)
	}
	switch o.Policy {
	case "":
		o.Policy = PolicyOnDemand
	case PolicyAlwaysOn, PolicyOnDemand:
	default:
		return fmt.Errorf("store: specialist occupant %q: unknown policy %q", o.Name, o.Policy)
	}
	if o.Policy == PolicyOnDemand {
		o.HomeSlot = ""
	}
	seen := map[string]bool{}
	for _, f := range o.Functions {
		if !validFunction(f) {
			return fmt.Errorf("store: specialist occupant %q: unknown function %q", o.Name, f)
		}
		seen[f] = true
	}
	o.Functions = o.Functions[:0:0]
	for _, f := range SpecialistFunctions {
		if seen[f] {
			o.Functions = append(o.Functions, f)
		}
	}
	if o.Params == nil {
		o.Params = map[string]string{}
	}
	return nil
}

const occupantCols = `id, name, label, kind, params, functions, memory_bytes, policy, home_slot, enabled, stopped, notes`

func scanOccupant(sc interface{ Scan(...any) error }) (Occupant, error) {
	var o Occupant
	var params, fns string
	var enabled, stopped int
	if err := sc.Scan(&o.ID, &o.Name, &o.Label, &o.Kind, &params, &fns, &o.MemoryBytes, &o.Policy, &o.HomeSlot, &enabled, &stopped, &o.Notes); err != nil {
		return Occupant{}, err
	}
	o.Enabled, o.Stopped = enabled != 0, stopped != 0
	if err := json.Unmarshal([]byte(params), &o.Params); err != nil {
		return Occupant{}, fmt.Errorf("params of %q are not a JSON object: %w", o.Name, err)
	}
	if err := json.Unmarshal([]byte(fns), &o.Functions); err != nil {
		return Occupant{}, fmt.Errorf("functions of %q are not a JSON array: %w", o.Name, err)
	}
	return o, nil
}

func encodeOccupant(o Occupant) (params, fns []byte, err error) {
	if params, err = json.Marshal(o.Params); err != nil {
		return nil, nil, err
	}
	if fns, err = json.Marshal(o.Functions); err != nil {
		return nil, nil, err
	}
	return params, fns, nil
}

func (v specialistView) CreateOccupant(ctx context.Context, o Occupant) (int64, error) {
	if err := validateOccupant(&o); err != nil {
		return 0, err
	}
	params, fns, err := encodeOccupant(o)
	if err != nil {
		return 0, fmt.Errorf("store: specialist.create_occupant: %w", err)
	}
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO specialist_occupants (name, label, kind, params, functions, memory_bytes, policy, home_slot, enabled, stopped, notes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Name, o.Label, o.Kind, string(params), string(fns), o.MemoryBytes, o.Policy, o.HomeSlot, boolInt(o.Enabled), boolInt(o.Stopped), o.Notes)
	if err != nil {
		return 0, fmt.Errorf("store: specialist.create_occupant %q: %w", o.Name, err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v specialistView) UpdateOccupant(ctx context.Context, o Occupant) error {
	if err := validateOccupant(&o); err != nil {
		return err
	}
	params, fns, err := encodeOccupant(o)
	if err != nil {
		return fmt.Errorf("store: specialist.update_occupant: %w", err)
	}
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE specialist_occupants SET name=?, label=?, kind=?, params=?, functions=?, memory_bytes=?, policy=?, home_slot=?, enabled=?, stopped=?, notes=?
		 WHERE id=?`,
		o.Name, o.Label, o.Kind, string(params), string(fns), o.MemoryBytes, o.Policy, o.HomeSlot, boolInt(o.Enabled), boolInt(o.Stopped), o.Notes, o.ID)
	if err != nil {
		return fmt.Errorf("store: specialist.update_occupant %q: %w", o.Name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: specialist occupant %d", ErrNotFound, o.ID)
	}
	return nil
}

func (v specialistView) DeleteOccupant(ctx context.Context, name string) error {
	tx, err := v.d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: specialist.delete_occupant: %w", err)
	}
	defer tx.Rollback()
	// A default must never point at a deleted occupant: it would answer a function with nothing.
	if _, err := tx.ExecContext(ctx, `DELETE FROM specialist_defaults WHERE occupant = ?`, name); err != nil {
		return fmt.Errorf("store: specialist.delete_occupant defaults: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM specialist_occupants WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("store: specialist.delete_occupant: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: specialist occupant %q", ErrNotFound, name)
	}
	return tx.Commit()
}

func (v specialistView) SetStopped(ctx context.Context, name string, stopped bool) error {
	res, err := v.d.sql.ExecContext(ctx, `UPDATE specialist_occupants SET stopped = ? WHERE name = ?`, boolInt(stopped), name)
	if err != nil {
		return fmt.Errorf("store: specialist.set_stopped: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: specialist occupant %q", ErrNotFound, name)
	}
	return nil
}

func (v specialistView) OccupantByName(ctx context.Context, name string) (Occupant, error) {
	o, err := scanOccupant(v.d.sql.QueryRowContext(ctx, `SELECT `+occupantCols+` FROM specialist_occupants WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Occupant{}, fmt.Errorf("%w: specialist occupant %q", ErrNotFound, name)
	}
	if err != nil {
		return Occupant{}, fmt.Errorf("store: specialist.occupant_by_name: %w", err)
	}
	return o, nil
}

func (v specialistView) ListOccupants(ctx context.Context) ([]Occupant, error) {
	rows, err := v.d.sql.QueryContext(ctx, `SELECT `+occupantCols+` FROM specialist_occupants ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: specialist.list_occupants: %w", err)
	}
	defer rows.Close()
	out := []Occupant{}
	for rows.Next() {
		o, err := scanOccupant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: specialist.list_occupants: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (v specialistView) SetDefault(ctx context.Context, function, occupant string) error {
	if !validFunction(function) {
		return fmt.Errorf("store: unknown specialist function %q", function)
	}
	if occupant == "" {
		if _, err := v.d.sql.ExecContext(ctx, `DELETE FROM specialist_defaults WHERE function = ?`, function); err != nil {
			return fmt.Errorf("store: specialist.clear_default: %w", err)
		}
		return nil
	}
	o, err := v.OccupantByName(ctx, occupant)
	if err != nil {
		return err
	}
	served := false
	for _, f := range o.Functions {
		served = served || f == function
	}
	if !served {
		return fmt.Errorf("store: specialist occupant %q does not serve %q (it serves %v)", occupant, function, o.Functions)
	}
	if _, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO specialist_defaults (function, occupant) VALUES (?, ?)
		 ON CONFLICT(function) DO UPDATE SET occupant = excluded.occupant`, function, occupant); err != nil {
		return fmt.Errorf("store: specialist.set_default: %w", err)
	}
	return nil
}

func (v specialistView) Defaults(ctx context.Context) (map[string]string, error) {
	rows, err := v.d.sql.QueryContext(ctx, `SELECT function, occupant FROM specialist_defaults`)
	if err != nil {
		return nil, fmt.Errorf("store: specialist.defaults: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var f, o string
		if err := rows.Scan(&f, &o); err != nil {
			return nil, fmt.Errorf("store: specialist.defaults: %w", err)
		}
		out[f] = o
	}
	return out, rows.Err()
}

// DefaultFunctions returns the functions that have a default, sorted (stable output for APIs and tests).
func DefaultFunctions(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for f := range m {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
