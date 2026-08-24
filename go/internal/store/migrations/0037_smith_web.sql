-- SPDX-License-Identifier: Apache-2.0
-- Schema v37 (smith P5 — web research, docs/v5-smith.md §4.8).
--
-- smith_web_cache backs the URL/query-keyed TTL cache ("repeated questions
-- don't re-fetch", §4.8). One table for both call classes — a search
-- response and a fetched document differ only in what `body` holds, and a
-- single table keeps the TTL/singleflight/expiry logic in one place.
--
-- smith_messages.sources is the P5 sources-in-transcript carrier. NOT
-- reused from `evidence` (migration 0033): that column is already claimed
-- by the action/runbook message kinds (the FE parses it as an ActionCard
-- or RunbookCard shape by branching on which keys are present), and
-- finalizeMessage never writes it. Same additive shape as 0036's kb_refs,
-- for the same reason: the carrier column simply didn't exist yet.
--
-- Conventions: unix-seconds INTEGER timestamps, 0/1 booleans, JSON as TEXT.

CREATE TABLE IF NOT EXISTS smith_web_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT    NOT NULL CHECK (kind IN ('search','fetch')),
    cache_key    TEXT    NOT NULL,             -- fetch: normalized URL; search: normalized query
    provider     TEXT    NOT NULL DEFAULT '',  -- which adapter produced this entry
    title        TEXT    NOT NULL DEFAULT '',
    content_type TEXT    NOT NULL DEFAULT '',
    status_code  INTEGER NOT NULL DEFAULT 0,
    body         TEXT    NOT NULL DEFAULT '',  -- fetch: extracted text/markdown; search: JSON results array
    body_sha256  TEXT    NOT NULL DEFAULT '',  -- change detection for the blocked-item recheck (P5 §4.9)
    truncated    INTEGER NOT NULL DEFAULT 0,   -- bool: body hit the 1 MB cap
    bytes        INTEGER NOT NULL DEFAULT 0,
    fetched_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_smith_web_cache_key
    ON smith_web_cache(kind, cache_key);
CREATE INDEX IF NOT EXISTS idx_smith_web_cache_expiry
    ON smith_web_cache(expires_at);

ALTER TABLE smith_messages ADD COLUMN sources TEXT NOT NULL DEFAULT '[]';

-- smith.web.* seeds (docs/v5-smith.md §5; "seeded once, no hardcoded
-- default anywhere in code" — the same convention 0033 used for
-- smith.model). Base URLs point at ForgeHost's real self-hosted searxng/
-- firecrawl instances, live-verified 2026-08-11: both answer without any
-- API key. On a non-ForgeHost install these simply fail the reachability
-- probe and the always-present `direct` adapter carries the chain.
INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('smith.web.enabled', 'true', strftime('%s', 'now')),
    ('smith.web.provider_order', '["searxng","firecrawl","direct"]', strftime('%s', 'now')),
    ('smith.web.searxng',
        '{"base_url":"https://searxng.example.ts.net","enabled":true,"api_key":""}',
        strftime('%s', 'now')),
    ('smith.web.firecrawl',
        '{"base_url":"https://firecrawl.example.ts.net","enabled":true,"api_key":""}',
        strftime('%s', 'now')),
    ('smith.web.direct', '{"base_url":"","enabled":true,"api_key":""}', strftime('%s', 'now')),
    ('smith.web.cache_ttl', '"6h"', strftime('%s', 'now'));
