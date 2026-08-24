// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type routingView struct{ d *DB }

// Compressor returns the proxy/provider/savings surface. ProxyRow.Token and
// ProviderRow.APIKey are secrets — they never appear in error messages or
// logs, and API responses must mask them (Contract 1 §3).
func (d *DB) Routing() Routing { return routingView{d} }

func (v routingView) SaveProxy(ctx context.Context, p ProxyRow) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO compressor_proxies (service, label, port, target_url, unit,
		   provider_id, token, passthrough, orphaned_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(service) DO UPDATE SET
		   label = excluded.label, port = excluded.port,
		   target_url = excluded.target_url, unit = excluded.unit,
		   provider_id = excluded.provider_id, token = excluded.token,
		   passthrough = excluded.passthrough, orphaned_at = excluded.orphaned_at`,
		p.Service, p.Label, p.Port, p.TargetURL, p.Unit, intPtrArg(p.ProviderID),
		nullStr(p.Token), boolInt(p.Passthrough), nullUnix(p.OrphanedAt),
		unixOf(orNow(p.CreatedAt)),
	)
	if err != nil {
		return fmt.Errorf("store: routing.save_proxy: %w", err)
	}
	return nil
}

func (v routingView) DeleteProxy(ctx context.Context, service string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM compressor_proxies WHERE service = ?`, service)
	if err != nil {
		return fmt.Errorf("store: routing.delete_proxy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v routingView) Proxies(ctx context.Context) ([]ProxyRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT hp.id, hp.service, hp.label, hp.port, hp.target_url, hp.unit,
		        hp.provider_id, rp.name, hp.token,
		        hp.passthrough, hp.orphaned_at, hp.created_at
		 FROM compressor_proxies hp
		 LEFT JOIN router_providers rp ON rp.id = hp.provider_id
		 ORDER BY hp.service`)
	if err != nil {
		return nil, fmt.Errorf("store: routing.proxies: %w", err)
	}
	defer rows.Close()
	var out []ProxyRow
	for rows.Next() {
		var p ProxyRow
		var providerID sql.NullInt64
		var providerName, token sql.NullString
		var passthrough, created int64
		var orphaned sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Service, &p.Label, &p.Port, &p.TargetURL, &p.Unit,
			&providerID, &providerName, &token, &passthrough, &orphaned, &created); err != nil {
			return nil, fmt.Errorf("store: routing.proxies: %w", err)
		}
		if providerID.Valid {
			id := providerID.Int64
			p.ProviderID = &id
		}
		p.ProviderName = strOf(providerName)
		p.Token = strOf(token)
		p.Passthrough = passthrough != 0
		p.OrphanedAt = timeOf(orphaned)
		p.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: routing.proxies: %w", err)
	}
	return out, nil
}

// LinkProxyToProvider sets (providerID != nil) or clears (providerID == nil)
// the one FK that replaced the old bidirectional string pair (0042). The
// partial unique index on (provider_id WHERE provider_id IS NOT NULL AND
// orphaned_at IS NULL) rejects linking a provider that's already claimed by
// another active proxy.
func (v routingView) LinkProxyToProvider(ctx context.Context, proxyID int64, providerID *int64) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE compressor_proxies SET provider_id = ? WHERE id = ?`,
		intPtrArg(providerID), proxyID)
	if err != nil {
		return fmt.Errorf("store: routing.link_proxy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v routingView) SaveProvider(ctx context.Context, p ProviderRow) error {
	// bill_currency's column-level DEFAULT 'USD' (0002_polish.sql) only
	// applies when the column is omitted from the INSERT list — since this
	// statement binds it explicitly, an unset (zero-value "") BillCurrency
	// must be normalized here, or a fresh row would persist "" instead of
	// falling back to USD (the ON CONFLICT branch already COALESCEs this
	// for updates; INSERT needs the same normalization for new rows).
	billCcy := p.BillCurrency
	if billCcy == "" {
		billCcy = "USD"
	}
	if p.ID == 0 {
		// No id given — fall back to upsert-by-name (the pre-0042 ON
		// CONFLICT(name) behavior): a caller that already knows the row
		// exists but only has its name in hand (every direct store caller
		// before this migration, and the many call sites left that way
		// deliberately rather than force-threading an id everywhere) still
		// updates in place instead of creating a duplicate row. A caller
		// that means to RENAME must resolve the row's real id first
		// (resolveProviderRef+SaveProvider is the production pattern,
		// httpapi/settings_handlers.go) — passing id=0 can only ever match
		// on the CURRENT name, so it is never a way to rename.
		if existing, ok, err := v.providerBy(ctx, "rp.name = ? AND rp.deleted_at IS NULL", p.Name); err != nil {
			return fmt.Errorf("store: routing.save_provider: %w", err)
		} else if ok {
			p.ID = existing.ID
		}
	}
	if p.ID == 0 {
		_, err := v.d.sql.ExecContext(ctx,
			`INSERT INTO router_providers (name, api_key, target_url,
			   model, model2, bill_currency, status_url, credits_url, org_id,
			   billing_enabled, billing_console_url, enabled, country,
			   data_residency_group, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.Name, p.APIKey, p.TargetURL, p.Model, p.Model2,
			billCcy, p.StatusURL, p.CreditsURL, p.OrgID,
			boolInt(p.BillingEnabled), p.BillingConsoleURL, boolInt(p.Enabled),
			p.Country, p.DataResidencyGroup,
			unixOf(orNow(p.CreatedAt)),
		)
		if err != nil {
			return fmt.Errorf("store: routing.save_provider: %w", err)
		}
		return nil
	}
	// Update by id — this is how a rename happens now (name is no longer
	// the primary key, 0042). The partial unique index on
	// (name WHERE deleted_at IS NULL) still rejects colliding with another
	// live provider's name.
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE router_providers SET
		   name = ?, api_key = ?, target_url = ?, model = ?, model2 = ?,
		   bill_currency = COALESCE(NULLIF(?, ''), 'USD'),
		   status_url = ?, credits_url = ?, org_id = ?,
		   billing_enabled = ?, billing_console_url = ?,
		   enabled = ?, country = ?, data_residency_group = ?
		 WHERE id = ?`,
		p.Name, p.APIKey, p.TargetURL, p.Model, p.Model2,
		billCcy, p.StatusURL, p.CreditsURL, p.OrgID,
		boolInt(p.BillingEnabled), p.BillingConsoleURL, boolInt(p.Enabled),
		p.Country, p.DataResidencyGroup, p.ID,
	)
	if err != nil {
		return fmt.Errorf("store: routing.save_provider: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProvider soft-deletes: sets deleted_at rather than removing the row
// (0042 — see the Compressor interface doc comment for why). A no-op error
// (ErrNotFound) if the row is missing or already soft-deleted.
func (v routingView) DeleteProvider(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE router_providers SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		unixOf(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: routing.delete_provider: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (v routingView) Providers(ctx context.Context) ([]ProviderRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT rp.id, rp.name, rp.api_key, rp.target_url, rp.model, rp.model2,
		        rp.bill_currency, rp.status_url, rp.credits_url, rp.org_id,
		        rp.billing_enabled, rp.billing_console_url, rp.enabled, rp.country,
		        rp.data_residency_group, rp.created_at, hp.service
		 FROM router_providers rp
		 LEFT JOIN compressor_proxies hp
		   ON hp.provider_id = rp.id AND hp.orphaned_at IS NULL
		 WHERE rp.deleted_at IS NULL
		 ORDER BY rp.name`)
	if err != nil {
		return nil, fmt.Errorf("store: routing.providers: %w", err)
	}
	defer rows.Close()
	var out []ProviderRow
	for rows.Next() {
		var p ProviderRow
		var created int64
		var billingEnabled, enabled int64
		var proxyService sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.APIKey, &p.TargetURL,
			&p.Model, &p.Model2, &p.BillCurrency, &p.StatusURL, &p.CreditsURL,
			&p.OrgID, &billingEnabled, &p.BillingConsoleURL, &enabled,
			&p.Country, &p.DataResidencyGroup, &created, &proxyService); err != nil {
			return nil, fmt.Errorf("store: routing.providers: %w", err)
		}
		p.BillingEnabled = billingEnabled != 0
		p.Enabled = enabled != 0
		p.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
		p.CompressorProxyName = strOf(proxyService)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: routing.providers: %w", err)
	}
	return out, nil
}

func (v routingView) ProviderByID(ctx context.Context, id int64) (ProviderRow, bool, error) {
	return v.providerBy(ctx, "rp.id = ?", id)
}

func (v routingView) ProviderByName(ctx context.Context, name string) (ProviderRow, bool, error) {
	return v.providerBy(ctx, "rp.name = ? AND rp.deleted_at IS NULL", name)
}

// providerBy is the shared single-row lookup behind ProviderByID/ByName.
// ProviderByID deliberately does NOT filter deleted_at — a caller resolving
// by id (e.g. an audit trail, or a still-linked proxy) may legitimately need
// a soft-deleted row; ProviderByName always means "the live provider with
// this name" since names are only unique among non-deleted rows.
func (v routingView) providerBy(ctx context.Context, where string, arg any) (ProviderRow, bool, error) {
	var p ProviderRow
	var created int64
	var billingEnabled, enabled int64
	var deletedAt sql.NullInt64
	var proxyService sql.NullString
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT rp.id, rp.name, rp.api_key, rp.target_url, rp.model, rp.model2,
		        rp.bill_currency, rp.status_url, rp.credits_url, rp.org_id,
		        rp.billing_enabled, rp.billing_console_url, rp.enabled, rp.country,
		        rp.data_residency_group, rp.deleted_at, rp.created_at, hp.service
		 FROM router_providers rp
		 LEFT JOIN compressor_proxies hp
		   ON hp.provider_id = rp.id AND hp.orphaned_at IS NULL
		 WHERE `+where,
		arg).Scan(&p.ID, &p.Name, &p.APIKey, &p.TargetURL, &p.Model, &p.Model2,
		&p.BillCurrency, &p.StatusURL, &p.CreditsURL, &p.OrgID,
		&billingEnabled, &p.BillingConsoleURL, &enabled, &p.Country,
		&p.DataResidencyGroup, &deletedAt, &created, &proxyService)
	if err == sql.ErrNoRows {
		return ProviderRow{}, false, nil
	}
	if err != nil {
		return ProviderRow{}, false, fmt.Errorf("store: routing.provider_by: %w", err)
	}
	p.BillingEnabled = billingEnabled != 0
	p.Enabled = enabled != 0
	p.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
	p.DeletedAt = timeOf(deletedAt)
	p.CompressorProxyName = strOf(proxyService)
	return p, true, nil
}

// RecordSavings appends one savings sample. Samples are recorded as DELTAS
// between scrapes (the collector diffs the durable /metrics counters), so
// window aggregation is a plain SUM. NOTE: docs/v5-store-schema.md open
// question 4 (per-proxy counters possibly shared across proxies) must be
// resolved before the collector writes here — the storage layer is agnostic.
func (v routingView) RecordSavings(ctx context.Context, proxyID int64, at time.Time, tokensIn, saved int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO compressor_savings_totals (ts, proxy_id, tokens_in, saved_tokens)
		 VALUES (?, ?, ?, ?)`,
		unixOf(orNow(at)), proxyID, tokensIn, saved)
	if err != nil {
		return fmt.Errorf("store: routing.record_savings: %w", err)
	}
	return nil
}

// Savings sums per-proxy sample deltas with ts >= since, keyed by proxy
// SERVICE NAME (a read-side join — see the Compressor interface doc comment).
func (v routingView) Savings(ctx context.Context, since time.Time) (map[string]SavingsTotal, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT hp.service, COALESCE(SUM(hs.tokens_in), 0), COALESCE(SUM(hs.saved_tokens), 0)
		 FROM compressor_savings_totals hs
		 JOIN compressor_proxies hp ON hp.id = hs.proxy_id
		 WHERE hs.ts >= ? GROUP BY hp.service`,
		unixOf(since))
	if err != nil {
		return nil, fmt.Errorf("store: routing.savings: %w", err)
	}
	defer rows.Close()
	out := map[string]SavingsTotal{}
	for rows.Next() {
		var proxy string
		var t SavingsTotal
		if err := rows.Scan(&proxy, &t.TokensIn, &t.Saved); err != nil {
			return nil, fmt.Errorf("store: routing.savings: %w", err)
		}
		out[proxy] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: routing.savings: %w", err)
	}
	return out, nil
}

// RecordSavingsSample appends one compressor_savings_samples row plus its labelled
// breakdown rows in a single transaction.
func (v routingView) RecordSavingsSample(ctx context.Context, s CompressorSavingsSampleRow, labels []CompressorLabelSample) error {
	tx, err := v.d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: routing.record_sample: begin: %w", err)
	}
	defer tx.Rollback()

	ts := unixOf(orNow(s.TS))
	_, err = tx.ExecContext(ctx,
		`INSERT INTO compressor_savings_samples (
		   ts, proxy_id, tokens_in, tokens_out, cache_read_tokens, uncached_tokens, compressed_saved_tokens,
		   requests, requests_cached, requests_failed, requests_rate_limited,
		   requests_timeout, requests_canceled,
		   cache_busts, cache_bust_tokens_lost,
		   ttfb_count, ttfb_sum_ms, ttfb_min_ms, ttfb_max_ms,
		   latency_count, latency_sum_ms, latency_min_ms, latency_max_ms,
		   overhead_count, overhead_sum_ms, overhead_min_ms, overhead_max_ms
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts, s.ProxyID, s.TokensIn, s.TokensOut, s.CacheReadTokens, s.UncachedTokens, s.TokensSaved,
		s.Requests, s.RequestsCached, s.RequestsFailed, s.RequestsRateLimited,
		s.RequestsTimeout, s.RequestsCanceled,
		s.CacheBusts, s.CacheBustTokensLost,
		s.TTFBCount, s.TTFBSumMs, floatPtrArg(s.TTFBMinMs), floatPtrArg(s.TTFBMaxMs),
		s.LatencyCount, s.LatencySumMs, floatPtrArg(s.LatencyMinMs), floatPtrArg(s.LatencyMaxMs),
		s.OverheadCount, s.OverheadSumMs, floatPtrArg(s.OverheadMinMs), floatPtrArg(s.OverheadMaxMs),
	)
	if err != nil {
		return fmt.Errorf("store: routing.record_sample: %w", err)
	}
	for _, l := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO compressor_label_samples (ts, proxy_id, label_key, label_value, metric, delta, delta_f)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			unixOf(orNow(l.TS)), l.ProxyID, l.LabelKey, l.LabelValue, l.Metric, l.Delta, floatPtrArg(l.DeltaF),
		); err != nil {
			return fmt.Errorf("store: routing.record_sample: label: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: routing.record_sample: commit: %w", err)
	}
	return nil
}

// SavingsSummary aggregates compressor_savings_samples/compressor_label_samples rows
// with ts >= since into per-proxy totals, keyed by proxy SERVICE NAME (a
// read-side join — see the Compressor interface doc comment). Counters are
// summed in Go over samples ordered oldest-first; min/max fields take the
// most recent non-null value encountered (the last one in ts order), never
// summed or averaged — see CompressorProxySummary's doc comment.
func (v routingView) SavingsSummary(ctx context.Context, since time.Time) (map[string]CompressorProxySummary, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT hp.service, hs.tokens_in, hs.tokens_out, hs.compressed_saved_tokens,
		        hs.requests, hs.requests_cached, hs.requests_failed, hs.requests_rate_limited,
		        hs.requests_timeout, hs.requests_canceled,
		        hs.cache_read_tokens, hs.uncached_tokens, hs.cache_busts, hs.cache_bust_tokens_lost,
		        hs.ttfb_count, hs.ttfb_sum_ms, hs.ttfb_min_ms, hs.ttfb_max_ms,
		        hs.latency_count, hs.latency_sum_ms, hs.latency_min_ms, hs.latency_max_ms,
		        hs.overhead_count, hs.overhead_sum_ms, hs.overhead_min_ms, hs.overhead_max_ms
		 FROM compressor_savings_samples hs
		 JOIN compressor_proxies hp ON hp.id = hs.proxy_id
		 WHERE hs.ts >= ? ORDER BY hs.ts ASC`,
		unixOf(since))
	if err != nil {
		return nil, fmt.Errorf("store: routing.summary: %w", err)
	}
	defer rows.Close()

	out := map[string]CompressorProxySummary{}
	for rows.Next() {
		var proxy string
		var tokensIn, tokensSaved int64
		var tokensOut sql.NullInt64
		var requests, requestsCached, requestsFailed, requestsRateLimited int64
		var requestsTimeout, requestsCanceled int64
		var cacheReadTokens, uncachedTokens, cacheBusts, cacheBustTokensLost int64
		var ttfbCount, latencyCount, overheadCount int64
		var ttfbSum, latencySum, overheadSum float64
		var ttfbMin, ttfbMax, latencyMin, latencyMax, overheadMin, overheadMax sql.NullFloat64
		if err := rows.Scan(&proxy, &tokensIn, &tokensOut, &tokensSaved,
			&requests, &requestsCached, &requestsFailed, &requestsRateLimited,
			&requestsTimeout, &requestsCanceled,
			&cacheReadTokens, &uncachedTokens, &cacheBusts, &cacheBustTokensLost,
			&ttfbCount, &ttfbSum, &ttfbMin, &ttfbMax,
			&latencyCount, &latencySum, &latencyMin, &latencyMax,
			&overheadCount, &overheadSum, &overheadMin, &overheadMax,
		); err != nil {
			return nil, fmt.Errorf("store: routing.summary: %w", err)
		}
		p := out[proxy]
		p.Proxy = proxy
		p.TokensIn += tokensIn
		p.TokensOut += tokensOut.Int64
		p.TokensSaved += tokensSaved
		p.Requests += requests
		p.RequestsCached += requestsCached
		p.RequestsFailed += requestsFailed
		p.RequestsRateLimited += requestsRateLimited
		p.RequestsTimeout += requestsTimeout
		p.RequestsCanceled += requestsCanceled
		p.CacheReadTokens += cacheReadTokens
		p.UncachedTokens += uncachedTokens
		p.CacheBusts += cacheBusts
		p.CacheBustTokensLost += cacheBustTokensLost
		p.TTFBCount += ttfbCount
		p.TTFBSumMs += ttfbSum
		p.LatencyCount += latencyCount
		p.LatencySumMs += latencySum
		p.OverheadCount += overheadCount
		p.OverheadSumMs += overheadSum
		if v := nullFloat64Ptr(ttfbMin); v != nil {
			p.TTFBMinMs = v
		}
		if v := nullFloat64Ptr(ttfbMax); v != nil {
			p.TTFBMaxMs = v
		}
		if v := nullFloat64Ptr(latencyMin); v != nil {
			p.LatencyMinMs = v
		}
		if v := nullFloat64Ptr(latencyMax); v != nil {
			p.LatencyMaxMs = v
		}
		if v := nullFloat64Ptr(overheadMin); v != nil {
			p.OverheadMinMs = v
		}
		if v := nullFloat64Ptr(overheadMax); v != nil {
			p.OverheadMaxMs = v
		}
		out[proxy] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: routing.summary: %w", err)
	}

	labelRows, err := v.d.sql.QueryContext(ctx,
		`SELECT hp.service, hls.label_key, hls.label_value, hls.metric,
		        COALESCE(SUM(hls.delta), 0), COALESCE(SUM(hls.delta_f), 0)
		 FROM compressor_label_samples hls
		 JOIN compressor_proxies hp ON hp.id = hls.proxy_id
		 WHERE hls.ts >= ?
		 GROUP BY hp.service, hls.label_key, hls.label_value, hls.metric`,
		unixOf(since))
	if err != nil {
		return nil, fmt.Errorf("store: routing.summary: labels: %w", err)
	}
	defer labelRows.Close()
	for labelRows.Next() {
		var proxy, labelKey, labelValue, metric string
		var sum int64
		var sumF float64
		if err := labelRows.Scan(&proxy, &labelKey, &labelValue, &metric, &sum, &sumF); err != nil {
			return nil, fmt.Errorf("store: routing.summary: labels: %w", err)
		}
		p, ok := out[proxy]
		if !ok {
			p = CompressorProxySummary{Proxy: proxy}
		}
		switch labelKey {
		case "provider":
			switch metric {
			case "requests":
				if p.RequestsByProvider == nil {
					p.RequestsByProvider = map[string]int64{}
				}
				p.RequestsByProvider[labelValue] = sum
			case "cache_read_tokens":
				if p.CacheReadTokensByProvider == nil {
					p.CacheReadTokensByProvider = map[string]int64{}
				}
				p.CacheReadTokensByProvider[labelValue] = sum
			case "uncached_tokens":
				if p.UncachedTokensByProvider == nil {
					p.UncachedTokensByProvider = map[string]int64{}
				}
				p.UncachedTokensByProvider[labelValue] = sum
			case "provider_cache_requests":
				if p.ProviderCacheRequests == nil {
					p.ProviderCacheRequests = map[string]int64{}
				}
				p.ProviderCacheRequests[labelValue] = sum
			case "provider_cache_hit_requests":
				if p.ProviderCacheHitRequests == nil {
					p.ProviderCacheHitRequests = map[string]int64{}
				}
				p.ProviderCacheHitRequests[labelValue] = sum
			}
		case "model":
			if p.RequestsByModel == nil {
				p.RequestsByModel = map[string]int64{}
			}
			p.RequestsByModel[labelValue] = sum
		case "transform":
			switch metric {
			case "timing_ms_sum":
				if p.TransformTimingSum == nil {
					p.TransformTimingSum = map[string]float64{}
				}
				p.TransformTimingSum[labelValue] = sumF
			case "timing_ms_count":
				if p.TransformTimingCount == nil {
					p.TransformTimingCount = map[string]int64{}
				}
				p.TransformTimingCount[labelValue] = sum
			}
		}
		out[proxy] = p
	}
	if err := labelRows.Err(); err != nil {
		return nil, fmt.Errorf("store: routing.summary: labels: %w", err)
	}
	return out, nil
}
