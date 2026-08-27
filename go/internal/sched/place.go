// SPDX-License-Identifier: Apache-2.0

package sched

// place.go — placement and the two-tier eviction model (docs/scheduler.md
// "Eviction Philosophy" / "Two Eviction Tiers", forge/scheduler.py
// _place_with_eviction). The scheduler layers idle- and
// reservation-awareness on top of the engine's memory math: FitPlan is the
// engine's live-probed, smallest-footprint-first fit check (never a cached
// budget — scheduling decision paths must not read stale metrics).
//
// One deliberate V5 refinement over V4, enabled by FitPlan existing at
// all: V4's placement never consulted memory — a free bay was used
// unconditionally and an OOM surfaced as a load failure. V5 folds
// FitPlan's memory-driven eviction list (tier-checked) into placement, so
// a model that needs memory freed evicts for it up front instead of
// failing the load. Everything else is a faithful port: free bay
// preferred, otherwise evict exactly one eligible slot, normal tier never
// touches a busy/protected slot, reservation tier may force anything not
// itself under an active reservation.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
)

// placement is one placement decision. slot == "" means nothing is
// placeable right now; terminal distinguishes "never will be without
// something changing that retrying can't cause" (unknown mode, model
// larger than the whole box) from "retry as conditions change" (idle
// thresholds crossing, reservations opening, busy slots draining).
type placement struct {
	slot      string
	evict     []string
	evictComfy bool
	message   string
	terminal  bool
	reason    RefusalReason
}

// RefusalReason classifies why place() could not place a model right now,
// giving callers (a future a0 load-status surface, smith, tests) a stable
// code to switch on instead of parsing prose out of message. "" means no
// refusal happened (a slot was found) — it is never used as a distinct
// "unknown reason" value. Execution failures that happen *after* place()
// succeeds (an evict/stop/load call itself failing) are a different
// failure class and deliberately leave this empty; only place()'s own
// refusal-to-choose-a-slot decisions get a reason.
type RefusalReason string

const (
	ReasonFitProbeFailed         RefusalReason = "fit_probe_failed"
	ReasonModelTooLarge          RefusalReason = "model_too_large"
	ReasonReservationActive      RefusalReason = "reservation_active"
	ReasonReservationSoon        RefusalReason = "reservation_soon"
	ReasonIdleThresholdNotMet    RefusalReason = "idle_threshold_not_met"
	ReasonActivityUnknown        RefusalReason = "activity_unknown"
	ReasonComfyEvictionDisabled  RefusalReason = "comfyui_eviction_disabled"
	ReasonComfyReservationActive RefusalReason = "comfyui_reservation_active"
	ReasonComfyReservationSoon   RefusalReason = "comfyui_reservation_soon"
	ReasonComfyNotConfigured     RefusalReason = "comfyui_not_configured"
	ReasonComfyBusy              RefusalReason = "comfyui_busy"
	ReasonNoEvictableReserved    RefusalReason = "no_evictable_slot_reserved"
	ReasonNoEvictableIdle        RefusalReason = "no_evictable_slot_idle"
)

// occupancy is the engine's authoritative in-memory slot reconciliation,
// fed the latest collector snapshot's unit states (one probe per cycle —
// the scheduler never probes systemd itself).
func (c *Core) occupancy() map[string]collector.SlotAssignment {
	return c.d.Engine.SlotStates(c.d.Source.Current().Units)
}

// findLoaded returns the slot where model is currently loaded, or "".
// When target is given the check is scoped to that one slot — a model
// loaded elsewhere doesn't count for a pinned caller (a0's backend
// records have fixed port/slot mappings; V4 parity). A slot whose unit is
// mid-unload keeps its mode per the crown-jewels rule but is NOT
// "loaded" for a caller about to send traffic to it.
func (c *Core) findLoaded(model, target string) string {
	occ := c.occupancy()
	usable := func(s string) bool {
		a := occ[s]
		return a.Mode == model && a.Unloading == nil
	}
	if target != "" {
		if usable(target) {
			return target
		}
		return ""
	}
	for _, s := range c.d.Engine.Slots() {
		if usable(s) {
			return s
		}
	}
	return ""
}

// idleSeconds reads the slot's idle time from the collector snapshot
// (LastActivity is maintained by the collector's per-cycle llama scrape,
// the V5 form of V4's monitor.get_slot_idle_seconds). ok=false means
// activity is unknown — an unknown-activity slot is never idle-eligible
// (V4 parity: idle_s is not None).
func (c *Core) idleSeconds(slot string) (float64, bool) {
	snap := c.d.Source.Current()
	st, ok := snap.Slots[slot]
	if !ok || st.LastActivity.IsZero() {
		return 0, false
	}
	// Measured against the snapshot's own capture time, not the live
	// clock — same reasoning as Status() in core.go: a stalled collector
	// cycle must not manufacture extra idle time (and therefore extra
	// eviction eligibility) just because the live clock kept moving while
	// the snapshot didn't get any fresher.
	now := snap.TakenAt
	if now.IsZero() {
		now = c.d.Now()
	}
	idle := now.Sub(st.LastActivity).Seconds()
	if idle < 0 {
		idle = 0
	}
	return idle, true
}

func (c *Core) idleEligible(slot string, idleUnloadS int) bool {
	idle, ok := c.idleSeconds(slot)
	return ok && idle >= float64(idleUnloadS)
}

// place decides where model goes and what gets evicted first. horizon is
// the requesting caller's remaining patience: a refusal whose blocker
// cannot clear within it fails immediately with its reason (structural)
// instead of polling to the deadline; one that can clear in time stays
// retryable with the ETA carried in the message.
func (c *Core) place(ctx context.Context, model, target string, cfg Config, reservations []Reservation, horizon time.Duration) placement {
	plan, err := c.d.Engine.FitPlan(model)
	if err != nil {
		// Budget probe failure — transient, retry.
		return placement{message: fmt.Sprintf("fit check failed: %v", err), reason: ReasonFitProbeFailed}
	}

	now := c.d.Now()
	occ := c.occupancy()
	forced := activeReservationForModel(reservations, model, now) != nil
	soon := time.Duration(cfg.ReservationSoonMin) * time.Minute

	free := func(s string) bool {
		a := occ[s]
		return a.Mode == "" && a.Loading == nil && a.Unloading == nil
	}
	activeProtected := func(s string) bool {
		m := occ[s].Mode
		return m != "" && activeReservationForModel(reservations, m, now) != nil
	}
	soonProtected := func(s string) bool {
		m := occ[s].Mode
		return m != "" && modelReservedSoon(reservations, m, now, soon)
	}

	// Memory-driven evictions (engine's smallest-footprint-first list),
	// tier-checked. With ComfyUI eviction live (S1), the engine's "won't
	// fit even after every slot" verdict may be solvable by stopping
	// ComfyUI — handled after the slot loop below, never treated as
	// terminal while that path exists.
	var memEvict []string
	if !plan.Fits {
		if len(plan.Evict) == 0 && !plan.EvictComfyUI {
			return placement{message: plan.Message, terminal: true, reason: ReasonModelTooLarge}
		}
		for _, s := range plan.Evict {
			if s == target {
				continue // the pinned target is handled below
			}
			if activeProtected(s) {
				end := activeReservationEnd(reservations, occ[s].Mode, now)
				return c.structuralRefusal(horizon, end, ReasonReservationActive,
					fmt.Sprintf("not enough VRAM to load %s: %s must be evicted to free memory but is protected by an active reservation", model, s))
			}
			if !forced {
				if msg, terminal, eligible, reason := c.idleHorizonRefusal(s, cfg.IdleUnloadS, horizon); !eligible {
					return placement{message: msg, terminal: terminal, reason: reason}
				}
				if soonProtected(s) {
					end := soonReservationStart(reservations, occ[s].Mode, now, soon)
					return c.structuralRefusal(horizon, end, ReasonReservationSoon,
						fmt.Sprintf("not enough VRAM to load %s: %s must be evicted to free memory but is protected by an upcoming reservation", model, s))
				}
			}
			memEvict = append(memEvict, s)
		}
	}

	// ComfyUI gating: the plan only proposes it when slots alone cannot
	// free enough. Opt-out config and comfyui-scope reservations are
	// structural; a busy queue is transient (workflows finish).
	evictComfy := false
	if plan.EvictComfyUI {
		switch {
		case !cfg.ComfyEvictable():
			return placement{terminal: true, reason: ReasonComfyEvictionDisabled, message: fmt.Sprintf(
				"not enough VRAM to load %s: stopping ComfyUI would free the needed memory (%.1f GiB) but ComfyUI eviction is disabled in the scheduler config",
				model, plan.ComfyUIBytes/(1<<30))}
		case activeComfyReservation(reservations, now):
			end := activeComfyReservationEnd(reservations, now)
			return c.structuralRefusal(horizon, end, ReasonComfyReservationActive, fmt.Sprintf(
				"not enough VRAM to load %s: stopping ComfyUI would free the needed memory but a comfyui reservation is active", model))
		case soonComfyReservation(reservations, now, soon):
			return placement{terminal: true, reason: ReasonComfyReservationSoon, message: fmt.Sprintf(
				"not enough VRAM to load %s: stopping ComfyUI would free the needed memory but a comfyui reservation starts soon", model)}
		default:
			if c.d.ComfyUI == nil || c.d.ComfyUI.Idle == nil || c.d.ComfyUI.Stop == nil || c.d.ComfyUI.Unit == nil {
				return placement{terminal: true, reason: ReasonComfyNotConfigured, message: fmt.Sprintf(
					"not enough VRAM to load %s: stopping ComfyUI would free the needed memory but no ComfyUI service is configured for this deployment", model)}
			}
			if ok, reason := c.d.ComfyUI.Idle(ctx); !ok {
				// Transient — workflows drain. Retryable within the horizon.
				return placement{reason: ReasonComfyBusy, message: fmt.Sprintf(
					"not enough VRAM to load %s yet: ComfyUI must stop to free memory (%.1f GiB) but is not idle — %s",
					model, plan.ComfyUIBytes/(1<<30), reason)}
			}
			evictComfy = true
		}
	}

	// Pinned placement: a0's on-demand path, where a router backend record
	// has a fixed port/slot it expects the model to land on. Free-slot
	// preference and both eviction tiers apply, scoped to this one slot.
	if target != "" {
		if free(target) {
			return placement{slot: target, evict: memEvict, evictComfy: evictComfy}
		}
		if activeProtected(target) {
			end := activeReservationEnd(reservations, occ[target].Mode, now)
			return c.structuralRefusal(horizon, end, ReasonReservationActive, fmt.Sprintf(
				"not enough room on %s: %s is protected by an active reservation", target, occ[target].Mode))
		}
		if !forced {
			if msg, terminal, eligible, reason := c.idleHorizonRefusal(target, cfg.IdleUnloadS, horizon); !eligible {
				return placement{message: msg, terminal: terminal, reason: reason}
			}
			if soonProtected(target) {
				end := soonReservationStart(reservations, occ[target].Mode, now, soon)
				return c.structuralRefusal(horizon, end, ReasonReservationSoon, fmt.Sprintf(
					"%s must be evicted but is protected by an upcoming reservation", target))
			}
		}
		return placement{slot: target, evict: append([]string{target}, memEvict...), evictComfy: evictComfy}
	}

	// Free choice: prefer any free bay.
	for _, s := range c.d.Engine.Slots() {
		if free(s) {
			return placement{slot: s, evict: memEvict, evictComfy: evictComfy}
		}
	}

	// No free bay. If memory evictions are already happening, the first
	// one (smallest footprint) doubles as the bay — likewise when ComfyUI
	// is stopping for memory and its stop alone closes the gap (the plan
	// named no slot; any bay opening after the evictions works).
	if len(memEvict) > 0 {
		return placement{slot: memEvict[0], evict: memEvict, evictComfy: evictComfy}
	}

	// Memory fits but every bay is occupied: evict exactly one slot.
	if forced {
		// Reservation tier — first in-window request for a reserved model
		// may force out anything except a slot protected by ITS OWN
		// active reservation (two simultaneous active reservations
		// wanting the same slot is a config error, not something to
		// silently resolve here). Prefer idle slots even under forced
		// eviction, then smallest footprint.
		var candidates []string
		for _, s := range c.d.Engine.Slots() {
			if occ[s].Mode != "" && !activeProtected(s) {
				candidates = append(candidates, s)
			}
		}
		if len(candidates) == 0 {
			return placement{message: "No evictable slot — all protected by active reservations", reason: ReasonNoEvictableReserved}
		}
		c.sortByFootprint(candidates, plan)
		sort.SliceStable(candidates, func(i, j int) bool {
			ii, jj := c.idleEligible(candidates[i], cfg.IdleUnloadS), c.idleEligible(candidates[j], cfg.IdleUnloadS)
			return ii && !jj
		})
		return placement{slot: candidates[0], evict: []string{candidates[0]}, evictComfy: evictComfy}
	}

	// Normal tier — only a slot that is idle past the threshold AND not
	// protected by an active or soon-starting reservation. Never touches
	// a busy slot. Idle time alone is never sufficient: eviction only
	// happens because this request needs the memory (on-demand only).
	var evictable []string
	for _, s := range c.d.Engine.Slots() {
		if occ[s].Mode == "" {
			continue
		}
		if !c.idleEligible(s, cfg.IdleUnloadS) || soonProtected(s) || activeProtected(s) {
			continue
		}
		evictable = append(evictable, s)
	}
	if len(evictable) == 0 {
		return placement{message: "No idle, unreserved slot available to evict", reason: ReasonNoEvictableIdle}
	}
	c.sortByFootprint(evictable, plan)
	return placement{slot: evictable[0], evict: []string{evictable[0]}, evictComfy: evictComfy}
}

// structuralRefusal turns a blocker with a known clearance time into a
// terminal refusal when clearance lies beyond the caller's horizon, or a
// retryable one carrying the ETA. base must already carry the memory-explicit
// phrasing ("not enough VRAM to load X because …"). reason is the same
// regardless of which branch fires below — only terminality and the ETA
// wording change with timing, not why the slot is blocked.
func (c *Core) structuralRefusal(horizon time.Duration, clearsAt time.Time, reason RefusalReason, base string) placement {
	if clearsAt.IsZero() {
		return placement{terminal: true, reason: reason, message: base + "; the protection has no end — free memory manually or cancel the reservation"}
	}
	eta := clearsAt.Sub(c.d.Now())
	if eta > horizon {
		return placement{terminal: true, reason: reason, message: fmt.Sprintf(
			"%s; the protection lifts in %s, beyond this request's remaining %.0fs wait budget — free memory manually or adjust reservations",
			base, eta.Round(time.Second), horizon.Seconds())}
	}
	return placement{reason: reason, message: fmt.Sprintf("%s (eligible in %s)", base, eta.Round(time.Second))}
}

// idleHorizonRefusal checks slot against the idle threshold with the
// caller's horizon in mind. eligible=true → the caller proceeds (reason is
// "" in that case — no refusal happened); otherwise it returns the composed
// refusal message, whether it is terminal, and why.
func (c *Core) idleHorizonRefusal(slot string, idleUnloadS int, horizon time.Duration) (message string, terminal, eligible bool, reason RefusalReason) {
	idle, known := c.idleSeconds(slot)
	if !known {
		return fmt.Sprintf("not enough VRAM: %s is using memory and must be evicted, but its activity state is unknown — it will never be auto-evicted; free it manually", slot), true, false, ReasonActivityUnknown
	}
	if idle >= float64(idleUnloadS) {
		return "", false, true, ""
	}
	wait := time.Duration(float64(idleUnloadS)-idle) * time.Second
	base := fmt.Sprintf("not enough VRAM yet: %s is using memory but has been idle only %.0fs of the required %ds", slot, idle, idleUnloadS)
	if wait > horizon {
		return fmt.Sprintf("%s; it becomes evictable in %s, beyond this request's remaining %.0fs wait budget",
			base, wait.Round(time.Second), horizon.Seconds()), true, false, ReasonIdleThresholdNotMet
	}
	return fmt.Sprintf("%s (evictable in ~%s)", base, wait.Round(time.Second)), false, false, ReasonIdleThresholdNotMet
}

// sortByFootprint orders slots smallest-footprint-first. When the engine
// exposes per-slot footprints (Footprints — see the integrator request in
// core.go) they are used directly; otherwise the order implied by
// FitPlan's Evict list (already smallest-first) ranks the slots it names,
// with unranked slots after in engine slot order.
func (c *Core) sortByFootprint(slots []string, plan engine.Plan) {
	if fp, ok := c.d.Engine.(Footprints); ok {
		sort.SliceStable(slots, func(i, j int) bool {
			fi, fj := fp.SlotFootprintMB(slots[i]), fp.SlotFootprintMB(slots[j])
			if fi != fj {
				return fi < fj
			}
			return slots[i] < slots[j]
		})
		return
	}
	rank := map[string]int{}
	for i, s := range plan.Evict {
		rank[s] = i
	}
	sort.SliceStable(slots, func(i, j int) bool {
		ri, iOK := rank[slots[i]]
		rj, jOK := rank[slots[j]]
		switch {
		case iOK && jOK:
			return ri < rj
		case iOK:
			return true
		default:
			return false
		}
	})
}
