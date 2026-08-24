-- SPDX-License-Identifier: Apache-2.0
-- Schema v67 — upstream-nightly tracking for smith's build_refresh fork
-- registry (P3smith).
--
-- The fork registry itself (migration 0061) is a settings JSON document
-- (smith.build_refresh.forks), not a table — its entries are operator-
-- reviewed recipe data. Upstream tracking needs three per-fork facts that
-- are runtime STATE rather than reviewed recipe: which upstream URL to
-- watch, whether watching is on, and the last upstream revision a build
-- was made from (recorded by the build_refresh procedure itself mid-run,
-- not by any operator). This migration gives those facts a real home.
--
-- Rows are keyed by source_ref — the same lookup key as the settings JSON
-- entries (buildRefreshFork.SourceRef). Resolution order: a row's
-- upstream_url overrides the settings entry when non-empty, a row can
-- ENABLE track_upstream, and last_built_upstream_sha always comes from
-- here — but a bare sha-only row (the routine side effect of
-- build_record_upstream_sha, written with column defaults for url/track)
-- never silently disables a settings-level opt-in. See
-- build_refresh_forks.go's effectiveForkUpstream for the exact merge.
--
-- track_upstream follows the task's declared shape (INTEGER DEFAULT 0);
-- upstream_url/last_built_upstream_sha are nullable exactly as specified.

CREATE TABLE IF NOT EXISTS smith_build_refresh_upstream (
    source_ref              TEXT PRIMARY KEY,
    upstream_url            TEXT,
    track_upstream          INTEGER NOT NULL DEFAULT 0,
    last_built_upstream_sha TEXT,
    updated_at              INTEGER NOT NULL
);
