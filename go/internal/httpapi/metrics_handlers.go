// SPDX-License-Identifier: Apache-2.0

package httpapi

// metrics_handlers.go — live metrics snapshot + (Sprint 0) history/export.
// Split out of handlers.go by Sprint 0 (docs/v5-sprint0-contract-freeze.md
// §0.1); pure move, no behavior change. Owner track after split: BE-1.
//
// §0.4 implementation (BE-1): disk sampling lives in internal/collector
// (sampleDisk, wired into buildMetrics); the metric_samples time-series
// lives in internal/store (Metrics interface + RunSampler/RunRetention
// tickers). This file wires the two together for the live daemon
// (startMetricsSampling, called once from Handler()) and serves the read
// paths (history downsample, full-resolution export).

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// handleMetrics returns the latest metrics snapshot (Contract 1 §2 #4).
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	snap := s.snapshot()
	resp := metricsResponse{}
	if snap != nil {
		resp.Mode = snap.Metrics.Mode
		resp.Memory = metricsMemory{
			TotalBytes: snap.Metrics.Memory.TotalBytes,
			UsedBytes:  snap.Metrics.Memory.UsedBytes,
			AvailBytes: snap.Metrics.Memory.AvailBytes,
			Pct:        snap.Metrics.Memory.Pct,
		}
		resp.CPU = metricsCPU{
			Load1: snap.Metrics.CPU.Load1,
			Pct:   snap.Metrics.CPU.Pct,
		}
		// Sprint 0 §0.4: Disk is the models/data volume sample, populated by
		// the collector (internal/collector.sampleDisk). Zero value when the
		// probe failed or Paths.ModelsDir is unset — matches the frozen
		// metricsDisk contract (zero, not null).
		resp.Disk = metricsDisk{
			TotalBytes: snap.Metrics.Disk.TotalBytes,
			FreeBytes:  snap.Metrics.Disk.FreeBytes,
			UsedBytes:  snap.Metrics.Disk.UsedBytes,
			Pct:        snap.Metrics.Disk.Pct,
		}
		resp.GPUUsePct = snap.Metrics.GPUUsePct
		resp.GTTUsedBytes = snap.Metrics.GTTUsedBytes
		resp.GTTTotalBytes = snap.Metrics.GTTTotalBytes
		resp.TempCelsius = snap.Metrics.TempCelsius
		resp.UptimeSeconds = snap.Metrics.UptimeSeconds
		resp.InferenceRSSBytes = snap.Metrics.InferenceRSSBytes
		resp.ModelWeightsBytes = snap.Metrics.ModelWeightsBytes
		resp.KVCacheBytes = snap.Metrics.KVCacheBytes
		resp.PackagePowerW = snap.Metrics.PackagePowerW
		resp.NetRxBytesPerSec = snap.Metrics.NetRxBytesPerSec
		resp.NetTxBytesPerSec = snap.Metrics.NetTxBytesPerSec
		resp.GPUJunctionTempC = snap.Metrics.GPUJunctionTempC
		resp.CPUPackageTempC = snap.Metrics.CPUPackageTempC
		resp.NVMeTempC = snap.Metrics.NVMeTempC
		resp.Storage = make([]metricsStorageMount, 0, len(snap.Metrics.Storage))
		for _, m := range snap.Metrics.Storage {
			resp.Storage = append(resp.Storage, metricsStorageMount{
				Name: m.Name, Path: m.Path,
				TotalBytes: m.Disk.TotalBytes, FreeBytes: m.Disk.FreeBytes,
				UsedBytes: m.Disk.UsedBytes, Pct: m.Disk.Pct,
			})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── §0.4 settings-backed defaults ───────────────────────────────────────────

const (
	defaultSampleIntervalS = 60
	defaultRetentionDays   = 90
	// historyTargetPoints bounds "auto" resolution so a 7-day window returns
	// hundreds, not tens of thousands, of points (§0.4 requirement).
	historyTargetPoints = 300
)

// metricsSampleIntervalS reads metrics.sample_interval_s (Sprint 0 §0.12
// registered key, settings_handlers.go), defaulting to 60s when unset or the
// Settings dependency isn't wired (Phase 4 stub environment).
func (s *Server) metricsSampleIntervalS() int {
	return s.readIntSetting(SettingMetricsSampleInterval, defaultSampleIntervalS)
}

// metricsRetentionDaysSetting reads metrics.retention_days, defaulting to 90.
func (s *Server) metricsRetentionDaysSetting() int {
	return s.readIntSetting(SettingMetricsRetentionDays, defaultRetentionDays)
}

func (s *Server) readIntSetting(key string, def int) int {
	if s.deps.Settings == nil {
		return def
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := s.deps.Settings.Get(ctx, key)
	if err != nil {
		return def
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n <= 0 {
		return def
	}
	return n
}

// ── §0.4 background sampler ─────────────────────────────────────────────────

// startMetricsSampling starts the metric_samples background sampler + daily
// retention prune (store.RunSampler / store.RunRetention), once per Server.
// No-op when either the Metrics or Snapshots dependency isn't wired (Phase 4
// stub environment, and most unit tests). Called from Handler() alongside
// startHeartbeat(); both use s.bgCtx so they stop together on Close().
func (s *Server) startMetricsSampling() {
	if s.deps.Metrics == nil || s.deps.Snapshots == nil {
		return
	}
	s.metricsSamplerOnce.Do(func() {
		interval := time.Duration(s.metricsSampleIntervalS()) * time.Second
		if interval <= 0 {
			interval = defaultSampleIntervalS * time.Second
		}
		sampleTicker := time.NewTicker(interval)
		retentionTicker := time.NewTicker(24 * time.Hour)
		goSafe("metrics_ticker", func() {
			<-s.bgCtx.Done()
			sampleTicker.Stop()
			retentionTicker.Stop()
		})
		onErr := func(err error) { log.Printf("httpapi: metrics sampler: %v", err) }
		go store.RunSampler(s.bgCtx, sampleTicker.C, s.deps.Metrics, s.sampleMetricsNow, onErr)
		go store.RunRetention(s.bgCtx, retentionTicker.C, s.deps.Metrics, s.metricsRetentionDaysSetting, time.Now, onErr)
	})
}

// slotStateSyncInterval is how often the slot_state crash-recovery table
// is corrected against live reality (store.RunSlotStateSync). 5 minutes:
// frequent enough that a slot killed outside a tracked Load/Switch/Unload
// (a crash, an OOM, or the killLingering collateral-kill bug fixed
// 2026-07-29) doesn't sit stale for long, infrequent enough to be a
// non-event on a SQLite write budget already dominated by the collector
// poll.
const slotStateSyncInterval = 5 * time.Minute

// startSlotStateSync starts the slot_state background corrector
// (store.RunSlotStateSync), once per Server. No-op when either the
// SlotStateStore or Snapshots dependency isn't wired (Phase 4 stub
// environment, and most unit tests). Called from Handler() alongside
// startMetricsSampling(); both use s.bgCtx so they stop together on
// Close().
func (s *Server) startSlotStateSync() {
	if s.deps.SlotStateStore == nil || s.deps.Snapshots == nil {
		return
	}
	s.slotStateSyncOnce.Do(func() {
		ticker := time.NewTicker(slotStateSyncInterval)
		goSafe("slot_state_ticker", func() {
			<-s.bgCtx.Done()
			ticker.Stop()
		})
		go store.RunSlotStateSync(s.bgCtx, ticker.C, s.deps.SlotStateStore, s.liveSlotModes, nil)
	})
}

// liveSlotModes reads the latest collector snapshot's reconciled
// slot->mode map — the same live-accurate source the dashboard reads, and
// the seam RunSlotStateSync calls each tick (store cannot import
// collector, so this glue lives here rather than in internal/store).
func (s *Server) liveSlotModes() map[string]string {
	snap := s.snapshot()
	if snap == nil {
		return nil
	}
	out := make(map[string]string, len(snap.Slots))
	for name, st := range snap.Slots {
		out[name] = st.Mode
	}
	return out
}

// startCompressorSampling starts the compressor_samples background sampler
// (Sprint 4, resource bounding + monitoring), once per Server. No-op when
// Compressors, Compressor, or Snapshots isn't wired. Deliberately reuses
// defaultSampleIntervalS (60s) rather than the 2s collector cycle: this
// writes one row per proxy per tick (not one row total like metric_samples),
// and leak/restart-churn detection doesn't need sub-minute resolution — at
// 2s x 3 proxies this would be ~65k rows/day instead of ~4k. Retention is
// covered by the same daily prune pattern as store.RunRetention, inlined
// here since Compressors' Prune signature matches Metrics' exactly but the
// per-proxy RecordSample loop below doesn't fit RunSampler's one-sample-
// per-tick shape.
func (s *Server) startCompressorSampling() {
	if s.deps.Compressors == nil || s.deps.Routing == nil || s.deps.Snapshots == nil {
		return
	}
	s.compressorSamplerOnce.Do(func() {
		sampleTicker := time.NewTicker(defaultSampleIntervalS * time.Second)
		retentionTicker := time.NewTicker(24 * time.Hour)
		goSafe("compressor_sampler_ticker", func() {
			<-s.bgCtx.Done()
			sampleTicker.Stop()
			retentionTicker.Stop()
		})
		go func() {
			for {
				select {
				case <-s.bgCtx.Done():
					return
				case <-sampleTicker.C:
					s.sampleCompressorsNow(s.bgCtx)
				case <-retentionTicker.C:
					days := s.metricsRetentionDaysSetting()
					if days <= 0 {
						continue
					}
					cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
					if _, err := s.deps.Compressors.Prune(s.bgCtx, cutoff); err != nil {
						log.Printf("httpapi: compressor sampler: prune: %v", err)
					}
				}
			}
		}()
	})
}

// sampleCompressorsNow records one compressor_samples row per proxy from
// the latest collector snapshot's Compressors map (systemd + /proc,
// unconditional every tick) — unlike the Compressor traffic-counter scrape,
// which skips a proxy outright on a failed /metrics scrape or an idle
// interval, exactly the moments process health matters most. A missed
// tick or a single proxy's write failure is logged and never stops the
// loop.
func (s *Server) sampleCompressorsNow(ctx context.Context) {
	snap := s.snapshot()
	if snap == nil || len(snap.Compressors) == 0 {
		return
	}
	proxies, err := s.deps.Routing.Proxies(ctx)
	if err != nil {
		log.Printf("httpapi: compressor sampler: proxies read: %v", err)
		return
	}
	idByService := make(map[string]int64, len(proxies))
	for _, p := range proxies {
		idByService[p.Service] = p.ID
	}
	now := time.Now()
	for service, cs := range snap.Compressors {
		proxyID, ok := idByService[service]
		if !ok {
			continue // torn-down/renamed proxy — same hygiene recordCompressorSavings applies
		}
		row := store.CompressorSampleRow{
			TS: now, ProxyID: proxyID,
			Up: cs.Up, MainPID: cs.MainPID, RSSBytes: cs.RSSBytes, NRestarts: cs.NRestarts,
		}
		if err := s.deps.Compressors.RecordSample(ctx, row); err != nil {
			log.Printf("httpapi: compressor sampler: record %s: %v", service, err)
		}
	}
}

// sampleMetricsNow builds one MetricSample from the latest collector
// snapshot — the seam RunSampler calls each tick (store cannot import
// collector, so this glue lives here rather than in internal/store).
func (s *Server) sampleMetricsNow(_ context.Context) store.MetricSample {
	sample := store.MetricSample{TS: time.Now().UTC()}
	snap := s.snapshot()
	if snap == nil {
		return sample
	}
	sample.GTTUsedBytes = snap.Metrics.GTTUsedBytes
	sample.GTTTotalBytes = snap.Metrics.GTTTotalBytes
	sample.GPUUsePct = snap.Metrics.GPUUsePct
	sample.TempCelsius = snap.Metrics.TempCelsius
	sample.InferenceRSSBytes = snap.Metrics.InferenceRSSBytes
	sample.PackagePowerW = snap.Metrics.PackagePowerW
	cpuPct := snap.Metrics.CPU.Pct
	sample.CPUPct = &cpuPct
	sample.NetRxBytesPerSec = snap.Metrics.NetRxBytesPerSec
	sample.NetTxBytesPerSec = snap.Metrics.NetTxBytesPerSec
	sample.GPUJunctionTempC = snap.Metrics.GPUJunctionTempC
	sample.CPUPackageTempC = snap.Metrics.CPUPackageTempC
	sample.NVMeTempC = snap.Metrics.NVMeTempC
	activeSlots := 0
	for _, st := range snap.Slots {
		if st.Mode != "" {
			activeSlots++
		}
	}
	sample.ActiveSlots = &activeSlots
	if snap.Metrics.Memory.TotalBytes > 0 {
		used, total := snap.Metrics.Memory.UsedBytes, snap.Metrics.Memory.TotalBytes
		sample.MemUsedBytes, sample.MemTotalBytes = &used, &total
	}
	if snap.Metrics.Disk.TotalBytes > 0 {
		used, total := snap.Metrics.Disk.UsedBytes, snap.Metrics.Disk.TotalBytes
		sample.DiskUsedBytes, sample.DiskTotalBytes = &used, &total
	}
	return sample
}

// ── §0.4 history ─────────────────────────────────────────────────────────────

// validHistorySeries is the series token vocabulary (?series=gtt,gpu,disk,mem
// — docs/v5-sprint0-contract-freeze.md §0.4). "power" added additively
// (cost/savings sprint 2026-07-30) — see parseHistorySeries's doc comment
// for why widening this accepted set doesn't break the response freeze.
var validHistorySeries = map[string]bool{
	"gtt": true, "gpu": true, "mem": true, "disk": true, "power": true,
	"cpu": true, "network": true,
}

// handleMetricsHistory — GET /api/v1/metrics/history?window=7d&series=…&res=auto
// (Sprint 0 §0.4). Downsamples the metric_samples table (avg per bucket) so
// a 7-day window returns ~hundreds of points, matching the frozen
// metricsHistoryResponse shape.
func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	windowRaw := q.Get("window")
	if windowRaw == "" {
		windowRaw = "7d"
	}
	window, err := parseMetricsWindow(windowRaw)
	if err != nil {
		writeValidationError(w, map[string]string{"window": err.Error()})
		return
	}

	series, err := parseHistorySeries(q.Get("series"))
	if err != nil {
		writeValidationError(w, map[string]string{"series": err.Error()})
		return
	}

	resolutionS, err := resolveHistoryResolution(q.Get("res"), window, s.metricsSampleIntervalS())
	if err != nil {
		writeValidationError(w, map[string]string{"res": err.Error()})
		return
	}

	resp := metricsHistoryResponse{Window: windowRaw, ResolutionS: resolutionS, Points: []metricsHistoryPoint{}}
	if s.deps.Metrics != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		to := time.Now().UTC()
		from := to.Add(-window)
		samples, err := s.deps.Metrics.Range(ctx, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metrics history read failed")
			return
		}
		resp.Points = downsampleMetrics(samples, from, resolutionS, series, s.wallWatts)
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseMetricsWindow parses "<n>d" / "<n>h" / "<n>m" / "<n>s" (the query
// forms used throughout §0.4's examples — window=7d, window=90d).
func parseMetricsWindow(raw string) (time.Duration, error) {
	if len(raw) < 2 {
		return 0, invalidWindowErr(raw)
	}
	unit := raw[len(raw)-1]
	n, err := strconv.ParseFloat(raw[:len(raw)-1], 64)
	if err != nil || n <= 0 {
		return 0, invalidWindowErr(raw)
	}
	switch unit {
	case 'd':
		return time.Duration(n * float64(24*time.Hour)), nil
	case 'h':
		return time.Duration(n * float64(time.Hour)), nil
	case 'm':
		return time.Duration(n * float64(time.Minute)), nil
	case 's':
		return time.Duration(n * float64(time.Second)), nil
	default:
		return 0, invalidWindowErr(raw)
	}
}

func invalidWindowErr(raw string) error {
	return &validationErr{"window " + strconv.Quote(raw) + " must look like 7d, 24h, 30m, or 90d"}
}

// validationErr is a tiny local error type for field-level 422 messages
// (this file doesn't otherwise need Pydantic-style validators.go plumbing).
type validationErr struct{ msg string }

func (e *validationErr) Error() string { return e.msg }

// parseHistorySeries parses the comma-separated ?series= token list against
// the vocabulary, defaulting to the original four when omitted.
//
// "power" is intentionally excluded from the default set (empty ?series=)
// even though it's in validHistorySeries — that's what keeps widening the
// accepted series vocabulary additive under the Sprint 0 §0.4 freeze: no
// existing client sends series=power, so every existing request (including
// the omitted-series default) yields a byte-identical response. Only a
// request that explicitly asks for it gets the new fields.
func parseHistorySeries(raw string) (map[string]bool, error) {
	if raw == "" {
		return map[string]bool{"gtt": true, "gpu": true, "mem": true, "disk": true}, nil
	}
	out := map[string]bool{}
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if !validHistorySeries[tok] {
			return nil, &validationErr{"unknown series " + strconv.Quote(tok) + " (want gtt, gpu, mem, disk, power, cpu, network)"}
		}
		out[tok] = true
	}
	if len(out) == 0 {
		return nil, &validationErr{"series must not be empty"}
	}
	return out, nil
}

// resolveHistoryResolution implements res=auto (bucket so the window returns
// ~historyTargetPoints points, never finer than the sample interval) or an
// explicit positive integer number of seconds.
func resolveHistoryResolution(raw string, window time.Duration, sampleIntervalS int) (int, error) {
	if sampleIntervalS <= 0 {
		sampleIntervalS = defaultSampleIntervalS
	}
	if raw == "" || raw == "auto" {
		res := int(window.Seconds()) / historyTargetPoints
		if res < sampleIntervalS {
			res = sampleIntervalS
		}
		return res, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, &validationErr{"res " + strconv.Quote(raw) + " must be \"auto\" or a positive integer number of seconds"}
	}
	return n, nil
}

// avgAcc accumulates a running average over nullable samples within one
// downsample bucket; a bucket with zero contributing samples for a field
// stays nil in the output point (never a false zero).
type avgAcc struct {
	sum float64
	n   int
}

func (a *avgAcc) addInt(v *int64) {
	if v == nil {
		return
	}
	a.sum += float64(*v)
	a.n++
}

func (a *avgAcc) addFloat(v *float64) {
	if v == nil {
		return
	}
	a.sum += *v
	a.n++
}

func (a avgAcc) intPtr() *int64 {
	if a.n == 0 {
		return nil
	}
	v := int64(a.sum/float64(a.n) + 0.5)
	return &v
}

func (a avgAcc) floatPtr() *float64 {
	if a.n == 0 {
		return nil
	}
	v := a.sum / float64(a.n)
	return &v
}

// downsampleMetrics buckets raw samples into resolutionS-wide, from-aligned
// windows and averages each requested series within a bucket. samples must
// already be ordered oldest-first (store.Metrics.Range's contract).
//
// wallWatts converts a bucket-averaged package watts figure to the
// calibrated wall-power estimate (config.Cost.WallWatts) for the "power"
// series' wall_power_w_est field; nil when the series isn't requested or no
// Config dependency is wired (then only package_power_w is populated).
func downsampleMetrics(samples []store.MetricSample, from time.Time, resolutionS int, series map[string]bool, wallWatts func(float64) float64) []metricsHistoryPoint {
	if resolutionS <= 0 {
		resolutionS = defaultSampleIntervalS
	}
	fromUnix := from.Unix()
	res64 := int64(resolutionS)

	type bucket struct {
		ts                  int64
		gttUsed, gttTotal   avgAcc
		gpu                 avgAcc
		memUsed, memTotal   avgAcc
		diskUsed, diskTotal avgAcc
		power               avgAcc
		cpu                 avgAcc
		netRx, netTx        avgAcc
	}
	flush := func(points []metricsHistoryPoint, b *bucket) []metricsHistoryPoint {
		if b == nil {
			return points
		}
		p := metricsHistoryPoint{TS: b.ts}
		if series["gtt"] {
			p.GTTUsedBytes = b.gttUsed.intPtr()
			p.GTTTotalBytes = b.gttTotal.intPtr()
		}
		if series["gpu"] {
			p.GPUUsePct = b.gpu.floatPtr()
		}
		if series["mem"] {
			p.MemUsedBytes = b.memUsed.intPtr()
			p.MemTotalBytes = b.memTotal.intPtr()
		}
		if series["disk"] {
			p.DiskUsedBytes = b.diskUsed.intPtr()
			p.DiskTotalBytes = b.diskTotal.intPtr()
		}
		if series["power"] {
			p.PackagePowerW = b.power.floatPtr()
			if p.PackagePowerW != nil && wallWatts != nil {
				est := wallWatts(*p.PackagePowerW)
				p.WallPowerWEst = &est
			}
		}
		if series["cpu"] {
			p.CPUPct = b.cpu.floatPtr()
		}
		if series["network"] {
			p.NetRxBytesPerSec = b.netRx.floatPtr()
			p.NetTxBytesPerSec = b.netTx.floatPtr()
		}
		return append(points, p)
	}

	var points []metricsHistoryPoint
	var cur *bucket
	for _, sm := range samples {
		ts := sm.TS.Unix()
		bucketTS := fromUnix + ((ts-fromUnix)/res64)*res64
		if cur == nil || cur.ts != bucketTS {
			points = flush(points, cur)
			cur = &bucket{ts: bucketTS}
		}
		cur.gttUsed.addInt(sm.GTTUsedBytes)
		cur.gttTotal.addInt(sm.GTTTotalBytes)
		cur.gpu.addFloat(sm.GPUUsePct)
		cur.memUsed.addInt(sm.MemUsedBytes)
		cur.memTotal.addInt(sm.MemTotalBytes)
		cur.diskUsed.addInt(sm.DiskUsedBytes)
		cur.diskTotal.addInt(sm.DiskTotalBytes)
		cur.power.addFloat(sm.PackagePowerW)
		cur.cpu.addFloat(sm.CPUPct)
		cur.netRx.addFloat(sm.NetRxBytesPerSec)
		cur.netTx.addFloat(sm.NetTxBytesPerSec)
	}
	points = flush(points, cur)
	if points == nil {
		points = []metricsHistoryPoint{}
	}
	return points
}

// ── §0.4 export ──────────────────────────────────────────────────────────────

// metricsExportResponse is the JSON export body. Not part of the Sprint 0
// frozen shapes (only history/disk were frozen there) — a local shape,
// export's consumer is a download link, not a typed FE query hook.
type metricsExportResponse struct {
	Window string             `json:"window"`
	Rows   []metricsExportRow `json:"rows"`
}

type metricsExportRow struct {
	TS                int64    `json:"ts"`
	GTTUsedBytes      *int64   `json:"gtt_used_bytes"`
	GTTTotalBytes     *int64   `json:"gtt_total_bytes"`
	GPUUsePct         *float64 `json:"gpu_use_pct"`
	MemUsedBytes      *int64   `json:"mem_used_bytes"`
	MemTotalBytes     *int64   `json:"mem_total_bytes"`
	DiskUsedBytes     *int64   `json:"disk_used_bytes"`
	DiskTotalBytes    *int64   `json:"disk_total_bytes"`
	TempCelsius       *float64 `json:"temp_celsius"`
	InferenceRSSBytes *int64   `json:"inference_rss_bytes"`

	// Appended, not inserted (cost/savings sprint, 2026-07-30) — this export
	// isn't part of the Sprint 0 frozen shapes, but someone may already
	// parse the CSV positionally, so new columns only ever go at the end.
	PackagePowerW *float64 `json:"package_power_w"`
	ActiveSlots   *int     `json:"active_slots"`
	EnergyWhTotal *float64 `json:"energy_wh_total"`

	// Appended for Phase 4 collector metrics (2026-08-12), same
	// append-only convention as the block above.
	CPUPct           *float64 `json:"cpu_pct"`
	NetRxBytesPerSec *float64 `json:"net_rx_bytes_per_sec"`
	NetTxBytesPerSec *float64 `json:"net_tx_bytes_per_sec"`
	GPUJunctionTempC *float64 `json:"gpu_junction_temp_celsius"`
	CPUPackageTempC  *float64 `json:"cpu_package_temp_celsius"`
	NVMeTempC        *float64 `json:"nvme_temp_celsius"`
}

var metricsExportCSVHeader = []string{
	"ts", "gtt_used_bytes", "gtt_total_bytes", "gpu_use_pct", "mem_used_bytes", "mem_total_bytes",
	"disk_used_bytes", "disk_total_bytes", "temp_celsius", "inference_rss_bytes",
	"package_power_w", "active_slots", "energy_wh_total",
	"cpu_pct", "net_rx_bytes_per_sec", "net_tx_bytes_per_sec",
	"gpu_junction_temp_celsius", "cpu_package_temp_celsius", "nvme_temp_celsius",
}

// handleMetricsExport — GET /api/v1/metrics/export?format=csv|json&window=90d
// (Sprint 0 §0.4). Full-resolution dump — no downsampling.
func (s *Server) handleMetricsExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	format := q.Get("format")
	if format == "" {
		format = "json"
	}
	if format != "csv" && format != "json" {
		writeValidationError(w, map[string]string{"format": "must be \"csv\" or \"json\""})
		return
	}

	windowRaw := q.Get("window")
	if windowRaw == "" {
		windowRaw = "90d"
	}
	window, err := parseMetricsWindow(windowRaw)
	if err != nil {
		writeValidationError(w, map[string]string{"window": err.Error()})
		return
	}

	var samples []store.MetricSample
	if s.deps.Metrics != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		to := time.Now().UTC()
		from := to.Add(-window)
		samples, err = s.deps.Metrics.Range(ctx, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metrics export read failed")
			return
		}
	}

	if format == "csv" {
		writeMetricsExportCSV(w, samples)
		return
	}
	rows := make([]metricsExportRow, 0, len(samples))
	for _, sm := range samples {
		rows = append(rows, metricsExportRow{
			TS: sm.TS.Unix(), GTTUsedBytes: sm.GTTUsedBytes, GTTTotalBytes: sm.GTTTotalBytes,
			GPUUsePct: sm.GPUUsePct, MemUsedBytes: sm.MemUsedBytes, MemTotalBytes: sm.MemTotalBytes,
			DiskUsedBytes: sm.DiskUsedBytes, DiskTotalBytes: sm.DiskTotalBytes,
			TempCelsius: sm.TempCelsius, InferenceRSSBytes: sm.InferenceRSSBytes,
			PackagePowerW: sm.PackagePowerW, ActiveSlots: sm.ActiveSlots, EnergyWhTotal: sm.EnergyWhTotal,
			CPUPct: sm.CPUPct, NetRxBytesPerSec: sm.NetRxBytesPerSec, NetTxBytesPerSec: sm.NetTxBytesPerSec,
			GPUJunctionTempC: sm.GPUJunctionTempC, CPUPackageTempC: sm.CPUPackageTempC, NVMeTempC: sm.NVMeTempC,
		})
	}
	writeJSON(w, http.StatusOK, metricsExportResponse{Window: windowRaw, Rows: rows})
}

func writeMetricsExportCSV(w http.ResponseWriter, samples []store.MetricSample) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="forge-metrics-export.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(metricsExportCSVHeader)
	for _, sm := range samples {
		_ = cw.Write([]string{
			strconv.FormatInt(sm.TS.Unix(), 10),
			csvInt(sm.GTTUsedBytes), csvInt(sm.GTTTotalBytes), csvFloat(sm.GPUUsePct),
			csvInt(sm.MemUsedBytes), csvInt(sm.MemTotalBytes),
			csvInt(sm.DiskUsedBytes), csvInt(sm.DiskTotalBytes),
			csvFloat(sm.TempCelsius), csvInt(sm.InferenceRSSBytes),
			csvFloat(sm.PackagePowerW), csvIntPtr(sm.ActiveSlots), csvFloat(sm.EnergyWhTotal),
			csvFloat(sm.CPUPct), csvFloat(sm.NetRxBytesPerSec), csvFloat(sm.NetTxBytesPerSec),
			csvFloat(sm.GPUJunctionTempC), csvFloat(sm.CPUPackageTempC), csvFloat(sm.NVMeTempC),
		})
	}
	cw.Flush()
}

func csvInt(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

func csvIntPtr(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func csvFloat(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}
