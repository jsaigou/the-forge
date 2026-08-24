// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
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
	"github.com/jsaigou/the-forge/internal/sched"
)

// slowLoadEngine embeds engine.Stub and overrides Load to run past the
// point where the triggering HTTP request completes, recording whether the
// context it was handed was ever canceled. This is what an httptest.
// NewRecorder-based test can't exercise: real net/http cancels a request's
// Context() as soon as its handler returns (documented behavior), and
// runLoadBackground's goroutine is specifically meant to keep running after
// that — a real listening server is required to observe the cancellation
// that r.Context() would have caused.
type slowLoadEngine struct {
	engine.Stub
	started  chan struct{}
	proceed  chan struct{}
	canceled bool
}

func (e *slowLoadEngine) Load(ctx context.Context, mode, slot string) engine.Result {
	close(e.started)
	select {
	case <-e.proceed:
	case <-ctx.Done():
		e.canceled = true
		return engine.Result{Success: false, Message: "canceled"}
	}
	if ctx.Err() != nil {
		e.canceled = true
	}
	return engine.Result{Success: true, Message: "loaded"}
}

// TestLoadBackgroundSurvivesRequestCompletion reproduces the bug found
// live-verifying Phase 9b against ForgeHost: POST /api/v1/load returns
// immediately (in_progress: true) while the actual engine.Load call runs in
// a background goroutine. That goroutine's context must NOT be derived from
// the triggering request — net/http cancels a request's Context() as soon
// as its ServeHTTP call returns, which for this handler is essentially
// immediately (right after it spawns the goroutine). Wiring the goroutine
// to r.Context() made every real load/switch/unload fail near-instantly
// with context.Canceled once the HTTP response was written, invisible to
// httptest.NewRecorder-based tests because a synthetic request's context is
// never canceled that way. This test uses a real listening httptest.Server
// so the request context genuinely gets canceled on handler return, and
// asserts the background load is unaffected.
func TestLoadBackgroundSurvivesRequestCompletion(t *testing.T) {
	events := bus.New()
	cfg, err := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a4": {Unit: "forge-a4", Port: 8088, Label: "A4", Order: 3},
		},
		Modes: map[string]config.Mode{
			"swallow-8b": {Label: "Swallow 8B", Services: []config.Service{{Model: "swallow-8b.gguf", Alias: "swallow-8b"}}},
		},
	})
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}

	fakeEngine := &slowLoadEngine{started: make(chan struct{}), proceed: make(chan struct{})}
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    fakeEngine,
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })

	// A real listening server, unlike httptest.NewRecorder, actually
	// cancels the request's Context() when the handler returns.
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/load",
		strings.NewReader(`{"mode":"swallow-8b","slot":"a4"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/load: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/load: got %d, want 200", resp.StatusCode)
	}

	// The request/response round-trip is done, so the request's own
	// Context() is now canceled. Give the goroutine's Load() a moment to
	// actually start (it may not have been scheduled yet).
	select {
	case <-fakeEngine.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background Load() never started")
	}

	// Simulate the real load taking a while after the request completed.
	time.Sleep(100 * time.Millisecond)
	close(fakeEngine.proceed)
	time.Sleep(100 * time.Millisecond)

	if fakeEngine.canceled {
		t.Fatal("background Load()'s context was canceled by the completed HTTP request — " +
			"the background goroutine must use the server's lifecycle context, not r.Context()")
	}
}
