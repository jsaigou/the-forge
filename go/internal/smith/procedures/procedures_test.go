// SPDX-License-Identifier: Apache-2.0

package procedures

import "testing"

func TestGet_KnownAndUnknown(t *testing.T) {
	p, ok := Get("disk_usage_report")
	if !ok {
		t.Fatal("expected disk_usage_report to be registered")
	}
	if p.Title == "" || len(p.Steps) == 0 {
		t.Fatal("disk_usage_report loaded with empty Title/Steps")
	}
	if _, ok := Get("does_not_exist"); ok {
		t.Fatal("expected unknown procedure id to report ok=false")
	}
}

func TestAll_ReturnsACopy(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("expected at least one registered procedure")
	}
	origID := all[0].ID
	all[0].ID = "mutated"
	fresh, ok := Get(origID)
	if !ok || fresh.ID != origID {
		t.Fatal("All() must return a copy, not the live registry slice — mutating it corrupted the registry")
	}
}

func TestArgvAllowed(t *testing.T) {
	p, _ := Get("disk_usage_report")
	if !ArgvAllowed(p.ID, p.Steps[0].Argv) {
		t.Fatal("expected the procedure's own first-step argv to be allowed")
	}
	if ArgvAllowed(p.ID, []string{"rm", "-rf", "/"}) {
		t.Fatal("expected an unregistered argv to be refused")
	}
	if ArgvAllowed("does_not_exist", p.Steps[0].Argv) {
		t.Fatal("expected an unknown procedure id to refuse everything")
	}
}

func TestRegister_PanicsOnDuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected Register to panic on a duplicate id")
		}
	}()
	Register(Procedure{ID: "disk_usage_report"})
}

func TestValidateParams(t *testing.T) {
	proc := Procedure{
		ID: "test_validate_params",
		Params: []Param{
			{Name: "unit", Allowed: shallowTokenAllowed},
		},
	}
	cases := []struct {
		name    string
		params  map[string]string
		wantErr bool
	}{
		{"valid", map[string]string{"unit": "forge-stt"}, false},
		{"missing", map[string]string{}, true},
		{"empty value", map[string]string{"unit": ""}, true},
		{"disallowed shape", map[string]string{"unit": "forge stt; rm -rf /"}, true},
		{"unknown extra key", map[string]string{"unit": "forge-stt", "extra": "x"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateParams(proc, c.params)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateParams(%v) err = %v, wantErr %v", c.params, err, c.wantErr)
			}
		})
	}
}

func TestValidateParams_NoDeclaredParams(t *testing.T) {
	proc, _ := Get("disk_usage_report")
	if err := ValidateParams(proc, nil); err != nil {
		t.Errorf("expected nil params to validate cleanly against a procedure with no declared Params, got %v", err)
	}
	if err := ValidateParams(proc, map[string]string{"unexpected": "x"}); err == nil {
		t.Error("expected an unknown param key to be rejected even when the procedure declares none")
	}
}

func TestShallowTokenAllowed(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"forge-stt", true},
		{"headroom@deepseek", true},
		{"forge-compress@local", true},
		{"a1", true},
		{"", false},
		{"forge stt", false},
		{"forge;rm -rf /", false},
		{"../etc/passwd", false},
	}
	for _, c := range cases {
		if got := shallowTokenAllowed(c.v); got != c.want {
			t.Errorf("shallowTokenAllowed(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestJSONArrayParamAllowed(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{`[{"path":"/x"}]`, true},
		{`  [{"path":"/x"}]  `, true},
		{`[]`, true},
		{`{"path":"/x"}`, false},
		{`not json`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := jsonArrayParamAllowed(c.v); got != c.want {
			t.Errorf("jsonArrayParamAllowed(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestRestartDownUnitProcedure_Registered(t *testing.T) {
	p, ok := Get("restart_down_unit")
	if !ok {
		t.Fatal("expected restart_down_unit to be registered")
	}
	if len(p.Steps) != 1 || p.Steps[0].Op != "restart_unit" {
		t.Fatalf("restart_down_unit steps = %+v, want one restart_unit op step", p.Steps)
	}
	if len(p.Params) != 1 || p.Params[0].Name != "unit" {
		t.Fatalf("restart_down_unit params = %+v, want one 'unit' param", p.Params)
	}
}

func TestReconcileOrphanedSlotProcedure_Registered(t *testing.T) {
	p, ok := Get("reconcile_orphaned_slot")
	if !ok {
		t.Fatal("expected reconcile_orphaned_slot to be registered")
	}
	if len(p.Steps) != 1 || p.Steps[0].Op != "unload_slot" {
		t.Fatalf("reconcile_orphaned_slot steps = %+v, want one unload_slot op step", p.Steps)
	}
	if len(p.Params) != 1 || p.Params[0].Name != "slot" {
		t.Fatalf("reconcile_orphaned_slot params = %+v, want one 'slot' param", p.Params)
	}
}

func TestComfyUIPruneProcedure_Registered(t *testing.T) {
	p, ok := Get("comfyui_prune")
	if !ok {
		t.Fatal("expected comfyui_prune to be registered")
	}
	if len(p.Steps) != 1 || p.Steps[0].Op != "delete_comfyui_files" {
		t.Fatalf("comfyui_prune steps = %+v, want one delete_comfyui_files op step", p.Steps)
	}
	if len(p.Params) != 1 || p.Params[0].Name != "files_json" {
		t.Fatalf("comfyui_prune params = %+v, want one 'files_json' param", p.Params)
	}
}

// TestBuildRefreshProcedure_Registered pins Sprint 6's registry shape: 12
// steps, exactly two Checkpoint:true steps (the promote decision and the
// pre-cleanup pause), exactly one FailCheckpoint step (the rebase-conflict
// judgment point), one declared Param ("binary"), and disk_space as a
// Precondition.
func TestBuildRefreshProcedure_Registered(t *testing.T) {
	p, ok := Get("build_refresh")
	if !ok {
		t.Fatal("expected build_refresh to be registered")
	}
	if len(p.Steps) != 13 {
		t.Fatalf("build_refresh has %d steps, want 13 (P3smith added build_record_upstream_sha)", len(p.Steps))
	}
	var checkpoints, failCheckpoints, needsMaint int
	for _, s := range p.Steps {
		if s.Checkpoint {
			checkpoints++
		}
		if s.OnFail == FailCheckpoint {
			failCheckpoints++
		}
		if s.NeedsMaintenance {
			needsMaint++
		}
	}
	if checkpoints != 2 {
		t.Errorf("checkpoints = %d, want 2 (promote decision + pre-cleanup pause)", checkpoints)
	}
	if failCheckpoints != 1 {
		t.Errorf("FailCheckpoint steps = %d, want 1 (the rebase step)", failCheckpoints)
	}
	if needsMaint != 1 {
		t.Errorf("NeedsMaintenance steps = %d, want 1 (the reliability test)", needsMaint)
	}
	if !p.Impact.NeedsMaintenance {
		t.Error("Impact.NeedsMaintenance = false, want true (must agree with the step that declares it)")
	}
	if p.Impact.DaemonRestart {
		t.Error("Impact.DaemonRestart = true, want false — refreshing a llama.cpp build never restarts forge itself")
	}
	if len(p.Params) != 1 || p.Params[0].Name != "binary" {
		t.Fatalf("build_refresh params = %+v, want one 'binary' param", p.Params)
	}
	if len(p.Preconditions) != 1 || p.Preconditions[0] != "disk_space" {
		t.Fatalf("build_refresh preconditions = %+v, want [disk_space]", p.Preconditions)
	}
}

// TestBuildRefreshProcedure_RebaseStepIsBeforePrecheck confirms step
// ordering: precheck (index 2) must run before rebase (index 3), and the
// rebase step's Op must be build_git_rebase with OnFail FailCheckpoint —
// pins the exact step this sprint's "conflict pauses, doesn't fail" design
// depends on, in case a future edit reorders the Steps slice.
func TestBuildRefreshProcedure_RebaseStepIsBeforePrecheck(t *testing.T) {
	p, ok := Get("build_refresh")
	if !ok {
		t.Fatal("expected build_refresh to be registered")
	}
	if p.Steps[2].Op != "build_git_precheck" {
		t.Fatalf("step 2 op = %q, want build_git_precheck", p.Steps[2].Op)
	}
	if p.Steps[3].Op != "build_git_rebase" || p.Steps[3].OnFail != FailCheckpoint {
		t.Fatalf("step 3 = %+v, want build_git_rebase with OnFail FailCheckpoint", p.Steps[3])
	}
}
