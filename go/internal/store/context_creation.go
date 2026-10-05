// SPDX-License-Identifier: Apache-2.0

package store

// context_creation.go — persistence for the a0 creation ledger's hourly
// rollups (WS-N1, migration 0090). Sizes and names only; see the migration's
// header. Deliberately a concrete accessor on *DB rather than an entry in the
// Store interface so it adds no surface to store.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Hist is a sparse histogram: bucket index -> count.
type Hist map[int]int64

// Merge adds o into h (h must be non-nil).
func (h Hist) Merge(o Hist) {
	for k, v := range o {
		h[k] += v
	}
}

// Encode renders "bucket:count,..." in ascending bucket order ("" when empty).
func (h Hist) Encode() string {
	if len(h) == 0 {
		return ""
	}
	keys := make([]int, 0, len(h))
	for k, v := range h {
		if v != 0 {
			keys = append(keys, k)
		}
	}
	sort.Ints(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(k))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(h[k], 10))
	}
	return b.String()
}

// DecodeHist parses Encode's output; malformed pairs are skipped.
func DecodeHist(s string) Hist {
	h := Hist{}
	if s == "" {
		return h
	}
	for _, p := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(p, ":")
		if !ok {
			continue
		}
		ki, err1 := strconv.Atoi(k)
		vi, err2 := strconv.ParseInt(v, 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		h[ki] += vi
	}
	return h
}

// ContextCreationRow is one (hour, consumer, model, tool) rollup row. Tool ""
// is the request-level row. See migration 0090 for column semantics.
type ContextCreationRow struct {
	Hour     int64
	Consumer string
	Model    string
	Tool     string

	Requests                int64
	NewMsgs                 int64
	UserChars               int64
	AssistantTextChars      int64
	AssistantReasoningChars int64
	ToolArgsChars           int64
	OtherChars              int64
	ToolResults             int64
	ToolOutputChars         int64
	Big8k                   int64
	Big24k                  int64
	MaxResult               int64
	ResultHist              Hist
	SchemaRequests          int64
	SchemaCharsSum          int64
	SchemaHist              Hist
	ReuseN                  int64
	ReuseHist               Hist
	CacheBustSuspects       int64
}

// Merge folds o (same key) into r: sums, max for MaxResult, merged histograms.
func (r *ContextCreationRow) Merge(o ContextCreationRow) {
	r.Requests += o.Requests
	r.NewMsgs += o.NewMsgs
	r.UserChars += o.UserChars
	r.AssistantTextChars += o.AssistantTextChars
	r.AssistantReasoningChars += o.AssistantReasoningChars
	r.ToolArgsChars += o.ToolArgsChars
	r.OtherChars += o.OtherChars
	r.ToolResults += o.ToolResults
	r.ToolOutputChars += o.ToolOutputChars
	r.Big8k += o.Big8k
	r.Big24k += o.Big24k
	if o.MaxResult > r.MaxResult {
		r.MaxResult = o.MaxResult
	}
	r.SchemaRequests += o.SchemaRequests
	r.SchemaCharsSum += o.SchemaCharsSum
	r.ReuseN += o.ReuseN
	r.CacheBustSuspects += o.CacheBustSuspects
	r.ResultHist = mergeHist(r.ResultHist, o.ResultHist)
	r.SchemaHist = mergeHist(r.SchemaHist, o.SchemaHist)
	r.ReuseHist = mergeHist(r.ReuseHist, o.ReuseHist)
}

func mergeHist(a, b Hist) Hist {
	if a == nil {
		a = Hist{}
	}
	a.Merge(b)
	return a
}

// ContextCreationStore persists context_creation_hourly.
type ContextCreationStore struct{ d *DB }

// ContextCreation returns the creation-ledger rollup store.
func (d *DB) ContextCreation() *ContextCreationStore { return &ContextCreationStore{d} }

const ccCols = `requests, new_msgs, user_chars, assistant_text_chars, assistant_reasoning_chars,
	tool_args_chars, other_chars, tool_results, tool_output_chars, big_8k, big_24k, max_result,
	result_hist, schema_requests, schema_chars_sum, schema_hist, reuse_n, reuse_hist, cache_bust_suspects`

type ccScanner interface{ Scan(...any) error }

func scanCCVals(sc ccScanner, r *ContextCreationRow, lead ...any) error {
	var rh, sh, uh string
	dest := append(lead,
		&r.Requests, &r.NewMsgs, &r.UserChars, &r.AssistantTextChars, &r.AssistantReasoningChars,
		&r.ToolArgsChars, &r.OtherChars, &r.ToolResults, &r.ToolOutputChars, &r.Big8k, &r.Big24k, &r.MaxResult,
		&rh, &r.SchemaRequests, &r.SchemaCharsSum, &sh, &r.ReuseN, &uh, &r.CacheBustSuspects)
	err := sc.Scan(dest...)
	r.ResultHist, r.SchemaHist, r.ReuseHist = DecodeHist(rh), DecodeHist(sh), DecodeHist(uh)
	return err
}

// Upsert merges each row into its (hour, consumer, model, tool) row in one
// transaction (read-merge-write, so histograms and max compose correctly).
func (s *ContextCreationStore) Upsert(ctx context.Context, rows []ContextCreationRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: context_creation.upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	for _, in := range rows {
		cur := ContextCreationRow{}
		err := scanCCVals(tx.QueryRowContext(ctx,
			`SELECT `+ccCols+` FROM context_creation_hourly WHERE hour=? AND consumer=? AND model=? AND tool=?`,
			in.Hour, in.Consumer, in.Model, in.Tool), &cur)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: context_creation.upsert read: %w", err)
		}
		cur.Merge(in)
		_, err = tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO context_creation_hourly (hour, consumer, model, tool, `+ccCols+`)
			 VALUES (?,?,?,?, ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			in.Hour, in.Consumer, in.Model, in.Tool,
			cur.Requests, cur.NewMsgs, cur.UserChars, cur.AssistantTextChars, cur.AssistantReasoningChars,
			cur.ToolArgsChars, cur.OtherChars, cur.ToolResults, cur.ToolOutputChars, cur.Big8k, cur.Big24k, cur.MaxResult,
			cur.ResultHist.Encode(), cur.SchemaRequests, cur.SchemaCharsSum, cur.SchemaHist.Encode(),
			cur.ReuseN, cur.ReuseHist.Encode(), cur.CacheBustSuspects)
		if err != nil {
			return fmt.Errorf("store: context_creation.upsert write: %w", err)
		}
	}
	return tx.Commit()
}

// Since returns every row with hour >= sinceHour.
func (s *ContextCreationStore) Since(ctx context.Context, sinceHour int64) ([]ContextCreationRow, error) {
	rows, err := s.d.sql.QueryContext(ctx,
		`SELECT hour, consumer, model, tool, `+ccCols+` FROM context_creation_hourly WHERE hour >= ? ORDER BY hour`, sinceHour)
	if err != nil {
		return nil, fmt.Errorf("store: context_creation.since: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ContextCreationRow
	for rows.Next() {
		var r ContextCreationRow
		if err := scanCCVals(rows, &r, &r.Hour, &r.Consumer, &r.Model, &r.Tool); err != nil {
			return nil, fmt.Errorf("store: context_creation.since: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Prune deletes rows older than beforeHour (retention job).
func (s *ContextCreationStore) Prune(ctx context.Context, beforeHour int64) (int64, error) {
	res, err := s.d.sql.ExecContext(ctx, `DELETE FROM context_creation_hourly WHERE hour < ?`, beforeHour)
	if err != nil {
		return 0, fmt.Errorf("store: context_creation.prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
