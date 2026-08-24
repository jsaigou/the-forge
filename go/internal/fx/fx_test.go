// SPDX-License-Identifier: Apache-2.0

package fx_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/fx"
	"github.com/jsaigou/the-forge/internal/store"
)

// openDB opens an in-memory store (runs migrations 0001+0002 → fx_rates +
// router_providers with bill_currency).
func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeSettings is a minimal store.Settings for driving the billing keys.
type fakeSettings struct{ kv map[string][]byte }

func newFakeSettings() *fakeSettings { return &fakeSettings{kv: map[string][]byte{}} }

func (f *fakeSettings) Get(_ context.Context, key string) ([]byte, error) {
	v, ok := f.kv[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}
func (f *fakeSettings) Set(_ context.Context, key string, value []byte) error {
	f.kv[key] = value
	return nil
}

func setJSON(t *testing.T, s *fakeSettings, key string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	if err := s.Set(context.Background(), key, b); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}

// staticFetch returns a fetch func yielding the given base+rates. When failOn
// > 0, the failOn-th call returns err (1-indexed) instead of rates.
func staticFetch(base string, rates map[string]float64, failOn int) func(context.Context, string) (string, map[string]float64, error) {
	var calls int
	return func(_ context.Context, _ string) (string, map[string]float64, error) {
		calls++
		if failOn > 0 && calls >= failOn {
			return "", nil, errors.New("simulated fetch failure")
		}
		// Return a copy so the cache can't mutate our fixture.
		out := make(map[string]float64, len(rates))
		for k, v := range rates {
			out[k] = v
		}
		return base, out, nil
	}
}

func TestRate_DirectInverseCross(t *testing.T) {
	db := openDB(t)
	fetch := staticFetch("USD", map[string]float64{"EUR": 0.9, "CNY": 7.2}, 0)
	c := fx.New(db.SQL(), nil, fx.WithFetch(fetch))
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	ctx := context.Background()
	cases := []struct {
		from, to string
		want     float64
	}{
		{"USD", "USD", 1.0},
		{"usd", "eur", 0.9},       // case-insensitive
		{"EUR", "USD", 1 / 0.9},   // inverse
		{"EUR", "CNY", 7.2 / 0.9}, // cross via USD base
		{"CNY", "EUR", 0.9 / 7.2}, // cross the other way
	}
	for _, tc := range cases {
		got, ok := c.Rate(ctx, tc.from, tc.to)
		if !ok {
			t.Errorf("Rate(%s→%s): ok=false, want true", tc.from, tc.to)
			continue
		}
		if approx(got, tc.want, 1e-9) {
			continue
		}
		t.Errorf("Rate(%s→%s) = %v, want %v", tc.from, tc.to, got, tc.want)
	}

	if _, ok := c.Rate(ctx, "USD", "JPY"); ok {
		t.Errorf("Rate(USD→JPY) should be ok=false (unknown quote)")
	}
}

func TestProvenance_FreshAndStale(t *testing.T) {
	db := openDB(t)
	fetch := staticFetch("USD", map[string]float64{"EUR": 0.9}, 0)
	c := fx.New(db.SQL(), nil, fx.WithFetch(fetch))
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	asOf, stale, hasRates := c.Provenance(context.Background())
	if !hasRates {
		t.Fatal("hasRates=false after a successful refresh")
	}
	if stale {
		t.Error("rate should be fresh immediately after refresh")
	}
	if asOf.IsZero() {
		t.Error("asOf should be non-zero after a successful refresh")
	}

	// Simulate a stale cache: age the persisted rows, then build a fresh
	// Cache (loadFromDB runs in New) so Provenance reads the aged rate.
	old := time.Now().Add(-3 * time.Hour).Unix()
	if _, err := db.SQL().Exec(
		`UPDATE fx_rates SET fetched_at = ? WHERE base = 'USD'`, old); err != nil {
		t.Fatalf("age fx_rates: %v", err)
	}
	c2 := fx.New(db.SQL(), nil, fx.WithFetch(fetch))
	_, stale2, hasRates2 := c2.Provenance(context.Background())
	if !hasRates2 {
		t.Fatal("hasRates=false after loading aged cache")
	}
	if !stale2 {
		t.Error("rate should be stale (fetched 3h ago > 2×60min threshold)")
	}
}

func TestProvenance_NoRatesWhenNeverFetched(t *testing.T) {
	db := openDB(t)
	fetch := staticFetch("USD", map[string]float64{"EUR": 0.9}, 0)
	c := fx.New(db.SQL(), nil, fx.WithFetch(fetch)) // no Refresh
	_, _, hasRates := c.Provenance(context.Background())
	if hasRates {
		t.Error("hasRates should be false before any refresh")
	}
	if _, ok := c.Rate(context.Background(), "USD", "EUR"); ok {
		t.Error("Rate should be ok=false with no rates cached")
	}
}

func TestRefreshFailure_KeepsCachedRate(t *testing.T) {
	db := openDB(t)
	// First call succeeds, second call fails (failOn=2).
	fetch := staticFetch("USD", map[string]float64{"EUR": 0.9}, 2)
	c := fx.New(db.SQL(), nil, fx.WithFetch(fetch))

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if got, ok := c.Rate(context.Background(), "USD", "EUR"); !ok || got != 0.9 {
		t.Fatalf("after first refresh Rate = (%v, %v), want (0.9, true)", got, ok)
	}

	// Second refresh fails; the cached rate must stay served.
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("second Refresh should have failed")
	}
	if got, ok := c.Rate(context.Background(), "USD", "EUR"); !ok || got != 0.9 {
		t.Errorf("after failed refresh Rate = (%v, %v), want cached (0.9, true)", got, ok)
	}
	_, _, hasRates := c.Provenance(context.Background())
	if !hasRates {
		t.Error("hasRates should still be true after a failed refresh (cached)")
	}
}

func TestRates_SurviveRestart(t *testing.T) {
	// Persist with cache A, then construct cache B against the same DB handle
	// (loadFromDB runs in New) and confirm the rates are served without a
	// Refresh — the restart-recovery path.
	db := openDB(t)
	fetch := staticFetch("USD", map[string]float64{"EUR": 0.9, "CNY": 7.2}, 0)
	cA := fx.New(db.SQL(), nil, fx.WithFetch(fetch))
	if err := cA.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh A: %v", err)
	}

	cB := fx.New(db.SQL(), nil, fx.WithFetch(fetch)) // no Refresh on B
	got, ok := cB.Rate(context.Background(), "EUR", "CNY")
	if !ok {
		t.Fatal("Rate(EUR→CNY) after restart: ok=false, want true (loaded from fx_rates)")
	}
	if !approx(got, 7.2/0.9, 1e-9) {
		t.Errorf("Rate(EUR→CNY) after restart = %v, want %v", got, 7.2/0.9)
	}
}

func TestDisplayCurrency_FromSettings(t *testing.T) {
	db := openDB(t)
	s := newFakeSettings()
	c := fx.New(db.SQL(), s)

	if got := c.DisplayCurrency(context.Background()); got != "USD" {
		t.Errorf("unset display_currency = %q, want USD", got)
	}
	setJSON(t, s, "billing.display_currency", "cny")
	if got := c.DisplayCurrency(context.Background()); got != "CNY" {
		t.Errorf("display_currency = %q, want CNY (upper-cased)", got)
	}
}

func TestBillCurrency_FromProviders(t *testing.T) {
	db := openDB(t)
	s := newFakeSettings()
	// Seed a router_providers row via the store API (handles created_at +
	// 0002's bill_currency column, which SaveProvider leaves at its default).
	if err := db.Routing().SaveProvider(context.Background(), store.ProviderRow{
		Name:      "deepseek",
		APIKey:    "sk-x",
		TargetURL: "https://api.deepseek.com",
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	// SaveProvider doesn't write bill_currency; set it directly so the
	// non-USD path is exercised.
	if _, err := db.SQL().Exec(
		`UPDATE router_providers SET bill_currency = 'CNY' WHERE name = 'deepseek'`); err != nil {
		t.Fatalf("set bill_currency: %v", err)
	}
	c := fx.New(db.SQL(), s)

	if got := c.BillCurrency(context.Background(), "deepseek"); got != "CNY" {
		t.Errorf("BillCurrency(deepseek) = %q, want CNY", got)
	}
	if got := c.BillCurrency(context.Background(), "unknown"); got != "USD" {
		t.Errorf("BillCurrency(unknown) = %q, want USD default", got)
	}
}

// TestHTTPFetch_ParsesFrankfurterShape confirms the default fetcher parses the
// {base, rates} JSON shape the §0.2 provider pick returns.
func TestHTTPFetch_ParsesFrankfurterShape(t *testing.T) {
	db := openDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"base":"USD","date":"2026-07-22","rates":{"EUR":0.92,"CNY":7.2}}`))
	}))
	t.Cleanup(srv.Close)

	s := newFakeSettings()
	setJSON(t, s, "billing.fx_source_url", srv.URL+"/latest?from=USD")
	c := fx.New(db.SQL(), s)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, ok := c.Rate(context.Background(), "USD", "EUR")
	if !ok || !approx(got, 0.92, 1e-9) {
		t.Errorf("Rate(USD→EUR) = (%v, %v), want (0.92, true)", got, ok)
	}
}

func approx(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
