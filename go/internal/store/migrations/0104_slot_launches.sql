-- SPDX-License-Identifier: Apache-2.0
-- ADR-0026 / Radiance plan Phase 1b step (a). The launch of a slot is a database row, written by the engine manager before it starts the unit
-- and read by the generic slot shim (`forge slot-exec <slot>`) through a read-only connection. One row per slot; the primary key is the slot.
--
-- `generation` is +1 on every write (set inside the write transaction); `armed` is cleared by a compare-and-set on the generation when the slot is
-- unloaded; `format_version` lets slot-exec refuse a row layout it does not know. `config_id` deliberately has NO foreign key: a deleted config
-- must not delete the history of what was launched. No secret is ever stored here: not in argv_json, not in env_json, and there is no key column.
-- IF NOT EXISTS so a re-run (the upgrade tests re-apply recent migrations) is harmless.
CREATE TABLE IF NOT EXISTS slot_launches (
    slot            TEXT    PRIMARY KEY,
    generation      INTEGER NOT NULL,
    format_version  INTEGER NOT NULL DEFAULT 1,
    armed           INTEGER NOT NULL DEFAULT 1 CHECK (armed IN (0, 1)),
    config_id       INTEGER,
    config_name     TEXT    NOT NULL,
    engine_kind     TEXT    NOT NULL,
    backend         TEXT    NOT NULL DEFAULT '',
    exe             TEXT    NOT NULL,
    argv_json       TEXT    NOT NULL,
    env_json        TEXT    NOT NULL DEFAULT '{}',
    cwd             TEXT    NOT NULL DEFAULT '',
    port            INTEGER NOT NULL,
    model_path      TEXT    NOT NULL DEFAULT '',
    mmproj_path     TEXT    NOT NULL DEFAULT '',
    n_ctx           INTEGER NOT NULL DEFAULT 0,
    spec_hash       TEXT    NOT NULL,
    written_at      INTEGER NOT NULL,
    written_by      TEXT    NOT NULL DEFAULT ''
);
