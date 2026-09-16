-- SPDX-License-Identifier: Apache-2.0
-- Schema v82 (performance-level routing, Sprint P1, 2026-09-13). Ground
-- data for a0 substituting an already-loaded model for a requested one when
-- they are operator-declared equivalent-or-better. Deliberately curated,
-- not derived from `benchmarks`: capability scores there are sparse, parsed
-- from a TEXT column with silently-ignored errors, inherited unchanged by
-- quantized sibling configs, and routinely sourced from different
-- benchmarks per model — none of that is safe to rank on automatically.
-- This follows the same pattern as `smith.brain_chain` (an operator-ordered
-- list of config names), generalized into a real table because multiple
-- independent groups are needed (not just smith's one reasoning chain) and
-- because a table gives free referential integrity across config renames
-- that a settings-KV blob would not.
--
-- A Config with perf_class_id NULL never substitutes and is never
-- substituted for — joining a class is opt-in per config. perf_rank is
-- LOWER = more capable; equal ranks are freely interchangeable. `mode`
-- lets one class override the global `router.impersonation` setting
-- (e.g. force a coding-tools class to `off` while chat classes run
-- `prefer_smarter`); NULL inherits the global setting.
CREATE TABLE perf_classes (
    id    INTEGER PRIMARY KEY,
    name  TEXT NOT NULL UNIQUE,
    mode  TEXT CHECK (mode IS NULL OR mode IN ('off', 'fallback_only', 'prefer_smarter')),
    notes TEXT NOT NULL DEFAULT ''
);

-- ON DELETE SET NULL: deleting a class degrades every member config to "no
-- substitution", never to a dangling reference (same rule as
-- families.genealogy_id / models.family_id).
ALTER TABLE configs ADD COLUMN perf_class_id INTEGER REFERENCES perf_classes(id) ON DELETE SET NULL;
ALTER TABLE configs ADD COLUMN perf_rank INTEGER NOT NULL DEFAULT 0;
