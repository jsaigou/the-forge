// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }

func TestMetricsRecordAndRange(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	base := ts(1_700_000_000)
	samples := []MetricSample{
		{TS: base, GTTUsedBytes: i64(1000), GTTTotalBytes: i64(96000), GPUUsePct: f64(12.5),
			MemUsedBytes: i64(20000), MemTotalBytes: i64(128000), DiskUsedBytes: i64(500000),
			DiskTotalBytes: i64(2000000), TempCelsius: f64(55.0), InferenceRSSBytes: i64(30000),
			PackagePowerW: f64(74.2), ActiveSlots: iptr(2), EnergyWhTotal: f64(1204.5),
			CPUPct: f64(33.4), NetRxBytesPerSec: f64(1024.0), NetTxBytesPerSec: f64(512.0),
			GPUJunctionTempC: f64(78.0), CPUPackageTempC: f64(62.0), NVMeTempC: f64(41.0)},
		{TS: base.Add(60 * time.Second), GTTUsedBytes: i64(1200), GTTTotalBytes: i64(96000)},
		{TS: base.Add(120 * time.Second)}, // all-nil tick — probe absent
	}
	for _, s := range samples {
		if err := db.Metrics().RecordSample(ctx, s); err != nil {
			t.Fatalf("RecordSample: %v", err)
		}
	}

	got, err := db.Metrics().Range(ctx, base, base.Add(120*time.Second))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Range returned %d rows, want 3", len(got))
	}
	if !got[0].TS.Equal(base) {
		t.Errorf("row 0 ts = %v, want %v", got[0].TS, base)
	}
	if got[0].GTTUsedBytes == nil || *got[0].GTTUsedBytes != 1000 {
		t.Errorf("row 0 gtt_used_bytes = %v, want 1000", got[0].GTTUsedBytes)
	}
	if got[0].GPUUsePct == nil || *got[0].GPUUsePct != 12.5 {
		t.Errorf("row 0 gpu_use_pct = %v, want 12.5", got[0].GPUUsePct)
	}
	if got[0].PackagePowerW == nil || *got[0].PackagePowerW != 74.2 {
		t.Errorf("row 0 package_power_w = %v, want 74.2", got[0].PackagePowerW)
	}
	if got[0].ActiveSlots == nil || *got[0].ActiveSlots != 2 {
		t.Errorf("row 0 active_slots = %v, want 2", got[0].ActiveSlots)
	}
	if got[0].EnergyWhTotal == nil || *got[0].EnergyWhTotal != 1204.5 {
		t.Errorf("row 0 energy_wh_total = %v, want 1204.5", got[0].EnergyWhTotal)
	}
	if got[0].CPUPct == nil || *got[0].CPUPct != 33.4 {
		t.Errorf("row 0 cpu_pct = %v, want 33.4", got[0].CPUPct)
	}
	if got[0].NetRxBytesPerSec == nil || *got[0].NetRxBytesPerSec != 1024.0 {
		t.Errorf("row 0 net_rx_bytes_per_sec = %v, want 1024.0", got[0].NetRxBytesPerSec)
	}
	if got[0].NetTxBytesPerSec == nil || *got[0].NetTxBytesPerSec != 512.0 {
		t.Errorf("row 0 net_tx_bytes_per_sec = %v, want 512.0", got[0].NetTxBytesPerSec)
	}
	if got[0].GPUJunctionTempC == nil || *got[0].GPUJunctionTempC != 78.0 {
		t.Errorf("row 0 gpu_junction_temp_celsius = %v, want 78.0", got[0].GPUJunctionTempC)
	}
	if got[0].CPUPackageTempC == nil || *got[0].CPUPackageTempC != 62.0 {
		t.Errorf("row 0 cpu_package_temp_celsius = %v, want 62.0", got[0].CPUPackageTempC)
	}
	if got[0].NVMeTempC == nil || *got[0].NVMeTempC != 41.0 {
		t.Errorf("row 0 nvme_temp_celsius = %v, want 41.0", got[0].NVMeTempC)
	}
	if got[2].GTTUsedBytes != nil || got[2].TempCelsius != nil {
		t.Errorf("row 2 should be all-nil (probe absent), got %+v", got[2])
	}
	if got[1].PackagePowerW != nil || got[1].ActiveSlots != nil || got[1].EnergyWhTotal != nil {
		t.Errorf("row 1 left the new columns unset — must round-trip as nil, got %+v", got[1])
	}
	if got[1].CPUPct != nil || got[1].NetRxBytesPerSec != nil || got[1].NetTxBytesPerSec != nil ||
		got[1].GPUJunctionTempC != nil || got[1].CPUPackageTempC != nil || got[1].NVMeTempC != nil {
		t.Errorf("row 1 left the Phase 4 columns unset — must round-trip as nil, got %+v", got[1])
	}

	// Re-sampling the same second replaces, not duplicates (ts is the PK).
	if err := db.Metrics().RecordSample(ctx, MetricSample{TS: base, GTTUsedBytes: i64(9999)}); err != nil {
		t.Fatalf("RecordSample replace: %v", err)
	}
	got, err = db.Metrics().Range(ctx, base, base)
	if err != nil {
		t.Fatalf("Range after replace: %v", err)
	}
	if len(got) != 1 || got[0].GTTUsedBytes == nil || *got[0].GTTUsedBytes != 9999 {
		t.Fatalf("replace did not take effect: %+v", got)
	}

	// Range outside the written window returns nothing.
	empty, err := db.Metrics().Range(ctx, base.Add(time.Hour), base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Range empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("Range outside window = %d rows, want 0", len(empty))
	}
}

func TestMetricsPruneRetention(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	old := ts(1_600_000_000)    // well outside a 90-day retention window
	recent := ts(1_700_000_000) // kept
	for _, s := range []MetricSample{{TS: old, GTTUsedBytes: i64(1)}, {TS: recent, GTTUsedBytes: i64(2)}} {
		if err := db.Metrics().RecordSample(ctx, s); err != nil {
			t.Fatalf("RecordSample: %v", err)
		}
	}

	cutoff := recent.Add(-24 * time.Hour) // older than this goes
	n, err := db.Metrics().Prune(ctx, cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune removed %d rows, want 1", n)
	}

	got, err := db.Metrics().Range(ctx, ts(0), ts(2_000_000_000))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 1 || !got[0].TS.Equal(recent) {
		t.Fatalf("Range after prune = %+v, want only %v", got, recent)
	}
}

// fakeClock lets TestRunRetentionUsesInjectedClock advance "now" without
// sleeping — the DoD calls for a fake-clock retention test, not a real
// 90-day wait.
type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

func TestRunRetentionUsesInjectedClock(t *testing.T) {
	db := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := &fakeClock{now: ts(1_700_000_000)}
	old := clock.now.Add(-100 * 24 * time.Hour) // older than 90d retention
	kept := clock.now.Add(-10 * 24 * time.Hour) // inside 90d retention
	for _, s := range []MetricSample{{TS: old, GTTUsedBytes: i64(1)}, {TS: kept, GTTUsedBytes: i64(2)}} {
		if err := db.Metrics().RecordSample(ctx, s); err != nil {
			t.Fatalf("RecordSample: %v", err)
		}
	}

	tick := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		RunRetention(ctx, tick, db.Metrics(), func() int { return 90 }, clock.Now, nil)
		close(done)
	}()

	tick <- clock.now // fire one retention pass
	// Give the goroutine a moment to process the tick before asserting.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := db.Metrics().Range(ctx, ts(0), ts(2_000_000_000))
		if err != nil {
			t.Fatalf("Range: %v", err)
		}
		if len(got) == 1 {
			if !got[0].TS.Equal(kept) {
				t.Fatalf("surviving row = %v, want %v", got[0].TS, kept)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retention tick did not prune within deadline; rows=%d", len(got))
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	<-done
}
