// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
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

// serverWithNotifications builds a test Server backed by a real in-memory
// store.DB for the Notifications dependency — same pattern as
// serverWithMetricsStore (metrics_handlers_test.go): exercises real SQL
// dedupe/upsert, not a hand-rolled fake.
func serverWithNotifications(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots:     collector.NewStatic(nil),
		Engine:        &engine.Stub{},
		Sched:         &sched.Stub{},
		Auth:          &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:        events,
		Publish:       events,
		Config:        func() *config.Config { return cfg },
		Hostname:      "test-host",
		Notifications: db.Notifications(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db
}

func TestNotificationsListEmptyWhenUnwired(t *testing.T) {
	s := newTestServer(t) // no Notifications wired
	w := do(t, s, authedRequest("GET", "/api/v1/notifications", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp notificationsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Notifications == nil {
		t.Fatal("notifications = nil, want empty slice (frozen shape requires an array)")
	}
}

func TestNotificationsListAckDismiss(t *testing.T) {
	s, db := serverWithNotifications(t)
	ctx := t.Context()

	id, err := db.Notifications().Upsert(ctx, "GTT_HIGH", "warn", "", "GTT at 95%", "GTT_HIGH:", time.Now())
	if err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	// List (default) includes the active row.
	w := do(t, s, authedRequest("GET", "/api/v1/notifications", nil))
	if w.Code != 200 {
		t.Fatalf("list status = %d, want 200", w.Code)
	}
	var resp notificationsResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Notifications) != 1 || resp.Notifications[0].Code != "GTT_HIGH" {
		t.Fatalf("expected 1 GTT_HIGH notification, got %+v", resp.Notifications)
	}
	if resp.Notifications[0].AcknowledgedAt != nil {
		t.Errorf("expected acknowledged_at absent before ack")
	}

	// Ack.
	w = do(t, s, authedRequest("POST", "/api/v1/notifications/"+itoa(id)+"/ack", nil))
	if w.Code != 200 {
		t.Fatalf("ack status = %d, want 200", w.Code)
	}
	decodeJSON(t, do(t, s, authedRequest("GET", "/api/v1/notifications", nil)).Body, &resp)
	if resp.Notifications[0].AcknowledgedAt == nil {
		t.Errorf("expected acknowledged_at set after ack")
	}

	// Dismiss removes it from the default (active-only) list.
	w = do(t, s, authedRequest("POST", "/api/v1/notifications/"+itoa(id)+"/dismiss", nil))
	if w.Code != 200 {
		t.Fatalf("dismiss status = %d, want 200", w.Code)
	}
	decodeJSON(t, do(t, s, authedRequest("GET", "/api/v1/notifications", nil)).Body, &resp)
	if len(resp.Notifications) != 0 {
		t.Fatalf("expected 0 active after dismiss, got %d", len(resp.Notifications))
	}

	// include_dismissed=1 still shows it.
	decodeJSON(t, do(t, s, authedRequest("GET", "/api/v1/notifications?include_dismissed=1", nil)).Body, &resp)
	if len(resp.Notifications) != 1 || resp.Notifications[0].DismissedAt == nil {
		t.Fatalf("expected 1 dismissed row with dismissed_at set, got %+v", resp.Notifications)
	}
}

func TestNotificationsAckAll(t *testing.T) {
	s, db := serverWithNotifications(t)
	ctx := t.Context()

	if _, err := db.Notifications().Upsert(ctx, "UNIT_OOM", "crit", "ai-mode-comfyui", "OOM", "UNIT_OOM:ai-mode-comfyui", time.Now()); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, err := db.Notifications().Upsert(ctx, "UNIT_CRASH", "crit", "forge-tts", "crashed", "UNIT_CRASH:forge-tts", time.Now()); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	w := do(t, s, authedRequest("POST", "/api/v1/notifications/ack-all", nil))
	if w.Code != 200 {
		t.Fatalf("ack-all status = %d, want 200", w.Code)
	}

	var resp notificationsResponse
	decodeJSON(t, do(t, s, authedRequest("GET", "/api/v1/notifications", nil)).Body, &resp)
	if len(resp.Notifications) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(resp.Notifications))
	}
	for _, n := range resp.Notifications {
		if n.AcknowledgedAt == nil {
			t.Errorf("row %d (%s) not acknowledged", n.ID, n.Code)
		}
	}
}

// TestNotificationsMutationsRequireOperator asserts ack/dismiss/ack-all are
// gated (RoleOperator), same tier as service-mode toggle / reservation
// mutations — a viewer-only identity must be refused.
func TestNotificationsMutationsRequireOperator(t *testing.T) {
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "viewer", Role: authz.RoleViewer}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })

	for _, path := range []string{"/api/v1/notifications/1/ack", "/api/v1/notifications/1/dismiss", "/api/v1/notifications/ack-all"} {
		w := do(t, s, authedRequest("POST", path, nil))
		if w.Code != 403 {
			t.Errorf("%s: status = %d, want 403 for viewer role", path, w.Code)
		}
	}
}
