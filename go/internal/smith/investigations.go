// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/bus"
)

// EventNotificationNew is the bus event name for new notifications, mirrored
// from httpapi.EventNotificationNew. smith cannot import httpapi (it's the
// reverse dependency), so the name is duplicated here. The notification
// pipeline (httpapi/notifications_handlers.go) publishes this event when an
// alert first becomes active.
const EventNotificationNew = "notification:new"

// critNotificationCodes are the notification codes that trigger an anomaly
// investigation. They mirror notificationSeverity()'s "crit" classification
// in httpapi/notifications_handlers.go — smith cannot import httpapi, so the
// set is duplicated here with a comment pointing back to the source.
var critNotificationCodes = map[string]bool{
	"INFERENCE_HANG":   true,
	"UNIT_OOM":         true,
	"UNIT_CRASH":       true,
	"SLOT_ERROR_STORM": true,
	// GTT_DRAIN_TIMEOUT is the pre-hang/post-unload GTT-lingering signal
	// (engine waitGTTDrain, surfaced via OnGTTDrainTimeout). Not an
	// emergency on its own, but it fired before both 2026-08-16 device-lost
	// hangs — worth an investigation, not a notification-only.
	"GTT_DRAIN_TIMEOUT": true,
}

// anomalyRelevantChecks narrows self-review's investigation-closure gate
// (self_review.go's reviewInvestigations, via relevantWarnCritCheckIDs) to
// the checks that actually represent each anomaly code's own condition,
// instead of every warn/crit check_id ever swept into the investigation's
// findings trail. The initial deep sweep (handleAnomaly) deliberately stays
// broad — full diagnostic context is still valuable to a human reading the
// trail — this map only affects what must read clean to *propose* closure.
// Found live 2026-08-19: every "anomaly:GTT_DRAIN_TIMEOUT" investigation
// also carried ambient, frequently-true warns unrelated to the drain
// condition (comfyui_prune while ComfyUI is simply off, brain_resolvable
// while smith's brain is simply idle/unloaded — both normal steady states,
// not the GTT problem), so self-review's "every warn/crit ever seen must
// clear" gate could functionally never fire for these investigations. Only
// codes with real evidence behind the mapping are listed; a code absent
// here falls back to the full unnarrowed set (today's pre-existing
// behavior) rather than risk hiding a genuinely relevant check.
var anomalyRelevantChecks = map[string][]string{
	// GTT_DRAIN_TIMEOUT: engine's post-unload GTT-lingering signal
	// (waitGTTDrain / OnGTTDrainTimeout, see critNotificationCodes above) —
	// about GTT memory not draining and the hang risk it precedes.
	"GTT_DRAIN_TIMEOUT": {"gtt_ceiling", "gpu_hang", "slot_agreement"},
	// INFERENCE_HANG: the collector's own hang detector (requests_processing
	// > 0 and TPS < 0.1 sustained 90s) — gpu_hang IS this condition;
	// slot_agreement catches an inconsistent unit/scheduler state behind it.
	"INFERENCE_HANG": {"gpu_hang", "slot_agreement"},
	// SLOT_ERROR_STORM: repeated slot errors — mirrors the check set
	// autorecover.go's openRecoveryInvestigation already uses for the same
	// underlying device-lost condition.
	"SLOT_ERROR_STORM": {"slot_agreement", "gpu_device_lost"},
}

// Investigation is one diagnostic investigation (smith_investigations table,
// docs/v5-smith.md §4.4). An investigation is an ordered evidence trail:
// auto-seeded findings from its trigger, check runs added manually or by
// the anomaly hook, and (from a later phase) Tier 2 commentary. Resolution
// requires a human close.
type Investigation struct {
	ID             int64  `json:"id"`
	Trigger        string `json:"trigger"`
	Status         string `json:"status"`
	OpenedAt       int64  `json:"opened_at"`
	ClosedAt       *int64 `json:"closed_at"`
	Summary        string `json:"summary"`
	ConversationID *int64 `json:"conversation_id"`
	// ResolvedByActionID (S3, migration 0055) points at the smith_actions row
	// whose verified execution closed this investigation. omitempty so the
	// wire key is absent (not null) for investigations that predate S3 or
	// were closed by hand — additive to the frozen wire shape.
	ResolvedByActionID *int64 `json:"resolved_by_action_id,omitempty"`
}

// CheckMeta is the check catalog metadata surfaced by GET /api/v1/smith/checks
// (id/name/category/fast) for the FE's custom check picker.
type CheckMeta struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Fast     bool   `json:"fast"`
}

// CreateInvestigation inserts a new investigation row and returns its ID.
// trigger is "manual" for API-opened investigations or "anomaly:<code>" for
// anomaly-triggered ones.
func (s *Smith) CreateInvestigation(ctx context.Context, trigger, summary string) (int64, error) {
	if s.d.Store == nil {
		return 0, ErrStoreUnwired
	}
	now := s.d.Now().Unix()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`INSERT INTO smith_investigations (trigger, status, opened_at, summary) VALUES (?, 'open', ?, ?)`,
		trigger, now, summary)
	if err != nil {
		return 0, fmt.Errorf("smith: create investigation: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// ListInvestigations returns persisted investigations, newest first. status
// filters on exact match ("" = all).
func (s *Smith) ListInvestigations(ctx context.Context, status string) ([]Investigation, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	query := `SELECT id, trigger, status, opened_at, closed_at, summary, conversation_id, resolved_by_action_id
	          FROM smith_investigations`
	args := []any{}
	if status != "" {
		query += " WHERE status = ?"
		args = append(args, status)
	}
	query += " ORDER BY opened_at DESC, id DESC"

	rows, err := s.d.Store.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("smith: list investigations: %w", err)
	}
	defer rows.Close()

	out := []Investigation{}
	for rows.Next() {
		var inv Investigation
		var openedAt int64
		var closedAt, convID, resolvedBy sql.NullInt64
		if err := rows.Scan(&inv.ID, &inv.Trigger, &inv.Status, &openedAt,
			&closedAt, &inv.Summary, &convID, &resolvedBy); err != nil {
			return nil, fmt.Errorf("smith: scan investigation: %w", err)
		}
		inv.OpenedAt = openedAt
		if closedAt.Valid {
			v := closedAt.Int64
			inv.ClosedAt = &v
		}
		if convID.Valid {
			v := convID.Int64
			inv.ConversationID = &v
		}
		if resolvedBy.Valid {
			v := resolvedBy.Int64
			inv.ResolvedByActionID = &v
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// GetInvestigation returns an investigation's metadata plus its findings
// (newest first). Returns sql.ErrNoRows wrapped if the investigation doesn't
// exist.
func (s *Smith) GetInvestigation(ctx context.Context, id int64) (*Investigation, []StoredFinding, error) {
	if s.d.Store == nil {
		return nil, nil, ErrStoreUnwired
	}
	var inv Investigation
	var openedAt int64
	var closedAt, convID, resolvedBy sql.NullInt64
	err := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT id, trigger, status, opened_at, closed_at, summary, conversation_id, resolved_by_action_id
		 FROM smith_investigations WHERE id = ?`, id).
		Scan(&inv.ID, &inv.Trigger, &inv.Status, &openedAt, &closedAt, &inv.Summary, &convID, &resolvedBy)
	if err != nil {
		return nil, nil, fmt.Errorf("smith: get investigation: %w", err)
	}
	inv.OpenedAt = openedAt
	if closedAt.Valid {
		v := closedAt.Int64
		inv.ClosedAt = &v
	}
	if convID.Valid {
		v := convID.Int64
		inv.ConversationID = &v
	}
	if resolvedBy.Valid {
		v := resolvedBy.Int64
		inv.ResolvedByActionID = &v
	}

	findings, err := s.findingsForInvestigation(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("smith: get investigation findings: %w", err)
	}
	return &inv, findings, nil
}

// findingsForInvestigation returns the findings attached to an investigation,
// newest first.
func (s *Smith) findingsForInvestigation(ctx context.Context, invID int64) ([]StoredFinding, error) {
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT id, investigation_id, check_id, severity, summary, evidence, sweep_kind, created_at, kb_refs, repeat_count
		 FROM smith_findings WHERE investigation_id = ?
		 ORDER BY created_at DESC, id DESC`, invID)
	if err != nil {
		return nil, fmt.Errorf("smith: list investigation findings: %w", err)
	}
	defer rows.Close()

	out := []StoredFinding{}
	for rows.Next() {
		var sf StoredFinding
		var invIDCol sql.NullInt64
		var createdAt int64
		var kbRefsJSON string
		if err := rows.Scan(&sf.ID, &invIDCol, &sf.CheckID, &sf.Severity,
			&sf.Summary, &sf.Evidence, &sf.SweepKind, &createdAt, &kbRefsJSON, &sf.RepeatCount); err != nil {
			return nil, fmt.Errorf("smith: scan finding: %w", err)
		}
		if invIDCol.Valid {
			sf.InvestigationID = &invIDCol.Int64
		}
		sf.CreatedAt = time.Unix(createdAt, 0).UTC()
		sf.KBRefs = unmarshalKBRefs(kbRefsJSON)
		out = append(out, sf)
	}
	return out, rows.Err()
}

// RunChecksIntoInvestigation runs a check sweep and persists the findings
// WITH investigation_id set (docs/v5-smith.md §4.4). The sweep is serialized
// with RunChecks via the same sweeping flag. sweepKind is the persistence
// attribution (manual|scheduled|anomaly).
func (s *Smith) RunChecksIntoInvestigation(ctx context.Context, invID int64, checkIDs []string, scope, sweepKind string) ([]Finding, error) {
	s.mu.Lock()
	if s.sweeping {
		s.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	s.sweeping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sweeping = false
		s.mu.Unlock()
	}()

	selected, err := selectChecks(scope, checkIDs)
	if err != nil {
		return nil, err
	}
	switch sweepKind {
	case SweepManual, SweepScheduled, SweepAnomaly:
	default:
		sweepKind = SweepManual
	}

	env := s.checkEnv(ctx)
	findings := make([]Finding, 0, len(selected))
	for _, c := range selected {
		f := runOne(ctx, c, env)
		findings = append(findings, f.normalize())
	}

	at := s.d.Now()
	ids, err := s.persistFindings(ctx, findings, sweepKind, at, &invID)
	if err != nil {
		s.logf("persist findings into investigation %d (%s): %v", invID, sweepKind, err)
	}
	s.proposeFrom(ctx, env, findings, ids, &invID)

	return findings, nil
}

// ResolveInvestigation sets the investigation's status and closed_at. status
// must be "resolved" or "dismissed". Also cleans up the anomaly debounce map
// so a future notification for the same code opens a new investigation.
func (s *Smith) ResolveInvestigation(ctx context.Context, id int64, status string) error {
	if s.d.Store == nil {
		return ErrStoreUnwired
	}
	if status != "resolved" && status != "dismissed" {
		return fmt.Errorf("smith: invalid status %q (want resolved|dismissed)", status)
	}
	now := s.d.Now().Unix()
	_, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_investigations SET status = ?, closed_at = ? WHERE id = ?`,
		status, now, id)
	if err != nil {
		return fmt.Errorf("smith: resolve investigation: %w", err)
	}

	// Clean up the anomaly debounce map for this investigation.
	s.mu.Lock()
	for code, invID := range s.openAnomaly {
		if invID == id {
			delete(s.openAnomaly, code)
			break
		}
	}
	s.mu.Unlock()

	return nil
}

// setInvestigationResolvedBy stamps the investigation's resolved_by_action_id
// so the FE can render a "resolved by" link pointing at the action that closed
// it (§2.4.1).
func (s *Smith) setInvestigationResolvedBy(ctx context.Context, invID, actionID int64) error {
	if s.d.Store == nil {
		return ErrStoreUnwired
	}
	_, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_investigations SET resolved_by_action_id = ? WHERE id = ?`,
		actionID, invID)
	if err != nil {
		return fmt.Errorf("smith: set investigation resolved_by: %w", err)
	}
	return nil
}

// proposeResolution is the §2.4.1 resolution loop. After an approved action
// finishes post-verify clean (status=done) and it is attached to an
// investigation, this re-runs the investigation's previously-failing checks.
// If they're all clean now: resolve the investigation, post a summary to the
// linked conversation, and stamp resolved_by_action_id. If checks still fail:
// post a summary saying the action executed but the problem persists — do NOT
// resolve. No-op (logged) when no store is wired or the investigation has no
// warn/crit findings to re-check.
func (s *Smith) proposeResolution(ctx context.Context, actionID, invID int64) {
	if s.d.Store == nil {
		return
	}
	inv, findings, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		s.logf("proposeResolution: get investigation %d: %v", invID, err)
		return
	}

	// Collect the check IDs that were warn/crit — the ones the action was
	// meant to fix. ok/info findings are not "problems" to re-verify.
	ids := warnCritFindingCheckIDs(findings)
	if len(ids) == 0 {
		// No warn/crit findings to re-check — nothing to verify against.
		// Still resolve (the action succeeded and there was nothing failing
		// to begin with) and post a summary.
		s.finishResolution(ctx, actionID, invID, inv, nil, true)
		return
	}

	recheck := s.runChecksBare(ctx, ids)
	allClean := true
	var stillFailing []string
	for _, f := range recheck {
		if f.Severity == SeverityWarn || f.Severity == SeverityCrit {
			allClean = false
			stillFailing = append(stillFailing, f.CheckID)
		}
	}
	s.finishResolution(ctx, actionID, invID, inv, stillFailing, allClean)
}

// warnCritFindingCheckIDs returns the distinct warn/crit check IDs from a
// finding list, preserving first-seen order. ok/info findings are not
// "problems" to re-verify, so they're excluded. Shared by proposeResolution
// and RecheckRunbook's investigation-attached path (§5.5).
func warnCritFindingCheckIDs(findings []StoredFinding) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, f := range findings {
		if f.Severity != SeverityWarn && f.Severity != SeverityCrit {
			continue
		}
		if seen[f.CheckID] {
			continue
		}
		seen[f.CheckID] = true
		ids = append(ids, f.CheckID)
	}
	return ids
}

// relevantWarnCritCheckIDs is warnCritFindingCheckIDs, further narrowed to
// anomalyRelevantChecks[code] when trigger is a known "anomaly:<code>" with
// a curated entry there — see that map's comment for why. A manual trigger,
// or an anomaly code with no curated entry, gets the full unnarrowed set
// (today's pre-existing behavior). Used only by self-review's closure gate
// (reviewInvestigations) — proposeResolution's reactive path (verifying one
// specific approved action's fix) intentionally keeps the full set, since
// there the "what must be clean" question is already scoped by the action.
func relevantWarnCritCheckIDs(trigger string, findings []StoredFinding) []string {
	ids := warnCritFindingCheckIDs(findings)
	code, ok := strings.CutPrefix(trigger, "anomaly:")
	if !ok {
		return ids
	}
	relevant, ok := anomalyRelevantChecks[code]
	if !ok {
		return ids
	}
	allow := make(map[string]bool, len(relevant))
	for _, c := range relevant {
		allow[c] = true
	}
	var out []string
	for _, id := range ids {
		if allow[id] {
			out = append(out, id)
		}
	}
	return out
}

// finishResolution posts the resolution summary and (if clean) resolves the
// investigation. Extracted from proposeResolution so the clean/still-failing
// branches share one code path.
func (s *Smith) finishResolution(ctx context.Context, actionID, invID int64, inv *Investigation, stillFailing []string, allClean bool) {
	// Resolve the conversation to post the summary to: prefer the action's
	// conversation, fall back to the investigation's linked conversation.
	action, err := s.GetAction(ctx, actionID)
	if err != nil {
		s.logf("proposeResolution: get action %d: %v", actionID, err)
		return
	}
	convID := int64(0)
	if action.ConversationID != nil {
		convID = *action.ConversationID
	} else if inv.ConversationID != nil {
		convID = *inv.ConversationID
	}

	var summary string
	if allClean {
		summary = fmt.Sprintf("fixed — action #%d completed and its checks are green on re-run.", actionID)
		if err := s.ResolveInvestigation(ctx, invID, "resolved"); err != nil {
			s.logf("proposeResolution: resolve investigation %d: %v", invID, err)
		}
		if err := s.setInvestigationResolvedBy(ctx, invID, actionID); err != nil {
			s.logf("proposeResolution: set resolved_by on investigation %d: %v", invID, err)
		}
	} else {
		summary = fmt.Sprintf("action #%d executed but %d check(s) still failing on re-run: %s — the problem may not be fully resolved.",
			actionID, len(stillFailing), strings.Join(stillFailing, ", "))
	}

	if convID != 0 {
		summary = scrubSecretPatterns(summary)
		if _, err := s.appendMessage(ctx, convID, MsgKindDeterministic, summary, nil, nil, nil, nil, nil); err != nil {
			s.logf("proposeResolution: post summary to conversation %d: %v", convID, err)
		}
	}
}

// ListChecks returns the check catalog metadata (id/name/category/fast), for
// the GET /api/v1/smith/checks endpoint.
func (s *Smith) ListChecks() []CheckMeta {
	out := make([]CheckMeta, len(registry))
	for i, c := range registry {
		out[i] = CheckMeta{ID: c.ID, Name: c.Name, Category: c.Category, Fast: c.Fast}
	}
	return out
}

// ── Anomaly hook (bus subscription → auto-open investigation) ───────────────

// reconcileOpenAnomalies rehydrates the in-memory openAnomaly dedupe map
// from the store. openAnomaly is process-local and starts empty on every
// New() call (smith.go) — without this, every daemon restart forgets which
// anomaly investigations are still open, so the next occurrence of an
// already-open code opens a brand-new investigation instead of reusing it,
// even though debounce is documented (startAnomalyHook's own doc comment)
// as "one open investigation per code." Found live 2026-08-19: 11 duplicate
// open "anomaly:GTT_DRAIN_TIMEOUT" investigations had accumulated across a
// week of frequent redeploys, each one abandoned at the next restart.
// ListInvestigations orders newest-first, so when duplicates already exist
// for a code (pre-existing ones from before this fix, or an unlikely race),
// the most recently opened is adopted as canonical going forward — the
// older duplicates are left untouched, never auto-closed here (this
// codebase's "propose, never do" posture applies to cleanup too; self-review
// or an operator closes them).
func (s *Smith) reconcileOpenAnomalies(ctx context.Context) {
	if s.d.Store == nil {
		return
	}
	invs, err := s.ListInvestigations(ctx, "open")
	if err != nil {
		s.logf("reconcile open anomalies: list investigations: %v", err)
		return
	}
	s.mu.Lock()
	for _, inv := range invs {
		code, ok := strings.CutPrefix(inv.Trigger, "anomaly:")
		if !ok || !critNotificationCodes[code] {
			continue
		}
		if _, exists := s.openAnomaly[code]; exists {
			continue
		}
		s.openAnomaly[code] = inv.ID
	}
	s.mu.Unlock()
}

// startAnomalyHook subscribes to the bus and listens for notification:new
// events. When a crit-class notification arrives, it auto-opens an
// investigation (trigger=anomaly:<code>), runs a deep sweep, and attaches the
// findings with sweep_kind=anomaly. Debounce: one open investigation per
// code — a repeat while open attaches new findings to the existing
// investigation, never a duplicate. No-op when no Subscriber is wired.
//
// Subscribe is called synchronously (before the goroutine starts) so the
// subscriber channel is registered by the time Start returns — events
// published immediately after Start are buffered and never lost.
func (s *Smith) startAnomalyHook(ctx context.Context) {
	if s.d.Subscriber == nil {
		return
	}
	ch := s.d.Subscriber.Subscribe(ctx)
	go s.anomalyLoop(ctx, ch)
}

// anomalyLoop reads bus events until ctx is done. Events arrive one at a
// time (single goroutine consumer), so the debounce map has no concurrent-
// access races within the hook.
func (s *Smith) anomalyLoop(ctx context.Context, ch <-chan bus.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			s.handleBusEvent(ctx, ev)
		}
	}
}

// handleBusEvent filters for notification:new events and dispatches crit-class
// ones to the anomaly handler.
func (s *Smith) handleBusEvent(ctx context.Context, ev bus.Event) {
	if ev.Name != EventNotificationNew {
		return
	}
	data, ok := ev.Data.(map[string]any)
	if !ok {
		return
	}
	code, _ := data["code"].(string)
	if code == "" {
		return
	}
	if !critNotificationCodes[code] {
		return
	}
	subject, _ := data["subject"].(string)
	s.handleAnomaly(ctx, code)
	// Device-lost auto-recovery: after opening the anomaly investigation for
	// a SLOT_ERROR_STORM, attempt the trivial unload→reload (autorecover.go).
	// Only for journal-confirmed device-lost; gated by the
	// smith.auto_recover_device_lost setting and a per-slot cooldown.
	s.maybeAutoRecover(ctx, code, subject)
}

// handleAnomaly opens (or finds) an investigation for the given anomaly code
// and runs a deep sweep into it. Debounce: if an open investigation already
// exists for this code, findings are attached to it rather than opening a
// duplicate.
func (s *Smith) handleAnomaly(ctx context.Context, code string) {
	s.mu.Lock()
	invID, exists := s.openAnomaly[code]
	s.mu.Unlock()

	if !exists {
		newID, err := s.CreateInvestigation(ctx, "anomaly:"+code, "")
		if err != nil {
			s.logf("anomaly hook: create investigation for %s: %v", code, err)
			return
		}
		s.mu.Lock()
		// Another event for the same code can't have arrived in the
		// meantime (single-goroutine consumer), but be defensive.
		if existing, ok := s.openAnomaly[code]; ok {
			invID = existing
		} else {
			s.openAnomaly[code] = newID
			invID = newID
		}
		s.mu.Unlock()
	}

	findings, err := s.RunChecksIntoInvestigation(ctx, invID, nil, ScopeDeep, SweepAnomaly)
	if err != nil {
		s.logf("anomaly hook: run checks into investigation %d: %v", invID, err)
		return
	}

	if s.d.Publisher != nil {
		s.d.Publisher.Publish(EventFindingsNew, map[string]any{
			"sweep_kind":       SweepAnomaly,
			"count":            len(findings),
			"worst":            string(worstSeverity(findings)),
			"swept_at":         s.d.Now().Unix(),
			"check_ids":        checkIDsOf(findings),
			"investigation_id": invID,
		})
	}
}
