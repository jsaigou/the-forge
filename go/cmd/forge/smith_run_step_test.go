// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/procedures"
)

// TestSmithRunStep_TimeoutIsEnforced pins the Sprint 6 fix: three separate
// doc comments across this codebase used to claim RunStep was "bounded by
// spec.Timeout", but nothing ever applied it — a hung step (a stuck cmake
// build, a network-blocked git fetch) would run forever while holding
// whatever maintenance window its procedure had opened. `sleep 5` with a
// 50ms timeout must return promptly with a timeout-shaped error, not hang
// for anywhere near 5s.
func TestSmithRunStep_TimeoutIsEnforced(t *testing.T) {
	start := time.Now()
	_, err := smithRunStep(context.Background(), procedures.StepSpec{
		Argv:    []string{"sleep", "5"},
		Timeout: 50 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to mention a timeout", err.Error())
	}
	if elapsed > 3*time.Second {
		t.Errorf("smithRunStep took %s to return — the timeout was not actually enforced", elapsed)
	}
}

// TestSmithRunStep_NoTimeoutRunsToCompletion confirms spec.Timeout == 0
// (the default before Sprint 6, and still used by every fixed-duration
// step whose Timeout is populated by runProcedureSteps' own fallback) is
// unaffected — a fast step with no deadline set still just runs.
func TestSmithRunStep_NoTimeoutRunsToCompletion(t *testing.T) {
	res, err := smithRunStep(context.Background(), procedures.StepSpec{Argv: []string{"true"}})
	if err != nil {
		t.Fatalf("smithRunStep: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestSmithRunStep_EnvIsScrubbed pins the Sprint 6 fix: a step used to
// inherit forge's ENTIRE environment (including secrets such as
// FORGE_RECOVERY_CODE_PEPPER) with its stdout/stderr then captured and
// persisted into the run journal. A step must see only
// smithRunStepEnvBase's fixed names plus whatever it explicitly declares —
// never an ambient secret-shaped variable that happens to be set in
// forge's own process environment.
func TestSmithRunStep_EnvIsScrubbed(t *testing.T) {
	t.Setenv("FORGE_TEST_SECRET_TOKEN", "super-secret-value")
	t.Setenv("PATH", os.Getenv("PATH")) // keep PATH resolvable for `env`

	res, err := smithRunStep(context.Background(), procedures.StepSpec{Argv: []string{"env"}})
	if err != nil {
		t.Fatalf("smithRunStep: %v", err)
	}
	if strings.Contains(res.Stdout, "FORGE_TEST_SECRET_TOKEN") {
		t.Errorf("child environment leaked an ambient var not on the base/passthrough/Env list: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "PATH=") {
		t.Errorf("child environment missing PATH from the base set: %q", res.Stdout)
	}
}

// TestSmithRunStep_EnvPassthroughAndFixedEnv confirms a step CAN see a
// named var it explicitly asks to inherit (EnvPassthrough), and that a
// fixed Env value always wins over whatever the parent process has set for
// the same name — the two intentional escape hatches from the minimal
// base, both fixed registry data rather than anything operator-supplied.
func TestSmithRunStep_EnvPassthroughAndFixedEnv(t *testing.T) {
	t.Setenv("FORGE_TEST_ROCM_PATH", "/opt/forge/rocm-therock-10.1")

	res, err := smithRunStep(context.Background(), procedures.StepSpec{
		Argv:           []string{"env"},
		EnvPassthrough: []string{"FORGE_TEST_ROCM_PATH"},
		Env:            map[string]string{"FORGE_TEST_ROCM_PATH": "/opt/forge/rocm-therock-99.9"},
	})
	if err != nil {
		t.Fatalf("smithRunStep: %v", err)
	}
	if !strings.Contains(res.Stdout, "FORGE_TEST_ROCM_PATH=/opt/forge/rocm-therock-99.9") {
		t.Errorf("stdout = %q, want the fixed Env value to win over the passed-through one", res.Stdout)
	}
	if strings.Count(res.Stdout, "FORGE_TEST_ROCM_PATH=") != 1 {
		t.Errorf("stdout = %q, want exactly one FORGE_TEST_ROCM_PATH entry, not a duplicate", res.Stdout)
	}
}
