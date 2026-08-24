-- SPDX-License-Identifier: Apache-2.0
-- Schema v42 (pre-release feedback sprint Phase 6, surrogate-key migration,
-- 2026-08-13). Full plan: /home/testuser/.claude/plans/enchanted-snuggling-kitten.md.
--
-- router_providers.name / headroom_proxies.service / reservations.label were
-- natural-key primary keys. router_providers.name in particular meant a
-- provider could never be renamed. Worse: headroom_proxies.provider and
-- router_providers.headroom_proxy were a BIDIRECTIONAL untyped string pair
-- with no FK in either direction — nothing but application code kept the
-- two halves agreeing. model_profiles.mode / model_prefill_stats.mode
-- joined to configs BY NAME despite configs.id existing since 0011.
--
-- This migration gives router_providers / headroom_proxies / reservations a
-- real integer id, demotes their natural keys to UNIQUE columns, and
-- converts every cross-table string reference to a real FK: offerings,
-- provider_models, provider_state, provider_credit_samples, usage_events,
-- headroom_savings, headroom_samples, headroom_label_samples (provider or
-- proxy -> *_id), model_profiles + model_prefill_stats (mode -> config_id).
-- The provider<->proxy pair collapses to ONE FK: headroom_proxies.provider_id
-- (router_providers.headroom_proxy is dropped; the service name becomes a
-- join-derived read projection in Go).
--
-- Provider deletion becomes soft (deleted_at + a partial unique index on
-- name), forced by usage_events.provider_id becoming a real FK: CASCADE
-- would erase spend history, SET NULL would make it unattributable, RESTRICT
-- would block the delete outright. Soft-delete is the only option that
-- doesn't lose information a hard delete today doesn't already preserve
-- (the name string survives in the event row regardless). This mirrors the
-- existing headroom_proxies.orphaned_at precedent; store.Headroom.DeleteProxy
-- was confirmed to have zero live callers (only test fakes) before reusing
-- that pattern here.
--
-- ORDERING TRAP: store.Open runs with foreign_keys=ON, and this whole file
-- executes in one transaction — PRAGMA foreign_keys is a no-op mid-transaction,
-- so it cannot be switched off here. DROP TABLE on a parent performs an
-- implicit DELETE that fires ON DELETE CASCADE on any child still pointing
-- at it. Every rebuild below therefore goes: create the new parent -> rebuild
-- every child to reference the new parent -> only then drop the old
-- children -> drop the old parent -> rename all *_new tables into place.
-- SQLite (legacy_alter_table OFF, the default) rewrites a child's REFERENCES
-- clause when the table it points at is renamed — verified by the dry-run
-- reading children's sql back out of sqlite_master after the swap, not
-- assumed. Precedent for the rebuild-and-swap idiom itself:
-- 0011_config_status_rename.sql.
--
-- offerings.id / model_profiles.id are preserved verbatim (not
-- re-assigned by autoincrement) because benchmarks.subject_id /
-- notes.subject_id and model_profile_benchmarks.profile_id reference them
-- polymorphically with no declared FK — silently renumbering would point
-- existing annotations/benchmarks at the wrong row.
--
-- Orphan rows (a natural key with no live parent): model_profiles/
-- model_prefill_stats require config_id NOT NULL and drop unmatched rows —
-- both tables are already unreadable in practice once their config is gone
-- (every real read path joins by name), so this loses nothing live. Every
-- other converted table (usage_events, headroom_savings/samples/
-- label_samples, provider_credit_samples) keeps its id column NULLABLE —
-- these are pure append-only history/telemetry and must never lose a row
-- just because its provider/proxy was later renamed or removed. Dry-run
-- against a real ForgeHost DB copy confirms actual orphan counts before deploy
-- (docs: this file's companion plan, "Verification" section).

-- ── router_providers ─────────────────────────────────────────────────────

CREATE TABLE router_providers_new (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT    NOT NULL,
    api_key            TEXT    NOT NULL,
    target_url         TEXT    NOT NULL DEFAULT '',
    model              TEXT    NOT NULL DEFAULT '',
    model2             TEXT    NOT NULL DEFAULT '',
    bill_currency      TEXT    NOT NULL DEFAULT 'USD',
    status_url         TEXT    NOT NULL DEFAULT '',
    credits_url        TEXT    NOT NULL DEFAULT '',
    org_id             TEXT    NOT NULL DEFAULT '',
    billing_enabled    INTEGER NOT NULL DEFAULT 1,
    billing_console_url TEXT   NOT NULL DEFAULT '',
    enabled            INTEGER NOT NULL DEFAULT 1,
    country            TEXT    NOT NULL DEFAULT '',
    data_residency_group TEXT  NOT NULL DEFAULT '',
    deleted_at         INTEGER,
    created_at         INTEGER NOT NULL
);
INSERT INTO router_providers_new (id, name, api_key, target_url, model, model2,
    bill_currency, status_url, credits_url, org_id, billing_enabled,
    billing_console_url, enabled, country, data_residency_group, deleted_at,
    created_at)
SELECT rowid, name, api_key, target_url, model, model2,
    bill_currency, status_url, credits_url, org_id, billing_enabled,
    billing_console_url, enabled, country, data_residency_group, NULL,
    created_at
FROM router_providers;
-- The old table's PK was `name` (TEXT) — only a single-column INTEGER PK
-- aliases SQLite's rowid, so this table has an ordinary hidden rowid rather
-- than a selectable `id`. It only has to be *some* stable per-row integer
-- for this one INSERT, not any previously-observed value, so the implicit
-- rowid is fine to reuse as the new surrogate id.

-- Partial unique index (not a plain UNIQUE column) so a name can be reused
-- after a soft delete.
CREATE UNIQUE INDEX idx_router_providers_name ON router_providers_new(name)
    WHERE deleted_at IS NULL;

-- ── headroom_proxies (references router_providers_new) ──────────────────

CREATE TABLE headroom_proxies_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    service     TEXT    NOT NULL,
    label       TEXT    NOT NULL DEFAULT '',
    port        INTEGER NOT NULL,
    target_url  TEXT    NOT NULL,
    unit        TEXT    NOT NULL,
    provider_id INTEGER REFERENCES router_providers_new(id) ON DELETE SET NULL,
    token       TEXT,
    passthrough INTEGER NOT NULL DEFAULT 0,
    orphaned_at INTEGER,
    created_at  INTEGER NOT NULL
);
INSERT INTO headroom_proxies_new (id, service, label, port, target_url, unit,
    provider_id, token, passthrough, orphaned_at, created_at)
SELECT hp.rowid, hp.service, hp.label, hp.port, hp.target_url, hp.unit,
    -- Prefer the proxy's own recorded link (hp.provider — what
    -- reconcileHeadroomProxyLink actually writes); fall back to the reverse
    -- pointer (router_providers.headroom_proxy) only when hp.provider is
    -- unset but some provider still names this service — the one real
    -- desync shape the doc comments warned about.
    (SELECT rpn.id FROM router_providers_new rpn
       WHERE rpn.name = COALESCE(
         NULLIF(hp.provider, ''),
         (SELECT rp.name FROM router_providers rp WHERE rp.headroom_proxy = hp.service LIMIT 1)
       )),
    hp.token, hp.passthrough, hp.orphaned_at, hp.created_at
FROM headroom_proxies hp;

CREATE UNIQUE INDEX idx_headroom_proxies_service ON headroom_proxies_new(service);
-- At most one ACTIVE proxy may claim a given provider (an orphaned/torn-down
-- row is exempt so a re-provision under the same provider doesn't collide
-- with its own history).
CREATE UNIQUE INDEX idx_headroom_proxies_provider ON headroom_proxies_new(provider_id)
    WHERE provider_id IS NOT NULL AND orphaned_at IS NULL;

-- ── offerings (references router_providers_new; models/variants untouched) ──

CREATE TABLE offerings_new (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id            INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    variant_id          INTEGER REFERENCES variants(id) ON DELETE SET NULL,
    provider_id         INTEGER NOT NULL REFERENCES router_providers_new(id) ON DELETE CASCADE,
    wire_model          TEXT    NOT NULL,
    price_in_per_1m     REAL    NOT NULL DEFAULT 0,
    price_out_per_1m    REAL    NOT NULL DEFAULT 0,
    price_cached_in_per_1m REAL,
    currency            TEXT    NOT NULL DEFAULT 'USD',
    context_length      INTEGER NOT NULL DEFAULT 0,
    enabled             INTEGER NOT NULL DEFAULT 1,
    priority            INTEGER NOT NULL DEFAULT 100
);
-- id preserved verbatim — benchmarks.subject_id / notes.subject_id can
-- reference an offering by id with subject_type='offering' and no FK.
INSERT INTO offerings_new (id, model_id, variant_id, provider_id, wire_model,
    price_in_per_1m, price_out_per_1m, price_cached_in_per_1m, currency,
    context_length, enabled, priority)
SELECT o.id, o.model_id, o.variant_id,
    (SELECT rpn.id FROM router_providers_new rpn WHERE rpn.name = o.provider),
    o.wire_model, o.price_in_per_1m, o.price_out_per_1m,
    o.price_cached_in_per_1m, o.currency, o.context_length, o.enabled, o.priority
FROM offerings o;
-- The old offerings table's same-named indexes still exist at this point
-- (it isn't dropped until later, per the ordering-trap comment) — drop them
-- first since index names are unique per-schema regardless of which table
-- they're attached to.
DROP INDEX idx_offerings_model;
DROP INDEX idx_offerings_provider;
CREATE INDEX idx_offerings_model ON offerings_new(model_id);
CREATE INDEX idx_offerings_provider ON offerings_new(provider_id);

-- ── provider_models (references router_providers_new, headroom_proxies_new) ──

CREATE TABLE provider_models_new (
    provider_id       INTEGER NOT NULL REFERENCES router_providers_new(id) ON DELETE CASCADE,
    model_id          TEXT    NOT NULL,
    display_name      TEXT    NOT NULL DEFAULT '',
    logo              TEXT    NOT NULL DEFAULT '',
    price_in_per_1m   REAL    NOT NULL DEFAULT 0,
    price_out_per_1m  REAL    NOT NULL DEFAULT 0,
    headroom_proxy_id INTEGER REFERENCES headroom_proxies_new(id) ON DELETE SET NULL,
    enabled           INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (provider_id, model_id)
);
INSERT INTO provider_models_new (provider_id, model_id, display_name, logo,
    price_in_per_1m, price_out_per_1m, headroom_proxy_id, enabled)
SELECT (SELECT rpn.id FROM router_providers_new rpn WHERE rpn.name = pm.provider),
    pm.model_id, pm.display_name, pm.logo, pm.price_in_per_1m, pm.price_out_per_1m,
    (SELECT hpn.id FROM headroom_proxies_new hpn WHERE hpn.service = pm.headroom_proxy),
    pm.enabled
FROM provider_models pm;

-- ── provider_state (references router_providers_new) ────────────────────

CREATE TABLE provider_state_new (
    provider_id  INTEGER PRIMARY KEY REFERENCES router_providers_new(id) ON DELETE CASCADE,
    health_json  TEXT    NOT NULL DEFAULT '',
    credits_json TEXT    NOT NULL DEFAULT '',
    fetched_at   INTEGER NOT NULL DEFAULT 0
);
INSERT INTO provider_state_new (provider_id, health_json, credits_json, fetched_at)
SELECT (SELECT rpn.id FROM router_providers_new rpn WHERE rpn.name = ps.provider),
    ps.health_json, ps.credits_json, ps.fetched_at
FROM provider_state ps
WHERE (SELECT rpn.id FROM router_providers_new rpn WHERE rpn.name = ps.provider) IS NOT NULL;
-- provider_state.provider already had a real FK+cascade in the old schema,
-- so this WHERE only guards against a same-transaction read race; it is not
-- expected to drop any row.

-- ── provider_credit_samples (references router_providers_new; nullable) ──

CREATE TABLE provider_credit_samples_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    ts             INTEGER NOT NULL,
    provider_id    INTEGER REFERENCES router_providers_new(id) ON DELETE RESTRICT,
    balance_native REAL,
    currency       TEXT
);
INSERT INTO provider_credit_samples_new (id, ts, provider_id, balance_native, currency)
SELECT pcs.id, pcs.ts,
    (SELECT rpn.id FROM router_providers_new rpn WHERE rpn.name = pcs.provider),
    pcs.balance_native, pcs.currency
FROM provider_credit_samples pcs;
DROP INDEX idx_provider_credit_samples;
CREATE INDEX idx_provider_credit_samples ON provider_credit_samples_new(provider_id, ts);

-- ── usage_events (references router_providers_new; nullable — local events
--    have no provider at all) ─────────────────────────────────────────────

CREATE TABLE usage_events_new (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                INTEGER NOT NULL,
    kind              TEXT    NOT NULL,
    model             TEXT,
    slot              TEXT,
    provider_id       INTEGER REFERENCES router_providers_new(id) ON DELETE RESTRICT,
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    cost_usd          REAL,
    detail            TEXT,
    cost_native          REAL,
    cost_currency        TEXT,
    cached_prompt_tokens INTEGER,
    unmetered         INTEGER NOT NULL DEFAULT 0
);
INSERT INTO usage_events_new (id, ts, kind, model, slot, provider_id,
    prompt_tokens, completion_tokens, cost_usd, detail, cost_native,
    cost_currency, cached_prompt_tokens, unmetered)
SELECT ue.id, ue.ts, ue.kind, ue.model, ue.slot,
    (SELECT rpn.id FROM router_providers_new rpn WHERE rpn.name = ue.provider),
    ue.prompt_tokens, ue.completion_tokens, ue.cost_usd, ue.detail,
    ue.cost_native, ue.cost_currency, ue.cached_prompt_tokens, ue.unmetered
FROM usage_events ue;
DROP INDEX idx_usage_events_ts;
CREATE INDEX idx_usage_events_ts ON usage_events_new(ts);

-- ── headroom_savings / headroom_samples / headroom_label_samples
--    (reference headroom_proxies_new; nullable) ──────────────────────────

CREATE TABLE headroom_savings_new (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           INTEGER NOT NULL,
    proxy_id     INTEGER REFERENCES headroom_proxies_new(id) ON DELETE RESTRICT,
    tokens_in    INTEGER NOT NULL,
    saved_tokens INTEGER NOT NULL
);
INSERT INTO headroom_savings_new (id, ts, proxy_id, tokens_in, saved_tokens)
SELECT hs.id, hs.ts,
    (SELECT hpn.id FROM headroom_proxies_new hpn WHERE hpn.service = hs.proxy),
    hs.tokens_in, hs.saved_tokens
FROM headroom_savings hs;
DROP INDEX idx_headroom_savings;
CREATE INDEX idx_headroom_savings ON headroom_savings_new(proxy_id, ts);

CREATE TABLE headroom_samples_new (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                      INTEGER NOT NULL,
    proxy_id                INTEGER REFERENCES headroom_proxies_new(id) ON DELETE RESTRICT,
    tokens_in               INTEGER NOT NULL DEFAULT 0,
    tokens_out              INTEGER,
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
INSERT INTO headroom_samples_new (id, ts, proxy_id, tokens_in, tokens_out,
    cache_read_tokens, uncached_tokens, compressed_saved_tokens, requests,
    requests_cached, requests_failed, requests_rate_limited, ttfb_count,
    ttfb_sum_ms, ttfb_min_ms, ttfb_max_ms, latency_count, latency_sum_ms,
    latency_min_ms, latency_max_ms, overhead_count, overhead_sum_ms,
    overhead_min_ms, overhead_max_ms)
SELECT hs.id, hs.ts,
    (SELECT hpn.id FROM headroom_proxies_new hpn WHERE hpn.service = hs.proxy),
    hs.tokens_in, hs.tokens_out, hs.cache_read_tokens, hs.uncached_tokens,
    hs.compressed_saved_tokens, hs.requests, hs.requests_cached,
    hs.requests_failed, hs.requests_rate_limited, hs.ttfb_count,
    hs.ttfb_sum_ms, hs.ttfb_min_ms, hs.ttfb_max_ms, hs.latency_count,
    hs.latency_sum_ms, hs.latency_min_ms, hs.latency_max_ms,
    hs.overhead_count, hs.overhead_sum_ms, hs.overhead_min_ms, hs.overhead_max_ms
FROM headroom_samples hs;

CREATE TABLE headroom_label_samples_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          INTEGER NOT NULL,
    proxy_id    INTEGER REFERENCES headroom_proxies_new(id) ON DELETE RESTRICT,
    label_key   TEXT    NOT NULL,
    label_value TEXT    NOT NULL,
    metric      TEXT    NOT NULL,
    delta       INTEGER NOT NULL
);
INSERT INTO headroom_label_samples_new (id, ts, proxy_id, label_key, label_value, metric, delta)
SELECT hls.id, hls.ts,
    (SELECT hpn.id FROM headroom_proxies_new hpn WHERE hpn.service = hls.proxy),
    hls.label_key, hls.label_value, hls.metric, hls.delta
FROM headroom_label_samples hls;
DROP INDEX idx_headroom_label_samples;
CREATE INDEX idx_headroom_label_samples
    ON headroom_label_samples_new(proxy_id, label_key, label_value, ts);

-- ── reservations (self-contained; gains id, label becomes UNIQUE) ───────

CREATE TABLE reservations_new (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    label                    TEXT    NOT NULL,
    model                    TEXT    NOT NULL,
    start_ts                 INTEGER NOT NULL,
    end_ts                   INTEGER NOT NULL,
    scope                    TEXT    NOT NULL CHECK (scope IN ('bay', 'whole_box', 'comfyui')),
    bay                      TEXT,
    created_by               TEXT    NOT NULL,
    allow_agent_reschedule   INTEGER NOT NULL,
    allow_agent_cancellation INTEGER NOT NULL,
    created_at               INTEGER NOT NULL,
    CHECK (end_ts > start_ts),
    CHECK ((scope = 'bay') = (bay IS NOT NULL))
);
INSERT INTO reservations_new (label, model, start_ts, end_ts, scope, bay,
    created_by, allow_agent_reschedule, allow_agent_cancellation, created_at)
SELECT label, model, start_ts, end_ts, scope, bay, created_by,
    allow_agent_reschedule, allow_agent_cancellation, created_at
FROM reservations;
CREATE UNIQUE INDEX idx_reservations_label ON reservations_new(label);

-- ── model_profiles + its child model_profile_benchmarks (mode -> config_id;
--    id preserved so the child's profile_id stays valid) ────────────────

CREATE TABLE model_profiles_new (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    config_id       INTEGER NOT NULL REFERENCES configs(id) ON DELETE CASCADE,
    model_id        TEXT    NOT NULL DEFAULT '',
    n_ctx           INTEGER NOT NULL,
    backend         TEXT    NOT NULL,
    parallel        INTEGER NOT NULL DEFAULT 1,
    safe_memory_bytes INTEGER NOT NULL,
    prefill_tps     REAL    NOT NULL,
    decode_tps      REAL    NOT NULL,
    actual_n_ctx    INTEGER NOT NULL,
    fingerprint     TEXT    NOT NULL,
    measured_at     INTEGER NOT NULL,
    UNIQUE(config_id, backend, parallel, n_ctx)
);
-- A profile whose mode no longer matches any live config is already
-- unreadable in practice (every real read path — Get/List/the fit check —
-- joins by name), so unmatched rows are dropped here rather than kept
-- dangling. Confirmed near-zero-risk by the pre-deploy dry-run (see the
-- migration's top comment).
INSERT INTO model_profiles_new (id, config_id, model_id, n_ctx, backend,
    parallel, safe_memory_bytes, prefill_tps, decode_tps, actual_n_ctx,
    fingerprint, measured_at)
SELECT mp.id, c.id, mp.model_id, mp.n_ctx, mp.backend, mp.parallel,
    mp.safe_memory_bytes, mp.prefill_tps, mp.decode_tps, mp.actual_n_ctx,
    mp.fingerprint, mp.measured_at
FROM model_profiles mp
JOIN configs c ON c.name = mp.mode;
CREATE INDEX idx_model_profiles_config ON model_profiles_new(config_id);

CREATE TABLE model_profile_benchmarks_new (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    profile_id   INTEGER NOT NULL REFERENCES model_profiles_new(id) ON DELETE CASCADE,
    depth_tokens INTEGER NOT NULL,
    pp2048_tps   REAL    NOT NULL,
    tg128_tps    REAL    NOT NULL
);
-- profile_id values are still valid against model_profiles_new because its
-- id column was preserved verbatim above; any benchmark row belonging to a
-- dropped (orphaned-mode) profile is naturally excluded by this join.
INSERT INTO model_profile_benchmarks_new (id, profile_id, depth_tokens, pp2048_tps, tg128_tps)
SELECT mpb.id, mpb.profile_id, mpb.depth_tokens, mpb.pp2048_tps, mpb.tg128_tps
FROM model_profile_benchmarks mpb
JOIN model_profiles_new mpn ON mpn.id = mpb.profile_id;
DROP INDEX idx_model_profile_benchmarks_profile;
CREATE INDEX idx_model_profile_benchmarks_profile ON model_profile_benchmarks_new(profile_id);

-- ── model_prefill_stats (mode -> config_id; same orphan reasoning) ───────

CREATE TABLE model_prefill_stats_new (
    config_id      INTEGER NOT NULL REFERENCES configs(id) ON DELETE CASCADE,
    fingerprint    TEXT    NOT NULL,
    prompt_tokens  INTEGER NOT NULL DEFAULT 0,
    prompt_seconds REAL    NOT NULL DEFAULT 0,
    samples        INTEGER NOT NULL DEFAULT 0,
    first_seen     INTEGER NOT NULL,
    last_seen      INTEGER NOT NULL,
    PRIMARY KEY (config_id, fingerprint)
);
INSERT INTO model_prefill_stats_new (config_id, fingerprint, prompt_tokens,
    prompt_seconds, samples, first_seen, last_seen)
SELECT c.id, mps.fingerprint, mps.prompt_tokens, mps.prompt_seconds,
    mps.samples, mps.first_seen, mps.last_seen
FROM model_prefill_stats mps
JOIN configs c ON c.name = mps.mode;

-- ── Drop every rebuilt child, then every rebuilt parent, THEN rename ────
-- (children first — see the ordering-trap comment at the top of this file).

DROP TABLE model_profile_benchmarks;
DROP TABLE model_profiles;
DROP TABLE model_prefill_stats;
DROP TABLE headroom_label_samples;
DROP TABLE headroom_samples;
DROP TABLE headroom_savings;
DROP TABLE usage_events;
DROP TABLE provider_credit_samples;
DROP TABLE provider_state;
DROP TABLE provider_models;
DROP TABLE offerings;
DROP TABLE reservations;
DROP TABLE headroom_proxies;
DROP TABLE router_providers;

ALTER TABLE router_providers_new RENAME TO router_providers;
ALTER TABLE headroom_proxies_new RENAME TO headroom_proxies;
ALTER TABLE offerings_new RENAME TO offerings;
ALTER TABLE provider_models_new RENAME TO provider_models;
ALTER TABLE provider_state_new RENAME TO provider_state;
ALTER TABLE provider_credit_samples_new RENAME TO provider_credit_samples;
ALTER TABLE usage_events_new RENAME TO usage_events;
ALTER TABLE headroom_savings_new RENAME TO headroom_savings;
ALTER TABLE headroom_samples_new RENAME TO headroom_samples;
ALTER TABLE headroom_label_samples_new RENAME TO headroom_label_samples;
ALTER TABLE reservations_new RENAME TO reservations;
ALTER TABLE model_profiles_new RENAME TO model_profiles;
ALTER TABLE model_profile_benchmarks_new RENAME TO model_profile_benchmarks;
ALTER TABLE model_prefill_stats_new RENAME TO model_prefill_stats;
