// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type modelProfilesView struct{ d *DB }

// ModelProfiles returns the model_profiles surface (PROFILE track,
// docs/v5-profiling-benchmarks.md §4).
func (d *DB) ModelProfiles() ModelProfiles { return modelProfilesView{d} }

// Save records a profile plus its depth-sweep benchmarks, replacing any
// existing row for the same (config_id, backend, parallel, n_ctx) combo. The
// UNIQUE constraint on those four columns makes INSERT OR REPLACE act as an
// upsert — note that SQLite's REPLACE semantics DELETE the conflicting row
// and INSERT a new one (a new id, not an in-place update), which is exactly
// why old benchmark rows (FK'd to the old id, ON DELETE CASCADE) are
// dropped automatically rather than orphaned: a fresh profile run replaces
// the whole profile, benchmarks included, never just some of it.
func (v modelProfilesView) Save(ctx context.Context, p ModelProfile, benchmarks []ModelProfileBenchmark) error {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT OR REPLACE INTO model_profiles
		   (config_id, model_id, n_ctx, backend, parallel, safe_memory_bytes,
		    prefill_tps, decode_tps, actual_n_ctx, fingerprint, measured_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ConfigID, p.ModelID, p.NCtx, p.Backend, p.Parallel, p.SafeMemoryBytes,
		p.PrefillTPS, p.DecodeTPS, p.ActualNCtx, p.Fingerprint, unixOf(p.MeasuredAt),
	)
	if err != nil {
		return fmt.Errorf("store: model_profiles.save: %w", err)
	}
	if len(benchmarks) == 0 {
		return nil
	}
	profileID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: model_profiles.save: profile id: %w", err)
	}
	for _, b := range benchmarks {
		if _, err := v.d.sql.ExecContext(ctx,
			`INSERT INTO model_profile_benchmarks (profile_id, depth_tokens, pp2048_tps, tg128_tps)
			 VALUES (?, ?, ?, ?)`,
			profileID, b.DepthTokens, b.PP2048TPS, b.TG128TPS,
		); err != nil {
			return fmt.Errorf("store: model_profiles.save: benchmark: %w", err)
		}
	}
	return nil
}

// Benchmarks returns the depth-sweep rows for a profile, ordered by
// depth_tokens ascending.
func (v modelProfilesView) Benchmarks(ctx context.Context, profileID int64) ([]ModelProfileBenchmark, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, profile_id, depth_tokens, pp2048_tps, tg128_tps
		 FROM model_profile_benchmarks WHERE profile_id = ? ORDER BY depth_tokens ASC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("store: model_profiles.benchmarks: %w", err)
	}
	defer rows.Close()
	out := []ModelProfileBenchmark{}
	for rows.Next() {
		var b ModelProfileBenchmark
		if err := rows.Scan(&b.ID, &b.ProfileID, &b.DepthTokens, &b.PP2048TPS, &b.TG128TPS); err != nil {
			return nil, fmt.Errorf("store: model_profiles.benchmarks: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

const profileSelectCols = `mp.id, mp.config_id, c.name, mp.model_id, mp.n_ctx, mp.backend,
		        mp.parallel, mp.safe_memory_bytes, mp.prefill_tps, mp.decode_tps,
		        mp.actual_n_ctx, mp.fingerprint, mp.measured_at
		 FROM model_profiles mp JOIN configs c ON c.id = mp.config_id`

// Get returns the latest profile for a config (ErrNotFound when none).
func (v modelProfilesView) Get(ctx context.Context, configID int64) (ModelProfile, error) {
	row := v.d.sql.QueryRowContext(ctx,
		`SELECT `+profileSelectCols+`
		 WHERE mp.config_id = ?
		 ORDER BY mp.measured_at DESC LIMIT 1`, configID)
	p, err := scanProfile(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return ModelProfile{}, fmt.Errorf("%w: model_profiles config_id %d", ErrNotFound, configID)
		}
		return ModelProfile{}, err
	}
	return p, nil
}

// List returns all profiles, ordered by config name then measured_at desc.
func (v modelProfilesView) List(ctx context.Context) ([]ModelProfile, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT `+profileSelectCols+`
		 ORDER BY c.name ASC, mp.measured_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: model_profiles.list: %w", err)
	}
	defer rows.Close()
	out := []ModelProfile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: model_profiles.list: %w", err)
	}
	return out, nil
}

// Delete removes a profile by config (cascades to its benchmark rows).
func (v modelProfilesView) Delete(ctx context.Context, configID int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM model_profiles WHERE config_id = ?`, configID)
	if err != nil {
		return fmt.Errorf("store: model_profiles.delete: %w", err)
	}
	return nil
}

// scanner abstracts *sql.Row and *sql.Rows for shared scan logic.
type scanner interface {
	Scan(dest ...any) error
}

func scanProfile(s scanner) (ModelProfile, error) {
	var p ModelProfile
	var ts int64
	if err := s.Scan(&p.ID, &p.ConfigID, &p.Mode, &p.ModelID, &p.NCtx, &p.Backend, &p.Parallel,
		&p.SafeMemoryBytes, &p.PrefillTPS, &p.DecodeTPS, &p.ActualNCtx,
		&p.Fingerprint, &ts); err != nil {
		if err == sql.ErrNoRows {
			return ModelProfile{}, err
		}
		return ModelProfile{}, fmt.Errorf("store: model_profiles.scan: %w", err)
	}
	p.MeasuredAt = time.Unix(ts, 0).UTC()
	return p, nil
}
