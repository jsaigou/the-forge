// SPDX-License-Identifier: Apache-2.0

package collector

// S2 attribution tests: UnitState.GPUBytes must name the non-slot GTT
// holders (ComfyUI, always-on services) while slot units stay at 0 — their
// canonical figure lives on SlotState.MemoryBytes, and consumers sum both
// maps without double-counting.

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
)

// setWithPID is fakeSystemd.set plus a MainPID (S2's attach hook).
func (f *fakeSystemd) setWithPID(unit, active, sub string, pid uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = map[string]UnitState{}
	}
	st := f.states[unit]
	st.ActiveState, st.SubState, st.MainPID = active, sub, pid
	f.states[unit] = st
}

func TestAttachGPUMemoryAttributesNonSlotUnits(t *testing.T) {
	cfg := testConfig(t)
	cfg.Modes["creative"] = config.Mode{Unit: "ai-mode-comfyui"} // service mode → watched

	sys := &fakeSystemd{}
	sys.setWithPID("ai-mode-comfyui", "active", "running", 501)
	sys.set("forge-a1", "active", "running") // slot unit: active but skipped

	proc := writeFakeProc(t, map[int]fakePid{
		501: {comm: "python", fdinfo: map[string]string{
			"3": amdgpuFdinfo("9", "100000", "500000"), // 600 MB total
		}},
	})

	c := New(Options{
		Cfg:      func() *config.Config { return cfg },
		Systemd:  sys,
		GPU:      &GPU{DRMRoot: t.TempDir()},
		Proc:     proc,
		DialPort: func(int) bool { return false },
		BaseURL:  func(int) string { return "http://127.0.0.1:1" },
	})
	snap := c.ProbeNow(context.Background())

	comfy, ok := snap.Units["ai-mode-comfyui"]
	if !ok {
		t.Fatal("ai-mode-comfyui missing from snapshot units")
	}
	if want := int64((100000 + 500000) * 1024); comfy.GPUBytes != want {
		t.Errorf("comfy GPUBytes = %d, want %d (deduped fdinfo)", comfy.GPUBytes, want)
	}

	a1 := snap.Units["forge-a1"]
	if a1.GPUBytes != 0 {
		t.Errorf("slot unit GPUBytes = %d, want 0 (canonical figure is SlotState.MemoryBytes)", a1.GPUBytes)
	}
}
