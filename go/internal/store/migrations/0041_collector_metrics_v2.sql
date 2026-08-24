-- SPDX-License-Identifier: Apache-2.0
-- Schema v41 (Pre-release feedback sprint Phase 4, collector metrics, 2026-08-12).
--
-- Makes the new/fixed collector readings chartable in metric_samples, matching
-- the additive-nullable-column precedent set by 0021: cpu_pct is real
-- /proc/stat jiffy-delta utilization (the pre-existing CPU.Pct was
-- load1/nproc*100 — load average, not utilization, never stored historically
-- at all); net_rx/tx_bytes_per_sec is a diffed /proc/net/dev rate (network
-- throughput was entirely absent before); the three temp columns are
-- additional hwmon channels beyond the original GPU-edge temp_celsius
-- (junction/hotspot, CPU package via k10temp/zenpower/coretemp, NVMe drive).
--
-- Per-mount storage (root/models/state) is deliberately NOT added here — it's
-- a live-snapshot-only field on GET /api/v1/metrics for now, not tracked
-- historically; the existing disk_used_bytes/disk_total_bytes columns already
-- cover the one volume (ModelsDir) that mattered before this migration.

ALTER TABLE metric_samples ADD COLUMN cpu_pct                   REAL;
ALTER TABLE metric_samples ADD COLUMN net_rx_bytes_per_sec      REAL;
ALTER TABLE metric_samples ADD COLUMN net_tx_bytes_per_sec      REAL;
ALTER TABLE metric_samples ADD COLUMN gpu_junction_temp_celsius REAL;
ALTER TABLE metric_samples ADD COLUMN cpu_package_temp_celsius  REAL;
ALTER TABLE metric_samples ADD COLUMN nvme_temp_celsius         REAL;
