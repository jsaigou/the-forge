// SPDX-License-Identifier: Apache-2.0

// Package web implements smith's P5 web-research providers
// (docs/v5-smith.md §4.8): searxng (search), firecrawl (fetch), and direct
// (fetch terminus). Camofox is deferred.
//
// Design departs from §4.8's single Provider interface: searxng cannot
// fetch and firecrawl/direct cannot search, so one combined interface would
// force two-thirds of every adapter to be a stub that "fails" and burns a
// slot in the fallback chain. Instead, unexported adapters implement
// role-specific methods that never return an error (the house convention
// at internal/providers/health.go:62-73 — the chain driver decides
// fallback, adapters don't); the exported Service keeps an error-returning
// signature because a chat turn genuinely needs to render "web research
// unavailable".
//
// Provider health lives in an in-memory map, not the DB (smith_web_cache is
// for content, not reachability). A provider that has never been probed, or
// whose probe itself failed, is still attempted on the next real call —
// "unknown" is never treated as "unreachable". In fact reachability never
// gates which adapters are *tried*: EffectiveChain-style filtering only
// decides which are *configured*; the chain driver's own fallback (moving
// to the next adapter on a failed Attempt) is what makes probe staleness
// harmless — a probe is purely for the status surface (GET /smith/status,
// the Settings UI), never a gate on real requests.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
	"golang.org/x/sync/singleflight"
)

// Per-call budgets (docs/v5-smith.md §4.8). Code constants, not settings —
// same class as smith's chatTimeout: safety budgets, not operator
// preferences.
const (
	searchTimeout = 10 * time.Second
	fetchTimeout  = 30 * time.Second
	probeTimeout  = 5 * time.Second
)

var (
	// ErrDisabled means smith.web.enabled is false (or unset — disabled is
	// the safe default until an operator opts in).
	ErrDisabled = errors.New("web: research disabled")
	// ErrNoSearchProvider means no search-capable adapter is configured +
	// enabled, or the one configured (searxng) failed.
	ErrNoSearchProvider = errors.New("web: no search provider reachable")
	// ErrAllProvidersFailed means every fetch-capable adapter in the
	// effective chain (including the always-last direct terminus) failed.
	ErrAllProvidersFailed = errors.New("web: all providers failed")
)

// Result is one search hit.
type Result struct {
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	Snippet     string     `json:"snippet"`
	Engine      string     `json:"engine"`
	Score       float64    `json:"score"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// Document is a fetched, extracted page.
type Document struct {
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Text        string    `json:"text"`
	ContentType string    `json:"content_type"`
	StatusCode  int       `json:"status_code"`
	Provider    string    `json:"provider"`
	Truncated   bool      `json:"truncated"`
	FetchedAt   time.Time `json:"fetched_at"`
	Cached      bool      `json:"cached"`
}

// Source is the minimal citation record attached to a chat message
// (smith_messages.sources) and rendered in the transcript.
type Source struct {
	Provider  string    `json:"provider"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Snippet   string    `json:"snippet"`
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
}

// ProviderStatus is the reachability + configuration view surfaced on
// GET /smith/status and the Settings "Web research" card. CheckedAt is nil
// until the first probe or real usage — "never probed" is nil, not a zero
// time.Time, so the wire form is a clean JSON null rather than a
// "0001-01-01T00:00:00Z" sentinel the FE would have to special-case.
type ProviderStatus struct {
	Name       string     `json:"name"`
	Role       string     `json:"role"` // "search" | "fetch"
	Configured bool       `json:"configured"`
	Enabled    bool       `json:"enabled"`
	Reachable  bool       `json:"reachable"`
	Detail     string     `json:"detail"`
	CheckedAt  *time.Time `json:"checked_at"`
	LatencyMS  int64      `json:"latency_ms"`
}

// Attempt records one adapter call's outcome — used for chain-order
// assertions in tests and for updating the in-memory status map. Never
// carries an error value; Detail is the human-readable reason.
type Attempt struct {
	Provider   string
	OK         bool
	Detail     string
	Status     int
	DurationMS int64
}

// searcher is the unexported search-adapter boundary. Implementations never
// return an error — a failure is OK:false on the Attempt.
type searcher interface {
	search(ctx context.Context, cfg ProviderConfig, q string, limit int) ([]Result, Attempt)
}

// fetcher is the unexported fetch-adapter boundary.
type fetcher interface {
	fetch(ctx context.Context, cfg ProviderConfig, url string) (*Document, Attempt)
}

// Service is the exported surface smith consumes.
type Service interface {
	// Search returns up to limit results for q, using smith.web.cache_ttl.
	Search(ctx context.Context, q string, limit int) ([]Result, error)
	// Fetch returns url's extracted content, using smith.web.cache_ttl.
	Fetch(ctx context.Context, url string) (*Document, error)
	// FetchWithTTL is Fetch with a caller-supplied cache freshness window
	// instead of the configured one — the blocked-item recheck (P5 §4.9)
	// uses a 7-day TTL so the cache itself acts as its cooldown.
	FetchWithTTL(ctx context.Context, url string, ttl time.Duration) (*Document, error)
	// FetchDirect fetches through the `direct` adapter only, bypassing
	// firecrawl — used for GitHub API calls (signals.go), where firecrawl's
	// markdown extraction would mangle a JSON response.
	FetchDirect(ctx context.Context, url string, ttl time.Duration) (*Document, error)
	// Providers reports the current configuration + last-known reachability
	// for every adapter, in role order. Never nil.
	Providers(ctx context.Context) []ProviderStatus
	// Probe actively re-checks reachability for every configured + enabled
	// adapter and updates the status map Providers reads. Never returns an
	// error; failures just leave a provider Reachable:false.
	Probe(ctx context.Context)
}

// Cache is the URL/query-keyed content cache (smith_web_cache). Get reports
// exists, not fresh — callers compare FetchedAt against their own TTL
// (search/normal fetch use smith.web.cache_ttl; the blocked-item recheck
// overrides it), which is what makes FetchWithTTL possible without a
// second table.
type Cache interface {
	Get(ctx context.Context, kind, key string) (CacheEntry, bool)
	Put(ctx context.Context, e CacheEntry) error
}

// Deps wires a Service. The zero-value Deps is usable — New fills every
// default — but Cache/Settings nil means "no caching" / "DefaultConfig
// always" respectively, both safe degrades.
type Deps struct {
	// Cache backs the content cache. nil = every call re-fetches, nothing
	// persisted.
	Cache Cache

	// Settings is the smith.web.* settings KV. nil = DefaultConfig()
	// (disabled) is used for every call.
	Settings store.Settings

	// HTTPClient overrides the client used for searxng + firecrawl calls
	// (operator-configured, trusted base URLs). nil = a plain client is
	// constructed. Never used for the `direct` adapter — see
	// AllowDirectHost.
	HTTPClient httpClient

	// AllowDirectHost overrides the `direct` adapter's SSRF guard for a
	// specific host. nil in production (the real guard: reject loopback /
	// private / link-local / CGNAT, checked at dial time after resolution
	// so DNS rebinding can't slip through). Tests pointing `direct` at an
	// httptest.Server (127.0.0.1) must set this explicitly — the guard
	// never loosens silently for convenience.
	AllowDirectHost func(host string) bool

	// UserAgent is sent on every outbound request. "" → a default value.
	UserAgent string

	// Now returns the current time. nil → time.Now.
	Now func() time.Time

	// Logf receives debug logging (every fetch, per §4.8: "every fetch is
	// logged at debug"). nil → no-op.
	Logf func(format string, args ...any)
}

// New builds a Service. Never panics on a zero-value Deps.
func New(deps Deps) Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	if deps.UserAgent == "" {
		deps.UserAgent = "forge-smith/dev"
	}
	plain := deps.HTTPClient
	if plain == nil {
		plain = newPlainHTTPClient()
	}
	guarded := newGuardedHTTPClient(deps.AllowDirectHost)
	return &service{
		deps:         deps,
		searxng:      &searxngAdapter{client: plain, userAgent: deps.UserAgent},
		firecrawl:    &firecrawlAdapter{client: plain, userAgent: deps.UserAgent},
		direct:       &directAdapter{client: guarded, userAgent: deps.UserAgent},
		customSearch: &customSearchAdapter{client: plain, userAgent: deps.UserAgent},
		customFetch:  &customFetchAdapter{client: plain, userAgent: deps.UserAgent},
	}
}

type service struct {
	deps Deps

	searxng      *searxngAdapter
	firecrawl    *firecrawlAdapter
	direct       *directAdapter
	customSearch *customSearchAdapter
	customFetch  *customFetchAdapter

	sf singleflight.Group

	statusMu sync.Mutex
	status   map[string]ProviderStatus
}

func (s *service) logf(format string, args ...any) {
	s.deps.Logf("smith/web: "+format, args...)
}

// recordAttempt updates the in-memory status map from a real usage attempt
// (not just an explicit Probe) — status stays fresh from ordinary traffic
// between probe intervals.
func (s *service) recordAttempt(a Attempt) {
	if a.Provider == "" {
		return
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.status == nil {
		s.status = map[string]ProviderStatus{}
	}
	now := s.deps.Now()
	s.status[a.Provider] = ProviderStatus{
		Reachable: a.OK,
		Detail:    a.Detail,
		CheckedAt: &now,
		LatencyMS: a.DurationMS,
	}
}

func (s *service) Search(ctx context.Context, q string, limit int) ([]Result, error) {
	cfg := LoadConfig(ctx, s.deps.Settings)
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if limit <= 0 {
		limit = 10
	}
	if len(cfg.searchOrder()) == 0 {
		return nil, ErrNoSearchProvider
	}
	key := normalizeSearchKey(q)
	if results, ok := s.cachedSearch(ctx, key, cfg.CacheTTL); ok {
		return results, nil
	}
	v, err, _ := s.sf.Do("search:"+key, func() (any, error) {
		if results, ok := s.cachedSearch(ctx, key, cfg.CacheTTL); ok {
			return results, nil
		}

		// Walk the enabled search-capable adapters in order; the first one
		// that returns OK wins. (Was searxng-only; the generic customsearch
		// adapter joined the chain 2026-08-14.)
		var last Attempt
		for _, name := range cfg.searchOrder() {
			sctx, cancel := context.WithTimeout(ctx, searchTimeout)
			var results []Result
			var att Attempt
			switch name {
			case "searxng":
				results, att = s.searxng.search(sctx, cfg.Searxng, q, limit)
			case "customsearch":
				results, att = s.customSearch.search(sctx, cfg.CustomSearch, q, limit)
			}
			cancel()
			s.logf("search provider=%s ok=%v detail=%q duration_ms=%d", name, att.OK, att.Detail, att.DurationMS)
			s.recordAttempt(att)
			if att.OK {
				if s.deps.Cache != nil {
					body, _ := json.Marshal(results)
					now := s.deps.Now()
					_ = s.deps.Cache.Put(ctx, CacheEntry{
						Kind: "search", Key: key, Provider: name, Body: string(body),
						Bytes: len(body), FetchedAt: now, ExpiresAt: now.Add(cfg.CacheTTL),
					})
				}
				return results, nil
			}
			last = att
		}
		detail := "no search provider succeeded"
		if last.Provider != "" {
			detail = last.Detail
		}
		return nil, fmt.Errorf("%w: %s", ErrNoSearchProvider, detail)
	})
	if err != nil {
		return nil, err
	}
	return v.([]Result), nil
}

func (s *service) cachedSearch(ctx context.Context, key string, ttl time.Duration) ([]Result, bool) {
	if s.deps.Cache == nil {
		return nil, false
	}
	entry, ok := s.deps.Cache.Get(ctx, "search", key)
	if !ok || s.deps.Now().Sub(entry.FetchedAt) >= ttl {
		return nil, false
	}
	var results []Result
	if json.Unmarshal([]byte(entry.Body), &results) != nil {
		return nil, false
	}
	return results, true
}

func (s *service) Fetch(ctx context.Context, url string) (*Document, error) {
	return s.FetchWithTTL(ctx, url, 0)
}

func (s *service) FetchWithTTL(ctx context.Context, url string, ttl time.Duration) (*Document, error) {
	cfg := LoadConfig(ctx, s.deps.Settings)
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if ttl <= 0 {
		ttl = cfg.CacheTTL
	}
	return s.doFetch(ctx, url, ttl, cfg.CacheTTL, cfg.fetchOrder(), cfg)
}

func (s *service) FetchDirect(ctx context.Context, url string, ttl time.Duration) (*Document, error) {
	cfg := LoadConfig(ctx, s.deps.Settings)
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if ttl <= 0 {
		ttl = cfg.CacheTTL
	}
	return s.doFetch(ctx, url, ttl, cfg.CacheTTL, []string{"direct"}, cfg)
}

// doFetch is the shared cache+chain walk behind Fetch/FetchWithTTL/
// FetchDirect. storeTTL is the duration written as the cache row's
// lifetime (always the configured cache_ttl, never a caller's freshness
// override, so a 7-day-tolerant recheck doesn't force every other reader
// to also tolerate 7-day-stale content) — freshness on read is governed by
// the caller-supplied ttl, not storeTTL.
func (s *service) doFetch(ctx context.Context, rawURL string, ttl, storeTTL time.Duration, order []string, cfg Config) (*Document, error) {
	key := normalizeFetchKey(rawURL)
	if doc, ok := s.cachedDocument(ctx, key, ttl); ok {
		return doc, nil
	}
	v, err, _ := s.sf.Do("fetch:"+key, func() (any, error) {
		if doc, ok := s.cachedDocument(ctx, key, ttl); ok {
			return doc, nil
		}

		// SSRF guard: validate the URL before any adapter runs. Firecrawl
		// bypasses the direct adapter's dial-time guard in newGuardedHTTPClient,
		// so the check must happen here, before the chain walk.
		if err := validateFetchURL(ctx, rawURL, s.deps.AllowDirectHost); err != nil {
			return nil, err
		}

		if len(order) == 0 {
			return nil, ErrAllProvidersFailed
		}
		attempts := 0
		for _, name := range order {
			fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
			var doc *Document
			var att Attempt
			switch name {
			case "firecrawl":
				doc, att = s.firecrawl.fetch(fctx, cfg.Firecrawl, rawURL)
			case "customfetch":
				doc, att = s.customFetch.fetch(fctx, cfg.CustomFetch, rawURL)
			case "direct":
				doc, att = s.direct.fetch(fctx, ProviderConfig{}, rawURL)
			}
			cancel()
			attempts++
			s.logf("fetch provider=%s url=%q ok=%v detail=%q duration_ms=%d", name, rawURL, att.OK, att.Detail, att.DurationMS)
			s.recordAttempt(att)
			if att.OK && doc != nil {
				now := s.deps.Now()
				doc.FetchedAt = now
				doc.Cached = false
				if s.deps.Cache != nil {
					_ = s.deps.Cache.Put(ctx, CacheEntry{
						Kind: "fetch", Key: key, Provider: doc.Provider, Title: doc.Title,
						ContentType: doc.ContentType, StatusCode: doc.StatusCode, Body: doc.Text,
						BodySHA256: sha256Hex(doc.Text), Truncated: doc.Truncated, Bytes: len(doc.Text),
						FetchedAt: now, ExpiresAt: now.Add(maxDuration(ttl, storeTTL)),
					})
				}
				return doc, nil
			}
		}
		return nil, fmt.Errorf("%w (%d attempts)", ErrAllProvidersFailed, attempts)
	})
	if err != nil {
		return nil, err
	}
	return v.(*Document), nil
}

func (s *service) cachedDocument(ctx context.Context, key string, ttl time.Duration) (*Document, bool) {
	if s.deps.Cache == nil {
		return nil, false
	}
	entry, ok := s.deps.Cache.Get(ctx, "fetch", key)
	if !ok || s.deps.Now().Sub(entry.FetchedAt) >= ttl {
		return nil, false
	}
	return &Document{
		URL: key, Title: entry.Title, Text: entry.Body, ContentType: entry.ContentType,
		StatusCode: entry.StatusCode, Provider: entry.Provider, Truncated: entry.Truncated,
		FetchedAt: entry.FetchedAt, Cached: true,
	}, true
}

func (s *service) Providers(ctx context.Context) []ProviderStatus {
	cfg := LoadConfig(ctx, s.deps.Settings)
	rows := []struct {
		name       string
		role       string
		configured bool
		enabled    bool
	}{
		{"searxng", "search", cfg.Searxng.BaseURL != "", cfg.Searxng.Enabled},
		{"customsearch", "search", cfg.CustomSearch.BaseURL != "", cfg.CustomSearch.Enabled},
		{"firecrawl", "fetch", cfg.Firecrawl.BaseURL != "", cfg.Firecrawl.Enabled},
		{"customfetch", "fetch", cfg.CustomFetch.BaseURL != "", cfg.CustomFetch.Enabled},
		{"direct", "fetch", true, cfg.Direct.Enabled},
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	out := make([]ProviderStatus, 0, len(rows))
	for _, r := range rows {
		ps := s.status[r.name]
		ps.Name = r.name
		ps.Role = r.role
		ps.Configured = r.configured
		ps.Enabled = r.enabled
		out = append(out, ps)
	}
	return out
}

func (s *service) Probe(ctx context.Context) {
	cfg := LoadConfig(ctx, s.deps.Settings)
	if cfg.Searxng.Enabled && cfg.Searxng.BaseURL != "" {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, att := s.searxng.search(pctx, cfg.Searxng, "forge smith probe", 1)
		cancel()
		s.recordAttempt(att)
	}
	if cfg.CustomSearch.Enabled && cfg.CustomSearch.BaseURL != "" {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, att := s.customSearch.search(pctx, cfg.CustomSearch, "forge smith probe", 1)
		cancel()
		s.recordAttempt(att)
	}
	if cfg.Firecrawl.Enabled && cfg.Firecrawl.BaseURL != "" {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, att := s.firecrawl.fetch(pctx, cfg.Firecrawl, "https://example.com")
		cancel()
		s.recordAttempt(att)
	}
	if cfg.CustomFetch.Enabled && cfg.CustomFetch.BaseURL != "" {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, att := s.customFetch.fetch(pctx, cfg.CustomFetch, "https://example.com")
		cancel()
		s.recordAttempt(att)
	}
	if cfg.Direct.Enabled {
		s.recordAttempt(Attempt{Provider: "direct", OK: true, Detail: "always available"})
	}
}

func normalizeSearchKey(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}

func normalizeFetchKey(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if i := strings.IndexByte(trimmed, '#'); i >= 0 {
		trimmed = trimmed[:i]
	}
	return trimmed
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
