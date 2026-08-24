// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"testing"
	"time"
)

func lm(processing, promptTPS, predictedTPS float64) *LlamaMetrics {
	return &LlamaMetrics{
		RequestsProcessing: processing,
		PromptTPS:          promptTPS,
		PredictedTPS:       predictedTPS,
	}
}

// lmDecode is lm plus a live n_decode_total counter (llama.cpp #26920-era
// builds: gauges/totals frozen during a long generation, decode counter
// advancing).
func lmDecode(processing, promptTPS, predictedTPS, decodeTotal float64) *LlamaMetrics {
	m := lm(processing, promptTPS, predictedTPS)
	m.NDecodeTotal = &decodeTotal
	return m
}

// Crown jewels: hang = requests_processing > 0 AND TPS < 0.1, sustained 90s,
// with a 120s post-switch cooldown.
func TestHangDetection(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	type obs struct {
		atS   int
		m     *LlamaMetrics
		alert bool
	}
	cases := []struct {
		name string
		obs  []obs
	}{
		{
			name: "sustained stall fires after 90s",
			obs: []obs{
				{0, lm(1, 0, 0), false},    // stall starts
				{45, lm(1, 0, 0), false},   // 45s — under threshold
				{89, lm(1, 0, 0), false},   // 89s — still under
				{91, lm(1, 0, 0), true},    // ≥90s sustained — alert
				{95, lm(1, 0, 0), true},    // level-triggered: stays up
				{99, lm(1, 5.0, 0), false}, // tokens flow again — clears
			},
		},
		{
			name: "idle port never alerts however slow",
			obs: []obs{
				{0, lm(0, 0, 0), false},
				{100, lm(0, 0, 0), false},
				{300, lm(0, 0, 0), false},
			},
		},
		{
			name: "flowing tokens reset the stall timer",
			obs: []obs{
				{0, lm(1, 0, 0), false},
				{60, lm(1, 0, 2.0), false}, // TG above min — reset
				{120, lm(1, 0, 0), false},  // new stall starts here
				{200, lm(1, 0, 0), false},  // only 80s into new stall
				{215, lm(1, 0, 0), true},   // 95s — fires
			},
		},
		{
			name: "prompt-processing throughput alone counts as flowing",
			obs: []obs{
				{0, lm(1, 0.5, 0), false}, // PP ≥ 0.1 tps (nemotron prefill)
				{120, lm(1, 0.5, 0), false},
			},
		},
		{
			name: "TPS just above threshold never stalls",
			obs: []obs{
				{0, lm(1, 0, 0.1), false},
				{120, lm(1, 0, 0.1), false},
			},
		},
		{
			name: "unreachable port clears stall state",
			obs: []obs{
				{0, lm(1, 0, 0), false},
				{60, nil, false},          // /metrics gone — forget
				{70, lm(1, 0, 0), false},  // stall restarts from scratch
				{140, lm(1, 0, 0), false}, // 70s — under
				{165, lm(1, 0, 0), true},  // 95s — fires
			},
		},
		{
			// llama.cpp #26920 regression: gauges read 0 for the WHOLE
			// of a long generation, but n_decode_total advances every
			// decode batch. An advancing decode counter must count as
			// flowing or every long agentic run false-positives.
			name: "long generation with advancing decode counter never alerts",
			obs: []obs{
				{0, lmDecode(1, 0, 0, 100), false}, // baseline; stall timer starts
				{45, lmDecode(1, 0, 0, 145), false},
				{89, lmDecode(1, 0, 0, 189), false},
				{120, lmDecode(1, 0, 0, 220), false},
				{300, lmDecode(1, 0, 0, 400), false}, // hours of this — never fires
			},
		},
		{
			// Decode counter advancing between scrapes is the live GPU
			// signal: a genuinely stalled (KFD-evicted) slot freezes it,
			// so the stall must still fire.
			name: "frozen decode counter with zero gauges still alerts",
			obs: []obs{
				{0, lmDecode(1, 0, 0, 500), false},
				{45, lmDecode(1, 0, 0, 500), false},
				{95, lmDecode(1, 0, 0, 500), true}, // ≥90s, no decode progress
			},
		},
		{
			// A long generation that finishes and is followed by a
			// genuine stall: decode counter advances then freezes.
			name: "decode counter advancing then frozen fires once stalled",
			obs: []obs{
				{0, lmDecode(1, 0, 0, 100), false},
				{60, lmDecode(1, 0, 0, 160), false},
				{120, lmDecode(1, 0, 0, 160), false}, // decode stops here
				{180, lmDecode(1, 0, 0, 160), false}, // 60s into new stall
				{215, lmDecode(1, 0, 0, 160), true},  // 95s — fires
			},
		},
		{
			// Counter wrap (llama-server restart) must not count as
			// flowing forever: it only counts when strictly greater.
			name: "decode counter restart is not falsely flowing",
			obs: []obs{
				{0, lmDecode(1, 0, 0, 500), false},
				{95, lmDecode(1, 0, 0, 3), true}, // reset to ~0, still stalled — fires
			},
		},
		{
			// Gauge flowing remains the primary signal when the counter
			// is absent (older builds export no n_decode_total).
			name: "missing decode counter falls back to gauges",
			obs: []obs{
				{0, lm(1, 0, 0), false},
				{95, lm(1, 2.0, 0), false}, // gauge flows — clears
				{190, lm(1, 0, 0), false},  // new stall from scratch
				{275, lm(1, 0, 0), false},  // 85s — under
				{282, lm(1, 0, 0), true},   // 92s — fires
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newHangDetector(0.1, 90*time.Second, 120*time.Second)
			for _, o := range tc.obs {
				got := d.Observe(at(o.atS), 8080, o.m)
				if (got != nil) != o.alert {
					t.Errorf("t=%ds: alert=%v, want %v", o.atS, got != nil, o.alert)
				}
			}
		})
	}
}

// Crown jewels: 120s cooldown after a switch — model load and KV allocation
// look exactly like a stall and must not alert.
func TestHangCooldownAfterSwitch(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	d := newHangDetector(0.1, 90*time.Second, 120*time.Second)

	// Stall in progress, 80s accumulated...
	if a := d.Observe(at(0), 8080, lm(1, 0, 0)); a != nil {
		t.Fatal("unexpected alert")
	}
	if a := d.Observe(at(80), 8080, lm(1, 0, 0)); a != nil {
		t.Fatal("unexpected alert")
	}

	// ...switch completes: state resets and detection sleeps for 120s.
	d.NotifySwitchComplete(at(85))
	for _, s := range []int{90, 150, 204} {
		if a := d.Observe(at(s), 8080, lm(1, 0, 0)); a != nil {
			t.Errorf("t=%ds inside cooldown: got alert", s)
		}
	}
	// Cooldown ends at 205. Stall timing starts fresh from the first
	// post-cooldown observation — pre-switch stall time must not carry.
	if a := d.Observe(at(206), 8080, lm(1, 0, 0)); a != nil {
		t.Error("stall timer must restart after cooldown")
	}
	if a := d.Observe(at(290), 8080, lm(1, 0, 0)); a != nil {
		t.Error("84s after cooldown — must not fire yet")
	}
	if a := d.Observe(at(297), 8080, lm(1, 0, 0)); a == nil {
		t.Error("91s sustained after cooldown — must fire")
	}
}

func TestHangTracksPortsIndependently(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	d := newHangDetector(0.1, 90*time.Second, 120*time.Second)

	d.Observe(at(0), 8080, lm(1, 0, 0))
	d.Observe(at(0), 8087, lm(1, 50, 30)) // busy and healthy

	if a := d.Observe(at(95), 8080, lm(1, 0, 0)); a == nil {
		t.Error("8080 should alert")
	}
	if a := d.Observe(at(95), 8087, lm(1, 50, 30)); a != nil {
		t.Error("8087 must not alert")
	}
}
