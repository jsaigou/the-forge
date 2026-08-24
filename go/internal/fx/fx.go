// SPDX-License-Identifier: Apache-2.0

// Package fx implements the daemon-side live-FX fetcher + fx_rates cache
// (Sprint 0 §0.2 billing & currency). The dashboard's usage handler never
// fetches FX itself — it asks an fx.Source for the display currency, a rate,
// and the FX provenance (fx_as_of / fx_stale) that rides the UsageResponse.
//
// Rates are fetched server-side from a configurable source
// (billing.fx_source_url, default ECB reference rates via Frankfurter),
// cached in the fx_rates SQLite table, and refreshed on a configurable
// interval (billing.fx_refresh_min, default 60). On fetch failure the last
// cached rate stays served and is flagged stale once it ages past
// 2× the refresh interval — mirroring the §0.2 freeze ("on fetch failure the
// last cached rate is used and flagged stale").
//
// The fx_rates table and router_providers.bill_currency column are owned by
// migration 0002_polish.sql; this package reads/writes them directly via the
// store's *sql.DB handle so the frozen store sub-interfaces (Contract 2)
// stay untouched. The settings KV keys are the Sprint 0 §0.12 registered keys
// (billing.display_currency / billing.fx_source_url / billing.fx_refresh_min,
// constants live in internal/httpapi settings_handlers.go).
package fx

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Source is the billing-currency surface the usage handler consumes. A nil
// Source (Phase 4 stub environment) means "display = USD, no FX" — the handler
// treats every conversion as 1:1 and reports fx_as_of=null / fx_stale=false.
type Source interface {
	// DisplayCurrency returns billing.display_currency ("USD" when unset).
	DisplayCurrency(ctx context.Context) string

	// BillCurrency returns the provider's bill_currency from
	// router_providers ("USD" when the provider is unknown or the column is
	// unset). This is the native_currency tagged on external usage rows.
	BillCurrency(ctx context.Context, provider string) string

	// Rate returns the conversion rate from `from` to `to` (1 from = rate to).
	// ok=false when no rate is cached for the pair; the caller falls back to
	// 1.0 and flags the FX stale. from==to always yields (1.0, true).
	Rate(ctx context.Context, from, to string) (rate float64, ok bool)

	// Provenance reports the FX state for the response-level fx_as_of /
	// fx_stale fields: the epoch the served rates were fetched, whether they
	// are stale, and whether any rates are cached at all. hasRates=false means
	// no conversion has ever succeeded (the handler reports fx_as_of=null,
	// fx_stale=true if a conversion was attempted).
	Provenance(ctx context.Context) (asOf time.Time, stale bool, hasRates bool)
}

// Default source: ECB reference rates served by Frankfurter (free, no API
// key, no rate limit for this volume). The §0.2 freeze leaves the provider
// choice to BE-2; this is it. Operators override with billing.fx_source_url,
// which must return the same {base, rates} JSON shape.
const defaultFxSourceURL = "https://api.frankfurter.app/latest?from=USD"

// Defaults for the settings-backed knobs (used when the setting is unset or
// unparseable). refreshInterval drives both the background tick and the
// staleness threshold (2×).
const (
	defaultRefreshMin  = 60
	defaultStaleFactor = 2
	httpTimeoutSec     = 10
)

// Settings keys (Sprint 0 §0.12 — frozen). These mirror the canonical
// constants in internal/httpapi/settings_handlers.go; they're duplicated
// here only because fx cannot import httpapi (httpapi imports fx for the
// Source interface). The keys are frozen strings, so drift is a non-issue.
const (
	settingDisplayCurrency = "billing.display_currency"
	settingFxSourceURL      = "billing.fx_source_url"
	settingFxRefreshMin     = "billing.fx_refresh_min"
)

// Cache is the daemon-side fx.Source: a background refresher that fetches
// live rates into the fx_rates table + an in-memory snapshot, and serves
// Rate/Provenance/DisplayCurrency/BillCurrency to the usage handler.
//
// The snapshot (base + rates + fetched_at from one consistent fetch) is the
// hot read path — Rate/Provenance never touch SQLite. fx_rates is the
// persistence layer so a daemon restart keeps the last good rates until the
// next refresh succeeds.
type Cache struct {
	db       *sql.DB
	settings store.Settings

	// fetch is the injectable rate-source call; the default hits
	// billing.fx_source_url (or defaultFxSourceURL) over HTTP. Tests inject
	// a fake to avoid the network.
	fetch func(ctx context.Context, sourceURL string) (base string, rates map[string]float64, err error)
	http  *http.Client

	// refreshMin caches the last-read billing.fx_refresh_min so Provenance's
	// staleness threshold doesn't read settings on every usage request; it is
	// refreshed on each Refresh tick (settings changes are picked up lazily,
	// not on every read).
	refreshMin atomic.Int64

	// snap is the consistent in-memory snapshot of the last successful fetch
	// (or the last persisted fetch loaded at startup). Nil/zero means no
	// rates are cached ⇒ Rate returns ok=false, Provenance hasRates=false.
	snap atomic.Pointer[snapshot]

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

type snapshot struct {
	base      string
	rates     map[string]float64 // base → quote: 1 base = rates[quote] quote
	fetchedAt time.Time
}

// Option configures New.
type Option func(*Cache)

// WithFetch replaces the default HTTP rate-source call (tests).
func WithFetch(f func(ctx context.Context, sourceURL string) (string, map[string]float64, error)) Option {
	return func(c *Cache) { c.fetch = f }
}

// New builds a Cache backed by db (fx_rates + router_providers) and the
// settings KV. Call Start to launch the background refresher; Close to stop.
func New(db *sql.DB, settings store.Settings, opts ...Option) *Cache {
	c := &Cache{
		db:       db,
		settings: settings,
		http:     &http.Client{Timeout: httpTimeoutSec * time.Second},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.fetch == nil {
		c.fetch = c.httpFetch
	}
	c.refreshMin.Store(defaultRefreshMin)
	c.loadFromDB()
	return c
}

// Start launches the background refresher (idempotent). It does one immediate
// refresh so rates are fresh on boot rather than waiting up to the refresh
// interval, then ticks on the configured cadence until Close. The first
// refresh is best-effort: a failure logs and the loop continues (the stale
// cached rate, if any, stays served).
func (c *Cache) Start(ctx context.Context) {
	c.once.Do(func() {
		go c.loop(ctx)
	})
}

// Close stops the background refresher. Idempotent; safe to call without Start.
func (c *Cache) Close() error {
	select {
	case <-c.stop:
		// already closed
	default:
		close(c.stop)
	}
	if c.done != nil {
		<-c.done
	}
	return nil
}

func (c *Cache) loop(parent context.Context) {
	defer close(c.done)

	// Immediate refresh on boot: don't serve a stale/empty rate for up to the
	// refresh interval if the source is reachable.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	c.refresh(ctx)

	for {
		min := c.currentRefreshMin()
		select {
		case <-c.stop:
			return
		case <-time.After(time.Duration(min) * time.Minute):
			c.refresh(ctx)
		}
	}
}

// Refresh fetches the latest rates and persists them. Exported so tests (and
// a future manual "refresh now" knob) can trigger a fetch outside the loop.
func (c *Cache) Refresh(ctx context.Context) error { return c.refresh(ctx) }

func (c *Cache) refresh(ctx context.Context) error {
	sourceURL := c.readSourceURL()
	min := c.currentRefreshMin()
	c.refreshMin.Store(int64(min))

	base, rates, err := c.fetch(ctx, sourceURL)
	if err != nil {
		// Leave the cached snapshot in place; it ages toward stale on its
		// own (Provenance's staleness threshold). Never crash the daemon on a
		// fetch failure — the dashboard must keep serving usage.
		log.Printf("fx: refresh from %s failed (serving cached rate): %v", sourceURL, err)
		return err
	}
	if base == "" || len(rates) == 0 {
		log.Printf("fx: refresh from %s returned no rates (base=%q, %d entries)", sourceURL, base, len(rates))
		return fmt.Errorf("fx: empty rate response")
	}

	now := time.Now()
	if err := c.persist(ctx, base, rates, now); err != nil {
		log.Printf("fx: persist rates: %v", err)
		return err
	}
	c.snap.Store(&snapshot{base: base, rates: rates, fetchedAt: now})
	return nil
}

// loadFromDB hydrates the in-memory snapshot from the last persisted fetch so
// a daemon restart keeps serving rates until the next refresh succeeds.
func (c *Cache) loadFromDB() {
	if c.db == nil {
		return
	}
	rows, err := c.db.Query(`SELECT base, quote, rate, fetched_at FROM fx_rates
		WHERE fetched_at = (SELECT MAX(fetched_at) FROM fx_rates)`)
	if err != nil {
		log.Printf("fx: load cached rates: %v", err)
		return
	}
	defer rows.Close()
	rates := map[string]float64{}
	var base string
	var fetchedAt time.Time
	for rows.Next() {
		var b, q string
		var r float64
		var ts int64
		if err := rows.Scan(&b, &q, &r, &ts); err != nil {
			log.Printf("fx: scan cached rate: %v", err)
			return
		}
		base = b
		rates[q] = r
		fetchedAt = time.Unix(ts, 0)
	}
	if err := rows.Err(); err != nil {
		log.Printf("fx: cached rates iteration: %v", err)
		return
	}
	if len(rates) == 0 {
		return
	}
	c.snap.Store(&snapshot{base: base, rates: rates, fetchedAt: fetchedAt})
}

func (c *Cache) persist(ctx context.Context, base string, rates map[string]float64, at time.Time) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	ts := at.Unix()
	for quote, rate := range rates {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO fx_rates (base, quote, rate, fetched_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT(base, quote) DO UPDATE SET rate = excluded.rate, fetched_at = excluded.fetched_at`,
			base, quote, rate, ts); err != nil {
			tx.Rollback()
			return fmt.Errorf("upsert %s/%s: %w", base, quote, err)
		}
	}
	return tx.Commit()
}

// ── Source implementation ─────────────────────────────────────────────────────

// DisplayCurrency reads billing.display_currency (default "USD" when unset).
func (c *Cache) DisplayCurrency(ctx context.Context) string {
	return c.readCurrencySetting(ctx, settingDisplayCurrency, "USD")
}

// BillCurrency reads router_providers.bill_currency for the provider ("USD"
// when the provider is unknown, the column is unset, or the DB is nil). This
// is the native_currency tagged on external usage rows.
func (c *Cache) BillCurrency(ctx context.Context, provider string) string {
	if c.db == nil || provider == "" {
		return "USD"
	}
	var ccy string
	err := c.db.QueryRowContext(ctx,
		`SELECT bill_currency FROM router_providers WHERE name = ?`, provider).Scan(&ccy)
	if err != nil || ccy == "" {
		return "USD"
	}
	return strings.ToUpper(ccy)
}

// Rate returns the conversion rate from `from` to `to`. from==to is always
// (1.0, true). With a cached snapshot from base B:
//   - from==B → rates[to]
//   - to==B   → 1/rates[from]
//   - both in rates (cross via B) → rates[to]/rates[from]
//
// ok=false when no snapshot is cached or the pair can't be resolved — the
// caller falls back to 1.0 and flags the FX stale.
func (c *Cache) Rate(_ context.Context, from, to string) (float64, bool) {
	from = strings.ToUpper(strings.TrimSpace(from))
	to = strings.ToUpper(strings.TrimSpace(to))
	if from == to {
		return 1.0, true
	}
	snap := c.snap.Load()
	if snap == nil || len(snap.rates) == 0 {
		return 0, false
	}
	if from == snap.base {
		if r, ok := snap.rates[to]; ok {
			return r, true
		}
		return 0, false
	}
	if to == snap.base {
		if r, ok := snap.rates[from]; ok && r != 0 {
			return 1 / r, true
		}
		return 0, false
	}
	rFrom, okFrom := snap.rates[from]
	rTo, okTo := snap.rates[to]
	if okFrom && okTo && rFrom != 0 {
		return rTo / rFrom, true
	}
	return 0, false
}

// Provenance reports the served rates' fetch time, staleness, and presence.
// stale = now-fetchedAt > 2× the refresh interval (so a failed refresh ages
// the cache into staleness rather than silently serving forever). hasRates is
// false when no snapshot is cached (the handler reports fx_as_of=null).
func (c *Cache) Provenance(_ context.Context) (asOf time.Time, stale bool, hasRates bool) {
	snap := c.snap.Load()
	if snap == nil || len(snap.rates) == 0 || snap.fetchedAt.IsZero() {
		return time.Time{}, false, false
	}
	threshold := time.Duration(c.refreshMin.Load()) * time.Minute * defaultStaleFactor
	if threshold <= 0 {
		threshold = time.Duration(defaultRefreshMin) * time.Minute * defaultStaleFactor
	}
	stale = time.Since(snap.fetchedAt) > threshold
	return snap.fetchedAt, stale, true
}

// ── Settings + HTTP ──────────────────────────────────────────────────────────

func (c *Cache) readCurrencySetting(ctx context.Context, key, fallback string) string {
	if c.settings == nil {
		return fallback
	}
	raw, err := c.settings.Get(ctx, key)
	if err != nil {
		return fallback
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return fallback
	}
	if v = strings.ToUpper(strings.TrimSpace(v)); v == "" {
		return fallback
	}
	return v
}

func (c *Cache) readSourceURL() string {
	if c.settings == nil {
		return defaultFxSourceURL
	}
	raw, err := c.settings.Get(context.Background(), settingFxSourceURL)
	if err != nil {
		return defaultFxSourceURL
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || strings.TrimSpace(v) == "" {
		return defaultFxSourceURL
	}
	return v
}

func (c *Cache) currentRefreshMin() int {
	if c.settings == nil {
		return defaultRefreshMin
	}
	raw, err := c.settings.Get(context.Background(), settingFxRefreshMin)
	if err != nil {
		return defaultRefreshMin
	}
	// Settings KV stores JSON; the value may be a JSON number or a JSON
	// string — accept both.
	if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n > 0 {
		return n
	}
	var v int
	if err := json.Unmarshal(raw, &v); err == nil && v > 0 {
		return v
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			return n
		}
	}
	return defaultRefreshMin
}

// httpFetch is the default rate-source call: GET sourceURL and parse the
// Frankfurter {base, rates} shape (the §0.2 provider pick; an operator
// overriding billing.fx_source_url must return the same shape).
func (c *Cache) httpFetch(ctx context.Context, sourceURL string) (string, map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("fetch %s: HTTP %d", sourceURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return "", nil, fmt.Errorf("read body: %w", err)
	}
	var parsed struct {
		Base  string             `json:"base"`
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", nil, fmt.Errorf("parse %s: %w (body: %s)", sourceURL, err, truncate(body, 200))
	}
	if parsed.Base == "" {
		return "", nil, errors.New("response missing base currency")
	}
	parsed.Base = strings.ToUpper(parsed.Base)
	return parsed.Base, parsed.Rates, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
