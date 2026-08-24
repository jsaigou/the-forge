-- SPDX-License-Identifier: Apache-2.0
-- Schema v15 (product/QA sprint, 2026-07-29 — Dashboard notifications).
-- Persists collector-detected alerts (hang, GTT-high, unit crash/OOM/restart
-- — see internal/collector/run.go's collectAlerts + unitAlerts) so the
-- Dashboard can show a real notifications panel with acknowledge/dismiss
-- instead of the previous unconsumed status.alerts field. dedupe_key
-- collapses a level-triggered alert (e.g. a hang that persists across many
-- collector cycles) into one row with a bumped occurrences/last_seen rather
-- than a new row every cycle.
CREATE TABLE IF NOT EXISTS notifications (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    code            TEXT    NOT NULL,             -- INFERENCE_HANG | GTT_HIGH | UNIT_OOM | UNIT_CRASH | UNIT_RESTARTED
    severity        TEXT    NOT NULL CHECK (severity IN ('info','warn','crit')),
    subject         TEXT    NOT NULL DEFAULT '',  -- unit name or port, "" if global
    message         TEXT    NOT NULL,
    dedupe_key      TEXT    NOT NULL UNIQUE,       -- code+":"+subject
    first_seen      INTEGER NOT NULL,
    last_seen       INTEGER NOT NULL,
    occurrences     INTEGER NOT NULL DEFAULT 1,
    acknowledged_at INTEGER,                       -- NULL = not acknowledged
    dismissed_at    INTEGER                        -- NULL = not dismissed
);
CREATE INDEX IF NOT EXISTS idx_notifications_active ON notifications(dismissed_at, last_seen);
