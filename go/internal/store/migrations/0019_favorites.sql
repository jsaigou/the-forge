-- SPDX-License-Identifier: Apache-2.0
-- Schema v19 (product/QA sprint, 2026-07-29 — Console config-card
-- favorites/starring). Keyed by username (TEXT), not a numeric user_id FK
-- — matches the existing lightweight convention already used for
-- audit_log.actor rather than introducing a new users(id) join for what is
-- otherwise a small, per-operator preference table.
CREATE TABLE IF NOT EXISTS favorites (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    username     TEXT    NOT NULL,
    subject_type TEXT    NOT NULL CHECK (subject_type IN ('config')),
    subject_id   INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    UNIQUE(username, subject_type, subject_id)
);
CREATE INDEX IF NOT EXISTS idx_favorites_username ON favorites(username, subject_type);
