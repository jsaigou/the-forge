// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
)

// forceInvestigationOpenedAt backdates an investigation's opened_at directly
// (test setup only — CreateInvestigation always stamps s.d.Now() itself, so
// there's no other way to make one look older than "now" without a second,
// separately-advanced Deps.Now).
func forceInvestigationOpenedAt(t *testing.T, s *Smith, invID int64, at time.Time) {
	t.Helper()
	if _, err := s.d.Store.SQL().Exec(`UPDATE smith_investigations SET opened_at = ? WHERE id = ?`, at.Unix(), invID); err != nil {
		t.Fatalf("force opened_at: %v", err)
	}
}

// forceActionCreatedAt backdates an action's created_at directly, same
// reasoning as forceInvestigationOpenedAt.
func forceActionCreatedAt(t *testing.T, s *Smith, id int64, at time.Time) {
	t.Helper()
	if _, err := s.d.Store.SQL().Exec(`UPDATE smith_actions SET created_at = ? WHERE id = ?`, at.Unix(), id); err != nil {
		t.Fatalf("force created_at: %v", err)
	}
}

func TestSelfReviewOnce_InvestigationCleanGetsClosureProposalNotAutoResolve(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	setSetting(t, db, SettingSelfReview, `{"enabled":true,"grace_minutes":30}`)
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "manual", "test")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	forceInvestigationOpenedAt(t, s, invID, now.Add(-time.Hour))
	// slot_agreement was warn at open time, but with no Sched wired it
	// re-checks as a skip (info, not warn/crit) — the "problem's gone" case.
	if _, err := s.persistFindings(ctx,
		[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"}},
		SweepManual, now.Add(-time.Hour), &invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	res, err := s.selfReviewOnce(ctx)
	if err != nil {
		t.Fatalf("selfReviewOnce: %v", err)
	}
	if res.InvestigationsReviewed != 1 {
		t.Errorf("InvestigationsReviewed = %d, want 1", res.InvestigationsReviewed)
	}
	if res.InvestigationsProposed != 1 {
		t.Errorf("InvestigationsProposed = %d, want 1", res.InvestigationsProposed)
	}

	inv, _, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if inv.Status != "open" {
		t.Errorf("investigation status = %s, want still open — self-review must propose, never auto-resolve", inv.Status)
	}

	actions, err := s.ListActions(ctx, StatusPending, &invID, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("pending actions on investigation = %d, want exactly 1", len(actions))
	}
	if actions[0].Kind != KindRunbook {
		t.Errorf("kind = %s, want runbook", actions[0].Kind)
	}
	if actions[0].CreatedBy != "smith" {
		t.Errorf("created_by = %s, want smith", actions[0].CreatedBy)
	}
	if !isSelfReviewCloseDetail(actions[0].Detail) {
		t.Error("expected the self_review_close marker on the proposed action's detail")
	}
}

// TestSelfReviewOnce_AnomalyInvestigationIgnoresUnrelatedAmbientWarn is the
// regression test for the 2026-08-19 live finding: an anomaly-triggered
// investigation's initial deep sweep can carry ambient warns unrelated to
// what actually triggered it (real example: comfyui_prune/brain_resolvable
// on a GTT_DRAIN_TIMEOUT investigation), which used to block self-review's
// closure gate forever since it required every warn/crit ever seen — not
// just the ones relevant to the trigger — to read clean.
func TestSelfReviewOnce_AnomalyInvestigationIgnoresUnrelatedAmbientWarn(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	setSetting(t, db, SettingSelfReview, `{"enabled":true,"grace_minutes":30}`)
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "anomaly:GTT_DRAIN_TIMEOUT", "")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	forceInvestigationOpenedAt(t, s, invID, now.Add(-time.Hour))
	// slot_agreement is in anomalyRelevantChecks["GTT_DRAIN_TIMEOUT"] and
	// re-checks clean (no Sched wired). brain_resolvable is NOT relevant to
	// a GTT drain timeout, and — with no smith.model setting — re-checks
	// warn deterministically every time, exactly like the live
	// comfyui_prune/brain_resolvable case that motivated this fix.
	if _, err := s.persistFindings(ctx,
		[]Finding{
			{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"},
			{CheckID: "brain_resolvable", Severity: SeverityWarn, Summary: "smith.model is unset"},
		},
		SweepAnomaly, now.Add(-time.Hour), &invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	res, err := s.selfReviewOnce(ctx)
	if err != nil {
		t.Fatalf("selfReviewOnce: %v", err)
	}
	if res.InvestigationsProposed != 1 {
		t.Errorf("InvestigationsProposed = %d, want 1 (an unrelated ambient warn must not block closure)", res.InvestigationsProposed)
	}

	inv, _, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if inv.Status != "open" {
		t.Errorf("investigation status = %s, want still open — a proposal, not an auto-resolve", inv.Status)
	}
}

func TestSelfReviewOnce_InvestigationStillFailingLeftOpenNoProposal(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	crit := snapWith(collector.Metrics{GTTUsedBytes: int64p(96), GTTTotalBytes: int64p(100)})
	src := collector.NewStatic(crit)
	s := New(Deps{Store: db, Settings: db.Settings(), Source: src, Now: fixedNow(now), Logf: func(string, ...any) {}})
	setSetting(t, db, SettingSelfReview, `{"enabled":true,"grace_minutes":30}`)
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "manual", "test")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	forceInvestigationOpenedAt(t, s, invID, now.Add(-time.Hour))
	// gtt_ceiling is genuinely still crit against the wired Source below —
	// unlike slot_agreement (which reads clean whenever no Sched is wired),
	// this check has no "clean by absence of wiring" escape hatch.
	if _, err := s.persistFindings(ctx,
		[]Finding{{CheckID: "gtt_ceiling", Severity: SeverityWarn, Summary: "high"}},
		SweepManual, now.Add(-time.Hour), &invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	res, err := s.selfReviewOnce(ctx)
	if err != nil {
		t.Fatalf("selfReviewOnce: %v", err)
	}
	if res.InvestigationsReviewed != 1 {
		t.Errorf("InvestigationsReviewed = %d, want 1", res.InvestigationsReviewed)
	}
	if res.InvestigationsProposed != 0 {
		t.Errorf("InvestigationsProposed = %d, want 0 (nothing re-checked clean)", res.InvestigationsProposed)
	}

	actions, err := s.ListActions(ctx, StatusPending, &invID, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("pending actions on investigation = %d, want 0", len(actions))
	}
}

func TestSelfReviewOnce_InvestigationYoungerThanGraceIsSkipped(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	setSetting(t, db, SettingSelfReview, `{"enabled":true,"grace_minutes":30}`)
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "manual", "test")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	// opened_at defaults to "now" — well within the 30-minute grace window.
	if _, err := s.persistFindings(ctx,
		[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"}},
		SweepManual, now, &invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	res, err := s.selfReviewOnce(ctx)
	if err != nil {
		t.Fatalf("selfReviewOnce: %v", err)
	}
	if res.InvestigationsReviewed != 0 {
		t.Errorf("InvestigationsReviewed = %d, want 0 (still inside the grace window)", res.InvestigationsReviewed)
	}
}

func TestSelfReviewOnce_DedupeOnRepeatSweepBeforeApproval(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	setSetting(t, db, SettingSelfReview, `{"enabled":true,"grace_minutes":30}`)
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "manual", "test")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	forceInvestigationOpenedAt(t, s, invID, now.Add(-time.Hour))
	if _, err := s.persistFindings(ctx,
		[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"}},
		SweepManual, now.Add(-time.Hour), &invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	if _, err := s.selfReviewOnce(ctx); err != nil {
		t.Fatalf("selfReviewOnce (1st): %v", err)
	}
	res2, err := s.selfReviewOnce(ctx)
	if err != nil {
		t.Fatalf("selfReviewOnce (2nd): %v", err)
	}
	if res2.InvestigationsProposed != 0 {
		t.Errorf("2nd sweep InvestigationsProposed = %d, want 0 — repeat sweep before approval must reuse the pending proposal, not duplicate it", res2.InvestigationsProposed)
	}

	actions, err := s.ListActions(ctx, StatusPending, &invID, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(actions) != 1 {
		t.Errorf("pending actions after 2 sweeps = %d, want exactly 1 (no duplicate)", len(actions))
	}
}

func TestSelfReviewOnce_PendingSmithProposalSupersededWhenFindingClean(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	setSetting(t, db, SettingSelfReview, `{"enabled":true,"grace_minutes":30}`)
	ctx := context.Background()

	old := now.Add(-time.Hour)
	findingIDs, err := s.persistFindings(ctx,
		[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"}},
		SweepManual, old, nil)
	if err != nil {
		t.Fatalf("persistFindings: %v", err)
	}
	fID := findingIDs[0]

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "auto-proposed", Risk: RiskInfo, CreatedBy: "smith", FindingID: &fID,
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	forceActionCreatedAt(t, s, a.ID, old)

	res, err := s.selfReviewOnce(ctx)
	if err != nil {
		t.Fatalf("selfReviewOnce: %v", err)
	}
	if res.ActionsSuperseded != 1 {
		t.Errorf("ActionsSuperseded = %d, want 1", res.ActionsSuperseded)
	}
	got, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Status != StatusSuperseded {
		t.Errorf("status = %s, want superseded", got.Status)
	}
}

func TestSelfReviewOnce_NonSmithOrNoFindingIDPendingActionsUntouched(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	setSetting(t, db, SettingSelfReview, `{"enabled":true,"grace_minutes":30}`)
	ctx := context.Background()

	old := now.Add(-time.Hour)
	findingIDs, err := s.persistFindings(ctx,
		[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"}},
		SweepManual, old, nil)
	if err != nil {
		t.Fatalf("persistFindings: %v", err)
	}
	fID := findingIDs[0]

	// A human-created proposal (not smith) with the same stale, now-clean
	// finding attached — must be left alone; self-review only ever supersedes
	// its own auto-proposals.
	human, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "human proposal", Risk: RiskInfo, CreatedBy: "operator", FindingID: &fID,
	})
	if err != nil {
		t.Fatalf("CreateAction (human): %v", err)
	}
	forceActionCreatedAt(t, s, human.ID, old)

	// A smith-created proposal with no FindingID at all — nothing to
	// re-check against, must be left alone too.
	noFinding, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "smith proposal, no finding", Risk: RiskInfo, CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction (no finding): %v", err)
	}
	forceActionCreatedAt(t, s, noFinding.ID, old)

	res, err := s.selfReviewOnce(ctx)
	if err != nil {
		t.Fatalf("selfReviewOnce: %v", err)
	}
	if res.ActionsSuperseded != 0 {
		t.Errorf("ActionsSuperseded = %d, want 0", res.ActionsSuperseded)
	}
	for _, id := range []int64{human.ID, noFinding.ID} {
		got, err := s.GetAction(ctx, id)
		if err != nil {
			t.Fatalf("GetAction(%d): %v", id, err)
		}
		if got.Status != StatusPending {
			t.Errorf("action %d status = %s, want still pending", id, got.Status)
		}
	}
}

func TestSelfReviewOnce_Disabled(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	setSetting(t, db, SettingSelfReview, `{"enabled":false,"grace_minutes":30}`)
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "manual", "test")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	forceInvestigationOpenedAt(t, s, invID, now.Add(-24*time.Hour))
	if _, err := s.persistFindings(ctx,
		[]Finding{{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"}},
		SweepManual, now.Add(-24*time.Hour), &invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	res, err := s.selfReviewOnce(ctx)
	if err != nil {
		t.Fatalf("selfReviewOnce: %v", err)
	}
	if res.InvestigationsReviewed != 0 || res.InvestigationsProposed != 0 {
		t.Errorf("disabled self-review still reviewed/proposed: %+v", res)
	}
}

func TestMaybeSelfReview_RespectsInterval(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	base := s.d.Now()

	s.maybeSelfReview(context.Background(), base)
	waitFor(t, time.Second, func() bool {
		s.selfReviewMu.Lock()
		defer s.selfReviewMu.Unlock()
		return !s.lastSelfReviewAt.IsZero()
	})
	s.selfReviewMu.Lock()
	first := s.lastSelfReviewAt
	s.selfReviewMu.Unlock()

	// Well within the interval — must not fire again.
	s.maybeSelfReview(context.Background(), base.Add(time.Minute))
	time.Sleep(20 * time.Millisecond)
	s.selfReviewMu.Lock()
	second := s.lastSelfReviewAt
	s.selfReviewMu.Unlock()
	if !second.Equal(first) {
		t.Errorf("lastSelfReviewAt changed within the interval: %v -> %v", first, second)
	}
}
