// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Target is a loaded pool member a request can be sent to.
type Target struct {
	Occupant string
	Slot     string
	// BaseURL is http://127.0.0.1:<port> with no path; the caller appends the function's upstream path.
	BaseURL string
	// Port is the member's listening port.
	Port int
	// Touch marks the member as just used (the on-demand idle clock).
	Touch func()
	// Release gives back the lease taken with the target (idempotent) and starts the idle clock from NOW. Every request holds a lease while it runs:
	// an on-demand member with a lease is never evicted, so the request cannot lose its engine mid-flight. A lease also expires on its own (the
	// lease TTL) so a caller that never releases cannot pin a member. Nil for a Target not made by a Manager (test fakes): use Finish.
	Release func()
	// Renew extends the lease a full TTL, for a request that may outlive it (a long stream); it reports false once the lease is gone.
	Renew func() bool
	// LeaseID and Expires identify the lease taken with the target, for a caller in another process that renews and releases it over the API
	// (forge-duplex, ADR-0029). Empty for a Target not made by a Manager.
	LeaseID string
	Expires time.Time
}

// Finish is what a caller does when its request has ended: release the lease, or, for a Target without one, touch the idle clock.
func (t Target) Finish() {
	switch {
	case t.Release != nil:
		t.Release()
	case t.Touch != nil:
		t.Touch()
	}
}

// leaseTarget builds a Target for engine `name` loaded in `slot`, holding a fresh lease.
func (m *Manager) leaseTarget(name, slot string, port int) (Target, error) {
	id, expires, err := m.grantLease(name, slot)
	if err != nil {
		return Target{}, err
	}
	return Target{Occupant: name, Slot: slot, BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port), Port: port,
		Touch:   func() { m.Touch(slot) },
		Release: func() { m.ReleaseSpeech(id) },
		Renew:   func() bool { return m.RenewLease(id) },
		LeaseID: id, Expires: expires}, nil
}

// ErrNoMatch means the pool has no member for this request (no member of that name and no default for the function). The caller keeps
// whatever it did before the pool existed, so nothing changes until the pool is configured.
var ErrNoMatch = errors.New("specialist: no pool member for this request")

// Resolve answers a request for function with a loaded member (ADR-0022 decision 6): the member named by model if there is one, otherwise the
// function's default. A model name that is not a pool member falls back to the default (a client may send any model string, e.g. "tts-1" or
// "text-embedding-3-small"). It loads the member on demand and returns *PoolFullError or *UnavailableError when it cannot.
func (m *Manager) Resolve(ctx context.Context, function, model string) (Target, error) {
	if len(m.d.Cfg().SpecialistSlots) == 0 {
		return Target{}, ErrNoMatch // a host with no engine slots has no pool to answer from: the caller carries on as before
	}
	name := ""
	if model != "" {
		o, err := m.d.Store.OccupantByName(ctx, model)
		switch {
		case err == nil:
			if !serves(o, function) {
				return Target{}, &UnavailableError{Occupant: model, Reason: fmt.Sprintf("it serves %v, not %s", o.Functions, function)}
			}
			name = model
		case errors.Is(err, store.ErrNotFound):
		default:
			return Target{}, err
		}
	}
	if name == "" {
		defaults, err := m.d.Store.Defaults(ctx)
		if err != nil {
			return Target{}, err
		}
		if name = defaults[function]; name == "" {
			return Target{}, ErrNoMatch
		}
	}
	return m.leaseNamed(ctx, name)
}

// leaseNamed loads the named member on demand (if it is not running) and takes a lease on it.
func (m *Manager) leaseNamed(ctx context.Context, name string) (Target, error) {
	if err := m.leaseRoom(); err != nil {
		return Target{}, err
	}
	st, err := m.Ensure(ctx, name)
	if err != nil {
		return Target{}, err
	}
	o, err := m.d.Store.OccupantByName(ctx, name)
	if err != nil {
		return Target{}, err
	}
	port := st.Port
	if k, ok := KindByName(o.Kind); ok && k.FixedPort != 0 {
		port = k.FixedPort
	}
	return m.leaseTarget(name, st.Slot, port)
}

// ResolveDuplex finds the duplex engine for one session (ADR-0029): the member named by name, or the function's default when name is empty. Unlike
// Resolve it never falls back to another member: with several speech-to-speech models a mistyped name must fail, not quietly talk to a different
// model. Errors: ErrNotFound (no such member), *UnavailableError (it does not serve duplex, is disabled or stopped, is starting past its timeout),
// ErrNoMatch (no name and no default), *PoolFullError, ErrTooManyLeases. The member is loaded on demand and the returned Target carries the lease.
func (m *Manager) ResolveDuplex(ctx context.Context, name string) (Target, error) {
	if name == "" {
		defaults, err := m.d.Store.Defaults(ctx)
		if err != nil {
			return Target{}, err
		}
		if name = defaults[store.FnDuplex]; name == "" {
			return Target{}, ErrNoMatch
		}
	}
	o, err := m.d.Store.OccupantByName(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Target{}, ErrNotFound
	case err != nil:
		return Target{}, err
	case !serves(o, store.FnDuplex):
		return Target{}, &UnavailableError{Occupant: name, Reason: fmt.Sprintf("it serves %v, not %s", o.Functions, store.FnDuplex)}
	}
	return m.leaseNamed(ctx, name)
}

func serves(o store.Occupant, function string) bool {
	for _, f := range o.Functions {
		if f == function {
			return true
		}
	}
	return false
}

// occupantForConfig finds the enabled catalog-config occupant that hosts the named catalog config.
func (m *Manager) occupantForConfig(ctx context.Context, config string) (store.Occupant, bool) {
	for _, o := range m.occupantsByName(ctx) {
		if o.Enabled && o.Kind == KindCatalogConfig && o.Params["config"] == config {
			return o, true
		}
	}
	return store.Occupant{}, false
}

// PooledConfig reports whether the named catalog config is hosted by the specialist pool. The one-owner rule hangs on this: a pooled config is
// never loaded into an LLM slot (the scheduler and the engine ask), and a0 routes it to the pool.
func (m *Manager) PooledConfig(ctx context.Context, config string) bool {
	if len(m.d.Cfg().SpecialistSlots) == 0 {
		return false
	}
	_, ok := m.occupantForConfig(ctx, config)
	return ok
}

// ResolveConfig answers a request for a pooled catalog config with its loaded member, loading it on demand. ErrNoMatch means the config is not
// pooled and the caller uses the scheduler as it always did.
func (m *Manager) ResolveConfig(ctx context.Context, config string) (Target, error) {
	if len(m.d.Cfg().SpecialistSlots) == 0 {
		return Target{}, ErrNoMatch // as Resolve: without slots the scheduler serves the config
	}
	o, ok := m.occupantForConfig(ctx, config)
	if !ok {
		return Target{}, ErrNoMatch
	}
	if err := m.leaseRoom(); err != nil {
		return Target{}, err
	}
	st, err := m.Ensure(ctx, o.Name)
	if err != nil {
		return Target{}, err
	}
	return m.leaseTarget(o.Name, st.Slot, st.Port)
}
