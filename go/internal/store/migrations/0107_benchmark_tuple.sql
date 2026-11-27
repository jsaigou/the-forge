-- SPDX-License-Identifier: Apache-2.0
-- Radiance Phase 1b step f5. A curated safe_memory_bytes figure for an engine whose footprint is a function of its launch tuple (Radiance:
-- --max-num-seqs x --max-model-len, KV dtype, speculation depth, build) is valid only for the tuple it was measured at. The benchmark row
-- records that tuple, as a JSON object (see launch.MemoryTuple), and the fit gate compares it with the config being loaded. NULL for every
-- benchmark that is not tuple-bound (all existing rows). Additive; no backfill.
ALTER TABLE benchmarks ADD COLUMN tuple_json TEXT;
