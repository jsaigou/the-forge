-- SPDX-License-Identifier: Apache-2.0

-- Schema v87 (2026-09-14). "Performance-level routing" / "perf_classes" was
-- found to be a misnomer during the initial live curation pass: perf_rank
-- measures relative capability ("how smart"), not throughput or latency —
-- confirmed live against the real catalog, where the correct call turned on
-- weighing GPQA/SWE-bench scores, not tokens/sec. Renamed throughout to
-- "capability tiers" / "capability-tier substitution" (operator decision,
-- 2026-09-14) to stop that misreading before more of the schema, API, and
-- UI are built on top of it. Same pattern as 0059's Headroom->compressor
-- rename: table/column renames auto-update REFERENCES clauses (SQLite
-- ALTER TABLE RENAME TO, default behavior since 3.25), settings keys and
-- smith_findings.check_id are rewritten in place; smith_actions history
-- (title/detail/dedupe_key referencing the old "perf_class"/"config_perf"
-- catalog_change Table values) is deliberately left untouched — it's a
-- record of what was actually proposed at the time, same principle 0059
-- established.
ALTER TABLE perf_classes RENAME TO capability_tiers;
ALTER TABLE configs RENAME COLUMN perf_class_id TO capability_tier_id;
ALTER TABLE configs RENAME COLUMN perf_rank TO capability_rank;

-- perf_classes.name's UNIQUE constraint created an implicit index, which
-- SQLite's table rename already carries over automatically (unlike 0059's
-- explicit named indexes, nothing to manually recreate here).

-- Settings key: router.impersonation -> router.capability_substitution.
-- Holds live operator state (this host's real value is "off" as of this
-- migration) — rewritten in place, not duplicated, so there is exactly one
-- source of truth after this runs.
UPDATE settings SET key = 'router.capability_substitution', updated_at = updated_at
    WHERE key = 'router.impersonation';

-- smith_findings.check_id: perf_class_coverage -> capability_tier_coverage,
-- so existing finding history stays attached to the still-registered check
-- (same reasoning as 0059's headroom_health -> compressor_reachability).
UPDATE smith_findings SET check_id = 'capability_tier_coverage'
    WHERE check_id = 'perf_class_coverage';
