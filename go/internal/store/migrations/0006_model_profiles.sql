-- SPDX-License-Identifier: Apache-2.0
-- Schema v6 (PROFILE track — docs/v5-profiling-benchmarks.md §4).
-- Stores measured memory + T/s profiles per (mode, n_ctx, backend, parallel).
-- Conventions: unix-seconds INTEGER timestamps, 0/1 booleans, JSON as TEXT.
CREATE TABLE IF NOT EXISTS model_profiles (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    mode            TEXT    NOT NULL,
    model_id        TEXT    NOT NULL DEFAULT '',
    n_ctx           INTEGER NOT NULL,          -- configured target context
    backend         TEXT    NOT NULL,          -- vulkan | rocm | vllm
    parallel        INTEGER NOT NULL DEFAULT 1,
    safe_memory_mb  INTEGER NOT NULL,          -- peak additive footprint + margin
    prefill_tps     REAL    NOT NULL,          -- prompt-eval tok/s (measured)
    decode_tps      REAL    NOT NULL,          -- generation tok/s (measured)
    actual_n_ctx    INTEGER NOT NULL,          -- what actually loaded (may be < n_ctx)
    fingerprint     TEXT    NOT NULL,          -- composite hash for staleness
    measured_at     INTEGER NOT NULL,          -- unix seconds UTC
    UNIQUE(mode, backend, parallel, n_ctx)     -- one profile per config-axis combo
);
CREATE INDEX IF NOT EXISTS idx_model_profiles_mode ON model_profiles(mode);
