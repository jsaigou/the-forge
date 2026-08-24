// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_procedurize_test.go — "let smith fix it" (autonomous-remediation
// Sprint 3, docs/v5-smith.md §13): the procedure_preview + procedurize HTTP
// surface, exercised end to end against a real smith.Smith + store.DB —
// same house conventions as smith_procedure_actions_test.go.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/store"
)

// serverWithSmithProcedurize is serverWithSmithProcedures plus Deps.Cfg
// wired — restartAllowed (execute.go/runNativeOp) fails closed on a nil
// config, and no other fixture in this package wires one, so the
// restart_down_unit procedurize path needs its own server.
func serverWithSmithProcedurize(t *testing.T, ident authz.Identity) (*Server, *store.DB) {
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
		Cfg:         func() *config.Config { return &config.Config{} },
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

func TestSmithActionProcedurePreview_MappedKind(t *testing.T) {
	s, _ := serverWithSmithProcedurize(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	_, resp := createAction(t, s, smith.KindRestartForgeUnit, smith.RiskLow, map[string]any{"unit": "forge-stt"})
	action := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w := do(t, s, authedRequest("GET", fmt.Sprintf("/api/v1/smith/actions/%d/procedure_preview", id), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("procedure_preview = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var previewResp map[string]any
	decodeJSON(t, w.Body, &previewResp)
	preview := previewResp["preview"].(map[string]any)
	if preview["procedure_id"] != "restart_down_unit" {
		t.Errorf("procedure_id = %v, want restart_down_unit", preview["procedure_id"])
	}
}

func TestSmithActionProcedurePreview_UnmappedKind(t *testing.T) {
	s, _ := serverWithSmithProcedurize(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	_, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": "disk_usage_report"})
	action := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w := do(t, s, authedRequest("GET", fmt.Sprintf("/api/v1/smith/actions/%d/procedure_preview", id), nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("procedure_preview for an unmapped kind = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestSmithActionProcedurize_EndToEnd(t *testing.T) {
	s, _ := serverWithSmithProcedurize(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	_, resp := createAction(t, s, smith.KindRestartForgeUnit, smith.RiskLow, map[string]any{"unit": "forge-stt"})
	source := resp["action"].(map[string]any)
	sourceID := int64(source["id"].(float64))

	w := do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/procedurize", sourceID), nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("procedurize = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var procResp map[string]any
	decodeJSON(t, w.Body, &procResp)
	replacement := procResp["action"].(map[string]any)
	replacementID := int64(replacement["id"].(float64))
	if replacement["kind"] != "procedure" {
		t.Fatalf("replacement kind = %v, want procedure", replacement["kind"])
	}

	run := waitForProcedureStatus(t, s, replacementID, "completed", "failed")
	if run["status"] != "completed" {
		t.Fatalf("run = %+v, want status completed", run)
	}

	w = do(t, s, authedRequest("GET", fmt.Sprintf("/api/v1/smith/actions/%d", sourceID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET source action = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var sourceResp map[string]any
	decodeJSON(t, w.Body, &sourceResp)
	sourceAfter := sourceResp["action"].(map[string]any)
	if sourceAfter["status"] != "superseded" {
		t.Fatalf("source status = %v, want superseded", sourceAfter["status"])
	}
}

func TestSmithActionProcedurize_NotPending(t *testing.T) {
	s, _ := serverWithSmithProcedurize(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	_, resp := createAction(t, s, smith.KindRestartForgeUnit, smith.RiskLow, map[string]any{"unit": "forge-stt"})
	source := resp["action"].(map[string]any)
	sourceID := int64(source["id"].(float64))

	if w := do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/approve", sourceID), nil)); w.Code != http.StatusAccepted {
		t.Fatalf("approve source = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	// Let the atomic restart finish before trying to procedurize a no-longer-
	// pending action.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w := do(t, s, authedRequest("GET", fmt.Sprintf("/api/v1/smith/actions/%d", sourceID), nil))
		var r map[string]any
		decodeJSON(t, w.Body, &r)
		if a, ok := r["action"].(map[string]any); ok && a["status"] != "approved" && a["status"] != "executing" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	w := do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/procedurize", sourceID), nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("procedurize on a non-pending source = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

func TestSmithActionProcedurize_UnmappedKind(t *testing.T) {
	s, _ := serverWithSmithProcedurize(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	_, resp := createAction(t, s, smith.KindProcedure, smith.RiskInfo, map[string]any{"procedure_id": "disk_usage_report"})
	source := resp["action"].(map[string]any)
	sourceID := int64(source["id"].(float64))

	w := do(t, s, authedRequest("POST", fmt.Sprintf("/api/v1/smith/actions/%d/procedurize", sourceID), nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("procedurize on an unmapped kind = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
