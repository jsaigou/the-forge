// SPDX-License-Identifier: Apache-2.0

package smith

import "github.com/jsaigou/the-forge/internal/store"

// CompressorHealth is ClassifyCompressorHealth's result — a single enum
// value plus the evidence that produced it, so a caller can report the
// verdict without recomputing anything.
type CompressorHealth struct {
	// Status is one of "unknown" (no samples in the window), "ok",
	// "restarting", or "memory_growth". Never "down" — that's a routing/
	// reachability fact (store.ProxyRow + unit active state), already
	// covered by headroom_health/CompressorState; this is resource health on
	// top of an assumed-reachable proxy.
	Status string

	WindowStart, WindowEnd store.CompressorSampleRow
	RestartDelta           int64
	RSSGrowthPct           float64
}

// ClassifyCompressorHealth reads a leak-shaped-growth or restart-churn
// signal from one proxy's compressor_samples history (Sprint 4). Shared by
// the compressor_health check (checks.go) and the Dashboard's Compressor tile
// (httpapi.compressorServiceRows) so both apply the identical judgment rather
// than two independently-tuned heuristics drifting apart.
//
// samples must be ordered oldest-first (store.Compressors().Range's
// contract). Restart churn is checked first and, if it fires, takes
// priority over memory growth: a unit that's actively restart-looping is
// the more acute problem, and a restart resets RSS to a fresh baseline
// anyway, which would otherwise mask real growth as a false negative — so
// growth is only evaluated when NO restart occurred in the window.
func ClassifyCompressorHealth(samples []store.CompressorSampleRow, th Thresholds) CompressorHealth {
	if len(samples) == 0 {
		return CompressorHealth{Status: "unknown"}
	}
	first, last := samples[0], samples[len(samples)-1]
	result := CompressorHealth{WindowStart: first, WindowEnd: last}
	result.RestartDelta = int64(last.NRestarts) - int64(first.NRestarts)

	if th.CompressorRestartsWarnPerHour > 0 && result.RestartDelta > 0 {
		hours := last.TS.Sub(first.TS).Hours()
		if hours < 1 {
			hours = 1 // sub-hour windows don't get an artificially inflated rate
		}
		if float64(result.RestartDelta)/hours >= th.CompressorRestartsWarnPerHour {
			result.Status = "restarting"
			return result
		}
	}

	if result.RestartDelta == 0 && first.RSSBytes > 0 && last.RSSBytes > first.RSSBytes {
		result.RSSGrowthPct = (float64(last.RSSBytes) - float64(first.RSSBytes)) / float64(first.RSSBytes) * 100
		if th.CompressorRSSGrowthWarnPct > 0 && result.RSSGrowthPct >= th.CompressorRSSGrowthWarnPct {
			result.Status = "memory_growth"
			return result
		}
	}

	result.Status = "ok"
	return result
}
