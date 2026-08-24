// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/maintenance"
	"github.com/jsaigou/the-forge/internal/sched"
)

// newMaintenanceTestServer builds a Server with a real, in-memory-backed
// maintenance.Gate wired in — newTestServer/newTestServerWith don't expose
// Deps.Maintenance, and this handler group needs one on every test.
func newMaintenanceTestServer(t *testing.T) (*Server, *maintenance.Gate) {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	gate := maintenance.New(newFakeSettings(), events, time.Now, t.Logf)
	s := New(Deps{
		Snapshots:   collector.NewStatic(nil),
		Engine:      &engine.Stub{},
		Sched:       &sched.Stub{},
		Auth:        &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:      events,
		Publish:     events,
		Config:      func() *config.Config { return cfg },
		Hostname:    "test-host",
		Maintenance: gate,
	})
	t.Cleanup(func() { s.Close() })
	return s, gate
}

func TestHandleMaintenanceGet_InactiveByDefault(t *testing.T) {
	s, _ := newMaintenanceTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/maintenance", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var st maintenance.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Active {
		t.Fatal("expected inactive by default")
	}
}

func TestHandleMaintenanceEnter_RequiresReason(t *testing.T) {
	s, _ := newMaintenanceTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/maintenance", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMaintenanceEnter_ThenGetReflectsActive(t *testing.T) {
	s, _ := newMaintenanceTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/maintenance",
		strings.NewReader(`{"reason":"build refresh","duration_minutes":30,"affected_slots":["a3"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var st maintenance.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !st.Active || st.LeaseID == "" || st.Reason != "build refresh" {
		t.Fatalf("unexpected state: %+v", st)
	}

	getRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(getRec, authedRequest(http.MethodGet, "/api/v1/maintenance", nil))
	var got maintenance.State
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Active || got.LeaseID != st.LeaseID {
		t.Fatalf("GET did not reflect the entered window: %+v", got)
	}
}

func TestHandleMaintenanceEnter_ConflictWhenAlreadyActive(t *testing.T) {
	s, gate := newMaintenanceTestServer(t)
	if _, err := gate.Enter(maintenance.EnterRequest{Reason: "already running", Duration: time.Hour}); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/maintenance", strings.NewReader(`{"reason":"second"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMaintenanceExit_ConflictWhenNotActive(t *testing.T) {
	s, _ := newMaintenanceTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/maintenance", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMaintenanceExit_ForcesExitRegardlessOfLease(t *testing.T) {
	s, gate := newMaintenanceTestServer(t)
	if _, err := gate.Enter(maintenance.EnterRequest{Reason: "r", Duration: time.Hour}); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/maintenance", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gate.Status().Active {
		t.Fatal("expected the window to be closed after DELETE")
	}
}

func TestHandleMaintenance_503WhenUnwired(t *testing.T) {
	s := newTestServer(t) // no Maintenance dep
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		rec := httptest.NewRecorder()
		var body strings.Reader
		if m == http.MethodPost {
			body = *strings.NewReader(`{"reason":"r"}`)
		}
		s.Handler().ServeHTTP(rec, authedRequest(m, "/api/v1/maintenance", &body))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503, body = %s", m, rec.Code, rec.Body.String())
		}
	}
}
