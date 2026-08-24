-- SPDX-License-Identifier: Apache-2.0
-- Schema v35 (smith P3 — reasoning tier, docs/v5-smith.md §4.3).
--
-- Adds to smith_messages (created empty in 0033, never written by any Go
-- code until this phase): model (which brain answered — a Config name or an
-- Offering's wire_model, NULL for user/action/runbook/notice rows), tier
-- (deterministic | reasoning, mirrors smith_conversations.tier but recorded
-- per-message since a conversation can span a mid-stream degrade), error
-- (non-NULL only for a `notice` row produced by a Tier 2 failure — a0
-- 5xx/timeout, brain-load failure), token_count (approximate, from the
-- context-assembly token budget accounting — lets a reconnecting client
-- distinguish "message finished short" from "message never streamed").
--
-- SQLite ALTER TABLE ADD COLUMN is safe here (no CHECK constraint touched,
-- unlike 0034's smith_actions rebuild) — a plain additive migration.
ALTER TABLE smith_messages ADD COLUMN model TEXT;
ALTER TABLE smith_messages ADD COLUMN tier TEXT;
ALTER TABLE smith_messages ADD COLUMN error TEXT;
ALTER TABLE smith_messages ADD COLUMN token_count INTEGER;
