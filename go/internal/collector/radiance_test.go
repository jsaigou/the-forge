// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
)

// Fixture scrapes are real Radiance /metrics output; see testdata/radiance/README.md.
func radFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "radiance", "seqstudy-"+name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// withSeries rewrites one unlabelled-besides-model series of a scrape to v (the series must exist).
func withSeries(t *testing.T, text, name, v string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^(` + regexp.QuoteMeta(name) + `\{[^}]*\}) \S+$`)
	if !re.MatchString(text) {
		t.Fatalf("series %s not in fixture", name)
	}
	return re.ReplaceAllString(text, "${1} "+v)
}

func fp(v float64) *float64 { return &v }

func TestRadianceFixturesParse(t *testing.T) {
	const base = "s1-l262144-d4.metrics-"
	cases := []struct {
		file                  string
		running, queued       float64
		steps, prefill, decTP float64
		capTokens             float64
		prefillTPS, decodeTPS float64
	}{
		{"ready", 0, 0, 0, 0, 0, 262208, 0, 0},
		{"idle", 0, 0, 3, 15, 9, 262208, 0.055348962545394897, 0},
		{"after-reach", 0, 0, 473, 266928, 788, 262208, 47.484523773193359, 0.42887318134307861},
		{"final", 0, 0, 501, 299244, 836, 262208, 573.13104248046875, 3.1401934623718262},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			m := ParseRadianceMetrics(radFixture(t, base+tc.file))
			if m.RequestsProcessing != tc.running || m.RequestsQueued != tc.queued {
				t.Errorf("requests = %v/%v, want %v/%v", m.RequestsProcessing, m.RequestsQueued, tc.running, tc.queued)
			}
			if m.ProgressTotal == nil || *m.ProgressTotal != tc.steps {
				t.Errorf("ProgressTotal = %v, want %v", m.ProgressTotal, tc.steps)
			}
			if m.PromptTotal == nil || *m.PromptTotal != tc.prefill || m.PredictedTotal == nil || *m.PredictedTotal != tc.decTP {
				t.Errorf("token totals = %v/%v, want %v/%v", m.PromptTotal, m.PredictedTotal, tc.prefill, tc.decTP)
			}
			if m.ContextCapacity == nil || *m.ContextCapacity != tc.capTokens {
				t.Errorf("ContextCapacity = %v, want %v", m.ContextCapacity, tc.capTokens)
			}
			if m.PromptTPS != tc.prefillTPS || m.PredictedTPS != tc.decodeTPS {
				t.Errorf("tps = %v/%v, want %v/%v", m.PromptTPS, m.PredictedTPS, tc.prefillTPS, tc.decodeTPS)
			}
			if m.NDecodeTotal != nil || m.NTokensMax != nil {
				t.Error("a radiance scrape must not fill the llama.cpp-only counters")
			}
		})
	}
}

func TestRadianceSeriesExposed(t *testing.T) {
	m := ParseRadianceMetrics(radFixture(t, "s1-l262144-d4.metrics-after-reach"))
	for name, want := range map[string]float64{
		`radiance:draft_position_reached_total{position="0"}`:                       339,
		`radiance:prefix_cache_ckpt_evictions_by_reason_total{reason="store_full"}`: 4,
		`vllm:request_success_total{finished_reason="length"}`:                      4,
		`radiance:kv_stream_bytes_total{dir="in"}`:                                  0,
		`radiance:engine_steps_decode_total`:                                        339,
	} {
		got, ok := m.Series[name]
		if !ok || got != want {
			t.Errorf("Series[%s] = %v (present %v), want %v", name, got, ok, want)
		}
	}
	for name := range m.Series {
		if strings.HasPrefix(name, "llamacpp:") || strings.Contains(name, "_bucket") {
			t.Errorf("unexpected series %s", name)
		}
	}
}

func TestRadianceMetricsMissingSeriesDegrade(t *testing.T) {
	for name, text := range map[string]string{
		"empty":        "",
		"llamacpp":     "llamacpp:requests_processing 3\nllamacpp:n_decode_total 9\n",
		"garbage":      "vllm:num_requests_running{broken 1\nradiance:engine_steps_total notanumber\n\x00\n",
		"only running": `vllm:num_requests_running{model_name="m",engine="0"} 2` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			m := ParseRadianceMetrics(text)
			if m.ProgressTotal != nil || m.PromptTotal != nil || m.ContextCapacity != nil {
				t.Errorf("absent series must stay nil: %+v", m)
			}
			if name == "llamacpp" && m.RequestsProcessing != 0 {
				t.Error("a radiance reader must not read llamacpp:requests_processing")
			}
			// Without the progress counter the rule falls back to the gauges; a bare "running" with no gauges is the llama.cpp rule's job, not a
			// false hang by itself within the sustain window.
			d := newHangDetector(0.1, 90*time.Second, 120*time.Second)
			t0 := time.Unix(1_800_000_000, 0)
			if a := d.Observe(t0, 1, m); a != nil {
				t.Errorf("first scrape alerted: %v", a)
			}
		})
	}
}

// Detector driven by recorded text: running is forced to 1 (the fixtures are quiescent scrapes) and the counters come from the recording.
func TestRadianceHangFromRecordedText(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	afterReach := radFixture(t, "s1-l262144-d4.metrics-after-reach") // rate gauges still hold 47 prefill tps here with nothing running
	busy := func(text string) *LlamaMetrics {
		return ParseRadianceMetrics(withSeries(t, text, "vllm:num_requests_running", "1"))
	}

	t.Run("stalled counter trips even though the rate gauges hold a nonzero value", func(t *testing.T) {
		d := newHangDetector(0.1, 90*time.Second, 120*time.Second)
		var alert *Alert
		for s := 0; s <= 100; s += 2 {
			alert = d.Observe(at(s), 7, busy(afterReach))
			if s < 90 && alert != nil {
				t.Fatalf("alerted at %ds, before the 90s sustain", s)
			}
		}
		if alert == nil || alert.Code != "INFERENCE_HANG" || alert.Port != 7 {
			t.Fatalf("no INFERENCE_HANG after 100s of a frozen step counter: %+v", alert)
		}
	})

	t.Run("prefill with no decode must not trip", func(t *testing.T) {
		// 240 s of running=1, decode tokens and decode TPS frozen at zero, the step and prefill counters advancing once per scrape (the study's
		// 233 s prefill at 236,863 tokens). Counter movement is synthesised from the recorded text; what the recordings cannot show is that it
		// moves mid-prefill (README).
		d := newHangDetector(0.1, 90*time.Second, 120*time.Second)
		for s, steps := 0, 3; s <= 240; s, steps = s+2, steps+1 {
			text := withSeries(t, radFixture(t, "s1-l262144-d4.metrics-idle"), "radiance:engine_steps_total", fmt.Sprint(steps))
			text = withSeries(t, text, "radiance:prefill_tokens_total", fmt.Sprint(15+steps*2000))
			m := busy(text)
			if m.PredictedTPS != 0 || *m.PredictedTotal != 9 {
				t.Fatal("test premise: no decode in this sequence")
			}
			if a := d.Observe(at(s), 7, m); a != nil {
				t.Fatalf("healthy prefill flagged hung at %ds: %s", s, a.Msg)
			}
		}
	})

	t.Run("progress after a stall clears it", func(t *testing.T) {
		d := newHangDetector(0.1, 90*time.Second, 120*time.Second)
		for s := 0; s <= 100; s += 2 {
			d.Observe(at(s), 7, busy(afterReach))
		}
		moved := withSeries(t, afterReach, "radiance:engine_steps_total", "474")
		if a := d.Observe(at(102), 7, busy(moved)); a != nil {
			t.Fatalf("alert persisted after the step counter moved: %s", a.Msg)
		}
	})

	t.Run("idle engine never alerts and queued work counts as active", func(t *testing.T) {
		d := newHangDetector(0.1, 90*time.Second, 120*time.Second)
		for s := 0; s <= 300; s += 2 {
			if a := d.Observe(at(s), 7, ParseRadianceMetrics(afterReach)); a != nil {
				t.Fatalf("idle engine flagged at %ds", s)
			}
		}
		d = newHangDetector(0.1, 90*time.Second, 120*time.Second)
		queued := ParseRadianceMetrics(withSeries(t, afterReach, "vllm:num_requests_waiting", "2"))
		var a *Alert
		for s := 0; s <= 100; s += 2 {
			a = d.Observe(at(s), 7, queued)
		}
		if a == nil {
			t.Error("waiting requests with a frozen engine must count as a stall")
		}
	})

	t.Run("cooldown after a switch suppresses it", func(t *testing.T) {
		d := newHangDetector(0.1, 90*time.Second, 120*time.Second)
		d.NotifySwitchComplete(at(0))
		for s := 0; s <= 100; s += 2 {
			if a := d.Observe(at(s), 7, busy(afterReach)); a != nil {
				t.Fatal("alerted inside the cooldown")
			}
		}
	})
}

func radianceHealthServer(t *testing.T, code int, body string, metrics string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(code)
			fmt.Fprint(w, body)
		case "/metrics":
			fmt.Fprint(w, metrics)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRadianceStuckHealthRaisesHangAtOnce(t *testing.T) {
	metrics := radFixture(t, "s1-l262144-d4.metrics-after-reach")
	stuckBody := `{"status":"stuck","model":"qwen35moe","in_flight":1,"queue_cap":0,"stuck_s":61}` // shape: core/server/admin.cpp handle_ping
	srv := radianceHealthServer(t, 503, stuckBody, metrics)
	l := NewLlamaClient(func(int) string { return srv.URL })
	m := l.RadianceMetrics(context.Background(), 1)
	if m == nil || !m.Stuck || m.StuckSeconds != 61 {
		t.Fatalf("stuck body not read: %+v", m)
	}
	d := newHangDetector(0.1, 90*time.Second, 120*time.Second)
	a := d.Observe(time.Unix(1_800_000_000, 0), 1, m) // first scrape: no sustain window
	if a == nil || a.Code != "INFERENCE_HANG" || !strings.Contains(a.Msg, "stuck") {
		t.Fatalf("stuck health did not raise INFERENCE_HANG at once: %+v", a)
	}

	// /metrics unreachable but /health says stuck: still reported.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(503)
			fmt.Fprint(w, stuckBody)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv2.Close()
	l2 := NewLlamaClient(func(int) string { return srv2.URL })
	if m2 := l2.RadianceMetrics(context.Background(), 1); m2 == nil || !m2.Stuck {
		t.Fatalf("stuck without /metrics lost: %+v", m2)
	}

	// Not-stuck 503s and 200s do not alert.
	for name, tc := range map[string]struct {
		code int
		body string
	}{
		"stopping": {503, `{"status":"stopping","model":"m","in_flight":0,"queue_cap":0}`},
		"ok":       {200, `{"status":"ok","model":"m","in_flight":0,"queue_cap":0}`},
		"nonjson":  {503, `upstream down`},
	} {
		s := radianceHealthServer(t, tc.code, tc.body, metrics)
		lc := NewLlamaClient(func(int) string { return s.URL })
		mm := lc.RadianceMetrics(context.Background(), 1)
		if mm == nil || mm.Stuck {
			t.Errorf("%s: Stuck = %v", name, mm != nil && mm.Stuck)
		}
	}
}

// End to end through the collector: the slot's mode names a radiance service, so the Spec selects the reader; /props is never asked for and the
// context comes from /v1/models bounded by the capacity series.
func TestCollectorSelectsRadianceReaderBySpec(t *testing.T) {
	cfg := testConfig(t)
	cfg.Modes["rad"] = config.Mode{Services: []config.Service{{
		Model: "m.rad", Alias: "rad", Context: 262144, PortRole: "a1", Backend: "rocm", EngineKind: "radiance",
	}}}
	sys := &fakeSystemd{}
	sys.set("forge-a1", "active", "running")

	var mu sync.Mutex
	var paths []string
	metrics := withSeries(t, radFixture(t, "s1-l262144-d4.metrics-after-reach"), "vllm:num_requests_running", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprint(w, metrics)
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"qwen35moe","max_model_len":262144,"capabilities":{"tools":true}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	now := time.Unix(1_800_000_000, 0)
	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		Slots:    &fakeSlots{m: map[string]SlotAssignment{"a1": {Mode: "rad"}}},
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     Proc{Root: t.TempDir()},
		BaseURL:  func(int) string { return srv.URL },
		DialPort: func(int) bool { return false },
		Now:      func() time.Time { return now },
	})
	var snap *Snapshot
	for i := 0; i < 50; i++ { // 100 s of a busy engine whose step counter is frozen
		snap = c.ProbeNow(context.Background())
		now = now.Add(2 * time.Second)
	}
	inf := snap.Inference["a1"]
	if inf.RequestsProcessing != 1 || inf.NCtx != 262144 || inf.ModelAlias != "qwen35moe" {
		t.Errorf("inference = %+v", inf)
	}
	if inf.PromptTokensTotal != 266928 || inf.PredictedTokensTotal != 788 {
		t.Errorf("token totals not from the per-step counters: %+v", inf)
	}
	hung := false
	for _, a := range snap.Alerts {
		if a.Code == "INFERENCE_HANG" {
			hung = true
		}
	}
	if !hung {
		t.Errorf("a busy radiance slot with a frozen step counter did not raise INFERENCE_HANG: %+v", snap.Alerts)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if p == "/props" {
			t.Error("a radiance slot must not be probed on /props")
		}
	}
}
