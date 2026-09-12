// SPDX-License-Identifier: Apache-2.0

// Package statutil holds small, dependency-free statistics helpers shared
// across packages that compute a distribution from a bounded set of raw
// samples (rather than a streaming histogram — this repo has no
// bucketed-histogram library anywhere, see internal/httpapi/cost_handlers.go
// and cmd/forge-compress/metrics.go's doc comments). Promoted 2026-09-11
// from internal/httpapi/cost_handlers.go's unexported percentile/median
// (originally written for the cost/summary energy-calibration figures) so a
// second caller (cmd/forge-compress's overhead percentiles) doesn't
// reimplement the same ~20 lines with its own drift risk.
package statutil

import "sort"

// Median returns the median of vals (sorted copy; even-length averages the
// two middle values). 0 for an empty slice.
func Median(vals []float64) float64 {
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

// Percentile returns the p-th percentile (0-100) of vals via nearest-rank.
// 0 for an empty slice — callers must check len(vals) against their own
// minimum-sample floor before trusting a figure computed from too few
// samples (this repo's established convention is 10 — see
// cost_handlers.go's activeSingleSlotWallW gate and
// compressor_summary_handlers.go's prefillObservedMinSamples, both of which
// cite this same floor by name).
func Percentile(vals []float64, p float64) float64 {
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
