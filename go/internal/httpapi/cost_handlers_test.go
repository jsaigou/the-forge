// SPDX-License-Identifier: Apache-2.0

package httpapi

// cost_handlers_test.go — tests for the measured-electricity cost/savings
// sprint's Phase 2 (see /home/testuser/.claude/plans/joyful-splashing-moonbeam.md):
// computeEnergy's integration rules, the median/percentile helpers, the
// GET/PUT infra.cost settings endpoints, and the cost/summary +
// cost/energy-history HTTP handlers.

import (
	"context"
	"net/http"
	"strings"
	"sync"
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

// ── computeEnergy ────────────────────────────────────────────────────────

func f64p(v float64) *float64 { return &v }
func intp(v int) *int         { return &v }

// flatCost is a Cost with no overhead/loss, so WallWatts is an identity
// function — isolates the integration math from the wall-power adjustment.
var flatCost = config.Cost{PowerKW: 0.14, RatePerKWh: 0.21, RateCurrency: "USD", OverheadW: 0, PSUEfficiency: 1}

func sampleAt(t time.Time, packageW *float64, gpuPct *float64, activeSlots *int) store.MetricSample {
	return store.MetricSample{TS: t, PackagePowerW: packageW, GPUUsePct: gpuPct, ActiveSlots: activeSlots}
}

// block builds n samples spaced step apart starting at start, with wattage
// interpolated linearly from startW to endW (constant when equal). Every
// gap between consecutive samples is step, which must stay under the test's
// gap threshold (default sampleIntervalS=60 -> maxGapS=300) so the whole
// block integrates as MEASURED rather than tripping gap detection — that
// path is exercised separately by TestComputeEnergyGapNotIntegrated.
func block(start time.Time, n int, step time.Duration, startW, endW float64, gpuPct *float64, slots *int) []store.MetricSample {
	out := make([]store.MetricSample, n)
	for i := 0; i < n; i++ {
		frac := float64(i) / float64(n-1)
		w := startW + (endW-startW)*frac
		out[i] = sampleAt(start.Add(time.Duration(i)*step), f64p(w), gpuPct, slots)
	}
	return out
}

func TestComputeEnergyConstantPowerOneHour(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	samples := block(base, 61, time.Minute, 100, 100, nil, nil)
	e := computeEnergy(samples, flatCost, 60)
	if got := round6(e.WallWhEst); got != 100 {
		t.Errorf("constant 100W for 1h: WallWhEst = %v, want 100", got)
	}
	if got := round6(e.PackageWh); got != 100 {
		t.Errorf("constant 100W for 1h: PackageWh = %v, want 100", got)
	}
	if e.MeasuredSeconds != 3600 {
		t.Errorf("MeasuredSeconds = %v, want 3600", e.MeasuredSeconds)
	}
	if e.GapSeconds != 0 || e.UnmeasuredSeconds != 0 {
		t.Errorf("gap/unmeasured should be 0, got gap=%v unmeasured=%v", e.GapSeconds, e.UnmeasuredSeconds)
	}
}

func TestComputeEnergyTrapezoidRamp(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	// Trapezoidal integration of a piecewise-linear ramp is exact regardless
	// of how finely it's sampled, so this also proves the integrator isn't
	// just averaging the two endpoints as if the power were constant.
	samples := block(base, 61, time.Minute, 0, 100, nil, nil)
	e := computeEnergy(samples, flatCost, 60)
	if got := round6(e.WallWhEst); got != 50 {
		t.Errorf("0->100W ramp over 1h: WallWhEst = %v, want 50 (trapezoid)", got)
	}
}

func TestComputeEnergyGapNotIntegrated(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	// sampleIntervalS=60 -> gap threshold = max(180, 300) = 300s. A 2h gap
	// must land entirely in GapSeconds, contributing zero energy, never
	// trapezoided as if 100W held for the whole gap.
	samples := []store.MetricSample{
		sampleAt(base, f64p(100), nil, nil),
		sampleAt(base.Add(2*time.Hour), f64p(100), nil, nil),
	}
	e := computeEnergy(samples, flatCost, 60)
	if e.GapSeconds != 2*3600 {
		t.Errorf("GapSeconds = %v, want %v", e.GapSeconds, 2*3600)
	}
	if e.MeasuredSeconds != 0 {
		t.Errorf("MeasuredSeconds = %v, want 0 (all in gap)", e.MeasuredSeconds)
	}
	if e.WallWhEst != 0 || e.PackageWh != 0 {
		t.Errorf("energy should be 0 across a gap, got wall=%v package=%v", e.WallWhEst, e.PackageWh)
	}
}

func TestComputeEnergyNilPowerIsUnmeasuredNotZero(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	samples := []store.MetricSample{
		sampleAt(base, f64p(100), nil, nil),
		sampleAt(base.Add(time.Minute), nil, nil, nil), // sensor dropout
		sampleAt(base.Add(2*time.Minute), f64p(100), nil, nil),
	}
	e := computeEnergy(samples, flatCost, 60)
	if e.UnmeasuredSeconds != 120 {
		t.Errorf("UnmeasuredSeconds = %v, want 120 (both intervals touch the nil sample)", e.UnmeasuredSeconds)
	}
	if e.MeasuredSeconds != 0 {
		t.Errorf("MeasuredSeconds = %v, want 0 — a nil endpoint must never be treated as 0W", e.MeasuredSeconds)
	}
}

func TestComputeEnergyIdleActiveSplit(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	// Two 1h regimes separated by a real 20-minute gap (>> the 300s
	// threshold) so the cross-regime interval lands in GapSeconds instead
	// of polluting the idle/active totals with a 50W->150W jump attributed
	// to whichever side happens to lead.
	idle := block(base, 61, time.Minute, 50, 50, f64p(0), nil)
	active := block(base.Add(80*time.Minute), 61, time.Minute, 150, 150, f64p(50), nil)
	samples := append(idle, active...)
	e := computeEnergy(samples, flatCost, 60)
	if round6(e.IdleWhEst) != 50 {
		t.Errorf("IdleWhEst = %v, want 50 (1h @ 50W)", round6(e.IdleWhEst))
	}
	if round6(e.ActiveWhEst) != 150 {
		t.Errorf("ActiveWhEst = %v, want 150 (1h @ 150W)", round6(e.ActiveWhEst))
	}
	if round6(e.IdleBaselineW) != 50 {
		t.Errorf("IdleBaselineW = %v, want 50 (median of the one idle interval)", round6(e.IdleBaselineW))
	}
	if e.ActiveSeconds != 3600 {
		t.Errorf("ActiveSeconds = %v, want 3600", e.ActiveSeconds)
	}
	wantAttrib := 150.0 - 50.0*3600/3600
	if round6(e.AttributableWhEst()) != round6(wantAttrib) {
		t.Errorf("AttributableWhEst = %v, want %v", round6(e.AttributableWhEst()), round6(wantAttrib))
	}
}

func TestComputeEnergyAttributableClampedAtZero(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	// Active interval cheaper than the idle baseline (noisy sensor) must not
	// produce a negative "cost of running models." Regimes separated by a
	// real gap, as in TestComputeEnergyIdleActiveSplit.
	idle := block(base, 61, time.Minute, 100, 100, f64p(0), nil)
	active := block(base.Add(80*time.Minute), 61, time.Minute, 10, 10, f64p(50), nil)
	samples := append(idle, active...)
	e := computeEnergy(samples, flatCost, 60)
	if e.AttributableWhEst() != 0 {
		t.Errorf("AttributableWhEst = %v, want 0 (clamped, not negative)", e.AttributableWhEst())
	}
}

func TestComputeEnergySingleMultiSlotSplit(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	single := block(base, 61, time.Minute, 100, 100, nil, intp(1))
	multi := block(base.Add(80*time.Minute), 61, time.Minute, 100, 100, nil, intp(3))
	samples := append(single, multi...)
	e := computeEnergy(samples, flatCost, 60)
	if e.SingleSlotSeconds != 3600 {
		t.Errorf("SingleSlotSeconds = %v, want 3600", e.SingleSlotSeconds)
	}
	if e.MultiSlotSeconds != 3600 {
		t.Errorf("MultiSlotSeconds = %v, want 3600", e.MultiSlotSeconds)
	}
}

func TestComputeEnergyCalibrationCollectsActiveSingleSlotOnly(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	// Each regime separated by a real 10-minute gap (>> the 300s threshold)
	// so a cross-regime interval never gets attributed to either side's
	// classification — otherwise the boundary interval (leading sample
	// governs) would leak an extra, wrongly-classified entry into the set.
	activeSingle := []store.MetricSample{
		sampleAt(base, f64p(120), f64p(50), intp(1)),
		sampleAt(base.Add(time.Minute), f64p(120), f64p(50), intp(1)),
	}
	activeMulti := []store.MetricSample{
		sampleAt(base.Add(11*time.Minute), f64p(200), f64p(50), intp(2)),
		sampleAt(base.Add(12*time.Minute), f64p(200), f64p(50), intp(2)),
	}
	idleSingle := []store.MetricSample{
		sampleAt(base.Add(22*time.Minute), f64p(20), f64p(0), intp(1)),
		sampleAt(base.Add(23*time.Minute), f64p(20), f64p(0), intp(1)),
	}
	samples := append(append(activeSingle, activeMulti...), idleSingle...)
	e := computeEnergy(samples, flatCost, 60)
	if len(e.activeSingleSlotWallW) != 1 {
		t.Fatalf("activeSingleSlotWallW = %v, want exactly 1 entry (the active+single-slot interval)", e.activeSingleSlotWallW)
	}
	if e.activeSingleSlotWallW[0] != 120 {
		t.Errorf("activeSingleSlotWallW[0] = %v, want 120", e.activeSingleSlotWallW[0])
	}
}

func TestComputeEnergyNonPositiveDtSkipped(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	// Out-of-order/duplicate timestamps must not contribute negative or
	// zero-duration energy.
	samples := []store.MetricSample{
		sampleAt(base, f64p(100), nil, nil),
		sampleAt(base, f64p(100), nil, nil), // dt == 0
	}
	e := computeEnergy(samples, flatCost, 60)
	if e.MeasuredSeconds != 0 || e.WallWhEst != 0 {
		t.Errorf("dt<=0 interval must be skipped entirely, got measured=%v wallWh=%v", e.MeasuredSeconds, e.WallWhEst)
	}
}

func TestComputeEnergyWallAdjustment(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	cost := config.Cost{PowerKW: 0.14, RatePerKWh: 0.21, RateCurrency: "USD", OverheadW: 25, PSUEfficiency: 0.9}
	samples := block(base, 61, time.Minute, 100, 100, nil, nil)
	e := computeEnergy(samples, cost, 60)
	wantWallW := (100.0 + 25) / 0.9
	if round6(e.WallWhEst) != round6(wantWallW) {
		t.Errorf("WallWhEst = %v, want %v (package Wh unaffected: %v)", round6(e.WallWhEst), round6(wantWallW), round6(e.PackageWh))
	}
	if round6(e.PackageWh) != 100 {
		t.Errorf("PackageWh = %v, want 100 (raw, unadjusted)", round6(e.PackageWh))
	}
}

// ── median / percentile ──────────────────────────────────────────────────

func TestMedian(t *testing.T) {
	cases := []struct {
		name string
		vals []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{7}, 7},
		{"odd", []float64{3, 1, 2}, 2},
		{"even", []float64{1, 2, 3, 4}, 2.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := median(c.vals); got != c.want {
				t.Errorf("median(%v) = %v, want %v", c.vals, got, c.want)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	// Nearest-rank on these 10 values: rank(p) = round(p/100*9). p50 ->
	// round(4.5) = 5 -> sorted[5] = 60, not the interpolated 50 a
	// linear-interpolation percentile would give — this pins down which
	// convention computeEnergy's calibration block actually uses.
	vals := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile(nil, 50) = %v, want 0", got)
	}
	if got := percentile(vals, 50); got != 60 {
		t.Errorf("percentile(vals, 50) = %v, want 60 (nearest-rank)", got)
	}
	if got := percentile(vals, 95); got != 100 {
		t.Errorf("percentile(vals, 95) = %v, want 100 (nearest-rank)", got)
	}
	if got := percentile(vals, 0); got != 10 {
		t.Errorf("percentile(vals, 0) = %v, want 10 (the minimum)", got)
	}
	// Order independence.
	shuffled := []float64{100, 10, 90, 20, 80, 30, 70, 40, 60, 50}
	if got := percentile(shuffled, 50); got != 60 {
		t.Errorf("percentile(shuffled, 50) = %v, want 60", got)
	}
}

// ── fakes for HTTP-level tests ───────────────────────────────────────────

type fakeAudit struct {
	mu      sync.Mutex
	entries []store.AuditEntry
}

func (f *fakeAudit) Write(_ context.Context, e store.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

// List satisfies store.Audit (Sprint C added it) — filters by
// actionPrefix/target the same way the real auditView.List does, so tests
// exercising handleAuditList against this fake see realistic behavior.
func (f *fakeAudit) List(_ context.Context, actionPrefix, target string, limit int) ([]store.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AuditEntry
	for i := len(f.entries) - 1; i >= 0; i-- {
		e := f.entries[i]
		if actionPrefix != "" && !strings.HasPrefix(e.Action, actionPrefix) {
			continue
		}
		if target != "" && e.Target != target {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// fakeFX is a minimal fx.Source with a fixed rate table, for exercising the
// rate_currency != display_currency conversion path deterministically.
type fakeFX struct {
	display string
	rates   map[[2]string]float64
}

func (f *fakeFX) DisplayCurrency(context.Context) string { return f.display }
func (f *fakeFX) BillCurrency(context.Context, string) string { return f.display }
func (f *fakeFX) Rate(_ context.Context, from, to string) (float64, bool) {
	if from == to {
		return 1, true
	}
	r, ok := f.rates[[2]string{from, to}]
	return r, ok
}
func (f *fakeFX) Provenance(context.Context) (time.Time, bool, bool) {
	return time.Unix(1700000000, 0), false, true
}

var _ fx.Source = (*fakeFX)(nil)

// newCostTestServer builds a Server with a real in-memory store.Metrics (so
// computeEnergy exercises real SQL Range reads), fake Settings/Audit, and a
// ReloadConfig spy.
func newCostTestServer(t *testing.T) (*Server, *store.DB, *fakeSettings, *fakeAudit, *int) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	fs := newFakeSettings()
	fa := &fakeAudit{}
	reloadCalls := 0
	s := New(Deps{
		Snapshots:    collector.NewStatic(nil),
		Engine:       &engine.Stub{},
		Sched:        &sched.Stub{},
		Auth:         &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:       events,
		Publish:      events,
		Config:       func() *config.Config { return cfg },
		Hostname:     "test-host",
		Metrics:      db.Metrics(),
		Settings:     fs,
		Audit:        fa,
		ReloadConfig: func() { reloadCalls++ },
	})
	t.Cleanup(func() { s.Close() })
	return s, db, fs, fa, &reloadCalls
}

func insertMetricSample(t *testing.T, db *store.DB, ts time.Time, packageW *float64, activeSlots *int) {
	t.Helper()
	if err := db.Metrics().RecordSample(context.Background(), store.MetricSample{
		TS: ts, PackagePowerW: packageW, ActiveSlots: activeSlots,
	}); err != nil {
		t.Fatalf("RecordSample: %v", err)
	}
}

// ── GET/PUT /api/v1/cost/settings ─────────────────────────────────────────

func TestCostSettingsGetDefaults(t *testing.T) {
	s, _, _, _, _ := newCostTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/cost/settings", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp costSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.PowerKW != config.DefaultPowerKW {
		t.Errorf("power_kw = %v, want default %v", resp.PowerKW, config.DefaultPowerKW)
	}
	if resp.OverheadW != config.DefaultOverheadW {
		t.Errorf("overhead_w = %v, want default %v", resp.OverheadW, config.DefaultOverheadW)
	}
	if resp.PSUEfficiency != config.DefaultPSUEfficiency {
		t.Errorf("psu_efficiency = %v, want default %v", resp.PSUEfficiency, config.DefaultPSUEfficiency)
	}
}

func TestCostSettingsPutMergesPartialBody(t *testing.T) {
	s, _, fs, fa, reloadCalls := newCostTestServer(t)
	body := strings.NewReader(`{"overhead_w":40}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/cost/settings", body))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp costSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.OverheadW != 40 {
		t.Errorf("overhead_w = %v, want 40", resp.OverheadW)
	}
	// Untouched fields must retain their resolved defaults, not zero out.
	if resp.PowerKW != config.DefaultPowerKW {
		t.Errorf("power_kw = %v, want untouched default %v", resp.PowerKW, config.DefaultPowerKW)
	}
	if resp.PSUEfficiency != config.DefaultPSUEfficiency {
		t.Errorf("psu_efficiency = %v, want untouched default %v", resp.PSUEfficiency, config.DefaultPSUEfficiency)
	}

	if _, err := fs.Get(context.Background(), SettingInfraCost); err != nil {
		t.Errorf("expected infra.cost to be persisted, Get error: %v", err)
	}
	if *reloadCalls != 1 {
		t.Errorf("ReloadConfig calls = %d, want 1", *reloadCalls)
	}
	if len(fa.entries) != 1 || fa.entries[0].Action != "cost_settings" {
		t.Errorf("expected one cost_settings audit entry, got %+v", fa.entries)
	}
}

func TestCostSettingsPutValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"power_kw zero", `{"power_kw":0}`},
		{"power_kw too high", `{"power_kw":10}`},
		{"rate_per_kwh zero", `{"rate_per_kwh":0}`},
		{"rate_per_kwh too high", `{"rate_per_kwh":1000}`},
		{"rate_currency lowercase", `{"rate_currency":"usd"}`},
		{"rate_currency too short", `{"rate_currency":"US"}`},
		{"overhead_w negative", `{"overhead_w":-1}`},
		{"overhead_w too high", `{"overhead_w":5000}`},
		{"psu_efficiency zero", `{"psu_efficiency":0}`},
		{"psu_efficiency above one", `{"psu_efficiency":1.5}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, _, _, _ := newCostTestServer(t)
			w := do(t, s, authedRequest("PUT", "/api/v1/cost/settings", strings.NewReader(c.body)))
			if w.Code != 422 {
				t.Errorf("PUT %s = %d, want 422: %s", c.body, w.Code, w.Body.String())
			}
		})
	}
}

func TestCostSettingsGetNoSettingsStoreStillWorks(t *testing.T) {
	// GET only reads the resolved (possibly-default) Cost; it must not
	// require Settings to be wired.
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })
	w := do(t, s, authedRequest("GET", "/api/v1/cost/settings", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestCostSettingsPutRequiresSettingsStore(t *testing.T) {
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })
	w := do(t, s, authedRequest("PUT", "/api/v1/cost/settings", strings.NewReader(`{"overhead_w":40}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT with no Settings wired = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}

// ── GET /api/v1/cost/summary ───────────────────────────────────────────────

func TestCostSummaryNoMetricsWired(t *testing.T) {
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })
	w := do(t, s, authedRequest("GET", "/api/v1/cost/summary", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestCostSummaryBasic(t *testing.T) {
	s, db, _, _, _ := newCostTestServer(t)
	now := time.Now().UTC()
	// 60s apart matches the default sample cadence, well under the 300s gap
	// threshold, so this interval integrates as MEASURED.
	insertMetricSample(t, db, now.Add(-time.Minute), f64p(100), intp(1))
	insertMetricSample(t, db, now, f64p(100), intp(1))

	w := do(t, s, authedRequest("GET", "/api/v1/cost/summary?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp costSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Energy.MeasuredSeconds != 60 {
		t.Errorf("measured_seconds = %v, want 60", resp.Energy.MeasuredSeconds)
	}
	if resp.Energy.PackageWh <= 0 {
		t.Errorf("package_wh = %v, want > 0", resp.Energy.PackageWh)
	}
	if resp.Energy.Calibration.SingleSlotActiveWallWP50 != nil {
		t.Errorf("calibration p50 should be nil with < 10 samples, got %v", *resp.Energy.Calibration.SingleSlotActiveWallWP50)
	}
}

// insertActiveSingleSlotSample records a sample with GPUUsePct above the
// idle/active threshold and ActiveSlots=1, so it contributes to the
// calibration percentile set.
func insertActiveSingleSlotSample(t *testing.T, db *store.DB, ts time.Time, packageW float64) {
	t.Helper()
	if err := db.Metrics().RecordSample(context.Background(), store.MetricSample{
		TS: ts, PackagePowerW: f64p(packageW), GPUUsePct: f64p(50), ActiveSlots: intp(1),
	}); err != nil {
		t.Fatalf("RecordSample: %v", err)
	}
}

func TestCostSummaryCalibrationBelowFloorIsNull(t *testing.T) {
	s, db, _, _, _ := newCostTestServer(t)
	now := time.Now().UTC()
	// Only 8 intervals (9 samples) — below the 10-sample calibration floor.
	for i := 0; i < 9; i++ {
		insertActiveSingleSlotSample(t, db, now.Add(-time.Duration(8-i)*time.Minute), 100+float64(i))
	}
	w := do(t, s, authedRequest("GET", "/api/v1/cost/summary?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp costSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Energy.Calibration.Samples != 8 {
		t.Errorf("calibration.samples = %d, want 8", resp.Energy.Calibration.Samples)
	}
	if resp.Energy.Calibration.SingleSlotActiveWallWP50 != nil {
		t.Errorf("p50 should be null below the 10-sample floor, got %v", *resp.Energy.Calibration.SingleSlotActiveWallWP50)
	}
}

func TestCostSummaryCalibrationAtFloorIsPopulated(t *testing.T) {
	s, db, _, _, _ := newCostTestServer(t)
	now := time.Now().UTC()
	// 11 samples -> 10 intervals, at the >=10 floor.
	for i := 0; i < 11; i++ {
		insertActiveSingleSlotSample(t, db, now.Add(-time.Duration(10-i)*time.Minute), 100+float64(i))
	}
	w := do(t, s, authedRequest("GET", "/api/v1/cost/summary?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp costSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Energy.Calibration.Samples != 10 {
		t.Errorf("calibration.samples = %d, want 10", resp.Energy.Calibration.Samples)
	}
	if resp.Energy.Calibration.SingleSlotActiveWallWP50 == nil {
		t.Fatalf("p50 should be populated at the 10-sample floor")
	}
	if *resp.Energy.Calibration.SingleSlotActiveWallWP50 <= 0 {
		t.Errorf("p50 = %v, want > 0", *resp.Energy.Calibration.SingleSlotActiveWallWP50)
	}
}

func TestCostSummaryRateCurrencyFXConversion(t *testing.T) {
	events := bus.New()
	// OverheadW: 0 is indistinguishable from "unset" to config.ResolveCost
	// (it only overrides defaults for values > 0), so this resolves to the
	// default 25W overhead at 1.0 PSU efficiency — accounted for below.
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Cost:   config.Cost{PowerKW: 0.14, RatePerKWh: 30, RateCurrency: "JPY", OverheadW: 0, PSUEfficiency: 1},
	})
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC()
	// 1000W for 60s = 1000/1000 kW * (1/60) h = 1/60 kWh; at 30 JPY/kWh that's
	// 0.5 JPY native, well under the 300s gap threshold so it integrates.
	insertMetricSample(t, db, now.Add(-time.Minute), f64p(1000), intp(1))
	insertMetricSample(t, db, now, f64p(1000), intp(1))

	ffx := &fakeFX{display: "USD", rates: map[[2]string]float64{{"JPY", "USD"}: 0.01}}
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Metrics:   db.Metrics(),
		FX:        ffx,
	})
	t.Cleanup(func() { s.Close() })

	w := do(t, s, authedRequest("GET", "/api/v1/cost/summary?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp costSummaryResponse
	decodeJSON(t, w.Body, &resp)
	if resp.DisplayCurrency != "USD" {
		t.Fatalf("display_currency = %q, want USD", resp.DisplayCurrency)
	}
	// Regression test for the hardcoded-USD conversion bug: rate_currency is
	// JPY, so cost_display must be converted from JPY, not treated as USD.
	// Wall watts = (1000 + 25 default overhead) / 1.0 PSU efficiency = 1025;
	// wallWh over 60s = 1025/60; native cost = wallWh/1000 * 30 JPY/kWh.
	wallWh := 1025.0 / 60.0
	wantNative := wallWh / 1000 * 30.0
	wantDisplay := round6(wantNative * 0.01)
	if round6(resp.Energy.CostDisplay) != wantDisplay {
		t.Errorf("cost_display = %v, want %v (JPY->USD converted)", round6(resp.Energy.CostDisplay), wantDisplay)
	}
	if resp.FxAsOf == nil {
		t.Errorf("fx_as_of = nil, want populated (a conversion was applied)")
	}
	if resp.FxStale {
		t.Errorf("fx_stale = true, want false (a real rate was found)")
	}
}

// ── GET /api/v1/cost/energy-history ────────────────────────────────────────

func TestCostEnergyHistoryBuckets(t *testing.T) {
	s, db, _, _, _ := newCostTestServer(t)
	now := time.Now().UTC().Truncate(time.Hour)
	// Two days of hourly samples at a constant 100W.
	for i := 0; i < 48; i++ {
		insertMetricSample(t, db, now.Add(-time.Duration(47-i)*time.Hour), f64p(100), intp(1))
	}
	w := do(t, s, authedRequest("GET", "/api/v1/cost/energy-history?window=2d&res=86400", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp costEnergyHistoryResponse
	decodeJSON(t, w.Body, &resp)
	if resp.ResolutionS != 86400 {
		t.Errorf("resolution_s = %d, want 86400", resp.ResolutionS)
	}
	if len(resp.Points) == 0 {
		t.Fatalf("expected at least one bucket, got none")
	}
	for _, p := range resp.Points {
		if p.PackageWh < 0 {
			t.Errorf("bucket %d has negative package_wh %v", p.TS, p.PackageWh)
		}
	}
}

func TestCostEnergyHistoryRejectsBadResolution(t *testing.T) {
	s, _, _, _, _ := newCostTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/cost/energy-history?window=2d&res=notanumber", nil))
	if w.Code != 422 {
		t.Fatalf("GET with bad res = %d, want 422: %s", w.Code, w.Body.String())
	}
}
