-- SPDX-License-Identifier: Apache-2.0
-- Schema v5 (BE-AUTH Phase C — docs/v5-sprint0-auth-design.md §4/§8).
-- The recovery_codes table in 0004_auth_v2.sql was created without an
-- autoincrement id column. Phase C needs per-code id for MarkUsed. Add it
-- here as a separate migration (additive — does not touch the 0004 table
-- shape beyond the new column).
CREATE TABLE IF NOT EXISTS recovery_codes_v2 (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    used_at    INTEGER
);
INSERT INTO recovery_codes_v2 (user_id, code_hash, created_at, used_at)
  SELECT user_id, code_hash, strftime('%s','now'), used_at FROM recovery_codes;
DROP TABLE recovery_codes;
ALTER TABLE recovery_codes_v2 RENAME TO recovery_codes;
