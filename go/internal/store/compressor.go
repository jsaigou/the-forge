// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type compressorsView struct{ d *DB }

// Compressors returns the compressor_samples surface (Sprint 4).
func (d *DB) Compressors() Compressors { return compressorsView{d} }

func (v compressorsView) RecordSample(ctx context.Context, s CompressorSampleRow) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO compressor_samples (ts, proxy_id, up, main_pid, rss_bytes, n_restarts)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		unixOf(orNow(s.TS)), s.ProxyID, boolInt(s.Up), pidArg(s.MainPID), s.RSSBytes, s.NRestarts,
	)
	if err != nil {
		return fmt.Errorf("store: compressors.record_sample: %w", err)
	}
	return nil
}

// Latest returns each proxy's single most recent compressor_samples row,
// keyed by service name.
func (v compressorsView) Latest(ctx context.Context) (map[string]CompressorSampleRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT hp.service, cs.ts, cs.proxy_id, cs.up, cs.main_pid, cs.rss_bytes, cs.n_restarts
		 FROM compressor_samples cs
		 JOIN compressor_proxies hp ON hp.id = cs.proxy_id
		 WHERE cs.id IN (
		   SELECT MAX(id) FROM compressor_samples GROUP BY proxy_id
		 )`)
	if err != nil {
		return nil, fmt.Errorf("store: compressors.latest: %w", err)
	}
	defer rows.Close()
	out := map[string]CompressorSampleRow{}
	for rows.Next() {
		s, err := scanCompressorSample(rows)
		if err != nil {
			return nil, fmt.Errorf("store: compressors.latest: %w", err)
		}
		out[s.Service] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: compressors.latest: %w", err)
	}
	return out, nil
}

func (v compressorsView) Range(ctx context.Context, service string, since time.Time) ([]CompressorSampleRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT hp.service, cs.ts, cs.proxy_id, cs.up, cs.main_pid, cs.rss_bytes, cs.n_restarts
		 FROM compressor_samples cs
		 JOIN compressor_proxies hp ON hp.id = cs.proxy_id
		 WHERE hp.service = ? AND cs.ts >= ?
		 ORDER BY cs.ts ASC`,
		service, unixOf(since))
	if err != nil {
		return nil, fmt.Errorf("store: compressors.range: %w", err)
	}
	defer rows.Close()
	var out []CompressorSampleRow
	for rows.Next() {
		s, err := scanCompressorSample(rows)
		if err != nil {
			return nil, fmt.Errorf("store: compressors.range: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: compressors.range: %w", err)
	}
	return out, nil
}

func (v compressorsView) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM compressor_samples WHERE ts < ?`, unixOf(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: compressors.prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: compressors.prune: %w", err)
	}
	return n, nil
}

type compressorScanner interface {
	Scan(dest ...any) error
}

func scanCompressorSample(rows compressorScanner) (CompressorSampleRow, error) {
	var s CompressorSampleRow
	var ts int64
	var up int64
	var mainPID sql.NullInt64
	if err := rows.Scan(&s.Service, &ts, &s.ProxyID, &up, &mainPID, &s.RSSBytes, &s.NRestarts); err != nil {
		return CompressorSampleRow{}, err
	}
	s.TS = time.Unix(ts, 0).UTC()
	s.Up = up != 0
	if mainPID.Valid {
		s.MainPID = uint32(mainPID.Int64)
	}
	return s, nil
}

// pidArg converts a possibly-zero PID to a nullable driver arg — 0 means
// "unit not running / PID unknown" and should read back as NULL, not a
// literal PID 0.
func pidArg(pid uint32) any {
	if pid == 0 {
		return nil
	}
	return int64(pid)
}
