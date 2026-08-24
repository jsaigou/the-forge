// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

func TestTTLCache_ProbeCachesWithinTTL(t *testing.T) {
	// Within the TTL window, repeated Probe() calls hit the upstream only
	// once — the mode-switch dead window isn't hammered with duplicate probes.
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			hits.Add(1) // count only /health probes (not /props best-effort reads)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":     "ok",
			"n_ctx":      4096,
			"model_path": "/models/test.gguf",
		})
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newTTLCatalog(nil)
	ttl := 100 * time.Millisecond

	p1 := cat.Probe(port, ttl)
	if !p1.Healthy || p1.NCtx != 4096 || p1.ModelPath != "/models/test.gguf" {
		t.Fatalf("first probe = %+v, want healthy+4096+/models/test.gguf", p1)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d after first probe, want 1", hits.Load())
	}

	// Second call within TTL → cached, no new upstream hit.
	p2 := cat.Probe(port, ttl)
	if p2 != p1 {
		t.Errorf("cached probe differs: %+v vs %+v", p2, p1)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d after cached probe, want 1", hits.Load())
	}

	// After TTL expires → fresh probe.
	time.Sleep(120 * time.Millisecond)
	_ = cat.Probe(port, ttl)
	if hits.Load() != 2 {
		t.Errorf("upstream hits = %d after TTL expiry, want 2", hits.Load())
	}
}

func TestTTLCache_UnhealthyOnConnectionRefused(t *testing.T) {
	// Port 1 (nothing listening) → connection refused → healthy=false, no error.
	// This is the mode-switch dead-window signal the router skip-on-failure
	// logic relies on.
	cat := newTTLCatalog(nil)
	p := cat.Probe(1, 50*time.Millisecond)
	if p.Healthy {
		t.Errorf("probe on closed port = healthy, want unhealthy")
	}
}

func TestTTLCache_IsBusy(t *testing.T) {
	// /metrics reports llamacpp:requests_processing > 0 → busy=true.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path == "/metrics" {
			w.Write([]byte("# HELP llamacpp:requests_processing Number of requests being processed\nllamacpp:requests_processing 2\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newTTLCatalog(nil)
	if !cat.IsBusy(port, 100*time.Millisecond) {
		t.Error("IsBusy = false, want true (requests_processing=2)")
	}
}

func TestTTLCache_NotBusyWhenIdle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Write([]byte("llamacpp:requests_processing 0\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newTTLCatalog(nil)
	if cat.IsBusy(port, 100*time.Millisecond) {
		t.Error("IsBusy = true, want false (requests_processing=0)")
	}
}

func TestTTLCache_NotBusyOnProbeFailure(t *testing.T) {
	// /metrics failure → not busy (fail toward wait-mode behavior).
	cat := newTTLCatalog(nil)
	if cat.IsBusy(1, 50*time.Millisecond) {
		t.Error("IsBusy on closed port = true, want false (fail to not-busy)")
	}
}

func TestParsePromScalar(t *testing.T) {
	// V4 monitor.py _parse_prom_scalar parity — the same signal the hang
	// detector uses, duplicated here.
	for _, tc := range []struct {
		name string
		text string
		want float64
	}{
		{"simple", "llamacpp:requests_processing 3\n", 3},
		{"labeled", `llamacpp:requests_processing{slot="a1"} 5` + "\n", 5},
		{"missing", "# HELP foo\nbar 1\n", 0},
		{"empty", "", 0},
		{"with_other_metrics", "llamacpp:slots_active 1\nllamacpp:requests_processing 7\nllamacpp:tokens 100\n", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePromScalar(tc.text, "llamacpp:requests_processing"); got != tc.want {
				t.Errorf("parsePromScalar = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewConfig_DefaultsAndValidation(t *testing.T) {
	cfg, err := NewConfig(RouterConfig{
		ListenPort: 8085,
		BusyMode:   BusyFailFast,
		Backends: []Backend{
			{Name: "a1", Kind: "foundry_slot", Port: 8080},
			{Name: "ds", Kind: "remote", BaseURL: "http://localhost:8790/v1", WireModel: "deepseek-chat", Credential: "deepseek"},
		},
		Routes: []Route{{Model: "gemma4", Primary: "a1", Fallback: []string{"ds"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BusyMode != BusyFailFast {
		t.Errorf("busy_mode = %q, want fail_fast", cfg.BusyMode)
	}
	// Defaults applied.
	if cfg.ConnectTimeoutS != 5 {
		t.Errorf("connect_timeout_s = %v, want 5 (default)", cfg.ConnectTimeoutS)
	}
	if cfg.RequestTimeoutS != 0 {
		t.Errorf("request_timeout_s = %v, want 0 (unbounded default)", cfg.RequestTimeoutS)
	}
	if cfg.HealthTTLS != 4 {
		t.Errorf("health_ttl_s = %v, want 4 (default)", cfg.HealthTTLS)
	}
	if cfg.MaxRetriesPerBackend != 1 {
		t.Errorf("max_retries_per_backend = %v, want 1 (default)", cfg.MaxRetriesPerBackend)
	}
	if cfg.EnsureLoadedTimeoutS != 320 {
		t.Errorf("ensure_loaded_timeout_s = %v, want 320 (default)", cfg.EnsureLoadedTimeoutS)
	}
	if len(cfg.Backends) != 2 || len(cfg.Routes) != 1 {
		t.Errorf("backends=%d routes=%d, want 2 and 1", len(cfg.Backends), len(cfg.Routes))
	}
	// Route + chain helpers.
	r := cfg.RouteFor("gemma4")
	if r == nil || r.Primary != "a1" || len(r.Fallback) != 1 || r.Fallback[0] != "ds" {
		t.Errorf("route lookup wrong: %+v", r)
	}
	b := cfg.BackendByName("ds")
	if b == nil || b.Kind != "remote" {
		t.Errorf("backend lookup wrong: %+v", b)
	}
}

func TestNewConfig_RejectsBadBackendKind(t *testing.T) {
	if _, err := NewConfig(RouterConfig{
		Backends: []Backend{{Name: "x", Kind: "nonsense"}},
	}); err == nil {
		t.Error("unknown backend kind should be rejected")
	}
}

func TestNewConfig_RejectsRouteWithUndefinedBackend(t *testing.T) {
	if _, err := NewConfig(RouterConfig{
		Backends: []Backend{{Name: "a1", Kind: "foundry_slot", Port: 8080}},
		Routes:   []Route{{Model: "m", Primary: "nonexistent"}},
	}); err == nil {
		t.Error("route with undefined backend should be rejected")
	}
}

// ── MODEL CATALOG Phase 2: store-backed Offerings in /v1/models ──────────────

// TestBuildModelsResponse_StoreOfferings verifies that store-backed Offerings
// appear in the /v1/models list when StoreCatalog is populated. Disabled
// offerings are excluded.
func TestBuildModelsResponse_StoreOfferings(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cat := db.Catalog()

	// Seed a provider + enabled offering.
	db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", Enabled: true, CreatedAt: time.Now(),
	})
	deepseek, _, _ := db.Routing().ProviderByName(ctx, "deepseek")
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "deepseek-chat"})
	cat.CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseek.ID,
		WireModel: "deepseek-chat", ContextLength: 65536, Enabled: true,
	})
	// Disabled offering should NOT appear.
	mdlID2, _ := cat.CreateModel(ctx, store.Model{Name: "disabled-model"})
	cat.CreateOffering(ctx, store.Offering{
		ModelID: mdlID2, ProviderID: deepseek.ID,
		WireModel: "disabled-model", Enabled: false,
	})

	rows, err := db.Routing().Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp := BuildModelsResponse(ctx, db.Catalog(), rows)

	found := false
	for _, e := range resp.Data {
		if e.ID == "deepseek-chat" {
			found = true
			if e.ContextLength != 65536 {
				t.Errorf("offering context_length: got %d, want 65536", e.ContextLength)
			}
			if e.OwnedBy != "deepseek" {
				t.Errorf("offering owned_by: got %q, want 'deepseek'", e.OwnedBy)
			}
		}
		if e.ID == "disabled-model" {
			t.Error("disabled offering should not appear in /v1/models")
		}
	}
	if !found {
		t.Error("enabled offering 'deepseek-chat' not found in /v1/models")
	}
}

// TestBuildModelsResponse_NilStoreCatalog verifies that a nil StoreCatalog
// (not wired) produces an empty list — there's no other source left
// (TOML decommission Phase 3, docs/v5-toml-decommission.md §6).
func TestBuildModelsResponse_NilStoreCatalog(t *testing.T) {
	resp := BuildModelsResponse(context.Background(), nil, nil)
	if len(resp.Data) != 0 {
		t.Errorf("nil store catalog: got %+v, want empty", resp.Data)
	}
}

// seedCatalogConfig creates the minimal Model → Variant → Artifact(weight) →
// Config chain needed to satisfy the configs table's FK constraints
// (name, variant_id, weight_artifact_id, engine_id all NOT NULL), mirroring
// TestCatalogFullRoundTrip in internal/store/catalog_test.go. Returns the new
// config's ID.
func seedCatalogConfig(t *testing.T, cat store.Catalog, name string, nCtx int, visibility string) int64 {
	t.Helper()
	ctx := context.Background()
	mdlID, err := cat.CreateModel(ctx, store.Model{Name: name})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
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
	weightID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, QuantizationID: q.ID, FormatID: f.ID,
		FilePath: name + ".gguf", ArtifactType: "weight",
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	cfgID, err := cat.CreateConfig(ctx, store.Config{
		Name: name, VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, NCtx: nCtx, Visibility: visibility,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	return cfgID
}

// TestBuildModelsResponse_CatalogConfigs verifies that visible store-backed
// local Configs are listed in /v1/models (the a0 local-config visibility
// fix), hidden Configs are not, and a name collision with an Offering's
// wire_model is deduplicated in favor of the Offering (listed first).
func TestBuildModelsResponse_CatalogConfigs(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cat := db.Catalog()

	seedCatalogConfig(t, cat, "swallow-8b-new", 32768, "visible")
	seedCatalogConfig(t, cat, "wip-config", 8192, "hidden")
	// Collides with the Offering seeded below — the Offering must win.
	seedCatalogConfig(t, cat, "gemma4-31b", 262144, "visible")

	db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: "deepseek", APIKey: "sk-test", Enabled: true, CreatedAt: time.Now(),
	})
	deepseek, _, _ := db.Routing().ProviderByName(ctx, "deepseek")
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "gemma4-31b remote mirror"})
	cat.CreateOffering(ctx, store.Offering{
		ModelID: mdlID, ProviderID: deepseek.ID,
		WireModel: "gemma4-31b", ContextLength: 128000, Enabled: true,
	})

	rows, err := db.Routing().Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp := BuildModelsResponse(ctx, cat, rows)

	byID := map[string]ModelEntry{}
	for _, e := range resp.Data {
		byID[e.ID] = e
	}

	visible, ok := byID["swallow-8b-new"]
	if !ok {
		t.Fatal("visible catalog config 'swallow-8b-new' not listed")
	}
	if visible.OwnedBy != "forge-local" {
		t.Errorf("owned_by: got %q, want 'forge-local'", visible.OwnedBy)
	}
	if visible.ContextLength != 32768 {
		t.Errorf("context_length: got %d, want 32768", visible.ContextLength)
	}

	if _, ok := byID["wip-config"]; ok {
		t.Error("hidden catalog config 'wip-config' should not be listed")
	}

	count := 0
	for _, e := range resp.Data {
		if e.ID == "gemma4-31b" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dedup 'gemma4-31b': got %d entries, want 1", count)
	}
	if got := byID["gemma4-31b"].OwnedBy; got != "deepseek" {
		t.Errorf("dedup 'gemma4-31b' owned_by: got %q, want 'deepseek' (offering wins)", got)
	}
}
