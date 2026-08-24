// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeProc builds a /proc lookalike. pids maps pid → (comm, cmdline args,
// rssKB).
type fakePid struct {
	comm  string
	args  []string
	rssKB int
	// fdinfo maps fd name (e.g. "3") to raw fdinfo file content, for
	// GPUMemoryBytes tests. nil = no fdinfo dir at all.
	fdinfo map[string]string
}

func writeFakeProc(t *testing.T, pids map[int]fakePid) Proc {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte(
		"MemTotal:       131072000 kB\nMemFree:        4096000 kB\nMemAvailable:   65536000 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loadavg"), []byte("2.5 1.8 1.2 3/1024 4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "uptime"), []byte("360000.5 2880000.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for pid, p := range pids {
		dir := filepath.Join(root, itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "comm"), []byte(p.comm+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(p.args, "\x00")+"\x00"), 0o644)
		os.WriteFile(filepath.Join(dir, "status"),
			[]byte("Name:\t"+p.comm+"\nVmRSS:\t"+itoa(p.rssKB)+" kB\n"), 0o644)
		if p.fdinfo != nil {
			fdDir := filepath.Join(dir, "fdinfo")
			if err := os.MkdirAll(fdDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for fd, content := range p.fdinfo {
				os.WriteFile(filepath.Join(fdDir, fd), []byte(content), 0o644)
			}
		}
	}
	return Proc{Root: root}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestProcStats(t *testing.T) {
	p := writeFakeProc(t, nil)
	s := p.Stats()
	if got := s.MemTotalBytes; got != 128000*1024*1024 {
		t.Errorf("MemTotalBytes = %v, want %d", got, 128000*1024*1024)
	}
	if got := s.MemAvailBytes; got != 64000*1024*1024 {
		t.Errorf("MemAvailBytes = %v, want %d", got, 64000*1024*1024)
	}
	if s.Load1 != 2.5 {
		t.Errorf("Load1 = %v", s.Load1)
	}
	if s.UptimeS != 360000.5 {
		t.Errorf("UptimeS = %v", s.UptimeS)
	}
}

func TestProcProcessHelpers(t *testing.T) {
	p := writeFakeProc(t, map[int]fakePid{
		101: {comm: "llama-server", args: []string{"llama-server", "--port", "8080", "-m", "x.gguf"}, rssKB: 90 * 1024 * 1024},
		102: {comm: "llama-server", args: []string{"llama-server", "--port", "8087"}, rssKB: 8 * 1024 * 1024},
		103: {comm: "python3", args: []string{"python3", "comfy.py"}, rssKB: 4 * 1024 * 1024},
	})

	pids := p.ByComm("llama-server")
	if len(pids) != 2 {
		t.Fatalf("ByComm = %v", pids)
	}
	port, ok := p.PortArg(101)
	if !ok || port != 8080 {
		t.Errorf("PortArg(101) = %d, %v", port, ok)
	}
	if _, ok := p.PortArg(103); ok {
		t.Error("PortArg(103) should be absent")
	}
	// rssKB was 90*1024*1024 kB (= 90 GiB). RSSBytes returns kB*1024 → bytes.
	if rss := p.RSSBytes(101); rss != float64(90*1024*1024)*1024 {
		t.Errorf("RSSBytes(101) = %v", rss)
	}
	if rss := p.RSSBytes(999); rss != 0 {
		t.Errorf("RSSBytes(999) = %v", rss)
	}
}

// amdgpuFdinfo builds one amdgpu fdinfo block in the real kernel format
// (ground-truthed live on ForgeHost, kernel 7.1.5, 2026-08-04) with the given
// drm-client-id / vram / gtt (both in KiB, matching the kernel's own unit).
func amdgpuFdinfo(clientID, vramKiB, gttKiB string) string {
	return "pos:\t0\nflags:\t02100002\nmnt_id:\t40\nino:\t694\n" +
		"drm-driver:\tamdgpu\ndrm-client-id:\t" + clientID + "\ndrm-pdev:\t0000:c5:00.0\n" +
		"drm-memory-vram:\t" + vramKiB + " KiB\ndrm-memory-gtt: \t" + gttKiB + " KiB\n" +
		"drm-memory-cpu: \t0 KiB\n"
}

func TestProcGPUMemoryBytes(t *testing.T) {
	p := writeFakeProc(t, map[int]fakePid{
		// Real shape: llama-server opens the DRM device on 2 fds, both
		// reporting the SAME drm-client-id — must count once, not twice.
		201: {comm: "llama-server", fdinfo: map[string]string{
			"3": amdgpuFdinfo("6", "625120", "232960"), // vram 625120 KiB, gtt 232960 KiB
			"4": amdgpuFdinfo("6", "625120", "232960"), // same client, repeated by the kernel
			"0": "pos:\t0\nflags:\t0\n",                // non-GPU fd, no drm-driver line
		}},
		// A second, distinct client on the same process (e.g. a second
		// context) must be counted additively, not overwritten.
		202: {comm: "llama-server", fdinfo: map[string]string{
			"3": amdgpuFdinfo("9", "1000", "2000"),
			"5": amdgpuFdinfo("11", "3000", "4000"),
		}},
		// No fdinfo dir at all (process died between ByComm and the read,
		// or a non-Linux-amdgpu host) → 0, not an error.
		203: {comm: "llama-server"},
	})

	if got, want := p.GPUMemoryBytes(201), (625120+232960)*float64(1024); got != want {
		t.Errorf("GPUMemoryBytes(201) = %v, want %v (deduped by client-id, not doubled)", got, want)
	}
	if got, want := p.GPUMemoryBytes(202), (1000+2000+3000+4000)*float64(1024); got != want {
		t.Errorf("GPUMemoryBytes(202) = %v, want %v (two distinct clients summed)", got, want)
	}
	if got := p.GPUMemoryBytes(203); got != 0 {
		t.Errorf("GPUMemoryBytes(203) = %v, want 0 (no fdinfo dir)", got)
	}
	if got := p.GPUMemoryBytes(999); got != 0 {
		t.Errorf("GPUMemoryBytes(999) = %v, want 0 (unknown pid)", got)
	}
}

func TestGPUSysfs(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "card1", "device")
	hw := filepath.Join(dev, "hwmon", "hwmon3")
	if err := os.MkdirAll(hw, 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-AMD card that must be skipped.
	other := filepath.Join(root, "card0", "device")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "vendor"), []byte("0x10de\n"), 0o644)

	os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "gpu_busy_percent"), []byte("37\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_used"), []byte("96636764160\n"), 0o644)   // raw bytes (was 92160 MB)
	os.WriteFile(filepath.Join(dev, "mem_info_gtt_total"), []byte("128849018880\n"), 0o644) // raw bytes (was 122880 MB)
	os.WriteFile(filepath.Join(hw, "temp1_label"), []byte("edge\n"), 0o644)
	os.WriteFile(filepath.Join(hw, "temp1_input"), []byte("54000\n"), 0o644)
	os.WriteFile(filepath.Join(hw, "temp2_label"), []byte("junction\n"), 0o644)
	os.WriteFile(filepath.Join(hw, "temp2_input"), []byte("99000\n"), 0o644)
	os.WriteFile(filepath.Join(hw, "power1_label"), []byte("PPT\n"), 0o644)
	os.WriteFile(filepath.Join(hw, "power1_average"), []byte("5200000\n"), 0o644) // 5.2W

	g := &GPU{DRMRoot: root}
	s := g.Sample()
	if s.UsePct == nil || *s.UsePct != 37 {
		t.Errorf("UsePct = %v", s.UsePct)
	}
	if s.GTTUsedBytes == nil || *s.GTTUsedBytes != 96636764160 {
		t.Errorf("GTTUsedBytes = %v", s.GTTUsedBytes)
	}
	if s.GTTTotalBytes == nil || *s.GTTTotalBytes != 128849018880 {
		t.Errorf("GTTTotalBytes = %v", s.GTTTotalBytes)
	}
	if s.TempC == nil || *s.TempC != 54 {
		t.Errorf("TempC = %v (must pick edge, not junction)", s.TempC)
	}
	if s.JunctionTempC == nil || *s.JunctionTempC != 99 {
		t.Errorf("JunctionTempC = %v, want 99 (Phase 4)", s.JunctionTempC)
	}
	if s.PackagePowerW == nil || *s.PackagePowerW != 5.2 {
		t.Errorf("PackagePowerW = %v", s.PackagePowerW)
	}

	used, ok := g.GTTUsedBytes()
	if !ok || used != 96636764160 {
		t.Errorf("GTTUsedBytes() = %d, %v", used, ok)
	}
}

func TestGPUAbsentSensors(t *testing.T) {
	g := &GPU{DRMRoot: t.TempDir()} // no cards at all
	s := g.Sample()
	if s.UsePct != nil || s.GTTUsedBytes != nil || s.GTTTotalBytes != nil || s.TempC != nil ||
		s.JunctionTempC != nil || s.PackagePowerW != nil {
		t.Errorf("all fields must be nil (null in the contract, never 0): %+v", s)
	}
}

func TestGPUPackagePowerFallbacks(t *testing.T) {
	newDev := func(t *testing.T) (root, dev, hw string) {
		t.Helper()
		root = t.TempDir()
		dev = filepath.Join(root, "card1", "device")
		hw = filepath.Join(dev, "hwmon", "hwmon3")
		if err := os.MkdirAll(hw, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644)
		return
	}

	t.Run("falls back to power1_input when power1_average missing", func(t *testing.T) {
		root, _, hw := newDev(t)
		os.WriteFile(filepath.Join(hw, "power1_label"), []byte("PPT\n"), 0o644)
		os.WriteFile(filepath.Join(hw, "power1_input"), []byte("6100000\n"), 0o644)
		g := &GPU{DRMRoot: root}
		s := g.Sample()
		if s.PackagePowerW == nil || *s.PackagePowerW != 6.1 {
			t.Errorf("PackagePowerW = %v, want 6.1 (fallback to power1_input)", s.PackagePowerW)
		}
	})

	t.Run("falls back to lone unlabeled power channel", func(t *testing.T) {
		root, _, hw := newDev(t)
		// No power1_label at all — only one power channel present.
		os.WriteFile(filepath.Join(hw, "power1_average"), []byte("4500000\n"), 0o644)
		g := &GPU{DRMRoot: root}
		s := g.Sample()
		if s.PackagePowerW == nil || *s.PackagePowerW != 4.5 {
			t.Errorf("PackagePowerW = %v, want 4.5 (unlabeled fallback)", s.PackagePowerW)
		}
	})

	t.Run("rejects implausibly large reading", func(t *testing.T) {
		root, _, hw := newDev(t)
		os.WriteFile(filepath.Join(hw, "power1_label"), []byte("PPT\n"), 0o644)
		// 4,600,000 W — a unit-confusion-shaped garbage value.
		os.WriteFile(filepath.Join(hw, "power1_average"), []byte("4600000000000\n"), 0o644)
		g := &GPU{DRMRoot: root}
		s := g.Sample()
		if s.PackagePowerW != nil {
			t.Errorf("PackagePowerW = %v, want nil (implausible value rejected)", s.PackagePowerW)
		}
	})

	t.Run("rejects non-positive reading", func(t *testing.T) {
		root, _, hw := newDev(t)
		os.WriteFile(filepath.Join(hw, "power1_label"), []byte("PPT\n"), 0o644)
		os.WriteFile(filepath.Join(hw, "power1_average"), []byte("0\n"), 0o644)
		g := &GPU{DRMRoot: root}
		s := g.Sample()
		if s.PackagePowerW != nil {
			t.Errorf("PackagePowerW = %v, want nil (zero reading rejected)", s.PackagePowerW)
		}
	})

	t.Run("ignores non-PPT label when another power channel exists", func(t *testing.T) {
		root, _, hw := newDev(t)
		os.WriteFile(filepath.Join(hw, "power1_label"), []byte("SPPT\n"), 0o644)
		os.WriteFile(filepath.Join(hw, "power1_average"), []byte("9900000\n"), 0o644)
		g := &GPU{DRMRoot: root}
		s := g.Sample()
		// A label is present but isn't "PPT", and there's more than zero
		// power labels in the dir, so the lone-channel fallback must not
		// fire either — the label existing at all means we trust the
		// label, not a blind single-channel guess.
		if s.PackagePowerW != nil {
			t.Errorf("PackagePowerW = %v, want nil (labeled non-PPT channel must not be guessed)", s.PackagePowerW)
		}
	})
}

// ── Phase 4 collector metrics (2026-08-12): CPU jiffies, net/dev, host sensors ──

func TestProcCPUJiffies(t *testing.T) {
	root := t.TempDir()
	// user nice system idle iowait irq softirq steal guest guest_nice
	stat := "cpu  100 10 50 800 5 2 3 0 0 0\ncpu0 50 5 25 400 2 1 1 0 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Proc{Root: root}
	idleAll, total, ok := p.CPUJiffies()
	if !ok {
		t.Fatal("CPUJiffies ok = false")
	}
	wantIdleAll := uint64(800 + 5)                           // idle+iowait
	wantTotal := uint64(100 + 10 + 50 + 800 + 5 + 2 + 3 + 0) // sum of first 8 fields, guest excluded
	if idleAll != wantIdleAll || total != wantTotal {
		t.Errorf("CPUJiffies = (%d, %d), want (%d, %d)", idleAll, total, wantIdleAll, wantTotal)
	}
}

func TestProcCPUJiffiesMissing(t *testing.T) {
	p := Proc{Root: t.TempDir()}
	if _, _, ok := p.CPUJiffies(); ok {
		t.Error("CPUJiffies ok = true, want false (no /proc/stat)")
	}
}

func TestProcNetDev(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	dev := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo:  123456     100    0    0    0     0          0         0   123456     100    0    0    0     0       0          0\n" +
		"  eth0: 1000000    2000    0    0    0     0          0         0  500000    1000    0    0    0     0       0          0\n" +
		"tailscale0: 200000     300    0    0    0     0          0         0   80000     150    0    0    0     0       0          0\n"
	if err := os.WriteFile(filepath.Join(root, "net", "dev"), []byte(dev), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Proc{Root: root}
	rx, tx, ok := p.NetDev()
	if !ok {
		t.Fatal("NetDev ok = false")
	}
	// lo excluded; eth0 + tailscale0 summed.
	wantRx, wantTx := uint64(1000000+200000), uint64(500000+80000)
	if rx != wantRx || tx != wantTx {
		t.Errorf("NetDev = (%d, %d), want (%d, %d)", rx, tx, wantRx, wantTx)
	}
}

func TestProcNetDevMissing(t *testing.T) {
	p := Proc{Root: t.TempDir()}
	if _, _, ok := p.NetDev(); ok {
		t.Error("NetDev ok = true, want false (no /proc/net/dev)")
	}
}

func TestHostSensorsCPUAndNVMe(t *testing.T) {
	root := t.TempDir()
	// k10temp: labeled Tctl preferred over Tdie.
	cpu := filepath.Join(root, "hwmon0")
	os.MkdirAll(cpu, 0o755)
	os.WriteFile(filepath.Join(cpu, "name"), []byte("k10temp\n"), 0o644)
	os.WriteFile(filepath.Join(cpu, "temp1_label"), []byte("Tdie\n"), 0o644)
	os.WriteFile(filepath.Join(cpu, "temp1_input"), []byte("45000\n"), 0o644)
	os.WriteFile(filepath.Join(cpu, "temp2_label"), []byte("Tctl\n"), 0o644)
	os.WriteFile(filepath.Join(cpu, "temp2_input"), []byte("48000\n"), 0o644)

	// Two NVMe drives — max across them wins.
	nvme1 := filepath.Join(root, "hwmon1")
	os.MkdirAll(nvme1, 0o755)
	os.WriteFile(filepath.Join(nvme1, "name"), []byte("nvme\n"), 0o644)
	os.WriteFile(filepath.Join(nvme1, "temp1_label"), []byte("Composite\n"), 0o644)
	os.WriteFile(filepath.Join(nvme1, "temp1_input"), []byte("38000\n"), 0o644)

	nvme2 := filepath.Join(root, "hwmon2")
	os.MkdirAll(nvme2, 0o755)
	os.WriteFile(filepath.Join(nvme2, "name"), []byte("nvme\n"), 0o644)
	os.WriteFile(filepath.Join(nvme2, "temp1_label"), []byte("Composite\n"), 0o644)
	os.WriteFile(filepath.Join(nvme2, "temp1_input"), []byte("52000\n"), 0o644)

	// An unrelated hwmon device that must be ignored.
	other := filepath.Join(root, "hwmon3")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "name"), []byte("iwlwifi_1\n"), 0o644)
	os.WriteFile(filepath.Join(other, "temp1_input"), []byte("99000\n"), 0o644)

	s := HostSensors{HWMonRoot: root}.Sample()
	if s.CPUPackageTempC == nil || *s.CPUPackageTempC != 48 {
		t.Errorf("CPUPackageTempC = %v, want 48 (Tctl preferred over Tdie)", s.CPUPackageTempC)
	}
	if s.NVMeTempC == nil || *s.NVMeTempC != 52 {
		t.Errorf("NVMeTempC = %v, want 52 (max across drives)", s.NVMeTempC)
	}
}

func TestHostSensorsUnlabeledFallback(t *testing.T) {
	root := t.TempDir()
	cpu := filepath.Join(root, "hwmon0")
	os.MkdirAll(cpu, 0o755)
	os.WriteFile(filepath.Join(cpu, "name"), []byte("zenpower\n"), 0o644)
	// No temp*_label at all — must still find temp1_input.
	os.WriteFile(filepath.Join(cpu, "temp1_input"), []byte("41000\n"), 0o644)

	s := HostSensors{HWMonRoot: root}.Sample()
	if s.CPUPackageTempC == nil || *s.CPUPackageTempC != 41 {
		t.Errorf("CPUPackageTempC = %v, want 41 (unlabeled fallback)", s.CPUPackageTempC)
	}
}

func TestHostSensorsAbsent(t *testing.T) {
	s := HostSensors{HWMonRoot: t.TempDir()}.Sample()
	if s.CPUPackageTempC != nil || s.NVMeTempC != nil {
		t.Errorf("all fields must be nil when no matching hwmon driver exists: %+v", s)
	}
}
