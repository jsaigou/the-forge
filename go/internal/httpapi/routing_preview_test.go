// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// ensureLoadedSpy wraps sched.Stub, recording whether EnsureLoaded was ever
// invoked — used to prove the routing-preview local path never triggers a
// real model load (a preview that loads a model is not a preview).
type ensureLoadedSpy struct {
	*sched.Stub
	called bool
}

func (s *ensureLoadedSpy) EnsureLoaded(ctx context.Context, req sched.EnsureRequest) (sched.Ticket, error) {
	s.called = true
	return s.Stub.EnsureLoaded(ctx, req)
}

// newRoutingPreviewTestServer builds a Server backed by a real in-memory
// store.DB (Catalog + Compressor wired to the same DB, matching production)
// and the EnsureLoaded spy, so tests can assert on both the HTTP response
// and whether a load was ever triggered.
func newRoutingPreviewTestServer(t *testing.T) (*Server, *store.DB, *ensureLoadedSpy) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	spy := &ensureLoadedSpy{Stub: &sched.Stub{}}
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     spy,
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Catalog:   db.Catalog(),
		Routing:  db.Routing(),
		Settings:  db.Settings(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db, spy
}

// seedLocalConfig creates a minimal visible catalog Config named "local-model".
func seedLocalConfig(t *testing.T, db *store.DB, name, visibility string) {
	t.Helper()
	ctx := t.Context()
	cat := db.Catalog()
	famID, err := cat.CreateFamily(ctx, store.Family{Name: "Fam-" + name})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}
	mdlID, err := cat.CreateModel(ctx, store.Model{FamilyID: famID, Name: "Model-" + name, Architecture: "llama"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base", TrainedCtx: 4096})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	q, err := cat.QuantizationByName(ctx, "Q8_0")
	if err != nil {
		t.Fatalf("QuantizationByName: %v", err)
	}
	f, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName: %v", err)
	}
	artID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, QuantizationID: q.ID, FormatID: f.ID,
		FilePath: "/models/" + name + ".gguf", ArtifactType: "weight",
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	engines, err := cat.ListEngines(ctx)
	if err != nil {
		t.Fatalf("ListEngines: %v", err)
	}
	if _, err := cat.CreateConfig(ctx, store.Config{
		Name: name, VariantID: varID, WeightArtifactID: artID, EngineID: engines[0].ID,
		NCtx: 4096, Parallel: 1, Status: "verified", Visibility: visibility,
	}); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
}

// seedTwoProviderOffering creates two providers, both offering the same
// catalog Model under wireModel, at the given priorities. Lower priority
// wins per SelectOfferingChain.
func seedTwoProviderOffering(t *testing.T, db *store.DB, wireModel string, lowPriorityProvider, highPriorityProvider string) {
	t.Helper()
	ctx := t.Context()
	cat := db.Catalog()
	hr := db.Routing()

	for _, name := range []string{lowPriorityProvider, highPriorityProvider} {
		if err := hr.SaveProvider(ctx, store.ProviderRow{Name: name, APIKey: "sk-x", Enabled: true, BillCurrency: "USD"}); err != nil {
			t.Fatalf("SaveProvider %s: %v", name, err)
		}
	}
	rows, err := hr.Providers(ctx)
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	idByName := map[string]int64{}
	for _, r := range rows {
		idByName[r.Name] = r.ID
	}

	famID, err := cat.CreateFamily(ctx, store.Family{Name: "Fam-" + wireModel})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}
	mdlID, err := cat.CreateModel(ctx, store.Model{FamilyID: famID, Name: "Model-" + wireModel, Architecture: "llama"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if _, err := cat.CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: idByName[lowPriorityProvider], WireModel: wireModel,
		Currency: "USD", Enabled: true, Priority: 10,
	}); err != nil {
		t.Fatalf("CreateOffering low: %v", err)
	}
	if _, err := cat.CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: idByName[highPriorityProvider], WireModel: wireModel,
		Currency: "USD", Enabled: true, Priority: 100,
	}); err != nil {
		t.Fatalf("CreateOffering high: %v", err)
	}
}

// TestRoutingPreview_LocalPathNeverLoads is the safety-critical assertion:
// previewing a local catalog config must never call sched.EnsureLoaded — a
// preview that loads a model is not a preview.
func TestRoutingPreview_LocalPathNeverLoads(t *testing.T) {
	s, db, spy := newRoutingPreviewTestServer(t)
	seedLocalConfig(t, db, "local-model", "visible")

	w := do(t, s, authedRequest("GET", "/api/v1/routing/preview?model=local-model", nil))
	if w.Code != 200 {
		t.Fatalf("preview = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp routingPreviewResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Kind != "local" {
		t.Errorf("kind = %q, want local", resp.Kind)
	}
	if spy.called {
		t.Error("EnsureLoaded was called by a preview request — a preview must never load a model")
	}
}

// TestRoutingPreview_HiddenConfigIsNotFound covers the visibility check —
// a hidden config exists but must not be presented as routable.
func TestRoutingPreview_HiddenConfigIsNotFound(t *testing.T) {
	s, db, spy := newRoutingPreviewTestServer(t)
	seedLocalConfig(t, db, "hidden-model", "hidden")

	w := do(t, s, authedRequest("GET", "/api/v1/routing/preview?model=hidden-model", nil))
	if w.Code != 200 {
		t.Fatalf("preview = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp routingPreviewResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Kind != "not_found" {
		t.Errorf("kind = %q, want not_found (hidden config)", resp.Kind)
	}
	if spy.called {
		t.Error("EnsureLoaded was called for a hidden config preview")
	}
}

// TestRoutingPreview_RemoteChainOrdersByPriority confirms the preview shares
// the exact same selection SelectOfferingChain gives live routing.
func TestRoutingPreview_RemoteChainOrdersByPriority(t *testing.T) {
	s, db, _ := newRoutingPreviewTestServer(t)
	seedTwoProviderOffering(t, db, "shared-model", "cheap-provider", "expensive-provider")

	w := do(t, s, authedRequest("GET", "/api/v1/routing/preview?model=shared-model", nil))
	if w.Code != 200 {
		t.Fatalf("preview = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp routingPreviewResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Kind != "remote" {
		t.Fatalf("kind = %q, want remote", resp.Kind)
	}
	if len(resp.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(resp.Candidates))
	}
	var selected string
	for _, c := range resp.Candidates {
		if c.Selected {
			selected = c.Provider
		}
	}
	if selected != "cheap-provider" {
		t.Errorf("selected = %q, want cheap-provider (priority 10 beats 100)", selected)
	}
}

// TestRoutingPreview_AssumeDisabledSwitchesPrimary covers the hypothetical
// override: disabling the real primary via assume_disabled must promote the
// next candidate, without touching the real Enabled state.
func TestRoutingPreview_AssumeDisabledSwitchesPrimary(t *testing.T) {
	s, db, _ := newRoutingPreviewTestServer(t)
	seedTwoProviderOffering(t, db, "shared-model", "cheap-provider", "expensive-provider")

	w := do(t, s, authedRequest("GET", "/api/v1/routing/preview?model=shared-model&assume_disabled=cheap-provider", nil))
	if w.Code != 200 {
		t.Fatalf("preview = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp routingPreviewResponse
	decodeJSON(t, w.Body, &resp)
	var selected string
	var cheapReason string
	for _, c := range resp.Candidates {
		if c.Selected {
			selected = c.Provider
		}
		if c.Provider == "cheap-provider" {
			cheapReason = c.Reason
		}
	}
	if selected != "expensive-provider" {
		t.Errorf("selected = %q, want expensive-provider (cheap assumed disabled)", selected)
	}
	if cheapReason == "" {
		t.Error("cheap-provider candidate has no reason explaining why it was skipped")
	}

	// The real provider row must be untouched by a hypothetical override.
	rows, err := db.Routing().Providers(t.Context())
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	for _, r := range rows {
		if r.Name == "cheap-provider" && !r.Enabled {
			t.Error("assume_disabled mutated the real provider's Enabled state — it must be a hypothetical only")
		}
	}
}

// TestRoutingPreview_UnknownModelIsNotFound covers a wire_model matching
// neither a local config nor any offering.
func TestRoutingPreview_UnknownModelIsNotFound(t *testing.T) {
	s, _, spy := newRoutingPreviewTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/routing/preview?model=nothing-like-this-exists", nil))
	if w.Code != 200 {
		t.Fatalf("preview = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp routingPreviewResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Kind != "not_found" {
		t.Errorf("kind = %q, want not_found", resp.Kind)
	}
	if spy.called {
		t.Error("EnsureLoaded was called for an unknown model")
	}
}

// TestRoutingPreview_MissingModelParamIsValidationError.
func TestRoutingPreview_MissingModelParamIsValidationError(t *testing.T) {
	s, _, _ := newRoutingPreviewTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/routing/preview", nil))
	if w.Code != 422 {
		t.Errorf("preview with no model = %d, want 422", w.Code)
	}
}
