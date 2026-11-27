-- SPDX-License-Identifier: Apache-2.0
-- Data fix (2026-11-01, ADR-0020 finding F1). The HF registrar recorded `parallel` only in a column the launcher
-- never reads (registry.go calls it a dead column), so configs it created carry no --parallel flag and leave the
-- slot count to llama-server's default. CLAUDE.md's rule is that --parallel is always explicit: it splits the
-- configured context across N slots, so an unpinned default can silently shrink the usable context.
--
-- Append `--parallel <parallel column>` to every llama.cpp config that has no --parallel, -np or --parallel=N
-- already. Today the column is 1 everywhere and the effective default is one slot (verified: /props n_ctx equals
-- the configured n_ctx on the affected configs), so this pins the current behaviour rather than changing it.
-- Idempotent: after one run every llama.cpp config has the flag. vLLM configs are left alone.
UPDATE configs
SET extra_args = json_insert(json_insert(extra_args, '$[#]', '--parallel'), '$[#]', CAST(parallel AS TEXT))
WHERE engine_id = (SELECT id FROM engines WHERE name = 'llama.cpp')
  AND parallel >= 1
  AND NOT EXISTS (
        SELECT 1 FROM json_each(configs.extra_args)
        WHERE value IN ('--parallel', '-np') OR value LIKE '--parallel=%');
