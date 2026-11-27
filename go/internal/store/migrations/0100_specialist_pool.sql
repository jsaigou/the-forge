-- SPDX-License-Identifier: Apache-2.0
-- ADR-0022 slice 2: what can live in a specialist slot, and which occupant answers each function by default.
-- An occupant is a REGISTERED LAUNCH TEMPLATE (kind + validated params), never a command line: the launcher maps kind to a binary under an
-- allow-listed root. policy is always_on (reserved, restored at daemon start with backoff) or on_demand (loaded on first use, evicted when idle
-- and needed). home_slot is the slot an always_on occupant is reserved to ('' for on_demand). Additive; the previous binary ignores both tables.
CREATE TABLE IF NOT EXISTS specialist_occupants (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL UNIQUE,
    label        TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL,
    params       TEXT NOT NULL DEFAULT '{}',   -- JSON object, validated against the kind's parameter spec in code
    functions    TEXT NOT NULL DEFAULT '[]',   -- JSON array of function names it can serve
    memory_bytes INTEGER NOT NULL DEFAULT 0,   -- estimate until measured; the fit gate uses it before a first run
    policy       TEXT NOT NULL DEFAULT 'on_demand',
    home_slot    TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    notes        TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS specialist_defaults (
    function  TEXT PRIMARY KEY,                -- embeddings | speech | transcription | alignment | decisions
    occupant  TEXT NOT NULL                    -- specialist_occupants.name (by name so a rename is a visible edit)
);
