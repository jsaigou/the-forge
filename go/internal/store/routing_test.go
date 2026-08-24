// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

func f64ptr(v float64) *float64 { return &v }

// seedProxy creates a minimal compressor_proxies row and returns its id —
// compressor_savings_samples/compressor_label_samples are FK'd to compressor_proxies.id
// since the 0042 surrogate-key migration, so tests recording samples need a
// real parent row to point at.
func seedProxy(t *testing.T, db *DB, service string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := db.Routing().SaveProxy(ctx, ProxyRow{
		Service: service, Port: 9000, TargetURL: "http://x", Unit: "headroom@" + service,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seedProxy(%q): %v", service, err)
	}
	proxies, err := db.Routing().Proxies(ctx)
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}
	for _, p := range proxies {
		if p.Service == service {
			return p.ID
		}
	}
	t.Fatalf("seedProxy(%q): not found after save", service)
	return 0
}

func TestRecordCompressorSampleAndSummary(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	h := db.Routing()

	localID := seedProxy(t, db, "local")
	deepseekID := seedProxy(t, db, "deepseek")

	base := time.Unix(1700000000, 0).UTC()
	if err := h.RecordSavingsSample(ctx, CompressorSavingsSampleRow{
		TS: base, ProxyID: localID,
		TokensIn: 1000, TokensOut: 200,
		Requests: 10, RequestsCached: 3, RequestsFailed: 1, RequestsRateLimited: 0,
		TTFBCount: 10, TTFBSumMs: 500, TTFBMinMs: f64ptr(20), TTFBMaxMs: f64ptr(80),
		LatencyCount: 10, LatencySumMs: 4000, LatencyMinMs: f64ptr(200), LatencyMaxMs: f64ptr(900),
		OverheadCount: 10, OverheadSumMs: 50, OverheadMinMs: f64ptr(2), OverheadMaxMs: f64ptr(9),
	}, []CompressorLabelSample{
		{TS: base, ProxyID: localID, LabelKey: "provider", LabelValue: "openai", Metric: "requests", Delta: 8},
		{TS: base, ProxyID: localID, LabelKey: "provider", LabelValue: "anthropic", Metric: "requests", Delta: 2},
	}); err != nil {
		t.Fatalf("RecordSavingsSample 1: %v", err)
	}

	// Second sample, later, with a higher TTFB max (must become the "most
	// recent" gauge) and a lower latency max (must NOT overwrite with a
	// smaller value if it were summed/averaged — it's the latest raw value
	// regardless of magnitude, since these are point-in-time gauges).
	next := base.Add(time.Minute)
	if err := h.RecordSavingsSample(ctx, CompressorSavingsSampleRow{
		TS: next, ProxyID: localID,
		TokensIn: 500, TokensOut: 100,
		Requests: 5, RequestsCached: 1, RequestsFailed: 0, RequestsRateLimited: 1,
		TTFBCount: 5, TTFBSumMs: 300, TTFBMinMs: f64ptr(15), TTFBMaxMs: f64ptr(95),
		LatencyCount: 5, LatencySumMs: 1500, LatencyMinMs: f64ptr(180), LatencyMaxMs: f64ptr(300),
		OverheadCount: 5, OverheadSumMs: 30, OverheadMinMs: f64ptr(1), OverheadMaxMs: f64ptr(6),
	}, []CompressorLabelSample{
		{TS: next, ProxyID: localID, LabelKey: "provider", LabelValue: "openai", Metric: "requests", Delta: 4},
		{TS: next, ProxyID: localID, LabelKey: "model", LabelValue: "gemma4-e2b", Metric: "requests", Delta: 5},
	}); err != nil {
		t.Fatalf("RecordSavingsSample 2: %v", err)
	}

	// Sample for a second proxy, to prove per-proxy isolation.
	if err := h.RecordSavingsSample(ctx, CompressorSavingsSampleRow{
		TS: base, ProxyID: deepseekID,
		TokensIn: 9999, Requests: 50,
	}, nil); err != nil {
		t.Fatalf("RecordSavingsSample deepseek: %v", err)
	}

	summary, err := h.SavingsSummary(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("SavingsSummary: %v", err)
	}

	local, ok := summary["local"]
	if !ok {
		t.Fatalf("no summary for proxy 'local': %+v", summary)
	}
	if local.TokensIn != 1500 {
		t.Errorf("TokensIn = %d, want 1500 (summed)", local.TokensIn)
	}
	if local.TokensOut != 300 {
		t.Errorf("TokensOut = %d, want 300 (summed)", local.TokensOut)
	}
	if local.Requests != 15 || local.RequestsCached != 4 || local.RequestsFailed != 1 || local.RequestsRateLimited != 1 {
		t.Errorf("request counters wrong: %+v", local)
	}
	if local.TTFBCount != 15 || local.TTFBSumMs != 800 {
		t.Errorf("ttfb count/sum wrong: count=%v sum=%v", local.TTFBCount, local.TTFBSumMs)
	}
	// Most-recent-sample semantics: the second (later) sample's min/max wins,
	// not a min-of-mins or max-of-maxes across the window.
	if local.TTFBMinMs == nil || *local.TTFBMinMs != 15 {
		t.Errorf("TTFBMinMs = %v, want 15 (the latest sample's value)", local.TTFBMinMs)
	}
	if local.TTFBMaxMs == nil || *local.TTFBMaxMs != 95 {
		t.Errorf("TTFBMaxMs = %v, want 95 (the latest sample's value)", local.TTFBMaxMs)
	}
	if local.LatencyMaxMs == nil || *local.LatencyMaxMs != 300 {
		t.Errorf("LatencyMaxMs = %v, want 300 (the latest sample's own max, even though it's lower than the earlier sample's 900)", local.LatencyMaxMs)
	}

	if local.RequestsByProvider["openai"] != 12 {
		t.Errorf("RequestsByProvider[openai] = %d, want 12 (8+4 summed)", local.RequestsByProvider["openai"])
	}
	if local.RequestsByProvider["anthropic"] != 2 {
		t.Errorf("RequestsByProvider[anthropic] = %d, want 2", local.RequestsByProvider["anthropic"])
	}
	if local.RequestsByModel["gemma4-e2b"] != 5 {
		t.Errorf("RequestsByModel[gemma4-e2b] = %d, want 5", local.RequestsByModel["gemma4-e2b"])
	}

	deepseek, ok := summary["deepseek"]
	if !ok {
		t.Fatalf("no summary for proxy 'deepseek'")
	}
	if deepseek.TokensIn != 9999 || deepseek.Requests != 50 {
		t.Errorf("deepseek summary wrong (proxy isolation broken?): %+v", deepseek)
	}
	if deepseek.TTFBMinMs != nil {
		t.Errorf("deepseek TTFBMinMs = %v, want nil (never recorded)", deepseek.TTFBMinMs)
	}
}

func TestCompressorSummaryWindowExcludesOldSamples(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	h := db.Routing()

	localID := seedProxy(t, db, "local")

	old := time.Unix(1700000000, 0).UTC()
	recent := old.Add(2 * time.Hour)
	if err := h.RecordSavingsSample(ctx, CompressorSavingsSampleRow{TS: old, ProxyID: localID, TokensIn: 100, Requests: 1}, nil); err != nil {
		t.Fatalf("record old: %v", err)
	}
	if err := h.RecordSavingsSample(ctx, CompressorSavingsSampleRow{TS: recent, ProxyID: localID, TokensIn: 10, Requests: 1}, nil); err != nil {
		t.Fatalf("record recent: %v", err)
	}

	summary, err := h.SavingsSummary(ctx, old.Add(time.Hour))
	if err != nil {
		t.Fatalf("SavingsSummary: %v", err)
	}
	local := summary["local"]
	if local.TokensIn != 10 {
		t.Errorf("TokensIn = %d, want 10 (the old sample must be excluded by the window)", local.TokensIn)
	}
}

func TestCompressorSummaryEmpty(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	summary, err := db.Routing().SavingsSummary(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SavingsSummary: %v", err)
	}
	if len(summary) != 0 {
		t.Errorf("summary = %+v, want empty", summary)
	}
}
