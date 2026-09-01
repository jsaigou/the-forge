// SPDX-License-Identifier: Apache-2.0

package smith

// autonomy.go — the standing autonomy policy (autonomous-remediation
// Sprint 5, docs/v5-smith.md §13.3). Generalizes autorecover.go's
// confirmation-gate discipline to the procedure engine: instead of a human
// clicking "let smith fix it" (Sprint 3's Procedurize, still the only path
// for everything not explicitly opted in here), an operator can opt a
// SPECIFIC procedure into running unattended the moment its atomic
// proposal is created — gated by a global kill switch (default off), a
// per-procedure opt-in (default off), a cooldown + rolling-24h rate cap,
// and a live re-confirmation that the problem being fixed is still real.
//
// Hard invariant, not configurable by policy: only RiskLow procedures are
// ever eligible. autonomyEligible below is the fixed, reviewed allowlist —
// deliberately NOT derived at runtime from procedureForActionKind + a risk
// lookup, so widening it is always a visible, reviewed code change. Today
// that's restart_down_unit and (2026-09-01) restore_unit_launcher — both
// RiskLow, both fail closed on their own real preconditions (restartAllowed,
// launcherInstallAllowed) rather than ever falsely reporting success.
// reconcile_orphaned_slot and comfyui_prune are RiskHigh and structurally
// excluded (see proposeComfyUIDelete's "never auto-approved (RiskHigh,
// always)" and proposeReconcileOrphanSlot's guardrail-2 posture in
// propose.go).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/procedures"
)

// SettingAutonomy is the standing-autonomy-policy settings key.
const SettingAutonomy = "smith.autonomy"

// autonomyActor marks every action/audit row an autonomous run produces —
// distinct from "smith" (proposals) and any human actor, so the audit trail
// and Sprint 4's scorecards can tell an unattended run apart at a glance.
const autonomyActor = "smith-autonomy"

const (
	// defaultAutonomyCooldown mirrors autoRecoverCooldown's 10-minute
	// window — used when a procedure's own policy entry doesn't override
	// it.
	defaultAutonomyCooldown = 10 * time.Minute
	// defaultAutonomyMaxPerDay bounds how many unattended runs of the same
	// procedure smith will perform in a rolling 24h window before falling
	// back to leaving proposals pending for a human — a thrashing fix is a
	// signal something deeper is wrong, not something to keep retrying.
	defaultAutonomyMaxPerDay = 3
)

// autonomyEligible is the fixed, reviewed set of procedure IDs the standing
// autonomy policy may ever apply to. TestAutonomyEligibleAreRegisteredProcedures
// cross-checks every entry here against the real procedure registry;
// maybeAutoRunProcedure's own Risk != RiskLow check (not this map) is what
// actually enforces the RiskLow invariant — proven by
// TestMaybeAutoRunProcedure_RiskHighNeverEligible, which deliberately adds a
// RiskHigh procedure to this map and confirms the run is still refused.
var autonomyEligible = map[string]bool{
	"restart_down_unit":     true,
	"restore_unit_launcher": true,
}

// ProcedureAutonomy is one procedure's opt-in policy entry. Zero value
// (Enabled: false) is the safe default — a procedure absent from
// AutonomyPolicy.Procedures entirely is exactly as disabled as one present
// with Enabled: false.
type ProcedureAutonomy struct {
	Enabled bool `json:"enabled"`
	// CooldownSeconds overrides defaultAutonomyCooldown when > 0.
	CooldownSeconds int `json:"cooldown_seconds"`
	// MaxPerDay overrides defaultAutonomyMaxPerDay when > 0.
	MaxPerDay int `json:"max_per_day"`
}

// AutonomyPolicy is smith.autonomy's decoded shape. Enabled is the global
// kill switch: false disables every procedure's autonomy regardless of its
// own opt-in — the two are deliberately independent so an operator can
// always reach "definitely nothing is autonomous right now" by flipping one
// field, without needing to remember or re-find every procedure they'd
// opted in.
type AutonomyPolicy struct {
	Enabled    bool                         `json:"enabled"`
	Procedures map[string]ProcedureAutonomy `json:"procedures"`
}

// DefaultAutonomyPolicy is used when smith.autonomy is unset or
// unreadable — default off, everything.
func DefaultAutonomyPolicy() AutonomyPolicy {
	return AutonomyPolicy{Enabled: false, Procedures: map[string]ProcedureAutonomy{}}
}

// AutonomyPolicy reads + decodes smith.autonomy, falling back to
// DefaultAutonomyPolicy on anything unset or unparseable. Unlike
// Schedule/Thresholds' field-by-field merge, this decodes wholesale: the
// zero value of every field here IS the safe default, so a malformed
// stored value degrading to "everything off" (rather than attempting a
// partial merge) is itself the correct failure mode for a security-relevant
// setting.
func (s *Smith) AutonomyPolicy(ctx context.Context) AutonomyPolicy {
	out := DefaultAutonomyPolicy()
	raw, ok := s.settingJSON(ctx, SettingAutonomy)
	if !ok {
		return out
	}
	var v AutonomyPolicy
	if err := json.Unmarshal(raw, &v); err != nil {
		return out
	}
	if v.Procedures == nil {
		v.Procedures = map[string]ProcedureAutonomy{}
	}
	return v
}

// AutonomyEligibleProcedures returns the registered procedures autonomy
// could ever apply to (autonomyEligible), in a stable order — the read
// surface for the Settings UI to render one policy row per procedure
// without hardcoding IDs on the frontend.
func AutonomyEligibleProcedures() []procedures.Procedure {
	out := make([]procedures.Procedure, 0, len(autonomyEligible))
	for _, p := range procedures.All() {
		if autonomyEligible[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// trimAutonomyWindow drops timestamps older than 24h, preserving order.
func trimAutonomyWindow(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-24 * time.Hour)
	out := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

// maybeAutoRunProcedure is the standing-autonomy decision point. Called
// once per freshly-INSERTED proposal from proposeFrom (never for a
// reused/still-pending row — a proposal autonomy previously declined stays
// a normal pending suggestion for a human; it is not repeatedly retried
// every sweep). Every gate below must pass, in order; any failure simply
// returns, leaving the action pending exactly as if autonomy didn't exist.
func (s *Smith) maybeAutoRunProcedure(ctx context.Context, actionID int64) {
	a, err := s.GetAction(ctx, actionID)
	if err != nil || a.Status != StatusPending {
		return
	}
	// Hard invariant — never overridable by policy.
	if a.Risk != RiskLow {
		return
	}
	procID, ok := procedureForAction(a)
	if !ok || !autonomyEligible[procID] {
		return
	}

	policy := s.AutonomyPolicy(ctx)
	if !policy.Enabled {
		return
	}
	pa, ok := policy.Procedures[procID]
	if !ok || !pa.Enabled {
		return
	}

	cooldown := defaultAutonomyCooldown
	if pa.CooldownSeconds > 0 {
		cooldown = time.Duration(pa.CooldownSeconds) * time.Second
	}
	maxPerDay := defaultAutonomyMaxPerDay
	if pa.MaxPerDay > 0 {
		maxPerDay = pa.MaxPerDay
	}

	now := s.d.Now()
	s.mu.Lock()
	runs := trimAutonomyWindow(s.autonomyRunsAt[procID], now)
	s.autonomyRunsAt[procID] = runs
	var last time.Time
	if len(runs) > 0 {
		last = runs[len(runs)-1]
	}
	blocked := (!last.IsZero() && now.Sub(last) < cooldown) || len(runs) >= maxPerDay
	s.mu.Unlock()
	if blocked {
		s.logf("autonomy: %s declined for action %d (cooldown/rate-cap) — leaving pending for a human", procID, actionID)
		return
	}

	// Multi-signal confirmation: the finding that triggered this proposal
	// must still be live right now, not only true at proposal time (mirrors
	// maybeAutoRecover's "signature must currently be true"). An action
	// with no traceable finding fails closed — never auto-run on
	// unattributed evidence.
	if a.FindingID == nil {
		s.logf("autonomy: %s declined for action %d (no traceable finding) — leaving pending for a human", procID, actionID)
		return
	}
	checkID, err := s.findingCheckID(ctx, *a.FindingID)
	if err != nil || checkID == "" {
		s.logf("autonomy: %s declined for action %d (finding lookup failed) — leaving pending for a human", procID, actionID)
		return
	}
	findings := s.runChecksBare(ctx, []string{checkID})
	if len(findings) != 1 || (findings[0].Severity != SeverityWarn && findings[0].Severity != SeverityCrit) {
		s.logf("autonomy: %s declined for action %d (%s no longer failing) — leaving pending for a human", procID, actionID, checkID)
		return
	}

	// Preconditions (procedures.Procedure.Preconditions) — previously
	// advisory-only metadata (registry.go's doc comment); this is the first
	// real enforcement, gating an unattended start the same way a human
	// approval implicitly gates on "does this still look safe."
	if proc, ok := procedures.Get(procID); ok {
		for _, pcID := range proc.Preconditions {
			pf := s.runChecksBare(ctx, []string{pcID})
			if len(pf) == 1 && pf[0].Severity == SeverityCrit {
				s.logf("autonomy: %s declined for action %d (precondition %s crit) — leaving pending for a human", procID, actionID, pcID)
				return
			}
		}
	}

	s.mu.Lock()
	s.autonomyRunsAt[procID] = append(s.autonomyRunsAt[procID], now)
	s.mu.Unlock()

	s.logf("autonomy: auto-running %s for action %d (all gates passed)", procID, actionID)
	if _, err := s.Procedurize(ctx, actionID, autonomyActor); err != nil {
		s.logf("autonomy: %s auto-run failed for action %d: %v", procID, actionID, err)
	}
}
