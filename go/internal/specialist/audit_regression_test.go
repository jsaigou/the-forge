// SPDX-License-Identifier: Apache-2.0

package specialist

// Audit reproductions (concurrency / failure handling). Tests only; they demonstrate bugs and are expected to FAIL until the production code is fixed.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// Regression (was a bug, fixed): Unload of an always-on member persists "stopped" only AFTER the stop, and leaves the slot in StateStopped even when, while
// the stop ran, the operator flipped the member back to enabled/always-on (handleSpecialistBehavior: SetStopped(false) + a refused Load) and the
// disable's `then` clearStop ran after Unload. End state: store says enabled, always_on, stopped=false, but the slot record is StateStopped, and
// neither Tick (dueForStart returns false for StateStopped) nor Ensure (reports "stopped by the operator") ever starts it.
func TestAudit_UnloadRacingAReEnableLeavesAMemberEnabledButNeverLoaded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.health(8100, true)
	e.add(t, "ao", store.PolicyAlwaysOn, "s1", 1<<30)
	if _, err := e.m.Load(ctx, "ao", "s1"); err != nil {
		t.Fatal(err)
	}
	gate := &gatedSys{fakeSys: e.sys, entered: make(chan struct{}), release: make(chan struct{})}
	e.m.d.Sys = gate

	// Request 1 (behavior=disabled): disable in the store, then the background Unload followed by clearStop.
	o, _ := e.sp.OccupantByName(ctx, "ao")
	o.Enabled = false
	if err := e.sp.UpdateOccupant(ctx, o); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if err := e.m.Unload(ctx, "s1"); err == nil {
			_ = e.sp.SetStopped(ctx, "ao", false) // the handler's `then`
		}
	}()
	<-gate.entered

	// Request 2 (behavior=always_on) while the slot is stopping: enable, clear the stop, then Load into the recorded slot (refused).
	o.Enabled, o.Policy, o.HomeSlot = true, store.PolicyAlwaysOn, "s1"
	if err := e.sp.UpdateOccupant(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := e.sp.SetStopped(ctx, "ao", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Load(ctx, "ao", "s1"); err == nil {
		t.Fatal("Load into a stopping slot was accepted")
	}
	close(gate.release)
	<-finished

	got, _ := e.sp.OccupantByName(ctx, "ao")
	if !got.Enabled || got.Stopped || got.Policy != store.PolicyAlwaysOn {
		t.Fatalf("store state not as the scenario assumes: %+v", got)
	}
	for i := 0; i < 3; i++ {
		e.m.Tick(ctx)
		e.advance(time.Minute)
	}
	if st := state(t, e, "s1"); st.State != StateRunning {
		t.Errorf("enabled, always-on, not stopped, but the slot is %q (err %q): the supervisor never starts it", st.State, st.Err)
	}
}

// ctxStateSys is a systemd whose calls fail once the context is done (as D-Bus calls do) and counts State polls.
type ctxStateSys struct {
	*fakeSys
	states atomic.Int64
}

func (c *ctxStateSys) Stop(ctx context.Context, u string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.fakeSys.Stop(ctx, u)
}
func (c *ctxStateSys) State(ctx context.Context, u string) (collector.UnitState, error) {
	c.states.Add(1)
	if err := ctx.Err(); err != nil {
		return collector.UnitState{}, err
	}
	return collector.UnitState{ActiveState: "deactivating", SubState: "stop-sigterm"}, nil // a member winding down its generation
}

// Regression (was a bug, fixed): stopUnit never checks ctx.Err(). When the caller's context ends mid-stop (specialist_infra.go passes r.Context() straight to
// Unload; a client disconnect cancels it) every State call errors, the real Sleep returns at once on a done ctx, and the loop hot-spins on the
// 75 s wall-clock deadline. It should return ctx.Err() promptly.
func TestAudit_StopUnitSpinsAfterTheContextIsCancelled(t *testing.T) {
	e := newEnv(t)
	e.add(t, "a", store.PolicyOnDemand, "", 0)
	e.health(8102, true)
	if _, err := e.m.Load(context.Background(), "a", "s3"); err != nil {
		t.Fatal(err)
	}
	cs := &ctxStateSys{fakeSys: e.sys}
	e.m.d.Sys = cs
	e.m.d.Sleep = func(ctx context.Context, _ time.Duration) { <-ctx.Done() } // the real Sleep: returns at once on a done ctx
	// A frozen clock stands in for "well inside the 75 s wall-clock window".
	frozen := e.clock()
	e.m.d.Now = func() time.Time { return frozen }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.m.stopUnit(ctx, "s3") }()
	time.Sleep(20 * time.Millisecond)
	cancel() // Stop was already issued; now the caller goes away
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Errorf("stopUnit still running 300 ms after its context ended (%d State polls so far): a busy loop until the 75 s deadline", cs.states.Load())
		e.mu.Lock()
		e.now = frozen.Add(time.Hour) // let it finish
		e.mu.Unlock()
		e.m.d.Now = e.clock
		<-done
	}
}

// Regression (was a bug, fixed): the default Busy probe treats an unreachable or failing /metrics as "not busy" (collector.LlamaClient.Metrics returns nil on
// any error; manager.go New maps nil to false), so a placement request evicts a member that may be mid-generation. It should fail closed (keep
// the member) when busy cannot be determined.
func TestAudit_BusyProbeErrorFailsOpenAndEvictsTheMember(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	cfg := e.m.d.Cfg()
	s1 := cfg.SpecialistSlots["s1"]
	s1.Port = port
	cfg.SpecialistSlots["s1"] = s1

	// Use the manager's own default Busy (built by New when Busy is nil).
	d := e.m.d
	d.Busy = nil
	m2, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	e.m.d.Busy = m2.d.Busy

	e.cat.specs["d1-2b"] = d1Spec()
	e.addCatalog(t, "llm", "d1-2b", store.PolicyOnDemand, "")
	e.add(t, "a", store.PolicyOnDemand, "", 0)
	e.add(t, "b", store.PolicyOnDemand, "", 0)
	e.add(t, "c", store.PolicyOnDemand, "", 0)
	for _, p := range []int{port, 8101, 8102} {
		e.health(p, true)
	}
	for _, n := range []string{"llm", "a", "b"} {
		if _, err := e.m.Ensure(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	e.advance(10 * time.Minute)
	e.m.Touch("s2")
	e.m.Touch("s3")
	e.advance(190 * time.Second) // llm is least recently used and its /metrics errors
	got, err := e.m.Ensure(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Slot == "s1" {
		t.Errorf("c took s1 from llm although llm's busy state could not be read (metrics answered 500)")
	}
}

var _ = sync.Mutex{}
var _ = config.Slot{}
