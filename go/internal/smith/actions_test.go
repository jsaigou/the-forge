// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── shared action-model test fixtures ───────────────────────────────────────

// stubPlacer is a Placer fake. Zero value is a happy-path stub: FitPlan
// returns an empty (nothing-fits) Plan, Load/Unload both report success.
type stubPlacer struct {
	mu sync.Mutex

	plan    engine.Plan
	planErr error

	slotNames []string

	loadResult   *engine.Result // nil = default success
	unloadResult *engine.Result

	loads   []loadCall
	unloads []string
}

type loadCall struct{ Mode, Slot string }

var _ Placer = (*stubPlacer)(nil)

func (p *stubPlacer) FitPlan(string) (engine.Plan, error) {
	if p.planErr != nil {
		return engine.Plan{}, p.planErr
	}
	return p.plan, nil
}

func (p *stubPlacer) Load(_ context.Context, mode, slot string) engine.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loads = append(p.loads, loadCall{Mode: mode, Slot: slot})
	if p.loadResult != nil {
		return *p.loadResult
	}
	return engine.Result{Success: true, Message: "stub load ok"}
}

func (p *stubPlacer) Unload(_ context.Context, slot string) engine.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unloads = append(p.unloads, slot)
	if p.unloadResult != nil {
		return *p.unloadResult
	}
	return engine.Result{Success: true, Message: "stub unload ok"}
}

func (p *stubPlacer) Slots() []string {
	if len(p.slotNames) == 0 {
		return []string{"a1", "a2", "a3", "a4"}
	}
	return p.slotNames
}

// stubPublisher is a bus.Publisher fake that records every published event.
type stubPublisher struct {
	mu     sync.Mutex
	events []bus.Event
}

var _ bus.Publisher = (*stubPublisher)(nil)

func (p *stubPublisher) Publish(name string, data any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, bus.Event{Name: name, Data: data})
}

func (p *stubPublisher) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.events))
	for i, e := range p.events {
		out[i] = e.Name
	}
	return out
}

func (p *stubPublisher) has(name string) bool {
	for _, n := range p.names() {
		if n == name {
			return true
		}
	}
	return false
}

// fixedNow returns a func() time.Time always yielding t.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// buildSnapshotAt returns a static Source with an explicit TakenAt.
func buildSnapshotAt(at time.Time) collector.Source {
	snap := snapWith(collector.Metrics{})
	snap.TakenAt = at
	return collector.NewStatic(snap)
}

// mustJSON marshals v, failing the test on error.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// forceStatus overwrites an action's status directly (test setup only —
// bypasses the state machine on purpose, to seed a specific starting state
// without depending on another code path having already been proven
// correct).
func forceStatus(t *testing.T, db *store.DB, id int64, status string) {
	t.Helper()
	if _, err := db.SQL().Exec(`UPDATE smith_actions SET status = ? WHERE id = ?`, status, id); err != nil {
		t.Fatalf("force status: %v", err)
	}
}

// ── CreateAction validation ──────────────────────────────────────────────

func TestCreateActionValidation(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog()})
	ctx := context.Background()

	t.Run("unknown kind rejected", func(t *testing.T) {
		_, err := s.CreateAction(ctx, ActionDraft{Kind: "bogus", Risk: RiskLow, CreatedBy: "op"})
		if err == nil {
			t.Fatal("expected error for unknown kind")
		}
	})
	t.Run("invalid risk rejected", func(t *testing.T) {
		_, err := s.CreateAction(ctx, ActionDraft{Kind: KindRunbook, Risk: "extreme", CreatedBy: "op"})
		if err == nil {
			t.Fatal("expected error for invalid risk")
		}
	})
	t.Run("missing created_by rejected", func(t *testing.T) {
		_, err := s.CreateAction(ctx, ActionDraft{Kind: KindRunbook, Risk: RiskInfo, CreatedBy: ""})
		if err == nil {
			t.Fatal("expected error for missing created_by")
		}
	})
	t.Run("valid runbook draft persists with pending status", func(t *testing.T) {
		a, err := s.CreateAction(ctx, ActionDraft{Kind: KindRunbook, Title: "t", Risk: RiskInfo, CreatedBy: "op"})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if a.Status != StatusPending || a.SelfEvicting {
			t.Errorf("action = %+v, want pending/not self-evicting", a)
		}
		if string(a.Detail) != "{}" {
			t.Errorf("detail = %q, want empty object default", a.Detail)
		}
	})
}

// ── lifecycle / state machine ────────────────────────────────────────────

func TestRejectAction(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{Kind: KindRunbook, Risk: RiskInfo, CreatedBy: "op"})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	rejected, err := s.RejectAction(ctx, a.ID, "operator")
	if err != nil {
		t.Fatalf("RejectAction: %v", err)
	}
	if rejected.Status != StatusRejected {
		t.Errorf("status = %s, want rejected", rejected.Status)
	}

	// Illegal: reject an already-rejected action. Must fail AND leave the
	// row byte-identical to before this call.
	before, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if _, err := s.RejectAction(ctx, a.ID, "operator"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("second reject error = %v, want ErrInvalidTransition", err)
	}
	after, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("row mutated by an illegal transition:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestApproveActionOnNonPendingIsReadOnly(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{Kind: KindUnloadSlot, Risk: RiskHigh, CreatedBy: "op",
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a3"})})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := s.RejectAction(ctx, a.ID, "op"); err != nil {
		t.Fatalf("RejectAction: %v", err)
	}

	before, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	// ApproveAction on a rejected action: the pre-CAS status guard rejects
	// this before touching the DB at all — genuinely zero mutation risk,
	// so this case is race-free even though ApproveAction can otherwise
	// spawn a background executor.
	if _, err := s.ApproveAction(ctx, a.ID, "op"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("approve-after-reject error = %v, want ErrInvalidTransition", err)
	}
	after, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("row mutated by an illegal transition:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestApproveRunbookShortCircuit(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{Kind: KindRunbook, Title: "t", Risk: RiskInfo, CreatedBy: "smith"})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	approved, err := s.ApproveAction(ctx, a.ID, "operator")
	if err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}
	if approved.Status != StatusDoneUnverified {
		t.Fatalf("status = %s, want done_unverified (runbook never executes)", approved.Status)
	}
	if approved.ApprovedBy == nil || *approved.ApprovedBy != "operator" {
		t.Errorf("approved_by = %v, want operator", approved.ApprovedBy)
	}
	if approved.Result == nil || !approved.Result.OK {
		t.Errorf("result = %+v, want ok:true", approved.Result)
	}
	if approved.ExecutedAt == nil || approved.VerifiedAt == nil || approved.ResolvedAt == nil {
		t.Errorf("timestamps not fully stamped: %+v", approved)
	}
}

// TestConcurrentApprove proves the CAS is the concurrency control: two
// goroutines racing ApproveAction on the same pending, non-self-evicting
// action must produce exactly one success and one ErrInvalidTransition, with
// no package-level mutex involved. Run with -race.
func TestConcurrentApprove(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Now: time.Now, Logf: func(string, ...any) {}})
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindUnloadSlot, Risk: RiskHigh, CreatedBy: "op",
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a3"}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.ApproveAction(ctx, a.ID, "op")
			errs[i] = err
		}(i)
	}
	wg.Wait()

	successes, invalid := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrInvalidTransition):
			invalid++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 || invalid != 1 {
		t.Errorf("successes=%d invalid=%d, want exactly 1 and 1 (errs=%v)", successes, invalid, errs)
	}
}

func TestPendingActionCount(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.CreateAction(ctx, ActionDraft{Kind: KindRunbook, Risk: RiskInfo, CreatedBy: "op"}); err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
	}
	n, err := s.PendingActionCount(ctx)
	if err != nil {
		t.Fatalf("PendingActionCount: %v", err)
	}
	if n != 3 {
		t.Errorf("pending count = %d, want 3", n)
	}

	acted, err := s.ListActions(ctx, "", nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(acted) != 3 {
		t.Fatalf("ListActions = %d, want 3", len(acted))
	}
	if _, err := s.RejectAction(ctx, acted[0].ID, "op"); err != nil {
		t.Fatalf("RejectAction: %v", err)
	}
	n, err = s.PendingActionCount(ctx)
	if err != nil {
		t.Fatalf("PendingActionCount: %v", err)
	}
	if n != 2 {
		t.Errorf("pending count after reject = %d, want 2", n)
	}

	rejectedOnly, err := s.ListActions(ctx, StatusRejected, nil, 0)
	if err != nil {
		t.Fatalf("ListActions(rejected): %v", err)
	}
	if len(rejectedOnly) != 1 {
		t.Errorf("rejected-only list = %d, want 1", len(rejectedOnly))
	}
}

// TestLegalTransitionsTable cross-checks legalTransitions against what the
// real mutators actually do: every documented (from,to) pair must be one an
// actual code path performs, and every code path's (from,to) must be
// documented. Keeps the map (docs/v5-smith.md §4.6's reference table) from
// silently drifting out of sync with the hand-written per-statement CAS SQL
// that does the real enforcement.
func TestLegalTransitionsTable(t *testing.T) {
	cases := []struct {
		from, to string
	}{
		{StatusPending, StatusApproved},         // ApproveAction
		{StatusPending, StatusRejected},         // RejectAction
		{StatusPending, StatusSuperseded},       // propose.go's casSuperseded
		{StatusPending, StatusDoneUnverified},   // approveRunbook short-circuit
		{StatusApproved, StatusExecuting},       // executeAction's first CAS
		{StatusApproved, StatusFailed},          // failApproved (CAS-to-executing write error)
		{StatusExecuting, StatusDone},           // executeAction's finalize
		{StatusExecuting, StatusDoneUnverified}, // executeAction's finalize
		{StatusExecuting, StatusFailed},         // executeAction's finalize + reconcileExecuting
		{StatusDoneUnverified, StatusDone},      // self_review.go's promoteDoneUnverified
	}
	for _, tc := range cases {
		if !isLegalTransition(tc.from, tc.to) {
			t.Errorf("legalTransitions[%s][%s] = false, want true (a real code path performs this move)", tc.from, tc.to)
		}
	}

	// A sample of moves no code path should ever perform.
	illegal := []struct{ from, to string }{
		{StatusPending, StatusExecuting},
		{StatusPending, StatusDone},
		{StatusPending, StatusFailed},
		{StatusApproved, StatusRejected},
		{StatusApproved, StatusDone},
		{StatusExecuting, StatusApproved},
		{StatusExecuting, StatusPending},
		{StatusDone, StatusPending},
		{StatusRejected, StatusPending},
	}
	for _, tc := range illegal {
		if isLegalTransition(tc.from, tc.to) {
			t.Errorf("legalTransitions[%s][%s] = true, want false", tc.from, tc.to)
		}
	}
}

func TestFailApproved(t *testing.T) {
	db := openDB(t)
	pub := &stubPublisher{}
	s := New(Deps{Store: db, Publisher: pub, Logf: func(string, ...any) {}})
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{Kind: KindUnloadSlot, Risk: RiskHigh, CreatedBy: "op",
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a3"})})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	forceStatus(t, db, a.ID, StatusApproved)

	s.failApproved(ctx, a.ID, "simulated CAS-to-executing write failure")

	after, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if after.Status != StatusFailed {
		t.Errorf("status = %s, want failed", after.Status)
	}
	if after.Result == nil || after.Result.Error == "" {
		t.Errorf("result = %+v, want a populated error", after.Result)
	}
	if !pub.has(EventActionUpdate) {
		t.Error("failApproved did not publish smith:action_update")
	}
}

// ── nil-deps: every exported method must degrade, never panic ──────────────

func TestActionsNilDeps(t *testing.T) {
	s := New(Deps{})
	ctx := context.Background()

	if _, err := s.CreateAction(ctx, ActionDraft{Kind: KindRunbook, Risk: RiskInfo, CreatedBy: "op"}); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("CreateAction = %v, want ErrStoreUnwired", err)
	}
	if _, err := s.ListActions(ctx, "", nil, 0); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("ListActions = %v, want ErrStoreUnwired", err)
	}
	if _, err := s.GetAction(ctx, 1); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("GetAction = %v, want ErrStoreUnwired", err)
	}
	if _, err := s.ApproveAction(ctx, 1, "op"); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("ApproveAction = %v, want ErrStoreUnwired", err)
	}
	if _, err := s.RejectAction(ctx, 1, "op"); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("RejectAction = %v, want ErrStoreUnwired", err)
	}
	if _, err := s.ResolveHandoff(ctx, 1, "runbook", "op"); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("ResolveHandoff = %v, want ErrStoreUnwired", err)
	}
	if _, err := s.PendingActionCount(ctx); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("PendingActionCount = %v, want ErrStoreUnwired", err)
	}
	// executeAction, reconcileExecuting, proposeFrom, publishActionUpdate
	// are unexported but reachable from any Deps combination via Start()/
	// ApproveAction — assert they don't panic either.
	s.reconcileExecuting(ctx) // no store: must no-op silently
	s.proposeFrom(ctx, &CheckEnv{}, nil, nil, nil)
	s.executeAction(ctx, 1) // no store: fetch fails, logs, returns
}

// ── execute.go: dispatch per kind ───────────────────────────────────────────

// seedApproved creates a pending action via CreateAction (so detail/kind/
// risk validation runs for real) then force-transitions it to "approved" —
// the state right before executeAction's own CAS to "executing", letting
// dispatch tests call executeAction directly and synchronously instead of
// racing ApproveAction's background goroutine.
func seedApproved(t *testing.T, s *Smith, kind, risk string, detail json.RawMessage) int64 {
	t.Helper()
	a, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: kind, Title: "test", Risk: risk, CreatedBy: "op", Detail: detail,
	})
	if err != nil {
		t.Fatalf("CreateAction(%s): %v", kind, err)
	}
	forceStatus(t, s.d.Store, a.ID, StatusApproved)
	return a.ID
}

func TestExecuteDispatchPerKind(t *testing.T) {
	execAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(1 * time.Minute)

	t.Run("load_config success -> done", func(t *testing.T) {
		db := openDB(t)
		placer := &stubPlacer{}
		pub := &stubPublisher{}
		s := New(Deps{
			Store: db, Placer: placer, Publisher: pub,
			Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
		})
		id := seedApproved(t, s, KindLoadConfig, RiskLow, mustJSON(t, loadConfigDetail{Mode: "ornith-35b", Slot: "a3"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDone {
			t.Errorf("status = %s, want done (result=%+v)", a.Status, a.Result)
		}
		if len(placer.loads) != 1 || placer.loads[0] != (loadCall{Mode: "ornith-35b", Slot: "a3"}) {
			t.Errorf("loads = %+v, want one call for ornith-35b/a3", placer.loads)
		}
		if !pub.has("load_started") || !pub.has("load_complete") {
			t.Errorf("events = %v, want load_started + load_complete", pub.names())
		}
	})

	t.Run("load_config dispatch failure -> failed", func(t *testing.T) {
		db := openDB(t)
		bad := engine.Result{Success: false, Message: "boom"}
		placer := &stubPlacer{loadResult: &bad}
		s := New(Deps{Store: db, Placer: placer, Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindLoadConfig, RiskLow, mustJSON(t, loadConfigDetail{Mode: "m", Slot: "a3"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusFailed {
			t.Errorf("status = %s, want failed", a.Status)
		}
		if a.Result == nil || a.Result.Error == "" {
			t.Errorf("result = %+v, want a populated error", a.Result)
		}
	})

	t.Run("load_config with nil Placer -> failed with ErrPlacerUnwired", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{Store: db, Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindLoadConfig, RiskLow, mustJSON(t, loadConfigDetail{Mode: "m", Slot: "a3"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusFailed || a.Result == nil {
			t.Fatalf("action = %+v, want failed with a result", a)
		}
	})

	t.Run("unload_slot success -> done", func(t *testing.T) {
		db := openDB(t)
		placer := &stubPlacer{}
		s := New(Deps{Store: db, Placer: placer, Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindUnloadSlot, RiskHigh, mustJSON(t, unloadSlotDetail{Slot: "a3"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDone {
			t.Errorf("status = %s, want done", a.Status)
		}
		if len(placer.unloads) != 1 || placer.unloads[0] != "a3" {
			t.Errorf("unloads = %v, want [a3]", placer.unloads)
		}
	})

	t.Run("restart_forge_unit success (non-compressor) -> done", func(t *testing.T) {
		db := openDB(t)
		var restarted []string
		s := New(Deps{
			Store: db, Cfg: func() *config.Config { return &config.Config{} },
			RestartUnit: func(_ context.Context, unit string) error { restarted = append(restarted, unit); return nil },
			Source:      buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
		})
		id := seedApproved(t, s, KindRestartForgeUnit, RiskLow, mustJSON(t, restartUnitDetail{Unit: "forge-stt"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDone {
			t.Errorf("status = %s, want done (result=%+v)", a.Status, a.Result)
		}
		if len(restarted) != 1 || restarted[0] != "forge-stt" {
			t.Errorf("restarted = %v, want [forge-stt]", restarted)
		}
	})

	t.Run("restart_forge_unit compressor unit uses compressor_reachability verify", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{
			Store: db, Cfg: func() *config.Config { return &config.Config{} },
			RestartUnit: func(context.Context, string) error { return nil },
			Source:      buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
		})
		id := seedApproved(t, s, KindRestartForgeUnit, RiskLow, mustJSON(t, restartUnitDetail{Unit: "headroom@local"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDone {
			t.Errorf("status = %s, want done", a.Status)
		}
		if len(a.Result.Verify) != 1 || a.Result.Verify[0].CheckID != "compressor_reachability" {
			t.Errorf("verify = %+v, want exactly headroom_health", a.Result.Verify)
		}
	})

	t.Run("restart_forge_unit disallowed unit -> failed", func(t *testing.T) {
		db := openDB(t)
		called := false
		s := New(Deps{
			Store: db, Cfg: func() *config.Config { return &config.Config{} },
			RestartUnit: func(context.Context, string) error { called = true; return nil },
			Source:      buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
		})
		id := seedApproved(t, s, KindRestartForgeUnit, RiskLow, mustJSON(t, restartUnitDetail{Unit: "nginx"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusFailed {
			t.Errorf("status = %s, want failed", a.Status)
		}
		if called {
			t.Error("RestartUnit was called for a disallowed unit")
		}
	})

	t.Run("restart_forge_unit nil RestartUnit -> failed with ErrRestartUnwired", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{Store: db, Cfg: func() *config.Config { return &config.Config{} },
			Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindRestartForgeUnit, RiskLow, mustJSON(t, restartUnitDetail{Unit: "forge-stt"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusFailed {
			t.Errorf("status = %s, want failed", a.Status)
		}
	})

	t.Run("settings_change success -> done (brain_resolvable verify, P3)", func(t *testing.T) {
		// P3: settings_change now maps to a real verify check
		// (verifyChecksFor's "brain_resolvable" case) so a brain-swap can
		// reach done, not just done_unverified. This particular change
		// targets smith.thresholds, unrelated to the check's subject — but
		// verifyChecksFor sees kind, not detail.key, so it runs anyway. No
		// Catalog wired here ⇒ brain_resolvable skips (info, not
		// warn/crit) ⇒ still counts as "clean".
		db := openDB(t)
		s := New(Deps{Store: db, Settings: db.Settings(), Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindSettingsChange, RiskLow,
			mustJSON(t, settingsChangeDetail{Key: SettingThresholds, Value: mustJSON(t, map[string]any{"gtt_warn_pct": 80})}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDone {
			t.Errorf("status = %s, want done", a.Status)
		}
		raw, err := db.Settings().Get(context.Background(), SettingThresholds)
		if err != nil {
			t.Fatalf("Settings.Get: %v", err)
		}
		if string(raw) == "" {
			t.Error("setting was not actually written")
		}
	})

	t.Run("settings_change disallowed key -> failed", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{Store: db, Settings: db.Settings(), Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindSettingsChange, RiskLow,
			mustJSON(t, settingsChangeDetail{Key: "auth.policy", Value: mustJSON(t, "x")}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusFailed {
			t.Errorf("status = %s, want failed", a.Status)
		}
	})

	t.Run("settings_change nil Settings -> failed", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{Store: db, Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindSettingsChange, RiskLow,
			mustJSON(t, settingsChangeDetail{Key: SettingModel, Value: mustJSON(t, "x")}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusFailed {
			t.Errorf("status = %s, want failed", a.Status)
		}
	})
}

// TestExecuteStaleSnapshotNeverDone is the load-bearing correctness
// property of the whole action model: a dispatch success is NEVER promoted
// to "done" when the collector snapshot hasn't refreshed since execution —
// even when every mapped verify check comes back clean.
func TestExecuteStaleSnapshotNeverDone(t *testing.T) {
	execAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	stale := execAt.Add(-1 * time.Minute) // older than execAt: not re-polled

	db := openDB(t)
	placer := &stubPlacer{}
	s := New(Deps{
		Store: db, Placer: placer,
		Source: buildSnapshotAt(stale), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	id := seedApproved(t, s, KindUnloadSlot, RiskHigh, mustJSON(t, unloadSlotDetail{Slot: "a3"}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusDoneUnverified {
		t.Fatalf("status = %s, want done_unverified despite clean verify checks (stale snapshot), result=%+v", a.Status, a.Result)
	}
	if a.Result == nil || a.Result.Message == "" {
		t.Error("result.message should explain the stale-snapshot reason")
	}
}

// TestExecuteNoSourceNeverDone covers the same property when no Source is
// wired at all — freshness can never be proven, so it must never be "done".
func TestExecuteNoSourceNeverDone(t *testing.T) {
	execAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	db := openDB(t)
	placer := &stubPlacer{}
	s := New(Deps{Store: db, Placer: placer, Now: fixedNow(execAt), Logf: func(string, ...any) {}})
	id := seedApproved(t, s, KindUnloadSlot, RiskHigh, mustJSON(t, unloadSlotDetail{Slot: "a3"}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusDoneUnverified {
		t.Fatalf("status = %s, want done_unverified with no Source wired", a.Status)
	}
}

// ── self_review.go: promoteDoneUnverified ───────────────────────────────

// TestPromoteDoneUnverified covers item 24's fix (docs/v5-smith-experience.md
// §8): a done_unverified action whose verify checks now re-check clean
// against a snapshot genuinely newer than execution is promoted to done;
// anything short of that (still stale, still failing, or a runbook — whose
// done_unverified is a documented terminal state) stays put.
func TestPromoteDoneUnverified(t *testing.T) {
	execAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	stale := execAt.Add(-1 * time.Minute)

	t.Run("clean checks + fresh snapshot promotes to done", func(t *testing.T) {
		db := openDB(t)
		placer := &stubPlacer{}
		pub := &stubPublisher{}
		staleSnap := snapWith(collector.Metrics{})
		staleSnap.TakenAt = stale
		src := collector.NewStatic(staleSnap)
		s := New(Deps{Store: db, Placer: placer, Publisher: pub, Source: src, Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindUnloadSlot, RiskHigh, mustJSON(t, unloadSlotDetail{Slot: "a3"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDoneUnverified {
			t.Fatalf("setup: status = %s, want done_unverified before promotion", a.Status)
		}

		// The collector "catches up" with a fresh poll.
		freshSnap := snapWith(collector.Metrics{})
		freshSnap.TakenAt = execAt.Add(1 * time.Minute)
		src.Set(freshSnap)

		if !s.promoteDoneUnverified(context.Background(), a) {
			t.Fatal("promoteDoneUnverified = false, want true")
		}
		got, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if got.Status != StatusDone {
			t.Errorf("status = %s, want done", got.Status)
		}
		if got.Result == nil || got.Result.Message == "" {
			t.Error("result.message should explain the promotion")
		}
		if !pub.has(EventActionUpdate) {
			t.Error("expected EventActionUpdate to be published")
		}
	})

	t.Run("still-stale snapshot at promotion time stays done_unverified", func(t *testing.T) {
		db := openDB(t)
		placer := &stubPlacer{}
		staleSnap := snapWith(collector.Metrics{})
		staleSnap.TakenAt = stale
		src := collector.NewStatic(staleSnap)
		s := New(Deps{Store: db, Placer: placer, Source: src, Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindUnloadSlot, RiskHigh, mustJSON(t, unloadSlotDetail{Slot: "a3"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDoneUnverified {
			t.Fatalf("setup: status = %s, want done_unverified", a.Status)
		}

		if s.promoteDoneUnverified(context.Background(), a) {
			t.Fatal("promoteDoneUnverified = true, want false (snapshot never caught up)")
		}
		got, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if got.Status != StatusDoneUnverified {
			t.Errorf("status = %s, want still done_unverified", got.Status)
		}
	})

	t.Run("still-failing verify check stays done_unverified", func(t *testing.T) {
		db := openDB(t)
		placer := &stubPlacer{}
		critSnap := snapWith(collector.Metrics{GTTUsedBytes: int64p(96), GTTTotalBytes: int64p(100)})
		critSnap.TakenAt = stale
		src := collector.NewStatic(critSnap)
		s := New(Deps{Store: db, Placer: placer, Source: src, Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		id := seedApproved(t, s, KindLoadConfig, RiskLow, mustJSON(t, loadConfigDetail{Mode: "ornith-35b", Slot: "a3"}))
		s.executeAction(context.Background(), id)

		a, err := s.GetAction(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDoneUnverified {
			t.Fatalf("setup: status = %s, want done_unverified", a.Status)
		}

		// The collector catches up, but gtt_ceiling (one of load_config's
		// verify checks) is still crit at 96%.
		freshCrit := snapWith(collector.Metrics{GTTUsedBytes: int64p(96), GTTTotalBytes: int64p(100)})
		freshCrit.TakenAt = execAt.Add(1 * time.Minute)
		src.Set(freshCrit)

		if s.promoteDoneUnverified(context.Background(), a) {
			t.Fatal("promoteDoneUnverified = true, want false (gtt_ceiling still crit)")
		}
	})

	t.Run("runbook actions are never promoted", func(t *testing.T) {
		s := New(Deps{Logf: func(string, ...any) {}})
		a := &Action{ID: 1, Kind: KindRunbook}
		if s.promoteDoneUnverified(context.Background(), a) {
			t.Fatal("promoteDoneUnverified = true for a runbook, want false")
		}
	})

	t.Run("investigation-attached promotion re-triggers proposeResolution", func(t *testing.T) {
		db := openDB(t)
		placer := &stubPlacer{}
		staleSnap := snapWith(collector.Metrics{})
		staleSnap.TakenAt = stale
		src := collector.NewStatic(staleSnap)
		s := New(Deps{Store: db, Placer: placer, Source: src, Now: fixedNow(execAt), Logf: func(string, ...any) {}})
		ctx := context.Background()

		invID, err := s.CreateInvestigation(ctx, "manual", "test")
		if err != nil {
			t.Fatalf("CreateInvestigation: %v", err)
		}
		if _, err := s.persistFindings(ctx,
			[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"}},
			SweepManual, execAt, &invID); err != nil {
			t.Fatalf("persistFindings: %v", err)
		}

		created, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindUnloadSlot, Title: "test", Risk: RiskHigh, CreatedBy: "op",
			Detail: mustJSON(t, unloadSlotDetail{Slot: "a3"}), InvestigationID: &invID,
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		forceStatus(t, db, created.ID, StatusApproved)
		s.executeAction(ctx, created.ID)

		a, err := s.GetAction(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if a.Status != StatusDoneUnverified {
			t.Fatalf("setup: status = %s, want done_unverified", a.Status)
		}

		freshSnap := snapWith(collector.Metrics{})
		freshSnap.TakenAt = execAt.Add(1 * time.Minute)
		src.Set(freshSnap)

		if !s.promoteDoneUnverified(ctx, a) {
			t.Fatal("promoteDoneUnverified = false, want true")
		}
		inv, _, err := s.GetInvestigation(ctx, invID)
		if err != nil {
			t.Fatalf("GetInvestigation: %v", err)
		}
		if inv.Status != "resolved" {
			t.Errorf("investigation status = %s, want resolved (promotion should re-trigger proposeResolution)", inv.Status)
		}
	})
}

// ── reconcileExecuting ───────────────────────────────────────────────────

func TestReconcileExecuting(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	db := openDB(t)
	pub := &stubPublisher{}
	s := New(Deps{Store: db, Publisher: pub, Now: fixedNow(now), Logf: func(string, ...any) {}})
	ctx := context.Background()

	stale, err := s.CreateAction(ctx, ActionDraft{Kind: KindUnloadSlot, Risk: RiskHigh, CreatedBy: "smith",
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a3"})})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	staleExecAt := now.Add(-20 * time.Minute).Unix()
	if _, err := db.SQL().Exec(`UPDATE smith_actions SET status = ?, executed_at = ? WHERE id = ?`,
		StatusExecuting, staleExecAt, stale.ID); err != nil {
		t.Fatalf("seed stale executing row: %v", err)
	}

	recent, err := s.CreateAction(ctx, ActionDraft{Kind: KindUnloadSlot, Risk: RiskHigh, CreatedBy: "smith",
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a4"})})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	recentExecAt := now.Add(-1 * time.Minute).Unix()
	if _, err := db.SQL().Exec(`UPDATE smith_actions SET status = ?, executed_at = ? WHERE id = ?`,
		StatusExecuting, recentExecAt, recent.ID); err != nil {
		t.Fatalf("seed recent executing row: %v", err)
	}

	s.reconcileExecuting(ctx)

	staleAfter, err := s.GetAction(ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetAction(stale): %v", err)
	}
	if staleAfter.Status != StatusFailed {
		t.Errorf("stale row status = %s, want failed", staleAfter.Status)
	}
	if staleAfter.Result == nil || staleAfter.Result.Error == "" {
		t.Errorf("stale row result = %+v, want a populated error explaining the crash", staleAfter.Result)
	}
	if !pub.has(EventActionUpdate) {
		t.Error("reconcileExecuting did not publish smith:action_update for the reconciled row")
	}

	recentAfter, err := s.GetAction(ctx, recent.ID)
	if err != nil {
		t.Fatalf("GetAction(recent): %v", err)
	}
	if recentAfter.Status != StatusExecuting {
		t.Errorf("recent row status = %s, want still executing (below the staleness threshold)", recentAfter.Status)
	}
}

func TestReconcileExecutingNilStoreIsNoop(t *testing.T) {
	s := New(Deps{Logf: func(string, ...any) {}})
	s.reconcileExecuting(context.Background()) // must not panic
}

// ── restart_forge_unit allowlist ──────────────────────────────────────────

func TestRestartAllowlist(t *testing.T) {
	cfg := &config.Config{
		Slots:  map[string]config.Slot{"a1": {Unit: "forge-a1"}, "a2": {Unit: "forge-a2"}},
		Server: config.Server{TTSUnit: "forge-tts"},
		Modes: map[string]config.Mode{
			"comfyui": {Type: "service", Unit: "ai-mode-comfyui-custom"},
		},
	}
	cases := []struct {
		name string
		unit string
		cfg  *config.Config
		want bool
	}{
		{"forge-stt allowed", "forge-stt", cfg, true},
		{"forge-embedding allowed", "forge-embedding", cfg, true},
		{"forge-aligner allowed", "forge-aligner", cfg, true},
		{"tts unit allowed", "forge-tts", cfg, true},
		{"forge-comfyui allowed", "forge-comfyui", cfg, true},
		{"headroom@ template allowed", "headroom@local", cfg, true},
		{"headroom@ deepseek allowed", "headroom@deepseek", cfg, true},
		{"legacy compressor- allowed", "headroom-local", cfg, true},
		{"forge-compress@ local allowed", "forge-compress@local", cfg, true},
		{"forge-compress@ external allowed", "forge-compress@external", cfg, true},
		{"forge-compress with no @ denied", "forge-compress-local", cfg, false},
		{"service-mode unit allowed", "ai-mode-comfyui-custom", cfg, true},
		{"forge-daemon denied", "forge-daemon", cfg, false},
		{"slot unit denied", "forge-a1", cfg, false},
		{"other slot unit denied", "forge-a2", cfg, false},
		{"unknown unit denied", "nginx", cfg, false},
		{"dotted unit denied (injection guard)", "forge-stt.service", cfg, false},
		{"path unit denied (injection guard)", "forge-stt/../evil", cfg, false},
		{"shell injection denied", "forge-stt; rm -rf /", cfg, false},
		{"empty unit denied", "", cfg, false},
		{"nil cfg denied even for otherwise-injection-free name", "nginx", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := restartAllowed(tc.cfg, tc.unit)
			if got != tc.want {
				t.Errorf("restartAllowed(%q) = %v (%q), want %v", tc.unit, got, reason, tc.want)
			}
		})
	}
}

func TestRestartAllowlistServiceModeUnit(t *testing.T) {
	cfg := &config.Config{Modes: map[string]config.Mode{
		"comfyui": {Type: "service", Unit: "ai-mode-comfyui-custom"},
	}}
	if ok, reason := restartAllowed(cfg, "ai-mode-comfyui-custom"); !ok {
		t.Errorf("service-mode unit should be allowed, got false: %s", reason)
	}
}

// ── settings_change allowlist ───────────────────────────────────────────────

func TestSettingsKeyAllowlist(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{SettingModel, true},
		{SettingSchedule, true},
		{SettingThresholds, true},
		{SettingHandoffOfferings, true},
		{"auth.policy", false},
		{"infra.server", false},
		{"router_providers.deepseek.api_key", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := settingsKeyAllowed(tc.key); got != tc.want {
			t.Errorf("settingsKeyAllowed(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// ── approveSelfReviewClose (self_review.go's proposed closures) ────────────

func TestApproveSelfReviewClose(t *testing.T) {
	t.Run("still clean at approval resolves the investigation", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{Store: db, Logf: func(string, ...any) {}})
		ctx := context.Background()

		invID, err := s.CreateInvestigation(ctx, "manual", "test")
		if err != nil {
			t.Fatalf("CreateInvestigation: %v", err)
		}
		// slot_agreement was warn when the investigation opened, but with no
		// Sched wired it re-checks clean (skip/info) — the "still clean at
		// approval" case.
		if _, err := s.persistFindings(ctx,
			[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"}},
			SweepManual, s.d.Now(), &invID); err != nil {
			t.Fatalf("persistFindings: %v", err)
		}

		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindRunbook, Title: "self-review closure", Risk: RiskInfo, CreatedBy: "smith",
			InvestigationID: &invID,
			Detail: mustJSON(t, map[string]any{
				"self_review_close": true, "investigation_id": invID, "check_ids": []string{"slot_agreement"},
			}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}

		approved, err := s.ApproveAction(ctx, a.ID, "operator")
		if err != nil {
			t.Fatalf("ApproveAction: %v", err)
		}
		if approved.Status != StatusDoneUnverified {
			t.Fatalf("status = %s, want done_unverified (runbooks never execute)", approved.Status)
		}

		inv, _, err := s.GetInvestigation(ctx, invID)
		if err != nil {
			t.Fatalf("GetInvestigation: %v", err)
		}
		if inv.Status != "resolved" {
			t.Errorf("investigation status = %s, want resolved", inv.Status)
		}
		if inv.ResolvedByActionID == nil || *inv.ResolvedByActionID != a.ID {
			t.Errorf("resolved_by_action_id = %v, want %d", inv.ResolvedByActionID, a.ID)
		}
	})

	t.Run("regressed before approval leaves the investigation open with an honest summary", func(t *testing.T) {
		db := openDB(t)
		crit := snapWith(collector.Metrics{GTTUsedBytes: int64p(96), GTTTotalBytes: int64p(100)})
		src := collector.NewStatic(crit)
		s := New(Deps{Store: db, Source: src, Logf: func(string, ...any) {}})
		ctx := context.Background()

		invID, err := s.CreateInvestigation(ctx, "manual", "test")
		if err != nil {
			t.Fatalf("CreateInvestigation: %v", err)
		}
		if _, err := s.persistFindings(ctx,
			[]Finding{{CheckID: "gtt_ceiling", Severity: SeverityWarn, Summary: "high"}},
			SweepManual, s.d.Now(), &invID); err != nil {
			t.Fatalf("persistFindings: %v", err)
		}

		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindRunbook, Title: "self-review closure", Risk: RiskInfo, CreatedBy: "smith",
			InvestigationID: &invID,
			Detail: mustJSON(t, map[string]any{
				"self_review_close": true, "investigation_id": invID, "check_ids": []string{"gtt_ceiling"},
			}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}

		if _, err := s.ApproveAction(ctx, a.ID, "operator"); err != nil {
			t.Fatalf("ApproveAction: %v", err)
		}

		inv, _, err := s.GetInvestigation(ctx, invID)
		if err != nil {
			t.Fatalf("GetInvestigation: %v", err)
		}
		if inv.Status != "open" {
			t.Errorf("investigation status = %s, want still open — gtt_ceiling is still crit, approval must not force-close it", inv.Status)
		}
		if inv.ResolvedByActionID != nil {
			t.Errorf("resolved_by_action_id = %v, want nil (never resolved)", inv.ResolvedByActionID)
		}
	})

	t.Run("anomaly investigation with unrelated ambient warn still closes on approval", func(t *testing.T) {
		// Reproduces a real bug found live 2026-08-19: reviewInvestigations
		// (self_review.go) proposes closure using relevantWarnCritCheckIDs,
		// narrowed to anomalyRelevantChecks[code] — but approveSelfReviewClose
		// used to re-verify with the unnarrowed warnCritFindingCheckIDs, so a
		// proposal that was correctly offered (because an unrelated ambient
		// warn like comfyui_prune/brain_resolvable was excluded) could never
		// actually close, since that same ambient warn re-checked warn again
		// at approval time. Mirrors
		// TestSelfReviewOnce_AnomalyInvestigationIgnoresUnrelatedAmbientWarn's
		// setup on the approval path instead of the propose path.
		db := openDB(t)
		s := New(Deps{Store: db, Logf: func(string, ...any) {}})
		ctx := context.Background()

		invID, err := s.CreateInvestigation(ctx, "anomaly:GTT_DRAIN_TIMEOUT", "test")
		if err != nil {
			t.Fatalf("CreateInvestigation: %v", err)
		}
		// slot_agreement is in anomalyRelevantChecks["GTT_DRAIN_TIMEOUT"] and
		// re-checks clean (no Sched wired). brain_resolvable is NOT relevant
		// to a GTT drain timeout and re-checks warn deterministically (no
		// smith.model setting) — the ambient-warn case.
		if _, err := s.persistFindings(ctx,
			[]Finding{
				{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"},
				{CheckID: "brain_resolvable", Severity: SeverityWarn, Summary: "smith.model is unset"},
			},
			SweepAnomaly, s.d.Now(), &invID); err != nil {
			t.Fatalf("persistFindings: %v", err)
		}

		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindRunbook, Title: "self-review closure", Risk: RiskInfo, CreatedBy: "smith",
			InvestigationID: &invID,
			Detail: mustJSON(t, map[string]any{
				"self_review_close": true, "investigation_id": invID,
			}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}

		if _, err := s.ApproveAction(ctx, a.ID, "operator"); err != nil {
			t.Fatalf("ApproveAction: %v", err)
		}

		inv, _, err := s.GetInvestigation(ctx, invID)
		if err != nil {
			t.Fatalf("GetInvestigation: %v", err)
		}
		if inv.Status != "resolved" {
			t.Errorf("investigation status = %s, want resolved — an unrelated ambient warn must not block a self-review closure at approval time either", inv.Status)
		}
		if inv.ResolvedByActionID == nil || *inv.ResolvedByActionID != a.ID {
			t.Errorf("resolved_by_action_id = %v, want %d", inv.ResolvedByActionID, a.ID)
		}
	})
}
