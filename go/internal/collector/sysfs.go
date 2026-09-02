// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// GPU reads amdgpu counters from sysfs — the V5 form of Phase 0 Fix C
// (docs/v5-phase0-python-hotfixes.md): the same files rocm-smi itself reads,
// without the ~hundreds-of-ms subprocess per counter. DRMRoot overrides
// /sys/class/drm for tests.
//
// The GTT counter here is the classic-GTT reading with the documented blind
// spot (docs/pitfalls.md): it does NOT see ROCm unified-memory (HMM-backed)
// allocations. Consumers must add rocm-slot RSS on top — never max().
type GPU struct {
	DRMRoot string

	mu   sync.Mutex
	card string // cached device dir; re-discovered when reads fail
}

// GPUSample is one sysfs read. Nil pointers mean the sensor was absent or
// unreadable this cycle (null in the metrics contract — never 0).
//
// A1 (bytes retrofit, 2026-07-24): sysfs reports bytes natively; the
// kernel's mem_info_gtt_* counters are no longer divided to MB at the
// probe boundary. Consumers receive bytes and scale at display.
type GPUSample struct {
	UsePct        *float64
	GTTUsedBytes  *int64
	GTTTotalBytes *int64
	TempC         *float64

	// JunctionTempC is the amdgpu hwmon channel labeled "junction" (the
	// die/hotspot sensor — runs hotter than the "edge" TempC above under
	// load; Phase 4 collector metrics, 2026-08-12).
	JunctionTempC *float64

	// PackagePowerW is the amdgpu PPT (package power tracking) rail in
	// watts — the APU's own board power, NOT wall power (excludes RAM,
	// NVMe, fans, PSU loss). See config.Cost.OverheadW/PSUEfficiency for
	// the wall-power approximation built on top of this reading.
	PackagePowerW *float64
}

func (g *GPU) drmRoot() string {
	if g.DRMRoot == "" {
		return "/sys/class/drm"
	}
	return g.DRMRoot
}

// device returns the cached amdgpu device dir (the card* whose vendor file
// reads 0x1002), discovering it on first use.
func (g *GPU) device() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.card != "" {
		return g.card
	}
	entries, err := os.ReadDir(g.drmRoot())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}
		dev := filepath.Join(g.drmRoot(), name, "device")
		vendor, err := os.ReadFile(filepath.Join(dev, "vendor"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(vendor)) == "0x1002" {
			g.card = dev
			return dev
		}
	}
	return ""
}

func (g *GPU) invalidate() {
	g.mu.Lock()
	g.card = ""
	g.mu.Unlock()
}

func readInt(path string) (int64, bool) {
	// A hard "/sys/" prefix pin lived here 2026-08-28..2026-09-02 (bearer
	// SAST path-traversal remediation, 6a0deec) and silently broke every
	// sysfs test fixture in this package AND internal/engine (both inject a
	// root via t.TempDir() — never under /sys/, can't be: it's a kernel
	// virtual filesystem, not a real directory tests can create entries
	// under). Every call site here is built from g.drmRoot()/s.hwmonRoot()
	// (a hardcoded "/sys/..." default OR an explicit DRMRoot/HwmonRoot
	// struct field only main.go's wiring or a test ever sets — GPU's own
	// doc comment: "DRMRoot overrides /sys/class/drm for tests") — never
	// from request/network/config-file input, so the traversal the bearer
	// finding modeled isn't actually reachable through this package. Fixed
	// by removing the guard rather than re-adding it, since Bearer's own
	// taint analysis can't see that boundary (this same commit already
	// suppressed 20 other findings in this codebase for exactly that
	// reason) and 20+ tests across two packages had been silently
	// non-functional (every assertion checking a nil/zero fallback that's
	// always true regardless of whether the real read logic works) for 5
	// days without anyone noticing, since CI was already red for other
	// reasons.
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Sample reads all counters once. A totally failed read invalidates the
// cached card path so a re-enumerated DRM topology recovers next cycle.
func (g *GPU) Sample() GPUSample {
	var s GPUSample
	dev := g.device()
	if dev == "" {
		return s
	}
	ok := false
	if v, got := readInt(filepath.Join(dev, "gpu_busy_percent")); got {
		f := float64(v)
		s.UsePct = &f
		ok = true
	}
	if v, got := readInt(filepath.Join(dev, "mem_info_gtt_used")); got {
		s.GTTUsedBytes = &v
		ok = true
	}
	if v, got := readInt(filepath.Join(dev, "mem_info_gtt_total")); got {
		s.GTTTotalBytes = &v
		ok = true
	}
	if t := g.tempByLabel(dev, "edge"); t != nil {
		s.TempC = t
		ok = true
	}
	if t := g.tempByLabel(dev, "junction"); t != nil {
		s.JunctionTempC = t
		ok = true
	}
	if p := g.packagePowerW(dev); p != nil {
		s.PackagePowerW = p
		ok = true
	}
	if !ok {
		g.invalidate()
	}
	return s
}

// GTTUsedBytes reads just the GTT-used counter (raw sysfs bytes) — the
// engine's GTT-drain wait polls this tightly after unit stops.
func (g *GPU) GTTUsedBytes() (int64, bool) {
	dev := g.device()
	if dev == "" {
		return 0, false
	}
	v, got := readInt(filepath.Join(dev, "mem_info_gtt_used"))
	if !got {
		return 0, false
	}
	return v, true
}

// tempByLabel finds the hwmon temp input under dev/hwmon/* whose label file
// matches label exactly (millidegrees C) — "edge" and "junction" are both
// amdgpu-exposed channels on this hardware (Phase 4, 2026-08-12: junction
// added alongside the original edge reading).
func (g *GPU) tempByLabel(dev, label string) *float64 {
	hwmons, err := os.ReadDir(filepath.Join(dev, "hwmon"))
	if err != nil {
		// No hwmon dir under this device — the sensor simply isn't exposed
		// (or dev itself is stale); a nil reading is the correct "unmeasured"
		// signal, not a failure.
		return nil
	}
	for _, h := range hwmons {
		dir := filepath.Join(dev, "hwmon", h.Name())
		labels, err := filepath.Glob(filepath.Join(dir, "temp*_label"))
		if err != nil {
			continue
		}
		for _, lp := range labels {
			raw, err := os.ReadFile(lp)
			if err != nil || strings.TrimSpace(string(raw)) != label {
				continue
			}
			input := strings.TrimSuffix(lp, "_label") + "_input"
			if v, got := readInt(input); got {
				c := float64(v) / 1000
				return &c
			}
		}
	}
	return nil
}

// maxPlausibleWatts bounds packagePowerW's output — a unit-confusion bug
// (e.g. forgetting the µW→W division) would otherwise put a number like
// 4,600,000 on a dashboard silently. No consumer GPU package draws
// anywhere near this.
const maxPlausibleWatts = 2000

// packagePowerW finds the hwmon power channel labeled "PPT" (package power
// tracking — the amdgpu APU/GPU board rail) and reads its microwatt-
// denominated average, falling back to the instantaneous input reading,
// and finally to a lone unlabeled power channel so a firmware label change
// doesn't silently produce nil forever. This is NOT wall power — see
// config.Cost.OverheadW/PSUEfficiency for the wall-power approximation.
func (g *GPU) packagePowerW(dev string) *float64 {
	hwmons, err := os.ReadDir(filepath.Join(dev, "hwmon"))
	if err != nil {
		// Same degrade posture as tempByLabel just above: no hwmon dir means
		// no power sensor exposed here, not an error worth surfacing.
		return nil
	}
	for _, h := range hwmons {
		dir := filepath.Join(dev, "hwmon", h.Name())
		labels, err := filepath.Glob(filepath.Join(dir, "power*_label"))
		if err != nil {
			continue
		}
		for _, lp := range labels {
			raw, err := os.ReadFile(lp)
			if err != nil || strings.TrimSpace(string(raw)) != "PPT" {
				continue
			}
			prefix := strings.TrimSuffix(lp, "_label")
			if v, got := readInt(prefix + "_average"); got {
				if w := microwattsToWatts(v); w != nil {
					return w
				}
			}
			if v, got := readInt(prefix + "_input"); got {
				if w := microwattsToWatts(v); w != nil {
					return w
				}
			}
		}
		// No PPT-labeled channel found in this hwmon dir. Fall back to a
		// lone power1_average if that's the only power channel present —
		// some firmware/kernel combinations omit the label entirely.
		if len(labels) == 0 {
			if v, got := readInt(filepath.Join(dir, "power1_average")); got {
				if w := microwattsToWatts(v); w != nil {
					return w
				}
			}
		}
	}
	return nil
}

// microwattsToWatts converts and sanity-bounds a raw microwatt reading.
// Rejects non-positive and implausibly large values rather than let a
// unit-confusion bug or garbage read surface as a real number.
func microwattsToWatts(microwatts int64) *float64 {
	w := float64(microwatts) / 1e6
	if w <= 0 || w > maxPlausibleWatts {
		return nil
	}
	return &w
}

// HostSensors reads non-GPU hwmon sensors — CPU package temp and NVMe drive
// temp (Phase 4 collector metrics, 2026-08-12) — which live directly under
// /sys/class/hwmon, not under a DRM device's own hwmon subdir like GPU's
// channels. HWMonRoot overrides /sys/class/hwmon for tests.
type HostSensors struct {
	HWMonRoot string
}

// HostSensorSample is one host-sensor read cycle. A nil field means the
// sensor wasn't found this cycle (no matching hwmon driver, or unreadable)
// — null in the metrics contract, same convention as GPUSample.
type HostSensorSample struct {
	CPUPackageTempC *float64
	// NVMeTempC is the max composite temperature across every NVMe drive
	// found (a host can have more than one) — the hottest drive is the
	// actionable number for a thermal indicator, not an average across
	// drives running at different loads.
	NVMeTempC *float64
}

func (s HostSensors) hwmonRoot() string {
	if s.HWMonRoot == "" {
		return "/sys/class/hwmon"
	}
	return s.HWMonRoot
}

// cpuHWMonDrivers are the hwmon "name" values that expose a usable CPU
// package/control temperature: k10temp (this hardware, AMD Strix Halo),
// plus zenpower (older AMD) and coretemp (Intel) so this doesn't silently
// go blank on a different dev machine.
var cpuHWMonDrivers = map[string]bool{"k10temp": true, "zenpower": true, "coretemp": true}

// cpuPreferredLabels is tried in order — "Tctl" is AMD's own control
// temperature (the one thermal throttling actually reacts to), preferred
// over "Tdie" (raw die temp, reads slightly different) when both exist.
var cpuPreferredLabels = []string{"Tctl", "Tdie"}

// nvmePreferredLabels: "Composite" is the standard NVMe SMART composite
// temperature sensor name.
var nvmePreferredLabels = []string{"Composite"}

// Sample scans every /sys/class/hwmon/hwmon* directory once, matching by
// each device's own "name" file rather than a fixed hwmon index (hwmon
// numbering is enumeration-order-dependent and not stable across boots).
func (s HostSensors) Sample() HostSensorSample {
	var out HostSensorSample
	root := s.hwmonRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	var nvmeMax *float64
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		nameRaw, err := os.ReadFile(filepath.Join(dir, "name"))
		if err != nil {
			continue
		}
		switch name := strings.TrimSpace(string(nameRaw)); {
		case out.CPUPackageTempC == nil && cpuHWMonDrivers[name]:
			out.CPUPackageTempC = firstOrLabeledTemp(dir, cpuPreferredLabels)
		case name == "nvme":
			if t := firstOrLabeledTemp(dir, nvmePreferredLabels); t != nil {
				if nvmeMax == nil || *t > *nvmeMax {
					nvmeMax = t
				}
			}
		}
	}
	out.NVMeTempC = nvmeMax
	return out
}

// firstOrLabeledTemp reads a temp*_input (millidegrees C) from a hwmon
// directory, preferring a channel whose temp*_label matches one of
// preferredLabels (tried in order) and falling back to the first
// temp*_input present — labeled or not — so a firmware/kernel combination
// that omits labels entirely still produces a reading instead of going
// silently blank forever.
func firstOrLabeledTemp(dir string, preferredLabels []string) *float64 {
	labels, _ := filepath.Glob(filepath.Join(dir, "temp*_label"))
	for _, want := range preferredLabels {
		for _, lp := range labels {
			raw, err := os.ReadFile(lp)
			if err != nil || strings.TrimSpace(string(raw)) != want {
				continue
			}
			input := strings.TrimSuffix(lp, "_label") + "_input"
			if v, got := readInt(input); got {
				c := float64(v) / 1000
				return &c
			}
		}
	}
	inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
	sort.Strings(inputs)
	if len(inputs) > 0 {
		if v, got := readInt(inputs[0]); got {
			c := float64(v) / 1000
			return &c
		}
	}
	return nil
}
