// SPDX-License-Identifier: Apache-2.0

// Package procedures is the declared-step-plan registry for Smith's
// autonomous-remediation procedure engine (Sprint 2 of the "let smith fix
// it" plan — docs/v5-smith.md §13). A Procedure is CODED GO DATA, never LLM
// output — the same posture checks.go's registry documents for diagnostic
// checks ("checks are code, their results are data"). Smith's brain may
// choose WHICH registered procedure to run; it never composes a Step's
// Argv. internal/smith's procedure.go is the only caller of this package —
// procedures itself knows nothing about actions, runs, or the store, so it
// stays trivially unit-testable and can never import smith (no cycle risk).
package procedures

import (
	"fmt"
	"time"
)

// FailPolicy decides what a step-runner does when a step's command exits
// non-zero or its VerifyCheckIDs come back warn/crit.
type FailPolicy string

const (
	// FailAbort stops the run immediately; the action finalizes failed.
	FailAbort FailPolicy = "abort"
	// FailRollback runs the step's own Rollback argv, then stops the run
	// (finalizes failed) — the rollback is best-effort cleanup, not a retry.
	FailRollback FailPolicy = "rollback"
	// FailCheckpoint pauses the run for a human decision instead of
	// deciding unilaterally — used for steps where "did this actually go
	// wrong, or is a non-zero exit expected" needs a human read (e.g. a
	// git rebase reporting conflicts).
	FailCheckpoint FailPolicy = "checkpoint"
)

// Impact is a procedure's declared blast radius — never guessed at runtime,
// always read off the registry entry so the operator-facing downtime
// disclosure (Sprint 3's modal) and the maintenance-mode request Sprint 2's
// runner issues are both grounded in the same one place.
type Impact struct {
	// NeedsMaintenance, when true, is a DISCLOSURE flag: this run will hold
	// a maintenance window (internal/maintenance) at some point before it
	// finishes — what the Sprint 3 downtime modal warns the operator about.
	// It does not by itself decide WHEN the window opens; that's each
	// Step's own NeedsMaintenance (below), because a long procedure can have
	// real non-disruptive work (fetch, rebase, build) before the part that
	// actually needs the host quiet (autonomous-remediation Sprint 6 —
	// build_refresh's hours-long dual build must not hold a0 down the whole
	// time). The runner asserts these two agree: if any Step declares
	// NeedsMaintenance, the Procedure's own Impact.NeedsMaintenance must too
	// (checked once at registration via Register, so a mismatch is a
	// compile-time-adjacent registry bug, never a runtime surprise).
	NeedsMaintenance bool
	// EstDuration is an honest estimate, not a promise — Sprint 4's
	// scorecard records estimate-vs-actual so a chronically-wrong Impact is
	// visible as a bug in the registry entry, not written off as noise.
	EstDuration time.Duration
	// AffectedSlots/AffectedServices are surfaced verbatim in the
	// maintenance.EnterRequest and the (Sprint 3) downtime modal.
	AffectedSlots    []string
	AffectedServices []string
	// DaemonRestart marks a procedure that restarts forge itself
	// mid-run (e.g. build-refresh's redeploy step) — such a run must
	// persist state before that step and be resumable across the restart
	// it causes. No registered Sprint 2 procedure sets this; it exists so
	// the runner's resume path has something real to be tested against
	// ahead of Sprint 6's build-refresh capstone.
	DaemonRestart bool
}

// Step is one command in a procedure. Argv is fixed at registration time —
// there is no shell, no string interpolation, no LLM-authored argument.
// ArgvAllowed (allowlist.go) re-validates that a step about to run still
// matches its own registry entry verbatim, so even a hypothetical corrupted
// in-memory Procedure value can't smuggle a different command through.
//
// Op is the alternative to Argv (mutually exclusive — a step sets exactly
// one): a native smith-side operation (restart a unit, unload a slot,
// delete a file) rather than an external command. The set of valid Op
// values is fixed by smith's own runNativeOp switch (procedure.go) — this
// package deliberately knows nothing about what an Op does, only that a
// step has one. Introduced for Sprint 3's "let smith fix it" procedures,
// which wrap the same native Go calls (Deps.RestartUnit, Placer.Unload,
// Deps.DeleteFile) the existing atomic dispatchers already use — Argv
// templating was considered and rejected as unnecessary complexity once it
// was clear those three operations are not shell commands to begin with.
type Step struct {
	Title, Why string
	Argv       []string
	Op         string
	Cwd        string
	Timeout    time.Duration
	// Env/EnvPassthrough are StepSpec's Env/EnvPassthrough, fixed at
	// registration time exactly like Argv — see StepSpec's doc comment
	// (step.go) for the default-minimal-environment posture this exists to
	// preserve.
	Env            map[string]string
	EnvPassthrough []string
	// NeedsMaintenance, when true, means the runner must be holding a
	// maintenance window before this step executes — opened lazily on the
	// first step in the Procedure that declares it (not necessarily step 0),
	// held through every later step regardless of that step's own value,
	// and released when the run ends (completed/failed/aborted). A
	// Procedure with any Step.NeedsMaintenance=true must set its own
	// Impact.NeedsMaintenance=true too (Register asserts this).
	NeedsMaintenance bool
	// VerifyCheckIDs are re-run (via smith's runChecksBare) immediately
	// after Argv exits zero; any warn/crit finding is treated exactly like
	// a non-zero exit for OnFail purposes.
	VerifyCheckIDs []string
	// Checkpoint, when true, pauses the run for operator approval AFTER
	// this step (and its verify) completes successfully, before the next
	// step begins.
	Checkpoint bool
	OnFail     FailPolicy
	// Rollback is executed (best-effort — its own failure is logged, never
	// escalated) when OnFail is FailRollback.
	Rollback []string
}

// Param declares one operator-supplied (really: reshaped from a source
// action's own Detail — never operator free text, never LLM output) input a
// procedure's steps read by name. Allowed is deliberately a SHALLOW check —
// charset/shape only (e.g. "looks like a unit name", "is well-formed JSON")
// — never the authoritative safety check: the real allowlist stays in the
// native op handler itself (restartAllowed/deleteAllowed, already-proven
// functions smith reuses unchanged), mirroring this codebase's existing
// "checked at proposal time AND re-checked at dispatch time" convention. A
// nil Allowed accepts any non-empty value.
type Param struct {
	Name    string
	Allowed func(string) bool
	// Optional, when true, means the param may be absent (or empty) —
	// ValidateParams then skips it instead of failing the run. A value that
	// IS present is still checked against Allowed exactly like a required
	// one: optional never means unvalidated. P3smith added this for
	// fetch_model, whose dest_rel_path/sha256/config_name inputs are all
	// genuinely optional behaviors of one procedure, not three procedures.
	Optional bool
}

// Procedure is one registered, reviewed-like-code remediation plan.
type Procedure struct {
	ID     string
	Title  string
	Impact Impact
	// Preconditions names check IDs that must be non-crit before the run
	// starts. Enforced by runProcedureSteps before step 0 runs, on every
	// entry path (operator-approved dispatch AND autonomy — autonomy.go
	// previously duplicated this check itself pre-dispatch; Sprint 6 moved
	// the check here so there is exactly one enforcement point for both
	// paths). A run that fails a precondition never executes any step and
	// finalizes failed with the precondition finding in the journal.
	Preconditions []string
	// Params declares the named inputs this procedure's Op steps read from
	// the params map threaded through by the runner. Empty for a procedure
	// with no operator-supplied input (e.g. disk_usage_report).
	Params []Param
	Steps  []Step
}

// ValidateParams checks that params satisfies exactly proc's declared
// Params: every declared name present with a non-empty value passing its
// own (shallow) Allowed check, and no unknown extra keys. Called once by
// the runner before a procedure's step loop begins.
func ValidateParams(proc Procedure, params map[string]string) error {
	declared := make(map[string]bool, len(proc.Params))
	for _, p := range proc.Params {
		declared[p.Name] = true
		v, ok := params[p.Name]
		if !ok || v == "" {
			if p.Optional {
				continue
			}
			return fmt.Errorf("procedures: %s: missing required param %q", proc.ID, p.Name)
		}
		if p.Allowed != nil && !p.Allowed(v) {
			return fmt.Errorf("procedures: %s: param %q has a disallowed value", proc.ID, p.Name)
		}
	}
	for k := range params {
		if !declared[k] {
			return fmt.Errorf("procedures: %s: unknown param %q", proc.ID, k)
		}
	}
	return nil
}

// registry is package-level, coded data — same shape as checks.go's
// var registry = []Check{...}. Order is display order (registration order).
var registry []Procedure

func init() {
	Register(diskUsageReportProcedure)
	Register(restartDownUnitProcedure)
	Register(reconcileOrphanedSlotProcedure)
	Register(comfyUIPruneProcedure)
	Register(restoreUnitLauncherProcedure)
}

// Register adds p to the registry. Panics on a duplicate ID — a
// programming error caught at package init, same posture as a duplicate
// route registration panicking in net/http. Exported so package smith's
// tests can register throwaway fixture procedures (e.g. to exercise
// checkpoint/rollback/abort without inventing a real destructive command)
// without this package growing a runtime "add a procedure" API for
// anything else — every procedure that ships to production is still a
// compiled var in this package, reviewed like any other code.
func Register(p Procedure) {
	if _, exists := Get(p.ID); exists {
		panic("procedures: duplicate procedure id " + p.ID)
	}
	for _, s := range p.Steps {
		if s.NeedsMaintenance && !p.Impact.NeedsMaintenance {
			panic("procedures: " + p.ID + ": a step declares NeedsMaintenance but Impact.NeedsMaintenance is false — the disclosure and the runner's gating must agree")
		}
	}
	registry = append(registry, p)
}

// Get returns the registered procedure with the given ID, or ok=false.
func Get(id string) (Procedure, bool) {
	for _, p := range registry {
		if p.ID == id {
			return p, true
		}
	}
	return Procedure{}, false
}

// All returns every registered procedure, in display order.
func All() []Procedure {
	out := make([]Procedure, len(registry))
	copy(out, registry)
	return out
}
