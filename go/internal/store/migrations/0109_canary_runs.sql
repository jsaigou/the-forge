-- SPDX-License-Identifier: Apache-2.0
-- Radiance corruption canary results (docs/radiance-canary-scheduling-proposal.md, Option A). One row per canary round, scheduled or skipped; a manual
-- `forge canary --record` row has mode 'manual'. The table is also the canary's own state (last attempt, last checked launch generation, rounds in
-- the last 24 h are read from it), so a daemon restart forgets nothing. It holds counts, latencies and at most three wrong answers cut to 80
-- characters: never a prompt, a key or a full response. `slot` and `config_name` are copied, not foreign keys, so history outlives a config.
-- Pruned to 90 days or 20000 rows by the Smith retention pass. Additive; IF NOT EXISTS so the upgrade tests may re-apply it.
CREATE TABLE IF NOT EXISTS canary_runs (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                INTEGER NOT NULL,
    slot              TEXT    NOT NULL,
    config_name       TEXT    NOT NULL DEFAULT '',
    build_id          INTEGER NOT NULL DEFAULT 0,
    engine_kind       TEXT    NOT NULL DEFAULT '',
    launch_generation INTEGER NOT NULL DEFAULT 0,
    mode              TEXT    NOT NULL CHECK (mode IN ('simultaneous', 'mix', 'manual')),
    sentences         INTEGER NOT NULL DEFAULT 0,
    requests          INTEGER NOT NULL DEFAULT 0,
    wrong             INTEGER NOT NULL DEFAULT 0,
    degenerate        INTEGER NOT NULL DEFAULT 0,
    errors            INTEGER NOT NULL DEFAULT 0,
    skipped_reason    TEXT    NOT NULL DEFAULT '',
    p50_ms            INTEGER NOT NULL DEFAULT 0,
    p95_ms            INTEGER NOT NULL DEFAULT 0,
    first_wrong_texts TEXT    NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_canary_runs_slot_ts ON canary_runs (slot, ts);
CREATE INDEX IF NOT EXISTS idx_canary_runs_ts ON canary_runs (ts);
