// SPDX-License-Identifier: Apache-2.0

package procedures

import "time"

// StepSpec is what smith.Deps.RunStep receives to execute one step. Argv is
// fixed argv — no shell, ever (see registry.go's Step doc comment).
//
// Env/EnvPassthrough (autonomous-remediation Sprint 6, added for
// build_refresh's multi-minute cmake steps): production wiring
// (forge's smithRunStep) does NOT hand the child the daemon's full
// environment by default — only a minimal base (PATH/HOME/LANG/TERM), plus
// whichever named vars EnvPassthrough asks to inherit from the daemon's own
// environment (e.g. "ROCM_PATH" so an operator-configured ROCm install is
// visible to a build without smith ever reading or logging its value), plus
// Env's literal fixed key/values (registry data, never operator/LLM text).
// A step that sets neither gets exactly the minimal base — this is the
// default for every pre-Sprint-6 procedure, which never declared either
// field and must not gain env exposure by accident.
type StepSpec struct {
	Argv           []string
	Cwd            string
	Timeout        time.Duration
	Env            map[string]string
	EnvPassthrough []string
}

// StepResult is one step's real outcome. ExitCode 0 with a nil error is the
// only success shape; production wiring (forge's smithRunStep) returns a
// non-nil error whenever ExitCode != 0, so callers never have to check both.
//
// CheckpointNote (autonomous-remediation Sprint 6, added after the first
// live build_refresh run): when non-empty on a step that declares
// Checkpoint, it supersedes the engine's generic "step complete — approve
// to continue" pause note. The pause exists so a HUMAN makes the judgment
// call with the run's evidence in front of them; an op that knows what the
// next step will actually change (build_refresh's promote repoints a
// specific, enumerable set of configs) owes the operator that enumeration
// at decision time, not after the fact.
type StepResult struct {
	Stdout         string
	Stderr         string
	ExitCode       int
	Duration       time.Duration
	CheckpointNote string
}
