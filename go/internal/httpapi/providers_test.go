// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/providers"
	"github.com/jsaigou/the-forge/internal/sched"
)

// fakeProvidersService is a stub providers.Service for the httpapi test.
// Returns a fixed list so the handler test asserts the frozen wire shape
// without depending on the providers package's live-fetch behavior (that's
// covered by internal/providers's own tests).
type fakeProvidersService struct {
	out []providers.Provider
	err error
}

func (f fakeProvidersService) List(ctx context.Context) ([]providers.Provider, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.out == nil {
		return []providers.Provider{}, nil
	}
	return f.out, nil
}

// serverWithProviders builds a Server with a custom providers.Service wired
// in (so the test doesn't depend on the live HTTP clients).
func serverWithProviders(t *testing.T, svc providers.Service) *Server {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
		Modes: map[string]config.Mode{
			"qwen3": {Label: "Qwen3", Default: true},
		},
	})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Providers: svc,
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })
	return s
}

func floatPtr(v float64) *float64 { return &v }
func strPtr(v string) *string     { return &v }
func boolPtr(v bool) *bool        { return &v }

// TestProvidersListEmptyWhenUnwired covers the nil-deps path: when
// forge hasn't wired providers.Service, GET /api/v1/providers still
// returns 200 with {"providers":[]} (frozen shape — array, not null).
func TestProvidersListEmptyWhenUnwired(t *testing.T) {
	s := newTestServer(t) // no Providers wired
	w := do(t, s, authedRequest("GET", "/api/v1/providers", nil))
	if w.Code != 200 {
		t.Fatalf("providers = %d, want 200", w.Code)
	}
	var resp providersResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Providers == nil {
		t.Fatal("providers = nil, want empty slice (frozen shape requires an array)")
	}
	if len(resp.Providers) != 0 {
		t.Errorf("providers = %d, want 0", len(resp.Providers))
	}
}

// TestProvidersListFrozenShape asserts the frozen §0.3 wire shape:
// - Each provider has models[], health, credits
// - api_key_masked is the prefix+ellipsis form (never the full secret)
// - "idle" never appears in health.state (vocabulary: reachable|degraded|down|unknown)
// - supported:false when a provider has no balance API (mirrors AI&)
func TestProvidersListFrozenShape(t *testing.T) {
	bal := 32.15
	svc := fakeProvidersService{
		out: []providers.Provider{
			{
				Name:         "deepseek",
				APIKeyMasked: "sk-d…",
				BillCurrency: "USD",
				Health: providers.Health{
					State:  providers.HealthStateReachable,
					Source: providers.HealthSourceLiveProbe,
				},
				Credits: providers.Credits{
					BalanceNative: &bal,
					Currency:      "USD",
					Supported:     true,
				},
				Models: []providers.Model{
					{ModelID: "deepseek-chat", CatalogModelID: 101, DisplayName: "DeepSeek Chat",
						PriceInPer1M: 0.27, PriceOutPer1M: 1.10, Currency: "USD", Priority: 100, Enabled: true,
						CompressorProxy: "deepseek", Passthrough: false},
					{ModelID: "deepseek-reasoner", CatalogModelID: 102, DisplayName: "DeepSeek Reasoner",
						PriceInPer1M: 0.55, PriceOutPer1M: 2.19, Currency: "USD", Priority: 100, Enabled: true},
				},
			},
			{
				Name:         "aiand",
				APIKeyMasked: "sk-a…",
				BillCurrency: "USD",
				Health: providers.Health{
					State:  providers.HealthStateReachable,
					Source: providers.HealthSourceLiveProbe,
				},
				Credits: providers.Credits{Supported: false},
				Models: []providers.Model{
					{ModelID: "moonshotai/kimi-k2.7-code", CatalogModelID: 103, DisplayName: "Kimi K2.7 Code",
						PriceInPer1M: 0.60, PriceOutPer1M: 2.50, Currency: "USD", Priority: 100, Enabled: true,
						CompressorProxy: "external", Passthrough: true},
				},
			},
		},
	}
	s := serverWithProviders(t, svc)
	w := do(t, s, authedRequest("GET", "/api/v1/providers", nil))
	if w.Code != 200 {
		t.Fatalf("providers = %d, want 200 (body=%s)", w.Code, w.Body)
	}

	// ── Assert the frozen shape end-to-end against the wire payload ──
	var raw struct {
		Providers []struct {
			Name         string `json:"name"`
			APIKeyMasked string `json:"api_key_masked"`
			BillCurrency string `json:"bill_currency"`
			Health       struct {
				State  string   `json:"state"`
				AsOf   *float64 `json:"as_of"`
				Source string   `json:"source"`
				Detail *string  `json:"detail"`
			} `json:"health"`
			Credits struct {
				BalanceNative *float64 `json:"balance_native"`
				Currency      *string  `json:"currency"`
				AsOf          *float64 `json:"as_of"`
				Supported     bool     `json:"supported"`
			} `json:"credits"`
			Models []struct {
				ModelID        string  `json:"model_id"`
				CatalogModelID int64   `json:"catalog_model_id"`
				DisplayName    string  `json:"display_name"`
				Logo           string  `json:"logo"`
				PriceInPer1M   float64 `json:"price_in_per_1m"`
				PriceOutPer1M  float64 `json:"price_out_per_1m"`
				Currency       string  `json:"currency"`
				Priority       int     `json:"priority"`
				Enabled        bool    `json:"enabled"`
				CompressorProxy  *string `json:"compressor_proxy"`
				Passthrough    *bool   `json:"passthrough"`
			} `json:"models"`
		} `json:"providers"`
	}
	decodeJSON(t, w.Body, &raw)

	if len(raw.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(raw.Providers))
	}
	deepseek := raw.Providers[0]
	aiand := raw.Providers[1]

	// API key masked (Contract 1 §18 — never the full secret).
	if deepseek.APIKeyMasked != "sk-d…" {
		t.Errorf("deepseek api_key_masked = %q, want sk-d…", deepseek.APIKeyMasked)
	}
	if strings.Contains(deepseek.APIKeyMasked, "secret") {
		t.Errorf("deepseek api_key_masked leaked the secret: %q", deepseek.APIKeyMasked)
	}

	// "idle" must never appear in health.state.
	for _, p := range raw.Providers {
		if p.Health.State == "idle" {
			t.Errorf("%s: health.state is idle — must never appear (frozen vocabulary)", p.Name)
		}
	}
	// Frozen vocabulary enforcement.
	switch deepseek.Health.State {
	case "reachable", "degraded", "down", "unknown":
	default:
		t.Errorf("deepseek health.state = %q, must be in frozen vocabulary", deepseek.Health.State)
	}

	// Credits.supported false ⇒ nil balance/currency for AI&.
	if !deepseek.Credits.Supported {
		t.Error("deepseek: credits.supported = false, want true")
	}
	if deepseek.Credits.BalanceNative == nil || *deepseek.Credits.BalanceNative != 32.15 {
		t.Errorf("deepseek: balance_native = %v, want 32.15", deepseek.Credits.BalanceNative)
	}
	if deepseek.Credits.Currency == nil || *deepseek.Credits.Currency != "USD" {
		t.Errorf("deepseek: currency = %v, want USD", deepseek.Credits.Currency)
	}
	if aiand.Credits.Supported {
		t.Error("aiand: credits.supported = true, want false (no balance API)")
	}
	if aiand.Credits.BalanceNative != nil {
		t.Errorf("aiand: balance_native = %v, want nil (unsupported)", aiand.Credits.BalanceNative)
	}
	if aiand.Credits.Currency != nil {
		t.Errorf("aiand: currency = %v, want nil (unsupported)", aiand.Credits.Currency)
	}

	// Models: grouped under the right provider, in catalog order.
	if len(deepseek.Models) != 2 {
		t.Errorf("deepseek: models = %d, want 2", len(deepseek.Models))
	}
	if deepseek.Models[0].ModelID != "deepseek-chat" {
		t.Errorf("deepseek models[0].model_id = %q, want deepseek-chat", deepseek.Models[0].ModelID)
	}
	if len(aiand.Models) != 1 {
		t.Fatalf("aiand: models = %d, want 1", len(aiand.Models))
	}
	if aiand.Models[0].ModelID != "moonshotai/kimi-k2.7-code" {
		t.Errorf("aiand models[0].model_id = %q, want moonshotai/kimi-k2.7-code", aiand.Models[0].ModelID)
	}

	// compressor_proxy + passthrough: linked proxy → both present; unlinked → null.
	if deepseek.Models[0].CompressorProxy == nil || *deepseek.Models[0].CompressorProxy != "deepseek" {
		t.Errorf("deepseek models[0].compressor_proxy = %v, want \"deepseek\"", deepseek.Models[0].CompressorProxy)
	}
	if deepseek.Models[0].Passthrough == nil {
		t.Error("deepseek models[0].passthrough = nil, want false (linked proxy present)")
	}
	if deepseek.Models[1].CompressorProxy != nil {
		t.Errorf("deepseek models[1].compressor_proxy = %v, want nil (no proxy linked)", deepseek.Models[1].CompressorProxy)
	}
	if deepseek.Models[1].Passthrough != nil {
		t.Errorf("deepseek models[1].passthrough = %v, want nil (no proxy linked)", deepseek.Models[1].Passthrough)
	}
}

// TestProvidersListOperatorRoleEnforced covers the §0.3 RBAC requirement
// (operator role minimum). viewer → 403.
func TestProvidersListOperatorRoleEnforced(t *testing.T) {
	viewer := authz.Identity{Name: "viewer", Role: authz.RoleViewer}
	s := serverWithProviders(t, fakeProvidersService{})
	w := do(t, s, authedRequest("GET", "/api/v1/providers", nil))
	_ = viewer // server uses stubAuth wired in serverWithProviders (admin)
	if w.Code != 200 {
		t.Fatalf("admin GET providers = %d, want 200", w.Code)
	}
	// Now a viewer.
	sViewer := newTestServerWith(t, viewer) // no Providers wired; just RBAC
	w = do(t, sViewer, authedRequest("GET", "/api/v1/providers", nil))
	if w.Code != 403 {
		t.Errorf("viewer GET providers = %d, want 403", w.Code)
	}
}

// TestProvidersListCatalogErrorReturns500 covers the catalog-read-failure
// path: a DB error must surface as 500, distinct from "no providers" (an
// empty list with 200). Per-provider fetch failures never reach this path
// — they degrade to unknown/supported:false inside providers.Service.
func TestProvidersListCatalogErrorReturns500(t *testing.T) {
	svc := fakeProvidersService{err: errCatalogRead}
	s := serverWithProviders(t, svc)
	w := do(t, s, authedRequest("GET", "/api/v1/providers", nil))
	if w.Code != 500 {
		t.Errorf("catalog-error providers = %d, want 500", w.Code)
	}
}

// errCatalogRead is a sentinel for the catalog-read-failure test.
var errCatalogRead = &catalogErr{}

type catalogErr struct{}

func (e *catalogErr) Error() string { return "catalog read failed" }

// TestProvidersListIsJSON ensures the response is well-formed JSON (the
// FE's TanStack Query parse step would crash otherwise).
func TestProvidersListIsJSON(t *testing.T) {
	s := serverWithProviders(t, fakeProvidersService{})
	w := do(t, s, authedRequest("GET", "/api/v1/providers", nil))
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var v any
	if err := json.NewDecoder(w.Body).Decode(&v); err != nil {
		t.Errorf("response body not JSON: %v", err)
	}
}

// Ensure unused helpers don't trip staticcheck (U1000) — they're here so
// future tests have them ready without re-declaring.
var _ = floatPtr
var _ = strPtr
var _ = boolPtr
var _ = (*http.Server)(nil)
var _ = httptest.NewServer
