// SPDX-License-Identifier: Apache-2.0

package smith

// check_now_test.go — S7-followup smith UX sprint (2026-08-26):
// CheckPendingRunbook, the on-demand "check now" that replaces the removed
// self-attestation "done — I ran it myself" button. Mirrors
// recheck_test.go's shape (clean / still-failing / no-check / investigation-
// attached), since it shares resolveRunbookRecheckTargets with RecheckRunbook
// — the only real difference is what happens to the action's own state.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckPendingRunbookStandaloneClean(t *testing.T) {
	s := recheckSmith(t, true)
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "t", Risk: RiskInfo,
		Detail:    mustJSON(t, map[string]any{"check_id": "always_on_ports"}),
		CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	re, err := s.CheckPendingRunbook(ctx, a.ID, "operator")
	if err != nil {
		t.Fatalf("CheckPendingRunbook: %v", err)
	}
	if re.Status != StatusSuperseded {
		t.Errorf("status = %s, want superseded (smith verified it clean, closed it itself)", re.Status)
	}
	if re.Result == nil || !re.Result.OK {
		t.Fatalf("result = %+v, want ok:true on a clean check", re.Result)
	}
}

func TestCheckPendingRunbookStandaloneStillFailing(t *testing.T) {
	s := recheckSmith(t, false)
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "t", Risk: RiskInfo,
		Detail:    mustJSON(t, map[string]any{"check_id": "always_on_ports"}),
		CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	_, err = s.CheckPendingRunbook(ctx, a.ID, "operator")
	var stillFailing *RunbookStillFailingError
	if !errors.As(err, &stillFailing) {
		t.Fatalf("CheckPendingRunbook error = %v, want *RunbookStillFailingError", err)
	}
	if len(stillFailing.CheckIDs) != 1 || stillFailing.CheckIDs[0] != "always_on_ports" {
		t.Errorf("still-failing check IDs = %v, want [always_on_ports]", stillFailing.CheckIDs)
	}

	// The action itself must be untouched — still pending, not superseded,
	// not carrying a persisted result (pending has no interim-result slot).
	cur, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if cur.Status != StatusPending {
		t.Errorf("status after a still-failing check-now = %s, want pending (untouched)", cur.Status)
	}
}

func TestCheckPendingRunbookNoCheckToVerify(t *testing.T) {
	s := recheckSmith(t, true)
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "t", Risk: RiskInfo, CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	_, err = s.CheckPendingRunbook(ctx, a.ID, "operator")
	if err == nil || !strings.Contains(err.Error(), "nothing to check") {
		t.Errorf("CheckPendingRunbook error = %v, want a nothing-to-check refusal", err)
	}
	cur, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if cur.Status != StatusPending {
		t.Errorf("status = %s, want still pending (nothing-to-check is a refusal, not a close)", cur.Status)
	}
}

func TestCheckPendingRunbookInvestigationAttachedResolves(t *testing.T) {
	s := recheckSmith(t, true)
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "test")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	invID, err := s.CreateInvestigation(ctx, "manual", "always_on_ports was warn")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	if _, err := s.persistFindings(ctx, []Finding{{
		CheckID: "always_on_ports", Severity: SeverityWarn, Summary: "stt not listening",
	}}, SweepManual, time.Unix(500, 0), &invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "Restart stt by hand", Risk: RiskInfo,
		Detail:          mustJSON(t, map[string]any{"check_id": "always_on_ports"}),
		InvestigationID: &invID, ConversationID: &convID, CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	if _, err := s.CheckPendingRunbook(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("CheckPendingRunbook: %v", err)
	}

	inv, _, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if inv.Status != "resolved" {
		t.Errorf("investigation status = %q, want resolved after a clean check-now", inv.Status)
	}
	if inv.ResolvedByActionID == nil || *inv.ResolvedByActionID != a.ID {
		t.Errorf("resolved_by_action_id = %v, want %d", inv.ResolvedByActionID, a.ID)
	}
}

func TestCheckPendingRunbookGuards(t *testing.T) {
	s := recheckSmith(t, true)
	ctx := context.Background()

	// A done_unverified runbook is not check-now-able — that's
	// RecheckRunbook's job.
	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "t", Risk: RiskInfo,
		Detail:    mustJSON(t, map[string]any{"check_id": "always_on_ports"}),
		CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := s.ApproveAction(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}
	if _, err := s.CheckPendingRunbook(ctx, a.ID, "operator"); err != ErrInvalidTransition {
		t.Errorf("CheckPendingRunbook(done_unverified runbook) error = %v, want ErrInvalidTransition", err)
	}

	// A non-runbook action is never check-now-able.
	b, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindUnloadSlot, Title: "t", Risk: RiskHigh,
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a3"}), CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction(unload_slot): %v", err)
	}
	if _, err := s.CheckPendingRunbook(ctx, b.ID, "operator"); err == nil ||
		!strings.Contains(err.Error(), "only to runbook") {
		t.Errorf("CheckPendingRunbook(non-runbook) error = %v, want a runbook-only refusal", err)
	}
}
