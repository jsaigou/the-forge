// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

// writeFakeProc creates /proc/<pid>/{comm,cmdline} under root so
// collector.Proc's ByComm/PortArg see a process named comm listening on
// --port port.
func writeFakeProc(t *testing.T, root string, pid int, comm string, port int) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatalf("write comm: %v", err)
	}
	cmdline := comm + "\x00--port\x00" + strconv.Itoa(port) + "\x00"
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
}

// TestKillLingeringOnlyTargetsSlotPorts reproduces the bug found
// live-verifying a real unload against ForgeHost in the Phase 9b parallel-run:
// unloading slot A4 killed the permanent CPU-only embedding service
// (forge-embedding.service, also a "llama-server" process, on its own
// fixed port 8083) as collateral damage. killLingering must only kill
// processes bound to one of the canonical inference-slot ports
// (cfg.Slots[*].Port) — never a process on some other port just because it
// shares the "llama-server"/"vllm" binary name.
func TestKillLingeringOnlyTargetsSlotPorts(t *testing.T) {
	cfg := testConfig(t) // slots: a1=8080, a2=8081, a3=8087, a4=8088

	procRoot := t.TempDir()
	writeFakeProc(t, procRoot, 100, "llama-server", 8088) // a4 — a genuine lingering slot process
	writeFakeProc(t, procRoot, 200, "llama-server", 8083) // embedding — must survive
	writeFakeProc(t, procRoot, 300, "llama-server", 8082) // tts — must survive
	writeFakeProc(t, procRoot, 400, "vllm", 8081)         // a2 — another genuine lingering slot process

	var mu sync.Mutex
	var killed []int
	m, err := NewManager(Deps{
		Cfg:  func() *config.Config { return cfg },
		Sys:  newFakeSys(),
		GPU:  &collector.GPU{DRMRoot: t.TempDir()},
		Proc: collector.Proc{Root: procRoot},
		Kill: func(pid int) error {
			mu.Lock()
			killed = append(killed, pid)
			mu.Unlock()
			return nil
		},
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	m.killLingering(8080, 8081, 8087, 8088) // stopAll's usage: every canonical slot port

	mu.Lock()
	defer mu.Unlock()
	got := map[int]bool{}
	for _, pid := range killed {
		got[pid] = true
	}
	if !got[100] || !got[400] {
		t.Errorf("killed = %v, want 100 and 400 (genuine slot-port processes) killed", killed)
	}
	if got[200] {
		t.Error("killed the embedding service (port 8083) — must be scoped to slot ports only")
	}
	if got[300] {
		t.Error("killed the TTS service (port 8082) — must be scoped to slot ports only")
	}
	if len(killed) != 2 {
		t.Errorf("killed = %v, want exactly [100, 400] in some order", killed)
	}
}

// TestKillLingeringDoesNotTargetOtherSlots reproduces a second, narrower
// version of the same class of bug, found 2026-07-29: killLingering used to
// be scoped to *every* canonical slot port unconditionally, so a single-slot
// Unload() would SIGKILL whatever was running on every *other* slot too, not
// just the one that was actually stopped. Live-reproduced on ForgeHost: loading
// two unrelated modes on two different slots, then unloading just one,
// silently killed and auto-restarted the other. killLingering must only
// consider the port(s) explicitly passed by the caller — Unload(slot) now
// passes just that slot's own port.
func TestKillLingeringDoesNotTargetOtherSlots(t *testing.T) {
	cfg := testConfig(t) // slots: a1=8080, a2=8081, a3=8087, a4=8088

	procRoot := t.TempDir()
	writeFakeProc(t, procRoot, 100, "llama-server", 8088) // a4 — the slot actually being unloaded
	writeFakeProc(t, procRoot, 200, "llama-server", 8080) // a1 — a different, healthy slot
	writeFakeProc(t, procRoot, 300, "vllm", 8081)         // a2 — another different, healthy slot

	var mu sync.Mutex
	var killed []int
	m, err := NewManager(Deps{
		Cfg:  func() *config.Config { return cfg },
		Sys:  newFakeSys(),
		GPU:  &collector.GPU{DRMRoot: t.TempDir()},
		Proc: collector.Proc{Root: procRoot},
		Kill: func(pid int) error {
			mu.Lock()
			killed = append(killed, pid)
			mu.Unlock()
			return nil
		},
		PollInterval: time.Millisecond,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	m.killLingering(8088) // Unload("a4")'s usage: only a4's own port

	mu.Lock()
	defer mu.Unlock()
	if len(killed) != 1 || killed[0] != 100 {
		t.Errorf("killed = %v, want exactly [100] (a4's own lingering process)", killed)
	}
}

// TestLoadRejectsAlreadyLoadedOnAnotherSlot verifies the §0.6
// defense-in-depth guard in engine.Manager.Load: a mode already loaded on
// another slot is rejected before any systemd operations begin. The
// handler-level guard resolves by registry model id; this engine-level
// guard is a mode-name safety net for direct callers (e.g., the scheduler).
func TestLoadRejectsAlreadyLoadedOnAnotherSlot(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	// a1's unit starts active so Load's stop-and-reload path is
	// reachable for the target slot; a2's unit is inactive (free).
	sys.setSeq("forge-a1", st("active", "running"))
	sys.setSeq("forge-a2", st("inactive", "dead"))
	m, _, _ := newTestManager(t, cfg, sys, newLlamaStub(t, 32768))

	// Simulate "gemma" already loaded on a1.
	m.setSlotMode("a1", "gemma")

	// Attempting to load "gemma" on a2 must fail immediately.
	res := m.Load(context.Background(), "gemma", "a2")
	if res.Success {
		t.Fatal("Load must reject when the same mode is loaded on another slot")
	}
	if !strings.Contains(res.Message, "already loaded") {
		t.Errorf("Load message = %q, want it to mention 'already loaded'", res.Message)
	}

	// The target slot's unit must not have been stopped or started —
	// the guard fires before any systemd operation.
	if len(sys.stopped) != 0 {
		t.Errorf("guard must fire before stopping units; stopped = %v", sys.stopped)
	}
	if len(sys.started) != 0 {
		t.Errorf("guard must fire before starting units; started = %v", sys.started)
	}
}

// TestLoadAllowsSameSlotReload verifies the guard does not false-positive
// when loading a mode onto the same slot where it's already tracked — the
// target slot is excluded from the cross-slot check, and Load's existing
// stop-and-reload path handles the rest.
func TestLoadAllowsSameSlotReload(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	// Sequence: active (Load sees it needs to stop) → inactive (waitUnitGone
	// sees it's gone after Stop) → active/running (waitServiceRunning sees
	// it's up after Start).
	sys.setSeq("forge-a1",
		st("active", "running"),
		st("inactive", "dead"),
		st("active", "running"),
	)
	m, _, _ := newTestManager(t, cfg, sys, newLlamaStub(t, 32768))

	// Write the slot env so SlotStates can re-infer the mode after reload.
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf")
	m.setSlotMode("a1", "gemma")

	// Loading "gemma" on a1 (same slot) must not be rejected by
	// the cross-slot guard — it proceeds to the stop-and-reload path.
	res := m.Load(context.Background(), "gemma", "a1")
	if !res.Success {
		t.Fatalf("Load on same slot must succeed, got: %s", res.Message)
	}
}

func TestWaitGTTDrainTimeoutCallback(t *testing.T) {
	// GTT stays elevated past the 20s drain window → OnGTTDrainTimeout fires
	// with the before/after byte figures (the pre-hang/post-unload signal).
	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte(strconv.Itoa(2*1024*1024*1024)+"\n"), 0o644) // 2 GiB, above the 1 GiB baseline

	var gotBefore, gotAfter int64
	cb := func(before, after int64) {
		gotBefore, gotAfter = before, after
	}
	cfg := testConfig(t)
	sys := newFakeSys()
	m, err := NewManager(Deps{
		Cfg:               func() *config.Config { return cfg },
		Sys:               sys,
		GPU:               &collector.GPU{DRMRoot: gpuRoot},
		Proc:              collector.Proc{Root: t.TempDir()},
		PollInterval:      time.Millisecond,
		Logf:              t.Logf,
		OnGTTDrainTimeout: cb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.waitGTTDrain(gttSnapshot{}) {
		t.Fatal("waitGTTDrain reported a drain with GTT stuck at 2 GiB and no snapshot")
	}
	if gotBefore == 0 {
		t.Fatalf("OnGTTDrainTimeout not called (before=%d after=%d)", gotBefore, gotAfter)
	}
	if gotBefore != 2*1024*1024*1024 || gotAfter != 2*1024*1024*1024 {
		t.Errorf("callback args = (%d, %d), want (2GiB, 2GiB)", gotBefore, gotAfter)
	}
}

// gttDrainManager builds a Manager whose GTT counter reads gttBytes, counting OnGTTDrainTimeout calls.
func gttDrainManager(t *testing.T, gttBytes int64) (*Manager, *int) {
	t.Helper()
	gpuRoot := t.TempDir()
	dev := filepath.Join(gpuRoot, "card0", "device")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte(strconv.FormatInt(gttBytes, 10)+"\n"), 0o644)
	timeouts := new(int)
	cfg := testConfig(t)
	m, err := NewManager(Deps{
		Cfg:               func() *config.Config { return cfg },
		Sys:               newFakeSys(),
		GPU:               &collector.GPU{DRMRoot: gpuRoot},
		Proc:              collector.Proc{Root: t.TempDir()},
		PollInterval:      time.Millisecond,
		Logf:              t.Logf,
		OnGTTDrainTimeout: func(_, _ int64) { *timeouts++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, timeouts
}

// The ForgeHost case (measured 2026-11-02): other slots and always-on services keep 12.5 GiB resident, so the absolute floor can
// never be met, but the unloaded slot's 7.5 GiB is gone. That is a drain, with no timeout and no anomaly.
func TestWaitGTTDrain_RelativeDropWithNeighboursResident(t *testing.T) {
	m, timeouts := gttDrainManager(t, 125*gib/10)               // 12.5 GiB left
	snap := gttSnapshot{used: 20 * gib, expected: 15 * gib / 2} // was 20 GiB, slot held 7.5 GiB
	start := time.Now()
	if !m.waitGTTDrain(snap) {
		t.Fatal("a drop of the slot's whole footprint was not seen as a drain")
	}
	if *timeouts != 0 {
		t.Errorf("OnGTTDrainTimeout fired %d times for a clean drain", *timeouts)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("waited %v; the relative check must finish before any poll sleep", time.Since(start))
	}
}

// 80% of the footprint is enough (fdinfo and the GTT counter only roughly agree); well under that is still a timeout.
func TestWaitGTTDrain_PartialDropThreshold(t *testing.T) {
	snap := gttSnapshot{used: 20 * gib, expected: 10 * gib}
	m, timeouts := gttDrainManager(t, 12*gib) // 8 GiB back = exactly 80%
	if !m.waitGTTDrain(snap) || *timeouts != 0 {
		t.Errorf("80%% of the footprint back should count as drained (timeouts=%d)", *timeouts)
	}
	m, timeouts = gttDrainManager(t, 13*gib) // 7 GiB back = 70%
	if m.waitGTTDrain(snap) || *timeouts != 1 {
		t.Errorf("70%% back must still time out and report (timeouts=%d)", *timeouts)
	}
}

// The case the anomaly exists for: the process is gone but GTT did not move.
func TestWaitGTTDrain_NoDropStillTimesOut(t *testing.T) {
	m, timeouts := gttDrainManager(t, 20*gib)
	if m.waitGTTDrain(gttSnapshot{used: 20 * gib, expected: 7 * gib}) {
		t.Fatal("GTT did not move, yet the wait reported a drain")
	}
	if *timeouts != 1 {
		t.Errorf("OnGTTDrainTimeout fired %d times, want 1", *timeouts)
	}
}

// Without a footprint or counter reading nothing is learned, so behavior is the old one: an idle GPU exits at once, a
// busy one times out.
func TestWaitGTTDrain_UnknownFootprintKeepsOldBehaviour(t *testing.T) {
	m, timeouts := gttDrainManager(t, gib/2)
	if !m.waitGTTDrain(gttSnapshot{}) || *timeouts != 0 {
		t.Error("an idle GPU (under 1 GiB) must still end the wait at once")
	}
	m, timeouts = gttDrainManager(t, 12*gib)
	if m.waitGTTDrain(gttSnapshot{used: 20 * gib}) || *timeouts != 1 {
		t.Errorf("a drop with no known footprint cannot be judged; expected the timeout (timeouts=%d)", *timeouts)
	}
}
