-- SPDX-License-Identifier: Apache-2.0
-- Schema v18 (product/QA sprint, 2026-07-29 — profiling depth-sweep
-- benchmarks). The scalar prefill_tps/decode_tps on model_profiles stay as
-- the TYPICAL figure (measured at depth 0, empty context) — the fit check
-- and cost formula already consume those two columns and don't need to
-- change. This child table adds the full depth curve (empty/25%/50%/full)
-- the FE's "Show more" reveals, and the WORST CASE figure it shows by
-- default (the deepest row).
CREATE TABLE IF NOT EXISTS model_profile_benchmarks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    profile_id   INTEGER NOT NULL REFERENCES model_profiles(id) ON DELETE CASCADE,
    depth_tokens INTEGER NOT NULL,  -- KV-cache depth this row was measured at
    pp2048_tps   REAL    NOT NULL,  -- prefill throughput for a fresh 2048-token prompt at this depth
    tg128_tps    REAL    NOT NULL   -- decode throughput for 128 generated tokens at this depth
);
CREATE INDEX IF NOT EXISTS idx_model_profile_benchmarks_profile ON model_profile_benchmarks(profile_id);
