// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// newUsageHeatmapTestServer builds a Server around a real (in-memory)
// store.DB's Usage view — the heatmap handler needs nothing else (no
// catalog/registry/FX), unlike the cost-calc tests in model_cards_test.go.
func newUsageHeatmapTestServer(t *testing.T, db *store.DB) *Server {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Usage:     db.Usage(),
	})
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsageHeatmap_NilStore_EmptyResponse(t *testing.T) {
	s := newTestServer(t) // Deps.Usage is nil
	w := do(t, s, authedRequest("GET", "/api/v1/usage/heatmap", nil))
	if w.Code != 200 {
		t.Fatalf("heatmap = %d: %s", w.Code, w.Body.String())
	}
	var resp usageHeatmapResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Days) != 0 {
		t.Fatalf("days = %d, want 0 (nil store)", len(resp.Days))
	}
}

// TestUsageHeatmap_DenseZeroFill is the regression test for the design's
// core requirement: every calendar day in the window appears, even ones with
// zero traffic, so the frontend never has to gap-fill a GitHub-style grid.
func TestUsageHeatmap_DenseZeroFill(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := newUsageHeatmapTestServer(t, db)
	w := do(t, s, authedRequest("GET", "/api/v1/usage/heatmap?window=7d&tz=UTC", nil))
	if w.Code != 200 {
		t.Fatalf("heatmap = %d: %s", w.Code, w.Body.String())
	}
	var resp usageHeatmapResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Days) != 7 {
		t.Fatalf("days = %d, want 7 (dense, no traffic recorded)", len(resp.Days))
	}
	for _, d := range resp.Days {
		if d.Tokens != 0 || d.Requests != 0 {
			t.Errorf("day %s = (%d tokens, %d requests), want (0, 0)", d.Date, d.Tokens, d.Requests)
		}
	}
	// Days must be contiguous and end on today (UTC).
	today := time.Now().UTC().Format("2006-01-02")
	if resp.Days[len(resp.Days)-1].Date != today {
		t.Errorf("last day = %s, want today (%s)", resp.Days[len(resp.Days)-1].Date, today)
	}
}

// TestUsageHeatmap_BucketsByLocalCalendarDay is the regression test for the
// design's explicit departure from /metrics/history's and
// /cost/energy-history's from-anchored modulo bucketing: a heatmap cell must
// be a real calendar day in the requested tz, not a floating window-relative
// bucket. A timestamp just after local midnight in Tokyo (UTC+9) falls on the
// PREVIOUS UTC calendar day — bucketing in UTC would silently misfile it.
func TestUsageHeatmap_BucketsByLocalCalendarDay(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("Asia/Tokyo tzdata unavailable: %v", err)
	}
	// 2026-08-05 00:30 JST = 2026-08-04 15:30 UTC.
	jstMidnightPlus := time.Date(2026, 8, 5, 0, 30, 0, 0, loc)

	if err := db.Usage().Record(t.Context(), store.UsageEvent{
		TS: jstMidnightPlus, Kind: "inference", Model: "gemma4-e2b",
		PromptTokens: 1000, CompletionTokens: 234,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	s := newUsageHeatmapTestServer(t, db)

	// Query the window from a fixed reference "now" isn't possible (the
	// handler uses time.Now()), so use a wide-enough window that the fixed
	// 2026-08-05 event is guaranteed to fall inside it regardless of when
	// the test runs, and assert on which bucket it landed in rather than
	// dense-day count.
	w := do(t, s, authedRequest("GET", "/api/v1/usage/heatmap?window=3650d&tz=Asia%2FTokyo", nil))
	if w.Code != 200 {
		t.Fatalf("heatmap (JST) = %d: %s", w.Code, w.Body.String())
	}
	var jstResp usageHeatmapResponse
	decodeJSON(t, w.Body, &jstResp)
	var jstDay *usageHeatmapDay
	for i := range jstResp.Days {
		if jstResp.Days[i].Date == "2026-08-05" {
			jstDay = &jstResp.Days[i]
		}
	}
	if jstDay == nil || jstDay.Tokens != 1234 {
		t.Fatalf("JST bucket 2026-08-05 = %+v, want 1234 tokens", jstDay)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/usage/heatmap?window=3650d&tz=UTC", nil))
	if w.Code != 200 {
		t.Fatalf("heatmap (UTC) = %d: %s", w.Code, w.Body.String())
	}
	var utcResp usageHeatmapResponse
	decodeJSON(t, w.Body, &utcResp)
	for _, d := range utcResp.Days {
		if d.Date == "2026-08-05" && d.Tokens != 0 {
			t.Errorf("UTC bucket 2026-08-05 = %+v, want 0 tokens — the event's UTC instant falls on 2026-08-04", d)
		}
		if d.Date == "2026-08-04" && d.Tokens != 1234 {
			t.Errorf("UTC bucket 2026-08-04 = %+v, want 1234 tokens", d)
		}
	}
}

// TestUsageHeatmap_LocalExternalSplit is the regression test for the
// ALL/Local/External toggle: local ("inference") and external
// ("external_request") token/request counts must land in their own fields,
// additive to (not instead of) the combined Tokens/Requests totals.
func TestUsageHeatmap_LocalExternalSplit(t *testing.T) {
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

	now := time.Now()
	events := []store.UsageEvent{
		{TS: now, Kind: "inference", Model: "gemma4-e2b", PromptTokens: 100, CompletionTokens: 50},
		{TS: now, Kind: "external_request", Model: "deepseek-v4-flash", ProviderID: &deepseek.ID, PromptTokens: 200, CompletionTokens: 20},
		{TS: now, Kind: "external_request", Model: "deepseek-v4-flash", ProviderID: &deepseek.ID, PromptTokens: 30, CompletionTokens: 10},
	}
	for _, e := range events {
		if err := db.Usage().Record(t.Context(), e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	s := newUsageHeatmapTestServer(t, db)
	w := do(t, s, authedRequest("GET", "/api/v1/usage/heatmap?window=1d&tz=UTC", nil))
	if w.Code != 200 {
		t.Fatalf("heatmap = %d: %s", w.Code, w.Body.String())
	}
	var resp usageHeatmapResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Days) != 1 {
		t.Fatalf("days = %d, want 1", len(resp.Days))
	}
	d := resp.Days[0]
	if d.Tokens != 410 || d.Requests != 3 {
		t.Errorf("combined = (%d tokens, %d requests), want (410, 3)", d.Tokens, d.Requests)
	}
	if d.TokensLocal != 150 || d.RequestsLocal != 1 {
		t.Errorf("local = (%d tokens, %d requests), want (150, 1)", d.TokensLocal, d.RequestsLocal)
	}
	if d.TokensExternal != 260 || d.RequestsExternal != 2 {
		t.Errorf("external = (%d tokens, %d requests), want (260, 2)", d.TokensExternal, d.RequestsExternal)
	}
}

func TestUsageHeatmap_UnknownTZFallsBackToUTC(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := newUsageHeatmapTestServer(t, db)
	w := do(t, s, authedRequest("GET", "/api/v1/usage/heatmap?tz=Not%2FAZone", nil))
	if w.Code != 200 {
		t.Fatalf("heatmap = %d: %s", w.Code, w.Body.String())
	}
	var resp usageHeatmapResponse
	decodeJSON(t, w.Body, &resp)
	if resp.TZ != "UTC" {
		t.Errorf("tz = %q, want UTC fallback for an unloadable zone", resp.TZ)
	}
	if resp.Window != "84d" {
		t.Errorf("window = %q, want default 84d", resp.Window)
	}
}
