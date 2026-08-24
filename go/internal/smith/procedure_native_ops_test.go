// SPDX-License-Identifier: Apache-2.0

package smith

// procedure_native_ops_test.go — Sprint 3 (autonomous-remediation,
// docs/v5-smith.md §13): runNativeOp's three cases (restart_unit,
// unload_slot, delete_comfyui_files), exercised end to end through the real
// registered restart_down_unit/reconcile_orphaned_slot/comfyui_prune
// procedures — the same dispatchProcedure/executeAction path
// TestDispatchProcedure_TwoStepSuccess (procedure_test.go) already proves
// for Argv steps.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
)

func TestDispatchProcedure_RestartDownUnit_Success(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	var restarted []string
	s := New(Deps{
		Store: db, Cfg: func() *config.Config { return &config.Config{} },
		RestartUnit: func(_ context.Context, unit string) error { restarted = append(restarted, unit); return nil },
		Source:      buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	id := seedApproved(t, s, KindProcedure, RiskLow, mustJSON(t, procedureDetail{
		ProcedureID: "restart_down_unit", Params: map[string]string{"unit": "forge-stt"},
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusDone {
		t.Fatalf("status = %s, want done (result=%+v)", a.Status, a.Result)
	}
	if len(restarted) != 1 || restarted[0] != "forge-stt" {
		t.Fatalf("restarted = %v, want [forge-stt]", restarted)
	}
}

func TestDispatchProcedure_RestartDownUnit_UnitNotAllowed_FailsClosed(t *testing.T) {
	db := openDB(t)
	var restarted []string
	s := New(Deps{
		Store: db, Cfg: func() *config.Config { return &config.Config{} },
		RestartUnit: func(_ context.Context, unit string) error { restarted = append(restarted, unit); return nil },
		Source:      buildSnapshotAt(time.Now()), Logf: func(string, ...any) {},
	})
	// forge-daemon is explicitly refused by restartAllowed (execute.go) —
	// runNativeOp must re-check the exact same allowlist a bare
	// restart_forge_unit action would, not just trust the param.
	id := seedApproved(t, s, KindProcedure, RiskLow, mustJSON(t, procedureDetail{
		ProcedureID: "restart_down_unit", Params: map[string]string{"unit": "forge-daemon"},
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", a.Status)
	}
	if len(restarted) != 0 {
		t.Error("RestartUnit must never be called when the unit fails restartAllowed")
	}
}

func TestDispatchProcedure_ReconcileOrphanedSlot_Success(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	placer := &stubPlacer{}
	s := New(Deps{
		Store: db, Placer: placer,
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	id := seedApproved(t, s, KindProcedure, RiskLow, mustJSON(t, procedureDetail{
		ProcedureID: "reconcile_orphaned_slot", Params: map[string]string{"slot": "a3"},
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusDone {
		t.Fatalf("status = %s, want done (result=%+v)", a.Status, a.Result)
	}
	if len(placer.unloads) != 1 || placer.unloads[0] != "a3" {
		t.Fatalf("unloads = %v, want [a3]", placer.unloads)
	}
}

func TestDispatchProcedure_ReconcileOrphanedSlot_PlacerUnwired_FailsClosed(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Source: buildSnapshotAt(time.Now()), Logf: func(string, ...any) {}})
	id := seedApproved(t, s, KindProcedure, RiskLow, mustJSON(t, procedureDetail{
		ProcedureID: "reconcile_orphaned_slot", Params: map[string]string{"slot": "a3"},
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed (Placer not wired)", a.Status)
	}
}

func TestDispatchProcedure_ComfyUIPrune_Success(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	root := t.TempDir()
	f := filepath.Join(root, "checkpoints", "a.safetensors")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	s := New(Deps{
		Store: db, Settings: db.Settings(),
		DeleteFile: func(_ context.Context, path string) error {
			deleted = append(deleted, path)
			return os.Remove(path)
		},
		Source: buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	if err := db.Settings().Set(context.Background(), SettingComfyUIModelRoots, mustJSON(t, []string{root})); err != nil {
		t.Fatal(err)
	}
	filesJSON, err := json.Marshal([]deleteFileEntry{{Path: f, FolderType: "checkpoints", SizeBytes: 1}})
	if err != nil {
		t.Fatal(err)
	}
	id := seedApproved(t, s, KindProcedure, RiskLow, mustJSON(t, procedureDetail{
		ProcedureID: "comfyui_prune", Params: map[string]string{"files_json": string(filesJSON)},
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusDone {
		t.Fatalf("status = %s, want done (result=%+v)", a.Status, a.Result)
	}
	if len(deleted) != 1 || deleted[0] != f {
		t.Fatalf("deleted = %v, want [%s]", deleted, f)
	}
}

func TestDispatchProcedure_ComfyUIPrune_PathNotAllowed_FailsClosed(t *testing.T) {
	db := openDB(t)
	outside := t.TempDir()
	f := filepath.Join(outside, "not_a_configured_root.safetensors")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	s := New(Deps{
		Store: db, Settings: db.Settings(), Logf: func(string, ...any) {},
		DeleteFile: func(_ context.Context, path string) error { deleted = append(deleted, path); return nil },
		Source:     buildSnapshotAt(time.Now()),
	})
	// smith.comfyui.model_roots is left unset — f is not under any
	// configured root, so the op handler's deleteAllowed re-check must
	// refuse it exactly like dispatchDeleteFiles would.
	filesJSON, err := json.Marshal([]deleteFileEntry{{Path: f, FolderType: "checkpoints", SizeBytes: 1}})
	if err != nil {
		t.Fatal(err)
	}
	id := seedApproved(t, s, KindProcedure, RiskLow, mustJSON(t, procedureDetail{
		ProcedureID: "comfyui_prune", Params: map[string]string{"files_json": string(filesJSON)},
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", a.Status)
	}
	if len(deleted) != 0 {
		t.Error("DeleteFile must never be called when re-validation fails")
	}
}

// TestDispatchProcedure_MissingRequiredParam_FailsClosed proves
// runProcedureSteps' own ValidateParams call (not just CreateAction's, which
// TestCreateAction_ProcedureRejectsInvalidParams already covers) fails
// closed — same defense-in-depth posture as
// TestDispatchProcedure_UnknownProcedureID_Fails, bypassing CreateAction via
// a direct insert to simulate a row that predates a param becoming
// required, or a registry entry that changed between proposal and dispatch.
func TestDispatchProcedure_MissingRequiredParam_FailsClosed(t *testing.T) {
	db := openDB(t)
	var restarted []string
	s := New(Deps{
		Store: db, Cfg: func() *config.Config { return &config.Config{} },
		RestartUnit: func(_ context.Context, unit string) error { restarted = append(restarted, unit); return nil },
		Source:      buildSnapshotAt(time.Now()), Logf: func(string, ...any) {},
	})
	now := s.d.Now().Unix()
	res, err := db.SQL().Exec(
		`INSERT INTO smith_actions (kind, title, detail, risk, status, created_by, created_at)
		 VALUES (?, 't', ?, ?, ?, 'op', ?)`,
		KindProcedure, string(mustJSON(t, procedureDetail{ProcedureID: "restart_down_unit"})), RiskLow, StatusApproved, now)
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
		t.Fatalf("status = %s, want failed (missing required param)", a.Status)
	}
	if len(restarted) != 0 {
		t.Error("RestartUnit must never be called when required params are missing")
	}
}

func TestCreateAction_ProcedureRejectsInvalidParams(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	_, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindProcedure, Title: "test", Risk: RiskLow, CreatedBy: "op",
		Detail: mustJSON(t, procedureDetail{ProcedureID: "restart_down_unit"}),
	})
	if err == nil {
		t.Fatal("expected CreateAction to reject a procedure detail missing its required param")
	}
}
