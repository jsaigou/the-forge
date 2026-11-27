// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Radiance series the neutral struct is filled from (core/server/metrics.cpp). The names are data about the engine, so they live in one place.
const (
	radSeriesRunning     = "vllm:num_requests_running"
	radSeriesWaiting     = "vllm:num_requests_waiting"
	radSeriesPreempted   = "radiance:num_requests_preempted"
	radSeriesHTTPQueue   = "radiance:http_queue_depth"
	radSeriesSteps       = "radiance:engine_steps_total"
	radSeriesPrefillTPS  = "radiance:prefill_tokens_per_second"
	radSeriesDecodeTPS   = "radiance:decode_tokens_per_second"
	radSeriesPrefillTok  = "radiance:prefill_tokens_total"
	radSeriesDecodeTok   = "radiance:decode_tokens_total"
	radSeriesKVMaxTokens = "radiance:kv_cache_max_tokens"
)

// parseRadianceSeries reads every scalar sample of the vllm: and radiance: families into a map. The model_name and engine labels are dropped (one
// server serves one model); any other label stays in the key as name{k="v",...} in file order, so a labelled family is kept per label value and
// never summed into a meaningless total. Histogram buckets are skipped (their _sum and _count lines are kept). A malformed line is skipped, not
// fatal: the reader must degrade, never panic.
func parseRadianceSeries(text string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		if !strings.HasPrefix(line, "vllm:") && !strings.HasPrefix(line, "radiance:") {
			continue
		}
		name, labels, rest, ok := splitPromLine(line)
		if !ok || strings.HasSuffix(name, "_bucket") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 1 {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		out[name+extraLabels(labels)] = v
	}
	return out
}

// splitPromLine splits `name{labels} value` (labels optional) into its parts.
func splitPromLine(line string) (name, labels, rest string, ok bool) {
	open := strings.IndexByte(line, '{')
	sp := strings.IndexByte(line, ' ')
	if sp < 0 {
		return "", "", "", false
	}
	if open < 0 || open > sp {
		return line[:sp], "", line[sp+1:], true
	}
	closeIdx := strings.LastIndexByte(line, '}')
	if closeIdx < open {
		return "", "", "", false
	}
	return line[:open], line[open+1 : closeIdx], line[closeIdx+1:], true
}

// extraLabels renders the labels other than model_name and engine as {k="v",...}, or "" when none remain.
func extraLabels(block string) string {
	if block == "" {
		return ""
	}
	var keep []string
	for _, kv := range strings.Split(block, ",") {
		k, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "model_name" || k == "engine" {
			continue
		}
		keep = append(keep, strings.TrimSpace(kv))
	}
	if len(keep) == 0 {
		return ""
	}
	return "{" + strings.Join(keep, ",") + "}"
}

// RadianceMetrics scrapes a Radiance server into the neutral struct. It reads vllm:* and radiance:* series only, never llamacpp:* (Radiance does not
// serve them). Nil means /metrics was unreachable and /health did not report a wedge either (still starting, or gone).
//
// A series that is absent stays absent: the pointer fields are nil and the gauges zero, so a missing series can neither fake progress nor, because
// the hang rule needs the progress counter to be present to apply, fake a hang.
func (l *LlamaClient) RadianceMetrics(ctx context.Context, port int) *LlamaMetrics {
	stuck, stuckS := l.radianceStuck(ctx, port)
	raw, err := l.get(ctx, port, "/metrics")
	if err != nil {
		if stuck {
			return &LlamaMetrics{Stuck: true, StuckSeconds: stuckS}
		}
		return nil
	}
	m := ParseRadianceMetrics(string(raw))
	m.Stuck, m.StuckSeconds = stuck, stuckS
	return m
}

// ParseRadianceMetrics fills the neutral struct from one /metrics text.
func ParseRadianceMetrics(text string) *LlamaMetrics {
	s := parseRadianceSeries(text)
	m := &LlamaMetrics{Series: s}
	m.RequestsProcessing = s[radSeriesRunning]
	m.RequestsQueued = s[radSeriesWaiting] + s[radSeriesPreempted] + s[radSeriesHTTPQueue]
	m.PromptTPS = s[radSeriesPrefillTPS]
	m.PredictedTPS = s[radSeriesDecodeTPS]
	ptr := func(name string) *float64 {
		if v, ok := s[name]; ok {
			return &v
		}
		return nil
	}
	// Token totals for usage tracking: the per-step counters, which advance during a request, not the vllm: ones charged at finish.
	m.PromptTotal = ptr(radSeriesPrefillTok)
	m.PredictedTotal = ptr(radSeriesDecodeTok)
	m.ProgressTotal = ptr(radSeriesSteps)
	m.ContextCapacity = ptr(radSeriesKVMaxTokens)
	return m
}

// radianceStuck asks /health whether the engine reports a wedge: HTTP 503 with status "stuck" (the device advanced nothing for 60 s,
// core/server/admin.cpp handle_ping). An unreachable /health is "not stuck": the absence of a signal is not a signal.
func (l *LlamaClient) radianceStuck(ctx context.Context, port int) (bool, int) {
	h, err := l.HealthStatus(ctx, port)
	if err != nil || h.Code != http.StatusServiceUnavailable {
		return false, 0
	}
	return h.Status == "stuck", h.StuckS
}

// HealthBody is what /health answered, whatever the status code: Radiance puts the reason ("stopping", "stuck") in the body of a 503.
type HealthBody struct {
	Code   int
	Status string // "" when the body is empty or not JSON; the code then stands alone
	StuckS int
}

// HealthStatus reads /health without turning a non-200 into an error, so a 503 body can be inspected. A transport or read failure is an error.
func (l *LlamaClient) HealthStatus(ctx context.Context, port int) (HealthBody, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL(port)+"/health", nil)
	if err != nil {
		return HealthBody{}, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return HealthBody{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return HealthBody{Code: resp.StatusCode}, err
	}
	h := HealthBody{Code: resp.StatusCode}
	var body struct {
		Status string  `json:"status"`
		StuckS float64 `json:"stuck_s"`
	}
	if json.Unmarshal(raw, &body) == nil { // a non-JSON body is a legitimate answer from other kinds; it just carries no status
		h.Status, h.StuckS = body.Status, int(body.StuckS)
	}
	return h, nil
}
