// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jsaigou/the-forge/internal/smith/procedures"
	"github.com/jsaigou/the-forge/internal/store"
)

// Action kinds (docs/v5-smith.md §4.6). run_script remains deliberately
// deferred (no allowlisted scripts dir wired) — catalog_change and
// delete_files land in P6 (§4.9).
const (
	KindRunbook          = "runbook"
	KindLoadConfig       = "load_config"
	KindUnloadSlot       = "unload_slot"
	KindRestartForgeUnit = "restart_forge_unit"
	KindSettingsChange   = "settings_change"
	// KindCatalogChange executes a create/update against store.Catalog
	// (P6 FR4 — model sourcing proposals). Risk is always RiskLow: it only
	// ever adds or edits catalog rows, never deletes.
	KindCatalogChange = "catalog_change"
	// KindDeleteFiles executes real file deletion, confined to an
	// allowlisted set of roots (P6 FR7 — ComfyUI model pruning). Risk is
	// always RiskHigh.
	KindDeleteFiles = "delete_files"
	// KindProcedure executes a registered multi-step procedure
	// (go/internal/smith/procedures) via the runner in procedure.go —
	// autonomous-remediation Sprint 2, docs/v5-smith.md §13. Detail carries
	// only a procedure_id; the steps themselves are never in the action
	// row, only in the registry and (once running) smith_procedure_runs.
	KindProcedure = "procedure"
)

// dedupeKeyBinaryUpstreamPrefix is the DedupeKey prefix propose.go's
// proposeRebuildRunbook uses for "N commits behind upstream" runbook
// actions (KindRunbook + ":binary_upstream:" + binary name). Named here,
// not just inlined at its one construction site in propose.go, because
// procedurize.go's procedureForAction reads the same prefix to recognize
// this specific runbook shape among all the other KindRunbook actions
// (self-review closures, kernel_params, gtt_ceiling, binary_stale) — a
// second inlined copy of the string is exactly the kind of drift this
// codebase's "fixed coded data, mirrored ... deliberately duplicated
// rather than a round trip" convention (procedurize.go) is careful to
// avoid when nothing forces a round trip anyway.
const dedupeKeyBinaryUpstreamPrefix = KindRunbook + ":binary_upstream:"

// Action lifecycle statuses (docs/v5-smith.md §4.6). The legal transitions
// are legalTransitions below — every mutator CAS's through it, never a
// blind UPDATE.
const (
	StatusPending        = "pending"
	StatusApproved       = "approved"
	StatusRejected       = "rejected"
	StatusExecuting      = "executing"
	StatusDone           = "done"
	StatusDoneUnverified = "done_unverified"
	StatusFailed         = "failed"
	StatusSuperseded     = "superseded"
)

// Risk levels (docs/v5-smith.md §4.6).
const (
	RiskInfo = "info"
	RiskLow  = "low"
	RiskHigh = "high"
)

// EventActionUpdate is the SSE event published on every action status
// transition (Contract 1 amendment, docs/v5-smith.md §5).
const EventActionUpdate = "smith:action_update"

// Sentinel errors for the action model, alongside ErrStoreUnwired (smith.go)
// and ErrAlreadyRunning (findings.go).
var (
	// ErrPlacerUnwired is returned by load_config/unload_slot proposal
	// stamping and execution when Deps.Placer is nil.
	ErrPlacerUnwired = errors.New("smith: placer not wired")

	// ErrRestartUnwired is returned by restart_forge_unit execution when
	// Deps.RestartUnit is nil.
	ErrRestartUnwired = errors.New("smith: restart unit func not wired")

	// ErrUnitNotAllowed is wrapped into restart_forge_unit proposal/
	// execution errors when the target unit fails restartAllowed.
	ErrUnitNotAllowed = errors.New("smith: unit not on the restart allowlist")

	// ErrKeyNotAllowed is wrapped into settings_change proposal/execution
	// errors when the target key fails settingsKeyAllowed.
	ErrKeyNotAllowed = errors.New("smith: settings key not on the smith.* allowlist")

	// ErrDeleteUnwired is returned by delete_files execution when
	// Deps.DeleteFile is nil.
	ErrDeleteUnwired = errors.New("smith: delete file func not wired")

	// ErrPathNotAllowed is wrapped into delete_files proposal/execution
	// errors when the target path fails deleteAllowed.
	ErrPathNotAllowed = errors.New("smith: path not on the delete allowlist")

	// ErrProcedureNotFound is wrapped into procedure proposal/execution
	// errors when detail.procedure_id doesn't resolve in the procedures
	// registry.
	ErrProcedureNotFound = errors.New("smith: unknown procedure id")

	// ErrProcedureNotAutonomyEligible is wrapped into dispatchProcedure's
	// error when an action created by the standing autonomy actor
	// (autonomyActor) targets a procedure ID outside autonomyEligible.
	// Defense-in-depth (2026-08-27, idea from reviewing amd/skills'
	// rocm-doctor CLI, which bakes its own auto-applicable allowlist into the
	// tool itself rather than trusting only the caller): maybeAutoRunProcedure
	// already refuses to call Procedurize for an ineligible procedure, so
	// this should never fire in practice — it exists so a future bug in that
	// caller (a new code path that creates an autonomy-actor action some
	// other way, a refactor that drops the eligibility check) fails the run
	// outright at the actual privileged-execution boundary instead of
	// silently letting an unreviewed procedure run unattended.
	ErrProcedureNotAutonomyEligible = errors.New("smith: procedure is not on the autonomy allowlist")

	// ErrProcedureUnwired is returned by procedure execution when
	// Deps.RunStep is nil — an unwired daemon can never run a command by
	// accident.
	ErrProcedureUnwired = errors.New("smith: procedure step runner not wired")

	// ErrMaintenanceUnwired is returned when a procedure declares
	// Impact.NeedsMaintenance but Deps.Maintenance is nil — fails closed
	// rather than silently running a maintenance-requiring procedure
	// without the quiet-host guarantee.
	ErrMaintenanceUnwired = errors.New("smith: procedure needs a maintenance window but the maintenance gate is not wired")

	// ErrGitLsRemoteUnwired is returned by upstream-nightly tracking paths
	// (build_refresh's build_record_upstream_sha step, binary_versions'
	// drift probe) when Deps.GitLsRemote is nil. Tracking-enabled forks
	// fail closed rather than silently recording/reporting nothing.
	ErrGitLsRemoteUnwired = errors.New("smith: git ls-remote seam not wired")

	// ErrCheckpointPaused is returned by dispatchProcedure (never wrapped
	// into a failure) when a run pauses at a Step.Checkpoint gate —
	// executeAction treats it as "leave the action executing", not a
	// dispatch failure.
	ErrCheckpointPaused = errors.New("smith: procedure run is paused at a checkpoint")

	// ErrInvalidTransition is returned when a state-machine mutation's
	// compare-and-set affects zero rows — the action was not in the
	// expected starting state (already transitioned by a concurrent call,
	// or simply the wrong state for the requested move).
	ErrInvalidTransition = errors.New("smith: invalid action state transition")
)

// legalTransitions is the action lifecycle's documented source of truth for
// which status moves are allowed (docs/v5-smith.md §4.6). Every mutator
// enforces its OWN transition via a literal compare-and-set UPDATE
// (`WHERE id=? AND status=?`), never a read-check-write — that CAS is what
// makes double-approve and concurrent approvals correct with no
// package-level mutex (the existing s.mu/s.sweeping pair is for sweeps
// only), and per-statement literal SQL is easier to audit than routing every
// mutation through one generic table-driven executor would be. This map is
// the cross-check: isLegalTransition asserts every mutator's hand-written
// CAS agrees with it (actions_test.go), so the two can never silently drift
// apart.
var legalTransitions = map[string]map[string]bool{
	StatusPending:   {StatusApproved: true, StatusRejected: true, StatusSuperseded: true, StatusDoneUnverified: true},
	StatusApproved:  {StatusExecuting: true, StatusFailed: true},
	StatusExecuting: {StatusDone: true, StatusDoneUnverified: true, StatusFailed: true},
	// DoneUnverified -> Done is the self_review.go promotion path
	// (docs/v5-smith-experience.md §8 item 24): a re-run of the action's own
	// verify checks that now comes back clean, against a collector snapshot
	// now genuinely newer than execution, promotes the row. KindRunbook
	// actions never take this path (verifyChecksFor has no "runbook" case,
	// so the promotion function's check-ID list is always empty for them) —
	// a runbook's done_unverified remains its documented terminal state,
	// refreshed only by RecheckRunbook's own result-only update.
	StatusDoneUnverified: {StatusDone: true},
}

// isLegalTransition reports whether legalTransitions documents from→to as
// allowed.
func isLegalTransition(from, to string) bool {
	return legalTransitions[from][to]
}

// Action is one proposed-or-executed mutation (smith_actions,
// migration 0034; docs/v5-smith.md §4.6). Detail is the request; Result is
// the outcome — kept as separate JSON blobs so the audit trail never
// conflates what was asked for with what happened.
type Action struct {
	ID              int64           `json:"id"`
	InvestigationID *int64          `json:"investigation_id"`
	ConversationID  *int64          `json:"conversation_id"`
	FindingID       *int64          `json:"finding_id"`
	Kind            string          `json:"kind"`
	Title           string          `json:"title"`
	Detail          json.RawMessage `json:"detail"`
	Risk            string          `json:"risk"`
	Status          string          `json:"status"`
	SelfEvicting    bool            `json:"self_evicting"`
	Handoff         *Handoff        `json:"handoff"`
	DedupeKey       string          `json:"dedupe_key"`
	Result          *ActionResult   `json:"result"`
	CreatedBy       string          `json:"created_by"`
	ApprovedBy      *string         `json:"approved_by"`
	AuditRef        *string         `json:"audit_ref"`
	CreatedAt       int64           `json:"created_at"`
	ExecutedAt      *int64          `json:"executed_at"`
	VerifiedAt      *int64          `json:"verified_at"`
	ResolvedAt      *int64          `json:"resolved_at"`
}

// actionJSON is Action's field set under a different name, purely so
// MarshalJSON (below) can embed it without recursing into itself.
type actionJSON Action

// MarshalJSON adds a computed, never-stored Procedurizable field — Sprint
// 6's fix for a real gap: the frontend used to decide whether to show the
// "let smith fix it" button off PROCEDURIZABLE_KINDS, a third
// hand-maintained copy of procedureForActionKind's key set with nothing
// cross-checking it against the Go map, so a newly-mapped kind (or, before
// Sprint 6, the runbook/DedupeKey discriminator this same method now
// covers) would silently leave the button missing. Every JSON encoding of
// an Action — single or in a list, *Action or Action by value, this
// package's own httpapi callers — goes through this automatically; there
// is no second call site to keep in sync. Mirrors ActionCard.tsx's
// canProcedurize predicate exactly (status pending, not self-evicting, a
// procedure is mapped) rather than just "a procedure is mapped" alone, so
// the frontend can render the field directly without re-deriving the other
// two conditions itself.
func (a Action) MarshalJSON() ([]byte, error) {
	_, mapped := procedureForAction(&a)
	procedurizable := mapped && a.Status == StatusPending && !a.SelfEvicting
	return json.Marshal(struct {
		actionJSON
		Procedurizable bool `json:"procedurizable"`
	}{actionJSON(a), procedurizable})
}

// ActionResult is an executed action's outcome (execute.go). Message always
// explains the final status in operator-facing language, even on the
// done_unverified path where dispatch itself succeeded.
type ActionResult struct {
	OK      bool           `json:"ok"`
	Message string         `json:"message"`
	Error   string         `json:"error,omitempty"`
	Verify  []VerifyResult `json:"verify"`
}

// VerifyResult is one post-execution check outcome (execute.go's
// runChecksBare pass).
type VerifyResult struct {
	CheckID  string `json:"check_id"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	At       int64  `json:"at"`
}

// ActionDraft is a proposal before it has an ID — the shared input shape for
// both the auto-proposer (propose.go) and manual creation (track B's
// httpapi layer, from a later phase). Pure data; CreateAction is the only
// thing that turns one into a persisted Action.
type ActionDraft struct {
	Kind            string
	Title           string
	Risk            string
	Detail          json.RawMessage
	DedupeKey       string
	InvestigationID *int64
	ConversationID  *int64
	FindingID       *int64
	CreatedBy       string
}

// defaultActionsLimit bounds an actions list read when the caller doesn't.
const defaultActionsLimit = 200

// CreateAction validates and persists a proposal. For load_config/
// unload_slot drafts, this is also where self-eviction stamping runs
// (handoff.go's stampSelfEviction) — self_evicting/handoff/risk on the
// stored row may differ from d's inputs as a result (stampSelfEviction can
// force Risk to "high" and add a fit_plan to Detail).
func (s *Smith) CreateAction(ctx context.Context, d ActionDraft) (*Action, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	switch d.Kind {
	case KindRunbook, KindLoadConfig, KindUnloadSlot, KindRestartForgeUnit, KindSettingsChange,
		KindCatalogChange, KindDeleteFiles, KindProcedure:
	default:
		return nil, fmt.Errorf("smith: unknown action kind %q", d.Kind)
	}
	if d.Kind == KindCatalogChange {
		return nil, errors.New("smith: catalog_change actions are not yet available")
	}
	if d.Kind == KindProcedure {
		pd, err := parseDetail[procedureDetail](d.Detail)
		if err != nil {
			return nil, err
		}
		proc, ok := procedures.Get(pd.ProcedureID)
		if !ok {
			return nil, fmt.Errorf("smith: %w: %q", ErrProcedureNotFound, pd.ProcedureID)
		}
		if err := procedures.ValidateParams(proc, pd.Params); err != nil {
			return nil, fmt.Errorf("smith: %w", err)
		}
	}
	switch d.Risk {
	case RiskInfo, RiskLow, RiskHigh:
	default:
		return nil, fmt.Errorf("smith: invalid risk %q (want info|low|high)", d.Risk)
	}
	if d.CreatedBy == "" {
		return nil, errors.New("smith: action draft requires created_by")
	}
	if len(d.Detail) == 0 {
		d.Detail = json.RawMessage("{}")
	}

	var selfEvicting bool
	var handoff *Handoff
	if d.Kind == KindLoadConfig || d.Kind == KindUnloadSlot {
		var err error
		selfEvicting, handoff, err = s.stampSelfEviction(ctx, &d)
		if err != nil {
			return nil, err
		}
	}

	var handoffJSON any
	if handoff != nil {
		b, err := json.Marshal(handoff)
		if err != nil {
			return nil, fmt.Errorf("smith: marshal handoff: %w", err)
		}
		handoffJSON = string(b)
	}
	var dedupe any
	if d.DedupeKey != "" {
		dedupe = d.DedupeKey
	}

	now := s.d.Now().Unix()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`INSERT INTO smith_actions
			(investigation_id, conversation_id, finding_id, kind, title, detail, risk, status,
			 self_evicting, handoff, dedupe_key, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?)`,
		d.InvestigationID, d.ConversationID, d.FindingID, d.Kind, d.Title, string(d.Detail), d.Risk,
		boolToInt(selfEvicting), handoffJSON, dedupe, d.CreatedBy, now)
	if err != nil {
		return nil, fmt.Errorf("smith: create action: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("smith: create action: %w", err)
	}
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: d.CreatedBy, Action: "smith_action_create",
			Target: fmt.Sprintf("%d:%s", a.ID, a.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	return a, nil
}

// ListActions returns persisted actions, newest first. status filters on
// exact match ("" = all); invID filters on investigation_id when non-nil.
func (s *Smith) ListActions(ctx context.Context, status string, invID *int64, limit int) ([]Action, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	if limit <= 0 {
		limit = defaultActionsLimit
	}
	query := `SELECT ` + actionColumns + ` FROM smith_actions`
	var wheres []string
	var args []any
	if status != "" {
		wheres = append(wheres, "status = ?")
		args = append(args, status)
	}
	if invID != nil {
		wheres = append(wheres, "investigation_id = ?")
		args = append(args, *invID)
	}
	if len(wheres) > 0 {
		query += " WHERE " + strings.Join(wheres, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.d.Store.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("smith: list actions: %w", err)
	}
	defer rows.Close()

	out := []Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, fmt.Errorf("smith: scan action: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAction returns one action by ID. The returned error wraps sql.ErrNoRows
// (via %w) when id doesn't exist, so callers can errors.Is against it.
func (s *Smith) GetAction(ctx context.Context, id int64) (*Action, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	row := s.d.Store.SQL().QueryRowContext(ctx, `SELECT `+actionColumns+` FROM smith_actions WHERE id = ?`, id)
	a, err := scanAction(row)
	if err != nil {
		return nil, fmt.Errorf("smith: get action %d: %w", id, err)
	}
	return &a, nil
}

// PendingActionCount returns the count of actions currently in "pending"
// status, regardless of creator — the Console pending-tray chip's number.
func (s *Smith) PendingActionCount(ctx context.Context) (int, error) {
	if s.d.Store == nil {
		return 0, ErrStoreUnwired
	}
	var n int
	if err := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM smith_actions WHERE status = ?`, StatusPending).Scan(&n); err != nil {
		return 0, fmt.Errorf("smith: pending action count: %w", err)
	}
	return n, nil
}

// ApproveAction moves a pending action to approved (or, for KindRunbook,
// directly to done_unverified — a runbook is never executed by smith) and,
// on success for every other kind, kicks off execution in the background.
// Blocked with a *HandoffRequiredError when the action is self_evicting and
// its handoff isn't yet acknowledged — no row mutation happens on that path.
func (s *Smith) ApproveAction(ctx context.Context, id int64, actor string) (*Action, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status != StatusPending {
		return nil, ErrInvalidTransition
	}
	if a.Kind == KindRunbook {
		return s.approveRunbook(ctx, a, actor)
	}
	if a.SelfEvicting {
		h := Handoff{}
		if a.Handoff != nil {
			h = *a.Handoff
		}
		if h.State != HandoffAcknowledged && h.State != HandoffRemoteSwapped {
			return nil, &HandoffRequiredError{ActionID: a.ID, Handoff: h}
		}
	}

	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, approved_by = ? WHERE id = ? AND status = ?`,
		StatusApproved, actor, id, StatusPending)
	if err != nil {
		return nil, fmt.Errorf("smith: approve action %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrInvalidTransition
	}
	updated, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_action_approve",
			Target: fmt.Sprintf("%d:%s", updated.ID, updated.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	s.publishActionUpdate(ctx, updated, StatusApproved)

	execCtx := s.bgCtx
	if execCtx == nil {
		execCtx = context.Background()
	}
	go func() {
		s.executeAction(execCtx, id)
		s.maybeProposeResolution(execCtx, id)
	}()
	return updated, nil
}

// approveRunbook implements the pending→done_unverified short-circuit for
// KindRunbook actions: acknowledging a runbook is the whole approval — smith
// never executes one.
func (s *Smith) approveRunbook(ctx context.Context, a *Action, actor string) (*Action, error) {
	now := s.d.Now().Unix()
	result, err := json.Marshal(ActionResult{
		OK: true, Message: "manual runbook acknowledged; not executed by smith", Verify: []VerifyResult{},
	})
	if err != nil {
		return nil, fmt.Errorf("smith: marshal runbook result: %w", err)
	}
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions
		 SET status = ?, approved_by = ?, result = ?, executed_at = ?, verified_at = ?, resolved_at = ?
		 WHERE id = ? AND status = ?`,
		StatusDoneUnverified, actor, string(result), now, now, now, a.ID, StatusPending)
	if err != nil {
		return nil, fmt.Errorf("smith: approve runbook %d: %w", a.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrInvalidTransition
	}
	updated, err := s.GetAction(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_action_approve",
			Target: fmt.Sprintf("%d:%s", updated.ID, updated.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	s.publishActionUpdate(ctx, updated, StatusDoneUnverified)

	// self_review.go's proposed investigation closures (self_review_close
	// detail marker) are the one runbook shape where approval itself is the
	// human confirmation gate for closing an investigation — everywhere else
	// a runbook's done_unverified is terminal (§5.5, RecheckRunbook only
	// refreshes the result). Re-verify fresh here rather than trusting
	// whatever self_review.go saw when it proposed this: state can have
	// drifted since. finishResolution honestly leaves the investigation open
	// (with a "still failing" summary) if it has.
	if updated.InvestigationID != nil && isSelfReviewCloseDetail(updated.Detail) {
		s.approveSelfReviewClose(ctx, updated)
	}
	return updated, nil
}

// selfReviewCloseDetail is the marker RunbookDetail shape self_review.go
// stamps onto its investigation-closure proposals (self_review.go's
// proposeInvestigationClosure).
type selfReviewCloseDetail struct {
	SelfReviewClose bool `json:"self_review_close"`
}

// isSelfReviewCloseDetail reports whether a runbook's detail carries the
// self_review_close marker.
func isSelfReviewCloseDetail(detail json.RawMessage) bool {
	d, err := parseDetail[selfReviewCloseDetail](detail)
	if err != nil {
		return false
	}
	return d.SelfReviewClose
}

// approveSelfReviewClose re-verifies a's linked investigation's warn/crit
// checks fresh (a's own approval is the human confirmation; the investigation
// only actually closes if the re-check is still clean right now) and calls
// the same finishResolution proposeResolution uses, so a genuinely-resolved
// investigation closes exactly like the reactive §2.4.1 path — and a
// regressed one stays open with an honest summary instead of being
// force-closed on stale evidence. Best-effort: logged, never fails the
// approval itself (the action is already durably done_unverified by the time
// this runs).
func (s *Smith) approveSelfReviewClose(ctx context.Context, a *Action) {
	invID := *a.InvestigationID
	inv, findings, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		s.logf("approveSelfReviewClose: get investigation %d: %v", invID, err)
		return
	}
	if inv.Status != "open" {
		return // already closed by some other path — nothing to do
	}
	// Must match reviewInvestigations' propose-time gate (self_review.go):
	// narrowed to the anomaly-relevant checks, not every warn/crit finding
	// in the trail. Using the unnarrowed set here re-flags ambient warns
	// (e.g. comfyui_prune on a GTT_DRAIN_TIMEOUT investigation) that the
	// propose side had already deliberately excluded, so a proposal that
	// was correctly offered for approval could never actually close.
	ids := relevantWarnCritCheckIDs(inv.Trigger, findings)
	if len(ids) == 0 {
		s.finishResolution(ctx, a.ID, invID, inv, nil, true)
		return
	}
	recheck := s.runChecksBare(ctx, ids)
	allClean := true
	var stillFailing []string
	for _, f := range recheck {
		if f.Severity == SeverityWarn || f.Severity == SeverityCrit {
			allClean = false
			stillFailing = append(stillFailing, f.CheckID)
		}
	}
	s.finishResolution(ctx, a.ID, invID, inv, stillFailing, allClean)
}

// runbookCheckID extracts the originating check ID from a runbook action's
// detail (stamped at proposal time, §5.5). "" when absent — a runbook from
// before this field existed, or a manually-created runbook with no source
// check to re-verify.
func runbookCheckID(detail json.RawMessage) string {
	if len(detail) == 0 {
		return ""
	}
	var d struct {
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal(detail, &d); err != nil {
		return ""
	}
	return d.CheckID
}

// RecheckRunbook re-runs the source check(s) a "done — I ran it myself"
// runbook was meant to fix, then reports the outcome on the action's result
// (§5.5). For an investigation-attached runbook this reuses the §2.4.1
// resolution loop (re-run the investigation's warn/crit checks; a clean
// re-run resolves the investigation + posts a summary). A standalone runbook
// re-runs the single check stamped into its detail; one with no check at all
// reports "no check to re-verify" honestly. The action's status never moves
// — done_unverified is a runbook's terminal state; only its result (and
// verified_at) are refreshed.
func (s *Smith) RecheckRunbook(ctx context.Context, id int64, actor string) (*Action, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Kind != KindRunbook {
		return nil, errors.New("smith: re-check applies only to runbook actions")
	}
	if a.Status != StatusDoneUnverified {
		return nil, ErrInvalidTransition
	}

	inv, checkIDs, err := s.resolveRunbookRecheckTargets(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("smith: recheck runbook %d: %w", a.ID, err)
	}

	var result ActionResult
	if len(checkIDs) == 0 {
		result = ActionResult{OK: true, Message: "no check to re-verify — done", Verify: []VerifyResult{}}
		if inv != nil {
			// Nothing warn/crit on the investigation to re-check — still close
			// it out (the operator ran the runbook), mirroring
			// proposeResolution's empty-branch behavior.
			s.finishResolution(ctx, a.ID, *a.InvestigationID, inv, nil, true)
		}
	} else {
		findings := s.runChecksBare(ctx, checkIDs)
		verify := toVerifyResults(findings, s.d.Now())
		allClean := true
		var failing []string
		for _, f := range findings {
			if f.Severity == SeverityWarn || f.Severity == SeverityCrit {
				allClean = false
				failing = append(failing, f.CheckID)
			}
		}
		if allClean {
			result = ActionResult{OK: true, Message: "re-checked — " + strings.Join(checkIDs, ", ") + " clean", Verify: verify}
		} else {
			result = ActionResult{OK: false, Message: "re-checked — still failing: " + strings.Join(failing, ", "), Verify: verify}
		}
		if inv != nil {
			s.finishResolution(ctx, a.ID, *a.InvestigationID, inv, failing, allClean)
		}
	}

	// Persist the refreshed result without a status transition. The CAS on
	// verified_at is the concurrency anchor a status transition normally
	// provides (a runbook's status never moves here): a second concurrent
	// re-check reads the same pre-recheck verified_at and loses the CAS,
	// so a racing double-submit can't double-run checks or double-resolve
	// the investigation.
	prevVerified := int64(0)
	if a.VerifiedAt != nil {
		prevVerified = *a.VerifiedAt
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("smith: marshal recheck result: %w", err)
	}
	now := s.d.Now().Unix()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET result = ?, verified_at = ? WHERE id = ? AND status = ? AND verified_at = ?`,
		string(b), now, a.ID, StatusDoneUnverified, prevVerified)
	if err != nil {
		return nil, fmt.Errorf("smith: recheck runbook %d: %w", a.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrInvalidTransition
	}
	updated, err := s.GetAction(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_action_recheck",
			Target: fmt.Sprintf("%d:%s", updated.ID, updated.Kind),
			Detail: result.Message,
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	s.publishActionUpdate(ctx, updated, StatusDoneUnverified)
	return updated, nil
}

// resolveRunbookRecheckTargets figures out which check(s) re-verify a
// runbook's underlying condition — an attached investigation's warn/crit set
// (narrowed for a self-review-close runbook via relevantWarnCritCheckIDs, the
// same gate approveSelfReviewClose uses, so a re-check matches what the
// operator was actually shown when the proposal was offered), or the
// standalone check stamped into detail.check_id. Shared by RecheckRunbook
// (done_unverified) and CheckPendingRunbook (pending) — the two only differ
// in what happens to the action's own status afterward.
func (s *Smith) resolveRunbookRecheckTargets(ctx context.Context, a *Action) (inv *Investigation, checkIDs []string, err error) {
	if a.InvestigationID != nil {
		var findings []StoredFinding
		inv, findings, err = s.GetInvestigation(ctx, *a.InvestigationID)
		if err != nil {
			return nil, nil, fmt.Errorf("get investigation: %w", err)
		}
		if isSelfReviewCloseDetail(a.Detail) {
			checkIDs = relevantWarnCritCheckIDs(inv.Trigger, findings)
		} else {
			checkIDs = warnCritFindingCheckIDs(findings)
		}
		return inv, checkIDs, nil
	}
	if cid := runbookCheckID(a.Detail); cid != "" {
		return nil, []string{cid}, nil
	}
	return nil, nil, nil
}

// RunbookStillFailingError is CheckPendingRunbook's non-terminal outcome —
// the on-demand check ran fine, it just found the underlying condition still
// failing. Not really an "error" in the exec-failed sense, but the caller
// needs the failing check IDs to report back, and a pending runbook has no
// interim-result column to persist an OK:false result into the way
// done_unverified's `result` field does (RecheckRunbook's equivalent case).
type RunbookStillFailingError struct{ CheckIDs []string }

func (e *RunbookStillFailingError) Error() string {
	return "still failing: " + strings.Join(e.CheckIDs, ", ")
}

// CheckPendingRunbook re-runs a PENDING runbook's underlying check(s) on
// demand — the operator's "check now", replacing the removed self-attestation
// "done — I ran it myself" button (S7-followup smith UX sprint, 2026-08-26).
// A clean result closes the proposal as resolved via supersedeActionWithNote
// — the exact same write path self-review's periodic reviewPendingProposals
// already uses for this — so the operator never asserts done, smith verifies
// it, on-demand instead of waiting for the next sweep. A still-failing result
// leaves the action untouched (still pending) and returns
// *RunbookStillFailingError.
func (s *Smith) CheckPendingRunbook(ctx context.Context, id int64, actor string) (*Action, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Kind != KindRunbook {
		return nil, errors.New("smith: check-now applies only to runbook actions")
	}
	if a.Status != StatusPending {
		return nil, ErrInvalidTransition
	}

	inv, checkIDs, err := s.resolveRunbookRecheckTargets(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("smith: check-now runbook %d: %w", a.ID, err)
	}
	if len(checkIDs) == 0 {
		return nil, errors.New("smith: nothing to check — this runbook has no source check attached")
	}

	findings := s.runChecksBare(ctx, checkIDs)
	var failing []string
	for _, f := range findings {
		if f.Severity == SeverityWarn || f.Severity == SeverityCrit {
			failing = append(failing, f.CheckID)
		}
	}
	if len(failing) > 0 {
		return nil, &RunbookStillFailingError{CheckIDs: failing}
	}

	if inv != nil {
		s.finishResolution(ctx, a.ID, *a.InvestigationID, inv, nil, true)
	}
	if !s.supersedeActionWithNote(ctx, a.ID, "operator-requested check: underlying condition(s) clean, closing as resolved") {
		return nil, ErrInvalidTransition
	}
	updated, err := s.GetAction(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_action_check_now",
			Target: fmt.Sprintf("%d:%s", updated.ID, updated.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	return updated, nil
}

// RejectAction moves a pending action to rejected. Always the safe
// direction — no guard beyond the state-machine CAS.
func (s *Smith) RejectAction(ctx context.Context, id int64, actor string) (*Action, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	now := s.d.Now().Unix()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, resolved_at = ? WHERE id = ? AND status = ?`,
		StatusRejected, now, id, StatusPending)
	if err != nil {
		return nil, fmt.Errorf("smith: reject action %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrInvalidTransition
	}
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_action_reject",
			Target: fmt.Sprintf("%d:%s", a.ID, a.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	s.publishActionUpdate(ctx, a, StatusRejected)
	return a, nil
}

// supersedeActionWithNote CASes a pending action to superseded with a result
// message explaining why — shared by self_review.go's moot-proposal sweep
// and procedurize.go's "let smith fix it" replacement flow (Sprint 3,
// docs/v5-smith.md §13). createOrReuseProposal's own casSuperseded
// (propose.go) does the same CAS but without a result note; both these
// callers want the note visible in the action's history.
func (s *Smith) supersedeActionWithNote(ctx context.Context, id int64, message string) bool {
	result, err := json.Marshal(ActionResult{OK: true, Message: message, Verify: []VerifyResult{}})
	if err != nil {
		s.logf("supersede action %d: marshal result: %v", id, err)
		return false
	}
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, result = ?, resolved_at = ? WHERE id = ? AND status = ?`,
		StatusSuperseded, string(result), s.d.Now().Unix(), id, StatusPending)
	if err != nil {
		s.logf("supersede action %d: %v", id, err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	if a, err := s.GetAction(ctx, id); err == nil {
		s.publishActionUpdate(ctx, a, StatusSuperseded)
	}
	return true
}

// publishActionUpdate emits EventActionUpdate for a, nil-tolerant. status is
// passed explicitly rather than read off a (which may be pre- or
// post-transition depending on the caller) so every call site is unambiguous
// about which state it's announcing.
func (s *Smith) publishActionUpdate(ctx context.Context, a *Action, status string) {
	if s.d.Publisher == nil {
		return
	}
	payload := map[string]any{
		"action_id":     a.ID,
		"status":        status,
		"kind":          a.Kind,
		"risk":          a.Risk,
		"self_evicting": a.SelfEvicting,
	}
	if n, err := s.PendingActionCount(ctx); err == nil {
		payload["pending_count"] = n
	}
	s.d.Publisher.Publish(EventActionUpdate, payload)
}

// boolToInt converts a bool to the 0/1 this repo's SQLite convention uses
// for boolean columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// maybeProposeResolution is the §2.4.1 resolution-loop trigger, invoked as a
// goroutine after executeAction completes (from ApproveAction's launch site).
// It only fires for actions that (a) reached status=done (post-verify clean)
// and (b) are attached to an investigation — everything else is a no-op. The
// actual re-check + resolve + summary lives in investigations.go's
// proposeResolution so the investigation close-proposal surface is co-located
// with the investigation model.
func (s *Smith) maybeProposeResolution(ctx context.Context, id int64) {
	if s.d.Store == nil {
		return
	}
	a, err := s.GetAction(ctx, id)
	if err != nil {
		s.logf("maybeProposeResolution: get action %d: %v", id, err)
		return
	}
	if a.Status != StatusDone {
		return
	}
	if a.InvestigationID == nil {
		return
	}
	s.proposeResolution(ctx, id, *a.InvestigationID)
}

// actionColumns is the fixed column order scanAction expects.
const actionColumns = `id, investigation_id, conversation_id, finding_id, kind, title, detail, risk, status,
	self_evicting, handoff, dedupe_key, result, created_by, approved_by, audit_ref,
	created_at, executed_at, verified_at, resolved_at`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAction scans one actionColumns-ordered row.
func scanAction(rs rowScanner) (Action, error) {
	var a Action
	var invID, convID, findingID, executedAt, verifiedAt, resolvedAt sql.NullInt64
	var detail string
	var handoffCol, dedupeCol, resultCol, approvedBy, auditRef sql.NullString
	var selfEvicting int
	var createdAt int64

	if err := rs.Scan(
		&a.ID, &invID, &convID, &findingID, &a.Kind, &a.Title, &detail, &a.Risk, &a.Status,
		&selfEvicting, &handoffCol, &dedupeCol, &resultCol, &a.CreatedBy, &approvedBy, &auditRef,
		&createdAt, &executedAt, &verifiedAt, &resolvedAt,
	); err != nil {
		return Action{}, err
	}

	// Rebrand tolerance (2026-08): rows persisted before the Foundry → Forge
	// rename carry kind "restart_foundry_unit"; normalize so they still
	// dispatch against the renamed KindRestartForgeUnit.
	if a.Kind == "restart_foundry_unit" {
		a.Kind = KindRestartForgeUnit
	}

	a.Detail = json.RawMessage(detail)
	a.SelfEvicting = selfEvicting != 0
	a.CreatedAt = createdAt
	if invID.Valid {
		v := invID.Int64
		a.InvestigationID = &v
	}
	if convID.Valid {
		v := convID.Int64
		a.ConversationID = &v
	}
	if findingID.Valid {
		v := findingID.Int64
		a.FindingID = &v
	}
	if approvedBy.Valid {
		v := approvedBy.String
		a.ApprovedBy = &v
	}
	if auditRef.Valid {
		v := auditRef.String
		a.AuditRef = &v
	}
	if executedAt.Valid {
		v := executedAt.Int64
		a.ExecutedAt = &v
	}
	if verifiedAt.Valid {
		v := verifiedAt.Int64
		a.VerifiedAt = &v
	}
	if resolvedAt.Valid {
		v := resolvedAt.Int64
		a.ResolvedAt = &v
	}
	if dedupeCol.Valid {
		a.DedupeKey = dedupeCol.String
	}
	if handoffCol.Valid && handoffCol.String != "" {
		var h Handoff
		if err := json.Unmarshal([]byte(handoffCol.String), &h); err == nil {
			a.Handoff = &h
		}
	}
	if resultCol.Valid && resultCol.String != "" {
		var r ActionResult
		if err := json.Unmarshal([]byte(resultCol.String), &r); err == nil {
			a.Result = &r
		}
	}
	return a, nil
}
