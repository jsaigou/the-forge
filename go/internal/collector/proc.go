// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Proc reads /proc. Root overrides the mount point for tests ("" = /proc).
// Exported because the engine (same track) reuses the process helpers for
// lingering-PID cleanup and unified-memory RSS accounting.
type Proc struct {
	Root string
}

func (p Proc) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

// ProcStats is one cycle's host-level /proc read.
//
// A1 (bytes retrofit): /proc/meminfo reports KiB; the probe now emits bytes
// (kiB × 1024) rather than MB. RAM figures scale at display boundaries.
type ProcStats struct {
	MemTotalBytes float64
	MemAvailBytes float64
	Load1         float64
	UptimeS       float64
}

// Stats reads meminfo, loadavg, and uptime. Fields that fail to parse stay
// zero (UptimeS zero maps to null in the metrics contract).
func (p Proc) Stats() ProcStats {
	var s ProcStats
	if raw, err := os.ReadFile(filepath.Join(p.root(), "meminfo")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			kb, err := strconv.ParseFloat(f[1], 64)
			if err != nil {
				continue
			}
			switch f[0] {
			case "MemTotal:":
				s.MemTotalBytes = kb * 1024
			case "MemAvailable:":
				s.MemAvailBytes = kb * 1024
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(p.root(), "loadavg")); err == nil {
		if f := strings.Fields(string(raw)); len(f) >= 1 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(p.root(), "uptime")); err == nil {
		if f := strings.Fields(string(raw)); len(f) >= 1 {
			s.UptimeS, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	return s
}

// CPUJiffies reads the aggregate "cpu " line of /proc/stat and returns the
// idle (idle+iowait) and total jiffie counts. A single read is meaningless —
// callers must diff two readings over an interval to get a utilization pct
// (Phase 4 collector metrics: CPU.Pct used to be load1/nproc*100, which is
// load average, not utilization). Fields per proc(5): user nice system idle
// iowait irq softirq steal guest guest_nice. guest/guest_nice are excluded
// from total — the kernel already folds guest time into user/nice on the
// host accounting side, so including them would double-count.
func (p Proc) CPUJiffies() (idleAll, total uint64, ok bool) {
	raw, err := os.ReadFile(filepath.Join(p.root(), "stat"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 9 {
			return 0, 0, false
		}
		vals := make([]uint64, 8)
		for i := 0; i < 8; i++ {
			v, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			vals[i] = v
		}
		idleAll = vals[3] + vals[4]
		for _, v := range vals {
			total += v
		}
		return idleAll, total, true
	}
	return 0, 0, false
}

// NetDev reads /proc/net/dev and sums receive/transmit byte counters across
// every interface except loopback. A single read is a cumulative
// since-interface-up counter, not a rate — callers must diff two readings
// over a known interval (Phase 4 collector metrics: network throughput was
// entirely absent before this). Interfaces are summed rather than picking
// "the" NIC because this host's real traffic (LAN + Tailscale) legitimately
// spans more than one interface; this can double-count on hosts with
// bridged/veth virtual interfaces, which this deployment doesn't have.
func (p Proc) NetDev() (rxBytes, txBytes uint64, ok bool) {
	raw, err := os.ReadFile(filepath.Join(p.root(), "net", "dev"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue // header lines carry no colon
		}
		iface := strings.TrimSpace(line[:idx])
		if iface == "" || iface == "lo" {
			continue
		}
		f := strings.Fields(line[idx+1:])
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rxBytes += rx
		txBytes += tx
		ok = true
	}
	return rxBytes, txBytes, ok
}

// ByComm returns PIDs whose /proc/<pid>/comm equals name (pgrep -x port).
func (p Proc) ByComm(name string) []int {
	entries, err := os.ReadDir(p.root())
	if err != nil {
		// /proc unreadable (non-Linux dev host, sandboxed environment) —
		// degrade to "nothing found" rather than fail; every caller already
		// treats an empty result as "not running".
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(p.root(), e.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(raw)) == name {
			pids = append(pids, pid)
		}
	}
	return pids
}

// PortArg returns the value following "--port" in the process's cmdline
// (how V4 attributed llama-server processes to slots).
func (p Proc) PortArg(pid int) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return 0, false
	}
	args := strings.Split(string(raw), "\x00")
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			port, err := strconv.Atoi(args[i+1])
			if err != nil {
				return 0, false
			}
			return port, true
		}
	}
	return 0, false
}

// RSSBytes returns the process's VmRSS in bytes, 0 when unreadable.
// /proc/<pid>/status reports VmRSS in KiB; the probe multiplies once at the
// boundary (A1 bytes retrofit).
func (p Proc) RSSBytes(pid int) float64 {
	raw, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			if f := strings.Fields(line); len(f) >= 2 {
				kb, err := strconv.ParseFloat(f[1], 64)
				if err == nil {
					return kb * 1024
				}
			}
			return 0
		}
	}
	return 0
}

// GPUMemoryBytes returns pid's real live GPU memory footprint (VRAM + GTT),
// read from the amdgpu driver's own per-process accounting in
// /proc/<pid>/fdinfo (drm-memory-vram/drm-memory-gtt, kernel ≥5.19). Unlike
// the curated catalog "weights on disk" estimate (registry.WeightEstimateBytes),
// this is the actual allocation — it includes KV cache, activation buffers,
// and everything else the model process holds, live.
//
// A process opens the DRM device on more than one fd (llama-server: 2, one
// per GPU context) — all fds from the same open share one "drm-client-id"
// and the kernel repeats that client's totals on every one of them. Summing
// across fds would double- (or triple-, or...) count the same memory, so
// this dedupes by drm-client-id and keeps one reading per unique client.
func (p Proc) GPUMemoryBytes(pid int) float64 {
	dir := filepath.Join(p.root(), strconv.Itoa(pid), "fdinfo")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	seen := map[string]bool{}
	var total float64
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		text := string(raw)
		if !strings.Contains(text, "drm-driver:\tamdgpu") {
			continue
		}
		clientID := ""
		var vram, gtt float64
		for _, line := range strings.Split(text, "\n") {
			switch {
			case strings.HasPrefix(line, "drm-client-id:"):
				clientID = strings.TrimSpace(strings.TrimPrefix(line, "drm-client-id:"))
			case strings.HasPrefix(line, "drm-memory-vram:"):
				vram = parseFdinfoKiB(line, "drm-memory-vram:")
			case strings.HasPrefix(line, "drm-memory-gtt:"):
				gtt = parseFdinfoKiB(line, "drm-memory-gtt:")
			}
		}
		if clientID == "" || seen[clientID] {
			continue
		}
		seen[clientID] = true
		total += vram + gtt
	}
	return total
}

// parseFdinfoKiB extracts the "<N> KiB"-style value after prefix from an
// fdinfo line (the drm-memory-* fields are reported in KiB — same convention
// /proc/<pid>/status uses for VmRSS).
func parseFdinfoKiB(line, prefix string) float64 {
	f := strings.Fields(strings.TrimPrefix(line, prefix))
	if len(f) < 1 {
		return 0
	}
	kb, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}
