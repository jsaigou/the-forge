// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"strings"
	"testing"
)

func TestSpecialistPoolCheck(t *testing.T) {
	ctx := context.Background()
	if f := runSpecialistPool(ctx, &CheckEnv{}); f.Severity != SeverityInfo || f.Evidence["skipped"] == nil {
		t.Errorf("no pool wired must be a skipped (info) finding, got %+v", f)
	}
	ok := runSpecialistPool(ctx, &CheckEnv{SpecialistProblems: func(context.Context) []SpecialistProblem { return nil }})
	if ok.Severity != SeverityOK || ok.SummaryKey != "checks.specialist_pool.ok" {
		t.Errorf("healthy pool: %+v", ok)
	}
	one := runSpecialistPool(ctx, &CheckEnv{SpecialistProblems: func(context.Context) []SpecialistProblem {
		return []SpecialistProblem{{Slot: "s6", Occupant: "kokoro", State: "failed", Err: "exited 2", Attempts: 3}}
	}})
	if one.Severity != SeverityWarn || one.SummaryKey != "checks.specialist_pool.down" || !strings.Contains(one.Summary, `"kokoro" in s6 is failed: exited 2`) || one.Params["error"] != ": exited 2" {
		t.Errorf("one problem: %+v", one)
	}
	two := runSpecialistPool(ctx, &CheckEnv{SpecialistProblems: func(context.Context) []SpecialistProblem {
		return []SpecialistProblem{{Slot: "s6", Occupant: "kokoro", State: "failed"}, {Slot: "s2", Occupant: "asr", State: "starting"}}
	}})
	if two.SummaryKey != "checks.specialist_pool.down_more" || two.Params["extra"] != 1 || two.Params["error"] != "" {
		t.Errorf("two problems: %+v", two)
	}
	// The anomaly clears against this check and no other.
	if got := anomalyRelevantChecks["SPECIALIST_FAILED"]; len(got) != 1 || got[0] != "specialist_pool" {
		t.Errorf("SPECIALIST_FAILED relevant checks = %v", got)
	}
}
