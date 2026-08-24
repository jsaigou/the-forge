-- SPDX-License-Identifier: Apache-2.0
-- Schema v11 (Console config-led gallery migration — ADR 0006, grilling Q4b).
-- Renames the configs.status enum value 'experimental' -> 'unverified'
-- ("experimental" read as if the config itself were unstable/beta software,
-- when it just means "no PROFILE run yet"; "unverified" says that plainly).
--
-- SQLite has no ALTER TABLE ... DROP/ADD CONSTRAINT, so a CHECK constraint
-- change requires the standard rebuild-and-swap: create the table with the
-- new CHECK, copy rows across (translating the enum value), drop the old
-- table, rename the new one into place. No other table has a foreign key
-- into configs, so this is safe without disabling foreign_keys.
CREATE TABLE configs_new (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT NOT NULL UNIQUE,
    variant_id         INTEGER NOT NULL REFERENCES variants(id) ON DELETE RESTRICT,
    weight_artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE RESTRICT,
    engine_id          INTEGER NOT NULL REFERENCES engines(id),
    build_id           INTEGER REFERENCES builds(id) ON DELETE SET NULL,
    mmproj_artifact_id INTEGER REFERENCES artifacts(id) ON DELETE SET NULL,
    n_ctx              INTEGER NOT NULL DEFAULT 0,
    parallel           INTEGER NOT NULL DEFAULT 1,
    extra_args         TEXT NOT NULL DEFAULT '[]',
    status             TEXT NOT NULL DEFAULT 'unverified'
        CHECK (status IN ('unverified', 'verified')),
    visibility         TEXT NOT NULL DEFAULT 'visible'
        CHECK (visibility IN ('visible', 'hidden')),
    is_default         INTEGER NOT NULL DEFAULT 0,
    fingerprint        TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL DEFAULT 0
);

INSERT INTO configs_new (id, name, variant_id, weight_artifact_id, engine_id,
    build_id, mmproj_artifact_id, n_ctx, parallel, extra_args, status,
    visibility, is_default, fingerprint, created_at)
SELECT id, name, variant_id, weight_artifact_id, engine_id,
    build_id, mmproj_artifact_id, n_ctx, parallel, extra_args,
    CASE status WHEN 'experimental' THEN 'unverified' ELSE status END,
    visibility, is_default, fingerprint, created_at
FROM configs;

DROP TABLE configs;
ALTER TABLE configs_new RENAME TO configs;

CREATE INDEX IF NOT EXISTS idx_configs_variant ON configs(variant_id);
