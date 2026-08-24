// SPDX-License-Identifier: Apache-2.0

package smith

// autonomy_test.go — the standing autonomy policy (autonomous-remediation
// Sprint 5, docs/v5-smith.md §13.5): the global kill switch, per-procedure
// opt-in, cooldown/rate-cap, the live re-confirmation gate, and the hard
// RiskLow-only invariant no policy setting can override.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

// seedAlwaysOnPortsFinding persists one always_on_ports finding at the
// given severity and returns its ID, for building a FindingID-carrying
// action without going through a full RunChecks sweep.
func seedAlwaysOnPortsFinding(t *testing.T, s *Smith, sev Severity) int64 {
	t.Helper()
	ids, err := s.persistFindings(context.Background(),
		[]Finding{{CheckID: "always_on_ports", Severity: sev, Summary: "test finding"}},
		SweepManual, time.Now(), nil)
	if err != nil || len(ids) != 1 || ids[0] == 0 {
		t.Fatalf("seed finding: ids=%v err=%v", ids, err)
	}
	return ids[0]
}

func setAutonomyPolicy(t *testing.T, s *Smith, p AutonomyPolicy) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if err := s.d.Settings.Set(context.Background(), SettingAutonomy, raw); err != nil {
		t.Fatalf("set autonomy policy: %v", err)
	}
}

// TestMaybeAutoRunProcedure_EndToEnd drives the real proposeFrom → autonomy
// hook path (RunChecks finds forge-stt down, proposes a restart, and
// standing autonomy — opted in below — auto-procedurizes + executes it
// with no human approval).
func TestMaybeAutoRunProcedure_EndToEnd(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	db := openDB(t)
	cfg := &config.Config{Ports: map[string]int{"stt": 8084}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8084: false}
	var restarted []string
	s := New(Deps{
		Store: db, Settings: db.Settings(), Cfg: func() *config.Config { return cfg },
		Source: collector.NewStatic(snap), Now: fixedNow(execAt), Logf: func(string, ...any) {},
		RestartUnit: func(_ context.Context, unit string) error { restarted = append(restarted, unit); return nil },
	})
	s.bgCtx = context.Background()

	setAutonomyPolicy(t, s, AutonomyPolicy{
		Enabled:    true,
		Procedures: map[string]ProcedureAutonomy{"restart_down_unit": {Enabled: true}},
	})

	if _, err := s.RunChecks(context.Background(), ScopeQuick, nil, SweepManual); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}

	var sourceID int64
	waitFor(t, 2*time.Second, func() bool {
		actions, err := s.ListActions(context.Background(), "", nil, 0)
		if err != nil {
			return false
		}
		for _, a := range actions {
			if a.Kind == KindRestartForgeUnit && a.DedupeKey == KindRestartForgeUnit+":forge-stt" {
				sourceID = a.ID
				return a.Status == StatusSuperseded
			}
		}
		return false
	})
	if sourceID == 0 {
		t.Fatal("no restart_forge_unit proposal for forge-stt was created")
	}

	actions, err := s.ListActions(context.Background(), "", nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	var replacementID int64
	for _, a := range actions {
		if a.Kind == KindProcedure && a.CreatedBy == autonomyActor {
			replacementID = a.ID
		}
	}
	if replacementID == 0 {
		t.Fatal("expected a smith-autonomy-created procedure action, found none")
	}
	waitFor(t, 2*time.Second, func() bool {
		a, _ := s.GetAction(context.Background(), replacementID)
		return a != nil && a.Status != StatusExecuting && a.Status != StatusApproved && a.Status != StatusPending
	})
	if len(restarted) != 1 || restarted[0] != "forge-stt" {
		t.Fatalf("restarted = %v, want [forge-stt] — autonomy should have executed the restart unattended", restarted)
	}
}

// TestMaybeAutoRunProcedure_GlobalKillSwitch proves the kill switch wins
// even when the specific procedure is opted in.
func TestMaybeAutoRunProcedure_GlobalKillSwitch(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	setAutonomyPolicy(t, s, AutonomyPolicy{
		Enabled:    false, // kill switch off
		Procedures: map[string]ProcedureAutonomy{"restart_down_unit": {Enabled: true}},
	})
	fid := seedAlwaysOnPortsFinding(t, s, SeverityWarn)
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindRestartForgeUnit, Title: "restart forge-stt", Risk: RiskLow, CreatedBy: "smith",
		Detail: mustJSON(t, restartUnitDetail{Unit: "forge-stt"}), FindingID: &fid,
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	s.maybeAutoRunProcedure(context.Background(), src.ID)
	got, err := s.GetAction(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %s, want pending — global kill switch should have blocked autonomy", got.Status)
	}
}

// TestMaybeAutoRunProcedure_ProcedureNotOptedIn proves the global switch
// alone isn't enough — each procedure needs its own opt-in too.
func TestMaybeAutoRunProcedure_ProcedureNotOptedIn(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	setAutonomyPolicy(t, s, AutonomyPolicy{Enabled: true}) // no procedures opted in
	fid := seedAlwaysOnPortsFinding(t, s, SeverityWarn)
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindRestartForgeUnit, Title: "restart forge-stt", Risk: RiskLow, CreatedBy: "smith",
		Detail: mustJSON(t, restartUnitDetail{Unit: "forge-stt"}), FindingID: &fid,
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	s.maybeAutoRunProcedure(context.Background(), src.ID)
	got, err := s.GetAction(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %s, want pending — procedure not opted in", got.Status)
	}
}

// TestMaybeAutoRunProcedure_StaleFindingDeclines proves the multi-signal
// confirmation gate: a finding that no longer reproduces on a live re-check
// must not trigger an autonomous run, mirroring maybeAutoRecover's
// "signature must currently be true, not only once observed" discipline.
func TestMaybeAutoRunProcedure_StaleFindingDeclines(t *testing.T) {
	db := openDB(t)
	cfg := &config.Config{Ports: map[string]int{"stt": 8084}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8084: true} // port is UP right now — the problem already resolved
	s := New(Deps{
		Store: db, Settings: db.Settings(), Cfg: func() *config.Config { return cfg },
		Source: collector.NewStatic(snap), Logf: func(string, ...any) {},
	})
	setAutonomyPolicy(t, s, AutonomyPolicy{
		Enabled:    true,
		Procedures: map[string]ProcedureAutonomy{"restart_down_unit": {Enabled: true}},
	})
	fid := seedAlwaysOnPortsFinding(t, s, SeverityWarn) // stale — recorded when it WAS down
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindRestartForgeUnit, Title: "restart forge-stt", Risk: RiskLow, CreatedBy: "smith",
		Detail: mustJSON(t, restartUnitDetail{Unit: "forge-stt"}), FindingID: &fid,
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	s.maybeAutoRunProcedure(context.Background(), src.ID)
	got, err := s.GetAction(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %s, want pending — the finding no longer reproduces, autonomy must decline", got.Status)
	}
}

// TestMaybeAutoRunProcedure_CooldownBlocksRepeat proves a run within the
// cooldown window is declined, leaving the action pending for a human —
// exactly the fallback autoRecoverCooldown demonstrates for device-lost.
func TestMaybeAutoRunProcedure_CooldownBlocksRepeat(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	db := openDB(t)
	cfg := &config.Config{Ports: map[string]int{"stt": 8084}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8084: false}
	s := New(Deps{
		Store: db, Settings: db.Settings(), Cfg: func() *config.Config { return cfg },
		Source: collector.NewStatic(snap), Now: fixedNow(now), Logf: func(string, ...any) {},
	})
	setAutonomyPolicy(t, s, AutonomyPolicy{
		Enabled:    true,
		Procedures: map[string]ProcedureAutonomy{"restart_down_unit": {Enabled: true, CooldownSeconds: 600}},
	})
	// Simulate a run 1 minute ago — well inside the 10-minute cooldown.
	s.mu.Lock()
	s.autonomyRunsAt["restart_down_unit"] = []time.Time{now.Add(-1 * time.Minute)}
	s.mu.Unlock()

	fid := seedAlwaysOnPortsFinding(t, s, SeverityWarn)
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindRestartForgeUnit, Title: "restart forge-stt", Risk: RiskLow, CreatedBy: "smith",
		Detail: mustJSON(t, restartUnitDetail{Unit: "forge-stt"}), FindingID: &fid,
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	s.maybeAutoRunProcedure(context.Background(), src.ID)
	got, err := s.GetAction(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %s, want pending — cooldown should have blocked this run", got.Status)
	}
}

// TestMaybeAutoRunProcedure_RiskHighNeverEligible proves the hard
// invariant: even a hypothetically misconfigured autonomyEligible entry
// (temporarily added here — never true in production, see autonomy.go's
// doc comment) cannot make a RiskHigh action run unattended. The Risk
// check in maybeAutoRunProcedure, not just the autonomyEligible allowlist,
// is what actually enforces this.
func TestMaybeAutoRunProcedure_RiskHighNeverEligible(t *testing.T) {
	autonomyEligible["reconcile_orphaned_slot"] = true
	defer delete(autonomyEligible, "reconcile_orphaned_slot")

	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	setAutonomyPolicy(t, s, AutonomyPolicy{
		Enabled:    true,
		Procedures: map[string]ProcedureAutonomy{"reconcile_orphaned_slot": {Enabled: true}},
	})
	fid, err := s.persistFindings(context.Background(),
		[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "test finding"}},
		SweepManual, time.Now(), nil)
	if err != nil || len(fid) != 1 {
		t.Fatalf("seed finding: %v %v", fid, err)
	}
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindUnloadSlot, Title: "unload orphaned a1", Risk: RiskHigh, CreatedBy: "smith",
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a1"}), FindingID: &fid[0],
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	s.maybeAutoRunProcedure(context.Background(), src.ID)
	got, err := s.GetAction(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %s, want pending — RiskHigh must never auto-run regardless of policy", got.Status)
	}
}

// TestAutonomyEligibleAreRegisteredProcedures cross-checks autonomyEligible
// against the real procedure registry — a typo'd or removed procedure ID
// here would otherwise silently never match anything in
// AutonomyEligibleProcedures.
func TestAutonomyEligibleAreRegisteredProcedures(t *testing.T) {
	found := map[string]bool{}
	for _, p := range AutonomyEligibleProcedures() {
		found[p.ID] = true
	}
	for id := range autonomyEligible {
		if !found[id] {
			t.Errorf("autonomyEligible entry %q does not resolve to a registered procedure", id)
		}
	}
}
