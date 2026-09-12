-- Compressor investigation (2026-09-11): the mean-only overhead figure was
-- found to hide a bimodal real-traffic shape — most messages barely pay the
-- compression tax, a few huge ones pay a lot. Add nullable percentile
-- columns (latest-window-sample gauges, same convention as the existing
-- overhead_min_ms/overhead_max_ms — never summed/averaged) computed by
-- cmd/forge-compress's new bounded-ring overhead sampler
-- (percentileMinSamples = 10; NULL below that floor, never a fabricated 0).

ALTER TABLE compressor_savings_samples ADD COLUMN overhead_p50_ms REAL;
ALTER TABLE compressor_savings_samples ADD COLUMN overhead_p90_ms REAL;
ALTER TABLE compressor_savings_samples ADD COLUMN overhead_p99_ms REAL;
