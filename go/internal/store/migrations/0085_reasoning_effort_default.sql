-- SPDX-License-Identifier: Apache-2.0
-- Schema v85 (per-request thinking control, Sprint T2, 2026-09-14).
-- reasoning_effort_default is the per-config default reasoning level
-- (none|low|medium|high|'' — '' means unset, no opinion) applied by a0's
-- reasoning_effort translation layer (internal/router/reasoning.go) whenever
-- a request doesn't send its own reasoning_effort. This is what replaces
-- today's launch-time flags (--chat-template-kwargs '{"enable_thinking":false}',
-- --reasoning-budget) as the mechanism for a config's default thinking
-- behavior — see T3's plan to retire gemma4-26b-a4b-nothink as a duplicate
-- config once its launch-time '{"enable_thinking":false}' becomes this
-- column plus an alias's forced request default instead.
--
-- NOT NULL DEFAULT '' with the CHECK covering '' too (same pattern as
-- 0012_build_backend.sql's backend column) so no table rebuild is needed —
-- every existing config gets '' (no default; behavior unchanged until an
-- operator sets one).
ALTER TABLE configs ADD COLUMN reasoning_effort_default TEXT NOT NULL DEFAULT ''
    CHECK (reasoning_effort_default IN ('none', 'low', 'medium', 'high', ''));
