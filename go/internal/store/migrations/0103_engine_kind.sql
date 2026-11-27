-- SPDX-License-Identifier: Apache-2.0
-- ADR-0025 / Radiance plan Phase 1a. `builds.backend` meant both the GPU path (vulkan | rocm) and, for the value "vllm", the engine.
-- The engine is now explicit on the engines row: `kind` selects launch, readiness, context and metrics behaviour in Go
-- (internal/enginekind). Deliberately no CHECK: the set of kinds is owned by that table, and Go refuses an unknown kind loudly
-- (adding a kind must not need a table rebuild).
--
-- Backfill keeps behaviour identical: the seeded vLLM engine becomes 'vllm', everything else (llama.cpp, any operator-added
-- row) stays 'llamacpp'. Builds whose backend is the legacy value "vllm" are still read as vLLM (enginekind.Resolve), so no build or
-- config row is rewritten.
ALTER TABLE engines ADD COLUMN kind TEXT NOT NULL DEFAULT 'llamacpp';
UPDATE engines SET kind = 'vllm' WHERE name = 'vLLM';
