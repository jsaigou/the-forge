// SPDX-License-Identifier: Apache-2.0

package smith

// procedure.go — the KindProcedure runner (autonomous-remediation Sprint 2,
// docs/v5-smith.md §13). A procedure is registered, coded Go data
// (internal/smith/procedures); this file is purely the execution engine:
// resolving a run's persisted state (smith_procedure_runs, migration 0056),
// stepping through Steps sequentially via Deps.RunStep, holding a
// maintenance window for the run's duration when the procedure declares it
// needs one, pausing at Step.Checkpoint gates for an operator decision, and
// resuming from the right step after a daemon restart instead of the start
// (Start()'s resumeProcedureRuns, called before reconcileExecuting's
// wall-clock reap — which explicitly excludes kind=procedure rows, since
// this file's boot-time resume fully replaces that reaper for them).
//
// Three entry points call into the shared step loop (runProcedureSteps):
// dispatchProcedure (the normal approved→executing path, from
// executeAction), continueProcedureRun (checkpoint-approval and boot-time
// resume — both are "keep going from CurrentStep"), and nothing else. Every
// exit from the loop is one of: nil (all steps done), ErrCheckpointPaused
// (leave the action "executing", parked), or a real error (the run and the
// action both finalize failed).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jsaigou/the-forge/internal/maintenance"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
	"github.com/jsaigou/the-forge/internal/store"
)

// EventProcedureStep is the SSE event published whenever a procedure run's
// state changes (step completed, paused at checkpoint, resumed, finished) —
// Contract 1 amendment, docs/v5-smith.md §5. Payload mirrors
// publishActionUpdate's shape plus run-specific fields.
const EventProcedureStep = "smith:procedure_step"

// procRunStatus values — smith_procedure_runs.status (migration 0056).
//
// procRunStatusFailed vs. procRunStatusPreconditionFailed (added 2026-08-27,
// idea borrowed from reviewing amd/skills' rocm-doctor CLI, which returns a
// distinct exit code for "not applicable on this host" vs. "attempted and
// failed" vs. "user declined" rather than collapsing all three into one
// non-zero status): a precondition check failing crit means the procedure
// never started — nothing was attempted, nothing broke, the host just isn't
// in the state this procedure expects. That is a fundamentally different
// signal from a step executing and erroring partway through, and conflating
// them under one "failed" status made every run-history/scorecard reader
// (ListProcedureRuns, ProcedureScorecard, Diagnostics' timeline) unable to
// tell "this never should have run" apart from "this broke while running" —
// the former needs no investigation, the latter always does.
// procRunStatusAborted (a human declined at a checkpoint) was already a
// correctly distinct third state; this only adds the missing fourth.
const (
	procRunStatusRunning            = "running"
	procRunStatusAwaitingCheckpoint = "awaiting_checkpoint"
	procRunStatusCompleted          = "completed"
	procRunStatusFailed             = "failed"
	procRunStatusPreconditionFailed = "precondition_failed"
	procRunStatusAborted            = "aborted"
	procedureMaintenanceSlack       = 5 * time.Minute // padding over Impact.EstDuration when requesting a window
	procedureDefaultStepTimeout     = 30 * time.Second
	procedureStdoutTailBytes        = 4000
)

// procedureDetail is KindProcedure's Action.Detail shape. Params (Sprint 3,
// docs/v5-smith.md §13) carries the named inputs the target procedure's Op
// steps read — reshaped from a SOURCE atomic action's own Detail by
// procedurize.go, never operator free text and never LLM-composed. Empty
// for a procedure with no declared Params (e.g. disk_usage_report).
type procedureDetail struct {
	ProcedureID string            `json:"procedure_id"`
	Params      map[string]string `json:"params,omitempty"`
}

// ProcedureStepOutcome is one step's persisted result (part of ProcedureRun,
// stored as the steps_result JSON array).
type ProcedureStepOutcome struct {
	Index      int            `json:"index"`
	Title      string         `json:"title"`
	Argv       []string       `json:"argv"`
	OK         bool           `json:"ok"`
	Error      string         `json:"error,omitempty"`
	ExitCode   int            `json:"exit_code"`
	DurationMS int64          `json:"duration_ms"`
	StdoutTail string         `json:"stdout_tail,omitempty"`
	StderrTail string         `json:"stderr_tail,omitempty"`
	Verify     []VerifyResult `json:"verify,omitempty"`
	At         int64          `json:"at"`
}

// ProcedureRun is the persisted execution state of one KindProcedure action
// (smith_procedure_runs, 1:1 with the action via a unique index).
type ProcedureRun struct {
	ID             int64                  `json:"id"`
	ActionID       int64                  `json:"action_id"`
	ProcedureID    string                 `json:"procedure_id"`
	Status         string                 `json:"status"`
	CurrentStep    int                    `json:"current_step"`
	LeaseID        string                 `json:"lease_id,omitempty"`
	Steps          []ProcedureStepOutcome `json:"steps"`
	CheckpointNote string                 `json:"checkpoint_note,omitempty"`
	StartedAt      int64                  `json:"started_at"`
	HeartbeatAt    int64                  `json:"heartbeat_at"`
	FinishedAt     *int64                 `json:"finished_at"`
}

// dispatchProcedure is KindProcedure's executeAction dispatch case. Returns
// the procedure ID regardless of outcome (mirroring dispatchRestartUnit's
// unit-regardless-of-outcome convention) so the caller can pick the right
// verify checks even on failure, and so a checkpoint pause still reports
// which procedure is waiting.
func (s *Smith) dispatchProcedure(ctx context.Context, a *Action) (string, error) {
	pd, err := parseDetail[procedureDetail](a.Detail)
	if err != nil {
		return "", err
	}
	proc, ok := procedures.Get(pd.ProcedureID)
	if !ok {
		return pd.ProcedureID, fmt.Errorf("smith: %w: %q", ErrProcedureNotFound, pd.ProcedureID)
	}
	// Defense-in-depth (see ErrProcedureNotAutonomyEligible's doc comment):
	// re-check the allowlist here, at the actual execution boundary, not
	// just in maybeAutoRunProcedure's decision to call Procedurize.
	if a.CreatedBy == autonomyActor && !autonomyEligible[proc.ID] {
		return proc.ID, fmt.Errorf("smith: %w: %q", ErrProcedureNotAutonomyEligible, proc.ID)
	}
	run, err := s.getOrCreateProcedureRun(ctx, a.ID, proc.ID)
	if err != nil {
		return proc.ID, err
	}
	return proc.ID, s.runProcedureSteps(ctx, run, proc, pd.Params)
}

// continueProcedureRun resumes an already-executing procedure action from
// its persisted CurrentStep — the shared tail for both checkpoint approval
// and boot-time resume. Unlike dispatchProcedure it is never launched from
// executeAction's approved→executing CAS (the action is already
// "executing"); callers are responsible for getting the run into a
// resumable state (status=running) before calling this.
func (s *Smith) continueProcedureRun(ctx context.Context, actionID int64) {
	a, err := s.GetAction(ctx, actionID)
	if err != nil {
		s.logf("continue procedure run: get action %d: %v", actionID, err)
		return
	}
	if a.Status != StatusExecuting {
		s.logf("continue procedure run: action %d is %q, not executing — skipping", actionID, a.Status)
		return
	}
	run, err := s.getProcedureRunByAction(ctx, actionID)
	if err != nil {
		s.logf("continue procedure run: get run for action %d: %v", actionID, err)
		return
	}
	proc, ok := procedures.Get(run.ProcedureID)
	if !ok {
		dispatchErr := fmt.Errorf("smith: %w: %q", ErrProcedureNotFound, run.ProcedureID)
		s.failProcedureRun(ctx, run, dispatchErr)
		s.finishExecution(ctx, actionID, KindProcedure, run.ProcedureID, execAtOrNow(a, s.d.Now()), dispatchErr)
		return
	}
	pd, err := parseDetail[procedureDetail](a.Detail)
	if err != nil {
		dispatchErr := fmt.Errorf("smith: continue procedure run: parse action %d detail: %w", actionID, err)
		s.failProcedureRun(ctx, run, dispatchErr)
		s.finishExecution(ctx, actionID, KindProcedure, run.ProcedureID, execAtOrNow(a, s.d.Now()), dispatchErr)
		return
	}
	dispatchErr := s.runProcedureSteps(ctx, run, proc, pd.Params)
	if errors.Is(dispatchErr, ErrCheckpointPaused) {
		s.publishActionUpdate(ctx, a, StatusExecuting)
		return
	}
	s.finishExecution(ctx, actionID, KindProcedure, proc.ID, execAtOrNow(a, s.d.Now()), dispatchErr)
}

// execAtOrNow returns a's ExecutedAt (the moment dispatch first began) or
// now when unset — finalizeResult's snapFresh check needs a stable
// reference point that survives a resume, not the moment this particular
// resume happened to run.
func execAtOrNow(a *Action, now time.Time) time.Time {
	if a.ExecutedAt != nil {
		return time.Unix(*a.ExecutedAt, 0)
	}
	return now
}

// finishExecution is executeAction's post-dispatch tail, extracted so
// continueProcedureRun/ApproveProcedureCheckpoint/AbortProcedureRun (which
// never go through executeAction's approved→executing CAS) can finalize a
// procedure-backed action exactly the same way a normal dispatch does:
// post-verify, status decision, persistence, SSE, audit, swap-back.
func (s *Smith) finishExecution(ctx context.Context, id int64, kind, unit string, execAt time.Time, dispatchErr error) {
	finalStatus, result := s.finalizeResult(ctx, kind, unit, execAt, dispatchErr)
	resultJSON, err := json.Marshal(result)
	if err != nil {
		resultJSON = []byte(`{"ok":false,"error":"smith: failed to marshal execution result"}`)
	}
	now := s.d.Now().Unix()
	upd, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, result = ?, verified_at = ?, resolved_at = ? WHERE id = ? AND status = ?`,
		finalStatus, string(resultJSON), now, now, id, StatusExecuting)
	if err != nil {
		s.logf("finish execution %d: CAS to %s: %v", id, finalStatus, err)
		return
	}
	if n, _ := upd.RowsAffected(); n == 0 {
		s.logf("finish execution %d: could not finalize to %s (row left executing)", id, finalStatus)
		return
	}
	final, err := s.GetAction(ctx, id)
	if err != nil {
		s.logf("finish execution %d: refetch after finalize: %v", id, err)
		return
	}
	s.publishActionUpdate(ctx, final, finalStatus)
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: "smith", Action: "smith_action_execute",
			Target: fmt.Sprintf("%d:%s", final.ID, final.Kind),
			Detail: fmt.Sprintf("status=%s message=%s", finalStatus, result.Message),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	if final.SelfEvicting && final.Handoff != nil && final.Handoff.State == HandoffRemoteSwapped {
		s.proposeSwapBack(ctx, final)
	}
}

// runProcedureSteps is the shared step loop. It mutates run in place and
// persists after every step (and on every status transition), so a crash
// mid-loop leaves the row at the last completed step, not the start. params
// carries the procedure's declared Params (Sprint 3) — validated once here,
// before any step runs, so a bad/missing param fails the whole run up front
// rather than partway through.
func (s *Smith) runProcedureSteps(ctx context.Context, run *ProcedureRun, proc procedures.Procedure, params map[string]string) error {
	if err := procedures.ValidateParams(proc, params); err != nil {
		return s.failProcedureRun(ctx, run, fmt.Errorf("smith: %w", err))
	}
	// Preconditions (Sprint 6): enforced here, once, before step 0 — the
	// one check point for both the operator-approved dispatch path and the
	// autonomy path (autonomy.go's own pre-Procedurize check stays as an
	// earlier, non-authoritative decline-without-a-failed-run-record; this
	// is the backstop that was previously entirely absent from the
	// operator path). A resumed run (CurrentStep > 0) skips this — the run
	// already started, so re-litigating whether it should have is moot.
	if run.CurrentStep == 0 {
		for _, pcID := range proc.Preconditions {
			pf := s.runChecksBare(ctx, []string{pcID})
			if len(pf) == 1 && pf[0].Severity == SeverityCrit {
				return s.failProcedureRunPrecondition(ctx, run, fmt.Errorf("smith: procedure %s: precondition %s is crit: %s", proc.ID, pcID, pf[0].Summary))
			}
		}
	}

	for run.CurrentStep < len(proc.Steps) {
		step := proc.Steps[run.CurrentStep]

		// Maintenance window: opened lazily on the first step that declares
		// NeedsMaintenance (not necessarily step 0), held for the rest of
		// the run regardless of later steps' own value, released once by
		// the run's normal completion/failure/abort paths — never entered
		// twice (run.LeaseID == "" guards this the same way it always has).
		if step.NeedsMaintenance && run.LeaseID == "" {
			if s.d.Maintenance == nil {
				return s.failProcedureRun(ctx, run, ErrMaintenanceUnwired)
			}
			st, err := s.d.Maintenance.Enter(maintenance.EnterRequest{
				Reason:           "smith procedure: " + proc.Title,
				EnteredBy:        "smith",
				AffectedSlots:    proc.Impact.AffectedSlots,
				AffectedServices: proc.Impact.AffectedServices,
				Duration:         proc.Impact.EstDuration + procedureMaintenanceSlack,
			})
			if err != nil {
				return s.failProcedureRun(ctx, run, fmt.Errorf("smith: enter maintenance for procedure %s: %w", proc.ID, err))
			}
			run.LeaseID = st.LeaseID
			if err := s.persistProcedureRun(ctx, run); err != nil {
				s.logf("procedure run %d: persist lease: %v", run.ID, err)
			}
		}
		stepCtx := ctx
		if run.LeaseID != "" {
			stepCtx = maintenance.WithLease(ctx, run.LeaseID)
		}
		s.heartbeatProcedureRun(stepCtx, run)
		start := s.d.Now()

		var result procedures.StepResult
		var execErr error
		var argv []string
		if step.Op != "" {
			result, execErr = s.runNativeOp(stepCtx, step.Op, params)
		} else {
			if !procedures.ArgvAllowed(proc.ID, step.Argv) {
				return s.failProcedureRun(ctx, run, fmt.Errorf("smith: procedure %s step %d argv is not on its own allowlist", proc.ID, run.CurrentStep))
			}
			if s.d.RunStep == nil {
				return s.failProcedureRun(ctx, run, ErrProcedureUnwired)
			}
			spec := procedures.StepSpec{Argv: step.Argv, Cwd: step.Cwd, Timeout: step.Timeout, Env: step.Env, EnvPassthrough: step.EnvPassthrough}
			if spec.Timeout <= 0 {
				spec.Timeout = procedureDefaultStepTimeout
			}
			result, execErr = s.d.RunStep(stepCtx, spec)
			argv = step.Argv
		}
		outcome := ProcedureStepOutcome{
			Index:      run.CurrentStep,
			Title:      step.Title,
			Argv:       argv,
			OK:         execErr == nil,
			ExitCode:   result.ExitCode,
			DurationMS: durationMS(result.Duration, s.d.Now().Sub(start)),
			StdoutTail: truncateAndRedact(result.Stdout),
			StderrTail: truncateAndRedact(result.Stderr),
			At:         s.d.Now().Unix(),
		}
		if execErr != nil {
			outcome.Error = scrubSecretPatterns(execErr.Error())
		}

		verifyFailed := false
		if execErr == nil && len(step.VerifyCheckIDs) > 0 {
			findings := s.runChecksBare(stepCtx, step.VerifyCheckIDs)
			outcome.Verify = toVerifyResults(findings, s.d.Now())
			for _, f := range findings {
				if f.Severity == SeverityWarn || f.Severity == SeverityCrit {
					verifyFailed = true
				}
			}
		}
		run.Steps = append(run.Steps, outcome)

		if execErr != nil || verifyFailed {
			failErr := execErr
			if failErr == nil {
				failErr = fmt.Errorf("post-verify check(s) reported warn or crit: %s", step.VerifyCheckIDs)
			}
			stepErr := fmt.Errorf("smith: procedure %s step %d (%s) failed: %w", proc.ID, run.CurrentStep, step.Title, failErr)
			switch step.OnFail {
			case procedures.FailRollback:
				s.rollbackProcedureStep(stepCtx, run, step)
				return s.failProcedureRun(ctx, run, stepErr)
			case procedures.FailCheckpoint:
				run.CheckpointNote = fmt.Sprintf("step %d (%s) reported a failure — review before continuing or aborting: %v", run.CurrentStep, step.Title, failErr)
				return s.pauseProcedureRun(ctx, run)
			default: // procedures.FailAbort, or unset
				return s.failProcedureRun(ctx, run, stepErr)
			}
		}

		run.CurrentStep++
		if err := s.persistProcedureRun(ctx, run); err != nil {
			s.logf("procedure run %d: persist step %d: %v", run.ID, run.CurrentStep-1, err)
		}
		s.publishProcedureStep(run, "step_complete")

		if step.Checkpoint {
			// An op-supplied note wins over the generic message — the op
			// knows what the NEXT step will actually change, and the
			// checkpoint exists precisely so a human decides with that
			// evidence (StepResult.CheckpointNote's doc comment).
			if result.CheckpointNote != "" {
				run.CheckpointNote = result.CheckpointNote
			} else {
				run.CheckpointNote = fmt.Sprintf("step %d (%s) complete — approve to continue", run.CurrentStep-1, step.Title)
			}
			return s.pauseProcedureRun(ctx, run)
		}
	}

	if run.LeaseID != "" && s.d.Maintenance != nil {
		if _, err := s.d.Maintenance.Exit(run.LeaseID, false); err != nil {
			s.logf("procedure run %d: exit maintenance: %v", run.ID, err)
		}
	}
	run.Status = procRunStatusCompleted
	now := s.d.Now().Unix()
	run.FinishedAt = &now
	if err := s.persistProcedureRun(ctx, run); err != nil {
		s.logf("procedure run %d: persist completion: %v", run.ID, err)
	}
	s.publishProcedureStep(run, "completed")
	return nil
}

// runNativeOp executes a Step.Op step — Sprint 3's alternative to Argv for
// procedures that wrap smith's existing native Go dispatchers instead of an
// external command. The switch reuses the EXACT SAME validators
// (restartAllowed/deleteAllowed) and the exact same Deps
// (RestartUnit/Placer.Unload/DeleteFile) the atomic dispatchers in execute.go
// use — a deliberate design choice to avoid inventing a second, parallel
// privilege path for the same three real operations. The set of valid op
// values is fixed here; procedures.Step.Op is untyped package-procedures
// data that means nothing until this switch interprets it.
func (s *Smith) runNativeOp(ctx context.Context, op string, params map[string]string) (procedures.StepResult, error) {
	start := s.d.Now()
	switch op {
	case "restart_unit":
		unit := params["unit"]
		if ok, reason := restartAllowed(s.cfg(), unit); !ok {
			return procedures.StepResult{}, fmt.Errorf("smith: unit %q not allowed: %s: %w", unit, reason, ErrUnitNotAllowed)
		}
		if s.d.RestartUnit == nil {
			return procedures.StepResult{}, ErrRestartUnwired
		}
		if err := s.d.RestartUnit(ctx, unit); err != nil {
			return procedures.StepResult{}, fmt.Errorf("smith: restart %s: %w", unit, err)
		}
		return procedures.StepResult{Stdout: "restarted " + unit, Duration: s.d.Now().Sub(start)}, nil

	case "install_unit_launcher":
		unit := params["unit"]
		if unit == "" {
			return procedures.StepResult{}, errors.New("smith: install_unit_launcher op requires a unit param")
		}
		snap := s.snapshot()
		if snap == nil {
			return procedures.StepResult{}, errors.New("smith: no collector snapshot available")
		}
		ut, ok := snap.Units[unit]
		if !ok {
			return procedures.StepResult{}, fmt.Errorf("smith: unit %q not found in the current snapshot", unit)
		}
		content, ok, reason := launcherInstallAllowed(ut.ExecStartPath)
		if !ok {
			return procedures.StepResult{}, fmt.Errorf("smith: launcher path %q not allowed: %s: %w", ut.ExecStartPath, reason, ErrLauncherNotAllowed)
		}
		if s.d.InstallLauncherFile == nil {
			return procedures.StepResult{}, ErrInstallLauncherUnwired
		}
		if err := s.d.InstallLauncherFile(ctx, ut.ExecStartPath, content); err != nil {
			return procedures.StepResult{}, fmt.Errorf("smith: install launcher %s: %w", ut.ExecStartPath, err)
		}
		return procedures.StepResult{
			Stdout:   fmt.Sprintf("installed %s (%d bytes) for unit %s", ut.ExecStartPath, len(content), unit),
			Duration: s.d.Now().Sub(start),
		}, nil

	case "unload_slot":
		slot := params["slot"]
		if slot == "" {
			return procedures.StepResult{}, errors.New("smith: unload_slot op requires a slot param")
		}
		if s.d.Placer == nil {
			return procedures.StepResult{}, ErrPlacerUnwired
		}
		result := s.d.Placer.Unload(ctx, slot)
		if !result.Success {
			return procedures.StepResult{}, fmt.Errorf("smith: unload slot %s: %s", slot, result.Message)
		}
		return procedures.StepResult{Stdout: result.Message, Duration: s.d.Now().Sub(start)}, nil

	case "delete_comfyui_files":
		var files []deleteFileEntry
		if err := json.Unmarshal([]byte(params["files_json"]), &files); err != nil {
			return procedures.StepResult{}, fmt.Errorf("smith: parse files_json param: %w", err)
		}
		if len(files) == 0 {
			return procedures.StepResult{}, errors.New("smith: delete_comfyui_files op requires at least one file")
		}
		roots := s.ComfyUIModelRoots(ctx)
		for _, f := range files {
			if ok, reason := deleteAllowed(roots, f.Path); !ok {
				return procedures.StepResult{}, fmt.Errorf("smith: path %q not allowed: %s: %w", f.Path, reason, ErrPathNotAllowed)
			}
		}
		if s.d.DeleteFile == nil {
			return procedures.StepResult{}, ErrDeleteUnwired
		}
		var reclaimed int64
		var deleted, failed []string
		var firstErr error
		for _, f := range files {
			if err := s.d.DeleteFile(ctx, f.Path); err != nil {
				failed = append(failed, f.Path)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			deleted = append(deleted, f.Path)
			reclaimed += f.SizeBytes
		}
		note := fmt.Sprintf("deleted %d of %d file(s), reclaimed %.1f GB", len(deleted), len(files), float64(reclaimed)/(1<<30))
		if len(failed) > 0 {
			return procedures.StepResult{Stdout: note}, fmt.Errorf("smith: %d of %d file(s) failed to delete, first error: %w", len(failed), len(files), firstErr)
		}
		return procedures.StepResult{Stdout: note, Duration: s.d.Now().Sub(start)}, nil

	// build_refresh's ops (autonomous-remediation Sprint 6,
	// build_refresh_ops.go): each resolves its own timing internally (real
	// wall-clock exec durations, some spanning multiple sub-commands) and
	// returns a StepResult with Duration already populated, so none of
	// them need this switch's own `start` var the way the simpler
	// single-call ops above do.
	case "build_record_installed":
		return s.opBuildRecordInstalled(ctx, params)
	case "build_git_fetch":
		return s.opBuildGitFetch(ctx, params)
	case "build_git_precheck":
		return s.opBuildGitPrecheck(ctx, params)
	case "build_git_rebase":
		return s.opBuildGitRebase(ctx, params)
	case "build_cmake_configure":
		return s.opBuildCmakeConfigure(ctx, params)
	case "build_cmake_build":
		return s.opBuildCmakeBuild(ctx, params)
	case "build_verify_binary":
		return s.opBuildVerifyBinary(ctx, params)
	case "build_relabel":
		return s.opBuildRelabel(ctx, params)
	case "build_reliability_test":
		return s.opBuildReliabilityTest(ctx, params)
	case "build_perf_measure":
		return s.opBuildPerfMeasure(ctx, params)
	case "build_catalog_promote":
		return s.opBuildCatalogPromote(ctx, params)
	case "build_cleanup_old":
		return s.opBuildCleanupOld(ctx, params)
	case "build_record_upstream_sha":
		return s.opBuildRecordUpstreamSha(ctx, params)

	default:
		return procedures.StepResult{}, fmt.Errorf("smith: unknown native op %q", op)
	}
}

// rollbackProcedureStep best-effort-runs step's own Rollback argv (only
// meaningful when OnFail is FailRollback). Its own failure is logged, never
// escalated — it's cleanup on a best-effort basis, not a second chance for
// the run to succeed.
func (s *Smith) rollbackProcedureStep(ctx context.Context, run *ProcedureRun, step procedures.Step) {
	if len(step.Rollback) == 0 || s.d.RunStep == nil {
		return
	}
	proc, ok := procedures.Get(run.ProcedureID)
	if !ok || !procedures.ArgvAllowed(proc.ID, step.Rollback) {
		s.logf("procedure run %d: rollback argv not on the %s allowlist, skipping", run.ID, run.ProcedureID)
		return
	}
	spec := procedures.StepSpec{Argv: step.Rollback, Cwd: step.Cwd, Timeout: step.Timeout, Env: step.Env, EnvPassthrough: step.EnvPassthrough}
	if spec.Timeout <= 0 {
		spec.Timeout = procedureDefaultStepTimeout
	}
	if _, err := s.d.RunStep(ctx, spec); err != nil {
		s.logf("procedure run %d: rollback for step %q failed: %v", run.ID, step.Title, err)
	}
}

// failProcedureRun marks run failed, exits any held maintenance window, and
// returns err unchanged so call sites can `return s.failProcedureRun(...)`.
func (s *Smith) failProcedureRun(ctx context.Context, run *ProcedureRun, err error) error {
	if run.LeaseID != "" && s.d.Maintenance != nil {
		if _, exitErr := s.d.Maintenance.Exit(run.LeaseID, false); exitErr != nil {
			s.logf("procedure run %d: exit maintenance on failure: %v", run.ID, exitErr)
		}
	}
	run.Status = procRunStatusFailed
	now := s.d.Now().Unix()
	run.FinishedAt = &now
	if persistErr := s.persistProcedureRun(ctx, run); persistErr != nil {
		s.logf("procedure run %d: persist failure: %v", run.ID, persistErr)
	}
	s.publishProcedureStep(run, "failed")
	return err
}

// failProcedureRunPrecondition marks run precondition_failed — the run never
// started, so there is never a held maintenance window to exit (preconditions
// are checked before step 0's own maintenance.Enter, run.LeaseID == "" here
// unconditionally). Otherwise identical to failProcedureRun; kept as a
// separate function rather than a shared helper with a status parameter so
// the maintenance-exit skip is visible at the call site, not hidden behind a
// branch a future reader could miss.
func (s *Smith) failProcedureRunPrecondition(ctx context.Context, run *ProcedureRun, err error) error {
	run.Status = procRunStatusPreconditionFailed
	now := s.d.Now().Unix()
	run.FinishedAt = &now
	if persistErr := s.persistProcedureRun(ctx, run); persistErr != nil {
		s.logf("procedure run %d: persist precondition failure: %v", run.ID, persistErr)
	}
	s.publishProcedureStep(run, "precondition_failed")
	return err
}

// pauseProcedureRun marks run awaiting_checkpoint and returns
// ErrCheckpointPaused — the maintenance window (if any) stays held, since
// the run isn't done. Always returns ErrCheckpointPaused so call sites can
// `return s.pauseProcedureRun(...)`.
func (s *Smith) pauseProcedureRun(ctx context.Context, run *ProcedureRun) error {
	run.Status = procRunStatusAwaitingCheckpoint
	if err := s.persistProcedureRun(ctx, run); err != nil {
		s.logf("procedure run %d: persist checkpoint pause: %v", run.ID, err)
	}
	s.publishProcedureStep(run, "checkpoint")
	return ErrCheckpointPaused
}

// publishProcedureStep emits EventProcedureStep, nil-tolerant.
func (s *Smith) publishProcedureStep(run *ProcedureRun, event string) {
	if s.d.Publisher == nil {
		return
	}
	s.d.Publisher.Publish(EventProcedureStep, map[string]any{
		"action_id":    run.ActionID,
		"procedure_id": run.ProcedureID,
		"status":       run.Status,
		"current_step": run.CurrentStep,
		"event":        event,
	})
}

// ApproveProcedureCheckpoint clears an awaiting_checkpoint run's gate and
// resumes execution in the background (docs/v5-smith.md §13 — the "let
// smith fix it" checkpoint UI's approve action). The action's own status is
// untouched by this call (it has been "executing" since the run started and
// stays that way until the whole procedure finishes) — only the run's
// status moves, via a CAS so a concurrent double-approve can't launch two
// resume goroutines for the same run.
func (s *Smith) ApproveProcedureCheckpoint(ctx context.Context, actionID int64, actor string) (*Action, error) {
	a, err := s.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if a.Kind != KindProcedure || a.Status != StatusExecuting {
		return nil, ErrInvalidTransition
	}
	run, err := s.getProcedureRunByAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	ok, err := s.casProcedureRunStatus(ctx, run.ID, procRunStatusAwaitingCheckpoint, procRunStatusRunning)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidTransition
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_procedure_checkpoint_approve",
			Target: fmt.Sprintf("%d:%s", a.ID, a.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	execCtx := s.bgCtx
	if execCtx == nil {
		execCtx = context.Background()
	}
	go s.continueProcedureRun(execCtx, actionID)
	return a, nil
}

// AbortProcedureRun ends an awaiting_checkpoint run without continuing it —
// the operator's "abort" option at a checkpoint gate. Exits any held
// maintenance window and finalizes the action failed via the same
// finishExecution path a real dispatch error would take, so the audit trail
// and SSE story look identical either way.
func (s *Smith) AbortProcedureRun(ctx context.Context, actionID int64, actor string) (*Action, error) {
	a, err := s.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if a.Kind != KindProcedure || a.Status != StatusExecuting {
		return nil, ErrInvalidTransition
	}
	run, err := s.getProcedureRunByAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	ok, err := s.casProcedureRunStatus(ctx, run.ID, procRunStatusAwaitingCheckpoint, procRunStatusAborted)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidTransition
	}
	if run.LeaseID != "" && s.d.Maintenance != nil {
		if _, err := s.d.Maintenance.Exit(run.LeaseID, false); err != nil {
			s.logf("abort procedure run %d: exit maintenance: %v", run.ID, err)
		}
	}
	now := s.d.Now().Unix()
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_procedure_runs SET finished_at = ? WHERE id = ?`, now, run.ID); err != nil {
		s.logf("abort procedure run %d: persist finished_at: %v", run.ID, err)
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_procedure_checkpoint_abort",
			Target: fmt.Sprintf("%d:%s", a.ID, a.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	s.finishExecution(ctx, actionID, KindProcedure, run.ProcedureID, execAtOrNow(a, s.d.Now()),
		fmt.Errorf("smith: procedure run aborted by %s at checkpoint", actor))
	return s.GetAction(ctx, actionID)
}

// resumeProcedureRuns is called once from Start(), before reconcileExecuting
// — every action still "executing" whose run is still "running" was mid-step
// when the previous process instance stopped (a restart, a crash, or an
// intentional Impact.DaemonRestart step); the old goroutine that was
// advancing it is gone, so nothing else will ever continue these runs
// unless this does. A run left "awaiting_checkpoint" is untouched — it is
// correctly parked for a human, restart or not, and is never resumed
// automatically. reconcileExecuting's wall-clock stale-executing sweep
// excludes kind=procedure rows entirely; this function is their only
// liveness path.
func (s *Smith) resumeProcedureRuns(ctx context.Context) {
	if s.d.Store == nil {
		return
	}
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT a.id FROM smith_actions a
		 JOIN smith_procedure_runs r ON r.action_id = a.id
		 WHERE a.kind = ? AND a.status = ? AND r.status = ?`,
		KindProcedure, StatusExecuting, procRunStatusRunning)
	if err != nil {
		s.logf("resume procedure runs: query: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			s.logf("resume procedure runs: scan: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		s.logf("resume procedure runs: %v", err)
	}
	rows.Close()

	for _, id := range ids {
		s.logf("resuming procedure run for action %d after restart", id)
		go s.continueProcedureRun(ctx, id)
	}
}

// HasLiveProcedureRun reports whether any procedure run currently holds
// leaseID and is still live (running or awaiting_checkpoint) — main.go
// calls this before maintenance.Gate.ReconcileOnBoot's blanket force-exit,
// so a maintenance window a live-resumable run legitimately holds survives
// the boot reconcile instead of being torn out from under
// resumeProcedureRuns.
func (s *Smith) HasLiveProcedureRun(ctx context.Context, leaseID string) bool {
	if s.d.Store == nil || leaseID == "" {
		return false
	}
	var n int
	err := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM smith_procedure_runs WHERE lease_id = ? AND status IN (?, ?)`,
		leaseID, procRunStatusRunning, procRunStatusAwaitingCheckpoint).Scan(&n)
	return err == nil && n > 0
}

// GetProcedureRun returns actionID's run, or an error wrapping
// sql.ErrNoRows when the action has no procedure run (wrong kind, or the
// row hasn't been created yet — dispatchProcedure creates it as its first
// step, so a pending/approved action has none).
func (s *Smith) GetProcedureRun(ctx context.Context, actionID int64) (*ProcedureRun, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	run, err := s.getProcedureRunByAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	return run, nil
}

// ── Sprint 4: supervision & evaluation harness ──────────────────────────
// (docs/v5-smith.md §13). The per-step journal (ProcedureStepOutcome, above)
// and the audit trail (finishExecution / checkpoint approve+abort, already
// written) are the raw record; what's missing before the Sprint 6 capstone
// evaluation runs is a way to browse past runs and read the evaluation
// questions ("did it finish unattended", "was the downtime estimate
// honest") off that record instead of reconstructing them by hand.

// ProcedureRunSummary is one row of the run history list (Diagnostics'
// "Procedure runs" section) — a ProcedureRun plus the two fields from its
// linked action a list view needs (title, and the action's own final
// status, which is a stronger "did this actually work" signal than the
// run's own status: a run can finish `completed` while its action still
// reads `done_unverified` because post-verify came back dirty).
type ProcedureRunSummary struct {
	ProcedureRun
	ActionTitle  string `json:"action_title"`
	ActionStatus string `json:"action_status"`
}

// ListProcedureRuns returns the most recent procedure runs of any status,
// most-recent-first, for the run history view. Every run is durable
// (smith_procedure_runs is never pruned by retention.go — only findings
// are), so this is a straight read, not a reconstruction.
func (s *Smith) ListProcedureRuns(ctx context.Context, limit int) ([]ProcedureRunSummary, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT r.id, r.action_id, r.procedure_id, r.status, r.current_step, r.lease_id,
		        r.steps_result, r.checkpoint_note, r.started_at, r.heartbeat_at, r.finished_at,
		        a.title, a.status
		 FROM smith_procedure_runs r
		 JOIN smith_actions a ON a.id = r.action_id
		 ORDER BY r.started_at DESC, r.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("smith: list procedure runs: %w", err)
	}
	defer rows.Close()

	var out []ProcedureRunSummary
	for rows.Next() {
		var sum ProcedureRunSummary
		run, err := scanProcedureRun(rows, &sum.ActionTitle, &sum.ActionStatus)
		if err != nil {
			return nil, fmt.Errorf("smith: list procedure runs: scan: %w", err)
		}
		sum.ProcedureRun = *run
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("smith: list procedure runs: %w", err)
	}
	return out, nil
}

// ProcedureScorecard is the evaluation record for one run, computed on read
// from the run + its registered Procedure + the linked action's final
// status. Nothing here is stored separately — a stored copy could drift
// from the durable run/action rows it's derived from, and there's no need:
// every input is already permanent.
type ProcedureScorecard struct {
	ActionID     int64  `json:"action_id"`
	ProcedureID  string `json:"procedure_id"`
	RunStatus    string `json:"run_status"`
	ActionStatus string `json:"action_status"`
	Completed    bool   `json:"completed"`
	// PreconditionFailed is true when the run never executed a single step —
	// its preconditions weren't met, so this is "not applicable to this
	// host," not a genuine attempt-and-failure. Exposed as its own bool
	// (rather than making every caller string-compare RunStatus) so a
	// scorecard reader can tell "nothing to investigate here" apart from a
	// real mid-run failure at a glance.
	PreconditionFailed bool `json:"precondition_failed"`
	// UnattendedCompletion is true only when the run finished completed
	// AND never paused at a declared judgment checkpoint (Step.Checkpoint)
	// — a run that paused for a failure (OnFail: checkpoint) but was then
	// approved through to completion still counts as needing a human, so
	// it's excluded too (CheckpointsReached counts both kinds — see below).
	UnattendedCompletion bool `json:"unattended_completion"`
	// CheckpointsDeclared is how many of the registered procedure's steps
	// declare Checkpoint: true — the plan's own judgment points, fixed at
	// registration time.
	CheckpointsDeclared int `json:"checkpoints_declared"`
	// CheckpointsReached is how many of THIS run's completed steps were a
	// declared checkpoint (i.e. how many times the run actually paused for
	// a human on the happy path) plus one more if the run is currently (or
	// ended) paused on a step-failure checkpoint (OnFail: checkpoint) —
	// both are real instances of "Smith stopped and asked a human," which
	// is the thing this field measures.
	CheckpointsReached int `json:"checkpoints_reached"`
	// PostVerifyPassed reflects the ACTION's final status, not the run's —
	// a run can finish `completed` (every step executed) while the action
	// still resolves `done_unverified` because post-verify came back dirty
	// or the collector snapshot hadn't refreshed yet (finalizeResult).
	PostVerifyPassed      bool  `json:"post_verify_passed"`
	StepsTotal            int   `json:"steps_total"`
	StepsCompleted        int   `json:"steps_completed"`
	NeedsMaintenance      bool  `json:"needs_maintenance"`
	EstDurationSeconds    int64 `json:"est_duration_seconds,omitempty"`
	ActualDurationSeconds int64 `json:"actual_duration_seconds,omitempty"`
}

// ProcedureScorecard computes actionID's scorecard. Returns an error
// wrapping sql.ErrNoRows when the action has no procedure run, same
// convention as GetProcedureRun. A run whose procedure has since been
// deregistered (renamed/removed from the coded registry) still scores —
// CheckpointsDeclared/EstDurationSeconds just read as zero, since that
// declared-at-registration data is no longer available; everything read
// off the run and action rows themselves is unaffected.
func (s *Smith) ProcedureScorecard(ctx context.Context, actionID int64) (*ProcedureScorecard, error) {
	run, err := s.getProcedureRunByAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	a, err := s.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	proc, procKnown := procedures.Get(run.ProcedureID)

	sc := &ProcedureScorecard{
		ActionID:           actionID,
		ProcedureID:        run.ProcedureID,
		RunStatus:          run.Status,
		ActionStatus:       a.Status,
		Completed:          run.Status == procRunStatusCompleted,
		PreconditionFailed: run.Status == procRunStatusPreconditionFailed,
		StepsCompleted:     len(run.Steps),
		PostVerifyPassed:   a.Status == StatusDone,
	}
	if procKnown {
		sc.StepsTotal = len(proc.Steps)
		sc.NeedsMaintenance = proc.Impact.NeedsMaintenance
		if proc.Impact.NeedsMaintenance {
			sc.EstDurationSeconds = int64(proc.Impact.EstDuration.Seconds())
		}
		for i := 0; i < len(run.Steps) && i < len(proc.Steps); i++ {
			if proc.Steps[i].Checkpoint {
				sc.CheckpointsDeclared++
				sc.CheckpointsReached++
			}
		}
		for i := len(run.Steps); i < len(proc.Steps); i++ {
			if proc.Steps[i].Checkpoint {
				sc.CheckpointsDeclared++
			}
		}
	} else {
		sc.StepsTotal = len(run.Steps)
	}
	// A run paused awaiting_checkpoint is either (a) a declared Checkpoint:
	// true step that just succeeded — runProcedureSteps already incremented
	// CurrentStep past it before pausing, so len(Steps) == CurrentStep, and
	// the loop above already counted it via proc.Steps[len(Steps)-1]; or
	// (b) a step that FAILED with OnFail: checkpoint — CurrentStep is NOT
	// incremented on failure, so len(Steps) == CurrentStep+1, and the loop
	// above never counted it (that step's own Checkpoint field is
	// irrelevant to a failure pause). Only (b) needs adding here, or a
	// checkpoint that fired via a genuinely declared step gets double-
	// counted (a real bug caught by TestProcedureScorecard_
	// CheckpointCountsAsAttended before this fix).
	if run.Status == procRunStatusAwaitingCheckpoint && len(run.Steps) == run.CurrentStep+1 {
		sc.CheckpointsReached++
	}
	sc.UnattendedCompletion = sc.Completed && sc.CheckpointsReached == 0
	if run.FinishedAt != nil {
		sc.ActualDurationSeconds = *run.FinishedAt - run.StartedAt
	}
	return sc, nil
}

// ── persistence ──────────────────────────────────────────────────────────

const procedureRunColumns = `id, action_id, procedure_id, status, current_step, lease_id,
	steps_result, checkpoint_note, started_at, heartbeat_at, finished_at`

func (s *Smith) getOrCreateProcedureRun(ctx context.Context, actionID int64, procID string) (*ProcedureRun, error) {
	run, err := s.getProcedureRunByAction(ctx, actionID)
	if err == nil {
		return run, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	now := s.d.Now().Unix()
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`INSERT INTO smith_procedure_runs
			(action_id, procedure_id, status, current_step, steps_result, started_at, heartbeat_at)
		 VALUES (?, ?, ?, 0, '[]', ?, ?)`,
		actionID, procID, procRunStatusRunning, now, now); err != nil {
		return nil, fmt.Errorf("smith: create procedure run: %w", err)
	}
	return s.getProcedureRunByAction(ctx, actionID)
}

// getProcedureRunByAction returns an error wrapping sql.ErrNoRows (via %w)
// when actionID has no run yet, so callers (getOrCreateProcedureRun's
// create-on-miss, and httpapi's writeActionFetchError 404 path via
// GetProcedureRun) can errors.Is against it exactly like GetAction already
// documents for a missing action id.
func (s *Smith) getProcedureRunByAction(ctx context.Context, actionID int64) (*ProcedureRun, error) {
	row := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT `+procedureRunColumns+` FROM smith_procedure_runs WHERE action_id = ?`, actionID)
	run, err := scanProcedureRun(row)
	if err != nil {
		return nil, fmt.Errorf("smith: get procedure run for action %d: %w", actionID, err)
	}
	return run, nil
}

func (s *Smith) persistProcedureRun(ctx context.Context, run *ProcedureRun) error {
	stepsJSON, err := json.Marshal(run.Steps)
	if err != nil {
		return fmt.Errorf("smith: marshal procedure run steps: %w", err)
	}
	run.HeartbeatAt = s.d.Now().Unix()
	var leaseVal, noteVal, finishedVal any
	if run.LeaseID != "" {
		leaseVal = run.LeaseID
	}
	if run.CheckpointNote != "" {
		noteVal = run.CheckpointNote
	}
	if run.FinishedAt != nil {
		finishedVal = *run.FinishedAt
	}
	_, err = s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_procedure_runs
		 SET status = ?, current_step = ?, lease_id = ?, steps_result = ?, checkpoint_note = ?,
		     heartbeat_at = ?, finished_at = ?
		 WHERE id = ?`,
		run.Status, run.CurrentStep, leaseVal, string(stepsJSON), noteVal, run.HeartbeatAt, finishedVal, run.ID)
	if err != nil {
		return fmt.Errorf("smith: persist procedure run %d: %w", run.ID, err)
	}
	return nil
}

func (s *Smith) heartbeatProcedureRun(ctx context.Context, run *ProcedureRun) {
	run.HeartbeatAt = s.d.Now().Unix()
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_procedure_runs SET heartbeat_at = ? WHERE id = ?`, run.HeartbeatAt, run.ID); err != nil {
		s.logf("procedure run %d: heartbeat: %v", run.ID, err)
	}
}

// casProcedureRunStatus atomically moves run id from `from` to `to`,
// reporting whether the row was in `from` (false ⇒ some other caller
// already moved it — the standard double-submit guard this codebase uses
// everywhere else in the action model).
func (s *Smith) casProcedureRunStatus(ctx context.Context, id int64, from, to string) (bool, error) {
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_procedure_runs SET status = ? WHERE id = ? AND status = ?`, to, id, from)
	if err != nil {
		return false, fmt.Errorf("smith: cas procedure run %d %s->%s: %w", id, from, to, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// scanProcedureRun scans one procedureRunColumns-ordered row. extra, when
// given, is appended to the Scan call as-is — ListProcedureRuns' joined
// query uses it for the two trailing a.title/a.status columns rather than
// duplicating the 11-column scan for a summary row.
func scanProcedureRun(row rowScanner, extra ...any) (*ProcedureRun, error) {
	var r ProcedureRun
	var leaseCol, noteCol sql.NullString
	var stepsJSON string
	var finishedAt sql.NullInt64
	dest := []any{
		&r.ID, &r.ActionID, &r.ProcedureID, &r.Status, &r.CurrentStep, &leaseCol,
		&stepsJSON, &noteCol, &r.StartedAt, &r.HeartbeatAt, &finishedAt,
	}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if leaseCol.Valid {
		r.LeaseID = leaseCol.String
	}
	if noteCol.Valid {
		r.CheckpointNote = noteCol.String
	}
	if finishedAt.Valid {
		v := finishedAt.Int64
		r.FinishedAt = &v
	}
	if stepsJSON != "" {
		if err := json.Unmarshal([]byte(stepsJSON), &r.Steps); err != nil {
			return nil, fmt.Errorf("smith: unmarshal procedure run steps: %w", err)
		}
	}
	return &r, nil
}

// ── small helpers ────────────────────────────────────────────────────────

func durationMS(measured, fallback time.Duration) int64 {
	if measured > 0 {
		return measured.Milliseconds()
	}
	return fallback.Milliseconds()
}

// truncateAndRedact bounds a captured stdout/stderr blob to
// procedureStdoutTailBytes (keeping the TAIL — the end of a long build log
// is almost always the interesting part) and scrubs secret-shaped
// substrings before it's ever persisted.
func truncateAndRedact(s string) string {
	if len(s) > procedureStdoutTailBytes {
		s = s[len(s)-procedureStdoutTailBytes:]
	}
	return scrubSecretPatterns(s)
}
