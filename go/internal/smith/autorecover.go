// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/store"
)

// Auto-recovery for confirmed GPU device-lost (docs/v5-smith.md §4.4
// amendment, 2026-08-17). A device-lost slot is a special case: llama-server
// survives with /health green while every request errors, so neither the
// collector's stall detector nor the engine's health check ever flags it —
// only the journal signature (gpu_device_lost check) and/or the router's
// 5xx storm (SLOT_ERROR_STORM notification) do. Recovery is a trivial
// unload→reload of the same mode; the only genuinely "intelligent" step is
// escalation (propose a different brain/mode) when the model refuses to
// come back.
//
// Auto-recovery deliberately bypasses the human-approval action gate ONLY
// for journal-confirmed device-lost, and ONLY via this path. Everything
// else keeps the "propose, never do" posture.

// autoRecoverCooldown bounds how often smith will auto-recover the same
// slot — a device-lost that instantly recurs must escalate, not thrash.
const autoRecoverCooldown = 10 * time.Minute

// maybeAutoRecover attempts device-lost auto-recovery when a
// SLOT_ERROR_STORM notification arrives. subject is the port-scoped
// notification subject from notifications_handlers.go (a bare port string),
// resolved to a slot name via the snapshot's Slots map.
func (s *Smith) maybeAutoRecover(ctx context.Context, code, subject string) {
	if code != "SLOT_ERROR_STORM" {
		return
	}
	if !s.AutoRecoverDeviceLost(ctx) {
		return
	}
	port := parsePortSubject(subject)
	if port == 0 {
		return
	}

	// Resolve slot name + loaded mode from the snapshot.
	slot, mode := s.slotForPort(ctx, port)
	if slot == "" {
		s.logf("auto-recover: port %d is not a known slot, skipping", port)
		return
	}
	if mode == "" {
		s.logf("auto-recover: slot %s (port %d) has no loaded mode, skipping", slot, port)
		return
	}

	// Cooldown: don't auto-recover the same slot more than once per window.
	// Read-only here — deliberately NOT stamped until the confirmation gate
	// below passes, so a run that correctly bails out (no signature, still
	// busy) never burns the window (found live 2026-08-18: the old code
	// stamped the cooldown before checking anything else).
	now := s.d.Now()
	s.mu.Lock()
	last, seen := s.autoRecoveredAt[slot]
	s.mu.Unlock()
	if seen && now.Sub(last) < autoRecoverCooldown {
		s.logf("auto-recover: slot %s already recovered %s ago — escalating instead", slot, now.Sub(last).Round(time.Second))
		s.proposeDeviceLostEscalation(ctx, slot, mode, "recurred within "+autoRecoverCooldown.String())
		return
	}

	// Confirmation gate: only auto-recover when BOTH a windowed journal
	// signature is present (gpu_device_lost crit, --since bounded — see
	// checks.go) AND the router is currently seeing a real error storm on
	// this slot (SlotErrors > 0, the same signal that raised
	// SLOT_ERROR_STORM in the first place). Signature alone is not enough:
	// a resolved incident's journal lines are still readable, unwindowed
	// reads aside — requiring both keeps a transient 5xx blip or a stale
	// journal line from reloading a healthy model.
	gpuCheck := findCheckByID("gpu_device_lost")
	if gpuCheck.ID == "" || gpuCheck.Run == nil {
		s.logf("auto-recover: gpu_device_lost check not registered — not auto-recovering")
		return
	}
	if f := runOne(ctx, gpuCheck, s.checkEnv(ctx)); f.Severity != SeverityCrit {
		s.logf("auto-recover: SLOT_ERROR_STORM on %s but no journal device-lost signature (gpu_device_lost=%s) — not auto-recovering", slot, f.Severity)
		return
	}
	inf, haveInf := s.slotInference(slot)
	if !haveInf || inf.SlotErrors == 0 {
		s.logf("auto-recover: SLOT_ERROR_STORM on %s but no current router error storm (slot_errors=%d) — not auto-recovering", slot, inf.SlotErrors)
		return
	}

	// Idle gate: a device-lost slot cannot make progress, so
	// RequestsProcessing > 0 alongside a confirmed signature is
	// contradictory evidence — escalate to a human rather than silently
	// doing nothing (a real device-lost slot stays broken) or silently
	// unloading what might be genuine in-flight work.
	if inf.RequestsProcessing > 0 {
		s.logf("auto-recover: slot %s shows %d in-flight request(s) despite a confirmed device-lost signature — contradictory, escalating instead of unloading", slot, inf.RequestsProcessing)
		s.proposeDeviceLostEscalation(ctx, slot, mode, "confirmed device-lost signature but slot reports in-flight requests — needs a human look")
		return
	}

	// Everything checks out — stamp the cooldown now that we're actually
	// about to act, and execute the recovery: unload then reload the same
	// mode. This is the sanctioned non-approved path — journal-confirmed
	// device-lost, same model back, nothing else. Audit every step.
	s.mu.Lock()
	s.autoRecoveredAt[slot] = now
	s.mu.Unlock()

	s.logf("auto-recover: slot %s (mode %s, port %d) device-lost confirmed — unloading", slot, mode, port)
	if s.d.Placer == nil {
		s.logf("auto-recover: Placer not wired — cannot recover slot %s", slot)
		return
	}

	if res := s.d.Placer.Unload(ctx, slot); !res.Success {
		s.logf("auto-recover: unload %s failed: %s", slot, res.Message)
		s.auditAutoRecover(slot, mode, "unload_failed", res.Message)
		s.proposeDeviceLostEscalation(ctx, slot, mode, "unload failed: "+res.Message)
		return
	}
	s.auditAutoRecover(slot, mode, "unloaded", "device-lost confirmed; slot unloaded for reload")

	// Load back the same mode. Pick the slot's own port is handled by the
	// Placer; we request the same slot name.
	if res := s.d.Placer.Load(ctx, mode, slot); !res.Success {
		s.logf("auto-recover: reload %s into %s failed: %s", mode, slot, res.Message)
		s.auditAutoRecover(slot, mode, "reload_failed", res.Message)
		s.proposeDeviceLostEscalation(ctx, slot, mode, "reload failed: "+res.Message)
		return
	}
	s.auditAutoRecover(slot, mode, "reloaded", "device-lost recovery: mode loaded back onto slot")

	s.logf("auto-recover: slot %s recovered (mode %s reloaded)", slot, mode)

	// Post-recovery verification: confirm the slot is serving again. Run the
	// relevant checks through the normal path so the evidence trail lands in
	// a finding/investigation. We open an investigation describing what was
	// done (the "propose future investigation while explaining actions"
	// requirement).
	s.openRecoveryInvestigation(ctx, slot, mode, port)
}

// slotForPort resolves a port to (slot name, loaded mode) from the live
// snapshot's Slots map. Returns "" for an unknown/empty slot.
func (s *Smith) slotForPort(ctx context.Context, port int) (string, string) {
	snap := s.snapshot()
	if snap == nil {
		return "", ""
	}
	for name, st := range snap.Slots {
		if st.Port == port {
			return name, st.Mode
		}
	}
	return "", ""
}

// slotInference returns the live collector.SlotInference for a slot name, and
// whether the snapshot/entry exists at all. Used by the confirmation and
// idle gates above — SlotErrors and RequestsProcessing are both already
// scraped from llama-server /metrics, no new Deps wiring needed.
func (s *Smith) slotInference(slot string) (collector.SlotInference, bool) {
	snap := s.snapshot()
	if snap == nil {
		return collector.SlotInference{}, false
	}
	inf, ok := snap.Inference[slot]
	return inf, ok
}

// parsePortSubject parses the notifications subject (a bare port string for
// port-scoped alerts) to an int. 0 on any non-port subject.
func parsePortSubject(subject string) int {
	var port int
	for _, ch := range subject {
		if ch < '0' || ch > '9' {
			return 0
		}
		port = port*10 + int(ch-'0')
		if port > 65535 {
			return 0
		}
	}
	return port
}

// auditAutoRecover writes one audit entry describing an auto-recovery step.
func (s *Smith) auditAutoRecover(slot, mode, action, detail string) {
	if s.d.Audit == nil {
		return
	}
	_ = s.d.Audit.Write(context.Background(), store.AuditEntry{
		Actor:  "smith",
		Action: "smith_auto_recover",
		Target: fmt.Sprintf("%s:%s", slot, action),
		Detail: detail,
	})
}

// proposeDeviceLostEscalation is the "intelligent" part of recovery: the
// model refused to come back (reload failed or device-lost recurred — a
// crash loop), so smith stops auto-retrying and hands off. First choice is a
// graceful swap of smith's brain to a healthy remote (cloud) offering so the
// reasoning tier keeps working during the outage; only if no healthy cloud
// candidate exists does it fall back to proposing an operator-runbook
// escalation. Both paths are audited and recorded in an investigation.
func (s *Smith) proposeDeviceLostEscalation(ctx context.Context, slot, mode, reason string) {
	// Try the graceful cloud handoff first: pick the first healthy remote
	// candidate (probeHandoffCandidates covers smith.handoff_offerings ∪
	// same-model offerings, provider-health-probed). This mirrors the
	// operator-driven "remote" handoff resolution but runs automatically —
	// a crash-looping local brain is already unusable, so swapping smith's
	// own brain off it is strictly an improvement.
	if s.AutoHandoffCloud(ctx) {
		if chosen := s.firstHealthyCandidate(ctx); chosen != nil {
			if _, swapErr := s.swapBrainToRemote(ctx, chosen, "smith", "auto-recover:"+slot); swapErr == nil {
				s.logf("auto-recover: handoff — swapped smith brain to cloud offering %s (%s) after %s", chosen.Model, chosen.Provider, reason)
				s.openHandoffInvestigation(ctx, slot, mode, reason, chosen)
				return
			} else {
				s.logf("auto-recover: cloud handoff swap failed for %s (%s): %v — falling back to runbook", chosen.Model, chosen.Provider, swapErr)
			}
		}
	}

	// No healthy candidate (or the swap failed): propose an operator-runbook
	// escalation so a human decides the transition.
	detail, _ := json.Marshal(map[string]any{"slot": slot, "mode": mode, "reason": reason})
	draft := ActionDraft{
		Kind:      KindRunbook,
		Risk:      RiskHigh,
		CreatedBy: "smith",
		Title:     fmt.Sprintf("Escalate: slot %s (mode %s) will not recover — %s", slot, mode, reason),
		Detail:    detail,
	}
	if _, err := s.CreateAction(ctx, draft); err != nil {
		s.logf("auto-recover: escalation proposal failed: %v", err)
	}
}

// firstHealthyCandidate returns the first provider-healthy remote offering
// candidate, or nil when none is available. Uses the same probe the handoff
// machinery does (probeHandoffCandidates) so the auto path and the
// operator-driven path agree on what "can take over" means.
func (s *Smith) firstHealthyCandidate(ctx context.Context) *Candidate {
	// brainModel is irrelevant to the candidate set for a crash-loop handoff
	// (the local config is gone); probe with the current smith.model so
	// same-model remote offerings still appear, but the handoff_offerings
	// list is what actually drives it.
	cands := s.probeHandoffCandidates(ctx, s.settingModel(ctx))
	for i := range cands {
		if cands[i].Healthy {
			return &cands[i]
		}
	}
	return nil
}

// openHandoffInvestigation records an automatic cloud handoff in a
// smith_investigations row so a human has a full trail: why the local brain
// was abandoned, what it was swapped to, and how to swap back.
func (s *Smith) openHandoffInvestigation(ctx context.Context, slot, mode, reason string, chosen *Candidate) {
	if chosen == nil {
		return
	}
	summary := fmt.Sprintf(
		"AUTO-HANDOFF: slot %s (mode %s) entered a crash loop (%s). smith's brain was swapped to cloud offering %s (%s). Swap back via Settings → smith.model when the local slot is healthy again.",
		slot, mode, reason, chosen.Model, chosen.Provider)
	invID, err := s.CreateInvestigation(ctx, "anomaly:INFERENCE_DEVICE_LOST_HANDOFF", summary)
	if err != nil {
		s.logf("auto-recover: open handoff investigation failed: %v", err)
		return
	}
	if _, err := s.RunChecksIntoInvestigation(ctx, invID, []string{"slot_agreement", "gpu_device_lost", "brain_resolvable"}, ScopeQuick, SweepAnomaly); err != nil {
		s.logf("auto-recover: post-handoff checks failed: %v", err)
	}
}

// openRecoveryInvestigation records the auto-recovery in a smith_investigations
// row so a human has a trail of what was done and why (the "explain actions
// taken" requirement). Best-effort; never fails the recovery itself.
func (s *Smith) openRecoveryInvestigation(ctx context.Context, slot, mode string, port int) {
	summary := fmt.Sprintf(
		"AUTO-RECOVERED device-lost on slot %s (mode %s, port %d): unloaded and reloaded the same mode automatically. Verify the slot serves a completion before closing.",
		slot, mode, port)
	invID, err := s.CreateInvestigation(ctx, "anomaly:INFERENCE_DEVICE_LOST_RECOVERED", summary)
	if err != nil {
		s.logf("auto-recover: open investigation failed: %v", err)
		return
	}
	// Run the post-recovery verification checks into the investigation.
	if _, err := s.RunChecksIntoInvestigation(ctx, invID, []string{"slot_agreement", "n_ctx_actual", "gpu_device_lost"}, ScopeQuick, SweepAnomaly); err != nil {
		s.logf("auto-recover: post-recovery checks failed: %v", err)
	}
}
