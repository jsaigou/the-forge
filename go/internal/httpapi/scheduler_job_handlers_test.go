// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
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
	"github.com/jsaigou/the-forge/internal/store"
)

// newSchedulerJobTestServer builds a Server with a real in-memory
// scheduler_jobs table, the sched stub, and an operator-or-admin identity —
// mirroring newCostTestServer's shape.
func newSchedulerJobTestServer(t *testing.T, role authz.Role) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
		Modes: map[string]config.Mode{
			"qwen3": {
				Label: "Qwen3", Default: true,
				Services: []config.Service{{Model: "qwen3.gguf", Alias: "qwen3", Context: 131072, PortRole: "a1"}},
			},
		},
	})
	s := New(Deps{
		Snapshots:     collector.NewStatic(nil),
		Engine:        &engine.Stub{},
		Sched:         &sched.Stub{},
		SchedulerJobs: db.SchedulerJobs(),
		Auth:          &stubAuth{identity: authz.Identity{Name: "tester", Role: role}},
		Events:        events,
		Publish:       events,
		Config:        func() *config.Config { return cfg },
		Hostname:      "test-host",
	})
	t.Cleanup(func() { s.Close() })
	return s, db
}

const createJobBody = `{"name":"nightly-batch","cron":"0 3 * * *","config_name":"qwen3","slot":"a1"}`

func TestSchedulerJobCreateAndList(t *testing.T) {
	s, _ := newSchedulerJobTestServer(t, authz.RoleAdmin)

	w := do(t, s, authedRequest("POST", "/api/v1/scheduler/jobs", strings.NewReader(createJobBody)))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201: %s", w.Code, w.Body.String())
	}

	w = do(t, s, authedRequest("GET", "/api/v1/scheduler/jobs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Jobs  []schedulerJobJSON `json:"jobs"`
		Total int                `json:"total"`
	}
	decodeJSON(t, w.Body, &resp)
	if resp.Total != 1 || len(resp.Jobs) != 1 {
		t.Fatalf("list total = %d len = %d, want 1/1", resp.Total, len(resp.Jobs))
	}
	j := resp.Jobs[0]
	if j.Name != "nightly-batch" || j.Cron != "0 3 * * *" || j.ConfigName != "qwen3" {
		t.Errorf("job roundtrip mismatch: %+v", j)
	}
	if j.Slot == nil || *j.Slot != "a1" {
		t.Errorf("slot = %v, want a1", j.Slot)
	}
	if !j.Enabled {
		t.Error("enabled should default true")
	}
	if j.NextRunAt == nil {
		t.Error("next_run_at should be computed on create")
	}
	if j.LastRunAt != nil {
		t.Error("last_run_at should be null on a fresh job")
	}
}

func TestSchedulerJobCreateValidation(t *testing.T) {
	cases := map[string]string{
		"missing name":    `{}`,
		"missing cron":    `{"name":"x","config_name":"qwen3"}`,
		"bad cron":        `{"name":"x","cron":"99 * * * *","config_name":"qwen3"}`,
		"short cron":      `{"name":"x","cron":"* * * *","config_name":"qwen3"}`,
		"unknown config":  `{"name":"x","cron":"* * * * *","config_name":"nope"}`,
		"bad slot":        `{"name":"x","cron":"* * * * *","config_name":"qwen3","slot":"a9"}`,
		"unknown field":   `{"name":"x","cron":"* * * * *","config_name":"qwen3","wat":1}`,
		"name not string": `{"name":5,"cron":"* * * * *","config_name":"qwen3"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newSchedulerJobTestServer(t, authz.RoleAdmin)
			w := do(t, s, authedRequest("POST", "/api/v1/scheduler/jobs", strings.NewReader(body)))
			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s = %d, want 422: %s", body, w.Code, w.Body.String())
			}
		})
	}
}

func TestSchedulerJobDuplicateName422(t *testing.T) {
	s, _ := newSchedulerJobTestServer(t, authz.RoleAdmin)
	w := do(t, s, authedRequest("POST", "/api/v1/scheduler/jobs", strings.NewReader(createJobBody)))
	if w.Code != http.StatusCreated {
		t.Fatalf("first POST = %d, want 201", w.Code)
	}
	w = do(t, s, authedRequest("POST", "/api/v1/scheduler/jobs",
		strings.NewReader(`{"name":"nightly-batch","cron":"* * * * *","config_name":"qwen3"}`)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("duplicate POST = %d, want 422: %s", w.Code, w.Body.String())
	}
}

func TestSchedulerJobUpdateAndDelete(t *testing.T) {
	s, _ := newSchedulerJobTestServer(t, authz.RoleAdmin)
	w := do(t, s, authedRequest("POST", "/api/v1/scheduler/jobs", strings.NewReader(createJobBody)))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d: %s", w.Code, w.Body.String())
	}

	w = do(t, s, authedRequest("PUT", "/api/v1/scheduler/jobs/1",
		strings.NewReader(`{"name":"renamed","cron":"*/30 * * * *","config_name":"qwen3","enabled":false}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	var upd struct {
		Job schedulerJobJSON `json:"job"`
	}
	decodeJSON(t, w.Body, &upd)
	if upd.Job.Name != "renamed" || !strings.Contains(upd.Job.Cron, "*/30") || upd.Job.Enabled {
		t.Errorf("update not reflected: %+v", upd.Job)
	}

	w = do(t, s, authedRequest("DELETE", "/api/v1/scheduler/jobs/1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200: %s", w.Code, w.Body.String())
	}
	w = do(t, s, authedRequest("GET", "/api/v1/scheduler/jobs", nil))
	var resp struct{ Total int }
	decodeJSON(t, w.Body, &resp)
	if resp.Total != 0 {
		t.Errorf("total after delete = %d, want 0", resp.Total)
	}

	// Missing rows 404.
	w = do(t, s, authedRequest("PUT", "/api/v1/scheduler/jobs/42",
		strings.NewReader(`{"name":"x","cron":"* * * * *","config_name":"qwen3"}`)))
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT missing = %d, want 404", w.Code)
	}
	w = do(t, s, authedRequest("DELETE", "/api/v1/scheduler/jobs/42", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("DELETE missing = %d, want 404", w.Code)
	}
	w = do(t, s, authedRequest("POST", "/api/v1/scheduler/jobs/42/run-now", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("run-now missing = %d, want 404", w.Code)
	}
}

func TestSchedulerJobRunNow(t *testing.T) {
	s, _ := newSchedulerJobTestServer(t, authz.RoleOperator)
	// Seed through the store — POST is admin-only and this server speaks
	// operator, which is exactly who run-now targets.
	if _, err := s.deps.SchedulerJobs.Create(context.Background(), store.SchedulerJob{
		Name: "nightly-batch", Cron: "0 3 * * *", ConfigName: "qwen3", Slot: "a1",
		Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	w := do(t, s, authedRequest("POST", "/api/v1/scheduler/jobs/1/run-now", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("run-now = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool    `json:"ok"`
		Status string  `json:"status"`
		Slot   *string `json:"slot"`
	}
	decodeJSON(t, w.Body, &resp)
	if !resp.OK || resp.Status != "loaded" {
		t.Errorf("run-now response = %+v, want ok loaded", resp)
	}

	// last_run_at recorded by run-now.
	w = do(t, s, authedRequest("GET", "/api/v1/scheduler/jobs", nil))
	var list struct {
		Jobs []schedulerJobJSON `json:"jobs"`
	}
	decodeJSON(t, w.Body, &list)
	if list.Jobs[0].LastRunAt == nil {
		t.Error("last_run_at should be set after run-now")
	}
}

func TestSchedulerJobRoles(t *testing.T) {
	s, _ := newSchedulerJobTestServer(t, authz.RoleViewer)

	// Viewer can't even list (operator gate).
	w := do(t, s, authedRequest("GET", "/api/v1/scheduler/jobs", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer GET = %d, want 403", w.Code)
	}

	// Operator can list and run-now but not create/update/delete.
	sOp, _ := newSchedulerJobTestServer(t, authz.RoleOperator)
	wOp := do(t, sOp, authedRequest("GET", "/api/v1/scheduler/jobs", nil))
	if wOp.Code != http.StatusOK {
		t.Errorf("operator GET = %d, want 200", wOp.Code)
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/scheduler/jobs"},
		{"PUT", "/api/v1/scheduler/jobs/1"},
		{"DELETE", "/api/v1/scheduler/jobs/1"},
	} {
		w := do(t, sOp, authedRequest(tc.method, tc.path, strings.NewReader(createJobBody)))
		if w.Code != http.StatusForbidden {
			t.Errorf("operator %s %s = %d, want 403", tc.method, tc.path, w.Code)
		}
	}
}

// TestSchedulerJobsNilStoreListEmpty pins the unwired-daemon behavior: GET
// returns the frozen empty shape instead of erroring.
func TestSchedulerJobsNilStoreListEmpty(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/scheduler/jobs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Jobs  []schedulerJobJSON `json:"jobs"`
		Total int                `json:"total"`
	}
	decodeJSON(t, w.Body, &resp)
	if resp.Jobs == nil || resp.Total != 0 {
		t.Errorf("nil-store list = %+v, want empty non-nil jobs / total 0", resp)
	}
}
