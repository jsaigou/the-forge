// SPDX-License-Identifier: Apache-2.0

package engine

// Regression tests for the 2026-08-22 GTT-exhaustion host crash:
//   - vulkan-backend slots' RSS was invisible to the fit gate (rocm-only
//     filter), so two sibling configs of one GGUF passed a check that could
//     not physically be satisfied;
//   - nothing reserved an in-flight load's footprint between admission and
//     materialization;
//   - kernel-reported host headroom was ignored entirely;
//   - same-weights sibling loads now need proven headroom at Load.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/gguf"
)

const incidentGiB = int64(1) << 30

// incidentNeedBytes is the WeightEstimateBytes test figure — bigger than any
// staged GTT total in the refusal cases, smaller than it where room must exist.
const incidentNeedBytes = int64(50) * incidentGiB

// put writes content to path, creating parent directories.
func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// siblingConfig adds gemma-nothink — same GGUF as gemma under a different
// config name — mirroring the live catalog's duplicate-artifact-row pair.
func siblingConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	nothink := cfg.Modes["gemma"]
	svc := nothink.Services[0]
	svc.Alias = "gemma-nothink"
	nothink.Services = []config.Service{svc}
	cfg.Modes["gemma-nothink"] = nothink
	g := cfg.Modes["gemma"]
	g.ConfigID = 1
	cfg.Modes["gemma"] = g
	n := cfg.Modes["gemma-nothink"]
	n.ConfigID = 2
	cfg.Modes["gemma-nothink"] = n
	return cfg
}

// incidentGPU stages a GPU DRM root reporting gtt used/total bytes.
func incidentGPU(t *testing.T, used, total int64) *collector.GPU {
	t.Helper()
	root := t.TempDir()
	dev := filepath.Join(root, "card0", "device")
	put(t, filepath.Join(dev, "vendor"), "0x1002\n")
	put(t, filepath.Join(dev, "mem_info_gtt_used"), strconv.FormatInt(used, 10)+"\n")
	put(t, filepath.Join(dev, "mem_info_gtt_total"), strconv.FormatInt(total, 10)+"\n")
	return &collector.GPU{DRMRoot: root}
}

// incidentProcRoot stages /proc with one llama-server on port carrying
// rssKiB of VmRSS, plus optional meminfo text. port < 0 stages no process.
func incidentProcRoot(t *testing.T, port int, rssKiB int64, meminfo string) string {
	t.Helper()
	root := t.TempDir()
	if meminfo != "" {
		put(t, filepath.Join(root, "meminfo"), meminfo)
	}
	if port >= 0 {
		pidDir := filepath.Join(root, strconv.Itoa(500))
		put(t, filepath.Join(pidDir, "comm"), "llama-server\n")
		put(t, filepath.Join(pidDir, "cmdline"), "llama-server\x00--port\x00"+strconv.Itoa(port)+"\x00")
		put(t, filepath.Join(pidDir, "status"), "VmRSS:\t"+strconv.FormatInt(rssKiB, 10)+" kB\n")
	}
	return root
}

// newIncidentManager builds a Manager over the staged environment with a
// catalog weight estimate of incidentNeedBytes for every config. sys is
// passed in so callers can pre-seed unit-state sequences.
func newIncidentManager(t *testing.T, cfg *config.Config, sys *fakeSys, gpu *collector.GPU, procRoot string, stub *llamaStub) (*Manager, *fakeUsage, *notifyCounter) {
	t.Helper()
	usage := &fakeUsage{}
	notify := &notifyCounter{}
	baseURL := func(int) string { return "http://127.0.0.1:1" }
	if stub != nil {
		baseURL = func(int) string { return stub.srv.URL }
	}
	m, err := NewManager(Deps{
		Cfg:          func() *config.Config { return cfg },
		Sys:          sys,
		GPU:          gpu,
		Proc:         collector.Proc{Root: procRoot},
		Usage:        usage,
		Notify:       notify,
		BaseURL:      baseURL,
		Kill:         func(int) error { return nil },
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
		ReadMeta: func(string) (gguf.Metadata, error) {
			return gguf.Metadata{TrainedCtx: 131072}, nil
		},
		WeightEstimateBytes: func(int64) (int64, bool) { return incidentNeedBytes, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, usage, notify
}

// A vulkan slot's llama-server RSS must reach the budget: before the fix
// only rocm slots counted, so vulkan loads were admitted blind.
func TestMemoryBudgetCountsVulkanSlotRSS(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	gpu := incidentGPU(t, int64(4000)*1024*1024, int64(122880)*1024*1024)
	procRoot := incidentProcRoot(t, 8080, 40*1024*1024, "")

	sys.setSeq("forge-a1", st("active", "running"))
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf") // vulkan backend mode

	m, _, _ := newIncidentManager(t, cfg, sys, gpu, procRoot, nil)

	b, err := m.MemoryBudget()
	if err != nil {
		t.Fatal(err)
	}
	want := int64(4000)*1024*1024 + int64(40)*incidentGiB
	if b.UsedBytes != want {
		t.Errorf("UsedBytes = %d, want %d (vulkan RSS must count)", b.UsedBytes, want)
	}
}

// Kernel-reported headroom caps FreeBytes: never plan into the reserve.
func TestMemoryBudgetCapsFreeByHostHeadroom(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	gpu := incidentGPU(t, 0, int64(122880)*1024*1024)
	meminfo := "MemTotal:       " + strconv.Itoa(128*1024*1024) + " kB\n" +
		"MemAvailable:   " + strconv.Itoa(10*1024*1024) + " kB\n" // 10 GiB
	procRoot := incidentProcRoot(t, -1, 0, meminfo)

	m, _, _ := newIncidentManager(t, cfg, sys, gpu, procRoot, nil)

	b, err := m.MemoryBudget()
	if err != nil {
		t.Fatal(err)
	}
	wantCap := int64(2) * incidentGiB // MemAvailable − 8 GiB reserve
	if b.FreeBytes > wantCap {
		t.Errorf("FreeBytes = %d, must be capped at %d by MemAvailable", b.FreeBytes, wantCap)
	}
}

// An in-flight load reserves its full estimate: back-to-back admissions
// cannot both claim memory only one will get.
func TestFitPlanReservesInFlightLoad(t *testing.T) {
	cfg := siblingConfig(t)
	// 90 GiB GTT vs a 50 GiB estimate each: one load fits, two don't —
	// exactly the incident shape without real page accounting.
	gpu := incidentGPU(t, 0, int64(90)*incidentGiB)

	newM := func(t *testing.T) *Manager {
		m, _, _ := newIncidentManager(t, cfg, newFakeSys(), gpu, t.TempDir(), nil)
		return m
	}

	t.Run("without in-flight load fits", func(t *testing.T) {
		plan, err := newM(t).FitPlan("gemma")
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Fits {
			t.Fatalf("plan = %+v, want fits", plan)
		}
	})

	t.Run("with in-flight sibling load refused", func(t *testing.T) {
		m := newM(t)
		m.setTransition("a2", &collector.Transition{Mode: "gemma-nothink", StartedAt: time.Now()}, nil)
		plan, err := m.FitPlan("gemma")
		if err != nil {
			t.Fatal(err)
		}
		if plan.Fits || len(plan.Evict) != 0 {
			t.Fatalf("plan = %+v, want terminal refusal (nothing loaded to evict)", plan)
		}
	})
}

// The engine-level sibling guard: same weights + no room ⇒ refuse fast with
// an explicit reason instead of wedging the host.
func TestLoadRefusesSiblingWeightsWithoutRoom(t *testing.T) {
	cfg := siblingConfig(t)
	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"))
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf")

	// 10 GiB GTT < 50 GiB need; no model files on disk ⇒ eviction recovers
	// nothing ⇒ terminal refusal shape.
	gpu := incidentGPU(t, 0, int64(10)*incidentGiB)
	stub := newLlamaStub(t, 131072)
	m, _, _ := newIncidentManager(t, cfg, sys, gpu, t.TempDir(), stub)
	m.setSlotMode("a1", "gemma")

	res := m.Load(context.Background(), "gemma-nothink", "a2")
	if res.Success {
		t.Fatalf("res = %+v, want refusal", res)
	}
	if !strings.Contains(res.Message, "identical weights already resident") {
		t.Fatalf("res.Message = %q, want sibling refusal", res.Message)
	}
}

// Same weights WITH room proceeds past the guard into the normal flow.
func TestLoadAllowsSiblingWeightsWithRoom(t *testing.T) {
	cfg := siblingConfig(t)
	sys := newFakeSys()
	stub := newLlamaStub(t, 131072)
	gpu := incidentGPU(t, 0, int64(122880)*1024*1024)

	m, _, _ := newIncidentManager(t, cfg, sys, gpu, t.TempDir(), stub)
	m.setSlotMode("a1", "gemma")
	// Leading inactive states absorb the live reconciles the sibling guard
	// adds before Load's crown-jewels pre-check (constructor + FitPlan +
	// MemoryBudget each probe every unit; fakeSys consumes per call).
	sys.setSeq("forge-a2",
		st("inactive", "dead"),
		st("inactive", "dead"),
		st("inactive", "dead"),
		st("activating", "start"),
		st("active", "running"),
	)

	res := m.Load(context.Background(), "gemma-nothink", "a2")
	if !res.Success {
		t.Fatalf("res = %+v, want success (room exists)", res)
	}
	if strings.Contains(res.Message, "identical weights") {
		t.Fatalf("sibling guard fired despite room: %s", res.Message)
	}
}
