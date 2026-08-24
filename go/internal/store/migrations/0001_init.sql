-- SPDX-License-Identifier: Apache-2.0
-- Schema v1 (Contract 3 — docs/v5-store-schema.md is the annotated version).
-- Conventions: timestamps are unix seconds UTC (INTEGER), booleans are
-- INTEGER 0/1, JSON payloads are TEXT. The DB file carries secrets
-- (provider API keys, session ids) — 0600, owned by the service user.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,             -- Argon2id encoded string
    role          TEXT    NOT NULL CHECK (role IN ('viewer', 'operator', 'admin')),
    created_at    INTEGER NOT NULL,
    disabled      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE sessions (
    id           TEXT    PRIMARY KEY,           -- random 256-bit, url-safe
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER,
    remote_addr  TEXT,
    user_agent   TEXT
);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- Bearer keys for all three token kinds; token format is
-- sk-<kind>-<keyid>-<secret> with kind in (foundry, router, mcp) and keyid
-- 12 lowercase hex chars (routes to exactly one row -> one Argon2 verify per
-- request).
CREATE TABLE api_keys (
    keyid        TEXT    PRIMARY KEY,
    kind         TEXT    NOT NULL CHECK (kind IN ('foundry', 'router', 'mcp')),
    name         TEXT    NOT NULL,              -- agent/consumer identity (requested_by)
    secret_hash  TEXT    NOT NULL,              -- Argon2id encoded string
    role         TEXT    CHECK (role IN ('viewer', 'operator', 'admin')), -- foundry kind only
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER                        -- soft revoke; NULL = active
);

-- Scheduler persistence for restart recovery (in-memory is authoritative at
-- runtime; replaces V4 slots.json + queue.json).
CREATE TABLE slot_state (
    slot       TEXT    PRIMARY KEY,
    mode       TEXT,                            -- NULL = empty
    loaded_at  INTEGER,
    updated_at INTEGER NOT NULL
);

CREATE TABLE sched_queue (
    ticket_id   TEXT    PRIMARY KEY,
    model       TEXT    NOT NULL,
    requested_by TEXT   NOT NULL,
    target_slot TEXT,
    status      TEXT    NOT NULL,
    small_job   INTEGER NOT NULL DEFAULT 0,
    priority    INTEGER NOT NULL DEFAULT 0,
    enqueued_at INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE reservations (
    label                    TEXT    PRIMARY KEY,
    model                    TEXT    NOT NULL,
    start_ts                 INTEGER NOT NULL,
    end_ts                   INTEGER NOT NULL,
    scope                    TEXT    NOT NULL CHECK (scope IN ('bay', 'whole_box', 'comfyui')),
    bay                      TEXT,              -- set iff scope = 'bay'
    created_by               TEXT    NOT NULL,
    allow_agent_reschedule   INTEGER NOT NULL,
    allow_agent_cancellation INTEGER NOT NULL,
    created_at               INTEGER NOT NULL,
    CHECK (end_ts > start_ts),
    CHECK ((scope = 'bay') = (bay IS NOT NULL))
);

-- Usage/cost event stream (V4 usage.py). Aggregations are computed at read
-- time over the window.
CREATE TABLE usage_events (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                INTEGER NOT NULL,
    kind              TEXT    NOT NULL,          -- load_ok, load_failure, inference,
                                                 -- inference_hang, kfd_eviction, unload,
                                                 -- external_request, ...
    model             TEXT,
    slot              TEXT,
    provider          TEXT,                      -- external events only
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    cost_usd          REAL,
    detail            TEXT
);
CREATE INDEX idx_usage_events_ts ON usage_events(ts);

-- Headroom savings samples scraped from each proxy's /metrics counters.
CREATE TABLE headroom_savings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           INTEGER NOT NULL,
    proxy        TEXT    NOT NULL,
    tokens_in    INTEGER NOT NULL,
    saved_tokens INTEGER NOT NULL
);
CREATE INDEX idx_headroom_savings ON headroom_savings(proxy, ts);

CREATE TABLE headroom_proxies (
    service     TEXT    PRIMARY KEY,
    label       TEXT    NOT NULL DEFAULT '',
    port        INTEGER NOT NULL,
    target_url  TEXT    NOT NULL,
    unit        TEXT    NOT NULL,
    provider    TEXT,                            -- linked router_providers.name, if any
    token       TEXT,                            -- headroom-native proxy token
    passthrough INTEGER NOT NULL DEFAULT 0,
    orphaned_at INTEGER,
    created_at  INTEGER NOT NULL
);

-- External provider API keys (V4 secrets.toml [[router_providers]]).
CREATE TABLE router_providers (
    name           TEXT    PRIMARY KEY,
    api_key        TEXT    NOT NULL,
    target_url     TEXT    NOT NULL DEFAULT '',
    headroom_proxy TEXT    NOT NULL DEFAULT '',
    model          TEXT    NOT NULL DEFAULT '',
    model2         TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL
);

-- Per-mode runtime history ring buffer (V4 history.py).
CREATE TABLE mode_history (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    mode           TEXT    NOT NULL,
    ts             INTEGER NOT NULL,
    trained_ctx    INTEGER,
    configured_ctx INTEGER,
    actual_ctx     INTEGER,
    load_time_s    REAL,
    result         TEXT    NOT NULL
);
CREATE INDEX idx_mode_history ON mode_history(mode, ts);

-- App-mutated settings that lived in config.toml in V4 (config is read-only
-- to V5). JSON values keyed by dotted name: ui.theme, ui.bookmarks,
-- ui.help_button, nfs.shares, notes.sections, scheduler.config,
-- router.busy_mode, headroom.passthrough_all.
CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,                 -- JSON
    updated_at INTEGER NOT NULL
);

-- Paired node agents (V4 secrets.toml [[nodes]]).
CREATE TABLE nodes (
    node_id   TEXT    PRIMARY KEY,
    address   TEXT    NOT NULL,
    label     TEXT    NOT NULL DEFAULT '',
    secret    TEXT    NOT NULL,
    paired_at INTEGER NOT NULL
);

-- Audit log (V4 audit.py JSONL; optional JSONL mirror stays available).
CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          INTEGER NOT NULL,
    actor       TEXT    NOT NULL,
    action      TEXT    NOT NULL,
    target      TEXT,
    detail      TEXT,                            -- JSON
    remote_addr TEXT
);
CREATE INDEX idx_audit_ts ON audit_log(ts);
