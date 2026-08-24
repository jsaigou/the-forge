// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestProvidersCatalogRoundTrip covers the §0.3 read side: the extended
// router_providers columns + provider_state cache. Goes through the
// *DB.Providers() surface BE-3 owns; the existing TestCompressorRoundTrip
// covers the legacy 6-column Compressor.Providers() path and stays the owner
// of router_providers CRUD. (provider_models — this test used to seed and
// read it too — was dropped in migration 0043, Phase 7: zero rows on the
// live deployment and no production write path had ever existed for it;
// see providers.go's doc comment.)
func TestProvidersCatalogRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	// Seed router_providers via Compressor.SaveProvider (the legacy write
	// path) — the extended columns land via the 0002 ALTER TABLE defaults
	// and are only readable through Providers.List (the new SELECT). The
	// Compressor proxy link now lives on the proxy row (provider_id, 0042),
	// not a router_providers column — out of scope for this test.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "deepseek", APIKey: "sk-deepseek-secret-12345",
		TargetURL: "https://api.deepseek.com/v1",
		Model:     "deepseek-chat", CreatedAt: ts(1000),
	}); err != nil {
		t.Fatalf("SaveProvider deepseek: %v", err)
	}
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "aiand", APIKey: "sk-aiand-verylongsecret",
		TargetURL: "https://api.aiand.com/v1",
		Model:     "moonshotai/kimi-k2.7-code", Model2: "zai-org/glm-5.2",
		CreatedAt: ts(1001),
	}); err != nil {
		t.Fatalf("SaveProvider aiand: %v", err)
	}

	// ── Providers.List: extended columns readable (defaults from ALTER TABLE) ──
	rows, err := db.Providers().List(ctx)
	if err != nil {
		t.Fatalf("Providers.List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List = %d rows, want 2", len(rows))
	}
	// Ordered by name: aiand, deepseek.
	if rows[0].Name != "aiand" || rows[1].Name != "deepseek" {
		t.Errorf("List order = %s, %s; want aiand, deepseek", rows[0].Name, rows[1].Name)
	}
	// Extended columns default to ('USD', '', '') per 0002_polish.sql.
	for _, r := range rows {
		if r.BillCurrency != "USD" {
			t.Errorf("%s: BillCurrency = %q, want USD (default)", r.Name, r.BillCurrency)
		}
		if r.StatusURL != "" || r.CreditsURL != "" {
			t.Errorf("%s: status_url/credits_url = %q/%q, want empty (default)", r.Name, r.StatusURL, r.CreditsURL)
		}
	}

	// Resolve the real ids SaveProvider assigned.
	deepseekID := testProviderID(t, db, "deepseek")

	// ── provider_state cache: read-missing returns ErrNotFound, save then read ──
	if _, err := db.Providers().State(ctx, deepseekID); !errors.Is(err, ErrNotFound) {
		t.Errorf("State missing: got %v, want ErrNotFound", err)
	}
	healthJSON := `{"state":"reachable","as_of":1700000000.0,"source":"live_probe","detail":null}`
	creditsJSON := `{"balance_native":32.15,"currency":"USD","as_of":1700000000.0,"supported":true}`
	if err := db.Providers().SaveState(ctx, ProviderStateRow{
		ProviderID: deepseekID, HealthJSON: healthJSON, CreditsJSON: creditsJSON,
		FetchedAt: ts(1700000000),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := db.Providers().State(ctx, deepseekID)
	if err != nil {
		t.Fatalf("State after SaveState: %v", err)
	}
	if got.HealthJSON != healthJSON || got.CreditsJSON != creditsJSON {
		t.Errorf("State round-trip: health=%q credits=%q", got.HealthJSON, got.CreditsJSON)
	}
	if !got.FetchedAt.Equal(ts(1700000000)) {
		t.Errorf("FetchedAt = %v, want %v", got.FetchedAt, ts(1700000000))
	}

	// Upsert: SaveState overwrites.
	if err := db.Providers().SaveState(ctx, ProviderStateRow{
		ProviderID: deepseekID, HealthJSON: `{"state":"down"}`, CreditsJSON: `{}`,
		FetchedAt: ts(1700000099),
	}); err != nil {
		t.Fatalf("SaveState upsert: %v", err)
	}
	got, _ = db.Providers().State(ctx, deepseekID)
	if got.HealthJSON != `{"state":"down"}` || !got.FetchedAt.Equal(ts(1700000099)) {
		t.Errorf("upsert: health=%q fetched_at=%v", got.HealthJSON, got.FetchedAt)
	}

	// Compressor.DeleteProvider is a SOFT delete since 0042 (see its doc
	// comment: usage_events.provider_id is a real FK now, so a hard delete
	// would either erase spend history or need a lossy SET NULL/RESTRICT).
	// It must disappear from Providers()/List() but its provider_state and
	// provider_models rows are NOT cascaded away — only an actual row
	// removal (which nothing in this app does anymore) would trigger the
	// schema's ON DELETE CASCADE.
	if err := db.Routing().DeleteProvider(ctx, deepseekID); err != nil {
		t.Fatalf("DeleteProvider deepseek: %v", err)
	}
	if _, err := db.Providers().State(ctx, deepseekID); err != nil {
		t.Errorf("State after soft-delete: got %v, want the row still readable (no cascade on soft delete)", err)
	}
	rows, _ = db.Providers().List(ctx)
	for _, r := range rows {
		if r.Name == "deepseek" {
			t.Error("List should exclude a soft-deleted provider")
		}
	}
	deleted, ok, err := db.Routing().ProviderByID(ctx, deepseekID)
	if err != nil || !ok {
		t.Fatalf("ProviderByID(soft-deleted): ok=%v err=%v", ok, err)
	}
	if deleted.DeletedAt.IsZero() {
		t.Error("ProviderByID should still report DeletedAt set")
	}
}

// TestProvidersCurrencyRoundTrip covers the per-provider bill_currency
// column (Sprint 0 §0.2/§0.3, 0002_polish.sql) end-to-end through both
// write paths (create + update) and both read paths (Compressor.Providers,
// the read-optimized Providers.List). TestProvidersCatalogRoundTrip already
// covers the ('USD', ”, ”) ALTER TABLE defaults; this test covers an
// explicit non-default currency actually persisting and round-tripping,
// which is the part the BE-3 F4 fix-plan DoD calls out by name.
func TestProvidersCurrencyRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	// Create with an explicit non-default currency.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "aiand", APIKey: "sk-aiand-secret", TargetURL: "https://api.aiand.com/v1",
		BillCurrency: "JPY", CreatedAt: ts(2000),
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	rows, err := db.Routing().Providers(ctx)
	if err != nil {
		t.Fatalf("Compressor.Providers: %v", err)
	}
	if len(rows) != 1 || rows[0].BillCurrency != "JPY" {
		t.Fatalf("Compressor.Providers = %+v, want one row with BillCurrency=JPY", rows)
	}

	// The BE-3 read-optimized surface must see the same value.
	catalogRows, err := db.Providers().List(ctx)
	if err != nil {
		t.Fatalf("Providers.List: %v", err)
	}
	if len(catalogRows) != 1 || catalogRows[0].BillCurrency != "JPY" {
		t.Fatalf("Providers.List = %+v, want one row with BillCurrency=JPY", catalogRows)
	}

	// Update to a different currency — SaveProvider is an upsert.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "aiand", APIKey: "sk-aiand-secret", TargetURL: "https://api.aiand.com/v1",
		BillCurrency: "EUR", CreatedAt: ts(2000),
	}); err != nil {
		t.Fatalf("SaveProvider (update): %v", err)
	}
	rows, err = db.Routing().Providers(ctx)
	if err != nil {
		t.Fatalf("Compressor.Providers after update: %v", err)
	}
	if len(rows) != 1 || rows[0].BillCurrency != "EUR" {
		t.Fatalf("Compressor.Providers after update = %+v, want BillCurrency=EUR", rows)
	}

	// Explicitly setting "" on update falls back to USD (SaveProvider's
	// COALESCE(NULLIF(...)) normalization) rather than persisting empty —
	// bill_currency must never be blank once a provider exists.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "aiand", APIKey: "sk-aiand-secret", TargetURL: "https://api.aiand.com/v1",
		BillCurrency: "", CreatedAt: ts(2000),
	}); err != nil {
		t.Fatalf("SaveProvider (clear): %v", err)
	}
	rows, err = db.Routing().Providers(ctx)
	if err != nil {
		t.Fatalf("Compressor.Providers after clear: %v", err)
	}
	if len(rows) != 1 || rows[0].BillCurrency != "USD" {
		t.Fatalf("Compressor.Providers after clearing currency = %+v, want fallback to USD", rows)
	}
}

// TestProvidersEmptyDB covers the no-providers-configured path: List returns
// an empty (non-nil) slice, State returns ErrNotFound. The httpapi handler
// depends on this so handleProvidersList returns {"providers":[]} rather
// than null when no providers are configured.
func TestProvidersEmptyDB(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	rows, err := db.Providers().List(ctx)
	if err != nil {
		t.Fatalf("List on empty DB: %v", err)
	}
	if rows == nil {
		t.Error("List = nil, want empty slice")
	}
	if len(rows) != 0 {
		t.Errorf("List = %d rows, want 0", len(rows))
	}

	if _, err := db.Providers().State(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("State on empty DB: got %v, want ErrNotFound", err)
	}
	// SaveState on a missing provider FK violates the cascade — surfaces as
	// an error (the cache row can only exist for a configured provider).
	if err := db.Providers().SaveState(ctx, ProviderStateRow{
		ProviderID: 999999, HealthJSON: "{}", CreditsJSON: "{}", FetchedAt: time.Now(),
	}); err == nil {
		t.Error("SaveState with no provider row should fail (FK violation)")
	}
}

// TestProvidersBillingColumns covers the 0016 migration's two new columns
// (product/QA sprint, 2026-07-29). Two things worth proving:
//  1. They round-trip correctly through both read paths (Providers.List,
//     the new BE-3 surface, and Compressor.Providers, the legacy surface —
//     both project all columns).
//  2. Unlike bill_currency, the store layer does NOT default BillingEnabled
//     to true when omitted — SaveProvider binds it explicitly, so a caller
//     that doesn't set it gets false, same as any other Go zero-value bool.
//     The "default to true on create" business rule lives in the httpapi
//     handler (handleProviderCreate), not here — this test pins that the
//     store itself stays a plain, un-opinionated CRUD layer.
func TestProvidersBillingColumns(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	// Row saved without touching either new field.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "bare", APIKey: "sk-bare", CreatedAt: ts(1000),
	}); err != nil {
		t.Fatalf("SaveProvider bare: %v", err)
	}
	// Row saved with both new fields set.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "configured", APIKey: "sk-configured",
		BillingEnabled: true, BillingConsoleURL: "https://platform.example.com/usage",
		CreatedAt: ts(1001),
	}); err != nil {
		t.Fatalf("SaveProvider configured: %v", err)
	}

	for _, read := range []struct {
		name string
		fn   func() ([]ProviderRow, error)
	}{
		{"Providers.List", func() ([]ProviderRow, error) { return db.Providers().List(ctx) }},
		{"Compressor.Providers", func() ([]ProviderRow, error) { return db.Routing().Providers(ctx) }},
	} {
		rows, err := read.fn()
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		byName := map[string]ProviderRow{}
		for _, r := range rows {
			byName[r.Name] = r
		}
		bare, ok := byName["bare"]
		if !ok {
			t.Fatalf("%s: missing 'bare' row", read.name)
		}
		if bare.BillingEnabled {
			t.Errorf("%s: bare.BillingEnabled = true, want false (store doesn't default it)", read.name)
		}
		if bare.BillingConsoleURL != "" {
			t.Errorf("%s: bare.BillingConsoleURL = %q, want \"\"", read.name, bare.BillingConsoleURL)
		}
		configured, ok := byName["configured"]
		if !ok {
			t.Fatalf("%s: missing 'configured' row", read.name)
		}
		if !configured.BillingEnabled {
			t.Errorf("%s: configured.BillingEnabled = false, want true", read.name)
		}
		if configured.BillingConsoleURL != "https://platform.example.com/usage" {
			t.Errorf("%s: configured.BillingConsoleURL = %q", read.name, configured.BillingConsoleURL)
		}
	}

	// Toggling it off via a second SaveProvider (the update path) must
	// actually flip it, not just leave the original value stuck.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "configured", APIKey: "sk-configured",
		BillingEnabled: false, BillingConsoleURL: "https://platform.example.com/usage",
		CreatedAt: ts(1001),
	}); err != nil {
		t.Fatalf("SaveProvider configured (toggle off): %v", err)
	}
	rows, err := db.Providers().List(ctx)
	if err != nil {
		t.Fatalf("List after toggle: %v", err)
	}
	for _, r := range rows {
		if r.Name == "configured" && r.BillingEnabled {
			t.Error("expected billing_enabled to flip to false on re-save")
		}
	}
}

// TestProviderEnabledCountryRoundTrip covers the 0032 columns: enabled
// (disable without deleting) + country/data_residency_group (surfaced from
// the 0008 columns that were stored-but-unread before). Both read paths —
// the legacy Compressor.Providers() and the extended Providers.List() — must
// project them identically.
func TestProviderEnabledCountryRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "qwen", APIKey: "sk-q", TargetURL: "https://example.com/v1",
		Enabled: true, Country: "SG", DataResidencyGroup: "Southeast Asia",
		CreatedAt: ts(2000),
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	compressorRows, err := db.Routing().Providers(ctx)
	if err != nil {
		t.Fatalf("Compressor.Providers: %v", err)
	}
	if !compressorRows[0].Enabled || compressorRows[0].Country != "SG" ||
		compressorRows[0].DataResidencyGroup != "Southeast Asia" {
		t.Errorf("Compressor projection = %+v, want enabled/SG/Southeast Asia", compressorRows[0])
	}

	// Disable via the same upsert the update handler uses, then confirm BOTH
	// surfaces see the flip.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{
		Name: "qwen", APIKey: "sk-q", TargetURL: "https://example.com/v1",
		Enabled: false, Country: "SG", DataResidencyGroup: "Southeast Asia",
		CreatedAt: ts(2000),
	}); err != nil {
		t.Fatalf("SaveProvider (disable): %v", err)
	}
	listRows, err := db.Providers().List(ctx)
	if err != nil {
		t.Fatalf("Providers.List: %v", err)
	}
	if len(listRows) != 1 {
		t.Fatalf("List = %d rows, want 1", len(listRows))
	}
	if listRows[0].Enabled {
		t.Error("Providers.List: enabled should be false after disable")
	}
	if listRows[0].Country != "SG" || listRows[0].DataResidencyGroup != "Southeast Asia" {
		t.Errorf("Providers.List residency = %q/%q", listRows[0].Country, listRows[0].DataResidencyGroup)
	}
	compressorRows, _ = db.Routing().Providers(ctx)
	if compressorRows[0].Enabled {
		t.Error("Compressor.Providers: enabled should be false after disable")
	}
}
