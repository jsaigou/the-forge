// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/sched"
)

// brain_residency.go — on-demand brain loading + the opt-in "stay
// resident" policy, added alongside the Sprint 6 brain switch (smith
// efficiency initiative, docs/v5-smith-efficiency.md). Two previously
// missing capabilities:
//
//  1. Brain() is deliberately pure-read ("never triggers a load") and
//     nothing ever called sched.EnsureLoaded on smith's behalf — a
//     resolvable-but-unloaded brain left smith permanently in
//     deterministic_only, even for a question that genuinely needed the
//     reasoning tier. ensureBrainLoaded is the on-demand escalation path,
//     wired into decideTier (reasoning.go) so a load is only attempted
//     when a turn actually wants to escalate — never on every turn.
//  2. Nothing let the operator choose to keep the brain always loaded.
//     maybeEnsureBrainResident is the opt-in (smith.brain_residency,
//     default OFF) periodic counterpart, reusing the same primitive.
//
// Neither path ever targets a specific slot — sched.EnsureRequest.TargetSlot
// is always "" here, so the scheduler picks any free slot per its own
// placement policy (never evicts a busy/reserved slot), exactly like every
// other EnsureLoaded caller (a0's on-demand path, MCP's ensure_loaded tool).

// brainLoadTimeout bounds one on-demand load attempt — deliberately shorter
// than sched's own 150s default (core.go's DefaultTimeout) so a slow/failed
// load still leaves compressor inside smith's 480s turnBudget for the actual
// reasoning turn that follows, and so a stay_resident sweep doesn't wedge
// the scheduleLoop tick waiting on a hung load.
const brainLoadTimeout = 90 * time.Second

// brainResidencyInterval is how often scheduleLoop's 1-minute tick
// considers a stay-resident re-load — short relative to selfReviewInterval
// (2h) / retentionInterval, since the whole point of stay_resident is
// recovering quickly if something else evicted the brain.
const brainResidencyInterval = 5 * time.Minute

// BrainResidencyResult is one ensureBrainLoaded attempt's outcome —
// surfaced on GET /smith/status (SelfContext.BrainResidency) so "is
// auto-escalation actually working" is a live status check, not a leap of
// faith, same posture as RetentionResult/SelfReviewResult.
type BrainResidencyResult struct {
	AttemptedAt time.Time `json:"attempted_at"`
	Loaded      bool      `json:"loaded"` // resolution was (or became) BrainLocalSlot/BrainRemote
	Slot        string    `json:"slot,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// BrainResidencyStatus is the SelfContext projection. LastAttemptAt is nil
// until the first attempt completes (never faked as "now").
type BrainResidencyStatus struct {
	StayResident  bool   `json:"stay_resident"`
	LastAttemptAt *int64 `json:"last_attempt_at"`
	LastLoaded    bool   `json:"last_loaded"`
	LastSlot      string `json:"last_slot,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// ensureBrainLoaded is the shared on-demand escalation primitive. Brain()
// itself is untouched and stays pure-read — every existing caller
// (propose.go, handoff.go, SelfContext, checks.go's runBrainResolvable)
// keeps calling Brain() directly with no behavior change. This is a new,
// separate entry point used only where smith is actively deciding to try
// the reasoning tier: decideTier's on-demand path and
// maybeEnsureBrainResident's periodic path.
//
// A no-op (single Brain() read, no load attempt, nothing recorded) unless
// the brain is genuinely a local Config that's just not currently loaded —
// smith.model unset, unresolvable, or a remote offering all pass through
// unchanged, since there is nothing for EnsureLoaded to do.
func (s *Smith) ensureBrainLoaded(ctx context.Context) BrainResolution {
	br := s.Brain(ctx)
	if br.Resolution != BrainDeterministicOnly || s.d.Sched == nil || s.d.Catalog == nil {
		return br
	}
	// S5: the load target is chain[0] when a brain chain is provisioned,
	// else smith.model — same effective-model rule Brain() uses.
	model := s.settingModel(ctx)
	if chain := s.BrainChain(ctx); len(chain) > 0 {
		model = chain[0]
	}
	if model == "" {
		return br
	}
	cfg, err := s.d.Catalog.ConfigByName(ctx, model)
	if err != nil || cfg.ID == 0 {
		return br // not a local config at all — nothing this path can load
	}

	lctx, cancel := context.WithTimeout(ctx, brainLoadTimeout)
	defer cancel()
	ticket, err := s.d.Sched.EnsureLoaded(lctx, sched.EnsureRequest{
		Model: model, RequestedBy: "smith", TargetSlot: "", SmallJob: true,
	})
	if err != nil {
		s.logf("ensureBrainLoaded: %v", err)
		s.recordBrainResidencyResult(BrainResidencyResult{AttemptedAt: s.d.Now(), Error: err.Error()})
		return br // graceful: caller falls back to deterministic, same as before this existed
	}

	// Build the resolution directly from the successful ticket rather than
	// re-reading Brain() — found live (Sprint 6 brain residency follow-up):
	// sched.Status() (what Brain()'s slot scan reads) is deliberately
	// snapshot-based ("design decision 2" in sched/core.go — handlers read
	// snapshots, only FitPlan-style decision paths probe live), lagging
	// real state by up to one collector cycle. An immediate Brain() re-read
	// right after EnsureLoaded returns can genuinely still see
	// deterministic_only even though the model is already loaded and
	// serving — verified against real ForgeHost behavior, not a hypothetical.
	// The ticket itself already carries everything needed.
	fresh := BrainResolution{
		Resolution: BrainLocalSlot,
		Model:      model,
		Slot:       ticket.TargetSlot,
		Detail:     "local config on slot " + strings.ToUpper(ticket.TargetSlot),
	}
	s.recordBrainResidencyResult(BrainResidencyResult{
		AttemptedAt: s.d.Now(),
		Loaded:      true,
		Slot:        ticket.TargetSlot,
	})
	return fresh
}

// maybeEnsureBrainResident runs on scheduleLoop's 1-minute tick, mirroring
// maybePrune/maybeSelfReview's interval-guard shape. Unconditional once due
// and stay_resident is on — not gated by autoEscalate's crit-findings
// heuristic, since stay_resident is a deliberate, blunt "always keep it
// loaded" choice distinct from decideTier's narrower per-turn escalation.
func (s *Smith) maybeEnsureBrainResident(ctx context.Context, now time.Time) {
	if !s.BrainResidencyConfig(ctx).StayResident {
		return
	}
	// Check residency BEFORE touching the interval clock — found live on
	// ForgeHost: stamping lastBrainResidencyAt on every due tick, even the
	// already-resident no-op ones, meant a tick that happened to land while
	// the brain was already loaded (e.g. right after an on-demand load)
	// reset the 5-minute cooldown, so a genuine later eviction couldn't be
	// noticed and re-loaded until the FULL interval had re-elapsed from
	// that unrelated no-op, not from when it actually went missing. The
	// interval only needs to gate real load *attempts*, not this cheap
	// check.
	br := s.Brain(ctx)
	if br.Resolution == BrainLocalSlot || br.Resolution == BrainRemote {
		return // already resident, nothing to do — never consumes the interval budget
	}
	s.brainResidencyMu.Lock()
	due := now.Sub(s.lastBrainResidencyAt) >= brainResidencyInterval
	if due {
		s.lastBrainResidencyAt = now
	}
	s.brainResidencyMu.Unlock()
	if !due {
		return
	}
	go s.ensureBrainLoaded(ctx)
}

// recordBrainResidencyResult stores res as the latest attempt's outcome,
// guarded by mu (mirroring recordSelfReviewResult).
func (s *Smith) recordBrainResidencyResult(res BrainResidencyResult) {
	s.mu.Lock()
	s.lastBrainResidency = res
	s.mu.Unlock()
}

// brainResidencyStatus assembles SelfContext.BrainResidency — a pure read
// of the in-memory last-attempt record, never triggers a load itself.
func (s *Smith) brainResidencyStatus(ctx context.Context) BrainResidencyStatus {
	s.mu.Lock()
	last := s.lastBrainResidency
	s.mu.Unlock()
	st := BrainResidencyStatus{StayResident: s.BrainResidencyConfig(ctx).StayResident}
	if !last.AttemptedAt.IsZero() {
		ts := last.AttemptedAt.Unix()
		st.LastAttemptAt = &ts
		st.LastLoaded = last.Loaded
		st.LastSlot = last.Slot
		st.LastError = last.Error
	}
	return st
}
