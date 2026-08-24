// SPDX-License-Identifier: Apache-2.0

package httpapi

// cost_handlers.go — measured electricity cost (cost/savings sprint,
// 2026-07-30). GET /api/v1/cost/summary integrates metric_samples'
// package_power_w into a real energy/cost figure; GET /api/v1/cost/settings
// + PUT expose the previously CLI-only infra.cost config; GET
// /api/v1/cost/energy-history gives the Dashboard a per-bucket watts/kWh/
// money trend. Owner track: this sprint's Phase 2 (see
// /home/testuser/.claude/plans/joyful-splashing-moonbeam.md).
//
// Every unmeasurable or estimated figure in these responses is null or
// carries explicit provenance (method, basis) rather than silently
// defaulting to zero — a dashboard number an operator can't trust is worse
// than a missing one.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// SettingInfraCost is the store-backed config key GET/PUT /api/v1/cost/settings
// edits. Deliberately NOT added to registeredSettingKeys (settings_handlers.go)
// — infra.* keys are a different vocabulary, loaded by config.LoadFromStore
// rather than read ad-hoc by individual handlers, and
// TestRegisteredSettingKeys asserts that list's exact membership.
const SettingInfraCost = "infra.cost"

// idleActiveGPUPct is the gpu_use_pct threshold above which a sample is
// classified "active" for the idle/active energy split. One constant, not a
// setting — see docs/v5-review-fixes.md-style precedent of keeping
// classification thresholds simple until real tuning need shows up.
const idleActiveGPUPct = 5.0

// ── config.Cost accessors ────────────────────────────────────────────────

// resolvedCost returns the fully-defaulted Cost the running daemon is
// actually using (config.ResolveCost handles a nil Config dependency).
func (s *Server) resolvedCost() config.Cost {
	var cfg *config.Config
	if s.deps.Config != nil {
		cfg = s.deps.Config()
	}
	return config.ResolveCost(cfg)
}

// wallWatts approximates wall power from a package-watts reading using the
// live resolved Cost — the same formula registry.powerRate() applies to the
// per-model card estimate, so both paths agree.
func (s *Server) wallWatts(packageW float64) float64 {
	return s.resolvedCost().WallWatts(packageW)
}

// ── GET/PUT /api/v1/cost/settings ────────────────────────────────────────

type costSettingsBody struct {
	PowerKW       *float64 `json:"power_kw"`
	RatePerKWh    *float64 `json:"rate_per_kwh"`
	RateCurrency  *string  `json:"rate_currency"`
	OverheadW     *float64 `json:"overhead_w"`
	PSUEfficiency *float64 `json:"psu_efficiency"`
	MaxPowerW     *float64 `json:"max_power_w"`
}

type costSettingsResponse struct {
	PowerKW       float64 `json:"power_kw"`
	RatePerKWh    float64 `json:"rate_per_kwh"`
	RateCurrency  string  `json:"rate_currency"`
	OverheadW     float64 `json:"overhead_w"`
	PSUEfficiency float64 `json:"psu_efficiency"`
	MaxPowerW     float64 `json:"max_power_w"`
}

func costSettingsResponseFrom(c config.Cost) costSettingsResponse {
	return costSettingsResponse{
		PowerKW: c.PowerKW, RatePerKWh: c.RatePerKWh, RateCurrency: c.RateCurrency,
		OverheadW: c.OverheadW, PSUEfficiency: c.PSUEfficiency, MaxPowerW: c.MaxPowerW,
	}
}

// handleCostSettingsGet — GET /api/v1/cost/settings (operator). Returns the
// live resolved values (defaults filled in), not the raw possibly-partial
// stored JSON — matches what the running daemon is actually pricing with.
func (s *Server) handleCostSettingsGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, costSettingsResponseFrom(s.resolvedCost()))
}

// handleCostSettingsPut — PUT /api/v1/cost/settings (admin). Merges the
// provided fields onto the current resolved Cost (so a partial body doesn't
// blow away untouched fields), validates, persists to the infra.cost
// settings key config.LoadFromStore reads, then reloads the running
// config — infra.* keys otherwise only take effect on SIGHUP
// (cmd/forge/main.go's reloadConfigFromStore).
func (s *Server) handleCostSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var b costSettingsBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}

	merged := s.resolvedCost()
	if b.PowerKW != nil {
		merged.PowerKW = *b.PowerKW
	}
	if b.RatePerKWh != nil {
		merged.RatePerKWh = *b.RatePerKWh
	}
	if b.RateCurrency != nil {
		merged.RateCurrency = *b.RateCurrency
	}
	if b.OverheadW != nil {
		merged.OverheadW = *b.OverheadW
	}
	if b.PSUEfficiency != nil {
		merged.PSUEfficiency = *b.PSUEfficiency
	}
	if b.MaxPowerW != nil {
		merged.MaxPowerW = *b.MaxPowerW
	}

	fields := map[string]string{}
	if merged.PowerKW <= 0 || merged.PowerKW > 5 {
		fields["power_kw"] = "must be in (0, 5]"
	}
	if merged.RatePerKWh <= 0 || merged.RatePerKWh > 100 {
		fields["rate_per_kwh"] = "must be in (0, 100)"
	}
	if !currencyRE.MatchString(merged.RateCurrency) {
		fields["rate_currency"] = "must be a 3-letter ISO 4217 code (e.g. USD)"
	}
	if merged.OverheadW < 0 || merged.OverheadW > 1000 {
		fields["overhead_w"] = "must be in [0, 1000]"
	}
	if merged.PSUEfficiency <= 0 || merged.PSUEfficiency > 1 {
		fields["psu_efficiency"] = "must be in (0, 1]"
	}
	if merged.MaxPowerW <= 0 || merged.MaxPowerW > 2000 {
		fields["max_power_w"] = "must be in (0, 2000]"
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	raw, err := json.Marshal(merged)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(ctx, SettingInfraCost, raw); err != nil {
		writeInternalError(w, err)
		return
	}
	if s.deps.ReloadConfig != nil {
		s.deps.ReloadConfig()
	}
	s.audit(r, identity(r).Name, "cost_settings", "power_kw", strconv.FormatFloat(merged.PowerKW, 'g', -1, 64))
	writeJSON(w, http.StatusOK, costSettingsResponseFrom(merged))
}

// ── Energy integration ────────────────────────────────────────────────────

// energyResult is computeEnergy's pure output — kept free of any FX/config
// dependency so it's directly unit-testable against synthetic samples.
type energyResult struct {
	MeasuredSeconds   float64
	GapSeconds        float64
	UnmeasuredSeconds float64
	PackageWh         float64 // raw sensor total, unadjusted
	WallWhEst         float64 // wall-adjusted total (the basis for cost)
	IdleWhEst         float64
	ActiveWhEst       float64
	IdleBaselineW     float64 // median wall watts across idle samples
	ActiveSeconds     float64
	SingleSlotSeconds float64
	MultiSlotSeconds  float64
	// activeSingleSlotWallW collects wall-watt readings from
	// active+single-slot intervals for the calibration percentile block.
	activeSingleSlotWallW []float64
}

func (e energyResult) CoveragePct() float64 {
	total := e.MeasuredSeconds + e.GapSeconds + e.UnmeasuredSeconds
	if total <= 0 {
		return 0
	}
	return e.MeasuredSeconds / total * 100
}

// AttributableWhEst is the marginal energy of active work over just leaving
// the box idle: active energy minus what idling for the same duration would
// have cost at the observed idle baseline. Clamped at 0 — a negative figure
// would only mean the idle baseline sample was noisier than the active one,
// not that running models saved energy.
func (e energyResult) AttributableWhEst() float64 {
	v := e.ActiveWhEst - e.IdleBaselineW*e.ActiveSeconds/3600
	if v < 0 {
		return 0
	}
	return v
}

// computeEnergy integrates consecutive metric_samples pairs into an energy
// estimate. samples must be ordered oldest-first (store.Metrics.Range's
// contract). sampleIntervalS is the configured sampler cadence (defines the
// gap threshold: a gap wider than 3 sample intervals, floored at 5 minutes,
// is treated as a real outage, not integrated across).
//
// Rules, in order, per consecutive pair:
//  1. either endpoint's PackagePowerW is nil -> the interval is UNMEASURED
//     (never valued at 0W, never interpolated across).
//  2. the interval is wider than the gap threshold -> a GAP (a daemon
//     restart or host downtime; also never integrated across).
//  3. otherwise trapezoid-integrate: Wh += (w1+w2)/2 * dt/3600.
//
// Idle/active classification and single/multi-slot accounting use the
// interval's leading sample only (gpu_use_pct / active_slots respectively);
// an interval whose leading sample lacks that field contributes to the
// totals above but not to the idle/active or single/multi split — so
// idle_wh_est+active_wh_est can be slightly less than wall_wh_est. This is
// disclosed via CoveragePct/the caller, not hidden.
func computeEnergy(samples []store.MetricSample, cost config.Cost, sampleIntervalS int) energyResult {
	var e energyResult
	if sampleIntervalS <= 0 {
		sampleIntervalS = defaultSampleIntervalS
	}
	maxGapS := float64(3 * sampleIntervalS)
	if maxGapS < 300 {
		maxGapS = 300
	}

	var idleWallWatts []float64

	for i := 0; i+1 < len(samples); i++ {
		s1, s2 := samples[i], samples[i+1]
		dt := s2.TS.Sub(s1.TS).Seconds()
		if dt <= 0 {
			continue
		}
		if s1.PackagePowerW == nil || s2.PackagePowerW == nil {
			e.UnmeasuredSeconds += dt
			continue
		}
		if dt > maxGapS {
			e.GapSeconds += dt
			continue
		}
		e.MeasuredSeconds += dt
		avgPackageW := (*s1.PackagePowerW + *s2.PackagePowerW) / 2
		packageWh := avgPackageW * dt / 3600
		e.PackageWh += packageWh
		avgWallW := cost.WallWatts(avgPackageW)
		wallWh := avgWallW * dt / 3600
		e.WallWhEst += wallWh

		active := s1.GPUUsePct != nil && *s1.GPUUsePct >= idleActiveGPUPct
		if s1.GPUUsePct != nil {
			if active {
				e.ActiveWhEst += wallWh
				e.ActiveSeconds += dt
			} else {
				e.IdleWhEst += wallWh
				idleWallWatts = append(idleWallWatts, avgWallW)
			}
		}

		if s1.ActiveSlots != nil {
			switch {
			case *s1.ActiveSlots <= 1:
				e.SingleSlotSeconds += dt
				if active {
					e.activeSingleSlotWallW = append(e.activeSingleSlotWallW, avgWallW)
				}
			case *s1.ActiveSlots > 1:
				e.MultiSlotSeconds += dt
			}
		}
	}
	e.IdleBaselineW = median(idleWallWatts)
	return e
}

// median returns the median of vals (sorted copy; even-length averages the
// two middle values). 0 for an empty slice.
func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// percentile returns the p-th percentile (0-100) of vals via nearest-rank.
// 0 for an empty slice — callers must check len(vals) before trusting a
// calibration figure computed from too few samples.
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	rank := int(p/100*float64(len(sorted)-1) + 0.5)
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// ── GET /api/v1/cost/summary ──────────────────────────────────────────────

type costSummaryResponse struct {
	Window          string   `json:"window"`
	DisplayCurrency string   `json:"display_currency"`
	FxAsOf          *float64 `json:"fx_as_of"`
	FxStale         bool     `json:"fx_stale"`
	Energy          costEnergyJSON `json:"energy"`
}

type costEnergyJSON struct {
	Method            string  `json:"method"` // "trapezoid" (Phase 1b adds "counter"/"mixed")
	MeasuredSeconds   float64 `json:"measured_seconds"`
	GapSeconds        float64 `json:"gap_seconds"`
	UnmeasuredSeconds float64 `json:"unmeasured_seconds"`
	CoveragePct       float64 `json:"coverage_pct"`
	PackageWh         float64 `json:"package_wh"`
	WallWhEst         float64 `json:"wall_wh_est"`
	OverheadW         float64 `json:"overhead_w"`
	PSUEfficiency     float64 `json:"psu_efficiency"`
	RatePerKWh        float64 `json:"rate_per_kwh"`
	RateCurrency      string  `json:"rate_currency"`
	CostDisplay       float64 `json:"cost_display"`
	IdleWhEst         float64 `json:"idle_wh_est"`
	ActiveWhEst       float64 `json:"active_wh_est"`
	AttributableWhEst float64 `json:"attributable_wh_est"`
	IdleBaselineW     float64 `json:"idle_baseline_w"`
	ActiveSeconds     float64 `json:"active_seconds"`
	SingleSlotSeconds float64 `json:"single_slot_seconds"`
	MultiSlotSeconds  float64 `json:"multi_slot_seconds"`
	Calibration       costCalibrationJSON `json:"calibration"`
}

type costCalibrationJSON struct {
	SingleSlotActiveWallWP50 *float64 `json:"single_slot_active_wall_w_p50"`
	SingleSlotActiveWallWP95 *float64 `json:"single_slot_active_wall_w_p95"`
	Samples                  int      `json:"samples"`
}

// handleCostSummary — GET /api/v1/cost/summary?window=7d (operator). The
// Dashboard Cost tab's single call for the measured-electricity figures.
// Virtual-spend/remote-spend/Compressor-savings sections land additively in
// later phases of this sprint (Phase 3/4) — not yet part of this response.
func (s *Server) handleCostSummary(w http.ResponseWriter, r *http.Request) {
	windowRaw := r.URL.Query().Get("window")
	if windowRaw == "" {
		windowRaw = "7d"
	}
	window, err := parseMetricsWindow(windowRaw)
	if err != nil {
		writeValidationError(w, map[string]string{"window": err.Error()})
		return
	}

	cost := s.resolvedCost()
	resp := costSummaryResponse{
		Window:          windowRaw,
		DisplayCurrency: cost.RateCurrency,
		Energy: costEnergyJSON{
			Method:        "trapezoid",
			OverheadW:     cost.OverheadW,
			PSUEfficiency: cost.PSUEfficiency,
			RatePerKWh:    cost.RatePerKWh,
			RateCurrency:  cost.RateCurrency,
		},
	}

	if s.deps.Metrics == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	to := time.Now().UTC()
	from := to.Add(-window)
	samples, err := s.deps.Metrics.Range(ctx, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cost summary read failed")
		return
	}

	e := computeEnergy(samples, cost, s.metricsSampleIntervalS())
	display := s.displayCurrency(ctx)
	resp.DisplayCurrency = display
	costNative := e.WallWhEst / 1000 * cost.RatePerKWh
	costDisplay, missing := s.convert(ctx, costNative, cost.RateCurrency, display)
	anyConversion := display != cost.RateCurrency
	resp.FxAsOf, resp.FxStale = s.fxProvenance(ctx, anyConversion, missing)

	resp.Energy.MeasuredSeconds = e.MeasuredSeconds
	resp.Energy.GapSeconds = e.GapSeconds
	resp.Energy.UnmeasuredSeconds = e.UnmeasuredSeconds
	resp.Energy.CoveragePct = round6(e.CoveragePct())
	resp.Energy.PackageWh = round6(e.PackageWh)
	resp.Energy.WallWhEst = round6(e.WallWhEst)
	resp.Energy.CostDisplay = round6(costDisplay)
	resp.Energy.IdleWhEst = round6(e.IdleWhEst)
	resp.Energy.ActiveWhEst = round6(e.ActiveWhEst)
	resp.Energy.AttributableWhEst = round6(e.AttributableWhEst())
	resp.Energy.IdleBaselineW = round6(e.IdleBaselineW)
	resp.Energy.ActiveSeconds = e.ActiveSeconds
	resp.Energy.SingleSlotSeconds = e.SingleSlotSeconds
	resp.Energy.MultiSlotSeconds = e.MultiSlotSeconds
	resp.Energy.Calibration.Samples = len(e.activeSingleSlotWallW)
	// Calibration percentiles need a minimally meaningful sample count —
	// a single-slot-active interval or two shouldn't produce a confident
	// p95. 10 is an arbitrary but reasonable floor (below it: null, not a
	// misleadingly precise-looking number from noise).
	if len(e.activeSingleSlotWallW) >= 10 {
		p50 := round6(percentile(e.activeSingleSlotWallW, 50))
		p95 := round6(percentile(e.activeSingleSlotWallW, 95))
		resp.Energy.Calibration.SingleSlotActiveWallWP50 = &p50
		resp.Energy.Calibration.SingleSlotActiveWallWP95 = &p95
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/cost/energy-history ───────────────────────────────────────

type costEnergyHistoryResponse struct {
	Window      string                   `json:"window"`
	ResolutionS int                      `json:"resolution_s"`
	Points      []costEnergyHistoryPoint `json:"points"`
}

type costEnergyHistoryPoint struct {
	TS            int64   `json:"ts"`
	PackageWh     float64 `json:"package_wh"`
	WallWhEst     float64 `json:"wall_wh_est"`
	CostDisplay   float64 `json:"cost_display"`
	CoveragePct   float64 `json:"coverage_pct"`
}

// handleCostEnergyHistory — GET /api/v1/cost/energy-history?window=30d&res=auto
// (operator). Per-bucket watts/kWh/money — a day-scale trend, unlike
// /metrics/history's "auto" resolution (money-per-bucket depends on
// config.Cost + FX, which that frozen endpoint has no business knowing).
func (s *Server) handleCostEnergyHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	windowRaw := q.Get("window")
	if windowRaw == "" {
		windowRaw = "30d"
	}
	window, err := parseMetricsWindow(windowRaw)
	if err != nil {
		writeValidationError(w, map[string]string{"window": err.Error()})
		return
	}
	// Energy buckets default to a day, not historyTargetPoints-derived —
	// a sub-hour "auto" bucket for a money trend is noise, not signal.
	resolutionS := 24 * 3600
	if raw := q.Get("res"); raw != "" && raw != "auto" {
		n, err := parsePositiveInt(raw)
		if err != nil || n <= 0 {
			writeValidationError(w, map[string]string{"res": "must be a positive integer number of seconds, or \"auto\""})
			return
		}
		resolutionS = n
	}

	resp := costEnergyHistoryResponse{Window: windowRaw, ResolutionS: resolutionS, Points: []costEnergyHistoryPoint{}}
	if s.deps.Metrics == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	to := time.Now().UTC()
	from := to.Add(-window)
	samples, err := s.deps.Metrics.Range(ctx, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cost energy history read failed")
		return
	}

	cost := s.resolvedCost()
	sampleIntervalS := s.metricsSampleIntervalS()
	fromUnix := from.Unix()
	res64 := int64(resolutionS)

	// Bucket the raw samples, then run computeEnergy independently per
	// bucket — bucket boundaries must not smear a real gap/unmeasured
	// interval across two buckets' worth of coverage accounting.
	buckets := map[int64][]store.MetricSample{}
	var order []int64
	for _, sm := range samples {
		ts := sm.TS.Unix()
		bucketTS := fromUnix + ((ts-fromUnix)/res64)*res64
		if _, ok := buckets[bucketTS]; !ok {
			order = append(order, bucketTS)
		}
		buckets[bucketTS] = append(buckets[bucketTS], sm)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	for _, ts := range order {
		e := computeEnergy(buckets[ts], cost, sampleIntervalS)
		costNative := e.WallWhEst / 1000 * cost.RatePerKWh
		costDisplay, _ := s.convert(ctx, costNative, cost.RateCurrency, s.displayCurrency(ctx))
		resp.Points = append(resp.Points, costEnergyHistoryPoint{
			TS: ts, PackageWh: round6(e.PackageWh), WallWhEst: round6(e.WallWhEst),
			CostDisplay: round6(costDisplay), CoveragePct: round6(e.CoveragePct()),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
