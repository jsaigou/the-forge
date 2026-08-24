// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"fmt"
	"time"
)

// hangDetector ports monitor._check_inference_hang: a port is hung only
// when requests_processing > 0 (there is active work) AND both prompt and
// predicted TPS are below minTPS, sustained for sustain. A switch-complete
// notification suppresses detection for cooldown (model load and initial KV
// allocation look exactly like a stall).
//
// Live-progress caveat (llama.cpp #26920 regression, 2026-08-13): since the
// upstream server-metrics refactor, the /metrics TPS gauges AND the
// cumulative token totals are flushed ONLY at slot reset — during a long
// single request (agentic task, huge context) they read 0.0 / stay frozen
// for the entire generation while the GPU decodes fine. That made the old
// gauge-only rule false-positive INFERENCE_HANG on every healthy long run
// (live-reproduced on qwen38-27b 2026-08-18: 75K-token generation, GPU 99%,
// counters pinned at 0). The live signal is llamacpp:n_decode_total /
// llamacpp:n_tokens_max, which llama-server updates after every decode
// batch DURING generation: an advancing decode counter means the GPU is
// crunching, so the port counts as flowing even with 0.0 tps gauges.
//
// V4 pushed a one-shot alert into a drain-on-read queue and reset the stall
// timer (re-firing every 90s while hung). V5 snapshots are level-triggered
// instead: Observe keeps reporting the alert while the stall persists and
// stops the moment work completes or tokens flow — same detection rule,
// steadier signal for SSE consumers.
type hangDetector struct {
	minTPS        float64
	sustain       time.Duration
	cooldown      time.Duration
	cooldownUntil time.Time
	stalledSince  map[int]time.Time
	// lastDecode holds each port's previous llamacpp:n_decode_total (and
	// n_tokens_max) so consecutive scrapes can prove live GPU progress.
	lastDecode map[int]float64
	lastMax    map[int]float64
}

func newHangDetector(minTPS float64, sustain, cooldown time.Duration) *hangDetector {
	return &hangDetector{
		minTPS:       minTPS,
		sustain:      sustain,
		cooldown:     cooldown,
		stalledSince: map[int]time.Time{},
		lastDecode:   map[int]float64{},
		lastMax:      map[int]float64{},
	}
}

// NotifySwitchComplete resets state and opens the post-switch cooldown
// window (default 120s — crown jewels).
func (d *hangDetector) NotifySwitchComplete(now time.Time) {
	d.cooldownUntil = now.Add(d.cooldown)
	d.stalledSince = map[int]time.Time{}
	d.lastDecode = map[int]float64{}
	d.lastMax = map[int]float64{}
}

// Forget drops state for a port no longer being scraped (unit stopped, or
// /metrics unreachable — a starting service must not accumulate stall time).
func (d *hangDetector) Forget(port int) {
	delete(d.stalledSince, port)
	delete(d.lastDecode, port)
	delete(d.lastMax, port)
}

// decodeAdvanced reports whether the port's live decode counters advanced
// since its previous scrape. An advancing n_decode_total (or growing
// n_tokens_max) proves llama_decode() is being called — the GPU is working —
// which the 0.0 TPS gauges of a long generation can't show.
func (d *hangDetector) decodeAdvanced(port int, m *LlamaMetrics) bool {
	advanced := false
	if m.NDecodeTotal != nil {
		prev, seen := d.lastDecode[port]
		d.lastDecode[port] = *m.NDecodeTotal
		if seen && *m.NDecodeTotal > prev {
			advanced = true
		}
	}
	if m.NTokensMax != nil {
		prev, seen := d.lastMax[port]
		d.lastMax[port] = *m.NTokensMax
		if seen && *m.NTokensMax > prev {
			advanced = true
		}
	}
	return advanced
}

// Observe feeds one port's scrape into the detector and returns an Alert
// while the port is in sustained stall.
func (d *hangDetector) Observe(now time.Time, port int, m *LlamaMetrics) *Alert {
	if now.Before(d.cooldownUntil) {
		d.stalledSince = map[int]time.Time{}
		return nil
	}
	if m == nil {
		d.Forget(port)
		return nil
	}

	active := m.RequestsProcessing > 0
	flowing := m.PromptTPS >= d.minTPS || m.PredictedTPS >= d.minTPS
	if !flowing {
		flowing = d.decodeAdvanced(port, m)
	}
	if !active || flowing {
		delete(d.stalledSince, port)
		return nil
	}

	since, ok := d.stalledSince[port]
	if !ok {
		d.stalledSince[port] = now
		return nil
	}
	stalled := now.Sub(since)
	if stalled < d.sustain {
		return nil
	}
	return &Alert{
		Code: "INFERENCE_HANG",
		Port: port,
		Msg: fmt.Sprintf(
			"Port %d: active request stalled %ds (PP %.1f tps, TG %.1f tps) with no decode progress — possible KFD queue eviction. "+
				"Confirm before restarting: sudo dmesg | grep -i 'kfd\\|amdgpu\\|evict' | tail -20; "+
				"a slot that is decoding (n_decode_total advancing) is NOT hung — do not restart it",
			port, int(stalled.Seconds()), m.PromptTPS, m.PredictedTPS,
		),
	}
}
