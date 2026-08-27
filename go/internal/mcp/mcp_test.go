// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── fakes ──────────────────────────────────────────────────────────────────

// fakeSched is a controllable sched.Scheduler for the tool tests.
type fakeSched struct {
	status       sched.Status
	cfg          sched.Config
	reserv       []sched.Reservation
	ensureFn     func(context.Context, sched.EnsureRequest) (sched.Ticket, error)
	unloadFn     func(ctx context.Context, slot, requestedBy string) error
	createFn     func(context.Context, sched.Reservation) error
	updateFn     func(ctx context.Context, label string, r sched.Reservation) error
	cancelFn     func(ctx context.Context, label, requestedBy string) error
	loadStatusFn func(model string) sched.LoadState

	// captured call arguments for assertions.
	lastEnsure sched.EnsureRequest
	lastUnload [2]string // slot, requestedBy
	lastCreate sched.Reservation
	lastUpdate sched.Reservation
	lastCancel [2]string // label, requestedBy
}

var _ sched.Scheduler = (*fakeSched)(nil)

func (f *fakeSched) EnsureLoaded(ctx context.Context, req sched.EnsureRequest) (sched.Ticket, error) {
	f.lastEnsure = req
	if f.ensureFn != nil {
		return f.ensureFn(ctx, req)
	}
	return sched.Ticket{TicketID: "t1", Model: req.Model, RequestedBy: req.RequestedBy,
		TargetSlot: req.TargetSlot, Status: "loaded", SmallJob: req.SmallJob, EnqueuedAt: time.Unix(1000, 0)}, nil
}

func (f *fakeSched) Unload(ctx context.Context, slot, requestedBy string) error {
	f.lastUnload = [2]string{slot, requestedBy}
	if f.unloadFn != nil {
		return f.unloadFn(ctx, slot, requestedBy)
	}
	return nil
}

func (f *fakeSched) Status() sched.Status { return f.status }

func (f *fakeSched) Config() sched.Config {
	if f.cfg == (sched.Config{}) {
		return sched.DefaultConfig()
	}
	return f.cfg
}

func (f *fakeSched) SetConfig(c sched.Config) error { f.cfg = c; return nil }

func (f *fakeSched) Reservations() []sched.Reservation { return f.reserv }

func (f *fakeSched) CreateReservation(ctx context.Context, r sched.Reservation) error {
	f.lastCreate = r
	if f.createFn != nil {
		return f.createFn(ctx, r)
	}
	f.reserv = append(f.reserv, r)
	return nil
}

func (f *fakeSched) UpdateReservation(ctx context.Context, label string, r sched.Reservation) error {
	f.lastUpdate = r
	if f.updateFn != nil {
		return f.updateFn(ctx, label, r)
	}
	return nil
}

func (f *fakeSched) CancelReservation(ctx context.Context, label, requestedBy string) error {
	f.lastCancel = [2]string{label, requestedBy}
	if f.cancelFn != nil {
		return f.cancelFn(ctx, label, requestedBy)
	}
	return nil
}

func (f *fakeSched) LoadStatus(model string) sched.LoadState {
	if f.loadStatusFn != nil {
		return f.loadStatusFn(model)
	}
	return sched.LoadState{Model: model, State: "idle"}
}

// fakeEngine is a controllable CanFitter.
type fakeEngine struct {
	fit engine.CanFit
	err error
}

func (f *fakeEngine) CanFit(string) (engine.CanFit, error) { return f.fit, f.err }

// fakeAuth accepts a specific token → agent name; anything else is rejected.
// It also asserts the caller passes the expected KeyKind.
type fakeAuth struct {
	token string
	agent string
	t     *testing.T
}

var _ authz.Authenticator = (*fakeAuth)(nil)

func (a *fakeAuth) VerifySession(string) (authz.Identity, error) {
	return authz.Identity{}, errors.New("no sessions in mcp")
}

func (a *fakeAuth) VerifyBearerFrom(_ context.Context, _, token string, want authz.KeyKind) (authz.Identity, error) {
	if a.t != nil && want != authz.KindMCP {
		a.t.Errorf("VerifyBearerFrom want kind = %q, expected KindMCP", want)
	}
	if token != a.token {
		return authz.Identity{}, authz.ErrBadToken
	}
	return authz.Identity{Name: a.agent, Kind: authz.KindMCP, Role: authz.Role("")}, nil
}

// fakeAudit records audit writes for the R3 tests. List is unused here.
type fakeAudit struct {
	entries []store.AuditEntry
	err     error // when set, Write fails with this error
}

var _ store.Audit = (*fakeAudit)(nil)

func (a *fakeAudit) Write(_ context.Context, e store.AuditEntry) error {
	if a.err != nil {
		return a.err
	}
	a.entries = append(a.entries, e)
	return nil
}

func (a *fakeAudit) List(context.Context, string, string, int) ([]store.AuditEntry, error) {
	return nil, nil
}

// fakeCatalog is a controllable ModelLister for the list_models tests.
type fakeCatalog struct {
	configs      []store.Config
	offerings    []store.Offering
	configsErr   error
	offeringsErr error
}

var _ ModelLister = (*fakeCatalog)(nil)

func (c *fakeCatalog) ListConfigs(context.Context) ([]store.Config, error) {
	return c.configs, c.configsErr
}

func (c *fakeCatalog) ListOfferings(context.Context) ([]store.Offering, error) {
	return c.offerings, c.offeringsErr
}

// ── harness ────────────────────────────────────────────────────────────────

const goodToken = "sk-mcp-0123456789ab-secretsecretsecret"

func newTestServer(t *testing.T, sc sched.Scheduler, eng CanFitter) *Server {
	t.Helper()
	return NewWithDeps(Deps{
		Sched:  sc,
		Engine: eng,
		Auth:   &fakeAuth{token: goodToken, agent: "agent-x", t: t},
	})
}

// do issues a request against the server handler and returns status + parsed body.
func do(t *testing.T, srv *Server, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var parsed map[string]any
	if b := rec.Body.Bytes(); len(b) > 0 {
		_ = json.Unmarshal(b, &parsed)
	}
	return rec.Code, parsed
}

// ── unauthenticated routes ──────────────────────────────────────────────────

func TestHealthz(t *testing.T) {
	code, body := do(t, New(), http.MethodGet, "/healthz", "", "")
	if code != http.StatusOK {
		t.Fatalf("healthz status = %d", code)
	}
	if body["status"] != "ok" || body["service"] != "mcp" {
		t.Fatalf("healthz body = %v", body)
	}
}

func TestToolsDiscovery(t *testing.T) {
	// No auth required for discovery.
	code, body := do(t, New(), http.MethodGet, "/v1/tools", "", "")
	if code != http.StatusOK {
		t.Fatalf("tools status = %d", code)
	}
	list, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools not a list: %v", body["tools"])
	}
	want := map[string]bool{
		"list_models": true,
		"status":      true, "can_fit": true, "ensure_loaded": true, "unload": true,
		"list_reservations": true, "create_reservation": true,
		"update_reservation": true, "cancel_reservation": true,
	}
	// list_models is the post-freeze R2 addition (docs/v5-mcp-audit.md) and
	// must stay non-mutating; the mutating flag is what callers rely on.
	mutatingFlag := map[string]bool{
		"list_models": false, "status": false, "can_fit": false,
		"list_reservations": false,
		"ensure_loaded":     true, "unload": true,
		"create_reservation": true, "update_reservation": true,
		"cancel_reservation": true,
	}
	got := map[string]bool{}
	for _, e := range list {
		m := e.(map[string]any)
		name := m["name"].(string)
		got[name] = true
		if _, ok := m["description"]; !ok {
			t.Errorf("tool %q missing description", name)
		}
		if _, ok := m["input_schema"]; !ok {
			t.Errorf("tool %q missing input_schema", name)
		}
		if m["mutating"] != mutatingFlag[name] {
			t.Errorf("tool %q mutating = %v, want %v", name, m["mutating"], mutatingFlag[name])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("tool count = %d, want %d (%v)", len(got), len(want), got)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing tool %q in discovery", name)
		}
	}
	// list_models IS exposed (audit roadmap R2).
	if !got["list_models"] {
		t.Errorf("list_models should be exposed")
	}
}

func TestToolsDispatchMatchesDiscovery(t *testing.T) {
	if len(dispatch) != len(tools) {
		t.Fatalf("dispatch has %d entries, tools listing has %d", len(dispatch), len(tools))
	}
	for _, ts := range tools {
		if _, ok := dispatch[ts.Name]; !ok {
			t.Errorf("tool %q listed but not dispatchable", ts.Name)
		}
	}
}

// ── auth: no tailnet bypass, fail-closed ────────────────────────────────────

func TestPostMissingBearer401(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/status", "", "{}")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if body["error"] == nil {
		t.Errorf("missing error body: %v", body)
	}
}

func TestPostInvalidBearer401(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/status", "sk-mcp-ffffffffffff-wrongwrongwrongwrong", "{}")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestPostMalformedBearer401(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/status", "not-a-token", "{}")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

// A tailnet-source request with no bearer is STILL 401 — MCP has no tailnet
// bypass (unlike a0). We simulate a CGNAT source address + XFF; neither must
// grant access.
func TestNoTailnetBypass(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/status", strings.NewReader("{}"))
	req.RemoteAddr = "100.100.100.100:12345" // Tailscale CGNAT range
	req.Header.Set("X-Forwarded-For", "100.64.0.1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tailnet-source no-bearer status = %d, want 401", rec.Code)
	}
}

// Skeleton mode (New(), no Authenticator) is fail-closed: every POST is 401.
func TestSkeletonFailsClosed(t *testing.T) {
	code, _ := do(t, New(), http.MethodPost, "/v1/tools/status", goodToken, "{}")
	if code != http.StatusUnauthorized {
		t.Fatalf("skeleton POST status = %d, want 401 (fail-closed)", code)
	}
}

// ── unknown tool ────────────────────────────────────────────────────────────

func TestUnknownTool404(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/bogus", goodToken, "{}")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if body["error"] != "unknown_tool" {
		t.Errorf("error = %v, want unknown_tool", body["error"])
	}
	if _, ok := body["available"].([]any); !ok {
		t.Errorf("missing available list: %v", body)
	}
}

// Auth is checked before tool resolution: unknown tool + no bearer = 401.
func TestUnknownToolNoAuthIs401(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/bogus", "", "{}")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (auth before 404)", code)
	}
}

// ── list_models (roadmap R2) ───────────────────────────────────────────────

func TestListModelsHappyPath(t *testing.T) {
	cat := &fakeCatalog{
		offerings: []store.Offering{
			{WireModel: "kimi-k2", ProviderName: "moonshot", ContextLength: 131072, Enabled: true},
			{WireModel: "gone-remote", ProviderName: "old-provider", Enabled: false},
		},
		configs: []store.Config{
			{Name: "nemotron", Visibility: "visible", Status: "verified", IsDefault: true, NCtx: 65536},
			{Name: "internal-only", Visibility: "hidden", Status: "unverified"},
			// Name collides with the enabled offering — the offering must
			// win (its wire_model is what a0 actually routes), matching
			// BuildModelsResponse's dedup.
			{Name: "kimi-k2", Visibility: "visible", Status: "unverified"},
		},
	}
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	srv.deps.Catalog = cat
	code, body := do(t, srv, http.MethodPost, "/v1/tools/list_models", goodToken, "{}")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	// Disabled offering, hidden config, and the offering/config name
	// collision are all dropped.
	if body["total"].(float64) != 2 {
		t.Fatalf("total = %v, want 2: %v", body["total"], body)
	}
	models := body["models"].([]any)
	first := models[0].(map[string]any)
	if first["name"] != "kimi-k2" || first["kind"] != "remote" || first["provider"] != "moonshot" {
		t.Errorf("first entry = %v, want the enabled offering listed first", first)
	}
	if first["context_length"].(float64) != 131072 {
		t.Errorf("offering context_length = %v", first["context_length"])
	}
	second := models[1].(map[string]any)
	if second["name"] != "nemotron" || second["kind"] != "local" ||
		second["status"] != "verified" || second["default"] != true {
		t.Errorf("second entry = %v", second)
	}
	if second["context_length"].(float64) != 65536 {
		t.Errorf("config context_length = %v", second["context_length"])
	}
}

func TestListModelsNoCatalog503(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{}) // Catalog nil
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/list_models", goodToken, "{}")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
}

func TestListModelsCatalogError500(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	srv.deps.Catalog = &fakeCatalog{configsErr: errors.New("db locked")}
	code, body := do(t, srv, http.MethodPost, "/v1/tools/list_models", goodToken, "{}")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if body["error"] != "tool_failed" {
		t.Errorf("error = %v, want tool_failed", body["error"])
	}
}

// ── status ──────────────────────────────────────────────────────────────────

func TestStatusHappyPath(t *testing.T) {
	idle := 12.5
	sc := &fakeSched{status: sched.Status{
		Slots:        map[string]string{"a1": "nemotron", "a3": ""},
		SlotLabels:   map[string]string{"a1": "A1"},
		IdleSeconds:  map[string]*float64{"a1": &idle, "a3": nil},
		MemoryBudget: sched.Budget{TotalBytes: 128000 * 1024 * 1024, UsedBytes: 91000 * 1024 * 1024, FreeBytes: 37000 * 1024 * 1024},
		Queue: []sched.Ticket{{TicketID: "q1", Model: "llama", RequestedBy: "agent-x",
			Status: "queued", EnqueuedAt: time.Unix(2000, 0)}},
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/status", goodToken, "{}")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	slots := body["slots"].(map[string]any)
	if slots["a1"] != "nemotron" {
		t.Errorf("a1 slot = %v", slots["a1"])
	}
	if slots["a3"] != nil {
		t.Errorf("empty slot a3 should be null, got %v", slots["a3"])
	}
	mb := body["memory_budget"].(map[string]any)
	if mb["used_bytes"].(float64) != 91000*1024*1024 {
		t.Errorf("used_bytes = %v", mb["used_bytes"])
	}
	if q := body["queue"].([]any); len(q) != 1 {
		t.Errorf("queue len = %d", len(q))
	}
}

func TestStatusNoSched503(t *testing.T) {
	srv := NewWithDeps(Deps{Auth: &fakeAuth{token: goodToken, agent: "agent-x", t: t}})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/status", goodToken, "{}")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
}

// ── can_fit ─────────────────────────────────────────────────────────────────

func TestCanFitHappyPath(t *testing.T) {
	eng := &fakeEngine{fit: engine.CanFit{Fits: true, RequiredBytes: 91000 * 1024 * 1024, FreeBytes: 100000 * 1024 * 1024, Reason: ""}}
	srv := newTestServer(t, &fakeSched{}, eng)
	code, body := do(t, srv, http.MethodPost, "/v1/tools/can_fit", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["fits"] != true {
		t.Errorf("fits = %v", body["fits"])
	}
	if body["required_bytes"].(float64) != 91000*1024*1024 {
		t.Errorf("required_bytes = %v", body["required_bytes"])
	}
}

func TestCanFitBadArgs422(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/can_fit", goodToken, `{"model":"Bad Name!"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
	if body["error"] != "validation_failed" {
		t.Errorf("error = %v", body["error"])
	}
	if _, ok := body["fields"].(map[string]any)["model"]; !ok {
		t.Errorf("missing model field error: %v", body["fields"])
	}
}

func TestCanFitMissingModel422(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/can_fit", goodToken, `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
}

func TestCanFitNoEngine503(t *testing.T) {
	srv := NewWithDeps(Deps{Sched: &fakeSched{}, Auth: &fakeAuth{token: goodToken, agent: "a", t: t}})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/can_fit", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
}

func TestCanFitEngineNotFound404(t *testing.T) {
	eng := &fakeEngine{err: fmt.Errorf("mode %q: %w", "ghost", sched.ErrNotFound)}
	srv := newTestServer(t, &fakeSched{}, eng)
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/can_fit", goodToken, `{"model":"ghost"}`)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// ── ensure_loaded ───────────────────────────────────────────────────────────

func TestEnsureLoadedHappyPath(t *testing.T) {
	sc := &fakeSched{}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken,
		`{"model":"nemotron","token_hint":50000,"target_slot":"a3","timeout":30}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["success"] != true || body["status"] != "loaded" {
		t.Errorf("body = %v", body)
	}
	// requested_by is the agent identity, not "human".
	if sc.lastEnsure.RequestedBy != "agent-x" {
		t.Errorf("requested_by = %q, want agent-x", sc.lastEnsure.RequestedBy)
	}
	if sc.lastEnsure.TargetSlot != "a3" {
		t.Errorf("target_slot = %q", sc.lastEnsure.TargetSlot)
	}
	// token_hint 50000 > threshold 1500 → not a small job.
	if sc.lastEnsure.SmallJob {
		t.Errorf("SmallJob should be false for token_hint 50000")
	}
}

func TestEnsureLoadedSmallJobFromHint(t *testing.T) {
	sc := &fakeSched{}
	srv := newTestServer(t, sc, &fakeEngine{})
	// token_hint 100 <= threshold 1500 → small job.
	do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"llama","token_hint":100}`)
	if !sc.lastEnsure.SmallJob {
		t.Errorf("SmallJob should be true for token_hint 100")
	}
}

func TestEnsureLoadedAbsentHintIsSmall(t *testing.T) {
	sc := &fakeSched{}
	srv := newTestServer(t, sc, &fakeEngine{})
	// Absent hint → 0 → small (V4 parity per SmallJobFromHint).
	do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"llama"}`)
	if !sc.lastEnsure.SmallJob {
		t.Errorf("absent token_hint should be small-job")
	}
}

func TestEnsureLoadedTimeoutIs200(t *testing.T) {
	sc := &fakeSched{ensureFn: func(context.Context, sched.EnsureRequest) (sched.Ticket, error) {
		return sched.Ticket{}, fmt.Errorf("sched: timed out waiting for x to load: %w", context.DeadlineExceeded)
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for timeout", code)
	}
	if body["success"] != false {
		t.Errorf("success = %v, want false", body["success"])
	}
}

func TestEnsureLoadedTimeoutSurfacesRefusalReason(t *testing.T) {
	sc := &fakeSched{
		ensureFn: func(context.Context, sched.EnsureRequest) (sched.Ticket, error) {
			return sched.Ticket{}, fmt.Errorf("sched: timed out waiting for x to load: %w", context.DeadlineExceeded)
		},
		loadStatusFn: func(model string) sched.LoadState {
			return sched.LoadState{Model: model, State: "failed", Reason: sched.ReasonNoEvictableIdle}
		},
	}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for timeout", code)
	}
	if body["reason"] != string(sched.ReasonNoEvictableIdle) {
		t.Errorf("reason = %v, want %q", body["reason"], sched.ReasonNoEvictableIdle)
	}
}

func TestEnsureLoadedEngineError500(t *testing.T) {
	sc := &fakeSched{ensureFn: func(context.Context, sched.EnsureRequest) (sched.Ticket, error) {
		return sched.Ticket{}, errors.New("engine load failed: OOM")
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if body["error"] != "tool_failed" {
		t.Errorf("error = %v", body["error"])
	}
}

func TestEnsureLoadedBadTimeout422(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"x","timeout":9999}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
}

// ── unload ──────────────────────────────────────────────────────────────────

func TestUnloadHappyPath(t *testing.T) {
	sc := &fakeSched{status: sched.Status{Slots: map[string]string{"a3": "nemotron", "a1": ""}}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/unload", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["success"] != true || body["slot"] != "a3" {
		t.Errorf("body = %v", body)
	}
	// Model resolved to slot a3; agent identity passed through.
	if sc.lastUnload[0] != "a3" || sc.lastUnload[1] != "agent-x" {
		t.Errorf("Unload args = %v", sc.lastUnload)
	}
}

func TestUnloadNotLoaded(t *testing.T) {
	sc := &fakeSched{status: sched.Status{Slots: map[string]string{"a3": "llama"}}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/unload", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["success"] != false {
		t.Errorf("success = %v, want false (not loaded)", body["success"])
	}
}

func TestUnloadSchedError500(t *testing.T) {
	sc := &fakeSched{
		status:   sched.Status{Slots: map[string]string{"a3": "nemotron"}},
		unloadFn: func(context.Context, string, string) error { return errors.New("systemctl failed") },
	}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/unload", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

// ── reservations ────────────────────────────────────────────────────────────

func validReservationBody(label string) string {
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"label":%q,"model":"nemotron","start":%q,"end":%q,"scope":"whole_box"}`, label, start, end)
}

func TestListReservations(t *testing.T) {
	sc := &fakeSched{reserv: []sched.Reservation{
		{Label: "a", Model: "nemotron", Scope: "whole_box", CreatedBy: "agent-x",
			Start: time.Unix(1000, 0), End: time.Unix(2000, 0)},
		{Label: "b", Model: "llama", Scope: "bay", Bay: "a1", CreatedBy: "human",
			Start: time.Unix(1000, 0), End: time.Unix(2000, 0)},
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/list_reservations", goodToken, "{}")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["total"].(float64) != 2 {
		t.Errorf("total = %v", body["total"])
	}

	// Filter by model.
	code, body = do(t, srv, http.MethodPost, "/v1/tools/list_reservations?model=llama", goodToken, "{}")
	if code != http.StatusOK || body["total"].(float64) != 1 {
		t.Errorf("filtered total = %v", body["total"])
	}
}

func TestCreateReservationHappyPath(t *testing.T) {
	sc := &fakeSched{}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/create_reservation", goodToken, validReservationBody("nightly"))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if body["ok"] != true || body["label"] != "nightly" {
		t.Errorf("body = %v", body)
	}
	// created_by is the agent identity, and agent-created reservations
	// default open (true/true) via ResolveAgentFlags.
	if sc.lastCreate.CreatedBy != "agent-x" {
		t.Errorf("created_by = %q, want agent-x", sc.lastCreate.CreatedBy)
	}
	if !sc.lastCreate.AllowAgentReschedule || !sc.lastCreate.AllowAgentCancellation {
		t.Errorf("agent reservation should default open, got resched=%v cancel=%v",
			sc.lastCreate.AllowAgentReschedule, sc.lastCreate.AllowAgentCancellation)
	}
}

func TestCreateReservationExplicitFlagOverridesDefault(t *testing.T) {
	sc := &fakeSched{}
	srv := newTestServer(t, sc, &fakeEngine{})
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	b := fmt.Sprintf(`{"label":"x","model":"nemotron","start":%q,"end":%q,"scope":"whole_box","allow_agent_cancellation":false}`, start, end)
	do(t, srv, http.MethodPost, "/v1/tools/create_reservation", goodToken, b)
	if sc.lastCreate.AllowAgentCancellation {
		t.Errorf("explicit false should override agent default-open")
	}
	if !sc.lastCreate.AllowAgentReschedule {
		t.Errorf("absent reschedule should still default open for agent")
	}
}

func TestCreateReservationConflict409(t *testing.T) {
	sc := &fakeSched{createFn: func(context.Context, sched.Reservation) error {
		return fmt.Errorf("sched: reservation %q already exists: %w", "dup", sched.ErrConflict)
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/create_reservation", goodToken, validReservationBody("dup"))
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", code)
	}
	if body["error"] != "conflict" {
		t.Errorf("error = %v", body["error"])
	}
}

func TestCreateReservationBadArgs422(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	// scope=bay but no bay set.
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	b := fmt.Sprintf(`{"label":"x","model":"nemotron","start":%q,"end":%q,"scope":"bay"}`, start, end)
	code, body := do(t, srv, http.MethodPost, "/v1/tools/create_reservation", goodToken, b)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
	if _, ok := body["fields"].(map[string]any)["bay"]; !ok {
		t.Errorf("missing bay field error: %v", body["fields"])
	}
}

func TestCreateReservationMalformedJSON400(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/create_reservation", goodToken, `{not json`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestUpdateReservationHappyPath(t *testing.T) {
	sc := &fakeSched{}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/update_reservation?label=nightly", goodToken, validReservationBody("nightly"))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["label"] != "nightly" {
		t.Errorf("label = %v", body["label"])
	}
	// Requester identity travels in r.CreatedBy for updates.
	if sc.lastUpdate.CreatedBy != "agent-x" {
		t.Errorf("update requester = %q, want agent-x", sc.lastUpdate.CreatedBy)
	}
}

func TestUpdateReservationMissingLabel400(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	// Body with no label field and no ?label= query param.
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	b := fmt.Sprintf(`{"model":"nemotron","start":%q,"end":%q,"scope":"whole_box"}`, start, end)
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/update_reservation", goodToken, b)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestUpdateReservationNotFound404(t *testing.T) {
	sc := &fakeSched{updateFn: func(context.Context, string, sched.Reservation) error {
		return fmt.Errorf("sched: reservation %q: %w", "ghost", sched.ErrNotFound)
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/update_reservation?label=ghost", goodToken, validReservationBody("ghost"))
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestUpdateReservationPermissionDenied403(t *testing.T) {
	sc := &fakeSched{updateFn: func(context.Context, string, sched.Reservation) error {
		return fmt.Errorf("sched: %q may not reschedule: %w", "agent-x", sched.ErrPermissionDenied)
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/update_reservation?label=other", goodToken, validReservationBody("other"))
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	if body["error"] != "permission_denied" {
		t.Errorf("error = %v", body["error"])
	}
}

func TestCancelReservationHappyPath(t *testing.T) {
	sc := &fakeSched{}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, body := do(t, srv, http.MethodPost, "/v1/tools/cancel_reservation", goodToken, `{"label":"nightly"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["ok"] != true {
		t.Errorf("body = %v", body)
	}
	if sc.lastCancel[0] != "nightly" || sc.lastCancel[1] != "agent-x" {
		t.Errorf("cancel args = %v, want [nightly agent-x]", sc.lastCancel)
	}
}

func TestCancelReservationMissingLabel422(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/cancel_reservation", goodToken, `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
}

func TestCancelReservationPermissionDenied403(t *testing.T) {
	sc := &fakeSched{cancelFn: func(context.Context, string, string) error {
		return fmt.Errorf("sched: %q may not cancel: %w", "agent-x", sched.ErrPermissionDenied)
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/cancel_reservation", goodToken, `{"label":"other"}`)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
}

func TestCancelReservationNotFound404(t *testing.T) {
	sc := &fakeSched{cancelFn: func(context.Context, string, string) error {
		return fmt.Errorf("sched: reservation %q: %w", "ghost", sched.ErrNotFound)
	}}
	srv := newTestServer(t, sc, &fakeEngine{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/cancel_reservation", goodToken, `{"label":"ghost"}`)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// ── audit (roadmap R3) ─────────────────────────────────────────────────────

// newAuditedServer wires a recording fakeAudit into the standard test
// server; the authenticated identity is agent-x per fakeAuth.
func newAuditedServer(t *testing.T, sc sched.Scheduler) (*Server, *fakeAudit) {
	t.Helper()
	fa := &fakeAudit{}
	srv := newTestServer(t, sc, &fakeEngine{})
	srv.deps.Audit = fa
	return srv, fa
}

// assertAuditEntry checks that exactly one entry was written with the
// expected action/target, actor = authenticated key name (never "human"),
// and the caller address recorded.
func assertAuditEntry(t *testing.T, fa *fakeAudit, action, target string) store.AuditEntry {
	t.Helper()
	if len(fa.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1: %v", len(fa.entries), fa.entries)
	}
	e := fa.entries[0]
	if e.Action != action {
		t.Errorf("audit action = %q, want %q", e.Action, action)
	}
	if e.Target != target {
		t.Errorf("audit target = %q, want %q", e.Target, target)
	}
	if e.Actor != "agent-x" {
		t.Errorf("audit actor = %q, want the authenticated key name agent-x", e.Actor)
	}
	if e.RemoteAddr == "" {
		t.Errorf("audit remote_addr not recorded")
	}
	return e
}

func TestEnsureLoadedWritesAudit(t *testing.T) {
	srv, fa := newAuditedServer(t, &fakeSched{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	e := assertAuditEntry(t, fa, "mcp_ensure_loaded", "nemotron")
	if e.Detail != "status=loaded" {
		t.Errorf("detail = %q, want status=loaded", e.Detail)
	}
}

// A load timeout is a 200 success:false outcome, but the load is still in
// flight — it must be audited, with the timeout recorded in the detail.
func TestEnsureLoadedTimeoutWritesAudit(t *testing.T) {
	sc := &fakeSched{ensureFn: func(context.Context, sched.EnsureRequest) (sched.Ticket, error) {
		return sched.Ticket{}, fmt.Errorf("sched: timed out waiting for nemotron to load: %w", context.DeadlineExceeded)
	}}
	srv, fa := newAuditedServer(t, sc)
	code, body := do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK || body["success"] != false {
		t.Fatalf("status = %d, body = %v (timeout is a normal 200)", code, body)
	}
	e := assertAuditEntry(t, fa, "mcp_ensure_loaded", "nemotron")
	if e.Detail != "status=timeout" {
		t.Errorf("detail = %q, want status=timeout", e.Detail)
	}
}

func TestUnloadWritesAudit(t *testing.T) {
	sc := &fakeSched{status: sched.Status{Slots: map[string]string{"a3": "nemotron"}}}
	srv, fa := newAuditedServer(t, sc)
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/unload", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	e := assertAuditEntry(t, fa, "mcp_unload", "nemotron")
	if e.Detail != "slot=a3" {
		t.Errorf("detail = %q, want slot=a3", e.Detail)
	}
}

func TestCreateReservationWritesAudit(t *testing.T) {
	srv, fa := newAuditedServer(t, &fakeSched{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/create_reservation", goodToken, validReservationBody("nightly"))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	assertAuditEntry(t, fa, "mcp_reservation_create", "nightly")
}

func TestUpdateReservationWritesAudit(t *testing.T) {
	srv, fa := newAuditedServer(t, &fakeSched{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/update_reservation?label=nightly", goodToken, validReservationBody("nightly"))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	assertAuditEntry(t, fa, "mcp_reservation_update", "nightly")
}

func TestCancelReservationWritesAudit(t *testing.T) {
	srv, fa := newAuditedServer(t, &fakeSched{})
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/cancel_reservation", goodToken, `{"label":"nightly"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	assertAuditEntry(t, fa, "mcp_reservation_cancel", "nightly")
}

// A scheduler-rejected mutation did not change the fleet, so it is not
// audited — the audit log records mutations that actually happened.
func TestRejectedMutationWritesNoAudit(t *testing.T) {
	sc := &fakeSched{cancelFn: func(context.Context, string, string) error {
		return fmt.Errorf("sched: reservation %q: %w", "ghost", sched.ErrNotFound)
	}}
	srv, fa := newAuditedServer(t, sc)
	code, _ := do(t, srv, http.MethodPost, "/v1/tools/cancel_reservation", goodToken, `{"label":"ghost"}`)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if len(fa.entries) != 0 {
		t.Errorf("rejected mutation should not be audited, got %v", fa.entries)
	}
}

// Audit write failures never fail the tool call (best-effort, httpapi
// parity).
func TestAuditFailureDoesNotFailTool(t *testing.T) {
	srv := newTestServer(t, &fakeSched{}, &fakeEngine{})
	srv.deps.Audit = &fakeAudit{err: errors.New("db locked")}
	code, body := do(t, srv, http.MethodPost, "/v1/tools/ensure_loaded", goodToken, `{"model":"nemotron"}`)
	if code != http.StatusOK || body["success"] != true {
		t.Fatalf("audit failure leaked into tool result: status = %d, body = %v", code, body)
	}
}

// Nil Audit (skeleton / unwired store) degrades gracefully: every mutating
// tool still succeeds, just unaudited.
func TestNilAuditDegradesGracefully(t *testing.T) {
	sc := &fakeSched{status: sched.Status{Slots: map[string]string{"a3": "nemotron"}}}
	srv := newTestServer(t, sc, &fakeEngine{}) // Deps.Audit deliberately nil
	calls := []struct{ path, body string }{
		{"/v1/tools/ensure_loaded", `{"model":"llama"}`},
		{"/v1/tools/unload", `{"model":"nemotron"}`},
		{"/v1/tools/create_reservation", validReservationBody("nil-audit")},
		{"/v1/tools/update_reservation?label=nil-audit", validReservationBody("nil-audit")},
		{"/v1/tools/cancel_reservation", `{"label":"nil-audit"}`},
	}
	for _, call := range calls {
		code, _ := do(t, srv, http.MethodPost, call.path, goodToken, call.body)
		if code != http.StatusOK && code != http.StatusCreated {
			t.Errorf("nil Audit: %s = %d, want 2xx", call.path, code)
		}
	}
}

// engine.Engine (the real interface) satisfies the CanFitter seam.
func TestEngineSatisfiesCanFitter(t *testing.T) {
	var _ CanFitter = (*engine.Stub)(nil)
}
