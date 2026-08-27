// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/maintenance"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
)

// ── test-only fixture procedures ────────────────────────────────────────
//
// Registered once via procedures.Register (exported for exactly this
// purpose, registry.go's doc comment) so this package's tests can exercise
// checkpoint/rollback/maintenance/resume without inventing risk in the one
// real procedure (disk_usage_report) that runs live on ForgeHost.

var (
	testProcTwoStep = procedures.Procedure{
		ID:    "test_two_step",
		Title: "test two-step procedure",
		Steps: []procedures.Step{
			{Title: "step1", Argv: []string{"test-step", "1"}, VerifyCheckIDs: []string{"always_on_ports"}, OnFail: procedures.FailAbort},
			{Title: "step2", Argv: []string{"test-step", "2"}, VerifyCheckIDs: []string{"always_on_ports"}, OnFail: procedures.FailAbort},
		},
	}
	testProcCheckpoint = procedures.Procedure{
		ID:    "test_checkpoint",
		Title: "test checkpoint procedure",
		Steps: []procedures.Step{
			{Title: "before", Argv: []string{"test-step", "ckpt1"}, Checkpoint: true, OnFail: procedures.FailAbort},
			{Title: "after", Argv: []string{"test-step", "ckpt2"}, OnFail: procedures.FailAbort},
		},
	}
	testProcRollback = procedures.Procedure{
		ID:    "test_rollback",
		Title: "test rollback procedure",
		Steps: []procedures.Step{
			{Title: "risky", Argv: []string{"test-step", "fail1"}, OnFail: procedures.FailRollback, Rollback: []string{"test-step", "undo1"}},
		},
	}
	testProcFailCheckpoint = procedures.Procedure{
		ID:    "test_fail_checkpoint",
		Title: "test failure-pauses-for-review procedure",
		Steps: []procedures.Step{
			{Title: "risky", Argv: []string{"test-step", "fail2"}, OnFail: procedures.FailCheckpoint},
		},
	}
	testProcMaintenance = procedures.Procedure{
		ID:    "test_maintenance",
		Title: "test maintenance-holding procedure",
		Impact: procedures.Impact{
			NeedsMaintenance: true,
			EstDuration:      time.Minute,
			AffectedSlots:    []string{"a1"},
		},
		Steps: []procedures.Step{
			{Title: "step1", Argv: []string{"test-step", "maint1"}, NeedsMaintenance: true, VerifyCheckIDs: []string{"always_on_ports"}, OnFail: procedures.FailAbort},
		},
	}
	// testProcPrecondition exercises Sprint 6's Preconditions enforcement
	// (runProcedureSteps, before step 0). Its one step would otherwise run
	// fine — testPreconditionCheckID's severity is what the test controls,
	// via testPreconditionSeverity below.
	testProcPrecondition = procedures.Procedure{
		ID:            "test_precondition",
		Title:         "test precondition-gated procedure",
		Preconditions: []string{testPreconditionCheckID},
		Steps: []procedures.Step{
			{Title: "step1", Argv: []string{"test-step", "precond1"}, OnFail: procedures.FailAbort},
		},
	}
)

// testPreconditionCheckID/testPreconditionSeverity back a synthetic check
// registered into package smith's real check registry (below) purely so
// testProcPrecondition has something controllable to gate on — real
// preconditions in production name a real diagnostic check ID
// (build_refresh's is disk_space), but forcing a real check to a specific
// severity deterministically from a test would mean faking a whole
// collector snapshot for an unrelated subsystem. Package-level, not
// per-test, because these tests never run in parallel (no t.Parallel() in
// this file) — set at the top of a test, read once during that test's
// executeAction call.
const testPreconditionCheckID = "test_precondition_gate"

var testPreconditionSeverity = SeverityOK

func init() {
	procedures.Register(testProcTwoStep)
	procedures.Register(testProcCheckpoint)
	procedures.Register(testProcRollback)
	procedures.Register(testProcFailCheckpoint)
	procedures.Register(testProcMaintenance)
	procedures.Register(testProcPrecondition)
	registry = append(registry, Check{
		ID: testPreconditionCheckID, Name: "test precondition gate", Fast: false,
		Run: func(_ context.Context, _ *CheckEnv) Finding {
			return Finding{CheckID: testPreconditionCheckID, Severity: testPreconditionSeverity, Summary: "test precondition gate"}
		},
	})
}

// fakeRunStep is a Deps.RunStep fake: records every call and fails exactly
// the argvs named in failArgs (joined by " "), everything else succeeds.
type fakeRunStep struct {
	mu       sync.Mutex
	calls    [][]string
	failArgs map[string]bool
	onCall   func(spec procedures.StepSpec) // optional, invoked with mu held
}

func (f *fakeRunStep) run(_ context.Context, spec procedures.StepSpec) (procedures.StepResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{}, spec.Argv...))
	if f.onCall != nil {
		f.onCall(spec)
	}
	f.mu.Unlock()
	key := strings.Join(spec.Argv, " ")
	if f.failArgs[key] {
		return procedures.StepResult{ExitCode: 1}, fmt.Errorf("fake failure for %q", key)
	}
	return procedures.StepResult{Stdout: "ok", ExitCode: 0, Duration: time.Millisecond}, nil
}

func (f *fakeRunStep) callArgs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func procedureDetailJSON(t *testing.T, procID string) []byte {
	t.Helper()
	return mustJSON(t, procedureDetail{ProcedureID: procID})
}

func TestDispatchProcedure_TwoStepSuccess(t *testing.T) {
	execAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	fake := &fakeRunStep{}
	pub := &stubPublisher{}
	s := New(Deps{
		Store: db, RunStep: fake.run, Publisher: pub,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcTwoStep.ID))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusDone {
		t.Fatalf("status = %s, want done (result=%+v)", a.Status, a.Result)
	}
	calls := fake.callArgs()
	if len(calls) != 2 || calls[0][1] != "1" || calls[1][1] != "2" {
		t.Fatalf("calls = %v, want [test-step 1] then [test-step 2]", calls)
	}
	if !pub.has(EventProcedureStep) {
		t.Errorf("events = %v, want %s", pub.names(), EventProcedureStep)
	}
	run, err := s.GetProcedureRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProcedureRun: %v", err)
	}
	if run.Status != procRunStatusCompleted || run.CurrentStep != 2 || len(run.Steps) != 2 {
		t.Fatalf("run = %+v, want completed/2/2 steps", run)
	}
}

// TestDispatchProcedure_UnknownProcedureID_Fails simulates a procedure
// removed from the registry between proposal and execution (CreateAction
// itself refuses an unknown ID up front — TestCreateAction_
// ProcedureRejectsUnknownID — so this bypasses it via a direct insert,
// mirroring how dispatchRestartUnit/dispatchDeleteFiles re-validate their
// own allowlists at dispatch time, not just at proposal time).
func TestDispatchProcedure_UnknownProcedureID_Fails(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, RunStep: (&fakeRunStep{}).run, Source: buildSnapshotAt(time.Now()), Logf: func(string, ...any) {}})
	now := s.d.Now().Unix()
	res, err := db.SQL().Exec(
		`INSERT INTO smith_actions (kind, title, detail, risk, status, created_by, created_at)
		 VALUES (?, 't', ?, ?, ?, 'op', ?)`,
		KindProcedure, string(procedureDetailJSON(t, "does_not_exist")), RiskInfo, StatusApproved, now)
	if err != nil {
		t.Fatalf("seed action: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed action: %v", err)
	}
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", a.Status)
	}
	if a.Result == nil || !strings.Contains(a.Result.Error, "unknown procedure") {
		t.Fatalf("result = %+v, want an unknown-procedure error", a.Result)
	}
}

func TestCreateAction_ProcedureRejectsUnknownID(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	_, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindProcedure, Title: "t", Risk: RiskInfo, CreatedBy: "op",
		Detail: procedureDetailJSON(t, "does_not_exist"),
	})
	if !errors.Is(err, ErrProcedureNotFound) {
		t.Fatalf("err = %v, want ErrProcedureNotFound", err)
	}
}

func TestDispatchProcedure_RunStepUnwired_FailsClosed(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Source: buildSnapshotAt(time.Now()), Logf: func(string, ...any) {}})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcTwoStep.ID))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", a.Status)
	}
	if a.Result == nil || !strings.Contains(a.Result.Error, "not wired") {
		t.Fatalf("result = %+v, want an unwired-runner error", a.Result)
	}
}

func TestDispatchProcedure_CheckpointPausesThenApproveContinues(t *testing.T) {
	execAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	fake := &fakeRunStep{}
	pub := &stubPublisher{}
	s := New(Deps{
		Store: db, RunStep: fake.run, Publisher: pub,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	s.bgCtx = context.Background() // ApproveProcedureCheckpoint launches its resume goroutine on this
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcCheckpoint.ID))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusExecuting {
		t.Fatalf("status = %s, want executing (paused at checkpoint)", a.Status)
	}
	run, err := s.GetProcedureRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProcedureRun: %v", err)
	}
	if run.Status != procRunStatusAwaitingCheckpoint || run.CurrentStep != 1 {
		t.Fatalf("run = %+v, want awaiting_checkpoint at step 1", run)
	}
	if len(fake.callArgs()) != 1 {
		t.Fatalf("calls = %v, want exactly the first step to have run", fake.callArgs())
	}

	if _, err := s.ApproveProcedureCheckpoint(context.Background(), id, "operator"); err != nil {
		t.Fatalf("ApproveProcedureCheckpoint: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		a, _ := s.GetAction(context.Background(), id)
		return a != nil && a.Status != StatusExecuting
	})
	a, err = s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusDone && a.Status != StatusDoneUnverified {
		t.Fatalf("status = %s, want done or done_unverified after resuming past the checkpoint", a.Status)
	}
	calls := fake.callArgs()
	if len(calls) != 2 || calls[1][1] != "ckpt2" {
		t.Fatalf("calls = %v, want the second step to have run after approval", calls)
	}
}

func TestAbortProcedureRun_StopsAtCheckpoint(t *testing.T) {
	execAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	fake := &fakeRunStep{}
	s := New(Deps{
		Store: db, RunStep: fake.run,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcCheckpoint.ID))
	s.executeAction(context.Background(), id)

	a, err := s.AbortProcedureRun(context.Background(), id, "operator")
	if err != nil {
		t.Fatalf("AbortProcedureRun: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", a.Status)
	}
	run, err := s.GetProcedureRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProcedureRun: %v", err)
	}
	if run.Status != procRunStatusAborted {
		t.Fatalf("run status = %s, want aborted", run.Status)
	}
	if len(fake.callArgs()) != 1 {
		t.Fatalf("calls = %v, want the second step to NEVER have run after abort", fake.callArgs())
	}

	// A second abort must refuse — the run is no longer awaiting_checkpoint.
	if _, err := s.AbortProcedureRun(context.Background(), id, "operator"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second abort err = %v, want ErrInvalidTransition", err)
	}
}

func TestDispatchProcedure_RollbackRunsOnFailure(t *testing.T) {
	execAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	db := openDB(t)
	fake := &fakeRunStep{failArgs: map[string]bool{"test-step fail1": true}}
	s := New(Deps{
		Store: db, RunStep: fake.run,
		Source: buildSnapshotAt(execAt), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcRollback.ID))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", a.Status)
	}
	calls := fake.callArgs()
	if len(calls) != 2 || calls[1][1] != "undo1" {
		t.Fatalf("calls = %v, want the failed step followed by its rollback", calls)
	}
}

func TestDispatchProcedure_FailCheckpointPausesInsteadOfFailing(t *testing.T) {
	execAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	db := openDB(t)
	fake := &fakeRunStep{failArgs: map[string]bool{"test-step fail2": true}}
	s := New(Deps{
		Store: db, RunStep: fake.run,
		Source: buildSnapshotAt(execAt), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcFailCheckpoint.ID))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusExecuting {
		t.Fatalf("status = %s, want executing (a FailCheckpoint failure pauses for review, it doesn't fail outright)", a.Status)
	}
	run, err := s.GetProcedureRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProcedureRun: %v", err)
	}
	if run.Status != procRunStatusAwaitingCheckpoint || run.CheckpointNote == "" {
		t.Fatalf("run = %+v, want awaiting_checkpoint with a non-empty note", run)
	}
}

func TestDispatchProcedure_MaintenanceHeldDuringRunAndReleasedAfter(t *testing.T) {
	execAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	gate := maintenance.New(db.Settings(), bus.New(), fixedNow(execAt), func(string, ...any) {})
	var sawActiveDuringStep bool
	fake := &fakeRunStep{onCall: func(procedures.StepSpec) {
		if gate.Status().Active {
			sawActiveDuringStep = true
		}
	}}
	s := New(Deps{
		Store: db, RunStep: fake.run, Maintenance: gate,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcMaintenance.ID))

	if gate.Status().Active {
		t.Fatal("maintenance must not be active before the run starts")
	}
	s.executeAction(context.Background(), id)

	if !sawActiveDuringStep {
		t.Fatal("expected the maintenance window to be active while the procedure's step ran")
	}
	if gate.Status().Active {
		t.Fatal("expected the maintenance window to be released once the run completed")
	}
	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusDone {
		t.Fatalf("status = %s, want done (result=%+v)", a.Status, a.Result)
	}
}

func TestDispatchProcedure_MaintenanceUnwired_FailsClosed(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, RunStep: (&fakeRunStep{}).run, Source: buildSnapshotAt(time.Now()), Logf: func(string, ...any) {}})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcMaintenance.ID))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", a.Status)
	}
	if a.Result == nil || !strings.Contains(a.Result.Error, "maintenance") {
		t.Fatalf("result = %+v, want a maintenance-unwired error", a.Result)
	}
}

// TestRunProcedureSteps_CritPreconditionBlocksRun pins Sprint 6's G3 fix:
// before this, Preconditions was enforced only on the autonomy path
// (autonomy.go) — a human clicking "let Smith fix it" (Procedurize) or a
// dispatch off a plain approve bypassed it entirely. A crit precondition
// must fail the run before its one step ever executes.
func TestRunProcedureSteps_CritPreconditionBlocksRun(t *testing.T) {
	testPreconditionSeverity = SeverityCrit
	defer func() { testPreconditionSeverity = SeverityOK }()

	db := openDB(t)
	fake := &fakeRunStep{}
	s := New(Deps{Store: db, RunStep: fake.run, Source: buildSnapshotAt(time.Now()), Logf: func(string, ...any) {}})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcPrecondition.ID))
	s.executeAction(context.Background(), id)

	if len(fake.calls) != 0 {
		t.Fatalf("expected the step to never run when a precondition is crit, but RunStep was called %d time(s)", len(fake.calls))
	}
	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", a.Status)
	}
	if a.Result == nil || !strings.Contains(a.Result.Error, "precondition") {
		t.Fatalf("result = %+v, want a precondition-shaped error", a.Result)
	}

	// The action's own status stays "failed" (unchanged, correct — the
	// request as a whole did not succeed), but the underlying procedure run
	// must be distinguishable from a real mid-run failure: nothing was
	// attempted here, so a scorecard/run-history reader shouldn't have to
	// guess whether this needs investigation (2026-08-27 fix, idea from
	// reviewing amd/skills' rocm-doctor exit-code contract).
	run, err := s.GetProcedureRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProcedureRun: %v", err)
	}
	if run.Status != procRunStatusPreconditionFailed {
		t.Fatalf("run.Status = %q, want %q (not %q — a precondition gate is not a mid-run failure)",
			run.Status, procRunStatusPreconditionFailed, procRunStatusFailed)
	}
	sc, err := s.ProcedureScorecard(context.Background(), id)
	if err != nil {
		t.Fatalf("ProcedureScorecard: %v", err)
	}
	if !sc.PreconditionFailed {
		t.Fatalf("scorecard.PreconditionFailed = false, want true")
	}
	if sc.Completed {
		t.Fatalf("scorecard.Completed = true, want false")
	}
}

// TestRunProcedureSteps_OKPreconditionAllowsRun confirms the same procedure
// runs normally to completion when its precondition is clean — the gate
// only refuses, it never otherwise changes behavior.
func TestRunProcedureSteps_OKPreconditionAllowsRun(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	testPreconditionSeverity = SeverityOK
	defer func() { testPreconditionSeverity = SeverityOK }()

	db := openDB(t)
	fake := &fakeRunStep{}
	s := New(Deps{Store: db, RunStep: fake.run, Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {}})
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcPrecondition.ID))
	s.executeAction(context.Background(), id)

	if len(fake.calls) != 1 {
		t.Fatalf("expected the step to run once, RunStep was called %d time(s)", len(fake.calls))
	}
	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	// testProcPrecondition's step declares no VerifyCheckIDs, so the
	// action-level post-verify pass (finalizeResult) has nothing to check —
	// done_unverified, not failed, is the correct outcome here; this test
	// is only about the precondition gate letting the run start at all.
	if a.Status != StatusDoneUnverified {
		t.Fatalf("status = %s, want done_unverified (result=%+v)", a.Status, a.Result)
	}
}

// TestResumeProcedureRuns_ContinuesFromPersistedStep simulates a daemon
// restart mid-procedure: an action left "executing" with its run parked
// "running" at step 1 (step 0 already completed and persisted) — nobody
// re-ran executeAction's approved->executing CAS (the row is already past
// that), so only resumeProcedureRuns can ever pick this back up.
// Asserts the resumed run executes ONLY step 2, never re-running step 1.
func TestResumeProcedureRuns_ContinuesFromPersistedStep(t *testing.T) {
	execAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	fake := &fakeRunStep{}
	s := New(Deps{
		Store: db, RunStep: fake.run,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	s.bgCtx = context.Background()
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindProcedure, Title: "t", Risk: RiskInfo, CreatedBy: "op",
		Detail: procedureDetailJSON(t, testProcTwoStep.ID),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	forceStatus(t, db, a.ID, StatusExecuting)
	// Simulate step 0 already completed and persisted before the "crash".
	if _, err := db.SQL().Exec(
		`INSERT INTO smith_procedure_runs (action_id, procedure_id, status, current_step, steps_result, started_at, heartbeat_at)
		 VALUES (?, ?, 'running', 1, '[]', ?, ?)`,
		a.ID, testProcTwoStep.ID, execAt.Unix(), execAt.Unix()); err != nil {
		t.Fatalf("seed procedure run: %v", err)
	}

	s.resumeProcedureRuns(ctx)
	waitFor(t, time.Second, func() bool {
		got, _ := s.GetAction(ctx, a.ID)
		return got != nil && got.Status != StatusExecuting
	})

	got, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Status != StatusDone && got.Status != StatusDoneUnverified {
		t.Fatalf("status = %s, want done or done_unverified", got.Status)
	}
	calls := fake.callArgs()
	if len(calls) != 1 || calls[0][1] != "2" {
		t.Fatalf("calls = %v, want ONLY step 2 to run (step 1 was already done before the simulated crash)", calls)
	}
}

// TestResumeProcedureRuns_LeavesAwaitingCheckpointAlone proves a run parked
// at a checkpoint across a restart is never auto-resumed — it must stay
// exactly where a human left it.
func TestResumeProcedureRuns_LeavesAwaitingCheckpointAlone(t *testing.T) {
	db := openDB(t)
	fake := &fakeRunStep{}
	s := New(Deps{Store: db, RunStep: fake.run, Logf: func(string, ...any) {}})
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindProcedure, Title: "t", Risk: RiskInfo, CreatedBy: "op",
		Detail: procedureDetailJSON(t, testProcCheckpoint.ID),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	forceStatus(t, db, a.ID, StatusExecuting)
	if _, err := db.SQL().Exec(
		`INSERT INTO smith_procedure_runs (action_id, procedure_id, status, current_step, steps_result, checkpoint_note, started_at, heartbeat_at)
		 VALUES (?, ?, 'awaiting_checkpoint', 1, '[]', 'parked', ?, ?)`,
		a.ID, testProcCheckpoint.ID, time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatalf("seed procedure run: %v", err)
	}

	s.resumeProcedureRuns(ctx)
	time.Sleep(50 * time.Millisecond) // resumeProcedureRuns only launches goroutines for status=running; nothing should fire here
	if len(fake.callArgs()) != 0 {
		t.Fatalf("calls = %v, want zero — an awaiting_checkpoint run must never auto-resume", fake.callArgs())
	}
	got, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Status != StatusExecuting {
		t.Fatalf("status = %s, want unchanged executing", got.Status)
	}
}

// TestReconcileExecuting_SkipsProcedureKind proves the wall-clock stale
// reaper never touches a procedure-backed action — resumeProcedureRuns is
// its only liveness path (see execute.go's reconcileExecuting doc comment).
func TestReconcileExecuting_SkipsProcedureKind(t *testing.T) {
	db := openDB(t)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Now: fixedNow(old.Add(24 * time.Hour)), Logf: func(string, ...any) {}})
	ctx := context.Background()

	proc, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindProcedure, Title: "t", Risk: RiskInfo, CreatedBy: "op",
		Detail: procedureDetailJSON(t, testProcTwoStep.ID),
	})
	if err != nil {
		t.Fatalf("CreateAction(procedure): %v", err)
	}
	restart, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRestartForgeUnit, Title: "t", Risk: RiskLow, CreatedBy: "op",
		Detail: mustJSON(t, restartUnitDetail{Unit: "forge-stt"}),
	})
	if err != nil {
		t.Fatalf("CreateAction(restart): %v", err)
	}
	for _, id := range []int64{proc.ID, restart.ID} {
		if _, err := db.SQL().Exec(
			`UPDATE smith_actions SET status = ?, executed_at = ? WHERE id = ?`,
			StatusExecuting, old.Unix(), id); err != nil {
			t.Fatalf("seed executing action %d: %v", id, err)
		}
	}

	s.reconcileExecuting(ctx)

	gotProc, err := s.GetAction(ctx, proc.ID)
	if err != nil {
		t.Fatalf("GetAction(procedure): %v", err)
	}
	if gotProc.Status != StatusExecuting {
		t.Fatalf("procedure action status = %s, want unchanged executing (must not be wall-clock reaped)", gotProc.Status)
	}
	gotRestart, err := s.GetAction(ctx, restart.ID)
	if err != nil {
		t.Fatalf("GetAction(restart): %v", err)
	}
	if gotRestart.Status != StatusFailed {
		t.Fatalf("restart action status = %s, want failed (the ordinary stale-executing reap)", gotRestart.Status)
	}
}

func TestHasLiveProcedureRun(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Logf: func(string, ...any) {}})
	ctx := context.Background()

	if s.HasLiveProcedureRun(ctx, "") {
		t.Fatal("empty lease id must never report live")
	}
	if s.HasLiveProcedureRun(ctx, "mw-nope") {
		t.Fatal("unknown lease id must report false")
	}

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindProcedure, Title: "t", Risk: RiskInfo, CreatedBy: "op",
		Detail: procedureDetailJSON(t, testProcMaintenance.ID),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := db.SQL().Exec(
		`INSERT INTO smith_procedure_runs (action_id, procedure_id, status, current_step, lease_id, steps_result, started_at, heartbeat_at)
		 VALUES (?, ?, 'running', 0, 'mw-live', '[]', ?, ?)`,
		a.ID, testProcMaintenance.ID, time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatalf("seed procedure run: %v", err)
	}
	if !s.HasLiveProcedureRun(ctx, "mw-live") {
		t.Fatal("expected a running run holding this lease to report live")
	}

	if _, err := db.SQL().Exec(`UPDATE smith_procedure_runs SET status = 'completed' WHERE action_id = ?`, a.ID); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	if s.HasLiveProcedureRun(ctx, "mw-live") {
		t.Fatal("a completed run must no longer report live")
	}
}

// ── Sprint 4: supervision & evaluation harness ──────────────────────────

func TestListProcedureRuns_MostRecentFirstWithActionFields(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	fake := &fakeRunStep{}
	s := New(Deps{
		Store: db, RunStep: fake.run,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	ctx := context.Background()
	id1 := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcTwoStep.ID))
	s.executeAction(ctx, id1)
	// Second run starts a tick later so started_at strictly orders it after
	// the first — SQLite's second-resolution unix timestamps would tie
	// otherwise.
	s2 := New(Deps{
		Store: db, RunStep: fake.run,
		Source: buildSnapshotAt(fresh.Add(time.Minute)), Now: fixedNow(execAt.Add(time.Second)), Logf: func(string, ...any) {},
	})
	id2 := seedApproved(t, s2, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcTwoStep.ID))
	s2.executeAction(ctx, id2)

	runs, err := s.ListProcedureRuns(ctx, 0)
	if err != nil {
		t.Fatalf("ListProcedureRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].ActionID != id2 || runs[1].ActionID != id1 {
		t.Fatalf("runs = %+v, want id2 (most recent) first, then id1", runs)
	}
	if runs[0].ActionStatus == "" || runs[0].ActionTitle == "" {
		t.Fatalf("runs[0] = %+v, want joined action title/status populated", runs[0])
	}
	if runs[0].Status != procRunStatusCompleted {
		t.Fatalf("runs[0].Status = %s, want completed", runs[0].Status)
	}
}

func TestProcedureScorecard_UnattendedTwoStepCompletion(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	fake := &fakeRunStep{}
	s := New(Deps{
		Store: db, RunStep: fake.run,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	ctx := context.Background()
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcTwoStep.ID))
	s.executeAction(ctx, id)

	sc, err := s.ProcedureScorecard(ctx, id)
	if err != nil {
		t.Fatalf("ProcedureScorecard: %v", err)
	}
	if !sc.Completed || !sc.UnattendedCompletion {
		t.Fatalf("scorecard = %+v, want completed+unattended (no declared checkpoints)", sc)
	}
	if sc.CheckpointsDeclared != 0 || sc.CheckpointsReached != 0 {
		t.Fatalf("scorecard = %+v, want zero checkpoints (test_two_step declares none)", sc)
	}
	if sc.StepsTotal != 2 || sc.StepsCompleted != 2 {
		t.Fatalf("scorecard = %+v, want 2/2 steps", sc)
	}
	if !sc.PostVerifyPassed {
		t.Fatalf("scorecard = %+v, want post_verify_passed (action reached StatusDone)", sc)
	}
	if sc.NeedsMaintenance || sc.EstDurationSeconds != 0 {
		t.Fatalf("scorecard = %+v, want no maintenance/estimate for test_two_step", sc)
	}
}

func TestProcedureScorecard_CheckpointCountsAsAttended(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	fake := &fakeRunStep{}
	s := New(Deps{
		Store: db, RunStep: fake.run, Publisher: &stubPublisher{},
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	s.bgCtx = context.Background()
	ctx := context.Background()
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcCheckpoint.ID))
	s.executeAction(ctx, id)

	// Paused at the checkpoint — a run in this state has already asked a
	// human, regardless of whether they've answered yet.
	sc, err := s.ProcedureScorecard(ctx, id)
	if err != nil {
		t.Fatalf("ProcedureScorecard (paused): %v", err)
	}
	if sc.Completed || sc.CheckpointsDeclared != 1 || sc.CheckpointsReached != 1 {
		t.Fatalf("scorecard (paused) = %+v, want not-completed with 1 declared/reached checkpoint", sc)
	}
	if sc.UnattendedCompletion {
		t.Fatalf("scorecard (paused) = %+v, want unattended_completion false", sc)
	}

	if _, err := s.ApproveProcedureCheckpoint(ctx, id, "operator"); err != nil {
		t.Fatalf("ApproveProcedureCheckpoint: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		a, _ := s.GetAction(ctx, id)
		return a != nil && a.Status != StatusExecuting
	})

	sc, err = s.ProcedureScorecard(ctx, id)
	if err != nil {
		t.Fatalf("ProcedureScorecard (completed): %v", err)
	}
	if !sc.Completed || sc.CheckpointsDeclared != 1 || sc.CheckpointsReached != 1 {
		t.Fatalf("scorecard (completed) = %+v, want completed with 1 declared/reached checkpoint", sc)
	}
	if sc.UnattendedCompletion {
		t.Fatalf("scorecard (completed) = %+v, want unattended_completion false — a checkpoint fired", sc)
	}
}

func TestProcedureScorecard_MaintenanceEstimateVsActualDuration(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	gate := maintenance.New(db.Settings(), bus.New(), fixedNow(execAt), func(string, ...any) {})
	fake := &fakeRunStep{}
	s := New(Deps{
		Store: db, RunStep: fake.run, Maintenance: gate,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	ctx := context.Background()
	id := seedApproved(t, s, KindProcedure, RiskInfo, procedureDetailJSON(t, testProcMaintenance.ID))
	s.executeAction(ctx, id)

	sc, err := s.ProcedureScorecard(ctx, id)
	if err != nil {
		t.Fatalf("ProcedureScorecard: %v", err)
	}
	if !sc.NeedsMaintenance {
		t.Fatalf("scorecard = %+v, want needs_maintenance true", sc)
	}
	if sc.EstDurationSeconds != int64(testProcMaintenance.Impact.EstDuration.Seconds()) {
		t.Fatalf("scorecard.EstDurationSeconds = %d, want %d", sc.EstDurationSeconds, int64(testProcMaintenance.Impact.EstDuration.Seconds()))
	}
	if sc.ActualDurationSeconds < 0 {
		t.Fatalf("scorecard.ActualDurationSeconds = %d, want >= 0 for a finished run", sc.ActualDurationSeconds)
	}
}

func TestProcedureScorecard_NoRun_NotFound(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Logf: func(string, ...any) {}})
	ctx := context.Background()
	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindProcedure, Title: "t", Risk: RiskInfo, CreatedBy: "op",
		Detail: procedureDetailJSON(t, testProcTwoStep.ID),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := s.ProcedureScorecard(ctx, a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want wrapping sql.ErrNoRows (no run created yet, action still pending)", err)
	}
}
