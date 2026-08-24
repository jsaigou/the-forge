// SPDX-License-Identifier: Apache-2.0

package httpapi

// settings_handlers_test.go — tests for billing settings (§0.9), provider
// CRUD (§0.9), and the compressor config provider-key leakage removal (§0.9).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

// fakeCompressor is an in-memory store.Routing for tests. SaveProvider/
// SaveProxy auto-assign an id (mirroring AUTOINCREMENT) when the passed row
// has ID/ID==0, so tests written before the 0042 surrogate-key migration
// that construct a bare store.ProviderRow{Name: ...} still work — only
// updates/deletes/lookups now need a real id, matching the production store.
type fakeCompressor struct {
	mu        sync.Mutex
	providers []store.ProviderRow
	proxies   []store.ProxyRow
	savings   map[string]store.SavingsTotal
	nextProviderID int64
	nextProxyID    int64
}

func (f *fakeCompressor) SaveProvider(_ context.Context, p store.ProviderRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.ID == 0 {
		// Upsert-by-name fallback, mirroring the real store (see
		// routingView.SaveProvider's doc comment): a bare-literal re-save
		// under an existing name updates in place instead of duplicating.
		for i, existing := range f.providers {
			if existing.Name == p.Name && existing.DeletedAt.IsZero() {
				p.ID = existing.ID
				f.providers[i] = p
				return nil
			}
		}
		f.nextProviderID++
		p.ID = f.nextProviderID
		f.providers = append(f.providers, p)
		return nil
	}
	for i, existing := range f.providers {
		if existing.ID == p.ID {
			f.providers[i] = p
			return nil
		}
	}
	f.providers = append(f.providers, p)
	if p.ID > f.nextProviderID {
		f.nextProviderID = p.ID
	}
	return nil
}

func (f *fakeCompressor) DeleteProvider(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.providers {
		if p.ID == id && p.DeletedAt.IsZero() {
			f.providers[i].DeletedAt = time.Now()
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeCompressor) Providers(_ context.Context) ([]store.ProviderRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.ProviderRow{}
	for _, p := range f.providers {
		if p.DeletedAt.IsZero() {
			out = append(out, f.withCompressorProxyName(p))
		}
	}
	return out, nil
}

// withCompressorProxyName populates the read-only join, matching the real
// store's providerBy (compressor.go) — every single-row lookup must reflect
// the current link, since callers like handleProviderUpdate read it right
// after resolving the row to decide whether the link is changing.
func (f *fakeCompressor) withCompressorProxyName(p store.ProviderRow) store.ProviderRow {
	for _, proxy := range f.proxies {
		if proxy.ProviderID != nil && *proxy.ProviderID == p.ID && proxy.OrphanedAt.IsZero() {
			p.CompressorProxyName = proxy.Service
			return p
		}
	}
	return p
}

func (f *fakeCompressor) ProviderByID(_ context.Context, id int64) (store.ProviderRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.providers {
		if p.ID == id {
			return f.withCompressorProxyName(p), true, nil
		}
	}
	return store.ProviderRow{}, false, nil
}

func (f *fakeCompressor) ProviderByName(_ context.Context, name string) (store.ProviderRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.providers {
		if p.Name == name && p.DeletedAt.IsZero() {
			return f.withCompressorProxyName(p), true, nil
		}
	}
	return store.ProviderRow{}, false, nil
}

func (f *fakeCompressor) LinkProxyToProvider(_ context.Context, proxyID int64, providerID *int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.proxies {
		if p.ID == proxyID {
			f.proxies[i].ProviderID = providerID
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeCompressor) SaveProxy(_ context.Context, p store.ProxyRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.ID == 0 {
		for i, existing := range f.proxies {
			if existing.Service == p.Service {
				p.ID = existing.ID
				f.proxies[i] = p
				return nil
			}
		}
		f.nextProxyID++
		p.ID = f.nextProxyID
		f.proxies = append(f.proxies, p)
		return nil
	}
	for i, existing := range f.proxies {
		if existing.ID == p.ID {
			f.proxies[i] = p
			return nil
		}
	}
	f.proxies = append(f.proxies, p)
	if p.ID > f.nextProxyID {
		f.nextProxyID = p.ID
	}
	return nil
}

func (f *fakeCompressor) DeleteProxy(_ context.Context, service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.proxies {
		if p.Service == service {
			f.proxies = append(f.proxies[:i], f.proxies[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeCompressor) Proxies(_ context.Context) ([]store.ProxyRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.ProxyRow, len(f.proxies))
	copy(out, f.proxies)
	// Populate the read-only ProviderName join, same as the real store.
	for i, p := range out {
		if p.ProviderID == nil {
			continue
		}
		for _, pr := range f.providers {
			if pr.ID == *p.ProviderID {
				out[i].ProviderName = pr.Name
				break
			}
		}
	}
	return out, nil
}

func (f *fakeCompressor) RecordSavings(_ context.Context, proxyID int64, _ time.Time, tokensIn, saved int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.savings == nil {
		f.savings = map[string]store.SavingsTotal{}
	}
	key := strconv.FormatInt(proxyID, 10)
	s := f.savings[key]
	s.TokensIn += tokensIn
	s.Saved += saved
	f.savings[key] = s
	return nil
}

func (f *fakeCompressor) Savings(_ context.Context, _ time.Time) (map[string]store.SavingsTotal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]store.SavingsTotal, len(f.savings))
	for k, v := range f.savings {
		out[k] = v
	}
	return out, nil
}

func (f *fakeCompressor) RecordSavingsSample(context.Context, store.CompressorSavingsSampleRow, []store.CompressorLabelSample) error {
	return nil
}

func (f *fakeCompressor) SavingsSummary(context.Context, time.Time) (map[string]store.CompressorProxySummary, error) {
	return map[string]store.CompressorProxySummary{}, nil
}

// newSettingsTestServer builds a Server with fake Compressor + Settings wired.
func newSettingsTestServer(t *testing.T) (*Server, *fakeCompressor, *fakeSettings) {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
	})
	fh := &fakeCompressor{}
	fs := newFakeSettings()
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
		Routing:  fh,
		Settings:  fs,
	})
	t.Cleanup(func() { s.Close() })
	return s, fh, fs
}

// ── Billing settings tests ───────────────────────────────────────────────────

func TestBillingSettingsGetDefaults(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	w := do(t, s, authedRequest("GET", "/api/v1/billing/settings", nil))
	if w.Code != 200 {
		t.Fatalf("GET = %d, want 200", w.Code)
	}
	var resp billingSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.DisplayCurrency != "USD" {
		t.Errorf("display_currency = %q, want USD (default)", resp.DisplayCurrency)
	}
	if resp.FxSourceURL != nil {
		t.Errorf("fx_source_url = %v, want nil", *resp.FxSourceURL)
	}
	if resp.FxRefreshMin != nil {
		t.Errorf("fx_refresh_min = %v, want nil", *resp.FxRefreshMin)
	}
}

func TestBillingSettingsPutPersistsDisplayCurrency(t *testing.T) {
	s, _, fs := newSettingsTestServer(t)
	body := strings.NewReader(`{"display_currency":"CNY"}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/billing/settings", body))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp billingSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.DisplayCurrency != "CNY" {
		t.Errorf("display_currency = %q, want CNY", resp.DisplayCurrency)
	}
	// Verify it persisted to the store.
	raw, err := fs.Get(context.Background(), SettingBillingDisplayCurrency)
	if err != nil {
		t.Fatalf("settings Get: %v", err)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != "CNY" {
		t.Errorf("persisted display_currency = %q, want CNY", got)
	}
	// GET should return the persisted value.
	w2 := do(t, s, authedRequest("GET", "/api/v1/billing/settings", nil))
	var resp2 billingSettingsResponse
	decodeJSON(t, w2.Body, &resp2)
	if resp2.DisplayCurrency != "CNY" {
		t.Errorf("GET display_currency = %q, want CNY", resp2.DisplayCurrency)
	}
}

func TestBillingSettingsPutRejectsInvalidCurrency(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"display_currency":"us"}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/billing/settings", body))
	if w.Code != 422 {
		t.Fatalf("PUT invalid currency = %d, want 422: %s", w.Code, w.Body.String())
	}
}

func TestBillingSettingsPutRejectsMissingCurrency(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/billing/settings", body))
	if w.Code != 422 {
		t.Fatalf("PUT missing currency = %d, want 422", w.Code)
	}
}

func TestBillingSettingsPutFxFields(t *testing.T) {
	s, _, fs := newSettingsTestServer(t)
	body := strings.NewReader(`{"display_currency":"USD","fx_source_url":"https://fx.example.com/rates","fx_refresh_min":30}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/billing/settings", body))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp billingSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.FxSourceURL == nil || *resp.FxSourceURL != "https://fx.example.com/rates" {
		t.Errorf("fx_source_url = %v, want https://fx.example.com/rates", resp.FxSourceURL)
	}
	if resp.FxRefreshMin == nil || *resp.FxRefreshMin != 30 {
		t.Errorf("fx_refresh_min = %v, want 30", resp.FxRefreshMin)
	}
	// Verify persistence.
	raw, err := fs.Get(context.Background(), SettingBillingFxSourceURL)
	if err != nil {
		t.Fatalf("settings Get fx_source_url: %v", err)
	}
	var got string
	json.Unmarshal(raw, &got)
	if got != "https://fx.example.com/rates" {
		t.Errorf("persisted fx_source_url = %q", got)
	}
}

// ── Provider CRUD tests ──────────────────────────────────────────────────────

func TestProviderCreate(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"name":"deepseek","bill_currency":"USD","target_url":"https://api.deepseek.com/v1","status_url":"https://status.deepseek.com","credits_url":"https://api.deepseek.com/user/balance"}`)
	w := do(t, s, authedRequest("POST", "/api/v1/providers", body))
	if w.Code != 201 {
		t.Fatalf("POST = %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp providerJSON
	decodeJSON(t, w.Body, &resp)
	if resp.Name != "deepseek" {
		t.Errorf("name = %q, want deepseek", resp.Name)
	}
	if resp.BillCurrency != "USD" {
		t.Errorf("bill_currency = %q, want USD", resp.BillCurrency)
	}
	if resp.APIKeyMasked != "" {
		t.Errorf("api_key_masked = %q, want empty on create", resp.APIKeyMasked)
	}
	// Verify it persisted.
	providers, _ := fh.Providers(context.Background())
	if len(providers) != 1 || providers[0].Name != "deepseek" {
		t.Errorf("persisted providers = %+v", providers)
	}
	if providers[0].TargetURL != "https://api.deepseek.com/v1" {
		t.Errorf("target_url = %q", providers[0].TargetURL)
	}
	if providers[0].StatusURL != "https://status.deepseek.com" {
		t.Errorf("status_url = %q", providers[0].StatusURL)
	}
	if providers[0].CreditsURL != "https://api.deepseek.com/user/balance" {
		t.Errorf("credits_url = %q", providers[0].CreditsURL)
	}
}

// TestProviderCreateDefaultsBillingEnabled proves a create body that
// doesn't mention billing_enabled at all still ends up enabled — the
// column's DEFAULT 1 only helps when a column is OMITTED from an INSERT,
// but SaveProvider always binds it explicitly, so without this handler-
// level default every new provider would silently start with billing off.
func TestProviderCreateDefaultsBillingEnabled(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"name":"deepseek","bill_currency":"USD"}`)
	w := do(t, s, authedRequest("POST", "/api/v1/providers", body))
	if w.Code != 201 {
		t.Fatalf("POST = %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp providerJSON
	decodeJSON(t, w.Body, &resp)
	if !resp.BillingEnabled {
		t.Errorf("response billing_enabled = false, want true (default)")
	}
	providers, _ := fh.Providers(context.Background())
	if len(providers) != 1 || !providers[0].BillingEnabled {
		t.Errorf("persisted billing_enabled = %+v, want true", providers)
	}
}

// TestProviderCreateExplicitBillingDisabled proves the default can be
// overridden — an explicit false in the create body must stick, not be
// silently forced back to true.
func TestProviderCreateExplicitBillingDisabled(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"name":"deepseek","billing_enabled":false}`)
	w := do(t, s, authedRequest("POST", "/api/v1/providers", body))
	if w.Code != 201 {
		t.Fatalf("POST = %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp providerJSON
	decodeJSON(t, w.Body, &resp)
	if resp.BillingEnabled {
		t.Errorf("response billing_enabled = true, want false (explicit)")
	}
	providers, _ := fh.Providers(context.Background())
	if len(providers) != 1 || providers[0].BillingEnabled {
		t.Errorf("persisted billing_enabled = %+v, want false", providers)
	}
}

// TestProviderUpdateBillingFields covers the two new PUT-editable fields.
func TestProviderUpdateBillingFields(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	do(t, s, authedRequest("POST", "/api/v1/providers", strings.NewReader(`{"name":"deepseek"}`)))

	upd := strings.NewReader(`{"billing_enabled":false,"billing_console_url":"https://platform.deepseek.com/usage"}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek", upd))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp providerJSON
	decodeJSON(t, w.Body, &resp)
	if resp.BillingEnabled {
		t.Errorf("billing_enabled = true, want false after update")
	}
	if resp.BillingConsoleURL != "https://platform.deepseek.com/usage" {
		t.Errorf("billing_console_url = %q", resp.BillingConsoleURL)
	}
	providers, _ := fh.Providers(context.Background())
	if providers[0].BillingEnabled {
		t.Error("persisted billing_enabled should be false")
	}
	if providers[0].BillingConsoleURL != "https://platform.deepseek.com/usage" {
		t.Errorf("persisted billing_console_url = %q", providers[0].BillingConsoleURL)
	}
}

// TestProviderCreateDefaultsEnabled proves a new provider starts ENABLED —
// same explicit-binding trap as billing_enabled: SaveProvider binds the
// column, so a Go zero-value would silently create every provider disabled.
func TestProviderCreateDefaultsEnabled(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/providers", strings.NewReader(`{"name":"qwen","country":"SG","data_residency_group":"Southeast Asia"}`)))
	if w.Code != 201 {
		t.Fatalf("POST = %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp providerJSON
	decodeJSON(t, w.Body, &resp)
	if !resp.Enabled {
		t.Errorf("response enabled = false, want true (default)")
	}
	if resp.Country != "SG" || resp.DataResidencyGroup != "Southeast Asia" {
		t.Errorf("residency = %q/%q, want SG/Southeast Asia", resp.Country, resp.DataResidencyGroup)
	}
	providers, _ := fh.Providers(context.Background())
	if len(providers) != 1 || !providers[0].Enabled {
		t.Errorf("persisted enabled = %+v, want true", providers)
	}
	if providers[0].Country != "SG" || providers[0].DataResidencyGroup != "Southeast Asia" {
		t.Errorf("persisted residency = %q/%q", providers[0].Country, providers[0].DataResidencyGroup)
	}
}

// TestProviderUpdateEnabled covers the disable-without-delete flow: PUT
// enabled:false must persist (the router reads the same row), and fields
// not named in the body — including enabled, when omitted — are preserved.
func TestProviderUpdateEnabled(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	do(t, s, authedRequest("POST", "/api/v1/providers", strings.NewReader(`{"name":"qwen"}`)))

	w := do(t, s, authedRequest("PUT", "/api/v1/providers/qwen", strings.NewReader(`{"enabled":false}`)))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp providerJSON
	decodeJSON(t, w.Body, &resp)
	if resp.Enabled {
		t.Errorf("enabled = true after disable, want false")
	}
	providers, _ := fh.Providers(context.Background())
	if providers[0].Enabled {
		t.Error("persisted enabled should be false")
	}

	// Re-enable + set residency; enabled stays whatever the body says.
	w = do(t, s, authedRequest("PUT", "/api/v1/providers/qwen",
		strings.NewReader(`{"enabled":true,"country":"SG","data_residency_group":"Southeast Asia"}`)))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w.Body, &resp)
	if !resp.Enabled || resp.Country != "SG" || resp.DataResidencyGroup != "Southeast Asia" {
		t.Errorf("after re-enable = enabled:%v country:%q group:%q", resp.Enabled, resp.Country, resp.DataResidencyGroup)
	}

	// An update that omits enabled preserves the current state.
	w = do(t, s, authedRequest("PUT", "/api/v1/providers/qwen", strings.NewReader(`{"bill_currency":"USD"}`)))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w.Body, &resp)
	if !resp.Enabled {
		t.Error("enabled flipped to false by an update that never named it")
	}
}

// TestProviderDiscoverBilling covers POST .../discover-billing: a hit gets
// saved to credits_url when none was set; a provider that already has a
// credits_url must never have it overwritten, even if discovery finds a
// different candidate.
func TestProviderDiscoverBilling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/credits" {
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"9.99"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s, fh, _ := newSettingsTestServer(t)
	create := strings.NewReader(`{"name":"newprov","target_url":"` + srv.URL + `/v1"}`)
	if w := do(t, s, authedRequest("POST", "/api/v1/providers", create)); w.Code != 201 {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}

	w := do(t, s, authedRequest("POST", "/api/v1/providers/newprov/discover-billing", nil))
	if w.Code != 200 {
		t.Fatalf("discover-billing = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	if resp["found"] != true || resp["saved"] != true {
		t.Fatalf("expected found+saved, got %+v", resp)
	}
	providers, _ := fh.Providers(context.Background())
	if providers[0].CreditsURL != srv.URL+"/v1/credits" {
		t.Errorf("persisted credits_url = %q, want the discovered candidate", providers[0].CreditsURL)
	}

	// Re-running discovery must NOT overwrite the now-set credits_url, even
	// though it would find the same (or a different) candidate again.
	w2 := do(t, s, authedRequest("POST", "/api/v1/providers/newprov/discover-billing", nil))
	var resp2 map[string]any
	decodeJSON(t, w2.Body, &resp2)
	if resp2["saved"] != false {
		t.Errorf("expected saved=false on a provider that already has credits_url, got %+v", resp2)
	}
	providers2, _ := fh.Providers(context.Background())
	if providers2[0].CreditsURL != srv.URL+"/v1/credits" {
		t.Errorf("credits_url changed on re-discovery: %q", providers2[0].CreditsURL)
	}
}

func TestProviderCreateDuplicate(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"name":"deepseek","bill_currency":"USD"}`)
	w := do(t, s, authedRequest("POST", "/api/v1/providers", body))
	if w.Code != 201 {
		t.Fatalf("first POST = %d, want 201", w.Code)
	}
	body2 := strings.NewReader(`{"name":"deepseek","bill_currency":"USD"}`)
	w2 := do(t, s, authedRequest("POST", "/api/v1/providers", body2))
	if w2.Code != 409 {
		t.Errorf("duplicate POST = %d, want 409", w2.Code)
	}
}

func TestProviderCreateInvalidName(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"name":"Bad@Name!","bill_currency":"USD"}`)
	w := do(t, s, authedRequest("POST", "/api/v1/providers", body))
	if w.Code != 422 {
		t.Errorf("invalid name POST = %d, want 422", w.Code)
	}
}

func TestProviderUpdate(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	// Create first.
	body := strings.NewReader(`{"name":"deepseek","bill_currency":"USD","target_url":"https://old.example.com"}`)
	do(t, s, authedRequest("POST", "/api/v1/providers", body))
	// Update non-secret fields.
	upd := strings.NewReader(`{"target_url":"https://new.example.com","bill_currency":"CNY"}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek", upd))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp providerJSON
	decodeJSON(t, w.Body, &resp)
	if resp.BillCurrency != "CNY" {
		t.Errorf("bill_currency = %q, want CNY", resp.BillCurrency)
	}
	// Verify persistence (providerJSON frozen shape has no target_url; check store).
	providers, _ := fh.Providers(context.Background())
	if providers[0].TargetURL != "https://new.example.com" {
		t.Errorf("persisted target_url = %q", providers[0].TargetURL)
	}
	if providers[0].BillCurrency != "CNY" {
		t.Errorf("persisted bill_currency = %q", providers[0].BillCurrency)
	}
}

func TestProviderUpdateNotFound(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"target_url":"https://new.example.com"}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/providers/nonexistent", body))
	if w.Code != 404 {
		t.Errorf("PUT nonexistent = %d, want 404", w.Code)
	}
}

func TestProviderKeyWriteOnly(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	// Create a provider first.
	body := strings.NewReader(`{"name":"deepseek","bill_currency":"USD"}`)
	do(t, s, authedRequest("POST", "/api/v1/providers", body))
	// Set the API key.
	keyBody := strings.NewReader(`{"api_key":"sk-deepseek-very-long-secret-value"}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek/key", keyBody))
	if w.Code != 200 {
		t.Fatalf("PUT key = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	decodeJSON(t, w.Body, &resp)
	// The response must contain the masked key, never the full secret.
	masked := resp["api_key_masked"]
	if masked == "" {
		t.Error("api_key_masked is empty")
	}
	if strings.Contains(masked, "very-long-secret-value") {
		t.Errorf("masked key leaked the full secret: %q", masked)
	}
	// Verify the full secret persisted.
	providers, _ := fh.Providers(context.Background())
	if providers[0].APIKey != "sk-deepseek-very-long-secret-value" {
		t.Errorf("persisted api_key = %q, want the full secret", providers[0].APIKey)
	}
}

func TestProviderKeyNotFound(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"api_key":"sk-test"}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/providers/nonexistent/key", body))
	if w.Code != 404 {
		t.Errorf("PUT key nonexistent = %d, want 404", w.Code)
	}
}

func TestProviderKeyEmptyKey(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	// Create a provider first.
	body := strings.NewReader(`{"name":"deepseek","bill_currency":"USD"}`)
	do(t, s, authedRequest("POST", "/api/v1/providers", body))
	// Try to set an empty key.
	keyBody := strings.NewReader(`{"api_key":""}`)
	w := do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek/key", keyBody))
	if w.Code != 422 {
		t.Errorf("PUT empty key = %d, want 422", w.Code)
	}
}

func TestProviderDelete(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	body := strings.NewReader(`{"name":"deepseek","bill_currency":"USD"}`)
	do(t, s, authedRequest("POST", "/api/v1/providers", body))
	w := do(t, s, authedRequest("DELETE", "/api/v1/providers/deepseek", nil))
	if w.Code != 200 {
		t.Fatalf("DELETE = %d, want 200", w.Code)
	}
	providers, _ := fh.Providers(context.Background())
	if len(providers) != 0 {
		t.Errorf("providers after delete = %d, want 0", len(providers))
	}
}

func TestProviderDeleteNotFound(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	w := do(t, s, authedRequest("DELETE", "/api/v1/providers/nonexistent", nil))
	if w.Code != 404 {
		t.Errorf("DELETE nonexistent = %d, want 404", w.Code)
	}
}

// ── Compressor config: provider keys not leaked ────────────────────────────────

func TestCompressorConfigDoesNotReturnAPIKey(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	// Seed a provider with a real secret.
	_ = fh.SaveProvider(context.Background(), store.ProviderRow{
		Name:   "deepseek",
		APIKey: "sk-deepseek-super-secret-key",
	})
	w := do(t, s, authedRequest("GET", "/api/v1/compressor/config", nil))
	if w.Code != 200 {
		t.Fatalf("GET config = %d, want 200", w.Code)
	}
	var resp compressorConfigResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(resp.Providers))
	}
	p := resp.Providers[0]
	if p.APIKey != "" {
		t.Errorf("api_key = %q, want empty (keys managed via Settings routes)", p.APIKey)
	}
	if p.Name != "deepseek" {
		t.Errorf("name = %q, want deepseek", p.Name)
	}
}

// TestCompressorPassthroughProxyPersistsToStoreRow guards the 2026-07-29 fix:
// PUT .../passthrough with scope="proxy" used to write only to a
// compressor.passthrough_services settings-KV list that router.compressorBypassed
// (routing.go) never read — the real enforcement path reads
// store.ProxyRow.Passthrough exclusively. A human toggling "bypass this
// proxy" in Settings saw it flip to "on" (handleCompressorConfig's read side
// consulted the same disconnected list) while every real request kept
// routing straight through the proxy, unbypassed. This test would have
// failed against the old settings-list-only implementation.
func TestCompressorPassthroughProxyPersistsToStoreRow(t *testing.T) {
	s, fh, _ := newSettingsTestServer(t)
	_ = fh.SaveProxy(context.Background(), store.ProxyRow{
		Service: "deepseek", Label: "DeepSeek", Port: 8790, Unit: "headroom@deepseek",
	})

	w := do(t, s, authedRequest("PUT", "/api/v1/compressor/passthrough",
		strings.NewReader(`{"scope":"proxy","service":"deepseek","enabled":true}`)))
	if w.Code != 200 {
		t.Fatalf("PUT passthrough = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	// The fix: the store row itself — what routing.go's compressorBypassed
	// actually reads — must be updated, not just a settings-KV side list.
	proxies, err := fh.Proxies(context.Background())
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}
	if len(proxies) != 1 || !proxies[0].Passthrough {
		t.Fatalf("store row Passthrough = %+v, want Passthrough=true on the deepseek row", proxies)
	}

	// The read side (GET config, what the FE displays) must agree with the
	// same store row rather than a separately-maintained list.
	w = do(t, s, authedRequest("GET", "/api/v1/compressor/config", nil))
	var resp compressorConfigResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Proxies) != 1 || !resp.Proxies[0].Passthrough {
		t.Fatalf("GET config proxies = %+v, want deepseek passthrough=true", resp.Proxies)
	}

	// Toggling back off must also hit the store row.
	w = do(t, s, authedRequest("PUT", "/api/v1/compressor/passthrough",
		strings.NewReader(`{"scope":"proxy","service":"deepseek","enabled":false}`)))
	if w.Code != 200 {
		t.Fatalf("PUT passthrough off = %d, want 200", w.Code)
	}
	proxies, _ = fh.Proxies(context.Background())
	if len(proxies) != 1 || proxies[0].Passthrough {
		t.Fatalf("store row Passthrough after disabling = %+v, want false", proxies)
	}
}

// TestCompressorPassthroughProxyUnknownService confirms scope="proxy" for a
// service with no matching proxy row returns 404 rather than silently
// succeeding (the old settings-list implementation never checked whether
// the service actually existed).
func TestCompressorPassthroughProxyUnknownService(t *testing.T) {
	s, _, _ := newSettingsTestServer(t)
	w := do(t, s, authedRequest("PUT", "/api/v1/compressor/passthrough",
		strings.NewReader(`{"scope":"proxy","service":"nonexistent","enabled":true}`)))
	if w.Code != 404 {
		t.Fatalf("PUT passthrough for unknown service = %d, want 404", w.Code)
	}
}
