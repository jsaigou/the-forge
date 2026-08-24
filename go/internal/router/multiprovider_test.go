// SPDX-License-Identifier: Apache-2.0

package router

// multiprovider_test.go — the "same model on several providers" edge case
// (multi-provider routing sprint, 2026-08-06): offerings of one catalog
// Model form a group, the operator's offerings.priority picks the PRIMARY,
// a disabled provider drops out of the group without deleting anything,
// /v1/models lists the group once, and router.provider_failover controls
// whether a failed primary falls through to the next provider.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// newRecordingUpstream serves a marker chat-completion payload and records
// the model name it was asked for (the wire_model the router rewrote).
func newRecordingUpstream(t *testing.T, marker string, status int) (*httptest.Server, *atomic.Value) {
	t.Helper()
	lastModel := &atomic.Value{}
	lastModel.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		lastModel.Store(req.Model)
		w.WriteHeader(status)
		if status < 400 {
			w.Write([]byte(`{"id":"` + marker + `","object":"chat.completion","choices":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, lastModel
}

// seedGLMGroup reproduces the live glm-5.2 shape: one catalog model offered
// by aiand (wire "zai-org/glm-5.2") and qwen (wire "glm-5.2").
func seedGLMGroup(t *testing.T, db *store.DB, aiandURL, qwenURL string, qwenPriority int, qwenEnabled bool) {
	t.Helper()
	ctx := context.Background()
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "aiand", APIKey: "sk-a", TargetURL: aiandURL, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveProvider(aiand): %v", err)
	}
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "qwen", APIKey: "sk-q", TargetURL: qwenURL, Enabled: qwenEnabled,
	}); err != nil {
		t.Fatalf("SaveProvider(qwen): %v", err)
	}
	aiand, _, err := db.Routing().ProviderByName(ctx, "aiand")
	if err != nil {
		t.Fatalf("ProviderByName(aiand): %v", err)
	}
	qwen, _, err := db.Routing().ProviderByName(ctx, "qwen")
	if err != nil {
		t.Fatalf("ProviderByName(qwen): %v", err)
	}
	mdlID, err := db.Catalog().CreateModel(ctx, store.Model{Name: "glm-5.2"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if _, err := db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: aiand.ID, WireModel: "zai-org/glm-5.2",
		Enabled: true, Priority: 100,
	}); err != nil {
		t.Fatalf("CreateOffering(aiand): %v", err)
	}
	if _, err := db.Catalog().CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: qwen.ID, WireModel: "glm-5.2",
		Enabled: true, Priority: qwenPriority,
	}); err != nil {
		t.Fatalf("CreateOffering(qwen): %v", err)
	}
}

func newMultiProviderServer(t *testing.T, db *store.DB, settings *fakeSettings) *Server {
	t.Helper()
	return NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Routing:     db.Routing(),
		Settings:     settings,
		Auth:         &stubAuth{validToken: "x"},
	})
}

func chatForModel(srv *Server, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestMultiProvider_PrioritySelectionAndAliasConvergence: qwen's offering
// has priority 10, aiand's 100 — both the canonical name and aiand's own
// wire alias must converge on qwen, with the wire name rewritten.
func TestMultiProvider_PrioritySelectionAndAliasConvergence(t *testing.T) {
	aiandSrv, aiandModel := newRecordingUpstream(t, "aiand", 200)
	qwenSrv, qwenModel := newRecordingUpstream(t, "qwen", 200)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedGLMGroup(t, db, aiandSrv.URL, qwenSrv.URL, 10, true)
	srv := newMultiProviderServer(t, db, newFakeSettings())

	// Canonical name → qwen (priority 10 beats 100).
	rec := chatForModel(srv, "glm-5.2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"qwen"`) {
		t.Fatalf("glm-5.2: code=%d body=%s, want qwen to serve", rec.Code, rec.Body.String())
	}
	if got := qwenModel.Load(); got != "glm-5.2" {
		t.Errorf("qwen saw model %q, want glm-5.2", got)
	}

	// aiand's alias converges on the same primary — the wire name is
	// rewritten to qwen's, aiand never sees the request.
	rec = chatForModel(srv, "zai-org/glm-5.2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"qwen"`) {
		t.Fatalf("alias: code=%d body=%s, want qwen to serve", rec.Code, rec.Body.String())
	}
	if got := qwenModel.Load(); got != "glm-5.2" {
		t.Errorf("alias: qwen saw model %q, want the rewritten glm-5.2", got)
	}
	if got := aiandModel.Load(); got != "" {
		t.Errorf("aiand saw model %q, want no traffic at all", got)
	}

	// /v1/models lists the group exactly once, under the primary.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.RemoteAddr = "100.64.0.1:1234"
	srv.Handler().ServeHTTP(rec, req)
	var models ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	glmEntries := []ModelEntry{}
	for _, e := range models.Data {
		if e.ID == "glm-5.2" || e.ID == "zai-org/glm-5.2" {
			glmEntries = append(glmEntries, e)
		}
	}
	if len(glmEntries) != 1 {
		t.Fatalf("/v1/models glm entries = %+v, want exactly one", glmEntries)
	}
	if glmEntries[0].ID != "glm-5.2" || glmEntries[0].OwnedBy != "qwen" {
		t.Errorf("/v1/models entry = %+v, want glm-5.2 owned by qwen", glmEntries[0])
	}
}

// TestMultiProvider_DisabledProviderDropsOut: disable qwen and the whole
// group flips to aiand — listing, canonical name, and alias alike.
func TestMultiProvider_DisabledProviderDropsOut(t *testing.T) {
	aiandSrv, aiandModel := newRecordingUpstream(t, "aiand", 200)
	qwenSrv, qwenModel := newRecordingUpstream(t, "qwen", 200)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedGLMGroup(t, db, aiandSrv.URL, qwenSrv.URL, 10, false) // qwen DISABLED
	srv := newMultiProviderServer(t, db, newFakeSettings())

	rec := chatForModel(srv, "glm-5.2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"aiand"`) {
		t.Fatalf("code=%d body=%s, want aiand to serve while qwen is disabled", rec.Code, rec.Body.String())
	}
	if got := aiandModel.Load(); got != "zai-org/glm-5.2" {
		t.Errorf("aiand saw model %q, want its own wire zai-org/glm-5.2", got)
	}
	if got := qwenModel.Load(); got != "" {
		t.Errorf("disabled qwen saw model %q, want no traffic", got)
	}

	// Listing follows the same primary.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.RemoteAddr = "100.64.0.1:1234"
	srv.Handler().ServeHTTP(rec, req)
	var models ModelsResponse
	json.Unmarshal(rec.Body.Bytes(), &models)
	glmEntries := []ModelEntry{}
	for _, e := range models.Data {
		if e.ID == "glm-5.2" || e.ID == "zai-org/glm-5.2" {
			glmEntries = append(glmEntries, e)
		}
	}
	if len(glmEntries) != 1 || glmEntries[0].ID != "zai-org/glm-5.2" || glmEntries[0].OwnedBy != "aiand" {
		t.Fatalf("/v1/models glm entries = %+v, want one zai-org/glm-5.2 owned by aiand", glmEntries)
	}
}

// TestMultiProvider_AllDisabled: every offering of the group disabled →
// a clean 502 that names the reason, never a silent 404.
func TestMultiProvider_AllDisabled(t *testing.T) {
	aiandSrv, _ := newRecordingUpstream(t, "aiand", 200)
	qwenSrv, _ := newRecordingUpstream(t, "qwen", 200)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedGLMGroup(t, db, aiandSrv.URL, qwenSrv.URL, 10, false)
	// Disable aiand too via the store (the dashboard's PUT path is covered
	// in httpapi's TestProviderUpdateEnabled).
	row, ok, err := findProviderRow(db, "aiand")
	if err != nil || !ok {
		t.Fatalf("findProviderRow: %v %v", ok, err)
	}
	row.Enabled = false
	if err := db.Routing().SaveProvider(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	srv := newMultiProviderServer(t, db, newFakeSettings())
	rec := chatForModel(srv, "glm-5.2")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no enabled offering") {
		t.Errorf("body = %s, want the no-enabled-offering reason", rec.Body.String())
	}
}

// TestMultiProvider_FailoverSetting: with router.provider_failover on, a
// 5xx from the primary falls through to the next offering of the same
// model; with it off (the default), the error surfaces instead of silently
// spending on the second provider.
func TestMultiProvider_FailoverSetting(t *testing.T) {
	aiandSrv, aiandModel := newRecordingUpstream(t, "aiand", 200)
	qwenSrv, _ := newRecordingUpstream(t, "qwen", 500) // primary errors

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedGLMGroup(t, db, aiandSrv.URL, qwenSrv.URL, 10, true)

	// Default (off): the 5xx exhausts a single-backend chain.
	srv := newMultiProviderServer(t, db, newFakeSettings())
	rec := chatForModel(srv, "glm-5.2")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failover off: code = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := aiandModel.Load(); got != "" {
		t.Errorf("failover off: aiand saw %q, want no traffic", got)
	}

	// On: the same request lands on aiand.
	settings := newFakeSettings()
	settings.set("router.provider_failover", []byte("true"))
	srv = newMultiProviderServer(t, db, settings)
	rec = chatForModel(srv, "glm-5.2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"aiand"`) {
		t.Fatalf("failover on: code=%d body=%s, want aiand to take over", rec.Code, rec.Body.String())
	}
	if got := aiandModel.Load(); got != "zai-org/glm-5.2" {
		t.Errorf("failover on: aiand saw %q, want the rewritten wire", got)
	}
}

// findProviderRow reads one provider row back out of a live store.
func findProviderRow(db *store.DB, name string) (store.ProviderRow, bool, error) {
	rows, err := db.Routing().Providers(context.Background())
	if err != nil {
		return store.ProviderRow{}, false, err
	}
	for _, p := range rows {
		if p.Name == name {
			return p, true, nil
		}
	}
	return store.ProviderRow{}, false, nil
}
