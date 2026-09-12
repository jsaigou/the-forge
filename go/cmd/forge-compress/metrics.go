// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/jsaigou/the-forge/internal/statutil"
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
	// overheadRing backs compress_overhead_ms_p50/p90/p99 — see sampleRing's
	// doc comment for why overhead specifically gets a percentile view and
	// ttfb/latency don't (yet): overhead is the one number this session's
	// investigation found was actively misleading as a mean (dominated by a
	// small share of huge messages, hiding that most real traffic barely
	// pays the tax).
	overheadRing *sampleRing

	failOpenTimeout counter
	failOpenError   counter

	requestsByProvider labelCounter
	requestsByModel    labelCounter
	// messagesByOutcomeSize is keyed by a composite "outcome:size_tier"
	// label value (e.g. "compressed:huge") rather than two independent
	// label dimensions — this repo's label-sample storage
	// (internal/store's compressor_label_samples) is a flat
	// (label_key, label_value, metric) shape with one dimension per row, so
	// a composite value is how a second dimension rides along without a
	// schema change. outcome ∈ {compressed, gated_passthrough,
	// alldrop_passthrough, failopen_timeout, failopen_error}; size_tier ∈
	// {small, medium, large, huge} — see messageOutcomeSize in messages.go.
	// Added 2026-09-11 to answer whether compression's real value is
	// concentrated in a few huge messages (the operator's own early-testing
	// finding) or spread evenly — something the prior mean-only metrics
	// couldn't show.
	messagesByOutcomeSize labelCounter
}

func newMetrics() *metrics {
	return &metrics{
		overheadRing:          newSampleRing(overheadRingCapacity),
		requestsByProvider:    newLabelCounter(),
		requestsByModel:       newLabelCounter(),
		messagesByOutcomeSize: newLabelCounter(),
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
	writePercentiles(write, "compress_overhead_ms", m.overheadRing)

	writeLabelCounter(write, "compress_requests_by_provider", "provider", &m.requestsByProvider)
	writeLabelCounter(write, "compress_requests_by_model", "model", &m.requestsByModel)
	writeLabelCounter(write, "compress_messages_total", "outcome_size", &m.messagesByOutcomeSize)

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

const (
	// overheadRingCapacity mirrors the collector's existing 120-sample
	// sparkline-ring pattern (internal/collector/run.go's rings field) —
	// this process has no other precedent for bounding an otherwise
	// unbounded-lifetime sample set.
	overheadRingCapacity = 256
	// percentileMinSamples is this repo's established floor for trusting a
	// percentile computed from a raw sample set — see
	// internal/httpapi/cost_handlers.go's activeSingleSlotWallW gate and
	// compressor_summary_handlers.go's prefillObservedMinSamples, both
	// named "10" for the same reason: a couple of noisy early observations
	// shouldn't produce a misleadingly-precise-looking figure.
	percentileMinSamples = 10
)

// sampleRing is a fixed-capacity, thread-safe ring buffer of recent
// float64 samples. This binary's histogram accumulators are lifetime-since-
// process-start (never reset — see histogram's doc comment), so a plain
// growing []float64 isn't safe for a long-running process; a bounded ring
// gives "percentile of recent traffic" instead, which is what actually
// answers "is this request typical" during an incident.
type sampleRing struct {
	mu   sync.Mutex
	buf  []float64
	next int
	full bool
}

func newSampleRing(capacity int) *sampleRing {
	return &sampleRing{buf: make([]float64, capacity)}
}

func (r *sampleRing) add(v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = v
	r.next++
	if r.next == len(r.buf) {
		r.next = 0
		r.full = true
	}
}

// snapshot returns a copy of the samples currently held. Order doesn't
// matter — statutil.Percentile sorts its own copy.
func (r *sampleRing) snapshot() []float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		out := make([]float64, len(r.buf))
		copy(out, r.buf)
		return out
	}
	out := make([]float64, r.next)
	copy(out, r.buf[:r.next])
	return out
}

// writePercentiles emits p50/p90/p99 for r under the "name_pNN" series
// names, below percentileMinSamples samples emits nothing at all — a
// missing series is the honest signal, never a percentile computed from too
// few points to mean anything. Stored/read as a latest-snapshot gauge (like
// histogram's own min/max), not summed or averaged across a window — same
// invariant documented at internal/store/store.go's CompressorSavingsSampleRow.
func writePercentiles(write func(string, ...any), name string, r *sampleRing) {
	vals := r.snapshot()
	if len(vals) < percentileMinSamples {
		return
	}
	write("%s_p50 %g\n", name, statutil.Percentile(vals, 50))
	write("%s_p90 %g\n", name, statutil.Percentile(vals, 90))
	write("%s_p99 %g\n", name, statutil.Percentile(vals, 99))
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
