// SPDX-License-Identifier: Apache-2.0

package collector

// CompressorSample is one interval's per-proxy Compressor counter deltas plus
// the most recent lifetime min/max gauges (cost/savings sprint, 2026-07-30).
// Deliberately not part of the frozen Snapshot (Contract 2, snapshot.go) —
// Compressor is an external process with its own evolving metric set, not the
// collector's own measured state.
//
// Provider cache metrics (CacheReadTokensDelta, UncachedTokensDelta, etc.)
// were confirmed live against the aiand proxy on headroom-ai 0.30.0
// (2026-08-14) — the metrics were always available but were not scraped
// until 0.35.0. The prior "no per-provider cache-hit token count" claim
// in this doc comment was wrong: compress_cache_read_tokens_total{provider},
// compress_uncached_input_tokens_total{provider}, and the provider cache
// request/hit/bust family have been present since at least 0.30.0. They
// are lazily registered (only appear in /metrics after the first request
// that triggers them), which is why a freshly-restarted proxy's metrics
// dump appeared to omit them.
type CompressorSample struct {
	TokensInDelta  int64
	TokensOutDelta int64
	// TokensSavedDelta is Compressor's own compression-savings counter.
	// Usually 0 on this host — headroom@* run --lossless, so semantic/lossy
	// compression is off — but it is NOT pinned to 0: a real, DB-persisted
	// 950-token delta was recorded on the deepseek proxy 2026-07-31
	// (11241 tokens in, 1 request), so --lossless evidently still allows
	// some lossless-safe reduction (e.g. dedup/whitespace) to register
	// here. Priced in httpapi's compressor summary handler
	// (estimateRemoteCompressionSaved) as a real Compressor saving.
	TokensSavedDelta int64

	RequestsDelta            int64
	RequestsCachedDelta      int64
	RequestsFailedDelta      int64
	RequestsRateLimitedDelta int64
	// RequestsTimeoutDelta / RequestsCanceledDelta (Sprint 4) are a subset
	// of RequestsFailedDelta's causes, broken out separately — see
	// store.CompressorSavingsSampleRow's doc comment.
	RequestsTimeoutDelta  int64
	RequestsCanceledDelta int64

	// FailOpenDelta is the total fail-open events (timeout + error) across
	// the interval — the sum of compress_failopen_total{reason} label
	// values. Used by the smith fail-open rate check to detect when the
	// budget is too low.
	FailOpenDelta int64

	TTFBCountDelta      int64
	TTFBSumMsDelta      float64
	TTFBMinMsSinceStart *float64
	TTFBMaxMsSinceStart *float64

	LatencyCountDelta      int64
	LatencySumMsDelta      float64
	LatencyMinMsSinceStart *float64
	LatencyMaxMsSinceStart *float64

	OverheadCountDelta      int64
	OverheadSumMsDelta      float64
	OverheadMinMsSinceStart *float64
	OverheadMaxMsSinceStart *float64

	// RequestsByProviderDelta / RequestsByModelDelta are request COUNTS per
	// label value, not token counts — Compressor's compressor_requests_by_{
	// provider,model} metrics carry no token dimension.
	RequestsByProviderDelta map[string]int64
	RequestsByModelDelta    map[string]int64

	// Provider cache metrics (scraped from compress_cache_read_tokens_total,
	// compress_uncached_input_tokens_total, etc. — labelled by provider).
	// These resolve the old "no per-provider cache-hit token count" gap.
	CacheReadTokensDelta          map[string]int64
	CacheWriteTokensDelta         map[string]int64
	UncachedTokensDelta           map[string]int64
	ProviderCacheRequestsDelta    map[string]int64
	ProviderCacheHitRequestsDelta map[string]int64

	// Cache bust metrics (scalar — not per-provider).
	CacheBustsDelta          int64
	CacheBustTokensLostDelta int64

	// Per-compressor timing (labelled by transform name — e.g.
	// "content_router", "compressor:text", "compressor:kompress").
	// Sum/Count are counter deltas; Max is a lifetime gauge (latest value,
	// not a delta — same pattern as TTFBMaxMsSinceStart).
	TransformTimingSumDelta      map[string]float64
	TransformTimingCountDelta    map[string]int64
	TransformTimingMaxSinceStart map[string]float64
}

// AllZero reports whether every counter delta is zero — the interval-skip
// condition for an idle proxy. Since-start min/max can only move when their
// paired count moves, so checking the three *Count deltas alongside the
// token/request deltas is exhaustive.
func (s CompressorSample) AllZero() bool {
	return s.TokensInDelta == 0 && s.TokensOutDelta == 0 && s.TokensSavedDelta == 0 &&
		s.RequestsDelta == 0 && s.RequestsCachedDelta == 0 && s.RequestsFailedDelta == 0 &&
		s.RequestsRateLimitedDelta == 0 && s.TTFBCountDelta == 0 && s.LatencyCountDelta == 0 &&
		s.OverheadCountDelta == 0 && s.CacheBustsDelta == 0 && s.CacheBustTokensLostDelta == 0 &&
		// A canceled request never bumps RequestsFailedDelta (Sprint 4,
		// cmd/forge-compress/server.go) — without these two, an interval
		// with only cancellations would misread as idle and get skipped.
		s.RequestsTimeoutDelta == 0 && s.RequestsCanceledDelta == 0 &&
		// Fail-opens with zero other traffic would also look idle; include
		// so an all-fail-open interval is not skipped.
		s.FailOpenDelta == 0
}
