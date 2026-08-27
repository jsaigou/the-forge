-- Sprint D: compressor fail-open auto-tuning.
--
-- 1. Add fail_open_total column to compressor_savings_samples — the rolling
--    sum of fail-open events (timeout + error) that the collector computes
--    from the new compress_failopen_total{reason} Prometheus metric.
-- 2. Add fail_open_budget_ms column to compressor_proxies — the per-proxy
--    override of FORGE_COMPRESS_FAILOPEN_BUDGET_MS (0 = use the binary's
--    2000ms default).  The smith proposer writes here; the provisioner's
--    writeEnv reads it.
-- 3. Seed the smith.failopen_budget_ms setting (the current forge-compress
--    default, editable by smith's settings_change proposal).

ALTER TABLE compressor_savings_samples ADD COLUMN fail_open_total INTEGER NOT NULL DEFAULT 0;

ALTER TABLE compressor_proxies ADD COLUMN fail_open_budget_ms INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('smith.failopen_budget_ms', '2000', strftime('%s', 'now'));
