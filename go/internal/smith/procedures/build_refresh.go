// SPDX-License-Identifier: Apache-2.0

package procedures

import "time"

// buildRefreshProcedure is the autonomous-remediation Sprint 6 capstone
// (docs/v5-smith.md §13.4) — the first procedure that makes a genuinely
// previously-unexecutable Smith suggestion executable, rather than
// wrapping an operation Smith already performed atomically. It implements
// go/internal/smith/kb/runbooks/build-refresh.md end to end: fetch,
// precheck, rebase, dual-backend configure+build, binary verification,
// SELinux relabel, a real reliability test under a held maintenance
// window, a perf measurement, catalog promotion, and cleanup of the
// retired build.
//
// Every step is Op-based (internal/smith's runNativeOp, build_refresh_ops.go)
// rather than fixed Argv — the per-fork paths, remotes, and cmake flags
// this procedure needs come from the reviewed fork registry
// (smith.build_refresh.forks settings, migration 0061 seeds the reviewed
// entries; build_refresh_forks.go), composed into argv by the op handler
// itself at dispatch time, never templated into a Step. The one declared Param,
// "binary", names a smith.binaries.tracked entry (reshaped from the source
// binary_upstream runbook's own Detail by procedurize.go — never operator
// free text) and is deliberately the ONLY input: every other fact (source
// tree, backends to rebuild, cmake flags, which config to canary) is
// resolved fresh from either the live tracked-binary setting or the live
// catalog, never cached from proposal time.
//
// Two mandatory checkpoints, matching the two genuine judgment points a
// human — not Smith — must make (build-refresh.md's own framing): a git
// rebase conflict (OnFail: FailCheckpoint on the rebase step itself,
// pausing on FAILURE) and the promote/deploy decision (Checkpoint: true on
// the perf-measurement step, pausing on SUCCESS, before any catalog write
// happens). A second Checkpoint after promotion, before the irreversible
// cleanup step, is a third pause — deliberately not counted as a second
// "genuine judgment point" since by then the decision has already been
// made; it exists because deleting a build's on-disk directory can't be
// undone and there is no reason not to let a human glance at the
// promotion's own outcome first.
//
// DaemonRestart is false: build-refresh.md's §8 (deploying forge
// itself) is not part of refreshing a llama.cpp BUILD — the daemon's own
// binary never changes here, so restarting it would just be outage risk
// for zero change. SIGHUP (an in-process config reload, not a re-exec)
// isn't needed either — every mutation here is a store-backed catalog
// write, already live the moment UpdateConfig commits, per the store-
// backed config design (CLAUDE.md's TOML-decommission note).
var buildRefreshProcedure = Procedure{
	ID:    "build_refresh",
	Title: "Refresh a tracked llama.cpp build",
	Impact: Impact{
		NeedsMaintenance: true,
		// A realistic full wall-clock estimate for a dual-backend fork on
		// this hardware (32 cores) — the fetch/rebase/configure/build/
		// verify/relabel steps run BEFORE any maintenance window opens
		// (step 8 is the first to declare NeedsMaintenance), so this
		// number is disclosure for the operator, not just window sizing;
		// an oversized requested window is harmless (runProcedureSteps
		// always Exit()s explicitly on completion, never waits out the
		// TTL), so it errs generous rather than tight.
		EstDuration:      90 * time.Minute,
		AffectedServices: []string{"a0"},
	},
	// disk_space must be non-crit before a multi-gigabyte dual build even
	// starts — Sprint 6's first real Preconditions enforcement
	// (runProcedureSteps, procedure.go).
	Preconditions: []string{"disk_space"},
	Params:        []Param{{Name: "binary", Allowed: binaryNameAllowed}},
	Steps: []Step{
		{
			Title:          "Record the currently installed version",
			Why:            "the 'before' fact the whole run — and the operator's promote decision — is judged against.",
			Op:             "build_record_installed",
			VerifyCheckIDs: nil,
			OnFail:         FailAbort,
		},
		{
			Title:  "Fetch upstream",
			Why:    "a fork is only valuable for its own patches on top of CURRENT upstream — a stale base compounds bugs (build-refresh.md's 2026-08-16 device-lost incident).",
			Op:     "build_git_fetch",
			OnFail: FailAbort,
		},
		{
			Title:  "Confirm the tree is safe to rebase",
			Why:    "a dirty working tree or an already-in-progress rebase must never be touched automatically — this is the step that protects hand-made, uncommitted patches from being silently rebased over.",
			Op:     "build_git_precheck",
			OnFail: FailAbort,
		},
		{
			Title: "Rebase onto upstream",
			Why:   "pulls in upstream bug fixes and keeps the fork's own patches current — conflicts are EXPECTED here (build-refresh.md §1); resolving one requires a human read, not an automated guess.",
			Op:    "build_git_rebase",
			// Judgment point 1: a rebase conflict pauses the run instead
			// of failing it outright — build-refresh_ops.go's doc comment
			// on opBuildGitRebase explains why re-running this same step
			// after a human resolves the conflict outside Smith (git
			// mergetool + `rebase --continue`) is safe and idempotent.
			OnFail: FailCheckpoint,
		},
		{
			Title: "Configure the build(s)",
			Why:   "fresh build dirs per backend, never overwriting the live build — build-refresh.md §2's dual-build policy, discovered fresh from the catalog (never a hardcoded single backend) so a source tree backing more than one live build (found live on ForgeHost this sprint) gets every real backend refreshed, not just one.",
			Op:    "build_cmake_configure",
			// A dual-backend fork's whole configure pass is bounded by
			// buildRefreshConfigureTimeout per backend inside the op
			// itself — Step.Timeout is unused for Op-based steps (the
			// engine only applies it on the Argv path), so the real
			// timeout lives as reviewed Go constants next to the op.
			OnFail: FailAbort,
		},
		{
			Title:  "Build",
			Why:    "compiles each configured backend.",
			Op:     "build_cmake_build",
			OnFail: FailAbort,
		},
		{
			Title:  "Verify the new binary",
			Why:    "confirms the new build actually IS new (--version), carries the $ORIGIN RPATH literal (build-refresh.md's documented deploy-outage trap), and resolves its ROCm libs from the intended install via a clean-env ldd (the 7.13->7.15 silent-nothing trap) — three real historical outages, all re-checked here before this binary goes anywhere near production.",
			Op:     "build_verify_binary",
			OnFail: FailAbort,
		},
		{
			Title:  "Re-apply the SELinux label",
			Why:    "required after any llama-server rebuild — a fresh binary defaults to the wrong context and systemd refuses to exec it.",
			Op:     "build_relabel",
			OnFail: FailAbort,
		},
		{
			Title: "Record the built-from upstream revision",
			Why:   "when the fork has upstream-nightly tracking enabled, records the remote HEAD this run built from (git ls-remote, read-only) so binary_versions' drift mode compares future upstreams against what THIS build actually contains — a no-op for untracked forks.",
			Op:    "build_record_upstream_sha",
			// Runs BEFORE the maintenance-gated reliability test so the
			// record exists even if a bad build later fails the run — a
			// failed candidate still tells the truth about which upstream
			// it was based on.
			OnFail: FailAbort,
		},
		{
			Title: "Reliability test",
			Why:   "the exact giant-cold-prefill scenario that caused the 2026-08-16 device-lost incident, plus an unload/reload cycle and a journal scan for the documented failure signatures — a build is only trusted after it survives what actually broke the last one.",
			Op:    "build_reliability_test",
			// The one step that touches live traffic — the maintenance
			// window opens here (lazily, per-step; see registry.go's
			// Step.NeedsMaintenance doc comment), not at step 0.
			NeedsMaintenance: true,
			OnFail:           FailAbort,
		},
		{
			Title: "Measure performance",
			Why:   "records the candidate's real decode t/s as evidence for the promote decision.",
			Op:    "build_perf_measure",
			// Judgment point 2: pauses AFTER a successful reliability test
			// and perf measurement, before any catalog write — the
			// promote/deploy decision, with every step's evidence already
			// in the run journal for the operator to read.
			Checkpoint: true,
			OnFail:     FailAbort,
		},
		{
			Title: "Promote to every remaining config",
			Why:   "repoints every other config still on the old build to the now-vetted candidate — reloading only what's currently resident, reverting everything this step touched if any one repoint fails.",
			Op:    "build_catalog_promote",
			// A third pause before the one irreversible step (deleting the
			// old build's directory) — not a second genuine judgment
			// point (the decision was already made at the prior
			// checkpoint), just a last look at promotion's own real
			// outcome before cleanup.
			Checkpoint: true,
			OnFail:     FailAbort,
		},
		{
			Title:  "Clean up the old build",
			Why:    "confirms nothing still references the retired build, then removes its catalog row and on-disk directory — irreversible, which is exactly why it's last and gated behind its own checkpoint.",
			Op:     "build_cleanup_old",
			OnFail: FailAbort,
		},
	},
}

func init() {
	Register(buildRefreshProcedure)
}
