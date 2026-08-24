// SPDX-License-Identifier: Apache-2.0

// Package providers implements the per-provider clients + read-side assembly
// for the GET /api/v1/providers endpoint (Sprint 0 §0.3, BE-3).
//
// The taxonomy fix lives here: one provider has many models. The catalog
// (router_providers + provider_models) is read from store.Providers; the
// health + credits clients fetch live data and cache it in the
// provider_state table (TTL ≥60s per §0.3, stale-serve on fetch failure).
// The handler (httpapi/providers_handlers.go) consumes the assembled
// []Provider and marshals it to the frozen providersResponse wire shape;
// "idle" is never present — the health state vocabulary is
// reachable | degraded | down | unknown.
//
// Live-verified against ForgeHost's real DeepSeek + AI& provider config on
// 2026-07-22 (balance API shape + /v1/models probe). DeepSeek exposes a
// balance endpoint (supported:true). AI& was originally probed with
// OpenAI-shaped balance paths (user/balance, v1/credits, etc.) which all
// 404 — the post-deploy review (F4, docs/v5-review-fixes.md) found the
// real surface is AI&'s Analytics API (docs.aiand.com/billing/usage/,
// GET /analytics/summary); see credits.go for the fetch + the caveats on
// how confidently the exact response shape is known.
package providers

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Service is the read surface for GET /api/v1/providers. Implementations
// read the catalog, refresh+serve cached health/credits, mask the API key,
// and return one Provider per configured row. nil deps (Phase 4 stub
// environment) is a programming error — the httpapi handler nil-checks
// deps.Providers and returns an empty list when unwired.
type Service interface {
	// List assembles the full providers response payload. The caller
	// (httpapi handler) wraps it in providersResponse{} and marshals.
	// Models are grouped under their provider in catalog order; the
	// slice is never nil (callers can range over it without a nil-check).
	List(ctx context.Context) ([]Provider, error)
}

// Provider is one row of the providers response. APIKeyMasked is the
// prefix+ellipsis form (Contract 1 §18: "must not leak proxy tokens or
// full API keys"); the full secret stays inside this package for the
// health/credits clients to use and never crosses the wire.
type Provider struct {
	// ID is the surrogate PK (Phase 6 surrogate-key migration, 0042).
	ID           int64
	Name         string
	APIKeyMasked string
	BillCurrency string
	Health       Health
	Credits      Credits
	Models       []Model

	// BillingEnabled/BillingConsoleURL (product/QA sprint, 2026-07-29):
	// see store.ProviderRow's doc comments. CreditsURL is included too so
	// the FE can show/edit the machine balance-API endpoint distinct from
	// BillingConsoleURL's human dashboard link.
	BillingEnabled    bool
	BillingConsoleURL string
	CreditsURL        string

	// TargetURL/StatusURL/OrgID: surfaced on the GET path so the Edit form
	// can pre-fill them. Unlike api_key (write-only, masked), these are not
	// secrets — the preset table publishes them. Their omission from the
	// frozen GET shape was an oversight that left every Edit field blank.
	TargetURL string
	StatusURL string
	OrgID     string

	// Enabled (multi-provider routing sprint, 2026-08-06): false = disabled
	// without deletion. The router skips its offerings; List serves the last
	// cached health/credits without refreshing (no probes leave the daemon
	// for a provider the operator switched off).
	Enabled bool
	// Country/DataResidencyGroup: the provider-level residency fact surfaced
	// from router_providers (0008 columns). "" when unknown.
	Country            string
	DataResidencyGroup string
}

// Model is one row under a provider — Phase 7 (2026-08-13): sourced from
// the real, populated `offerings` table (via Offerings.ListOfferings)
// instead of the always-empty provider_models catalog this used to read
// (0 rows on the live deployment, no production write path ever existed —
// see store/providers.go's doc comment). ModelID is the offering's
// wire_model, the same provider-side model identifier this field always
// represented. DisplayName/Logo come from the catalog Model the offering
// routes to. CompressorProxy is the *provider's* linked proxy (offerings have
// no per-model proxy concept) — "" when none linked. Passthrough was never
// populated even before this change (provider_models had no rows to read),
// kept false here for wire-shape continuity, not a regression.
type Model struct {
	ModelID        string
	CatalogModelID int64
	DisplayName    string
	Logo           string
	PriceInPer1M   float64
	PriceOutPer1M  float64
	Currency       string
	Priority       int
	Enabled        bool
	CompressorProxy  string
	Passthrough    bool
}

// Health is the provider status. State is the frozen §0.3 vocabulary
// (reachable | degraded | down | unknown). Source is which client
// produced it (status_page | live_probe | none). AsOf is the probe time
// (zero = never probed → handler emits null). Detail is a short status
// line (e.g. incident title from a status page, or "" = no detail).
type Health struct {
	State  string
	AsOf   time.Time
	Source string
	Detail string
}

// Credits is the provider balance. Supported=false ⇒ the provider API has
// no balance endpoint (AI& today); the other fields are zero. When
// Supported=true, BalanceNative+Currency are populated from the live API
// response and AsOf is the fetch time. Currency is the provider's native
// billing currency (e.g. "USD" for DeepSeek) — display-currency conversion
// happens in the usage layer (§0.2), not here.
type Credits struct {
	BalanceNative *float64
	Currency      string
	AsOf          time.Time
	Supported     bool

	// SpendPeriod/SpendPeriodLabel (BE-3 F4 fix): for providers with no
	// queryable balance API but a real usage/cost analytics API — AI&,
	// confirmed against the real docs (docs.aiand.com/billing/credits/):
	// credits are purchased via a Stripe web checkout only, no endpoint
	// ever returns a remaining balance. GET /analytics/metrics does return
	// real period cost_usd, which is what SpendPeriod carries.
	// SpendPeriodLabel is human text ("24h spend"); both zero-value when
	// BalanceNative is the relevant figure instead (e.g. DeepSeek).
	SpendPeriod      *float64
	SpendPeriodLabel string
}

// OfferingsSource is the minimal read surface Service.List needs to build
// the provided-models list from real routing data: store.Catalog is much
// larger than this, so the dependency is narrowed to just what's used
// (db.Catalog() satisfies this with no wrapper — Go interface matching is
// structural). Nil is valid (skeleton/test mode) — every provider's Models
// is then just empty, same as the always-empty provider_models catalog
// this replaced.
type OfferingsSource interface {
	ListOfferings(ctx context.Context) ([]store.Offering, error)
	GetModel(ctx context.Context, id int64) (store.Model, error)
}

// Deps wires the catalog + cache store, the HTTP client used by the
// health/credits fetchers, and tunable knobs. The zero-value Deps is not
// usable; construct via New.
type Deps struct {
	// Catalog is the read+write surface for router_providers +
	// provider_state (store.Providers). Required.
	Catalog store.Providers

	// Offerings is the real provider→model data (store.Catalog, narrowed —
	// see OfferingsSource). Nil ⇒ every Provider.Models is empty.
	Offerings OfferingsSource

	// HTTPClient is used for status-page, live-probe, and balance-API
	// fetches. Nil → a client with sensible timeouts is constructed
	// (ProbeTimeout for live probes; see client.go).
	HTTPClient httpClient

	// CacheTTL is the provider_state cache freshness window. §0.3 froze
	// the original minimum at 60s; the post-deploy review (F4,
	// docs/v5-review-fixes.md — provider credit polling was hitting
	// upstream balance/analytics APIs too often) raised the *default* to
	// 5 minutes. Zero → 5 minutes. Callers (tests, the live-verify tool)
	// may still set a shorter or longer value explicitly; this only
	// changes what an unset Deps.CacheTTL means. A fetch is attempted
	// when the cache age exceeds this; on failure the stale cache (if
	// any) is served.
	CacheTTL time.Duration

	// ProbeTimeout caps each live-probe + balance-API fetch. Zero → 10s.
	// A fetch that exceeds this is abandoned and the stale cache (or
	// "unknown" / "supported:false") is served.
	ProbeTimeout time.Duration

	// now returns the current time. Tests inject a fixed clock; production
	// leaves it nil → time.Now.
	now func() time.Time
}

// New builds a Service over the catalog+cache store. The returned Service
// is safe for concurrent use (the cache refresh takes a per-provider mutex
// so two concurrent List calls for the same provider don't double-fetch).
func New(deps Deps) Service {
	if deps.CacheTTL <= 0 {
		deps.CacheTTL = 5 * time.Minute
	}
	if deps.ProbeTimeout <= 0 {
		deps.ProbeTimeout = 10 * time.Second
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = newDefaultHTTPClient(deps.ProbeTimeout)
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	return &service{
		deps:    deps,
		health:  newHealthClients(deps.HTTPClient, deps.ProbeTimeout),
		credits: newCreditsClients(deps.HTTPClient, deps.ProbeTimeout),
	}
}

// service implements Service. The per-provider refreshMu ensures only one
// goroutine fetches health+credits for a given provider at a time — the
// cache row is the singleflight rendezvous point.
type service struct {
	deps    Deps
	health  *healthClients
	credits *creditsClients

	// refreshMu keyed by provider name; the inner mutex guards one
	// provider's refresh so concurrent List calls share a single fetch.
	mu        sync.Mutex
	refreshMu map[string]*sync.Mutex
}

// perProviderMu returns the refresh mutex for one provider, creating it
// on first use. Caller must hold s.mu only to look up the entry; the
// returned *sync.Mutex is then locked/released without holding s.mu.
func (s *service) perProviderMu(name string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshMu == nil {
		s.refreshMu = map[string]*sync.Mutex{}
	}
	m, ok := s.refreshMu[name]
	if !ok {
		m = &sync.Mutex{}
		s.refreshMu[name] = m
	}
	return m
}

// offeringsByProvider builds the provided-models list from real offerings
// (Phase 7), grouped by provider name and sorted by (priority, wire_model)
// for a stable, priority-first display. A read failure or unwired
// Deps.Offerings degrades to no models for anyone — the health/credits
// half of the response still renders, matching how a per-provider fetch
// failure never fails the whole list.
func (s *service) offeringsByProvider(ctx context.Context, providers []store.ProviderRow) map[string][]Model {
	out := map[string][]Model{}
	if s.deps.Offerings == nil {
		return out
	}
	offerings, err := s.deps.Offerings.ListOfferings(ctx)
	if err != nil {
		return out
	}
	sort.SliceStable(offerings, func(i, j int) bool {
		if offerings[i].Priority != offerings[j].Priority {
			return offerings[i].Priority < offerings[j].Priority
		}
		return offerings[i].WireModel < offerings[j].WireModel
	})
	compressorByProvider := map[string]string{}
	for _, p := range providers {
		compressorByProvider[p.Name] = p.CompressorProxyName
	}
	modelCache := map[int64]store.Model{}
	for _, o := range offerings {
		cm, ok := modelCache[o.ModelID]
		if !ok {
			cm, _ = s.deps.Offerings.GetModel(ctx, o.ModelID) // zero-value on error — display degrades, doesn't fail
			modelCache[o.ModelID] = cm
		}
		out[o.ProviderName] = append(out[o.ProviderName], Model{
			ModelID:        o.WireModel,
			CatalogModelID: o.ModelID,
			DisplayName:    cm.Name,
			Logo:           cm.Logo,
			PriceInPer1M:   o.PriceInPer1M,
			PriceOutPer1M:  o.PriceOutPer1M,
			Currency:       o.Currency,
			Priority:       o.Priority,
			Enabled:        o.Enabled,
			CompressorProxy: compressorByProvider[o.ProviderName],
		})
	}
	return out
}

// List implements Service. The flow per provider:
//  1. Read the cached (health, credits) from provider_state.
//  2. If fresh (age < CacheTTL) → use as-is.
//  3. If stale or missing → fetch fresh health+credits under the
//     per-provider refreshMu (so concurrent List calls share one fetch).
//     On fetch failure: serve stale cache if present, else "unknown" /
//     supported:false. Persist the fresh (or fallback) row.
//  4. Mask the API key, attach models, return.
//
// Catalog read failures are surfaced as errors (the handler maps to 500);
// per-provider fetch failures never fail the whole list — one provider
// being unreachable doesn't hide the others.
func (s *service) List(ctx context.Context) ([]Provider, error) {
	rows, err := s.deps.Catalog.List(ctx)
	if err != nil {
		return nil, err
	}
	modelsByProv := s.offeringsByProvider(ctx, rows)

	out := make([]Provider, 0, len(rows))
	for _, row := range rows {
		var health Health
		var credits Credits
		if row.Enabled {
			health, credits = s.refresh(ctx, row)
		} else {
			// Disabled provider: serve whatever was last cached, never
			// refresh — no status-page/balance probes leave the daemon for
			// a provider the operator switched off. decodeHealth/
			// decodeCredits map a missing cache ("") to unknown/
			// supported:false.
			cached, _ := s.deps.Catalog.State(ctx, row.ID)
			if cached == nil {
				cached = &store.ProviderStateRow{}
			}
			health = decodeHealth(cached.HealthJSON)
			credits = decodeCredits(cached.CreditsJSON)
		}
		models := modelsByProv[row.Name]
		if models == nil {
			models = []Model{}
		}
		out = append(out, Provider{
			ID:                 row.ID,
			Name:               row.Name,
			APIKeyMasked:       maskSecret(row.APIKey),
			BillCurrency:       row.BillCurrency,
			Health:             health,
			Credits:            credits,
			Models:             models,
			BillingEnabled:     row.BillingEnabled,
			BillingConsoleURL:  row.BillingConsoleURL,
			CreditsURL:         row.CreditsURL,
			TargetURL:          row.TargetURL,
			StatusURL:          row.StatusURL,
			OrgID:              row.OrgID,
			Enabled:            row.Enabled,
			Country:            row.Country,
			DataResidencyGroup: row.DataResidencyGroup,
		})
	}
	return out, nil
}

// refresh returns the (health, credits) pair for one provider, preferring
// the cache and refreshing stale entries. Never returns an error — a
// fetch failure degrades to "unknown" health + supported:false credits
// (or the stale cache when present). The cache row is updated to reflect
// whatever was served so the next reader sees consistent state.
func (s *service) refresh(ctx context.Context, row store.ProviderRow) (Health, Credits) {
	cached, _ := s.deps.Catalog.State(ctx, row.ID)
	now := s.deps.now()
	if cached != nil && now.Sub(cached.FetchedAt) < s.deps.CacheTTL {
		return decodeHealth(cached.HealthJSON), decodeCredits(cached.CreditsJSON)
	}

	// Per-provider mutex: a concurrent List call may have already refreshed
	// this provider while we were waiting. Re-read the cache after acquiring
	// the lock; if it's now fresh, use it.
	mu := s.perProviderMu(row.Name)
	mu.Lock()
	defer mu.Unlock()

	cached, _ = s.deps.Catalog.State(ctx, row.ID)
	if cached != nil && now.Sub(cached.FetchedAt) < s.deps.CacheTTL {
		return decodeHealth(cached.HealthJSON), decodeCredits(cached.CreditsJSON)
	}

	health := s.health.fetch(ctx, row)
	credits := s.credits.fetch(ctx, row)

	// Persist the fresh cache. Best-effort: a write failure doesn't fail
	// the read (the next request will just re-fetch).
	_ = s.deps.Catalog.SaveState(ctx, store.ProviderStateRow{
		ProviderID:  row.ID,
		HealthJSON:  encodeHealth(health),
		CreditsJSON: encodeCredits(credits),
		FetchedAt:   now,
	})
	return health, credits
}

// maskSecret returns prefix+ellipsis (Contract 1 §18: never the full
// secret). Mirrors httpapi.maskSecret so the masked form is identical
// across the providers + compressor surfaces — the handler can compare them
// for equality in tests. "" stays "".
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	const prefix = 4
	if len(s) <= prefix {
		return s + "…"
	}
	return s[:prefix] + "…"
}
