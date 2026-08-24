// SPDX-License-Identifier: Apache-2.0

package smith

// recheck_test.go — Sprint S4 (§5.5): the "done — I ran it myself" runbook
// post-verify re-check. Guardrail tests: standalone re-check (clean vs still
// failing), the honest "no check to re-verify" answer for a runbook with no
// source check, and the investigation-attached flow into the §2.4.1
// resolution loop (reusing proposeResolution's precedent).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

func recheckSmith(t *testing.T, portsUp bool) *Smith {
	t.Helper()
	cfg := &config.Config{Ports: map[string]int{"embedding": 8083, "stt": 8084}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8083: portsUp, 8084: portsUp}
	return answerSmith(t, snap, func(d *Deps) {
		d.Cfg = func() *config.Config { return cfg }
	})
}

func TestRecheckRunbookStandaloneClean(t *testing.T) {
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
	if _, err := s.ApproveAction(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}

	re, err := s.RecheckRunbook(ctx, a.ID, "operator")
	if err != nil {
		t.Fatalf("RecheckRunbook: %v", err)
	}
	if re.Status != StatusDoneUnverified {
		t.Errorf("status = %s, want done_unverified (runbook terminal state)", re.Status)
	}
	if re.Result == nil || !re.Result.OK {
		t.Fatalf("result = %+v, want ok:true on a clean re-check", re.Result)
	}
	if !strings.Contains(re.Result.Message, "clean") {
		t.Errorf("result message = %q, want it to say clean", re.Result.Message)
	}
	if len(re.Result.Verify) == 0 || re.Result.Verify[0].CheckID != "always_on_ports" {
		t.Errorf("verify = %+v, want an always_on_ports entry", re.Result.Verify)
	}
}

func TestRecheckRunbookStandaloneStillFailing(t *testing.T) {
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
	if _, err := s.ApproveAction(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}

	re, err := s.RecheckRunbook(ctx, a.ID, "operator")
	if err != nil {
		t.Fatalf("RecheckRunbook: %v", err)
	}
	if re.Result == nil || re.Result.OK {
		t.Fatalf("result = %+v, want ok:false when the check still fails", re.Result)
	}
	if !strings.Contains(re.Result.Message, "still failing") {
		t.Errorf("result message = %q, want it to say still failing", re.Result.Message)
	}
}

func TestRecheckRunbookNoCheckToReverify(t *testing.T) {
	s := recheckSmith(t, true)
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "t", Risk: RiskInfo, CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := s.ApproveAction(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}

	re, err := s.RecheckRunbook(ctx, a.ID, "operator")
	if err != nil {
		t.Fatalf("RecheckRunbook: %v", err)
	}
	if re.Result == nil || !re.Result.OK {
		t.Fatalf("result = %+v, want ok:true (honest no-op, not a failure)", re.Result)
	}
	if !strings.Contains(re.Result.Message, "no check to re-verify") {
		t.Errorf("result message = %q, want the honest no-check answer", re.Result.Message)
	}
}

func TestRecheckRunbookInvestigationAttachedResolves(t *testing.T) {
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
	if _, err := s.ApproveAction(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}

	if _, err := s.RecheckRunbook(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("RecheckRunbook: %v", err)
	}

	inv, _, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if inv.Status != "resolved" {
		t.Errorf("investigation status = %q, want resolved after a clean re-check", inv.Status)
	}
	if inv.ResolvedByActionID == nil || *inv.ResolvedByActionID != a.ID {
		t.Errorf("resolved_by_action_id = %v, want %d", inv.ResolvedByActionID, a.ID)
	}

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var foundSummary bool
	for _, m := range msgs {
		if m.Kind == MsgKindDeterministic && strings.Contains(m.Content, "fixed") {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Error("expected a resolution summary message containing 'fixed'")
	}
}

func TestRecheckRunbookSelfReviewCloseIgnoresUnrelatedAmbientWarn(t *testing.T) {
	// Companion to TestApproveSelfReviewClose's ambient-warn case, on the
	// recheck path instead of the initial approval: a self_review_close
	// runbook's recheck must narrow to relevantWarnCritCheckIDs the same way
	// approveSelfReviewClose does, or an investigation whose only self-review
	// closure runbook already went to done_unverified (e.g. because approval
	// hit a transient regression) could never be closed via recheck either —
	// found live 2026-08-19 alongside the approval-time version of this bug.
	s := New(Deps{Store: openDB(t), Logf: func(string, ...any) {}})
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "anomaly:GTT_DRAIN_TIMEOUT", "test")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	// slot_agreement is anomaly-relevant and re-checks clean (no Sched
	// wired). brain_resolvable is unrelated ambient noise (no smith.model
	// set) and re-checks warn deterministically — must not block closure.
	if _, err := s.persistFindings(ctx,
		[]Finding{
			{CheckID: "slot_agreement", Severity: SeverityWarn, Summary: "mismatch"},
			{CheckID: "brain_resolvable", Severity: SeverityWarn, Summary: "smith.model is unset"},
		},
		SweepAnomaly, s.d.Now(), &invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "self-review closure", Risk: RiskInfo, CreatedBy: "smith",
		InvestigationID: &invID,
		Detail: mustJSON(t, map[string]any{
			"self_review_close": true, "investigation_id": invID,
		}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := s.ApproveAction(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}

	// Approval itself already closes it in the fixed code — recheck must
	// keep it closed (idempotent), and must not require another ambient-warn
	// re-verify to have been the thing that got it there.
	inv, _, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if inv.Status != "resolved" {
		t.Fatalf("investigation status after approve = %s, want resolved", inv.Status)
	}

	if _, err := s.RecheckRunbook(ctx, a.ID, "operator"); err != nil {
		t.Fatalf("RecheckRunbook: %v", err)
	}
	inv, _, err = s.GetInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if inv.Status != "resolved" {
		t.Errorf("investigation status after recheck = %s, want still resolved — unrelated ambient warn must not reopen it", inv.Status)
	}
}

func TestRecheckRunbookGuards(t *testing.T) {
	s := recheckSmith(t, true)
	ctx := context.Background()

	// A pending runbook is not re-checkable (must be "done — I ran it myself" first).
	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRunbook, Title: "t", Risk: RiskInfo,
		Detail:    mustJSON(t, map[string]any{"check_id": "always_on_ports"}),
		CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := s.RecheckRunbook(ctx, a.ID, "operator"); err != ErrInvalidTransition {
		t.Errorf("RecheckRunbook(pending runbook) error = %v, want ErrInvalidTransition", err)
	}

	// A non-runbook action is never re-checkable.
	b, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindUnloadSlot, Title: "t", Risk: RiskHigh,
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a3"}), CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction(unload_slot): %v", err)
	}
	if _, err := s.RecheckRunbook(ctx, b.ID, "operator"); err == nil ||
		!strings.Contains(err.Error(), "only to runbook") {
		t.Errorf("RecheckRunbook(non-runbook) error = %v, want a runbook-only refusal", err)
	}
}
