-- 0056: Sprint 4 (resource bounding + monitoring, docs/v5-headroom-replacement.md).
--
-- compressor_samples is a new, independent time series: collector-observed
-- process health (systemd unit state + /proc RSS) for each Headroom-shaped
-- proxy, sampled unconditionally every tick regardless of whether the proxy
-- had traffic or its own /metrics scrape succeeded. headroom_samples cannot
-- carry this — recordHeadroomSavings skips a proxy entirely on a failed
-- scrape or an idle interval (internal/collector/run.go), which is exactly
-- when process health matters most (this is the gap that let the 0.35.0
-- --lossless regression run undetected for a day).
--
-- n_restarts is stored ABSOLUTE (systemd's own lifetime counter for the
-- unit), not as a delta — so restart-looping shows up as a slope over a
-- window, the same way the memory-growth check reads RSS.
CREATE TABLE compressor_samples (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         INTEGER NOT NULL,
    proxy_id   INTEGER NOT NULL REFERENCES headroom_proxies(id) ON DELETE CASCADE,
    up         INTEGER NOT NULL DEFAULT 0,
    main_pid   INTEGER,
    rss_bytes  INTEGER,
    n_restarts INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_compressor_samples ON compressor_samples(proxy_id, ts);

-- requests_timeout / requests_canceled: new foundry-compress counters
-- (headroom_requests_timeout_total / headroom_requests_canceled_total,
-- cmd/foundry-compress/metrics.go). Canceled requests were previously
-- indistinguishable from real failures inside requests_failed.
ALTER TABLE headroom_samples ADD COLUMN requests_timeout INTEGER NOT NULL DEFAULT 0;
ALTER TABLE headroom_samples ADD COLUMN requests_canceled INTEGER NOT NULL DEFAULT 0;
