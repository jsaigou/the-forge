// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
	"github.com/jsaigou/the-forge/internal/store"
)

// EventHandoffUpdate is the SSE event published on every handoff state
// change (Contract 1 amendment, docs/v5-smith.md §5).
const EventHandoffUpdate = "smith:handoff_update"

// Placer is the slice of engine behaviour the action model needs: the
// eviction-aware fit plan (self-eviction stamping, handoff.go) plus the
// slot lifecycle operations load_config/unload_slot execute against
// (dispatchLoadConfig/dispatchUnloadSlot below). *engine.Manager satisfies
// it as-is (compile-checked below). Same seam shape as sched.Engine
// (internal/sched/core.go:20-36) — a named, explicitly-wired interface
// rather than a type assertion on Deps.Engine: a failed type assertion
// degrades silently, and a silently-unavailable FitPlan would mean
// self-eviction stamping silently returns "safe" — the one failure mode
// this whole feature exists to prevent. FitPlan is not on engine.Engine; it
// lives on *engine.Manager (engine/memory.go:131).
type Placer interface {
	FitPlan(mode string) (engine.Plan, error)
	Load(ctx context.Context, mode, slot string) engine.Result
	Unload(ctx context.Context, slot string) engine.Result
	Slots() []string
}

var _ Placer = (*engine.Manager)(nil)

// staleExecutingThreshold is how long an action may sit in "executing"
// before Smith.Start's crash-consistency reconciler gives up on it and
// marks it failed — a daemon restart mid-execution otherwise strands the
// row forever (no other code path ever revisits an "executing" row).
const staleExecutingThreshold = 15 * time.Minute

// reconcileExecuting runs once at Start() before the periodic scheduler
// begins: any row still "executing" with executed_at older than
// staleExecutingThreshold becomes "failed" — deliberately NOT
// "done_unverified", because unlike a completed-but-unconfirmed operation,
// we don't even know whether this one started, finished, or crashed
// mid-flight.
//
// kind != 'procedure' excludes every procedure-backed action from this
// wall-clock reap: resumeProcedureRuns (procedure.go), called just before
// this from Start(), is their own liveness path — a multi-hour procedure
// run is exactly the "it's still legitimately in progress" case this
// threshold exists to distinguish from a real crash, and a run correctly
// parked at a checkpoint must never be aged out regardless of how long a
// human takes to get back to it.
func (s *Smith) reconcileExecuting(ctx context.Context) {
	if s.d.Store == nil {
		return
	}
	cutoff := s.d.Now().Add(-staleExecutingThreshold).Unix()
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT id FROM smith_actions WHERE status = ? AND kind != ? AND executed_at IS NOT NULL AND executed_at < ?`,
		StatusExecuting, KindProcedure, cutoff)
	if err != nil {
		s.logf("reconcile executing actions: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			s.logf("reconcile executing actions: scan: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		s.logf("reconcile executing actions: %v", err)
	}
	rows.Close()

	for _, id := range ids {
		result, _ := json.Marshal(ActionResult{
			OK:    false,
			Error: "daemon restarted or crashed during execution; outcome unknown — re-run the checks",
		})
		now := s.d.Now().Unix()
		res, err := s.d.Store.SQL().ExecContext(ctx,
			`UPDATE smith_actions SET status = ?, result = ?, resolved_at = ? WHERE id = ? AND status = ?`,
			StatusFailed, string(result), now, id, StatusExecuting)
		if err != nil {
			s.logf("reconcile executing action %d: %v", id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		s.logf("reconciled stale executing action %d -> failed (crash/restart during execution)", id)
		if s.d.Publisher != nil {
			s.d.Publisher.Publish(EventActionUpdate, map[string]any{"action_id": id, "status": StatusFailed})
		}
	}
}

// executeAction runs an approved action's dispatch + post-verify pass, then
// finalizes its status. Invoked as a goroutine from ApproveAction — never
// called synchronously from an HTTP handler (execution can take as long as a
// model load).
func (s *Smith) executeAction(ctx context.Context, id int64) {
	a, err := s.GetAction(ctx, id)
	if err != nil {
		s.logf("execute action %d: fetch: %v", id, err)
		return
	}

	execAt := s.d.Now()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, executed_at = ? WHERE id = ? AND status = ?`,
		StatusExecuting, execAt.Unix(), id, StatusApproved)
	if err != nil {
		s.logf("execute action %d: CAS to executing: %v", id, err)
		// A write error here (not the ordinary "0 rows" race below) would
		// otherwise strand the row in "approved" forever — approve is a
		// one-shot trigger, nothing else ever revisits it. Best-effort
		// approved→failed (the documented legalTransitions escape hatch for
		// exactly this) so the row surfaces as needing attention instead of
		// silently vanishing from the pending/approved views.
		s.failApproved(ctx, id, fmt.Sprintf("could not start execution: %v", err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		s.logf("execute action %d: no longer 'approved', skipping execution", id)
		return
	}
	execAtUnix := execAt.Unix()
	a.Status = StatusExecuting
	a.ExecutedAt = &execAtUnix
	s.publishActionUpdate(ctx, a, StatusExecuting)

	var dispatchErr error
	var unit string       // set by dispatchRestartUnit, used to pick the verify check
	var resultNote string // set by dispatchDeleteFiles: the actually-reclaimed-bytes summary
	switch a.Kind {
	case KindLoadConfig:
		dispatchErr = s.dispatchLoadConfig(ctx, a)
	case KindUnloadSlot:
		dispatchErr = s.dispatchUnloadSlot(ctx, a)
	case KindRestartForgeUnit:
		unit, dispatchErr = s.dispatchRestartUnit(ctx, a)
	case KindSettingsChange:
		dispatchErr = s.dispatchSettingsChange(ctx, a)
	case KindDeleteFiles:
		resultNote, dispatchErr = s.dispatchDeleteFiles(ctx, a)
	case KindCatalogChange:
		dispatchErr = s.dispatchCatalogChange(ctx, a)
	case KindProcedure:
		unit, dispatchErr = s.dispatchProcedure(ctx, a)
	default:
		// Unreachable in practice — ApproveAction short-circuits KindRunbook
		// before ever spawning executeAction, and CreateAction rejects any
		// other kind. Guarded here anyway so a future kind added to one
		// switch and not the other fails loudly in logs, not via panic.
		s.logf("execute action %d: unrecognized kind %q reached the executor", id, a.Kind)
	}

	if errors.Is(dispatchErr, ErrCheckpointPaused) {
		// The procedure run paused at a Step.Checkpoint gate — leave the
		// action "executing" (not a failure, not done); dispatchProcedure
		// already published smith:procedure_step for this. Approving or
		// aborting the checkpoint (procedure.go) is what finalizes the
		// action from here, via finishExecution.
		return
	}

	finalStatus, result := s.finalizeResult(ctx, a.Kind, unit, execAt, dispatchErr)
	if resultNote != "" {
		if result.Message != "" {
			result.Message = resultNote + " — " + result.Message
		} else {
			result.Message = resultNote
		}
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		resultJSON = []byte(`{"ok":false,"error":"smith: failed to marshal execution result"}`)
	}
	now := s.d.Now().Unix()
	upd, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, result = ?, verified_at = ?, resolved_at = ? WHERE id = ? AND status = ?`,
		finalStatus, string(resultJSON), now, now, id, StatusExecuting)
	if err != nil {
		s.logf("execute action %d: CAS to %s: %v", id, finalStatus, err)
		return
	}
	if n, _ := upd.RowsAffected(); n == 0 {
		s.logf("execute action %d: could not finalize to %s (row left executing)", id, finalStatus)
		return
	}
	final, err := s.GetAction(ctx, id)
	if err != nil {
		s.logf("execute action %d: refetch after finalize: %v", id, err)
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

// proposeSwapBack auto-proposes a settings_change action to restore
// smith.model to its pre-handoff value, once a remote-swapped operation
// finishes (any final status — a return path is offered even after a
// failure, since the brain is still remote either way). Deliberately a
// normal, separately-approved proposal, never auto-executed: every state
// change stays behind approval (§3 constraint 1), and unlike an in-process
// auto-restore this survives a daemon restart between the operation
// finishing and an operator getting back to it. Uses createOrReuseProposal
// (propose.go) so a stale prior swap-back proposal for the same action
// (e.g. a retry after a crash) is reused/superseded rather than duplicated.
func (s *Smith) proposeSwapBack(ctx context.Context, a *Action) {
	if a.Handoff.BrainModel == "" {
		return
	}
	val, err := json.Marshal(a.Handoff.BrainModel)
	if err != nil {
		s.logf("propose swap-back for action %d: marshal model: %v", a.ID, err)
		return
	}
	detail, err := json.Marshal(settingsChangeDetail{Key: SettingModel, Value: val})
	if err != nil {
		s.logf("propose swap-back for action %d: marshal detail: %v", a.ID, err)
		return
	}
	draft := ActionDraft{
		Kind:      KindSettingsChange,
		Title:     "Return smith's brain to " + a.Handoff.BrainModel,
		Detail:    detail,
		Risk:      RiskLow,
		CreatedBy: "smith",
		DedupeKey: fmt.Sprintf("swapback:%d", a.ID),
	}
	if _, _, err := s.createOrReuseProposal(ctx, draft); err != nil {
		s.logf("propose swap-back for action %d: %v", a.ID, err)
	}
}

// failApproved is the approved→failed escape hatch (legalTransitions
// documents it; executeAction's only user is the CAS-to-executing write
// error path above). Best-effort: if even this write fails, there's nothing
// further to do but log — the row stays approved and a human/reconciler
// will need to notice.
func (s *Smith) failApproved(ctx context.Context, id int64, reason string) {
	result, _ := json.Marshal(ActionResult{OK: false, Error: reason, Verify: []VerifyResult{}})
	now := s.d.Now().Unix()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, result = ?, resolved_at = ? WHERE id = ? AND status = ?`,
		StatusFailed, string(result), now, id, StatusApproved)
	if err != nil {
		s.logf("execute action %d: failApproved: %v", id, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	if a, err := s.GetAction(ctx, id); err == nil {
		s.publishActionUpdate(ctx, a, StatusFailed)
	}
}

// finalizeResult runs post-verify (when dispatch succeeded) and decides the
// final status. This is the load-bearing correctness property of the whole
// action model: a dispatch success is NEVER promoted to "done" unless every
// mapped verify check came back clean AND the collector snapshot is
// genuinely newer than execAt — Placer.Load blocks until the load call
// returns, but the collector may not have re-polled yet, and a check run
// against a pre-operation snapshot proves nothing.
func (s *Smith) finalizeResult(ctx context.Context, kind, unit string, execAt time.Time, dispatchErr error) (string, ActionResult) {
	if dispatchErr != nil {
		return StatusFailed, ActionResult{OK: false, Error: dispatchErr.Error(), Verify: []VerifyResult{}}
	}

	verifyIDs := verifyChecksFor(kind, unit)
	var verifyFindings []Finding
	if len(verifyIDs) > 0 {
		verifyFindings = s.runChecksBare(ctx, verifyIDs)
	}
	result := ActionResult{OK: true, Verify: toVerifyResults(verifyFindings, s.d.Now())}

	allClean := true
	for _, f := range verifyFindings {
		if f.Severity == SeverityWarn || f.Severity == SeverityCrit {
			allClean = false
		}
	}
	snapFresh := false
	if s.d.Source != nil {
		if snap := s.d.Source.Current(); snap != nil && snap.TakenAt.After(execAt) {
			snapFresh = true
		}
	}

	switch {
	case len(verifyIDs) == 0:
		result.Message = "operation completed; no automatic post-verify is defined for this action kind"
		return StatusDoneUnverified, result
	case !snapFresh:
		result.Message = "operation completed but the collector snapshot has not refreshed since execution — cannot confirm the new state"
		return StatusDoneUnverified, result
	case !allClean:
		result.Message = "operation completed but a post-verify check reported warn or crit"
		return StatusDoneUnverified, result
	default:
		result.Message = "operation completed and re-verified clean"
		return StatusDone, result
	}
}

// runChecksBare runs the given checks against a fresh CheckEnv WITHOUT the
// sweeping mutex and WITHOUT persisting to smith_findings — RunChecks does
// both, and both are wrong for a post-verify pass (it isn't a sweep, and
// "did the operation work" findings aren't diagnostic history).
func (s *Smith) runChecksBare(ctx context.Context, checkIDs []string) []Finding {
	if len(checkIDs) == 0 {
		return nil
	}
	selected, err := selectChecks("", checkIDs)
	if err != nil {
		f := Finding{CheckID: "post_verify", Severity: SeverityInfo, Summary: "post-verify: " + err.Error()}
		return []Finding{f.normalize()}
	}
	env := s.checkEnv(ctx)
	out := make([]Finding, 0, len(selected))
	for _, c := range selected {
		out = append(out, runOne(ctx, c, env).normalize())
	}
	return out
}

// verifyChecksFor maps an action kind (+ for restart_forge_unit, the
// restarted unit) to the checks executeAction re-runs after dispatch.
func verifyChecksFor(kind, unit string) []string {
	switch kind {
	case KindLoadConfig:
		return []string{"slot_agreement", "n_ctx_actual", "gtt_ceiling"}
	case KindUnloadSlot:
		return []string{"slot_agreement"}
	case KindRestartForgeUnit:
		if strings.HasPrefix(unit, "headroom@") || strings.HasPrefix(unit, "headroom-") ||
			strings.HasPrefix(unit, "forge-compress@") {
			return []string{"compressor_reachability"}
		}
		return []string{"always_on_ports"}
	case KindSettingsChange:
		// Only smith.model touches something checkably real (brain-swap /
		// swap-back); every other allowlisted key (schedule/thresholds/
		// handoff_offerings) has no coded check to re-run — this returns
		// unconditionally for the whole kind rather than inspecting detail.key
		// because verifyChecksFor only sees kind+unit, not detail; a settings
		// change to a non-model key just harmlessly re-confirms brain
		// resolvability, which was already true before the change.
		return []string{"brain_resolvable"}
	case KindDeleteFiles:
		// comfyui_health confirms ComfyUI itself is still fine post-delete
		// (a real regression, not this action, would be a different bug);
		// disk_space is the actual reclaimed-space confirmation. A fresh
		// comfyui_prune re-run (full disk walk + 3 HTTP calls) is
		// deliberately NOT in this list — dispatchDeleteFiles already
		// stat()s every path it removed and reports real reclaimed bytes
		// directly in the result message, which is cheaper and more precise
		// than re-deriving it from a second full map build.
		return []string{"comfyui_health", "disk_space"}
	case KindProcedure:
		// unit here carries the procedure ID (dispatchProcedure's return
		// convention). The whole run's own per-step verify checks already
		// ran inline (runProcedureSteps) as each step completed; this
		// second pass — the SAME snapFresh + all-checks-clean discipline
		// finalizeResult applies to every other action kind — re-confirms
		// the union of every step's VerifyCheckIDs against a snapshot taken
		// after the run finished, not mid-run.
		return procedureVerifyChecks(unit)
	default:
		return nil
	}
}

// procedureVerifyChecks returns the de-duplicated union of every step's
// VerifyCheckIDs for procID, in first-seen order. Empty (not an error) for
// an unknown procID — finalizeResult already treats an empty verify list as
// "done_unverified, no automatic post-verify defined", which is the honest
// outcome for a procedure ID that no longer resolves (e.g. removed from the
// registry between dispatch and this call — should not happen, but this
// keeps the failure mode boring).
func procedureVerifyChecks(procID string) []string {
	proc, ok := procedures.Get(procID)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, step := range proc.Steps {
		for _, id := range step.VerifyCheckIDs {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// toVerifyResults projects post-verify findings into the wire VerifyResult
// shape, stamped with a single "at" timestamp for the whole batch.
func toVerifyResults(findings []Finding, now time.Time) []VerifyResult {
	at := now.Unix()
	out := make([]VerifyResult, 0, len(findings))
	for _, f := range findings {
		out = append(out, VerifyResult{CheckID: f.CheckID, Severity: string(f.Severity), Summary: f.Summary, At: at})
	}
	return out
}

// ── Per-kind detail shapes + dispatch ───────────────────────────────────────
//
// Each of these mirrors one Action.Detail JSON shape — the "request" a
// draft/proposal carries. Also reused by handoff.go's operationCommand for
// rendering the runbook's real equivalent command.

// loadConfigDetail is KindLoadConfig's detail shape.
type loadConfigDetail struct {
	Mode string `json:"mode"`
	Slot string `json:"slot"`
}

// unloadSlotDetail is KindUnloadSlot's detail shape.
type unloadSlotDetail struct {
	Slot string `json:"slot"`
}

// restartUnitDetail is KindRestartForgeUnit's detail shape.
type restartUnitDetail struct {
	Unit string `json:"unit"`
}

// settingsChangeDetail is KindSettingsChange's detail shape.
type settingsChangeDetail struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// deleteFileEntry is one file in a delete_files proposal — copied from the
// comfyui_prune finding's Candidates (comfyui.FileInfo) at proposal time,
// carried in the action's own Detail so the confirmation card can render
// evidence without re-running BuildMap (docs/v5-smith.md §4.9: "the
// confirmation card lists every file with its evidence before the approve
// button arms").
type deleteFileEntry struct {
	Path       string `json:"path"`
	FolderType string `json:"folder_type"`
	SizeBytes  int64  `json:"size_bytes"`
}

// deleteFilesDetail is KindDeleteFiles' detail shape. Guidance carries the
// operator-facing "how do I keep one of these" text (comfyUIKeepGuidance)
// straight into the confirmation card's own data — an operator reviewing
// the action doesn't have to go find the originating finding to learn how
// to change the outcome before approving.
type deleteFilesDetail struct {
	Files      []deleteFileEntry `json:"files"`
	TotalBytes int64             `json:"total_bytes"`
	Guidance   string            `json:"guidance,omitempty"`
}

// catalogChangeDetail is KindCatalogChange's detail shape (P6 FR4 — model
// sourcing proposals). Op is "create" or "update"; Table names the
// store.Catalog row kind ("model"|"variant"|"artifact"|"offering"); Row is
// the row itself as JSON (a full store.Model/Variant/Artifact/Offering,
// including ID for update). Kept generic rather than one detail shape per
// table: sourcing.go is the only writer, and a fixed small enum of tables
// beats four near-identical structs.
type catalogChangeDetail struct {
	Op    string          `json:"op"`
	Table string          `json:"table"`
	Row   json.RawMessage `json:"row"`
}

// parseDetail unmarshals raw into T, tolerating an empty/absent payload as
// the zero value.
func parseDetail[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("smith: parse action detail: %w", err)
	}
	return v, nil
}

// dispatchLoadConfig executes a load_config action via the Placer seam.
//
// §4.6's design note (issue #5): httpapi's real /api/v1/load route enforces
// two guards smith cannot reach directly — the single-instance
// already-loaded check and the slotLoading in-progress map — because smith
// cannot import httpapi (the reverse dependency; see investigations.go's
// note on this). This re-implements the one guard cheaply reachable from
// here (mode not already loaded on a DIFFERENT slot, via Sched) and
// republishes the same load_started/load_complete/load_failed event names
// httpapi uses, so the Console reacts identically regardless of which path
// triggered the load.
func (s *Smith) dispatchLoadConfig(ctx context.Context, a *Action) error {
	if s.d.Placer == nil {
		return ErrPlacerUnwired
	}
	d, err := parseDetail[loadConfigDetail](a.Detail)
	if err != nil {
		return err
	}
	if d.Mode == "" || d.Slot == "" {
		return errors.New("smith: load_config detail requires mode and slot")
	}
	if s.d.Sched != nil {
		for slot, mode := range s.d.Sched.Status().Slots {
			if mode == d.Mode && slot != d.Slot {
				return fmt.Errorf("smith: %s is already loaded on slot %s", d.Mode, slot)
			}
		}
	}
	if s.d.Publisher != nil {
		s.d.Publisher.Publish("load_started", map[string]any{"slot": d.Slot, "mode": d.Mode})
	}
	result := s.d.Placer.Load(ctx, d.Mode, d.Slot)
	if s.d.Publisher != nil {
		name := "load_complete"
		if !result.Success {
			name = "load_failed"
		}
		s.d.Publisher.Publish(name, map[string]any{
			"slot": d.Slot, "mode": d.Mode,
			"result": map[string]any{"success": result.Success, "message": result.Message, "n_ctx": result.NCtx},
		})
	}
	if !result.Success {
		return fmt.Errorf("smith: load %s into %s: %s", d.Mode, d.Slot, result.Message)
	}
	return nil
}

// dispatchUnloadSlot executes an unload_slot action via the Placer seam.
func (s *Smith) dispatchUnloadSlot(ctx context.Context, a *Action) error {
	if s.d.Placer == nil {
		return ErrPlacerUnwired
	}
	d, err := parseDetail[unloadSlotDetail](a.Detail)
	if err != nil {
		return err
	}
	if d.Slot == "" {
		return errors.New("smith: unload_slot detail requires slot")
	}
	result := s.d.Placer.Unload(ctx, d.Slot)
	if s.d.Publisher != nil {
		s.d.Publisher.Publish("unload_complete", map[string]any{
			"slot":   d.Slot,
			"result": map[string]any{"success": result.Success, "message": result.Message},
		})
	}
	if !result.Success {
		return fmt.Errorf("smith: unload slot %s: %s", d.Slot, result.Message)
	}
	return nil
}

// dispatchRestartUnit executes a restart_forge_unit action via
// Deps.RestartUnit, re-checking restartAllowed at execution time (not just
// at proposal/creation time — config can change in between). Returns the
// target unit regardless of outcome, so executeAction can pick the right
// verify check even on failure.
func (s *Smith) dispatchRestartUnit(ctx context.Context, a *Action) (string, error) {
	d, err := parseDetail[restartUnitDetail](a.Detail)
	if err != nil {
		return "", err
	}
	if ok, reason := restartAllowed(s.cfg(), d.Unit); !ok {
		return d.Unit, fmt.Errorf("smith: unit %q not allowed: %s: %w", d.Unit, reason, ErrUnitNotAllowed)
	}
	if s.d.RestartUnit == nil {
		return d.Unit, ErrRestartUnwired
	}
	if err := s.d.RestartUnit(ctx, d.Unit); err != nil {
		return d.Unit, fmt.Errorf("smith: restart %s: %w", d.Unit, err)
	}
	return d.Unit, nil
}

// dispatchSettingsChange executes a settings_change action, gated to the
// smith.* allowlist. The previous value is recorded on the audit entry
// (not the action's own detail, which is the immutable request) so the
// change is hand-reversible from the audit log.
func (s *Smith) dispatchSettingsChange(ctx context.Context, a *Action) error {
	d, err := parseDetail[settingsChangeDetail](a.Detail)
	if err != nil {
		return err
	}
	if !settingsKeyAllowed(d.Key) {
		return fmt.Errorf("smith: settings key %q not allowed: %w", d.Key, ErrKeyNotAllowed)
	}
	if s.d.Settings == nil {
		return errors.New("smith: settings not wired")
	}
	if len(d.Value) == 0 {
		return errors.New("smith: settings_change detail requires a value")
	}
	previous, prevErr := s.d.Settings.Get(ctx, d.Key)
	if err := s.d.Settings.Set(ctx, d.Key, d.Value); err != nil {
		return fmt.Errorf("smith: write setting %s: %w", d.Key, err)
	}
	if s.d.Audit != nil {
		detail := map[string]any{"key": d.Key, "new": json.RawMessage(d.Value)}
		if prevErr == nil {
			detail["previous"] = json.RawMessage(previous)
		}
		b, _ := json.Marshal(detail)
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: "smith", Action: "smith_settings_change", Target: d.Key, Detail: string(b),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	return nil
}

// dispatchDeleteFiles executes a delete_files action (P6 FR7), re-validating
// every path against the CURRENTLY configured smith.comfyui.model_roots
// (not whatever roots were configured at proposal time — config can change
// in between, same reasoning as dispatchRestartUnit's restartAllowed
// re-check) before removing anything. Fails closed: if ANY path fails
// deleteAllowed, nothing is deleted at all (the whole batch is one
// approval, so a partially-invalid batch is a proposal bug worth surfacing
// loudly, not a reason to silently skip one file and delete the rest).
// Returns a human-readable reclaimed-bytes summary (executeAction folds
// this into the result message) — computed directly from which deletes
// actually succeeded, not by re-deriving it from a second BuildMap call.
func (s *Smith) dispatchDeleteFiles(ctx context.Context, a *Action) (string, error) {
	d, err := parseDetail[deleteFilesDetail](a.Detail)
	if err != nil {
		return "", err
	}
	if len(d.Files) == 0 {
		return "", errors.New("smith: delete_files detail requires at least one file")
	}
	roots := s.ComfyUIModelRoots(ctx)
	for _, f := range d.Files {
		if ok, reason := deleteAllowed(roots, f.Path); !ok {
			return "", fmt.Errorf("smith: path %q not allowed: %s: %w", f.Path, reason, ErrPathNotAllowed)
		}
	}
	if s.d.DeleteFile == nil {
		return "", ErrDeleteUnwired
	}

	var reclaimed int64
	var deleted, failed []string
	var firstErr error
	for _, f := range d.Files {
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
	note := fmt.Sprintf("deleted %d of %d file(s), reclaimed %.1f GB", len(deleted), len(d.Files), float64(reclaimed)/(1<<30))
	if len(failed) > 0 {
		return note, fmt.Errorf("smith: %d of %d file(s) failed to delete, first error: %w", len(failed), len(d.Files), firstErr)
	}
	return note, nil
}

// deleteAllowed reports whether path may be deleted by a smith action: it
// must resolve (symlinks included) to somewhere strictly inside one of
// roots, and must not be a directory. Re-checked at proposal time
// (propose.go's proposeComfyUIPrune) AND at dispatch time here — the same
// double-check convention as restartAllowed, and the ONLY thing standing
// between an approved action and a real `rm` on the filesystem, so it fails
// closed on any error (a Lstat failure is never treated as "safe").
func deleteAllowed(roots []string, path string) (bool, string) {
	if path == "" {
		return false, "empty path"
	}
	if !filepath.IsAbs(path) {
		return false, "path must be absolute"
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, "resolve: " + err.Error()
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return false, "stat: " + err.Error()
	}
	if info.IsDir() {
		return false, "path is a directory"
	}
	for _, root := range roots {
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue // an unresolvable configured root just isn't a match — not this function's error to report
		}
		rel, err := filepath.Rel(rootResolved, resolved)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true, ""
		}
	}
	return false, "path is not under any configured comfyui model root"
}

// dispatchCatalogChange executes a catalog_change action (P6 FR4). See
// sourcing.go.
func (s *Smith) dispatchCatalogChange(ctx context.Context, a *Action) error {
	d, err := parseDetail[catalogChangeDetail](a.Detail)
	if err != nil {
		return err
	}
	return s.applyCatalogChange(ctx, d)
}

// cfg returns the infra config or nil when no Cfg func is wired — the same
// pattern CheckEnv.cfg() uses.
func (s *Smith) cfg() *config.Config {
	if s.d.Cfg == nil {
		return nil
	}
	return s.d.Cfg()
}

// ── restart_forge_unit allowlist ──────────────────────────────────────────

// compressorUnitPattern matches headroom@<service>/headroom-<name> (the
// topology-redesign dynamic proxy units, docs/v5-headroom-topology.md, and
// any surviving legacy hand-created unit predating Sprint 3 — tolerated
// here as defense-in-depth even though Sprint 6 confirmed none are live
// any more) and forge-compress@<service> (the Sprint 3 Go replacement's
// own dynamic proxy units, docs/v5-headroom-replacement.md — needs no new
// polkit grant, since forge-compress@*.service already matches
// 50-forge.rules's existing ^forge-.*\.service$ glob).
var compressorUnitPattern = regexp.MustCompile(`^(headroom[@-]|forge-compress@)[a-z0-9_-]+$`)

// restartAllowed reports whether unit may be restarted by a smith action,
// and if not, why. This is strictly NARROWER than the real polkit grant
// (polkit/50-forge.rules + 51-headroom.rules — forge-*.service,
// ai-mode-comfyui.service, compressor-*.service, headroom@*.service, all
// passwordless for the user forge runs as): this function adds no
// privilege, it only decides which of those already-grantable units smith
// itself may touch autonomously. Re-checked at proposal/creation time
// (propose.go) AND at execution time (dispatchRestartUnit) — config can
// change in between.
func restartAllowed(cfg *config.Config, unit string) (bool, string) {
	if unit == "" {
		return false, "empty unit name"
	}
	// Injection guard: unit is concatenated with ".service" inside
	// DBus.Restart, so anything resembling a second command or path segment
	// is rejected outright, independent of the allowlist below.
	if strings.ContainsAny(unit, "./ \t\n;|&$`") {
		return false, "unit name contains disallowed characters"
	}
	if unit == "forge-daemon" {
		return false, "restarting forge-daemon would kill the executing action itself — propose a runbook instead"
	}
	if cfg == nil {
		return false, "config not wired"
	}
	for _, slot := range cfg.Slots {
		if slot.Unit != "" && slot.Unit == unit {
			return false, "unit " + unit + " belongs to a scheduler slot — use unload_slot/load_config instead of restarting it directly"
		}
	}
	switch unit {
	case "forge-stt", "forge-embedding", "forge-aligner", "ai-mode-comfyui":
		return true, ""
	}
	if cfg.Server.TTSUnit != "" && unit == cfg.Server.TTSUnit {
		return true, ""
	}
	if compressorUnitPattern.MatchString(unit) {
		return true, ""
	}
	for _, m := range cfg.Modes {
		if m.Type == "service" && m.Unit != "" && m.Unit == unit {
			return true, ""
		}
	}
	return false, "unit " + unit + " is not on the smith restart allowlist"
}

// ── settings_change allowlist ───────────────────────────────────────────────

// allowedSmithSettingsKeys keeps settings_change confined to smith's own
// settings vocabulary — never auth.policy, infra.*, or provider keys.
var allowedSmithSettingsKeys = map[string]bool{
	SettingModel:            true,
	SettingHandoffOfferings: true,
	SettingSchedule:         true,
	SettingThresholds:       true,
}

// settingsKeyAllowed reports whether key is on the smith.* allowlist.
func settingsKeyAllowed(key string) bool {
	return allowedSmithSettingsKeys[key]
}
