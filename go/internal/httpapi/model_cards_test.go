// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/fx"
	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// seedTestCatalog seeds an in-memory catalog DB with a minimal model
// (family → model "Qwen3 Test" → variant → config "qwen3" → benchmarks)
// and returns the DB + config ID. decode_tps=100.0 pairs with
// newRegistryTestServer's [cost] section (power_kw=0.36, rate_per_kwh=1.0)
// to make the BE-COST formula collapse to exactly $1.00/1M
// (0.36 * 1.0 * 1e6 / (3600 * 100) = 1.0).
func seedTestCatalog(t *testing.T, ctx context.Context) (*store.DB, int64) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cat := db.Catalog()

	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "Qwen3 Test"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt.ID,
		FilePath: "qwen3.gguf", ArtifactType: "weight",
	})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	configID, _ := cat.CreateConfig(ctx, store.Config{
		Name: "qwen3", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, NCtx: 32768,
	})
	// Benchmarks: decode_tps=100.0 + safe_memory_bytes=12 GiB.
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "decode_tps", Value: "100.0", Source: "self_measured",
		SubjectType: "variant", SubjectID: varID,
	})
	cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "safe_memory_bytes", Value: "12884901888", // 12 * 1024^3
		Source: "self_measured", SubjectType: "variant", SubjectID: varID,
	})
	return db, configID
}

// newRegistryTestServer builds a Server with Registry (and optionally Usage +
// FX) wired to a real catalog-backed store, for coverage of
// GET /api/v1/configs/cards, /api/v1/models/cards, and the usage cost calc.
func newRegistryTestServer(t *testing.T, usage store.Usage, fxSource fx.Source) *Server {
	t.Helper()
	db, configID := seedTestCatalog(t, context.Background())
	return serverForCatalog(t, db, configID, usage, fxSource)
}

// serverForCatalog builds a Server around an already-seeded catalog DB
// (seedTestCatalog, optionally with extra rows added by the caller first —
// e.g. Sprint D's model-scoped capability benchmark). Factored out of
// newRegistryTestServer so tests can inject rows between seeding and server
// construction.
func serverForCatalog(t *testing.T, db *store.DB, configID int64, usage store.Usage, fxSource fx.Source) *Server {
	t.Helper()
	if usage == nil {
		usage = db.Usage()
	}

	events := bus.New()
	cfg, err := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
		Modes: map[string]config.Mode{
			"qwen3": {Label: "Qwen3", Services: []config.Service{{Model: "qwen3.gguf", Alias: "qwen3"}}},
		},
		Cost: config.Cost{PowerKW: 0.36, RatePerKWh: 1.0},
	})
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}
	// B2: set ConfigID so the usage handler can resolve mode → configID.
	mode := cfg.Modes["qwen3"]
	mode.ConfigID = configID
	cfg.Modes["qwen3"] = mode

	reg := registry.New(db.Catalog(), func() *config.Config { return cfg }, usage)

	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Usage:     usage,
		Registry:  reg,
		FX:        fxSource,
	})
	t.Cleanup(func() { s.Close() })
	return s
}

func TestModelCardsEmptyWithNoRegistryWired(t *testing.T) {
	s := newTestServer(t) // Deps.Registry is nil
	w := do(t, s, authedRequest("GET", "/api/v1/models/cards", nil))
	if w.Code != 200 {
		t.Fatalf("models/cards = %d", w.Code)
	}
	var resp modelCardsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Window != "7d" {
		t.Errorf("window = %q, want default 7d", resp.Window)
	}
	if resp.Cards == nil {
		t.Error("cards must be [] not null (PWA expects an array)")
	}
	if len(resp.Cards) != 0 {
		t.Errorf("cards = %d, want 0 with no registry wired", len(resp.Cards))
	}
}

func TestModelCardsWithRegistryWired(t *testing.T) {
	s := newRegistryTestServer(t, nil, nil)
	w := do(t, s, authedRequest("GET", "/api/v1/models/cards?window=24h", nil))
	if w.Code != 200 {
		t.Fatalf("models/cards = %d: %s", w.Code, w.Body.String())
	}
	var resp modelCardsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Window != "24h" {
		t.Errorf("window = %q, want 24h", resp.Window)
	}
	if len(resp.Cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(resp.Cards))
	}
	card := resp.Cards[0]
	if card.Name != "Qwen3 Test" {
		t.Errorf("card name: got %q", card.Name)
	}
	if len(card.Modes) != 1 || card.Modes[0] != "qwen3" {
		t.Errorf("card modes: got %+v, want [qwen3]", card.Modes)
	}
	if card.Performance.MemoryReqBytes == nil || *card.Performance.MemoryReqBytes != int64(12*1024*1024*1024) {
		t.Errorf("card.performance.memory_req_bytes: %+v", card.Performance)
	}
}

// TestModelCardsCapabilities_ModelScopedBenchmark is the HTTP-contract half
// of the Sprint D subject_type-trap regression (registry-level coverage is
// TestCards_ModelScopedBenchmarkReachesCard /
// TestModelCards_ModelScopedBenchmarkReachesCard in the registry package):
// a subject_type="model" benchmark row must reach card.capabilities over
// the wire on both /api/v1/models/cards and /api/v1/configs/cards.
func TestModelCardsCapabilities_ModelScopedBenchmark(t *testing.T) {
	ctx := context.Background()
	db, configID := seedTestCatalog(t, ctx)
	cfg, err := db.Catalog().GetConfig(ctx, configID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	vt, err := db.Catalog().GetVariant(ctx, cfg.VariantID)
	if err != nil {
		t.Fatalf("GetVariant: %v", err)
	}
	db.Catalog().CreateBenchmark(ctx, store.Benchmark{
		Metric: "reasoning", Value: "0.86", Source: "published",
		SourceURL: "https://lushbinary.com/blog/qwen-3-6-developer-guide-benchmarks-architecture-api-self-hosting",
		SourceDate: "2026-08-04", Notes: "GPQA Diamond",
		SubjectType: "model", SubjectID: vt.ModelID,
	})
	s := serverForCatalog(t, db, configID, nil, nil)

	w := do(t, s, authedRequest("GET", "/api/v1/models/cards", nil))
	if w.Code != 200 {
		t.Fatalf("models/cards = %d: %s", w.Code, w.Body.String())
	}
	var mResp modelCardsResponse
	decodeJSON(t, w.Body, &mResp)
	if len(mResp.Cards) != 1 || len(mResp.Cards[0].Capabilities) != 1 {
		t.Fatalf("model cards: %+v", mResp.Cards)
	}
	if got := mResp.Cards[0].Capabilities[0]; got.ID != "reasoning" || got.Score != 0.86 || got.Benchmark != "GPQA Diamond" {
		t.Errorf("model card capability: got %+v", got)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/configs/cards", nil))
	if w.Code != 200 {
		t.Fatalf("configs/cards = %d: %s", w.Code, w.Body.String())
	}
	var cResp configCardsResponse
	decodeJSON(t, w.Body, &cResp)
	if len(cResp.Cards) != 1 || len(cResp.Cards[0].Capabilities) != 1 {
		t.Fatalf("config cards: %+v", cResp.Cards)
	}
	if got := cResp.Cards[0].Capabilities[0]; got.ID != "reasoning" || got.Score != 0.86 {
		t.Errorf("config card capability: got %+v", got)
	}
}

func TestConfigCardsWithRegistryWired(t *testing.T) {
	s := newRegistryTestServer(t, nil, nil)
	w := do(t, s, authedRequest("GET", "/api/v1/configs/cards?window=24h", nil))
	if w.Code != 200 {
		t.Fatalf("configs/cards = %d: %s", w.Code, w.Body.String())
	}
	var resp configCardsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Window != "24h" {
		t.Errorf("window = %q, want 24h", resp.Window)
	}
	if len(resp.Cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(resp.Cards))
	}
	card := resp.Cards[0]
	if card.Name != "qwen3" {
		t.Errorf("card name: got %q, want 'qwen3'", card.Name)
	}
	if card.ModelName != "Qwen3 Test" {
		t.Errorf("card model_name: got %q", card.ModelName)
	}
	if card.NCtx != 32768 {
		t.Errorf("card n_ctx: got %d", card.NCtx)
	}
	if card.Performance.MemoryReqBytes == nil || *card.Performance.MemoryReqBytes != int64(12*1024*1024*1024) {
		t.Errorf("card.performance.memory_req_bytes: %+v", card.Performance)
	}
}

// TestModelCardsHiddenModelFiltered covers the model-level decommission flag
// (0062): a hidden model is excluded from /api/v1/models/cards and its config
// from /api/v1/configs/cards, while the Settings catalog CRUD still returns
// it.
func TestModelCardsHiddenModelFiltered(t *testing.T) {
	ctx := context.Background()
	db, configID := seedTestCatalog(t, ctx)
	cat := db.Catalog()
	// Create a second visible model + config.
	mdl2, err := cat.CreateModel(ctx, store.Model{Name: "Visible Second"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	vt2, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdl2, Name: "base"})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	fmt2, _ := cat.FormatByName(ctx, "GGUF")
	weight2, err := cat.CreateArtifact(ctx, store.Artifact{VariantID: vt2, FormatID: fmt2.ID, FilePath: "visible2.gguf", ArtifactType: "weight"})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	if _, err := cat.CreateConfig(ctx, store.Config{Name: "visible2", VariantID: vt2, WeightArtifactID: weight2, EngineID: eng.ID, NCtx: 8192}); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	// Now hide the original model (the one seedTestCatalog created).
	var origID int64
	mdls, err := cat.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	for _, m := range mdls {
		if m.Name == "Qwen3 Test" {
			origID = m.ID
		}
	}
	if origID == 0 {
		t.Fatal("seed model not found")
	}
	if err := cat.UpdateModel(ctx, store.Model{ID: origID, Name: "Qwen3 Test", Visibility: "hidden"}); err != nil {
		t.Fatalf("UpdateModel hidden: %v", err)
	}
	s := serverForCatalog(t, db, configID, nil, nil)

	w := do(t, s, authedRequest("GET", "/api/v1/models/cards", nil))
	if w.Code != 200 {
		t.Fatalf("models/cards = %d: %s", w.Code, w.Body.String())
	}
	var mResp modelCardsResponse
	decodeJSON(t, w.Body, &mResp)
	if len(mResp.Cards) != 1 || mResp.Cards[0].Name != "Visible Second" {
		t.Fatalf("hidden model leaked into cards: %+v", mResp.Cards)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/configs/cards", nil))
	if w.Code != 200 {
		t.Fatalf("configs/cards = %d: %s", w.Code, w.Body.String())
	}
	var cResp configCardsResponse
	decodeJSON(t, w.Body, &cResp)
	if len(cResp.Cards) != 1 || cResp.Cards[0].Name != "visible2" {
		t.Fatalf("hidden model's config leaked into config cards: %+v", cResp.Cards)
	}

	// Settings CRUD still lists the hidden model.
	mdls, err = cat.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	found := false
	for _, m := range mdls {
		if m.Name == "Qwen3 Test" && m.Visibility == "hidden" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hidden model missing from ListModels: %+v", mdls)
	}
}

func TestModelCardsInvalidWindowDefaultsTo7d(t *testing.T) {
	s := newRegistryTestServer(t, nil, nil)
	w := do(t, s, authedRequest("GET", "/api/v1/models/cards?window=bogus", nil))
	if w.Code != 200 {
		t.Fatalf("models/cards = %d", w.Code)
	}
	var resp modelCardsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Window != "7d" {
		t.Errorf("window = %q, want fallback 7d", resp.Window)
	}
}

func TestUsageLocalCostFromRegistry(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Usage().Record(t.Context(), store.UsageEvent{
		TS: time.Now(), Kind: "inference", Model: "qwen3",
		PromptTokens: 1000, CompletionTokens: 1000,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	s := newRegistryTestServer(t, db.Usage(), nil)
	w := do(t, s, authedRequest("GET", "/api/v1/usage?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("usage = %d: %s", w.Code, w.Body.String())
	}
	var resp usageResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(resp.Models))
	}
	m := resp.Models[0]
	// BE-COST (F5): cost_per_1M is computed from decode_tps=100.0 and [cost]
	// power_kw=0.36/rate_per_kwh=1.0 — see seedTestCatalog. Cost/savings
	// sprint 2026-07-30: registry.powerRate() now wall-adjusts power_kw
	// (+DefaultOverheadW=25W, /DefaultPSUEfficiency=0.9, since this config
	// doesn't set them) before pricing, so this is no longer the raw
	// $1.00/1M — (360+25)/0.9=427.78W -> $1.1882.../1M. 2000 tokens/1e6 *
	// $1.1882.../1M = 0.0023765...
	want := 0.0023765432098765433
	if !approx(m.PowerCostDisplay, want) {
		t.Errorf("power_cost_display = %v, want %v", m.PowerCostDisplay, want)
	}
	if !approx(resp.Totals.LocalCostDisplay, want) {
		t.Errorf("totals.local_cost_display = %v, want %v", resp.Totals.LocalCostDisplay, want)
	}
}

// stubFX is an in-memory fx.Source for usage-handler FX tests. It returns a
// fixed display currency, a single (from,to)→rate mapping, and a fixed
// provenance — enough to exercise the per-1M + FX-conversion paths without a
// real fx.Cache.
type stubFX struct {
	display   string
	rates     map[string]float64 // key "FROM->TO", case-insensitive
	billCcy   map[string]string  // provider → currency
	asOf      time.Time
	stale     bool
	hasRates  bool
}

func (s *stubFX) DisplayCurrency(_ context.Context) string { return s.display }

func (s *stubFX) BillCurrency(_ context.Context, provider string) string {
	if c, ok := s.billCcy[provider]; ok {
		return c
	}
	return "USD"
}

func (s *stubFX) Rate(_ context.Context, from, to string) (float64, bool) {
	from = strings.ToUpper(strings.TrimSpace(from))
	to = strings.ToUpper(strings.TrimSpace(to))
	if from == to {
		return 1.0, true
	}
	if r, ok := s.rates[from+"->"+to]; ok {
		return r, true
	}
	return 0, false
}

func (s *stubFX) Provenance(_ context.Context) (time.Time, bool, bool) {
	return s.asOf, s.stale, s.hasRates
}

// TestUsageLocalCost_Per1mWithFXConversion confirms the per-1M power estimate
// is FX-converted when display != USD, and fx_as_of/fx_stale ride the response.
func TestUsageLocalCost_Per1mWithFXConversion(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Usage().Record(t.Context(), store.UsageEvent{
		TS: time.Now(), Kind: "inference", Model: "qwen3",
		PromptTokens: 500_000, CompletionTokens: 500_000,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	asOf := time.Now()
	fxSrc := &stubFX{
		display:  "CNY",
		rates:    map[string]float64{"USD->CNY": 7.2},
		asOf:     asOf,
		hasRates: true,
	}
	s := newRegistryTestServer(t, db.Usage(), fxSrc)

	w := do(t, s, authedRequest("GET", "/api/v1/usage?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("usage = %d: %s", w.Code, w.Body.String())
	}
	var resp usageResponse
	decodeJSON(t, w.Body, &resp)

	if resp.DisplayCurrency != "CNY" {
		t.Fatalf("display_currency = %q, want CNY", resp.DisplayCurrency)
	}
	// BE-COST (F5): cost_per_1M is computed from seedTestCatalog's [cost]
	// power_kw=0.36/rate_per_kwh=1.0. Cost/savings sprint 2026-07-30:
	// wall-adjusted (+25W overhead, /0.9 PSU efficiency, defaults since
	// this config doesn't set them) to $1.1882.../1M, not the raw $1.00/1M.
	// 1M tokens × $1.1882.../1M = 1.1882... USD → ×7.2 = 8.5556 CNY.
	want := 8.555555555555555
	if len(resp.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(resp.Models))
	}
	if got := resp.Models[0].PowerCostDisplay; !approx(got, want) {
		t.Errorf("power_cost_display = %v, want %v (CNY)", got, want)
	}
	if got := resp.Totals.LocalCostDisplay; !approx(got, want) {
		t.Errorf("totals.local_cost_display = %v, want %v", got, want)
	}
	if resp.FxAsOf == nil {
		t.Fatal("fx_as_of should be non-null when a conversion was applied")
	}
	if got := *resp.FxAsOf; !approx(got, float64(asOf.UnixNano())/1e9) {
		t.Errorf("fx_as_of = %v, want %v", got, float64(asOf.UnixNano())/1e9)
	}
	if resp.FxStale {
		t.Error("fx_stale should be false with a fresh rate cached")
	}
}

// TestCards_PowerEstConvertedToDisplayCurrency confirms the card wire's
// power_est_per_1m is FX-converted from the electricity rate currency to the
// display currency, and that display_currency rides the cards response —
// profiling/pricing sprint 2026-08-07. Without this, a JPY-denominated rate
// rendered as "$" (the "wildly wrong" prices this sprint fixes).
func TestCards_PowerEstConvertedToDisplayCurrency(t *testing.T) {
	fxSrc := &stubFX{
		display:  "CNY",
		rates:    map[string]float64{"USD->CNY": 7.2},
		asOf:     time.Now(),
		hasRates: true,
	}
	s := newRegistryTestServer(t, nil, fxSrc)

	w := do(t, s, authedRequest("GET", "/api/v1/configs/cards", nil))
	if w.Code != 200 {
		t.Fatalf("configs/cards = %d: %s", w.Code, w.Body.String())
	}
	var resp configCardsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.DisplayCurrency != "CNY" {
		t.Fatalf("display_currency = %q, want CNY", resp.DisplayCurrency)
	}
	if len(resp.Cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(resp.Cards))
	}
	got := resp.Cards[0].Performance.PowerEstPer1m
	if got == nil {
		t.Fatal("card power_est_per_1m: nil")
	}
	// seedTestCatalog: decode_tps=100.0, [cost] power_kw=0.36 rate=1.0,
	// rate_currency defaults USD; wall-adjusted (360+25)/0.9 W. USD value
	// × 7.2 = CNY value.
	wallKW := (0.36*1000 + config.DefaultOverheadW) / config.DefaultPSUEfficiency / 1000
	usdPer1M := wallKW * 1.0 * 1e6 / (3600 * 100.0)
	want := usdPer1M * 7.2
	if !approx(*got, want) {
		t.Errorf("card power_est_per_1m = %v, want %v (CNY-converted)", *got, want)
	}

	// Model-scoped cards convert identically.
	w = do(t, s, authedRequest("GET", "/api/v1/models/cards", nil))
	if w.Code != 200 {
		t.Fatalf("models/cards = %d: %s", w.Code, w.Body.String())
	}
	var mResp modelCardsResponse
	decodeJSON(t, w.Body, &mResp)
	if mResp.DisplayCurrency != "CNY" {
		t.Fatalf("models display_currency = %q, want CNY", mResp.DisplayCurrency)
	}
	if len(mResp.Cards) != 1 || mResp.Cards[0].Performance.PowerEstPer1m == nil {
		t.Fatalf("model cards: %+v", mResp.Cards)
	}
	if !approx(*mResp.Cards[0].Performance.PowerEstPer1m, want) {
		t.Errorf("model card power_est_per_1m = %v, want %v", *mResp.Cards[0].Performance.PowerEstPer1m, want)
	}
}

// TestUsageExternalCost_NativeAndDisplay confirms external rows carry
// cost_native + native_currency (as billed) and a FX-converted cost_display.
func TestUsageExternalCost_NativeAndDisplay(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Seed a provider with bill_currency CNY so the row tags native_currency.
	if err := db.Routing().SaveProvider(t.Context(), store.ProviderRow{
		Name: "deepseek", APIKey: "sk-x", TargetURL: "https://api.deepseek.com",
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	if _, err := db.SQL().Exec(
		`UPDATE router_providers SET bill_currency = 'CNY' WHERE name = 'deepseek'`); err != nil {
		t.Fatalf("set bill_currency: %v", err)
	}
	deepseek, _, err := db.Routing().ProviderByName(t.Context(), "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}

	cost := 1.4
	if err := db.Usage().Record(t.Context(), store.UsageEvent{
		TS: time.Now(), Kind: "external_request",
		ProviderID: &deepseek.ID, Model: "deepseek-chat",
		PromptTokens: 1_000_000, CompletionTokens: 0,
		CostNative: &cost, CostCurrency: "CNY",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	asOf := time.Now()
	fxSrc := &stubFX{
		display:  "USD",
		rates:    map[string]float64{"CNY->USD": 1.0 / 7.2},
		billCcy:  map[string]string{"deepseek": "CNY"},
		asOf:     asOf,
		hasRates: true,
	}
	s := newRegistryTestServer(t, db.Usage(), fxSrc)

	w := do(t, s, authedRequest("GET", "/api/v1/usage?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("usage = %d: %s", w.Code, w.Body.String())
	}
	var resp usageResponse
	decodeJSON(t, w.Body, &resp)

	if resp.DisplayCurrency != "USD" {
		t.Fatalf("display_currency = %q, want USD", resp.DisplayCurrency)
	}
	if len(resp.External) != 1 {
		t.Fatalf("external = %d, want 1", len(resp.External))
	}
	e := resp.External[0]
	if e.NativeCurrency != "CNY" {
		t.Errorf("native_currency = %q, want CNY", e.NativeCurrency)
	}
	if e.CostNative != 1.4 {
		t.Errorf("cost_native = %v, want 1.4", e.CostNative)
	}
	// 1.4 CNY × (1/7.2) = ~0.1944 USD.
	want := 1.4 / 7.2
	if !approx(e.CostDisplay, want) {
		t.Errorf("cost_display = %v, want %v (USD)", e.CostDisplay, want)
	}
	if !approx(resp.Totals.ExternalCostDisplay, want) {
		t.Errorf("totals.external_cost_display = %v, want %v", resp.Totals.ExternalCostDisplay, want)
	}
	if resp.FxAsOf == nil {
		t.Error("fx_as_of should be non-null (a conversion was applied)")
	}
}

// TestUsageExternalCost_PrefersCostNative is the regression test for the
// cost/savings sprint Phase 4 read-path gap found live 2026-07-30: a real
// remote completion through a0 correctly wrote CostNative/CostCurrency, but
// handleUsage's aggregation only ever read the legacy CostUSD field, so the
// real cost was silently invisible via GET /api/v1/usage despite being
// correctly persisted.
func TestUsageExternalCost_PrefersCostNative(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Routing().SaveProvider(t.Context(), store.ProviderRow{
		Name: "deepseek", APIKey: "sk-x", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseek, _, err := db.Routing().ProviderByName(t.Context(), "deepseek")
	if err != nil {
		t.Fatalf("ProviderByName: %v", err)
	}

	cost := 0.5
	if err := db.Usage().Record(t.Context(), store.UsageEvent{
		TS: time.Now(), Kind: "external_request",
		ProviderID: &deepseek.ID, Model: "deepseek-v4-flash",
		PromptTokens: 1000, CompletionTokens: 200,
		CostNative: &cost, CostCurrency: "USD",
	}); err != nil {
		t.Fatalf("Record metered: %v", err)
	}
	// A second request that completed but carried no usage object.
	if err := db.Usage().Record(t.Context(), store.UsageEvent{
		TS: time.Now(), Kind: "external_request",
		ProviderID: &deepseek.ID, Model: "deepseek-v4-flash",
		Unmetered: true,
	}); err != nil {
		t.Fatalf("Record unmetered: %v", err)
	}

	fxSrc := &stubFX{display: "USD", hasRates: true}
	s := newRegistryTestServer(t, db.Usage(), fxSrc)

	w := do(t, s, authedRequest("GET", "/api/v1/usage?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("usage = %d: %s", w.Code, w.Body.String())
	}
	var resp usageResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.External) != 1 {
		t.Fatalf("external = %d, want 1", len(resp.External))
	}
	e := resp.External[0]
	if e.CostNative != 0.5 {
		t.Errorf("cost_native = %v, want 0.5 (from the new CostNative field, not the unset legacy CostUSD)", e.CostNative)
	}
	if e.NativeCurrency != "USD" {
		t.Errorf("native_currency = %q, want USD (from the event's own CostCurrency)", e.NativeCurrency)
	}
	if e.Requests != 2 {
		t.Errorf("requests = %d, want 2 (both rows counted)", e.Requests)
	}
	if e.RequestsUnmetered != 1 {
		t.Errorf("requests_unmetered = %d, want 1", e.RequestsUnmetered)
	}
	if e.PromptTokens != 1000 || e.CompletionTokens != 200 {
		t.Errorf("tokens = (%d,%d), want (1000,200) — the unmetered row must not contribute", e.PromptTokens, e.CompletionTokens)
	}
}

// TestUsageFxStale_WhenRateMissing confirms fx_stale=true is surfaced when a
// needed rate isn't cached (the caller falls back to 1:1 and flags it stale).
func TestUsageFxStale_WhenRateMissing(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Usage().Record(t.Context(), store.UsageEvent{
		TS: time.Now(), Kind: "inference", Model: "qwen3",
		PromptTokens: 1000, CompletionTokens: 0,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// display=CNY but no USD->CNY rate cached → fallback 1:1, fx_stale=true.
	fxSrc := &stubFX{
		display:  "CNY",
		rates:    map[string]float64{}, // no rates
		asOf:     time.Now(),
		hasRates: false,
	}
	s := newRegistryTestServer(t, db.Usage(), fxSrc)

	w := do(t, s, authedRequest("GET", "/api/v1/usage?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("usage = %d: %s", w.Code, w.Body.String())
	}
	var resp usageResponse
	decodeJSON(t, w.Body, &resp)

	if resp.DisplayCurrency != "CNY" {
		t.Fatalf("display_currency = %q, want CNY", resp.DisplayCurrency)
	}
	if resp.FxAsOf != nil {
		t.Errorf("fx_as_of = %v, want null (no rates cached)", *resp.FxAsOf)
	}
	if !resp.FxStale {
		t.Error("fx_stale should be true when a conversion was needed but no rate cached")
	}
}

// TestUsageNoConversion_FxNullWhenDisplayIsUSD confirms fx_as_of stays null
// when display == the cost currency (no conversion applied), even with FX
// wired. This is the common all-USD case.
func TestUsageNoConversion_FxNullWhenDisplayIsUSD(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Usage().Record(t.Context(), store.UsageEvent{
		TS: time.Now(), Kind: "inference", Model: "qwen3",
		PromptTokens: 1000, CompletionTokens: 1000,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	fxSrc := &stubFX{
		display: "USD", // same as the power estimate's currency → no conversion
		hasRates: true,
	}
	s := newRegistryTestServer(t, db.Usage(), fxSrc)

	w := do(t, s, authedRequest("GET", "/api/v1/usage?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("usage = %d: %s", w.Code, w.Body.String())
	}
	var resp usageResponse
	decodeJSON(t, w.Body, &resp)

	if resp.FxAsOf != nil {
		t.Errorf("fx_as_of = %v, want null (no conversion applied)", *resp.FxAsOf)
	}
	if resp.FxStale {
		t.Error("fx_stale should be false when no conversion was applied")
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
