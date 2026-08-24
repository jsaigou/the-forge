// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// self_review.go — Thread C's periodic self-review sweep
// (docs/v5-smith-efficiency.md §4): the real missing capability was that
// nothing ever re-validates an open investigation or a parked
// done_unverified/pending action — retention.go (P7) only ages out
// standalone findings and permanently exempts anything investigation-
// attached. proposeResolution (investigations.go) is purely reactive: it
// only fires right after a specific action reaches status=done. This sweep
// is the proactive counterpart, gated behind smith.self_review
// (Enabled/GraceMinutes) and always propose-only for anything that closes
// an investigation — never a silent auto-resolve (§3 constraint 1's
// "propose, never do" posture, same as everywhere else in the action
// model except the pre-existing device-lost auto-recover carve-out).

// selfReviewInterval is how often scheduleLoop's 1-minute tick considers
// running a self-review — a fixed constant, mirroring retentionInterval
// (not settings-driven; only enabled/grace_minutes are operator-tunable).
const selfReviewInterval = 2 * time.Hour

// SelfReviewResult is one sweep's outcome — surfaced on GET /smith/status
// (SelfContext.SelfReview) so "does self-review actually run" is a live
// status check, not a leap of faith, same posture as RetentionResult.
type SelfReviewResult struct {
	RanAt                  time.Time `json:"ran_at"`
	InvestigationsReviewed int       `json:"investigations_reviewed"`
	InvestigationsProposed int       `json:"investigations_proposed"` // close-proposals created this run
	ActionsPromoted        int       `json:"actions_promoted"`        // done_unverified -> done
	ActionsSuperseded      int       `json:"actions_superseded"`      // pending -> superseded, moot
}

// SelfReviewStatus is the SelfContext projection of the last sweep.
// LastRunAt is nil until the first run completes (never faked as "now").
type SelfReviewStatus struct {
	Enabled                bool   `json:"enabled"`
	LastRunAt              *int64 `json:"last_run_at"`
	ActionsPromoted        int    `json:"actions_promoted"`
	ActionsSuperseded      int    `json:"actions_superseded"`
	InvestigationsProposed int    `json:"investigations_proposed"`
}

// maybeSelfReview runs a sweep when the last one is older than
// selfReviewInterval, called from scheduleLoop's 1-minute tick — the exact
// maybePrune/maybeProbeWeb shape.
func (s *Smith) maybeSelfReview(ctx context.Context, now time.Time) {
	s.selfReviewMu.Lock()
	due := now.Sub(s.lastSelfReviewAt) >= selfReviewInterval
	s.selfReviewMu.Unlock()
	if due {
		go s.selfReviewOnce(ctx)
	}
}

// selfReviewOnce runs one self-review pass: open investigations whose
// warn/crit findings now re-check clean get a closure *proposal* (never an
// auto-resolve); done_unverified actions (excluding runbooks, whose
// done_unverified is a documented terminal state) whose verify checks now
// re-check clean against a fresh snapshot get promoted straight to done —
// promoting a bookkeeping status to match reality that already happened is
// not a new real-world action, so no extra approval gate; smith's own
// pending proposals whose originating finding no longer reads warn/crit get
// superseded, same reasoning. Settings are re-read every call, so an
// operator edit lands on the next scheduled run with no restart, same as
// smith.schedule/smith.retention.
func (s *Smith) selfReviewOnce(ctx context.Context) (SelfReviewResult, error) {
	now := s.d.Now()
	res := SelfReviewResult{RanAt: now}
	s.selfReviewMu.Lock()
	s.lastSelfReviewAt = now
	s.selfReviewMu.Unlock()

	if s.d.Store == nil {
		return res, nil
	}
	cfg := s.SelfReviewConfig(ctx)
	if !cfg.Enabled {
		s.recordSelfReviewResult(res)
		return res, nil
	}
	grace := time.Duration(cfg.GraceMinutes) * time.Minute

	s.reviewInvestigations(ctx, now, grace, &res)
	s.reviewDoneUnverifiedActions(ctx, now, grace, &res)
	s.reviewPendingProposals(ctx, now, grace, &res)

	s.recordSelfReviewResult(res)
	return res, nil
}

// reviewInvestigations re-checks each open investigation older than grace;
// one whose *relevant* warn/crit findings all re-check clean (or had none
// to begin with, mirroring proposeResolution's own empty-branch case) gets
// a closure proposal via proposeInvestigationClosure — never resolved
// directly here. "Relevant" is relevantWarnCritCheckIDs, not the full
// findings trail — an anomaly-triggered investigation's initial deep sweep
// can carry ambient warns unrelated to what actually triggered it (see
// anomalyRelevantChecks), which would otherwise block closure forever.
func (s *Smith) reviewInvestigations(ctx context.Context, now time.Time, grace time.Duration, res *SelfReviewResult) {
	invs, err := s.ListInvestigations(ctx, "open")
	if err != nil {
		s.logf("self-review: list investigations: %v", err)
		return
	}
	for _, inv := range invs {
		if now.Sub(time.Unix(inv.OpenedAt, 0)) < grace {
			continue
		}
		res.InvestigationsReviewed++

		_, findings, err := s.GetInvestigation(ctx, inv.ID)
		if err != nil {
			s.logf("self-review: get investigation %d: %v", inv.ID, err)
			continue
		}
		ids := relevantWarnCritCheckIDs(inv.Trigger, findings)
		allClean := true
		if len(ids) > 0 {
			for _, f := range s.runChecksBare(ctx, ids) {
				if f.Severity == SeverityWarn || f.Severity == SeverityCrit {
					allClean = false
					break
				}
			}
		}
		if !allClean {
			continue
		}
		if s.proposeInvestigationClosure(ctx, inv.ID, ids) {
			res.InvestigationsProposed++
		}
	}
}

// proposeInvestigationClosure creates (or, via the existing dedupe/supersede
// machinery, reuses) a runbook action carrying the self_review_close marker
// actions.go's approveRunbook checks for — approving it is the human
// confirmation gate that actually closes the investigation, re-verified
// fresh at approval time (approveSelfReviewClose), not trusted blindly from
// this sweep's possibly-stale snapshot. Returns whether a NEW proposal was
// created (false when an identical one was already pending — dedupe keeps a
// repeat sweep before approval from spamming duplicates).
func (s *Smith) proposeInvestigationClosure(ctx context.Context, invID int64, checkIDs []string) bool {
	steps := []RunbookStep{{
		Title:  fmt.Sprintf("Review investigation #%d's re-checked evidence, then Approve to close", invID),
		Why:    "self-review found the checks that were previously warn/crit on this investigation now read clean",
		Verify: "the investigation closes and a summary is posted to its linked conversation, if any",
	}}
	detail, err := json.Marshal(map[string]any{
		"self_review_close": true,
		"investigation_id":  invID,
		"check_ids":         checkIDs,
		"steps":             steps,
	})
	if err != nil {
		s.logf("self-review: marshal closure proposal for investigation %d: %v", invID, err)
		return false
	}
	invIDCopy := invID
	draft := ActionDraft{
		Kind:            KindRunbook,
		Title:           fmt.Sprintf("Self-review: investigation #%d's checks are clean — confirm closure", invID),
		Risk:            RiskInfo,
		Detail:          detail,
		DedupeKey:       fmt.Sprintf("selfreview_close:%d", invID),
		InvestigationID: &invIDCopy,
		CreatedBy:       "smith",
	}
	_, inserted, err := s.createOrReuseProposal(ctx, draft)
	if err != nil {
		s.logf("self-review: propose closure for investigation %d: %v", invID, err)
		return false
	}
	return inserted
}

// reviewDoneUnverifiedActions re-verifies each non-runbook done_unverified
// action older than grace and promotes it to done when its verify checks
// now re-check clean against a snapshot genuinely newer than execution
// (docs/v5-smith-experience.md §8 item 24 — previously stranded forever).
func (s *Smith) reviewDoneUnverifiedActions(ctx context.Context, now time.Time, grace time.Duration, res *SelfReviewResult) {
	actions, err := s.ListActions(ctx, StatusDoneUnverified, nil, 0)
	if err != nil {
		s.logf("self-review: list done_unverified actions: %v", err)
		return
	}
	for i := range actions {
		a := &actions[i]
		if a.Kind == KindRunbook {
			continue // a runbook's done_unverified is its documented terminal state
		}
		if a.ExecutedAt == nil || now.Sub(time.Unix(*a.ExecutedAt, 0)) < grace {
			continue
		}
		if s.promoteDoneUnverified(ctx, a) {
			res.ActionsPromoted++
		}
	}
}

// promoteDoneUnverified re-runs a's verify checks (verifyChecksFor, the same
// mapping finalizeResult used originally) and CASes done_unverified -> done
// when they're clean and the current collector snapshot is now newer than
// execution — the exact freshness test finalizeResult applies at execution
// time, just re-tried later once the collector has actually caught up.
// KindRunbook actions are excluded by the caller; verifyChecksFor also has
// no case for "runbook" (defense in depth). On a successful promotion, an
// investigation-attached action re-triggers the existing proposeResolution
// reactive close path — the same call maybeProposeResolution makes after a
// normal execute.
func (s *Smith) promoteDoneUnverified(ctx context.Context, a *Action) bool {
	var unit string
	if a.Kind == KindRestartForgeUnit {
		if d, err := parseDetail[restartUnitDetail](a.Detail); err == nil {
			unit = d.Unit
		}
	}
	verifyIDs := verifyChecksFor(a.Kind, unit)
	if len(verifyIDs) == 0 {
		return false
	}
	findings := s.runChecksBare(ctx, verifyIDs)
	for _, f := range findings {
		if f.Severity == SeverityWarn || f.Severity == SeverityCrit {
			return false
		}
	}
	if s.d.Source == nil || a.ExecutedAt == nil {
		return false
	}
	snap := s.d.Source.Current()
	if snap == nil || !snap.TakenAt.After(time.Unix(*a.ExecutedAt, 0)) {
		return false // still stale — leave it for a later sweep
	}

	result := ActionResult{
		OK:      true,
		Message: "self-review: re-verified clean against a fresh snapshot, promoted to done",
		Verify:  toVerifyResults(findings, s.d.Now()),
	}
	b, err := json.Marshal(result)
	if err != nil {
		s.logf("self-review: marshal promotion result for action %d: %v", a.ID, err)
		return false
	}
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, result = ?, verified_at = ? WHERE id = ? AND status = ?`,
		StatusDone, string(b), s.d.Now().Unix(), a.ID, StatusDoneUnverified)
	if err != nil {
		s.logf("self-review: promote action %d: %v", a.ID, err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	updated, err := s.GetAction(ctx, a.ID)
	if err != nil {
		s.logf("self-review: refetch promoted action %d: %v", a.ID, err)
		return true
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: "smith", Action: "smith_action_self_review_promote",
			Target: fmt.Sprintf("%d:%s", updated.ID, updated.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	s.publishActionUpdate(ctx, updated, StatusDone)
	if updated.InvestigationID != nil {
		s.proposeResolution(ctx, updated.ID, *updated.InvestigationID)
	}
	return true
}

// reviewPendingProposals supersedes smith's own pending auto-proposals whose
// originating finding no longer reads warn/crit — the condition that
// prompted the proposal is gone, so the proposal is moot. Only ever touches
// smith-created proposals with a FindingID (never a human-created or
// manually-proposed action) and only ever moves pending -> superseded, an
// already-legal transition that removes a stale suggestion from the queue
// without touching anything in the world — same risk class as
// createOrReuseProposal's existing auto-supersede-on-changed-detail
// behavior, so no extra approval gate.
func (s *Smith) reviewPendingProposals(ctx context.Context, now time.Time, grace time.Duration, res *SelfReviewResult) {
	actions, err := s.ListActions(ctx, StatusPending, nil, 0)
	if err != nil {
		s.logf("self-review: list pending actions: %v", err)
		return
	}
	for _, a := range actions {
		if a.CreatedBy != "smith" || a.FindingID == nil {
			continue
		}
		if now.Sub(time.Unix(a.CreatedAt, 0)) < grace {
			continue
		}
		checkID, err := s.findingCheckID(ctx, *a.FindingID)
		if err != nil || checkID == "" {
			continue
		}
		findings := s.runChecksBare(ctx, []string{checkID})
		if len(findings) != 1 {
			continue
		}
		if findings[0].Severity == SeverityWarn || findings[0].Severity == SeverityCrit {
			continue // still real, leave the proposal pending
		}
		note := fmt.Sprintf("self-review: underlying condition (%s) no longer failing, proposal is moot", checkID)
		if s.supersedeActionWithNote(ctx, a.ID, note) {
			res.ActionsSuperseded++
		}
	}
}

// findingCheckID looks up one finding's check_id — no full StoredFinding
// getter exists for a single ID (findings.go's readers all list/filter),
// and this needs only the one column.
func (s *Smith) findingCheckID(ctx context.Context, id int64) (string, error) {
	var checkID string
	err := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT check_id FROM smith_findings WHERE id = ?`, id).Scan(&checkID)
	if err != nil {
		return "", fmt.Errorf("smith: finding check_id %d: %w", id, err)
	}
	return checkID, nil
}

// recordSelfReviewResult stores res as the latest sweep outcome, guarded by
// mu (mirroring recordRetentionResult).
func (s *Smith) recordSelfReviewResult(res SelfReviewResult) {
	s.mu.Lock()
	s.lastSelfReview = res
	s.mu.Unlock()
}

// selfReviewStatus assembles SelfContext.SelfReview — a pure read of the
// in-memory last-run record, never triggers a sweep itself.
func (s *Smith) selfReviewStatus(ctx context.Context) SelfReviewStatus {
	s.mu.Lock()
	last := s.lastSelfReview
	s.mu.Unlock()
	st := SelfReviewStatus{Enabled: s.SelfReviewConfig(ctx).Enabled}
	if !last.RanAt.IsZero() {
		ts := last.RanAt.Unix()
		st.LastRunAt = &ts
		st.ActionsPromoted = last.ActionsPromoted
		st.ActionsSuperseded = last.ActionsSuperseded
		st.InvestigationsProposed = last.InvestigationsProposed
	}
	return st
}
