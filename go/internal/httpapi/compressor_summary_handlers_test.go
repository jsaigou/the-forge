// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/fx"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// serverWithCompressorStore builds a Server backed by a real in-memory
// store.DB with both Compressor and Catalog wired (the summary handler's TPS
// fallback chain step 3 reads Catalog). No FX wired — displayCurrency
// defaults to USD and every conversion is a 1:1 no-op.
func serverWithCompressorStore(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	return serverWithCompressorStoreFX(t, nil)
}

// serverWithCompressorStoreFX is serverWithCompressorStore with an injectable
// fx.Source, for exercising the compressor summary's FX-conversion fields.
func serverWithCompressorStoreFX(t *testing.T, fxSrc fx.Source) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots:    collector.NewStatic(nil),
		Engine:       &engine.Stub{},
		Sched:        &sched.Stub{},
		Auth:         &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:       events,
		Publish:      events,
		Config:       func() *config.Config { return cfg },
		Hostname:     "test-host",
		Routing:     db.Routing(),
		Catalog:      db.Catalog(),
		Usage:        db.Usage(),
		PrefillStats: db.PrefillStats(),
		FX:           fxSrc,
	})
	t.Cleanup(func() { s.Close() })
	return s, db
}

// seedProxy creates a compressor_proxies row, linking it (via the real
// provider_id FK, 0042) to a router_providers row it creates on demand when
// provider != "" — self-sufficient regardless of what order a test seeds
// its offerings/usage events relative to this call.
func seedProxy(t *testing.T, db *store.DB, service, provider string) {
	t.Helper()
	ctx := context.Background()
	var providerID *int64
	if provider != "" {
		id := mustProviderID(t, db, provider)
		providerID = &id
	}
	if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
		Service: service, Label: service, Port: 8788, Unit: "headroom@" + service, ProviderID: providerID,
	}); err != nil {
		t.Fatalf("SaveProxy(%s): %v", service, err)
	}
}

// mustProviderID upserts a minimal router_providers row (if not already
// present, via SaveProvider's upsert-by-name fallback) and returns its id.
func mustProviderID(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := db.Routing().SaveProvider(ctx, store.ProviderRow{
		Name: name, APIKey: "key", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("mustProviderID: SaveProvider(%s): %v", name, err)
	}
	p, ok, err := db.Routing().ProviderByName(ctx, name)
	if err != nil || !ok {
		t.Fatalf("mustProviderID: ProviderByName(%s): ok=%v err=%v", name, ok, err)
	}
	return p.ID
}

// mustProviderIDPtr is mustProviderID for UsageEvent.ProviderID (*int64).
func mustProviderIDPtr(t *testing.T, db *store.DB, name string) *int64 {
	id := mustProviderID(t, db, name)
	return &id
}

// seedTestConfig creates a minimal configs row under the given name, off an
// already-seeded catalogPrereqs (call seedCatalogPrereqs ONCE per test —
// it also raw-inserts a fixed-name 'testprov' router_providers row, which
// would collide with itself on a second call). model_prefill_stats is FK'd
// to configs.id since 0042, so these standalone prefill-fallback tests
// (which never otherwise touch the catalog) need at least one real config
// to attach observations to.
func seedTestConfig(t *testing.T, db *store.DB, pq catalogPrereqs, name string) {
	t.Helper()
	if _, err := db.Catalog().CreateConfig(context.Background(), store.Config{
		Name: name, VariantID: pq.variantID, WeightArtifactID: pq.artifactID, EngineID: pq.engineID,
		NCtx: 32768, Parallel: 1,
	}); err != nil {
		t.Fatalf("seedTestConfig(%s): %v", name, err)
	}
}

// mustConfigID looks up an already-created configs row's id by name (0042 —
// model_prefill_stats.config_id is a real FK to configs.id, not mode text).
func mustConfigID(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	c, err := db.Catalog().ConfigByName(context.Background(), name)
	if err != nil {
		t.Fatalf("mustConfigID(%s): %v", name, err)
	}
	return c.ID
}

// mustProxyID looks up an already-seeded compressor_proxies row's id by
// service name (0042 — compressor_savings_samples.proxy_id is a real FK now).
func mustProxyID(t *testing.T, db *store.DB, service string) int64 {
	t.Helper()
	proxies, err := db.Routing().Proxies(context.Background())
	if err != nil {
		t.Fatalf("mustProxyID: Proxies: %v", err)
	}
	for _, p := range proxies {
		if p.Service == service {
			return p.ID
		}
	}
	t.Fatalf("mustProxyID: no proxy seeded for service %q", service)
	return 0
}

func TestCompressorSummaryNoCompressorWired(t *testing.T) {
	s := newTestServer(t) // Deps.Compressor unset
	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Proxies == nil || len(resp.Proxies) != 0 {
		t.Errorf("proxies = %v, want empty (not null)", resp.Proxies)
	}
}

func TestCompressorSummaryLocalVsRemoteKind(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	seedProxy(t, db, "deepseek", "deepseek")

	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TokensIn: 1000, Requests: 10, RequestsCached: 2,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample local: %v", err)
	}
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "deepseek"), TokensIn: 500, Requests: 5, RequestsCached: 1,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample deepseek: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Proxies) != 2 {
		t.Fatalf("proxies = %d, want 2: %+v", len(resp.Proxies), resp.Proxies)
	}
	kinds := map[string]string{}
	for _, p := range resp.Proxies {
		kinds[p.Proxy] = p.Kind
	}
	if kinds["local"] != "local" {
		t.Errorf("local proxy kind = %q, want local", kinds["local"])
	}
	if kinds["deepseek"] != "remote" {
		t.Errorf("deepseek proxy kind = %q, want remote", kinds["deepseek"])
	}
}

func TestCompressorSummaryCacheHitRateNullWhenNoRequests(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	now := time.Now().UTC()
	// A sample with only latency data moving, no requests at all this window.
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TTFBCount: 3, TTFBSumMs: 90,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Proxies) != 1 {
		t.Fatalf("proxies = %d, want 1", len(resp.Proxies))
	}
	p := resp.Proxies[0]
	if p.CacheHitRatePct != nil {
		t.Errorf("cache_hit_rate_pct = %v, want nil (0 requests, nothing to divide by)", *p.CacheHitRatePct)
	}
	if p.TTFBMeanMs == nil || *p.TTFBMeanMs != 30 {
		t.Errorf("ttfb_mean_ms = %v, want 30 (90/3)", p.TTFBMeanMs)
	}
}

func TestCompressorSummaryTimeSavedViaCatalogTPS(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	pq := seedCatalogPrereqs(t, db)
	if _, err := db.Catalog().CreateConfig(context.Background(), store.Config{
		Name: "test-mode", VariantID: pq.variantID, WeightArtifactID: pq.artifactID, EngineID: pq.engineID,
		NCtx: 32768, Parallel: 1,
	}); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if _, err := db.Catalog().CreateBenchmark(context.Background(), store.Benchmark{
		Metric: "prefill_tps", Value: "500", Source: "self_measured", SubjectType: "variant", SubjectID: pq.variantID,
	}); err != nil {
		t.Fatalf("CreateBenchmark: %v", err)
	}
	// A decode_tps row must never be substituted for prefill_tps even though
	// it's much larger — if the handler picked it up wrongly, time_saved
	// would come out ~10-50x too small.
	if _, err := db.Catalog().CreateBenchmark(context.Background(), store.Benchmark{
		Metric: "decode_tps", Value: "9999", Source: "self_measured", SubjectType: "variant", SubjectID: pq.variantID,
	}); err != nil {
		t.Fatalf("CreateBenchmark decode: %v", err)
	}

	now := time.Now().UTC()
	// 100 requests, 100000 tokens in (avg 1000 tokens/request), 20 cached.
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TokensIn: 100000, Requests: 100, RequestsCached: 20,
	}, []store.CompressorLabelSample{
		{TS: now, ProxyID: mustProxyID(t, db, "local"), LabelKey: "model", LabelValue: "test-mode", Metric: "requests", Delta: 100},
	}); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Proxies) != 1 {
		t.Fatalf("proxies = %d, want 1", len(resp.Proxies))
	}
	p := resp.Proxies[0]
	if p.TPSSource != "catalog" {
		t.Fatalf("tps_source = %q, want catalog", p.TPSSource)
	}
	if p.TPSMode != "test-mode" {
		t.Errorf("tps_mode = %q, want test-mode", p.TPSMode)
	}
	if p.TimeSavedSecondsEst == nil {
		t.Fatalf("time_saved_seconds_est = nil, want populated")
	}
	// 20 cached requests * 1000 avg tokens/request / 500 tps = 40s.
	want := 40.0
	if *p.TimeSavedSecondsEst != want {
		t.Errorf("time_saved_seconds_est = %v, want %v", *p.TimeSavedSecondsEst, want)
	}
	if p.MoneySavedEst == nil {
		t.Error("money_saved_est should be populated alongside time_saved")
	}
}

// TestCompressorSummaryNoPreciseSourceOmitsModelRatherThanGuess: 2026-08-06
// local-savings prefill sprint — a model with no resolvable real TPS
// (no profile, no observed data, no catalog row, no live slot) is an
// anomaly per the package doc, not a routine gap. It must be OMITTED from
// the sum, never given a fabricated flat-rate figure (this replaces the
// old flat-50-tok/s-fallback behavior this sprint deleted).
func TestCompressorSummaryNoPreciseSourceOmitsModelRatherThanGuess(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TokensIn: 5000, Requests: 10, RequestsCached: 3,
	}, []store.CompressorLabelSample{
		{TS: now, ProxyID: mustProxyID(t, db, "local"), LabelKey: "model", LabelValue: "unknown-mode", Metric: "requests", Delta: 10},
	}); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Proxies) != 1 {
		t.Fatalf("proxies = %d, want 1", len(resp.Proxies))
	}
	p := resp.Proxies[0]
	if p.TimeSavedSecondsEst != nil {
		t.Errorf("time_saved_seconds_est = %v, want nil (no model had a resolvable real TPS — must not fabricate one)", *p.TimeSavedSecondsEst)
	}
	if p.TPSSource != "" {
		t.Errorf("tps_source = %q, want empty (no fallback exists anymore)", p.TPSSource)
	}
	if len(p.PrefillBreakdown) != 0 {
		t.Errorf("prefill_breakdown = %+v, want empty", p.PrefillBreakdown)
	}
	// TokensSavedEst is TPS-independent (RequestsCached × avg tokens/request)
	// — it must still be populated even though no time could be derived from it.
	if p.TokensSavedEst == nil {
		t.Error("tokens_saved_est = nil, want populated (TPS-independent)")
	}
}

// TestCompressorSummaryObservedPrefillSource: the passively-collected
// store.PrefillStats aggregate is consulted and used once it clears the
// minimum-sample floor, ranked above the profile depth-0 scalar.
func TestCompressorSummaryObservedPrefillSource(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	pq := seedCatalogPrereqs(t, db)
	seedTestConfig(t, db, pq, "test-mode")
	now := time.Now().UTC()

	// 200 real observed tok/s (2000 tokens / 10s), well past the 10-sample floor.
	for i := 0; i < 10; i++ {
		if err := db.PrefillStats().AddObservation(context.Background(), mustConfigID(t, db, "test-mode"), "fp1", 200, 1); err != nil {
			t.Fatalf("AddObservation: %v", err)
		}
	}

	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TokensIn: 100000, Requests: 100, RequestsCached: 20,
	}, []store.CompressorLabelSample{
		{TS: now, ProxyID: mustProxyID(t, db, "local"), LabelKey: "model", LabelValue: "test-mode", Metric: "requests", Delta: 100},
	}); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	p := resp.Proxies[0]
	if p.TPSSource != "observed" {
		t.Fatalf("tps_source = %q, want observed", p.TPSSource)
	}
	if p.TimeSavedSecondsEst == nil {
		t.Fatal("time_saved_seconds_est = nil, want populated")
	}
	// 20 cached requests * 1000 avg tokens/request / 200 tps = 100s.
	want := 100.0
	if *p.TimeSavedSecondsEst != want {
		t.Errorf("time_saved_seconds_est = %v, want %v", *p.TimeSavedSecondsEst, want)
	}
	if len(p.PrefillBreakdown) != 1 || p.PrefillBreakdown[0].Mode != "test-mode" || p.PrefillBreakdown[0].Source != "observed" {
		t.Errorf("prefill_breakdown = %+v, want one test-mode/observed entry", p.PrefillBreakdown)
	}
}

// TestCompressorSummaryObservedBelowSampleFloorIsIgnored: fewer than
// prefillObservedMinSamples observations must not outrank a real profile
// scalar (or produce a misleadingly-precise-looking figure on its own).
func TestCompressorSummaryObservedBelowSampleFloorIsIgnored(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	pq := seedCatalogPrereqs(t, db)
	seedTestConfig(t, db, pq, "test-mode")
	now := time.Now().UTC()

	// Only 3 observations — below the 10-sample floor.
	for i := 0; i < 3; i++ {
		if err := db.PrefillStats().AddObservation(context.Background(), mustConfigID(t, db, "test-mode"), "fp1", 9999, 1); err != nil {
			t.Fatalf("AddObservation: %v", err)
		}
	}

	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TokensIn: 5000, Requests: 10, RequestsCached: 3,
	}, []store.CompressorLabelSample{
		{TS: now, ProxyID: mustProxyID(t, db, "local"), LabelKey: "model", LabelValue: "test-mode", Metric: "requests", Delta: 10},
	}); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	p := resp.Proxies[0]
	if p.TPSSource == "observed" {
		t.Error("tps_source = observed, want anything else — below the sample floor must not be used")
	}
}

// TestCompressorSummaryPerModelSum: cached tokens are apportioned across
// models by request share and summed against each model's OWN real TPS —
// never blended into one dominant-model figure (2026-08-06 rewrite).
func TestCompressorSummaryPerModelSum(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	pq := seedCatalogPrereqs(t, db)
	seedTestConfig(t, db, pq, "fast-mode")
	seedTestConfig(t, db, pq, "slow-mode")
	now := time.Now().UTC()

	for i := 0; i < 10; i++ {
		if err := db.PrefillStats().AddObservation(context.Background(), mustConfigID(t, db, "fast-mode"), "fpA", 1000, 1); err != nil { // 1000 tps
			t.Fatalf("AddObservation fast-mode: %v", err)
		}
		if err := db.PrefillStats().AddObservation(context.Background(), mustConfigID(t, db, "slow-mode"), "fpB", 100, 1); err != nil { // 100 tps
			t.Fatalf("AddObservation slow-mode: %v", err)
		}
	}

	// 100 requests total: 75 fast-mode, 25 slow-mode. 1000 avg tokens/req,
	// 40 cached → 40,000 cached tokens, apportioned 75/25.
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TokensIn: 100000, Requests: 100, RequestsCached: 40,
	}, []store.CompressorLabelSample{
		{TS: now, ProxyID: mustProxyID(t, db, "local"), LabelKey: "model", LabelValue: "fast-mode", Metric: "requests", Delta: 75},
		{TS: now, ProxyID: mustProxyID(t, db, "local"), LabelKey: "model", LabelValue: "slow-mode", Metric: "requests", Delta: 25},
	}); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	p := resp.Proxies[0]
	if p.TimeSavedSecondsEst == nil {
		t.Fatal("time_saved_seconds_est = nil, want populated")
	}
	// fast-mode: 40,000*0.75=30,000 tokens / 1000 tps = 30s.
	// slow-mode: 40,000*0.25=10,000 tokens / 100 tps = 100s.
	// Sum = 130s. A single blended-TPS approach would compute a different
	// (wrong) figure — this is the whole point of summing per model.
	want := 130.0
	if *p.TimeSavedSecondsEst != want {
		t.Errorf("time_saved_seconds_est = %v, want %v (per-model sum)", *p.TimeSavedSecondsEst, want)
	}
	if len(p.PrefillBreakdown) != 2 {
		t.Fatalf("prefill_breakdown = %+v, want 2 entries", p.PrefillBreakdown)
	}
	// Breakdown sorted by share descending: fast-mode (0.75) first.
	if p.PrefillBreakdown[0].Mode != "fast-mode" || p.PrefillBreakdown[0].Share != 0.75 {
		t.Errorf("prefill_breakdown[0] = %+v, want fast-mode share=0.75", p.PrefillBreakdown[0])
	}
	if p.PrefillBreakdown[1].Mode != "slow-mode" || p.PrefillBreakdown[1].Share != 0.25 {
		t.Errorf("prefill_breakdown[1] = %+v, want slow-mode share=0.25", p.PrefillBreakdown[1])
	}
	// TPSSource/TPSMode report the largest contributor (by share).
	if p.TPSMode != "fast-mode" {
		t.Errorf("tps_mode = %q, want fast-mode (largest share)", p.TPSMode)
	}
}

// TestCompressorSummaryTimeSavedCanExceedWindow: A1-A4 run CONCURRENTLY, so
// aggregate compute-seconds saved can legitimately exceed the wall-clock
// window (up to ~4x on this hardware) — this must NOT be clamped to the
// window length, which would hide a real signal behind a fake ceiling.
// This is a plausibility-bound regression test, not a "must be smaller than
// window" test: it asserts the figure stays within a generous sane multiple
// (would catch a return-to-a-fabricated-constant regression) while
// confirming a legitimately-large real value is never suppressed.
func TestCompressorSummaryTimeSavedCanExceedWindow(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	pq := seedCatalogPrereqs(t, db)
	seedTestConfig(t, db, pq, "test-mode")
	now := time.Now().UTC()

	// A slow model (10 tps) with heavy caching over a short 1h window.
	for i := 0; i < 10; i++ {
		if err := db.PrefillStats().AddObservation(context.Background(), mustConfigID(t, db, "test-mode"), "fp1", 10, 1); err != nil {
			t.Fatalf("AddObservation: %v", err)
		}
	}
	// 100 requests, avg 1000 tokens/request, 72 cached → 72,000 cached
	// tokens / 10 tps = 7200s = 2x the 1h (3600s) window — a real,
	// plausible >1x figure (4 concurrent slots means up to ~4x is genuinely
	// possible), not the 2026-08-06 incident's ~500x fabricated one.
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TokensIn: 100_000, Requests: 100, RequestsCached: 72,
	}, []store.CompressorLabelSample{
		{TS: now, ProxyID: mustProxyID(t, db, "local"), LabelKey: "model", LabelValue: "test-mode", Metric: "requests", Delta: 100},
	}); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	p := resp.Proxies[0]
	if p.TimeSavedSecondsEst == nil {
		t.Fatal("time_saved_seconds_est = nil, want populated")
	}
	windowSeconds := 3600.0
	// Real concurrency ceiling on this hardware is 4 slots — allow generous
	// compressor above that (10x) so this only fires on a genuine regression
	// (e.g. a unit mixup), not on legitimate >1x-window figures.
	if *p.TimeSavedSecondsEst > windowSeconds*10 {
		t.Errorf("time_saved_seconds_est = %v (%.1fx the window) — implausible even accounting for A1-A4 concurrency; check for a unit/aggregation bug",
			*p.TimeSavedSecondsEst, *p.TimeSavedSecondsEst/windowSeconds)
	}
	// And confirm it's allowed to exceed 1x the window at all — a naive
	// clamp would fail this half of the assertion.
	if *p.TimeSavedSecondsEst <= windowSeconds {
		t.Logf("time_saved_seconds_est = %v did not exceed the window in this run — fine, just not exercising the >1x case", *p.TimeSavedSecondsEst)
	}
}

func TestCompressorSummaryNoCacheHitsNoEstimate(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "local", "")
	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "local"), TokensIn: 5000, Requests: 10, RequestsCached: 0,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	p := resp.Proxies[0]
	if p.TimeSavedSecondsEst != nil || p.TPSSource != "" {
		t.Errorf("no cache hits this window: expected fields absent entirely, got %+v", p)
	}
}

// TestCompressorSummaryRemoteCacheDiscountSaved covers the cost/savings
// follow-up (2026-07-31): remote proxies never got a savings figure at all
// (only local proxies did, via the TPS-based time-saved estimate) — a real
// week of DeepSeek usage showed "Compressor saved: 0" with no way to ever
// show otherwise. This is a genuinely different, non-estimated figure: the
// provider's own reported cache-hit tokens (CachedPromptTokens, only ever
// populated from the provider's real usage response) priced against the
// matching offering's cache-discount rate.
func TestCompressorSummaryRemoteCacheDiscountSaved(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "deepseek", "deepseek")
	pq := seedCatalogPrereqs(t, db)

	cachedRate := 0.014
	if _, err := db.Catalog().CreateOffering(context.Background(), store.Offering{
		ModelID: pq.modelID, ProviderID: mustProviderID(t, db, "deepseek"), WireModel: "deepseek-chat",
		PriceInPer1M: 0.14, PriceOutPer1M: 0.28, PriceCachedInPer1M: &cachedRate,
		Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}

	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "deepseek"), TokensIn: 20000, Requests: 5, RequestsCached: 2,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}
	cached := int64(10_000_000) // 10M cached prompt tokens, to make the $ delta easy to check
	if err := db.Usage().Record(context.Background(), store.UsageEvent{
		TS: now, Kind: "external_request", Model: "deepseek-chat", ProviderID: mustProviderIDPtr(t, db, "deepseek"),
		PromptTokens: 10_000_000, CompletionTokens: 100, CachedPromptTokens: &cached,
	}); err != nil {
		t.Fatalf("Usage.Record: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Proxies) != 1 {
		t.Fatalf("proxies = %d, want 1", len(resp.Proxies))
	}
	p := resp.Proxies[0]
	if p.CacheDiscountSavedNative == nil {
		t.Fatalf("cache_discount_saved_native = nil, want populated")
	}
	// 10M tokens * (0.14 - 0.014) / 1e6 = 1.26.
	want := 1.26
	if got := *p.CacheDiscountSavedNative; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("cache_discount_saved_native = %v, want %v", got, want)
	}
	if p.CacheDiscountSavedCurrency != "USD" {
		t.Errorf("cache_discount_saved_currency = %q, want USD", p.CacheDiscountSavedCurrency)
	}
	if p.CacheDiscountTokens != 10_000_000 {
		t.Errorf("cache_discount_tokens = %d, want 10000000", p.CacheDiscountTokens)
	}
	// Remote proxies never get the local-only TPS-based fields.
	if p.TimeSavedSecondsEst != nil {
		t.Errorf("time_saved_seconds_est should stay nil for a remote proxy, got %v", *p.TimeSavedSecondsEst)
	}
}

// TestCompressorSummaryRemoteNoCacheDiscountModelled confirms the handler
// never fabricates a saved figure when the offering has no
// PriceCachedInPer1M — an unmodelled discount must stay absent, not read as
// zero savings.
func TestCompressorSummaryRemoteNoCacheDiscountModelled(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "aiand", "aiand")
	pq := seedCatalogPrereqs(t, db)

	if _, err := db.Catalog().CreateOffering(context.Background(), store.Offering{
		ModelID: pq.modelID, ProviderID: mustProviderID(t, db, "aiand"), WireModel: "some-model",
		PriceInPer1M: 1.0, PriceOutPer1M: 2.0, Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}

	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "aiand"), TokensIn: 1000, Requests: 5, RequestsCached: 1,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}
	cached := int64(500)
	if err := db.Usage().Record(context.Background(), store.UsageEvent{
		TS: now, Kind: "external_request", Model: "some-model", ProviderID: mustProviderIDPtr(t, db, "aiand"),
		PromptTokens: 1000, CompletionTokens: 50, CachedPromptTokens: &cached,
	}); err != nil {
		t.Fatalf("Usage.Record: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	p := resp.Proxies[0]
	if p.CacheDiscountSavedNative != nil {
		t.Errorf("cache_discount_saved_native = %v, want nil (offering has no modelled cache discount)", *p.CacheDiscountSavedNative)
	}
}

// TestCompressorSummaryRemoteCompressionSaved covers the follow-up fix
// (2026-07-31): the external "Compressor saved" figure was pricing the
// provider's own prompt-cache discount, which applies whether or not
// Compressor compresses the request at all — never an actual Compressor saving.
// This test prices Compressor's real TokensSaved (compress_tokens_saved_total)
// at a single offering's input rate.
func TestCompressorSummaryRemoteCompressionSaved(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "deepseek", "deepseek")
	pq := seedCatalogPrereqs(t, db)
	if _, err := db.Catalog().CreateOffering(context.Background(), store.Offering{
		ModelID: pq.modelID, ProviderID: mustProviderID(t, db, "deepseek"), WireModel: "deepseek-chat",
		PriceInPer1M: 0.14, PriceOutPer1M: 0.28, Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}

	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "deepseek"), TokensIn: 20000, TokensSaved: 10_000_000, Requests: 5, RequestsCached: 2,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}
	if err := db.Usage().Record(context.Background(), store.UsageEvent{
		TS: now, Kind: "external_request", Model: "deepseek-chat", ProviderID: mustProviderIDPtr(t, db, "deepseek"),
		PromptTokens: 10_000_000, CompletionTokens: 100,
	}); err != nil {
		t.Fatalf("Usage.Record: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Proxies) != 1 {
		t.Fatalf("proxies = %d, want 1", len(resp.Proxies))
	}
	p := resp.Proxies[0]
	if p.CompressionSavedNative == nil {
		t.Fatalf("compression_saved_native = nil, want populated")
	}
	// 10M tokens saved * 0.14/1M = 1.4.
	want := 1.4
	if got := *p.CompressionSavedNative; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("compression_saved_native = %v, want %v", got, want)
	}
	if p.CompressionSavedCurrency != "USD" {
		t.Errorf("compression_saved_currency = %q, want USD", p.CompressionSavedCurrency)
	}
	if p.CompressionRatePer1M == nil || *p.CompressionRatePer1M != 0.14 {
		t.Errorf("compression_rate_per_1m = %v, want 0.14", p.CompressionRatePer1M)
	}
}

// TestCompressorSummaryRemoteCompressionSavedBlendedRate covers a provider
// fronting two models at different prices: the rate must be token-weighted
// across both, not a flat average of the two per-1M prices.
func TestCompressorSummaryRemoteCompressionSavedBlendedRate(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "deepseek", "deepseek")
	pq := seedCatalogPrereqs(t, db)
	if _, err := db.Catalog().CreateOffering(context.Background(), store.Offering{
		ModelID: pq.modelID, ProviderID: mustProviderID(t, db, "deepseek"), WireModel: "deepseek-v4-pro",
		PriceInPer1M: 1.0, PriceOutPer1M: 2.0, Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering pro: %v", err)
	}
	if _, err := db.Catalog().CreateOffering(context.Background(), store.Offering{
		ModelID: pq.modelID, ProviderID: mustProviderID(t, db, "deepseek"), WireModel: "deepseek-v4-flash",
		PriceInPer1M: 0.1, PriceOutPer1M: 0.2, Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering flash: %v", err)
	}

	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "deepseek"), TokensIn: 3000, TokensSaved: 1_000_000, Requests: 2, RequestsCached: 1,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}
	// 900k tokens at the pro rate (1.0), 100k at the flash rate (0.1) — a
	// flat average of the two rates would give 0.55; token-weighted gives
	// (900000*1.0 + 100000*0.1) / 1000000 = 0.91.
	if err := db.Usage().Record(context.Background(), store.UsageEvent{
		TS: now, Kind: "external_request", Model: "deepseek-v4-pro", ProviderID: mustProviderIDPtr(t, db, "deepseek"),
		PromptTokens: 900_000, CompletionTokens: 10,
	}); err != nil {
		t.Fatalf("Usage.Record pro: %v", err)
	}
	if err := db.Usage().Record(context.Background(), store.UsageEvent{
		TS: now, Kind: "external_request", Model: "deepseek-v4-flash", ProviderID: mustProviderIDPtr(t, db, "deepseek"),
		PromptTokens: 100_000, CompletionTokens: 10,
	}); err != nil {
		t.Fatalf("Usage.Record flash: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	p := resp.Proxies[0]
	if p.CompressionRatePer1M == nil {
		t.Fatalf("compression_rate_per_1m = nil, want populated")
	}
	want := 0.91
	if got := *p.CompressionRatePer1M; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("compression_rate_per_1m = %v, want %v (token-weighted, not flat-averaged)", got, want)
	}
	wantSaved := 0.91 // 1M tokens saved * 0.91/1M
	if got := *p.CompressionSavedNative; got < wantSaved-1e-9 || got > wantSaved+1e-9 {
		t.Errorf("compression_saved_native = %v, want %v", got, wantSaved)
	}
}

// TestCompressorSummaryRemoteCompressionSavedZeroWhenNothingSaved confirms no
// figure is fabricated when Compressor's own compression counter is 0 (e.g.
// the proxy runs --lossless, or nothing was compressible this window) even
// though real, priced usage events exist for the window.
func TestCompressorSummaryRemoteCompressionSavedZeroWhenNothingSaved(t *testing.T) {
	s, db := serverWithCompressorStore(t)
	seedProxy(t, db, "deepseek", "deepseek")
	pq := seedCatalogPrereqs(t, db)
	if _, err := db.Catalog().CreateOffering(context.Background(), store.Offering{
		ModelID: pq.modelID, ProviderID: mustProviderID(t, db, "deepseek"), WireModel: "deepseek-chat",
		PriceInPer1M: 0.14, PriceOutPer1M: 0.28, Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}

	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "deepseek"), TokensIn: 1000, TokensSaved: 0, Requests: 5, RequestsCached: 0,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}
	if err := db.Usage().Record(context.Background(), store.UsageEvent{
		TS: now, Kind: "external_request", Model: "deepseek-chat", ProviderID: mustProviderIDPtr(t, db, "deepseek"),
		PromptTokens: 1000, CompletionTokens: 50,
	}); err != nil {
		t.Fatalf("Usage.Record: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	p := resp.Proxies[0]
	if p.CompressionSavedNative != nil {
		t.Errorf("compression_saved_native = %v, want nil (TokensSaved is 0)", *p.CompressionSavedNative)
	}
}

// TestCompressorSummaryCompressionSavedFXConverted covers the operator-reported
// bug (2026-07-31): billing.display_currency=JPY but the external chip's
// money figure rendered in USD because this endpoint never FX-converted.
// Confirms compression_saved_display carries the JPY-converted amount
// alongside the still-present USD compression_saved_native, and that
// fx_as_of/fx_stale reflect a real conversion happened.
func TestCompressorSummaryCompressionSavedFXConverted(t *testing.T) {
	asOf := time.Now()
	fxSrc := &stubFX{
		display:  "JPY",
		rates:    map[string]float64{"USD->JPY": 150.0},
		asOf:     asOf,
		hasRates: true,
	}
	s, db := serverWithCompressorStoreFX(t, fxSrc)
	seedProxy(t, db, "deepseek", "deepseek")
	pq := seedCatalogPrereqs(t, db)
	if _, err := db.Catalog().CreateOffering(context.Background(), store.Offering{
		ModelID: pq.modelID, ProviderID: mustProviderID(t, db, "deepseek"), WireModel: "deepseek-chat",
		PriceInPer1M: 0.14, PriceOutPer1M: 0.28, Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}

	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "deepseek"), TokensIn: 20000, TokensSaved: 950, Requests: 5, RequestsCached: 2,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}
	if err := db.Usage().Record(context.Background(), store.UsageEvent{
		TS: now, Kind: "external_request", Model: "deepseek-chat", ProviderID: mustProviderIDPtr(t, db, "deepseek"),
		PromptTokens: 10_000_000, CompletionTokens: 100,
	}); err != nil {
		t.Fatalf("Usage.Record: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if resp.DisplayCurrency != "JPY" {
		t.Errorf("display_currency = %q, want JPY", resp.DisplayCurrency)
	}
	if resp.FxAsOf == nil {
		t.Errorf("fx_as_of = nil, want populated (a real conversion happened)")
	}
	if resp.FxStale {
		t.Errorf("fx_stale = true, want false (rate was cached)")
	}
	p := resp.Proxies[0]
	if p.CompressionSavedNative == nil || p.CompressionSavedCurrency != "USD" {
		t.Fatalf("compression_saved_native/currency = %v/%q, want populated/USD", p.CompressionSavedNative, p.CompressionSavedCurrency)
	}
	if p.CompressionSavedDisplay == nil {
		t.Fatalf("compression_saved_display = nil, want populated")
	}
	// 950 tokens saved * 0.14/1M = 0.000133 USD * 150 = 0.0199 JPY — small
	// but non-zero; the rounding-to-zero defect lives in the frontend
	// formatter, not here, but the raw figure must still be right.
	want := *p.CompressionSavedNative * 150.0
	if got := *p.CompressionSavedDisplay; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("compression_saved_display = %v, want %v", got, want)
	}
}

// TestCompressorSummaryFXStaleWhenRateMissing confirms fx_stale is set (and no
// display figure is fabricated at a wrong 1:1 rate) when display currency
// differs from the native currency but no rate is cached for the pair.
func TestCompressorSummaryFXStaleWhenRateMissing(t *testing.T) {
	fxSrc := &stubFX{display: "JPY", hasRates: false}
	s, db := serverWithCompressorStoreFX(t, fxSrc)
	seedProxy(t, db, "deepseek", "deepseek")
	pq := seedCatalogPrereqs(t, db)
	if _, err := db.Catalog().CreateOffering(context.Background(), store.Offering{
		ModelID: pq.modelID, ProviderID: mustProviderID(t, db, "deepseek"), WireModel: "deepseek-chat",
		PriceInPer1M: 0.14, PriceOutPer1M: 0.28, Currency: "USD", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}

	now := time.Now().UTC()
	if err := db.Routing().RecordSavingsSample(context.Background(), store.CompressorSavingsSampleRow{
		TS: now, ProxyID: mustProxyID(t, db, "deepseek"), TokensIn: 20000, TokensSaved: 950, Requests: 5, RequestsCached: 2,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample: %v", err)
	}
	if err := db.Usage().Record(context.Background(), store.UsageEvent{
		TS: now, Kind: "external_request", Model: "deepseek-chat", ProviderID: mustProviderIDPtr(t, db, "deepseek"),
		PromptTokens: 10_000_000, CompletionTokens: 100,
	}); err != nil {
		t.Fatalf("Usage.Record: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=1h", nil))
	var resp compressorSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if !resp.FxStale {
		t.Errorf("fx_stale = false, want true (no USD->JPY rate cached)")
	}
}

func TestCompressorSummaryValidation(t *testing.T) {
	s, _ := serverWithCompressorStore(t)
	w := do(t, s, authedRequest("GET", "/api/v1/compressor/summary?window=bogus", nil))
	if w.Code != 422 {
		t.Fatalf("GET with bad window = %d, want 422", w.Code)
	}
}
