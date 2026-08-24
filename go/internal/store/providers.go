// SPDX-License-Identifier: Apache-2.0

package store

// providers.go — Sprint 0 §0.3 read side of the provider taxonomy +
// health/credits cache. NEW file owned by track BE-3 (the existing
// store.Routing surface stays the owner of the legacy router_providers
// 6-column read/write used by the Compressor page; this surface adds the
// extended columns + the provider_state cache table that the
// GET /api/v1/providers handler consumes).
//
// Phase 7 (2026-08-13): the provider_models catalog this file used to also
// own was dropped (migration 0043) — it had zero rows on the live
// deployment and no production write path had ever existed for it (only a
// test seeded it directly). internal/providers.Service now builds the
// provided-models list from the real, populated `offerings` table instead
// (internal/router.GroupOfferingsByModel); see providers.go's Deps.Offerings.
//
// Why a separate sub-interface rather than extending Compressor: BE-5 owns
// compressor_handlers.go + the provider-key CRUD write path (§0.9) and will
// extend store.Routing for those mutations; this surface is the read-only
// catalog+cache that BE-3's GET handler + the per-provider HTTP clients
// consume. Keeping them disjoint lets the two tracks land independently
// without one editing the other's store file.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ProviderStateRow is the cached (health, credits) pair for one provider
// (Sprint 0 §0.3, table provider_state). The JSON blobs mirror the
// providerHealthJSON / providerCreditsJSON wire shapes 1:1 — the daemon
// fetches, the read path serves. fetched_at is the cache age; TTL is set
// by the caller (BE-3's providers.Service, frozen at ≥60s per §0.3).
type ProviderStateRow struct {
	ProviderID  int64
	HealthJSON  string // "" = never cached
	CreditsJSON string // "" = never cached
	FetchedAt   time.Time
}

// Providers is the read surface for the provider taxonomy + health/credits
// cache. Additive to the existing Store interface (Contract 2 amendment by
// BE-3); implementations live below on *DB. The Compressor interface stays
// the owner of router_providers write/CRUD for the Compressor page + §0.9
// provider-key management — this surface reads the extended columns +
// provider_state, and writes only the cache row.
type Providers interface {
	// List returns every provider row including the §0.2 columns
	// (bill_currency, status_url, credits_url). Ordered by name.
	List(ctx context.Context) ([]ProviderRow, error)
	// State returns the cached health+credits for one provider, or
	// ErrNotFound when no row exists (never an error for a fresh cache).
	State(ctx context.Context, providerID int64) (*ProviderStateRow, error)
	// SaveState upserts the cache row. Callers must populate both JSON
	// blobs (use the zero-value providerHealthJSON/providerCreditsJSON
	// shape, not "", when a fetch returned nothing — keeps the cache
	// forward-compatible with schema additions).
	SaveState(ctx context.Context, row ProviderStateRow) error
}

type providersView struct{ d *DB }

// Providers returns the provider taxonomy + cache surface. nil DB is a
// programming error (callers go through *DB).
func (d *DB) Providers() Providers { return providersView{d} }

// List reads router_providers including the §0.2 columns. The legacy
// Compressor.Providers() SELECT stays as-is (it reads only the 6 original
// columns for the Compressor page); this SELECT reads all 9 so the providers
// handler can assemble bill_currency + the URLs the health/credits clients
// need without a second round-trip.
func (v providersView) List(ctx context.Context) ([]ProviderRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name, api_key, target_url, model, model2,
		        bill_currency, status_url, credits_url, org_id,
		        billing_enabled, billing_console_url, enabled, country,
		        data_residency_group, created_at
		 FROM router_providers WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: providers.list: %w", err)
	}
	defer rows.Close()
	// Pre-allocated empty slice (not nil) so the handler marshals []
	// instead of null — the PWA's .map() would crash on null. Matches the
	// Contract 1 §3 "arrays must not be nil" convention (see TestStatusShape).
	out := []ProviderRow{}
	for rows.Next() {
		var p ProviderRow
		var created int64
		var billingEnabled, enabled int64
		if err := rows.Scan(&p.ID, &p.Name, &p.APIKey, &p.TargetURL,
			&p.Model, &p.Model2, &p.BillCurrency, &p.StatusURL, &p.CreditsURL,
			&p.OrgID, &billingEnabled, &p.BillingConsoleURL, &enabled,
			&p.Country, &p.DataResidencyGroup, &created); err != nil {
			return nil, fmt.Errorf("store: providers.list: %w", err)
		}
		p.BillingEnabled = billingEnabled != 0
		p.Enabled = enabled != 0
		p.CreatedAt = timeOf(sql.NullInt64{Int64: created, Valid: true})
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: providers.list: %w", err)
	}
	// Second pass: fill CompressorProxyName via a join keyed on provider_id
	// (0042 dropped router_providers.headroom_proxy — the link lives on
	// the proxy side now). A second query rather than a LEFT JOIN above
	// keeps the primary scan untouched and this is a small, infrequent list.
	if len(out) > 0 {
		linkRows, err := v.d.sql.QueryContext(ctx,
			`SELECT provider_id, service FROM compressor_proxies
			 WHERE provider_id IS NOT NULL AND orphaned_at IS NULL`)
		if err != nil {
			return nil, fmt.Errorf("store: providers.list: proxy links: %w", err)
		}
		defer linkRows.Close()
		byProvider := map[int64]string{}
		for linkRows.Next() {
			var pid int64
			var service string
			if err := linkRows.Scan(&pid, &service); err != nil {
				return nil, fmt.Errorf("store: providers.list: proxy links: %w", err)
			}
			byProvider[pid] = service
		}
		for i := range out {
			out[i].CompressorProxyName = byProvider[out[i].ID]
		}
	}
	return out, nil
}

// State reads one provider_state cache row. ErrNotFound (not nil) when no
// row exists — callers treat that as "never cached" and fall through to a
// fresh fetch.
func (v providersView) State(ctx context.Context, providerID int64) (*ProviderStateRow, error) {
	var (
		healthJSON, creditsJSON string
		fetched                 int64
	)
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT health_json, credits_json, fetched_at
		 FROM provider_state WHERE provider_id = ?`, providerID).
		Scan(&healthJSON, &creditsJSON, &fetched)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: providers.state: %w", err)
	}
	return &ProviderStateRow{
		ProviderID:  providerID,
		HealthJSON:  healthJSON,
		CreditsJSON: creditsJSON,
		FetchedAt:   time.Unix(fetched, 0).UTC(),
	}, nil
}

// SaveState upserts the cache row. fetched_at is the cache age used by the
// caller's TTL check; both JSON blobs are overwritten atomically so a
// concurrent reader never sees a half-updated cache.
func (v providersView) SaveState(ctx context.Context, row ProviderStateRow) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO provider_state (provider_id, health_json, credits_json, fetched_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(provider_id) DO UPDATE SET
		   health_json = excluded.health_json,
		   credits_json = excluded.credits_json,
		   fetched_at = excluded.fetched_at`,
		row.ProviderID, row.HealthJSON, row.CreditsJSON, unixOf(orNow(row.FetchedAt)))
	if err != nil {
		return fmt.Errorf("store: providers.save_state: %w", err)
	}
	return nil
}
