// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/launch"
)

var radTestArgs = []string{"--tp", "1", "--kv-cache-dtype", "fp8", "--max-num-seqs", "4", "--num-speculative-tokens", "4"}

func radService(ctxLen int, args []string) config.Service {
	return config.Service{Model: "m.rad", Alias: "rad", Context: ctxLen, PortRole: "a1", Backend: "rocm", EngineKind: "radiance",
		LlamaBin: "/opt/radiance/bin/radiance", ExtraArgs: args, BuildID: 3, VariantID: 4, WeightArtifactID: 5, StartupTimeoutS: 60,
		LaunchEnv: map[string]string{"RADIANCE_HOME": "/opt/radiance/home"}}
}

// radiance stub serves /v1/models and /metrics.
func radStub(t *testing.T, maxModelLen int, kvTokens string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"object":"list","data":[{"id":"rad","max_model_len":%d,"capabilities":{"tools":true,"reasoning":false,"efforts":"x"}}]}`, maxModelLen)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		if kvTokens != "" {
			fmt.Fprintf(w, "# TYPE radiance:kv_cache_max_tokens gauge\nradiance:kv_cache_max_tokens{model=\"rad\"} %s\n", kvTokens)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func radManager(t *testing.T, cfg *config.Config, srv *httptest.Server, gpu *collector.GPU, procRoot string, figures func(int64) []CuratedFigure) *Manager {
	t.Helper()
	baseURL := func(int) string { return "http://127.0.0.1:1" }
	if srv != nil {
		baseURL = func(int) string { return srv.URL }
	}
	m, err := NewManager(Deps{
		Cfg: func() *config.Config { return cfg }, Sys: newFakeSys(), GPU: gpu, Proc: collector.Proc{Root: procRoot},
		Usage: &fakeUsage{}, Notify: &notifyCounter{}, BaseURL: baseURL, Kill: func(int) error { return nil },
		PollInterval: time.Millisecond, Logf: t.Logf,
		// A tiny file-size / curated figure that must never be used for a radiance mode.
		WeightEstimateBytes: func(int64) (int64, bool) { return 1, true },
		CuratedFigures:      figures,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVerifiedContext_Radiance(t *testing.T) {
	ctx := context.Background()
	svc := radService(262144, radTestArgs)
	for _, tc := range []struct {
		name        string
		models, kv  string
		maxModelLen int
		want        int
	}{
		{"uncapped pool holds seqs x len", "", "1048656", 262144, 262144},
		{"capacity below the bound is the actual", "", "100000", 262144, 100000},
		{"capacity series absent: configured stands", "", "", 262144, 262144},
		{"models reports less than configured", "", "1048656", 131072, 131072},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := radManager(t, testConfig(t), radStub(t, tc.maxModelLen, tc.kv), nil, t.TempDir(), nil)
			if got := m.verifiedContext(ctx, 8080, svc); got != tc.want {
				t.Errorf("verifiedContext = %d, want %d", got, tc.want)
			}
		})
	}
	// /v1/models unreachable: never a guess.
	m := radManager(t, testConfig(t), nil, nil, t.TempDir(), nil)
	if got := m.verifiedContext(ctx, 8080, svc); got != 0 {
		t.Errorf("unreachable /v1/models gave %d, want 0", got)
	}
}

// The chat-template probe for a kind that serves no /props falls back to /v1/models capabilities (non-boolean members dropped).
func TestChatTemplateCapsFallsBackToModels(t *testing.T) {
	var got map[string]bool
	cfg := testConfig(t)
	m := radManager(t, cfg, radStub(t, 4096, "4096"), nil, t.TempDir(), nil)
	m.d.UpdateChatTemplateCaps = func(_ context.Context, id int64, caps map[string]bool) error {
		if id != 9 {
			t.Errorf("config id %d", id)
		}
		got = caps
		return nil
	}
	svc := radService(4096, radTestArgs)
	if !m.probesChatTemplateCaps(svc) {
		t.Fatal("radiance must be probed")
	}
	m.probeChatTemplateCaps(context.Background(), 8080, 9, svc)
	if len(got) != 2 || !got["tools"] || got["reasoning"] {
		t.Errorf("caps = %v", got)
	}
}

// A radiance process counts toward the budget's RSS exactly like llama-server (the process-name sweep follows the kind table).
func TestMemoryBudgetCountsRadianceRSS(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"))
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf")
	root := t.TempDir()
	pid := filepath.Join(root, "500")
	put(t, filepath.Join(pid, "comm"), "radiance\n")
	put(t, filepath.Join(pid, "cmdline"), "radiance\x00--port\x008080\x00")
	put(t, filepath.Join(pid, "status"), "VmRSS:\t"+strconv.Itoa(40*1024*1024)+" kB\n")
	gpu := incidentGPU(t, int64(4000)*1024*1024, int64(122880)*1024*1024)
	m, _, _ := newIncidentManager(t, cfg, sys, gpu, root, nil)
	b, err := m.MemoryBudget()
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(4000)*1024*1024 + int64(40)*1024*1024*1024; b.UsedBytes != want {
		t.Errorf("used = %d, want gtt + radiance RSS = %d", b.UsedBytes, want)
	}
}

func radFitConfig(t *testing.T, ctxLen int, args []string) *config.Config {
	cfg := testConfig(t)
	cfg.Modes["rad"] = config.Mode{ConfigID: 7, Services: []config.Service{radService(ctxLen, args)}}
	return cfg
}

func tupleKey(buildID, variantID, artifactID int64, nctx int, args []string) string {
	return launch.NewMemoryTuple(buildID, variantID, artifactID, nctx, args).Key()
}

func TestFitGate_CuratedTuple(t *testing.T) {
	gpu := incidentGPU(t, int64(10)*incidentGiB, int64(120)*incidentGiB)
	procRoot := t.TempDir()
	put(t, filepath.Join(procRoot, "meminfo"), "MemTotal: 125829120 kB\nMemAvailable: 104857600 kB\n") // 100 GiB available
	fig := int64(54117000000)
	good := tupleKey(3, 4, 5, 262144, radTestArgs)

	plan := func(figures []CuratedFigure, args []string, ctxLen int) Plan {
		cfg := radFitConfig(t, ctxLen, args)
		m := radManager(t, cfg, nil, gpu, procRoot, func(id int64) []CuratedFigure {
			if id != 7 {
				t.Errorf("figures asked for config %d", id)
			}
			return figures
		})
		p, err := m.FitPlan("rad")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// No benchmark: refused, names the missing tuple, and the 1-byte WeightEstimateBytes / file size is never used.
	p := plan(nil, radTestArgs, 262144)
	if p.Fits || !strings.Contains(p.Message, "no measured memory figure") || !strings.Contains(p.Message, "--max-num-seqs 4") {
		t.Errorf("no figure: %+v", p)
	}
	// A figure measured at another tuple (different seqs; different len; different build) is not reused.
	for name, k := range map[string]string{
		"other seqs":  tupleKey(3, 4, 5, 262144, []string{"--tp", "1", "--kv-cache-dtype", "fp8", "--max-num-seqs", "8", "--num-speculative-tokens", "4"}),
		"other len":   tupleKey(3, 4, 5, 131072, radTestArgs),
		"other build": tupleKey(9, 4, 5, 262144, radTestArgs),
		"other kv":    tupleKey(3, 4, 5, 262144, []string{"--tp", "1", "--kv-cache-dtype", "bf16", "--max-num-seqs", "4", "--num-speculative-tokens", "4"}),
	} {
		p := plan([]CuratedFigure{{Bytes: fig, Tuple: k}}, radTestArgs, 262144)
		if p.Fits || !strings.Contains(p.Message, "other tuples") {
			t.Errorf("%s: %+v", name, p)
		}
	}
	// An unreadable tuple is ignored, never matched.
	if p := plan([]CuratedFigure{{Bytes: fig, Tuple: "{"}}, radTestArgs, 262144); p.Fits {
		t.Errorf("garbage tuple admitted: %+v", p)
	}
	// The matching tuple admits (flag order does not matter), with the registered figure as the need.
	reordered := []string{"--num-speculative-tokens", "4", "--max-num-seqs", "4", "--kv-cache-dtype", "fp8", "--tp", "1"}
	p = plan([]CuratedFigure{{Bytes: fig, Tuple: good}}, reordered, 262144)
	if !p.Fits || int64(p.NeedBytes) != fig {
		t.Errorf("matching tuple: %+v", p)
	}
	// Largest matching figure wins.
	p = plan([]CuratedFigure{{Bytes: fig, Tuple: good}, {Bytes: fig + 1000, Tuple: good}}, radTestArgs, 262144)
	if int64(p.NeedBytes) != fig+1000 {
		t.Errorf("need = %v", p.NeedBytes)
	}
	// Admission is need + used <= budget: the same figure with the card mostly occupied is refused (nothing to evict).
	full := incidentGPU(t, int64(100)*incidentGiB, int64(120)*incidentGiB)
	m := radManager(t, radFitConfig(t, 262144, radTestArgs), nil, full, procRoot, func(int64) []CuratedFigure { return []CuratedFigure{{Bytes: fig, Tuple: good}} })
	if p, _ := m.FitPlan("rad"); p.Fits || !strings.Contains(p.Message, "not enough VRAM") {
		t.Errorf("occupied card: %+v", p)
	}
	// No figure source wired at all: refused, never file size.
	m = radManager(t, radFitConfig(t, 262144, radTestArgs), nil, gpu, procRoot, nil)
	if p, _ := m.FitPlan("rad"); p.Fits {
		t.Errorf("nil figure source admitted: %+v", p)
	}
	// An in-flight radiance load with no figure reserves nothing it cannot size and the gate does not pass it by.
	if _, ok := m.modeNeedEstimate(m.d.Cfg(), "rad"); ok {
		t.Error("modeNeedEstimate returned a size for an unregistered tuple")
	}
}

// Other kinds are untouched by the gate.
func TestFitGate_OtherKindsKeepTheirChain(t *testing.T) {
	cfg := testConfig(t)
	g := cfg.Modes["gemma"]
	g.ConfigID = 1
	cfg.Modes["gemma"] = g
	m := radManager(t, cfg, nil, incidentGPU(t, 0, int64(120)*incidentGiB), t.TempDir(), nil)
	if need, ok := m.modeNeedEstimate(cfg, "gemma"); !ok || need != 1 {
		t.Errorf("llama.cpp mode: need %d ok %v, want the curated figure 1 from WeightEstimateBytes", need, ok)
	}
}
