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
	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/memledger"
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
	b, _, _ := m.probeMemory()
	return b, nil
}

// probeMemory is MemoryBudget together with the policy and measurements it was computed from, so a fit check can ask what the ledger would
// say with a slot's holdings given back.
func (m *Manager) probeMemory() (Budget, memledger.Policy, memledger.Inputs) {
	ctx := context.Background()
	cfg := m.d.Cfg()

	sample := m.d.GPU.Sample()
	var total, gttUsed float64
	if sample.GTTTotalBytes != nil {
		total = float64(*sample.GTTTotalBytes)
	}
	if sample.GTTUsedBytes != nil {
		gttUsed = float64(*sample.GTTUsedBytes)
	}

	slots := m.reconcileLive(ctx)
	slotRSS := m.slotRSSBytes(cfg, slots)
	used := gttUsed + slotRSS // ADDITIVE — never max()

	var declared int64
	if m.d.SpecialistDeclaredBytes != nil {
		declared = m.d.SpecialistDeclaredBytes()
	}
	st := m.d.Proc.Stats()
	in := memledger.Inputs{
		HostTotal: st.MemTotalBytes, HostAvail: st.MemAvailBytes,
		SwapTotal: st.SwapTotalBytes, SwapUsed: st.SwapTotalBytes - st.SwapFreeBytes,
		GTTUsed: int64(gttUsed), GTTTotal: int64(total),
		SlotRSS: slotRSS, SpecialistGPU: m.specialistGPUBytes(ctx, cfg), SpecialistDeclared: declared,
	}
	policy := memledger.Policy{
		OSFloorBytes: cfg.Memory.OSFloorBytes, GrowthMarginBytes: cfg.Memory.GrowthMarginBytes, HostFloorBytes: cfg.Memory.HostFloorBytes,
	}
	l := memledger.Compute(policy, in)

	// Memory still owed to specialists that should be running and are not is reported for the refusal message only. It is NOT subtracted again:
	// every enabled always-on specialist's declared peak is already outside the model budget, running or not (ADR-0027).
	var reserved int64
	if m.d.ReservedBytes != nil {
		reserved = m.d.ReservedBytes()
	}
	return Budget{TotalBytes: int64(total), UsedBytes: int64(used), FreeBytes: l.Free, ReservedBytes: reserved, Ledger: l}, policy, in
}

// specialistGPUBytes is the measured GPU footprint of the specialist pool's processes: GTT that the whole-GPU counter includes but that is not a
// model's. A unit that is stopped, or whose footprint cannot be read, contributes zero (which only makes the model figure larger: conservative).
func (m *Manager) specialistGPUBytes(ctx context.Context, cfg *config.Config) float64 {
	if m.d.Sys == nil {
		return 0
	}
	var total float64
	for _, slot := range cfg.SpecialistSlots {
		if slot.Unit == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		pid, err := m.d.Sys.MainPID(cctx, slot.Unit)
		cancel()
		if err != nil || pid == 0 {
			continue
		}
		total += float64(m.d.Proc.GPUMemoryBytes(int(pid)))
	}
	return total
}

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
	for _, pid := range m.slotPIDs(ports) {
		total += m.d.Proc.RSSBytes(pid)
	}
	return total
}

// slotPIDs returns the inference processes serving one of the ports.
func (m *Manager) slotPIDs(ports map[int]bool) []int {
	var pids []int
	for _, name := range enginekind.ProcessNames() {
		for _, pid := range m.d.Proc.ByComm(name) {
			if port, ok := m.d.Proc.PortArg(pid); ok && ports[port] {
				pids = append(pids, pid)
			}
		}
	}
	return pids
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

// slotHolding is what a loaded slot would give back if it were evicted, in the terms the ledger counts it.
type slotHolding struct {
	gpu     float64 // the process's GPU memory (inside the whole-GPU GTT counter)
	rss     float64 // its resident set, which the ledger counts beside the GPU memory
	private float64 // the part of rss that raises MemAvailable when the process exits
}

// total is the slot's footprint as the model budget charges it; it orders eviction candidates.
func (h slotHolding) total() int64 { return int64(h.gpu + h.rss) }

// returnedTo is the measurements as they would read after the holding is freed.
func (h slotHolding) returnedTo(in memledger.Inputs) memledger.Inputs {
	in.GTTUsed -= int64(h.gpu)
	if in.GTTUsed < 0 {
		in.GTTUsed = 0
	}
	in.SlotRSS -= h.rss
	if in.SlotRSS < 0 {
		in.SlotRSS = 0
	}
	if in.HostAvail > 0 {
		in.HostAvail += h.gpu + h.private
		if in.HostTotal > 0 && in.HostAvail > in.HostTotal {
			in.HostAvail = in.HostTotal
		}
	}
	return in
}

// slotHolding measures what evicting a slot recovers. The slot's LIVE figures win whenever they can be read: the GPU memory of the unit's
// MainPID (the fdinfo accounting the collector's slot_memory_bytes reports) plus the resident memory of the slot's process, which a model can
// hold in tens of GiB beside it (2026-11-27: crediting 64.6 GiB of GPU memory alone refused a load that evicting a ~95 GiB model made room
// for). A model's real footprint also routinely exceeds its on-disk weight set (Sprint 6, 2026-08-20: 16.8 GB of weights, 52.9 GB live).
// The on-disk weight set is the fallback when nothing is measurable: never a guess, never more than the evidence supports.
func (m *Manager) slotHolding(cfg *config.Config, slot string) slotHolding {
	sc, ok := cfg.Slots[slot]
	if !ok {
		return slotHolding{}
	}
	var h slotHolding
	if sc.Unit != "" && m.d.Sys != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if pid, err := m.d.Sys.MainPID(ctx, sc.Unit); err == nil && pid != 0 {
			h.gpu = float64(m.d.Proc.GPUMemoryBytes(int(pid)))
		}
	}
	for _, pid := range m.slotPIDs(map[int]bool{sc.Port: true}) {
		h.rss += m.d.Proc.RSSBytes(pid)
		h.private += m.d.Proc.PrivateRSSBytes(pid)
	}
	if h.gpu == 0 && h.rss == 0 {
		model, mmproj := m.SlotWeightPaths(slot)
		w := float64(collector.WeightSetSizeBytes(model, mmproj, cfg.Paths.ModelsDir))
		h = slotHolding{gpu: w}
	}
	return h
}

// SlotFootprintBytes is the exported wrapper the scheduler's optional
// sched.Footprints seam consumes for exact smallest-footprint-first
// eviction ordering (see the Phase 5 progress entry's integrator request).
func (m *Manager) SlotFootprintBytes(slot string) int64 {
	return m.slotHolding(m.d.Cfg(), slot).total()
}

// Plan is the eviction-aware fit check (port of engine.can_fit +
// place_model, which the frozen CanFit shape doesn't carry). The scheduler
// (Phase 5) consumes this; CanFit adapts it to Contract 2.
//
// A1 (bytes retrofit): all figures are bytes.
type Plan struct {
	Fits         bool
	NeedBytes    float64
	FreeBytes    float64
	Slot         string   // proposed placement ("" when nothing fits)
	Evict        []string // slots to evict first, smallest footprint first
	EvictComfyUI bool     // stopping ComfyUI is also required to fit
	ComfyUIBytes float64  // ComfyUI's measured footprint (when EvictComfyUI)
	Message      string
}

// modeNeedEstimate is FitPlan's need-bytes derivation for any mode: a
// fresh profiled safe-memory figure first (weights + KV cache at max
// context, measured), then the catalog's curated safe_memory_bytes, then
// the on-disk weight set — each of the latter two floored against a real
// weights+KV-cache estimate computed from GGUF metadata (kvcache.go)
// whenever the architecture and metadata support it. ok=false means no
// defensible figure exists. Used by FitPlan for its own mode and by the
// in-flight reservation logic, which must reserve a loading slot's
// eventual footprint before any of it is measurable (the 2026-08-22 crash
// admitted a second load while the first sibling's pages were still
// materializing).
//
// 2026-09-07 incident fix: a curated safe_memory_bytes value is a
// hand-entered, often weight-adjacent guess with no guarantee it accounts
// for KV cache at the mode's configured context — gemma4-26b-a4b-nothink's
// curated 25 GiB let a load through that actually needed ~40 GiB once its
// 262144-context, --swa-full KV cache materialized, triggering a
// whole-host OOM cascade while a co-resident model's real ~90 GiB
// footprint left only ~25.5 GiB genuinely free. A stale/optimistic curated
// or file-size figure must never be allowed to undercut what
// kvAwareNeedBytes can actually compute from the model's own metadata.
func (m *Manager) modeNeedEstimate(cfg *config.Config, modeName string) (int64, bool) {
	if _, ok := cfg.Modes[modeName]; !ok {
		return 0, false
	}
	// An engine whose footprint is set by its launch tuple has exactly one defensible figure: the one measured at this tuple (curated.go).
	if need, ok, handled, _ := m.curatedMemory(cfg, modeName); handled {
		return need, ok
	}
	if m.d.ProfileBytes != nil {
		if b, ok := m.d.ProfileBytes(modeName); ok && b > 0 {
			return b, true
		}
	}

	kvBytes, kvOK := m.kvAwareNeedBytes(modeName)

	if m.d.WeightEstimateBytes != nil {
		configID := cfg.Modes[modeName].ConfigID
		if configID != 0 {
			if b, ok := m.d.WeightEstimateBytes(configID); ok && b > 0 {
				if kvOK && kvBytes > b {
					m.logf("fit %s: curated safe_memory_bytes (%.1f GiB) undercuts the computed weights+KV floor (%.1f GiB) — using the computed floor",
						modeName, float64(b)/(1<<30), float64(kvBytes)/(1<<30))
					return kvBytes, true
				}
				return b, true
			}
		}
	}
	if w := modeWeightBytes(cfg, modeName); w > 0 {
		if kvOK && kvBytes > w {
			m.logf("fit %s: on-disk weight size (%.1f GiB) undercuts the computed weights+KV floor (%.1f GiB) — using the computed floor",
				modeName, float64(w)/(1<<30), float64(kvBytes)/(1<<30))
			return kvBytes, true
		}
		return w, true
	}
	if kvOK {
		return kvBytes, true
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

	budget, policy, inputs := m.probeMemory()
	free := float64(budget.FreeBytes)

	// Fail closed (F3): a memory requirement that can't be derived from
	// either the curated registry or an on-disk weight size must never be
	// treated as fitting — the old needBytes==0 <= free path always returned
	// Fits:true and silently over-subscribed. Refuse instead of guessing.
	if !weightKnown {
		m.logf("fit %s: REFUSED — no memory requirement derivable (free=%.1f GiB)", modeName, free/(1<<30))
		if _, _, handled, why := m.curatedMemory(cfg, modeName); handled {
			return Plan{NeedBytes: 0, FreeBytes: free, Message: "cannot load " + why}, nil
		}
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
	for _, name := range m.LoadOrder() {
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
		slot    string
		holding slotHolding
	}
	var candidates []cand
	for _, name := range m.Slots() {
		if slots[name].Mode != "" {
			candidates = append(candidates, cand{name, m.slotHolding(cfg, name)})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if a, b := candidates[i].holding.total(), candidates[j].holding.total(); a != b {
			return a < b
		}
		return candidates[i].slot < candidates[j].slot
	})

	// What would be free after each eviction comes from the ledger itself, with that slot's holdings given back: the bounds count memory
	// differently, so adding a footprint to today's free figure is not the same thing.
	var evict []string
	recovered := 0.0
	after := inputs
	freed := func() float64 { return float64(memledger.Compute(policy, after).Free) - inFlight - effectiveFree }
	for _, cd := range candidates {
		if effectiveFree+recovered >= needBytes {
			break
		}
		evict = append(evict, cd.slot)
		after = cd.holding.returnedTo(after)
		recovered = freed()
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
			after = slotHolding{gpu: comfyBytes}.returnedTo(after)
			recovered = freed()
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
	if l := budget.Ledger; l.SpecialistDeclared > 0 {
		msg += fmt.Sprintf(" (the model budget is %.1f GiB: host %.1f GiB less %.1f GiB OS, %.1f GiB declared for always-on engines and %.1f GiB margin; limited by the %s bound)",
			float64(l.ModelBudget)/(1<<30), float64(l.HostTotal)/(1<<30), float64(l.OSFloor)/(1<<30),
			float64(l.SpecialistDeclared)/(1<<30), float64(l.GrowthMargin)/(1<<30), l.Binding)
	}
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
