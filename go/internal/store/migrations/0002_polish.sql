-- SPDX-License-Identifier: Apache-2.0
-- Schema v2 (V5 polish sprint — docs/v5-sprint0-contract-freeze.md §0.11).
-- Carries the billing/provider/metrics tables the parallel matrix builds on.
-- Same conventions as 0001: unix-seconds INTEGER timestamps, 0/1 booleans,
-- JSON as TEXT. Frozen here (tables exist, empty) so BE-1/BE-2/BE-3 fill them
-- without a schema round-trip. Auth-v2 tables land in a SEPARATE migration
-- owned by that subsystem.

-- ── §0.2 billing & currency ──────────────────────────────────────────────────

-- Providers charge real per-1M in/out pricing in their own bill currency, and
-- one provider has many models (§0.3 taxonomy fix). Extend the existing
-- router_providers row with billing/status/credits metadata.
ALTER TABLE router_providers ADD COLUMN bill_currency TEXT NOT NULL DEFAULT 'USD';
ALTER TABLE router_providers ADD COLUMN status_url    TEXT NOT NULL DEFAULT '';
ALTER TABLE router_providers ADD COLUMN credits_url   TEXT NOT NULL DEFAULT '';

-- Per-model pricing/catalog under each provider (§0.2 / §0.3). External cost =
-- prompt_tokens/1e6*price_in + completion_tokens/1e6*price_out, in
-- provider.bill_currency, then FX-converted to the display currency.
CREATE TABLE provider_models (
    provider         TEXT    NOT NULL REFERENCES router_providers(name) ON DELETE CASCADE,
    model_id         TEXT    NOT NULL,             -- e.g. "kimi-k2.7", "deepseek-chat"
    display_name     TEXT    NOT NULL DEFAULT '',
    logo             TEXT    NOT NULL DEFAULT '',   -- icon slug (§0.8)
    price_in_per_1m  REAL    NOT NULL DEFAULT 0,    -- in provider.bill_currency
    price_out_per_1m REAL    NOT NULL DEFAULT 0,
    headroom_proxy   TEXT    NOT NULL DEFAULT '',   -- optional linked proxy service
    enabled          INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (provider, model_id)
);

-- Live FX rates, daemon-fetched and cached; served to the PWA, never fetched
-- from it (§0.2). On fetch failure the last cached rate is used and flagged
-- stale by the read path.
CREATE TABLE fx_rates (
    base       TEXT    NOT NULL,   -- e.g. 'USD'
    quote      TEXT    NOT NULL,   -- e.g. 'CNY'
    rate       REAL    NOT NULL,   -- 1 base = rate quote
    fetched_at INTEGER NOT NULL,
    PRIMARY KEY (base, quote)
);

-- ── §0.3 provider health/credits cache ───────────────────────────────────────

-- Daemon-fetched provider health + credit balance, cached (TTL frozen ≥60s in
-- §0.3). One row per provider; the read path serves this rather than probing
-- the provider API per request. JSON payloads mirror the providerHealthJSON /
-- providerCreditsJSON wire shapes.
CREATE TABLE provider_state (
    provider     TEXT    PRIMARY KEY REFERENCES router_providers(name) ON DELETE CASCADE,
    health_json  TEXT    NOT NULL DEFAULT '',   -- cached ProviderHealth
    credits_json TEXT    NOT NULL DEFAULT '',   -- cached ProviderCredits
    fetched_at   INTEGER NOT NULL DEFAULT 0
);

-- ── §0.4 metrics time-series ─────────────────────────────────────────────────

-- One row per sampler tick (interval a config setting, default 60s). Retention
-- prunes rows older than metrics.retention_days (default 90) via a daily job.
-- The history endpoint downsamples (avg per bucket) on read.
CREATE TABLE metric_samples (
    ts               INTEGER PRIMARY KEY,   -- unix sec, one row per sample tick
    gtt_used_mb      INTEGER,
    gtt_total_mb     INTEGER,
    gpu_use_pct      REAL,
    mem_used_mb      INTEGER,
    mem_total_mb     INTEGER,
    disk_used_mb     INTEGER,
    disk_total_mb    INTEGER,
    temp_celsius     REAL,
    inference_rss_mb INTEGER
);
