-- SPDX-License-Identifier: Apache-2.0
-- Schema v21 (Dashboard cost/savings data layer, 2026-07-30).
--
-- Adds the storage needed to answer, with real data instead of estimates or
-- permanent zeros: measured electricity cost (package_power_w, sampled
-- alongside the existing metric_samples columns; energy is integrated at
-- read time, never pre-aggregated, so a later overhead_w/psu_efficiency/
-- rate_per_kwh calibration recomputes every historical figure rather than
-- being baked into stale rows); real remote API spend (cost_native/
-- cost_currency/cached_prompt_tokens on usage_events, and
-- price_cached_in_per_1m on offerings, both currently unpopulated columns
-- the read path already half-supports); Headroom's own per-proxy counters
-- and latency aggregates (headroom_samples/headroom_label_samples — a new
-- table rather than extending headroom_savings, because headroom_savings'
-- only accessor is a plain SUM and latency min/max/mean are not summable
-- that way); and periodic provider balance snapshots so a balance delta can
-- approximate real spend for providers with no usage-based billing API.
--
-- All ADD COLUMN are nullable, no table rewrite. metric_samples' ts PRIMARY
-- KEY and existing indexes are untouched, matching 0009's precedent.

ALTER TABLE metric_samples ADD COLUMN package_power_w REAL;
ALTER TABLE metric_samples ADD COLUMN active_slots    INTEGER;
ALTER TABLE metric_samples ADD COLUMN energy_wh_total REAL;

-- cost_usd is retained as a read-path fallback (usage_handlers.go already
-- treats it as native-currency, a pre-existing naming mismatch this fixes
-- going forward without a rename): new writes fill cost_native +
-- cost_currency in the provider's own billed currency, never a converted
-- figure — FX conversion stays a read-time display concern so a later FX
-- correction retroactively fixes displayed history.
ALTER TABLE usage_events ADD COLUMN cost_native          REAL;
ALTER TABLE usage_events ADD COLUMN cost_currency        TEXT;
ALTER TABLE usage_events ADD COLUMN cached_prompt_tokens INTEGER;

-- DeepSeek (and similarly-priced providers) bill cache-hit input tokens at a
-- lower rate than fresh input tokens (prompt_cache_hit_tokens in their
-- usage payload). Without this column, remote spend for cache-heavy agent
-- traffic would be an overestimate — the read path must report
-- cost_is_upper_bound=true when it's NULL for a priced offering.
ALTER TABLE offerings ADD COLUMN price_cached_in_per_1m REAL;

-- Per-proxy Headroom counters + latency aggregates, sampled from each
-- headroom@<service> instance's own /metrics (volatile in-process counters,
-- NOT the shared-file headroom_persistent_savings_* counters
-- headroom_savings was designed around — those are contaminated across all
-- three proxies by a shared ~/.headroom/proxy_savings.json, confirmed live
-- 2026-07-29/30; see docs/v5-headroom-topology.md). ttfb/latency/overhead
-- _min_ms/_max_ms are LIFETIME gauges from Headroom's own process — never
-- meaningful as a window statistic, only as "*_since_start". Mean is a real
-- per-interval statistic (delta(sum)/delta(count)); true percentiles are not
-- derivable from count/sum/min/max and must not be synthesized.
CREATE TABLE headroom_samples (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                      INTEGER NOT NULL,
    proxy                   TEXT    NOT NULL,
    tokens_in               INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens       INTEGER NOT NULL DEFAULT 0,
    uncached_tokens         INTEGER NOT NULL DEFAULT 0,
    compressed_saved_tokens INTEGER NOT NULL DEFAULT 0,
    requests                INTEGER NOT NULL DEFAULT 0,
    requests_cached         INTEGER NOT NULL DEFAULT 0,
    requests_failed         INTEGER NOT NULL DEFAULT 0,
    requests_rate_limited   INTEGER NOT NULL DEFAULT 0,
    ttfb_count              INTEGER,
    ttfb_sum_ms             REAL,
    ttfb_min_ms             REAL,
    ttfb_max_ms             REAL,
    latency_count           INTEGER,
    latency_sum_ms          REAL,
    latency_min_ms          REAL,
    latency_max_ms          REAL,
    overhead_count          INTEGER,
    overhead_sum_ms         REAL,
    overhead_min_ms         REAL,
    overhead_max_ms         REAL
);
CREATE INDEX idx_headroom_samples ON headroom_samples(proxy, ts);

-- One long-and-thin table for every labelled Headroom series (currently
-- {provider} and {model}; Headroom's own label set is a moving target, so a
-- column-per-dimension schema would need a migration each time it grows).
CREATE TABLE headroom_label_samples (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          INTEGER NOT NULL,
    proxy       TEXT    NOT NULL,
    label_key   TEXT    NOT NULL,
    label_value TEXT    NOT NULL,
    metric      TEXT    NOT NULL,
    delta       INTEGER NOT NULL
);
CREATE INDEX idx_headroom_label_samples
    ON headroom_label_samples(proxy, label_key, label_value, ts);

-- Periodic snapshots of each provider's live-reported balance
-- (internal/providers/credits.go fetches it but persists nothing today).
-- spend over a window ~= balance(t0) - balance(t1); a negative result means
-- a topup happened in that window (invisible to us otherwise) and must be
-- reported as status="topup_detected", never as a negative spend figure.
CREATE TABLE provider_credit_samples (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    ts             INTEGER NOT NULL,
    provider       TEXT    NOT NULL,
    balance_native REAL,
    currency       TEXT
);
CREATE INDEX idx_provider_credit_samples ON provider_credit_samples(provider, ts);
