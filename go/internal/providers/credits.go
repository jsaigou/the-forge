// SPDX-License-Identifier: Apache-2.0

package providers

// credits.go — provider balance/credits clients (Sprint 0 §0.3).
//
// Per the freeze: "per-provider client queries the balance API (DeepSeek
// supports it; AI& TBD — supported:false when absent)". §0.3 shipped with
// AI& hardcoded to OpenAI-shaped balance paths (user/balance, v1/credits,
// v1/billing/credit_grants, v1/me), all of which 404 against the real AI&
// API. The first F4 fix attempt (docs/v5-review-fixes.md) guessed at a
// GET /v1/analytics/summary "balance" endpoint — also wrong: confirmed
// against the real docs (docs.aiand.com/billing/credits/, saved 2026-07-23)
// that AI& has **no balance-query API at all**. It's a prepaid model:
// credits are purchased only via a Stripe web checkout
// (console.aiand.com/settings/billing); the only API-visible signals are a
// 402 at zero balance and real usage/cost data via the Analytics API
// (docs.aiand.com/analytics/metrics/, also confirmed real). So "credits"
// for AI& is fundamentally a different shape than DeepSeek's balance
// lookup — this file surfaces AI&'s real period *spend* (Credits.SpendPeriod)
// instead of pretending a balance figure exists. This file carries the
// DeepSeek client, the AI& client (spend, not balance), the unsupported
// fallback, and a generic client for providers that set credits_url to a
// DeepSeek-shape endpoint.
//
// Live-verified against ForgeHost's real DeepSeek API key on 2026-07-22:
//   GET https://api.deepseek.com/user/balance
//   Authorization: Bearer <api_key>
//   ⇒ {"is_available": true,
//      "balance_infos": [{"currency":"USD",
//                          "total_balance":"32.15",
//                          "granted_balance":"0.00",
//                          "topped_up_balance":"32.15"}]}
//
// The currency field is per-balance-info (a provider may carry multiple
// balances in different currencies); we surface the first balance_info's
// currency + total_balance.
//
// AI&'s Analytics endpoint shape is confirmed from the real docs (not
// live-called — this sandbox has no AI& key):
//   GET https://api.aiand.com/v1/analytics/metrics?range=24h
//   Authorization: Bearer <api_key>
//   X-Org-ID: <org_id>
//   ⇒ {"range":"24h","buckets":[{"ts":"...","requests":1234,
//        "input_tokens":567890,"output_tokens":12345,"cost_usd":1.23,
//        "errors":5,"p50_latency_ms":420,"p95_latency_ms":1850}, ...]}
// cost_usd is summed across every bucket in the range to produce the
// period spend figure. Auth requires X-Org-ID alongside the API key —
// store.ProviderRow.OrgID (0006_provider_org_id.sql) carries it; if unset,
// the client can't call at all (Supported:false, same as "no balance API"
// — matches reality, since without an org id there's nothing to query).
//
// OpenRouter's key-info endpoint (Sprint E, docs.openrouter.ai's Go/Python/
// TS SDK reference pages, confirmed 2026-08-04 — this sandbox has no
// OpenRouter key to live-verify against):
//   GET https://openrouter.ai/api/v1/key
//   Authorization: Bearer <api_key>
//   ⇒ {"data":{"label":"...","limit":100,"limit_remaining":42.5,
//        "usage":57.5, ...}}
// This is the ordinary inference key's own key-info endpoint, distinct from
// GET /api/v1/credits (their SDK docs state that one requires a *management*
// key, which is never what's stored in router_providers.api_key — do not
// switch to it). limit_remaining is nullable (unlimited key ⇒ no balance
// figure, same treatment as DeepSeek's is_available:false case); usage is
// all-time spend, surfaced as SpendPeriod alongside BalanceNative — unlike
// AI&, OpenRouter's shape gives us both concepts at once.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// creditsClients dispatches per provider name, falling back to a generic
// client when credits_url is set on a provider with no specific client.
type creditsClients struct {
	deepSeek    *deepSeekCreditsClient
	aiAnd       *aiAndCreditsClient
	openRouter  *openRouterCreditsClient
	generic     *genericCreditsClient
	unsupported *unsupportedCreditsClient
}

func newCreditsClients(c httpClient, timeout time.Duration) *creditsClients {
	return &creditsClients{
		deepSeek:    &deepSeekCreditsClient{client: c, timeout: timeout},
		aiAnd:       &aiAndCreditsClient{client: c, timeout: timeout},
		openRouter:  &openRouterCreditsClient{client: c, timeout: timeout},
		generic:     &genericCreditsClient{client: c, timeout: timeout},
		unsupported: &unsupportedCreditsClient{},
	}
}

// providerCreditsKey normalizes a provider display name to the stable key the
// credits dispatch switches on. The switch used to match row.Name verbatim,
// which broke the moment a provider was renamed off the exact slug — "AI&"
// (allowed by validate.go's 2026-08-14 "&" opening) fell into the default
// branch and silently lost its spend feed. Mirrors the FE providerIconSlug
// aliasing so a rename keeps routing to the right client: lowercase, "&"→"and",
// drop everything non-alphanumeric ("AI&"→"aiand", "DeepSeek"→"deepseek",
// "OpenRouter"→"openrouter", "ai and"→"aiand").
func providerCreditsKey(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "&", "and")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fetch picks the right client for one provider. Never returns an error —
// a fetch failure degrades to Supported:false (or the stale cache, which
// the caller's refresh() applies before reaching here).
func (c *creditsClients) fetch(ctx context.Context, row store.ProviderRow) Credits {
	// Billing API toggle (product/QA sprint, 2026-07-29): short-circuit
	// before any probe when the operator has disabled it for this
	// provider. Supported:false renders the same as "no balance API" on
	// the Console chip — the credits/spend region goes blank rather than
	// showing a stale cached figure.
	if !row.BillingEnabled {
		return Credits{Supported: false}
	}
	switch providerCreditsKey(row.Name) {
	case "deepseek":
		// DeepSeek's balance endpoint is a known constant. credits_url on
		// the provider row overrides it (an operator who routes DeepSeek
		// through a proxy can point at the proxy's balance path).
		url := row.CreditsURL
		if url == "" {
			url = "https://api.deepseek.com/user/balance"
		}
		return c.deepSeek.fetch(ctx, url, row.APIKey)
	case "aiand":
		// AI& has no balance API (confirmed — see the file doc comment);
		// this surfaces real period spend via the Analytics API instead.
		// credits_url overrides the default endpoint, same pattern as
		// DeepSeek. Requires row.OrgID (X-Org-ID) — see aiAndCreditsClient.
		url := row.CreditsURL
		if url == "" {
			url = "https://api.aiand.com/v1/analytics/metrics?range=24h"
		}
		return c.aiAnd.fetch(ctx, url, row.APIKey, row.OrgID)
	case "openrouter":
		// OpenRouter's key-info endpoint is a known constant, same override
		// pattern as DeepSeek/AI&.
		url := row.CreditsURL
		if url == "" {
			url = "https://openrouter.ai/api/v1/key"
		}
		return c.openRouter.fetch(ctx, url, row.APIKey)
	default:
		// Unknown provider: a configured credits_url ⇒ try the generic
		// DeepSeek-shape parser; else Unsupported:false (no balance API).
		if row.CreditsURL == "" {
			return c.unsupported.fetch()
		}
		return c.generic.fetch(ctx, row.CreditsURL, row.APIKey)
	}
}

// ── DeepSeek ─────────────────────────────────────────────────────────────────

// deepSeekCreditsClient queries the DeepSeek balance API. Live-verified
// response shape (2026-07-22):
//   {"is_available": true,
//    "balance_infos": [{"currency":"USD","total_balance":"32.15",
//                       "granted_balance":"0.00","topped_up_balance":"32.15"}]}
//
// total_balance is a string (DeepSeek returns "32.15", not 32.15) — parsed
// as float64. is_available:false means the account is frozen/disabled;
// we surface that as Supported:true (the API exists) with a nil balance
// (the handler renders balance_native:null).
type deepSeekCreditsClient struct {
	client  httpClient
	timeout time.Duration
}

type deepSeekBalanceJSON struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency      string `json:"currency"`
		TotalBalance  string `json:"total_balance"`
		GrantedBalance string `json:"granted_balance"`
		ToppedUpBalance string `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

func (c *deepSeekCreditsClient) fetch(ctx context.Context, url, apiKey string) Credits {
	body, code, err := fetchJSON(ctx, c.client, url, apiKey, c.timeout)
	if err != nil || code != 200 {
		// Network/HTTP failure ⇒ Supported:true (the API exists, we just
		// couldn't reach it) but no balance. The caller's cache layer
		// will serve stale if available.
		return Credits{Supported: true}
	}
	var bal deepSeekBalanceJSON
	if err := json.Unmarshal(body, &bal); err != nil {
		return Credits{Supported: true}
	}
	if !bal.IsAvailable {
		// Account frozen — surface as supported with no balance.
		return Credits{Supported: true, AsOf: time.Now()}
	}
	if len(bal.BalanceInfos) == 0 {
		return Credits{Supported: true, AsOf: time.Now()}
	}
	info := bal.BalanceInfos[0]
	bal32, _ := strconv.ParseFloat(info.TotalBalance, 64)
	return Credits{
		BalanceNative: &bal32,
		Currency:      info.Currency,
		AsOf:          time.Now(),
		Supported:     true,
	}
}

// ── Generic (DeepSeek-shape) ─────────────────────────────────────────────────

// genericCreditsClient parses the same shape as DeepSeek's balance API but
// for an arbitrary URL — used when a provider sets credits_url to a custom
// endpoint that happens to return the DeepSeek shape (e.g. a proxy that
// forwards DeepSeek's balance response). If the response doesn't match,
// Supported:true with a nil balance (we know the endpoint exists, just
// not what shape it returns).
type genericCreditsClient struct {
	client  httpClient
	timeout time.Duration
}

func (c *genericCreditsClient) fetch(ctx context.Context, url, apiKey string) Credits {
	body, code, err := fetchJSON(ctx, c.client, url, apiKey, c.timeout)
	if err != nil || code != 200 {
		return Credits{Supported: true}
	}
	var bal deepSeekBalanceJSON
	if err := json.Unmarshal(body, &bal); err != nil {
		return Credits{Supported: true}
	}
	if !bal.IsAvailable || len(bal.BalanceInfos) == 0 {
		return Credits{Supported: true, AsOf: time.Now()}
	}
	info := bal.BalanceInfos[0]
	bal32, _ := strconv.ParseFloat(info.TotalBalance, 64)
	return Credits{
		BalanceNative: &bal32,
		Currency:      info.Currency,
		AsOf:          time.Now(),
		Supported:     true,
	}
}

// ── AI& ───────────────────────────────────────────────────────────────────

// aiAndCreditsClient queries AI&'s Analytics API for real period spend
// (docs/v5-review-fixes.md F4). AI& has **no balance-query API** —
// confirmed against the real docs (docs.aiand.com/billing/credits/, saved
// 2026-07-23): credits are a prepaid balance purchased only via a Stripe
// web checkout (console.aiand.com/settings/billing); there is no endpoint
// anywhere that returns a remaining-balance figure. What the API *does*
// expose is real usage/cost data via the Analytics API
// (docs.aiand.com/analytics/metrics/, also confirmed real):
//
//	GET https://api.aiand.com/v1/analytics/metrics?range=24h
//	Authorization: Bearer <api_key>
//	X-Org-ID: <org_id>
//	⇒ {"range":"24h","buckets":[{"ts":"2026-05-18T00:00:00Z","requests":1234,
//	     "input_tokens":567890,"output_tokens":12345,"cost_usd":1.23,
//	     "errors":5,"p50_latency_ms":420,"p95_latency_ms":1850}, ...]}
//
// This client sums cost_usd across every returned bucket and reports it as
// Credits.SpendPeriod (currency USD — the response has no per-bucket
// currency field, and cost_usd's name says so directly), not
// BalanceNative — the two are different concepts and must not be
// conflated (a spend figure rendered where a remaining-balance figure is
// expected would mislead an operator into over- or under-estimating
// available credit). auth requires X-Org-ID alongside the API key
// (confirmed on the same docs page's "Auth" section); store.ProviderRow.OrgID
// (0006_provider_org_id.sql) carries it — if unset, there's nothing valid to
// call, so this degrades to Supported:false (matches "no balance API"
// exactly, since without an org id the analytics endpoint can't be reached
// either).
type aiAndCreditsClient struct {
	client  httpClient
	timeout time.Duration
}

// aiAndMetricsJSON mirrors the confirmed GET /analytics/metrics response
// shape (docs.aiand.com/analytics/metrics/). Only the fields this client
// needs are decoded; the rest (requests, tokens, errors, latencies) are
// left for a future consumer.
type aiAndMetricsJSON struct {
	Range   string `json:"range"`
	Buckets []struct {
		CostUSD float64 `json:"cost_usd"`
	} `json:"buckets"`
}

func (c *aiAndCreditsClient) fetch(ctx context.Context, url, apiKey, orgID string) Credits {
	if orgID == "" {
		// No X-Org-ID configured ⇒ the required auth can't be sent, so
		// there's nothing valid to query — matches "no balance API"
		// (Supported:false) rather than guessing at a spend figure we
		// can't actually fetch.
		return Credits{Supported: false}
	}
	body, code, err := fetchJSONWithHeaders(ctx, c.client, url, apiKey,
		map[string]string{"X-Org-ID": orgID}, c.timeout)
	if err != nil || code != 200 {
		// Unreachable/erroring ⇒ Supported:true (the concept — period
		// spend — is real for this provider) with no figure this round;
		// the caller's cache layer serves stale data if any exists.
		return Credits{Supported: true}
	}
	var m aiAndMetricsJSON
	if err := json.Unmarshal(body, &m); err != nil {
		return Credits{Supported: true, AsOf: time.Now()}
	}
	var total float64
	for _, b := range m.Buckets {
		total += b.CostUSD
	}
	label := m.Range + " spend"
	if label == " spend" {
		label = "24h spend"
	}
	return Credits{
		SpendPeriod:      &total,
		SpendPeriodLabel: label,
		Currency:         "USD",
		AsOf:             time.Now(),
		Supported:        true,
	}
}

// ── OpenRouter ───────────────────────────────────────────────────────────

// openRouterCreditsClient queries OpenRouter's key-info endpoint (Sprint E).
// See the file doc comment above for the endpoint, auth, and the
// management-key-vs-inference-key trap. limit_remaining null ⇒ an
// unlimited key, surfaced the same way DeepSeek surfaces a frozen account:
// Supported:true with a nil balance rather than a fabricated zero.
type openRouterCreditsClient struct {
	client  httpClient
	timeout time.Duration
}

type openRouterKeyJSON struct {
	Data struct {
		LimitRemaining *float64 `json:"limit_remaining"`
		Usage          float64  `json:"usage"`
	} `json:"data"`
}

func (c *openRouterCreditsClient) fetch(ctx context.Context, url, apiKey string) Credits {
	body, code, err := fetchJSON(ctx, c.client, url, apiKey, c.timeout)
	if err != nil || code != 200 {
		return Credits{Supported: true}
	}
	var k openRouterKeyJSON
	if err := json.Unmarshal(body, &k); err != nil {
		return Credits{Supported: true}
	}
	usage := k.Data.Usage
	return Credits{
		BalanceNative:    k.Data.LimitRemaining,
		SpendPeriod:      &usage,
		SpendPeriodLabel: "all-time usage",
		Currency:         "USD",
		AsOf:             time.Now(),
		Supported:        true,
	}
}

// ── Unsupported (any provider without a balance API and no configured
// credits_url) ───────────────────────────────────────────────────────────

// unsupportedCreditsClient returns the frozen §0.3 "provider API has no
// balance endpoint" shape: Supported:false, no balance, no as_of. No
// network fetch — this is a static answer per provider. Providers with a
// dedicated client above (deepseek, aiand) never reach this path.
type unsupportedCreditsClient struct{}

func (c *unsupportedCreditsClient) fetch() Credits {
	return Credits{Supported: false}
}
