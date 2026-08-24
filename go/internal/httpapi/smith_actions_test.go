// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"github.com/jsaigou/the-forge/internal/store"
)

// ── fixtures ─────────────────────────────────────────────────────────────────

// actionsStubSched overrides Status on the stock sched.Stub so Brain() can
// resolve a local_slot brain deterministically (same pattern as
// internal/smith's own stubSched — duplicated here rather than exported,
// since smith's test helpers are unexported to their package).
type actionsStubSched struct {
	*sched.Stub
	status sched.Status
}

func (s *actionsStubSched) Status() sched.Status { return s.status }

func newActionsStubSched(slots map[string]string) *actionsStubSched {
	labels := map[string]string{}
	for slot := range slots {
		labels[slot] = strings.ToUpper(slot)
	}
	return &actionsStubSched{
		Stub: &sched.Stub{},
		status: sched.Status{
			Slots:      slots,
			SlotLabels: labels,
			MemoryBudget: sched.Budget{
				TotalBytes: 120 << 30, UsedBytes: 10 << 30, FreeBytes: 110 << 30,
			},
		},
	}
}

// fakePlacer is a minimal smith.Placer for execution-path tests. Unload and
// Load both report success unconditionally — the point of these tests is
// the HTTP surface's handling of the resulting state transitions, not the
// dispatch logic itself (already covered exhaustively by internal/smith's
// own tests).
type fakePlacer struct {
	slots []string
}

var _ smith.Placer = (*fakePlacer)(nil)

func (p *fakePlacer) FitPlan(string) (engine.Plan, error) { return engine.Plan{}, nil }
func (p *fakePlacer) Load(_ context.Context, mode, slot string) engine.Result {
	return engine.Result{Success: true, Message: "loaded", NCtx: 1024}
}
func (p *fakePlacer) Unload(_ context.Context, slot string) engine.Result {
	return engine.Result{Success: true, Message: "unloaded"}
}
func (p *fakePlacer) Slots() []string { return p.slots }

// seedActionsCatalog seeds a minimal real catalog with one local Config
// ("ornith-35b") so Brain() can resolve it as a local_slot brain when paired
// with a sched status placing it on a slot.
func seedActionsCatalog(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, err := cat.CreateModel(ctx, store.Model{Name: "ornith"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "35b"})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	gguf, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName: %v", err)
	}
	artID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: gguf.ID, ArtifactType: "weight",
		FilePath: "ornith-35b.gguf", FileSizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	if _, err := cat.CreateConfig(ctx, store.Config{
		Name: "ornith-35b", VariantID: varID, WeightArtifactID: artID, EngineID: eng.ID,
		NCtx: 262144, Parallel: 1, Status: "unverified", Visibility: "visible",
	}); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
}

// serverWithSmithActions builds a test Server with a real in-memory
// store.DB, a wired smith.Smith (with Store/Catalog/Settings/Sched/Placer so
// self-eviction stamping + execution both work), and bearer-key auth (which
// skips requireAssurance — see TestSmithActionCreateStepUp for the session-
// based path that path deliberately can't cover).
func serverWithSmithActions(t *testing.T, ident authz.Identity) (*Server, *store.DB) {
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

// setBrainOnSlot sets smith.model so Brain() resolves local_slot on the
// given slot (the actionsStubSched wired by serverWithSmithActions already
// places "ornith-35b" on "a2").
func setBrainOnSlot(t *testing.T, db *store.DB) {
	t.Helper()
	if err := db.Settings().Set(context.Background(), smith.SettingModel, []byte(`"ornith-35b"`)); err != nil {
		t.Fatalf("set smith.model: %v", err)
	}
}

// createAction is a small helper for POSTing a well-formed action-create
// body and decoding the response.
func createAction(t *testing.T, s *Server, kind, risk string, detail map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body := map[string]any{"kind": kind, "title": "test action", "risk": risk}
	if detail != nil {
		body["detail"] = detail
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	w := do(t, s, authedRequest("POST", "/api/v1/smith/actions", bytes.NewReader(b)))
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	return w, resp
}

// ── 503 (Smith not wired) ───────────────────────────────────────────────────

func TestSmithActionsUnwired(t *testing.T) {
	s := newTestServer(t) // no Smith wired
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/smith/actions"},
		{"POST", "/api/v1/smith/actions"},
		{"GET", "/api/v1/smith/actions/1"},
		{"POST", "/api/v1/smith/actions/1/approve"},
		{"POST", "/api/v1/smith/actions/1/reject"},
		{"POST", "/api/v1/smith/actions/1/handoff"},
		{"POST", "/api/v1/smith/actions/1/recheck"},
		{"GET", "/api/v1/smith/actions/1/procedure"},
		{"POST", "/api/v1/smith/actions/1/procedure/checkpoint/approve"},
		{"POST", "/api/v1/smith/actions/1/procedure/checkpoint/abort"},
		{"GET", "/api/v1/smith/actions/1/procedure_preview"},
		{"POST", "/api/v1/smith/actions/1/procedurize"},
	} {
		w := do(t, s, authedRequest(route.method, route.path, bytes.NewBufferString(`{}`)))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", route.method, route.path, w.Code)
		}
	}
}

// ── 404 unknown id ───────────────────────────────────────────────────────────

func TestSmithActionDetailNotFound(t *testing.T) {
	s, _ := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/actions/999999", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET unknown action = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// ── 400 bad kind/risk/resolution ────────────────────────────────────────────

func TestSmithActionCreateBadKindOrRisk(t *testing.T) {
	s, _ := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w, _ := createAction(t, s, "not_a_real_kind", smith.RiskLow, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad kind = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	w, _ = createAction(t, s, smith.KindRunbook, "not_a_real_risk", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad risk = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestSmithActionHandoffBadResolution(t *testing.T) {
	s, _ := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("POST", "/api/v1/smith/actions/1/handoff",
		bytes.NewBufferString(`{"resolution":"teleport"}`)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad resolution = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ── 409 invalid_state ────────────────────────────────────────────────────────

func TestSmithActionApproveTwiceIsInvalidState(t *testing.T) {
	s, _ := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	// A runbook action short-circuits pending -> done_unverified on its
	// first approve (never lands in "approved") — a second approve must
	// hit the CAS's "0 rows" path and come back invalid_state.
	_, created := createAction(t, s, smith.KindRunbook, smith.RiskInfo, nil)
	action := created["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w := do(t, s, authedRequest("POST", "/api/v1/smith/actions/"+itoa(id)+"/approve", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("first approve (runbook) = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	w = do(t, s, authedRequest("POST", "/api/v1/smith/actions/"+itoa(id)+"/approve", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var errResp map[string]string
	decodeJSON(t, w.Body, &errResp)
	if errResp["error"] != "invalid_state" {
		t.Errorf("error = %q, want invalid_state", errResp["error"])
	}
}

// ── Sprint S4: runbook re-check (§5.5) ─────────────────────────────────────

func TestSmithActionRecheck(t *testing.T) {
	s, _ := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	// A runbook with no source check and no investigation reports the honest
	// "no check to re-verify" answer (no checks run).
	w, created := createAction(t, s, smith.KindRunbook, smith.RiskInfo, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create runbook = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	action := created["action"].(map[string]any)
	id := int64(action["id"].(float64))

	// Re-check before "done — I ran it myself" is invalid state.
	w = do(t, s, authedRequest("POST", "/api/v1/smith/actions/"+itoa(id)+"/recheck", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("recheck before approve = %d, want 409; body=%s", w.Code, w.Body.String())
	}

	// Approve -> done_unverified, then re-check.
	w = do(t, s, authedRequest("POST", "/api/v1/smith/actions/"+itoa(id)+"/approve", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("approve runbook = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	w = do(t, s, authedRequest("POST", "/api/v1/smith/actions/"+itoa(id)+"/recheck", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("recheck = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	re := resp["action"].(map[string]any)
	if re["status"] != smith.StatusDoneUnverified {
		t.Errorf("status = %v, want done_unverified", re["status"])
	}
	result := re["result"].(map[string]any)
	if msg, _ := result["message"].(string); !strings.Contains(msg, "no check to re-verify") {
		t.Errorf("result.message = %q, want the honest no-check answer", msg)
	}
}

// ── 409 handoff_required (exact body keys) + the full handoff loop ─────────

func TestSmithActionHandoffRequiredAndFullLoop(t *testing.T) {
	s, db := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	setBrainOnSlot(t, db)

	// unload_slot targeting the brain's own slot ("a2") is the explicit
	// self-eviction case (handoff.go's stampSelfEviction case 2) — needs no
	// Placer/FitPlan to trigger.
	w, created := createAction(t, s, smith.KindUnloadSlot, smith.RiskHigh, map[string]any{"slot": "a2"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create self-evicting action = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	action := created["action"].(map[string]any)
	id := int64(action["id"].(float64))
	if action["self_evicting"] != true {
		t.Fatalf("action not stamped self_evicting: %+v", action)
	}
	handoff := action["handoff"].(map[string]any)
	if handoff["state"] != smith.HandoffRequired {
		t.Fatalf("handoff.state = %v, want required", handoff["state"])
	}

	path := "/api/v1/smith/actions/" + itoa(id)

	// Approve -> 409 handoff_required with the exact frozen body keys.
	w = do(t, s, authedRequest("POST", path+"/approve", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("approve before handoff = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var handoffBody map[string]any
	decodeJSON(t, w.Body, &handoffBody)
	for _, key := range []string{"error", "action_id", "handoff", "message"} {
		if _, ok := handoffBody[key]; !ok {
			t.Errorf("409 body missing key %q: %+v", key, handoffBody)
		}
	}
	if handoffBody["error"] != "handoff_required" {
		t.Errorf("error = %v, want handoff_required", handoffBody["error"])
	}
	if int64(handoffBody["action_id"].(float64)) != id {
		t.Errorf("action_id = %v, want %d", handoffBody["action_id"], id)
	}

	// Resolve "runbook" -> runbook_issued, with real rendered steps.
	w = do(t, s, authedRequest("POST", path+"/handoff", bytes.NewBufferString(`{"resolution":"runbook"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("handoff runbook = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var afterRunbook map[string]any
	decodeJSON(t, w.Body, &afterRunbook)
	a := afterRunbook["action"].(map[string]any)
	h := a["handoff"].(map[string]any)
	if h["state"] != smith.HandoffRunbookIssued {
		t.Fatalf("handoff.state after runbook = %v, want runbook_issued", h["state"])
	}
	steps, _ := h["runbook"].([]any)
	if len(steps) == 0 {
		t.Fatalf("runbook has no steps: %+v", h)
	}

	// Approve again -> STILL 409 handoff_required (issuing the runbook alone
	// does not unblock approval).
	w = do(t, s, authedRequest("POST", path+"/approve", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("approve after runbook-issued = %d, want 409 (still blocked); body=%s", w.Code, w.Body.String())
	}

	// Resolve "acknowledge" -> acknowledged.
	w = do(t, s, authedRequest("POST", path+"/handoff", bytes.NewBufferString(`{"resolution":"acknowledge"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("handoff acknowledge = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Approve now succeeds (202 — execution continues async in the
	// background) and the action eventually reaches a terminal or
	// executing state.
	w = do(t, s, authedRequest("POST", path+"/approve", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("approve after acknowledge = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	waitForTerminalOrExecuting(t, s, id)
}

// ── remote handoff (P3) ────────────────────────────────────────────────────

// TestSmithActionHandoffRemoteNoCandidate exercises the httpapi path when
// probeHandoffCandidates finds nothing to offer: seedActionsCatalog creates
// no Offering at all, so the "remote" resolution is a plain 400 rather than
// P2's blanket 501.
func TestSmithActionHandoffRemoteNoCandidate(t *testing.T) {
	s, db := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	setBrainOnSlot(t, db)

	rec, resp := createAction(t, s, smith.KindLoadConfig, smith.RiskLow,
		map[string]any{"mode": "big-model", "slot": "a2"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	action := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w := do(t, s, authedRequest("POST", "/api/v1/smith/actions/"+strconv.FormatInt(id, 10)+"/handoff",
		bytes.NewBufferString(`{"resolution":"remote"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("handoff remote (no candidate) = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestSmithActionHandoffRemoteSwapsAndApproves is the full positive path: a
// healthy candidate is available, "remote" swaps smith.model and moves the
// handoff to remote_swapped, and approval — previously blocked — now
// succeeds without ever visiting the runbook path.
func TestSmithActionHandoffRemoteSwapsAndApproves(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, err := cat.CreateModel(ctx, store.Model{Name: "ornith"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "35b"})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	gguf, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName: %v", err)
	}
	artID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: gguf.ID, ArtifactType: "weight",
		FilePath: "ornith-35b.gguf", FileSizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	if _, err := cat.CreateConfig(ctx, store.Config{
		Name: "ornith-35b", VariantID: varID, WeightArtifactID: artID, EngineID: eng.ID,
		NCtx: 262144, Parallel: 1, Status: "unverified", Visibility: "visible",
	}); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{Name: "deepseek", APIKey: "sk-test"}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseek, _, err := db.Routing().ProviderByName(ctx, "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}
	if _, err := cat.CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseek.ID, WireModel: "deepseek-chat",
		Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}
	if err := db.Settings().Set(ctx, smith.SettingModel, []byte(`"ornith-35b"`)); err != nil {
		t.Fatalf("set smith.model: %v", err)
	}

	events := bus.New()
	placer := &fakePlacer{slots: []string{"a1", "a2", "a3", "a4"}}
	agent := smith.New(smith.Deps{
		Store: db, Catalog: db.Catalog(), Settings: db.Settings(),
		Sched:       newActionsStubSched(map[string]string{"a2": "ornith-35b"}),
		Placer:      placer,
		RestartUnit: func(context.Context, string) error { return nil },
		Source:      collector.NewStatic(nil),
		Publisher:   events, Subscriber: events, Audit: db.Audit(),
		ProviderHealth: func(context.Context, string) (string, error) { return "reachable", nil },
	})
	agentCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	agent.Start(agentCtx)
	t.Cleanup(agent.Stop)

	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil), Engine: &engine.Stub{}, Sched: &sched.Stub{},
		Auth:   &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events: events, Publish: events, Config: func() *config.Config { return cfg },
		Hostname: "test-host", Smith: agent, Audit: db.Audit(),
	})
	t.Cleanup(func() { s.Close() })

	rec, resp := createAction(t, s, smith.KindLoadConfig, smith.RiskLow,
		map[string]any{"mode": "big-model", "slot": "a2"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	action := resp["action"].(map[string]any)
	id := int64(action["id"].(float64))
	path := "/api/v1/smith/actions/" + strconv.FormatInt(id, 10)

	w := do(t, s, authedRequest("POST", path+"/handoff", bytes.NewBufferString(`{"resolution":"remote"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("handoff remote = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	raw, err := db.Settings().Get(ctx, smith.SettingModel)
	if err != nil {
		t.Fatalf("read smith.model: %v", err)
	}
	var model string
	json.Unmarshal(raw, &model)
	if model != "deepseek-chat" {
		t.Errorf("smith.model = %q, want deepseek-chat", model)
	}

	w = do(t, s, authedRequest("POST", path+"/approve", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("approve after remote swap = %d, want 202; body=%s", w.Code, w.Body.String())
	}
}

// ── 202-or-200 on a non-self-evicting approve, with a real terminal state ──

func TestSmithActionApproveExecutesAndReachesTerminalState(t *testing.T) {
	s, _ := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	// unload_slot on a3 — not the brain's slot (brain unset in this test,
	// so nothing is self-evicting regardless), so approve executes
	// immediately via the fakePlacer.
	w, created := createAction(t, s, smith.KindUnloadSlot, smith.RiskLow, map[string]any{"slot": "a3"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	action := created["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w = do(t, s, authedRequest("POST", "/api/v1/smith/actions/"+itoa(id)+"/approve", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("approve = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	final := waitForTerminalOrExecuting(t, s, id)
	// The fakePlacer always succeeds, so dispatch never fails — the final
	// state must not be "failed".
	if final.Status == smith.StatusFailed {
		t.Errorf("final status = failed, want a successful terminal state; result=%+v", final.Result)
	}
}

// waitForTerminalOrExecuting polls GET /api/v1/smith/actions/{id} until the
// status leaves "approved" (executeAction's goroutine runs in-process
// against an in-memory DB, so this resolves near-instantly in practice) or
// the deadline expires.
func waitForTerminalOrExecuting(t *testing.T, s *Server, id int64) smith.Action {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w := do(t, s, authedRequest("GET", "/api/v1/smith/actions/"+itoa(id), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET action %d = %d, want 200; body=%s", id, w.Code, w.Body.String())
		}
		var resp struct {
			Action smith.Action `json:"action"`
		}
		decodeJSON(t, w.Body, &resp)
		switch resp.Action.Status {
		case smith.StatusExecuting, smith.StatusDone, smith.StatusDoneUnverified, smith.StatusFailed:
			return resp.Action
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("action %d never left 'approved' within the deadline", id)
	return smith.Action{}
}

// ── 403 step_up_required ────────────────────────────────────────────────────

// serverWithSmithActionsAuth builds a Server with the real authz stack
// (Authorizer + PolicyStore + network identity bootstrap), matching
// newAuthTestServer's pattern in auth_test.go, plus a wired Smith agent.
// authedRequest's bearer-key path deliberately skips requireAssurance, so
// the step-up gate can only be exercised through a real session — this is
// that path.
func serverWithSmithActionsAuth(t *testing.T) (*Server, *authz.Authorizer, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	seedActionsCatalog(t, db)

	auth := authz.New(db)
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})

	placer := &fakePlacer{slots: []string{"a1", "a2", "a3", "a4"}}
	agent := smith.New(smith.Deps{
		Store:       db,
		Catalog:     db.Catalog(),
		Settings:    db.Settings(),
		Sched:       newActionsStubSched(map[string]string{"a2": "ornith-35b"}),
		Placer:      placer,
		RestartUnit: func(context.Context, string) error { return nil },
		Source:      collector.NewStatic(nil),
		Publisher:   events,
		Subscriber:  events,
		Audit:       db.Audit(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	agent.Start(ctx)
	t.Cleanup(agent.Stop)

	policyStore := authz.NewPolicyStore(authz.SettingsAdapter{
		GetFn: func(ctx context.Context, key string) ([]byte, error) { return db.Settings().Get(ctx, key) },
		SetFn: func(ctx context.Context, key string, value []byte) error { return db.Settings().Set(ctx, key, value) },
	})

	s := New(Deps{
		Snapshots:          collector.NewStatic(nil),
		Auth:               auth,
		AuthSetup:          auth,
		Events:             events,
		Publish:            events,
		Engine:             &engine.Stub{},
		Sched:              &sched.Stub{},
		Config:             func() *config.Config { return cfg },
		Hostname:           "test-host",
		Sessions:           db.Sessions(),
		Settings:           db.Settings(),
		Audit:              db.Audit(),
		IdentityLinks:      db.IdentityLinks(),
		TOTPStore:          db.TOTP(),
		Keys:               db.Keys(),
		PolicyStore:        policyStore,
		StepUpTTL:          authz.DefaultStepUpTTL,
		NetworkDefaultRole: authz.RoleOperator,
		StepUpVerifier:     auth,
		KeyManager:         auth,
		NetworkIdentity:    authz.NoNetworkIdentity{},
		Smith:              agent,
	})
	t.Cleanup(func() { s.Close() })
	return s, auth, db
}

func TestSmithActionCreateAndApproveStepUpRequired(t *testing.T) {
	s, auth, db := serverWithSmithActionsAuth(t)

	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleOperator); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	uid := getUserID(t, db, "testuser")
	if err := db.IdentityLinks().Create(context.Background(), store.IdentityLink{
		Provider: "tailscale", Principal: "user@github", UserID: uid, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("IdentityLinks.Create: %v", err)
	}
	s.deps.NetworkIdentity = &authz.TailscaleIdentityProvider{
		Client: &fakeWhoIsClientHTTP{login: "user@github"},
	}

	// Bootstrap an L0 (network-only) session.
	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.RemoteAddr = "100.100.100.100:54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie after network bootstrap")
	}

	// POST /api/v1/smith/actions at L0 (network) assurance -> 403
	// step_up_required (action.smith.execute defaults to password).
	body := bytes.NewBufferString(`{"kind":"runbook","title":"t","risk":"info"}`)
	req = httptest.NewRequest("POST", "/api/v1/smith/actions", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFToken(t, s, cookie))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create at L0 = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	var errResp map[string]string
	json.NewDecoder(rec.Body).Decode(&errResp)
	if errResp["error"] != "step_up_required" {
		t.Errorf("error = %q, want step_up_required", errResp["error"])
	}
	if errResp["required"] != "password" {
		t.Errorf("required = %q, want password", errResp["required"])
	}

	// Same gate on approve — create one action directly through the smith
	// package (bypassing the gated create route) so we can isolate the
	// approve gate.
	a, err := s.deps.Smith.CreateAction(context.Background(), smith.ActionDraft{
		Kind: smith.KindRunbook, Title: "t", Risk: smith.RiskInfo, CreatedBy: "testuser",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	req = httptest.NewRequest("POST", "/api/v1/smith/actions/"+itoa(a.ID)+"/approve", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", getCSRFToken(t, s, cookie))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("approve at L0 = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// ── Audit rows ───────────────────────────────────────────────────────────────

func TestSmithActionAuditRows(t *testing.T) {
	s, db := serverWithSmithActions(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	_, created := createAction(t, s, smith.KindRunbook, smith.RiskInfo, nil)
	action := created["action"].(map[string]any)
	id := int64(action["id"].(float64))

	w := do(t, s, authedRequest("POST", "/api/v1/smith/actions/"+itoa(id)+"/approve", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	entries, err := db.Audit().List(context.Background(), "smith_action_", "", 100)
	if err != nil {
		t.Fatalf("Audit.List: %v", err)
	}
	var sawCreate, sawApprove bool
	for _, e := range entries {
		if e.Action == "smith_action_create" && e.Actor == "operator" {
			sawCreate = true
		}
		if e.Action == "smith_action_approve" && e.Actor == "operator" {
			sawApprove = true
		}
	}
	if !sawCreate {
		t.Errorf("no smith_action_create audit row for actor operator: %+v", entries)
	}
	if !sawApprove {
		t.Errorf("no smith_action_approve audit row for actor operator: %+v", entries)
	}
}
