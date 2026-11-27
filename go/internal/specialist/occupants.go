// SPDX-License-Identifier: Apache-2.0

package specialist

// The rules for pool occupants (ADR-0022) in one place. The HTTP handlers translate requests and answers; every rule about what an occupant may be
// and what may happen to it lives here, so it can be tested without HTTP and no other route can write around it.
//
// The rules:
//   - an occupant must pass the launch registry, and a catalog-config occupant must name a config the pool can run;
//   - an enabled catalog config has one owner (a disabled occupant holds nothing, so it releases the config; re-enabling is checked again);
//   - an always-on member needs one of the specialist slots as its home, and enabled always-on members don't share a home;
//   - a function's default must be an enabled always-on member, and while it is one it can't be taken out of that state;
//   - a member that is running or starting can't be deleted.
//
// A failed check is never a passed check: when a rule has to read the store and the read fails, the write is refused with that error.

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Pool is the runtime side the rules need. *Manager satisfies it; tests use a fake.
type Pool interface {
	Snapshot(ctx context.Context) []SlotState
	Load(ctx context.Context, name, slot string) (SlotState, error)
	Unload(ctx context.Context, slot string) error
	// Forget drops the runtime record of an occupant that is not running.
	Forget(name string)
}

// Reader is the read side of the occupant store.
type Reader interface {
	OccupantByName(ctx context.Context, name string) (store.Occupant, error)
	ListOccupants(ctx context.Context) ([]store.Occupant, error)
	Defaults(ctx context.Context) (map[string]string, error)
}

// Writer is the write side of the occupant store. Only Occupants holds one: nothing else may write occupants or defaults.
type Writer interface {
	CreateOccupant(ctx context.Context, o store.Occupant) (int64, error)
	UpdateOccupant(ctx context.Context, o store.Occupant) error
	DeleteOccupant(ctx context.Context, name string) error
	SetStopped(ctx context.Context, name string, stopped bool) error
	SetDefault(ctx context.Context, function, occupant string) error
}

// ValidationError is the operator's mistake: field name -> what is wrong. Codes optionally gives a stable code per field.
type ValidationError struct {
	Fields map[string]string
	Codes  map[string]string
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "specialist: invalid occupant (" + strings.Join(parts, "; ") + ")"
}

// ConflictError is a refusal because of the pool's current state (code is the stable API code, e.g. has_dependents).
type ConflictError struct {
	Code     string
	Resource string
	// Params are the values the code's translated text interpolates (the API's error `params`). When nil, Resource is sent as {"resource": ...}.
	Params  map[string]any
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

// ErrAlreadyExists means an occupant with that name exists.
var ErrAlreadyExists = errors.New("specialist: an occupant with this name already exists")

// PoolError wraps an error from the runtime pool (load/unload), which callers map with the pool's own codes.
type PoolError struct{ Err error }

func (e *PoolError) Error() string { return e.Err.Error() }
func (e *PoolError) Unwrap() error { return e.Err }

// Occupants applies the rules. Build one per use from the live dependencies; it holds no state of its own.
type Occupants struct {
	Read  Reader
	Write Writer
	// Pool is nil when the pool isn't running (no specialist slots configured).
	Pool Pool
	// Catalog is nil when the catalog isn't available to the pool.
	Catalog CatalogResolver
	// HomeSlots lists the configured specialist slots; nil skips the home-slot existence check.
	HomeSlots  func() map[string]bool
	ModelRoots func() []string
	// UnloadWait is how long an unload waits for the stop before answering "still stopping" (default 10 s). It stays well under the UI's 30 s
	// timeout: stopping a member mid-generation can take ~75 s. UnloadBudget bounds the background stop itself (default 3 min).
	UnloadWait   time.Duration
	UnloadBudget time.Duration
}

func (m *Occupants) roots() []string {
	if m.ModelRoots == nil {
		return nil
	}
	return m.ModelRoots()
}

func isAlwaysOnMember(o store.Occupant) bool { return o.Enabled && o.Policy == store.PolicyAlwaysOn }

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// check validates an occupant against the launch registry, the catalog and the pool's slots. self is the name it is being saved under (so an edit
// doesn't conflict with itself).
func (m *Occupants) check(ctx context.Context, o store.Occupant, self string) error {
	fields := map[string]string{}
	if o.Name == "" {
		fields["name"] = "required"
	}
	if err := Validate(o, m.roots()); err != nil {
		fields["params"] = err.Error()
	} else if o.Kind == KindCatalogConfig {
		// A catalog config must exist, be a model config, and be something the pool can run (the same checks the launcher repeats).
		if m.Catalog == nil {
			fields["params"] = "the catalog is not available to the specialist pool on this server"
		} else if spec, err := m.Catalog.LaunchSpec(ctx, o.Params["config"]); err != nil {
			fields["params"] = err.Error()
		} else if err := ValidateLaunchSpec(spec, m.roots()); err != nil {
			fields["params"] = "catalog config " + o.Params["config"] + " cannot run in the pool: " + err.Error()
		} else if !o.Enabled {
			// A disabled occupant holds nothing, so it may name a config another enabled occupant owns; the same check runs when it is re-enabled.
		} else if owner, taken, err := m.configOwner(ctx, o.Params["config"], self, o.Name); err != nil {
			return err
		} else if taken {
			fields["params"] = "catalog config " + o.Params["config"] + " is already hosted by " + owner + " (a config has one owner)"
		}
	}
	if o.Policy == store.PolicyAlwaysOn {
		if m.HomeSlots != nil && !m.HomeSlots()[o.HomeSlot] {
			fields["home_slot"] = "an always-on member needs one of the specialist slots as its home"
		}
		list, err := m.Read.ListOccupants(ctx)
		if err != nil {
			return err
		}
		for _, other := range list {
			if other.Name != self && other.Name != o.Name && other.Enabled && other.Policy == store.PolicyAlwaysOn && other.HomeSlot == o.HomeSlot && o.HomeSlot != "" {
				fields["home_slot"] = "slot " + o.HomeSlot + " is already the home of " + other.Name
			}
		}
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// configOwner reports another enabled occupant that already hosts the same catalog config. A disabled occupant does not own its config: the
// runtime (Manager.occupantForConfig) ignores it too, so this agrees with what the scheduler and a0 do.
func (m *Occupants) configOwner(ctx context.Context, config, self, name string) (string, bool, error) {
	list, err := m.Read.ListOccupants(ctx)
	if err != nil {
		return "", false, err
	}
	for _, other := range list {
		if other.Name != self && other.Name != name && other.Enabled && other.Kind == KindCatalogConfig && other.Params["config"] == config {
			return other.Name, true, nil
		}
	}
	return "", false, nil
}

// guardDefault refuses a change that would stop cur being an enabled always-on member while it is the default for a function. The operator sets
// another default (or clears it) first; nothing is cleared silently. A member that is not always-on today (an older, mis-set default) is not
// blocked from further changes.
func (m *Occupants) guardDefault(ctx context.Context, cur store.Occupant, staysAlwaysOn bool) error {
	if staysAlwaysOn || !isAlwaysOnMember(cur) {
		return nil
	}
	defaults, err := m.Read.Defaults(ctx)
	if err != nil {
		return err
	}
	var fns []string
	for _, fn := range store.DefaultFunctions(defaults) {
		if defaults[fn] == cur.Name {
			fns = append(fns, fn)
		}
	}
	if len(fns) == 0 {
		return nil
	}
	list := strings.Join(fns, ", ")
	return &ConflictError{Code: "default_engine_in_use", Params: map[string]any{"functions": list},
		Message: cur.Name + " is the default for " + list + "; set another default first"}
}

// Create adds an occupant after the rules pass.
func (m *Occupants) Create(ctx context.Context, o store.Occupant) (store.Occupant, error) {
	if err := m.check(ctx, o, ""); err != nil {
		return store.Occupant{}, err
	}
	if o.Kind == KindCatalogConfig && o.MemoryBytes == 0 && m.Catalog != nil {
		o.MemoryBytes = m.Catalog.EstimateBytes(ctx, o.Params["config"]) // default to the catalog's own figure
	}
	if _, err := m.Write.CreateOccupant(ctx, o); err != nil {
		if isUnique(err) {
			return store.Occupant{}, ErrAlreadyExists
		}
		return store.Occupant{}, &ValidationError{Fields: map[string]string{"body": err.Error()}}
	}
	return m.Read.OccupantByName(ctx, o.Name)
}

// Update replaces the occupant called name with in (a full replace: the caller sends everything). An edit never starts or stops a member.
func (m *Occupants) Update(ctx context.Context, name string, in store.Occupant) (store.Occupant, error) {
	cur, err := m.Read.OccupantByName(ctx, name)
	if err != nil {
		return store.Occupant{}, err
	}
	in.ID = cur.ID
	in.Stopped = cur.Stopped
	if err := m.guardDefault(ctx, cur, isAlwaysOnMember(in)); err != nil {
		return store.Occupant{}, err
	}
	if err := m.check(ctx, in, name); err != nil {
		return store.Occupant{}, err
	}
	if err := m.Write.UpdateOccupant(ctx, in); err != nil {
		return store.Occupant{}, &ValidationError{Fields: map[string]string{"body": err.Error()}}
	}
	return m.Read.OccupantByName(ctx, in.Name)
}

// SetPolicy changes only the policy and home slot (the narrow write behind the always-on / on-demand toggle, so it can never wipe the rest).
func (m *Occupants) SetPolicy(ctx context.Context, name, policy, homeSlot string) (store.Occupant, error) {
	o, err := m.Read.OccupantByName(ctx, name)
	if err != nil {
		return store.Occupant{}, err
	}
	if policy != store.PolicyAlwaysOn && policy != store.PolicyOnDemand {
		return store.Occupant{}, &ValidationError{Fields: map[string]string{"policy": "must be always_on or on_demand"}, Codes: map[string]string{"policy": "must_be_one_of"}}
	}
	if err := m.guardDefault(ctx, o, policy == store.PolicyAlwaysOn); err != nil {
		return store.Occupant{}, err
	}
	o.Policy, o.HomeSlot = policy, homeSlot
	if o.Policy == store.PolicyAlwaysOn && o.HomeSlot == "" {
		_, o.HomeSlot, _ = m.slotsOf(ctx, name) // keep it where it is when no home is given
	}
	if err := m.check(ctx, o, name); err != nil {
		return store.Occupant{}, err
	}
	if err := m.Write.UpdateOccupant(ctx, o); err != nil {
		return store.Occupant{}, &ValidationError{Fields: map[string]string{"body": err.Error()}}
	}
	return m.Read.OccupantByName(ctx, name)
}

// Delete removes an occupant and its defaults. A member that is running or starting must be unloaded first.
func (m *Occupants) Delete(ctx context.Context, name string) error {
	if m.Pool != nil {
		for _, st := range m.Pool.Snapshot(ctx) {
			if st.Occupant == name && (st.State == StateRunning || st.State == StateStarting) {
				return &ConflictError{Code: "has_dependents", Resource: "running slot " + st.Slot, Message: "unload it from " + st.Slot + " first"}
			}
		}
	}
	if err := m.Write.DeleteOccupant(ctx, name); err != nil {
		return err
	}
	if m.Pool != nil {
		m.Pool.Forget(name)
	}
	return nil
}

// SetDefault points a function at an always-on member, or clears the default when occupant is empty. It returns the defaults as they now stand.
func (m *Occupants) SetDefault(ctx context.Context, function, occupant string) (map[string]string, error) {
	if occupant != "" {
		cand, err := m.Read.OccupantByName(ctx, occupant)
		if err != nil {
			return nil, err
		}
		if !isAlwaysOnMember(cand) {
			return nil, &ValidationError{
				Fields: map[string]string{"occupant": occupant + " must be an enabled always-on member to be the default for " + function},
				Codes:  map[string]string{"occupant": "engine_must_be_always_on"},
			}
		}
	}
	if err := m.Write.SetDefault(ctx, function, occupant); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		return nil, &ValidationError{Fields: map[string]string{"occupant": err.Error()}}
	}
	return m.Read.Defaults(ctx)
}

// SlotRemovable reports why a specialist slot can't be removed (something is in it, or an always-on member lives there); nil means it can.
func (m *Occupants) SlotRemovable(ctx context.Context, slot string) error {
	if m.Pool != nil {
		for _, st := range m.Pool.Snapshot(ctx) {
			if st.Slot == slot && st.Occupant != "" {
				return &ConflictError{Code: "has_dependents", Resource: st.Occupant + " in " + slot, Message: "empty " + slot + " first"}
			}
		}
	}
	occs, err := m.Read.ListOccupants(ctx)
	if err != nil {
		return err
	}
	for _, o := range occs {
		if o.Policy == store.PolicyAlwaysOn && o.HomeSlot == slot {
			return &ConflictError{Code: "has_dependents", Resource: o.Name + " (home)", Message: o.Name + " lives in " + slot}
		}
	}
	return nil
}

// BehaviorDisabled is the third behavior, next to store.PolicyAlwaysOn and store.PolicyOnDemand.
const BehaviorDisabled = "disabled"

// BehaviorResult is what SetBehavior did. Stopping means the member is still winding down in Slot and will finish on its own.
type BehaviorResult struct {
	Occupant store.Occupant
	Stopping bool
	Slot     string
}

// SetBehavior is the single write behind Always on / On demand / Disabled. It touches only behavior, never the rest of the occupant. Disabled
// unloads a running member and frees its slot; enabling clears any earlier stop and, for Always on, picks a free slot and loads the member.
// ctx ends the wait for a slow stop, not the stop itself.
func (m *Occupants) SetBehavior(ctx context.Context, name, behavior string) (BehaviorResult, error) {
	o, err := m.Read.OccupantByName(ctx, name)
	if err != nil {
		return BehaviorResult{}, err
	}
	if behavior != store.PolicyAlwaysOn && behavior != store.PolicyOnDemand && behavior != BehaviorDisabled {
		return BehaviorResult{}, &ValidationError{Fields: map[string]string{"behavior": "must be always_on, on_demand or disabled"}, Codes: map[string]string{"behavior": "must_be_one_of"}}
	}
	if err := m.guardDefault(ctx, o, behavior == store.PolicyAlwaysOn); err != nil {
		return BehaviorResult{}, err
	}
	running, recorded, recordedState := m.slotsOf(ctx, name)

	if behavior == BehaviorDisabled {
		// Stop it being (re)started first, then free its slot.
		o.Enabled = false
		if err := m.Write.UpdateOccupant(ctx, o); err != nil {
			return BehaviorResult{}, err
		}
		// After the stop: clear the stop flag, and drop the slot's record of the member so Disabled really frees the slot (an always-on member's
		// stop otherwise leaves it showing as "stopped" in its slot).
		clearStop := func(ctx context.Context) error {
			if err := m.Write.SetStopped(ctx, name, false); err != nil {
				return err
			}
			if m.Pool != nil {
				m.Pool.Forget(name)
			}
			return nil
		}
		stopping := false
		if running != "" && m.Pool != nil {
			// The stop may outlast the client's timeout (a member mid-generation): wait a bounded time, then report "stopping" and let it finish.
			// clearStop runs after the stop either way, so it can never be overwritten by the stop's own "operator stopped" record.
			finished, uerr := m.UnloadWithin(ctx, running, clearStop)
			if uerr != nil {
				return BehaviorResult{}, &ConflictError{Code: "operation_failed", Message: uerr.Error()}
			}
			stopping = !finished
		} else if err := clearStop(ctx); err != nil {
			return BehaviorResult{}, err
		}
		got, err := m.Read.OccupantByName(ctx, name)
		if err != nil {
			return BehaviorResult{}, err
		}
		return BehaviorResult{Occupant: got, Stopping: stopping, Slot: running}, nil
	}

	o.Enabled, o.Policy = true, behavior
	if behavior == store.PolicyAlwaysOn {
		home := running
		if home == "" {
			home = recorded // a stopped or failed member still holds its own slot
		}
		if home == "" {
			home = m.freeSlot(ctx, o.HomeSlot)
		}
		if home == "" {
			return BehaviorResult{}, &ConflictError{Code: "specialist_no_free_slot", Message: "every specialist slot is taken; add a slot or set another specialist to On demand or Disabled first"}
		}
		o.HomeSlot = home
	}
	if err := m.check(ctx, o, name); err != nil {
		return BehaviorResult{}, err
	}
	if err := m.Write.UpdateOccupant(ctx, o); err != nil {
		return BehaviorResult{}, &ValidationError{Fields: map[string]string{"body": err.Error()}}
	}
	if err := m.Write.SetStopped(ctx, name, false); err != nil {
		return BehaviorResult{}, err
	}
	// A member whose slot is still being unloaded cannot be loaded yet: the behavior is saved, and the supervisor starts it as soon as the stop ends
	// (an always-on member that is enabled and not stopped is its job), so this is not an error.
	if behavior == store.PolicyAlwaysOn && running == "" && recordedState != StateStopping && m.Pool != nil {
		if _, err := m.Pool.Load(ctx, name, o.HomeSlot); err != nil {
			return BehaviorResult{}, &PoolError{Err: err}
		}
	}
	got, err := m.Read.OccupantByName(ctx, name)
	if err != nil {
		return BehaviorResult{}, err
	}
	return BehaviorResult{Occupant: got}, nil
}

// UnloadWithin stops slot, waiting at most UnloadWait. The stop runs on a context detached from ctx, so a client that gives up (or the
// answer-fast timer) never abandons it half-way. then, if set, runs after a successful stop, in the same goroutine. It reports finished=false when
// the stop is still running: the slot reads "stopping" until it ends, and a stop that later fails is logged and left in the slot's Err.
func (m *Occupants) UnloadWithin(ctx context.Context, slot string, then func(ctx context.Context) error) (finished bool, err error) {
	if m.Pool == nil {
		return false, errors.New("specialist pool not running")
	}
	budget, wait := m.UnloadBudget, m.UnloadWait
	if budget <= 0 {
		budget = 3 * time.Minute
	}
	if wait <= 0 {
		wait = 10 * time.Second
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	done := make(chan error, 1)
	go func() {
		defer cancel()
		err := m.Pool.Unload(bg, slot)
		if err == nil && then != nil {
			err = then(bg)
		}
		if err != nil {
			log.Printf("specialist: unload of %s: %v", slot, err)
		}
		done <- err
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-done:
		return true, err
	case <-timer.C:
		return false, nil
	case <-ctx.Done():
		return false, nil
	}
}

// slotsOf reports where a member is: running is the slot it is running or starting in; recorded is the slot that holds its record in any state
// (a stopped or failed member keeps its slot, but is not running there and must be loaded again).
func (m *Occupants) slotsOf(ctx context.Context, name string) (running, recorded, recordedState string) {
	if m.Pool == nil {
		return "", "", ""
	}
	for _, st := range m.Pool.Snapshot(ctx) {
		if st.Occupant != name {
			continue
		}
		recorded, recordedState = st.Slot, st.State
		if st.State == StateRunning || st.State == StateStarting {
			running = st.Slot
		}
	}
	return running, recorded, recordedState
}

// freeSlot picks the slot an always-on member will live in: the one it had before if it is still free, else the first free slot.
func (m *Occupants) freeSlot(ctx context.Context, prefer string) string {
	if m.Pool == nil {
		return ""
	}
	var free []string
	for _, st := range m.Pool.Snapshot(ctx) {
		if st.Occupant == "" {
			free = append(free, st.Slot)
		}
	}
	sort.Strings(free)
	for _, f := range free {
		if f == prefer {
			return f
		}
	}
	if len(free) > 0 {
		return free[0]
	}
	return ""
}
