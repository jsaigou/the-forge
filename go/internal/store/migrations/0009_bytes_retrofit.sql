-- SPDX-License-Identifier: Apache-2.0
-- Schema v9 (A1 bytes retrofit — docs/v5-modes-config-editable.md §"Post-
-- MODEL-CATALOG sprint" principle 1, "Store memory in bytes"). Renames the
-- last *_mb columns to *_bytes and converts existing rows by ×1024×1024
-- (the values were MiB quantities; bytes = MiB × 1024 × 1024).
--
-- Why rename-and-convert rather than rebuild: SQLite ≥ 3.25 supports
-- ALTER TABLE ... RENAME COLUMN, which is transactional and preserves
-- indexes (idx_* on metric_samples/migration_version PKs are untouched).
-- The multiply is a no-op on empty tables (fresh installs), so the same
-- migration is correct for both fresh and existing databases.
--
-- The original *_mb column definitions stay in 0002_polish.sql and
-- 0006_model_profiles.sql for migration-history accuracy; this file is the
-- authoritative rename. See docs/v5-store-schema.md for the annotated
-- current schema.

-- ── metric_samples ───────────────────────────────────────────────────────────

ALTER TABLE metric_samples RENAME COLUMN gtt_used_mb      TO gtt_used_bytes;
ALTER TABLE metric_samples RENAME COLUMN gtt_total_mb     TO gtt_total_bytes;
ALTER TABLE metric_samples RENAME COLUMN mem_used_mb      TO mem_used_bytes;
ALTER TABLE metric_samples RENAME COLUMN mem_total_mb     TO mem_total_bytes;
ALTER TABLE metric_samples RENAME COLUMN disk_used_mb     TO disk_used_bytes;
ALTER TABLE metric_samples RENAME COLUMN disk_total_mb    TO disk_total_bytes;
ALTER TABLE metric_samples RENAME COLUMN inference_rss_mb TO inference_rss_bytes;

-- Convert existing rows (MiB → bytes). No-op on a fresh (empty) table.
UPDATE metric_samples SET gtt_used_bytes      = gtt_used_bytes      * 1024 * 1024 WHERE gtt_used_bytes      IS NOT NULL;
UPDATE metric_samples SET gtt_total_bytes      = gtt_total_bytes      * 1024 * 1024 WHERE gtt_total_bytes      IS NOT NULL;
UPDATE metric_samples SET mem_used_bytes       = mem_used_bytes       * 1024 * 1024 WHERE mem_used_bytes       IS NOT NULL;
UPDATE metric_samples SET mem_total_bytes      = mem_total_bytes      * 1024 * 1024 WHERE mem_total_bytes      IS NOT NULL;
UPDATE metric_samples SET disk_used_bytes      = disk_used_bytes      * 1024 * 1024 WHERE disk_used_bytes      IS NOT NULL;
UPDATE metric_samples SET disk_total_bytes     = disk_total_bytes     * 1024 * 1024 WHERE disk_total_bytes     IS NOT NULL;
UPDATE metric_samples SET inference_rss_bytes  = inference_rss_bytes  * 1024 * 1024 WHERE inference_rss_bytes  IS NOT NULL;

-- ── model_profiles ──────────────────────────────────────────────────────────

ALTER TABLE model_profiles RENAME COLUMN safe_memory_mb TO safe_memory_bytes;
UPDATE model_profiles SET safe_memory_bytes = safe_memory_bytes * 1024 * 1024 WHERE safe_memory_bytes IS NOT NULL;
