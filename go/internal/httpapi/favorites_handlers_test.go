// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

func serverWithFavorites(t *testing.T, ident authz.Identity) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()
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
		Favorites: db.Favorites(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db
}

func TestFavoritesListEmptyWhenUnwired(t *testing.T) {
	s := newTestServer(t) // no Favorites wired
	w := do(t, s, authedRequest("GET", "/api/v1/favorites", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp favoritesResponse
	decodeJSON(t, w.Body, &resp)
	if resp.SubjectIDs == nil {
		t.Fatal("subject_ids = nil, want empty slice")
	}
}

func TestFavoritesAddListRemove(t *testing.T) {
	s, _ := serverWithFavorites(t, authz.Identity{Name: "testuser", Role: authz.RoleViewer})

	// A viewer (not operator/admin) can star — this is a personal
	// preference, not a system-affecting mutation.
	w := do(t, s, authedRequest("PUT", "/api/v1/favorites/config/1", nil))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	do(t, s, authedRequest("PUT", "/api/v1/favorites/config/2", nil))

	w = do(t, s, authedRequest("GET", "/api/v1/favorites?subject_type=config", nil))
	var resp favoritesResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.SubjectIDs) != 2 {
		t.Fatalf("expected 2 favorites, got %+v", resp.SubjectIDs)
	}

	w = do(t, s, authedRequest("DELETE", "/api/v1/favorites/config/1", nil))
	if w.Code != 200 {
		t.Fatalf("DELETE = %d, want 200: %s", w.Code, w.Body.String())
	}
	w = do(t, s, authedRequest("GET", "/api/v1/favorites?subject_type=config", nil))
	decodeJSON(t, w.Body, &resp)
	if len(resp.SubjectIDs) != 1 || resp.SubjectIDs[0] != 2 {
		t.Errorf("expected only subject 2 remaining, got %+v", resp.SubjectIDs)
	}
}

func TestFavoritesScopedPerUser(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	newServer := func(name string) *Server {
		s := New(Deps{
			Snapshots: collector.NewStatic(nil),
			Engine:    &engine.Stub{},
			Sched:     &sched.Stub{},
			Auth:      &stubAuth{identity: authz.Identity{Name: name, Role: authz.RoleViewer}},
			Events:    events,
			Publish:   events,
			Config:    func() *config.Config { return cfg },
			Hostname:  "test-host",
			Favorites: db.Favorites(),
		})
		t.Cleanup(func() { s.Close() })
		return s
	}

	testuser := newServer("testuser")
	other := newServer("other")

	do(t, testuser, authedRequest("PUT", "/api/v1/favorites/config/1", nil))

	var resp favoritesResponse
	decodeJSON(t, do(t, other, authedRequest("GET", "/api/v1/favorites?subject_type=config", nil)).Body, &resp)
	if len(resp.SubjectIDs) != 0 {
		t.Errorf("expected 'other' to see 0 favorites (testuser's are scoped away), got %+v", resp.SubjectIDs)
	}
	decodeJSON(t, do(t, testuser, authedRequest("GET", "/api/v1/favorites?subject_type=config", nil)).Body, &resp)
	if len(resp.SubjectIDs) != 1 {
		t.Errorf("expected testuser to see 1 favorite, got %+v", resp.SubjectIDs)
	}
}
