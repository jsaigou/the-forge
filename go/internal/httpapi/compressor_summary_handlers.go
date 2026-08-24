// SPDX-License-Identifier: Apache-2.0

package httpapi

// compressor_summary_handlers.go — GET /api/v1/compressor/summary (cost/savings
// sprint, Phase 3, 2026-07-30). Per-proxy Compressor activity: cache-hit rate,
// tokens/requests, mean+since-start-min/max latency/ttfb/overhead, and (for
// the local proxy only) an ESTIMATED time-saved figure.
//
// Provider cache token metrics (cache_read_tokens, uncached_tokens, etc.)
// were confirmed live on headroom-ai 0.30.0 (2026-08-14) — the metrics were
// always available in /metrics but were lazily registered (only appearing
// after the first request that triggers them). The prior comment claiming
// "that metric doesn't exist in the installed build" was wrong: it was a
// freshly-restarted proxy that hadn't seen traffic yet. As of the 0.35.0
// upgrade, these metrics are now scraped and persisted.
//
// Time-saved remains a count-based estimate for LOCAL proxies: cached
// requests × (this window's own average tokens/request) ÷ a real prefill-tok/s
// figure. Every such figure carries "_est" in its JSON name and a
// tps_source/tps_mode disclosing exactly how the TPS was obtained.
//
// Local-savings prefill sprint (2026-08-06): Compressor's local benefit is
// PURELY avoided re-prefill — it has no effect on decode/generation at all
// — so the TPS divisor above must always be a real prefill measurement,
// never a decode/generation figure or a fabricated constant (see
// docs/progress.md's 2026-08-06 entries for the incident this fixes: a flat
// 50 tok/s "generation" fallback produced a 493-hour "saved" figure inside
// a 168-hour window). estimateCompressorTimeSaved is a SUM over the models
// that actually contributed cached requests this window, each priced
// against its OWN real prefill TPS via lookupPrefillTPS — never one
// dominant-model figure applied to the whole window's cached tokens. A
// model with cached requests but no resolvable TPS is logged as an anomaly
// and excluded from the sum (never guessed): the set of models needing a
// TPS figure and the set having one are the same set by construction (a
// model that ran had its counters scraped) — see internal/collector's
// OnPrefillSample and store.PrefillStats.
import (
	"context"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jsaigou/the-forge/internal/profile"
	"github.com/jsaigou/the-forge/internal/store"
)

// prefillObservedMinSamples gates lookupPrefillTPS's "observed" step (real,
// passively-collected data) behind a minimum sample count so a couple of
// noisy early observations don't produce a misleadingly-precise-looking
// figure — mirrors cost_handlers.go's calibration-percentile floor
// (computeEnergy's activeSingleSlotWallW gate).
const prefillObservedMinSamples = 10

type compressorSummaryResponse struct {
	Window  string                     `json:"window"`
	Proxies []compressorSummaryProxyJSON `json:"proxies"`
	// DisplayCurrency/FxAsOf/FxStale mirror the same fields on UsageResponse
	// (usage_handlers.go) / costSummaryResponse (cost_handlers.go) — every
	// *_native money figure below is additionally FX-converted into this
	// currency in the matching *_display field. Added 2026-07-31: this
	// endpoint used to be the only money-bearing response that never
	// converted, so an operator with billing.display_currency=JPY still saw
	// a raw USD figure on the Compressor chips.
	DisplayCurrency string   `json:"display_currency"`
	FxAsOf          *float64 `json:"fx_as_of"`
	FxStale         bool     `json:"fx_stale"`
}

type compressorSummaryProxyJSON struct {
	Proxy string `json:"proxy"`
	Kind  string `json:"kind"` // "local" | "remote"

	TokensIn            int64 `json:"tokens_in"`
	TokensOut           int64 `json:"tokens_out"`
	TokensSaved         int64 `json:"tokens_saved"` // compression savings; 0 under --lossless
	Requests            int64 `json:"requests"`
	RequestsCached      int64 `json:"requests_cached"`
	RequestsFailed      int64 `json:"requests_failed"`
	RequestsRateLimited int64 `json:"requests_rate_limited"`
	// CacheHitRatePct is null when Requests is 0 (nothing to divide by),
	// never a fabricated 0%.
	CacheHitRatePct *float64 `json:"cache_hit_rate_pct"`

	TTFBMeanMs          *float64 `json:"ttfb_mean_ms"`
	TTFBMinMsSinceStart *float64 `json:"ttfb_min_ms_since_start"`
	TTFBMaxMsSinceStart *float64 `json:"ttfb_max_ms_since_start"`

	LatencyMeanMs          *float64 `json:"latency_mean_ms"`
	LatencyMinMsSinceStart *float64 `json:"latency_min_ms_since_start"`
	LatencyMaxMsSinceStart *float64 `json:"latency_max_ms_since_start"`

	OverheadMeanMs          *float64 `json:"overhead_mean_ms"`
	OverheadMinMsSinceStart *float64 `json:"overhead_min_ms_since_start"`
	OverheadMaxMsSinceStart *float64 `json:"overhead_max_ms_since_start"`

	RequestsByProvider map[string]int64 `json:"requests_by_provider,omitempty"`
	RequestsByModel    map[string]int64 `json:"requests_by_model,omitempty"`

	// Provider cache token metrics (from compress_cache_read_tokens_total,
	// compress_uncached_input_tokens_total, etc. — available since headroom-ai
	// 0.30.0, scraped as of the 0.35.0 upgrade).
	CacheReadTokens           int64            `json:"cache_read_tokens,omitempty"`
	UncachedTokens            int64            `json:"uncached_tokens,omitempty"`
	CacheBusts                int64            `json:"cache_busts,omitempty"`
	CacheBustTokensLost       int64            `json:"cache_bust_tokens_lost,omitempty"`
	CacheReadTokensByProvider map[string]int64 `json:"cache_read_tokens_by_provider,omitempty"`
	UncachedTokensByProvider  map[string]int64 `json:"uncached_tokens_by_provider,omitempty"`
	ProviderCacheRequests    map[string]int64 `json:"provider_cache_requests,omitempty"`
	ProviderCacheHitRequests map[string]int64 `json:"provider_cache_hit_requests,omitempty"`

	// Per-compressor timing (from compressor_transform_timing_ms_{sum,count}).
	TransformTimingMs    map[string]float64 `json:"transform_timing_ms,omitempty"`
	TransformTimingCount map[string]int64   `json:"transform_timing_count,omitempty"`

	// Local-only, TPS-independent: cached-request tokens that avoided a full
	// re-prefill (RequestsCached × this window's own avg tokens/request).
	// Set whenever anything was cached this window.
	TokensSavedEst *float64 `json:"tokens_saved_est,omitempty"`

	// Local-only estimate, TPS-sourced (nil for remote proxies — see the
	// package doc). Every TPS source behind this is a REAL measurement now
	// (no flat-rate fallback exists) — nil here means no model contributing
	// cached requests this window had a resolvable TPS, which is an anomaly
	// (see PrefillBreakdown), not the routine case.
	TimeSavedSecondsEst *float64 `json:"time_saved_seconds_est,omitempty"`
	MoneySavedEst       *float64 `json:"money_saved_est,omitempty"`
	MoneySavedCurrency  string   `json:"money_saved_currency,omitempty"`
	// MoneySavedDisplay is MoneySavedEst FX-converted from MoneySavedCurrency
	// (cost.RateCurrency) to the response's DisplayCurrency. Nil whenever
	// MoneySavedEst is nil.
	MoneySavedDisplay *float64 `json:"money_saved_display,omitempty"`
	// TPSSource/TPSMode describe the single largest contributor to
	// TimeSavedSecondsEst (by share of cached requests) — see
	// PrefillBreakdown for the full per-model picture.
	TPSSource string `json:"tps_source,omitempty"`
	TPSMode   string `json:"tps_mode,omitempty"`
	// PrefillBreakdown is the full per-model accounting behind
	// TimeSavedSecondsEst: every model that contributed cached requests this
	// window AND had a resolvable real prefill TPS, with its share of this
	// proxy's requests and which source produced its TPS. Local-only, nil
	// for remote proxies. A model missing from this list despite appearing
	// in RequestsByModel is the anomaly case — logged server-side, never
	// silently substituted.
	PrefillBreakdown []compressorPrefillModelJSON `json:"prefill_breakdown,omitempty"`

	// Remote-only. CachedPromptTokens is EVERY cached-prompt token the
	// provider reported this window, regardless of whether we have pricing
	// to discount it — real (not estimated), from usage_events'
	// CachedPromptTokens. CacheDiscountSavedNative/Tokens are the (real,
	// measured) priced SUBSET of that: only cached tokens whose offering has
	// a modelled PriceCachedInPer1M contribute money. The two token counts
	// can differ when pricing is incomplete — itself worth surfacing, not
	// worth hiding by dropping the unpriced tokens on the floor.
	CachedPromptTokens         int64    `json:"cached_prompt_tokens,omitempty"`
	CacheDiscountSavedNative   *float64 `json:"cache_discount_saved_native,omitempty"`
	CacheDiscountSavedCurrency string   `json:"cache_discount_saved_currency,omitempty"`
	CacheDiscountTokens        int64    `json:"cache_discount_tokens,omitempty"`
	// CacheDiscountSavedDisplay is CacheDiscountSavedNative FX-converted from
	// CacheDiscountSavedCurrency (the offering's own currency) to the
	// response's DisplayCurrency. Nil whenever CacheDiscountSavedNative is nil.
	CacheDiscountSavedDisplay *float64 `json:"cache_discount_saved_display,omitempty"`

	// Remote-only, REAL (not estimated): Compressor's own compression saving —
	// TokensSaved (compress_tokens_saved_total, tokens the compressor removed
	// from the prompt before it ever reached the provider) priced at this
	// window's own blended input rate for the provider. This is distinct
	// from CacheDiscountSaved* above: that figure prices the PROVIDER's own
	// prompt-cache discount, which applies whether or not Compressor is in the
	// path at all, so it was never actually a Compressor saving. This is.
	CompressionSavedNative   *float64 `json:"compression_saved_native,omitempty"`
	CompressionSavedCurrency string   `json:"compression_saved_currency,omitempty"`
	CompressionRatePer1M     *float64 `json:"compression_rate_per_1m,omitempty"`
	// CompressionSavedDisplay is CompressionSavedNative FX-converted from
	// CompressionSavedCurrency (the offering's own currency) to the
	// response's DisplayCurrency. Nil whenever CompressionSavedNative is nil.
	CompressionSavedDisplay *float64 `json:"compression_saved_display,omitempty"`
}

// compressorPrefillModelJSON is one model's contribution to a local proxy's
// TimeSavedSecondsEst — see compressorSummaryProxyJSON.PrefillBreakdown.
type compressorPrefillModelJSON struct {
	Mode   string  `json:"mode"`
	Share  float64 `json:"share"` // 0-1, this model's share of the proxy's requests this window
	TPS    float64 `json:"tps"`
	Source string  `json:"source"`
}

// handleCompressorSummary — GET /api/v1/compressor/summary?window=7d (operator).
func (s *Server) handleCompressorSummary(w http.ResponseWriter, r *http.Request) {
	windowRaw := r.URL.Query().Get("window")
	if windowRaw == "" {
		windowRaw = "7d"
	}
	window, err := parseMetricsWindow(windowRaw)
	if err != nil {
		writeValidationError(w, map[string]string{"window": err.Error()})
		return
	}
	display := s.displayCurrency(r.Context())
	resp := compressorSummaryResponse{Window: windowRaw, Proxies: []compressorSummaryProxyJSON{}, DisplayCurrency: display}
	if s.deps.Routing == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	since := time.Now().Add(-window)
	summary, err := s.deps.Routing.SavingsSummary(ctx, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "compressor summary read failed")
		return
	}
	proxyRows, err := s.deps.Routing.Proxies(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "compressor proxies read failed")
		return
	}
	kindByService := map[string]string{}
	// providerNameByService (0042): the estimators below used to be handed
	// the PROXY's service name and filter usage_events by treating it as
	// the provider name — only correct because this deployment's remote
	// proxies happen to be named identically to their linked provider, a
	// soft convention with no enforcement. Now that Proxies() carries the
	// real linked ProviderName via the provider_id FK, use that instead.
	providerNameByService := map[string]string{}
	for _, p := range proxyRows {
		if p.ProviderID == nil {
			kindByService[p.Service] = "local"
		} else {
			kindByService[p.Service] = "remote"
			providerNameByService[p.Service] = p.ProviderName
		}
	}

	var proxies []string
	for service := range summary {
		proxies = append(proxies, service)
	}
	sort.Strings(proxies)

	// Loaded once up front (not per-proxy): every remote estimator below
	// reads the same window of usage_events + the same offering price list,
	// just filtered by provider.
	roc, hasROC := s.loadRemoteOfferingContext(ctx, since)

	var anyConversion, anyMissing bool
	for _, service := range proxies {
		p := summary[service]
		kind := kindByService[service]
		if kind == "" {
			kind = "local" // unknown proxy (row deleted since) — safest default, never estimate remote money
		}
		j := compressorSummaryProxyJSON{
			Proxy: service, Kind: kind,
			TokensIn: p.TokensIn, TokensOut: p.TokensOut, TokensSaved: p.TokensSaved,
			Requests: p.Requests, RequestsCached: p.RequestsCached,
			RequestsFailed: p.RequestsFailed, RequestsRateLimited: p.RequestsRateLimited,
			RequestsByProvider: p.RequestsByProvider, RequestsByModel: p.RequestsByModel,
			CacheReadTokens:           p.CacheReadTokens,
			UncachedTokens:            p.UncachedTokens,
			CacheBusts:                p.CacheBusts,
			CacheBustTokensLost:       p.CacheBustTokensLost,
			CacheReadTokensByProvider: p.CacheReadTokensByProvider,
			UncachedTokensByProvider:  p.UncachedTokensByProvider,
			ProviderCacheRequests:    p.ProviderCacheRequests,
			ProviderCacheHitRequests: p.ProviderCacheHitRequests,
			TransformTimingMs:        p.TransformTimingSum,
			TransformTimingCount:     p.TransformTimingCount,
		}
		if p.Requests > 0 {
			rate := round6(float64(p.RequestsCached) / float64(p.Requests) * 100)
			j.CacheHitRatePct = &rate
		}
		j.TTFBMeanMs = meanMs(p.TTFBSumMs, p.TTFBCount)
		j.TTFBMinMsSinceStart, j.TTFBMaxMsSinceStart = p.TTFBMinMs, p.TTFBMaxMs
		j.LatencyMeanMs = meanMs(p.LatencySumMs, p.LatencyCount)
		j.LatencyMinMsSinceStart, j.LatencyMaxMsSinceStart = p.LatencyMinMs, p.LatencyMaxMs
		j.OverheadMeanMs = meanMs(p.OverheadSumMs, p.OverheadCount)
		j.OverheadMinMsSinceStart, j.OverheadMaxMsSinceStart = p.OverheadMinMs, p.OverheadMaxMs

		if kind == "local" {
			c, m := s.estimateCompressorTimeSaved(ctx, &j, p, display)
			anyConversion, anyMissing = anyConversion || c, anyMissing || m
		} else if hasROC {
			c1, m1 := s.estimateRemoteCacheDiscountSaved(ctx, &j, roc, providerNameByService[service], display)
			c2, m2 := s.estimateRemoteCompressionSaved(ctx, &j, roc, providerNameByService[service], display)
			anyConversion, anyMissing = anyConversion || c1 || c2, anyMissing || m1 || m2
		}
		resp.Proxies = append(resp.Proxies, j)
	}
	resp.FxAsOf, resp.FxStale = s.fxProvenance(ctx, anyConversion, anyMissing)
	writeJSON(w, http.StatusOK, resp)
}

// meanMs returns sum/count, or nil when count is 0 (never a fabricated 0ms).
func meanMs(sum float64, count int64) *float64 {
	if count <= 0 {
		return nil
	}
	v := round6(sum / float64(count))
	return &v
}

// estimateCompressorTimeSaved fills j's TimeSavedSecondsEst/MoneySavedEst from
// p as a SUM over models (2026-08-06 rewrite — see the package doc). Compressor
// gives per-model REQUEST counts, not per-model token counts, so each
// model's cached-token share is apportioned by its share of this proxy's
// requests. Summing (that model's apportioned cached tokens ÷ that model's
// OWN real prefill TPS) per model is the correct math; blending every
// model's TPS into one average and dividing the whole window's cached
// tokens by it once is not — the whole reason a per-window aggregate
// dominant-model figure produced a fabricated-looking result before.
//
// Money uses the same wall-power cost model as /api/v1/cost/summary, so the
// two "what did this cost/save me" figures agree on their unit economics.
// estimateCompressorTimeSaved returns (conversion, missing) — whether a
// non-trivial FX conversion was needed for MoneySavedDisplay, and whether the
// rate for it was missing (caller aggregates these into the response-level
// fx_stale). Both are false on every early return, since no money was
// computed yet in those cases.
func (s *Server) estimateCompressorTimeSaved(ctx context.Context, j *compressorSummaryProxyJSON, p store.CompressorProxySummary, display string) (conversion, missing bool) {
	if p.Requests <= 0 || p.RequestsCached <= 0 {
		return false, false // nothing cached this window — leave the estimate fields absent
	}
	avgTokensPerRequest := float64(p.TokensIn) / float64(p.Requests)
	cachedTokens := float64(p.RequestsCached) * avgTokensPerRequest
	j.TokensSavedEst = &cachedTokens

	var totalRequests int64
	for _, n := range p.RequestsByModel {
		totalRequests += n
	}
	if totalRequests <= 0 {
		return false, false // no per-model breakdown at all — can't apportion or look anything up
	}

	var timeSaved float64
	var breakdown []compressorPrefillModelJSON
	var best compressorPrefillModelJSON
	var bestShare float64
	for rawMode, reqCount := range p.RequestsByModel {
		// Compressor's compress_requests_by_model metric labels local requests
		// by the raw GGUF weight path llama-server was launched with, not
		// the catalog mode/config name every other lookup here is keyed by
		// (found live 2026-08-06: tps_mode was reporting
		// "/opt/forge/models/Qwen3.6-35B-A3B-MTP-UD-Q4_K_XL.gguf" instead of
		// "qwen36-mtp"). Resolve it back to a mode name first.
		mode := s.resolveModePathAlias(rawMode)
		share := float64(reqCount) / float64(totalRequests)
		modelCachedTokens := cachedTokens * share

		tps, source, ok := s.lookupPrefillTPS(ctx, mode, avgTokensPerRequest)
		if !ok {
			// Anomaly, not a routine gap (package doc): a model contributing
			// cached requests to this window necessarily ran, so its prefill
			// counters were scraped. Landing here means something upstream
			// is actually broken — log it and omit the contribution rather
			// than guess.
			log.Printf("compressor summary: no real prefill TPS for mode %q (raw %q, proxy %q) despite %d cached-eligible requests this window — omitted from local time-saved",
				mode, rawMode, j.Proxy, reqCount)
			continue
		}
		timeSaved += modelCachedTokens / tps
		breakdown = append(breakdown, compressorPrefillModelJSON{Mode: mode, Share: round6(share), TPS: round6(tps), Source: source})
		if share > bestShare {
			best, bestShare = breakdown[len(breakdown)-1], share
		}
	}
	if timeSaved <= 0 {
		return false, false // every contributing model was an anomaly — nothing real to report
	}
	sort.Slice(breakdown, func(i, k int) bool { return breakdown[i].Share > breakdown[k].Share })
	j.PrefillBreakdown = breakdown
	j.TimeSavedSecondsEst = &timeSaved
	j.TPSSource = best.Source
	j.TPSMode = best.Mode

	cost := s.resolvedCost()
	wallKW := cost.WallWatts(cost.PowerKW*1000) / 1000
	moneyNative := timeSaved / 3600 * wallKW * cost.RatePerKWh
	j.MoneySavedEst = &moneyNative
	j.MoneySavedCurrency = cost.RateCurrency

	converted, missing := s.convert(ctx, moneyNative, cost.RateCurrency, display)
	d := round6(converted)
	j.MoneySavedDisplay = &d
	return display != cost.RateCurrency, missing
}

// resolveModePathAlias maps a raw model weight path back to the catalog mode
// (config) name that serves it, by resolving every mode's configured
// Service.Model against the same Paths.ResolveModelPath rule the engine
// itself uses and matching on the result. Falls back to the raw value
// unchanged when no mode matches — e.g. a model since removed from the
// catalog but still present in recent usage — so callers degrade to the old
// (broken) behavior rather than panic.
func (s *Server) resolveModePathAlias(raw string) string {
	cfg := s.deps.Config()
	if cfg == nil {
		return raw
	}
	for name, mode := range cfg.Modes {
		for _, svc := range mode.Services {
			if svc.Model != "" && cfg.Paths.ResolveModelPath(svc.Model) == raw {
				return name
			}
		}
	}
	return raw
}

// lookupPrefillTPS resolves a REAL prefill tok/s figure for mode, preferring
// the most precise/authoritative measurement available. depthTarget is the
// KV-cache depth (in tokens) to interpolate the profile depth curve at —
// callers pass this window's own average tokens/request, since cache-hit
// tokens are by definition deep-context and a shallower point would
// overstate throughput. Never substitutes decode_tps for prefill_tps — on
// this hardware the two aren't even reliably ordered (a real profiled
// example has prefill SLOWER than decode) — and, as of the 2026-08-06
// local-savings prefill sprint, there is deliberately no flat-rate
// fallback. ok=false + source="unavailable" means genuinely nothing is
// known for mode; callers must treat that as zero contribution, never guess
// (see the package doc).
func (s *Server) lookupPrefillTPS(ctx context.Context, mode string, depthTarget float64) (float64, string, bool) {
	prof, hasProfile := s.freshProfile(mode)

	// Step 1: nearest-to-depthTarget point on the real measured depth curve.
	if hasProfile && len(prof.DepthBenchmarks) > 0 && depthTarget > 0 {
		best := prof.DepthBenchmarks[0]
		bestDist := absFloat(float64(best.DepthTokens) - depthTarget)
		for _, db := range prof.DepthBenchmarks[1:] {
			if d := absFloat(float64(db.DepthTokens) - depthTarget); d < bestDist {
				best, bestDist = db, d
			}
		}
		if best.PP2048TPS > 0 {
			return best.PP2048TPS, "profile_depth_curve", true
		}
	}

	// Step 2: passively observed real prefill throughput, accumulated from
	// ordinary traffic (internal/collector's OnPrefillSample →
	// store.PrefillStats) rather than a destructive PROFILE run. Ranked
	// above the depth-0 scalar (step 3) because it reflects this mode's
	// ACTUAL request-depth mix over time, not one synthetic sample point;
	// gated behind a minimum sample count so a couple of noisy early
	// observations can't outrank real profiled data.
	if s.deps.PrefillStats != nil {
		if stats, err := s.deps.PrefillStats.ByMode(ctx); err == nil {
			if stat, ok := stats[mode]; ok && stat.Samples >= prefillObservedMinSamples {
				if tps := stat.TPS(); tps > 0 {
					return tps, "observed", true
				}
			}
		}
	}

	// Step 3: the depth-0 scalar (overstates for deep prompts, hence ranked
	// below both the depth curve and real passive observation).
	if hasProfile && prof.PrefillTPS > 0 {
		return prof.PrefillTPS, "profile_scalar", true
	}

	// Step 4: curated catalog benchmark, variant-scoped only (config-scoped
	// rows exist but aren't card-visible data — see
	// reference_catalog_benchmark_subject_type_trap). Never falls back to
	// decode_tps if only that metric exists.
	if s.deps.Catalog != nil {
		if cfg, err := s.deps.Catalog.ConfigByName(ctx, mode); err == nil && cfg.VariantID != 0 {
			if benches, err := s.deps.Catalog.ListBenchmarksForSubject(ctx, "variant", cfg.VariantID); err == nil {
				for _, b := range benches {
					if b.Metric != "prefill_tps" {
						continue
					}
					if v, err := strconv.ParseFloat(b.Value, 64); err == nil && v > 0 {
						return v, "catalog", true
					}
				}
			}
		}
	}

	// Step 5: live scrape of the currently-loaded slot running this mode —
	// valid only as a "right now" figure, but better than nothing when no
	// profile/observed/catalog data exists at all.
	if s.deps.Llama != nil {
		if snap := s.snapshot(); snap != nil {
			for _, slot := range snap.Slots {
				if slot.Mode != mode || slot.Port == 0 {
					continue
				}
				if m := s.deps.Llama.Metrics(ctx, slot.Port); m != nil && m.PromptTPS > 0 {
					return m.PromptTPS, "live", true
				}
			}
		}
	}

	return 0, "unavailable", false
}

// freshProfile returns mode's profile result if one exists and isn't stale,
// fetched once so lookupPrefillTPS's depth-curve and depth-0-scalar steps
// don't each pay a separate profileRunner.Get call.
func (s *Server) freshProfile(mode string) (profile.Result, bool) {
	runner, ok := s.profileRunner()
	if !ok {
		return profile.Result{}, false
	}
	result, ok := runner.Get(mode)
	if !ok || result.Stale {
		return profile.Result{}, false
	}
	return result, true
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// remoteOfferingContext holds one window's usage_events plus the offering
// price list, shared by estimateRemoteCacheDiscountSaved and
// estimateRemoteCompressionSaved so both read Events/ListOfferings once per
// request instead of once per remote proxy.
type remoteOfferingContext struct {
	events     []store.UsageEvent
	priceByKey map[string]store.Offering // keyed "<provider>/<wire_model>"
}

func (s *Server) loadRemoteOfferingContext(ctx context.Context, since time.Time) (*remoteOfferingContext, bool) {
	if s.deps.Usage == nil || s.deps.Catalog == nil {
		return nil, false
	}
	events, err := s.deps.Usage.Events(ctx, since, 1_000_000)
	if err != nil {
		return nil, false
	}
	offerings, err := s.deps.Catalog.ListOfferings(ctx)
	if err != nil {
		return nil, false
	}
	priceByKey := map[string]store.Offering{}
	for _, o := range offerings {
		priceByKey[o.ProviderName+"/"+o.WireModel] = o
	}
	return &remoteOfferingContext{events: events, priceByKey: priceByKey}, true
}

// estimateRemoteCacheDiscountSaved fills j's CacheDiscountSaved* fields for
// a remote proxy from real (not estimated) data: usage_events' per-request
// CachedPromptTokens, which the router only ever populates from the
// provider's own usage response (recordExternalUsage), priced against the
// matching offering's PriceInPer1M/PriceCachedInPer1M. This prices the
// PROVIDER's own prompt-cache discount — it applies whether or not Compressor
// is in the request path at all, so it is not a Compressor saving; kept as a
// separate, still-real "provider cache" figure alongside
// estimateRemoteCompressionSaved below, which is the actual Compressor saving.
// estimateRemoteCacheDiscountSaved returns (conversion, missing) — see
// estimateCompressorTimeSaved's doc comment for the contract.
func (s *Server) estimateRemoteCacheDiscountSaved(ctx context.Context, j *compressorSummaryProxyJSON, roc *remoteOfferingContext, provider, display string) (conversion, missing bool) {
	var saved float64
	var allCachedTokens, pricedTokens int64
	var currency string
	for _, ev := range roc.events {
		if ev.Kind != "external_request" || ev.ProviderName != provider || ev.CachedPromptTokens == nil {
			continue
		}
		allCachedTokens += *ev.CachedPromptTokens
		o, ok := roc.priceByKey[ev.ProviderName+"/"+ev.Model]
		if !ok || o.PriceCachedInPer1M == nil {
			continue // provider/model has no modelled cache-hit discount — token still counted above
		}
		saved += float64(*ev.CachedPromptTokens) / 1e6 * (o.PriceInPer1M - *o.PriceCachedInPer1M)
		pricedTokens += *ev.CachedPromptTokens
		currency = o.Currency
	}
	j.CachedPromptTokens = allCachedTokens
	if pricedTokens == 0 {
		return false, false // nothing priced this window — leave the discount/currency fields absent
	}
	j.CacheDiscountSavedNative = &saved
	j.CacheDiscountSavedCurrency = currency
	j.CacheDiscountTokens = pricedTokens

	converted, missing := s.convert(ctx, saved, currency, display)
	d := round6(converted)
	j.CacheDiscountSavedDisplay = &d
	return display != currency, missing
}

// estimateRemoteCompressionSaved fills j's CompressionSaved* fields: the
// TokensSaved already on j (from the proxy's own compress_tokens_saved_total
// counter — real, not estimated, the number of input tokens Compressor's
// compression removed from the prompt before it reached the provider),
// priced at this window's own token-weighted blended input rate for
// provider. Unlike the cache-discount figure above, this needs no
// per-request cache breakdown — every enabled offering already carries
// PriceInPer1M, so this is computable with zero missing-data cases. If the
// provider fronts models billed in different currencies this window, the
// figure is left absent rather than blended across currencies.
// estimateRemoteCompressionSaved returns (conversion, missing) — see
// estimateCompressorTimeSaved's doc comment for the contract.
func (s *Server) estimateRemoteCompressionSaved(ctx context.Context, j *compressorSummaryProxyJSON, roc *remoteOfferingContext, provider, display string) (conversion, missing bool) {
	if j.TokensSaved <= 0 {
		return false, false // nothing compressed away this window
	}
	var tokenWeightedRate float64
	var totalTokens int64
	var currency string
	mixedCurrency := false
	for _, ev := range roc.events {
		if ev.Kind != "external_request" || ev.ProviderName != provider || ev.PromptTokens <= 0 {
			continue
		}
		o, ok := roc.priceByKey[ev.ProviderName+"/"+ev.Model]
		if !ok {
			continue
		}
		if currency == "" {
			currency = o.Currency
		} else if currency != o.Currency {
			mixedCurrency = true
		}
		tokenWeightedRate += float64(ev.PromptTokens) * o.PriceInPer1M
		totalTokens += ev.PromptTokens
	}
	if totalTokens == 0 || mixedCurrency {
		return false, false
	}
	rate := tokenWeightedRate / float64(totalTokens)
	saved := float64(j.TokensSaved) / 1e6 * rate
	j.CompressionSavedNative = &saved
	j.CompressionSavedCurrency = currency
	j.CompressionRatePer1M = &rate

	converted, missing := s.convert(ctx, saved, currency, display)
	d := round6(converted)
	j.CompressionSavedDisplay = &d
	return display != currency, missing
}
