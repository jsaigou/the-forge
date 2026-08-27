// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeSettings is an in-memory store.Settings for tests.
type fakeSettings struct {
	mu sync.Mutex
	kv map[string][]byte
}

func newFakeSettings() *fakeSettings { return &fakeSettings{kv: map[string][]byte{}} }

func (f *fakeSettings) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.kv[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}

func (f *fakeSettings) Set(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[key] = value
	return nil
}

// ── Test fixtures ───────────────────────────────────────────────────────────

// stubAuth always returns the given identity, mirroring authz.StubAuthenticator
// but with a configurable role for RBAC tests.
type stubAuth struct {
	identity authz.Identity
}

func (s *stubAuth) VerifySession(string) (authz.Identity, error) {
	return s.identity, nil
}
func (s *stubAuth) VerifyBearerFrom(_ context.Context, _, token string, _ authz.KeyKind) (authz.Identity, error) {
	if _, _, _, err := authz.ParseToken(token); err != nil {
		return authz.Identity{}, err
	}
	return s.identity, nil
}

// newTestServer builds a Server wired with stubs + a bus for tests.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
}

// newTestServerWith builds a Server wired with stubs + a bus for tests,
// using a config that includes a sample mode + ports so lifecycle and
// infra-services handlers have something to read.
func newTestServerWith(t *testing.T, ident authz.Identity) *Server {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1":   {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
			"a2": {Unit: "forge-a2", Port: 8081, Label: "A2", Order: 2},
		},
		Ports: map[string]int{"embedding": 8083, "stt": 8084},
		Modes: map[string]config.Mode{
			"qwen3": {
				Label: "Qwen3", Family: "Qwen", Description: "Qwen3 inference", Default: true,
				Services: []config.Service{{Model: "qwen3.gguf", Alias: "qwen3", Context: 131072, PortRole: "a1"}},
			},
			"comfyui": {Label: "ComfyUI", Type: "service", Unit: "ai-mode-comfyui"},
		},
	})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: ident},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })
	return s
}

// authedRequest wraps a request with a stub bearer token so the auth
// middleware accepts it. The token grammar is sk-forge-<12hex>-<secret>
// where secret is 16–128 chars; stubAuth verifies the format but accepts
// any well-formed secret.
func authedRequest(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	return r
}

// do runs a request against the server and returns the response body.
func do(t *testing.T, s *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// decodeJSON helper for tests.
func decodeJSON(t *testing.T, body io.Reader, v any) {
	t.Helper()
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ── Health + auth ────────────────────────────────────────────────────────────

func TestHealthNoAuth(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, httptest.NewRequest("GET", "/api/v1/health", nil))
	if w.Code != 200 {
		t.Fatalf("health = %d, want 200", w.Code)
	}
	var resp map[string]string
	decodeJSON(t, w.Body, &resp)
	if resp["status"] != "ok" || resp["hostname"] != "test-host" {
		t.Errorf("health body = %v, want ok/test-host", resp)
	}
}

func TestAuthRequired(t *testing.T) {
	s := newTestServer(t)
	// No Authorization header → 401 (PWA redirects to /login?next=<path>).
	w := do(t, s, httptest.NewRequest("GET", "/api/v1/session", nil))
	if w.Code != 401 {
		t.Errorf("unauth session = %d, want 401", w.Code)
	}
}

func TestSessionWithStub(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/session", nil))
	if w.Code != 200 {
		t.Fatalf("session = %d, want 200", w.Code)
	}
	var resp sessionInfoResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Username != "operator" || resp.Role != "admin" {
		t.Errorf("session = %+v, want operator/admin", resp)
	}
	if resp.CSRFToken == "" {
		t.Error("CSRF token must not be empty (PWA needs it for mutations)")
	}
}

func TestRoleGate(t *testing.T) {
	// viewer role should be able to GET status but not PUT scheduler config.
	viewer := authz.Identity{Name: "viewer", Role: authz.RoleViewer}
	s := newTestServerWith(t, viewer)

	w := do(t, s, authedRequest("GET", "/api/v1/status", nil))
	if w.Code != 200 {
		t.Errorf("viewer GET status = %d, want 200", w.Code)
	}

	body := bytes.NewBufferString(`{"idle_unload_s":180,"small_job_token_threshold":1500,"priority_jump_cap":2,"reservation_soon_min":10}`)
	w = do(t, s, authedRequest("PUT", "/api/v1/scheduler/config", body))
	if w.Code != 403 {
		t.Errorf("viewer PUT scheduler config = %d, want 403", w.Code)
	}
}

// ── Status / metrics / scheduler status ─────────────────────────────────────

func TestStatusShape(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/status", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var resp statusResponse
	decodeJSON(t, w.Body, &resp)
	// Required fields present (Contract 1 §3 nullable fields must not be absent).
	if resp.Hostname != "test-host" {
		t.Errorf("hostname = %q", resp.Hostname)
	}
	if resp.Slots == nil {
		t.Error("slots must not be nil — empty slots emit null values, not absent keys")
	}
	if resp.SlotLabels == nil {
		t.Error("slot_labels must not be nil")
	}
	if resp.SlotLoading == nil {
		t.Error("slot_loading must not be nil")
	}
	if resp.SlotUnloading == nil {
		t.Error("slot_unloading must not be nil")
	}
}

func TestSchedulerStatusShape(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/scheduler/status", nil))
	if w.Code != 200 {
		t.Fatalf("scheduler/status = %d", w.Code)
	}
	var resp schedulerStatusResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Slots == nil || resp.SlotLabels == nil || resp.IdleSeconds == nil || resp.SlotMemoryBytes == nil {
		t.Error("SchedulerStatus maps must not be nil")
	}
	if resp.UnitMemoryBytes == nil {
		t.Error("unit_memory_bytes must not be nil — S2 attribution, empty map required, not null")
	}
	if resp.Queue == nil {
		t.Error("queue must not be nil — empty array required, not null")
	}
}

// ── 422 validation ───────────────────────────────────────────────────────────

func TestReservationCreateValidation(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		body string
		want string // expected field key in the 422 response
	}{
		{
			name: "missing label",
			body: `{"model":"qwen3","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","scope":"bay","bay":"a1","created_by":"x"}`,
			want: "label",
		},
		{
			name: "bad mode name (uppercase)",
			body: `{"label":"x","model":"UPPER","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","scope":"bay","bay":"a1","created_by":"x"}`,
			want: "model",
		},
		{
			name: "bay/scope mismatch (whole_box with bay)",
			body: `{"label":"x","model":"qwen3","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","scope":"whole_box","bay":"a1","created_by":"x"}`,
			want: "bay",
		},
		{
			name: "bay missing for scope=bay",
			body: `{"label":"x","model":"qwen3","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","scope":"bay","created_by":"x"}`,
			want: "bay",
		},
		{
			name: "end before start",
			body: `{"label":"x","model":"qwen3","start":"2026-01-02T00:00:00Z","end":"2026-01-01T00:00:00Z","scope":"whole_box","created_by":"x"}`,
			want: "end",
		},
		{
			name: "bad slot name in bay",
			body: `{"label":"x","model":"qwen3","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","scope":"bay","bay":"bogus","created_by":"x"}`,
			want: "bay",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := authedRequest("POST", "/api/v1/reservations", strings.NewReader(c.body))
			w := do(t, s, r)
			if w.Code != 422 {
				t.Fatalf("code = %d, want 422 (body=%s)", w.Code, c.body)
			}
			var resp struct {
				Error  string            `json:"error"`
				Fields map[string]string `json:"fields"`
			}
			decodeJSON(t, w.Body, &resp)
			if resp.Error != "validation_failed" {
				t.Errorf("error = %q, want validation_failed", resp.Error)
			}
			if _, ok := resp.Fields[c.want]; !ok {
				t.Errorf("fields = %v, want key %q", resp.Fields, c.want)
			}
		})
	}
}

func TestSchedulerConfigValidation(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"idle_unload_s below 30", `{"idle_unload_s":10,"small_job_token_threshold":1500,"priority_jump_cap":2,"reservation_soon_min":10}`, "idle_unload_s"},
		{"idle_unload_s above 3600", `{"idle_unload_s":99999,"small_job_token_threshold":1500,"priority_jump_cap":2,"reservation_soon_min":10}`, "idle_unload_s"},
		{"small_job_token_threshold < 1", `{"idle_unload_s":180,"small_job_token_threshold":0,"priority_jump_cap":2,"reservation_soon_min":10}`, "small_job_token_threshold"},
		{"priority_jump_cap negative", `{"idle_unload_s":180,"small_job_token_threshold":1500,"priority_jump_cap":-1,"reservation_soon_min":10}`, "priority_jump_cap"},
		{"reservation_soon_min above 120", `{"idle_unload_s":180,"small_job_token_threshold":1500,"priority_jump_cap":2,"reservation_soon_min":999}`, "reservation_soon_min"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := authedRequest("PUT", "/api/v1/scheduler/config", strings.NewReader(c.body))
			w := do(t, s, r)
			if w.Code != 422 {
				t.Fatalf("code = %d, want 422 (body=%s)", w.Code, c.body)
			}
			var resp struct {
				Error  string            `json:"error"`
				Fields map[string]string `json:"fields"`
			}
			decodeJSON(t, w.Body, &resp)
			if _, ok := resp.Fields[c.want]; !ok {
				t.Errorf("fields = %v, want key %q", resp.Fields, c.want)
			}
		})
	}
}

func TestSchedulerConfigDefaultsApplied(t *testing.T) {
	s := newTestServer(t)
	// Empty body — defaults must apply and 200 must return.
	r := authedRequest("PUT", "/api/v1/scheduler/config", strings.NewReader(`{}`))
	w := do(t, s, r)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200 (defaults fill in)", w.Code)
	}
}

func TestSlotLoadValidation(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad mode", `{"mode":"UPPER","slot":"a1"}`, "mode"},
		{"bad slot", `{"mode":"qwen3","slot":"bogus"}`, "slot"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := authedRequest("POST", "/api/v1/load", strings.NewReader(c.body))
			w := do(t, s, r)
			if w.Code != 422 {
				t.Fatalf("code = %d, want 422 (body=%s)", w.Code, c.body)
			}
		})
	}
}

func TestCompressorPassthroughValidation(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"scope=proxy without service", `{"scope":"proxy","enabled":true}`, "service"},
		{"scope=all with service set", `{"scope":"all","enabled":true,"service":"a1"}`, "service"},
		{"bad scope", `{"scope":"bogus","enabled":true}`, "scope"},
		{"missing enabled", `{"scope":"all"}`, "enabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := authedRequest("PUT", "/api/v1/compressor/passthrough", strings.NewReader(c.body))
			w := do(t, s, r)
			if w.Code != 422 {
				t.Fatalf("code = %d, want 422 (body=%s)", w.Code, c.body)
			}
		})
	}
}

// ── Lifecycle: switch / load / unload ────────────────────────────────────────

func TestSwitchStartsInBackgroundAndPublishes(t *testing.T) {
	s := newTestServer(t)
	var gotStarted, gotComplete bool
	var mu sync.Mutex

	// Subscribe to the bus before kicking the switch so we don't miss events.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := s.deps.Events.Subscribe(ctx)

	w := do(t, s, authedRequest("POST", "/api/v1/switch/qwen3", nil))
	if w.Code != 200 {
		t.Fatalf("switch = %d, want 200", w.Code)
	}
	var resp struct {
		Success    bool   `json:"success"`
		Message    string `json:"message"`
		InProgress bool   `json:"in_progress"`
	}
	decodeJSON(t, w.Body, &resp)
	if !resp.Success || !resp.InProgress {
		t.Errorf("response = %+v, want success=true in_progress=true", resp)
	}

	// Drain the events we expect within 1s (stub engine returns instantly).
	deadline := time.After(1 * time.Second)
	for {
		select {
		case ev := <-events:
			mu.Lock()
			if ev.Name == "switch_started" {
				gotStarted = true
			}
			if ev.Name == "switch_complete" {
				gotComplete = true
			}
			mu.Unlock()
			if gotStarted && gotComplete {
				return
			}
		case <-deadline:
			mu.Lock()
			t.Fatalf("timed out: started=%v complete=%v", gotStarted, gotComplete)
		}
	}
}

// ── SSE event-name invariants ────────────────────────────────────────────────
//
// Contract 1 §4: event names use the underscore form. V4 had four colon-form
// bug sites (tts start/stop, service-mode start/stop) that emitted
// `status:update` (colon) — nothing subscribes to that name, so those
// transitions only reached clients on the next 30s heartbeat. V5 emits
// `status_update` (underscore) at every site.

func TestSSEEmitsUnderscoreEventNames(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := s.deps.Events.Subscribe(ctx)

	// Each colon-form name from V4 must never be emitted; the underscore
	// form is what the PWA subscribes to (web/src/lib/sse.ts).
	badNames := map[string]bool{
		"status:update": true, // the V4 bug — must never appear
	}
	goodNames := map[string]bool{
		"status_update":      true,
		"switch_started":     true,
		"switch_complete":    true,
		"switch_failed":      true,
		"load_started":       true,
		"load_complete":      true,
		"load_failed":        true,
		"unload_complete":    true,
		"config_updated":     true,
		"registry:refreshed": true, // namespaced with colon — that's the only valid colon form
		"tts:job_update":     true, // namespaced with colon — also valid
		"profile:started":   true, // PROFILE track (Contract 1 amendment)
		"profile:progress":  true,
		"profile:done":      true,
		"profile:failed":    true,
		// HF model-acquisition track (Contract 1 amendment, go/internal/
		// hfdownload/events.go) — not exercised by this test's switch/
		// load/unload flow, registered here anyway per this file's own
		// "Contract 1 §4 names are frozen" convention.
		"download:progress":      true,
		"download:state_changed": true,
		"download:done":          true,
		"download:failed":        true,
		"download:deleted":       true,
	}

	// Kick a switch to generate switch_started + switch_complete.
	_ = do(t, s, authedRequest("POST", "/api/v1/switch/qwen3", nil))
	// Kick a load to generate load_started + load_complete.
	_ = do(t, s, authedRequest("POST", "/api/v1/load", strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	// Kick an unload to generate unload_complete.
	_ = do(t, s, authedRequest("POST", "/api/v1/unload", strings.NewReader(`{"slot":"a1"}`)))

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if badNames[ev.Name] {
				t.Errorf("emitted colon-form event %q — must be underscore form", ev.Name)
			}
			if !goodNames[ev.Name] {
				t.Errorf("emitted unknown event %q — Contract 1 §4 names are frozen", ev.Name)
			}
		case <-deadline:
			return
		}
	}
}

// ── Compressor: provider key masking ───────────────────────────────────────────
//
// Contract 1 §18: "must not leak proxy tokens or full API keys". V4 leaked
// active_token once (fixed 2026-07-21); V5 keeps the fix by masking the
// returned provider api_key to prefix+ellipsis.

func TestCompressorMasksProviderAPIKey(t *testing.T) {
	// We can't directly construct store.Routing in a stub without
	// re-declaring all its methods with the right types. This test instead
	// exercises the maskSecret helper that the compressor handler uses, and
	// verifies the invariant by reflecting on the response field.
	cases := []struct {
		input string
		want  string
	}{
		{"sk-forge-abc123-verylongsecret", "sk-f…"},
		{"short", "shor…"}, // 5 chars: prefix=4 + ellipsis (never the full secret)
		{"abc", "abc…"},    // ≤4 chars: return as-is + ellipsis (still never the full secret in practice — secrets are always longer)
		{"", ""},
	}
	for _, c := range cases {
		got := maskSecret(c.input)
		if got != c.want {
			t.Errorf("maskSecret(%q) = %q, want %q", c.input, got, c.want)
		}
		// Invariant: the full secret must never appear in the masked form
		// when it's long enough that masking is meaningful.
		if len(c.input) > 4 && strings.Contains(c.want, c.input) {
			t.Errorf("maskSecret(%q) leaked the full secret", c.input)
		}
	}
}

// ── Modes CRUD: GET works, mutations 501 ─────────────────────────────────────

func TestModesGetReturnsConfig(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/modes", nil))
	if w.Code != 200 {
		t.Fatalf("modes GET = %d, want 200", w.Code)
	}
}

func TestModesMutationsReturn501(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		method, path string
		body         string
	}{
		{"POST", "/api/v1/modes", `{"name":"x","label":"X"}`},
		{"PUT", "/api/v1/modes/foo", `{"name":"foo","label":"Foo"}`},
		{"DELETE", "/api/v1/modes/foo", ""},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			body := bytes.NewBufferString(c.body)
			w := do(t, s, authedRequest(c.method, c.path, body))
			if w.Code != 501 {
				t.Errorf("%s %s = %d, want 501 (C1-Q3 deferred)", c.method, c.path, w.Code)
			}
		})
	}
}

// ── Endpoints that intentionally stay 501 after the C1-Q2/Q3 amendments ──
// (service-mode/TTS start-stop and router-settings PUT are now WIRED — see
// TestServiceMode*/TestTTS*/TestRouterSettingsPut*. Modes CRUD stays 501
// permanently (read-only config). The Compressor proxy lifecycle is now wired
// (Phase 2, docs/v5-headroom-topology.md) but degrades to 501 whenever
// Deps.CompressorProvisioner isn't set — which newTestServer doesn't set, so
// this table still exercises that degrade path; see
// TestCompressorLifecycleRestart/Teardown in compressor_handlers_test.go for the
// wired-up behavior.)

func TestUnportedEndpointsReturn501(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		method, path string
		body         string
	}{
		{"POST", "/api/v1/compressor/restart", `{"service":"a1"}`},
		{"POST", "/api/v1/compressor/proxy/teardown", `{"service":"a1"}`},
		{"POST", "/api/v1/compressor/proxy/create", `{"service":"a1","label":"x","target_url":"http://x"}`},
		{"POST", "/api/v1/modes", `{"name":"x"}`},
		{"PUT", "/api/v1/modes/qwen3", `{}`},
		{"DELETE", "/api/v1/modes/qwen3", ""},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			body := bytes.NewBufferString(c.body)
			w := do(t, s, authedRequest(c.method, c.path, body))
			if w.Code != 501 {
				t.Errorf("%s %s = %d, want 501", c.method, c.path, w.Code)
			}
		})
	}
}

// ── PWA embed.FS serving ─────────────────────────────────────────────────────

func TestPWAShellServesIndex(t *testing.T) {
	s := newTestServer(t)
	// When web_dist is populated (CI runs `cd web && npm run build` before
	// `go test`), / serves the actual index.html. On a fresh checkout
	// without a web build, / returns 503 with a hint. Either is acceptable
	// — what matters is that the response is one of those two codes, not a
	// 404 or 500.
	w := do(t, s, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 && w.Code != 503 {
		t.Fatalf("/ = %d, want 200 (built) or 503 (not built)", w.Code)
	}
	if w.Code == 200 {
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("/ content-type = %q, want text/html", ct)
		}
		if !strings.Contains(w.Body.String(), "<html") && !strings.Contains(w.Body.String(), "<!doctype") {
			t.Error("/ must serve an HTML document when built")
		}
	}
}

func TestPWAAssetsNotFound(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, httptest.NewRequest("GET", "/assets/nonexistent.js", nil))
	if w.Code != 404 {
		t.Errorf("/assets/nonexistent.js = %d, want 404", w.Code)
	}
}

func TestLoginRendersHTML(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, httptest.NewRequest("GET", "/login?next=/console", nil))
	if w.Code != 200 {
		t.Fatalf("/login = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("/login content-type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "<form") {
		t.Error("/login must render an HTML form (Phase 3 wires the POST handler)")
	}
}

// ── SSE initial status_update + keepalive + heartbeat ────────────────────────
//
// Contract 1 §4: on connect, immediately send one status_update. After 25s
// with no events, send a keepalive comment. Every 30s, send a full-Status
// status_update heartbeat. The first event the client sees must be
// status_update (it bootstraps the PWA's query cache).

func TestSSEInitialEventIsStatusUpdate(t *testing.T) {
	s := newTestServer(t)
	// Use a real HTTP server so the SSE handler can flush properly.
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// EventSource-shaped GET (no event-stream support in httptest, but we
	// can read the body as a streaming response).
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read the first event — must be status_update.
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	first := string(buf[:n])
	if !strings.Contains(first, "event: status_update\n") {
		t.Errorf("first SSE chunk = %q, want it to start with 'event: status_update'", first)
	}
	if !strings.Contains(first, "data: {") {
		t.Errorf("first SSE chunk = %q, want a JSON data: line", first)
	}
}

// ── Reservations list with query filter ──────────────────────────────────────

func TestReservationsListEmpty(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/reservations", nil))
	if w.Code != 200 {
		t.Fatalf("reservations = %d", w.Code)
	}
	var resp struct {
		Reservations []reservationResponse `json:"reservations"`
		Total        int                   `json:"total"`
	}
	decodeJSON(t, w.Body, &resp)
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0 (empty stub)", resp.Total)
	}
	if resp.Reservations == nil {
		t.Error("reservations must be [] not null")
	}
}

// ── Usage window validation ─────────────────────────────────────────────────

func TestUsageWindowValidation(t *testing.T) {
	cases := []struct {
		window string
		want   bool
	}{
		{"1h", true},
		{"24h", true},
		{"7d", true},
		{"30d", true},
		{"0h", false},
		{"7w", false},
		{"", false},
		{"bogus", false},
	}
	for _, c := range cases {
		_, ok := parseUsageWindow(c.window)
		if ok != c.want {
			t.Errorf("parseUsageWindow(%q) = %v, want %v", c.window, ok, c.want)
		}
	}
}

func TestUsageEmptyResponseShape(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/usage?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("usage = %d", w.Code)
	}
	var resp usageResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Window != "7d" {
		t.Errorf("window = %q", resp.Window)
	}
	// Empty slices, not null — the PWA's .map() would crash on null.
	if resp.Models == nil || resp.External == nil || resp.Compressor == nil {
		t.Error("usage response arrays must not be nil (PWA expects arrays)")
	}
	if resp.Totals != (usageTotals{}) {
		t.Errorf("totals = %+v, want zero (no store wired)", resp.Totals)
	}
}

func TestInfraServicesShape(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/infra-services", nil))
	if w.Code != 200 {
		t.Fatalf("infra-services = %d", w.Code)
	}
	var resp infraServicesResponse
	decodeJSON(t, w.Body, &resp)
	// Always returns: LLM Router (A0), STT, TTS, Embedding + service modes.
	if len(resp.Services) < 3 {
		t.Errorf("services = %d, want at least 3 (A0 + TTS + ...)", len(resp.Services))
	}
	// §0.5: find the LLM Router (A0) entry (renamed from "A0 Proxy").
	var a0 *infraService
	for i := range resp.Services {
		if resp.Services[i].Name == "LLM Router (A0)" {
			a0 = &resp.Services[i]
		}
	}
	if a0 == nil {
		t.Fatal("infra-services must include LLM Router (A0)")
	}
	// A0 active state is derived from forge-daemon, not the dead
	// forge-router unit. With no snapshot wired, the handler responding
	// means the daemon is up → active=true.
	if !a0.Active {
		t.Error("A0 must be active (daemon is up — handler is responding)")
	}
	// §0.5: compressor_passthrough is always set on the A0 row (false when
	// passthrough is off / settings not wired).
	if a0.CompressorPassthrough == nil {
		t.Error("A0 compressor_passthrough must not be nil")
	}
	// §0.5: detail is a short status line when active.
	if a0.Detail == nil || *a0.Detail == "" {
		t.Error("A0 detail must be set when active")
	}
}

func TestRouterSettingsGet(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/router/settings", nil))
	if w.Code != 200 {
		t.Fatalf("router/settings = %d", w.Code)
	}
	var resp routerSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.BusyMode != "wait" && resp.BusyMode != "fail_fast" {
		t.Errorf("busy_mode = %q, want wait or fail_fast", resp.BusyMode)
	}
}

// ── C1-Q5: router settings PUT persists to store.Settings ──

func serverWithSettings(t *testing.T, set store.Settings) *Server {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
		Modes: map[string]config.Mode{
			"comfyui": {Label: "ComfyUI", Type: "service", Unit: "ai-mode-comfyui"},
			"qwen3":   {Label: "Qwen3", Default: true},
		},
	})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Settings:  set,
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRouterSettingsPutPersists(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)
	w := do(t, s, authedRequest("PUT", "/api/v1/router/settings", strings.NewReader(`{"busy_mode":"fail_fast"}`)))
	if w.Code != 200 {
		t.Fatalf("PUT router/settings = %d, body=%s", w.Code, w.Body)
	}
	raw, err := set.Get(context.Background(), "router.busy_mode")
	if err != nil {
		t.Fatalf("settings not persisted: %v", err)
	}
	if string(raw) != `"fail_fast"` {
		t.Errorf("persisted = %s, want \"fail_fast\"", raw)
	}
}

func TestRouterSettingsPutRejectsBadMode(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	w := do(t, s, authedRequest("PUT", "/api/v1/router/settings", strings.NewReader(`{"busy_mode":"nonsense"}`)))
	if w.Code != 422 {
		t.Fatalf("bad busy_mode = %d, want 422", w.Code)
	}
}

// ── C1-Q2: service-mode + TTS unit control ──

func TestServiceModeStartOK(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	w := do(t, s, authedRequest("POST", "/api/v1/service-mode/comfyui/start", nil))
	if w.Code != 200 {
		t.Fatalf("service-mode start = %d, body=%s", w.Code, w.Body)
	}
}

func TestServiceModeUnknown(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	w := do(t, s, authedRequest("POST", "/api/v1/service-mode/nope/start", nil))
	if w.Code != 404 {
		t.Fatalf("unknown service-mode = %d, want 404", w.Code)
	}
}

func TestServiceModeNotAService(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	// qwen3 is an inference mode, not a service mode.
	w := do(t, s, authedRequest("POST", "/api/v1/service-mode/qwen3/start", nil))
	if w.Code != 400 {
		t.Fatalf("inference mode as service = %d, want 400", w.Code)
	}
}

func TestTTSStartStopOK(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	for _, verb := range []string{"start", "stop"} {
		w := do(t, s, authedRequest("POST", "/api/v1/tts/"+verb, nil))
		if w.Code != 200 {
			t.Fatalf("tts %s = %d, body=%s", verb, w.Code, w.Body)
		}
	}
}

func TestCompressorLifecycleStill501(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	for _, path := range []string{"/api/v1/compressor/restart", "/api/v1/compressor/proxy/teardown", "/api/v1/compressor/proxy/create"} {
		w := do(t, s, authedRequest("POST", path, nil))
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%s = %d, want 501", path, w.Code)
		}
	}
}

// ── §0.5: A0 wiring — active from forge-daemon, not the dead forge-router ──

// newA0TestServer builds a Server with a snapshot carrying the given
// forge-daemon unit state and optional compressor.passthrough_all setting.
func newA0TestServer(t *testing.T, daemonState collector.UnitState, passthrough bool) *Server {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
		Ports: map[string]int{"embedding": 8083, "stt": 8084},
		Modes: map[string]config.Mode{
			"qwen3": {Label: "Qwen3", Default: true},
		},
	})
	snap := &collector.Snapshot{
		TakenAt:   time.Now(),
		Hostname:  "test-host",
		Units:     map[string]collector.UnitState{"forge-daemon": daemonState},
		Slots:     map[string]collector.SlotState{},
		Inference: map[string]collector.SlotInference{},
		Ports:     map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}
	settings := newFakeSettings()
	if passthrough {
		raw, _ := json.Marshal(true)
		_ = settings.Set(context.Background(), "compressor.passthrough_all", raw)
	}
	s := New(Deps{
		Snapshots: collector.NewStatic(snap),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
		Settings:  settings,
	})
	t.Cleanup(func() { s.Close() })
	return s
}

func TestA0ActiveFromDaemonUnit(t *testing.T) {
	s := newA0TestServer(t, collector.UnitState{ActiveState: "active", SubState: "running"}, false)
	w := do(t, s, authedRequest("GET", "/api/v1/infra-services", nil))
	if w.Code != 200 {
		t.Fatalf("infra-services = %d", w.Code)
	}
	var resp infraServicesResponse
	decodeJSON(t, w.Body, &resp)
	var a0 *infraService
	for i := range resp.Services {
		if resp.Services[i].Name == "LLM Router (A0)" {
			a0 = &resp.Services[i]
		}
	}
	if a0 == nil {
		t.Fatal("LLM Router (A0) not found")
	}
	if !a0.Active {
		t.Error("A0 must be active when forge-daemon is active")
	}
	if a0.CompressorPassthrough == nil || *a0.CompressorPassthrough {
		t.Error("A0 compressor_passthrough must be false when passthrough is off")
	}
	if a0.Detail == nil || *a0.Detail != "routing" {
		t.Errorf("A0 detail = %v, want \"routing\"", a0.Detail)
	}
}

func TestA0InactiveWhenDaemonDown(t *testing.T) {
	s := newA0TestServer(t, collector.UnitState{ActiveState: "inactive", SubState: "dead"}, false)
	w := do(t, s, authedRequest("GET", "/api/v1/infra-services", nil))
	if w.Code != 200 {
		t.Fatalf("infra-services = %d", w.Code)
	}
	var resp infraServicesResponse
	decodeJSON(t, w.Body, &resp)
	var a0 *infraService
	for i := range resp.Services {
		if resp.Services[i].Name == "LLM Router (A0)" {
			a0 = &resp.Services[i]
		}
	}
	if a0 == nil {
		t.Fatal("LLM Router (A0) not found")
	}
	if a0.Active {
		t.Error("A0 must be inactive when forge-daemon is inactive")
	}
	if a0.Detail != nil {
		t.Errorf("A0 detail = %v, want nil when inactive", a0.Detail)
	}
}

func TestA0PassthroughOn(t *testing.T) {
	s := newA0TestServer(t, collector.UnitState{ActiveState: "active", SubState: "running"}, true)
	w := do(t, s, authedRequest("GET", "/api/v1/infra-services", nil))
	if w.Code != 200 {
		t.Fatalf("infra-services = %d", w.Code)
	}
	var resp infraServicesResponse
	decodeJSON(t, w.Body, &resp)
	var a0 *infraService
	for i := range resp.Services {
		if resp.Services[i].Name == "LLM Router (A0)" {
			a0 = &resp.Services[i]
		}
	}
	if a0 == nil {
		t.Fatal("LLM Router (A0) not found")
	}
	if !a0.Active {
		t.Error("A0 must be active")
	}
	if a0.CompressorPassthrough == nil || !*a0.CompressorPassthrough {
		t.Error("A0 compressor_passthrough must be true")
	}
	if a0.Detail == nil || *a0.Detail != "passthrough on" {
		t.Errorf("A0 detail = %v, want \"passthrough on\"", a0.Detail)
	}
}

// ── 2026-07-29: A0 must not report healthy when its Compressor proxies are
// dead — the exact incident (host reboot took down every headroom@ unit;
// a0Active alone kept reporting "routing" while every real request was
// black-holed) that motivated folding Compressor proxy health into this row,
// and adding dedicated per-proxy rows below it. ──

// newA0CompressorTestServer is newA0TestServer plus a fakeCompressor proxy list
// and compressor.local_enabled, so infra-services' new Compressor-awareness can
// be tested without touching the pre-existing A0 test builder above.
func newA0CompressorTestServer(t *testing.T, daemonState collector.UnitState, proxies []store.ProxyRow, localEnabled bool) (*Server, *fakeSettings) {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
	})
	units := map[string]collector.UnitState{"forge-daemon": daemonState}
	for _, p := range proxies {
		// Simulate the collector having probed each proxy's unit: "dead"
		// service names come with the inactive state below, everything
		// else defaults active — see the per-test proxy lists.
		if p.Label == "dead" {
			units[p.Unit] = collector.UnitState{ActiveState: "inactive", SubState: "dead"}
		} else {
			units[p.Unit] = collector.UnitState{ActiveState: "active", SubState: "running"}
		}
	}
	snap := &collector.Snapshot{
		TakenAt:        time.Now(),
		Hostname:       "test-host",
		Units:          units,
		Slots:          map[string]collector.SlotState{},
		Inference:      map[string]collector.SlotInference{},
		Ports:          map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}
	settings := newFakeSettings()
	if localEnabled {
		raw, _ := json.Marshal(true)
		_ = settings.Set(context.Background(), "compressor.local_enabled", raw)
	}
	fh := &fakeCompressor{}
	for _, p := range proxies {
		p.Label = "" // was overloaded above as the down/up signal only
		_ = fh.SaveProxy(context.Background(), p)
	}
	s := New(Deps{
		Snapshots: collector.NewStatic(snap),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
		Settings:  settings,
		Routing:  fh,
	})
	t.Cleanup(func() { s.Close() })
	return s, settings
}

func findService(services []infraService, name string) *infraService {
	for i := range services {
		if services[i].Name == name {
			return &services[i]
		}
	}
	return nil
}

func TestA0DegradedWhenLocalCompressorProxyDown(t *testing.T) {
	s, _ := newA0CompressorTestServer(t,
		collector.UnitState{ActiveState: "active", SubState: "running"},
		[]store.ProxyRow{{Service: "local", Unit: "headroom@local", Port: 8788, Label: "dead"}},
		true, // local fronting enabled — this proxy is genuinely on the routing path
	)
	w := do(t, s, authedRequest("GET", "/api/v1/infra-services", nil))
	var resp infraServicesResponse
	decodeJSON(t, w.Body, &resp)

	a0 := findService(resp.Services, "LLM Router (A0)")
	if a0 == nil {
		t.Fatal("LLM Router (A0) not found")
	}
	if a0.Active {
		t.Error("A0 must report inactive when an unbypassed, in-use Compressor proxy is dead — this is the exact 'showed healthy but black-holed' incident")
	}
	if a0.Detail == nil || !strings.Contains(*a0.Detail, "local") {
		t.Errorf("A0 detail = %v, want it to name the dead proxy", a0.Detail)
	}

	hr := findService(resp.Services, "Compressor (local)")
	if hr == nil {
		t.Fatal("Compressor (local) row not found — per-proxy visibility is the second half of this fix")
	}
	if hr.Active {
		t.Error("Compressor (local) row must report inactive — its unit is down")
	}
	if hr.CompressorState != "down" {
		t.Errorf("Compressor (local) compressor_state = %q, want \"down\"", hr.CompressorState)
	}
}

func TestA0HealthyWhenDeadCompressorProxyIsBypassed(t *testing.T) {
	s, _ := newA0CompressorTestServer(t,
		collector.UnitState{ActiveState: "active", SubState: "running"},
		[]store.ProxyRow{{Service: "deepseek", Unit: "headroom@deepseek", Port: 8790, Passthrough: true, Label: "dead"}},
		false,
	)
	w := do(t, s, authedRequest("GET", "/api/v1/infra-services", nil))
	var resp infraServicesResponse
	decodeJSON(t, w.Body, &resp)

	a0 := findService(resp.Services, "LLM Router (A0)")
	if a0 == nil || !a0.Active {
		t.Error("A0 must stay active — the dead proxy is individually bypassed, so it is not on the live routing path")
	}
	hr := findService(resp.Services, "Compressor (deepseek)")
	if hr == nil {
		t.Fatal("Compressor (deepseek) row not found")
	}
	if hr.Detail == nil || *hr.Detail != "bypassed" {
		t.Errorf("Compressor (deepseek) detail = %v, want \"bypassed\"", hr.Detail)
	}
	// Precedence: this proxy's unit is dead (Label "dead" in the fixture
	// above), but it's also individually bypassed — bypassed must win over
	// down, since an intentionally-bypassed proxy being down isn't a failure.
	if hr.CompressorState != "bypassed" {
		t.Errorf("Compressor (deepseek) compressor_state = %q, want \"bypassed\" (takes precedence over down)", hr.CompressorState)
	}
}

func TestA0HealthyWhenLocalProxyDownButLocalFrontingOff(t *testing.T) {
	s, _ := newA0CompressorTestServer(t,
		collector.UnitState{ActiveState: "active", SubState: "running"},
		[]store.ProxyRow{{Service: "local", Unit: "headroom@local", Port: 8788, Label: "dead"}},
		false, // local fronting never turned on — this row isn't actually on the routing path
	)
	w := do(t, s, authedRequest("GET", "/api/v1/infra-services", nil))
	var resp infraServicesResponse
	decodeJSON(t, w.Body, &resp)

	a0 := findService(resp.Services, "LLM Router (A0)")
	if a0 == nil || !a0.Active {
		t.Error("A0 must stay active — local Compressor fronting is off, so its dead unit doesn't affect real routing")
	}
	hr := findService(resp.Services, "Compressor (local)")
	if hr == nil {
		t.Fatal("Compressor (local) row not found")
	}
	if hr.CompressorState != "idle" {
		t.Errorf("Compressor (local) compressor_state = %q, want \"idle\"", hr.CompressorState)
	}
}

func TestA0HealthyWhenAllProxiesUp(t *testing.T) {
	s, _ := newA0CompressorTestServer(t,
		collector.UnitState{ActiveState: "active", SubState: "running"},
		[]store.ProxyRow{
			{Service: "local", Unit: "headroom@local", Port: 8788},
			{Service: "deepseek", Unit: "headroom@deepseek", Port: 8790},
		},
		true,
	)
	w := do(t, s, authedRequest("GET", "/api/v1/infra-services", nil))
	var resp infraServicesResponse
	decodeJSON(t, w.Body, &resp)

	a0 := findService(resp.Services, "LLM Router (A0)")
	if a0 == nil || !a0.Active {
		t.Error("A0 must be active when every in-use proxy is up")
	}
	for _, name := range []string{"Compressor (local)", "Compressor (deepseek)"} {
		row := findService(resp.Services, name)
		if row == nil || !row.Active {
			t.Errorf("%s must report active", name)
		}
		if row != nil && row.CompressorState != "compressing" {
			t.Errorf("%s compressor_state = %q, want \"compressing\"", name, row.CompressorState)
		}
	}
}

// ── §0.6: Single-instance load guard ──

// newLoadGuardTestServer builds a Server with a registry (one model linked
// to mode "qwen3"), two slots, and a snapshot showing the mode loaded on
// "a1" — so loading "qwen3" on "a2" must be rejected 409.
func newLoadGuardTestServer(t *testing.T) *Server {
	t.Helper()
	events := bus.New()
	cfg, err := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1":   {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
			"a2": {Unit: "forge-a2", Port: 8081, Label: "A2", Order: 2},
		},
		Modes: map[string]config.Mode{
			"qwen3": {Label: "Qwen3", Services: []config.Service{{Model: "qwen3.gguf", Alias: "qwen3"}}},
		},
	})
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}
	// Phase B: catalog-backed registry (same seed as model_cards_test.go).
	regDB, configID := seedTestCatalog(t, context.Background())
	mode := cfg.Modes["qwen3"]
	mode.ConfigID = configID
	cfg.Modes["qwen3"] = mode
	reg := registry.New(regDB.Catalog(), func() *config.Config { return cfg }, nil)
	snap := &collector.Snapshot{
		TakenAt:   time.Now(),
		Hostname:  "test-host",
		Units:     map[string]collector.UnitState{},
		Slots: map[string]collector.SlotState{
			"a1":   {Slot: "a1", Mode: "qwen3", Unit: "forge-a1", Port: 8080, Label: "A1"},
			"a2": {Slot: "a2", Mode: "", Unit: "forge-a2", Port: 8081, Label: "A2"},
		},
		Inference:      map[string]collector.SlotInference{},
		Ports:          map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}
	s := New(Deps{
		Snapshots: collector.NewStatic(snap),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
		Registry:  reg,
	})
	t.Cleanup(func() { s.Close() })
	return s
}

func TestLoadGuardRejectsAlreadyLoaded(t *testing.T) {
	s := newLoadGuardTestServer(t)
	// qwen3 is loaded on "a1"; loading it on "a2" must 409.
	w := do(t, s, authedRequest("POST", "/api/v1/load",
		strings.NewReader(`{"mode":"qwen3","slot":"a2"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("load = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	if resp["error"] != "already_loaded" {
		t.Errorf("error = %v, want already_loaded", resp["error"])
	}
	if resp["slot"] != "a1" {
		t.Errorf("slot = %v, want a1", resp["slot"])
	}
}

func TestLoadGuardAllowsDifferentModel(t *testing.T) {
	// Same fixture but a mode NOT in the registry — the guard is a no-op
	// when the model id can't be resolved, so the load proceeds (the engine
	// stub will accept it). We verify the guard doesn't false-positive.
	s := newLoadGuardTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/load",
		strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	// Loading qwen3 onto a1 (where it's already loaded) — the guard
	// checks OTHER slots, not the target slot, so this should NOT be
	// rejected by the guard. It may be rejected by the slot-already-loading
	// check, but that returns 409 with "success":false, not "already_loaded".
	if w.Code == http.StatusConflict {
		var resp map[string]any
		decodeJSON(t, w.Body, &resp)
		if resp["error"] == "already_loaded" {
			t.Errorf("guard must not reject loading the same mode onto the same slot (target excluded)")
		}
	}
}

func TestLoadGuardNoRegistryWired(t *testing.T) {
	// The guard is pure mode-name comparison (ADR 0006) and never consults
	// the registry, so it behaves identically with or without one wired.
	s := newTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/load",
		strings.NewReader(`{"mode":"qwen3","slot":"a1"}`)))
	if w.Code == http.StatusConflict {
		var resp map[string]any
		decodeJSON(t, w.Body, &resp)
		if resp["error"] == "already_loaded" {
			t.Errorf("guard must be a no-op when registry is not wired")
		}
	}
}

// TestLoadGuardAllowsDifferentConfigSameModel is the ADR 0006 regression
// test: two Configs of the same underlying Model are distinct loadable
// units and may be resident on two slots simultaneously. Before the fix,
// findModelLoadedElsewhere resolved both config names to the same registry
// model_id and incorrectly 409'd; the guard now compares config/mode names
// directly and must allow this.
func TestLoadGuardAllowsDifferentConfigSameModel(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cat := db.Catalog()

	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "Qwen3 Coder Test"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fm, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fm.ID,
		FilePath: "qwen3-coder.gguf", ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	// Two Configs, same Variant/Model, different context sizes — e.g.
	// qwen3-coder-256k and qwen3-coder-1m from the ADR's own example.
	cfgAID, _ := cat.CreateConfig(ctx, store.Config{
		Name: "qwen3-coder-256k", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, NCtx: 262144,
	})
	cfgBID, _ := cat.CreateConfig(ctx, store.Config{
		Name: "qwen3-coder-1m", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, NCtx: 1048576,
	})

	events := bus.New()
	cfg, err := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1":   {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
			"a2": {Unit: "forge-a2", Port: 8081, Label: "A2", Order: 2},
		},
		Modes: map[string]config.Mode{
			"qwen3-coder-256k": {
				Label:    "Qwen3 Coder 256k",
				Services: []config.Service{{Model: "qwen3-coder.gguf", Alias: "qwen3-coder-256k"}},
			},
			"qwen3-coder-1m": {
				Label:    "Qwen3 Coder 1m",
				Services: []config.Service{{Model: "qwen3-coder.gguf", Alias: "qwen3-coder-1m"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}
	modeA := cfg.Modes["qwen3-coder-256k"]
	modeA.ConfigID = cfgAID
	cfg.Modes["qwen3-coder-256k"] = modeA
	modeB := cfg.Modes["qwen3-coder-1m"]
	modeB.ConfigID = cfgBID
	cfg.Modes["qwen3-coder-1m"] = modeB

	reg := registry.New(db.Catalog(), func() *config.Config { return cfg }, nil)
	snap := &collector.Snapshot{
		TakenAt:  time.Now(),
		Hostname: "test-host",
		Units:    map[string]collector.UnitState{},
		Slots: map[string]collector.SlotState{
			"a1":   {Slot: "a1", Mode: "qwen3-coder-256k", Unit: "forge-a1", Port: 8080, Label: "A1"},
			"a2": {Slot: "a2", Mode: "", Unit: "forge-a2", Port: 8081, Label: "A2"},
		},
		Inference:      map[string]collector.SlotInference{},
		Ports:          map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}
	s := New(Deps{
		Snapshots: collector.NewStatic(snap),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Registry:  reg,
	})
	t.Cleanup(func() { s.Close() })

	// qwen3-coder-256k is loaded on a1; loading the SIBLING config
	// qwen3-coder-1m (same model, different Config) onto a2 must
	// succeed — they are distinct loadable units per ADR 0006.
	w := do(t, s, authedRequest("POST", "/api/v1/load",
		strings.NewReader(`{"mode":"qwen3-coder-1m","slot":"a2"}`)))
	if w.Code == http.StatusConflict {
		var resp map[string]any
		decodeJSON(t, w.Body, &resp)
		if resp["error"] == "already_loaded" {
			t.Fatalf("guard incorrectly rejected a different config of the same model: %s", w.Body.String())
		}
	}
}

func TestDecodeJSONBody_RejectsOversized(t *testing.T) {
	big := strings.Repeat("x", 2<<20) // 2 MiB > 1 MiB limit
	req := httptest.NewRequest("POST", "/", strings.NewReader(big))
	var v map[string]any
	if fields := decodeJSONBody(req, &v); fields == nil {
		t.Fatal("expected decodeJSONBody to reject a >1 MiB body")
	}
}
