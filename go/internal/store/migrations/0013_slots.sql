-- SPDX-License-Identifier: Apache-2.0
-- Schema v13 (TOML decommission Phase 0 — docs/v5-toml-decommission.md §3.2,
-- ADR-0007). Slot is already first-class domain vocabulary (CONTEXT.md), not
-- incidental config — this table gives it the same store-backed treatment as
-- Family/Model/Config/Build. It replaces `foundryd.toml`'s `[slots.*]`
-- blocks; the row data itself is populated later by the one-time cutover
-- migration (§5), not here — this migration only creates the empty table so
-- schema and bootstrap-loader work can proceed without touching ForgeHost's
-- live config.
CREATE TABLE IF NOT EXISTS slots (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE, -- primary, secondary, a3, a4
    unit       TEXT NOT NULL,        -- systemd unit, e.g. foundry-primary
    port       INTEGER NOT NULL,
    label      TEXT NOT NULL,        -- A1, A2, A3, A4
    sort_order INTEGER NOT NULL DEFAULT 0
);
