-- SPDX-License-Identifier: Apache-2.0
-- Schema v10 (Console config-led gallery migration — ADR 0006, grilling Q3).
-- Adds created_at to configs so the console gallery can offer a "new" sort
-- toggle alongside alpha/use. Existing rows are backfilled to the migration
-- time (unixepoch()) — there's no earlier truth to recover, so "new" simply
-- treats pre-migration configs as all equally old relative to ones created
-- after this lands.
ALTER TABLE configs ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
UPDATE configs SET created_at = unixepoch() WHERE created_at = 0;
