// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parsePromScalar returns the first scalar value for a metric name in
// Prometheus text output (port of monitor._parse_prom_scalar).
func parsePromScalar(text, name string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			v, err := strconv.ParseFloat(f[len(f)-1], 64)
			if err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

// parsePromFirst returns the first present scalar among names — for metrics
// whose name differs across llama.cpp builds (predicted_tokens_total vs
// tokens_predicted_total).
func parsePromFirst(text string, names ...string) (float64, bool) {
	for _, n := range names {
		if v, ok := parsePromScalar(text, n); ok {
			return v, true
		}
	}
	return 0, false
}

// parsePromSum returns the sum of every series for a metric name (labelled
// or not) in Prometheus text output, plus how many series contributed.
// Unlike parsePromScalar (which returns only the first matching line), this
// is required for metrics Compressor exposes per-label (e.g. one
// {provider=...} line per provider) where the dashboard wants the total
// across every label combination, not an arbitrary one of them.
func parsePromSum(text, name string) (sum float64, series int, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, name+" ") && !strings.HasPrefix(line, name+"{") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			continue
		}
		sum += v
		series++
	}
	return sum, series, series > 0
}

// parsePromByLabel returns, for every series of a labelled metric, the
// value of one requested label keyed to that series' value (e.g.
// parsePromByLabel(text, "compress_requests_by_model", "model") maps a
// model path to its request count). A series missing the requested label,
// or with a malformed label block, is skipped rather than mis-attributed —
// this is a narrow reader for Compressor's own metric set, not a general
// Prometheus text-format parser (it assumes label values contain no commas,
// true for the provider/model names Compressor emits).
func parsePromByLabel(text, name, label string) map[string]float64 {
	out := map[string]float64{}
	prefix := name + "{"
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, prefix) {
			continue
		}
		open := strings.IndexByte(line, '{')
		closeIdx := strings.IndexByte(line, '}')
		if open < 0 || closeIdx < 0 || closeIdx < open {
			continue
		}
		labelValue, ok := promLabelValue(line[open+1:closeIdx], label)
		if !ok {
			continue
		}
		rest := strings.Fields(line[closeIdx+1:])
		if len(rest) < 1 {
			continue
		}
		v, err := strconv.ParseFloat(rest[len(rest)-1], 64)
		if err != nil {
			continue
		}
		out[labelValue] += v
	}
	return out
}

// promLabelValue extracts one label's value from a Prometheus label block
// (the text between "{" and "}", e.g. `provider="openai",ttl="1h"`).
func promLabelValue(block, label string) (string, bool) {
	for _, kv := range strings.Split(block, ",") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.TrimSpace(parts[0]) != label {
			continue
		}
		return strings.Trim(strings.TrimSpace(parts[1]), `"`), true
	}
	return "", false
}

// LlamaMetrics is one llama-server /metrics scrape. Totals are nil on older
// builds without cumulative counters (usage tracking skips those slots).
type LlamaMetrics struct {
	RequestsProcessing float64
	PromptTPS          float64
	PredictedTPS       float64
	PromptTotal        *float64
	PredictedTotal     *float64
	// NDecodeTotal is llamacpp:n_decode_total — cumulative llama_decode()
	// calls. Unlike PromptTPS/PredictedTPS and the token totals (which
	// llama.cpp only flushes at slot reset since upstream #26920 — see
	// hang.go), this counter advances after every decode batch *during*
	// generation, so a delta across scrapes proves the GPU is still
	// decoding even when the gauges read 0. That is the discriminator
	// between a healthy long generation and a genuine KFD-eviction stall.
	// Nil on builds that don't export it.
	NDecodeTotal *float64
	// NTokensMax is llamacpp:n_tokens_max — largest observed sequence
	// length (prompt + generation). Also updated live per decode batch
	// (monotonic while a sequence grows), a secondary live-progress
	// signal.
	NTokensMax *float64
	// PromptSecondsTotal is llamacpp:prompt_seconds_total — cumulative real
	// prefill processing time in seconds. Paired with PromptTotal, a delta
	// of each across two scrapes gives a true measured average prefill
	// tok/s over that interval (unlike PromptTPS above, which is only the
	// most recent single request's rate — see
	// internal/httpapi/compressor_summary_handlers.go's prefill-collection
	// doc comment for why the distinction matters).
	PromptSecondsTotal *float64
}

// LlamaClient probes llama-server endpoints. BaseURL maps a port to a URL
// prefix; tests point it at httptest servers. 127.0.0.1 (not "localhost") is
// deliberate — localhost can resolve to ::1 first and stall on v4-only
// listeners (see the ForgeHost live-verification memory note).
type LlamaClient struct {
	client  *http.Client
	baseURL func(port int) string
}

func NewLlamaClient(baseURL func(port int) string) *LlamaClient {
	if baseURL == nil {
		baseURL = func(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }
	}
	return &LlamaClient{
		client:  &http.Client{Timeout: 3 * time.Second},
		baseURL: baseURL,
	}
}

func (l *LlamaClient) get(ctx context.Context, port int, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL(port)+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// Metrics scrapes /metrics. Nil result means unreachable (still starting, or
// gone) — the V4 "polled but unreachable" state.
func (l *LlamaClient) Metrics(ctx context.Context, port int) *LlamaMetrics {
	raw, err := l.get(ctx, port, "/metrics")
	if err != nil {
		return nil
	}
	text := string(raw)
	m := &LlamaMetrics{}
	m.RequestsProcessing, _ = parsePromScalar(text, "llamacpp:requests_processing")
	m.PromptTPS, _ = parsePromScalar(text, "llamacpp:prompt_tokens_seconds")
	m.PredictedTPS, _ = parsePromScalar(text, "llamacpp:predicted_tokens_seconds")
	if v, ok := parsePromScalar(text, "llamacpp:prompt_tokens_total"); ok {
		m.PromptTotal = &v
	}
	if v, ok := parsePromFirst(text, "llamacpp:predicted_tokens_total", "llamacpp:tokens_predicted_total"); ok {
		m.PredictedTotal = &v
	}
	if v, ok := parsePromScalar(text, "llamacpp:prompt_seconds_total"); ok {
		m.PromptSecondsTotal = &v
	}
	if v, ok := parsePromScalar(text, "llamacpp:n_decode_total"); ok {
		m.NDecodeTotal = &v
	}
	if v, ok := parsePromScalar(text, "llamacpp:n_tokens_max"); ok {
		m.NTokensMax = &v
	}
	return m
}

// NCtx queries /props for the actual context size. llama.cpp moved n_ctx
// into default_generation_settings in newer builds; both locations are
// checked (port of engine._verify_model_context's read).
func (l *LlamaClient) NCtx(ctx context.Context, port int) (int, error) {
	raw, err := l.get(ctx, port, "/props")
	if err != nil {
		return 0, err
	}
	var props struct {
		NCtx                      int `json:"n_ctx"`
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(raw, &props); err != nil {
		return 0, err
	}
	if props.NCtx > 0 {
		return props.NCtx, nil
	}
	return props.DefaultGenerationSettings.NCtx, nil
}

// Healthy queries /health. llama.cpp returns {"status":"ok"}; vLLM returns
// an empty body (any 200 = ready).
func (l *LlamaClient) Healthy(ctx context.Context, port int) bool {
	raw, err := l.get(ctx, port, "/health")
	if err != nil {
		return false
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Status == "" {
		return true // empty/non-JSON 200 counts as healthy (vLLM)
	}
	return body.Status == "ok"
}

// compressorCounters is one raw scrape of a Compressor proxy's own volatile,
// per-process /metrics counters — NOT the headroom_persistent_savings_*
// family, which is contaminated by a shared state file
// (~/.compressor/proxy_savings.json) across every headroom@ instance on this
// host (confirmed live 2026-07-29/30; see docs/v5-headroom-topology.md).
// The volatile counters reset to 0 on proxy restart, which delta() (run.go)
// already handles as a fresh baseline rather than a negative delta.
//
// Provider cache metrics (CacheReadTokens, UncachedTokens, etc.) were
// confirmed live on the aiand proxy (headroom-ai 0.30.0, 2026-08-14).
// They are lazily registered — only appearing in /metrics after the first
// request that triggers them — so a freshly-restarted proxy may not emit
// them until traffic flows.
type compressorCounters struct {
	TokensIn, TokensOut, TokensSaved                              float64
	Requests, RequestsCached, RequestsFailed, RequestsRateLimited float64
	// RequestsTimeout / RequestsCanceled (Sprint 4, forge-compress only —
	// absent/0 on legacy headroom-ai proxies, an honest "no data" via
	// parsePromScalar's ok=false being ignored below, same as every other
	// optional counter in this struct).
	RequestsTimeout, RequestsCanceled                    float64
	TTFBCount, TTFBSum, TTFBMin, TTFBMax                 float64
	LatencyCount, LatencySum, LatencyMin, LatencyMax     float64
	OverheadCount, OverheadSum, OverheadMin, OverheadMax float64
	// RequestsByProvider / RequestsByModel are request COUNTS keyed by
	// label value (compressor_requests_by_{provider,model}) — no token
	// dimension is exposed per label.
	RequestsByProvider map[string]float64
	RequestsByModel    map[string]float64
	// Provider cache metrics — compress_cache_read_tokens_total{provider},
	// compress_uncached_input_tokens_total{provider}, etc. Available since
	// at least 0.30.0 but lazily registered (only appear after first
	// request that triggers them).
	CacheReadTokens          map[string]float64
	CacheWriteTokens         map[string]float64
	UncachedTokens           map[string]float64
	ProviderCacheRequests    map[string]float64
	ProviderCacheHitRequests map[string]float64
	// Scalar cache-bust counters (no labels).
	CacheBusts          float64
	CacheBustTokensLost float64
	// Per-compressor timing (labelled by transform name).
	TransformTimingSum   map[string]float64
	TransformTimingCount map[string]float64
	TransformTimingMax   map[string]float64
}

// scrapeCompressorCounters reads a Compressor proxy's volatile /metrics
// counters. ok=false when the proxy is unreachable, or up but exposes none
// of these counters (an old/incompatible build) — the caller skips that
// service for this cycle rather than recording a bogus all-zero baseline.
func (l *LlamaClient) scrapeCompressorCounters(ctx context.Context, port int) (*compressorCounters, bool) {
	raw, err := l.get(ctx, port, "/metrics")
	if err != nil {
		return nil, false
	}
	text := string(raw)
	tokensIn, ok := parsePromScalar(text, "compress_tokens_input_total")
	if !ok {
		return nil, false
	}
	c := &compressorCounters{TokensIn: tokensIn}
	c.TokensOut, _ = parsePromScalar(text, "compress_tokens_output_total")
	c.TokensSaved, _ = parsePromScalar(text, "compress_tokens_saved_total")
	c.Requests, _ = parsePromScalar(text, "compress_requests_total")
	c.RequestsCached, _ = parsePromScalar(text, "compress_requests_cached_total")
	c.RequestsFailed, _ = parsePromScalar(text, "compress_requests_failed_total")
	c.RequestsRateLimited, _ = parsePromScalar(text, "compress_requests_rate_limited_total")
	c.RequestsTimeout, _ = parsePromScalar(text, "compress_requests_timeout_total")
	c.RequestsCanceled, _ = parsePromScalar(text, "compress_requests_canceled_total")
	c.TTFBCount, _ = parsePromScalar(text, "compress_ttfb_ms_count")
	c.TTFBSum, _ = parsePromScalar(text, "compress_ttfb_ms_sum")
	c.TTFBMin, _ = parsePromScalar(text, "compress_ttfb_ms_min")
	c.TTFBMax, _ = parsePromScalar(text, "compress_ttfb_ms_max")
	c.LatencyCount, _ = parsePromScalar(text, "compress_latency_ms_count")
	c.LatencySum, _ = parsePromScalar(text, "compress_latency_ms_sum")
	c.LatencyMin, _ = parsePromScalar(text, "compress_latency_ms_min")
	c.LatencyMax, _ = parsePromScalar(text, "compress_latency_ms_max")
	c.OverheadCount, _ = parsePromScalar(text, "compress_overhead_ms_count")
	c.OverheadSum, _ = parsePromScalar(text, "compress_overhead_ms_sum")
	c.OverheadMin, _ = parsePromScalar(text, "compress_overhead_ms_min")
	c.OverheadMax, _ = parsePromScalar(text, "compress_overhead_ms_max")
	c.RequestsByProvider = parsePromByLabel(text, "compress_requests_by_provider", "provider")
	c.RequestsByModel = parsePromByLabel(text, "compress_requests_by_model", "model")
	c.CacheReadTokens = parsePromByLabel(text, "compress_cache_read_tokens_total", "provider")
	c.CacheWriteTokens = parsePromByLabel(text, "compress_cache_write_tokens_total", "provider")
	c.UncachedTokens = parsePromByLabel(text, "compress_uncached_input_tokens_total", "provider")
	c.ProviderCacheRequests = parsePromByLabel(text, "compress_provider_cache_requests_total", "provider")
	c.ProviderCacheHitRequests = parsePromByLabel(text, "compress_provider_cache_hit_requests_total", "provider")
	c.CacheBusts, _ = parsePromScalar(text, "compress_cache_bust_total")
	c.CacheBustTokensLost, _ = parsePromScalar(text, "compress_cache_bust_tokens_lost_total")
	c.TransformTimingSum = parsePromByLabel(text, "compress_transform_timing_ms_sum", "transform")
	c.TransformTimingCount = parsePromByLabel(text, "compress_transform_timing_ms_count", "transform")
	c.TransformTimingMax = parsePromByLabel(text, "compress_transform_timing_ms_max", "transform")
	return c, true
}
