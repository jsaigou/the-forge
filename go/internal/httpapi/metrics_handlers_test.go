// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"
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

// serverWithMetricsStore builds a test Server backed by a real in-memory
// store.DB for the Metrics dependency — history/export exercise real SQL
// downsampling/range-reads, not a hand-rolled fake.
func serverWithMetricsStore(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

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
		Metrics:   db.Metrics(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db
}

// seedMetricSamples bulk-inserts n rows spaced interval apart, ending at
// `to`, directly via SQL (bypassing RecordSample's one-txn-per-row cost —
// store/metrics_test.go already covers RecordSample itself) so a realistic
// multi-thousand-row 7-day window seeds fast.
func seedMetricSamples(t *testing.T, db *store.DB, to time.Time, n int, interval time.Duration) {
	t.Helper()
	tx, err := db.SQL().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO metric_samples
		(ts, gtt_used_bytes, gtt_total_bytes, gpu_use_pct, mem_used_bytes, mem_total_bytes,
		 disk_used_bytes, disk_total_bytes, temp_celsius, inference_rss_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	start := to.Add(-time.Duration(n-1) * interval)
	for i := 0; i < n; i++ {
		ts := start.Add(time.Duration(i) * interval).Unix()
		gttUsed := int64(1000 + i%500)
		if _, err := stmt.Exec(ts, gttUsed, 96000, 20.0+float64(i%10), 20000, 128000,
			500000, 2000000, 55.0, 30000); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestMetricsHandlerPopulatesDisk(t *testing.T) {
	s := newTestServer(t)
	snap := &collector.Snapshot{
		Metrics: collector.Metrics{
			Disk: collector.Disk{TotalBytes: 2_000_000 * 1024 * 1024, FreeBytes: 500_000 * 1024 * 1024, UsedBytes: 1_500_000 * 1024 * 1024, Pct: 75.0},
		},
	}
	s.deps.Snapshots.(*collector.Static).Set(snap)

	w := do(t, s, authedRequest("GET", "/api/v1/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Disk struct {
			TotalBytes int64   `json:"total_bytes"`
			FreeBytes  int64   `json:"free_bytes"`
			UsedBytes  int64   `json:"used_bytes"`
			Pct        float64 `json:"pct"`
		} `json:"disk"`
	}
	decodeJSON(t, w.Body, &got)
	if got.Disk.TotalBytes != 2_000_000*1024*1024 || got.Disk.FreeBytes != 500_000*1024*1024 ||
		got.Disk.UsedBytes != 1_500_000*1024*1024 || got.Disk.Pct != 75.0 {
		t.Errorf("disk = %+v, want the seeded snapshot values", got.Disk)
	}
}

// TestMetricsHandlerPopulatesPhase4Fields checks GET /api/v1/metrics wires
// the new collector fields (Phase 4, 2026-08-12) through to the wire shape
// — network rates, per-mount storage, and the three additional temp
// channels — not just the pre-existing Disk field covered above.
func TestMetricsHandlerPopulatesPhase4Fields(t *testing.T) {
	s := newTestServer(t)
	rx, tx := 1234.5, 678.9
	junction, cpuTemp, nvme := 82.0, 61.0, 44.0
	snap := &collector.Snapshot{
		Metrics: collector.Metrics{
			NetRxBytesPerSec: &rx,
			NetTxBytesPerSec: &tx,
			GPUJunctionTempC: &junction,
			CPUPackageTempC:  &cpuTemp,
			NVMeTempC:        &nvme,
			Storage: []collector.StorageMount{
				{Name: "root", Path: "/", Disk: collector.Disk{TotalBytes: 100, FreeBytes: 40, UsedBytes: 60, Pct: 60}},
				{Name: "models", Path: "/opt/models", Disk: collector.Disk{TotalBytes: 200, FreeBytes: 50, UsedBytes: 150, Pct: 75}},
			},
		},
	}
	s.deps.Snapshots.(*collector.Static).Set(snap)

	w := do(t, s, authedRequest("GET", "/api/v1/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		NetRxBytesPerSec float64 `json:"net_rx_bytes_per_sec"`
		NetTxBytesPerSec float64 `json:"net_tx_bytes_per_sec"`
		GPUJunctionTempC float64 `json:"gpu_junction_temp_celsius"`
		CPUPackageTempC  float64 `json:"cpu_package_temp_celsius"`
		NVMeTempC        float64 `json:"nvme_temp_celsius"`
		Storage          []struct {
			Name string  `json:"name"`
			Path string  `json:"path"`
			Pct  float64 `json:"pct"`
		} `json:"storage"`
	}
	decodeJSON(t, w.Body, &got)
	if got.NetRxBytesPerSec != rx || got.NetTxBytesPerSec != tx {
		t.Errorf("net rates = (%v, %v), want (%v, %v)", got.NetRxBytesPerSec, got.NetTxBytesPerSec, rx, tx)
	}
	if got.GPUJunctionTempC != junction || got.CPUPackageTempC != cpuTemp || got.NVMeTempC != nvme {
		t.Errorf("temps = (%v, %v, %v), want (%v, %v, %v)",
			got.GPUJunctionTempC, got.CPUPackageTempC, got.NVMeTempC, junction, cpuTemp, nvme)
	}
	if len(got.Storage) != 2 || got.Storage[0].Name != "root" || got.Storage[1].Name != "models" {
		t.Errorf("storage = %+v, want root+models mounts", got.Storage)
	}
}

func TestMetricsHistoryDownsamples7Day(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	// One sample per minute for just over 7 days — 10,081 raw rows, the
	// realistic volume at the default 60s sample interval.
	seedMetricSamples(t, db, now, 7*24*60+1, time.Minute)

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/history?window=7d&series=gtt,gpu,disk,mem&res=auto", nil))
	if w.Code != 200 {
		t.Fatalf("GET history = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Window      string `json:"window"`
		ResolutionS int    `json:"resolution_s"`
		Points      []struct {
			TS           int64    `json:"ts"`
			GTTUsedBytes *int64   `json:"gtt_used_bytes"`
			GPUUsePct    *float64 `json:"gpu_use_pct"`
		} `json:"points"`
	}
	decodeJSON(t, w.Body, &got)

	if got.Window != "7d" {
		t.Errorf("window = %q, want 7d", got.Window)
	}
	if len(got.Points) < 50 || len(got.Points) > 2000 {
		t.Fatalf("7d window returned %d points, want hundreds (not tens of thousands, not near-zero)", len(got.Points))
	}
	if len(got.Points) >= 10081 {
		t.Fatalf("history returned %d points — raw row count, not downsampled", len(got.Points))
	}
	for _, p := range got.Points {
		if p.GTTUsedBytes == nil {
			t.Fatalf("point at ts=%d missing gtt_used_bytes despite series=gtt", p.TS)
		}
	}
	t.Logf("7d window: %d raw rows -> %d points (resolution_s=%d)", 7*24*60+1, len(got.Points), got.ResolutionS)
}

func TestMetricsHistorySeriesFilter(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	seedMetricSamples(t, db, now, 5, time.Minute)

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/history?window=1h&series=disk", nil))
	if w.Code != 200 {
		t.Fatalf("GET history = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Points []map[string]json.RawMessage `json:"points"`
	}
	decodeJSON(t, w.Body, &got)
	if len(got.Points) == 0 {
		t.Fatal("expected at least one point")
	}
	for _, p := range got.Points {
		if _, ok := p["gtt_used_bytes"]; ok {
			t.Errorf("series=disk point unexpectedly carries gtt_used_bytes: %v", p)
		}
		if _, ok := p["disk_used_bytes"]; !ok {
			t.Errorf("series=disk point missing disk_used_bytes: %v", p)
		}
	}
}

func TestMetricsHistoryValidation(t *testing.T) {
	s := newTestServer(t)
	cases := []string{
		"/api/v1/metrics/history?window=bogus",
		"/api/v1/metrics/history?window=7d&series=nonsense",
		"/api/v1/metrics/history?window=7d&res=notanumber",
	}
	for _, path := range cases {
		w := do(t, s, authedRequest("GET", path, nil))
		if w.Code != 422 {
			t.Errorf("%s = %d, want 422", path, w.Code)
		}
	}
}

func TestMetricsHistoryNilStoreReturnsEmpty(t *testing.T) {
	s := newTestServer(t) // Deps.Metrics unset
	w := do(t, s, authedRequest("GET", "/api/v1/metrics/history?window=7d", nil))
	if w.Code != 200 {
		t.Fatalf("code = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Points []any `json:"points"`
	}
	decodeJSON(t, w.Body, &got)
	if got.Points == nil {
		t.Error("points should be [] (not null) when Metrics is unwired")
	}
	if len(got.Points) != 0 {
		t.Errorf("points = %v, want empty", got.Points)
	}
}

func TestMetricsExportJSON(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	seedMetricSamples(t, db, now, 10, time.Minute)

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/export?format=json&window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("export json = %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var got struct {
		Window string `json:"window"`
		Rows   []struct {
			TS           int64  `json:"ts"`
			GTTUsedBytes *int64 `json:"gtt_used_bytes"`
		} `json:"rows"`
	}
	decodeJSON(t, w.Body, &got)
	if len(got.Rows) != 10 {
		t.Fatalf("rows = %d, want 10 (full resolution, no downsampling)", len(got.Rows))
	}
}

func TestMetricsExportCSV(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	seedMetricSamples(t, db, now, 5, time.Minute)

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/export?format=csv&window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("export csv = %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content-type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("content-disposition = %q, want attachment", cd)
	}
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 6 { // header + 5 samples
		t.Fatalf("csv rows = %d, want 6 (1 header + 5 data)", len(rows))
	}
	wantHeader := []string{"ts", "gtt_used_bytes", "gtt_total_bytes", "gpu_use_pct", "mem_used_bytes",
		"mem_total_bytes", "disk_used_bytes", "disk_total_bytes", "temp_celsius", "inference_rss_bytes",
		"package_power_w", "active_slots", "energy_wh_total"}
	for i, col := range wantHeader {
		if rows[0][i] != col {
			t.Errorf("header[%d] = %q, want %q", i, rows[0][i], col)
		}
	}
	// Data rows have no power/active_slots/energy data seeded — the new
	// trailing columns must be present but empty, not absent or "0".
	for r := 1; r < len(rows); r++ {
		for _, col := range []int{10, 11, 12} {
			if rows[r][col] != "" {
				t.Errorf("row %d col %d = %q, want empty (no data seeded)", r, col, rows[r][col])
			}
		}
	}
}

func TestMetricsExportCSVPopulatesPowerColumns(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	if err := db.Metrics().RecordSample(context.Background(), store.MetricSample{
		TS: now, PackagePowerW: f64p(42.5), ActiveSlots: intp(2), EnergyWhTotal: f64p(1234.5),
	}); err != nil {
		t.Fatalf("RecordSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/export?format=csv&window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("export csv = %d, body=%s", w.Code, w.Body.String())
	}
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("csv rows = %d, want 2 (1 header + 1 data)", len(rows))
	}
	if rows[1][10] != "42.5" {
		t.Errorf("package_power_w col = %q, want 42.5", rows[1][10])
	}
	if rows[1][11] != "2" {
		t.Errorf("active_slots col = %q, want 2", rows[1][11])
	}
	if rows[1][12] != "1234.5" {
		t.Errorf("energy_wh_total col = %q, want 1234.5", rows[1][12])
	}
}

func TestMetricsExportJSONPopulatesPowerFields(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	if err := db.Metrics().RecordSample(context.Background(), store.MetricSample{
		TS: now, PackagePowerW: f64p(42.5), ActiveSlots: intp(2), EnergyWhTotal: f64p(1234.5),
	}); err != nil {
		t.Fatalf("RecordSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/export?format=json&window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("export json = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Rows []struct {
			PackagePowerW *float64 `json:"package_power_w"`
			ActiveSlots   *int     `json:"active_slots"`
			EnergyWhTotal *float64 `json:"energy_wh_total"`
		} `json:"rows"`
	}
	decodeJSON(t, w.Body, &got)
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(got.Rows))
	}
	r := got.Rows[0]
	if r.PackagePowerW == nil || *r.PackagePowerW != 42.5 {
		t.Errorf("package_power_w = %v, want 42.5", r.PackagePowerW)
	}
	if r.ActiveSlots == nil || *r.ActiveSlots != 2 {
		t.Errorf("active_slots = %v, want 2", r.ActiveSlots)
	}
	if r.EnergyWhTotal == nil || *r.EnergyWhTotal != 1234.5 {
		t.Errorf("energy_wh_total = %v, want 1234.5", r.EnergyWhTotal)
	}
}

func TestMetricsHistoryPowerSeries(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	if err := db.Metrics().RecordSample(context.Background(), store.MetricSample{
		TS: now, PackagePowerW: f64p(55),
	}); err != nil {
		t.Fatalf("RecordSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/history?window=1h&series=power", nil))
	if w.Code != 200 {
		t.Fatalf("GET history = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Points []map[string]json.RawMessage `json:"points"`
	}
	decodeJSON(t, w.Body, &got)
	if len(got.Points) == 0 {
		t.Fatal("expected at least one point")
	}
	found := false
	for _, p := range got.Points {
		if _, ok := p["package_power_w"]; ok {
			found = true
		}
	}
	if !found {
		t.Error("series=power requested but no point carries package_power_w")
	}
}

func TestMetricsHistoryDefaultSeriesExcludesPower(t *testing.T) {
	// Load-bearing: "power" must NOT join the default series set (empty
	// ?series=) — that would change every existing /metrics/history caller's
	// response shape under the shape freeze.
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	if err := db.Metrics().RecordSample(context.Background(), store.MetricSample{
		TS: now, PackagePowerW: f64p(55),
	}); err != nil {
		t.Fatalf("RecordSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/history?window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("GET history = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Points []map[string]json.RawMessage `json:"points"`
	}
	decodeJSON(t, w.Body, &got)
	for _, p := range got.Points {
		if _, ok := p["package_power_w"]; ok {
			t.Error("default series must not include package_power_w")
		}
	}
}

func TestMetricsHistoryPowerSeriesAllNilOmitsKey(t *testing.T) {
	// series=power requested but every sample's PackagePowerW is nil -> no
	// point should carry a package_power_w key at all (never a fabricated 0).
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	seedMetricSamples(t, db, now, 5, time.Minute) // no power data

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/history?window=1h&series=power", nil))
	if w.Code != 200 {
		t.Fatalf("GET history = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Points []map[string]json.RawMessage `json:"points"`
	}
	decodeJSON(t, w.Body, &got)
	for _, p := range got.Points {
		if _, ok := p["package_power_w"]; ok {
			t.Errorf("point unexpectedly carries package_power_w with no underlying data: %v", p)
		}
	}
}

// TestMetricsHistoryCPUAndNetworkSeries is the Phase 4 (2026-08-12)
// counterpart to TestMetricsHistoryPowerSeries — same additive,
// not-in-the-default-set pattern for the two new series tokens.
func TestMetricsHistoryCPUAndNetworkSeries(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	if err := db.Metrics().RecordSample(context.Background(), store.MetricSample{
		TS: now, CPUPct: f64p(42), NetRxBytesPerSec: f64p(1000), NetTxBytesPerSec: f64p(500),
	}); err != nil {
		t.Fatalf("RecordSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/history?window=1h&series=cpu,network", nil))
	if w.Code != 200 {
		t.Fatalf("GET history = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Points []map[string]json.RawMessage `json:"points"`
	}
	decodeJSON(t, w.Body, &got)
	if len(got.Points) == 0 {
		t.Fatal("expected at least one point")
	}
	var sawCPU, sawRx, sawTx bool
	for _, p := range got.Points {
		if _, ok := p["cpu_pct"]; ok {
			sawCPU = true
		}
		if _, ok := p["net_rx_bytes_per_sec"]; ok {
			sawRx = true
		}
		if _, ok := p["net_tx_bytes_per_sec"]; ok {
			sawTx = true
		}
	}
	if !sawCPU || !sawRx || !sawTx {
		t.Errorf("series=cpu,network requested but a point is missing a field: cpu=%v rx=%v tx=%v", sawCPU, sawRx, sawTx)
	}
}

func TestMetricsHistoryDefaultSeriesExcludesCPUAndNetwork(t *testing.T) {
	s, db := serverWithMetricsStore(t)
	now := time.Now().UTC()
	if err := db.Metrics().RecordSample(context.Background(), store.MetricSample{
		TS: now, CPUPct: f64p(42), NetRxBytesPerSec: f64p(1000),
	}); err != nil {
		t.Fatalf("RecordSample: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/metrics/history?window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("GET history = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Points []map[string]json.RawMessage `json:"points"`
	}
	decodeJSON(t, w.Body, &got)
	for _, p := range got.Points {
		if _, ok := p["cpu_pct"]; ok {
			t.Error("default series must not include cpu_pct")
		}
		if _, ok := p["net_rx_bytes_per_sec"]; ok {
			t.Error("default series must not include net_rx_bytes_per_sec")
		}
	}
}

func TestMetricsExportValidation(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/metrics/export?format=xml", nil))
	if w.Code != 422 {
		t.Errorf("format=xml = %d, want 422", w.Code)
	}
}

// TestMetricsSamplingRetentionPruneEndToEnd drives the RunSampler/RunRetention
// tickers directly (store package, injected ticker channel — no real sleep)
// rather than the httpapi auto-started goroutine, matching the DoD's "fake
// clock/injected time" guidance for the 90-day retention test. Confirms the
// two store-level primitives BE-1 owns compose correctly end to end.
func TestMetricsSamplingRetentionPruneEndToEnd(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	fakeNow := time.Unix(1_700_000_000, 0).UTC()
	n := 0
	sampleTick := make(chan time.Time, 1)
	retentionTick := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sampleDone := make(chan struct{})
	go func() {
		store.RunSampler(ctx, sampleTick, db.Metrics(), func(context.Context) store.MetricSample {
			n++
			offset := -200 * 24 * time.Hour // first tick: far outside 90d retention
			if n > 1 {
				offset = 0 // second tick: "now" — survives the prune
			}
			v := int64(n)
			return store.MetricSample{TS: fakeNow.Add(offset), GTTUsedBytes: &v}
		}, nil)
		close(sampleDone)
	}()
	// Two ticks: one sample ~199 days old (pruned), one "now" (kept).
	sampleTick <- fakeNow
	sampleTick <- fakeNow
	waitForRowCount(t, db, 2)

	retentionDone := make(chan struct{})
	go func() {
		store.RunRetention(ctx, retentionTick, db.Metrics(), func() int { return 90 }, func() time.Time { return fakeNow }, nil)
		close(retentionDone)
	}()
	retentionTick <- fakeNow
	waitForRowCount(t, db, 1)

	cancel()
	<-sampleDone
	<-retentionDone
}

func waitForRowCount(t *testing.T, db *store.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := db.Metrics().Range(context.Background(), time.Unix(0, 0), time.Now().Add(24*365*time.Hour))
		if err != nil {
			t.Fatalf("Range: %v", err)
		}
		if len(rows) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("row count = %d, want %d (timed out waiting)", len(rows), want)
		}
		time.Sleep(time.Millisecond)
	}
}
