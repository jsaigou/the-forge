// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeOccPool is the runtime pool as the rules see it: slot states in, load/unload/forget recorded.
type fakeOccPool struct {
	mu        sync.Mutex
	slots     []SlotState
	unloadErr error
	hold      chan struct{} // when set, Unload blocks until it is closed
	unloaded  []string
	forgot    []string
	loaded    []string
}

func (p *fakeOccPool) Snapshot(context.Context) []SlotState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]SlotState(nil), p.slots...)
}
func (p *fakeOccPool) Load(_ context.Context, name, slot string) (SlotState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loaded = append(p.loaded, name+"@"+slot)
	return SlotState{Slot: slot, Occupant: name, State: StateRunning}, nil
}
func (p *fakeOccPool) Unload(_ context.Context, slot string) error {
	if p.hold != nil {
		<-p.hold
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unloaded = append(p.unloaded, slot)
	return p.unloadErr
}
func (p *fakeOccPool) Forget(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forgot = append(p.forgot, name)
}

type fakeOccCatalog struct{ specs map[string]LaunchSpec }

func (c fakeOccCatalog) LaunchSpec(_ context.Context, config string) (LaunchSpec, error) {
	if s, ok := c.specs[config]; ok {
		return s, nil
	}
	return LaunchSpec{}, errors.New("no such config " + config)
}
func (c fakeOccCatalog) LaunchRow(context.Context, string, string, int) (store.SlotLaunch, error) {
	return store.SlotLaunch{}, nil
}
func (c fakeOccCatalog) EstimateBytes(context.Context, string) int64 { return 7 << 30 }

// listFailsReader is the real store with ListOccupants switched to fail on demand.
type listFailsReader struct {
	Reader
	fail bool
}

func (r *listFailsReader) ListOccupants(ctx context.Context) ([]store.Occupant, error) {
	if r.fail {
		return nil, errors.New("disk I/O error")
	}
	return r.Reader.ListOccupants(ctx)
}

func newOccupants(t *testing.T) (*Occupants, *fakeOccPool, *listFailsReader) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	good := LaunchSpec{Bin: "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", Model: "/opt/forge/models/d1.gguf", Alias: "d1-2b", Backend: "vulkan", Ctx: 128000}
	pool := &fakeOccPool{slots: []SlotState{{Slot: "s1", State: StateEmpty}, {Slot: "s2", State: StateEmpty}}}
	rd := &listFailsReader{Reader: db.Specialist()}
	return &Occupants{
		Read: rd, Write: db.Specialist(), Pool: pool, Catalog: fakeOccCatalog{specs: map[string]LaunchSpec{"d1-2b": good}},
		HomeSlots:  func() map[string]bool { return map[string]bool{"s1": true, "s2": true} },
		ModelRoots: func() []string { return []string{"/opt/forge/models"} },
		UnloadWait: 50 * time.Millisecond, UnloadBudget: 5 * time.Second,
	}, pool, rd
}

func catalogOcc(name string, enabled bool) store.Occupant {
	return store.Occupant{Name: name, Kind: KindCatalogConfig, Functions: []string{"decisions"}, Policy: store.PolicyOnDemand, Enabled: enabled,
		Params: map[string]string{"config": "d1-2b"}}
}

func kokoro(name, policy, home string) store.Occupant {
	return store.Occupant{Name: name, Kind: "kokoro", Functions: []string{"speech"}, Policy: policy, HomeSlot: home, Enabled: true}
}

func wantField(t *testing.T, err error, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields[field] == "" {
		t.Fatalf("want a validation error on %q, got %v", field, err)
	}
}

func wantFieldCode(t *testing.T, err error, field, code string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Codes[field] != code {
		t.Fatalf("want field %q to carry code %q, got %v", field, code, err)
	}
}

func wantConflict(t *testing.T, err error, code string) {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("want a %s conflict, got %v", code, err)
	}
}

func TestOccupants_ConfigHasOneEnabledOwner(t *testing.T) {
	m, _, _ := newOccupants(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, catalogOcc("a", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, catalogOcc("b", true)); err == nil {
		t.Fatal("a second enabled owner was accepted")
	} else {
		wantField(t, err, "params")
	}
	// A disabled occupant holds nothing, so it may be created for an owned config.
	if _, err := m.Create(ctx, catalogOcc("c", false)); err != nil {
		t.Fatalf("disabled occupant for an owned config: %v", err)
	}
	// Disabling the owner releases the config; re-enabling is checked like a create.
	if _, err := m.SetBehavior(ctx, "a", BehaviorDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, catalogOcc("b", true)); err != nil {
		t.Fatalf("a disabled owner must release its config: %v", err)
	}
	_, err := m.SetBehavior(ctx, "a", store.PolicyOnDemand)
	wantField(t, err, "params")
	if _, err := m.SetBehavior(ctx, "b", BehaviorDisabled); err != nil {
		t.Fatal(err)
	}
	if res, err := m.SetBehavior(ctx, "a", store.PolicyOnDemand); err != nil || !res.Occupant.Enabled {
		t.Fatalf("re-enable once the config is free: %v %+v", err, res)
	}
}

func TestOccupants_StoreReadFailureIsNotAPassedCheck(t *testing.T) {
	m, _, rd := newOccupants(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, catalogOcc("a", true)); err != nil {
		t.Fatal(err)
	}
	rd.fail = true
	// Both rules read the other occupants: a failed read refuses the write, with the read error and not a validation error.
	for name, o := range map[string]store.Occupant{"one-owner": catalogOcc("b", true), "home-slot": kokoro("tts", store.PolicyAlwaysOn, "s1")} {
		_, err := m.Create(ctx, o)
		var ve *ValidationError
		if err == nil || errors.As(err, &ve) {
			t.Errorf("%s with a failing store: %v, want the read error", name, err)
		}
	}
	rd.fail = false
	list, _ := m.Read.ListOccupants(ctx)
	if len(list) != 1 {
		t.Errorf("a refused write left occupants behind: %v", list)
	}
}

func TestOccupants_HomeSlots(t *testing.T) {
	m, _, _ := newOccupants(t)
	ctx := context.Background()
	_, err := m.Create(ctx, kokoro("tts", store.PolicyAlwaysOn, ""))
	wantField(t, err, "home_slot")
	_, err = m.Create(ctx, kokoro("tts", store.PolicyAlwaysOn, "s9"))
	wantField(t, err, "home_slot")
	if _, err := m.Create(ctx, kokoro("tts", store.PolicyAlwaysOn, "s1")); err != nil {
		t.Fatal(err)
	}
	_, err = m.Create(ctx, kokoro("tts2", store.PolicyAlwaysOn, "s1"))
	wantField(t, err, "home_slot")
	// An edit doesn't conflict with itself.
	if _, err := m.Update(ctx, "tts", kokoro("tts", store.PolicyAlwaysOn, "s1")); err != nil {
		t.Fatalf("editing in place: %v", err)
	}
}

func TestOccupants_DefaultMustBeAlwaysOnAndStaysSo(t *testing.T) {
	m, _, _ := newOccupants(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, kokoro("tts", store.PolicyOnDemand, "")); err != nil {
		t.Fatal(err)
	}
	_, err := m.SetDefault(ctx, "speech", "tts")
	wantField(t, err, "occupant")
	wantFieldCode(t, err, "occupant", "engine_must_be_always_on")
	if _, err := m.SetPolicy(ctx, "tts", store.PolicyAlwaysOn, "s1"); err != nil {
		t.Fatal(err)
	}
	defaults, err := m.SetDefault(ctx, "speech", "tts")
	if err != nil || defaults["speech"] != "tts" {
		t.Fatalf("always-on default: %v %v", defaults, err)
	}
	_, err = m.SetPolicy(ctx, "tts", store.PolicyOnDemand, "")
	wantConflict(t, err, "default_engine_in_use")
	_, err = m.SetBehavior(ctx, "tts", BehaviorDisabled)
	wantConflict(t, err, "default_engine_in_use")
	_, err = m.SetBehavior(ctx, "tts", store.PolicyOnDemand)
	wantConflict(t, err, "default_engine_in_use")
	_, err = m.Update(ctx, "tts", kokoro("tts", store.PolicyOnDemand, ""))
	wantConflict(t, err, "default_engine_in_use")
	var ce *ConflictError
	if errors.As(err, &ce); ce == nil || ce.Params["functions"] != "speech" {
		t.Errorf("the refusal must name the functions for the translated text: %+v", ce)
	}
	if _, err := m.Update(ctx, "tts", kokoro("tts", store.PolicyAlwaysOn, "s1")); err != nil {
		t.Fatalf("an edit that stays always-on: %v", err)
	}
	// Clearing the default frees it.
	if _, err := m.SetDefault(ctx, "speech", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetBehavior(ctx, "tts", BehaviorDisabled); err != nil {
		t.Fatalf("disable once it is not a default: %v", err)
	}
}

func TestOccupants_DeleteRefusesARunningMemberAndClearsDefaults(t *testing.T) {
	m, pool, _ := newOccupants(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, kokoro("tts", store.PolicyAlwaysOn, "s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetDefault(ctx, "speech", "tts"); err != nil {
		t.Fatal(err)
	}
	pool.slots[0] = SlotState{Slot: "s1", Occupant: "tts", State: StateRunning}
	wantConflict(t, m.Delete(ctx, "tts"), "has_dependents")
	pool.slots[0] = SlotState{Slot: "s1", Occupant: "tts", State: StateStopped}
	if err := m.Delete(ctx, "tts"); err != nil {
		t.Fatal(err)
	}
	if d, _ := m.Read.Defaults(ctx); len(d) != 0 {
		t.Errorf("a default survived its occupant: %v", d)
	}
	if len(pool.forgot) != 1 || pool.forgot[0] != "tts" {
		t.Errorf("the runtime record was not dropped: %v", pool.forgot)
	}
	if err := m.Delete(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a stranger: %v", err)
	}
}

func TestOccupants_DisableUnloadsAndReportsASlowStop(t *testing.T) {
	m, pool, _ := newOccupants(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, kokoro("tts", store.PolicyAlwaysOn, "s1")); err != nil {
		t.Fatal(err)
	}
	pool.slots[0] = SlotState{Slot: "s1", Occupant: "tts", State: StateRunning}
	pool.hold = make(chan struct{})

	res, err := m.SetBehavior(ctx, "tts", BehaviorDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopping || res.Slot != "s1" || res.Occupant.Enabled {
		t.Fatalf("a slow stop must report stopping and leave the member disabled: %+v", res)
	}
	close(pool.hold) // the stop finishes in the background, then the follow-up runs
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.mu.Lock()
		done := len(pool.forgot) == 1
		pool.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the follow-up after the stop never ran")
}

func TestOccupants_AlwaysOnPicksAFreeSlotOrRefuses(t *testing.T) {
	m, pool, _ := newOccupants(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, kokoro("a", store.PolicyOnDemand, "")); err != nil {
		t.Fatal(err)
	}
	res, err := m.SetBehavior(ctx, "a", store.PolicyAlwaysOn)
	if err != nil || res.Occupant.HomeSlot != "s1" || len(pool.loaded) != 1 || pool.loaded[0] != "a@s1" {
		t.Fatalf("always-on: %v %+v loaded=%v", err, res, pool.loaded)
	}
	// Every slot taken: refuse with the stable code, change nothing.
	pool.slots = []SlotState{{Slot: "s1", Occupant: "x", State: StateRunning}, {Slot: "s2", Occupant: "y", State: StateRunning}}
	if _, err := m.Create(ctx, kokoro("b", store.PolicyOnDemand, "")); err != nil {
		t.Fatal(err)
	}
	_, err = m.SetBehavior(ctx, "b", store.PolicyAlwaysOn)
	wantConflict(t, err, "specialist_no_free_slot")
	if got, _ := m.Read.OccupantByName(ctx, "b"); got.Policy != store.PolicyOnDemand {
		t.Errorf("a refused change altered the member: %+v", got)
	}
}

func TestOccupants_SlotRemovable(t *testing.T) {
	m, pool, rd := newOccupants(t)
	ctx := context.Background()
	if err := m.SlotRemovable(ctx, "s2"); err != nil {
		t.Fatalf("an empty slot: %v", err)
	}
	pool.slots[1] = SlotState{Slot: "s2", Occupant: "x", State: StateRunning}
	wantConflict(t, m.SlotRemovable(ctx, "s2"), "has_dependents")
	pool.slots[1] = SlotState{Slot: "s2", State: StateEmpty}
	if _, err := m.Create(ctx, kokoro("tts", store.PolicyAlwaysOn, "s2")); err != nil {
		t.Fatal(err)
	}
	wantConflict(t, m.SlotRemovable(ctx, "s2"), "has_dependents")
	// A failed read of the occupants must not read as "nothing lives there".
	rd.fail = true
	err := m.SlotRemovable(ctx, "s1")
	var ce *ConflictError
	if err == nil || errors.As(err, &ce) {
		t.Errorf("slot check with a failing store: %v, want the read error", err)
	}
}
