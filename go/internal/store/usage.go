// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type usageView struct{ d *DB }

// Usage returns the usage/cost event stream + mode-history surface.
// Aggregates are computed at read time over the window (Contract 3).
func (d *DB) Usage() Usage { return usageView{d} }

func (v usageView) Record(ctx context.Context, e UsageEvent) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO usage_events (ts, kind, model, slot, provider_id,
		   prompt_tokens, completion_tokens, cost_usd, detail,
		   cost_native, cost_currency, cached_prompt_tokens, unmetered)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		unixOf(orNow(e.TS)), e.Kind, nullStr(e.Model), nullStr(e.Slot),
		intPtrArg(e.ProviderID), e.PromptTokens, e.CompletionTokens, e.CostUSD,
		nullStr(e.Detail),
		floatPtrArg(e.CostNative), nullStr(e.CostCurrency), intPtrArg(e.CachedPromptTokens),
		boolInt(e.Unmetered),
	)
	if err != nil {
		return fmt.Errorf("store: usage.record: %w", err)
	}
	return nil
}

// Events returns events with ts >= since, newest first. limit <= 0 means no
// limit.
func (v usageView) Events(ctx context.Context, since time.Time, limit int) ([]UsageEvent, error) {
	if limit <= 0 {
		limit = -1 // SQLite: LIMIT -1 = unlimited
	}
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT ue.ts, ue.kind, ue.model, ue.slot, ue.provider_id, rp.name,
		        ue.prompt_tokens, ue.completion_tokens,
		        ue.cost_usd, ue.detail, ue.cost_native, ue.cost_currency,
		        ue.cached_prompt_tokens, ue.unmetered
		 FROM usage_events ue
		 LEFT JOIN router_providers rp ON rp.id = ue.provider_id
		 WHERE ue.ts >= ? ORDER BY ue.ts DESC, ue.id DESC LIMIT ?`,
		unixOf(since), limit)
	if err != nil {
		return nil, fmt.Errorf("store: usage.events: %w", err)
	}
	defer rows.Close()
	var out []UsageEvent
	for rows.Next() {
		var e UsageEvent
		var ts int64
		var model, slot, providerName, detail, costCurrency sql.NullString
		var providerID, prompt, completion, cachedPromptTokens sql.NullInt64
		var cost, costNative sql.NullFloat64
		var unmetered int64
		if err := rows.Scan(&ts, &e.Kind, &model, &slot, &providerID, &providerName,
			&prompt, &completion, &cost, &detail,
			&costNative, &costCurrency, &cachedPromptTokens, &unmetered); err != nil {
			return nil, fmt.Errorf("store: usage.events: %w", err)
		}
		e.TS = timeOf(sql.NullInt64{Int64: ts, Valid: true})
		e.Model = strOf(model)
		e.Slot = strOf(slot)
		e.ProviderID = nullInt64Ptr(providerID)
		e.ProviderName = strOf(providerName)
		e.PromptTokens = intOf(prompt)
		e.CompletionTokens = intOf(completion)
		e.CostUSD = floatOf(cost)
		e.Detail = strOf(detail)
		e.CostNative = nullFloat64Ptr(costNative)
		e.CostCurrency = strOf(costCurrency)
		e.CachedPromptTokens = nullInt64Ptr(cachedPromptTokens)
		e.Unmetered = unmetered != 0
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage.events: %w", err)
	}
	return out, nil
}

// TokenActivity returns per-event token totals (prompt+completion) for
// token-bearing kinds only ("inference" local slot samples, "external_request"
// remote provider requests), oldest first, ts >= since. Index-covered by
// idx_usage_events_ts. Narrower than Events — the activity heatmap (Sprint L)
// only needs a timestamp to bucket by and a token count to sum, not the full
// row.
func (v usageView) TokenActivity(ctx context.Context, since time.Time) ([]TokenActivityRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT ts, kind, COALESCE(prompt_tokens, 0) + COALESCE(completion_tokens, 0)
		 FROM usage_events
		 WHERE ts >= ? AND kind IN ('inference', 'external_request')
		 ORDER BY ts ASC`,
		unixOf(since))
	if err != nil {
		return nil, fmt.Errorf("store: usage.token_activity: %w", err)
	}
	defer rows.Close()
	var out []TokenActivityRow
	for rows.Next() {
		var ts int64
		var kind string
		var tokens int64
		if err := rows.Scan(&ts, &kind, &tokens); err != nil {
			return nil, fmt.Errorf("store: usage.token_activity: %w", err)
		}
		out = append(out, TokenActivityRow{TS: timeOf(sql.NullInt64{Int64: ts, Valid: true}), Kind: kind, Tokens: tokens})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage.token_activity: %w", err)
	}
	return out, nil
}

func (v usageView) RecordHistory(ctx context.Context, h ModeHistoryEntry) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO mode_history (mode, ts, trained_ctx, configured_ctx,
		   actual_ctx, load_time_s, result)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		h.Mode, unixOf(orNow(h.TS)), h.TrainedCtx, h.ConfiguredCtx, h.ActualCtx,
		h.LoadTimeS, h.Result,
	)
	if err != nil {
		return fmt.Errorf("store: usage.record_history: %w", err)
	}
	return nil
}

// History returns entries for one mode ("" = all modes), newest first.
// limit <= 0 means no limit.
func (v usageView) History(ctx context.Context, mode string, limit int) ([]ModeHistoryEntry, error) {
	if limit <= 0 {
		limit = -1
	}
	query := `SELECT mode, ts, trained_ctx, configured_ctx, actual_ctx, load_time_s, result
	          FROM mode_history`
	args := []any{}
	if mode != "" {
		query += ` WHERE mode = ?`
		args = append(args, mode)
	}
	query += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := v.d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: usage.history: %w", err)
	}
	defer rows.Close()
	var out []ModeHistoryEntry
	for rows.Next() {
		var h ModeHistoryEntry
		var ts int64
		var trained, configured, actual sql.NullInt64
		var loadTime sql.NullFloat64
		if err := rows.Scan(&h.Mode, &ts, &trained, &configured, &actual,
			&loadTime, &h.Result); err != nil {
			return nil, fmt.Errorf("store: usage.history: %w", err)
		}
		h.TS = timeOf(sql.NullInt64{Int64: ts, Valid: true})
		h.TrainedCtx = int(intOf(trained))
		h.ConfiguredCtx = int(intOf(configured))
		h.ActualCtx = int(intOf(actual))
		h.LoadTimeS = floatOf(loadTime)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage.history: %w", err)
	}
	return out, nil
}
