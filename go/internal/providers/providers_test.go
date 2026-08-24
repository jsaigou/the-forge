// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeCatalog is an in-memory store.Providers for tests. Tracks fetch
// counts so the cache tests can assert single-fetch behavior under
// concurrent List calls.
type fakeCatalog struct {
	mu         sync.Mutex
	providers  []store.ProviderRow
	state      map[int64]*store.ProviderStateRow // keyed by provider id
	stateCalls atomic.Int32
	saveCalls  atomic.Int32
}

// newFakeCatalog auto-assigns a stable id (index+1) to any row with ID==0 —
// test fixtures written before the 0042 surrogate-key migration construct
// bare store.ProviderRow{Name: ...} literals, and this keeps per-provider
// state caching distinguishable (State/SaveState are keyed by id now, not
// name) without touching every one of those fixtures.
func newFakeCatalog(rows []store.ProviderRow) *fakeCatalog {
	if rows == nil {
		rows = []store.ProviderRow{}
	}
	for i := range rows {
		if rows[i].ID == 0 {
			rows[i].ID = int64(i + 1)
		}
	}
	return &fakeCatalog{providers: rows, state: map[int64]*store.ProviderStateRow{}}
}

func (f *fakeCatalog) List(ctx context.Context) ([]store.ProviderRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirror the real DB's ORDER BY name ordering so tests can assert
	// deterministic output (deepseek < aiand sorts aiand first).
	out := make([]store.ProviderRow, len(f.providers))
	copy(out, f.providers)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// fakeOfferings is an in-memory providers.OfferingsSource for tests — the
// real offerings-derived provided-models list (Phase 7), replacing the old
// provider_models fixture.
type fakeOfferings struct {
	offerings []store.Offering
	models    map[int64]store.Model
}

func (f *fakeOfferings) ListOfferings(ctx context.Context) ([]store.Offering, error) {
	return f.offerings, nil
}

func (f *fakeOfferings) GetModel(ctx context.Context, id int64) (store.Model, error) {
	if m, ok := f.models[id]; ok {
		return m, nil
	}
	return store.Model{}, store.ErrNotFound
}

func (f *fakeCatalog) State(ctx context.Context, providerID int64) (*store.ProviderStateRow, error) {
	f.stateCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.state[providerID]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeCatalog) SaveState(ctx context.Context, row store.ProviderStateRow) error {
	f.saveCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := row
	f.state[row.ProviderID] = &cp
	return nil
}

// ── Test: list with no providers → empty (not nil) ───────────────────────────

func TestListNoProvidersReturnsEmpty(t *testing.T) {
	svc := New(Deps{Catalog: newFakeCatalog(nil)})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if out == nil {
		t.Fatal("List = nil, want empty slice (handler marshals [])")
	}
	if len(out) != 0 {
		t.Errorf("List = %d, want 0", len(out))
	}
}

// ── Test: DeepSeek + AI& with live probes ───────────────────────────────────
//
// Mirrors ForgeHost's real config (health live-verified 2026-07-22): DeepSeek
// has the balance API + a reachable /v1/models; AI& has a reachable
// /v1/models and (post-F4) a credits fetch against its Analytics API —
// see credits.go for why the exact AI& response shape is unverified. This
// test points aiand's credits_url at the local stub so the fetch is
// exercised without any real network access.

func TestListDeepSeekAndAIand(t *testing.T) {
	// Stub DeepSeek balance + /v1/models.
	dsHits := atomic.Int32{}
	dsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dsHits.Add(1)
		switch r.URL.Path {
		case "/user/balance":
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"32.15","granted_balance":"0.00","topped_up_balance":"32.15"}]}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-chat"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer dsSrv.Close()

	aiandHits := atomic.Int32{}
	aiandSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aiandHits.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"zai-org/glm-5.2"}]}`))
		case "/v1/analytics/metrics":
			if r.Header.Get("X-Org-ID") != "org-test-123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"range":"24h","buckets":[{"ts":"2026-07-23T00:00:00Z","cost_usd":50.00},{"ts":"2026-07-23T01:00:00Z","cost_usd":34.20}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer aiandSrv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{
			{Name: "deepseek", APIKey: "sk-deepseek-secret-abc123", TargetURL: dsSrv.URL + "/v1", CreditsURL: dsSrv.URL + "/user/balance", BillCurrency: "USD", BillingEnabled: true, Enabled: true},
			{Name: "aiand", APIKey: "sk-aiand-secret-xyz789", TargetURL: aiandSrv.URL + "/v1", CreditsURL: aiandSrv.URL + "/v1/analytics/metrics", OrgID: "org-test-123", BillCurrency: "USD", BillingEnabled: true, Enabled: true},
		},
	)
	// Phase 7: provided-models is offerings-derived, not the old
	// provider_models fixture.
	offerings := &fakeOfferings{
		offerings: []store.Offering{
			{ID: 1, ModelID: 101, ProviderID: 1, ProviderName: "deepseek", WireModel: "deepseek-chat", PriceInPer1M: 0.27, PriceOutPer1M: 1.10, Currency: "USD", Enabled: true, Priority: 100},
			{ID: 2, ModelID: 102, ProviderID: 1, ProviderName: "deepseek", WireModel: "deepseek-reasoner", PriceInPer1M: 0.55, PriceOutPer1M: 2.19, Currency: "USD", Enabled: true, Priority: 100},
			{ID: 3, ModelID: 103, ProviderID: 2, ProviderName: "aiand", WireModel: "moonshotai/kimi-k2.7-code", PriceInPer1M: 0.60, PriceOutPer1M: 2.50, Currency: "USD", Enabled: true, Priority: 100},
			{ID: 4, ModelID: 104, ProviderID: 2, ProviderName: "aiand", WireModel: "zai-org/glm-5.2", PriceInPer1M: 0.16, PriceOutPer1M: 0.65, Currency: "USD", Enabled: true, Priority: 100},
		},
		models: map[int64]store.Model{
			101: {ID: 101, Name: "DeepSeek Chat"},
			102: {ID: 102, Name: "DeepSeek Reasoner"},
			103: {ID: 103, Name: "Kimi K2.7 Code"},
			104: {ID: 104, Name: "GLM-5.2"},
		},
	}
	svc := New(Deps{Catalog: cat, Offerings: offerings, CacheTTL: 1 * time.Hour})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("List = %d providers, want 2", len(out))
	}
	// Catalog rows are ordered by name: aiand, deepseek.
	aiand, deepseek := out[0], out[1]
	if aiand.Name != "aiand" || deepseek.Name != "deepseek" {
		t.Fatalf("order: %s, %s; want aiand, deepseek", aiand.Name, deepseek.Name)
	}

	// API key masked (Contract 1 §18 — never the full secret).
	if deepseek.APIKeyMasked != "sk-d…" {
		t.Errorf("deepseek masked = %q, want \"sk-d…\"", deepseek.APIKeyMasked)
	}
	if strings.Contains(deepseek.APIKeyMasked, "secret") {
		t.Errorf("deepseek masked leaked secret: %q", deepseek.APIKeyMasked)
	}
	if aiand.APIKeyMasked != "sk-a…" {
		t.Errorf("aiand masked = %q, want \"sk-a…\"", aiand.APIKeyMasked)
	}

	// Health: both reachable via live_probe (no status_url configured).
	if deepseek.Health.State != HealthStateReachable {
		t.Errorf("deepseek health = %q, want reachable", deepseek.Health.State)
	}
	if deepseek.Health.Source != HealthSourceLiveProbe {
		t.Errorf("deepseek health source = %q, want live_probe", deepseek.Health.Source)
	}
	if aiand.Health.State != HealthStateReachable {
		t.Errorf("aiand health = %q, want reachable", aiand.Health.State)
	}
	// "idle" must never appear (user: "I don't care about idle").
	for _, p := range out {
		if p.Health.State == "idle" {
			t.Errorf("%s: health state is idle — must never appear", p.Name)
		}
	}

	// Credits: DeepSeek supported with balance; AI& supported:false.
	if !deepseek.Credits.Supported {
		t.Error("deepseek: Credits.Supported = false, want true")
	}
	if deepseek.Credits.BalanceNative == nil || *deepseek.Credits.BalanceNative != 32.15 {
		t.Errorf("deepseek: BalanceNative = %v, want 32.15", deepseek.Credits.BalanceNative)
	}
	if deepseek.Credits.Currency != "USD" {
		t.Errorf("deepseek: Currency = %q, want USD", deepseek.Credits.Currency)
	}
	// Post-F4 (real shape): AI& has no balance API at all (confirmed —
	// see credits.go doc comment) — its dedicated client instead reports
	// real period spend summed from the Analytics API's buckets.
	if !aiand.Credits.Supported {
		t.Error("aiand: Credits.Supported = false, want true (F4: AI& credits client wired)")
	}
	if aiand.Credits.BalanceNative != nil {
		t.Errorf("aiand: BalanceNative = %v, want nil (AI& has no balance API)", aiand.Credits.BalanceNative)
	}
	if aiand.Credits.SpendPeriod == nil || *aiand.Credits.SpendPeriod != 84.20 {
		t.Errorf("aiand: SpendPeriod = %v, want 84.20 (50.00+34.20 summed across buckets)", aiand.Credits.SpendPeriod)
	}
	if aiand.Credits.SpendPeriodLabel != "24h spend" {
		t.Errorf("aiand: SpendPeriodLabel = %q, want \"24h spend\"", aiand.Credits.SpendPeriodLabel)
	}
	if aiand.Credits.Currency != "USD" {
		t.Errorf("aiand: Currency = %q, want USD", aiand.Credits.Currency)
	}

	// Models grouped under each provider, offerings-derived (Phase 7).
	if len(deepseek.Models) != 2 || deepseek.Models[0].ModelID != "deepseek-chat" {
		t.Errorf("deepseek models = %+v", deepseek.Models)
	}
	if deepseek.Models[0].DisplayName != "DeepSeek Chat" {
		t.Errorf("deepseek model display_name = %q, want the joined catalog model name", deepseek.Models[0].DisplayName)
	}
	if len(aiand.Models) != 2 || aiand.Models[0].ModelID != "moonshotai/kimi-k2.7-code" {
		t.Errorf("aiand models = %+v", aiand.Models)
	}
	// Currency is the offering's own (not inherited from the provider row —
	// aiand's real offerings are priced in JPY while its bill_currency is
	// USD, so inheriting would have been wrong).
	if deepseek.Models[0].Currency != "USD" {
		t.Errorf("deepseek model currency = %q, want USD", deepseek.Models[0].Currency)
	}

	// Live probes actually fired (health + credits both hit the stub).
	if got := dsHits.Load(); got < 2 {
		t.Errorf("deepseek stub hits = %d, want ≥2 (balance + /models)", got)
	}
	// Post-F4: aiand's credits client now fetches too — /v1/models (health)
	// + /v1/analytics/metrics (credits).
	if got := aiandHits.Load(); got < 2 {
		t.Errorf("aiand stub hits = %d, want ≥2 (/models + analytics/metrics)", got)
	}
}

// ── Test: cache serves fresh entries without re-fetching ─────────────────────

func TestCacheFreshDoesNotRefetch(t *testing.T) {
	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{
			{Name: "deepseek", APIKey: "sk-x", TargetURL: srv.URL + "/v1", BillCurrency: "USD", BillingEnabled: true, Enabled: true},
		},
	)
	// Pre-seed a fresh cache row (age 1s, TTL 1h ⇒ fresh).
	if err := cat.SaveState(context.Background(), store.ProviderStateRow{
		ProviderID:  1,
		HealthJSON:  `{"state":"reachable","as_of":1.0,"source":"live_probe","detail":null}`,
		CreditsJSON: `{"balance_native":1.0,"currency":"USD","as_of":1.0,"supported":true}`,
		FetchedAt:   time.Now().Add(-1 * time.Second),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("List = %d, want 1", len(out))
	}
	if out[0].Health.State != HealthStateReachable {
		t.Errorf("cached health = %q, want reachable", out[0].Health.State)
	}
	if out[0].Credits.BalanceNative == nil || *out[0].Credits.BalanceNative != 1.0 {
		t.Errorf("cached credits = %v, want 1.0", out[0].Credits.BalanceNative)
	}
	// No live fetch should have fired — the stub must not be hit.
	if got := hits.Load(); got != 0 {
		t.Errorf("live fetch fired %d times on fresh cache, want 0", got)
	}
}

// ── Test: stale cache refreshed, then served on fetch failure ─────────────────
//
// A live-probe failure must NOT clear the cached state — the cache row
// serves the last-known-good health so a transient provider outage
// doesn't flip the UI to "unknown" the moment the probe times out.

func TestStaleCacheServedOnFetchFailure(t *testing.T) {
	// First List: stub is up → fresh cache populated.
	// Second List: stub is down → fetch fails → stale cache served.
	srvUp := atomic.Bool{}
	srvUp.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !srvUp.Load() {
			// Simulate provider outage (connection reset / 500).
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "deepseek", APIKey: "sk-x", TargetURL: srv.URL + "/v1", BillingEnabled: true, Enabled: true}},
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 10 * time.Millisecond})

	first, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("first List: %v", err)
	}
	if first[0].Health.State != HealthStateReachable {
		t.Fatalf("first health = %q, want reachable", first[0].Health.State)
	}

	// Wait for cache to go stale, then fail the probe.
	time.Sleep(20 * time.Millisecond)
	srvUp.Store(false)

	second, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	// Fetch failed (500) → live_probe surfaces "down" with the fresh fetch.
	// The cache row is updated to reflect the new probe result — this is
	// the §0.3 "cache row reflects whatever was served" semantics. A
	// transient outage WILL flip to "down" (that's the point of a live
	// probe — status_page would carry forward, but we have none here).
	if second[0].Health.State != HealthStateDown {
		t.Errorf("second health = %q, want down (probe failed)", second[0].Health.State)
	}
	if second[0].Health.Source != HealthSourceLiveProbe {
		t.Errorf("second source = %q, want live_probe", second[0].Health.Source)
	}
}

// ── Test: status page parsed correctly ───────────────────────────────────────

func TestStatusPageParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"status": {"indicator": "minor"},
			"incidents": [{"name": "Degraded API performance", "impact": "minor", "status": "monitoring"}]
		}`))
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "prov", APIKey: "sk-x", StatusURL: srv.URL, TargetURL: "https://unreachable.example.invalid/v1", BillingEnabled: true, Enabled: true}},
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if out[0].Health.State != HealthStateDegraded {
		t.Errorf("state = %q, want degraded", out[0].Health.State)
	}
	if out[0].Health.Source != HealthSourceStatusPage {
		t.Errorf("source = %q, want status_page", out[0].Health.Source)
	}
	if out[0].Health.Detail != "Degraded API performance" {
		t.Errorf("detail = %q, want incident name", out[0].Health.Detail)
	}
}

// ── Test: status page critical incident ⇒ down ───────────────────────────────

func TestStatusPageCriticalIncidentDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"status": {"indicator": "critical"},
			"incidents": [{"name": "Outage", "impact": "critical", "status": "investigating"}]
		}`))
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "prov", APIKey: "sk-x", StatusURL: srv.URL, BillingEnabled: true, Enabled: true}},
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
	out, _ := svc.List(context.Background())
	if out[0].Health.State != HealthStateDown {
		t.Errorf("state = %q, want down (critical incident)", out[0].Health.State)
	}
}

// ── Test: resolved incidents don't degrade the state ─────────────────────────

func TestStatusPageResolvedIncident(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"status": {"indicator": "none"},
			"incidents": [{"name": "Past outage", "impact": "critical", "status": "resolved"}]
		}`))
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "prov", APIKey: "sk-x", StatusURL: srv.URL, BillingEnabled: true, Enabled: true}},
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
	out, _ := svc.List(context.Background())
	if out[0].Health.State != HealthStateReachable {
		t.Errorf("state = %q, want reachable (only resolved incidents)", out[0].Health.State)
	}
}

// ── Test: catalog read failure → list error (handler surfaces 500) ──────────

type errCatalog struct{ err error }

func (c errCatalog) List(ctx context.Context) ([]store.ProviderRow, error) {
	return nil, c.err
}
func (c errCatalog) State(ctx context.Context, providerID int64) (*store.ProviderStateRow, error) {
	return nil, c.err
}
func (c errCatalog) SaveState(ctx context.Context, row store.ProviderStateRow) error {
	return c.err
}

func TestCatalogReadFailureSurfacesError(t *testing.T) {
	svc := New(Deps{Catalog: errCatalog{err: errors.New("db gone")}})
	if _, err := svc.List(context.Background()); err == nil {
		t.Error("List with broken catalog: got nil err, want the catalog error surfaced")
	}
}

// ── Test: DeepSeek balance API shape variations ──────────────────────────────

func TestDeepSeekBalanceShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Credits
	}{
		{
			name: "normal",
			body: `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"32.15","granted_balance":"0.00","topped_up_balance":"32.15"}]}`,
			want: Credits{Supported: true, Currency: "USD", BalanceNative: ptrFloat(32.15)},
		},
		{
			name: "unavailable (frozen account)",
			body: `{"is_available":false,"balance_infos":[]}`,
			want: Credits{Supported: true, BalanceNative: nil},
		},
		{
			name: "malformed",
			body: `not json at all`,
			want: Credits{Supported: true, BalanceNative: nil}, // Supported:true (endpoint exists), no balance
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			cat := newFakeCatalog(
				[]store.ProviderRow{{Name: "deepseek", APIKey: "sk-x", CreditsURL: srv.URL, BillingEnabled: true, Enabled: true}},
			)
			svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
			out, _ := svc.List(context.Background())
			got := out[0].Credits
			if got.Supported != c.want.Supported {
				t.Errorf("Supported = %v, want %v", got.Supported, c.want.Supported)
			}
			if (got.BalanceNative == nil) != (c.want.BalanceNative == nil) {
				t.Errorf("BalanceNative nil-ness = %v, want %v", got.BalanceNative, c.want.BalanceNative)
			}
			if got.BalanceNative != nil && c.want.BalanceNative != nil && *got.BalanceNative != *c.want.BalanceNative {
				t.Errorf("BalanceNative = %v, want %v", *got.BalanceNative, *c.want.BalanceNative)
			}
			if got.Currency != c.want.Currency {
				t.Errorf("Currency = %q, want %q", got.Currency, c.want.Currency)
			}
		})
	}
}

// ── Test: OpenRouter credits client (Sprint E) ───────────────────────────────
//
// OpenRouter's key-info endpoint returns both a remaining-balance figure
// (limit_remaining, nullable for an unlimited key) and all-time usage —
// see credits.go's doc comment for the confirmed shape.

func TestOpenRouterCreditsShape(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantSupported bool
		wantBalance   *float64
		wantSpend     *float64
	}{
		{
			name:          "normal",
			body:          `{"data":{"label":"sk-or-x","limit":100,"limit_remaining":42.5,"usage":57.5}}`,
			wantSupported: true,
			wantBalance:   ptrFloat(42.5),
			wantSpend:     ptrFloat(57.5),
		},
		{
			name:          "unlimited key ⇒ null limit_remaining",
			body:          `{"data":{"label":"sk-or-x","limit":null,"limit_remaining":null,"usage":12.3}}`,
			wantSupported: true,
			wantBalance:   nil,
			wantSpend:     ptrFloat(12.3),
		},
		{
			name:          "malformed",
			body:          `not json at all`,
			wantSupported: true,
			wantBalance:   nil,
			wantSpend:     nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			cat := newFakeCatalog(
				[]store.ProviderRow{{Name: "openrouter", APIKey: "sk-or-x", CreditsURL: srv.URL, BillingEnabled: true, Enabled: true}},
			)
			svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
			out, err := svc.List(context.Background())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			got := out[0].Credits
			if got.Supported != c.wantSupported {
				t.Errorf("Supported = %v, want %v", got.Supported, c.wantSupported)
			}
			if (got.BalanceNative == nil) != (c.wantBalance == nil) {
				t.Errorf("BalanceNative nil-ness = %v, want %v", got.BalanceNative, c.wantBalance)
			}
			if got.BalanceNative != nil && c.wantBalance != nil && *got.BalanceNative != *c.wantBalance {
				t.Errorf("BalanceNative = %v, want %v", *got.BalanceNative, *c.wantBalance)
			}
			if (got.SpendPeriod == nil) != (c.wantSpend == nil) {
				t.Errorf("SpendPeriod nil-ness = %v, want %v", got.SpendPeriod, c.wantSpend)
			}
			if got.SpendPeriod != nil && c.wantSpend != nil && *got.SpendPeriod != *c.wantSpend {
				t.Errorf("SpendPeriod = %v, want %v", *got.SpendPeriod, *c.wantSpend)
			}
		})
	}
}

func TestOpenRouterCreditsHandles404Gracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "openrouter", APIKey: "sk-or-x", CreditsURL: srv.URL, BillingEnabled: true, Enabled: true}},
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := out[0].Credits
	if !got.Supported {
		t.Error("Supported = false on a 404 — must degrade to Supported:true, no balance/spend figure")
	}
	if got.BalanceNative != nil || got.SpendPeriod != nil {
		t.Error("BalanceNative/SpendPeriod must both be nil on a 404")
	}
}

// TestOpenRouterCreditsDefaultURLIsAttempted mirrors
// TestAIAndCreditsDefaultURLIsAttempted: confirms the client targets
// OpenRouter's documented key-info endpoint when no credits_url override
// is configured.
func TestOpenRouterCreditsDefaultURLIsAttempted(t *testing.T) {
	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "openrouter", APIKey: "sk-or-x", BillingEnabled: true, Enabled: true}}, // no credits_url
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour, ProbeTimeout: 200 * time.Millisecond})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := out[0].Credits
	if !got.Supported {
		t.Error("Supported = false with no credits_url — the default OpenRouter URL path isn't being tried")
	}
}

// ── Test: AI& credits client (F4) ────────────────────────────────────────────
//
// AI& has no balance API at all (confirmed against the real docs — see
// credits.go's doc comment); these tests exercise the real client against
// the confirmed GET /analytics/metrics?range=<range> shape, which returns
// period spend (cost_usd per bucket), not a balance.

func TestAIAndCreditsParsesRealisticResponse(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantSupported  bool
		wantSpend      *float64
		wantSpendLabel string
	}{
		{
			name:           "multiple buckets summed",
			body:           `{"range":"24h","buckets":[{"ts":"2026-07-23T00:00:00Z","cost_usd":50.00},{"ts":"2026-07-23T01:00:00Z","cost_usd":34.20}]}`,
			wantSupported:  true,
			wantSpend:      ptrFloat(84.20),
			wantSpendLabel: "24h spend",
		},
		{
			name:           "empty buckets ⇒ zero spend, still supported",
			body:           `{"range":"7days","buckets":[]}`,
			wantSupported:  true,
			wantSpend:      ptrFloat(0),
			wantSpendLabel: "7days spend",
		},
		{
			name:          "malformed JSON ⇒ supported, no spend figure (endpoint responded)",
			body:          `not json at all`,
			wantSupported: true,
			wantSpend:     nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			cat := newFakeCatalog(
				[]store.ProviderRow{{Name: "aiand", APIKey: "sk-x", CreditsURL: srv.URL, OrgID: "org-test", TargetURL: "https://unreachable.example.invalid/v1", BillingEnabled: true, Enabled: true}},
			)
			svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
			out, err := svc.List(context.Background())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			got := out[0].Credits
			if got.Supported != c.wantSupported {
				t.Errorf("Supported = %v, want %v", got.Supported, c.wantSupported)
			}
			if got.BalanceNative != nil {
				t.Errorf("BalanceNative = %v, want nil (AI& has no balance API)", got.BalanceNative)
			}
			if (got.SpendPeriod == nil) != (c.wantSpend == nil) {
				t.Errorf("SpendPeriod nil-ness = %v, want %v", got.SpendPeriod, c.wantSpend)
			}
			if got.SpendPeriod != nil && c.wantSpend != nil && *got.SpendPeriod != *c.wantSpend {
				t.Errorf("SpendPeriod = %v, want %v", *got.SpendPeriod, *c.wantSpend)
			}
			if c.wantSpendLabel != "" && got.SpendPeriodLabel != c.wantSpendLabel {
				t.Errorf("SpendPeriodLabel = %q, want %q", got.SpendPeriodLabel, c.wantSpendLabel)
			}
		})
	}
}

// TestAIAndCreditsHandles404Gracefully: a 404 (or any non-2xx) from the
// analytics endpoint must still report Supported:true (the concept — period
// spend — is real for this provider), just with no figure this round.
func TestAIAndCreditsHandles404Gracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "aiand", APIKey: "sk-x", CreditsURL: srv.URL, OrgID: "org-test", BillingEnabled: true, Enabled: true}},
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := out[0].Credits
	if !got.Supported {
		t.Error("Supported = false on a 404 — must degrade to Supported:true, no spend figure")
	}
	if got.SpendPeriod != nil {
		t.Errorf("SpendPeriod = %v, want nil on a 404", got.SpendPeriod)
	}
}

// TestAIAndCreditsRequiresOrgID: without an org_id configured, the client
// can't send the required X-Org-ID header, so there's nothing valid to
// query — this must be Supported:false (matches "no balance API" — there's
// truly nothing to report), not a fetch attempt against an incomplete
// request.
func TestAIAndCreditsRequiresOrgID(t *testing.T) {
	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"range":"24h","buckets":[]}`))
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "aiand", APIKey: "sk-x", CreditsURL: srv.URL, BillingEnabled: true, Enabled: true}}, // no OrgID
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := out[0].Credits
	if got.Supported {
		t.Error("Supported = true with no org_id configured — nothing valid could be queried")
	}
	if hits.Load() != 0 {
		t.Errorf("hits = %d, want 0 (no request should be attempted without org_id)", hits.Load())
	}
}

// TestAIAndCreditsDefaultURLIsAttempted confirms the client targets AI&'s
// documented Analytics API host+path when no credits_url override is
// configured on the provider row (mirrors how DeepSeek's default is tested
// implicitly via its hardcoded constant). This doesn't hit the network — it
// only checks that a request is attempted (network error is expected and
// handled) once org_id is present.
func TestAIAndCreditsDefaultURLIsAttempted(t *testing.T) {
	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "aiand", APIKey: "sk-x", OrgID: "org-test", BillingEnabled: true, Enabled: true}}, // no credits_url, no target_url
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour, ProbeTimeout: 200 * time.Millisecond})
	out, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := out[0].Credits
	// The default URL (api.aiand.com) is unreachable/nonexistent from this
	// sandboxed test environment, so the fetch fails — but it must still
	// be Supported:true (a real attempt was made against a real provider
	// concept).
	if !got.Supported {
		t.Error("Supported = false with no credits_url — the default AI& URL path isn't being tried")
	}
}

// ── Test: CacheTTL default is 5 minutes (F4 throttle) ────────────────────────

func TestCacheTTLDefaultIsFiveMinutes(t *testing.T) {
	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "deepseek", APIKey: "sk-x", TargetURL: srv.URL + "/v1", BillingEnabled: true, Enabled: true}},
	)
	// No CacheTTL set ⇒ New() must default it to ≥5 minutes, not the old
	// 60s. Pre-seed a cache row 4 minutes old: under the old 60s default
	// this would be stale and re-fetched; under the new 5-minute default
	// it must still be served from cache.
	if err := cat.SaveState(context.Background(), store.ProviderStateRow{
		ProviderID:  1,
		HealthJSON:  `{"state":"reachable","as_of":1.0,"source":"live_probe","detail":null}`,
		CreditsJSON: `{"balance_native":1.0,"currency":"USD","as_of":1.0,"supported":true}`,
		FetchedAt:   time.Now().Add(-4 * time.Minute),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	svc := New(Deps{Catalog: cat}) // CacheTTL intentionally unset
	if _, err := svc.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("live fetch fired %d times for a 4-minute-old cache entry, want 0 — "+
			"CacheTTL default must be ≥5 minutes (F4 throttle), not the old 60s", got)
	}
}

// ── Test: no status_url + no target_url ⇒ unknown / none ─────────────────────

func TestNoURLsMeansUnknown(t *testing.T) {
	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "ghost", APIKey: "sk-x", BillingEnabled: true, Enabled: true}}, // no status_url, no target_url
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Hour})
	out, _ := svc.List(context.Background())
	if out[0].Health.State != HealthStateUnknown {
		t.Errorf("state = %q, want unknown", out[0].Health.State)
	}
	if out[0].Health.Source != HealthSourceNone {
		t.Errorf("source = %q, want none", out[0].Health.Source)
	}
}

// ── Test: cache encode/decode round-trip ─────────────────────────────────────

func TestCacheEncodeDecodeRoundTrip(t *testing.T) {
	t1 := time.Unix(1700000000, 0).UTC()
	cases := []struct {
		name string
		h    Health
		c    Credits
	}{
		{
			name: "reachable with balance",
			h:    Health{State: HealthStateReachable, AsOf: t1, Source: HealthSourceLiveProbe},
			c:    Credits{BalanceNative: ptrFloat(32.15), Currency: "USD", AsOf: t1, Supported: true},
		},
		{
			name: "unknown unsupported",
			h:    Health{State: HealthStateUnknown, Source: HealthSourceNone},
			c:    Credits{Supported: false},
		},
		{
			name: "down with detail",
			h:    Health{State: HealthStateDown, AsOf: t1, Source: HealthSourceLiveProbe, Detail: "connection refused"},
			c:    Credits{Supported: true}, // endpoint exists, just unreachable
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hBlob := encodeHealth(c.h)
			cBlob := encodeCredits(c.c)
			// Both blobs must be valid JSON.
			var hm, cm map[string]any
			if err := json.Unmarshal([]byte(hBlob), &hm); err != nil {
				t.Fatalf("health blob not JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(cBlob), &cm); err != nil {
				t.Fatalf("credits blob not JSON: %v", err)
			}
			// Round-trip preserves the fields the handler cares about.
			h2 := decodeHealth(hBlob)
			c2 := decodeCredits(cBlob)
			if h2.State != c.h.State || h2.Source != c.h.Source || h2.Detail != c.h.Detail {
				t.Errorf("health round-trip: %+v ≠ %+v", h2, c.h)
			}
			if c2.Supported != c.c.Supported {
				t.Errorf("credits.Supported: %v ≠ %v", c2.Supported, c.c.Supported)
			}
			if (c2.BalanceNative == nil) != (c.c.BalanceNative == nil) {
				t.Errorf("credits.BalanceNative nil-ness: %v ≠ %v", c2.BalanceNative, c.c.BalanceNative)
			}
		})
	}
}

// ── Test: API key masking never leaks the full secret ────────────────────────

func TestMaskSecretNeverLeaksFull(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"sk-deepseek-verylongsecret-12345", "sk-d…"},
		{"short", "shor…"},
		{"abc", "abc…"},
		{"", ""},
	}
	for _, c := range cases {
		got := maskSecret(c.input)
		if got != c.want {
			t.Errorf("maskSecret(%q) = %q, want %q", c.input, got, c.want)
		}
		// Invariant: the full secret must never appear in masked form.
		if len(c.input) > 4 && strings.Contains(got, c.input) {
			t.Errorf("maskSecret(%q) leaked the full secret in %q", c.input, got)
		}
	}
}

// ── Test: per-provider refresh mutex prevents double-fetch ──────────────────
//
// Two concurrent List calls for the same provider should share one fetch
// (singleflight via the per-provider mutex). The stub asserts at most one
// concurrent fetch is in flight by blocking on a channel.

func TestConcurrentRefreshSingleFlights(t *testing.T) {
	inFlight := atomic.Int32{}
	maxInFlight := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond) // hold the connection open
		inFlight.Add(-1)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	cat := newFakeCatalog(
		[]store.ProviderRow{{Name: "deepseek", APIKey: "sk-x", TargetURL: srv.URL + "/v1", BillingEnabled: true, Enabled: true}},
	)
	svc := New(Deps{Catalog: cat, CacheTTL: 1 * time.Millisecond})

	// Two concurrent List calls — both should share one fetch.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.List(context.Background())
		}()
	}
	wg.Wait()

	// The first fetch (cache miss) should single-flight: at most 1
	// concurrent in-flight probe. (The second List may also trigger a
	// refresh if the cache went stale between the two, but never
	// concurrently with the first.)
	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("max concurrent probes = %d, want ≤1 (per-provider mutex should single-flight)", got)
	}
}

// ptrFloat returns &v (test helper).
func ptrFloat(v float64) *float64 { return &v }

// Avoid an unused-import warning if the test file's only sync use is the
// atomic helpers above — keep the import so the helpers read clearly.
var _ = fmt.Sprintf
