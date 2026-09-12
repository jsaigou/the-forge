// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// TestMigration0078PeakPricingIsNullableAdditive seeds a pre-0078 offering
// + provider + usage event, applies 0078, and confirms: the new columns
// exist and read back NULL/"" (no backfill — every existing row keeps
// meaning exactly what it meant before), and the pre-existing base price
// and event data survive untouched.
func TestMigration0078PeakPricingIsNullableAdditive(t *testing.T) {
	sqlDB := openThrough(t, 77)
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(
		`INSERT INTO router_providers (name, api_key, created_at) VALUES ('deepseek', 'sk-x', 0)`,
	); err != nil {
		t.Fatalf("seed router_providers: %v", err)
	}
	var providerID int64
	if err := sqlDB.QueryRow(`SELECT id FROM router_providers WHERE name = 'deepseek'`).Scan(&providerID); err != nil {
		t.Fatalf("resolve provider id: %v", err)
	}

	if _, err := sqlDB.Exec(
		`INSERT INTO models (family_id, name) VALUES (NULL, 'deepseek-v4-flash')`,
	); err != nil {
		t.Fatalf("seed models: %v", err)
	}
	var modelID int64
	if err := sqlDB.QueryRow(`SELECT id FROM models WHERE name = 'deepseek-v4-flash'`).Scan(&modelID); err != nil {
		t.Fatalf("resolve model id: %v", err)
	}

	if _, err := sqlDB.Exec(
		`INSERT INTO offerings (model_id, provider_id, wire_model, price_in_per_1m, price_out_per_1m, currency, enabled, priority)
		 VALUES (?, ?, 'deepseek-v4-flash', 0.14, 0.28, 'USD', 1, 100)`,
		modelID, providerID,
	); err != nil {
		t.Fatalf("seed offerings: %v", err)
	}

	if _, err := sqlDB.Exec(
		`INSERT INTO usage_events (ts, kind, model, provider_id, prompt_tokens, completion_tokens, cost_usd)
		 VALUES (0, 'external_request', 'deepseek-v4-flash', ?, 1000, 500, 0.001)`,
		providerID,
	); err != nil {
		t.Fatalf("seed usage_events: %v", err)
	}

	body, err := migrationsFS.ReadFile("migrations/0078_peak_pricing.sql")
	if err != nil {
		t.Fatalf("read 0078: %v", err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0078: %v", err)
	}

	var priceIn, priceOut float64
	var peakIn, peakOut, peakCached any
	if err := sqlDB.QueryRow(
		`SELECT price_in_per_1m, price_out_per_1m, price_in_per_1m_peak, price_out_per_1m_peak, price_cached_in_per_1m_peak
		 FROM offerings WHERE model_id = ?`, modelID,
	).Scan(&priceIn, &priceOut, &peakIn, &peakOut, &peakCached); err != nil {
		t.Fatalf("read offering after 0078: %v", err)
	}
	if priceIn != 0.14 || priceOut != 0.28 {
		t.Errorf("base prices changed: in=%v out=%v, want 0.14/0.28 (no backfill)", priceIn, priceOut)
	}
	if peakIn != nil || peakOut != nil || peakCached != nil {
		t.Errorf("peak columns not NULL after migration: in=%v out=%v cached=%v", peakIn, peakOut, peakCached)
	}

	var peakWindows any
	if err := sqlDB.QueryRow(`SELECT peak_windows FROM router_providers WHERE id = ?`, providerID).Scan(&peakWindows); err != nil {
		t.Fatalf("read provider after 0078: %v", err)
	}
	if peakWindows != nil {
		t.Errorf("peak_windows not NULL after migration: %v", peakWindows)
	}

	var promptTokens int64
	var priceTier any
	if err := sqlDB.QueryRow(`SELECT prompt_tokens, price_tier FROM usage_events LIMIT 1`).Scan(&promptTokens, &priceTier); err != nil {
		t.Fatalf("read usage_event after 0078: %v", err)
	}
	if promptTokens != 1000 {
		t.Errorf("pre-existing event data changed: prompt_tokens=%v, want 1000", promptTokens)
	}
	if priceTier != nil {
		t.Errorf("price_tier not NULL for pre-migration event: %v", priceTier)
	}
}
