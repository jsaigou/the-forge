package engine

import (
	"path/filepath"
	"strconv"
	"testing"
)

// evictionManager stages one loaded slot (a1, pid 500) holding gpuGiB of GPU memory and the given resident memory, on a host with availGiB
// available. argv is the process's command line. The mode to place needs incidentNeedBytes (50 GiB).
func evictionManager(t *testing.T, argv string, gpuGiB, anonGiB, fileGiB, availGiB int64) *Manager {
	t.Helper()
	cfg := siblingConfig(t)
	const kibPerGiB = 1024 * 1024
	meminfo := "MemTotal: " + strconv.FormatInt(128*kibPerGiB, 10) + " kB\nMemAvailable: " + strconv.FormatInt(availGiB*kibPerGiB, 10) + " kB\n"
	root := incidentProcRoot(t, -1, 0, meminfo)
	pid := filepath.Join(root, "500")
	put(t, filepath.Join(pid, "comm"), "radiance\n")
	put(t, filepath.Join(pid, "cmdline"), argv)
	put(t, filepath.Join(pid, "status"), "VmRSS:\t"+strconv.FormatInt((anonGiB+fileGiB)*kibPerGiB, 10)+" kB\nRssAnon:\t"+
		strconv.FormatInt(anonGiB*kibPerGiB, 10)+" kB\nRssFile:\t"+strconv.FormatInt(fileGiB*kibPerGiB, 10)+" kB\nRssShmem:\t0 kB\n")
	put(t, filepath.Join(pid, "fdinfo", "3"), "drm-driver:\tamdgpu\ndrm-client-id:\t7\ndrm-memory-gtt:\t"+strconv.FormatInt(gpuGiB*kibPerGiB, 10)+" KiB\n")
	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"))
	sys.mainPIDs["forge-a1"] = 500
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf")
	m, _, _ := newIncidentManager(t, cfg, sys, incidentGPU(t, gpuGiB*incidentGiB, 120*incidentGiB), root, nil)
	return m
}

func radianceArgv(t *testing.T) string {
	t.Helper()
	cfg := testConfig(t)
	var port int
	for name, s := range cfg.Slots {
		if name == "a1" {
			port = s.Port
		}
	}
	f := filepath.Join(t.TempDir(), "a1.yaml")
	put(t, f, "server:\n  host: 127.0.0.1\n  port: "+strconv.Itoa(port)+"\n")
	return "radiance\x00--config\x00" + f + "\x00"
}

func TestMemoryBudgetCountsRadiance2SlotRSS(t *testing.T) {
	m := evictionManager(t, radianceArgv(t), 40, 20, 0, 60)
	b, err := m.MemoryBudget()
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(60) * incidentGiB; b.Ledger.ModelUsed != want {
		t.Errorf("ModelUsed = %d GiB, want 60 (40 GPU + 20 resident)", b.Ledger.ModelUsed/incidentGiB)
	}
}

// 2026-11-27: a 96 GiB model was refused ("64.6 GiB recoverable") because eviction was credited with GPU memory only.
func TestFitPlanEvictionFreesTheHostMemoryToo(t *testing.T) {
	m := evictionManager(t, radianceArgv(t), 40, 20, 0, 6) // nothing free now; GPU alone frees too little for 50 GiB
	plan, err := m.FitPlan("gemma-nothink")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Fits || len(plan.Evict) != 1 || plan.Evict[0] != "a1" {
		t.Fatalf("plan = %+v, want an eviction of a1: it frees 40 GiB of GPU memory and 20 GiB of resident memory", plan)
	}
}

// File-backed pages are already counted as available by the kernel, so evicting their owner frees none of them.
func TestFitPlanEvictionDoesNotCountFileBackedMemory(t *testing.T) {
	m := evictionManager(t, radianceArgv(t), 40, 0, 20, 6)
	plan, err := m.FitPlan("gemma-nothink")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Fits || len(plan.Evict) != 0 {
		t.Fatalf("plan = %+v, want a refusal: only 40 GiB would come back, 6+40-8 = 38 GiB for a 50 GiB model", plan)
	}
}
