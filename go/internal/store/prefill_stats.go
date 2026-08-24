// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type prefillStatsView struct{ d *DB }

// PrefillStats returns the model_prefill_stats surface (Compressor
// local-savings prefill sprint, 2026-08-06 — see
// migrations/0031_model_prefill_stats.sql).
func (d *DB) PrefillStats() PrefillStats { return prefillStatsView{d} }

// AddObservation upserts one interval's real prefill delta into the running
// (mode, fingerprint) aggregate. SQLite's ON CONFLICT DO UPDATE (unlike
// model_profiles' INSERT OR REPLACE) is a genuine in-place accumulate, not a
// delete+reinsert — there's nothing here that needs a fresh id on update
// (no child rows to cascade), so a plain upsert is the right tool.
func (v prefillStatsView) AddObservation(ctx context.Context, configID int64, fingerprint string, tokens int64, seconds float64) error {
	if tokens <= 0 || seconds <= 0 {
		return fmt.Errorf("store: prefill_stats.add_observation: tokens and seconds must both be > 0 (got %d, %v)", tokens, seconds)
	}
	// Nanosecond resolution (not Unix()'s whole seconds): ByMode selects the
	// current regime by "greatest last_seen per mode", and a live collector
	// can plausibly deliver two different fingerprints' first observations
	// within the same wall-clock second (e.g. right after a config change,
	// or in a fast test) — whole-second resolution would make that
	// selection genuinely ambiguous rather than just cosmetically so.
	now := time.Now().UTC().UnixNano()
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO model_prefill_stats (config_id, fingerprint, prompt_tokens, prompt_seconds, samples, first_seen, last_seen)
		 VALUES (?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT(config_id, fingerprint) DO UPDATE SET
		   prompt_tokens  = prompt_tokens + excluded.prompt_tokens,
		   prompt_seconds = prompt_seconds + excluded.prompt_seconds,
		   samples        = samples + 1,
		   last_seen      = excluded.last_seen`,
		configID, fingerprint, tokens, seconds, now, now,
	)
	if err != nil {
		return fmt.Errorf("store: prefill_stats.add_observation: %w", err)
	}
	return nil
}

// ByMode returns each mode's current-regime aggregate — see the PrefillStats
// interface doc for why "greatest last_seen per mode" is the right
// selection without re-deriving a fingerprint here. Uses ROW_NUMBER() rather
// than a WHERE last_seen = MAX(...) correlated filter so an exact tie (two
// fingerprints' first observation landing in the same nanosecond — possible
// in principle, not just in a fast test) deterministically picks exactly
// one row instead of returning both and leaving the map-insertion order to
// decide silently which one wins.
func (v prefillStatsView) ByMode(ctx context.Context) (map[string]PrefillStat, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT config_id, mode, fingerprint, prompt_tokens, prompt_seconds, samples, first_seen, last_seen
		 FROM (
		   SELECT mps.*, c.name AS mode,
		          ROW_NUMBER() OVER (PARTITION BY mps.config_id ORDER BY mps.last_seen DESC) AS rn
		   FROM model_prefill_stats mps
		   JOIN configs c ON c.id = mps.config_id
		 )
		 WHERE rn = 1`)
	if err != nil {
		return nil, fmt.Errorf("store: prefill_stats.by_mode: %w", err)
	}
	defer rows.Close()
	out := map[string]PrefillStat{}
	for rows.Next() {
		var p PrefillStat
		var first, last int64
		if err := rows.Scan(&p.ConfigID, &p.Mode, &p.Fingerprint, &p.PromptTokens, &p.PromptSeconds, &p.Samples, &first, &last); err != nil {
			if err == sql.ErrNoRows {
				break
			}
			return nil, fmt.Errorf("store: prefill_stats.by_mode: scan: %w", err)
		}
		p.FirstSeen = time.Unix(0, first).UTC()
		p.LastSeen = time.Unix(0, last).UTC()
		out[p.Mode] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: prefill_stats.by_mode: %w", err)
	}
	return out, nil
}
