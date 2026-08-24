// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
	"github.com/jsaigou/the-forge/internal/store"
)

// httpTestProcCheckpoint is a throwaway fixture procedure, registered once
// for this test binary (a separate process from package smith's own tests,
// so the id namespace can't collide) — proves the HTTP checkpoint
// approve/abort surface actually reaches smith.ApproveProcedureCheckpoint/
// AbortProcedureRun end to end, not just that the routes are mounted.
var httpTestProcCheckpoint = procedures.Procedure{
	ID:    "http_test_checkpoint",
	Title: "http test checkpoint procedure",
	Steps: []procedures.Step{
		{Title: "before", Argv: []string{"http-test-step", "1"}, Checkpoint: true, OnFail: procedures.FailAbort},
		{Title: "after", Argv: []string{"http-test-step", "2"}, OnFail: procedures.FailAbort},
	},
}

func init() {
	procedures.Register(httpTestProcCheckpoint)
}

// fakeHTTPRunStep is a minimal, deterministic Deps.RunStep — every call
// succeeds, nothing shells out for real.
func fakeHTTPRunStep(_ context.Context, spec procedures.StepSpec) (procedures.StepResult, error) {
	return procedures.StepResult{Stdout: strings.Join(spec.Argv, " "), Duration: time.Millisecond}, nil
}

// serverWithSmithProcedures is serverWithSmithActions plus Deps.RunStep
// wired — every other smith_actions_test.go test deliberately leaves
// RunStep nil (proving the unwired-fails-closed path via the real HTTP
// surface), so procedure-execution tests need their own server.
func serverWithSmithProcedures(t *testing.T, ident authz.Identity) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	seedActionsCatalog(t, db)

	events := bus.New()
	placer := &fakePlacer{slots: []string{"a1", "a2", "a3", "a4"}}
	agent := smith.New(smith.Deps{
		Store:       db,
		Catalog:     db.Catalog(),
		Settings:    db.Settings(),
		Sched:       newActionsStubSched(map[string]string{"a2": "ornith-35b"}),
		Placer:      placer,
		RestartUnit: func(context.Context, string) error { return nil },
		RunStep:     fakeHTTPRunStep,
		Source:      collector.NewStatic(nil),
		Publisher:   events,
		Subscriber:  events,
		Audit:       db.Audit(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	agent.Start(ctx)
	t.Cleanup(agent.Stop)

	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: ident},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Smith:     agent,
		Audit:     db.Audit(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db
}

func waitForProcedureStatus(t *testing.T, s *Server, id int64, want ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var run map[string]any
	for time.Now().Before(deadline) {
		w := do(t, s, authedRequest("GET", fmt.Sprintf("/api/v1/smith/actions/%d/procedure", id), nil))
		if w.Code == http.StatusOK {
			var resp map[string]any
			decodeJSON(t, w.Body, &resp)
			run, _ = resp["run"].(map[string]any)
			if run != nil {
				status, _ := run["status"].(string)
				for _, w := range want {
					if status == w {
						return run
					}
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("procedure run for action %d did not reach %v within timeout (last=%+v)", id, want, run)
	return nil
}

func TestSmithProcedureActions_CreateApproveRunsToCompletion(t *testing.T) {
	s, _ := serverWithSmithProcedures(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": "disk_usage_report"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	action, _ := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w = do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/approve", id), nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("approve = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	run := waitForProcedureStatus(t, s, id, "completed", "failed")
	if run["status"] != "completed" {
		t.Fatalf("run = %+v, want status completed", run)
	}
}

func TestSmithProcedureActions_CheckpointApproveAndAbort(t *testing.T) {
	t.Run("approve continues past the checkpoint", func(t *testing.T) {
		s, _ := serverWithSmithProcedures(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
		w, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": httpTestProcCheckpoint.ID})
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		action := resp["action"].(map[string]any)
		id := int64(action["id"].(float64))
		if w := do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/approve", id), nil)); w.Code != http.StatusAccepted {
			t.Fatalf("approve = %d, want 202; body=%s", w.Code, w.Body.String())
		}

		waitForProcedureStatus(t, s, id, "awaiting_checkpoint")

		w = do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/procedure/checkpoint/approve", id), nil))
		if w.Code != http.StatusAccepted {
			t.Fatalf("checkpoint approve = %d, want 202; body=%s", w.Code, w.Body.String())
		}

		run := waitForProcedureStatus(t, s, id, "completed", "failed")
		if run["status"] != "completed" {
			t.Fatalf("run = %+v, want status completed after checkpoint approval", run)
		}
	})

	t.Run("abort stops the run and refuses a second abort", func(t *testing.T) {
		s, _ := serverWithSmithProcedures(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
		w, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": httpTestProcCheckpoint.ID})
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		action := resp["action"].(map[string]any)
		id := int64(action["id"].(float64))
		if w := do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/approve", id), nil)); w.Code != http.StatusAccepted {
			t.Fatalf("approve = %d, want 202; body=%s", w.Code, w.Body.String())
		}
		waitForProcedureStatus(t, s, id, "awaiting_checkpoint")

		w = do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/procedure/checkpoint/abort", id), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("checkpoint abort = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var abortResp map[string]any
		decodeJSON(t, w.Body, &abortResp)
		aborted := abortResp["action"].(map[string]any)
		if aborted["status"] != "failed" {
			t.Fatalf("action status = %v, want failed", aborted["status"])
		}

		w = do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/procedure/checkpoint/abort", id), nil))
		if w.Code != http.StatusConflict {
			t.Fatalf("second abort = %d, want 409; body=%s", w.Code, w.Body.String())
		}
	})
}

func TestSmithProcedureRun_NotFoundBeforeApproval(t *testing.T) {
	s, _ := serverWithSmithProcedures(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	_, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": "disk_usage_report"})
	action := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w := do(t, s, authedRequest("GET", fmt.Sprintf("/api/v1/smith/actions/%d/procedure", id), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET procedure run before approval = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestSmithProcedureActions_CreateRejectsUnknownProcedureID(t *testing.T) {
	s, _ := serverWithSmithProcedures(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w, _ := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": "does_not_exist"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create with unknown procedure_id = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ── Sprint 4: supervision & evaluation harness ──────────────────────────

func TestSmithProcedureRunsList_ReturnsCompletedRunWithActionFields(t *testing.T) {
	s, _ := serverWithSmithProcedures(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": "disk_usage_report"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	action := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))
	if w := do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/approve", id), nil)); w.Code != http.StatusAccepted {
		t.Fatalf("approve = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	waitForProcedureStatus(t, s, id, "completed", "failed")

	w = do(t, s, authedRequest("GET", "/api/v1/smith/procedures/runs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list runs = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var listResp map[string]any
	decodeJSON(t, w.Body, &listResp)
	runs, _ := listResp["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want exactly 1", runs)
	}
	row := runs[0].(map[string]any)
	if int64(row["action_id"].(float64)) != id {
		t.Fatalf("runs[0].action_id = %v, want %d", row["action_id"], id)
	}
	if row["procedure_id"] != "disk_usage_report" {
		t.Fatalf("runs[0].procedure_id = %v, want disk_usage_report", row["procedure_id"])
	}
	if row["action_title"] == "" || row["action_title"] == nil {
		t.Fatalf("runs[0].action_title missing: %+v", row)
	}
}

func TestSmithProcedureScorecard_UnattendedAfterCompletion(t *testing.T) {
	s, _ := serverWithSmithProcedures(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	_, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": "disk_usage_report"})
	action := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))
	do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/approve", id), nil))
	waitForProcedureStatus(t, s, id, "completed", "failed")

	w := do(t, s, authedRequest("GET", fmt.Sprintf("/api/v1/smith/actions/%d/procedure/scorecard", id), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("scorecard = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var scResp map[string]any
	decodeJSON(t, w.Body, &scResp)
	sc := scResp["scorecard"].(map[string]any)
	if sc["completed"] != true {
		t.Fatalf("scorecard.completed = %v, want true", sc["completed"])
	}
	if sc["unattended_completion"] != true {
		t.Fatalf("scorecard.unattended_completion = %v, want true (disk_usage_report has no checkpoints)", sc["unattended_completion"])
	}
	if sc["checkpoints_declared"] != float64(0) {
		t.Fatalf("scorecard.checkpoints_declared = %v, want 0", sc["checkpoints_declared"])
	}
}

func TestSmithProcedureScorecard_NotFoundBeforeApproval(t *testing.T) {
	s, _ := serverWithSmithProcedures(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	_, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": "disk_usage_report"})
	action := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w := do(t, s, authedRequest("GET", fmt.Sprintf("/api/v1/smith/actions/%d/procedure/scorecard", id), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("scorecard before approval = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
