// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

// selfRSSBytes reads this process's resident set from /proc/self/statm
// (field 2, pages). ok=false on any read/parse failure — the gauge is
// best-effort and its absence must not break the exposition.
func selfRSSBytes() (int64, bool) {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	var totalPages, residentPages uint64
	if _, err := fmt.Sscanf(string(raw), "%d %d", &totalPages, &residentPages); err != nil {
		return 0, false
	}
	return int64(residentPages) * int64(os.Getpagesize()), true
}

// metrics reproduces the subset of headroom-ai's own /metrics Prometheus
// surface that this repo's downstream consumers actually read
// (internal/collector/llama.go's scrapeCompressorCounters —
// docs/v5-headroom-replacement.md Sprint 3's Architecture section lists the
// full name set this was ground-truthed against). Deliberately not the
// provider-cache-specific series (compress_cache_read_tokens_total,
// _uncached_input_tokens_total, _provider_cache_{requests,hit_requests}_total,
// _transform_timing_ms_*) — those need parsing each provider's own usage
// response fields (DeepSeek's prompt_cache_hit_tokens etc.), which is out of
// scope for this pass; scrapeCompressorCounters already treats an absent
// series as an honest empty map, so omitting them here is not a lie, just an
// acknowledged v1 gap.
//
// Volatile, per-process, in-memory — matches the existing convention this
// repo already migrated onto (collector reads each proxy's own counters,
// not a shared-file mechanism — see this repo's headroom_persistent_savings_*
// history for why that migration happened).
type metrics struct {
	tokensInput      counter
	tokensOutput     counter
	tokensSaved      counter
	requests         counter
	requestsCached   counter
	requestsFailed   counter
	requestsTimeout  counter
	requestsCanceled counter
	cacheBust        counter

	ttfb     histogram
	latency  histogram
	overhead histogram

	failOpenTimeout counter
	failOpenError   counter

	requestsByProvider labelCounter
	requestsByModel    labelCounter
}

func newMetrics() *metrics {
	return &metrics{
		requestsByProvider: newLabelCounter(),
		requestsByModel:    newLabelCounter(),
	}
}

// WriteTo renders the current snapshot as Prometheus text exposition —
// plain "name value" / `name{label="x"} value` lines, matching what
// parsePromScalar/parsePromByLabel (internal/collector/llama.go) parse; no
// HELP/TYPE metadata lines are required by that reader, so none are
// emitted.
func (m *metrics) WriteTo(w io.Writer) (int64, error) {
	var n int64
	write := func(format string, args ...any) {
		written, _ := fmt.Fprintf(w, format, args...)
		n += int64(written)
	}

	write("compress_tokens_input_total %d\n", m.tokensInput.load())
	write("compress_tokens_output_total %d\n", m.tokensOutput.load())
	write("compress_tokens_saved_total %d\n", m.tokensSaved.load())
	write("compress_requests_total %d\n", m.requests.load())
	write("compress_requests_cached_total %d\n", m.requestsCached.load())
	write("compress_requests_failed_total %d\n", m.requestsFailed.load())
	write("compress_requests_timeout_total %d\n", m.requestsTimeout.load())
	write("compress_requests_canceled_total %d\n", m.requestsCanceled.load())
	write("compress_failopen_total{reason=\"timeout\"} %d\n", m.failOpenTimeout.load())
	write("compress_failopen_total{reason=\"error\"} %d\n", m.failOpenError.load())
	write("compress_cache_bust_total %d\n", m.cacheBust.load())
	// S3 hardening: a live RSS gauge so the collector's compressor scrape
	// (and any human reading /metrics during an incident) sees this
	// process's own memory without reaching for systemctl show. The
	// external compressor_samples series exists but is sampled on the
	// dashboard's cadence; this is the proxy's own view.
	if rss, ok := selfRSSBytes(); ok {
		write("compress_rss_bytes %d\n", rss)
	}

	writeHistogram(write, "compress_ttfb_ms", &m.ttfb)
	writeHistogram(write, "compress_latency_ms", &m.latency)
	writeHistogram(write, "compress_overhead_ms", &m.overhead)

	writeLabelCounter(write, "compress_requests_by_provider", "provider", &m.requestsByProvider)
	writeLabelCounter(write, "compress_requests_by_model", "model", &m.requestsByModel)

	return n, nil
}

func writeHistogram(write func(string, ...any), name string, h *histogram) {
	count, sum, min, max := h.snapshot()
	write("%s_count %d\n", name, count)
	write("%s_sum %g\n", name, sum)
	write("%s_min %g\n", name, min)
	write("%s_max %g\n", name, max)
}

func writeLabelCounter(write func(string, ...any), name, label string, lc *labelCounter) {
	for _, kv := range lc.snapshot() {
		write("%s{%s=%q} %d\n", name, label, kv.key, kv.value)
	}
}

// counter is a simple mutex-guarded monotonic counter. Not atomic.Int64 —
// this binary's request volume never approaches contention that would
// matter, and a mutex keeps every metric type in this file uniform.
type counter struct {
	mu sync.Mutex
	v  int64
}

func (c *counter) add(delta int64) {
	c.mu.Lock()
	c.v += delta
	c.mu.Unlock()
}

func (c *counter) load() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.v
}

// histogram tracks count/sum/min/max — the four series every consumer of
// this proxy's timing metrics actually reads (no bucket boundaries are
// scraped anywhere downstream, so none are computed).
type histogram struct {
	mu      sync.Mutex
	count   int64
	sum     float64
	min     float64
	max     float64
	hasData bool
}

func (h *histogram) observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += v
	if !h.hasData || v < h.min {
		h.min = v
	}
	if !h.hasData || v > h.max {
		h.max = v
	}
	h.hasData = true
}

func (h *histogram) snapshot() (count int64, sum, min, max float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count, h.sum, h.min, h.max
}

// labelCounter is a set of independent counters keyed by one label value
// (e.g. provider name, model name).
type labelCounter struct {
	mu sync.Mutex
	m  map[string]int64
}

func newLabelCounter() labelCounter {
	return labelCounter{m: make(map[string]int64)}
}

func (lc *labelCounter) add(key string, delta int64) {
	if key == "" {
		return
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.m[key] += delta
}

type kv struct {
	key   string
	value int64
}

func (lc *labelCounter) snapshot() []kv {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	out := make([]kv, 0, len(lc.m))
	for k, v := range lc.m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}
