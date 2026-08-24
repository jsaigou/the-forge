// SPDX-License-Identifier: Apache-2.0

package providers

// discover.go — best-effort billing-API auto-discovery (product/QA sprint,
// 2026-07-29).
//
// There is NO cross-provider billing standard to parse: DeepSeek's own
// /user/balance publishes no OpenAPI spec, and a survey of LLM provider
// APIs (2026-07-29) found each rolls its own billing surface — only
// *inference* is OpenAI-compatible-standardized across providers, never
// billing/credits. So this is necessarily a curated-candidate-list-first
// approach, not a spec-parsing one:
//  1. A known real endpoint, for providers we already have one for
//     (deepseek, aiand — the same defaults credits.go's fetch() dispatch
//     hardcodes).
//  2. A handful of generic OpenAI-compatible-ish path guesses against an
//     unknown provider's target_url host.
//  3. One cheap opportunistic GET of <host>/openapi.json, scanning its
//     path list for anything balance/credit/usage-shaped — in case a
//     provider turns out to be the rare exception that publishes one.
//
// Detection reuses genericCreditsClient (the DeepSeek-shape parser) as the
// sole "does this look like a real balance response" signal, since that's
// the only generic parser that exists. A provider whose balance response
// uses a different JSON shape won't be detected here and needs its
// credits_url set manually — same as today, this doesn't make that case
// worse, it just doesn't help it either.
//
// Discover never persists anything — it's a pure probe. The caller (the
// HTTP handler) decides whether to save the result, and must never
// overwrite a credits_url the operator already set explicitly.

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// knownBillingEndpoints mirrors credits.go's fetch() dispatch defaults.
// Kept as its own table (not shared code) so discovery can offer these as
// *candidates* to probe/confirm, independent of the dispatch switch that
// picks a client at request time.
//
// "openrouter" is deliberately absent (Sprint E, 2026-08-04) despite
// credits.go now having a real parser for it: Discover always confirms a
// hit with genericCreditsClient (the DeepSeek shape, see below) regardless
// of which candidate URL matched, and OpenRouter's real response
// ({"data":{"limit_remaining":...}}) doesn't parse as that shape. Adding
// OpenRouter here would make candidateURLs return its one real endpoint
// exclusively (see below) and then guarantee a false "not found" on every
// call — worse than leaving it out, which at least falls through to the
// generic host-guess path. OpenRouter's credits_url is supplied directly
// by the Sprint E preset (web/src/lib/providerPresets.ts) at create time
// instead, so the "Discover billing" button was never the path that needed
// to cover it.
var knownBillingEndpoints = map[string][]string{
	"deepseek": {"https://api.deepseek.com/user/balance"},
	"aiand":    {"https://api.aiand.com/v1/analytics/metrics?range=24h"},
}

// genericBillingPaths are OpenAI-compatible-ish guesses tried against an
// unknown provider's target_url host. None are guaranteed to exist —
// they're cheap (a handful of GETs) against a host we're already about to
// make authenticated requests to anyway.
var genericBillingPaths = []string{
	"/user/balance",
	"/v1/dashboard/billing/credit_grants",
	"/v1/billing/credit_grants",
	"/v1/credits",
	"/v1/me",
}

// DiscoveryAttempt records one candidate URL probed, for the API response
// (the operator sees what was tried, not just the winner).
type DiscoveryAttempt struct {
	URL    string
	Parsed bool // did the response look like a real balance/spend figure
}

// DiscoveryResult is the outcome of Discover.
type DiscoveryResult struct {
	Tried []DiscoveryAttempt
	Found bool
	URL   string  // winning URL; "" if none found
	Cred  Credits // fetched from the winner; zero value if none found
}

// Discover probes candidate balance-API URLs for row and returns the first
// one that yields a parseable balance figure.
func Discover(ctx context.Context, deps Deps, row store.ProviderRow) DiscoveryResult {
	c := deps.HTTPClient
	if c == nil {
		c = newDefaultHTTPClient(deps.ProbeTimeout)
	}
	timeout := deps.ProbeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	generic := &genericCreditsClient{client: c, timeout: timeout}

	var result DiscoveryResult
	for _, cand := range candidateURLs(ctx, c, timeout, row) {
		cr := generic.fetch(ctx, cand, row.APIKey)
		hit := cr.Supported && cr.BalanceNative != nil
		result.Tried = append(result.Tried, DiscoveryAttempt{URL: cand, Parsed: hit})
		if hit {
			result.Found = true
			result.URL = cand
			result.Cred = cr
			return result
		}
	}
	return result
}

// candidateURLs builds the probe list for row: the curated defaults when
// the provider name is known, else generic path guesses plus one
// opportunistic OpenAPI-spec probe against its target_url host.
func candidateURLs(ctx context.Context, c httpClient, timeout time.Duration, row store.ProviderRow) []string {
	if known, ok := knownBillingEndpoints[row.Name]; ok {
		return known
	}
	host := hostOf(row.TargetURL)
	if host == "" {
		return nil
	}
	out := make([]string, 0, len(genericBillingPaths)+1)
	for _, p := range genericBillingPaths {
		out = append(out, host+p)
	}
	if spec := probeOpenAPI(ctx, c, host, timeout); spec != "" {
		out = append(out, spec)
	}
	return out
}

// hostOf returns targetURL's scheme+host with no path/query, "" if
// targetURL doesn't parse or has no host (e.g. a relative/malformed URL).
func hostOf(targetURL string) string {
	u, err := url.Parse(targetURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// openAPISpecShape decodes just enough of an OpenAPI/Swagger document to
// scan its path list — this is deliberately not a real OpenAPI parser
// (nothing here needs request/response schemas, only path names).
type openAPISpecShape struct {
	Paths map[string]json.RawMessage `json:"paths"`
}

// probeOpenAPI fetches <host>/openapi.json (unauthenticated — a published
// spec is public by definition) and returns the first path whose name
// looks billing-shaped, "" if the fetch fails, isn't valid JSON, or no
// path matches. This is the "opportunistic" probe: cheap (one GET), and
// per the file doc comment, no provider surveyed so far has actually
// published one — this exists for the rare exception, not the common case.
func probeOpenAPI(ctx context.Context, c httpClient, host string, timeout time.Duration) string {
	body, code, err := fetchJSON(ctx, c, host+"/openapi.json", "", timeout)
	if err != nil || code != 200 {
		return ""
	}
	var spec openAPISpecShape
	if err := json.Unmarshal(body, &spec); err != nil || len(spec.Paths) == 0 {
		return ""
	}
	for p := range spec.Paths {
		lower := strings.ToLower(p)
		if strings.Contains(lower, "balance") || strings.Contains(lower, "credit") || strings.Contains(lower, "usage") {
			return host + p
		}
	}
	return ""
}
