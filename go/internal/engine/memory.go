// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

// WeightIdentity resolves a mode's loadable-weight identity: the canonical
// on-disk path of its first service's model file plus its mmproj when
// present. Sibling configs that point at the same GGUF through different
// catalog rows resolve identically (the gemma4-26b-a4b / -nothink pair even
// carried duplicate artifact rows — row IDs are not identity; files are).
// "" means unresolvable, which disables same-weights checks for that mode
// rather than guessing.
func (m *Manager) WeightIdentity(modeName string) string {
	cfg := m.d.Cfg()
	mode, ok := cfg.Modes[modeName]
	if !ok || len(mode.Services) == 0 || mode.Services[0].Model == "" {
		return ""
	}
	svc := mode.Services[0]
	id := filepath.Clean(cfg.Paths.ResolveModelPath(svc.Model))
	if svc.MMProj != "" {
		id += "|" + filepath.Clean(cfg.Paths.ResolveModelPath(svc.MMProj))
	}
	return id
}

// MemoryBudget implements Engine (port of engine.memory_budget): live
// unified-GTT budget. Used is the additive inference-RSS the metrics report
// — gtt_used (whole-GPU floor: Vulkan slots, vLLM, ComfyUI, every
// classic-GTT consumer) PLUS slot llama-server RSS for every backend.
//
// 2026-08-22 incident fix: RSS used to be counted for ROCm slots only, so
// vulkan builds' entire footprint was invisible to scheduling decisions and
// two sibling loads of one GGUF passed a fit check that could not physically
// be satisfied (the host wedged; power cycle required). Counting every
// backend's RSS is deliberately conservative — where a page is both
// GTT-resident and process-mapped it double-counts, erring toward refusing
// loads instead of over-committing unified memory.
//
// FreeBytes is additionally capped by kernel-reported host headroom
// (/proc/meminfo MemAvailable minus hostReserveBytes): on Strix Halo GTT
// pages ARE system RAM, so when the GPU counter lags materializing
// allocations the kernel's own availability figure still bounds what a new
// load may claim.
//
// Probes live rather than reading a snapshot: this feeds scheduling
// decisions, where collector staleness right after an unload mis-sizes the
// budget (the Phase 0 acceptance criteria kept the same rule in V4).
//
// A1 (bytes retrofit, 2026-07-24): the budget is bytes-native. sysfs
// reports bytes; there is no MB conversion at the probe boundary.
func (m *Manager) MemoryBudget() (Budget, error) {
	ctx := context.Background()
	cfg := m.d.Cfg()

	sample := m.d.GPU.Sample()
	var total, used float64
	if sample.GTTTotalBytes != nil {
		total = float64(*sample.GTTTotalBytes)
	}
	if sample.GTTUsedBytes != nil {
		used = float64(*sample.GTTUsedBytes)
	}

	slots := m.reconcileLive(ctx)
	used += m.slotRSSBytes(cfg, slots) // ADDITIVE — never max()

	free := total - used

	// Host-headroom cap: never plan into the reserve the kernel and
	// non-forge services need to keep the box responsive (the crash
	// signature was swap-thrash into a hard amdgpu/KFD wedge, not a clean
	// OOM kill). A failed meminfo read skips the cap — the GTT math above
	// still applies.
	if st := m.d.Proc.Stats(); st.MemAvailBytes > 0 {
		hostFree := st.MemAvailBytes - float64(hostReserveBytes)
		if free > hostFree {
			free = hostFree
		}
	}
	if free < 0 {
		free = 0
	}
	return Budget{TotalBytes: int64(total), UsedBytes: int64(used), FreeBytes: int64(free)}, nil
}

// hostReserveBytes is system RAM the fit gate refuses to plan into: the
// kernel, always-on services (embedding/STT/aligner/compress proxies), and
// page-cache headroom live here. Eating into it is how 2026-08-22 turned
// "model won't fit" into "whole server needs a power cycle".
const hostReserveBytes int64 = 8 << 30 // 8 GiB

// slotRSSBytes sums llama-server VmRSS across every loaded inference slot,
// regardless of backend. (Was unifiedRSSBytes, which counted ROCm slots
// only — see the MemoryBudget doc comment for the 2026-08-22 incident this
// caused.) Matching is by llama-server --port argument so auxiliary
// processes are excluded.
func (m *Manager) slotRSSBytes(cfg *config.Config, slots map[string]collector.SlotAssignment) float64 {
	ports := map[int]bool{}
	for slotName, a := range slots {
		if a.Mode == "" {
			continue
		}
		slot, ok := cfg.Slots[slotName]
		if !ok {
			continue
		}
		mode, ok := cfg.Modes[a.Mode]
		if !ok || len(mode.Services) == 0 {
			continue
		}
		ports[slot.Port] = true
	}
	if len(ports) == 0 {
		return 0
	}
	var total float64
	for _, pid := range m.d.Proc.ByComm("llama-server") {
		if port, ok := m.d.Proc.PortArg(pid); ok && ports[port] {
			total += m.d.Proc.RSSBytes(pid)
		}
	}
	return total
}

// modeWeightBytes is the on-disk weight of a mode's first service (fallback
// memory estimate when the registry has no curated figure).
func modeWeightBytes(cfg *config.Config, modeName string) int64 {
	mode, ok := cfg.Modes[modeName]
	if !ok || len(mode.Services) == 0 {
		return 0
	}
	svc := mode.Services[0]
	model, mmproj := "", ""
	if svc.Model != "" {
		model = cfg.Paths.ResolveModelPath(svc.Model)
	}
	if svc.MMProj != "" {
		mmproj = cfg.Paths.ResolveModelPath(svc.MMProj)
	}
	return collector.WeightSetSizeBytes(model, mmproj, cfg.Paths.ModelsDir)
}

// slotFootprintBytes estimates memory recovered by evicting a slot. The
// slot's LIVE measured GPU footprint wins whenever it can be read (the
// unit's MainPID → Proc.GPUMemoryBytes, the same fdinfo accounting the
// collector's slot_memory_bytes reports): a model's real footprint
// routinely exceeds its on-disk weight set — MTP draft state, KV cache,
// activation buffers, unified-memory overhead. The Sprint 6 build_refresh
// eval measured exactly this live (2026-08-20): a vulkan canary's 16.8 GB
// weight set was crediting an eviction while its real footprint was
// 52.9 GB, and the 36 GB under-credit made the fit check refuse a load
// that the eviction demonstrably made room for. The on-disk weight set is
// the fallback when the slot isn't running or nothing is measurable —
// never a guess, never more than the evidence supports.
func (m *Manager) slotFootprintBytes(cfg *config.Config, slot string) int64 {
	if live := m.liveSlotFootprintBytes(cfg, slot); live > 0 {
		return live
	}
	env := m.slotEnv(slot)
	return collector.WeightSetSizeBytes(env["FORGE_MODEL_PATH"], env["FORGE_MMPROJ"], cfg.Paths.ModelsDir)
}

// liveSlotFootprintBytes returns the running slot process's real GPU
// footprint, or 0 when any link in the chain is missing (unknown slot, no
// unit, unit stopped, no PID, Proc unable to read fdinfo). 0 sends the
// caller to the weight-set fallback.
func (m *Manager) liveSlotFootprintBytes(cfg *config.Config, slot string) int64 {
	sc, ok := cfg.Slots[slot]
	if !ok || sc.Unit == "" || m.d.Sys == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pid, err := m.d.Sys.MainPID(ctx, sc.Unit)
	if err != nil || pid == 0 {
		return 0
	}
	return int64(m.d.Proc.GPUMemoryBytes(int(pid)))
}

// SlotFootprintBytes is the exported wrapper the scheduler's optional
// sched.Footprints seam consumes for exact smallest-footprint-first
// eviction ordering (see the Phase 5 progress entry's integrator request).
func (m *Manager) SlotFootprintBytes(slot string) int64 {
	return m.slotFootprintBytes(m.d.Cfg(), slot)
}

// Plan is the eviction-aware fit check (port of engine.can_fit +
// place_model, which the frozen CanFit shape doesn't carry). The scheduler
// (Phase 5) consumes this; CanFit adapts it to Contract 2.
//
// A1 (bytes retrofit): all figures are bytes.
type Plan struct {
	Fits        bool
	NeedBytes   float64
	FreeBytes   float64
	Slot        string   // proposed placement ("" when nothing fits)
	Evict       []string // slots to evict first, smallest footprint first
	EvictComfyUI bool    // stopping ComfyUI is also required to fit
	ComfyUIBytes float64 // ComfyUI's measured footprint (when EvictComfyUI)
	Message     string
}

// modeNeedEstimate is FitPlan's need-bytes derivation for any mode: a
// fresh profiled safe-memory figure first (weights + KV cache at max
// context, measured), then the catalog's curated safe_memory_bytes, then
// the on-disk weight set. ok=false means no defensible figure exists.
// Used by FitPlan for its own mode and by the in-flight reservation logic,
// which must reserve a loading slot's eventual footprint before any of it
// is measurable (the 2026-08-22 crash admitted a second load while the
// first sibling's pages were still materializing).
func (m *Manager) modeNeedEstimate(cfg *config.Config, modeName string) (int64, bool) {
	if _, ok := cfg.Modes[modeName]; !ok {
		return 0, false
	}
	if m.d.ProfileBytes != nil {
		if b, ok := m.d.ProfileBytes(modeName); ok && b > 0 {
			return b, true
		}
	}
	if m.d.WeightEstimateBytes != nil {
		configID := cfg.Modes[modeName].ConfigID
		if configID != 0 {
			if b, ok := m.d.WeightEstimateBytes(configID); ok && b > 0 {
				return b, true
			}
		}
	}
	if w := modeWeightBytes(cfg, modeName); w > 0 {
		return w, true
	}
	return 0, false
}

// FitPlan reports whether modeName fits now and, if not, which loaded slots
// would need evicting — smallest-footprint-first (free the least memory
// necessary; idle-aware ordering is the scheduler's layer on top).
func (m *Manager) FitPlan(modeName string) (Plan, error) {
	cfg := m.d.Cfg()
	if _, ok := cfg.Modes[modeName]; !ok {
		return Plan{Message: fmt.Sprintf("Unknown mode: %s", modeName)}, nil
	}

	weightBytes, weightKnown := m.modeNeedEstimate(cfg, modeName)

	budget, err := m.MemoryBudget()
	if err != nil {
		return Plan{}, err
	}
	free := float64(budget.FreeBytes)

	// Fail closed (F3): a memory requirement that can't be derived from
	// either the curated registry or an on-disk weight size must never be
	// treated as fitting — the old needBytes==0 <= free path always returned
	// Fits:true and silently over-subscribed. Refuse instead of guessing.
	if !weightKnown {
		m.logf("fit %s: REFUSED — no memory requirement derivable (free=%.1f GiB)", modeName, free/(1<<30))
		return Plan{NeedBytes: 0, FreeBytes: free, Message: fmt.Sprintf(
			"cannot determine memory requirement for %s: no curated memory_req_bytes and model weights not found on disk — refusing to load without a size estimate",
			modeName)}, nil
	}

	// needBytes is weight-only (curated safe_memory_bytes, or on-disk file
	// size) unless a real PROFILE measurement exists, in which case that
	// figure already includes KV cache at max context and is used as-is.
	//
	// This used to also add a flat 0.125 MiB/token KV-cache guess on top for
	// unprofiled modes (F3/BE-2, docs/v5-review-fixes.md). That guess is
	// calibrated for a dense/GQA attention model; on Nemotron's hybrid
	// Mamba2/Attention modes (nemotron, nemotron-puzzle — 1M configured
	// context) it inflated needBytes by ~24x (found live 2026-07-25: computed
	// 176 GB / 219 GB needed for loads that in fact use ~90 GB / ~108 GB —
	// exactly the risk BE-2's own review flagged and never live-verified).
	// Removed: an unprofiled mode is no longer gated on a guess, only on a
	// real weight size or real profile data. The "can't size it at all"
	// refusal above is unaffected — that's the actually-sound half of F3.
	needBytes := float64(weightBytes)

	slots := m.reconcileLive(context.Background())
	var freeSlots []string
	for _, name := range m.Slots() {
		if slots[name].Mode == "" && slots[name].Unloading == nil && slots[name].Loading == nil {
			freeSlots = append(freeSlots, name)
		}
	}

	// In-flight reservation: a load that has started but not yet finished
	// (Loading transition, no process to probe yet) reserves its full
	// estimate here, so back-to-back requests cannot both admit against
	// memory only one of them will get. This is the exact 2026-08-22 race:
	// the second sibling GGUF load passed a fit check evaluated while the
	// first sibling's pages were still materializing.
	inFlight := 0.0
	for _, name := range m.Slots() {
		a := slots[name]
		if a.Loading != nil && a.Loading.Mode != "" && a.Loading.Mode != modeName {
			if est, ok := m.modeNeedEstimate(cfg, a.Loading.Mode); ok {
				inFlight += float64(est)
				m.logf("fit %s: reserving %.1f GiB for in-flight load of %s on %s",
					modeName, float64(est)/(1<<30), a.Loading.Mode, name)
			}
		}
	}
	effectiveFree := free - inFlight

	needGiB, freeGiB := needBytes/(1<<30), effectiveFree/(1<<30)

	if needBytes <= effectiveFree {
		m.logf("fit %s: OK (need %.1f GiB <= free %.1f GiB)", modeName, needGiB, freeGiB)
		p := Plan{Fits: true, NeedBytes: needBytes, FreeBytes: effectiveFree, Message: "Fits now"}
		if len(freeSlots) > 0 {
			p.Slot = freeSlots[0]
		}
		return p, nil
	}

	// Evict smallest-footprint-first until the budget covers the need.
	type cand struct {
		slot      string
		footprint int64
	}
	var candidates []cand
	for _, name := range m.Slots() {
		if slots[name].Mode != "" {
			candidates = append(candidates, cand{name, m.slotFootprintBytes(cfg, name)})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].footprint != candidates[j].footprint {
			return candidates[i].footprint < candidates[j].footprint
		}
		return candidates[i].slot < candidates[j].slot
	})

	var evict []string
	recovered := 0.0
	for _, cd := range candidates {
		if effectiveFree+recovered >= needBytes {
			break
		}
		evict = append(evict, cd.slot)
		recovered += float64(cd.footprint)
	}

	// ComfyUI as an eviction candidate (S1, operator feedback F3): its GTT
	// footprint counts against the same budget, so when the loaded slots
	// alone cannot free enough — and only then — the plan offers stopping
	// ComfyUI as additional recovery. The SCHEDULER decides whether to act
	// on it (opt-out config, comfyui reservations, queue-idle check) and
	// executes the stop; the engine only measures and reports.
	comfyBytes := 0.0
	if effectiveFree+recovered < needBytes && m.d.ComfyUIFootprintBytes != nil {
		if fp := m.d.ComfyUIFootprintBytes(); fp > 0 {
			comfyBytes = float64(fp)
			recovered += comfyBytes
		}
	}

	if effectiveFree+recovered >= needBytes {
		if comfyBytes > 0 {
			m.logf("fit %s: OK after evicting %v + stopping ComfyUI (%.1f GiB) (need %.1f GiB, free %.1f GiB + %.1f GiB recovered)",
				modeName, evict, comfyBytes/(1<<30), needGiB, freeGiB, (recovered-comfyBytes)/(1<<30))
			p := Plan{NeedBytes: needBytes, FreeBytes: effectiveFree, Evict: evict,
				EvictComfyUI: true, ComfyUIBytes: comfyBytes,
				Message: fmt.Sprintf("Needs %v evicted and ComfyUI stopped to fit", evict)}
			if len(evict) > 0 {
				p.Slot = evict[0] // land in the first slot eviction frees
			} else if len(freeSlots) > 0 {
				// Memory was the only blocker — a bay is already open.
				p.Fits = true
				p.Slot = freeSlots[0]
				p.Message = "Fits after ComfyUI is stopped"
			}
			return p, nil
		}
		m.logf("fit %s: OK after evicting %v (need %.1f GiB, free %.1f GiB + %.1f GiB recovered)",
			modeName, evict, needGiB, freeGiB, recovered/(1<<30))
		p := Plan{NeedBytes: needBytes, FreeBytes: effectiveFree, Evict: evict,
			Message: fmt.Sprintf("Needs %v evicted to fit", evict)}
		p.Slot = evict[0] // land in the first slot eviction frees
		return p, nil
	}
	m.logf("fit %s: REFUSED (need %.1f GiB > free %.1f GiB + %.1f GiB recoverable — nothing evicts far enough)",
		modeName, needGiB, freeGiB, recovered/(1<<30))
	msg := fmt.Sprintf(
		"not enough VRAM to load %s: it needs %.1f GiB but only %.1f GiB is available, and evicting every loaded model (%.1f GiB recoverable) still leaves too little",
		modeName, needGiB, freeGiB, recovered/(1<<30))
	if comfyBytes > 0 {
		msg += " — even stopping ComfyUI would not free enough"
	} else if m.d.ComfyUIFootprintBytes == nil || m.d.ComfyUIFootprintBytes() == 0 {
		msg += "; no other GPU memory holder can be freed"
	}
	return Plan{NeedBytes: needBytes, FreeBytes: effectiveFree, Message: msg}, nil
}

// CanFit implements Engine, adapting FitPlan to the frozen Contract 2 shape.
func (m *Manager) CanFit(modeName string) (CanFit, error) {
	plan, err := m.FitPlan(modeName)
	if err != nil {
		return CanFit{}, err
	}
	out := CanFit{
		Fits:          plan.Fits,
		RequiredBytes: int64(plan.NeedBytes),
		FreeBytes:     int64(plan.FreeBytes),
	}
	if !plan.Fits {
		out.Reason = plan.Message
	}
	return out, nil
}
