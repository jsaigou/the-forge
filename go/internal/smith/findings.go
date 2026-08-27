// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAlreadyRunning is returned when a sweep is requested while another is
// in flight (sweeps are serialized — one look at the box at a time).
var ErrAlreadyRunning = errors.New("smith: a sweep is already in progress")

// ErrInvalidPurgeAge is returned by PurgeFindings when max_age is neither
// "all" nor a parseable positive duration.
var ErrInvalidPurgeAge = errors.New("smith: invalid purge max_age")

// defaultFindingsLimit bounds a findings list read when the caller doesn't.
const defaultFindingsLimit = 500

// persistFindings inserts the sweep's findings into smith_findings
// (migration 0033). invID is the investigation to attach findings to (nil =
// standalone sweep findings with NULL investigation_id). Best-effort per
// row: a single bad row must not lose the rest of the sweep.
//
// Returns one ID per input finding, same order: the inserted row's ID, or 0
// for any finding that failed to insert (or for every finding when Store is
// nil). proposeFrom (propose.go) uses this to link a Finding to the
// smith_actions rows it produced via finding_id — a 0 there means "propose
// with no finding_id", never an error on its own.
func (s *Smith) persistFindings(ctx context.Context, findings []Finding, sweepKind string, at time.Time, invID *int64) ([]int64, error) {
	ids := make([]int64, len(findings))
	if s.d.Store == nil {
		return ids, nil // nothing to persist; not an error
	}
	var firstErr error
	for i, f := range findings {
		kbRefs := f.KBRefs
		if kbRefs == nil {
			kbRefs = []string{}
		}
		kbRefsJSON, err := json.Marshal(kbRefs)
		if err != nil {
			kbRefsJSON = []byte("[]") // never block a finding's own persistence over this
		}
		confidence := f.Confidence
		if confidence == "" {
			confidence = ConfidenceHigh
		}
		res, err := s.d.Store.SQL().ExecContext(ctx,
			`INSERT INTO smith_findings
				(investigation_id, check_id, severity, summary, evidence, sweep_kind, created_at, kb_refs, confidence, confidence_note)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			invID, f.CheckID, string(f.Severity), f.Summary, evidenceJSON(f.Evidence), sweepKind, at.Unix(), string(kbRefsJSON),
			string(confidence), f.ConfidenceNote)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("insert finding %s: %w", f.CheckID, err)
			}
			continue
		}
		if id, err := res.LastInsertId(); err == nil {
			ids[i] = id
		}
	}
	return ids, firstErr
}

// ListFindings returns persisted findings, newest first. since filters on
// created_at (zero time = no filter); severity on exact match ("" = all);
// limit ≤ 0 falls back to defaultFindingsLimit.
func (s *Smith) ListFindings(ctx context.Context, since time.Time, severity string, limit int) ([]StoredFinding, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	if limit <= 0 {
		limit = defaultFindingsLimit
	}

	query := `SELECT id, investigation_id, check_id, severity, summary, evidence, sweep_kind, created_at, kb_refs, repeat_count, confidence, confidence_note
	          FROM smith_findings`
	args := []any{}
	wheres := []string{}
	if !since.IsZero() {
		wheres = append(wheres, "created_at >= ?")
		args = append(args, since.Unix())
	}
	if severity != "" {
		wheres = append(wheres, "severity = ?")
		args = append(args, severity)
	}
	if len(wheres) > 0 {
		for i, w := range wheres {
			if i == 0 {
				query += " WHERE " + w
			} else {
				query += " AND " + w
			}
		}
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.d.Store.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("smith: list findings: %w", err)
	}
	defer rows.Close()

	out := []StoredFinding{}
	for rows.Next() {
		var sf StoredFinding
		var invID sql.NullInt64
		var createdAt int64
		var kbRefsJSON string
		if err := rows.Scan(&sf.ID, &invID, &sf.CheckID, &sf.Severity, &sf.Summary,
			&sf.Evidence, &sf.SweepKind, &createdAt, &kbRefsJSON, &sf.RepeatCount, &sf.Confidence, &sf.ConfidenceNote); err != nil {
			return nil, fmt.Errorf("smith: scan finding: %w", err)
		}
		if invID.Valid {
			sf.InvestigationID = &invID.Int64
		}
		sf.CreatedAt = time.Unix(createdAt, 0).UTC()
		sf.KBRefs = unmarshalKBRefs(kbRefsJSON)
		out = append(out, sf)
	}
	return out, rows.Err()
}

// unmarshalKBRefs parses a smith_findings.kb_refs JSON column into a
// non-nil slice — malformed/empty JSON degrades to [] rather than failing
// the whole row (kb_refs is display metadata, never load-bearing for a
// finding's own correctness).
func unmarshalKBRefs(raw string) []string {
	var refs []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &refs)
	}
	if refs == nil {
		refs = []string{}
	}
	return refs
}

// PurgeFindings manually deletes standalone findings older than maxAge, or
// all standalone findings when maxAge is "all" (or ""). Investigation-
// attached findings are never purged (the evidence-trail rule retention.go's
// tier pruning follows — the manual purge is the same operation an operator
// would otherwise wait hours for the scheduled cycle to do). Returns the
// number of rows deleted.
func (s *Smith) PurgeFindings(ctx context.Context, maxAge string) (int64, error) {
	if s.d.Store == nil {
		return 0, ErrStoreUnwired
	}
	query := `DELETE FROM smith_findings WHERE investigation_id IS NULL`
	args := []any{}
	if maxAge != "" && maxAge != "all" {
		d, err := time.ParseDuration(maxAge)
		if err != nil || d <= 0 {
			return 0, ErrInvalidPurgeAge
		}
		query += ` AND created_at < ?`
		args = append(args, s.d.Now().Add(-d).Unix())
	}
	r, err := s.d.Store.SQL().ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("smith: purge findings: %w", err)
	}
	n, _ := r.RowsAffected()
	return n, nil
}

// ── history queries (§3.3) ─────────────────────────────────────────────────
//
// FindingDuration and FindingFrequency are read-only smith_findings queries
// answering the history family's "how long / how often" questions. No new
// schema (Sprint R confirmed: idx_smith_findings_check + idx_smith_findings_created
// cover both shapes at sub-0.01ms latency). created_at is a unix-seconds
// integer column; repeat_count is the dedup-collapsed repeat counter
// (migration 0045).

// FindingDuration returns how long a finding has been happening — the elapsed
// time since the oldest smith_findings row for checkID. Zero when no rows.
func (s *Smith) FindingDuration(ctx context.Context, checkID string) (time.Duration, error) {
	if s.d.Store == nil {
		return 0, ErrStoreUnwired
	}
	var oldest int64
	err := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT MIN(created_at) FROM smith_findings WHERE check_id = ?`, checkID).Scan(&oldest)
	if err != nil {
		return 0, fmt.Errorf("smith: finding duration: %w", err)
	}
	if oldest == 0 {
		return 0, nil
	}
	return s.d.Now().Sub(time.Unix(oldest, 0).UTC()), nil
}

// FindingFrequency returns how often a finding fires over a window.
// count = row count for checkID within the window; totalRepeats = SUM(repeat_count).
// A window of 0 means "all time".
func (s *Smith) FindingFrequency(ctx context.Context, checkID string, window time.Duration) (count int, totalRepeats int, err error) {
	if s.d.Store == nil {
		return 0, 0, ErrStoreUnwired
	}
	query := `SELECT COUNT(*), COALESCE(SUM(repeat_count), 0) FROM smith_findings WHERE check_id = ?`
	args := []any{checkID}
	if window > 0 {
		since := s.d.Now().Add(-window).Unix()
		query += ` AND created_at >= ?`
		args = append(args, since)
	}
	err = s.d.Store.SQL().QueryRowContext(ctx, query, args...).Scan(&count, &totalRepeats)
	if err != nil {
		return 0, 0, fmt.Errorf("smith: finding frequency: %w", err)
	}
	return count, totalRepeats, nil
}

// ── missed-pattern ledger (§3.7) ────────────────────────────────────────────
//
// When the classifier misses and the reasoning tier answers, the redacted
// user text (+ tools used) is appended to a capped candidate list so the
// catalog can grow from real usage. Storage = settings-KV (a JSON array
// under smith.missed_patterns) capped at missedPatternsCap entries — bounded,
// redacted (scrubSecretPatterns is mandatory), no migration needed. Promotion
// into the fast path is always a reviewed code change (never auto-learned),
// keeping the classifier 100% deterministic and testable.

// SettingMissedPatterns is the settings-KV key for the capped missed-pattern
// ledger (§3.7). Stored as a JSON array of MissedPattern.
const SettingMissedPatterns = "smith.missed_patterns"

// missedPatternsCap bounds the candidate list (§3.7: "capped candidate list").
const missedPatternsCap = 50

// MissedPattern is one recorded question the fast path couldn't answer.
type MissedPattern struct {
	Text      string   `json:"text"`       // redacted user question
	ToolsUsed []string `json:"tools_used"` // tool IDs the reasoning turn used
	At        int64    `json:"at"`         // unix seconds
}

// RecordMissedPattern records a redacted question that the fast path missed
// and the reasoning tier answered. Capped list; oldest entries evicted. The
// redaction pass is mandatory (scrubSecretPatterns) — the ledger must never
// persist a secret.
func (s *Smith) RecordMissedPattern(ctx context.Context, redactedText string, toolsUsed []string) error {
	if s.d.Store == nil {
		return ErrStoreUnwired
	}
	redactedText = scrubSecretPatterns(redactedText)
	if strings.TrimSpace(redactedText) == "" {
		return nil
	}
	if toolsUsed == nil {
		toolsUsed = []string{}
	}
	patterns, _ := s.missedPatternsRaw(ctx)
	patterns = append(patterns, MissedPattern{Text: redactedText, ToolsUsed: toolsUsed, At: s.d.Now().Unix()})
	// Evict oldest beyond cap.
	if len(patterns) > missedPatternsCap {
		patterns = patterns[len(patterns)-missedPatternsCap:]
	}
	return s.saveMissedPatterns(ctx, patterns)
}

// MissedPatterns returns the capped list of missed patterns for Diagnostics /
// GET /smith/status. Empty (never nil) when the key is unset or unreadable.
func (s *Smith) MissedPatterns(ctx context.Context) ([]MissedPattern, error) {
	patterns, err := s.missedPatternsRaw(ctx)
	if err != nil {
		return []MissedPattern{}, err
	}
	if patterns == nil {
		return []MissedPattern{}, nil
	}
	return patterns, nil
}

func (s *Smith) missedPatternsRaw(ctx context.Context) ([]MissedPattern, error) {
	if s.d.Settings == nil {
		return nil, nil
	}
	raw, err := s.d.Settings.Get(ctx, SettingMissedPatterns)
	if err != nil || len(raw) == 0 {
		return nil, nil
	}
	var patterns []MissedPattern
	if err := json.Unmarshal(raw, &patterns); err != nil {
		return nil, nil
	}
	return patterns, nil
}

func (s *Smith) saveMissedPatterns(ctx context.Context, patterns []MissedPattern) error {
	if s.d.Settings == nil {
		return ErrStoreUnwired
	}
	b, err := json.Marshal(patterns)
	if err != nil {
		return fmt.Errorf("smith: marshal missed patterns: %w", err)
	}
	return s.d.Settings.Set(ctx, SettingMissedPatterns, b)
}

// setPendingMissed stashes the redacted user text for a no-match turn that
// is heading to the reasoning tier, keyed by its assistant message ID.
func (s *Smith) setPendingMissed(msgID int64, redactedText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingMissed[msgID] = redactedText
}

// takePendingMissed pops and returns the stashed redacted text for msgID, or "".
func (s *Smith) takePendingMissed(msgID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := s.pendingMissed[msgID]
	delete(s.pendingMissed, msgID)
	return text
}
